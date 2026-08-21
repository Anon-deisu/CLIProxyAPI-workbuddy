package cursor

import (
	"strings"
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func pelicanHistory(reads int) []ChatMessage {
	conversation := []ChatMessage{
		{Role: "user", Content: "把动画写成完整 html 文件"},
	}
	for i := 0; i < reads; i++ {
		id := "toolu_read_" + string(rune('a'+i))
		conversation = append(conversation,
			ChatMessage{Role: "assistant", Content: "现在真正写入文件。", ToolCalls: []ToolCall{
				{ID: id, Name: "Read", Arguments: map[string]any{"path": `pelican-ride\鹈鹕骑行 v2.html`}}}},
			ChatMessage{Role: "tool", ToolCallID: id, Name: "Read", Content: "File not found"},
		)
	}
	return conversation
}

func TestFirstReadIsShownToClient(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	req := &agentv1.ExecServerMessage{Id: 1, ExecId: "exec-1", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "a.txt"}}}
	if _, err := s.handleNativeArgs(req, "read_args", req.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) == 0 {
		t.Fatal("first Read must still be shown to the client")
	}
}

func TestSecondReadOfSamePathIsNotShown(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	first := &agentv1.ExecServerMessage{Id: 1, ExecId: "exec-1", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: `pelican-ride\鹈鹕骑行 v2.html`}}}
	if _, err := s.handleNativeArgs(first, "read_args", first.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) == 0 {
		t.Fatal("first Read must bind")
	}
	second := &agentv1.ExecServerMessage{Id: 2, ExecId: "exec-2", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "pelican-ride/鹈鹕骑行 v2.html"}}}
	if _, err := s.handleNativeArgs(second, "read_args", second.GetReadArgs()); err == nil {
		t.Fatal("duplicate Read must be answered locally")
	}
	if s.pending["exec-2"] != nil {
		t.Fatal("duplicate Read must not become a client tool_call")
	}
	for _, item := range s.pending {
		if item != nil && item.request != nil && item.request.GetId() == 2 {
			t.Fatal("duplicate Read was bound for the client")
		}
	}
}

func TestHistorySeededReadIsSuppressedImmediately(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	s.history = pelicanHistory(2)
	s.turnReadCounts, s.lastReadByPath = seedTurnReadState(s.history)
	req := &agentv1.ExecServerMessage{Id: 3, ExecId: "exec-3", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: `pelican-ride\鹈鹕骑行 v2.html`}}}
	if _, err := s.handleNativeArgs(req, "read_args", req.GetReadArgs()); err == nil {
		t.Fatal("history already has this Read; it must not be shown again")
	}
	if len(s.pending) != 0 {
		t.Fatalf("suppressed Read bound pending: %#v", s.pending)
	}
	reply, ok := s.duplicateReadReply("Read", map[string]any{"path": "pelican-ride/鹈鹕骑行 v2.html"})
	if !ok || !strings.Contains(reply, "Do not Read") || !strings.Contains(reply, "File not found") {
		t.Fatalf("reply=%q ok=%v", reply, ok)
	}
}

func TestWriteClearsReadLoopSoVerifyReadIsAllowed(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{
		{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}},
		{Name: "Write", Kind: "function", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{}, "contents": map[string]any{}}}},
	})
	read := &agentv1.ExecServerMessage{Id: 1, ExecId: "exec-1", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "out.html"}}}
	if _, err := s.handleNativeArgs(read, "read_args", read.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	write := &agentv1.ExecServerMessage{Id: 2, ExecId: "exec-2", Message: &agentv1.ExecServerMessage_WriteArgs{WriteArgs: &agentv1.WriteArgs{Path: "out.html", FileText: "<html/>"}}}
	if _, err := s.handleNativeArgs(write, "write_args", write.GetWriteArgs()); err != nil {
		t.Fatal(err)
	}
	verify := &agentv1.ExecServerMessage{Id: 3, ExecId: "exec-3", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "out.html"}}}
	if _, err := s.handleNativeArgs(verify, "read_args", verify.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range s.pending {
		if item != nil && item.request != nil && item.request.GetId() == 3 {
			found = true
		}
	}
	if !found {
		t.Fatal("Read after Write must be allowed so the client can verify the file")
	}
}

func TestDifferentPathReadIsNotSuppressed(t *testing.T) {
	s := newProtocolSession([]ToolDefinition{{Name: "Read", Kind: "function", Parameters: map[string]any{"type": "object"}}})
	one := &agentv1.ExecServerMessage{Id: 1, ExecId: "exec-1", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "a.txt"}}}
	two := &agentv1.ExecServerMessage{Id: 2, ExecId: "exec-2", Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "b.txt"}}}
	if _, err := s.handleNativeArgs(one, "read_args", one.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.handleNativeArgs(two, "read_args", two.GetReadArgs()); err != nil {
		t.Fatal(err)
	}
	if len(s.pendingOrder) < 2 {
		t.Fatalf("two different files must both be shown, pending=%d", len(s.pendingOrder))
	}
}

func TestTokenStallSkipsToolWaitAndFreshFrames(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	oldEmit := now.Add(-cursorTokenStallTimeout - time.Minute)
	freshFrame := now.Add(-time.Minute)
	if tokenStallShouldFail(true, oldEmit, time.Time{}, now) {
		t.Fatal("awaiting tools must not be killed as a token stall")
	}
	if tokenStallShouldFail(false, oldEmit, freshFrame, now) {
		t.Fatal("a large Write still receiving upstream bytes must not be killed")
	}
	if !tokenStallShouldFail(false, oldEmit, now.Add(-cursorTokenStallTimeout-time.Minute), now) {
		t.Fatal("no tokens and no frames for 8m must fail")
	}
	if tokenStallShouldFail(false, now.Add(-time.Minute), time.Time{}, now) {
		t.Fatal("recent client tokens must not fail")
	}
}

func TestNormalizeReadPathTreatsSlashVariantsAsOneFile(t *testing.T) {
	if toolCallPath(map[string]any{"path": `pelican-ride\鹈鹕骑行 v2.html`}) !=
		toolCallPath(map[string]any{"file_path": "pelican-ride/鹈鹕骑行 v2.html"}) {
		t.Fatal("backslash and slash paths must count as the same file")
	}
}
