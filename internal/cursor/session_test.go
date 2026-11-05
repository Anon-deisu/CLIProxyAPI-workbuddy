package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestExtractToolResultsTrailingOnly(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "old", Name: "x", Arguments: map[string]any{}}}},
		{Role: "tool", ToolCallID: "old", Content: "stale"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "NY"}}}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"ok":true}`},
	}
	got := extractToolResults(messages)
	if len(got) != 1 || got[0].ToolCallID != "c1" {
		t.Fatalf("expected trailing c1, got %#v", got)
	}
}

func TestSplitSystemMessagesAndPreserveHistory(t *testing.T) {
	sys, conv := splitSystemMessages([]ChatMessage{
		{Role: "system", Content: "A"},
		{Role: "user", Content: "u1"},
		{Role: "system", Content: "B"},
		{Role: "assistant", Content: "a1"},
	})
	if sys != "A\n\nB" {
		t.Fatalf("system=%q", sys)
	}
	if len(conv) != 2 || conv[0].Role != "user" || conv[1].Role != "assistant" {
		t.Fatalf("conv=%#v", conv)
	}

	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "system", Content: "Client system rules"},
		{Role: "user", Content: "hello"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.GetRunRequest() == nil {
		t.Fatal("missing run")
	}
	// Cursor discards a root-prompt blob with role "system", so the client's
	// rules only survive the trip on a user blob.
	var carrier string
	for _, data := range blobs {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		content := ""
		switch c := payload["content"].(type) {
		case string:
			content = c
		case []any:
			for _, part := range c {
				m, _ := part.(map[string]any)
				if m == nil {
					continue
				}
				if t, _ := m["text"].(string); t != "" {
					if content != "" {
						content += "\n"
					}
					content += t
				}
			}
		}
		if strings.Contains(content, "Client system rules") {
			carrier, _ = payload["role"].(string)
		}
		if payload["role"] == "system" && content == "You are a helpful assistant." {
			t.Fatalf("dummy system should not replace client system: %q", content)
		}
	}
	if carrier == "" {
		t.Fatal("expected client system prompt in blobs")
	}
	if carrier != "user" {
		t.Fatalf("client system prompt rode a %q blob; Cursor only keeps user turns", carrier)
	}
	um := msg.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage()
	if um == nil || um.GetText() != "hello" {
		t.Fatalf("user text should stay unmodified, got %#v", um.GetText())
	}
	if strings.Contains(um.GetText(), "System instructions") {
		t.Fatal("must not inject System instructions into user text")
	}

	history := make([]ChatMessage, 0, 121)
	for i := 0; i < 60; i++ {
		history = append(history, ChatMessage{Role: "user", Content: fmt.Sprintf("m%d", i)})
		history = append(history, ChatMessage{Role: "assistant", Content: fmt.Sprintf("a%d", i)})
	}
	history = append(history, ChatMessage{Role: "user", Content: "active"})
	_, historyBlobs, _, err := buildRunRequest("default", history, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawFirst, sawLast bool
	for _, data := range historyBlobs {
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		content, _ := payload["content"].([]any)
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			text, _ := part["text"].(string)
			sawFirst = sawFirst || text == "m0"
			sawLast = sawLast || text == "a59"
		}
	}
	if !sawFirst || !sawLast {
		t.Fatalf("full history was not preserved: first=%v last=%v", sawFirst, sawLast)
	}
}

func TestBuildRunRequestIncludesMcpTools(t *testing.T) {
	msg, _, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "weather?"},
	}, []ToolDefinition{{
		Name:        "get_weather",
		Description: "weather",
		Parameters:  map[string]any{"type": "object"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	if run == nil || run.GetMcpTools() == nil || len(run.GetMcpTools().GetMcpTools()) != 1 {
		t.Fatalf("expected mcp tools on run request, got %#v", run.GetMcpTools())
	}
	tool := run.GetMcpTools().GetMcpTools()[0]
	if tool.GetProviderIdentifier() != MCPProviderIdentifier || tool.GetName() != "get_weather" {
		t.Fatalf("unexpected tool: %#v", tool)
	}
}

func TestBuildRunRequestNativeToolJSON(t *testing.T) {
	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "thinking", ToolCalls: []ToolCall{
			{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "NY"}},
		}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"ok":true}`},
		{Role: "user", Content: "again"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.GetRunRequest() == nil {
		t.Fatal("missing run request")
	}
	var sawToolCall, sawToolResult bool
	for _, data := range blobs {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		role, _ := payload["role"].(string)
		content, _ := payload["content"].([]any)
		switch role {
		case "assistant":
			for _, part := range content {
				m, _ := part.(map[string]any)
				if m["type"] == "tool-call" && m["toolCallId"] == "c1" {
					sawToolCall = true
				}
			}
			if _, ok := payload["tool_calls"]; ok {
				t.Fatalf("expected Cursor-native tool-call parts, not OpenAI tool_calls: %#v", payload)
			}
		case "tool":
			if payload["id"] != "c1" {
				continue
			}
			for _, part := range content {
				m, _ := part.(map[string]any)
				if m["type"] == "tool-result" && m["toolCallId"] == "c1" {
					sawToolResult = true
				}
			}
			if _, ok := payload["tool_call_id"]; ok {
				t.Fatalf("expected Cursor-native tool id/content, not tool_call_id: %#v", payload)
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("native tool history missing call=%v result=%v", sawToolCall, sawToolResult)
	}
}

func TestBuildRunRequestGivesToolContinuationAnExecutableAction(t *testing.T) {
	// This used to assert ResumeAction. Live traffic disproved that design: a
	// fresh conversation id carries no server-side state, so a bare
	// ResumeAction gets an immediate empty END_STREAM — the "N in / 0 out"
	// dashboard rows and the endless 502 retry loops.
	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "Beijing"}}}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"temperature":30}`},
	}, []ToolDefinition{{Name: "get_weather", Parameters: map[string]any{"type": "object"}}})
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	action := run.GetAction().GetUserMessageAction()
	if action == nil {
		t.Fatalf("tool continuation needs an executable action, got %#v", run.GetAction())
	}
	if action.GetUserMessage().GetText() != toolContinuationNudge {
		t.Fatalf("tool continuation must use the constant nudge, got %q", action.GetUserMessage().GetText())
	}
	var sawCall, sawResult bool
	for _, data := range blobs {
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		content, _ := payload["content"].([]any)
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			if part["type"] == "tool-call" && part["toolCallId"] == "c1" {
				sawCall = true
			}
			if part["type"] == "tool-result" && part["toolCallId"] == "c1" {
				sawResult = true
			}
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("structured continuation missing call=%v result=%v", sawCall, sawResult)
	}
}

func TestCookieJarRemembersSetCookie(t *testing.T) {
	jar := &CookieJar{byHost: map[string]map[string]string{}}
	hdr := make(http.Header)
	hdr.Add("Set-Cookie", "CursorCookie=server-issued; Path=/; HttpOnly")
	jar.RememberResponse("https://api2.cursor.sh", hdr)
	got := jar.Header("https://api2.cursor.sh")
	if !strings.Contains(got, "CursorCookie=server-issued") {
		t.Fatalf("expected server cookie, got %q", got)
	}
}

func TestProtobufValueRoundTrip(t *testing.T) {
	in := map[string]any{"city": "北京", "n": float64(1), "ok": true}
	v, err := toProtobufValue(in)
	if err != nil {
		t.Fatal(err)
	}
	out := fromProtobufValue(v)
	raw, _ := json.Marshal(out)
	if string(raw) == "" {
		t.Fatal("empty roundtrip")
	}
}

func TestAwaitingClientToolsKeepsSessionAlive(t *testing.T) {
	s := &Session{pending: map[string]*pendingExec{}}
	if s.awaitingClientTools() {
		t.Fatal("empty session should not await tools")
	}
	pend := &pendingExec{call: ToolCall{ID: "call_1", Name: "Bash"}}
	s.pending["call_1"] = pend
	s.pending["toolu_1"] = pend // alias for same exec
	if !s.awaitingClientTools() {
		t.Fatal("pending tool exec should block end-stream close")
	}
	if got := s.pendingToolCount(); got != 1 {
		t.Fatalf("pendingToolCount=%d want 1", got)
	}
	s.beginWaitingTools()
	if !s.awaitingClientTools() {
		t.Fatal("waitingTools should keep session awaiting")
	}
}

func TestParkForRebuildTombsAndDropsContinuation(t *testing.T) {
	id := "conv-park-helper"
	dropContinuation(id)
	snapshot := snapshotWithAssistant(t, "held")
	rememberContinuation(id, snapshot)
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	item := &pendingExec{call: ToolCall{ID: "toolu_park", Name: "Bash"}}
	s := &Session{
		ID:             "park-s",
		ConversationID: id,
		pending:        map[string]*pendingExec{"toolu_park": item},
		waitingTools:   true,
		manager:        m,
	}
	m.Register(s)
	m.BindPending("toolu_park", s)
	s.parkForRebuild("stream error while awaiting client tool results: INTERNAL_ERROR")
	if !s.parkedForRebuild() {
		t.Fatal("expected parked for rebuild")
	}
	if _, ok := takeContinuation(id); ok {
		t.Fatal("park must drop the tool-pause checkpoint")
	}
	_ = s.Close()
	_, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: "toolu_park"}})
	if !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("got %v want rebuild tombstone", err)
	}
	s.parkForRebuild("stream error while awaiting client tool results: http2: response body closed")
	if !s.responseEnded {
		t.Fatal("repeat park must keep responseEnded")
	}
}

func TestSubmitToolResultsRebuildsWhenResponseEnded(t *testing.T) {
	s := &Session{
		pending: map[string]*pendingExec{
			"toolu_1": {request: nil, call: ToolCall{ID: "toolu_1", Name: "Bash"}},
		},
		responseEnded: true,
	}
	err := s.SubmitToolResults([]ToolResult{{ToolCallID: "toolu_1", Content: "ok"}})
	if err != ErrToolResumeRebuild {
		t.Fatalf("got %v want ErrToolResumeRebuild", err)
	}
	if !s.closed {
		t.Fatal("expected session closed after rebuild signal")
	}
}

func TestReadLoopKeepsReadingTrailingUsageWhileWaitingForTools(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	go func() {
		_, _ = io.Copy(io.Discard, requestReader)
	}()
	responseReader, responseWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		stream:       &BidiStream{writer: requestWriter, body: responseReader},
		events:       make(chan StreamEvent, 4),
		errCh:        make(chan error, 1),
		waitingTools: true,
		pauseCh:      make(chan struct{}),
		done:         ctx.Done(),
		cancel:       cancel,
		lastActivity: time.Now(),
	}
	go s.readLoop(ctx)

	inputTokens := int64(120)
	cacheWriteTokens := int64(80)
	serverMessage := &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TurnEnded{
					TurnEnded: &agentv1.TurnEndedUpdate{
						InputTokens:      &inputTokens,
						CacheWriteTokens: &cacheWriteTokens,
					},
				},
			},
		},
	}
	payload, err := proto.Marshal(serverMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = responseWriter.Write(EncodeEnvelope(payload, false)); err != nil {
		t.Fatal(err)
	}

	usage, ok := s.TakeUsage(time.Second)
	if !ok {
		t.Fatal("trailing TurnEnded was not read while waiting for tools")
	}
	if usage.InputTokens != inputTokens || usage.CacheWriteTokens != cacheWriteTokens {
		t.Fatalf("usage=%+v", usage)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	_ = responseWriter.Close()
	_ = requestReader.Close()
}

func TestReleasePendingExecClearsAllAliases(t *testing.T) {
	m := NewSessionManager()
	pend := &pendingExec{call: ToolCall{ID: "toolu_abc", Name: "Bash"}}
	s := &Session{
		ID:      "s1",
		pending: map[string]*pendingExec{},
		manager: m,
	}
	s.pending["call_raw"] = pend
	s.pending["toolu_abc"] = pend
	m.Register(s)
	m.BindPending("call_raw", s)
	m.BindPending("toolu_abc", s)

	s.releasePendingExec(pend, "call_raw", "toolu_abc")
	if len(s.pending) != 0 {
		t.Fatalf("pending leftover %#v", s.pending)
	}
	if m.LookupPending("call_raw") != nil || m.LookupPending("toolu_abc") != nil {
		t.Fatal("manager lookup keys should be unbound")
	}
}

func TestSubmitToolResultsKeepsPendingAfterWriteFailure(t *testing.T) {
	reader, writer := io.Pipe()
	_ = reader.Close()
	item := &pendingExec{
		request: &agentv1.ExecServerMessage{
			Id:      7,
			Message: &agentv1.ExecServerMessage_McpArgs{McpArgs: &agentv1.McpArgs{ToolCallId: "tool_raw"}},
		},
		call:    ToolCall{ID: "tool_raw", Name: "weather"},
		variant: "mcp_args",
		emitted: true,
	}
	s := &Session{
		stream:       &BidiStream{writer: writer},
		pending:      map[string]*pendingExec{"tool_raw": item},
		pendingOrder: []*pendingExec{item},
		batches:      map[string]*pendingToolBatch{},
	}
	err := s.SubmitToolResults([]ToolResult{{ToolCallID: "tool_raw", Content: "sunny"}})
	if !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("dead request stream must rebuild, got %v", err)
	}
}

func TestPiEditGroupRollsBackFinalResultAfterWriteFailure(t *testing.T) {
	reader, writer := io.Pipe()
	_ = reader.Close()
	request := &agentv1.ExecServerMessage{
		Id:      8,
		Message: &agentv1.ExecServerMessage_PiEditArgs{PiEditArgs: &agentv1.PiEditExecArgs{Path: "a.go"}},
	}
	group := &nativeExecGroup{request: request, variant: "pi_edit_args", results: map[int]ToolResult{}}
	first := &pendingExec{request: request, call: ToolCall{ID: "edit_1"}, variant: "pi_edit_args", group: group, index: 0}
	second := &pendingExec{request: request, call: ToolCall{ID: "edit_2"}, variant: "pi_edit_args", group: group, index: 1}
	group.items = []*pendingExec{first, second}
	s := &Session{stream: &BidiStream{writer: writer}}
	completed, err := s.submitNativeGroupResult(first, ToolResult{ToolCallID: "edit_1", Content: "one"})
	if err != nil || completed {
		t.Fatalf("first result completed=%v err=%v", completed, err)
	}
	completed, err = s.submitNativeGroupResult(second, ToolResult{ToolCallID: "edit_2", Content: "two"})
	if err == nil || completed {
		t.Fatalf("final result completed=%v err=%v", completed, err)
	}
	if group.sent || len(group.results) != 1 || group.results[0].Content != "one" {
		t.Fatalf("group state was not rolled back: sent=%v results=%#v", group.sent, group.results)
	}
}

func TestIncompleteCheckpointEmitsWriteOnForceAndEager(t *testing.T) {
	write := &pendingExec{call: ToolCall{ID: "toolu_write", Name: "Write"}}
	s := &Session{
		pending:        map[string]*pendingExec{"toolu_write": write},
		pendingOrder:   []*pendingExec{write},
		parallelTools:  true,
		events:         make(chan StreamEvent, 4),
		lifecyclesByID: map[string]*nativeLifecycle{},
	}
	s.conversationState = &agentv1.ConversationStateStructure{PendingToolCalls: []string{
		`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"toolu_read","toolName":"Read"},{"type":"tool-call","toolCallId":"toolu_write","toolName":"Write"}]}`,
	}}
	s.applyCheckpointToolBatches(s.conversationState)
	if !s.emitPendingToolBatch(false) {
		t.Fatal("Write sitting in an incomplete checkpoint must emit eagerly")
	}
	if got := (<-s.events).ToolCall.Name; got != "Write" {
		t.Fatalf("eager tool=%q want Write", got)
	}
}

func TestIncompleteCheckpointEmitsWhatItHasOnForce(t *testing.T) {
	read := &pendingExec{call: ToolCall{ID: "toolu_read", Name: "Read"}}
	s := &Session{
		pending:        map[string]*pendingExec{"toolu_read": read},
		pendingOrder:   []*pendingExec{read},
		parallelTools:  true,
		events:         make(chan StreamEvent, 4),
		lifecyclesByID: map[string]*nativeLifecycle{},
	}
	s.conversationState = &agentv1.ConversationStateStructure{PendingToolCalls: []string{
		`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"toolu_read","toolName":"Read"},{"type":"tool-call","toolCallId":"toolu_write","toolName":"Write"}]}`,
	}}
	s.applyCheckpointToolBatches(s.conversationState)
	if s.emitPendingToolBatch(false) {
		t.Fatal("Read-only incomplete checkpoint must still wait on the live path")
	}
	if !s.emitPendingToolBatch(true) {
		t.Fatal("force must emit the Read we already have")
	}
	if got := (<-s.events).ToolCall.Name; got != "Read" {
		t.Fatalf("forced tool=%q want Read", got)
	}
}

func TestCheckpointToolBatchUsesFormalOrderAndAliases(t *testing.T) {
	first := &pendingExec{call: ToolCall{ID: "toolu_second", Name: "Read"}}
	second := &pendingExec{call: ToolCall{ID: "toolu_first", Name: "Bash"}}
	s := &Session{
		pendingOrder:  []*pendingExec{first, second},
		parallelTools: true,
		events:        make(chan StreamEvent, 4),
	}
	s.conversationState = &agentv1.ConversationStateStructure{PendingToolCalls: []string{
		`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"toolu_first","toolName":"Bash"},{"type":"tool-call","toolCallId":"toolu_second","toolName":"Read"}]}`,
	}}
	s.applyCheckpointToolBatches(s.conversationState)
	if !s.pendingToolBatchReady() || !s.emitPendingToolBatch(false) {
		t.Fatal("formal checkpoint batch was not emitted")
	}
	if got := (<-s.events).ToolCall.Name; got != "Bash" {
		t.Fatalf("first tool=%q want Bash", got)
	}
	if got := (<-s.events).ToolCall.Name; got != "Read" {
		t.Fatalf("second tool=%q want Read", got)
	}
}

func TestSerialToolBatchEmitsOneCallPerContinuation(t *testing.T) {
	first := &pendingExec{call: ToolCall{ID: "call_1", Name: "one"}}
	second := &pendingExec{call: ToolCall{ID: "call_2", Name: "two"}}
	batch := &pendingToolBatch{id: "model_1", modelCallID: "model_1", ordered: []*pendingExec{first, second}, terminal: true}
	first.batch, second.batch = batch, batch
	s := &Session{
		pending:       map[string]*pendingExec{"call_1": first, "call_2": second},
		pendingOrder:  []*pendingExec{first, second},
		batches:       map[string]*pendingToolBatch{"model_1": batch},
		batchOrder:    []*pendingToolBatch{batch},
		parallelTools: false,
		events:        make(chan StreamEvent, 4),
		done:          context.Background().Done(),
	}
	if !s.emitPendingToolBatch(true) {
		t.Fatal("serial batch did not emit first call")
	}
	batch.emitted = true
	if got := (<-s.events).ToolCall.ID; got != "call_1" {
		t.Fatalf("first call=%q", got)
	}
	if second.emitted {
		t.Fatal("second serial call must remain staged")
	}
	if !s.emitNextPendingTool() {
		t.Fatal("serial continuation did not emit next call")
	}
	if got := (<-s.events).ToolCall.ID; got != "call_2" {
		t.Fatalf("second call=%q", got)
	}
}

func TestParallelToolBatchRejectsPartialNativeResumeForHistoryRebuild(t *testing.T) {
	first := &pendingExec{call: ToolCall{ID: "call_1", Name: "one"}, emitted: true}
	second := &pendingExec{call: ToolCall{ID: "call_2", Name: "two"}, emitted: true}
	s := &Session{
		pending:       map[string]*pendingExec{"call_1": first, "call_2": second},
		pendingOrder:  []*pendingExec{first, second},
		parallelTools: true,
		events:        make(chan StreamEvent, 4),
		errCh:         make(chan error, 1),
		pauseCh:       make(chan struct{}),
		lastActivity:  time.Now(),
	}
	err := s.SubmitToolResults([]ToolResult{{ToolCallID: "call_1", Content: "ok"}})
	if err != ErrToolResumeRebuild {
		t.Fatalf("partial parallel batch error=%v", err)
	}
	// Closing here used to kill the whole session including the events
	// channel of any sibling request still streaming from it, which surfaced
	// downstream as "stream closed before a terminal boundary". The session
	// stays alive (parked for rebuild) and the sweep tears it down.
	if s.closed {
		t.Fatal("partial batch must not close the session under a live sibling consumer")
	}
	if !s.parkedForRebuild() {
		t.Fatal("partial batch must park the session for rebuild")
	}
}

// An externally closed session under an active collector routes to the
// rebuild path while tools are pending, never to a client-facing error.
func TestCollectorTreatsExternalCloseAsRebuildWhileToolsPending(t *testing.T) {
	s := &Session{
		waitingTools: true,
		pending:      map[string]*pendingExec{"call_1": {}},
		events:       make(chan StreamEvent, 1),
		errCh:        make(chan error, 1),
	}
	close(s.events)

	if _, err := s.CollectSegment(context.Background()); !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("collect: want rebuild signal, got %v", err)
	}

	s2 := &Session{events: make(chan StreamEvent, 1), errCh: make(chan error, 1)}
	close(s2.events)
	if _, err := s2.CollectSegment(context.Background()); !errors.Is(err, ErrEmptyTerminal) {
		t.Fatalf("collect without pending tools: want empty terminal, got %v", err)
	}
}

func TestPreferredToolNamesKeepsBash(t *testing.T) {
	tools := make([]ToolDefinition, 0, 30)
	for i := 0; i < 20; i++ {
		tools = append(tools, ToolDefinition{Name: fmt.Sprintf("mcp__x__t%d", i), Description: "x", Parameters: map[string]any{"type": "object"}})
	}
	tools = append(tools, ToolDefinition{Name: "Bash", Description: "shell", Parameters: map[string]any{"type": "object"}})
	tools = append(tools, ToolDefinition{Name: "Read", Description: "read", Parameters: map[string]any{"type": "object"}})
	prefer := PreferredToolNames(nil, tools)
	out := CapToolDefinitions(tools, 12, prefer)
	names := map[string]bool{}
	for _, tdef := range out {
		names[tdef.Name] = true
	}
	if !names["Bash"] || !names["Read"] {
		t.Fatalf("expected Bash/Read in capped set, got %v", out)
	}
}

func TestStallActionKeepsAliveBeforeFirstFrame(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	heartbeat, fail := stallAction(time.Time{}, now)
	if !heartbeat || fail {
		t.Fatalf("zero lastFrame should ping, not fail (heartbeat=%v fail=%v)", heartbeat, fail)
	}

	started := now.Add(-10 * time.Second)
	heartbeat, fail = stallAction(started, now)
	if heartbeat || fail {
		t.Fatalf("10s idle should wait (heartbeat=%v fail=%v)", heartbeat, fail)
	}

	started = now.Add(-cursorIdleHeartbeatInterval)
	heartbeat, fail = stallAction(started, now)
	if !heartbeat || fail {
		t.Fatalf("idle interval should ping (heartbeat=%v fail=%v)", heartbeat, fail)
	}

	started = now.Add(-cursorStallTimeout)
	heartbeat, fail = stallAction(started, now)
	if heartbeat || !fail {
		t.Fatalf("stall timeout should fail (heartbeat=%v fail=%v)", heartbeat, fail)
	}
}

func TestTokenStallDoesNotKillToolWait(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-cursorTokenStallTimeout - time.Minute)
	if tokenStallShouldFail(true, old, old, now) {
		t.Fatal("token stall must skip while awaiting client tools")
	}
}

func TestPublishedToolIDIsBoundAndTombstoned(t *testing.T) {
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	item := &pendingExec{call: ToolCall{ID: "call:raw id", Name: "Bash"}}
	s := &Session{
		ID:      "pub-s",
		pending: map[string]*pendingExec{"call:raw id": item},
		manager: m,
	}
	m.Register(s)
	s.rememberPublishedToolID("call:raw id")
	if m.LookupPending("call:raw id") != s {
		t.Fatal("published id must be bound")
	}
	ids := s.pendingIDs()
	found := false
	for _, id := range ids {
		if id == "call:raw id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pendingIDs missing published id: %v", ids)
	}
	if !s.PendingCallIDAlive("call:raw id") {
		t.Fatal("published id must count as alive")
	}
	s.parkForRebuild("test")
	_ = s.Close()
	_, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: "call:raw id"}})
	if !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("published id must rebuild after park, got %v", err)
	}
}

func TestSubmitUnknownPendingRebuilds(t *testing.T) {
	s := &Session{
		pending: map[string]*pendingExec{"toolu_known": {call: ToolCall{ID: "toolu_known"}}},
	}
	err := s.SubmitToolResults([]ToolResult{{ToolCallID: "toolu_missing", Content: "ok"}})
	if !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("unknown pending id must rebuild, got %v", err)
	}
}

type countingReadCloser struct {
	reads int
}

func (c *countingReadCloser) Read([]byte) (int, error) {
	c.reads++
	return 0, io.EOF
}

func (c *countingReadCloser) Close() error { return nil }

func TestReadLoopDoesNotRereadAfterResponseEnded(t *testing.T) {
	body := &countingReadCloser{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Session{
		stream:        &BidiStream{body: body},
		events:        make(chan StreamEvent, 4),
		errCh:         make(chan error, 1),
		waitingTools:  true,
		responseEnded: true,
		pauseCh:       make(chan struct{}),
		done:          ctx.Done(),
		cancel:        cancel,
		lastActivity:  time.Now(),
	}
	done := make(chan struct{})
	go func() {
		s.readLoop(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if body.reads != 0 {
		t.Fatalf("parked session re-read a dead body %d times", body.reads)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit after cancel")
	}
}
