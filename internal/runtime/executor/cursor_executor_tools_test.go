package executor

import (
	"fmt"
	"strings"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
)

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

func TestCompactMessagesAlwaysKeepsAUserTurn(t *testing.T) {
	// A long tool-only tail: trimming used to leave no user message at all.
	msgs := []cursorlib.ChatMessage{{Role: "system", Content: "rules"}}
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			cursorlib.ChatMessage{Role: "assistant", ToolCalls: []cursorlib.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "Bash"}}},
			cursorlib.ChatMessage{Role: "tool", ToolCallID: fmt.Sprintf("c%d", i), Content: "output"},
		)
	}
	out := compactMessagesForRetry(msgs)
	if !hasUserTurn(out) {
		t.Fatalf("compacted request must keep a user turn: %#v", out)
	}
}

func TestCompactMessagesKeepsExistingUserTurn(t *testing.T) {
	msgs := []cursorlib.ChatMessage{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: "the real question"},
	}
	out := compactMessagesForRetry(msgs)
	last := out[len(out)-1]
	if last.Role != "user" || last.Content != "the real question" {
		t.Fatalf("existing user turn must be preserved, got %#v", out)
	}
}

func TestStripTrailingToolRound(t *testing.T) {
	in := []cursorlib.ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []cursorlib.ToolCall{{ID: "c1", Name: "t"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	out := stripTrailingToolRound(in)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("got %#v", out)
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
