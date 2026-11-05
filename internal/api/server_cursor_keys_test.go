package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cursorstore "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
)

func TestCursorAPIKeyManagementRouteMasksSecret(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)

	// API key 凭据改为从 JSON 存储加载（config.yaml 已彻底废弃 cursor-api-key 污染），
	// 这里通过 store 写入一条记录来模拟已保存的凭据。
	store := cursorstore.NewAPIKeyStore(server.cfg.AuthDir)
	if _, err := store.Create(&cursorstore.APIKeyStorage{
		Type:   cursorstore.StoredAPIKeyType,
		APIKey: "crsr_server-route-secret",
		Prefix: "team",
	}); err != nil {
		t.Fatalf("seed cursor api key: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/cursor-api-key", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "crsr_server-route-secret") {
		t.Fatalf("management route leaked raw Cursor API key: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "crsr_********cret") {
		t.Fatalf("management route did not return expected mask: %s", rec.Body.String())
	}
}

// 管理面板的 Cursor API Key 管理界面已按需求从「漂浮按钮」重构为
// 「卡片内嵌输入框」：服务端在 #/oauth 的 Cursor OAuth 卡片内注入内联 API
// Key 输入框与管理脚本（幂等），不再注入独立导航/路由；同时管理页关闭缓存，
// 避免刷新时拿到旧版本。
func TestManagementPageIntegratesCursorEntryAndDisablesCaching(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	backtick := "\x60"
	fixture := []byte("<!doctype html><html><body>" +
		"<nav>{gateway:[{path:" + backtick + "/oauth" + backtick +
		",labelKey:" + backtick + "nav.oauth" + backtick + ",icon:K9.oauth}]}</nav>" +
		"<v>{path:" + backtick + "/oauth" + backtick + ",element:(0,H.jsx)(VM,{})}</v>" +
		"<z>oauth:" + backtick + "OAuth Login" + backtick + ",quota_management:" + backtick + "Quota" + backtick + "</z>" +
		"</body></html>")
	if errWrite := os.WriteFile(
		filepath.Join(staticDir, "management.html"),
		fixture,
		0o600,
	); errWrite != nil {
		t.Fatalf("write management fixture: %v", errWrite)
	}
	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		// 内联管理脚本（非漂浮按钮）
		`id="cpa-key-inline-script"`,
		// 卡片内嵌输入框文案
		`Cursor API Key`,
		// 管理脚本（GET/POST/PATCH/DELETE 复用同一端点）
		`managementPath + 'cursor-api-key'`,

		// 运行时事件捕获
		`MutationObserver`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("management page missing native Cursor integration %q", want)
		}
	}
	if strings.Contains(rec.Body.String(), ":host{position:fixed;inset:0;z-index:2147482999") {
		t.Fatal("management page still contains the floating Cursor button (expected native integration)")
	}
	// 幂等：重复请求不应重复注入导航/路由/脚本。
	rec2 := httptest.NewRecorder()
	server.engine.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/management.html", nil))
	if count := strings.Count(rec2.Body.String(), `id="cpa-key-inline-script"`); count != 1 {
		t.Fatalf("inline script injected %d times, want 1 (idempotency broken)", count)
	}

	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
}
