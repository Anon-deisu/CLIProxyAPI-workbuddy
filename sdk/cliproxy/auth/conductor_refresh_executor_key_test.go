package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type countingRefreshExecutor struct {
	id           string
	refreshCalls atomic.Int32
}

func (e *countingRefreshExecutor) Identifier() string { return e.id }

func (e *countingRefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *countingRefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *countingRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "refreshed-token"
	return auth, nil
}

func (e *countingRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *countingRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRefreshAuthForRequest_UsesExecutorKeyFromAuth(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &countingRefreshExecutor{id: "openai-compatible-custom"}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "compat-oauth",
		Provider: "plugin-provider",
		Attributes: map[string]string{
			"compat_name":  "custom",
			"provider_key": "custom",
			"base_url":     "https://compat.example.com/v1",
		},
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-1",
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	refreshed, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, "old-token")
	if errRefresh != nil {
		t.Fatalf("refreshAuthForRequest() error = %v", errRefresh)
	}
	if executor.refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", executor.refreshCalls.Load())
	}
	if refreshed == nil || refreshed.Metadata["access_token"] != "refreshed-token" {
		t.Fatalf("refreshed auth = %#v, want updated access_token", refreshed)
	}
}

type quotaRefreshTestExecutor struct {
	schedulerProviderTestExecutor
	fetch   func(context.Context, *Auth) (http.Header, error)
	refresh func(context.Context, *Auth) (*Auth, error)
}

func (e *quotaRefreshTestExecutor) FetchQuota(ctx context.Context, auth *Auth) (http.Header, error) {
	return e.fetch(ctx, auth)
}

func (e *quotaRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if e.refresh != nil {
		return e.refresh(ctx, auth)
	}
	return auth, nil
}

type quotaRefreshTestTransport struct{}

func (*quotaRefreshTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected HTTP request")
}

func (rt *quotaRefreshTestTransport) RoundTripperFor(*Auth) http.RoundTripper { return rt }

type quotaRefreshTestStore struct {
	*memoryAuthTestStore
	saves atomic.Int32
}

func (s *quotaRefreshTestStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.saves.Add(1)
	return s.memoryAuthTestStore.Save(ctx, auth)
}

func TestManagerQuotaRefreshUnauthorizedRecovery(t *testing.T) {
	for _, outcome := range []string{"recovered", "still-401", "refresh-401", "refresh-string401", "refresh-network", "retry-network", "no-refresh-token", "policy-off"} {
		t.Run(outcome, func(t *testing.T) {
			store := &quotaRefreshTestStore{memoryAuthTestStore: newMemoryAuthTestStore()}
			hook := &quotaRefreshTestHook{}
			m := NewManager(store, nil, hook)
			cfg := &internalconfig.Config{}
			cfg.Routing.AutoDisableUnauthorized = outcome != "policy-off"
			m.runtimeConfig.Store(cfg)
			rt := &quotaRefreshTestTransport{}
			m.SetRoundTripperProvider(rt)
			auth := quotaRefreshTestAuth(t.Name())
			if outcome != "no-refresh-token" {
				auth.Metadata["refresh_token"] = "refresh"
			}
			auth.recordRecentRequest(time.Unix(10, 0), true)
			auth.ModelStates = make(map[string]*ModelState)
			for _, status := range []int{401, 429, 503} {
				auth.ModelStates[fmt.Sprint(status)] = &ModelState{
					Status: StatusError, Unavailable: true, StatusMessage: "unrelated model cooldown",
					LastError: &Error{HTTPStatus: status}, NextRetryAfter: time.Now().Add(time.Hour),
				}
			}
			before, err := m.Register(WithSkipPersist(t.Context()), auth)
			if err != nil {
				t.Fatal(err)
			}
			fetchCalls, refreshCalls := 0, 0
			checkTransport := func(ctx context.Context) {
				t.Helper()
				if ctx.Value(roundTripperContextKey{}) != rt || ctx.Value("cliproxy.roundtripper") != rt {
					t.Error("quota probe/recovery lost the per-auth context transport")
				}
			}
			exec := &quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"}}
			exec.fetch = func(ctx context.Context, auth *Auth) (http.Header, error) {
				checkTransport(ctx)
				fetchCalls++
				if fetchCalls == 1 || outcome == "still-401" || outcome == "policy-off" {
					return nil, &Error{HTTPStatus: 401, Message: "unauthorized"}
				}
				if authAccessToken(auth) != "fresh" {
					t.Error("quota retry used stale token")
				}
				if outcome == "retry-network" {
					return nil, errors.New("network")
				}
				return http.Header{"X-Codex-Primary-Used-Percent": {"40"}}, nil
			}
			exec.refresh = func(ctx context.Context, auth *Auth) (*Auth, error) {
				checkTransport(ctx)
				refreshCalls++
				if outcome == "refresh-401" {
					return nil, &Error{HTTPStatus: 401, Message: "invalid refresh"}
				}
				if outcome == "refresh-string401" {
					return nil, errors.New("token refresh failed with status 401: invalid_grant")
				}
				if outcome == "refresh-network" {
					return nil, errors.New("network")
				}
				auth.Metadata["access_token"] = "fresh"
				auth.Metadata["refresh_token"] = "rotated-refresh"
				return auth, nil
			}
			m.RegisterExecutor(exec)
			m.refreshQuota(t.Context(), auth.ID)
			after, _ := m.GetByID(auth.ID)
			wantFetch, wantRefresh := 2, 1
			if outcome == "no-refresh-token" {
				wantFetch, wantRefresh = 1, 0
			}
			if outcome == "refresh-401" || outcome == "refresh-string401" || outcome == "refresh-network" {
				wantFetch = 1
			}
			if fetchCalls != wantFetch || refreshCalls != wantRefresh {
				t.Fatalf("fetch/refresh calls = %d/%d, want %d/%d", fetchCalls, refreshCalls, wantFetch, wantRefresh)
			}
			wantDisable := outcome == "still-401" || outcome == "refresh-401" || outcome == "no-refresh-token"
			if after.AutoDisabled != wantDisable || (wantDisable && after.AutoDisableReason != AutoDisableReasonUnauthorized) {
				t.Fatalf("automatic disable = %t/%q, want disabled=%t", after.AutoDisabled, after.AutoDisableReason, wantDisable)
			}
			if outcome == "recovered" {
				if after.Quota.Signals["X-Codex-Primary-Used-Percent"] != "40" {
					t.Fatal("recovered quota missing")
				}
			} else if !reflect.DeepEqual(before.Quota, after.Quota) {
				t.Fatal("failed quota probe erased old quota")
			}
			if after.Success != before.Success || after.Failed != before.Failed || after.recentRequests != before.recentRequests || hook.results != 0 {
				t.Fatal("background quota recovery changed request accounting")
			}
			if !reflect.DeepEqual(before.ModelStates, after.ModelStates) {
				t.Fatal("quota recovery reset an unrelated model cooldown")
			}
			wantSaves := int32(0)
			if wantFetch == 2 {
				wantSaves = 1
			}
			if store.saves.Load() != wantSaves {
				t.Fatalf("credential writes = %d, want %d", store.saves.Load(), wantSaves)
			}
			stored, errList := store.List(t.Context())
			if errList != nil {
				t.Fatal(errList)
			}
			if wantSaves == 1 {
				if len(stored) != 1 || authAccessToken(stored[0]) != "fresh" || authMetadataString(stored[0], "refresh_token") != "rotated-refresh" {
					t.Fatal("rotated OAuth credentials were not persisted")
				}
				if !reflect.DeepEqual(stored[0].Quota, before.Quota) {
					t.Fatal("quota observation triggered another credential write")
				}
			} else if len(stored) != 0 {
				t.Fatal("failed recovery persisted credentials")
			}
		})
	}
}

func TestManagerQuotaRefreshFiltersAndPreservesDispatchSlots(t *testing.T) {
	m := NewManager(nil, nil, nil)
	var mu sync.Mutex
	seen := map[string]int{}
	m.RegisterExecutor(&quotaRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
		fetch: func(_ context.Context, auth *Auth) (http.Header, error) {
			mu.Lock()
			seen[auth.ID]++
			mu.Unlock()
			if auth.ActiveDispatchRequests() != 1 {
				t.Error("probe changed business dispatch slots")
			}
			return http.Header{"X-Codex-Primary-Used-Percent": {"40"}}, nil
		},
	})
	for _, kind := range []string{"active", "degraded", "5h-paused", "cooling", "manual", "automatic", "status-disabled", "no-token", "no-account", "apikey", "other-provider"} {
		auth := quotaRefreshTestAuth(kind)
		switch kind {
		case "degraded":
			auth.Metadata["degraded"] = true
		case "5h-paused":
			auth.CodexQuotaResumeAt = time.Now().Add(time.Hour)
		case "cooling":
			auth.Unavailable, auth.Status, auth.NextRetryAfter = true, StatusError, time.Now().Add(time.Hour)
		case "manual":
			auth.Disabled = true
		case "automatic":
			auth.AutoDisabled = true
		case "status-disabled":
			auth.Status = StatusDisabled
		case "no-token":
			delete(auth.Metadata, "access_token")
		case "no-account":
			delete(auth.Metadata, "account_id")
		case "apikey":
			auth.Attributes = map[string]string{AttributeAPIKey: "key"}
		case "other-provider":
			auth.Provider = "claude"
		}
		auth.SetConcurrencyLimit(1)
		if !auth.TryAcquireDispatchSlot() {
			t.Fatal("reserve business slot")
		}
		t.Cleanup(auth.ReleaseDispatchSlot)
		if _, err := m.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	m.homeRuntimeAuths["remote"] = map[string]*Auth{"home": quotaRefreshTestAuth("home")}
	m.refreshQuotaBatch(t.Context(), 2)
	want := map[string]int{"active": 1, "degraded": 1, "5h-paused": 1, "cooling": 1}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("probes = %v, want %v", seen, want)
	}
	cfg := &internalconfig.Config{}
	cfg.Home.Enabled = true
	m.runtimeConfig.Store(cfg)
	m.refreshQuotaBatch(t.Context(), 2)
	if !reflect.DeepEqual(seen, want) {
		t.Fatal("Home mode probed local credentials")
	}
	cfg = &internalconfig.Config{}
	m.runtimeConfig.Store(cfg)
	m.RegisterExecutor(schedulerProviderTestExecutor{provider: "codex"})
	m.refreshQuotaBatch(t.Context(), 2)
	if !reflect.DeepEqual(seen, want) {
		t.Fatal("old quota executor used after replacement")
	}
}

func TestManagerQuotaRefreshPollingReloadAndExecutor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		if _, err := m.Register(t.Context(), quotaRefreshTestAuth(t.Name())); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		exec := &quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			fetch: func(context.Context, *Auth) (http.Header, error) { calls.Add(1); return nil, nil },
		}
		m.RegisterExecutor(exec)
		m.StartQuotaRefresh(t.Context())
		defer m.StopQuotaRefresh()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("startup probes = %d, want 1", calls.Load())
		}
		<-time.After(5*time.Minute - time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("default interval fired early")
		}
		<-time.After(time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("default interval did not poll")
		}
		cfg := &internalconfig.Config{}
		cfg.Codex.QuotaRefresh.Interval = "1m"
		m.runtimeConfig.Store(cfg)
		m.UpdateQuotaRefresh()
		synctest.Wait()
		<-time.After(time.Minute)
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatal("interval reload did not take effect")
		}
		next := *cfg
		next.Codex.QuotaRefresh.Enabled = new(false)
		m.runtimeConfig.Store(&next)
		m.UpdateQuotaRefresh()
		synctest.Wait()
		<-time.After(10 * time.Minute)
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatal("disabled loop continued polling")
		}
		homeConfig := *cfg
		homeConfig.Home.Enabled = true
		m.runtimeConfig.Store(&homeConfig)
		m.UpdateQuotaRefresh()
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatal("Home mode polled local credentials")
		}
		m.runtimeConfig.Store(cfg)
		m.UpdateQuotaRefresh()
		synctest.Wait()
		if calls.Load() != 4 {
			t.Fatal("reenabling did not poll immediately")
		}
		var replacementCalls atomic.Int32
		m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			fetch: func(context.Context, *Auth) (http.Header, error) { replacementCalls.Add(1); return nil, nil },
		})
		<-time.After(time.Minute)
		synctest.Wait()
		if replacementCalls.Load() != 1 || calls.Load() != 4 {
			t.Fatal("poll did not use the current registered executor")
		}
	})
}

func TestManagerQuotaRefreshConcurrencyCancellationAndWait(t *testing.T) {
	for _, concurrency := range []int{0, 3} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &internalconfig.Config{}
				cfg.Codex.QuotaRefresh.Concurrency = concurrency
				m.runtimeConfig.Store(cfg)
				_, _, limit := cfg.Codex.QuotaRefresh.Effective()
				for i := range 7 {
					if _, err := m.Register(t.Context(), quotaRefreshTestAuth(fmt.Sprint(i))); err != nil {
						t.Fatal(err)
					}
				}
				var calls, active, canceled atomic.Int32
				release := make(chan struct{})
				m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
					fetch: func(ctx context.Context, auth *Auth) (http.Header, error) {
						calls.Add(1)
						active.Add(1)
						defer active.Add(-1)
						<-ctx.Done()
						canceled.Add(1)
						<-release
						return http.Header{"X-Codex-Primary-Used-Percent": {"99"}}, nil
					},
				})
				m.StartQuotaRefresh(t.Context())
				synctest.Wait()
				if int(active.Load()) != limit {
					t.Fatalf("concurrency = %d, want %d", active.Load(), limit)
				}
				<-time.After(11 * time.Minute)
				synctest.Wait()
				if int(calls.Load()) != limit {
					t.Fatal("polling rounds overlapped")
				}
				stopped := make(chan struct{})
				go func() { m.StopQuotaRefresh(); close(stopped) }()
				synctest.Wait()
				if int(canceled.Load()) != limit {
					t.Fatal("stop did not cancel active probes")
				}
				select {
				case <-stopped:
					t.Fatal("stop returned before probes completed")
				default:
				}
				close(release)
				<-stopped
				if active.Load() != 0 || int(calls.Load()) != limit {
					t.Fatal("stop leaked workers or started queued probes")
				}
				for _, auth := range m.List() {
					if auth.Quota.Signals["X-Codex-Primary-Used-Percent"] != "10" {
						t.Fatal("canceled probe committed late success")
					}
				}
				m.StopQuotaRefresh()
			})
		})
	}
}

func TestManagerQuotaRefreshSingleProbePerAuth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		auth, err := m.Register(t.Context(), quotaRefreshTestAuth(t.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			fetch: func(ctx context.Context, _ *Auth) (http.Header, error) {
				calls.Add(1)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { m.refreshQuota(ctx, auth.ID) })
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("same-account probes = %d, want 1", calls.Load())
		}
		cancel()
		wg.Wait()
	})
}

func TestManagerQuotaRefreshRecoveryRejectsReplacedCredentials(t *testing.T) {
	for _, stage := range []string{"fetch", "refresh"} {
		for _, mutation := range []string{"removed", "epoch", "token", "refresh-token", "provider", "account", "manual", "auto-disabled", "status-disabled", "home", "config-disabled", "canceled"} {
			t.Run(stage+"/"+mutation, func(t *testing.T) {
				store := &quotaRefreshTestStore{memoryAuthTestStore: newMemoryAuthTestStore()}
				m := NewManager(store, nil, nil)
				cfg := &internalconfig.Config{}
				cfg.Routing.AutoDisableUnauthorized = true
				m.runtimeConfig.Store(cfg)
				auth := quotaRefreshTestAuth(t.Name())
				auth.Metadata["refresh_token"] = "refresh"
				auth.ModelStates = map[string]*ModelState{"unrelated": {
					Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour), LastError: &Error{HTTPStatus: 503},
				}}
				if _, err := m.Register(WithSkipPersist(t.Context()), auth); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var want *Auth
				replace := func() {
					m.mu.Lock()
					defer m.mu.Unlock()
					current := m.auths[auth.ID]
					switch mutation {
					case "removed":
						delete(m.auths, auth.ID)
					case "epoch":
						current.RegistrationEpoch++
					case "token":
						current.Metadata["access_token"] = "replacement"
					case "refresh-token":
						current.Metadata["refresh_token"] = "replacement"
					case "provider":
						current.Provider = "claude"
					case "account":
						current.Metadata["account_id"] = "replacement"
					case "manual":
						current.Disabled = true
						current.StatusMessage = "operator disabled"
					case "auto-disabled":
						current.AutoDisabled, current.AutoDisableReason = true, AutoDisableReasonUnauthorized
						current.AutoDisabledAt, current.Status, current.StatusMessage = time.Now(), StatusError, "unauthorized"
						current.LastError = &Error{HTTPStatus: 401}
					case "status-disabled":
						current.Status = StatusDisabled
					case "home":
						next := *cfg
						next.Home.Enabled = true
						m.runtimeConfig.Store(&next)
					case "config-disabled":
						next := *cfg
						next.Codex.QuotaRefresh.Enabled = new(false)
						m.runtimeConfig.Store(&next)
					case "canceled":
						cancel()
					}
					want = m.auths[auth.ID].Clone()
				}
				fetchCalls, refreshCalls := 0, 0
				m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
					fetch: func(context.Context, *Auth) (http.Header, error) {
						fetchCalls++
						if stage == "fetch" {
							replace()
						}
						return nil, &Error{HTTPStatus: 401}
					},
					refresh: func(_ context.Context, updated *Auth) (*Auth, error) {
						refreshCalls++
						if stage == "refresh" {
							replace()
						}
						updated.Metadata["access_token"] = "fresh"
						updated.Metadata["refresh_token"] = "rotated-refresh"
						return updated, nil
					},
				})
				m.refreshQuota(ctx, auth.ID)
				got, _ := m.GetByID(auth.ID)
				wantSaves := int32(0)
				if stage == "refresh" && (mutation == "manual" || mutation == "auto-disabled" || mutation == "status-disabled" || mutation == "home" || mutation == "config-disabled" || mutation == "canceled") {
					wantSaves = 1
					if got == nil || authAccessToken(got) != "fresh" || authMetadataString(got, "refresh_token") != "rotated-refresh" {
						t.Fatal("successful rotation was discarded when quota polling stopped")
					}
					want.Metadata["access_token"], want.Metadata["refresh_token"] = "fresh", "rotated-refresh"
					want.Generation, want.UpdatedAt = got.Generation, got.UpdatedAt
					want.LastRefreshedAt, want.NextRefreshAfter = got.LastRefreshedAt, got.NextRefreshAfter
					stored, errList := store.List(t.Context())
					if errList != nil || len(stored) != 1 || authMetadataString(stored[0], "refresh_token") != "rotated-refresh" {
						t.Fatal("rotated refresh token was not persisted with a live commit context")
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatal("probe changed identity, disable state, cooldown, quota, or request accounting")
				}
				if store.saves.Load() != wantSaves {
					t.Fatalf("credential saves = %d, want %d", store.saves.Load(), wantSaves)
				}
				wantRefresh := 0
				if stage == "refresh" {
					wantRefresh = 1
				}
				if fetchCalls != 1 || refreshCalls != wantRefresh {
					t.Fatalf("fetch/refresh = %d/%d, want 1/%d", fetchCalls, refreshCalls, wantRefresh)
				}
			})
		}
	}
}

type quotaOAuthRefreshError struct {
	status int
	code   string
}

func (e quotaOAuthRefreshError) Error() string     { return "OAuth refresh rejected" }
func (e quotaOAuthRefreshError) StatusCode() int   { return e.status }
func (e quotaOAuthRefreshError) ErrorCode() string { return e.code }

func TestManagerQuotaRefreshExplicitOAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		disabled bool
	}{
		{"invalid-grant", quotaOAuthRefreshError{400, "invalid_grant"}, true},
		{"reused", fmt.Errorf("refresh: %w", quotaOAuthRefreshError{400, "refresh_token_reused"}), true},
		{"expired", quotaOAuthRefreshError{400, "refresh_token_expired"}, true},
		{"invalidated", quotaOAuthRefreshError{401, "refresh_token_invalidated"}, true},
		{"invalid-client", quotaOAuthRefreshError{400, "invalid_client"}, false},
		{"unknown-400", quotaOAuthRefreshError{400, ""}, false},
		{"server", quotaOAuthRefreshError{503, "invalid_grant"}, false},
		{"rate-limit", quotaOAuthRefreshError{429, "refresh_token_reused"}, false},
		{"network", errors.New("network failure mentioning invalid_grant"), false},
		{"canceled", errors.Join(context.Canceled, quotaOAuthRefreshError{400, "invalid_grant"}), false},
	} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/policy=%t", tc.name, enabled), func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &internalconfig.Config{}
				cfg.Routing.AutoDisableUnauthorized = enabled
				m.runtimeConfig.Store(cfg)
				auth := quotaRefreshTestAuth(t.Name())
				auth.Metadata["refresh_token"] = "refresh"
				before, errRegister := m.Register(t.Context(), auth)
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				fetches, refreshes := 0, 0
				m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
					fetch:   func(context.Context, *Auth) (http.Header, error) { fetches++; return nil, &Error{HTTPStatus: 401} },
					refresh: func(context.Context, *Auth) (*Auth, error) { refreshes++; return nil, tc.err },
				})
				m.refreshQuota(t.Context(), auth.ID)
				after, _ := m.GetByID(auth.ID)
				if after.AutoDisabled != (enabled && tc.disabled) {
					t.Fatalf("disabled = %t, want %t", after.AutoDisabled, enabled && tc.disabled)
				}
				if fetches != 1 || refreshes != 1 {
					t.Fatalf("fetch/refresh = %d/%d, want 1/1", fetches, refreshes)
				}
				if !reflect.DeepEqual(before.Quota, after.Quota) || before.Success != after.Success || before.Failed != after.Failed || before.recentRequests != after.recentRequests {
					t.Fatal("OAuth failure changed quota or request counts")
				}
			})
		}
	}
}

func TestManagerQuotaRefreshStopWaitsForRotatedCredentialCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &quotaRefreshTestStore{memoryAuthTestStore: newMemoryAuthTestStore()}
		m := NewManager(store, nil, nil)
		auth := quotaRefreshTestAuth(t.Name())
		auth.Metadata["refresh_token"] = "refresh"
		before, errRegister := m.Register(WithSkipPersist(t.Context()), auth)
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var fetches atomic.Int32
		m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			fetch: func(context.Context, *Auth) (http.Header, error) { fetches.Add(1); return nil, &Error{HTTPStatus: 401} },
			refresh: func(_ context.Context, updated *Auth) (*Auth, error) {
				close(entered)
				<-release
				updated.Metadata["access_token"], updated.Metadata["refresh_token"] = "fresh", "rotated-refresh"
				return updated, nil
			},
		})
		m.StartQuotaRefresh(t.Context())
		<-entered
		stopped := make(chan struct{})
		go func() { m.StopQuotaRefresh(); close(stopped) }()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned before credential acquisition finished")
		default:
		}
		close(release)
		<-stopped
		stored, errList := store.List(t.Context())
		if errList != nil || len(stored) != 1 || authMetadataString(stored[0], "refresh_token") != "rotated-refresh" {
			t.Fatal("stop discarded a successfully rotated refresh token")
		}
		after, _ := m.GetByID(auth.ID)
		if fetches.Load() != 1 || !reflect.DeepEqual(before.Quota, after.Quota) {
			t.Fatal("stop continued quota probing after credential commit")
		}
	})
}

func TestManagerQuotaRefreshCredentialCommitGuardsIdentityUnderLock(t *testing.T) {
	for _, mutation := range []string{"epoch", "access", "refresh", "account", "provider"} {
		t.Run(mutation, func(t *testing.T) {
			store := &quotaRefreshTestStore{memoryAuthTestStore: newMemoryAuthTestStore()}
			m := NewManager(store, nil, nil)
			auth := quotaRefreshTestAuth(t.Name())
			auth.Metadata["refresh_token"] = "refresh"
			base, errRegister := m.Register(WithSkipPersist(t.Context()), auth)
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			updated := base.Clone()
			updated.Metadata["access_token"], updated.Metadata["refresh_token"] = "fresh", "rotated-refresh"
			m.mu.Lock()
			current := m.auths[auth.ID]
			switch mutation {
			case "epoch":
				current.RegistrationEpoch++
			case "access":
				current.Metadata["access_token"] = "replacement"
			case "refresh":
				current.Metadata["refresh_token"] = "replacement"
			case "account":
				current.Metadata["account_id"] = "replacement"
			case "provider":
				current.Provider = "claude"
			}
			want := current.Clone()
			m.mu.Unlock()
			ctx := context.WithValue(t.Context(), quotaRefreshRecoveryKey{}, &quotaRefreshRecovery{observed: base})
			if _, errUpdate := m.UpdateRefreshedAuth(ctx, base, updated); errUpdate == nil {
				t.Fatal("UpdateRefreshedAuth accepted a stale quota recovery")
			}
			got, _ := m.GetByID(auth.ID)
			if !reflect.DeepEqual(got, want) || store.saves.Load() != 0 {
				t.Fatal("stale rotation overwrote replacement credentials")
			}
		})
	}
}

func TestManagerQuotaRefreshNetworkFailureKeepsRuntimeState(t *testing.T) {
	for _, probeErr := range []error{errors.New("invalid JSON"), errors.New("connection reset"), &Error{HTTPStatus: 403}, &Error{HTTPStatus: 429}, &Error{HTTPStatus: 503}} {
		t.Run(probeErr.Error(), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			cfg := &internalconfig.Config{}
			cfg.Routing.AutoDisableQuota, cfg.Routing.AutoDisableUnauthorized = true, true
			m.runtimeConfig.Store(cfg)
			before, err := m.Register(t.Context(), quotaRefreshTestAuth(t.Name()))
			if err != nil {
				t.Fatal(err)
			}
			m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
				fetch: func(context.Context, *Auth) (http.Header, error) {
					return http.Header{"X-Codex-Primary-Used-Percent": {"100"}}, probeErr
				},
			})
			m.refreshQuota(t.Context(), before.ID)
			after, _ := m.GetByID(before.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed probe changed auth runtime state")
			}
		})
	}
}

func TestManagerQuotaRefreshRestartWaitsAndParentCancellationStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		if _, err := m.Register(t.Context(), quotaRefreshTestAuth(t.Name())); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		release := make(chan struct{})
		m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			fetch: func(ctx context.Context, _ *Auth) (http.Header, error) {
				if calls.Add(1) == 1 {
					<-ctx.Done()
					<-release
				}
				return nil, nil
			},
		})
		m.StartQuotaRefresh(t.Context())
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		restarted := make(chan struct{})
		go func() { m.StartQuotaRefresh(ctx); close(restarted) }()
		synctest.Wait()
		select {
		case <-restarted:
			t.Fatal("restart returned before old probe finished")
		default:
		}
		if calls.Load() != 1 {
			t.Fatal("restart overlapped old probe")
		}
		close(release)
		<-restarted
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("restart did not immediately poll")
		}
		m.mu.RLock()
		done := m.quotaRefreshLoop.done
		m.mu.RUnlock()
		cancel()
		<-done
		m.StopQuotaRefresh()
		if calls.Load() != 2 {
			t.Fatal("parent cancellation started another round")
		}
	})
}

func TestManagerQuotaRefreshConfigPollingCancelsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		if _, err := m.Register(t.Context(), quotaRefreshTestAuth(t.Name())); err != nil {
			t.Fatal(err)
		}
		var calls, canceled atomic.Int32
		m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			fetch: func(ctx context.Context, _ *Auth) (http.Header, error) {
				calls.Add(1)
				<-ctx.Done()
				canceled.Add(1)
				return nil, ctx.Err()
			},
		})
		m.StartQuotaRefresh(t.Context())
		defer m.StopQuotaRefresh()
		synctest.Wait()
		cfg := &internalconfig.Config{}
		cfg.Codex.QuotaRefresh.Enabled = new(false)
		m.runtimeConfig.Store(cfg)
		<-time.After(5 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 || canceled.Load() != 1 {
			t.Fatal("config polling did not cancel the disabled round")
		}
		m.runtimeConfig.Store(&internalconfig.Config{})
		<-time.After(5 * time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("config polling did not restart after enabling")
		}
	})
}

func TestManagerQuotaRefreshCancellationWhileWaitingForCredentialRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		auth := quotaRefreshTestAuth(t.Name())
		auth.Metadata["refresh_token"] = "refresh"
		if _, err := m.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		m.RegisterExecutor(&quotaRefreshTestExecutor{schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
			fetch:   func(context.Context, *Auth) (http.Header, error) { return nil, &Error{HTTPStatus: 401} },
			refresh: func(_ context.Context, auth *Auth) (*Auth, error) { close(entered); <-release; return auth, nil },
		})
		ordinaryDone := make(chan struct{})
		go func() { defer close(ordinaryDone); _, _ = m.ForceRefreshAuth(t.Context(), auth.ID) }()
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		probeDone := make(chan struct{})
		go func() { defer close(probeDone); m.refreshQuota(ctx, auth.ID) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-probeDone:
		default:
			t.Error("canceled probe is stuck behind another credential refresh")
		}
		close(release)
		<-ordinaryDone
		<-probeDone
	})
}
