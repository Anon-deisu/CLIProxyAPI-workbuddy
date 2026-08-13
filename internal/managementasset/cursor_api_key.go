package managementasset

import (
	"bytes"
	"fmt"
)

const cursorAPIKeyManagerMarker = `id="cpa-cursor-api-key-manager-script"`

var cursorAPIKeyManagerPatch = []byte(`
<script id="cpa-cursor-api-key-manager-script">
(function () {
  'use strict';
  if (window.__cpaCursorAPIKeyManagerLoaded) return;
  window.__cpaCursorAPIKeyManagerLoaded = true;

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

  function install() {
    if (!document.body || document.getElementById('cpa-cursor-api-key-manager-host')) return;

    var host = document.createElement('div');
    host.id = 'cpa-cursor-api-key-manager-host';
    document.body.appendChild(host);
    var root = host.attachShadow({ mode: 'open' });
    root.innerHTML = [
      '<style>',
      ':host{position:fixed;inset:0;z-index:2147482999;pointer-events:none;--bg:#fff;--panel:#f7f8fa;--text:#172033;--muted:#697386;--line:#dce1e8;--primary:#6558d9;--primary2:#5145b8;--danger:#c73e4e;--ok:#16845b;--shadow:0 24px 70px rgba(15,23,42,.28);font-family:Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;color:var(--text)}',
      '@media(prefers-color-scheme:dark){:host{--bg:#17191f;--panel:#20232b;--text:#f3f4f6;--muted:#a3aab8;--line:#343944;--primary:#8b7cf6;--primary2:#a89cf9;--danger:#f07886;--ok:#51c995;--shadow:0 24px 80px rgba(0,0,0,.58)}}',
      '*{box-sizing:border-box}.hidden{display:none!important}',
      '#open{position:fixed;left:22px;top:22px;z-index:1;pointer-events:auto;border:0;border-radius:999px;padding:12px 18px;background:linear-gradient(135deg,var(--primary),var(--primary2));color:#fff;font-weight:750;font-size:14px;box-shadow:0 8px 28px rgba(85,71,190,.52);cursor:pointer}',
      '#open:hover{transform:translateY(-1px)}',
      '#overlay{position:fixed;inset:0;z-index:2;pointer-events:auto;background:rgba(8,11,18,.62);backdrop-filter:blur(5px);display:flex;align-items:center;justify-content:center;padding:22px}',
      '#dialog{width:min(980px,100%);max-height:min(850px,94vh);overflow:auto;background:var(--bg);border:1px solid var(--line);border-radius:18px;box-shadow:var(--shadow)}',
      '.head{position:sticky;top:0;z-index:2;display:flex;align-items:flex-start;justify-content:space-between;gap:16px;padding:20px 22px;background:color-mix(in srgb,var(--bg) 94%,transparent);backdrop-filter:blur(12px);border-bottom:1px solid var(--line)}',
      '.head h2{margin:0;font-size:21px}.head p{margin:5px 0 0;color:var(--muted);font-size:13px}.icon{border:0;background:transparent;color:var(--muted);font-size:26px;line-height:1;cursor:pointer}',
      '.body{padding:20px 22px 24px}.toolbar{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:14px}',
      '.status{font-size:12px;color:var(--muted)}.status.error{color:var(--danger)}.status.ok{color:var(--ok)}',
      'button{font:inherit}.btn{border:1px solid var(--line);border-radius:9px;padding:8px 12px;background:var(--bg);color:var(--text);cursor:pointer;font-size:13px}.btn:hover{border-color:var(--primary)}.btn.primary{background:var(--primary);border-color:var(--primary);color:#fff;font-weight:650}.btn.danger{color:var(--danger)}.btn.small{padding:6px 9px;font-size:12px}',
      '#key-list{display:grid;gap:10px}.empty{padding:26px;border:1px dashed var(--line);border-radius:12px;text-align:center;color:var(--muted)}',
      '.card{border:1px solid var(--line);border-radius:12px;background:var(--panel);padding:14px}.card-top{display:flex;justify-content:space-between;gap:12px}.key{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:14px;font-weight:700;word-break:break-all}.meta{display:flex;gap:7px;flex-wrap:wrap;margin-top:8px}.tag{border:1px solid var(--line);border-radius:999px;padding:3px 7px;color:var(--muted);font-size:11px}.tag.ok{color:var(--ok);border-color:color-mix(in srgb,var(--ok) 38%,var(--line))}.tag.off{color:var(--danger);border-color:color-mix(in srgb,var(--danger) 38%,var(--line))}.actions{display:flex;gap:7px;flex-wrap:wrap;justify-content:flex-end}.runtime{display:flex;gap:14px;align-items:center;flex-wrap:wrap;margin-top:12px;padding-top:11px;border-top:1px solid var(--line);font-size:12px;color:var(--muted)}.metric b{margin-left:4px;color:var(--text);font-variant-numeric:tabular-nums}.metric.success b{color:var(--ok)}.metric.failed b{color:var(--danger)}.error-box{margin-top:10px;padding:10px 11px;border:1px solid color-mix(in srgb,var(--danger) 38%,var(--line));border-radius:9px;background:color-mix(in srgb,var(--danger) 7%,var(--panel));font-size:12px;line-height:1.45;word-break:break-word}.error-title{color:var(--danger);font-weight:700;margin-bottom:3px}.error-meta{display:flex;gap:10px;flex-wrap:wrap;margin-top:5px;color:var(--muted);font-size:11px}',
      '#form-wrap{margin-top:18px;border-top:1px solid var(--line);padding-top:18px}#form-wrap h3{margin:0 0 14px;font-size:16px}.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:13px}.field.full{grid-column:1/-1}.field label{display:block;margin-bottom:6px;color:var(--muted);font-size:12px;font-weight:650}',
      'input,textarea{width:100%;border:1px solid var(--line);border-radius:9px;padding:9px 10px;background:var(--panel);color:var(--text);outline:none;font:inherit;font-size:13px}input:focus,textarea:focus{border-color:var(--primary);box-shadow:0 0 0 3px color-mix(in srgb,var(--primary) 17%,transparent)}textarea{min-height:74px;resize:vertical}',
      '.checks{display:flex;gap:18px;flex-wrap:wrap;align-items:center}.check{display:flex!important;align-items:center;gap:7px;color:var(--text)!important;font-weight:500!important;cursor:pointer}.check input{width:auto}.hint{margin-top:5px;color:var(--muted);font-size:11px}.form-actions{display:flex;justify-content:flex-end;gap:9px;margin-top:15px}',
      '.auth{display:flex;align-items:end;gap:10px;padding:12px;margin-bottom:15px;border:1px solid var(--line);border-radius:11px;background:var(--panel)}.auth .field{flex:1}.auth-note{color:var(--muted);font-size:11px;line-height:1.45;margin-top:4px}',
      '@media(max-width:680px){#open{left:14px;top:14px}#overlay{padding:8px;align-items:flex-end}#dialog{max-height:96vh;border-radius:16px 16px 0 0}.grid{grid-template-columns:1fr}.field.full{grid-column:auto}.card-top{flex-direction:column}.actions{justify-content:flex-start}.auth{align-items:stretch;flex-direction:column}}',
      '</style>',
      '<button id="open" type="button">管理 Cursor API Key</button>',
      '<div id="overlay" class="hidden">',
      '  <section id="dialog" role="dialog" aria-modal="true" aria-labelledby="cursor-key-title">',
      '    <div class="head"><div><h2 id="cursor-key-title">Cursor API Key</h2><p>无需 OAuth，直接管理模型与工具调用凭据</p></div><button id="close" class="icon" type="button" aria-label="关闭">&times;</button></div>',
      '    <div class="body">',
      '      <div class="auth"><div class="field"><label for="management-key">管理密钥（通常自动获取）</label><input id="management-key" type="password" autocomplete="off" placeholder="仅在自动获取失败时填写"><div class="auth-note">不会写入配置或浏览器永久存储；刷新页面后清空。</div></div></div>',
      '      <div class="toolbar"><span id="status" class="status">等待打开</span><div><button id="refresh" class="btn" type="button">刷新</button> <button id="add" class="btn primary" type="button">新增 Key</button></div></div>',
      '      <div id="key-list"></div>',
      '      <form id="form-wrap" class="hidden">',
      '        <h3 id="form-title">新增 Cursor API Key</h3>',
      '        <div class="grid">',
      '          <div class="field full"><label for="api-key">Cursor API Key</label><input id="api-key" type="password" autocomplete="off" placeholder="crsr_..."><div id="api-key-hint" class="hint">Key 只在保存时提交，列表不会返回明文。</div></div>',
      '          <div class="field"><label for="prefix">模型前缀</label><input id="prefix" placeholder="留空，与 Cursor OAuth 共用模型"><div class="hint">留空时共用同名模型、工具调用和故障切换；仅隔离账号时填写。</div></div>',
      '          <div class="field"><label for="priority">优先级</label><input id="priority" type="number" value="0" step="1"></div>',
      '          <div class="field full"><label for="base-url">Cursor API 地址</label><input id="base-url" type="url" placeholder="默认 https://api2.cursor.sh"></div>',
      '          <div class="field full"><label for="excluded-models">排除模型</label><textarea id="excluded-models" placeholder="每行一个模型名"></textarea></div>',
      '          <div class="field full checks"><label class="check"><input id="disable-cooling" type="checkbox">禁用冷却调度</label><label class="check"><input id="disabled" type="checkbox">停用此 Key</label></div>',
      '        </div>',
      '        <div class="form-actions"><button id="cancel" class="btn" type="button">取消</button><button class="btn primary" type="submit">保存</button></div>',
      '      </form>',
      '    </div>',
      '  </section>',
      '</div>'
    ].join('');

    var openButton = root.getElementById('open');
    var overlay = root.getElementById('overlay');
    var closeButton = root.getElementById('close');
    var status = root.getElementById('status');
    var list = root.getElementById('key-list');
    var form = root.getElementById('form-wrap');
    var formTitle = root.getElementById('form-title');
    var apiKeyInput = root.getElementById('api-key');
    var apiKeyHint = root.getElementById('api-key-hint');
    var managementKeyInput = root.getElementById('management-key');
    var entries = [];
    var editingIndex = null;

    function currentManagementKey() {
      return managementKeyInput.value.trim() || capturedManagementKey || rememberedManagementKey();
    }

    function setStatus(message, kind) {
      status.textContent = message;
      status.className = 'status' + (kind ? ' ' + kind : '');
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
        throw new Error('未获取到管理密钥。请先登录管理面板，或在上方临时填写管理密钥。');
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
        if (response.status === 401) {
          message = '管理密钥无效或已过期，请重新登录或在上方填写。';
        }
        throw new Error(message || ('请求失败（HTTP ' + response.status + '）'));
      }
      return payload;
    }

    function tag(text, className) {
      return '<span class="tag' + (className ? ' ' + className : '') + '">' + escapeHTML(text) + '</span>';
    }

    function formatDateTime(value) {
      if (!value) return '';
      var date = new Date(value);
      return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString();
    }

    function render() {
      if (!entries.length) {
        list.innerHTML = '<div class="empty">尚未配置 Cursor API Key</div>';
        return;
      }
      list.innerHTML = entries.map(function (entry) {
        var base = entry['base-url'] || 'https://api2.cursor.sh（默认）';
        var meta = [
          tag(entry.disabled ? '已停用' : '启用', entry.disabled ? 'off' : 'ok'),
          tag('优先级 ' + (entry.priority || 0)),
          tag('地址 ' + base)
        ];
        if (entry.prefix) meta.push(tag('前缀 ' + entry.prefix));
        if (entry['disable-cooling']) meta.push(tag('不冷却'));
        if (entry['auth-index']) meta.push(tag('Auth ' + entry['auth-index']));
        var success = Number(entry.success || 0);
        var failed = Number(entry.failed || 0);
        var runtime = [
          '<div class="runtime">',
          '<span class="metric">调用<b>' + (success + failed) + '</b></span>',
          '<span class="metric success">成功<b>' + success + '</b></span>',
          '<span class="metric failed">失败<b>' + failed + '</b></span>'
        ];
        if (entry.unavailable) runtime.push(tag('暂不可用', 'off'));
        runtime.push('</div>');
        var lastError = entry.last_error || {};
        var errorMessage = lastError.message || entry.status_message || '';
        var errorHTML = '';
        if (errorMessage) {
          var errorMeta = [];
          if (lastError.code) errorMeta.push('<span>代码 ' + escapeHTML(lastError.code) + '</span>');
          if (lastError.http_status) errorMeta.push('<span>HTTP ' + escapeHTML(lastError.http_status) + '</span>');
          if (lastError.retryable) errorMeta.push('<span>可重试</span>');
          if (entry.next_retry_after) errorMeta.push('<span>下次重试 ' + escapeHTML(formatDateTime(entry.next_retry_after)) + '</span>');
          if (entry.updated_at) errorMeta.push('<span>状态更新 ' + escapeHTML(formatDateTime(entry.updated_at)) + '</span>');
          errorHTML = '<div class="error-box"><div class="error-title">最近错误</div><div>' + escapeHTML(errorMessage) + '</div>' +
            (errorMeta.length ? '<div class="error-meta">' + errorMeta.join('') + '</div>' : '') + '</div>';
        }
        return [
          '<article class="card">',
          '<div class="card-top"><div><div class="key">' + escapeHTML(entry['api-key']) + '</div><div class="meta">' + meta.join('') + '</div></div>',
          '<div class="actions">',
          '<button class="btn small" type="button" data-action="edit" data-index="' + entry.index + '">编辑</button>',
          '<button class="btn small" type="button" data-action="toggle" data-index="' + entry.index + '">' + (entry.disabled ? '启用' : '停用') + '</button>',
          '<button class="btn small danger" type="button" data-action="delete" data-index="' + entry.index + '">删除</button>',
          '</div></div>',
          runtime.join(''),
          errorHTML,
          '</article>'
        ].join('');
      }).join('');
    }

    async function load() {
      setStatus('正在读取 Cursor API Key…');
      try {
        var payload = await request('GET', '/v0/management/cursor-api-key');
        entries = Array.isArray(payload['cursor-api-key']) ? payload['cursor-api-key'] : [];
        render();
        setStatus('已加载 ' + entries.length + ' 个 Key', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
        list.innerHTML = '<div class="empty">读取失败，请检查管理密钥</div>';
      }
    }

    function resetForm(entry) {
      editingIndex = entry ? entry.index : null;
      formTitle.textContent = entry ? '编辑 Cursor API Key' : '新增 Cursor API Key';
      apiKeyInput.value = '';
      apiKeyInput.placeholder = entry ? '留空表示不更换现有 Key' : 'crsr_...';
      apiKeyHint.textContent = entry
        ? '当前 Key 已掩码；留空会保留原 Key。'
        : 'Key 只在保存时提交，列表不会返回明文。';
      root.getElementById('prefix').value = entry ? (entry.prefix || '') : '';
      root.getElementById('priority').value = entry ? String(entry.priority || 0) : '0';
      root.getElementById('base-url').value = entry ? (entry['base-url'] || '') : '';
      root.getElementById('excluded-models').value = entry && Array.isArray(entry['excluded-models'])
        ? entry['excluded-models'].join('\n')
        : '';
      root.getElementById('disable-cooling').checked = !!(entry && entry['disable-cooling']);
      root.getElementById('disabled').checked = !!(entry && entry.disabled);
      form.classList.remove('hidden');
      apiKeyInput.focus();
    }

    function closeForm() {
      editingIndex = null;
      apiKeyInput.value = '';
      form.classList.add('hidden');
    }

    async function save(event) {
      event.preventDefault();
      var rawKey = apiKeyInput.value.trim();
      if (editingIndex === null && !rawKey) {
        setStatus('新增时必须填写 Cursor API Key', 'error');
        apiKeyInput.focus();
        return;
      }
      var excluded = root.getElementById('excluded-models').value
        .split(/\r?\n/)
        .map(function (value) { return value.trim(); })
        .filter(Boolean);
      var value = {
        'prefix': root.getElementById('prefix').value.trim(),
        'priority': Number(root.getElementById('priority').value || 0),
        'base-url': root.getElementById('base-url').value.trim(),
        'excluded-models': excluded,
        'disable-cooling': root.getElementById('disable-cooling').checked,
        'disabled': root.getElementById('disabled').checked
      };
      if (rawKey) value['api-key'] = rawKey;

      setStatus('正在保存…');
      try {
        if (editingIndex === null) {
          await request('POST', '/v0/management/cursor-api-key', value);
        } else {
          await request('PATCH', '/v0/management/cursor-api-key', {
            index: editingIndex,
            value: value
          });
        }
        apiKeyInput.value = '';
        closeForm();
        await load();
        setStatus('保存成功，配置已热重载', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
      }
    }

    list.addEventListener('click', async function (event) {
      var button = event.target.closest('button[data-action]');
      if (!button) return;
      var index = Number(button.getAttribute('data-index'));
      var action = button.getAttribute('data-action');
      var entry = entries.find(function (item) { return item.index === index; });
      if (!entry) return;
      if (action === 'edit') {
        resetForm(entry);
        return;
      }
      if (action === 'delete') {
        if (!window.confirm('确定删除 ' + entry['api-key'] + ' 吗？此操作不可撤销。')) return;
        setStatus('正在删除…');
        try {
          await request('DELETE', '/v0/management/cursor-api-key?index=' + encodeURIComponent(index));
          closeForm();
          await load();
          setStatus('删除成功', 'ok');
        } catch (error) {
          setStatus(error.message || String(error), 'error');
        }
        return;
      }
      if (action === 'toggle') {
        setStatus(entry.disabled ? '正在启用…' : '正在停用…');
        try {
          await request('PATCH', '/v0/management/cursor-api-key', {
            index: index,
            value: { disabled: !entry.disabled }
          });
          await load();
          setStatus(entry.disabled ? '已启用' : '已停用', 'ok');
        } catch (error) {
          setStatus(error.message || String(error), 'error');
        }
      }
    });

    openButton.addEventListener('click', function () {
      overlay.classList.remove('hidden');
      load();
    });
    closeButton.addEventListener('click', function () {
      overlay.classList.add('hidden');
      closeForm();
    });
    overlay.addEventListener('click', function (event) {
      if (event.target === overlay) closeButton.click();
    });
    root.getElementById('refresh').addEventListener('click', load);
    root.getElementById('add').addEventListener('click', function () { resetForm(null); });
    root.getElementById('cancel').addEventListener('click', closeForm);
    form.addEventListener('submit', save);
    document.addEventListener('keydown', function (event) {
      if (event.key === 'Escape' && !overlay.classList.contains('hidden')) closeButton.click();
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', install, { once: true });
  } else {
    install();
  }
})();
</script>
`)

// AddCursorAPIKeyManagerToManagementHTML injects an isolated, dependency-free
// Cursor API key editor into the downloaded upstream management panel. The
// upstream asset remains untouched on disk, so automatic panel updates cannot
// remove this server-specific feature.
func AddCursorAPIKeyManagerToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(cursorAPIKeyManagerMarker)) {
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
	return insertBytes(data, index, cursorAPIKeyManagerPatch), nil
}
