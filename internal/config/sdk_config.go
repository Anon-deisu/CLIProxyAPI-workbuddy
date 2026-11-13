// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// MaxRequestBodyBytes limits incoming HTTP request bodies. Non-positive values disable the limit.
	MaxRequestBodyBytes int64 `yaml:"max-request-body-bytes" json:"max-request-body-bytes"`

	// ResponseCompression is the canonical downstream response compression configuration.
	// Config.ServerResponseCompression mirrors it for JSON and programmatic access.
	ResponseCompression ResponseCompressionConfig `yaml:"response-compression" json:"-"`

	// Client configures client-facing compatibility behavior.
	Client ClientConfig `yaml:"client" json:"client"`

	// OAuthOnlyFields records v8 provider settings that apply only to OAuth/file credentials.
	OAuthOnlyFields map[string]bool `yaml:"-" json:"-"`

	// CodexResponseSteering mirrors the provider-wide runtime setting for API handlers.
	CodexResponseSteering bool `yaml:"-" json:"-"`

	// CacheTokenRewrite overrides only downstream cache usage. Nil disables it.
	CacheTokenRewrite *CacheTokenRewriteConfig `yaml:"-" json:"cache-token-rewrite,omitempty"`
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// TLSHandshakeTimeoutSeconds bounds upstream TLS handshakes made by the
	// shared uTLS client, in seconds. It applies to both the Claude Code
	// Anthropic transport and the Chrome ChatGPT/Codex transport; the
	// standard fallback transport is unaffected. It starts after TCP or
	// proxy connection establishment returns and does not bound dialing or
	// proxy CONNECT. Those uTLS paths use custom TLS dialing, for which Go
	// ignores http.Transport.TLSHandshakeTimeout, so a peer that accepts the
	// connection but never answers the ClientHello would otherwise block the
	// request indefinitely and request-retry could never engage. The bound
	// applies only during the TLS handshake and is removed once it succeeds,
	// so inference and long-running streams are unaffected. The default 0
	// disables the bound (previous behavior); negative values are treated as
	// 0, and values beyond the representable duration range saturate.
	TLSHandshakeTimeoutSeconds int `yaml:"tls-handshake-timeout-seconds" json:"tls-handshake-timeout-seconds"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// RequestLogSampleRate samples successful request logs. Nil preserves full logging.
	RequestLogSampleRate *float64 `yaml:"request-log-sample-rate,omitempty" json:"request-log-sample-rate,omitempty"`

	// CodexOrphanDelegationCompatibility mirrors the provider-wide runtime setting for API handlers.
	CodexOrphanDelegationCompatibility bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// EffectiveRequestLogSampleRate returns the configured probability, defaulting to one.
func (c *SDKConfig) EffectiveRequestLogSampleRate() float64 {
	if c == nil || c.RequestLogSampleRate == nil {
		return 1
	}
	return *c.RequestLogSampleRate
}

func validateRequestLogSampleRateNode(node *yaml.Node) error {
	if node == nil || node.Tag == "!!null" {
		return nil
	}
	var rate float64
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!int" && node.Tag != "!!float") || node.Decode(&rate) != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
		return fmt.Errorf("observability.logs.request-log-sample-rate must be a finite number between 0 and 1")
	}
	return nil
}

// Validate sampling before optional loading can replace a malformed value with defaults.
func validateRequestLogSampleRateYAML(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil // The normal loader handles malformed YAML and optional empty files.
	}
	var shape any
	if err := doc.Decode(&shape); err != nil {
		return nil // Reject cyclic aliases and duplicate keys before alias expansion in the normal loader.
	}
	root := expandConfigAliases(doc.Content[0])
	node := yamlPath(root, "observability.logs.request-log-sample-rate")
	if node == nil {
		node = yamlPath(root, "request-log-sample-rate")
	}
	return validateRequestLogSampleRateNode(node)
}

// TLSHandshakeTimeout returns the upstream TLS handshake bound for the shared uTLS client.
// Non-positive values disable the bound; overflowing durations saturate.
func (c *SDKConfig) TLSHandshakeTimeout() time.Duration {
	if c == nil || c.TLSHandshakeTimeoutSeconds <= 0 {
		return 0
	}
	const maxTLSHandshakeTimeoutSeconds = math.MaxInt64 / int64(time.Second)
	if int64(c.TLSHandshakeTimeoutSeconds) > maxTLSHandshakeTimeoutSeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(c.TLSHandshakeTimeoutSeconds) * time.Second
}

// ClientConfig configures client-facing compatibility behavior.
type ClientConfig struct {
	Codex  CodexClientConfig  `yaml:"codex" json:"codex"`
	Claude ClaudeClientConfig `yaml:"claude" json:"claude"`
}

// ClaudeClientConfig controls client-facing Claude protocol compatibility.
type ClaudeClientConfig struct {
	ShowCodexCommentary bool `yaml:"show-codex-commentary" json:"show-codex-commentary"`
}

// CodexClientConfig configures Codex client compatibility and the model catalog.
type CodexClientConfig struct {
	OptimizeMultiAgentV2 bool                            `yaml:"optimize-multi-agent-v2" json:"optimize-multi-agent-v2"`
	EnableApplyPatch     bool                            `yaml:"enable-apply-patch" json:"enable-apply-patch"`
	WebSearchResultCache CodexWebSearchResultCacheConfig `yaml:"web-search-result-cache" json:"web-search-result-cache"`
}

const (
	DefaultCodexWebSearchResultCacheTTL    = 24 * time.Hour
	DefaultCodexWebSearchResultCacheLimit  = 1024
	maxCodexWebSearchResultCacheTTLSeconds = int64(math.MaxInt64 / time.Second)
)

// CodexWebSearchResultCacheConfig configures the session-scoped web search result cache.
type CodexWebSearchResultCacheConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	TTL     string `yaml:"ttl" json:"ttl"`
	Limit   int    `yaml:"limit" json:"limit"`
}

func (c CodexWebSearchResultCacheConfig) Validate() error {
	if _, err := parseCodexWebSearchResultCacheTTL(c.TTL); err != nil {
		return err
	}
	if c.Limit < 0 {
		return fmt.Errorf("client.codex.web-search-result-cache.limit must not be negative")
	}
	return nil
}

func (c CodexWebSearchResultCacheConfig) EffectiveTTL() time.Duration {
	d, err := parseCodexWebSearchResultCacheTTL(c.TTL)
	if err != nil || d <= 0 {
		return DefaultCodexWebSearchResultCacheTTL
	}
	return d
}

func (c CodexWebSearchResultCacheConfig) EffectiveLimit() int {
	if c.Limit > 0 {
		return c.Limit
	}
	return DefaultCodexWebSearchResultCacheLimit
}

func (c *CodexWebSearchResultCacheConfig) UnmarshalYAML(value *yaml.Node) error {
	if c == nil || value == nil || value.Kind == 0 || value.Tag == "!!null" {
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("client.codex.web-search-result-cache must be a mapping")
	}
	var next CodexWebSearchResultCacheConfig
	seen := map[string]bool{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		keyNode, valueNode := value.Content[i], value.Content[i+1]
		if keyNode == nil || valueNode == nil {
			continue
		}
		key := keyNode.Value
		if seen[key] {
			return fmt.Errorf("client.codex.web-search-result-cache: duplicate field %q", key)
		}
		seen[key] = true
		switch key {
		case "enabled":
			if valueNode.Tag != "!!null" && (valueNode.Kind != yaml.ScalarNode || valueNode.Tag != "!!bool") {
				return fmt.Errorf("client.codex.web-search-result-cache.enabled must be a boolean")
			}
			_ = valueNode.Decode(&next.Enabled)
		case "ttl":
			if valueNode.Tag != "!!null" {
				if valueNode.Kind != yaml.ScalarNode || (valueNode.Tag != "!!str" && valueNode.Tag != "!!int") {
					return fmt.Errorf("client.codex.web-search-result-cache.ttl must be a duration string or integer seconds")
				}
				next.TTL = strings.TrimSpace(valueNode.Value)
			}
		case "limit":
			if valueNode.Tag != "!!null" {
				if valueNode.Kind != yaml.ScalarNode || valueNode.Tag != "!!int" {
					return fmt.Errorf("client.codex.web-search-result-cache.limit must be an integer")
				}
				if err := valueNode.Decode(&next.Limit); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("client.codex.web-search-result-cache: unknown field %q", key)
		}
	}
	if err := next.Validate(); err != nil {
		return err
	}
	*c = next
	return nil
}

func (c *CodexWebSearchResultCacheConfig) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if c == nil || len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var raw struct {
		Enabled bool            `json:"enabled"`
		TTL     json.RawMessage `json:"ttl"`
		Limit   int             `json:"limit"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	ttl, err := parseCodexWebSearchResultCacheTTLJSON(raw.TTL)
	if err != nil {
		return err
	}
	next := CodexWebSearchResultCacheConfig{Enabled: raw.Enabled, TTL: ttl, Limit: raw.Limit}
	if err := next.Validate(); err != nil {
		return err
	}
	*c = next
	return nil
}

func parseCodexWebSearchResultCacheTTLJSON(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", fmt.Errorf("client.codex.web-search-result-cache.ttl: %w", err)
		}
		return strings.TrimSpace(value), nil
	}
	var seconds int64
	if err := json.Unmarshal(trimmed, &seconds); err != nil {
		return "", fmt.Errorf("client.codex.web-search-result-cache.ttl must be a duration string or integer seconds")
	}
	return strconv.FormatInt(seconds, 10), nil
}

func parseCodexWebSearchResultCacheTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return DefaultCodexWebSearchResultCacheTTL, nil
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds < 0 || seconds > maxCodexWebSearchResultCacheTTLSeconds {
			return 0, fmt.Errorf("client.codex.web-search-result-cache.ttl is out of range")
		}
		return time.Duration(seconds) * time.Second, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("client.codex.web-search-result-cache.ttl %q is invalid", raw)
	}
	if duration == 0 {
		return DefaultCodexWebSearchResultCacheTTL, nil
	}
	return duration, nil
}

// CacheTokenRewriteConfig chooses a percentage once per downstream request.
// An omitted maximum uses a fixed percentage. Persistence is managed by AccountScheduling.
type CacheTokenRewriteConfig struct {
	Enabled    bool     `yaml:"enabled" json:"enabled"`
	MinPercent *float64 `yaml:"min-percent" json:"min-percent"`
	MaxPercent *float64 `yaml:"max-percent" json:"max-percent"`
}

func (c CacheTokenRewriteConfig) Validate() error {
	if c.MinPercent == nil && (c.Enabled || c.MaxPercent != nil) {
		return fmt.Errorf("cache-token-rewrite requires min-percent")
	}
	for _, value := range []*float64{c.MinPercent, c.MaxPercent} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 100) {
			return fmt.Errorf("cache-token-rewrite percentages must be between 0 and 100")
		}
	}
	if c.MaxPercent != nil && *c.MaxPercent < *c.MinPercent {
		return fmt.Errorf("cache-token-rewrite max-percent must be at least min-percent")
	}
	return nil
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n")
	// or WebSocket Ping control frames.
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`

	// SendKeepAliveBeforeFT controls whether SSE heartbeats are sent before the first token arrives
	// when KeepAliveSeconds > 0. Default is false.
	SendKeepAliveBeforeFT bool `yaml:"send-keepalive-before-ft,omitempty" json:"send-keepalive-before-ft,omitempty"`
}
