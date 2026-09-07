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
// OAuth card (卡片内嵌输入框).
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
  var capturedManagementKey = '';
  var nativeFetch = window.fetch;

  function captureAuthorization(value) {
    if (typeof value !== 'string') return;
    var match = value.match(/^\s*Bearer\s+(.+?)\s*$/i);
    if (match && match[1]) capturedManagementKey = match[1];
  }

  function rememberedManagementKey() {
    try {
      var raw = localStorage.getItem('cli-proxy-auth');
      if (!raw) return '';
      if (raw.indexOf('enc::v1::') === 0) {
        var key = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + window.location.host + '|' + navigator.userAgent);
        var encoded = atob(raw.slice(9));
        var decoded = new Uint8Array(encoded.length);
        for (var i = 0; i < encoded.length; i += 1) decoded[i] = encoded.charCodeAt(i) ^ key[i % key.length];
        raw = new TextDecoder().decode(decoded);
      }
      var parsed = JSON.parse(raw);
      return String((parsed && parsed.state && parsed.state.managementKey) || (parsed && parsed.managementKey) || '');
    } catch (_) {
      return '';
    }
  }

  function currentManagementKey() {
    return capturedManagementKey || rememberedManagementKey();
  }

  function request(method, path, body) {
    var key = currentManagementKey();
    var options = { method: method, headers: { 'Content-Type': 'application/json' } };
    if (key) options.headers.Authorization = 'Bearer ' + key;
    if (body !== undefined) options.body = JSON.stringify(body);
    return nativeFetch.call(window, path, options).then(function (response) {
      if (!response.ok) throw new Error('http ' + response.status);
      return response;
    });
  }

  var originalFetch = window.fetch;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : (input && input.url) || '';
    if (String(url).indexOf(managementPath) !== -1) {
      var headers = new Headers((init && init.headers) || (input && input.headers) || {});
      captureAuthorization(headers.get('authorization') || '');
    }
    return originalFetch.apply(window, arguments);
  };

  function make(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
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
    var save = make('button', 'cpa-runtime-scheduler-save', '应用');
    save.type = 'button';
    var status = make('span', 'cpa-runtime-scheduler-status');
    form.appendChild(strategyLabel);
    form.appendChild(unauthorizedLabel);
    form.appendChild(quotaLabel);
    form.appendChild(save);
    form.appendChild(status);
    var disabledTitle = make('div', 'cpa-runtime-scheduler-subtitle', '账号调度与自动禁用认证文件');
    var disabledList = make('div', 'cpa-runtime-scheduler-disabled-list', '正在读取...');
    panel.appendChild(title);
    panel.appendChild(hint);
    panel.appendChild(form);
    panel.appendChild(disabledTitle);
    panel.appendChild(disabledList);
    document.body.appendChild(toggle);
    document.body.appendChild(panel);

    toggle.addEventListener('click', function () {
      panel.hidden = !panel.hidden;
      if (!panel.hidden) {
        loadSettings();
        loadDisabledAuths();
      }
    });

    function loadSettings() {
      request('GET', managementPath + 'routing/settings').then(function (response) {
        return response.json();
      }).then(function (data) {
        unauthorized.checked = !!data['auto-disable-unauthorized'];
        quota.checked = !!data['auto-disable-quota'];
      }).catch(function () {
        status.textContent = '读取失败';
      });
    }

    function loadDisabledAuths() {
      request('GET', managementPath + 'auth-files').then(function (response) {
        return response.json();
      }).then(function (data) {
        var files = (data && data.files || []).filter(function (item) { return item && item.runtime_only !== true; });
        disabledList.textContent = '';
        if (!files.length) {
          disabledList.appendChild(make('div', 'cpa-runtime-scheduler-empty', '暂无自动禁用认证文件'));
          return;
        }
        files.forEach(function (item) {
          var row = make('div', 'cpa-runtime-scheduler-disabled-row');
          var identity = make('strong', '', item.name || item.email || item.id || '未知认证');
          var detail = make('span', '', item.auto_disabled ? ('自动禁用原因：' + (item.auto_disable_reason || item.status_message || '未知')) : '状态正常');
          var concurrency = document.createElement('input');
          concurrency.type = 'number';
          concurrency.min = '0';
          concurrency.max = '1000000';
          concurrency.step = '1';
          concurrency.value = String(item.concurrency_limit || 0);
          concurrency.title = '每账号最大并发数，0 表示不限制';
          var saveConcurrency = make('button', 'cpa-runtime-scheduler-account-save', '并发');
          saveConcurrency.type = 'button';
          saveConcurrency.addEventListener('click', function () {
            var limit = Math.max(0, Math.min(1000000, parseInt(concurrency.value || '0', 10) || 0));
            saveConcurrency.disabled = true;
            request('PATCH', managementPath + 'auth-files/fields', {
              name: item.name || item.id,
              auth_index: item.auth_index,
              concurrency_limit: limit
            }).then(function () {
              concurrency.value = String(limit);
              detail.textContent = item.auto_disabled ? ('自动禁用原因：' + (item.auto_disable_reason || item.status_message || '未知')) : '状态正常';
            }).catch(function () {
              detail.textContent = '并发设置失败';
            }).finally(function () {
              saveConcurrency.disabled = false;
            });
          });
          row.appendChild(identity);
          row.appendChild(detail);
          var concurrencyLine = make('div', 'cpa-runtime-scheduler-account-concurrency');
          concurrencyLine.appendChild(make('span', '', '最大并发'));
          concurrencyLine.appendChild(concurrency);
          concurrencyLine.appendChild(saveConcurrency);
          row.appendChild(concurrencyLine);
          disabledList.appendChild(row);
        });
      }).catch(function () {
        disabledList.textContent = '读取认证文件状态失败';
      });
    }

    save.addEventListener('click', function () {
      save.disabled = true;
      status.textContent = '应用中...';
      request('PATCH', managementPath + 'routing/settings', {
        'auto-disable-unauthorized': unauthorized.checked,
        'auto-disable-quota': quota.checked
      }).then(function () {
        status.textContent = '已应用';
        loadDisabledAuths();
      }).catch(function () {
        status.textContent = '应用失败';
      }).finally(function () {
        save.disabled = false;
      });
    });
  }

  var style = document.createElement('style');
  style.textContent = '.cpa-runtime-scheduler-toggle{position:fixed;right:20px;bottom:20px;z-index:2147483646;border:0;border-radius:8px;padding:9px 14px;background:#2563eb;color:#fff;font-size:13px;font-weight:600;cursor:pointer;box-shadow:0 4px 16px rgba(0,0,0,.2)}' +
    '.cpa-runtime-scheduler-panel{position:fixed;right:20px;bottom:64px;z-index:2147483646;width:340px;max-height:70vh;overflow:auto;padding:16px;border:1px solid #d0d7de;border-radius:8px;background:#fff;color:#1f2328;box-shadow:0 10px 32px rgba(0,0,0,.2);font:14px/1.4 system-ui,sans-serif}' +
    '.cpa-runtime-scheduler-title{font-size:16px;font-weight:700}.cpa-runtime-scheduler-hint{margin:4px 0 14px;color:#57606a;font-size:12px}.cpa-runtime-scheduler-form{display:grid;gap:9px}.cpa-runtime-scheduler-default{padding:7px 9px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:13px}.cpa-runtime-scheduler-check{display:flex;gap:8px;align-items:center}.cpa-runtime-scheduler-save,.cpa-runtime-scheduler-account-save{width:max-content;border:0;border-radius:6px;padding:7px 12px;background:#2563eb;color:#fff;cursor:pointer}.cpa-runtime-scheduler-status{color:#57606a;font-size:12px}.cpa-runtime-scheduler-subtitle{margin-top:18px;padding-top:12px;border-top:1px solid #d8dee4;font-weight:700}.cpa-runtime-scheduler-disabled-list{display:grid;gap:7px;margin-top:8px}.cpa-runtime-scheduler-disabled-row{display:grid;gap:4px;padding:8px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:12px}.cpa-runtime-scheduler-disabled-row:has(span:first-of-type){border-left:3px solid #d0d7de}.cpa-runtime-scheduler-account-concurrency{display:flex;align-items:center;gap:5px;color:#57606a}.cpa-runtime-scheduler-account-concurrency input{width:70px;padding:4px;border:1px solid #d0d7de;border-radius:5px}.cpa-runtime-scheduler-account-save{padding:4px 8px;font-size:11px}.cpa-runtime-scheduler-empty{color:#57606a;font-size:12px}';
  document.documentElement.appendChild(style);
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount, { once: true });
  } else {
    mount();
  }
})();
</script>
`

// AddRuntimeSchedulerToManagementHTML injects runtime-only scheduler controls and
// the automatic-disable reason list without modifying the downloaded bundle.
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
