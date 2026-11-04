'use strict';

const assert = require('node:assert/strict');
const { spawn } = require('node:child_process');
const { once } = require('node:events');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

// Regression test for the runner's stdin contract: Go writes exactly one JSON
// line and keeps stdin open until the call returns. The runner must start work
// on that first line instead of blocking until stdin EOF, and must not treat
// its own readline close as a failure.
test('runner starts on a single stdin line without waiting for EOF', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cpa-auto-oauth-stub-'));
  const stub = path.join(dir, 'playwright-stub.cjs');
  // The stub records that launch() was reached, so a parse/close regression
  // fails loudly instead of being masked by an error result line.
  fs.writeFileSync(
    stub,
    "const fs = require('node:fs');\n" +
      "module.exports = { chromium: { launch: async () => { fs.writeFileSync(process.env.CPA_STUB_MARKER, 'launched'); throw new Error('stub'); } } };\n"
  );
  const marker = path.join(dir, 'launched.marker');

  const child = spawn(process.execPath, [path.join(__dirname, 'auto_oauth_runner.cjs')], {
    env: { ...process.env, CPA_CODEX_AUTO_OAUTH_PLAYWRIGHT: stub, CPA_STUB_MARKER: marker },
    stdio: ['pipe', 'pipe', 'pipe'],
  });

  let output = '';
  let killed = false;
  child.stdout.on('data', chunk => { output += chunk.toString(); });

  const finished = new Promise(resolve => child.on('close', resolve));
  // Intentionally keep stdin open to mirror the Go caller.
  child.stdin.write(JSON.stringify({ account: { email: 'a@b.test' }, auth_url: 'http://127.0.0.1/never', redirect_uri: 'http://localhost:1455/auth/callback' }) + '\n');

  const timer = setTimeout(() => { killed = true; child.kill('SIGKILL'); }, 15000);
  await finished;
  clearTimeout(timer);

  assert.equal(killed, false, 'runner must exit on its own; a lingering process would block Go Wait()');
  assert.ok(fs.existsSync(marker), 'runner must reach browser launch instead of failing during stdin parse');
  assert.doesNotMatch(output, /browser_unavailable/, 'runner must not report browser_unavailable for a valid payload');
});

// Go invokes the runner through `node -e`, where require.main !== module. A
// `require.main === module` entry guard therefore skips main() entirely and the
// service reports browser_unavailable. This test pins the entry contract.
test('runner executes when launched the way the Go caller does (node -e)', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cpa-auto-oauth-eval-'));
  const stub = path.join(dir, 'playwright-stub.cjs');
  fs.writeFileSync(
    stub,
    "const fs = require('node:fs');\n" +
      "module.exports = { chromium: { launch: async () => { fs.writeFileSync(process.env.CPA_STUB_MARKER, 'launched'); throw new Error('stub'); } } };\n"
  );
  const marker = path.join(dir, 'launched.marker');
  const source = fs.readFileSync(path.join(__dirname, 'auto_oauth_runner.cjs'), 'utf8');

  const child = spawn(process.execPath, ['-e', source], {
    env: { ...process.env, CPA_CODEX_AUTO_OAUTH_PLAYWRIGHT: stub, CPA_STUB_MARKER: marker },
    stdio: ['pipe', 'pipe', 'pipe'],
  });

  let output = '';
  let killed = false;
  child.stdout.on('data', chunk => { output += chunk.toString(); });

  const finished = new Promise(resolve => child.on('close', resolve));
  // Keep stdin open, matching the real caller.
  child.stdin.write(JSON.stringify({ account: { email: 'a@b.test' }, auth_url: 'http://127.0.0.1/never', redirect_uri: 'http://localhost:1455/auth/callback' }) + '\n');

  const timer = setTimeout(() => { killed = true; child.kill('SIGKILL'); }, 15000);
  await finished;
  clearTimeout(timer);

  assert.equal(killed, false, 'runner must exit when invoked via node -e');
  assert.ok(fs.existsSync(marker), 'runner must run main() under node -e (require.main guard must not gate entry)');
});

test('Chromium gets an isolated writable HOME which is cleaned after launch failure', async t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cpa-oauth-home-test-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const marker = path.join(dir, 'browser-home.json');
  const stub = path.join(dir, 'playwright.cjs');
  fs.writeFileSync(stub, `const fs = require('node:fs');
module.exports = { chromium: { launch: async options => {
  fs.writeFileSync(process.env.CPA_STUB_MARKER, JSON.stringify({env:options.env}));
  throw new Error('fixture launch failure');
} } };`);
  const source = fs.readFileSync(path.join(__dirname, 'auto_oauth_runner.cjs'), 'utf8');
  const child = spawn(process.execPath, ['-e', source], {
    env: { ...process.env, CPA_CODEX_AUTO_OAUTH_LIB: '0', CPA_CODEX_AUTO_OAUTH_PROXY: '', CPA_CODEX_AUTO_OAUTH_PLAYWRIGHT: stub, CPA_STUB_MARKER: marker },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  let output = '';
  child.stdout.on('data', data => { output += data; });
  child.stderr.resume();
  const finished = new Promise((resolve, reject) => { child.once('close', resolve); child.once('error', reject); });
  const timer = setTimeout(() => child.kill(), 10000);
  t.after(() => { clearTimeout(timer); child.kill(); });
  child.stdin.write(JSON.stringify({ account: {}, auth_url: 'https://oauth.example.test/start', redirect_uri: 'http://localhost:1455/auth/callback' }) + '\n');
  await finished;
  const launch = JSON.parse(fs.readFileSync(marker, 'utf8'));
  assert.ok(launch.env && launch.env.HOME, 'launch must supply its own writable HOME');
  assert.notEqual(launch.env.HOME, process.env.HOME);
  assert.equal(fs.existsSync(launch.env.HOME), false, 'temporary browser HOME must be removed');
  assert.match(output, /browser_launch_failed/);
});

const fixturePlaywright = process.env.CPA_CODEX_AUTO_OAUTH_TEST_PLAYWRIGHT;
test('runner captures and validates HTTP redirect callbacks', { skip: !fixturePlaywright }, async t => {
  process.env.CPA_CODEX_AUTO_OAUTH_LIB = '1';
  const { run } = require('./auto_oauth_runner.cjs');
  const { chromium } = require(fixturePlaywright);
  let fixture;
  let callback;
  let seen;
  const server = http.createServer((request, response) => {
    const pathname = new URL(request.url, 'http://fixture.test').pathname;
    seen.push(request.method + ' ' + pathname);
    request.resume();
    if (pathname === '/auth/callback') return;
    if (pathname === '/authorize' && !fixture.noCallback) {
      response.writeHead(fixture.redirectStatus || 303, { Location: '/resume' });
    } else if (pathname === '/resume' || fixture.initialRedirect) {
      response.writeHead(302, { Location: callback + fixture.query });
    } else {
      response.writeHead(200, { 'Content-Type': 'text/html' });
      if (fixture.subresource) {
        response.write('<h1>Waiting</h1><img src="' + callback + fixture.query + '">');
      } else {
        response.write('<h1>Sign in to Codex</h1><form method="post" action="/authorize"><button type="submit">Continue</button></form>');
      }
    }
    response.end();
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => {
    server.closeAllConnections();
    return new Promise(resolve => server.close(resolve));
  });
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const origin = 'http://127.0.0.1:' + server.address().port;
  const validQuery = '?code=fixture&state=expected';
  for (const scenario of [
    { name: 'authorization POST via 303 and 302', query: validQuery },
    { name: '307 preserves POST through the redirect chain', query: validQuery, redirectStatus: 307 },
    { name: 'callback server never responds', query: validQuery, pendingCallback: true },
    { name: 'initial authorization URL redirects to callback', query: validQuery, initialRedirect: true },
    { name: 'initial redirect callback server never responds', query: validQuery, initialRedirect: true, pendingCallback: true },
    { name: 'wrong state', query: '?code=fixture&state=wrong', error: 'invalid_callback' },
    { name: 'duplicate state', query: validQuery + '&state=expected', error: 'invalid_callback' },
    { name: 'duplicate code', query: validQuery + '&code=second', error: 'invalid_callback' },
    { name: 'missing code', query: '?state=expected', error: 'invalid_callback' },
    { name: 'missing query', query: '', error: 'invalid_callback' },
    { name: 'OAuth error with a code', query: validQuery + '&error=access_denied', error: 'invalid_callback' },
    { name: 'initial redirect with wrong state', query: '?code=fixture&state=wrong', initialRedirect: true, error: 'invalid_callback' },
    { name: 'subresource cannot supply a callback', query: validQuery, subresource: true, error: 'login_timeout' },
    { name: 'consent click alone does not complete authorization', query: validQuery, noCallback: true, error: 'login_timeout' },
  ]) {
    await t.test(scenario.name, { timeout: 10000 }, async subtest => {
      fixture = scenario;
      seen = [];
      callback = (scenario.pendingCallback ? origin : 'http://localhost:1455') + '/auth/callback';
      const context = await browser.newContext();
      subtest.after(() => context.close());
      const steps = [];
      const result = run({ account: {}, auth_url: origin + '/start?state=expected', redirect_uri: callback, timeout_ms: 2000 }, (step, percent) => steps.push({ step, percent }), { chromium: { launch: async () => ({ newContext: async () => context, close: async () => context.close() }) } });
      if (scenario.error) {
        await assert.rejects(result, error => error.code === scenario.error);
        assert.equal(steps.some(entry => entry.percent === 78), false, 'invalid or absent callback must not complete authorization');
      } else {
        assert.equal(new URL((await result).callback).searchParams.get('code'), 'fixture');
        assert.ok(steps.some(entry => entry.percent === 78), 'redirect callback must complete without loading the callback page');
      }
      if (!scenario.initialRedirect && !scenario.subresource) {
        assert.ok(seen.includes('POST /authorize'));
        assert.ok(steps.some(entry => entry.percent === 76), 'fixture must reach the real waiting-for-callback phase');
        if (!scenario.noCallback) assert.ok(seen.includes((scenario.redirectStatus === 307 ? 'POST' : 'GET') + ' /resume'));
      }
    });
  }
});

test('actual runner waits for each delayed page, chooses Team and captures callback without a port listener', { skip: !fixturePlaywright }, async t => {
  process.env.CPA_CODEX_AUTO_OAUTH_LIB = '1';
  const { run } = require('./auto_oauth_runner.cjs');
  const { chromium } = require(fixturePlaywright);
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const context = await browser.newContext();
  const origin = 'https://oauth.example.test';
  const callback = 'http://localhost:1455/auth/callback';
  const seen = [];
  const stageHTML = (name, input, next) => `<h1>${name}</h1><div id="fields"></div><script>setTimeout(()=>{document.getElementById('fields').innerHTML=${JSON.stringify('<form><input '+input+'><button type="submit">Continue</button></form>')}; document.querySelector('form').onsubmit=e=>{e.preventDefault();location.href=${JSON.stringify(next)}}},150)</script>`;
  const pages = {
    '/start': stageHTML('Sign in', 'type="email" name="email"', '/password'),
    '/password': stageHTML('Password', 'type="password" name="password"', '/mfa'),
    '/mfa': stageHTML('Authenticator code', 'name="code" autocomplete="one-time-code"', '/workspaces'),
    '/workspaces': `<h1>Choose a workspace</h1><form><select name="workspace_id"><option value="personal">Personal</option><option value="team">Example Team</option></select><button>Continue</button></form><script>document.querySelector('form').onsubmit=e=>{e.preventDefault();location.href='/consent?chosen='+document.querySelector('select').value}</script>`,
    '/consent': `<h1>Sign in to Codex</h1><button onclick="location.href='${callback}?code=fixture&state=expected'">Continue</button>`,
  };
  await context.route('**/*', route => {
    const url = new URL(route.request().url());
    seen.push(url.pathname + url.search);
    if (url.origin !== origin) return route.abort();
    return route.fulfill({ contentType: 'text/html', body: pages[url.pathname] || '<h1>Not found</h1>' });
  });
  const steps = [];
  const result = await run({ account: { email: 'example@example.test', password: 'test-only', totp_secret: 'JBSWY3DPEHPK3PXP' }, auth_url: origin+'/start?state=expected', redirect_uri: callback, timeout_ms: 12000, workspace_id: '', workspace_name: '' }, (step, percent) => steps.push({ step, percent }), { chromium: { launch: async () => ({ newContext: async () => context, close: async () => context.close() }) } });
  assert.equal(result.team_selected, true);
  assert.equal(result.workspace_id, 'team');
  assert.ok(seen.includes('/consent?chosen=team'));
  assert.equal(new URL(result.callback).searchParams.get('code'), 'fixture');
  for (const step of ['填写邮箱', '填写密码', '填写 2FA 验证码', '选择工作空间', '确认 Codex 授权', '收到 OAuth 回调']) {
    assert.ok(steps.some(entry => entry.step === step), 'missing real phase: '+step);
  }
});

test('account API challenge responses retain the failed login phase', { skip: !fixturePlaywright }, async t => {
  process.env.CPA_CODEX_AUTO_OAUTH_LIB = '1';
  const { run } = require('./auto_oauth_runner.cjs');
  const { chromium } = require(fixturePlaywright);
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const origin = 'https://oauth.example.test';
  for (const scenario of [
    { name: 'fetch challenge header with opaque HTML', headers: { 'cf-mitigated': 'challenge' }, body: '<p>Blocked</p>', error: 'cloudflare_blocked' },
    { name: 'fetch challenge HTML without header', body: '<title>Just a moment...</title><p>Enable JavaScript and cookies to continue</p>', error: 'cloudflare_blocked' },
    { name: 'XHR challenge header', transport: 'xhr', headers: { 'cf-mitigated': 'challenge' }, body: '<p>Blocked</p>', error: 'cloudflare_blocked' },
    { name: 'invalid password JSON is preserved', contentType: 'application/json', body: '{"error":{"code":"invalid_password"}}', error: 'invalid_credentials' },
    { name: 'deactivated account JSON is preserved', contentType: 'application/json', body: '{"error":{"code":"account_deactivated"}}', error: 'account_banned' },
    { name: 'ordinary HTML denial is not a challenge', body: '<p>Access denied</p>', error: 'login_timeout' },
    { name: 'successful HTML is not a challenge failure', status: 200, body: '<p>Just a moment</p>', error: 'login_timeout' },
    { name: 'third-party challenge is not an account failure', thirdParty: true, headers: { 'cf-mitigated': 'challenge' }, body: '<p>Just a moment</p>', error: 'login_timeout' },
    { name: 'image challenge is not an account failure', transport: 'image', headers: { 'cf-mitigated': 'challenge' }, body: '<p>Just a moment</p>', error: 'login_timeout' },
  ]) {
    await t.test(scenario.name, { timeout: 10000 }, async subtest => {
      const context = await browser.newContext();
      subtest.after(() => context.close());
      const endpoint = (scenario.thirdParty ? 'https://asset.example.test' : origin) + '/api/accounts/authorize/continue';
      let submitted = 0;
      const submit = scenario.transport === 'xhr'
        ? `const request = new XMLHttpRequest(); request.open('POST', ${JSON.stringify(endpoint)}); request.send();`
        : scenario.transport === 'image'
          ? `const image = new Image(); image.src = ${JSON.stringify(endpoint)};`
          : `fetch(${JSON.stringify(endpoint)}, {method:'POST'}).catch(() => {});`;
      await context.route('**/*', route => {
        if (route.request().url() === endpoint) {
          submitted++;
          return route.fulfill({ status: scenario.status || 403, contentType: scenario.contentType || 'text/html', headers: { 'Access-Control-Allow-Origin': '*', ...scenario.headers }, body: scenario.body });
        }
        return route.fulfill({ contentType: 'text/html', body: `<h1>Welcome back</h1><form><input type="email" name="email"><button type="submit">Continue</button></form><script>document.querySelector('form').onsubmit = event => {event.preventDefault(); ${submit} document.body.innerHTML = '<h1>Oops, an error occurred!</h1><button>Try again</button>';};</script>` });
      });
      const steps = [];
      await assert.rejects(
        run({ account: { email: 'example@example.test' }, auth_url: origin + '/log-in?state=expected', redirect_uri: 'http://localhost:1455/auth/callback', timeout_ms: 1500 }, (step, percent) => steps.push({ step, percent }), { chromium: { launch: async () => ({ newContext: async () => context, close: async () => context.close() }) } }),
        error => error.code === scenario.error,
        'must classify the response without inventing password or authorization progress'
      );
      assert.equal(submitted, 1);
      assert.ok(steps.some(entry => entry.step === '填写邮箱'));
      assert.equal(steps.some(entry => entry.percent > 28), false);
      if (scenario.error === 'cloudflare_blocked') assert.ok(steps.some(entry => entry.step === '登录接口安全验证未通过'));
    });
  }
});

test('a delayed security interstitial never advances to workspace selection', { skip: !fixturePlaywright }, async t => {
  process.env.CPA_CODEX_AUTO_OAUTH_LIB = '1';
  const { run } = require('./auto_oauth_runner.cjs');
  const { chromium } = require(fixturePlaywright);
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const context = await browser.newContext();
  await context.route('**/*', route => route.fulfill({ contentType: 'text/html', body: '<body><script>setTimeout(()=>{document.body.innerHTML="<h1>Performing security verification</h1>"},100)</script></body>' }));
  const steps = [];
  await assert.rejects(run({ account: {}, auth_url: 'https://oauth.example.test/start', redirect_uri: 'http://localhost:1455/auth/callback', timeout_ms: 1000 }, step => steps.push(step), { chromium: { launch: async () => ({ newContext: async () => context, close: async () => context.close() }) } }), error => error.code === 'cloudflare_blocked');
  assert.ok(steps.includes('等待登录页安全验证'));
  assert.equal(steps.some(step => step.includes('工作空间')), false);
});

// The bundled Linux headless Chromium advertises "HeadlessChrome" in its default
// User-Agent, which OpenAI's bot protection rejects before showing the form.
// This pins the desktop Chrome identity the runner must present instead.
test('login context presents a desktop Chrome identity, not HeadlessChrome', { skip: !fixturePlaywright }, async t => {
  process.env.CPA_CODEX_AUTO_OAUTH_LIB = '1';
  const { run } = require('./auto_oauth_runner.cjs');
  const { chromium } = require(fixturePlaywright);
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const context = await browser.newContext();
  let observed = null;
  await context.route('**/*', route => route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>Sign in</title>' }));
  await run(
    { account: {}, auth_url: 'https://oauth.example.test/start', redirect_uri: 'http://localhost:1455/auth/callback', timeout_ms: 1500 },
    () => {},
    { chromium: { launch: async () => ({ newContext: async options => { observed = options; return context; }, close: async () => context.close() }) } }
  ).catch(() => {});
  assert.ok(observed, 'runner must create a context');
  assert.match(observed.userAgent, /Windows NT 10\.0/);
  assert.doesNotMatch(observed.userAgent, /HeadlessChrome/i);
  assert.equal(observed.timezoneId, 'America/New_York');
  assert.deepEqual(observed.viewport, { width: 1280, height: 800 });
});

test('management progress retains a failed phase on desktop and mobile', { skip: !fixturePlaywright }, async t => {
  const { chromium } = require(fixturePlaywright);
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const go = fs.readFileSync(path.join(__dirname, '../../managementasset/codex_auto_oauth.go'), 'utf8');
  const script = go.match(/const codexAutoOAuthScript = `([\s\S]*?)`/)[1];
  for (const width of [1280, 390]) {
    const context = await browser.newContext({ viewport: { width, height: 844 } });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    let polls = 0;
    await page.route('**/*', route => {
      const url = new URL(route.request().url());
      if (url.origin !== 'https://panel.example.test') return route.abort();
      if (url.pathname === '/management.html') return route.fulfill({ contentType: 'text/html; charset=utf-8', body: '<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head><body><button>开始 Codex 登录</button>'+script+'</body></html>' });
      if (url.pathname === '/v0/management/config') return route.fulfill({ json: {} });
      const status = route.request().method() === 'POST' ? 'queued' : (++polls > 1 ? 'failed' : 'running');
      return route.fulfill({ json: {
        job_id: 'fixture-job', status: status === 'failed' ? 'completed' : 'running', success: 0, total: 1,
        accounts: [{ email: 'example@example.test', status, step: '等待登录页安全验证', progress: status === 'queued' ? 0 : 20, error: status === 'failed' ? '安全验证未完成，需要人工处理' : '' }],
      } });
    });
    await page.goto('https://panel.example.test/management.html');
    await page.evaluate(() => fetch('/v0/management/config', { headers: { Authorization: 'Bearer fixture-management-key' } }));
    await page.getByRole('button', { name: '自动 OAuth', exact: true }).click();
    await page.locator('textarea').fill('example@example.test----fixture-password----JBSWY3DPEHPK3PXP');
    await page.getByRole('button', { name: '开始自动 OAuth', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.cpa-codex-auto-result-state')?.textContent === '认证失败');
    assert.equal(await page.getByRole('progressbar').getAttribute('aria-valuenow'), '20');
    assert.match(await page.locator('.cpa-codex-auto-result-step').innerText(), /等待登录页安全验证.*安全验证未完成/);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    assert.deepEqual(errors, []);
    if (process.env.CPA_CODEX_AUTO_OAUTH_SCREENSHOT_DIR) {
      await page.screenshot({ path: path.join(process.env.CPA_CODEX_AUTO_OAUTH_SCREENSHOT_DIR, 'oauth-progress-'+width+'.png'), fullPage: true });
    }
    await context.close();
  }
});
