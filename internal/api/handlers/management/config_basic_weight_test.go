package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyRemainingQuota(t *testing.T) {
	for _, input := range []string{"remaining-quota", "remaining_quota", "remainingquota", "quota"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "remaining-quota" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want remaining-quota, true", input, got, ok)
		}
	}
}

func TestPutRoutingSettingsUpdatesMemoryOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"scheduler-mode":"remaining-quota","auto-disable-unauthorized":true,"auto-disable-quota":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutRoutingSettings(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if cfg.Routing.SchedulerMode != "remaining-quota" || !cfg.Routing.AutoDisableUnauthorized || !cfg.Routing.AutoDisableQuota {
		t.Fatalf("runtime routing config = %+v", cfg.Routing)
	}
	var response map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response["runtime_only"] != true {
		t.Fatalf("response = %#v, want runtime_only=true", response)
	}
}

func TestPutRoutingSettingsUpdatesC2ARetryOverridesInMemory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RequestRetry: 2, MaxRetryInterval: 30}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"c2a-max-retry-count":5,"c2a-max-retry-interval":7}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutRoutingSettings(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if cfg.Routing.C2AMaxRetryCount == nil || *cfg.Routing.C2AMaxRetryCount != 5 {
		t.Fatalf("c2a max retry count = %v, want 5", cfg.Routing.C2AMaxRetryCount)
	}
	if cfg.Routing.C2AMaxRetryInterval == nil || *cfg.Routing.C2AMaxRetryInterval != 7 {
		t.Fatalf("c2a max retry interval = %v, want 7", cfg.Routing.C2AMaxRetryInterval)
	}
	if cfg.RequestRetry != 2 || cfg.MaxRetryInterval != 30 {
		t.Fatalf("global retry settings changed: request=%d interval=%d", cfg.RequestRetry, cfg.MaxRetryInterval)
	}
}

func TestPutRoutingSettingsAppliesConcurrencyToAllAccounts(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"first", "second"} {
		if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex"}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	cfg := &config.Config{}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	for _, limit := range []int{2, 1, 0} {
		body, errMarshal := json.Marshal(map[string]int{"auth-concurrency-limit": limit})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(string(body)))
		h.PutRoutingSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("set uniform limit %d: status=%d body=%s", limit, recorder.Code, recorder.Body.String())
		}
		for _, auth := range manager.List() {
			if got := auth.EffectiveConcurrencyLimit(); got != limit {
				t.Fatalf("auth %s limit=%d, want uniform limit=%d", auth.ID, got, limit)
			}
		}
		if limit == 2 {
			if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "new-account", Provider: "codex"}); errRegister != nil {
				t.Fatal(errRegister)
			}
			newAuth, ok := manager.GetByID("new-account")
			if !ok || newAuth.EffectiveConcurrencyLimit() != limit {
				t.Fatalf("new account limit = %d, want uniform limit=%d", newAuth.EffectiveConcurrencyLimit(), limit)
			}
		}
		getRecorder := httptest.NewRecorder()
		getCtx, _ := gin.CreateTestContext(getRecorder)
		h.GetRoutingSettings(getCtx)
		var settings map[string]any
		if errDecode := json.Unmarshal(getRecorder.Body.Bytes(), &settings); errDecode != nil {
			t.Fatal(errDecode)
		}
		if settings["auth-concurrency-limit"] != float64(limit) {
			t.Fatalf("GET uniform limit = %v, want %d", settings["auth-concurrency-limit"], limit)
		}
		data, errYAML := yaml.Marshal(cfg)
		if errYAML != nil {
			t.Fatal(errYAML)
		}
		if strings.Contains(string(data), "auth-concurrency-limit") {
			t.Fatal("runtime concurrency limit leaked into YAML")
		}
	}
}

func TestPutRoutingSettingsRejectsInvalidConcurrency(t *testing.T) {
	for _, value := range []string{"-1", "1000001", "1.5", `"2"`} {
		h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"auth-concurrency-limit":`+value+`,"auto-disable-quota":true}`))
		h.PutRoutingSettings(ctx)
		if recorder.Code != http.StatusBadRequest || h.cfg.Routing.AutoDisableQuota {
			t.Fatalf("invalid limit %s: status=%d quota=%v", value, recorder.Code, h.cfg.Routing.AutoDisableQuota)
		}
	}
}
