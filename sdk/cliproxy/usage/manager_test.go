package usage

import (
	"context"
	"math"
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

func TestPrismCostUsesCodexPriceRules(t *testing.T) {
	prism, ok := modelPrice("prism", "gpt-6-astra")
	codex, _ := modelPrice("codex", "gpt-6-astra")
	if !ok || prism != codex {
		t.Fatalf("Prism price = %+v (%t), want Codex price %+v", prism, ok, codex)
	}
}

func TestAuthCostLedgerResetsAtLocalDayBoundaryAndExplicitReset(t *testing.T) {
	ledger := newAuthCostLedger()
	first := time.Date(2026, time.January, 12, 23, 59, 0, 0, time.Local)
	second := first.Add(2 * time.Minute)
	record := Record{
		Provider: "openai",
		AuthID:   "auth-1",
		Model:    "gpt-4o",
		Detail:   Detail{InputTokens: 1000000, OutputTokens: 0},
	}
	ledger.record(record, first)
	if got := ledger.snapshot("auth-1", first); got.InputTokens != 1000000 {
		t.Fatalf("first cycle input tokens = %d, want 1000000", got.InputTokens)
	}
	if got := ledger.snapshot("auth-1", second); got.InputTokens != 0 || got.EstimatedCostUSD != 0 {
		t.Fatalf("new day snapshot = %+v, want cleared values", got)
	}

	ledger.record(record, second)
	ledger.reset("auth-1", second)
	if got := ledger.snapshot("auth-1", second); got.InputTokens != 0 || got.EstimatedCostUSD != 0 {
		t.Fatalf("explicit reset snapshot = %+v, want cleared values", got)
	}
}
