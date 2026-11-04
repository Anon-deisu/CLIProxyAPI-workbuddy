package codex

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

const (
	AutoOAuthKey            = "codex_auto_oauth"
	AutoOAuthRecoveryKey    = "codex_auto_oauth_recovery"
	AutoOAuthOrigin         = "cpa-auto-oauth-v1"
	AutoOAuthMaxAccounts    = 100
	AutoOAuthMaxConcurrency = 8
	AutoOAuthTimeout        = 5 * time.Minute
)

//go:embed auto_oauth_runner.cjs
var autoOAuthBrowserScript string

var autoOAuthSlots = make(chan struct{}, AutoOAuthMaxConcurrency)

type AutoOAuthAccount struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret"`
}

type AutoOAuthResult struct {
	Bundle        *CodexAuthBundle
	WorkspaceID   string
	WorkspaceName string
}

type AutoOAuthProgress func(step string, percent int)

// AutoOAuthError exposes only fixed, non-sensitive diagnostics to callers.
type AutoOAuthError struct {
	Code string
}

func (e *AutoOAuthError) Error() string {
	switch e.Code {
	case "account_banned":
		return "账号被封禁（登录服务明确返回停用或封禁）"
	case "invalid_credentials":
		return "账号密码或 2FA 验证失败，需要人工处理"
	case "manual_required":
		return "需要人工完成验证码、邮箱验证或额外登录验证"
	case "workspace_mismatch":
		return "工作空间不匹配，未覆盖原认证文件"
	case "identity_mismatch":
		return "登录邮箱与输入账号不匹配，未保存认证文件"
	case "browser_unavailable":
		return "自动 OAuth 浏览器未就绪，请安装 Node.js、Playwright 和 Chromium"
	case "browser_launch_failed":
		return "自动 OAuth 浏览器启动失败，请检查浏览器运行目录和权限"
	case "browser_protocol_error":
		return "自动 OAuth 浏览器进程未返回有效结果"
	case "navigation_failed":
		return "自动 OAuth 无法打开登录页面，请检查登录代理连接"
	case "login_timeout":
		return "自动 OAuth 等待当前登录步骤超时，需要人工检查页面"
	case "cloudflare_blocked":
		return "登录页安全验证未完成，请人工验证后再登录"
	case "proxy_invalid":
		return "自动 OAuth 代理配置无效"
	case "canceled":
		return "自动 OAuth 已取消"
	case "timeout":
		return "自动 OAuth 超时，需要人工检查登录步骤"
	case "invalid_callback":
		return "OAuth 回调校验失败"
	case "token_exchange_failed":
		return "OAuth token 交换失败"
	default:
		return "自动 OAuth 失败，请检查网络或改为人工登录"
	}
}

func (e *AutoOAuthError) AutoOAuthFailureCode() string { return e.Code }

func (e *AutoOAuthError) StatusCode() int {
	switch e.Code {
	case "account_banned", "invalid_credentials":
		return http.StatusUnauthorized
	default:
		return 0
	}
}

func (e *AutoOAuthError) Unwrap() error {
	switch e.Code {
	case "canceled":
		return context.Canceled
	case "timeout", "login_timeout":
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// Preserve context signals without exposing sensitive upstream error details.
func newAutoOAuthError(code string, err error) *AutoOAuthError {
	switch {
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	}
	return &AutoOAuthError{Code: code}
}

func ParseAutoOAuthAccounts(raw string) ([]AutoOAuthAccount, error) {
	var accounts []AutoOAuthAccount
	seen := make(map[string]bool)
	for i, line := range strings.Split(strings.TrimPrefix(raw, "\ufeff"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		first, last := strings.Index(line, "----"), strings.LastIndex(line, "----")
		if first < 0 || last <= first {
			return nil, fmt.Errorf("第 %d 行：需要 邮箱----密码----2FA 格式", i+1)
		}
		a := AutoOAuthAccount{Email: strings.TrimSpace(line[:first]), Password: line[first+4 : last], TOTPSecret: strings.ToUpper(strings.TrimSpace(line[last+4:]))}
		address, err := mail.ParseAddress(a.Email)
		if err != nil || address.Address != a.Email || strings.ContainsAny(a.Email, "/\\") || a.Password == "" {
			return nil, fmt.Errorf("第 %d 行：邮箱或密码格式无效", i+1)
		}
		a.TOTPSecret = strings.TrimRight(a.TOTPSecret, "=")
		secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(a.TOTPSecret)
		if err != nil || len(secret) < 10 {
			return nil, fmt.Errorf("第 %d 行：2FA 必须是有效的 Base32 TOTP 种子", i+1)
		}
		key := strings.ToLower(a.Email)
		if seen[key] {
			return nil, fmt.Errorf("第 %d 行：邮箱重复", i+1)
		}
		seen[key] = true
		accounts = append(accounts, a)
		if len(accounts) > AutoOAuthMaxAccounts {
			return nil, fmt.Errorf("每批最多 %d 个账号", AutoOAuthMaxAccounts)
		}
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("请输入至少一个账号")
	}
	return accounts, nil
}

func (a AutoOAuthAccount) Metadata(workspaceID, workspaceName string) map[string]any {
	return map[string]any{"origin": AutoOAuthOrigin, "email": a.Email, "password": a.Password, "totp_secret": a.TOTPSecret, "workspace_id": workspaceID, "workspace_name": workspaceName}
}

// AutoOAuthCredentials never infers consent from a password or refresh token.
func AutoOAuthCredentials(metadata map[string]any) (AutoOAuthAccount, bool) {
	stored, ok := metadata[AutoOAuthKey].(map[string]any)
	if !ok || stored["origin"] != AutoOAuthOrigin {
		return AutoOAuthAccount{}, false
	}
	email, _ := stored["email"].(string)
	password, _ := stored["password"].(string)
	secret, _ := stored["totp_secret"].(string)
	currentEmail, _ := metadata["email"].(string)
	workspace, _ := stored["workspace_id"].(string)
	storedAccountID, _ := stored["account_id"].(string)
	workspaceMatches := workspace != ""
	if storedAccountID != "" {
		workspaceMatches = storedAccountID == metadata["account_id"]
	} else {
		workspaceMatches = workspaceMatches && workspace == metadata["account_id"]
	}
	if email == "" || password == "" || secret == "" || !workspaceMatches || !strings.EqualFold(email, currentEmail) {
		return AutoOAuthAccount{}, false
	}
	return AutoOAuthAccount{Email: email, Password: password, TOTPSecret: secret}, true
}

// StripImportedAutoOAuth removes recovery consent and credentials from imported files.
func StripImportedAutoOAuth(data []byte) ([]byte, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("invalid auth JSON")
	}
	if metadata == nil {
		return nil, fmt.Errorf("auth JSON must be an object")
	}
	_, hasCredentials := metadata[AutoOAuthKey]
	_, hasRecovery := metadata[AutoOAuthRecoveryKey]
	if !hasCredentials && !hasRecovery {
		return data, nil
	}
	delete(metadata, AutoOAuthKey)
	delete(metadata, AutoOAuthRecoveryKey)
	return json.Marshal(metadata)
}

func autoOAuthCallbackCode(raw, state string) (string, error) {
	callback, err := url.Parse(raw)
	if err != nil {
		return "", &AutoOAuthError{Code: "invalid_callback"}
	}
	expected, _ := url.Parse(RedirectURI)
	query := callback.Query()
	if callback.Scheme != expected.Scheme || callback.Host != expected.Host || callback.Path != expected.Path || query.Get("state") != state || len(query["state"]) != 1 || len(query["code"]) != 1 || query.Get("code") == "" || query.Get("error") != "" {
		return "", &AutoOAuthError{Code: "invalid_callback"}
	}
	return query.Get("code"), nil
}

func AutoOAuthLogin(ctx context.Context, cfg *config.Config, account AutoOAuthAccount, proxyURL, workspaceID, workspaceName string) (*AutoOAuthResult, error) {
	return AutoOAuthLoginWithProgress(ctx, cfg, account, proxyURL, workspaceID, workspaceName, nil)
}

// The authorization browser and token exchange must share the same login proxy.
func autoOAuthProxyURL(cfg *config.Config, explicit string) string {
	if override := strings.TrimSpace(os.Getenv("CPA_CODEX_AUTO_OAUTH_PROXY")); override != "" {
		return override
	}
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func AutoOAuthLoginWithProgress(ctx context.Context, cfg *config.Config, account AutoOAuthAccount, proxyURL, workspaceID, workspaceName string, progress AutoOAuthProgress) (*AutoOAuthResult, error) {
	reportAutoOAuthProgress(progress, "等待浏览器调度", 2)
	ctx, cancel := context.WithTimeout(ctx, AutoOAuthTimeout)
	defer cancel()
	select {
	case autoOAuthSlots <- struct{}{}:
		defer func() { <-autoOAuthSlots }()
		reportAutoOAuthProgress(progress, "启动浏览器", 8)
	case <-ctx.Done():
		return nil, newAutoOAuthError("canceled", ctx.Err())
	}
	pkce, err := GeneratePKCECodes()
	if err != nil {
		return nil, err
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		return nil, err
	}
	proxyURL = autoOAuthProxyURL(cfg, proxyURL)
	svc := NewCodexAuthWithProxyURL(cfg, proxyURL)
	authURL, err := svc.GenerateAuthURL(state, pkce)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"account": account, "auth_url": authURL, "redirect_uri": RedirectURI, "proxy_url": proxyURL, "workspace_id": workspaceID, "workspace_name": workspaceName, "timeout_ms": AutoOAuthTimeout.Milliseconds()}
	result, err := runAutoOAuthBrowser(ctx, payload, progress)
	if err != nil {
		return nil, err
	}
	reportAutoOAuthProgress(progress, "校验 OAuth 回调", 80)
	code, err := autoOAuthCallbackCode(result.Callback, state)
	if err != nil {
		return nil, err
	}
	reportAutoOAuthProgress(progress, "交换 OAuth Token", 82)
	bundle, err := svc.ExchangeCodeForTokens(ctx, code, pkce)
	if err != nil {
		return nil, newAutoOAuthError("token_exchange_failed", err)
	}
	if !strings.EqualFold(bundle.TokenData.Email, account.Email) {
		return nil, &AutoOAuthError{Code: "identity_mismatch"}
	}
	reportAutoOAuthProgress(progress, "校验账号身份", 90)
	if bundle.TokenData.AccountID == "" {
		return nil, &AutoOAuthError{Code: "workspace_mismatch"}
	}
	if result.TeamSelected {
		claims, errClaims := ParseJWTToken(bundle.TokenData.IDToken)
		if errClaims != nil || claims == nil || (claims.CodexAuthInfo.ChatgptPlanType != "team" && claims.CodexAuthInfo.ChatgptPlanType != "business" && claims.CodexAuthInfo.ChatgptPlanType != "enterprise") {
			return nil, &AutoOAuthError{Code: "workspace_mismatch"}
		}
	}
	selectedWorkspaceID := strings.TrimSpace(result.WorkspaceID)
	return &AutoOAuthResult{Bundle: bundle, WorkspaceID: selectedWorkspaceID, WorkspaceName: result.WorkspaceName}, nil
}

func reportAutoOAuthProgress(progress AutoOAuthProgress, step string, percent int) {
	if progress != nil {
		progress(step, percent)
	}
}

type autoOAuthBrowserResult struct {
	Callback      string `json:"callback"`
	Error         string `json:"error"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	TeamSelected  bool   `json:"team_selected"`
}

func runAutoOAuthBrowser(ctx context.Context, payload map[string]any, progress AutoOAuthProgress) (*autoOAuthBrowserResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, newAutoOAuthError("canceled", err)
	}
	node := os.Getenv("CPA_CODEX_AUTO_OAUTH_NODE")
	if node == "" {
		node = "node"
	}
	cmd := exec.CommandContext(ctx, node, "-e", autoOAuthBrowserScript)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, newAutoOAuthError("browser_unavailable", ctx.Err())
	}
	defer func() { _ = stdin.Close() }()
	// EOF asks the runner to close its browser before Node is terminated.
	cmd.Cancel = func() error { return stdin.Close() }
	cmd.WaitDelay = 5 * time.Second
	stdout, errStdout := cmd.StdoutPipe()
	if errStdout != nil {
		return nil, newAutoOAuthError("browser_unavailable", ctx.Err())
	}
	cmd.Stderr = io.Discard
	if errStart := cmd.Start(); errStart != nil {
		return nil, newAutoOAuthError("browser_unavailable", ctx.Err())
	}
	if errWrite := json.NewEncoder(stdin).Encode(payload); errWrite != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, newAutoOAuthError("browser_unavailable", ctx.Err())
	}
	var result autoOAuthBrowserResult
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var event struct {
			Type          string                  `json:"type"`
			Step          string                  `json:"step"`
			Percent       int                     `json:"percent"`
			Result        *autoOAuthBrowserResult `json:"result"`
			Callback      string                  `json:"callback"`
			Error         string                  `json:"error"`
			WorkspaceID   string                  `json:"workspace_id"`
			WorkspaceName string                  `json:"workspace_name"`
			TeamSelected  bool                    `json:"team_selected"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "progress" {
			reportAutoOAuthProgress(progress, event.Step, event.Percent)
			continue
		}
		if event.Type == "result" && event.Result != nil {
			result = *event.Result
			continue
		}
		if event.Type == "result" {
			result = autoOAuthBrowserResult{Callback: event.Callback, Error: event.Error, WorkspaceID: event.WorkspaceID, WorkspaceName: event.WorkspaceName, TeamSelected: event.TeamSelected}
		}
	}
	errScan := scanner.Err()
	if errScan != nil {
		_ = cmd.Process.Kill()
	}
	errWait := cmd.Wait()
	if err := ctx.Err(); err != nil {
		return nil, newAutoOAuthError("canceled", err)
	}
	if result.Callback == "" && result.Error == "" {
		return nil, &AutoOAuthError{Code: "browser_protocol_error"}
	}
	if result.Error != "" {
		return nil, &AutoOAuthError{Code: result.Error}
	}
	if errWait != nil || errScan != nil {
		return nil, &AutoOAuthError{Code: "browser_failed"}
	}
	return &result, nil
}
