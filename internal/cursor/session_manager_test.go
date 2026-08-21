package cursor

import (
	"errors"
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
	if !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("unknown tool ids must rebuild, got %v", err)
	}
}

func TestResolveTombstoneReturnsRebuild(t *testing.T) {
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	s := newPausedSession("s1", "conv1")
	s.responseEnded = true
	s.manager = m
	m.Register(s)
	m.BindPending("toolu_1", s)

	_ = s.Close()

	_, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: "toolu_1"}})
	if !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("got %v want ErrToolResumeRebuild", err)
	}
	if m.LookupPending("toolu_1") != nil {
		t.Fatal("live pending binding should be gone after close")
	}
}
