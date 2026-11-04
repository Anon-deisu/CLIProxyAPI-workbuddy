(function () {
  "use strict";
  if (window.__cpaAgentWorkspaceLoaded) return;
  window.__cpaAgentWorkspaceLoaded = true;
  const assetBase = new URL("./", document.currentScript.src);
  const nativeFetch = window.fetch.bind(window);
  let auth = "",
    apiBase = "",
    generation = 0;
  let host,
    shadow,
    ui,
    launch,
    ready,
    iconsReady,
    opened = false,
    viewGeneration = 0,
    returnHash = "#/",
    dismissDialog;
  const workspaceHash = "#/?cpa-agent=chat";
  let restoreWorkspace = location.hash === workspaceHash;
  const nativeActiveLinks = new Set();
  let active = "",
    models = [],
    conversations = new Map(),
    snapshots = new Map();
  const drafts = new Map(),
    streams = new Map(),
    retryTimers = new Map(),
    messages = new Map();
  let listTimer,
    toastTimer,
    fileTimer,
    refreshPending = false,
    fileRequest = 0;
  let sending = new Set(),
    selectedModel = "",
    selectedEffort = "";
  const statusText = {
    idle: "已完成",
    running: "生成中",
    canceled: "已停止",
    failed: "失败",
    interrupted: "已中断",
  };
  const toolNames = {
    write_file: "写入文件",
    read_file: "读取文件",
    list_files: "查看文件",
    grep_files: "搜索文件",
    delete_file: "删除文件",
    run_command: "执行命令",
  };
  const $ = (selector) => shadow.querySelector(selector);
  const node = (tag, className, text) => {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = text;
    return element;
  };

  function credential(input, init) {
    const isRequest = input instanceof Request;
    const url = new URL(isRequest ? input.url : String(input), location.href);
    const match = url.pathname.match(/\/v(?:0|8)\/management\//);
    if (!match) return null;
    const headers = new Headers(
      (init && init.headers) || (isRequest && input.headers) || {},
    );
    const key =
      headers.get("authorization") ||
      (headers.get("x-management-key")
        ? "Bearer " + headers.get("x-management-key")
        : "");
    return key
      ? {
          key,
          base:
            url.origin + url.pathname.slice(0, match.index) + "/v0/management/",
        }
      : null;
  }
  function observe(info, status) {
    if (!info) return;
    if (
      (status === 401 || status === 403) &&
      auth === info.key &&
      apiBase === info.base
    ) {
      reset();
      return;
    }
    if (status < 200 || status >= 300) return;
    if (auth !== info.key || apiBase !== info.base) {
      reset();
      auth = info.key;
      apiBase = info.base;
    }
    mount();
    syncPanel();
    if (location.hash === workspaceHash) openWorkspace().catch(report);
  }
  window.fetch = function (input, init) {
    const info = credential(input, init);
    return nativeFetch(input, init).then((response) => {
      observe(info, response.status);
      return response;
    });
  };
  const xhrOpen = XMLHttpRequest.prototype.open;
  const xhrHeader = XMLHttpRequest.prototype.setRequestHeader;
  const xhrSend = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cpaAgentURL = url;
    this.__cpaAgentHeaders = new Headers();
    return xhrOpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    this.__cpaAgentHeaders.append(name, value);
    return xhrHeader.apply(this, arguments);
  };
  XMLHttpRequest.prototype.send = function () {
    const info = credential(this.__cpaAgentURL, {
      headers: this.__cpaAgentHeaders,
    });
    if (info)
      this.addEventListener("load", () => observe(info, this.status), {
        once: true,
      });
    return xhrSend.apply(this, arguments);
  };
  function onNavigation() {
    if (/login|logout/i.test(location.hash)) reset();
    else if (location.hash === workspaceHash && auth)
      openWorkspace().catch(report);
    else closeWorkspace();
  }
  window.addEventListener("hashchange", onNavigation);
  window.addEventListener("popstate", onNavigation);
  for (const method of ["pushState", "replaceState"]) {
    const original = history[method];
    history[method] = function () {
      const result = original.apply(this, arguments);
      queueMicrotask(onNavigation);
      return result;
    };
  }
  window.addEventListener("storage", (event) => {
    if (event.key === "cli-proxy-auth" && !event.newValue) reset();
  });

  function reset() {
    generation++;
    auth = "";
    apiBase = "";
    closeWorkspace();
    for (const stream of streams.values()) stream.abort();
    for (const timer of retryTimers.values()) clearTimeout(timer);
    streams.clear();
    retryTimers.clear();
    conversations.clear();
    snapshots.clear();
    drafts.clear();
    messages.clear();
    sending.clear();
    active = "";
    selectedModel = "";
    selectedEffort = "";
    models = [];
    if (launch) launch.hidden = true;
    if (ui) {
      $(".messages").replaceChildren();
      $(".sessions").replaceChildren();
      $(".file-list").replaceChildren();
      $(".preview").textContent = "";
      $(".prompt").value = "";
      renderModelChoices();
    }
  }

  async function api(path, options = {}) {
    if (!auth) throw new Error("请先登录管理面板");
    const requestGeneration = generation;
    const headers = new Headers(options.headers);
    headers.set("Authorization", auth);
    if (options.json !== undefined) {
      headers.set("Content-Type", "application/json");
      options.body = JSON.stringify(options.json);
    }
    const response = await nativeFetch(apiBase + "agent/" + path, {
      ...options,
      headers,
    });
    if (generation !== requestGeneration)
      throw new DOMException("Session changed", "AbortError");
    if (response.status === 401 || response.status === 403) {
      reset();
      throw new Error("管理登录已失效");
    }
    if (!response.ok) {
      const body = await response.text();
      let data;
      try {
        data = JSON.parse(body);
      } catch (_) {
        data = { error: "HTTP " + response.status };
      }
      throw new Error(
        typeof data.error === "string"
          ? data.error
          : data.error?.message || "HTTP " + response.status,
      );
    }
    return response;
  }
  const json = async (path, options) => {
    const requestGeneration = generation;
    const result = await (await api(path, options)).json();
    if (generation !== requestGeneration)
      throw new DOMException("Session changed", "AbortError");
    return result;
  };
  function report(error) {
    if (error.name === "AbortError") return;
    if (!ui) {
      if (launch) {
        launch.title = error.message;
      }
      return;
    }
    const toast = $(".error-banner");
    toast.textContent = error.message;
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (toast.hidden = true), 6000);
  }
  const action = (fn) => (event) => {
    Promise.resolve()
      .then(() => fn(event))
      .catch(report);
  };

  function loadScript(name) {
    return new Promise((resolve, reject) => {
      const script = document.createElement("script");
      script.src = new URL(name, assetBase);
      script.onload = resolve;
      script.onerror = () => {
        script.remove();
        reject(new Error("无法加载对话资源：" + name));
      };
      document.head.append(script);
    });
  }
  function icon(name) {
    const svg = window.lucide.createElement(window.lucide.icons[name]);
    svg.setAttribute("aria-hidden", "true");
    return svg;
  }
  function button(name, label, fn, className = "") {
    const b = node("button", "icon " + className);
    b.type = "button";
    b.title = label;
    b.setAttribute("aria-label", label);
    b.append(icon(name));
    if (fn) b.addEventListener("click", action(fn));
    return b;
  }
  function mount() {
    if (host) return;
    host = node("div");
    host.id = "cpa-agent-workspace";
    host.hidden = true;
    shadow = host.attachShadow({ mode: "open" });
    const style = node("link");
    style.rel = "stylesheet";
    style.href = new URL("workspace.css", assetBase);
    shadow.append(style);
    const panelStyle = node("style");
    panelStyle.textContent = `
      #cpa-agent-workspace { flex: 1; min-width: 0; min-height: 0; height: 100%; padding: var(--header-height) var(--shell-gutter) 20px; }
      #cpa-agent-workspace[hidden], #cpa-agent-nav[hidden] { display: none !important; }
      .content.cpa-agent-page { position: relative; overflow: hidden; scrollbar-gutter: auto; }
      .content.cpa-agent-page > .main-content { position: absolute; inset: 0; opacity: 0; pointer-events: none; }
      body:has(.content.cpa-agent-page) > .cpa-runtime-scheduler-toggle { display: none !important; }
      @media (max-width: 760px) { #cpa-agent-workspace { padding: var(--header-height) 12px 12px; } }
    `;
    document.head.append(panelStyle);
    launch = node("a", "nav-item");
    launch.id = "cpa-agent-nav";
    launch.href = workspaceHash;
    launch.hidden = true;
    launch.title = "对话";
    launch.append(node("span", "nav-icon"));
    const text = node("span", "nav-text");
    text.append(node("span", "nav-label", "对话"));
    launch.append(text);
    launch.addEventListener("click", (event) => {
      event.preventDefault();
      if (location.hash !== workspaceHash) {
        returnHash = location.hash || "#/";
        location.hash = workspaceHash;
      }
      document.querySelector(".sidebar-backdrop.visible")?.click();
      openWorkspace().catch(report);
    });
    iconsReady = loadScript("lucide.js").then(() =>
      launch.querySelector(".nav-icon").append(icon("MessagesSquare")),
    );
    iconsReady.catch(report);
    let syncFrame;
    new MutationObserver(() => {
      if (syncFrame) return;
      syncFrame = requestAnimationFrame(() => {
        syncFrame = null;
        syncPanel();
      });
    }).observe(document.body, { childList: true, subtree: true });
    syncPanel();
  }
  function syncPanel() {
    if (!host) return;
    const content = document.querySelector(".app-shell .main-body > .content");
    const group = document.querySelector(".sidebar .nav-section .nav-group");
    if (!content || !group) return;
    if (host.parentElement !== content) content.append(host);
    if (launch.parentElement !== group) group.append(launch);
    const visible = opened && ui && !ui.hidden;
    host.hidden = !visible;
    content.classList.toggle("cpa-agent-page", Boolean(visible));
    const main = content.querySelector(":scope > .main-content");
    main.inert = Boolean(visible);
    if (visible) main.setAttribute("aria-hidden", "true");
    else main.removeAttribute("aria-hidden");
    launch.hidden = !auth;
    launch.classList.toggle("active", Boolean(visible));
    if (visible) {
      launch.setAttribute("aria-current", "page");
      for (const link of document.querySelectorAll(
        ".sidebar .nav-item.active",
      )) {
        if (link === launch) continue;
        nativeActiveLinks.add(link);
        link.classList.remove("active");
        link.removeAttribute("aria-current");
      }
    } else {
      launch.removeAttribute("aria-current");
      for (const link of nativeActiveLinks) {
        if (link.getAttribute("href") === location.hash.split("?")[0]) {
          link.classList.add("active");
          link.setAttribute("aria-current", "page");
        }
      }
      nativeActiveLinks.clear();
    }
    if (restoreWorkspace && auth) {
      restoreWorkspace = false;
      location.hash = workspaceHash;
      openWorkspace().catch(report);
    }
  }
  async function openWorkspace() {
    if (opened || !auth) return;
    opened = true;
    const view = ++viewGeneration;
    const requestGeneration = generation;
    if (!ready) {
      ready = (async () => {
        await Promise.all([
          loadScript("marked.js"),
          loadScript("purify.js"),
          iconsReady,
        ]);
        build();
      })().catch((error) => {
        ready = null;
        throw error;
      });
    }
    try {
      await ready;
    } catch (error) {
      closeWorkspace();
      throw error;
    }
    if (requestGeneration !== generation || view !== viewGeneration || !auth)
      return;
    ui.hidden = false;
    syncPanel();
    listTimer = setInterval(() => {
      refreshModels().catch(report);
      refreshList().catch(report);
      scheduleFiles();
    }, 3500);
    $(".prompt").focus();
    await refreshModels();
    await refreshList();
    if (view !== viewGeneration) return;
    if (active && conversations.has(active)) await selectConversation(active);
    else if (conversations.size)
      await selectConversation(conversations.keys().next().value);
    else renderEmpty();
    refreshSandbox().catch(report);
  }
  function leaveWorkspace() {
    closeWorkspace();
    if (location.hash === workspaceHash) location.hash = returnHash;
    launch.focus();
  }
  function closeWorkspace() {
    opened = false;
    viewGeneration++;
    clearInterval(listTimer);
    clearTimeout(fileTimer);
    fileTimer = null;
    if (active && ui) drafts.set(active, $(".prompt").value);
    for (const stream of streams.values()) stream.abort();
    streams.clear();
    for (const timer of retryTimers.values()) clearTimeout(timer);
    retryTimers.clear();
    dismissDialog?.();
    if (ui) ui.hidden = true;
    syncPanel();
  }
  function build() {
    ui = node("section", "workspace");
    ui.hidden = true;
    ui.setAttribute("role", "region");
    ui.setAttribute("aria-label", "CPA 对话工作区");
    ui.innerHTML = `<header class="topbar"><h1>对话</h1><div class="spacer"></div><span class="sandbox" role="status">沙箱检查中</span></header>
      <div class="shell"><aside class="sidebar"><div class="sidebar-head"><strong>会话</strong></div><input class="search" type="search" placeholder="搜索对话" aria-label="搜索对话"><nav class="sessions" aria-label="会话列表"></nav><div class="sidebar-footer">0 个对话</div></aside>
      <main class="chat"><div class="chat-head"><button class="chat-title" type="button" title="重命名对话">新对话</button></div>
      <div class="messages" role="log" aria-label="对话消息" aria-live="off"></div><div class="composer-area"><div class="run-error" role="status" hidden></div><div class="composer"><textarea class="prompt" rows="3" aria-label="消息" placeholder="输入消息…"></textarea><div class="composer-actions"><div class="controls"><label class="field"><select class="model" aria-label="模型" title="模型"></select></label><label class="field reasoning"><select class="effort" aria-label="思考等级" title="思考等级"></select></label></div><span class="run-state" role="status" hidden></span></div></div></div></main>
      <aside class="files"><div class="files-head"><strong>工作区文件</strong><span class="file-count"></span><div class="spacer"></div></div><div class="file-list"></div><div class="preview-title">文件预览</div><pre class="preview" aria-label="文件预览">选择一个文件</pre></aside></div><div class="error-banner" role="alert" hidden></div>`;
    shadow.append(ui);
    $(".topbar").prepend(
      button(
        "PanelLeft",
        "切换会话列表",
        () => $(".shell").classList.toggle("sessions-visible"),
        "mobile-sessions",
      ),
    );
    $(".topbar").append(
      button("SlidersHorizontal", "账号调度", () =>
        document.querySelector(".cpa-runtime-scheduler-toggle").click(),
      ),
      button("FolderOpen", "切换文件面板", () => {
        const shell = $(".shell");
        if (ui.clientWidth > 1100) shell.classList.toggle("files-hidden");
        else shell.classList.toggle("files-visible");
      }),
      button("X", "返回管理页", leaveWorkspace),
    );
    $(".sidebar-head").append(button("Plus", "新建对话", createConversation));
    $(".chat-head").append(
      button("Plus", "新建对话", createConversation, "mobile-sessions"),
    );
    $(".chat-title").addEventListener("click", action(renameConversation));
    $(".files-head").append(
      button("RefreshCw", "刷新文件", () => refreshFiles(active)),
      button(
        "X",
        "收起文件面板",
        () => {
          const shell = $(".shell");
          shell.classList.remove("files-visible");
          shell.classList.add("files-hidden");
        },
        "mobile-sessions",
      ),
    );
    const upload = node("input");
    upload.type = "file";
    upload.multiple = true;
    upload.hidden = true;
    upload.className = "upload";
    $(".composer-actions").prepend(
      upload,
      button("Paperclip", "上传文件", () => upload.click(), "upload-button"),
    );
    upload.addEventListener(
      "change",
      action(async () => {
        await uploadFiles(Array.from(upload.files));
        upload.value = "";
      }),
    );
    const stop = button("Square", "停止生成", cancelRun, "stop");
    stop.hidden = true;
    const send = button("ArrowUp", "发送消息", sendMessage, "primary send");
    $(".composer-actions").append(node("div", "composer-spacer"), stop, send);
    $(".search").addEventListener("input", renderSessions);
    $(".model").addEventListener(
      "change",
      action(async () => {
        selectedModel = $(".model").value;
        selectedEffort = "";
        renderEfforts();
        updateControls();
        await saveSettings();
      }),
    );
    $(".effort").addEventListener(
      "change",
      action(async () => {
        selectedEffort = $(".effort").value;
        await saveSettings();
      }),
    );
    $(".prompt").addEventListener("input", () => {
      if (active) drafts.set(active, $(".prompt").value);
      updateControls();
    });
    $(".prompt").addEventListener("keydown", (event) => {
      if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
        event.preventDefault();
        action(sendMessage)(event);
      }
    });
    ui.addEventListener("keydown", (event) => {
      if (event.key === "Escape") {
        event.preventDefault();
        if (!$(".dialog-backdrop")) leaveWorkspace();
      }
      if (event.key === "Tab" && $(".dialog-backdrop")) {
        const scope = $(".dialog-backdrop");
        const focusable = Array.from(
          scope.querySelectorAll(
            "button,input,select,textarea,a[href],summary",
          ),
        ).filter((n) => !n.disabled && n.getClientRects().length);
        const first = focusable[0],
          last = focusable.at(-1);
        if (!first) return;
        if (event.shiftKey && shadow.activeElement === first) {
          event.preventDefault();
          last.focus();
        } else if (!event.shiftKey && shadow.activeElement === last) {
          event.preventDefault();
          first.focus();
        }
      }
    });
    renderModelChoices();
    renderEmpty();
  }
  async function refreshModels() {
    const result = await json("models");
    if (JSON.stringify(models) === JSON.stringify(result.models)) return;
    models = result.models;
    if (!models.some((m) => m.id === selectedModel))
      selectedModel = active ? "" : models[0]?.id || "";
    renderModelChoices();
  }
  function renderModelChoices() {
    $(".model").replaceChildren();
    if (!models.length) {
      $(".model").append(new Option("暂无可用模型", ""));
      selectedModel = "";
    } else {
      if (!selectedModel) {
        const placeholder = new Option("选择模型", "");
        placeholder.disabled = true;
        $(".model").append(placeholder);
      }
      for (const model of models)
        $(".model").append(
          new Option(
            model.name === model.id ? model.id : model.name + " · " + model.id,
            model.id,
          ),
        );
    }
    $(".model").value = selectedModel;
    renderEfforts();
    updateControls();
  }
  function renderEfforts() {
    const model = models.find((m) => m.id === selectedModel);
    const efforts = model ? model.reasoning_efforts : [];
    $(".effort").replaceChildren(new Option("模型默认", ""));
    for (const effort of efforts)
      $(".effort").append(new Option(effort, effort));
    if (!efforts.includes(selectedEffort)) selectedEffort = "";
    $(".effort").value = selectedEffort;
  }
  async function refreshSandbox() {
    const result = await json("status");
    const badge = $(".sandbox");
    badge.dataset.ready = String(result.sandbox.available);
    badge.textContent = result.sandbox.available ? "沙箱已就绪" : "终端不可用";
    badge.title = result.sandbox.image + " · " + (result.sandbox.message || "");
  }
  async function refreshList() {
    if (refreshPending || !opened) return;
    refreshPending = true;
    try {
      const result = await json("conversations");
      conversations = new Map(result.conversations.map((c) => [c.id, c]));
      for (const [id, controller] of streams)
        if (id !== active && conversations.get(id)?.status !== "running") {
          controller.abort();
          streams.delete(id);
        }
      for (const c of conversations.values())
        if (c.status === "running") watch(c.id);
      renderSessions();
      if (active && !conversations.has(active)) {
        active = "";
        messages.clear();
        renderEmpty();
      }
    } finally {
      refreshPending = false;
    }
  }
  function renderSessions() {
    if (!ui) return;
    const filter = $(".search").value.toLowerCase();
    const list = $(".sessions");
    list.replaceChildren();
    let running = 0;
    for (const c of conversations.values()) {
      if (c.status === "running") running++;
      if (filter && !c.title.toLowerCase().includes(filter)) continue;
      const row = node("div", "session" + (c.id === active ? " active" : ""));
      row.dataset.id = c.id;
      const pick = node("button", "session-pick");
      pick.type = "button";
      pick.setAttribute("aria-current", c.id === active ? "true" : "false");
      pick.append(node("span", "session-title", c.title));
      const meta = node("span", "session-meta");
      meta.append(
        node("i", "status-dot " + c.status),
        node("span", "", statusText[c.status] || c.status),
      );
      pick.append(meta);
      pick.title = c.title;
      pick.addEventListener(
        "click",
        action(() => selectConversation(c.id)),
      );
      row.append(
        pick,
        button(
          "Trash2",
          "删除 " + c.title,
          () => deleteConversation(c.id),
          "delete-session danger",
        ),
      );
      list.append(row);
    }
    if (!list.childElementCount)
      list.append(
        node("div", "file-empty", filter ? "没有匹配的对话" : "还没有对话"),
      );
    $(".sidebar-footer").textContent =
      conversations.size +
      " 个对话" +
      (running ? " · " + running + " 个正在生成" : "");
  }
  async function selectConversation(id) {
    if (active) drafts.set(active, $(".prompt").value);
    const previous = active;
    active = id;
    messages.clear();
    $(".messages").replaceChildren();
    $(".shell").classList.remove("sessions-visible", "files-visible");
    $(".preview").textContent = "选择一个文件";
    $(".preview-title").textContent = "文件预览";
    fileRequest++;
    if (previous !== id && conversations.get(previous)?.status !== "running") {
      streams.get(previous)?.abort();
      streams.delete(previous);
    }
    $(".prompt").value = drafts.get(id) || "";
    const data = await json("conversations/" + encodeURIComponent(id));
    if (active !== id) return;
    applySnapshot(data, true);
    watch(id);
    await refreshFiles(id);
    $(".messages").scrollTop = $(".messages").scrollHeight;
  }
  function applySnapshot(c, selection = false) {
    const old = snapshots.get(c.id);
    if (old && !selection && c.revision < old.revision) return;
    snapshots.set(c.id, c);
    conversations.set(c.id, { ...conversations.get(c.id), ...c });
    renderSessions();
    if (active !== c.id) return;
    if (selection) {
      selectedModel = c.model
        ? models.some((m) => m.id === c.model)
          ? c.model
          : ""
        : models[0]?.id || "";
      selectedEffort = c.reasoning_effort || "";
      renderModelChoices();
    }
    $(".chat-title").textContent = c.title;
    $(".run-error").textContent = c.error || "";
    $(".run-error").hidden = !c.error;
    renderMessages(c);
    updateControls();
    if (old?.status === "running" && c.status !== "running") scheduleFiles();
  }
  function watch(id) {
    if (!opened || streams.has(id) || retryTimers.has(id)) return;
    const controller = new AbortController();
    streams.set(id, controller);
    (async () => {
      const response = await api(
        "conversations/" + encodeURIComponent(id) + "/events",
        { signal: controller.signal },
      );
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let pending = "";
      try {
        while (true) {
          const { value, done } = await reader.read();
          if (done) break;
          pending += decoder.decode(value, { stream: true });
          let split;
          while ((split = pending.indexOf("\n\n")) >= 0) {
            const frame = pending.slice(0, split);
            pending = pending.slice(split + 2);
            const line = frame.split("\n").find((l) => l.startsWith("data: "));
            if (line) {
              const snapshot = JSON.parse(line.slice(6));
              applySnapshot(snapshot);
            }
          }
        }
      } finally {
        reader.releaseLock();
      }
    })()
      .catch((error) => {
        if (error.name !== "AbortError") report(error);
      })
      .finally(() => {
        if (streams.get(id) !== controller) return;
        streams.delete(id);
        if (
          !controller.signal.aborted &&
          opened &&
          auth &&
          conversations.has(id) &&
          (active === id || conversations.get(id).status === "running")
        ) {
          retryTimers.set(
            id,
            setTimeout(() => {
              retryTimers.delete(id);
              watch(id);
            }, 1500),
          );
        }
      });
  }
  async function createConversation() {
    const c = await json("conversations", {
      method: "POST",
      json: { model: selectedModel, reasoning_effort: selectedEffort },
    });
    conversations.set(c.id, c);
    await selectConversation(c.id);
    $(".prompt").focus();
    return c.id;
  }
  async function saveSettings() {
    if (!active) return;
    const id = active;
    const c = await json("conversations/" + id, {
      method: "PATCH",
      json: { model: selectedModel, reasoning_effort: selectedEffort },
    });
    applySnapshot(c);
  }
  async function sendMessage() {
    const prompt = $(".prompt").value.trim();
    if (!prompt || !selectedModel) return;
    if (
      active &&
      (sending.has(active) || snapshots.get(active)?.status === "running")
    )
      return;
    const savedPrompt = prompt;
    let id = active;
    const model = selectedModel,
      effort = selectedEffort;
    if (!id) id = await createConversation();
    sending.add(id);
    updateControls();
    try {
      await json("conversations/" + id + "/messages", {
        method: "POST",
        json: { message: savedPrompt, model, reasoning_effort: effort },
      });
      drafts.set(id, "");
      if (active === id) $(".prompt").value = "";
      watch(id);
      applySnapshot(await json("conversations/" + id));
      scheduleFiles();
    } finally {
      sending.delete(id);
      updateControls();
    }
  }
  async function cancelRun() {
    if (active)
      await json("conversations/" + active + "/cancel", { method: "POST" });
  }
  function updateControls() {
    if (!ui) return;
    const running = snapshots.get(active)?.status === "running";
    const busy = sending.has(active);
    $(".send").disabled =
      !selectedModel || !$(".prompt").value.trim() || running || busy;
    $(".send").hidden = running;
    $(".stop").hidden = !running;
    $(".model").disabled = running || busy || !models.length;
    $(".effort").disabled =
      running ||
      busy ||
      !models.find((m) => m.id === selectedModel)?.reasoning_efforts.length;
    $(".upload-button").disabled = running || busy;
    $(".chat-title").disabled = !active || running;
    $(".run-state").textContent = running
      ? "正在生成"
      : selectedModel
        ? statusText[snapshots.get(active)?.status] || ""
        : "暂无可用模型";
    $(".run-state").hidden = !$(".run-state").textContent;
  }
  function renderEmpty() {
    messages.clear();
    $(".messages").replaceChildren();
    $(".chat-title").textContent = "新对话";
    $(".run-error").hidden = true;
    const empty = node("div", "empty");
    empty.append(icon("MessageSquareText"), node("h2", "", "新对话"));
    $(".messages").append(empty);
    renderEfforts();
    updateControls();
    $(".file-list").replaceChildren(node("div", "file-empty", "暂无文件"));
    $(".file-count").textContent = "";
  }
  function renderMessages(c) {
    const log = $(".messages");
    const stick = log.scrollHeight - log.scrollTop - log.clientHeight < 100;
    if (!c.messages?.length) {
      renderEmpty();
      $(".chat-title").textContent = c.title;
      return;
    }
    log.querySelector(".empty")?.remove();
    const toolResults = new Map(
      c.messages
        .filter((m) => m.role === "tool")
        .map((m) => [m.tool_call_id, m]),
    );
    for (const message of c.messages) {
      if (message.role === "tool" || message.role === "system") continue;
      let view = messages.get(message.id);
      if (!view) {
        const article = node("article", "message " + message.role);
        article.dataset.id = message.id;
        const header = node("div", "message-header");
        const avatar = node("span", "avatar");
        avatar.append(icon(message.role === "user" ? "User" : "Terminal"));
        header.append(
          avatar,
          node("strong", "", message.role === "user" ? "你" : "Assistant"),
          node(
            "span",
            "",
            new Date(message.created_at).toLocaleTimeString([], {
              hour: "2-digit",
              minute: "2-digit",
            }),
          ),
        );
        header.append(
          button("Copy", "复制消息", () => copyText(view.text), "copy-message"),
        );
        const content = node("div", "content");
        article.append(header, content);
        log.append(article);
        view = { article, content, text: null, tools: new Map() };
        messages.set(message.id, view);
      }
      const text = message.content || "";
      if (view.text !== text) {
        view.text = text;
        if (message.role === "user") view.content.textContent = text;
        else renderMarkdown(view.content, text);
      }
      for (const call of message.tool_calls || []) {
        let tool = view.tools.get(call.id);
        const result = toolResults.get(call.id);
        if (!tool) {
          const details = node("details", "tool");
          const summary = node("summary");
          summary.append(
            icon(call.name === "run_command" ? "Terminal" : "FileText"),
            node("span", "", toolNames[call.name] || call.name),
          );
          const state = node("span", "tool-state");
          summary.append(state);
          const pre = node("pre");
          details.append(summary, pre);
          view.article.append(details);
          tool = { details, state, pre };
          view.tools.set(call.id, tool);
        }
        tool.state.textContent = result
          ? "已完成"
          : c.status === "running"
            ? "执行中"
            : "已中断";
        tool.pre.textContent =
          call.arguments + (result ? "\n\n" + result.content : "");
      }
    }
    if (stick) log.scrollTop = log.scrollHeight;
  }
  function renderMarkdown(target, text) {
    target.innerHTML = window.DOMPurify.sanitize(
      window.marked.parse(text, { gfm: true, breaks: true }),
      {
        USE_PROFILES: { html: true },
        FORBID_TAGS: [
          "img",
          "video",
          "audio",
          "iframe",
          "form",
          "input",
          "style",
          "button",
        ],
        FORBID_ATTR: ["style", "id", "name"],
      },
    );
    for (const a of target.querySelectorAll("a")) {
      a.target = "_blank";
      a.rel = "noopener noreferrer";
    }
    for (const pre of target.querySelectorAll("pre")) {
      const code = pre.querySelector("code");
      pre.append(
        button(
          "Copy",
          "复制代码",
          () => copyText(code?.textContent || ""),
          "copy-code",
        ),
      );
    }
    for (const table of target.querySelectorAll("table")) {
      const wrapper = node("div", "table-wrap");
      table.before(wrapper);
      wrapper.append(table);
    }
  }
  async function copyText(text) {
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(text);
      return;
    }
    const field = node("textarea");
    field.value = text;
    ui.append(field);
    field.select();
    document.execCommand("copy");
    field.remove();
  }
  function scheduleFiles() {
    if (!active || !opened || fileTimer) return;
    fileTimer = setTimeout(() => {
      fileTimer = null;
      refreshFiles(active).catch(report);
    }, 800);
  }
  async function refreshFiles(id) {
    if (!id) return;
    const result = await json("conversations/" + id + "/files");
    if (active !== id) return;
    const list = $(".file-list");
    list.replaceChildren();
    $(".file-count").textContent = result.files.length;
    if (!result.files.length)
      list.append(node("div", "file-empty", "暂无文件"));
    for (const file of result.files) {
      const row = node("div", "file-row");
      const pick = node("button", "file-pick");
      pick.type = "button";
      pick.title = file.path + " · " + file.size + " bytes";
      pick.append(icon("FileCode"), node("span", "", file.path));
      pick.addEventListener(
        "click",
        action(() => previewFile(id, file)),
      );
      row.append(
        pick,
        button("Download", "下载 " + file.path, () =>
          downloadFile(id, file.path),
        ),
        button(
          "Trash2",
          "删除文件 " + file.path,
          () => deleteFile(id, file.path),
          "danger",
        ),
      );
      list.append(row);
    }
  }
  async function previewFile(id, file) {
    const ticket = ++fileRequest;
    $(".preview-title").textContent = file.path;
    if (file.size > 1024 * 1024) {
      $(".preview").textContent = "文件较大，请下载查看。";
      return;
    }
    const response = await api(
      "conversations/" + id + "/files?path=" + encodeURIComponent(file.path),
    );
    const buffer = await response.arrayBuffer();
    if (active !== id || ticket !== fileRequest) return;
    const text = new TextDecoder().decode(buffer);
    $(".preview").textContent = text.includes("\0")
      ? "二进制文件，请下载查看。"
      : text;
  }
  async function downloadFile(id, path) {
    const response = await api(
      "conversations/" + id + "/files?path=" + encodeURIComponent(path),
    );
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const a = node("a");
    a.href = url;
    a.download = path.split("/").at(-1);
    shadow.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  async function uploadFiles(files) {
    if (!files.length) return;
    const id = active || (await createConversation());
    for (const file of files)
      await api(
        "conversations/" + id + "/files?path=" + encodeURIComponent(file.name),
        { method: "POST", body: file },
      );
    if (active === id) await refreshFiles(id);
  }
  async function deleteFile(id, path) {
    if (!(await confirmDialog("删除文件", "将永久删除 " + path + "。"))) return;
    await api(
      "conversations/" + id + "/files?path=" + encodeURIComponent(path),
      { method: "DELETE" },
    );
    await refreshFiles(id);
  }
  async function deleteConversation(id) {
    if (
      !(await confirmDialog(
        "删除对话",
        "这会停止该对话的任务，并永久删除聊天记录、沙箱与全部工作区文件。",
      ))
    )
      return;
    await api("conversations/" + id, { method: "DELETE" });
    streams.get(id)?.abort();
    streams.delete(id);
    clearTimeout(retryTimers.get(id));
    retryTimers.delete(id);
    conversations.delete(id);
    snapshots.delete(id);
    drafts.delete(id);
    if (active === id) {
      active = "";
      $(".prompt").value = "";
      const next = conversations.keys().next().value;
      if (next) await selectConversation(next);
      else renderEmpty();
    }
    renderSessions();
  }
  async function renameConversation() {
    if (!active) return;
    const id = active;
    const title = await confirmDialog(
      "重命名对话",
      "",
      conversations.get(id)?.title || "",
    );
    if (title === null) return;
    applySnapshot(
      await json("conversations/" + id, { method: "PATCH", json: { title } }),
    );
  }
  function confirmDialog(title, description, initial) {
    return new Promise((resolve) => {
      const previous = shadow.activeElement;
      const backdrop = node("div", "dialog-backdrop");
      const dialog = node("div", "confirm");
      dialog.setAttribute("role", "alertdialog");
      dialog.setAttribute("aria-label", title);
      dialog.append(node("h3", "", title));
      if (description) dialog.append(node("p", "", description));
      let input;
      if (initial !== undefined) {
        input = node("input");
        input.value = initial;
        input.maxLength = 80;
        input.setAttribute("aria-label", "对话标题");
        dialog.append(input);
      }
      const actions = node("div", "confirm-actions");
      const cancel = node("button", "", "取消");
      const ok = node(
        "button",
        initial === undefined ? "destructive" : "primary",
        initial === undefined ? "删除" : "保存",
      );
      const finish = (value) => {
        dismissDialog = null;
        backdrop.remove();
        previous?.focus();
        resolve(value);
      };
      dismissDialog = () => finish(initial === undefined ? false : null);
      cancel.onclick = () => finish(initial === undefined ? false : null);
      ok.onclick = () => finish(input ? input.value : true);
      actions.append(cancel, ok);
      dialog.append(actions);
      backdrop.append(dialog);
      ui.append(backdrop);
      backdrop.addEventListener("keydown", (e) => {
        if (e.key === "Escape") {
          e.stopPropagation();
          finish(initial === undefined ? false : null);
        }
        if (e.key === "Enter" && input) {
          e.preventDefault();
          finish(input.value);
        }
      });
      (input || cancel).focus();
    });
  }
})();
