package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// errAPIIndexOutOfRange is returned when a caller addresses a credential by a
// list index that no longer exists.
var errAPIIndexOutOfRange = errors.New("cursor API key index out of range")

// IsIndexOutOfRange reports whether err came from addressing a cursor API key
// by a stale list index, so HTTP handlers can map it onto a 404.
func IsIndexOutOfRange(err error) bool {
	return errors.Is(err, errAPIIndexOutOfRange)
}

// authKindAPIKeyStorage is the value written into every credential's
// "auth_kind" discriminator. It mirrors coreauth.AuthKindAPIKey so the file
// synthesizer classifies the credential as an API key (not the OAuth branch).
const authKindAPIKeyStorage = "apikey"

// APIKeyStore is a small, pure file-backed store for Cursor API key
// credentials. Each credential is one JSON file inside the auth directory
// whose name is derived by apiKeyStorageFileName; the store loads them all
// into a stable (file-name sorted) list and addresses them by list index so
// the management API can mutate credentials without ever handling raw tokens.
//
// The store deliberately has no dependency on gin or the auth manager: it only
// reads and writes JSON files. Writing into the auth directory is enough to
// re-dispatch the credential through the watcher's fsnotify -> core auth
// manager pipeline, so no extra registration call is required here.
type APIKeyStore struct {
	dir string
}

// NewAPIKeyStore returns a store rooted at an auth directory.
func NewAPIKeyStore(authDir string) *APIKeyStore {
	return &APIKeyStore{dir: strings.TrimSpace(authDir)}
}

// Dir returns the auth directory the store was rooted at.
func (s *APIKeyStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// cursorAPIKeyFileSuffix is the static file-name suffix every credential file
// carries, e.g. "me@example.com_cursor-api-key.json".
const cursorAPIKeyFileSuffix = "_" + StoredAPIKeyType + ".json"

// isCursorAPIKeyFileName reports whether name looks like a Cursor API key
// credential file (case-insensitive suffix, not a hidden temp file).
func isCursorAPIKeyFileName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(name), strings.ToLower(cursorAPIKeyFileSuffix))
}

// List returns every Cursor API key credential currently on disk, sorted by
// file name so indexes are stable across restarts. Malformed files are skipped
// (never fatal).
func (s *APIKeyStore) List() ([]*APIKeyStorage, error) {
	if s == nil || s.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read auth dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isCursorAPIKeyFileName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make([]*APIKeyStorage, 0, len(names))
	for _, name := range names {
		rec, errRead := readAPIKeyStorageFile(filepath.Join(s.dir, name))
		if errRead != nil || rec == nil || !IsCursorAPIKeyRecord(rec) {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// Get returns the credential at list index, or a *cursor API key index error.
func (s *APIKeyStore) Get(index int) (*APIKeyStorage, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(list) {
		return nil, fmt.Errorf("%w: index %d (have %d)", errAPIIndexOutOfRange, index, len(list))
	}
	return list[index], nil
}

// filePathFor returns the on-disk path for a named credential file.
func (s *APIKeyStore) filePathFor(name string) string {
	return filepath.Join(s.dir, filepath.Base(name))
}

// removeAtFile deletes the credential file at list index.
func (s *APIKeyStore) removeAtFile(list []*APIKeyStorage, index int) error {
	if index < 0 || index >= len(list) {
		return fmt.Errorf("%w: index %d (have %d)", errAPIIndexOutOfRange, index, len(list))
	}
	return os.Remove(list[index].FileName())
}

// Create writes a new credential file and returns its file name. The API key
// must be non-blank.
func (s *APIKeyStore) Create(rec *APIKeyStorage) (string, error) {
	if s == nil || s.dir == "" {
		return "", errors.New("auth dir is not configured")
	}
	if rec == nil || strings.TrimSpace(rec.APIKey) == "" {
		return "", errors.New("api key is required")
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	if strings.TrimSpace(rec.Type) == "" {
		rec.Type = StoredAPIKeyType
	}
	if strings.TrimSpace(rec.AuthKind) == "" {
		rec.AuthKind = authKindAPIKeyStorage
	}
	name := apiKeyStorageFileName(rec.APIKey, rec.BaseURL, rec.Prefix, rec.Email)
	if err := s.writeRecord(name, rec); err != nil {
		return "", err
	}
	return name, nil
}

// Update overwrites the credential at list index, preserving the original file
// name unless the identity changed (in which case the file is renamed so the
// auth ID stays meaningful).
func (s *APIKeyStore) Update(index int, rec *APIKeyStorage) error {
	list, err := s.List()
	if err != nil {
		return err
	}
	if index < 0 || index >= len(list) {
		return fmt.Errorf("%w: index %d (have %d)", errAPIIndexOutOfRange, index, len(list))
	}
	prev := list[index]
	if strings.TrimSpace(rec.APIKey) == "" {
		rec.APIKey = prev.APIKey
	}
	rec.UpdatedAt = time.Now()
	if !prev.CreatedAt.IsZero() {
		rec.CreatedAt = prev.CreatedAt
	}
	if strings.TrimSpace(rec.Type) == "" {
		rec.Type = StoredAPIKeyType
	}
	if strings.TrimSpace(rec.AuthKind) == "" {
		rec.AuthKind = prev.AuthKind
	}
	oldName := filepath.Base(prev.FileName())
	newName := apiKeyStorageFileName(rec.APIKey, rec.BaseURL, rec.Prefix, rec.Email)
	if newName != oldName {
		if err := os.Remove(prev.FileName()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale credential file: %w", err)
		}
	}
	return s.writeRecord(newName, rec)
}

// Delete removes the credential at list index.
func (s *APIKeyStore) Delete(index int) error {
	list, err := s.List()
	if err != nil {
		return err
	}
	if index < 0 || index >= len(list) {
		return fmt.Errorf("%w: index %d (have %d)", errAPIIndexOutOfRange, index, len(list))
	}
	return os.Remove(list[index].FileName())
}

// writeRecord marshals a credential and writes it atomically into the auth
// directory. The write itself is what re-pumps the credential into the core
// auth manager via the watcher, so no manager call is made here.
func (s *APIKeyStore) writeRecord(name string, rec *APIKeyStorage) error {
	target := s.filePathFor(name)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cursor api key: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("ensure auth dir: %w", err)
	}
	// Write to a temp file then rename so fsnotify never observes a partial
	// JSON payload.
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write cursor api key: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit cursor api key: %w", err)
	}
	return nil
}
