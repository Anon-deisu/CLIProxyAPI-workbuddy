package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	latestReleaseURL       = "https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest"
	latestReleaseUserAgent = "CLIProxyAPI"
)

func (h *Handler) GetConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{})
		return
	}
	c.JSON(200, new(*h.cfg))
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
}

func setLatestReleaseRequestHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", latestReleaseUserAgent)
	if token := util.ResolveGitHubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// GetLatestVersion returns the latest release version from GitHub without downloading assets.
func (h *Handler) GetLatestVersion(c *gin.Context) {
	client := &http.Client{Timeout: 10 * time.Second}
	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request_create_failed", "message": err.Error()})
		return
	}
	setLatestReleaseRequestHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request_failed", "message": err.Error()})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close latest version response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected_status", "message": fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))})
		return
	}

	var info releaseInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "decode_failed", "message": errDecode.Error()})
		return
	}

	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_response", "message": "missing release version"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"latest-version": version})
}

func WriteConfig(path string, data []byte) error {
	data = config.NormalizeCommentIndentation(data)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	return f.Close()
}

func (h *Handler) PutConfigYAML(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	var cfg config.Config
	if err = yaml.Unmarshal(body, &cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": err.Error()})
		return
	}
	// Validate config using LoadConfigOptional with optional=false to enforce parsing.
	// Use the system temp dir instead of the config directory so validation does
	// not require write permission on the config directory (the service may run
	// as a non-owner group user).
	tmpFile, err := os.CreateTemp("", "config-validate-*.yaml")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	tempFile := tmpFile.Name()
	if _, errWrite := tmpFile.Write(body); errWrite != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errWrite.Error()})
		return
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errClose.Error()})
		return
	}
	defer func() {
		_ = os.Remove(tempFile)
	}()
	_, err = config.LoadConfigOptional(tempFile, false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if WriteConfig(h.configFilePath, body) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to write config"})
		return
	}
	// Reload into handler to keep memory in sync
	newCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reload_failed", "message": err.Error()})
		return
	}
	h.cfg = newCfg.CloneForReload(h.cfg)
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": []string{"config"}})
}

// GetConfigYAML returns the raw config.yaml file bytes without re-encoding.
// It preserves comments and original formatting/styles.
func (h *Handler) GetConfigYAML(c *gin.Context) {
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "config file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	// Write raw bytes as-is
	_, _ = c.Writer.Write(data)
}

// Debug
func (h *Handler) GetDebug(c *gin.Context) { c.JSON(200, gin.H{"debug": h.cfg.Debug}) }
func (h *Handler) PutDebug(c *gin.Context) { h.updateBoolField(c, func(v bool) { h.cfg.Debug = v }) }

// UsageStatisticsEnabled
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	c.JSON(200, gin.H{"usage-statistics-enabled": h.cfg.UsageStatisticsEnabled})
}
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.UsageStatisticsEnabled = v })
}

// UsageStatisticsEnabled
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	c.JSON(200, gin.H{"logging-to-file": h.cfg.LoggingToFile})
}
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.LoggingToFile = v })
}

// LogsMaxTotalSizeMB
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	c.JSON(200, gin.H{"logs-max-total-size-mb": h.cfg.LogsMaxTotalSizeMB})
}
func (h *Handler) PutLogsMaxTotalSizeMB(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 0
	}
	h.cfg.LogsMaxTotalSizeMB = value
	h.persist(c)
}

// ErrorLogsMaxFiles
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	c.JSON(200, gin.H{"error-logs-max-files": h.cfg.ErrorLogsMaxFiles})
}
func (h *Handler) PutErrorLogsMaxFiles(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 10
	}
	h.cfg.ErrorLogsMaxFiles = value
	h.persist(c)
}

// Request log
func (h *Handler) GetRequestLog(c *gin.Context) { c.JSON(200, gin.H{"request-log": h.cfg.RequestLog}) }
func (h *Handler) PutRequestLog(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.RequestLog = v })
}

// Websocket auth
func (h *Handler) GetWebsocketAuth(c *gin.Context) {
	c.JSON(200, gin.H{"ws-auth": h.cfg.WebsocketAuth})
}
func (h *Handler) PutWebsocketAuth(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.WebsocketAuth = v })
}

// Request retry
func (h *Handler) GetRequestRetry(c *gin.Context) {
	c.JSON(200, gin.H{"request-retry": h.cfg.RequestRetry})
}
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.RequestRetry = v })
}

// Max retry credentials
func (h *Handler) GetMaxRetryCredentials(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-credentials": h.cfg.MaxRetryCredentials})
}
func (h *Handler) PutMaxRetryCredentials(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryCredentials = v })
}

// Max retry interval
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-interval": h.cfg.MaxRetryInterval})
}
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryInterval = v })
}

// ForceModelPrefix
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	c.JSON(200, gin.H{"force-model-prefix": h.cfg.ForceModelPrefix})
}
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.ForceModelPrefix = v })
}

func normalizeRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return "weighted-round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	case "remaining-quota", "remaining_quota", "remainingquota", "quota":
		return "remaining-quota", true
	default:
		return "", false
	}
}

func activeRoutingStrategy(cfg *config.Config) string {
	if cfg == nil {
		return "round-robin"
	}
	strategy := strings.TrimSpace(cfg.Routing.SchedulerMode)
	if strategy == "" {
		strategy = strings.TrimSpace(cfg.Routing.Strategy)
	}
	if normalized, ok := normalizeRoutingStrategy(strategy); ok {
		return normalized
	}
	return strategy
}

// RoutingStrategy
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	strategy, ok := normalizeRoutingStrategy(activeRoutingStrategy(h.cfg))
	if !ok {
		c.JSON(200, gin.H{"strategy": strings.TrimSpace(h.cfg.Routing.Strategy)})
		return
	}
	c.JSON(200, gin.H{"strategy": strategy})
}

// GetRoutingSettings returns the runtime-only scheduler controls used by the management panel.
func (h *Handler) GetRoutingSettings(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg
	strategy := activeRoutingStrategy(cfg)
	unauthorized := cfg != nil && cfg.Routing.AutoDisableUnauthorized
	quota := cfg != nil && cfg.Routing.AutoDisableQuota
	disableCooling := false
	c2aRetryCount := 0
	c2aRetryInterval := 0
	c2aRetryCredentials := 0
	authConcurrencyLimit := 0
	degradedPort := 0
	degradedPortConfigured := false
	codexFingerprintConvergence := false
	codexTurnStateEnabled := false
	codexAutoDegrade := false
	cliproxy := config.CliproxyConfig{}
	codexRoutingHint := "europe"
	cacheTokenRewrite := config.CacheTokenRewriteConfig{}
	if cfg != nil {
		if cfg.Routing.DegradedPort != nil {
			degradedPort = *cfg.Routing.DegradedPort
			degradedPortConfigured = true
		}
		if cfg.CacheTokenRewrite != nil {
			cacheTokenRewrite = *cfg.CacheTokenRewrite
		}
		c2aRetryCount = cfg.RequestRetry
		c2aRetryInterval = cfg.MaxRetryInterval
		if cfg.Routing.AuthConcurrencyLimit != nil {
			authConcurrencyLimit = *cfg.Routing.AuthConcurrencyLimit
		}
		if cfg.Routing.CodexFingerprintConvergence != nil {
			codexFingerprintConvergence = *cfg.Routing.CodexFingerprintConvergence
		}
		if cfg.Routing.CodexTurnStateEnabled != nil {
			codexTurnStateEnabled = *cfg.Routing.CodexTurnStateEnabled
		}
		if cfg.Routing.CodexAutoDegrade != nil {
			codexAutoDegrade = *cfg.Routing.CodexAutoDegrade
		}
		if cfg.Routing.CodexStateCliproxy != nil {
			cliproxy = *cfg.Routing.CodexStateCliproxy
		}
		if cfg.Routing.CodexRoutingHint != nil {
			codexRoutingHint = strings.TrimSpace(*cfg.Routing.CodexRoutingHint)
		}
		if cfg.Routing.DisableCooling != nil {
			disableCooling = *cfg.Routing.DisableCooling
		}
		if cfg.Routing.C2AMaxRetryCount != nil {
			c2aRetryCount = *cfg.Routing.C2AMaxRetryCount
		}
		if cfg.Routing.C2AMaxRetryInterval != nil {
			c2aRetryInterval = *cfg.Routing.C2AMaxRetryInterval
		}
		c2aRetryCredentials = cfg.MaxRetryCredentials
		if cfg.Routing.C2AMaxRetryCredentials != nil {
			c2aRetryCredentials = *cfg.Routing.C2AMaxRetryCredentials
		}
	}
	h.mu.Unlock()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"strategy":                      strategy,
		"scheduler-mode":                strategy,
		"auto-disable-unauthorized":     unauthorized,
		"auto-disable-quota":            quota,
		"disable-cooling":               disableCooling,
		"c2a-max-retry-count":           c2aRetryCount,
		"c2a-max-retry-interval":        c2aRetryInterval,
		"c2a-max-retry-credentials":     c2aRetryCredentials,
		"auth-concurrency-limit":        authConcurrencyLimit,
		"degraded-port":                 degradedPort,
		"degraded-port-configured":      degradedPortConfigured,
		"codex-fingerprint-convergence": codexFingerprintConvergence,
		"codex-turn-state-enabled":      codexTurnStateEnabled,
		"codex-auto-degrade":            codexAutoDegrade,
		"codex-state-cliproxy": gin.H{
			"enabled": cliproxy.Enabled, "gateway": cliproxy.Gateway,
			"username": cliproxy.Username, "password-configured": cliproxy.Password != "",
		},
		"codex-routing-hint":  codexRoutingHint,
		"cache-token-rewrite": cacheTokenRewrite,
		"runtime_only":        true,
	})
}

// GetCodexTurnStateStatus returns only per-account validity estimates and collection status.
func (h *Handler) GetCodexTurnStateStatus(c *gin.Context) {
	h.mu.Lock()
	enabled := h.cfg != nil && h.cfg.Routing.CodexTurnStateEnabled != nil && *h.cfg.Routing.CodexTurnStateEnabled
	manager := h.authManager
	h.mu.Unlock()
	accounts := []helps.CodexTurnStateStatus{}
	if manager != nil {
		if executor, ok := manager.Executor("codex"); ok {
			if source, ok := executor.(interface {
				CodexTurnStateKeeper() *helps.CodexTurnStateKeeper
			}); ok {
				if keeper := source.CodexTurnStateKeeper(); keeper != nil {
					accounts = keeper.Snapshot()
				}
			}
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"enabled": enabled, "accounts": accounts, "ttl_seconds": int64(helps.CodexTurnStateTTL / time.Second)})
}

// PutRoutingSettings updates scheduler controls in memory and deliberately skips config.yaml writes.
func (h *Handler) PutRoutingSettings(c *gin.Context) {
	var body struct {
		Strategy                    *string `json:"strategy"`
		SchedulerMode               *string `json:"scheduler-mode"`
		AutoDisableUnauthorized     *bool   `json:"auto-disable-unauthorized"`
		AutoDisableQuota            *bool   `json:"auto-disable-quota"`
		DisableCooling              *bool   `json:"disable-cooling"`
		C2AMaxRetryCount            *int    `json:"c2a-max-retry-count"`
		C2AMaxRetryInterval         *int    `json:"c2a-max-retry-interval"`
		C2AMaxRetryCredentials      *int    `json:"c2a-max-retry-credentials"`
		AuthConcurrencyLimit        *int    `json:"auth-concurrency-limit"`
		DegradedPort                *int    `json:"degraded-port"`
		CodexFingerprintConvergence *bool   `json:"codex-fingerprint-convergence"`
		CodexTurnStateEnabled       *bool   `json:"codex-turn-state-enabled"`
		CodexAutoDegrade            *bool   `json:"codex-auto-degrade"`
		CodexStateCliproxy          *struct {
			Enabled  *bool   `json:"enabled"`
			Gateway  *string `json:"gateway"`
			Username *string `json:"username"`
			Password *string `json:"password"`
		} `json:"codex-state-cliproxy"`
		CodexRoutingHint  *string                         `json:"codex-routing-hint"`
		CacheTokenRewrite *config.CacheTokenRewriteConfig `json:"cache-token-rewrite"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.CacheTokenRewrite != nil {
		if errValidate := body.CacheTokenRewrite.Validate(); errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
	}
	mode := body.SchedulerMode
	if mode == nil {
		mode = body.Strategy
	}
	var normalizedMode string
	if mode != nil {
		var ok bool
		normalizedMode, ok = normalizeRoutingStrategy(*mode)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
			return
		}
	}
	if body.C2AMaxRetryCount != nil && (*body.C2AMaxRetryCount < -1 || *body.C2AMaxRetryCount > 1000000) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "c2a-max-retry-count must be -1 or between 0 and 1000000"})
		return
	}
	if body.C2AMaxRetryInterval != nil && *body.C2AMaxRetryInterval < -1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "c2a-max-retry-interval must be -1 or non-negative"})
		return
	}
	if body.C2AMaxRetryCredentials != nil && (*body.C2AMaxRetryCredentials < -1 || *body.C2AMaxRetryCredentials > 1000000) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "c2a-max-retry-credentials must be -1 or between 0 and 1000000"})
		return
	}
	if body.AuthConcurrencyLimit != nil && (*body.AuthConcurrencyLimit < 0 || *body.AuthConcurrencyLimit > 1000000) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth-concurrency-limit must be between 0 and 1000000"})
		return
	}
	if body.DegradedPort != nil && (*body.DegradedPort < 0 || *body.DegradedPort > 65535) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "degraded-port must be between 0 and 65535"})
		return
	}
	if body.CodexRoutingHint != nil {
		value := strings.ToLower(strings.TrimSpace(*body.CodexRoutingHint))
		switch value {
		case "off", "none", "disabled":
			value = ""
		case "", "europe", "north-america", "asia-pacific":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid codex-routing-hint"})
			return
		}
		body.CodexRoutingHint = &value
	}
	if mode == nil && body.AutoDisableUnauthorized == nil && body.AutoDisableQuota == nil && body.DisableCooling == nil && body.C2AMaxRetryCount == nil && body.C2AMaxRetryInterval == nil && body.C2AMaxRetryCredentials == nil && body.AuthConcurrencyLimit == nil && body.DegradedPort == nil && body.CodexFingerprintConvergence == nil && body.CodexTurnStateEnabled == nil && body.CodexAutoDegrade == nil && body.CodexStateCliproxy == nil && body.CodexRoutingHint == nil && body.CacheTokenRewrite == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no settings to update"})
		return
	}

	h.applyRuntimeConfig(c, func(cfg *config.Config) error {
		var cliproxy *config.CliproxyConfig
		if patch := body.CodexStateCliproxy; patch != nil {
			cliproxy = &config.CliproxyConfig{}
			if cfg.Routing.CodexStateCliproxy != nil {
				*cliproxy = *cfg.Routing.CodexStateCliproxy
			}
			if patch.Enabled != nil {
				cliproxy.Enabled = *patch.Enabled
			}
			if patch.Gateway != nil {
				cliproxy.Gateway = strings.TrimSpace(*patch.Gateway)
			}
			if patch.Username != nil {
				cliproxy.Username = strings.TrimSpace(*patch.Username)
			}
			if patch.Password != nil {
				cliproxy.Password = *patch.Password
			}
			if _, errProxy := cliproxy.ProxyURL(); errProxy != nil {
				return errProxy
			}
		}
		if body.DegradedPort != nil {
			if h.degradedPortController == nil {
				return fmt.Errorf("degraded listener controller unavailable")
			}
			if errPort := h.degradedPortController(*body.DegradedPort); errPort != nil {
				return errPort
			}
			value := *body.DegradedPort
			cfg.Routing.DegradedPort = &value
		}
		if cliproxy != nil {
			cfg.Routing.CodexStateCliproxy = cliproxy
		}
		if mode != nil {
			cfg.Routing.SchedulerMode = normalizedMode
		}
		if body.AutoDisableUnauthorized != nil {
			cfg.Routing.AutoDisableUnauthorized = *body.AutoDisableUnauthorized
		}
		if body.AutoDisableQuota != nil {
			cfg.Routing.AutoDisableQuota = *body.AutoDisableQuota
		}
		if body.DisableCooling != nil {
			value := *body.DisableCooling
			cfg.Routing.DisableCooling = &value
		}
		if body.C2AMaxRetryCount != nil {
			if *body.C2AMaxRetryCount < 0 {
				cfg.Routing.C2AMaxRetryCount = nil
			} else {
				value := *body.C2AMaxRetryCount
				cfg.Routing.C2AMaxRetryCount = &value
			}
		}
		if body.C2AMaxRetryInterval != nil {
			if *body.C2AMaxRetryInterval < 0 {
				cfg.Routing.C2AMaxRetryInterval = nil
			} else {
				value := *body.C2AMaxRetryInterval
				cfg.Routing.C2AMaxRetryInterval = &value
			}
		}
		if body.C2AMaxRetryCredentials != nil {
			if *body.C2AMaxRetryCredentials < 0 {
				cfg.Routing.C2AMaxRetryCredentials = nil
			} else {
				value := *body.C2AMaxRetryCredentials
				cfg.Routing.C2AMaxRetryCredentials = &value
			}
		}
		if body.AuthConcurrencyLimit != nil {
			value := *body.AuthConcurrencyLimit
			cfg.Routing.AuthConcurrencyLimit = &value
		}
		if body.CodexFingerprintConvergence != nil {
			value := *body.CodexFingerprintConvergence
			cfg.Routing.CodexFingerprintConvergence = &value
		}
		if body.CodexTurnStateEnabled != nil {
			value := *body.CodexTurnStateEnabled
			cfg.Routing.CodexTurnStateEnabled = &value
		}
		if body.CodexAutoDegrade != nil {
			value := *body.CodexAutoDegrade
			cfg.Routing.CodexAutoDegrade = &value
		}
		if body.CodexRoutingHint != nil {
			value := *body.CodexRoutingHint
			cfg.Routing.CodexRoutingHint = &value
		}
		if body.CacheTokenRewrite != nil {
			cfg.CacheTokenRewrite = body.CacheTokenRewrite
		}
		return nil
	})
}
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	var body struct {
		Value *string `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	normalized, ok := normalizeRoutingStrategy(*body.Value)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
		return
	}
	h.cfg.Routing.Strategy = normalized
	h.cfg.Routing.SchedulerMode = ""
	h.persist(c)
}

// Proxy URL
func (h *Handler) GetProxyURL(c *gin.Context) { c.JSON(200, gin.H{"proxy-url": h.cfg.ProxyURL}) }
func (h *Handler) PutProxyURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.ProxyURL = v })
}
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	h.cfg.ProxyURL = ""
	h.persist(c)
}
