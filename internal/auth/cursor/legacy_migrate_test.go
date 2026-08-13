package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMigrateLegacyCursorAPIKeys(t *testing.T) {
	authDir := t.TempDir()
	store := NewAPIKeyStore(authDir)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgContent := `host: "0.0.0.0"
port: 8317
debug: false
cursor-api-key:
  - api-key: crsr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    base-url: https://api2.cursor.sh
  - api-key: crsr_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  - api-key: crsr_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
other-key:
  - keep: me
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	n, err := MigrateLegacyCursorAPIKeys(store, cfgPath)
	if err != nil {
		t.Fatalf("MigrateLegacyCursorAPIKeys() error = %v", err)
	}
	if n != 3 {
		t.Fatalf("migrated count = %d, want 3", n)
	}

	// All three keys must now be in the JSON store.
	list, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("store length = %d, want 3", len(list))
	}
	seen := map[string]bool{}
	for _, rec := range list {
		if rec.Type != StoredAPIKeyType {
			t.Errorf("rec type = %q, want %q", rec.Type, StoredAPIKeyType)
		}
		seen[rec.APIKey] = true
		if strings.TrimSpace(rec.AuthKind) == "" {
			t.Errorf("auth_kind empty for %q", rec.APIKey)
		}
	}
	for _, want := range []string{
		"crsr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"crsr_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"crsr_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	} {
		if !seen[want] {
			t.Errorf("migrated store missing key %q", want)
		}
	}

	// The legacy block must be stripped from config.yaml; other keys survive.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if strings.Contains(string(data), "cursor-api-key") {
		t.Errorf("config.yaml still contains legacy cursor-api-key block:\n%s", data)
	}
	if !strings.Contains(string(data), "other-key") || !strings.Contains(string(data), "keep: me") {
		t.Errorf("config.yaml lost unrelated keys:\n%s", data)
	}
	// Comments/ordering are not asserted here (comment preservation is tested
	// separately in the config package); we only require unrelated data survives.
}

func TestMigrateLegacyCursorAPIKeys_NoOpWhenAlreadyMigrated(t *testing.T) {
	authDir := t.TempDir()
	store := NewAPIKeyStore(authDir)

	// First migration writes the key and strips the block.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgContent := "cursor-api-key:\n  - api-key: crsr_dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd\n"
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := MigrateLegacyCursorAPIKeys(store, cfgPath); err != nil {
		t.Fatalf("first migration error = %v", err)
	}

	// Second boot: no legacy block remains, so it must be a silent no-op.
	n, err := MigrateLegacyCursorAPIKeys(store, cfgPath)
	if err != nil {
		t.Fatalf("second migration error = %v", err)
	}
	if n != 0 {
		t.Fatalf("second migration count = %d, want 0", n)
	}
	list, _ := store.List()
	if len(list) != 1 {
		t.Fatalf("store length after second migration = %d, want 1 (no duplicates)", len(list))
	}
}

func TestMigrateLegacyCursorAPIKeys_NoLegacyBlockIsNoOp(t *testing.T) {
	authDir := t.TempDir()
	store := NewAPIKeyStore(authDir)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("host: \"127.0.0.1\"\nport: 9000\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	n, err := MigrateLegacyCursorAPIKeys(store, cfgPath)
	if err != nil || n != 0 {
		t.Fatalf("no-op migration = (%d, %v), want (0, nil)", n, err)
	}
	// Config file must remain byte-identical (no rewrite).
	after, _ := os.ReadFile(cfgPath)
	if string(after) != "host: \"127.0.0.1\"\nport: 9000\n" {
		t.Errorf("config.yaml was rewritten unnecessarily:\n%s", after)
	}
}

func TestMigrateLegacyCursorAPIKeys_ZeroKeysAllowed(t *testing.T) {
	// A present-but-empty legacy block should be stripped (it is dead config),
	// and the write path must handle an empty list gracefully.
	authDir := t.TempDir()
	store := NewAPIKeyStore(authDir)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("cursor-api-key: []\nport: 999\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	n, err := MigrateLegacyCursorAPIKeys(store, cfgPath)
	if err != nil {
		t.Fatalf("empty-block migration error = %v", err)
	}
	if n != 0 {
		t.Fatalf("empty-block migration count = %d, want 0", n)
	}
	data, _ := os.ReadFile(cfgPath)
	// The empty legacy block should be gone; unrelated key survives.
	if strings.Contains(string(data), "cursor-api-key") {
		t.Errorf("empty legacy block not stripped:\n%s", data)
	}
	if !strings.Contains(string(data), "port: 999") {
		t.Errorf("port key lost:\n%s", data)
	}
	if err := yaml.Unmarshal(data, &map[string]interface{}{}); err != nil {
		t.Errorf("stripped config is invalid YAML: %v", err)
	}
}
