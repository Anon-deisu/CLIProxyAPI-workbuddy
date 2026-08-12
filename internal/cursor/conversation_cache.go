package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	conversationIDTTL = 2 * time.Hour
	maxConversationID = 2048
)

type conversationEntry struct {
	id       string
	lastSeen time.Time
}

var conversationIDs = struct {
	sync.Mutex
	byKey map[string]conversationEntry
}{byKey: map[string]conversationEntry{}}

// conversationKeyFor derives a stable identity for a logical conversation from
// the parts that do not change as turns are appended.
func conversationKeyFor(model, systemPrompt string, conversation []ChatMessage) string {
	h := sha256.New()
	h.Write([]byte(BaseModelID(model)))
	h.Write([]byte{0})
	h.Write([]byte(systemPrompt))
	h.Write([]byte{0})
	// The first user turn anchors the conversation; later turns only append.
	for _, msg := range conversation {
		if msg.Role != "user" {
			continue
		}
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		h.Write([]byte(text))
		break
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reuseConversationID returns the conversation id previously used for this
// logical conversation, minting (and remembering) one on first use.
//
// Cursor treats every new conversation id as a fresh prompt, so generating one
// per request made the provider re-write the prompt cache each turn instead of
// reading it back.
func reuseConversationID(key string) string {
	now := time.Now()
	conversationIDs.Lock()
	defer conversationIDs.Unlock()
	if entry, ok := conversationIDs.byKey[key]; ok && now.Sub(entry.lastSeen) < conversationIDTTL {
		entry.lastSeen = now
		conversationIDs.byKey[key] = entry
		return entry.id
	}
	if len(conversationIDs.byKey) >= maxConversationID {
		for k, entry := range conversationIDs.byKey {
			if now.Sub(entry.lastSeen) >= conversationIDTTL {
				delete(conversationIDs.byKey, k)
			}
		}
		if len(conversationIDs.byKey) >= maxConversationID {
			oldestKey := ""
			var oldest time.Time
			for k, entry := range conversationIDs.byKey {
				if oldestKey == "" || entry.lastSeen.Before(oldest) {
					oldestKey, oldest = k, entry.lastSeen
				}
			}
			delete(conversationIDs.byKey, oldestKey)
		}
	}
	id := uuid.NewString()
	conversationIDs.byKey[key] = conversationEntry{id: id, lastSeen: now}
	return id
}

// rotateConversationID abandons the id currently mapped to key and remembers a
// fresh one, so later turns of the same chat stay cacheable under the new id.
//
// A conversation whose last run died mid tool call keeps server-side state that
// makes Cursor reject the next run on that id, so a retry must not reuse it.
func rotateConversationID(key string) string {
	id := uuid.NewString()
	conversationIDs.Lock()
	defer conversationIDs.Unlock()
	conversationIDs.byKey[key] = conversationEntry{id: id, lastSeen: time.Now()}
	return id
}
