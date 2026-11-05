package executor

import (
	"encoding/json"
	"strings"
	"sync"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/tiktoken-go/tokenizer"
)

// Cursor bills a turn across the whole tool round-trip: TurnEnded, which is
// where every token count comes from, only arrives once the tool results are
// back. Clients treat each tool round as its own request, so the request that
// ends in a tool call gets no counts at all and shows up as 0 in / 0 out even
// though it sent a full prompt and produced a tool call.
//
// These estimates stand in for counts the provider has not reported yet. They
// never overwrite a reported figure.

var (
	estimatorOnce sync.Once
	estimator     tokenizer.Codec
)

func tokenEstimator() tokenizer.Codec {
	estimatorOnce.Do(func() {
		// cl100k is the closest public encoder available; Cursor does not
		// publish one for the models it fronts, so counts are approximate.
		enc, err := tokenizer.Get(tokenizer.Cl100kBase)
		if err == nil {
			estimator = enc
		}
	})
	return estimator
}

// promptTotalOf reads a usage report's full prompt size. Cursor reports input
// inclusive of the cache buckets, but max() also tolerates an exclusive
// report, where the buckets alone are the larger figure.
func promptTotalOf(input, cacheRead, cacheWrite int64) int64 {
	if buckets := cacheRead + cacheWrite; buckets > input {
		return buckets
	}
	return input
}

func estimateTokens(text string) int64 {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	if enc := tokenEstimator(); enc != nil {
		if ids, _, err := enc.Encode(text); err == nil {
			return int64(len(ids))
		}
	}
	// Roughly four characters per token, the usual rule of thumb.
	return int64((len(text) + 3) / 4)
}

// estimateCursorPromptTokens approximates what the turn's prompt cost.
func estimateCursorPromptTokens(messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition) int64 {
	var b strings.Builder
	for i := range messages {
		msg := &messages[i]
		b.WriteString(msg.Role)
		b.WriteString(msg.Content)
		b.WriteString(msg.Name)
		for j := range msg.ToolCalls {
			b.WriteString(msg.ToolCalls[j].Name)
			if args, err := json.Marshal(msg.ToolCalls[j].Arguments); err == nil {
				b.Write(args)
			}
		}
	}
	for i := range tools {
		b.WriteString(tools[i].Name)
		b.WriteString(tools[i].Description)
		if schema, err := json.Marshal(tools[i].Parameters); err == nil {
			b.Write(schema)
		}
	}
	return estimateTokens(b.String())
}

// estimateCursorOutputTokens approximates what the turn generated.
func estimateCursorOutputTokens(text string, calls []cursorlib.ToolCall) int64 {
	var b strings.Builder
	b.WriteString(text)
	for i := range calls {
		b.WriteString(calls[i].Name)
		if args, err := json.Marshal(calls[i].Arguments); err == nil {
			b.Write(args)
		}
	}
	return estimateTokens(b.String())
}

// fillMissingChatResultUsage is the non-streaming counterpart.
func fillMissingChatResultUsage(result *cursorlib.ChatResult, conversationID string,
	messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition) {
	if result == nil {
		return
	}
	filled := fillMissingCursorUsage(cursorlib.StreamEvent{
		InputTokens:      result.InputTokens,
		OutputTokens:     result.OutputTokens,
		CacheReadTokens:  result.CacheReadTokens,
		CacheWriteTokens: result.CacheWriteTokens,
	}, conversationID, messages, tools, result.Text+result.Thinking, result.ToolCalls)
	result.InputTokens = filled.InputTokens
	result.OutputTokens = filled.OutputTokens
	result.CacheReadTokens = filled.CacheReadTokens
	result.CacheWriteTokens = filled.CacheWriteTokens
}

// fillMissingCursorUsage supplies counts the provider left at zero for a turn
// that demonstrably did work. Reported figures are always preserved.
//
// An estimated prompt is not reported as fresh input wholesale. Requests in an
// agent loop share their prefix with the previous request of the same
// conversation, and Cursor's cache serves that prefix — the turns Cursor does
// bill show hit rates above 99%. Calling the whole prompt fresh made every
// tool round look like a six-figure uncached request, which is exactly the
// dashboard regression this replaces. The split mirrors how Anthropic bills an
// agent loop: the prefix carried over from the previous request is a cache
// read, the new suffix is a cache write, and fresh input stays at zero.
func fillMissingCursorUsage(reported cursorlib.StreamEvent, conversationID string,
	messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition,
	generated string, calls []cursorlib.ToolCall) cursorlib.StreamEvent {
	didWork := strings.TrimSpace(generated) != "" || len(calls) > 0
	if !didWork {
		return reported
	}
	if reported.InputTokens == 0 && reported.CacheReadTokens == 0 && reported.CacheWriteTokens == 0 {
		prompt := estimateCursorPromptTokens(messages, tools)
		if prompt > 0 {
			reported.InputTokens = prompt
			prefix, known := cursorlib.LastPromptTotal(conversationID)
			if known {
				if prefix > prompt {
					prefix = prompt
				}
				reported.CacheReadTokens = prefix
				reported.CacheWriteTokens = prompt - prefix
			} else {
				// First sighting of this conversation: the whole prompt goes
				// into the cache, as the later billed turns confirm.
				reported.CacheWriteTokens = prompt
			}
			cursorlib.RememberPromptTotal(conversationID, prompt)
		}
	}
	if reported.OutputTokens == 0 {
		reported.OutputTokens = estimateCursorOutputTokens(generated, calls)
	}
	return reported
}
