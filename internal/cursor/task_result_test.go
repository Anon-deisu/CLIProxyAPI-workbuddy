package cursor

import (
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func taskStep(task *agentv1.TaskToolCall) []byte {
	step := &agentv1.ConversationStep{
		Message: &agentv1.ConversationStep_ToolCall{
			ToolCall: &agentv1.ToolCall{
				Tool: &agentv1.ToolCall_TaskToolCall{TaskToolCall: task},
			},
		},
	}
	raw, err := proto.Marshal(step)
	if err != nil {
		panic(err)
	}
	return raw
}

func successSteps(args ...string) []*agentv1.ConversationStep {
	steps := make([]*agentv1.ConversationStep, 0, len(args))
	for _, text := range args {
		steps = append(steps, &agentv1.ConversationStep{
			Message: &agentv1.ConversationStep_AssistantMessage{
				AssistantMessage: &agentv1.AssistantMessage{Text: text},
			},
		})
	}
	return steps
}

func TestTaskToolCallFinalTextSuccessLastStep(t *testing.T) {
	task := &agentv1.TaskToolCall{
		Result: &agentv1.TaskResult{
			Result: &agentv1.TaskResult_Success{
				Success: &agentv1.TaskSuccess{
					ConversationSteps: successSteps(
						"Scanning the module for the defect…",
						"The bug is a nil dereference in session.close.",
					),
				},
			},
		},
	}
	if got := taskToolCallFinalText(task); got != "The bug is a nil dereference in session.close." {
		t.Fatalf("final text = %q", got)
	}
}

func TestTaskToolCallFinalTextSkipsStatusWords(t *testing.T) {
	task := &agentv1.TaskToolCall{
		Result: &agentv1.TaskResult{
			Result: &agentv1.TaskResult_Success{
				Success: &agentv1.TaskSuccess{
					ConversationSteps: successSteps("created", "running subagent", ""),
				},
			},
		},
	}
	// The last non-empty assistant message is the conclusion, never a status
	// word that happens to appear earlier in the transcript.
	if got := taskToolCallFinalText(task); got != "running subagent" {
		t.Fatalf("final text = %q", got)
	}
}

func TestTaskToolCallFinalTextError(t *testing.T) {
	task := &agentv1.TaskToolCall{
		Result: &agentv1.TaskResult{
			Result: &agentv1.TaskResult_Error{
				Error: &agentv1.TaskError{Error: "subagent timed out after 60s"},
			},
		},
	}
	if got := taskToolCallFinalText(task); got != "subagent timed out after 60s" {
		t.Fatalf("error text = %q", got)
	}
}

func TestTaskToolCallFinalTextEmptySurfacesNothing(t *testing.T) {
	cases := []*agentv1.TaskToolCall{
		nil,
		{},
		{Result: &agentv1.TaskResult{}},
		{Result: &agentv1.TaskResult{Result: &agentv1.TaskResult_Success{Success: &agentv1.TaskSuccess{}}}},
	}
	for i, task := range cases {
		if got := taskToolCallFinalText(task); got != "" {
			t.Fatalf("case %d: expected empty, got %q", i, got)
		}
	}
}

func TestParseAssistantBlobSurfacesTaskConclusion(t *testing.T) {
	raw := taskStep(&agentv1.TaskToolCall{
		Result: &agentv1.TaskResult{
			Result: &agentv1.TaskResult_Success{
				Success: &agentv1.TaskSuccess{
					ConversationSteps: successSteps("Fixed the read-slot race."),
				},
			},
		},
	})
	msg, ok := parseAssistantBlob("task-blob", raw)
	if !ok {
		t.Fatal("completed task step should surface assistant text")
	}
	if msg.text != "Fixed the read-slot race." {
		t.Fatalf("text = %q", msg.text)
	}
	if msg.identity != "blob:task-blob:task" {
		t.Fatalf("identity = %q", msg.identity)
	}
}

func TestParseAssistantBlobIgnoresIncompleteTask(t *testing.T) {
	// No result yet: the subagent is still running, so nothing may surface.
	raw := taskStep(&agentv1.TaskToolCall{Args: &agentv1.TaskArgs{Prompt: "audit usage"}})
	if _, ok := parseAssistantBlob("running", raw); ok {
		t.Fatal("an incomplete task must not surface assistant text")
	}
}
