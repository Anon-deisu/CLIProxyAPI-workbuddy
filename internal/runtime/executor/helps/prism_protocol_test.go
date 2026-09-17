package helps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	usage := gjson.GetBytes(response, "usage")
	if usage.Get("input_tokens").Int() <= 0 || usage.Get("output_tokens").Int() <= 0 || !usage.Get("estimated").Bool() {
		t.Fatalf("missing estimated tool usage: %s", response)
	}
	if usage.Get("total_tokens").Int() != usage.Get("input_tokens").Int()+usage.Get("output_tokens").Int() {
		t.Fatal("Prism usage totals do not match")
	}
	if got, want := usage.Get("input_tokens_details.cached_tokens").Int(), usage.Get("input_tokens").Int()*95/100; got != want {
		t.Fatalf("estimated cached tokens = %d, want %d", got, want)
	}
	events, err := PrismResponseEvents(response)
	if err != nil || !strings.Contains(string(events[len(events)-1]), "response.completed") {
		t.Fatalf("tool SSE did not complete: %v", err)
	}
	if _, err = PrismResponse(payload, []byte(`{"tools":[],"tool_choice":"none"}`), "gpt-6-astra", "resp-other"); err == nil {
		t.Fatal("undeclared tool was accepted")
	}
}

func TestPrismProtocolPreservesReportedUsage(t *testing.T) {
	const reported = `{"input_tokens":100,"output_tokens":23,"total_tokens":123,"input_tokens_details":{"cached_tokens":60},"output_tokens_details":{"reasoning_tokens":7}}`
	payload := []byte(`{"usage":` + reported + `,"output":[{"type":"message","content":[{"type":"output_text","text":"{\"content\":\"hello\",\"tool_calls\":[]}"}]}]}`)
	response, err := PrismResponse(payload, []byte(`{"input":"hello"}`), "gpt-6-astra", "resp-usage")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(response, "usage").Raw; got != reported {
		t.Fatalf("reported usage changed: %s", got)
	}
}

func TestPrismProtocolAcceptsHostedTools(t *testing.T) {
	request := []byte(`{"input":"find current docs","tools":[{"type":"web_search_preview"},{"type":"image_generation"}],"tool_choice":"required"}`)
	if _, err := BuildPrismInput(request); err != nil {
		t.Fatalf("hosted tools rejected: %v", err)
	}
	payload := []byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"content\":\"hosted result\",\"tool_calls\":[]}"}]}]}`)
	response, err := PrismResponse(payload, request, "gpt-6-astra", "resp-hosted")
	if err != nil {
		t.Fatalf("hosted final response rejected: %v", err)
	}
	if gjson.GetBytes(response, "output.0.content.0.text").String() != "hosted result" {
		t.Fatalf("unexpected hosted response: %s", response)
	}
}

func TestPrismCompletedRunDoesNotSendStop(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	run := &PrismRun{client: NewPrismClient(server.Client(), server.URL, "test"), turn: prismTurn{Status: "completed"}}
	if err := run.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("completed Prism generation sent another upstream stop request")
	}
}

func TestPrismGenerationErrorIncludesTraceWithoutCredentials(t *testing.T) {
	client := NewPrismClient(http.DefaultClient, "https://prism.example", "test")
	client.sandbox = prismSandbox{URL: "https://prism.example/s/sandboxes/proxy", Token: "sandbox-test"}
	run := &PrismRun{client: client, conversationID: "cdx1-test", sandboxToken: "sandbox-test", turn: prismTurn{Status: "completed", RequestID: "request-test", State: json.RawMessage(`{"sandbox_token":"private-token"}`)}}
	run.turn.Response.Status = "error"
	run.turn.Response.Payload = json.RawMessage(`{"reason":"unknown","message":"Error while processing conversation"}`)
	_, err := run.Wait(context.Background())
	if err == nil {
		t.Fatal("missing generation error")
	}
	for _, want := range []string{"Error while processing conversation", "request_id=request-test", "conversation_id=cdx1-test", "stage=start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("generation error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "private-token") {
		t.Fatal("generation error leaked turn credentials")
	}
	var prismErr *PrismHTTPError
	if !errors.As(err, &prismErr) || !prismErr.RefreshSandbox || prismErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("conversation processing failure is not recoverable: %#v", err)
	}
	if client.sandbox.Token != "" {
		t.Fatal("conversation processing failure retained the broken sandbox")
	}
}

func TestPrismRefreshesExpiredResourcesWithoutRecreatingSandbox(t *testing.T) {
	var acquisitions, resources, generations, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = map[string]any{"status": "success"}
		switch r.URL.Path {
		case "/auth/session":
			data = map[string]any{"userTier": "logged_in", "user": map[string]string{"id": "test-user"}}
		case "/api/backend/1/new":
			acquisitions.Add(1)
			data = map[string]string{"url": "https://prism.example/s/sandboxes/proxy", "token": "test-sandbox"}
		case "/api/projects/test-project/sandbox/resources-token":
			expiry := time.Now().Add(time.Hour).Unix()
			if resources.Add(1) == 1 {
				expiry = time.Now().Add(-time.Hour).Unix()
			}
			data = map[string]any{"access_token": "test-resource", "resources_base_url": "https://prism.example", "expires_at": expiry}
		case "/api/y":
			data = map[string]string{"docId": "test-project", "token": "test-document"}
		case "/s/sandboxes/proxy/wait-for-sync":
			data = map[string]string{"status": "synced"}
		case "/api/llm/response_with_tools_start":
			generations.Add(1)
			data = map[string]any{"status": "started", "request_id": "job-test", "turn_state": map[string]int{"cursor": 0}}
		case "/api/llm/response_with_tools_status":
			var body struct {
				RequestID string `json:"request_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RequestID != "job-test" {
				t.Error("poll did not retain the original generation ID")
			}
			data = map[string]any{"status": "pending", "request_id": "job-test", "turn_state": map[string]int{"cursor": 1}}
			if polls.Add(1) == 2 {
				data = map[string]any{"status": "completed", "request_id": "job-test", "response": map[string]any{"status": "success", "payload": map[string]any{"id": "result-test", "output": []any{}}}}
			}
		}
		if err := json.NewEncoder(w).Encode(data); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client := NewPrismClient(server.Client(), server.URL, "test")
	client.projectID = "test-project"
	for range 3 {
		if err := client.prepare(context.Background(), "test-at"); err != nil {
			t.Fatal(err)
		}
	}
	if resources.Load() != 2 || acquisitions.Load() != 1 {
		t.Fatalf("resource refreshes=%d sandbox acquisitions=%d, want 2 and 1", resources.Load(), acquisitions.Load())
	}
	client.pollInterval = 0
	run, errStart := client.Start(context.Background(), "test-at", "gpt-6-astra", "low", json.RawMessage(`[]`))
	if errStart != nil {
		t.Fatal(errStart)
	}
	client.resourcesExpireAt = time.Now().Add(-time.Hour)
	if _, errWait := run.Wait(context.Background()); errWait != nil {
		t.Fatal(errWait)
	}
	if resources.Load() != 3 || acquisitions.Load() != 1 || generations.Load() != 1 || polls.Load() != 2 {
		t.Fatalf("renewal replayed a generation: resources=%d sandbox=%d starts=%d polls=%d", resources.Load(), acquisitions.Load(), generations.Load(), polls.Load())
	}
}
