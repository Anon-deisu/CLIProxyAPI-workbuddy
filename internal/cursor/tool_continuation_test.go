package cursor

import (
	"fmt"
	"strings"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func toolTailConversation() []ChatMessage {
	return []ChatMessage{
		{Role: "user", Content: "Run git status and report the branch."},
		{Role: "assistant", Content: "Checking.", ToolCalls: []ToolCall{
			{ID: "toolu_01dead", Name: "run_command", Arguments: map[string]any{"command": "git status"}}}},
		{Role: "tool", ToolCallID: "toolu_01dead", Name: "run_command",
			Content: "On branch main, nothing to commit."},
	}
}

// A rebuilt run whose history ends in tool results used to ship a bare
// ResumeAction; the server has no state for the fresh conversation id and
// answers with an immediate empty END_STREAM — the "N in / 0 out" dashboard
// rows and, once gated, endless 502 retries.
func TestToolResultTailGetsAnExecutableAction(t *testing.T) {
	msg, _, _, err := buildRunRequestForConversation("claude-fable-5", toolTailConversation(), nil, "conv-t1")
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	action := run.GetAction().GetUserMessageAction()
	if action == nil {
		t.Fatalf("tool continuation still built a non-user action: %T", run.GetAction().GetAction())
	}
	if !strings.Contains(action.GetUserMessage().GetText(), "tool results") {
		t.Fatalf("continuation nudge missing: %q", action.GetUserMessage().GetText())
	}
}

func TestPlainQuestionsKeepTheirOwnText(t *testing.T) {
	conversation := []ChatMessage{{Role: "user", Content: "hello there"}}
	msg, _, _, err := buildRunRequestForConversation("claude-fable-5", conversation, nil, "conv-t2")
	if err != nil {
		t.Fatal(err)
	}
	action := msg.GetRunRequest().GetAction().GetUserMessageAction()
	if action == nil || action.GetUserMessage().GetText() != "hello there" {
		t.Fatalf("plain user text was altered: %+v", action)
	}
}

func TestResumeUserAfterToolsUsesNudgeNotBareContinue(t *testing.T) {
	conversation := append(toolTailConversation(), ChatMessage{Role: "user", Content: "继续"})
	msg, _, _, err := buildRunRequestForConversation("claude-fable-5", conversation, nil, "conv-resume")
	if err != nil {
		t.Fatal(err)
	}
	action := msg.GetRunRequest().GetAction().GetUserMessageAction()
	if action == nil {
		t.Fatal("expected an executable user action")
	}
	text := action.GetUserMessage().GetText()
	if text == "继续" || text == "continue" {
		t.Fatalf("bare resume text must not be the action: %q", text)
	}
	if !strings.Contains(text, "tool results") && !strings.Contains(text, "Write") {
		t.Fatalf("resume after tools must use a continuation nudge, got %q", text)
	}
}

func TestReadLoopAfterContinueUsesWriteBreaker(t *testing.T) {
	conversation := []ChatMessage{
		{Role: "user", Content: "把动画写成完整 html 文件"},
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("toolu_read_%d", i)
		conversation = append(conversation,
			ChatMessage{Role: "assistant", Content: "现在真正写入文件。", ToolCalls: []ToolCall{
				{ID: id, Name: "Read", Arguments: map[string]any{"path": "pelican-ride/v2.html"}}}},
			ChatMessage{Role: "tool", ToolCallID: id, Name: "Read", Content: "File not found"},
		)
	}
	conversation = append(conversation, ChatMessage{Role: "user", Content: "继续"})
	msg, _, _, err := buildRunRequestForConversation("claude-fable-5", conversation, nil, "conv-loop")
	if err != nil {
		t.Fatal(err)
	}
	text := msg.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if text != writeLoopBreakerNudge {
		t.Fatalf("read/write loop must use the write breaker, got %q", text)
	}
}

func TestTrailingUserTextWinsOverToolResults(t *testing.T) {
	conversation := append(toolTailConversation(),
		ChatMessage{Role: "user", Content: "Also list untracked files."})
	msg, _, _, err := buildRunRequestForConversation("claude-fable-5", conversation, nil, "conv-t3")
	if err != nil {
		t.Fatal(err)
	}
	action := msg.GetRunRequest().GetAction().GetUserMessageAction()
	if action == nil || !strings.Contains(action.GetUserMessage().GetText(), "untracked") {
		t.Fatalf("real user instruction was replaced: %+v", action)
	}
}

// The client prefill / "continue" pattern: [user, assistant] with no trailing
// user text. A bare ResumeAction here died the same silent death as the tool
// tail (empty END_STREAM, then 502 retry loops).
func TestAssistantTailGetsAContinuationAction(t *testing.T) {
	conversation := []ChatMessage{
		{Role: "user", Content: "Write a long essay about caching."},
		{Role: "assistant", Content: "Caching is the art of remembering. It"},
	}
	msg, _, _, err := buildRunRequestForConversation("claude-fable-5", conversation, nil, "conv-t4")
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	action := run.GetAction().GetUserMessageAction()
	if action == nil {
		t.Fatalf("assistant tail still built a non-user action: %T", run.GetAction().GetAction())
	}
	if action.GetUserMessage().GetText() != assistantContinuationNudge {
		t.Fatalf("expected the continuation nudge, got %q", action.GetUserMessage().GetText())
	}
}

func TestEndsWithToolResultsDetection(t *testing.T) {
	if !conversationEndsWithToolResults(toolTailConversation()) {
		t.Fatal("tool tail not detected")
	}
	if conversationEndsWithToolResults([]ChatMessage{{Role: "user", Content: "hi"}}) {
		t.Fatal("plain user tail misdetected")
	}
	if conversationEndsWithToolResults(append(toolTailConversation(),
		ChatMessage{Role: "assistant", Content: "done"})) {
		t.Fatal("assistant tail after tools misdetected")
	}
	var _ *agentv1.ResumeAction // keep import for clarity of the regression class
}
