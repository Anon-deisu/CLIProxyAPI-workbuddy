package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerAutoDisablesUnauthorizedAuthInMemory(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	cfg := &internalconfig.Config{}
	cfg.Routing.AutoDisableUnauthorized = true
	manager.SetConfig(cfg)
	auth := &Auth{ID: "auth-401", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Code: "auth_unavailable", Message: "token expired"},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth was not retained")
	}
	if !updated.AutoDisabled || updated.AutoDisableReason != AutoDisableReasonUnauthorized {
		t.Fatalf("auto-disable state = %+v", updated)
	}
	if updated.Disabled {
		t.Fatal("automatic disable must not set persistent Disabled")
	}
	if _, errPick := manager.SelectAuth(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}); errPick == nil {
		t.Fatal("automatically disabled auth was selected")
	}
}

func TestManagerAutoDisablesQuotaFromResponseSignal(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	cfg := &internalconfig.Config{}
	cfg.Routing.AutoDisableQuota = true
	manager.SetConfig(cfg)
	auth := &Auth{ID: "auth-quota", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": []string{"100"}})
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: auth.Provider, Model: "gpt-5"})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil || !updated.AutoDisabled {
		t.Fatalf("quota auto-disable state = %#v", updated)
	}
	if updated.AutoDisableReason != AutoDisableReasonQuotaExhausted {
		t.Fatalf("quota auto-disable reason = %q", updated.AutoDisableReason)
	}
}

func TestResetQuotaClearsAutomaticQuotaDisable(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth := &Auth{
		ID:                "auth-reset",
		Provider:          "codex",
		Status:            StatusError,
		AutoDisabled:      true,
		AutoDisableReason: AutoDisableReasonQuotaExhausted,
		AutoDisabledAt:    time.Now(),
		Unavailable:       true,
		NextRetryAfter:    time.Now().Add(time.Hour),
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	updated, _, errReset := manager.ResetQuota(context.Background(), auth.ID)
	if errReset != nil {
		t.Fatalf("reset quota: %v", errReset)
	}
	if updated == nil || updated.AutoDisabled {
		t.Fatalf("reset did not clear auto-disable: %#v", updated)
	}
}
