package cursor

import (
	"strings"
	"testing"
)

func TestClampMessageContentKeepsHeadAndTail(t *testing.T) {
	body := strings.Repeat("x", maxMessageChars*3)
	got := clampMessageContent("HEAD_MARKER" + body + "TAIL_MARKER")
	if len([]rune(got)) > maxMessageChars+128 {
		t.Fatalf("clamped length %d exceeds budget", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "HEAD_MARKER") {
		t.Error("the ask at the top of the turn was cut")
	}
	if !strings.HasSuffix(got, "TAIL_MARKER") {
		t.Error("the newest output at the bottom was cut")
	}
	if !strings.Contains(got, "CLIProxyAPI truncated") {
		t.Error("truncation must be visible to the model")
	}
}

func TestClampLeavesNormalMessagesUntouched(t *testing.T) {
	msg := "a normal turn"
	if clampMessageContent(msg) != msg {
		t.Fatal("small messages must pass through byte-identical")
	}
}

func TestTrimHistoryBoundsAGiantTrailingToolResult(t *testing.T) {
	// The case retries could never fix: history is short, so no message gets
	// dropped, but one tool result alone overflows the model window.
	history := []ChatMessage{
		{Role: "user", Content: "read the file"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "Read"}}},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("y", 400_000)},
	}
	out := trimConversationHistory(history)
	total := 0
	for _, msg := range out {
		total += len(msg.Content)
		if len([]rune(msg.Content)) > maxMessageChars+128 {
			t.Fatalf("message left at %d chars", len([]rune(msg.Content)))
		}
	}
	if total > maxHistoryChars {
		t.Fatalf("history total %d exceeds %d", total, maxHistoryChars)
	}
	if len(out) != 3 {
		t.Fatalf("clamping should not drop turns, got %d", len(out))
	}
}

func TestTrimHistoryDoesNotMutateCaller(t *testing.T) {
	original := strings.Repeat("z", 200_000)
	history := []ChatMessage{{Role: "user", Content: original}}
	_ = trimConversationHistory(history)
	if history[0].Content != original {
		t.Fatal("caller's messages must not be rewritten in place")
	}
}

func TestLongChatOfShortTurnsIsKeptWhole(t *testing.T) {
	// Bounding by turn count used to throw away context that comfortably fit.
	history := make([]ChatMessage, 0, 200)
	for i := 0; i < 100; i++ {
		history = append(history,
			ChatMessage{Role: "user", Content: "question"},
			ChatMessage{Role: "assistant", Content: "answer"})
	}
	if got := len(trimConversationHistory(history)); got != len(history) {
		t.Fatalf("kept %d of %d small turns", got, len(history))
	}
}

func TestOversizedHistoryStillGetsCut(t *testing.T) {
	history := make([]ChatMessage, 0, 20)
	for i := 0; i < 20; i++ {
		history = append(history, ChatMessage{Role: "user", Content: strings.Repeat("q", 20_000)})
	}
	out := trimConversationHistory(history)
	total := 0
	for _, msg := range out {
		total += len(msg.Content)
	}
	if total > maxHistoryChars {
		t.Fatalf("history total %d exceeds %d", total, maxHistoryChars)
	}
	if len(out) == len(history) {
		t.Fatal("nothing was dropped from an oversized history")
	}
	if out[len(out)-1].Content != history[len(history)-1].Content {
		t.Fatal("the newest turn must survive")
	}
}

func TestSystemPromptIsBounded(t *testing.T) {
	huge := strings.Repeat("s", maxSystemPromptRunes*2)
	system, _ := splitSystemMessages([]ChatMessage{
		{Role: "system", Content: huge},
		{Role: "user", Content: "hi"},
	})
	if len([]rune(system)) > maxSystemPromptRunes {
		t.Fatalf("system prompt left at %d runes", len([]rune(system)))
	}
}
