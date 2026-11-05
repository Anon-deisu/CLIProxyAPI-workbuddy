package helps

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type qoderCatalogTestTransport func(*http.Request) (*http.Response, error)

func (transport qoderCatalogTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func qoderCatalogTestAuth(t *testing.T, provider string) *coreauth.Auth {
	t.Helper()
	auth := &coreauth.Auth{ID: t.Name(), Provider: provider, Metadata: map[string]any{
		"uid": "catalog-user", "access_token": "catalog-test-access", "refresh_token": "catalog-test-refresh",
	}}
	t.Cleanup(func() {
		registry.DeleteQoderModelsForClient(qoderCatalogClientID(auth), provider)
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	return auth
}

// Route the actual signed GET through httptest without overriding account-site
// selection. The signature covers the original path and body, not the host.
func qoderCatalogTestContext(t *testing.T, auth *coreauth.Auth, server *httptest.Server) context.Context {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	gateway, errGateway := url.Parse(qoder.GatewayForAuth(auth))
	if errGateway != nil {
		t.Fatal(errGateway)
	}
	transport := qoderCatalogTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != gateway.Host || request.URL.Scheme != gateway.Scheme {
			t.Errorf("account %s requested %s", auth.Provider, request.URL.Redacted())
		}
		if _, deadline := request.Context().Deadline(); deadline {
			t.Error("catalog imposed a network deadline")
		}
		clone := request.Clone(request.Context())
		clone.URL.Scheme, clone.URL.Host, clone.Host = target.Scheme, target.Host, target.Host
		return server.Client().Transport.RoundTrip(clone)
	})
	return context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
}

func TestFetchQoderModelsSignedGETBodyAndRegionalCatalog(t *testing.T) {
	for _, provider := range []string{"qoder", "qoder-cn"} {
		t.Run(provider, func(t *testing.T) {
			auth := qoderCatalogTestAuth(t, provider)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Error(errRead)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/algo/api/v2/model/list?Encode=1" || string(body) != "$kwm" || !bytes.Equal(body, EncodeQoderBody([]byte("{}"))) {
					t.Errorf("incorrect signed GET: %s %s body=%q", r.Method, r.URL.RequestURI(), body)
				}
				if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cosy-User") != "catalog-user" || r.Header.Get("X-Model-Key") != "" || r.Header.Get("Cosy-Machinehostname") != "" {
					t.Error("catalog did not use the account's non-SSE signing identity")
				}
				authorization := r.Header.Get("Authorization")
				parts := strings.Split(strings.TrimPrefix(authorization, "Bearer COSY."), ".")
				if !strings.HasPrefix(authorization, "Bearer COSY.") || len(parts) != 2 || r.Header.Get("Cosy-Key") == "" || r.Header.Get("Cosy-Date") == "" {
					t.Error("catalog has no COSY signature")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				// Verify the protocol digest independently against bytes received on
				// the wire, catching a signer-only body that was omitted by GET.
				digest := md5.Sum([]byte(strings.Join([]string{parts[0], r.Header.Get("Cosy-Key"), r.Header.Get("Cosy-Date"), string(body), "/api/v2/model/list"}, "\n")))
				if parts[1] != hex.EncodeToString(digest[:]) {
					t.Error("signature does not cover the received body")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = io.WriteString(w, `{"chat":[null,3,"ignored",{"name":"missing key"},{"key":"mmodel","enable":false}],"completion":[{"key":"ignore"}]}`)
			}))
			defer server.Close()
			ctx := qoderCatalogTestContext(t, auth, server)
			models, err := FetchQoderModels(ctx, &config.Config{}, auth)
			if err != nil {
				t.Fatal(err)
			}
			wantName := "MiniMax-M3"
			if provider == "qoder-cn" {
				wantName = "MiniMax-M2.7"
			}
			if requests.Load() != 1 || len(models) != 1 || models[0].ID != wantName || models[0].MetadataModelID != "mmodel" || models[0].Type != provider {
				t.Fatalf("wrong regional catalog: %+v", models)
			}
			if !strings.Contains(models[0].Description, "enable=false") {
				t.Fatal("restricted model was filtered or described as available")
			}
			_, metadata, ok := registry.GetQoderCatalogForClient(auth.ID, provider)
			if !ok || metadata[0]["key"] != "mmodel" || metadata[0]["enable"] != false {
				t.Fatal("key or enable metadata lost")
			}
		})
	}
}

func TestResolveQoderModelRegionalAliases(t *testing.T) {
	for _, provider := range []string{"qoder", "qoder-cn"} {
		t.Run(provider, func(t *testing.T) {
			auth := qoderCatalogTestAuth(t, provider)
			auth.Prefix = "team"
			cases := map[string]string{
				"": "auto", " Auto ": "auto", "qoder-auto": "auto",
				"Qwen3.8-Max": "qmodel_38max", "qwen3.8-max-preview": "qmodel_38max", "qmodel_preview": "qmodel_38max",
				"QMODEL_38MAX": "qmodel_38max", "qmodel_38max (Qwen3.8-Max)": "qmodel_38max",
				"team/Qwen3.8-Max": "qmodel_38max", provider + "/Qwen3.8-Max": "qmodel_38max",
				"DeepSeek-V4-Flash": "dfmodel", "DeepSeek-V4-Pro": "dmodel", "Kimi-K2.7-Code": "kmodel", "Kimi-K3": "kmodel_latest",
				"Qwen3.8-Flash": "qfmodel", "Qwen3.7-Max": "qmodel_latest", "Qwen3.7-Plus": "qmodel",
				"Qwen3.6-Flash": "Qwen3.6-Flash",
				"mmodel":        "mmodel", "unlisted": "unlisted", "claude-sonnet-4-5": "claude-sonnet-4-5",
			}
			if provider == "qoder" {
				cases["MiniMax-M3"], cases["MiniMax-M2.7"], cases["MiniMax-M2.5"] = "mmodel", "MiniMax-M2.7", "MiniMax-M2.5"
				cases["mmodel (MiniMax-M2.7)"], cases["mmodel (MiniMax-M3)"] = "mmodel (MiniMax-M2.7)", "mmodel"
				cases["Sonus"], cases["Cantus"], cases["极致"] = "smodel", "cmodel", "ultimate"
				cases["GLM-5.2"], cases["Qwen3.7-Flash"] = "GLM-5.2", "Qwen3.7-Flash"
			} else {
				cases["MiniMax-M2.7"], cases["MiniMax-M2.5"], cases["MiniMax-M3"] = "mmodel", "mmodel", "MiniMax-M3"
				cases["mmodel (MiniMax-M3)"], cases["mmodel (MiniMax-M2.7)"] = "mmodel (MiniMax-M3)", "mmodel"
				cases["Sonus"], cases["Cantus"], cases["极致"] = "Sonus", "Cantus", "极致"
				cases["GLM-5.2"], cases["Qwen3.7-Flash"] = "gm51model", "q37fmodel"
			}
			for input, want := range cases {
				if got := ResolveQoderModel(auth, input); got != want {
					t.Errorf("resolve(%q) = %q, want %q", input, got, want)
				}
			}
		})
	}
}

func TestFetchQoderModelsGatewayOverrideAndAccountProxy(t *testing.T) {
	auth := qoderCatalogTestAuth(t, "qoder-cn")
	auth.Metadata["gateway_url"] = "http://catalog-gateway.invalid"
	auth.Metadata["openapi_url"] = "http://account-api.invalid"
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Host != "catalog-gateway.invalid" || r.URL.Path != "/algo/api/v2/model/list" || r.Method != http.MethodGet {
			t.Errorf("unexpected proxy destination: %s %s", r.Method, r.URL.Redacted())
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil || string(body) != "$kwm" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			t.Error("proxy request lost its signed GET body")
		}
		_, _ = io.WriteString(w, `{"chat":[]}`)
	}))
	defer proxy.Close()
	auth.ProxyURL = proxy.URL
	cfg := &config.Config{}
	cfg.ProxyURL = "http://config-proxy.invalid:1"
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderCatalogTestTransport(func(*http.Request) (*http.Response, error) {
		t.Error("account proxy did not take precedence over the context transport")
		return nil, errors.New("unexpected context transport")
	}))
	models, errFetch := FetchQoderModels(ctx, cfg, auth)
	if errFetch != nil || len(models) != 0 || requests.Load() != 1 {
		t.Fatalf("gateway/proxy override failed: %v, requests=%d", errFetch, requests.Load())
	}
}

func TestFetchQoderModelsDynamicKeysAliasesReplacementAndFailure(t *testing.T) {
	auth := qoderCatalogTestAuth(t, "qoder-cn")
	var response atomic.Value
	response.Store(`{"chat":[{"key":"qwen-new-key","display_name":"Qwen3.8-Max","enable":true,"aliases":["fast-qwen"],"name":"Qwen Custom"},{"key":"future-key","display_name":"Future-Model","enable":false}]}`)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, response.Load().(string))
	}))
	defer server.Close()
	ctx := qoderCatalogTestContext(t, auth, server)
	models, err := FetchQoderModels(ctx, nil, auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Qwen3.8-Max", "qwen3.8-max", "qwen-new-key", "qmodel_preview", "qwen3.8-max-preview", "fast-qwen", "Qwen Custom", "qwen-new-key (Qwen3.8-Max)"} {
		if got := ResolveQoderModel(auth, name); got != "qwen-new-key" {
			t.Errorf("dynamic resolve(%q) = %q", name, got)
		}
	}
	if got := ResolveQoderModel(auth, "Future-Model"); got != "future-key" {
		t.Fatalf("new model not resolved: %s", got)
	}
	alias := *models[0]
	alias.ID = "configured-alias"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{&alias})
	if got := ResolveQoderModel(auth, alias.ID); got != "qwen-new-key" {
		t.Fatalf("per-client alias not resolved: %s", got)
	}
	auth.Prefix = "team"
	alias.ID = "team/configured-alias"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{&alias})
	if got := ResolveQoderModel(auth, alias.ID); got != "qwen-new-key" {
		t.Fatalf("prefixed per-client alias not resolved: %s", got)
	}
	models[0].MetadataModelID = "caller-mutated"
	if got := ResolveQoderModel(auth, "Qwen3.8-Max"); got != "qwen-new-key" {
		t.Fatal("returned models share cached storage")
	}
	response.Store(`{"error":"catalog unavailable"}`)
	if failed, errFetch := FetchQoderModels(ctx, nil, auth); errFetch == nil || failed != nil {
		t.Fatal("upstream failure masqueraded as success")
	}
	if got := ResolveQoderModel(auth, "fast-qwen"); got != "qwen-new-key" {
		t.Fatal("failure erased the previous catalog")
	}
	response.Store(`{"chat":[{"key":"second-key","display_name":"Second-Model","aliases":["second"]}]}`)
	updated, errUpdate := FetchQoderModels(ctx, nil, auth)
	if errUpdate != nil || len(updated) != 1 || updated[0].ID != "Second-Model" {
		t.Fatalf("catalog not replaced: %v, %v", updated, errUpdate)
	}
	for _, name := range []string{"Qwen3.8-Max", "qmodel_preview", "fast-qwen", "team/configured-alias", "Future-Model"} {
		if got := ResolveQoderModel(auth, name); got != name {
			t.Errorf("removed %q was revived as %q", name, got)
		}
	}
	if got := ResolveQoderModel(auth, "second"); got != "second-key" {
		t.Fatalf("new alias not resolved: %s", got)
	}
	response.Store(`{"chat":[]}`)
	empty, errEmpty := FetchQoderModels(ctx, nil, auth)
	if errEmpty != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("successful empty catalog: %v, %v", empty, errEmpty)
	}
	for _, name := range []string{"Second-Model", "Qwen3.8-Max", "team/configured-alias"} {
		if got := ResolveQoderModel(auth, name); got != name {
			t.Errorf("empty catalog revived %q as %q", name, got)
		}
	}
	if requests.Load() != 4 {
		t.Fatalf("fetch was cached: %d requests", requests.Load())
	}
}

func TestFetchQoderModelsErrorsPreserveCatalog(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"forbidden", `{"chat":[]}`, http.StatusForbidden},
		{"invalid-json", `{`, http.StatusOK},
		{"no-chat", `{"code":403}`, http.StatusOK},
		{"error-with-chat", `{"code":403,"chat":[]}`, http.StatusOK},
		{"failed-with-chat", `{"success":false,"chat":[]}`, http.StatusOK},
		{"null-chat", `{"chat":null}`, http.StatusOK},
		{"object-chat", `{"chat":{}}`, http.StatusOK},
		{"bad-entry", `{"chat":[{"name":"missing key"}]}`, http.StatusOK},
		{"trailing-data", `{"chat":[]} {}`, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := qoderCatalogTestAuth(t, "qoder")
			before, err := registry.SetQoderModelsForClient(auth.ID, auth.Provider, []map[string]any{{"key": "kept", "display_name": "Kept"}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			if got, errFetch := FetchQoderModels(qoderCatalogTestContext(t, auth, server), nil, auth); errFetch == nil || got != nil {
				t.Fatal("invalid response accepted")
			}
			after, _, _ := registry.GetQoderCatalogForClient(auth.ID, auth.Provider)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed fetch erased the last successful catalog")
			}
		})
	}
	auth := qoderCatalogTestAuth(t, "qoder")
	sentinel := errors.New("catalog transport failed")
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", qoderCatalogTestTransport(func(*http.Request) (*http.Response, error) { return nil, sentinel }))
	if _, err := FetchQoderModels(ctx, nil, auth); !errors.Is(err, sentinel) {
		t.Fatalf("transport cause lost: %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := FetchQoderModels(canceled, nil, auth); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	for _, invalid := range []*coreauth.Auth{nil, {Provider: "other"}, {Provider: "qoder"}} {
		if _, err := FetchQoderModels(t.Context(), nil, invalid); err == nil {
			t.Fatal("accepted invalid account")
		}
	}
}

func TestResolveQoderModelAccountAndProviderIsolation(t *testing.T) {
	auth := qoderCatalogTestAuth(t, "qoder")
	other := qoderCatalogTestAuth(t, "qoder-cn")
	third := qoderCatalogTestAuth(t, "qoder")
	third.ID += "-other"
	for _, test := range []struct {
		auth *coreauth.Auth
		key  string
	}{{auth, "intl-key"}, {other, "cn-key"}, {third, "other-account-key"}} {
		if _, err := registry.SetQoderModelsForClient(test.auth.ID, test.auth.Provider, []map[string]any{{"key": test.key, "display_name": "Same-Name"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		auth *coreauth.Auth
		key  string
	}{{auth, "intl-key"}, {other, "cn-key"}, {third, "other-account-key"}} {
		if got := ResolveQoderModel(test.auth, "Same-Name"); got != test.key {
			t.Fatalf("cross-account/provider resolution: %s", got)
		}
	}
	// A catalog already registered by the caller is sufficient for resolution.
	registry.DeleteQoderModelsForClient(auth.ID, auth.Provider)
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "Only-Registered", Name: "Only-Registered", MetadataModelID: "registered-key", Type: auth.Provider}})
	if got := ResolveQoderModel(auth, "Only-Registered"); got != "registered-key" {
		t.Fatalf("registered metadata ignored: %s", got)
	}
	if got := ResolveQoderModel(auth, "Qwen3.8-Max"); got != "Qwen3.8-Max" {
		t.Fatal("static fallback expanded a registered dynamic catalog")
	}
	// An imported record without ID still has a stable account-scoped identity.
	imported := &coreauth.Auth{Provider: "qoder", Metadata: map[string]any{"uid": "imported-catalog-user"}}
	importedID := qoderCatalogClientID(imported)
	if importedID == "" || importedID != qoder.NewAuthRecord(imported.Provider, qoder.CredentialsFromAuth(imported)).ID {
		t.Fatal("imported account cache key differs from the assigned auth ID")
	}
	imported.Metadata["organization_id"] = "another-organization"
	if importedID == qoderCatalogClientID(imported) {
		t.Fatal("imported organizations share a catalog")
	}
}
