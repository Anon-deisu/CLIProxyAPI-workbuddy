package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"scheduler-mode":"remaining-quota","auto-disable-unauthorized":true,"auto-disable-quota":true,"codex-fingerprint-convergence":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutRoutingSettings(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if cfg.Routing.SchedulerMode != "remaining-quota" || !cfg.Routing.AutoDisableUnauthorized || !cfg.Routing.AutoDisableQuota || cfg.Routing.CodexFingerprintConvergence == nil || !*cfg.Routing.CodexFingerprintConvergence {
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

func TestWorkBuddyFingerprintRoutingSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const original = "# keep deployment settings\nport: 8317\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	h := NewHandler(cfg, path, nil)
	get := func(handler *Handler) bool {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		handler.GetRoutingSettings(ctx)
		var settings map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
			t.Fatal(err)
		}
		value, ok := settings["workbuddy-fingerprint-convergence"].(bool)
		if !ok {
			t.Fatal("WorkBuddy fingerprint setting is missing")
		}
		return value
	}
	patch := func(raw string) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(raw))
		h.PutRoutingSettings(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
		}
	}
	if get(h) {
		t.Fatal("convergence should default to off")
	}
	patch(`{"workbuddy-fingerprint-convergence":true}`)
	if !get(h) {
		t.Fatal("runtime enable failed")
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != original {
		t.Fatal("runtime-only change modified the file")
	}
	h.SetConfig(&config.Config{Debug: true})
	if !get(h) || cfg.Routing.CodexFingerprintConvergence != nil {
		t.Fatal("reload lost WorkBuddy state or changed Codex")
	}
	patch(`{"persist-settings":true}`)
	fresh, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !get(NewHandlerWithoutConfigFilePath(fresh, nil)) {
		t.Fatal("persisted convergence was not restored")
	}
	patch(`{"workbuddy-fingerprint-convergence":false}`)
	h.SetConfig(&config.Config{})
	if get(h) || !get(NewHandlerWithoutConfigFilePath(fresh, nil)) {
		t.Fatal("false reload was lost or previous config snapshot was mutated")
	}
	fresh, err = config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if get(NewHandlerWithoutConfigFilePath(fresh, nil)) {
		t.Fatal("explicit false did not persist")
	}
}

func TestWorkBuddyTaskRoutingSettings(t *testing.T) {
	for _, key := range []string{"workbuddy-auto-checkin", "workbuddy-auto-tasks"} {
		t.Run(key, func(t *testing.T) {
			path := writeTestConfigFile(t)
			h := NewHandler(&config.Config{}, path, nil)
			defer h.CloseWorkBuddyTasks()
			patch := func(body string, want int) {
				t.Helper()
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/routing/settings", strings.NewReader(body))
				h.PutRoutingSettings(ctx)
				if rec.Code != want {
					t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
				}
			}
			get := func() bool {
				t.Helper()
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				h.GetRoutingSettings(ctx)
				return ojson.GetBytes(rec.Body.Bytes(), key).Bool()
			}
			if get() {
				t.Fatal("automatic tasks must default to off")
			}
			patch(`{"`+key+`":true,"codex-routing-hint":"invalid"}`, http.StatusBadRequest)
			if get() {
				t.Fatal("invalid settings changed the task switch")
			}
			patch(`{"`+key+`":true,"workbuddy-checkin-interval-seconds":12}`, http.StatusOK)
			if !get() {
				t.Fatal("task switch was not enabled")
			}
			select {
			case <-h.workBuddyTasks.wake:
			default:
				t.Fatal("enabling tasks did not wake the task service")
			}
			h.SetConfig(&config.Config{})
			if !get() {
				t.Fatal("reload lost the task switch")
			}
			if h.cfg.Routing.WorkBuddyCheckinIntervalSeconds == nil || *h.cfg.Routing.WorkBuddyCheckinIntervalSeconds != 12 {
				t.Fatal("reload lost the checkin interval")
			}
			patch(`{"persist-settings":true}`, http.StatusOK)
			fresh, errLoad := config.LoadConfig(path)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			value := fresh.Routing.WorkBuddyAutoCheckin
			if key == "workbuddy-auto-tasks" {
				value = fresh.Routing.WorkBuddyAutoTasks
			}
			if value == nil || !*value {
				t.Fatal("task switch was not persisted")
			}
			if fresh.Routing.WorkBuddyCheckinIntervalSeconds == nil || *fresh.Routing.WorkBuddyCheckinIntervalSeconds != 12 {
				t.Fatal("checkin interval was not persisted")
			}
			patch(`{"workbuddy-checkin-interval-seconds":0}`, http.StatusBadRequest)
			patch(`{"workbuddy-checkin-interval-seconds":-1}`, http.StatusBadRequest)
			patch(`{"workbuddy-checkin-interval-seconds":86401}`, http.StatusBadRequest)
			patch(`{"`+key+`":false}`, http.StatusOK)
			h.SetConfig(&config.Config{})
			_, checkin, growth := h.workBuddyTasks.configuration()
			if get() || checkin || growth {
				t.Fatal("disabled tasks were re-enabled on reload")
			}
		})
	}
}

func TestPutRoutingSettingsFakeModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("port: 8317\n"), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg := &config.Config{}
	h := NewHandler(cfg, path, nil)
	patch := func(body string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(body))
		h.PutRoutingSettings(ctx)
		if rec.Code != want {
			t.Fatalf("PATCH %s: status = %d, want %d; body = %s", body, rec.Code, want, rec.Body.String())
		}
	}
	patch(`{"fake-models":[{"name":"fake-a","responses":["A","Alternative"],"input-tokens":114514,"output-tokens":1919810,"cache-read-tokens":325,"cache-write-tokens":799,"chars-per-second":25},{"name":"fake-b","content":"B","input-tokens":2}]}`, http.StatusOK)
	if len(cfg.Routing.FakeModels) != 2 || cfg.Routing.FakeModels[0].CacheWriteTokens != 799 || len(cfg.Routing.FakeModels[0].Responses) != 2 {
		t.Fatalf("runtime fake models = %#v", cfg.Routing.FakeModels)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing/settings", nil)
	h.GetRoutingSettings(ctx)
	var settings struct {
		FakeModels []config.FakeModelConfig `json:"fake-models"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &settings); errDecode != nil || !reflect.DeepEqual(settings.FakeModels, cfg.Routing.FakeModels) {
		t.Fatalf("GET fake models = %#v, decode error = %v", settings.FakeModels, errDecode)
	}
	patch(`{"persist-settings":true}`, http.StatusOK)
	fresh, errLoad := config.LoadConfig(path)
	if errLoad != nil || !reflect.DeepEqual(fresh.Routing.FakeModels, cfg.Routing.FakeModels) {
		t.Fatalf("fake models did not survive persisted reload: %v", errLoad)
	}
	patch(`{"fake-models":[{"name":"fake-a","content":"A"},{"name":"fake-a","content":"B"}]}`, http.StatusBadRequest)
	patch(`{"fake-models":[{"name":"fake-a","content":"A","output-tokens":-2}]}`, http.StatusBadRequest)
	patch(`{"fake-models":[{"name":"fake-a","responses":[]}]}`, http.StatusBadRequest)
	patch(`{"fake-models":[{"name":"fake-a","responses":["A"],"chars-per-second":-1}]}`, http.StatusBadRequest)
	patch(`{"fake-models":[{"name":"fake-a","responses":["A"],"chars-per-second":1.5}]}`, http.StatusBadRequest)
	patch(`{"fake-models":[{"name":"fake-a","responses":["A"," "]}]}`, http.StatusBadRequest)
	if len(cfg.Routing.FakeModels) != 2 {
		t.Fatal("invalid patch changed runtime fake models")
	}
	patch(`{"fake-models":[]}`, http.StatusOK)
	if len(cfg.Routing.FakeModels) != 0 {
		t.Fatal("fake models were not removed")
	}
	fresh, errLoad = config.LoadConfig(path)
	if errLoad != nil || len(fresh.Routing.FakeModels) != 0 {
		t.Fatalf("fake model removal did not survive persisted reload: %v", errLoad)
	}
}

func TestRoutingSettingsPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("# deployment comment\nport: 8317\napi-keys: [independent-key]\nrouting:\n  strategy: round-robin\n"), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := NewHandler(cfg, path, nil)
	h.SetDegradedPortController(func(int) error { return nil })
	h.SetExoFreePortController(func(int) error { return nil })
	patch := func(raw string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(raw))
		h.PutRoutingSettings(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH status=%d body=%s", rec.Code, rec.Body.String())
		}
		var result map[string]any
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &result); errDecode != nil {
			t.Fatal(errDecode)
		}
		return result
	}
	result := patch(`{"persist-settings":true,"degraded-port":9444,"exo-free-port":18888,"auth-concurrency-limit":2,"c2a-max-retry-count":5,"c2a-max-retry-interval":7,"c2a-max-retry-credentials":1,"auto-disable-unauthorized":true,"auto-disable-quota":true,"disable-cooling":true,"codex-fingerprint-convergence":true,"codex-routing-hint":"","cache-token-rewrite":{"enabled":true,"min-percent":0,"max-percent":95}}`)
	if result["persisted"] != true || result["runtime_only"] != false {
		t.Fatalf("save was not acknowledged: %v", result)
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(saved), "# deployment comment") || !strings.Contains(string(saved), "independent-key") {
		t.Fatal("saving scheduler settings changed unrelated configuration")
	}
	fresh, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if fresh.Routing.DegradedPort == nil || *fresh.Routing.DegradedPort != 9444 || fresh.Routing.ExoFreePort == nil || *fresh.Routing.ExoFreePort != 18888 || fresh.Routing.AuthConcurrencyLimit == nil || *fresh.Routing.AuthConcurrencyLimit != 2 || !fresh.Routing.AutoDisableQuota || !fresh.Routing.AutoDisableUnauthorized {
		t.Fatal("scheduler settings were not restored on fresh load")
	}
	if fresh.Routing.CodexRoutingHint == nil || *fresh.Routing.CodexRoutingHint != "" || fresh.CacheTokenRewrite == nil || fresh.CacheTokenRewrite.MinPercent == nil || *fresh.CacheTokenRewrite.MinPercent != 0 {
		t.Fatal("Codex or explicit zero/empty settings were not restored")
	}
	result = patch(`{"persist-settings":false,"auth-concurrency-limit":9}`)
	after, _ := os.ReadFile(path)
	if string(after) != string(saved) || result["persisted"] != false || result["runtime_only"] != true {
		t.Fatal("disabling persistence overwrote the last saved configuration")
	}
	if *h.cfg.Routing.AuthConcurrencyLimit != 9 || *h.cfg.Routing.PersistSettings {
		t.Fatal("disabling persistence did not apply the runtime-only changes")
	}
	h.SetConfig(fresh)
	if *h.cfg.Routing.AuthConcurrencyLimit != 2 || !*h.cfg.Routing.PersistSettings {
		t.Fatal("explicit file reload did not restore the last saved snapshot")
	}
	result = patch(`{"persist-settings":true,"degraded-port":0,"exo-free-port":0,"auth-concurrency-limit":0,"c2a-max-retry-count":-1,"disable-cooling":false}`)
	if result["persisted"] != true {
		t.Fatal("re-enabling persistence did not save")
	}
	fresh, errLoad = config.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if fresh.Routing.DegradedPort == nil || *fresh.Routing.DegradedPort != 0 || fresh.Routing.ExoFreePort == nil || *fresh.Routing.ExoFreePort != 0 || fresh.Routing.AuthConcurrencyLimit == nil || *fresh.Routing.AuthConcurrencyLimit != 0 || fresh.Routing.C2AMaxRetryCount != nil || *fresh.Routing.DisableCooling {
		t.Fatal("saving zero, false or reset overrides retained stale values")
	}
}

func TestRoutingSettingsPersistenceFailureIsReported(t *testing.T) {
	cfg := &config.Config{}
	h := NewHandler(cfg, filepath.Join(t.TempDir(), "missing", "config.yaml"), nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"persist-settings":true,"auth-concurrency-limit":3}`))
	h.PutRoutingSettings(ctx)
	if rec.Code != http.StatusInternalServerError || cfg.Routing.AuthConcurrencyLimit == nil || *cfg.Routing.AuthConcurrencyLimit != 3 {
		t.Fatalf("save failure did not report applied runtime settings: status=%d", rec.Code)
	}
	var result map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &result); errDecode != nil {
		t.Fatal(errDecode)
	}
	if result["applied"] != true || result["persisted"] != false || result["error"] == nil {
		t.Fatalf("ambiguous persistence failure: %v", result)
	}
}

func TestRoutingSettingsRemovedCodexFeatures(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	removed := map[string]any{
		"codex-turn-state-enabled":     true,
		"codex-auto-degrade":           true,
		"codex-excel-routing-enabled":  true,
		"codex-excel-disable-fallback": true,
		"codex-state-cliproxy":         map[string]any{"enabled": true, "gateway": "proxy.test:1080", "username": "test", "password": "test-secret"},
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	h.GetRoutingSettings(ctx)
	var settings map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &settings); errDecode != nil {
		t.Fatal(errDecode)
	}
	for key, value := range removed {
		t.Run(key, func(t *testing.T) {
			if _, exists := settings[key]; exists {
				t.Error("removed setting is still exposed")
			}
			body, errMarshal := json.Marshal(map[string]any{key: value})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(string(body)))
			h.PutRoutingSettings(ctx)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("removed setting was accepted: status=%d", rec.Code)
			}
		})
	}
}

func TestPutRoutingSettingsUpdatesCodexRoutingHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"codex-routing-hint":"asia-pacific"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutRoutingSettings(ctx)
	if recorder.Code != http.StatusOK || cfg.Routing.CodexRoutingHint == nil || *cfg.Routing.CodexRoutingHint != "asia-pacific" {
		t.Fatalf("status = %d, body=%s, routing = %+v", recorder.Code, recorder.Body.String(), cfg.Routing)
	}
}

func TestPutRoutingSettingsUpdatesDisableCooling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	credential := &coreauth.Auth{ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive}
	const model = "management-cooling-bypass-test"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
	if _, errRegister := manager.Register(t.Context(), credential); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager.MarkResult(t.Context(), coreauth.Result{AuthID: credential.ID, Provider: credential.Provider, Model: model, Error: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporary failure"}})
	if !reg.IsModelSuspendedForClient(credential.ID, model) {
		t.Fatal("expected the model to be cooling before the settings update")
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"disable-cooling":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutRoutingSettings(ctx)
	if recorder.Code != http.StatusOK || cfg.Routing.DisableCooling == nil || !*cfg.Routing.DisableCooling {
		t.Fatalf("status = %d, body=%s, routing = %+v", recorder.Code, recorder.Body.String(), cfg.Routing)
	}
	if reg.IsModelSuspendedForClient(credential.ID, model) || reg.IsModelQuotaExceededForClient(credential.ID, model) {
		t.Fatal("successful settings PATCH did not release the model registry cooldown")
	}
}

func TestRoutingSettingsCacheTokenRewriteRoundTripAndReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	for _, enabled := range []bool{true, false} {
		body, errMarshal := json.Marshal(map[string]any{"cache-token-rewrite": map[string]any{"enabled": enabled, "min-percent": 80, "max-percent": 95}})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(string(body)))
		h.PutRoutingSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PATCH rewrite=%t: status=%d body=%s", enabled, recorder.Code, recorder.Body.String())
		}
		h.SetConfig(&config.Config{})
		recorder = httptest.NewRecorder()
		ctx, _ = gin.CreateTestContext(recorder)
		h.GetRoutingSettings(ctx)
		var settings map[string]any
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &settings); errDecode != nil {
			t.Fatal(errDecode)
		}
		want := map[string]any{"enabled": enabled, "min-percent": float64(80), "max-percent": float64(95)}
		if !reflect.DeepEqual(settings["cache-token-rewrite"], want) {
			t.Fatalf("rewrite after reload = %v, want %v", settings["cache-token-rewrite"], want)
		}
		encoded, errMarshal := yaml.Marshal(h.cfg)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if strings.Contains(string(encoded), "cache-token-rewrite:") {
			t.Fatal("runtime cache token rewrite was persisted to YAML")
		}
	}
}

func TestRoutingSettingsCacheTokenRewriteValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	for _, raw := range []string{
		`{"enabled":true}`, `{"enabled":true,"min-percent":-1}`, `{"enabled":true,"min-percent":101}`,
		`{"enabled":true,"min-percent":90,"max-percent":80}`, `{"enabled":true,"max-percent":95}`,
		`{"enabled":true,"min-percent":"95"}`, `{"enabled":true,"min-percent":80,"max-percent":101}`,
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"auto-disable-quota":true,"cache-token-rewrite":`+raw+`}`))
		h.PutRoutingSettings(ctx)
		if recorder.Code != http.StatusBadRequest || h.cfg.Routing.AutoDisableQuota {
			t.Fatalf("invalid rewrite mutated config: status=%d, body=%s", recorder.Code, recorder.Body.String())
		}
	}
	for _, raw := range []string{`{"enabled":true,"min-percent":0}`, `{"enabled":true,"min-percent":95.5,"max-percent":null}`, `{"enabled":true,"min-percent":100}`, `{"enabled":false}`} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"cache-token-rewrite":`+raw+`}`))
		h.PutRoutingSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("valid rewrite rejected: status=%d, body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestPutRoutingSettingsCanDisableCodexFingerprintConvergence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enabled := true
	cfg := &config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: &enabled}}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"codex-fingerprint-convergence":false}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutRoutingSettings(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if cfg.Routing.CodexFingerprintConvergence == nil || *cfg.Routing.CodexFingerprintConvergence {
		t.Fatalf("convergence = %v, want explicit false", cfg.Routing.CodexFingerprintConvergence)
	}
	reloaded := &config.Config{Debug: true}
	h.SetConfig(reloaded)
	if h.cfg.Routing.CodexFingerprintConvergence == nil || *h.cfg.Routing.CodexFingerprintConvergence || h.cfg.Routing.AuthConcurrencyLimit != nil {
		t.Fatalf("routing after explicit false hot reload = %+v", h.cfg.Routing)
	}
	if reloaded.Routing.CodexFingerprintConvergence != nil || h.cfg.Routing.CodexFingerprintConvergence == cfg.Routing.CodexFingerprintConvergence {
		t.Fatal("hot reload mutated the caller or shares the previous convergence pointer")
	}
}

func TestGetRoutingSettingsReportsCodexFingerprintConvergenceStates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enabled := true
	disabled := false
	for _, test := range []struct {
		name  string
		value *bool
		want  bool
	}{
		{name: "unset", want: false},
		{name: "true", value: &enabled, want: true},
		{name: "false", value: &disabled, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Routing.CodexFingerprintConvergence = test.value
			h := NewHandlerWithoutConfigFilePath(cfg, nil)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)

			h.GetRoutingSettings(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			var settings map[string]any
			if errDecode := json.Unmarshal(recorder.Body.Bytes(), &settings); errDecode != nil {
				t.Fatal(errDecode)
			}
			if got, ok := settings["codex-fingerprint-convergence"].(bool); !ok || got != test.want {
				t.Fatalf("convergence = %#v, want %v", settings["codex-fingerprint-convergence"], test.want)
			}

			patchRecorder := httptest.NewRecorder()
			patchCtx, _ := gin.CreateTestContext(patchRecorder)
			patchCtx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"auto-disable-quota":true}`))
			h.PutRoutingSettings(patchCtx)
			if patchRecorder.Code != http.StatusOK || !cfg.Routing.AutoDisableQuota {
				t.Fatalf("PATCH status = %d, body=%s", patchRecorder.Code, patchRecorder.Body.String())
			}
			if cfg.Routing.CodexFingerprintConvergence != test.value || (test.value != nil && *test.value != test.want) {
				t.Fatal("PATCH changed omitted convergence setting")
			}
		})
	}
}

func TestSetConfigInheritsConvergenceWithoutSharingCaller(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit *int
	}{
		{name: "no-concurrency"},
		{name: "explicit-concurrency", limit: new(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			initial := &config.Config{}
			initial.Routing.CodexFingerprintConvergence = new(true)
			h := NewHandlerWithoutConfigFilePath(initial, nil)
			reloaded := &config.Config{Debug: true}
			reloaded.Routing.AuthConcurrencyLimit = test.limit
			reloaded.APIKeys = []string{"caller-key"}

			h.SetConfig(reloaded)

			if h.cfg == reloaded || h.cfg == initial || !h.cfg.Debug {
				t.Fatal("SetConfig() did not install an independent config with the reload fields")
			}
			if reloaded.Routing.CodexFingerprintConvergence != nil || reloaded.Routing.AuthConcurrencyLimit != test.limit {
				t.Fatal("SetConfig() mutated the caller config while inheriting convergence")
			}
			if h.cfg.Routing.CodexFingerprintConvergence == nil || !*h.cfg.Routing.CodexFingerprintConvergence {
				t.Fatalf("inherited convergence = %v, want true", h.cfg.Routing.CodexFingerprintConvergence)
			}
			if h.cfg.Routing.CodexFingerprintConvergence == initial.Routing.CodexFingerprintConvergence {
				t.Fatal("inherited convergence shares the previous config pointer")
			}
			if !reflect.DeepEqual(h.cfg.Routing.AuthConcurrencyLimit, test.limit) || (test.limit != nil && h.cfg.Routing.AuthConcurrencyLimit == test.limit) {
				t.Fatal("explicit concurrency was changed or shares the caller pointer")
			}
			*initial.Routing.CodexFingerprintConvergence = false
			reloaded.APIKeys[0] = "changed-key"
			if !*h.cfg.Routing.CodexFingerprintConvergence || h.cfg.APIKeys[0] != "caller-key" {
				t.Fatal("reload snapshot changed with the previous or caller config")
			}

			explicit := &config.Config{}
			explicit.Routing.CodexFingerprintConvergence = new(false)
			h.SetConfig(explicit)
			if h.cfg.Routing.CodexFingerprintConvergence == nil || *h.cfg.Routing.CodexFingerprintConvergence {
				t.Fatalf("explicit convergence = %v, want false", h.cfg.Routing.CodexFingerprintConvergence)
			}
			if h.cfg.Routing.CodexFingerprintConvergence == explicit.Routing.CodexFingerprintConvergence {
				t.Fatal("explicit convergence shares the caller config pointer")
			}
		})
	}
}

func TestPutRoutingSettingsRuntimeHookReceivesIndependentSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.APIKeys = []string{"caller-key"}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	var snapshots []*config.Config
	h.SetRuntimeConfigHook(func(_ context.Context, got *config.Config) {
		snapshots = append(snapshots, got)
	})
	for index, enabled := range []bool{true, false} {
		body, errMarshal := json.Marshal(map[string]any{"auth-concurrency-limit": 2, "codex-fingerprint-convergence": enabled})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(string(body)))
		h.PutRoutingSettings(ctx)
		if recorder.Code != http.StatusOK || len(snapshots) != index+1 {
			t.Fatalf("status=%d snapshots=%d body=%s", recorder.Code, len(snapshots), recorder.Body.String())
		}
		snapshot := snapshots[index]
		if snapshot == nil || snapshot == cfg || snapshot == h.cfg || h.cfg != cfg {
			t.Fatal("runtime hook did not receive an independent snapshot of the active caller config")
		}
		if cfg.Routing.CodexFingerprintConvergence == nil || *cfg.Routing.CodexFingerprintConvergence != enabled || !reflect.DeepEqual(snapshot, cfg) {
			t.Fatal("caller config and runtime hook snapshot do not contain the requested settings")
		}
		if snapshot.Routing.AuthConcurrencyLimit == cfg.Routing.AuthConcurrencyLimit || snapshot.Routing.CodexFingerprintConvergence == cfg.Routing.CodexFingerprintConvergence {
			t.Fatal("runtime hook snapshot shares runtime field pointers")
		}
	}
	if !*snapshots[0].Routing.CodexFingerprintConvergence || *snapshots[1].Routing.CodexFingerprintConvergence {
		t.Fatal("a later runtime update changed an earlier hook snapshot")
	}
	*snapshots[1].Routing.CodexFingerprintConvergence = true
	*snapshots[1].Routing.AuthConcurrencyLimit = 9
	snapshots[1].APIKeys[0] = "hook-key"
	if *cfg.Routing.CodexFingerprintConvergence || *cfg.Routing.AuthConcurrencyLimit != 2 || cfg.APIKeys[0] != "caller-key" {
		t.Fatal("mutating the runtime hook snapshot changed the active config")
	}
}

func TestPutConfigYAMLPreservesRuntimeRoutingFieldsWithoutPersisting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, state := range []struct {
		name  string
		value *bool
	}{
		{name: "unset"},
		{name: "true", value: new(true)},
		{name: "false", value: new(false)},
	} {
		t.Run(state.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Routing.AuthConcurrencyLimit = new(2)
			cfg.Routing.CodexFingerprintConvergence = state.value
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			h := NewHandler(cfg, configPath, nil)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", strings.NewReader("debug: true\n"))
			h.PutConfigYAML(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if !h.cfg.Debug || h.cfg.Routing.AuthConcurrencyLimit == nil || *h.cfg.Routing.AuthConcurrencyLimit != 2 || !reflect.DeepEqual(h.cfg.Routing.CodexFingerprintConvergence, state.value) {
				t.Fatalf("runtime fields after YAML reload = %+v", h.cfg.Routing)
			}
			if h.cfg == cfg || h.cfg.Routing.AuthConcurrencyLimit == cfg.Routing.AuthConcurrencyLimit || (state.value != nil && h.cfg.Routing.CodexFingerprintConvergence == state.value) {
				t.Fatal("YAML reload shares the original config or runtime field pointers")
			}
			if errSave := config.SaveConfigPreserveComments(configPath, h.cfg); errSave != nil {
				t.Fatal(errSave)
			}
			persisted, errRead := os.ReadFile(configPath)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if strings.Contains(string(persisted), "auth-concurrency-limit") || strings.Contains(string(persisted), "codex-fingerprint-convergence") {
				t.Fatalf("runtime routing fields persisted to YAML: %s", persisted)
			}
			reloaded, errLoad := config.LoadConfig(configPath)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if reloaded.Routing.AuthConcurrencyLimit != nil || reloaded.Routing.CodexFingerprintConvergence != nil {
				t.Fatal("fresh config loaded runtime routing fields from YAML")
			}
			h.SetConfig(reloaded)
			if h.cfg.Routing.AuthConcurrencyLimit == nil || *h.cfg.Routing.AuthConcurrencyLimit != 2 || !reflect.DeepEqual(h.cfg.Routing.CodexFingerprintConvergence, state.value) {
				t.Fatalf("runtime fields after watcher reload = %+v", h.cfg.Routing)
			}
			if reloaded.Routing.AuthConcurrencyLimit != nil || reloaded.Routing.CodexFingerprintConvergence != nil {
				t.Fatal("watcher reload changed the caller config")
			}
		})
	}
}

func TestPutRoutingSettingsUpdatesC2ARetryOverridesInMemory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RequestRetry: 2, MaxRetryInterval: 30}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(`{"c2a-max-retry-count":5,"c2a-max-retry-interval":7,"c2a-max-retry-credentials":1}`))
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
	if cfg.Routing.C2AMaxRetryCredentials == nil || *cfg.Routing.C2AMaxRetryCredentials != 1 {
		t.Fatalf("c2a max retry credentials = %v, want 1", cfg.Routing.C2AMaxRetryCredentials)
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

func TestRoutingConcurrencySurvivesConfigReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reload := range []string{"watcher", "yaml-save"} {
		t.Run(reload, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			auth, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "existing", Provider: "codex"})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			cfg := &config.Config{}
			h := NewHandler(cfg, filepath.Join(t.TempDir(), "config.yaml"), manager)
			for _, limit := range []int{1, 0} {
				body, errMarshal := json.Marshal(map[string]int{"auth-concurrency-limit": limit})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/routing/settings", strings.NewReader(string(body)))
				h.PutRoutingSettings(ctx)
				if recorder.Code != http.StatusOK {
					t.Fatal(recorder.Body.String())
				}
				if reload == "watcher" {
					reloaded, errParse := config.ParseConfigBytes([]byte("debug: true\n"))
					if errParse != nil {
						t.Fatal(errParse)
					}
					manager.SetConfig(reloaded)
					h.SetConfig(reloaded)
				} else {
					saveRecorder := httptest.NewRecorder()
					saveCtx, _ := gin.CreateTestContext(saveRecorder)
					saveCtx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", strings.NewReader("debug: true\n"))
					h.PutConfigYAML(saveCtx)
					if saveRecorder.Code != http.StatusOK {
						t.Fatal(saveRecorder.Body.String())
					}
					persisted, errRead := os.ReadFile(h.configFilePath)
					if errRead != nil || strings.Contains(string(persisted), "auth-concurrency-limit") {
						t.Fatalf("runtime limit persisted: error=%v data=%s", errRead, persisted)
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
					t.Fatalf("after %s: settings limit=%v, want %d", reload, settings["auth-concurrency-limit"], limit)
				}
				newAuth, errNew := manager.Register(context.Background(), &coreauth.Auth{ID: "new", Provider: "codex"})
				if errNew != nil {
					t.Fatal(errNew)
				}
				if auth.EffectiveConcurrencyLimit() != limit || newAuth.EffectiveConcurrencyLimit() != limit {
					t.Fatalf("after reload: existing=%d new=%d, want %d", auth.EffectiveConcurrencyLimit(), newAuth.EffectiveConcurrencyLimit(), limit)
				}
			}
		})
	}
}
