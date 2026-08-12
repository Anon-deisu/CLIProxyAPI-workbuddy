package cursor

import (
	"encoding/json"
	"testing"
)

func body(t *testing.T, system string, firstUser string, extraTurns ...map[string]any) []byte {
	t.Helper()
	messages := []map[string]any{{"role": "user", "content": firstUser}}
	messages = append(messages, extraTurns...)
	raw, err := json.Marshal(map[string]any{"system": system, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInjectedContextDoesNotSplitTheConversation(t *testing.T) {
	sys := "You are Claude Code."
	// Agents re-render these blocks every turn; the ask underneath is what
	// actually identifies the chat.
	turn1 := body(t, sys, "<system-reminder>todo: []</system-reminder>\nFix the login bug.")
	turn2 := body(t, sys, "<system-reminder>todo: [1 done, 2 pending]</system-reminder>\nFix the login bug.",
		map[string]any{"role": "assistant", "content": "Looking."},
		map[string]any{"role": "user", "content": "keep going"})

	if ConversationFingerprint("claude-opus-5", turn1) != ConversationFingerprint("claude-opus-5", turn2) {
		t.Fatal("a re-rendered reminder must not start a new conversation")
	}
}

func TestDifferentAsksStillSeparate(t *testing.T) {
	sys := "You are Claude Code."
	a := body(t, sys, "<system-reminder>x</system-reminder>\nFix the login bug.")
	b := body(t, sys, "<system-reminder>x</system-reminder>\nWrite the release notes.")
	if ConversationFingerprint("claude-opus-5", a) == ConversationFingerprint("claude-opus-5", b) {
		t.Fatal("different asks must not share a conversation")
	}
}

func TestContextOnlyMessageKeepsItsOwnIdentity(t *testing.T) {
	// Nothing survives the strip, so the raw text has to keep anchoring the
	// fingerprint rather than collapsing every such chat onto one conversation.
	a := body(t, "sys", "<system-reminder>only context A</system-reminder>")
	b := body(t, "sys", "<system-reminder>only context B</system-reminder>")
	if ConversationFingerprint("claude-opus-5", a) == ConversationFingerprint("claude-opus-5", b) {
		t.Fatal("context-only messages collapsed onto one conversation")
	}
}

func TestFingerprintSurvivesModelVariantsButNotModelChanges(t *testing.T) {
	raw := body(t, "sys", "hello")
	if ConversationFingerprint("claude-opus-5-thinking-high", raw) != ConversationFingerprint("claude-opus-5", raw) {
		t.Error("a thinking suffix is the same chat")
	}
	if ConversationFingerprint("gpt-5.6-sol", raw) == ConversationFingerprint("claude-opus-5", raw) {
		t.Error("different models must not share a conversation")
	}
}

func TestFingerprintFallsThroughToTheNextBody(t *testing.T) {
	raw := body(t, "sys", "hello")
	if ConversationFingerprint("claude-opus-5", nil, raw) != ConversationFingerprint("claude-opus-5", raw) {
		t.Fatal("an empty first body should defer to the next one")
	}
	if ConversationFingerprint("claude-opus-5", []byte("not json"), nil) != "" {
		t.Fatal("unusable bodies must produce no fingerprint")
	}
}
