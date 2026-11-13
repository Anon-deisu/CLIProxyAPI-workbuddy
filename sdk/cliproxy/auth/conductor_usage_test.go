package auth

import (
	"context"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestContextWithRequestedModelAliasIncludesStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream_false", true: "stream_true"}[stream], func(t *testing.T) {
			ctx := contextWithRequestedModelAlias(context.Background(), cliproxyexecutor.Options{
				Stream: stream,
			}, "fallback-model")

			if got := coreusage.StreamFromContext(ctx); got != stream {
				t.Fatalf("stream = %v, want %v", got, stream)
			}
		})
	}
}

func TestContextWithRequestedModelAliasIncludesReasoningEffort(t *testing.T) {
	ctx := contextWithRequestedModelAlias(context.Background(), cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey:  "client-model",
			cliproxyexecutor.ReasoningEffortMetadataKey: "medium",
			cliproxyexecutor.ServiceTierMetadataKey:     "auto",
			cliproxyexecutor.GenerateMetadataKey:        false,
		},
	}, "fallback-model")

	if got := coreusage.RequestedModelAliasFromContext(ctx); got != "client-model" {
		t.Fatalf("requested model alias = %q, want %q", got, "client-model")
	}
	if got := coreusage.ReasoningEffortFromContext(ctx); got != "medium" {
		t.Fatalf("reasoning effort = %q, want %q", got, "medium")
	}
	gotServiceTier := coreusage.ServiceTierFromContext(ctx)
	if gotServiceTier != "auto" {
		t.Fatalf("service tier = %q, want %q", gotServiceTier, "auto")
	}
	if got := coreusage.GenerateFromContext(ctx); got {
		t.Fatalf("generate = %v, want false", got)
	}
}

func TestContextWithRequestedModelAliasDefaultsGenerateTrue(t *testing.T) {
	ctx := contextWithRequestedModelAlias(context.Background(), cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "client-model",
		},
	}, "fallback-model")

	if got := coreusage.GenerateFromContext(ctx); !got {
		t.Fatalf("generate = %v, want true", got)
	}
}

func TestContextWithRequestedModelAliasPreservesExistingGenerateFalse(t *testing.T) {
	ctx := coreusage.WithGenerate(context.Background(), false)
	ctx = contextWithRequestedModelAlias(ctx, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "client-model",
		},
	}, "fallback-model")

	if got := coreusage.GenerateFromContext(ctx); got {
		t.Fatalf("generate = %v, want false", got)
	}
}

func TestContextWithRequestedModelAliasPropagatesAndClearsSelectionID(t *testing.T) {
	ctx := contextWithRequestedModelAlias(context.Background(), cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SelectionIDMetadataKey: "sel-123",
		},
	}, "fallback-model")
	if got := coreusage.SelectionIDFromContext(ctx); got != "sel-123" {
		t.Fatalf("SelectionIDFromContext = %q, want sel-123", got)
	}

	// Explicit empty key in metadata clears an existing context SelectionID.
	clearedCtx := contextWithRequestedModelAlias(ctx, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SelectionIDMetadataKey: "",
		},
	}, "fallback-model")
	if got := coreusage.SelectionIDFromContext(clearedCtx); got != "" {
		t.Fatalf("SelectionIDFromContext after clear = %q, want empty", got)
	}
}

func TestItem14StreamCompletedContracts(t *testing.T) {
	for _, tc := range []struct {
		name, payload                  string
		completed, incomplete, execute bool
	}{
		{"delta", `{"type":"response.output_text.delta","delta":"response.completed error usage [DONE]"}`, false, false, false},
		{"completed", `{"type":"response.completed"}`, true, false, true},
		{"done", `{"type":"response.done","response":{"status":"completed"}}`, true, false, true},
		{"missing-error", `{"type":"message_stop"}`, true, false, true},
		{"null-error", `{"type":"message_stop","error":null}`, true, false, true},
		{"empty-error", `{"type":"message_stop","error":{}}`, false, true, false},
		{"empty-string-error", `{"type":"message_stop","error":""}`, false, true, false},
		{"false-error", `{"type":"message_stop","error":false}`, false, true, false},
		{"zero-error", `{"type":"message_stop","error":0}`, false, true, false},
		{"nested-null", `{"type":"response.completed","response":{"error":null,"incomplete_details":null}}`, true, false, true},
		{"nested-error", `{"type":"response.completed","response":{"error":{}}}`, true, true, false},
		{"nested-incomplete", `{"type":"response.completed","response":{"incomplete_details":{}}}`, true, true, false},
		{"root-incomplete", `{"type":"response.completed","incomplete_details":{}}`, true, false, false},
		{"status-override", `{"status":"failed","response":{"status":" COMPLETED "}}`, false, false, true},
		{"status-fallback", `{"type":"response.done","status":"failed","response":{"status":" "}}`, false, true, false},
		{"status-wrong-type", `{"type":"response.done","status":42}`, true, false, true},
		{"event-wrong-type", `{"type":true}`, false, false, false},
		{"pending-terminal", `{"type":"response.completed","status":"in_progress"}`, false, true, false},
		{"pending", `{"status":"queued","output":[{}]}`, false, false, false},
		{"failed", `{"type":"response.failed"}`, false, true, false},
		{"incomplete", `{"type":"response.incomplete"}`, false, true, false},
		{"cancelled", `{"type":"response.cancelled"}`, false, true, false},
		{"canceled", `{"type":"response.canceled"}`, false, true, false},
		{"error-event", `{"type":"error","error":null}`, false, true, false},
		{"claude-stop", `{"type":"message_delta","delta":{"stop_reason":" end_turn "}}`, true, false, true},
		{"claude-tool", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, true, false, true},
		{"claude-limit", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`, false, true, false},
		{"claude-message", `{"type":"message","stop_reason":"max_tokens"}`, false, false, true},
		{"claude-null", `{"type":"message_delta","delta":{"stop_reason":null}}`, false, false, false},
		{"chat", `{"choices":[{"finish_reason":"tool_calls"}]}`, true, false, true},
		{"chat-null", `{"choices":[{"finish_reason":null}]}`, false, false, false},
		{"chat-mixed", `{"choices":[null,4,{}, {"finish_reason":"stop"},{"finish_reason":"length"}]}`, true, true, false},
		{"chat-filter", `{"choices":[{"finish_reason":"content_filter"}]}`, false, true, false},
		{"gemini", `{"candidates":[{"finishReason":"STOP"}]}`, true, false, true},
		{"gemini-limit", `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, false, true, false},
		{"gemini-safety", `{"candidates":[{"finishReason":"SAFETY"}]}`, false, true, false},
		{"gemini-recitation", `{"candidates":[{"finishReason":"RECITATION"}]}`, false, true, false},
		{"gemini-other", `{"candidates":[{"finishReason":"OTHER"}]}`, false, true, false},
		{"output", `{"output":[{}]}`, false, false, true},
		{"null-output", `{"output":[null]}`, false, false, true},
		{"empty-output", `{"output":[]}`, false, false, false},
		{"array", `[{"type":"response.completed"}]`, false, false, false},
		{"malformed", `{"type":"response.completed","x":}`, false, false, false},
		{"trailing-comma", `{"type":"response.completed",}`, false, false, false},
		{"trailing-value", `{"type":"response.completed"} {}`, false, false, false},
		{"escaped-key", `{"\u0074ype":"response.completed"}`, true, false, true},
		{"duplicate-error-null", `{"error":{},"error":null,"type":"response.completed"}`, true, false, true},
		{"duplicate-error-object", `{"error":null,"error":{},"type":"response.completed"}`, false, true, false},
		{"duplicate-response", `{"response":{"error":{}},"response":{"status":"completed"}}`, false, false, true},
		{"duplicate-nested-status", `{"response":{"status":"failed","status":"completed"}}`, false, false, true},
		{"duplicate-finish", `{"choices":[{"finish_reason":"length","finish_reason":"stop"}]}`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			if c, i := jsonObjectCompletionFlags(payload); c != tc.completed || i != tc.incomplete {
				t.Fatalf("flags = %v/%v, want %v/%v", c, i, tc.completed, tc.incomplete)
			}
			if got := isCompletedExecutionPayload(payload); got != tc.execute {
				t.Fatalf("execution completed = %v, want %v", got, tc.execute)
			}
			wire := []byte(": comment\r\nevent: ignored\r\nid: 1\r\nretry: 5\r\ndata: " + tc.payload + "\r\n\r\ndata: [DONE]\r\n\r\n")
			if c, i := streamPayloadCompletionFlags(wire); c != tc.completed || i != tc.incomplete {
				t.Fatalf("SSE flags = %v/%v, want %v/%v", c, i, tc.completed, tc.incomplete)
			}
		})
	}
}

func TestItem14StreamFramingContracts(t *testing.T) {
	for _, tc := range []struct {
		payload               string
		completed, incomplete bool
	}{
		{"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\r\n\r\ndata: {\"type\":\"response.failed\"}\n\n", true, true},
		{"event: response.completed\n: {\"type\":\"response.failed\"}\nid: {\"error\":{}}\nretry: 1\ndata: [DONE]\n\n", false, false},
		// The stateless observer retains its per-line contract; it must not join
		// unknown framing or guess that an incomplete chunk is a terminal event.
		{"data: {\"type\":\n data: \"response.completed\"}\n\n", false, false},
		{"data: {\"type\":\"response.comp", false, false},
		{"leted\"}\n\n", false, false},
	} {
		if c, i := streamPayloadCompletionFlags([]byte(tc.payload)); c != tc.completed || i != tc.incomplete {
			t.Fatalf("%q: flags = %v/%v, want %v/%v", tc.payload, c, i, tc.completed, tc.incomplete)
		}
	}
}

func BenchmarkItem14StreamCompletion(b *testing.B) {
	for _, tc := range []struct{ name, payload string }{
		{"delta", `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n"},
		{"large_delta", `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 8192) + `"}` + "\n\n"},
		{"chat_delta", `data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n"},
		{"terminal", `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":100,"output_tokens":7}}}` + "\n\n"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			payload := []byte(tc.payload)
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				streamPayloadCompletionFlags(payload)
			}
		})
	}
}

func BenchmarkItem14CompletedExecution(b *testing.B) {
	payload := []byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + strings.Repeat("x", 8192) + `"}]}],"usage":{"input_tokens":100,"output_tokens":10}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		isCompletedExecutionPayload(payload)
	}
}
