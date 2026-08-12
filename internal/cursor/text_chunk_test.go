package cursor

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitTextForSSEKeepsRunesIntact(t *testing.T) {
	text := strings.Repeat("我是 Opus 模型，为你提供帮助。", 20) + "🚀🎉"
	parts := splitTextForSSE(text, sseChunkBytes)
	if len(parts) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(parts))
	}
	for i, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatalf("chunk %d is not valid UTF-8: %q", i, p)
		}
		if strings.ContainsRune(p, utf8.RuneError) {
			t.Fatalf("chunk %d contains replacement char: %q", i, p)
		}
		if len(p) > sseChunkBytes+utf8.UTFMax {
			t.Fatalf("chunk %d too large: %d bytes", i, len(p))
		}
	}
	if got := strings.Join(parts, ""); got != text {
		t.Fatalf("reassembled text differs from input")
	}
}

func TestSplitTextForSSEShortAndEmpty(t *testing.T) {
	if got := splitTextForSSE("hi", 64); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("short text should pass through: %#v", got)
	}
	if got := splitTextForSSE("", 64); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty text should pass through: %#v", got)
	}
	// A single rune larger than the limit must still be emitted whole.
	if got := splitTextForSSE("🚀", 2); len(got) != 1 || got[0] != "🚀" {
		t.Fatalf("oversized rune must not be split: %#v", got)
	}
}
