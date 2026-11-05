package executor

import (
	"fmt"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestTranslateCursorRequestConvertsResponsesInputDirectly(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.6-sol",
		"instructions":"Use tools precisely.",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"read it"}]},
			{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"tools":[{"type":"function","name":"Read","description":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"parallel_tool_calls":true
	}`)

	out, errTranslate := translateCursorRequest(sdktranslator.FormatOpenAIResponse, "gpt-5.6-sol", payload, true)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	if !gjson.GetBytes(out, "messages").IsArray() {
		t.Fatalf("Responses input was not converted: %s", out)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "system" {
		t.Fatalf("instructions role = %q, want system; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.2.tool_calls.0.id").String(); got != "call_1" {
		t.Fatalf("function call id = %q; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.3.tool_call_id").String(); got != "call_1" {
		t.Fatalf("function output id = %q; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "Read" {
		t.Fatalf("tool name = %q; body=%s", got, out)
	}
	if !gjson.GetBytes(out, "parallel_tool_calls").Bool() {
		t.Fatalf("parallel_tool_calls was lost: %s", out)
	}
}

func TestTranslateCursorRequestPreservesTypedAndCustomTools(t *testing.T) {
	payload := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"role":"user","content":"continue"},
			{"type":"shell_call","call_id":"call_shell","action":{"commands":["pwd"]}},
			{"type":"shell_call_output","call_id":"call_shell","output":[{"stdout":"ok"}]},
			{"type":"custom_tool_call","call_id":"call_custom","name":"render","input":"raw text"},
			{"type":"custom_tool_call_output","call_id":"call_custom","output":"done"}
		],
		"tools":[
			{"type":"shell"},
			{"type":"local_shell"},
			{"type":"apply_patch"},
			{"type":"custom","name":"render","description":"render raw text"}
		]
	}`)

	out, errTranslate := translateCursorRequest(sdktranslator.FormatOpenAIResponse, "gpt-5.6-sol", payload, true)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	if got := gjson.GetBytes(out, "tools.#").Int(); got != 4 {
		t.Fatalf("typed tools were lost: count=%d body=%s", got, out)
	}
	for index, name := range []string{"shell", "local_shell", "apply_patch", "render"} {
		if got := gjson.GetBytes(out, fmt.Sprintf("tools.%d.function.name", index)).String(); got != name {
			t.Fatalf("tool %d name=%q want=%q body=%s", index, got, name, out)
		}
	}
	if got := gjson.GetBytes(out, "messages.1.tool_calls.0.function.name").String(); got != "shell" {
		t.Fatalf("shell call was not structured: %s", out)
	}
	if got := gjson.GetBytes(out, "messages.3.tool_calls.0.function.arguments").String(); got != `{"input":"raw text"}` {
		t.Fatalf("custom raw input was not preserved: got=%q body=%s", got, out)
	}
}

func TestExtractCursorToolsPreservesResponsesKinds(t *testing.T) {
	original := []byte(`{
		"tools":[
			{"type":"shell"},
			{"type":"custom","name":"Read"},
			{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"Read"}]}
		]
	}`)
	translated, errTranslate := translateCursorRequest(sdktranslator.FormatOpenAIResponse, "gpt-5.6-sol", append([]byte(nil), original...), true)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	tools := extractCursorTools(translated, original, sdktranslator.FormatOpenAIResponse)
	if len(tools) != 3 {
		t.Fatalf("tools=%#v translated=%s", tools, translated)
	}
	if tools[0].Kind != "shell" || tools[1].Kind != "custom" || tools[2].Namespace != "codex_app" {
		t.Fatalf("tool metadata=%#v", tools)
	}
	if got := cursorlibTestCustomMcpCount(tools); got != 2 {
		t.Fatalf("custom MCP count=%d want=2 tools=%#v", got, tools)
	}
}

func cursorlibTestCustomMcpCount(tools []cursorlib.ToolDefinition) int {
	count := 0
	for _, tool := range tools {
		if tool.Kind == "custom" || tool.Namespace != "" {
			count++
		}
	}
	return count
}

func TestExtractCursorChatMessagesPreservesResponsesHistoryKinds(t *testing.T) {
	original := []byte(`{
		"input":[
			{"type":"apply_patch_call","call_id":"call_patch","operation":{"type":"delete_file","path":"old.txt"}},
			{"type":"apply_patch_call_output","call_id":"call_patch","output":"done"},
			{"type":"custom_tool_call","call_id":"call_custom","name":"render","namespace":"studio","input":"raw"},
			{"type":"custom_tool_call_output","call_id":"call_custom","output":"ok"}
		]
	}`)
	translated, errTranslate := translateCursorRequest(sdktranslator.FormatOpenAIResponse, "model", original, true)
	if errTranslate != nil {
		t.Fatal(errTranslate)
	}
	messages, err := extractCursorChatMessages(translated, original, sdktranslator.FormatOpenAIResponse)
	if err != nil {
		t.Fatal(err)
	}
	foundPatch := false
	foundCustom := false
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.ID == "call_patch" {
				foundPatch = call.Kind == "apply_patch"
			}
			if call.ID == "call_custom" {
				foundCustom = call.Kind == "custom" && call.Namespace == "studio"
			}
		}
		if message.Role == "tool" && message.ToolCallID == "call_patch" && message.ToolKind != "apply_patch" {
			t.Fatalf("patch result kind=%q", message.ToolKind)
		}
	}
	if !foundPatch || !foundCustom {
		t.Fatalf("history kinds lost: %#v", messages)
	}
}
