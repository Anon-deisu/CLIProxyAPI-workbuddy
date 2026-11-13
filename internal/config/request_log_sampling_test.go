package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRequestLogSamplingLoadValidation(t *testing.T) {
	for _, value := range []string{"-0.01", "1.01", ".nan", ".inf", "-.inf", "true", "'0.5'", "[]", "{}"} {
		for _, layout := range []string{"legacy", "v8"} {
			for _, optional := range []bool{false, true} {
				t.Run(layout+"/"+value+"/optional="+map[bool]string{false: "false", true: "true"}[optional], func(t *testing.T) {
					raw := "request-log-sample-rate: " + value + "\n"
					if layout == "v8" {
						raw = "observability:\n  logs:\n    request-log-sample-rate: " + value + "\n"
						if err := ValidateV8Config([]byte(raw)); err == nil || !strings.Contains(err.Error(), "request-log-sample-rate") {
							t.Errorf("ValidateV8Config = %v, want sampling range error", err)
						}
					}
					path := filepath.Join(t.TempDir(), "config.yaml")
					if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
						t.Fatal(err)
					}
					if _, err := LoadConfigOptional(path, optional); err == nil || !strings.Contains(err.Error(), "request-log-sample-rate") {
						t.Fatalf("LoadConfigOptional = %v, want sampling range error", err)
					}
				})
			}
		}
	}
}

func TestRequestLogSamplingRoundTrip(t *testing.T) {
	for _, value := range []string{"0", "0.25", "1"} {
		t.Run(value, func(t *testing.T) {
			raw := "request-log: true\nrequest-log-sample-rate: " + value + "\n"
			cfg := Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
			if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatal(err)
			}
			encoded, err := yaml.Marshal(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _, err = NormalizeConfigLayout(encoded, true)
			if err != nil {
				t.Fatal(err)
			}
			var doc yaml.Node
			if err = yaml.Unmarshal(encoded, &doc); err != nil {
				t.Fatal(err)
			}
			node := yamlPath(doc.Content[0], "observability.logs.request-log-sample-rate")
			if node == nil || node.Value != value {
				t.Fatalf("v8 sampling rate = %v, want %s", node, value)
			}
			if err = ValidateV8Config(encoded); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err = os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.RequestLogSampleRate == nil || loaded.EffectiveRequestLogSampleRate() != cfg.EffectiveRequestLogSampleRate() || !loaded.RequestLog {
				t.Fatal("loading v8 changed request logging settings")
			}
		})
	}
}

func TestRequestLogSamplingDefaultsAndClone(t *testing.T) {
	var cfg Config
	if cfg.RequestLogSampleRate != nil || cfg.EffectiveRequestLogSampleRate() != 1 {
		t.Fatal("absent sampling rate must preserve full request logging")
	}
	rate := 0.25
	cfg.RequestLogSampleRate = &rate
	clone := cfg.CloneForRuntime()
	*clone.RequestLogSampleRate = 0
	if cfg.EffectiveRequestLogSampleRate() != 0.25 {
		t.Fatal("runtime clone shares the sampling rate pointer")
	}
	reloaded := (&Config{}).CloneForReload(&cfg)
	if reloaded.RequestLogSampleRate != nil || reloaded.EffectiveRequestLogSampleRate() != 1 {
		t.Fatal("removing the rate on reload must restore full sampling")
	}
}

func TestRequestLogSamplingCanonicalPresence(t *testing.T) {
	for _, value := range []string{"0", "null"} {
		raw := "request-log-sample-rate: 0.5\nobservability:\n  logs:\n    request-log: true\n    request-log-sample-rate: " + value + "\n"
		var cfg Config
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		if value == "null" {
			if cfg.RequestLogSampleRate != nil || cfg.EffectiveRequestLogSampleRate() != 1 {
				t.Fatal("null did not restore full sampling")
			}
		} else if cfg.RequestLogSampleRate == nil || cfg.EffectiveRequestLogSampleRate() != 0 {
			t.Fatal("explicit canonical zero was lost")
		}
	}
}
