package cursor

import (
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

// snapshotWithAssistant builds a checkpoint whose blob store records the given
// assistant reply, the way a real conversation_checkpoint_update would.
func snapshotWithAssistant(t *testing.T, assistantText string) checkpointSnapshot {
	t.Helper()
	blobs := map[string][]byte{}
	blob := storeBlob(blobs, mustJSON(map[string]any{
		"role": "assistant",
		"id":   "msg_1",
		"content": []map[string]string{
			{"type": "text", "text": assistantText},
		},
	}))
	state := &agentv1.ConversationStateStructure{
		RootPromptMessagesJson: [][]byte{blob},
		Turns:                  [][]byte{[]byte("opaque-turn")},
	}
	return checkpointSnapshot{state: state, blobs: blobs}
}

func TestContinuationStoreRoundTrip(t *testing.T) {
	id := "conv-roundtrip"
	dropContinuation(id)
	snapshot := snapshotWithAssistant(t, "the answer is 42")
	rememberContinuation(id, snapshot)

	taken, ok := takeContinuation(id)
	if !ok {
		t.Fatal("stored continuation not found")
	}
	if !proto.Equal(taken.state, snapshot.state) {
		t.Fatal("state did not round-trip")
	}
	// The taken copy must be isolated: a session mutating it cannot corrupt
	// the store.
	taken.state.Turns = nil
	for key := range taken.blobs {
		taken.blobs[key] = []byte("scribbled")
	}
	again, ok := takeContinuation(id)
	if !ok || len(again.state.GetTurns()) != 1 {
		t.Fatalf("store was corrupted by the caller: %#v", again.state)
	}
	dropContinuation(id)
	if _, ok := takeContinuation(id); ok {
		t.Fatal("dropped continuation still resolvable")
	}
}

func TestContinuationWithoutAssistantIsNotStored(t *testing.T) {
	id := "conv-empty"
	dropContinuation(id)
	rememberContinuation(id, checkpointSnapshot{
		state: &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("t")}},
		blobs: map[string][]byte{},
	})
	if _, ok := takeContinuation(id); ok {
		t.Fatal("nothing to validate against, must not be stored")
	}
}

func TestContinuationMatchesHistory(t *testing.T) {
	snapshot := snapshotWithAssistant(t, "the answer is 42")
	base := []ChatMessage{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: "what is the answer?"},
		{Role: "assistant", Content: "the answer is 42"},
		{Role: "user", Content: "and why?"},
	}
	if !continuationMatchesHistory(snapshot, base) {
		t.Fatal("clean follow-up turn must continue")
	}

	edited := append([]ChatMessage(nil), base...)
	edited[2].Content = "the answer is 41"
	if continuationMatchesHistory(snapshot, edited) {
		t.Fatal("history that disagrees with the checkpoint must rebuild")
	}

	toolTail := []ChatMessage{
		{Role: "user", Content: "run it"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "Bash"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "user", Content: "now what?"},
	}
	if continuationMatchesHistory(snapshot, toolTail) {
		t.Fatal("a trailing tool round must rebuild")
	}

	if continuationMatchesHistory(snapshot, []ChatMessage{{Role: "user", Content: "hi"}}) {
		t.Fatal("no assistant history must rebuild")
	}

	trailingAssistant := append([]ChatMessage(nil), base[:3]...)
	if continuationMatchesHistory(snapshot, trailingAssistant) {
		t.Fatal("request not ending in a user ask must rebuild")
	}
}

func TestBuildContinuationRunRequestShape(t *testing.T) {
	snapshot := snapshotWithAssistant(t, "the answer is 42")
	stateBefore := proto.Clone(snapshot.state).(*agentv1.ConversationStateStructure)
	messages := []ChatMessage{
		{Role: "user", Content: "what is the answer?"},
		{Role: "assistant", Content: "the answer is 42"},
		{Role: "user", Content: "and why is that?"},
	}
	clientMsg, blobs := buildContinuationRunRequest("default", messages, nil, "conv-shape", snapshot)
	run := clientMsg.GetRunRequest()
	if run == nil {
		t.Fatal("no run request built")
	}
	// The server's state goes back untouched.
	if !proto.Equal(run.GetConversationState(), stateBefore) {
		t.Fatal("conversation state was modified")
	}
	if run.GetConversationId() != "conv-shape" {
		t.Fatalf("conversation id = %q", run.GetConversationId())
	}
	action := run.GetAction().GetUserMessageAction()
	if action == nil || action.GetUserMessage().GetText() != "and why is that?" {
		t.Fatalf("action must carry only the new ask: %#v", run.GetAction())
	}
	if len(blobs) != len(snapshot.blobs) {
		t.Fatalf("blob store lost entries: %d vs %d", len(blobs), len(snapshot.blobs))
	}
}

func TestClosepersistsAndFailDrops(t *testing.T) {
	snapshot := snapshotWithAssistant(t, "done")
	clean := &Session{
		ConversationID:    "conv-close",
		conversationState: snapshot.state,
		blobStore:         snapshot.blobs,
		events:            make(chan StreamEvent, 8),
	}
	dropContinuation("conv-close")
	_ = clean.Close()
	if _, ok := takeContinuation("conv-close"); !ok {
		t.Fatal("clean close must persist the checkpoint")
	}

	failing := &Session{
		ConversationID:    "conv-close",
		conversationState: snapshot.state,
		blobStore:         snapshot.blobs,
		events:            make(chan StreamEvent, 8),
		usedContinuation:  true,
	}
	failing.fail(errUnavailableForTest)
	if _, ok := takeContinuation("conv-close"); ok {
		t.Fatal("a failed continuation run must drop the stored checkpoint")
	}
}

func TestCloseParkedDropsContinuation(t *testing.T) {
	id := "conv-parked-drop"
	dropContinuation(id)
	snapshot := snapshotWithAssistant(t, "done")
	rememberContinuation(id, snapshot)
	s := &Session{
		ConversationID:    id,
		conversationState: snapshot.state,
		blobStore:         snapshot.blobs,
		waitingTools:      true,
		responseEnded:     true,
		pending:           map[string]*pendingExec{"c1": {}},
		events:            make(chan StreamEvent, 4),
	}
	_ = s.Close()
	if _, ok := takeContinuation(id); ok {
		t.Fatal("parked close must not keep a tool-pause checkpoint")
	}
}

var errUnavailableForTest = errTest("upstream unavailable")

type errTest string

func (e errTest) Error() string { return string(e) }
