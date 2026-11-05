package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexStaticISPManagementPersistenceAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("# preserve deployment\nport: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil)
	call := func(method string, handler func(*gin.Context), body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(method, "/v0/management/config", strings.NewReader(body))
		handler(ctx)
		return rec
	}
	patch := `{"persist-settings":true,"codex-static-isp":{"enabled":true,"nodes":[{"id":"a","scheme":"http","host":"proxy.test","port":8080,"username":"u","password":"static-test-secret"}]}}`
	if rec := call(http.MethodPatch, h.PutRoutingSettings, patch); rec.Code != http.StatusOK {
		t.Fatalf("static ISP patch failed: %d %s", rec.Code, rec.Body.String())
	}
	fresh, err := config.LoadConfig(path)
	if err != nil || fresh.Routing.CodexStaticISP == nil || fresh.Routing.CodexStaticISP.Nodes[0].Password != "static-test-secret" {
		t.Fatal("static ISP configuration was not persisted")
	}
	for _, handler := range []func(*gin.Context){h.GetRoutingSettings, h.GetConfig, h.GetConfigYAML} {
		rec := call(http.MethodGet, handler, "")
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "static-test-secret") {
			t.Fatal("management response leaked static ISP password or failed")
		}
	}
	yamlView := call(http.MethodGet, h.GetConfigYAML, "").Body.String()
	if !strings.Contains(yamlView, "# preserve deployment") {
		t.Fatal("YAML redaction removed unrelated comments")
	}
	if rec := call(http.MethodPut, h.PutConfigYAML, yamlView); rec.Code != http.StatusOK {
		t.Fatalf("redacted YAML could not round trip: %s", rec.Body.String())
	}
	fresh, err = config.LoadConfig(path)
	if err != nil || fresh.Routing.CodexStaticISP.Nodes[0].Password != "static-test-secret" {
		t.Fatal("redacted YAML replaced the real password")
	}
	patch = `{"codex-static-isp":{"nodes":[{"id":"a","host":"other.test","port":1080,"username":"u"}]}}`
	if rec := call(http.MethodPatch, h.PutRoutingSettings, patch); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if h.cfg.Routing.CodexStaticISP.Nodes[0].Password != "static-test-secret" {
		t.Fatal("omitted password was not retained by node ID")
	}
	before := h.cfg.CloneForRuntime()
	if rec := call(http.MethodPatch, h.PutRoutingSettings, `{"auth-concurrency-limit":99,"codex-static-isp":{"nodes":[{"id":"new","host":"proxy.test","port":1080,"username":"u"}]}}`); rec.Code != http.StatusBadRequest {
		t.Fatal("new node without password accepted")
	}
	if h.cfg.Routing.AuthConcurrencyLimit != before.Routing.AuthConcurrencyLimit || h.cfg.Routing.CodexStaticISP.Nodes[0].ID != "a" {
		t.Fatal("invalid static ISP patch partially applied")
	}
}

func TestCodexStaticISPYAMLRedactsMergedPasswords(t *testing.T) {
	raw := []byte("defaults: &defaults\n  scheme: http\n  host: proxy.test\n  port: 8080\n  username: u\n  password: merged-secret\naccount-scheduling:\n  codex-static-isp:\n    enabled: true\n    nodes:\n      - <<: *defaults\n        id: a\n")
	redacted, err := rewriteStaticISPPasswords(raw, nil, true)
	if err != nil || strings.Contains(string(redacted), "merged-secret") {
		t.Fatal("merged YAML password leaked")
	}
	previous := &config.CodexStaticISPConfig{Nodes: []config.StaticISPNode{{ID: "a", Password: "merged-secret"}}}
	restored, err := rewriteStaticISPPasswords(redacted, previous, false)
	if err != nil || !strings.Contains(string(restored), "merged-secret") {
		t.Fatal("merged YAML password could not round trip")
	}
}

func TestCodexStaticISPRuntimeApplyFailureIsReported(t *testing.T) {
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	keeper := helps.NewCodexEgressManager(manager.List)
	t.Cleanup(keeper.Close)
	parent := filepath.Join(t.TempDir(), "runtime")
	if err := keeper.SetStaticISPBindingFile(filepath.Join(parent, "bindings.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	executor := runtimeexecutor.NewCodexAutoExecutor(cfg)
	executor.SetEgressManager(keeper)
	manager.RegisterExecutor(executor)
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	h.authManager = manager
	h.SetRuntimeConfigHook(func(_ context.Context, snapshot *config.Config) { _ = keeper.Configure(snapshot) })
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"codex-static-isp":{"enabled":true,"nodes":[{"id":"a","host":"proxy.test","port":1080,"username":"u","password":"synthetic"}]}}`))
	h.PutRoutingSettings(ctx)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"runtime_error":true`) {
		t.Fatalf("runtime failure reported as successful apply: %d %s", rec.Code, rec.Body.String())
	}
}
