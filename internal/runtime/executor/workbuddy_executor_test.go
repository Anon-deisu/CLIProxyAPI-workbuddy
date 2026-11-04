package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const workBuddyTestSSE = "data: {\"id\":\"c1\",\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"Think\",\"tool_calls\":[]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\",\"tool_calls\":[]},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n" + "data: [DONE]\n\n"

func TestWorkBuddyFingerprintConvergence(t *testing.T) {
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider, stream), func(t *testing.T) {
				type observation struct {
					headers http.Header
					body    []byte
				}
				observed := make(chan observation, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					observed <- observation{r.Header.Clone(), body}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, workBuddyTestSSE)
				}))
				defer upstream.Close()
				cfg := &config.Config{}
				if err := json.Unmarshal([]byte(`{"routing":{"workbuddy-fingerprint-convergence":true}}`), cfg); err != nil {
					t.Fatal(err)
				}
				executor := NewWorkBuddyExecutor(cfg, provider)
				auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "uid-a", EnterpriseID: "team-a", AccessToken: "old-token"})
				auth.Metadata["base_url"] = upstream.URL
				run := func(executor *WorkBuddyExecutor, account *coreauth.Auth, session string, format sdktranslator.Format, payload string) observation {
					t.Helper()
					headers := http.Header{"X-Conversation-Id": {session}, "X-Conversation-Request-Id": {"caller-turn"}, "User-Agent": {session + "-client"}}
					originalHeaders := headers.Clone()
					req := cliproxyexecutor.Request{Model: "m", Payload: []byte(payload)}
					opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: req.Payload, Stream: stream, Headers: headers}
					if stream {
						result, err := executor.ExecuteStream(t.Context(), account, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(t.Context(), account, req, opts); err != nil {
						t.Fatal(err)
					}
					if string(req.Payload) != payload || !reflect.DeepEqual(headers, originalHeaders) {
						t.Fatal("fingerprint convergence mutated the caller request")
					}
					return <-observed
				}
				const chat = `{"messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"caller-cache","user":"caller-user"}`
				first := run(executor, auth, "client-a", sdktranslator.FormatOpenAI, chat)
				second := run(executor, auth, "client-b", sdktranslator.FormatOpenAI, chat)
				identity := first.headers.Get("X-Conversation-ID")
				if len(identity) != 32 || identity == "client-a" || identity != second.headers.Get("X-Conversation-ID") {
					t.Fatalf("account identity changed across clients: %q vs %q", identity, second.headers.Get("X-Conversation-ID"))
				}
				if first.headers.Get("X-Conversation-Request-ID") == second.headers.Get("X-Conversation-Request-ID") || first.headers.Get("X-Conversation-Request-ID") == "caller-turn" {
					t.Fatal("caller turns were not isolated across conversations")
				}
				for _, sample := range []observation{first, second} {
					if sample.headers.Get("User-Agent") != "WorkBuddy/"+workbuddy.UserAgentVersion || sample.headers.Get("X-IDE-Version") != workbuddy.UserAgentVersion {
						t.Fatal("client identity changed the native product headers")
					}
					if sample.headers.Get("X-Request-ID") != sample.headers.Get("X-Conversation-Message-ID") {
						t.Fatal("message and request IDs disagree")
					}
					if gjson.GetBytes(sample.body, "prompt_cache_key").String() != "caller-cache" || gjson.GetBytes(sample.body, "user").String() != "caller-user" {
						t.Fatal("convergence changed cache or user payload fields")
					}
				}
				if first.headers.Get("X-Request-ID") == second.headers.Get("X-Request-ID") {
					t.Fatal("convergence reused a request ID")
				}
				rotated := auth.Clone()
				rotated.ID = "renamed-auth.json"
				rotated.Metadata["access_token"] = "new-token"
				restarted := run(NewWorkBuddyExecutor(cfg, provider), rotated, "client-a", sdktranslator.FormatOpenAI, chat)
				if restarted.headers.Get("X-Conversation-ID") != identity || restarted.headers.Get("X-Conversation-Request-ID") != first.headers.Get("X-Conversation-Request-ID") || restarted.headers.Get("Authorization") != "Bearer new-token" {
					t.Fatal("token rotation, rename, or executor recreation changed the account fingerprint")
				}
				for key, value := range map[string]string{"uid": "uid-b", "enterprise_id": "team-b"} {
					other := auth.Clone()
					other.Metadata[key] = value
					if run(executor, other, "client-a", sdktranslator.FormatOpenAI, chat).headers.Get("X-Conversation-ID") == identity {
						t.Fatalf("%s did not isolate account fingerprints", key)
					}
				}
				otherSite := auth.Clone()
				otherSite.Provider = workbuddy.ProviderAI
				if provider == workbuddy.ProviderAI {
					otherSite.Provider = workbuddy.ProviderCN
				}
				if run(NewWorkBuddyExecutor(cfg, otherSite.Provider), otherSite, "client-a", sdktranslator.FormatOpenAI, chat).headers.Get("X-Conversation-ID") == identity {
					t.Fatal("regional sites share a fingerprint")
				}
				for _, protocol := range []struct {
					format sdktranslator.Format
					body   string
				}{
					{sdktranslator.FormatOpenAIResponse, `{"input":"hi"}`},
					{sdktranslator.FormatClaude, `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
					{sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
				} {
					if run(executor, auth, "other-client", protocol.format, protocol.body).headers.Get("X-Conversation-ID") != identity {
						t.Fatalf("%s changed the account fingerprint", protocol.format)
					}
				}
				if err := json.Unmarshal([]byte(`{"routing":{"workbuddy-fingerprint-convergence":false}}`), cfg); err != nil {
					t.Fatal(err)
				}
				if run(executor, auth, "client-a", sdktranslator.FormatOpenAI, chat).headers.Get("X-Conversation-ID") != identity {
					t.Fatal("caller config mutation changed an in-flight executor snapshot")
				}
				disabled := run(NewWorkBuddyExecutor(cfg, provider), auth, "client-a", sdktranslator.FormatOpenAI, chat)
				if disabled.headers.Get("X-Conversation-ID") != "client-a" || disabled.headers.Get("X-Conversation-Request-ID") != "caller-turn" {
					t.Fatal("disabling convergence did not restore native behavior")
				}
			})
		}
	}
}

func TestWorkBuddyAccountBannedAllResponsePaths(t *testing.T) {
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, payload := range []string{
				`{"code":11140,"msg":"request illegal","requestId":"ban-request","retry_policy":0}`,
				`{"error":{"code":"11140","message":"request illegal"}}`,
				`{"error":{"data":{"code":11140,"msg":"request illegal"}}}`,
			} {
				for _, response := range []struct {
					name, contentType, body string
					status                  int
				}{
					{"json-200", "application/json", payload, http.StatusOK},
					{"json-400", "application/json", payload, http.StatusBadRequest},
					{"json-403", "application/json", payload, http.StatusForbidden},
					{"json-429", "application/json", payload, http.StatusTooManyRequests},
					{"plain-json", "text/plain", payload, http.StatusOK},
					{"sse-data", "text/event-stream", "data: " + payload + "\n\n", http.StatusOK},
					{"sse-error", "text/event-stream", "event: error\ndata: " + payload + "\n\n", http.StatusOK},
					{"sse-final-frame", "text/event-stream", "data: " + payload, http.StatusOK},
				} {
					t.Run(fmt.Sprintf("%s/stream=%t/%s/%s", provider, stream, response.name, payload), func(t *testing.T) {
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", response.contentType)
							w.WriteHeader(response.status)
							_, _ = io.WriteString(w, response.body)
						}))
						defer upstream.Close()
						auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "banned", AccessToken: "test-access"})
						auth.Metadata["base_url"] = upstream.URL
						executor := NewWorkBuddyExecutor(&config.Config{}, provider)
						req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
						var executionErr error
						if stream {
							result, errStream := executor.ExecuteStream(t.Context(), auth, req, opts)
							executionErr = errStream
							if errStream == nil {
								for chunk := range result.Chunks {
									if len(chunk.Payload) != 0 {
										t.Error("account ban was delivered as model content")
									}
									if chunk.Err != nil {
										executionErr = chunk.Err
									}
								}
							}
						} else {
							_, executionErr = executor.Execute(t.Context(), auth, req, opts)
						}
						var upstreamErr statusErr
						if !errors.As(executionErr, &upstreamErr) || upstreamErr.StatusCode() != http.StatusForbidden || !upstreamErr.IsCredentialScoped() || upstreamErr.ResultCode() != "workbuddy_account_banned" {
							t.Fatalf("11140 must become credential-scoped workbuddy_account_banned (403), got %#v", executionErr)
						}
						if upstreamErr.Error() != payload {
							t.Fatalf("upstream error details were lost: %v", upstreamErr)
						}
					})
				}
			}
		}
	}
}

func TestWorkBuddyAccountBannedRequiresErrorCode(t *testing.T) {
	executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN)
	for _, payload := range []string{
		`{"code":11128,"msg":"request illegal"}`,
		`{"code":14003,"msg":"too many requests"}`,
		`{"msg":"request illegal: 11140"}`,
		`{"error":{"data":{"code":11141,"msg":"request illegal"}}}`,
		`{"choices":[{"delta":{"content":"11140 request illegal"}}]}`,
	} {
		err := executor.upstreamStatusError(http.StatusBadRequest, nil, []byte(payload))
		if err.StatusCode() != http.StatusBadRequest || err.IsCredentialScoped() || err.ResultCode() != "" {
			t.Fatalf("unrelated payload was classified as an account ban: %+v", err)
		}
	}
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"11140 request illegal\"}}]}\n\ndata: [DONE]\n\n"
	if err := executor.readStream(t.Context(), strings.NewReader(stream), nil, "m", nil, func([]byte, bool) error { return nil }); err != nil {
		t.Fatalf("model content was classified as an account ban: %v", err)
	}
}

func TestWorkBuddyAccountBannedManagerFailover(t *testing.T) {
	const payload = `{"code":11140,"msg":"request illegal"}`
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, wire := range []string{"json", "sse", "sse-after-content"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", provider, stream, wire), func(t *testing.T) {
					var bannedHits, healthyHits atomic.Int32
					bannedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						bannedHits.Add(1)
						if wire == "json" {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, payload)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						if wire == "sse-after-content" {
							_, _ = io.WriteString(w, "data: {\"id\":\"partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
						}
						_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
					}))
					defer bannedServer.Close()
					healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						healthyHits.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, workBuddyTestSSE)
					}))
					defer healthyServer.Close()
					// A confirmed ban must disable even with optional policies off and cooling ignored.
					cfg := &config.Config{Routing: config.RoutingConfig{DisableCooling: new(true)}}
					selector := coreauth.NewSessionAffinitySelector(&coreauth.FillFirstSelector{})
					t.Cleanup(selector.Stop)
					manager := coreauth.NewManager(nil, selector, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(0, 0, 2)
					manager.RegisterExecutor(NewWorkBuddyExecutor(cfg, provider))
					reg := registry.GetGlobalRegistry()
					for i, server := range []*httptest.Server{bannedServer, healthyServer} {
						auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: fmt.Sprintf("uid-%d", i), AccessToken: "test-access"})
						auth.ID = fmt.Sprintf("%s-%d", t.Name(), i)
						auth.Metadata["base_url"] = server.URL
						reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "m"}, {ID: "sibling"}})
						t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
						if _, err := manager.Register(t.Context(), auth); err != nil {
							t.Fatal(err)
						}
					}
					for i, model := range []string{"m", "sibling", "m"} {
						req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream, Headers: http.Header{"X-Session-Id": {"ban-session"}}}
						var output strings.Builder
						var executionErr error
						if stream {
							result, errStream := manager.ExecuteStream(t.Context(), []string{provider}, req, opts)
							executionErr = errStream
							if errStream == nil {
								for chunk := range result.Chunks {
									output.Write(chunk.Payload)
									if chunk.Err != nil {
										executionErr = chunk.Err
									}
								}
							}
						} else {
							result, errExecute := manager.Execute(t.Context(), []string{provider}, req, opts)
							executionErr = errExecute
							output.Write(result.Payload)
						}
						if stream && wire == "sse-after-content" && i == 0 {
							if executionErr == nil || !strings.Contains(output.String(), "partial") || healthyHits.Load() != 0 {
								t.Fatal("partially delivered stream must fail without replay")
							}
						} else if executionErr != nil || !strings.Contains(output.String(), "Hello") {
							t.Fatalf("request %d did not fail over to the healthy account: %v %s", i, executionErr, output.String())
						}
					}
					banned, _ := manager.GetByID(t.Name() + "-0")
					if !banned.AutoDisabled || banned.Disabled || banned.AutoDisableReason != "account_banned" || banned.LastError == nil || banned.LastError.Code != "workbuddy_account_banned" {
						t.Fatalf("incorrect banned account state: %+v", banned)
					}
					if bannedHits.Load() != 1 {
						t.Fatalf("banned account received %d attempts, want 1", bannedHits.Load())
					}
					healthy, _ := manager.GetByID(t.Name() + "-1")
					if healthy.IsDispatchDisabled() || len(coreauth.CooldownSnapshotForAuth(healthy, time.Now())) != 0 {
						t.Fatal("account ban affected the healthy account")
					}
					bound, status := manager.LookupSessionAffinity(provider, "m", "ban-session")
					if status != "bound" || bound.ID != healthy.ID {
						t.Fatalf("session did not rebind to healthy account: status=%q auth=%+v", status, bound)
					}
				})
			}
		}
	}
}

func TestWorkBuddyQuotaExhaustedAllResponsePaths(t *testing.T) {
	const message = "\u989d\u5ea6\u5df2\u7528\u5c3d\uff0c\u8bf7\u8bbf\u95ee\u4ee5\u4e0b\u94fe\u63a5\uff0c\u8d2d\u4e70\u52a0\u91cf\u5305\u4ee5\u83b7\u53d6\u66f4\u591a\u989d\u5ea6\uff1ahttps://www.codebuddy.cn/profile/usage"
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, envelope := range []string{"root", "error", "error-data"} {
				payload := map[string]any{"code": 14018, "msg": message}
				switch envelope {
				case "error":
					payload = map[string]any{"error": payload}
				case "error-data":
					payload = map[string]any{"error": map[string]any{"data": payload}}
				}
				compact, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				for _, response := range []struct {
					name, contentType, body string
					status                  int
				}{
					{"json-200", "application/json", string(compact), http.StatusOK},
					{"json-400", "application/json", string(compact), http.StatusBadRequest},
					{"json-403", "application/json", string(compact), http.StatusForbidden},
					{"json-429", "application/json", string(compact), http.StatusTooManyRequests},
					{"plain-json", "text/plain", string(compact), http.StatusOK},
					{"sse-data", "text/event-stream", "data: " + string(compact) + "\n\n", http.StatusOK},
					{"sse-error", "text/event-stream", "event: error\ndata: " + string(compact) + "\n\n", http.StatusOK},
					{"sse-final-frame", "text/event-stream", "data: " + string(compact), http.StatusOK},
				} {
					t.Run(fmt.Sprintf("%s/stream=%t/%s/%s", provider, stream, envelope, response.name), func(t *testing.T) {
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", response.contentType)
							w.WriteHeader(response.status)
							_, _ = io.WriteString(w, response.body)
						}))
						defer upstream.Close()
						auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "exhausted", AccessToken: "test-access"})
						auth.Metadata["base_url"] = upstream.URL
						executor := NewWorkBuddyExecutor(&config.Config{}, provider)
						req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
						var executionErr error
						if stream {
							result, errStream := executor.ExecuteStream(t.Context(), auth, req, opts)
							executionErr = errStream
							if errStream == nil {
								for chunk := range result.Chunks {
									if len(chunk.Payload) != 0 {
										t.Error("quota error was delivered as model content")
									}
									if chunk.Err != nil {
										executionErr = chunk.Err
									}
								}
							}
						} else {
							_, executionErr = executor.Execute(t.Context(), auth, req, opts)
						}
						var upstreamErr statusErr
						if !errors.As(executionErr, &upstreamErr) || upstreamErr.StatusCode() != http.StatusTooManyRequests || !upstreamErr.IsCredentialScoped() || upstreamErr.ResultCode() != "usage_limit_reached" {
							t.Fatalf("14018 must become credential-scoped usage_limit_reached (429), got %#v", executionErr)
						}
						if !strings.Contains(upstreamErr.Error(), message) || !strings.Contains(upstreamErr.Error(), "14018") {
							t.Fatalf("upstream error details were lost: %v", upstreamErr)
						}
					})
				}
			}
		}
	}
}

func TestWorkBuddyQuotaExhaustedRequiresErrorCode(t *testing.T) {
	executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN)
	for _, payload := range []string{
		`{"error":{"data":{"code":14019,"msg":"unrelated error"}}}`,
		`{"error":{"data":{"msg":"error 14018: quota exhausted"}}}`,
		`{"choices":[{"delta":{"content":"14018"}}]}`,
	} {
		err := executor.upstreamStatusError(http.StatusBadRequest, nil, []byte(payload))
		if err.StatusCode() != http.StatusBadRequest || err.IsCredentialScoped() || err.ResultCode() != "" {
			t.Fatalf("unrelated payload was classified as quota exhaustion: %+v", err)
		}
	}
}

func TestWorkBuddyQuotaExhaustedManagerFailover(t *testing.T) {
	const payload = `{"error":{"data":{"code":14018,"msg":"\u989d\u5ea6\u5df2\u7528\u5c3d"}}}`
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, wire := range []string{"json", "sse", "sse-after-content"} {
				for _, sticky := range []bool{false, true} {
					for _, autoDisable := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/stream=%t/%s/sticky=%t/auto-disable=%t", provider, stream, wire, sticky, autoDisable), func(t *testing.T) {
							var exhaustedHits, healthyHits atomic.Int32
							exhaustedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								exhaustedHits.Add(1)
								if wire == "json" {
									w.Header().Set("Content-Type", "application/json")
									_, _ = io.WriteString(w, payload)
									return
								}
								w.Header().Set("Content-Type", "text/event-stream")
								if wire == "sse-after-content" {
									_, _ = io.WriteString(w, "data: {\"id\":\"partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
								}
								_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
							}))
							defer exhaustedServer.Close()
							healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								healthyHits.Add(1)
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, workBuddyTestSSE)
							}))
							defer healthyServer.Close()
							// Auto-disable must still gate an exhausted account when cooling is ignored.
							ignoreCooling := autoDisable
							cfg := &config.Config{Routing: config.RoutingConfig{DisableCooling: &ignoreCooling, AutoDisableQuota: autoDisable}}
							var selector coreauth.Selector = &coreauth.FillFirstSelector{}
							if sticky {
								affinity := coreauth.NewSessionAffinitySelector(selector)
								t.Cleanup(affinity.Stop)
								selector = affinity
							}
							manager := coreauth.NewManager(nil, selector, nil)
							manager.SetConfig(cfg)
							manager.SetRetryConfig(0, 0, 2)
							manager.RegisterExecutor(NewWorkBuddyExecutor(cfg, provider))
							reg := registry.GetGlobalRegistry()
							for i, server := range []*httptest.Server{exhaustedServer, healthyServer} {
								auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: fmt.Sprintf("uid-%d", i), AccessToken: "test-access"})
								auth.ID = fmt.Sprintf("%s-%d", t.Name(), i)
								auth.Metadata["base_url"] = server.URL
								reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "m"}, {ID: "sibling"}})
								t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
								if _, err := manager.Register(t.Context(), auth); err != nil {
									t.Fatal(err)
								}
							}
							for i, model := range []string{"m", "sibling", "m"} {
								req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
								opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream, Headers: http.Header{"X-Session-Id": {"quota-session"}}}
								var output strings.Builder
								var executionErr error
								if stream {
									result, errStream := manager.ExecuteStream(t.Context(), []string{provider}, req, opts)
									executionErr = errStream
									if errStream == nil {
										for chunk := range result.Chunks {
											output.Write(chunk.Payload)
											if chunk.Err != nil {
												executionErr = chunk.Err
											}
										}
									}
								} else {
									result, errExecute := manager.Execute(t.Context(), []string{provider}, req, opts)
									executionErr = errExecute
									output.Write(result.Payload)
								}
								if stream && wire == "sse-after-content" && i == 0 {
									if executionErr == nil || !strings.Contains(output.String(), "partial") || healthyHits.Load() != 0 {
										t.Fatal("partially delivered stream must fail without replay")
									}
								} else if executionErr != nil || !strings.Contains(output.String(), "Hello") {
									t.Fatalf("request %d did not fail over to the healthy account: %v %s", i, executionErr, output.String())
								}
							}
							exhausted, ok := manager.GetByID(t.Name() + "-0")
							if !ok || exhausted.AutoDisabled != autoDisable || exhausted.Disabled || exhausted.LastError == nil || exhausted.LastError.Code != "usage_limit_reached" {
								t.Fatalf("incorrect exhausted account state: %+v", exhausted)
							}
							if autoDisable && exhausted.AutoDisableReason != coreauth.AutoDisableReasonQuotaExhausted {
								t.Fatalf("auto-disable reason = %q", exhausted.AutoDisableReason)
							}
							if exhaustedHits.Load() != 1 {
								t.Fatalf("exhausted account received %d attempts, want 1", exhaustedHits.Load())
							}
							healthy, _ := manager.GetByID(t.Name() + "-1")
							if healthy.AutoDisabled || len(coreauth.CooldownSnapshotForAuth(healthy, time.Now())) != 0 {
								t.Fatal("quota exhaustion affected the healthy account")
							}
							if sticky {
								bound, status := manager.LookupSessionAffinity(provider, "m", "quota-session")
								if status != "bound" || bound.ID != healthy.ID {
									t.Fatalf("session did not rebind to healthy account: status=%q auth=%+v", status, bound)
								}
							}
						})
					}
				}
			}
		}
	}
}

func TestWorkBuddyRateLimitAllResponsePaths(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Truncate(time.Second)
	message := "\u60a8\u7684\u4f7f\u7528\u91cf\u5df2\u8d85\u51fa\u9891\u7387\u9650\u5236\uff0c\u5c06\u5728 " + resetAt.In(time.FixedZone("UTC+8", 8*60*60)).Format("2006-01-02 15:04:05") + " UTC+8 \u91cd\u7f6e"
	payload, err := json.MarshalIndent(map[string]any{"code": 6004, "msg": message}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(map[string]any{"code": 6004, "msg": message})
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, response := range []struct {
				name, contentType, body string
				status                  int
			}{
				{"json-200", "application/json", string(payload), http.StatusOK},
				{"json-400", "application/json", string(payload), http.StatusBadRequest},
				{"json-429", "application/json", string(payload), http.StatusTooManyRequests},
				{"plain-json", "text/plain", string(compact), http.StatusOK},
				{"sse-data", "text/event-stream", "data: " + string(compact) + "\n\n", http.StatusOK},
				{"sse-error", "text/event-stream", "event: error\ndata: " + string(compact) + "\n\n", http.StatusOK},
				{"sse-final-frame", "text/event-stream", "data: " + string(compact), http.StatusOK},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", provider, stream, response.name), func(t *testing.T) {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", response.contentType)
						w.Header().Set("Retry-After", "60")
						w.WriteHeader(response.status)
						_, _ = io.WriteString(w, response.body)
					}))
					defer upstream.Close()
					auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "rate-limited", AccessToken: "access"})
					auth.Metadata["base_url"] = upstream.URL
					executor := NewWorkBuddyExecutor(&config.Config{}, provider)
					req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
					before := time.Now()
					var executionErr error
					if stream {
						result, errStream := executor.ExecuteStream(t.Context(), auth, req, opts)
						executionErr = errStream
						if errStream == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									executionErr = chunk.Err
								}
							}
						}
					} else {
						_, executionErr = executor.Execute(t.Context(), auth, req, opts)
					}
					var limit interface {
						error
						StatusCode() int
						RetryAfter() *time.Duration
						IsCredentialScoped() bool
					}
					if !errors.As(executionErr, &limit) || limit.StatusCode() != http.StatusTooManyRequests || !limit.IsCredentialScoped() {
						t.Fatalf("6004 must become a credential-scoped 429, got %v", executionErr)
					}
					retry := limit.RetryAfter()
					if retry == nil || before.Add(*retry).After(resetAt) || time.Now().Add(*retry).Before(resetAt) {
						t.Fatalf("retry delay %v does not target reset %s", retry, resetAt)
					}
					if !strings.Contains(executionErr.Error(), message) {
						t.Fatal("upstream reset message was lost")
					}
				})
			}
		}
	}
}

func TestWorkBuddyRateLimitMissingResetAndOrdinaryErrors(t *testing.T) {
	executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN)
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests} {
		err := executor.upstreamStatusError(status, http.Header{"Retry-After": {"90"}}, []byte(`{"code":6004,"msg":"rate limited"}`))
		if err.StatusCode() != http.StatusTooManyRequests || !err.IsCredentialScoped() || err.RetryAfter() == nil || *err.RetryAfter() != 90*time.Second {
			t.Fatalf("HTTP %d lost the header-based cooldown: %+v", status, err)
		}
	}
	if err := executor.upstreamStatusError(http.StatusOK, nil, []byte(`{"code":6004,"msg":"rate limited"}`)); !err.IsCredentialScoped() || err.RetryAfter() != nil {
		t.Fatal("missing reset must use the manager's quota backoff")
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests} {
		err := executor.upstreamStatusError(status, nil, []byte(`{"code":6005,"msg":"unrelated error"}`))
		if err.StatusCode() != status || err.IsCredentialScoped() || err.ResultCode() != "" {
			t.Fatalf("unrelated error was reclassified: %+v", err)
		}
	}
	for _, frame := range []string{"data: ", "event: error\ndata: "} {
		err := executor.readStream(t.Context(), strings.NewReader(frame+`{"code":6004,"msg":"rate limited"}`+"\n\n"), http.Header{"Retry-After": {"90"}}, "m", nil, func([]byte, bool) error {
			t.Fatal("rate-limit payload was delivered as model content")
			return nil
		})
		var upstream statusErr
		if !errors.As(err, &upstream) || upstream.RetryAfter() == nil || *upstream.RetryAfter() != 90*time.Second {
			t.Fatalf("SSE lost Retry-After: %v", err)
		}
	}
}

func TestWorkBuddyModelRateLimitAllResponsePaths(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Truncate(time.Second)
	message := "model usage limit reached; reset " + resetAt.In(time.FixedZone("UTC+8", 8*60*60)).Format("2006-01-02 15:04:05") + " UTC+8"
	rootPayload, err := json.Marshal(map[string]any{"code": 6000, "msg": message})
	if err != nil {
		t.Fatal(err)
	}
	nestedPayload, err := json.Marshal(map[string]any{"error": map[string]any{"data": map[string]any{"code": "6000", "message": message}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, response := range []struct {
				name, contentType, body string
				status                  int
			}{
				{"json-200", "application/json", string(rootPayload), http.StatusOK},
				{"json-nested-429", "application/json", string(nestedPayload), http.StatusTooManyRequests},
				{"sse-data", "text/event-stream", "data: " + string(rootPayload) + "\n\n", http.StatusOK},
				{"sse-error", "text/event-stream", "event: error\ndata: " + string(nestedPayload) + "\n\n", http.StatusOK},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", provider, stream, response.name), func(t *testing.T) {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", response.contentType)
						w.Header().Set("Retry-After", "120")
						w.WriteHeader(response.status)
						_, _ = io.WriteString(w, response.body)
					}))
					defer upstream.Close()
					auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "model-limited", AccessToken: "access"})
					auth.Metadata["base_url"] = upstream.URL
					executor := NewWorkBuddyExecutor(&config.Config{}, provider)
					req := cliproxyexecutor.Request{Model: "model-a", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
					var executionErr error
					if stream {
						result, errStream := executor.ExecuteStream(t.Context(), auth, req, opts)
						executionErr = errStream
						if errStream == nil {
							for chunk := range result.Chunks {
								if len(chunk.Payload) != 0 {
									t.Error("model limit was delivered as model content")
								}
								if chunk.Err != nil {
									executionErr = chunk.Err
								}
							}
						}
					} else {
						_, executionErr = executor.Execute(t.Context(), auth, req, opts)
					}
					var limit interface {
						error
						StatusCode() int
						RetryAfter() *time.Duration
						IsCredentialScoped() bool
						ResultCode() string
					}
					if !errors.As(executionErr, &limit) || limit.StatusCode() != http.StatusTooManyRequests || limit.IsCredentialScoped() || limit.ResultCode() != "workbuddy_model_rate_limit" {
						t.Fatalf("6000 must become a model-scoped 429, got %v", executionErr)
					}
					retry := limit.RetryAfter()
					if retry == nil || time.Now().Add(*retry).Before(resetAt) || time.Now().Add(*retry).After(resetAt.Add(time.Second)) {
						t.Fatalf("retry delay %v does not target model reset %s", retry, resetAt)
					}
				})
			}
		}
	}
}

func TestWorkBuddyRateLimitManagerFailover(t *testing.T) {
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, wire := range []string{"json", "sse", "sse-after-content"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", provider, stream, wire), func(t *testing.T) {
					resetAt := time.Now().Add(time.Hour).Truncate(time.Second)
					message := "usage limit reached; reset " + resetAt.In(time.FixedZone("UTC+8", 8*60*60)).Format("2006-01-02 15:04:05") + " UTC+8"
					payload, err := json.Marshal(map[string]any{"code": 6004, "msg": message})
					if err != nil {
						t.Fatal(err)
					}
					var limitedHits, healthyHits atomic.Int32
					limitedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						limitedHits.Add(1)
						if wire == "json" {
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write(payload)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						if wire == "sse-after-content" {
							_, _ = io.WriteString(w, "data: {\"id\":\"partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
						}
						_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
					}))
					defer limitedServer.Close()
					healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						healthyHits.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, workBuddyTestSSE)
					}))
					defer healthyServer.Close()
					ignoreCooling := true
					cfg := &config.Config{Routing: config.RoutingConfig{DisableCooling: &ignoreCooling, AutoDisableQuota: true}}
					manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(0, 0, 2)
					manager.RegisterExecutor(NewWorkBuddyExecutor(cfg, provider))
					reg := registry.GetGlobalRegistry()
					for i, server := range []*httptest.Server{limitedServer, healthyServer} {
						auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: fmt.Sprintf("uid-%d", i), AccessToken: "test-access"})
						auth.ID = fmt.Sprintf("%s-%d", t.Name(), i)
						auth.Metadata["base_url"] = server.URL
						reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "m"}, {ID: "sibling"}})
						t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
						if _, err := manager.Register(t.Context(), auth); err != nil {
							t.Fatal(err)
						}
					}
					for i, model := range []string{"m", "sibling", "m"} {
						req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
						var output strings.Builder
						var executionErr error
						if stream {
							result, errStream := manager.ExecuteStream(t.Context(), []string{provider}, req, opts)
							executionErr = errStream
							if errStream == nil {
								for chunk := range result.Chunks {
									output.Write(chunk.Payload)
									if chunk.Err != nil {
										executionErr = chunk.Err
									}
								}
							}
						} else {
							result, errExecute := manager.Execute(t.Context(), []string{provider}, req, opts)
							executionErr = errExecute
							output.Write(result.Payload)
						}
						if stream && wire == "sse-after-content" && i == 0 {
							if executionErr == nil || !strings.Contains(output.String(), "partial") || healthyHits.Load() != 0 {
								t.Fatal("a partially delivered stream must fail without replaying on another account")
							}
						} else if executionErr != nil || !strings.Contains(output.String(), "Hello") {
							t.Fatalf("request %d did not use the healthy account: %v %s", i, executionErr, output.String())
						}
					}
					if limitedHits.Load() != 1 {
						t.Fatalf("cooled account received %d attempts, want 1", limitedHits.Load())
					}
					limited, _ := manager.GetByID(t.Name() + "-0")
					healthy, _ := manager.GetByID(t.Name() + "-1")
					if limited.AutoDisabled || limited.Disabled || limited.LastError.Code != coreauth.ErrorCodeWorkBuddyRateLimit || limited.Quota.Reason != "credential_quota" {
						t.Fatal("6004 did not remain a temporary account cooldown")
					}
					if limited.Quota.NextRecoverAt.Before(resetAt) || limited.Quota.NextRecoverAt.After(resetAt.Add(time.Second)) {
						t.Fatalf("cooldown reset = %s, want %s", limited.Quota.NextRecoverAt, resetAt)
					}
					if len(coreauth.CooldownSnapshotForAuth(healthy, time.Now())) != 0 {
						t.Fatal("rate limit affected the other account")
					}
				})
			}
		}
	}
}

func TestWorkBuddyBothSitesStreamingAndNonStreaming(t *testing.T) {
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider, stream), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/v2/chat/completions" || !gjson.GetBytes(body, "stream").Bool() {
						t.Errorf("upstream must always stream: %s %s", r.URL.Path, body)
					}
					for key, value := range map[string]string{"Authorization": "Bearer access", "X-User-Id": "uid", "X-Domain": "account.example", "User-Agent": "WorkBuddy/" + workbuddy.UserAgentVersion, "X-IDE-Type": "WorkBuddy", "X-Enterprise-Id": "team", "X-Tenant-Id": "team"} {
						if r.Header.Get(key) != value {
							t.Errorf("%s=%q", key, r.Header.Get(key))
						}
					}
					if (r.Header.Get("X-Agent-Type") == "main") != (provider == workbuddy.ProviderAI) {
						t.Error("site-specific headers were mixed")
					}
					if gjson.GetBytes(body, "messages.0.role").String() != "system" || gjson.GetBytes(body, "max_tokens").Int() != 64 || gjson.GetBytes(body, "max_completion_tokens").Exists() {
						t.Errorf("incorrect WorkBuddy request normalization: %s", body)
					}
					if got := gjson.GetBytes(body, "messages.0.content").String(); got != "Be helpful.\n  - Users can report issues and share feedback at https://github.com/anthropics/claude-code/issues" {
						t.Errorf("Claude feedback notice was not normalized: %q", got)
					}
					if stop := gjson.GetBytes(body, "stop"); !stop.IsArray() || len(stop.Array()) != 1 || stop.Array()[0].String() != "</block>" {
						t.Errorf("stop string was not normalized to a single-element array: %s", stop.Raw)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, workBuddyTestSSE)
				}))
				defer upstream.Close()
				auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{AccessToken: "access", RefreshToken: "refresh", UID: "uid", Domain: "account.example", EnterpriseID: "team"})
				auth.Metadata["base_url"] = upstream.URL
				executor := NewWorkBuddyExecutor(&config.Config{}, provider)
				req := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{"model":"test-model","messages":[{"role":"developer","content":"x-anthropic-billing-header: cc_version=2.1.306.c1c; cc_entrypoint=claude-vscode;Be helpful.\n  - To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},{"role":"user","content":"Hi"}],"max_completion_tokens":64,"stop":"</block>"}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream}
				if stream {
					result, err := executor.ExecuteStream(t.Context(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					var text strings.Builder
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						text.Write(chunk.Payload)
					}
					if !strings.Contains(text.String(), "Hello") || !strings.Contains(text.String(), "Think") || !strings.Contains(text.String(), `"finish_reason":"stop"`) || strings.Contains(text.String(), `"tool_calls":[]`) {
						t.Fatalf("incorrect stream %s", text.String())
					}
				} else {
					response, err := executor.Execute(t.Context(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					if gjson.GetBytes(response.Payload, "choices.0.message.content").String() != "Hello" || gjson.GetBytes(response.Payload, "choices.0.message.reasoning_content").String() != "Think" || gjson.GetBytes(response.Payload, "usage.total_tokens").Int() != 15 {
						t.Fatalf("incorrect aggregate %s", response.Payload)
					}
				}
			})
		}
	}
}

func TestWorkBuddyToolNamesAndIncompleteStreams(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprint(incomplete), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				tool := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
				if tool["name"] != "mcp_search" || body["tool_choice"] != "mcp_search" || tool["parameters"].(map[string]any)["type"] != "object" {
					t.Errorf("tool was not normalized: %v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"mcp_search\",\"arguments\":\"{\"}}]}}]}\n\n")
				if !incomplete {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			auth := &coreauth.Auth{Provider: workbuddy.ProviderCN, Metadata: map[string]any{"access_token": "access", "uid": "uid", "base_url": upstream.URL}}
			req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"Search"}],"tools":[{"type":"function","function":{"name":"mcp.search","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"mcp.search"}}}`)}
			response, err := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN).Execute(t.Context(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
			if incomplete {
				if err == nil {
					t.Fatal("incomplete stream was reported as successful")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(response.Payload, "choices.0.message.tool_calls.0.function.name").String() != "mcp.search" || gjson.GetBytes(response.Payload, "choices.0.message.tool_calls.0.function.arguments").String() != "{}" {
				t.Fatalf("tool call did not round trip: %s", response.Payload)
			}
		})
	}
}

func TestWorkBuddyClientProtocolConversions(t *testing.T) {
	for _, test := range []struct {
		format sdktranslator.Format
		body   string
		marker string
	}{
		{sdktranslator.FormatClaude, `{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`, "message_stop"},
		{sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi"}`, "response.completed"},
		{sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, "STOP"},
	} {
		t.Run(test.format.String(), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if gjson.GetBytes(body, "messages.1.content").String() != "hi" || !gjson.GetBytes(body, "stream").Bool() {
					t.Errorf("client request did not become a WorkBuddy chat: %s", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, workBuddyTestSSE)
			}))
			defer upstream.Close()
			auth := workbuddy.NewAuthRecord(workbuddy.ProviderAI, workbuddy.Credentials{UID: "uid", AccessToken: "access"})
			auth.Metadata["base_url"] = upstream.URL
			executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderAI)
			req := cliproxyexecutor.Request{Model: "m", Payload: []byte(test.body)}
			opts := cliproxyexecutor.Options{SourceFormat: test.format, OriginalRequest: req.Payload}
			result, err := executor.Execute(t.Context(), auth, req, opts)
			if err != nil || !strings.Contains(string(result.Payload), "Hello") {
				t.Fatalf("non-stream conversion failed: %v %s", err, result.Payload)
			}
			opts.Stream = true
			stream, err := executor.ExecuteStream(t.Context(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if !strings.Contains(output.String(), "Hello") || !strings.Contains(output.String(), test.marker) {
				t.Fatalf("stream conversion failed: %s", output.String())
			}
		})
	}
}
