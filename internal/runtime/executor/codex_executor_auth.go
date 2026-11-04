package executor

import (
	"context"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		if tokenData, errRelogin := e.reloginAutoOAuth(ctx, auth, ""); errRelogin == nil {
			applyCodexTokenData(auth, tokenData)
			return auth, nil
		} else if _, okAuto := codexauth.AutoOAuthCredentials(auth.Metadata); okAuto {
			return nil, errRelogin
		}
		return auth, nil
	}
	bound, errBinding := e.bindEgress(auth)
	if errBinding != nil {
		return nil, errBinding
	}
	svc := codexauth.NewCodexAuthWithProxyURL(e.cfg, bound.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		if tokenData, errRelogin := e.reloginAutoOAuth(ctx, auth, bound.ProxyURL); errRelogin == nil {
			applyCodexTokenData(auth, tokenData)
			return auth, nil
		} else if _, okAuto := codexauth.AutoOAuthCredentials(auth.Metadata); okAuto {
			return nil, errRelogin
		}
		return nil, err
	}
	applyCodexTokenData(auth, td)
	return auth, nil
}

func (e *CodexExecutor) reloginAutoOAuth(ctx context.Context, auth *cliproxyauth.Auth, proxyURL string) (*codexauth.CodexTokenData, error) {
	account, ok := codexauth.AutoOAuthCredentials(auth.Metadata)
	if !ok {
		return nil, statusErr{code: 401, msg: "codex automatic OAuth recovery is not enabled"}
	}
	workspaceID, workspaceName := "", ""
	if stored, okStored := auth.Metadata[codexauth.AutoOAuthKey].(map[string]any); okStored {
		workspaceID, _ = stored["workspace_id"].(string)
		workspaceName, _ = stored["workspace_name"].(string)
	}
	result, errLogin := codexauth.AutoOAuthLogin(ctx, e.cfg, account, proxyURL, workspaceID, workspaceName)
	if errLogin != nil {
		return nil, errLogin
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	stored := account.Metadata(result.WorkspaceID, result.WorkspaceName)
	stored["account_id"] = result.Bundle.TokenData.AccountID
	auth.Metadata[codexauth.AutoOAuthKey] = stored
	auth.Metadata[codexauth.AutoOAuthRecoveryKey] = map[string]any{"enabled": true, "status": "active"}
	return &result.Bundle.TokenData, nil
}

func applyCodexTokenData(auth *cliproxyauth.Auth, td *codexauth.CodexTokenData) {
	if auth == nil || td == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.CodexKey {
		entry := &e.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range e.cfg.CodexKey {
			entry := &e.cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}
