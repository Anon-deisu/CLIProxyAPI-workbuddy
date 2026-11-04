package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/agentworkspace"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func registerAgentTestAuth(t *testing.T, s *Server, auth *coreauth.Auth, models ...*registry.ModelInfo) {
	t.Helper()
	if _, errRegister := s.handlers.AuthManager.Register(t.Context(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, models)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
}

func TestAgentModelsOnlyIncludeEnabledAuthFiles(t *testing.T) {
	s := newTestServer(t)
	settings := s.cfg.CloneForRuntime()
	settings.Routing.FakeModels = []config.FakeModelConfig{{Name: "agent-fake", Content: "Not a credential model"}}
	s.UpdateClients(settings)
	for _, auth := range []*coreauth.Auth{
		{ID: t.Name() + "-disabled", FileName: "disabled.json", Provider: "codex", Disabled: true},
		{ID: t.Name() + "-auto-disabled", FileName: "auto-disabled.json", Provider: "codex", AutoDisabled: true},
		{ID: t.Name() + "-status-disabled", FileName: "status-disabled.json", Provider: "codex", Status: coreauth.StatusDisabled},
		{ID: t.Name() + "-config", Provider: "codex", Attributes: map[string]string{coreauth.AttributeSource: "config:codex[test]"}},
		{ID: t.Name() + "-memory", FileName: "runtime.json", Provider: "codex", Attributes: map[string]string{coreauth.AttributeRuntimeOnly: "true"}},
	} {
		registerAgentTestAuth(t, s, auth, &registry.ModelInfo{ID: auth.ID, OwnedBy: "other"}, &registry.ModelInfo{ID: "agent-shared", DisplayName: "Wrong metadata", Thinking: &registry.ThinkingSupport{Levels: []string{"max"}}})
	}
	registerAgentTestAuth(t, s, &coreauth.Auth{ID: t.Name() + "-active", FileName: "active.json", Provider: "codex", Status: coreauth.StatusActive},
		&registry.ModelInfo{ID: "agent-shared", DisplayName: "Shared Model", OwnedBy: "openai", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}},
		&registry.ModelInfo{ID: "agent-image", Type: registry.OpenAIImageModelType},
	)
	registerAgentTestAuth(t, s, &coreauth.Auth{ID: t.Name() + "-path", Provider: "claude", Attributes: map[string]string{coreauth.AttributePath: "/auth/path.json"}},
		&registry.ModelInfo{ID: "agent-path", Thinking: &registry.ThinkingSupport{Max: 16384, ZeroAllowed: true, DynamicAllowed: true}},
	)
	registerAgentTestAuth(t, s, &coreauth.Auth{ID: t.Name() + "-duplicate", FileName: "duplicate.json", Provider: "codex"},
		&registry.ModelInfo{ID: "agent-shared", DisplayName: "Shared Model", OwnedBy: "openai", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}},
	)
	want := []agentModel{
		{ID: "agent-path", Name: "agent-path", ReasoningEfforts: []string{"none", "low", "medium", "high", "auto"}},
		{ID: "agent-shared", Name: "Shared Model", Provider: "openai", ReasoningEfforts: []string{"low", "high"}},
	}
	if got := s.agentModels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("enabled credential models = %#v, want %#v", got, want)
	}
	for _, auth := range s.handlers.AuthManager.List() {
		auth.Disabled = true
		if _, errUpdate := s.handlers.AuthManager.Update(t.Context(), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
	}
	if got := s.agentModels(); len(got) != 0 {
		t.Fatalf("disabled credentials still expose models: %#v", got)
	}
}

func TestAgentManagementRoutesPersistDownloadAndDelete(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "agent-test-management-key")
	s := newTestServer(t)
	registerAgentTestAuth(t, s, &coreauth.Auth{ID: t.Name(), FileName: "agent-test.json", Provider: "codex"}, &registry.ModelInfo{ID: "agent-test-model"})
	defer func() {
		if s.agentStore != nil {
			s.agentStore.Close()
		}
	}()
	request := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		return rec
	}
	for _, prefix := range []string{"/v0/management", "/v8/management"} {
		unauthorized := request(http.MethodGet, prefix+"/agent/conversations", "", "")
		if unauthorized.Code != http.StatusUnauthorized {
			t.Fatalf("agent bypassed management auth: %d %s", unauthorized.Code, unauthorized.Body.String())
		}
		models := request(http.MethodGet, prefix+"/agent/models", "", "agent-test-management-key")
		if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), "agent-test-model") {
			t.Fatalf("model catalog = %d %s", models.Code, models.Body.String())
		}
	}
	created := request(http.MethodPost, "/v0/management/agent/conversations", `{"model":"agent-test-model"}`, "agent-test-management-key")
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var conversation agentworkspace.Conversation
	if errDecode := json.Unmarshal(created.Body.Bytes(), &conversation); errDecode != nil {
		t.Fatal(errDecode)
	}
	base := "/v0/management/agent/conversations/" + conversation.ID
	auth, _ := s.handlers.AuthManager.GetByID(t.Name())
	auth.Disabled = true
	if _, errUpdate := s.handlers.AuthManager.Update(t.Context(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if rejected := request(http.MethodPost, base+"/messages", `{"message":"hello"}`, "agent-test-management-key"); rejected.Code != http.StatusBadRequest {
		t.Fatalf("disabled credential accepted an agent message: %d %s", rejected.Code, rejected.Body.String())
	}
	write := request(http.MethodPost, base+"/files?path=result.txt", "downloaded artifact", "agent-test-management-key")
	if write.Code != http.StatusNoContent {
		t.Fatalf("upload = %d %s", write.Code, write.Body.String())
	}
	download := request(http.MethodGet, base+"/files?path=result.txt", "", "agent-test-management-key")
	if download.Code != http.StatusOK || download.Body.String() != "downloaded artifact" || !strings.Contains(download.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %d %s", download.Code, download.Body.String())
	}
	escape := request(http.MethodGet, base+"/files?path=../outside", "", "agent-test-management-key")
	if escape.Code != http.StatusBadRequest {
		t.Fatalf("workspace path traversal = %d", escape.Code)
	}
	deleted := request(http.MethodDelete, base, "", "agent-test-management-key")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", deleted.Code, deleted.Body.String())
	}
	if missing := request(http.MethodGet, base+"/files?path=result.txt", "", "agent-test-management-key"); missing.Code != http.StatusNotFound {
		t.Fatalf("deleted file is available: %d", missing.Code)
	}
}

func TestAgentStreamAccumulatorCombinesFragmentedToolArguments(t *testing.T) {
	a := agentStreamAccumulator{calls: make(map[int]*agentworkspace.ToolCall)}
	var deltas []string
	emit := func(delta agentworkspace.ModelDelta) { deltas = append(deltas, delta.Content) }
	for _, chunk := range []string{
		`{"choices":[{"index":0,"delta":{"content":"Creating "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"a file.","tool_calls":[{"index":0,"id":"tool-1","function":{"name":"write_file","arguments":"{\"path\":\"result.txt\","}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"content\":\"ready\"}"}}]},"finish_reason":"tool_calls"}]}`,
	} {
		if errAdd := a.add([]byte(chunk), emit); errAdd != nil {
			t.Fatal(errAdd)
		}
	}
	result := a.response()
	if !a.finished || result.Content != "Creating a file." || len(result.ToolCalls) != 1 || result.ToolCalls[0].Arguments != `{"path":"result.txt","content":"ready"}` || len(deltas) != 2 {
		t.Fatalf("accumulated response = %#v", result)
	}
}

type agentCompletionCaptureExecutor struct {
	codexSearchCaptureExecutor
	payload []byte
}

func (e *agentCompletionCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, request coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.payload = append([]byte(nil), request.Payload...)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"choices":[{"index":0,"delta":{"content":"Direct answer."},"finish_reason":"stop"}]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestAgentCompletePreservesToolChoiceAndHistory(t *testing.T) {
	for _, choice := range []string{"auto", "none"} {
		t.Run(choice, func(t *testing.T) {
			s := newTestServer(t)
			executor := &agentCompletionCaptureExecutor{}
			s.handlers.AuthManager.RegisterExecutor(executor)
			registerAgentTestAuth(t, s, &coreauth.Auth{ID: t.Name(), FileName: "chat.json", Provider: "codex"}, &registry.ModelInfo{ID: "agent-chat-test", OwnedBy: "openai"})
			request := agentworkspace.ModelRequest{
				Model: "agent-chat-test", ToolChoice: choice,
				Messages: []agentworkspace.Message{
					{Role: "user", Content: "Inspect the workspace"},
					{Role: "assistant", ToolCalls: []agentworkspace.ToolCall{{ID: "list-1", Name: "list_files", Arguments: `{}`}}},
					{Role: "tool", ToolCallID: "list-1", Content: "Workspace is empty."},
				},
				Tools: []agentworkspace.ToolDefinition{{Name: "list_files", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
			}
			response, errComplete := s.agentComplete(t.Context(), request, func(agentworkspace.ModelDelta) {})
			if errComplete != nil || response.Content != "Direct answer." {
				t.Fatalf("completion = %#v, %v", response, errComplete)
			}
			var body struct {
				ToolChoice string            `json:"tool_choice"`
				Tools      []json.RawMessage `json:"tools"`
				Messages   []struct {
					Role       string `json:"role"`
					ToolCallID string `json:"tool_call_id"`
					Content    string `json:"content"`
				} `json:"messages"`
			}
			if errDecode := json.Unmarshal(executor.payload, &body); errDecode != nil {
				t.Fatal(errDecode)
			}
			if body.ToolChoice != choice || len(body.Tools) != 1 || len(body.Messages) != 3 {
				t.Fatalf("tool selection or history was lost: %s", executor.payload)
			}
			result := body.Messages[2]
			if result.Role != "tool" || result.ToolCallID != "list-1" || result.Content != "Workspace is empty." {
				t.Fatalf("tool result was not forwarded: %#v", result)
			}
		})
	}
}

func TestAgentEventStreamReconnectsWithoutCancelingRun(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "agent-test-management-key")
	s := newTestServer(t)
	started := make(chan struct{})
	finished := make(chan struct{})
	store, errNew := agentworkspace.NewStore(t.TempDir(), agentworkspace.ModelRunnerFunc(func(ctx context.Context, req agentworkspace.ModelRequest, emit func(agentworkspace.ModelDelta)) (agentworkspace.ModelResponse, error) {
		close(started)
		emit(agentworkspace.ModelDelta{Content: "still running"})
		<-ctx.Done()
		close(finished)
		return agentworkspace.ModelResponse{}, ctx.Err()
	}), nil)
	if errNew != nil {
		t.Fatal(errNew)
	}
	s.agentStoreOnce.Do(func() { s.agentStore = store })
	defer store.Close()
	conversation, errCreate := store.Create(agentworkspace.ConversationOptions{Model: "test-model"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	if errStart := store.Start(conversation.ID, "hello", agentworkspace.ConversationOptions{}); errStart != nil {
		t.Fatal(errStart)
	}
	<-started
	server := httptest.NewServer(s.engine)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v0/management/agent/conversations/"+conversation.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer agent-test-management-key")
	response, errDo := server.Client().Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", response.StatusCode)
	}
	buffer := make([]byte, 16)
	if _, errRead := io.ReadFull(response.Body, buffer); errRead != nil {
		t.Fatal(errRead)
	}
	cancel()
	_ = response.Body.Close()
	snapshot, errGet := store.Get(conversation.ID)
	if errGet != nil || snapshot.Status != agentworkspace.ConversationRunning {
		t.Fatalf("closing event view canceled background run: %#v %v", snapshot, errGet)
	}
	if errCancel := store.Cancel(conversation.ID); errCancel != nil {
		t.Fatal(errCancel)
	}
	<-finished
}
