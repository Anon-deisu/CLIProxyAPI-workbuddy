package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/tidwall/gjson"
)

// stableForConversationKey drops the context agents re-render on every turn
// (todo state, command output, interrupt notices). Leaving it in made the
// fingerprint change mid-chat, which hands Cursor a new conversation and costs
// a full prompt cache write on a turn whose content was otherwise identical.
func stableForConversationKey(text string) string {
	if injected, ask := splitInjectedContext(text); injected != "" && ask != "" {
		return ask
	}
	return text
}

// ConversationFingerprint identifies a chat that carries no session id of its
// own, from the parts that do not change as turns are appended: the model, the
// system prompt and the opening ask. Bodies are tried in order so a caller can
// pass the original request first and the translated one as a backup.
//
// Without this such clients would get a fresh conversation on every turn, and
// Cursor bills a fresh conversation as a full cache write.
func ConversationFingerprint(model string, bodies ...[]byte) string {
	for _, body := range bodies {
		system, firstUser := conversationAnchors(body)
		if strings.TrimSpace(system) == "" && strings.TrimSpace(firstUser) == "" {
			continue
		}
		h := sha256.New()
		h.Write([]byte(BaseModelID(model)))
		h.Write([]byte{0})
		h.Write([]byte(stableForConversationKey(system)))
		h.Write([]byte{0})
		h.Write([]byte(stableForConversationKey(firstUser)))
		return hex.EncodeToString(h.Sum(nil))
	}
	return ""
}

// conversationAnchors pulls the system prompt and the first user turn out of an
// Anthropic or OpenAI style request body.
func conversationAnchors(body []byte) (system, firstUser string) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", ""
	}
	system = messageText(gjson.GetBytes(body, "system"))
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		text := messageText(msg.Get("content"))
		if strings.TrimSpace(text) == "" {
			return true
		}
		switch role {
		case "system":
			if system == "" {
				system = text
			}
		case "user":
			firstUser = text
			return false
		}
		return true
	})
	return system, firstUser
}

// messageText flattens a content field that may be a plain string or a list of
// typed parts.
func messageText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var parts []string
	content.ForEach(func(_, part gjson.Result) bool {
		if part.Type == gjson.String {
			parts = append(parts, part.String())
			return true
		}
		if text := part.Get("text").String(); text != "" {
			parts = append(parts, text)
		}
		return true
	})
	return strings.Join(parts, "\n")
}
