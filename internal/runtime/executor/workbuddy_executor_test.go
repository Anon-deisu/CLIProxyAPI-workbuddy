package executor

import (
	"bytes"
	"context"
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

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const workBuddyTestSSE = "data: {\"id\":\"c1\",\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"Think\",\"tool_calls\":[]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\",\"tool_calls\":[]},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n" + "data: [DONE]\n\n"

type workBuddyTestTransport func(*http.Request) (*http.Response, error)

func (f workBuddyTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestWorkBuddyDuplicateKeysExecutorCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, finish := range []string{"length", "stop"} {
			for _, rules := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/finish=%s/rules=%t", stream, finish, rules), func(t *testing.T) {
					cfg := &config.Config{}
					if rules {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "m", Match: []map[string]any{{"stream": true, "max_tokens": 64, "messages.0.role": "system", "tools.0.function.name": "mcp_lookup"}}}}, Params: map[string]any{"stream": false, "audit.-1": "once", "response_format": "configured"}}}
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "m"}}, Params: []string{"unknown"}}}
					}
					firstFinish := "stop"
					if finish == "stop" {
						firstFinish = "length"
					}
					upstreamFrame := fmt.Sprintf(`{"id":"duplicate","choices":[{"index":0,"delta":{"content":"old","content":"Hello","tool_calls":[{"index":0,"id":"c","function":{"name":"wrong","na\u006de":"mcp_lookup","arguments":"{}"}}]},"finish_reason":"%s","finish_\u0072eason":"%s"}],"usage":{"total_tokens":1,"total_tokens":15}}`, firstFinish, finish)
					var hits, finalized int
					ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", workBuddyTestTransport(func(r *http.Request) (*http.Response, error) {
						hits++
						body, err := io.ReadAll(r.Body)
						if err != nil {
							return nil, err
						}
						var decoded map[string]json.RawMessage
						if err := json.Unmarshal(body, &decoded); err != nil {
							t.Fatal(err)
						}
						wantStream := "true"
						if rules {
							wantStream = "false"
						}
						if string(decoded["stream"]) != wantStream || ojson.GetBytes(body, "stream").Raw != wantStream || ojson.GetBytes(body, "max_tokens").Int() != 64 || ojson.GetBytes(body, "max_completion_tokens").Exists() ||
							ojson.GetBytes(body, "messages.0.role").String() != "system" || ojson.GetBytes(body, "tools.0.function.name").String() != "mcp_lookup" ||
							ojson.GetBytes(body, "tool_choice").String() != "mcp_lookup" {
							t.Errorf("upstream saw wrong duplicate resolution: %s", body)
						}
						counts := make(map[string]int)
						ojson.GetBytesNoCopy(body, "@this").ForEach(func(key, _ ojson.Result) bool { counts[key.String()]++; return true })
						if counts["stream"] != 1 || finalized != hits {
							t.Errorf("stream keys=%d finalized=%d hits=%d", counts["stream"], finalized, hits)
						}
						if rules {
							if ojson.GetBytes(body, "audit.#").Int() != 1 || ojson.GetBytes(body, "unknown").Exists() {
								t.Errorf("final rules were overwritten/repeated: %s", body)
							}
						} else if ojson.GetBytes(body, "unknown.n").Raw != "9007199254740993" {
							t.Errorf("unknown duplicate lost last integer: %s", body)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + upstreamFrame + "\n\ndata: [DONE]\n\n"))}, nil
					}))
					auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: "duplicates", AccessToken: "test"})
					req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"stream":false,"str\u0065am":false,"max_completion_tokens":7,"max_completion_tokens":64,"messages":[{"role":"user","role":"developer","content":"hi"}],"tools":[{"function":{"name":"mcp.old","na\u006de":"mcp.lookup","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"mcp.lookup"}},"unknown":{"n":0,"n":9007199254740993}}`)}
					original := bytes.Clone(req.Payload)
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload, Stream: stream, Metadata: map[string]any{cliproxyexecutor.FinalizedResponseFormatCallbackMetadataKey: func(string) { finalized++ }}}
					executor := NewWorkBuddyExecutor(cfg, auth.Provider)
					if stream {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						finishes, contents := 0, 0
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
								frame := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
								if len(frame) == 0 || bytes.Equal(frame, []byte("[DONE]")) {
									continue
								}
								if reason := ojson.GetBytes(frame, "choices.0.finish_reason"); reason.Exists() {
									finishes++
									if reason.String() != finish || ojson.GetBytes(frame, "choices.0.delta").Raw != "{}" {
										t.Errorf("wrong terminal: %s", frame)
									}
								} else if ojson.GetBytes(frame, "choices.0.delta.content").String() == "Hello" {
									contents++
									if ojson.GetBytes(frame, "choices.0.delta.tool_calls.0.function.name").String() != "mcp.lookup" {
										t.Errorf("tool identity did not roundtrip: %s", frame)
									}
								}
							}
						}
						if finishes != 1 || contents != 1 {
							t.Errorf("finish frames=%d content frames=%d", finishes, contents)
						}
					} else {
						response, err := executor.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						if ojson.GetBytes(response.Payload, "choices.0.finish_reason").String() != finish || ojson.GetBytes(response.Payload, "choices.0.message.content").String() != "Hello" || ojson.GetBytes(response.Payload, "choices.0.message.tool_calls.0.function.name").String() != "mcp.lookup" {
							t.Errorf("aggregate changed duplicate semantics: %s", response.Payload)
						}
					}
					if !bytes.Equal(original, req.Payload) || hits != 1 || finalized != 1 {
						t.Fatal("input isolation or final rule count changed")
					}
				})
			}
		}
	}
}

type workBuddyPreparationCounter struct {
	model string
	calls atomic.Int64
}

func (h *workBuddyPreparationCounter) Levels() []log.Level { return []log.Level{log.DebugLevel} }
func (h *workBuddyPreparationCounter) Fire(entry *log.Entry) error {
	if entry.Message == "thinking: processed config to apply |" && entry.Data["model"] == h.model {
		h.calls.Add(1)
	}
	return nil
}

type workBuddyHolderProbe struct {
	*WorkBuddyExecutor
	check func(context.Context)
}

func (e *workBuddyHolderProbe) Execute(ctx context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.check(ctx)
	return cliproxyexecutor.Response{Payload: []byte(`{"choices":[]}`)}, nil
}
func (e *workBuddyHolderProbe) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.check(ctx)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}
func (e *workBuddyHolderProbe) CountTokens(ctx context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.check(ctx)
	return cliproxyexecutor.Response{Payload: []byte(`{"total_tokens":1}`)}, nil
}

func TestWorkBuddyManagerPreparationHolderBoundaries(t *testing.T) {
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	parent := cliproxyexecutor.RequestPreparationCacheFromContext(ctx)
	parent.Store("probe", "parent")
	seen := make(map[*cliproxyexecutor.RequestPreparationCache]bool)
	probe := &workBuddyHolderProbe{WorkBuddyExecutor: NewWorkBuddyExecutor(cfg, workbuddy.ProviderCN), check: func(ctx context.Context) {
		holder := cliproxyexecutor.RequestPreparationCacheFromContext(ctx)
		if holder == nil || holder == parent || seen[holder] {
			t.Error("Manager invocation did not create an independent holder")
		}
		if _, ok := holder.Load("probe"); ok {
			t.Error("new invocation inherited preparation")
		}
		holder.Store("probe", "attempt")
		seen[holder] = true
	}}
	manager.RegisterExecutor(probe)
	auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: "probe", AccessToken: "probe"})
	auth.ID = t.Name()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "probe"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	req := cliproxyexecutor.Request{Model: "probe", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	for range 2 {
		if _, err := manager.Execute(ctx, []string{auth.Provider}, req, opts); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.ExecuteCount(ctx, []string{auth.Provider}, req, opts); err != nil {
			t.Fatal(err)
		}
		result, err := manager.ExecuteStream(ctx, []string{auth.Provider}, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
	}
	if len(seen) != 6 {
		t.Fatalf("Manager invocation count = %d", len(seen))
	}
	if value, _ := parent.Load("probe"); value != "parent" {
		t.Fatal("Manager mutated parent holder")
	}
}

func TestWorkBuddyManagerFailoverReusesPreparation(t *testing.T) {
	const model = "workbuddy-retry-preparation"
	from := sdktranslator.Format("workbuddy-manager-retry-translation")
	var translations atomic.Int64
	sdktranslator.Register(from, sdktranslator.FormatOpenAI, func(_ string, body []byte, _ bool) ([]byte, error) {
		translations.Add(1)
		return bytes.Clone(body), nil
	}, sdktranslator.ResponseTransform{})
	t.Cleanup(func() { sdktranslator.Unregister(from, sdktranslator.FormatOpenAI) })
	hook := &workBuddyPreparationCounter{model: model}
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldLevel, oldOutput := logger.GetLevel(), logger.Out
	logger.AddHook(hook)
	logger.SetLevel(log.DebugLevel)
	logger.SetOutput(io.Discard)
	t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel); logger.SetOutput(oldOutput) })
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			hook.calls.Store(0)
			translations.Store(0)
			cfg := &config.Config{Routing: config.RoutingConfig{DisableCooling: new(true), WorkBuddyFingerprintConvergence: new(true)}, Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: model}}, Params: map[string]any{"audit.-1": "once", "response_format": "configured"},
			}}}}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.SetRetryConfig(0, 0, 3)
			manager.RegisterExecutor(NewWorkBuddyExecutor(cfg, workbuddy.ProviderCN))
			reg := registry.GetGlobalRegistry()
			for i := range 3 {
				uid := fmt.Sprintf("uid-%d", i)
				auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: uid, AccessToken: "token-" + uid, EnterpriseID: "team-" + uid})
				auth.ID = fmt.Sprintf("%s-%d", t.Name(), i)
				reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model, Type: auth.Provider, Thinking: &registry.ThinkingSupport{Levels: []string{"high", "max"}}}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
			}
			var hits, finalized atomic.Int64
			seenIDs, seenAccounts := make(map[string]bool), make(map[string]bool)
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", workBuddyTestTransport(func(r *http.Request) (*http.Response, error) {
				n := hits.Add(1)
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					return nil, errRead
				}
				uid := r.Header.Get("X-User-ID")
				if r.Header.Get("Authorization") != "Bearer token-"+uid || r.Header.Get("X-Enterprise-ID") != "team-"+uid {
					t.Error("attempt reused another account's signing headers")
				}
				if id := r.Header.Get("X-Request-ID"); id == "" || seenIDs[id] {
					t.Error("attempt reused request identity")
				} else {
					seenIDs[id] = true
				}
				seenAccounts[uid] = true
				if ojson.GetBytes(body, "audit.#").Int() != 1 || finalized.Load() != n || ojson.GetBytes(body, "reasoning_effort").String() != "high" ||
					ojson.GetBytes(body, "messages.0.role").String() != "system" || ojson.GetBytes(body, "tools.0.function.name").String() != "mcp_lookup" {
					t.Errorf("attempt %d skipped preparation or final rules: %s, callbacks=%d", n, body, finalized.Load())
				}
				if n%3 != 0 {
					return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"retry on next account"}}`))}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(workBuddyTestSSE))}, nil
			}))
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"messages":[{"role":"developer","content":"instructions"},{"role":"user","content":"hello"}],"reasoning_effort":"high","tools":[{"function":{"name":"mcp.lookup","parameters":{}}}]}`)}
			original := bytes.Clone(req.Payload)
			opts := cliproxyexecutor.Options{SourceFormat: from, ResponseFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload, Stream: stream, Metadata: map[string]any{
				cliproxyexecutor.FinalizedResponseFormatCallbackMetadataKey: func(format string) {
					finalized.Add(1)
					if format != "configured" {
						t.Errorf("final format = %q", format)
					}
				},
			}}
			for request := int64(1); request <= 2; request++ {
				if stream {
					result, err := manager.ExecuteStream(ctx, []string{workbuddy.ProviderCN}, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := manager.Execute(ctx, []string{workbuddy.ProviderCN}, req, opts); err != nil {
					t.Fatal(err)
				}
				if hits.Load() != request*3 || finalized.Load() != request*3 || len(seenAccounts) != 3 {
					t.Fatalf("real failover attempts=%d finalized=%d accounts=%v", hits.Load(), finalized.Load(), seenAccounts)
				}
				if got := hook.calls.Load(); got != request {
					t.Errorf("base thinking conversions = %d after %d requests and %d attempts, want %d", got, request, hits.Load(), request)
				}
				if translations.Load() != request*3 {
					t.Errorf("registered translator side effects skipped: calls=%d", translations.Load())
				}
			}
			if !bytes.Equal(req.Payload, original) {
				t.Fatal("retry mutated caller input")
			}
		})
	}
}

func TestWorkBuddyPayloadBarrierAfterNormalization(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, filter := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/filter=%t", stream, filter), func(t *testing.T) {
				models := []config.PayloadModelRule{{Name: "m", Protocol: "openai", Match: []map[string]any{{
					"model": "m", "stream": true, "messages.0.role": "system", "max_tokens": 64,
					"stop.0": "END", "tools.0.function.name": "mcp_lookup", "tool_choice": "mcp_lookup",
				}}}}
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"audit.-1": "once"}}}}}
				cfg.Payload.Override = append(cfg.Payload.Override, config.PayloadRule{Models: []config.PayloadModelRule{{Name: "m"}}, Params: map[string]any{"apply_calls.-1": "once"}})
				paths := []string{"model", "stream", "messages", "tools", "tool_choice", "stop", "max_tokens"}
				if filter {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: paths}}
				} else {
					cfg.Payload.Override = append(cfg.Payload.Override, config.PayloadRule{Models: models, Params: map[string]any{
						"model": "configured", "stream": false, "messages.0.role": "developer", "messages.0.content": "You are an AI agent powered by DeepSeek Harness",
						"stop": "configured-stop", "tool_choice": "none", "tools.0.function.name": "configured.tool", "max_completion_tokens": 99,
					}})
				}
				var requests int
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", workBuddyTestTransport(func(r *http.Request) (*http.Response, error) {
					requests++
					body, errRead := io.ReadAll(r.Body)
					if errRead != nil {
						t.Fatal(errRead)
					}
					if ojson.GetBytes(body, "audit.#").Int() != 1 || ojson.GetBytes(body, "apply_calls.#").Int() != 1 {
						t.Errorf("rules must run once after normalization: %s", body)
					}
					if filter {
						for _, path := range paths {
							if ojson.GetBytes(body, path).Exists() {
								t.Errorf("filtered %s restored: %s", path, body)
							}
						}
					} else {
						for path, want := range map[string]string{
							"model": "configured", "stream": "false", "messages.0.role": "developer", "messages.0.content": "You are an AI agent powered by DeepSeek Harness",
							"stop": "configured-stop", "tool_choice": "none", "tools.0.function.name": "configured.tool", "max_completion_tokens": "99",
						} {
							if got := ojson.GetBytes(body, path).String(); got != want {
								t.Errorf("%s = %q, want %q", path, got, want)
							}
						}
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(workBuddyTestSSE))}, nil
				}))
				auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: "barrier", AccessToken: "test"})
				executor := NewWorkBuddyExecutor(cfg, auth.Provider)
				req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"model":"client","messages":[{"role":"developer","content":"hi"}],"max_completion_tokens":64,"stop":"END","tools":[{"function":{"name":"mcp.lookup","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"mcp.lookup"}}}`)}
				before := bytes.Clone(req.Payload)
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload, Stream: stream}
				for range 2 {
					if stream {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				}
				if requests != 2 || !bytes.Equal(before, req.Payload) {
					t.Fatal("attempt state leaked into the caller request")
				}
			})
		}
	}
}

func TestWorkBuddyRequestPairTranslationAndIsolation(t *testing.T) {
	from := sdktranslator.Format("workbuddy-count-translations")
	var calls int
	sdktranslator.Register(from, sdktranslator.FormatOpenAI, func(_ string, body []byte, stream bool) ([]byte, error) {
		calls++
		if !stream {
			t.Error("WorkBuddy translation must request streaming")
		}
		return bytes.Clone(body), nil
	}, sdktranslator.ResponseTransform{})
	t.Cleanup(func() { sdktranslator.Unregister(from, sdktranslator.FormatOpenAI) })
	for _, mode := range []string{"absent", "same-slice", "same-bytes", "different"} {
		t.Run(mode, func(t *testing.T) {
			payload := []byte(`{"messages":[{"role":"user","content":"working"}],"tools":[{"function":{"name":"mcp.lookup"}}]}`)
			original := payload
			wantCalls := 1
			switch mode {
			case "absent":
				original = nil
			case "same-bytes":
				original = bytes.Clone(payload)
			case "different":
				original = []byte(`{"messages":[{"role":"user","content":"original"}],"temperature":0.9}`)
				wantCalls = 2
			}
			before, originalBefore := bytes.Clone(payload), bytes.Clone(original)
			req := cliproxyexecutor.Request{Model: "m", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: from, OriginalRequest: original}
			cfg := &config.Config{Payload: config.PayloadConfig{Default: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "m"}}, Params: map[string]any{"temperature": 0.2}}}}}
			executor := NewWorkBuddyExecutor(cfg, workbuddy.ProviderCN)
			for _, uid := range []string{"first", "second"} {
				calls = 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", workBuddyTestTransport(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("X-User-ID") != uid {
						t.Error("account headers leaked")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}))
				auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: uid, AccessToken: uid})
				reporter := helps.NewExecutorUsageReporter(ctx, executor, "m", auth)
				response, body, names, err := executor.open(ctx, auth, req, opts, reporter)
				if err != nil {
					t.Fatal(err)
				}
				executor.closeResponse(response)
				if calls != wantCalls {
					t.Errorf("translations = %d, want %d", calls, wantCalls)
				}
				if ojson.GetBytes(body, "messages.1.content").String() != "working" || names["mcp_lookup"] != "mcp.lookup" {
					t.Errorf("incorrect prepared payload: %s %v", body, names)
				}
				if mode == "different" && ojson.GetBytes(body, "temperature").Exists() {
					t.Error("default rules lost original request intent")
				}
				body[0] = '!'
				names["mcp_lookup"] = "mutated"
			}
			if !bytes.Equal(payload, before) || !bytes.Equal(original, originalBefore) {
				t.Fatal("preparation mutated caller inputs")
			}
		})
	}
}

func TestWorkBuddyQueueWaitingStreamScope(t *testing.T) {
	executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN)
	for _, prefix := range []string{"data: ", "event: error\ndata: "} {
		payload := `{"code":6020,"msg":"queue waiting","data":{"retry_after":7},"request_id":"keep"}`
		err := executor.readStream(t.Context(), strings.NewReader(prefix+payload+"\n\n"), http.Header{"Retry-After": {"90"}}, "m", nil, func([]byte, bool) error {
			t.Error("queue error delivered as content")
			return nil
		})
		var upstream statusErr
		if !errors.As(err, &upstream) || upstream.StatusCode() != 429 || upstream.IsCredentialScoped() || upstream.ResultCode() != "" || upstream.RetryAfter() == nil || *upstream.RetryAfter() != 7*time.Second || upstream.Error() != payload {
			t.Errorf("queue scope, retry or details lost: %#v", err)
		}
	}
}

func TestWorkBuddyGenericStreamErrorCompatibility(t *testing.T) {
	executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN)
	for _, payload := range []string{
		`{"choices":[{"delta":{"content":"ordinary"}}]}`,
		`{"code":500,"msg":"not a standard message"}`,
		`{"code":500,"message":"generic error","status":404}`,
		`{"error":{"message":"failed","status_code":503},"status":200}`,
		`{"error":false,"status_code":"429"}`,
		`{"error":null}`, `{"type":"ReSpOnSe.FaIlEd","status":401}`,
		`{"response":{"error":{"message":"failed","status":400}}}`,
		`{"code":6020,"msg":"queue","data":{"retry_after":7}}`,
		`{"code":6020,"msg":"queue without delay"}`,
	} {
		for _, event := range []string{"", "error", "RESPONSE.ERROR", "response.failed"} {
			want, failed := openAICompatStreamDataError([]byte(payload), event)
			input := "event: " + event + "\ndata: " + payload + "\n\ndata: [DONE]\n\n"
			gotErr := executor.readStream(t.Context(), strings.NewReader(input), http.Header{"Retry-After": {"90"}}, "m", nil, func([]byte, bool) error { return nil })
			if !failed {
				if gotErr != nil {
					t.Errorf("%s/%s: unexpected error %v", event, payload, gotErr)
				}
				continue
			}
			var got statusErr
			if !errors.As(gotErr, &got) || got.StatusCode() != want.StatusCode() || got.Error() != want.Error() || got.IsCredentialScoped() != want.IsCredentialScoped() || !reflect.DeepEqual(got.RetryAfter(), want.RetryAfter()) {
				t.Errorf("%s/%s: got %#v, want %#v", event, payload, gotErr, want)
			}
		}
	}
}

func BenchmarkWorkBuddyReadStream(b *testing.B) {
	executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN)
	stream := strings.Repeat("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\",\"tool_calls\":[]}}]}\n\n", 64) + "data: [DONE]\n\n"
	consume := func([]byte, bool) error { return nil }
	b.ReportAllocs()
	for b.Loop() {
		if err := executor.readStream(b.Context(), strings.NewReader(stream), nil, "m", nil, consume); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWorkBuddyOpen(b *testing.B) {
	executor := NewWorkBuddyExecutor(&config.Config{}, workbuddy.ProviderCN)
	auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: "bench", AccessToken: "bench"})
	ctx := context.WithValue(b.Context(), "cliproxy.roundtripper", workBuddyTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	text, _ := json.Marshal(strings.Repeat("Ordinary instructions and tool output.\n", 128))
	req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"developer","content":` + string(text) + `},{"role":"user","content":"hi"}],"max_completion_tokens":1024}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload}
	b.ReportAllocs()
	for b.Loop() {
		reporter := helps.NewExecutorUsageReporter(ctx, executor, "m", auth)
		response, _, _, err := executor.open(ctx, auth, req, opts, reporter)
		if err != nil {
			b.Fatal(err)
		}
		executor.closeResponse(response)
	}
}

func TestWorkBuddyThinkingFallbackReachesUpstream(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, body, suffix, want string
				format                   sdktranslator.Format
				payload                  config.PayloadConfig
			}{
				{name: "unsupported chat effort", format: sdktranslator.FormatOpenAI, body: `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`, want: "max"},
				{name: "supported chat effort", format: sdktranslator.FormatOpenAI, body: `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, want: "high"},
				{name: "model suffix", format: sdktranslator.FormatOpenAI, body: `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, suffix: "(medium)", want: "max"},
				{name: "Responses effort", format: sdktranslator.FormatOpenAIResponse, body: `{"input":"hi","reasoning":{"effort":"medium"}}`, want: "max"},
				{name: "Claude budget", format: sdktranslator.FormatClaude, body: `{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8192},"max_tokens":16384}`, want: "max"},
				{
					name: "payload override remains final", format: sdktranslator.FormatOpenAI, body: `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`, want: "high",
					payload: config.PayloadConfig{Override: []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: model, Protocol: "openai", Match: []map[string]any{{"reasoning_effort": "max"}}}},
						Params: map[string]any{"reasoning_effort": "high"},
					}}},
				},
				{
					name: "payload filter remains final", format: sdktranslator.FormatOpenAI, body: `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`,
					payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{
						Models: []config.PayloadModelRule{{Name: model, Protocol: "openai", Match: []map[string]any{{"reasoning_effort": "max"}}}},
						Params: []string{"reasoning_effort"},
					}}},
				},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", provider, stream, tc.name), func(t *testing.T) {
					var requests atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						body, errRead := io.ReadAll(r.Body)
						if errRead != nil {
							t.Error(errRead)
							return
						}
						effort := ojson.GetBytes(body, "reasoning_effort")
						if effort.String() != tc.want || (tc.want == "" && effort.Exists()) {
							t.Errorf("upstream reasoning_effort = %s, want %q", effort.Raw, tc.want)
						}
						if got := ojson.GetBytes(body, "model").String(); got != model {
							t.Errorf("upstream model = %q, want %q", got, model)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, workBuddyTestSSE)
					}))
					defer upstream.Close()
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(t.Name(), provider, []*registry.ModelInfo{{ID: model, Type: provider, Thinking: &registry.ThinkingSupport{Levels: []string{"high", "max"}}}})
					t.Cleanup(func() { reg.UnregisterClient(t.Name()) })
					auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "thinking-test", AccessToken: "test-access"})
					auth.Metadata["base_url"] = upstream.URL
					executor := NewWorkBuddyExecutor(&config.Config{Payload: tc.payload}, provider)
					req := cliproxyexecutor.Request{Model: model + tc.suffix, Payload: []byte(tc.body)}
					opts := cliproxyexecutor.Options{SourceFormat: tc.format, OriginalRequest: req.Payload, Stream: stream}
					if stream {
						result, errStream := executor.ExecuteStream(t.Context(), auth, req, opts)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExecute := executor.Execute(t.Context(), auth, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
					if requests.Load() != 1 {
						t.Fatalf("upstream requests = %d, want 1", requests.Load())
					}
					if string(req.Payload) != tc.body {
						t.Fatal("thinking fallback mutated the caller request")
					}
				})
			}
		}
	}
}

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
					if ojson.GetBytes(sample.body, "prompt_cache_key").String() != "caller-cache" || ojson.GetBytes(sample.body, "user").String() != "caller-user" {
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
					if r.URL.Path != "/v2/chat/completions" || !ojson.GetBytes(body, "stream").Bool() {
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
					if ojson.GetBytes(body, "messages.0.role").String() != "system" || ojson.GetBytes(body, "max_tokens").Int() != 64 || ojson.GetBytes(body, "max_completion_tokens").Exists() {
						t.Errorf("incorrect WorkBuddy request normalization: %s", body)
					}
					if got := ojson.GetBytes(body, "messages.0.content").String(); got != "Be helpful.\n  - Users can report issues and share feedback at https://github.com/anthropics/claude-code/issues" {
						t.Errorf("Claude feedback notice was not normalized: %q", got)
					}
					if stop := ojson.GetBytes(body, "stop"); !stop.IsArray() || len(stop.Array()) != 1 || stop.Array()[0].String() != "</block>" {
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
					if ojson.GetBytes(response.Payload, "choices.0.message.content").String() != "Hello" || ojson.GetBytes(response.Payload, "choices.0.message.reasoning_content").String() != "Think" || ojson.GetBytes(response.Payload, "usage.total_tokens").Int() != 15 {
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
			if ojson.GetBytes(response.Payload, "choices.0.message.tool_calls.0.function.name").String() != "mcp.search" || ojson.GetBytes(response.Payload, "choices.0.message.tool_calls.0.function.arguments").String() != "{}" {
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
				if ojson.GetBytes(body, "messages.1.content").String() != "hi" || !ojson.GetBytes(body, "stream").Bool() {
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
