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
		[]byte("auth-concurrency-limit"),
		[]byte("codex-fingerprint-convergence"),
		[]byte("QuotaCard-module__body"),
		[]byte("estimated_cost_usd"),
		[]byte("预估费用"),
		[]byte("auto-disable-unauthorized"),
		[]byte("auto_disable_reason"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("runtime scheduler injection missing %q", want)
		}
	}
	for _, removed := range []string{"cpa-runtime-scheduler-account-estimate", "cpa-runtime-scheduler-account-concurrency", "auth-files/fields", "formatQuota"} {
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
  querySelectorAll: () => cards,
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
