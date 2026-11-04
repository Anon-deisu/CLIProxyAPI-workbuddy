package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var _ Authenticator = (*QoderAuthenticator)(nil)

type qoderSDKTestTransport func(*http.Request) (*http.Response, error)

func (f qoderSDKTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestQoderAuthenticatorBrowserPollingBothSites(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			a := NewQoderAuthenticator(provider)
			if a.Provider() != provider || a.RefreshLead() == nil || *a.RefreshLead() != 4*time.Minute {
				t.Fatal("incorrect authenticator contract")
			}
			var calls int
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch r.URL.Path {
				case "/api/v1/deviceToken/poll":
					if r.URL.Query().Get("verifier") == "" || r.URL.Query().Get("challenge_method") != "S256" {
						t.Error("missing PKCE poll parameters")
					}
					_, _ = w.Write([]byte(`{"token":"dt-sdk","refresh_token":"drt-sdk","expires_in":120000}`))
				case "/api/v1/userinfo":
					if r.Header.Get("Authorization") != "Bearer dt-sdk" {
						t.Error("missing userinfo token")
					}
					_, _ = w.Write([]byte(`{"id":"sdk-user","name":"Display Name","organization_id":"sdk-org"}`))
				default:
					t.Errorf("unexpected SDK login request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			http.DefaultTransport = qoderSDKTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Scheme+"://"+req.URL.Host != qoder.SiteForProvider(provider).OpenAPI {
					t.Error("SDK used the wrong regional API")
				}
				copyReq := req.Clone(req.Context())
				copyReq.URL.Scheme, copyReq.URL.Host = target.Scheme, target.Host
				return server.Client().Transport.RoundTrip(copyReq)
			})
			before := time.Now().Unix()
			output := captureStdout(t, func() {
				record, err := a.Login(t.Context(), &config.Config{}, &LoginOptions{NoBrowser: true, Prompt: func(string) (string, error) {
					t.Error("device polling requested a pasted code")
					return "", errors.New("unexpected prompt")
				}})
				if err != nil {
					t.Error(err)
					return
				}
				creds := qoder.CredentialsFromAuth(record)
				if record.Provider != provider || record.Label != "Display Name" || creds.UID != "sdk-user" || creds.OrganizationID != "sdk-org" || creds.Email != "" || creds.MachineID == "" || creds.Realm != qoder.SiteForProvider(provider).Realm || creds.ExpiresAt < before+120 || creds.ExpiresAt > time.Now().Unix()+120 {
					t.Error("SDK login lost identity, machine or expiry")
				}
				if record.FileName != qoder.NewAuthRecord(provider, creds).FileName {
					t.Error("SDK returned an unstable account filename")
				}
			})
			if calls != 2 || !strings.Contains(output, qoder.SiteForProvider(provider).Website+"/device/selectAccounts?") || strings.Contains(output, "verifier=") || strings.Contains(output, "dt-sdk") {
				t.Fatal("unexpected browser instructions or token output")
			}
		})
	}
}

func TestQoderAuthenticatorRejectsInvalidConfigurationAndCancellation(t *testing.T) {
	a := NewQoderAuthenticator(qoder.ProviderIntl)
	if _, err := a.Login(t.Context(), nil, &LoginOptions{NoBrowser: true}); err == nil {
		t.Fatal("missing config accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Login(ctx, &config.Config{}, &LoginOptions{NoBrowser: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled SDK login: %v", err)
	}
}
