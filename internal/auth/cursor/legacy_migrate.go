package cursor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// legacyConfigCursorAPIKey is the on-disk shape the *old* standalone-page
// manager used to persist Cursor API keys: a top-level "cursor-api-key" block
// in config.yaml holding a list of bare keys. The current design deleted the
// config.CursorKey field and stores credentials as JSON files in the auth dir,
// so this struct exists only to read those legacy values once, on first boot,
// and migrate them into the JSON store before the field is dropped from the
// file for good.
type legacyConfigCursorAPIKey struct {
	Items []struct {
		APIKey  string `yaml:"api-key"`
		BaseURL string `yaml:"base-url"`
		Prefix  string `yaml:"prefix"`
		Email   string `yaml:"email"`
	} `yaml:"cursor-api-key"`
}

const legacyCursorAPIKeyKey = "cursor-api-key"

// MigrateLegacyCursorAPIKeys performs the one-time, first-boot migration of
// Cursor API keys left behind by the old standalone-page manager (a
// "cursor-api-key" block in config.yaml) into the file-backed JSON store. It
// is intentionally safe to call on every start:
//
//   - when no legacy block exists it is a no-op;
//   - a key already present in the JSON store is never rewritten;
//   - the legacy block is stripped from config.yaml (comment-preserving) only
//     after every legacy key has been persisted, so a crash before the save
//     step leaves the source data intact for the next attempt.
//
// It returns the number of keys migrated from the legacy block.
func MigrateLegacyCursorAPIKeys(store *APIKeyStore, configFile string) (int, error) {
	if store == nil || store.Dir() == "" {
		return 0, nil
	}
	if strings.TrimSpace(configFile) == "" {
		return 0, nil
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read config for cursor API key migration: %w", err)
	}
	var legacy legacyConfigCursorAPIKey
	hasLegacyBlock := false
	{
		// Parse just enough of the document to know whether a top-level
		// "cursor-api-key" block is actually present (even an empty one), which
		// the struct-based unmarshal cannot distinguish from an absent key.
		var doc yaml.Node
		if errY := yaml.Unmarshal(data, &doc); errY == nil &&
			doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 && doc.Content[0] != nil &&
			doc.Content[0].Kind == yaml.MappingNode {
			for i := 0; i+1 < len(doc.Content[0].Content); i += 2 {
				if doc.Content[0].Content[i] != nil && doc.Content[0].Content[i].Value == legacyCursorAPIKeyKey {
					hasLegacyBlock = true
					break
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		// A malformed block must never block startup: log and leave the file
		// untouched rather than dropping keys silently.
		log.Warnf("skip legacy cursor-api-key migration: malformed config: %v", err)
		return 0, nil
	}

	existing, err := store.List()
	if err != nil {
		// A read failure here would be fatal in normal operation too; surface it.
		return 0, fmt.Errorf("list cursor API key store during migration: %w", err)
	}
	have := make(map[string][]string, len(existing))
	for _, rec := range existing {
		if rec == nil || strings.TrimSpace(rec.APIKey) == "" {
			continue
		}
		have[rec.ResolvedBaseURL()] = append(have[rec.ResolvedBaseURL()], rec.APIKey)
	}

	migrated := 0
	created := make([]string, 0, len(legacy.Items))
	for i := range legacy.Items {
		item := &legacy.Items[i]
		key := strings.TrimSpace(item.APIKey)
		if key == "" {
			continue
		}
		baseURL := strings.TrimRight(strings.TrimSpace(item.BaseURL), "/")
		if containsStoredKey(have[baseURL], key) {
			continue // already migrated
		}
		rec := &APIKeyStorage{
			Type:    StoredAPIKeyType,
			APIKey:  key,
			BaseURL: item.BaseURL,
			Prefix:  item.Prefix,
			Email:   item.Email,
		}
		if _, errCreate := store.Create(rec); errCreate != nil {
			// A single bad key should not abort the rest; record it and keep
			// going. The block stays on disk so a later boot retries.
			log.Errorf("migrate legacy cursor API key #%d: %v", migrated, errCreate)
			continue
		}
		migrated++
		created = append(created, rec.FileName())
	}

	if !hasLegacyBlock {
		// Nothing to do; do not rewrite config.yaml just to discover no block.
		return 0, nil
	}

	// Strip the legacy block now that (all migrated) keys live in the store.
	if errRemove := stripLegacyCursorAPIKeyBlock(configFile, data); errRemove != nil {
		log.Warnf("legacy cursor-api-key block migrated but could not be removed: %v", errRemove)
		return migrated, nil
	}
	for i, name := range created {
		log.Infof("migrated legacy cursor API key #%d -> %s", i+1, name)
	}
	return migrated, nil
}

// stripLegacyCursorAPIKeyBlock removes the top-level "cursor-api-key" mapping
// from configFile's YAML node tree and rewrites the file, preserving comments
// and the ordering of every other key. It is deliberately conservative: any
// error leaves the original file byte-for-byte untouched.
func stripLegacyCursorAPIKeyBlock(configFile string, data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0] == nil {
		return errors.New("invalid yaml document structure")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i] != nil && root.Content[i].Value == legacyCursorAPIKeyKey {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return atomicWriteFile(configFile, buf.Bytes())
}

// atomicWriteFile writes payload to path via a temp file + rename so a crash
// mid-write can never leave a truncated config file.
func atomicWriteFile(path string, payload []byte) error {
	tmp := path + ".cursor-migrate.tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// containsStoredKey reports whether key already lives in list.
func containsStoredKey(list []string, key string) bool {
	for _, k := range list {
		if k == key {
			return true
		}
	}
	return false
}
