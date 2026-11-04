package usage

import (
	"testing"
	"time"
)

func TestRecentTokenUsageSnapshotAggregatesRollingBuckets(t *testing.T) {
	manager := NewManager(1)
	base := time.Unix(1_800_000_000, 0).Truncate(10 * time.Minute)

	manager.costLedger.record(Record{
		Provider: "openai",
		Generate: GenerateFlag(true),
		Detail:   Detail{InputTokens: 100, CacheReadTokens: 25, CacheCreationTokens: 10, OutputTokens: 30},
	}, base)
	manager.costLedger.record(Record{
		Provider: "claude",
		Detail:   Detail{TokenBreakdown: NewIndependentTokenBreakdown(50, 20, 5, 10, 0, 85)},
	}, base.Add(10*time.Minute))
	manager.costLedger.record(Record{
		Provider: "openai",
		Generate: GenerateFlag(false),
		Detail:   Detail{InputTokens: 1000, CacheReadTokens: 900, OutputTokens: 500},
	}, base.Add(10*time.Minute))
	got := manager.RecentTokenUsageSnapshot(base.Add(10 * time.Minute))
	wantRate := float64(45) / 175 * 100
	if got.WindowMinutes != 200 || got.InputTokens != 175 || got.CachedTokens != 45 || got.OutputTokens != 40 || got.CacheRate != wantRate {
		t.Fatalf("recent token usage = %+v, want 200-minute window, input/cache/output=175/45/40, cache rate=%.2f%%", got, wantRate)
	}
}

func TestRecentTokenUsageSnapshotExpiresOldBuckets(t *testing.T) {
	manager := NewManager(1)
	base := time.Unix(1_800_000_000, 0).Truncate(10 * time.Minute)
	manager.costLedger.record(Record{
		Provider: "openai",
		Detail:   Detail{InputTokens: 100, CacheReadTokens: 25, OutputTokens: 10},
	}, base)

	got := manager.RecentTokenUsageSnapshot(base.Add(200 * time.Minute))
	if got.InputTokens != 0 || got.CachedTokens != 0 || got.OutputTokens != 0 || got.CacheRate != 0 {
		t.Fatalf("expired token usage = %+v, want an empty snapshot", got)
	}
}

func TestRecentTokenUsageSnapshotKeepsKnownPartialBreakdown(t *testing.T) {
	manager := NewManager(1)
	now := time.Unix(1_800_000_000, 0)
	manager.costLedger.record(Record{
		Provider: "openai",
		Detail:   Detail{TokenBreakdown: NewPartialSubsetTokenBreakdown(100, 25, 0, 40, 0, 160)},
	}, now)

	got := manager.RecentTokenUsageSnapshot(now)
	if got.InputTokens != 100 || got.CachedTokens != 25 || got.OutputTokens != 40 || got.CacheRate != 25 {
		t.Fatalf("partial token usage = %+v, want input/cache/output=100/25/40 and cache rate 25%%", got)
	}
}

func TestRecentTokenUsageSnapshotMergesAuthFilesAndSortsModelsByInput(t *testing.T) {
	manager := NewManager(1)
	base := time.Unix(1_800_000_000, 0).Truncate(10 * time.Minute)
	for _, sample := range []struct {
		auth, model        string
		input, cached, out int64
		offset             time.Duration
		generate           *bool
	}{
		{auth: "auth-a", model: "shared", input: 80, cached: 25, out: 10},
		{auth: "auth-a", model: "winner", input: 500, cached: 50, out: 60, offset: 10 * time.Minute},
		{auth: "auth-a", model: "shared", input: 120, cached: 30, out: 20, offset: 20 * time.Minute},
		{auth: "auth-b", model: "winner", input: 400, cached: 10, out: 30, offset: 20 * time.Minute},
		{auth: "auth-a", model: "ignored", input: 1000, out: 1000, offset: 20 * time.Minute, generate: GenerateFlag(false)},
	} {
		manager.costLedger.record(Record{
			Provider: "openai", AuthID: sample.auth, Model: sample.model, Generate: sample.generate,
			Detail: Detail{InputTokens: sample.input, CacheReadTokens: sample.cached, OutputTokens: sample.out},
		}, base.Add(sample.offset))
	}

	got := manager.RecentTokenUsageSnapshot(base.Add(20 * time.Minute))
	if got.InputTokens != 1100 || len(got.Models) != 2 {
		t.Fatalf("rolling token summary = %+v, want 1100 input tokens merged across auth files", got)
	}
	if got.Models[0] != (AuthModelTokenUsage{Model: "winner", InputTokens: 900, CachedInputTokens: 60, OutputTokens: 90}) ||
		got.Models[1] != (AuthModelTokenUsage{Model: "shared", InputTokens: 200, CachedInputTokens: 55, OutputTokens: 30}) {
		t.Fatalf("global model usage = %+v, want merged per-model totals sorted by input", got.Models)
	}

	expired := manager.RecentTokenUsageSnapshot(base.Add(400 * time.Minute))
	if len(expired.Models) != 0 {
		t.Fatalf("expired rolling model details = %+v, want none", expired.Models)
	}
}
