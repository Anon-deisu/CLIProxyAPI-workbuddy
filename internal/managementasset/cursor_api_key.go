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
  var managementBase = managementPath;

  function observeManagementURL(url) {
    var parsed = new URL(url, window.location.href);
    var match = parsed.pathname.match(/\/v(?:0|8)\/management\//);
    if (!match) return false;
    managementBase = parsed.origin + parsed.pathname.slice(0, match.index) + managementPath;
    return true;
  }

  function captureAuthorization(value) {
    if (typeof value !== 'string') return;
    var match = value.match(/^\s*Bearer\s+(.+?)\s*$/i);
    if (match && match[1]) capturedManagementKey = match[1];
  }

  var nativeXHROpen = XMLHttpRequest.prototype.open;
  var nativeXHRSetRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cpaCursorManagementRequest = observeManagementURL(String(url || ''));
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
    if (observeManagementURL(String(url))) {
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
    var response = await nativeFetch.call(window, managementBase + path.slice(managementPath.length), options);
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

const legacyRuntimeSchedulerInlineMarker = `id="cpa-runtime-scheduler-script"`
const runtimeSchedulerInlineMarker = `id="cpa-runtime-scheduler-script-v2"`

const quotaProviderTabs = "(0,U.jsxs)(`div`,{className:`cpa-quota-provider-picker`,children:[" +
	"(0,U.jsx)(VA,{types:[`all`],counts:R,active:l,resolvedTheme:n,onChange:ie})," +
	"(0,U.jsx)(VA,{types:SN.filter(type=>type!==`all`),counts:R,active:l,resolvedTheme:n,onChange:ie})]})"

const runtimeSchedulerInlineScript = `
<script id="cpa-runtime-scheduler-script-v2">
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
  var accountTokenTooltip = null;
  var accountTokenAnchor = null;
  var accountTokenHideTimer = null;
  var accountTokenSignature = '';
  var resetTurnStateView = function () {};
  var renderSchedulerAccounts = function () {};
  var accountStatePending = new Set();
  var workBuddyTaskModal = null;
  var workBuddyTaskAccount = null;
  var workBuddyTaskPoll = null;
  var workBuddyTaskPending = false;
  var workBuddyTaskGeneration = 0;
  var workBuddyTaskData = null;
  var workBuddyTaskStates = new Map();
  var workBuddyTaskBadgePending = new Set();
  var nativeFetch = window.fetch;

  function clearManagementSession() {
    sessionGeneration += 1;
    managementBase = '';
    managementAuthorization = '';
    managementReady = false;
    authFilesRefreshPending = false;
    authFiles.clear();
    accountStatePending.clear();
    workBuddyTaskStates.clear();
    workBuddyTaskBadgePending.clear();
    closeWorkBuddyTasks();
    resetTurnStateView();
    renderQuotaCosts();
    renderAccountStateControls();
    renderModelTokenTooltips();
  }

  function observeManagementRequest(method, url, headers) {
    var parsed = new URL(url, window.location.href);
    var match = parsed.pathname.match(/\/v(?:0|8)\/management\//);
    if (!match) return null;
    var index = match.index;
    var authorization = headers.get('authorization');
    if (!authorization && headers.has('x-management-key')) authorization = 'Bearer ' + headers.get('x-management-key');
    if (!authorization) return null;
    var base = parsed.origin + parsed.pathname.slice(0, index) + managementPath;
    if (base !== managementBase || authorization !== managementAuthorization) {
      clearManagementSession();
      managementBase = base;
      managementAuthorization = authorization;
    }
    var endpoint = parsed.pathname.slice(index + match[0].length);
    var endpointAliases = {'credentials':'auth-files', 'credentials/status':'auth-files/status', 'routing/cooldown/reset':'reset-quota', 'requests/api-call':'api-call'};
    endpoint = endpointAliases[endpoint] || endpoint;
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
      renderModelTokenTooltips();
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
    var base = path.indexOf('workbuddy-tasks') === 0 ? managementBase.replace('/v0/management/', '/v8/management/credentials/') : managementBase;
    return window.fetch(base + path, options).then(function (response) {
      if (!response.ok) return response.json().then(function (data) {
        var error = new Error(data.error || ('http ' + response.status));
        error.applied = data.applied === true;
        error.runtimeError = data.runtime_error === true;
        throw error;
      });
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
    hideAccountTokenTooltip();
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

  function isWorkBuddyAccount(item) {
    var provider = String((item && (item.provider || item.type)) || '').toLowerCase();
    return provider === 'workbuddy' || provider === 'workbuddy-ai';
  }

  function workBuddyTaskLabel(state) {
    return ({checked:'签到成功', already:'今日已签到', completed:'已完成', claimed:'已领取', accepted:'进行中', manual:'需手动完成', pending:'待处理', not_accepted:'未接取', failed:'失败', unsupported:'不支持'})[state] || state || '待处理';
  }

  function workBuddyTaskIcon(name) {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('width', '16'); svg.setAttribute('height', '16');
    svg.setAttribute('fill', 'none'); svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '2'); svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round'); svg.setAttribute('aria-hidden', 'true');
    var path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', ({check:'M20 6 9 17l-5-5', x:'m18 6-12 12M6 6l12 12', refresh:'M20 7v5h-5M4 17v-5h5M6.1 7a7 7 0 0 1 11.55-2.61L20 7M4 17l2.35 2.61A7 7 0 0 0 17.9 17'})[name]);
    svg.appendChild(path);
    return svg;
  }

  function updateWorkBuddyTaskButton(button, item) {
    var status = workBuddyTaskStates.get(item.auth_index);
    var intl = (item.provider || item.type) === 'workbuddy-ai';
    var checked = !!(status && (intl ? status.activity.completed : status.checkin.checked_in));
    var value = checked ? 'checked' : 'unchecked';
    if (button.getAttribute('data-checkin-state') === value) return;
    button.setAttribute('data-checkin-state', value);
    button.textContent = '';
    button.appendChild(make('span', '', '任务'));
    button.appendChild(workBuddyTaskIcon(checked ? 'check' : 'x'));
    button.title = status && status.error ? '签到状态读取失败' : (intl ? (checked ? '今日活跃已完成' : '今日活跃未完成') : (checked ? '今日已签到' : '今日未签到'));
    button.setAttribute('aria-label', '任务，' + button.title);
  }

  function rememberWorkBuddyTaskStatus(data) {
    if (!data.auth_index) return;
    workBuddyTaskStates.set(data.auth_index, data);
    document.querySelectorAll('[data-cpa-workbuddy-tasks]').forEach(function (button) {
      if (button.account && button.account.auth_index === data.auth_index) updateWorkBuddyTaskButton(button, button.account);
    });
  }

  function loadWorkBuddyTaskBadge(item) {
    if (workBuddyTaskBadgePending.has(item.auth_index) || !managementReady) return;
    var generation = sessionGeneration;
    workBuddyTaskBadgePending.add(item.auth_index);
    return request('GET', 'workbuddy-tasks?auth_index=' + encodeURIComponent(item.auth_index) + '&refresh=false').then(function (response) {
      return response.json();
    }).then(function (data) {
      if (generation === sessionGeneration) rememberWorkBuddyTaskStatus(data);
    }).catch(function (err) {
      if (generation === sessionGeneration) rememberWorkBuddyTaskStatus({auth_index:item.auth_index,checkin:{},activity:{},error:err.message});
    }).finally(function () {
      if (generation === sessionGeneration) workBuddyTaskBadgePending.delete(item.auth_index);
    });
  }

  function workBuddyTaskStateClass(ok, status) {
    if (ok || status === 'completed' || status === 'claimed' || status === 'checked' || status === 'already') return 'success';
    if (status === 'failed') return 'failed';
    return 'pending';
  }

  function formatTaskNumber(value) {
    return Number(value || 0).toLocaleString('zh-CN', {maximumFractionDigits:2});
  }

  function closeWorkBuddyTasks() {
    if (workBuddyTaskPoll) clearInterval(workBuddyTaskPoll);
    workBuddyTaskPoll = null;
    workBuddyTaskAccount = null;
    workBuddyTaskData = null;
    workBuddyTaskGeneration += 1;
    workBuddyTaskPending = false;
    if (workBuddyTaskModal) workBuddyTaskModal.hidden = true;
  }

  function appendTaskResultRow(list, result) {
    var row = make('div', 'cpa-workbuddy-task-result');
    var heading = make('div', 'cpa-workbuddy-task-result-title');
    heading.appendChild(make('strong', '', result.name || result.code));
    heading.appendChild(make('span', 'cpa-workbuddy-task-badge ' + workBuddyTaskStateClass(result.ok, result.status), workBuddyTaskLabel(result.status)));
    row.appendChild(heading);
    var detail = result.message || '';
    if (result.target > 0) detail = '进度 ' + result.current + '/' + result.target + (detail ? '；' : '') + detail;
    if (result.reward_credit) detail += (detail ? '；' : '') + '到账 +' + formatTaskNumber(result.reward_credit) + ' 积分';
    row.appendChild(make('div', 'cpa-workbuddy-task-result-message', detail));
    list.appendChild(row);
  }

  function renderWorkBuddyTasks(data) {
    if (!workBuddyTaskModal || !workBuddyTaskAccount) return;
    var body = workBuddyTaskModal.querySelector('.cpa-workbuddy-task-body');
    body.textContent = '';
    if (data.error && !data.checkin) {
      body.appendChild(make('div', 'cpa-workbuddy-task-error', data.error));
      return;
    }
    workBuddyTaskData = data;
    rememberWorkBuddyTaskStatus(data);
    var summary = make('div', 'cpa-workbuddy-task-summary');
    var checkin = data.checkin || {};
    var activity = data.activity || {};
    var growth = data.growth || {};
    summary.appendChild(make('span', 'cpa-workbuddy-task-badge', data.realm === 'intl' ? '国际版' : '国内版'));
    summary.appendChild(make('span', '', data.running ? '执行中' : '状态已更新'));
    if (data.checked_at) summary.appendChild(make('span', '', new Date(data.checked_at).toLocaleString()));
    body.appendChild(summary);
    var overview = make('div', 'cpa-workbuddy-task-overviews');
    var checkinSection = make('section', 'cpa-workbuddy-task-section');
    checkinSection.appendChild(make('h3', '', '每日签到'));
    if (!checkin.supported) {
      checkinSection.appendChild(make('div', 'cpa-workbuddy-task-muted', checkin.message || (data.realm === 'intl' ? '国际版无签到活动' : '当前账号不支持签到')));
    } else {
      var checkinRow = make('div', 'cpa-workbuddy-task-overview');
      checkinRow.appendChild(make('strong', '', checkin.checked_in ? '今日已签到' : '今日未签到'));
      checkinRow.appendChild(make('span', '', '连续 ' + (checkin.streak_days || 0) + ' 天 · 今日 +' + formatTaskNumber(checkin.today_credit) + ' 积分'));
      if (checkin.message && !checkin.checked_in) checkinRow.appendChild(make('span', 'cpa-workbuddy-task-error', checkin.message));
      checkinSection.appendChild(checkinRow);
    }
    overview.appendChild(checkinSection);
    var activitySection = make('section', 'cpa-workbuddy-task-section');
    activitySection.appendChild(make('h3', '', data.realm === 'intl' ? '每日活跃任务' : '对话活跃'));
    var activityRow = make('div', 'cpa-workbuddy-task-overview');
    activityRow.appendChild(make('strong', '', activity.completed ? '今日已完成' : '今日未完成'));
    activityRow.appendChild(make('span', '', '连续打卡 ' + (activity.streak_days || 0) + ' 天'));
    if (activity.message) activityRow.appendChild(make('span', '', activity.message));
    activitySection.appendChild(activityRow);
    overview.appendChild(activitySection);
    body.appendChild(overview);
    var growthSection = make('section', 'cpa-workbuddy-task-section');
    var growthTitle = make('h3', '', '成长任务');
    growthSection.appendChild(growthTitle);
    if (growth.supported && growth.total > 0) {
      growthTitle.appendChild(make('span', 'cpa-workbuddy-task-count', growth.completed + '/' + growth.total + ' 已领奖'));
    }
    if (growth.message && !(growth.tasks || []).length) growthSection.appendChild(make('div', 'cpa-workbuddy-task-muted', growth.message));
    var tasks = growth.tasks || [];
    if (!tasks.length && growth.supported && !growth.message) growthSection.appendChild(make('div', 'cpa-workbuddy-task-muted', '暂无成长任务'));
    tasks.forEach(function (task) {
      var row = make('div', 'cpa-workbuddy-task-row');
      var head = make('div', 'cpa-workbuddy-task-row-head');
      head.appendChild(make('strong', '', task.name || task.code));
      head.appendChild(make('span', 'cpa-workbuddy-task-badge ' + workBuddyTaskStateClass(task.status === 'claimed', task.status), workBuddyTaskLabel(task.status)));
      row.appendChild(head);
      var progress = make('div', 'cpa-workbuddy-task-progress');
      progress.appendChild(make('span', '', '进度 ' + (task.current || 0) + '/' + (task.target || 1)));
      progress.appendChild(make('span', '', '奖励 ' + formatTaskNumber(task.reward_credit) + ' 积分' + (task.reward_energy ? ' + ' + formatTaskNumber(task.reward_energy) + ' 能量' : '')));
      row.appendChild(progress);
      var track = make('div', 'cpa-workbuddy-task-track');
      var fill = make('div', 'cpa-workbuddy-task-track-fill');
      fill.style.width = Math.min(100, Math.max(0, (task.current || 0) * 100 / (task.target || 1))) + '%';
      track.appendChild(fill); row.appendChild(track);
      if (task.manual_reason) row.appendChild(make('div', 'cpa-workbuddy-task-muted', task.manual_reason));
      growthSection.appendChild(row);
    });
    body.appendChild(growthSection);
    if (data.last_run) {
      var run = data.last_run;
      var resultSection = make('section', 'cpa-workbuddy-task-section');
      var runTitle = make('h3', '', '最近执行结果');
      runTitle.appendChild(make('span', 'cpa-workbuddy-task-count', (run.trigger === 'auto' ? '自动' : '手动') + ' · 新增 +' + formatTaskNumber(run.earned_credit) + ' 积分'));
      resultSection.appendChild(runTitle);
      resultSection.appendChild(make('div', 'cpa-workbuddy-task-muted', (run.ok ? '执行成功' : '存在未完成项') + ' · ' + new Date(run.started_at).toLocaleString() + (run.message ? ' · ' + run.message : '')));
      var list = make('div', 'cpa-workbuddy-task-results');
      (run.results || []).forEach(function (result) { appendTaskResultRow(list, result); });
      resultSection.appendChild(list);
      body.appendChild(resultSection);
    }
    var actions = workBuddyTaskModal.querySelector('.cpa-workbuddy-task-actions');
    actions.querySelectorAll('button').forEach(function (button) {
      var action = button.getAttribute('data-cpa-task-action');
      button.disabled = !!data.running || (action === 'checkin' && (!checkin.supported || checkin.checked_in)) || (action === 'growth' && !growth.supported && (!activity.supported || activity.completed));
      if (action === 'checkin') button.textContent = checkin.checked_in ? '今日已签到' : '立即签到';
      if (action === 'growth') button.textContent = data.realm === 'intl' ? '完成每日活跃' : '完成成长任务';
    });
  }

  function loadWorkBuddyTasks(refresh) {
    if (!workBuddyTaskAccount || workBuddyTaskPending) return Promise.resolve();
    workBuddyTaskPending = true;
    var generation = workBuddyTaskGeneration;
    return request('GET', 'workbuddy-tasks?auth_index=' + encodeURIComponent(workBuddyTaskAccount.auth_index) + (refresh === false ? '&refresh=false' : '')).then(function (response) {
      return response.json().then(function (data) { return {ok:response.ok, data:data}; });
    }).then(function (result) {
      if (generation !== workBuddyTaskGeneration || !workBuddyTaskAccount) return;
      renderWorkBuddyTasks(result.ok ? result.data : {error:result.data.error || '任务状态读取失败'});
    }).catch(function (err) {
      if (generation === workBuddyTaskGeneration && workBuddyTaskAccount) renderWorkBuddyTasks({error:err.message});
    }).finally(function () { if (generation === workBuddyTaskGeneration) workBuddyTaskPending = false; });
  }

  function runWorkBuddyTasks(checkin, growth) {
    if (!workBuddyTaskAccount || workBuddyTaskPending) return;
    if (checkin && workBuddyTaskData && workBuddyTaskData.checkin.checked_in) return;
    workBuddyTaskPending = true;
    workBuddyTaskModal.querySelector('.cpa-workbuddy-task-actions').querySelectorAll('button').forEach(function (button) { button.disabled = true; });
    var generation = workBuddyTaskGeneration;
    workBuddyTaskModal.querySelector('.cpa-workbuddy-task-error-line').textContent = '';
    return request('POST', 'workbuddy-tasks/run', {auth_index:workBuddyTaskAccount.auth_index, checkin:checkin, growth:growth}).then(function (response) {
      return response.json();
    }).then(function (result) {
      if (generation !== workBuddyTaskGeneration) return;
      if (result.account) renderWorkBuddyTasks(result.account);
      workBuddyTaskPending = false;
      return loadWorkBuddyTasks(true);
    }).catch(function (err) {
      if (generation === workBuddyTaskGeneration) workBuddyTaskModal.querySelector('.cpa-workbuddy-task-error-line').textContent = err.message;
    }).finally(function () { if (generation === workBuddyTaskGeneration) workBuddyTaskPending = false; });
  }

  function openWorkBuddyTasks(item) {
    closeWorkBuddyTasks();
    if (!workBuddyTaskModal) {
      var backdrop = make('div', 'modal-overlay cpa-workbuddy-task-backdrop');
      backdrop.hidden = true;
      backdrop.setAttribute('role', 'dialog');
      backdrop.setAttribute('aria-modal', 'true');
      backdrop.setAttribute('aria-label', 'WorkBuddy 账号任务');
      var dialog = make('div', 'modal cpa-workbuddy-task-dialog');
      var header = make('div', 'modal-header cpa-workbuddy-task-header');
      var heading = make('div', 'cpa-workbuddy-task-heading');
      var title = make('h2', 'modal-title', 'WorkBuddy 任务');
      var subtitle = make('div', 'cpa-workbuddy-task-subtitle');
      heading.appendChild(title); heading.appendChild(subtitle);
      var close = make('button', 'modal-close-floating cpa-workbuddy-task-close');
      close.appendChild(workBuddyTaskIcon('x'));
      close.type = 'button'; close.title = '关闭任务'; close.setAttribute('aria-label', '关闭任务');
      close.addEventListener('click', closeWorkBuddyTasks);
      header.appendChild(heading); header.appendChild(close);
      var body = make('div', 'modal-body cpa-workbuddy-task-body');
      var errorLine = make('div', 'cpa-workbuddy-task-error-line');
      var actions = make('div', 'modal-footer cpa-workbuddy-task-actions');
      [['立即签到', true, false], ['完成成长任务', false, true]].forEach(function (spec) {
        var button = make('button', 'btn ' + (spec[1] ? 'btn-primary' : 'btn-secondary') + ' cpa-workbuddy-task-action', spec[0]);
        button.type = 'button';
        button.setAttribute('data-cpa-task-action', spec[1] ? 'checkin' : 'growth');
        button.addEventListener('click', function () { runWorkBuddyTasks(spec[1], spec[2]); });
        actions.appendChild(button);
      });
      var refresh = make('button', 'btn btn-secondary cpa-workbuddy-task-refresh');
      refresh.appendChild(workBuddyTaskIcon('refresh'));
      refresh.title = '刷新状态'; refresh.setAttribute('aria-label', '刷新状态');
      refresh.type = 'button'; refresh.addEventListener('click', function () { loadWorkBuddyTasks(true); });
      actions.appendChild(refresh);
      dialog.appendChild(header); dialog.appendChild(body); dialog.appendChild(errorLine); dialog.appendChild(actions);
      backdrop.appendChild(dialog);
      backdrop.addEventListener('click', function (event) { if (event.target === backdrop) closeWorkBuddyTasks(); });
      document.body.appendChild(backdrop);
      workBuddyTaskModal = backdrop;
    }
    workBuddyTaskAccount = item;
    workBuddyTaskModal.querySelector('.cpa-workbuddy-task-heading h2').textContent = 'WorkBuddy 任务';
    workBuddyTaskModal.querySelector('.cpa-workbuddy-task-subtitle').textContent = item.email || item.nickname || item.name;
    workBuddyTaskModal.querySelector('.cpa-workbuddy-task-error-line').textContent = '';
    workBuddyTaskModal.querySelector('.cpa-workbuddy-task-body').textContent = '正在读取任务状态...';
    workBuddyTaskModal.hidden = false;
    workBuddyTaskModal.querySelector('.cpa-workbuddy-task-actions').querySelectorAll('button').forEach(function (button) { button.disabled = true; });
    loadWorkBuddyTasks(true);
    if (workBuddyTaskPoll) clearInterval(workBuddyTaskPoll);
    workBuddyTaskPoll = setInterval(function () { loadWorkBuddyTasks(false); }, 2000);
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
    group.setAttribute('data-cpa-auth-state', item.name);
    var state = accountSchedulingState(item);
    group.querySelectorAll('button').forEach(function (button) {
      button.setAttribute('aria-pressed', String(button.value === state));
      button.disabled = !managementReady || accountStatePending.has(item.id || item.name);
    });
  }

  function findAuthFileForCard(card, nameNode) {
    var identity = card.getAttribute('data-cpa-auth-name');
    var candidates = [identity];
    if (nameNode) candidates.push(nameNode.getAttribute('title'), (nameNode.textContent || '').trim());
    candidates = candidates.filter(function (value, index, values) { return value && values.indexOf(value) === index; });
    for (var i = 0; i < candidates.length; i += 1) {
      var exact = authFiles.get(candidates[i]);
      if (exact) return exact;
    }
    for (var j = 0; j < candidates.length; j += 1) {
      var aliasMatches = [];
      authFiles.forEach(function (item) {
        if (!item) return;
        var aliases = [item.id, item.name, item.label, item.email, item.nickname];
        if (aliases.indexOf(candidates[j]) >= 0) aliasMatches.push(item);
      });
      if (aliasMatches.length === 1) return aliasMatches[0];
    }
    return null;
  }

  function renderAccountStateControls() {
    document.querySelectorAll('[class*="AuthFileCard-module__card___"]').forEach(function (card) {
      var nameNode = card.querySelector('[class*="AuthFileCard-module__fileName"]');
      var nativeToggle = card.querySelector('[class*="AuthFileCard-module__toggleWrap"]');
      if (!nativeToggle) return;
      var item = findAuthFileForCard(card, nameNode);
      var control = card.querySelector('[data-cpa-auth-state]');
      var taskButton = card.querySelector('[data-cpa-workbuddy-tasks]');
      if (!item || item.runtime_only) {
        if (control) control.remove();
        if (taskButton) taskButton.remove();
        nativeToggle.classList.remove('cpa-native-status-hidden');
        card.classList.remove('cpa-account-managed');
        card.removeAttribute('data-cpa-managed-account');
        card.removeAttribute('data-cpa-scheduling-state');
        return;
      }
      if (!control) {
        control = makeAccountStateControl(item, refreshAuthFiles);
        nativeToggle.insertAdjacentElement('afterend', control);
      }
      if (isWorkBuddyAccount(item)) {
        if (!taskButton) {
          taskButton = make('button', 'btn btn-secondary btn-sm cpa-workbuddy-task-button');
          taskButton.type = 'button';
          taskButton.setAttribute('data-cpa-workbuddy-tasks', 'true');
          taskButton.addEventListener('click', function (event) {
            event.stopPropagation();
            openWorkBuddyTasks(taskButton.account);
          });
          control.insertAdjacentElement('beforebegin', taskButton);
        }
        taskButton.account = item;
        taskButton.disabled = !managementReady;
        updateWorkBuddyTaskButton(taskButton, item);
        if (!workBuddyTaskStates.has(item.auth_index)) loadWorkBuddyTaskBadge(item);
      } else if (taskButton) {
        taskButton.remove();
      }
      nativeToggle.classList.add('cpa-native-status-hidden');
      card.classList.add('cpa-account-managed');
      card.setAttribute('data-cpa-managed-account', 'true');
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

  function renderModelTokenTooltips() {
    var cards = document.querySelectorAll('article[class*="AuthFileCard-module__card___"]');
    for (var i = 0; i < cards.length; i += 1) {
      var card = cards[i];
      var counts = card.querySelector('[class*="AuthFileCard-module__healthCounts"]');
      if (!counts) continue;
      counts.__cpaTokenFile = card.getAttribute('data-cpa-auth-name');
      counts.querySelectorAll('span[title]').forEach(function (span) { span.removeAttribute('title'); });
      if (counts.__cpaTokenBound) continue;
      counts.__cpaTokenBound = true;
      counts.tabIndex = 0;
      counts.setAttribute('aria-describedby', 'cpa-account-token-tooltip');
      counts.addEventListener('pointerenter', function (event) {
        if (event.pointerType === 'mouse') showAccountTokenTooltip(event.currentTarget);
      });
      counts.addEventListener('pointerleave', scheduleAccountTokenHide);
      counts.addEventListener('focus', function (event) { showAccountTokenTooltip(event.currentTarget); });
      counts.addEventListener('blur', scheduleAccountTokenHide);
      counts.addEventListener('pointerdown', function (event) {
        if (event.pointerType !== 'touch') return;
        if (accountTokenAnchor === event.currentTarget) hideAccountTokenTooltip();
        else showAccountTokenTooltip(event.currentTarget);
      });
    }
    if (accountTokenAnchor) showAccountTokenTooltip(accountTokenAnchor);
  }

  function scheduleAccountTokenHide() {
    clearTimeout(accountTokenHideTimer);
    accountTokenHideTimer = setTimeout(hideAccountTokenTooltip, 150);
  }

  function hideAccountTokenTooltip() {
    clearTimeout(accountTokenHideTimer);
    if (accountTokenTooltip) accountTokenTooltip.hidden = true;
    accountTokenAnchor = null;
  }

  function showAccountTokenTooltip(anchor) {
    var item = authFiles.get(anchor.__cpaTokenFile);
    if (!anchor.isConnected || !item) {
      hideAccountTokenTooltip();
      return;
    }
    clearTimeout(accountTokenHideTimer);
    accountTokenAnchor = anchor;
    if (!accountTokenTooltip) {
      accountTokenTooltip = make('div', 'cpa-account-token-tooltip');
      accountTokenTooltip.id = 'cpa-account-token-tooltip';
      accountTokenTooltip.setAttribute('data-cpa-model-token-tooltip', 'true');
      accountTokenTooltip.setAttribute('role', 'tooltip');
      accountTokenTooltip.setAttribute('aria-label', '账号模型 Token 统计');
      accountTokenTooltip.tabIndex = 0;
      accountTokenTooltip.addEventListener('pointerenter', function () { clearTimeout(accountTokenHideTimer); });
      accountTokenTooltip.addEventListener('pointerleave', scheduleAccountTokenHide);
      document.body.appendChild(accountTokenTooltip);
    }
     var tokenUsage = (item.model_token_usage || []).slice().sort(function (a, b) {
       var difference = (Number(b.input_tokens) || 0) - (Number(a.input_tokens) || 0);
       return difference || String(a.model || '').localeCompare(String(b.model || ''));
     });
    var signature = item.name + JSON.stringify(tokenUsage);
    if (accountTokenSignature !== signature) {
      accountTokenSignature = signature;
      accountTokenTooltip.textContent = '';
      accountTokenTooltip.appendChild(make('div', 'cpa-account-token-title', '模型 Token 统计'));
      if (!tokenUsage.length) {
        accountTokenTooltip.appendChild(make('div', 'cpa-account-token-empty', '暂无模型 token 统计'));
      } else {
        var table = make('table', 'cpa-account-token-table');
        var head = table.createTHead().insertRow();
        ['模型 ID', '总输入', '缓存输入', '缓存率', '输出'].forEach(function (label) {
          var cell = make('th', '', label);
          cell.scope = 'col';
          head.appendChild(cell);
        });
        var body = table.createTBody();
        var formatter = new Intl.NumberFormat();
        tokenUsage.forEach(function (model) {
          var row = body.insertRow();
          var modelCell = row.insertCell();
          modelCell.className = 'cpa-account-token-model';
          modelCell.textContent = model.model;
          row.insertCell().textContent = formatter.format(model.input_tokens);
          row.insertCell().textContent = formatter.format(model.cached_input_tokens);
          row.insertCell().textContent = model.input_tokens > 0 ? (model.cached_input_tokens * 100 / model.input_tokens).toFixed(1) + '%' : '--';
          row.insertCell().textContent = formatter.format(model.output_tokens);
        });
        accountTokenTooltip.appendChild(table);
      }
    }
    accountTokenTooltip.hidden = false;
    positionAccountTokenTooltips();
  }

  function positionModelTooltips() {
    document.querySelectorAll(modelTooltipSelector).forEach(function (tooltip) {
      tooltip.style.translate = 'none';
      var margin = 8;
      var clip = tooltip.closest('.page-transition').getBoundingClientRect();
      var left = Math.max(margin, clip.left + margin);
      var right = Math.min(window.innerWidth - margin, clip.right - margin);
      tooltip.style.setProperty('--cpa-model-tooltip-width', (right - left) + 'px');
      var bounds = tooltip.getBoundingClientRect();
      var shift = Math.max(left - bounds.left, Math.min(0, right - bounds.right));
      tooltip.style.translate = shift + 'px 0';
    });
  }

  function positionAccountTokenTooltips() {
    if (!accountTokenTooltip || accountTokenTooltip.hidden || !accountTokenAnchor) return;
    if (!accountTokenAnchor.isConnected) { hideAccountTokenTooltip(); return; }
    var clipElement = accountTokenAnchor.closest('.page-transition');
    var clip = clipElement ? clipElement.getBoundingClientRect() : {left:0,right:window.innerWidth};
    var left = Math.max(8, clip.left + 8);
    var right = Math.min(window.innerWidth - 8, clip.right - 8);
    accountTokenTooltip.style.maxWidth = Math.max(0, right-left) + 'px';
    var anchor = accountTokenAnchor.getBoundingClientRect();
    var box = accountTokenTooltip.getBoundingClientRect();
    accountTokenTooltip.style.left = Math.max(left, Math.min(anchor.right-box.width, right-box.width)) + 'px';
    var top = anchor.top-box.height-8;
    if (top < 8) top = anchor.bottom+8;
    accountTokenTooltip.style.top = Math.max(8, Math.min(top, window.innerHeight-box.height-8)) + 'px';
  }

  function mount() {
    if (document.getElementById('cpa-runtime-scheduler-panel')) return;
    var toggle = make('button', 'cpa-runtime-scheduler-toggle', '账号调度');
    toggle.type = 'button';
    var panel = make('section', 'cpa-runtime-scheduler-panel');
    panel.id = 'cpa-runtime-scheduler-panel';
    panel.hidden = true;
    var title = make('div', 'cpa-runtime-scheduler-title', '账号调度设置');
    var hint = make('div', 'cpa-runtime-scheduler-hint', '账号状态会保存；调度配置可选持久化');
    var form = make('div', 'cpa-runtime-scheduler-form');
    var persistLabel = make('label', 'cpa-runtime-scheduler-check');
    var persistSettings = document.createElement('input');
    persistSettings.type = 'checkbox';
    persistLabel.appendChild(persistSettings);
    persistLabel.appendChild(document.createTextNode('写入配置文件'));
    var strategyLabel = make('div', 'cpa-runtime-scheduler-default', '调度方式：Codex2API 默认（额度加权均衡）');
    var degradedPortLabel = make('label', 'cpa-runtime-scheduler-number');
    degradedPortLabel.appendChild(document.createTextNode('降级账号 API 端口'));
    var degradedPort = document.createElement('input');
    degradedPort.type = 'number';
    degradedPort.min = '0';
    degradedPort.max = '65535';
    degradedPort.step = '1';
    degradedPort.title = '0 表示关闭；与主端口共用 API Key';
    degradedPortLabel.appendChild(degradedPort);
    var exoFreePortLabel = make('label', 'cpa-runtime-scheduler-number');
    exoFreePortLabel.appendChild(document.createTextNode('OpenCode Free API 端口'));
    var exoFreePort = document.createElement('input');
    exoFreePort.type = 'number';
    exoFreePort.min = '0';
    exoFreePort.max = '65535';
    exoFreePort.step = '1';
    exoFreePort.title = '0 表示关闭；OpenCode 免费模型，共用 API Key，支持流式 Chat Completions 和 Responses';
    exoFreePortLabel.appendChild(exoFreePort);
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
    var workBuddyFingerprintLabel = make('label', 'cpa-runtime-scheduler-check');
    var workBuddyFingerprintConvergence = document.createElement('input');
    workBuddyFingerprintConvergence.type = 'checkbox';
    workBuddyFingerprintConvergence.title = 'WorkBuddy 国内站与国际站按账号固定身份，刷新 token 后保持一致';
    workBuddyFingerprintLabel.appendChild(workBuddyFingerprintConvergence);
    workBuddyFingerprintLabel.appendChild(document.createTextNode('WorkBuddy 账号指纹收敛'));
    var workBuddyCheckinLabel = make('label', 'cpa-runtime-scheduler-check');
    var workBuddyAutoCheckin = document.createElement('input');
    workBuddyAutoCheckin.type = 'checkbox';
    workBuddyAutoCheckin.title = '仅国内版个人 OAuth 账号；开启后立即巡检并定期补跑';
    workBuddyCheckinLabel.appendChild(workBuddyAutoCheckin);
    workBuddyCheckinLabel.appendChild(document.createTextNode('WorkBuddy 自动签到'));
    var workBuddyCheckinRow = make('div', 'cpa-runtime-scheduler-checkin-row');
    workBuddyCheckinRow.appendChild(workBuddyCheckinLabel);
    var workBuddyIntervalLabel = make('label', 'cpa-runtime-scheduler-checkin-interval');
    var workBuddyCheckinInterval = document.createElement('input');
    workBuddyCheckinInterval.type = 'number';
    workBuddyCheckinInterval.min = '1'; workBuddyCheckinInterval.max = '86400'; workBuddyCheckinInterval.step = '1';
    workBuddyCheckinInterval.required = true;
    workBuddyCheckinInterval.setAttribute('aria-label', '自动签到账号间隔（秒）');
    workBuddyCheckinInterval.title = '自动签到账号间隔（秒）';
    workBuddyIntervalLabel.appendChild(workBuddyCheckinInterval);
    workBuddyIntervalLabel.appendChild(make('span', '', '秒'));
    workBuddyCheckinRow.appendChild(workBuddyIntervalLabel);
    workBuddyAutoCheckin.addEventListener('change', function () { workBuddyCheckinInterval.disabled = !workBuddyAutoCheckin.checked; });
    var workBuddyTasksLabel = make('label', 'cpa-runtime-scheduler-check');
    var workBuddyAutoTasks = document.createElement('input');
    workBuddyAutoTasks.type = 'checkbox';
    workBuddyAutoTasks.title = '国内版执行支持的成长任务，国内/国际版完成每日活跃';
    workBuddyTasksLabel.appendChild(workBuddyAutoTasks);
    workBuddyTasksLabel.appendChild(document.createTextNode('WorkBuddy 自动完成任务'));
    var staticISPLabel = make('label', 'cpa-runtime-scheduler-check');
    var staticISPEnabled = document.createElement('input');
    staticISPEnabled.type = 'checkbox';
    staticISPEnabled.id = 'cpa-runtime-scheduler-static-isp-enabled';
    staticISPLabel.appendChild(staticISPEnabled);
    staticISPLabel.appendChild(document.createTextNode('Cliproxy 静态 ISP 固定出口'));
    var staticISPFields = make('div', 'cpa-runtime-scheduler-proxy-fields');
    var staticISPList = make('div', 'cpa-static-isp-list');
    var staticISPRows = [];
    var addStaticISP = make('button', 'cpa-static-isp-add', '添加 ISP 节点');
    addStaticISP.type = 'button';
    addStaticISP.id = 'cpa-static-isp-add';
    staticISPFields.appendChild(staticISPList);
    staticISPFields.appendChild(addStaticISP);
    function updateStaticISPInputs() {
      staticISPFields.hidden = !staticISPEnabled.checked && staticISPRows.length === 0;
    }
    function addStaticISPRow(node) {
      var row = make('div', 'cpa-static-isp-node');
      var fields = {};
      var configured = !!node['password-configured'];
      [['id', '节点 ID', 'text'], ['scheme', '协议', 'select'], ['host', 'IP / 主机名', 'text'], ['port', '端口', 'number'], ['username', '用户名', 'text'], ['password', '密码', 'password']].forEach(function (spec) {
        var label = make('label', 'cpa-runtime-scheduler-text', spec[1]);
        var input = document.createElement(spec[2] === 'select' ? 'select' : 'input');
        input.setAttribute('data-isp-field', spec[0]);
        if (spec[2] === 'select') {
          ['socks5', 'socks5h', 'http', 'https'].forEach(function (scheme) {
            var option = make('option', '', scheme.toUpperCase());
            option.value = scheme;
            input.appendChild(option);
          });
          input.value = node.scheme || 'socks5';
        } else {
          input.type = spec[2];
          input.autocomplete = spec[2] === 'password' ? 'new-password' : 'off';
          input.value = spec[0] === 'password' ? '' : (node[spec[0]] || '');
          input.required = spec[0] !== 'password' || !configured;
          if (spec[0] === 'password') input.placeholder = configured ? '已设置，留空保留' : '代理密码';
          if (spec[0] === 'id') input.readOnly = configured;
          if (spec[0] === 'port') { input.min = '1'; input.max = '65535'; input.step = '1'; }
        }
        fields[spec[0]] = input;
        label.appendChild(input);
        row.appendChild(label);
      });
      var remove = make('button', 'cpa-static-isp-remove', '×');
      remove.type = 'button';
      remove.title = '移除 ISP 节点';
      remove.setAttribute('aria-label', '移除 ISP 节点');
      row.appendChild(remove);
      var view = {element:row, fields:fields};
      staticISPRows.push(view);
      staticISPList.appendChild(row);
      remove.addEventListener('click', function () {
        fields.password.value = '';
        staticISPRows = staticISPRows.filter(function (item) { return item !== view; });
        row.remove();
        updateStaticISPInputs();
      });
      updateStaticISPInputs();
      return view;
    }
    function loadStaticISPNodes(nodes) {
      staticISPRows.forEach(function (row) { row.fields.password.value = ''; });
      staticISPRows = [];
      staticISPList.textContent = '';
      nodes.forEach(addStaticISPRow);
      updateStaticISPInputs();
    }
    function clearSubmittedISPPasswords() {
      staticISPRows.forEach(function (row) {
        if (row.fields.password.value !== '') {
          row.fields.password.value = '';
          row.fields.password.required = false;
          row.fields.password.placeholder = '已设置，留空保留';
          row.fields.id.readOnly = true;
        }
      });
    }
    addStaticISP.addEventListener('click', function () {
      var index = 1;
      while (staticISPRows.some(function (row) { return row.fields.id.value === 'isp-' + index; })) index++;
      addStaticISPRow({id:'isp-' + index}).fields.host.focus();
    });
    staticISPEnabled.addEventListener('change', updateStaticISPInputs);
    updateStaticISPInputs();
    var fakeModelsSection = make('details', 'cpa-runtime-scheduler-section');
    fakeModelsSection.id = 'cpa-fake-models-section';
    fakeModelsSection.appendChild(make('summary', 'cpa-runtime-scheduler-section-title', '假模型'));
    var fakeModelsContent = make('div', 'cpa-runtime-scheduler-section-content');
    var fakeModelsList = make('div', 'cpa-fake-model-list');
    var fakeModelRows = [];
    var addFakeModel = make('button', 'cpa-static-isp-add', '添加假模型');
    addFakeModel.type = 'button';
    addFakeModel.id = 'cpa-fake-model-add';
    function addFakeModelRow(model) {
      var row = make('div', 'cpa-fake-model-row');
      var details = document.createElement('details');
      details.className = 'cpa-fake-model-details';
      var summary = document.createElement('summary');
      summary.className = 'cpa-fake-model-summary';
      summary.textContent = model.name || '未命名假模型';
      details.appendChild(summary);
      var editor = make('div', 'cpa-fake-model-editor');
      details.appendChild(editor);
      var fields = {};
      var responseRows = [];
      var responseList = make('div', 'cpa-fake-response-list');
      var responses = Array.isArray(model.responses) ? model.responses.slice() : (model.content ? [model.content] : ['']);
      function syncSummary() { summary.textContent = fields.name && fields.name.value.trim() ? fields.name.value.trim() : '未命名假模型'; }
      function addFakeResponse(value) {
        var responseRow = make('div', 'cpa-fake-response-row');
        var input = document.createElement('textarea');
        input.setAttribute('data-fake-response', 'true');
        input.value = value == null ? '' : String(value);
        input.required = true;
        input.setAttribute('aria-label', '固定回复');
        var removeResponse = make('button', 'cpa-static-isp-remove', '×');
        removeResponse.type = 'button';
        removeResponse.title = '移除固定回复';
        removeResponse.setAttribute('aria-label', '移除固定回复');
        responseRow.appendChild(input);
        responseRow.appendChild(removeResponse);
        var view = {element: responseRow, input: input};
        responseRows.push(view);
        responseList.appendChild(responseRow);
        removeResponse.addEventListener('click', function () {
          responseRows.splice(responseRows.indexOf(view), 1);
          responseRow.remove();
        });
        return input;
      }
      [['name', '模型名', 'text'], ['input-tokens', '输入 token', 'number'], ['output-tokens', '输出 token', 'number'], ['cache-read-tokens', '缓存读 token', 'number'], ['cache-write-tokens', '缓存写 token', 'number'], ['chars-per-second', '每秒输出字符数', 'number']].forEach(function (spec) {
        var label = make('label', 'cpa-runtime-scheduler-text', spec[1]);
        var input = document.createElement('input');
        input.setAttribute('data-fake-field', spec[0]);
        input.type = spec[2];
        if (spec[2] === 'number') { input.min = '0'; input.max = String(Number.MAX_SAFE_INTEGER); input.step = '1'; }
        input.value = model[spec[0]] == null ? (spec[2] === 'number' ? '0' : '') : String(model[spec[0]]);
        input.required = true;
        if (spec[0] === 'chars-per-second') {
          input.required = false;
          input.min = '1';
          input.value = model[spec[0]] ? String(model[spec[0]]) : '';
          input.placeholder = '不限速';
          label.style.gridColumn = '1 / -1';
        }
        fields[spec[0]] = input;
        label.appendChild(input);
        editor.appendChild(label);
      });
      fields.name.addEventListener('input', syncSummary);
      var responseTitle = make('div', 'cpa-runtime-scheduler-text', '固定回复（随机选择）');
      editor.appendChild(responseTitle);
      editor.appendChild(responseList);
      var addResponse = make('button', 'cpa-static-isp-add', '添加固定回复');
      addResponse.type = 'button';
      addResponse.addEventListener('click', function () { addFakeResponse('').focus(); });
      editor.appendChild(addResponse);
      responses.forEach(addFakeResponse);
      details.open = !responses.some(function (value) { return String(value || '').trim() !== ''; });
      var remove = make('button', 'cpa-static-isp-remove', '×');
      remove.type = 'button';
      remove.title = '移除假模型';
      remove.setAttribute('aria-label', '移除假模型');
      row.appendChild(details);
      row.appendChild(remove);
      var view = {element:row, fields:fields, responseRows:responseRows, details:details};
      fakeModelRows.push(view);
      fakeModelsList.appendChild(row);
      remove.addEventListener('click', function () {
        fakeModelRows = fakeModelRows.filter(function (item) { return item !== view; });
        row.remove();
      });
      return view;
    }
    function loadFakeModels(models) {
      fakeModelRows = [];
      fakeModelsList.textContent = '';
      models.forEach(addFakeModelRow);
    }
    addFakeModel.addEventListener('click', function () { fakeModelsSection.open = true; addFakeModelRow({}).fields.name.focus(); });
    var ispStatusData = null;
    var ispStatusPending = false;
    var ispStatusError = '';
    var accountStatusNodes = new Map();
    resetTurnStateView = function () {
      persistSettings.checked = false;
      workBuddyAutoCheckin.checked = false;
      workBuddyCheckinInterval.value = '';
      workBuddyCheckinInterval.disabled = true;
      workBuddyAutoTasks.checked = false;
      ispStatusData = null;
      ispStatusPending = false;
      ispStatusError = '';
      staticISPEnabled.checked = false;
      loadStaticISPNodes([]);
      loadFakeModels([]);
      fakeModelsSection.open = false;
      cacheRewriteSection.open = false;
      accountStatusNodes.clear();
      statusList.textContent = '未连接管理接口';
    };
    function renderISPBindings() {
      var ispBindings = new Map(ispStatusData ? (ispStatusData.static_isp_bindings || []).map(function (item) { return [item.auth_id, item.node_id]; }) : []);
      accountStatusNodes.forEach(function (view, authID) {
        var text = ispBindings.has(authID) ? 'ISP: ' + ispBindings.get(authID) : '';
        var error = ispStatusError || (ispStatusData && ispStatusData.static_isp_error) || '';
        if (view.state.textContent !== text) view.state.textContent = text;
        if (view.error.textContent !== error) view.error.textContent = error;
      });
    }
    function loadISPBindings() {
      if (ispStatusPending || !managementReady) return;
      ispStatusPending = true;
      var generation = sessionGeneration;
      request('GET', 'routing/codex-static-isp').then(function (response) {
        return response.json();
      }).then(function (data) {
        if (generation !== sessionGeneration) return;
        ispStatusData = data;
        ispStatusError = '';
        renderISPBindings();
        return loadAuthStatus();
      }).catch(function () {
        if (generation !== sessionGeneration) return;
        ispStatusData = null;
        ispStatusError = '读取 ISP 状态失败';
        renderISPBindings();
        return loadAuthStatus();
      }).finally(function () {
        if (generation === sessionGeneration) ispStatusPending = false;
      });
    }
    var cacheRewriteSection = make('details', 'cpa-runtime-scheduler-section');
    cacheRewriteSection.id = 'cpa-cache-rewrite-section';
    cacheRewriteSection.appendChild(make('summary', 'cpa-runtime-scheduler-section-title', '自定义复写 token 数'));
    var cacheRewriteContent = make('div', 'cpa-runtime-scheduler-section-content');
    var cacheRewriteLabel = make('label', 'cpa-runtime-scheduler-check');
    var cacheRewrite = document.createElement('input');
    cacheRewrite.type = 'checkbox';
    cacheRewriteLabel.appendChild(cacheRewrite);
    cacheRewriteLabel.appendChild(document.createTextNode('启用复写'));
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
    form.appendChild(persistLabel);
    form.appendChild(degradedPortLabel);
    form.appendChild(exoFreePortLabel);
    form.appendChild(c2aRetryCountLabel);
    form.appendChild(c2aRetryIntervalLabel);
    form.appendChild(c2aRetryCredentialsLabel);
    form.appendChild(concurrencyLabel);
    form.appendChild(unauthorizedLabel);
    form.appendChild(quotaLabel);
    form.appendChild(disableCoolingLabel);
    form.appendChild(fingerprintLabel);
    form.appendChild(workBuddyFingerprintLabel);
    form.appendChild(workBuddyCheckinRow);
    form.appendChild(workBuddyTasksLabel);
    form.appendChild(staticISPLabel);
    form.appendChild(staticISPFields);
    fakeModelsContent.appendChild(fakeModelsList);
    fakeModelsContent.appendChild(addFakeModel);
    fakeModelsSection.appendChild(fakeModelsContent);
    form.appendChild(fakeModelsSection);
    cacheRewriteContent.appendChild(cacheRewriteLabel);
    cacheRewriteContent.appendChild(cacheMinLabel);
    cacheRewriteContent.appendChild(cacheMaxLabel);
    cacheRewriteSection.appendChild(cacheRewriteContent);
    form.appendChild(cacheRewriteSection);
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
        loadISPBindings();
      }
    });
    window.addEventListener('resize', avoidManagementSaveButton);

    function loadSettings() {
      var generation = sessionGeneration;
      save.disabled = true;
      request('GET', 'routing/settings').then(function (response) {
        return response.json();
      }).then(function (data) {
        if (generation !== sessionGeneration) return;
        persistSettings.checked = !!data['persist-settings'];
        c2aRetryCount.value = String(data['c2a-max-retry-count'] == null ? 0 : data['c2a-max-retry-count']);
        c2aRetryInterval.value = String(data['c2a-max-retry-interval'] == null ? 0 : data['c2a-max-retry-interval']);
        c2aRetryCredentials.value = String(data['c2a-max-retry-credentials'] == null ? 0 : data['c2a-max-retry-credentials']);
        concurrency.value = String(data['auth-concurrency-limit'] == null ? 0 : data['auth-concurrency-limit']);
        degradedPort.value = data['degraded-port-configured'] === false ? '9444' : String(data['degraded-port'] || 0);
        exoFreePort.value = String(data['exo-free-port'] || 0);
        unauthorized.checked = !!data['auto-disable-unauthorized'];
        quota.checked = !!data['auto-disable-quota'];
        disableCooling.checked = !!data['disable-cooling'];
        fingerprintConvergence.checked = !!data['codex-fingerprint-convergence'];
        workBuddyFingerprintConvergence.checked = !!data['workbuddy-fingerprint-convergence'];
        workBuddyAutoCheckin.checked = !!data['workbuddy-auto-checkin'];
        workBuddyCheckinInterval.value = String(data['workbuddy-checkin-interval-seconds']);
        workBuddyCheckinInterval.disabled = !workBuddyAutoCheckin.checked;
        workBuddyAutoTasks.checked = !!data['workbuddy-auto-tasks'];
        var staticISP = data['codex-static-isp'] || {};
        staticISPEnabled.checked = !!staticISP.enabled;
        loadStaticISPNodes(staticISP.nodes || []);
        loadFakeModels(data['fake-models'] || []);
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
            var stateError = make('span', 'cpa-runtime-scheduler-isp-error');
            row.appendChild(stateLine);
            row.appendChild(stateError);
            accountStatusNodes.set(item.id, {account:item, state:stateLine, error:stateError});
          }
          row.appendChild(makeAccountStateControl(item, loadAuthStatus));
          statusList.appendChild(row);
        });
        renderISPBindings();
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
      if (!degradedPort.reportValidity() || !exoFreePort.reportValidity()) return;
      if (cacheRewrite.checked && (!cacheMin.checkValidity() || !cacheMax.checkValidity())) {
        cacheRewriteSection.open = true;
        if (!cacheMin.reportValidity()) return;
        cacheMax.reportValidity();
        return;
      }
      if (!workBuddyCheckinInterval.reportValidity()) return;
      if (!staticISPRows.every(function (row) { return Object.keys(row.fields).every(function (key) { return row.fields[key].reportValidity(); }); })) return;
      if (!fakeModelRows.every(function (row) {
        if (!row.responseRows.length) {
          fakeModelsSection.open = true;
          row.details.open = true;
          status.textContent = '每个假模型至少填写一条固定回复';
          return false;
        }
        var inputs = Object.keys(row.fields).map(function (key) { return row.fields[key]; }).concat(row.responseRows.map(function (response) { return response.input; }));
        return inputs.every(function (input) {
          if (input.checkValidity()) return true;
          fakeModelsSection.open = true;
          row.details.open = true;
          input.reportValidity();
          return false;
        });
      })) return;
      var generation = sessionGeneration;
      save.disabled = true;
      status.textContent = '应用中...';
      request('PATCH', 'routing/settings', {
        'persist-settings': persistSettings.checked,
        'degraded-port': Number(degradedPort.value || '0'),
        'exo-free-port': Number(exoFreePort.value || '0'),
        'c2a-max-retry-count': Math.max(0, Math.min(1000000, parseInt(c2aRetryCount.value || '0', 10) || 0)),
         'c2a-max-retry-interval': Math.max(0, Math.min(1000000, parseInt(c2aRetryInterval.value || '0', 10) || 0)),
         'c2a-max-retry-credentials': Math.max(0, Math.min(1000000, parseInt(c2aRetryCredentials.value || '0', 10) || 0)),
        'auth-concurrency-limit': Math.max(0, Math.min(1000000, parseInt(concurrency.value || '0', 10) || 0)),
        'auto-disable-unauthorized': unauthorized.checked,
         'auto-disable-quota': quota.checked,
         'disable-cooling': disableCooling.checked,
        'codex-fingerprint-convergence': fingerprintConvergence.checked,
        'workbuddy-fingerprint-convergence': workBuddyFingerprintConvergence.checked,
        'workbuddy-auto-checkin': workBuddyAutoCheckin.checked,
        'workbuddy-checkin-interval-seconds': Number(workBuddyCheckinInterval.value),
        'workbuddy-auto-tasks': workBuddyAutoTasks.checked,
        'codex-static-isp': {enabled:staticISPEnabled.checked, nodes:staticISPRows.map(function (row) {
          var node = {};
          Object.keys(row.fields).forEach(function (key) {
            var value = row.fields[key].value;
            if (key === 'password' && value === '') return;
            node[key] = key === 'port' ? Number(value) : value;
          });
          return node;
        })},
        'fake-models': fakeModelRows.map(function (row) {
          var fields = row.fields;
          return {name:fields.name.value.trim(), responses:row.responseRows.map(function (response) { return response.input.value; }),
            'chars-per-second':Number(fields['chars-per-second'].value),
            'input-tokens':Number(fields['input-tokens'].value), 'output-tokens':Number(fields['output-tokens'].value),
            'cache-read-tokens':Number(fields['cache-read-tokens'].value), 'cache-write-tokens':Number(fields['cache-write-tokens'].value)};
        }),
        'cache-token-rewrite': {
          enabled: cacheRewrite.checked,
          'min-percent': cacheMin.value === '' ? null : Number(cacheMin.value),
          'max-percent': cacheMax.value === '' ? null : Number(cacheMax.value)
        },
        'codex-routing-hint': routingHint.value
      }).then(function (response) {
        return response.json();
      }).then(function (result) {
        if (generation !== sessionGeneration) return;
        clearSubmittedISPPasswords();
        fakeModelRows.forEach(function (row) { row.details.open = false; });
        status.textContent = result.persisted ? '已应用并写入配置文件' : '已应用（仅当前进程）';
        loadISPBindings();
      }).catch(function (err) {
        if (generation !== sessionGeneration) return;
        status.textContent = (err.runtimeError ? '配置已接收，静态 ISP 不可用：' : (err.applied ? '已应用，写入文件失败：' : '应用失败：')) + err.message;
        if (err.applied) {
          clearSubmittedISPPasswords();
          loadISPBindings();
        }
      }).finally(function () {
        if (generation === sessionGeneration) save.disabled = false;
      });
    });
    new MutationObserver(function () { renderQuotaCosts(); renderAccountStateControls(); renderModelTokenTooltips(); positionModelTooltips(); positionAccountTokenTooltips(); }).observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ['data-cpa-auth-name', 'title'] });
    window.addEventListener('resize', positionModelTooltips);
    window.addEventListener('resize', positionAccountTokenTooltips);
    document.addEventListener('scroll', positionAccountTokenTooltips, true);
    document.addEventListener('keydown', function (event) { if (event.key === 'Escape') { hideAccountTokenTooltip(); closeWorkBuddyTasks(); } });
    document.addEventListener('pointerdown', function (event) {
      if (accountTokenAnchor && !accountTokenAnchor.contains(event.target) && !accountTokenTooltip.contains(event.target)) hideAccountTokenTooltip();
    });
    setInterval(function () { if (!panel.hidden) loadISPBindings(); }, 5000);
    setInterval(function () {
      document.querySelectorAll('[data-cpa-workbuddy-tasks]').forEach(function (button) { if (button.account) loadWorkBuddyTaskBadge(button.account); });
    }, 30000);
    renderQuotaCosts();
    renderAccountStateControls();
  }

  var style = document.createElement('style');
     style.textContent = '.cpa-runtime-scheduler-toggle{position:fixed;right:20px;bottom:20px;z-index:2147483646;border:0;border-radius:8px;padding:9px 14px;background:#2563eb;color:#fff;font-size:13px;font-weight:600;cursor:grab;touch-action:none;user-select:none;box-shadow:0 4px 16px rgba(0,0,0,.2)}' +
      'body:has([role="dialog"][aria-modal="true"]:not([hidden])) .cpa-runtime-scheduler-toggle,body:has([role="dialog"][aria-modal="true"]:not([hidden])) .cpa-runtime-scheduler-panel{display:none!important}' +
      '.cpa-runtime-scheduler-panel{box-sizing:border-box;position:fixed;right:20px;bottom:64px;z-index:2147483646;width:min(340px,calc(100vw - 16px));max-height:70vh;overflow:auto;padding:16px;border:1px solid #d0d7de;border-radius:8px;background:#fff;color:#1f2328;box-shadow:0 10px 32px rgba(0,0,0,.2);font:14px/1.4 system-ui,sans-serif}' +
       '.cpa-runtime-scheduler-title{font-size:16px;font-weight:700;cursor:move;touch-action:none;user-select:none}.cpa-runtime-scheduler-hint{margin:4px 0 14px;color:#57606a;font-size:12px}.cpa-runtime-scheduler-form{display:grid;gap:9px}.cpa-runtime-scheduler-default{padding:7px 9px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:13px}.cpa-runtime-scheduler-number{display:flex;align-items:center;justify-content:space-between;gap:10px;color:#57606a}.cpa-runtime-scheduler-number input{width:100px;padding:5px;border:1px solid #d0d7de;border-radius:5px}.cpa-runtime-scheduler-check{display:flex;gap:8px;align-items:center}.cpa-runtime-scheduler-save{width:max-content;border:0;border-radius:6px;padding:7px 12px;background:#2563eb;color:#fff;cursor:pointer}.cpa-runtime-scheduler-status{color:#57606a;font-size:12px}.cpa-runtime-scheduler-subtitle{margin-top:18px;padding-top:12px;border-top:1px solid #d8dee4;font-weight:700}.cpa-runtime-scheduler-section{min-width:0;border-top:1px solid #d8dee4}.cpa-runtime-scheduler-section-title{padding:12px 0;cursor:pointer;font-weight:700;letter-spacing:0}.cpa-runtime-scheduler-section-title:focus-visible{outline:2px solid #2563eb;outline-offset:2px}.cpa-runtime-scheduler-section-content{display:grid;gap:9px;padding:0 0 8px;min-width:0}.cpa-runtime-scheduler-disabled-list{display:grid;gap:7px;margin-top:8px}.cpa-runtime-scheduler-disabled-row{display:grid;gap:4px;padding:8px;border-radius:6px;background:#f6f8fa;color:#24292f;font-size:12px}.cpa-runtime-scheduler-empty{color:#57606a;font-size:12px}';
  style.textContent += '.cpa-quota-estimate{color:var(--text-secondary,#57606a);font-size:12px;line-height:1.5;font-variant-numeric:tabular-nums;overflow-wrap:anywhere;letter-spacing:0}';
  style.textContent += '.cpa-quota-provider-picker{display:flex;align-items:stretch;gap:8px;width:100%;min-width:0;max-width:100%}.cpa-quota-provider-picker>:first-child{flex:0 0 auto;overflow:visible}.cpa-quota-provider-picker>:last-child{flex:1 1 0;min-width:0;overflow-x:auto;overscroll-behavior-x:contain;scrollbar-width:thin;scrollbar-color:var(--border-hover) transparent;scroll-padding-inline:8px}.cpa-quota-provider-picker>:last-child::-webkit-scrollbar{display:block;height:6px}.cpa-quota-provider-picker>:last-child::-webkit-scrollbar-thumb{background:var(--border-hover);border-radius:3px}.cpa-quota-provider-picker>:last-child::-webkit-scrollbar-track{background:transparent}';
  var modelTooltipSelector = ':is([class*="AuthFileCard-module__statusTooltip___"],[class*="providerStatusBar-module__statusTooltip___"]):has(>[class*="__tooltipStats"]>:nth-child(4))';
   style.textContent += modelTooltipSelector + '{display:grid;grid-template-columns:minmax(0,1fr) max-content max-content max-content;gap:4px 10px;box-sizing:border-box;width:max-content;max-width:min(480px,calc(100vw - 48px),var(--cpa-model-tooltip-width,100vw));letter-spacing:0}' +
    modelTooltipSelector + '>[class*="__tooltipTime"]{grid-column:1 / -1}' +
    modelTooltipSelector + '>[class*="__tooltipStats"]{display:grid!important;grid-template-columns:subgrid;grid-column:1 / -1;align-items:baseline;column-gap:inherit}' +
    modelTooltipSelector + '>[class*="__tooltipStats"]:not(:has(>:nth-child(4))){grid-column:2 / -1}' +
    modelTooltipSelector + '>[class*="__tooltipStats"]>span{min-width:0;margin-left:0;text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums}' +
     modelTooltipSelector + '>[class*="__tooltipStats"]:has(>:nth-child(4))>:first-child{text-align:left;white-space:normal;overflow-wrap:anywhere}';
   style.textContent += '[class*="AuthFileCard-module__healthCounts___"]{cursor:help}' +
     '.cpa-account-token-tooltip{position:fixed;z-index:2147483647;box-sizing:border-box;width:max-content;max-width:calc(100vw - 16px);max-height:min(70vh,480px);overflow:auto;overscroll-behavior:contain;padding:8px 10px;border:1px solid var(--border-color);border-radius:8px;background:var(--floating-surface);color:var(--text-primary);box-shadow:var(--floating-shadow);font:11px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace;font-variant-numeric:tabular-nums;white-space:nowrap;letter-spacing:0}' +
     '.cpa-account-token-tooltip[hidden]{display:none}' +
     '.cpa-account-token-title{margin-bottom:6px;color:var(--text-tertiary);white-space:nowrap}' +
     '.cpa-account-token-table{width:max-content;border-collapse:separate;border-spacing:0;font:inherit;font-variant-numeric:tabular-nums;white-space:nowrap;letter-spacing:0}' +
     '.cpa-account-token-table th,.cpa-account-token-table td{padding:2px 0 2px 16px;text-align:right;white-space:nowrap;letter-spacing:0}' +
     '.cpa-account-token-table th{color:var(--text-tertiary);font-weight:600;border-bottom:1px solid var(--border-color)}' +
     '.cpa-account-token-table th:first-child,.cpa-account-token-table td:first-child{padding-left:0;text-align:left}' +
     '.cpa-account-token-empty{color:var(--text-secondary);white-space:nowrap}';
   style.textContent += '.cpa-runtime-scheduler-account-state{color:#57606a;overflow-wrap:anywhere;font-variant-numeric:tabular-nums;letter-spacing:0}.cpa-runtime-scheduler-account-state:empty{display:none}.cpa-runtime-scheduler-isp-error{color:#b42318;overflow-wrap:anywhere}.cpa-runtime-scheduler-isp-error:empty{display:none}';
  style.textContent += '.cpa-runtime-scheduler-proxy-fields{display:grid;gap:8px;min-width:0}.cpa-runtime-scheduler-proxy-fields[hidden]{display:none}.cpa-runtime-scheduler-text{display:grid;gap:4px;min-width:0;color:#57606a;font-size:12px}.cpa-runtime-scheduler-text input{box-sizing:border-box;width:100%;min-width:0;padding:6px 8px;border:1px solid #d0d7de;border-radius:5px;font:inherit;letter-spacing:0}';
  style.textContent += '.cpa-static-isp-list{display:grid;gap:10px;min-width:0}.cpa-static-isp-node{position:relative;display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:8px;padding:10px 0 36px;border-bottom:1px solid #d0d7de;min-width:0;letter-spacing:0}.cpa-static-isp-node select{box-sizing:border-box;width:100%;min-width:0;padding:6px;border:1px solid #d0d7de;border-radius:5px;font:inherit;background:#fff}.cpa-static-isp-remove{position:absolute;right:0;bottom:4px;width:28px;height:28px;padding:0;border:1px solid #d0d7de;border-radius:5px;background:#fff;color:#b42318;font-size:20px;cursor:pointer}.cpa-static-isp-remove:hover{background:#fff1f0}.cpa-static-isp-node input:focus-visible,.cpa-static-isp-node select:focus-visible,.cpa-static-isp-remove:focus-visible{outline:2px solid #2563eb;outline-offset:2px}';
  style.textContent += '.cpa-runtime-scheduler-panel .cpa-static-isp-add{box-sizing:border-box;display:inline-flex;align-items:center;justify-content:center;justify-self:start;width:max-content;max-width:100%;min-width:0;height:26px;min-height:0;border:1px solid #d0d7de;border-radius:4px;padding:0 8px;background:transparent;color:#57606a;font:500 12px/1.2 system-ui,sans-serif;cursor:pointer;letter-spacing:0}.cpa-runtime-scheduler-panel .cpa-static-isp-add:hover{background:#f6f8fa;border-color:#afb8c1;color:#24292f}.cpa-runtime-scheduler-panel .cpa-static-isp-add:focus-visible{outline:2px solid #2563eb;outline-offset:2px}';
  style.textContent += '.cpa-fake-model-list{display:grid;gap:10px;min-width:0}.cpa-fake-model-row{position:relative;min-width:0;border-bottom:1px solid #d0d7de;padding-bottom:8px;letter-spacing:0}.cpa-fake-model-row>.cpa-static-isp-remove{top:0;bottom:auto}.cpa-fake-model-summary{padding:5px 34px 5px 0;overflow-wrap:anywhere;cursor:pointer;font-weight:600}.cpa-fake-model-editor{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px;margin-top:10px;min-width:0}.cpa-fake-model-editor>:first-child,.cpa-fake-model-editor>div,.cpa-fake-model-editor>button{grid-column:1 / -1}.cpa-fake-response-list{display:grid;gap:8px;min-width:0}.cpa-fake-response-row{display:grid;grid-template-columns:minmax(0,1fr) 28px;align-items:start;gap:6px;min-width:0}.cpa-fake-response-row>.cpa-static-isp-remove{position:static}.cpa-fake-model-row textarea{box-sizing:border-box;width:100%;min-width:0;min-height:72px;resize:vertical;padding:6px 8px;border:1px solid #d0d7de;border-radius:5px;font:inherit;letter-spacing:0}.cpa-fake-model-row input:focus-visible,.cpa-fake-model-row textarea:focus-visible,.cpa-fake-model-summary:focus-visible{outline:2px solid #2563eb;outline-offset:2px}';
  style.textContent += '.cpa-native-status-hidden,.cpa-account-managed [class*="AuthFileCard-module__stateBadge"]{display:none!important}.cpa-account-state{display:flex;flex-wrap:wrap;gap:0;max-width:100%;font-size:12px;letter-spacing:0}.cpa-account-state-choice{min-width:48px;padding:5px 8px;border:1px solid var(--border-color,#d0d7de);background:var(--bg-primary,#fff);color:var(--text-secondary,#57606a);cursor:pointer}.cpa-account-state-choice:first-child{border-radius:6px 0 0 6px}.cpa-account-state-choice:nth-child(3){border-radius:0 6px 6px 0}.cpa-account-state-choice[aria-pressed="true"]{background:#2563eb;color:#fff}.cpa-account-state-choice[value="degraded"][aria-pressed="true"]{background:#b45309}.cpa-account-state-choice[value="disabled"][aria-pressed="true"]{background:#6b7280}.cpa-account-state-choice:disabled{cursor:wait;opacity:.6}.cpa-account-state-error{flex-basis:100%;color:#b42318;overflow-wrap:anywhere}.cpa-runtime-scheduler-disabled-row strong{overflow-wrap:anywhere}';
  style.textContent += '.cpa-workbuddy-task-button{margin-right:8px;gap:6px;letter-spacing:0}.cpa-workbuddy-task-button svg{flex:none;width:14px;height:14px}.cpa-workbuddy-task-button[data-checkin-state="checked"] svg{color:var(--success-color)}.cpa-workbuddy-task-button[data-checkin-state="unchecked"] svg{color:var(--text-tertiary)}' +
    '.cpa-workbuddy-task-backdrop{z-index:2147483647;letter-spacing:0}.cpa-workbuddy-task-backdrop[hidden]{display:none}.cpa-workbuddy-task-dialog{width:680px;color:var(--text-primary)}.cpa-workbuddy-task-header{padding-right:64px}.cpa-workbuddy-task-heading{min-width:0}.cpa-workbuddy-task-heading h2{margin:0}.cpa-workbuddy-task-subtitle{margin-top:4px;font-size:12px;color:var(--text-secondary);overflow-wrap:anywhere}.cpa-workbuddy-task-body{min-height:0;max-height:none;padding:0 24px 12px;overscroll-behavior:contain}.cpa-workbuddy-task-summary{display:flex;flex-wrap:wrap;align-items:center;gap:8px 12px;padding:16px 0;color:var(--text-secondary);font-size:12px}' +
    '.cpa-workbuddy-task-overviews{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:24px;border-bottom:1px solid var(--border-color)}.cpa-workbuddy-task-overviews>.cpa-workbuddy-task-section{border:0;padding:4px 0 20px}.cpa-workbuddy-task-section{padding:20px 0;border-bottom:1px solid var(--border-color)}.cpa-workbuddy-task-section:last-child{border-bottom:0}.cpa-workbuddy-task-section h3{display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:6px 12px;margin:0 0 12px;font-size:14px;font-weight:600}.cpa-workbuddy-task-count{font-size:12px;font-weight:400;color:var(--text-secondary)}.cpa-workbuddy-task-overview{display:flex;flex-direction:column;align-items:flex-start;gap:6px;font-size:14px}.cpa-workbuddy-task-overview span,.cpa-workbuddy-task-muted{font-size:12px;color:var(--text-secondary);overflow-wrap:anywhere}' +
    '.cpa-workbuddy-task-row{padding:14px 0;border-top:1px solid var(--border-color)}.cpa-workbuddy-task-row:first-of-type{padding-top:0;border-top:0}.cpa-workbuddy-task-row:last-child{padding-bottom:0}.cpa-workbuddy-task-row-head,.cpa-workbuddy-task-result-title{display:flex;align-items:flex-start;justify-content:space-between;gap:12px}.cpa-workbuddy-task-row-head strong,.cpa-workbuddy-task-result-title strong{min-width:0;font-size:13px;font-weight:500;overflow-wrap:anywhere}.cpa-workbuddy-task-badge{flex:none;padding:2px 7px;border:1px solid var(--border-color);border-radius:4px;background:var(--bg-tertiary);color:var(--text-secondary);font-size:11px;line-height:1.4}.cpa-workbuddy-task-badge.success{background:var(--success-badge-bg);color:var(--success-badge-text);border-color:var(--success-badge-border)}.cpa-workbuddy-task-badge.failed{background:var(--failure-badge-bg);color:var(--failure-badge-text);border-color:var(--failure-badge-border)}' +
    '.cpa-workbuddy-task-progress{display:flex;justify-content:space-between;flex-wrap:wrap;gap:4px 12px;margin-top:7px;font-size:12px;color:var(--text-secondary);font-variant-numeric:tabular-nums}.cpa-workbuddy-task-track{height:3px;margin:8px 0;border-radius:2px;background:var(--bg-tertiary);overflow:hidden}.cpa-workbuddy-task-track-fill{height:100%;background:var(--primary-color)}.cpa-workbuddy-task-results{margin-top:12px}.cpa-workbuddy-task-result{padding:10px 0;border-top:1px solid var(--border-color);font-size:12px}.cpa-workbuddy-task-result-message{margin-top:5px;color:var(--text-secondary);overflow-wrap:anywhere}.cpa-workbuddy-task-actions{flex-wrap:wrap}.cpa-workbuddy-task-refresh{order:-1;margin-right:auto;min-width:34px;padding:8px}.cpa-workbuddy-task-error,.cpa-workbuddy-task-error-line{color:var(--error-color);font-size:12px;overflow-wrap:anywhere}.cpa-workbuddy-task-error-line:not(:empty){padding:0 24px 12px}' +
    '.cpa-runtime-scheduler-checkin-row{display:flex;align-items:center;justify-content:space-between;gap:8px}.cpa-runtime-scheduler-checkin-row>.cpa-runtime-scheduler-check{min-width:0}.cpa-runtime-scheduler-checkin-interval{display:flex;align-items:center;gap:4px;white-space:nowrap;font-size:12px}.cpa-runtime-scheduler-checkin-interval input{box-sizing:border-box;width:62px;min-width:0;padding:4px 6px;border:1px solid var(--border-color);border-radius:4px;background:var(--bg-primary);color:var(--text-primary)}.cpa-runtime-scheduler-checkin-interval input:disabled{opacity:.5}.cpa-runtime-scheduler-checkin-interval input:focus-visible{outline:2px solid var(--primary-color);outline-offset:2px}' +
    '@media(max-width:600px){.cpa-workbuddy-task-backdrop{padding:12px}.cpa-workbuddy-task-header{padding:16px 56px 16px 16px}.cpa-workbuddy-task-body{padding:0 16px 8px}.cpa-workbuddy-task-actions{padding:12px 16px;flex-direction:row}.cpa-workbuddy-task-actions .btn{width:auto}.cpa-workbuddy-task-overviews{gap:16px}.cpa-workbuddy-task-section{padding:16px 0}.cpa-workbuddy-task-error-line:not(:empty){padding:0 16px 12px}}';
  style.textContent += '[data-cpa-managed-account="true"] [class*="AuthFileCard-module__toggleWrap"],[data-cpa-managed-account="true"] [class*="AuthFileCard-module__stateBadge"]{display:none!important}';
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
// and quota cost labels and aligns model statistics without modifying the downloaded bundle.
func AddRuntimeSchedulerToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(runtimeSchedulerInlineMarker)) {
		return data, nil
	}
	if legacyStart := bytes.Index(data, []byte("<script "+legacyRuntimeSchedulerInlineMarker+">")); legacyStart >= 0 {
		if legacyEnd := bytes.Index(data[legacyStart:], []byte("</script>")); legacyEnd >= 0 {
			legacyEnd += legacyStart + len("</script>")
			data = bytes.Replace(data, data[legacyStart:legacyEnd], nil, 1)
		}
	}
	data = bytes.Replace(data, []byte("(0,U.jsxs)(`article`,{className:[aA.card,"), []byte("(0,U.jsxs)(`article`,{\"data-cpa-auth-name\":n.name,className:[aA.card,"), 1)
	data = bytes.Replace(data, []byte("(0,U.jsx)(VA,{types:SN,counts:R,active:l,resolvedTheme:n,onChange:ie})"), []byte(quotaProviderTabs), 1)
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
