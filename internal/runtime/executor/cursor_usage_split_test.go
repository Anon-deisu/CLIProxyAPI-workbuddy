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

func TestCursorUsageParallelCacheBuckets(t *testing.T) {
	// Anthropic-official and some cross-account upstreams report input_tokens as
	// the fresh portion only, with the cache read/write buckets parallel to it.
	// The cache can then legitimately exceed the fresh input (a request reuses a
	// big cached prefix against a small new tail). Clamping the overflow to zero
	// dropped the real fresh input from the statistics — the "cache missing /
	// input went to zero" reports. The fresh input is the input itself; only the
	// inclusive shape (buckets inside input) gets the subtraction.
	got := cursorUsageDetail(100, 5, 400, 50, 0)
	if got.InputTokens != 100 {
		t.Fatalf("input = %d, want the 100 fresh tokens kept", got.InputTokens)
	}
	// Every bucket counted once: 100 fresh + 400 read + 50 written + 5 out.
	if got.TotalTokens != 555 {
		t.Fatalf("total = %d, want 555 (fresh input must not be dropped)", got.TotalTokens)
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

// Downstream gateways read prompt_tokens as the billable input; reporting the
// cache inside it made every request look like tens of thousands of fresh
// input tokens with no hit, and priced it that way.
func TestWirePayloadReportsOnlyTheUncachedInput(t *testing.T) {
	payload := cursorUsagePayload(25249, 11, 25117, 130, 0)
	if payload["prompt_tokens"] != int64(2) {
		t.Fatalf("prompt_tokens = %v, want the 2 uncached tokens", payload["prompt_tokens"])
	}
	details, _ := payload["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != int64(25117) || details["cache_creation_tokens"] != int64(130) {
		t.Fatalf("cache details = %v", details)
	}
	if payload["total_tokens"] != int64(25260) {
		t.Fatalf("total_tokens = %v, want every bucket counted once", payload["total_tokens"])
	}
}

// The Anthropic surface subtracts the cache from prompt_tokens itself; with the
// split already applied it must not subtract a second time.
func TestSplitPayloadSurvivesTheClaudeTranslator(t *testing.T) {
	payload := cursorUsagePayload(25249, 11, 25117, 130, 0)
	prompt := payload["prompt_tokens"].(int64)
	details := payload["prompt_tokens_details"].(map[string]any)
	cached := details["cached_tokens"].(int64) + details["cache_creation_tokens"].(int64)
	if prompt >= cached {
		t.Fatalf("prompt %d >= cache %d would be subtracted again downstream", prompt, cached)
	}
}

func TestSplitCursorInputBothShapes(t *testing.T) {
	cases := []struct {
		name               string
		input, read, write int64
		wantUncached       int64
	}{
		{"inclusive input, buckets inside it", 25249, 25117, 130, 2},
		{"inclusive, exactly equal buckets", 250, 200, 50, 0},
		{"parallel buckets exceed fresh input", 100, 400, 50, 100},
		{"parallel, no cache at all", 300, 0, 0, 300},
		{"parallel, read only exceeds", 50, 500, 0, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := splitCursorInput(c.input, c.read, c.write); got != c.wantUncached {
				t.Fatalf("splitCursorInput(%d,%d,%d) = %d, want %d", c.input, c.read, c.write, got, c.wantUncached)
			}
		})
	}
}
