package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestQoderDSMLToolRecovery(t *testing.T) {
	const chat = `{"tools":[{"type":"function","function":{"name":"lookup"}}]}`
	const dsml = `<\uff5cDSML\uff5ccalls><\uff5cDSML\uff5cinvoke name="lookup"><\uff5cDSML\uff5cparameter name="query" string="true">hello</\uff5cDSML\uff5cparameter><\uff5cDSML\uff5cparameter name="count" string="false">3</\uff5cDSML\uff5cparameter></\uff5cDSML\uff5cinvoke></\uff5cDSML\uff5ccalls>`
	wire := strings.ReplaceAll(dsml, `\uff5c`, "｜")
	for _, tc := range []struct {
		name, content string
		wantCalls     int
	}{
		{"complete", "before " + wire + " after", 1},
		{"multiple", wire + wire, 2},
		{"truncated", wire[:len(wire)-5], 0},
		{"undeclared", strings.ReplaceAll(wire, "lookup", "unknown"), 0},
		{"ordinary", "Talk about DSML and compare 1 < 2.", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var text strings.Builder
			calls := 0
			finish := ""
			consumer := QoderToolEchoConsumer([]byte(chat), func(payload []byte, done bool) error {
				if done {
					return nil
				}
				for _, choice := range gjson.GetBytes(payload, "choices").Array() {
					text.WriteString(choice.Get("delta.content").String())
					for _, call := range choice.Get("delta.tool_calls").Array() {
						calls++
						if call.Get("function.name").String() != "lookup" || call.Get("id").String() == "" {
							t.Errorf("bad call: %s", call.Raw)
						}
						args := gjson.Parse(call.Get("function.arguments").String())
						if args.Get("query").String() != "hello" || args.Get("count").Type != gjson.Number || args.Get("count").Int() != 3 {
							t.Errorf("bad args: %s", args.Raw)
						}
					}
					if reason := choice.Get("finish_reason").String(); reason != "" {
						finish = reason
					}
				}
				return nil
			})
			for _, char := range tc.content {
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": string(char)}, "finish_reason": nil}}})
				if err := consumer(frame, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := consumer([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`), false); err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d, text=%q", calls, tc.wantCalls, text.String())
			}
			if calls == 0 && text.String() != tc.content {
				t.Fatalf("ordinary/malformed text changed: %q", text.String())
			}
			if calls > 0 && (finish != "tool_calls" || strings.Contains(text.String(), "DSML")) {
				t.Fatalf("finish=%s text=%q", finish, text.String())
			}
			if tc.name == "complete" && text.String() != "before  after" {
				t.Fatalf("surrounding prose changed: %q", text.String())
			}
		})
	}
}

func TestQoderGatewayFallbackBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, provider, endpoint string
		readBody                 bool
		status                   int
		want                     []string
	}{
		{"connect", "qoder", "https://api3.qoder.sh", false, 0, []string{"api3.qoder.sh", "api2.qoder.sh", "api1.qoder.sh"}},
		{"sent", "qoder", "https://api3.qoder.sh", true, 0, []string{"api3.qoder.sh"}},
		{"quota", "qoder", "https://api3.qoder.sh", false, 429, []string{"api3.qoder.sh"}},
		{"custom", "qoder", "https://custom.example", false, 0, []string{"custom.example"}},
		{"cn", "qoder-cn", "https://gateway.qoder.com.cn", false, 0, []string{"gateway.qoder.com.cn"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hosts []string
			client := &http.Client{Transport: qoderCatalogTestTransport(func(r *http.Request) (*http.Response, error) {
				hosts = append(hosts, r.URL.Host)
				if tc.readBody {
					_, _ = io.Copy(io.Discard, r.Body)
				}
				_ = r.Body.Close()
				if tc.status != 0 {
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
				}
				return nil, io.EOF
			})}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.endpoint+QoderChatPath, strings.NewReader("encoded"))
			res, _ := DoQoderGatewayRequest(client, req, &coreauth.Auth{Provider: tc.provider})
			if res != nil {
				_ = res.Body.Close()
			}
			if !reflect.DeepEqual(hosts, tc.want) {
				t.Fatalf("hosts=%v want=%v", hosts, tc.want)
			}
		})
	}
}

func qoderTestEnvelope(inner string) string {
	b, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	return "data: " + string(b) + "\n\n"
}

func TestQoderBodyUsesAccountRawCatalog(t *testing.T) {
	auth := &coreauth.Auth{ID: t.Name(), Provider: "qoder-cn"}
	other := &coreauth.Auth{ID: t.Name() + "-other", Provider: "qoder-cn"}
	for _, account := range []*coreauth.Auth{auth, other} {
		t.Cleanup(func() {
			registry.DeleteQoderModelsForClient(account.ID, account.Provider)
			registry.GetGlobalRegistry().UnregisterClient(account.ID)
		})
	}
	entries := []map[string]any{{"key": "dmodel", "display_name": "DeepSeek-Custom", "max_input_tokens": 96000, "max_output_tokens": 4096, "is_vl": false, "is_reasoning": false,
		"thinking_config": map[string]any{"enabled": map[string]any{"efforts": map[string]any{"high": map[string]any{}}}},
		"context_config":  map[string]any{"1M": map[string]any{"token_count": 1000000, "is_default": true}}, "url": "must-not-copy", "api_key": "must-not-copy", "model": "must-not-copy", "format": "must-not-copy"}}
	models, err := registry.SetQoderModelsForClient(auth.ID, auth.Provider, entries)
	if err != nil {
		t.Fatal(err)
	}
	models[0].ContextLength = 1000000
	models[0].InputTokenLimit = 777777
	models[0].DisplayName = "stale-registration"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
	if _, err = registry.SetQoderModelsForClient(other.ID, other.Provider, []map[string]any{{"key": "dmodel", "display_name": "Other account", "max_input_tokens": 12345}}); err != nil {
		t.Fatal(err)
	}
	const chat = `{"model":"configured-alias","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"result"}],"max_tokens":8000}`
	body, err := BuildQoderBody([]byte(chat), auth, "dmodel")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "model_config.max_input_tokens").Int() != 96000 || gjson.GetBytes(body, "model_config.display_name").String() != "DeepSeek-Custom" || gjson.GetBytes(body, "model_config.is_vl").Bool() || gjson.GetBytes(body, "model_config.is_reasoning").Bool() || gjson.GetBytes(body, "parameters.max_tokens").Int() != 4096 {
		t.Fatalf("raw model metadata lost: %s", body)
	}
	if gjson.GetBytes(body, "model_config").Raw != gjson.GetBytes(body, "chat_context.extra.modelConfig").Raw || strings.Contains(string(body), "must-not-copy") || !gjson.GetBytes(body, "messages.2.tool_calls").Exists() || gjson.GetBytes(body, "messages.3.role").String() != "tool" {
		t.Fatalf("native model config/history mismatch: %s", body)
	}
	if _, err = registry.SetQoderModelsForClient(auth.ID, auth.Provider, []map[string]any{}); err != nil {
		t.Fatal(err)
	}
	body, err = BuildQoderBody([]byte(chat), auth, "dmodel")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "model_config.max_input_tokens").Int() != 180000 || gjson.GetBytes(body, "model_config.display_name").String() != "dmodel" {
		t.Fatalf("empty catalog revived stale metadata: %s", body)
	}
}

func TestQoderStructuredHistoryUsesCatalogDisplayName(t *testing.T) {
	const history = `{"model":"%s","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"result"}]}`
	for _, provider := range []string{"qoder-cn", "qoder"} {
		for _, key := range []string{"dmodel", "gmodel", "qmodel"} {
			auth := &coreauth.Auth{Provider: provider}
			body, err := BuildQoderBody([]byte(fmt.Sprintf(history, key)), auth, key)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "messages.2.tool_calls").Exists(); got != (provider == "qoder-cn") {
				t.Fatalf("%s/%s structured=%v: %s", provider, key, got, body)
			}
		}
	}
}

func TestQoderBodyHistoryAndImages(t *testing.T) {
	const input = `{"model":"DeepSeek-V4-Flash","messages":[{"role":"system","content":"first"},{"role":"developer","content":"second"},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},{"role":"assistant","content":null,"reasoning_content":"reason","tool_calls":[{"id":"call-1","type":"function","function":{"name":"search","arguments":{"q":"hello"}}}]},{"role":"tool","tool_call_id":"call-1","name":"search","content":"result"}],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],"max_completion_tokens":128,"reasoning_effort":"high"}`
	for _, provider := range []string{"qoder", "qoder-cn"} {
		body, err := BuildQoderBody([]byte(input), &coreauth.Auth{Provider: provider}, "dfmodel")
		if err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string]string{"messages.0.content": "first\n\nsecond", "messages.1.content.1.image_url.url": "data:image/png;base64,AA==", "model_config.key": "dfmodel", "chat_context.extra.modelConfig.key": "dfmodel", "parameters.reasoning_effort": "high", "tools.0.function.name": "search", "session_type": "qodercli"} {
			if gjson.GetBytes(body, path).String() != want {
				t.Errorf("%s = %s", path, gjson.GetBytes(body, path))
			}
		}
		if gjson.GetBytes(body, "parameters.max_tokens").Int() != 128 || !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "messages.2.content").Type == gjson.Null {
			t.Fatal("invalid gateway parameters or null assistant")
		}
		if gjson.GetBytes(body, "request_id").String() != gjson.GetBytes(body, "chat_record_id").String() {
			t.Fatal("request/record IDs differ")
		}
		if strings.Contains(string(body), "WorkBuddy") || strings.Contains(string(body), "imageUrls\":[") {
			t.Fatal("foreign fingerprint or redundant image copy")
		}
		if provider == "qoder-cn" {
			if gjson.GetBytes(body, "messages.2.tool_calls.0.function.arguments").String() != `{"q":"hello"}` || gjson.GetBytes(body, "messages.3.tool_call_id").String() != "call-1" || gjson.GetBytes(body, "messages.2.reasoning_content").String() != "reason" {
				t.Fatal("structured tool history lost")
			}
		} else if !strings.Contains(gjson.GetBytes(body, "messages.2.content").String(), qoderToolMarker) || gjson.GetBytes(body, "messages.3.role").String() != "user" {
			t.Fatal("legacy tool history is not encoded")
		}
	}
}

func TestQoderInnerStreamToolsReasoningAndUsage(t *testing.T) {
	stream := ": heartbeat\n\n" + qoderTestEnvelope(`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"}}]}`) +
		qoderTestEnvelope(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`) +
		qoderTestEnvelope(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`) +
		qoderTestEnvelope(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"prompt_cache_hit_tokens":8,"completion_tokens_details":{"reasoning_tokens":2}}}`) + qoderTestEnvelope("[DONE]") + "event:finish{\"time\":12}\n"
	var aggregate WorkBuddyAggregate
	var frames [][]byte
	err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(stream)), "m", nil, func(payload []byte, done bool) error {
		frames = append(frames, bytes.Clone(payload))
		if done {
			return nil
		}
		return aggregate.Add(payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := aggregate.Response("m")
	for path, want := range map[string]string{"choices.0.message.reasoning_content": "think", "choices.0.message.tool_calls.0.function.arguments": `{"q":"hi"}`, "choices.0.message.tool_calls.0.function.name": "search", "choices.0.finish_reason": "tool_calls"} {
		if gjson.GetBytes(body, path).String() != want {
			t.Fatalf("%s: %s", path, body)
		}
	}
	if gjson.GetBytes(body, "usage.prompt_tokens_details.cached_tokens").Int() != 8 || gjson.GetBytes(body, "usage.total_tokens").Int() != 15 {
		t.Fatalf("usage lost: %s", body)
	}
	if string(frames[len(frames)-1]) != "[DONE]" {
		t.Fatal("missing terminal")
	}
	for _, frame := range frames {
		if gjson.GetBytes(frame, "choices.0.finish_reason").String() != "" && gjson.GetBytes(frame, "choices.0.delta.tool_calls").Exists() {
			t.Fatal("finish mixed with tool delta")
		}
	}
}

func TestQoderStreamBoundariesAndErrors(t *testing.T) {
	for _, test := range []struct {
		name, stream        string
		status              int
		credential, request bool
	}{
		{"truncated", qoderTestEnvelope(`{"choices":[{"delta":{"content":"partial"}}]}`), 502, false, false},
		{"metadata-only", "event:finish{\"time\":1}\n\n", 502, false, false},
		{"malformed", "data: {oops}\n\n", 502, false, false},
		{"outer-error", `data: {"statusCodeValue":401,"body":"expired"}` + "\n\n", 401, false, false},
		{"business-error", qoderTestEnvelope(`{"code":"112","message":"quota"}`), 429, true, false},
		{"daily", qoderTestEnvelope(`{"error":{"code":110,"message":"daily"}}`), 429, false, false},
		{"queue", `data: {"statusCodeValue":403,"body":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"retryAfterSeconds\\\":30}\"}"}` + "\n\n", 429, false, true},
		{"hidden-inner", qoderTestEnvelope(`{"error":{"type":"invalid_request_error","message":"bad tools"}}`), 400, false, true},
		{"invalid-tool-args", qoderTestEnvelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"search","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`) + qoderTestEnvelope("[DONE]"), 502, false, false},
		{"missing-tool-name", qoderTestEnvelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`) + qoderTestEnvelope("[DONE]"), 502, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(test.stream)), "m", nil, func([]byte, bool) error { return nil })
			var detail *QoderError
			if !errors.As(err, &detail) || detail.StatusCode() != test.status || detail.IsCredentialScoped() != test.credential || detail.IsRequestScoped() != test.request {
				t.Fatalf("unexpected classification: %#v", err)
			}
		})
	}
	for _, tail := range []string{"", qoderTestEnvelope("[DONE]")} {
		stream := qoderTestEnvelope(`{"choices":[{"index":0,"delta":{"content":"last"},"finish_reason":"stop"}]}`) + tail
		if err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(stream)), "m", nil, func([]byte, bool) error { return nil }); err != nil {
			t.Fatalf("valid finish boundary: %v", err)
		}
	}
}

func TestQoderToolChoiceAndLegacyFunctions(t *testing.T) {
	for _, choice := range []string{`"none"`, `{"type":"function","function":{"name":"search"}}`} {
		input := []byte(`{"messages":[{"role":"user","content":"hi"}],"functions":[{"name":"search","parameters":{"type":"object"}}],"tool_choice":` + choice + `}`)
		body, err := BuildQoderBody(input, &coreauth.Auth{Provider: "qoder"}, "m")
		if err != nil {
			t.Fatal(err)
		}
		want := "search"
		if choice == `"none"` {
			want = "none"
		}
		if gjson.GetBytes(body, "tools.0.function.name").String() != "search" || gjson.GetBytes(body, "tool_choice").String() != want || gjson.GetBytes(body, "parameters.tool_choice").String() != want {
			t.Fatalf("tool declaration/choice lost: %s", body)
		}
	}
}

func TestQoderStreamCancellationClosesReader(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	result := make(chan error, 1)
	go func() { result <- ReadQoderStream(ctx, r, "m", nil, func([]byte, bool) error { return nil }) }()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestQoderLegacyToolEchoBoundaries(t *testing.T) {
	const request = `{"tools":[{"type":"function","function":{"name":"search"}}]}`
	for _, marker := range []string{qoderToolMarker, "<assistant 请求调用工具>"} {
		for _, bad := range []bool{false, true} {
			text := "Prefix\n" + marker + `[{"name":"search","arguments":"{\"q\":\"hi\"}"}]`
			if bad {
				text = "Prefix\n" + marker + `[{"name":"search","arguments":"{"}]`
			}
			var stream strings.Builder
			for _, r := range text {
				chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": string(r)}}}})
				stream.WriteString(qoderTestEnvelope(string(chunk)))
			}
			stream.WriteString(qoderTestEnvelope(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
			stream.WriteString(qoderTestEnvelope("[DONE]"))
			var aggregate WorkBuddyAggregate
			err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(stream.String())), "m", nil, QoderToolEchoConsumer([]byte(request), func(payload []byte, done bool) error {
				if done {
					return nil
				}
				return aggregate.Add(payload)
			}))
			if bad {
				if err == nil {
					t.Fatal("invalid tool envelope was accepted")
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			out, _ := aggregate.Response("m")
			if gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String() != `{"q":"hi"}` || gjson.GetBytes(out, "choices.0.message.content").String() != "Prefix\n" || gjson.GetBytes(out, "choices.0.finish_reason").String() != "tool_calls" {
				t.Fatalf("legacy tool conversion failed: %s", out)
			}
		}
	}
}

func TestQoderUsageMergesAndErrorScopes(t *testing.T) {
	stream := qoderTestEnvelope(`{"choices":[{"delta":{"content":"112 is ordinary text"}}],"usage":{"prompt_tokens":20,"prompt_tokens_details":{"cached_tokens":7}}}`) +
		qoderTestEnvelope(`{"choices":[],"usage":{"completion_tokens":4,"completion_tokens_details":{"reasoning_tokens":2}}}`) +
		qoderTestEnvelope(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	var aggregate WorkBuddyAggregate
	if err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(stream)), "m", nil, func(payload []byte, done bool) error {
		if done {
			return nil
		}
		return aggregate.Add(payload)
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := aggregate.Response("m")
	if gjson.GetBytes(out, "usage.total_tokens").Int() != 24 || gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int() != 7 || gjson.GetBytes(out, "usage.completion_tokens_details.reasoning_tokens").Int() != 2 || gjson.GetBytes(out, "usage.completion_tokens_details.total_tokens").Exists() {
		t.Fatalf("merged usage is invalid: %s", out)
	}
	queue := QoderUpstreamError(403, nil, []byte(`{"code":"10605","message":"{\"isQueued\":true,\"retryAfterSeconds\":17}"}`))
	if queue.StatusCode() != 429 || queue.RetryAfter() == nil || queue.RetryAfter().Seconds() != 17 || !queue.IsRequestScoped() || queue.IsCredentialScoped() {
		t.Fatalf("queue classification: %#v", queue)
	}
	for _, code := range []string{"112", "110"} {
		err := QoderUpstreamError(200, nil, []byte(`{"body":"{\"code\":\"`+code+`\"}"}`))
		if err.StatusCode() != 429 || err.IsRequestScoped() || err.IsCredentialScoped() != (code == "112") {
			t.Fatalf("quota classification: %#v", err)
		}
	}
}

func TestQoderStreamPartialChoiceAndMetadata(t *testing.T) {
	for _, stream := range []string{
		qoderTestEnvelope(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{"content":"still running"}}]}`),
		qoderTestEnvelope(`{"choices":[{"delta":{"content":"partial"}}]}`) + "event:finish{\"time\":1}\n",
	} {
		if err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(stream)), "m", nil, func([]byte, bool) error { return nil }); err == nil {
			t.Fatal("metadata or one choice concealed truncated completion")
		}
	}
	stream := strings.ReplaceAll(qoderTestEnvelope(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)+qoderTestEnvelope("[DONE]"), "\n\n", "\n")
	if err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(stream)), "m", nil, func([]byte, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

const qoderDSMLTestRequest = `{"tools":[{"type":"function","function":{"name":"search"}},{"type":"function","function":{"name":"read_file"}}]}`
const qoderDSMLTestCall = `<｜DSML｜invoke name="search"><｜DSML｜parameter name="q" string="true">中文🙂</｜DSML｜parameter></｜DSML｜invoke>`
const qoderDSMLTestBlock = `<｜DSML｜function_calls>` + qoderDSMLTestCall + `</｜DSML｜function_calls>`

func qoderTestContentFrame(text string) []byte {
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
	return frame
}

func qoderTestRecoverText(t *testing.T, request, text string, split bool) ([]byte, [][]byte) {
	t.Helper()
	var stream strings.Builder
	if split {
		for _, r := range text {
			stream.WriteString(qoderTestEnvelope(string(qoderTestContentFrame(string(r)))))
		}
	} else {
		stream.WriteString(qoderTestEnvelope(string(qoderTestContentFrame(text))))
	}
	stream.WriteString(qoderTestEnvelope(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	stream.WriteString(qoderTestEnvelope("[DONE]"))
	var aggregate WorkBuddyAggregate
	var frames [][]byte
	err := ReadQoderStream(t.Context(), io.NopCloser(strings.NewReader(stream.String())), "m", nil, QoderToolEchoConsumer([]byte(request), func(payload []byte, done bool) error {
		frames = append(frames, bytes.Clone(payload))
		if done {
			return nil
		}
		return aggregate.Add(payload)
	}))
	if err != nil {
		t.Fatal(err)
	}
	out, err := aggregate.Response("m")
	if err != nil {
		t.Fatal(err)
	}
	return out, frames
}

func TestQoderDSMLRecovery(t *testing.T) {
	if calls, clean, err := qoderRecoverDSML(qoderDSMLTestBlock, map[string]bool{"search": true, "read_file": true}); err != nil || len(calls) != 1 || clean != "" {
		t.Fatalf("direct DSML parse: calls=%v clean=%q err=%v", calls, clean, err)
	}
	const params = `<｜DSML｜parameter name="obj" string="false">{"城市":"上海","n":9007199254740993}</｜DSML｜parameter>` +
		`<｜DSML｜parameter name="array" string="false">[true,null,2]</｜DSML｜parameter>` +
		`<｜DSML｜parameter name="bool" string="false">false</｜DSML｜parameter>` +
		`<｜DSML｜parameter name="null" string="false">null</｜DSML｜parameter>` +
		`<｜DSML｜parameter name="number" string="false">12.5</｜DSML｜parameter>` +
		`<｜DSML｜parameter name="literal" string="true">{"a":true}</｜DSML｜parameter>` +
		`<｜DSML｜parameter name="文本" string="true">  中🙂 &amp; <tag>｜DSML｜ <｜DSML｜invoke name="literal">` + "\r\n  " + `</｜DSML｜parameter>`
	const value = "  中🙂 &amp; <tag>｜DSML｜ <｜DSML｜invoke name=\"literal\">\r\n  "
	invoke := `<｜DSML｜invoke name="search">` + params + `</｜DSML｜invoke>`
	for _, shape := range []struct{ name, body string }{
		{"wrapped", `<｜DSML｜function_calls>` + invoke + `<｜DSML｜invoke name='read_file'><｜DSML｜parameter name='path'>文件.go</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜function_calls>`},
		{"bare", invoke + `<｜DSML｜invoke name='read_file'><｜DSML｜parameter name='path'>文件.go</｜DSML｜parameter></｜DSML｜invoke>`},
		{"ascii", strings.ReplaceAll(`<｜DSML｜calls>`+invoke+`<｜DSML｜invoke name='read_file'><｜DSML｜parameter name='path'>文件.go</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜calls>`, "｜DSML｜", "|DSML|")},
	} {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split=%v", shape.name, split), func(t *testing.T) {
				const prefix, suffix = " \t前文\r\n", "\r\n后文 \t"
				out, frames := qoderTestRecoverText(t, qoderDSMLTestRequest, prefix+shape.body+suffix, split)
				if gjson.GetBytes(out, "choices.0.message.content").String() != prefix+suffix || gjson.GetBytes(out, "choices.0.finish_reason").String() != "tool_calls" {
					t.Fatalf("text/finish changed: %s", out)
				}
				calls := gjson.GetBytes(out, "choices.0.message.tool_calls").Array()
				if len(calls) != 2 || calls[0].Get("function.name").String() != "search" || calls[1].Get("function.name").String() != "read_file" {
					t.Fatalf("calls = %s", out)
				}
				wantValue := value
				if shape.name == "ascii" {
					wantValue = strings.ReplaceAll(value, "｜DSML｜", "|DSML|")
				}
				args := gjson.Parse(calls[0].Get("function.arguments").String())
				if args.Get("obj.n").Raw != "9007199254740993" || args.Get("obj.城市").String() != "上海" || args.Get("array").Raw != "[true,null,2]" || args.Get("bool").Type != gjson.False || args.Get("null").Type != gjson.Null || args.Get("number").Raw != "12.5" || args.Get("literal").Type != gjson.String || args.Get("literal").String() != `{"a":true}` || args.Get("文本").String() != wantValue {
					t.Fatalf("typed/raw arguments changed: %s", args)
				}
				if gjson.Get(calls[1].Get("function.arguments").String(), "path").String() != "文件.go" {
					t.Fatalf("second call arguments = %s", calls[1])
				}
				ids := map[string]bool{}
				for _, call := range calls {
					id := call.Get("id").String()
					if !strings.HasPrefix(id, "call_") || len(id) <= 5 || ids[id] || call.Get("type").String() != "function" {
						t.Fatalf("invalid call identity: %s", call)
					}
					ids[id] = true
				}
				var streamed []gjson.Result
				for _, frame := range frames {
					choice := gjson.GetBytes(frame, "choices.0")
					for _, call := range choice.Get("delta.tool_calls").Array() {
						if choice.Get("finish_reason").String() != "" || choice.Get("delta.content").Exists() || !json.Valid([]byte(call.Get("function.arguments").String())) {
							t.Fatalf("mixed or incomplete tool frame: %s", frame)
						}
						streamed = append(streamed, call)
					}
				}
				if len(streamed) != 2 || streamed[0].Get("index").Int() != 0 || streamed[1].Get("index").Int() != 1 || streamed[0].Get("id").String() != calls[0].Get("id").String() || streamed[1].Get("function.arguments").String() != calls[1].Get("function.arguments").String() {
					t.Fatalf("stream/aggregate mismatch: %v", streamed)
				}
			})
		}
	}
}

func TestQoderDSMLFailOpenAndOrdinaryText(t *testing.T) {
	for name, text := range map[string]string{
		"ordinary":             " \tDSML invoke(parameter): <xml>中文🙂 &\r\n",
		"inline-code":          "Example `" + qoderDSMLTestBlock + "` stays literal.",
		"fenced-code":          "Example:\n```xml\n" + qoderDSMLTestBlock + "\n```\nEnd",
		"tilde-code":           "~~~xml\n" + qoderDSMLTestBlock + "\n~~~",
		"empty":                `<｜DSML｜function_calls></｜DSML｜function_calls>`,
		"undeclared":           strings.ReplaceAll(qoderDSMLTestBlock, `name="search"`, `name="undeclared"`),
		"mixed-allowlist":      `<｜DSML｜function_calls>` + qoderDSMLTestCall + `<｜DSML｜invoke name="undeclared"></｜DSML｜invoke></｜DSML｜function_calls>`,
		"invalid-json":         strings.ReplaceAll(qoderDSMLTestBlock, `string="true">中文🙂`, `string="false">{oops`),
		"invalid-bool":         strings.ReplaceAll(qoderDSMLTestBlock, `string="true"`, `string="maybe"`),
		"mismatch":             strings.ReplaceAll(qoderDSMLTestBlock, `</｜DSML｜invoke>`, `</｜DSML｜calls>`),
		"duplicate-param":      `<｜DSML｜invoke name="search"><｜DSML｜parameter name="q">a</｜DSML｜parameter><｜DSML｜parameter name="q">b</｜DSML｜parameter></｜DSML｜invoke>`,
		"duplicate-attribute":  strings.ReplaceAll(qoderDSMLTestBlock, `name="search"`, `name="search" name="read_file"`),
		"unexpected-text":      strings.ReplaceAll(qoderDSMLTestBlock, `<｜DSML｜invoke`, `unparsed<｜DSML｜invoke`),
		"unknown-tag":          `<｜DSML｜function_calls><｜DSML｜example/></｜DSML｜function_calls>`,
		"partial-root":         `<｜DSML｜function_calls>` + qoderDSMLTestCall,
		"partial-invoke":       `<｜DSML｜invoke name="search"><｜DSML｜parameter name="q">text</｜DSML｜parameter>`,
		"partial-marker":       "text <｜DS",
		"valid-then-truncated": qoderDSMLTestBlock + `<｜DSML｜invoke name="search">`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, split := range []bool{false, true} {
				out, _ := qoderTestRecoverText(t, qoderDSMLTestRequest, text, split)
				if gjson.GetBytes(out, "choices.0.message.content").String() != text || len(gjson.GetBytes(out, "choices.0.message.tool_calls").Array()) != 0 || gjson.GetBytes(out, "choices.0.finish_reason").String() != "stop" {
					t.Fatalf("fail-open/text preservation failed (split=%v): %s", split, out)
				}
			}
		})
	}
	for _, request := range []string{`{}`, `{"tools":[]}`} {
		out, _ := qoderTestRecoverText(t, request, qoderDSMLTestBlock, true)
		if gjson.GetBytes(out, "choices.0.message.content").String() != qoderDSMLTestBlock {
			t.Fatalf("undeclared recovery: %s", out)
		}
	}
}

func TestQoderDSMLMultipleBlocksAndCode(t *testing.T) {
	text := "pre\n" + qoderDSMLTestBlock + "\n`" + qoderDSMLTestBlock + "`\n" + qoderDSMLTestCall + "\npost"
	out, _ := qoderTestRecoverText(t, qoderDSMLTestRequest, text, true)
	if gjson.GetBytes(out, "choices.0.message.content").String() != "pre\n\n`"+qoderDSMLTestBlock+"`\n\npost" || len(gjson.GetBytes(out, "choices.0.message.tool_calls").Array()) != 2 {
		t.Fatalf("multiple blocks/code: %s", out)
	}
}

func TestQoderDSMLWaitsForFinishAndNativeWins(t *testing.T) {
	for _, nativePosition := range []string{"none", "before", "after", "same"} {
		t.Run(nativePosition, func(t *testing.T) {
			var aggregate WorkBuddyAggregate
			toolFrames := 0
			consumer := QoderToolEchoConsumer([]byte(qoderDSMLTestRequest), func(payload []byte, done bool) error {
				if done {
					return nil
				}
				toolFrames += len(gjson.GetBytes(payload, "choices.0.delta.tool_calls").Array())
				return aggregate.Add(payload)
			})
			feed := func(payload []byte) {
				t.Helper()
				if err := consumer(payload, false); err != nil {
					t.Fatal(err)
				}
			}
			native := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"native-id","type":"function","function":{"name":"search","arguments":"{\"q\":\"native\"}"}}]}}]}`)
			if nativePosition == "before" {
				feed(native)
			}
			if nativePosition == "same" {
				var frame map[string]any
				_ = json.Unmarshal(native, &frame)
				frame["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"] = qoderDSMLTestBlock
				payload, _ := json.Marshal(frame)
				feed(payload)
			} else {
				feed(qoderTestContentFrame(qoderDSMLTestBlock))
			}
			if (nativePosition == "none" || nativePosition == "after") && toolFrames != 0 {
				t.Fatal("recovery emitted before terminal/native decision")
			}
			if nativePosition == "after" {
				feed(native)
			}
			feed([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
			out, err := aggregate.Response("m")
			if err != nil {
				t.Fatal(err)
			}
			if toolFrames != 1 {
				t.Fatalf("duplicate or missing tools: %s", out)
			}
			if nativePosition != "none" && (gjson.GetBytes(out, "choices.0.message.content").String() != qoderDSMLTestBlock || gjson.GetBytes(out, "choices.0.message.tool_calls.0.id").String() != "native-id" || gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String() != `{"q":"native"}`) {
				t.Fatalf("native call/text changed: %s", out)
			}
		})
	}
}

func TestQoderDSMLTailAndPendingLimit(t *testing.T) {
	var text strings.Builder
	consumer := QoderToolEchoConsumer([]byte(qoderDSMLTestRequest), func(payload []byte, _ bool) error {
		text.WriteString(gjson.GetBytes(payload, "choices.0.delta.content").String())
		return nil
	})
	for _, piece := range []string{"ordinary ", "text <｜DS", "X stays text\n"} {
		if err := consumer(qoderTestContentFrame(piece), false); err != nil {
			t.Fatal(err)
		}
	}
	if text.String() != "ordinary text <｜DSX stays text\n" {
		t.Fatalf("ordinary text buffered or changed: %q", text.String())
	}
	if err := consumer(qoderTestContentFrame("prefix <｜DS"), false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(text.String(), "prefix ") {
		t.Fatalf("DSML prefix leaked: %q", text.String())
	}
	if err := consumer([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`), false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(text.String(), "prefix <｜DS") {
		t.Fatalf("tail dropped: %q", text.String())
	}
	consumer = QoderToolEchoConsumer([]byte(qoderDSMLTestRequest), func([]byte, bool) error { return nil })
	if err := consumer(qoderTestContentFrame(`<｜DSML｜function_calls>`), false); err != nil {
		t.Fatal(err)
	}
	err := consumer(qoderTestContentFrame(strings.Repeat("x", 4<<20)), false)
	var protocol *QoderError
	if !errors.As(err, &protocol) || protocol.StatusCode() != 502 {
		t.Fatalf("pending cap = %v", err)
	}
}

type qoderWireTestTransport func(*http.Request) (*http.Response, error)

func (f qoderWireTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDoQoderGatewayRequestFallbackOrder(t *testing.T) {
	for _, success := range []int{1, 2, 3, 0} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			var hosts []string
			connectErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
			client := &http.Client{Transport: qoderWireTestTransport(func(req *http.Request) (*http.Response, error) {
				hosts = append(hosts, req.URL.Host)
				defer func() { _ = req.Body.Close() }()
				if req.Method != http.MethodPost || req.URL.RequestURI() != "/generate?x=1" || req.Header.Get("X-Test") != "same" {
					t.Fatalf("request changed: %v", req)
				}
				if len(hosts) != success {
					return nil, connectErr
				}
				body, err := io.ReadAll(req.Body)
				if err != nil || string(body) != "paid-body" {
					t.Fatalf("body = %q, %v", body, err)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
			})}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api3.qoder.sh/generate?x=1", strings.NewReader("paid-body"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Test", "same")
			response, err := DoQoderGatewayRequest(client, req, &coreauth.Auth{Provider: "qoder"})
			if success == 0 {
				if response != nil || !errors.Is(err, connectErr) {
					t.Fatalf("exhausted = %v, %v", response, err)
				}
			} else {
				if err != nil || response == nil {
					t.Fatalf("fallback = %v, %v", response, err)
				}
				_ = response.Body.Close()
			}
			want := []string{"api3.qoder.sh", "api2.qoder.sh", "api1.qoder.sh"}
			if success > 0 {
				want = want[:success]
			}
			if !reflect.DeepEqual(hosts, want) || req.URL.Host != "api3.qoder.sh" {
				t.Fatalf("hosts = %v; original = %s", hosts, req.URL.Host)
			}
		})
	}
}

func TestDoQoderGatewayRequestNoReplay(t *testing.T) {
	for _, name := range []string{"cn", "custom", "http", "no-getbody", "non-network", "canceled", "deadline", "pre-canceled", "cancel-during", "body-read", "wrote-request", "write-error", "response-byte", "http-400", "http-401", "http-403", "http-429", "http-500", "http-503"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			provider, endpoint := "qoder", "https://api3.qoder.sh/generate"
			if name == "cn" {
				provider = "qoder-cn"
			}
			if name == "custom" {
				endpoint = "https://custom.example/generate"
			}
			if name == "http" {
				endpoint = "http://api3.qoder.sh/generate"
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("paid-body"))
			if err != nil {
				t.Fatal(err)
			}
			if name == "no-getbody" {
				req.GetBody = nil
			}
			if name == "pre-canceled" {
				cancel()
			}
			attempts := 0
			var wantErr error = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNRESET}
			if name == "non-network" {
				wantErr = errors.New("permanent transport error")
			}
			if name == "canceled" || name == "pre-canceled" || name == "cancel-during" {
				wantErr = context.Canceled
			}
			if name == "deadline" {
				wantErr = context.DeadlineExceeded
			}
			status := 0
			if strings.HasPrefix(name, "http-") {
				_, _ = fmt.Sscanf(name, "http-%d", &status)
			}
			client := &http.Client{Transport: qoderWireTestTransport(func(attempt *http.Request) (*http.Response, error) {
				attempts++
				defer func() { _ = attempt.Body.Close() }()
				trace := httptrace.ContextClientTrace(attempt.Context())
				switch name {
				case "body-read":
					var b [1]byte
					if n, errRead := attempt.Body.Read(b[:]); n != 1 || errRead != nil {
						t.Fatalf("read = %d, %v", n, errRead)
					}
				case "wrote-request":
					trace.WroteRequest(httptrace.WroteRequestInfo{})
				case "write-error":
					trace.WroteRequest(httptrace.WroteRequestInfo{Err: wantErr})
				case "response-byte":
					trace.GotFirstResponseByte()
				case "cancel-during":
					cancel()
				}
				if status > 0 {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("error")), Request: attempt}, nil
				}
				return nil, wantErr
			})}
			response, err := DoQoderGatewayRequest(client, req, &coreauth.Auth{Provider: provider})
			wantAttempts := 1
			if name == "pre-canceled" {
				wantAttempts = 0
			}
			if attempts != wantAttempts {
				t.Fatalf("request replayed: %d attempts", attempts)
			}
			if status > 0 {
				if err != nil || response == nil || response.StatusCode != status {
					t.Fatalf("HTTP response = %v, %v", response, err)
				}
				_ = response.Body.Close()
			} else if response != nil || !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, %v", response, err)
			}
		})
	}
}
