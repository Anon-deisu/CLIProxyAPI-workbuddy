package cursor

import (
	"strings"
	"testing"
)

// A Cursor MCP exec can carry the provider call id and the model's
// function-call id in one toolCallId separated by a newline. Published verbatim
// that newline lands inside tool_call.id, which clients validating against
// ^[a-zA-Z0-9_-]+$ reject.
const doubleID = "call-fdf3e299-7616-4722-aede-755dbe807b58-0\nfc_656b05c2-6de7-95ed-87d0-b030178c6827_0"

func TestPublishedToolCallIDIsClientSafe(t *testing.T) {
	m := NewSessionManager()
	item := &pendingExec{call: ToolCall{ID: doubleID, Name: "get_weather"}}
	s := &Session{
		ID:      "pub-safe",
		pending: map[string]*pendingExec{doubleID: item},
		events:  make(chan StreamEvent, 4),
		manager: m,
	}
	m.Register(s)

	s.publishToolCall(item, item.call)

	event := <-s.events
	if event.ToolCall == nil {
		t.Fatal("no tool call was published")
	}
	published := event.ToolCall.ID
	if strings.ContainsAny(published, "\r\n") {
		t.Fatalf("published id still carries a newline: %q", published)
	}
	for _, r := range published {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			t.Fatalf("published id has an unsafe rune %q in %q", r, published)
		}
	}

	// The raw id keeps working, and the published form must resolve too:
	// clients echo back what they were shown.
	if got, _ := s.lookupPendingTool(doubleID); got != item {
		t.Fatal("raw id stopped resolving after publication")
	}
	if got, _ := s.lookupPendingTool(published); got != item {
		t.Fatalf("published id %q does not resolve to its exec", published)
	}
	if m.LookupPending(published) != s {
		t.Fatal("published id is not bound in the manager")
	}
}

// Aliasing must not enqueue the exec a second time; a duplicate would emit the
// same tool call twice.
func TestAliasingAPublishedIDKeepsOnePlaceInTheBatch(t *testing.T) {
	item := &pendingExec{call: ToolCall{ID: doubleID, Name: "get_weather"}}
	s := &Session{
		ID:           "alias-order",
		pending:      map[string]*pendingExec{doubleID: item},
		pendingOrder: []*pendingExec{item},
		events:       make(chan StreamEvent, 4),
	}
	s.publishToolCall(item, item.call)
	if len(s.pendingOrder) != 1 {
		t.Fatalf("pendingOrder = %d entries, want the exec listed once", len(s.pendingOrder))
	}
}

// An already-safe id must be published untouched so existing clients keep the
// ids they have been correlating against.
func TestSafeToolCallIDIsPublishedUnchanged(t *testing.T) {
	item := &pendingExec{call: ToolCall{ID: "toolu_01abc", Name: "Bash"}}
	s := &Session{
		ID:      "safe-id",
		pending: map[string]*pendingExec{"toolu_01abc": item},
		events:  make(chan StreamEvent, 4),
	}
	s.publishToolCall(item, item.call)
	if got := (<-s.events).ToolCall.ID; got != "toolu_01abc" {
		t.Fatalf("published id = %q, want it unchanged", got)
	}
}
