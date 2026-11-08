package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBrowserLoginBothSites(t *testing.T) {
	for _, provider := range []string{ProviderCN, ProviderAI} {
		t.Run(provider, func(t *testing.T) {
			var tokenPolls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != "WorkBuddy/"+UserAgentVersion {
					t.Error("missing WorkBuddy user agent")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v2/plugin/auth/state":
					if r.Method != http.MethodPost || r.URL.Query().Get("platform") != provider || r.Header.Get("X-No-Authorization") != "true" {
						t.Error("incorrect site authorization request")
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"state":"upstream-state","authUrl":"https://login.example/signin?existing=1"}}`))
				case "/v2/plugin/auth/token":
					if r.URL.Query().Get("state") != "upstream-state" {
						t.Error("incorrect upstream state")
					}
					tokenPolls++
					if tokenPolls == 1 {
						_, _ = w.Write([]byte(`{"code":11217,"msg":"pending"}`))
						return
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"test-access","refreshToken":"test-refresh","expiresIn":3600,"refreshExpiresIn":86400,"domain":"account.example"}}`))
				case "/v2/plugin/login/account":
					if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("X-Domain") != "account.example" {
						t.Error("account lookup was not signed")
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"same-user","nickname":"Tester","type":"enterprise"}}`))
				case "/v2/plugin/accounts":
					_, _ = w.Write([]byte(`{"code":0,"data":{"accounts":[{"uid":"same-user","type":"enterprise","enterpriseId":"team-1"}]}}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			svc := NewService(provider, server.Client())
			svc.site.Endpoint = server.URL
			svc.pollInterval = time.Nanosecond
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			session, err := svc.Start(ctx)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(session.URL)
			if u.Query().Get("version") != AppVersion || u.Query().Get("loginSessionId") == "" || u.Query().Get("existing") != "1" {
				t.Fatalf("bad login URL %s", session.URL)
			}
			creds, err := svc.Wait(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			if creds.UID != "same-user" || creds.EnterpriseID != "team-1" || creds.RefreshToken != "test-refresh" || creds.ExpiresAt <= time.Now().UnixMilli() {
				t.Fatalf("login returned incomplete identity or expiry: uid=%s enterprise=%s", creds.UID, creds.EnterpriseID)
			}
			record := NewAuthRecord(provider, creds)
			other := NewAuthRecord(ProviderAI, creds)
			if provider == ProviderCN && record.FileName == other.FileName {
				t.Fatal("international and China accounts share a credential file")
			}
			if record.Provider != provider || record.Metadata["auth_kind"] != "oauth" || CredentialsFromAuth(record).RefreshToken != creds.RefreshToken {
				t.Fatal("credential metadata did not round trip")
			}
		})
	}
}

func TestRefreshPreservesIdentityAndRotatesTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" || r.Header.Get("X-Refresh-Token") != "old-refresh" || r.Header.Get("X-Auth-Refresh-Source") != "plugin" || r.Header.Get("X-Tenant-Id") != "team" {
			t.Error("incorrect refresh request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"accessToken": "new-access", "refreshToken": "new-refresh", "expiresIn": 3600}})
	}))
	defer server.Close()
	svc := NewService(ProviderAI, server.Client())
	svc.site.Endpoint = server.URL
	got, err := svc.Refresh(t.Context(), Credentials{UID: "uid", EnterpriseID: "team", Domain: "account.example", RefreshToken: "old-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.UID != "uid" || got.Domain != "account.example" || got.EnterpriseID != "team" {
		t.Fatal("refresh lost identity or token rotation")
	}
}

func TestRefreshReportsUpstreamRevokedGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Refresh-Token") != "revoked-refresh" {
			t.Error("refresh request lost the stored grant")
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":12153,"msg":"12153:refresh token failed:idsvc token: invalid_grant: refresh token revoked (http 401)"}`))
	}))
	defer server.Close()
	service := NewService(ProviderCN, server.Client())
	service.SetEndpoint(server.URL)
	_, err := service.Refresh(t.Context(), Credentials{RefreshToken: "revoked-refresh", RefreshExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 12153 || apiErr.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("revoked grant did not preserve the upstream failure: %v", err)
	}
}

func TestLoginRejectsInvalidAndCancelledFlows(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"insecure-url", `{"code":0,"data":{"state":"s","authUrl":"http://login.example"}}`},
		{"missing-state", `{"code":0,"data":{"authUrl":"https://login.example"}}`},
		{"business-error", `{"code":10085,"msg":"invalid request"}`},
		{"malformed", `<html>bad gateway</html>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			svc := NewService(ProviderCN, server.Client())
			svc.site.Endpoint = server.URL
			if _, err := svc.Start(t.Context()); err == nil {
				t.Fatal("invalid authorization accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewService(ProviderCN, http.DefaultClient).Wait(ctx, &LoginSession{State: "pending"}); err == nil {
		t.Fatal("cancelled authorization continued")
	}
}

func TestQuotaBothSitesAggregatesCreditsAndSignsEnterpriseIdentity(t *testing.T) {
	for _, provider := range []string{ProviderCN, ProviderAI} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/billing/meter/get-user-resource-summary" || r.Method != http.MethodPost {
					t.Errorf("unexpected quota request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("X-User-Id") != "uid" || r.Header.Get("X-Domain") != "account.example" || r.Header.Get("X-Tenant-Id") != "team" || r.Header.Get("User-Agent") != "WorkBuddy/"+UserAgentVersion {
					t.Error("quota request lost account or product identity")
				}
				_, _ = w.Write([]byte(`{"code":0,"data":{"IsPaidUser":true,"Packages":[{"CycleTotalCapacity":"9500","CycleUsedCapacity":"2000"},{"CycleTotalCapacity":500,"CycleUsedCapacity":"500.25"},{"CycleTotalCapacity":"","CycleUsedCapacity":null}]}}`))
			}))
			defer server.Close()
			service := NewService(provider, server.Client())
			service.SetEndpoint(server.URL)
			quota, err := service.FetchQuota(t.Context(), Credentials{AccessToken: "test-access", UID: "uid", Domain: "account.example", EnterpriseID: "team"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if quota.Plan != "Pro" || quota.Credits == nil || quota.Credits.Total != 10000 || quota.Credits.Used != 2500.25 || quota.Credits.Remaining != 7499.75 {
				t.Fatalf("unexpected quota: %+v", quota)
			}
		})
	}
}

func TestQuotaEnterpriseFallbackAndResetTimezone(t *testing.T) {
	var enterpriseCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/billing/meter/get-user-resource-summary" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[]}}`))
			return
		}
		if r.URL.Path != "/billing/meter/get-enterprise-user-usage" || r.Header.Get("X-Enterprise-Id") != "team" {
			t.Error("enterprise quota used the wrong endpoint or scope")
		}
		enterpriseCalls++
		_, _ = w.Write([]byte(`{"code":0,"data":{"limitNum":"1000","credit":"125.5","cycleResetTime":"2026-11-01 00:00:00"}}`))
	}))
	defer server.Close()
	service := NewService(ProviderCN, server.Client())
	service.SetEndpoint(server.URL)
	quota, err := service.FetchQuota(t.Context(), Credentials{AccessToken: "access", UID: "uid", EnterpriseID: "team"}, "")
	if err != nil || quota.Plan != "Enterprise" || quota.Credits == nil || quota.Credits.Remaining != 874.5 || quota.Credits.ResetsAt != "2026-10-31T16:00:00Z" {
		t.Fatalf("unexpected enterprise quota: %+v, %v", quota, err)
	}
	quota, err = service.FetchQuota(t.Context(), Credentials{AccessToken: "access", UID: "uid"}, "Custom plan")
	if err != nil || quota.Plan != "Custom plan" || quota.Credits != nil || enterpriseCalls != 1 {
		t.Fatal("an empty personal package was treated as an enterprise or unlimited quota")
	}
}

func TestQuotaRejectsBusinessErrorsAndInvalidCapacities(t *testing.T) {
	for _, payload := range []string{
		`{"code":11001,"msg":"token expired"}`,
		`{"code":0,"data":{"Packages":[{"CycleTotalCapacity":"lots"}]}}`,
		`<html>bad gateway</html>`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(payload)) }))
		service := NewService(ProviderAI, server.Client())
		service.SetEndpoint(server.URL)
		_, err := service.FetchQuota(t.Context(), Credentials{AccessToken: "access", UID: "uid"}, "")
		server.Close()
		if err == nil {
			t.Fatal("an upstream error was presented as quota")
		}
	}
}
