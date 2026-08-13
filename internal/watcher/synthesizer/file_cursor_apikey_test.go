package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSynthesizeCursorAPIKeyClassification(t *testing.T) {
	path := "/srv/auth/me_cursor-api-key.json"
	ctx := &SynthesisContext{
		AuthDir:     "/srv/auth",
		Now:         time.Now(),
		Config:      &config.Config{},
		IDGenerator: NewStableIDGenerator(),
	}
	auths := synthesizeFileAuths(ctx, path, []byte(`{
		"type":"cursor-api-key",
		"api_key":"sk-test-123",
		"base_url":"https://api2.cursor.sh",
		"prefix":"ck1",
		"priority":5,
		"excluded_models":["gpt-4o"]
	}`))
	if len(auths) != 1 {
		t.Fatalf("want 1 auth, got %d", len(auths))
	}
	a := auths[0]
	if a.Provider != "cursor-api-key" {
		t.Errorf("Provider = %q, want cursor-api-key", a.Provider)
	}
	if a.Attributes[coreauth.AttributeAPIKey] != "sk-test-123" {
		t.Errorf("AttributeAPIKey = %q", a.Attributes[coreauth.AttributeAPIKey])
	}
	if a.Attributes["base_url"] != "https://api2.cursor.sh" {
		t.Errorf("base_url attr = %q", a.Attributes["base_url"])
	}
	if a.Attributes["provider"] != "cursor" {
		t.Errorf("provider attr = %q, want cursor", a.Attributes["provider"])
	}
	if a.Attributes["priority"] != "5" {
		t.Errorf("priority attr = %q, want 5", a.Attributes["priority"])
	}
	if k := a.AuthKind(); k != coreauth.AuthKindAPIKey {
		t.Errorf("AuthKind = %q, want apikey", k)
	}
	if s := a.AuthSourceKind(); s != coreauth.AuthSourceFile {
		t.Errorf("AuthSourceKind = %q, want file", s)
	}
	if a.ID != "me_cursor-api-key.json" {
		t.Errorf("ID = %q, want me_cursor-api-key.json", a.ID)
	}
}
