package config

import (
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	sdkpluginstore "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	"gopkg.in/yaml.v3"
)

// RequestScopedErrorRule configures custom classification and handling for upstream errors.
type RequestScopedErrorRule struct {
	// Status matches the HTTP status code of the upstream response (e.g. 400).
	Status int `yaml:"status,omitempty" json:"status,omitempty"`
	// Match matches substrings in the upstream error body.
	Match []string `yaml:"match,omitempty" json:"match,omitempty"`
	// MatchRegexr matches regular expressions in the upstream error body.
	MatchRegexr []string `yaml:"match-regexr,omitempty" json:"match-regexr,omitempty"`
	// Action specifies the handling behavior: "stop", "stop-and-cooldown", "continue", "continue-and-cooldown".
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
}

// PluginsConfig holds dynamic plugin system settings.
type PluginsConfig struct {
	// Enabled toggles dynamic plugin loading.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Dir is the plugin discovery directory.
	Dir string `yaml:"dir" json:"dir"`
	// StoreSources appends third-party plugin store registries to the built-in official source.
	StoreSources []string `yaml:"store-sources,omitempty" json:"store-sources,omitempty"`
	// StoreAuth defines optional auth rules for plugin store registry, metadata, and artifact requests.
	StoreAuth []sdkpluginstore.AuthConfig `yaml:"store-auth,omitempty" json:"store-auth,omitempty"`
	// AuthRevision changes when Home-managed plugin credentials change.
	AuthRevision int64 `yaml:"auth-revision,omitempty" json:"auth-revision,omitempty"`
	// Configs stores per-plugin instance configuration by plugin ID.
	Configs map[string]PluginInstanceConfig `yaml:"configs" json:"configs"`
}

// PluginInstanceConfig stores host-owned plugin settings and the original plugin YAML subtree.
type PluginInstanceConfig struct {
	// Enabled toggles this plugin instance. Nil is normalized to false during YAML parsing.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Priority controls plugin startup and routing order.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Raw preserves the full original plugin configuration YAML subtree.
	Raw yaml.Node `yaml:"-" json:"-"`
}

// UnmarshalYAML extracts host-owned fields while preserving the full original YAML node.
func (c *PluginInstanceConfig) UnmarshalYAML(value *yaml.Node) error {
	if c == nil {
		return nil
	}

	c.Priority = 0
	defaultEnabled := false
	c.Enabled = &defaultEnabled

	if value == nil || value.Kind == 0 {
		c.Raw = *defaultPluginInstanceConfigNode()
		return nil
	}

	c.Raw = *deepCopyNode(value)
	if value.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i]
		node := value.Content[i+1]
		if key == nil {
			continue
		}
		switch key.Value {
		case "enabled":
			var enabled bool
			if errDecodeEnabled := node.Decode(&enabled); errDecodeEnabled != nil {
				return fmt.Errorf("parse plugin enabled: %w", errDecodeEnabled)
			}
			c.Enabled = &enabled
		case "priority":
			var priority int
			if errDecodePriority := node.Decode(&priority); errDecodePriority != nil {
				return fmt.Errorf("parse plugin priority: %w", errDecodePriority)
			}
			c.Priority = priority
		}
	}

	return nil
}

// MarshalYAML returns the preserved raw plugin YAML subtree for lossless config output.
func (c PluginInstanceConfig) MarshalYAML() (any, error) {
	if c.Raw.Kind == 0 {
		return defaultPluginInstanceConfigNode(), nil
	}
	return deepCopyNode(&c.Raw), nil
}

func defaultPluginInstanceConfigNode() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: []*yaml.Node{},
	}
}

// ClaudeConfig configures provider-wide Claude request behavior.
type ClaudeConfig struct {
	// ModelLevelCooling scopes Claude quota cooldowns to the requested model
	// rather than cooling down the entire credential across all sibling models.
	ModelLevelCooling bool `yaml:"model-level-cooling" json:"model-level-cooling"`
	// ToolSearch enables Codex client tool_search on Claude Responses routes:
	// Anthropic native tool search where the credential supports it, a pre-tool_search
	// translation elsewhere, and per-credential search advertising in the Codex
	// catalog. Default false keeps the translation that predates tool_search support.
	ToolSearch bool `yaml:"tool-search" json:"tool-search"`
	// SessionDatePinDays retains the cloaked currentDate pin per session for
	// this many days, independently of continuity expiry and credential
	// switches. A nil value (unset) defaults to 7 days; an explicit 0 keeps
	// the continuity-scoped pin (1h lifetime).
	SessionDatePinDays *int `yaml:"session-date-pin-days" json:"session-date-pin-days"`
}

// DefaultClaudeSessionDatePinDays is the session date pin retention applied
// when session-date-pin-days is not configured.
const DefaultClaudeSessionDatePinDays = 7

// SessionDatePinRetention returns the configured session date pin retention.
// An unset value defaults to 7 days; an explicit zero keeps the legacy
// continuity-scoped behavior. Days are clamped so the duration cannot
// overflow.
func (c ClaudeConfig) SessionDatePinRetention() time.Duration {
	days := DefaultClaudeSessionDatePinDays
	if c.SessionDatePinDays != nil {
		days = *c.SessionDatePinDays
	}
	if days <= 0 {
		return 0
	}
	maxDays := int((time.Duration(math.MaxInt64) / (24 * time.Hour)))
	if days > maxDays {
		days = maxDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// ClaudeHeaderDefaults configures the measured Claude Code software baseline.
// Verified native requests preserve their entrypoint and software shape only when their
// Claude Code, package, and runtime versions exactly match this baseline; unmeasured
// versions use the configured values. Timeout remains a fallback. Stabilized profiles
// also pin OS and Arch and never learn newer software versions automatically.
// NativeEntrypoints extends the built-in native Claude Code entrypoint allowlist.
// AnthropicBetaStrip and AnthropicBetaAppend are exact-name operator overrides applied
// to the final Anthropic-Beta set; InjectDiagnostics and ClientRequestID tune the
// first-party Claude Code CLI request shape.
type ClaudeHeaderDefaults struct {
	UserAgent              string   `yaml:"user-agent" json:"user-agent"`
	PackageVersion         string   `yaml:"package-version" json:"package-version"`
	RuntimeVersion         string   `yaml:"runtime-version" json:"runtime-version"`
	OS                     string   `yaml:"os" json:"os"`
	Arch                   string   `yaml:"arch" json:"arch"`
	Timeout                string   `yaml:"timeout" json:"timeout"`
	Timezone               string   `yaml:"timezone" json:"timezone"`
	StabilizeDeviceProfile *bool    `yaml:"stabilize-device-profile,omitempty" json:"stabilize-device-profile,omitempty"`
	NativeEntrypoints      []string `yaml:"native-entrypoints,omitempty" json:"native-entrypoints,omitempty"`
	AnthropicBetaStrip     []string `yaml:"anthropic-beta-strip,omitempty" json:"anthropic-beta-strip,omitempty"`
	AnthropicBetaAppend    []string `yaml:"anthropic-beta-append,omitempty" json:"anthropic-beta-append,omitempty"`
	InjectDiagnostics      *bool    `yaml:"inject-diagnostics,omitempty" json:"inject-diagnostics,omitempty"`
	ClientRequestID        string   `yaml:"client-request-id,omitempty" json:"client-request-id,omitempty"`
}

// Supported ClaudeHeaderDefaults.ClientRequestID values. An empty value means
// ClaudeClientRequestIDOn.
const (
	ClaudeClientRequestIDOn          = "on"
	ClaudeClientRequestIDOff         = "off"
	ClaudeClientRequestIDPassthrough = "passthrough"
)

// CodexHeaderDefaults configures fallback header values injected into Codex
// model requests for OAuth/file-backed auth when the client omits them.
// UserAgent applies to HTTP and websocket requests; BetaFeatures only applies to websockets.
type CodexHeaderDefaults struct {
	UserAgent    string `yaml:"user-agent" json:"user-agent"`
	BetaFeatures string `yaml:"beta-features" json:"beta-features"`
}

// XAIConfig configures provider-wide xAI request behavior.
type XAIConfig struct {
	// InjectXSearch injects xAI's native x_search tool when the request does not declare it.
	InjectXSearch bool `yaml:"inject-x-search" json:"inject-x-search"`
}

// DevinConfig configures provider-wide Devin request behavior.
type DevinConfig struct {
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`
}

// AntigravityConfig configures provider-wide Antigravity request behavior.
type AntigravityConfig struct {
	// SensitiveWords is a list of words to obfuscate with zero-width characters in system instructions.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// ConnectionPool configures upstream HTTP connection pooling behavior for Antigravity.
	ConnectionPool AntigravityConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`
}

// AntigravityConnectionPoolConfig controls upstream HTTP/1.1 connection pooling behavior for Antigravity.
type AntigravityConnectionPoolConfig struct {
	// Enabled controls whether upstream connection pooling is enabled.
	// Defaults to false (short-lived connection mode). Set to true to enable connection pooling.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// IdleConnTimeout specifies how long an idle connection stays in the pool before expiring.
	// Defaults to "30s". Capped at 210s to prevent exceeding Google Frontend (GFE) 240s cutoff.
	IdleConnTimeout string `yaml:"idle-conn-timeout,omitempty" json:"idle-conn-timeout,omitempty"`

	// MaxIdleConnsPerHost specifies the maximum number of idle connections to retain per host per credential.
	// Defaults to 2.
	MaxIdleConnsPerHost *int `yaml:"max-idle-conns-per-host,omitempty" json:"max-idle-conns-per-host,omitempty"`
}

// ResponseCompressionConfig controls downstream HTTP response compression.
type ResponseCompressionConfig struct {
	// Enabled defaults to true when unset.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MinBytes defaults to 1024 when zero or negative.
	MinBytes int `yaml:"min-bytes" json:"min-bytes"`
}

// IsEnabled reports whether negotiated response compression is enabled.
func (c ResponseCompressionConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// EffectiveMinBytes returns the minimum non-streaming response size to compress.
func (c ResponseCompressionConfig) EffectiveMinBytes() int {
	if c.MinBytes <= 0 {
		return 1024
	}
	return c.MinBytes
}

// CodexConfig configures provider-wide Codex request behavior.
type CodexConfig struct {
	IdentityConfuse bool `yaml:"identity-confuse" json:"identity-confuse"`
	// QuotaRefresh keeps local account quota weights current without manual probes.
	QuotaRefresh CodexQuotaRefreshConfig `yaml:"quota-refresh,omitempty" json:"quota-refresh"`
	// DisableCodexCloaking disables forcing the official Codex identity headers on HTTP/SSE and WebSocket requests.
	DisableCodexCloaking bool `yaml:"disable-codex-cloaking" json:"disable-codex-cloaking"`
	// StreamBootstrapBuffering holds back the frames that arrive before generation starts, none of
	// which the client has seen anything from - the handshake (response.created, response.in_progress,
	// the websocket metadata frames), keepalive heartbeats, and the *.added announcements of an item
	// or part that is still empty - until the first generated event arrives. The upstream delivers
	// server_is_overloaded rejections inside an HTTP 200 stream right after those frames instead of
	// returning 503 on the wire, so buffering them keeps the downstream response headers uncommitted
	// long enough to retry on another credential. Trade-off: the response headers are delayed until
	// the upstream starts generating, which on a slow reasoning turn now means several heartbeat
	// intervals rather than one, and can trip client or reverse-proxy read timeouts. The hold is
	// bounded by a frame and a byte budget, not by wall-clock time, and neither budget is advanced
	// by a websocket peer that sends only control frames or by an upstream that never terminates an
	// SSE line. Only overload and rate-limit rejections fail over deliberately. A stream that ends
	// while the bootstrap is still holding ends the attempt rather than reaching the client, and
	// what follows is pre-existing but now far more likely, since the hold can span the whole
	// reasoning phase instead of ending at the first keepalive: a clean end with no terminal event
	// is request-scoped on SSE and stops there, while a websocket close or a transport error on
	// either transport is not, so the request may be retried on another credential.
	// Default is false.
	StreamBootstrapBuffering bool `yaml:"stream-bootstrap-buffering" json:"stream-bootstrap-buffering"`
	// StreamBootstrapTimeout specifies an optional maximum duration to hold back uncommitted response
	// headers during bootstrap buffering before releasing the stream to the client.
	// Defaults to "0" (unlimited time, relying purely on the 48-frame and 1MB byte bounds).
	// When set (e.g. "20s"), the stream is released once the time ceiling is reached, avoiding
	// reverse-proxy timeouts (e.g. Nginx 60s proxy_read_timeout).
	StreamBootstrapTimeout string `yaml:"stream-bootstrap-timeout,omitempty" json:"stream-bootstrap-timeout,omitempty"`
	// OrphanDelegationCompatibility enables opt-in compatibility for orphan Codex delegation outputs.
	OrphanDelegationCompatibility bool `yaml:"orphan-delegation-compatibility" json:"orphan-delegation-compatibility"`
	// ModelLevelCooling scopes Codex usage_limit_reached quota cooldowns to the requested model
	// rather than cooling down the entire credential across all sibling models.
	ModelLevelCooling bool `yaml:"model-level-cooling" json:"model-level-cooling"`
	// LiveMediaRelay terminates and relays Codex Live WebRTC media in this process.
	LiveMediaRelay CodexLiveMediaRelayConfig `yaml:"live-media-relay" json:"live-media-relay"`
	// ResponseSteering enables full-duplex Codex WebSocket steering for OAuth credentials.
	ResponseSteering bool `yaml:"response-steering" json:"response-steering"`
}

// CodexQuotaRefreshConfig controls periodic usage-window observations, not token refresh.
type CodexQuotaRefreshConfig struct {
	Enabled     *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Interval    string `yaml:"interval,omitempty" json:"interval,omitempty"`
	Concurrency int    `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
}

func (c CodexQuotaRefreshConfig) Effective() (bool, time.Duration, int) {
	enabled := c.Enabled == nil || *c.Enabled
	interval := 5 * time.Minute
	if value := strings.TrimSpace(c.Interval); value != "" {
		interval, _ = time.ParseDuration(value)
	}
	concurrency := c.Concurrency
	if concurrency == 0 {
		concurrency = 2
	}
	return enabled, interval, concurrency
}

func (c CodexQuotaRefreshConfig) Validate() error {
	if value := strings.TrimSpace(c.Interval); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil || interval <= 0 {
			return fmt.Errorf("codex.quota-refresh.interval must be a positive duration")
		}
	}
	if c.Concurrency < 0 {
		return fmt.Errorf("codex.quota-refresh.concurrency must not be negative")
	}
	return nil
}

// CodexLiveMediaRelayConfig configures the in-process Codex Live WebRTC gateway.
type CodexLiveMediaRelayConfig struct {
	Enabled                 bool                 `yaml:"enabled" json:"enabled"`
	MaxSessions             int                  `yaml:"max-sessions" json:"max-sessions"`
	DisablePrivateRemoteIPs bool                 `yaml:"disable-private-remote-ips" json:"disable-private-remote-ips"`
	PublicIP                string               `yaml:"public-ip" json:"public-ip"`
	UDPPortMin              uint16               `yaml:"udp-port-min" json:"udp-port-min"`
	UDPPortMax              uint16               `yaml:"udp-port-max" json:"udp-port-max"`
	ICEServers              []CodexLiveICEServer `yaml:"ice-servers" json:"ice-servers"`
}

// CodexLiveICEServer configures a STUN or TURN server for the media relay.
type CodexLiveICEServer struct {
	URLs       []string `yaml:"urls" json:"urls"`
	Username   string   `yaml:"username" json:"-"`
	Credential string   `yaml:"credential" json:"-"`
}

// TLSConfig holds HTTPS server settings.
type TLSConfig struct {
	// Enable toggles HTTPS server mode.
	Enable bool `yaml:"enable" json:"enable"`
	// Cert is the path to the TLS certificate file.
	Cert string `yaml:"cert" json:"cert"`
	// Key is the path to the TLS private key file.
	Key string `yaml:"key" json:"key"`
}

// PprofConfig holds pprof HTTP server settings.
type PprofConfig struct {
	// Enable toggles the pprof HTTP debug server.
	Enable bool `yaml:"enable" json:"enable"`
	// Addr is the host:port address for the pprof HTTP server.
	Addr string `yaml:"addr" json:"addr"`
}

// DiscoveryInterfacesConfig specifies interface inclusion and exclusion rules.
type DiscoveryInterfacesConfig struct {
	Include []string `yaml:"include" json:"include"`
	Exclude []string `yaml:"exclude" json:"exclude"`
}

// DiscoveryConfig controls local network mDNS / DNS-SD service advertising.
type DiscoveryConfig struct {
	// Enabled toggles mDNS service advertising on the local network (default: false).
	Enabled bool `yaml:"enabled" json:"enabled"`

	// ServiceName is the optional custom instance name. When empty, defaults to CPA-<ShortID>.
	ServiceName string `yaml:"service-name" json:"service-name"`

	// ServiceType is the DNS-SD service type (default: _ai-gateway._tcp).
	ServiceType string `yaml:"service-type" json:"service-type"`

	// Subtypes specifies DNS-SD API protocol subtypes to advertise (e.g. _responses, _messages, _generate-content).
	Subtypes []string `yaml:"subtypes" json:"subtypes"`

	// Interfaces specifies network interface filtering rules.
	Interfaces DiscoveryInterfacesConfig `yaml:"interfaces" json:"interfaces"`

	// AuthRequired indicates whether authentication is required for client calls (default: true).
	AuthRequired *bool `yaml:"auth-required" json:"auth-required"`

	// AdvertiseManagement is deprecated and ignored. Management endpoints are never
	// advertised over DNS-SD; the field remains so existing config files still load.
	AdvertiseManagement bool `yaml:"advertise-management" json:"advertise-management"`
}

// RemoteManagement holds management API configuration under 'remote-management'.
type RemoteManagement struct {
	// AllowRemote toggles remote (non-localhost) access to management API.
	AllowRemote bool `yaml:"allow-remote"`
	// SecretKey is the management key (plaintext or bcrypt hashed). YAML key intentionally 'secret-key'.
	SecretKey string `yaml:"secret-key"`
	// DisableControlPanel skips serving and syncing the bundled management UI when true.
	DisableControlPanel bool `yaml:"disable-control-panel"`
	// DisableAutoUpdatePanel disables automatic periodic background updates of the management panel asset from GitHub.
	// When false (the default), the background updater remains enabled; when true, the panel is only downloaded on first access if missing.
	DisableAutoUpdatePanel bool `yaml:"disable-auto-update-panel"`
	// PanelGitHubRepository overrides the GitHub repository used to fetch the management panel asset.
	// Accepts either a repository URL (https://github.com/org/repo) or an API releases endpoint.
	PanelGitHubRepository string `yaml:"panel-github-repository"`
	// BaseURL specifies the base URL of the remote management API for TUI client mode (e.g. "https://proxy.example.com").
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`
}

// QuotaExceeded defines the behavior when API quota limits are exceeded.
// It provides configuration options for automatic failover mechanisms.
type QuotaExceeded struct {
	// SwitchProject indicates whether to automatically switch to another project when a quota is exceeded.
	SwitchProject bool `yaml:"switch-project" json:"switch-project"`

	// SwitchPreviewModel indicates whether to automatically switch to a preview model when a quota is exceeded.
	SwitchPreviewModel bool `yaml:"switch-preview-model" json:"switch-preview-model"`

	// AntigravityCredits enables credits-based last-resort fallback for Claude models.
	// When all free-tier auths are exhausted (429/503), the conductor retries with
	// an auth that has available Google One AI credits.
	AntigravityCredits bool `yaml:"antigravity-credits" json:"antigravity-credits"`
}

// RoutingConfig configures how credentials are selected for requests.
type RoutingConfig struct {
	// PersistSettings opts management-panel changes into saving an independent snapshot.
	PersistSettings *bool `yaml:"-" json:"persist-settings,omitempty"`
	// Strategy selects the credential selection strategy.
	// Supported values: "round-robin" (default), "weighted-round-robin", "fill-first", "remaining-quota".
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`

	// SchedulerMode is a runtime-only override used by the management panel.
	// It is saved only through the optional AccountScheduling snapshot.
	SchedulerMode string `yaml:"-" json:"scheduler-mode,omitempty"`

	// DegradedPort is a runtime-only model API listener for degraded accounts; zero disables it.
	DegradedPort *int `yaml:"-" json:"degraded-port,omitempty"`
	// ExoFreePort exposes OpenCode Free models on a separate listener; the config key is kept for compatibility.
	ExoFreePort *int `yaml:"-" json:"exo-free-port,omitempty"`

	// AutoDisableUnauthorized excludes credentials after an upstream 401 once
	// request-scoped errors and OAuth refresh recovery have been ruled out.
	AutoDisableUnauthorized bool `yaml:"-" json:"auto-disable-unauthorized"`
	// AutoDisableQuota excludes credentials after an explicit quota exhaustion
	// response or a provider quota watermark reaches 100 percent.
	// Codex account windows are handled separately: 5H pauses until reset; 7D disables.
	AutoDisableQuota bool `yaml:"-" json:"auto-disable-quota"`
	// AuthConcurrencyLimit is the runtime-only in-flight request limit for every credential.
	// Nil retains the current setting across config reloads; an explicit zero removes the limit.
	AuthConcurrencyLimit *int `yaml:"-" json:"auth-concurrency-limit,omitempty"`
	// CodexFingerprintConvergence is a runtime-only opt-in for account-scoped Codex
	// OAuth/SetupToken identity convergence. Nil retains the current setting across
	// config reloads; false explicitly restores native request identity behavior.
	CodexFingerprintConvergence *bool `yaml:"-" json:"codex-fingerprint-convergence,omitempty"`
	// WorkBuddyFingerprintConvergence pins WorkBuddy conversation identity per account.
	// Nil inherits across reloads; false restores caller-owned conversation headers.
	WorkBuddyFingerprintConvergence *bool `yaml:"-" json:"workbuddy-fingerprint-convergence,omitempty"`
	// CodexStaticISP pins inference to a fixed per-account proxy.
	CodexStaticISP *CodexStaticISPConfig `yaml:"-" json:"codex-static-isp,omitempty"`
	// CodexRoutingHint is a runtime-only upstream routing hint for Codex OAuth requests.
	// An empty value disables the hint; omitted values use the default "europe" hint.
	CodexRoutingHint *string `yaml:"-" json:"codex-routing-hint,omitempty"`
	// DisableCooling is a runtime-only force override for all account and model cooldowns.
	// A true value makes every registered account behave as if disable_cooling were true.
	DisableCooling *bool `yaml:"-" json:"disable-cooling,omitempty"`

	// C2AMaxRetryCount overrides the global request-retry count for the Codex2API
	// default scheduler. It is runtime-only; nil inherits the global setting.
	C2AMaxRetryCount *int `yaml:"-" json:"c2a-max-retry-count,omitempty"`
	// C2AMaxRetryInterval overrides the global max-retry-interval in seconds for
	// the Codex2API default scheduler. It is runtime-only; nil inherits globally.
	C2AMaxRetryInterval *int `yaml:"-" json:"c2a-max-retry-interval,omitempty"`
	// C2AMaxRetryCredentials overrides the global per-round credential limit for
	// the Codex2API default scheduler. Zero keeps the all-credentials behavior.
	C2AMaxRetryCredentials *int `yaml:"-" json:"c2a-max-retry-credentials,omitempty"`

	// SessionAffinity enables universal session-sticky routing for all clients.
	// Explicit Claude Code, Codex, OpenCode, and pi session headers are preferred,
	// followed by prompt_cache_key, Responses conversation IDs, legacy body IDs,
	// execution or derived session identity, and the existing message-content hash fallback.
	// Automatic failover is always enabled when bound auth becomes unavailable.
	SessionAffinity bool `yaml:"session-affinity,omitempty" json:"session-affinity,omitempty"`

	// SessionAffinityTTL specifies how long session-to-auth bindings are retained.
	// Default: 1h. Accepts duration strings like "30m", "1h", "2h30m".
	SessionAffinityTTL string `yaml:"session-affinity-ttl,omitempty" json:"session-affinity-ttl,omitempty"`

	// SessionAffinitySubagents controls whether subagents (child sessions with parent references)
	// inherit and bind to the parent's upstream credential across all providers (Claude, Codex,
	// Antigravity, Gemini), maximizing prompt and KV cache reuse.
	// When false, subagents are distributed across the credential pool via the fallback selector.
	// Default: true. Ignored when SessionAffinity is false.
	SessionAffinitySubagents *bool `yaml:"session-affinity-subagents,omitempty" json:"session-affinity-subagents,omitempty"`
	// FakeModels are runtime-only fixed responses owned by the account scheduling panel.
	FakeModels []FakeModelConfig `yaml:"-" json:"fake-models,omitempty"`
}

// FakeModelConfig defines one synthetic OpenAI-compatible model and its reported usage.
type FakeModelConfig struct {
	Name             string   `yaml:"name" json:"name"`
	Content          string   `yaml:"content,omitempty" json:"content,omitempty"`
	Responses        []string `yaml:"responses,omitempty" json:"responses,omitempty"`
	InputTokens      int      `yaml:"input-tokens" json:"input-tokens"`
	OutputTokens     int      `yaml:"output-tokens" json:"output-tokens"`
	CacheReadTokens  int      `yaml:"cache-read-tokens" json:"cache-read-tokens"`
	CacheWriteTokens int      `yaml:"cache-write-tokens" json:"cache-write-tokens"`
	CharsPerSecond   int      `yaml:"chars-per-second,omitempty" json:"chars-per-second,omitempty"`
}

func (model *FakeModelConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain FakeModelConfig
	if errDecode := node.Decode((*plain)(model)); errDecode != nil {
		return errDecode
	}
	// yaml.v3 otherwise truncates fractional numbers when decoding into int fields.
	node = expandConfigAliases(node)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "input-tokens", "output-tokens", "cache-read-tokens", "cache-write-tokens", "chars-per-second":
			if tag := value.ShortTag(); tag != "!!int" && tag != "!!null" {
				return fmt.Errorf("fake-models %s must be an integer", key)
			}
		}
	}
	return nil
}

// FixedResponses returns the configured response pool, including the legacy single content field.
func (model FakeModelConfig) FixedResponses() []string {
	if model.Responses != nil {
		return model.Responses
	}
	if model.Content != "" {
		return []string{model.Content}
	}
	return nil
}

// CloneFakeModels keeps response pools independent across runtime snapshots.
func CloneFakeModels(models []FakeModelConfig) []FakeModelConfig {
	if models == nil {
		return nil
	}
	cloned := make([]FakeModelConfig, len(models))
	copy(cloned, models)
	for i := range cloned {
		if models[i].Responses != nil {
			cloned[i].Responses = append([]string{}, models[i].Responses...)
		}
	}
	return cloned
}

func ValidateFakeModels(models []FakeModelConfig) error {
	names := make(map[string]struct{}, len(models))
	for _, model := range models {
		responses := model.FixedResponses()
		if model.Name == "" || strings.TrimSpace(model.Name) != model.Name || len(responses) == 0 {
			return fmt.Errorf("fake-models require a model name and at least one fixed response")
		}
		for _, response := range responses {
			if strings.TrimSpace(response) == "" {
				return fmt.Errorf("fake-models fixed responses must not be empty")
			}
		}
		if _, exists := names[model.Name]; exists {
			return fmt.Errorf("duplicate fake model %q", model.Name)
		}
		names[model.Name] = struct{}{}
		if model.InputTokens < 0 || model.OutputTokens < 0 || model.CacheReadTokens < 0 || model.CacheWriteTokens < 0 {
			return fmt.Errorf("fake-models token counts must be non-negative")
		}
		const maxSafeInteger = 1<<53 - 1
		if model.CharsPerSecond < 0 || int64(model.CharsPerSecond) > maxSafeInteger {
			return fmt.Errorf("fake-models chars-per-second must be between 0 and %d", int64(maxSafeInteger))
		}
		if int64(model.InputTokens) > maxSafeInteger-int64(model.OutputTokens) || int64(model.CacheReadTokens) > maxSafeInteger || int64(model.CacheWriteTokens) > maxSafeInteger {
			return fmt.Errorf("fake-models token counts and input/output total must not exceed %d", int64(maxSafeInteger))
		}
	}
	return nil
}

// AccountSchedulingConfig contains only settings owned by the account scheduling panel.
type AccountSchedulingConfig struct {
	PersistSettings                 bool                     `yaml:"persist-settings"`
	SchedulerMode                   string                   `yaml:"scheduler-mode"`
	DegradedPort                    *int                     `yaml:"degraded-port"`
	ExoFreePort                     *int                     `yaml:"exo-free-port"`
	AutoDisableUnauthorized         bool                     `yaml:"auto-disable-unauthorized"`
	AutoDisableQuota                bool                     `yaml:"auto-disable-quota"`
	AuthConcurrencyLimit            *int                     `yaml:"auth-concurrency-limit"`
	DisableCooling                  *bool                    `yaml:"disable-cooling"`
	C2AMaxRetryCount                *int                     `yaml:"c2a-max-retry-count"`
	C2AMaxRetryInterval             *int                     `yaml:"c2a-max-retry-interval"`
	C2AMaxRetryCredentials          *int                     `yaml:"c2a-max-retry-credentials"`
	CodexFingerprintConvergence     *bool                    `yaml:"codex-fingerprint-convergence"`
	WorkBuddyFingerprintConvergence *bool                    `yaml:"workbuddy-fingerprint-convergence"`
	CodexRoutingHint                *string                  `yaml:"codex-routing-hint"`
	CodexStaticISP                  *CodexStaticISPConfig    `yaml:"codex-static-isp"`
	CacheTokenRewrite               *CacheTokenRewriteConfig `yaml:"cache-token-rewrite"`
	FakeModels                      []FakeModelConfig        `yaml:"fake-models"`
}

func (cfg *Config) AccountSchedulingSnapshot() *AccountSchedulingConfig {
	cloned := cfg.CloneForRuntime()
	routing := cloned.Routing
	return &AccountSchedulingConfig{
		PersistSettings: routing.PersistSettings != nil && *routing.PersistSettings,
		SchedulerMode:   routing.SchedulerMode, DegradedPort: routing.DegradedPort, ExoFreePort: routing.ExoFreePort,
		AutoDisableUnauthorized: routing.AutoDisableUnauthorized, AutoDisableQuota: routing.AutoDisableQuota,
		AuthConcurrencyLimit: routing.AuthConcurrencyLimit, DisableCooling: routing.DisableCooling,
		C2AMaxRetryCount: routing.C2AMaxRetryCount, C2AMaxRetryInterval: routing.C2AMaxRetryInterval, C2AMaxRetryCredentials: routing.C2AMaxRetryCredentials,
		CodexFingerprintConvergence:     routing.CodexFingerprintConvergence,
		WorkBuddyFingerprintConvergence: routing.WorkBuddyFingerprintConvergence,
		CodexRoutingHint:                routing.CodexRoutingHint,
		CodexStaticISP:                  routing.CodexStaticISP, CacheTokenRewrite: cloned.CacheTokenRewrite,
		FakeModels: routing.FakeModels,
	}
}

func (settings *AccountSchedulingConfig) apply(cfg *Config) {
	settings = cloneRuntimeValue(reflect.ValueOf(settings)).Interface().(*AccountSchedulingConfig)
	cfg.Routing.PersistSettings = &settings.PersistSettings
	cfg.Routing.SchedulerMode = settings.SchedulerMode
	cfg.Routing.DegradedPort = settings.DegradedPort
	cfg.Routing.ExoFreePort = settings.ExoFreePort
	cfg.Routing.AutoDisableUnauthorized = settings.AutoDisableUnauthorized
	cfg.Routing.AutoDisableQuota = settings.AutoDisableQuota
	cfg.Routing.AuthConcurrencyLimit = settings.AuthConcurrencyLimit
	cfg.Routing.DisableCooling = settings.DisableCooling
	cfg.Routing.C2AMaxRetryCount = settings.C2AMaxRetryCount
	cfg.Routing.C2AMaxRetryInterval = settings.C2AMaxRetryInterval
	cfg.Routing.C2AMaxRetryCredentials = settings.C2AMaxRetryCredentials
	cfg.Routing.CodexFingerprintConvergence = settings.CodexFingerprintConvergence
	cfg.Routing.WorkBuddyFingerprintConvergence = settings.WorkBuddyFingerprintConvergence
	cfg.Routing.CodexRoutingHint = settings.CodexRoutingHint
	cfg.Routing.CodexStaticISP = settings.CodexStaticISP
	cfg.CacheTokenRewrite = settings.CacheTokenRewrite
	cfg.Routing.FakeModels = settings.FakeModels
}

func (cfg *Config) restoreAccountScheduling() error {
	settings := cfg.AccountScheduling
	if settings == nil {
		return nil
	}
	for _, limit := range []struct {
		name    string
		value   *int
		maximum int
	}{
		{"degraded-port", settings.DegradedPort, 65535},
		{"exo-free-port", settings.ExoFreePort, 65535},
		{"auth-concurrency-limit", settings.AuthConcurrencyLimit, 1000000},
		{"c2a-max-retry-count", settings.C2AMaxRetryCount, 1000000},
		{"c2a-max-retry-credentials", settings.C2AMaxRetryCredentials, 1000000},
	} {
		if limit.value != nil && (*limit.value < 0 || *limit.value > limit.maximum) {
			return fmt.Errorf("account-scheduling.%s must be between 0 and %d", limit.name, limit.maximum)
		}
	}
	if settings.C2AMaxRetryInterval != nil && *settings.C2AMaxRetryInterval < 0 {
		return fmt.Errorf("account-scheduling.c2a-max-retry-interval must be non-negative")
	}
	if settings.CacheTokenRewrite != nil {
		if errValidate := settings.CacheTokenRewrite.Validate(); errValidate != nil {
			return errValidate
		}
	}
	if errValidate := ValidateFakeModels(settings.FakeModels); errValidate != nil {
		return fmt.Errorf("account-scheduling: %w", errValidate)
	}
	settings.apply(cfg)
	if cfg.Routing.CodexStaticISP != nil {
		return cfg.Routing.CodexStaticISP.Validate()
	}
	return nil
}

// CodexStaticISPConfig is an opt-in pool of fixed, credentialed proxy endpoints.
type CodexStaticISPConfig struct {
	Enabled bool            `yaml:"enabled" json:"enabled"`
	Nodes   []StaticISPNode `yaml:"nodes" json:"nodes"`
}

type StaticISPNode struct {
	ID       string `yaml:"id" json:"id"`
	Scheme   string `yaml:"scheme,omitempty" json:"scheme,omitempty"`
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port" json:"port"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"`
}

func (cfg *CodexStaticISPConfig) Validate() error {
	ids := make(map[string]bool, len(cfg.Nodes))
	for i, node := range cfg.Nodes {
		if node.ID == "" || strings.TrimSpace(node.ID) != node.ID || ids[node.ID] {
			return fmt.Errorf("static ISP node %d requires a unique, non-empty ID", i+1)
		}
		ids[node.ID] = true
		if _, errProxy := node.ProxyURL(); errProxy != nil {
			return fmt.Errorf("static ISP node %d: %w", i+1, errProxy)
		}
	}
	return nil
}

func (node StaticISPNode) ProxyURL() (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(node.Scheme))
	if scheme == "" {
		scheme = "socks5"
	}
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("scheme must be HTTP(S) or SOCKS5")
	}
	host := strings.TrimSpace(node.Host)
	if host == "" || strings.ContainsAny(host, "/@?#[]\\ \t\r\n") || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return "", fmt.Errorf("host must be an IP address or hostname without credentials, scheme or port")
	}
	if node.Port < 1 || node.Port > 65535 {
		return "", fmt.Errorf("port must be between 1 and 65535")
	}
	if strings.TrimSpace(node.Username) == "" || node.Password == "" {
		return "", fmt.Errorf("username and password are required")
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(node.Port)), User: url.UserPassword(node.Username, node.Password)}
	if _, _, errBuild := proxyutil.BuildDialer(u.String()); errBuild != nil {
		return "", fmt.Errorf("invalid proxy endpoint")
	}
	return u.String(), nil
}

// OAuthModelAlias defines a model ID alias for a specific channel.
// It maps the upstream model name (Name) to the client-visible alias (Alias).
// When Fork is true, the alias is added as an additional model in listings while
// keeping the original model ID available.
type OAuthModelAlias struct {
	// OpenAPI overrides Vertex endpoint selection for this model.
	OpenAPI *bool `yaml:"openapi,omitempty" json:"openapi,omitempty"`

	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias" json:"alias"`
	Fork  bool   `yaml:"fork,omitempty" json:"fork,omitempty"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

// OAuthModelSetting defines provider/channel model settings for OAuth credentials.
type OAuthModelSetting struct {
	Name             string `yaml:"name" json:"name"`
	Alias            string `yaml:"alias,omitempty" json:"alias,omitempty"`
	MaxContextLength int    `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`
}

// GetMaxContextLength returns the configured context-window override.
func (s OAuthModelSetting) GetMaxContextLength() int { return s.MaxContextLength }

// ResolveOAuthModelSetting finds the last matching setting, preferring an alias match.
func ResolveOAuthModelSetting(settings []OAuthModelSetting, modelID, metadataModelID, modelName string) *OAuthModelSetting {
	id := strings.ToLower(strings.TrimSpace(modelID))
	metadataID := strings.ToLower(strings.TrimSpace(metadataModelID))
	name := strings.ToLower(strings.TrimSpace(modelName))
	var aliasMatch *OAuthModelSetting
	var nameMatch *OAuthModelSetting
	for i := range settings {
		entry := &settings[i]
		entryName := strings.ToLower(strings.TrimSpace(entry.Name))
		if entryName == "" {
			continue
		}
		entryAlias := strings.ToLower(strings.TrimSpace(entry.Alias))
		if entryAlias != "" && entryAlias == id {
			aliasMatch = entry
			continue
		}
		if (entryAlias == "" || entryAlias == id) && (entryName == id || entryName == metadataID || entryName == name) {
			nameMatch = entry
		}
	}
	if aliasMatch != nil {
		return aliasMatch
	}
	return nameMatch
}

// PayloadConfig defines default and override parameter rules applied to provider payloads.
type PayloadConfig struct {
	// Default defines rules that only set parameters when they are missing in the payload.
	Default []PayloadRule `yaml:"default" json:"default"`
	// DefaultRaw defines rules that set raw JSON values only when they are missing.
	DefaultRaw []PayloadRule `yaml:"default-raw" json:"default-raw"`
	// Override defines rules that always set parameters, overwriting any existing values.
	Override []PayloadRule `yaml:"override" json:"override"`
	// OverrideRaw defines rules that always set raw JSON values, overwriting any existing values.
	OverrideRaw []PayloadRule `yaml:"override-raw" json:"override-raw"`
	// Filter defines rules that remove parameters from the payload by JSON path.
	Filter []PayloadFilterRule `yaml:"filter" json:"filter"`
}

// PayloadFilterRule describes a rule to remove specific JSON paths from matching model payloads.
type PayloadFilterRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params lists JSON paths (gjson/sjson syntax) to remove from the payload.
	Params []string `yaml:"params" json:"params"`
}

// PayloadRule describes a single rule targeting a list of models with parameter updates.
type PayloadRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params maps JSON paths (gjson/sjson syntax) to values written into the payload.
	// For *-raw rules, values are treated as raw JSON fragments (strings are used as-is).
	Params map[string]any `yaml:"params" json:"params"`
}

// PayloadModelRule ties a model name pattern to a specific translator protocol.
type PayloadModelRule struct {
	// Name is the model name or wildcard pattern (e.g., "gpt-*", "*-5", "gemini-*-pro").
	Name string `yaml:"name" json:"name"`
	// Protocol restricts the rule to a specific translator format (e.g., "gemini", "responses").
	Protocol string `yaml:"protocol" json:"protocol"`
	// Headers restricts the rule to requests whose headers match all configured wildcard patterns.
	Headers map[string]string `yaml:"headers" json:"headers"`
	// FromProtocol restricts the rule to a specific source protocol (e.g., "gemini", "responses").
	FromProtocol string `yaml:"from-protocol" json:"from-protocol"`
	// Match requires payload JSON paths to equal the configured values.
	Match []map[string]any `yaml:"match" json:"match"`
	// NotMatch requires payload JSON paths to not equal the configured values.
	NotMatch []map[string]any `yaml:"not-match" json:"not-match"`
	// Exist requires payload JSON paths to exist and not be null.
	Exist []string `yaml:"exist" json:"exist"`
	// NotExist requires payload JSON paths to be missing or null.
	NotExist []string `yaml:"not-exist" json:"not-exist"`
}

// CloakConfig configures request cloaking for non-Claude-Code clients.
// Cloaking disguises API requests to appear as originating from the official Claude Code CLI.
type CloakConfig struct {
	// Mode controls cloaking behavior: "auto" (default), "always", "never", or "uncloak".
	// Supplying this CloakConfig explicitly enables cloaking for an unprofiled API key.
	// - "auto": cloak unless strong request signals identify a verified native entrypoint
	// - "always": cloak every unconfirmed client; confirmed native Claude Code remains passthrough
	// - "never" / "uncloak": never apply cloaking; on non-CLI API keys, also strip incoming Claude Code identity and billing attribution
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`

	// StrictMode controls how caller system prompts are handled when cloaking.
	// - false (default): legacy-model whitelist uses a user reminder; all other models use a mid-conversation system message
	// - true: strip caller system prompts and keep only the Claude Code billing and identity blocks
	StrictMode bool `yaml:"strict-mode,omitempty" json:"strict-mode,omitempty"`

	// SensitiveWords is a list of words to obfuscate with zero-width characters.
	// This can help bypass certain content filters.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// CacheUserID controls whether Claude user_id values are cached per API key.
	// When false, a fresh random user_id is generated for every request.
	CacheUserID *bool `yaml:"cache-user-id,omitempty" json:"cache-user-id,omitempty"`
}

// ClaudeKey represents the configuration for a Claude API key,
// including the API key itself and an optional base URL for the API endpoint.
type ClaudeKey struct {
	// APIKey is the authentication key for accessing Claude API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/claude-sonnet-4").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Claude API endpoint.
	// If empty, the default Claude API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ConnectionPool configures upstream connection reuse for this credential.
	// Omitted disables pooling (preserving the fresh-transport-per-request default);
	// pools stay per credential, so opting in never shares connections with another key.
	ConnectionPool *ProxyConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []ClaudeModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// RebuildMidSystemMessage moves Claude messages with role "system" into the top-level system field.
	RebuildMidSystemMessage bool `yaml:"rebuild-mid-system-message,omitempty" json:"rebuild-mid-system-message,omitempty"`

	// NativeToolSearch trusts a custom base URL with Anthropic native tool search
	// (defer_loading tools and tool_reference results) for Codex tool_search.
	// Official api.anthropic.com credentials use native tool search without it.
	NativeToolSearch bool `yaml:"native-tool-search,omitempty" json:"native-tool-search,omitempty"`

	// DisableCooling overrides the global cooling policy for this credential when set.
	// True disables auth/model cooldowns; false explicitly enables them.
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry optionally overrides the global request-retry for this credential.
	// Nil or a negative value means "use the global request-retry". 0 disables additional retry rounds.
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`

	// Cloak configures request cloaking for non-Claude-Code clients.
	Cloak *CloakConfig `yaml:"cloak,omitempty" json:"cloak,omitempty"`

	// FingerprintProfile selects the Claude Code request fingerprint for this
	// credential on Anthropic Messages. Empty/default keeps the caller request
	// fingerprint and headers, including first-party api.anthropic.com API keys.
	// "claude-code-cli" opts official Anthropic API keys, custom gateways, and
	// delegated providers such as Kimi into the Claude Code OAuth CLI Messages
	// shape (OAuth betas, CCH signing, stable CLI identity) without treating the
	// credential as a real OAuth token for refresh/profile/runtime semantics.
	// CCH is a per-request hash and follows the native gate: it is emitted only on
	// api.anthropic.com and Vertex, so an opt-in on any other gateway sends the
	// billing block unsigned and cannot bust that gateway's prompt cache. Kimi
	// strips the attribution entirely by default and keeps it, unsigned, after an
	// explicit opt-in. count_tokens keeps the native model/messages/tools shape.
	// Recognized values are defined by NormalizeClaudeFingerprintProfile.
	FingerprintProfile string `yaml:"fingerprint-profile,omitempty" json:"fingerprint-profile,omitempty"`

	// ExperimentalCCHSigning is retained for configuration compatibility.
	// CCH signing is automatic for Claude OAuth and supported direct upstreams.
	ExperimentalCCHSigning bool `yaml:"experimental-cch-signing,omitempty" json:"experimental-cch-signing,omitempty"`
}

func (k ClaudeKey) GetAPIKey() string { return k.APIKey }

func (k ClaudeKey) GetBaseURL() string { return k.BaseURL }

func (k ClaudeKey) GetPrefix() string { return k.Prefix }

func (k ClaudeKey) GetProxyURL() string { return k.ProxyURL }

// ClaudeModel describes a mapping between an alias and the actual upstream model name.
type ClaudeModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// IsCompat preserves thinking blocks with empty signatures for compatible upstreams
	// and enables provider-aware signed-thinking replay for Claude-compatible API-key models.
	// Default false keeps the normal signature validation behavior.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// Thinking configures the thinking/reasoning capability for this model.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m ClaudeModel) GetName() string { return m.Name }

func (m ClaudeModel) GetAlias() string { return m.Alias }

func (m ClaudeModel) GetDisplayName() string   { return m.DisplayName }
func (m ClaudeModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m ClaudeModel) GetForceMapping() bool    { return m.ForceMapping }
func (m ClaudeModel) GetIsCompat() bool        { return m.IsCompat }

func (m ClaudeModel) GetThinking() *registry.ThinkingSupport { return m.Thinking }

// CodexKey represents the configuration for a Codex API key,
// including the API key itself and an optional base URL for the API endpoint.
type CodexKey struct {
	// APIKey is the authentication key for accessing Codex API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gpt-5-codex").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Codex API endpoint.
	// If empty, the default Codex API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// Websockets enables the Responses API websocket transport for this credential.
	Websockets bool `yaml:"websockets,omitempty" json:"websockets,omitempty"`

	// AlphaSearch allows this Codex API key to serve the Alpha Search endpoint.
	AlphaSearch bool `yaml:"alpha-search,omitempty" json:"alpha-search,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ConnectionPool configures upstream connection reuse for this credential.
	// Omitted disables pooling (preserving the fresh-transport-per-request default);
	// pools stay per credential, so opting in never shares connections with another key.
	ConnectionPool *ProxyConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []CodexModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling overrides the global cooling policy for this credential when set.
	// True disables auth/model cooldowns; false explicitly enables them.
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// DisableCodexCloaking optionally overrides the global codex.disable-codex-cloaking for this credential.
	// True disables cloaking; false explicitly enables cloaking; omitted inherits global codex.disable-codex-cloaking.
	DisableCodexCloaking *bool `yaml:"disable-codex-cloaking,omitempty" json:"disable-codex-cloaking,omitempty"`

	// RequestRetry optionally overrides the global request-retry for this credential.
	// Nil or a negative value means "use the global request-retry". 0 disables additional retry rounds.
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`
}

func (k CodexKey) GetAPIKey() string { return k.APIKey }

func (k CodexKey) GetBaseURL() string { return k.BaseURL }

func (k CodexKey) GetPrefix() string { return k.Prefix }

func (k CodexKey) GetProxyURL() string { return k.ProxyURL }

// CodexModel describes a mapping between an alias and the actual upstream model name.
type CodexModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// IsCompat converts Codex MultiAgentV2 agent_message items into portable
	// Responses message/user input when client.codex.optimize-multi-agent-v2 is also true.
	// Use this for third-party Responses-compatible endpoints that do not accept
	// native agent_message items or empty-signature thinking blocks. Default false
	// keeps the native behavior unchanged.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// SupportConfigurationUpdate enables configuration_update for this API-key model.
	// It defaults to false, independently of the built-in OAuth model catalog.
	SupportConfigurationUpdate bool `yaml:"support-configuration-update,omitempty" json:"support-configuration-update,omitempty"`

	// Thinking configures the thinking/reasoning capability for this model.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m CodexModel) GetName() string { return m.Name }

func (m CodexModel) GetAlias() string { return m.Alias }

func (m CodexModel) GetDisplayName() string   { return m.DisplayName }
func (m CodexModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m CodexModel) GetForceMapping() bool    { return m.ForceMapping }
func (m CodexModel) GetIsCompat() bool        { return m.IsCompat }

func (m CodexModel) GetThinking() *registry.ThinkingSupport { return m.Thinking }

// XAIKey uses the Codex API key structure for native xAI execution.
type XAIKey = CodexKey

// XAIModel uses the Codex model mapping structure for xAI models.
type XAIModel = CodexModel

// MetaKey uses the Codex API key structure for native Meta execution.
type MetaKey = CodexKey

// MetaModel uses the Codex model mapping structure for Meta models.
type MetaModel = CodexModel

// GeminiKey represents the configuration for a Gemini API key,
// including optional overrides for upstream base URL, proxy routing, and headers.
type GeminiKey struct {
	// APIKey is the authentication key for accessing Gemini API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gemini-3-pro-preview").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL optionally overrides the Gemini API endpoint.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ConnectionPool configures upstream connection reuse for this credential.
	// Omitted disables pooling (preserving the fresh-transport-per-request default);
	// pools stay per credential, so opting in never shares connections with another key.
	ConnectionPool *ProxyConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []GeminiModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling overrides the global cooling policy for this credential when set.
	// True disables auth/model cooldowns; false explicitly enables them.
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry optionally overrides the global request-retry for this credential.
	// Nil or a negative value means "use the global request-retry". 0 disables additional retry rounds.
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`
}

func (k GeminiKey) GetAPIKey() string { return k.APIKey }

func (k GeminiKey) GetBaseURL() string { return k.BaseURL }

func (k GeminiKey) GetPrefix() string { return k.Prefix }

func (k GeminiKey) GetProxyURL() string { return k.ProxyURL }

// GeminiModel describes a mapping between an alias and the actual upstream model name.
type GeminiModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// IsCompat preserves thinking blocks with empty signatures for compatible upstreams.
	// Default false keeps the normal signature validation behavior.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// Thinking configures the thinking/reasoning capability for this model.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m GeminiModel) GetName() string { return m.Name }

func (m GeminiModel) GetAlias() string { return m.Alias }

func (m GeminiModel) GetDisplayName() string   { return m.DisplayName }
func (m GeminiModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m GeminiModel) GetForceMapping() bool    { return m.ForceMapping }
func (m GeminiModel) GetIsCompat() bool        { return m.IsCompat }

func (m GeminiModel) GetThinking() *registry.ThinkingSupport { return m.Thinking }

// OpenAICompatibility represents the configuration for OpenAI API compatibility
// with external providers, allowing model aliases to be routed through OpenAI API format.
type OpenAICompatibility struct {
	// Name is the identifier for this OpenAI compatibility configuration.
	Name string `yaml:"name" json:"name"`

	// Priority controls selection preference when multiple providers or credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Disabled prevents this provider from being used for routing.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// Prefix optionally namespaces model aliases for this provider (e.g., "teamA/kimi-k2").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the external OpenAI-compatible API endpoint.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// APIKeyEntries defines API keys with optional per-key proxy configuration.
	APIKeyEntries []OpenAICompatibilityAPIKey `yaml:"api-key-entries,omitempty" json:"api-key-entries,omitempty"`

	// ConnectionPool is the provider-group default for upstream connection reuse.
	// A per-key connection-pool entry overrides it field by field; omitted disables pooling.
	ConnectionPool *ProxyConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []OpenAICompatibilityModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent to this provider.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// SupportPromptCacheKey enables derived prompt_cache_key injection for supported requests.
	SupportPromptCacheKey bool `yaml:"support-prompt-cache-key,omitempty" json:"support-prompt-cache-key,omitempty"`

	// DisableCooling overrides the global cooling policy for this provider when set.
	// True disables auth/model cooldowns; false explicitly enables them.
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry optionally overrides the global request-retry for this provider.
	// Nil or a negative value means "use the global request-retry". 0 disables additional retry rounds.
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`
}

// OpenAICompatibilityAPIKey represents an API key configuration with optional proxy setting.
type OpenAICompatibilityAPIKey struct {
	// APIKey is the authentication key for accessing the external API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ConnectionPool configures upstream connection reuse for this key, overriding
	// the provider-group default field by field. Pools stay per credential, so
	// opting in never shares connections with another key.
	ConnectionPool *ProxyConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`
}

// OpenAICompatibilityModel represents a model configuration for OpenAI compatibility,
// including the actual model name and its alias for API routing.
type OpenAICompatibilityModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// Image marks this model as callable through /v1/images/generations and /v1/images/edits.
	Image bool `yaml:"image,omitempty" json:"image,omitempty"`

	// InputModalities declares chat/responses input capabilities (e.g. text, image) for Codex and other clients.
	// This is separate from Image, which only enables /v1/images/* endpoints.
	InputModalities []string `yaml:"input-modalities,omitempty" json:"input-modalities,omitempty"`

	// OutputModalities declares supported output modalities when known (e.g. text, image).
	OutputModalities []string `yaml:"output-modalities,omitempty" json:"output-modalities,omitempty"`

	// IsCompat preserves Claude thinking blocks for compatible upstreams.
	// Default false keeps the normal signature validation behavior.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// UseMaxCompletionTokens emits max_completion_tokens instead of legacy max_tokens for this model.
	// Default false preserves max_tokens for older compatible upstreams.
	UseMaxCompletionTokens bool `yaml:"use-max-completion-tokens,omitempty" json:"use-max-completion-tokens,omitempty"`

	// Thinking configures the thinking/reasoning capability for this model.
	// If nil, the model defaults to level-based reasoning with levels ["low", "medium", "high"].
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m OpenAICompatibilityModel) GetName() string { return m.Name }

func (m OpenAICompatibilityModel) GetAlias() string { return m.Alias }

func (m OpenAICompatibilityModel) GetDisplayName() string          { return m.DisplayName }
func (m OpenAICompatibilityModel) GetMaxContextLength() int        { return m.MaxContextLength }
func (m OpenAICompatibilityModel) GetForceMapping() bool           { return m.ForceMapping }
func (m OpenAICompatibilityModel) GetIsCompat() bool               { return m.IsCompat }
func (m OpenAICompatibilityModel) GetUseMaxCompletionTokens() bool { return m.UseMaxCompletionTokens }

func (m OpenAICompatibilityModel) GetThinking() *registry.ThinkingSupport { return m.Thinking }
