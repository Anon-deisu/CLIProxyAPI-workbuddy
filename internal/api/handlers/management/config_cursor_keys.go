package management

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type cursorKeyInput struct {
	APIKey         string   `json:"api-key"`
	Priority       int      `json:"priority,omitempty"`
	Prefix         string   `json:"prefix,omitempty"`
	BaseURL        string   `json:"base-url,omitempty"`
	ExcludedModels []string `json:"excluded-models,omitempty"`
	DisableCooling bool     `json:"disable-cooling,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
}

type cursorKeyPatch struct {
	APIKey         *string   `json:"api-key"`
	Priority       *int      `json:"priority"`
	Prefix         *string   `json:"prefix"`
	BaseURL        *string   `json:"base-url"`
	ExcludedModels *[]string `json:"excluded-models"`
	DisableCooling *bool     `json:"disable-cooling"`
	Disabled       *bool     `json:"disabled"`
}

type cursorKeyView struct {
	Index          int                            `json:"index"`
	APIKey         string                         `json:"api-key"`
	APIKeySet      bool                           `json:"api-key-set"`
	Priority       int                            `json:"priority,omitempty"`
	Prefix         string                         `json:"prefix,omitempty"`
	BaseURL        string                         `json:"base-url,omitempty"`
	ExcludedModels []string                       `json:"excluded-models,omitempty"`
	DisableCooling bool                           `json:"disable-cooling,omitempty"`
	Disabled       bool                           `json:"disabled"`
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

// GetCursorKeys returns Cursor API key settings without exposing credential
// material. Mutations use stable list indexes, so clients never need the raw key.
func (h *Handler) GetCursorKeys(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	runtimeByID := h.liveCursorKeyRuntimeByID()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}

	idGen := synthesizer.NewStableIDGenerator()
	out := make([]cursorKeyView, 0, len(h.cfg.CursorKey))
	for i := range h.cfg.CursorKey {
		entry := h.cfg.CursorKey[i]
		baseURL := config.NormalizeCursorBaseURL(entry.BaseURL)
		id, _ := idGen.Next("cursor:apikey", strings.TrimSpace(entry.APIKey), baseURL)
		runtime := runtimeByID[id]
		out = append(out, cursorKeyView{
			Index:          i,
			APIKey:         maskCursorAPIKey(entry.APIKey),
			APIKeySet:      strings.TrimSpace(entry.APIKey) != "",
			Priority:       entry.Priority,
			Prefix:         entry.Prefix,
			BaseURL:        baseURL,
			ExcludedModels: cursorVisibleExcludedModels(entry.ExcludedModels),
			DisableCooling: entry.DisableCooling,
			Disabled:       cursorKeyDisabled(entry.ExcludedModels),
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

// PutCursorKeys replaces the complete Cursor API key list. The management UI
// uses POST/PATCH so a masked GET response can never accidentally overwrite keys.
func (h *Handler) PutCursorKeys(c *gin.Context) {
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

	entries := make([]config.CursorKey, 0, len(inputs))
	for i := range inputs {
		entry, ok := cursorKeyFromInput(inputs[i])
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "api-key is required"})
			return
		}
		entries = append(entries, entry)
	}
	normalized, duplicate := normalizeCursorKeyEntries(entries)
	if duplicate {
		c.JSON(http.StatusConflict, gin.H{"error": "duplicate cursor API key and base URL"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	h.cfg.CursorKey = normalized
	h.persistLocked(c)
}

// PostCursorKey appends one Cursor API key without requiring clients to resend
// the existing (masked) credentials.
func (h *Handler) PostCursorKey(c *gin.Context) {
	var input cursorKeyInput
	if errBind := c.ShouldBindJSON(&input); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	entry, ok := cursorKeyFromInput(input)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api-key is required"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	entries := append(append([]config.CursorKey(nil), h.cfg.CursorKey...), entry)
	normalized, duplicate := normalizeCursorKeyEntries(entries)
	if duplicate {
		c.JSON(http.StatusConflict, gin.H{"error": "duplicate cursor API key and base URL"})
		return
	}
	h.cfg.CursorKey = normalized
	h.persistLocked(c)
}

// PatchCursorKey updates one entry by index. An omitted or blank api-key keeps
// the stored secret unchanged, which makes password-style edit forms safe.
func (h *Handler) PatchCursorKey(c *gin.Context) {
	var body struct {
		Index *int            `json:"index"`
		Value *cursorKeyPatch `json:"value"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil || body.Index == nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "index and value are required"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	index := *body.Index
	if index < 0 || index >= len(h.cfg.CursorKey) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.CursorKey[index]
	patch := body.Value
	if patch.APIKey != nil {
		if value := strings.TrimSpace(*patch.APIKey); value != "" {
			entry.APIKey = value
		}
	}
	if patch.Priority != nil {
		entry.Priority = *patch.Priority
	}
	if patch.Prefix != nil {
		entry.Prefix = *patch.Prefix
	}
	if patch.BaseURL != nil {
		entry.BaseURL = *patch.BaseURL
	}
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = append([]string(nil), (*patch.ExcludedModels)...)
	}
	if patch.DisableCooling != nil {
		entry.DisableCooling = *patch.DisableCooling
	}
	if patch.Disabled != nil {
		entry.ExcludedModels = setConfigAPIKeyExcludedAll(entry.ExcludedModels, *patch.Disabled)
	}

	entries := append([]config.CursorKey(nil), h.cfg.CursorKey...)
	entries[index] = entry
	normalized, duplicate := normalizeCursorKeyEntries(entries)
	if duplicate {
		c.JSON(http.StatusConflict, gin.H{"error": "duplicate cursor API key and base URL"})
		return
	}
	h.cfg.CursorKey = normalized
	h.persistLocked(c)
}

func (h *Handler) DeleteCursorKey(c *gin.Context) {
	index, ok := cursorQueryIndex(c)
	if !ok {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	if index < 0 || index >= len(h.cfg.CursorKey) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	h.cfg.CursorKey = append(h.cfg.CursorKey[:index], h.cfg.CursorKey[index+1:]...)
	h.cfg.SanitizeCursorKeys()
	h.persistLocked(c)
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

func cursorKeyFromInput(input cursorKeyInput) (config.CursorKey, bool) {
	entry := config.CursorKey{
		APIKey:         input.APIKey,
		Priority:       input.Priority,
		Prefix:         input.Prefix,
		BaseURL:        input.BaseURL,
		ExcludedModels: append([]string(nil), input.ExcludedModels...),
		DisableCooling: input.DisableCooling,
	}
	if input.Disabled {
		entry.ExcludedModels = setConfigAPIKeyExcludedAll(entry.ExcludedModels, true)
	}
	normalized, _ := normalizeCursorKeyEntries([]config.CursorKey{entry})
	if len(normalized) != 1 {
		return config.CursorKey{}, false
	}
	return normalized[0], true
}

func normalizeCursorKeyEntries(entries []config.CursorKey) ([]config.CursorKey, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	normalized := append([]config.CursorKey(nil), entries...)
	cfg := config.Config{CursorKey: normalized}
	cfg.SanitizeCursorKeys()
	return cfg.CursorKey, len(cfg.CursorKey) != len(entries)
}

func cursorKeyDisabled(models []string) bool {
	for _, model := range models {
		if strings.TrimSpace(model) == configAPIKeyDisablePattern {
			return true
		}
	}
	return false
}

func cursorVisibleExcludedModels(models []string) []string {
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
