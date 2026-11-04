package cmd

import (
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
)

// newAuthManager creates a new authentication manager instance with all supported
// authenticators and a file-based token store. It initializes authenticators for
// Codex, Claude, Antigravity, Kimi, and xAI providers.
//
// Returns:
//   - *sdkAuth.Manager: A configured authentication manager instance
func newAuthManager() *sdkAuth.Manager {
	store := sdkAuth.GetTokenStore()
	manager := sdkAuth.NewManager(store,
		sdkAuth.NewCodexAuthenticator(),
		sdkAuth.NewClaudeAuthenticator(),
		sdkAuth.NewAntigravityAuthenticator(),
		sdkAuth.NewKimiAuthenticator(),
		sdkAuth.NewKimiAIAuthenticator(),
		sdkAuth.NewKimiAIDotAuthenticator(),
		sdkAuth.NewXAIAuthenticator(),
		sdkAuth.NewMetaAuthenticator(),
		sdkAuth.NewDevinAuthenticator(),
		sdkAuth.NewWorkBuddyAuthenticator("workbuddy"),
		sdkAuth.NewWorkBuddyAuthenticator("workbuddy-ai"),
		sdkAuth.NewQoderAuthenticator("qoder"),
		sdkAuth.NewQoderAuthenticator("qoder-cn"),
		sdkAuth.NewCursorAuthenticator(),
	)
	return manager
}
