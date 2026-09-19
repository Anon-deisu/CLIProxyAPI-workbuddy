package managementasset

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// fixtureManagementHTML provides the bundle's closing body injection point.
func fixtureManagementHTML() []byte {
	return []byte("<!doctype html>\n" +
		"<html>\n<head><title>management</title></head>\n<body>\n" +
		"<div id=\"root\"><section class=\"OAuthPage-module__oauthCard\">Cursor OAuth</section></div>\n" +
		"</body>\n</html>")
}

func TestAddCursorAPIKeyInlineToManagementHTML(t *testing.T) {
	t.Parallel()

	input := fixtureManagementHTML()
	got, errPatch := AddCursorAPIKeyInlineToManagementHTML(input)
	if errPatch != nil {
		t.Fatalf("AddCursorAPIKeyInlineToManagementHTML() error = %v", errPatch)
	}

	// The idempotency marker must appear before the closing body tag.
	markerIdx := bytes.Index(got, []byte(cursorAPIKeyInlineMarker))
	if markerIdx < 0 {
		t.Fatalf("patched management html missing idempotency marker %q", cursorAPIKeyInlineMarker)
	}
	if bodyIdx := bytes.Index(got, []byte("</body>")); markerIdx > bodyIdx {
		t.Fatalf("inline script (%d) injected after </body> (%d)", markerIdx, bodyIdx)
	}

	// The removed standalone page must not return.
	for _, gone := range [][]byte{
		[]byte(`path:` + "\x60/cursor-api-key\x60"),
		[]byte("cpa-native-route-host"),
		[]byte("nav.cursor_api_key"),
	} {
		if bytes.Contains(got, gone) {
			t.Fatalf("patched management html still carries legacy standalone-page marker %q", gone)
		}
	}

	// Preserve the backend endpoint and inline input.
	for _, want := range [][]byte{
		[]byte("cpa-key-inline-script"),
		[]byte("managementPath = '/v0/management/'"),
		[]byte("managementPath + 'cursor-api-key'"),
		[]byte("MutationObserver"),
		[]byte("Cursor API Key"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("patched management html missing %q", want)
		}
	}

	// Repeated injection must be idempotent.
	again, errAgain := AddCursorAPIKeyInlineToManagementHTML(got)
	if errAgain != nil {
		t.Fatalf("second AddCursorAPIKeyInlineToManagementHTML() error = %v", errAgain)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("Cursor API key inline patch is not idempotent")
	}
	// An idempotent pass must reuse the input buffer.
	if !sameBacking(got, again) {
		t.Fatal("idempotent pass should return the already-patched buffer unchanged")
	}
}

func TestAddCursorAPIKeyInlineEmptyInput(t *testing.T) {
	t.Parallel()

	_, errPatch := AddCursorAPIKeyInlineToManagementHTML(nil)
	if errPatch == nil {
		t.Fatal("expected error for empty input, got nil")
	}
	if !strings.Contains(errPatch.Error(), "empty") {
		t.Fatalf("unexpected error text: %v", errPatch)
	}
}

func TestAddCursorAPIKeyInlineNoBodyFallsBackToEnd(t *testing.T) {
	t.Parallel()

	in := []byte("<html>management only, no body tag</html>")
	got, errPatch := AddCursorAPIKeyInlineToManagementHTML(in)
	if errPatch != nil {
		t.Fatalf("AddCursorAPIKeyInlineToManagementHTML() error = %v", errPatch)
	}
	if bytes.Index(got, []byte(cursorAPIKeyInlineMarker)) < 0 {
		t.Fatal("expected inline marker even without a </body> anchor")
	}
}

func TestAddRuntimeSchedulerToManagementHTML(t *testing.T) {
	t.Parallel()
	input := fixtureManagementHTML()
	got, errPatch := AddRuntimeSchedulerToManagementHTML(input)
	if errPatch != nil {
		t.Fatalf("AddRuntimeSchedulerToManagementHTML() error = %v", errPatch)
	}
	for _, want := range [][]byte{
		[]byte(runtimeSchedulerInlineMarker),
		[]byte("routing/settings"),
		[]byte("c2a-max-retry-count"),
		[]byte("c2a-max-retry-interval"),
		[]byte("c2a-max-retry-credentials"),
		[]byte("disable-cooling"),
		[]byte("无视指数冷却"),
		[]byte("auth-concurrency-limit"),
		[]byte("codex-fingerprint-convergence"),
		[]byte("codex-routing-hint"),
		[]byte("codex-auto-degrade"),
		[]byte("cacheRewrite.checked = !!rewrite.enabled"),
		[]byte("'cache-token-rewrite': {"),
		[]byte("document.createTextNode('自定义复写 token 数')"),
		[]byte("cacheMin.reportValidity()"),
		[]byte("cacheMax.reportValidity()"),
		[]byte("cpa-runtime-scheduler-position"),
		[]byte("cpa-runtime-scheduler-toggle-position"),
		[]byte("pointerdown"),
		[]byte("touch-action:none"),
		[]byte("avoidManagementSaveButton"),
		[]byte("QuotaCard-module__body"),
		[]byte("estimated_cost_usd"),
		[]byte("预估费用"),
		[]byte("auto-disable-unauthorized"),
		[]byte("auto_disable_reason"),
		[]byte("quota_paused"),
		[]byte("quota_resume_at"),
		[]byte("额度耗尽自动调度"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("runtime scheduler injection missing %q", want)
		}
	}
	for _, removed := range []string{"cpa-runtime-scheduler-account-estimate", "cpa-runtime-scheduler-account-concurrency", "auth-files/fields", "formatQuota", "cpa-runtime-scheduler-turn-states", "cpa-runtime-scheduler-turn-state-row"} {
		if bytes.Contains(got, []byte(removed)) {
			t.Fatalf("runtime panel still contains misplaced quota/account controls: %s", removed)
		}
	}
	again, errAgain := AddRuntimeSchedulerToManagementHTML(got)
	if errAgain != nil || !bytes.Equal(again, got) {
		t.Fatalf("runtime scheduler injection is not idempotent: err=%v", errAgain)
	}
}

func TestRuntimeSchedulerTracksNativeAuthFileResponses(t *testing.T) {
	node, errNode := exec.LookPath("node")
	if errNode != nil {
		t.Skip("Node.js is required for the inline JavaScript regression test")
	}
	start := strings.Index(runtimeSchedulerInlineScript, ">") + 1
	end := strings.LastIndex(runtimeSchedulerInlineScript, "</script>")
	script := runtimeSchedulerInlineScript[start:end]
	command := exec.Command(node, "-e", runtimeSchedulerNetworkTest)
	command.Stdin = strings.NewReader(script)
	if output, errRun := command.CombinedOutput(); errRun != nil {
		t.Fatalf("inline script regression: %v\n%s", errRun, output)
	}
}

func TestRuntimeSchedulerTurnStateControls(t *testing.T) {
	node, errNode := exec.LookPath("node")
	if errNode != nil {
		t.Skip("Node.js is required for the inline JavaScript regression test")
	}
	start := strings.Index(runtimeSchedulerInlineScript, ">") + 1
	end := strings.LastIndex(runtimeSchedulerInlineScript, "</script>")
	command := exec.Command(node, "-e", runtimeSchedulerTurnStateTest)
	command.Stdin = strings.NewReader(runtimeSchedulerInlineScript[start:end])
	if output, errRun := command.CombinedOutput(); errRun != nil {
		t.Fatalf("turn-state UI regression: %v\n%s", errRun, output)
	}
}

const runtimeSchedulerTurnStateTest = `
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const nodes = [], timers = [], listeners = {};
class Element {
  constructor(tag) { this.tagName = tag; this.children = []; this.style = {}; this.listeners = {}; this.attributes = {}; this._text = ''; this.hidden = false; this.value = ''; nodes.push(this); }
  set textContent(value) { this._text = value; this.children = []; }
  get textContent() { return this._text + this.children.map(node => node.textContent).join(''); }
  appendChild(child) { this.children.push(child); return child; }
  addEventListener(type, callback) { this.listeners[type] = callback; }
  click() { if (this.listeners.click) this.listeners.click({stopPropagation() {}}); }
  setAttribute(key, value) { this.attributes[key] = value; }
  querySelectorAll(tag) { return this.children.filter(child => child.tagName === tag); }
  contains(node) { return this.children.includes(node); }
  getBoundingClientRect() { return {left:10,top:10,width:320,height:500,right:330,bottom:510}; }
  reportValidity() { return true; }
}
globalThis.window = globalThis;
window.location = {href: 'https://panel.test/management.html', hash:'#/quota'};
window.innerWidth = 1280; window.innerHeight = 800;
window.addEventListener = (type, callback) => { listeners[type] = callback; };
globalThis.document = {
  readyState:'complete', documentElement:new Element('html'), body:new Element('body'),
  createElement: tag => new Element(tag),
  createTextNode: text => { const node = new Element('text'); node.textContent = text; return node; },
  getElementById: id => nodes.find(node => node.id === id),
  querySelectorAll: () => [], querySelector: () => null
};
globalThis.localStorage = {getItem: () => null};
globalThis.MutationObserver = class { observe() {} };
globalThis.XMLHttpRequest = class { open() {} setRequestHeader() {} send() {} };
globalThis.setInterval = (callback, interval) => { timers.push({callback,interval}); };
let now = 0;
Object.defineProperty(globalThis, 'performance', {value:{now: () => now}});
let enabled = false, automatic = false, badState = false, patch = null, failStatus = false, lateResolve, statePatch;
const account = {id:'account-a',name:'account-a.json',auth_index:'a',provider:'codex',disabled:false,degraded:false};
const other = {id:'account-b',name:'account-b.json',auth_index:'b',provider:'codex',disabled:false,degraded:false};
let delayStatus = false;
window.fetch = async (url, options = {}) => {
  if (url.endsWith('routing/settings')) {
    if (options.method === 'PATCH') {
      patch = JSON.parse(options.body); enabled = patch['codex-turn-state-enabled']; automatic = patch['codex-auto-degrade'];
      return new Response('{}');
    }
    return new Response(JSON.stringify({'codex-turn-state-enabled':enabled,'codex-auto-degrade':automatic,'degraded-port':18888}));
  }
  if (url.endsWith('auth-files')) return new Response(JSON.stringify({files:[account,other]}));
  if (url.endsWith('auth-files/status')) {
    statePatch = JSON.parse(options.body);
    account.disabled = statePatch.scheduling_state === 'disabled';
    account.degraded = statePatch.scheduling_state === 'degraded';
    return new Response('{}');
  }
  if (url.endsWith('routing/codex-turn-state')) {
    if (delayStatus) return new Promise(resolve => {lateResolve = resolve;});
    if (failStatus) return new Response('{}', {status:500});
    return new Response(JSON.stringify({enabled, accounts:[
      badState ? {auth_id:'account-a',status:'error',remaining_seconds:0,observed_length:356,last_error:'non-quality state'} : {auth_id:'account-a',status:'active',remaining_seconds:330,token_length:292},
      {auth_id:'account-b',status:'active',remaining_seconds:125,refreshing:true,token_length:332}
    ]}));
  }
  throw new Error('unexpected request ' + url);
};
vm.runInThisContext(fs.readFileSync(0, 'utf8'));
const flush = async () => { for (let i=0; i<8; i++) await new Promise(setImmediate); };
(async () => {
  await window.fetch('https://panel.test/v0/management/routing/settings', {headers:{Authorization:'Bearer synthetic-key'}});
  const toggle = nodes.find(node => node.className === 'cpa-runtime-scheduler-toggle');
  const list = nodes.find(node => node.className === 'cpa-runtime-scheduler-disabled-list');
  assert.equal(nodes.some(node => node.className === 'cpa-runtime-scheduler-turn-states'), false, 'state must not use a separate account list');
  const checkbox = nodes.find(node => node.tagName === 'label' && node.textContent === 'Codex 292/332 state 自动续期').children[0];
  const automaticCheckbox = nodes.find(node => node.tagName === 'label' && node.textContent === 'Codex 自动降级').children[0];
  const save = nodes.find(node => node.className === 'cpa-runtime-scheduler-save');
  toggle.click(); await flush();
  const portInput = nodes.find(node => node.tagName === 'label' && node.textContent === '降级账号 API 端口').children[1];
  assert.equal(portInput.value, '18888');
  const stateControl = nodes.findLast(node => node.className === 'cpa-account-state' && node.account.id === account.id);
  stateControl.children[1].click(); await flush();
  assert.equal(statePatch.scheduling_state, 'degraded');
  assert.equal(statePatch.auth_index, 'a');
  assert.equal(account.degraded, true);
  assert.equal(checkbox.checked, false);
  assert.equal(automaticCheckbox.disabled, true);
  assert.match(list.textContent, /state 采集未开启/);
  checkbox.checked = true; checkbox.listeners.change();
  assert.equal(automaticCheckbox.disabled, false);
  automaticCheckbox.checked = true; save.click(); await flush();
  assert.equal(patch['codex-turn-state-enabled'], true);
  assert.equal(patch['codex-auto-degrade'], true);
  assert.equal(patch['degraded-port'], 18888);
  assert.match(list.textContent, /account-a.*292 剩余.*5:30.*account-b.*332 剩余.*2:05.*续期中/);
  assert.equal(list.children.length, 2);
  assert.equal((list.textContent.match(/account-a.json/g) || []).length, 1, 'one row per account');
  badState = true; account.degraded = true;
  timers.find(timer => timer.interval === 5000).callback(); await flush();
  assert.match(list.children[0].textContent, /降级账号池.*最近采集：356.*non-quality state/);
  assert.equal(list.children[0].children.find(node => node.className === 'cpa-account-state').children[1].attributes['aria-pressed'], 'true');
  badState = false; account.degraded = false;
  timers.find(timer => timer.interval === 5000).callback(); await flush();
  assert.match(list.children[0].textContent, /状态正常.*292 剩余/);
  assert.equal(list.children[0].children.find(node => node.className === 'cpa-account-state').children[0].attributes['aria-pressed'], 'true');
  now += 2000;
  timers.find(timer => timer.interval === 1000).callback();
  assert.match(list.textContent, /5:28.*2:03/);
  now += 3600000;
  timers.find(timer => timer.interval === 1000).callback();
  assert.match(list.textContent, /已到期/);
  assert.doesNotMatch(list.textContent, /-\d+:/);
  failStatus = true;
  timers.find(timer => timer.interval === 5000).callback(); await flush();
  assert.match(list.textContent, /读取 state 状态失败/);
  assert.equal(list.children.length, 2, 'status errors must not remove account controls');
  failStatus = false; delayStatus = true;
  timers.find(timer => timer.interval === 5000).callback(); await flush();
  listeners.unauthorized();
  lateResolve(new Response(JSON.stringify({enabled:true,accounts:[{name:'stale-account',remaining_seconds:300}]})));
  await flush();
  assert.equal(list.textContent, '未连接管理接口');
  assert.equal(checkbox.checked, false);
  assert.equal(automaticCheckbox.checked, false);
  console.log('merged account rows, automatic degradation toggle, countdown and logout checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
`

const runtimeSchedulerNetworkTest = `
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const listeners = {};
const cards = ['a.json', 'b.json'].map(name => {
  const card = { line: null };
  card.querySelector = selector => {
    if (selector.includes('fileName')) return {textContent: name, getAttribute: () => name};
    if (selector.includes('body')) return {insertAdjacentElement: (_, node) => {
      card.line = node;
      node.remove = () => { card.line = null; };
    }};
    if (selector.includes('data-cpa-estimated-cost')) return card.line;
    return null;
  };
  return card;
});
globalThis.window = globalThis;
window.location = {href: 'https://panel.test/management.html', hash: '#/quota'};
window.addEventListener = (type, callback) => { listeners[type] = callback; };
globalThis.document = {
  readyState: 'loading',
  documentElement: {appendChild() {}},
  addEventListener() {},
  querySelectorAll: selector => selector.includes('QuotaCard') ? cards : [],
  querySelector: () => ({}),
  createElement: () => ({setAttribute() {}, textContent: ''})
};
globalThis.localStorage = {getItem: () => null};
globalThis.XMLHttpRequest = class {
  open(method, url) { this.method = method; this.url = url; }
  setRequestHeader() {}
  addEventListener(type, callback) { this[type] = callback; }
  send() {}
  complete(status, payload, responseType = '') {
    this.status = status;
    this.responseType = responseType;
    this.response = payload;
    this.responseText = JSON.stringify(payload);
    if (this.load) this.load();
  }
};
let fetchData = {files: []};
window.fetch = async () => new Response(JSON.stringify(fetchData), {status: 200});
vm.runInThisContext(fs.readFileSync(0, 'utf8'));
const files = amount => ({files: [
  {name:'a.json', estimated_cost_usd: amount},
  {name:'b.json', estimated_cost_usd: 4.5}
]});
function xhr(path = 'auth-files', method = 'GET') {
  const req = new XMLHttpRequest();
  req.open(method, 'https://server.test/proxy/v0/management/' + path);
  req.setRequestHeader('Authorization', 'Bearer test-session');
  req.send();
  return req;
}
(async () => {
  xhr().complete(200, files(1.25));
  assert.ok(cards[0].line, 'native XHR must render costs without polling or remembered credentials');
  assert.ok(cards[0].line.textContent.endsWith('$1.250000'));
  const slow = xhr();
  xhr().complete(200, files(0), 'json');
  assert.ok(cards[0].line.textContent.endsWith('$0.000000'), 'refresh must update costs immediately');
  slow.complete(200, files(9));
  assert.ok(cards[0].line.textContent.endsWith('$0.000000'), 'late response must not restore stale costs');
  fetchData = files(2);
  const response = await window.fetch('https://server.test/proxy/v0/management/auth-files', {headers: {Authorization:'Bearer test-session'}});
  assert.equal((await response.json()).files[0].estimated_cost_usd, 2, 'observer must not consume the original response');
  assert.ok(cards[0].line.textContent.endsWith('$2.000000'), 'native fetch must update costs too');
  const observedFetch = window.fetch;
  let refreshed;
  window.fetch = function () { return refreshed = observedFetch.apply(window, arguments); };
  fetchData = files(4);
  xhr('api-call', 'POST').complete(200, {status_code:200});
  await refreshed;
  assert.ok(cards[0].line.textContent.endsWith('$4.000000'), 'single-account quota refresh must also update costs');
  const late = xhr();
  window.location.hash = '#/login';
  listeners.hashchange();
  late.complete(200, files(8));
  assert.equal(cards[0].line, null, 'logout must discard cached and in-flight account costs');
  window.location.hash = '#/quota';
  xhr().complete(200, files(3));
  xhr('config').complete(401, {error: 'invalid key'});
  assert.equal(cards[0].line, null, 'unauthorized response must clear the captured session');
  console.log('native XHR/fetch refresh, ordering and logout regression checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
`

// sameBacking reports whether both slices share the same backing array.
func sameBacking(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return &a[0] == &b[0]
}
