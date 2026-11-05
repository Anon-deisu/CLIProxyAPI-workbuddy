package responses

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// responsesRequestEcho captures request fields once and preserves the typed
// conversions and field order used by both streaming and non-streaming replies.
type responsesRequestEcho struct {
	instructions          string
	hasInstructions       bool
	maxOutputTokens       int64
	hasMaxOutputTokens    bool
	maxTokens             int64
	hasMaxTokens          bool
	maxToolCalls          int64
	hasMaxToolCalls       bool
	model                 string
	hasModel              bool
	parallelToolCalls     bool
	hasParallelToolCalls  bool
	previousResponseID    string
	hasPreviousResponseID bool
	promptCacheKey        string
	hasPromptCacheKey     bool
	reasoning             any
	hasReasoning          bool
	safetyIdentifier      string
	hasSafetyIdentifier   bool
	serviceTier           string
	hasServiceTier        bool
	store                 bool
	hasStore              bool
	temperature           float64
	hasTemperature        bool
	text                  any
	hasText               bool
	toolChoice            any
	hasToolChoice         bool
	tools                 any
	hasTools              bool
	topLogprobs           int64
	hasTopLogprobs        bool
	topP                  float64
	hasTopP               bool
	truncation            string
	hasTruncation         bool
	user                  any
	hasUser               bool
	metadata              any
	hasMetadata           bool
}

func extractResponsesRequestEcho(root ojson.Result) responsesRequestEcho {
	var e responsesRequestEcho
	root.ForEach(func(key, value ojson.Result) bool {
		switch key.String() {
		case "instructions":
			if !e.hasInstructions {
				e.instructions, e.hasInstructions = strings.Clone(value.String()), true
			}
		case "max_output_tokens":
			if !e.hasMaxOutputTokens {
				e.maxOutputTokens, e.hasMaxOutputTokens = value.Int(), true
			}
		case "max_tokens":
			if !e.hasMaxTokens {
				e.maxTokens, e.hasMaxTokens = value.Int(), true
			}
		case "max_tool_calls":
			if !e.hasMaxToolCalls {
				e.maxToolCalls, e.hasMaxToolCalls = value.Int(), true
			}
		case "model":
			if !e.hasModel {
				e.model, e.hasModel = strings.Clone(value.String()), true
			}
		case "parallel_tool_calls":
			if !e.hasParallelToolCalls {
				e.parallelToolCalls, e.hasParallelToolCalls = value.Bool(), true
			}
		case "previous_response_id":
			if !e.hasPreviousResponseID {
				e.previousResponseID, e.hasPreviousResponseID = strings.Clone(value.String()), true
			}
		case "prompt_cache_key":
			if !e.hasPromptCacheKey {
				e.promptCacheKey, e.hasPromptCacheKey = strings.Clone(value.String()), true
			}
		case "reasoning":
			if !e.hasReasoning {
				e.reasoning, e.hasReasoning = ojson.Parse(strings.Clone(value.Raw)).Value(), true
			}
		case "safety_identifier":
			if !e.hasSafetyIdentifier {
				e.safetyIdentifier, e.hasSafetyIdentifier = strings.Clone(value.String()), true
			}
		case "service_tier":
			if !e.hasServiceTier {
				e.serviceTier, e.hasServiceTier = strings.Clone(value.String()), true
			}
		case "store":
			if !e.hasStore {
				e.store, e.hasStore = value.Bool(), true
			}
		case "temperature":
			if !e.hasTemperature {
				e.temperature, e.hasTemperature = value.Float(), true
			}
		case "text":
			if !e.hasText {
				e.text, e.hasText = ojson.Parse(strings.Clone(value.Raw)).Value(), true
			}
		case "tool_choice":
			if !e.hasToolChoice {
				e.toolChoice, e.hasToolChoice = ojson.Parse(strings.Clone(value.Raw)).Value(), true
			}
		case "tools":
			if !e.hasTools {
				e.tools, e.hasTools = ojson.Parse(strings.Clone(value.Raw)).Value(), true
			}
		case "top_logprobs":
			if !e.hasTopLogprobs {
				e.topLogprobs, e.hasTopLogprobs = value.Int(), true
			}
		case "top_p":
			if !e.hasTopP {
				e.topP, e.hasTopP = value.Float(), true
			}
		case "truncation":
			if !e.hasTruncation {
				e.truncation, e.hasTruncation = strings.Clone(value.String()), true
			}
		case "user":
			if !e.hasUser {
				e.user, e.hasUser = ojson.Parse(strings.Clone(value.Raw)).Value(), true
			}
		case "metadata":
			if !e.hasMetadata {
				e.metadata, e.hasMetadata = ojson.Parse(strings.Clone(value.Raw)).Value(), true
			}
		}
		return true
	})
	return e
}

func (e responsesRequestEcho) applyDoc(doc *ojson.Doc, prefix string) {
	if e.hasInstructions {
		_ = doc.Set(prefix+"instructions", e.instructions)
	}
	if e.hasMaxOutputTokens {
		_ = doc.Set(prefix+"max_output_tokens", e.maxOutputTokens)
	}
	if e.hasMaxToolCalls {
		_ = doc.Set(prefix+"max_tool_calls", e.maxToolCalls)
	}
	if e.hasModel {
		_ = doc.Set(prefix+"model", e.model)
	}
	if e.hasParallelToolCalls {
		_ = doc.Set(prefix+"parallel_tool_calls", e.parallelToolCalls)
	}
	if e.hasPreviousResponseID {
		_ = doc.Set(prefix+"previous_response_id", e.previousResponseID)
	}
	if e.hasPromptCacheKey {
		_ = doc.Set(prefix+"prompt_cache_key", e.promptCacheKey)
	}
	if e.hasReasoning {
		_ = doc.Set(prefix+"reasoning", e.reasoning)
	}
	if e.hasSafetyIdentifier {
		_ = doc.Set(prefix+"safety_identifier", e.safetyIdentifier)
	}
	if e.hasServiceTier {
		_ = doc.Set(prefix+"service_tier", e.serviceTier)
	}
	if e.hasStore {
		_ = doc.Set(prefix+"store", e.store)
	}
	if e.hasTemperature {
		_ = doc.Set(prefix+"temperature", e.temperature)
	}
	if e.hasText {
		_ = doc.Set(prefix+"text", e.text)
	}
	if e.hasToolChoice {
		_ = doc.Set(prefix+"tool_choice", e.toolChoice)
	}
	if e.hasTools {
		_ = doc.Set(prefix+"tools", e.tools)
	}
	if e.hasTopLogprobs {
		_ = doc.Set(prefix+"top_logprobs", e.topLogprobs)
	}
	if e.hasTopP {
		_ = doc.Set(prefix+"top_p", e.topP)
	}
	if e.hasTruncation {
		_ = doc.Set(prefix+"truncation", e.truncation)
	}
	if e.hasUser {
		_ = doc.Set(prefix+"user", e.user)
	}
	if e.hasMetadata {
		_ = doc.Set(prefix+"metadata", e.metadata)
	}
}
