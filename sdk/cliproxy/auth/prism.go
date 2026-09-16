package auth

import (
	"context"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (m *Manager) prismEnabled() bool {
	cfg := m.runtimeConfigSnapshot()
	return cfg != nil && cfg.Routing.Prism != nil && *cfg.Routing.Prism
}

func prismPlusCredential(a *Auth) bool {
	if a.Provider != "codex" || a.IsDispatchDisabled() {
		return false
	}
	token, _ := a.Metadata["access_token"].(string)
	if token == "" {
		return false
	}
	plan := a.Attributes["plan_type"]
	if plan == "" {
		plan, _ = a.Metadata["chatgpt_plan_type"].(string)
	}
	if plan == "" {
		plan, _ = a.Metadata["plan_type"].(string)
	}
	if plan == "" {
		if claims, err := codex.ParseJWTToken(token); err == nil {
			plan = claims.CodexAuthInfo.ChatgptPlanType
		}
	}
	return strings.EqualFold(strings.TrimSpace(plan), "plus")
}

func (m *Manager) pickPrism(ctx context.Context, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	m.mu.Lock()
	defer m.mu.Unlock()
	exec := m.executors["codex"]
	if exec == nil {
		return nil, nil, &Error{Code: "executor_not_found", Message: "Codex executor not registered"}
	}
	var candidates []*Auth
	for _, a := range m.auths {
		if !prismPlusCredential(a) || !eligibility.allows(a) || (pinned != "" && a.ID != pinned) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil, nil, &Error{Code: "auth_not_found", Message: "Prism requires an enabled Plus account with an access token"}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	start := int(m.prismOffset % uint64(len(candidates)))
	hasUntried := false
	for i := range candidates {
		a := candidates[(start+i)%len(candidates)]
		if _, used := tried[a.ID]; used {
			continue
		}
		hasUntried = true
		if !a.TryAcquireDispatchSlot() {
			continue
		}
		m.prismOffset += uint64(i + 1)
		a.EnsureIndex()
		selected := a.Clone()
		selected.PrismRoute = true
		opts.Metadata[cliproxyexecutor.PrismRouteMetadataKey] = true
		return selected, exec, nil
	}
	if !hasUntried {
		return nil, nil, &Error{Code: "auth_not_found", Message: "all enabled Plus accounts have been attempted"}
	}
	return nil, nil, NewConcurrencyLimitError()
}
