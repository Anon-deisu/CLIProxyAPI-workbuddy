package logging

import (
	"context"

	"github.com/gin-gonic/gin"
)

type requestLogCaptureKey struct{}

// WithRequestLogCapture fixes the full capture decision for a request and its descendants.
func WithRequestLogCapture(ctx context.Context, enabled bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestLogCaptureKey{}, enabled)
}

// RequestLogCaptureEnabled shares the middleware decision with upstream recorders.
// Callers without request logging middleware retain their configured behavior.
func RequestLogCaptureEnabled(ctx context.Context, fallback bool) bool {
	if ctx == nil {
		return fallback
	}
	if enabled, ok := ctx.Value(requestLogCaptureKey{}).(bool); ok {
		return enabled
	}
	if c, ok := ctx.Value("gin").(*gin.Context); ok && c != nil && c.Request != nil {
		if enabled, ok := c.Request.Context().Value(requestLogCaptureKey{}).(bool); ok {
			return enabled
		}
	}
	return fallback
}

type deferredAPIRequestCaptureDisabledKey struct{}

// WithDeferredAPIRequestCaptureDisabled skips error-only upstream request capture
// for this context and its descendants. Full request logging is unaffected.
// Connection-scoped contexts keep this policy even when asynchronous callbacks
// outlive the handler and its pooled Gin context is reused.
func WithDeferredAPIRequestCaptureDisabled(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, deferredAPIRequestCaptureDisabledKey{}, true)
}

// DeferredAPIRequestCaptureDisabled reports whether error-only capture is disabled.
func DeferredAPIRequestCaptureDisabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	disabled, _ := ctx.Value(deferredAPIRequestCaptureDisabledKey{}).(bool)
	return disabled
}
