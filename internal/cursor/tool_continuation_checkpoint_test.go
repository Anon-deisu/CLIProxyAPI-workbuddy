package cursor

import (
	"strings"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func checkpointWithToolCall(id string) checkpointSnapshot {
	blob := []byte(`{"role":"assistant","content":[{"type":"text","text":"Checking."},` +
		`{"type":"tool-call","toolCallId":"` + id + `","toolName":"run_command","args":{"command":"git status"}}]}`)
	return checkpointSnapshot{
		state: &agentv1.ConversationStateStructure{},
		blobs: map[string][]byte{"aabbcc": blob},
	}
}

func toolTail(id string) []ChatMessage {
	return []ChatMessage{
		{Role: "user", Content: "Run git status and report the branch."},
		{Role: "assistant", Content: "Checking.", ToolCalls: []ToolCall{
			{ID: id, Name: "run_command", Arguments: map[string]any{"command": "git status"}}}},
		{Role: "tool", ToolCallID: id, Name: "run_command",
			Content: "On branch main, nothing to commit."},
	}
}

// A tool round-trip whose session died (long tool run, stream EOF, restart)
// must continue from the server checkpoint instead of rebuilding a fresh
// conversation: the rebuild lost the prompt cache and replayed the turn's
// opening, which clients saw as answering twice.
func TestToolTailContinuesFromCheckpoint(t *testing.T) {
	id := "toolu_01KeepCacheAlive"
	if !continuationMatchesHistory(checkpointWithToolCall(id), toolTail(id)) {
		t.Fatal("tool tail with a matching checkpoint must continue natively")
	}
}

func TestToolTailFromAnotherConversationRebuilds(t *testing.T) {
	if continuationMatchesHistory(checkpointWithToolCall("toolu_01SomethingElse"), toolTail("toolu_01KeepCacheAlive")) {
		t.Fatal("checkpoint lacking the answered call ids must force a rebuild")
	}
}

func TestToolTailContinuationCarriesResultsInTheAction(t *testing.T) {
	id := "toolu_01KeepCacheAlive"
	msg, _ := buildContinuationRunRequest("claude-fable-5", toolTail(id), nil, "conv-cc1", checkpointWithToolCall(id))
	action := msg.GetRunRequest().GetAction().GetUserMessageAction()
	if action == nil {
		t.Fatal("continuation lost its user action")
	}
	text := action.GetUserMessage().GetText()
	if !strings.Contains(text, "On branch main") || !strings.Contains(text, id) {
		t.Fatalf("tool output missing from the action: %q", text)
	}
	if strings.Contains(text, "Run git status and report the branch.") {
		t.Fatalf("continuation replayed the previous user ask: %q", text)
	}
	if !strings.Contains(text, "Do not re-run tool calls") {
		t.Fatalf("continuation nudge missing: %q", text)
	}
}

func TestPlainTurnContinuationUnchanged(t *testing.T) {
	snapshot := checkpointSnapshot{
		state: &agentv1.ConversationStateStructure{},
		blobs: map[string][]byte{
			"ddeeff": []byte(`{"role":"assistant","content":[{"type":"text","text":"The branch is main."}]}`),
		},
	}
	messages := []ChatMessage{
		{Role: "user", Content: "Which branch?"},
		{Role: "assistant", Content: "The branch is main."},
		{Role: "user", Content: "And the remote?"},
	}
	if !continuationMatchesHistory(snapshot, messages) {
		t.Fatal("plain text-turn continuation regressed")
	}
	msg, _ := buildContinuationRunRequest("claude-fable-5", messages, nil, "conv-cc2", snapshot)
	action := msg.GetRunRequest().GetAction().GetUserMessageAction()
	if action == nil || action.GetUserMessage().GetText() != "And the remote?" {
		t.Fatalf("plain continuation action changed: %+v", action)
	}
}
