package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestParseAutoOAuthAccounts(t *testing.T) {
	accounts, err := ParseAutoOAuthAccounts("\ufeffone@example.test----pass----with-separator----JBSWY3DPEHPK3PXP\r\n\n two@example.test---- second pass ----jbswy3dpehpk3pxp\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].Password != "pass----with-separator" || accounts[1].Password != " second pass " || accounts[1].TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Fatal("account parsing changed credential fields")
	}
}

func TestParseAutoOAuthAccountsRejectsInvalidWithoutLeakingSecrets(t *testing.T) {
	for _, input := range []string{
		"",
		"not-email----private-password----JBSWY3DPEHPK3PXP",
		"one@example.test--------JBSWY3DPEHPK3PXP",
		"one@example.test----private-password----INVALID!",
		"one@example.test----private-password----JBSWY3DPEHPK3PXP\nONE@example.test----other----JBSWY3DPEHPK3PXP",
	} {
		_, err := ParseAutoOAuthAccounts(input)
		if err == nil {
			t.Fatal("expected invalid account input to fail")
		}
		if strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), "JBSWY3DPEHPK3PXP") {
			t.Fatal("validation leaked a secret")
		}
	}
}

func TestAutoOAuthCredentialsRequireExplicitOriginAndIdentity(t *testing.T) {
	account := AutoOAuthAccount{Email: "one@example.test", Password: "test-password", TOTPSecret: "JBSWY3DPEHPK3PXP"}
	metadata := map[string]any{"email": account.Email, "account_id": "team-id"}
	if _, ok := AutoOAuthCredentials(metadata); ok {
		t.Fatal("imported token must not enable automatic recovery")
	}
	metadata[AutoOAuthKey] = account.Metadata("team-id", "Test team")
	if _, ok := AutoOAuthCredentials(metadata); !ok {
		t.Fatal("automatic OAuth credentials should enable recovery")
	}
	metadata["account_id"] = "personal-id"
	if _, ok := AutoOAuthCredentials(metadata); ok {
		t.Fatal("workspace mismatch must disable automatic recovery")
	}
	metadata["account_id"] = "team-id"
	metadata["email"] = "another@example.test"
	if _, ok := AutoOAuthCredentials(metadata); ok {
		t.Fatal("email mismatch must disable automatic recovery")
	}
}

func TestStripImportedAutoOAuth(t *testing.T) {
	raw := []byte(`{"type":"codex","email":"one@example.test","access_token":"test-token","codex_auto_oauth":{"origin":"cpa-auto-oauth-v1","password":"private-password"},"codex_auto_oauth_recovery":{"status":"account_banned"}}`)
	cleaned, err := StripImportedAutoOAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(cleaned, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata[AutoOAuthKey] != nil || metadata[AutoOAuthRecoveryKey] != nil {
		t.Fatal("imported JSON retained automatic recovery metadata")
	}
	if metadata["access_token"] != "test-token" || metadata["email"] != "one@example.test" {
		t.Fatal("import sanitization changed token identity")
	}
	if strings.Contains(string(cleaned), "private-password") {
		t.Fatal("imported JSON retained recovery credentials")
	}
}

func TestStripImportedAutoOAuthPreservesUnrelatedJSON(t *testing.T) {
	raw := []byte("{ \"type\": \"codex\", \"access_token\": \"test-token\" }\n")
	cleaned, err := StripImportedAutoOAuth(raw)
	if err != nil || string(cleaned) != string(raw) {
		t.Fatal("ordinary imported JSON should remain byte-for-byte unchanged")
	}
	for _, invalid := range []string{"null", "[]", "broken JSON"} {
		if _, err := StripImportedAutoOAuth([]byte(invalid)); err == nil {
			t.Fatal("non-object auth JSON accepted")
		}
	}
}

func TestAutoOAuthRejectsWrongCallback(t *testing.T) {
	for _, callback := range []string{
		"http://localhost:1455/auth/callback?code=x&state=wrong",
		"https://example.test/auth/callback?code=x&state=expected",
		"http://localhost:1455/other?code=x&state=expected",
		"http://localhost:1455/auth/callback?state=expected",
	} {
		if _, err := autoOAuthCallbackCode(callback, "expected"); err == nil {
			t.Fatal("invalid callback accepted")
		}
	}
	code, err := autoOAuthCallbackCode("http://localhost:1455/auth/callback?code=ok&state=expected", "expected")
	if err != nil || code != "ok" {
		t.Fatal("valid callback rejected")
	}
}

func TestAutoOAuthProxyUsesSameOverrideForBrowserAndTokenExchange(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.test:8080"}}
	t.Setenv("CPA_CODEX_AUTO_OAUTH_PROXY", "http://home.example.test:3010")
	if got := autoOAuthProxyURL(cfg, "http://account.example.test:8080"); got != "http://home.example.test:3010" {
		t.Fatalf("login proxy override was not selected")
	}
	t.Setenv("CPA_CODEX_AUTO_OAUTH_PROXY", "")
	if got := autoOAuthProxyURL(cfg, "http://account.example.test:8080"); got != "http://account.example.test:8080" {
		t.Fatal("explicit proxy was not retained")
	}
	if got := autoOAuthProxyURL(cfg, ""); got != cfg.ProxyURL {
		t.Fatal("global proxy fallback was not retained")
	}
}

func TestAutoOAuthBrowserRunsEmbeddedEvalAndReportsLaunchFailure(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is not installed")
	}
	stub := filepath.Join(t.TempDir(), "playwright.cjs")
	if errWrite := os.WriteFile(stub, []byte(`module.exports = {chromium:{launch:async()=>{throw new Error('fixture launch failure')}}};`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	t.Setenv("CPA_CODEX_AUTO_OAUTH_NODE", node)
	t.Setenv("CPA_CODEX_AUTO_OAUTH_PLAYWRIGHT", stub)
	t.Setenv("CPA_CODEX_AUTO_OAUTH_PROXY", "")
	t.Setenv("CPA_CODEX_AUTO_OAUTH_LIB", "0")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = runAutoOAuthBrowser(ctx, map[string]any{"account": map[string]string{}, "auth_url": "https://example.test/start", "redirect_uri": RedirectURI}, nil)
	var failure *AutoOAuthError
	if !errors.As(err, &failure) || failure.Code != "browser_launch_failed" {
		t.Fatalf("unexpected launch result: %v", err)
	}
}

func TestAutoOAuthErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code    string
		status  int
		cause   error
		message string
	}{
		{"account_banned", http.StatusUnauthorized, nil, "账号被封禁（登录服务明确返回停用或封禁）"},
		{"invalid_credentials", http.StatusUnauthorized, nil, "账号密码或 2FA 验证失败，需要人工处理"},
		{"manual_required", 0, nil, "需要人工完成验证码、邮箱验证或额外登录验证"},
		{"workspace_mismatch", 0, nil, "工作空间不匹配，未覆盖原认证文件"},
		{"identity_mismatch", 0, nil, "登录邮箱与输入账号不匹配，未保存认证文件"},
		{"browser_unavailable", 0, nil, "自动 OAuth 浏览器未就绪，请安装 Node.js、Playwright 和 Chromium"},
		{"browser_launch_failed", 0, nil, "自动 OAuth 浏览器启动失败，请检查浏览器运行目录和权限"},
		{"browser_protocol_error", 0, nil, "自动 OAuth 浏览器进程未返回有效结果"},
		{"navigation_failed", 0, nil, "自动 OAuth 无法打开登录页面，请检查登录代理连接"},
		{"login_timeout", 0, context.DeadlineExceeded, "自动 OAuth 等待当前登录步骤超时，需要人工检查页面"},
		{"cloudflare_blocked", 0, nil, "登录页安全验证未完成，请人工验证后再登录"},
		{"proxy_invalid", 0, nil, "自动 OAuth 代理配置无效"},
		{"canceled", 0, context.Canceled, "自动 OAuth 已取消"},
		{"timeout", 0, context.DeadlineExceeded, "自动 OAuth 超时，需要人工检查登录步骤"},
		{"invalid_callback", 0, nil, "OAuth 回调校验失败"},
		{"token_exchange_failed", 0, nil, "OAuth token 交换失败"},
		{"browser_failed", 0, nil, "自动 OAuth 失败，请检查网络或改为人工登录"},
		{"network_error", 0, nil, "自动 OAuth 失败，请检查网络或改为人工登录"},
		{"unknown_fixture_private_detail", 0, nil, "自动 OAuth 失败，请检查网络或改为人工登录"},
		{"", 0, nil, "自动 OAuth 失败，请检查网络或改为人工登录"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			failure := &AutoOAuthError{Code: tc.code}
			if got := failure.StatusCode(); got != tc.status {
				t.Errorf("StatusCode() = %d, want %d", got, tc.status)
			}
			if got := failure.Error(); got != tc.message {
				t.Errorf("Error() = %q, want fixed message %q", got, tc.message)
			}
			if got := failure.AutoOAuthFailureCode(); got != tc.code {
				t.Errorf("AutoOAuthFailureCode() = %q, want %q", got, tc.code)
			}
			for _, err := range []error{failure, fmt.Errorf("caller: %w", failure)} {
				for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
					if got := errors.Is(err, target); got != (tc.cause == target) {
						t.Errorf("errors.Is(%T, %v) = %v, want %v", err, target, got, tc.cause == target)
					}
				}
			}
		})
	}
}

func TestAutoOAuthContextFailures(t *testing.T) {
	t.Setenv("CPA_CODEX_AUTO_OAUTH_NODE", os.Args[0])
	for _, phase := range []string{"waiting_for_slot", "before_browser_start"} {
		for _, tc := range []struct {
			code  string
			cause error
		}{
			{"canceled", context.Canceled},
			{"timeout", context.DeadlineExceeded},
		} {
			t.Run(phase+"/"+tc.code, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				if tc.cause == context.Canceled {
					ctx, cancel = context.WithCancel(t.Context())
					cancel()
				} else {
					ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
				}
				defer cancel()
				var err error
				if phase == "waiting_for_slot" {
					for range cap(autoOAuthSlots) {
						autoOAuthSlots <- struct{}{}
					}
					t.Cleanup(func() {
						for range cap(autoOAuthSlots) {
							<-autoOAuthSlots
						}
					})
					_, err = AutoOAuthLogin(ctx, nil, AutoOAuthAccount{}, "", "", "")
				} else {
					_, err = runAutoOAuthBrowser(ctx, nil, nil)
				}
				assertAutoOAuthFailure(t, err, tc.code, tc.cause)
			})
		}
	}
}

func TestAutoOAuthTokenExchangeFailures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is not installed")
	}
	t.Setenv("CPA_CODEX_AUTO_OAUTH_NODE", node)
	t.Setenv("CPA_CODEX_AUTO_OAUTH_PROXY", "")
	originalScript, originalTransport := autoOAuthBrowserScript, http.DefaultTransport
	t.Cleanup(func() {
		autoOAuthBrowserScript = originalScript
		http.DefaultTransport = originalTransport
	})
	autoOAuthBrowserScript = `require('node:readline').createInterface({input:process.stdin}).once('line', line => {
		const payload = JSON.parse(line);
		const callback = new URL(payload.redirect_uri);
		callback.searchParams.set('state', new URL(payload.auth_url).searchParams.get('state'));
		callback.searchParams.set('code', 'fixture-code');
		process.stdout.write(JSON.stringify({type:'result', callback:callback.href}) + '\n', () => process.exit(0));
	});`
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
		cause  error
	}{
		{"network", errors.New("fixture-private-network-detail"), 0, "token_exchange_failed", nil},
		{"unknown_401_text", errors.New("fixture-private-detail: status 401 invalid_credentials"), 0, "token_exchange_failed", nil},
		{"unauthorized_response", nil, http.StatusUnauthorized, "token_exchange_failed", nil},
		{"server_error_response", nil, http.StatusBadGateway, "token_exchange_failed", nil},
		{"canceled", fmt.Errorf("fixture-private-detail: %w", context.Canceled), 0, "canceled", context.Canceled},
		{"timeout", fmt.Errorf("fixture-private-detail: %w", context.DeadlineExceeded), 0, "timeout", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				if req.URL.String() != TokenURL {
					t.Fatalf("unexpected request: %s", req.URL)
				}
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("fixture-private-response")), Request: req}, nil
			})
			_, errLogin := AutoOAuthLogin(t.Context(), nil, AutoOAuthAccount{Email: "fixture@example.test"}, "", "", "")
			if !called {
				t.Fatal("token exchange was not reached")
			}
			assertAutoOAuthFailure(t, errLogin, tc.code, tc.cause)
		})
	}
}

func assertAutoOAuthFailure(t *testing.T, err error, code string, cause error) {
	t.Helper()
	var failure *AutoOAuthError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want AutoOAuthError", err)
	}
	if failure.Code != code || failure.StatusCode() != 0 {
		t.Errorf("failure code/status = %q/%d, want %q/0", failure.Code, failure.StatusCode(), code)
	}
	if want := (&AutoOAuthError{Code: code}).Error(); err.Error() != want {
		t.Errorf("error = %q, want fixed message %q", err.Error(), want)
	}
	for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := errors.Is(err, target); got != (cause == target) {
			t.Errorf("errors.Is(err, %v) = %v, want %v", target, got, cause == target)
		}
	}
	if got := errors.Unwrap(failure); got != cause {
		t.Errorf("Unwrap() = %v, want safe cause %v", got, cause)
	}
}
