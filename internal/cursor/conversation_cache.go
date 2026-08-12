package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
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

// stableForConversationKey drops the context agents re-render on every turn
// (todo state, command output, interrupt notices). Leaving it in made the key
// change mid-chat, which mints a new conversation id and costs a full prompt
// cache write on a turn whose content was otherwise identical.
func stableForConversationKey(text string) string {
	if injected, ask := splitInjectedContext(text); injected != "" && ask != "" {
		return ask
	}
	return text
}

// conversationKeyFor derives a stable identity for a logical conversation from
// the parts that do not change as turns are appended.
func conversationKeyFor(model, systemPrompt string, conversation []ChatMessage) string {
	h := sha256.New()
	h.Write([]byte(BaseModelID(model)))
	h.Write([]byte{0})
	h.Write([]byte(stableForConversationKey(systemPrompt)))
	h.Write([]byte{0})
	// The first user turn anchors the conversation; later turns only append.
	for _, msg := range conversation {
		if msg.Role != "user" {
			continue
		}
		text := strings.TrimSpace(stableForConversationKey(msg.Content))
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
	// Each mint is a fresh prompt prefix upstream, so a chat that mints on
	// every turn is paying a full cache write each time.
	log.Debugf("cursor: new conversation id (%d tracked)", len(conversationIDs.byKey))
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
