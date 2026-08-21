package executor

import (
	"context"
	"errors"
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

func TestCursorToolChoiceConstraintNeverTurnsCompletedResponseIntoError(t *testing.T) {
	constraint := cursorToolConstraint{required: true, name: "Read"}
	if constraint.Satisfied(nil) {
		t.Fatal("a response without Read does not satisfy the advisory constraint")
	}
	if !constraint.Satisfied([]cursor.ToolCall{{Name: "read"}}) {
		t.Fatal("tool names should match case-insensitively")
	}
	// The executor intentionally only observes Satisfied and logs a miss. It no
	// longer exposes a Validate method that could convert a completed turn into
	// `cursor: tool_choice required tool Read`.
}

func TestShouldRetryToolTailWithoutContinuation(t *testing.T) {
	hard := fmt.Errorf(`cursor connect end-stream error: {"code":"resource_exhausted","details":[{"debug":{"error":"ERROR_PROVIDER_ERROR","details":{"additionalInfo":{"providerStatusCode":"400"}}}}]}`)
	policy := fmt.Errorf(`cursor connect end-stream error: {"code":"invalid_argument","details":[{"debug":{"error":"ERROR_OPENAI","details":{"title":"Request blocked by Anthropic","detail":"blocked under [Anthropic's Usage Policy]","analyticsMetadata":{"actionRequired":"cyber_policy_review"}}}}]}`)
	messages := []cursor.ChatMessage{
		{Role: "assistant", ToolCalls: []cursor.ToolCall{{ID: "c1", Name: "Bash"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	if !shouldRetryToolTailWithoutContinuation(true, messages, hard) {
		t.Fatal("rejected continuation tool rebuild must retry from history")
	}
	if shouldRetryToolTailWithoutContinuation(false, messages, hard) {
		t.Fatal("full-history runs must not loop")
	}
	if shouldRetryToolTailWithoutContinuation(true, []cursor.ChatMessage{{Role: "user", Content: "hi"}}, hard) {
		t.Fatal("plain asks are not tool-tail rebuilds")
	}
	if shouldRetryToolTailWithoutContinuation(true, messages, policy) {
		t.Fatal("policy blocks must not be retried")
	}
}

func TestToolResumeRebuildKeepsSharedSessionAlive(t *testing.T) {
	if shouldCloseCursorSessionAfterToolSubmit(cursor.ErrToolResumeRebuild) {
		t.Fatal("rebuild signal must not close a session shared by parallel tool-result requests")
	}
	if !shouldCloseCursorSessionAfterToolSubmit(errors.New("submit failed")) {
		t.Fatal("ordinary submit failures must close the unusable live session")
	}
	if shouldCloseCursorSessionAfterToolSubmit(nil) {
		t.Fatal("successful submit must not close the session")
	}
}

func TestToolChoiceUndeclaredNamedToolIsAdvisoryNotError(t *testing.T) {
	// `tool_choice` asks for a Cursor-native tool (`Read`) that was not in the
	// declared set of this request. Cursor's wire has no exact named-tool
	// equivalent, so the request must proceed with the declared tools and no
	// local 400 — this is the exact `cursor: tool_choice required tool Read`
	// production symptom.
	tools := []cursor.ToolDefinition{{Name: "Bash"}}
	messages := []cursor.ChatMessage{{Role: "user", Content: "go"}}
	out, filtered, constraint, err := applyCursorToolChoice(
		[]byte(`{"tool_choice":{"type":"function","name":"Read"}}`), messages, tools)
	if err != nil {
		t.Fatalf("undeclared named tool_choice must not error: %v", err)
	}
	if len(out) != len(messages) {
		t.Fatalf("messages should be untouched: got %d", len(out))
	}
	if len(filtered) != len(tools) {
		t.Fatalf("tools should be untouched: got %d", len(filtered))
	}
	if constraint.required {
		t.Fatal("no advisory constraint may be installed for an undeclared tool")
	}
}

func TestToolChoiceUnsupportedShapeIsAdvisoryNotError(t *testing.T) {
	tools := []cursor.ToolDefinition{{Name: "Bash"}}
	messages := []cursor.ChatMessage{{Role: "user", Content: "go"}}
	out, filtered, constraint, err := applyCursorToolChoice(
		[]byte(`{"tool_choice":{"type":"something_unknown"}}`), messages, tools)
	if err != nil {
		t.Fatalf("unknown tool_choice shape must degrade, not error: %v", err)
	}
	if len(out) != len(messages) || len(filtered) != len(tools) || constraint.required {
		t.Fatalf("unknown shape should pass through untouched: out=%d tools=%d constraint=%+v", len(out), len(filtered), constraint)
	}
}

func TestToolChoiceAllowedToolsUndeclaredIsAdvisoryNotError(t *testing.T) {
	tools := []cursor.ToolDefinition{{Name: "Bash"}}
	messages := []cursor.ChatMessage{{Role: "user", Content: "go"}}
	out, filtered, constraint, err := applyCursorToolChoice([]byte(
		`{"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"Read"}]}}`),
		messages, tools)
	if err != nil {
		t.Fatalf("allowed_tools with undeclared tool must not error: %v", err)
	}
	if len(out) != len(messages) || len(filtered) != len(tools) || constraint.required {
		t.Fatalf("allowed_tools should fall back to declared tools: out=%d tools=%d constraint=%+v", len(out), len(filtered), constraint)
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
	resourceExhausted := fmt.Errorf(`cursor connect end-stream error: {"code":"resource_exhausted","message":"Error"}`)
	if isCursorProviderError(resourceExhausted) {
		t.Fatal("untyped resource exhaustion must not enter the provider retry ladder")
	}
	if !isCursorRateLimited(resourceExhausted) {
		t.Fatal("untyped resource exhaustion should be surfaced as a 429")
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

func TestCursorTransportResetIncludesResponseBodyClosed(t *testing.T) {
	closed := fmt.Errorf("cursor: stream error while awaiting client tool results: http2: response body closed")
	if !isCursorTransportReset(closed) || !isCursorTransientProviderError(closed) {
		t.Fatal("response body closed must retry like other H2 resets")
	}
	humanized := humanizeCursorError(closed)
	var gateway statusErr
	if !errors.As(humanized, &gateway) || gateway.StatusCode() != http.StatusBadGateway {
		t.Fatalf("response body closed must surface as 502, got %T %v", humanized, humanized)
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
	nonRetryable := fmt.Errorf(`cursor connect end-stream error: {"code":"resource_exhausted","details":[{"debug":{"error":"ERROR_PROVIDER_ERROR","details":{"isRetryable":false,"additionalInfo":{"providerStatusCode":"400"}}}}]}`)
	if isCursorProviderError(nonRetryable) || isCursorHardProviderError(nonRetryable) {
		t.Fatal("explicitly non-retryable provider 400 must not be replayed")
	}
	if isCursorRateLimited(nonRetryable) {
		t.Fatal("provider 400 must not be misclassified as account throttling")
	}
	humanized := humanizeCursorError(nonRetryable)
	var invalid statusErr
	if !errors.As(humanized, &invalid) || invalid.StatusCode() != http.StatusBadRequest {
		t.Fatalf("provider 400 must surface as a terminal bad request, got %T %v", humanized, humanized)
	}
	if !strings.Contains(humanized.Error(), "invalid_request_error") {
		t.Fatalf("provider 400 must stop outer credential retries: %v", humanized)
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

func TestCursorDownstreamPing(t *testing.T) {
	claude := string(cursorDownstreamPing(translator.FormatClaude))
	if !strings.Contains(claude, "event: ping") || !strings.Contains(claude, `"type":"ping"`) {
		t.Fatalf("claude ping = %q", claude)
	}
	openai := string(cursorDownstreamPing(translator.FormatOpenAI))
	if !strings.HasPrefix(openai, ": keep-alive") {
		t.Fatalf("openai ping = %q", openai)
	}
}
