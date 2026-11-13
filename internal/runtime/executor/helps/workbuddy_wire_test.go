package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func assertWorkBuddyUniqueKeys(t *testing.T, payload []byte) {
	t.Helper()
	if !ojson.ValidBytes(payload) {
		t.Fatalf("invalid JSON: %s", payload)
	}
	var walk func(ojson.Result)
	walk = func(value ojson.Result) {
		if value.Type != ojson.JSON {
			return
		}
		seen := make(map[string]bool)
		value.ForEach(func(key, child ojson.Result) bool {
			if value.IsObject() {
				if seen[key.String()] {
					t.Errorf("duplicate key %q in %s", key.String(), value.Raw)
				}
				seen[key.String()] = true
			}
			walk(child)
			return true
		})
	}
	walk(ojson.GetBytesNoCopy(payload, "@this"))
}

func TestWorkBuddyDuplicateRequestKeysLastWins(t *testing.T) {
	for _, escaped := range []bool{false, true} {
		t.Run(fmt.Sprint(escaped), func(t *testing.T) {
			payload := `{"stream":false,"stream":false,"max_completion_tokens":7,"max_completion_tokens":64,"messages":[{"role":"user","role":"developer","content":"old","content":"keep","extra":{"v":0,"v":9007199254740993}}],"tools":[{"function":{"name":"mcp.old","name":"mcp.lookup","parameters":{"properties":{"a.b":true,"a.b":false,"v":{"enum":[0],"enum":["last"]}}}}}],"tool_choice":{"type":"function","function":{"name":"mcp.old","name":"mcp.lookup"}},"unknown":{"gone":1},"unknown":{"keep":18446744073709551615},"array":[{"k":0,"k":1.2300e+40},{"k":1,"k":null}],"":1,"":2}`
			if escaped {
				payload = strings.Replace(payload, `"stream":false,"stream"`, `"stream":false,"str\u0065am"`, 1)
				payload = strings.ReplaceAll(payload, `"name":"mcp.lookup"`, `"na\u006de":"mcp.lookup"`)
				payload = strings.Replace(payload, `"a.b":false`, `"a\u002eb":false`, 1)
				payload = strings.Replace(payload, `"unknown":{"keep"`, `"un\u006bnown":{"keep"`, 1)
			}
			for _, useDoc := range []bool{false, true} {
				body := []byte(payload)
				var out []byte
				var names map[string]string
				if useDoc {
					doc := ojson.NewDoc(body)
					names = NormalizeWorkBuddyRequestDoc(doc)
					out = doc.Bytes()
				} else {
					var err error
					out, names, err = NormalizeWorkBuddyRequest(body)
					if err != nil {
						t.Fatal(err)
					}
				}
				assertWorkBuddyUniqueKeys(t, out)
				for path, want := range map[string]string{
					"stream": "true", "max_tokens": "64", "messages.0.role": `"system"`, "messages.0.content": `"keep"`,
					"messages.0.extra.v": "9007199254740993", "tools.0.function.name": `"mcp_lookup"`, "tool_choice": `"mcp_lookup"`,
					`tools.0.function.parameters.properties.a\.b.not`: "{}", "tools.0.function.parameters.properties.v.type": `"string"`,
					"unknown.keep": "18446744073709551615", "array.0.k": "1.2300e+40", "array.1.k": "null",
				} {
					if got := ojson.GetBytes(out, path).Raw; got != want {
						t.Errorf("doc=%t %s = %s, want %s", useDoc, path, got, want)
					}
				}
				if ojson.GetBytes(out, "max_completion_tokens").Exists() || ojson.GetBytes(out, "unknown.gone").Exists() || names["mcp_lookup"] != "mcp.lookup" || len(names) != 1 {
					t.Errorf("last-wins normalization lost deletion or tool identity: %s %v", out, names)
				}
				if string(body) != payload {
					t.Fatal("normalization mutated the caller's input")
				}
			}
		})
	}
	for _, payload := range []string{
		`{"max_tokens":1,"max_tokens":9007199254740993,"max_completion_tokens":2,"max_completion_tokens":3}`,
		`{"max_tokens":1,"max_to\u006bens":9007199254740993,"max_completion_tokens":2,"max_completion_tokens":3}`,
	} {
		out, _, err := NormalizeWorkBuddyRequest([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		assertWorkBuddyUniqueKeys(t, out)
		if ojson.GetBytes(out, "max_tokens").Raw != "9007199254740993" || ojson.GetBytes(out, "max_completion_tokens").Exists() {
			t.Fatalf("duplicate token fields lost last value: %s", out)
		}
	}
}

func TestWorkBuddyDuplicateFinishKeysLastWins(t *testing.T) {
	for _, reasons := range []struct{ first, last string }{{"stop", "length"}, {"length", "stop"}} {
		for _, escaped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%s/escaped=%t", reasons.first, reasons.last, escaped), func(t *testing.T) {
				finishKey := "finish_reason"
				if escaped {
					finishKey = `finish_\u0072eason`
				}
				payload := []byte(fmt.Sprintf(`{"id":"c","choices":[{"index":0,"delta":{"content":"old","content":"last","tool_calls":[{"index":0,"function":{"name":"old","name":"mcp_lookup","arguments":"{}"}}]},"finish_reason":%q,"%s":%q}],"usage":{"total_tokens":1,"total_tokens":9007199254740993},"unknown":{"n":1,"n":18446744073709551615}}`, reasons.first, finishKey, reasons.last))
				for _, route := range []string{"combined", "legacy-pair", "split-only"} {
					var frames [][]byte
					var err error
					switch route {
					case "combined":
						frames, err = NormalizeWorkBuddyChunks(payload, "m", map[string]string{"mcp_lookup": "mcp.lookup"})
					case "legacy-pair":
						var normalized []byte
						normalized, err = NormalizeWorkBuddyChunk(payload, "m", map[string]string{"mcp_lookup": "mcp.lookup"})
						if err == nil {
							assertWorkBuddyUniqueKeys(t, normalized)
							frames = SplitWorkBuddyFinishChunks(normalized)
						}
					case "split-only":
						frames = SplitWorkBuddyFinishChunks(payload)
					}
					if err != nil || len(frames) != 2 {
						t.Fatalf("%s: frames=%d err=%v", route, len(frames), err)
					}
					for _, frame := range frames {
						assertWorkBuddyUniqueKeys(t, frame)
					}
					if ojson.GetBytes(frames[0], "choices.0.finish_reason").Exists() || ojson.GetBytes(frames[1], "choices.0.finish_reason").String() != reasons.last ||
						ojson.GetBytes(frames[0], "choices.0.delta.content").String() != "last" || ojson.GetBytes(frames[0], "usage").Exists() ||
						ojson.GetBytes(frames[1], "usage.total_tokens").Raw != "9007199254740993" || ojson.GetBytes(frames[1], "unknown.n").Raw != "18446744073709551615" {
						t.Errorf("%s: incorrect content/terminal ordering: %s / %s", route, frames[0], frames[1])
					}
					var aggregate WorkBuddyAggregate
					for _, frame := range frames {
						if err := aggregate.Add(frame); err != nil {
							t.Fatal(err)
						}
					}
					response, err := aggregate.Response("m")
					if err != nil || ojson.GetBytes(response, "choices.0.finish_reason").String() != reasons.last {
						t.Errorf("%s: aggregate lost last finish: %s %v", route, response, err)
					}
				}
			})
		}
	}
}

func TestWorkBuddyDuplicateKeyDetectionAndRawPreservation(t *testing.T) {
	wide := make([]string, 40)
	for i := range wide {
		wide[i] = fmt.Sprintf(`"key_%d":%d`, i, i)
	}
	for _, tc := range []struct {
		name, payload string
		duplicate     bool
	}{
		{"ordinary", ` {"a":[{"x":1},{"x":2}],"b":"\"a\":0,\"a\":1","c":1.2300e+40,"d":"\\"} `, false},
		{"case-sensitive", `{"Key":1,"key":2,"KEY":3,"\u006bEy":4}`, false},
		{"wide", "{" + strings.Join(wide, ",") + "}", false},
		{"wide-duplicate", "{" + strings.Join(wide, ",") + `,"key_\u0030":9007199254740993}`, true},
		{"escaped-names", `{"":{"":0,"":9007199254740993},"a\\b":1,"a\u005cb":2,"a\"b":0,"a\u0022b":3,"𝄞":1,"\ud834\udd1e":18446744073709551615}`, true},
		{"replacement-object", `{"a":{"discard":0,"x":0,"x":1},"a":{"keep":{"n":0,"n":1.2300e+40}}}`, true},
		{"replacement-scalar", `{"a":{"x":1,"x":2},"a":null,"b":[{"n":0,"n":false}]}`, true},
		{"deep", strings.Repeat(`{"child":[`, 128) + `{"v":1,"v":9007199254740993}` + strings.Repeat("]}", 128), true},
		{"path-characters", `{"?":{".":1,"\u002e":2},"0":0,"0":3,"@this":1,"@this":4,"x:y":1,"x:y":5,"a|b":1,"a|b":6,"[a]":1,"[a]":7}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(body string) any {
				decoder := json.NewDecoder(strings.NewReader(body))
				decoder.UseNumber()
				var value any
				if err := decoder.Decode(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			want := decode(tc.payload)
			input := []byte(tc.payload)
			doc := ojson.NewDoc(input)
			canonical := normalizeWorkBuddyDuplicateKeysDoc(doc)
			if (canonical != "") != tc.duplicate {
				t.Fatalf("duplicate=%t want %t", canonical != "", tc.duplicate)
			}
			if !reflect.DeepEqual(decode(doc.String()), want) {
				t.Fatalf("last-key-wins differs from UseNumber decoder: %s", doc.String())
			}
			assertWorkBuddyUniqueKeys(t, doc.Bytes())
			if string(input) != tc.payload {
				t.Fatal("source mutated")
			}
			if !tc.duplicate && doc.String() != tc.payload {
				t.Fatal("ordinary input reformatted")
			}
			if next := normalizeWorkBuddyDuplicateKeysDoc(doc); next != "" {
				t.Fatalf("normalization is not idempotent: %s", next)
			}
		})
	}
	ordinary := []byte(`{"choices":[{"index":0,"delta":{"content":"hello","tool_calls":[]}}]}`)
	doc := ojson.NewDoc(ordinary)
	if allocs := testing.AllocsPerRun(10, func() { normalizeWorkBuddyDuplicateKeysDoc(doc) }); allocs != 0 {
		t.Fatalf("ordinary key check allocated %g times", allocs)
	}
}

func TestWorkBuddyDuplicatePreparationCacheAndFinalRules(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	req.Payload = []byte(`{"stream":false,"stream":false,"max_completion_tokens":7,"max_completion_tokens":64,"messages":[{"role":"user","role":"developer","content":"hi"}],"reasoning_effort":"low","reasoning_effort":"max"}`)
	opts.OriginalRequest = bytes.Clone(req.Payload)
	cfg.Payload.Override[0].Params["stream"] = false
	cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"max_tokens"}}}
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	var callbacks int
	opts.Metadata = map[string]any{cliproxyexecutor.FinalizedResponseFormatCallbackMetadataKey: func(string) { callbacks++ }}
	for i := 1; i <= 2; i++ {
		body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
		if err != nil {
			t.Fatal(err)
		}
		assertWorkBuddyUniqueKeys(t, body)
		if callbacks != i || ojson.GetBytes(body, "stream").Raw != "false" || ojson.GetBytes(body, "audit.#").Int() != 1 ||
			ojson.GetBytes(body, "max_tokens").Exists() || ojson.GetBytes(body, "max_completion_tokens").Exists() || ojson.GetBytes(body, "reasoning_effort").String() != "max" {
			t.Fatalf("attempt %d lost canonical base or final rules: %s", i, body)
		}
	}
}

func workBuddyRetryFixture() (*config.Config, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"audit.-1": "once"}}}}}
	req := cliproxyexecutor.Request{Model: "retry-fixture", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}],"input":"hello","reasoning_effort":"high","tools":[{"function":{"name":"mcp.lookup","parameters":{}}}]}`)}
	req.Metadata = map[string]any{"cliproxy.resolved_home_model_info": &registry.ModelInfo{ID: req.Model, Type: "workbuddy", Thinking: &registry.ThinkingSupport{Levels: []string{"high", "max"}}}}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: bytes.Clone(req.Payload), Headers: http.Header{"X-Test": {"first"}}}
	return cfg, req, opts
}

func TestWorkBuddyRetryPreparationInvalidation(t *testing.T) {
	for _, change := range []string{"payload-in-place", "original-in-place", "provider", "model", "suffix", "source", "request-format", "response-format", "headers", "gin-headers", "stream", "keep-effort", "config-pointer", "config-version", "config-in-place", "rules", "resolved-thinking"} {
		t.Run(change, func(t *testing.T) {
			cfg, req, opts := workBuddyRetryFixture()
			info := &registry.ModelInfo{ID: req.Model, Type: "workbuddy", Thinking: &registry.ThinkingSupport{Levels: []string{"high", "max"}}}
			req.Metadata = map[string]any{"cliproxy.resolved_home_model_info": info}
			ginCtx := &gin.Context{Request: &http.Request{Header: http.Header{"User-Agent": {"first"}}}}
			ctx := cliproxyexecutor.WithRequestPreparationCache(context.WithValue(t.Context(), "gin", ginCtx))
			holder := cliproxyexecutor.RequestPreparationCacheFromContext(ctx)
			provider := "workbuddy"
			prepare := func() ([]byte, map[string]string) {
				t.Helper()
				body, names, err := PrepareWorkBuddyRequest(ctx, cfg, provider, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				if ojson.GetBytes(body, "audit.#").Int() != 1 {
					t.Fatalf("final rules were cached/reapplied: %s", body)
				}
				return body, names
			}
			prepare()
			first, ok := holder.Load("workbuddy")
			if !ok {
				t.Fatal("base preparation was not cached")
			}
			prepare()
			if second, _ := holder.Load("workbuddy"); first != second {
				t.Fatal("identical input missed request cache")
			}
			switch change {
			case "payload-in-place":
				copy(req.Payload[bytes.Index(req.Payload, []byte("hello")):], "world")
			case "original-in-place":
				copy(opts.OriginalRequest[bytes.Index(opts.OriginalRequest, []byte("hello")):], "world")
			case "provider":
				provider = "workbuddy-ai"
			case "model":
				req.Model = "retry-other"
			case "suffix":
				req.Model += "(max)"
			case "source":
				opts.SourceFormat = sdktranslator.FormatOpenAIResponse
			case "request-format":
				req.Format = sdktranslator.FormatClaude
			case "response-format":
				opts.ResponseFormat = sdktranslator.FormatClaude
			case "headers":
				opts.Headers["X-Test"][0] = "other"
			case "gin-headers":
				ginCtx.Request.Header["User-Agent"][0] = "other"
			case "stream":
				opts.Stream = true
			case "keep-effort":
				opts.KeepTopLevelEffort = true
			case "config-pointer":
				cfg = cfg.CloneForRuntime()
			case "config-version":
				cfg.SourceHash = "next-version"
			case "config-in-place":
				cfg.Client.Codex.OptimizeMultiAgentV2 = true
			case "rules":
				cfg.Payload.Override[0].Params["marker"] = "new-rule"
			case "resolved-thinking":
				info.Thinking.Levels[0] = "low"
			}
			got, _ := prepare()
			if next, _ := holder.Load("workbuddy"); first == next {
				t.Fatalf("%s reused stale preparation", change)
			}
			want, _, err := PrepareWorkBuddyRequest(t.Context(), cfg, provider, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var gotJSON, wantJSON any
			if err := json.Unmarshal(got, &gotJSON); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want, &wantJSON); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotJSON, wantJSON) {
				t.Fatalf("cached preparation differs from uncached: %s vs %s", got, want)
			}
		})
	}
}

func TestWorkBuddyRetryPreparationIsolation(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	var callbacks int
	opts.Metadata = map[string]any{cliproxyexecutor.FinalizedResponseFormatCallbackMetadataKey: func(string) { callbacks++ }}
	first, names, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(first)
	first[0] = '!'
	names["mcp_lookup"] = "changed"
	second, secondNames, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
	if err != nil || !bytes.Equal(second, want) || secondNames["mcp_lookup"] != "mcp.lookup" || callbacks != 2 {
		t.Fatalf("attempt state leaked: %s %v callbacks=%d err=%v", second, secondNames, callbacks, err)
	}
	child := cliproxyexecutor.WithRequestPreparationCache(ctx)
	if cliproxyexecutor.RequestPreparationCacheFromContext(ctx) == cliproxyexecutor.RequestPreparationCacheFromContext(child) {
		t.Fatal("new request inherited previous holder")
	}
	if _, ok := cliproxyexecutor.RequestPreparationCacheFromContext(child).Load("workbuddy"); ok {
		t.Fatal("new request inherited cached body")
	}
	// Concurrent hits own their output bytes, docs and name maps.
	opts.Metadata = nil
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			body, gotNames, errPrepare := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
			if errPrepare != nil || !bytes.Equal(body, want) || gotNames["mcp_lookup"] != "mcp.lookup" {
				t.Errorf("concurrent preparation: %s %v %v", body, gotNames, errPrepare)
				return
			}
			body[0] = '!'
			gotNames["mcp_lookup"] = "changed"
		})
	}
	wg.Wait()
}

type workBuddyRetryHooks struct {
	sdktranslator.PluginHooks
	calls  int
	marker string
}

func (h *workBuddyRetryHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	h.calls++
	doc := ojson.NewDoc(body)
	_ = doc.Set("plugin_marker", h.marker)
	return doc.Bytes()
}

func TestWorkBuddyRetryPreparationPluginBypass(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdktranslator.SetPluginHooks(nil) })
	for _, marker := range []string{"first", "other"} {
		hooks := &workBuddyRetryHooks{marker: marker}
		sdktranslator.SetPluginHooks(hooks)
		for i := 1; i <= 2; i++ {
			body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
			if err != nil || hooks.calls != i || ojson.GetBytes(body, "plugin_marker").String() != marker {
				t.Fatalf("plugin call was cached: calls=%d body=%s err=%v", hooks.calls, body, err)
			}
		}
		if _, ok := cliproxyexecutor.RequestPreparationCacheFromContext(ctx).Load("workbuddy"); ok {
			t.Fatal("plugin path retained a reusable preparation")
		}
	}
	sdktranslator.SetPluginHooks(nil)
	body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
	if err != nil || ojson.GetBytes(body, "plugin_marker").Exists() {
		t.Fatalf("plugin output leaked after unload: %s %v", body, err)
	}
}

func TestWorkBuddyRetryPreparationTranslatorReplacement(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	opts.SourceFormat = sdktranslator.Format("workbuddy-retry-dynamic-translator")
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
		t.Fatal(err)
	}
	first, ok := cliproxyexecutor.RequestPreparationCacheFromContext(ctx).Load("workbuddy")
	if !ok {
		t.Fatal("pure fallback did not retain request preparation")
	}
	if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
		t.Fatal(err)
	}
	if next, _ := cliproxyexecutor.RequestPreparationCacheFromContext(ctx).Load("workbuddy"); first != next {
		t.Fatal("pure fallback missed its reusable snapshot")
	}
	t.Cleanup(func() { sdktranslator.Unregister(opts.SourceFormat, sdktranslator.FormatOpenAI) })
	var calls int
	for _, marker := range []string{"first", "other"} {
		sdktranslator.Register(opts.SourceFormat, sdktranslator.FormatOpenAI, func(_ string, body []byte, _ bool) ([]byte, error) {
			calls++
			doc := ojson.NewDoc(body)
			_ = doc.Set("translator_marker", marker)
			return doc.Bytes(), nil
		}, sdktranslator.ResponseTransform{})
		before := calls
		for range 2 {
			body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
			if err != nil || ojson.GetBytes(body, "translator_marker").String() != marker {
				t.Fatalf("replacement translator was skipped: %s %v", body, err)
			}
		}
		if calls-before != 2 {
			t.Fatalf("registry transforms must execute each attempt: calls=%d", calls-before)
		}
	}
}

func TestWorkBuddyRetryPreparationNativeThinkingReplacement(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
		t.Fatal(err)
	}
	original := thinking.GetProviderApplier("openai")
	applier := &workBuddyRetryThinking{}
	thinking.RegisterProvider("openai", applier)
	t.Cleanup(func() { thinking.RegisterProvider("openai", original) })
	for i := 1; i <= 2; i++ {
		body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
		if err != nil || applier.calls != i || ojson.GetBytes(body, "applier_calls").Int() != int64(i) {
			t.Fatalf("custom applier was cached: %s calls=%d err=%v", body, applier.calls, err)
		}
	}
}

type workBuddyRetryThinking struct{ calls int }

func (a *workBuddyRetryThinking) Apply(body []byte, _ thinking.ThinkingConfig, _ *registry.ModelInfo) ([]byte, error) {
	a.calls++
	doc := ojson.NewDoc(body)
	_ = doc.Set("applier_calls", a.calls)
	return doc.Bytes(), nil
}

func TestWorkBuddyRetryPreparationThinkingPluginBypass(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
		t.Fatal(err)
	}
	applier := &workBuddyRetryThinking{}
	if !thinking.RegisterPluginProvider(t.Name(), "workbuddy", 1, applier) {
		t.Fatal("register thinking plugin")
	}
	t.Cleanup(func() { thinking.UnregisterPluginProviders(t.Name()) })
	for i := 1; i <= 2; i++ {
		body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
		if err != nil || applier.calls != i || ojson.GetBytes(body, "applier_calls").Int() != int64(i) {
			t.Fatalf("thinking plugin was skipped: %s calls=%d err=%v", body, applier.calls, err)
		}
	}
}

func BenchmarkWorkBuddyRetryPreparation(b *testing.B) {
	for _, size := range []int{1024, 65536} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			cfg, req, opts := workBuddyRetryFixture()
			req.Metadata = nil
			doc := ojson.NewDoc(req.Payload)
			_ = doc.Set("messages.0.content", strings.Repeat("ordinary instructions ", size/22))
			req.Payload = doc.Bytes()
			opts.OriginalRequest = req.Payload
			b.ReportAllocs()
			for b.Loop() {
				ctx := cliproxyexecutor.WithRequestPreparationCache(b.Context())
				for range 3 {
					if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func BenchmarkWorkBuddyRegisteredRetryPreparation(b *testing.B) {
	for _, size := range []int{1024, 65536} {
		for _, cache := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/cache=%t", size, cache), func(b *testing.B) {
				cfg, req, opts := workBuddyRetryFixture()
				doc := ojson.NewDoc(req.Payload)
				_ = doc.Set("messages.0.content", strings.Repeat("ordinary instructions ", size/22))
				req.Payload = doc.Bytes()
				opts.OriginalRequest = req.Payload
				b.ReportAllocs()
				for b.Loop() {
					ctx := b.Context()
					if cache {
						ctx = cliproxyexecutor.WithRequestPreparationCache(ctx)
					}
					for range 3 {
						if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func TestWorkBuddyRetryPreparationModelRegistryRefresh(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	req.Metadata = nil
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(t.Name()) })
	for _, effort := range []string{"high", "max"} {
		reg.RegisterClient(t.Name(), "workbuddy", []*registry.ModelInfo{{ID: req.Model, Type: "workbuddy", Thinking: &registry.ThinkingSupport{Levels: []string{effort}}}})
		body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
		if err != nil || ojson.GetBytes(body, "reasoning_effort").String() != effort {
			t.Fatalf("model reload reused stale thinking: %s %v", body, err)
		}
	}
	reg.UnregisterClient(t.Name())
	if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := cliproxyexecutor.RequestPreparationCacheFromContext(ctx).Load("workbuddy"); ok {
		t.Fatal("unresolved/static model retained reusable preparation")
	}
}

func TestWorkBuddyRetryPreparationTranslatorMutatesInputs(t *testing.T) {
	cfg, req, opts := workBuddyRetryFixture()
	opts.SourceFormat = sdktranslator.Format("workbuddy-mutable-translator")
	info, _ := coreauth.ResolvedModelInfo(req)
	mutate := false
	sdktranslator.Register(opts.SourceFormat, sdktranslator.FormatOpenAI, func(_ string, body []byte, _ bool) ([]byte, error) {
		if mutate {
			info.Thinking.Levels = []string{"max"}
		}
		return bytes.Clone(body), nil
	}, sdktranslator.ResponseTransform{})
	t.Cleanup(func() { sdktranslator.Unregister(opts.SourceFormat, sdktranslator.FormatOpenAI) })
	ctx := cliproxyexecutor.WithRequestPreparationCache(t.Context())
	if _, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts); err != nil {
		t.Fatal(err)
	}
	mutate = true
	body, _, err := PrepareWorkBuddyRequest(ctx, cfg, "workbuddy", req, opts)
	if err != nil || ojson.GetBytes(body, "reasoning_effort").String() != "max" {
		t.Fatalf("translator input change reused cached thinking: %s %v", body, err)
	}
}

// Keep the original sequence as an oracle for case folding and replacement order.
func workBuddyLegacyClientText(text string) string {
	text = workBuddyBillingHeader.ReplaceAllString(text, "")
	text = workBuddyClaudeSignature.ReplaceAllString(text, "You are Claude Code, Anthropic's CLI for Claude")
	text = workBuddyClaudeFeedback.ReplaceAllString(text, "Users can report issues and share feedback at ${1}")
	text = workBuddyDSHSignature.ReplaceAllString(text, "You are an AI agent built with DeepSeek Harness")
	text = workBuddyDSHGUIContext.ReplaceAllString(text, "You interact with the user through the DeepSeek Harness Web GUI")
	text = workBuddyDSHPathReference.ReplaceAllString(text, "Tokens starting with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root")
	text = strings.ReplaceAll(text, "Main branch (you will usually use this for PRs)", "Main branch (usually the base for PRs)")
	const signature = "Codex CLI is an open source project led by OpenAI."
	if index := strings.Index(text, signature); index >= 0 && (index < 4 || !strings.EqualFold(text[index-4:index], "the ")) {
		text = text[:index] + "The " + text[index:]
	}
	return text
}

func TestWorkBuddyClientTextSimpleFoldParity(t *testing.T) {
	seeds := []string{
		"x-anthropic-billing-header: cc_version=1; cch=0;\r\nYou are an AI agent powered by DeepSeek Harness.",
		"You are Claude Code, Anthropic’s official CLI for Claude",
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"You are interacting with the user through the DeepSeek Harness Web GUI",
		"Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root",
		"Main branch (you will usually use this for PRs)\nCodex CLI is an open source project led by OpenAI.",
		"the Codex CLI is an open source project led by OpenAI.",
		"x-anthropic-billing-header: cc_bad=missing-semicolon\nYou are a different agent.\xff",
	}
	check := func(input string) {
		t.Helper()
		if got, want := normalizeWorkBuddyClientText(input), workBuddyLegacyClientText(input); got != want {
			t.Fatalf("input %q: got %q, want %q", input, got, want)
		}
	}
	for _, seed := range seeds {
		for _, input := range []string{seed, strings.ToUpper(seed), strings.ToLower(seed), "quoted: " + seed, seed + "\n" + seed} {
			check(input)
		}
		runes := []rune(seed)
		for i, original := range runes {
			for folded := unicode.SimpleFold(original); folded != original; folded = unicode.SimpleFold(folded) {
				runes[i] = folded
				check(string(runes))
			}
			runes[i] = original
		}
	}
}

func TestWorkBuddyClientTextNoMatchAllocations(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("Ordinary instructions and tool output.\n", 128),
		"You are a helpful assistant. Tokens prefixed with something else. x-anthropic-invalid: keep;",
		"用户消息：保留原文、大小写和标点。K ſ",
	} {
		if got := normalizeWorkBuddyClientText(text); got != text {
			t.Fatalf("unmatched text changed: %q", got)
		}
		if allocations := testing.AllocsPerRun(10, func() { normalizeWorkBuddyClientText(text) }); allocations != 0 {
			t.Errorf("unmatched text allocated %g times", allocations)
		}
	}
}

func TestWorkBuddyRequestPreservesPrecisionAndSchemaLiterals(t *testing.T) {
	body := []byte(`{"extension":{"n":9007199254740993},"messages":[{"role":"developer","content":"keep"}],"tools":[{"function":{"name":"mcp.lookup","parameters":{"properties":{"a.b":{"enum":[9007199254740993,"s",null],"anyOf":[{"const":true}],"allOf":[{"description":"keep"}]},"tuple":{"items":[true,false],"additionalItems":false},"literal":{"default":{"items":[true,false]},"enum":[{"not":false}]}},"additionalProperties":false}}}]}`)
	before := string(body)
	out, _, err := NormalizeWorkBuddyRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"extension.n": "9007199254740993",
		`tools.0.function.parameters.properties.a\.b.allOf.2.anyOf.0.enum.0`: "9007199254740993",
		`tools.0.function.parameters.properties.a\.b.allOf.1.anyOf.0.const`:  "true",
		"tools.0.function.parameters.properties.tuple.items.anyOf.0":         "{}",
		"tools.0.function.parameters.properties.tuple.items.anyOf.1.not":     "{}",
		"tools.0.function.parameters.properties.literal.default.items":       "[true,false]",
		"tools.0.function.parameters.properties.literal.enum.0.not":          "false",
		"tools.0.function.parameters.additionalProperties":                   "false",
	} {
		if got := ojson.GetBytes(out, path).Raw; got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	if ojson.GetBytes(out, "tools.0.function.parameters.properties.tuple.additionalItems").Exists() || string(body) != before {
		t.Fatal("tuple normalization or input isolation failed")
	}
}

func workBuddyChunkPipeline(payload []byte, model string, names map[string]string) ([][]byte, error) {
	return NormalizeWorkBuddyChunks(payload, model, names)
}

func TestWorkBuddyChunkPrecisionAndFinishOrder(t *testing.T) {
	payload := []byte(`{"id":"c","created":9007199254740993,"extension":{"big":18446744073709551615},"choices":[{"index":2,"delta":{"tool_calls":[{"index":1,"function":{"name":"mcp_lookup","arguments":"{}"},"extra":9007199254740993}]},"finish_reason":"tool_calls","extra":"keep"},{"index":0,"delta":{"content":"last","tool_calls":[]},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"length"}],"usage":{"total_tokens":9007199254740993}}`)
	before := string(payload)
	frames, err := workBuddyChunkPipeline(payload, "caller", map[string]string{"mcp_lookup": "mcp.lookup"})
	if err != nil || len(frames) != 2 {
		t.Fatalf("frames = %d, err = %v", len(frames), err)
	}
	for _, frame := range frames {
		if ojson.GetBytes(frame, "created").Raw != "9007199254740993" || ojson.GetBytes(frame, "extension.big").Raw != "18446744073709551615" || ojson.GetBytes(frame, "model").String() != "caller" {
			t.Errorf("metadata lost: %s", frame)
		}
	}
	if ojson.GetBytes(frames[0], "usage").Exists() || ojson.GetBytes(frames[0], "choices.0.finish_reason").Exists() ||
		ojson.GetBytes(frames[0], "choices.0.delta.tool_calls.0.function.name").String() != "mcp.lookup" ||
		ojson.GetBytes(frames[0], "choices.0.delta.tool_calls.0.extra").Raw != "9007199254740993" ||
		ojson.GetBytes(frames[0], "choices.0.extra").String() != "keep" ||
		ojson.GetBytes(frames[0], "choices.1.delta.tool_calls").Exists() ||
		ojson.GetBytes(frames[0], "choices.2.finish_reason").String() != "length" {
		t.Errorf("content frame changed: %s", frames[0])
	}
	if ojson.GetBytes(frames[1], "usage.total_tokens").Raw != "9007199254740993" ||
		ojson.GetBytes(frames[1], "choices.#").Int() != 2 || ojson.GetBytes(frames[1], "choices.0.index").Int() != 2 ||
		ojson.GetBytes(frames[1], "choices.0.delta").Raw != "{}" || ojson.GetBytes(frames[1], "choices.1.finish_reason").String() != "stop" || string(payload) != before {
		t.Errorf("terminal ordering, usage or isolation changed: %s", frames[1])
	}
}

func TestWorkBuddyAggregateInterleavedArguments(t *testing.T) {
	var aggregate WorkBuddyAggregate
	for i := range 128 {
		frame := fmt.Sprintf(`{"id":"c","created":9007199254740993,"choices":[{"index":2,"delta":{"tool_calls":[{"index":3,"id":"later","function":{"name":"%s","arguments":"abc"}}]}},{"index":0,"delta":{"tool_calls":[{"index":1,"id":"first","function":{"name":"%s","arguments":"xyz"}}]}}]}`, strings.Repeat("a", i%2), strings.Repeat("b", i%2))
		if err := aggregate.Add([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := aggregate.Response("m")
	if err != nil {
		t.Fatal(err)
	}
	if err := aggregate.Add([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"!"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":9007199254740993}}`)); err != nil {
		t.Fatal(err)
	}
	second, err := aggregate.Response("m")
	if err != nil {
		t.Fatal(err)
	}
	path := "choices.0.message.tool_calls.0.function.arguments"
	if ojson.GetBytes(first, path).String() != strings.Repeat("xyz", 128) || ojson.GetBytes(second, path).String() != strings.Repeat("xyz", 128)+"!" ||
		ojson.GetBytes(second, "choices.1.message.tool_calls.0.function.arguments").String() != strings.Repeat("abc", 128) ||
		ojson.GetBytes(second, "choices.0.message.tool_calls.0.function.name").String() != strings.Repeat("b", 64) ||
		ojson.GetBytes(second, "choices.0.message.content").Raw != "null" || ojson.GetBytes(second, "usage.total_tokens").Raw != "9007199254740993" {
		t.Fatalf("aggregate lost fragments, ordering or snapshot isolation: %s", second)
	}
}

func TestWorkBuddyOptionalFieldTypes(t *testing.T) {
	payload := []byte(`{"messages":false,"tools":[{"function":{"name":1.2,"parameters":{"enum":"keep","anyOf":{"description":"literal"},"properties":{"value":{"enum":true}}}}}],"tool_choice":{"type":"function","function":{"name":123}}}`)
	out, names, err := NormalizeWorkBuddyRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"messages.0.role": `"system"`, "messages.#": "1", "tool_choice": `""`,
		"tools.0.function.name": "1.2", "tools.0.function.parameters.enum": `"keep"`,
		"tools.0.function.parameters.type": `"object"`, "tools.0.function.parameters.properties.value.enum": "true",
	} {
		if got := ojson.GetBytes(out, path).Raw; got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	if len(names) != 0 || ojson.GetBytes(out, "tools.0.function.parameters.properties.value.type").Exists() {
		t.Errorf("non-string names or non-array enums were normalized: %s", out)
	}
}

func TestWorkBuddyCombinedChunkLegacyAPIParity(t *testing.T) {
	names := map[string]string{"mcp_lookup": "mcp.lookup"}
	for _, payload := range []string{
		`{"choices":[{"delta":{"content":"hello","tool_calls":[]}}],"extra":9007199254740993}`,
		`{"choices":[{"delta":{"tool_calls":[]},"finish_reason":"stop"}],"usage":{"total_tokens":2}}`,
		`{"choices":[{"delta":{"content":""},"finish_reason":"stop"}],"usage":null}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"name":"mcp_lookup","arguments":"{}"}}]},"message":{"tool_calls":[{"function":{"name":"mcp_lookup"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"index":0,"delta":{"unknown":null},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"length"}],"unknown":{"n":18446744073709551615}}`,
		`{"choices":[],"usage":{"total_tokens":9007199254740993}}`,
		`{"choices":null}`, `{"choices":[null,{},42]}`,
	} {
		normalized, err := NormalizeWorkBuddyChunk([]byte(payload), "m", names)
		if err != nil {
			t.Fatal(err)
		}
		want := SplitWorkBuddyFinishChunks(normalized)
		got, err := NormalizeWorkBuddyChunks([]byte(payload), "m", names)
		if err != nil || len(got) != len(want) {
			t.Fatalf("split mismatch: %s: %v", payload, err)
		}
		for i := range want {
			decode := func(body []byte) any {
				decoder := json.NewDecoder(strings.NewReader(string(body)))
				decoder.UseNumber()
				var value any
				if errDecode := decoder.Decode(&value); errDecode != nil {
					t.Fatal(errDecode)
				}
				return value
			}
			if !reflect.DeepEqual(decode(got[i]), decode(want[i])) {
				t.Fatalf("frame %d mismatch: %s vs %s", i, got[i], want[i])
			}
		}
	}
	for _, payload := range []string{"", " ", "null", "[]", "1", `{"choices":[`} {
		if _, err := NormalizeWorkBuddyChunks([]byte(payload), "m", nil); err == nil {
			t.Errorf("accepted malformed/non-object chunk %q", payload)
		}
	}
	for _, payload := range []string{`{"choices":[]}`, "not-json"} {
		body := []byte(payload)
		got := SplitWorkBuddyFinishChunks(body)
		if len(got) != 1 || &got[0][0] != &body[0] {
			t.Fatal("no-split compatibility API must return the input")
		}
	}
}

func TestParseWorkBuddyErrorClassificationCompatibility(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		payload, code, message string
		seconds                int64
	}{
		{`{"code":11140,"msg":"ban"}`, "11140", "ban", -1},
		{`{"code":500,"msg":"root","error":{"code":"14018","message":"quota"}}`, "14018", "quota", -1},
		{`{"code":6004,"msg":"root","error":{"message":"ignored","data":{"code":"6000","msg":"reset 2026-10-20 09:00:00 UTC+0"}}}`, "6000", "reset 2026-10-20 09:00:00 UTC+0", 3600},
		{`{"error":{"code":6004,"message":"reset 2026-10-20 07:00:00 UTC+0"}}`, "6004", "reset 2026-10-20 07:00:00 UTC+0", 0},
		{`{"code":6020,"msg":"queue","data":{"retry_after":"7"}}`, "6020", "queue", 7},
		{`{"code":6020,"Data":{"retry_after":7}}`, "6020", "", -1},
		{`{"code":6020,"data":{"retry_after":0}}`, "6020", "", -1},
		{`{"code":6005,"msg":"unknown"}`, "6005", "unknown", -1},
		{`{"code":"6004.0","msg":"exact-code"}`, "6004.0", "exact-code", -1},
		{`{"code":6004,"msg":7}`, "", "", -1},
		{`{"code":6004,"error":"invalid-shape"}`, "", "", -1},
		{`{"code":6004`, "", "", -1},
		{`{"choices":[{"delta":{"content":"6004 11140"}}]}`, "", "", -1},
	} {
		body := []byte(tc.payload)
		parsed := ParseWorkBuddyError(body)
		if parsed.Code != tc.code || parsed.Message != tc.message {
			t.Errorf("%s: parsed %+v", tc.payload, parsed)
		}
		checks := []struct {
			code  string
			check func([]byte) bool
		}{
			{"11140", IsWorkBuddyAccountBanned}, {"14018", IsWorkBuddyQuotaExhausted}, {"6000", IsWorkBuddyModelRateLimit}, {"6004", IsWorkBuddyRateLimit}, {"6020", IsWorkBuddyQueueWaiting},
		}
		wantRecognized := false
		for _, check := range checks {
			want := check.code == tc.code
			wantRecognized = wantRecognized || want
			if check.check(body) != want {
				t.Errorf("%s: compatibility check %s", tc.payload, check.code)
			}
		}
		if parsed.Recognized() != wantRecognized {
			t.Errorf("%s: incorrect classification", tc.payload)
		}
		delay := parsed.RetryAfter(now)
		if tc.seconds < 0 {
			if delay != nil {
				t.Errorf("%s: unexpected retry %v", tc.payload, *delay)
			}
		} else if delay == nil || *delay != time.Duration(tc.seconds)*time.Second {
			t.Errorf("%s: retry = %v", tc.payload, delay)
		}
	}
}

func BenchmarkWorkBuddyClientText(b *testing.B) {
	for name, text := range map[string]string{
		"plain":   strings.Repeat("Ordinary instructions and tool output.\n", 128),
		"near":    strings.Repeat("You are a helpful assistant. To give advice, use examples.\n", 64),
		"unicode": strings.Repeat("普通用户消息。K ſ\n", 128),
		"match":   strings.Repeat("You are Claude Code, Anthropic's official CLI for Claude.\n", 32),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				normalizeWorkBuddyClientText(text)
			}
		})
	}
}

func BenchmarkWorkBuddyNormalizeRequest(b *testing.B) {
	text, _ := json.Marshal(strings.Repeat("Ordinary instructions and tool output.\n", 128))
	payload := []byte(`{"messages":[{"role":"developer","content":` + string(text) + `},{"role":"user","content":"hello"}],"tools":[{"function":{"name":"mcp.lookup","parameters":{"properties":{"kind":{"enum":["a","b"]},"value":{"enum":[1,"one",null]}}}}}],"max_completion_tokens":1024}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := NormalizeWorkBuddyRequest(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWorkBuddyChunkPipeline(b *testing.B) {
	for name, payload := range map[string]string{
		"ordinary": `{"id":"c","choices":[{"index":0,"delta":{"content":"hello","tool_calls":[]}}]}`,
		"terminal": `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"mcp_lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":15}}`,
	} {
		b.Run(name, func(b *testing.B) {
			body := []byte(payload)
			names := map[string]string{"mcp_lookup": "mcp.lookup"}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := workBuddyChunkPipeline(body, "m", names); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkWorkBuddyErrorChecks(b *testing.B) {
	body := []byte(`{"id":"c","choices":[{"index":0,"delta":{"content":"hello"}}]}`)
	b.ReportAllocs()
	for b.Loop() {
		if ParseWorkBuddyError(body).Recognized() {
			b.Fatal("content became error")
		}
	}
}

func BenchmarkWorkBuddyAggregateArguments(b *testing.B) {
	for _, fragments := range []int{256, 1024} {
		b.Run(fmt.Sprint(fragments), func(b *testing.B) {
			payload := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"` + strings.Repeat("x", 128) + `"}}]}}]}`)
			b.ReportAllocs()
			for b.Loop() {
				var aggregate WorkBuddyAggregate
				for range fragments {
					if err := aggregate.Add(payload); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := aggregate.Response("m"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHPromptPrefix(t *testing.T) {
	const signature = "You are an AI agent powered by DeepSeek Harness"
	const replacement = "You are an AI agent built with DeepSeek Harness"
	for _, test := range []struct {
		name, input, want string
	}{
		{"first-line", signature + ".", replacement + "."},
		{"no-period", signature, replacement},
		{"uppercase", strings.ToUpper(signature) + ".", replacement + "."},
		{"lowercase", strings.ToLower(signature) + ".", replacement + "."},
		{"full-prompt", signature + ".\n\nKeep all other instructions.", replacement + ".\n\nKeep all other instructions."},
		{"punctuation", signature + "!", replacement + "!"},
		{"already-rewritten", replacement + ".", replacement + "."},
		{"quoted-signature", "Quoted text: " + signature + ".", "Quoted text: " + signature + "."},
		{"leading-space", " " + signature + ".", " " + signature + "."},
		{"later-line", "Context\n" + signature + ".", "Context\n" + signature + "."},
		{"ordinary-prompt", "You are a helpful assistant.", "You are a helpful assistant."},
		{"other-product", "You are an AI agent powered by DeepSeek.", "You are an AI agent powered by DeepSeek."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized prompt = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHGUIContext(t *testing.T) {
	const signature = "You are interacting with the user through the DeepSeek Harness Web GUI"
	const replacement = "You interact with the user through the DeepSeek Harness Web GUI"
	const suffix = " whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI. The browser provides no implicit DOM, route, or screenshot context. The client-plugin HMR receiver is active, but client-plugin changes reload without a refresh only while `pnpm run dev:web` is also running from this same checkout to rebuild their bundles; verify that watcher before promising automatic updates. Every other change requires rebuilding the affected Web artifacts and verifying this existing URL after a page refresh. Starting another server does not update this GUI. The apps/web Vite entry builds the shell but is not a standalone application because only dsh web injects window.__DSH_BOOT__. Do not start a replacement server unless the user asks; if one is needed, use a managed background job and verify its exact URL."
	for _, test := range []struct {
		name, input, want string
	}{
		{"gui-paragraph", signature + suffix, replacement + suffix},
		{"after-first-line", "You are an AI agent powered by DeepSeek Harness.\n\n" + signature + suffix, "You are an AI agent built with DeepSeek Harness.\n\n" + replacement + suffix},
		{"uppercase", strings.ToUpper(signature) + suffix, replacement + suffix},
		{"lowercase", strings.ToLower(signature) + suffix, replacement + suffix},
		{"multiple-blocks", signature + suffix + "\n\n" + signature + suffix, replacement + suffix + "\n\n" + replacement + suffix},
		{"already-rewritten", replacement + suffix, replacement + suffix},
		{"runtime-context-unchanged", "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815", "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815"},
		{"other-gui-unchanged", "You are interacting with the user through another Web GUI.", "You are interacting with the user through another Web GUI."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized GUI context = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("GUI normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHPathReferenceFingerprint(t *testing.T) {
	const signature = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	const replacement = "Tokens starting with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	for _, test := range []struct {
		name, input, want string
	}{
		{"exact-fingerprint", signature, replacement},
		{"full-paragraph", signature + "; absolute paths identify files or directories on the host.", replacement + "; absolute paths identify files or directories on the host."},
		{"uppercase", strings.ToUpper(signature), replacement},
		{"changed-word", strings.Replace(signature, "referenced", "mentioned", 1), strings.Replace(signature, "referenced", "mentioned", 1)},
		{"line-break", strings.Replace(signature, ". Relative", ".\nRelative", 1), strings.Replace(signature, ". Relative", ".\nRelative", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized path reference = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHPathReferenceMessageForms(t *testing.T) {
	const signature = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	const replacement = "Tokens starting with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	for _, block := range []bool{false, true} {
		t.Run(fmt.Sprintf("text-block=%t", block), func(t *testing.T) {
			var content any = signature + "."
			path := "messages.0.content"
			if block {
				content = []any{
					map[string]any{"type": "text", "text": signature + "."},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}},
				}
				path += ".0.text"
			}
			body, err := json.Marshal(map[string]any{
				"model":    "deepseek-v4.1-flash",
				"messages": []any{map[string]any{"role": "developer", "content": content}},
			})
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := NormalizeWorkBuddyRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			if got := ojson.GetBytes(out, path).String(); got != replacement+"." {
				t.Fatalf("normalized path reference = %q, want %q", got, replacement+".")
			}
			if got := ojson.GetBytes(out, "messages.0.role").String(); got != "system" {
				t.Fatalf("message role = %q, want system", got)
			}
			if block && ojson.GetBytes(out, "messages.0.content.1.image_url.url").String() != "https://example.test/image.png" {
				t.Fatalf("image content changed: %s", out)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHMessageForms(t *testing.T) {
	const prompt = "You are an AI agent powered by DeepSeek Harness.\nKeep the remaining instructions.\n\nYou are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	const want = "You are an AI agent built with DeepSeek Harness.\nKeep the remaining instructions.\n\nYou interact with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	for _, role := range []string{"system", "developer", "assistant", "user"} {
		for _, block := range []bool{false, true} {
			name := role + "/string"
			var content any = prompt
			if block {
				name = role + "/text-block"
				content = []any{
					map[string]any{"type": "text", "text": prompt},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}},
				}
			}
			t.Run(name, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{
					"model": "deepseek-v4.1-flash",
					"messages": []any{
						map[string]any{"role": "system", "content": "Keep this instruction."},
						map[string]any{"role": role, "content": content},
						map[string]any{"role": "user", "content": "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815"},
					},
					"tools": []any{map[string]any{"type": "function", "function": map[string]any{
						"name": "lookup", "parameters": map[string]any{"type": "object"},
					}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				out, _, err := NormalizeWorkBuddyRequest(body)
				if err != nil {
					t.Fatal(err)
				}
				path := "messages.1.content"
				if block {
					path += ".0.text"
					if got := ojson.GetBytes(out, "messages.1.content.1.image_url.url").String(); got != "https://example.test/image.png" {
						t.Fatalf("image content changed: %s", out)
					}
				}
				if got := ojson.GetBytes(out, path).String(); got != want {
					t.Fatalf("DSH prompt = %q, want %q", got, want)
				}
				wantRole := role
				if role == "developer" {
					wantRole = "system"
				}
				if ojson.GetBytes(out, "messages.1.role").String() != wantRole ||
					ojson.GetBytes(out, "messages.0.content").String() != "Keep this instruction." ||
					ojson.GetBytes(out, "messages.2.content").String() != "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815" ||
					ojson.GetBytes(out, "tools.0.function.name").String() != "lookup" {
					t.Fatalf("unrelated request fields changed: %s", out)
				}
			})
		}
	}
}

func TestWorkBuddyNormalizesClaudeFeedbackNotice(t *testing.T) {
	const url = "https://github.com/anthropics/claude-code/issues"
	const signature = "To give feedback, users should report the issue at " + url
	const replacement = "Users can report issues and share feedback at " + url
	for _, test := range []struct {
		name, input, want string
	}{
		{"feedback-line", "  - " + signature, "  - " + replacement},
		{"prompt-context", "# Doing tasks\nKeep these instructions.\n" + signature + "\nNext instruction.", "# Doing tasks\nKeep these instructions.\n" + replacement + "\nNext instruction."},
		{"uppercase", strings.ToUpper(signature), "Users can report issues and share feedback at " + strings.ToUpper(url)},
		{"lowercase", strings.ToLower(signature), replacement},
		{"multiple-notices", signature + "\n" + signature, replacement + "\n" + replacement},
		{"already-normalized", replacement, replacement},
		{"url-only", url, url},
		{"phrase-only", "To give feedback, users should report the issue at", "To give feedback, users should report the issue at"},
		{"another-project", "To give feedback, users should report the issue at https://github.com/example/project/issues", "To give feedback, users should report the issue at https://github.com/example/project/issues"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized feedback notice = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("feedback normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesClaudeFeedbackMessageForms(t *testing.T) {
	const notice = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	const replacement = "Users can report issues and share feedback at https://github.com/anthropics/claude-code/issues"
	for _, role := range []string{"system", "developer", "assistant", "user", "tool"} {
		for _, block := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/block=%t", role, block), func(t *testing.T) {
				var content any = notice
				if block {
					content = []any{
						map[string]any{"type": "text", "text": notice},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}},
					}
				}
				body, err := json.Marshal(map[string]any{
					"model": "glm-5.3-flash",
					"messages": []any{
						map[string]any{"role": "system", "content": "Keep this instruction."},
						map[string]any{"role": role, "content": content},
					},
					"tools": []any{map[string]any{"type": "function", "function": map[string]any{
						"name": "lookup", "description": notice, "parameters": map[string]any{"type": "object"},
					}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				original := string(body)
				out, _, err := NormalizeWorkBuddyRequest(body)
				if err != nil {
					t.Fatal(err)
				}
				path := "messages.1.content"
				if block {
					path += ".0.text"
					if got := ojson.GetBytes(out, "messages.1.content.1.image_url.url").String(); got != "https://example.test/image.png" {
						t.Fatalf("image content changed: %s", out)
					}
				}
				if got := ojson.GetBytes(out, path).String(); got != replacement {
					t.Fatalf("feedback notice = %q, want %q", got, replacement)
				}
				if string(body) != original || ojson.GetBytes(out, "messages.0.content").String() != "Keep this instruction." ||
					ojson.GetBytes(out, "tools.0.function.description").String() != notice {
					t.Fatal("normalization changed the original request or unrelated fields")
				}
			})
		}
	}
}

func TestWorkBuddyNormalizesStopSequences(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
	}{
		{"string", `"</block>"`, `["</block>"]`},
		{"single-array", `["</block>"]`, `["</block>"]`},
		{"multiple-array", `["</block>","END"]`, `["</block>","END"]`},
		{"empty-string", `""`, `[""]`},
		{"empty-array", `[]`, `[]`},
		{"null", `null`, `null`},
		{"absent", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := map[string]any{
				"messages":   []any{map[string]any{"role": "system", "content": "Keep the monitor instructions."}},
				"max_tokens": 2112,
			}
			if test.input != "" {
				request["stop"] = json.RawMessage(test.input)
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			original := string(body)
			out, _, err := NormalizeWorkBuddyRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			var normalized map[string]any
			if err := json.Unmarshal(out, &normalized); err != nil {
				t.Fatal(err)
			}
			got, exists := normalized["stop"]
			var want any
			if test.want != "" {
				if err := json.Unmarshal([]byte(test.want), &want); err != nil {
					t.Fatal(err)
				}
			}
			if exists != (test.want != "") || !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized stop = %#v (present=%t), want %s", got, exists, test.want)
			}
			if string(body) != original || ojson.GetBytes(out, "messages.0.content").String() != "Keep the monitor instructions." || ojson.GetBytes(out, "max_tokens").Int() != 2112 {
				t.Fatal("stop normalization changed the source request or unrelated fields")
			}
			twice, _, err := NormalizeWorkBuddyRequest(out)
			if err != nil || string(twice) != string(out) {
				t.Fatalf("stop normalization is not idempotent: %v", err)
			}
		})
	}
}

func TestWorkBuddyBillingHeaderPreservesPrompt(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=2.1.306.c1c; cc_entrypoint=claude-vscode;"
	const prompt = "You are a security monitor for autonomous AI coding agents.\n\n## Context\nKeep every instruction."
	for _, test := range []struct {
		name, input, want string
	}{
		{"same-line", billing + prompt, prompt},
		{"next-line", billing + "\n" + prompt, prompt},
		{"crlf", billing + "\r\n" + prompt, prompt},
		{"header-only", billing, ""},
		{"uppercase-header", strings.ToUpper(billing) + prompt, prompt},
		{"extra-fields", billing + " cch=00000; cc_prev_req=req_test;" + prompt, prompt},
		{"instruction-assignment", billing + "decision=deny; " + prompt, "decision=deny; " + prompt},
		{"plain-prompt", prompt, prompt},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("billing cleanup = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("billing cleanup is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyPreservesExistingClientRewrites(t *testing.T) {
	input := "x-anthropic-billing-header: cc_version=test;\nYou are Claude Code, Anthropic's official CLI for Claude.\nMain branch (you will usually use this for PRs): main\nCodex CLI is an open source project led by OpenAI."
	want := "You are Claude Code, Anthropic's CLI for Claude.\nMain branch (usually the base for PRs): main\nThe Codex CLI is an open source project led by OpenAI."
	if got := normalizeWorkBuddyClientText(input); got != want {
		t.Fatalf("existing client rewrites changed: %q", got)
	}
}

func TestParseWorkBuddyErrorQuotaExhausted(t *testing.T) {
	const message = "\u989d\u5ea6\u5df2\u7528\u5c3d"
	for _, payload := range []string{
		`{"code":14018,"msg":"` + message + `"}`,
		`{"error":{"code":"14018","message":"` + message + `"}}`,
		`{"error":{"data":{"code":14018,"msg":"` + message + `"}}}`,
		`{"msg":"` + message + `","error":{"data":{"code":14018}}}`,
		`{"code":500,"error":{"code":502,"message":"gateway error","data":{"code":"14018","message":"` + message + `"}}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			code, gotMessage, ok := parseWorkBuddyError([]byte(payload))
			if !ok || code != "14018" || gotMessage != message {
				t.Fatalf("parsed error = (%q, %q, %t), want (14018, %q, true)", code, gotMessage, ok, message)
			}
		})
	}
}

func TestWorkBuddyRateLimitResetTime(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		payload   string
		wantDelay *time.Duration
		limited   bool
	}{
		{"reported-cn-reset", `{"code":6004,"msg":"\u60a8\u7684\u4f7f\u7528\u91cf\u5df2\u8d85\u51fa\u9891\u7387\u9650\u5236\uff0c\u5c06\u5728 2026-10-21 00:14:52 UTC+8 \u91cd\u7f6e"}`, new(8*time.Hour + 14*time.Minute + 52*time.Second), true},
		{"string-code-offset-minutes", `{"error":{"code":"6004","message":"reset 2026-10-20 12:00:00 UTC+03:00"}}`, new(time.Hour), true},
		{"nested-data-reset", `{"error":{"data":{"code":6004,"msg":"reset 2026-10-20 12:00:00 UTC+03:00"}}}`, new(time.Hour), true},
		{"negative-offset", `{"code":6004,"message":"reset 2026-10-20 03:30:00 UTC-05:30"}`, new(time.Hour), true},
		{"at-reset", `{"code":6004,"msg":"reset 2026-10-20 16:00:00 UTC+8"}`, new(time.Duration(0)), true},
		{"past-reset", `{"code":6004,"msg":"reset 2026-10-19 16:00:00 UTC+8"}`, new(time.Duration(0)), true},
		{"no-reset-time", `{"code":6004,"msg":"rate limit"}`, nil, true},
		{"invalid-date", `{"code":6004,"msg":"reset 2026-02-30 16:00:00 UTC+8"}`, nil, true},
		{"invalid-zone", `{"code":6004,"msg":"reset 2026-10-20 16:00:00 UTC+88"}`, nil, true},
		{"invalid-zone-minutes", `{"code":6004,"msg":"reset 2026-10-20 16:00:00 UTC+08:99"}`, nil, true},
		{"other-code", `{"code":429,"msg":"reset 2026-10-20 16:00:00 UTC+8"}`, nil, false},
		{"success-text", `{"choices":[{"delta":{"content":"error code 6004"}}]}`, nil, false},
		{"invalid-json", `{"code":6004`, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(test.payload)
			if got := IsWorkBuddyRateLimit(payload); got != test.limited {
				t.Fatalf("IsWorkBuddyRateLimit() = %t, want %t", got, test.limited)
			}
			got := WorkBuddyRateLimitRetryAfter(payload, now)
			if (got == nil) != (test.wantDelay == nil) {
				t.Fatalf("retry delay = %v, want %v", got, test.wantDelay)
			}
			if got != nil && *got != *test.wantDelay {
				t.Fatalf("retry delay = %v, want %v", *got, *test.wantDelay)
			}
		})
	}
}

func TestWorkBuddyModelRateLimitResetTime(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		payload   string
		wantDelay *time.Duration
		limited   bool
	}{
		{"root-code", `{"code":6000,"msg":"model limit reset 2026-10-21 00:14:52 UTC+8"}`, new(8*time.Hour + 14*time.Minute + 52*time.Second), true},
		{"nested-string-code", `{"error":{"data":{"code":"6000","message":"reset 2026-10-20 12:00:00 UTC+03:00"}}}`, new(time.Hour), true},
		{"no-reset", `{"error":{"code":6000,"message":"model rate limit"}}`, nil, true},
		{"account-code-is-not-model-limit", `{"code":6004,"msg":"account rate limit"}`, nil, false},
		{"other-code", `{"code":429,"msg":"model limit reset 2026-10-20 12:00:00 UTC+8"}`, nil, false},
		{"success-text", `{"choices":[{"delta":{"content":"error code 6000"}}]}`, nil, false},
		{"invalid-json", `{"code":6000`, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(test.payload)
			if got := IsWorkBuddyModelRateLimit(payload); got != test.limited {
				t.Fatalf("IsWorkBuddyModelRateLimit() = %t, want %t", got, test.limited)
			}
			if got := IsWorkBuddyRateLimit(payload); got != (test.name == "account-code-is-not-model-limit") {
				t.Fatalf("IsWorkBuddyRateLimit() = %t for %s", got, test.name)
			}
			got := WorkBuddyRateLimitRetryAfter(payload, now)
			if (got == nil) != (test.wantDelay == nil) {
				t.Fatalf("retry delay = %v, want %v", got, test.wantDelay)
			}
			if got != nil && *got != *test.wantDelay {
				t.Fatalf("retry delay = %v, want %v", *got, *test.wantDelay)
			}
		})
	}
}

func TestWorkBuddyNormalizesMCPSchemasAndPreservesToolIdentity(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"mcp.search","arguments":"{}"}}]},{"role":"tool","name":"mcp.search","tool_call_id":"1","content":"ok"}],"tools":[{"type":"function","function":{"name":"mcp_search","parameters":{}}},{"type":"function","function":{"name":"mcp.search","parameters":{"properties":{"format":{"enum":["json","text"]},"literal":{"enum":[1,"one",null]},"allow":true,"deny":false,"labels":{"type":"object","additionalProperties":false}}}}}],"tool_choice":{"type":"function","function":{"name":"mcp.search"}}}`)
	out, names, err := NormalizeWorkBuddyRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if names["mcp_search_2"] != "mcp.search" || ojson.GetBytes(out, "messages.1.tool_calls.0.function.name").String() != "mcp_search_2" || ojson.GetBytes(out, "tool_choice").String() != "mcp_search_2" {
		t.Fatalf("tool name collision or history was not preserved: %s", out)
	}
	for path, expected := range map[string]string{
		"tools.1.function.parameters.type": "object", "tools.1.function.parameters.properties.format.type": "string",
		"tools.1.function.parameters.properties.literal.anyOf.0.type": "number", "tools.1.function.parameters.properties.allow": "{}",
		"tools.1.function.parameters.properties.deny.not": "{}", "tools.1.function.parameters.properties.labels.additionalProperties": "false",
	} {
		if got := ojson.GetBytes(out, path).String(); got != expected {
			t.Errorf("%s = %s, want %s", path, got, expected)
		}
	}
	none, _, err := NormalizeWorkBuddyRequest([]byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"tool"}}],"tool_choice":"none"}`))
	if err != nil || ojson.GetBytes(none, "tools").Exists() || ojson.GetBytes(none, "tool_choice").Exists() {
		t.Fatal("tool_choice none still exposed tools to the model")
	}
}

func TestWorkBuddyToolStepsShareTurnAndConversationIDs(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"Search"}]}`)
	step := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","tool_calls":[{"id":"1"}]},{"role":"tool","content":"found"}]}`)
	next := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","content":"found"},{"role":"user","content":"Next"}]}`)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Conversation-Id": []string{"client-session"}}}
	requests := make([]*http.Request, 0, 3)
	for _, body := range [][]byte{first, step, next} {
		req, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		ApplyWorkBuddyConversationHeaders(req, body, opts)
		requests = append(requests, req)
		if req.Header.Get("X-Conversation-ID") != "client-session" || strings.Contains(req.Header.Get("X-Conversation-Request-ID"), "Search") {
			t.Fatal("conversation identity lost or message text exposed")
		}
	}
	if requests[0].Header.Get("X-Conversation-Request-ID") != requests[1].Header.Get("X-Conversation-Request-ID") || requests[0].Header.Get("X-Conversation-Request-ID") == requests[2].Header.Get("X-Conversation-Request-ID") || requests[0].Header.Get("X-Request-ID") == requests[1].Header.Get("X-Request-ID") {
		t.Fatal("tool steps or the next user turn have incorrect request identities")
	}
}

func TestWorkBuddyFingerprintToolTurnsAndConcurrentRequests(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"Search"}]}`)
	step := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","tool_calls":[{"id":"1"}]},{"role":"tool","content":"found"}]}`)
	next := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","content":"found"},{"role":"user","content":"Next"}]}`)
	auth := &coreauth.Auth{Provider: "workbuddy", Metadata: map[string]any{"uid": "uid-a", "enterprise_id": "team-a"}}
	for _, headers := range []http.Header{
		nil,
		{"X-Conversation-Id": {"client-session"}},
		{"X-Session-Id": {"client-session"}},
	} {
		opts := cliproxyexecutor.Options{Headers: headers}
		var requests []*http.Request
		for _, body := range [][]byte{first, step, next} {
			req := &http.Request{Header: make(http.Header)}
			ApplyWorkBuddyFingerprintHeaders(req, auth, body, opts)
			requests = append(requests, req)
		}
		for _, request := range requests {
			if request.Header.Get("X-Conversation-ID") != requests[0].Header.Get("X-Conversation-ID") {
				t.Fatal("tool continuation or next turn changed the account identity")
			}
		}
		if requests[0].Header.Get("X-Conversation-Request-ID") != requests[1].Header.Get("X-Conversation-Request-ID") {
			t.Fatal("tool continuation changed its upstream turn")
		}
		if requests[0].Header.Get("X-Conversation-Request-ID") == requests[2].Header.Get("X-Conversation-Request-ID") {
			t.Fatal("a new user turn reused the previous turn ID")
		}
	}
	const count = 64
	observed := make(chan http.Header, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Conversation-Id": {fmt.Sprintf("session-%d", i)}, "X-Conversation-Request-Id": {"same-caller-turn"}}}
			req := &http.Request{Header: make(http.Header)}
			ApplyWorkBuddyFingerprintHeaders(req, auth, first, opts)
			observed <- req.Header
		})
	}
	wg.Wait()
	close(observed)
	identities, turns, messages := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for headers := range observed {
		identities[headers.Get("X-Conversation-ID")] = true
		turns[headers.Get("X-Conversation-Request-ID")] = true
		messages[headers.Get("X-Request-ID")] = true
	}
	if len(identities) != 1 || len(turns) != count || len(messages) != count {
		t.Fatalf("concurrent requests: identities=%d turns=%d messages=%d", len(identities), len(turns), len(messages))
	}
}
