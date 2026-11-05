package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCompressionConfigPersistsExplicitFalse(t *testing.T) {
	for _, source := range []string{"canonical", "mirror", "json"} {
		t.Run(source, func(t *testing.T) {
			disabled := false
			cfg, err := ParseConfigBytes([]byte("{}"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Codex.WebsocketCompression = &disabled
			switch source {
			case "canonical":
				cfg.ResponseCompression.Enabled = &disabled
			case "mirror":
				cfg.ServerResponseCompression.Enabled = &disabled
			case "json":
				if err := json.Unmarshal([]byte(`{"response-compression":{"enabled":false},"codex":{"websocket-compression":false}}`), cfg); err != nil {
					t.Fatal(err)
				}
			}
			assertDisabled := func(data []byte) {
				t.Helper()
				loaded, err := ParseConfigBytes(data)
				if err != nil {
					t.Fatal(err)
				}
				if loaded.EffectiveResponseCompression().IsEnabled() || loaded.Codex.WebsocketCompressionEnabled() {
					t.Fatalf("compression opt-out lost after round trip:\n%s", data)
				}
			}
			snapshot, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertDisabled(snapshot)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err = os.WriteFile(path, []byte("config-version: 8\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(saved); err != nil {
				t.Fatal(err)
			}
			assertDisabled(saved)
		})
	}
}

func TestResponseCompressionConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		enabled  bool
		minBytes int
	}{
		{name: "defaults", input: "{}", enabled: true, minBytes: 1024},
		{name: "disabled", input: "server:\n  response-compression:\n    enabled: false\n    min-bytes: 64", minBytes: 64},
		{name: "enabled", input: "server:\n  response-compression:\n    enabled: true\n    min-bytes: -1", enabled: true, minBytes: 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if errUnmarshal := yaml.Unmarshal([]byte(tc.input), &cfg); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if got := cfg.ServerResponseCompression.IsEnabled(); got != tc.enabled {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.enabled)
			}
			if got := cfg.ServerResponseCompression.EffectiveMinBytes(); got != tc.minBytes {
				t.Fatalf("EffectiveMinBytes() = %d, want %d", got, tc.minBytes)
			}
			encoded, errMarshal := json.Marshal(cfg)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var roundTrip Config
			if errUnmarshal := json.Unmarshal(encoded, &roundTrip); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if roundTrip.ServerResponseCompression.IsEnabled() != tc.enabled || roundTrip.ServerResponseCompression.EffectiveMinBytes() != tc.minBytes {
				t.Fatal("JSON round trip changed compression configuration")
			}
		})
	}
}

func TestCodexWebsocketCompressionConfig(t *testing.T) {
	var nilConfig *CodexConfig
	if !nilConfig.WebsocketCompressionEnabled() {
		t.Fatal("nil configuration must enable WebSocket compression")
	}
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{input: "{}", want: true},
		{input: "codex:\n  websocket-compression: true", want: true},
		{input: "codex:\n  websocket-compression: false", want: false},
	} {
		var cfg Config
		if errUnmarshal := yaml.Unmarshal([]byte(tc.input), &cfg); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		if got := cfg.Codex.WebsocketCompressionEnabled(); got != tc.want {
			t.Fatalf("WebsocketCompressionEnabled() = %v, want %v", got, tc.want)
		}
	}
}
