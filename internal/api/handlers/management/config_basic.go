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
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) > 0 && config.IsV8ConfigLayout(doc.Content[0]) {
		var err error
		data, _, err = config.NormalizeConfigLayout(data, true)
		if err != nil {
			return err
		}
	}
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
	h.mu.Lock()
	var saved config.Config
	if raw, errRead := os.ReadFile(h.configFilePath); errRead == nil {
		_ = yaml.Unmarshal(raw, &saved)
	}
	h.mu.Unlock()
	var previous *config.CodexStaticISPConfig
	if saved.AccountScheduling != nil {
		previous = saved.AccountScheduling.CodexStaticISP
	}
	body, err = rewriteStaticISPPasswords(body, previous, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": err.Error()})
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
	if cfg.AccountScheduling != nil && cfg.AccountScheduling.CodexStaticISP != nil && len(cfg.AccountScheduling.CodexStaticISP.Nodes) > 0 {
		if errMode := os.Chmod(h.configFilePath, 0600); errMode != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "cannot protect static ISP credentials"})
			return
		}
	}
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

// GetConfigYAML preserves raw YAML unless static ISP passwords require redaction.
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
	data, err = rewriteStaticISPPasswords(data, nil, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config", "message": "cannot redact config YAML"})
		return
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.LogsMaxTotalSizeMB = value
	h.persistLocked(c)
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ErrorLogsMaxFiles = value
	h.persistLocked(c)
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

// GetRoutingSettings returns scheduler controls and their persistence preference.
func (h *Handler) GetRoutingSettings(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg
	strategy := activeRoutingStrategy(cfg)
	persistSettings := cfg != nil && cfg.Routing.PersistSettings != nil && *cfg.Routing.PersistSettings
	unauthorized := cfg != nil && cfg.Routing.AutoDisableUnauthorized
	quota := cfg != nil && cfg.Routing.AutoDisableQuota
	disableCooling := false
	c2aRetryCount := 0
	c2aRetryInterval := 0
	c2aRetryCredentials := 0
	authConcurrencyLimit := 0
	degradedPort := 0
	degradedPortConfigured := false
	exoFreePort := 0
	codexFingerprintConvergence := false
	workBuddyFingerprintConvergence := false
	staticISPEnabled := false
	staticISPNodes := make([]gin.H, 0)
	fakeModels := make([]config.FakeModelConfig, 0)
	codexRoutingHint := "europe"
	cacheTokenRewrite := config.CacheTokenRewriteConfig{}
	if cfg != nil {
		fakeModels = append(fakeModels, config.CloneFakeModels(cfg.Routing.FakeModels)...)
		if cfg.Routing.DegradedPort != nil {
			degradedPort = *cfg.Routing.DegradedPort
			degradedPortConfigured = true
		}
		if cfg.Routing.ExoFreePort != nil {
			exoFreePort = *cfg.Routing.ExoFreePort
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
		if cfg.Routing.WorkBuddyFingerprintConvergence != nil {
			workBuddyFingerprintConvergence = *cfg.Routing.WorkBuddyFingerprintConvergence
		}
		if cfg.Routing.CodexStaticISP != nil {
			staticISPEnabled = cfg.Routing.CodexStaticISP.Enabled
			for _, node := range cfg.Routing.CodexStaticISP.Nodes {
				staticISPNodes = append(staticISPNodes, gin.H{"id": node.ID, "scheme": node.Scheme, "host": node.Host, "port": node.Port, "username": node.Username, "password-configured": node.Password != ""})
			}
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
		"persist-settings":                  persistSettings,
		"strategy":                          strategy,
		"scheduler-mode":                    strategy,
		"auto-disable-unauthorized":         unauthorized,
		"auto-disable-quota":                quota,
		"disable-cooling":                   disableCooling,
		"c2a-max-retry-count":               c2aRetryCount,
		"c2a-max-retry-interval":            c2aRetryInterval,
		"c2a-max-retry-credentials":         c2aRetryCredentials,
		"auth-concurrency-limit":            authConcurrencyLimit,
		"degraded-port":                     degradedPort,
		"degraded-port-configured":          degradedPortConfigured,
		"exo-free-port":                     exoFreePort,
		"codex-fingerprint-convergence":     codexFingerprintConvergence,
		"workbuddy-fingerprint-convergence": workBuddyFingerprintConvergence,
		"codex-routing-hint":                codexRoutingHint,
		"codex-static-isp":                  gin.H{"enabled": staticISPEnabled, "nodes": staticISPNodes},
		"cache-token-rewrite":               cacheTokenRewrite,
		"fake-models":                       fakeModels,
		"runtime_only":                      !persistSettings,
	})
}

// GetCodexStaticISPStatus returns per-account proxy bindings without credentials.
func (h *Handler) GetCodexStaticISPStatus(c *gin.Context) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	bindings := []helps.CodexStaticISPStatus{}
	staticISPError := ""
	if manager != nil {
		if executor, ok := manager.Executor("codex"); ok {
			if source, ok := executor.(interface {
				CodexEgressManager() *helps.CodexEgressManager
			}); ok {
				if keeper := source.CodexEgressManager(); keeper != nil {
					bindings = keeper.StaticISPBindings()
					staticISPError = keeper.StaticISPError()
				}
			}
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"static_isp_bindings": bindings, "static_isp_error": staticISPError,
	})
}

// PutRoutingSettings updates scheduler controls and optionally saves them to config.yaml.
func (h *Handler) PutRoutingSettings(c *gin.Context) {
	var body struct {
		PersistSettings                 *bool                           `json:"persist-settings"`
		Strategy                        *string                         `json:"strategy"`
		SchedulerMode                   *string                         `json:"scheduler-mode"`
		AutoDisableUnauthorized         *bool                           `json:"auto-disable-unauthorized"`
		AutoDisableQuota                *bool                           `json:"auto-disable-quota"`
		DisableCooling                  *bool                           `json:"disable-cooling"`
		C2AMaxRetryCount                *int                            `json:"c2a-max-retry-count"`
		C2AMaxRetryInterval             *int                            `json:"c2a-max-retry-interval"`
		C2AMaxRetryCredentials          *int                            `json:"c2a-max-retry-credentials"`
		AuthConcurrencyLimit            *int                            `json:"auth-concurrency-limit"`
		DegradedPort                    *int                            `json:"degraded-port"`
		ExoFreePort                     *int                            `json:"exo-free-port"`
		CodexFingerprintConvergence     *bool                           `json:"codex-fingerprint-convergence"`
		WorkBuddyFingerprintConvergence *bool                           `json:"workbuddy-fingerprint-convergence"`
		CodexStaticISP                  *staticISPConfigPatch           `json:"codex-static-isp"`
		CodexRoutingHint                *string                         `json:"codex-routing-hint"`
		CacheTokenRewrite               *config.CacheTokenRewriteConfig `json:"cache-token-rewrite"`
		FakeModels                      *[]config.FakeModelConfig       `json:"fake-models"`
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
	if body.FakeModels != nil {
		if errValidate := config.ValidateFakeModels(*body.FakeModels); errValidate != nil {
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
	if body.ExoFreePort != nil && (*body.ExoFreePort < 0 || *body.ExoFreePort > 65535) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exo-free-port must be between 0 and 65535"})
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
	if body.PersistSettings == nil && mode == nil && body.AutoDisableUnauthorized == nil && body.AutoDisableQuota == nil && body.DisableCooling == nil && body.C2AMaxRetryCount == nil && body.C2AMaxRetryInterval == nil && body.C2AMaxRetryCredentials == nil && body.AuthConcurrencyLimit == nil && body.DegradedPort == nil && body.ExoFreePort == nil && body.CodexFingerprintConvergence == nil && body.WorkBuddyFingerprintConvergence == nil && body.CodexStaticISP == nil && body.CodexRoutingHint == nil && body.CacheTokenRewrite == nil && body.FakeModels == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no settings to update"})
		return
	}

	h.applyRuntimeConfig(c, func(cfg *config.Config) error {
		var staticISP *config.CodexStaticISPConfig
		if body.CodexStaticISP != nil {
			var errPool error
			staticISP, errPool = body.CodexStaticISP.apply(cfg.Routing.CodexStaticISP)
			if errPool != nil {
				return errPool
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
		if body.ExoFreePort != nil {
			if h.exoFreePortController == nil {
				return fmt.Errorf("exo-free listener controller unavailable")
			}
			if errPort := h.exoFreePortController(*body.ExoFreePort); errPort != nil {
				return errPort
			}
			value := *body.ExoFreePort
			cfg.Routing.ExoFreePort = &value
		}
		if staticISP != nil {
			cfg.Routing.CodexStaticISP = staticISP
		}
		if body.PersistSettings != nil {
			value := *body.PersistSettings
			cfg.Routing.PersistSettings = &value
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
		if body.WorkBuddyFingerprintConvergence != nil {
			value := *body.WorkBuddyFingerprintConvergence
			cfg.Routing.WorkBuddyFingerprintConvergence = &value
		}
		if body.CodexRoutingHint != nil {
			value := *body.CodexRoutingHint
			cfg.Routing.CodexRoutingHint = &value
		}
		if body.CacheTokenRewrite != nil {
			cfg.CacheTokenRewrite = body.CacheTokenRewrite
		}
		if body.FakeModels != nil {
			cfg.Routing.FakeModels = config.CloneFakeModels(*body.FakeModels)
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
	h.mu.Lock()
	defer h.mu.Unlock()
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ProxyURL = ""
	h.persistLocked(c)
}
