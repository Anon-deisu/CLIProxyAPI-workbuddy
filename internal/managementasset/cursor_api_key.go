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
// Cursor OAuth card. The standalone #/cursor-api-key page is gone; the add/edit
// surface now lives directly inside the Cursor OAuth card (卡片内嵌输入框).
//
// It finds the rendered Cursor OAuth provider card inside #/oauth, appends an
// inline "API Key" input (key + identity) and a compact list of existing
// credentials below the login action, and manages them through the existing
// /v0/management/cursor-api-key endpoints backed by the JSON store. Full
// credential editing (model prefix, priority, base url, cool-down, per-record
// notes) lives natively in #/auth-files under the separate 「Cursor API Key」
// category, which this server already synthesizes from every
// *_cursor-api-key.json file. The inline card input therefore only needs the
// quick add / mask / enable / delete path for everyday use.
//
// The script reuses the same management-authorization capture as the previous
// standalone manager: it transparently reads the panel session's Bearer token
// so the user never re-enters credentials. It does not depend on upstream
// module scope; it builds plain DOM reusing the OAuth page design-system class
// names for a native look.
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

  function escapeHTML(value) {
    return String(value == null ? '' : value)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  async function request(method, path, body) {
    var managementKey = currentManagementKey();
    if (!managementKey) {
      throw new Error('尚未获取到管理密钥。请确认已登录管理面板，然后刷新页面重试。');
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
    var payload = null;
    try {
      payload = await response.json();
    } catch (_) {
      payload = {};
    }
    if (!response.ok) {
      var message = payload && (payload.error || payload.message);
      if (response.status === 401) message = '管理密钥无效或已过期，请重新登录后刷新。';
      throw new Error(message || ('请求失败（HTTP ' + response.status + '）'));
    }
    return payload;
  }

  // isCursorTitle reports whether a heading / button node is the Cursor OAuth
  // title, so we can anchor the inline block to the right provider card no
  // matter how upstream shuffles CSS module hashes.
  function isCursorTitle(node) {
    if (!node || !node.textContent) return false;
    var text = node.textContent.replace(/\s+/g, ' ').trim();
    return text === 'Cursor OAuth' || text === 'Cursor登录' || text === '开始 Cursor 登录' ||
      text === 'Start Cursor Login' || text === 'Cursor 登入' || text === '開始 Cursor 登入';
  }

  // findCursorCards scans the document for the Cursor OAuth provider card and
  // returns every candidate container that should host the inline block. A card
  // is recognised by a descendant whose text is the Cursor OAuth title; the
  // container is the nearest ancestor whose class hints at a card / section.
  function findCursorCards(root) {
    var cards = [];
    if (!root) return cards;
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
          if (cards.indexOf(el) === -1) cards.push(el);
          break;
        }
      }
      if (!el || el === root) {
        // No structural hint: fall back to the title's own parent card content.
        var wrap = node.parentElement;
        if (wrap && cards.indexOf(wrap) === -1) cards.push(wrap);
      }
    }
    return cards;
  }

  function mount(container) {
    if (container.__cpaKeyInlineMounted) return;
    container.__cpaKeyInlineMounted = true;

    var entries = [];
    var keyInput = null;
    var emailInput = null;
    var status = null;
    var list = null;

    function setStatus(message, kind) {
      status.textContent = message;
      status.className = 'cpa-key-status' + (kind ? ' cpa-key-status--' + kind : '');
    }

    async function load() {
      setStatus('正在读取 Cursor API Key…');
      try {
        var payload = await request('GET', managementPath + 'cursor-api-key');
        entries = Array.isArray(payload['cursor-api-key']) ? payload['cursor-api-key'] : [];
        renderList();
        setStatus(entries.length ? '共 ' + entries.length + ' 个 Key；完整编辑请前往「Auth Files → Cursor API Key」' : '尚未配置；填入 Key 后点「添加」', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
      }
    }

    function renderList() {
      if (!entries.length) {
        list.innerHTML = '<div class="cpa-key-empty"><span>无 Cursor API Key</span><small>凭据统一在 Auth Files「Cursor API Key」分类下整理</small></div>';
        return;
      }
      list.innerHTML = entries.map(function (entry) {
        var meta = [];
        if (entry.email) meta.push('<span class="cpa-key-tag">' + escapeHTML(entry.email) + '</span>');
        meta.push('<span class="cpa-key-tag' + (entry.disabled ? ' cpa-key-tag--off' : ' cpa-key-tag--ok') + '">' + (entry.disabled ? '已停用' : '启用') + '</span>');
        meta.push('<span class="cpa-key-tag">优先级 ' + (entry.priority || 0) + '</span>');
        if (entry.prefix) meta.push('<span class="cpa-key-tag">前缀 ' + escapeHTML(entry.prefix) + '</span>');
        var success = Number(entry.success || 0);
        var failed = Number(entry.failed || 0);
        return '<div class="cpa-key-row">' +
          '<div class="cpa-key-main"><span class="cpa-key-mask">' + escapeHTML(entry['api-key']) + '</span>' +
          '<span class="cpa-key-tags">' + meta.join('') +
          '<span class="cpa-key-metric">成功 <b>' + success + '</b> 失败 <b>' + failed + '</b></span>' +
          '</span></div>' +
          '<div class="cpa-key-actions">' +
          '<button type="button" class="btn" data-toggle="' + entry.index + '">' + (entry.disabled ? '启用' : '停用') + '</button>' +
          '<button type="button" class="btn btn-danger" data-delete="' + entry.index + '">移除</button>' +
          '</div></div>';
      }).join('');
    }

    async function add(event) {
      event.preventDefault();
      var rawKey = keyInput.value.trim();
      if (!rawKey) {
        setStatus('请先填入 Cursor API Key', 'error');
        keyInput.focus();
        return;
      }
      var value = { 'api-key': rawKey };
      var email = emailInput.value.trim();
      if (email) value.email = email;
      setStatus('正在保存…');
      try {
        await request('POST', managementPath + 'cursor-api-key', value);
        keyInput.value = '';
        emailInput.value = '';
        await load();
        setStatus('已添加，凭据已同步到 Auth Files「Cursor API Key」', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
      }
    }

    async function toggle(index, disabled) {
      setStatus(disabled ? '正在启用…' : '正在停用…');
      try {
        await request('PATCH', managementPath + 'cursor-api-key', { index: index, value: { disabled: disabled } });
        await load();
        setStatus(disabled ? '已启用' : '已停用', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
      }
    }

    async function remove(index, mask) {
      if (!window.confirm('确定移除 ' + mask + ' 吗？此操作不可撤销。')) return;
      setStatus('正在移除…');
      try {
        await request('DELETE', managementPath + 'cursor-api-key?index=' + encodeURIComponent(index));
        await load();
        setStatus('已移除', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
      }
    }

    // Native DOM appended inside the provider card, reusing OAuth design-system
    // classes so it looks like part of the page.
    var block = document.createElement('div');
    block.className = 'cpa-key-inline';
    block.innerHTML =
      '<div class="cpa-key-divider">────────── API Key 快速配置 ──────────</div>' +
      '<form class="cpa-key-form">' +
      '<div class="cpa-key-fields">' +
      '<label class="cpa-key-field cpa-key-field--wide"><span>Cursor API Key</span>' +
      '<input type="password" autocomplete="off" placeholder="crsr_... 或 cursor_..." class="form-control"></label>' +
      '<label class="cpa-key-field"><span>标识 / 邮箱（可选）</span>' +
      '<input type="text" autocomplete="off" placeholder="account@email 或名称" class="form-control"></label>' +
      '</div>' +
      '<p class="cpa-key-hint">Key 只在保存时提交，列表不以明文返回。凭据会写入 Auth Files「Cursor API Key」分类统一管理。</p>' +
      '<div class="cpa-key-actions-row"><button type="submit" class="btn btn-primary">添加</button>' +
      '<button type="button" class="btn" data-refresh>刷新</button>' +
      '<span class="cpa-key-status">等待加载</span></div>' +
      '</form>' +
      '<div class="cpa-key-list"></div>' +
      '<style class="cpa-key-style">' +
      '.cpa-key-divider{color:var(--text-secondary);font-size:12px;letter-spacing:.5px;margin:14px 0 10px;text-align:center}' +
      '.cpa-key-form{display:flex;flex-direction:column;gap:10px}' +
      '.cpa-key-fields{display:grid;grid-template-columns:2fr 1fr;gap:10px}' +
      '.cpa-key-field{display:flex;flex-direction:column;gap:5px}.cpa-key-field--wide{grid-column:1/-1}' +
      '.cpa-key-field span{color:var(--text-primary);font-weight:600;font-size:13px}' +
      '.cpa-key-hint{margin:0;color:var(--text-secondary);font-size:12px;line-height:1.5}' +
      '.cpa-key-actions-row{display:flex;align-items:center;gap:9px;flex-wrap:wrap}' +
      '.cpa-key-status{height:20px;font-size:13px;color:var(--text-secondary)}' +
      '.cpa-key-status--ok{color:#16a34a}.cpa-key-status--error{color:var(--danger,#c73e4e)}' +
      '.cpa-key-list{display:grid;gap:10px;margin-top:12px}' +
      '.cpa-key-empty{padding:16px;border:1px dashed var(--border-color);border-radius:10px;text-align:center;color:var(--text-secondary);font-size:13px;display:grid;gap:4px}' +
      '.cpa-key-empty small{opacity:.85}' +
      '.cpa-key-row{display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap;border:1px solid var(--border-color);border-radius:10px;padding:9px 11px}' +
      '.cpa-key-main{display:flex;flex-direction:column;gap:6px}' +
      '.cpa-key-mask{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-weight:700;font-size:13px;word-break:break-all;color:var(--text-primary)}' +
      '.cpa-key-tags{display:flex;gap:6px;flex-wrap:wrap;align-items:center}' +
      '.cpa-key-tag{border:1px solid var(--border-color);border-radius:999px;padding:2px 8px;color:var(--text-secondary);font-size:12px}' +
      '.cpa-key-tag--ok{color:#16a34a;border-color:#22c55e55}.cpa-key-tag--off{color:var(--danger,#c73e4e);border-color:#ef444455}' +
      '.cpa-key-metric{color:var(--text-secondary);font-size:12px}.cpa-key-metric b{font-variant-numeric:tabular-nums;color:var(--text-primary)}' +
      '.cpa-key-actions{display:flex;gap:8px}' +
      '.cpa-key-inline .form-control{width:100%}' +
      '@media(max-width:640px){.cpa-key-fields{grid-template-columns:1fr}}</style>';

    container.appendChild(block);
    status = block.querySelector('.cpa-key-status');
    list = block.querySelector('.cpa-key-list');
    keyInput = block.querySelector('input[type=password]');
    emailInput = block.querySelector('input[type=text]');
    block.querySelector('form').addEventListener('submit', add);
    block.querySelector('[data-refresh]').addEventListener('click', load);
    list.addEventListener('click', function (event) {
      var btn = event.target.closest('button[data-toggle],button[data-delete]');
      if (!btn) return;
      var index = Number(btn.getAttribute('data-toggle') || btn.getAttribute('data-delete'));
      var entry = entries.find(function (item) { return item.index === index; });
      if (!entry) return;
      if (btn.hasAttribute('data-toggle')) {
        toggle(index, entry.disabled);
      } else if (btn.hasAttribute('data-delete')) {
        remove(index, entry['api-key']);
      }
    });

    // Key capture may lag the panel's first management request; retry once.
    if (!currentManagementKey()) {
      setTimeout(load, 700);
      return;
    }
    load();
  }

  function ensureMounted(root) {
    var cards = findCursorCards(root || document);
    for (var i = 0; i < cards.length; i += 1) mount(cards[i]);
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
// card-embedded input next to the Cursor OAuth login action. The upstream asset
// stays byte-for-byte on disk; the injection happens at serve time.
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
