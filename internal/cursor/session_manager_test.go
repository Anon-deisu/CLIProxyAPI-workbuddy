package cursor

import (
	"strings"
	"testing"
)

func TestBindPendingUsesRawID(t *testing.T) {
	m := NewSessionManager()
	session := &Session{ID: "s1"}
	raw := "call:with:colons\nand-newline"
	m.BindPending(raw, session)
	got := m.LookupPending(raw)
	if got == nil || got.ID != "s1" {
		t.Fatalf("raw lookup failed, got %#v", got)
	}
	owner, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: raw}})
	if err != nil || owner == nil || owner.ID != "s1" {
		t.Fatalf("resolve raw id: owner=%#v err=%v", owner, err)
	}
}

func TestResolveUnknownToolCallID(t *testing.T) {
	m := NewSessionManager()
	_, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: "toolu_missing"}})
	if err == nil || !strings.Contains(err.Error(), "unknown or expired tool_call_id") {
		t.Fatalf("err=%v", err)
	}
}
