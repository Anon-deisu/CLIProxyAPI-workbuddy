package cursor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAssistantBlobCollectsThinking(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"role": "assistant",
		"id":   "msg-1",
		"content": []map[string]any{
			{"type": "thinking", "thinking": "I'm calculating 17*23."},
			{"type": "text", "text": "391"},
		},
	})
	msg, ok := parseAssistantBlob("k", raw)
	if !ok {
		t.Fatal("blob should parse")
	}
	if msg.thinking != "I'm calculating 17*23." {
		t.Fatalf("thinking = %q", msg.thinking)
	}
	if msg.text != "391" {
		t.Fatalf("text = %q", msg.text)
	}
}

func TestParseAssistantBlobThinkingOnly(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "reasoning", "text": "let me think"}},
	})
	msg, ok := parseAssistantBlob("k", raw)
	if !ok {
		t.Fatal("thinking-only blob should still parse")
	}
	if msg.thinking != "let me think" {
		t.Fatalf("thinking = %q", msg.thinking)
	}
}

func TestFlushThinkingRepairsLeadingToken(t *testing.T) {
	s := &Session{events: make(chan StreamEvent, 32)}
	// Wire deltas dropped the leading "I", checkpoint has the full text.
	s.visibleThinking.WriteString("'m calculating 17*23.")
	repaired := reconcileAssistantText(s.visibleThinking.String(), "I'm calculating 17*23.")
	s.flushThinking(repaired, "")

	var got strings.Builder
	for len(s.events) > 0 {
		ev := <-s.events
		if ev.Type == "thinking_delta" {
			got.WriteString(ev.Text)
		}
	}
	if got.String() != "I'm calculating 17*23." {
		t.Fatalf("thinking not repaired: %q", got.String())
	}
}

func TestFlushThinkingSkipsAlreadyEmitted(t *testing.T) {
	s := &Session{events: make(chan StreamEvent, 8)}
	s.flushThinking("same", "same")
	if len(s.events) != 0 {
		t.Fatalf("nothing should be emitted, got %d events", len(s.events))
	}
}
