package cursor

import "testing"

func TestInjectedContextDoesNotSplitTheConversation(t *testing.T) {
	sys := "You are Claude Code."
	// Agents re-render these blocks every turn; the ask underneath is what
	// actually identifies the chat.
	turn1 := []ChatMessage{{Role: "user", Content: "<system-reminder>todo: []</system-reminder>\nFix the login bug."}}
	turn2 := []ChatMessage{{Role: "user", Content: "<system-reminder>todo: [1 done, 2 pending]</system-reminder>\nFix the login bug."}}

	if conversationKeyFor("claude-opus-5", sys, turn1) != conversationKeyFor("claude-opus-5", sys, turn2) {
		t.Fatal("a re-rendered reminder must not mint a new conversation")
	}
}

func TestDifferentAsksStillSeparate(t *testing.T) {
	sys := "You are Claude Code."
	a := []ChatMessage{{Role: "user", Content: "<system-reminder>x</system-reminder>\nFix the login bug."}}
	b := []ChatMessage{{Role: "user", Content: "<system-reminder>x</system-reminder>\nWrite the release notes."}}
	if conversationKeyFor("claude-opus-5", sys, a) == conversationKeyFor("claude-opus-5", sys, b) {
		t.Fatal("different asks must not share a conversation")
	}
}

func TestContextOnlyMessageKeepsItsOwnIdentity(t *testing.T) {
	// Nothing survives the strip, so the raw text has to keep anchoring the
	// key rather than collapsing every such chat onto one conversation.
	sys := "sys"
	a := []ChatMessage{{Role: "user", Content: "<system-reminder>only context A</system-reminder>"}}
	b := []ChatMessage{{Role: "user", Content: "<system-reminder>only context B</system-reminder>"}}
	if conversationKeyFor("claude-opus-5", sys, a) == conversationKeyFor("claude-opus-5", sys, b) {
		t.Fatal("context-only messages collapsed onto one conversation")
	}
}
