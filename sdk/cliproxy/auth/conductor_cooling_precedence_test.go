package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagerMarkResultUsesCredentialCoolingPrecedence(t *testing.T) {
	previousGlobal := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousGlobal) })

	disabled := true
	enabled := false
	tests := []struct {
		name             string
		homeEnabled      bool
		globalDisable    bool
		credential       *bool
		providerOverride *bool
		wantCooldown     bool
	}{
		{name: "credential true overrides global false", credential: &disabled},
		{name: "credential false overrides global true", globalDisable: true, credential: &enabled, wantCooldown: true},
		{name: "unset inherits global true", globalDisable: true},
		{name: "unset inherits global false", wantCooldown: true},
		{name: "provider false overrides global true", globalDisable: true, providerOverride: &enabled, wantCooldown: true},
		{name: "provider true overrides global false", providerOverride: &disabled},
		{name: "credential false overrides provider true", credential: &enabled, providerOverride: &disabled, wantCooldown: true},
		{name: "home mode disables local cooling despite credential false", homeEnabled: true, credential: &enabled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			cfg := &internalconfig.Config{
				DisableCooling: tc.globalDisable,
				Home:           internalconfig.HomeConfig{Enabled: tc.homeEnabled},
			}
			auth := &Auth{ID: tc.name, Provider: "claude", Status: StatusActive}
			if tc.credential != nil {
				auth.Metadata = map[string]any{"disable_cooling": *tc.credential}
			}
			if tc.providerOverride != nil {
				auth.Provider = "openai-compatibility"
				auth.Attributes = map[string]string{
					"provider_key": "compat",
					"compat_name":  "compat",
				}
				cfg.OpenAICompatibility = []internalconfig.OpenAICompatibility{{
					Name:           "compat",
					BaseURL:        "https://compat.example.com",
					DisableCooling: tc.providerOverride,
				}}
			}
			manager.SetConfig(cfg)
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			const model = "test-model"
			manager.MarkResult(context.Background(), Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    model,
				Error:    &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream failed"},
			})

			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil || updated.ModelStates[model] == nil {
				t.Fatalf("updated auth/model state missing: %#v", updated)
			}
			gotCooldown := !updated.ModelStates[model].NextRetryAfter.IsZero()
			if gotCooldown != tc.wantCooldown {
				t.Fatalf("cooldown present = %t, want %t", gotCooldown, tc.wantCooldown)
			}
		})
	}
}

func TestManagerCoolingBypassResumesExistingRegistryAndSchedulerState(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			manager := NewManager(nil, &Codex2APIDefaultSelector{}, nil)
			manager.SetConfig(&internalconfig.Config{})
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			auth := &Auth{ID: t.Name(), Provider: "codex", Status: StatusActive, Metadata: map[string]any{"disable_cooling": false}}
			const model = "cooling-bypass-test"
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			if _, errSelect := manager.SelectAuth(t.Context(), auth.Provider, model, cliproxyexecutor.Options{}); errSelect != nil {
				t.Fatal(errSelect)
			}
			manager.MarkResult(t.Context(), Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Error: &Error{HTTPStatus: status, Message: "upstream unavailable"}})
			if !reg.IsModelSuspendedForClient(auth.ID, model) {
				t.Fatal("expected model to be suspended before enabling cooling bypass")
			}
			cooled, _ := manager.GetByID(auth.ID)
			_, epoch := reg.GetModelsAndEpochForClient(auth.ID)
			manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{DisableCooling: new(true)}})
			updated, _ := manager.GetByID(auth.ID)
			if blocked, _, _ := isAuthBlockedForModel(updated, model, time.Now()); blocked {
				t.Error("cooling bypass did not clear the auth state")
			}
			if _, errSelect := manager.SelectAuth(t.Context(), auth.Provider, model, cliproxyexecutor.Options{}); errSelect != nil {
				t.Errorf("cooling bypass did not restore scheduling: %v", errSelect)
			}
			if reg.IsModelSuspendedForClient(auth.ID, model) || reg.IsModelQuotaExceededForClient(auth.ID, model) {
				t.Error("cooling bypass left stale suspension/quota state in the model registry")
			}
			if reg.ApplyClientModelProjections(auth.ID, epoch, cooled.Generation, []registry.ClientModelProjection{{ModelID: model, Suspended: true, QuotaExceeded: true}}) {
				t.Error("an old failure projection overwrote the cooling bypass")
			}
			manager.MarkResult(t.Context(), Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Error: &Error{HTTPStatus: status, Message: "another upstream failure"}})
			if reg.IsModelSuspendedForClient(auth.ID, model) || reg.IsModelQuotaExceededForClient(auth.ID, model) {
				t.Error("new failure reinstated cooling while the bypass was enabled")
			}
			manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{DisableCooling: new(false)}})
			manager.MarkResult(t.Context(), Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Error: &Error{HTTPStatus: status, Message: "cooling enabled again"}})
			if !reg.IsModelSuspendedForClient(auth.ID, model) {
				t.Error("turning the bypass off did not restore normal cooling")
			}
		})
	}
}
