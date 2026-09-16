package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerAutoDisablesUnauthorizedAuthInMemory(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	cfg := &internalconfig.Config{}
	cfg.Routing.AutoDisableUnauthorized = true
	manager.SetConfig(cfg)
	auth := &Auth{ID: "auth-401", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Code: "auth_unavailable", Message: "token expired"},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth was not retained")
	}
	if !updated.AutoDisabled || updated.AutoDisableReason != AutoDisableReasonUnauthorized {
		t.Fatalf("auto-disable state = %+v", updated)
	}
	if updated.Disabled {
		t.Fatal("automatic disable must not set persistent Disabled")
	}
	if _, errPick := manager.SelectAuth(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}); errPick == nil {
		t.Fatal("automatically disabled auth was selected")
	}
}

func TestManagerAutoDisablesQuotaFromResponseSignal(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	cfg := &internalconfig.Config{}
	cfg.Routing.AutoDisableQuota = true
	manager.SetConfig(cfg)
	auth := &Auth{ID: "auth-quota", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"X-Codex-Primary-Used-Percent":   {"100"},
		"X-Codex-Primary-Window-Minutes": {"10080"},
	})
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: auth.Provider, Model: "gpt-5"})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil || !updated.AutoDisabled {
		t.Fatalf("quota auto-disable state = %#v", updated)
	}
	if updated.AutoDisableReason != AutoDisableReasonQuotaExhausted {
		t.Fatalf("quota auto-disable reason = %q", updated.AutoDisableReason)
	}
}

func TestResetQuotaClearsAutomaticQuotaDisable(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{
		ID:                "auth-reset",
		Provider:          "codex",
		Status:            StatusError,
		AutoDisabled:      true,
		AutoDisableReason: AutoDisableReasonQuotaExhausted,
		AutoDisabledAt:    time.Now(),
		Unavailable:       true,
		NextRetryAfter:    time.Now().Add(time.Hour),
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	updated, _, errReset := manager.ResetQuota(context.Background(), auth.ID)
	if errReset != nil {
		t.Fatalf("reset quota: %v", errReset)
	}
	if updated == nil || updated.AutoDisabled {
		t.Fatalf("reset did not clear auto-disable: %#v", updated)
	}
}

func codexWindowTestHeaders(resetAt time.Time, swapped bool, fiveHourUsed, weeklyUsed string) http.Header {
	five, week := "Primary", "Secondary"
	if swapped {
		five, week = week, five
	}
	headers := make(http.Header)
	headers.Set("X-Codex-"+five+"-Window-Minutes", "300")
	headers.Set("X-Codex-"+five+"-Used-Percent", fiveHourUsed)
	headers.Set("X-Codex-"+five+"-Reset-At", strconv.FormatInt(resetAt.Unix(), 10))
	headers.Set("X-Codex-"+week+"-Window-Minutes", "10080")
	headers.Set("X-Codex-"+week+"-Used-Percent", weeklyUsed)
	headers.Set("X-Codex-"+week+"-Reset-At", strconv.FormatInt(resetAt.Add(24*time.Hour).Unix(), 10))
	return headers
}

func newCodexWindowTestManager(t *testing.T, ignoreCooling bool) (*Manager, *Auth) {
	t.Helper()
	manager := NewManager(nil, &Codex2APIDefaultSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		AutoDisableQuota: true,
		DisableCooling:   &ignoreCooling,
	}})
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	auth := &Auth{ID: t.Name(), Provider: "codex", Status: StatusActive, Metadata: map[string]any{"disable_cooling": ignoreCooling}}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "quota-window-model"}, {ID: "quota-window-sibling"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	// Populate both shards before the result to exercise cross-model invalidation.
	for _, model := range []string{"quota-window-model", "quota-window-sibling"} {
		picked, err := manager.scheduler.pickSingle(t.Context(), "codex", model, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		picked.ReleaseDispatchSlot()
	}
	return manager, auth
}

func markCodexWindowResult(t *testing.T, manager *Manager, auth *Auth, headers http.Header, resultErr *Error) *Auth {
	t.Helper()
	ctx := internallogging.WithResponseHeadersHolder(t.Context())
	internallogging.SetResponseHeaders(ctx, headers)
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "codex", Model: "quota-window-model", Success: resultErr == nil, Error: resultErr})
	updated, _ := manager.GetByID(auth.ID)
	return updated
}

func TestCodexWindowQuotaPausesFiveHoursAndReadmitsAtReset(t *testing.T) {
	for _, ignoreCooling := range []bool{false, true} {
		for _, swapped := range []bool{false, true} {
			for _, failed := range []bool{false, true} {
				t.Run(fmt.Sprintf("ignore=%t/swapped=%t/failed=%t", ignoreCooling, swapped, failed), func(t *testing.T) {
					manager, auth := newCodexWindowTestManager(t, ignoreCooling)
					now := time.Now().UTC().Truncate(time.Second)
					resetAt := now.Add(time.Hour)
					var resultErr *Error
					if failed {
						resultErr = &Error{HTTPStatus: 429, Code: "usage_limit_reached", Message: "usage limit reached"}
					}
					updated := markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, swapped, "100", "40"), resultErr)
					if updated.Disabled || updated.AutoDisabled {
						t.Fatal("5H exhaustion must not disable the account")
					}
					for _, model := range []string{"quota-window-model", "quota-window-sibling"} {
						blocked, reason, next := isAuthBlockedForModel(updated, model, now)
						if !blocked || reason != blockReasonCooldown || !next.Equal(resetAt) {
							t.Fatalf("%s block=(%t,%v,%s), want quota pause until %s", model, blocked, reason, next, resetAt)
						}
						if _, err := manager.scheduler.pickSingle(t.Context(), "codex", model, cliproxyexecutor.Options{}, nil); err == nil {
							t.Fatalf("scheduler selected paused account for %s", model)
						}
						if blocked, _, _ := isAuthBlockedForModel(updated, model, resetAt); blocked {
							t.Fatalf("%s still blocked after 5H reset", model)
						}
						manager.scheduler.mu.Lock()
						shard := manager.scheduler.providers["codex"].ensureModelLocked(model, now)
						shard.promoteExpiredLocked(resetAt)
						state := shard.entries[auth.ID].state
						manager.scheduler.mu.Unlock()
						if state != scheduledStateReady {
							t.Fatalf("%s not readmitted at reset: %v", model, state)
						}
					}
				})
			}
		}
	}
}

func TestCodexWindowQuotaSevenDaysStaysAutomaticallyDisabled(t *testing.T) {
	for _, swapped := range []bool{false, true} {
		t.Run(fmt.Sprint(swapped), func(t *testing.T) {
			manager, auth := newCodexWindowTestManager(t, true)
			resetAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			updated := markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, swapped, "100", "100"), nil)
			if updated.Disabled || !updated.AutoDisabled || updated.AutoDisableReason != AutoDisableReasonQuotaExhausted {
				t.Fatal("7D exhaustion must use runtime quota auto-disable only")
			}
			if _, err := manager.Update(WithSkipPersist(t.Context()), &Auth{ID: auth.ID, Provider: "codex", Status: StatusActive}); err != nil {
				t.Fatal(err)
			}
			for _, model := range []string{"quota-window-model", "quota-window-sibling"} {
				if _, err := manager.scheduler.pickSingle(t.Context(), "codex", model, cliproxyexecutor.Options{}, nil); err == nil {
					t.Fatalf("disabled account selected for %s", model)
				}
			}
			updated = markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, swapped, "10", "10"), nil)
			if !updated.IsDispatchDisabled() {
				t.Fatal("an in-flight success silently re-enabled a 7D disabled account")
			}
			if blocked, _, _ := isAuthBlockedForModel(updated, "quota-window-model", resetAt.Add(8*24*time.Hour)); !blocked {
				t.Fatal("7D disable must require explicit resume")
			}
			updated, _, err := manager.ResetQuota(t.Context(), auth.ID)
			if err != nil || updated.IsDispatchDisabled() {
				t.Fatalf("explicit reset failed: %v", err)
			}
		})
	}
}

func TestCodexWindowQuotaDoesNotGuessUnknownOrAdditionalLimits(t *testing.T) {
	for name, headers := range map[string]http.Header{
		"missing-duration":         {"X-Codex-Primary-Used-Percent": {"100"}},
		"generic-limit":            {"X-Codex-Limit-Reached": {"true"}},
		"spark":                    {"X-Codex-Bengalfox-Primary-Used-Percent": {"100"}, "X-Codex-Bengalfox-Primary-Window-Minutes": {"10080"}},
		"review":                   {"X-Codex-Code-Review-Primary-Used-Percent": {"100"}, "X-Codex-Code-Review-Primary-Window-Minutes": {"10080"}},
		"invalid-percent":          {"X-Codex-Primary-Used-Percent": {"+Inf"}, "X-Codex-Primary-Window-Minutes": {"10080"}},
		"five-hours-without-reset": {"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Window-Minutes": {"300"}},
	} {
		t.Run(name, func(t *testing.T) {
			manager, auth := newCodexWindowTestManager(t, true)
			updated := markCodexWindowResult(t, manager, auth, headers, &Error{HTTPStatus: 429, Code: "usage_limit_reached"})
			if updated.IsDispatchDisabled() {
				t.Fatal("unknown/additional quota was treated as exhausted account 7D quota")
			}
		})
	}
}

func TestCodexWindowQuotaPauseSurvivesSuccessReloadAndRefresh(t *testing.T) {
	manager, auth := newCodexWindowTestManager(t, true)
	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	paused := markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, false, "100", "40"), nil)
	// An older in-flight success from the same quota window cannot undo exhaustion.
	updated := markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, false, "90", "40"), nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{AutoDisableQuota: true, DisableCooling: new(true)}})
	updated, err := manager.Update(t.Context(), &Auth{ID: auth.ID, Provider: "codex", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	refreshed := paused.Clone()
	refreshed.Metadata["access_token"] = "refreshed-test-token"
	updated, err = manager.UpdateRefreshedAuth(t.Context(), paused, refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Disabled || updated.AutoDisabled {
		t.Fatal("5H pause became a disable during reload or refresh")
	}
	if blocked, _, next := isAuthBlockedForModel(updated, "quota-window-sibling", now); !blocked || !next.Equal(resetAt) {
		t.Fatal("reload/refresh or cooling bypass erased the 5H pause")
	}
	updated = markCodexWindowResult(t, manager, auth, nil, nil)
	if !updated.CodexQuotaResumeAt.Equal(resetAt) {
		t.Fatal("headerless response erased the quota gate")
	}
	raw, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	var restored Auth
	if err = json.Unmarshal(raw, &restored); err != nil || !restored.CodexQuotaResumeAt.IsZero() {
		t.Fatalf("runtime pause leaked into persisted Auth JSON: %v", err)
	}
	// Advance only the observation clock; expiry tests must not sleep for real time.
	manager.mu.Lock()
	current := manager.auths[auth.ID]
	current.Quota.ObserveResponseHeadersForProvider("codex", codexWindowTestHeaders(resetAt.Add(5*time.Hour), false, "0", "41"), resetAt)
	manager.maybeAutoDisableAuthLocked(current, nil, true, resetAt)
	current.Generation++
	updated = current.Clone()
	manager.mu.Unlock()
	manager.scheduler.upsertAuth(updated)
	if blocked, _, _ := isAuthBlockedForModel(updated, "quota-window-sibling", now); blocked {
		t.Fatal("a refreshed quota window did not restore dispatch")
	}
	if _, err = manager.scheduler.pickSingle(t.Context(), "codex", "quota-window-sibling", cliproxyexecutor.Options{}, nil); err != nil {
		t.Fatalf("sibling shard was not readmitted after quota refresh: %v", err)
	}
}

func TestCodexWindowQuotaRelativeSuccessCannotReleasePause(t *testing.T) {
	manager, auth := newCodexWindowTestManager(t, true)
	resetAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, false, "100", "40"), nil)
	// A delayed response with a relative reset value can appear to reset later.
	headers := codexWindowTestHeaders(resetAt, false, "90", "40")
	headers.Del("X-Codex-Primary-Reset-At")
	headers.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	updated := markCodexWindowResult(t, manager, auth, headers, nil)
	if !updated.CodexQuotaResumeAt.Equal(resetAt) {
		t.Fatal("an in-flight success released or extended the quota pause")
	}
}

func TestCodexWindowQuotaConcurrentResultsKeepAccountExcluded(t *testing.T) {
	manager, auth := newCodexWindowTestManager(t, true)
	resetAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			used := "90"
			if i%2 == 0 {
				used = "100"
			}
			ctx := internallogging.WithResponseHeadersHolder(t.Context())
			internallogging.SetResponseHeaders(ctx, codexWindowTestHeaders(resetAt, false, used, "40"))
			manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "codex", Model: "quota-window-model", Success: true})
		})
	}
	wg.Wait()
	updated, _ := manager.GetByID(auth.ID)
	if updated.IsDispatchDisabled() || !updated.CodexQuotaResumeAt.Equal(resetAt) {
		t.Fatal("concurrent results lost the quota gate or disabled the account")
	}
	if _, err := manager.SelectAuth(t.Context(), "codex", "quota-window-sibling", cliproxyexecutor.Options{}); err == nil {
		t.Fatal("concurrent scheduler snapshots allowed an exhausted account")
	}
}

func TestCodexWindowQuotaSkipsNonUpstreamObservation(t *testing.T) {
	manager, auth := newCodexWindowTestManager(t, true)
	ctx := internallogging.WithResponseHeadersHolder(t.Context())
	internallogging.SetResponseHeaders(ctx, codexWindowTestHeaders(time.Now().Add(time.Hour), false, "100", "100"))
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "codex", Model: "quota-window-model", Success: true, SkipQuotaObservation: true})
	updated, _ := manager.GetByID(auth.ID)
	if updated.IsDispatchDisabled() || !updated.CodexQuotaResumeAt.IsZero() {
		t.Fatal("non-upstream result reused unrelated quota headers")
	}
}

func TestCodexWindowQuotaResetAndOptOut(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprint(reset), func(t *testing.T) {
			manager, auth := newCodexWindowTestManager(t, true)
			resetAt := time.Now().Add(time.Hour)
			markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, false, "100", "30"), nil)
			if reset {
				if _, _, err := manager.ResetQuota(t.Context(), auth.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{DisableCooling: new(true)}})
				markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(resetAt, false, "100", "100"), nil)
			}
			updated, _ := manager.GetByID(auth.ID)
			if updated.IsDispatchDisabled() || !updated.CodexQuotaResumeAt.IsZero() {
				t.Fatal("reset or policy opt-out did not release the quota pause")
			}
			if _, err := manager.scheduler.pickSingle(t.Context(), "codex", "quota-window-sibling", cliproxyexecutor.Options{}, nil); err != nil {
				t.Fatalf("account not schedulable after reset/opt-out: %v", err)
			}
		})
	}
}

func TestCodexWindowQuotaExtendsEveryModelShard(t *testing.T) {
	manager, auth := newCodexWindowTestManager(t, true)
	firstReset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	nextReset := firstReset.Add(time.Hour)
	markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(firstReset, false, "100", "20"), nil)
	markCodexWindowResult(t, manager, auth, codexWindowTestHeaders(nextReset, false, "100", "21"), nil)
	manager.scheduler.mu.Lock()
	shard := manager.scheduler.providers["codex"].ensureModelLocked("quota-window-sibling", time.Now())
	shard.promoteExpiredLocked(firstReset)
	state, next := shard.entries[auth.ID].state, shard.entries[auth.ID].nextRetryAt
	manager.scheduler.mu.Unlock()
	if state != scheduledStateCooldown || !next.Equal(nextReset) {
		t.Fatalf("sibling quota pause = (%v, %s), want until %s", state, next, nextReset)
	}
}

func TestCodexWindowQuotaResetParsing(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct {
		name       string
		resetAt    string
		resetAfter string
		want       time.Time
	}{
		{"absolute-wins", strconv.FormatInt(now.Add(time.Hour).Unix(), 10), "120", now.Add(time.Hour)},
		{"relative", "", "120", now.Add(2 * time.Minute)},
		{"expired-absolute", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10), "120", time.Time{}},
		{"invalid-reset", "bad", "bad", time.Time{}},
		{"relative-overflow", "", "9223372036854775807", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quota := QuotaState{ObservedAt: now, Signals: map[string]string{
				"X-Codex-Primary-Window-Minutes":      "300",
				"X-Codex-Primary-Used-Percent":        "100",
				"X-Codex-Primary-Reset-At":            tc.resetAt,
				"X-Codex-Primary-Reset-After-Seconds": tc.resetAfter,
			}}
			windows := codexAccountQuotaWindows(quota, now)
			if !windows.fiveHourResetAt.Equal(tc.want) {
				t.Fatalf("reset = %s, want %s", windows.fiveHourResetAt, tc.want)
			}
			later := codexAccountQuotaWindows(quota, now.Add(time.Minute))
			if !later.fiveHourResetAt.Equal(tc.want) {
				t.Fatal("re-reading relative quota extended its reset deadline")
			}
		})
	}
}

func TestCodexWindowQuotaLeavesOtherProvidersUnchanged(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{AutoDisableQuota: true}})
	auth := &Auth{ID: t.Name(), Provider: "claude"}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	manager.MarkResult(t.Context(), Result{AuthID: auth.ID, Provider: "claude", Error: &Error{HTTPStatus: 429, Code: "usage_limit_reached"}})
	updated, _ := manager.GetByID(auth.ID)
	if !updated.AutoDisabled || updated.Disabled || !updated.CodexQuotaResumeAt.IsZero() {
		t.Fatal("non-Codex auto-disable behavior changed")
	}
}
