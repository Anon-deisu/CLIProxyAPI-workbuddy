// Run with CPA_AGENT_TEST_BINARY and CPA_AGENT_PLAYWRIGHT pointing to local build/tools.
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const http = require("node:http");
const assert = require("node:assert/strict");
const { spawn } = require("node:child_process");
const { chromium, expect } = require(process.env.CPA_AGENT_PLAYWRIGHT);
const repo = path.resolve(__dirname, "../..");
const binary = process.env.CPA_AGENT_TEST_BINARY;
const root = fs.mkdtempSync(
  path.join(
    process.env.CPA_AGENT_TEST_TMP || os.tmpdir(),
    "cpa-agent-browser-",
  ),
);
const managementKey = "agent-browser-synthetic-management-key";
const gates = new Map(),
  calls = [],
  running = new Map();
let service,
  browser,
  upstream,
  output = "",
  mockFailure,
  apiRequest;
const toolContent = "<script>window.pwned=1</script>\n# Generated artifact\n";
async function listen(server) {
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return server.address().port;
}
async function verifyModelTooltips(context, base) {
  const page = await context.newPage();
  const models = [
    { model: "short", success: 1, failed: 0, estimated_cost_usd: 0.01 },
    {
      model: "claude-sonnet-4-5-long-model-name",
      success: 12345,
      failed: 67,
      estimated_cost_usd: 123.4567,
    },
    { model: "gpt-6", success: 42, failed: 999, estimated_cost_usd: 1.2 },
  ];
  let tokenUsage = [
    { model: "short", input_tokens: 1000, cached_input_tokens: 250, output_tokens: 75 },
    { model: "claude-sonnet-4-5-long-model-name", input_tokens: 120000, cached_input_tokens: 90000, output_tokens: 18000 },
    { model: "gpt-6", input_tokens: 40000, cached_input_tokens: 5000, output_tokens: 2500 },
  ];
  try {
    await page.route(
      /\/v(?:0|8)\/management\/(?:auth-files|credentials)(?:\?|$)/,
      async (route) => {
        const response = await route.fetch();
        const data = await response.json();
        for (const file of data.files) {
          file.model_token_usage = tokenUsage;
          file.recent_requests = Array.from({ length: 20 }, (_, i) => ({
            success: i === 0 ? 0 : 12388,
            failed: i === 0 ? 0 : 1066,
            models: i === 0 ? [] : models,
          }));
        }
        await route.fulfill({ response, json: data });
      },
    );
    await page.goto(base + "/management.html#/auth-files");
    const blocks = page
      .locator('[class*="AuthFileCard-module__card___"]')
      .first()
      .locator('[class*="__statusBlockWrapper___"]');
    const tooltip = page.locator(
      '[class*="AuthFileCard-module__statusTooltip___"]',
    );
    const tokenTooltip = page.locator('[data-cpa-model-token-tooltip]').first();
    for (const width of [1440, 768, 390, 320]) {
      await page.setViewportSize({ width, height: 900 });
      if (width <= 768) {
        await expect
          .poll(() =>
            page
              .locator(".app-shell .main-body > .sidebar")
              .evaluate((el) => el.getBoundingClientRect().right <= 0),
          )
          .toBeTruthy();
      }
      for (const index of [1, 10, 19]) {
        await blocks.nth(index).hover();
        await expect(tooltip).toBeVisible();
        const geometry = await tooltip.evaluate((el) => {
          const box = el.getBoundingClientRect();
          const clip = el.closest(".page-transition").getBoundingClientRect();
          return {
            left: box.left,
            right: box.right,
            clipLeft: clip.left,
            clipRight: clip.right,
            rows: [...el.querySelectorAll('[class*="__tooltipStats"]')].map(
              (row) =>
                [...row.children].map((cell) => {
                  const rect = cell.getBoundingClientRect();
                  return {
                    left: rect.left,
                    right: rect.right,
                    top: rect.top,
                    scroll: cell.scrollWidth,
                    width: cell.clientWidth,
                  };
                }),
            ),
          };
        });
        assert.ok(
          geometry.left >= Math.max(8, geometry.clipLeft + 8) - 1 &&
            geometry.right <= Math.min(width - 8, geometry.clipRight - 8) + 1,
          "model tooltip must fit its visible page area",
        );
        assert.equal(geometry.rows.length, models.length + 1);
        const rows = geometry.rows.slice(1);
        for (const column of [1, 2, 3]) {
          const rightEdges = rows.map((row) => row[column].right);
          assert.ok(
            Math.max(...rightEdges) - Math.min(...rightEdges) < 1,
            "model numeric columns must align",
          );
          assert.ok(
            rows.every((row) => row[column].scroll <= row[column].width + 1),
            "numeric values must not be clipped",
          );
        }
        assert.ok(
          Math.abs(geometry.rows[0][0].right - rows[0][1].right) < 1,
          "summary success must align",
        );
        assert.ok(
          Math.abs(geometry.rows[0][1].right - rows[0][2].right) < 1,
          "summary failure must align",
        );
        for (const row of rows) {
          for (let column = 1; column < row.length; column++) {
            assert.ok(
              row[column - 1].right <= row[column].left,
              "tooltip columns must not overlap",
            );
            assert.ok(
              Math.abs(row[0].top - row[column].top) < 1,
              "wrapped model names must preserve the first-line alignment",
            );
          }
        }
      }
      for (const countSelector of ['[class*="__countOk"]', '[class*="__countFail"]']) {
        const count = page.locator('[class*="AuthFileCard-module__card___"]').first().locator(countSelector);
        await count.hover();
        await expect(tokenTooltip).toBeVisible();
        await expect(tokenTooltip).toContainText('模型 ID');
        await expect(tokenTooltip).toContainText('缓存输入');
        await expect(tokenTooltip).toContainText('缓存率');
        await expect(tokenTooltip).toContainText('75.0%');
        await expect(tokenTooltip.locator('.cpa-account-token-model')).toHaveCount(3);
        const layout = await tokenTooltip.evaluate(el => {
          const box = el.getBoundingClientRect();
          return {
            left: box.left,
            right: box.right,
            cells: [...el.querySelectorAll('th,td')].map(cell => {
               const rect = cell.getBoundingClientRect();
              const style = getComputedStyle(cell);
              return {left:rect.left,right:rect.right,top:rect.top,height:rect.height,lineHeight:parseFloat(style.lineHeight),whiteSpace:style.whiteSpace};
            }),
            texts: [...el.querySelectorAll('tbody tr')].map(row => [...row.cells].map(cell => cell.textContent))
          };
        });
        assert.ok(layout.left >= 0 && layout.right <= width + 1, 'token tooltip must fit viewport');
        assert.equal(layout.cells.length, 20, 'token table must have one header and three five-column model rows');
        for (let row = 0; row < 4; row += 1) {
          const cells = layout.cells.slice(row * 5, row * 5 + 5);
          assert.equal(new Set(cells.map(cell => cell.top)).size, 1, 'token columns must stay on one line');
          assert.ok(cells.every(cell => cell.whiteSpace === 'nowrap' && cell.height <= cell.lineHeight + 5), 'token data must not wrap');
          for (let col = 1; col < cells.length; col += 1) {
            assert.ok(cells[col - 1].right <= cells[col].left + 1, 'token table columns must not overlap');
          }
        }
        assert.deepEqual(layout.texts.map(row => row[0]), ['claude-sonnet-4-5-long-model-name', 'gpt-6', 'short']);
        assert.deepEqual(layout.texts[0], ['claude-sonnet-4-5-long-model-name', '120,000', '90,000', '75.0%', '18,000']);
        assert.deepEqual(layout.texts[2], ['short', '1,000', '250', '25.0%', '75']);
        await tokenTooltip.hover();
        await expect(tokenTooltip).toBeVisible();
        await tokenTooltip.evaluate(el => { el.scrollLeft = el.scrollWidth; });
        assert.ok(await tokenTooltip.locator('th').last().evaluate(cell => {
          const box = cell.getBoundingClientRect();
          const parent = cell.closest('[role="tooltip"]').getBoundingClientRect();
          return box.left >= parent.left && box.right <= parent.right;
        }), 'horizontal scroll must reveal the output column without wrapping');
        await tokenTooltip.evaluate(el => { el.scrollLeft = 0; });
      }
      await page.screenshot({
        path: path.join(root, "model-tooltip-" + width + ".png"),
      });
      await blocks.first().hover();
      assert.equal(
        await tooltip.evaluate((el) => getComputedStyle(el).display),
        "flex",
        "empty tooltips retain their existing layout",
      );
      await page.mouse.move(0, 0);
    }
    tokenUsage = [];
    await page.getByRole('button', {name:'刷新',exact:true}).click();
    const counts = page.locator('[class*="AuthFileCard-module__card___"]').first().locator('[class*="__countOk"]');
    await counts.hover();
    await expect(tokenTooltip).toContainText('暂无模型 token 统计');
    await expect(tokenTooltip.locator('tbody tr')).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(tokenTooltip).toBeHidden();
    tokenUsage = [{model:'after-reset-model',input_tokens:200,cached_input_tokens:100,output_tokens:10}];
    await page.getByRole('button', {name:'刷新',exact:true}).click();
    await counts.hover();
    await expect(tokenTooltip.locator('tbody tr')).toHaveCount(1);
    await expect(tokenTooltip).toContainText('after-reset-model');
    await expect(tokenTooltip).toContainText('50.0%');
    await expect(tokenTooltip).not.toContainText('claude-sonnet');
    await page.keyboard.press('Escape');
    console.log(
      "model tooltip columns, complete account totals, clearing, single-line cells and viewport bounds passed",
    );
  } finally {
    await page.close();
  }
}
async function verifyRollingTokenTooltip(context, base) {
  const page = await context.newPage();
  try {
    await page.route(/\/v8\/management\/observability\/usage\/token-summary(?:\?|$)/, async route => {
      const response = await route.fetch();
      const data = await response.json();
      data.window_minutes = 200;
      data.input_tokens = 1220;
      data.models = [
        {model:"least",input_tokens:100,cached_input_tokens:0,output_tokens:5},
        {model:"most",input_tokens:900,cached_input_tokens:300,output_tokens:50},
        {model:"small",input_tokens:20,cached_input_tokens:5,output_tokens:1},
        {model:"large",input_tokens:200,cached_input_tokens:50,output_tokens:10},
      ];
      await route.fulfill({response,json:data});
    });
    await page.goto(base + "/management.html");
    const totalInput = page.locator("#cpa-rolling-token-summary .cpa-rolling-token-input-trigger");
    const tooltip = page.locator("#cpa-rolling-token-tooltip");
    await expect(totalInput).toHaveCount(1);
    await expect(totalInput).toContainText("1,220");
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({width,height:900});
      await totalInput.hover();
      await expect(tooltip).toBeVisible();
      await expect(tooltip).toContainText("近200 分钟");
      const geometry = await tooltip.evaluate(el => ({
        box:el.getBoundingClientRect().toJSON(),
        models:[...el.querySelectorAll('tbody tr')].map(row=>[...row.cells].map(cell=>cell.textContent)),
      }));
      assert.ok(geometry.box.left >= 7 && geometry.box.right <= width - 7, "rolling tooltip must fit the viewport");
      assert.equal(await tooltip.locator('.cpa-rolling-token-file-title').count(),0,"the summary tooltip must not show auth-file groups");
      assert.deepEqual(geometry.models.map(row=>row[0]),["most","large","least","small"],"models must be merged across auth files and sorted by total input");
      await page.mouse.move(0,0);
      await expect(tooltip).toBeHidden();
    }
    await totalInput.focus();
    await expect(tooltip).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(tooltip).toBeHidden();
    console.log("rolling total-token tooltip and global model ordering passed");
  } finally {
    await page.close();
  }
}
async function verifyAccountStateControls(context, base) {
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  let files, lastPatch;
  try {
    await page.route(/\/v(?:0|8)\/management\/(?:auth-files|credentials)(?:\/status)?(?:\?|$)/, async route => {
      if (route.request().method() === "PATCH") {
        lastPatch = route.request().postDataJSON();
        const file = files.find(item => item.name === lastPatch.name);
        assert.ok(file, "state PATCH must target the matching file");
        assert.equal(lastPatch.auth_index, file.auth_index);
        file.disabled = lastPatch.scheduling_state === "disabled";
        file.degraded = lastPatch.scheduling_state === "degraded";
        file.scheduling_state = lastPatch.scheduling_state;
        await route.fulfill({json:{status:"ok"}});
        return;
      }
      const response = await route.fetch();
      const data = await response.json();
      if (!files) {
        const original = data.files.find(file => file.name === "agent-browser.json");
        assert.ok(original);
        files = ["workbuddy", "workbuddy-ai"].map((provider, index) => ({
          ...original, id:provider+"-browser.json", name:provider+"-browser.json", auth_index:"wb-browser-"+index,
          provider, type:provider, label:"shared-display-name", nickname:"shared-display-name",
          disabled:false, degraded:index===1, runtime_only:false, scheduling_state:index===1?"degraded":"normal",
        }));
      }
      await route.fulfill({response, json:{...data, files}});
    });
    await page.goto(base + "/management.html#/auth-files");
    const cards = page.locator('article[data-cpa-auth-name]');
    await expect(cards).toHaveCount(2);
    const cn = page.locator('article[data-cpa-auth-name="workbuddy-browser.json"]');
    const ai = page.locator('article[data-cpa-auth-name="workbuddy-ai-browser.json"]');
    await expect(cn.locator('.cpa-account-state button[value="normal"]')).toHaveAttribute('aria-pressed', 'true');
    await expect(ai.locator('.cpa-account-state button[value="degraded"]')).toHaveAttribute('aria-pressed', 'true');
    await cards.evaluateAll(elements => elements.forEach(card => card.setAttribute('title', 'shared-display-name')));
    for (const card of [cn, ai]) {
      await expect(card.locator('.cpa-account-state')).toHaveCount(1);
      await expect(card.locator('.cpa-account-state button')).toHaveCount(3);
      await expect(card.locator('[class*="AuthFileCard-module__toggleWrap"]')).toBeHidden();
    }
    await cards.evaluateAll(elements => elements.forEach(card => {
      card.classList.remove('cpa-account-managed');
      card.querySelector('[class*="AuthFileCard-module__toggleWrap"]').classList.remove('cpa-native-status-hidden');
    }));
    await expect(cn.locator('[class*="AuthFileCard-module__toggleWrap"]')).toBeHidden();
    await expect(ai.locator('[class*="AuthFileCard-module__toggleWrap"]')).toBeHidden();
    await cn.locator('.cpa-account-state').evaluate(control => control.remove());
    await expect(cn.locator('.cpa-account-state')).toHaveCount(1);
    await cn.evaluate(card => card.setAttribute('data-cpa-auth-name', 'unmatched.json'));
    const unmatched = page.locator('article[data-cpa-auth-name="unmatched.json"]');
    await expect(unmatched.locator('.cpa-account-state')).toHaveCount(0);
    await expect(unmatched.locator('[class*="AuthFileCard-module__toggleWrap"]')).toBeVisible();
    await unmatched.evaluate(card => card.setAttribute('data-cpa-auth-name', 'workbuddy-browser.json'));
    await expect(cn.locator('.cpa-account-state')).toHaveCount(1);
    for (const width of [1440, 390]) {
      await page.setViewportSize({width, height:960});
      for (const state of ['degraded', 'disabled', 'normal']) {
        await cn.locator('.cpa-account-state button[value="' + state + '"]').click();
        await expect(cn.locator('.cpa-account-state button[value="' + state + '"]')).toHaveAttribute('aria-pressed', 'true');
        assert.equal(lastPatch.name, 'workbuddy-browser.json');
        await expect(ai.locator('.cpa-account-state button[value="degraded"]')).toHaveAttribute('aria-pressed', 'true');
        await expect(cn.locator('[class*="AuthFileCard-module__toggleWrap"]')).toBeHidden();
      }
      const geometry = await cn.locator('.cpa-account-state button').evaluateAll(buttons => buttons.map(button => {
        const box = button.getBoundingClientRect();
        return {left:box.left, right:box.right, top:box.top, bottom:box.bottom};
      }));
      assert.ok(geometry.every(box => box.left >= 0 && box.right <= width));
      assert.ok(geometry.every((box, index) => index===0 || box.left >= geometry[index-1].right || box.top >= geometry[index-1].bottom), "status buttons must not overlap");
      await page.screenshot({path:path.join(root, "workbuddy-account-state-"+width+".png")});
    }
    await page.reload();
    await expect(cn.locator('.cpa-account-state button[value="normal"]')).toHaveAttribute('aria-pressed', 'true');
    await expect(ai.locator('.cpa-account-state button[value="degraded"]')).toHaveAttribute('aria-pressed', 'true');
    assert.deepEqual(errors, []);
    console.log("WorkBuddy account identity, native control hiding, rerenders, state PATCH and desktop/mobile layouts passed");
  } finally {
    await page.close();
  }
}

async function main() {
  upstream = http.createServer(async (req, res) => {
    let raw = "";
    for await (const chunk of req) raw += chunk;
    const body = JSON.parse(raw);
    calls.push(body);
    const user = body.messages.filter((m) => m.role === "user").at(-1).content;
    const tools = body.messages.filter((m) => m.role === "tool");
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    const chunk = (delta, finish = null) =>
      res.write(
        "data: " +
          JSON.stringify({
            id: "chat-test",
            object: "chat.completion.chunk",
            model: body.model,
            choices: [{ index: 0, delta, finish_reason: finish }],
          }) +
          "\n\n",
      );
    if (user === "plain-chat") {
      assert.equal(body.tool_choice, "auto");
      assert.ok(
        body.messages[0].content.includes("Answer the user directly in chat"),
      );
      assert.ok(
        !body.messages[0].content.includes(
          "Work only through the provided tools",
        ),
      );
      chunk(
        { content: "Direct chat reply without files or commands." },
        "stop",
      );
      res.end("data: [DONE]\n\n");
      return;
    }
    if (user === "tool-limit") {
      if (body.tool_choice === "none") {
        assert.equal(tools.length, 12);
        assert.ok(
          body.messages.at(-1).content.includes("No tool calls remain"),
        );
        chunk(
          {
            content:
              "Tool limit reached. The workspace is empty; no files were changed.",
          },
          "stop",
        );
      } else {
        assert.equal(body.tool_choice, "auto");
        chunk(
          {
            tool_calls: [
              {
                index: 0,
                id: "list-" + tools.length,
                type: "function",
                function: { name: "list_files", arguments: "{}" },
              },
            ],
          },
          "tool_calls",
        );
      }
      res.end("data: [DONE]\n\n");
      return;
    }
    if (tools.length === 0) {
      assert.equal(body.reasoning_effort, "high");
      assert.ok(body.tools.some((t) => t.function.name === "run_command"));
      chunk({ content: "I will create the files for " + user + ".\n" });
      gates.set(user, () => {
        const args = JSON.stringify({
          path: user + "/report.md",
          content: toolContent,
        });
        chunk({
          tool_calls: [
            {
              index: 0,
              id: "write-" + user,
              type: "function",
              function: { name: "write_file", arguments: args.slice(0, 20) },
            },
          ],
        });
        chunk({
          tool_calls: [{ index: 0, function: { arguments: args.slice(20) } }],
        });
        const command =
          user === "gamma"
            ? "printf started > running.txt; while true; do sleep 1; done"
            : "printf sandbox-ready > shell.txt";
        chunk(
          {
            tool_calls: [
              {
                index: 1,
                id: "cmd-" + user,
                type: "function",
                function: {
                  name: "run_command",
                  arguments: JSON.stringify({ command }),
                },
              },
            ],
          },
          "tool_calls",
        );
        res.end("data: [DONE]\n\n");
      });
    } else {
      if (
        !tools.some(
          (t) =>
            t.content.includes("sandbox-ready") ||
            t.content.includes("Exit code: 0"),
        )
      )
        mockFailure = JSON.stringify(tools);
      chunk(
        {
          content:
            "## Completed " +
            user +
            '\n\n```javascript\nconsole.log("ready");\n```\n\n| File | State |\n| --- | --- |\n| report.md | ready |\n\n<img src=x onerror="window.pwned=1"><script>window.pwned=1</script>\n',
        },
        "stop",
      );
      res.end("data: [DONE]\n\n");
    }
    running.set(user, res);
  });
  const upstreamPort = await listen(upstream);
  const reservation = http.createServer();
  const port = await listen(reservation);
  await new Promise((resolve) => reservation.close(resolve));
  const config = {
    host: "127.0.0.1",
    port,
    "auth-dir": path.join(root, "auths"),
    "api-keys": ["agent-browser-test-key"],
    "remote-management": {
      "allow-remote": false,
      "secret-key": managementKey,
      "disable-auto-update-panel": true,
    },
    "request-retry": 0,
    codex: { "quota-refresh": { enabled: false } },
    "account-scheduling": {
      "persist-settings": true,
      "fake-models": [{ name: "agent-fake", responses: ["Excluded"] }],
    },
    "openai-compatibility": [
      {
        name: "agent-test-provider",
        priority: 10,
        "base-url": "http://127.0.0.1:" + upstreamPort + "/v1",
        "api-key-entries": [{ "api-key": "synthetic-upstream-key" }],
        models: [
          {
            name: "agent-code",
            alias: "agent-code",
            thinking: { levels: ["low", "medium", "high"] },
          },
          {
            name: "agent-review",
            alias: "agent-review",
            thinking: { levels: ["low", "high"] },
          },
        ],
      },
      {
        name: "agent-config-only",
        "base-url": "http://127.0.0.1:" + upstreamPort + "/v1",
        "api-key-entries": [{ "api-key": "synthetic-config-only-key" }],
        models: [{ name: "agent-config-only" }],
      },
    ],
  };
  fs.mkdirSync(config["auth-dir"]);
  // This file supplies the catalog; config credentials serve the synthetic upstream.
  fs.writeFileSync(
    path.join(config["auth-dir"], "agent-browser.json"),
    JSON.stringify({
      type: "agent-test-provider",
      access_token: "synthetic-file-token",
      priority: -10,
    }),
  );
  fs.writeFileSync(path.join(root, "config.yaml"), JSON.stringify(config));
  service = spawn(
    binary,
    ["--config", path.join(root, "config.yaml"), "--local-model"],
    {
      cwd: root,
      env: {
        ...process.env,
        MANAGEMENT_STATIC_PATH: path.join(repo, "static"),
        CPA_AGENT_WORKSPACE_DIR: path.join(root, "agent"),
        CPA_AGENT_SANDBOX_IMAGE:
          process.env.CPA_AGENT_TEST_IMAGE || "alpine:latest",
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  service.stdout.on("data", (data) => (output += data));
  service.stderr.on("data", (data) => (output += data));
  const base = "http://127.0.0.1:" + port;
  const request = async (url, options = {}) =>
    fetch(base + "/v0/management/agent/" + url, {
      ...options,
      headers: {
        Authorization: "Bearer " + managementKey,
        "Content-Type": "application/json",
        ...options.headers,
      },
    });
  const setAuthDisabled = async (disabled) => {
    const response = await fetch(base + "/v0/management/auth-files/status", {
      method: "PATCH",
      headers: {
        Authorization: "Bearer " + managementKey,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ name: "agent-browser.json", disabled }),
    });
    assert.equal(response.status, 200, await response.text());
  };
  apiRequest = request;
  let failure;
  for (let i = 0; i < 100; i++) {
    try {
      const response = await request("models");
      if (response.ok) {
        const models = await response.json();
        if (models.models.length === 2) break;
      }
    } catch (error) {
      failure = error;
    }
    if (service.exitCode !== null) throw new Error("CPA exited " + output);
    await new Promise((resolve) => setTimeout(resolve, 100));
    if (i === 99)
      throw failure || new Error("CPA did not register test models");
  }
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CPA_AGENT_CHROMIUM,
  });
  const context = await browser.newContext({
    viewport: { width: 1440, height: 960 },
    acceptDownloads: true,
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(base + "/management.html");
  await page.locator('input[type="password"]').fill(managementKey);
  await page.getByRole("checkbox", { name: "记住密码" }).evaluate((el) => {
    if (!el.checked) el.click();
  });
  await page.getByRole("button", { name: "登录", exact: true }).click();
  const launch = page.locator("#cpa-agent-nav");
  await expect(launch).toBeVisible();
  await verifyAccountStateControls(context, base);
  await verifyModelTooltips(context, base);
  await verifyRollingTokenTooltip(context, base);
  if (process.env.CPA_AGENT_TOOLTIP_ONLY === '1') {
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({result:'PASS',accountModelTokens:true,noWrapping:true,allViewports:[1440,768,390,320],artifacts:root}));
    return;
  }
  await expect(launch).toHaveClass("nav-item");
  await expect(
    page.locator(".sidebar .nav-section #cpa-agent-nav"),
  ).toHaveCount(1);
  await launch.evaluate((el) => {
    el.click();
    el.click();
  });
  const workspace = page.locator("#cpa-agent-workspace");
  await expect(workspace.locator(".model option")).toHaveCount(2);
  assert.equal(
    await workspace.evaluate((el) => el.parentElement.className),
    "content cpa-agent-page",
  );
  assert.notEqual(
    await workspace
      .locator(".workspace")
      .evaluate((el) => getComputedStyle(el).position),
    "fixed",
  );
  assert.equal(
    await workspace.locator(".workspace").getAttribute("aria-modal"),
    null,
  );
  await expect(page.locator(".sidebar .nav-section")).toBeVisible();
  await expect(page.locator(".main-header")).toBeVisible();
  await expect(workspace.locator(".composer .model")).toHaveCount(1);
  await expect(workspace.locator(".composer .effort")).toHaveCount(1);
  await expect(workspace.locator(".composer .upload-button")).toHaveCount(1);
  for (const [prompt, answer] of [
    ["plain-chat", "Direct chat reply without files or commands."],
    [
      "tool-limit",
      "Tool limit reached. The workspace is empty; no files were changed.",
    ],
  ]) {
    const created = await request("conversations", {
      method: "POST",
      body: JSON.stringify({ model: "agent-code", reasoning_effort: "high" }),
    });
    assert.equal(created.status, 201);
    const conversation = await created.json();
    const path = "conversations/" + conversation.id;
    const sent = await request(path + "/messages", {
      method: "POST",
      body: JSON.stringify({ message: prompt }),
    });
    assert.equal(sent.status, 202);
    await expect
      .poll(async () => (await (await request(path)).json()).status)
      .toBe("idle");
    const snapshot = await (await request(path)).json();
    assert.equal(snapshot.messages.at(-1).content, answer);
    assert.equal(
      snapshot.messages.filter((m) => m.role === "tool").length,
      prompt === "plain-chat" ? 0 : 12,
    );
    assert.deepEqual((await (await request(path + "/files")).json()).files, []);
    const pick = workspace.locator(
      '.session[data-id="' + conversation.id + '"] .session-pick',
    );
    await expect(pick).toBeVisible({ timeout: 10000 });
    await pick.click();
    await expect(workspace.locator(".messages")).toContainText(answer);
    const removed = await request(path, { method: "DELETE" });
    assert.equal(removed.status, 204);
    await expect(pick).toHaveCount(0, { timeout: 10000 });
  }
  console.log(
    "direct chat, tool-limit final answer and visible replies passed",
  );
  await expect(page.locator(".cpa-runtime-scheduler-toggle")).toBeHidden();
  await workspace
    .getByRole("button", { name: "账号调度", exact: true })
    .click();
  await expect(page.locator("#cpa-runtime-scheduler-panel")).toBeVisible();
  await workspace
    .getByRole("button", { name: "账号调度", exact: true })
    .click();
  await expect(page.locator("#cpa-runtime-scheduler-panel")).toBeHidden();
  await workspace
    .getByRole("button", { name: "返回管理页", exact: true })
    .click();
  await expect(workspace).toBeHidden();
  await expect(page.locator(".cpa-runtime-scheduler-toggle")).toBeVisible();
  await expect(page.locator("#root[inert]")).toHaveCount(0);
  await launch.click();
  await page.locator('.sidebar a[href="#/auth-files"]').click();
  await expect(workspace).toBeHidden();
  await expect(page.locator(".main-content")).toBeVisible();
  for (const route of ["#/auth-files", "#/config", "#/logs", "#/"]) {
    const nativeLink = page.locator('.sidebar a[href="' + route + '"]');
    await nativeLink.click();
    await expect(page).toHaveURL(base + "/management.html" + route);
    await expect(nativeLink).toHaveClass(/\bactive\b/);
    await expect(page.locator(".main-content")).toBeVisible();
    await launch.evaluate((el) => {
      el.click();
      el.click();
    });
    await expect(workspace.locator(".workspace")).toBeVisible();
    await workspace
      .getByRole("button", { name: "返回管理页", exact: true })
      .click();
    await expect(page.locator("#root[inert]")).toHaveCount(0);
    await expect(page.locator(".main-content")).toBeVisible();
  }
  await launch.click();
  await setAuthDisabled(true);
  await expect(workspace.locator(".model")).toBeDisabled({ timeout: 10000 });
  await expect(workspace.locator(".model")).toContainText("暂无可用模型");
  await setAuthDisabled(false);
  await expect(workspace.locator(".model")).toBeEnabled({ timeout: 10000 });
  await expect(workspace.locator(".model option")).toHaveCount(2);
  await workspace.locator(".model").selectOption("agent-code");
  await workspace.locator(".effort").selectOption("high");
  await workspace.locator(".prompt").fill("alpha");
  await workspace
    .getByRole("button", { name: "发送消息", exact: true })
    .click();
  await expect.poll(() => gates.has("alpha")).toBeTruthy();
  const firstID = (await (await request("conversations")).json())
    .conversations[0].id;
  await workspace
    .locator(".sidebar")
    .getByRole("button", { name: "新建对话", exact: true })
    .click();
  await workspace.locator(".model").selectOption("agent-review");
  await workspace.locator(".effort").selectOption("high");
  await workspace.locator(".prompt").fill("beta");
  await workspace
    .getByRole("button", { name: "发送消息", exact: true })
    .click();
  await expect.poll(() => gates.has("beta")).toBeTruthy();
  const summaries = (await (await request("conversations")).json())
    .conversations;
  assert.equal(summaries.filter((c) => c.status === "running").length, 2);
  const secondID = summaries.find((c) => c.id !== firstID).id;
  await page.reload();
  await page.evaluate(async (key) => {
    await fetch("/v8/management/agent/models", {
      headers: { Authorization: "Bearer " + key },
    });
  }, managementKey);
  await expect(workspace.locator(".workspace")).toBeVisible();
  await page.evaluate(async (key) => {
    await fetch("/v0/management/agent/models", {
      headers: { Authorization: "Bearer " + key },
    });
  }, managementKey);
  await expect(workspace.locator(".workspace")).toBeVisible();
  gates.get("beta")();
  gates.get("alpha")();
  await expect
    .poll(
      async () => {
        const rows = (await (await request("conversations")).json())
          .conversations;
        return rows.every((c) => c.status === "idle");
      },
      { timeout: 60000 },
    )
    .toBeTruthy();
  assert.equal(mockFailure, undefined, "tool execution failed: " + mockFailure);
  await expect(workspace.locator(".messages")).toContainText("Completed beta");
  assert.equal(
    await workspace
      .locator(".messages")
      .getByText("Completed alpha", { exact: true })
      .count(),
    0,
  );
  assert.equal(await page.evaluate(() => window.pwned), undefined);
  await expect(workspace.locator(".content table")).toHaveCount(1);
  await workspace
    .locator('.session[data-id="' + firstID + '"] .session-pick')
    .click();
  await expect(workspace.locator(".messages")).toContainText("Completed alpha");
  await expect(
    workspace.getByRole("button", {
      name: "下载 alpha/report.md",
      exact: true,
    }),
  ).toBeVisible();
  const downloadEvent = page.waitForEvent("download");
  await workspace
    .getByRole("button", { name: "下载 alpha/report.md", exact: true })
    .click();
  const download = await downloadEvent;
  assert.equal(fs.readFileSync(await download.path(), "utf8"), toolContent);
  const shellFile = await request(
    "conversations/" + firstID + "/files?path=shell.txt",
  );
  assert.equal(await shellFile.text(), "sandbox-ready");
  for (const width of [1440, 768, 390, 320]) {
    await page.setViewportSize({ width, height: 900 });
    if (width <= 768) {
      await expect
        .poll(() =>
          page
            .locator(".app-shell .main-body > .sidebar")
            .evaluate((el) => el.getBoundingClientRect().right <= 0),
        )
        .toBeTruthy();
    }
    await page.screenshot({
      path: path.join(root, "agent-" + width + ".png"),
      fullPage: true,
    });
    const overflow = await workspace
      .locator(".workspace")
      .evaluate(
        (el) =>
          el.scrollWidth > el.clientWidth ||
          el.querySelector(".chat").scrollWidth >
            el.querySelector(".chat").clientWidth,
      );
    assert.equal(overflow, false, "horizontal overflow at " + width);
    const composer = await workspace.locator(".composer").boundingBox();
    assert.ok(
      composer.y + composer.height <= 900,
      "composer outside viewport at " + width,
    );
    const controls = await workspace
      .locator(".composer-actions")
      .evaluate((el) => {
        const boxes = Array.from(
          el.querySelectorAll(".upload-button,.model,.effort,.send,.stop"),
        )
          .filter((n) => !n.hidden)
          .map((n) => n.getBoundingClientRect());
        return boxes.every((r, i) => !i || r.left >= boxes[i - 1].right);
      });
    assert.equal(controls, true, "composer controls overlap at " + width);
    const hit = await workspace.locator(".send").evaluate((el) => {
      const r = el.getBoundingClientRect();
      return (
        document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2).id ===
        "cpa-agent-workspace"
      );
    });
    assert.equal(hit, true, "send button is covered at " + width);
    if (width <= 390) {
      await page
        .getByRole("button", { name: "展开侧边栏", exact: true })
        .click();
      await expect(launch).toBeVisible();
      await launch.click();
      await expect(page.locator(".sidebar-backdrop")).not.toHaveClass(
        /visible/,
      );
      await expect(workspace.locator(".workspace")).toBeVisible();
    }
  }
  await page.setViewportSize({ width: 1440, height: 960 });
  await page.getByRole("button", { name: "主题", exact: true }).click();
  await page.locator(".theme-card").filter({ hasText: "暗色" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  assert.equal(
    await workspace
      .locator(".workspace")
      .evaluate((el) => getComputedStyle(el).getPropertyValue("--aw-bg")),
    await page
      .locator(".sidebar")
      .first()
      .evaluate((el) =>
        getComputedStyle(el).getPropertyValue("--bg-secondary"),
      ),
  );
  await page.screenshot({
    path: path.join(root, "agent-dark.png"),
    fullPage: true,
  });
  await workspace
    .getByRole("button", { name: "删除 alpha", exact: true })
    .click();
  const deleteResponse = page.waitForResponse(
    (r) => r.request().method() === "DELETE" && r.url().endsWith("/" + firstID),
  );
  await workspace
    .locator(".confirm")
    .getByRole("button", { name: "删除", exact: true })
    .click();
  assert.equal((await deleteResponse).status(), 204);
  await expect
    .poll(async () => (await request("conversations/" + firstID)).status)
    .toBe(404);
  assert.equal(
    fs.existsSync(path.join(root, "agent", "workspaces", firstID)),
    false,
  );
  assert.equal(
    (await request("conversations/" + firstID + "/files?path=alpha/report.md"))
      .status,
    404,
  );
  await page.reload();
  await page.evaluate(async (key) => {
    await fetch("/v0/management/agent/models", {
      headers: { Authorization: "Bearer " + key },
    });
  }, managementKey);
  await expect(workspace.locator(".workspace")).toBeVisible();
  await expect(workspace.locator(".messages")).toContainText("Completed beta");
  const deleteSecond = await request("conversations/" + secondID, {
    method: "DELETE",
  });
  assert.equal(deleteSecond.status, 204);
  const gamma = await (
    await request("conversations", {
      method: "POST",
      body: JSON.stringify({ model: "agent-code", reasoning_effort: "high" }),
    })
  ).json();
  assert.equal(
    (
      await request("conversations/" + gamma.id + "/messages", {
        method: "POST",
        body: JSON.stringify({ message: "gamma" }),
      })
    ).status,
    202,
  );
  await expect.poll(() => gates.has("gamma")).toBeTruthy();
  gates.get("gamma")();
  await expect
    .poll(
      async () => {
        const data = await (
          await request("conversations/" + gamma.id + "/files")
        ).json();
        return data.files.some((f) => f.path === "running.txt");
      },
      { timeout: 30000 },
    )
    .toBeTruthy();
  assert.equal(
    (await request("conversations/" + gamma.id, { method: "DELETE" })).status,
    204,
  );
  assert.equal(
    fs.existsSync(path.join(root, "agent", "workspaces", gamma.id)),
    false,
  );
  await page.getByRole("button", { name: "登出", exact: true }).click();
  await expect(page.locator('input[type="password"]')).toBeVisible();
  await expect(page.locator("#root[inert]")).toHaveCount(0);
  await page.locator('input[type="password"]').fill(managementKey);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(launch).toBeVisible();
  await launch.click();
  await expect(workspace.locator(".model option")).toHaveCount(2);
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify({
      result: "PASS",
      parallel: true,
      modelAndReasoning: true,
      toolLoop: true,
      dockerCommand: true,
      download: true,
      deletion: true,
      deleteRunningCommand: true,
      reloadWhileRunning: true,
      nativeSidebar: true,
      closeAndNativeNavigation: true,
      enabledAuthFileModelsOnly: true,
      composerSelectors: true,
      authV0V8: true,
      xssBlocked: true,
      viewports: [1440, 768, 390, 320],
      artifacts: root,
    }),
  );
}
main()
  .catch((error) => {
    console.error(error);
    console.error("Artifacts:", root);
    console.error(output.slice(-9000));
    process.exitCode = 1;
  })
  .finally(async () => {
    if (browser) {
      if (process.exitCode) {
        const page = browser.contexts()[0]?.pages()[0];
        if (page)
          await page.screenshot({
            path: path.join(root, "failure.png"),
            fullPage: true,
          });
      }
      await browser.close();
    }
    if (service && service.exitCode === null && apiRequest) {
      try {
        const list = await (await apiRequest("conversations")).json();
        for (const c of list.conversations)
          await apiRequest("conversations/" + c.id, { method: "DELETE" });
      } catch (error) {
        console.error("Test cleanup:", error.message);
      }
    }
    if (service) {
      service.kill();
      await new Promise((resolve) => {
        if (service.exitCode !== null) resolve();
        else service.once("exit", resolve);
      });
    }
    for (const res of running.values()) res.destroy();
    if (upstream) upstream.close();
  });
