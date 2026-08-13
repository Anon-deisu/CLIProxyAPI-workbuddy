package cursor

import "testing"

func TestClaudeCodeModelName(t *testing.T) {
	cases := []struct {
		name    string
		modelID string
		want    string
	}{
		{"empty", "", ""},
		{"sonnet wire id", "cursor-claude-4.6-sonnet", "sonnet"},
		{"opus wire id", "cursor-claude-opus-5", "opus"},
		{"haiku wire id", "cursor-claude-haiku-4.5", "haiku"},
		{"fable wire id", "cursor-claude-fable-5", "fable"},
		{"family with suffix", "cursor-claude-opus-5-fast", "opus"},
		{"family with thinking variant", "claude-sonnet-4.5-thinking", "sonnet"},
		{"bare family", "opus", "opus"},
		{"mixed case", "Cursor-Claude-4.6-Sonnet", "sonnet"},
		{"non-claude model", "gpt-5.4", ""},
		{"embedded token not standalone", "sonsonnet", ""},
		{"unknown family", "cursor-claude-4.6-turbo", ""},
		{"family not preceded by claude still maps", "fast-opus-x", "opus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeCodeModelName(tc.modelID); got != tc.want {
				t.Fatalf("claudeCodeModelName(%q) = %q, want %q", tc.modelID, got, tc.want)
			}
		})
	}
}
