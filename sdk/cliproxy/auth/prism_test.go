package auth

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestPrismSelectsEnabledPlusRegardlessOfCodexQuota(t *testing.T) {
	const model = "prism-selection-model"
	ids := []string{"prism-plus-a", "prism-plus-b", "prism-free", "prism-pro", "prism-disabled", "prism-auto-disabled"}
	registerSchedulerModels(t, "codex", model, ids...)
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.executors["codex"] = schedulerTestExecutor{provider: "codex"}
	m.runtimeConfig.Store(&config.Config{Routing: config.RoutingConfig{Prism: new(true)}})
	resetAt := time.Now().Add(time.Hour)
	for i, id := range ids {
		plan := "plus"
		if i == 2 {
			plan = "free"
		} else if i == 3 {
			plan = "pro"
		}
		a := &Auth{ID: id, Provider: "codex", Attributes: map[string]string{"plan_type": plan}, Metadata: map[string]any{"access_token": "test-token"}, Disabled: i == 4, AutoDisabled: i == 5}
		if i < 2 {
			a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: resetAt}
			a.CodexQuotaResumeAt = resetAt
			a.ModelStates = map[string]*ModelState{model: {Unavailable: true, NextRetryAfter: resetAt}}
		}
		if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	seen := make(map[string]int)
	for range 6 {
		opts := cliproxyexecutor.Options{Metadata: make(map[string]any)}
		a, _, provider, errPick := m.pickNextMixed(context.Background(), []string{"codex"}, model, opts, nil)
		if errPick != nil {
			t.Fatal(errPick)
		}
		if provider != "codex" || (a.ID != ids[0] && a.ID != ids[1]) {
			t.Fatalf("Prism selected %s/%s, want enabled Plus account", provider, a.ID)
		}
		if opts.Metadata["prism_route"] != true {
			t.Fatal("selected request did not retain its Prism transport")
		}
		if models := m.prepareExecutionModels(a, model); len(models) != 1 {
			t.Fatalf("Codex quota blocked Prism execution after selection: %v", models)
		}
		seen[a.ID]++
		a.ReleaseDispatchSlot()
	}
	if seen[ids[0]] != 3 || seen[ids[1]] != 3 {
		t.Fatalf("Prism did not rotate through all enabled Plus accounts: %v", seen)
	}
	for _, id := range ids[:2] {
		a, _ := m.GetByID(id)
		if !a.Quota.Exceeded || !a.CodexQuotaResumeAt.Equal(resetAt) {
			t.Fatal("Prism selection changed native Codex quota state")
		}
	}
}

func TestPrismResultsDoNotChangeCodexAvailability(t *testing.T) {
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	next := time.Now().Add(time.Hour)
	a := &Auth{ID: "prism-neutral", Provider: "codex", Status: StatusError, Unavailable: true,
		Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next}, CodexQuotaResumeAt: next,
		ModelStates: map[string]*ModelState{"gpt-6-astra": {Status: StatusError, Unavailable: true, NextRetryAfter: next}}}
	if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
		t.Fatal(errRegister)
	}
	before, _ := m.GetByID(a.ID)
	for _, success := range []bool{true, false} {
		result := Result{AuthID: a.ID, Provider: "codex", Model: "gpt-6-astra", Success: success,
			Options: cliproxyexecutor.Options{Metadata: map[string]any{"prism_route": true}}}
		if !success {
			result.Error = &Error{HTTPStatus: 429, Message: "Prism capacity exceeded"}
		}
		m.MarkResult(context.Background(), result)
		after, _ := m.GetByID(a.ID)
		if !reflect.DeepEqual(before.Quota, after.Quota) || !reflect.DeepEqual(before.ModelStates, after.ModelStates) || !before.CodexQuotaResumeAt.Equal(after.CodexQuotaResumeAt) {
			t.Fatal("Prism result changed native Codex quota or model availability")
		}
	}
}

func TestPrismSelectionRespectsPinnedAccountAndConcurrency(t *testing.T) {
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.executors["codex"] = schedulerTestExecutor{provider: "codex"}
	m.runtimeConfig.Store(&config.Config{Routing: config.RoutingConfig{Prism: new(true)}})
	for _, id := range []string{"prism-pinned", "prism-other"} {
		a := &Auth{ID: id, Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}, Metadata: map[string]any{"access_token": "test-token"}}
		a.SetConcurrencyLimit(1)
		if _, errRegister := m.Register(context.Background(), a); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "prism-pinned"}}
	a, _, errPick := m.pickNext(context.Background(), "codex", "", opts, nil)
	if errPick != nil || a.ID != "prism-pinned" {
		t.Fatalf("pinned selection = %v, %v", a, errPick)
	}
	defer a.ReleaseDispatchSlot()
	if second, _, errSecond := m.pickNext(context.Background(), "codex", "", opts, nil); errSecond == nil || second != nil {
		t.Fatal("Prism exceeded the pinned account concurrency limit")
	}
}
