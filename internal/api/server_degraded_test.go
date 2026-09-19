package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
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
