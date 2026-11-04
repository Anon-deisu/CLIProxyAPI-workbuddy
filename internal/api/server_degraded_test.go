package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type degradedTestExecutor struct {
	mu       sync.Mutex
	accounts []string
}

func (*degradedTestExecutor) Identifier() string { return "codex" }
func (e *degradedTestExecutor) Execute(_ context.Context, a *auth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.accounts = append(e.accounts, a.ID)
	e.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"id": a.ID, "object": "chat.completion", "choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": a.ID}, "finish_reason": "stop"}}})
	return coreexecutor.Response{Payload: body}, nil
}
func (e *degradedTestExecutor) ExecuteStream(_ context.Context, a *auth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.accounts = append(e.accounts, a.ID)
	e.mu.Unlock()
	body := `data: {"type":"response.completed","response":{"id":"resp_test","model":"degraded-pool-test","status":"completed","output":[]}}` + "\n\n"
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(body)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}
func (*degradedTestExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}
func (*degradedTestExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (*degradedTestExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unused")
}

func degradedTestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestDegradedListenerRestoresSavedPortOnStart(t *testing.T) {
	server := newTestServer(t)
	server.server.Addr = "127.0.0.1:0"
	server.degradedHost = "127.0.0.1"
	port := degradedTestPort(t)
	server.cfg.Routing.DegradedPort = &port
	ready := make(chan struct{})
	server.server.BaseContext = func(net.Listener) context.Context {
		close(ready)
		return context.Background()
	}
	done := make(chan error, 1)
	go func() { done <- server.Start() }()
	<-ready
	t.Cleanup(func() {
		if errStop := server.Stop(context.Background()); errStop != nil {
			t.Error(errStop)
		}
		if errStart := <-done; errStart != nil {
			t.Error(errStart)
		}
	})
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, errGet := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/v1/models")
	if errGet != nil {
		t.Fatal(errGet)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("restored listener returned %d without API key", response.StatusCode)
	}
	updated := server.cfg.CloneForRuntime()
	updated.Routing.DegradedPort = new(0)
	if !server.UpdateClientsContext(t.Context(), updated) || server.degradedPort != 0 {
		t.Fatal("config reload did not stop a disabled degraded listener")
	}
}

func TestDegradedListenerUsesSameKeyAndSeparateAccounts(t *testing.T) {
	server := newTestServer(t)
	server.degradedHost = "127.0.0.1"
	t.Cleanup(func() { _ = server.stopDegradedListener() })
	exec := &degradedTestExecutor{}
	server.handlers.AuthManager.RegisterExecutor(exec)
	const model = "degraded-pool-test"
	for _, id := range []string{"normal-account", "degraded-account"} {
		_, err := server.handlers.AuthManager.Register(t.Context(), &auth.Auth{ID: id, Provider: "codex", Status: auth.StatusActive, Metadata: map[string]any{"degraded": id == "degraded-account"}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	normal := httptest.NewServer(server.engine)
	defer normal.Close()
	port := degradedTestPort(t)
	if err := server.setDegradedPort(port); err != nil {
		t.Fatal(err)
	}
	degraded := "http://127.0.0.1:" + strconv.Itoa(port)
	for _, endpoint := range []struct{ base, want string }{{normal.URL, "normal-account"}, {degraded, "degraded-account"}} {
		for _, key := range []string{"wrong", "test-key"} {
			req, _ := http.NewRequest(http.MethodPost, endpoint.base+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if key == "wrong" {
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("wrong key: %d", resp.StatusCode)
				}
			} else if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), endpoint.want) {
				t.Fatalf("pool %s: %d %s", endpoint.want, resp.StatusCode, body)
			}
		}
	}
	for _, path := range []string{"/management.html", "/v0/management/auth-files", "/codex/callback", "/healthz", "/"} {
		resp, err := http.Get(degraded + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("non-model endpoint %s exposed: %d", path, resp.StatusCode)
		}
	}
	wsHeaders := http.Header{"Authorization": {"Bearer test-key"}}
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(degraded, "http://", "ws://", 1)+"/v1/responses", wsHeaders)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"`+model+`","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, body, err := conn.ReadMessage(); err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("websocket: %s %v", body, err)
	}
	exec.mu.Lock()
	lastAccount := exec.accounts[len(exec.accounts)-1]
	exec.mu.Unlock()
	if lastAccount != "degraded-account" {
		t.Fatal("websocket escaped the degraded pool")
	}
	degradedAuth, _ := server.handlers.AuthManager.GetByID("degraded-account")
	degradedAuth.Disabled = true
	if _, err := server.handlers.AuthManager.Update(t.Context(), degradedAuth); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, degraded+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("empty degraded pool used the normal account")
	}
}

func TestDegradedListenerPortSwitchRollbackAndClose(t *testing.T) {
	server := newTestServer(t)
	server.degradedHost = "127.0.0.1"
	t.Cleanup(func() { _ = server.stopDegradedListener() })
	first := degradedTestPort(t)
	if err := server.setDegradedPort(first); err != nil {
		t.Fatal(err)
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	if err := server.setDegradedPort(busy.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("accepted occupied port")
	}
	if server.degradedPort != first {
		t.Fatal("failed bind changed old port")
	}
	second := degradedTestPort(t)
	if err := server.setDegradedPort(second); err != nil {
		t.Fatal(err)
	}
	rebound, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(first))
	if err != nil {
		t.Fatalf("old port still occupied: %v", err)
	}
	_ = rebound.Close()
	if err := server.setDegradedPort(0); err != nil {
		t.Fatal(err)
	}
	rebound, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(second))
	if err != nil {
		t.Fatalf("disabled port still occupied: %v", err)
	}
	_ = rebound.Close()
	for _, invalid := range []int{-1, 65536} {
		if err := server.setDegradedPort(invalid); err == nil {
			t.Fatal("accepted invalid port")
		}
	}
}

func TestExoFreeListenerRoutesFirstIDAndRetries(t *testing.T) {
	var mu sync.Mutex
	var requests, sessions []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model         string `json:"model"`
			Stream        bool   `json:"stream"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name        string         `json:"name"`
					Description string         `json:"description"`
					Parameters  map[string]any `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
			t.Error(errDecode)
		}
		if body.Model != "exo-free" || !body.Stream || !body.StreamOptions.IncludeUsage {
			t.Errorf("upstream body = %+v", body)
		}
		toolNames := map[string]bool{}
		for _, tool := range body.Tools {
			if tool.Type != "function" || tool.Function.Parameters["type"] != "object" {
				t.Errorf("invalid tool definition: %+v", tool)
			}
			if tool.Function.Name == "bash" && tool.Function.Description != "client bash" {
				t.Errorf("client tool definition was replaced: %+v", tool.Function)
			}
			toolNames[tool.Function.Name] = true
		}
		if len(body.Tools) != 5 {
			t.Errorf("duplicated agent tools: %d", len(body.Tools))
		}
		for _, name := range []string{"bash", "edit", "glob", "grep", "read"} {
			if !toolNames[name] {
				t.Errorf("missing agent tool: %s", name)
			}
		}
		if got := r.Header.Get("Authorization"); got != "Bearer public" {
			t.Errorf("upstream authorization = %q", got)
		}
		if r.Header.Get("X-Opencode-Project") == "" {
			t.Error("missing OpenCode project header")
		}
		session := r.Header.Get("X-Opencode-Session")
		if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(session) || r.Header.Get("X-Session-Affinity") != session {
			t.Errorf("invalid OpenCode session headers: %q", session)
		}
		mu.Lock()
		requests = append(requests, r.Header.Get("X-Opencode-Request"))
		sessions = append(sessions, session)
		attempt := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": prelude\n\ndata: {"+`"usage":{"total_tokens":0}}`+"\n\n")
		if attempt == 1 {
			_, _ = fmt.Fprint(w, "data: {"+`"id":"msg_wrong","model":"exo-free","choices":[]}`+"\n\n")
			return
		}
		_, _ = fmt.Fprint(w, "data: {"+`"id":"resp_right","model":"exo-free","choices":[{"delta":{"content":"OK"}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	server := newTestServer(t)
	server.exoFreeHost = "127.0.0.1"
	server.exoFreeUpstreamURL = upstream.URL
	t.Cleanup(func() { _ = server.stopExoFreeListener() })
	port := degradedTestPort(t)
	if errPort := server.setExoFreePort(port); errPort != nil {
		t.Fatal(errPort)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for _, path := range []string{"/management.html", "/v0/management/routing/settings", "/v1/responses", "/v1/messages"} {
		resp, errGet := http.Get(base + path)
		if errGet != nil {
			t.Fatal(errGet)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unexpected route %s: %d", path, resp.StatusCode)
		}
	}
	request := func(key, model string) *http.Response {
		t.Helper()
		req, errNew := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}],"stream":true,"tools":[{"type":"function","function":{"name":"bash","description":"client bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`))
		if errNew != nil {
			t.Fatal(errNew)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, errDo := http.DefaultClient.Do(req)
		if errDo != nil {
			t.Fatal(errDo)
		}
		return resp
	}
	for _, tc := range []struct {
		key, model string
		want       int
	}{
		{"wrong", "gpt-6-sol", http.StatusUnauthorized},
		{"test-key", "gpt-paid-only", http.StatusBadRequest},
		{"test-key", "gpt-6-sol", http.StatusOK},
	} {
		resp := request(tc.key, tc.model)
		content, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("request %q: status %d body %s", tc.model, resp.StatusCode, content)
		}
		if tc.want == http.StatusOK && (!strings.Contains(string(content), `"model":"gpt-6-sol"`) || strings.Contains(string(content), "msg_wrong") || !strings.Contains(string(content), "resp_right")) {
			t.Errorf("response leaked rejected branch: %s", content)
		}
	}
	mu.Lock()
	if len(requests) != 2 || requests[0] == requests[1] || !strings.HasPrefix(requests[0], "req_") {
		t.Errorf("request IDs not regenerated: %v", requests)
	}
	if len(sessions) != 2 || sessions[0] == sessions[1] {
		t.Errorf("retry reused the same sticky session: %v", sessions)
	}
	mu.Unlock()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, errGet := http.DefaultClient.Do(req)
	if errGet != nil {
		t.Fatal(errGet)
	}
	content, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(content), "claude-opus-5-5") || strings.Contains(string(content), "gpt-6-sol") || !strings.Contains(string(content), "exo-free") {
		t.Errorf("model catalog = %d %s", resp.StatusCode, content)
	}
}

func TestOpenCodeFreeModelCatalogIsIndependent(t *testing.T) {
	server := newTestServer(t)
	server.cfg.Routing.FakeModels = []config.FakeModelConfig{{Name: "main-port-only", Content: "main reply"}}
	server.fakeModels.Store(config.CloneFakeModels(server.cfg.Routing.FakeModels))
	handler := server.exoFreeAPIHandler()
	for _, key := range []string{"wrong", "test-key"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if key == "wrong" {
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated model catalog: %d", rec.Code)
			}
			continue
		}
		var catalog struct {
			Object string `json:"object"`
			Data   []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &catalog); errDecode != nil {
			t.Fatal(errDecode)
		}
		want := map[string]bool{
			"big-pickle": true, "exo-free": true, "fledge-alpha-free": true,
			"space-bunny-free": true, "longcat-2.5-preview-free": true,
			"mimo-v2.6-flash-free": true, "ling-3.1-flash-free": true,
			"ling-3.0-flash-fin-free": true, "nemotron-3-ultra-free": true,
			"nemotron-3.5-lightning-free":     true,
			"muse-spark-1.3-contributor-free": true, "muse-spark-1.2-contributor-free": true,
		}
		if rec.Code != http.StatusOK || catalog.Object != "list" || len(catalog.Data) != len(want) {
			t.Fatalf("independent model catalog = %d %s", rec.Code, rec.Body.String())
		}
		for _, model := range catalog.Data {
			if !want[model.ID] || model.Object != "model" || model.OwnedBy != "opencode" {
				t.Errorf("unexpected model: %+v", model)
			}
			delete(want, model.ID)
		}
		if len(want) != 0 {
			t.Errorf("missing models: %v", want)
		}
	}
}

func TestOpenCodeFreeNativeStreams(t *testing.T) {
	for _, tc := range []struct {
		name, path, model, input, stream string
	}{
		{"chat", "chat/completions", "big-pickle", `"messages":[{"role":"user","content":"hello"}],"stream_options":null,"tools":[{"type":"function","function":{"name":"bash","description":"client bash","parameters":{"type":"object"}}}]`, ": prelude\n\ndata: {\"id\":\"opaque-id\",\"model\":\"big-pickle\",\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n"},
		{"responses", "responses", "muse-spark-1.3-contributor-free", `"input":[{"role":"user","content":"hello"}],"previous_response_id":"resp_previous","tools":[{"type":"function","name":"bash","description":"client bash","parameters":{"type":"object"}}]`, ": prelude\n\nevent: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\",\"model\":\"muse-spark-1.3-contributor-free\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"usage\":{\"total_tokens\":2}}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/"+tc.path || r.Header.Get("Authorization") != "Bearer public" {
					t.Errorf("upstream route or credential changed: %s", r.URL.Path)
				}
				var body map[string]json.RawMessage
				if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
					t.Error(errDecode)
				}
				if string(body["model"]) != strconv.Quote(tc.model) || string(body["stream"]) != "true" {
					t.Errorf("requested free model changed: %s", body["model"])
				}
				var tools []map[string]json.RawMessage
				if errDecode := json.Unmarshal(body["tools"], &tools); errDecode != nil || len(tools) != 5 {
					t.Errorf("agent tools: %v %s", errDecode, body["tools"])
				}
				for i, tool := range tools {
					definition := tool
					if tc.path == "chat/completions" {
						if errDecode := json.Unmarshal(tool["function"], &definition); errDecode != nil {
							t.Error(errDecode)
						}
					} else if tool["function"] != nil {
						t.Error("Responses tools were wrapped in Chat format")
					}
					if definition["name"] == nil || definition["parameters"] == nil {
						t.Errorf("invalid %s tool: %v", tc.path, tool)
					}
					if i == 0 && string(definition["description"]) != `"client bash"` {
						t.Error("client tool definition changed")
					}
				}
				if tc.path == "responses" {
					if body["input"] == nil || string(body["previous_response_id"]) != `"resp_previous"` || body["messages"] != nil || body["stream_options"] != nil {
						t.Errorf("Responses payload was converted: %v", body)
					}
				} else if body["messages"] == nil || !strings.Contains(string(body["stream_options"]), `"include_usage":true`) {
					t.Errorf("Chat payload: %v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			}))
			defer upstream.Close()
			server := newTestServer(t)
			server.exoFreeUpstreamURL = upstream.URL + "/chat/completions"
			server.exoFreeResponsesURL = upstream.URL + "/responses"
			req := httptest.NewRequest(http.MethodPost, "/v1/"+tc.path, strings.NewReader(`{"model":"`+tc.model+`","stream":true,`+tc.input+`}`))
			req.Header.Set("Authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.exoFreeAPIHandler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || calls != 1 || rec.Body.String() != tc.stream || rec.Header().Get("Content-Type") != "text/event-stream" || !rec.Flushed {
				t.Fatalf("native stream status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
		})
	}
}

func TestOpenCodeFreeRejectsUnsupportedRequests(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer upstream.Close()
	server := newTestServer(t)
	server.exoFreeUpstreamURL = upstream.URL
	server.exoFreeResponsesURL = upstream.URL
	handler := server.exoFreeAPIHandler()
	for _, tc := range []struct {
		path, body, key string
		want            int
	}{
		{"responses", `{"model":"muse-spark-1.3-contributor-free","stream":true}`, "wrong", http.StatusUnauthorized},
		{"responses", `{"model":"muse-spark-1.3-contributor-free","stream":false}`, "test-key", http.StatusBadRequest},
		{"responses", `{"model":"gpt-6-sol","stream":true}`, "test-key", http.StatusBadRequest},
		{"responses", `{"model":"big-pickle","stream":true}`, "test-key", http.StatusBadRequest},
		{"chat/completions", `{"model":"muse-spark-1.3-contributor-free","stream":true}`, "test-key", http.StatusBadRequest},
		{"chat/completions", `{"model":"paid-model-free","stream":true}`, "test-key", http.StatusBadRequest},
		{"chat/completions", `{"model":"mimo-v2.5-free","stream":true}`, "test-key", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/"+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+tc.key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: status=%d body=%s", tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("invalid requests reached upstream: %d", calls)
	}
}

func TestOpenCodeFreeCatalogModelsRouteWithoutAliases(t *testing.T) {
	for _, model := range openCodeFreeModels {
		t.Run(model.ID, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Model string `json:"model"`
				}
				if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
					t.Error(errDecode)
				}
				if body.Model != model.ID || r.URL.Path != "/"+model.Endpoint {
					t.Errorf("free model route = %q %q", body.Model, r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"opaque\"}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			server := newTestServer(t)
			server.exoFreeUpstreamURL = upstream.URL + "/chat/completions"
			server.exoFreeResponsesURL = upstream.URL + "/responses"
			req := httptest.NewRequest(http.MethodPost, "/v1/"+model.Endpoint, strings.NewReader(`{"model":"`+model.ID+`","stream":true,"input":"hello","messages":[]}`))
			req.Header.Set("Authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.exoFreeAPIHandler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || calls != 1 || !strings.Contains(rec.Body.String(), "opaque") {
				t.Fatalf("free model rejected: status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
		})
	}
}

func TestOpenCodeFreeNativeErrorsAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		path, model string
		status      int
	}{
		{"chat/completions", "exo-free", http.StatusPaymentRequired},
		{"responses", "muse-spark-1.3-contributor-free", http.StatusForbidden},
	} {
		t.Run(tc.path, func(t *testing.T) {
			calls := 0
			body := `{"error":{"type":"server_error","message":"Endpoint is unavailable."}}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			server := newTestServer(t)
			server.exoFreeUpstreamURL, server.exoFreeResponsesURL = upstream.URL, upstream.URL
			req := httptest.NewRequest(http.MethodPost, "/v1/"+tc.path, strings.NewReader(`{"model":"`+tc.model+`","stream":true}`))
			req.Header.Set("Authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.exoFreeAPIHandler().ServeHTTP(rec, req)
			if rec.Code != tc.status || rec.Body.String() != body || calls != 1 {
				t.Fatalf("upstream error changed: status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
		})
	}
}

func TestOpenCodeFreeResponsesFlushAndCancel(t *testing.T) {
	canceled := make(chan struct{})
	frame := "event: response.output_text.delta\ndata: {\"delta\":\"first\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frame)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	server := newTestServer(t)
	server.exoFreeResponsesURL = upstream.URL
	downstream := httptest.NewServer(server.exoFreeAPIHandler())
	defer downstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL+"/v1/responses", strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","input":"hello","stream":true}`))
	if errNew != nil {
		t.Fatal(errNew)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, errDo := downstream.Client().Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, len(frame))
	if _, errRead := io.ReadFull(resp.Body, first); errRead != nil || string(first) != frame {
		t.Fatalf("first Responses event was not flushed: %v %q", errRead, first)
	}
	cancel()
	<-canceled
}

func TestExoFreeListenerMismatchAndPortRollback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {"+`"id":"resp_wrong","choices":[]}`+"\n\n")
	}))
	defer upstream.Close()
	server := newTestServer(t)
	server.exoFreeHost = "127.0.0.1"
	server.exoFreeUpstreamURL = upstream.URL
	t.Cleanup(func() { _ = server.stopExoFreeListener() })
	port := degradedTestPort(t)
	if errPort := server.setExoFreePort(port); errPort != nil {
		t.Fatal(errPort)
	}
	busy, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = busy.Close() }()
	if errPort := server.setExoFreePort(busy.Addr().(*net.TCPAddr).Port); errPort == nil || server.exoFreePort != port {
		t.Fatal("failed bind changed running listener")
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(`{"model":"claude-opus-5-5","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	content, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(content), "5 attempts") {
		t.Fatalf("mismatched stream: %d %s", resp.StatusCode, content)
	}
	if errPort := server.setExoFreePort(0); errPort != nil {
		t.Fatal(errPort)
	}
	listener, errListen := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if errListen != nil {
		t.Fatalf("listener remained bound: %v", errListen)
	}
	_ = listener.Close()
}

func TestExoFreeListenerUsesFreshStaticISPForEachAttempt(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]string)
	nodes := make([]config.StaticISPNode, 0, 2)
	for i := 0; i < 2; i++ {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[fmt.Sprintf("isp-%d", i)] = r.Header.Get("X-Opencode-Request")
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			if i == 0 {
				_, _ = fmt.Fprint(w, "data: {\"id\":\"msg_wrong\"}\n\n")
				return
			}
			_, _ = fmt.Fprint(w, "data: {\"id\":\"resp_right\",\"model\":\"exo-free\"}\n\ndata: [DONE]\n\n")
		}))
		defer proxy.Close()
		parsed, errParse := url.Parse(proxy.URL)
		if errParse != nil {
			t.Fatal(errParse)
		}
		port, _ := strconv.Atoi(parsed.Port())
		nodes = append(nodes, config.StaticISPNode{ID: fmt.Sprintf("isp-%d", i), Scheme: "http", Host: parsed.Hostname(), Port: port, Username: "user", Password: "secret"})
	}
	server := newTestServer(t)
	server.exoFreeHost = "127.0.0.1"
	server.exoFreeUpstreamURL = "http://exo-free-upstream.invalid/chat/completions"
	server.exoFreeSelectNode = func(n int) int {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) % n
	}
	server.setExoFreePool(&config.CodexStaticISPConfig{Enabled: true, Nodes: nodes})
	t.Cleanup(func() { _ = server.stopExoFreeListener() })
	port := degradedTestPort(t)
	if errPort := server.setExoFreePort(port); errPort != nil {
		t.Fatal(errPort)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-6-sol","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	content, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(content), "resp_right") || strings.Contains(string(content), "msg_wrong") {
		t.Fatalf("proxy retry response: %d %s", resp.StatusCode, content)
	}
	mu.Lock()
	if len(seen) != 2 {
		t.Errorf("expected two independent ISP exits, got %v", seen)
	}
	mu.Unlock()
}

func TestExoFreeListenerClaudeAndUpstreamRejection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		upstreamID string
		status     int
	}{
		{"claude success", "msg_correct", http.StatusOK},
		{"upstream forbidden", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, `{"error":"free tier rejected"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":%q,\"model\":\"exo-free\"}\n\ndata: [DONE]\n\n", tc.upstreamID)
			}))
			defer upstream.Close()
			server := newTestServer(t)
			server.exoFreeHost = "127.0.0.1"
			server.exoFreeUpstreamURL = upstream.URL
			t.Cleanup(func() { _ = server.stopExoFreeListener() })
			port := degradedTestPort(t)
			if errPort := server.setExoFreePort(port); errPort != nil {
				t.Fatal(errPort)
			}
			req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(`{"model":"claude-opus-5-5","messages":[],"stream":true}`))
			req.Header.Set("Authorization", "Bearer test-key")
			resp, errDo := http.DefaultClient.Do(req)
			if errDo != nil {
				t.Fatal(errDo)
			}
			content, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, calls, content)
			}
			if tc.status == http.StatusOK && (!strings.Contains(string(content), "msg_correct") || !strings.Contains(string(content), `"model":"claude-opus-5-5"`)) {
				t.Errorf("Claude response = %s", content)
			}
		})
	}
}
