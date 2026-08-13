package managementasset

import (
	"bytes"
	"testing"
)

func TestAddCursorAPIKeyManagerToManagementHTML(t *testing.T) {
	t.Parallel()

	input := []byte("<!doctype html><html><body><main>management</main></body></html>")
	got, errPatch := AddCursorAPIKeyManagerToManagementHTML(input)
	if errPatch != nil {
		t.Fatalf("AddCursorAPIKeyManagerToManagementHTML() error = %v", errPatch)
	}
	for _, want := range [][]byte{
		[]byte(cursorAPIKeyManagerMarker),
		[]byte("管理 Cursor API Key"),
		[]byte("/v0/management/cursor-api-key"),
		[]byte("留空表示不更换现有 Key"),
		[]byte("与 Cursor OAuth 共用模型"),
		[]byte("最近错误"),
		[]byte("entry.last_error"),
		[]byte(":host{position:fixed;inset:0;z-index:2147482999"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("patched management html missing %q", want)
		}
	}
	if bytes.Contains(got, []byte("openButton.classList.toggle('hidden'")) {
		t.Fatal("Cursor API key entry must remain visible on the login page")
	}
	if bytes.Index(got, []byte(cursorAPIKeyManagerMarker)) > bytes.Index(got, []byte("</body>")) {
		t.Fatal("Cursor API key manager was not injected inside body")
	}

	again, errAgain := AddCursorAPIKeyManagerToManagementHTML(got)
	if errAgain != nil {
		t.Fatalf("second AddCursorAPIKeyManagerToManagementHTML() error = %v", errAgain)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("Cursor API key management patch is not idempotent")
	}
}

func TestAddCursorAPIKeyManagerWithoutBodyFallsBackBeforeHTML(t *testing.T) {
	t.Parallel()

	got, errPatch := AddCursorAPIKeyManagerToManagementHTML([]byte("<html>management</html>"))
	if errPatch != nil {
		t.Fatalf("AddCursorAPIKeyManagerToManagementHTML() error = %v", errPatch)
	}
	markerIndex := bytes.Index(got, []byte(cursorAPIKeyManagerMarker))
	htmlEndIndex := bytes.Index(got, []byte("</html>"))
	if markerIndex < 0 || htmlEndIndex < 0 || markerIndex > htmlEndIndex {
		t.Fatalf("manager marker index=%d, html end index=%d", markerIndex, htmlEndIndex)
	}
}
