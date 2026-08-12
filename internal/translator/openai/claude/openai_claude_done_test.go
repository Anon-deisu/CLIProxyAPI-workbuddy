package claude

import (
	"context"
	"strings"
	"testing"
)

func TestBareDoneMarkerEmitsMessageStop(t *testing.T) {
	var param any
	chunks := [][]byte{
		[]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`),
		[]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		[]byte(`[DONE]`), // bare marker as emitted by some executors
	}
	var out []string
	for _, c := range chunks {
		for _, line := range ConvertOpenAIResponseToClaude(context.Background(), "m", []byte(`{"stream":true}`), nil, c, &param) {
			out = append(out, string(line))
		}
	}
	joined := strings.Join(out, "")
	if !strings.Contains(joined, "event: message_delta") {
		t.Fatalf("missing message_delta in %q", joined)
	}
	if !strings.Contains(joined, "event: message_stop") {
		t.Fatalf("missing message_stop in %q", joined)
	}
}
