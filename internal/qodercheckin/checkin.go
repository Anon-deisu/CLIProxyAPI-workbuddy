// Package qodercheckin coordinates manual and automatic Qoder credit claims.
package qodercheckin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// All timestamps are Unix seconds. RoundKey stores the start of the successful
// round (02:00 UTC / 10:00 UTC+8), independently of the host's timezone.
const (
	AutoCheckInKey = "qoder_auto_checkin"
	LastSuccessKey = "qoder_checkin_last_success"
	RoundKey       = "qoder_checkin_round"
	LastAttemptKey = "qoder_checkin_last_attempt"
	LastResultKey  = "qoder_checkin_last_result"
	NextAttemptKey = "qoder_checkin_next_attempt"
	CompletedKey   = "qoder_checkin_completed"
	RetryBackoff   = time.Hour
)

type CheckInResult = qoder.CheckInResult

type accountLockKey struct {
	manager  *coreauth.Manager
	provider string
	account  string
}

type accountLock struct {
	gate chan struct{}
	refs int
}

var accountLocks = struct {
	sync.Mutex
	entries map[accountLockKey]*accountLock
}{entries: make(map[accountLockKey]*accountLock)}

// Reference counting covers both holders and waiters, including cancellations.
func acquire(ctx context.Context, key accountLockKey) (func(), error) {
	accountLocks.Lock()
	lock := accountLocks.entries[key]
	if lock == nil {
		lock = &accountLock{gate: make(chan struct{}, 1)}
		accountLocks.entries[key] = lock
	}
	lock.refs++
	accountLocks.Unlock()
	drop := func() {
		accountLocks.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(accountLocks.entries, key)
		}
		accountLocks.Unlock()
	}
	select {
	case lock.gate <- struct{}{}:
		return func() { <-lock.gate; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func lockKey(manager *coreauth.Manager, auth *coreauth.Auth) accountLockKey {
	uid := qoder.CredentialsFromAuth(auth).UID
	if uid == "" {
		uid = "id:" + auth.ID
	} else {
		uid = "uid:" + uid
	}
	return accountLockKey{manager, auth.Provider, uid}
}

func roundStart(now time.Time) int64 {
	const day, offset = int64(24 * 60 * 60), int64(2 * 60 * 60)
	shifted := now.Unix() - offset
	remainder := shifted % day
	if remainder < 0 {
		remainder += day
	}
	return shifted - remainder + offset
}

func metadataSeconds(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		if v > 0 && v < 253402300800 && v == float64(int64(v)) {
			return int64(v)
		}
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			return parsed.Unix()
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func supported(auth *coreauth.Auth) bool {
	return auth != nil && (auth.Provider == qoder.ProviderIntl || auth.Provider == qoder.ProviderCN)
}

func disabled(auth *coreauth.Auth) bool {
	return auth.IsDispatchDisabled()
}

var checkInZone = time.FixedZone("UTC+8", 8*60*60)

func automaticWindow(now time.Time) bool {
	return now.In(checkInZone).Hour() >= 10
}

func completed(auth *coreauth.Auth, now time.Time) bool {
	if metadataSeconds(auth.Metadata[LastSuccessKey]) <= 0 {
		return false
	}
	if round := metadataSeconds(auth.Metadata[RoundKey]); round > 0 {
		return round == roundStart(now)
	}
	return roundStart(time.Unix(metadataSeconds(auth.Metadata[LastSuccessKey]), 0)) == roundStart(now)
}

// Eligible is a cheap scheduler hint. Run always repeats these checks against
// the manager's current account after acquiring the shared account lock.
func Eligible(auth *coreauth.Auth, now time.Time) bool {
	return supported(auth) && !disabled(auth) && auth.Metadata[AutoCheckInKey] == true &&
		automaticWindow(now) && !completed(auth, now) && metadataSeconds(auth.Metadata[NextAttemptKey]) <= now.Unix()
}

// Run is the lifecycle entry point; automatic must be false for a manual request.
func Run(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, cfg *config.Config, automatic bool) (CheckInResult, error) {
	return RunCheckIn(ctx, manager, auth, cfg, automatic)
}

// RunCheckIn serializes all claims for an account across manual and automatic
// callers. Waiting is cancellable; callers must supply their request/lifecycle
// context. It persists metadata through UpdatePreparedAuth without quota probes.
func RunCheckIn(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, cfg *config.Config, automatic bool) (CheckInResult, error) {
	return run(ctx, manager, auth, cfg, automatic, time.Now)
}

func outcome(status, message string) CheckInResult {
	return CheckInResult{Status: status, Message: message}
}

func run(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, cfg *config.Config, automatic bool, now func() time.Time) (CheckInResult, error) {
	failed := outcome("error", "Qoder check-in failed")
	if err := ctx.Err(); err != nil {
		return failed, err
	}
	if manager == nil || !supported(auth) || auth.ID == "" {
		return failed, errors.New("qoder: check-in requires a registered Qoder account")
	}
	key := lockKey(manager, auth)
	release, errLock := acquire(ctx, key)
	if errLock != nil {
		return failed, errLock
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return failed, err
	}
	live, ok := manager.GetByID(auth.ID)
	if !ok || !supported(live) || lockKey(manager, live) != key || live.RegistrationEpoch != auth.RegistrationEpoch {
		return failed, errors.New("qoder: check-in account was removed or replaced")
	}
	if disabled(live) {
		return outcome("skipped", "Qoder account is disabled"), nil
	}
	if automatic && live.Metadata[AutoCheckInKey] != true {
		return outcome("skipped", "Automatic Qoder check-in is disabled"), nil
	}
	started := now()
	if automatic && !automaticWindow(started) {
		return outcome("skipped", "Automatic Qoder check-in starts at 10:00 UTC+8"), nil
	}
	// Duplicate auth files for the same regional UID share the lock and confirmed
	// completion. Replayed rewards are never reported as newly earned credits.
	var waiting *coreauth.Auth
	for _, candidate := range manager.List() {
		if !supported(candidate) || lockKey(manager, candidate) != key {
			continue
		}
		if completed(candidate, started) {
			result := outcome("already", "Qoder check-in already completed for this 10:00 UTC+8 round")
			result.Already = int(metadataSeconds(candidate.Metadata[CompletedKey]))
			if result.Already < 1 {
				result.Already = 1
			}
			return result, nil
		}
		if metadataSeconds(candidate.Metadata[NextAttemptKey]) > started.Unix() {
			waiting = candidate
		}
	}
	if waiting != nil {
		return retryOutcome(waiting)
	}
	prepared, errPrepare := prepare(ctx, manager, live, cfg, now)
	if errPrepare != nil {
		if current, exists := manager.GetByID(auth.ID); exists && current.RegistrationEpoch == auth.RegistrationEpoch &&
			lockKey(manager, current) == key && ctx.Err() == nil && (disabled(current) || automatic && current.Metadata[AutoCheckInKey] != true) {
			return outcome("skipped", "Qoder check-in was disabled before claiming"), nil
		}
		err := errors.New("qoder: check-in token refresh failed; sign in again")
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return persistOutcome(ctx, manager, live, nil, failed, err, started, now())
	}
	// A user may disable/remove the account while credentials are being refreshed.
	live, ok = manager.GetByID(auth.ID)
	if !ok || prepared == nil || live.RegistrationEpoch != auth.RegistrationEpoch || lockKey(manager, live) != key {
		return failed, errors.New("qoder: check-in account was removed or replaced")
	}
	if disabled(live) || (automatic && live.Metadata[AutoCheckInKey] != true) {
		return outcome("skipped", "Qoder check-in was disabled before claiming"), nil
	}
	ident := qoder.MachineIdentityFromAuth(live)
	if ident == nil {
		ident, _ = qoder.NativeMachineIdentity(ctx, live.Provider, qoder.CredentialsFromAuth(live).UID)
	}
	// Re-read after native sampling, which may take several seconds.
	latest, exists := manager.GetByID(live.ID)
	if !exists || latest.RegistrationEpoch != live.RegistrationEpoch || lockKey(manager, latest) != key {
		return failed, errors.New("qoder: check-in account was removed or replaced")
	}
	if disabled(latest) || (automatic && latest.Metadata[AutoCheckInKey] != true) {
		return outcome("skipped", "Qoder check-in was disabled before claiming"), nil
	}
	if currentIdentity := qoder.MachineIdentityFromAuth(latest); currentIdentity != nil {
		ident = currentIdentity
	}
	live = latest
	service := qoder.NewService(live.Provider, helps.NewProxyAwareHTTPClient(ctx, cfg, live, 0))
	service.SetEndpoint(qoder.EndpointForAuth(live))
	input := ident
	if input == nil {
		// Sampling was already attempted; CN can proceed without machine headers.
		input = &qoder.MachineIdentity{}
	}
	result, errCheckIn := service.CheckIn(ctx, qoder.CredentialsFromAuth(live), input)
	return persistOutcome(ctx, manager, live, ident, result, errCheckIn, started, now())
}

func retryOutcome(auth *coreauth.Auth) (CheckInResult, error) {
	// Metadata may be user-edited: never echo its message or past earned credits.
	last, _ := auth.Metadata[LastResultKey].(map[string]any)
	lastStatus, _ := last["status"].(string)
	switch lastStatus {
	case "error":
		return outcome("error", "Previous Qoder check-in failed; waiting for its next retry"), errors.New("qoder: check-in retry is deferred")
	case "blocked":
		return outcome("blocked", "Qoder blocked the previous claim; waiting for its next retry"), nil
	case "unavailable":
		return outcome("unavailable", "Qoder check-in is unavailable; verify native identity or regional API availability before the next retry"), nil
	default:
		return outcome("skipped", "Qoder check-in is waiting for its next retry"), nil
	}
}

type checkInPreparer struct {
	cfg     *config.Config
	now     func() time.Time
	manager *coreauth.Manager
	base    *coreauth.Auth
}

func (p checkInPreparer) ShouldPrepareRequestAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	expiry := qoder.CredentialsFromAuth(auth).ExpiresAt
	return expiry > 0 && expiry <= p.now().Add(qoder.RefreshLead).Unix()
}

func (p checkInPreparer) PrepareRequestAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, exists := p.manager.GetByID(p.base.ID)
	if !exists || auth.RegistrationEpoch != p.base.RegistrationEpoch || current.RegistrationEpoch != p.base.RegistrationEpoch ||
		lockKey(p.manager, auth) != lockKey(p.manager, p.base) || lockKey(p.manager, current) != lockKey(p.manager, p.base) || disabled(current) {
		return nil, errors.New("qoder: check-in account changed before refresh")
	}
	return executor.NewQoderExecutor(p.cfg, auth.Provider).Refresh(ctx, auth)
}

func prepare(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, cfg *config.Config, now func() time.Time) (*coreauth.Auth, error) {
	// Manager's preparation mutex is also used by quota requests and is not
	// cancellable. Keep that wait off the caller; the preparer checks cancellation
	// before it can issue a refresh after eventually acquiring the manager lock.
	type preparedAuth struct {
		auth *coreauth.Auth
		err  error
	}
	done := make(chan preparedAuth, 1)
	go func() {
		prepared, err := manager.PrepareRequestAuth(ctx, checkInPreparer{cfg: cfg, now: now, manager: manager, base: auth}, auth)
		done <- preparedAuth{prepared, err}
	}()
	select {
	case result := <-done:
		return result.auth, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func persistOutcome(ctx context.Context, manager *coreauth.Manager, base *coreauth.Auth, ident *qoder.MachineIdentity, result CheckInResult, err error, started, finished time.Time) (CheckInResult, error) {
	success := err == nil && (result.Status == "success" || result.Status == "already") && result.Claimed+result.Already > 0
	updated := base.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata[LastAttemptKey] = started.Unix()
	updated.Metadata[LastResultKey] = map[string]any{
		"status": result.Status, "claimed": result.Claimed, "already": result.Already,
		"credits": result.Credits, "message": result.Message,
	}
	if ident != nil {
		// Store a JSON object rather than a pointer shared by Auth.Clone callers.
		updated.Metadata[qoder.MachineIdentityMetadataKey] = map[string]any{
			"machineToken": ident.MachineToken, "machineType": ident.MachineType,
			"machineCode": ident.MachineCode, "machineOS": ident.MachineOS, "hostname": ident.Hostname,
		}
	}
	if success {
		updated.Metadata[LastSuccessKey] = finished.Unix()
		updated.Metadata[RoundKey] = roundStart(started)
		updated.Metadata[CompletedKey] = result.Claimed + result.Already
		updated.Metadata[NextAttemptKey] = roundStart(started) + int64(24*time.Hour/time.Second)
	} else {
		next := finished.Add(RetryBackoff).Unix()
		if finished.Nanosecond() > 0 {
			next++
		}
		if existing := metadataSeconds(base.Metadata[NextAttemptKey]); existing > next {
			next = existing
		}
		updated.Metadata[NextAttemptKey] = next
	}
	// Preserve a confirmed claim or failed attempt even if its caller
	// disconnects. Only metadata deltas participate in the manager's atomic merge.
	saved, errUpdate := manager.UpdatePreparedAuth(context.WithoutCancel(ctx), base, updated)
	if errUpdate != nil || saved == nil {
		result.Status, result.Message = "error", "Qoder check-in state could not be saved"
		if err != nil {
			return result, err
		}
		return result, errors.New("qoder: could not save check-in state")
	}
	return result, err
}
