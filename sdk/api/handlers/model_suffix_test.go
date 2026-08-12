package handlers

import "testing"

func TestStripsLongContextMarker(t *testing.T) {
	// Agents append this when they want the long-context variant; the id in
	// front of it is the model we actually have.
	cases := map[string]string{
		"claude-opus-4-8[1m]":   "claude-opus-4-8",
		"claude-sonnet-4-5[1M]": "claude-sonnet-4-5",
		"gpt-5.6-sol[200k]":     "gpt-5.6-sol",
		"  claude-opus-5[1m] ":  "claude-opus-5",
	}
	for in, want := range cases {
		if got := stripContextWindowSuffix(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestLeavesEverythingElseAlone(t *testing.T) {
	for _, in := range []string{
		"claude-opus-4-8",
		"claude-opus-5-thinking-high",
		"gpt-5.4(8192)",
		"my-model[beta]",  // not a context window
		"weird[1m]suffix", // marker must be at the end
		"",
	} {
		if got := stripContextWindowSuffix(in); got != in {
			t.Errorf("%q was rewritten to %q", in, got)
		}
	}
}
