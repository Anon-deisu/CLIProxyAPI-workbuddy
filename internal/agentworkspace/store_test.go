package agentworkspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type scriptedRunner struct {
	mu        sync.Mutex
	requests  []ModelRequest
	responses []ModelResponse
}

func (r *scriptedRunner) Complete(_ context.Context, request ModelRequest, emit func(ModelDelta)) (ModelResponse, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	index := len(r.requests) - 1
	response := r.responses[index]
	r.mu.Unlock()
	if response.Content != "" {
		emit(ModelDelta{Content: response.Content})
	}
	return response, nil
}

type recordingSandbox struct {
	mu       sync.Mutex
	commands []string
	result   CommandResult
}

func (s *recordingSandbox) Run(_ context.Context, _ string, command string) (CommandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
	return s.result, nil
}

func (s *recordingSandbox) Status(context.Context) SandboxStatus {
	return SandboxStatus{Available: true, Image: "test-sandbox"}
}

func TestStoreRunsToolLoopPersistsFilesAndDeletesWorkspace(t *testing.T) {
	runner := &scriptedRunner{responses: []ModelResponse{
		{ToolCalls: []ToolCall{
			{ID: "call-write", Name: "write_file", Arguments: `{"path":"notes/plan.txt","content":"first line"}`},
			{ID: "call-command", Name: "run_command", Arguments: `{"command":"printf ready"}`},
		}},
		{Content: "The plan is ready."},
	}}
	sandbox := &recordingSandbox{result: CommandResult{Output: "ready", ExitCode: 0}}
	store, errNew := NewStore(t.TempDir(), runner, sandbox)
	if errNew != nil {
		t.Fatal(errNew)
	}
	conversation, errCreate := store.Create(ConversationOptions{Model: "coding-model", ReasoningEffort: "high"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	var events []Event
	if errRun := store.Run(context.Background(), conversation.ID, "Create a plan file", ConversationOptions{}, func(event Event) {
		events = append(events, event)
	}); errRun != nil {
		t.Fatal(errRun)
	}

	if len(runner.requests) != 2 {
		t.Fatalf("model calls = %d, want tool round and completion", len(runner.requests))
	}
	if runner.requests[0].Model != "coding-model" || runner.requests[0].ReasoningEffort != "high" {
		t.Fatalf("first model request = %+v", runner.requests[0])
	}
	if !containsMessage(runner.requests[1].Messages, "tool", "ready") || !containsMessage(runner.requests[1].Messages, "tool", "notes/plan.txt") {
		t.Fatalf("second model request is missing tool output: %#v", runner.requests[1].Messages)
	}
	if len(sandbox.commands) != 1 || sandbox.commands[0] != "printf ready" {
		t.Fatalf("sandbox commands = %#v", sandbox.commands)
	}
	if !containsEvent(events, "tool_call") || !containsEvent(events, "tool_result") || !containsEvent(events, "delta") || !containsEvent(events, "completed") {
		t.Fatalf("agent events = %#v", events)
	}

	content, errRead := store.ReadFile(conversation.ID, "notes/plan.txt")
	if errRead != nil || string(content) != "first line" {
		t.Fatalf("workspace file = %q, %v", content, errRead)
	}
	files, errFiles := store.ListFiles(conversation.ID)
	if errFiles != nil || len(files) != 1 || files[0].Path != "notes/plan.txt" {
		t.Fatalf("workspace files = %#v, %v", files, errFiles)
	}

	persisted, errGet := store.Get(conversation.ID)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if persisted.Status != ConversationIdle || !containsMessage(persisted.Messages, "assistant", "The plan is ready.") {
		t.Fatalf("persisted conversation = %#v", persisted)
	}

	reloaded, errReload := NewStore(store.Root(), runner, sandbox)
	if errReload != nil {
		t.Fatal(errReload)
	}
	if restored, errRestore := reloaded.Get(conversation.ID); errRestore != nil || !containsMessage(restored.Messages, "assistant", "The plan is ready.") {
		t.Fatalf("restored conversation = %#v, %v", restored, errRestore)
	}
	if errDelete := reloaded.Delete(conversation.ID); errDelete != nil {
		t.Fatal(errDelete)
	}
	if _, errMissing := reloaded.Get(conversation.ID); !errors.Is(errMissing, ErrConversationNotFound) {
		t.Fatalf("deleted conversation error = %v", errMissing)
	}
	if _, errMissingFile := reloaded.ReadFile(conversation.ID, "notes/plan.txt"); !errors.Is(errMissingFile, ErrConversationNotFound) {
		t.Fatalf("deleted workspace error = %v", errMissingFile)
	}
}

func TestAgentSystemMessageAllowsDirectChatReplies(t *testing.T) {
	prompt := agentSystemMessage().Content
	for _, want := range []string{
		"Answer the user directly in chat",
		"Do not create files or run commands merely to compose, format, print, or save a reply",
		"finish with a user-facing answer",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("agent prompt is missing %q", want)
		}
	}
	if strings.Contains(prompt, "Work only through the provided tools") {
		t.Error("agent prompt still requires tools for ordinary conversation")
	}
}

func TestStoreAnswersWithoutCallingTools(t *testing.T) {
	runner := &scriptedRunner{responses: []ModelResponse{{Content: "Hello! How can I help?"}}}
	sandbox := &recordingSandbox{}
	store, errNew := NewStore(t.TempDir(), runner, sandbox)
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer store.Close()
	conversation, errCreate := store.Create(ConversationOptions{Model: "chat-model"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	if errRun := store.Run(t.Context(), conversation.ID, "Hello", ConversationOptions{}, nil); errRun != nil {
		t.Fatal(errRun)
	}
	if len(runner.requests) != 1 || len(sandbox.commands) != 0 {
		t.Fatal("plain chat must finish in one model turn without a command")
	}
	if runner.requests[0].ToolChoice != "auto" {
		t.Fatal("tools must be optional for ordinary conversation")
	}
	files, errFiles := store.ListFiles(conversation.ID)
	if errFiles != nil || len(files) != 0 {
		t.Fatalf("plain chat created files: %#v, %v", files, errFiles)
	}
	snapshot, errGet := store.Get(conversation.ID)
	if errGet != nil || snapshot.Status != ConversationIdle || snapshot.Messages[len(snapshot.Messages)-1].Content != "Hello! How can I help?" {
		t.Fatalf("plain chat did not retain its final reply: %#v, %v", snapshot, errGet)
	}
}

func TestStoreFinishesWithAnAnswerAtToolRoundLimit(t *testing.T) {
	runner := &scriptedRunner{}
	for round := 0; round < maxToolRounds; round++ {
		runner.responses = append(runner.responses, ModelResponse{ToolCalls: []ToolCall{{
			ID: fmt.Sprintf("call-%d", round), Name: "list_files", Arguments: `{}`,
		}}})
	}
	runner.responses = append(runner.responses, ModelResponse{Content: "The workspace is empty. No files were changed."})
	store, errNew := NewStore(t.TempDir(), runner, &recordingSandbox{})
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer store.Close()
	conversation, errCreate := store.Create(ConversationOptions{Model: "chat-model"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	if errRun := store.Run(t.Context(), conversation.ID, "Inspect the workspace", ConversationOptions{}, nil); errRun != nil {
		t.Fatalf("tool limit should request a final answer rather than discard it: %v", errRun)
	}
	if len(runner.requests) != maxToolRounds+1 {
		t.Fatalf("model turns = %d, want %d including final answer", len(runner.requests), maxToolRounds+1)
	}
	final := runner.requests[maxToolRounds]
	if final.ToolChoice != "none" {
		t.Fatal("tool-limit summary must disable further tool calls")
	}
	if !containsMessage(final.Messages, "system", "No tool calls remain") || !containsMessage(final.Messages, "tool", "Workspace is empty") {
		t.Fatal("final answer request must retain tool results and tell the model to stop using tools")
	}
	snapshot, errGet := store.Get(conversation.ID)
	if errGet != nil || snapshot.Status != ConversationIdle || !containsMessage(snapshot.Messages, "assistant", "No files were changed") {
		t.Fatalf("missing tool-limit summary: %#v, %v", snapshot, errGet)
	}
}

func TestStoreDoesNotExecuteToolsRequestedDuringFinalAnswer(t *testing.T) {
	turns := 0
	runner := ModelRunnerFunc(func(_ context.Context, request ModelRequest, _ func(ModelDelta)) (ModelResponse, error) {
		turns++
		call := ToolCall{ID: fmt.Sprintf("call-%d", turns), Name: "list_files", Arguments: `{}`}
		if request.ToolChoice == "none" {
			call.Name, call.Arguments = "write_file", `{"path":"unexpected.txt","content":"This must not be written"}`
		}
		return ModelResponse{ToolCalls: []ToolCall{call}}, nil
	})
	store, errNew := NewStore(t.TempDir(), runner, &recordingSandbox{})
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer store.Close()
	conversation, errCreate := store.Create(ConversationOptions{Model: "chat-model"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	errRun := store.Run(t.Context(), conversation.ID, "Inspect the workspace", ConversationOptions{}, nil)
	if errRun == nil || !strings.Contains(errRun.Error(), "final text answer") || turns != maxToolRounds+1 {
		t.Fatalf("unexpected finalization: turns=%d, error=%v", turns, errRun)
	}
	files, errFiles := store.ListFiles(conversation.ID)
	if errFiles != nil || len(files) != 0 {
		t.Fatalf("tool executed after the round limit: %#v, %v", files, errFiles)
	}
	snapshot, errGet := store.Get(conversation.ID)
	if errGet != nil || snapshot.Status != ConversationFailed || len(snapshot.Messages[len(snapshot.Messages)-1].ToolCalls) != 0 {
		t.Fatalf("finalization retained an unexecuted tool call: %#v, %v", snapshot, errGet)
	}
}

func TestStoreRejectsWorkspaceTraversal(t *testing.T) {
	store, errNew := NewStore(t.TempDir(), &scriptedRunner{}, &recordingSandbox{})
	if errNew != nil {
		t.Fatal(errNew)
	}
	conversation, errCreate := store.Create(ConversationOptions{Model: "coding-model"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	for _, path := range []string{"../outside.txt", "/etc/passwd", `C:\\Windows\\system.ini`} {
		if _, errRead := store.ReadFile(conversation.ID, path); !errors.Is(errRead, ErrInvalidWorkspacePath) {
			t.Errorf("ReadFile(%q) error = %v", path, errRead)
		}
	}
	if _, errWrite := store.ExecuteTool(context.Background(), conversation.ID, ToolCall{ID: "call", Name: "write_file", Arguments: `{"path":"../outside.txt","content":"no"}`}); !errors.Is(errWrite, ErrInvalidWorkspacePath) {
		t.Fatalf("tool traversal error = %v", errWrite)
	}
}

type blockingRunner struct {
	started chan string
}

func (r *blockingRunner) Complete(ctx context.Context, request ModelRequest, _ func(ModelDelta)) (ModelResponse, error) {
	r.started <- request.ConversationID
	<-ctx.Done()
	return ModelResponse{}, ctx.Err()
}

func TestStoreRunsConversationsInParallelAndCancelsIndependently(t *testing.T) {
	runner := &blockingRunner{started: make(chan string, 2)}
	store, errNew := NewStore(t.TempDir(), runner, &recordingSandbox{})
	if errNew != nil {
		t.Fatal(errNew)
	}
	first, errFirst := store.Create(ConversationOptions{Model: "model-a"})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	second, errSecond := store.Create(ConversationOptions{Model: "model-b"})
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	results := make(chan error, 2)
	go func() { results <- store.Run(context.Background(), first.ID, "first", ConversationOptions{}, nil) }()
	go func() { results <- store.Run(context.Background(), second.ID, "second", ConversationOptions{}, nil) }()

	started := map[string]bool{<-runner.started: true, <-runner.started: true}
	if !started[first.ID] || !started[second.ID] {
		t.Fatalf("started conversations = %#v", started)
	}
	if snapshot, errGet := store.Get(second.ID); errGet != nil || snapshot.Status != ConversationRunning {
		t.Fatalf("second session status = %#v, %v", snapshot, errGet)
	}
	if errCancel := store.Cancel(first.ID); errCancel != nil {
		t.Fatal(errCancel)
	}
	if errCancel := store.Cancel(second.ID); errCancel != nil {
		t.Fatal(errCancel)
	}
	for range 2 {
		if errRun := <-results; !errors.Is(errRun, context.Canceled) {
			t.Fatalf("canceled run error = %v", errRun)
		}
	}
	for _, id := range []string{first.ID, second.ID} {
		snapshot, errGet := store.Get(id)
		if errGet != nil || snapshot.Status != ConversationCanceled {
			t.Fatalf("canceled session %s = %#v, %v", id, snapshot, errGet)
		}
	}
}

func TestDockerRunArgsKeepCommandsInsideAnIsolatedWorkspace(t *testing.T) {
	args := dockerRunArgs("cpa-agent-test", "/tmp/cpa-agent-workspace", "alpine:3.20", "printf ready")
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"run", "--detach", "--pull", "never", "--user", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--pids-limit", "--memory", "--cpus", "--mount", "src=/tmp/cpa-agent-workspace", "dst=/workspace", "/bin/sh", "-lc", "printf ready",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker args missing %q: %#v", want, args)
		}
	}
	if strings.Contains(joined, "host") || strings.Contains(joined, "/var/run/docker.sock") {
		t.Fatalf("docker args expose host access: %#v", args)
	}
}

func containsMessage(messages []Message, role, text string) bool {
	for _, message := range messages {
		if message.Role == role && strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}

func containsEvent(events []Event, kind string) bool {
	for _, event := range events {
		if event.Type == kind {
			return true
		}
	}
	return false
}

type cancelingSandbox struct {
	started chan struct{}
	stopped chan struct{}
}

func (s *cancelingSandbox) Run(ctx context.Context, workspace, command string) (CommandResult, error) {
	close(s.started)
	<-ctx.Done()
	errWrite := os.WriteFile(filepath.Join(workspace, "last-output.txt"), []byte("written while stopping"), 0o600)
	close(s.stopped)
	if errWrite != nil {
		return CommandResult{}, errWrite
	}
	return CommandResult{}, ctx.Err()
}

func (s *cancelingSandbox) Status(context.Context) SandboxStatus {
	return SandboxStatus{Available: true}
}

func TestStoreDeletionWaitsForRunningCommandAndRemovesAllArtifacts(t *testing.T) {
	sandbox := &cancelingSandbox{started: make(chan struct{}), stopped: make(chan struct{})}
	runner := &scriptedRunner{responses: []ModelResponse{{ToolCalls: []ToolCall{{ID: "cmd", Name: "run_command", Arguments: `{"command":"build"}`}}}}}
	store, errNew := NewStore(t.TempDir(), runner, sandbox)
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer store.Close()
	conversation, errCreate := store.Create(ConversationOptions{Model: "test-model"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	if errStart := store.Start(conversation.ID, "build", ConversationOptions{}); errStart != nil {
		t.Fatal(errStart)
	}
	<-sandbox.started
	if errDelete := store.Delete(conversation.ID); errDelete != nil {
		t.Fatal(errDelete)
	}
	<-sandbox.stopped
	if _, errStat := os.Stat(store.workspacePath(conversation.ID)); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("workspace survived deletion: %v", errStat)
	}
	if _, errStat := os.Stat(store.conversationPath(conversation.ID)); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("conversation survived deletion: %v", errStat)
	}
}

func TestStoreFilesCannotFollowSymlinksOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "private.txt")
	if errWrite := os.WriteFile(outside, []byte("outside"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store, errNew := NewStore(filepath.Join(root, "agent"), nil, nil)
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer store.Close()
	c, errCreate := store.Create(ConversationOptions{})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	if errLink := os.Symlink(outside, filepath.Join(store.workspacePath(c.ID), "escape")); errLink != nil {
		t.Skipf("symlinks unavailable: %v", errLink)
	}
	if _, errRead := store.ReadFile(c.ID, "escape"); errRead == nil {
		t.Fatal("read escaped workspace")
	}
	if errWrite := store.WriteFile(c.ID, "escape", []byte("modified")); errWrite == nil {
		t.Fatal("write escaped workspace")
	}
	data, errRead := os.ReadFile(outside)
	if errRead != nil || string(data) != "outside" {
		t.Fatal("external file was changed")
	}
}

func TestStoreDownloadPreservesLargeBinaryArtifacts(t *testing.T) {
	store, errNew := NewStore(t.TempDir(), nil, nil)
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer store.Close()
	c, errCreate := store.Create(ConversationOptions{})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	data := make([]byte, maxToolOutputBytes+1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if errWrite := store.WriteFile(c.ID, "bundle.bin", data); errWrite != nil {
		t.Fatal(errWrite)
	}
	file, errOpen := store.OpenFile(c.ID, "bundle.bin")
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() { _ = file.Close() }()
	actual, errRead := io.ReadAll(file)
	if errRead != nil || string(actual) != string(data) {
		t.Fatal("binary download changed artifact")
	}
}

func TestStoreRestartMarksRunInterruptedAndRepairsPendingTools(t *testing.T) {
	store, errNew := NewStore(t.TempDir(), nil, nil)
	if errNew != nil {
		t.Fatal(errNew)
	}
	c, errCreate := store.Create(ConversationOptions{Model: "test"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	c.Status = ConversationRunning
	c.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Name: "write_file", Arguments: `{"path":"note","content":"x"}`}}}}
	if errPersist := store.persist(c); errPersist != nil {
		t.Fatal(errPersist)
	}
	store.Close()
	restarted, errRestart := NewStore(store.Root(), nil, nil)
	if errRestart != nil {
		t.Fatal(errRestart)
	}
	defer restarted.Close()
	got, errGet := restarted.Get(c.ID)
	if errGet != nil || got.Status != ConversationInterrupted || len(got.Messages) != 2 || got.Messages[1].ToolCallID != "pending" {
		t.Fatalf("restart recovery = %+v %v", got, errGet)
	}
}
