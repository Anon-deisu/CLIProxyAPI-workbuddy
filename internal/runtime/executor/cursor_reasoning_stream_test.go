package executor

import "testing"

// Reasoning must not hold the door shut on the recovery paths: they all key off
// contentEmitted, and a turn that has only shown its thinking can still be
// restarted.
func TestReasoningOnlyDeltaDoesNotCountAsContent(t *testing.T) {
	if !deltaIsReasoningOnly(map[string]any{"reasoning_content": "thinking..."}) {
		t.Error("a lone reasoning delta was not recognised")
	}
	for _, delta := range []map[string]any{
		{"content": "hello"},
		{"reasoning_content": "thinking...", "content": "hello"},
		{"tool_calls": []map[string]any{{"index": 0}}},
		{},
	} {
		if deltaIsReasoningOnly(delta) {
			t.Errorf("%v must count as real content", delta)
		}
	}
}
