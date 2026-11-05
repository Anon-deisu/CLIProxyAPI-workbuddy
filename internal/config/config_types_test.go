package config

import "testing"

func TestCodexEffectiveSignInWithChatGPTIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name, configured, want string
	}{
		{"unset", "", DefaultSignInWithChatGPTIdentifier},
		{"blank", " \t\n", DefaultSignInWithChatGPTIdentifier},
		{"custom", "  ExampleClient/2.0 \t", "ExampleClient/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := CodexConfig{SignInWithChatGPTIdentifier: tc.configured}
			if got := cfg.EffectiveSignInWithChatGPTIdentifier(); got != tc.want {
				t.Fatalf("identifier = %q, want %q", got, tc.want)
			}
			if cfg.SignInWithChatGPTIdentifier != tc.configured {
				t.Fatal("reading the effective identifier mutated the configuration")
			}
		})
	}
}
