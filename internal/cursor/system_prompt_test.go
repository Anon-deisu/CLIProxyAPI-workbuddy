package cursor

import (
	"encoding/json"
	"strings"
	"testing"
)

// blobRoles decodes the run's prefetched blobs into (role, text) pairs.
func blobRoles(t *testing.T, blobs map[string][]byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, data := range blobs {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		role, _ := payload["role"].(string)
		parts, _ := payload["content"].([]any)
		var text strings.Builder
		for _, part := range parts {
			m, _ := part.(map[string]any)
			if s, ok := m["text"].(string); ok {
				text.WriteString(s)
			}
		}
		out[role] += text.String()
	}
	return out
}

// Cursor's Agent accepts a root-prompt blob with role "system" and then
// discards it: the same text scored identical prompt_tokens whether it was sent
// or omitted, and instructions in it were never followed. It must travel as a
// user turn, which does arrive.
func TestClientSystemPromptTravelsAsAUserTurn(t *testing.T) {
	const rule = "Always answer in Latin."
	_, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "system", Content: rule},
		{Role: "user", Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	roles := blobRoles(t, blobs)
	if strings.Contains(roles["system"], rule) {
		t.Fatalf("system prompt still rides a discarded system blob: %q", roles["system"])
	}
	if !strings.Contains(roles["user"], rule) {
		t.Fatalf("system prompt never reached a user blob: %#v", roles)
	}
	if !strings.Contains(roles["user"], systemInstructionLabel) {
		t.Fatal("relayed system prompt lost its instruction label")
	}
}

// Several system messages are joined before relaying, and the active user turn
// must stay the user's own words rather than absorbing the instructions.
func TestSystemPromptsAreJoinedAndKeptOffTheActiveTurn(t *testing.T) {
	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "system", Content: "Rule one."},
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "Rule two."},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	roles := blobRoles(t, blobs)
	if !strings.Contains(roles["user"], "Rule one.") || !strings.Contains(roles["user"], "Rule two.") {
		t.Fatalf("both system messages must be relayed: %q", roles["user"])
	}
	action := msg.GetRunRequest().GetAction().GetUserMessageAction()
	if action == nil {
		t.Fatal("run lost its user action")
	}
	if got := action.GetUserMessage().GetText(); got != "hi" {
		t.Fatalf("active turn = %q, want the user's own text", got)
	}
}

// With no client system prompt the historical placeholder shape is kept so the
// root prompt is never empty.
func TestAbsentSystemPromptKeepsThePlaceholderBlob(t *testing.T) {
	_, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	roles := blobRoles(t, blobs)
	if !strings.Contains(roles["system"], "You are a helpful assistant.") {
		t.Fatalf("placeholder system blob missing: %#v", roles)
	}
	if strings.Contains(roles["user"], systemInstructionLabel) {
		t.Fatal("no client system prompt should produce no relayed instruction block")
	}
}
