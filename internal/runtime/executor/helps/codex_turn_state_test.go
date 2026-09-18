package helps

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func testTurnState(issued time.Time, degraded bool, marker byte) string {
	size := 217
	if degraded {
		size = 233
	}
	raw := make([]byte, size)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	raw[9] = marker
	return base64.URLEncoding.EncodeToString(raw)
}

func turnStateTestAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{
		"access_token": "access-" + id, "account_id": "account-" + id,
	}}
}

func TestCodexTurnStateTimestampAndClassification(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name  string
		token string
		valid bool
	}{
		{"quality", testTurnState(now, false, 1), true},
		{"unpadded", strings.TrimRight(testTurnState(now, false, 1), "="), true},
		{"degraded", testTurnState(now, true, 1), false},
		{"expired", testTurnState(now.Add(-time.Hour), false, 1), false},
		{"future", testTurnState(now.Add(time.Hour), false, 1), false},
		{"malformed", strings.Repeat("a", 292), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expires, err := codexTurnStateExpiry(tc.token, now)
			if (err == nil) != tc.valid {
				t.Fatalf("valid = %v, want %v: %v", err == nil, tc.valid, err)
			}
			if tc.valid && !expires.Equal(now.Add(time.Hour)) {
				t.Fatalf("expiry = %v, want issuance + one hour", expires)
			}
		})
	}
}

func TestCodexTurnStateAccountsRenewIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := turnStateTestAuth("a"), turnStateTestAuth("b")
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{a, b} })
		defer keeper.Close()
		var mu sync.Mutex
		calls := map[string]int{}
		keeper.fetch = func(_ context.Context, _ *config.Config, auth *cliproxyauth.Auth) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			calls[auth.ID]++
			return testTurnState(time.Now(), false, byte(auth.ID[0])+byte(calls[auth.ID])), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		tokenA, managedA := keeper.Token(a)
		tokenB, managedB := keeper.Token(b)
		if !managedA || !managedB || tokenA == "" || tokenB == "" || tokenA == tokenB {
			t.Fatal("each account must acquire its own state")
		}
		for range 20 {
			keeper.Observe(a, tokenA, testTurnState(time.Now(), true, 0))
		}
		synctest.Wait()
		refreshedA, _ := keeper.Token(a)
		unchangedB, _ := keeper.Token(b)
		if refreshedA == tokenA || unchangedB != tokenB {
			t.Fatal("312 must renew only the account that received it")
		}
		keeper.Observe(a, tokenA, testTurnState(time.Now(), true, 0))
		synctest.Wait()
		mu.Lock()
		if calls["a"] != 2 || calls["b"] != 1 {
			t.Errorf("duplicate or stale responses caused extra probes: %v", calls)
		}
		mu.Unlock()
		encoded, _ := json.Marshal(keeper.Snapshot())
		if strings.Contains(string(encoded), refreshedA) || strings.Contains(string(encoded), "access-") {
			t.Fatal("status must not contain state or credentials")
		}
	})
}

func TestCodexTurnStateTimerAndDisable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := turnStateTestAuth("a")
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{a} })
		defer keeper.Close()
		var calls atomic.Int32
		keeper.fetch = func(_ context.Context, _ *config.Config, _ *cliproxyauth.Auth) (string, error) {
			return testTurnState(time.Now(), false, byte(calls.Add(1))), nil
		}
		cfg := &config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}}
		keeper.Configure(cfg)
		synctest.Wait()
		time.Sleep(54 * time.Minute)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("renewed too early: %d", calls.Load())
		}
		keeper.Configure(cfg.CloneForRuntime())
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("unrelated reload must preserve cached state")
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("45-second inspection must renew before expiry: %d", calls.Load())
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(false)}})
		time.Sleep(time.Hour)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("disabled keeper continued fetching")
		}
		if token, managed := keeper.Token(a); token != "" || managed {
			t.Fatal("disabled keeper must stop injection")
		}
	})
}

func TestCodexTurnStateDisableCancelsInflightFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := turnStateTestAuth("a")
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{a} })
		defer keeper.Close()
		var cancelled atomic.Bool
		keeper.fetch = func(ctx context.Context, _ *config.Config, _ *cliproxyauth.Auth) (string, error) {
			<-ctx.Done()
			cancelled.Store(true)
			return testTurnState(time.Now(), false, 1), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		keeper.Configure(&config.Config{})
		synctest.Wait()
		if !cancelled.Load() || len(keeper.Snapshot()) != 0 {
			t.Fatal("disable must cancel probes and discard late results")
		}
	})
}

func TestFetchCodexTurnStateUsesAccountCredentials(t *testing.T) {
	token := testTurnState(time.Now(), false, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			t.Errorf("unexpected probe: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access-a" || r.Header.Get("ChatGPT-Account-ID") != "account-a" {
			t.Error("probe used another account's credentials")
		}
		if r.Header.Get(CodexTurnStateHeader) != "" {
			t.Error("probe must acquire a fresh state without reusing cached state")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode probe: %v", err)
		}
		if payload["stream"] != true || payload["store"] != false || payload["model"] == "" {
			t.Error("probe must be a minimal streaming Responses request")
		}
		w.Header().Set(CodexTurnStateHeader, token)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	a := turnStateTestAuth("a")
	a.Attributes = map[string]string{"base_url": server.URL}
	a.ProxyURL = "http://127.0.0.1:1"
	got, err := fetchCodexTurnState(t.Context(), &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:1"}}, a)
	if err != nil || got != token {
		t.Fatalf("fetch failed: %v", err)
	}
}

// Opt-in smoke test: credentials are read only from the explicitly supplied file.
func TestFetchCodexTurnStateLive(t *testing.T) {
	path := os.Getenv("CPA_TURN_STATE_TEST_AUTH")
	if path == "" {
		t.Skip("CPA_TURN_STATE_TEST_AUTH is not set")
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal("cannot read supplied test credential file")
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(data, &metadata); errDecode != nil {
		t.Fatal("invalid test credential JSON")
	}
	auth := &cliproxyauth.Auth{ID: "live-test", Provider: "codex", Metadata: metadata}
	token, errFetch := fetchCodexTurnState(t.Context(), &config.Config{}, auth)
	if errFetch != nil {
		t.Fatal(errFetch)
	}
	expires, errState := codexTurnStateExpiry(token, time.Now())
	if errState != nil {
		if codexTurnStateDegraded(token) {
			t.Skipf("direct probe returned state_length=%d; valid-state inference cannot be tested on this attempt", len(token))
		}
		t.Fatalf("direct probe: state_length=%d, validation=%v", len(token), errState)
	}
	t.Logf("direct probe: state_length=%d, estimated_remaining_seconds=%d", len(token), int64(time.Until(expires)/time.Second))
	if os.Getenv("CPA_TURN_STATE_TEST_INFERENCE") != "1" {
		return
	}
	keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
	keeper.ctx, keeper.cancel = context.WithCancel(t.Context())
	keeper.cfg = &config.Config{}
	keeper.entries[auth.ID] = &codexTurnStateEntry{auth: auth.Clone(), scope: codexTurnStateScope(auth), token: token, expires: expires}
	defer keeper.Close()
	payload, _ := json.Marshal(map[string]any{"model": codexTurnStateProbeModel, "store": false, "stream": true, "instructions": "", "input": []any{map[string]any{"role": "user", "content": "Reply with OK."}}})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, codexTurnStateBaseURL(auth)+"/responses", strings.NewReader(string(payload)))
	access, _ := metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("ChatGPT-Account-ID", codexTurnStateAccountID(auth))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	direct := auth.Clone()
	direct.ProxyURL = "direct"
	client := keeper.WrapHTTPClient(NewCodexHTTPClient(t.Context(), nil, direct, 0), auth)
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inference returned HTTP %d", resp.StatusCode)
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	completed := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "data:") && gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "type").String() == "response.completed" {
			completed = true
		}
	}
	if !completed {
		t.Fatal("inference did not reach response.completed")
	}
	t.Logf("direct inference with injected state: HTTP=%d, completed=%v, response_state_length=%d", resp.StatusCode, completed, len(resp.Header.Get(CodexTurnStateHeader)))
}

type turnStateRoundTripper func(*http.Request) (*http.Response, error)

func (f turnStateRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCodexTurnStateHTTPInjectionAnd312(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := turnStateTestAuth("a"), turnStateTestAuth("b")
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{a, b} })
		defer keeper.Close()
		var mu sync.Mutex
		calls := map[string]int{}
		keeper.fetch = func(_ context.Context, _ *config.Config, auth *cliproxyauth.Auth) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			calls[auth.ID]++
			return testTurnState(time.Now(), false, auth.ID[0]+byte(calls[auth.ID])), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		tokenA, _ := keeper.Token(a)
		tokenB, _ := keeper.Token(b)
		for _, auth := range []*cliproxyauth.Auth{a, b} {
			want := tokenA
			if auth == b {
				want = tokenB
			}
			client := keeper.WrapHTTPClient(&http.Client{Transport: turnStateRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get(CodexTurnStateHeader) != want {
					t.Error("wrong account state injected")
				}
				body, _ := io.ReadAll(req.Body)
				if gjson.GetBytes(body, codexTurnStateMetadataPath).Exists() || gjson.GetBytes(body, "input").String() != "hello" {
					t.Error("must remove client state while preserving request content")
				}
				if req.ContentLength != int64(len(body)) {
					t.Error("wrong rewritten content length")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{CodexTurnStateHeader: {testTurnState(time.Now(), true, 0)}}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})}, auth)
			req, _ := http.NewRequest(http.MethodPost, "https://codex.test/responses", strings.NewReader(`{"input":"hello","client_metadata":{"x-codex-turn-state":"foreign"}}`))
			req.Header.Set(CodexTurnStateHeader, "foreign")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if req.Header.Get(CodexTurnStateHeader) != "foreign" {
				t.Error("caller headers mutated")
			}
		}
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		if calls["a"] != 2 || calls["b"] != 2 {
			t.Fatalf("312 response did not renew each account: %v", calls)
		}
	})
}

func TestCodexTurnStateWebsocketPerTurnInjection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := turnStateTestAuth("a")
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{a} })
		defer keeper.Close()
		var calls atomic.Int32
		keeper.fetch = func(_ context.Context, _ *config.Config, _ *cliproxyauth.Auth) (string, error) {
			return testTurnState(time.Now(), false, byte(calls.Add(1))), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		headers := http.Header{CodexTurnStateHeader: {"foreign"}}
		body, token := keeper.StampWebsocket(a, headers, []byte(`{"type":"response.create","input":[],"previous_response_id":"resp_1"}`))
		if headers.Get(CodexTurnStateHeader) != "" || gjson.GetBytes(body, codexTurnStateMetadataPath).String() != token || token == "" {
			t.Fatal("websocket must inject into client_metadata, not the handshake")
		}
		if gjson.GetBytes(body, "previous_response_id").String() != "resp_1" {
			t.Fatal("incremental context was changed")
		}
		event, _ := json.Marshal(map[string]any{"type": "response.headers", "headers": map[string]string{"X-Codex-Turn-State": testTurnState(time.Now(), true, 0)}})
		keeper.ObserveWebsocket(a, token, event)
		synctest.Wait()
		_, next := keeper.StampWebsocket(a, headers, body)
		if calls.Load() != 2 || next == token {
			t.Fatal("next websocket turn must use renewed state")
		}
		if strings.Contains(string(RedactCodexTurnStatePayload(body)), token) {
			t.Fatal("request log leaked state")
		}
	})
}

func TestCodexTurnStateExpiryAndFailedRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := turnStateTestAuth("a")
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{a} })
		defer keeper.Close()
		var calls atomic.Int32
		keeper.fetch = func(_ context.Context, _ *config.Config, _ *cliproxyauth.Auth) (string, error) {
			if calls.Add(1) > 1 {
				return "", errors.New("probe failed")
			}
			return testTurnState(time.Now().Add(-54*time.Minute), false, 1), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		old, _ := keeper.Token(a)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got, _ := keeper.Token(a); got != old {
			t.Fatal("failed early renewal must retain unexpired state")
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if got, managed := keeper.Token(a); got != "" || !managed {
			t.Fatal("expired state must never be injected")
		}
	})
}

func TestCodexTurnStateAccountReplacementAndRemoval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := turnStateTestAuth("a")
		var sourceMu sync.Mutex
		auths := []*cliproxyauth.Auth{a}
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth {
			sourceMu.Lock()
			defer sourceMu.Unlock()
			return append([]*cliproxyauth.Auth(nil), auths...)
		})
		defer keeper.Close()
		keeper.fetch = func(_ context.Context, _ *config.Config, _ *cliproxyauth.Auth) (string, error) {
			return testTurnState(time.Now(), false, 1), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		replacement := turnStateTestAuth("a")
		replacement.Metadata["account_id"] = "another-upstream-account"
		if token, _ := keeper.Token(replacement); token != "" {
			t.Fatal("replaced account inherited old account state")
		}
		synctest.Wait()
		sourceMu.Lock()
		auths = nil
		sourceMu.Unlock()
		time.Sleep(CodexTurnStateCheckInterval)
		synctest.Wait()
		if len(keeper.Snapshot()) != 0 {
			t.Fatal("removed account is still being maintained")
		}
	})
}
