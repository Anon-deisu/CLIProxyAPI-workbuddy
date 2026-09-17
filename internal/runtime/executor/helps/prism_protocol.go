package helps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const prismToolProtocol = `Act as a protocol adapter for the external client whose complete request follows.
Honor the instructions and conversation in that request. Its function tools execute at the client, not in your sandbox.
Respond only to the latest unfinished client turn. Earlier assistant messages and completed tool calls are history, not new requests to repeat.
Do not invoke sandbox tools, inspect the workspace, or invent client tool results.
Return exactly one JSON object, without Markdown, with keys "content" (string) and "tool_calls" (array).
To request a declared function, return a tool_calls entry with "name" and "arguments" (a JSON object).
To request a declared custom tool, return "name" and "input" (a string conforming to its declared format) instead of arguments.
Tools with any other type are hosted capabilities requested from the upstream service. Use a matching native capability when available and return its final result as content; never expose a hosted capability as a client tool call and never claim it ran when unavailable.
If tools are needed, do not invent their results. The client will execute them and send function_call_output items in its next complete request.
Use existing function_call_output items as the tool results, preserving the original user instructions.
Honor tool_choice and parallel_tool_calls. With tool_choice none, do not request any tools.
For a final answer put the answer in content and return an empty tool_calls array.
Only tools explicitly declared in the client request are available. Any requested output format applies to the content string. Your outer JSON protocol remains unchanged.`

func PrismReasoningEffort(effort string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "max":
		return "xhigh", nil
	case "", "auto":
		return "medium", nil
	case "none", "minimal":
		return "low", nil
	case "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(effort)), nil
	default:
		return "", &PrismHTTPError{Code: http.StatusBadRequest, Message: "unsupported Prism reasoning effort"}
	}
}

func BuildPrismInput(body []byte) (json.RawMessage, error) {
	message := func(role, text string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": []map[string]string{{"type": "input_text", "text": text}}}
	}
	return json.Marshal([]map[string]any{message("system", prismToolProtocol), message("user", "Complete client request:\n"+string(body))})
}

// PrismInputTokens counts the text sent by this adapter, excluding unknown sandbox system context.
func PrismInputTokens(request []byte, model string) (int64, error) {
	input, err := BuildPrismInput(request)
	if err != nil {
		return 0, err
	}
	var parts []string
	for _, message := range gjson.ParseBytes(input).Array() {
		for _, part := range message.Get("content").Array() {
			parts = append(parts, part.Get("text").String())
		}
	}
	codec, errCodec := TokenizerForModel(model)
	if errCodec != nil {
		return 0, errCodec
	}
	count, errCount := codec.Count(strings.Join(parts, "\n"))
	return int64(count), errCount
}

func prismUsage(payload, request []byte, model, generated string) (json.RawMessage, error) {
	reported := gjson.GetBytes(payload, "usage")
	if reported.Get("input_tokens").Exists() && reported.Get("output_tokens").Exists() {
		return json.RawMessage(reported.Raw), nil
	}
	input, err := PrismInputTokens(request, model)
	if err != nil {
		return nil, fmt.Errorf("count Prism input tokens: %w", err)
	}
	codec, errCodec := TokenizerForModel(model)
	if errCodec != nil {
		return nil, errCodec
	}
	output, errCount := codec.Count(generated)
	if errCount != nil {
		return nil, fmt.Errorf("count Prism output tokens: %w", errCount)
	}
	// Prism currently omits usage. Apply the configured local billing assumption
	// consistently to downstream responses and CPA's internal cost ledger.
	cached := input * 95 / 100
	return json.Marshal(map[string]any{
		"input_tokens":          input,
		"output_tokens":         output,
		"total_tokens":          input + int64(output),
		"estimated":             true,
		"input_tokens_details":  map[string]int64{"cached_tokens": cached},
		"output_tokens_details": map[string]int64{"reasoning_tokens": 0},
	})
}

func PrismResponse(payload, request []byte, model, responseID string) ([]byte, error) {
	var text strings.Builder
	for _, item := range gjson.GetBytes(payload, "output").Array() {
		if item.Get("type").String() != "message" {
			continue
		}
		for _, content := range item.Get("content").Array() {
			if content.Get("type").String() == "output_text" {
				text.WriteString(content.Get("text").String())
			}
		}
	}
	var envelope struct {
		Content   *string `json:"content"`
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Input     *string         `json:"input"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(text.String()), &envelope); err != nil || envelope.Content == nil || envelope.ToolCalls == nil {
		return nil, &PrismHTTPError{Code: http.StatusBadGateway, Message: "Prism returned an invalid client tool protocol response"}
	}
	declared := make(map[string]string)
	hasHostedTools := false
	for _, tool := range gjson.GetBytes(request, "tools").Array() {
		kind := tool.Get("type").String()
		if kind == "function" || kind == "custom" {
			declared[tool.Get("name").String()] = kind
		} else {
			hasHostedTools = true
		}
	}
	choice := gjson.GetBytes(request, "tool_choice")
	if choice.String() == "none" && len(envelope.ToolCalls) != 0 {
		return nil, &PrismHTTPError{Code: http.StatusBadGateway, Message: "Prism did not honor tool_choice none"}
	}
	namedChoice := choice.Get("type").String() == "function" || choice.Get("type").String() == "custom"
	if ((choice.String() == "required" && !hasHostedTools) || namedChoice) && len(envelope.ToolCalls) == 0 {
		return nil, &PrismHTTPError{Code: http.StatusBadGateway, Message: "Prism did not return the required client tool call"}
	}
	if parallel := gjson.GetBytes(request, "parallel_tool_calls"); parallel.Exists() && !parallel.Bool() && len(envelope.ToolCalls) > 1 {
		return nil, &PrismHTTPError{Code: http.StatusBadGateway, Message: "Prism returned parallel calls when disabled"}
	}
	output := make([]map[string]any, 0, len(envelope.ToolCalls)+1)
	if *envelope.Content != "" || len(envelope.ToolCalls) == 0 {
		output = append(output, map[string]any{"id": "msg_" + uuid.NewString(), "type": "message", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": *envelope.Content, "annotations": []any{}}}})
	}
	for _, call := range envelope.ToolCalls {
		kind := declared[call.Name]
		if kind == "" || (kind == "function" && !gjson.ParseBytes(call.Arguments).IsObject()) || (kind == "custom" && call.Input == nil) {
			return nil, &PrismHTTPError{Code: http.StatusBadGateway, Message: "Prism returned an undeclared tool or invalid arguments"}
		}
		if namedChoice && choice.Get("name").String() != call.Name {
			return nil, &PrismHTTPError{Code: http.StatusBadGateway, Message: "Prism returned a different tool than requested"}
		}
		item := map[string]any{"id": "fc_" + uuid.NewString(), "type": "function_call", "call_id": "call_" + uuid.NewString(), "name": call.Name, "arguments": string(call.Arguments), "status": "completed"}
		if kind == "custom" {
			delete(item, "arguments")
			item["type"], item["input"] = "custom_tool_call", *call.Input
		}
		output = append(output, item)
	}
	usage, errUsage := prismUsage(payload, request, model, text.String())
	if errUsage != nil {
		return nil, errUsage
	}
	response, errMarshal := json.Marshal(map[string]any{"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage, "error": nil, "incomplete_details": nil})
	if errMarshal != nil {
		return nil, errMarshal
	}
	return EnsureResponsesUsageDetails(response), nil
}

// PrismResponseEvents preserves Responses event ordering without inventing token streaming.
func PrismResponseEvents(response []byte) ([][]byte, error) {
	var object map[string]any
	if err := json.Unmarshal(response, &object); err != nil {
		return nil, err
	}
	var events [][]byte
	appendEvent := func(kind string, fields map[string]any) error {
		fields["type"] = kind
		fields["sequence_number"] = len(events) + 2
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		events = append(events, append(append([]byte("data: "), data...), []byte("\n\n")...))
		return nil
	}
	for index, value := range object["output"].([]any) {
		item := value.(map[string]any)
		initial := make(map[string]any, len(item))
		for k, v := range item {
			initial[k] = v
		}
		initial["status"] = "in_progress"
		if item["type"] == "function_call" {
			initial["arguments"] = ""
		} else if item["type"] == "custom_tool_call" {
			initial["input"] = ""
		} else {
			initial["content"] = []any{}
		}
		if err := appendEvent("response.output_item.added", map[string]any{"output_index": index, "item": initial}); err != nil {
			return nil, err
		}
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
			field, prefix := "arguments", "response.function_call_arguments"
			if item["type"] == "custom_tool_call" {
				field, prefix = "input", "response.custom_tool_call_input"
			}
			for _, kind := range []string{prefix + ".delta", prefix + ".done"} {
				fields := map[string]any{"item_id": item["id"], "output_index": index, field: item[field]}
				if strings.HasSuffix(kind, ".delta") {
					delete(fields, field)
					fields["delta"] = item[field]
				}
				if err := appendEvent(kind, fields); err != nil {
					return nil, err
				}
			}
		} else {
			part := item["content"].([]any)[0]
			text := part.(map[string]any)["text"]
			for _, step := range []struct {
				kind   string
				fields map[string]any
			}{
				{"response.content_part.added", map[string]any{"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}},
				{"response.output_text.delta", map[string]any{"delta": text}},
				{"response.output_text.done", map[string]any{"text": text}},
				{"response.content_part.done", map[string]any{"part": part}},
			} {
				step.fields["item_id"], step.fields["output_index"], step.fields["content_index"] = item["id"], index, 0
				if err := appendEvent(step.kind, step.fields); err != nil {
					return nil, err
				}
			}
		}
		if err := appendEvent("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
			return nil, err
		}
	}
	if err := appendEvent("response.completed", map[string]any{"response": object}); err != nil {
		return nil, fmt.Errorf("encode Prism completion: %w", err)
	}
	return events, nil
}
