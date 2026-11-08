// Package management provides the management API handlers and middleware
// for configuring the server and managing auth files.
package management

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/hostmetrics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

type attemptInfo struct {
	count        int
	blockedUntil time.Time
	lastActivity time.Time // track last activity for cleanup
}

// attemptCleanupInterval controls how often stale IP entries are purged
const attemptCleanupInterval = 1 * time.Hour

// attemptMaxIdleTime controls how long an IP can be idle before cleanup
const attemptMaxIdleTime = 2 * time.Hour

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                     *config.Config
	configFilePath          string
	staleConfigFileHashes   []string
	pendingConfigFileHash   string
	mu                      sync.Mutex
	authStatusMu            sync.Mutex
	reloadMu                sync.Mutex
	reloadGeneration        uint64
	appliedReloadGeneration uint64
	attemptsMu              sync.Mutex
	failedAttempts          map[string]*attemptInfo // keyed by client IP
	authManager             *coreauth.Manager
	tokenStore              coreauth.Store
	localPassword           string
	allowRemoteOverride     bool
	envSecret               string
	logDir                  string
	postAuthHook            coreauth.PostAuthHook
	postAuthPersistHook     coreauth.PostAuthHook
	pluginHost              *pluginhost.Host
	configReloadHook        func(context.Context, *config.Config)
	runtimeConfigHook       func(context.Context, *config.Config)
	degradedPortController  func(int) error
	exoFreePortController   func(int) error
	pluginStoreRegistryURL  string
	pluginStoreHTTPClient   pluginstore.HTTPDoer
	pluginStoreRateLimiter  *pluginstore.GitHubRateLimiter
	pluginReleases          pluginReleaseCache
	autoOAuthJobsMu         sync.Mutex
	autoOAuthJobs           map[string]*codexAutoOAuthJob
	siwcOAuthFactory        func(*config.Config) siwcOAuthService
	siwcCommitMu            sync.Mutex
	pluginLoginSaveMu       sync.Mutex
	workBuddyTasks          *WorkBuddyTaskService
	performance             *hostmetrics.Sampler
	requestRate             *coreusage.RequestRate
}

type configReloadSnapshot struct {
	cfg        *config.Config
	generation uint64
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)

	h := &Handler{
		cfg:                 cfg,
		configFilePath:      configFilePath,
		failedAttempts:      make(map[string]*attemptInfo),
		authManager:         manager,
		tokenStore:          sdkAuth.GetTokenStore(),
		allowRemoteOverride: envSecret != "",
		envSecret:           envSecret,
		autoOAuthJobs:       make(map[string]*codexAutoOAuthJob),
		performance:         hostmetrics.NewSampler(),
		requestRate:         &coreusage.RequestRate{},
	}
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	h.workBuddyTasks = NewWorkBuddyTaskService(h.cfg, manager)
	h.workBuddyTasks.Start()
	h.startAttemptCleanup()
	return h
}

// startAttemptCleanup launches a background goroutine that periodically
// removes stale IP entries from failedAttempts to prevent memory leaks.
func (h *Handler) startAttemptCleanup() {
	go func() {
		ticker := time.NewTicker(attemptCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.purgeStaleAttempts()
		}
	}()
}

// purgeStaleAttempts removes IP entries that have been idle beyond attemptMaxIdleTime
// and whose ban (if any) has expired.
func (h *Handler) purgeStaleAttempts() {
	now := time.Now()
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	for ip, ai := range h.failedAttempts {
		// Skip if still banned
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			continue
		}
		// Remove if idle too long
		if now.Sub(ai.lastActivity) > attemptMaxIdleTime {
			delete(h.failedAttempts, ip)
		}
	}
}

// NewHandler creates a new management handler instance.
func NewHandlerWithoutConfigFilePath(cfg *config.Config, manager *coreauth.Manager) *Handler {
	return NewHandler(cfg, "", manager)
}

// SetConfig installs an independent config snapshot when the server hot-reloads.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil {
		return
	}
	cloned := cfg.CloneForRuntime()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.isStaleConfigFileSnapshotLocked(cloned) {
		log.Debug("management: ignoring config reload parsed from an older config file version")
		return
	}
	h.cfg = cloned.CloneForReload(h.cfg)
	if h.workBuddyTasks != nil {
		h.workBuddyTasks.Configure(h.cfg)
	}
}

// CloseWorkBuddyTasks stops background task requests owned by this handler.
func (h *Handler) CloseWorkBuddyTasks() {
	if h != nil && h.workBuddyTasks != nil {
		h.workBuddyTasks.Close()
	}
}

// configSnapshot reads h.cfg under h.mu, for code that runs outside the request holding it.
func (h *Handler) configSnapshot() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// oauthConfigWithProxy returns a configuration snapshot for an OAuth session.
// A session proxy overrides the global proxy without mutating the shared config.
func (h *Handler) oauthConfigWithProxy(proxyURL string) *config.Config {
	cfg := h.configSnapshot()
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return cfg
	}
	if cfg == nil {
		return &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxyURL}}
	}
	cloned := cfg.CloneForRuntime()
	if cloned == nil {
		copied := *cfg
		cloned = &copied
	}
	cloned.ProxyURL = proxyURL
	return cloned
}

// isStaleConfigFileSnapshotLocked checks the source version while h.mu is held.
func (h *Handler) isStaleConfigFileSnapshotLocked(cfg *config.Config) bool {
	if cfg == nil || h.configFilePath == "" {
		return false
	}
	data, errRead := os.ReadFile(h.configFilePath)
	if errRead != nil {
		return false
	}
	if cfg.SourceHash != "" {
		return config.ComputeSourceHash(data) != cfg.SourceHash
	}

	diskCfg, errLoad := config.ParseConfigBytes(data)
	if errLoad != nil {
		return false
	}
	candidateHash := managementConfigHash(cfg)
	diskHash := managementConfigHash(diskCfg)
	if candidateHash == "" || candidateHash == diskHash {
		return false
	}
	for _, hash := range h.staleConfigFileHashes {
		if hash != "" && hash == candidateHash {
			return true
		}
	}
	return false
}

// SetAuthManager updates the auth manager reference used by management endpoints.
func (h *Handler) SetAuthManager(manager *coreauth.Manager) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.authManager != manager {
		h.authManager = manager
	}
	h.mu.Unlock()
}

// SetPluginHost updates the plugin host used by plugin-backed management endpoints.
func (h *Handler) SetPluginHost(host *pluginhost.Host) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.pluginHost != host {
		h.pluginHost = host
	}
	h.mu.Unlock()
}

// SetConfigReloadHook updates the callback used after management saves config changes.
func (h *Handler) SetConfigReloadHook(hook func(context.Context, *config.Config)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.configReloadHook = hook
	h.mu.Unlock()
}

// SetRuntimeConfigHook updates the callback used by runtime-only management settings.
func (h *Handler) SetRuntimeConfigHook(hook func(context.Context, *config.Config)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.runtimeConfigHook = hook
	h.mu.Unlock()
}

// SetDegradedPortController installs the server-owned listener controller.
func (h *Handler) SetDegradedPortController(controller func(int) error) {
	h.mu.Lock()
	h.degradedPortController = controller
	h.mu.Unlock()
}

// SetExoFreePortController installs the dedicated streaming listener controller.
func (h *Handler) SetExoFreePortController(controller func(int) error) {
	h.mu.Lock()
	h.exoFreePortController = controller
	h.mu.Unlock()
}

// applyRuntimeConfig applies panel changes even if their optional file write fails.
func (h *Handler) applyRuntimeConfig(c *gin.Context, mutate func(*config.Config) error) bool {
	if h == nil || mutate == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "management handler unavailable"})
		return false
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return false
	}
	h.noteConfigMutationLocked()
	if errMutate := mutate(h.cfg); errMutate != nil {
		h.pendingConfigFileHash = ""
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": errMutate.Error()})
		return false
	}
	persisted := false
	var errSave error
	if h.cfg.Routing.PersistSettings != nil && *h.cfg.Routing.PersistSettings {
		persistCfg := h.cfg.CloneForRuntime()
		persistCfg.AccountScheduling = h.cfg.AccountSchedulingSnapshot()
		errSave = config.SaveConfigPreserveComments(h.configFilePath, persistCfg)
		if errSave == nil {
			h.recordConfigFileSnapshotLocked()
			h.cfg.AccountScheduling = persistCfg.AccountScheduling
			persisted = true
		}
	}
	if !persisted {
		h.pendingConfigFileHash = ""
	}
	snapshot := h.cfg.CloneForRuntime()
	hook := h.runtimeConfigHook
	manager := h.authManager
	h.mu.Unlock()

	if h.workBuddyTasks != nil {
		h.workBuddyTasks.Configure(snapshot)
	}
	if hook != nil {
		hook(c.Request.Context(), snapshot)
	} else if manager != nil {
		manager.SetConfig(snapshot)
	}
	if manager != nil && snapshot.Routing.CodexStaticISP != nil && snapshot.Routing.CodexStaticISP.Enabled {
		if executor, ok := manager.Executor("codex"); ok {
			if source, ok := executor.(interface {
				CodexEgressManager() *helps.CodexEgressManager
			}); ok {
				if keeper := source.CodexEgressManager(); keeper != nil {
					if message := keeper.StaticISPError(); message != "" {
						c.JSON(http.StatusServiceUnavailable, gin.H{"error": "static ISP configuration is saved in memory but unavailable: " + message, "applied": true, "persisted": persisted, "runtime_only": !persisted, "runtime_error": true})
						return false
					}
				}
			}
		}
	}
	if errSave != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save account scheduling: %v", errSave), "applied": true, "persisted": false, "runtime_only": true})
		return false
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "applied": true, "persisted": persisted, "runtime_only": !persisted})
	return true
}

// noteConfigMutationLocked records the file-backed state before a management
// mutation. It is used as a fallback reload fence when SourceHash is absent.
// Callers must hold h.mu.
func (h *Handler) noteConfigMutationLocked() {
	if h == nil || h.configFilePath == "" || h.cfg == nil {
		return
	}
	data, errRead := os.ReadFile(h.configFilePath)
	if errRead != nil {
		return
	}
	snapshot, errParse := config.ParseConfigBytes(data)
	if errParse != nil {
		return
	}
	h.pendingConfigFileHash = managementConfigHash(snapshot)
}

func (h *Handler) recordConfigFileSnapshotLocked() {
	if h.pendingConfigFileHash == "" {
		return
	}
	h.staleConfigFileHashes = append(h.staleConfigFileHashes, h.pendingConfigFileHash)
	h.pendingConfigFileHash = ""
	const maxTrackedSnapshots = 256
	if len(h.staleConfigFileHashes) > maxTrackedSnapshots {
		h.staleConfigFileHashes = append([]string(nil), h.staleConfigFileHashes[len(h.staleConfigFileHashes)-maxTrackedSnapshots:]...)
	}
}

func managementConfigHash(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	data, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		return ""
	}
	return config.ComputeSourceHash(data)
}

// reloadSnapshotConfigLocked clones the runtime config and assigns a reload generation.
// Callers must hold h.mu.
func (h *Handler) reloadSnapshotConfigLocked() configReloadSnapshot {
	if h == nil || h.cfg == nil {
		return configReloadSnapshot{}
	}
	h.reloadGeneration++
	return configReloadSnapshot{
		cfg:        h.cfg.CloneForRuntime(),
		generation: h.reloadGeneration,
	}
}

// saveConfigAndSnapshotLocked saves h.cfg and returns a full runtime config snapshot.
// Callers must hold h.mu.
func (h *Handler) saveConfigAndSnapshotLocked(c *gin.Context) (configReloadSnapshot, bool) {
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, c.GetBool(ConfigV8ContextKey)); errSave != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", errSave)})
		return configReloadSnapshot{}, false
	}
	return h.reloadSnapshotConfigLocked(), true
}

// reloadConfigAfterManagementSave reloads from an independent config snapshot.
// Callers must pass a full Config clone captured immediately after a successful save.
func (h *Handler) reloadConfigAfterManagementSave(ctx context.Context, snapshot configReloadSnapshot) {
	if h == nil || snapshot.cfg == nil || snapshot.generation == 0 {
		return
	}
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()

	h.mu.Lock()
	if snapshot.generation < h.appliedReloadGeneration {
		h.mu.Unlock()
		return
	}
	hook := h.configReloadHook
	host := h.pluginHost
	h.mu.Unlock()
	if hook != nil {
		hook(ctx, snapshot.cfg)
	} else if host != nil {
		host.ApplyConfig(ctx, snapshot.cfg)
	}

	h.mu.Lock()
	if snapshot.generation > h.appliedReloadGeneration {
		h.appliedReloadGeneration = snapshot.generation
	}
	h.mu.Unlock()
}

// reloadConfigAfterManagementSaveAsync reloads from an independent config snapshot.
// Callers must pass a full Config clone captured immediately after a successful save.
func (h *Handler) reloadConfigAfterManagementSaveAsync(ctx context.Context, snapshot configReloadSnapshot) {
	if h == nil || snapshot.cfg == nil || snapshot.generation == 0 {
		return
	}
	reloadCtx := context.Background()
	if ctx != nil {
		reloadCtx = context.WithoutCancel(ctx)
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.WithField("panic", recovered).Error("management: async config reload panicked")
			}
		}()
		h.reloadConfigAfterManagementSave(reloadCtx, snapshot)
	}()
}

// SetLocalPassword configures the runtime-local password accepted for localhost requests.
func (h *Handler) SetLocalPassword(password string) { h.localPassword = password }

// SetLogDirectory updates the directory where main.log should be looked up.
func (h *Handler) SetLogDirectory(dir string) {
	if dir == "" {
		return
	}
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	h.logDir = dir
}

// SetPostAuthHook registers a hook to be called after auth record creation but before persistence.
func (h *Handler) SetPostAuthHook(hook coreauth.PostAuthHook) {
	h.postAuthHook = hook
}

// SetPostAuthPersistHook registers a hook to be called after auth persistence.
func (h *Handler) SetPostAuthPersistHook(hook coreauth.PostAuthHook) {
	h.postAuthPersistHook = hook
}

// Middleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key.
// Additionally, remote access requires allow-remote-management=true.
func (h *Handler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-CPA-VERSION", buildinfo.Version)
		c.Header("X-CPA-COMMIT", buildinfo.Commit)
		c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)
		c.Header("X-CPA-SUPPORT-PLUGIN", pluginhost.SupportPluginHeaderValue())

		clientIP := c.ClientIP()
		localClient := clientIP == "127.0.0.1" || clientIP == "::1"

		// Accept either Authorization: Bearer <key> or X-Management-Key
		var provided string
		if ah := c.GetHeader("Authorization"); ah != "" {
			parts := strings.SplitN(ah, " ", 2)
			if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
				provided = parts[1]
			} else {
				provided = ah
			}
		}
		if provided == "" {
			provided = c.GetHeader("X-Management-Key")
		}

		allowed, statusCode, errMsg := h.AuthenticateManagementKey(clientIP, localClient, provided)
		if !allowed {
			c.AbortWithStatusJSON(statusCode, gin.H{"error": errMsg})
			return
		}
		c.Next()
	}
}

// AuthenticateManagementKey verifies the provided management key for the given client.
// It mirrors the behaviour of Middleware() so non-HTTP callers can reuse the same logic.
func (h *Handler) AuthenticateManagementKey(clientIP string, localClient bool, provided string) (bool, int, string) {
	const maxFailures = 5
	const banDuration = 30 * time.Minute

	if h == nil {
		return false, http.StatusForbidden, "remote management disabled"
	}

	h.mu.Lock()
	cfg := h.cfg
	var (
		allowRemote bool
		secretHash  string
	)
	if cfg != nil {
		allowRemote = cfg.RemoteManagement.AllowRemote
		secretHash = cfg.RemoteManagement.SecretKey
	}
	h.mu.Unlock()
	if h.allowRemoteOverride {
		allowRemote = true
	}
	envSecret := h.envSecret

	now := time.Now()
	h.attemptsMu.Lock()
	ai := h.failedAttempts[clientIP]
	if ai != nil && !ai.blockedUntil.IsZero() {
		if now.Before(ai.blockedUntil) {
			remaining := ai.blockedUntil.Sub(now).Round(time.Second)
			h.attemptsMu.Unlock()
			return false, http.StatusForbidden, fmt.Sprintf("IP banned due to too many failed attempts. Try again in %s", remaining)
		}
		// Ban expired, reset state
		ai.blockedUntil = time.Time{}
		ai.count = 0
	}
	h.attemptsMu.Unlock()

	if !localClient && !allowRemote {
		return false, http.StatusForbidden, "remote management disabled"
	}

	fail := func() {
		h.attemptsMu.Lock()
		aip := h.failedAttempts[clientIP]
		if aip == nil {
			aip = &attemptInfo{}
			h.failedAttempts[clientIP] = aip
		}
		aip.count++
		aip.lastActivity = time.Now()
		if aip.count >= maxFailures {
			aip.blockedUntil = time.Now().Add(banDuration)
			aip.count = 0
		}
		h.attemptsMu.Unlock()
	}

	reset := func() {
		h.attemptsMu.Lock()
		if ai := h.failedAttempts[clientIP]; ai != nil {
			ai.count = 0
			ai.blockedUntil = time.Time{}
		}
		h.attemptsMu.Unlock()
	}

	if secretHash == "" && envSecret == "" {
		return false, http.StatusForbidden, "remote management key not set"
	}

	if provided == "" {
		fail()
		return false, http.StatusUnauthorized, "missing management key"
	}

	if localClient {
		if lp := h.localPassword; lp != "" {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
				reset()
				return true, 0, ""
			}
		}
	}

	if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
		reset()
		return true, 0, ""
	}

	if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
		fail()
		return false, http.StatusUnauthorized, "invalid management key"
	}

	reset()

	return true, 0, ""
}

// persistLocked saves the current in-memory config to disk.
// It expects the caller to hold h.mu.
func (h *Handler) persistLocked(c *gin.Context) bool {
	// Preserve comments when writing
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, c.GetBool(ConfigV8ContextKey)); err != nil {
		h.pendingConfigFileHash = ""
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
		return false
	}
	h.recordConfigFileSnapshotLocked()
	return h.finishPersistLocked(c)
}

// finishPersistLocked publishes the post-save snapshot and response. Callers
// must hold h.mu and must have already persisted h.cfg successfully.
func (h *Handler) finishPersistLocked(c *gin.Context) bool {
	snapshot := h.reloadSnapshotConfigLocked()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	var reqCtx context.Context
	if c != nil && c.Request != nil {
		reqCtx = c.Request.Context()
	}
	h.reloadConfigAfterManagementSaveAsync(reqCtx, snapshot)
	return true
}

// Helper methods for simple types. The setter and the save run under h.mu, so a
// reload publishing h.cfg cannot replace it between the change and the save.
func (h *Handler) updateBoolField(c *gin.Context, set func(bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noteConfigMutationLocked()
	set(*body.Value)
	h.persistLocked(c)
}

func (h *Handler) updateIntField(c *gin.Context, set func(int)) {
	h.updateIntFieldAt(c, set, nil)
}

func (h *Handler) updateIntFieldAt(c *gin.Context, set func(int), legacyPath []string) {
	var body struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noteConfigMutationLocked()
	set(*body.Value)
	if len(legacyPath) > 0 {
		h.persistLegacyScalarLocked(c, legacyPath, strconv.Itoa(*body.Value))
		return
	}
	h.persistLocked(c)
}

func (h *Handler) updateStringField(c *gin.Context, set func(string)) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noteConfigMutationLocked()
	set(*body.Value)
	h.persistLocked(c)
}

// persistLegacyScalarLocked updates a legacy v0 scalar in place. The generic
// typed saver is intentionally retained for v8 documents and for callers that
// do not identify a scalar path.
func (h *Handler) persistLegacyScalarLocked(c *gin.Context, path []string, value string) bool {
	data, errRead := os.ReadFile(h.configFilePath)
	if errRead != nil {
		return h.persistLocked(c)
	}
	var doc yaml.Node
	if errUnmarshal := yaml.Unmarshal(data, &doc); errUnmarshal != nil || len(doc.Content) == 0 || isV8ManagementConfigRoot(doc.Content[0]) {
		return h.persistLocked(c)
	}
	updated, errUpdate := updateLegacyTopLevelScalar(data, doc.Content[0], path, value)
	if errUpdate != nil {
		return h.persistLocked(c)
	}
	f, errOpen := os.OpenFile(h.configFilePath, os.O_WRONLY|os.O_TRUNC, 0)
	if errOpen != nil {
		h.pendingConfigFileHash = ""
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", errOpen)})
		return false
	}
	_, errWrite := f.Write(updated)
	if errWrite == nil {
		errWrite = f.Sync()
	}
	if errClose := f.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite != nil {
		h.pendingConfigFileHash = ""
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", errWrite)})
		return false
	}
	h.recordConfigFileSnapshotLocked()
	return h.finishPersistLocked(c)
}

func isV8ManagementConfigRoot(root *yaml.Node) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "config-version", "upstream", "oauth", "server", "credentials", "observability", "management", "providers":
			return true
		case "api-keys":
			if root.Content[i+1].Kind == yaml.MappingNode {
				return true
			}
		}
	}
	return false
}

func updateLegacyTopLevelScalar(data []byte, root *yaml.Node, path []string, value string) ([]byte, error) {
	if root == nil || root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 || len(path) != 1 {
		return nil, fmt.Errorf("unsupported legacy scalar path")
	}
	var target *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == path[0] {
			target = root.Content[i+1]
			break
		}
	}
	if target == nil || target.Kind != yaml.ScalarNode || target.Line < 1 {
		return nil, fmt.Errorf("legacy scalar path not found")
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	key := append([]byte(nil), path[0]...)
	lineIndex := target.Line - 1
	if lineIndex >= len(lines) {
		return nil, fmt.Errorf("legacy scalar line out of range")
	}
	line := lines[lineIndex]
	content := bytes.TrimSuffix(line, []byte("\n"))
	content = bytes.TrimSuffix(content, []byte("\r"))
	if len(content) == 0 || len(content) != len(bytes.TrimLeft(content, " \t")) {
		return nil, fmt.Errorf("legacy scalar path not found")
	}
	colon := bytes.IndexByte(content, ':')
	if colon < 0 || !bytes.Equal(bytes.TrimSpace(content[:colon]), key) {
		return nil, fmt.Errorf("legacy scalar path not found")
	}
	start := colon + 1
	for start < len(content) && (content[start] == ' ' || content[start] == '\t') {
		start++
	}
	end := start
	for end < len(content) && content[end] != ' ' && content[end] != '\t' && content[end] != '#' {
		end++
	}
	if end == start {
		return nil, fmt.Errorf("legacy scalar token is empty")
	}
	updatedLine := make([]byte, 0, len(content)-end+start+len(value)+len(line)-len(content))
	updatedLine = append(updatedLine, content[:start]...)
	updatedLine = append(updatedLine, value...)
	updatedLine = append(updatedLine, content[end:]...)
	updatedLine = append(updatedLine, line[len(content):]...)
	lines[lineIndex] = updatedLine
	return bytes.Join(lines, nil), nil
}
