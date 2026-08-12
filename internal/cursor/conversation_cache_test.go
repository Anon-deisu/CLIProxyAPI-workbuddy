package cursor

import "testing"

func TestReuseConversationIDIsStableAcrossTurns(t *testing.T) {
	sys := "system rules"
	turn1 := []ChatMessage{{Role: "user", Content: "first question"}}
	turn2 := []ChatMessage{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "follow up"},
	}
	id1 := reuseConversationID(conversationKeyFor("claude-fable-5", sys, turn1))
	id2 := reuseConversationID(conversationKeyFor("claude-fable-5", sys, turn2))
	if id1 != id2 {
		t.Fatalf("appending turns must keep the conversation id: %s vs %s", id1, id2)
	}
}

func TestReuseConversationIDSeparatesDistinctChats(t *testing.T) {
	sys := "system rules"
	a := reuseConversationID(conversationKeyFor("claude-fable-5", sys, []ChatMessage{{Role: "user", Content: "chat A"}}))
	b := reuseConversationID(conversationKeyFor("claude-fable-5", sys, []ChatMessage{{Role: "user", Content: "chat B"}}))
	if a == b {
		t.Fatal("different first user messages must not share a conversation id")
	}
	c := reuseConversationID(conversationKeyFor("gpt-5.6-sol", sys, []ChatMessage{{Role: "user", Content: "chat A"}}))
	if c == a {
		t.Fatal("different models must not share a conversation id")
	}
}

func TestConversationKeyIgnoresVariantSuffix(t *testing.T) {
	sys := "system rules"
	msgs := []ChatMessage{{Role: "user", Content: "hello"}}
	if conversationKeyFor("claude-fable-5-thinking-high", sys, msgs) != conversationKeyFor("claude-fable-5", sys, msgs) {
		t.Fatal("variant suffixes should not split the conversation")
	}
}
