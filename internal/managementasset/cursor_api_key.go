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
