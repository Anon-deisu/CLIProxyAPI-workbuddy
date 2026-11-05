package executor

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// CodexExecutor executes Codex requests with optional fixed per-account proxy bindings.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg    *config.Config
	egress *helps.CodexEgressManager
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor {
	return &CodexExecutor{cfg: cfg.CloneForRuntime()}
}

func (e *CodexExecutor) Identifier() string { return "codex" }

// ForAPIKey isolates OAuth-only settings while retaining the shared egress keeper.
func (e *CodexExecutor) ForAPIKey() cliproxyauth.ProviderExecutor {
	copy := *e
	copy.cfg = e.cfg.ForAPIKey()
	return &copy
}

func (e *CodexExecutor) bindEgress(auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e.egress == nil && e.cfg != nil && auth != nil && auth.AuthKind() == cliproxyauth.AuthKindOAuth {
		pool := e.cfg.Routing.CodexStaticISP
		if pool != nil && pool.Enabled && len(pool.Nodes) > 0 {
			return nil, fmt.Errorf("Codex static ISP binding manager unavailable")
		}
	}
	return e.egress.BindEgress(auth)
}

func (e *CodexExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Codex.ModelLevelCooling
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *CodexExecutor) SupportsApplyPatch() bool { return e != nil }
