package cursor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func TestFlushAssistantTextRepairsMissingPrefix(t *testing.T) {
	blobID := []byte{0xab, 0xcd, 0xef, 0x01}
	payload, _ := json.Marshal(map[string]any{
		"id":   "msg-final",
		"role": "assistant",
		"content": []map[string]string{
			{"type": "text", "text": "MARKER123"},
		},
	})
	s := &Session{
		blobStore:           map[string][]byte{sprintfHex(blobID): payload},
		events:              make(chan StreamEvent, 8),
		seenAssistantMsgIDs: map[string]struct{}{},
		conversationState: &agentv1.ConversationStateStructure{
			RootPromptMessagesJson: [][]byte{blobID},
		},
	}
	s.handleTextDelta("ARKER123")

	s.flushAssistantText(false)

	var got strings.Builder
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-s.events:
			if ev.Type == "text_delta" {
				got.WriteString(ev.Text)
			}
			if got.String() == "MARKER123" {
				return
			}
		case <-deadline:
			t.Fatalf("got %q, want MARKER123", got.String())
		}
	}
}

func TestFlushAssistantTextAppendsCheckpointSuffix(t *testing.T) {
	blobID := []byte{0xab, 0xcd, 0xef, 0x02}
	payload, _ := json.Marshal(map[string]any{
		"id":   "msg-suffix",
		"role": "assistant",
		"content": []map[string]string{
			{"type": "text", "text": "hello world"},
		},
	})
	s := &Session{
		blobStore:           map[string][]byte{sprintfHex(blobID): payload},
		events:              make(chan StreamEvent, 8),
		seenAssistantMsgIDs: map[string]struct{}{},
		conversationState: &agentv1.ConversationStateStructure{
			RootPromptMessagesJson: [][]byte{blobID},
		},
	}
	s.visibleText.WriteString("hello")
	s.emittedText.WriteString("hello")

	s.flushAssistantText(true)

	select {
	case ev := <-s.events:
		if ev.Type != "text_delta" || ev.Text != " world" {
			t.Fatalf("got %#v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("expected flushed suffix")
	}
}

func TestFlushAssistantTextWaitsForCheckpointWhenNotForced(t *testing.T) {
	s := &Session{
		events:              make(chan StreamEvent, 8),
		seenAssistantMsgIDs: map[string]struct{}{},
	}
	s.handleTextDelta("partial")
	s.flushAssistantText(false)
	select {
	case ev := <-s.events:
		t.Fatalf("unexpected event %#v", ev)
	default:
	}
}

func TestReconcileAssistantText(t *testing.T) {
	if got := reconcileAssistantText("ARKER", "MARKER"); got != "MARKER" {
		t.Fatalf("suffix repair: %q", got)
	}
	if got := reconcileAssistantText("hello", "hello world"); got != "hello world" {
		t.Fatalf("prefix extend: %q", got)
	}
	if got := reconcileAssistantText("abc", ""); got != "abc" {
		t.Fatalf("empty checkpoint: %q", got)
	}
}

func sprintfHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}
