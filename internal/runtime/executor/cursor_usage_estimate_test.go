package executor

import (
	"fmt"
	"strings"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
)

var estimateMessages = []cursorlib.ChatMessage{
	{Role: "system", Content: "You are a coding assistant working in a Go repository."},
	{Role: "user", Content: "What is the weather in Paris?"},
}

var estimateTools = []cursorlib.ToolDefinition{
	{Name: "get_weather", Description: "Get weather for a city",
		Parameters: map[string]any{"type": "object"}},
}

var convSeq int

func freshConv(t *testing.T) string {
	convSeq++
	return fmt.Sprintf("%s-%d", t.Name(), convSeq)
}

// A turn that ends in a tool call gets no TurnEnded from Cursor, because
// Cursor bills the turn across the whole tool round-trip. The estimate must
// attribute the prompt to the cache the way the billed turns prove it is
// served, not report it as fresh uncached input.
func TestFirstToolRoundCreatesCacheRatherThanFreshInput(t *testing.T) {
	calls := []cursorlib.ToolCall{{ID: "c1", Name: "get_weather",
		Arguments: map[string]any{"city": "Paris"}}}

	got := fillMissingCursorUsage(cursorlib.StreamEvent{}, freshConv(t),
		estimateMessages, estimateTools, "", calls)

	if got.InputTokens == 0 {
		t.Error("a turn that sent a full prompt must not report zero input")
	}
	if got.OutputTokens == 0 {
		t.Error("a turn that produced a tool call must not report zero output")
	}
	if got.CacheWriteTokens != got.InputTokens {
		t.Errorf("first round should write the whole prompt to cache: write=%d input=%d",
			got.CacheWriteTokens, got.InputTokens)
	}
	if uncached := got.InputTokens - got.CacheReadTokens - got.CacheWriteTokens; uncached != 0 {
		t.Errorf("nothing should be billed as fresh uncached input, got %d", uncached)
	}
}

func TestSecondToolRoundReadsThePrefixFromCache(t *testing.T) {
	conv := freshConv(t)
	calls := []cursorlib.ToolCall{{ID: "c1", Name: "get_weather",
		Arguments: map[string]any{"city": "Paris"}}}

	first := fillMissingCursorUsage(cursorlib.StreamEvent{}, conv,
		estimateMessages, estimateTools, "", calls)

	grown := append(append([]cursorlib.ChatMessage{}, estimateMessages...),
		cursorlib.ChatMessage{Role: "assistant", ToolCalls: calls},
		cursorlib.ChatMessage{Role: "tool", ToolCallID: "c1",
			Content: strings.Repeat("Sunny, 24 degrees, light wind. ", 40)})
	second := fillMissingCursorUsage(cursorlib.StreamEvent{}, conv,
		grown, estimateTools, "", calls)

	if second.CacheReadTokens != first.InputTokens {
		t.Errorf("the previous prompt should be read back from cache: read=%d, previous=%d",
			second.CacheReadTokens, first.InputTokens)
	}
	if second.CacheWriteTokens != second.InputTokens-second.CacheReadTokens {
		t.Errorf("only the new suffix should be written: write=%d input=%d read=%d",
			second.CacheWriteTokens, second.InputTokens, second.CacheReadTokens)
	}
	if second.CacheWriteTokens <= 0 {
		t.Error("the appended tool result must show up as new cached tokens")
	}
}

func TestBilledTurnsFeedTheLedgerForLaterEstimates(t *testing.T) {
	conv := freshConv(t)
	// A real TurnEnded reported this conversation at 73,260 prompt tokens.
	cursorlib.RememberPromptTotal(conv, 73260)

	calls := []cursorlib.ToolCall{{ID: "c1", Name: "get_weather"}}
	got := fillMissingCursorUsage(cursorlib.StreamEvent{}, conv,
		estimateMessages, estimateTools, "", calls)

	// The estimated prompt here is tiny, far below the ledger figure; the
	// whole of it must then count as cache read, none as fresh.
	if got.CacheReadTokens != got.InputTokens {
		t.Errorf("prompt below the known prefix must be fully cache read: read=%d input=%d",
			got.CacheReadTokens, got.InputTokens)
	}
	if got.CacheWriteTokens != 0 {
		t.Errorf("nothing new to write, got %d", got.CacheWriteTokens)
	}
}

func TestReportedCountsAreNeverOverwritten(t *testing.T) {
	reported := cursorlib.StreamEvent{InputTokens: 107694, OutputTokens: 557,
		CacheReadTokens: 11392, CacheWriteTokens: 56}

	got := fillMissingCursorUsage(reported, freshConv(t), estimateMessages, estimateTools,
		"a long generated answer", nil)

	if got != reported {
		t.Fatalf("provider counts must win over estimates: got %+v want %+v", got, reported)
	}
}

func TestOutputIsFilledWhileCachedInputStaysUntouched(t *testing.T) {
	// Input served entirely from cache is a real report, not a missing count.
	reported := cursorlib.StreamEvent{CacheReadTokens: 25117}

	got := fillMissingCursorUsage(reported, freshConv(t), estimateMessages, estimateTools,
		"hello there", nil)

	if got.InputTokens != 0 || got.CacheReadTokens != 25117 || got.CacheWriteTokens != 0 {
		t.Errorf("reported input buckets were altered: %+v", got)
	}
	if got.OutputTokens == 0 {
		t.Error("generated text must be counted")
	}
}

func TestTurnsThatDidNothingStayZero(t *testing.T) {
	// Estimating here would invent usage for a turn that produced no reply.
	got := fillMissingCursorUsage(cursorlib.StreamEvent{}, freshConv(t),
		estimateMessages, estimateTools, "   ", nil)

	if got.InputTokens != 0 || got.OutputTokens != 0 || got.CacheWriteTokens != 0 {
		t.Fatalf("a turn with no output must not be given estimates: %+v", got)
	}
}

func TestPromptTotalToleratesBothReportingShapes(t *testing.T) {
	if got := promptTotalOf(73260, 73199, 61); got != 73260 {
		t.Errorf("inclusive input misread: %d", got)
	}
	if got := promptTotalOf(0, 73199, 61); got != 73260 {
		t.Errorf("exclusive input misread: %d", got)
	}
}

func TestEstimatesTrackContentSize(t *testing.T) {
	short := estimateTokens("hello")
	long := estimateTokens("hello world, this is a considerably longer sentence to encode")
	if short == 0 || long <= short {
		t.Fatalf("estimates should grow with content: short=%d long=%d", short, long)
	}
	if estimateTokens("") != 0 {
		t.Error("empty text must cost nothing")
	}
}

func TestNonStreamResultGetsTheSameTreatment(t *testing.T) {
	result := &cursorlib.ChatResult{
		Text:      "Here is the answer.",
		ToolCalls: []cursorlib.ToolCall{{ID: "c1", Name: "get_weather"}},
	}

	fillMissingChatResultUsage(result, freshConv(t), estimateMessages, estimateTools)

	if result.InputTokens == 0 || result.OutputTokens == 0 {
		t.Fatalf("non-stream turn still reports zeros: in=%d out=%d",
			result.InputTokens, result.OutputTokens)
	}
	if result.CacheWriteTokens != result.InputTokens {
		t.Fatalf("non-stream estimate must carry the same cache split: write=%d input=%d",
			result.CacheWriteTokens, result.InputTokens)
	}
}
