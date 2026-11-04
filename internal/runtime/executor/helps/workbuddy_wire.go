package helps

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

var workBuddyToolNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)
var workBuddyBillingHeader = regexp.MustCompile(`(?im)^x-anthropic-billing-header:[ \t]*(?:(?:cc_[a-z0-9_]+|cch)=[^;\r\n]*;[ \t]*)+(?:\r?\n)?`)
var workBuddyClaudeSignature = regexp.MustCompile(`(?i)You are Claude Code, Anthropic['\x{2019}]s official CLI for Claude`)
var workBuddyClaudeFeedback = regexp.MustCompile(`(?i)To give feedback, users should report the issue at (https://github\.com/anthropics/claude-code/issues)\b`)
var workBuddyDSHSignature = regexp.MustCompile(`(?i)^You are an AI agent powered by DeepSeek Harness`)
var workBuddyDSHGUIContext = regexp.MustCompile(`(?i)You are interacting with the user through the DeepSeek Harness Web GUI`)
var workBuddyDSHPathReference = regexp.MustCompile(`(?i)Tokens prefixed with @ are paths the user explicitly referenced\. Relative paths resolve from the workspace root`)
var workBuddyResetTime = regexp.MustCompile(`(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\s+UTC([+-])(\d{1,2})(?::(\d{2}))?`)

type workBuddyErrorDetails struct {
	Code    json.Number `json:"code"`
	Msg     string      `json:"msg"`
	Message string      `json:"message"`
}

type workBuddyErrorEnvelope struct {
	workBuddyErrorDetails
	Error struct {
		workBuddyErrorDetails
		Data workBuddyErrorDetails `json:"data"`
	} `json:"error"`
}

func parseWorkBuddyError(payload []byte) (string, string, bool) {
	var envelope workBuddyErrorEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", "", false
	}
	code := envelope.Code.String()
	message := envelope.Msg
	if message == "" {
		message = envelope.Message
	}
	for _, detail := range []workBuddyErrorDetails{envelope.Error.workBuddyErrorDetails, envelope.Error.Data} {
		if detail.Code == "" {
			continue
		}
		code = detail.Code.String()
		if detail.Msg != "" {
			message = detail.Msg
		} else if detail.Message != "" {
			message = detail.Message
		}
	}
	return code, message, code != ""
}

// IsWorkBuddyAccountBanned reports the WorkBuddy 11140 account ban.
func IsWorkBuddyAccountBanned(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "11140"
}

// IsWorkBuddyQuotaExhausted reports the WorkBuddy 14018 account credit limit.
func IsWorkBuddyQuotaExhausted(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "14018"
}

// IsWorkBuddyRateLimit reports the WorkBuddy 6004 account usage limit.
func IsWorkBuddyRateLimit(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "6004"
}

// IsWorkBuddyModelRateLimit reports the WorkBuddy 6000 model usage limit.
func IsWorkBuddyModelRateLimit(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "6000"
}

// IsWorkBuddyQueueWaiting reports the WorkBuddy 6020 admission-queue throttle.
// The account is healthy and the upstream only asks for a short wait, so this
// must stay a request-scoped 429 instead of a credential quota cooldown.
func IsWorkBuddyQueueWaiting(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "6020"
}

// WorkBuddyQueueWaitingRetryAfter returns the queue's advertised retry delay.
func WorkBuddyQueueWaitingRetryAfter(payload []byte, now time.Time) *time.Duration {
	if !IsWorkBuddyQueueWaiting(payload) {
		return nil
	}
	seconds := gjson.GetBytes(payload, "data.retry_after").Int()
	if seconds <= 0 {
		return nil
	}
	delay := time.Duration(seconds) * time.Second
	return &delay
}

// WorkBuddyRateLimitRetryAfter returns the advertised reset delay for a
// recognized WorkBuddy usage limit. A nil result means the payload is not a
// recognized limit or does not contain a parseable reset time.
func WorkBuddyRateLimitRetryAfter(payload []byte, now time.Time) *time.Duration {
	code, message, ok := parseWorkBuddyError(payload)
	if !ok || code != "6004" && code != "6000" {
		return nil
	}

	match := workBuddyResetTime.FindStringSubmatch(message)
	if len(match) != 5 {
		return nil
	}
	hourOffset, err := strconv.Atoi(match[3])
	if err != nil || hourOffset > 23 {
		return nil
	}
	minuteOffset := 0
	if match[4] != "" {
		minuteOffset, err = strconv.Atoi(match[4])
		if err != nil || minuteOffset > 59 {
			return nil
		}
	}
	if match[2] == "-" {
		hourOffset = -hourOffset
		minuteOffset = -minuteOffset
	}
	zoneOffset := (hourOffset*60 + minuteOffset) * 60
	zone := time.FixedZone("WorkBuddy", zoneOffset)
	resetAt, err := time.ParseInLocation("2006-01-02 15:04:05", match[1], zone)
	if err != nil {
		return nil
	}
	delay := resetAt.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return &delay
}

// NormalizeWorkBuddyRequest adapts Chat Completions to the desktop gateway.
func NormalizeWorkBuddyRequest(body []byte) ([]byte, map[string]string, error) {
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, nil, fmt.Errorf("workbuddy: invalid chat request: %w", err)
	}
	chat["stream"] = true
	if value, ok := chat["max_completion_tokens"]; ok {
		if _, exists := chat["max_tokens"]; !exists {
			chat["max_tokens"] = value
		}
		delete(chat, "max_completion_tokens")
	}
	if stop, ok := chat["stop"].(string); ok {
		chat["stop"] = []string{stop}
	}
	messages, _ := chat["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] == "developer" {
			message["role"] = "system"
		}
		if text, ok := message["content"].(string); ok {
			message["content"] = normalizeWorkBuddyClientText(text)
		} else if parts, ok := message["content"].([]any); ok {
			for _, rawPart := range parts {
				part, _ := rawPart.(map[string]any)
				if text, ok := part["text"].(string); ok {
					part["text"] = normalizeWorkBuddyClientText(text)
				}
			}
		}
	}
	if len(messages) == 0 || messages[0].(map[string]any)["role"] != "system" {
		messages = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, messages...)
	}
	chat["messages"] = messages

	tools, _ := chat["tools"].([]any)
	taken := make(map[string]bool)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name != "" && !workBuddyToolNameChars.MatchString(name) {
			taken[name] = true
		}
	}
	names, upstreamNames := make(map[string]string), make(map[string]string)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if workBuddyToolNameChars.MatchString(name) {
			base := workBuddyToolNameChars.ReplaceAllString(name, "_")
			safe := base
			for suffix := 2; taken[safe]; suffix++ {
				safe = fmt.Sprintf("%s_%d", base, suffix)
			}
			taken[safe] = true
			names[safe], upstreamNames[name], fn["name"] = name, safe, safe
		}
		parameters, ok := fn["parameters"].(map[string]any)
		if !ok {
			parameters = map[string]any{}
		}
		parameters = normalizeWorkBuddySchema(parameters).(map[string]any)
		if _, ok := parameters["type"]; !ok {
			parameters["type"] = "object"
		}
		if parameters["type"] == "object" && parameters["properties"] == nil {
			parameters["properties"] = map[string]any{}
		}
		fn["parameters"] = parameters
	}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] == "tool" {
			if name, ok := message["name"].(string); ok && upstreamNames[name] != "" {
				message["name"] = upstreamNames[name]
			}
		}
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok && upstreamNames[name] != "" {
				fn["name"] = upstreamNames[name]
			}
		}
	}
	if choice, ok := chat["tool_choice"].(map[string]any); ok {
		switch choice["type"] {
		case "function":
			fn, _ := choice["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if safe := upstreamNames[name]; safe != "" {
				name = safe
			}
			chat["tool_choice"] = name
		case "auto", "required", "none":
			chat["tool_choice"] = choice["type"]
		default:
			delete(chat, "tool_choice")
		}
	}
	if chat["tool_choice"] == "none" {
		delete(chat, "tools")
		delete(chat, "functions")
		delete(chat, "tool_choice")
	}
	result, err := json.Marshal(chat)
	return result, names, err
}

// WorkBuddy rejects these client boilerplates before routing to the model.
func normalizeWorkBuddyClientText(text string) string {
	text = workBuddyBillingHeader.ReplaceAllString(text, "")
	text = workBuddyClaudeSignature.ReplaceAllString(text, "You are Claude Code, Anthropic's CLI for Claude")
	text = workBuddyClaudeFeedback.ReplaceAllString(text, "Users can report issues and share feedback at ${1}")
	text = workBuddyDSHSignature.ReplaceAllString(text, "You are an AI agent built with DeepSeek Harness")
	text = workBuddyDSHGUIContext.ReplaceAllString(text, "You interact with the user through the DeepSeek Harness Web GUI")
	text = workBuddyDSHPathReference.ReplaceAllString(text, "Tokens starting with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root")
	text = strings.ReplaceAll(text, "Main branch (you will usually use this for PRs)", "Main branch (usually the base for PRs)")
	const codexSignature = "Codex CLI is an open source project led by OpenAI."
	if index := strings.Index(text, codexSignature); index >= 0 && (index < 4 || !strings.EqualFold(text[index-4:index], "the ")) {
		text = text[:index] + "The " + text[index:]
	}
	return text
}

// The gateway rejects boolean schemas, untyped enums and tuple-style items.
// Only schema positions are visited; property names and literal values stay intact.
func normalizeWorkBuddySchema(value any) any {
	if boolean, ok := value.(bool); ok {
		if boolean {
			return map[string]any{}
		}
		return map[string]any{"not": map[string]any{}}
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if enum, ok := schema["enum"].([]any); ok && schema["type"] == nil && len(enum) > 0 {
		groups := make(map[string][]any)
		var order []string
		for _, item := range enum {
			kind := "null"
			switch item.(type) {
			case string:
				kind = "string"
			case float64:
				kind = "number"
			case bool:
				kind = "boolean"
			case []any:
				kind = "array"
			case map[string]any:
				kind = "object"
			}
			if _, exists := groups[kind]; !exists {
				order = append(order, kind)
			}
			groups[kind] = append(groups[kind], item)
		}
		if len(order) == 1 {
			schema["type"] = order[0]
		} else {
			branches := make([]any, 0, len(order))
			for _, kind := range order {
				branches = append(branches, map[string]any{"type": kind, "enum": groups[kind]})
			}
			delete(schema, "enum")
			if existing, ok := schema["anyOf"]; ok {
				all, _ := schema["allOf"].([]any)
				schema["allOf"] = append(all, map[string]any{"anyOf": existing}, map[string]any{"anyOf": branches})
				delete(schema, "anyOf")
			} else {
				schema["anyOf"] = branches
			}
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if properties, ok := schema[key].(map[string]any); ok {
			for name, child := range properties {
				properties[name] = normalizeWorkBuddySchema(child)
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if children, ok := schema[key].([]any); ok {
			for i, child := range children {
				children[i] = normalizeWorkBuddySchema(child)
			}
		}
	}
	for _, key := range []string{"items", "additionalItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"} {
		child, exists := schema[key]
		if !exists {
			continue
		}
		if _, boolean := child.(bool); boolean && (key == "additionalProperties" || key == "unevaluatedProperties") {
			continue
		}
		if tuple, ok := child.([]any); ok && key == "items" {
			child = map[string]any{"anyOf": tuple}
			delete(schema, "additionalItems")
		}
		schema[key] = normalizeWorkBuddySchema(child)
	}
	return schema
}

// NormalizeWorkBuddyChunk restores caller tool names and removes empty call lists.
func NormalizeWorkBuddyChunk(payload []byte, model string, names map[string]string) ([]byte, error) {
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		return nil, err
	}
	frame["model"] = model
	frame["object"] = "chat.completion.chunk"
	choices, _ := frame["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		for _, key := range []string{"delta", "message"} {
			message, _ := choice[key].(map[string]any)
			calls, exists := message["tool_calls"].([]any)
			if exists && len(calls) == 0 {
				delete(message, "tool_calls")
			}
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				if name, ok := fn["name"].(string); ok && names[name] != "" {
					fn["name"] = names[name]
				}
			}
		}
	}
	return json.Marshal(frame)
}

// SplitWorkBuddyFinishChunks keeps final text/tool deltas separate from the
// terminal event, which the shared Gemini translator processes independently.
func SplitWorkBuddyFinishChunks(payload []byte) [][]byte {
	var frame map[string]any
	_ = json.Unmarshal(payload, &frame)
	choices, _ := frame["choices"].([]any)
	var finished []any
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		finish, _ := choice["finish_reason"].(string)
		delta, _ := choice["delta"].(map[string]any)
		if finish == "" || len(delta) == 0 {
			continue
		}
		finished = append(finished, map[string]any{"index": choice["index"], "delta": map[string]any{}, "finish_reason": finish})
		delete(choice, "finish_reason")
	}
	if len(finished) == 0 {
		return [][]byte{payload}
	}
	terminal := make(map[string]any, len(frame))
	for key, value := range frame {
		if key != "choices" {
			terminal[key] = value
		}
	}
	terminal["choices"] = finished
	delete(frame, "usage")
	content, _ := json.Marshal(frame)
	end, _ := json.Marshal(terminal)
	return [][]byte{content, end}
}

type workBuddyToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type workBuddyChoice struct {
	content, reasoning strings.Builder
	finish             string
	tools              map[int]*workBuddyToolCall
}

// WorkBuddyAggregate collects an SSE completion for clients requesting JSON.
type WorkBuddyAggregate struct {
	id      string
	created int64
	usage   json.RawMessage
	choices map[int]*workBuddyChoice
}

func (a *WorkBuddyAggregate) Add(payload []byte) error {
	var frame struct {
		ID      string          `json:"id"`
		Created int64           `json:"created"`
		Usage   json.RawMessage `json:"usage"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content   string              `json:"content"`
				Reasoning string              `json:"reasoning_content"`
				ToolCalls []workBuddyToolCall `json:"tool_calls"`
			} `json:"delta"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return err
	}
	if a.choices == nil {
		a.choices = make(map[int]*workBuddyChoice)
	}
	if frame.ID != "" {
		a.id = frame.ID
	}
	if frame.Created != 0 {
		a.created = frame.Created
	}
	if len(frame.Usage) > 0 && string(frame.Usage) != "null" {
		a.usage = frame.Usage
	}
	for _, incoming := range frame.Choices {
		choice := a.choices[incoming.Index]
		if choice == nil {
			choice = &workBuddyChoice{tools: make(map[int]*workBuddyToolCall)}
			a.choices[incoming.Index] = choice
		}
		choice.content.WriteString(incoming.Delta.Content)
		choice.reasoning.WriteString(incoming.Delta.Reasoning)
		if incoming.Finish != "" {
			choice.finish = incoming.Finish
		}
		for _, fragment := range incoming.Delta.ToolCalls {
			call := choice.tools[fragment.Index]
			if call == nil {
				call = &workBuddyToolCall{}
				choice.tools[fragment.Index] = call
			}
			if fragment.ID != "" {
				call.ID = fragment.ID
			}
			if fragment.Type != "" {
				call.Type = fragment.Type
			}
			call.Function.Name += fragment.Function.Name
			call.Function.Arguments += fragment.Function.Arguments
		}
	}
	return nil
}

func (a *WorkBuddyAggregate) Response(model string) ([]byte, error) {
	indices := make([]int, 0, len(a.choices))
	for index := range a.choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	choices := make([]any, 0, len(indices))
	for _, index := range indices {
		choice := a.choices[index]
		message := map[string]any{"role": "assistant", "content": choice.content.String()}
		if choice.reasoning.Len() > 0 {
			message["reasoning_content"] = choice.reasoning.String()
		}
		if len(choice.tools) > 0 {
			if choice.content.Len() == 0 {
				message["content"] = nil
			}
			toolIndices := make([]int, 0, len(choice.tools))
			for toolIndex := range choice.tools {
				toolIndices = append(toolIndices, toolIndex)
			}
			sort.Ints(toolIndices)
			calls := make([]any, 0, len(toolIndices))
			for _, toolIndex := range toolIndices {
				call := choice.tools[toolIndex]
				calls = append(calls, map[string]any{"id": call.ID, "type": "function", "function": call.Function})
			}
			message["tool_calls"] = calls
		}
		choices = append(choices, map[string]any{"index": index, "message": message, "finish_reason": choice.finish})
	}
	response := map[string]any{"id": a.id, "object": "chat.completion", "created": a.created, "model": model, "choices": choices}
	if len(a.usage) > 0 {
		response["usage"] = a.usage
	}
	return json.Marshal(response)
}
