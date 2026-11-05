package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func cacheRewriteGet(data []byte, path string) ojson.Result {
	return ojson.GetBytesNoCopy(data, path)
}

func rewriteConfig(minimum float64, maximum *float64) *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{CacheTokenRewrite: &sdkconfig.CacheTokenRewriteConfig{Enabled: true, MinPercent: &minimum, MaxPercent: maximum}}
}

func TestCacheTokenRewriteFormats(t *testing.T) {
	for _, tc := range []struct {
		name, format, input, path string
		want                      map[string]int64
	}{
		{"chat", "openai", `{"usage":{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":98,"cache_write_tokens":10,"cached_creation_tokens":10}}}`, "usage", map[string]int64{"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130, "prompt_tokens_details.cached_tokens": 95, "prompt_tokens_details.cache_write_tokens": 0, "prompt_tokens_details.cached_creation_tokens": 0}},
		{"responses", "openai-response", `{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":30,"total_tokens":130,"input_tokens_details":{"cached_tokens":98},"output_tokens_details":{"reasoning_tokens":8}}}}`, "response.usage", map[string]int64{"input_tokens": 100, "output_tokens": 30, "total_tokens": 130, "input_tokens_details.cached_tokens": 95, "output_tokens_details.reasoning_tokens": 8}},
		{"claude", "claude", `{"usage":{"input_tokens":1,"cache_read_input_tokens":98,"cache_creation_input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":0},"output_tokens":30}}`, "usage", map[string]int64{"input_tokens": 5, "cache_read_input_tokens": 95, "cache_creation_input_tokens": 0, "cache_creation.ephemeral_5m_input_tokens": 0, "cache_creation.ephemeral_1h_input_tokens": 0, "output_tokens": 30}},
		{"gemini", "gemini", `{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":98,"candidatesTokenCount":30,"totalTokenCount":130}}`, "usageMetadata", map[string]int64{"promptTokenCount": 100, "cachedContentTokenCount": 95, "candidatesTokenCount": 30, "totalTokenCount": 130}},
		{"antigravity", "antigravity", `{"response":{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":98,"candidatesTokenCount":30,"totalTokenCount":130}}}`, "response.usageMetadata", map[string]int64{"promptTokenCount": 100, "cachedContentTokenCount": 95, "candidatesTokenCount": 30}},
		{"interactions", "interactions", `{"interaction":{"usage":{"total_input_tokens":100,"total_cached_tokens":98,"total_output_tokens":30}}}`, "interaction.usage", map[string]int64{"total_input_tokens": 100, "total_cached_tokens": 95, "total_output_tokens": 30}},
		{"interactions-finish", "interactions", `{"event_type":"finish","metadata":{"total_usage":{"total_input_tokens":100,"total_cached_tokens":98,"total_output_tokens":30}}}`, "metadata.total_usage", map[string]int64{"total_input_tokens": 100, "total_cached_tokens": 95, "total_output_tokens": 30}},
		{"zero", "openai", `{"usage":{"prompt_tokens":0,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}}`, "usage", map[string]int64{"prompt_tokens_details.cached_tokens": 0, "completion_tokens": 2}},
		{"chat-with-cache-aliases", "openai", `{"usage":{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":98},"cached_tokens":98,"cache_read_input_tokens":98,"cache_creation_input_tokens":10}}`, "usage", map[string]int64{"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130, "prompt_tokens_details.cached_tokens": 95, "cached_tokens": 95, "cache_read_input_tokens": 95, "cache_creation_input_tokens": 0}},
		{"responses-with-cache-aliases", "openai-response", `{"response":{"usage":{"input_tokens":100,"output_tokens":30,"total_tokens":130,"input_tokens_details":{"cached_tokens":98},"cache_read_input_tokens":98,"cache_creation_input_tokens":10}}}`, "response.usage", map[string]int64{"input_tokens": 100, "output_tokens": 30, "total_tokens": 130, "input_tokens_details.cached_tokens": 95, "cache_read_input_tokens": 95, "cache_creation_input_tokens": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.input)
			r := newCacheTokenRewriter(rewriteConfig(95, nil), tc.format)
			output := r.rewriteJSON(input)
			if !json.Valid(output) || string(input) != tc.input {
				t.Fatal("invalid JSON or mutated upstream bytes")
			}
			for path, want := range tc.want {
				if got := cacheRewriteGet(output, tc.path+"."+path); !got.Exists() || got.Int() != want {
					t.Errorf("%s = %s, want %d; %s", path, got.Raw, want, output)
				}
			}
		})
	}
}

func TestCacheTokenRewriteFixedRandomAndDisabled(t *testing.T) {
	input := []byte(`{"usage":{"input_tokens":101,"output_tokens":7,"input_tokens_details":{"cached_tokens":101}}}`)
	for _, percent := range []float64{0, 33.3, 95, 100} {
		r := newCacheTokenRewriter(rewriteConfig(percent, nil), "openai-response")
		got := cacheRewriteGet(r.rewriteJSON(input), "usage.input_tokens_details.cached_tokens").Int()
		if want := int64(101 * percent / 100); got != want {
			t.Fatalf("percent %v: got %d want %d", percent, got, want)
		}
	}
	cfg := rewriteConfig(80, new(95.0))
	r := newCacheTokenRewriter(cfg, "openai-response")
	first := r.rewriteJSON([]byte(`{"usage":{"input_tokens":100000,"output_tokens":7,"input_tokens_details":{"cached_tokens":100000}}}`))
	cache := cacheRewriteGet(first, "usage.input_tokens_details.cached_tokens").Int()
	if cache < 80000 || cache > 95000 {
		t.Fatalf("random ratio out of range: %d", cache)
	}
	*cfg.CacheTokenRewrite.MinPercent = 0
	*cfg.CacheTokenRewrite.MaxPercent = 1
	for range 10 {
		got := r.rewriteJSON([]byte(`{"usage":{"input_tokens":100000,"output_tokens":7,"input_tokens_details":{"cached_tokens":100000}}}`))
		if !bytes.Equal(first, got) {
			t.Fatalf("random ratio changed within a request or after config change: ratio=%v first=%s got=%s", r.ratio, first, got)
		}
	}
	for _, cfg := range []*sdkconfig.SDKConfig{nil, {}, {CacheTokenRewrite: &sdkconfig.CacheTokenRewriteConfig{Enabled: false}}} {
		if newCacheTokenRewriter(cfg, "openai") != nil {
			t.Fatal("disabled rewrite was active")
		}
	}
	for _, payload := range []string{`{"error":{"message":"oops"}}`, `{"usage":{"output_tokens":7}}`, `{"choices":[{"message":{"content":"{\"usage\":{\"input_tokens\":100}}"}}]}`} {
		if got := r.rewriteJSON([]byte(payload)); string(got) != payload {
			t.Fatalf("rewrote non-usage content: %s", got)
		}
	}
}

func TestCacheTokenRewriteOnlyReducesExistingCachedReads(t *testing.T) {
	for _, test := range []struct {
		name, format, usage string
	}{
		{"chat-no-read", "openai", `{"prompt_tokens":100,"completion_tokens":9}`},
		{"chat-read-field-absent", "openai", `{"prompt_tokens":100,"completion_tokens":9,"prompt_tokens_details":{"cache_write_tokens":9}}`},
		{"chat-read-below-target", "openai", `{"prompt_tokens":100,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":94}}`},
		{"chat-read-equal-target", "openai", `{"prompt_tokens":100,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":95}}`},
		{"responses-no-read", "openai-response", `{"input_tokens":100,"output_tokens":9}`},
		{"responses-read-below-target", "openai-response", `{"input_tokens":100,"output_tokens":9,"input_tokens_details":{"cached_tokens":94}}`},
		{"claude-no-read", "claude", `{"input_tokens":100,"cache_creation_input_tokens":0}`},
		{"claude-read-below-target", "claude", `{"input_tokens":91,"cache_read_input_tokens":9,"cache_creation_input_tokens":0}`},
		{"gemini-no-read", "gemini", `{"promptTokenCount":100,"candidatesTokenCount":9}`},
		{"gemini-read-below-target", "gemini", `{"promptTokenCount":100,"cachedContentTokenCount":94}`},
		{"interactions-no-read", "interactions", `{"total_input_tokens":100,"total_output_tokens":9}`},
		{"interactions-read-below-target", "interactions", `{"total_input_tokens":100,"total_cached_tokens":94}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputDoc := ojson.NewDoc([]byte(`{"usage":{}}`))
			if err := inputDoc.SetRawBytes("usage", []byte(test.usage)); err != nil {
				t.Fatal(err)
			}
			input := inputDoc.Bytes()
			got := newCacheTokenRewriter(rewriteConfig(95, nil), test.format).rewriteJSON(input)
			if !bytes.Equal(got, input) {
				t.Fatalf("usage without excess existing cache read was rewritten: %s", got)
			}
		})
	}
}

func TestCacheTokenRewriteSplitStreamKeepsClaudeTotals(t *testing.T) {
	wire := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":98,\"cache_creation_input_tokens\":1,\"output_tokens\":0}}}\r\n\r\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	r := newCacheTokenRewriter(rewriteConfig(95, nil), "claude")
	var result []byte
	for i := 0; i < len(wire); i++ {
		result = append(result, r.rewriteChunk([]byte(wire[i:i+1]))...)
	}
	result = append(result, r.finish()...)
	starts, stops := 0, 0
	for _, line := range strings.Split(string(result), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if !json.Valid([]byte(data)) {
			t.Fatalf("corrupted SSE: %s", line)
		}
		frame := ojson.Parse(data)
		path := ""
		switch frame.Get("type").String() {
		case "message_start":
			starts++
			path = "message.usage"
		case "message_delta":
			path = "usage"
		case "message_stop":
			stops++
		}
		if path != "" {
			u := frame.Get(path)
			if u.Get("input_tokens").Int() != 5 || u.Get("cache_read_input_tokens").Int() != 95 || u.Get("cache_creation_input_tokens").Int() != 0 {
				t.Fatalf("inconsistent Claude usage: %s", data)
			}
		}
	}
	if starts != 1 || stops != 1 || !bytes.Contains(result, []byte(`"text":"hello"`)) {
		t.Fatalf("lost or duplicated stream: %s", result)
	}
}

func TestCacheTokenRewriteDoesNotChangeInternalUsage(t *testing.T) {
	const provider = "cache-rewrite-test-provider"
	plugin := newCapturePluginExecutorUsagePlugin(provider)
	registerUsagePluginForTest(t, "cache-rewrite-original-usage", plugin)
	upstream := []byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":98}}}`)
	host := &mockPluginUsageHost{execResp: coreexecutor.Response{Payload: upstream}}
	host.hasRouters = true
	host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
		return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: provider}, true
	}
	h := NewBaseAPIHandlers(rewriteConfig(95, nil), nil)
	h.SetModelRouterHost(host)
	body, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "openai", "test-model", []byte(`{"model":"test-model"}`), "")
	if errMsg != nil {
		t.Fatal(errMsg)
	}
	if cacheRewriteGet(body, "usage.prompt_tokens_details.cached_tokens").Int() != 95 {
		t.Fatalf("downstream not rewritten: %s", body)
	}
	record := plugin.waitRecord(t)
	if record.Detail.InputTokens != 100 || record.Detail.CachedTokens != 98 || record.Detail.OutputTokens != 10 {
		t.Fatalf("internal usage was overwritten: %+v", record.Detail)
	}
	if cacheRewriteGet(upstream, "usage.prompt_tokens_details.cached_tokens").Int() != 98 {
		t.Fatal("source buffer changed")
	}
	internal, errInternal := h.ExecuteModel(context.Background(), ModelExecutionRequest{EntryProtocol: "openai", Model: "test-model", Body: []byte(`{"model":"test-model"}`)})
	if errInternal != nil || cacheRewriteGet(internal.Body, "usage.prompt_tokens_details.cached_tokens").Int() != 98 {
		t.Fatalf("internal model callback rewritten: %s %+v", internal.Body, errInternal)
	}
}

func TestCacheTokenRewriteScannerLines(t *testing.T) {
	for _, format := range []string{"openai-response", "claude"} {
		t.Run(format, func(t *testing.T) {
			r := newCacheTokenRewriter(rewriteConfig(95, nil), format)
			startType, endType, usagePath := "response.created", "response.completed", "response.usage.input_tokens_details.cached_tokens"
			start := `{"type":"response.created","response":{"id":"r1"}}`
			end := `{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":100,"output_tokens":9,"input_tokens_details":{"cached_tokens":98}}}}`
			if format == "claude" {
				startType, endType, usagePath = "message_start", "message_delta", "usage.cache_read_input_tokens"
				start = `{"type":"message_start","message":{"usage":{"input_tokens":1,"cache_read_input_tokens":98,"cache_creation_input_tokens":1,"output_tokens":0}}}`
				end = `{"type":"message_delta","usage":{"output_tokens":9}}`
			}
			for _, event := range []struct{ name, body string }{{startType, start}, {endType, end}} {
				r.rewriteChunk([]byte("event: " + event.name))
				out := r.rewriteChunk([]byte("data: " + event.body))
				data, ok := sseJSONValidationDataPayload(out)
				if !ok || !json.Valid(data) || cacheRewriteGet(data, "type").String() != event.name {
					t.Fatalf("event %s was buffered or corrupted: %q", event.name, out)
				}
				if event.name == endType && cacheRewriteGet(data, usagePath).Int() != 95 {
					t.Fatalf("usage was not rewritten: %s", data)
				}
			}
			if tail := r.finish(); len(tail) != 0 {
				t.Fatalf("completed stream retained bytes: %q", tail)
			}
		})

	}
}
