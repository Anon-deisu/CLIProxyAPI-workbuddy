package cursor

import (
	"strings"
	"testing"
)

func TestSplitInjectedContextKeepsRealAsk(t *testing.T) {
	text := strings.Join([]string{
		"<system-reminder>\nCodebase instructions: " + strings.Repeat("x", 2000) + "\n</system-reminder>",
		"<command-message>context-sync</command-message>\n<command-name>/context-sync</command-name>",
		"<local-command-stdout>Set model to Fable 5</local-command-stdout>",
		"[Request interrupted by user]",
		"继续 coe-creator-auth，按 status.md 下一步走。",
	}, "\n")

	injected, ask := splitInjectedContext(text)
	if ask != "继续 coe-creator-auth，按 status.md 下一步走。" {
		t.Fatalf("ask = %q", ask)
	}
	for _, marker := range []string{"system-reminder", "command-name", "local-command-stdout", "Request interrupted"} {
		if !strings.Contains(injected, marker) {
			t.Fatalf("injected context lost %q", marker)
		}
	}
	if strings.Contains(ask, "system-reminder") {
		t.Fatalf("ask still carries injected context: %q", ask)
	}
}

func TestSplitInjectedContextLeavesPlainTextAlone(t *testing.T) {
	injected, ask := splitInjectedContext("just a normal question?")
	if injected != "" || ask != "" {
		t.Fatalf("plain text must be untouched, got injected=%q ask=%q", injected, ask)
	}
}

func TestSplitInjectedContextKeepsContextOnlyMessages(t *testing.T) {
	// Nothing but injected context: keep the original message so we never send
	// an empty user turn.
	injected, ask := splitInjectedContext("<system-reminder>only context</system-reminder>")
	if injected != "" || ask != "" {
		t.Fatalf("context-only message must fall back, got injected=%q ask=%q", injected, ask)
	}
}
