package cursor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
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

func TestSplitSystemMessagesAndTrimHistory(t *testing.T) {
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
	var sawClientSystem bool
	for _, data := range blobs {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		if payload["role"] == "system" {
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
				sawClientSystem = true
			}
			if content == "You are a helpful assistant." {
				t.Fatalf("dummy system should not replace client system: %q", content)
			}
		}
	}
	if !sawClientSystem {
		t.Fatal("expected client system prompt in blobs")
	}
	um := msg.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage()
	if um == nil || um.GetText() != "hello" {
		t.Fatalf("user text should stay unmodified, got %#v", um.GetText())
	}
	if strings.Contains(um.GetText(), "System instructions") {
		t.Fatal("must not inject System instructions into user text")
	}

	history := make([]ChatMessage, 0, 60)
	for i := 0; i < 60; i++ {
		history = append(history, ChatMessage{Role: "user", Content: fmt.Sprintf("m%d", i)})
		history = append(history, ChatMessage{Role: "assistant", Content: fmt.Sprintf("a%d", i)})
	}
	trimmed := trimConversationHistory(history)
	if len(trimmed) > maxHistoryMessages {
		t.Fatalf("trimmed len=%d > %d", len(trimmed), maxHistoryMessages)
	}
	if trimmed[len(trimmed)-1].Content != "a59" {
		t.Fatalf("expected newest kept, last=%q", trimmed[len(trimmed)-1].Content)
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
		t.Fatal("manager aliases should be unbound")
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
