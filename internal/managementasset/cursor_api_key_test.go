package managementasset

import (
	"bytes"
	"strings"
	"testing"
)

// fixtureManagementHTML 是一个逼近上游 bundle 关键结构的输入：拥有网关分类的
// nav 项（icon:K9.oauth）、/oauth 路由（(0,H.jsx)(VM,{})）、以及两条 locale 锚点
// （oauth:`OAuth Login`,quota_management: 与 oauth:`OAuth authorization`,quota_management:），
// 足以触发本函数的四处修补。反引号用 \x60 转义。
func fixtureManagementHTML() []byte {
	t := "\x60"
	return []byte("<!doctype html>\n" +
		"<html>\n<head><title>management</title></head>\n<body>\n" +
		"<nav>{gateway:[{path:" + t + "/oauth" + t + ",labelKey:" + t + "nav.oauth" + t + ",metaKey:" + t + "nav_meta.oauth" + t + ",icon:K9.oauth}]}</nav>\n" +
		"<v>{path:" + t + "/oauth" + t + ",element:(0,H.jsx)(VM,{})}</v>\n" +
		"<z>{oauth:" + t + "OAuth Login" + t + ",quota_management:" + t + "Quota" + t + "}</z>\n" +
		"<z>{oauth:" + t + "OAuth authorization" + t + ",quota_management:" + t + "Quota" + t + "}</z>\n" +
		"</body>\n</html>")
}

func TestAddCursorAPIKeyManagerToManagementHTML(t *testing.T) {
	t.Parallel()

	got, errPatch := AddCursorAPIKeyManagerToManagementHTML(fixtureManagementHTML())
	if errPatch != nil {
		t.Fatalf("AddCursorAPIKeyManagerToManagementHTML() error = %v", errPatch)
	}
	for _, want := range [][]byte{
		// 幂等标记
		[]byte(cursorAPIKeyManagerMarker),
		// 导航项 + 导航文案（nav 与 nav_meta）
		[]byte(",{path:`/cursor-api-key`,labelKey:`nav.cursor_api_key`,metaKey:`nav_meta.cursor_api_key`,icon:K9.oauth}"),
		[]byte("cursor_api_key:`Cursor API Key`,"),
		// 原生路由挂载点（非外挂按钮）
		[]byte(",{path:`/cursor-api-key`,element:(0,H.jsx)(\"div\",{id:\"cpa-native-route-host\"})}"),
		[]byte(`id:"cpa-native-route-host"`),
		// 管理脚本的关键后端逻辑
		[]byte("/v0/management/cursor-api-key"),
		[]byte("留空表示不更换现有 Key"),
		[]byte("entry.last_error"),
		[]byte("MutationObserver"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("patched management html missing %q", want)
		}
	}

	// 必须不再存在旧版紫色悬浮按钮的痕迹
	for _, gone := range [][]byte{
		[]byte(":host{position:fixed;inset:0;z-index:2147482999"),
		[]byte("#open"),
		[]byte("openButton.classList.toggle('hidden'"),
	} {
		if bytes.Contains(got, gone) {
			t.Fatalf("patched management html still carries legacy floating-button marker %q", gone)
		}
	}

	// 脚本注入点在 </body> 之前
	if bytes.Index(got, []byte(cursorAPIKeyManagerMarker)) > bytes.Index(got, []byte("</body>")) {
		t.Fatal("Cursor API key manager was not injected inside body")
	}

	// 再次修补必须无副作用（幂等）
	again, errAgain := AddCursorAPIKeyManagerToManagementHTML(got)
	if errAgain != nil {
		t.Fatalf("second AddCursorAPIKeyManagerToManagementHTML() error = %v", errAgain)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("Cursor API key management patch is not idempotent")
	}
}

func TestAddCursorAPIKeyManagerNoAnchorsReturnsError(t *testing.T) {
	t.Parallel()

	_, errPatch := AddCursorAPIKeyManagerToManagementHTML([]byte("<html>management</html>"))
	if errPatch == nil {
		t.Fatal("expected error when nav/route/locale anchors are absent, got nil")
	}
	if !strings.Contains(errPatch.Error(), "anchor not found") {
		t.Fatalf("unexpected error text: %v", errPatch)
	}
}

func TestAddCursorAPIKeyManagerEmptyInput(t *testing.T) {
	t.Parallel()

	if _, errPatch := AddCursorAPIKeyManagerToManagementHTML(nil); errPatch == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}
