package usage

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStreamFromContextDefaultsMissingToFalse(t *testing.T) {
	if StreamFromContext(context.Background()) {
		t.Fatalf("StreamFromContext(background) = true, want false")
	}
}

func TestStreamFromContextHonorsExplicitTrue(t *testing.T) {
	ctx := WithStream(context.Background(), true)
	if !StreamFromContext(ctx) {
		t.Fatalf("StreamFromContext(true) = false, want true")
	}
}

func TestRecordStreamField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		Stream:   true,
	}
	if !record.Stream {
		t.Fatalf("Record.Stream = false, want true")
	}
}

func TestGenerateEnabledDefaultsNilToTrue(t *testing.T) {
	if !GenerateEnabled(nil) {
		t.Fatalf("GenerateEnabled(nil) = false, want true")
	}
}

func TestGenerateEnabledHonorsExplicitFalse(t *testing.T) {
	if GenerateEnabled(GenerateFlag(false)) {
		t.Fatalf("GenerateEnabled(false) = true, want false")
	}
}

func TestGenerateEnabledHonorsExplicitTrue(t *testing.T) {
	if !GenerateEnabled(GenerateFlag(true)) {
		t.Fatalf("GenerateEnabled(true) = false, want true")
	}
}

func TestGenerateFromContextDefaultsMissingToTrue(t *testing.T) {
	if !GenerateFromContext(context.Background()) {
		t.Fatalf("GenerateFromContext(background) = false, want true")
	}
}

func TestGenerateFromContextHonorsExplicitFalse(t *testing.T) {
	ctx := WithGenerate(context.Background(), false)
	if GenerateFromContext(ctx) {
		t.Fatalf("GenerateFromContext(false) = true, want false")
	}
}

func TestRecordOmittedGenerateIsEnabled(t *testing.T) {
	// Existing callers construct Record without setting Generate.
	// Omission must remain distinguishable from explicit false and default to true.
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
	}
	if record.Generate != nil {
		t.Fatalf("Record.Generate = %v, want nil for omitted field", record.Generate)
	}
	if !GenerateEnabled(record.Generate) {
		t.Fatalf("GenerateEnabled(omitted) = false, want true")
	}
}

func TestAuthCostLedgerUsesCodexPriceRules(t *testing.T) {
	ledger := newAuthCostLedger()
	now := time.Date(2026, time.January, 12, 10, 0, 0, 0, time.Local)

	ledger.record(Record{
		Provider: "codex",
		AuthID:   "gpt6",
		Model:    "gpt-6-astra",
		Detail: Detail{TokenBreakdown: NewIndependentTokenBreakdown(
			150000, 100000, 50000, 1000, 0, 301000,
		)},
	}, now)
	gotGPT6 := ledger.snapshot("gpt6", now)
	if math.Abs(gotGPT6.EstimatedCostUSD-1.65) > 0.000000001 {
		t.Fatalf("gpt-6 cost = %.12f, want 1.65", gotGPT6.EstimatedCostUSD)
	}

	ledger.record(Record{
		Provider: "codex",
		AuthID:   "gpt56",
		Model:    "gpt-5.6-sol",
		Detail: Detail{TokenBreakdown: NewIndependentTokenBreakdown(
			150000, 100000, 50000, 1000, 0, 301000,
		)},
	}, now)
	gotGPT56 := ledger.snapshot("gpt56", now)
	if math.Abs(gotGPT56.EstimatedCostUSD-1.645) > 0.000000001 {
		t.Fatalf("gpt-5.6 long-context cost = %.12f, want 1.645", gotGPT56.EstimatedCostUSD)
	}
}

func TestBuiltinGPT6PricesIncludeLongContextTier(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  ModelPrice
	}{
		{"gpt-6-sol", ModelPrice{InputPerMillionUSD: 2, CachedInputPerMillionUSD: 0.2, CacheCreationPerMillionUSD: 2.5, OutputPerMillionUSD: 10, LongInputPerMillionUSD: 4, LongCachedInputPerMillionUSD: 0.4, LongCacheCreationPerMillionUSD: 5, LongOutputPerMillionUSD: 15, LongContextThreshold: 272001}},
		{"gpt-6-luna", ModelPrice{InputPerMillionUSD: 0.1, CachedInputPerMillionUSD: 0.01, CacheCreationPerMillionUSD: 0.125, OutputPerMillionUSD: 0.5, LongInputPerMillionUSD: 0.2, LongCachedInputPerMillionUSD: 0.02, LongCacheCreationPerMillionUSD: 0.25, LongOutputPerMillionUSD: 0.75, LongContextThreshold: 272001}},
		{"gpt-6.1-sol", ModelPrice{InputPerMillionUSD: 2, CachedInputPerMillionUSD: 0.1, CacheCreationPerMillionUSD: 2.5, OutputPerMillionUSD: 10, LongInputPerMillionUSD: 4, LongCachedInputPerMillionUSD: 0.2, LongCacheCreationPerMillionUSD: 5, LongOutputPerMillionUSD: 15, LongContextThreshold: 272001}},
	} {
		for _, provider := range []string{"codex", "openai", "openai-compatible-test"} {
			for _, model := range []string{tc.model, tc.model + "-2026-09-22", "openai/" + tc.model + "(max)", " " + strings.ToUpper(tc.model) + ":high ", strings.Replace(tc.model, "gpt-6", "gpt6", 1)} {
				t.Run(provider+"/"+model, func(t *testing.T) {
					price, ok := modelPrice(provider, model)
					if !ok || price != tc.want {
						t.Fatalf("modelPrice(%q, %q) = %+v, %v; want %+v", provider, model, price, ok, tc.want)
					}
				})
			}
		}
	}
}

func TestEstimateGPT6TierBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		input, read, write, output   int64
		wantSol, wantLuna, want61Sol float64
	}{
		{"below threshold", 271999, 0, 0, 1000, 0.553998, 0.0276999, 0.553998},
		{"at threshold", 272000, 0, 0, 1000, 0.554, 0.0277, 0.554},
		{"above threshold", 272001, 0, 0, 1000, 1.103004, 0.0551502, 1.103004},
		{"cache counts toward threshold", 272000, 100000, 22000, 1000, 0.385, 0.01925, 0.375},
		{"cache crosses threshold", 272001, 100000, 22001, 1000, 0.765005, 0.03825025, 0.745005},
		{"all cache reads", 272001, 272001, 0, 1000, 0.1238004, 0.00619002, 0.0694002},
		{"all cache writes", 272001, 0, 272001, 1000, 1.375005, 0.06875025, 1.375005},
		{"output does not select tier", 200000, 0, 0, 128000, 1.68, 0.084, 1.68},
		{"standard mixed input", 270000, 100000, 20000, 1000, 0.38, 0.019, 0.37},
		{"long mixed input", 300000, 100000, 50000, 1000, 0.905, 0.04525, 0.885},
	} {
		for _, model := range []struct {
			name string
			want float64
		}{{"gpt-6-sol", tc.wantSol}, {"gpt-6-luna", tc.wantLuna}, {"gpt-6.1-sol", tc.want61Sol}} {
			for _, provider := range []string{"codex", "openai"} {
				t.Run(provider+"/"+model.name+"/"+tc.name, func(t *testing.T) {
					for _, detail := range []Detail{
						{InputTokens: tc.input, CacheReadTokens: tc.read, CacheCreationTokens: tc.write, OutputTokens: tc.output},
						{TokenBreakdown: NewSubsetTokenBreakdown(tc.input, tc.read, tc.write, tc.output, tc.output/2, tc.input+tc.output)},
					} {
						got, input, output, unpricedInput, unpricedOutput := estimateRecordCost(Record{Provider: provider, Model: model.name, Detail: detail})
						if math.Abs(got-model.want) > 1e-12 {
							t.Fatalf("cost = %.12f, want %.12f", got, model.want)
						}
						if input != tc.input || output != tc.output || unpricedInput != 0 || unpricedOutput != 0 {
							t.Fatalf("incorrect token totals: input=%d output=%d unpriced=%d/%d", input, output, unpricedInput, unpricedOutput)
						}
					}
				})
			}
		}
	}
}

func TestAuthCostLedgerGPT6SelectsTierPerRequest(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  float64
	}{{"gpt-6-sol", 1.285}, {"gpt-6-luna", 0.06425}, {"gpt-6.1-sol", 1.255}} {
		t.Run(tc.model, func(t *testing.T) {
			ledger := newAuthCostLedger()
			now := time.Date(2026, time.September, 23, 10, 0, 0, 0, time.UTC)
			for _, detail := range []Detail{
				{InputTokens: 270000, CacheReadTokens: 100000, CacheCreationTokens: 20000, OutputTokens: 1000},
				{InputTokens: 300000, CacheReadTokens: 100000, CacheCreationTokens: 50000, OutputTokens: 1000},
			} {
				ledger.record(Record{Provider: "codex", AuthID: "account", Model: tc.model, Detail: detail}, now)
			}
			got := ledger.snapshot("account", now)
			if math.Abs(got.EstimatedCostUSD-tc.want) > 1e-12 || got.InputTokens != 570000 || got.OutputTokens != 2000 {
				t.Fatalf("cost snapshot = %+v, want per-request total %.12f", got, tc.want)
			}
		})
	}
}

func TestAuthCostLedgerTracksRecentModelCostBuckets(t *testing.T) {
	ledger := newAuthCostLedger()
	first := time.Date(2026, time.September, 23, 10, 0, 0, 0, time.Local)
	next := first.Add(10 * time.Minute)

	ledger.record(Record{
		Provider: "openai", AuthID: "auth-1", Model: "gpt-5",
		Detail: Detail{InputTokens: 1_000_000},
	}, first)
	ledger.record(Record{
		Provider: "openai", AuthID: "auth-1", Model: "gpt-5-mini",
		Detail: Detail{InputTokens: 1_000_000},
	}, first)
	ledger.record(Record{
		Provider: "openai", AuthID: "auth-1", Model: "gpt-5",
		Detail: Detail{InputTokens: 500_000},
	}, next)

	got := ledger.modelCostBuckets("auth-1", next)
	if len(got) != 3 {
		t.Fatalf("cost bucket count = %d, want 3", len(got))
	}
	if got[0].Model != "gpt-5" || math.Abs(got[0].EstimatedCostUSD-1.25) > 1e-12 {
		t.Fatalf("first cost bucket = %+v, want gpt-5 $1.25", got[0])
	}
	if got[1].Model != "gpt-5-mini" || math.Abs(got[1].EstimatedCostUSD-0.25) > 1e-12 {
		t.Fatalf("second cost bucket = %+v, want gpt-5-mini $0.25", got[1])
	}
	if got[2].BucketID != got[0].BucketID+1 || got[2].Model != "gpt-5" || math.Abs(got[2].EstimatedCostUSD-0.625) > 1e-12 {
		t.Fatalf("next cost bucket = %+v, want gpt-5 $0.625", got[2])
	}
}

func TestAuthCostLedgerTracksModelTokenUsageAndResetsIt(t *testing.T) {
	ledger := newAuthCostLedger()
	first := time.Date(2026, time.September, 23, 10, 0, 0, 0, time.Local)

	ledger.record(Record{Provider: "openai", AuthID: "auth-1", Model: "gpt-5", Detail: Detail{
		TokenBreakdown: NewSubsetTokenBreakdown(100, 40, 10, 30, 0, 130),
	}}, first)
	ledger.record(Record{Provider: "claude", AuthID: "auth-1", Model: "claude-sonnet-4", Detail: Detail{
		TokenBreakdown: NewIndependentTokenBreakdown(200, 25, 5, 40, 10, 280),
	}}, first)
	ledger.record(Record{Provider: "other", AuthID: "auth-1", Model: "unpriced-model", Detail: Detail{
		InputTokens: 17, CachedTokens: 7, OutputTokens: 3,
	}}, first)
	ledger.record(Record{Provider: "openai", AuthID: "other-auth", Model: "gpt-5", Detail: Detail{
		TokenBreakdown: NewSubsetTokenBreakdown(10, 4, 0, 5, 0, 15),
	}}, first)

	got := ledger.modelTokenUsage("auth-1")
	if len(got) != 3 {
		t.Fatalf("model token usage count = %d, want 3: %+v", len(got), got)
	}
	if got[0] != (AuthModelTokenUsage{Model: "claude-sonnet-4", InputTokens: 230, CachedInputTokens: 25, OutputTokens: 50}) {
		t.Fatalf("highest-input Claude token usage = %+v", got[0])
	}
	if got[1] != (AuthModelTokenUsage{Model: "gpt-5", InputTokens: 100, CachedInputTokens: 40, OutputTokens: 30}) {
		t.Fatalf("OpenAI token usage = %+v", got[1])
	}
	if got[2] != (AuthModelTokenUsage{Model: "unpriced-model", InputTokens: 17, CachedInputTokens: 7, OutputTokens: 3}) {
		t.Fatalf("unpriced model token usage = %+v", got[2])
	}

	ledger.record(Record{Provider: "openai", AuthID: "auth-1", Model: "gpt-5", Detail: Detail{InputTokens: 10, OutputTokens: 2}}, first.AddDate(0, 0, 30))
	want := []AuthModelTokenUsage{
		{Model: "claude-sonnet-4", InputTokens: 230, CachedInputTokens: 25, OutputTokens: 50},
		{Model: "gpt-5", InputTokens: 110, CachedInputTokens: 40, OutputTokens: 32},
		{Model: "unpriced-model", InputTokens: 17, CachedInputTokens: 7, OutputTokens: 3},
	}
	if got := ledger.modelTokenUsage("auth-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("cumulative token usage after 30 days = %+v, want %+v", got, want)
	}
	ledger.reset("auth-1", first.Add(time.Hour))
	if got := ledger.modelTokenUsage("auth-1"); len(got) != 0 {
		t.Fatalf("manually reset model token usage = %+v, want none", got)
	}
	if got := ledger.modelTokenUsage("other-auth"); len(got) != 1 || got[0].InputTokens != 10 || got[0].CachedInputTokens != 4 {
		t.Fatalf("reset changed another account's model usage: %+v", got)
	}
	ledger.record(Record{AuthID: "auth-1", Model: "after-reset", Failed: true}, first.Add(2*time.Hour))
	if got := ledger.modelTokenUsage("auth-1"); len(got) != 1 || got[0] != (AuthModelTokenUsage{Model: "after-reset"}) {
		t.Fatalf("zero-usage failed model after reset = %+v", got)
	}
}

func TestQueuedUsageCannotRestoreResetModelTotals(t *testing.T) {
	manager := NewManager(4)
	manager.Publish(t.Context(), Record{AuthID: "account", Provider: "openai", Model: "gpt-5", Detail: Detail{InputTokens: 100}})
	manager.ResetAuthCost("account")
	// Delivery happens after the reset; only the plugin queue should be consumed.
	manager.dispatch(queueItem{record: Record{AuthID: "account", Provider: "openai", Model: "gpt-5", Detail: Detail{InputTokens: 100}}})
	if got := manager.AuthModelTokens("account"); len(got) != 0 {
		t.Fatalf("queued usage restored reset model totals: %+v", got)
	}
	manager.Stop()
}

func TestModelTokenUsagePreservesDistinctModelIDs(t *testing.T) {
	ledger := newAuthCostLedger()
	for _, model := range []string{"vendor-a/model", "vendor-b/model", "Model", "model(high)"} {
		ledger.record(Record{AuthID: "account", Provider: "openai", Model: model, Detail: Detail{InputTokens: 100, CacheReadTokens: 100, OutputTokens: 2}}, time.Now())
	}
	got := ledger.modelTokenUsage("account")
	if len(got) != 4 {
		t.Fatalf("distinct model IDs were combined: %+v", got)
	}
	for _, model := range got {
		if model.InputTokens != 100 || model.CachedInputTokens != 100 || model.OutputTokens != 2 {
			t.Fatalf("fully cached input was counted twice: %+v", model)
		}
	}
	if got := newAuthCostLedger().modelTokenUsage("account"); len(got) != 0 {
		t.Fatalf("new statistics lifecycle retained previous records: %+v", got)
	}
}

func TestGPT6LegacyAstraPriceAliasesArePreserved(t *testing.T) {
	for _, model := range []string{"gpt-6", "gpt6", "gpt-6-astra", "openai/gpt-6-astra(max)"} {
		got, ok := modelPrice("codex", model)
		if !ok || got != BuiltinCodexModelPrices["gpt-6-astra"] {
			t.Fatalf("legacy Astra pricing changed for %q", model)
		}
	}
}

func TestAuthCostLedgerPreservesSnapshotsAcrossLocalDayBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first time.Time
	}{
		{"midnight", time.Date(2026, time.January, 12, 23, 59, 0, 0, time.Local)},
		{"month boundary", time.Date(2026, time.January, 31, 23, 59, 0, 0, time.Local)},
		{"year boundary", time.Date(2026, time.December, 31, 23, 59, 0, 0, time.Local)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newAuthCostLedger()
			ledger.record(Record{
				Provider: "openai", AuthID: "auth-1", Model: "gpt-4o",
				Detail: Detail{InputTokens: 1000000, OutputTokens: 1000},
			}, tc.first)
			ledger.record(Record{
				Provider: "openai", AuthID: "auth-1", Model: "unpriced-test-model",
				Detail: Detail{InputTokens: 50, OutputTokens: 7},
			}, tc.first)
			want := ledger.snapshot("auth-1", tc.first)
			if want.InputTokens != 1000050 || want.OutputTokens != 1007 || math.Abs(want.EstimatedCostUSD-2.51) > 1e-9 || want.UnpricedInputTokens != 50 || want.UnpricedOutputTokens != 7 {
				t.Fatalf("unexpected initial snapshot: %+v", want)
			}
			for _, now := range []time.Time{tc.first.Add(2 * time.Minute), tc.first.AddDate(0, 0, 7)} {
				if got := ledger.snapshot("auth-1", now); got != want {
					t.Fatalf("snapshot at %s = %+v, want unchanged %+v", now, got, want)
				}
			}
		})
	}
}

func TestAuthCostLedgerAccumulatesAcrossMidnightWithoutSnapshot(t *testing.T) {
	ledger := newAuthCostLedger()
	first := time.Date(2026, time.December, 31, 23, 59, 0, 0, time.Local)
	second := first.Add(2 * time.Minute)
	records := []Record{
		{Provider: "openai", AuthID: "auth-1", Model: "gpt-4o", Detail: Detail{InputTokens: 1000000, OutputTokens: 1000}},
		{Provider: "openai", AuthID: "auth-1", Model: "unpriced-test-model", Detail: Detail{InputTokens: 50, OutputTokens: 7}},
	}
	for _, record := range records {
		ledger.record(record, first)
	}
	want := ledger.snapshot("auth-1", first)
	// Exercise the write path before any next-day snapshot can touch the ledger.
	for _, record := range records {
		ledger.record(record, second)
	}
	want.InputTokens *= 2
	want.OutputTokens *= 2
	want.EstimatedCostUSD *= 2
	want.UnpricedInputTokens *= 2
	want.UnpricedOutputTokens *= 2
	want.UpdatedAt = second
	if got := ledger.snapshot("auth-1", second); got != want {
		t.Fatalf("cross-day cumulative snapshot = %+v, want %+v", got, want)
	}
}

func TestAuthCostLedgerExplicitResetStartsNewCycleForOneAuth(t *testing.T) {
	ledger := newAuthCostLedger()
	first := time.Date(2026, time.January, 12, 23, 59, 0, 0, time.Local)
	resetAt := first.Add(2 * time.Minute)
	record := Record{
		Provider: "openai", AuthID: "auth-1", Model: "gpt-4o",
		Detail: Detail{InputTokens: 1000000, OutputTokens: 1000},
	}
	ledger.record(record, first)
	wantAfterRecord := ledger.snapshot("auth-1", first)
	ledger.record(Record{
		Provider: "openai", AuthID: "auth-1", Model: "unpriced-test-model",
		Detail: Detail{InputTokens: 50, OutputTokens: 7},
	}, first)
	otherRecord := record
	otherRecord.AuthID = "auth-2"
	ledger.record(otherRecord, first)
	wantOther := ledger.snapshot("auth-2", first)

	ledger.reset("auth-1", resetAt)
	wantReset := AuthCostSnapshot{AuthID: "auth-1", CycleDate: costCycleDate(resetAt), UpdatedAt: resetAt}
	if got := ledger.snapshot("auth-1", resetAt); got != wantReset {
		t.Fatalf("explicit reset snapshot = %+v, want %+v", got, wantReset)
	}
	if got := ledger.snapshot("auth-2", resetAt); got != wantOther {
		t.Fatalf("reset changed another account: %+v, want %+v", got, wantOther)
	}
	nextDay := resetAt.AddDate(0, 0, 1)
	if got := ledger.snapshot("auth-1", nextDay); got != wantReset {
		t.Fatalf("empty cycle changed across midnight: %+v, want %+v", got, wantReset)
	}
	ledger.record(record, nextDay)
	wantAfterRecord.CycleDate = costCycleDate(resetAt)
	wantAfterRecord.UpdatedAt = nextDay
	if got := ledger.snapshot("auth-1", nextDay); got != wantAfterRecord {
		t.Fatalf("usage after reset = %+v, want %+v", got, wantAfterRecord)
	}
}
