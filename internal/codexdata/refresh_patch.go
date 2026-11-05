package codexdata

import "reflect"

var codexSIWCCredentialKeys = []string{
	"access_token", "refresh_token", "id_token", "client_id", "device_id",
	"expired", "expires_at", "last_refresh", "scope", "scopes",
}

// CredentialRefreshPatch is an immutable, partition-specific refresh result.
type CredentialRefreshPatch struct {
	outerBefore          map[string]any
	outerDiskBefore      map[string]any
	outerAfter           map[string]any
	siwcBefore           map[string]any
	siwcDiskBefore       map[string]any
	siwcAfter            map[string]any
	baselineOuterRefresh string
	baselineSIWCRefresh  string
	upstreamRefreshed    bool
	outerErr             error
	siwcErr              error
}

// NewCredentialRefreshPatch records only credential partitions actually refreshed.
func NewCredentialRefreshPatch(before, after map[string]any, outer, siwc bool) *CredentialRefreshPatch {
	patch := NewCredentialRefreshPatchWithDiskBaseline(before, nil, after, outer, siwc)
	patch.upstreamRefreshed = outer || siwc
	return patch
}

// NewCredentialRefreshPatchWithDiskBaseline records the in-memory and on-disk baselines.
func NewCredentialRefreshPatchWithDiskBaseline(before, diskBefore, after map[string]any, outer, siwc bool) *CredentialRefreshPatch {
	patch := &CredentialRefreshPatch{
		upstreamRefreshed:    outer || siwc,
		baselineOuterRefresh: lookupRefreshTokenField(diskBefore, before, false),
		baselineSIWCRefresh:  lookupRefreshTokenField(diskBefore, before, true),
	}
	if outer {
		patch.outerBefore = credentialFields(before, codexOuterCredentialKeys)
		if len(diskBefore) > 0 {
			patch.outerDiskBefore = credentialFields(diskBefore, codexOuterCredentialKeys)
		}
		patch.outerAfter = credentialFields(after, codexOuterCredentialKeys)
	}
	if siwc {
		beforeSIWC, _ := before["siwc"].(map[string]any)
		diskBeforeSIWC, _ := diskBefore["siwc"].(map[string]any)
		afterSIWC, _ := after["siwc"].(map[string]any)
		patch.siwcBefore = credentialFields(beforeSIWC, codexSIWCCredentialKeys)
		if len(diskBeforeSIWC) > 0 {
			patch.siwcDiskBefore = credentialFields(diskBeforeSIWC, codexSIWCCredentialKeys)
		}
		patch.siwcAfter = credentialFields(afterSIWC, codexSIWCCredentialKeys)
	}
	return patch
}

func lookupRefreshTokenField(diskBefore, memBefore map[string]any, siwc bool) string {
	for _, src := range []map[string]any{diskBefore, memBefore} {
		if len(src) == 0 {
			continue
		}
		target := src
		if siwc {
			nested, ok := src["siwc"].(map[string]any)
			if !ok || len(nested) == 0 {
				continue
			}
			target = nested
		}
		if refreshToken, ok := target["refresh_token"].(string); ok && refreshToken != "" {
			return refreshToken
		}
	}
	return ""
}

func credentialFields(metadata map[string]any, keys []string) map[string]any {
	fields := make(map[string]any)
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			fields[key] = value
		}
	}
	return cloneStringAnyMap(fields)
}

func sameCredentialVersion(a, b map[string]any) bool {
	for _, key := range []string{"access_token", "refresh_token", "id_token", "account_id", "client_id", "device_id"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			return false
		}
	}
	return true
}

func matchesBaselineOrAfter(target, memBefore, diskBefore, after map[string]any) (matched bool, effectiveBefore map[string]any) {
	if len(diskBefore) > 0 && sameCredentialVersion(target, diskBefore) {
		return true, diskBefore
	}
	if sameCredentialVersion(target, memBefore) {
		return true, memBefore
	}
	if len(after) > 0 && sameCredentialVersion(target, after) {
		return true, after
	}
	return false, nil
}

func applyCredentialFields(disk, before, after map[string]any) {
	for key, value := range after {
		oldValue, existed := before[key]
		if !existed || !reflect.DeepEqual(oldValue, value) {
			disk[key] = value
		}
	}
	if _, hadExpiresAt := before["expires_at"]; hadExpiresAt {
		if _, hasExpiresAt := after["expires_at"]; !hasExpiresAt {
			delete(disk, "expires_at")
		}
	}
}

// IsCodexCredentialKey reports whether key belongs to a Codex credential partition or switch.
func IsCodexCredentialKey(key string) bool {
	if key == "siwc" || key == "use_siwc" {
		return true
	}
	for _, candidate := range codexOuterCredentialKeys {
		if candidate == key {
			return true
		}
	}
	return false
}

// RefreshedOuter reports whether p carries a refreshed outer credential partition.
func (p *CredentialRefreshPatch) RefreshedOuter() bool {
	return p != nil && p.outerAfter != nil
}

// RefreshedSIWC reports whether p carries a refreshed SIWC credential partition.
func (p *CredentialRefreshPatch) RefreshedSIWC() bool {
	return p != nil && p.siwcAfter != nil
}

// SetUpstreamRefreshed records whether an upstream token refresh actually succeeded.
func (p *CredentialRefreshPatch) SetUpstreamRefreshed(refreshed bool) {
	if p != nil {
		p.upstreamRefreshed = refreshed
	}
}

// UpstreamRefreshed reports whether p includes an upstream token refresh.
func (p *CredentialRefreshPatch) UpstreamRefreshed() bool {
	return p != nil && p.upstreamRefreshed
}

// SetPartitionErrors records per-partition refresh failures accompanying a partial refresh.
func (p *CredentialRefreshPatch) SetPartitionErrors(outerErr, siwcErr error) {
	if p == nil {
		return
	}
	p.outerErr = outerErr
	p.siwcErr = siwcErr
}

// OuterError returns an error encountered while refreshing the outer OAuth partition.
func (p *CredentialRefreshPatch) OuterError() error {
	if p == nil {
		return nil
	}
	return p.outerErr
}

// SIWCError returns an error encountered while refreshing the SIWC partition.
func (p *CredentialRefreshPatch) SIWCError() error {
	if p == nil {
		return nil
	}
	return p.siwcErr
}

// FailedOuterRefreshToken returns the baseline outer refresh token submitted on failure.
func (p *CredentialRefreshPatch) FailedOuterRefreshToken() string {
	if p == nil || p.outerErr == nil {
		return ""
	}
	return p.baselineOuterRefresh
}

// FailedSIWCRefreshToken returns the baseline SIWC refresh token submitted on failure.
func (p *CredentialRefreshPatch) FailedSIWCRefreshToken() string {
	if p == nil || p.siwcErr == nil {
		return ""
	}
	return p.baselineSIWCRefresh
}

// PartitionRefreshError carries per-partition refresh errors.
type PartitionRefreshError struct {
	outerErr error
	siwcErr  error
}

// NewPartitionRefreshError combines outer and SIWC refresh errors when present.
func NewPartitionRefreshError(outerErr, siwcErr error) error {
	if outerErr == nil && siwcErr == nil {
		return nil
	}
	if outerErr != nil && siwcErr == nil {
		return outerErr
	}
	if siwcErr != nil && outerErr == nil {
		return siwcErr
	}
	return &PartitionRefreshError{outerErr: outerErr, siwcErr: siwcErr}
}

func (e *PartitionRefreshError) Error() string {
	if e == nil {
		return ""
	}
	if e.outerErr != nil && e.siwcErr != nil {
		return e.outerErr.Error() + "; " + e.siwcErr.Error()
	}
	if e.outerErr != nil {
		return e.outerErr.Error()
	}
	if e.siwcErr != nil {
		return e.siwcErr.Error()
	}
	return ""
}

func (e *PartitionRefreshError) Unwrap() []error {
	if e == nil {
		return nil
	}
	var errs []error
	if e.outerErr != nil {
		errs = append(errs, e.outerErr)
	}
	if e.siwcErr != nil {
		errs = append(errs, e.siwcErr)
	}
	return errs
}

func (e *PartitionRefreshError) OuterRefreshError() error {
	if e == nil {
		return nil
	}
	return e.outerErr
}

func (e *PartitionRefreshError) SIWCRefreshError() error {
	if e == nil {
		return nil
	}
	return e.siwcErr
}

// CanApplyTo reports whether a refreshed partition still matches disk's version.
func (p *CredentialRefreshPatch) CanApplyTo(disk map[string]any) bool {
	if p == nil {
		return false
	}
	if p.outerAfter != nil {
		if ok, _ := matchesBaselineOrAfter(disk, p.outerBefore, p.outerDiskBefore, p.outerAfter); ok {
			return true
		}
	}
	diskSIWC, _ := disk["siwc"].(map[string]any)
	if p.siwcAfter != nil {
		if ok, _ := matchesBaselineOrAfter(diskSIWC, p.siwcBefore, p.siwcDiskBefore, p.siwcAfter); ok {
			return true
		}
	}
	return false
}

// Apply overlays successful refreshes on disk, retaining partitions rotated since refresh began.
func (p *CredentialRefreshPatch) Apply(disk map[string]any) map[string]any {
	merged := cloneStringAnyMap(disk)
	if merged == nil {
		merged = make(map[string]any)
	}
	if p.outerAfter != nil {
		if ok, base := matchesBaselineOrAfter(disk, p.outerBefore, p.outerDiskBefore, p.outerAfter); ok {
			applyCredentialFields(merged, base, p.outerAfter)
		}
	}
	diskSIWC, _ := disk["siwc"].(map[string]any)
	if p.siwcAfter != nil {
		if ok, base := matchesBaselineOrAfter(diskSIWC, p.siwcBefore, p.siwcDiskBefore, p.siwcAfter); ok {
			mergedSIWC := cloneStringAnyMap(diskSIWC)
			if mergedSIWC == nil {
				mergedSIWC = make(map[string]any)
			}
			applyCredentialFields(mergedSIWC, base, p.siwcAfter)
			merged["siwc"] = mergedSIWC
		}
	}
	return merged
}
