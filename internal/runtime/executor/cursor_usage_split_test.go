package executor

import "testing"

// Measured against Cursor: prompt_tokens 25249 = cached 25117 + written 130 + 2
// genuinely fresh tokens.
func TestCursorInputExcludesTheCacheBuckets(t *testing.T) {
	got := cursorUsageDetail(25249, 11, 25117, 130, 0)

	if got.InputTokens != 2 {
		t.Errorf("input = %d, want the 2 uncached tokens", got.InputTokens)
	}
	if got.CacheReadTokens != 25117 || got.CacheCreationTokens != 130 {
		t.Errorf("cache buckets = %d/%d", got.CacheReadTokens, got.CacheCreationTokens)
	}
	// The relay totals input + output + both cache buckets, so counting the
	// cache inside input as well inflated a long session into the millions.
	if got.TotalTokens != 25260 {
		t.Errorf("total = %d, want 25260", got.TotalTokens)
	}
	if got.InputTokens+got.CacheReadTokens+got.CacheCreationTokens != 25249 {
		t.Error("the three input buckets must add back up to what Cursor reported")
	}
}

func TestCursorUsageNeverGoesNegative(t *testing.T) {
	// Cursor occasionally reports cache figures that exceed its own input.
	got := cursorUsageDetail(100, 5, 400, 50, 0)
	if got.InputTokens != 0 {
		t.Fatalf("input = %d, want 0", got.InputTokens)
	}
	if got.TotalTokens != 455 {
		t.Fatalf("total = %d, want 455", got.TotalTokens)
	}
}

func TestCursorUsageWithoutCacheIsUnchanged(t *testing.T) {
	got := cursorUsageDetail(300, 40, 0, 0, 12)
	if got.InputTokens != 300 || got.OutputTokens != 40 || got.TotalTokens != 340 {
		t.Fatalf("got %+v", got)
	}
	if got.ReasoningTokens != 12 {
		t.Fatalf("reasoning tokens lost: %+v", got)
	}
}

// The wire payload keeps OpenAI semantics, where prompt_tokens is defined to
// include the cached part; only our internal accounting splits them.
func TestWirePayloadStillReportsOpenAIPromptTokens(t *testing.T) {
	payload := cursorUsagePayload(25249, 11, 25117, 130, 0)
	if payload["prompt_tokens"] != int64(25249) {
		t.Fatalf("prompt_tokens = %v, want the inclusive figure", payload["prompt_tokens"])
	}
	details, _ := payload["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != int64(25117) || details["cache_creation_tokens"] != int64(130) {
		t.Fatalf("cache details = %v", details)
	}
}
