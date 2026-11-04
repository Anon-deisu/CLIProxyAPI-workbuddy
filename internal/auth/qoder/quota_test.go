package qoder

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQoderQuotaBothSitesDecimalsAndPlan(t *testing.T) {
	for _, provider := range []string{ProviderIntl, ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			var paths []string
			var requestIDs []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				requestIDs = append(requestIDs, r.Header.Get("X-Request-ID"))
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer dt-quota" || r.Header.Get("Origin") != SiteForProvider(provider).Website || r.Header.Get("X-Machine-ID") != DeriveID("uid", "machine") || r.Header.Get("X-Session-ID") != DeriveID("uid", "session") || r.UserAgent() != SiteForProvider(provider).userAgent() {
					t.Error("incorrect quota identity or protocol")
				}
				switch r.URL.Path {
				case "/api/v2/quota/usage":
					_, _ = w.Write([]byte(`{"userQuota":{"total":1000.5,"used":250.25,"remaining":750.25},"addOnQuota":{"total":"100.25","used":"0.125","remaining":"100.125"},"orgResourcePackage":{"cap":2500.5,"used":1548.25,"remaining":952.25,"percentage":0.62,"unit":"credits","available":true},"isQuotaExceeded":false,"expiresAt":253402214400}`))
				case "/api/v2/user/plan":
					_, _ = w.Write([]byte(`{"plan_tier_name":"Pro Trial","user_type":"personal_professional_trial"}`))
				default:
					t.Errorf("unexpected quota path: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			svc := NewService(provider, server.Client())
			svc.SetEndpoint(server.URL)
			quota, err := svc.FetchQuota(t.Context(), Credentials{AccessToken: "dt-quota", UID: "uid", MachineID: "login-machine", OrganizationID: "org", OrganizationName: "Team"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if quota.Plan != "Pro Trial" || quota.Realm != SiteForProvider(provider).Realm || quota.Credits == nil || quota.Credits.Total != 1100.75 || quota.Credits.Used != 250.375 || quota.Credits.Remaining != 850.375 || math.Abs(quota.Credits.RemainingPercent-850.375/1100.75*100) > 1e-10 || len(quota.Packages) != 2 {
				t.Fatalf("incorrect account quota: %+v", quota)
			}
			if quota.Shared == nil || quota.Shared.Cap != 2500.5 || quota.Shared.Remaining != 952.25 || quota.Shared.Used != 1548.25 || !quota.Shared.Available || quota.Shared.Unlimited || quota.Shared.OrganizationID != "org" || quota.Shared.OrganizationName != "Team" {
				t.Fatal("shared pool lost or merged into account credits")
			}
			if len(paths) != 2 || requestIDs[0] == "" || requestIDs[0] == requestIDs[1] {
				t.Fatal("missing plan query or reused request ID")
			}
			encoded, errJSON := json.Marshal(quota)
			if errJSON != nil {
				t.Fatal(errJSON)
			}
			var exported map[string]json.RawMessage
			if errJSON = json.Unmarshal(encoded, &exported); errJSON != nil {
				t.Fatal(errJSON)
			}
			for _, key := range []string{"plan", "credits", "packages", "shared", "realm"} {
				if _, exists := exported[key]; !exists {
					t.Errorf("missing exported quota key %s", key)
				}
			}
			if strings.Contains(string(encoded), "resets_at") || strings.Contains(string(encoded), "expires_at") {
				t.Fatal("unknown top-level expiry became a package expiry or reset")
			}
		})
	}
}

func TestQoderQuotaUnknownVersusZero(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		wantErr          bool
		total, remaining float64
	}{
		{"missing", `{}`, true, 0, 0},
		{"null-root", `null`, true, 0, 0},
		{"array-root", `[]`, true, 0, 0},
		{"wrong-envelope", `{"data":{"userQuota":{"total":0,"used":0,"remaining":0}}}`, true, 0, 0},
		{"empty-object", `{"userQuota":{}}`, true, 0, 0},
		{"null-object", `{"userQuota":null}`, true, 0, 0},
		{"missing-capacity", `{"userQuota":{"used":0,"remaining":0}}`, true, 0, 0},
		{"null-capacity", `{"userQuota":{"total":null,"used":0,"remaining":0}}`, true, 0, 0},
		{"empty-capacity", `{"userQuota":{"total":"","used":0,"remaining":0}}`, true, 0, 0},
		{"boolean-capacity", `{"userQuota":{"total":true,"used":0,"remaining":0}}`, true, 0, 0},
		{"invalid-usage", `{"userQuota":{"total":10,"used":"secret-body","remaining":0}}`, true, 0, 0},
		{"nan", `{"userQuota":{"total":"NaN","used":0,"remaining":0}}`, true, 0, 0},
		{"infinity", `{"userQuota":{"total":"Inf","used":0,"remaining":0}}`, true, 0, 0},
		{"negative", `{"userQuota":{"total":-1,"used":0,"remaining":0}}`, true, 0, 0},
		{"invalid-addon", `{"userQuota":{"total":10,"used":0,"remaining":10},"addOnQuota":{}}`, true, 0, 0},
		{"degraded", `{"userQuota":{"total":0,"used":0,"remaining":0},"isQuotaExceeded":true}`, true, 0, 0},
		{"degraded-string", `{"userQuota":{"total":0,"used":0,"remaining":0},"isQuotaExceeded":"true"}`, true, 0, 0},
		{"real-zero", `{"userQuota":{"total":0,"used":0,"remaining":0},"addOnQuota":{"total":0,"used":0,"remaining":0},"isQuotaExceeded":false}`, false, 0, 0},
		{"string-false", `{"userQuota":{"total":0,"used":0,"remaining":0},"isQuotaExceeded":"false"}`, false, 0, 0},
		{"exhausted", `{"userQuota":{"total":1000,"used":1000,"remaining":0},"isQuotaExceeded":true}`, false, 1000, 0},
		{"healthy-sentinel", `{"userQuota":{"total":10,"used":2.5,"remaining":7.5},"expiresAt":253402214400}`, false, 10, 7.5},
		{"addon-only", `{"addOnQuota":{"total":10,"used":2.5,"remaining":7.5}}`, false, 10, 7.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/quota/usage" {
					t.Error("cached plan caused another network request")
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			svc := NewService(ProviderIntl, server.Client())
			svc.SetEndpoint(server.URL)
			got, err := svc.FetchQuota(t.Context(), Credentials{AccessToken: "dt-quota", UID: "uid"}, "Known plan")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, wantError=%t", err, tc.wantErr)
			}
			if err != nil {
				if got.Credits != nil || strings.Contains(err.Error(), "secret-body") {
					t.Fatal("error returned a false zero quota or leaked a body")
				}
				return
			}
			if got.Credits == nil || got.Credits.Total != tc.total || got.Credits.Remaining != tc.remaining || got.Plan != "Known plan" || got.Shared != nil {
				t.Fatal("known zero or decimal balance lost")
			}
		})
	}
}

func TestQoderQuotaSharedPools(t *testing.T) {
	for _, tc := range []struct {
		name, shared                  string
		available, unlimited, wantErr bool
		cap                           float64
	}{
		{"finite", `{"cap":2500,"used":1548,"remaining":952,"available":true}`, true, false, false, 2500},
		{"unlimited", `{"cap":-1,"used":0,"remaining":1000000,"available":true}`, true, true, false, -1},
		{"unavailable", `{"cap":2500,"used":1548,"remaining":952,"available":false}`, false, false, false, 2500},
		{"string-false", `{"cap":2500,"used":1548,"remaining":952,"available":"false"}`, false, false, false, 2500},
		{"zero-available", `{"cap":2500,"used":1548,"remaining":952,"available":0}`, false, false, false, 2500},
		{"null-available", `{"cap":2500,"used":1548,"remaining":952,"available":null}`, false, false, false, 2500},
		{"missing-available", `{"cap":2500,"used":1548,"remaining":952}`, true, false, false, 2500},
		{"explicit-zero-cap", `{"cap":0,"total":999,"used":0,"remaining":0}`, true, false, false, 0},
		{"unknown-cap", `{"used":0,"remaining":0}`, false, false, true, 0},
		{"bad-cap", `{"cap":-2,"used":0,"remaining":0}`, false, false, true, 0},
		{"bad-availability", `{"cap":1,"used":0,"remaining":1,"available":"maybe"}`, false, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"userQuota":{"total":3000,"used":3000,"remaining":0},"orgResourcePackage":%s,"isQuotaExceeded":true}`, tc.shared)
			}))
			defer server.Close()
			svc := NewService(ProviderCN, server.Client())
			svc.SetEndpoint(server.URL)
			quota, err := svc.FetchQuota(t.Context(), Credentials{AccessToken: "dt-quota", UID: "uid", OrganizationID: "org-1"}, "Team")
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected shared parsing error: %v", err)
			}
			if tc.wantErr {
				return
			}
			if quota.Shared == nil || quota.Shared.Cap != tc.cap || quota.Shared.Unlimited != tc.unlimited || quota.Shared.Available != tc.available || quota.Shared.OrganizationID != "org-1" || quota.Credits.Remaining != 0 || quota.Credits.Total != 3000 || len(quota.Packages) != 1 {
				t.Fatal("shared organization credits mixed with account credits")
			}
		})
	}
}

func TestQoderQuotaPackageExpiryIsNotReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"userQuota":{"total":100,"used":1,"remaining":99},"addOnQuota":{"total":25.5,"used":0.5,"remaining":25,"expiresAt":"2026-11-01T00:00:00+08:00","daysLeft":1.25,"noExpiry":false,"packageCode":"gift"},"expiresAt":253402214400}`))
	}))
	defer server.Close()
	svc := NewService(ProviderCN, server.Client())
	svc.SetEndpoint(server.URL)
	quota, err := svc.FetchQuota(t.Context(), Credentials{AccessToken: "dt-quota", UID: "uid"}, "Pro")
	if err != nil {
		t.Fatal(err)
	}
	pack := quota.Packages[1]
	if quota.Packages[0].ExpiresAt != 0 || pack.ExpiresAt != time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC).Unix() || pack.DaysLeft == nil || *pack.DaysLeft != 1.25 || pack.NoExpiry == nil || *pack.NoExpiry || pack.PackageCode != "gift" {
		t.Fatal("package expiry metadata lost or fabricated")
	}
	encoded, _ := json.Marshal(quota)
	if strings.Contains(string(encoded), "resets_at") {
		t.Fatal("expiry misrepresented as a reset")
	}
}

func TestQoderQuotaPlanAndHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, plan string
		status     int
		wantErr    bool
	}{
		{"user-type", `{"user_type":"team"}`, 200, false},
		{"missing", `{}`, 200, true},
		{"invalid", `{"plan_tier_name":{}}`, 200, true},
		{"error", `{"error":"dt-secret"}`, 200, true},
		{"http-failure", `dt-secret`, 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/quota/usage" {
					_, _ = w.Write([]byte(`{"userQuota":{"total":10,"used":0,"remaining":10}}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.plan))
			}))
			defer server.Close()
			transport := server.Client().Transport
			client := &http.Client{Transport: qoderTestTransport(func(req *http.Request) (*http.Response, error) {
				if _, ok := req.Context().Deadline(); ok {
					t.Error("quota service added a read deadline")
				}
				return transport.RoundTrip(req)
			})}
			svc := NewService(ProviderIntl, client)
			svc.SetEndpoint(server.URL)
			quota, err := svc.FetchQuota(t.Context(), Credentials{AccessToken: "dt-secret", UID: "uid"}, "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected plan result: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "dt-secret") || quota.Credits != nil) {
				t.Fatal("plan failure leaked secrets or returned partial quota")
			}
			if err == nil && quota.Plan != "team" {
				t.Fatal("user_type plan fallback lost")
			}
		})
	}
}
