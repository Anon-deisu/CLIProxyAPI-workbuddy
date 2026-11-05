package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// NewProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use the execution-scoped request proxy if configured (highest priority)
// 2. Use auth.ProxyURL if configured
// 3. Use cfg.ProxyURL if auth proxy is not configured
// 4. Use RoundTripper from context if none are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func NewProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) (clientOut *http.Client) {
	return httptransport.Decorate(ctx, newProxyAwareHTTPClient(ctx, cfg, auth, timeout))
}

func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// Priority: request override, then auth.ProxyURL, then cfg.ProxyURL.
	proxyURL := effectiveProxyURL(ctx, cfg, auth)

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		if transport := pooledProxyTransport(proxyURL, cfg, auth); transport != nil {
			httpClient.Transport = transport
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyutil.Redact(proxyURL))
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			httpClient.Transport = rt
			return httpClient
		}
	}

	// Priority 4: A credential may opt into pooling without an explicit proxy-url.
	// With an empty proxy-url the pooled transport keeps the inherited
	// environment-proxy behavior while isolating the credential's pool.
	if transport := pooledProxyTransport("", cfg, auth); transport != nil {
		httpClient.Transport = transport
	}

	return httpClient
}

// proxyPoolKey separates runtime publishers, TLS transports, credentials and settings.
type proxyPoolKey struct {
	publisher   uint64
	kind        proxyPoolKind
	scope       string
	proxy       string
	idle        time.Duration
	max         int
	invalidIdle string
}

type proxyPoolKind uint8

const (
	proxyPoolKindStandard proxyPoolKind = iota
	proxyPoolKindClaudeCode
)

const (
	proxyPoolDefaultIdleConnTimeout     = 300 * time.Second
	proxyPoolDefaultMaxIdleConnsPerHost = 2
	proxyPoolKeepAlivePeriod            = 30 * time.Second
	proxyPoolCapacity                   = 512
)

var proxyTransportCache = NewTransportCache[proxyPoolKey](proxyPoolCapacity)

// The generation fence prevents retired snapshots from refilling their pools.
// Publishers isolate embedded runtimes so one reload cannot evict another's pools.
var proxyPoolFence struct {
	sync.RWMutex
	nextPublisher  uint64
	nextGeneration uint64
	current        map[uint64]uint64
}

// RegisterProxyPoolPublisher reserves a publisher identity for one runtime.
func RegisterProxyPoolPublisher() uint64 {
	proxyPoolFence.Lock()
	defer proxyPoolFence.Unlock()
	proxyPoolFence.nextPublisher++
	return proxyPoolFence.nextPublisher
}

// AdvanceProxyPoolGeneration publishes a private snapshot before handlers can see it.
func AdvanceProxyPoolGeneration(publisher uint64, cfg *config.Config) {
	if cfg == nil {
		return
	}
	proxyPoolFence.Lock()
	proxyPoolFence.nextGeneration++
	generation := proxyPoolFence.nextGeneration
	if proxyPoolFence.current == nil {
		proxyPoolFence.current = make(map[uint64]uint64)
	}
	proxyPoolFence.current[publisher] = generation
	cfg.ProxyPoolPublisher = publisher
	cfg.ProxyPoolGeneration = generation
	toClose := proxyTransportCache.detachMatching(func(key proxyPoolKey) bool { return key.publisher == publisher })
	proxyPoolFence.Unlock()
	for _, tr := range toClose {
		tr.CloseIdleConnections()
	}
}

// RetireProxyPoolPublisher closes idle transports when the owning runtime shuts down.
func RetireProxyPoolPublisher(publisher uint64) {
	if publisher == 0 {
		return
	}
	proxyPoolFence.Lock()
	delete(proxyPoolFence.current, publisher)
	toClose := proxyTransportCache.detachMatching(func(key proxyPoolKey) bool { return key.publisher == publisher })
	proxyPoolFence.Unlock()
	for _, tr := range toClose {
		tr.CloseIdleConnections()
	}
}

// ResetProxyTransports closes idle connections owned by the shared proxy cache.
func ResetProxyTransports() { proxyTransportCache.Purge() }

func pooledProxyTransport(proxyURL string, cfg *config.Config, auth *cliproxyauth.Auth) *http.Transport {
	tr, pooled := pooledTransport(proxyPoolKindStandard, proxyURL, cfg, auth, func() (*http.Transport, error) {
		return buildPooledBaseTransport(proxyURL)
	})
	if pooled {
		return tr
	}
	return buildProxyTransport(proxyURL)
}

// pooledTransport owns a pool only when this credential opted in. Other callers
// keep their existing transport lifecycle and custom default transports.
func pooledTransport(kind proxyPoolKind, proxyURL string, cfg *config.Config, auth *cliproxyauth.Auth, build func() (*http.Transport, error)) (*http.Transport, bool) {
	settings, enabled := resolveProxyPoolSettings(auth)
	if !enabled {
		return nil, false
	}
	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		return nil, false
	}
	if kind == proxyPoolKindStandard && strings.TrimSpace(proxyURL) == "" {
		if base, ok := http.DefaultTransport.(*http.Transport); !ok || base == nil {
			return nil, false
		}
	}
	idle := proxyPoolDefaultIdleConnTimeout
	invalidIdle := ""
	if raw := strings.TrimSpace(settings.IdleConnTimeout); raw != "" {
		if duration, errParse := time.ParseDuration(raw); errParse == nil {
			if duration <= 0 {
				return unpooledTransport(build), true
			}
			idle = duration
		} else {
			invalidIdle = raw
		}
	}
	max := proxyPoolDefaultMaxIdleConnsPerHost
	if settings.MaxIdleConnsPerHost != nil {
		if *settings.MaxIdleConnsPerHost <= 0 {
			return unpooledTransport(build), true
		}
		max = *settings.MaxIdleConnsPerHost
	}
	var publisher uint64
	if cfg != nil {
		publisher = cfg.ProxyPoolPublisher
	}
	cacheProxy, poolLabel := proxyPoolCacheTarget(setting)
	key := proxyPoolKey{
		publisher: publisher, kind: kind, scope: proxyPoolScope(auth),
		proxy: cacheProxy, idle: idle, max: max, invalidIdle: invalidIdle,
	}
	// Generation zero also supports SDK callers that do not publish snapshots.
	if cfg != nil && cfg.ProxyPoolGeneration != 0 {
		proxyPoolFence.RLock()
		defer proxyPoolFence.RUnlock()
		if cfg.ProxyPoolGeneration != proxyPoolFence.current[cfg.ProxyPoolPublisher] {
			return unpooledTransport(build), true
		}
	}
	tr, errGet := proxyTransportCache.Get(key, func() (*http.Transport, error) {
		tr, errBuild := build()
		if errBuild != nil {
			return nil, errBuild
		}
		if tr == nil {
			return nil, fmt.Errorf("build returned no transport")
		}
		if invalidIdle != "" {
			log.Warnf("proxy connection pool: invalid idle-conn-timeout %q, using %s", invalidIdle, proxyPoolDefaultIdleConnTimeout)
		}
		tr.IdleConnTimeout = idle
		tr.MaxIdleConnsPerHost = max
		tr.MaxIdleConns = max
		if dial := tr.DialContext; dial != nil {
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, errDial := dial(ctx, network, addr)
				if errDial != nil {
					return nil, errDial
				}
				return stampProxyKeepAlive(conn), nil
			}
		}
		log.Debugf("proxy connection pool: created transport kind=%d %s scope=%s idle=%s max=%d", kind, poolLabel, proxyPoolSecretScope("scope:", key.scope), idle, max)
		return tr, nil
	})
	if errGet != nil {
		log.Errorf("proxy connection pool: %v", errGet)
		return nil, true
	}
	return tr, true
}

func resolveProxyPoolSettings(auth *cliproxyauth.Auth) (config.ProxyConnectionPoolConfig, bool) {
	override := proxyPoolOverride(auth)
	if override == nil || (override.Enabled != nil && !*override.Enabled) {
		return config.ProxyConnectionPoolConfig{}, false
	}
	return config.ProxyConnectionPoolConfig{
		Enabled: override.Enabled, IdleConnTimeout: strings.TrimSpace(override.IdleConnTimeout),
		MaxIdleConnsPerHost: override.MaxIdleConnsPerHost,
	}, true
}

func proxyPoolOverride(auth *cliproxyauth.Auth) *cliproxyauth.ProxyConnectionPoolOverride {
	if auth == nil {
		return nil
	}
	return auth.ProxyConnectionPool
}

// Proxy credentials are digested in cache keys and redacted in log labels.
func proxyPoolCacheTarget(setting proxyutil.Setting) (cacheProxy, poolLabel string) {
	switch setting.Mode {
	case proxyutil.ModeDirect:
		return "direct", "direct"
	case proxyutil.ModeInherit:
		return "inherit", "inherit"
	default:
		redacted := proxyutil.Redact(setting.Raw)
		if setting.URL != nil && setting.URL.User != nil {
			return redacted + "#" + proxyPoolSecretScope("proxyauth:", setting.URL.User.String()), redacted
		}
		return setting.Raw, redacted
	}
}

const proxyPoolAnonymousScope = "anonymous"

func proxyPoolScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return proxyPoolAnonymousScope
	}
	var identity string
	if id := strings.TrimSpace(auth.ID); id != "" {
		identity = "id:" + id
	} else if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributePath]); path != "" {
			identity = "path:" + path
		} else if source := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeSource]); source != "" {
			identity = "source:" + source
		}
	}
	secret := proxyPoolCredentialSecretScope(auth)
	if identity != "" && secret != "" {
		return identity + "#" + secret
	}
	if identity != "" {
		return identity
	}
	if secret != "" {
		return secret
	}
	return proxyPoolAnonymousScope
}

func proxyPoolCredentialSecretScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if refresh := authMetadataString(auth.Metadata, "refresh_token"); refresh != "" {
		return proxyPoolSecretScope("refresh:", refresh)
	}
	if access := authMetadataString(auth.Metadata, "access_token"); access != "" {
		return proxyPoolSecretScope("token:", access)
	}
	if auth.Attributes != nil {
		if apiKey := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeAPIKey]); apiKey != "" {
			return proxyPoolSecretScope("apikey:", apiKey)
		}
	}
	return ""
}

func proxyPoolSecretScope(prefix, secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return prefix + hex.EncodeToString(digest[:8])
}

func authMetadataString(metadata map[string]any, key string) string {
	text, _ := metadata[key].(string)
	return strings.TrimSpace(text)
}

// A one-off transport keeps its TLS fingerprint but cannot retain unowned idle sockets.
func unpooledTransport(build func() (*http.Transport, error)) *http.Transport {
	tr, errBuild := build()
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	if tr != nil {
		tr.DisableKeepAlives = true
	}
	return tr
}

func buildPooledBaseTransport(proxyURL string) (*http.Transport, error) {
	if strings.TrimSpace(proxyURL) == "" {
		if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
			return base.Clone(), nil
		}
		return nil, fmt.Errorf("http.DefaultTransport is not *http.Transport")
	}
	tr, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		return nil, errBuild
	}
	return tr, nil
}

// Keepalive probes detect dead pooled peers without imposing a stream deadline.
func stampProxyKeepAlive(conn net.Conn) net.Conn {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return conn
	}
	if errKeepAlive := tcp.SetKeepAlive(true); errKeepAlive != nil {
		return conn
	}
	if errPeriod := tcp.SetKeepAlivePeriod(proxyPoolKeepAlivePeriod); errPeriod != nil {
		return conn
	}
	return conn
}

func effectiveProxyURL(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) string {
	if proxyURL := cliproxyexecutor.RequestProxyURL(ctx); proxyURL != "" {
		return proxyURL
	}
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

var devinTransportCache = NewTransportCache[string](DefaultTransportCacheCapacity)

// NewDevinHTTPClient suppresses automatic gzip on the Connect-RPC transport.
func NewDevinHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	client := newProxyAwareHTTPClient(httptransport.WithoutDecorator(ctx), cfg, auth, timeout)
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if tr, ok := transport.(*http.Transport); ok {
		key := fmt.Sprintf("transport:%p", tr)
		if _, pooled := resolveProxyPoolSettings(auth); !pooled {
			if proxyURL := effectiveProxyURL(ctx, cfg, auth); proxyURL != "" {
				digest := sha256.Sum256([]byte(proxyURL))
				key = "proxy:" + hex.EncodeToString(digest[:])
			}
		}
		cloned, errClone := devinTransportCache.Get(key, func() (*http.Transport, error) {
			copy := tr.Clone()
			copy.DisableCompression = true
			return copy, nil
		})
		if errClone == nil {
			client.Transport = cloned
			return httptransport.Decorate(ctx, client)
		}
	}
	client.Transport = devinNoGzipRoundTripper{base: transport}
	return httptransport.Decorate(ctx, client)
}

type devinNoGzipRoundTripper struct {
	base http.RoundTripper
}

func (rt devinNoGzipRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	return rt.base.RoundTrip(req)
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}
