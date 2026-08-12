package cursor

import (
	"regexp"
	"strings"
)

// Claude Code injects context into the user turn as extra content blocks:
// codebase reminders, slash-command metadata, hook output, local command stdout.
// Those arrive merged into one user message, and on a first turn they can be
// tens of kilobytes while the actual instruction is a single trailing line.
// Sending the merged blob as the Agent's UserMessage makes models answer
// "this only contains system context, there is no actual question".
var injectedContextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?s)<local-command-caveat>.*?</local-command-caveat>`),
	regexp.MustCompile(`(?s)<local-command-stdout>.*?</local-command-stdout>`),
	regexp.MustCompile(`(?s)<command-name>.*?</command-name>`),
	regexp.MustCompile(`(?s)<command-message>.*?</command-message>`),
	regexp.MustCompile(`(?s)<command-args>.*?</command-args>`),
	regexp.MustCompile(`\[Request interrupted by user[^\]]*\]`),
}

// splitInjectedContext separates client-injected context from the user's actual
// instruction. It returns the context (may be empty) and the remaining ask.
// When nothing recognizable is left, ask is empty and callers should keep the
// original text untouched.
func splitInjectedContext(text string) (context string, ask string) {
	if strings.TrimSpace(text) == "" {
		return "", ""
	}
	var parts []string
	remainder := text
	for _, re := range injectedContextPatterns {
		remainder = re.ReplaceAllStringFunc(remainder, func(match string) string {
			parts = append(parts, match)
			return "\n"
		})
	}
	if len(parts) == 0 {
		return "", ""
	}
	ask = strings.TrimSpace(remainder)
	if ask == "" {
		return "", ""
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n")), ask
}
