package qoder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQoderBrowserLoginBothSites(t *testing.T) {
	for _, tc := range []struct{ provider, realm, website, openapi, gateway, clientID, ua string }{
		{ProviderIntl, "intl", "https://qoder.com", "https://openapi.qoder.sh", "https://api3.qoder.sh", "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb", "Qoder/1.1.75"},
		{ProviderCN, "cn", "https://qoder.com.cn", "https://openapi.qoder.com.cn", "https://gateway.qoder.com.cn", "1c5e33e1-364d-4ce6-b02c-acaa81274a5c", "QoderWork/1.1.75"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			site := SiteForProvider(tc.provider)
			if site.Provider != tc.provider || site.Realm != tc.realm || site.Website != tc.website || site.OpenAPI != tc.openapi || site.Gateway != tc.gateway || site.ClientID != tc.clientID || site.Name == "" {
				t.Fatal("incorrect regional configuration")
			}
			var session *LoginSession
			var polls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected method %s", r.Method)
				}
				switch r.URL.Path {
				case "/api/v1/deviceToken/poll":
					query := r.URL.Query()
					if len(query) != 3 || query.Get("nonce") != session.Nonce || query.Get("verifier") != session.Verifier || query.Get("challenge_method") != "S256" {
						t.Error("incorrect poll parameters")
					}
					if r.Header.Get("Authorization") != "" || r.UserAgent() != "QoderWork" {
						t.Error("incorrect poll headers")
					}
					polls++
					if polls == 1 {
						w.WriteHeader(http.StatusAccepted)
						return
					}
					if polls == 2 {
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte("pending, not JSON"))
						return
					}
					if polls == 3 {
						_, _ = w.Write([]byte(`{}`))
						return
					}
					if tc.provider == ProviderCN {
						_, _ = w.Write([]byte(`{"device_token":"dt-access","refresh_token":"drt-refresh","expires_at":"2026-11-01T00:00:00.999+08:00"}`))
					} else {
						_, _ = w.Write([]byte(`{"token":"dt-access","refresh_token":"drt-refresh","user_id":"9007199254740993","expires_in":3600750}`))
					}
				case "/api/v1/userinfo":
					uid := "9007199254740993"
					if tc.provider == ProviderCN {
						uid = ""
					}
					if r.Header.Get("Authorization") != "Bearer dt-access" || r.UserAgent() != tc.ua || r.Header.Get("X-Machine-ID") != DeriveID(uid, "machine") || r.Header.Get("Origin") != tc.website {
						t.Error("userinfo lost token, site or UID-derived API identity")
					}
					_, _ = w.Write([]byte(`{"id":9007199254740993,"name":"Tester","email":"user@example.test","user_type":"team","organizationId":"org-1","organizationName":"Team"}`))
				default:
					t.Errorf("unexpected request path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			svc := NewService(tc.provider, server.Client())
			svc.SetEndpoint(server.URL + "/")
			svc.pollInterval = time.Nanosecond
			now := time.Date(2026, 10, 31, 12, 0, 0, 500000000, time.UTC)
			svc.nowFunc = func() time.Time { return now }
			var err error
			session, err = svc.Start(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			u, errURL := url.Parse(session.URL)
			if errURL != nil {
				t.Fatal(errURL)
			}
			q := u.Query()
			sum := sha256.Sum256([]byte(session.Verifier))
			if u.Scheme+"://"+u.Host != tc.website || u.Path != "/device/selectAccounts" || q.Get("challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("challenge_method") != "S256" || q.Get("nonce") != session.Nonce || q.Get("machine_id") != session.MachineID || q.Get("client_id") != tc.clientID {
				t.Fatal("incorrect browser authorization URL")
			}
			if !regexp.MustCompile(`^[A-Za-z0-9._~-]{64}$`).MatchString(session.Verifier) || session.State == "" || uuid.Validate(session.MachineID) != nil {
				t.Fatal("invalid verifier, state or machine ID")
			}
			if tc.provider == ProviderCN {
				if q.Get("redirect_uri") != "qoder-work-cn://" || uuid.Validate(session.Nonce) != nil || len(q) != 6 {
					t.Fatal("incorrect China device protocol")
				}
			} else if q.Has("redirect_uri") || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(session.Nonce) || len(q) != 5 {
				t.Fatal("incorrect international device protocol")
			}
			creds, errWait := svc.Wait(t.Context(), session)
			if errWait != nil {
				t.Fatal(errWait)
			}
			wantExpiry := now.Add(3600750 * time.Millisecond).Unix()
			if tc.provider == ProviderCN {
				wantExpiry = time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC).Unix()
			}
			if polls != 4 || creds.ExpiresAt != wantExpiry || creds.UID != "9007199254740993" || creds.OrganizationID != "org-1" || creds.OrganizationName != "Team" || creds.UserType != "team" || creds.Email != "user@example.test" || creds.Nickname != "Tester" || creds.MachineID != session.MachineID || creds.Realm != tc.realm || creds.Domain != strings.TrimPrefix(tc.website, "https://") {
				t.Fatal("login lost identity, milliseconds, timezone or region")
			}
			other, errStart := svc.Start(t.Context())
			if errStart != nil || other.State == session.State || other.Verifier == session.Verifier || other.Nonce == session.Nonce {
				t.Fatal("authorization sessions were reused")
			}
		})
	}
}

func TestQoderLoginAuthoritativeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, poll, userinfo, wantUID string
		status                        int
		wantErr                       bool
	}{
		{"missing", `{"token":"dt-secret"}`, `{}`, "", 200, true},
		{"boolean", `{"token":"dt-secret","user_id":false}`, `{"id":true}`, "", 200, true},
		{"object", `{"token":"dt-secret","user_id":{}}`, `{"id":{},"name":"not-an-id","email":"not-an-id@example.test"}`, "", 200, true},
		{"lookup-failure", `{"token":"dt-secret"}`, `{"error":"dt-secret"}`, "", 403, true},
		{"poll-authority", `{"token":"dt-secret","user_id":"uid"}`, `{"error":"dt-secret"}`, "uid", 503, false},
		{"userinfo-authority", `{"token":"dt-secret"}`, `{"id":"uid","organization_id":1234,"organization_name":"Team"}`, "uid", 200, false},
		{"conflict", `{"token":"dt-secret","user_id":"first"}`, `{"id":"second"}`, "", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/deviceToken/poll" {
					_, _ = w.Write([]byte(tc.poll))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.userinfo))
			}))
			defer server.Close()
			svc := NewService(ProviderIntl, server.Client())
			svc.SetEndpoint(server.URL)
			session, _ := svc.Start(t.Context())
			got, err := svc.Wait(t.Context(), session)
			if tc.wantErr {
				if err == nil || strings.Contains(err.Error(), "dt-secret") {
					t.Fatal("invalid identity was accepted or a secret leaked")
				}
				return
			}
			if err != nil || got.UID != tc.wantUID || got.ExpiresAt != 0 {
				t.Fatal("authoritative identity or unknown expiry lost")
			}
			if tc.name == "userinfo-authority" && (got.OrganizationID != "1234" || got.OrganizationName != "Team") {
				t.Fatal("snake_case organization was lost")
			}
		})
	}
}

func TestQoderSessionExpiryAndCancellation(t *testing.T) {
	var calls int
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; cancel(); w.WriteHeader(http.StatusAccepted) }))
	defer server.Close()
	svc := NewService(ProviderIntl, server.Client())
	svc.SetEndpoint(server.URL)
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	svc.nowFunc = func() time.Time { return now }
	session, _ := svc.Start(t.Context())
	now = now.Add(LoginTimeout)
	if _, err := svc.Wait(t.Context(), session); err == nil || calls != 0 {
		t.Fatal("expired session polled upstream")
	}
	if _, err := svc.Wait(t.Context(), nil); err == nil {
		t.Fatal("nil session accepted")
	}
	other := NewService(ProviderCN, server.Client())
	if _, err := other.Wait(t.Context(), session); err == nil {
		t.Fatal("session accepted by another region")
	}
	session, _ = svc.Start(t.Context())
	if _, err := svc.Wait(ctx, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := svc.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled login started")
	}
}

func TestQoderRefreshRotationAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 750000000, time.UTC)
	for _, provider := range []string{ProviderIntl, ProviderCN} {
		for _, tc := range []struct {
			name, fields string
			want         int64
			wantErr      bool
		}{
			{"milliseconds", `,"expires_in":3600500`, now.Add(3600500 * time.Millisecond).Unix(), false},
			{"quoted-milliseconds", `,"expires_in":"1500"`, now.Add(1500 * time.Millisecond).Unix(), false},
			{"rfc3339-offset", `,"expires_at":"2026-11-01T00:30:00.5+08:00"`, time.Date(2026, 10, 31, 16, 30, 0, 0, time.UTC).Unix(), false},
			{"rfc3339-utc", `,"expires_at":"2026-11-01T00:30:00Z"`, time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC).Unix(), false},
			{"omitted", "", 1700000000, false},
			{"null", `,"expires_in":null,"expires_at":null`, 1700000000, false},
			{"invalid", `,"expires_in":"token-secret"`, 0, true},
			{"unbounded", `,"expires_in":"Inf"`, 0, true},
			{"missing-timezone", `,"expires_at":"2026-11-01T00:30:00"`, 0, true},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				before := Credentials{AccessToken: "dt-old", RefreshToken: "drt-old", PersonalToken: "pt-keep", UID: "uid", Nickname: "Name", Email: "mail@example.test", UserType: "team", OrganizationID: "org", OrganizationName: "Org", Realm: SiteForProvider(provider).Realm, Domain: "stored.example", MachineID: "login-machine", ExpiresAt: 1700000000}
				original := before
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload map[string]string
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/deviceToken/refresh" || len(payload) != 1 || payload["refresh_token"] != "drt-old" || r.Header.Get("Authorization") != "" || r.UserAgent() != SiteForProvider(provider).userAgent() {
						t.Error("incorrect refresh protocol")
					}
					_, _ = w.Write([]byte(`{"device_token":"dt-new","refresh_token":"drt-new","organization_id":"must-not-replace"` + tc.fields + `}`))
				}))
				defer server.Close()
				svc := NewService(provider, server.Client())
				svc.SetEndpoint(server.URL)
				svc.nowFunc = func() time.Time { return now }
				got, err := svc.Refresh(t.Context(), before)
				if before != original {
					t.Fatal("input credentials mutated")
				}
				if tc.wantErr {
					if err == nil || strings.Contains(err.Error(), "token-secret") {
						t.Fatal("invalid expiry accepted or leaked")
					}
					return
				}
				want := before
				want.AccessToken = "dt-new"
				want.RefreshToken = "drt-new"
				want.ExpiresAt = tc.want
				if err != nil || got != want {
					t.Fatal("refresh lost identity, token rotation or expiry")
				}
			})
		}
	}
}

func TestQoderPATRefreshFallback(t *testing.T) {
	for _, tc := range []struct {
		name, refresh, personal string
		fail                    bool
		paths                   []string
		wantErr                 bool
	}{
		{"device-failure", "drt-old", "pt-fallback", true, []string{"/api/v1/deviceToken/refresh"}, true},
		{"device-success", "drt-old", "pt-fallback", false, []string{"/api/v1/deviceToken/refresh"}, false},
		{"job-success", "jrt-old", "pt-fallback", false, []string{"/api/v1/jobToken/refresh"}, false},
		{"job-fallback", "jrt-old", "pt-fallback", true, []string{"/api/v1/jobToken/refresh", "/api/v1/jobToken/exchange"}, false},
		{"other-refresh-fallback", "old", "pt-fallback", true, []string{"/api/v1/jobToken/refresh", "/api/v1/jobToken/exchange"}, false},
		{"job-no-pat", "jrt-old", "", true, []string{"/api/v1/jobToken/refresh"}, true},
		{"pat-only", "", "pt-fallback", false, []string{"/api/v1/jobToken/exchange"}, false},
		{"none", "", "", false, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if strings.HasSuffix(r.URL.Path, "/exchange") {
					if len(body) != 1 || body["personal_token"] != tc.personal {
						t.Error("incorrect PAT exchange body")
					}
				} else {
					if len(body) != 1 || body["refresh_token"] != tc.refresh {
						t.Error("incorrect refresh body")
					}
					if tc.fail {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = w.Write([]byte(`{"error":"pt-fallback drt-old jrt-old"}`))
						return
					}
				}
				_, _ = w.Write([]byte(`{"token":"new-token"}`))
			}))
			defer server.Close()
			svc := NewService(ProviderIntl, server.Client())
			svc.SetEndpoint(server.URL)
			got, err := svc.Refresh(t.Context(), Credentials{UID: "uid", RefreshToken: tc.refresh, PersonalToken: tc.personal, ExpiresAt: 1700000000, MachineID: "keep"})
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(paths, tc.paths) {
				t.Fatalf("unexpected fallback: paths=%v err=%v", paths, err)
			}
			if err == nil && (got.RefreshToken != tc.refresh || got.PersonalToken != tc.personal || got.ExpiresAt != 1700000000 || got.MachineID != "keep") {
				t.Fatal("fallback discarded persistent credentials")
			}
			if err != nil && (strings.Contains(err.Error(), "pt-fallback") || strings.Contains(err.Error(), "drt-old") || strings.Contains(err.Error(), "jrt-old")) {
				t.Fatal("refresh error leaked credentials")
			}
		})
	}
}

func TestQoderCredentialsRoundTripAndIsolation(t *testing.T) {
	creds := Credentials{AccessToken: "dt-secret", RefreshToken: "drt-secret", PersonalToken: "pt-secret", UID: "user/one", Nickname: "Display Name", UserType: "team", OrganizationID: "org/one", OrganizationName: "Org", Realm: "intl", Domain: "qoder.com", MachineID: "persisted-machine", ExpiresAt: 1793462400}
	record := NewAuthRecord(ProviderIntl, creds)
	if record == nil || record.Label != creds.Nickname || record.Metadata["email"] != "" || record.Metadata["auth_kind"] != "oauth" || record.Metadata["expires_at"] != creds.ExpiresAt || record.Metadata["expired"] != time.Unix(creds.ExpiresAt, 0).UTC().Format(time.RFC3339) {
		t.Fatal("incorrect credential metadata")
	}
	data, err := json.Marshal(record.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err = json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	loaded := &coreauth.Auth{Provider: ProviderIntl, Metadata: stored}
	if CredentialsFromAuth(loaded) != creds {
		t.Fatal("credentials failed JSON storage round trip")
	}
	for _, forbidden := range []string{"accessToken", "refreshToken", "expiresAt", "organizationId", "machineId"} {
		if _, exists := stored[forbidden]; exists {
			t.Error("camelCase credential key stored")
		}
	}
	changed := creds
	changed.AccessToken = "rotated"
	changed.Nickname = "Other Name"
	changed.MachineID = "another-machine"
	if NewAuthRecord(ProviderIntl, changed).FileName != record.FileName {
		t.Fatal("file identity depends on mutable fields")
	}
	for _, change := range []func(*Credentials){func(c *Credentials) { c.UID = "another-user" }, func(c *Credentials) { c.OrganizationID = "another-org" }, func(c *Credentials) { c.Realm = "cn" }} {
		other := creds
		change(&other)
		if NewAuthRecord(ProviderIntl, other).FileName == record.FileName {
			t.Fatal("credential filename collision")
		}
	}
	if NewAuthRecord(ProviderCN, creds).FileName == record.FileName || strings.ContainsAny(record.FileName, `/\`) {
		t.Fatal("unsafe regional filename")
	}
	creds.Nickname = ""
	if NewAuthRecord(ProviderIntl, creds).Label != creds.UID || NewAuthRecord(ProviderIntl, Credentials{}) != nil {
		t.Fatal("invalid account label or identity")
	}
	record.Metadata["openapi_url"] = " https://openapi.example/custom/ "
	record.Metadata["base_url"] = "https://base.example/"
	record.Metadata["gateway_url"] = "https://gateway.example/custom/"
	creds.ExpiresAt = 0
	ApplyCredentials(record, creds)
	if CredentialsFromAuth(record).ExpiresAt != 1793462400 || EndpointForAuth(record) != "https://openapi.example/custom" || GatewayForAuth(record) != "https://gateway.example/custom" {
		t.Fatal("apply lost expiry or endpoint overrides")
	}
	delete(record.Metadata, "openapi_url")
	if EndpointForAuth(record) != "https://base.example" {
		t.Fatal("base_url fallback lost")
	}
	delete(record.Metadata, "base_url")
	delete(record.Metadata, "gateway_url")
	if EndpointForAuth(record) != SiteForProvider(ProviderIntl).OpenAPI || GatewayForAuth(record) != SiteForProvider(ProviderIntl).Gateway {
		t.Fatal("default API and gateway confused")
	}
	delete(loaded.Metadata, "expires_at")
	if CredentialsFromAuth(loaded).ExpiresAt != 1793462400 {
		t.Fatal("RFC3339 metadata expiry fallback lost")
	}
}

func TestQoderFingerprintMatchesUpstream(t *testing.T) {
	t.Setenv("QD_DEVICE_ID_LEGACY", "")
	// Golden values from qoder_fingerprint.py at 45e76f9.
	for _, tc := range []struct{ uid, machine, session, machineType, token string }{
		{"uid-123", "156a8d19-9a1c-49aa-a5aa-224a5b998157", "20abc9f6-9c21-4906-a512-dda58cff2726", "9f7ad417c6594e3ead", "Mnt8AeqMlegW9ei-g5tl6yO0Wn8KZk-pGK5ZRXzeAzH"},
		{"", "fc1f3f60-3c8b-4dc0-a839-42ea0169ab1e", "be4befa7-ace5-45fd-a1f5-8bb31be975f8", "c8ea195d284147c2af", "HyjNtGtrc0r5FhrLxRhNVmrJ1uypzw1ICcGMaUG74Ar"},
	} {
		if DeriveID(tc.uid, "machine") != tc.machine || DeriveID(tc.uid, "session") != tc.session || MachineType(tc.uid) != tc.machineType || MachineToken(tc.uid) != tc.token {
			t.Fatal("fingerprint differs from pinned upstream")
		}
	}
	for _, provider := range []string{ProviderIntl, ProviderCN} {
		req := &http.Request{}
		SignAPIRequest(req, provider, Credentials{UID: "uid-123", AccessToken: "dt-test"})
		site := SiteForProvider(provider)
		if req.Header.Get("X-Machine-ID") != "156a8d19-9a1c-49aa-a5aa-224a5b998157" || req.Header.Get("X-Session-ID") != "20abc9f6-9c21-4906-a512-dda58cff2726" || req.Header.Get("Authorization") != "Bearer dt-test" || req.Header.Get("User-Agent") != site.userAgent() || req.Header.Get("Origin") != site.Website || req.Header.Get("Referer") != site.Website+"/" || req.Header.Get("Accept") != "application/json, text/plain, */*" {
			t.Fatal("incorrect OpenAPI headers")
		}
		firstID := req.Header.Get("X-Request-ID")
		if !regexp.MustCompile(`^[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`).MatchString(firstID) {
			t.Fatal("request ID is not random UUID4 hex")
		}
		SignAPIRequest(req, provider, Credentials{UID: "uid-123", MachineID: "login-machine"})
		if req.Header.Get("X-Request-ID") == firstID || req.Header.Get("X-Machine-ID") != DeriveID("uid-123", "machine") {
			t.Fatal("request ID reused or API fingerprint changed with the login machine")
		}
	}
	t.Setenv("QD_DEVICE_ID_LEGACY", "true")
	if len(DeriveID("uid-123", "machine")) != 32 || strings.Contains(DeriveID("uid-123", "machine"), "-") {
		t.Fatal("legacy fingerprint encoding lost")
	}
}

type qoderTestTransport func(*http.Request) (*http.Response, error)

func (f qoderTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQoderErrorsDoNotLeakSecrets(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, `{"error":"dt-secret drt-secret pt-secret"}`},
		{200, `{"success":false,"message":"dt-secret"}`},
		{200, `{"code":12153,"msg":"drt-secret"}`},
		{200, `{"token":"dt-secret","expires_in":"pt-secret"}`},
		{200, `{"token":`},
		{200, `{"token":"dt-secret"} {"error":"pt-secret"}`},
		{200, `null`},
		{200, `[]`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		svc := NewService(ProviderIntl, server.Client())
		svc.SetEndpoint(server.URL)
		_, err := svc.Refresh(t.Context(), Credentials{RefreshToken: "drt-secret", UID: "uid"})
		server.Close()
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid token response accepted or a secret leaked")
		}
	}
	client := &http.Client{Transport: qoderTestTransport(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("transport echoed %s drt-secret", req.URL)
	})}
	svc := NewService(ProviderIntl, client)
	session, _ := svc.Start(t.Context())
	_, err := svc.Wait(t.Context(), session)
	if err == nil || strings.Contains(err.Error(), session.Verifier) || strings.Contains(err.Error(), session.Nonce) || strings.Contains(err.Error(), "drt-secret") {
		t.Fatal("transport failure leaked authorization secrets")
	}
	if client.Timeout != 0 || NewService(ProviderIntl, nil).client.Timeout != http.DefaultClient.Timeout {
		t.Fatal("service added a network timeout")
	}
}
