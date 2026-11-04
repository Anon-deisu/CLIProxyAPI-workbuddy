package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval    = 5 * time.Second
	refreshMaxConcurrency   = 16
	refreshPendingBackoff   = time.Minute
	refreshFailureBackoff   = 5 * time.Minute
	invalidGrantBackoffBase = time.Minute
	invalidGrantBackoffMax  = 30 * time.Minute
	// refreshIneffectiveBackoff throttles refresh attempts when an executor returns
	// success but the auth still evaluates as needing refresh (e.g. token expiry
	// wasn't updated). Without this guard, the auto-refresh loop can tight-loop and
	// burn CPU at idle.
	refreshIneffectiveBackoff = 30 * time.Second
	quotaBackoffBase          = time.Second
	quotaBackoffMax           = 30 * time.Minute
	minQuotaCooldownFloor     = 10 * time.Second
	transientErrorCooldown    = time.Minute
)

type quotaRefreshLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
}

// StartQuotaRefresh starts immediate and periodic local Codex OAuth quota probes.
// Restarting cancels and waits for the old loop before creating a new one.
func (m *Manager) StartQuotaRefresh(parent context.Context) {
	if m == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	m.quotaRefreshMu.Lock()
	defer m.quotaRefreshMu.Unlock()
	m.stopQuotaRefreshLocked()
	ctx, cancel := context.WithCancel(parent)
	loop := &quotaRefreshLoop{cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	m.mu.Lock()
	m.quotaRefreshLoop = loop
	m.mu.Unlock()
	go func() {
		defer close(loop.done)
		m.runQuotaRefresh(ctx, loop.wake)
	}()
}

// StopQuotaRefresh cancels quota probes and waits for all workers to exit.
func (m *Manager) StopQuotaRefresh() {
	if m == nil {
		return
	}
	m.quotaRefreshMu.Lock()
	defer m.quotaRefreshMu.Unlock()
	m.stopQuotaRefreshLocked()
}

func (m *Manager) stopQuotaRefreshLocked() {
	m.mu.Lock()
	loop := m.quotaRefreshLoop
	m.quotaRefreshLoop = nil
	m.mu.Unlock()
	if loop != nil {
		loop.cancel()
		<-loop.done
	}
}

// UpdateQuotaRefresh wakes the loop after SetConfig. It is optional: config is
// also checked at most every refreshCheckInterval, including during a probe.
func (m *Manager) UpdateQuotaRefresh() {
	if m == nil {
		return
	}
	m.mu.RLock()
	loop := m.quotaRefreshLoop
	m.mu.RUnlock()
	if loop != nil {
		select {
		case loop.wake <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) quotaRefreshSettings() (bool, time.Duration, int) {
	var settings internalconfig.CodexQuotaRefreshConfig
	home := false
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil {
		settings = cfg.Codex.QuotaRefresh
		home = cfg.Home.Enabled
	}
	enabled, interval, concurrency := settings.Effective()
	return enabled && !home, interval, concurrency
}

func (m *Manager) runQuotaRefresh(ctx context.Context, wake <-chan struct{}) {
	timer := time.NewTimer(refreshCheckInterval)
	defer timer.Stop()
	var lastFinished time.Time
	var running <-chan struct{}
	var cancelRound context.CancelFunc
	roundCanceled := false
	defer func() {
		if cancelRound != nil {
			cancelRound()
		}
		if running != nil {
			<-running
		}
	}()
	for ctx.Err() == nil {
		enabled, interval, concurrency := m.quotaRefreshSettings()
		now := time.Now()
		wait := refreshCheckInterval
		if !enabled {
			lastFinished = time.Time{}
			if running != nil && !roundCanceled {
				cancelRound()
				roundCanceled = true
			}
		} else if running == nil {
			remaining := lastFinished.Add(interval).Sub(now)
			if lastFinished.IsZero() || remaining <= 0 {
				roundCtx, cancel := context.WithCancel(ctx)
				done := make(chan struct{})
				running, cancelRound, roundCanceled = done, cancel, false
				// A round owns its fixed Effective concurrency until it completes.
				go func(roundCtx context.Context, concurrency int, done chan struct{}) {
					defer close(done)
					m.refreshQuotaBatch(roundCtx, concurrency)
				}(roundCtx, concurrency, done)
			} else if remaining < wait {
				wait = remaining
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		case <-running:
			cancelRound()
			running, cancelRound = nil, nil
			if !roundCanceled {
				// Measure intervals from completion; a slow round never overlaps.
				lastFinished = time.Now()
			}
		}
	}
}

func (m *Manager) refreshQuotaBatch(ctx context.Context, concurrency int) {
	if enabled, _, _ := m.quotaRefreshSettings(); !enabled || ctx.Err() != nil {
		return
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.auths))
	for id, auth := range m.auths {
		if quotaRefreshAuthEligible(auth) {
			if _, ok := m.executors[executorKeyFromAuth(auth)].(QuotaFetcher); ok {
				ids = append(ids, id)
			}
		}
	}
	m.mu.RUnlock()
	jobs := make(chan string)
	var workers sync.WaitGroup
	for range min(max(1, concurrency), len(ids)) {
		workers.Go(func() {
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				m.refreshQuota(ctx, id)
			}
		})
	}
	defer workers.Wait()
	defer close(jobs)
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		case jobs <- id:
		}
	}
}

func (m *Manager) quotaProbeSnapshot(id string) (*Auth, QuotaFetcher) {
	if enabled, _, _ := m.quotaRefreshSettings(); !enabled {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[id]
	if !quotaRefreshAuthEligible(auth) {
		return nil, nil
	}
	fetcher, _ := m.executors[executorKeyFromAuth(auth)].(QuotaFetcher)
	return auth.Clone(), fetcher
}

func (m *Manager) quotaProbeContext(ctx context.Context, auth *Auth) context.Context {
	if rt := m.roundTripperFor(auth); rt != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	return ctx
}

func (m *Manager) refreshQuota(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	if _, loaded := m.quotaRefreshInFlight.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	defer m.quotaRefreshInFlight.Delete(id)
	startedAt := time.Now()
	observed, fetcher := m.quotaProbeSnapshot(id)
	if observed == nil || fetcher == nil {
		return
	}
	probeCtx := m.quotaProbeContext(ctx, observed)
	headers, errProbe := fetcher.FetchQuota(probeCtx, observed.Clone())
	if ctx.Err() != nil {
		return
	}
	if statusCodeFromError(errProbe) == http.StatusUnauthorized && !isRequestScopedError(errProbe) {
		recovery := &quotaRefreshRecovery{observed: observed}
		refreshCtx := context.WithValue(probeCtx, quotaRefreshRecoveryKey{}, recovery)
		if refreshed, ok := m.tryRefreshAfterUnauthorized(refreshCtx, observed, errProbe, false); ok {
			current, currentFetcher := m.quotaProbeSnapshot(id)
			if ctx.Err() != nil || currentFetcher == nil || !quotaRefreshIdentityMatches(current, refreshed) {
				return
			}
			observed = current
			headers, errProbe = currentFetcher.FetchQuota(m.quotaProbeContext(ctx, observed), observed.Clone())
		} else if recovery.err != nil {
			// A failed recovery transport/response is not proof of invalid credentials.
			errProbe = recovery.err
			if quotaRefreshCredentialsRejected(errProbe) {
				// Preserve the original probe's unauthorized outcome only when
				// refresh explicitly proves that the OAuth credential is invalid.
				errProbe = &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized", Message: "OAuth refresh credentials rejected"}
			}
		}
	}
	m.ApplyQuotaRefresh(ctx, observed, startedAt, headers, errProbe)
}

type quotaRefreshRecoveryKey struct{}

type quotaRefreshRecovery struct {
	observed *Auth
	err      error
}

func quotaRefreshCredentialsRejected(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isRequestScopedError(err) {
		return false
	}
	status := statusCodeFromError(err)
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	var oauthErr interface {
		StatusCode() int
		ErrorCode() string
	}
	if errors.As(err, &oauthErr) {
		switch oauthErr.ErrorCode() {
		case "invalid_grant", "refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated":
			return true
		}
	}
	return false
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)
	workers := m.refreshWorkers()
	loop := newAuthAutoRefreshLoop(m, interval, workers)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.mu.Unlock()

	loop.rebuild(time.Now())
	go loop.run(ctx)
}

// StopAutoRefresh cancels the background refresh loop, if running.
// It also stops the selector if it implements StoppableSelector.
func (m *Manager) StopAutoRefresh() {
	m.mu.Lock()
	cancel := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Stop selector if it implements StoppableSelector (e.g., SessionAffinitySelector)
	sel := m.Selector()
	if stoppable, ok := sel.(StoppableSelector); ok && stoppable != nil {
		stoppable.Stop()
	}
}

func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

func (m *Manager) queueRefreshUnschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.remove(authID)
}

// codexAutoRefreshUnavailable mirrors the executor's local renewal requirements.
// Runtime evaluators and Home retain control over their own refresh mechanisms.
func (m *Manager) codexAutoRefreshUnavailable(a *Auth, now time.Time) bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") || m.HomeEnabled() {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return false
	}
	// Intentionally ignore refreshToken: the local Codex executor only reads refresh_token.
	if authMetadataString(a, "refresh_token") != "" {
		return false
	}
	_, hasSIWC, _ := CodexSIWCNeedsRefresh(a, now)
	return !hasSIWC
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil || m.codexAutoRefreshUnavailable(a, now) {
		return false
	}
	if hasUnauthorizedAuthFailure(a) || hasDisabledInvalidGrantFailure(a) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if authHasUnchangedRejectedRefreshToken(a) {
		if a.HasValidAccessToken(now) {
			return false
		}
		return !a.Disabled && a.Status != StatusDisabled
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}
	if next, due := devinQuotaRevalidateAt(a, now); due {
		return !next.After(now)
	}

	if _, hasSIWC, needSIWC := CodexSIWCNeedsRefresh(a, now); hasSIWC {
		if needSIWC {
			return true
		}
		if _, hasOuter, needOuter := CodexOuterNeedsRefresh(a, now); hasOuter {
			return needOuter
		}
		return false
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return expiry.Sub(now) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(loop *authAutoRefreshLoop, id string, registrationEpoch uint64, now time.Time) *authRefreshJob {
	m.mu.Lock()
	auth := m.auths[id]
	if auth == nil || auth.RegistrationEpoch != registrationEpoch || hasUnauthorizedAuthFailure(auth) || hasDisabledInvalidGrantFailure(auth) {
		m.mu.Unlock()
		return nil
	}
	if m.refreshJobs[id] != nil || (!auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter)) {
		m.mu.Unlock()
		return nil
	}
	job := &authRefreshJob{
		id:                id,
		registrationEpoch: registrationEpoch,
		pendingUntil:      now.Add(refreshPendingBackoff),
		loop:              loop,
	}
	if m.refreshJobs == nil {
		m.refreshJobs = make(map[string]*authRefreshJob)
	}
	m.refreshJobs[id] = job
	auth.NextRefreshAfter = job.pendingUntil
	auth.Generation++
	auth.UpdatedAt = now
	m.mu.Unlock()

	m.queueRefreshReschedule(id)
	return job
}

func (m *Manager) beginRefreshJob(ctx context.Context, job *authRefreshJob) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[job.id]
	if ctx.Err() != nil || m.refreshJobs[job.id] != job || auth == nil || auth.RegistrationEpoch != job.registrationEpoch {
		return false
	}
	job.running = true
	return true
}

func (m *Manager) finishRefreshJob(job *authRefreshJob, retryAt time.Time, queuedOnly bool) {
	m.mu.Lock()
	if m.refreshJobs[job.id] != job || (queuedOnly && job.running) {
		m.mu.Unlock()
		return
	}
	delete(m.refreshJobs, job.id)
	if auth := m.auths[job.id]; auth != nil && auth.RegistrationEpoch == job.registrationEpoch && auth.NextRefreshAfter.Equal(job.pendingUntil) {
		// Clear only this job's pending marker, preserving newer refresh outcomes.
		auth.NextRefreshAfter = retryAt
		auth.Generation++
		auth.UpdatedAt = time.Now()
	}
	m.mu.Unlock()
	// Notify the current loop, which may differ from the job's original loop.
	m.queueRefreshReschedule(job.id)
}

type authRefreshLock struct {
	once sync.Once
	busy chan struct{}
}

func (l *authRefreshLock) acquire(ctx context.Context) bool {
	l.once.Do(func() { l.busy = make(chan struct{}, 1) })
	select {
	case <-ctx.Done():
		return false
	case l.busy <- struct{}{}:
		return true
	}
}

func authOuterAccessToken(auth *Auth) string {
	if token := authMetadataString(auth, "access_token"); token != "" {
		return token
	}
	return authMetadataString(auth, "accessToken")
}

func authAccessToken(auth *Auth) string {
	if IsCodexUseSIWC(auth) {
		return authSIWCTokenField(auth, "access_token")
	}
	return authOuterAccessToken(auth)
}

func authAccessTokenForRequest(ctx context.Context, auth *Auth, clientHeaders ...http.Header) string {
	if IsCodexUseSIWCForRequest(ctx, auth, clientHeaders...) {
		return authSIWCTokenField(auth, "access_token")
	}
	return authOuterAccessToken(auth)
}

func authMatchesTokenPartition(auth *Auth, token string) bool {
	token = strings.TrimSpace(token)
	if auth == nil || token == "" {
		return false
	}
	return authAccessToken(auth) == token || authOuterAccessToken(auth) == token
}

func authRefreshToken(auth *Auth) string {
	if token := authMetadataString(auth, "refresh_token"); token != "" {
		return token
	}
	return authMetadataString(auth, "refreshToken")
}

func authHasRefreshCredential(auth *Auth) bool {
	if authRefreshToken(auth) != "" {
		return true
	}
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") {
		return false
	}
	return authMetadataString(auth, "dca_token") != "" || strings.TrimSpace(auth.Attributes["dca_token"]) != ""
}

func authRefreshToken(auth *Auth) string {
	if token := authMetadataString(auth, "refresh_token"); token != "" {
		return token
	}
	return authMetadataString(auth, "refreshToken")
}

func clearUnauthorizedModelStates(auth *Auth, now time.Time) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	var resumed []string
	anyChanged := false
	for model, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		isUnauth := false
		if state.LastError != nil {
			if state.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(state.LastError.Code, "unauthorized") || isUnauthorizedError(state.LastError) || isInvalidGrantResultError(state.LastError) {
				isUnauth = true
			}
		}
		if !isUnauth && (strings.Contains(strings.ToLower(state.StatusMessage), "unauthorized") || isInvalidGrantErrorMessage(state.StatusMessage)) {
			isUnauth = true
		}
		if !isUnauth {
			continue
		}
		anyChanged = true
		if state.Quota.Exceeded && (state.Quota.NextRecoverAt.IsZero() || state.Quota.NextRecoverAt.After(now)) {
			state.LastError = nil
			if strings.Contains(strings.ToLower(state.StatusMessage), "unauthorized") || isInvalidGrantErrorMessage(state.StatusMessage) {
				state.StatusMessage = state.Quota.Reason
			}
			state.UpdatedAt = now
			continue
		}
		resetModelState(state, now)
		resumed = append(resumed, model)
	}
	if anyChanged {
		updateAggregatedAvailability(auth, now)
	}
	return resumed
}

// RefreshHomeSelectionAfterUnauthorized only reuses a newer snapshot already
// installed by Home. It never refreshes or mutates Home-owned credentials.
func (m *Manager) RefreshHomeSelectionAfterUnauthorized(ctx context.Context, selection *HomeDispatchSelection, failedAuth *Auth) (*Auth, bool, error) {
	if m == nil || selection == nil {
		return nil, false, nil
	}
	current := selection.CloneAuth()
	if failedAuth == nil {
		failedAuth = current
	}
	if current != nil && failedAuth != nil && current.ID == failedAuth.ID {
		currentToken := authAccessTokenForRequest(ctx, current)
		failedToken := authAccessTokenForRequest(ctx, failedAuth)
		if currentToken != "" && failedToken != "" && currentToken != failedToken {
			return current, true, nil
		}
	}
	return current, false, nil
}

// tryRefreshAfterUnauthorized refreshes local OAuth credentials once after a
// 401 so the current auth can be retried before fallback/suspend.
func (m *Manager) tryRefreshAfterUnauthorized(ctx context.Context, auth *Auth, execErr error, alreadyTried bool, clientHeaders ...http.Header) (*Auth, bool) {
	if m == nil || auth == nil || execErr == nil {
		return auth, false
	}
	// Request-scoped failures describe this request, not stale credentials.
	// Refreshing would turn a direct error response into an implicit retry.
	if isRequestScopedError(execErr) || !isUnauthorizedError(execErr) {
		return auth, false
	}
	if !isUnauthorizedError(execErr) || (!authHasRefreshCredential(auth) && !authHasAutomaticOAuthRecovery(auth)) {
		return auth, false
	}
	log.Debugf("unauthorized response for %s (%s), refreshing credentials before fallback", auth.Provider, auth.ID)
	refreshed, errRefresh := m.refreshAuthForRequest(ctx, auth.ID, failedToken)
	if errRefresh != nil || refreshed == nil {
		log.WithFields(log.Fields{"provider": auth.Provider, "auth_id": auth.ID, "status": statusCodeFromError(errRefresh)}).Debug("credential refresh before fallback failed")
		return auth, false
	}
	if failedToken != "" && authAccessTokenForRequest(ctx, refreshed, clientHeaders...) == failedToken && (refreshed.RejectedAccessToken == failedToken || refreshed.RejectedSIWCAccessToken == failedToken) {
		return auth, false
	}
	return refreshed, true
}

func authHasAutomaticOAuthRecovery(auth *Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	stored, ok := auth.Metadata["codex_auto_oauth"].(map[string]any)
	if !ok {
		return false
	}
	origin, _ := stored["origin"].(string)
	password, _ := stored["password"].(string)
	secret, _ := stored["totp_secret"].(string)
	return origin == "cpa-auto-oauth-v1" && password != "" && secret != ""
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	_, _ = m.refreshAuthForRequest(ctx, id, "")
}

// refreshAuthForRequest performs a synchronous credential refresh for the given auth.
// failedAccessToken lets concurrent callers reuse a refresh that already replaced the
// access token that produced the unauthorized response.
func (m *Manager) refreshAuthForRequest(ctx context.Context, id, failedAccessToken string) (refreshed *Auth, refreshErr error) {
	if m == nil {
		return nil, errors.New("auth manager is nil")
	}
	forceRefresh := isForceRefreshContext(ctx)
	ctx = cliproxyexecutor.WithoutRequestProxyURL(ctx)
	if ctx == nil {
		ctx = context.Background()
	}
	quotaRecovery, _ := ctx.Value(quotaRefreshRecoveryKey{}).(*quotaRefreshRecovery)
	if quotaRecovery != nil {
		defer func() { quotaRecovery.err = refreshErr }()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("auth id is empty")
	}
	if failedAccessToken != "" {
		m.markRejectedAccessToken(id, failedAccessToken)
	}

	lockValue, _ := m.refreshLocks.LoadOrStore(id, &authRefreshLock{})
	lock, _ := lockValue.(*authRefreshLock)
	if lock == nil {
		lock = &authRefreshLock{}
		m.refreshLocks.Store(id, lock)
	}
	if !lock.acquire(ctx) {
		return nil, ctx.Err()
	}
	defer func() { <-lock.busy }()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	m.mu.RLock()
	auth := m.auths[id].Clone()
	var exec ProviderExecutor
	if auth != nil {
		// Use the same effective provider key as request execution so OpenAI-compat
		// auths registered under namespaced keys still resolve for refresh.
		exec, _ = m.executorLocked(executorKeyFromAuth(auth))
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return nil, errors.New("auth or executor not found")
	}
	if quotaRecovery != nil {
		enabled, _, _ := m.quotaRefreshSettings()
		if !enabled || !quotaRefreshIdentityMatches(auth, quotaRecovery.observed) {
			return nil, errors.New("quota recovery credential is no longer current")
		}
	}

	// Another request may already have refreshed this credential.
	if failedAccessToken != "" {
		if !authMatchesTokenPartition(auth, failedAccessToken) && (authAccessToken(auth) != "" || authOuterAccessToken(auth) != "") {
			return auth.Clone(), nil
		}
	}

	// Do not repeatedly submit an unchanged refresh grant already known to be terminally rejected.
	if !forceRefresh && authHasUnchangedRejectedRefreshToken(auth) {
		checkNow := time.Now()
		accessTokenRejected := failedAccessToken != "" && authMatchesTokenPartition(auth, failedAccessToken)
		if !accessTokenRejected && auth.HasValidAccessToken(checkNow) {
			shouldReschedule := false
			shouldUnschedule := false
			m.mu.Lock()
			if current := m.auths[id]; current != nil &&
				current.RegistrationEpoch == auth.RegistrationEpoch &&
				current.CredentialVersion == auth.CredentialVersion &&
				authHasUnchangedRejectedRefreshToken(current) {
				if exp, ok := current.AccessTokenExpirationTime(); ok && !exp.IsZero() && exp.After(checkNow) {
					if !current.NextRefreshAfter.Equal(exp) {
						current.NextRefreshAfter = exp
						shouldReschedule = true
					}
				} else if !current.NextRefreshAfter.IsZero() {
					current.NextRefreshAfter = time.Time{}
					shouldUnschedule = true
				}
				m.auths[id] = current
				if m.scheduler != nil {
					m.scheduler.upsertAuth(current.Clone())
				}
			}
			m.mu.Unlock()
			if shouldReschedule {
				m.queueRefreshReschedule(id)
			} else if shouldUnschedule {
				m.queueRefreshUnschedule(id)
			}
			if auth.RefreshError != nil {
				return nil, auth.RefreshError
			}
			if auth.LastError != nil {
				return nil, auth.LastError
			}
			return nil, errors.New("refresh token requires sign-in")
		}
		// Access token has expired (or was rejected by a 401) while refresh grant remains terminally rejected.
		m.mu.Lock()
		if current := m.auths[id]; current != nil &&
			current.RegistrationEpoch == auth.RegistrationEpoch &&
			current.CredentialVersion == auth.CredentialVersion &&
			authHasUnchangedRejectedRefreshToken(current) {
			current.Generation++
			current.UpdatedAt = checkNow
			current.Unavailable = true
			if current.Disabled || current.Status == StatusDisabled {
				current.Status = StatusDisabled
				if current.StatusMessage == "" {
					current.StatusMessage = "disabled"
				}
			} else {
				current.Status = StatusError
				current.StatusMessage = "unauthorized (refresh token invalid)"
			}
			current.NextRefreshAfter = time.Time{}
			current.NextRetryAfter = time.Time{}
			current.RefreshFailures = 0
			current.RequiresSignIn = true
			msg := "unauthorized (refresh token invalid)"
			if current.RefreshError != nil && strings.TrimSpace(current.RefreshError.Message) != "" {
				msg = current.RefreshError.Message
			}
			current.LastError = &Error{Code: "unauthorized", Message: msg, HTTPStatus: http.StatusUnauthorized}
			m.auths[id] = current
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		m.queueRefreshUnschedule(id)
		if auth.RefreshError != nil {
			return nil, auth.RefreshError
		}
		return nil, &Error{Code: "unauthorized", Message: "unauthorized (refresh token invalid)", HTTPStatus: http.StatusUnauthorized}
	}

	base := auth.Clone()
	updated, err := exec.Refresh(ctx, base.Clone())
	if quotaRecovery != nil && err != nil {
		// Probe failures are committed by ApplyQuotaRefresh after its identity
		// and observation guards; they are not generation request failures.
		return nil, err
	}
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return nil, err
	}
	log.WithFields(log.Fields{"provider": auth.Provider, "auth_id": auth.ID, "status": statusCodeFromError(err), "success": err == nil}).Debug("credential refresh completed")
	now := time.Now()
	if err != nil {
		unauthorized := isUnauthorizedError(err)
		failureCode := autoOAuthFailureCode(err)
		shouldReschedule := false
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			if current.NextRefreshAfter.IsZero() || current.NextRefreshAfter.Before(now) {
				current.NextRefreshAfter = now.Add(time.Second)
			}
			m.auths[id] = current
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		m.queueRefreshReschedule(id)
		return nil, err
	}
	if err == nil {
		probe := updated
		if probe == nil {
			probe = base.Clone()
		} else {
			probe = probe.Clone()
		}
		applyPreservedRenewalState(base, probe, updateModeRefresh, now)
		if !secretGrantChanged(base, probe) && authHasUnchangedRejectedRefreshToken(probe) {
			if probe.RefreshError != nil {
				err = probe.RefreshError
			} else if base != nil && base.LastError != nil {
				err = base.LastError
			} else {
				err = &Error{Code: "unauthorized", Message: "unauthorized (refresh token invalid)", HTTPStatus: http.StatusUnauthorized}
			}
		}
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	if err != nil {
		unauthorized := isUnauthorizedError(err)
		invalidGrant := isInvalidGrantError(err)
		shouldReschedule := false
		isDisabled := false
		shouldUnschedule := false
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			if base != nil && (current.RegistrationEpoch != base.RegistrationEpoch || current.CredentialVersion != base.CredentialVersion || secretGrantChanged(base, current)) {
				m.mu.Unlock()
				return nil, err
			}
			if hasUnauthorizedAuthFailure(current) && !forceRefresh {
				m.mu.Unlock()
				return nil, err
			}
			terminalRefresh := isTerminalRefreshError(current, err)
			sanitizedRefreshErr := sanitizeRenewalError(current, err)
			wasTerminalUnauthorized := hasUnauthorizedAuthFailure(current)
			if wasTerminalUnauthorized {
				current.Generation++
				current.UpdatedAt = now
				current.Unavailable = true
				current.Status = StatusError
				current.NextRefreshAfter = time.Time{}
				current.NextRetryAfter = time.Time{}
				current.RefreshError = sanitizedRefreshErr
				if terminalRefresh || isInvalidGrantError(err) {
					current.RequiresSignIn = true
					recordRejectedRefreshTokens(current, err)
					if current.RenewalEventAt.IsZero() {
						current.RenewalEventAt = now
					}
					current.LastError = &Error{Code: "unauthorized", Message: sanitizedRefreshErr.Message, HTTPStatus: http.StatusUnauthorized}
					current.StatusMessage = "unauthorized (refresh token invalid)"
				}
				m.auths[id] = current
				if m.scheduler != nil {
					m.scheduler.upsertAuth(current.Clone())
				}
				m.mu.Unlock()
				m.queueRefreshUnschedule(id)
				return nil, err
			}
			current.Generation++
			current.UpdatedAt = now
			current.LastError = refreshErrorFromError(err)
			if unauthorized {
				m.maybeAutoDisableAuthLocked(current, current.LastError, false, now)
			}
			if failureCode != "" {
				if current.Metadata == nil {
					current.Metadata = make(map[string]any)
				}
				current.Metadata["codex_auto_oauth_recovery"] = map[string]any{
					"enabled":    true,
					"status":     "failed",
					"error_code": failureCode,
				}
				current.StatusMessage = autoOAuthFailureMessage(failureCode)
			}

			isDisabled = current.Disabled || current.Status == StatusDisabled
			hasValidAccessToken := current.HasValidAccessToken(now)
			// failedAccessToken is set only when upstream rejected this exact access
			// token. Its expiry time no longer proves it is usable.
			accessTokenRejected := failedAccessToken != "" && authMatchesTokenPartition(current, failedAccessToken)
			if isDisabled && invalidGrant {
				current.Unavailable = true
				current.Status = StatusDisabled
				current.NextRefreshAfter = time.Time{}
				current.RefreshFailures = 0
				current.StatusMessage = "disabled (invalid grant)"
				shouldUnschedule = true
			} else if isDisabled && terminalRefresh {
				current.Unavailable = true
				current.Status = StatusDisabled
				current.RefreshFailures = 0
				if current.StatusMessage == "" {
					current.StatusMessage = "disabled"
				}
				if !authHasUnchangedRejectedRefreshToken(current) {
					current.NextRefreshAfter = now.Add(refreshFailureBackoff)
					shouldReschedule = true
				} else if hasValidAccessToken && !accessTokenRejected {
					if exp, ok := current.AccessTokenExpirationTime(); ok && !exp.IsZero() && exp.After(now) {
						current.NextRefreshAfter = exp
						shouldReschedule = true
					} else {
						current.NextRefreshAfter = time.Time{}
						shouldUnschedule = true
					}
				} else {
					current.NextRefreshAfter = time.Time{}
					shouldUnschedule = true
				}
			} else if isDisabled {
				current.Unavailable = true
				current.Status = StatusDisabled
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
				if current.StatusMessage == "" {
					current.StatusMessage = "disabled"
				}
				shouldReschedule = true
			} else if accessTokenRejected && (invalidGrant || terminalRefresh) {
				// Neither token can recover without a new login. Stop selecting the
				// credential until its tokens change instead of retrying it after
				// every cooldown and returning the same 401 to clients.
				current.Unavailable = true
				current.Status = StatusError
				if unauthorized {
					current.NextRefreshAfter = time.Time{}
					if failureCode == "" {
						current.StatusMessage = "unauthorized"
					}
				} else {
					current.StatusMessage = "unauthorized (refresh token invalid)"
				}
				shouldUnschedule = true
			} else if !hasValidAccessToken || accessTokenRejected {
				current.Unavailable = true
				current.Status = StatusError
				if unauthorized || terminalRefresh {
					current.NextRefreshAfter = time.Time{}
					current.NextRetryAfter = time.Time{}
					current.RefreshFailures = 0
					current.LastError = &Error{Code: "unauthorized", Message: sanitizedRefreshErr.Message, HTTPStatus: http.StatusUnauthorized}
					if unauthorized {
						current.StatusMessage = "unauthorized"
					} else {
						current.StatusMessage = "unauthorized (refresh token invalid)"
					}
					shouldUnschedule = true
				} else if invalidGrant {
					current.RefreshFailures++
					current.NextRefreshAfter = now.Add(invalidGrantBackoffDuration(current.RefreshFailures))
					current.StatusMessage = "invalid grant (retrying)"
					shouldReschedule = true
				} else {
					current.RefreshFailures = 0
					current.NextRefreshAfter = now.Add(refreshFailureBackoff)
					if failureCode == "" {
						current.StatusMessage = "token expired"
					}
				}
			} else {
				// Access token remains valid. Preserve current in-flight/cooldown status without overwrite.
				nextRetry := now.Add(refreshFailureBackoff)
				if invalidGrant {
					current.RefreshFailures++
					nextRetry = now.Add(invalidGrantBackoffDuration(current.RefreshFailures))
				} else {
					current.RefreshFailures = 0
				}
				if exp, ok := current.AccessTokenExpirationTime(); ok && !exp.IsZero() && nextRetry.After(exp) {
					nextRetry = exp
				}
				current.NextRefreshAfter = nextRetry
				shouldReschedule = true

				if !current.Unavailable {
					log.Warnf("credential refresh failed for %s (%s): %s; retaining active credential as access token is unexpired", current.Provider, current.ID, sanitizedRefreshErr.Message)
				}
			}
			m.auths[id] = current
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		if shouldReschedule {
			m.queueRefreshReschedule(id)
		} else if shouldUnschedule {
			m.queueRefreshUnschedule(id)
		}
		return nil, err
	}
	if updated == nil {
		updated = base.Clone()
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.StatusMessage = ""
	updated.Unavailable = false
	if !updated.AutoDisabled && (updated.Status == StatusError || updated.Status == "") {
		updated.Status = StatusActive
	}
	updated.UpdatedAt = now
	if quotaRecovery == nil {
		_ = clearUnauthorizedModelStates(updated, now)
	} else {
		// Quota recovery refreshes credentials, not unrelated model cooldowns.
		// The normal three-way merge retains concurrent model-state changes.
		updated.ModelStates = base.ModelStates
	}
	if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	// A genuine OAuth refresh must persist rotated credentials, even when a
	// quota probe was canceled while credential acquisition completed. Detach
	// only the commit; the probe retains its cancellation and eligibility gates.
	commitCtx := ctx
	if quotaRecovery != nil {
		commitCtx = context.WithoutCancel(ctx)
	}
	saved, errUpdate := m.UpdateRefreshedAuth(commitCtx, base, updated)
	if errUpdate != nil {
		// Warn, not debug: a restart after a lost persist fails with invalid_grant.
		log.WithFields(log.Fields{"auth_id": auth.ID, "credential": auth.ID, "provider": auth.Provider}).Warnf("persist refreshed auth %s (%s) failed: %v", auth.Provider, auth.ID, errUpdate)
		return nil, errUpdate
	}
	if saved == nil {
		return nil, fmt.Errorf("auth %s not found", id)
	}
	targetAuth := saved
	supportedModels, regEpoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(id)
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(targetAuth, sm.ID, now))
	}
	if targetAuth != nil && len(projections) > 0 {
		registry.GetGlobalRegistry().ApplyClientModelProjections(id, regEpoch, targetAuth.Generation, projections)
	}
	return saved.Clone(), nil
}

func autoOAuthFailureCode(err error) string {
	if err == nil {
		return ""
	}
	type failureCoder interface {
		AutoOAuthFailureCode() string
	}
	var failure failureCoder
	if !errors.As(err, &failure) || failure == nil {
		return ""
	}
	return strings.TrimSpace(failure.AutoOAuthFailureCode())
}

func autoOAuthFailureMessage(code string) string {
	switch code {
	case "account_banned":
		return "账号被封禁"
	case "invalid_credentials":
		return "自动 OAuth 登录凭据无效，需要人工处理"
	case "manual_required":
		return "自动 OAuth 需要人工完成额外验证"
	case "workspace_mismatch":
		return "自动 OAuth 工作空间不匹配"
	default:
		return "自动 OAuth 找回失败"
	}
}

func autoOAuthFailureCodeFromMetadata(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	recovery, ok := auth.Metadata["codex_auto_oauth_recovery"].(map[string]any)
	if !ok || recovery["status"] != "failed" {
		return ""
	}
	code, _ := recovery["error_code"].(string)
	return strings.TrimSpace(code)
}

// ForceRefreshAuth triggers an immediate synchronous refresh for the credential.
func (m *Manager) ForceRefreshAuth(ctx context.Context, id string) (*Auth, error) {
	if m == nil {
		return nil, errors.New("auth manager is nil")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("auth id is empty")
	}
	return m.refreshAuthForRequest(withForceRefresh(ctx), id, "")
}

func (m *Manager) refreshWorkers() int {
	workers := refreshMaxConcurrency
	if m != nil {
		if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
			workers = cfg.AuthAutoRefreshWorkers
		}
	}
	return workers
}

// ForceRefreshResult records the outcome of a forced refresh for one credential.
type ForceRefreshResult struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// ForceRefreshAll triggers an immediate refresh for all credentials that have refresh tokens or custom refresh evaluators.
func (m *Manager) ForceRefreshAll(ctx context.Context) []ForceRefreshResult {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.auths))
	for id, auth := range m.auths {
		if auth != nil && !auth.Disabled && (authHasRefreshCredential(auth) || auth.Runtime != nil) {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()

	results := make([]ForceRefreshResult, len(ids))
	if len(ids) == 0 {
		return results
	}

	workers := m.refreshWorkers()
	if workers <= 0 {
		workers = 1
	}
	if workers > len(ids) {
		workers = len(ids)
	}

	type refreshJob struct {
		index  int
		authID string
	}

	jobCh := make(chan refreshJob, len(ids))
	for i, id := range ids {
		jobCh <- refreshJob{index: i, authID: id}
	}
	close(jobCh)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				if errCtx := ctx.Err(); errCtx != nil {
					results[job.index] = ForceRefreshResult{
						ID:      job.authID,
						Success: false,
						Error:   errCtx.Error(),
					}
					continue
				}

				_, err := m.ForceRefreshAuth(ctx, job.authID)
				res := ForceRefreshResult{ID: job.authID, Success: err == nil}
				if err != nil {
					res.Error = err.Error()
				}
				results[job.index] = res
			}
		}()
	}
	wg.Wait()
	return results
}
