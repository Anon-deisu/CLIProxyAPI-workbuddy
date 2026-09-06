package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
