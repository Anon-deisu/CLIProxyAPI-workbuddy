package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestWeightedRoundRobinRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "wrr"},
	})
	if state.strategy != "weighted-round-robin" {
		t.Fatalf("strategy = %q, want weighted-round-robin", state.strategy)
	}
	// This fork always wraps the strategy selector with session affinity so
	// Cursor conversations stay pinned to one account (see newRoutingSelector).
	if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state))
	}
}

func TestDefaultRoutingBalancesRemainingQuota(t *testing.T) {
	for _, strategy := range []string{"", "round-robin", "codex2api-default"} {
		t.Run(strategy, func(t *testing.T) {
			cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: strategy, AuthConcurrencyLimit: new(0)}}
			selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
			t.Cleanup(selector.(coreauth.StoppableSelector).Stop)
			auths := []*coreauth.Auth{
				{ID: "a", Provider: "codex", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{Signals: map[string]string{"used-percent": "10"}}},
				{ID: "b", Provider: "codex", Status: coreauth.StatusActive, Quota: coreauth.QuotaState{Signals: map[string]string{"used-percent": "40"}}},
			}
			counts := map[string]int{}
			for range 50 {
				selected, err := selector.Pick(t.Context(), "codex", "", cliproxyexecutor.Options{}, auths)
				if err != nil {
					t.Fatal(err)
				}
				counts[selected.ID]++
			}
			if counts["a"] != 30 || counts["b"] != 20 {
				t.Fatalf("configured picks=%v, want 30:20", counts)
			}
		})
	}
}

func TestServiceRejectsInvalidCredentialWeightConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{}
	service := &Service{cfg: originalCfg}
	invalidWeight := internalconfig.MaxCredentialWeight + 1
	newCfg := &internalconfig.Config{
		VertexCompatAPIKey: []internalconfig.VertexCompatKey{{
			APIKey: "vertex-key",
			Weight: &invalidWeight,
		}},
	}

	if service.applyConfigUpdateWithAuthSynthesis(nil, newCfg, true) {
		t.Fatal("hot config application accepted an invalid credential weight")
	}
	if service.cfg != originalCfg {
		t.Fatal("invalid hot config replaced the active config")
	}
	if service.configSequence != 0 {
		t.Fatalf("config sequence = %d, want 0", service.configSequence)
	}
}

type trackingStoppableSelector struct {
	stopped bool
}

func (s *trackingStoppableSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (s *trackingStoppableSelector) Stop() {
	s.stopped = true
}

func TestApplyManagerConfigStopsReplacedServiceAffinitySelector(t *testing.T) {
	tracking := &trackingStoppableSelector{}
	service := &Service{
		coreManager: coreauth.NewManager(nil, tracking, nil),
	}

	newCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "round-robin",
		},
	}
	commit := configCommit{cfg: newCfg, sequence: 1}
	if !service.applyManagerConfig(context.Background(), commit) {
		t.Fatal("applyManagerConfig failed")
	}

	if !tracking.stopped {
		t.Fatal("expected replaced selector to be stopped during routing config apply")
	}
}

func TestCodexRoutingAffinityWithSuccessfulQuotaUpdates(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		accounts []string
		affinity bool
		want     map[string]int
	}{
		{"account-a", []string{"a"}, false, map[string]int{"a": 6}},
		{"account-b", []string{"b"}, false, map[string]int{"b": 6}},
		{"two-accounts", []string{"a", "b"}, false, map[string]int{"a": 3, "b": 3}},
		{"two-accounts-affinity", []string{"a", "b"}, true, map[string]int{"a": 6}},
	} {
		for _, convergence := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/convergence=%t", testCase.name, convergence), func(t *testing.T) {
				const model = "gpt-5.4"
				const cacheKey = "same-client-cache-key"
				const turns = 6
				type observation struct {
					account, cacheKey, sessionID, installationID string
				}
				observed := make(chan observation, turns*2)
				quota := map[string]int{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, errRead := io.ReadAll(r.Body)
					if errRead != nil {
						t.Errorf("read request: %v", errRead)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					account := r.Header.Get("Chatgpt-Account-Id")
					if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer synthetic-"+account {
						t.Errorf("unexpected upstream request for account %q", account)
					}
					observed <- observation{
						account: account, cacheKey: gjson.GetBytes(body, "prompt_cache_key").String(),
						sessionID: r.Header.Get("Session-Id"), installationID: r.Header.Get("X-Codex-Installation-Id"),
					}
					quota[account]++
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("X-Codex-Primary-Used-Percent", strconv.Itoa(quota[account]))
					const response = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"routing-test\"}}\n\n" +
						"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\",\"output_index\":0,\"content_index\":0}\n\n" +
						"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"routing-test\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
					if _, errWrite := io.WriteString(w, response); errWrite != nil {
						t.Errorf("write response: %v", errWrite)
					}
				}))
				t.Cleanup(server.Close)
				cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{
					Strategy: "round-robin", SessionAffinity: testCase.affinity,
					AuthConcurrencyLimit: new(0), CodexFingerprintConvergence: &convergence,
				}}
				selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
				t.Cleanup(selector.(coreauth.StoppableSelector).Stop)
				manager := coreauth.NewManager(nil, selector, nil)
				manager.SetConfig(cfg)
				manager.SetRetryConfig(0, 0, 1)
				manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
				for _, account := range testCase.accounts {
					id := t.Name() + "-" + account
					registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					_, errRegister := manager.Register(t.Context(), &coreauth.Auth{
						ID: id, Provider: "codex", Status: coreauth.StatusActive,
						Attributes: map[string]string{"base_url": server.URL},
						Metadata:   map[string]any{"type": "codex", "access_token": "synthetic-" + account, "account_id": account},
					})
					if errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				var accounts []string
				installations := map[string]string{}
				for turn := 0; turn < turns; turn++ {
					body := []byte(fmt.Sprintf(`{"model":%q,"prompt_cache_key":%q,"input":[{"role":"user","content":"same conversation turn %d"}]}`, model, cacheKey, turn))
					stream, errStream := manager.ExecuteStream(t.Context(), []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: body}, cliproxyexecutor.Options{
						Stream: true, SourceFormat: sdktranslator.FormatOpenAIResponse,
						OriginalRequest: body, Headers: http.Header{"Session-Id": {cacheKey}},
					})
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
					if len(observed) != 1 {
						t.Fatalf("upstream attempts per turn = %d, want exactly 1", len(observed))
					}
					got := <-observed
					accounts = append(accounts, got.account)
					if got.cacheKey != cacheKey || got.sessionID != cacheKey {
						t.Fatalf("client cache identity changed: key=%q session=%q", got.cacheKey, got.sessionID)
					}
					if prior, exists := installations[got.account]; exists && got.installationID != prior {
						t.Fatal("installation identity changed within an account")
					}
					installations[got.account] = got.installationID
				}
				counts := map[string]int{}
				for _, account := range accounts {
					counts[account]++
				}
				if !reflect.DeepEqual(counts, testCase.want) {
					t.Fatalf("account sequence = %v, counts = %v, want %v", accounts, counts, testCase.want)
				}
				if convergence && len(installations) == 2 && installations["a"] == installations["b"] {
					t.Fatal("different accounts share a converged installation identity")
				}
				var successes, failures int64
				for _, auth := range manager.List() {
					successes += auth.Success
					failures += auth.Failed
					if auth.EffectiveConcurrencyLimit() != 0 || auth.ActiveDispatchRequests() != 0 {
						t.Fatal("test did not run with unlimited, released dispatch slots")
					}
				}
				if successes != turns || failures != 0 {
					t.Fatalf("results: success=%d failed=%d", successes, failures)
				}
				t.Logf("accounts=%v successes=%d failures=%d; upstream attempts=%d, client cache key unchanged", accounts, successes, failures, turns)
			})
		}
	}
}

func TestCodexRoutingDeliversTextBeforeCompletionAndUsage(t *testing.T) {
	for _, format := range []string{"openai-response", "openai", "claude"} {
		for _, rewrite := range []bool{false, true} {
			for _, buffering := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/rewrite=%t/bootstrap=%t", format, rewrite, buffering), func(t *testing.T) {
					const model = "gpt-5.4"
					const marker = "first-text-before-usage"
					allowCompletion := make(chan struct{})
					releaseCompletion := sync.OnceFunc(func() { close(allowCompletion) })
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						initial := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"streaming-test\",\"model\":\"gpt-5.4\"}}\n\n" +
							"data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + marker + "\",\"output_index\":0,\"content_index\":0}\n\n"
						if _, errWrite := io.WriteString(w, initial); errWrite != nil {
							t.Errorf("write initial events: %v", errWrite)
							return
						}
						w.(http.Flusher).Flush()
						select {
						case <-allowCompletion:
						case <-r.Context().Done():
							return
						}
						terminal := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"streaming-test\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1,\"total_tokens\":101}}}\n\n"
						if _, errWrite := io.WriteString(w, terminal); errWrite != nil {
							t.Errorf("write terminal event: %v", errWrite)
						}
					}))
					t.Cleanup(server.Close)
					t.Cleanup(releaseCompletion)
					cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "round-robin", AuthConcurrencyLimit: new(0)}}
					cfg.Codex.StreamBootstrapBuffering = buffering
					cfg.CacheTokenRewrite = &internalconfig.CacheTokenRewriteConfig{Enabled: rewrite, MinPercent: new(95.0)}
					selector := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
					t.Cleanup(selector.(coreauth.StoppableSelector).Stop)
					manager := coreauth.NewManager(nil, selector, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(0, 0, 1)
					manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
					for _, account := range []string{"a", "b"} {
						id := t.Name() + "-" + account
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
						if _, errRegister := manager.Register(t.Context(), &coreauth.Auth{
							ID: id, Provider: "codex", Status: coreauth.StatusActive,
							Attributes: map[string]string{"base_url": server.URL},
							Metadata:   map[string]any{"access_token": "synthetic-" + account, "account_id": account},
						}); errRegister != nil {
							t.Fatal(errRegister)
						}
					}
					body := []byte(`{"model":"gpt-5.4","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`)
					if format == "openai-response" {
						body = []byte(`{"model":"gpt-5.4","stream":true,"input":[{"role":"user","content":"hello"}]}`)
					}
					handler := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					firstText := make(chan struct{})
					done := make(chan error, 1)
					go func() {
						data, _, errs := handler.ExecuteStreamWithAuthManager(ctx, format, model, body, "")
						var streamErr error
						seenText := false
						for data != nil || errs != nil {
							select {
							case payload, ok := <-data:
								if !ok {
									data = nil
								} else if !seenText && strings.Contains(string(payload), marker) {
									seenText = true
									close(firstText)
								}
							case errMessage, ok := <-errs:
								if !ok {
									errs = nil
								} else if errMessage != nil {
									streamErr = errMessage.Error
								}
							}
						}
						done <- streamErr
					}()
					timer := time.NewTimer(5 * time.Second)
					defer timer.Stop()
					select {
					case <-firstText:
					case errStream := <-done:
						t.Fatalf("stream ended without initial text: %v", errStream)
					case <-timer.C:
						cancel()
						releaseCompletion()
						<-done
						t.Fatal("initial text was held until upstream completion or usage")
					}
					releaseCompletion()
					if errStream := <-done; errStream != nil {
						t.Fatal(errStream)
					}
				})
			}
		}
	}
}
