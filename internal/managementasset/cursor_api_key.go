package managementasset

import (
	"bytes"
	"fmt"
	"regexp"
)

// cursorAPIKeyManagerMarker 是幂等性守卫标记：注入成功后再次调用会直接返回原内容，
// 避免重复注入导致页面出现多个导航项。
const cursorAPIKeyManagerMarker = `id:"cpa-native-route-host"`

// 导航标签：在所有语言里统一使用 "Cursor API Key" 这一拉丁专有名词，
// 与上游自身对 OAuth / quota 等专有名称的处理方式一致（不逐语言翻译专名）。
const cursorAPIKeyNavLabel = "Cursor API Key"

// cursorAPIKeyLocales 按语言注入 `nav.cursor_api_key` 与 `nav_meta.cursor_api_key` 文案。
// 每个语言对象里有 nav 与 nav_meta 两处，各自以 `oauth:`<值>`,quota_management:` 结尾，
// 共 4 种语言 × 2 = 8 处。锚点只用这段经过验证的、稳定的短尾（不依赖每处前面
// 容器的完整前缀，因为不同版本的容器文案会变动）。在 oauth 文案之后插入 nav 标签。
var cursorAPIKeyLocales = []struct {
	marker      []byte
	replacement []byte
}{
	// 简体中文：nav 与 nav_meta
	{
		marker:      []byte("oauth:`OAuth 登录`,quota_management:"),
		replacement: []byte("oauth:`OAuth 登录`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
	{
		marker:      []byte("oauth:`OAuth 授权登录`,quota_management:"),
		replacement: []byte("oauth:`OAuth 授权登录`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
	// 繁体中文：nav 与 nav_meta
	{
		marker:      []byte("oauth:`OAuth 登入`,quota_management:"),
		replacement: []byte("oauth:`OAuth 登入`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
	{
		marker:      []byte("oauth:`OAuth 授權登入`,quota_management:"),
		replacement: []byte("oauth:`OAuth 授權登入`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
	// 英文：nav 与 nav_meta
	{
		marker:      []byte("oauth:`OAuth Login`,quota_management:"),
		replacement: []byte("oauth:`OAuth Login`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
	{
		marker:      []byte("oauth:`OAuth authorization`,quota_management:"),
		replacement: []byte("oauth:`OAuth authorization`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
	// 俄文：nav 与 nav_meta
	{
		marker:      []byte("oauth:`OAuth вход`,quota_management:"),
		replacement: []byte("oauth:`OAuth вход`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
	{
		marker:      []byte("oauth:`Авторизация OAuth`,quota_management:"),
		replacement: []byte("oauth:`Авторизация OAuth`,cursor_api_key:`Cursor API Key`,quota_management:"),
	},
}

// remainingCursorAPIKeyLocale 兜底正则：匹配任意语言里尚未被固定锚点命中的
// `oauth:`...`,quota_management:`（例如上游未来新增语言时），在 `quota_management:` 之前
// 插入导航标签。
var remainingCursorAPIKeyLocale = regexp.MustCompile(
	"oauth:`([^`]*)`,quota_management:",
)

// cursorAPIKeyManagerScript 是在页面底部注入的独立 IIFE：监听原生路由挂载点
// `#cpa-native-route-host`，一旦出现（用户进入 #/cursor-api-key 路由）就把管理界面
// 渲染进去。界面复用 OAuth 页的设计系统类名，视觉上完全原生。
// 它不依赖页面模块、也不声明新 React 组件（React 是模块作用域，注入脚本无法访问）。
const cursorAPIKeyManagerScript = `
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

  function mount(host) {
    if (host.__cpaMounted) return;
    host.__cpaMounted = true;

    var entries = [];
    var editingIndex = null;
    var status = null;
    var list = null;
    var form = null;
    var formTitle = null;
    var apiKeyInput = null;
    var apiKeyHint = null;

    // 管理密钥完全来自当前已登录面板会话（自动捕获 Authorization 头或读取 cli-proxy-auth），
    // 页面不提供任何手动填写入口——用户已经登录，不该再出现凭据输入框。
    function currentManagementKey() {
      return capturedManagementKey || rememberedManagementKey();
    }

    function setStatus(message, kind) {
      status.textContent = message;
      status.className = 'cpa-status' + (kind ? ' cpa-status--' + kind : '');
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
        if (response.status === 401) {
          message = '管理密钥无效或已过期，请重新登录或在下方填写。';
        }
        throw new Error(message || ('请求失败（HTTP ' + response.status + '）'));
      }
      return payload;
    }

    function render() {
      if (!entries.length) {
        list.innerHTML = '<div class="cpa-empty">尚未配置 Cursor API Key</div>';
        return;
      }
      list.innerHTML = entries.map(function (entry) {
        var base = entry['base-url'] || '默认 https://api2.cursor.sh';
        var meta = [
          '<span class="cpa-tag' + (entry.disabled ? ' cpa-tag--off' : ' cpa-tag--ok') + '">' + (entry.disabled ? '已停用' : '启用') + '</span>',
          '<span class="cpa-tag">优先级 ' + (entry.priority || 0) + '</span>',
          '<span class="cpa-tag">' + escapeHTML(base) + '</span>'
        ];
        if (entry.prefix) meta.push('<span class="cpa-tag">前缀 ' + escapeHTML(entry.prefix) + '</span>');
        if (entry['disable-cooling']) meta.push('<span class="cpa-tag">不冷却</span>');
        if (entry['auth-index']) meta.push('<span class="cpa-tag">Auth ' + escapeHTML(entry['auth-index']) + '</span>');
        var success = Number(entry.success || 0);
        var failed = Number(entry.failed || 0);

        var runtime = '<div class="cpa-runtime">' +
          '<span class="cpa-metric">调用 <b>' + (success + failed) + '</b></span>' +
          '<span class="cpa-metric cpa-metric--ok">成功 <b>' + success + '</b></span>' +
          '<span class="cpa-metric cpa-metric--fail">失败 <b>' + failed + '</b></span>' +
          (entry.unavailable ? '<span class="cpa-tag cpa-tag--off">暂不可用</span>' : '') +
          '</div>';

        var lastError = entry.last_error || {};
        var errorMessage = lastError.message || entry.status_message || '';
        var errorHTML = '';
        if (errorMessage) {
          var errorMeta = [];
          if (lastError.code) errorMeta.push('<span>代码 ' + escapeHTML(lastError.code) + '</span>');
          if (lastError.http_status) errorMeta.push('<span>HTTP ' + escapeHTML(lastError.http_status) + '</span>');
          if (lastError.retryable) errorMeta.push('<span>可重试</span>');
          if (entry.next_retry_after) errorMeta.push('<span>下次重试 ' + escapeHTML(new Date(entry.next_retry_after).toLocaleString()) + '</span>');
          if (entry.updated_at) errorMeta.push('<span>状态更新 ' + escapeHTML(new Date(entry.updated_at).toLocaleString()) + '</span>');
          errorHTML = '<div class="cpa-error-box"><div class="cpa-error-title">最近错误</div><div>' + escapeHTML(errorMessage) + '</div>' +
            (errorMeta.length ? '<div class="cpa-error-meta">' + errorMeta.join('') + '</div>' : '') + '</div>';
        }

        return '<section class="card">' +
          '<div class="cpa-card-top"><div class="cpa-key">' + escapeHTML(entry['api-key']) + '</div><div class="cpa-tags">' + meta.join('') + '</div></div>' +
          '<div class="cpa-actions">' +
            '<button type="button" class="btn" data-action="edit" data-index="' + entry.index + '">编辑</button>' +
            '<button type="button" class="btn" data-action="toggle" data-index="' + entry.index + '">' + (entry.disabled ? '启用' : '停用') + '</button>' +
            '<button type="button" class="btn btn-danger" data-action="delete" data-index="' + entry.index + '">删除</button>' +
          '</div>' +
          runtime +
          errorHTML +
        '</section>';
      }).join('');
    }

    var keyRetried = false;
    async function load() {
      setStatus('正在读取 Cursor API Key…');
      // 若刚进入该路由、面板自身的认证请求还没来得及发出，密钥暂缺；
      // 稍后重试一次（面板初始化会在几百毫秒内以 Authorization 头发起管理请求，从而被自动捕获）。
      if (!currentManagementKey() && !keyRetried) {
        keyRetried = true;
        setTimeout(load, 600);
        return;
      }
      try {
        var payload = await request('GET', '/v0/management/cursor-api-key');
        entries = Array.isArray(payload['cursor-api-key']) ? payload['cursor-api-key'] : [];
        render();
        setStatus('已加载 ' + entries.length + ' 个 Key', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
        list.innerHTML = '<div class="cpa-empty">读取失败：' + escapeHTML(error.message || String(error)) + '</div>';
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
      var g = function (name) { return host.getElementsByClassName(name)[0]; };
      g('cpa-form-prefix').value = entry ? (entry.prefix || '') : '';
      g('cpa-form-priority').value = entry ? String(entry.priority || 0) : '0';
      g('cpa-form-baseurl').value = entry ? (entry['base-url'] || '') : '';
      g('cpa-form-excluded').value = entry && Array.isArray(entry['excluded-models'])
        ? entry['excluded-models'].join('\n')
        : '';
      g('cpa-form-cooling').checked = !!(entry && entry['disable-cooling']);
      g('cpa-form-disabled').checked = !!(entry && entry.disabled);
      form.classList.remove('cpa-hidden');
      form.scrollIntoView({ behavior: 'smooth', block: 'start' });
      apiKeyInput.focus();
    }

    function closeForm() {
      editingIndex = null;
      apiKeyInput.value = '';
      form.classList.add('cpa-hidden');
    }

    async function save(event) {
      event.preventDefault();
      var g = function (name) { return host.getElementsByClassName(name)[0]; };
      var rawKey = apiKeyInput.value.trim();
      if (editingIndex === null && !rawKey) {
        setStatus('新增时必须填写 Cursor API Key', 'error');
        apiKeyInput.focus();
        return;
      }
      var excluded = g('cpa-form-excluded').value
        .split(/\r?\n/)
        .map(function (value) { return value.trim(); })
        .filter(Boolean);
      var value = {
        'prefix': g('cpa-form-prefix').value.trim(),
        'priority': Number(g('cpa-form-priority').value || 0),
        'base-url': g('cpa-form-baseurl').value.trim(),
        'excluded-models': excluded,
        'disable-cooling': g('cpa-form-cooling').checked,
        'disabled': g('cpa-form-disabled').checked
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
        closeForm();
        await load();
        setStatus('保存成功，配置已热重载', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
      }
    }

    // 构造原生 DOM，复用 OAuth 页设计系统类名
    var panel = document.createElement('div');
    panel.className = 'OAuthPage-module__container';
    panel.innerHTML =
      '<h1 class="OAuthPage-module__pageTitle">Cursor API Key</h1>' +
      '<div class="OAuthPage-module__content" style="width:100%">' +
      '<section class="OAuthPage-module__providerSection">' +
      '<h2 class="OAuthPage-module__sectionTitle">API Key 列表</h2>' +
      '<div class="cpa-toolbar"><span class="cpa-status" role="status">等待加载</span>' +
      '<button type="button" class="btn" id="cpa-refresh">刷新</button>' +
      '<button type="button" class="btn btn-primary" id="cpa-add">新增 Key</button></div>' +
      '<div id="cpa-key-list" class="cpa-list"></div></section>' +
      '<section class="OAuthPage-module__providerSection">' +
      '<h2 class="OAuthPage-module__sectionTitle">新建 / 编辑</h2>' +
      '<form class="card cpa-form cpa-hidden" id="cpa-form">' +
      '<div class="cpa-form-title" id="cpa-form-title">新增 Cursor API Key</div>' +
      '<div class="OAuthPage-module__oauthGrid">' +
      '<div class="cpa-field cpa-field--full"><label for="cpa-api-key">Cursor API Key</label>' +
      '<input id="cpa-api-key" class="form-control cpa-form-key" type="password" autocomplete="off" placeholder="crsr_...">' +
      '<p class="cpa-hint" id="cpa-api-key-hint">Key 只在保存时提交，列表不会返回明文。</p></div>' +
      '<div class="cpa-field"><label for="cpa-prefix">模型前缀</label>' +
      '<input id="cpa-prefix" class="form-control cpa-form-prefix" placeholder="留空，与 Cursor OAuth 共用模型">' +
      '<p class="cpa-hint">留空时共用同名模型、工具调用和故障切换；仅隔离账号时填写。</p></div>' +
      '<div class="cpa-field"><label for="cpa-priority">优先级</label>' +
      '<input id="cpa-priority" class="form-control cpa-form-priority" type="number" value="0" step="1"></div>' +
      '<div class="cpa-field cpa-field--full"><label for="cpa-base-url">Cursor API 地址</label>' +
      '<input id="cpa-base-url" class="form-control cpa-form-baseurl" type="url" placeholder="默认 https://api2.cursor.sh"></div>' +
      '<div class="cpa-field cpa-field--full"><label for="cpa-excluded-models">排除模型</label>' +
      '<textarea id="cpa-excluded-models" class="form-control cpa-form-excluded" placeholder="每行一个模型名"></textarea></div>' +
      '<div class="cpa-field cpa-field--full">' +
      '<label class="check" style="display:inline-flex;align-items:center;gap:7px;cursor:pointer"><input class="cpa-form-cooling" type="checkbox" style="width:auto">禁用冷却调度</label>' +
      '<label class="check" style="display:inline-flex;align-items:center;gap:7px;cursor:pointer"><input class="cpa-form-disabled" type="checkbox" style="width:auto">停用此 Key</label></div>' +
      '</div>' +
      '<div class="cpa-form-actions"><button type="button" class="btn cpa-form-cancel">取消</button>' +
      '<button type="button" class="btn btn-primary cpa-form-save">保存</button></div>' +
      '</form></section></div>' +
      '<style class="cpa-style">' +
      '.cpa-status{height:20px;font-size:13px;color:var(--text-secondary)}' +
      '.cpa-status--ok{color:#16a34a}.cpa-status--error{color:var(--danger,#c73e4e)}' +
      '.cpa-toolbar{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:6px}' +
      '.cpa-list{display:grid;gap:14px}' +
      '.cpa-hidden{display:none!important}' +
      '.cpa-empty{padding:26px;border:1px dashed var(--border-color);border-radius:12px;text-align:center;color:var(--text-secondary)}' +
      '.cpa-card-top{display:flex;flex-direction:column;gap:10px}' +
      '.cpa-key{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:14px;font-weight:700;word-break:break-all;color:var(--text-primary)}' +
      '.cpa-tags{display:flex;gap:7px;flex-wrap:wrap}' +
      '.cpa-tag{border:1px solid var(--border-color);border-radius:999px;padding:3px 9px;color:var(--text-secondary);font-size:12px}' +
      '.cpa-tag--ok{color:#16a34a;border-color:#22c55e55}.cpa-tag--off{color:var(--danger,#c73e4e);border-color:#ef444455}' +
      '.cpa-actions{display:flex;gap:9px;flex-wrap:wrap}' +
      '.cpa-runtime{display:flex;gap:16px;align-items:center;flex-wrap:wrap;margin-top:10px;padding-top:11px;border-top:1px solid var(--border-color);font-size:13px;color:var(--text-secondary)}' +
      '.cpa-metric b{font-variant-numeric:tabular-nums;color:var(--text-primary)}.cpa-metric--ok b{color:#16a34a}.cpa-metric--fail b{color:var(--danger,#c73e4e)}' +
      '.cpa-error-box{margin-top:12px;padding:11px 12px;border:1px solid #ef444455;border-radius:9px;background:#ef444410;font-size:13px;line-height:1.5;word-break:break-word}' +
      '.cpa-error-title{color:var(--danger,#c73e4e);font-weight:700;margin-bottom:4px}' +
      '.cpa-error-meta{display:flex;gap:12px;flex-wrap:wrap;margin-top:6px;color:var(--text-secondary);font-size:12px}' +
      '.cpa-form-title{font-size:16px;font-weight:700;color:var(--text-primary);margin:0 0 14px}' +
      '.cpa-field{display:flex;flex-direction:column;gap:5px}.cpa-field--full{grid-column:1/-1}' +
      '.cpa-field label{color:var(--text-primary);font-weight:600;font-size:13px}' +
      '.cpa-hint{margin:0;color:var(--text-secondary);font-size:12px;line-height:1.5}' +
      '.cpa-form-actions{display:flex;justify-content:flex-end;gap:9px;margin-top:16px}' +
      '.cpa-form .form-control{width:100%}' +
      '@media(max-width:640px){.cpa-actions{justify-content:flex-start}}</style>';

    host.appendChild(panel);
    status = panel.querySelector('.cpa-status');
    list = panel.querySelector('#cpa-key-list');
    form = panel.querySelector('#cpa-form');
    formTitle = panel.querySelector('#cpa-form-title');
    apiKeyInput = panel.querySelector('#cpa-api-key');
    apiKeyHint = panel.querySelector('#cpa-api-key-hint');

    // DOM 全部就绪后再挂事件监听（此前版本在 DOM 构造前执行触发空指针）
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

    form.addEventListener('submit', save);
    host.getElementsByClassName('cpa-form-cancel')[0].addEventListener('click', closeForm);
    host.getElementsByClassName('cpa-form-save')[0].addEventListener('click', function () {
      form.dispatchEvent(new Event('submit', { cancelable: true, bubbles: true }));
    });

    panel.querySelector('#cpa-refresh').addEventListener('click', load);
    panel.querySelector('#cpa-add').addEventListener('click', function () { resetForm(null); });

    load();
  }

  function ensureHost() {
    var host = document.getElementById('cpa-native-route-host');
    if (host) mount(host);
  }

  // 挂载点由上游路由渲染：进入 #/cursor-api-key 时出现、离开时被 React 移除。
  // React 只卸载自己持有的节点，原生挂载内容随之一起消失；每次再次进入时
  // 由监听器重新检测并挂载。
  function start() {
    ensureHost();
    if (window.MutationObserver) {
      try {
        new MutationObserver(function () { ensureHost(); })
          .observe(document.documentElement, { childList: true, subtree: true });
      } catch (_) {}
    }
    setInterval(ensureHost, 800);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start, { once: true });
  } else {
    start();
  }
})();
</script>
`

// AddCursorAPIKeyManagerToManagementHTML adapts the upstream management bundle
// so the Cursor API-key manager becomes a native page under #/cursor-api-key
// instead of a floating third-party button. The upstream asset stays
// byte-for-byte on disk; adaptation happens at serve time, so an automatic
// panel refresh cannot remove this server-specific feature.
func AddCursorAPIKeyManagerToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(cursorAPIKeyManagerMarker)) {
		return data, nil
	}

	// 1. 导航项：插入到 /oauth 导航项之后（网关分类里的 OAuth 之后）
	navMarker := []byte("icon:K9.oauth}")
	if idx := bytes.Index(data, navMarker); idx >= 0 {
		addition := []byte(",{path:`/cursor-api-key`,labelKey:`nav.cursor_api_key`,metaKey:`nav_meta.cursor_api_key`,icon:K9.oauth}")
		data = insertBytes(data, idx+len(navMarker), addition)
	} else {
		return nil, fmt.Errorf("management html Cursor API Key nav anchor not found")
	}

	// 2. 路由：插入到 /oauth 路由之后
	routeMarker := []byte("(0,H.jsx)(VM,{})}")
	if idx := bytes.Index(data, routeMarker); idx >= 0 {
		addition := []byte(",{path:`/cursor-api-key`,element:(0,H.jsx)(\"div\",{id:\"cpa-native-route-host\"})}")
		data = insertBytes(data, idx+len(routeMarker), addition)
	} else {
		return nil, fmt.Errorf("management html Cursor API Key route anchor not found")
	}

	// 3. 导航文案：8 处固定锚点
	patchedLocales := 0
	for _, locale := range cursorAPIKeyLocales {
		if bytes.Contains(data, locale.marker) {
			data = bytes.Replace(data, locale.marker, locale.replacement, 1)
			patchedLocales++
		}
	}
	// 兜底：匹配任何尚未被固定锚点覆盖的 `oauth:`...`,quota_management:`（如未来新增语言），
	// 在 `,quota_management:` 之前插入导航标签。nav 与 nav_meta 两处同构锚点都会被覆盖。
	data = remainingCursorAPIKeyLocale.ReplaceAllFunc(data, func(match []byte) []byte {
		patchedLocales++
		label := []byte("cursor_api_key:`" + cursorAPIKeyNavLabel + "`,")
		return insertBytes(match, bytes.Index(match, []byte(",quota_management:")), label)
	})
	if patchedLocales == 0 {
		return nil, fmt.Errorf("management html Cursor API Key locale anchor not found")
	}

	// 4. 注入管理脚本（监听挂载点并渲染管理界面）
	lower := bytes.ToLower(data)
	index := bytes.LastIndex(lower, []byte("</body>"))
	if index < 0 {
		index = bytes.LastIndex(lower, []byte("</html>"))
	}
	if index < 0 {
		index = len(data)
	}
	data = insertBytes(data, index, []byte(cursorAPIKeyManagerScript))

	return data, nil
}
