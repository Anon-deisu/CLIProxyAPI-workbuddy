package qodercheckin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const campaignList = `{"campaigns":[{"campaignId":"daily","claimStatus":"CLAIMABLE","actionType":"CLAIM_BENEFIT","benefit":{"kind":"CREDITS","amount":100}}]}`
const campaignClaim = `{"status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`

type checkInTransport func(*http.Request) (*http.Response, error)

func (f checkInTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func checkInResponse(code int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func testCheckInAuth(provider string) *coreauth.Auth {
	auth := qoder.NewAuthRecord(provider, qoder.Credentials{UID: "test-uid", AccessToken: "dt-old", RefreshToken: "drt-old"})
	auth.Metadata[AutoCheckInKey] = true
	auth.Metadata[qoder.MachineIdentityMetadataKey] = map[string]any{
		"machineToken": "native-test-token", "machineType": "3", "machineCode": "native-test-code",
		"machineOS": "x86_64_linux", "hostname": "test-host",
	}
	return auth
}

func registerCheckIn(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) *coreauth.Auth {
	t.Helper()
	registered, err := manager.Register(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	return registered
}

func testCheckInTime() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, checkInZone) }

func TestQoderCheckInConcurrentManualAutoAndCancel(t *testing.T) {
	t.Setenv("QODER_RUNTIME_INFO_PATH", "")
	manager := coreauth.NewManager(nil, nil, nil)
	auth := registerCheckIn(t, manager, testCheckInAuth(qoder.ProviderIntl))
	duplicate := auth.Clone()
	duplicate.ID, duplicate.FileName = "duplicate.json", "duplicate.json"
	duplicate = registerCheckIn(t, manager, duplicate)
	var gets, posts atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			gets.Add(1)
			return checkInResponse(200, campaignList)
		}
		if posts.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return checkInResponse(200, campaignClaim)
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}))
	now := func() time.Time { return testCheckInTime() }
	results := make(chan runResult, 13)
	go func() {
		got, err := run(ctx, manager, auth, &config.Config{}, false, now)
		results <- runResult{got, err}
	}()
	waitCheckIn(t, entered)
	for i := range 12 {
		go func() {
			selected := auth
			if i%3 == 0 {
				selected = duplicate
			}
			got, err := run(ctx, manager, selected, &config.Config{}, i%2 == 0, now)
			results <- runResult{got, err}
		}()
	}
	waitCtx, cancel := context.WithCancel(ctx)
	observed := &observedCheckInContext{Context: waitCtx, waiting: make(chan struct{})}
	cancelled := make(chan runResult, 1)
	go func() {
		got, err := run(observed, manager, auth, &config.Config{}, false, now)
		cancelled <- runResult{got, err}
	}()
	waitCheckIn(t, observed.waiting)
	cancel()
	if got := receiveCheckIn(t, cancelled); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("waiting caller was not cancelled: %v", got.err)
	}
	releaseOnce.Do(func() { close(release) })
	claimed, already, credits := 0, 0, 0.0
	for range 13 {
		got := receiveCheckIn(t, results)
		if got.err != nil {
			t.Fatal(got.err)
		}
		claimed += got.result.Claimed
		already += got.result.Already
		credits += got.result.Credits
	}
	if claimed != 1 || already != 12 || credits != 100 || gets.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("claimed=%d already=%d credits=%v GET=%d POST=%d", claimed, already, credits, gets.Load(), posts.Load())
	}
	live, _ := manager.GetByID(auth.ID)
	if Eligible(live, now()) || metadataSeconds(live.Metadata[RoundKey]) != roundStart(now()) {
		t.Fatal("successful round was not saved")
	}
	accountLocks.Lock()
	_, leaked := accountLocks.entries[lockKey(manager, auth)]
	accountLocks.Unlock()
	if leaked {
		t.Fatal("completed account lock retained manager/account")
	}
}

type runResult struct {
	result CheckInResult
	err    error
}

type observedCheckInContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedCheckInContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func waitCheckIn(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("check-in synchronization did not complete")
	}
}

func receiveCheckIn(t *testing.T, done <-chan runResult) runResult {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("check-in did not complete")
		return runResult{}
	}
}

func TestQoderCheckInEligibilityWindowAndCycle(t *testing.T) {
	auth := testCheckInAuth(qoder.ProviderCN)
	boundary := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{boundary.Add(-time.Second), false}, {boundary, true}, {boundary.Add(13*time.Hour + 59*time.Minute), true},
		{boundary.Add(14 * time.Hour), false}, {boundary.In(time.FixedZone("other", -7*3600)), true},
	} {
		if got := Eligible(auth, tc.at); got != tc.want {
			t.Errorf("at=%s eligible=%v want=%v", tc.at, got, tc.want)
		}
	}
	for _, disabledField := range []string{"disabled", "auto", "status", "opt-out", "string-true", "provider"} {
		copy := auth.Clone()
		switch disabledField {
		case "disabled":
			copy.Disabled = true
		case "auto":
			copy.AutoDisabled = true
		case "status":
			copy.Status = coreauth.StatusDisabled
		case "opt-out":
			delete(copy.Metadata, AutoCheckInKey)
		case "string-true":
			copy.Metadata[AutoCheckInKey] = "true"
		case "provider":
			copy.Provider = "codex"
		}
		if Eligible(copy, boundary) {
			t.Errorf("ineligible account accepted: %s", disabledField)
		}
	}
	if Eligible(nil, boundary) {
		t.Fatal("nil auth eligible")
	}
	auth.Metadata[LastSuccessKey] = boundary.Add(-time.Hour).Unix()
	auth.Metadata[RoundKey] = roundStart(boundary.Add(-time.Hour))
	if !Eligible(auth, boundary) {
		t.Fatal("yesterday's round suppressed today's window")
	}
	auth.Metadata[RoundKey] = boundary.Unix()
	if Eligible(auth, boundary) {
		t.Fatal("completed round eligible")
	}
	delete(auth.Metadata, RoundKey)
	auth.Metadata[LastSuccessKey] = boundary.Format(time.RFC3339)
	if Eligible(auth, boundary) {
		t.Fatal("RFC3339 success ignored")
	}
	auth.Metadata[LastSuccessKey] = 0
	auth.Metadata[NextAttemptKey] = json.Number("1793588400")
	if Eligible(auth, boundary) {
		t.Fatal("future retry ignored")
	}
}

func TestQoderCheckInFailuresPersistAndRetryAtLeastOneHour(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
		code               int
		wantErr            bool
	}{
		{"blocked", `{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}`, "blocked", 200, false},
		{"error", `{"error":"private-secret"}`, "error", 503, true},
		{"empty", `{"campaigns":[]}`, "skipped", 200, false},
		{"hidden", `{"showCampaign":false,"campaigns":[]}`, "unavailable", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			auth := registerCheckIn(t, manager, testCheckInAuth(qoder.ProviderCN))
			clock := testCheckInTime()
			var calls atomic.Int32
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if tc.name == "blocked" && req.Method == http.MethodGet {
					return checkInResponse(200, campaignList)
				}
				return checkInResponse(tc.code, tc.body)
			}))
			got, err := run(ctx, manager, auth, &config.Config{}, false, func() time.Time { return clock })
			if (err != nil) != tc.wantErr || got.Status != tc.status {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			before := calls.Load()
			for _, automatic := range []bool{true, false} {
				repeated, errRepeat := run(ctx, manager, auth, &config.Config{}, automatic, func() time.Time { return clock.Add(59 * time.Minute) })
				if (errRepeat != nil) != tc.wantErr || repeated.Credits != 0 || repeated.Claimed != 0 || calls.Load() != before {
					t.Fatal("manual/automatic retry bypassed backoff or reported old credits")
				}
			}
			stored := readCheckInMetadata(t, filepath.Join(dir, auth.FileName))
			if metadataSeconds(stored[LastSuccessKey]) != 0 || metadataSeconds(stored[RoundKey]) != 0 ||
				metadataSeconds(stored[NextAttemptKey]) != clock.Add(time.Hour).Unix() || metadataSeconds(stored[LastAttemptKey]) != clock.Unix() {
				t.Fatal("failure marked success or did not persist retry")
			}
			last, ok := stored[LastResultKey].(map[string]any)
			if !ok || last["status"] != tc.status || len(last) != 5 {
				t.Fatal("last result not persisted")
			}
			loaded := auth.Clone()
			loaded.Metadata = stored
			restarted := coreauth.NewManager(store, nil, nil)
			loaded = registerCheckIn(t, restarted, loaded)
			if Eligible(loaded, clock.Add(59*time.Minute)) || !Eligible(loaded, clock.Add(time.Hour)) {
				t.Fatal("restart lost retry deadline")
			}
			_, _ = run(ctx, restarted, loaded, &config.Config{}, true, func() time.Time { return clock.Add(time.Hour) })
			if calls.Load() != before*2 {
				t.Fatal("due retry did not execute")
			}
		})
	}
}

func readCheckInMetadata(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(raw, &metadata); errDecode != nil {
		t.Fatal(errDecode)
	}
	return metadata
}

func TestQoderCheckInSuccessPersistenceAndNextRound(t *testing.T) {
	store := sdkauth.NewFileTokenStore()
	dir := t.TempDir()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	auth := registerCheckIn(t, manager, testCheckInAuth(qoder.ProviderIntl))
	var posts atomic.Int32
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return checkInResponse(200, campaignList)
		}
		posts.Add(1)
		return checkInResponse(200, campaignClaim)
	}))
	boundary := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	clock := boundary.Add(-time.Second)
	if result, err := run(ctx, manager, auth, &config.Config{}, false, func() time.Time { return clock }); err != nil || result.Claimed != 1 {
		t.Fatalf("manual: %+v %v", result, err)
	}
	metadata := readCheckInMetadata(t, filepath.Join(dir, auth.FileName))
	if metadataSeconds(metadata[RoundKey]) != boundary.Add(-24*time.Hour).Unix() || metadataSeconds(metadata[NextAttemptKey]) != boundary.Unix() {
		t.Fatal("manual claim used wrong daily window")
	}
	loaded := auth.Clone()
	loaded.Metadata = metadata
	restarted := coreauth.NewManager(store, nil, nil)
	loaded = registerCheckIn(t, restarted, loaded)
	if result, err := run(ctx, restarted, loaded, &config.Config{}, false, func() time.Time { return clock }); err != nil || result.Already != 1 || result.Credits != 0 || posts.Load() != 1 {
		t.Fatal("restart repeated a completed POST")
	}
	clock = boundary
	if !Eligible(loaded, clock) {
		t.Fatal("new 10:00 round not eligible")
	}
	if result, err := run(ctx, restarted, loaded, &config.Config{}, true, func() time.Time { return clock }); err != nil || result.Claimed != 1 || posts.Load() != 2 {
		t.Fatalf("next round: %+v %v", result, err)
	}
}

func TestQoderCheckInDisabledAndOptIn(t *testing.T) {
	for _, mode := range []string{"disabled", "auto-disabled", "status-disabled", "opt-out", "before-window"} {
		for _, automatic := range []bool{true, false} {
			t.Run(mode+"/"+map[bool]string{true: "auto", false: "manual"}[automatic], func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				auth := testCheckInAuth(qoder.ProviderCN)
				now := testCheckInTime()
				switch mode {
				case "disabled":
					auth.Disabled = true
				case "auto-disabled":
					auth.AutoDisabled = true
				case "status-disabled":
					auth.Status = coreauth.StatusDisabled
				case "opt-out":
					auth.Metadata[AutoCheckInKey] = false
				case "before-window":
					now = now.Add(-3 * time.Hour)
				}
				auth = registerCheckIn(t, manager, auth)
				calls := 0
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(*http.Request) (*http.Response, error) { calls++; return checkInResponse(200, `{"campaigns":[]}`) }))
				result, err := run(ctx, manager, auth, &config.Config{}, automatic, func() time.Time { return now })
				wantCalls := 0
				if !automatic && (mode == "opt-out" || mode == "before-window") {
					wantCalls = 1
				}
				if err != nil || result.Status != "skipped" || calls != wantCalls {
					t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
				}
			})
		}
	}
}

func TestQoderCheckInRefreshPreservesConcurrentChangesAndPersists(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, disableAt := range []string{"none", "refresh", "claim"} {
			t.Run(provider+"/"+disableAt, func(t *testing.T) {
				dir := t.TempDir()
				store := sdkauth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				auth := testCheckInAuth(provider)
				auth.Metadata["expires_at"] = int64(1)
				auth.Metadata["note"] = "old note"
				auth.Prefix = "old-prefix"
				auth = registerCheckIn(t, manager, auth)
				var refreshes, posts atomic.Int32
				change := func(disable bool) {
					current, _ := manager.GetByID(auth.ID)
					current.Metadata["note"] = "concurrent note"
					current.Metadata["weight"] = 7
					current.Prefix, current.Label = "new-prefix", "new label"
					current.Metadata["prefix"], current.Metadata["label"] = current.Prefix, current.Label
					current.Disabled, current.AutoDisabled = disable, disable
					if _, errUpdate := manager.Update(t.Context(), current); errUpdate != nil {
						t.Error(errUpdate)
					}
				}
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(req *http.Request) (*http.Response, error) {
					switch req.URL.Path {
					case "/api/v1/deviceToken/refresh":
						refreshes.Add(1)
						var payload map[string]string
						if json.NewDecoder(req.Body).Decode(&payload) != nil || payload["refresh_token"] != "drt-old" {
							t.Error("incorrect refresh credentials")
						}
						change(disableAt == "refresh")
						return checkInResponse(200, `{"token":"dt-new","refresh_token":"drt-new","expires_in":3600000}`)
					case "/sash/api/v1/me/campaigns":
						if req.Header.Get("Authorization") != "Bearer dt-new" {
							t.Error("campaign did not use refreshed access token")
						}
						return checkInResponse(200, campaignList)
					case "/sash/api/v1/me/campaigns/daily/claim":
						posts.Add(1)
						if disableAt == "claim" {
							change(true)
							current, _ := manager.GetByID(auth.ID)
							current.Metadata["access_token"], current.Metadata["refresh_token"] = "dt-concurrent", "drt-concurrent"
							current.ProxyURL = "direct"
							_, _ = manager.Update(t.Context(), current)
						}
						return checkInResponse(200, campaignClaim)
					default:
						t.Errorf("unexpected request %s", req.URL.Path)
						return checkInResponse(500, `{}`)
					}
				}))
				got, err := run(ctx, manager, auth, &config.Config{}, false, testCheckInTime)
				if err != nil {
					t.Fatal(err)
				}
				live, _ := manager.GetByID(auth.ID)
				if live.Metadata["note"] != "concurrent note" || live.Prefix != "new-prefix" || live.Label != "new label" || live.Metadata["weight"] != 7 || live.Disabled != (disableAt != "none") || live.AutoDisabled != (disableAt != "none") {
					t.Fatal("refresh or claim persistence rolled back concurrent account settings")
				}
				if disableAt == "refresh" {
					if got.Status != "skipped" || posts.Load() != 0 {
						t.Fatal("disabled account claimed after refresh")
					}
				} else if got.Claimed != 1 || posts.Load() != 1 {
					t.Fatalf("claim=%+v", got)
				}
				stored := readCheckInMetadata(t, filepath.Join(dir, auth.FileName))
				wantRefresh := "drt-new"
				if disableAt == "claim" {
					wantRefresh = "drt-concurrent"
				}
				if refreshes.Load() != 1 || stored["refresh_token"] != wantRefresh || stored["note"] != "concurrent note" || stored["prefix"] != "new-prefix" || qoder.CredentialsFromAuth(live).ExpiresAt <= 1 {
					t.Fatal("rotated tokens/settings were not persisted")
				}
			})
		}
	}
}

type blockingCheckInPreparer struct {
	entered chan struct{}
	release chan struct{}
}

func (p blockingCheckInPreparer) ShouldPrepareRequestAuth(*coreauth.Auth) bool { return true }
func (p blockingCheckInPreparer) PrepareRequestAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	close(p.entered)
	select {
	case <-p.release:
		return auth, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestQoderCheckInCancellationWhileWaitingForRefresh(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := testCheckInAuth(qoder.ProviderCN)
	auth.Metadata["expires_at"] = int64(1)
	auth = registerCheckIn(t, manager, auth)
	blocker := blockingCheckInPreparer{make(chan struct{}), make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(blocker.release) })
	finished := make(chan struct{})
	go func() { _, _ = manager.PrepareRequestAuth(t.Context(), blocker, auth); close(finished) }()
	waitCheckIn(t, blocker.entered)
	var calls atomic.Int32
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return checkInResponse(500, `{}`) }))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	preparing := make(chan struct{})
	var clockReads atomic.Int32
	clock := func() time.Time {
		if clockReads.Add(1) == 2 {
			close(preparing)
		}
		return testCheckInTime()
	}
	result := make(chan runResult, 1)
	go func() {
		got, err := run(ctx, manager, auth, &config.Config{}, false, clock)
		result <- runResult{got, err}
	}()
	waitCheckIn(t, preparing)
	cancel()
	if got := receiveCheckIn(t, result); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancel=%v", got.err)
	}
	once.Do(func() { close(blocker.release) })
	waitCheckIn(t, finished)
	if calls.Load() != 0 {
		t.Fatal("cancelled waiter refreshed tokens")
	}
}

func TestQoderCheckInRefreshFailureAndMissingIdentity(t *testing.T) {
	t.Setenv("QODER_RUNTIME_INFO_PATH", "")
	for _, refreshFailure := range []bool{false, true} {
		manager := coreauth.NewManager(nil, nil, nil)
		auth := testCheckInAuth(qoder.ProviderIntl)
		delete(auth.Metadata, qoder.MachineIdentityMetadataKey)
		if refreshFailure {
			auth.Metadata["expires_at"] = int64(1)
		}
		auth = registerCheckIn(t, manager, auth)
		var requests atomic.Int32
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			if req.URL.Path != "/api/v1/deviceToken/refresh" {
				t.Error("missing native identity queried campaigns")
			}
			return checkInResponse(401, `{"error":"private-refresh-secret"}`)
		}))
		got, err := run(ctx, manager, auth, &config.Config{}, true, testCheckInTime)
		if (err != nil) != refreshFailure || got.Claimed != 0 || got.Credits != 0 || (!refreshFailure && got.Status != "unavailable") {
			t.Fatalf("result=%+v err=%v", got, err)
		}
		live, _ := manager.GetByID(auth.ID)
		if metadataSeconds(live.Metadata[NextAttemptKey]) < testCheckInTime().Add(time.Hour).Unix() || completed(live, testCheckInTime()) {
			t.Fatal("failure retry not persisted")
		}
		if refreshFailure && (requests.Load() != 1 || strings.Contains(err.Error(), "private-refresh-secret")) {
			t.Fatal("unsafe refresh failure")
		}
	}
}

func TestQoderCheckInRejectsStaleRegistration(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := registerCheckIn(t, manager, testCheckInAuth(qoder.ProviderCN))
	registerCheckIn(t, manager, auth.Clone())
	if _, err := Run(t.Context(), manager, auth, &config.Config{}, false); err == nil {
		t.Fatal("stale registration accepted")
	}
	if _, err := Run(t.Context(), nil, auth, &config.Config{}, false); err == nil {
		t.Fatal("missing manager accepted")
	}
}

func TestQoderCheckInPartialClaimsDoNotCompleteRound(t *testing.T) {
	for _, finalStatus := range []string{"blocked", "error"} {
		t.Run(finalStatus, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			auth := registerCheckIn(t, manager, testCheckInAuth(qoder.ProviderIntl))
			var posts atomic.Int32
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return checkInResponse(200, `{"campaigns":[{"campaignId":"first","claimStatus":"CLAIMABLE"},{"campaignId":"second","claimStatus":"CLAIMABLE"}]}`)
				}
				posts.Add(1)
				if strings.HasSuffix(req.URL.Path, "/first/claim") {
					return checkInResponse(200, campaignClaim)
				}
				if finalStatus == "blocked" {
					return checkInResponse(200, `{"status":"BLOCKED","benefit":{"amount":100}}`)
				}
				return checkInResponse(500, `{}`)
			}))
			clock := testCheckInTime().Add(500 * time.Millisecond)
			got, err := run(ctx, manager, auth, &config.Config{}, false, func() time.Time { return clock })
			if got.Status != finalStatus || got.Claimed != 1 || got.Credits != 100 || (err != nil) != (finalStatus == "error") {
				t.Fatalf("partial result=%+v err=%v", got, err)
			}
			live, _ := manager.GetByID(auth.ID)
			next := time.Unix(metadataSeconds(live.Metadata[NextAttemptKey]), 0)
			if completed(live, clock) || next.Sub(clock) < time.Hour {
				t.Fatal("partial claim completed round or shortened retry delay")
			}
			repeat, _ := run(ctx, manager, auth, &config.Config{}, true, func() time.Time { return clock })
			if repeat.Claimed != 0 || repeat.Credits != 0 || posts.Load() != 2 {
				t.Fatal("partial result was reported or posted twice")
			}
		})
	}
}

func TestQoderCheckInCancellationDuringClaimPersistsRetry(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := registerCheckIn(t, manager, testCheckInAuth(qoder.ProviderCN))
	entered := make(chan struct{})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", checkInTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return checkInResponse(200, campaignList)
		}
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan runResult, 1)
	go func() {
		got, err := run(ctx, manager, auth, &config.Config{}, false, testCheckInTime)
		done <- runResult{got, err}
	}()
	waitCheckIn(t, entered)
	cancel()
	got := receiveCheckIn(t, done)
	live, _ := manager.GetByID(auth.ID)
	if !errors.Is(got.err, context.Canceled) || got.result.Claimed != 0 || completed(live, testCheckInTime()) || metadataSeconds(live.Metadata[NextAttemptKey]) <= testCheckInTime().Unix() {
		t.Fatal("cancelled claim lost retry state or was marked successful")
	}
}
