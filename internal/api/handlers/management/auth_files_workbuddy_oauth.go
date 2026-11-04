package management

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type workBuddyOAuthService interface {
	Start(context.Context) (*workbuddy.LoginSession, error)
	Wait(context.Context, *workbuddy.LoginSession) (workbuddy.Credentials, error)
}

var newWorkBuddyOAuthService = func(cfg *config.Config, provider string) workBuddyOAuthService {
	client := util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})
	return workbuddy.NewService(provider, client)
}

func (h *Handler) RequestWorkBuddyToken(c *gin.Context) {
	h.requestWorkBuddyToken(c, workbuddy.ProviderCN)
}

func (h *Handler) RequestWorkBuddyAIToken(c *gin.Context) {
	h.requestWorkBuddyToken(c, workbuddy.ProviderAI)
}

func (h *Handler) requestWorkBuddyToken(c *gin.Context, provider string) {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	service := newWorkBuddyOAuthService(cfg, provider)
	ctx := PopulateAuthContext(context.Background(), c)
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	session, err := service.Start(startCtx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to start WorkBuddy authorization"})
		return
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate OAuth state"})
		return
	}
	RegisterOAuthSession(state, provider)
	go func() {
		pollCtx, cancelPoll := context.WithTimeout(ctx, workbuddy.LoginTimeout)
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, state, provider)
		creds, errWait := service.Wait(pollCtx, session)
		if !IsOAuthSessionPending(state, provider) {
			return
		}
		if errWait != nil {
			SetOAuthSessionError(state, "WorkBuddy authorization failed or expired; start login again")
			return
		}
		if creds.AccessToken == "" || creds.UID == "" {
			SetOAuthSessionError(state, "WorkBuddy returned incomplete account credentials")
			return
		}
		record := workbuddy.NewAuthRecord(provider, creds)
		if errGuard := guardOAuthSessionPendingForSave(state, provider); errGuard != nil {
			return
		}
		if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
			SetOAuthSessionError(state, "Failed to save WorkBuddy credentials")
			return
		}
		CompleteOAuthSession(state)
	}()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": session.URL, "state": state, "flow": "device", "expires_in": int(workbuddy.LoginTimeout.Seconds())})
}

type workBuddyQuotaPreparer struct{ cfg *config.Config }

func (p workBuddyQuotaPreparer) ShouldPrepareRequestAuth(auth *coreauth.Auth) bool {
	creds := workbuddy.CredentialsFromAuth(auth)
	return creds.ExpiresAt > 0 && creds.ExpiresAt <= time.Now().Add(workbuddy.RefreshLead).UnixMilli()
}

func (p workBuddyQuotaPreparer) PrepareRequestAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return executor.NewWorkBuddyExecutor(p.cfg, auth.Provider).Refresh(ctx, auth)
}

// GetWorkBuddyQuota keeps account credentials on the server for both regional sites.
func (h *Handler) GetWorkBuddyQuota(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	authIndex := c.Query("auth_index")
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	if auth.Provider != workbuddy.ProviderCN && auth.Provider != workbuddy.ProviderAI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential is not a WorkBuddy account"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	auth, err := h.authManager.PrepareRequestAuth(c.Request.Context(), workBuddyQuotaPreparer{cfg}, auth)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "WorkBuddy token refresh failed; sign in again"})
		return
	}
	service := workbuddy.NewService(auth.Provider, &http.Client{Transport: h.apiCallTransport(auth, "")})
	service.SetEndpoint(workbuddy.EndpointForAuth(auth))
	quota, err := service.FetchQuota(c.Request.Context(), workbuddy.CredentialsFromAuth(auth), stringValue(auth.Metadata, "plan"))
	if err != nil {
		var apiErr *workbuddy.APIError
		if errors.As(err, &apiErr) {
			c.JSON(http.StatusBadGateway, gin.H{"error": apiErr.Error()})
		} else {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to query WorkBuddy quota"})
		}
		return
	}
	c.JSON(http.StatusOK, quota)
}
