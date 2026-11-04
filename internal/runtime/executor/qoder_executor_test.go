package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type qoderTestTransport func(*http.Request) (*http.Response, error)

func (f qoderTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func qoderExecutorEnvelope(inner string) string {
	b, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	return "data: " + string(b) + "\n\n"
}

func qoderExecutorStream(tool bool) string {
	s := qoderExecutorEnvelope(`{"id":"c1","created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Think"}}]}`)
	if tool {
		s += qoderExecutorEnvelope(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"sea","arguments":""}}]}}]}`)
		s += qoderExecutorEnvelope(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"rch","arguments":"{\"q\":"}}]}}]}`)
		s += qoderExecutorEnvelope(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	} else {
		s += qoderExecutorEnvelope(`{"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":"stop"}]}`)
	}
	return s + qoderExecutorEnvelope(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"completion_tokens_details":{"cached_tokens":8,"reasoning_tokens":2}}}`) + qoderExecutorEnvelope("[DONE]")
}

func qoderDecodeTestBody(t *testing.T, body []byte) []byte {
	t.Helper()
	n, a := len(body), len(body)/3
	unrotated := append(append(append([]byte{}, body[n-a:]...), body[a:n-a]...), body[:a]...)
	decoded, err := base64.NewEncoding("_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!").WithPadding('$').DecodeString(string(unrotated))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func qoderTestAuth(provider string) *coreauth.Auth {
	auth := &coreauth.Auth{ID: "qoder-test-" + provider, Provider: provider}
	qoder.ApplyCredentials(auth, qoder.Credentials{UID: "uid-test", AccessToken: "access-test", RefreshToken: "refresh-test", OrganizationID: "org"})
	return auth
}

func TestQoderExecutorFourProtocols(t *testing.T) {
	t.Setenv("QD_COSY_UA", "")
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, test := range []struct {
			format                sdktranslator.Format
			body, terminal, cache string
		}{
			{sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}]}`, `finish_reason`, `cached_tokens`},
			{sdktranslator.FormatOpenAIResponse, `{"input":"hi","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}`, `response.completed`, `cached_tokens`},
			{sdktranslator.FormatClaude, `{"max_tokens":128,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"search","input_schema":{"type":"object"}}]}`, `message_stop`, `cache_read_input_tokens`},
			{sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"search","parameters":{"type":"object"}}]}]}`, `STOP`, `cachedContentTokenCount`},
		} {
			for _, tool := range []bool{false, true} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/tool=%t/stream=%t", provider, test.format, tool, stream), func(t *testing.T) {
						auth := qoderTestAuth(provider)
						ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderTestTransport(func(r *http.Request) (*http.Response, error) {
							if r.URL.Path != "/algo/api/v2/service/pro/sse/agent_chat_generation" || r.URL.Query().Get("Encode") != "1" || r.Header.Get("User-Agent") != "undici" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
								t.Errorf("invalid gateway request: %s", r.URL.Path)
							}
							encoded, _ := io.ReadAll(r.Body)
							body := qoderDecodeTestBody(t, encoded)
							if gjson.GetBytes(body, "messages.1.content").String() != "hi" || gjson.GetBytes(body, "tools.0.function.name").String() != "search" || gjson.GetBytes(body, "parameters.reasoning_effort").String() != "high" {
								t.Errorf("translation/thinking failed: %s", body)
							}
							if r.Header.Get("X-Model-Key") != gjson.GetBytes(body, "model_config.key").String() {
								t.Error("model routing differs from body")
							}
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Cosy-Info": {"secret-echo"}, "Authorization": {"secret-echo"}}, Body: io.NopCloser(strings.NewReader(qoderExecutorStream(tool)))}, nil
						}))
						e := NewQoderExecutor(&config.Config{}, provider)
						req := cliproxyexecutor.Request{Model: "qoder-test-model(high)", Payload: []byte(test.body)}
						opts := cliproxyexecutor.Options{SourceFormat: test.format, OriginalRequest: req.Payload, Stream: stream}
						var output []byte
						if stream {
							result, err := e.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							if result.Headers.Get("Cosy-Info") != "" || result.Headers.Get("Authorization") != "" {
								t.Fatal("credentials leaked through response headers")
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								output = append(output, chunk.Payload...)
							}
							if !bytes.Contains(output, []byte(test.terminal)) {
								t.Fatalf("terminal missing: %s", output)
							}
						} else {
							result, err := e.Execute(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							output = result.Payload
						}
						for _, marker := range []string{"Think", test.cache} {
							if !bytes.Contains(output, []byte(marker)) {
								t.Fatalf("%s missing: %s", marker, output)
							}
						}
						if tool {
							if !bytes.Contains(output, []byte("search")) || !bytes.Contains(output, []byte("hi")) {
								t.Fatalf("tool lost: %s", output)
							}
						} else if !bytes.Contains(output, []byte("Hello")) {
							t.Fatalf("text lost: %s", output)
						}
						if bytes.Contains(output, []byte("statusCodeValue")) || bytes.Contains(output, []byte("secret-echo")) {
							t.Fatal("outer envelope leaked")
						}
					})
				}
			}
		}
	}
}

func TestQoderGatewayFallbackBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, provider, gateway string
		mode                    string
		want                    []string
		success                 bool
	}{
		{"pre-send", qoder.ProviderIntl, "https://api3.qoder.sh", "transport", []string{"api3.qoder.sh", "api2.qoder.sh", "api1.qoder.sh"}, true},
		{"bounded", qoder.ProviderIntl, "https://api3.qoder.sh", "all-fail", []string{"api3.qoder.sh", "api2.qoder.sh", "api1.qoder.sh"}, false},
		{"sent-body", qoder.ProviderIntl, "https://api3.qoder.sh", "sent", []string{"api3.qoder.sh"}, false},
		{"wrote-request", qoder.ProviderIntl, "https://api3.qoder.sh", "wrote", []string{"api3.qoder.sh"}, false},
		{"first-response-byte", qoder.ProviderIntl, "https://api3.qoder.sh", "responding", []string{"api3.qoder.sh"}, false},
		{"http403", qoder.ProviderIntl, "https://api3.qoder.sh", "403", []string{"api3.qoder.sh"}, false},
		{"business403", qoder.ProviderIntl, "https://api3.qoder.sh", "business", []string{"api3.qoder.sh"}, false},
		{"stream-eof", qoder.ProviderIntl, "https://api3.qoder.sh", "truncated", []string{"api3.qoder.sh"}, false},
		{"non-transient", qoder.ProviderIntl, "https://api3.qoder.sh", "other-error", []string{"api3.qoder.sh"}, false},
		{"custom", qoder.ProviderIntl, "https://custom.example", "transport", []string{"custom.example"}, false},
		{"cn", qoder.ProviderCN, "https://gateway.qoder.com.cn", "transport", []string{"gateway.qoder.com.cn"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := qoderTestAuth(test.provider)
			auth.Metadata["gateway_url"] = test.gateway
			var visited []string
			var signed, path string
			var encoded []byte
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderTestTransport(func(r *http.Request) (*http.Response, error) {
				visited = append(visited, r.URL.Host)
				copyBody, err := r.GetBody()
				if err != nil {
					return nil, err
				}
				copyBytes, err := io.ReadAll(copyBody)
				if errClose := copyBody.Close(); err == nil {
					err = errClose
				}
				if err != nil {
					return nil, err
				}
				if len(visited) == 1 {
					signed = r.Header.Get("Authorization")
					encoded = copyBytes
					path = r.URL.RequestURI()
				} else if signed != r.Header.Get("Authorization") || !bytes.Equal(encoded, copyBytes) || path != r.URL.RequestURI() {
					t.Fatal("fallback changed the signed body/path")
				}
				switch test.mode {
				case "403":
					return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":10605}`))}, nil
				case "business":
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(`data: {"statusCodeValue":403,"body":"rejected"}` + "\n\n"))}, nil
				case "truncated":
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(qoderExecutorEnvelope(`{"choices":[{"delta":{"content":"partial"}}]}`)))}, nil
				case "other-error":
					return nil, errors.New("invalid transport configuration")
				case "sent":
					_, _ = r.Body.Read(make([]byte, 1))
				case "wrote":
					httptrace.ContextClientTrace(r.Context()).WroteRequest(httptrace.WroteRequestInfo{})
				case "responding":
					httptrace.ContextClientTrace(r.Context()).GotFirstResponseByte()
				}
				if r.URL.Host == "api1.qoder.sh" && test.mode == "transport" {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(qoderExecutorStream(false)))}, nil
				}
				if errClose := r.Body.Close(); errClose != nil {
					return nil, errClose
				}
				return nil, io.ErrUnexpectedEOF
			}))
			_, err := NewQoderExecutor(&config.Config{}, test.provider).Execute(ctx, auth, cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
			if (err == nil) != test.success || !reflect.DeepEqual(visited, test.want) {
				t.Fatalf("visited=%v error=%v", visited, err)
			}
		})
	}
}

func TestQoderGatewayFallbackCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	count := 0
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", qoderTestTransport(func(r *http.Request) (*http.Response, error) {
		count++
		cancel()
		if errClose := r.Body.Close(); errClose != nil {
			return nil, errClose
		}
		return nil, io.ErrUnexpectedEOF
	}))
	_, err := NewQoderExecutor(&config.Config{}, qoder.ProviderIntl).Execute(ctx, qoderTestAuth(qoder.ProviderIntl), cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("cancel error=%v attempts=%d", err, count)
	}
}

func TestQoderExecutorErrorsAndCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, test := range []struct {
			status               int
			contentType, payload string
			want                 int
		}{
			{200, "application/json", `{"code":112,"message":"quota"}`, 429},
			{200, "application/json", `{"statusCodeValue":401,"body":"expired"}`, 401},
			{401, "text/plain", "expired", 401},
			{403, "application/json", `{"code":10605,"isQueued":true,"retryAfterSeconds":30}`, 429},
			{200, "text/event-stream", qoderExecutorEnvelope(`{"error":{"code":110}}`), 429},
			{200, "text/event-stream", qoderExecutorEnvelope(`{"choices":[{"delta":{"content":"partial"}}]}`), 502},
		} {
			t.Run(fmt.Sprintf("%d/%t/%s", test.status, stream, test.payload), func(t *testing.T) {
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderTestTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": {test.contentType}}, Body: io.NopCloser(strings.NewReader(test.payload))}, nil
				}))
				e := NewQoderExecutor(&config.Config{}, qoder.ProviderIntl)
				req := cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
				var err error
				if stream {
					var result *cliproxyexecutor.StreamResult
					result, err = e.ExecuteStream(ctx, qoderTestAuth(qoder.ProviderIntl), req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = e.Execute(ctx, qoderTestAuth(qoder.ProviderIntl), req, opts)
				}
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != test.want {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", qoderTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: r}, nil
	}))
	result, err := NewQoderExecutor(&config.Config{}, qoder.ProviderIntl).ExecuteStream(ctx, qoderTestAuth(qoder.ProviderIntl), cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range result.Chunks {
	}
}

func TestQoderHTTPRequestPurposes(t *testing.T) {
	auth := qoderTestAuth(qoder.ProviderIntl)
	e := NewQoderExecutor(&config.Config{}, qoder.ProviderIntl)
	for _, test := range []struct {
		url, authorization string
		cosy               bool
	}{
		{qoder.EndpointForAuth(auth) + "/api/v1/user", "Bearer access-test", false},
		{qoder.GatewayForAuth(auth) + "/algo/api/v2/model/list?Encode=1", "Bearer COSY.", true},
	} {
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderTestTransport(func(r *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), test.authorization) || (r.Header.Get("Cosy-Key") != "") != test.cosy {
				t.Error("ordinary API used Cosy credentials or gateway missed signing")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}))
		req, _ := http.NewRequest(http.MethodGet, test.url, nil)
		res, err := e.HttpRequest(ctx, auth, req)
		if err != nil {
			t.Fatal(err)
		}
		if errClose := res.Body.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("mutated caller request")
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "https://unrelated.invalid/algo/api/v2/model/list", nil)
	if _, err := e.HttpRequest(t.Context(), auth, req); err == nil {
		t.Fatal("sent credentials to an unrelated endpoint")
	}
}

func TestQoderSafeHeaders(t *testing.T) {
	headers := http.Header{"Cosy-Info": {"encrypted-credential"}, "Cosy-Key": {"encrypted-key"}, "Authorization": {"Bearer token"}, "Set-Cookie": {"token=secret"}, "X-Request-Id": {"id"}}
	headers["cosy-info"] = []string{"lowercase-secret"}
	safe := helps.QoderSafeHeaders(headers)
	if len(safe) != 1 || safe.Get("X-Request-Id") != "id" || len(headers) != 6 {
		t.Fatal("headers were not safely cloned and filtered")
	}
}

func TestQoderExecutorHistoryAndMultimodalConversions(t *testing.T) {
	for _, test := range []struct {
		format  sdktranslator.Format
		payload string
	}{
		{sdktranslator.FormatOpenAI, `{"messages":[{"role":"system","content":"system rule"},{"role":"user","content":"before"},{"role":"assistant","content":"","reasoning_content":"prior thought","tool_calls":[{"id":"call1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hi\"}"}}]},{"role":"tool","tool_call_id":"call1","content":"tool result"},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}]}`},
		{sdktranslator.FormatClaude, `{"system":"system rule","max_tokens":128,"messages":[{"role":"user","content":"before"},{"role":"assistant","content":[{"type":"thinking","thinking":"prior thought"},{"type":"tool_use","id":"call1","name":"search","input":{"q":"hi"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"tool result"}]},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}],"tools":[{"name":"search","input_schema":{"type":"object"}}]}`},
		{sdktranslator.FormatOpenAIResponse, `{"instructions":"system rule","input":[{"role":"user","content":"before"},{"type":"function_call","call_id":"call1","name":"search","arguments":"{\"q\":\"hi\"}"},{"type":"function_call_output","call_id":"call1","output":"tool result"},{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}],"tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}`},
		{sdktranslator.FormatGemini, `{"systemInstruction":{"parts":[{"text":"system rule"}]},"contents":[{"role":"user","parts":[{"text":"before"}]},{"role":"model","parts":[{"functionCall":{"id":"call1","name":"search","args":{"q":"hi"}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call1","name":"search","response":{"result":"tool result"}}}]},{"role":"user","parts":[{"text":"look"},{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}],"tools":[{"functionDeclarations":[{"name":"search","parameters":{"type":"object"}}]}]}`},
	} {
		t.Run(test.format.String(), func(t *testing.T) {
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderTestTransport(func(r *http.Request) (*http.Response, error) {
				encoded, _ := io.ReadAll(r.Body)
				body := qoderDecodeTestBody(t, encoded)
				for _, marker := range []string{"system rule", "tool result", "data:image/png;base64,AA==", `"tool_call_id"`, `"tool_calls"`, `"search"`} {
					if !bytes.Contains(body, []byte(marker)) {
						t.Errorf("history/image %s missing: %s", marker, body)
					}
				}
				if gjson.GetBytes(body, "chat_context.text.text").String() != "look" {
					t.Errorf("latest prompt was lost: %s", body)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(qoderExecutorStream(false)))}, nil
			}))
			e := NewQoderExecutor(&config.Config{}, qoder.ProviderCN)
			_, err := e.Execute(ctx, qoderTestAuth(qoder.ProviderCN), cliproxyexecutor.Request{Model: "dfmodel", Payload: []byte(test.payload)}, cliproxyexecutor.Options{SourceFormat: test.format})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQoderRefreshAndProviderIsolation(t *testing.T) {
	auth := qoderTestAuth(qoder.ProviderIntl)
	before, _ := json.Marshal(auth.Metadata)
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/jobToken/refresh" || r.Header.Get("Cosy-Key") != "" {
			t.Errorf("refresh used wrong protocol: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"token":"new-access","refresh_token":"new-refresh","expires_in":3600000}`))}, nil
	}))
	e := NewQoderExecutor(&config.Config{}, qoder.ProviderIntl)
	updated, err := e.Refresh(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(auth.Metadata)
	if !bytes.Equal(before, after) || updated == auth || updated.LastRefreshedAt.IsZero() || qoder.CredentialsFromAuth(updated).AccessToken != "new-access" || qoder.CredentialsFromAuth(updated).OrganizationID != "org" {
		t.Fatal("refresh mutated original or lost identity")
	}
	if _, err := e.Refresh(ctx, qoderTestAuth(qoder.ProviderCN)); err == nil {
		t.Fatal("refreshed another provider's credential")
	}
	_, err = e.Execute(ctx, qoderTestAuth(qoder.ProviderCN), cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("executed with another provider's credential")
	}
}

func TestQoderNonStreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	started := make(chan struct{})
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", qoderTestTransport(func(*http.Request) (*http.Response, error) {
		close(started)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
	}))
	finished := make(chan error, 1)
	go func() {
		_, err := NewQoderExecutor(&config.Config{}, qoder.ProviderIntl).Execute(ctx, qoderTestAuth(qoder.ProviderIntl), cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
		finished <- err
	}()
	<-started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("nonstream cancellation: %v", err)
	}
}
