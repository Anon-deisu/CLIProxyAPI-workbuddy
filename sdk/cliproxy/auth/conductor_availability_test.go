package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagerWorkBuddy6004CooldownResumesAtReset(t *testing.T) {
	for _, provider := range []string{"workbuddy", "workbuddy-ai"} {
		for _, disableCooling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disable_cooling=%t", provider, disableCooling), func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{
					AutoDisableQuota: true,
					DisableCooling:   &disableCooling,
				}}
				manager.SetConfig(cfg)
				authID := t.Name()
				models := []string{"workbuddy-model-a", "workbuddy-model-b"}
				reg := registry.GetGlobalRegistry()
				modelInfo := []*registry.ModelInfo{{ID: models[0]}, {ID: models[1]}}
				reg.RegisterClient(authID, provider, modelInfo)
				t.Cleanup(func() { reg.UnregisterClient(authID) })
				auth := &Auth{ID: authID, Provider: provider, Status: StatusActive}
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatalf("register auth: %v", err)
				}
				for _, model := range models {
					picked, err := manager.scheduler.pickSingle(t.Context(), provider, model, cliproxyexecutor.Options{}, nil)
					if err != nil {
						t.Fatal(err)
					}
					picked.ReleaseDispatchSlot()
				}

				retryAfter := time.Hour
				manager.MarkResult(t.Context(), Result{
					AuthID: authID, Provider: provider, Model: models[0], Success: false,
					CredentialScope: true, RetryAfter: &retryAfter,
					Error: &Error{Code: ErrorCodeWorkBuddyRateLimit, HTTPStatus: http.StatusTooManyRequests, Message: "WorkBuddy usage limit reached"},
				})

				updated, ok := manager.GetByID(authID)
				if !ok || updated == nil {
					t.Fatal("rate-limited account was not retained")
				}
				if updated.AutoDisabled || updated.Disabled || updated.Status == StatusDisabled {
					t.Fatalf("temporary limit permanently disabled account: %+v", updated)
				}
				if !updated.Unavailable || updated.Quota.Reason != "credential_quota" || !updated.Quota.NextRecoverAt.After(time.Now()) {
					t.Fatalf("credential-wide cooldown missing: unavailable=%t quota=%+v", updated.Unavailable, updated.Quota)
				}
				for _, model := range models {
					blocked, reason, next := isAuthBlockedForModel(updated, model, time.Now())
					if !blocked || reason != blockReasonCooldown || !next.Equal(updated.Quota.NextRecoverAt) {
						t.Fatalf("model %q block = (%t, %v, %v), want account cooldown until %v", model, blocked, reason, next, updated.Quota.NextRecoverAt)
					}
				}
				resetAt := updated.Quota.NextRecoverAt
				manager.MarkResult(t.Context(), Result{AuthID: authID, Provider: provider, Model: models[1], Success: true})
				if _, err := manager.Update(t.Context(), auth.Clone()); err != nil {
					t.Fatal(err)
				}
				manager.SetConfig(cfg)
				updated, _ = manager.GetByID(authID)
				if !updated.Quota.NextRecoverAt.Equal(resetAt) || !updated.Unavailable {
					t.Fatal("concurrent success, account reload or config reload erased the active limit")
				}
				records := manager.cooldownStateRecordsSnapshot()
				if len(records) == 0 {
					t.Fatal("active limit was omitted from cooldown persistence")
				}
				restored := NewManager(nil, &RoundRobinSelector{}, nil)
				restored.SetConfig(cfg)
				if _, err := restored.Register(t.Context(), auth.Clone()); err != nil {
					t.Fatal(err)
				}
				restored.SetCooldownStateStore(&mockCooldownStateStore{records: records})
				if err := restored.RestoreCooldownStates(t.Context()); err != nil {
					t.Fatal(err)
				}
				restoredAuth, _ := restored.GetByID(authID)
				for _, model := range append(models, "previously-unused-model") {
					if blocked, _, next := isAuthBlockedForModel(restoredAuth, model, resetAt.Add(-time.Nanosecond)); !blocked || !next.Equal(resetAt) {
						t.Fatalf("restored account admitted %q before the reset", model)
					}
					if blocked, _, _ := isAuthBlockedForModel(restoredAuth, model, resetAt); blocked {
						t.Fatalf("restored account remained blocked for %q at the reset", model)
					}
				}
				if len(CooldownSnapshotForAuth(restoredAuth, resetAt)) != 0 {
					t.Fatal("management cooldown view remained active after the reset")
				}
				manuallyDisabled := updated.Clone()
				manuallyDisabled.Disabled = true
				if blocked, reason, _ := isAuthBlockedForModel(manuallyDisabled, models[0], resetAt); !blocked || reason != blockReasonDisabled {
					t.Fatal("reset must not re-enable a manually disabled account")
				}

				manager.scheduler.mu.Lock()
				defer manager.scheduler.mu.Unlock()
				providerState := manager.scheduler.providers[provider]
				if providerState == nil {
					t.Fatal("provider missing from scheduler")
				}
				for _, model := range models {
					entry := providerState.modelShards[model].entries[authID]
					if entry == nil || entry.state != scheduledStateCooldown {
						t.Fatalf("model %q was not removed from ready scheduling", model)
					}
				}
				for _, model := range models {
					providerState.modelShards[model].promoteExpiredLocked(resetAt)
					if entry := providerState.modelShards[model].entries[authID]; entry == nil || entry.state != scheduledStateReady {
						t.Fatalf("model %q was not restored to ready scheduling at reset", model)
					}
				}
			})
		}
	}
}

func TestManagerWorkBuddy6000CooldownIsModelScoped(t *testing.T) {
	for _, provider := range []string{"workbuddy", "workbuddy-ai"} {
		t.Run(provider, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			disableCooling := true
			manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
				AutoDisableQuota: true,
				DisableCooling:   &disableCooling,
			}})
			authID := t.Name()
			models := []string{"workbuddy-model-a", "workbuddy-model-b"}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: models[0]}, {ID: models[1]}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })
			auth := &Auth{ID: authID, Provider: provider, Status: StatusActive}
			if _, err := manager.Register(t.Context(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}
			for _, model := range models {
				picked, err := manager.scheduler.pickSingle(t.Context(), provider, model, cliproxyexecutor.Options{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				picked.ReleaseDispatchSlot()
			}

			retryAfter := time.Hour
			manager.MarkResult(t.Context(), Result{
				AuthID: authID, Provider: provider, Model: models[0], Success: false,
				RetryAfter: &retryAfter,
				Error:      &Error{Code: ErrorCodeWorkBuddyModelRateLimit, HTTPStatus: http.StatusTooManyRequests, Message: "model usage limit reached"},
			})

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("rate-limited account was not retained")
			}
			if updated.AutoDisabled || updated.Disabled {
				t.Fatalf("temporary model limit disabled the account: %+v", updated)
			}
			resetAt := updated.ModelStates[models[0]].NextRetryAfter
			if !resetAt.After(time.Now()) {
				t.Fatalf("model cooldown deadline = %s, want future reset", resetAt)
			}
			if blocked, reason, next := isAuthBlockedForModel(updated, models[0], time.Now()); !blocked || reason != blockReasonCooldown || !next.Equal(resetAt) {
				t.Fatalf("limited model block = (%t, %v, %v), want cooldown until %v", blocked, reason, next, resetAt)
			}
			if blocked, _, _ := isAuthBlockedForModel(updated, models[1], time.Now()); blocked {
				t.Fatal("model limit blocked a sibling model on the same auth")
			}
			if blocked, _, _ := isAuthBlockedForModel(updated, models[0], resetAt); blocked {
				t.Fatal("limited model remained blocked at its reset")
			}
			pickedSibling, err := manager.scheduler.pickSingle(t.Context(), provider, models[1], cliproxyexecutor.Options{}, nil)
			if err != nil || pickedSibling.ID != authID {
				t.Fatalf("sibling model was not schedulable on the same auth: auth=%v error=%v", pickedSibling, err)
			}
			pickedSibling.ReleaseDispatchSlot()
			if pickedLimited, errPick := manager.scheduler.pickSingle(t.Context(), provider, models[0], cliproxyexecutor.Options{}, nil); errPick == nil {
				pickedLimited.ReleaseDispatchSlot()
				t.Fatal("limited model remained schedulable before its reset")
			}

			manager.scheduler.mu.Lock()
			defer manager.scheduler.mu.Unlock()
			providerState := manager.scheduler.providers[provider]
			if providerState == nil {
				t.Fatal("provider missing from scheduler")
			}
			limited := providerState.modelShards[models[0]].entries[authID]
			sibling := providerState.modelShards[models[1]].entries[authID]
			if limited == nil || limited.state != scheduledStateCooldown {
				t.Fatal("limited model was not removed from ready scheduling")
			}
			if sibling == nil || sibling.state != scheduledStateReady {
				t.Fatal("sibling model was removed from ready scheduling")
			}
			providerState.modelShards[models[0]].promoteExpiredLocked(resetAt)
			if limited.state != scheduledStateReady {
				t.Fatal("limited model was not restored to scheduling at reset")
			}
		})
	}
}

func TestUpdateAggregatedAvailability_UnavailableWithoutNextRetryDoesNotBlockAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusError,
				Unavailable: true,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if auth.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false")
	}
	if !auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = %v, want zero", auth.NextRetryAfter)
	}
}

func TestUpdateAggregatedAvailability_FutureNextRetryBlocksAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	next := now.Add(5 * time.Minute)
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if !auth.Unavailable {
		t.Fatalf("auth.Unavailable = false, want true")
	}
	if auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = zero, want %v", next)
	}
	if auth.NextRetryAfter.Sub(next) > time.Second || next.Sub(auth.NextRetryAfter) > time.Second {
		t.Fatalf("auth.NextRetryAfter = %v, want %v", auth.NextRetryAfter, next)
	}
}

func TestManager_AvailableProvidersAndHasProviderAuth_ExcludeDisabled(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()

	if _, err := manager.Register(ctx, &Auth{ID: "active", Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register active auth: %v", err)
	}
	// Provider gemini only has an auth with the Disabled flag set.
	if _, err := manager.Register(ctx, &Auth{ID: "flag-disabled", Provider: "gemini", Disabled: true}); err != nil {
		t.Fatalf("register flag-disabled auth: %v", err)
	}
	// Provider codex only has an auth whose Status is StatusDisabled.
	if _, err := manager.Register(ctx, &Auth{ID: "status-disabled", Provider: "codex", Status: StatusDisabled}); err != nil {
		t.Fatalf("register status-disabled auth: %v", err)
	}

	providers := manager.AvailableProviders()
	present := make(map[string]bool, len(providers))
	for _, p := range providers {
		present[p] = true
	}
	if !present["claude"] {
		t.Errorf("AvailableProviders() = %v, want to include active provider claude", providers)
	}
	if present["gemini"] {
		t.Errorf("AvailableProviders() = %v, want to exclude Disabled provider gemini", providers)
	}
	if present["codex"] {
		t.Errorf("AvailableProviders() = %v, want to exclude StatusDisabled provider codex", providers)
	}

	if !manager.HasProviderAuth("claude") {
		t.Errorf("HasProviderAuth(claude) = false, want true")
	}
	if manager.HasProviderAuth("gemini") {
		t.Errorf("HasProviderAuth(gemini) = true, want false (only Disabled auth registered)")
	}
	if manager.HasProviderAuth("codex") {
		t.Errorf("HasProviderAuth(codex) = true, want false (only StatusDisabled auth registered)")
	}
}

func TestManager_ResetQuotaClearsRuntimeAndRegistryState(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reset-quota-auth"
	model := "reset-quota-model"
	next := time.Now().Add(time.Hour)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	if _, errRegister := manager.Register(ctx, &Auth{
		ID:             authID,
		Provider:       "claude",
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
				UpdatedAt:      next,
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "quota")
	if count := reg.GetModelCount(model); count != 0 {
		t.Fatalf("registry model count before reset = %d, want 0", count)
	}

	updated, models, errReset := manager.ResetQuota(ctx, authID)
	if errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if updated == nil {
		t.Fatalf("ResetQuota() updated auth is nil")
	}
	if len(models) != 1 || models[0] != model {
		t.Fatalf("ResetQuota() models = %v, want [%s]", models, model)
	}
	if updated.Status != StatusActive || updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("updated auth state = status %q message %q unavailable %v next %v", updated.Status, updated.StatusMessage, updated.Unavailable, updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() || updated.Quota.BackoffLevel != 0 {
		t.Fatalf("updated auth quota = %+v, want cleared", updated.Quota)
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("updated model state missing")
	}
	if state.Status != StatusActive || state.StatusMessage != "" || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf("updated model state = status %q message %q unavailable %v next %v", state.Status, state.StatusMessage, state.Unavailable, state.NextRetryAfter)
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		t.Fatalf("updated model quota = %+v, want cleared", state.Quota)
	}
	if count := reg.GetModelCount(model); count != 1 {
		t.Fatalf("registry model count after reset = %d, want 1", count)
	}
}
