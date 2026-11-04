package managementasset

import (
	"bytes"
	"fmt"
)

const codexAutoOAuthMarker = `id="cpa-codex-auto-oauth-script"`

const codexAutoOAuthScript = `
<script id="cpa-codex-auto-oauth-script">
(function () {
  'use strict';
  if (window.__cpaCodexAutoOAuthLoaded) return;
  window.__cpaCodexAutoOAuthLoaded = true;

  var managementBase = '';
  var authorization = '';
  var nativeFetch = window.fetch;

  function observe(url, headers) {
    try {
      var parsed = new URL(url, window.location.href);
      var match = parsed.pathname.match(/\/v(?:0|8)\/management\//);
      if (!match) return;
      var index = match.index;
      var value = headers.get('authorization') || '';
      if (!value && headers.has('x-management-key')) value = 'Bearer ' + headers.get('x-management-key');
      if (value) {
        authorization = value;
        managementBase = parsed.origin + parsed.pathname.slice(0, index) + '/v0/management/';
      }
    } catch (_) {}
  }

  window.fetch = function (input, init) {
    var request = input instanceof Request;
    var url = request ? input.url : String(input || '');
    var headers = new Headers((init && init.headers) || (request && input.headers) || {});
    observe(url, headers);
    return nativeFetch.apply(window, arguments);
  };

  var nativeXHROpen = XMLHttpRequest.prototype.open;
  var nativeXHRHeader = XMLHttpRequest.prototype.setRequestHeader;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cpaCodexAutoURL = url;
    return nativeXHROpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    if (String(name).toLowerCase() === 'authorization') {
      observe(this.__cpaCodexAutoURL || '', new Headers({ authorization: String(value || '') }));
    }
    return nativeXHRHeader.apply(this, arguments);
  };

  function storedKey() {
    try {
      var raw = localStorage.getItem('cli-proxy-auth');
      if (raw && raw.indexOf('enc::v1::') === 0) {
        var key = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + window.location.host + '|' + navigator.userAgent);
        var encoded = atob(raw.slice(9));
        var decoded = new Uint8Array(encoded.length);
        for (var i = 0; i < encoded.length; i += 1) decoded[i] = encoded.charCodeAt(i) ^ key[i % key.length];
        raw = new TextDecoder().decode(decoded);
      }
      var parsed = JSON.parse(raw || '{}');
      return (parsed.state && parsed.state.managementKey) || parsed.managementKey || '';
    } catch (_) { return ''; }
  }

  function managementRequest(method, path, body) {
    var key = authorization || (storedKey() ? 'Bearer ' + storedKey() : '');
    if (!key || !managementBase) return Promise.reject(new Error('管理面板会话尚未就绪'));
    var options = {
      method: method,
      headers: { 'Authorization': key, 'Content-Type': 'application/json' },
    };
    if (body !== undefined) options.body = JSON.stringify(body);
    return nativeFetch(managementBase + path, options).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (data) {
        if (!response.ok && response.status !== 207) throw new Error(data.error || ('HTTP ' + response.status));
        return data;
      });
    });
  }

  function textOf(node) {
    return (node && node.textContent || '').replace(/\s+/g, ' ').trim();
  }

  function isCodexLogin(node) {
    var text = textOf(node);
    return /开始\s*Codex\s*登录|开始\s*Codex\s*登入|Start\s*Codex\s*Login|Codex\s*登录|Codex\s*登入/i.test(text);
  }

  function findLoginButton(root) {
    var nodes = (root || document).querySelectorAll('button, a, [role="button"]');
    for (var i = 0; i < nodes.length; i += 1) if (isCodexLogin(nodes[i])) return nodes[i];
    return null;
  }

  function closestSurface(node) {
    var current = node;
    for (var i = 0; current && i < 8; i += 1, current = current.parentElement) {
      if (/card|provider|oauth|section|content/i.test(String(current.className || ''))) return current;
    }
    return node.parentElement || node;
  }

  function makeModal() {
    var overlay = document.createElement('div');
    overlay.className = 'cpa-codex-auto-overlay';
    overlay.innerHTML = '<div class="cpa-codex-auto-modal" role="dialog" aria-modal="true">' +
      '<div class="cpa-codex-auto-title">Codex 自动 OAuth</div>' +
      '<div class="cpa-codex-auto-hint">每行一个：邮箱----密码----2FA。登录失败或需要额外验证的账号会单独标记。</div>' +
      '<textarea class="cpa-codex-auto-input" rows="8" autocomplete="off" spellcheck="false" placeholder="account@example.com----password----BASE32TOTP"></textarea>' +
      '<div class="cpa-codex-auto-actions"><button type="button" class="cpa-codex-auto-cancel">取消</button><button type="button" class="cpa-codex-auto-submit">开始自动 OAuth</button></div>' +
      '<div class="cpa-codex-auto-status" aria-live="polite"></div>' +
      '<div class="cpa-codex-auto-results"></div>' +
      '</div>';
    document.body.appendChild(overlay);
    return overlay;
  }

  function renderProgress(container, data) {
    var result = container.querySelector('.cpa-codex-auto-results');
    if (!container.__cpaCodexAutoRows) container.__cpaCodexAutoRows = new Map();
    (data.accounts || data.results || []).forEach(function (item) {
      var row = container.__cpaCodexAutoRows.get(item.email);
      if (!row) {
        row = document.createElement('div');
        row.className = 'cpa-codex-auto-result';
        var header = document.createElement('div');
        header.className = 'cpa-codex-auto-result-header';
        var email = document.createElement('span');
        email.className = 'cpa-codex-auto-result-email';
        header.appendChild(email);
        var state = document.createElement('span');
        state.className = 'cpa-codex-auto-result-state';
        header.appendChild(state);
        var track = document.createElement('div');
        track.className = 'cpa-codex-auto-progress-track';
        track.setAttribute('role', 'progressbar');
        track.setAttribute('aria-valuemin', '0');
        track.setAttribute('aria-valuemax', '100');
        track.setAttribute('aria-label', item.email + ' 登录进度');
        var fill = document.createElement('div');
        fill.className = 'cpa-codex-auto-progress-fill';
        track.appendChild(fill);
        var step = document.createElement('div');
        step.className = 'cpa-codex-auto-result-step';
        row.appendChild(header);
        row.appendChild(track);
        row.appendChild(step);
        result.appendChild(row);
        row.__email = email;
        row.__state = state;
        row.__fill = fill;
        row.__step = step;
        container.__cpaCodexAutoRows.set(item.email, row);
      }
      row.__email.textContent = item.email;
      var percent = Math.max(0, Math.min(100, Number(item.progress) || 0));
      row.__fill.style.width = percent + '%';
      row.__fill.parentElement.setAttribute('aria-valuenow', String(percent));
      row.__fill.style.backgroundColor = item.status === 'failed' ? '#cf222e' : (item.status === 'success' ? '#1a7f37' : '');
      row.__state.textContent = item.status === 'success' ? '认证成功' : (item.status === 'failed' ? '认证失败' : ((Number(item.progress) || 0) + '%'));
      row.__state.className = 'cpa-codex-auto-result-state ' + (item.status === 'success' ? 'success' : (item.status === 'failed' ? 'failure' : 'running'));
      row.__step.textContent = (item.step || '等待调度') + (item.error ? '：' + item.error : (item.workspace_name ? '，工作空间：' + item.workspace_name : ''));
      row.__step.style.whiteSpace = 'normal';
      row.__step.style.overflowWrap = 'anywhere';
      row.__fill.parentElement.setAttribute('aria-valuetext', row.__step.textContent);
    });
  }

  function pollJob(container, jobID, status) {
    if (!container.isConnected) return;
    managementRequest('GET', 'codex-auto-oauth/' + encodeURIComponent(jobID)).then(function (data) {
      renderProgress(container, data);
      if (data.status === 'completed') {
        status.textContent = '处理完成：成功 ' + (data.success || 0) + '/' + (data.total || 0);
        container.querySelector('.cpa-codex-auto-submit').disabled = false;
        return;
      }
      var completed = (data.accounts || []).filter(function (item) { return item.status === 'success' || item.status === 'failed'; }).length;
      status.textContent = '正在并发登录：已完成 ' + completed + '/' + (data.total || 0);
      window.setTimeout(function () { pollJob(container, jobID, status); }, 700);
    }).catch(function (error) {
      status.textContent = error.message || '状态查询失败';
      container.querySelector('.cpa-codex-auto-submit').disabled = false;
    });
  }

  function openModal() {
    var modal = makeModal();
    var input = modal.querySelector('.cpa-codex-auto-input');
    var status = modal.querySelector('.cpa-codex-auto-status');
    modal.querySelector('.cpa-codex-auto-cancel').onclick = function () { modal.remove(); };
    modal.addEventListener('click', function (event) { if (event.target === modal) modal.remove(); });
    modal.querySelector('.cpa-codex-auto-submit').onclick = function () {
      var raw = input.value;
      if (!raw.trim()) { status.textContent = '请输入账号'; return; }
      var submit = modal.querySelector('.cpa-codex-auto-submit');
      submit.disabled = true;
      status.textContent = '正在并发登录，请保持页面打开...';
      managementRequest('POST', 'codex-auto-oauth', { accounts: raw }).then(function (data) {
        input.value = '';
        modal.querySelector('.cpa-codex-auto-results').textContent = '';
        modal.__cpaCodexAutoRows = new Map();
        renderProgress(modal, data);
        if (data.job_id) {
          status.textContent = '任务已开始：正在并发登录...';
          pollJob(modal, data.job_id, status);
        } else {
          status.textContent = '处理完成：成功 ' + (data.success || 0) + '/' + (data.total || 0);
          submit.disabled = false;
        }
      }).catch(function (error) {
        status.textContent = error.message || '请求失败';
        submit.disabled = false;
      });
    };
    input.focus();
  }

  function mount(root) {
    var button = findLoginButton(root || document);
    if (!button || button.parentElement.querySelector('[data-cpa-codex-auto-oauth]')) return;
    var auto = document.createElement('button');
    auto.type = 'button';
    auto.textContent = '自动 OAuth';
    auto.setAttribute('data-cpa-codex-auto-oauth', 'true');
    auto.className = button.className;
    auto.style.marginRight = '8px';
    auto.addEventListener('click', openModal);
    button.parentElement.insertBefore(auto, button);
  }

  var style = document.createElement('style');
  style.textContent = '.cpa-codex-auto-overlay{position:fixed;inset:0;z-index:99999;background:rgba(0,0,0,.36);display:flex;align-items:center;justify-content:center;padding:20px}.cpa-codex-auto-modal{width:min(680px,100%);background:var(--surface-color,#fff);color:var(--text-primary,#1f2328);border:1px solid var(--border-color,#d0d7de);border-radius:8px;padding:20px;box-shadow:0 18px 60px rgba(0,0,0,.24)}.cpa-codex-auto-title{font-size:18px;font-weight:650;margin-bottom:8px}.cpa-codex-auto-hint{font-size:13px;line-height:1.5;margin-bottom:12px;color:var(--text-secondary,#57606a)}.cpa-codex-auto-input{width:100%;box-sizing:border-box;resize:vertical;min-height:150px;padding:10px;border:1px solid var(--border-color,#d0d7de);border-radius:6px;background:var(--input-bg,#fff);color:inherit;font:13px ui-monospace,SFMono-Regular,Consolas,monospace}.cpa-codex-auto-actions{display:flex;justify-content:flex-end;gap:8px;margin-top:12px}.cpa-codex-auto-actions button{border:0;border-radius:6px;padding:8px 14px;cursor:pointer}.cpa-codex-auto-submit{background:var(--accent,#0969da);color:#fff}.cpa-codex-auto-cancel{background:var(--button-secondary-bg,#f6f8fa);color:inherit}.cpa-codex-auto-status{margin-top:12px;font-size:13px}.cpa-codex-auto-result{padding:8px 0;font-size:13px}.cpa-codex-auto-result-header{display:flex;justify-content:space-between;gap:8px;min-width:0}.cpa-codex-auto-result-email{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.cpa-codex-auto-result-state{flex:0 0 auto}.cpa-codex-auto-result-state.success{color:#1a7f37}.cpa-codex-auto-result-state.failure{color:#cf222e}.cpa-codex-auto-result-state.running{color:var(--text-secondary,#57606a)}.cpa-codex-auto-progress-track{height:6px;margin-top:6px;background:var(--border-color,#d0d7de);border-radius:3px;overflow:hidden}.cpa-codex-auto-progress-fill{height:100%;width:0;background:var(--accent,#0969da);transition:width .25s ease}.cpa-codex-auto-result-state.success~.cpa-codex-auto-progress-track .cpa-codex-auto-progress-fill{background:#1a7f37}.cpa-codex-auto-result-step{margin-top:4px;color:var(--text-secondary,#57606a);font-size:12px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.cpa-codex-auto-results{max-height:300px;overflow:auto;margin-top:8px}';
  document.head.appendChild(style);

  mount(document);
  if (window.MutationObserver) new MutationObserver(function () { mount(document); }).observe(document.documentElement, { childList: true, subtree: true });
  setInterval(function () { mount(document); }, 1000);
})();
</script>
`

func AddCodexAutoOAuthToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(codexAutoOAuthMarker)) {
		return data, nil
	}
	lower := bytes.ToLower(data)
	index := bytes.LastIndex(lower, []byte("</body>"))
	if index < 0 {
		index = len(data)
	}
	return insertBytes(data, index, []byte(codexAutoOAuthScript)), nil
}
