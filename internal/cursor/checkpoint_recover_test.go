package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
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

// A checkpoint that arrives AFTER the truncated opening was released must still
// repair it: emit only the recovered leading token(s), never replay what the
// client already owns. Cursor drops the opening token ("每次" → "次"), the
// checkpoint is the only authoritative copy, and it can land after the hold
// window already released the truncated text live.
func TestFlushAssistantTextRepairsLateOpening(t *testing.T) {
	blobID := []byte{0xab, 0xcd, 0xef, 0x0a}
	full := "每次它返回的都有变化"
	payload, _ := json.Marshal(map[string]any{
		"id":      "msg-late-open",
		"role":    "assistant",
		"content": []map[string]string{{"type": "text", "text": full}},
	})

	// No checkpoint present yet: releasing emits the truncated streamed copy.
	s := &Session{
		blobStore:           map[string][]byte{},
		events:              make(chan StreamEvent, 16),
		seenAssistantMsgIDs: map[string]struct{}{},
	}
	s.handleTextDelta(strings.TrimPrefix(full, "每")) // wire delta dropped the leading "每"
	s.releaseTextStream()

	var got strings.Builder
	for len(s.events) > 0 {
		ev := <-s.events
		if ev.Type == "text_delta" {
			got.WriteString(ev.Text)
		}
	}
	if got.String() != strings.TrimPrefix(full, "每") {
		t.Fatalf("pre-checkpoint release = %q, want truncated %q", got.String(), strings.TrimPrefix(full, "每"))
	}

	// The authoritative checkpoint lands late.
	s.conversationState = &agentv1.ConversationStateStructure{
		RootPromptMessagesJson: [][]byte{blobID},
	}
	s.blobStore[sprintfHex(blobID)] = payload
	s.flushAssistantText(false)

	got.Reset()
	for len(s.events) > 0 {
		ev := <-s.events
		if ev.Type == "text_delta" {
			got.WriteString(ev.Text)
		}
	}
	// Only the recovered first character ("每") may be re-emitted.
	if got.String() != "每" {
		t.Fatalf("late checkpoint recovery = %q, want %q", got.String(), "每")
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

func TestFlushAssistantTextRecoversTypedTurnStep(t *testing.T) {
	turnID := []byte{0x10, 0x20, 0x30, 0x40}
	stepID := []byte{0x50, 0x60, 0x70, 0x80}
	turn, err := proto.Marshal(&agentv1.ConversationTurnStructure{
		Turn: &agentv1.ConversationTurnStructure_AgentConversationTurn{
			AgentConversationTurn: &agentv1.AgentConversationTurnStructure{
				Steps: [][]byte{stepID},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := proto.Marshal(&agentv1.ConversationStep{
		Message: &agentv1.ConversationStep_AssistantMessage{
			AssistantMessage: &agentv1.AssistantMessage{Text: "typed final answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{
		blobStore: map[string][]byte{
			sprintfHex(turnID): turn,
			sprintfHex(stepID): step,
		},
		events:              make(chan StreamEvent, 8),
		seenAssistantMsgIDs: map[string]struct{}{},
		conversationState: &agentv1.ConversationStateStructure{
			Turns: [][]byte{turnID},
		},
	}

	s.flushAssistantText(true)

	select {
	case ev := <-s.events:
		if ev.Type != "text_delta" || ev.Text != "typed final answer" {
			t.Fatalf("got %#v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("expected typed checkpoint text")
	}
}

func TestCollectSegmentAcceptsAuthoritativeEmptyStop(t *testing.T) {
	s := &Session{
		ConversationID: "conversation",
		ID:             "session",
		events:         make(chan StreamEvent, 1),
		errCh:          make(chan error, 1),
	}
	s.events <- StreamEvent{Type: "segment_end", Reason: "stop"}

	result, err := s.CollectSegment(context.Background())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.FinishReason != "stop" || result.Text != "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestIterSegmentRejectsToolBoundaryWithoutToolCall(t *testing.T) {
	s := &Session{
		events: make(chan StreamEvent, 1),
		errCh:  make(chan error, 1),
	}
	s.events <- StreamEvent{Type: "segment_end", Reason: "tool_calls"}

	err := s.IterSegment(context.Background(), func(StreamEvent) error { return nil })
	if !errors.Is(err, ErrEmptyTerminal) {
		t.Fatalf("error = %v, want ErrEmptyTerminal", err)
	}
}

func TestIterSegmentAcceptsReasoningOnlyAuthoritativeStop(t *testing.T) {
	s := &Session{
		events: make(chan StreamEvent, 2),
		errCh:  make(chan error, 1),
	}
	s.events <- StreamEvent{Type: "thinking_delta", Text: "Inspecting the workspace"}
	s.events <- StreamEvent{Type: "segment_end", Reason: "stop"}

	var events []StreamEvent
	err := s.IterSegment(context.Background(), func(ev StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 2 || events[0].Type != "thinking_delta" {
		t.Fatalf("events = %#v", events)
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
