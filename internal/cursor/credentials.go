package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
)

// CredentialsFromAPIKey exchanges a Cursor API key and builds the stable
// per-key identity required by the Agent Connect transport.
func CredentialsFromAPIKey(ctx context.Context, apiKey, baseURL string, metadata map[string]any) (AccountCredentials, error) {
	apiKey = strings.TrimSpace(apiKey)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = cursorauth.DefaultBaseURL
	}
	result, err := cursorauth.ResolveAPIKeyToken(ctx, apiKey, baseURL)
	if err != nil {
		return AccountCredentials{}, err
	}

	merged := make(map[string]any, len(metadata)+3)
	for key, value := range metadata {
		merged[key] = value
	}
	merged["access_token"] = result.AccessToken
	delete(merged, "refresh_token")
	merged["base_url"] = baseURL

	identity := sha256.Sum256([]byte(baseURL + "\x00" + apiKey))
	identityHex := hex.EncodeToString(identity[:])
	if value, _ := merged["session_id"].(string); strings.TrimSpace(value) == "" {
		merged["session_id"] = uuid.NewSHA1(uuid.NameSpaceOID, identity[:]).String()
	}

	creds := CredentialsFromMetadata(merged)
	creds.RefreshToken = ""
	creds.CookieJar = CookieJarForAccount("cursor-api-key:" + identityHex)
	return creds, nil
}
