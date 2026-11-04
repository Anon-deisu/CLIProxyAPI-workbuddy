package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type fakeWorkBuddyOAuth struct {
	ready chan struct{}
	done  chan struct{}
}

func TestWorkBuddyQuotaRefreshesThroughManagerAndKeepsRoutingState(t *testing.T) {
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		t.Run(provider, func(t *testing.T) {
			var refreshes, reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/plugin/auth/token/refresh":
					refreshes.Add(1)
					_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`))
				case "/billing/meter/get-user-resource-summary":
					reads.Add(1)
					if r.Header.Get("Authorization") != "Bearer new-access" || r.Header.Get("X-User-Id") != "uid" {
						t.Error("quota did not use the refreshed identity")
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[{"CycleTotalCapacity":"100","CycleUsedCapacity":"40.5"}]}}`))
				default:
					t.Errorf("unexpected upstream path %s", r.URL.Path)
				}
			}))
			defer upstream.Close()
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			manager.RegisterExecutor(executor.NewWorkBuddyExecutor(&config.Config{}, provider))
			record := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "uid", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()})
			record.Metadata["base_url"] = upstream.URL
			record.Prefix = "keep-prefix"
			record.Disabled = true
			auth, err := manager.Register(t.Context(), record)
			if err != nil {
				t.Fatal(err)
			}
			h := &Handler{cfg: &config.Config{}, authManager: manager}
			router := gin.New()
			router.GET("/quota", h.GetWorkBuddyQuota)
			var requests sync.WaitGroup
			for range 4 {
				requests.Go(func() {
					w := httptest.NewRecorder()
					router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quota?auth_index="+auth.Index, nil))
					var quota workbuddy.Quota
					if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &quota) != nil || quota.Credits == nil || quota.Credits.Remaining != 59.5 {
						t.Errorf("quota query failed: %d %s", w.Code, w.Body.String())
					}
					var fields map[string]any
					_ = json.Unmarshal(w.Body.Bytes(), &fields)
					if fields["access_token"] != nil || fields["refresh_token"] != nil {
						t.Error("quota response leaked credentials")
					}
				})
			}
			requests.Wait()
			live, _ := manager.GetByID(auth.ID)
			saved, err := store.List(t.Context())
			if err != nil || len(saved) != 1 || saved[0].Metadata["refresh_token"] != "new-refresh" || refreshes.Load() != 1 || reads.Load() != 4 || !live.Disabled || live.Prefix != "keep-prefix" {
				t.Fatal("quota refresh was not persisted or changed account routing state")
			}
		})
	}
}

func TestWorkBuddyQuotaRejectsMissingAndUnrelatedCredentials(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	unrelated, err := manager.Register(t.Context(), &coreauth.Auth{ID: "other", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.GET("/quota", h.GetWorkBuddyQuota)
	for _, test := range []struct {
		query  string
		status int
	}{{"", http.StatusBadRequest}, {"?auth_index=missing", http.StatusNotFound}, {"?auth_index=" + unrelated.Index, http.StatusBadRequest}} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quota"+test.query, nil))
		if w.Code != test.status {
			t.Fatalf("quota%s status = %d, want %d", test.query, w.Code, test.status)
		}
	}
}

func (f *fakeWorkBuddyOAuth) Start(context.Context) (*workbuddy.LoginSession, error) {
	return &workbuddy.LoginSession{State: "private-upstream-state", URL: "https://login.example/workbuddy"}, nil
}

func (f *fakeWorkBuddyOAuth) Wait(ctx context.Context, _ *workbuddy.LoginSession) (workbuddy.Credentials, error) {
	defer close(f.done)
	select {
	case <-ctx.Done():
		return workbuddy.Credentials{}, ctx.Err()
	case <-f.ready:
		return workbuddy.Credentials{UID: "test-uid", AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil
	}
}

func TestWorkBuddyManagementOAuthPersistsAndCancelsBothSites(t *testing.T) {
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		for _, cancelLogin := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "/save", true: "/cancel"}[cancelLogin], func(t *testing.T) {
				authDir := t.TempDir()
				h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
				service := &fakeWorkBuddyOAuth{ready: make(chan struct{}), done: make(chan struct{})}
				original := newWorkBuddyOAuthService
				newWorkBuddyOAuthService = func(_ *config.Config, selected string) workBuddyOAuthService {
					if selected != provider {
						t.Errorf("provider = %s", selected)
					}
					return service
				}
				t.Cleanup(func() { newWorkBuddyOAuthService = original })
				saved := make(chan *coreauth.Auth, 1)
				h.SetPostAuthPersistHook(func(_ context.Context, record *coreauth.Auth) error { saved <- record; return nil })
				router := gin.New()
				router.GET("/oauth/auth-url", h.StartOAuthV8)
				requestCtx, cancelRequest := context.WithCancel(t.Context())
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/oauth/auth-url?provider="+provider, nil).WithContext(requestCtx))
				cancelRequest()
				if w.Code != http.StatusOK {
					t.Fatalf("start: %d %s", w.Code, w.Body.String())
				}
				var start struct{ State, URL, Flow string }
				if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
					t.Fatal(err)
				}
				defer CancelOAuthSession(start.State)
				if start.State == "private-upstream-state" || start.State == "" || start.Flow != "device" || !IsOAuthSessionPending(start.State, provider) {
					t.Fatal("management session did not isolate its upstream state or site")
				}
				if cancelLogin {
					CancelOAuthSession(start.State)
				}
				close(service.ready)
				select {
				case <-service.done:
				case <-time.After(5 * time.Second):
					t.Fatal("authorization did not finish")
				}
				if cancelLogin {
					entries, err := os.ReadDir(authDir)
					if err != nil || len(entries) != 0 {
						t.Fatal("cancelled login wrote credentials")
					}
					return
				}
				select {
				case record := <-saved:
					data, err := os.ReadFile(filepath.Join(authDir, record.ID))
					if err != nil {
						t.Fatal(err)
					}
					var stored map[string]any
					_ = json.Unmarshal(data, &stored)
					if stored["type"] != provider || stored["auth_kind"] != "oauth" || stored["access_token"] != "test-access" || stored["refresh_token"] != "test-refresh" {
						t.Fatal("saved credentials are incomplete or belong to the wrong site")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("login inherited cancelled HTTP request or failed to save")
				}
			})
		}
	}
}
