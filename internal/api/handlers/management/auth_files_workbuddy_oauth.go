package management

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

const workBuddyCreditRefreshInterval = 30 * time.Minute
const workBuddyQuotaConcurrency = 4

type workBuddyCreditSummary struct {
	Available bool      `json:"available"`
	Remaining float64   `json:"remaining"`
	Accounts  int       `json:"accounts"`
	Known     int       `json:"known"`
	Failed    int       `json:"failed"`
	UpdatedAt time.Time `json:"updated_at"`
}

type workBuddyCreditEntry struct {
	version   uint64
	remaining float64
	known     bool
	failed    bool
}

type workBuddyCreditCache struct {
	mu          sync.Mutex
	entries     map[string]workBuddyCreditEntry
	version     uint64
	initialized bool
	updatedAt   time.Time
	slots       chan struct{}
}

func (s *workBuddyCreditCache) begin(id string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]workBuddyCreditEntry)
		s.slots = make(chan struct{}, workBuddyQuotaConcurrency)
	}
	s.version++
	entry := s.entries[id]
	entry.version = s.version
	s.entries[id] = entry
	return entry.version
}

func (s *workBuddyCreditCache) finish(id string, version uint64, quota workbuddy.Quota, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[id]
	if !exists || entry.version != version {
		return
	}
	entry.failed = err != nil
	if err == nil {
		entry.remaining = 0
		if quota.Credits != nil {
			entry.remaining = quota.Credits.Remaining
		}
		entry.known = true
	}
	s.entries[id] = entry
	s.updatedAt = time.Now()
}

func (s *workBuddyCreditCache) snapshot() workBuddyCreditSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := workBuddyCreditSummary{Accounts: len(s.entries), UpdatedAt: s.updatedAt}
	for _, entry := range s.entries {
		if entry.known {
			result.Remaining += entry.remaining
			result.Known++
		}
		if entry.failed {
			result.Failed++
		}
	}
	result.Available = result.Known > 0 || (s.initialized && result.Accounts == 0)
	return result
}

// GetWorkBuddyCredits reads the last observations without contacting upstream.
func (h *Handler) GetWorkBuddyCredits(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, h.workBuddyCredits.snapshot())
}

func (h *Handler) RunWorkBuddyCredits(ctx context.Context) {
	ticker := time.NewTicker(workBuddyCreditRefreshInterval)
	defer ticker.Stop()
	h.runWorkBuddyCredits(ctx, ticker.C)
}

func (h *Handler) runWorkBuddyCredits(ctx context.Context, ticks <-chan time.Time) {
	h.refreshWorkBuddyCredits(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			h.refreshWorkBuddyCredits(ctx)
		}
	}
}

func (h *Handler) refreshWorkBuddyCredits(ctx context.Context) {
	if h.authManager == nil || h.authManager.HomeEnabled() {
		return
	}
	accounts := make(map[string]*coreauth.Auth)
	for _, auth := range h.authManager.List() {
		if auth.Provider == workbuddy.ProviderCN || auth.Provider == workbuddy.ProviderAI {
			accounts[auth.ID] = auth
		}
	}
	h.workBuddyCredits.mu.Lock()
	for id := range h.workBuddyCredits.entries {
		if accounts[id] == nil {
			delete(h.workBuddyCredits.entries, id)
		}
	}
	h.workBuddyCredits.initialized = true
	h.workBuddyCredits.mu.Unlock()
	type job struct {
		auth    *coreauth.Auth
		version uint64
	}
	jobs := make(chan job, len(accounts))
	for _, auth := range accounts {
		jobs <- job{auth: auth, version: h.workBuddyCredits.begin(auth.ID)}
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(workBuddyQuotaConcurrency, len(accounts)) {
		workers.Go(func() {
			for item := range jobs {
				if ctx.Err() != nil {
					return
				}
				_, _ = h.fetchWorkBuddyQuota(ctx, item.auth, item.version)
			}
		})
	}
	workers.Wait()
}

func (h *Handler) fetchWorkBuddyQuota(ctx context.Context, auth *coreauth.Auth, version uint64) (quota workbuddy.Quota, err error) {
	defer func() { h.workBuddyCredits.finish(auth.ID, version, quota, err) }()
	select {
	case h.workBuddyCredits.slots <- struct{}{}:
		defer func() { <-h.workBuddyCredits.slots }()
	case <-ctx.Done():
		return quota, ctx.Err()
	}
	cfg := h.configSnapshot()
	prepared, errPrepare := h.authManager.PrepareRequestAuth(ctx, workBuddyQuotaPreparer{cfg}, auth)
	if errPrepare != nil {
		return quota, errors.New("WorkBuddy token refresh failed; sign in again")
	}
	service := workbuddy.NewService(prepared.Provider, &http.Client{Transport: (&Handler{cfg: cfg}).apiCallTransport(prepared, "")})
	service.SetEndpoint(workbuddy.EndpointForAuth(prepared))
	return service.FetchQuota(ctx, workbuddy.CredentialsFromAuth(prepared), stringValue(prepared.Metadata, "plan"))
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
	quota, err := h.fetchWorkBuddyQuota(c.Request.Context(), auth, h.workBuddyCredits.begin(auth.ID))
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
