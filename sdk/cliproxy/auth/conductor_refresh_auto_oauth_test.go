package auth

import "testing"

type autoOAuthFailureTestError struct{}

func (autoOAuthFailureTestError) Error() string                { return "automatic OAuth failed" }
func (autoOAuthFailureTestError) AutoOAuthFailureCode() string { return "account_banned" }

func TestAutoOAuthFailureMessage(t *testing.T) {
	if got := autoOAuthFailureCode(autoOAuthFailureTestError{}); got != "account_banned" {
		t.Fatalf("failure code = %q, want account_banned", got)
	}
	if got := autoOAuthFailureMessage("account_banned"); got != "账号被封禁" {
		t.Fatalf("failure message = %q, want account banned message", got)
	}
	if got := autoOAuthFailureMessage("manual_required"); got == "" {
		t.Fatal("manual recovery failure message is empty")
	}
}

func TestAutoOAuthFailureCodeIgnoresOrdinaryErrors(t *testing.T) {
	if got := autoOAuthFailureCode(nil); got != "" {
		t.Fatalf("nil failure code = %q", got)
	}
}

func TestAutoOAuthFailureCodeFromMetadata(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{
		"codex_auto_oauth_recovery": map[string]any{"status": "failed", "error_code": "account_banned"},
	}}
	if got := autoOAuthFailureCodeFromMetadata(auth); got != "account_banned" {
		t.Fatalf("metadata failure code = %q, want account_banned", got)
	}
}
