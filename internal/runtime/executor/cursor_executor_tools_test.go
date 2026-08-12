package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestApplyCursorToolChoicePreservesExactSemantics(t *testing.T) {
	tools := []cursor.ToolDefinition{{Name: "read"}, {Name: "write"}}
	messages := []cursor.ChatMessage{{Role: "user", Content: "go"}}

	_, none, constraint, err := applyCursorToolChoice([]byte(`{"tool_choice":"none"}`), messages, tools)
	if err != nil || len(none) != 0 || constraint.required {
		t.Fatalf("none: tools=%v constraint=%+v err=%v", none, constraint, err)
	}
	requiredMessages, required, constraint, err := applyCursorToolChoice([]byte(`{"tool_choice":"required"}`), messages, tools)
	if err != nil || len(required) != 2 || !constraint.required || len(requiredMessages) != 2 {
		t.Fatalf("required: tools=%v constraint=%+v messages=%v err=%v", required, constraint, requiredMessages, err)
	}
	_, named, constraint, err := applyCursorToolChoice([]byte(`{"tool_choice":{"type":"function","name":"WRITE"}}`), messages, tools)
	if err != nil || len(named) != 1 || named[0].Name != "write" || constraint.name != "write" {
		t.Fatalf("named: tools=%v constraint=%+v err=%v", named, constraint, err)
	}
	_, allowed, constraint, err := applyCursorToolChoice([]byte(`{"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"read"}]}}`), messages, tools)
	if err != nil || len(allowed) != 1 || allowed[0].Name != "read" || !constraint.required {
		t.Fatalf("allowed: tools=%v constraint=%+v err=%v", allowed, constraint, err)
	}
}

func TestResolveCursorConversationIDIsAccountScoped(t *testing.T) {
	request := cliproxyexecutor.Request{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "session-1"}}
	opts := cliproxyexecutor.Options{SourceFormat: translator.FormatOpenAI, Headers: make(http.Header)}
	a := &cliproxyauth.Auth{ID: "account-a"}
	b := &cliproxyauth.Auth{ID: "account-b"}
	first := resolveCursorConversationID(context.Background(), a, request, opts, nil, nil)
	if repeat := resolveCursorConversationID(context.Background(), a, request, opts, nil, nil); repeat != first {
		t.Fatalf("same account/session changed: %q != %q", repeat, first)
	}
	if other := resolveCursorConversationID(context.Background(), b, request, opts, nil, nil); other == first {
		t.Fatal("different accounts must not share a Cursor conversation")
	}
}

func TestExtractToolsAcceptsAnthropicInputSchema(t *testing.T) {
	body := []byte(`{
		"tools":[
			{
				"name":"get_weather",
				"description":"weather",
				"input_schema":{
					"$schema":"https://json-schema.org/draft/2020-12/schema",
					"type":"object",
					"properties":{"city":{"type":"string"}},
					"required":["city"]
				}
			}
		]
	}`)
	tools := extractTools(body)
	if len(tools) != 1 {
		t.Fatalf("len=%d", len(tools))
	}
	if tools[0].Name != "get_weather" {
		t.Fatalf("name=%q", tools[0].Name)
	}
	if _, ok := tools[0].Parameters["$schema"]; ok {
		t.Fatalf("$schema should be sanitized away: %#v", tools[0].Parameters)
	}
	props, _ := tools[0].Parameters["properties"].(map[string]any)
	if props == nil || props["city"] == nil {
		t.Fatalf("properties=%#v", tools[0].Parameters)
	}
}

func TestIsCursorProviderError(t *testing.T) {
	if !isCursorProviderError(fmt.Errorf(`cursor connect end-stream error: {"code":"resource_exhausted","message":"Error"}`)) {
		t.Fatal("expected provider error detection")
	}
	if isCursorProviderError(fmt.Errorf("cursor: unknown or expired tool_call_id x")) {
		t.Fatal("tool_call_id errors are not provider errors")
	}
}

func TestCursorPolicyBlockIsNotRetried(t *testing.T) {
	blocked := fmt.Errorf(`cursor connect end-stream error: {"code":"invalid_argument","details":[{"debug":{"error":"ERROR_OPENAI","details":{"title":"Request blocked by Anthropic","detail":"blocked under [Anthropic's Usage Policy]","analyticsMetadata":{"actionRequired":"cyber_policy_review"}}}}]}`)
	if !isCursorPolicyBlock(blocked) {
		t.Fatal("expected policy block detection")
	}
	if isCursorProviderError(blocked) {
		t.Fatal("policy blocks must not enter the retry ladder")
	}
	if isCursorTransientProviderError(blocked) {
		// ERROR_OPENAI would otherwise classify it as transient.
		t.Fatal("policy block must not look transient to callers that check directly")
	}
	humanized := humanizeCursorError(blocked)
	if !strings.Contains(humanized.Error(), "usage policy") {
		t.Fatalf("client-facing message unclear: %v", humanized)
	}
}

func TestHumanizeLeavesOtherErrorsAlone(t *testing.T) {
	err := fmt.Errorf("cursor: unknown or expired tool_call_id x")
	if humanizeCursorError(err) != err {
		t.Fatal("non-policy errors must pass through untouched")
	}
}

func TestCursorErrorClassification(t *testing.T) {
	transient := fmt.Errorf(`cursor connect end-stream error: {"code":"unavailable","message":"Error","details":[{"debug":{"error":"ERROR_OPENAI","details":{"title":"Unable to reach the model provider"}}}]}`)
	if !isCursorTransientProviderError(transient) {
		t.Fatal("expected transient ERROR_OPENAI")
	}
	if isCursorHardProviderError(transient) {
		t.Fatal("ERROR_OPENAI should not be hard")
	}
	hard := fmt.Errorf(`cursor connect end-stream error: {"code":"resource_exhausted","details":[{"debug":{"error":"ERROR_PROVIDER_ERROR","details":{"additionalInfo":{"providerStatusCode":"400"}}}}]}`)
	if !isCursorHardProviderError(hard) {
		t.Fatal("expected hard 400")
	}
	if isCursorTransientProviderError(hard) {
		t.Fatal("hard 400 should not be transient")
	}
}

func TestApplyCursorReasoningEffort(t *testing.T) {
	got := applyCursorReasoningEffort("claude-fable-5", []byte(`{"reasoning_effort":"high"}`))
	if got != "claude-fable-5-thinking-high" {
		t.Fatalf("got %q", got)
	}
	got = applyCursorReasoningEffort("claude-fable-5-thinking-high", []byte(`{"reasoning_effort":"high"}`))
	if got != "claude-fable-5-thinking-high" {
		t.Fatalf("idempotent got %q", got)
	}
	got = applyCursorReasoningEffort("grok-4.5", []byte(`{}`))
	if got != "grok-4.5" {
		t.Fatalf("unchanged got %q", got)
	}
	if !strings.HasSuffix(applyCursorReasoningEffort("m", []byte(`{"reasoning_effort":"auto"}`)), "-thinking-high") {
		t.Fatalf("auto mapping failed")
	}
}
