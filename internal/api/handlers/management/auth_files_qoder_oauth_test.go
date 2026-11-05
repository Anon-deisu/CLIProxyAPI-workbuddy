package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeQoderOAuth struct {
	session  *qoder.LoginSession
	creds    qoder.Credentials
	startErr error
	waitErr  error
	ready    chan struct{}
	begun    chan context.Context
	onWait   func()
}

func (f *fakeQoderOAuth) Start(context.Context) (*qoder.LoginSession, error) {
	return f.session, f.startErr
}

func (f *fakeQoderOAuth) Wait(ctx context.Context, session *qoder.LoginSession) (qoder.Credentials, error) {
	if session != f.session {
		return qoder.Credentials{}, errors.New("wrong login session")
	}
	if f.begun != nil {
		f.begun <- ctx
	}
	if f.ready != nil {
		select {
		case <-ctx.Done():
			return qoder.Credentials{}, ctx.Err()
		case <-f.ready:
		}
	}
	if f.onWait != nil {
		f.onWait()
	}
	return f.creds, f.waitErr
}

func qoderTestService(provider string) *fakeQoderOAuth {
	return &fakeQoderOAuth{
		session: &qoder.LoginSession{URL: "https://login.example/" + provider, State: "private-upstream-state", Verifier: "private-verifier", Nonce: "private-nonce", MachineID: "private-machine"},
		creds:   qoder.Credentials{UID: "9007199254740993", Nickname: "Test account", AccessToken: "dt-test-access", RefreshToken: "drt-test-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}
}

func qoderTestRequest(t *testing.T, router http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func qoderWaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Qoder authorization worker did not finish")
	}
}

func TestQoderManagementOAuthStartsIsolatedDeviceSessions(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			authDir := t.TempDir()
			h := &Handler{cfg: &config.Config{AuthDir: authDir}, tokenStore: sdkauth.NewFileTokenStore()}
			original := newQoderOAuthService
			var services []*fakeQoderOAuth
			newQoderOAuthService = func(_ *config.Config, selected string) qoderOAuthService {
				if selected != provider {
					t.Errorf("provider = %s, want %s", selected, provider)
				}
				service := qoderTestService(selected)
				service.ready, service.begun = make(chan struct{}), make(chan context.Context, 1)
				services = append(services, service)
				return service
			}
			t.Cleanup(func() { newQoderOAuthService = original })
			router := gin.New()
			if provider == qoder.ProviderCN {
				router.GET("/start", h.RequestQoderCNToken)
			} else {
				router.GET("/start", h.RequestQoderToken)
			}
			router.GET("/status", h.GetAuthStatus)
			router.DELETE("/cancel", h.CancelAuthSession)
			var states []string
			var pollContexts []context.Context
			for range 2 {
				requestCtx, cancelRequest := context.WithCancel(t.Context())
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/start", nil).WithContext(requestCtx))
				cancelRequest()
				var result struct {
					State, URL, Flow string
					ExpiresIn        int `json:"expires_in"`
				}
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Flow != "device" || result.ExpiresIn != 600 || result.URL != "https://login.example/"+provider {
					t.Fatalf("invalid start response: %d %s", w.Code, w.Body.String())
				}
				if result.State == "" || strings.Contains(w.Body.String(), "private-") || !IsOAuthSessionPending(result.State, provider) {
					t.Fatal("management state is not isolated from the upstream session")
				}
				other := qoder.ProviderCN
				if provider == other {
					other = qoder.ProviderIntl
				}
				if IsOAuthSessionPending(result.State, other) {
					t.Fatal("session was not bound to its regional provider")
				}
				t.Cleanup(func() { CancelOAuthSession(result.State) })
				states = append(states, result.State)
				select {
				case ctx := <-services[len(services)-1].begun:
					pollContexts = append(pollContexts, ctx)
				case <-time.After(5 * time.Second):
					t.Fatal("polling did not start")
				}
				status := qoderTestRequest(t, router, http.MethodGet, "/status?state="+result.State)
				if !strings.Contains(status.Body.String(), `"status":"wait"`) {
					t.Fatalf("session is not pending: %s", status.Body.String())
				}
			}
			if states[0] == states[1] {
				t.Fatal("independent logins reused management state")
			}
			cancelled := qoderTestRequest(t, router, http.MethodDelete, "/cancel?state="+states[0])
			if !strings.Contains(cancelled.Body.String(), `"cancelled":true`) || !IsOAuthSessionPending(states[1], provider) {
				t.Fatal("cancelling one session affected another")
			}
			close(services[0].ready)
			qoderWaitDone(t, pollContexts[0].Done())
			entries, errRead := os.ReadDir(authDir)
			if errRead != nil || len(entries) != 0 {
				t.Fatal("cancelled login saved credentials")
			}
			close(services[1].ready)
			qoderWaitDone(t, pollContexts[1].Done())
			status := qoderTestRequest(t, router, http.MethodGet, "/status?state="+states[1])
			if !strings.Contains(status.Body.String(), `"status":"ok"`) {
				t.Fatalf("login failed after the HTTP request ended: %s", status.Body.String())
			}
			record := qoder.NewAuthRecord(provider, services[1].creds)
			data, errRead := os.ReadFile(filepath.Join(authDir, record.FileName))
			if errRead != nil {
				t.Fatal(errRead)
			}
			var stored map[string]any
			if errDecode := json.Unmarshal(data, &stored); errDecode != nil || stored["type"] != provider || stored["nickname"] != "Test account" || stored["uid"] != services[1].creds.UID || stored["access_token"] != "dt-test-access" || stored["refresh_token"] != "drt-test-refresh" || stored["expires_at"] != float64(services[1].creds.ExpiresAt) {
				t.Fatal("persisted credentials lost the site, identity or seconds expiry")
			}
		})
	}
}

func TestQoderManagementOAuthErrorsAndSaveGuard(t *testing.T) {
	for _, scenario := range []string{"start-error", "nil-session", "wait-error", "missing-token", "missing-uid", "save-error", "cancel-during-wait", "wrong-provider", "cancel-wait-context"} {
		t.Run(scenario, func(t *testing.T) {
			service := qoderTestService(qoder.ProviderIntl)
			store := &memoryAuthStore{}
			h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}, tokenStore: store}
			state := "qoder-test-" + scenario
			RegisterOAuthSession(state, qoder.ProviderIntl)
			t.Cleanup(func() { CancelOAuthSession(state) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "start-error", "nil-session":
				if scenario == "start-error" {
					service.startErr = errors.New("private-verifier")
				} else {
					service.session = nil
				}
				original := newQoderOAuthService
				newQoderOAuthService = func(*config.Config, string) qoderOAuthService { return service }
				t.Cleanup(func() { newQoderOAuthService = original })
				router := gin.New()
				router.GET("/start", h.RequestQoderToken)
				w := qoderTestRequest(t, router, http.MethodGet, "/start")
				if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "private-") {
					t.Fatal("start failure was not sanitized")
				}
				return
			case "wait-error":
				service.waitErr = errors.New("private-verifier")
			case "missing-token":
				service.creds.AccessToken = " "
			case "missing-uid":
				service.creds.UID = " "
			case "save-error":
				h.postAuthHook = func(context.Context, *coreauth.Auth) error { return errors.New("private-access-token") }
			case "cancel-during-wait":
				service.onWait = func() { CancelOAuthSession(state) }
			case "wrong-provider":
				RegisterOAuthSession(state, qoder.ProviderCN)
			case "cancel-wait-context":
				service.onWait = cancel
			}
			h.waitQoderOAuth(ctx, service, service.session, state, qoder.ProviderIntl)
			items, errList := store.List(t.Context())
			if errList != nil || len(items) != 0 {
				t.Fatal("invalid or cancelled login saved credentials")
			}
			_, status, _, _, completed, exists := GetOAuthSessionDetails(state)
			if completed || strings.Contains(status, "private-") {
				t.Fatal("failed login completed or exposed upstream details")
			}
			if scenario != "wrong-provider" && scenario != "cancel-during-wait" && (!exists || status == "") {
				t.Fatal("failed login did not expose an error status")
			}
		})
	}
}

func TestQoderQuotaPreparerUsesUnixSeconds(t *testing.T) {
	preparer := qoderQuotaPreparer{}
	for _, test := range []struct {
		expires int64
		want    bool
	}{{0, false}, {time.Now().Add(-time.Hour).Unix(), true}, {time.Now().Add(3 * time.Minute).Unix(), true}, {time.Now().Add(5 * time.Minute).Unix(), false}, {time.Now().Add(time.Hour).Unix(), false}} {
		auth := qoder.NewAuthRecord(qoder.ProviderIntl, qoder.Credentials{UID: "uid", ExpiresAt: test.expires})
		if got := preparer.ShouldPrepareRequestAuth(auth); got != test.want {
			t.Errorf("expiry %d: should prepare = %v, want %v", test.expires, got, test.want)
		}
	}
	if preparer.ShouldPrepareRequestAuth(nil) {
		t.Fatal("nil auth needs preparation")
	}
}

func TestQoderQuotaRefreshesOncePersistsAndUsesNewToken(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			var refreshes, reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/deviceToken/refresh":
					refreshes.Add(1)
					var request map[string]string
					if json.NewDecoder(r.Body).Decode(&request) != nil || request["refresh_token"] != "drt-old" {
						t.Error("refresh did not use the account's refresh token")
					}
					_, _ = w.Write([]byte(`{"token":"dt-new","refresh_token":"drt-new","expires_in":3600000}`))
				case "/api/v2/quota/usage", "/api/v2/user/plan":
					if r.Header.Get("Authorization") != "Bearer dt-new" || r.Header.Get("Origin") != qoder.SiteForProvider(provider).Website {
						t.Error("quota did not use refreshed credentials for the selected site")
					}
					if r.URL.Path == "/api/v2/user/plan" {
						_, _ = w.Write([]byte(`{"plan_tier_name":"Pro"}`))
						return
					}
					reads.Add(1)
					_, _ = w.Write([]byte(`{"userQuota":{"total":100,"used":40.5,"remaining":59.5},"addOnQuota":{"total":20,"used":5,"remaining":15},"orgResourcePackage":{"cap":-1,"used":0,"remaining":999,"available":true}}`))
				default:
					t.Errorf("unexpected upstream path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			record := qoder.NewAuthRecord(provider, qoder.Credentials{UID: "uid", Nickname: "Nickname", AccessToken: "dt-old", RefreshToken: "drt-old", ExpiresAt: time.Now().Add(-time.Hour).Unix()})
			record.Metadata["base_url"] = upstream.URL
			record.Metadata["qoder_auto_checkin"] = true
			record.Prefix, record.Disabled, record.ProxyURL = "keep-prefix", true, "direct"
			auth, errRegister := manager.Register(t.Context(), record)
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			h := &Handler{cfg: &config.Config{}, authManager: manager}
			router := gin.New()
			router.GET("/quota", h.GetQoderQuota)
			var requests sync.WaitGroup
			for range 4 {
				requests.Go(func() {
					w := qoderTestRequest(t, router, http.MethodGet, "/quota?auth_index="+auth.Index)
					var quota qoder.Quota
					if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &quota) != nil || quota.Credits == nil || quota.Credits.Remaining != 74.5 || quota.Plan != "Pro" || quota.Realm != qoder.SiteForProvider(provider).Realm || quota.Shared == nil || !quota.Shared.Unlimited || len(quota.Packages) != 2 {
						t.Errorf("quota failed: %d %s", w.Code, w.Body.String())
					}
					for _, private := range []string{"dt-new", "drt-new", "dt-old", "access_token", "refresh_token"} {
						if strings.Contains(w.Body.String(), private) {
							t.Error("quota response exposed credentials")
						}
					}
				})
			}
			requests.Wait()
			live, _ := manager.GetByID(auth.ID)
			saved, errList := store.List(t.Context())
			if errList != nil || len(saved) != 1 || saved[0].Metadata["refresh_token"] != "drt-new" || refreshes.Load() != 1 || reads.Load() != 4 || live.Prefix != "keep-prefix" || !live.Disabled || live.ProxyURL != "direct" || live.Metadata["qoder_auto_checkin"] != true || live.Label != "Nickname" {
				t.Fatal("quota preparation did not persist once or changed account settings")
			}
		})
	}
}

func TestQoderQuotaRejectsInvalidAccountsAndSanitizesFailures(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	unrelated, _ := manager.Register(t.Context(), &coreauth.Auth{ID: "other", Provider: "codex"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"private-secret"}`))
	}))
	defer server.Close()
	for _, expiry := range []time.Duration{-time.Hour, time.Hour} {
		record := qoder.NewAuthRecord(qoder.ProviderIntl, qoder.Credentials{UID: expiry.String(), AccessToken: "private-secret", RefreshToken: "drt-private-secret", ExpiresAt: time.Now().Add(expiry).Unix()})
		record.Metadata["base_url"] = server.URL
		_, _ = manager.Register(t.Context(), record)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.GET("/quota", h.GetQoderQuota)
	for _, test := range []struct {
		query  string
		status int
	}{{"", http.StatusBadRequest}, {"?auth_index=%20", http.StatusBadRequest}, {"?auth_index=missing", http.StatusNotFound}, {"?auth_index=" + unrelated.Index, http.StatusBadRequest}} {
		if w := qoderTestRequest(t, router, http.MethodGet, "/quota"+test.query); w.Code != test.status {
			t.Errorf("quota%s: status = %d, want %d", test.query, w.Code, test.status)
		}
	}
	for _, record := range manager.List() {
		if record.Provider != qoder.ProviderIntl {
			continue
		}
		w := qoderTestRequest(t, router, http.MethodGet, "/quota?auth_index="+record.Index)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "private-secret") {
			t.Fatal("quota or refresh failure exposed upstream credentials")
		}
	}
	h.authManager = nil
	if w := qoderTestRequest(t, router, http.MethodGet, "/quota?auth_index=missing"); w.Code != http.StatusServiceUnavailable {
		t.Fatal("missing manager must return service unavailable")
	}
}
