package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPrismExecutorProtocolsAndScopedMax(t *testing.T) {
	var conversations []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cookie, errCookie := r.Cookie("prism_oai_access_token")
		if errCookie != nil || cookie.Value != "test-at" || r.Header.Get("Authorization") != "" {
			t.Error("Prism request did not use its isolated AT cookie")
		}
		var response any = map[string]any{"status": "success"}
		switch r.URL.Path {
		case "/auth/session":
			response = map[string]any{"userTier": "logged_in", "user": map[string]any{"id": "prism-user", "is_anonymous": false}}
		case "/api/projects":
			if r.Method == http.MethodGet {
				response = map[string]any{"projects": []any{}}
			} else {
				response = map[string]string{"uuid": "probe-project"}
			}
		case "/api/backend/1/new":
			response = map[string]string{"url": server.URL + "/s/sandboxes/proxy", "token": "sandbox-token"}
		case "/api/projects/probe-project/sandbox/resources-token":
			response = map[string]any{"access_token": "resource-token", "resources_base_url": server.URL, "expires_at": time.Now().Add(time.Hour).Unix()}
		case "/api/y":
			response = map[string]string{"docId": "probe-project", "token": "document-token"}
		case "/s/sandboxes/proxy/wait-for-sync":
			response = map[string]string{"status": "synced"}
		case "/api/llm/response_with_tools_start":
			body, _ := io.ReadAll(r.Body)
			if got := gjson.GetBytes(body, "metadata.reasoning_effort").String(); got != "xhigh" {
				t.Errorf("Prism effort = %q, want xhigh", got)
			}
			conversations = append(conversations, gjson.GetBytes(body, "conversationId").String())
			response = map[string]any{"status": "completed", "request_id": "probe-request", "response": map[string]any{"status": "success", "payload": map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": `{"content":"PRISM_OK","tool_calls":[]}`}}}}}}}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Codex: config.CodexConfig{PrismBaseURL: server.URL}}
	e := NewPrismExecutor(cfg)
	a := &cliproxyauth.Auth{ID: "plus", Metadata: map[string]any{"access_token": "test-at"}}
	for _, tc := range []struct{ format, body, textPath string }{
		{"openai-response", `{"model":"gpt-6-astra","input":"hello","reasoning":{"effort":"max"}}`, "output.0.content.0.text"},
		{"openai", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"max"}`, "choices.0.message.content"},
		{"claude", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"output_config":{"effort":"max"}}`, "content.0.text"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.format), Metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "session", cliproxyexecutor.CallerScopeMetadataKey: "caller"}}
			req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(tc.body)}
			out, err := e.Execute(context.Background(), a, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(out.Payload, tc.textPath).String() != "PRISM_OK" {
				t.Fatalf("unexpected %s response: %s", tc.format, out.Payload)
			}
			if string(req.Payload) != tc.body {
				t.Fatal("Prism modified the original request")
			}
			usage := gjson.GetBytes(out.Payload, "usage")
			inputKey, outputKey := "input_tokens", "output_tokens"
			if tc.format == "openai" {
				inputKey, outputKey = "prompt_tokens", "completion_tokens"
			}
			if usage.Get(inputKey).Int() <= 0 || usage.Get(outputKey).Int() <= 0 || !usage.Get("estimated").Bool() {
				t.Fatalf("%s response missing usable token counts: %s", tc.format, out.Payload)
			}
			stream, errStream := e.ExecuteStream(context.Background(), a, req, opts)
			if errStream != nil {
				t.Fatal(errStream)
			}
			var transcript strings.Builder
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				transcript.Write(chunk.Payload)
				transcript.WriteByte('\n')
			}
			var text strings.Builder
			completed := 0
			var finalUsage gjson.Result
			for _, line := range strings.Split(transcript.String(), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "event:") || line == "" {
					continue
				}
				line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if line == "[DONE]" {
					continue
				}
				if !gjson.Valid(line) {
					t.Fatalf("invalid stream frame: %s", line)
				}
				frame := gjson.Parse(line)
				switch tc.format {
				case "openai-response":
					if frame.Get("type").String() != "response.completed" && frame.Get("response.usage.total_tokens").Int() > 0 {
						t.Fatal("Responses billed usage before the terminal event")
					}
					if frame.Get("type").String() == "response.output_text.delta" {
						text.WriteString(frame.Get("delta").String())
					}
					if frame.Get("type").String() == "response.completed" {
						completed++
						finalUsage = frame.Get("response.usage")
					}
				case "openai":
					text.WriteString(frame.Get("choices.0.delta.content").String())
					if frame.Get("choices.0.finish_reason").String() == "stop" {
						completed++
						finalUsage = frame.Get("usage")
					}
				case "claude":
					text.WriteString(frame.Get("delta.text").String())
					if frame.Get("type").String() == "message_start" && frame.Get("message.usage.input_tokens").Int() <= 0 {
						t.Fatal("Claude start frame has zero input tokens")
					}
					if frame.Get("type").String() == "message_delta" {
						finalUsage = frame.Get("usage")
					}
					if frame.Get("type").String() == "message_stop" {
						completed++
					}
				}
			}
			if completed != 1 || text.String() != "PRISM_OK" {
				t.Fatalf("%s stream repeated or failed to terminate: completions=%d text=%q", tc.format, completed, text.String())
			}
			if finalUsage.Get(inputKey).Int() != usage.Get(inputKey).Int() || finalUsage.Get(outputKey).Int() != usage.Get(outputKey).Int() || !finalUsage.Get("estimated").Bool() {
				t.Fatalf("%s stream usage differs from non-stream: %s vs %s", tc.format, finalUsage.Raw, usage.Raw)
			}
		})
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "session", cliproxyexecutor.CallerScopeMetadataKey: "caller"}}
	stream, err := e.ExecuteStream(context.Background(), a, cliproxyexecutor.Request{Model: "gpt-6-astra(max)", Payload: []byte(`{"input":"stream"}`)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), "response.completed") || !strings.Contains(output.String(), "PRISM_OK") {
		t.Fatalf("incomplete Prism stream: %s", output.String())
	}
	seen := make(map[string]bool)
	for _, id := range conversations {
		if id == "" || seen[id] {
			t.Fatal("two requests shared an upstream conversation")
		}
		seen[id] = true
	}
	native, err := thinking.ApplyThinkingWithModelInfo([]byte(`{"reasoning":{"effort":"max"}}`), nil, "custom(max)", "codex", "codex", "codex", &registry.ModelInfo{ID: "custom", UserDefined: true})
	if err != nil || gjson.GetBytes(native, "reasoning.effort").String() != "max" {
		t.Fatalf("native Codex max changed: %s, %v", native, err)
	}
}

func TestPrismExecutorConversationIsolation(t *testing.T) {
	e := NewPrismExecutor(&config.Config{})
	a := &cliproxyauth.Auth{ID: "account-a"}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller-a", cliproxyexecutor.CanonicalSessionIDMetadataKey: "conversation-a"}}
	first := e.client(context.Background(), a, opts)
	if first != e.client(context.Background(), a, opts) {
		t.Fatal("same scoped conversation did not reuse its workspace")
	}
	e.remember(opts, []byte(`{"input":[{"role":"user","content":"private-a"}]}`), []byte(`{"id":"resp-a","output":[]}`))
	for _, change := range []struct{ caller, session, account string }{{"caller-b", "conversation-a", "account-a"}, {"caller-a", "conversation-b", "account-a"}, {"caller-a", "conversation-a", "account-b"}} {
		other := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: change.caller, cliproxyexecutor.CanonicalSessionIDMetadataKey: change.session}}
		if first == e.client(context.Background(), &cliproxyauth.Auth{ID: change.account}, other) {
			t.Fatal("Prism workspace crossed an account, caller, or conversation boundary")
		}
		if change.account == "account-a" {
			_, _, err := e.prepare(cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"previous_response_id":"resp-a","input":"continue"}`)}, other)
			if err == nil {
				t.Fatal("another caller or conversation could access response history")
			}
		}
	}
}
