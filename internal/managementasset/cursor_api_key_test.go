package managementasset

import (
	"bytes"
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

// sameBacking reports whether both slices share the same backing array.
func sameBacking(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return &a[0] == &b[0]
}
