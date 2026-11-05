package executor

import (
	"errors"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
)

func TestTurnsWithNothingForTheClientAreFailures(t *testing.T) {
	cases := map[string]*cursorlib.ChatResult{
		// The dashboard case: input billed, nothing generated.
		"billed input, no reply": {InputTokens: 107694},
		"cached input, no reply": {CacheReadTokens: 25117},
		// The 389k/0 case: the stream died so early not even usage exists.
		// Gateways then count the prompt locally and record a huge 0-output row.
		"no usage, no reply": {},
		// Counters claim output but none of it reached the payload.
		"phantom output tokens": {OutputTokens: 40},
	}
	for name, result := range cases {
		if !generatedNothing(result, nil) {
			t.Errorf("%s must count as a failed turn", name)
		}
	}
}

func TestTurnsWithRealContentPassThrough(t *testing.T) {
	cases := map[string]*cursorlib.ChatResult{
		"answered":       {InputTokens: 100, OutputTokens: 12, Text: "hello"},
		"tool call":      {InputTokens: 100, ToolCalls: []cursorlib.ToolCall{{ID: "c1", Name: "Bash"}}},
		"reasoning only": {InputTokens: 100, OutputTokens: 40, Thinking: "thinking..."},
		"text no usage":  {Text: "short"},
	}
	for name, result := range cases {
		if generatedNothing(result, nil) {
			t.Errorf("%s was wrongly treated as an empty turn", name)
		}
	}
}

func TestUpstreamErrorsKeepTheirOwnHandling(t *testing.T) {
	if generatedNothing(nil, errors.New("boom")) {
		t.Fatal("an error must not be reported as an empty turn")
	}
	if generatedNothing(nil, nil) {
		t.Fatal("a nil result must not panic or report empty")
	}
}
