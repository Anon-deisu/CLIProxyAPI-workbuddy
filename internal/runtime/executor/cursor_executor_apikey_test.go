package executor

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCursorExecutorExchangesAPIKeyCredentials(t *testing.T) {
	accessToken := executorTestJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer crsr-test-executor" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = fmt.Fprintf(w, `{"accessToken":%q,"refreshToken":"not-used"}`, accessToken)
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		Provider: "cursor",
		Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey,
			coreauth.AttributeAPIKey:   "crsr-test-executor",
			"base_url":                 server.URL,
		},
	}
	executor := NewCursorExecutor(&config.Config{})
	first, err := executor.ensureCredentials(t.Context(), auth)
	if err != nil {
		t.Fatalf("ensureCredentials() error = %v", err)
	}
	second, err := executor.ensureCredentials(t.Context(), auth)
	if err != nil {
		t.Fatalf("cached ensureCredentials() error = %v", err)
	}
	if first.AccessToken != accessToken || first.RefreshToken != "" {
		t.Fatalf("unexpected API key credentials: access=%t refresh=%t", first.AccessToken != "", first.RefreshToken != "")
	}
	if first.SessionID == "" || first.SessionID != second.SessionID {
		t.Fatalf("API key session identity is not stable: %q != %q", first.SessionID, second.SessionID)
	}
	if first.CookieJar == nil || first.CookieJar != second.CookieJar {
		t.Fatal("API key cookie jar is not stable")
	}
}

func executorTestJWT(expires time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, expires.Unix()))
	return header + "." + payload + ".signature"
}
