package helps

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func staticISPTestAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{
		"access_token": "access-" + id, "account_id": "account-" + id,
	}}
}

func staticISPTestConfig(count int) *config.Config {
	cfg := &config.Config{Routing: config.RoutingConfig{CodexStaticISP: &config.CodexStaticISPConfig{Enabled: true}}}
	for i := 0; i < count; i++ {
		cfg.Routing.CodexStaticISP.Nodes = append(cfg.Routing.CodexStaticISP.Nodes, config.StaticISPNode{ID: fmt.Sprintf("isp-%d", i), Host: "proxy.test", Port: 1080 + i, Username: "u", Password: "secret"})
	}
	return cfg
}

func TestCodexStaticISPStableBalancedBindings(t *testing.T) {
	var auths []*cliproxyauth.Auth
	for i := 0; i < 12; i++ {
		auths = append(auths, staticISPTestAuth(fmt.Sprintf("a-%02d", i)))
	}
	k := NewCodexEgressManager(func() []*cliproxyauth.Auth { return auths })
	t.Cleanup(k.Close)
	cfg := staticISPTestConfig(2)
	if err := k.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	bindings := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, auth := range auths {
			bound, err := k.BindEgress(auth)
			if err != nil {
				t.Fatal(err)
			}
			out[auth.ID] = bound.ProxyURL
			if auth.ProxyURL != "" || auth.Runtime != nil {
				t.Fatal("binding mutated the credential record")
			}
		}
		return out
	}
	before := bindings()
	counts := map[string]int{}
	for _, proxyURL := range before {
		counts[proxyURL]++
	}
	for _, count := range counts {
		if count != 6 {
			t.Fatal("initial bindings are not balanced")
		}
	}
	auths[0].Disabled = true
	auths[1].Metadata["degraded"] = true
	if err := k.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, bindings()) {
		t.Fatal("account scheduling state changed ISP bindings")
	}
	if err := k.Configure(staticISPTestConfig(3)); err != nil {
		t.Fatal(err)
	}
	after := bindings()
	changed := 0
	counts = map[string]int{}
	for id, proxyURL := range after {
		counts[proxyURL]++
		if before[id] != proxyURL {
			changed++
		}
	}
	if changed != 4 || len(counts) != 3 {
		t.Fatalf("added node moved %d accounts, want minimum 4", changed)
	}
	for _, count := range counts {
		if count != 4 {
			t.Fatal("expanded pool is not balanced")
		}
	}
	if err := k.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	removed := bindings()
	for id, proxyURL := range after {
		if proxyURL != removed[id] && (proxyURL == before["a-00"] || proxyURL == before["a-01"]) {
			t.Fatal("node removal moved an unaffected account")
		}
	}
}

func TestCodexStaticISPBindingPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-static-isp-bindings.json")
	auths := []*cliproxyauth.Auth{staticISPTestAuth("z"), staticISPTestAuth("b")}
	k := NewCodexEgressManager(func() []*cliproxyauth.Auth { return auths })
	t.Cleanup(k.Close)
	if err := k.SetStaticISPBindingFile(path); err != nil {
		t.Fatal(err)
	}
	cfg := staticISPTestConfig(2)
	if err := k.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	auths = append(auths, staticISPTestAuth("a"))
	_, err := k.BindEgress(auths[2])
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewCodexEgressManager(func() []*cliproxyauth.Auth { return auths })
	t.Cleanup(restarted.Close)
	if err := restarted.SetStaticISPBindingFile(path); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	for _, auth := range auths {
		old, _ := k.BindEgress(auth)
		got, err := restarted.BindEgress(auth)
		if err != nil || got.ProxyURL != old.ProxyURL {
			t.Fatal("restart changed a saved binding")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "proxy.test") {
		t.Fatal("binding snapshot failed or leaked proxy credentials")
	}
}

func TestCodexStaticISPHTTPUsesProxyWithoutFallbackOrProbes(t *testing.T) {
	var directCalls, proxyCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { directCalls.Add(1); w.WriteHeader(500) }))
	t.Cleanup(upstream.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxyCalls.Add(1)
		if req.Header.Get("Proxy-Authorization") == "" {
			t.Error("missing static ISP credentials")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	u, _ := url.Parse(proxy.URL)
	port, _ := strconv.Atoi(u.Port())
	cfg := staticISPTestConfig(1)
	cfg.ProxyURL = upstream.URL
	cfg.Routing.CodexStaticISP.Nodes[0].Scheme = "http"
	cfg.Routing.CodexStaticISP.Nodes[0].Host = u.Hostname()
	cfg.Routing.CodexStaticISP.Nodes[0].Port = port
	auth := staticISPTestAuth("a")
	auth.ProxyURL = upstream.URL
	auth.Attributes = map[string]string{"base_url": upstream.URL}
	k := NewCodexEgressManager(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
	t.Cleanup(k.Close)
	if err := k.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	bound, err := k.BindEgress(auth)
	if err != nil {
		t.Fatal(err)
	}
	if proxyCalls.Load() != 0 || directCalls.Load() != 0 {
		t.Fatal("configuring static ISP issued an upstream probe")
	}
	client := k.WrapHTTPClient(NewCodexHTTPClient(t.Context(), cfg, bound, 0), bound)
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if proxyCalls.Load() != 1 || directCalls.Load() != 0 {
		t.Fatal("inference did not use the fixed proxy")
	}
	proxy.Close()
	if response, err := client.Get(upstream.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("failed node fell back to a different route")
	}
	if directCalls.Load() != 0 {
		t.Fatal("failed proxy fell back to direct routing")
	}
}

func TestCodexStaticISPDefaultAndInvalidStore(t *testing.T) {
	auth := staticISPTestAuth("a")
	auth.ProxyURL = "http://existing.test:8080"
	k := NewCodexEgressManager(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
	t.Cleanup(k.Close)
	for _, cfg := range []*config.Config{{}, staticISPTestConfig(0)} {
		if err := k.Configure(cfg); err != nil {
			t.Fatal(err)
		}
		bound, err := k.BindEgress(auth)
		if err != nil || bound.ProxyURL != auth.ProxyURL {
			t.Fatal("empty or disabled pool changed existing routing")
		}
	}
	dir := filepath.Join(t.TempDir(), "runtime")
	path := filepath.Join(dir, "bindings.json")
	if err := k.SetStaticISPBindingFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := k.Configure(staticISPTestConfig(1)); err == nil {
		t.Fatal("unwritable binding store accepted")
	}
	if _, err := k.BindEgress(auth); err == nil {
		t.Fatal("binding failure allowed legacy egress")
	}
}

func TestCodexStaticISPDedicatedDirectoryAsNonRoot(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires a non-root POSIX user to enforce file permissions")
	}
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "runtime")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	auth := staticISPTestAuth("a")
	source := func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} }
	blocked := NewCodexEgressManager(source)
	t.Cleanup(blocked.Close)
	if err := blocked.SetStaticISPBindingFile(filepath.Join(dir, "bindings.json")); err != nil {
		t.Fatal(err)
	}
	if err := blocked.Configure(staticISPTestConfig(1)); err == nil {
		t.Fatal("test baseline did not enforce protected installation directory")
	}
	keeper := NewCodexEgressManager(source)
	t.Cleanup(keeper.Close)
	if err := keeper.SetStaticISPBindingFile(filepath.Join(stateDir, "bindings.json")); err != nil {
		t.Fatal(err)
	}
	if err := keeper.Configure(staticISPTestConfig(1)); err != nil {
		t.Fatalf("dedicated writable runtime directory failed: %v", err)
	}
	if _, err := keeper.BindEgress(auth); err != nil {
		t.Fatal(err)
	}
}

func TestCodexStaticISPRebindRejectsOldRequests(t *testing.T) {
	auth := staticISPTestAuth("a")
	k := NewCodexEgressManager(func() []*cliproxyauth.Auth { return nil })
	t.Cleanup(k.Close)
	cfg := staticISPTestConfig(1)
	if err := k.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	old, err := k.BindEgress(auth)
	if err != nil {
		t.Fatal(err)
	}
	client := k.WrapHTTPClient(NewCodexHTTPClient(t.Context(), cfg, old, 0), old)
	closed := new(staticISPTestCloser)
	if err := k.TrackEgressConnection(old, closed); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.CodexStaticISP.Nodes[0].Port++
	if err := k.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if !closed.closed {
		t.Fatal("rebind did not close the old connection")
	}
	if _, err := k.BindEgress(old); err == nil {
		t.Fatal("stale request snapshot was rebound instead of rejected")
	}
	if response, err := client.Get("http://upstream.invalid/responses"); err == nil {
		_ = response.Body.Close()
		t.Fatal("stale HTTP client was allowed to send a request")
	}
}

type staticISPTestCloser struct{ closed bool }

func (c *staticISPTestCloser) Close() error { c.closed = true; return nil }

func TestCodexStaticISPConcurrentReconfigure(t *testing.T) {
	auth := staticISPTestAuth("a")
	k := NewCodexEgressManager(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
	t.Cleanup(k.Close)
	if err := k.Configure(staticISPTestConfig(1)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			for j := 0; j < 30; j++ {
				bound, err := k.BindEgress(auth)
				if err != nil {
					t.Error(err)
					return
				}
				_ = CodexEgressFingerprint(bound)
				_ = k.StaticISPBindings()
			}
		})
	}
	for i := 0; i < 30; i++ {
		cfg := staticISPTestConfig(1)
		cfg.Routing.CodexStaticISP.Nodes[0].Port += i % 2
		if err := k.Configure(cfg); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
