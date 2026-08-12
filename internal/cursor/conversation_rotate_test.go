package cursor

import "testing"

func TestRotateConversationIDReplacesAndSticks(t *testing.T) {
	key := conversationKeyFor("gpt-5.6-sol", "sys", []ChatMessage{{Role: "user", Content: "rotate me"}})
	original := reuseConversationID(key)

	rotated := rotateConversationID(key)
	if rotated == original {
		t.Fatal("rotation must abandon the tainted conversation id")
	}
	// Later turns must settle on the rotated id, otherwise every turn after a
	// rebuild would mint a new conversation and lose the prompt cache.
	if again := reuseConversationID(key); again != rotated {
		t.Fatalf("reuse after rotate = %s, want %s", again, rotated)
	}
}

func TestFreshConversationOptionRotatesRunID(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: "fresh option"}}
	_, _, first, err := buildRunRequest("default", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, same, err := buildRunRequest("default", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if same != first {
		t.Fatal("plain rebuilds must reuse the conversation id")
	}
	_, _, fresh, err := buildRunRequest("default", msgs, nil, startConfig{freshConversation: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh == first {
		t.Fatal("WithFreshConversation must produce a different conversation id")
	}
}
