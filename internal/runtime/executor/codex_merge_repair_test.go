package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexMergeFinalizerWinsOverIdentityAndNormalization(t *testing.T) {
	for _, mode := range []string{"responses", "compact", "image-responses", "direct-images", "websocket"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{
				Routing: config.RoutingConfig{SessionAffinity: true, CodexFingerprintConvergence: new(true)},
				Codex:   config.CodexConfig{IdentityConfuse: true},
				Payload: config.PayloadConfig{
					Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{
						"prompt_cache_key": "operator-cache", "client_metadata.session_id": "operator-session",
						"input.0.id": "operator-item", "audit.-1": "once",
					}}},
					Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{
						"client_metadata.x-codex-installation-id", "client_metadata.turn_id", "parallel_tool_calls",
					}}},
				},
			}
			auth := &cliproxyauth.Auth{ID: "merge-finalizer", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}}
			raw := []byte(`{"model":"gpt-5.4","prompt_cache_key":"client-cache","client_metadata":{"session_id":"client-session","x-codex-installation-id":"client-install"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
			req := core.Request{Model: "gpt-5.4", Payload: raw}
			opts := core.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			exec := NewCodexExecutor(cfg)
			var body []byte
			var state *codexIdentityConfuseState
			var headers http.Header
			if mode == "websocket" {
				prepared, errPrepare := NewCodexWebsocketsExecutor(cfg).prepareCodexWebsocketStream(t.Context(), auth, req, opts)
				if errPrepare != nil {
					t.Fatal(errPrepare)
				}
				body = frameCodexWebsocketRequestBody(prepared.clientBody)
				state, headers = &prepared.identityState, prepared.wsHeaders
			} else {
				doc := ojson.NewDoc(raw)
				url := "https://example.test/responses"
				if mode == "compact" {
					url += "/compact"
				}
				if mode == "image-responses" {
					var errPrepare error
					doc, errPrepare = exec.prepareCodexOpenAIImageDoc(t.Context(), auth, raw, req, opts, req.Model)
					if errPrepare != nil {
						t.Fatal(errPrepare)
					}
				}
				var httpReq *http.Request
				var errPrepare error
				if mode == "direct-images" {
					httpReq, body, _, errPrepare = exec.prepareDirectOpenAIImageRequest(t.Context(), auth, req, opts, raw, "application/json", req.Model, "https://example.test/images/generations")
				} else {
					ctx := helps.WithPayloadFinalizer(t.Context(), helps.NewPayloadFinalizer(cfg, "codex", req.Model, "codex", "", raw, req, opts))
					httpReq, body, errPrepare = exec.cacheHelperDoc(ctx, opts.SourceFormat, url, req, doc, auth)
				}
				if errPrepare != nil {
					t.Fatal(errPrepare)
				}
				state, headers = codexIdentityStateForRequest(httpReq), httpReq.Header
				wire, errRead := io.ReadAll(httpReq.Body)
				if errClose := httpReq.Body.Close(); errClose != nil {
					t.Fatal(errClose)
				}
				if errRead != nil || !bytes.Equal(wire, body) {
					t.Fatalf("wire differs from finalized payload: %v", errRead)
				}
			}
			if state == nil || !state.enabled {
				t.Fatal("request lost its identity restoration state")
			}
			applyCodexIdentityConfuseHeaders(headers, state)
			for path, want := range map[string]string{"prompt_cache_key": "operator-cache", "client_metadata.session_id": "operator-session", "input.0.id": "operator-item"} {
				if got := ojson.GetBytes(body, path).String(); got != want {
					t.Errorf("%s = %q, want %q", path, got, want)
				}
			}
			for _, path := range []string{"client_metadata.x-codex-installation-id", "client_metadata.turn_id", "parallel_tool_calls"} {
				if ojson.GetBytes(body, path).Exists() {
					t.Errorf("filtered field restored after finalization: %s", path)
				}
			}
			if got := ojson.GetBytes(body, "audit").Raw; got != `["once"]` {
				t.Fatalf("payload rules ran more than once: %s", got)
			}
			if got := codexSessionHeaderValue(headers); got != "operator-cache" {
				t.Fatalf("session header = %q, want finalized key", got)
			}
		})
	}
}

func TestCodexMergeFingerprintRestoresEachRequestWithoutChangingOutput(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: new(true)}}
	auth := &cliproxyauth.Auth{ID: "same-websocket-account", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}}
	var states []codexIdentityConfuseState
	var requests [][]byte
	for _, metadata := range []string{`{"session_id":"first","large":9007199254740993}`, `{"session_id":"second"}`, `null`} {
		raw := []byte(`{"prompt_cache_key":"explicit","client_metadata":` + metadata + `}`)
		body, state := applyCodexIdentityConfuseBody(cfg, auth, raw, raw)
		requests, states = append(requests, body), append(states, state)
	}
	for i, state := range states {
		event := ojson.NewDocString(`{"type":"response.completed","response":{"output":[]}}`)
		_ = event.SetRaw("response.client_metadata", ojson.GetBytes(requests[i], "client_metadata").Raw)
		_ = event.Set("response.output.0.text", state.fingerprint.SessionID)
		payload := []byte("event: response.completed\r\ndata: " + event.String() + "\r\n\r\n")
		restored := applyCodexIdentityExposeResponsePayload(payload, state)
		line := bytes.Split(restored, []byte("\r\n"))[1]
		root := ojson.ParseBytes(bytes.TrimPrefix(line, []byte("data: ")))
		want := []string{`{"session_id":"first","large":9007199254740993}`, `{"session_id":"second"}`, `null`}[i]
		if got := root.Get("response.client_metadata").Raw; got != want {
			t.Fatalf("request %d restored metadata %s, want %s", i, got, want)
		}
		if got := root.Get("response.output.0.text").String(); got != state.fingerprint.SessionID {
			t.Fatalf("generated output changed: %q", got)
		}
		if !strings.HasSuffix(string(restored), "\r\n\r\n") {
			t.Fatal("SSE framing changed")
		}
	}
}

func TestCodexMergeSessionReuseChecksSIWCProxyAndConvergence(t *testing.T) {
	conn := &websocket.Conn{}
	cfg := &config.Config{}
	sess := &codexWebsocketSession{
		conn: conn, connCloser: newWebsocketConnectionCloser(conn), authID: "account", wsURL: "ws://example.test/responses",
		proxyURL: "http://proxy.test:8080", useSIWC: true, convergence: true, egressFingerprint: "binding",
		connCfg: cfg, hasConnCfg: true,
	}
	sess.resetUpstreamDisconnectError(conn)
	for _, tc := range []struct {
		name, proxy, binding string
		siwc, convergence    bool
		want                 bool
	}{
		{"same", sess.proxyURL, "binding", true, true, true},
		{"siwc", sess.proxyURL, "binding", false, true, false},
		{"proxy", "direct", "binding", true, true, false},
		{"binding", sess.proxyURL, "new-binding", true, true, false},
		{"convergence", sess.proxyURL, "binding", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := existingWebsocketSessionConnForEgress(sess, sess.authID, sess.wsURL, tc.proxy, tc.convergence, tc.binding, tc.siwc)
			if (got == conn) != tc.want {
				t.Fatalf("reusable=%t, want %t", got == conn, tc.want)
			}
		})
	}
	if sess.effectiveConfig("account", nil) != cfg || sess.effectiveConfig("different", nil) != nil {
		t.Fatal("connection config escaped its account")
	}
	sess.detachConnection(conn, nil)
	if sess.effectiveConfig("account", nil) != nil || sess.hasConnCfg || sess.useSIWC || sess.convergence {
		t.Fatal("detached connection retained handshake state")
	}
}

func TestCodexMergeDirectImagesRestoreIdentityAcrossTransportReads(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Routing: config.RoutingConfig{SessionAffinity: true}, Codex: config.CodexConfig{IdentityConfuse: true}}
			auth := &cliproxyauth.Auth{ID: "image-identity", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}}
			req := core.Request{Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"a cat","prompt_cache_key":"client-cache","client_metadata":{"x-codex-installation-id":"client-install"}}`)}
			var upstreamKey string
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(request.Body)
				if errRead != nil {
					return nil, errRead
				}
				upstreamKey = ojson.GetBytes(body, "prompt_cache_key").String()
				if upstreamKey == "" || upstreamKey == "client-cache" {
					t.Errorf("identity confusion did not run: %s", body)
				}
				doc := ojson.NewDocString(`{"type":"image_generation.completed","created":1,"data":[{"b64_json":"AA=="}]}`)
				_ = doc.Set("prompt_cache_key", upstreamKey)
				_ = doc.SetRaw("client_metadata", ojson.GetBytes(body, "client_metadata").Raw)
				payload := doc.String()
				contentType := "application/json"
				if stream {
					payload = "event: image_generation.completed\r\ndata: " + payload + "\r\n\r\n"
					contentType = "text/event-stream"
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
					Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(payload))),
				}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
			exec := NewCodexExecutor(cfg)
			opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream)
			var output []byte
			if stream {
				result, errStream := exec.ExecuteStream(ctx, auth, req, opts)
				if errStream != nil {
					t.Fatal(errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					output = append(output, chunk.Payload...)
				}
			} else {
				result, errExecute := exec.Execute(ctx, auth, req, opts)
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				output = result.Payload
			}
			if !bytes.Contains(output, []byte(`"prompt_cache_key":"client-cache"`)) || !bytes.Contains(output, []byte(`"x-codex-installation-id":"client-install"`)) || bytes.Contains(output, []byte(upstreamKey)) {
				t.Fatalf("client identity was not restored: %s", output)
			}
		})
	}
}
