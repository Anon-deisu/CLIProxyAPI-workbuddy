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
