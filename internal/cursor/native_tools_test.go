package cursor

import (
	"encoding/binary"
	"io"
	"runtime"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestCustomMcpToolsExcludeTypedNativeTools(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "Read"}, {Name: "Write"}, {Name: "Glob"}, {Name: "Grep"},
		{Name: "Bash"}, {Name: "Edit"}, {Name: "TodoWrite"}, {Name: "get_weather"},
		{Name: "Read", Kind: "custom"}, {Name: "Read", Namespace: "codex_app"},
	}
	got := customMcpTools(tools)
	want := []string{"get_weather"}
	if len(got) != len(want) {
		t.Fatalf("custom tools = %#v, want %v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("custom tool %d = %q, want %q", i, got[i].Name, want[i])
		}
	}
}

func newProtocolSession(tools []ToolDefinition) *Session {
	s := &Session{
		tools:            tools,
		toolIndex:        indexTools(tools),
		pending:          map[string]*pendingExec{},
		batches:          map[string]*pendingToolBatch{},
		lifecyclesByID:   map[string]*nativeLifecycle{},
		serverOwnedTodos: map[string]*nativeLifecycle{},
		events:           make(chan StreamEvent, 16),
	}
	return s
}

func TestNativeLifecycleBeforeExecBindsAuthoritativeIDAndBatch(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	s.handleToolLifecycle("envelope-read", "model-call-1", &agentv1.ToolCall{
		ToolCallId: proto.String("toolu_read"),
		Tool:       &agentv1.ToolCall_ReadToolCall{ReadToolCall: &agentv1.ReadToolCall{Args: &agentv1.ReadToolArgs{Path: "a.txt"}}},
	}, false)
	request := &agentv1.ExecServerMessage{Id: 1, ExecId: "exec-1", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "a.txt"}}}
	if _, err := s.handleNativeArgs(request, "read_args", request.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	pending := s.pending["toolu_read"]
	if pending == nil || pending.call.ID != "toolu_read" || pending.modelCallID != "model-call-1" {
		t.Fatalf("pending=%#v", pending)
	}
	if pending.batch == nil || pending.batch.id != "model-call-1" {
		t.Fatalf("batch=%#v", pending.batch)
	}
}

func TestLifecycleOuterToolIDOverridesNestedExecID(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	s.handleToolLifecycle("envelope-read", "model-call-outer", &agentv1.ToolCall{
		ToolCallId: proto.String("provider-outer"),
		Tool:       &agentv1.ToolCall_McpToolCall{McpToolCall: &agentv1.McpToolCall{Args: &agentv1.McpArgs{ToolCallId: "nested-exec", ToolName: "Read"}}},
	}, false)
	lifecycle := s.lifecyclesByID["nested-exec"]
	if lifecycle == nil || lifecycle.callID != "provider-outer" {
		t.Fatalf("lifecycle=%#v", lifecycle)
	}
}

func TestNativeExecBeforeLifecycleRebindsPendingAndManager(t *testing.T) {
	manager := NewSessionManager()
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	s.ID = "session-rebind"
	s.manager = manager
	manager.Register(s)
	request := &agentv1.ExecServerMessage{Id: 2, ExecId: "exec-2", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "b.txt"}}}
	if _, err := s.handleNativeArgs(request, "read_args", request.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	var provisional string
	for id, pending := range s.pending {
		if pending != nil && pending.provisional {
			provisional = id
			break
		}
	}
	if provisional == "" {
		t.Fatal("expected provisional Exec identity")
	}
	s.handleToolLifecycle("envelope-read", "model-call-2", &agentv1.ToolCall{
		ToolCallId: proto.String("toolu_authoritative"),
		Tool:       &agentv1.ToolCall_ReadToolCall{ReadToolCall: &agentv1.ReadToolCall{Args: &agentv1.ReadToolArgs{Path: "b.txt"}}},
	}, false)
	pending := s.pending["toolu_authoritative"]
	if pending == nil || pending.provisional || pending.modelCallID != "model-call-2" {
		t.Fatalf("rebound pending=%#v", pending)
	}
	if manager.LookupPending("toolu_authoritative") != s {
		t.Fatal("manager did not bind authoritative id")
	}
	if s.pending[provisional] == nil || manager.LookupPending(provisional) != s {
		t.Fatal("provisional id must stay resolvable after the authoritative rebind")
	}
}

func TestCheckpointBeforeExecAttachesWhenExecArrives(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	state := &agentv1.ConversationStateStructure{PendingToolCalls: []string{
		`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"call-checkpoint","toolName":"Read"}]}`,
	}}
	s.applyCheckpointToolBatches(state)
	request := &agentv1.ExecServerMessage{Id: 3, ExecId: "exec-3", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "c.txt"}}}
	if _, err := s.handleNativeArgs(request, "read_args", request.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	pending := s.pending["call-checkpoint"]
	if pending == nil || pending.provisional {
		t.Fatalf("checkpoint did not bind late Exec: %#v", pending)
	}
	if pending.batch == nil || !pending.batch.checkpoint {
		t.Fatalf("checkpoint batch=%#v", pending.batch)
	}
}

func TestMixedNativeAndMcpShareModelCallBatch(t *testing.T) {
	s := newProtocolSession(nil)
	native := &pendingExec{call: ToolCall{ID: "call-native", Name: "Read"}, variant: "read_args", modelCallID: "model-mixed"}
	mcp := &pendingExec{call: ToolCall{ID: "call-mcp", Name: "weather"}, variant: "mcp_args", modelCallID: "model-mixed"}
	s.bindPendingExec(native, native.call.ID)
	s.bindPendingExec(mcp, mcp.call.ID)
	if native.batch == nil || native.batch != mcp.batch || len(native.batch.ordered) != 2 {
		t.Fatalf("native batch=%#v mcp batch=%#v", native.batch, mcp.batch)
	}
}

func TestTerminalDoesNotMergeDifferentModelCallBatches(t *testing.T) {
	s := newProtocolSession(nil)
	one := &pendingExec{call: ToolCall{ID: "call-1", Name: "one"}, modelCallID: "model-1"}
	two := &pendingExec{call: ToolCall{ID: "call-2", Name: "two"}, modelCallID: "model-2"}
	s.bindPendingExec(one, one.call.ID)
	s.bindPendingExec(two, two.call.ID)
	s.markPendingBatchesTerminal()
	batch, ready := s.orderedPendingToolBatch(true)
	if !ready || len(batch) != 1 || batch[0] != one {
		t.Fatalf("first terminal batch=%#v ready=%v", batch, ready)
	}
}

func TestEmittedBatchDoesNotHideNextModelCallBatch(t *testing.T) {
	s := newProtocolSession(nil)
	one := &pendingExec{call: ToolCall{ID: "call-1", Name: "one"}, modelCallID: "model-1", emitted: true}
	two := &pendingExec{call: ToolCall{ID: "call-2", Name: "two"}, modelCallID: "model-2"}
	firstBatch := &pendingToolBatch{id: "model-1", modelCallID: "model-1", ordered: []*pendingExec{one}, emitted: true, terminal: true}
	secondBatch := &pendingToolBatch{id: "model-2", modelCallID: "model-2", ordered: []*pendingExec{two}, terminal: true}
	one.batch, two.batch = firstBatch, secondBatch
	s.pendingOrder = []*pendingExec{one, two}
	s.batchOrder = []*pendingToolBatch{firstBatch, secondBatch}
	s.batches = map[string]*pendingToolBatch{"model-1": firstBatch, "model-2": secondBatch}
	pending, ready := s.orderedPendingToolBatch(true)
	if !ready || len(pending) != 1 || pending[0] != two {
		t.Fatalf("next batch=%#v ready=%v", pending, ready)
	}
}

func TestServerOwnedTodoLifecycleNeverCreatesPending(t *testing.T) {
	s := newProtocolSession(nil)
	s.handleToolLifecycle("todo-envelope", "model-todo", &agentv1.ToolCall{
		ToolCallId: proto.String("todo-call"),
		Tool:       &agentv1.ToolCall_UpdateTodosToolCall{UpdateTodosToolCall: &agentv1.UpdateTodosToolCall{Args: &agentv1.UpdateTodosArgs{}}},
	}, false)
	if len(s.pending) != 0 || len(s.pendingOrder) != 0 {
		t.Fatalf("todo created downstream pending: %#v", s.pending)
	}
	if s.serverOwnedTodos["todo-call"] == nil {
		t.Fatal("todo lifecycle was not retained")
	}
}

func TestTypedApplyPatchMappings(t *testing.T) {
	def := &ToolDefinition{Name: "apply_patch", Kind: "apply_patch", Parameters: map[string]any{"type": "object"}}
	write, err := nativeToolArguments("write_args", &agentv1.WriteArgs{Path: "new.txt", FileText: "hello"}, def)
	if err != nil || write["operation"].(map[string]any)["type"] != "create_file" {
		t.Fatalf("write mapping=%#v err=%v", write, err)
	}
	deleted, err := nativeToolArguments("delete_args", &agentv1.DeleteArgs{Path: "old.txt"}, def)
	if err != nil || deleted["operation"].(map[string]any)["type"] != "delete_file" {
		t.Fatalf("delete mapping=%#v err=%v", deleted, err)
	}
	updated, err := nativeToolArguments("pi_edit_args", &agentv1.PiEditExecArgs{Path: "main.go", Edits: []*agentv1.PiEditReplacement{{OldText: "old", NewText: "new"}}}, def)
	operation := updated["operation"].(map[string]any)
	if err != nil || operation["type"] != "update_file" || operation["diff"] == "" {
		t.Fatalf("edit mapping=%#v err=%v", updated, err)
	}
}

func TestLocalShellMappingUsesInterpreterArgv(t *testing.T) {
	def := &ToolDefinition{Name: "local_shell", Kind: "local_shell", Parameters: map[string]any{"type": "object"}}
	args, err := nativeToolArguments("shell_args", &agentv1.ShellArgs{Command: "echo ok"}, def)
	if err != nil {
		t.Fatal(err)
	}
	command := args["command"].([]string)
	if len(command) < 3 || command[len(command)-1] != "echo ok" {
		t.Fatalf("command=%#v", command)
	}
	if runtime.GOOS == "windows" && command[0] != "powershell" {
		t.Fatalf("windows command=%#v", command)
	}
}

func TestNativeSubagentSynthesizesDeclaredDescription(t *testing.T) {
	def := &ToolDefinition{Name: "Task", Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":        map[string]any{"type": "string"},
			"description":   map[string]any{"type": "string"},
			"subagent_type": map[string]any{"type": "string"},
		},
	}}
	args, err := nativeToolArguments("subagent_args", &agentv1.SubagentArgs{
		Prompt:       "Inspect the authentication flow.\nReturn only concrete findings.",
		SubagentType: "explore",
	}, def)
	if err != nil {
		t.Fatal(err)
	}
	if got := args["description"]; got != "Inspect the authentication flow." {
		t.Fatalf("description = %#v", got)
	}
	if got := args["subagent_type"]; got != "explore" {
		t.Fatalf("subagent_type = %#v", got)
	}
}

func TestNativeSubagentDescriptionIsSchemaSafeAndRuneBounded(t *testing.T) {
	withoutDescription := &ToolDefinition{Name: "custom_subagent", Parameters: map[string]any{
		"properties": map[string]any{"prompt": map[string]any{"type": "string"}},
	}}
	args, err := nativeToolArguments("subagent_args", &agentv1.SubagentArgs{Prompt: "Do the work"}, withoutDescription)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := args["description"]; exists {
		t.Fatal("description must not be injected into a schema that does not declare it")
	}

	longPrompt := ""
	for i := 0; i < 130; i++ {
		longPrompt += "界"
	}
	description := subagentDescription(longPrompt, "")
	if got := len([]rune(description)); got != 120 {
		t.Fatalf("description rune length = %d, want 120", got)
	}
	if description[len(description)-len("…"):] != "…" {
		t.Fatalf("truncated description = %q", description)
	}
	if fallback := subagentDescription(" \n ", "explore"); fallback != "Delegate this task to the explore subagent." {
		t.Fatalf("fallback description = %q", fallback)
	}
}

func TestNativeSubagentModelRespectsDeclaredEnum(t *testing.T) {
	enumDef := &ToolDefinition{Name: "Task", Parameters: map[string]any{
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string"},
			"model": map[string]any{
				"type": "string",
				"enum": []any{"sonnet", "opus", "haiku", "fable"},
			},
		},
	}}
	tests := []struct {
		name    string
		modelID string
		want    string
		present bool
	}{
		{name: "empty", modelID: "", present: false},
		{name: "sonnet wire id", modelID: "cursor-claude-4.6-sonnet", want: "sonnet", present: true},
		{name: "opus wire id", modelID: "cursor-claude-opus-5", want: "opus", present: true},
		{name: "haiku wire id", modelID: "cursor-claude-haiku-4.5", want: "haiku", present: true},
		{name: "fable wire id", modelID: "cursor-claude-fable-5", want: "fable", present: true},
		{name: "family with suffix", modelID: "cursor-claude-opus-5-fast", want: "opus", present: true},
		{name: "family with thinking variant", modelID: "claude-sonnet-4.5-thinking", want: "sonnet", present: true},
		{name: "exact enum", modelID: "fable", want: "fable", present: true},
		{name: "mixed case", modelID: "Cursor-Claude-4.6-Sonnet", want: "sonnet", present: true},
		{name: "unrepresentable cursor model", modelID: "composer-2.5-fast", present: false},
		{name: "non claude model", modelID: "gpt-5.4", present: false},
		{name: "embedded token", modelID: "sonsonnet", present: false},
		{name: "embedded token with prefix", modelID: "myopus-v3", present: false},
		{name: "unknown family", modelID: "cursor-claude-4.6-turbo", present: false},
		{name: "standalone family without claude", modelID: "fast-opus-x", want: "opus", present: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args, err := nativeToolArguments("subagent_args", &agentv1.SubagentArgs{
				Prompt:  "Inspect authentication",
				ModelId: test.modelID,
			}, enumDef)
			if err != nil {
				t.Fatal(err)
			}
			got, present := args["model"]
			if present != test.present || (present && got != test.want) {
				t.Fatalf("model = %#v, present=%v; want %q, present=%v", got, present, test.want, test.present)
			}
		})
	}
}

func TestNativeSubagentModelIsSchemaSafe(t *testing.T) {
	withoutModel := &ToolDefinition{Name: "Task", Parameters: map[string]any{
		"properties": map[string]any{"prompt": map[string]any{"type": "string"}},
	}}
	args, err := nativeToolArguments("subagent_args", &agentv1.SubagentArgs{
		Prompt: "Inspect authentication", ModelId: "composer-2.5-fast",
	}, withoutModel)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := args["model"]; present {
		t.Fatal("model must not be injected into a schema that does not declare it")
	}

	unrestricted := &ToolDefinition{Name: "Task", Parameters: map[string]any{
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string"},
			"model":  map[string]any{"type": "string"},
		},
	}}
	args, err = nativeToolArguments("subagent_args", &agentv1.SubagentArgs{
		Prompt: "Inspect authentication", ModelId: "composer-2.5-fast",
	}, unrestricted)
	if err != nil {
		t.Fatal(err)
	}
	if got := args["model"]; got != "composer-2.5-fast" {
		t.Fatalf("unrestricted model = %#v", got)
	}
}

func TestNativeSubagentOptionalFieldsRespectSchema(t *testing.T) {
	resume := "agent-123"
	background := false
	args, err := nativeToolArguments("subagent_args", &agentv1.SubagentArgs{
		Prompt:          "Inspect authentication",
		SubagentType:    "explore",
		ModelId:         "composer-2.5-fast",
		ResumeAgentId:   &resume,
		RunInBackground: &background,
	}, &ToolDefinition{Name: "strict_task", Parameters: map[string]any{
		"properties": map[string]any{
			"prompt":      map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"model": map[string]any{
				"type": "string",
				"enum": []any{"sonnet", "opus", "haiku", "fable"},
			},
		},
		"additionalProperties": false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args["prompt"] == nil || args["description"] == nil {
		t.Fatalf("strict arguments contain undeclared fields: %#v", args)
	}

	args, err = nativeToolArguments("subagent_args", &agentv1.SubagentArgs{
		Prompt:          "Inspect authentication",
		SubagentType:    "explore",
		ResumeAgentId:   &resume,
		RunInBackground: &background,
	}, &ToolDefinition{Name: "Task", Parameters: map[string]any{
		"properties": map[string]any{
			"prompt":             map[string]any{"type": "string"},
			"subagent_type_name": map[string]any{"type": "string"},
			"resume_agent_id":    map[string]any{"type": "string"},
			"runInBackground":    map[string]any{"type": "boolean"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if args["subagent_type_name"] != "explore" ||
		args["resume_agent_id"] != resume ||
		args["runInBackground"] != false {
		t.Fatalf("declared aliases were not mapped: %#v", args)
	}
}

func TestNativeReadExecRoundTripUsesDeclaredToolName(t *testing.T) {
	offset := int32(4)
	limit := uint32(20)
	req := &agentv1.ExecServerMessage{
		Id: 19,
		Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{
			Path: "a.txt", ToolCallId: "call-read", Offset: &offset, Limit: &limit,
		}},
	}
	raw, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &agentv1.ExecServerMessage{}
	if err = proto.Unmarshal(raw, decoded); err != nil {
		t.Fatal(err)
	}
	def := &ToolDefinition{Name: "Read", Parameters: map[string]any{
		"properties": map[string]any{"file_path": map[string]any{}, "offset": map[string]any{}, "limit": map[string]any{}},
	}}
	args, err := nativeToolArguments("read_args", decoded.GetReadArgs(), def)
	if err != nil {
		t.Fatal(err)
	}
	if args["file_path"] != "a.txt" || args["offset"] != int32(4) || args["limit"] != uint32(20) {
		t.Fatalf("arguments = %#v", args)
	}
}

func TestExecServerMessageDecodesEnvelopeMetadata(t *testing.T) {
	span := protowire.AppendTag(nil, 1, protowire.BytesType)
	span = protowire.AppendString(span, "0123456789abcdef0123456789abcdef")
	span = protowire.AppendTag(span, 2, protowire.BytesType)
	span = protowire.AppendString(span, "0123456789abcdef")
	span = protowire.AppendTag(span, 3, protowire.VarintType)
	span = protowire.AppendVarint(span, 1)
	span = protowire.AppendTag(span, 4, protowire.BytesType)
	span = protowire.AppendString(span, "vendor=cursor")

	readArgs, err := proto.Marshal(&agentv1.ReadArgs{Path: "metadata.txt", ToolCallId: "call-metadata"})
	if err != nil {
		t.Fatal(err)
	}
	raw := protowire.AppendTag(nil, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 91)
	raw = protowire.AppendTag(raw, 15, protowire.BytesType)
	raw = protowire.AppendString(raw, "exec-metadata")
	raw = protowire.AppendTag(raw, 19, protowire.BytesType)
	raw = protowire.AppendBytes(raw, span)
	raw = protowire.AppendTag(raw, 7, protowire.BytesType)
	raw = protowire.AppendBytes(raw, readArgs)
	raw = protowire.AppendTag(raw, 55, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 1)

	decoded := &agentv1.ExecServerMessage{}
	if err = proto.Unmarshal(raw, decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetId() != 91 || decoded.GetExecId() != "exec-metadata" {
		t.Fatalf("exec envelope id=%d exec_id=%q", decoded.GetId(), decoded.GetExecId())
	}
	gotSpan := decoded.GetSpanContext()
	if gotSpan == nil || gotSpan.GetTraceId() != "0123456789abcdef0123456789abcdef" || gotSpan.GetSpanId() != "0123456789abcdef" || gotSpan.GetTraceFlags() != 1 || gotSpan.GetTraceState() != "vendor=cursor" {
		t.Fatalf("span_context=%#v", gotSpan)
	}
	if !decoded.GetAcceptHookAdditionalContexts() {
		t.Fatal("accept_hook_additional_contexts was not decoded")
	}
	if decoded.GetReadArgs().GetPath() != "metadata.txt" || decoded.GetReadArgs().GetToolCallId() != "call-metadata" {
		t.Fatalf("read_args=%#v", decoded.GetReadArgs())
	}
	if fields := unknownExecFieldNumbers(decoded); len(fields) != 0 {
		t.Fatalf("known Exec envelope metadata remained unknown: %v", fields)
	}
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	if _, err = s.handleExec(decoded); err != nil {
		t.Fatal(err)
	}
	if pending := s.pending["call-metadata"]; pending == nil || pending.variant != "read_args" {
		t.Fatalf("metadata-bearing native Exec was not dispatched: %#v", pending)
	}
}

func TestExecEnvelopeMetadataOnlyClosesNormally(t *testing.T) {
	traceFlags := uint32(1)
	acceptContexts := true
	req := &agentv1.ExecServerMessage{
		Id: 92,
		SpanContext: &agentv1.SpanContext{
			TraceId:    "0123456789abcdef0123456789abcdef",
			SpanId:     "0123456789abcdef",
			TraceFlags: &traceFlags,
		},
		AcceptHookAdditionalContexts: &acceptContexts,
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	payloadCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		header := make([]byte, 5)
		if _, err := io.ReadFull(reader, header); err != nil {
			errCh <- err
			return
		}
		payload := make([]byte, int(binary.BigEndian.Uint32(header[1:])))
		if _, err := io.ReadFull(reader, payload); err != nil {
			errCh <- err
			return
		}
		payloadCh <- payload
	}()

	s := &Session{stream: &BidiStream{writer: writer}}
	handled, err := s.handleExec(req)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("metadata-only Exec unexpectedly entered tool handling")
	}

	var payload []byte
	select {
	case err = <-errCh:
		t.Fatal(err)
	case payload = <-payloadCh:
	}
	client := &agentv1.AgentClientMessage{}
	if err = proto.Unmarshal(payload, client); err != nil {
		t.Fatal(err)
	}
	control := client.GetExecClientControlMessage()
	if control == nil || control.GetStreamClose().GetId() != req.GetId() {
		t.Fatalf("metadata-only response=%#v", control)
	}
	if control.GetThrow() != nil {
		t.Fatalf("metadata-only Exec was refused: %#v", control.GetThrow())
	}
}

func TestKnownNativeExecWithoutClientToolReturnsTypedError(t *testing.T) {
	traceFlags := uint32(1)
	acceptContexts := true
	req := &agentv1.ExecServerMessage{
		Id:     93,
		ExecId: "exec-unavailable-read",
		SpanContext: &agentv1.SpanContext{
			TraceId:    "0123456789abcdef0123456789abcdef",
			SpanId:     "0123456789abcdef",
			TraceFlags: &traceFlags,
		},
		AcceptHookAdditionalContexts: &acceptContexts,
		Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{
			Path: "unavailable.txt",
		}},
	}
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	payloadCh := make(chan [][]byte, 1)
	errCh := make(chan error, 1)
	go func() {
		payloads := make([][]byte, 0, 2)
		for range 2 {
			header := make([]byte, 5)
			if _, err := io.ReadFull(reader, header); err != nil {
				errCh <- err
				return
			}
			payload := make([]byte, int(binary.BigEndian.Uint32(header[1:])))
			if _, err := io.ReadFull(reader, payload); err != nil {
				errCh <- err
				return
			}
			payloads = append(payloads, payload)
		}
		payloadCh <- payloads
	}()

	s := newProtocolSession(nil)
	s.stream = &BidiStream{writer: writer}
	handled, err := s.handleExec(req)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("unavailable native Exec unexpectedly entered client tool handling")
	}

	var payloads [][]byte
	select {
	case err = <-errCh:
		t.Fatal(err)
	case payloads = <-payloadCh:
	}
	resultEnvelope := &agentv1.AgentClientMessage{}
	if err = proto.Unmarshal(payloads[0], resultEnvelope); err != nil {
		t.Fatal(err)
	}
	execResult := resultEnvelope.GetExecClientMessage()
	readError := execResult.GetReadResult().GetError()
	if execResult.GetId() != req.GetId() || execResult.GetExecId() != req.GetExecId() || readError.GetPath() != "unavailable.txt" || readError.GetError() == "" {
		t.Fatalf("typed unavailable result=%#v", execResult)
	}
	closeEnvelope := &agentv1.AgentClientMessage{}
	if err = proto.Unmarshal(payloads[1], closeEnvelope); err != nil {
		t.Fatal(err)
	}
	control := closeEnvelope.GetExecClientControlMessage()
	if control == nil || control.GetStreamClose().GetId() != req.GetId() || control.GetThrow() != nil {
		t.Fatalf("unavailable native close=%#v", control)
	}
}

func TestUnknownExecVariantRemainsVisible(t *testing.T) {
	raw := protowire.AppendTag(nil, 99, protowire.BytesType)
	raw = protowire.AppendBytes(raw, nil)
	decoded := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(raw, decoded); err != nil {
		t.Fatal(err)
	}
	fields := unknownExecFieldNumbers(decoded)
	if len(fields) != 1 || fields[0] != 99 {
		t.Fatalf("unknown Exec fields=%v, want [99]", fields)
	}

	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	payloadCh := make(chan [][]byte, 1)
	errCh := make(chan error, 1)
	go func() {
		payloads := make([][]byte, 0, 2)
		for range 2 {
			header := make([]byte, 5)
			if _, err := io.ReadFull(reader, header); err != nil {
				errCh <- err
				return
			}
			payload := make([]byte, int(binary.BigEndian.Uint32(header[1:])))
			if _, err := io.ReadFull(reader, payload); err != nil {
				errCh <- err
				return
			}
			payloads = append(payloads, payload)
		}
		payloadCh <- payloads
	}()
	s := newProtocolSession(nil)
	s.stream = &BidiStream{writer: writer}
	if _, err := s.handleExec(decoded); err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	select {
	case err := <-errCh:
		t.Fatal(err)
	case payloads = <-payloadCh:
	}
	throwEnvelope := &agentv1.AgentClientMessage{}
	if err := proto.Unmarshal(payloads[0], throwEnvelope); err != nil {
		t.Fatal(err)
	}
	throw := throwEnvelope.GetExecClientControlMessage().GetThrow()
	if throw == nil || throw.GetErrorCode() != "exec_variant_unsupported" {
		t.Fatalf("unknown Exec response=%#v", throwEnvelope.GetExecClientControlMessage())
	}
	closeEnvelope := &agentv1.AgentClientMessage{}
	if err := proto.Unmarshal(payloads[1], closeEnvelope); err != nil {
		t.Fatal(err)
	}
	if closeEnvelope.GetExecClientControlMessage().GetStreamClose() == nil {
		t.Fatalf("unknown Exec close=%#v", closeEnvelope.GetExecClientControlMessage())
	}
}

func TestNativePiFindMapsToGlobWithoutMCPAlias(t *testing.T) {
	path := "src"
	limit := int32(50)
	def := &ToolDefinition{Name: "Glob", Parameters: map[string]any{
		"properties": map[string]any{"pattern": map[string]any{}, "path": map[string]any{}},
	}}
	args, err := nativeToolArguments("pi_find_args", &agentv1.PiFindExecArgs{
		Pattern: "**/*.go", Path: &path, Limit: &limit,
	}, def)
	if err != nil {
		t.Fatal(err)
	}
	if args["pattern"] != "**/*.go" || args["path"] != "src" {
		t.Fatalf("arguments = %#v", args)
	}
}

func TestNativePiEditPreservesEveryEdit(t *testing.T) {
	def := &ToolDefinition{Name: "Edit", Parameters: map[string]any{
		"properties": map[string]any{
			"file_path": map[string]any{}, "old_string": map[string]any{}, "new_string": map[string]any{},
		},
	}}
	bags, err := nativeToolArgumentBags("pi_edit_args", &agentv1.PiEditExecArgs{
		Path: "main.go",
		Edits: []*agentv1.PiEditReplacement{
			{OldText: "one", NewText: "ONE"},
			{OldText: "two", NewText: "TWO"},
		},
	}, def)
	if err != nil {
		t.Fatal(err)
	}
	if len(bags) != 2 || bags[0]["old_string"] != "one" || bags[1]["new_string"] != "TWO" {
		t.Fatalf("edit bags = %#v", bags)
	}
}

func TestNativePiEditUsesBatchSchemaWhenDeclared(t *testing.T) {
	def := &ToolDefinition{Name: "MultiEdit", Parameters: map[string]any{
		"properties": map[string]any{"file_path": map[string]any{}, "edits": map[string]any{}},
	}}
	bags, err := nativeToolArgumentBags("pi_edit_args", &agentv1.PiEditExecArgs{
		Path:  "main.go",
		Edits: []*agentv1.PiEditReplacement{{OldText: "one", NewText: "ONE"}, {OldText: "two", NewText: "TWO"}},
	}, def)
	if err != nil {
		t.Fatal(err)
	}
	if len(bags) != 1 {
		t.Fatalf("edit bags = %#v", bags)
	}
	edits, ok := bags[0]["edits"].([]any)
	if !ok || len(edits) != 2 {
		t.Fatalf("batch edits = %#v", bags[0]["edits"])
	}
}

func TestCustomToolNamedReadStaysMCP(t *testing.T) {
	tools := []ToolDefinition{{Name: "Read", Kind: "custom"}, {Name: "Read", Namespace: "codex_app"}}
	if got := customMcpTools(tools); len(got) != 1 {
		t.Fatalf("custom Read tools were classified native: %#v", got)
	}
}

func TestNativeToolSuppressesSameNameMCP(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "Read", Kind: "custom"},
		{Name: "Read", Namespace: "codex_app"},
		{Name: "Read", Kind: "function"},
	}
	cleaned := SanitizeToolDefinitions(tools)
	if len(cleaned) != 1 || !hasNativeCursorToolMapping(cleaned[0]) {
		t.Fatalf("cleaned tools = %#v", cleaned)
	}
	if got := customMcpTools(tools); len(got) != 0 {
		t.Fatalf("conflicting MCP tools = %#v", got)
	}
}

func TestCursorToolIDPassesThroughUnchanged(t *testing.T) {
	rawID := "toolu:raw\nid"
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	s.handleToolLifecycle("envelope", "model-call", &agentv1.ToolCall{
		ToolCallId: proto.String(rawID),
		Tool:       &agentv1.ToolCall_ReadToolCall{ReadToolCall: &agentv1.ReadToolCall{Args: &agentv1.ReadToolArgs{Path: "a.txt"}}},
	}, false)
	req := &agentv1.ExecServerMessage{Id: 4, Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "a.txt"}}}
	if _, err := s.handleNativeArgs(req, "read_args", req.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	pending := s.pending[rawID]
	if pending == nil || pending.call.ID != rawID {
		t.Fatalf("raw id was changed: %#v", pending)
	}
}

func TestGrepAndLsResultsCarryClientOutput(t *testing.T) {
	grep := buildGrepSuccess(&agentv1.GrepArgs{Pattern: "needle"}, "a.go:12:needle here\nb.go:3:needle too")
	content := grep.GetWorkspaceResults()["default"].GetContent()
	if content == nil || len(content.GetMatches()) != 2 || content.GetTotalMatchedLines() != 2 {
		t.Fatalf("grep content = %#v", content)
	}
	tree := buildLsTree("src", "src/main.go\nsrc/pkg/tool.go\nsrc/README")
	if tree.GetNumFiles() != 3 || len(tree.GetChildrenDirs()) != 1 || len(tree.GetChildrenFiles()) != 2 {
		t.Fatalf("ls tree = %#v", tree)
	}
}

func TestDiagnosticsResultPreservesStructuredFields(t *testing.T) {
	content := `{"path":"main.go","diagnostics":[{"severity":"warning","range":{"start":{"line":3,"character":4},"end":{"line":3,"character":9}},"message":"unused value","source":"gopls","code":"unused","is_stale":true}]}`
	success := buildDiagnosticsSuccess("fallback.go", content)
	if success.GetPath() != "main.go" || success.GetTotalDiagnostics() != 1 || len(success.GetDiagnostics()) != 1 {
		t.Fatalf("diagnostics success = %#v", success)
	}
	diagnostic := success.GetDiagnostics()[0]
	if diagnostic.GetSeverity() != agentv1.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_WARNING ||
		diagnostic.GetRange().GetStart().GetLine() != 3 || diagnostic.GetRange().GetStart().GetColumn() != 4 ||
		diagnostic.GetSource() != "gopls" || diagnostic.GetCode() != "unused" || !diagnostic.GetIsStale() {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	raw, err := proto.Marshal(&agentv1.DiagnosticsResult{Result: &agentv1.DiagnosticsResult_Success{Success: success}})
	if err != nil {
		t.Fatal(err)
	}
	decoded := &agentv1.DiagnosticsResult{}
	if err = proto.Unmarshal(raw, decoded); err != nil || decoded.GetSuccess().GetDiagnostics()[0].GetMessage() != "unused value" {
		t.Fatalf("round trip diagnostics=%#v err=%v", decoded, err)
	}
}

func TestDiagnosticsTextAndFetchJSONRemainSuccessful(t *testing.T) {
	diagnostics := buildDiagnosticsSuccess("main.go", "main.go:3:4: warning: unused value")
	if diagnostics.GetTotalDiagnostics() != 1 || diagnostics.GetDiagnostics()[0].GetMessage() == "" {
		t.Fatalf("text diagnostics = %#v", diagnostics)
	}
	fetch := buildFetchSuccess("https://example.test", `{"content":"ok","status_code":206,"content_type":"text/html"}`)
	if fetch.GetContent() != "ok" || fetch.GetStatusCode() != 206 || fetch.GetContentType() != "text/html" {
		t.Fatalf("fetch = %#v", fetch)
	}
	plain := buildFetchSuccess("https://example.test/plain", "plain body")
	if plain.GetContent() != "plain body" || plain.GetStatusCode() != 200 {
		t.Fatalf("plain fetch = %#v", plain)
	}
}
