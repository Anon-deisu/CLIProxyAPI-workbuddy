package kimi

import (
	"net/url"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	KimiDefaultDomain = "kimi.com"
	KimiAIDomain      = "kimi.ai"
	KimiOAuthHost     = "https://auth.kimi.com"
	KimiAIOAuthHost   = "https://auth.kimi.ai"
	KimiAIAPIBaseURL  = "https://api.kimi.ai/coding"
)

// IsKimiAIDomain accepts the supported Kimi.ai provider and domain names.
func IsKimiAIDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	return domain == "kimi.ai" || domain == "ai" || domain == "kimi-ai" || strings.HasSuffix(domain, ".kimi.ai")
}

// IsKimiComDomain accepts the supported Kimi.com provider and domain names.
func IsKimiComDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	return domain == "kimi.com" || domain == "com" || domain == "kimi" || strings.HasSuffix(domain, ".kimi.com")
}

// NormalizeKimiDomain returns the canonical domain used for token acquisition.
func NormalizeKimiDomain(domain string) string {
	if IsKimiAIDomain(domain) {
		return KimiAIDomain
	}
	return KimiDefaultDomain
}

// ResolveKimiOAuthHost returns the authorization server for a Kimi domain.
func ResolveKimiOAuthHost(domain string) string {
	if IsKimiAIDomain(domain) {
		return KimiAIOAuthHost
	}
	return KimiOAuthHost
}

// ResolveKimiAPIBaseURL returns the model endpoint for a Kimi domain.
func ResolveKimiAPIBaseURL(domain string) string {
	if IsKimiAIDomain(domain) {
		return KimiAIAPIBaseURL
	}
	return KimiAPIBaseURL
}

func kimiDomainFromURL(rawURL string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if IsKimiAIDomain(host) {
		return KimiAIDomain
	}
	if IsKimiComDomain(host) {
		return KimiDefaultDomain
	}
	return ""
}

// ResolveKimiDomainFromAuth prioritizes explicit domain and endpoint metadata,
// then storage, provider and filename. Kimi.ai tokens never use Kimi.com refresh.
func ResolveKimiDomainFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return KimiDefaultDomain
	}
	resolve := func(domain, base, provider string) string {
		if IsKimiAIDomain(domain) {
			return KimiAIDomain
		}
		if IsKimiComDomain(domain) {
			return KimiDefaultDomain
		}
		if known := kimiDomainFromURL(base); known != "" {
			return known
		}
		if IsKimiAIDomain(provider) {
			return KimiAIDomain
		}
		if IsKimiComDomain(provider) {
			return KimiDefaultDomain
		}
		return ""
	}
	if domain := resolve(auth.Attributes["domain"], auth.Attributes["base_url"], ""); domain != "" {
		return domain
	}
	domain, _ := auth.Metadata["domain"].(string)
	base, _ := auth.Metadata["base_url"].(string)
	provider, _ := auth.Metadata["type"].(string)
	if known := resolve(domain, base, provider); known != "" {
		return known
	}
	if storage, ok := auth.Storage.(*KimiTokenStorage); ok && storage != nil {
		if known := resolve(storage.Domain, storage.BaseURL, storage.Type); known != "" {
			return known
		}
	}
	if known := resolve("", "", auth.Provider); known != "" {
		return known
	}
	name := strings.ToLower(auth.ID + " " + auth.FileName)
	if strings.Contains(name, "kimi-ai") || strings.Contains(name, "kimi.ai") {
		return KimiAIDomain
	}
	return KimiDefaultDomain
}

// IsKimiAIAuth reports whether the credential targets Kimi.ai.
func IsKimiAIAuth(auth *coreauth.Auth) bool {
	return IsKimiAIDomain(ResolveKimiDomainFromAuth(auth))
}
