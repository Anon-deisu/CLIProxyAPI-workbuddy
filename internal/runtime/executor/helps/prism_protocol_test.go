package helps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPrismProtocolFunctionCall(t *testing.T) {
	request := []byte(`{"input":[],"tools":[{"type":"function","name":"add","parameters":{"type":"object"}}]}`)
	payload, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": `{"content":"","tool_calls":[{"name":"add","arguments":{"a":17,"b":25}}]}`}}}}})
	response, err := PrismResponse(payload, request, "gpt-6-astra", "resp-test")
	if err != nil {
		t.Fatal(err)
	}
	call := gjson.GetBytes(response, "output.0")
	if call.Get("type").String() != "function_call" || call.Get("name").String() != "add" || call.Get("call_id").String() == "" || !gjson.Valid(call.Get("arguments").String()) {
		t.Fatalf("invalid client tool call: %s", response)
	}
	events, err := PrismResponseEvents(response)
	if err != nil || !strings.Contains(string(events[len(events)-1]), "response.completed") {
		t.Fatalf("tool SSE did not complete: %v", err)
	}
	if _, err = PrismResponse(payload, []byte(`{"tools":[],"tool_choice":"none"}`), "gpt-6-astra", "resp-other"); err == nil {
		t.Fatal("undeclared tool was accepted")
	}
}
