package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// WorkBuddy's 6020 admission-queue throttle is a short, request-scoped wait. It
// must not fan out to every model of the account: doing so marks 39 of 40
// healthy accounts unschedulable and pushes all traffic onto the last one.
func TestManagerWorkBuddyQueueWaitingDoesNotCoolWholeCredential(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	authID := "workbuddy-queue-waiting"
	models := []string{"queue-model-a", "queue-model-b"}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "workbuddy", []*registry.ModelInfo{{ID: models[0]}, {ID: models[1]}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	if _, err := manager.Register(context.Background(), &Auth{ID: authID, Provider: "workbuddy", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	retryAfter := 40 * time.Second
	manager.MarkResult(context.Background(), Result{
		AuthID: authID, Provider: "workbuddy", Model: models[0], Success: false,
		RetryAfter: &retryAfter,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"code":6020,"msg":"queue.waiting.title","data":{"retry_after":40}}`},
	})

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("auth missing after queue-waiting result")
	}
	// The queue wait applies to the model that received it, for its advertised
	// delay, and is not a credential quota exhaustion.
	state := existingModelState(updated, canonicalModelKey(models[0]))
	if state == nil {
		t.Fatal("model state missing after queue-waiting result")
	}
	if state.Quota.Reason == "credential_quota" {
		t.Fatalf("queue waiting escalated to a credential quota cooldown: %+v", state.Quota)
	}
	remaining := time.Until(state.NextRetryAfter)
	if remaining < 30*time.Second || remaining > 50*time.Second {
		t.Fatalf("model cooldown = %v, want about 40s", remaining)
	}

	// A sibling model must remain schedulable: the queue wait applies only to the
	// attempt that received it.
	blocked, _, _ := isAuthBlockedForModel(updated, models[1], time.Now())
	if blocked {
		t.Fatal("sibling model was blocked by another model's queue wait")
	}
}

// One account's queue wait must not remove the other accounts from dispatch:
// this is the 39-of-40 regression the fix targets.
func TestManagerWorkBuddyQueueWaitingKeepsOtherAccountsSchedulable(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const model = "queue-shared-model"
	reg := registry.GetGlobalRegistry()
	ids := []string{"workbuddy-queue-a", "workbuddy-queue-b", "workbuddy-queue-c"}
	for _, id := range ids {
		reg.RegisterClient(id, "workbuddy", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), &Auth{ID: id, Provider: "workbuddy", Status: StatusActive}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}

	retryAfter := 40 * time.Second
	manager.MarkResult(context.Background(), Result{
		AuthID: ids[0], Provider: "workbuddy", Model: model, Success: false,
		RetryAfter: &retryAfter,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"code":6020,"msg":"queue.waiting.title","data":{"retry_after":40}}`},
	})

	first, _ := manager.GetByID(ids[0])
	if blocked, _, _ := isAuthBlockedForModel(first, model, time.Now()); !blocked {
		t.Fatal("queue-waiting account stayed immediately schedulable")
	}
	for _, id := range ids[1:] {
		other, _ := manager.GetByID(id)
		if blocked, _, _ := isAuthBlockedForModel(other, model, time.Now()); blocked {
			t.Fatalf("account %s was blocked by another account's queue wait", id)
		}
	}

	// After the advertised delay the account returns to dispatch on its own.
	after := time.Now().Add(41 * time.Second)
	if blocked, _, _ := isAuthBlockedForModel(first, model, after); blocked {
		t.Fatal("queue-waiting account did not recover after its retry delay")
	}
}
