package managementasset

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAddCursorOAuthToManagementHTML(t *testing.T) {
	input := []byte("<script>" +
		"var MM=[{kind:`builtin`,id:`codex`,titleKey:`auth_login.codex_oauth_title`,icon:Nx}," +
		"{kind:`builtin`,id:`xai`,titleKey:`auth_login.xai_oauth_title`,icon:{light:Gx,dark:Kx}}]," +
		"Aee=new Set(MM.map(e=>e.id)),jee=new Set([`codex`,`anthropic`,`antigravity`,`xai`]);" +
		"const zh={xai_oauth_polling_error:`检查认证状态失败:`,plugin_oauth_title:`{{name}} OAuth`};" +
		"</script>")

	got, err := AddCursorOAuthToManagementHTML(input)
	if err != nil {
		t.Fatalf("AddCursorOAuthToManagementHTML() error = %v", err)
	}
	for _, want := range [][]byte{
		[]byte("id:`cursor`,titleKey:`auth_login.cursor_oauth_title`"),
		[]byte("cursor_oauth_button:`开始 Cursor 登录`"),
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("patched management html missing %q", want)
		}
	}

	again, err := AddCursorOAuthToManagementHTML(got)
	if err != nil {
		t.Fatalf("second AddCursorOAuthToManagementHTML() error = %v", err)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("Cursor OAuth management patch is not idempotent")
	}
}

func TestAddCursorOAuthToLatestManagementHTML(t *testing.T) {
	path := os.Getenv("CURSOR_MANAGEMENT_HTML_FIXTURE")
	if path == "" {
		t.Skip("CURSOR_MANAGEMENT_HTML_FIXTURE is not set")
	}
	input, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read management fixture: %v", err)
	}
	got, err := AddCursorOAuthToManagementHTML(input)
	if err != nil {
		t.Fatalf("patch latest management fixture: %v", err)
	}
	if !bytes.Contains(got, []byte("id:`cursor`,titleKey:`auth_login.cursor_oauth_title`")) {
		t.Fatal("latest management fixture does not expose Cursor OAuth")
	}
	if !bytes.Contains(got, []byte("cursor_oauth_button:`开始 Cursor 登录`")) {
		t.Fatal("latest management fixture does not contain the Chinese Cursor OAuth action")
	}
	if bytes.Count(got, []byte("cursor_oauth_title:`Cursor OAuth`")) < 4 {
		t.Fatal("latest management fixture does not provide Cursor OAuth labels for every locale")
	}
}
