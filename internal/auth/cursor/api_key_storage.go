package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// StoredAPIKeyType is the "type" discriminator written into every Cursor API
// key JSON file. The watcher's file synthesizer maps it straight onto
// auth.Provider == "cursor-api-key", which surfaces a distinct Cursor API Key
// category in the management UI and routes these credentials through the
// dedicated cursor executor branch.
const StoredAPIKeyType = "cursor-api-key"

// APIKeyStorage is the on-disk JSON schema that persists a Cursor API key
// credential. A single credential maps to exactly one file named
// "<email>_cursor-api-key.json" (falling back to "<prefix>_cursor-api-key.json"
// or a UUID pseudo-email when no stable identity can be derived).
//
// It is deliberately a well-defined, self-describing format rather than a bare
// token dump: every field the executor and dispatcher need is stored here so
// the file synthesizer can promote them first-class on the synthesized Auth
// without re-deriving anything from the file name.
type APIKeyStorage struct {
	// Type discriminator; always StoredAPIKeyType.
	Type string `json:"type"`
	// APIKey is the raw Cursor user / service-account API key. It is never
	// logged and never stored anywhere except this file.
	APIKey string `json:"api_key"`
	// Priority is the load-balancing priority for this credential when the
	// dispatcher selects among multiple cursor-api-key auths.
	Priority int `json:"priority,omitempty"`
	// Prefix optionally namespaces the models served by this credential.
	Prefix string `json:"prefix,omitempty"`
	// BaseURL overrides the Cursor exchange endpoint (empty -> default).
	BaseURL string `json:"base_url,omitempty"`
	// ExcludedModels lists model ids this credential must never serve.
	ExcludedModels []string `json:"excluded_models,omitempty"`
	// DisableCooling opts out of provider cooldown for this credential.
	DisableCooling bool `json:"disable_cooling,omitempty"`
	// Disabled marks the credential excluded from routing without deleting it.
	Disabled bool `json:"disabled,omitempty"`
	// Note is an operator-facing free-text annotation.
	Note string `json:"note,omitempty"`
	// Email is the accountable account / label for this credential, surfaced in
	// the management UI and used as the preferred file-name identity.
	Email string `json:"email,omitempty"`
	// AuthKind pins the credential kind so the synthesizer never has to guess.
	AuthKind string `json:"auth_kind"`
	// CreatedAt / UpdatedAt are runtime bookkeeping timestamps.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// file is the exact path this record was read from / written to. It is
	// runtime-only bookkeeping and never serialised into the JSON body.
	file string `json:"-"`
}

// FileName returns the JSON file path backing this record, or "" if it was
// never attached to a file.
func (k *APIKeyStorage) FileName() string {
	if k == nil {
		return ""
	}
	return k.file
}

// setFile binds the backing file path (internal use only).
func (k *APIKeyStorage) setFile(path string) {
	k.file = path
}

// ResolvedBaseURL returns the effective Cursor exchange endpoint after
// canonicalising the default away and normalising slashes.
func (k *APIKeyStorage) ResolvedBaseURL() string {
	if k == nil {
		return DefaultBaseURL
	}
	baseURL := strings.TrimRight(strings.TrimSpace(k.BaseURL), "/")
	if baseURL == "" {
		return DefaultBaseURL
	}
	return baseURL
}

// readAPIKeyStorageFile loads a single Cursor API key JSON file from disk. It
// returns nil (not an error) when the file cannot be parsed: a partial or
// malformed credential is ignored rather than bricking the whole file sweep.
func readAPIKeyStorageFile(path string) (*APIKeyStorage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec APIKeyStorage
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	rec.setFile(path)
	return &rec, nil
}

// IsCursorAPIKeyRecord reports whether a decoded JSON payload is a Cursor API
// key credential rather than an OAuth file, based on the type discriminator.
func IsCursorAPIKeyRecord(rec *APIKeyStorage) bool {
	return rec != nil && strings.EqualFold(strings.TrimSpace(rec.Type), StoredAPIKeyType)
}

// apiKeyStorageFileName derives a stable, human-legible file name for a Cursor
// API key credential. The preferred identity is the key's email; when no email
// is present the prefix (or a compact digest of the key) is used so repeated
// writes always land on the same file rather than spawning duplicates.
func apiKeyStorageFileName(apiKey, baseURL, prefix, email string) string {
	base := strings.TrimSpace(email)
	if base == "" {
		base = strings.TrimSpace(prefix)
	}
	if base == "" {
		base = apiKeyDigest(apiKey, baseURL)
		if base == "" {
			base = "cursor-api-key"
		}
	}
	base = sanitizeFileNameComponent(base)
	if base == "" {
		base = "cursor-api-key"
	}
	return base + "_" + StoredAPIKeyType + ".json"
}

// apiKeyDigest returns a compact hex digest of the API key identity so a file
// name collision is effectively impossible even for two anonymous keys.
func apiKeyDigest(apiKey, baseURL string) string {
	if strings.TrimSpace(apiKey) == "" {
		return ""
	}
	return shortHex(sha256sum(baseURL+"\x00"+apiKey))
}

// sanitizeFileNameComponent strips characters that are unsafe on common
// filesystems, preserving letters, digits, dots, dashes and underscores.
func sanitizeFileNameComponent(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(trimmed))
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// sha256sum returns the lower-case hex digest of s.
func sha256sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// shortHex trims a hex digest to its first 12 characters.
func shortHex(h string) string {
	h = strings.TrimSpace(h)
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
