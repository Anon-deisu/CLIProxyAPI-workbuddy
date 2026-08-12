package util

import "testing"

func TestRegistryModelIDStripsCursorSuffixes(t *testing.T) {
	cases := map[string]string{
		"grok-4.5":                    "grok-4.5",
		"grok-4.5-fast":               "grok-4.5",
		"grok-4.5-high-fast":          "grok-4.5",
		"claude-fable-5-thinking":     "claude-fable-5",
		"claude-fable-5-thinking-high": "claude-fable-5",
		"gpt-5.4(high)":               "gpt-5.4",
		"gpt-5.4-fast":                "gpt-5.4",
	}
	for in, want := range cases {
		if got := RegistryModelID(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}
