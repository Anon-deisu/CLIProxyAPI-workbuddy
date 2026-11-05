package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/qodercheckin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func qoderCheckInHandlerAuth(t *testing.T, manager *coreauth.Manager, provider, endpoint string) *coreauth.Auth {
	t.Helper()
	auth := qoder.NewAuthRecord(provider, qoder.Credentials{UID: "test-uid", AccessToken: "dt-private", RefreshToken: "drt-private"})
	auth.Metadata["openapi_url"] = endpoint
	auth.Metadata[qoder.MachineIdentityMetadataKey] = map[string]any{"machineToken": "native-private", "machineType": "3", "machineCode": "code-private"}
	registered, err := manager.Register(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	return registered
}

func qoderCheckInRequest(t *testing.T, ctx context.Context, h *Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Any("/checkin", h.PostQoderCheckIn)
	request := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	return w
}

func TestQoderCheckInHandlerBothRegionsRefreshAndResult(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			var refreshes, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/deviceToken/refresh":
					refreshes.Add(1)
					_, _ = io.WriteString(w, `{"token":"dt-new-private","refresh_token":"drt-new-private","expires_in":3600000}`)
				case "/sash/api/v1/me/campaigns":
					if r.Header.Get("Authorization") != "Bearer dt-new-private" || r.Header.Get("Cosy-MachineToken") != "native-private" {
						t.Error("wrong claim credentials/identity")
					}
					_, _ = io.WriteString(w, `{"campaigns":[{"campaignId":"daily","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS"}}]}`)
				case "/sash/api/v1/me/campaigns/daily/claim":
					posts.Add(1)
					_, _ = io.WriteString(w, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer upstream.Close()
			manager := coreauth.NewManager(nil, nil, nil)
			auth := qoderCheckInHandlerAuth(t, manager, provider, upstream.URL)
			auth.Metadata["expires_at"] = int64(1)
			_, _ = manager.Update(t.Context(), auth)
			h := &Handler{cfg: &config.Config{}, authManager: manager}
			for _, bodyRequest := range []bool{true, false} {
				target, body := "/checkin?auth_index="+auth.Index, ""
				if bodyRequest {
					target, body = "/checkin", `{"auth_index":"`+auth.Index+`"}`
				}
				w := qoderCheckInRequest(t, t.Context(), h, http.MethodPost, target, body)
				var result qoder.CheckInResult
				var fields map[string]any
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || json.Unmarshal(w.Body.Bytes(), &fields) != nil || len(fields) != 5 {
					t.Fatalf("invalid response %d %s", w.Code, w.Body.String())
				}
				if bodyRequest {
					if result.Status != "success" || result.Claimed != 1 || result.Credits != 100 {
						t.Fatalf("result=%+v", result)
					}
				} else if result.Status != "already" || result.Already != 1 || result.Claimed != 0 || result.Credits != 0 {
					t.Fatalf("repeat=%+v", result)
				}
				for _, secret := range []string{"dt-", "drt-", "native-private", "code-private", "access_token", "machineToken"} {
					if strings.Contains(w.Body.String(), secret) {
						t.Fatal("management check-in exposed credentials")
					}
				}
			}
			if refreshes.Load() != 1 || posts.Load() != 1 {
				t.Fatal("repeat request duplicated refresh or POST")
			}
			live, _ := manager.GetByID(auth.ID)
			if live.Metadata[qodercheckin.LastResultKey] == nil {
				t.Fatal("manual result was not saved")
			}
		})
	}
}

func TestQoderCheckInHandlerInputValidation(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	other, err := manager.Register(t.Context(), &coreauth.Auth{ID: "other", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	for _, tc := range []struct {
		method, target, body string
		code                 int
	}{
		{http.MethodGet, "/checkin?auth_index=missing", "", 405},
		{http.MethodPost, "/checkin", "", 400},
		{http.MethodPost, "/checkin?auth_index=%20", "", 400},
		{http.MethodPost, "/checkin?auth_index=missing", "", 404},
		{http.MethodPost, "/checkin?auth_index=" + other.Index, "", 400},
		{http.MethodPost, "/checkin", `{"auth_index":123}`, 400},
		{http.MethodPost, "/checkin", `{"auth_index":"missing"} {}`, 400},
		{http.MethodPost, "/checkin", `null`, 400},
		{http.MethodPost, "/checkin", `{"auth_index":"` + strings.Repeat("x", 4096) + `"}`, 400},
		{http.MethodPost, "/checkin?auth_index=a&auth_index=b", "", 400},
		{http.MethodPost, "/checkin?auth_index=a", `{"auth_index":"b"}`, 400},
	} {
		w := qoderCheckInRequest(t, t.Context(), h, tc.method, tc.target, tc.body)
		if w.Code != tc.code {
			t.Errorf("%s: code=%d want=%d", tc.target, w.Code, tc.code)
		}
		if tc.code == 405 && w.Header().Get("Allow") != http.MethodPost {
			t.Error("missing Allow header")
		}
	}
	for _, missing := range []*Handler{nil, {}, {cfg: &config.Config{}}, {authManager: manager}} {
		w := qoderCheckInRequest(t, t.Context(), missing, http.MethodPost, "/checkin?auth_index=missing", "")
		if w.Code != 503 {
			t.Fatalf("unconfigured handler: %d", w.Code)
		}
	}
}

func TestQoderCheckInHandlerFailuresDisabledAndMissingComponent(t *testing.T) {
	t.Setenv("QODER_RUNTIME_INFO_PATH", "")
	for _, scenario := range []string{"error", "refresh-error", "blocked", "missing-component", "disabled", "auto-disabled", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if scenario == "blocked" {
					_, _ = io.WriteString(w, `{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED","message":"private-response"}`)
					return
				}
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":"private-response dt-private drt-private"}`)
			}))
			defer upstream.Close()
			manager := coreauth.NewManager(nil, nil, nil)
			auth := qoderCheckInHandlerAuth(t, manager, qoder.ProviderIntl, upstream.URL)
			ctx := t.Context()
			wantCode, wantStatus, wantRequests := 200, "skipped", int32(0)
			switch scenario {
			case "error", "refresh-error":
				wantCode, wantStatus, wantRequests = 502, "error", 1
				if scenario == "refresh-error" {
					auth.Metadata["expires_at"] = int64(1)
				}
			case "blocked":
				wantStatus, wantRequests = "blocked", 1
			case "missing-component":
				delete(auth.Metadata, qoder.MachineIdentityMetadataKey)
				wantStatus = "unavailable"
			case "disabled":
				auth.Disabled = true
			case "auto-disabled":
				auth.AutoDisabled = true
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantCode, wantStatus = 408, "error"
			}
			_, _ = manager.Update(t.Context(), auth)
			h := &Handler{cfg: &config.Config{}, authManager: manager}
			w := qoderCheckInRequest(t, ctx, h, http.MethodPost, "/checkin?auth_index="+auth.Index, "")
			var result qoder.CheckInResult
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != wantCode || result.Status != wantStatus || requests.Load() != wantRequests {
				t.Fatalf("code=%d result=%+v calls=%d", w.Code, result, requests.Load())
			}
			if strings.Contains(w.Body.String(), "private") || result.Credits != 0 || result.Claimed != 0 {
				t.Fatal("failure leaked details or reported credits")
			}
		})
	}
}
