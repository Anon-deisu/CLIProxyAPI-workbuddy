package executor

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexStaticISPWebsocketRouteAndRebind(t *testing.T) {
	accepted := make(chan *websocket.Conn, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		accepted <- conn
	}))
	t.Cleanup(upstream.Close)
	var firstCalls, secondCalls atomic.Int32
	newProxy := func(calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") == "" {
				t.Error("WebSocket did not use an authenticated static ISP tunnel")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			target, errDial := net.Dial("tcp", r.Host)
			if errDial != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			conn, _, errHijack := w.(http.Hijacker).Hijack()
			if errHijack != nil {
				_ = target.Close()
				return
			}
			_, _ = fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
			go func() { _, _ = io.Copy(target, conn); _ = target.Close(); _ = conn.Close() }()
			go func() { _, _ = io.Copy(conn, target); _ = target.Close(); _ = conn.Close() }()
		}))
	}
	p1, p2 := newProxy(&firstCalls), newProxy(&secondCalls)
	t.Cleanup(p1.Close)
	t.Cleanup(p2.Close)
	node := func(proxyURL string) config.StaticISPNode {
		u, _ := url.Parse(proxyURL)
		port, _ := strconv.Atoi(u.Port())
		return config.StaticISPNode{ID: "isp-a", Scheme: "http", Host: u.Hostname(), Port: port, Username: "u", Password: "p"}
	}
	auth := &cliproxyauth.Auth{ID: "a", Provider: "codex", ProxyURL: "http://unused.test:1234", Metadata: map[string]any{"access_token": "test", "account_id": "account-a"}}
	cfg := &config.Config{Routing: config.RoutingConfig{CodexStaticISP: &config.CodexStaticISPConfig{Enabled: true, Nodes: []config.StaticISPNode{node(p1.URL)}}}}
	keeper := helps.NewCodexEgressManager(func() []*cliproxyauth.Auth { return []*cliproxyauth.Auth{auth} })
	t.Cleanup(keeper.Close)
	if err := keeper.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	exec := NewCodexWebsocketsExecutor(cfg)
	exec.egress = keeper
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	t.Cleanup(func() { exec.CloseExecutionSession("isp-test") })
	sess := exec.getOrCreateSession("isp-test")
	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	old, err := keeper.BindEgress(auth)
	if err != nil {
		t.Fatal(err)
	}
	conn, closer, _, err := exec.ensureUpstreamConn(t.Context(), old, sess, auth.ID, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	serverConn := <-accepted
	defer func() { _ = serverConn.Close() }()
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatal("WebSocket used the wrong proxy")
	}
	cfg.Routing.CodexStaticISP.Nodes[0] = node(p2.URL)
	if err := keeper.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := serverConn.ReadMessage(); err == nil {
		t.Fatal("old websocket survived the proxy rebind")
	}
	if reused, _ := existingWebsocketSessionConnForEgress(sess, auth.ID, wsURL, old.ProxyURL, false, "different-binding"); reused != nil {
		t.Fatal("connection matched a different egress fingerprint")
	}
	if _, _, _, err := exec.ensureUpstreamConn(t.Context(), old, sess, auth.ID, wsURL, nil); err == nil {
		t.Fatal("old request was allowed to reconnect")
	}
	current, _ := keeper.BindEgress(auth)
	next, nextCloser, _, err := exec.ensureUpstreamConn(t.Context(), current, sess, auth.ID, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nextCloser.Close() }()
	nextServer := <-accepted
	defer func() { _ = nextServer.Close() }()
	if next == conn || secondCalls.Load() != 1 || firstCalls.Load() != 1 {
		t.Fatal("rebound request did not open a new connection through the new ISP")
	}
	p2.Close()
	if fresh, freshCloser, _, err := exec.dialCodexWebsocket(t.Context(), current, wsURL, nil); err == nil {
		_ = freshCloser.Close()
		_ = fresh.Close()
		t.Fatal("failed static ISP fell back to another WebSocket route")
	}
	if firstCalls.Load() != 1 || auth.ProxyURL != "http://unused.test:1234" || auth.Runtime != nil {
		t.Fatal("rebind changed credential material or fell back to the first ISP")
	}
}
