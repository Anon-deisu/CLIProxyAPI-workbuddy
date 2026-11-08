package management

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/hostmetrics"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func (h *Handler) SetRequestRate(rate *coreusage.RequestRate) {
	h.requestRate = rate
}

func (h *Handler) SetPerformanceStore(path string) error {
	return h.performance.SetStore(path)
}

func (h *Handler) RunPerformance(ctx context.Context) {
	h.performance.Run(ctx)
}

// GetPerformance returns host samples without recording management polling as RPM.
func (h *Handler) GetPerformance(c *gin.Context) {
	now := time.Now()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"system":          h.performance.Snapshot(now),
		"rpm":             h.requestRate.Count(now),
		"window_seconds":  60,
		"refresh_seconds": hostmetrics.SampleInterval.Seconds(),
	})
}
