package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/qodercheckin"
)

// PostQoderCheckIn handles POST /qoder-checkin?auth_index=... for both regions.
// The shared coordinator prepares credentials and serializes manual/auto claims.
func (h *Handler) PostQoderCheckIn(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		c.Header("Allow", http.MethodPost)
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "Qoder check-in requires POST"})
		return
	}
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if len(c.QueryArray("auth_index")) > 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index must identify one account"})
		return
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
		if errDecode := decoder.Decode(&body); errDecode != nil || decoder.Decode(new(any)) != io.EOF {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Qoder check-in request"})
			return
		}
		bodyIndex := strings.TrimSpace(body.AuthIndex)
		if authIndex != "" && bodyIndex != "" && authIndex != bodyIndex {
			c.JSON(http.StatusBadRequest, gin.H{"error": "conflicting auth_index values"})
			return
		}
		if authIndex == "" {
			authIndex = bodyIndex
		}
	}
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	h.mu.Lock()
	manager, cfg := h.authManager, h.cfg
	if manager == nil || cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Qoder check-in configuration or auth manager unavailable"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		h.mu.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	if auth.Provider != qoder.ProviderIntl && auth.Provider != qoder.ProviderCN {
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential is not a Qoder account"})
		return
	}
	transport := h.apiCallTransport(auth, "")
	h.mu.Unlock()
	ctx := context.WithValue(c.Request.Context(), "cliproxy.roundtripper", transport)
	result, err := qodercheckin.Run(ctx, manager, auth, cfg, false)
	status := http.StatusOK
	if err != nil {
		status = http.StatusBadGateway
		// No transport, refresh, identity, or token-store details reach the panel.
		result.Status, result.Message = "error", "Qoder check-in failed"
		if c.Request.Context().Err() != nil {
			status = http.StatusRequestTimeout
			result.Message = "Qoder check-in request cancelled"
		}
	}
	c.JSON(status, result)
}
