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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func TestWorkBuddyTaskServicePersistsWithoutOverwritingConcurrentChanges(t *testing.T) {
	reportStarted := make(chan struct{})
	releaseReport := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseReport) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
		case "/v2/report":
			close(reportStarted)
			select {
			case <-releaseReport:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			t.Errorf("unexpected task endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := workbuddy.NewAuthRecord(workbuddy.ProviderAI, workbuddy.Credentials{UID: "uid", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	record.Metadata["base_url"] = upstream.URL
	auth, errRegister := manager.Register(t.Context(), record)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	service := NewWorkBuddyTaskService(&config.Config{}, manager)
	defer service.Close()
	if !service.execute(auth, false, true, "manual") {
		t.Fatal("task execution did not start")
	}
	<-reportStarted
	done, acquired := service.acquire(auth.ID)
	if acquired || service.execute(auth, false, true, "manual") {
		t.Fatal("duplicate task execution started")
	}
	_, _, errModify := manager.Modify(t.Context(), auth.ID, auth.RegistrationEpoch, func(current *coreauth.Auth) error {
		current.Disabled = true
		current.Prefix = "changed-during-task"
		current.Metadata["access_token"] = "new-access"
		return nil
	})
	if errModify != nil {
		t.Fatal(errModify)
	}
	releaseOnce.Do(func() { close(releaseReport) })
	<-done
	status := service.Cached(auth)
	if status.Running || status.LastRun == nil || !status.LastRun.OK || !status.Activity.Completed {
		t.Fatalf("completed task status is stale: %+v", status)
	}
	current, _ := manager.GetByID(auth.ID)
	if !current.Disabled || current.Prefix != "changed-during-task" || current.Metadata["access_token"] != "new-access" || current.Metadata["workbuddy_last_activity_at"] == nil {
		t.Fatal("task persistence overwrote concurrent account changes or lost activity")
	}
}

func TestWorkBuddyTaskServiceCloseWaitsForActiveTasks(t *testing.T) {
	service := NewWorkBuddyTaskService(&config.Config{}, nil)
	done, acquired := service.acquire("account")
	if !acquired {
		t.Fatal("task slot was not acquired")
	}
	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	<-service.ctx.Done()
	select {
	case <-closed:
		t.Fatal("service closed before its task finished")
	default:
	}
	service.release("account", done)
	<-closed
	if _, acquired := service.acquire("another-account"); acquired {
		t.Fatal("closed service accepted a new task")
	}
}

func TestWorkBuddyAutomaticCheckinsRespectConfiguredInterval(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[stop], func(t *testing.T) {
			var checkins, requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/v2/billing/meter/checkin-activity-status":
					_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":false}}`))
				case "/v2/billing/meter/daily-checkin":
					checkins.Add(1)
					_, _ = w.Write([]byte(`{"code":0,"msg":"checked","data":{"credit":30}}`))
				case "/activity/growth/streak":
					_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
				case "/v2/activity/growth/tasks":
					_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer upstream.Close()
			manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
			var accounts []*coreauth.Auth
			for _, uid := range []string{"first", "second"} {
				auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: uid, AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
				auth.Metadata["base_url"], auth.Metadata["billing_url"] = upstream.URL, upstream.URL
				registered, errRegister := manager.Register(t.Context(), auth)
				if errRegister != nil {
					t.Fatal(errRegister)
				}
				accounts = append(accounts, registered)
			}
			service := NewWorkBuddyTaskService(&config.Config{Routing: config.RoutingConfig{WorkBuddyAutoCheckin: new(true), WorkBuddyCheckinIntervalSeconds: new(12)}}, manager)
			defer service.Close()
			waits := 0
			service.wait = func(ctx context.Context, interval time.Duration) error {
				waits++
				if interval != 12*time.Second || checkins.Load() != 1 {
					t.Fatalf("accounts were not spaced: interval=%v checkins=%d", interval, checkins.Load())
				}
				if stop {
					service.cancel()
					return ctx.Err()
				}
				return nil
			}
			service.runAutomatic()
			want := int32(2)
			if stop {
				want = 1
			}
			if waits != 1 || checkins.Load() != want {
				t.Fatalf("unexpected batch: waits=%d checkins=%d", waits, checkins.Load())
			}
			if !stop {
				for _, auth := range accounts {
					current, _ := manager.GetByID(auth.ID)
					if !service.Cached(current).Checkin.CheckedIn {
						t.Fatal("confirmed checkin regressed while upstream status was stale")
					}
				}
				beforeRequests := requests.Load()
				beforeRun := service.Cached(accounts[0]).LastRun
				service.runAutomatic()
				if requests.Load() != beforeRequests || waits != 1 {
					t.Fatal("completed accounts performed another automatic task cycle")
				}
				if service.execute(accounts[0], true, false, "auto") {
					t.Fatal("a stale account snapshot restarted a completed checkin")
				}
				afterRun := service.Cached(accounts[0]).LastRun
				if afterRun.StartedAt != beforeRun.StartedAt || afterRun.EarnedCredit != beforeRun.EarnedCredit {
					t.Fatal("a skipped checkin replaced the successful task result")
				}
				restarted := NewWorkBuddyTaskService(&config.Config{Routing: config.RoutingConfig{WorkBuddyAutoCheckin: new(true)}}, manager)
				defer restarted.Close()
				restarted.runAutomatic()
				if requests.Load() != beforeRequests {
					t.Fatal("service restart forgot persisted checkin completion")
				}
				current, _ := manager.GetByID(accounts[0].ID)
				_, _, errModify := manager.Modify(t.Context(), current.ID, current.RegistrationEpoch, func(updated *coreauth.Auth) error {
					updated.Metadata["workbuddy_last_checkin_at"] = time.Now().AddDate(0, 0, -1).UTC().Format(time.RFC3339)
					return nil
				})
				if errModify != nil {
					t.Fatal(errModify)
				}
				restarted.runAutomatic()
				if checkins.Load() != 3 {
					t.Fatal("a previous-day checkin prevented the next daily checkin")
				}
			}
		})
	}
}

func TestWorkBuddyAutomaticCheckinPreservesEnabledGrowth(t *testing.T) {
	var checkins, reports atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":false}}`))
		case "/v2/billing/meter/daily-checkin":
			checkins.Add(1)
			_, _ = w.Write([]byte(`{"code":0}`))
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
		case "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
		case "/v2/report":
			reports.Add(1)
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: "uid", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	auth.Metadata["base_url"], auth.Metadata["billing_url"] = upstream.URL, upstream.URL
	auth.Metadata["workbuddy_last_checkin_at"] = time.Now().UTC().Format(time.RFC3339)
	registered, errRegister := manager.Register(t.Context(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	service := NewWorkBuddyTaskService(&config.Config{Routing: config.RoutingConfig{WorkBuddyAutoCheckin: new(true), WorkBuddyAutoTasks: new(true)}}, manager)
	defer service.Close()
	service.runAutomatic()
	result := service.Cached(registered).LastRun
	if checkins.Load() != 0 || reports.Load() != 1 || result == nil || !result.OK {
		t.Fatalf("checkin suppression stopped enabled growth: checkins=%d reports=%d result=%+v", checkins.Load(), reports.Load(), result)
	}
	for _, task := range result.Results {
		if task.Code == "checkin" {
			t.Fatal("automatic execution re-enabled an already completed checkin")
		}
	}
}

func TestWorkBuddyAutomaticCheckinRemembersUpstreamCompletion(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
		case "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
		default:
			t.Errorf("completed upstream checkin made an unexpected request: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	auth := workbuddy.NewAuthRecord(workbuddy.ProviderCN, workbuddy.Credentials{UID: "uid", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	auth.Metadata["base_url"], auth.Metadata["billing_url"] = upstream.URL, upstream.URL
	registered, errRegister := manager.Register(t.Context(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	service := NewWorkBuddyTaskService(&config.Config{Routing: config.RoutingConfig{WorkBuddyAutoCheckin: new(true)}}, manager)
	defer service.Close()
	service.runAutomatic()
	current, _ := manager.GetByID(registered.ID)
	if !workbuddy.CheckinCompletedToday(current, time.Now()) {
		t.Fatal("upstream checkin completion was not persisted")
	}
	before := requests.Load()
	service.runAutomatic()
	if requests.Load() != before {
		t.Fatal("confirmed upstream checkin was queried by another automatic cycle")
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
