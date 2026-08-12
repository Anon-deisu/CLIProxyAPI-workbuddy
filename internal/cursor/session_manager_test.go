package cursor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

func TestBindPendingIndexesClaudeSafeAlias(t *testing.T) {
	m := NewSessionManager()
	session := &Session{ID: "s1"}
	raw := "call:with:colons"
	m.BindPending(raw, session)
	safe := util.SanitizeClaudeToolID(raw)
	if safe == raw {
		t.Fatalf("expected sanitizer to change %q", raw)
	}
	got := m.LookupPending(safe)
	if got == nil || got.ID != "s1" {
		t.Fatalf("alias lookup failed, got %#v", got)
	}
	owner, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: safe}})
	if err != nil || owner == nil || owner.ID != "s1" {
		t.Fatalf("resolve alias: owner=%#v err=%v", owner, err)
	}
}

func TestResolveUnknownToolCallID(t *testing.T) {
	m := NewSessionManager()
	_, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: "toolu_missing"}})
	if err == nil || !strings.Contains(err.Error(), "unknown or expired tool_call_id") {
		t.Fatalf("err=%v", err)
	}
}
