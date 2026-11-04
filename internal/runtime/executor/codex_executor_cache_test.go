package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexExecutorCacheHelper_OpenAIChatCompletions_StablePromptCacheKeyFromAPIKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("userApiKey", "test-api-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	executor := &CodexExecutor{}
	rawJSON := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex"}`),
	}
	url := "https://example.com/responses"

	httpReq, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON, nil)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	expectedKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:test-api-key")).String()
	gotKey := ojson.GetBytes(body, "prompt_cache_key").String()
	if gotKey != expectedKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedKey)
	}
	if gotConversation := httpReq.Header.Get("Conversation_id"); gotConversation != "" {
		t.Fatalf("Conversation_id = %q, want empty", gotConversation)
	}
	if gotSession := httpReq.Header["Session-Id"]; len(gotSession) != 1 || gotSession[0] != expectedKey {
		t.Fatalf("Session-Id = %#v, want [%q]", gotSession, expectedKey)
	}
	if gotLegacySession := httpReq.Header.Get("Session_id"); gotLegacySession != "" {
		t.Fatalf("Session_id = %q, want empty", gotLegacySession)
	}

	httpReq2, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, req, rawJSON, nil)
	if err != nil {
		t.Fatalf("cacheHelper error (second call): %v", err)
	}
	body2, errRead2 := io.ReadAll(httpReq2.Body)
	if errRead2 != nil {
		t.Fatalf("read request body (second call): %v", errRead2)
	}
	gotKey2 := ojson.GetBytes(body2, "prompt_cache_key").String()
	if gotKey2 != expectedKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q", gotKey2, expectedKey)
	}
}

func TestCodexExecutorCacheHelper_UsesDerivedSessionUUID(t *testing.T) {
	t.Parallel()

	executor := &CodexExecutor{}
	req := cliproxyexecutor.Request{
		Model:    "gpt-5.4",
		Payload:  []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`),
		Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:derived-root"},
	}
	expectedKey := helps.DerivedSessionUUID("codex", req.Metadata)

	httpReq, body, err := executor.cacheHelper(context.Background(), sdktranslator.FormatOpenAI, "https://example.com/responses", req, []byte(`{"model":"gpt-5.4","stream":true}`), nil)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	if got := ojson.GetBytes(body, "prompt_cache_key").String(); got != expectedKey {
		t.Fatalf("prompt_cache_key = %q, want %q", got, expectedKey)
	}
	if got := httpReq.Header.Get("Session-Id"); got != expectedKey {
		t.Fatalf("Session-Id = %q, want %q", got, expectedKey)
	}
	if _, errParse := uuid.Parse(expectedKey); errParse != nil {
		t.Fatalf("derived prompt cache key %q is not a UUID: %v", expectedKey, errParse)
	}
}

func TestCodexExecutorCacheHelper_ClaudeUsesClaudeCodeSessionID(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := context.Background()
	url := "https://example.com/responses"
	rawJSON := []byte(`{"model":"gpt-5.4","stream":true}`)
	firstReq := cliproxyexecutor.Request{
		Model: "gpt-5.4-claude-cache-session",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-a\",\"account_uuid\":\"\",\"session_id\":\"cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]
		}`),
	}
	secondReq := cliproxyexecutor.Request{
		Model: "gpt-5.4-claude-cache-session",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-b\",\"account_uuid\":\"\",\"session_id\":\"cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}]
		}`),
	}

	firstHTTPReq, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, firstReq, rawJSON, nil)
	if err != nil {
		t.Fatalf("cacheHelper first error: %v", err)
	}
	secondHTTPReq, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, secondReq, rawJSON, nil)
	if err != nil {
		t.Fatalf("cacheHelper second error: %v", err)
	}

	firstBody, errRead := io.ReadAll(firstHTTPReq.Body)
	if errRead != nil {
		t.Fatalf("read first request body: %v", errRead)
	}
	secondBody, errRead := io.ReadAll(secondHTTPReq.Body)
	if errRead != nil {
		t.Fatalf("read second request body: %v", errRead)
	}
	firstKey := ojson.GetBytes(firstBody, "prompt_cache_key").String()
	secondKey := ojson.GetBytes(secondBody, "prompt_cache_key").String()
	if firstKey == "" {
		t.Fatalf("first prompt_cache_key is empty; body=%s", string(firstBody))
	}
	if secondKey != firstKey {
		t.Fatalf("same Claude Code session_id produced different prompt_cache_key: first=%q second=%q", firstKey, secondKey)
	}
	if gotSession := firstHTTPReq.Header["Session-Id"]; len(gotSession) != 1 || gotSession[0] != firstKey {
		t.Fatalf("first Session-Id = %#v, want [%q]", gotSession, firstKey)
	}
	if gotSession := secondHTTPReq.Header["Session-Id"]; len(gotSession) != 1 || gotSession[0] != firstKey {
		t.Fatalf("second Session-Id = %#v, want [%q]", gotSession, firstKey)
	}
}

func TestCodexExecutorCacheHelper_ClaudeRejectsBareUserID(t *testing.T) {
	executor := &CodexExecutor{}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4-claude-cache-bare-user",
		Payload: []byte(`{"model":"gpt-5.4","metadata":{"user_id":"same-user-across-chats"},"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
	}

	httpReq, _, err := executor.cacheHelper(context.Background(), sdktranslator.FromString("claude"), "https://example.com/responses", req, []byte(`{"model":"gpt-5.4","stream":true}`), nil)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	if got := ojson.GetBytes(body, "prompt_cache_key").String(); got != "" {
		t.Fatalf("bare metadata.user_id must not create prompt_cache_key, got %q; body=%s", got, string(body))
	}
	if got := httpReq.Header["Session-Id"]; len(got) != 0 {
		t.Fatalf("bare metadata.user_id must not create Session-Id, got %#v", got)
	}
	if got := httpReq.Header.Get("Session_id"); got != "" {
		t.Fatalf("bare metadata.user_id must not create Session_id, got %q", got)
	}
}

func TestApplyCodexHeadersUsesAccountHeaderForOAuth(t *testing.T) {
	httpReq := httptest.NewRequest("POST", "https://example.com/responses", nil)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct-1"},
	}

	applyCodexHeaders(httpReq, auth, "oauth-token", true, nil)

	if got := httpReq.Header.Get("Chatgpt-Account-Id"); got != "acct-1" {
		t.Fatalf("Chatgpt-Account-Id = %q, want acct-1", got)
	}
}

func TestCodexIdentityConfuseKeepsClientBodySeparateFromUpstreamBody(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"cache-1"}`)

	upstreamBody, identityState := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)
	expectedPromptCacheKey := codexIdentityConfuseUUID("auth-1", "prompt-cache", "cache-1")
	if identityState.promptCacheKey != expectedPromptCacheKey {
		t.Fatalf("identity prompt_cache_key = %q, want %q", identityState.promptCacheKey, expectedPromptCacheKey)
	}
	if gotKey := gjson.GetBytes(upstreamBody, "prompt_cache_key").String(); gotKey != expectedPromptCacheKey {
		t.Fatalf("upstream prompt_cache_key = %q, want %q", gotKey, expectedPromptCacheKey)
	}
	if gotKey := gjson.GetBytes(clientBody, "prompt_cache_key").String(); gotKey != "cache-1" {
		t.Fatalf("client prompt_cache_key = %q, want cache-1", gotKey)
	}
}

func TestCodexFingerprintConvergenceConfigSnapshot(t *testing.T) {
	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ID: "fingerprint-snapshot", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}}
	native := NewCodexExecutor(cfg)
	cfg.Routing.CodexFingerprintConvergence = new(true)
	converged := NewCodexExecutor(cfg)
	*cfg.Routing.CodexFingerprintConvergence = false
	if codexFingerprintConvergenceEnabled(native.cfg, auth) || !codexFingerprintConvergenceEnabled(converged.cfg, auth) {
		t.Fatal("runtime setting mutation changed an executor's active request snapshot")
	}
}

func TestCodexFingerprintConvergenceUsesStableUniqueAccountIdentity(t *testing.T) {
	enabled := true
	cfg := &config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: &enabled}}
	authA := &cliproxyauth.Auth{ID: "auth-converged-a", Provider: "codex", Metadata: map[string]any{"access_token": "token-a"}}
	authB := &cliproxyauth.Auth{ID: "auth-converged-b", Provider: "codex", Metadata: map[string]any{"access_token": "token-b"}}
	request := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"client-session","client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-thread:0"}}`)

	first, firstState := applyCodexIdentityConfuseBody(cfg, authA, request, request)
	second, secondState := applyCodexIdentityConfuseBody(cfg, authA, request, request)
	other, otherState := applyCodexIdentityConfuseBody(cfg, authB, request, request)

	if !firstState.convergence || !secondState.convergence || !otherState.convergence {
		t.Fatal("expected convergence state for Codex OAuth accounts")
	}
	for _, field := range []string{"x-codex-installation-id", "session_id", "thread_id", "x-codex-window-id"} {
		firstValue := gjson.GetBytes(first, "client_metadata."+field).String()
		secondValue := gjson.GetBytes(second, "client_metadata."+field).String()
		otherValue := gjson.GetBytes(other, "client_metadata."+field).String()
		if firstValue == "" || firstValue != secondValue {
			t.Fatalf("client_metadata.%s is not stable: first=%q second=%q", field, firstValue, secondValue)
		}
		if firstValue == otherValue {
			t.Fatalf("client_metadata.%s is reused across accounts: %q", field, firstValue)
		}
	}
	firstHeaders := make(http.Header)
	applyCodexIdentityConfuseHeaders(firstHeaders, &firstState)
	if got := firstHeaders.Get("X-Codex-Installation-Id"); got != gjson.GetBytes(first, "client_metadata.x-codex-installation-id").String() {
		t.Fatalf("installation header=%q does not match body=%q", got, gjson.GetBytes(first, "client_metadata.x-codex-installation-id").String())
	}
}

func TestCodexFingerprintConvergenceDoesNotAffectAPIKeys(t *testing.T) {
	enabled := true
	cfg := &config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: &enabled}}
	auth := &cliproxyauth.Auth{ID: "api-key-auth", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	body := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"client-session","client_metadata":{"x-codex-installation-id":"client-install"}}`)
	updated, state := applyCodexIdentityConfuseBody(cfg, auth, body, body)
	if state.convergence || !bytes.Equal(updated, body) {
		t.Fatalf("API key body changed by convergence: state=%+v body=%s", state, string(updated))
	}
}

func TestCodexFingerprintConvergencePreservesExplicitCacheKeysAndPayload(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: new(true)}}
	auth := &cliproxyauth.Auth{ID: "fingerprint-cache", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}}
	for _, payload := range []string{
		`{"prompt_cache_key":"explicit-cache","client_metadata":{"session_id":"client-session","custom":{"large":9007199254740993}},"input":"client-session"}`,
		`{"input":"no cache key"}`,
	} {
		body := []byte(payload)
		updated, state := applyCodexIdentityConfuseBody(cfg, auth, body, body)
		if got, want := gjson.GetBytes(updated, "prompt_cache_key").Raw, gjson.GetBytes(body, "prompt_cache_key").Raw; got != want {
			t.Fatalf("explicit or missing prompt_cache_key changed: got=%s want=%s", got, want)
		}
		if got, want := gjson.GetBytes(updated, "client_metadata.custom").Raw, gjson.GetBytes(body, "client_metadata.custom").Raw; got != want {
			t.Fatalf("unrelated metadata changed: got=%s want=%s", got, want)
		}
		if !state.convergence || string(body) != payload {
			t.Fatal("convergence was skipped or mutated the client payload")
		}
	}
}

func TestCodexFingerprintConvergencePreservesUserPromptCacheSession(t *testing.T) {
	enabled := true
	executor := &CodexExecutor{cfg: &config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: &enabled}}}
	auth := &cliproxyauth.Auth{
		ID:       "cache-preserving-auth",
		Provider: "codex",
		Metadata: map[string]any{"access_token": "test-token"},
	}
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"user-cache-key","client_metadata":{"session_id":"user-cache-key","thread_id":"user-thread"}}`)
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: clientBody}
	httpReq, body, state, err := executor.cacheHelper(context.Background(), sdktranslator.FormatOpenAIResponse, "https://example.test/responses", auth, req, clientBody, clientBody)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	applyCodexHeaders(httpReq, auth, "oauth-token", true, executor.cfg)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &state)
	if !state.convergence {
		t.Fatal("expected Codex fingerprint convergence")
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "user-cache-key" {
		t.Fatalf("prompt_cache_key = %q, want user-cache-key", got)
	}
	if got := httpReq.Header.Get("Session-Id"); got != "user-cache-key" {
		t.Fatalf("Session-Id = %q, want user-cache-key", got)
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got == "" {
		t.Fatal("missing account-scoped installation identity")
	}
	if got := gjson.GetBytes(body, "client_metadata.session_id").String(); got == "user-cache-key" || got == "" {
		t.Fatalf("account fingerprint session_id = %q, want stable non-user identity", got)
	}
}

func TestCodexFingerprintConvergenceSkipsNonResponseEndpoints(t *testing.T) {
	e := NewCodexExecutor(&config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: new(true)}})
	auth := &cliproxyauth.Auth{ID: "fingerprint-endpoints", Provider: "codex", Metadata: map[string]any{"access_token": "test-token"}}
	for _, path := range []string{"/responses/compact", "/images/generations", "/images/edits"} {
		body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
		req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: body}
		_, updated, state, err := e.cacheHelper(context.Background(), sdktranslator.FormatOpenAIResponse, "https://example.test"+path, auth, req, body, body)
		if err != nil || state.convergence || !bytes.Equal(updated, body) {
			t.Fatalf("unsupported endpoint %s changed: body=%s state=%+v err=%v", path, updated, state, err)
		}
	}
}

func TestCodexExecutorCacheHelper_ClaudeUsesSessionHeader(t *testing.T) {
	executor := &CodexExecutor{}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Request.Header.Set(helps.ClaudeCodeSessionHeader, "cache-session-header")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	firstReq := cliproxyexecutor.Request{
		Model:   "gpt-5.4-claude-cache-header",
		Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
	}
	secondReq := cliproxyexecutor.Request{
		Model:   "gpt-5.4-claude-cache-header",
		Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":[{"type":"text","text":"next"}]}]}`),
	}
	rawJSON := []byte(`{"model":"gpt-5.4","stream":true}`)
	url := "https://example.com/responses"

	firstHTTPReq, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, firstReq, rawJSON, nil)
	if err != nil {
		t.Fatalf("cacheHelper first error: %v", err)
	}
	secondHTTPReq, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, secondReq, rawJSON, nil)
	if err != nil {
		t.Fatalf("cacheHelper second error: %v", err)
	}

	firstBody, errRead := io.ReadAll(firstHTTPReq.Body)
	if errRead != nil {
		t.Fatalf("read first request body: %v", errRead)
	}
	secondBody, errRead := io.ReadAll(secondHTTPReq.Body)
	if errRead != nil {
		t.Fatalf("read second request body: %v", errRead)
	}
	firstKey := ojson.GetBytes(firstBody, "prompt_cache_key").String()
	secondKey := ojson.GetBytes(secondBody, "prompt_cache_key").String()
	if firstKey == "" {
		t.Fatalf("first prompt_cache_key is empty; body=%s", string(firstBody))
	}
	if secondKey != firstKey {
		t.Fatalf("same Claude Code session header produced different prompt_cache_key: first=%q second=%q", firstKey, secondKey)
	}
}

func TestCodexExecutorCacheHelper_ClaudeAgentScopeUsesResolvedModelAcrossHTTPAndWebsocket(t *testing.T) {
	executor := &CodexExecutor{}
	url := "https://example.com/responses"
	req := cliproxyexecutor.Request{
		Model:   "requested-alias-high",
		Payload: []byte(`{"model":"requested-alias","messages":[{"role":"user","content":"hello"}]}`),
	}
	rootHeaders := http.Header{}
	rootHeaders.Set(helps.ClaudeCodeSessionHeader, "resolved-model-session")
	childHeaders := rootHeaders.Clone()
	childHeaders.Set(helps.ClaudeCodeAgentHeader, "agent-a")
	rawJSON := []byte(`{"model":"gpt-5.4","stream":true}`)

	rootRequest, _, errRoot := executor.cacheHelper(context.Background(), sdktranslator.FromString("claude"), url, req, rawJSON, nil, rootHeaders)
	if errRoot != nil {
		t.Fatalf("root cacheHelper error: %v", errRoot)
	}
	rootBody, errReadRoot := io.ReadAll(rootRequest.Body)
	if errReadRoot != nil {
		t.Fatalf("read root body: %v", errReadRoot)
	}
	rootKey := ojson.GetBytes(rootBody, "prompt_cache_key").String()

	childRequest, _, errChild := executor.cacheHelper(context.Background(), sdktranslator.FromString("claude"), url, req, rawJSON, nil, childHeaders)
	if errChild != nil {
		t.Fatalf("child cacheHelper error: %v", errChild)
	}
	childBody, errReadChild := io.ReadAll(childRequest.Body)
	if errReadChild != nil {
		t.Fatalf("read child body: %v", errReadChild)
	}
	childKey := ojson.GetBytes(childBody, "prompt_cache_key").String()
	if rootKey == "" || childKey == "" || rootKey == childKey {
		t.Fatalf("agent prompt keys are not isolated: root=%q child=%q", rootKey, childKey)
	}

	aliasReq := req
	aliasReq.Model = "another-local-alias-low"
	aliasRequest, _, errAlias := executor.cacheHelper(context.Background(), sdktranslator.FromString("claude"), url, aliasReq, rawJSON, nil, childHeaders)
	if errAlias != nil {
		t.Fatalf("alias cacheHelper error: %v", errAlias)
	}
	aliasBody, errReadAlias := io.ReadAll(aliasRequest.Body)
	if errReadAlias != nil {
		t.Fatalf("read alias body: %v", errReadAlias)
	}
	if aliasKey := ojson.GetBytes(aliasBody, "prompt_cache_key").String(); aliasKey != childKey {
		t.Fatalf("resolved model key fragmented by request alias: first=%q alias=%q", childKey, aliasKey)
	}

	websocketBody, _, errWebsocket := applyCodexPromptCacheHeadersWithContext(context.Background(), sdktranslator.FromString("claude"), aliasReq, rawJSON, childHeaders)
	if errWebsocket != nil {
		t.Fatalf("websocket prompt cache error: %v", errWebsocket)
	}
	if websocketKey := ojson.GetBytes(websocketBody, "prompt_cache_key").String(); websocketKey != childKey {
		t.Fatalf("HTTP/WebSocket prompt keys differ: http=%q websocket=%q", childKey, websocketKey)
	}
}

const codexQuotaUsageTestPayload = `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":60}}}`

func TestCodexExecutorFetchQuotaRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		apiKey  bool
		wantURL string
	}{
		{"default", "", false, "https://chatgpt.com/backend-api/wham/usage"},
		{"custom origin", "https://quota.example.test:8443/custom/codex/responses?secret=ignored#fragment", false, "https://quota.example.test:8443/backend-api/wham/usage"},
		{"API key uses usage endpoint", "http://quota.example.test/v1", true, "http://quota.example.test/backend-api/wham/usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Routing: config.RoutingConfig{CodexFingerprintConvergence: new(true)}}
			auth := &cliproxyauth.Auth{
				ID: "quota-test", Provider: "codex",
				Metadata:   map[string]any{"access_token": "oauth-secret", "account_id": "acct-quota"},
				Attributes: map[string]string{"base_url": tc.baseURL, "header:X-Quota-Test": "custom"},
			}
			if tc.apiKey {
				auth.Attributes["api_key"] = "api-secret"
			}
			token, _ := codexCreds(auth)
			expected := httptest.NewRequest(http.MethodGet, tc.wantURL, nil)
			applyCodexHeaders(expected, auth, token, false, cfg)
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet || req.URL.String() != tc.wantURL || req.Body != nil || req.ContentLength != 0 {
					t.Fatalf("unexpected quota request: method=%s URL=%s length=%d", req.Method, req.URL, req.ContentLength)
				}
				for _, key := range []string{"Authorization", "User-Agent", "Originator", "Chatgpt-Account-Id", "Accept", "X-Codex-Routing-Hint", "X-Quota-Test"} {
					if got, want := req.Header.Get(key), expected.Header.Get(key); got != want {
						t.Errorf("quota header %s differs from normal Codex request", key)
					}
				}
				if req.Context().Done() != t.Context().Done() {
					t.Error("quota request introduced another timeout context")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(codexQuotaUsageTestPayload))}, nil
			}))
			headers, err := NewCodexExecutor(cfg).FetchQuota(ctx, auth)
			if err != nil || headers.Get("X-Codex-Primary-Used-Percent") != "25" || headers.Get("X-Codex-Primary-Window-Minutes") != "300" || calls != 1 {
				t.Fatalf("FetchQuota() = %#v, %v; requests=%d", headers, err, calls)
			}
		})
	}
}

func TestCodexExecutorFetchQuotaHTTPProxyAndStaticISP(t *testing.T) {
	for _, route := range []string{"direct", "auth proxy", "static ISP"} {
		t.Run(route, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" || r.URL.RawQuery != "" {
					t.Errorf("unexpected quota HTTP request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer quota-token" || r.Header.Get("Chatgpt-Account-Id") != "quota-account" {
					t.Error("quota credentials missing")
				}
				if route != "direct" && (r.URL.Host != "quota.invalid" || r.Header.Get("Proxy-Authorization") == "") {
					t.Error("quota request did not use the authenticated proxy")
				}
				_, _ = io.WriteString(w, codexQuotaUsageTestPayload)
			}))
			t.Cleanup(server.Close)
			auth := &cliproxyauth.Auth{ID: "quota-egress", Provider: "codex",
				Attributes: map[string]string{"base_url": server.URL + "/custom/responses?discard=true"},
				Metadata:   map[string]any{"access_token": "quota-token", "account_id": "quota-account"}}
			cfg := &config.Config{}
			if route != "direct" {
				auth.Attributes["base_url"] = "http://quota.invalid/custom/responses"
				proxyURL, errParse := url.Parse(server.URL)
				if errParse != nil {
					t.Fatal(errParse)
				}
				proxyURL.User = url.UserPassword("proxy-user", "proxy-password")
				auth.ProxyURL = proxyURL.String()
				cfg.ProxyURL = "http://unused.invalid:1"
				if route == "static ISP" {
					port, errPort := strconv.Atoi(proxyURL.Port())
					if errPort != nil {
						t.Fatal(errPort)
					}
					cfg.Routing.CodexStaticISP = &config.CodexStaticISPConfig{Enabled: true, Nodes: []config.StaticISPNode{{ID: "quota-isp", Scheme: "http", Host: proxyURL.Hostname(), Port: port, Username: "proxy-user", Password: "proxy-password"}}}
					auth.ProxyURL = "http://unused-auth.invalid:1"
				}
			}
			exec := NewCodexExecutor(cfg)
			if route == "static ISP" {
				keeper := helps.NewCodexEgressManager(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
				t.Cleanup(keeper.Close)
				if errConfigure := keeper.Configure(cfg); errConfigure != nil {
					t.Fatal(errConfigure)
				}
				exec.egress = keeper
			}
			original := auth.Clone()
			headers, err := exec.FetchQuota(t.Context(), auth)
			if err != nil || headers.Get("X-Codex-Primary-Used-Percent") != "25" || calls.Load() != 1 {
				t.Fatalf("FetchQuota() = %#v, %v; requests=%d", headers, err, calls.Load())
			}
			if auth.ProxyURL != original.ProxyURL || auth.Runtime != original.Runtime || !reflect.DeepEqual(auth.Attributes, original.Attributes) {
				t.Fatal("FetchQuota mutated credential egress")
			}
		})
	}
}

type codexQuotaTestBody struct {
	io.Reader
	closed bool
}

func (b *codexQuotaTestBody) Close() error {
	b.closed = true
	return errors.New("close-secret")
}

func TestCodexExecutorFetchQuotaPreservesHTTPStatusAndRedactsErrors(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		for _, readFails := range []bool{false, true} {
			t.Run(strconv.Itoa(status)+"/readFails="+strconv.FormatBool(readFails), func(t *testing.T) {
				body := &codexQuotaTestBody{Reader: strings.NewReader(`{"error":{"code":"usage_limit_reached","message":"body-secret oauth-secret"}}`)}
				if readFails {
					body.Reader = iotest.ErrReader(errors.New("read-secret oauth-secret"))
				}
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://chatgpt.com/backend-api/codex/responses"}}, Body: body}, nil
				}))
				headers, err := NewCodexExecutor(&config.Config{}).FetchQuota(ctx, &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "oauth-secret"}})
				var statusErr interface{ StatusCode() int }
				if headers != nil || !errors.As(err, &statusErr) || statusErr.StatusCode() != status || !body.closed || calls != 1 {
					t.Fatalf("FetchQuota() = %#v, %v; closed=%v calls=%d", headers, err, body.closed, calls)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatalf("quota error leaked upstream content: %v", err)
				}
			})
		}
	}
}

func TestCodexExecutorFetchQuotaInvalidPayloadKeepsOldQuota(t *testing.T) {
	for _, payload := range []string{`null`, `{}`, `{"plan_type":"pro","credits":{"balance":"0"}}`, `{"rate_limit":{"allowed":true,"primary_window":null}}`, `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":60}}`} {
		body := &codexQuotaTestBody{Reader: strings.NewReader(payload)}
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Codex-Primary-Used-Percent": []string{"0"}}, Body: body}, nil
		}))
		auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "oauth-secret"}}
		auth.Quota.Signals = map[string]string{"X-Codex-Primary-Used-Percent": "70"}
		headers, err := NewCodexExecutor(&config.Config{}).FetchQuota(ctx, auth)
		if headers != nil || err == nil || err.Error() != "codex quota: usage response contains no valid quota windows" || !body.closed {
			t.Fatalf("invalid payload FetchQuota() = %#v, %v; closed=%v", headers, err, body.closed)
		}
		if auth.Quota.Signals["X-Codex-Primary-Used-Percent"] != "70" {
			t.Fatal("invalid usage response replaced old quota")
		}
	}
}

func TestCodexExecutorFetchQuotaTransportAndReadErrors(t *testing.T) {
	for _, tc := range []string{"transport", "body read", "canceled"} {
		t.Run(tc, func(t *testing.T) {
			body := &codexQuotaTestBody{Reader: iotest.ErrReader(errors.New("read-secret"))}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", roundTripperFunc(func(*http.Request) (*http.Response, error) {
				if tc == "transport" {
					return nil, errors.New("transport-secret")
				}
				if tc == "canceled" {
					cancel()
					return nil, context.Canceled
				}
				return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
			}))
			headers, err := NewCodexExecutor(&config.Config{}).FetchQuota(ctx, &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "oauth-secret"}})
			if headers != nil || err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unexpected quota failure: %#v, %v", headers, err)
			}
			if tc == "body read" && !body.closed {
				t.Fatal("failed quota response was not closed")
			}
			if tc == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("context cancellation lost: %v", err)
			}
		})
	}
}

func TestCodexExecutorFetchQuotaRejectsInvalidSetup(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		auth *cliproxyauth.Auth
	}{
		{"nil auth", &config.Config{}, nil},
		{"missing token", &config.Config{}, &cliproxyauth.Auth{Provider: "codex"}},
		{"invalid URL", &config.Config{}, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "secret", "base_url": "https://user:secret@%zz"}}},
		{"relative URL", &config.Config{}, &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "secret", "base_url": "/secret"}}},
		{"static ISP keeper missing", &config.Config{Routing: config.RoutingConfig{CodexStaticISP: &config.CodexStaticISPConfig{Enabled: true, Nodes: []config.StaticISPNode{{ID: "quota-isp", Scheme: "http", Host: "proxy.invalid", Port: 1234}}}}}, &cliproxyauth.Auth{ID: "quota-static", Provider: "codex", Metadata: map[string]any{"access_token": "secret"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(*http.Request) (*http.Response, error) {
				t.Error("invalid quota setup made a network request")
				return nil, errors.New("unexpected network request")
			}))
			headers, err := NewCodexExecutor(tc.cfg).FetchQuota(ctx, tc.auth)
			if headers != nil || err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid setup FetchQuota() = %#v, %v", headers, err)
			}
		})
	}
}
