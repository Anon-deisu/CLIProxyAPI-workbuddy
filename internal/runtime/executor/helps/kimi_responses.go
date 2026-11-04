package helps

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	kimiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ResolveKimiBaseURL respects explicit endpoints before selecting the domain default.
func ResolveKimiBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/"); raw != "" {
			return raw
		}
	}
	if auth != nil {
		if raw, ok := auth.Metadata["base_url"].(string); ok && strings.TrimSpace(raw) != "" {
			return strings.TrimRight(strings.TrimSpace(raw), "/")
		}
	}
	return kimiauth.ResolveKimiAPIBaseURL(kimiauth.ResolveKimiDomainFromAuth(auth))
}

// ResolveKimiResponsesURL resolves the domain-specific Responses endpoint.
func ResolveKimiResponsesURL(auth *cliproxyauth.Auth) string {
	base := ResolveKimiBaseURL(auth)
	if strings.HasSuffix(base, "/v1") {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

// ResolveKimiChatURL resolves the domain-specific Chat Completions endpoint.
func ResolveKimiChatURL(auth *cliproxyauth.Auth) string {
	base := ResolveKimiBaseURL(auth)
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// ResolveKimiClaudeBaseURL resolves the domain-specific Messages delegation endpoint.
func ResolveKimiClaudeBaseURL(auth *cliproxyauth.Auth) string {
	return strings.TrimSuffix(ResolveKimiBaseURL(auth), "/v1")
}
