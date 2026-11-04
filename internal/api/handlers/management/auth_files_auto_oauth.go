package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type codexAutoOAuthRequest struct {
	Accounts string `json:"accounts"`
}

type codexAutoOAuthResult struct {
	Email         string `json:"email"`
	Status        string `json:"status"`
	Step          string `json:"step,omitempty"`
	Progress      int    `json:"progress"`
	FileName      string `json:"file_name,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	Error         string `json:"error,omitempty"`
}

type codexAutoOAuthJob struct {
	ID        string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	Accounts  []codexAutoOAuthResult
}

// RequestCodexAutoOAuth creates a background batch job. Each account gets its
// own browser session and progress is exposed through GetCodexAutoOAuth.
func (h *Handler) RequestCodexAutoOAuth(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "management handler unavailable"})
		return
	}
	var req codexAutoOAuthRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	accounts, errParse := codex.ParseAutoOAuthAccounts(req.Accounts)
	if errParse != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errParse.Error()})
		return
	}

	jobID := uuid.NewString()
	now := time.Now()
	job := &codexAutoOAuthJob{ID: jobID, Status: "running", CreatedAt: now, UpdatedAt: now, Accounts: make([]codexAutoOAuthResult, len(accounts))}
	for index, account := range accounts {
		job.Accounts[index] = codexAutoOAuthResult{Email: account.Email, Status: "queued", Step: "等待调度", Progress: 0}
	}
	h.autoOAuthJobsMu.Lock()
	if h.autoOAuthJobs == nil {
		h.autoOAuthJobs = make(map[string]*codexAutoOAuthJob)
	}
	for id, existing := range h.autoOAuthJobs {
		if now.Sub(existing.UpdatedAt) > time.Hour {
			delete(h.autoOAuthJobs, id)
		}
	}
	h.autoOAuthJobs[jobID] = job
	initial := cloneCodexAutoOAuthJob(job)
	h.autoOAuthJobsMu.Unlock()

	go h.runCodexAutoOAuthJob(jobID, accounts)
	c.JSON(http.StatusAccepted, codexAutoOAuthJobPayload(initial))
}

func (h *Handler) GetCodexAutoOAuth(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobID"))
	h.autoOAuthJobsMu.Lock()
	job, ok := h.autoOAuthJobs[jobID]
	if ok {
		job = cloneCodexAutoOAuthJob(job)
	}
	h.autoOAuthJobsMu.Unlock()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "OAuth job not found"})
		return
	}
	c.JSON(http.StatusOK, codexAutoOAuthJobPayload(job))
}

func codexAutoOAuthJobPayload(job *codexAutoOAuthJob) gin.H {
	success := 0
	for _, account := range job.Accounts {
		if account.Status == "success" {
			success++
		}
	}
	return gin.H{"job_id": job.ID, "status": job.Status, "success": success, "total": len(job.Accounts), "accounts": job.Accounts}
}

func cloneCodexAutoOAuthJob(job *codexAutoOAuthJob) *codexAutoOAuthJob {
	if job == nil {
		return nil
	}
	clone := *job
	clone.Accounts = append([]codexAutoOAuthResult(nil), job.Accounts...)
	return &clone
}

func (h *Handler) runCodexAutoOAuthJob(jobID string, accounts []codex.AutoOAuthAccount) {
	var wait sync.WaitGroup
	for index, account := range accounts {
		wait.Add(1)
		go func(index int, account codex.AutoOAuthAccount) {
			defer wait.Done()
			progress := func(step string, percent int) {
				h.updateCodexAutoOAuthAccount(jobID, index, func(result *codexAutoOAuthResult) {
					result.Status = "running"
					result.Step = step
					result.Progress = percent
				})
			}
			result := h.runCodexAutoOAuth(context.Background(), account, progress)
			h.updateCodexAutoOAuthAccount(jobID, index, func(target *codexAutoOAuthResult) {
				finishCodexAutoOAuthAccount(target, result)
			})
		}(index, account)
	}
	wait.Wait()
	h.autoOAuthJobsMu.Lock()
	if job := h.autoOAuthJobs[jobID]; job != nil {
		job.Status = "completed"
		job.UpdatedAt = time.Now()
	}
	h.autoOAuthJobsMu.Unlock()
}

func finishCodexAutoOAuthAccount(current *codexAutoOAuthResult, result codexAutoOAuthResult) {
	if result.Status == "success" {
		result.Step = "已完成"
		result.Progress = 100
	} else {
		result.Step = current.Step
		result.Progress = current.Progress
	}
	*current = result
}

func (h *Handler) updateCodexAutoOAuthAccount(jobID string, index int, update func(*codexAutoOAuthResult)) {
	h.autoOAuthJobsMu.Lock()
	defer h.autoOAuthJobsMu.Unlock()
	job := h.autoOAuthJobs[jobID]
	if job == nil || index < 0 || index >= len(job.Accounts) {
		return
	}
	update(&job.Accounts[index])
	job.UpdatedAt = time.Now()
}

func (h *Handler) runCodexAutoOAuth(ctx context.Context, account codex.AutoOAuthAccount, progress codex.AutoOAuthProgress) codexAutoOAuthResult {
	result := codexAutoOAuthResult{Email: account.Email, Status: "failed", Step: "启动登录", Progress: 5}
	login, errLogin := codex.AutoOAuthLoginWithProgress(ctx, h.cfg, account, h.cfg.ProxyURL, "", "", progress)
	if errLogin != nil {
		result.Error = errLogin.Error()
		return result
	}
	bundle := login.Bundle
	claims, _ := codex.ParseJWTToken(bundle.TokenData.IDToken)
	planType := ""
	hashAccountID := ""
	if claims != nil {
		planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
		if accountID := claims.GetAccountID(); accountID != "" {
			digest := sha256.Sum256([]byte(accountID))
			hashAccountID = hex.EncodeToString(digest[:])[:8]
		}
	}
	progress("保存认证文件", 96)
	service := codex.NewCodexAuth(h.cfg)
	storage := service.CreateTokenStorage(bundle)
	fileName := codex.CredentialFileName(storage.Email, planType, hashAccountID, true)
	autoMetadata := account.Metadata(login.WorkspaceID, login.WorkspaceName)
	autoMetadata["account_id"] = storage.AccountID
	metadata := map[string]any{
		"email":            storage.Email,
		"account_id":       storage.AccountID,
		"type":             "codex",
		codex.AutoOAuthKey: autoMetadata,
		codex.AutoOAuthRecoveryKey: map[string]any{
			"enabled": true,
			"status":  "active",
		},
	}
	storage.SetMetadata(metadata)
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Storage:  storage,
		Metadata: metadata,
		Label:    storage.Email,
	}
	if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
		result.Error = "认证文件保存失败"
		return result
	}
	result.Status = "success"
	result.Step = "已完成"
	result.Progress = 100
	result.FileName = fileName
	result.WorkspaceName = login.WorkspaceName
	return result
}
