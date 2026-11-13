package helps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var workBuddyIDSeed = uuid.NewString()

// ApplyWorkBuddyConversationHeaders keeps tool steps in the same upstream turn.
func ApplyWorkBuddyConversationHeaders(req *http.Request, body []byte, opts cliproxyexecutor.Options) {
	applyWorkBuddyConversationHeaders(req, body, opts, workBuddyIDSeed, false)
}

// ApplyWorkBuddyFingerprintHeaders pins the upstream identity to the regional account,
// independently of tokens, auth filenames, caller clients, or process lifetime.
func ApplyWorkBuddyFingerprintHeaders(req *http.Request, auth *coreauth.Auth, body []byte, opts cliproxyexecutor.Options) {
	uid, _ := auth.Metadata["uid"].(string)
	enterprise, _ := auth.Metadata["enterprise_id"].(string)
	identity, _ := json.Marshal([]string{auth.Provider, uid, enterprise})
	seed := "cli-proxy-api:workbuddy:fingerprint:v1:" + string(identity)
	applyWorkBuddyConversationHeaders(req, body, opts, seed, true)
}

func applyWorkBuddyConversationHeaders(req *http.Request, body []byte, opts cliproxyexecutor.Options, seed string, converge bool) {
	idFor := func(kind, value string) string {
		digest := sha256.Sum256([]byte(seed + "|" + kind + "|" + value))
		return fmt.Sprintf("%x", digest[:16])
	}
	messageID := strings.ReplaceAll(uuid.NewString(), "-", "")
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	conversation := opts.Headers.Get("X-Conversation-ID")
	if conversation == "" {
		if session := coreauth.CanonicalSessionID(opts.Headers, body, opts.Metadata); session != "" {
			conversation = idFor("conversation", session)
		}
	}
	messages := ojson.GetBytes(body, "messages").Array()
	if conversation == "" {
		for _, message := range messages {
			if message.Get("role").String() == "user" {
				conversation = idFor("conversation", message.Get("content").Raw)
				break
			}
		}
	}
	if conversation != "" {
		req.Header.Set("X-Conversation-ID", conversation)
	}
	turn := opts.Headers.Get("X-Conversation-Request-ID")
	if turn == "" {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Get("role").String() != "user" {
				continue
			}
			if i > 0 && (messages[i-1].Get("role").String() == "tool" || len(messages[i-1].Get("tool_calls").Array()) > 0) {
				continue
			}
			turn = idFor("turn", fmt.Sprintf("%s|%d|%s", conversation, i, messages[i].Get("content").Raw))
			break
		}
	}
	if turn == "" {
		turn = messageID
	}
	if converge {
		// Keep caller conversations separate at the turn layer even though the
		// upstream conversation identity is shared by this account's requests.
		scope, _ := json.Marshal([]string{conversation, turn})
		turn = idFor("scoped-turn", string(scope))
		req.Header.Set("X-Conversation-ID", idFor("identity", ""))
	}
	req.Header.Set("X-Conversation-Request-ID", turn)
}
