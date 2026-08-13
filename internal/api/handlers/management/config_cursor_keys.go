package management

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type cursorKeyInput struct {
	APIKey         string   `json:"api-key"`
	Email          string   `json:"email,omitempty"`
	Priority       int      `json:"priority,omitempty"`
	Prefix         string   `json:"prefix,omitempty"`
	BaseURL        string   `json:"base-url,omitempty"`
	ExcludedModels []string `json:"excluded-models,omitempty"`
	DisableCooling bool     `json:"disable-cooling,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
	Note           string   `json:"note,omitempty"`
}

type cursorKeyPatch struct {
	APIKey         *string   `json:"api-key"`
	Email          *string   `json:"email"`
	Priority       *int      `json:"priority"`
	Prefix         *string   `json:"prefix"`
	BaseURL        *string   `json:"base-url"`
	ExcludedModels *[]string `json:"excluded-models"`
	DisableCooling *bool     `json:"disable-cooling"`
	Disabled       *bool     `json:"disabled"`
	Note           *string   `json:"note"`
}

type cursorKeyView struct {
	Index          int                            `json:"index"`
	APIKey         string                         `json:"api-key"`
	APIKeySet      bool                           `json:"api-key-set"`
	Email          string                         `json:"email,omitempty"`
	Priority       int                            `json:"priority,omitempty"`
	Prefix         string                         `json:"prefix,omitempty"`
	BaseURL        string                         `json:"base-url,omitempty"`
	ExcludedModels []string                       `json:"excluded-models,omitempty"`
	DisableCooling bool                           `json:"disable-cooling,omitempty"`
	Disabled       bool                           `json:"disabled"`
	Note           string                         `json:"note,omitempty"`
	AuthIndex      string                         `json:"auth-index,omitempty"`
	Success        int64                          `json:"success"`
	Failed         int64                          `json:"failed"`
	Status         coreauth.Status                `json:"status,omitempty"`
	StatusMessage  string                         `json:"status_message,omitempty"`
	Unavailable    bool                           `json:"unavailable"`
	LastError      *coreauth.Error                `json:"last_error,omitempty"`
	NextRetryAfter *time.Time                     `json:"next_retry_after,omitempty"`
	UpdatedAt      *time.Time                     `json:"updated_at,omitempty"`
	RecentRequests []coreauth.RecentRequestBucket `json:"recent_requests,omitempty"`
}

// cursorKeyStore returns a file-backed store rooted at the configured auth
// directory. It is built on demand so a fresh view of the directory is always
// reflected, and it stays nil-safe (callers treat it as unavailable).
func (h *Handler) cursorKeyStore() *cursor.APIKeyStore {
	if h == nil || h.cfg == nil {
		return nil
	}
	return cursor.NewAPIKeyStore(h.cfg.AuthDir)
}

// GetCursorKeys returns Cursor API key settings without exposing credential
// material. Mutations use stable list indexes, so clients never need the raw key.
func (h *Handler) GetCursorKeys(c *gin.Context) {
	store := h.cursorKeyStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	runtimeByID := h.liveCursorKeyRuntimeByID()

	list, err := store.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list cursor API keys"})
		return
	}
	out := make([]cursorKeyView, 0, len(list))
	for i, rec := range list {
		baseURL := rec.ResolvedBaseURL()
		runtime := runtimeByID[filepath.Base(rec.FileName())]
		out = append(out, cursorKeyView{
			Index:          i,
			APIKey:         maskCursorAPIKey(rec.APIKey),
			APIKeySet:      strings.TrimSpace(rec.APIKey) != "",
			Email:          rec.Email,
			Priority:       rec.Priority,
			Prefix:         rec.Prefix,
			BaseURL:        baseURL,
			ExcludedModels: cursorVisibleExcludedModels(rec.ExcludedModels),
			DisableCooling: rec.DisableCooling,
			Disabled:       rec.Disabled,
			Note:           rec.Note,
			AuthIndex:      runtime.AuthIndex,
			Success:        runtime.Success,
			Failed:         runtime.Failed,
			Status:         runtime.Status,
			StatusMessage:  runtime.StatusMessage,
			Unavailable:    runtime.Unavailable,
			LastError:      runtime.LastError,
			NextRetryAfter: runtime.NextRetryAfter,
			UpdatedAt:      runtime.UpdatedAt,
			RecentRequests: runtime.RecentRequests,
		})
	}
	c.JSON(http.StatusOK, gin.H{"cursor-api-key": out})
}

type cursorKeyRuntimeView struct {
	AuthIndex      string
	Success        int64
	Failed         int64
	Status         coreauth.Status
	StatusMessage  string
	Unavailable    bool
	LastError      *coreauth.Error
	NextRetryAfter *time.Time
	UpdatedAt      *time.Time
	RecentRequests []coreauth.RecentRequestBucket
}

func (h *Handler) liveCursorKeyRuntimeByID() map[string]cursorKeyRuntimeView {
	out := map[string]cursorKeyRuntimeView{}
	if h == nil {
		return out
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return out
	}
	now := time.Now()
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		id := strings.TrimSpace(auth.ID)
		if id == "" {
			continue
		}
		index := strings.TrimSpace(auth.Index)
		if index == "" {
			index = auth.EnsureIndex()
		}
		runtime := cursorKeyRuntimeView{
			AuthIndex:      index,
			Success:        auth.Success,
			Failed:         auth.Failed,
			Status:         auth.Status,
			StatusMessage:  auth.StatusMessage,
			Unavailable:    auth.Unavailable,
			RecentRequests: auth.RecentRequestsSnapshot(now),
		}
		if auth.LastError != nil {
			lastError := *auth.LastError
			runtime.LastError = &lastError
		}
		if !auth.NextRetryAfter.IsZero() {
			nextRetryAfter := auth.NextRetryAfter
			runtime.NextRetryAfter = &nextRetryAfter
		}
		if !auth.UpdatedAt.IsZero() {
			updatedAt := auth.UpdatedAt
			runtime.UpdatedAt = &updatedAt
		}
		out[id] = runtime
	}
	return out
}

// cursorKeyFromInput converts an API key input into the on-disk JSON record.
// A blank API key is rejected here so the store's "api key required" contract
// is enforced at the HTTP boundary with a readable message.
func cursorKeyFromInput(input cursorKeyInput) (*cursor.APIKeyStorage, bool) {
	rec := &cursor.APIKeyStorage{
		APIKey:         input.APIKey,
		Email:          input.Email,
		Priority:       input.Priority,
		Prefix:         input.Prefix,
		BaseURL:        input.BaseURL,
		ExcludedModels: append([]string(nil), input.ExcludedModels...),
		DisableCooling: input.DisableCooling,
		Disabled:       input.Disabled,
		Note:           input.Note,
	}
	if strings.TrimSpace(rec.APIKey) == "" {
		return nil, false
	}
	return rec, true
}

// PutCursorKeys replaces the complete Cursor API key list. The management UI
// uses POST/PATCH so a masked GET response can never accidentally overwrite keys.
func (h *Handler) PutCursorKeys(c *gin.Context) {
	store := h.cursorKeyStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	data, errRead := c.GetRawData()
	if errRead != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var inputs []cursorKeyInput
	if errUnmarshal := json.Unmarshal(data, &inputs); errUnmarshal != nil {
		var wrapper struct {
			Items *[]cursorKeyInput `json:"items"`
		}
		if errWrapper := json.Unmarshal(data, &wrapper); errWrapper != nil || wrapper.Items == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		inputs = *wrapper.Items
	}

	records := make([]*cursor.APIKeyStorage, 0, len(inputs))
	for i := range inputs {
		rec, ok := cursorKeyFromInput(inputs[i])
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "api-key is required"})
			return
		}
		records = append(records, rec)
	}

	// Remove every existing credential first so the list is a true replace.
	list, errList := store.List()
	if errList != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list cursor API keys"})
		return
	}
	for i := len(list) - 1; i >= 0; i-- {
		if err := store.Delete(i); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to replace cursor API keys"})
			return
		}
	}
	for _, rec := range records {
		if _, err := store.Create(rec); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save cursor API key"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"cursor-api-key": "ok"})
}

// PostCursorKey appends one Cursor API key without requiring clients to resend
// the existing (masked) credentials.
func (h *Handler) PostCursorKey(c *gin.Context) {
	store := h.cursorKeyStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	var input cursorKeyInput
	if errBind := c.ShouldBindJSON(&input); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	rec, ok := cursorKeyFromInput(input)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api-key is required"})
		return
	}
	if _, err := store.Create(rec); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save cursor API key"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cursor-api-key": "ok"})
}

// PatchCursorKey updates one entry by index. An omitted or blank api-key keeps
// the stored secret unchanged, which makes password-style edit forms safe.
func (h *Handler) PatchCursorKey(c *gin.Context) {
	store := h.cursorKeyStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	var body struct {
		Index *int            `json:"index"`
		Value *cursorKeyPatch `json:"value"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil || body.Index == nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "index and value are required"})
		return
	}

	rec, errGet := store.Get(*body.Index)
	if errGet != nil {
		if cursor.IsIndexOutOfRange(errGet) {
			c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load cursor API key"})
		return
	}
	patch := body.Value
	if patch.APIKey != nil {
		if value := strings.TrimSpace(*patch.APIKey); value != "" {
			rec.APIKey = value
		}
	}
	if patch.Email != nil {
		rec.Email = *patch.Email
	}
	if patch.Priority != nil {
		rec.Priority = *patch.Priority
	}
	if patch.Prefix != nil {
		rec.Prefix = *patch.Prefix
	}
	if patch.BaseURL != nil {
		rec.BaseURL = *patch.BaseURL
	}
	if patch.ExcludedModels != nil {
		rec.ExcludedModels = append([]string(nil), (*patch.ExcludedModels)...)
	}
	if patch.DisableCooling != nil {
		rec.DisableCooling = *patch.DisableCooling
	}
	if patch.Disabled != nil {
		rec.Disabled = *patch.Disabled
	}
	if patch.Note != nil {
		rec.Note = *patch.Note
	}

	if err := store.Update(*body.Index, rec); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update cursor API key"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cursor-api-key": "ok"})
}

func (h *Handler) DeleteCursorKey(c *gin.Context) {
	store := h.cursorKeyStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	index, ok := cursorQueryIndex(c)
	if !ok {
		return
	}
	if err := store.Delete(index); err != nil {
		if cursor.IsIndexOutOfRange(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete cursor API key"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cursor-api-key": "ok"})
}

func cursorQueryIndex(c *gin.Context) (int, bool) {
	raw := strings.TrimSpace(c.Query("index"))
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "index is required"})
		return 0, false
	}
	index := -1
	if parsed, errParse := strconv.Atoi(raw); errParse != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid index"})
		return 0, false
	} else {
		index = parsed
	}
	return index, true
}

// cursorVisibleExcludedModels hides the "*" disable sentinel from the API view
// (a disabled credential is surfaced through the Disabled bool instead), while
// preserving any real model-level exclusions.
func cursorVisibleExcludedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	out := make([]string, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model) != configAPIKeyDisablePattern {
			out = append(out, model)
		}
	}
	return config.NormalizeExcludedModels(out)
}

func maskCursorAPIKey(raw string) string {
	key := strings.TrimSpace(raw)
	if key == "" {
		return ""
	}
	prefix := ""
	switch {
	case strings.HasPrefix(key, "crsr_"):
		prefix = "crsr_"
	case strings.HasPrefix(key, "cursor_"):
		prefix = "cursor_"
	default:
		if len(key) > 4 {
			prefix = key[:4]
		}
	}
	suffix := key
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	}
	if len(key) <= len(prefix)+4 {
		return prefix + "****"
	}
	return prefix + "********" + suffix
}
