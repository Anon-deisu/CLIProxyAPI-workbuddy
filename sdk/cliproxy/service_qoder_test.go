package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type qoderServiceTransport func(*http.Request) (*http.Response, error)

func (f qoderServiceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func qoderServiceResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func qoderServiceAuth(t *testing.T, provider string) *coreauth.Auth {
	t.Helper()
	auth := qoder.NewAuthRecord(provider, qoder.Credentials{UID: t.Name(), Nickname: "Test account", AccessToken: "dt-service-old", RefreshToken: "drt-service-old"})
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(auth.ID)
		registry.DeleteQoderModelsForClient(auth.ID, provider)
	})
	return auth
}

func qoderServiceAssertModels(t *testing.T, auth *coreauth.Auth, want map[string]string) {
	t.Helper()
	got := make(map[string]string)
	for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
		if model == nil {
			t.Fatal("registered a nil model")
		}
		if model.Type != auth.Provider || model.OwnedBy != auth.Provider {
			t.Fatalf("model %q crossed provider boundaries", model.ID)
		}
		if _, duplicate := got[model.ID]; duplicate {
			t.Fatalf("duplicate model %q", model.ID)
		}
		got[model.ID] = model.MetadataModelID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

func TestQoderServiceBaselineExecutorsAndRefreshLead(t *testing.T) {
	s := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	s.registerAvailableExecutors(t.Context(), executorRegistrationOptions{includeBaseline: true})
	if s.codexEgress != nil {
		t.Cleanup(s.codexEgress.Close)
	}
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			exec, ok := s.coreManager.Executor(provider)
			if !ok || exec == nil {
				t.Fatal("baseline executor missing")
			}
			if _, okQoder := exec.(*runtimeexecutor.QoderExecutor); !okQoder || exec.Identifier() != provider {
				t.Fatalf("baseline executor = %T (%s)", exec, exec.Identifier())
			}
			lead := coreauth.ProviderRefreshLead(provider, nil)
			if lead == nil || *lead != 4*time.Minute {
				t.Fatalf("refresh lead = %v, want 4m", lead)
			}
			auth := qoderServiceAuth(t, provider)
			auth.Prefix = "tenant"
			auth.Metadata["qoder_auto_checkin"] = true
			auth.Metadata["expires_at"] = time.Now().Add(2 * time.Minute).Unix()
			if _, err := s.coreManager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderServiceTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != qoder.SiteForProvider(provider).OpenAPI+"/api/v1/deviceToken/refresh" || r.Method != http.MethodPost {
					return nil, fmt.Errorf("unexpected refresh endpoint: %s %s", r.Method, r.URL.Redacted())
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["refresh_token"] != "drt-service-old" {
					t.Error("refresh lost selected account token")
				}
				return qoderServiceResponse(r, http.StatusOK, `{"token":"dt-service-new","refresh_token":"drt-service-new","expires_in":3600000}`), nil
			}))
			refreshed, err := s.coreManager.ForceRefreshAuth(ctx, auth.ID)
			if err != nil {
				t.Fatal(err)
			}
			live, found := s.coreManager.GetByID(auth.ID)
			if !found || calls != 1 || refreshed.Provider != provider || live.Metadata["access_token"] != "dt-service-new" || live.Metadata["refresh_token"] != "drt-service-new" || live.Metadata["qoder_auto_checkin"] != true || live.Label != auth.Label || live.Prefix != auth.Prefix || live.LastRefreshedAt.IsZero() {
				t.Fatal("refresh did not preserve identity/settings and commit rotated credentials")
			}
			if expiry, okExpiry := live.ExpirationTime(); !okExpiry || !expiry.After(time.Now().Add(50*time.Minute)) {
				t.Fatal("refresh expiry was not propagated in seconds")
			}
		})
	}
}

func TestQoderServiceDefaultManagerDeviceLogin(t *testing.T) {
	previousStore, previousTransport := sdkauth.GetTokenStore(), http.DefaultTransport
	t.Cleanup(func() { sdkauth.RegisterTokenStore(previousStore); http.DefaultTransport = previousTransport })
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			store := sdkauth.NewFileTokenStore()
			cfg := &config.Config{AuthDir: t.TempDir()}
			store.SetBaseDir(cfg.AuthDir)
			sdkauth.RegisterTokenStore(store)
			calls := 0
			http.DefaultTransport = qoderServiceTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme+"://"+r.URL.Host != qoder.SiteForProvider(provider).OpenAPI || r.Method != http.MethodGet {
					return nil, fmt.Errorf("unexpected login destination: %s", r.URL.Redacted())
				}
				switch r.URL.Path {
				case "/api/v1/deviceToken/poll":
					if r.URL.Query().Get("nonce") == "" || r.URL.Query().Get("verifier") == "" {
						t.Error("device flow omitted session binding")
					}
					return qoderServiceResponse(r, 200, `{"token":"dt-login-synthetic","refresh_token":"drt-login-synthetic","user_id":"9007199254740993","expires_in":3600000}`), nil
				case "/api/v1/userinfo":
					return qoderServiceResponse(r, 200, `{"id":"9007199254740993","name":"Login account","organization_id":"test-org"}`), nil
				default:
					return nil, fmt.Errorf("unexpected login path %s", r.URL.Path)
				}
			})
			auth, path, err := newDefaultAuthManager().Login(t.Context(), provider, cfg, &sdkauth.LoginOptions{NoBrowser: true})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || auth.Provider != provider || auth.Label != "Login account" || path == "" {
				t.Fatal("default manager did not complete regional device login")
			}
			saved, err := store.List(t.Context())
			if err != nil || len(saved) != 1 {
				t.Fatalf("stored login count=%d err=%v", len(saved), err)
			}
			creds := qoder.CredentialsFromAuth(saved[0])
			if saved[0].ID != auth.ID || saved[0].Provider != provider || creds.UID != "9007199254740993" || creds.OrganizationID != "test-org" || creds.Realm != qoder.SiteForProvider(provider).Realm || creds.AccessToken != "dt-login-synthetic" {
				t.Fatal("stored login lost regional identity or credentials")
			}
		})
	}
}

func TestQoderServiceModelCatalogFallbackAndRemoval(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			auth := qoderServiceAuth(t, provider)
			s := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
			if _, err := s.coreManager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			status, payload, calls := http.StatusServiceUnavailable, `{"error":"synthetic failure"}`, 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderServiceTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != qoder.SiteForProvider(provider).Gateway+"/algo/api/v2/model/list?Encode=1" || r.Method != http.MethodGet {
					return nil, fmt.Errorf("incorrect regional discovery endpoint %s", r.URL.Redacted())
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "$kwm" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") || r.Header.Get("Cosy-User") != t.Name() {
					t.Error("service discovery lost signed request identity/body")
				}
				return qoderServiceResponse(r, status, payload), nil
			}))
			s.registerModelsForAuth(ctx, auth)
			fallback := make(map[string]string)
			for _, model := range registry.GetQoderModels(provider) {
				fallback[model.ID] = model.MetadataModelID
			}
			qoderServiceAssertModels(t, auth, fallback)
			if _, _, found := registry.GetQoderCatalogForClient(auth.ID, provider); found {
				t.Fatal("failed discovery was retained as a successful catalog")
			}

			status, payload = http.StatusOK, `{"chat":[{"key":"mmodel"},{"key":"service-future","display_name":"Future","max_input_tokens":8192}]}`
			s.registerModelsForAuth(ctx, auth)
			regional := "MiniMax-M3"
			if provider == qoder.ProviderCN {
				regional = "MiniMax-M2.7"
			}
			want := map[string]string{regional: "mmodel", "Future": "service-future"}
			qoderServiceAssertModels(t, auth, want)
			previous, metadata, found := registry.GetQoderCatalogForClient(auth.ID, provider)
			if !found || len(previous) != 2 || len(metadata) != 2 {
				t.Fatal("successful catalog was not retained")
			}
			for _, failure := range []struct {
				status int
				body   string
			}{
				{503, `{"error":"unavailable"}`}, {200, `{"chat":null}`}, {200, `{"success":false,"chat":[]}`},
			} {
				status, payload = failure.status, failure.body
				s.registerModelsForAuth(ctx, auth)
				qoderServiceAssertModels(t, auth, want)
			}

			status, payload = 200, `{"chat":[]}`
			s.registerModelsForAuth(ctx, auth)
			qoderServiceAssertModels(t, auth, map[string]string{})
			empty, _, found := registry.GetQoderCatalogForClient(auth.ID, provider)
			if !found || len(empty) != 0 {
				t.Fatal("empty success became a cache miss")
			}
			status = 503
			s.registerModelsForAuth(ctx, auth)
			qoderServiceAssertModels(t, auth, map[string]string{})
			if got := helps.ResolveQoderModel(auth, regional); got != regional {
				t.Fatal("empty catalog resurrected a static model")
			}

			status, payload = 200, `{"chat":[{"key":"service-new","display_name":"Replacement"}]}`
			s.registerModelsForAuth(ctx, auth)
			qoderServiceAssertModels(t, auth, map[string]string{"Replacement": "service-new"})
			other := qoderServiceAuth(t, provider)
			other.ID += "-other"
			other, errRegister := s.coreManager.Register(ctx, other)
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			s.registerModelsForAuth(ctx, other)
			otherRegion := qoder.ProviderIntl
			if provider == otherRegion {
				otherRegion = qoder.ProviderCN
			}
			if _, err := registry.SetQoderModelsForClient(auth.ID, otherRegion, []map[string]any{{"key": "other-region"}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { registry.DeleteQoderModelsForClient(auth.ID, otherRegion) })
			current, found := s.coreManager.GetByID(auth.ID)
			if !found || current == nil {
				t.Fatal("registered account disappeared before disable")
			}
			disabled := current.Clone()
			disabled.Disabled = true
			disabled.Status = coreauth.StatusDisabled
			disabled, errUpdate := s.coreManager.Update(ctx, disabled)
			if errUpdate != nil {
				t.Fatal(errUpdate)
			}
			before := calls
			s.registerModelsForAuth(ctx, disabled)
			qoderServiceAssertModels(t, auth, map[string]string{})
			if calls != before {
				t.Fatal("disabled account fetched models")
			}
			enabled := disabled.Clone()
			enabled.Disabled = false
			enabled.Status = coreauth.StatusActive
			enabled, errUpdate = s.coreManager.Update(ctx, enabled)
			if errUpdate != nil {
				t.Fatal(errUpdate)
			}
			s.registerModelsForAuth(ctx, enabled)
			s.applyCoreAuthRemoval(ctx, auth.ID)
			qoderServiceAssertModels(t, auth, map[string]string{})
			if _, ok := s.coreManager.GetByID(auth.ID); ok {
				t.Fatal("removed auth remains registered")
			}
			if _, _, found := registry.GetQoderCatalogForClient(auth.ID, provider); found {
				t.Fatal("removed account retained its catalog")
			}
			qoderServiceAssertModels(t, other, map[string]string{"Replacement": "service-new"})
			if _, _, found := registry.GetQoderCatalogForClient(auth.ID, otherRegion); !found {
				t.Fatal("removal cleared another provider catalog")
			}
		})
	}
}

func TestQoderServiceModelAliasesExclusionsAndPrefixes(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/forced=%t", provider, force), func(t *testing.T) {
				auth := qoderServiceAuth(t, provider)
				auth.Prefix = "tenant"
				other := qoder.ProviderIntl
				if provider == other {
					other = qoder.ProviderCN
				}
				cfg := &config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: force}, OAuthExcludedModels: map[string][]string{provider: {"Blocked*"}}, OAuthModelAlias: map[string][]config.OAuthModelAlias{
					provider: {{Name: "Future", Alias: "public"}}, other: {{Name: "Future", Alias: "wrong-region"}},
				}}
				s := &Service{cfg: cfg}
				status := http.StatusOK
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderServiceTransport(func(r *http.Request) (*http.Response, error) {
					return qoderServiceResponse(r, status, `{"chat":[{"key":"future-key","display_name":"Future"},{"key":"blocked-key","display_name":"Blocked model"}]}`), nil
				}))
				assertAlias := func(alias string) {
					t.Helper()
					want := map[string]string{"tenant/" + alias: "future-key"}
					if !force {
						want[alias] = "future-key"
					}
					qoderServiceAssertModels(t, auth, want)
					if got := helps.ResolveQoderModel(auth, "tenant/"+alias); got != "future-key" {
						t.Fatalf("alias resolves to %q", got)
					}
				}
				s.registerModelsForAuth(ctx, auth)
				assertAlias("public")
				// Reapply current settings to retained raw models, never to old public aliases.
				status = 503
				cfg.OAuthModelAlias[provider] = []config.OAuthModelAlias{{Name: "Future", Alias: "renamed"}}
				s.registerModelsForAuth(ctx, auth)
				assertAlias("renamed")
				for range 2 {
					s.registerModelsForAuth(ctx, auth)
					assertAlias("renamed")
				}
				coreauth.SetOAuthModelAliasesAttribute(auth, []config.OAuthModelAlias{{Name: "Future", Alias: "account-alias"}})
				cfg.OAuthModelAlias[provider] = nil
				s.registerModelsForAuth(ctx, auth)
				assertAlias("account-alias")
				auth.Attributes["excluded_models"] = "Future"
				s.registerModelsForAuth(ctx, auth)
				want := map[string]string{"tenant/Blocked model": "blocked-key"}
				if !force {
					want["Blocked model"] = "blocked-key"
				}
				qoderServiceAssertModels(t, auth, want)
				auth.Attributes["excluded_models"] = "*"
				s.registerModelsForAuth(ctx, auth)
				qoderServiceAssertModels(t, auth, map[string]string{})
				delete(auth.Attributes, "excluded_models")
				s.registerModelsForAuth(ctx, auth)
				assertAlias("account-alias")
			})
		}
	}
}
