package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCursorAPIKeyManagementRouteMasksSecret(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	server.cfg.CursorKey = []proxyconfig.CursorKey{{
		APIKey: "crsr_server-route-secret",
		Prefix: "team",
	}}

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

func TestManagementPageAlwaysShowsCursorEntryAndDisablesCaching(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	if errWrite := os.WriteFile(
		filepath.Join(staticDir, "management.html"),
		[]byte("<!doctype html><html><body>management</body></html>"),
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
	if !strings.Contains(rec.Body.String(), "管理 Cursor API Key") {
		t.Fatal("management page does not contain the always-visible Cursor entry")
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
}
