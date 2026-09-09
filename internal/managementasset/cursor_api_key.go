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
  var nativeFetch = window.fetch;

  function clearManagementSession() {
    sessionGeneration += 1;
    managementBase = '';
    managementAuthorization = '';
    managementReady = false;
    authFilesRefreshPending = false;
    authFiles.clear();
    renderQuotaCosts();
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
    }
    if (info.endpoint === 'reset-quota' || (info.endpoint === 'api-call' && document.querySelector('[class*="QuotaPage-module__page"]'))) {
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
      if (!response.ok) throw new Error('http ' + response.status);
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
    var hint = make('div', 'cpa-runtime-scheduler-hint', '仅当前进程生效，重启后恢复默认值');
    var form = make('div', 'cpa-runtime-scheduler-form');
    var strategyLabel = make('div', 'cpa-runtime-scheduler-default', '调度方式：Codex2API 默认（round-robin）');
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
    quotaLabel.appendChild(quota);
    quotaLabel.appendChild(document.createTextNode('额度用尽自动停用账号'));
    var fingerprintLabel = make('label', 'cpa-runtime-scheduler-check');
    var fingerprintConvergence = document.createElement('input');
    fingerprintConvergence.type = 'checkbox';
    fingerprintConvergence.title = '仅 Codex OAuth/SetupToken 生效，每个账号固定使用独立身份';
    fingerprintLabel.appendChild(fingerprintConvergence);
    fingerprintLabel.appendChild(document.createTextNode('Codex 账号指纹收敛'));
    var save = make('button', 'cpa-runtime-scheduler-save', '应用');
    save.type = 'button';
    save.disabled = true;
    var status = make('span', 'cpa-runtime-scheduler-status');
    form.appendChild(strategyLabel);
    form.appendChild(c2aRetryCountLabel);
    form.appendChild(c2aRetryIntervalLabel);
    form.appendChild(concurrencyLabel);
    form.appendChild(unauthorizedLabel);
    form.appendChild(quotaLabel);
    form.appendChild(fingerprintLabel);
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

    toggle.addEventListener('click', function () {
      panel.hidden = !panel.hidden;
      if (!panel.hidden) {
        loadSettings();
        loadAuthStatus();
      }
    });

    function loadSettings() {
      save.disabled = true;
      request('GET', 'routing/settings').then(function (response) {
        return response.json();
      }).then(function (data) {
        c2aRetryCount.value = String(data['c2a-max-retry-count'] == null ? 0 : data['c2a-max-retry-count']);
        c2aRetryInterval.value = String(data['c2a-max-retry-interval'] == null ? 0 : data['c2a-max-retry-interval']);
        concurrency.value = String(data['auth-concurrency-limit'] == null ? 0 : data['auth-concurrency-limit']);
        unauthorized.checked = !!data['auto-disable-unauthorized'];
        quota.checked = !!data['auto-disable-quota'];
        fingerprintConvergence.checked = !!data['codex-fingerprint-convergence'];
        status.textContent = '';
        save.disabled = false;
      }).catch(function () {
        status.textContent = '读取失败';
      });
    }

    function loadAuthStatus() {
      request('GET', 'auth-files').then(function (response) {
        return response.json();
      }).then(function (data) {
        var files = (data && data.files || []).filter(function (item) { return item && item.runtime_only !== true; });
        statusList.textContent = '';
        if (!files.length) {
          statusList.appendChild(make('div', 'cpa-runtime-scheduler-empty', '暂无认证文件'));
          return;
        }
        files.forEach(function (item) {
          var row = make('div', 'cpa-runtime-scheduler-disabled-row');
          row.appendChild(make('strong', '', item.name || item.email || item.id || '未知认证'));
          row.appendChild(make('span', '', item.auto_disabled ? ('自动禁用原因：' + (item.auto_disable_reason || item.status_message || '未知')) : '状态正常'));
          statusList.appendChild(row);
        });
      }).catch(function () {
        statusList.textContent = '读取认证文件状态失败';
      });
    }

    save.addEventListener('click', function () {
      save.disabled = true;
      status.textContent = '应用中...';
      request('PATCH', 'routing/settings', {
        'c2a-max-retry-count': Math.max(0, Math.min(1000000, parseInt(c2aRetryCount.value || '0', 10) || 0)),
        'c2a-max-retry-interval': Math.max(0, Math.min(1000000, parseInt(c2aRetryInterval.value || '0', 10) || 0)),
        'auth-concurrency-limit': Math.max(0, Math.min(1000000, parseInt(concurrency.value || '0', 10) || 0)),
        'auto-disable-unauthorized': unauthorized.checked,
        'auto-disable-quota': quota.checked,
        'codex-fingerprint-convergence': fingerprintConvergence.checked
      }).then(function () {
        status.textContent = '已应用';
      }).catch(function () {
        status.textContent = '应用失败';
      }).finally(function () {
        save.disabled = false;
      });
    });
    new MutationObserver(renderQuotaCosts).observe(document.body, { childList: true, subtree: true });
    renderQuotaCosts();
  }

  var style = document.createElement('style');
  style.textContent = '.cpa-runtime-scheduler-toggle{position:fixed;right:20px;bottom:20px;z-index:2147483646;border:0;border-radius:8px;padding:9px 14px;background:#2563eb;color:#fff;font-size:13px;font-weight:600;cursor:pointer;box-shadow:0 4px 16px rgba(0,0,0,.2)}' +
    '.cpa-runtime-scheduler-panel{position:fixed;right:20px;bottom:64px;z-index:2147483646;width:340px;max-height:70vh;overflow:auto;padding:16px;border:1px solid #d0d7de;border-radius:8px;background:#fff;color:#1f2328;box-shadow:0 10px 32px rgba(0,0,0,.2);font:14px/1.4 system-ui,sans-serif}' +
     '.cpa-runtime-scheduler-title{font-size:16px;font-weight:700}.cpa-runtime-scheduler-hint{margin:4px 0 14px;color:#57606a;font-size:12px}.cpa-runtime-scheduler-form{display:grid;gap:9px}.cpa-runtime-scheduler-default{padding:7px 9px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:13px}.cpa-runtime-scheduler-number{display:flex;align-items:center;justify-content:space-between;gap:10px;color:#57606a}.cpa-runtime-scheduler-number input{width:100px;padding:5px;border:1px solid #d0d7de;border-radius:5px}.cpa-runtime-scheduler-check{display:flex;gap:8px;align-items:center}.cpa-runtime-scheduler-save{width:max-content;border:0;border-radius:6px;padding:7px 12px;background:#2563eb;color:#fff;cursor:pointer}.cpa-runtime-scheduler-status{color:#57606a;font-size:12px}.cpa-runtime-scheduler-subtitle{margin-top:18px;padding-top:12px;border-top:1px solid #d8dee4;font-weight:700}.cpa-runtime-scheduler-disabled-list{display:grid;gap:7px;margin-top:8px}.cpa-runtime-scheduler-disabled-row{display:grid;gap:4px;padding:8px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:12px}.cpa-runtime-scheduler-empty{color:#57606a;font-size:12px}';
  style.textContent += '.cpa-quota-estimate{color:var(--text-secondary,#57606a);font-size:12px;line-height:1.5;font-variant-numeric:tabular-nums;overflow-wrap:anywhere;letter-spacing:0}';
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
