package helps

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
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
	return testTurnStateBytes(issued, size, marker)
}

func testTurnStateBytes(issued time.Time, size int, marker byte) string {
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
			expires, err := codexTurnStateExpiry(turnStateTestAuth("a"), tc.token, now)
			if (err == nil) != tc.valid {
				t.Fatalf("valid = %v, want %v: %v", err == nil, tc.valid, err)
			}
			if tc.valid && !expires.Equal(now.Add(time.Hour)) {
				t.Fatalf("expiry = %v, want issuance + one hour", expires)
			}
		})
	}
}

func TestCodexTurnStatePlanClassification(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	quality292 := testTurnState(now, false, 1)
	quality332 := testTurnStateBytes(now, 249, 2)
	if len(quality332) != 332 {
		t.Fatal("invalid Team test state")
	}
	for _, tc := range []struct {
		plan string
		team bool
	}{
		{"plus", false}, {"pro", false}, {"", false},
		{"team", true}, {"team5x", true}, {"team_5x", true},
		{"business", true}, {"self_serve_business_prolite", true}, {" Team5x ", true},
	} {
		for _, source := range []string{"attribute", "plan_type", "chatgpt_plan_type", "id_token", "access_token"} {
			t.Run(tc.plan+"/"+source, func(t *testing.T) {
				auth := turnStateTestAuth("a")
				switch source {
				case "attribute":
					auth.Attributes = map[string]string{"plan_type": tc.plan}
				case "id_token", "access_token":
					claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": tc.plan}})
					auth.Metadata[source] = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
				default:
					auth.Metadata[source] = tc.plan
				}
				for _, candidate := range []struct {
					token string
					valid bool
				}{
					{quality292, !tc.team}, {quality332, tc.team},
					{strings.TrimRight(quality332, "="), tc.team},
					{testTurnState(now, true, 0), false},
					{testTurnStateBytes(now.Add(-time.Hour), 249, 2), false},
				} {
					expires, err := codexTurnStateExpiry(auth, candidate.token, now)
					if (err == nil) != candidate.valid {
						t.Fatalf("length=%d valid=%v want=%v: %v", len(candidate.token), err == nil, candidate.valid, err)
					}
					if candidate.valid && !expires.Equal(now.Add(time.Hour)) {
						t.Fatal("incorrect state expiry")
					}
				}
			})
		}
	}
}

func TestCodexTurnStateMixedPlansAndPlanChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plus, team := turnStateTestAuth("plus"), turnStateTestAuth("team")
		plus.Metadata["plan_type"] = "plus"
		team.Metadata["chatgpt_plan_type"] = "business"
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{plus, team} })
		defer keeper.Close()
		keeper.fetch = func(_ context.Context, _ *config.Config, auth *cliproxyauth.Auth) (string, error) {
			if auth.ID == team.ID {
				return testTurnStateBytes(time.Now(), 249, 2), nil
			}
			return testTurnState(time.Now(), false, 1), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		plusToken, _ := keeper.Token(plus)
		teamToken, _ := keeper.Token(team)
		if len(plusToken) != 292 || len(teamToken) != 332 {
			t.Fatal("mixed plans did not retain their own state")
		}
		for _, account := range keeper.Snapshot() {
			if account.Status != "active" || account.LastError != "" {
				t.Fatalf("state rejected: %+v", account)
			}
			if account.AuthID == team.ID && account.TokenLength != 332 {
				t.Fatal("Team status reported wrong state length")
			}
		}
		headers := http.Header{CodexTurnStateHeader: {plusToken}}
		body, used := keeper.StampWebsocket(team, headers, []byte(`{"input":[]}`))
		if used != teamToken || gjson.GetBytes(body, codexTurnStateMetadataPath).String() != teamToken {
			t.Fatal("Team websocket did not inject its 332 state")
		}
		client := keeper.WrapHTTPClient(&http.Client{Transport: turnStateRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get(CodexTurnStateHeader) != teamToken {
				t.Error("Team HTTP did not inject its 332 state")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{CodexTurnStateHeader: {teamToken}}, Body: http.NoBody}, nil
		})}, team)
		req, _ := http.NewRequest(http.MethodPost, "https://codex.test/responses", strings.NewReader(`{"input":[]}`))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		changed := team.Clone()
		changed.Metadata["chatgpt_plan_type"] = "pro"
		if token, _ := keeper.Token(changed); token != "" {
			t.Fatal("plan change reused incompatible cached state")
		}
	})
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

func TestCodexTurnStateCached332RestoresSchedulingWithoutRefetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := cliproxyauth.NewManager(nil, nil, nil)
		auth := turnStateTestAuth("team")
		auth.Metadata["plan_type"] = "business"
		auth.Metadata["degraded"] = true
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		cfg := &config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true), CodexAutoDegrade: new(false)}}
		manager.SetConfig(cfg)
		keeper := NewCodexTurnStateKeeper(manager.List)
		keeper.SetSchedulingUpdater(manager.ApplyCodexStateScheduling)
		defer keeper.Close()
		var calls atomic.Int32
		keeper.fetch = func(context.Context, *config.Config, *cliproxyauth.Auth) (string, error) {
			return testTurnStateBytes(time.Now(), 249, byte(calls.Add(1))), nil
		}
		keeper.Configure(cfg)
		synctest.Wait()
		cached, _ := keeper.Token(auth)
		cfg.Routing.CodexAutoDegrade = new(true)
		manager.SetConfig(cfg)
		keeper.Configure(cfg)
		time.Sleep(CodexTurnStateCheckInterval)
		synctest.Wait()
		current, _ := manager.GetByID(auth.ID)
		if current.IsDegraded() {
			t.Fatal("cached valid state must restore scheduling after enabling automation")
		}
		if token, _ := keeper.Token(current); token != cached || calls.Load() != 1 {
			t.Fatal("restoring scheduling unnecessarily collected another 332 state")
		}
		for range 3 {
			keeper.Configure(cfg.CloneForRuntime())
			keeper.Observe(current, cached, testTurnStateBytes(time.Now(), 249, 7))
			_ = keeper.Snapshot()
			time.Sleep(15 * time.Minute)
			synctest.Wait()
		}
		if calls.Load() != 1 {
			t.Fatalf("normal account with valid 332 was collected %d times", calls.Load())
		}
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("state was not renewed when approaching expiry")
		}
	})
}

func TestCodexTurnStateIgnoresUnsentState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth := turnStateTestAuth("team")
		auth.Metadata["plan_type"] = "business"
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
		defer keeper.Close()
		var calls atomic.Int32
		keeper.fetch = func(context.Context, *config.Config, *cliproxyauth.Auth) (string, error) {
			size := 249
			if calls.Add(1) == 1 {
				size = 265
			}
			return testTurnStateBytes(time.Now(), size, 1), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}})
		synctest.Wait()
		for range 3 {
			keeper.Observe(auth, "", testTurnStateBytes(time.Now(), 265, 2))
			synctest.Wait()
		}
		if calls.Load() != 1 {
			t.Fatal("requests without a cached state triggered extra collection")
		}
		time.Sleep(CodexTurnStateCheckInterval)
		synctest.Wait()
		cached, _ := keeper.Token(auth)
		keeper.Observe(auth, "", testTurnStateBytes(time.Now(), 265, 2))
		synctest.Wait()
		if token, _ := keeper.Token(auth); token != cached || calls.Load() != 2 {
			t.Fatal("an unstamped request invalidated an unrelated cached state")
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

func TestFetchCodexTurnStateCliproxySOCKS5(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = listener.Close() }()
	state := testTurnState(time.Now(), false, 1)
	proxyResult := make(chan error, 1)
	const username, password = "test-region-US", "p@ss:word /+"
	go func() {
		proxyResult <- func() error {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return errAccept
			}
			defer func() { _ = conn.Close() }()
			header := make([]byte, 2)
			if _, errRead := io.ReadFull(conn, header); errRead != nil {
				return errRead
			}
			if header[0] != 5 {
				return fmt.Errorf("expected SOCKS5 handshake")
			}
			if _, errCopy := io.CopyN(io.Discard, conn, int64(header[1])); errCopy != nil {
				return errCopy
			}
			if _, errWrite := conn.Write([]byte{5, 2}); errWrite != nil {
				return errWrite
			}
			if _, errRead := io.ReadFull(conn, header); errRead != nil {
				return errRead
			}
			userBytes := make([]byte, int(header[1]))
			if _, errRead := io.ReadFull(conn, userBytes); errRead != nil {
				return errRead
			}
			if _, errRead := io.ReadFull(conn, header[:1]); errRead != nil {
				return errRead
			}
			passwordBytes := make([]byte, int(header[0]))
			if _, errRead := io.ReadFull(conn, passwordBytes); errRead != nil {
				return errRead
			}
			if string(userBytes) != username || string(passwordBytes) != password {
				return fmt.Errorf("proxy credentials were not transmitted correctly")
			}
			if _, errWrite := conn.Write([]byte{1, 0}); errWrite != nil {
				return errWrite
			}
			connect := make([]byte, 5)
			if _, errRead := io.ReadFull(conn, connect); errRead != nil {
				return errRead
			}
			if connect[1] != 1 || connect[3] != 3 {
				return fmt.Errorf("expected CONNECT with proxy-side hostname resolution")
			}
			if _, errCopy := io.CopyN(io.Discard, conn, int64(connect[4])+2); errCopy != nil {
				return errCopy
			}
			if _, errWrite := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); errWrite != nil {
				return errWrite
			}
			request, errRead := http.ReadRequest(bufio.NewReader(conn))
			if errRead != nil {
				return errRead
			}
			defer func() { _ = request.Body.Close() }()
			if request.Header.Get("Authorization") != "Bearer access-a" || request.Header.Get("ChatGPT-Account-ID") != "account-a" || request.URL.Path != "/responses" {
				return fmt.Errorf("incorrect account or request through SOCKS5")
			}
			_, errWrite := fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nX-Codex-Turn-State: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", state)
			return errWrite
		}()
	}()
	auth := turnStateTestAuth("a")
	auth.Attributes = map[string]string{"base_url": "http://upstream.test"}
	auth.ProxyURL = "http://inference.test:8080"
	cfg := &config.Config{Routing: config.RoutingConfig{CodexStateCliproxy: &config.CliproxyConfig{
		Enabled: true, Gateway: listener.Addr().String(), Username: username, Password: password,
	}}}
	got, errFetch := fetchCodexTurnState(t.Context(), cfg, auth)
	if errFetch != nil || got != state {
		t.Fatalf("SOCKS5 state collection failed: %v", errFetch)
	}
	if errProxy := <-proxyResult; errProxy != nil {
		t.Fatal(errProxy)
	}
	if auth.ProxyURL != "http://inference.test:8080" {
		t.Fatal("collection changed the account's inference proxy")
	}
}

func TestFetchCodexTurnStateCliproxyIsolationAndErrors(t *testing.T) {
	state := testTurnState(time.Now(), false, 1)
	var directCalls, proxyCalls, inferenceCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		w.Header().Set(CodexTurnStateHeader, state)
	}))
	defer upstream.Close()
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inferenceCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer inference.Close()
	const password = "test-secret@/+"
	var fail atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("test-region-US:"+password)) {
			t.Error("missing proxy credentials")
		}
		if fail.Load() {
			w.WriteHeader(http.StatusProxyAuthRequired)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": password + " " + url.QueryEscape(password) + " access-a"})
			return
		}
		w.Header().Set(CodexTurnStateHeader, state)
	}))
	defer proxy.Close()
	auth := turnStateTestAuth("a")
	auth.Attributes = map[string]string{"base_url": upstream.URL}
	auth.ProxyURL = inference.URL
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:1"}, Routing: config.RoutingConfig{
		CodexStateCliproxy: &config.CliproxyConfig{Enabled: true, Gateway: proxy.URL, Username: "test-region-US", Password: password},
	}}
	for range 2 {
		got, errFetch := fetchCodexTurnState(t.Context(), cfg, auth)
		if errFetch != nil || got != state {
			t.Fatalf("proxy state collection failed: %v", errFetch)
		}
	}
	if proxyCalls.Load() != 2 || directCalls.Load() != 0 || inferenceCalls.Load() != 0 {
		t.Fatal("collection used the wrong route")
	}
	response, errGet := NewCodexHTTPClient(t.Context(), cfg, auth, 0).Get(upstream.URL)
	if errGet != nil {
		t.Fatal(errGet)
	}
	_ = response.Body.Close()
	if inferenceCalls.Load() != 1 {
		t.Fatal("inference stopped using the account proxy")
	}
	fail.Store(true)
	_, errFetch := fetchCodexTurnState(t.Context(), cfg, auth)
	if errFetch == nil || !strings.Contains(errFetch.Error(), "407") || strings.Contains(errFetch.Error(), password) || strings.Contains(errFetch.Error(), url.QueryEscape(password)) || strings.Contains(errFetch.Error(), "access-a") {
		t.Fatal("proxy failure did not preserve status or redact credentials")
	}
	if directCalls.Load() != 0 {
		t.Fatal("proxy failure silently fell back to direct")
	}
	cfg.Routing.CodexStateCliproxy.Enabled = false
	if _, errFetch = fetchCodexTurnState(t.Context(), cfg, auth); errFetch != nil || directCalls.Load() != 1 {
		t.Fatalf("disabling the state proxy did not restore direct collection: %v", errFetch)
	}
}

func TestCodexTurnStateCliproxyReconfigure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth := turnStateTestAuth("a")
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
		defer keeper.Close()
		var calls atomic.Int32
		cancelled, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseProbe()
		keeper.fetch = func(ctx context.Context, cfg *config.Config, _ *cliproxyauth.Auth) (string, error) {
			n := calls.Add(1)
			if n == 2 {
				<-ctx.Done()
				close(cancelled)
				<-release
			}
			return testTurnState(time.Now(), false, byte(n)), nil
		}
		cfg := &config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true)}}
		keeper.Configure(cfg)
		synctest.Wait()
		original, _ := keeper.Token(auth)
		cfg.Routing.CodexStateCliproxy = &config.CliproxyConfig{Enabled: true, Gateway: "proxy.test:1080", Username: "test-region-US", Password: "secret"}
		keeper.Configure(cfg)
		synctest.Wait()
		if token, _ := keeper.Token(auth); token != original || calls.Load() != 1 {
			t.Fatal("proxy change must retain a healthy cached state without collecting again")
		}
		time.Sleep(56 * time.Minute)
		synctest.Wait()
		if token, _ := keeper.Token(auth); token != original || calls.Load() != 2 {
			t.Fatal("renewal must preserve usable state while collection is pending")
		}
		cfg.Routing.CodexStateCliproxy.Username = "test-region-JP"
		keeper.Configure(cfg)
		<-cancelled
		synctest.Wait()
		latest, _ := keeper.Token(auth)
		if latest == original || calls.Load() != 3 {
			t.Fatal("changed route did not cancel and replace its pending collection")
		}
		releaseProbe()
		synctest.Wait()
		if token, _ := keeper.Token(auth); token != latest {
			t.Fatal("late state from an old route replaced the latest state")
		}
		keeper.Configure(cfg.CloneForRuntime())
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatal("unchanged proxy settings caused an unnecessary collection")
		}
		cfg.Routing.CodexStateCliproxy.Enabled = false
		keeper.Configure(cfg)
		synctest.Wait()
		if calls.Load() != 3 {
			t.Fatal("disabling the proxy unnecessarily replaced valid cached state")
		}
	})
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
	expires, errState := codexTurnStateExpiry(auth, token, time.Now())
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

func TestCodexTurnStateAutoDegradeAndRecover(t *testing.T) {
	for _, team := range []bool{false, true} {
		t.Run(strconv.FormatBool(team), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := cliproxyauth.NewManager(nil, nil, nil)
				auth := turnStateTestAuth("a")
				badSize, goodSize := 233, 217
				if team {
					auth.Metadata["plan_type"] = "business"
					badSize, goodSize = 265, 249
				}
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true), CodexAutoDegrade: new(true)}}
				manager.SetConfig(cfg)
				keeper := NewCodexTurnStateKeeper(manager.List)
				keeper.SetSchedulingUpdater(manager.ApplyCodexStateScheduling)
				defer keeper.Close()
				var calls atomic.Int32
				keeper.fetch = func(_ context.Context, _ *config.Config, _ *cliproxyauth.Auth) (string, error) {
					size := badSize
					if calls.Add(1) > 1 {
						size = goodSize
					}
					return testTurnStateBytes(time.Now(), size, 1), nil
				}
				keeper.Configure(cfg)
				synctest.Wait()
				current, _ := manager.GetByID(auth.ID)
				if !current.IsDegraded() || current.Disabled {
					t.Fatal("non-quality state must move only this account to the degraded pool")
				}
				if got := keeper.Snapshot()[0].ObservedLength; got != len(testTurnStateBytes(time.Now(), badSize, 1)) {
					t.Fatalf("observed length = %d", got)
				}
				time.Sleep(CodexTurnStateCheckInterval)
				synctest.Wait()
				current, _ = manager.GetByID(auth.ID)
				if current.IsDegraded() || calls.Load() != 2 {
					t.Fatal("degraded account was not probed and restored")
				}
				if token, _ := keeper.Token(current); len(token) != len(testTurnStateBytes(time.Now(), goodSize, 1)) {
					t.Fatal("restored account did not retain its good state")
				}
			})
		})
	}
}

func TestCodexTurnStateAutoDegradeRequiresBothSwitchesAndState(t *testing.T) {
	for _, tc := range []struct {
		name               string
		enabled, automatic bool
		state              string
		err                error
	}{
		{name: "collector disabled", automatic: true},
		{name: "automation disabled", enabled: true},
		{name: "network error", enabled: true, automatic: true, err: errors.New("connection failed")},
		{name: "missing state", enabled: true, automatic: true, state: ""},
		{name: "invalid state", enabled: true, automatic: true, state: "not-a-state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{turnStateTestAuth("a")} })
				defer keeper.Close()
				var updates atomic.Int32
				keeper.SetSchedulingUpdater(func(context.Context, *cliproxyauth.Auth, bool) error { updates.Add(1); return nil })
				keeper.fetch = func(context.Context, *config.Config, *cliproxyauth.Auth) (string, error) {
					if !tc.enabled || !tc.automatic {
						return testTurnState(time.Now(), true, 1), nil
					}
					return tc.state, tc.err
				}
				keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(tc.enabled), CodexAutoDegrade: new(tc.automatic)}})
				synctest.Wait()
				if updates.Load() != 0 {
					t.Fatal("account changed without both switches and a usable observation")
				}
			})
		})
	}
}

func TestCodexTurnStateAutoDegradeDisableDuringProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		keeper := NewCodexTurnStateKeeper(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{turnStateTestAuth("a")} })
		defer keeper.Close()
		release := make(chan struct{})
		var updates atomic.Int32
		keeper.SetSchedulingUpdater(func(context.Context, *cliproxyauth.Auth, bool) error { updates.Add(1); return nil })
		keeper.fetch = func(context.Context, *config.Config, *cliproxyauth.Auth) (string, error) {
			<-release
			return testTurnState(time.Now(), true, 1), nil
		}
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true), CodexAutoDegrade: new(true)}})
		synctest.Wait()
		keeper.Configure(&config.Config{Routing: config.RoutingConfig{CodexTurnStateEnabled: new(true), CodexAutoDegrade: new(false)}})
		close(release)
		synctest.Wait()
		if updates.Load() != 0 {
			t.Fatal("late probe applied a disabled automation policy")
		}
	})
}
