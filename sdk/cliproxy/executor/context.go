package executor

import (
	"context"
	"sync"
	"sync/atomic"
)

type downstreamWebsocketContextKey struct{}
type requireUpstreamWebsocketContextKey struct{}
type upstreamAttemptTrackerContextKey struct{}
type degradedRoutingContextKey struct{}
type requestPreparationCacheContextKey struct{}

// DegradedRoutingMetadataKey scopes session affinity within the selected account pool.
const DegradedRoutingMetadataKey = "degraded_routing"

// WithDegradedRouting selects a credential pool using a server-owned context value.
func WithDegradedRouting(ctx context.Context, degraded bool) context.Context {
	return context.WithValue(ctx, degradedRoutingContextKey{}, degraded)
}

// DegradedRouting is false on the normal listener and for existing SDK callers.
func DegradedRouting(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	degraded, _ := ctx.Value(degradedRoutingContextKey{}).(bool)
	return degraded
}

// HasRoutingPool reports whether the caller is a pool-scoped model request.
func HasRoutingPool(ctx context.Context) bool {
	return ctx != nil && ctx.Value(degradedRoutingContextKey{}) != nil
}

type upstreamAttemptTracker struct {
	attempted atomic.Bool
}

// RequestPreparationCache stores one request-scoped, immutable preparation snapshot.
// Executors own the value types; the holder only provides isolated synchronization
// and never survives the request context.
type RequestPreparationCache struct {
	mu    sync.Mutex
	key   string
	value any
}

// WithRequestPreparationCache always installs a fresh holder for one Manager
// invocation, even when its parent context belongs to an earlier invocation.
func WithRequestPreparationCache(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestPreparationCacheContextKey{}, &RequestPreparationCache{})
}

// RequestPreparationCacheFromContext returns the request-local holder, if any.
func RequestPreparationCacheFromContext(ctx context.Context) *RequestPreparationCache {
	if ctx == nil {
		return nil
	}
	holder, _ := ctx.Value(requestPreparationCacheContextKey{}).(*RequestPreparationCache)
	return holder
}

// Load retrieves one request-local snapshot.
func (c *RequestPreparationCache) Load(key string) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != key || c.value == nil {
		return nil, false
	}
	return c.value, true
}

// Store publishes one request-local snapshot.
func (c *RequestPreparationCache) Store(key string, value any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.value = key, value
}

// WithDownstreamWebsocket marks the current request as coming from a downstream websocket connection.
func WithDownstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, downstreamWebsocketContextKey{}, true)
}

// DownstreamWebsocket reports whether the current request originates from a downstream websocket connection.
func DownstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(downstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithRequiredUpstreamWebsocket marks a request whose incremental context is valid only on the current upstream websocket.
func WithRequiredUpstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requireUpstreamWebsocketContextKey{}, true)
}

// RequiredUpstreamWebsocket reports whether falling back to an HTTP upstream would lose request context.
func RequiredUpstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(requireUpstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithUpstreamAttemptTracker installs a fresh tracker for one provider execution attempt.
func WithUpstreamAttemptTracker(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamAttemptTrackerContextKey{}, &upstreamAttemptTracker{})
}

// MarkUpstreamAttempt records that the provider execution reached an upstream transport boundary.
func MarkUpstreamAttempt(ctx context.Context) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(upstreamAttemptTrackerContextKey{}).(*upstreamAttemptTracker)
	if !ok || tracker == nil {
		return
	}
	tracker.attempted.Store(true)
}

// UpstreamAttempted reports whether the tracked provider execution reached an upstream transport boundary.
func UpstreamAttempted(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	tracker, ok := ctx.Value(upstreamAttemptTrackerContextKey{}).(*upstreamAttemptTracker)
	return ok && tracker != nil && tracker.attempted.Load()
}
