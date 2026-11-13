package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestManagerSelectionIndexInvalidation(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	reg := registry.GetGlobalRegistry()
	authID := t.Name()
	register := func(model string) {
		reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	register("first")
	if _, errRegister := manager.Register(ctx, &Auth{ID: authID, Provider: "codex"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	index := func(provider, model string) *selectionIndex {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.selectionIndexLocked(provider, model)
	}
	initial := index("codex", "first")
	if len(initial.ids) != 1 || initial != index("codex", "first") {
		t.Fatal("unchanged route did not reuse membership index")
	}
	// Result updates are dynamic; they must not invalidate static membership.
	manager.MarkResult(ctx, Result{AuthID: authID, Provider: "codex", Model: "first", Success: true})
	if initial != index("codex", "first") {
		t.Fatal("result invalidated static membership")
	}
	register("second")
	if len(index("codex", "first").ids) != 0 || len(index("codex", "second").ids) != 1 {
		t.Fatal("registry replacement retained stale membership")
	}
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"codex": {{Name: "second", Alias: "alias"}}})
	if len(index("codex", "alias").ids) != 1 {
		t.Fatal("OAuth alias was not indexed")
	}
	manager.SetOAuthModelAlias(nil)
	if len(index("codex", "alias").ids) != 0 {
		t.Fatal("OAuth alias removal retained membership")
	}
	beforeConfig := index("codex", "second")
	manager.SetConfig(&internalconfig.Config{})
	if beforeConfig == index("codex", "second") {
		t.Fatal("config publication retained previous route index")
	}
	auth, _ := manager.GetByID(authID)
	auth.Provider = "claude"
	if _, errUpdate := manager.Update(ctx, auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if len(index("codex", "second").ids) != 0 || len(index("claude", "second").ids) != 1 {
		t.Fatal("credential provider update retained stale membership")
	}
	manager.Remove(ctx, authID)
	if len(index("claude", "second").ids) != 0 {
		t.Fatal("removed credential remains indexed")
	}
}

func TestManagerSelectionIndexMetadataPublication(t *testing.T) {
	for _, initialKind := range []string{AuthKindAPIKey, AuthKindOAuth} {
		t.Run(initialKind, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: "real", Alias: "alias"}}})
			authID := t.Name()
			registered, errRegister := manager.Register(ctx, &Auth{ID: authID, Provider: "claude", Metadata: map[string]any{AttributeAuthKind: initialKind}})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "real"}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })
			assertSelection := func(kind string) {
				t.Helper()
				selected, errSelect := manager.SelectAuth(ctx, "claude", "alias", cliproxyexecutor.Options{})
				if kind == AuthKindOAuth {
					if errSelect != nil || selected == nil || selected.ID != authID || selected.AuthKind() != kind {
						t.Errorf("OAuth alias membership missing: selected=%t err=%v", selected != nil, errSelect)
					}
				} else if selected != nil || errSelect == nil {
					t.Errorf("API key retained OAuth alias membership: selected=%t err=%v", selected != nil, errSelect)
				}
			}
			assertSelection(initialKind)
			manager.mu.RLock()
			before := manager.selectionIndexLocked("claude", "alias")
			manager.mu.RUnlock()
			if saved, _ := manager.PersistMetadataEntry(ctx, authID, registered.RegistrationEpoch, "observed_quota", 42, nil); !saved {
				t.Fatal("quota metadata save failed")
			}
			manager.mu.RLock()
			afterQuota := manager.selectionIndexLocked("claude", "alias")
			manager.mu.RUnlock()
			if before != afterQuota {
				t.Error("unrelated quota metadata invalidated static membership")
			}
			nextKind := AuthKindOAuth
			if initialKind == AuthKindOAuth {
				nextKind = AuthKindAPIKey
			}
			if saved, _ := manager.PersistMetadataEntry(ctx, authID, registered.RegistrationEpoch, AttributeAuthKind, nextKind, nil); !saved {
				t.Fatal("auth kind metadata save failed")
			}
			current, _ := manager.GetByID(authID)
			if current.Generation != registered.Generation || current.RegistrationEpoch != registered.RegistrationEpoch {
				t.Fatal("metadata save changed lifecycle versions")
			}
			assertSelection(nextKind)
			manager.mu.RLock()
			afterKind := manager.selectionIndexLocked("claude", "alias")
			manager.mu.RUnlock()
			if afterKind == before {
				t.Error("auth kind publication retained stale route index")
			}
		})
	}
}

func TestManagerSelectionIndexStorePublication(t *testing.T) {
	for _, path := range []string{"metadata-enrichment", "quota-enrichment", "metadata-invalidation"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: "real", Alias: "alias"}}})
			registered, errRegister := manager.Register(ctx, &Auth{ID: t.Name(), Provider: "claude", Metadata: map[string]any{AttributeAuthKind: AuthKindAPIKey}})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(registered.ID, "claude", []*registry.ModelInfo{{ID: "real"}})
			t.Cleanup(func() { reg.UnregisterClient(registered.ID) })
			if selected, errSelect := manager.SelectAuth(ctx, "claude", "alias", cliproxyexecutor.Options{}); selected != nil || errSelect == nil {
				t.Fatal("API key must not support the OAuth alias")
			}
			metadataValid := true
			manager.SetStore(&callbackAuthStore{save: func(auth *Auth) error {
				if path == "metadata-invalidation" {
					metadataValid = false
				} else {
					auth.Metadata[AttributeAuthKind] = AuthKindOAuth
				}
				return nil
			}})
			switch path {
			case "metadata-enrichment":
				if saved, _ := manager.PersistMetadataEntry(ctx, registered.ID, registered.RegistrationEpoch, "notes", "store enriches routing", nil); !saved {
					t.Fatal("metadata save failed")
				}
			case "quota-enrichment":
				if _, _, errReset := manager.ResetQuota(ctx, registered.ID); errReset != nil {
					t.Fatal(errReset)
				}
			case "metadata-invalidation":
				if saved, _ := manager.PersistMetadataEntry(ctx, registered.ID, registered.RegistrationEpoch, AttributeAuthKind, AuthKindOAuth, func(*Auth) bool { return metadataValid }); saved {
					t.Fatal("stale metadata should be removed after validation fails")
				}
			}
			// Removing an explicit API-key kind restores the legacy OAuth alias
			// channel too. Both successful saves and invalidation must be visible.
			selected, errSelect := manager.SelectAuth(ctx, "claude", "alias", cliproxyexecutor.Options{})
			if errSelect != nil || selected == nil || selected.ID != registered.ID {
				t.Fatalf("store publication retained stale membership: selected=%t err=%v", selected != nil, errSelect)
			}
		})
	}
}

func TestManagerSchedulingSnapshotIsolation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID: "snapshot", Provider: "codex", Status: StatusActive,
		Attributes:  map[string]string{"priority": "2", "unrelated": "keep"},
		Metadata:    map[string]any{"profile": map[string]any{"value": "keep"}, AttributeWeight: 3},
		Quota:       QuotaState{Signals: map[string]string{"primary-used-percent": "20"}},
		ModelStates: map[string]*ModelState{"model": {Status: StatusActive}},
	}
	for _, test := range []struct {
		name     string
		selector Selector
		plugin   bool
		light    bool
	}{
		{"builtin", &RoundRobinSelector{}, false, true},
		{"custom", lastAuthSelector{}, false, false},
		{"plugin", &RoundRobinSelector{}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, snapshots, errAvailable := manager.availableAuthsForSelector(test.selector, []*Auth{auth}, "codex", "model", time.Now(), false, test.plugin)
			if errAvailable != nil {
				t.Fatal(errAvailable)
			}
			got := snapshots[0]
			if got.selectionOnly != test.light || authWeight(got) != 3 || authPriority(got) != 2 {
				t.Fatal("snapshot lost scheduling fields or isolation mode")
			}
			got.Attributes["priority"] = "99"
			got.Quota.Signals["primary-used-percent"] = "99"
			if test.light {
				if got.Metadata != nil || got.ModelStates != nil || got.Attributes["unrelated"] != "" {
					t.Fatal("light snapshot copied execution-only data")
				}
			} else {
				got.Metadata["profile"].(map[string]any)["value"] = "changed"
				got.ModelStates["model"].Status = StatusDisabled
			}
			if authPriority(auth) != 2 || auth.Quota.Signals["primary-used-percent"] != "20" || auth.ModelStates["model"].Status != StatusActive || auth.Metadata["profile"].(map[string]any)["value"] != "keep" {
				t.Fatal("selector mutated manager-owned data")
			}
		})
	}
}

func TestManagerSessionAffinityConcurrentLiveSnapshots(t *testing.T) {
	selector := NewSessionAffinitySelector(nil)
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	for _, id := range []string{t.Name() + "-a", t.Name() + "-b"} {
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"profile": map[string]any{"value": "original"}}}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		workers.Go(func() {
			for attempt := 0; attempt < 30; attempt++ {
				opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"shared"}}, Metadata: map[string]any{}}
				selected, _, _, errPick := manager.pickNextMixed(context.Background(), []string{"codex"}, "", opts, nil)
				if errPick != nil {
					t.Error(errPick)
					return
				}
				selected.Metadata["profile"].(map[string]any)["value"] = "executor-owned"
				manager.MarkResult(context.Background(), Result{AuthID: selected.ID, Provider: "codex", Success: true, Options: opts})
			}
		})
	}
	workers.Wait()
	for _, auth := range manager.List() {
		if auth.Metadata["profile"].(map[string]any)["value"] != "original" {
			t.Fatal("execution snapshot mutated stored credential")
		}
	}
}

type schedulerTestExecutor struct {
	provider string
}

type schedulerLoadStore struct {
	auths []*Auth
}

func (s *schedulerLoadStore) List(context.Context) ([]*Auth, error) {
	return s.auths, nil
}

func (s *schedulerLoadStore) Save(context.Context, *Auth) (string, error) {
	return "", nil
}

func (s *schedulerLoadStore) Delete(context.Context, string) error {
	return nil
}

func (e schedulerTestExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "test"
}

func (schedulerTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (schedulerTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (schedulerTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (schedulerTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type fakePluginScheduler struct {
	acrossPriorities     bool
	acrossPrioritiesFunc func() bool
	resp                 pluginapi.SchedulerPickResponse
	handled              bool
	err                  error
	calls                int
	requests             []pluginapi.SchedulerPickRequest
	pick                 func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
}

func (s *fakePluginScheduler) SchedulerWantsAcrossPriorities() bool {
	if s.acrossPrioritiesFunc != nil {
		return s.acrossPrioritiesFunc()
	}
	return s.acrossPriorities
}

func (s *fakePluginScheduler) PickAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.calls++
	s.requests = append(s.requests, req)
	if s.pick != nil {
		return s.pick(ctx, req)
	}
	return s.resp, s.handled, s.err
}

type inactivePluginScheduler struct {
	fakePluginScheduler
}

type authKindHomeDispatcher struct {
	auths    []Auth
	counts   []int
	policies []string
}

func (d *authKindHomeDispatcher) HeartbeatOK() bool {
	return true
}

func (d *authKindHomeDispatcher) RPopAuth(_ context.Context, _ string, _ string, _ http.Header, count int) ([]byte, error) {
	d.counts = append(d.counts, count)
	if count < 1 || count > len(d.auths) {
		return nil, home.ErrAuthNotFound
	}
	return json.Marshal(homeAuthDispatchResponse{Auth: d.auths[count-1]})
}

func (d *authKindHomeDispatcher) RPopAuthWithPolicy(ctx context.Context, model string, sessionID string, headers http.Header, count int, policy string) ([]byte, error) {
	d.policies = append(d.policies, policy)
	return d.RPopAuth(ctx, model, sessionID, headers, count)
}

func (*authKindHomeDispatcher) AbortAmbiguousDispatch() {}

func (s *inactivePluginScheduler) HasScheduler() bool {
	return false
}

type trackingSelector struct {
	calls      int
	lastAuthID []string
}

func (s *trackingSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	s.calls++
	s.lastAuthID = s.lastAuthID[:0]
	for _, auth := range auths {
		s.lastAuthID = append(s.lastAuthID, auth.ID)
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[len(auths)-1], nil
}

func newSchedulerForTest(selector Selector, auths ...*Auth) *authScheduler {
	scheduler := newAuthScheduler(selector)
	scheduler.rebuild(auths)
	return scheduler
}

func registerSchedulerModels(t *testing.T, provider string, model string, authIDs ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			reg.UnregisterClient(authID)
		}
	})
}

func TestSchedulerPick_RoundRobinHighestPriority(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "high-b", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "high-a", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
	)

	want := []string{"high-a", "high-b", "high-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestCodex2APIDefaultSelectorOrdersPriorityHealthAndUsage(t *testing.T) {
	priorityCandidate := &Auth{
		ID:       "priority-warm",
		Provider: "codex",
		Status:   StatusError,
		Attributes: map[string]string{
			"priority": "10",
		},
	}
	healthyHighUsage := &Auth{
		ID:       "healthy-high-usage",
		Provider: "codex",
		Status:   StatusActive,
		Quota:    QuotaState{Signals: map[string]string{"used-percent": "70"}},
	}
	healthyLowUsage := &Auth{
		ID:       "healthy-low-usage",
		Provider: "codex",
		Status:   StatusActive,
		Quota:    QuotaState{Signals: map[string]string{"used-percent": "20"}},
	}

	if got := pickCodex2APIAuth([]*Auth{healthyHighUsage, healthyLowUsage}, &smoothWeightedState{}); got != healthyLowUsage {
		t.Fatalf("healthy selection = %q, want %q", got.ID, healthyLowUsage.ID)
	}
	if got := pickCodex2APIAuth([]*Auth{healthyLowUsage, priorityCandidate}, &smoothWeightedState{}); got != priorityCandidate {
		t.Fatalf("priority selection = %q, want %q", got.ID, priorityCandidate.ID)
	}
}

func TestAuthDispatchSlotsRespectRuntimeLimit(t *testing.T) {
	auth := &Auth{ID: "limited", ConcurrencyLimit: 1}
	if !auth.TryAcquireDispatchSlot() {
		t.Fatal("first dispatch slot acquisition failed")
	}
	if auth.TryAcquireDispatchSlot() {
		t.Fatal("second dispatch slot acquisition succeeded at limit")
	}
	if got := auth.ActiveDispatchRequests(); got != 1 {
		t.Fatalf("active requests = %d, want 1", got)
	}

	auth.SetConcurrencyLimit(0)
	if !auth.TryAcquireDispatchSlot() {
		t.Fatal("unlimited auth rejected a dispatch slot")
	}
	if got := auth.EffectiveConcurrencyLimit(); got != 0 {
		t.Fatalf("effective concurrency limit = %d, want 0", got)
	}
	auth.ReleaseDispatchSlot()
	auth.ReleaseDispatchSlot()
	if got := auth.ActiveDispatchRequests(); got != 0 {
		t.Fatalf("active requests after release = %d, want 0", got)
	}
}

func TestCodex2APIQuotaWeightedUnlimitedDistribution(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			for _, hold := range []bool{false, true} {
				for _, tc := range []struct {
					name, usageA, usageB   string
					requests, wantA, wantB int
				}{
					{"unequal", "10", "40", 50, 30, 20},
					{"equal", "20", "20", 50, 25, 25},
					{"unknown", "", "40", 80, 50, 30},
					{"warm", "85", "90", 50, 30, 20},
				} {
					t.Run(fmt.Sprintf("%s/wrapped=%t/mixed=%t/hold=%t", tc.name, wrapped, mixed, hold), func(t *testing.T) {
						var selector Selector = &Codex2APIDefaultSelector{}
						if wrapped {
							affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, ProviderFilters: []string{"cursor"}})
							t.Cleanup(affinity.Stop)
							selector = affinity
						}
						manager := NewManager(nil, selector, nil)
						manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{AuthConcurrencyLimit: new(0)}})
						manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
						for _, item := range []struct{ id, usage string }{{"a", tc.usageA}, {"b", tc.usageB}} {
							if _, err := manager.Register(t.Context(), &Auth{ID: item.id, Provider: "codex", Status: StatusActive,
								Quota: QuotaState{Signals: map[string]string{"x-codex-primary-used-percent": item.usage}},
							}); err != nil {
								t.Fatal(err)
							}
						}
						counts := map[string]int{}
						for range tc.requests {
							var selected *Auth
							var err error
							if mixed {
								selected, _, _, err = manager.pickNextMixed(t.Context(), []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
							} else {
								selected, _, err = manager.pickNext(t.Context(), "codex", "", cliproxyexecutor.Options{}, nil)
							}
							if err != nil {
								t.Fatal(err)
							}
							counts[selected.ID]++
							if selected.EffectiveConcurrencyLimit() != 0 {
								t.Fatal("test did not use unlimited concurrency")
							}
							if hold {
								t.Cleanup(selected.ReleaseDispatchSlot)
							} else {
								selected.ReleaseDispatchSlot()
								manager.MarkResult(t.Context(), Result{AuthID: selected.ID, Provider: "codex", Success: true, SkipQuotaObservation: true})
							}
						}
						if counts["a"] != tc.wantA || counts["b"] != tc.wantB {
							t.Fatalf("picks=%v, want a:b=%d:%d", counts, tc.wantA, tc.wantB)
						}
					})
				}
			}
		}
	}
}

func TestCodex2APIQuotaWeightedMixedProviders(t *testing.T) {
	scheduler := newSchedulerForTest(&Codex2APIDefaultSelector{},
		&Auth{ID: "a", Provider: "codex", Status: StatusActive, Quota: QuotaState{Signals: map[string]string{"used-percent": "10"}}},
		&Auth{ID: "b", Provider: "gemini", Status: StatusActive, Quota: QuotaState{Signals: map[string]string{"used-percent": "40"}}},
	)
	counts := map[string]int{}
	for range 50 {
		selected, _, err := scheduler.pickMixed(t.Context(), []string{"codex", "gemini"}, "", cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[selected.ID]++
		selected.ReleaseDispatchSlot()
	}
	if counts["a"] != 30 || counts["b"] != 20 {
		t.Fatalf("mixed picks=%v, want 30:20", counts)
	}
}

func TestCodex2APIQuotaWeightedUpdatesPreserveRotation(t *testing.T) {
	for _, rebuilt := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuilt=%t", rebuilt), func(t *testing.T) {
			a, b := &Auth{ID: "a", Provider: "codex", Status: StatusActive}, &Auth{ID: "b", Provider: "codex", Status: StatusActive}
			selector := &Codex2APIDefaultSelector{}
			scheduler := newSchedulerForTest(selector, a, b)
			counts := map[string]int{}
			for index := 0; index < 100; index++ {
				a.Quota.Signals = map[string]string{"used-percent": fmt.Sprintf("%.2f", 10+float64(index)/100)}
				b.Quota.Signals = map[string]string{"used-percent": fmt.Sprintf("%.2f", 40+float64(index)/100)}
				var selected *Auth
				var err error
				if rebuilt {
					scheduler.upsertAuth(a.Clone())
					scheduler.upsertAuth(b.Clone())
					selected, err = scheduler.pickSingle(t.Context(), "codex", "", cliproxyexecutor.Options{}, nil)
				} else {
					selected, err = selector.Pick(t.Context(), "codex", "", cliproxyexecutor.Options{}, []*Auth{a, b})
				}
				if err != nil {
					t.Fatal(err)
				}
				counts[selected.ID]++
				if rebuilt {
					selected.ReleaseDispatchSlot()
				}
			}
			if counts["a"] < 59 || counts["a"] > 61 || counts["b"] < 39 || counts["b"] > 41 {
				t.Fatalf("changing quota reset rotation: picks=%v, want approximately 60:40", counts)
			}
		})
	}
}

func TestCodex2APIQuotaWeightedPoolIsolation(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			var selector Selector = &Codex2APIDefaultSelector{}
			if wrapped {
				affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, ProviderFilters: []string{"cursor"}})
				t.Cleanup(affinity.Stop)
				selector = affinity
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			for _, item := range []struct {
				id, usage string
				degraded  bool
			}{
				{"normal-a", "10", false}, {"normal-b", "40", false},
				{"degraded-a", "20", true}, {"degraded-b", "20", true},
			} {
				if _, err := manager.Register(t.Context(), &Auth{ID: item.id, Provider: "codex", Status: StatusActive,
					Metadata: map[string]any{"degraded": item.degraded},
					Quota:    QuotaState{Signals: map[string]string{"used-percent": item.usage}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			counts := map[string]int{}
			for range 50 {
				for _, degraded := range []bool{false, true} {
					ctx := cliproxyexecutor.WithDegradedRouting(t.Context(), degraded)
					selected, _, _, err := manager.pickNextMixed(ctx, []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
					if err != nil {
						t.Fatal(err)
					}
					selected.ReleaseDispatchSlot()
					if selected.IsDegraded() != degraded {
						t.Fatal("quota weighting crossed scheduling pools")
					}
					counts[selected.ID]++
				}
			}
			want := map[string]int{"normal-a": 30, "normal-b": 20, "degraded-a": 25, "degraded-b": 25}
			if !reflect.DeepEqual(counts, want) {
				t.Fatalf("pool picks=%v, want %v", counts, want)
			}
		})
	}
}

func TestManagerConcurrencyReloadPreservesActiveSlots(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	cfg := &internalconfig.Config{}
	cfg.Routing.AuthConcurrencyLimit = new(1)
	manager.SetConfig(cfg)
	auth, errRegister := manager.Register(context.Background(), &Auth{ID: "old", Provider: "codex"})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	if !auth.TryAcquireDispatchSlot() {
		t.Fatal("failed to acquire initial slot")
	}
	defer auth.ReleaseDispatchSlot()
	manager.SetConfigSnapshot(&internalconfig.Config{Debug: true})
	newAuth, errNew := manager.Register(context.Background(), &Auth{ID: "new", Provider: "codex"})
	if errNew != nil {
		t.Fatal(errNew)
	}
	if auth.ActiveDispatchRequests() != 1 || auth.DispatchSlotAvailable() || newAuth.EffectiveConcurrencyLimit() != 1 {
		t.Fatalf("reload changed concurrency: active=%d available=%v new_limit=%d", auth.ActiveDispatchRequests(), auth.DispatchSlotAvailable(), newAuth.EffectiveConcurrencyLimit())
	}
	cfg.Routing.AuthConcurrencyLimit = new(0)
	manager.SetConfig(cfg)
	manager.SetConfigSnapshot(&internalconfig.Config{})
	if !auth.DispatchSlotAvailable() || newAuth.EffectiveConcurrencyLimit() != 0 || auth.ActiveDispatchRequests() != 1 {
		t.Fatal("explicit unlimited setting changed active slots or did not take effect")
	}
	fresh := NewManager(nil, nil, nil)
	restarted, errRestart := fresh.Register(context.Background(), &Auth{ID: "restart", Provider: "codex"})
	if errRestart != nil || restarted.EffectiveConcurrencyLimit() != 0 {
		t.Fatalf("new manager inherited runtime settings: err=%v", errRestart)
	}
}

func TestManagerConfigReloadPreservesCodexFingerprintConvergence(t *testing.T) {
	for _, method := range []string{"SetConfig", "SetConfigSnapshot", "ApplyConfigWithCooldownStateStore"} {
		t.Run(method, func(t *testing.T) {
			for _, state := range []struct {
				name     string
				previous *bool
				value    *bool
				want     *bool
			}{
				{name: "unset"},
				{name: "enabled", value: new(true), want: new(true)},
				{name: "inherit-enabled", previous: new(true), want: new(true)},
				{name: "disabled", previous: new(true), value: new(false), want: new(false)},
				{name: "inherit-disabled", previous: new(false), want: new(false)},
				{name: "reenabled", previous: new(false), value: new(true), want: new(true)},
			} {
				t.Run(state.name, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					initial := &internalconfig.Config{}
					initial.Routing.CodexFingerprintConvergence = state.previous
					manager.SetConfig(initial)
					previous := manager.runtimeConfigSnapshot()
					if previous == initial || (state.previous != nil && previous.Routing.CodexFingerprintConvergence == state.previous) {
						t.Fatal("manager initial config shares the caller pointer")
					}
					cfg := &internalconfig.Config{Debug: true}
					cfg.Routing.CodexFingerprintConvergence = state.value
					before := cfg.CloneForRuntime()
					switch method {
					case "SetConfig":
						manager.SetConfig(cfg)
					case "SetConfigSnapshot":
						manager.SetConfigSnapshot(cfg)
					case "ApplyConfigWithCooldownStateStore":
						if !manager.ApplyConfigWithCooldownStateStore(context.Background(), cfg, nil) {
							t.Fatal("config application failed")
						}
					}
					snapshot := manager.runtimeConfigSnapshot()
					if snapshot == nil || snapshot == cfg || snapshot == previous {
						t.Fatal("manager did not install an independent config snapshot")
					}
					if !snapshot.Debug || !reflect.DeepEqual(snapshot.Routing.CodexFingerprintConvergence, state.want) || snapshot.Routing.AuthConcurrencyLimit != nil {
						t.Fatalf("manager routing = %+v", snapshot.Routing)
					}
					if !reflect.DeepEqual(cfg, before) {
						t.Fatal("manager changed the caller config")
					}
					if state.want != nil && (snapshot.Routing.CodexFingerprintConvergence == cfg.Routing.CodexFingerprintConvergence || snapshot.Routing.CodexFingerprintConvergence == previous.Routing.CodexFingerprintConvergence) {
						t.Fatal("manager convergence shares the caller or previous config pointer")
					}
					manager.SetConfig(nil)
					if !reflect.DeepEqual(manager.runtimeConfigSnapshot().Routing.CodexFingerprintConvergence, state.want) {
						t.Fatal("nil config reload lost the runtime convergence setting")
					}
				})
			}
		})
	}
}

func TestSchedulerSkipsAuthAtConcurrencyLimit(t *testing.T) {
	busy := &Auth{ID: "busy", Provider: "codex", Status: StatusActive, ConcurrencyLimit: 1}
	ready := &Auth{ID: "ready", Provider: "codex", Status: StatusActive}
	if !busy.TryAcquireDispatchSlot() {
		t.Fatal("failed to occupy busy auth")
	}
	defer busy.ReleaseDispatchSlot()

	scheduler := newSchedulerForTest(&Codex2APIDefaultSelector{}, busy, ready)
	picked, errPick := scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if picked == nil || picked.ID != ready.ID {
		t.Fatalf("picked auth = %#v, want %q", picked, ready.ID)
	}
	defer picked.ReleaseDispatchSlot()
}

func TestSchedulerReturns429WhenAllReadyAuthsAreAtConcurrencyLimit(t *testing.T) {
	busy := &Auth{ID: "busy", Provider: "codex", Status: StatusActive, ConcurrencyLimit: 1}
	if !busy.TryAcquireDispatchSlot() {
		t.Fatal("failed to occupy busy auth")
	}
	defer busy.ReleaseDispatchSlot()

	scheduler := newSchedulerForTest(&Codex2APIDefaultSelector{}, busy)
	_, errPick := scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick == nil {
		t.Fatal("pickSingle() error = nil, want concurrency limit error")
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr.Code != ErrorCodeConcurrencyLimit {
		t.Fatalf("pickSingle() error = %#v, want %s", errPick, ErrorCodeConcurrencyLimit)
	}
	if authErr.HTTPStatus != http.StatusTooManyRequests || authErr.Message != ErrorMessageConcurrencyLimit {
		t.Fatalf("concurrency error = %#v, want HTTP 429 and exact message", authErr)
	}
}

func TestManagerLegacySelectionSkipsAuthAtConcurrencyLimit(t *testing.T) {
	manager := NewManager(nil, &Codex2APIDefaultSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{provider: "codex"}

	busy := &Auth{ID: "a-busy", Provider: "codex", Status: StatusActive, ConcurrencyLimit: 1}
	ready := &Auth{ID: "b-ready", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), busy); errRegister != nil {
		t.Fatalf("register busy auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), ready); errRegister != nil {
		t.Fatalf("register ready auth: %v", errRegister)
	}
	registeredBusy, ok := manager.GetByID(busy.ID)
	if !ok || registeredBusy == nil || !registeredBusy.TryAcquireDispatchSlot() {
		t.Fatal("failed to occupy registered busy auth")
	}
	defer registeredBusy.ReleaseDispatchSlot()

	selected, errSelect := manager.SelectAuth(context.Background(), "codex", "", cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuth() error = %v", errSelect)
	}
	if selected == nil || selected.ID != ready.ID {
		t.Fatalf("SelectAuth() auth = %#v, want %q", selected, ready.ID)
	}
	if got := selected.ActiveDispatchRequests(); got != 0 {
		t.Fatalf("selected auth active requests = %d, want 0 after SelectAuth", got)
	}
}

func TestManagerLegacySelectionFiltersBusyCandidatesBeforeScheduling(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
				Fallback:        &Codex2APIDefaultSelector{},
				ProviderFilters: []string{"cursor"},
			})
			t.Cleanup(selector.Stop)
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			plugin := &fakePluginScheduler{}
			manager.SetPluginScheduler(plugin)
			const busyCount = 32
			for index := 0; index <= busyCount; index++ {
				auth, errRegister := manager.Register(t.Context(), &Auth{
					ID:               fmt.Sprintf("capacity-%02d", index),
					Provider:         "codex",
					Status:           StatusActive,
					ConcurrencyLimit: 1,
				})
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				if index < busyCount {
					if !auth.TryAcquireDispatchSlot() {
						t.Fatal("failed to occupy auth")
					}
					t.Cleanup(auth.ReleaseDispatchSlot)
				}
			}
			var selected *Auth
			var errPick error
			if mixed {
				selected, _, _, errPick = manager.pickNextMixed(t.Context(), []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
			} else {
				selected, _, errPick = manager.pickNext(t.Context(), "codex", "", cliproxyexecutor.Options{}, nil)
			}
			if errPick != nil {
				t.Fatal(errPick)
			}
			t.Cleanup(selected.ReleaseDispatchSlot)
			if selected.ID != fmt.Sprintf("capacity-%02d", busyCount) || selected.ActiveDispatchRequests() != 1 {
				t.Fatalf("selection = %q active=%d, want the reserved free auth", selected.ID, selected.ActiveDispatchRequests())
			}
			if plugin.calls != 1 {
				t.Fatalf("scheduler calls = %d, want one pass regardless of busy account count", plugin.calls)
			}
			if got := len(plugin.requests[0].Candidates); got != 1 {
				t.Fatalf("scheduler candidates = %d, want only the free auth", got)
			}
			for _, auth := range manager.List() {
				if auth.Success != 0 || auth.Failed != 0 {
					t.Fatal("capacity filtering must not record an upstream result")
				}
			}
		})
	}
}

func TestManagerLegacySelectionRechecksCapacityAfterScheduling(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			manager := NewManager(nil, &Codex2APIDefaultSelector{}, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			for _, id := range []string{"a-raced", "b-free"} {
				if _, errRegister := manager.Register(t.Context(), &Auth{ID: id, Provider: "codex", Status: StatusActive, ConcurrencyLimit: 1}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			raced, _ := manager.GetByID("a-raced")
			plugin := &fakePluginScheduler{}
			plugin.pick = func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				if plugin.calls == 1 {
					if !raced.TryAcquireDispatchSlot() {
						t.Fatal("failed to occupy auth after candidate snapshot")
					}
					t.Cleanup(raced.ReleaseDispatchSlot)
				}
				return pluginapi.SchedulerPickResponse{}, false, nil
			}
			manager.SetPluginScheduler(plugin)
			var selected *Auth
			var errPick error
			if mixed {
				selected, _, _, errPick = manager.pickNextMixed(t.Context(), []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
			} else {
				selected, _, errPick = manager.pickNext(t.Context(), "codex", "", cliproxyexecutor.Options{}, nil)
			}
			if errPick != nil {
				t.Fatal(errPick)
			}
			t.Cleanup(selected.ReleaseDispatchSlot)
			if selected.ID != "b-free" || selected.ActiveDispatchRequests() != 1 || raced.ActiveDispatchRequests() != 1 {
				t.Fatalf("capacity race was not respected: selected=%q active=%d raced=%d", selected.ID, selected.ActiveDispatchRequests(), raced.ActiveDispatchRequests())
			}
		})
	}
}

func TestAvailableAuthsForSelectorPreservesPriorityWhenFilteringCapacity(t *testing.T) {
	busy := &Auth{ID: "high-busy", Provider: "codex", Attributes: map[string]string{"priority": "10"}, ConcurrencyLimit: 1}
	free := &Auth{ID: "low-free", Provider: "codex"}
	if !busy.TryAcquireDispatchSlot() {
		t.Fatal("failed to occupy high priority auth")
	}
	t.Cleanup(busy.ReleaseDispatchSlot)
	free.ensureDispatchState()
	manager := NewManager(nil, nil, nil)
	_, _, errAvailable := manager.availableAuthsForSelector(&Codex2APIDefaultSelector{}, []*Auth{busy, free}, "codex", "", time.Now())
	var capacityErr *Error
	if !errors.As(errAvailable, &capacityErr) || capacityErr.Code != ErrorCodeConcurrencyLimit {
		t.Fatalf("priority-scoped selection error = %v, want concurrency limit", errAvailable)
	}

	affinity := NewSessionAffinitySelector(&Codex2APIDefaultSelector{})
	t.Cleanup(affinity.Stop)
	priorityAuths, selectorAuths, errAvailable := manager.availableAuthsForSelector(affinity, []*Auth{busy, free}, "codex", "", time.Now())
	if errAvailable != nil {
		t.Fatal(errAvailable)
	}
	if len(priorityAuths) != 1 || priorityAuths[0].ID != free.ID || len(selectorAuths) != 1 || selectorAuths[0].ID != free.ID {
		t.Fatal("affinity selection must retain the free lower-priority tier for failover")
	}
}

func TestManagerLegacySelectionReturns429WhenAllAuthsAreAtConcurrencyLimit(t *testing.T) {
	manager := NewManager(nil, &Codex2APIDefaultSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{provider: "codex"}
	auth := &Auth{ID: "busy", Provider: "codex", Status: StatusActive, ConcurrencyLimit: 1}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registered, ok := manager.GetByID(auth.ID)
	if !ok || registered == nil || !registered.TryAcquireDispatchSlot() {
		t.Fatal("failed to occupy registered auth")
	}
	defer registered.ReleaseDispatchSlot()

	_, errSelect := manager.SelectAuth(context.Background(), "codex", "", cliproxyexecutor.Options{})
	if errSelect == nil {
		t.Fatal("SelectAuth() error = nil, want concurrency limit error")
	}
	var authErr *Error
	if !errors.As(errSelect, &authErr) || authErr.Code != ErrorCodeConcurrencyLimit {
		t.Fatalf("SelectAuth() error = %#v, want %s", errSelect, ErrorCodeConcurrencyLimit)
	}
	if authErr.HTTPStatus != http.StatusTooManyRequests || authErr.Message != ErrorMessageConcurrencyLimit {
		t.Fatalf("concurrency error = %#v, want HTTP 429 and exact message", authErr)
	}
}

func TestManagerExecutionReturns429AtConcurrencyLimitWithoutRetry(t *testing.T) {
	manager := NewManager(nil, &Codex2APIDefaultSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{provider: "codex"}
	manager.requestRetry.Store(10)
	auth := &Auth{ID: "busy", Provider: "codex", Status: StatusActive, ConcurrencyLimit: 1}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registered, ok := manager.GetByID(auth.ID)
	if !ok || registered == nil || !registered.TryAcquireDispatchSlot() {
		t.Fatal("failed to occupy registered auth")
	}
	defer registered.ReleaseDispatchSlot()

	for name, execute := range map[string]func() error{
		"execute": func() error {
			_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
			return err
		},
		"count": func() error {
			_, err := manager.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
			return err
		},
		"stream": func() error {
			stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{Stream: true})
			if stream != nil {
				return errors.New("ExecuteStream() returned a stream before an account slot was available")
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := execute()
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.Code != ErrorCodeConcurrencyLimit || authErr.HTTPStatus != http.StatusTooManyRequests {
				t.Fatalf("execution error = %#v, want HTTP 429 concurrency limit", err)
			}
			if wait, retry := manager.shouldRetryAfterError(err, 0, []string{"codex"}, "", time.Minute); retry || wait != 0 {
				t.Fatalf("concurrency limit retry = %v wait=%s, want immediate rejection", retry, wait)
			}
		})
	}
}

func TestSchedulerPick_WeightedRoundRobin(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{ID: "a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "5"}},
		&Auth{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "3"}},
		&Auth{ID: "c", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "2"}},
	)

	counts := make(map[string]int)
	for index := 0; index < 100; index++ {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		counts[got.ID]++
	}
	want := map[string]int{"a": 50, "b": 30, "c": 20}
	for authID, wantCount := range want {
		if counts[authID] != wantCount {
			t.Fatalf("auth %q picks = %d, want %d", authID, counts[authID], wantCount)
		}
	}
}

func TestManagerLoad_WeightedRoundRobinUsesPersistedMetadataWeight(t *testing.T) {
	t.Parallel()

	manager := NewManager(&schedulerLoadStore{auths: []*Auth{
		{ID: "a", Provider: "gemini", Metadata: map[string]any{AttributeWeight: float64(5)}},
		{ID: "b", Provider: "gemini", Metadata: map[string]any{AttributeWeight: float64(1)}},
	}}, &WeightedRoundRobinSelector{}, nil)
	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}

	counts := make(map[string]int)
	for index := 0; index < 60; index++ {
		got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		counts[got.ID]++
	}
	if counts["a"] != 50 || counts["b"] != 10 {
		t.Fatalf("metadata-weighted picks = %#v, want a:b=50:10", counts)
	}
}

func TestSchedulerPick_WeightedRoundRobinResetsCreditsWhenWeightsChange(t *testing.T) {
	t.Parallel()

	authA := &Auth{ID: "a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1000000"}}
	authB := &Auth{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}}
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{}, authA, authB)
	for index := 0; index < 1000; index++ {
		if _, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil); errPick != nil {
			t.Fatalf("warmup pickSingle() #%d error = %v", index, errPick)
		}
	}

	authA.Attributes[AttributeWeight] = "1"
	scheduler.upsertAuth(authA)
	counts := make(map[string]int)
	for index := 0; index < 20; index++ {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() after weight change #%d error = %v", index, errPick)
		}
		counts[got.ID]++
	}
	if counts["a"] != 10 || counts["b"] != 10 {
		t.Fatalf("picks after weight change = %#v, want a:b=10:10", counts)
	}
}

func TestSchedulerPick_WeightedWebsocketResetsCreditsWhenWeightsChange(t *testing.T) {
	t.Parallel()

	authA := &Auth{ID: "a", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1000000", "websockets": "true"}}
	authB := &Auth{ID: "b", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1", "websockets": "true"}}
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{}, authA, authB)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	for index := 0; index < 1000; index++ {
		if _, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil); errPick != nil {
			t.Fatalf("warmup websocket pickSingle() #%d error = %v", index, errPick)
		}
	}

	authA.Attributes[AttributeWeight] = "1"
	scheduler.upsertAuth(authA)
	counts := make(map[string]int)
	for index := 0; index < 20; index++ {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("websocket pickSingle() after weight change #%d error = %v", index, errPick)
		}
		counts[got.ID]++
	}
	if counts["a"] != 10 || counts["b"] != 10 {
		t.Fatalf("websocket picks after weight change = %#v, want a:b=10:10", counts)
	}
}

func TestManagerLegacyWeightedRoundRobinKeepsIndependentAliasPrefixedModelState(t *testing.T) {
	manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.SetPluginScheduler(&fakePluginScheduler{})

	auths := []*Auth{
		{ID: "a-heavy", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "3"}},
		{ID: "a-light", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
		{ID: "b-light", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}},
		{ID: "b-heavy", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "3"}},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	registerSchedulerModels(t, "gemini", "team-a/shared", "a-heavy", "a-light")
	registerSchedulerModels(t, "gemini", "team-b/shared", "b-light", "b-heavy")

	counts := make(map[string]int)
	for index := 0; index < 40; index++ {
		for _, model := range []string{"team-a/shared", "team-b/shared"} {
			got, _, errPick := manager.pickNext(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickNext(%q) #%d error = %v", model, index, errPick)
			}
			counts[got.ID]++
		}
	}
	want := map[string]int{"a-heavy": 30, "a-light": 10, "b-light": 10, "b-heavy": 30}
	for authID, wantCount := range want {
		if counts[authID] != wantCount {
			t.Fatalf("auth %q picks = %d, want %d; all=%#v", authID, counts[authID], wantCount, counts)
		}
	}
}

func TestSchedulerPick_WeightedRoundRobinSkipsNonPositiveWeightPriorityTier(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{ID: "excluded", Provider: "gemini", Attributes: map[string]string{"priority": "10", AttributeWeight: "0"}},
		&Auth{ID: "available", Provider: "gemini", Attributes: map[string]string{"priority": "0", AttributeWeight: "1"}},
	)
	got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "available" {
		t.Fatalf("pickSingle() auth = %#v, want available", got)
	}
}

func TestSchedulerPick_FillFirstSticksToFirstReady(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "b", Provider: "gemini"},
		&Auth{ID: "a", Provider: "gemini"},
		&Auth{ID: "c", Provider: "gemini"},
	)

	for index := 0; index < 3; index++ {
		got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != "a" {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, "a")
		}
	}
}

func TestSchedulerPick_RemainingQuotaPrefersLowerUsage(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RemainingQuotaSelector{},
		&Auth{ID: "used-more", Provider: "codex", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Used-Percent": "80"}}},
		&Auth{ID: "used-less", Provider: "codex", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Used-Percent": "20"}}},
	)

	got, errPick := scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "used-less" {
		t.Fatalf("pickSingle() auth = %#v, want used-less", got)
	}
}

func TestSchedulerPick_FillFirstPrefersHigherUsageWhenObserved(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "used-less", Provider: "codex", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Used-Percent": "20"}}},
		&Auth{ID: "used-more", Provider: "codex", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Used-Percent": "80"}}},
	)

	got, errPick := scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "used-more" {
		t.Fatalf("pickSingle() auth = %#v, want used-more", got)
	}
}

func TestSchedulerPick_PromotesExpiredCooldownBeforePick(t *testing.T) {
	t.Parallel()

	model := "gemini-2.5-pro"
	registerSchedulerModels(t, "gemini", model, "cooldown-expired")
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{
			ID:       "cooldown-expired",
			Provider: "gemini",
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(-1 * time.Second),
				},
			},
		},
	)

	got, errPick := scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickSingle() auth = nil")
	}
	if got.ID != "cooldown-expired" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "cooldown-expired")
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex"},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_XAIWebsocketPrefersWebsocketEnabledSubset(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "xai-http", Provider: "xai"},
		&Auth{ID: "xai-ws-a", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
		&Auth{ID: "xai-ws-b", Provider: "xai", Attributes: map[string]string{"websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"xai-ws-a", "xai-ws-b", "xai-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "xai", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_CodexWebsocketPrefersWebsocketEnabledAcrossPriorities(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "codex-http", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
		&Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"priority": "0", "websockets": "true"}},
	)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	want := []string{"codex-ws-a", "codex-ws-b", "codex-ws-a"}
	for index, wantID := range want {
		got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickSingle() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestSchedulerPick_MixedProvidersUsesWeightedProviderRotationOverReadyCandidates(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini"},
		&Auth{ID: "gemini-b", Provider: "gemini"},
		&Auth{ID: "claude-a", Provider: "claude"},
	)

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestSchedulerPick_MixedProvidersWeightedRoundRobin(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "5"}},
		&Auth{ID: "claude-b", Provider: "claude", Attributes: map[string]string{AttributeWeight: "3"}},
		&Auth{ID: "claude-c", Provider: "claude", Attributes: map[string]string{AttributeWeight: "2"}},
	)

	counts := make(map[string]int)
	for index := 0; index < 100; index++ {
		got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil || provider == "" {
			t.Fatalf("pickMixed() #%d returned auth=%v provider=%q", index, got, provider)
		}
		counts[got.ID]++
	}
	want := map[string]int{"gemini-a": 50, "claude-b": 30, "claude-c": 20}
	for authID, wantCount := range want {
		if counts[authID] != wantCount {
			t.Fatalf("auth %q picks = %d, want %d", authID, counts[authID], wantCount)
		}
	}
}

func TestSchedulerPick_MixedProvidersResetsCreditsWhenWeightsChange(t *testing.T) {
	t.Parallel()

	authA := &Auth{ID: "gemini-a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1000000"}}
	authB := &Auth{ID: "claude-b", Provider: "claude", Attributes: map[string]string{AttributeWeight: "1"}}
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{}, authA, authB)
	providers := []string{"gemini", "claude"}
	for index := 0; index < 1000; index++ {
		if _, _, errPick := scheduler.pickMixed(context.Background(), providers, "", cliproxyexecutor.Options{}, nil); errPick != nil {
			t.Fatalf("warmup pickMixed() #%d error = %v", index, errPick)
		}
	}

	authA.Attributes[AttributeWeight] = "1"
	scheduler.upsertAuth(authA)
	counts := make(map[string]int)
	for index := 0; index < 20; index++ {
		got, _, errPick := scheduler.pickMixed(context.Background(), providers, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() after weight change #%d error = %v", index, errPick)
		}
		counts[got.ID]++
	}
	if counts[authA.ID] != 10 || counts[authB.ID] != 10 {
		t.Fatalf("mixed picks after weight change = %#v, want 10 each", counts)
	}
}

func TestSchedulerPickMixed_RetryTriedFilterPreservesSmoothWeightedDistribution(t *testing.T) {
	t.Parallel()

	authA := &Auth{ID: "auth-a", Provider: "provider-a"}
	authB := &Auth{ID: "auth-b", Provider: "provider-b"}
	authC := &Auth{ID: "auth-c", Provider: "provider-c"}
	authD := &Auth{ID: "auth-d", Provider: "provider-d"}
	auths := []*Auth{authA, authB, authC, authD}

	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{}, auths...)
	providers := []string{"provider-a", "provider-b", "provider-c", "provider-d"}

	// Simulate retries where auth-a failed and is in the tried filter:
	// Verify that retry picks rotate smoothly across auth-b, auth-c, auth-d without alphabetical bias towards auth-b.
	retryCounts := make(map[string]int)
	tried := map[string]struct{}{"auth-a": {}}
	for index := 0; index < 30; index++ {
		picked, _, errPick := scheduler.pickMixed(context.Background(), providers, "", cliproxyexecutor.Options{}, tried)
		if errPick != nil {
			t.Fatalf("pickMixed(tried) error = %v", errPick)
		}
		retryCounts[picked.ID]++
	}

	for _, authID := range []string{"auth-b", "auth-c", "auth-d"} {
		if retryCounts[authID] != 10 {
			t.Fatalf("auth %q retry picks = %d, want 10 (even distribution without alphabetical bias, counts=%#v)", authID, retryCounts[authID], retryCounts)
		}
	}
}

func TestReadyViewRoundRobinPreservesSuccessorAcrossRebuild(t *testing.T) {
	t.Parallel()

	entry := func(id string) *scheduledAuth {
		return &scheduledAuth{auth: &Auth{ID: id}}
	}

	t.Run("a cooling resumes at b", func(t *testing.T) {
		original := readyView{
			flat: []*scheduledAuth{entry("A"), entry("B"), entry("C")},
		}
		if got := original.pickRoundRobin(nil); got == nil || got.auth.ID != "A" {
			t.Fatalf("first pick = %v, want A", got)
		}

		state := snapshotReadyViewCursors(original)
		rebuilt := readyView{
			flat: []*scheduledAuth{entry("B"), entry("C")},
		}
		restoreReadyViewCursors(&rebuilt, state)

		got := rebuilt.pickRoundRobin(nil)
		if got == nil || got.auth.ID != "B" {
			t.Fatalf("pick after A cooldown = %v, want B", got)
		}
	})

	t.Run("b cooling resumes at c", func(t *testing.T) {
		original := readyView{
			flat: []*scheduledAuth{entry("A"), entry("B"), entry("C")},
		}
		// Pick A, then B
		if got := original.pickRoundRobin(nil); got == nil || got.auth.ID != "A" {
			t.Fatalf("first pick = %v, want A", got)
		}
		if got := original.pickRoundRobin(nil); got == nil || got.auth.ID != "B" {
			t.Fatalf("second pick = %v, want B", got)
		}

		state := snapshotReadyViewCursors(original)
		rebuilt := readyView{
			flat: []*scheduledAuth{entry("A"), entry("C")},
		}
		restoreReadyViewCursors(&rebuilt, state)

		got := rebuilt.pickRoundRobin(nil)
		if got == nil || got.auth.ID != "C" {
			t.Fatalf("pick after B cooldown = %v, want C", got)
		}
	})

	t.Run("c cooling wraps to a", func(t *testing.T) {
		original := readyView{
			flat: []*scheduledAuth{entry("A"), entry("B"), entry("C")},
		}
		// Pick A, B, C
		for _, want := range []string{"A", "B", "C"} {
			if got := original.pickRoundRobin(nil); got == nil || got.auth.ID != want {
				t.Fatalf("pick = %v, want %s", got, want)
			}
		}

		state := snapshotReadyViewCursors(original)
		rebuilt := readyView{
			flat: []*scheduledAuth{entry("A"), entry("B")},
		}
		restoreReadyViewCursors(&rebuilt, state)

		got := rebuilt.pickRoundRobin(nil)
		if got == nil || got.auth.ID != "A" {
			t.Fatalf("pick after C cooldown = %v, want A", got)
		}
	})

	t.Run("recovery preserves successor", func(t *testing.T) {
		original := readyView{
			flat: []*scheduledAuth{entry("B"), entry("C")},
		}
		if got := original.pickRoundRobin(nil); got == nil || got.auth.ID != "B" {
			t.Fatalf("first pick = %v, want B", got)
		}

		state := snapshotReadyViewCursors(original)
		// A recovered and is prepended back
		rebuilt := readyView{
			flat: []*scheduledAuth{entry("A"), entry("B"), entry("C")},
		}
		restoreReadyViewCursors(&rebuilt, state)

		got := rebuilt.pickRoundRobin(nil)
		if got == nil || got.auth.ID != "C" {
			t.Fatalf("pick after A recovery = %v, want C", got)
		}
	})

	t.Run("retry exclusion resumes without rebuild", func(t *testing.T) {
		view := readyView{
			flat: []*scheduledAuth{entry("A"), entry("B"), entry("C")},
		}
		if got := view.pickRoundRobin(nil); got == nil || got.auth.ID != "A" {
			t.Fatalf("first pick = %v, want A", got)
		}
		got := view.pickRoundRobin(func(candidate *scheduledAuth) bool {
			return candidate.auth.ID != "B"
		})
		if got == nil || got.auth.ID != "C" {
			t.Fatalf("pick after excluding B = %v, want C", got)
		}
	})

	t.Run("multiple cooldown skips to first surviving successor", func(t *testing.T) {
		original := readyView{
			flat: []*scheduledAuth{entry("A"), entry("B"), entry("C"), entry("D")},
		}
		if got := original.pickRoundRobin(nil); got == nil || got.auth.ID != "A" {
			t.Fatalf("first pick = %v, want A", got)
		}

		state := snapshotReadyViewCursors(original)
		rebuilt := readyView{
			flat: []*scheduledAuth{entry("C"), entry("D")},
		}
		restoreReadyViewCursors(&rebuilt, state)

		got := rebuilt.pickRoundRobin(nil)
		if got == nil || got.auth.ID != "C" {
			t.Fatalf("pick after A and B cooldown = %v, want C", got)
		}
	})
}

func TestScheduledSuccessorIndex_WrapsAndSkipsFilteredCandidates(t *testing.T) {
	t.Parallel()

	entries := []*scheduledAuth{
		{auth: &Auth{ID: "aaa"}},
		{auth: &Auth{ID: "ccc"}},
		{auth: &Auth{ID: "eee"}},
	}
	tests := []struct {
		name   string
		lastID string
		want   int
	}{
		{name: "no previous pick starts at head", lastID: "", want: 0},
		{name: "resumes after previous pick", lastID: "aaa", want: 1},
		{name: "resumes after filtered-out pick", lastID: "bbb", want: 1},
		{name: "wraps at the end of the ring", lastID: "eee", want: 0},
		{name: "wraps for removed trailing pick", lastID: "zzz", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scheduledSuccessorIndex(entries, tt.lastID); got != tt.want {
				t.Fatalf("scheduledSuccessorIndex(%q) = %d, want %d", tt.lastID, got, tt.want)
			}
		})
	}
	if got := scheduledSuccessorIndex(nil, "aaa"); got != 0 {
		t.Fatalf("scheduledSuccessorIndex(nil, aaa) = %d, want 0", got)
	}
}

func TestManagerRoundRobinPreservesSuccessorAcrossCooldown(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	model := "test-successor-model"
	authIDs := []string{"successor-auth-a", "successor-auth-b", "successor-auth-c"}
	registerSchedulerModels(t, "gemini", model, authIDs...)

	for _, id := range authIDs {
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle #1 error = %v", errPick)
	}
	if got == nil || got.ID != "successor-auth-a" {
		t.Fatalf("pickSingle #1 = %v, want successor-auth-a", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "successor-auth-a",
		Provider: "gemini",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "rate limit"},
	})

	got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle #2 after successor-auth-a cooldown error = %v", errPick)
	}
	if got == nil || got.ID != "successor-auth-b" {
		t.Fatalf("pickSingle #2 after successor-auth-a cooldown = %v, want successor-auth-b", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "successor-auth-b",
		Provider: "gemini",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "rate limit"},
	})

	got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle #3 after successor-auth-b cooldown error = %v", errPick)
	}
	if got == nil || got.ID != "successor-auth-c" {
		t.Fatalf("pickSingle #3 after successor-auth-b cooldown = %v, want successor-auth-c", got)
	}
}

func TestSchedulerPick_RoundRobinPreservesWebsocketSuccessorAcrossCooldown(t *testing.T) {
	t.Parallel()

	wsA := &Auth{ID: "codex-ws-a", Provider: "codex", Attributes: map[string]string{"websockets": "true"}}
	wsB := &Auth{ID: "codex-ws-b", Provider: "codex", Attributes: map[string]string{"websockets": "true"}}
	wsC := &Auth{ID: "codex-ws-c", Provider: "codex", Attributes: map[string]string{"websockets": "true"}}
	httpOnly := &Auth{ID: "codex-http", Provider: "codex"}
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, httpOnly, wsA, wsB, wsC)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, errPick := scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() first error = %v", errPick)
	}
	if got == nil || got.ID != "codex-ws-a" {
		t.Fatalf("pickSingle() first = %v, want codex-ws-a", got)
	}

	wsA.Unavailable = true
	wsA.NextRetryAfter = time.Now().Add(time.Hour)
	scheduler.upsertAuth(wsA)

	got, errPick = scheduler.pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() after ws-a cooldown error = %v", errPick)
	}
	if got == nil || got.ID != "codex-ws-b" {
		t.Fatalf("pickSingle() after ws-a cooldown = %v, want codex-ws-b", got)
	}
}

func TestSchedulerPick_MixedProvidersPrefersHighestPriorityTier(t *testing.T) {
	t.Parallel()

	model := "gpt-default"
	registerSchedulerModels(t, "provider-low", model, "low")
	registerSchedulerModels(t, "provider-high-a", model, "high-a")
	registerSchedulerModels(t, "provider-high-b", model, "high-b")

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "low", Provider: "provider-low", Attributes: map[string]string{"priority": "4"}},
		&Auth{ID: "high-a", Provider: "provider-high-a", Attributes: map[string]string{"priority": "7"}},
		&Auth{ID: "high-b", Provider: "provider-high-b", Attributes: map[string]string{"priority": "7"}},
	)

	providers := []string{"provider-low", "provider-high-a", "provider-high-b"}
	wantProviders := []string{"provider-high-a", "provider-high-b", "provider-high-a", "provider-high-b"}
	wantIDs := []string{"high-a", "high-b", "high-a", "high-b"}
	for index := range wantProviders {
		got, provider, errPick := scheduler.pickMixed(context.Background(), providers, model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_UsesWeightedProviderRotationBeforeCredentialRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, map[string]struct{}{})
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_PickNextMixed_DisallowFreeAuthSkipsCodexFreePlan(t *testing.T) {
	t.Parallel()

	model := "gpt-5.4-mini"
	registerSchedulerModels(t, "codex", model, "codex-a-free", "codex-b-plus")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}); errRegister != nil {
		t.Fatalf("Register(codex-a-free) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "codex-b-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}); errRegister != nil {
		t.Fatalf("Register(codex-b-plus) error = %v", errRegister)
	}

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true},
	}
	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"codex"}, model, opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "codex" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "codex")
	}
	if got.ID != "codex-b-plus" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "codex-b-plus")
	}
}

func TestManagerPluginSchedulerSelectsAuthID(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-b"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{Stream: true}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if got.ID != "auth-b" {
		t.Fatalf("pickNext() auth.ID = %q, want %q", got.ID, "auth-b")
	}
	if scheduler.calls != 1 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 1)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want %d", len(scheduler.requests), 1)
	}
	if !scheduler.requests[0].Stream {
		t.Fatalf("scheduler request Stream = false, want true")
	}
}

func TestManagerPluginSchedulerAcrossPriorities(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	authHigh := &Auth{ID: "auth-high", Provider: "gemini", Attributes: map[string]string{"priority": "10", "weight": "100"}}
	authLow := &Auth{ID: "auth-low", Provider: "gemini", Attributes: map[string]string{"priority": "5", "weight": "50"}}
	if _, errRegister := manager.Register(context.Background(), authHigh); errRegister != nil {
		t.Fatalf("Register(authHigh) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), authLow); errRegister != nil {
		t.Fatalf("Register(authLow) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		acrossPriorities: true,
		resp:             pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-low"},
		handled:          true,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want 1", len(scheduler.requests))
	}
	if len(scheduler.requests[0].Candidates) != 2 {
		t.Fatalf("Candidates count = %d, want 2", len(scheduler.requests[0].Candidates))
	}
	if got == nil || got.ID != "auth-low" {
		t.Fatalf("picked auth = %v, want auth-low", got)
	}
}

func TestManagerPluginSchedulerAcrossPrioritiesMixed(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	authHigh := &Auth{ID: "auth-high", Provider: "gemini", Attributes: map[string]string{"priority": "10", "weight": "100"}}
	authLow := &Auth{ID: "auth-low", Provider: "claude", Attributes: map[string]string{"priority": "5", "weight": "50"}}
	if _, errRegister := manager.Register(context.Background(), authHigh); errRegister != nil {
		t.Fatalf("Register(authHigh) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), authLow); errRegister != nil {
		t.Fatalf("Register(authLow) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		acrossPriorities: true,
		resp:             pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-low"},
		handled:          true,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want 1", len(scheduler.requests))
	}
	if len(scheduler.requests[0].Candidates) != 2 {
		t.Fatalf("Candidates count = %d, want 2", len(scheduler.requests[0].Candidates))
	}
	if got == nil || got.ID != "auth-low" {
		t.Fatalf("picked auth = %v, want auth-low", got)
	}
	if provider != "claude" {
		t.Fatalf("provider = %q, want claude", provider)
	}
}

func TestManagerPluginSchedulerDefaultHighestPriorityOnly(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	authHigh := &Auth{ID: "auth-high", Provider: "gemini", Attributes: map[string]string{"priority": "10", "weight": "100"}}
	authLow := &Auth{ID: "auth-low", Provider: "gemini", Attributes: map[string]string{"priority": "5", "weight": "50"}}
	if _, errRegister := manager.Register(context.Background(), authHigh); errRegister != nil {
		t.Fatalf("Register(authHigh) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), authLow); errRegister != nil {
		t.Fatalf("Register(authLow) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		acrossPriorities: false, // default opt-in false
		resp:             pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-high"},
		handled:          true,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want 1", len(scheduler.requests))
	}
	if len(scheduler.requests[0].Candidates) != 1 {
		t.Fatalf("Candidates count = %d, want 1", len(scheduler.requests[0].Candidates))
	}
	if got == nil || got.ID != "auth-high" {
		t.Fatalf("picked auth = %v, want auth-high", got)
	}
}

func TestManagerPluginSchedulerAcrossPrioritiesUnhandledFallsBackToHighestPriority(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	authHigh := &Auth{ID: "auth-high", Provider: "gemini", Attributes: map[string]string{"priority": "10", "weight": "100"}}
	authLow := &Auth{ID: "auth-low", Provider: "gemini", Attributes: map[string]string{"priority": "5", "weight": "50"}}
	if _, errRegister := manager.Register(context.Background(), authHigh); errRegister != nil {
		t.Fatalf("Register(authHigh) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), authLow); errRegister != nil {
		t.Fatalf("Register(authLow) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		acrossPriorities: true,
		handled:          false,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want 1", len(scheduler.requests))
	}
	if len(scheduler.requests[0].Candidates) != 2 {
		t.Fatalf("Candidates count = %d, want 2", len(scheduler.requests[0].Candidates))
	}
	// Fallback should pick auth-high because it has the highest priority tier
	if got == nil || got.ID != "auth-high" {
		t.Fatalf("picked auth = %v, want auth-high", got)
	}
}

func TestManagerPluginSchedulerAcrossPrioritiesAttributesAndCooldownFilter(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}

	authHighActive := &Auth{ID: "high-act", Provider: "gemini", Attributes: map[string]string{"priority": "10", "weight": "100"}}
	authHighCooldown := &Auth{
		ID:         "high-cool",
		Provider:   "gemini",
		Attributes: map[string]string{"priority": "10", "weight": "80"},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "credential_quota",
			NextRecoverAt: time.Now().Add(time.Hour),
		},
	}
	authLowActive := &Auth{ID: "low-act", Provider: "gemini", Attributes: map[string]string{"priority": "5", "weight": "50"}}
	authLowDisabled := &Auth{ID: "low-dis", Provider: "gemini", Disabled: true, Attributes: map[string]string{"priority": "5", "weight": "20"}}

	for _, a := range []*Auth{authHighActive, authHighCooldown, authLowActive, authLowDisabled} {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatalf("Register(%s) error = %v", a.ID, err)
		}
	}

	scheduler := &fakePluginScheduler{
		acrossPriorities: true,
		resp:             pluginapi.SchedulerPickResponse{Handled: true, AuthID: "low-act"},
		handled:          true,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil || got.ID != "low-act" {
		t.Fatalf("picked auth = %v, want low-act", got)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want 1", len(scheduler.requests))
	}
	candidates := scheduler.requests[0].Candidates
	if len(candidates) != 2 {
		t.Fatalf("Candidates count = %d, want 2", len(candidates))
	}

	candidateMap := make(map[string]pluginapi.SchedulerAuthCandidate, len(candidates))
	for _, c := range candidates {
		candidateMap[c.ID] = c
	}
	cHigh, okHigh := candidateMap["high-act"]
	if !okHigh {
		t.Fatalf("missing high-act candidate")
	}
	if cHigh.Priority != 10 || cHigh.Attributes["weight"] != "100" {
		t.Fatalf("high-act priority/weight mismatch: Priority=%d, weight=%s", cHigh.Priority, cHigh.Attributes["weight"])
	}
	cLow, okLow := candidateMap["low-act"]
	if !okLow {
		t.Fatalf("missing low-act candidate")
	}
	if cLow.Priority != 5 || cLow.Attributes["weight"] != "50" {
		t.Fatalf("low-act priority/weight mismatch: Priority=%d, weight=%s", cLow.Priority, cLow.Attributes["weight"])
	}
}

func TestManagerPluginSchedulerAcrossPrioritiesMixedUnhandledFallsBackToHighestPriority(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}

	authHigh := &Auth{ID: "auth-high", Provider: "gemini", Attributes: map[string]string{"priority": "10"}}
	authLow := &Auth{ID: "auth-low", Provider: "claude", Attributes: map[string]string{"priority": "5"}}

	for _, a := range []*Auth{authHigh, authLow} {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatalf("Register(%s) error = %v", a.ID, err)
		}
	}

	scheduler := &fakePluginScheduler{
		acrossPriorities: true,
		handled:          false,
	}
	manager.SetPluginScheduler(scheduler)

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want 1", len(scheduler.requests))
	}
	if len(scheduler.requests[0].Candidates) != 2 {
		t.Fatalf("Candidates count = %d, want 2", len(scheduler.requests[0].Candidates))
	}
	if got == nil || got.ID != "auth-high" {
		t.Fatalf("picked auth = %v, want auth-high", got)
	}
	if provider != "gemini" {
		t.Fatalf("provider = %q, want gemini", provider)
	}
}

func TestManagerSelectAuthByKindSkipsAPIKey(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, candidate := range []*Auth{
		{ID: "codex-api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "test-key"}},
		{ID: "codex-oauth", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "codex-api-key"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", AuthKindOAuth, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuthByKind() error = %v", errSelect)
	}
	if selected == nil || selected.ID != "codex-oauth" {
		t.Fatalf("SelectAuthByKind() auth = %#v, want codex-oauth", selected)
	}
	if scheduler.calls != 1 {
		t.Fatalf("scheduler.calls = %d, want 1", scheduler.calls)
	}
	if len(scheduler.requests) != 1 || len(scheduler.requests[0].Candidates) != 1 || scheduler.requests[0].Candidates[0].ID != "codex-oauth" {
		t.Fatalf("scheduler candidates = %#v, want only codex-oauth", scheduler.requests)
	}
}

func TestManagerCodexAlphaSearchPolicyFiltersBeforePluginScheduler(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, candidate := range []*Auth{
		{ID: "ordinary-api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "ordinary"}},
		{ID: "alpha-api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "alpha", AttributeCodexAlphaSearch: "true", "base_url": "https://codex.example.com"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "alpha-api-key"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	selected, errSelect := manager.SelectAuthWithCredentialPolicy(context.Background(), "codex", "", CredentialPolicyCodexAlphaSearchV1, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuthWithCredentialPolicy() error = %v", errSelect)
	}
	if selected == nil || selected.ID != "alpha-api-key" {
		t.Fatalf("SelectAuthWithCredentialPolicy() auth = %#v, want alpha-api-key", selected)
	}
	if len(scheduler.requests) != 1 || len(scheduler.requests[0].Candidates) != 1 || scheduler.requests[0].Candidates[0].ID != "alpha-api-key" {
		t.Fatalf("scheduler candidates = %#v, want only alpha-api-key", scheduler.requests)
	}
}

func TestManagerCodexAlphaSearchPolicyRejectsOrdinaryAPIKey(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:         "ordinary-api-key",
		Provider:   "codex",
		Attributes: map[string]string{AttributeAPIKey: "ordinary"},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	selected, errSelect := manager.SelectAuthWithCredentialPolicy(context.Background(), "codex", "", CredentialPolicyCodexAlphaSearchV1, cliproxyexecutor.Options{})
	if selected != nil {
		t.Fatalf("SelectAuthWithCredentialPolicy() auth = %#v, want nil", selected)
	}
	var authErr *Error
	if !errors.As(errSelect, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("SelectAuthWithCredentialPolicy() error = %#v, want auth_not_found", errSelect)
	}
}

func TestManagerSelectAuthByKindWeightedRoundRobinIgnoresIneligibleAPIKeyWeight(t *testing.T) {
	manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, candidate := range []*Auth{
		{ID: "api-high", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "test-key", AttributeWeight: "100"}},
		{ID: "oauth-heavy", Provider: "codex", Attributes: map[string]string{AttributeWeight: "5"}, Metadata: map[string]any{"access_token": "heavy-token"}},
		{ID: "oauth-light", Provider: "codex", Attributes: map[string]string{AttributeWeight: "1"}, Metadata: map[string]any{"access_token": "light-token"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	counts := make(map[string]int)
	for index := 0; index < 600; index++ {
		selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", AuthKindOAuth, cliproxyexecutor.Options{})
		if errSelect != nil {
			t.Fatalf("SelectAuthByKind() #%d error = %v", index, errSelect)
		}
		counts[selected.ID]++
	}
	if counts["oauth-heavy"] != 500 || counts["oauth-light"] != 100 || counts["api-high"] != 0 {
		t.Fatalf("weighted OAuth picks = %#v, want oauth-heavy:oauth-light=500:100 and no API key", counts)
	}
}

func TestManagerWeightedRoundRobinDisallowFreeAuthIgnoresFreeWeight(t *testing.T) {
	tests := []struct {
		name  string
		mixed bool
	}{
		{name: "single provider"},
		{name: "mixed providers", mixed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
			manager.executors["codex"] = schedulerTestExecutor{}
			lightProvider := "codex"
			if tt.mixed {
				lightProvider = "gemini"
				manager.executors["gemini"] = schedulerTestExecutor{provider: "gemini"}
			}
			for _, candidate := range []*Auth{
				{ID: "free-high", Provider: "codex", Attributes: map[string]string{"plan_type": "free", AttributeWeight: "100"}, Metadata: map[string]any{"access_token": "free-token"}},
				{ID: "paid-heavy", Provider: "codex", Attributes: map[string]string{"plan_type": "plus", AttributeWeight: "5"}, Metadata: map[string]any{"access_token": "heavy-token"}},
				{ID: "paid-light", Provider: lightProvider, Attributes: map[string]string{"plan_type": "plus", AttributeWeight: "1"}, Metadata: map[string]any{"access_token": "light-token"}},
			} {
				if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
					t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
				}
			}

			opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true}}
			counts := make(map[string]int)
			for index := 0; index < 600; index++ {
				var selected *Auth
				var errPick error
				if tt.mixed {
					selected, _, _, errPick = manager.pickNextMixed(context.Background(), []string{"codex", "gemini"}, "", opts, nil)
				} else {
					selected, _, errPick = manager.pickNext(context.Background(), "codex", "", opts, nil)
				}
				if errPick != nil {
					t.Fatalf("weighted pick #%d error = %v", index, errPick)
				}
				counts[selected.ID]++
			}
			if counts["paid-heavy"] != 500 || counts["paid-light"] != 100 || counts["free-high"] != 0 {
				t.Fatalf("weighted non-free picks = %#v, want paid-heavy:paid-light=500:100 and no free auth", counts)
			}
		})
	}
}

func TestManagerSelectAuthByKindRoundRobinKeepsEligibleRotation(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, candidate := range []*Auth{
		{ID: "api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "test-key"}},
		{ID: "oauth-a", Provider: "codex", Metadata: map[string]any{"access_token": "token-a"}},
		{ID: "oauth-b", Provider: "codex", Metadata: map[string]any{"access_token": "token-b"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	counts := make(map[string]int)
	for index := 0; index < 6; index++ {
		selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", AuthKindOAuth, cliproxyexecutor.Options{})
		if errSelect != nil {
			t.Fatalf("SelectAuthByKind() #%d error = %v", index, errSelect)
		}
		counts[selected.ID]++
	}
	if counts["oauth-a"] != 3 || counts["oauth-b"] != 3 || counts["api-key"] != 0 {
		t.Fatalf("round-robin OAuth picks = %#v, want three picks per OAuth auth and no API key", counts)
	}
}

func TestManagerSelectAuthByKindReturnsErrorWhenUnavailable(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:         "codex-api-key",
		Provider:   "codex",
		Attributes: map[string]string{AttributeAPIKey: "test-key"},
	}); errRegister != nil {
		t.Fatalf("Register(codex-api-key) error = %v", errRegister)
	}

	selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", AuthKindOAuth, cliproxyexecutor.Options{})
	if selected != nil {
		t.Fatalf("SelectAuthByKind() auth = %#v, want nil", selected)
	}
	var authErr *Error
	if !errors.As(errSelect, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("SelectAuthByKind() error = %#v, want auth_not_found", errSelect)
	}
}

func TestManagerSelectAuthByKindRejectsInvalidKind(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	selected, errSelect := manager.SelectAuthByKind(context.Background(), "codex", "", "certificate", cliproxyexecutor.Options{})
	if selected != nil {
		t.Fatalf("SelectAuthByKind() auth = %#v, want nil", selected)
	}
	var authErr *Error
	if !errors.As(errSelect, &authErr) || authErr.Code != "invalid_auth_kind" || authErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("SelectAuthByKind() error = %#v, want invalid_auth_kind", errSelect)
	}
}

func TestManagerLegacySelectAuthFailsClosedWhenHomeEnabled(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{
		ID:       "home-oauth",
		Provider: "test",
		Metadata: map[string]any{"access_token": "test-token"},
	}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = oldCurrentHomeDispatcher })

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{})

	for name, selectAuth := range map[string]func() (*Auth, error){
		"SelectAuth": func() (*Auth, error) {
			return manager.SelectAuth(context.Background(), "test", "model", cliproxyexecutor.Options{})
		},
		"SelectAuthByKind": func() (*Auth, error) {
			return manager.SelectAuthByKind(context.Background(), "test", "model", AuthKindOAuth, cliproxyexecutor.Options{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			selected, errSelect := selectAuth()
			if selected != nil {
				t.Fatalf("%s() auth = %#v, want nil", name, selected)
			}
			var authErr *Error
			if !errors.As(errSelect, &authErr) || authErr.Code != "home_unavailable" || authErr.HTTPStatus != http.StatusServiceUnavailable {
				t.Fatalf("%s() error = %#v, want home_unavailable", name, errSelect)
			}
		})
	}
	if len(dispatcher.counts) != 0 {
		t.Fatalf("legacy selection issued Home RPOP calls: %v", dispatcher.counts)
	}
}

func TestSelectHomeAuthByKindReturnsHomeSelection(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{
		ID:       "home-oauth",
		Provider: "test",
		Metadata: map[string]any{"access_token": "test-token"},
	}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{})

	selection, errSelect := manager.SelectHomeAuthByKind(context.Background(), "test", "gpt-5.4", AuthKindOAuth, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthByKind() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil || selection.Auth.ID != "home-oauth" {
		t.Fatalf("SelectHomeAuthByKind() = %#v, want home-oauth", selection)
	}
	if selection.Executor == nil || selection.Provider != "test" {
		t.Fatalf("selection executor/provider = %#v/%q, want test", selection.Executor, selection.Provider)
	}
	selection.End("test_complete")
}

func TestSelectHomeAuthByKindSkipsProviderMismatch(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{
		{ID: "wrong-provider", Provider: "other", Metadata: map[string]any{"access_token": "test-token"}},
		{ID: "matching-provider", Provider: "test", Metadata: map[string]any{"access_token": "test-token"}},
	}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{})
	manager.RegisterExecutor(schedulerTestExecutor{provider: "other"})

	selection, errSelect := manager.SelectHomeAuthByKind(context.Background(), "test", "gpt-5.4", AuthKindOAuth, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthByKind() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil || selection.Auth.ID != "matching-provider" {
		t.Fatalf("SelectHomeAuthByKind() = %#v, want matching provider auth", selection)
	}
	if got := dispatcher.counts; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("home auth counts = %v, want [1 2]", got)
	}
	selection.End("test_complete")
}

func TestSelectHomeAuthWithCredentialPolicyTransportsAndValidatesPolicy(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{
		{ID: "ordinary-api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "ordinary", "base_url": "https://ordinary.example.com"}},
		{ID: "alpha-api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "alpha", AttributeCodexAlphaSearch: "true", "base_url": "https://alpha.example.com"}},
	}}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	registry := executionregistry.New()
	manager.PublishHomeDispatch(dispatcher, registry, 1)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})

	selection, errSelect := manager.SelectHomeAuthWithCredentialPolicy(context.Background(), "codex", "gpt-5.4", CredentialPolicyCodexAlphaSearchV1, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthWithCredentialPolicy() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil || selection.Auth.ID != "alpha-api-key" {
		t.Fatalf("SelectHomeAuthWithCredentialPolicy() = %#v, want alpha-api-key", selection)
	}
	if got := dispatcher.counts; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Home auth counts = %v, want [1 2]", got)
	}
	if got := dispatcher.policies; len(got) != 2 || got[0] != CredentialPolicyCodexAlphaSearchV1 || got[1] != CredentialPolicyCodexAlphaSearchV1 {
		t.Fatalf("Home credential policies = %v", got)
	}
	selection.End("test_complete")
	if errDrain := registry.Drain(context.Background()); errDrain != nil {
		t.Fatalf("Drain() error = %v", errDrain)
	}
}

func TestSelectHomeAuthByKindKeepsLogicalProviderWhenUsingCompatibilityExecutor(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{
		ID:       "compat-auth",
		Provider: "base-url-provider",
		Attributes: map[string]string{
			"base_url":      "https://compat.example.com",
			AttributeAPIKey: "test-key",
		},
	}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetHomeExecutionRegistry(executionregistry.New())
	manager.RegisterExecutor(schedulerTestExecutor{provider: "openai-compatibility"})

	selection, errSelect := manager.SelectHomeAuthByKind(context.Background(), "base-url-provider", "gpt-5.4", AuthKindAPIKey, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthByKind() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil || selection.Auth.ID != "compat-auth" {
		t.Fatalf("SelectHomeAuthByKind() = %#v, want compat-auth", selection)
	}
	if selection.Provider != "base-url-provider" {
		t.Fatalf("selection.Provider = %q, want logical provider base-url-provider", selection.Provider)
	}
	if selection.Executor == nil || selection.Executor.Identifier() != "openai-compatibility" {
		t.Fatalf("selection.Executor = %#v, want openai-compatibility", selection.Executor)
	}
	selection.End("test_complete")
}

func TestPickNextViaHomeEndsPendingOnInvalidAuth(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{Provider: "test"}}}
	oldCurrentHomeDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher {
		return dispatcher
	}
	t.Cleanup(func() {
		currentHomeDispatcher = oldCurrentHomeDispatcher
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	registry := executionregistry.New()
	manager.SetHomeExecutionRegistry(registry)
	manager.RegisterExecutor(schedulerTestExecutor{})

	_, _, _, errPick := manager.pickNextViaHome(context.Background(), "gpt-5.4", cliproxyexecutor.Options{}, nil)
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr.Code != "invalid_auth" {
		t.Fatalf("pickNextViaHome() error = %v, want invalid_auth", errPick)
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if errDrain := registry.Drain(drainCtx); errDrain != nil {
		t.Fatalf("Drain() error = %v, pending dispatch was not ended", errDrain)
	}
}

func TestManagerPluginSchedulerSkippedWhenHomeEnabled(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	_, _, _ = manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)

	if scheduler.calls != 0 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 0)
	}
}

func TestManagerInactivePluginSchedulerKeepsFastPath(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	scheduler := &inactivePluginScheduler{}
	manager.SetPluginScheduler(scheduler)

	gotA, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() first error = %v", errPick)
	}
	gotB, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() second error = %v", errPick)
	}
	if gotA == nil || gotB == nil {
		t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotA, gotB)
	}
	if gotA.ID != "auth-a" || gotB.ID != "auth-b" {
		t.Fatalf("fast path picks = %q, %q; want auth-a, auth-b", gotA.ID, gotB.ID)
	}
	if scheduler.calls != 0 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 0)
	}
}

func TestManagerPluginSchedulerCalledOutsideManagerLock(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	var currentTest *testing.T
	acrossCalls := 0
	scheduler := &fakePluginScheduler{
		handled: true,
		acrossPrioritiesFunc: func() bool {
			acrossCalls++
			if !manager.mu.TryLock() {
				currentTest.Fatalf("SchedulerWantsAcrossPriorities called while manager lock is held")
			}
			manager.mu.Unlock()
			return true
		},
		pick: func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
			if !manager.mu.TryLock() {
				currentTest.Fatalf("plugin scheduler called while manager lock is held")
			}
			manager.mu.Unlock()
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"}, true, nil
		},
	}
	manager.SetPluginScheduler(scheduler)

	t.Run("PluginSchedulerWantsAcrossPriorities", func(t *testing.T) {
		currentTest = t
		if !manager.PluginSchedulerWantsAcrossPriorities() {
			t.Fatalf("PluginSchedulerWantsAcrossPriorities() = false, want true")
		}
	})

	t.Run("pickNext", func(t *testing.T) {
		currentTest = t
		got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() error = %v", errPick)
		}
		if got == nil {
			t.Fatalf("pickNext() auth = nil")
		}
		if got.ID != "auth-a" {
			t.Fatalf("pickNext() auth.ID = %q, want auth-a", got.ID)
		}
	})

	t.Run("pickNextMixed", func(t *testing.T) {
		currentTest = t
		gotMixed, _, _, errPickMixed := manager.pickNextMixed(context.Background(), []string{"gemini"}, "", cliproxyexecutor.Options{}, nil)
		if errPickMixed != nil {
			t.Fatalf("pickNextMixed() error = %v", errPickMixed)
		}
		if gotMixed == nil {
			t.Fatalf("pickNextMixed() auth = nil")
		}
		if gotMixed.ID != "auth-a" {
			t.Fatalf("pickNextMixed() auth.ID = %q, want auth-a", gotMixed.ID)
		}
	})

	if scheduler.calls != 2 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 2)
	}
	if acrossCalls != 3 {
		t.Fatalf("acrossCalls = %d, want %d", acrossCalls, 3)
	}
}

func TestManagerPluginSchedulerSnapshotConsistencyAcrossSwap(t *testing.T) {
	for _, mode := range []string{"single", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.executors["gemini"] = schedulerTestExecutor{}
			if _, errRegister := manager.Register(context.Background(), &Auth{
				ID:         "auth-high",
				Provider:   "gemini",
				Attributes: map[string]string{"priority": "10"},
			}); errRegister != nil {
				t.Fatalf("Register(auth-high) error = %v", errRegister)
			}
			if _, errRegister := manager.Register(context.Background(), &Auth{
				ID:         "auth-low",
				Provider:   "gemini",
				Attributes: map[string]string{"priority": "1"},
			}); errRegister != nil {
				t.Fatalf("Register(auth-low) error = %v", errRegister)
			}

			schedulerB := &fakePluginScheduler{
				acrossPriorities: false,
				handled:          true,
				pick: func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
					for _, candidate := range req.Candidates {
						if candidate.ID == "auth-low" {
							t.Fatalf("schedulerB (acrossPriorities=false) received low-priority candidate from schedulerA capability")
						}
					}
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-high"}, true, nil
				},
			}
			schedulerA := &fakePluginScheduler{
				handled: true,
				acrossPrioritiesFunc: func() bool {
					manager.SetPluginScheduler(schedulerB)
					return true
				},
				pick: func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
					if len(req.Candidates) != 2 {
						t.Fatalf("schedulerA candidates = %d, want 2 across priorities", len(req.Candidates))
					}
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-low"}, true, nil
				},
			}
			manager.SetPluginScheduler(schedulerA)

			if mode == "single" {
				got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
				if errPick != nil {
					t.Fatalf("pickNext() error = %v", errPick)
				}
				if got == nil || got.ID != "auth-low" {
					t.Fatalf("pickNext() auth = %#v, want auth-low", got)
				}
			} else {
				got, _, _, errPick := manager.pickNextMixed(context.Background(), []string{"gemini"}, "", cliproxyexecutor.Options{}, nil)
				if errPick != nil {
					t.Fatalf("pickNextMixed() error = %v", errPick)
				}
				if got == nil || got.ID != "auth-low" {
					t.Fatalf("pickNextMixed() auth = %#v, want auth-low", got)
				}
			}
		})
	}
}

func TestManagerPluginSchedulerErrorStopsPick(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		handled: true,
		err:     errors.New("tenant denied"),
	}
	manager.SetPluginScheduler(scheduler)

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick == nil {
		t.Fatalf("pickNext() error = nil, want tenant denied")
	}
	if errPick.Error() != "tenant denied" {
		t.Fatalf("pickNext() error = %v, want tenant denied", errPick)
	}
	if got != nil {
		t.Fatalf("pickNext() auth = %v, want nil", got)
	}
}

func TestManagerPluginSchedulerFallsBackWhenUnhandledOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    pluginapi.SchedulerPickResponse
		handled bool
	}{
		{
			name:    "unhandled",
			resp:    pluginapi.SchedulerPickResponse{Handled: false},
			handled: false,
		},
		{
			name:    "unknown auth id",
			resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "missing"},
			handled: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.executors["gemini"] = schedulerTestExecutor{}
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
				t.Fatalf("Register(auth-b) error = %v", errRegister)
			}
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
				t.Fatalf("Register(auth-a) error = %v", errRegister)
			}

			scheduler := &fakePluginScheduler{resp: tc.resp, handled: tc.handled}
			manager.SetPluginScheduler(scheduler)

			got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickNext() error = %v", errPick)
			}
			if got == nil {
				t.Fatalf("pickNext() auth = nil")
			}
			if got.ID != "auth-a" {
				t.Fatalf("pickNext() auth.ID = %q, want %q", got.ID, "auth-a")
			}
		})
	}
}

func TestManagerPluginSchedulerDelegatesBuiltin(t *testing.T) {
	t.Run("round-robin", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
			handled: true,
		})

		gotA, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() first error = %v", errPick)
		}
		gotB, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() second error = %v", errPick)
		}
		if gotA == nil || gotB == nil {
			t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotA, gotB)
		}
		if gotA.ID != "auth-a" || gotB.ID != "auth-b" {
			t.Fatalf("round-robin picks = %q, %q; want auth-a, auth-b", gotA.ID, gotB.ID)
		}
	})

	t.Run("round-robin model cursors", func(t *testing.T) {
		reg := registry.GetGlobalRegistry()
		models := []*registry.ModelInfo{{ID: "model-a"}, {ID: "model-b"}}
		for _, authID := range []string{"auth-a", "auth-b"} {
			reg.RegisterClient(authID, "gemini", models)
			t.Cleanup(func() {
				reg.UnregisterClient(authID)
			})
		}

		manager := NewManager(nil, &FillFirstSelector{}, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
			handled: true,
		})

		gotModelA, _, errPick := manager.pickNext(context.Background(), "gemini", "model-a", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(model-a) error = %v", errPick)
		}
		gotModelB, _, errPick := manager.pickNext(context.Background(), "gemini", "model-b", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(model-b) error = %v", errPick)
		}
		if gotModelA == nil || gotModelB == nil {
			t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotModelA, gotModelB)
		}
		if gotModelA.ID != "auth-a" || gotModelB.ID != "auth-a" {
			t.Fatalf("model-scoped round-robin picks = %q, %q; want auth-a, auth-a", gotModelA.ID, gotModelB.ID)
		}
	})

	t.Run("fill-first", func(t *testing.T) {
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst},
			handled: true,
		})

		got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() error = %v", errPick)
		}
		if got == nil {
			t.Fatalf("pickNext() auth = nil")
		}
		if got.ID != "auth-a" {
			t.Fatalf("fill-first pick = %q, want auth-a", got.ID)
		}
	})
}

func TestManagerPluginSchedulerDelegateRoundRobinUsesNativeMixedRotation(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}
	manager.SetPluginScheduler(&fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
		handled: true,
	})

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManagerPluginSchedulerPickNextMixedSelectsProvider(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}
	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "claude-a"},
		handled: true,
	}
	manager.SetPluginScheduler(scheduler)

	got, executor, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want claude-a", got.ID)
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want claude", provider)
	}
	if executor == nil {
		t.Fatalf("pickNextMixed() executor = nil")
	}
	if len(scheduler.requests) != 1 {
		t.Fatalf("len(scheduler.requests) = %d, want %d", len(scheduler.requests), 1)
	}
	req := scheduler.requests[0]
	if req.Provider != "" {
		t.Fatalf("scheduler request Provider = %q, want empty for mixed provider pick", req.Provider)
	}
	if len(req.Providers) != 2 || req.Providers[0] != "gemini" || req.Providers[1] != "claude" {
		t.Fatalf("scheduler request Providers = %#v, want [gemini claude]", req.Providers)
	}
}

func TestManagerInactivePluginSchedulerKeepsMixedFastPath(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	scheduler := &inactivePluginScheduler{}
	manager.SetPluginScheduler(scheduler)

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "gemini" {
		t.Fatalf("pickNextMixed() provider = %q, want gemini", provider)
	}
	if got.ID != "gemini-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want gemini-a", got.ID)
	}
	if scheduler.calls != 0 {
		t.Fatalf("scheduler.calls = %d, want %d", scheduler.calls, 0)
	}
}

func TestManagerPluginSchedulerCandidatesAreSafeCopies(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	auth := &Auth{
		ID:       "auth-a",
		Provider: "gemini",
		Status:   StatusActive,
		Attributes: map[string]string{
			"access_token": "token-value",
			"api_key":      "api-key-value",
			"cookie":       "cookie-value",
			"priority":     "7",
			"team":         "alpha",
		},
		Metadata: map[string]any{"tenant": "one"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	scheduler := &fakePluginScheduler{
		handled: true,
		pick: func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
			if len(req.Candidates) != 1 {
				t.Fatalf("len(req.Candidates) = %d, want %d", len(req.Candidates), 1)
			}
			candidate := req.Candidates[0]
			if candidate.ID != "auth-a" || candidate.Provider != "gemini" || candidate.Priority != 7 || candidate.Status != string(StatusActive) {
				t.Fatalf("scheduler candidate = %#v, want sanitized auth-a metadata", candidate)
			}
			for _, key := range []string{"access_token", "api_key", "cookie"} {
				if _, ok := candidate.Attributes[key]; ok {
					t.Fatalf("scheduler candidate Attributes contains sensitive key %q", key)
				}
			}
			if candidate.Attributes["priority"] != "7" {
				t.Fatalf("scheduler candidate priority attribute = %q, want 7", candidate.Attributes["priority"])
			}
			if len(candidate.Metadata) != 0 {
				t.Fatalf("scheduler candidate Metadata = %#v, want empty", candidate.Metadata)
			}
			candidate.Attributes["team"] = "mutated"
			req.Candidates[0] = candidate
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"}, true, nil
		},
	}
	manager.SetPluginScheduler(scheduler)

	if _, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil); errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}

	manager.mu.RLock()
	gotAttr := manager.auths["auth-a"].Attributes["team"]
	gotAPIKey := manager.auths["auth-a"].Attributes["api_key"]
	manager.mu.RUnlock()
	if gotAttr != "alpha" {
		t.Fatalf("manager auth attribute team = %q, want alpha", gotAttr)
	}
	if gotAPIKey != "api-key-value" {
		t.Fatalf("manager auth attribute api_key = %q, want api-key-value", gotAPIKey)
	}
}

func TestManagerCustomSelector_FallsBackToLegacyPath(t *testing.T) {
	t.Parallel()

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.auths["auth-a"] = &Auth{ID: "auth-a", Provider: "gemini"}
	manager.auths["auth-b"] = &Auth{ID: "auth-b", Provider: "gemini"}

	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if selector.calls != 1 {
		t.Fatalf("selector.calls = %d, want %d", selector.calls, 1)
	}
	if len(selector.lastAuthID) != 2 {
		t.Fatalf("len(selector.lastAuthID) = %d, want %d", len(selector.lastAuthID), 2)
	}
	if got.ID != selector.lastAuthID[len(selector.lastAuthID)-1] {
		t.Fatalf("pickNext() auth.ID = %q, want selector-picked %q", got.ID, selector.lastAuthID[len(selector.lastAuthID)-1])
	}
}

func TestManager_InitializesSchedulerForBuiltInSelector(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if manager.scheduler == nil {
		t.Fatalf("manager.scheduler = nil")
	}
	if manager.scheduler.strategy != schedulerStrategyRoundRobin {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyRoundRobin)
	}

	manager.SetSelector(&FillFirstSelector{})
	if manager.scheduler.strategy != schedulerStrategyFillFirst {
		t.Fatalf("manager.scheduler.strategy = %v, want %v", manager.scheduler.strategy, schedulerStrategyFillFirst)
	}
}

func TestManager_SchedulerTracksRegisterAndUpdate(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("scheduler.pickSingle() auth = %v, want auth-a", got)
	}

	if _, errUpdate := manager.Update(context.Background(), &Auth{ID: "auth-a", Provider: "gemini", Disabled: true}); errUpdate != nil {
		t.Fatalf("Update(auth-a) error = %v", errUpdate)
	}

	got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after update error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after update auth = %v, want auth-b", got)
	}
}

func TestManager_PickNextMixed_UsesSchedulerRotation(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestManager_SchedulerSharesThinkingSuffixCooldownAndRegistryState(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	reg := registry.GetGlobalRegistry()
	baseModel := "scheduler-thinking-model"
	reg.RegisterClient("thinking-auth-a", "gemini", []*registry.ModelInfo{{ID: baseModel}})
	reg.RegisterClient("thinking-auth-b", "gemini", []*registry.ModelInfo{{ID: baseModel}})
	t.Cleanup(func() {
		reg.UnregisterClient("thinking-auth-a")
		reg.UnregisterClient("thinking-auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "thinking-auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(thinking-auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "thinking-auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(thinking-auth-b) error = %v", errRegister)
	}

	retryAfter := time.Hour
	manager.MarkResult(context.Background(), Result{
		AuthID:     "thinking-auth-a",
		Provider:   "gemini",
		Model:      baseModel + "(high)",
		Success:    false,
		Error:      &Error{HTTPStatus: 429, Message: "quota"},
		RetryAfter: &retryAfter,
	})

	auth, ok := manager.GetByID("thinking-auth-a")
	if !ok || auth == nil {
		t.Fatal("thinking-auth-a was not found")
	}
	if len(auth.ModelStates) != 1 || auth.ModelStates[baseModel] == nil {
		t.Fatalf("ModelStates = %+v, want only canonical key %q", auth.ModelStates, baseModel)
	}
	// Only thinking-auth-a is cooling down; thinking-auth-b remains available.
	if count := reg.GetModelCount(baseModel); count != 1 {
		t.Fatalf("registry model count during cooldown = %d, want 1", count)
	}
	for _, model := range []string{baseModel, baseModel + "(medium)", baseModel + "(low)"} {
		got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("scheduler.pickSingle(%q) error = %v", model, errPick)
		}
		if got == nil || got.ID != "thinking-auth-b" {
			t.Fatalf("scheduler.pickSingle(%q) auth = %v, want thinking-auth-b", model, got)
		}
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "thinking-auth-a",
		Provider: "gemini",
		Model:    baseModel + "(low)",
		Success:  true,
	})

	auth, ok = manager.GetByID("thinking-auth-a")
	if !ok || auth == nil || auth.ModelStates[baseModel] == nil {
		t.Fatal("canonical model state was not retained after success")
	}
	state := auth.ModelStates[baseModel]
	if state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() {
		t.Fatalf("canonical model state after success = %+v, want cleared", state)
	}
	if count := reg.GetModelCount(baseModel); count != 2 {
		t.Fatalf("registry model count after recovery = %d, want 2", count)
	}
}

func TestManager_PickNextMixed_SkipsProvidersWithoutExecutors(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatalf("pickNextMixed() auth = nil")
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "claude")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "claude-a")
	}
}

func TestManager_SchedulerTracksMarkResultCooldownAndRecovery(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	reg.RegisterClient("auth-b", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient("auth-a")
		reg.UnregisterClient("auth-b")
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-b) error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})

	got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler.pickSingle() after cooldown error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("scheduler.pickSingle() after cooldown auth = %v, want auth-b", got)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-a",
		Provider: "gemini",
		Model:    "test-model",
		Success:  true,
	})

	seen := make(map[string]struct{}, 2)
	for index := 0; index < 2; index++ {
		got, errPick = manager.scheduler.pickSingle(context.Background(), "gemini", "test-model", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("scheduler.pickSingle() after recovery #%d auth = nil", index)
		}
		seen[got.ID] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("len(seen) = %d, want %d", len(seen), 2)
	}
}

func TestManagerPluginSchedulerTerminalRejection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		resp        pluginapi.SchedulerPickResponse
		wantCode    string
		wantMessage string
	}{
		{
			name: "explicit code and message",
			resp: pluginapi.SchedulerPickResponse{
				Handled:      true,
				Reject:       true,
				RejectCode:   "quota_reserve_exhausted",
				RejectReason: "all candidates violate quota reserve policy",
			},
			wantCode:    "quota_reserve_exhausted",
			wantMessage: "all candidates violate quota reserve policy",
		},
		{
			name: "default code and message",
			resp: pluginapi.SchedulerPickResponse{
				Handled: true,
				Reject:  true,
			},
			wantCode:    "auth_unavailable",
			wantMessage: "scheduler rejected candidate selection",
		},
		{
			name: "whitespace code and message fallback to defaults",
			resp: pluginapi.SchedulerPickResponse{
				Handled:      true,
				Reject:       true,
				RejectCode:   "   ",
				RejectReason: " \t ",
			},
			wantCode:    "auth_unavailable",
			wantMessage: "scheduler rejected candidate selection",
		},
		{
			name: "reject with auth id takes precedence",
			resp: pluginapi.SchedulerPickResponse{
				Handled:      true,
				Reject:       true,
				AuthID:       "auth-a",
				RejectCode:   "quota_reserve_exhausted",
				RejectReason: "rejected despite auth present",
			},
			wantCode:    "quota_reserve_exhausted",
			wantMessage: "rejected despite auth present",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.executors["gemini"] = schedulerTestExecutor{}
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
				t.Fatalf("Register(auth-a) error = %v", errRegister)
			}
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
				t.Fatalf("Register(auth-b) error = %v", errRegister)
			}

			scheduler := &fakePluginScheduler{resp: tc.resp, handled: true}
			manager.SetPluginScheduler(scheduler)

			got, errSelect := manager.SelectAuth(context.Background(), "gemini", "", cliproxyexecutor.Options{})
			if got != nil {
				t.Fatalf("SelectAuth() got auth %v, want nil", got.ID)
			}
			if errSelect == nil {
				t.Fatal("SelectAuth() error = nil, want error")
			}
			var authErr *Error
			if !errors.As(errSelect, &authErr) {
				t.Fatalf("SelectAuth() error type = %T, want *auth.Error", errSelect)
			}
			if authErr.Code != tc.wantCode {
				t.Fatalf("SelectAuth() error code = %q, want %q", authErr.Code, tc.wantCode)
			}
			if authErr.Message != tc.wantMessage {
				t.Fatalf("SelectAuth() error message = %q, want %q", authErr.Message, tc.wantMessage)
			}

			gotMixed, _, _, errMixed := manager.pickNextMixed(context.Background(), []string{"gemini"}, "", cliproxyexecutor.Options{}, nil)
			if gotMixed != nil {
				t.Fatalf("pickNextMixed() got auth %v, want nil", gotMixed.ID)
			}
			if errMixed == nil {
				t.Fatal("pickNextMixed() error = nil, want error")
			}
			var mixedAuthErr *Error
			if !errors.As(errMixed, &mixedAuthErr) {
				t.Fatalf("pickNextMixed() error type = %T, want *auth.Error", errMixed)
			}
			if mixedAuthErr.Code != tc.wantCode {
				t.Fatalf("pickNextMixed() error code = %q, want %q", mixedAuthErr.Code, tc.wantCode)
			}
			if mixedAuthErr.Message != tc.wantMessage {
				t.Fatalf("pickNextMixed() error message = %q, want %q", mixedAuthErr.Message, tc.wantMessage)
			}
		})
	}
}

func TestManagerPluginSchedulerUnhandledWithRejectFlagFallsBack(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(auth-a) error = %v", errRegister)
	}

	// When Handled is false, even if Reject is set, it must remain unhandled and fall back to built-in selection.
	scheduler := &fakePluginScheduler{
		resp:    pluginapi.SchedulerPickResponse{Handled: false, Reject: true},
		handled: false,
	}
	manager.SetPluginScheduler(scheduler)

	got, errSelect := manager.SelectAuth(context.Background(), "gemini", "", cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuth() error = %v, want nil", errSelect)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("SelectAuth() got = %v, want auth-a", got)
	}
}

func TestManagerPluginSchedulerSelectionID(t *testing.T) {
	t.Run("single_credential_outer_retry_after_429_yields_distinct_ids_matching_context", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		manager.SetRetryConfig(1, 0, 0)
		SetQuotaCooldownDisabled(true)
		t.Cleanup(func() { SetQuotaCooldownDisabled(false) })

		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		t.Cleanup(func() {
			reg.UnregisterClient("auth-a")
		})
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}

		var execSelIDs []string
		manager.executors["gemini"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "gemini"},
			execute: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				execSelIDs = append(execSelIDs, coreusage.SelectionIDFromContext(ctx))
				if len(execSelIDs) == 1 {
					return cliproxyexecutor.Response{}, &Error{Code: "rate_limited", Message: "429", HTTPStatus: http.StatusTooManyRequests, Retryable: true}
				}
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			},
		}

		scheduler := &fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"},
			handled: true,
		}
		manager.SetPluginScheduler(scheduler)

		_, errExec := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.SelectionIDMetadataKey: "stale-caller-id"},
		})
		if errExec != nil {
			t.Fatalf("Execute() error = %v, want nil", errExec)
		}
		if len(scheduler.requests) != 2 {
			t.Fatalf("len(scheduler.requests) = %d, want 2", len(scheduler.requests))
		}
		selID1, _ := scheduler.requests[0].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		selID2, _ := scheduler.requests[1].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		if selID1 == "" || selID2 == "" {
			t.Fatalf("selection_id values = (%q, %q), want non-empty IDs on both Picks", selID1, selID2)
		}
		if selID1 == "stale-caller-id" || selID2 == "stale-caller-id" {
			t.Fatalf("selection_id did not overwrite caller value: (%q, %q)", selID1, selID2)
		}
		if selID1 == selID2 {
			t.Fatalf("selection_id across outer retry = %q, want distinct IDs", selID1)
		}
		if len(execSelIDs) != 2 || execSelIDs[0] != selID1 || execSelIDs[1] != selID2 {
			t.Fatalf("executor context selection IDs = %v, want [%q, %q]", execSelIDs, selID1, selID2)
		}
	})

	t.Run("different_credentials_on_retry_get_different_ids", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		reg.RegisterClient("auth-b", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		t.Cleanup(func() {
			reg.UnregisterClient("auth-a")
			reg.UnregisterClient("auth-b")
		})
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}

		var execAuths []string
		var execSelIDs []string
		manager.executors["gemini"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "gemini"},
			execute: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				execAuths = append(execAuths, auth.ID)
				execSelIDs = append(execSelIDs, coreusage.SelectionIDFromContext(ctx))
				if auth.ID == "auth-a" {
					return cliproxyexecutor.Response{}, &Error{Code: "rate_limited", Message: "429", HTTPStatus: http.StatusTooManyRequests, Retryable: true}
				}
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			},
		}

		scheduler := &fakePluginScheduler{
			pick: func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: req.Candidates[0].ID}, true, nil
			},
		}
		manager.SetPluginScheduler(scheduler)

		_, errExec := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, cliproxyexecutor.Options{})
		if errExec != nil {
			t.Fatalf("Execute() error = %v, want nil", errExec)
		}
		if len(scheduler.requests) != 2 {
			t.Fatalf("len(scheduler.requests) = %d, want 2", len(scheduler.requests))
		}
		selID1, _ := scheduler.requests[0].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		selID2, _ := scheduler.requests[1].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		if selID1 == "" || selID2 == "" || selID1 == selID2 {
			t.Fatalf("selection_id values = (%q, %q), want two distinct non-empty IDs", selID1, selID2)
		}
		if len(execSelIDs) != 2 || execSelIDs[0] != selID1 || execSelIDs[1] != selID2 {
			t.Fatalf("execSelIDs = %v, want [%q, %q]", execSelIDs, selID1, selID2)
		}
	})

	t.Run("attempts_within_one_selection_keep_same_id_for_token_refresh_and_model_pool", func(t *testing.T) {
		// Part A: 401 token refresh retry within one selection keeps the same SelectionID.
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("auth-oauth", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		t.Cleanup(func() {
			reg.UnregisterClient("auth-oauth")
		})
		auth := &Auth{
			ID:       "auth-oauth",
			Provider: "gemini",
			Metadata: map[string]any{"access_token": "tok-1", "refresh_token": "ref-1", "type": "oauth"},
			Attributes: map[string]string{
				"auth_kind": "oauth",
			},
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(auth-oauth) error = %v", errRegister)
		}

		var refreshSelIDs []string
		manager.executors["gemini"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "gemini"},
			execute: func(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				refreshSelIDs = append(refreshSelIDs, coreusage.SelectionIDFromContext(ctx))
				if len(refreshSelIDs) == 1 {
					return cliproxyexecutor.Response{}, &Error{Code: "unauthorized", Message: "401", HTTPStatus: http.StatusUnauthorized}
				}
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			},
			refresh: func(ctx context.Context, a *Auth) (*Auth, error) {
				clone := a.Clone()
				clone.Metadata = map[string]any{"access_token": "tok-2", "refresh_token": "ref-1", "type": "oauth"}
				return clone, nil
			},
		}

		scheduler := &fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-oauth"},
			handled: true,
		}
		manager.SetPluginScheduler(scheduler)

		_, errExec := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, cliproxyexecutor.Options{})
		if errExec != nil {
			t.Fatalf("Execute(token refresh) error = %v, want nil", errExec)
		}
		if len(scheduler.requests) != 1 {
			t.Fatalf("len(scheduler.requests) = %d, want 1", len(scheduler.requests))
		}
		selID, _ := scheduler.requests[0].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		if selID == "" {
			t.Fatal("selection_id on Pick is empty")
		}
		if len(refreshSelIDs) != 2 || refreshSelIDs[0] != selID || refreshSelIDs[1] != selID {
			t.Fatalf("refreshSelIDs = %v, want [%q, %q]", refreshSelIDs, selID, selID)
		}

		// Part B: Model pool attempts within one selection keep the same SelectionID.
		poolManager := NewManager(nil, &FillFirstSelector{}, nil)
		poolManager.SetConfig(&internalconfig.Config{
			OpenAICompatibility: []internalconfig.OpenAICompatibility{
				{
					Name: "openai-compatibility",
					APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{
						{APIKey: "sk-pool"},
					},
					Models: []internalconfig.OpenAICompatibilityModel{
						{Name: "model-pool-1", Alias: "route-model"},
						{Name: "model-pool-2", Alias: "route-model"},
					},
				},
			},
		})
		reg.RegisterClient("auth-pool", "openai-compatibility", []*registry.ModelInfo{{ID: "route-model"}, {ID: "model-pool-1"}, {ID: "model-pool-2"}})
		t.Cleanup(func() {
			reg.UnregisterClient("auth-pool")
		})
		poolAuth := &Auth{
			ID:       "auth-pool",
			Provider: "openai-compatibility",
			Attributes: map[string]string{
				"auth_kind":    "apikey",
				"api_key":      "sk-pool",
				"compat_name":  "openai-compatibility",
				"provider_key": "openai-compatibility",
			},
		}
		if _, errRegister := poolManager.Register(context.Background(), poolAuth); errRegister != nil {
			t.Fatalf("Register(auth-pool) error = %v", errRegister)
		}

		var poolSelIDs []string
		var poolModels []string
		poolManager.executors["openai-compatibility"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "openai-compatibility"},
			execute: func(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				poolSelIDs = append(poolSelIDs, coreusage.SelectionIDFromContext(ctx))
				poolModels = append(poolModels, req.Model)
				if len(poolSelIDs) == 1 {
					return cliproxyexecutor.Response{}, &Error{Code: "upstream_error", Message: "500", HTTPStatus: http.StatusInternalServerError, Retryable: true}
				}
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			},
		}

		poolScheduler := &fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-pool"},
			handled: true,
		}
		poolManager.SetPluginScheduler(poolScheduler)

		_, errPoolExec := poolManager.Execute(context.Background(), []string{"openai-compatibility"}, cliproxyexecutor.Request{Model: "route-model"}, cliproxyexecutor.Options{})
		if errPoolExec != nil {
			t.Fatalf("Execute(model pool) error = %v, want nil", errPoolExec)
		}
		if len(poolScheduler.requests) != 1 {
			t.Fatalf("len(poolScheduler.requests) = %d, want 1", len(poolScheduler.requests))
		}
		poolPickID, _ := poolScheduler.requests[0].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		if poolPickID == "" {
			t.Fatal("poolPickID is empty")
		}
		if len(poolSelIDs) != 2 || poolSelIDs[0] != poolPickID || poolSelIDs[1] != poolPickID {
			t.Fatalf("poolSelIDs = %v (models=%v), want [%q, %q]", poolSelIDs, poolModels, poolPickID, poolPickID)
		}
	})

	t.Run("unhandled_scheduler_clears_selection_id_and_does_not_attribute_builtin_selection", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		reg.RegisterClient("auth-b", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		t.Cleanup(func() {
			reg.UnregisterClient("auth-a")
			reg.UnregisterClient("auth-b")
		})
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}

		var execSelIDs []string
		manager.executors["gemini"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "gemini"},
			execute: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				execSelIDs = append(execSelIDs, coreusage.SelectionIDFromContext(ctx))
				if _, leaked := opts.Metadata[cliproxyexecutor.SelectionIDMetadataKey]; leaked && len(execSelIDs) == 2 {
					t.Errorf("unhandled second selection leaked selection_id in opts.Metadata: %v", opts.Metadata[cliproxyexecutor.SelectionIDMetadataKey])
				}
				if auth.ID == "auth-a" {
					return cliproxyexecutor.Response{}, &Error{Code: "rate_limited", Message: "429", HTTPStatus: http.StatusTooManyRequests, Retryable: true}
				}
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			},
		}

		pickCall := 0
		scheduler := &fakePluginScheduler{
			pick: func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				pickCall++
				if pickCall == 1 {
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"}, true, nil
				}
				// Second pick is unhandled -> falls back to built-in FillFirstSelector for auth-b.
				return pluginapi.SchedulerPickResponse{Handled: false}, false, nil
			},
		}
		manager.SetPluginScheduler(scheduler)

		presetCtx := coreusage.WithSelectionID(context.Background(), "stale-ctx-selection-id")
		_, errExec := manager.Execute(presetCtx, []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.SelectionIDMetadataKey: "stale-meta-selection-id"},
		})
		if errExec != nil {
			t.Fatalf("Execute() error = %v, want nil", errExec)
		}
		if len(scheduler.requests) != 2 {
			t.Fatalf("len(scheduler.requests) = %d, want 2", len(scheduler.requests))
		}
		selID1, _ := scheduler.requests[0].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		if selID1 == "" {
			t.Fatal("first Pick selection_id is empty")
		}
		if len(execSelIDs) != 2 {
			t.Fatalf("len(execSelIDs) = %d, want 2", len(execSelIDs))
		}
		if execSelIDs[0] != selID1 {
			t.Fatalf("execSelIDs[0] = %q, want %q", execSelIDs[0], selID1)
		}
		if execSelIDs[1] != "" {
			t.Fatalf("execSelIDs[1] on unhandled fallback = %q, want empty", execSelIDs[1])
		}
	})

	t.Run("rejected_pick_causes_no_fallback_and_clears_selection_id", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		t.Cleanup(func() {
			reg.UnregisterClient("auth-a")
		})
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}

		execCalled := false
		manager.executors["gemini"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "gemini"},
			execute: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				execCalled = true
				return cliproxyexecutor.Response{}, nil
			},
		}

		scheduler := &fakePluginScheduler{
			resp: pluginapi.SchedulerPickResponse{
				Handled:      true,
				Reject:       true,
				RejectCode:   "policy_denied",
				RejectReason: "denied by scheduler",
			},
			handled: true,
		}
		manager.SetPluginScheduler(scheduler)

		opts := cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.SelectionIDMetadataKey: "stale-selection-id"},
		}
		_, errSelect := manager.SelectAuth(context.Background(), "gemini", "gemini-2.5-pro", opts)
		if errSelect == nil {
			t.Fatal("SelectAuth() error = nil, want rejection error")
		}
		if _, exists := opts.Metadata[cliproxyexecutor.SelectionIDMetadataKey]; exists {
			t.Fatalf("rejected SelectAuth kept selection_id in Metadata: %v", opts.Metadata)
		}

		_, errExec := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, cliproxyexecutor.Options{})
		if errExec == nil {
			t.Fatal("Execute() error = nil, want rejection error")
		}
		if execCalled {
			t.Fatal("executor was called after rejected Pick")
		}

		// Rejected Pick with default RejectCode ("auth_unavailable") must not trigger Antigravity credits fallback
		// on either Execute or ExecuteStream.
		creditsManager := NewManager(nil, &FillFirstSelector{}, nil)
		creditsManager.SetConfig(&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		})
		reg.RegisterClient("ag-auth-1", "antigravity", []*registry.ModelInfo{{ID: "claude-sonnet-4-5"}})
		t.Cleanup(func() {
			reg.UnregisterClient("ag-auth-1")
		})
		if _, errRegister := creditsManager.Register(context.Background(), &Auth{ID: "ag-auth-1", Provider: "antigravity"}); errRegister != nil {
			t.Fatalf("Register(ag-auth-1) error = %v", errRegister)
		}
		agExecCalled := false
		agStreamCalled := false
		creditsManager.executors["antigravity"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "antigravity"},
			execute: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				agExecCalled = true
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			},
			executeStream: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				agStreamCalled = true
				ch := make(chan cliproxyexecutor.StreamChunk, 1)
				close(ch)
				return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
			},
		}
		creditsManager.SetPluginScheduler(&fakePluginScheduler{
			resp: pluginapi.SchedulerPickResponse{
				Handled: true,
				Reject:  true, // Omit RejectCode so it defaults to "auth_unavailable".
			},
			handled: true,
		})
		if _, errAgExec := creditsManager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "claude-sonnet-4-5"}, cliproxyexecutor.Options{}); errAgExec == nil {
			t.Fatal("Execute(antigravity rejected) error = nil, want rejection error")
		}
		if agExecCalled {
			t.Fatal("antigravity executor Execute was called via credits fallback after rejected Pick")
		}
		if _, errAgStream := creditsManager.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "claude-sonnet-4-5"}, cliproxyexecutor.Options{Stream: true}); errAgStream == nil {
			t.Fatal("ExecuteStream(antigravity rejected) error = nil, want rejection error")
		}
		if agStreamCalled {
			t.Fatal("antigravity executor ExecuteStream was called via credits fallback after rejected Pick")
		}
	})

	t.Run("execute_count_and_stream_propagate_selection_id", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("auth-a", "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
		t.Cleanup(func() {
			reg.UnregisterClient("auth-a")
		})
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}

		var countSelID string
		var streamSelID string
		manager.executors["gemini"] = &callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "gemini"},
			countTokens: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				countSelID = coreusage.SelectionIDFromContext(ctx)
				return cliproxyexecutor.Response{Payload: []byte(`{"totalTokens":5}`)}, nil
			},
			executeStream: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				streamSelID = coreusage.SelectionIDFromContext(ctx)
				ch := make(chan cliproxyexecutor.StreamChunk, 1)
				ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ok\n\n")}
				close(ch)
				return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
			},
		}

		scheduler := &fakePluginScheduler{
			resp:    pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"},
			handled: true,
		}
		manager.SetPluginScheduler(scheduler)

		if _, errCount := manager.ExecuteCount(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, cliproxyexecutor.Options{}); errCount != nil {
			t.Fatalf("ExecuteCount() error = %v", errCount)
		}
		streamRes, errStream := manager.ExecuteStream(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, cliproxyexecutor.Options{Stream: true})
		if errStream != nil {
			t.Fatalf("ExecuteStream() error = %v", errStream)
		}
		for range streamRes.Chunks {
		}

		if len(scheduler.requests) != 2 {
			t.Fatalf("len(scheduler.requests) = %d, want 2", len(scheduler.requests))
		}
		wantCountID, _ := scheduler.requests[0].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		wantStreamID, _ := scheduler.requests[1].Options.Metadata[cliproxyexecutor.SelectionIDMetadataKey].(string)
		if countSelID == "" || countSelID != wantCountID {
			t.Fatalf("countSelID = %q, want %q", countSelID, wantCountID)
		}
		if streamSelID == "" || streamSelID != wantStreamID {
			t.Fatalf("streamSelID = %q, want %q", streamSelID, wantStreamID)
		}
		if countSelID == streamSelID {
			t.Fatalf("ExecuteCount and ExecuteStream shared SelectionID %q", countSelID)
		}
	})

	t.Run("home_execute_count_and_stream_clear_stale_selection_id", func(t *testing.T) {
		dispatcher := &retryContractHomeDispatcher{authIDs: []string{"home-auth-1", "home-auth-1", "home-auth-1"}}
		homeReg := executionregistry.New()
		manager := NewManager(nil, nil, nil)
		manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
		manager.PublishHomeDispatch(dispatcher, homeReg, 1)

		var execSelID, countSelID, streamSelID string
		var execMetaHasKey, countMetaHasKey, streamMetaHasKey bool
		manager.RegisterExecutor(&callbackSchedulerTestExecutor{
			schedulerTestExecutor: schedulerTestExecutor{provider: "home-retry-contract"},
			execute: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				execSelID = coreusage.SelectionIDFromContext(ctx)
				_, execMetaHasKey = opts.Metadata[cliproxyexecutor.SelectionIDMetadataKey]
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			},
			countTokens: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				countSelID = coreusage.SelectionIDFromContext(ctx)
				_, countMetaHasKey = opts.Metadata[cliproxyexecutor.SelectionIDMetadataKey]
				return cliproxyexecutor.Response{Payload: []byte(`{"totalTokens":3}`)}, nil
			},
			executeStream: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				streamSelID = coreusage.SelectionIDFromContext(ctx)
				_, streamMetaHasKey = opts.Metadata[cliproxyexecutor.SelectionIDMetadataKey]
				ch := make(chan cliproxyexecutor.StreamChunk, 1)
				close(ch)
				return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
			},
		})

		staleCtx := coreusage.WithSelectionID(context.Background(), "stale-ctx-selection-id")
		makeOpts := func(stream bool) cliproxyexecutor.Options {
			return cliproxyexecutor.Options{
				Stream:   stream,
				Metadata: map[string]any{cliproxyexecutor.SelectionIDMetadataKey: "stale-meta-selection-id"},
			}
		}

		if _, errExec := manager.Execute(staleCtx, []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"}, makeOpts(false)); errExec != nil {
			t.Fatalf("Home Execute() error = %v", errExec)
		}
		if _, errCount := manager.ExecuteCount(staleCtx, []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"}, makeOpts(false)); errCount != nil {
			t.Fatalf("Home ExecuteCount() error = %v", errCount)
		}
		streamRes, errStream := manager.ExecuteStream(staleCtx, []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"}, makeOpts(true))
		if errStream != nil {
			t.Fatalf("Home ExecuteStream() error = %v", errStream)
		}
		for range streamRes.Chunks {
		}

		if execSelID != "" || execMetaHasKey {
			t.Fatalf("Home Execute leaked SelectionID=%q metaHasKey=%v", execSelID, execMetaHasKey)
		}
		if countSelID != "" || countMetaHasKey {
			t.Fatalf("Home ExecuteCount leaked SelectionID=%q metaHasKey=%v", countSelID, countMetaHasKey)
		}
		if streamSelID != "" || streamMetaHasKey {
			t.Fatalf("Home ExecuteStream leaked SelectionID=%q metaHasKey=%v", streamSelID, streamMetaHasKey)
		}
	})
}

type callbackSchedulerTestExecutor struct {
	schedulerTestExecutor
	execute       func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	executeStream func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	countTokens   func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	refresh       func(context.Context, *Auth) (*Auth, error)
}

func (e *callbackSchedulerTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.execute != nil {
		return e.execute(ctx, auth, req, opts)
	}
	return cliproxyexecutor.Response{}, nil
}

func (e *callbackSchedulerTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e.executeStream != nil {
		return e.executeStream(ctx, auth, req, opts)
	}
	return nil, nil
}

func (e *callbackSchedulerTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.countTokens != nil {
		return e.countTokens(ctx, auth, req, opts)
	}
	return cliproxyexecutor.Response{}, nil
}

func (e *callbackSchedulerTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if e.refresh != nil {
		return e.refresh(ctx, auth)
	}
	return auth, nil
}
