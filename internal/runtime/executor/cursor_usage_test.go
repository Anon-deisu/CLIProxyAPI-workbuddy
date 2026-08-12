package executor

import (
	"encoding/json"
	"testing"
)

func TestCursorUsagePayloadIncludesCacheTokens(t *testing.T) {
	// Cursor's input_tokens is the complete input; cache details are subsets.
	got := cursorUsagePayload(9000, 50, 8000, 200, 30)
	raw, _ := json.Marshal(got)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["prompt_tokens"].(float64) != 9000 {
		t.Fatalf("prompt_tokens should preserve Cursor input: %v", out["prompt_tokens"])
	}
	details, _ := out["prompt_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"].(float64) != 8000 {
		t.Fatalf("cached_tokens missing: %v", out)
	}
	if details["cache_creation_tokens"].(float64) != 200 {
		t.Fatalf("cache_creation_tokens missing: %v", details)
	}
	comp, _ := out["completion_tokens_details"].(map[string]any)
	if comp == nil || comp["reasoning_tokens"].(float64) != 30 {
		t.Fatalf("reasoning_tokens missing: %v", out)
	}
}

func TestCursorUsagePayloadOmitsEmptyDetails(t *testing.T) {
	got := cursorUsagePayload(10, 5, 0, 0, 0)
	if _, ok := got["prompt_tokens_details"]; ok {
		t.Fatalf("should not emit empty details: %v", got)
	}
	if got["total_tokens"].(int64) != 15 {
		t.Fatalf("total_tokens=%v", got["total_tokens"])
	}
}

func TestCursorUsagePayloadBucketsSumToPrompt(t *testing.T) {
	// Live sample: Cursor reported input == cacheRead + cacheWrite.
	const input, cacheRead, cacheWrite = 50740, 25332, 25404
	got := cursorUsagePayload(input, 121, cacheRead, cacheWrite, 0)
	prompt := got["prompt_tokens"].(int64)
	anthropicInput := prompt - cacheRead - cacheWrite
	if anthropicInput != 4 {
		t.Fatalf("uncached input = %d, want 4", anthropicInput)
	}
	if anthropicInput+cacheRead+cacheWrite != input {
		t.Fatalf("buckets %d+%d+%d != %d", anthropicInput, cacheRead, cacheWrite, input)
	}
}
