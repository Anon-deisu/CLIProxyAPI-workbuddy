package executor

import (
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCursorReasoningDeltaTranslatesImmediately(t *testing.T) {
	request := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
	line := []byte(`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","model":"gpt-5.6-sol","choices":[{"index":0,"delta":{"reasoning_content":"thinking now"},"finish_reason":null}]}`)

	for _, format := range []sdktranslator.Format{
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
	} {
		var param any
		chunks := sdktranslator.TranslateStream(
			context.Background(),
			sdktranslator.FormatOpenAI,
			format,
			"gpt-5.6-sol",
			request,
			request,
			line,
			&param,
		)
		if len(chunks) == 0 {
			t.Fatalf("%s translator buffered the reasoning delta", format)
		}
		joined := ""
		for _, chunk := range chunks {
			joined += string(chunk)
		}
		if !strings.Contains(joined, "thinking now") {
			t.Fatalf("%s translator lost reasoning text: %s", format, joined)
		}
	}
}
