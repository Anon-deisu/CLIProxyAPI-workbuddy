package cursor

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveAPIKeyTokenExchangesAndCaches(t *testing.T) {
	var requests atomic.Int32
	accessToken := testJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != apiKeyExchangePath {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-cursor-key" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"accessToken":%q,"refreshToken":"unused"}`, accessToken)
	}))
	defer server.Close()

	apiKeyTokenCache.Lock()
	apiKeyTokenCache.entries = make(map[string]cachedAPIKeyToken)
	apiKeyTokenCache.Unlock()

	first, err := ResolveAPIKeyToken(t.Context(), " test-cursor-key ", server.URL)
	if err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	second, err := ResolveAPIKeyToken(t.Context(), "test-cursor-key", server.URL+"/")
	if err != nil {
		t.Fatalf("cached exchange: %v", err)
	}
	if first.AccessToken != accessToken || second.AccessToken != accessToken {
		t.Fatal("resolved access token mismatch")
	}
	if first.RefreshToken != "" || second.RefreshToken != "" {
		t.Fatal("exchange refresh token must not be retained; renewal uses the original API key")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("exchange requests = %d, want 1", got)
	}
}

func TestResolveAPIKeyTokenReExchangesOriginalKeyBeforeJWTExpiry(t *testing.T) {
	var requests atomic.Int32
	accessToken := testJWT(time.Now().Add(4 * time.Minute))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer original-api-key" {
			t.Fatalf("renewal authorization = %q", got)
		}
		_, _ = fmt.Fprintf(w, `{"accessToken":%q,"refreshToken":"must-not-be-reused"}`, accessToken)
	}))
	defer server.Close()

	apiKeyTokenCache.Lock()
	apiKeyTokenCache.entries = make(map[string]cachedAPIKeyToken)
	apiKeyTokenCache.Unlock()

	for i := 0; i < 2; i++ {
		result, err := ResolveAPIKeyToken(t.Context(), "original-api-key", server.URL)
		if err != nil {
			t.Fatalf("exchange %d: %v", i+1, err)
		}
		if result.RefreshToken != "" {
			t.Fatal("exchange response refresh token must not be retained")
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("exchange requests = %d, want 2 for a JWT inside the refresh window", got)
	}
}

func TestExchangeAPIKeyRejectsUnauthorizedWithoutLeakingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected secret-key-value", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := NewAuthService().ExchangeAPIKey(t.Context(), "secret-key-value", server.URL)
	if err == nil {
		t.Fatal("expected unauthorized exchange error")
	}
	if got := err.Error(); got != "cursor API key exchange failed: HTTP 401" {
		t.Fatalf("unexpected sanitized error: %q", got)
	}
}

func testJWT(expires time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, expires.Unix()))
	return header + "." + payload + ".signature"
}
