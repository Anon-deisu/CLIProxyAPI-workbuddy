package helps

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// CodexFingerprintDoc adapts the account identity protocol to the request Doc
// pipeline. Each request owns its snapshot, including on a reused websocket.
type CodexFingerprintDoc struct {
	*CodexFingerprint
}

func NewCodexFingerprintDoc(authID string) *CodexFingerprintDoc {
	return &CodexFingerprintDoc{CodexFingerprint: NewCodexFingerprint(authID)}
}

// ApplyBodyDoc preserves explicit cache keys and unrelated metadata verbatim.
func (f *CodexFingerprint) ApplyBodyDoc(doc *ojson.Doc) {
	metadata := doc.Get("client_metadata")
	f.originalMetadata = strings.Clone(metadata.Raw)
	turnMetadata := f.TurnMetadata(metadata.Get("x-codex-turn-metadata").String())
	if !metadata.IsObject() {
		_ = doc.SetRaw("client_metadata", "{}")
	}
	_ = doc.Set("client_metadata.x-codex-installation-id", f.InstallationID)
	_ = doc.Set("client_metadata.session_id", f.SessionID)
	_ = doc.Set("client_metadata.thread_id", f.SessionID)
	_ = doc.Set("client_metadata.turn_id", f.TurnID)
	_ = doc.Set("client_metadata.x-codex-window-id", f.SessionID+":0")
	_ = doc.Set("client_metadata.x-codex-turn-metadata", turnMetadata)
}

// RestoreResponseDoc only restores echoed metadata, never generated text.
func (f *CodexFingerprint) RestoreResponseDoc(doc *ojson.Doc) {
	for _, prefix := range []string{"", "response."} {
		if f.originalPromptCacheKey != "" && doc.Get(prefix+"prompt_cache_key").String() == f.SessionID {
			_ = doc.Set(prefix+"prompt_cache_key", f.originalPromptCacheKey)
		}
		if !doc.Get(prefix + "client_metadata").Exists() {
			continue
		}
		if f.originalMetadata == "" {
			_ = doc.Delete(prefix + "client_metadata")
		} else {
			_ = doc.SetRaw(prefix+"client_metadata", f.originalMetadata)
		}
	}
}
