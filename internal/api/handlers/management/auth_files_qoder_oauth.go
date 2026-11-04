package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type qoderOAuthService interface {
	Start(context.Context) (*qoder.LoginSession, error)
	Wait(context.Context, *qoder.LoginSession) (qoder.Credentials, error)
}

var newQoderOAuthService = func(cfg *config.Config, provider string) qoderOAuthService {
	return qoder.NewService(provider, util.SetProxy(&cfg.SDKConfig, &http.Client{}))
}

// RequestQoderToken starts an international Qoder device authorization.
func (h *Handler) RequestQoderToken(c *gin.Context) {
	h.requestQoderToken(c, qoder.ProviderIntl)
}

// RequestQoderCNToken starts a Qoder China device authorization.
func (h *Handler) RequestQoderCNToken(c *gin.Context) {
	h.requestQoderToken(c, qoder.ProviderCN)
}

func (h *Handler) requestQoderToken(c *gin.Context, provider string) {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	service := newQoderOAuthService(cfg, provider)
	// Device polling must survive completion of the management HTTP request.
	ctx := PopulateAuthContext(context.Background(), c)
	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	session, errStart := service.Start(startCtx)
	if errStart != nil || session == nil || strings.TrimSpace(session.URL) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to start Qoder authorization"})
		return
	}
	// The browser-facing state is independent of the upstream PKCE session.
	state, errState := misc.GenerateRandomState()
	if errState != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate OAuth state"})
		return
	}
	RegisterOAuthSession(state, provider)
	go h.waitQoderOAuth(ctx, service, session, state, provider)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": session.URL, "state": state, "flow": "device", "expires_in": int(qoder.LoginTimeout.Seconds())})
}

func (h *Handler) waitQoderOAuth(ctx context.Context, service qoderOAuthService, session *qoder.LoginSession, state, provider string) {
	pollCtx, cancelPoll := context.WithTimeout(ctx, qoder.LoginTimeout)
	defer cancelPoll()
	go watchOAuthSessionCancel(pollCtx, cancelPoll, state, provider)
	creds, errWait := service.Wait(pollCtx, session)
	if !IsOAuthSessionPending(state, provider) {
		return
	}
	if errWait != nil || pollCtx.Err() != nil {
		SetOAuthSessionError(state, "Qoder authorization failed or expired; start login again")
		return
	}
	if strings.TrimSpace(creds.AccessToken) == "" || strings.TrimSpace(creds.UID) == "" {
		SetOAuthSessionError(state, "Qoder returned incomplete account credentials")
		return
	}
	record := qoder.NewAuthRecord(provider, creds)
	if errGuard := guardOAuthSessionPendingForSave(state, provider); errGuard != nil {
		return
	}
	if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
		SetOAuthSessionError(state, "Failed to save Qoder credentials")
		return
	}
	CompleteOAuthSession(state)
}

type qoderQuotaPreparer struct{ cfg *config.Config }

func (p qoderQuotaPreparer) ShouldPrepareRequestAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	creds := qoder.CredentialsFromAuth(auth)
	// Qoder expiry timestamps are Unix seconds, unlike WorkBuddy milliseconds.
	return creds.ExpiresAt > 0 && creds.ExpiresAt <= time.Now().Add(qoder.RefreshLead).Unix()
}

func (p qoderQuotaPreparer) PrepareRequestAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return executor.NewQoderExecutor(p.cfg, auth.Provider).Refresh(ctx, auth)
}

// GetQoderQuota refreshes and persists credentials through the request auth manager.
func (h *Handler) GetQoderQuota(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	if auth.Provider != qoder.ProviderIntl && auth.Provider != qoder.ProviderCN {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential is not a Qoder account"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	auth, errPrepare := h.authManager.PrepareRequestAuth(c.Request.Context(), qoderQuotaPreparer{cfg}, auth)
	if errPrepare != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Qoder token refresh failed; sign in again"})
		return
	}
	service := qoder.NewService(auth.Provider, &http.Client{Transport: h.apiCallTransport(auth, "")})
	service.SetEndpoint(qoder.EndpointForAuth(auth))
	quota, errQuota := service.FetchQuota(c.Request.Context(), qoder.CredentialsFromAuth(auth), stringValue(auth.Metadata, "plan"))
	if errQuota != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to query Qoder quota"})
		return
	}
	c.JSON(http.StatusOK, quota)
}
