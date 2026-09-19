package managementasset

import (
	"bytes"
	"fmt"
)

// cursorAPIKeyInlineMarker is the idempotency guard: once the inline Cursor
// API-key card input has been injected, later calls short-circuit and leave the
// bundle untouched.
const cursorAPIKeyInlineMarker = `id="cpa-key-inline-script"`

// cursorAPIKeyInlineScript is a self-contained IIFE injected alongside the
// Cursor OAuth card. The standalone #/cursor-api-key page is gone; the only
// add surface is a minimal inline API-Key input directly inside the Cursor
// OAuth card.
//
// The OAuth page must show only: the API-Key input, a submit button, and the
// existing OAuth login button. No headline, no label text, no list, no status
// line, no email / identity field. Submitting POSTs the key to
// /v0/management/cursor-api-key via the JSON store (auth kind api-key); it
// reuses the panel session's captured Bearer token so the user never
// re-enters credentials. There is deliberately no read-back: a GET would only
// serve the management list, which must stay off this page. Success clears the
// field; failure turns the input border red (no text).
const cursorAPIKeyInlineScript = `
<script id="cpa-key-inline-script">
(function () {
  'use strict';
  if (window.__cpaCursorAPIKeyInlineLoaded) return;
  window.__cpaCursorAPIKeyInlineLoaded = true;

  var capturedManagementKey = '';
  var managementPath = '/v0/management/';

  function captureAuthorization(value) {
    if (typeof value !== 'string') return;
    var match = value.match(/^\s*Bearer\s+(.+?)\s*$/i);
    if (match && match[1]) capturedManagementKey = match[1];
  }

  var nativeXHROpen = XMLHttpRequest.prototype.open;
  var nativeXHRSetRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cpaCursorManagementRequest = String(url || '').indexOf(managementPath) !== -1;
    return nativeXHROpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    if (this.__cpaCursorManagementRequest && String(name).toLowerCase() === 'authorization') {
      captureAuthorization(String(value || ''));
    }
    return nativeXHRSetRequestHeader.apply(this, arguments);
  };

  var nativeFetch = window.fetch;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : (input && input.url) || '';
    if (String(url).indexOf(managementPath) !== -1) {
      var headers = new Headers((init && init.headers) || (input && input.headers) || {});
      captureAuthorization(headers.get('authorization') || '');
    }
    return nativeFetch.apply(window, arguments);
  };

  function rememberedManagementKey() {
    try {
      var raw = localStorage.getItem('cli-proxy-auth');
      if (!raw) return '';
      if (raw.indexOf('enc::v1::') === 0) {
        var key = new TextEncoder().encode(
          'cli-proxy-api-webui::secure-storage|' + window.location.host + '|' + navigator.userAgent
        );
        var encoded = atob(raw.slice(9));
        var decoded = new Uint8Array(encoded.length);
        for (var i = 0; i < encoded.length; i += 1) {
          decoded[i] = encoded.charCodeAt(i) ^ key[i % key.length];
        }
        raw = new TextDecoder().decode(decoded);
      }
      var parsed = JSON.parse(raw);
      return String(
        (parsed && parsed.state && parsed.state.managementKey) ||
        (parsed && parsed.managementKey) ||
        ''
      );
    } catch (_) {
      return '';
    }
  }

  function currentManagementKey() {
    return capturedManagementKey || rememberedManagementKey();
  }

  async function request(method, path, body) {
    var managementKey = currentManagementKey();
    if (!managementKey) {
      throw new Error('no-management-key');
    }
    var options = {
      method: method,
      headers: {
        'Authorization': 'Bearer ' + managementKey,
        'Content-Type': 'application/json'
      }
    };
    if (body !== undefined) options.body = JSON.stringify(body);
    var response = await nativeFetch.call(window, path, options);
    if (!response.ok) {
      throw new Error('http ' + response.status);
    }
  }

  // isCursorTitle reports whether a heading / button node is the Cursor OAuth
  // title, so we can anchor the inline block to the right provider card no
  // matter how upstream shuffles CSS module hash classes.
  function isCursorTitle(node) {
    if (!node || !node.textContent) return false;
    var text = node.textContent.replace(/\s+/g, ' ').trim();
    return text === 'Cursor OAuth' || text === 'Cursor登录' || text === '开始 Cursor 登录' ||
      text === 'Start Cursor Login' || text === 'Cursor 登入' || text === '開始 Cursor 登入';
  }

  // findCursorContainer scans the document for the Cursor OAuth provider card
  // and returns a container that should host the inline input. A card is
  // recognised by a descendant whose text is the Cursor OAuth title; the
  // container is the nearest ancestor whose class hints at a card / section.
  function findCursorContainer(root) {
    if (!root) return null;
    var all = root.querySelectorAll('*');
    for (var i = 0; i < all.length; i += 1) {
      var node = all[i];
      if (!isCursorTitle(node)) continue;
      var el = node;
      var guard = 0;
      while (el && el !== root && guard < 8) {
        el = el.parentElement;
        guard += 1;
        if (!el) break;
        var cls = String(el.className || '');
        if (/card|section|provider|oauthContent|role="section"|content/i.test(cls)) {
          return el;
        }
      }
      return node.parentElement;
    }
    return null;
  }

  function mount(container) {
    if (container.__cpaKeyInlineMounted) return;
    container.__cpaKeyInlineMounted = true;

    var input = document.createElement('input');
    input.type = 'password';
    input.autocomplete = 'off';
    input.placeholder = 'Cursor API Key';
    input.className = 'cpa-key-input';

    var submit = document.createElement('button');
    submit.type = 'submit';
    submit.className = 'cpa-key-submit';
    submit.textContent = '添加';

    var form = document.createElement('form');
    form.className = 'cpa-key-form';
    form.appendChild(input);
    form.appendChild(submit);

    var style = document.createElement('style');
    style.className = 'cpa-key-style';
    style.textContent =
      '.cpa-key-form{display:flex;gap:8px;margin-top:12px}' +
      '.cpa-key-input{flex:1;min-width:0;padding:8px 10px;border:1px solid var(--border-color,#d0d7de);border-radius:8px;background:var(--input-bg,#fff);color:var(--text-primary,#1f2328);font-size:14px}' +
      '.cpa-key-input:focus{outline:none;border-color:var(--accent,#0969da)}' +
      '.cpa-key-input.cpa-key-fail{border-color:#c73e4e}' +
      '.cpa-key-submit{border:0;border-radius:8px;padding:8px 16px;background:var(--accent,#0969da);color:#fff;font-weight:600;font-size:14px;cursor:pointer}';

    form.appendChild(style);
    container.appendChild(form);

    form.addEventListener('submit', async function (event) {
      event.preventDefault();
      var raw = input.value.trim();
      if (!raw) return;
      submit.disabled = true;
      var ok = true;
      try {
        await request('POST', managementPath + 'cursor-api-key', { 'api-key': raw });
        input.value = '';
      } catch (_) {
        ok = false;
      }
      submit.disabled = false;
      input.classList.toggle('cpa-key-fail', !ok);
    });
  }

  function ensureMounted(root) {
    var container = findCursorContainer(root || document);
    if (container) mount(container);
  }

  function start() {
    ensureMounted();
    if (window.MutationObserver) {
      try {
        new MutationObserver(function () { ensureMounted(); })
          .observe(document.documentElement, { childList: true, subtree: true });
      } catch (_) {}
    }
    setInterval(function () { ensureMounted(); }, 900);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start, { once: true });
  } else {
    start();
  }
})();
</script>
`

// AddCursorAPIKeyInlineToManagementHTML embeds the inline Cursor API-key config
// surface into the management panel. It replaces the old standalone
// #/cursor-api-key page: no nav item, no route, no standalone script — just a
// single card-embedded API-Key input plus a submit button next to the Cursor
// OAuth login action. The upstream asset stays byte-for-byte on disk; the
// injection happens at serve time.
func AddCursorAPIKeyInlineToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(cursorAPIKeyInlineMarker)) {
		return data, nil
	}

	lower := bytes.ToLower(data)
	index := bytes.LastIndex(lower, []byte("</body>"))
	if index < 0 {
		index = bytes.LastIndex(lower, []byte("</html>"))
	}
	if index < 0 {
		index = len(data)
	}
	data = insertBytes(data, index, []byte(cursorAPIKeyInlineScript))
	return data, nil
}

const runtimeSchedulerInlineMarker = `id="cpa-runtime-scheduler-script"`

const runtimeSchedulerInlineScript = `
<script id="cpa-runtime-scheduler-script">
(function () {
  'use strict';
  if (window.__cpaRuntimeSchedulerLoaded) return;
  window.__cpaRuntimeSchedulerLoaded = true;

  var managementPath = '/v0/management/';
  var managementBase = '';
  var managementAuthorization = '';
  var managementReady = false;
  var sessionGeneration = 0;
  var authFilesGeneration = 0;
  var authFiles = new Map();
  var authFilesRefreshPending = false;
  var resetTurnStateView = function () {};
  var renderSchedulerAccounts = function () {};
  var accountStatePending = new Set();
  var nativeFetch = window.fetch;

  function clearManagementSession() {
    sessionGeneration += 1;
    managementBase = '';
    managementAuthorization = '';
    managementReady = false;
    authFilesRefreshPending = false;
    authFiles.clear();
    accountStatePending.clear();
    resetTurnStateView();
    renderQuotaCosts();
    renderAccountStateControls();
  }

  function observeManagementRequest(method, url, headers) {
    var parsed = new URL(url, window.location.href);
    var index = parsed.pathname.indexOf(managementPath);
    if (index < 0) return null;
    var authorization = headers.get('authorization');
    if (!authorization && headers.has('x-management-key')) authorization = 'Bearer ' + headers.get('x-management-key');
    if (!authorization) return null;
    var base = parsed.origin + parsed.pathname.slice(0, index) + managementPath;
    if (base !== managementBase || authorization !== managementAuthorization) {
      clearManagementSession();
      managementBase = base;
      managementAuthorization = authorization;
    }
    var endpoint = parsed.pathname.slice(index + managementPath.length);
    var readsAuthFiles = method.toUpperCase() === 'GET' && endpoint === 'auth-files';
    if (readsAuthFiles) authFilesGeneration += 1;
    return { endpoint: endpoint, readsAuthFiles: readsAuthFiles, session: sessionGeneration, files: authFilesGeneration };
  }

  function observeManagementResponse(info, status, data) {
    if (!info || info.session !== sessionGeneration) return;
    if (status === 401 || status === 403) {
      clearManagementSession();
      return;
    }
    if (status < 200 || status >= 300) return;
    managementReady = true;
    if (info.readsAuthFiles && info.files === authFilesGeneration) {
      authFiles = new Map(data.files.map(function (item) { return [item.name, item]; }));
      renderQuotaCosts();
      renderAccountStateControls();
      renderSchedulerAccounts(data.files);
    }
    if (info.endpoint === 'reset-quota' || info.endpoint === 'auth-files/status' || (info.endpoint === 'api-call' && document.querySelector('[class*="QuotaPage-module__page"]'))) {
      refreshAuthFiles();
    }
  }

  function refreshAuthFiles() {
    if (authFilesRefreshPending) return;
    authFilesRefreshPending = true;
    var generation = sessionGeneration;
    request('GET', 'auth-files').catch(function () {
      console.warn('cpa: account costs refresh failed');
    }).finally(function () {
      if (generation === sessionGeneration) authFilesRefreshPending = false;
    });
  }

  function request(method, path, body) {
    if (!managementReady) return Promise.reject(new Error('no-management-session'));
    var options = { method: method, headers: { 'Content-Type': 'application/json', 'Authorization': managementAuthorization } };
    if (body !== undefined) options.body = JSON.stringify(body);
    return window.fetch(managementBase + path, options).then(function (response) {
      if (!response.ok) return response.json().then(function (data) { throw new Error(data.error || ('http ' + response.status)); });
      return response;
    });
  }

  // Observe the panel's session and account responses without consuming them.
  var nativeXHROpen = XMLHttpRequest.prototype.open;
  var nativeXHRSetRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
  var nativeXHRSend = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cpaSchedulerMethod = method;
    this.__cpaSchedulerURL = url;
    this.__cpaSchedulerHeaders = new Headers();
    return nativeXHROpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    this.__cpaSchedulerHeaders.append(name, value);
    return nativeXHRSetRequestHeader.apply(this, arguments);
  };
  XMLHttpRequest.prototype.send = function () {
    var info = observeManagementRequest(this.__cpaSchedulerMethod, this.__cpaSchedulerURL, this.__cpaSchedulerHeaders);
    if (info) this.addEventListener('load', function () {
      var data;
      if (info.readsAuthFiles && this.status >= 200 && this.status < 300) {
        try {
          data = this.responseType === 'json' ? this.response : JSON.parse(this.responseText);
        } catch (_) {
          console.warn('cpa: invalid account list response');
          return;
        }
      }
      observeManagementResponse(info, this.status, data);
    }, { once: true });
    return nativeXHRSend.apply(this, arguments);
  };
  window.fetch = function (input, init) {
    var isRequest = input instanceof Request;
    var url = isRequest ? input.url : String(input);
    var method = (init && init.method) || (isRequest && input.method) || 'GET';
    var headers = new Headers((init && init.headers) || (isRequest && input.headers) || {});
    var info = observeManagementRequest(method, url, headers);
    return nativeFetch.apply(window, arguments).then(function (response) {
      if (info && info.readsAuthFiles && response.ok) {
        return response.clone().json().then(function (data) {
          observeManagementResponse(info, response.status, data);
          return response;
        }, function () {
          console.warn('cpa: invalid account list response');
          return response;
        });
      }
      observeManagementResponse(info, response.status);
      return response;
    });
  };
  window.addEventListener('unauthorized', clearManagementSession);
  window.addEventListener('hashchange', function () {
    if (/^#\/login(?:[/?]|$)/.test(window.location.hash)) clearManagementSession();
  });

  function make(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function accountSchedulingState(item) {
    return item.disabled ? 'disabled' : (item.degraded ? 'degraded' : 'normal');
  }

  function makeAccountStateControl(item, afterUpdate) {
    var group = make('div', 'cpa-account-state');
    group.setAttribute('data-cpa-auth-state', item.name);
    group.setAttribute('role', 'group');
    group.setAttribute('aria-label', '账号调度状态');
    group.account = item;
    var error = make('span', 'cpa-account-state-error');
    [['normal', '正常'], ['degraded', '降级'], ['disabled', '停用']].forEach(function (option) {
      var button = make('button', 'cpa-account-state-choice', option[1]);
      button.type = 'button';
      button.value = option[0];
      button.addEventListener('click', function (event) {
        event.stopPropagation();
        var current = group.account;
        var key = current.id || current.name;
        if (accountStatePending.has(key) || accountSchedulingState(current) === option[0]) return;
        accountStatePending.add(key);
        updateAccountStateControl(group, current);
        error.textContent = '';
        var generation = sessionGeneration;
        request('PATCH', 'auth-files/status', {name:current.name, auth_index:current.auth_index, scheduling_state:option[0]}).then(function () {
          if (generation !== sessionGeneration) return;
          current.disabled = option[0] === 'disabled';
          current.degraded = option[0] === 'degraded';
          current.auto_disabled = false;
          current.scheduling_state = option[0];
          authFiles.set(current.name, current);
          renderAccountStateControls();
          afterUpdate();
        }).catch(function (err) {
          if (generation === sessionGeneration) error.textContent = err.message;
        }).finally(function () {
          if (generation !== sessionGeneration) return;
          accountStatePending.delete(key);
          updateAccountStateControl(group, group.account);
          renderAccountStateControls();
        });
      });
      group.appendChild(button);
    });
    group.appendChild(error);
    updateAccountStateControl(group, item);
    return group;
  }

  function updateAccountStateControl(group, item) {
    group.account = item;
    var state = accountSchedulingState(item);
    group.querySelectorAll('button').forEach(function (button) {
      button.setAttribute('aria-pressed', String(button.value === state));
      button.disabled = !managementReady || accountStatePending.has(item.id || item.name);
    });
  }

  function renderAccountStateControls() {
    document.querySelectorAll('[class*="AuthFileCard-module__card___"]').forEach(function (card) {
      var nameNode = card.querySelector('[class*="AuthFileCard-module__fileName"]');
      var nativeToggle = card.querySelector('[class*="AuthFileCard-module__toggleWrap"]');
      if (!nameNode || !nativeToggle) return;
      var item = authFiles.get(nameNode.getAttribute('title') || nameNode.textContent.trim());
      var control = card.querySelector('[data-cpa-auth-state]');
      if (!item || item.runtime_only) {
        if (control) control.remove();
        nativeToggle.classList.remove('cpa-native-status-hidden');
        card.classList.remove('cpa-account-managed');
        return;
      }
      if (!control) {
        control = makeAccountStateControl(item, refreshAuthFiles);
        nativeToggle.insertAdjacentElement('afterend', control);
      }
      nativeToggle.classList.add('cpa-native-status-hidden');
      card.classList.add('cpa-account-managed');
      card.setAttribute('data-cpa-scheduling-state', accountSchedulingState(item));
      updateAccountStateControl(control, item);
    });
  }

  function renderQuotaCosts() {
    var cards = document.querySelectorAll('article[class*="QuotaCard-module__card"]');
    for (var i = 0; i < cards.length; i += 1) {
      var card = cards[i];
      var nameNode = card.querySelector('[class*="QuotaCard-module__fileName"]');
      var body = card.querySelector('[class*="QuotaCard-module__body"]');
      if (!nameNode || !body) continue;
      var item = authFiles.get(nameNode.getAttribute('title') || nameNode.textContent.trim());
      var line = card.querySelector('[data-cpa-estimated-cost]');
      if (!item) {
        if (line) line.remove();
        continue;
      }
      if (!line) {
        line = make('div', 'cpa-quota-estimate');
        line.setAttribute('data-cpa-estimated-cost', 'true');
        body.insertAdjacentElement('afterend', line);
      }
      var text = '预估费用：$' + Number(item.estimated_cost_usd || 0).toFixed(6);
      if (line.textContent !== text) line.textContent = text;
    }
  }

  function mount() {
    if (document.getElementById('cpa-runtime-scheduler-panel')) return;
    var toggle = make('button', 'cpa-runtime-scheduler-toggle', '账号调度');
    toggle.type = 'button';
    var panel = make('section', 'cpa-runtime-scheduler-panel');
    panel.id = 'cpa-runtime-scheduler-panel';
    panel.hidden = true;
    var title = make('div', 'cpa-runtime-scheduler-title', '账号调度设置');
    var hint = make('div', 'cpa-runtime-scheduler-hint', '全局设置与端口仅当前进程生效；账号状态会保存');
    var form = make('div', 'cpa-runtime-scheduler-form');
    var strategyLabel = make('div', 'cpa-runtime-scheduler-default', '调度方式：Codex2API 默认（round-robin）');
    var degradedPortLabel = make('label', 'cpa-runtime-scheduler-number');
    degradedPortLabel.appendChild(document.createTextNode('降级账号 API 端口'));
    var degradedPort = document.createElement('input');
    degradedPort.type = 'number';
    degradedPort.min = '0';
    degradedPort.max = '65535';
    degradedPort.step = '1';
    degradedPort.title = '0 表示关闭；与主端口共用 API Key';
    degradedPortLabel.appendChild(degradedPort);
    var c2aRetryCountLabel = make('label', 'cpa-runtime-scheduler-number');
    c2aRetryCountLabel.appendChild(document.createTextNode('C2A 最大重试次数'));
    var c2aRetryCount = document.createElement('input');
    c2aRetryCount.type = 'number';
    c2aRetryCount.min = '0';
    c2aRetryCount.max = '1000000';
    c2aRetryCount.step = '1';
    c2aRetryCountLabel.appendChild(c2aRetryCount);
    var c2aRetryIntervalLabel = make('label', 'cpa-runtime-scheduler-number');
    c2aRetryIntervalLabel.appendChild(document.createTextNode('C2A 最大重试间隔（秒）'));
    var c2aRetryInterval = document.createElement('input');
    c2aRetryInterval.type = 'number';
    c2aRetryInterval.min = '0';
    c2aRetryInterval.max = '1000000';
    c2aRetryInterval.step = '1';
    c2aRetryIntervalLabel.appendChild(c2aRetryInterval);
    var c2aRetryCredentialsLabel = make('label', 'cpa-runtime-scheduler-number');
    c2aRetryCredentialsLabel.appendChild(document.createTextNode('C2A 每轮最大账号数'));
    var c2aRetryCredentials = document.createElement('input');
    c2aRetryCredentials.type = 'number';
    c2aRetryCredentials.min = '0';
    c2aRetryCredentials.max = '1000000';
    c2aRetryCredentials.step = '1';
    c2aRetryCredentials.title = '0 表示每轮尝试所有可用账号';
    c2aRetryCredentialsLabel.appendChild(c2aRetryCredentials);
    var concurrencyLabel = make('label', 'cpa-runtime-scheduler-number');
    concurrencyLabel.appendChild(document.createTextNode('所有账号最大并发数'));
    var concurrency = document.createElement('input');
    concurrency.type = 'number';
    concurrency.min = '0';
    concurrency.max = '1000000';
    concurrency.step = '1';
    concurrency.title = '统一设置所有账号，0 表示不限制';
    concurrencyLabel.appendChild(concurrency);
    var unauthorizedLabel = make('label', 'cpa-runtime-scheduler-check');
    var unauthorized = document.createElement('input');
    unauthorized.type = 'checkbox';
    unauthorizedLabel.appendChild(unauthorized);
    unauthorizedLabel.appendChild(document.createTextNode('401 自动停用账号'));
    var quotaLabel = make('label', 'cpa-runtime-scheduler-check');
    var quota = document.createElement('input');
    quota.type = 'checkbox';
    quota.title = 'Codex：5H 耗尽暂停至重置，7D 耗尽自动停用；不受无视指数冷却影响';
    quotaLabel.appendChild(quota);
    quotaLabel.appendChild(document.createTextNode('额度耗尽自动调度'));
    var disableCoolingLabel = make('label', 'cpa-runtime-scheduler-check');
    var disableCooling = document.createElement('input');
    disableCooling.type = 'checkbox';
    disableCooling.title = '所有账号忽略账号和模型的错误冷却，等同于 disable_cooling=true';
    disableCoolingLabel.appendChild(disableCooling);
    disableCoolingLabel.appendChild(document.createTextNode('无视指数冷却'));
    var fingerprintLabel = make('label', 'cpa-runtime-scheduler-check');
    var fingerprintConvergence = document.createElement('input');
    fingerprintConvergence.type = 'checkbox';
    fingerprintConvergence.title = '仅 Codex OAuth/SetupToken 生效，每个账号固定使用独立身份';
    fingerprintLabel.appendChild(fingerprintConvergence);
    fingerprintLabel.appendChild(document.createTextNode('Codex 账号指纹收敛'));
    var turnStateLabel = make('label', 'cpa-runtime-scheduler-check');
    var turnStateEnabled = document.createElement('input');
    turnStateEnabled.type = 'checkbox';
    turnStateLabel.appendChild(turnStateEnabled);
    turnStateLabel.appendChild(document.createTextNode('Codex 292/332 state 自动续期'));
    var autoDegradeLabel = make('label', 'cpa-runtime-scheduler-check');
    var autoDegradeEnabled = document.createElement('input');
    autoDegradeEnabled.type = 'checkbox';
    autoDegradeEnabled.title = '随 state 采集结果自动切换正常或降级档位，不改变停用账号';
    autoDegradeEnabled.disabled = true;
    autoDegradeLabel.appendChild(autoDegradeEnabled);
    autoDegradeLabel.appendChild(document.createTextNode('Codex 自动降级'));
    turnStateEnabled.addEventListener('change', function () { autoDegradeEnabled.disabled = !turnStateEnabled.checked; });
    var turnStateData = null;
    var turnStateLoadedAt = 0;
    var turnStatePending = false;
    var turnStateViewError = '';
    var accountStatusNodes = new Map();
    resetTurnStateView = function () {
      turnStateData = null;
      turnStatePending = false;
      turnStateViewError = '';
      turnStateEnabled.checked = false;
      autoDegradeEnabled.checked = false;
      autoDegradeEnabled.disabled = true;
      accountStatusNodes.clear();
      statusList.textContent = '未连接管理接口';
    };
    function renderTurnStates() {
      var states = new Map(turnStateData ? turnStateData.accounts.map(function (item) { return [item.auth_id, item]; }) : []);
      var elapsed = Math.floor((performance.now() - turnStateLoadedAt) / 1000);
      accountStatusNodes.forEach(function (view, authID) {
        var item = states.get(authID);
        var text = '等待采集';
        var error = '';
        if (view.account.disabled) {
          text = '采集已暂停';
        } else if (turnStateViewError) {
          text = turnStateViewError;
        } else if (turnStateData && !turnStateData.enabled) {
          text = 'state 采集未开启';
        } else if (item) {
          var remaining = Math.max(0, item.remaining_seconds - elapsed);
          text = item.refreshing ? '正在采集...' : (item.last_error ? '采集失败 · 等待重试' : '等待采集');
          if (remaining > 0) {
            var minutes = Math.floor(remaining / 60);
            var seconds = String(remaining % 60).padStart(2, '0');
            text = item.token_length + ' 剩余（估算）：' + minutes + ':' + seconds;
            if (item.refreshing) text += ' · 续期中';
          } else if (item.status === 'active') {
            text = item.refreshing ? '已到期 · 续期中' : '已到期 · 等待续期';
          }
          if (item.observed_length && item.observed_length !== item.token_length) text += ' · 最近采集：' + item.observed_length;
          error = item.last_error || '';
        }
        if (view.state.textContent !== text) view.state.textContent = text;
        if (view.error.textContent !== error) view.error.textContent = error;
      });
    }
    function loadTurnStates() {
      if (turnStatePending || !managementReady) return;
      turnStatePending = true;
      var generation = sessionGeneration;
      request('GET', 'routing/codex-turn-state').then(function (response) {
        return response.json();
      }).then(function (data) {
        if (generation !== sessionGeneration) return;
        turnStateData = data;
        turnStateViewError = '';
        turnStateLoadedAt = performance.now();
        renderTurnStates();
        return loadAuthStatus();
      }).catch(function () {
        if (generation !== sessionGeneration) return;
        turnStateData = null;
        turnStateViewError = '读取 state 状态失败';
        renderTurnStates();
        return loadAuthStatus();
      }).finally(function () {
        if (generation === sessionGeneration) turnStatePending = false;
      });
    }
    var cacheRewriteLabel = make('label', 'cpa-runtime-scheduler-check');
    var cacheRewrite = document.createElement('input');
    cacheRewrite.type = 'checkbox';
    cacheRewriteLabel.appendChild(cacheRewrite);
    cacheRewriteLabel.appendChild(document.createTextNode('自定义复写 token 数'));
    var cacheMinLabel = make('label', 'cpa-runtime-scheduler-number');
    cacheMinLabel.appendChild(document.createTextNode('缓存比例 / 下限 (%)'));
    var cacheMin = document.createElement('input');
    cacheMin.type = 'number';
    cacheMin.min = '0';
    cacheMin.max = '100';
    cacheMin.step = 'any';
    cacheMin.placeholder = '0–100';
    cacheMinLabel.appendChild(cacheMin);
    var cacheMaxLabel = make('label', 'cpa-runtime-scheduler-number');
    cacheMaxLabel.appendChild(document.createTextNode('随机上限 (%)'));
    var cacheMax = document.createElement('input');
    cacheMax.type = 'number';
    cacheMax.min = '0';
    cacheMax.max = '100';
    cacheMax.step = 'any';
    cacheMax.placeholder = '可选';
    cacheMaxLabel.appendChild(cacheMax);
    function updateCacheRewriteInputs() {
      cacheMin.disabled = !cacheRewrite.checked;
      cacheMax.disabled = !cacheRewrite.checked;
      cacheMin.required = cacheRewrite.checked;
      cacheMax.min = cacheMin.value || '0';
    }
    cacheRewrite.addEventListener('change', updateCacheRewriteInputs);
    cacheMin.addEventListener('input', updateCacheRewriteInputs);
    updateCacheRewriteInputs();
    var routingLabel = make('label', 'cpa-runtime-scheduler-number');
    routingLabel.appendChild(document.createTextNode('Codex 路由提示'));
    var routingHint = document.createElement('select');
    [['europe', '欧洲'], ['north-america', '北美'], ['asia-pacific', '亚太'], ['', '关闭']].forEach(function (option) {
      var item = document.createElement('option');
      item.value = option[0];
      item.textContent = option[1];
      routingHint.appendChild(item);
    });
    routingLabel.appendChild(routingHint);
    var save = make('button', 'cpa-runtime-scheduler-save', '应用');
    save.type = 'button';
    save.disabled = true;
    var status = make('span', 'cpa-runtime-scheduler-status');
    form.appendChild(strategyLabel);
    form.appendChild(degradedPortLabel);
    form.appendChild(c2aRetryCountLabel);
    form.appendChild(c2aRetryIntervalLabel);
    form.appendChild(c2aRetryCredentialsLabel);
    form.appendChild(concurrencyLabel);
    form.appendChild(unauthorizedLabel);
    form.appendChild(quotaLabel);
    form.appendChild(disableCoolingLabel);
    form.appendChild(fingerprintLabel);
    form.appendChild(turnStateLabel);
    form.appendChild(autoDegradeLabel);
    form.appendChild(cacheRewriteLabel);
    form.appendChild(cacheMinLabel);
    form.appendChild(cacheMaxLabel);
    form.appendChild(routingLabel);
    form.appendChild(save);
    form.appendChild(status);
    var statusTitle = make('div', 'cpa-runtime-scheduler-subtitle', '账号状态');
    var statusList = make('div', 'cpa-runtime-scheduler-disabled-list', '正在读取...');
    panel.appendChild(title);
    panel.appendChild(hint);
    panel.appendChild(form);
    panel.appendChild(statusTitle);
    panel.appendChild(statusList);
    document.body.appendChild(toggle);
    document.body.appendChild(panel);

    var savedTogglePosition = null;
    try { savedTogglePosition = JSON.parse(localStorage.getItem('cpa-runtime-scheduler-toggle-position') || 'null'); } catch (_) {}
    if (savedTogglePosition && typeof savedTogglePosition.left === 'number' && typeof savedTogglePosition.top === 'number') {
      toggle.style.left = Math.max(8, Math.min(window.innerWidth - 160, savedTogglePosition.left)) + 'px';
      toggle.style.top = Math.max(8, Math.min(window.innerHeight - 56, savedTogglePosition.top)) + 'px';
      toggle.style.right = 'auto'; toggle.style.bottom = 'auto';
    }
    var suppressToggleClick = false;
    toggle.addEventListener('pointerdown', function (event) {
      var rect = toggle.getBoundingClientRect();
      var startX = event.clientX, startY = event.clientY;
      var originLeft = rect.left, originTop = rect.top;
      var moved = false;
      function move(moveEvent) {
        var dx = moveEvent.clientX - startX, dy = moveEvent.clientY - startY;
        if (!moved && Math.sqrt(dx * dx + dy * dy) < 4) return;
        moved = true;
        suppressToggleClick = true;
        var left = Math.max(8, Math.min(window.innerWidth - rect.width - 8, originLeft + dx));
        var top = Math.max(8, Math.min(window.innerHeight - rect.height - 8, originTop + dy));
        toggle.style.left = left + 'px'; toggle.style.top = top + 'px';
        toggle.style.right = 'auto'; toggle.style.bottom = 'auto';
      }
      function stop() {
        document.removeEventListener('pointermove', move);
        document.removeEventListener('pointerup', stop);
        if (moved) {
          var saved = toggle.getBoundingClientRect();
          localStorage.setItem('cpa-runtime-scheduler-toggle-position', JSON.stringify({left: saved.left, top: saved.top}));
        }
      }
      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', stop, { once: true });
    });

    var savedPosition = null;
    try { savedPosition = JSON.parse(localStorage.getItem('cpa-runtime-scheduler-position') || 'null'); } catch (_) {}
    if (savedPosition && typeof savedPosition.left === 'number' && typeof savedPosition.top === 'number') {
      panel.style.left = Math.max(8, Math.min(window.innerWidth - 360, savedPosition.left)) + 'px';
      panel.style.top = Math.max(8, Math.min(window.innerHeight - 120, savedPosition.top)) + 'px';
      panel.style.right = 'auto'; panel.style.bottom = 'auto';
    }
    function overlaps(first, second, padding) {
      return first.left < second.right + padding && first.right > second.left - padding && first.top < second.bottom + padding && first.bottom > second.top - padding;
    }
    function setPanelPosition(left, top) {
      var rect = panel.getBoundingClientRect();
      left = Math.max(8, Math.min(window.innerWidth - rect.width - 8, left));
      top = Math.max(8, Math.min(window.innerHeight - rect.height - 8, top));
      panel.style.left = left + 'px'; panel.style.top = top + 'px';
      panel.style.right = 'auto'; panel.style.bottom = 'auto';
    }
    function avoidManagementSaveButton() {
      if (panel.hidden) return;
      var panelRect = panel.getBoundingClientRect();
      var buttons = document.querySelectorAll('button,[role="button"]');
      for (var i = 0; i < buttons.length; i += 1) {
        var button = buttons[i];
        if (button === toggle || panel.contains(button)) continue;
        var text = (button.textContent || '').trim().toLowerCase();
        if (!text || (text.indexOf('保存') < 0 && text.indexOf('save') < 0)) continue;
        var buttonRect = button.getBoundingClientRect();
        if (buttonRect.width === 0 || buttonRect.height === 0 || !overlaps(panelRect, buttonRect, 0)) continue;
        var candidates = [
          [buttonRect.right + 12, buttonRect.top],
          [buttonRect.left - panelRect.width - 12, buttonRect.top],
          [buttonRect.left, buttonRect.top - panelRect.height - 12],
          [buttonRect.left, buttonRect.bottom + 12],
          [8, 8]
        ];
        for (var j = 0; j < candidates.length; j += 1) {
          setPanelPosition(candidates[j][0], candidates[j][1]);
          panelRect = panel.getBoundingClientRect();
          if (!overlaps(panelRect, buttonRect, 8)) {
            localStorage.setItem('cpa-runtime-scheduler-position', JSON.stringify({left: panelRect.left, top: panelRect.top}));
            return;
          }
        }
      }
    }
    title.style.cursor = 'move';
    title.addEventListener('pointerdown', function (event) {
      var rect = panel.getBoundingClientRect();
      var offsetX = event.clientX - rect.left, offsetY = event.clientY - rect.top;
      function move(moveEvent) {
        var left = Math.max(8, Math.min(window.innerWidth - rect.width - 8, moveEvent.clientX - offsetX));
        var top = Math.max(8, Math.min(window.innerHeight - rect.height - 8, moveEvent.clientY - offsetY));
        panel.style.left = left + 'px'; panel.style.top = top + 'px';
        panel.style.right = 'auto'; panel.style.bottom = 'auto';
      }
      function stop() {
        document.removeEventListener('pointermove', move);
        document.removeEventListener('pointerup', stop);
        avoidManagementSaveButton();
        var saved = panel.getBoundingClientRect();
        localStorage.setItem('cpa-runtime-scheduler-position', JSON.stringify({left: saved.left, top: saved.top}));
      }
      document.addEventListener('pointermove', move);
      document.addEventListener('pointerup', stop, { once: true });
      event.preventDefault();
    });

    toggle.addEventListener('click', function () {
      if (suppressToggleClick) {
        suppressToggleClick = false;
        return;
      }
      panel.hidden = !panel.hidden;
      if (!panel.hidden) {
        setTimeout(avoidManagementSaveButton, 0);
        loadSettings();
        loadTurnStates();
      }
    });
    window.addEventListener('resize', avoidManagementSaveButton);

    function loadSettings() {
      save.disabled = true;
      request('GET', 'routing/settings').then(function (response) {
        return response.json();
      }).then(function (data) {
        c2aRetryCount.value = String(data['c2a-max-retry-count'] == null ? 0 : data['c2a-max-retry-count']);
        c2aRetryInterval.value = String(data['c2a-max-retry-interval'] == null ? 0 : data['c2a-max-retry-interval']);
        c2aRetryCredentials.value = String(data['c2a-max-retry-credentials'] == null ? 0 : data['c2a-max-retry-credentials']);
        concurrency.value = String(data['auth-concurrency-limit'] == null ? 0 : data['auth-concurrency-limit']);
        degradedPort.value = String(data['degraded-port'] || 0);
        unauthorized.checked = !!data['auto-disable-unauthorized'];
        quota.checked = !!data['auto-disable-quota'];
        disableCooling.checked = !!data['disable-cooling'];
        fingerprintConvergence.checked = !!data['codex-fingerprint-convergence'];
        turnStateEnabled.checked = !!data['codex-turn-state-enabled'];
        autoDegradeEnabled.checked = !!data['codex-auto-degrade'];
        autoDegradeEnabled.disabled = !turnStateEnabled.checked;
        var rewrite = data['cache-token-rewrite'] || {};
        cacheRewrite.checked = !!rewrite.enabled;
        cacheMin.value = rewrite['min-percent'] == null ? '' : String(rewrite['min-percent']);
        cacheMax.value = rewrite['max-percent'] == null ? '' : String(rewrite['max-percent']);
        updateCacheRewriteInputs();
        routingHint.value = data['codex-routing-hint'] == null ? 'europe' : String(data['codex-routing-hint']);
        status.textContent = '';
        save.disabled = false;
      }).catch(function () {
        status.textContent = '读取失败';
      });
    }

    renderSchedulerAccounts = function (accounts) {
        var files = accounts.filter(function (item) { return item && item.runtime_only !== true; });
        statusList.textContent = '';
        accountStatusNodes.clear();
        if (!files.length) {
          statusList.appendChild(make('div', 'cpa-runtime-scheduler-empty', '暂无认证文件'));
          return;
        }
        files.forEach(function (item) {
          var row = make('div', 'cpa-runtime-scheduler-disabled-row');
          row.appendChild(make('strong', '', item.name || item.email || item.id || '未知认证'));
          var accountStatus = item.degraded ? '降级账号池' : '状态正常';
          if (item.auto_disabled) {
            accountStatus = '自动禁用原因：' + (item.status_message || item.auto_disable_reason || '未知');
          } else if (item.disabled) {
            accountStatus = '已停用';
          } else if (item.quota_paused) {
            accountStatus = '5H 额度耗尽，恢复时间：' + new Date(item.quota_resume_at).toLocaleString();
          } else if (item.unavailable) {
            accountStatus = item.status_message || '暂不可用';
          }
          row.appendChild(make('span', '', accountStatus));
          if (String(item.provider || item.type).toLowerCase() === 'codex') {
            var stateLine = make('span', 'cpa-runtime-scheduler-account-state');
            var stateError = make('span', 'cpa-runtime-scheduler-turn-state-error');
            row.appendChild(stateLine);
            row.appendChild(stateError);
            accountStatusNodes.set(item.id, {account:item, state:stateLine, error:stateError});
          }
          row.appendChild(makeAccountStateControl(item, loadAuthStatus));
          statusList.appendChild(row);
        });
        renderTurnStates();
    };

    function loadAuthStatus() {
      var generation = sessionGeneration;
      return request('GET', 'auth-files').catch(function () {
        if (generation !== sessionGeneration) return;
        accountStatusNodes.clear();
        statusList.textContent = '读取认证文件状态失败';
      });
    }

    save.addEventListener('click', function () {
      if (!degradedPort.reportValidity()) return;
      if (cacheRewrite.checked && (!cacheMin.reportValidity() || !cacheMax.reportValidity())) return;
      save.disabled = true;
      status.textContent = '应用中...';
      request('PATCH', 'routing/settings', {
        'degraded-port': Number(degradedPort.value || '0'),
        'c2a-max-retry-count': Math.max(0, Math.min(1000000, parseInt(c2aRetryCount.value || '0', 10) || 0)),
         'c2a-max-retry-interval': Math.max(0, Math.min(1000000, parseInt(c2aRetryInterval.value || '0', 10) || 0)),
         'c2a-max-retry-credentials': Math.max(0, Math.min(1000000, parseInt(c2aRetryCredentials.value || '0', 10) || 0)),
        'auth-concurrency-limit': Math.max(0, Math.min(1000000, parseInt(concurrency.value || '0', 10) || 0)),
        'auto-disable-unauthorized': unauthorized.checked,
         'auto-disable-quota': quota.checked,
         'disable-cooling': disableCooling.checked,
        'codex-fingerprint-convergence': fingerprintConvergence.checked,
        'codex-turn-state-enabled': turnStateEnabled.checked,
        'codex-auto-degrade': autoDegradeEnabled.checked,
        'cache-token-rewrite': {
          enabled: cacheRewrite.checked,
          'min-percent': cacheMin.value === '' ? null : Number(cacheMin.value),
          'max-percent': cacheMax.value === '' ? null : Number(cacheMax.value)
        },
        'codex-routing-hint': routingHint.value
      }).then(function () {
        status.textContent = '已应用';
        loadTurnStates();
      }).catch(function (err) {
        status.textContent = '应用失败：' + err.message;
      }).finally(function () {
        save.disabled = false;
      });
    });
    new MutationObserver(function () { renderQuotaCosts(); renderAccountStateControls(); }).observe(document.body, { childList: true, subtree: true });
    setInterval(function () { if (!panel.hidden) loadTurnStates(); }, 5000);
    setInterval(function () { if (!panel.hidden) renderTurnStates(); }, 1000);
    renderQuotaCosts();
    renderAccountStateControls();
  }

  var style = document.createElement('style');
   style.textContent = '.cpa-runtime-scheduler-toggle{position:fixed;right:20px;bottom:20px;z-index:2147483646;border:0;border-radius:8px;padding:9px 14px;background:#2563eb;color:#fff;font-size:13px;font-weight:600;cursor:grab;touch-action:none;user-select:none;box-shadow:0 4px 16px rgba(0,0,0,.2)}' +
     '.cpa-runtime-scheduler-panel{position:fixed;right:20px;bottom:64px;z-index:2147483646;width:min(340px,calc(100vw - 16px));max-height:70vh;overflow:auto;padding:16px;border:1px solid #d0d7de;border-radius:8px;background:#fff;color:#1f2328;box-shadow:0 10px 32px rgba(0,0,0,.2);font:14px/1.4 system-ui,sans-serif}' +
     '.cpa-runtime-scheduler-title{font-size:16px;font-weight:700;cursor:move;touch-action:none;user-select:none}.cpa-runtime-scheduler-hint{margin:4px 0 14px;color:#57606a;font-size:12px}.cpa-runtime-scheduler-form{display:grid;gap:9px}.cpa-runtime-scheduler-default{padding:7px 9px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:13px}.cpa-runtime-scheduler-number{display:flex;align-items:center;justify-content:space-between;gap:10px;color:#57606a}.cpa-runtime-scheduler-number input{width:100px;padding:5px;border:1px solid #d0d7de;border-radius:5px}.cpa-runtime-scheduler-check{display:flex;gap:8px;align-items:center}.cpa-runtime-scheduler-save{width:max-content;border:0;border-radius:6px;padding:7px 12px;background:#2563eb;color:#fff;cursor:pointer}.cpa-runtime-scheduler-status{color:#57606a;font-size:12px}.cpa-runtime-scheduler-subtitle{margin-top:18px;padding-top:12px;border-top:1px solid #d8dee4;font-weight:700}.cpa-runtime-scheduler-disabled-list{display:grid;gap:7px;margin-top:8px}.cpa-runtime-scheduler-disabled-row{display:grid;gap:4px;padding:8px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:12px}.cpa-runtime-scheduler-empty{color:#57606a;font-size:12px}';
  style.textContent += '.cpa-quota-estimate{color:var(--text-secondary,#57606a);font-size:12px;line-height:1.5;font-variant-numeric:tabular-nums;overflow-wrap:anywhere;letter-spacing:0}';
  style.textContent += '.cpa-runtime-scheduler-account-state{color:#57606a;overflow-wrap:anywhere;font-variant-numeric:tabular-nums;letter-spacing:0}.cpa-runtime-scheduler-turn-state-error{color:#b42318;overflow-wrap:anywhere}.cpa-runtime-scheduler-turn-state-error:empty{display:none}';
  style.textContent += '.cpa-native-status-hidden,.cpa-account-managed [class*="AuthFileCard-module__stateBadge"]{display:none!important}.cpa-account-state{display:flex;flex-wrap:wrap;gap:0;max-width:100%;font-size:12px;letter-spacing:0}.cpa-account-state-choice{min-width:48px;padding:5px 8px;border:1px solid var(--border-color,#d0d7de);background:var(--bg-primary,#fff);color:var(--text-secondary,#57606a);cursor:pointer}.cpa-account-state-choice:first-child{border-radius:6px 0 0 6px}.cpa-account-state-choice:nth-child(3){border-radius:0 6px 6px 0}.cpa-account-state-choice[aria-pressed="true"]{background:#2563eb;color:#fff}.cpa-account-state-choice[value="degraded"][aria-pressed="true"]{background:#b45309}.cpa-account-state-choice[value="disabled"][aria-pressed="true"]{background:#6b7280}.cpa-account-state-choice:disabled{cursor:wait;opacity:.6}.cpa-account-state-error{flex-basis:100%;color:#b42318;overflow-wrap:anywhere}.cpa-runtime-scheduler-disabled-row strong{overflow-wrap:anywhere}';
  document.documentElement.appendChild(style);
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount, { once: true });
  } else {
    mount();
  }
})();
</script>
`

// AddRuntimeSchedulerToManagementHTML injects runtime-only scheduler controls
// and quota cost labels without modifying the downloaded bundle.
func AddRuntimeSchedulerToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(runtimeSchedulerInlineMarker)) {
		return data, nil
	}
	lower := bytes.ToLower(data)
	index := bytes.LastIndex(lower, []byte("</body>"))
	if index < 0 {
		index = bytes.LastIndex(lower, []byte("</html>"))
	}
	if index < 0 {
		index = len(data)
	}
	return insertBytes(data, index, []byte(runtimeSchedulerInlineScript)), nil
}
