package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func TestPanelV8LoadKeepsHashedSecretAfterConflictCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("# keep comment\nport: 8316\nserver: {port: 8317}\nmanagement: {secret-key: test-management-secret}\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(saved, &doc); err != nil {
		t.Fatal(err)
	}
	secret := yamlPath(doc.Content[0], "management.secret-key")
	if secret == nil || bcrypt.CompareHashAndPassword([]byte(secret.Value), []byte("test-management-secret")) != nil {
		t.Fatal("layout cleanup restored a plaintext management secret")
	}
	if secret.Value != cfg.RemoteManagement.SecretKey || cfg.Port != 8317 {
		t.Fatal("persisted and runtime configuration differ")
	}
	if yamlPath(doc.Content[0], "port") != nil || !bytes.Contains(saved, []byte("# keep comment")) {
		t.Fatal("layout cleanup did not preserve the canonical setting and comments")
	}
	if _, err = LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, reloaded) {
		t.Fatalf("repeated load changed the hashed document: %v", err)
	}
}

func TestPanelV8ConfigPresenceAndGroupInheritance(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`request-retry: 4
disable-cooling: true
api-keys: [old]
access: {api-keys: []}
routing:
  retry: {request-retry: 0}
  cooldown: {disable-cooling: false}
api-keys:
  codex:
    - name: shared
      base-url: https://example.invalid
      priority: 7
      headers: {X-Test: inherited}
      keys:
        - api-key: first
          priority: null
        - api-key: second
          weight: 0
          priority: 0
          headers: {}
`))
	if err == nil {
		t.Fatal("duplicate api-keys must be rejected before layout conversion")
	}
	raw := `request-retry: 4
disable-cooling: true
access: {api-keys: []}
routing:
  retry: {request-retry: 0}
  cooldown: {disable-cooling: false}
api-keys:
  codex:
    - name: shared
      base-url: https://example.invalid
      priority: 7
      headers: {X-Test: inherited}
      keys:
        - api-key: first
          priority: null
        - api-key: second
          weight: 0
          priority: 0
          headers: {}
`
	cfg, err = ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestRetry != 0 || cfg.DisableCooling || len(cfg.APIKeys) != 0 {
		t.Fatal("explicit v8 zero, false and empty values must win")
	}
	if len(cfg.CodexKey) != 2 || cfg.CodexKey[0].Priority != 7 || cfg.CodexKey[0].Headers["X-Test"] != "inherited" {
		t.Fatal("group settings were not inherited")
	}
	second := cfg.CodexKey[1]
	if second.Priority != 0 || second.Weight == nil || *second.Weight != 0 || len(second.Headers) != 0 {
		t.Fatal("explicit key overrides were not preserved")
	}
}

func TestPanelV8OAuthSettingsNormalizeOrder(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`oauth:
  settings:
    " META ":
      - {name: " muse-spark-1.3 ", max-context-length: 4096}
      - {name: "muse-spark-1.3", alias: " public ", max-context-length: 8192}
      - {name: "muse-spark-1.3", max-context-length: 16384}
      - {name: " ", max-context-length: 2048}
`))
	if err != nil {
		t.Fatal(err)
	}
	settings := cfg.OAuthSettings["meta"]
	if len(settings) != 2 || settings[0].Alias != "public" || settings[1].MaxContextLength != 16384 {
		t.Fatalf("OAuth settings did not retain last occurrence order: %#v", settings)
	}
	if got := ResolveOAuthModelSetting(settings, "public", "muse-spark-1.3", ""); got == nil || got.MaxContextLength != 8192 {
		t.Fatal("alias-specific setting must override the general setting")
	}
}

func TestPanelV8MigrationPreservesCustomConfiguration(t *testing.T) {
	raw := []byte(`host: 127.0.0.1
port: 8317
api-keys: [client]
codex:
  quota-refresh: {enabled: false, interval: 7m, concurrency: 3}
relay: {enabled: false}
account-scheduling:
  persist-settings: true
  scheduler-mode: remaining-quota
  auto-disable-unauthorized: true
  codex-static-isp:
    enabled: true
    nodes:
      - {id: node, host: proxy.invalid, port: 1080, username: user, password: secret}
`)
	before, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := NormalizeConfigLayout(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	after.OAuthOnlyFields = nil
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration lost custom runtime or persisted configuration")
	}
	if strings.Contains(string(data), "# account-scheduling:") || strings.Contains(string(data), "# relay:") {
		t.Fatal("custom configuration must remain active, not archived as comments")
	}
}

func TestPanelV8SavePreservesGroupIdentityAndLegacyEndpoints(t *testing.T) {
	raw := []byte(`config-version: 8
server: {host: 127.0.0.1, port: 8317}
access: {api-keys: [client]}
api-keys:
  codex:
    - name: keep-this-group
      base-url: https://example.invalid
      headers: {X-Group: stable}
      keys:
        - {api-key: first, weight: 0, headers: null}
        - {api-key: second, headers: {}}
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProxyURL = "direct"
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(saved, &doc); err != nil {
		t.Fatal(err)
	}
	group := yamlPath(doc.Content[0], "api-keys.codex")
	if group == nil || len(group.Content) != 1 || yamlPath(group.Content[0], "name").Value != "keep-this-group" {
		t.Fatal("a legacy save changed the group identity")
	}
	if bytes.Contains(saved, []byte("codex-api-key:")) {
		t.Fatal("a legacy save reintroduced shadowing credential fields")
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.ProxyURL != "direct" || reloaded.CodexKey[0].Weight == nil || *reloaded.CodexKey[0].Weight != 0 {
		t.Fatalf("save/reload did not preserve configuration: %v", err)
	}
}
