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
		[]byte("调度方式：Codex2API 默认（额度加权均衡）"),
		[]byte("codex-fingerprint-convergence"),
		[]byte("WorkBuddy 账号指纹收敛"),
		[]byte("workBuddyFingerprintConvergence.checked = !!data['workbuddy-fingerprint-convergence']"),
		[]byte("'workbuddy-fingerprint-convergence': workBuddyFingerprintConvergence.checked"),
		[]byte("form.appendChild(workBuddyFingerprintLabel)"),
		[]byte("codex-routing-hint"),
		[]byte("routing/codex-static-isp"),
		[]byte("codex-static-isp"),
		[]byte("static_isp_bindings"),
		[]byte("loadStaticISPNodes"),
		[]byte("clearSubmittedISPPasswords"),
		[]byte("Cliproxy 静态 ISP 固定出口"),
		[]byte("persist-settings"),
		[]byte("OpenCode Free API 端口"),
		[]byte("cacheRewrite.checked = !!rewrite.enabled"),
		[]byte("'cache-token-rewrite': {"),
		[]byte("make('summary', 'cpa-runtime-scheduler-section-title', '自定义复写 token 数')"),
		[]byte("cacheMin.reportValidity()"),
		[]byte("cacheMax.reportValidity()"),
		[]byte("cpa-fake-model-add"),
		[]byte("cpa-fake-models-section"),
		[]byte("cpa-cache-rewrite-section"),
		[]byte("loadFakeModels(data['fake-models'] || [])"),
		[]byte("'fake-models': fakeModelRows.map"),
		[]byte("cpa-runtime-scheduler-position"),
		[]byte("cpa-runtime-scheduler-toggle-position"),
		[]byte("body:has([role=\"dialog\"][aria-modal=\"true\"]) .cpa-runtime-scheduler-toggle"),
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
	for _, removed := range []string{"cpa-runtime-scheduler-account-estimate", "cpa-runtime-scheduler-account-concurrency", "auth-files/fields", "formatQuota", "cpa-runtime-scheduler-turn-states", "cpa-runtime-scheduler-turn-state-row", "Codex 292/332 state 自动续期", "Codex 自动降级", "Codex Excel 路由", "禁用 Excel 回退", "Cliproxy 动态代理采集 state", "codex-turn-state-enabled", "codex-auto-degrade", "codex-excel-routing-enabled", "codex-excel-disable-fallback", "codex-state-cliproxy", "routing/codex-turn-state"} {
		if bytes.Contains(got, []byte(removed)) {
			t.Fatalf("runtime panel still contains misplaced quota/account controls: %s", removed)
		}
	}
	again, errAgain := AddRuntimeSchedulerToManagementHTML(got)
	if errAgain != nil || !bytes.Equal(again, got) {
		t.Fatalf("runtime scheduler injection is not idempotent: err=%v", errAgain)
	}
}

func TestRuntimeSchedulerUpgradesLegacyFingerprintControls(t *testing.T) {
	input := []byte(`<html><body><main>Keep panel</main><script id="cpa-runtime-scheduler-script">window.__cpaRuntimeSchedulerLoaded = true;</script><script id="unrelated">keep();</script></body></html>`)
	original := bytes.Clone(input)
	got, err := AddRuntimeSchedulerToManagementHTML(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("upgrading the scheduler mutated the cached source HTML")
	}
	if bytes.Contains(got, []byte(legacyRuntimeSchedulerInlineMarker)) || bytes.Count(got, []byte(runtimeSchedulerInlineMarker)) != 1 || !bytes.Contains(got, []byte("workbuddy-fingerprint-convergence")) {
		t.Fatal("legacy scheduler was not replaced with the WorkBuddy controls")
	}
	if !bytes.Contains(got, []byte(`<script id="unrelated">keep();</script>`)) || !bytes.Contains(got, []byte("<main>Keep panel</main>")) {
		t.Fatal("upgrading the scheduler changed unrelated page content")
	}
	again, err := AddRuntimeSchedulerToManagementHTML(got)
	if err != nil || !bytes.Equal(again, got) {
		t.Fatal("scheduler upgrade is not idempotent")
	}
}

func TestRuntimeSchedulerModelTooltipAlignment(t *testing.T) {
	t.Parallel()
	got, errPatch := AddRuntimeSchedulerToManagementHTML(fixtureManagementHTML())
	if errPatch != nil {
		t.Fatal(errPatch)
	}
	for _, want := range []string{
		`[class*="AuthFileCard-module__statusTooltip___"]`,
		`[class*="providerStatusBar-module__statusTooltip___"]`,
		`:has(>[class*="__tooltipStats"]>:nth-child(4))`,
		`grid-template-columns:minmax(0,1fr) max-content max-content max-content`,
		`grid-template-columns:subgrid`,
		`'缓存率'`,
		`model.cached_input_tokens * 100 / model.input_tokens`,
		`counts.addEventListener('pointerenter'`,
		`position:fixed`,
		`model_token_usage`,
		`grid-column:2 / -1`,
		`text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums`,
		`window.addEventListener('resize', positionModelTooltips)`,
		`(item.model_token_usage || []).slice().sort(function (a, b)`,
		`Number(b.input_tokens) || 0) - (Number(a.input_tokens) || 0)`,
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("model tooltip alignment missing %q", want)
		}
	}
}

func TestRuntimeSchedulerInjectsAuthFileIdentityForCardBinding(t *testing.T) {
	input := []byte("<html><body>(0,U.jsxs)(`article`,{className:[aA.card,`AuthFileCard-module__card___x`],children:n.name})</body></html>")
	got, err := AddRuntimeSchedulerToManagementHTML(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("{\"data-cpa-auth-name\":n.name,className:[aA.card,")) {
		t.Fatal("auth cards do not expose their stable file name to the runtime control binder")
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
	for _, version := range []string{"v0", "v8"} {
		t.Run(version, func(t *testing.T) {
			harness := runtimeSchedulerNetworkTest
			if version == "v8" {
				harness = strings.NewReplacer("/v0/management/", "/v8/management/", "auth-files", "credentials", "'api-call'", "'requests/api-call'").Replace(harness)
			}
			command := exec.Command(node, "-e", harness)
			command.Stdin = strings.NewReader(script)
			if output, errRun := command.CombinedOutput(); errRun != nil {
				t.Fatalf("inline script regression: %v\n%s", errRun, output)
			}
		})
	}
}

func TestRuntimeSchedulerSettingsAndISPControls(t *testing.T) {
	node, errNode := exec.LookPath("node")
	if errNode != nil {
		t.Skip("Node.js is required for the inline JavaScript regression test")
	}
	start := strings.Index(runtimeSchedulerInlineScript, ">") + 1
	end := strings.LastIndex(runtimeSchedulerInlineScript, "</script>")
	command := exec.Command(node, "-e", runtimeSchedulerTurnStateTest)
	command.Stdin = strings.NewReader(runtimeSchedulerInlineScript[start:end])
	if output, errRun := command.CombinedOutput(); errRun != nil {
		t.Fatalf("scheduler UI regression: %v\n%s", errRun, output)
	}
}

const runtimeSchedulerTurnStateTest = `
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const nodes = [], timers = [], listeners = {};
class Element {
  constructor(tag) { this.tagName = tag; this.children = []; this.style = {}; this.listeners = {}; this.attributes = {}; this._text = ''; this.hidden = false; this.value = ''; this.classList = {values:new Set(), add:(...values) => values.forEach(value => this.classList.values.add(value)), remove:(...values) => values.forEach(value => this.classList.values.delete(value)), contains:value => this.classList.values.has(value)}; nodes.push(this); }
  set textContent(value) { this._text = value; this.children = []; }
  get textContent() { return this._text + this.children.map(node => node.textContent).join(''); }
  appendChild(child) { this.children.push(child); child.parentElement = this; return child; }
  focus() {}
  remove() { this.parentElement.children = this.parentElement.children.filter(node => node !== this); }
  addEventListener(type, callback) { this.listeners[type] = callback; }
  click() { if (this.listeners.click) this.listeners.click({stopPropagation() {}}); }
  setAttribute(key, value) { this.attributes[key] = String(value); if (key === 'id') this.id = String(value); }
  getAttribute(key) { return this.attributes[key] ?? null; }
  removeAttribute(key) { delete this.attributes[key]; }
  querySelectorAll(tag) { return this.children.filter(child => child.tagName === tag); }
  contains(node) { return this.children.includes(node); }
  getBoundingClientRect() { return {left:10,top:10,width:320,height:500,right:330,bottom:510}; }
  reportValidity() { return true; }
  checkValidity() { return true; }
}
globalThis.window = globalThis;
window.location = {href: 'https://panel.test/management.html', hash:'#/quota'};
window.innerWidth = 1280; window.innerHeight = 800;
window.addEventListener = (type, callback) => { listeners[type] = callback; };
globalThis.document = {
  readyState:'complete', documentElement:new Element('html'), body:new Element('body'),
  addEventListener() {},
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
let patch = null, failStatus = false, lateResolve, statePatch;
let staticISPSettings = {enabled:false,nodes:[]};
let fakeModels = [];
let degradedPort = 0, degradedPortConfigured = false, exoFreePort = 0;
let persistedSettings = false, failPersistence = false, failRuntime = false;
const account = {id:'account-a',name:'account-a.json',auth_index:'a',provider:'codex',disabled:false,degraded:false};
const other = {id:'account-b',name:'account-b.json',auth_index:'b',provider:'codex',disabled:false,degraded:false};
const authCard = new Element('article');
authCard.setAttribute('data-cpa-auth-name', account.name);
const authName = new Element('span');
authName.textContent = other.name;
authName.setAttribute('title', other.name);
const nativeToggle = new Element('div');
authCard.appendChild(authName); authCard.appendChild(nativeToggle);
nativeToggle.insertAdjacentElement = (_, node) => authCard.appendChild(node);
authCard.querySelector = selector => {
  if (selector.includes('fileName')) return authName;
  if (selector.includes('toggleWrap')) return nativeToggle;
  if (selector.includes('data-cpa-auth-state')) return authCard.children.find(node => node.getAttribute('data-cpa-auth-state')) || null;
  return null;
};
document.querySelectorAll = selector => selector.includes('AuthFileCard-module__card___') ? [authCard] : [];
let delayStatus = false;
window.fetch = async (url, options = {}) => {
  if (url.endsWith('routing/settings')) {
    if (options.method === 'PATCH') {
      patch = JSON.parse(options.body);
      degradedPort = patch['degraded-port']; degradedPortConfigured = true;
      exoFreePort = patch['exo-free-port'];
      const isp = patch['codex-static-isp'];
      staticISPSettings = {enabled:isp.enabled,nodes:isp.nodes.map(node => {
        const {password,...publicNode} = node;
        publicNode['password-configured'] = !!password || staticISPSettings.nodes.some(old => old.id === node.id && old['password-configured']);
        return publicNode;
      })};
      persistedSettings = patch['persist-settings'];
      fakeModels = patch['fake-models'];
      if (failRuntime) return new Response(JSON.stringify({error:'binding directory unavailable',applied:true,runtime_error:true,persisted:persistedSettings}), {status:503});
      if (failPersistence) return new Response(JSON.stringify({error:'permission denied',applied:true,persisted:false}), {status:500});
      return new Response(JSON.stringify({persisted:persistedSettings,runtime_only:!persistedSettings}));
    }
    return new Response(JSON.stringify({'persist-settings':persistedSettings,'codex-static-isp':staticISPSettings,'fake-models':fakeModels,'cache-token-rewrite':{enabled:true,'min-percent':80,'max-percent':95},'degraded-port':degradedPort,'degraded-port-configured':degradedPortConfigured,'exo-free-port':exoFreePort}));
  }
  if (url.endsWith('auth-files')) return new Response(JSON.stringify({files:[account,other]}));
  if (url.endsWith('auth-files/status')) {
    statePatch = JSON.parse(options.body);
    account.disabled = statePatch.scheduling_state === 'disabled';
    account.degraded = statePatch.scheduling_state === 'degraded';
    return new Response('{}');
  }
  if (url.endsWith('routing/codex-static-isp')) {
    if (delayStatus) return new Promise(resolve => {lateResolve = resolve;});
    if (failStatus) return new Response('{}', {status:500});
    return new Response(JSON.stringify({static_isp_bindings:staticISPSettings.enabled && staticISPSettings.nodes.length ? [{auth_id:'account-a',node_id:staticISPSettings.nodes[0].id}] : []}));
  }
  throw new Error('unexpected request ' + url);
};
vm.runInThisContext(fs.readFileSync(0, 'utf8'));
const flush = async () => { for (let i=0; i<8; i++) await new Promise(setImmediate); };
(async () => {
  await window.fetch('https://panel.test/v0/management/routing/settings', {headers:{Authorization:'Bearer synthetic-key'}});
  const toggle = nodes.find(node => node.className === 'cpa-runtime-scheduler-toggle');
  const list = nodes.find(node => node.className === 'cpa-runtime-scheduler-disabled-list');
  const fakeSection = document.getElementById('cpa-fake-models-section');
  const rewriteSection = document.getElementById('cpa-cache-rewrite-section');
  assert.equal(!!fakeSection.open, false, 'existing fake models must be hidden by default');
  assert.equal(!!rewriteSection.open, false, 'token rewrite controls must be hidden by default');
  const save = nodes.find(node => node.className === 'cpa-runtime-scheduler-save');
  toggle.click(); await flush();
  const cardControl = () => authCard.querySelector('[data-cpa-auth-state]');
  assert.equal(cardControl().account.name, account.name, 'injected filename must win over another account appearing as the display label');
  assert.equal(nativeToggle.classList.contains('cpa-native-status-hidden'), true);
  assert.equal(authCard.getAttribute('data-cpa-managed-account'), 'true');
  const portInput = nodes.find(node => node.tagName === 'label' && node.textContent === '降级账号 API 端口').children[1];
  const exoPortInput = nodes.find(node => node.tagName === 'label' && node.textContent === 'OpenCode Free API 端口').children[1];
  assert.equal(exoPortInput.value, '0', 'new listener must be off by default');
  assert.equal(portInput.value, '9444');
  assert.equal(patch, null, 'prefilling the port must not apply settings');
  portInput.value = '18888';
  exoPortInput.value = '18889';
  const stateControl = nodes.findLast(node => node.className === 'cpa-account-state' && node.account.id === account.id);
  stateControl.children[1].click(); await flush();
  assert.equal(statePatch.scheduling_state, 'degraded');
  assert.equal(statePatch.auth_index, 'a');
  assert.equal(account.degraded, true);
  assert.equal(cardControl().children[1].attributes['aria-pressed'], 'true', 'card must show the same degraded state as the scheduling panel');
  assert.equal(!!fakeSection.open, false);
  assert.equal(!!rewriteSection.open, false);
  save.click(); await flush();
  for (const key of ['codex-turn-state-enabled','codex-auto-degrade','codex-excel-routing-enabled','codex-excel-disable-fallback','codex-state-cliproxy']) assert.equal(Object.hasOwn(patch,key), false);
  assert.deepEqual(patch['cache-token-rewrite'], {enabled:true,'min-percent':80,'max-percent':95}, 'collapsed values must still save');
  assert.equal(patch['degraded-port'], 18888);
  assert.equal(patch['exo-free-port'], 18889);
  const addFake = document.getElementById('cpa-fake-model-add');
  addFake.click(); addFake.click();
  assert.equal(fakeSection.open, true);
  const fakeRows = nodes.filter(node => node.className === 'cpa-fake-model-row');
  const editorOf = row => row.children[0].children[1];
  const fieldsOf = row => Object.fromEntries(editorOf(row).children.filter(node => node.tagName === 'label').map(label => [label.children[0].attributes['data-fake-field'],label.children[0]]));
  const responsesOf = row => editorOf(row).children.find(node => node.className === 'cpa-fake-response-list');
  const firstFake = fieldsOf(fakeRows[0]), secondFake = fieldsOf(fakeRows[1]);
  assert.equal(firstFake['chars-per-second'].value, '');
  assert.equal(firstFake['chars-per-second'].required, false);
  firstFake['chars-per-second'].value = '25';
  assert.equal(fakeRows[0].children[0].open, true, 'new models must be expanded');
  firstFake.name.value = 'fake-alpha'; firstFake.name.listeners.input();
  responsesOf(fakeRows[0]).children[0].children[0].value = 'first fixed reply';
  editorOf(fakeRows[0]).children.at(-1).click();
  responsesOf(fakeRows[0]).children[1].children[0].value = 'alternative fixed reply';
  firstFake['input-tokens'].value = '114514'; firstFake['output-tokens'].value = '1919810';
  firstFake['cache-read-tokens'].value = '325'; firstFake['cache-write-tokens'].value = '799';
  secondFake.name.value = 'fake-beta'; secondFake.name.listeners.input();
  responsesOf(fakeRows[1]).children[0].children[0].value = 'second fixed reply';
  save.click(); await flush();
  assert.deepEqual(fakeModels.map(model => model.name), ['fake-alpha','fake-beta']);
  assert.equal(fakeModels[0]['cache-write-tokens'], 799);
  assert.equal(fakeModels[0]['chars-per-second'], 25);
  assert.equal(fakeModels[1]['chars-per-second'], 0);
  assert.deepEqual(fakeModels[0].responses, ['first fixed reply','alternative fixed reply']);
  assert.equal(fakeRows[0].children[0].open, false, 'applied entries must collapse');
  fakeSection.open = false;
  toggle.click(); toggle.click(); await flush();
  assert.equal(fakeSection.open, false, 'reopening the panel must not reveal saved models');
  assert.equal(fieldsOf(nodes.findLast(node => node.className === 'cpa-fake-model-row')).name.value, 'fake-beta');
  const savedFirst = nodes.find(node => node.className === 'cpa-fake-model-list').children[0];
  assert.equal(fieldsOf(savedFirst)['chars-per-second'].value, '25');
  fieldsOf(savedFirst)['chars-per-second'].value = '';
  assert.equal(savedFirst.children[0].open, false, 'saved entries must load collapsed');
  assert.equal(savedFirst.children[0].children[0].textContent, 'fake-alpha');
  responsesOf(savedFirst).children[0].children[1].click();
  save.click(); await flush();
  assert.deepEqual(fakeModels[0].responses, ['alternative fixed reply'], 'deleted replies must not remain in the saved pool');
  assert.equal(fakeModels[0]['chars-per-second'], 0, 'clearing the rate must restore a single content chunk');
  nodes.findLast(node => node.className === 'cpa-fake-model-row').children.at(-1).click();
  save.click(); await flush();
  assert.deepEqual(fakeModels.map(model => model.name), ['fake-alpha']);
  const ispCheckbox = document.getElementById('cpa-runtime-scheduler-static-isp-enabled');
  assert.equal(ispCheckbox.checked, false);
  ispCheckbox.checked = true; ispCheckbox.listeners.change();
  document.getElementById('cpa-static-isp-add').click();
  let ispRow = nodes.findLast(node => node.className === 'cpa-static-isp-node');
  let ispFields = Object.fromEntries(ispRow.children.filter(node => node.tagName === 'label').map(label => [label.children[0].attributes['data-isp-field'],label.children[0]]));
  ispFields.host.value = '192.0.2.10'; ispFields.port.value = '1080'; ispFields.username.value = 'isp-user'; ispFields.password.value = 'isp-synthetic-secret';
  save.click(); await flush();
  assert.equal(patch['codex-static-isp'].nodes[0].password, 'isp-synthetic-secret');
  assert.equal(ispFields.password.value, '');
  assert.equal(ispFields.password.required, false);
  assert.equal(ispFields.id.readOnly, true);
  assert.match(list.textContent, /ISP: isp-1/);
  toggle.click(); toggle.click(); await flush();
  ispRow = nodes.findLast(node => node.className === 'cpa-static-isp-node');
  ispFields = Object.fromEntries(ispRow.children.filter(node => node.tagName === 'label').map(label => [label.children[0].attributes['data-isp-field'],label.children[0]]));
  assert.equal(ispFields.password.value, '');
  save.click(); await flush();
  assert.equal(Object.hasOwn(patch['codex-static-isp'].nodes[0], 'password'), false);
  document.getElementById('cpa-static-isp-add').click();
  nodes.findLast(node => node.className === 'cpa-static-isp-remove').click();
  save.click(); await flush();
  assert.equal(patch['codex-static-isp'].nodes.length, 1);
  assert.equal(document.getElementById('cpa-runtime-scheduler-cliproxy-gateway'), undefined);
  toggle.click(); toggle.click(); await flush();
  save.click(); await flush();
  assert.equal(portInput.value, '18888', 'existing custom port must be preserved');
  portInput.value = '0'; save.click(); await flush();
  toggle.click(); toggle.click(); await flush();
  assert.equal(portInput.value, '0', 'explicitly disabled port must stay disabled');
  const persistCheckbox = nodes.find(node => node.tagName === 'label' && node.textContent === '写入配置文件').children[0];
  const saveStatus = nodes.find(node => node.className === 'cpa-runtime-scheduler-status');
  assert.equal(persistCheckbox.checked, false);
  persistCheckbox.checked = true; save.click(); await flush();
  assert.equal(patch['persist-settings'], true);
  assert.match(saveStatus.textContent, /已应用并写入配置文件/);
  toggle.click(); toggle.click(); await flush();
  assert.equal(persistCheckbox.checked, true);
  failPersistence = true; save.click(); await flush();
  assert.match(saveStatus.textContent, /已应用，写入文件失败.*permission denied/);
  assert.equal(save.disabled, false);
  failPersistence = false;
  failRuntime = true; save.click(); await flush();
  assert.match(saveStatus.textContent, /静态 ISP 不可用.*binding directory unavailable/);
  failRuntime = false;
  persistCheckbox.checked = false; save.click(); await flush();
  assert.equal(patch['persist-settings'], false);
  assert.match(saveStatus.textContent, /仅当前进程/);
  assert.doesNotMatch(list.textContent, /292 剩余|332 剩余|采集/);
  assert.equal(list.children.length, 2);
  assert.equal((list.textContent.match(/account-a.json/g) || []).length, 1, 'one row per account');
  assert.match(list.children[0].textContent, /降级账号池.*ISP: isp-1/);
  assert.equal(list.children[0].children.find(node => node.className === 'cpa-account-state').children[1].attributes['aria-pressed'], 'true');
  account.degraded = false;
  timers.find(timer => timer.interval === 5000).callback(); await flush();
  assert.match(list.children[0].textContent, /状态正常.*ISP: isp-1/);
  assert.equal(list.children[0].children.find(node => node.className === 'cpa-account-state').children[0].attributes['aria-pressed'], 'true');
  failStatus = true;
  timers.find(timer => timer.interval === 5000).callback(); await flush();
  assert.match(list.textContent, /读取 ISP 状态失败/);
  assert.equal(list.children.length, 2, 'status errors must not remove account controls');
  failStatus = false; delayStatus = true;
  timers.find(timer => timer.interval === 5000).callback(); await flush();
  listeners.unauthorized();
  lateResolve(new Response(JSON.stringify({static_isp_bindings:[{auth_id:'account-a',node_id:'stale-isp'}]})));
  await flush();
  assert.equal(list.textContent, '未连接管理接口');
  assert.equal(cardControl(), null, 'logout must remove controls bound to the previous session');
  assert.equal(nativeToggle.classList.contains('cpa-native-status-hidden'), false);
  assert.equal(authCard.getAttribute('data-cpa-managed-account'), null);
  assert.equal(persistCheckbox.checked, false);
  assert.equal(fakeSection.open, false);
  assert.equal(rewriteSection.open, false);
  assert.equal(ispCheckbox.checked, false);
  assert.equal(nodes.find(node => node.className === 'cpa-static-isp-list').children.length, 0);
  assert.equal(nodes.find(node => node.className === 'cpa-fake-model-list').children.length, 0);
  console.log('account rows, static ISP, collapsed settings, fake models and logout checks passed');
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
