package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestDegradedPoolSelection(t *testing.T) {
	for _, fast := range []bool{true, false} {
		for _, mixed := range []bool{true, false} {
			t.Run(fmt.Sprintf("fast=%t/mixed=%t", fast, mixed), func(t *testing.T) {
				var selector Selector = &RoundRobinSelector{}
				if !fast {
					affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
					t.Cleanup(affinity.Stop)
					selector = affinity
				}
				manager := NewManager(nil, selector, nil)
				manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
				for _, account := range []*Auth{
					{ID: "normal", Provider: "codex", Status: StatusActive},
					{ID: "degraded", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"degraded": true}},
					{ID: "disabled", Provider: "codex", Status: StatusDisabled, Disabled: true, Metadata: map[string]any{"degraded": true}},
				} {
					if _, err := manager.Register(t.Context(), account); err != nil {
						t.Fatal(err)
					}
				}
				for _, degraded := range []bool{false, true} {
					ctx := cliproxyexecutor.WithDegradedRouting(t.Context(), degraded)
					want, other := "normal", "degraded"
					if degraded {
						want, other = other, want
					}
					pick := func(opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, error) {
						if mixed {
							selected, _, _, err := manager.pickNextMixed(ctx, []string{"codex"}, "", opts, tried)
							return selected, err
						}
						selected, _, err := manager.pickNext(ctx, "codex", "", opts, tried)
						return selected, err
					}
					for range 3 {
						selected, err := pick(cliproxyexecutor.Options{}, nil)
						if err != nil {
							t.Fatal(err)
						}
						selected.ReleaseDispatchSlot()
						if selected.ID != want {
							t.Fatalf("selected %s, want %s", selected.ID, want)
						}
					}
					if selected, err := pick(cliproxyexecutor.Options{}, map[string]struct{}{want: {}}); err == nil || selected != nil {
						t.Fatal("exhausted pool leaked to other accounts")
					}
					if selected, err := pick(cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: other}}, nil); err == nil || selected != nil {
						t.Fatal("pinned account bypassed pool isolation")
					}
				}
			})
		}
	}
}

func TestDegradedPoolRetryEligibility(t *testing.T) {
	account := &Auth{ID: "degraded", Metadata: map[string]any{"degraded": true}}
	if authSelectionEligibilityForRequest(context.Background(), cliproxyexecutor.Options{}).allows(account) {
		t.Fatal("normal pool allowed degraded account")
	}
	if !authSelectionEligibilityForRequest(cliproxyexecutor.WithDegradedRouting(context.Background(), true), cliproxyexecutor.Options{}).allows(account) {
		t.Fatal("degraded pool rejected degraded account")
	}
}

func TestDegradedPoolSessionBindingsStayIndependent(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	for _, id := range []string{"normal", "degraded"} {
		if _, err := manager.Register(t.Context(), &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"degraded": id == "degraded"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, degraded := range []bool{false, true, false, true} {
		opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"same-session"}}, Metadata: map[string]any{}}
		selected, _, err := manager.pickNext(cliproxyexecutor.WithDegradedRouting(t.Context(), degraded), "codex", "", opts, nil)
		if err != nil {
			t.Fatal(err)
		}
		selected.ReleaseDispatchSlot()
		if selected.IsDegraded() != degraded {
			t.Fatal("session affinity crossed pools")
		}
		selector.OnResult(Result{AuthID: selected.ID, Provider: "codex", Success: true, Options: opts})
	}
	selector.cache.mu.RLock()
	defer selector.cache.mu.RUnlock()
	if len(selector.cache.entries) != 2 {
		t.Fatalf("want independent affinity bindings, got %d", len(selector.cache.entries))
	}
}
