package cursor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

// Text is streamed live, so a checkpoint that disagrees with what already went
// out must not be re-sent: the client would see the answer twice.
func TestFlushAssistantTextNeverDuplicatesStreamedText(t *testing.T) {
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
	// Cursor dropped the leading token, and the opening was already released to
	// the client, so the checkpoint copy can no longer replace it.
	s.handleTextDelta("ARKER123")
	s.releaseTextStream()
	drainEvents(s)

	s.flushAssistantText(true)

	select {
	case ev := <-s.events:
		t.Fatalf("checkpoint must not re-emit diverging text, got %#v", ev)
	default:
	}
}

func TestTextStreamsLiveAfterOpeningIsReleased(t *testing.T) {
	s := &Session{events: make(chan StreamEvent, 16), seenAssistantMsgIDs: map[string]struct{}{}}
	// Opening is withheld until the checkpoint arrives or the hold window ends.
	s.handleTextDelta("hel")
	if len(s.events) != 0 {
		t.Fatal("opening must be withheld")
	}
	s.releaseTextStream()
	// Everything after the release streams immediately.
	s.handleTextDelta("lo")
	s.handleTextDelta(" world")

	var got strings.Builder
	for len(s.events) > 0 {
		ev := <-s.events
		if ev.Type == "text_delta" {
			got.WriteString(ev.Text)
		}
	}
	if got.String() != "hello world" {
		t.Fatalf("streamed text = %q", got.String())
	}
}

func TestReleaseTextStreamRepairsOpeningFromCheckpoint(t *testing.T) {
	blobID := []byte{0xab, 0xcd, 0xef, 0x03}
	payload, _ := json.Marshal(map[string]any{
		"id":      "msg-open",
		"role":    "assistant",
		"content": []map[string]string{{"type": "text", "text": "MARKER123"}},
	})
	s := &Session{
		blobStore:           map[string][]byte{sprintfHex(blobID): payload},
		events:              make(chan StreamEvent, 16),
		seenAssistantMsgIDs: map[string]struct{}{},
		conversationState: &agentv1.ConversationStateStructure{
			RootPromptMessagesJson: [][]byte{blobID},
		},
	}
	s.handleTextDelta("ARKER123") // Cursor dropped the leading token
	s.releaseTextStream()

	var got strings.Builder
	for len(s.events) > 0 {
		ev := <-s.events
		if ev.Type == "text_delta" {
			got.WriteString(ev.Text)
		}
	}
	if got.String() != "MARKER123" {
		t.Fatalf("opening not repaired, got %q", got.String())
	}
}

func drainEvents(s *Session) {
	for len(s.events) > 0 {
		<-s.events
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

func TestFlushAssistantTextAddsNothingWithoutCheckpoint(t *testing.T) {
	s := &Session{
		events:              make(chan StreamEvent, 8),
		seenAssistantMsgIDs: map[string]struct{}{},
	}
	s.handleTextDelta("partial")
	drainEvents(s)

	s.flushAssistantText(false)
	select {
	case ev := <-s.events:
		t.Fatalf("nothing to append without a checkpoint, got %#v", ev)
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
