package managementasset

import (
	"bytes"
	"strings"
	"testing"
)

// fixtureManagementHTML 是一个逼近上游 bundle 结构的输入：拥有闭合的 </body>
// 注入点，足以触发 AddCursorAPIKeyInlineToManagementHTML 的注入。具体 OAuth
// 卡片结构由运行时的 findCursorCards 依据文案匹配，无需在此构造。
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

	// 注入点必须在 </body> 之前，且幂等标记必须出现。
	markerIdx := bytes.Index(got, []byte(cursorAPIKeyInlineMarker))
	if markerIdx < 0 {
		t.Fatalf("patched management html missing idempotency marker %q", cursorAPIKeyInlineMarker)
	}
	if bodyIdx := bytes.Index(got, []byte("</body>")); markerIdx > bodyIdx {
		t.Fatalf("inline script (%d) injected after </body> (%d)", markerIdx, bodyIdx)
	}

	// 不再注入独立页导航/路由痕迹（旧版 AddCursorAPIKeyManagerToManagementHTML 已删除）。
	for _, gone := range [][]byte{
		[]byte(`path:` + "\x60/cursor-api-key\x60"),
		[]byte("cpa-native-route-host"),
		[]byte("nav.cursor_api_key"),
	} {
		if bytes.Contains(got, gone) {
			t.Fatalf("patched management html still carries legacy standalone-page marker %q", gone)
		}
	}

	// 内联管理脚本的 CRUD 后端与关键 UI 文案必须存在。
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

	// 再次修补必须无副作用（幂等）。
	again, errAgain := AddCursorAPIKeyInlineToManagementHTML(got)
	if errAgain != nil {
		t.Fatalf("second AddCursorAPIKeyInlineToManagementHTML() error = %v", errAgain)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("Cursor API key inline patch is not idempotent")
	}
	// 幂等命中时输入缓冲被原样返回（不拷贝、不追加）。
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
		[]byte("estimated_cost_usd"),
		[]byte("预估费用"),
		[]byte("auto-disable-unauthorized"),
		[]byte("auto_disable_reason"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("runtime scheduler injection missing %q", want)
		}
	}
	again, errAgain := AddRuntimeSchedulerToManagementHTML(got)
	if errAgain != nil || !bytes.Equal(again, got) {
		t.Fatalf("runtime scheduler injection is not idempotent: err=%v", errAgain)
	}
}

// sameBacking 报告 b 是否为 a 的零拷贝切片（同一底层数组头地址）。
func sameBacking(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return &a[0] == &b[0]
}
