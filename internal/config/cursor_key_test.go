package config

import "testing"

func TestParseConfigSanitizesCursorKeys(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
cursor-api-key:
  - api-key: " key-one "
    prefix: " team "
    excluded-models: [" Claude-Opus-* ", "claude-opus-*"]
  - api-key: "key-one"
    prefix: "duplicate"
    base-url: " https://api2.cursor.sh/ "
  - api-key: " "
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.CursorKey) != 1 {
		t.Fatalf("cursor key count = %d, want 1", len(cfg.CursorKey))
	}
	entry := cfg.CursorKey[0]
	if entry.APIKey != "key-one" || entry.Prefix != "team" || entry.BaseURL != "" {
		t.Fatalf("sanitized cursor key = %#v", entry)
	}
	if len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "claude-opus-*" {
		t.Fatalf("excluded models = %#v", entry.ExcludedModels)
	}
}
