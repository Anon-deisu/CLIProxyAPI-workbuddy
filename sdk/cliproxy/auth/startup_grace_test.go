package auth

import (
	"testing"
	"time"
)

// A restart burst: credentials and model catalogs load asynchronously, and
// requests arriving in that window used to fail with auth_unavailable — a
// stream of 503s that tripped downstream gateway circuit breakers ("所有供应商
// 已熔断"). Within the grace window those failures wait and retry instead.
func TestStartupGraceRetriesSelectionFailures(t *testing.T) {
	startupGraceEnabledForTests = true
	defer func() { startupGraceEnabledForTests = false }()
	m := NewManager(nil, nil, nil)
	err := &Error{Code: "auth_unavailable", Message: "no auth available"}

	wait, retry := m.shouldRetryAfterError(err, 0, []string{"cursor"}, "claude-fable-5", 0)
	if !retry {
		t.Fatal("warmup selection failure must be retried, not surfaced as 503")
	}
	if wait != startupGraceRetryDelay {
		t.Fatalf("expected the grace delay, got %v", wait)
	}
	// Bounded per request: a genuinely empty credential set still fails.
	if _, retry = m.shouldRetryAfterError(err, startupGraceMaxAttempts, []string{"cursor"}, "claude-fable-5", 0); retry {
		t.Fatal("grace retries must stop after the per-request bound")
	}
}

func TestStartupGraceExpires(t *testing.T) {
	startupGraceEnabledForTests = true
	defer func() { startupGraceEnabledForTests = false }()
	m := NewManager(nil, nil, nil)
	m.startedAt = time.Now().Add(-startupGraceWindow - time.Second)
	err := &Error{Code: "auth_unavailable", Message: "no auth available"}

	if _, retry := m.shouldRetryAfterError(err, 0, []string{"cursor"}, "claude-fable-5", 0); retry {
		t.Fatal("selection failures after warmup must fail fast as before")
	}
}

func TestStartupGraceIgnoresOtherErrors(t *testing.T) {
	m := NewManager(nil, nil, nil)
	err := &Error{Code: "provider_error", Message: "boom", HTTPStatus: 502}

	if _, retry := m.shouldRetryAfterError(err, 0, []string{"cursor"}, "claude-fable-5", 0); retry {
		t.Fatal("non-selection errors must not enter the grace loop")
	}
}
