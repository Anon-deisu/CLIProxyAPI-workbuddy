package managementasset

import (
	"bytes"
	"embed"
	"fmt"
)

//go:embed agent_workspace.js agent_workspace.css agent_vendor/*.js
var agentWorkspaceAssets embed.FS

const agentWorkspaceMarker = `id="cpa-agent-workspace-script"`
const rollingTokenUsageMarker = `id="cpa-rolling-token-usage-script"`

const rollingTokenUsageScript = `
<script id="cpa-rolling-token-usage-script">
(function () {
  'use strict';
  if (window.__cpaRollingTokenUsageLoaded) return;
  window.__cpaRollingTokenUsageLoaded = true;

  var managementPrefix = '';
  var managementKey = '';
  var rollingTokenData = null;
  var rollingInputAnchor = null;
  var rollingTokenTooltip = null;
  var rollingTooltipHideTimer = 0;
  var rollingTokenSignature = '';

  function captureURL(value) {
    try {
      var parsed = new URL(String(value || ''), window.location.href);
      var match = parsed.pathname.match(/\/v(?:0|8)\/management\//);
      if (match) managementPrefix = parsed.origin + parsed.pathname.slice(0, match.index);
    } catch (_) {}
  }

  function captureHeader(value) {
    var match = String(value || '').match(/^\s*Bearer\s+(.+?)\s*$/i);
    if (match && match[1]) managementKey = match[1];
  }

  var nativeFetch = window.fetch;
  window.fetch = function (input, init) {
    captureURL(typeof input === 'string' ? input : (input && input.url) || '');
    try {
      var headers = new Headers((init && init.headers) || (input && input.headers) || {});
      captureHeader(headers.get('authorization') || '');
    } catch (_) {}
    return nativeFetch.apply(window, arguments);
  };

  var nativeOpen = XMLHttpRequest.prototype.open;
  var nativeSetHeader = XMLHttpRequest.prototype.setRequestHeader;
  XMLHttpRequest.prototype.open = function (method, url) {
    captureURL(url);
    return nativeOpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    if (String(name || '').toLowerCase() === 'authorization') captureHeader(value);
    return nativeSetHeader.apply(this, arguments);
  };

  function storedManagementKey() {
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

  function tokenPanel() {
    var panel = document.querySelector('[class*="dashboard-module__heroPanel"]');
    if (!panel) return null;
    var label = panel.querySelector('[class*="heroPanelLabel"]');
    var split = panel.querySelector('[class*="heroSplit"]');
    if (!split) return null;
    var labelText = (label && label.textContent) || '';
    return { panel: panel, split: split, chinese: labelText.indexOf('已处理') >= 0 || labelText.indexOf('已處理') >= 0 };
  }

  function formatNumber(value) {
    return Math.max(0, Number(value) || 0).toLocaleString();
  }

  function sortedModels(models) {
    return (models || []).slice().sort(function (a, b) {
      var difference = (Number(b.input_tokens) || 0) - (Number(a.input_tokens) || 0);
      return difference || String(a.model || '').localeCompare(String(b.model || ''));
    });
  }

  function ensureTokenTooltip() {
    if (rollingTokenTooltip) return rollingTokenTooltip;
    rollingTokenTooltip = document.createElement('div');
    rollingTokenTooltip.id = 'cpa-rolling-token-tooltip';
    rollingTokenTooltip.className = 'cpa-rolling-token-tooltip';
    rollingTokenTooltip.setAttribute('role', 'tooltip');
    rollingTokenTooltip.hidden = true;
    rollingTokenTooltip.addEventListener('pointerenter', function () { clearTimeout(rollingTooltipHideTimer); });
    rollingTokenTooltip.addEventListener('pointerleave', scheduleTokenTooltipHide);
    document.body.appendChild(rollingTokenTooltip);
    return rollingTokenTooltip;
  }

  function positionTokenTooltip() {
    if (!rollingTokenTooltip || rollingTokenTooltip.hidden || !rollingInputAnchor) return;
    if (!rollingInputAnchor.isConnected) { hideTokenTooltip(); return; }
    var anchor = rollingInputAnchor.getBoundingClientRect();
    var bounds = rollingTokenTooltip.getBoundingClientRect();
    var left = Math.max(8, Math.min(anchor.left, window.innerWidth - bounds.width - 8));
    var top = anchor.bottom + 8;
    if (top + bounds.height > window.innerHeight - 8) top = anchor.top - bounds.height - 8;
    rollingTokenTooltip.style.left = left + 'px';
    rollingTokenTooltip.style.top = Math.max(8, Math.min(top, window.innerHeight - bounds.height - 8)) + 'px';
  }

  function renderTokenTooltip() {
    if (!rollingTokenTooltip || !rollingTokenData || !rollingInputAnchor) return;
    var chinese = rollingInputAnchor.closest('.cpa-rolling-token-summary').dataset.chinese === 'true';
    var minutes = Number(rollingTokenData.window_minutes) || 0;
    var models = sortedModels(rollingTokenData.models);
    var signature = JSON.stringify([chinese, minutes, models]);
    if (signature !== rollingTokenSignature) {
      rollingTokenSignature = signature;
      rollingTokenTooltip.textContent = '';
      var title = document.createElement('div');
      title.className = 'cpa-rolling-token-tooltip-title';
      title.textContent = chinese ? '近' + minutes + ' 分钟模型 Token 汇总' : 'Model token totals in the last ' + minutes + ' minutes';
      rollingTokenTooltip.appendChild(title);
      if (!models.length) {
        var empty = document.createElement('div');
        empty.className = 'cpa-rolling-token-tooltip-empty';
        empty.textContent = chinese ? '暂无模型 Token 统计' : 'No model token usage';
        rollingTokenTooltip.appendChild(empty);
      }
      var labels = chinese ? ['模型 ID', '总输入', '缓存输入', '缓存率', '输出'] : ['Model ID', 'Total input', 'Cached input', 'Cache rate', 'Output'];
      var formatter = new Intl.NumberFormat();
      var table = document.createElement('table');
      table.className = 'cpa-rolling-token-table';
      var head = table.createTHead().insertRow();
      labels.forEach(function (label) {
        var cell = document.createElement('th');
        cell.scope = 'col';
        cell.textContent = label;
        head.appendChild(cell);
      });
      var body = table.createTBody();
      models.forEach(function (model) {
        var row = body.insertRow();
        var modelCell = row.insertCell();
        modelCell.className = 'cpa-rolling-token-model';
        modelCell.textContent = model.model;
        row.insertCell().textContent = formatter.format(model.input_tokens || 0);
        row.insertCell().textContent = formatter.format(model.cached_input_tokens || 0);
        row.insertCell().textContent = model.input_tokens > 0 ? (model.cached_input_tokens * 100 / model.input_tokens).toFixed(1) + '%' : '--';
        row.insertCell().textContent = formatter.format(model.output_tokens || 0);
      });
      if (models.length) rollingTokenTooltip.appendChild(table);
    }
    rollingTokenTooltip.hidden = false;
    positionTokenTooltip();
  }

  function showTokenTooltip(anchor) {
    clearTimeout(rollingTooltipHideTimer);
    rollingInputAnchor = anchor;
    ensureTokenTooltip();
    renderTokenTooltip();
  }

  function scheduleTokenTooltipHide() {
    clearTimeout(rollingTooltipHideTimer);
    rollingTooltipHideTimer = window.setTimeout(hideTokenTooltip, 150);
  }

  function hideTokenTooltip() {
    clearTimeout(rollingTooltipHideTimer);
    if (rollingTokenTooltip) rollingTokenTooltip.hidden = true;
    rollingInputAnchor = null;
  }

  function bindInputTooltip(item) {
    item.classList.add('cpa-rolling-token-input-trigger');
    item.tabIndex = 0;
    item.setAttribute('aria-describedby', 'cpa-rolling-token-tooltip');
    item.addEventListener('pointerenter', function (event) {
      if (event.pointerType === 'mouse') showTokenTooltip(event.currentTarget);
    });
    item.addEventListener('pointerleave', scheduleTokenTooltipHide);
    item.addEventListener('focus', function (event) { showTokenTooltip(event.currentTarget); });
    item.addEventListener('blur', scheduleTokenTooltipHide);
    item.addEventListener('pointerdown', function (event) {
      if (event.pointerType !== 'touch') return;
      if (rollingInputAnchor === event.currentTarget) hideTokenTooltip();
      else showTokenTooltip(event.currentTarget);
    });
  }

  function createSummary(chinese) {
    var summary = document.createElement('div');
    summary.id = 'cpa-rolling-token-summary';
    summary.className = 'cpa-rolling-token-summary';
    summary.dataset.chinese = String(chinese);
    var labels = chinese ? ['总输入 token', '缓存 token', '输出 token', '缓存率'] : ['Input tokens', 'Cached tokens', 'Output tokens', 'Cache rate'];
    ['input_tokens', 'cached_tokens', 'output_tokens', 'cache_rate'].forEach(function (field, index) {
      var item = document.createElement('span');
      item.className = 'cpa-rolling-token-item';
      if (field === 'input_tokens') bindInputTooltip(item);
      var name = document.createElement('small');
      name.textContent = labels[index];
      var value = document.createElement('b');
      value.dataset.field = field;
      value.textContent = field === 'cache_rate' ? '0.0%' : '0';
      item.appendChild(name);
      item.appendChild(value);
      summary.appendChild(item);
    });
    return summary;
  }

  function ensureStyle() {
    if (document.getElementById('cpa-rolling-token-style')) return;
    var style = document.createElement('style');
    style.id = 'cpa-rolling-token-style';
    style.textContent = '.cpa-rolling-token-summary{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:8px;margin-top:12px;padding-top:10px;border-top:1px solid color-mix(in srgb,var(--border-color,#d8d4cc) 70%,transparent);font-size:12px}.cpa-rolling-token-item{display:flex;min-width:0;flex-direction:column;gap:2px}.cpa-rolling-token-item small{color:var(--text-secondary,#6d6760);white-space:nowrap}.cpa-rolling-token-item b{color:var(--text-primary,#2d2a26);font-variant-numeric:tabular-nums;white-space:nowrap}.cpa-rolling-token-input-trigger{cursor:help}.cpa-rolling-token-input-trigger:focus-visible{outline:2px solid var(--accent-color,#2563eb);outline-offset:2px}.cpa-rolling-token-tooltip{position:fixed;z-index:2147483647;box-sizing:border-box;width:max-content;max-width:calc(100vw - 16px);max-height:min(70vh,480px);overflow:auto;overscroll-behavior:contain;padding:8px 10px;border:1px solid var(--border-color,#d0d7de);border-radius:8px;background:var(--floating-surface,#fff);color:var(--text-primary,#1f2328);box-shadow:var(--floating-shadow,0 4px 20px rgba(0,0,0,.2));font:11px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace;font-variant-numeric:tabular-nums;white-space:nowrap}.cpa-rolling-token-tooltip[hidden]{display:none}.cpa-rolling-token-tooltip-title{margin-bottom:6px;color:var(--text-secondary,#57606a)}.cpa-rolling-token-table{width:max-content;border-collapse:separate;border-spacing:0;font:inherit;font-variant-numeric:tabular-nums;white-space:nowrap}.cpa-rolling-token-table th,.cpa-rolling-token-table td{padding:2px 0 2px 12px;text-align:right}.cpa-rolling-token-table th{color:var(--text-secondary,#57606a);font-weight:600;border-bottom:1px solid var(--border-color,#d0d7de)}.cpa-rolling-token-table th:first-child,.cpa-rolling-token-table td:first-child{padding-left:0;text-align:left}.cpa-rolling-token-model{max-width:220px;overflow-wrap:anywhere;white-space:normal}.cpa-rolling-token-tooltip-empty{color:var(--text-secondary,#57606a)}@media(max-width:640px){.cpa-rolling-token-summary{grid-template-columns:repeat(2,minmax(0,1fr))}}';
    document.head.appendChild(style);
  }

  function mount() {
    var target = tokenPanel();
    if (!target || target.panel.querySelector('#cpa-rolling-token-summary')) return;
    ensureStyle();
    target.split.insertAdjacentElement('afterend', createSummary(target.chinese));
    window.setTimeout(update, 0);
  }

  function update() {
    mount();
    var summary = document.getElementById('cpa-rolling-token-summary');
    if (!summary) return;
    var key = managementKey || storedManagementKey();
    if (!key) return;
    var path = (managementPrefix || window.location.origin) + '/v8/management/observability/usage/token-summary';
    nativeFetch(path, { headers: { 'Authorization': 'Bearer ' + key, 'Accept': 'application/json' } }).then(function (response) {
      if (!response.ok) throw new Error('token-summary request failed');
      return response.json();
    }).then(function (data) {
      summary.querySelector('[data-field="input_tokens"]').textContent = formatNumber(data.input_tokens);
      summary.querySelector('[data-field="cached_tokens"]').textContent = formatNumber(data.cached_tokens);
      summary.querySelector('[data-field="output_tokens"]').textContent = formatNumber(data.output_tokens);
      summary.querySelector('[data-field="cache_rate"]').textContent = (Number(data.cache_rate) || 0).toFixed(1) + '%';
      rollingTokenData = data;
      if (rollingInputAnchor) renderTokenTooltip();
    }).catch(function () {});
  }

  window.addEventListener('resize', positionTokenTooltip);
  document.addEventListener('scroll', positionTokenTooltip, true);
  document.addEventListener('keydown', function (event) { if (event.key === 'Escape') hideTokenTooltip(); });
  document.addEventListener('pointerdown', function (event) {
    if (rollingInputAnchor && !rollingInputAnchor.contains(event.target) && !(rollingTokenTooltip && rollingTokenTooltip.contains(event.target))) hideTokenTooltip();
  });

  var observer = new MutationObserver(function () { mount(); });
  observer.observe(document.documentElement, { childList: true, subtree: true });
  setInterval(update, 30000);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', update, { once: true });
  else update();
})();
</script>
`

// AgentWorkspaceAsset returns a public browser asset from an explicit allowlist.
func AgentWorkspaceAsset(name string) ([]byte, string, bool) {
	path, contentType := "", "application/javascript; charset=utf-8"
	switch name {
	case "workspace.js":
		path = "agent_workspace.js"
	case "workspace.css":
		path, contentType = "agent_workspace.css", "text/css; charset=utf-8"
	case "marked.js", "purify.js", "lucide.js":
		path = "agent_vendor/" + name
	default:
		return nil, "", false
	}
	data, errRead := agentWorkspaceAssets.ReadFile(path)
	return data, contentType, errRead == nil
}

// AddAgentWorkspaceToManagementHTML adds the shared chat workspace to the existing panel.
func AddAgentWorkspaceToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(agentWorkspaceMarker)) {
		return data, nil
	}
	index := bytes.LastIndex(bytes.ToLower(data), []byte("</body>"))
	if index < 0 {
		index = len(data)
	}
	return insertBytes(data, index, []byte(`<script id="cpa-agent-workspace-script" src="./management-agent/workspace.js"></script>`)), nil
}

// AddRollingTokenUsageToManagementHTML adds token totals below the dashboard request split.
func AddRollingTokenUsageToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(rollingTokenUsageMarker)) {
		return data, nil
	}
	index := bytes.LastIndex(bytes.ToLower(data), []byte("</body>"))
	if index < 0 {
		index = len(data)
	}
	return insertBytes(data, index, []byte(rollingTokenUsageScript)), nil
}
