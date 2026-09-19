package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestAccountSchedulingStatePersists(t *testing.T) {
	store := sdkauth.NewFileTokenStore()
	dir := t.TempDir()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(t.Context(), &coreauth.Auth{ID: "account.json", FileName: "account.json", Provider: "codex", Attributes: map[string]string{"path": filepath.Join(dir, "account.json")}, Metadata: map[string]any{"type": "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	for _, state := range []string{"degraded", "disabled", "normal", "degraded"} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"account.json","scheduling_state":"`+state+`"}`))
		h.PatchAuthFileStatus(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("state %s: %d %s", state, rec.Code, rec.Body.String())
		}
		loaded, err := store.List(t.Context())
		if err != nil || len(loaded) != 1 {
			t.Fatalf("reload: %v", err)
		}
		degraded, _ := loaded[0].Metadata["degraded"].(bool)
		if degraded != (state == "degraded") || loaded[0].Disabled != (state == "disabled") {
			t.Fatalf("persisted wrong scheduling state for %s", state)
		}
		auth, _ := manager.GetByID("account.json")
		entry := h.buildAuthFileEntry(auth)
		if entry["scheduling_state"] != state {
			t.Fatalf("entry state=%v", entry["scheduling_state"])
		}
	}
}

func TestDegradedPortSettingsAreRuntimeOnly(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	port := 0
	h.SetDegradedPortController(func(value int) error { port = value; return nil })
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"degraded-port":18888}`))
	h.PutRoutingSettings(ctx)
	if rec.Code != http.StatusOK || port != 18888 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	h.SetConfig(&config.Config{})
	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	h.GetRoutingSettings(ctx)
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if payload["degraded-port"] != float64(port) {
		t.Fatal("port lost after reload")
	}
	encoded, _ := yaml.Marshal(h.cfg)
	if strings.Contains(string(encoded), "degraded-port") {
		t.Fatal("runtime port leaked into config.yaml")
	}
}

func TestDegradedPortFailureDoesNotMutateSettings(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetDegradedPortController(func(int) error { return errors.New("port occupied") })
	for _, body := range []string{`{"degraded-port":18888,"auto-disable-quota":true}`, `{"degraded-port":-1}`, `{"degraded-port":65536}`} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(body))
		h.PutRoutingSettings(ctx)
		if rec.Code != http.StatusBadRequest || h.cfg.Routing.AutoDisableQuota || h.cfg.Routing.DegradedPort != nil {
			t.Fatal("failed port update mutated runtime settings")
		}
	}
}
