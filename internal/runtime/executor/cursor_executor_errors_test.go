package executor

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
)

const rateLimitTrailer = `cursor connect end-stream error: {"code":"resource_exhausted",` +
	`"details":[{"debug":{"error":"ERROR_PRO_USER_RATE_LIMIT_EXCEEDED",` +
	`"details":{"title":"Rate limit exceeded."}}}]}`

func TestRateLimitBypassesRetryLadder(t *testing.T) {
	err := errors.New(rateLimitTrailer)
	if !isCursorRateLimited(err) {
		t.Fatal("rate limit trailer not recognised")
	}
	// Every retry tier keys off these; a throttled account must hit none of
	// them, otherwise the ladder deepens the throttle.
	if isCursorProviderError(err) {
		t.Error("rate limit must not enter the degrade ladder")
	}
	if isCursorTransientProviderError(err) || isCursorHardProviderError(err) {
		t.Error("rate limit must not be classified as transient or hard")
	}
}

func TestRateLimitSurfacesAs429(t *testing.T) {
	humanized := humanizeCursorError(errors.New(rateLimitTrailer))
	var se statusErr
	if !errors.As(humanized, &se) {
		t.Fatalf("want statusErr, got %T", humanized)
	}
	if se.StatusCode() != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", se.StatusCode())
	}
	if !strings.Contains(se.Error(), "rate limit") {
		t.Errorf("message not actionable: %q", se.Error())
	}
}

func TestUntypedResourceExhaustionSurfacesAs429WithoutRetry(t *testing.T) {
	err := errors.New(`cursor connect end-stream error: {"code":"resource_exhausted","message":"Error"}`)
	if isCursorProviderError(err) {
		t.Fatal("untyped resource exhaustion must not enter the retry ladder")
	}
	humanized := humanizeCursorError(err)
	var se statusErr
	if !errors.As(humanized, &se) {
		t.Fatalf("want statusErr, got %T", humanized)
	}
	if se.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", se.StatusCode())
	}
}

func TestEmptyCursorTerminalSurfacesAs502(t *testing.T) {
	humanized := humanizeCursorError(cursorlib.ErrEmptyTerminal)
	var se statusErr
	if !errors.As(humanized, &se) {
		t.Fatalf("want statusErr, got %T", humanized)
	}
	if se.StatusCode() != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", se.StatusCode())
	}
}

func TestProviderBlipStaysTransient(t *testing.T) {
	err := errors.New(`cursor connect end-stream error: {"code":"unavailable",` +
		`"details":[{"debug":{"error":"ERROR_OPENAI","details":{"title":"Unable to reach the model provider"}}}]}`)
	if !isCursorTransientProviderError(err) {
		t.Error("provider connectivity blip should stay retryable")
	}
	if isCursorRateLimited(err) {
		t.Error("provider blip misread as rate limit")
	}
}
