package qoder

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
)

type Quota struct {
	Plan     string         `json:"plan"`
	Credits  *CreditQuota   `json:"credits"`
	Packages []QuotaPackage `json:"packages"`
	Shared   *SharedQuota   `json:"shared"`
	Realm    string         `json:"realm"`
}

type CreditQuota struct {
	Total            float64 `json:"total"`
	Used             float64 `json:"used"`
	Remaining        float64 `json:"remaining"`
	RemainingPercent float64 `json:"remaining_percent"`
}

type QuotaPackage struct {
	CreditQuota
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	ExpiresAt   int64    `json:"expires_at,omitempty"`
	DaysLeft    *float64 `json:"days_left,omitempty"`
	NoExpiry    *bool    `json:"no_expiry,omitempty"`
	PackageCode string   `json:"package_code,omitempty"`
}

// SharedQuota is an organization pool, never included in the account's Credits.
// With Unlimited=true, Remaining is an upstream placeholder, not a finite balance.
type SharedQuota struct {
	QuotaPackage
	Cap              float64 `json:"cap"`
	Available        bool    `json:"available"`
	Unlimited        bool    `json:"unlimited"`
	OrganizationID   string  `json:"organization_id,omitempty"`
	OrganizationName string  `json:"organization_name,omitempty"`
}

// FetchQuota reads the root objects returned by the v2 quota and plan endpoints.
// A supplied plan is retained; otherwise the plan endpoint must return a real name.
func (s *Service) FetchQuota(ctx context.Context, creds Credentials, plan string) (Quota, error) {
	if creds.AccessToken == "" {
		return Quota{}, fmt.Errorf("qoder: quota requires an access token")
	}
	if creds.Realm != "" && creds.Realm != s.site.Realm {
		return Quota{}, fmt.Errorf("qoder: credential region does not match provider")
	}
	data, _, err := s.call(ctx, http.MethodGet, "/api/v2/quota/usage", nil, apiHeaders(s.site.Provider, creds), false)
	if err != nil {
		return Quota{}, err
	}
	quota := Quota{Plan: strings.TrimSpace(plan), Realm: s.site.Realm, Credits: &CreditQuota{}, Packages: make([]QuotaPackage, 0, 2)}
	for _, pool := range []struct{ key, name string }{{"userQuota", "Base credits"}, {"addOnQuota", "Add-on credits"}} {
		raw, exists := data[pool.key]
		if !exists {
			continue
		}
		src, ok := raw.(map[string]any)
		if !ok {
			return Quota{}, fmt.Errorf("qoder: invalid account quota object")
		}
		pack, errPackage := parseQuotaPackage(src, pool.key, pool.name, false)
		if errPackage != nil {
			return Quota{}, errPackage
		}
		quota.Packages = append(quota.Packages, pack)
		quota.Credits.Total += pack.Total
		quota.Credits.Used += pack.Used
		quota.Credits.Remaining += pack.Remaining
	}
	if len(quota.Packages) == 0 {
		return Quota{}, fmt.Errorf("qoder: missing account quota objects")
	}
	if math.IsInf(quota.Credits.Total, 0) || math.IsInf(quota.Credits.Used, 0) || math.IsInf(quota.Credits.Remaining, 0) {
		return Quota{}, fmt.Errorf("qoder: invalid aggregate quota")
	}
	exceeded := false
	if value, exists := data["isQuotaExceeded"]; exists {
		var ok bool
		exceeded, ok = quotaBool(value)
		if !ok {
			return Quota{}, fmt.Errorf("qoder: invalid quota exceeded flag")
		}
	}
	if exceeded && quota.Credits.Total == 0 && quota.Credits.Remaining == 0 {
		return Quota{}, fmt.Errorf("qoder: degraded quota response (exceeded with zero capacity)")
	}
	quota.Credits.RemainingPercent = remainingPercent(quota.Credits.Total, quota.Credits.Remaining)
	if raw := data["orgResourcePackage"]; raw != nil {
		src, ok := raw.(map[string]any)
		if !ok {
			return Quota{}, fmt.Errorf("qoder: invalid shared quota object")
		}
		pack, errPackage := parseQuotaPackage(src, "orgResourcePackage", "Organization credits", true)
		if errPackage != nil {
			return Quota{}, errPackage
		}
		available := true
		if value, exists := src["available"]; exists {
			available, ok = quotaBool(value)
			if !ok {
				return Quota{}, fmt.Errorf("qoder: invalid shared quota availability")
			}
		}
		quota.Shared = &SharedQuota{QuotaPackage: pack, Cap: pack.Total, Available: available, Unlimited: pack.Total == -1,
			OrganizationID: creds.OrganizationID, OrganizationName: creds.OrganizationName}
	}
	// Top-level expiresAt has no established package or reset meaning upstream.
	if quota.Plan == "" {
		info, _, errPlan := s.call(ctx, http.MethodGet, "/api/v2/user/plan", nil, apiHeaders(s.site.Provider, creds), false)
		if errPlan != nil {
			return Quota{}, errPlan
		}
		quota.Plan = firstString(info, "plan_tier_name", "user_type")
		if quota.Plan == "" {
			return Quota{}, fmt.Errorf("qoder: missing account plan")
		}
	}
	return quota, nil
}

func parseQuotaPackage(src map[string]any, source, name string, shared bool) (QuotaPackage, error) {
	capacityKey := "total"
	if _, exists := src["cap"]; shared && exists {
		capacityKey = "cap"
	}
	total, okTotal := finiteNumber(src[capacityKey])
	used, okUsed := finiteNumber(src["used"])
	remaining, okRemaining := finiteNumber(src["remaining"])
	if !okTotal || !okUsed || !okRemaining || used < 0 || remaining < 0 || (total < 0 && !(shared && capacityKey == "cap" && total == -1)) {
		return QuotaPackage{}, fmt.Errorf("qoder: missing or invalid quota capacity, usage or balance")
	}
	pack := QuotaPackage{CreditQuota: CreditQuota{Total: total, Used: used, Remaining: remaining, RemainingPercent: remainingPercent(total, remaining)}, Name: name, Source: source}
	for _, key := range []string{"expiresAt", "expireAt", "expires_at"} {
		if value := src[key]; value != nil {
			// Only package-local expiry is copied; it is not a billing reset.
			pack.ExpiresAt, _ = absoluteExpiry(value)
			break
		}
	}
	for _, key := range []string{"daysLeft", "days_left"} {
		if value := src[key]; value != nil {
			days, ok := finiteNumber(value)
			if !ok {
				return QuotaPackage{}, fmt.Errorf("qoder: invalid quota expiry interval")
			}
			pack.DaysLeft = &days
			break
		}
	}
	if value, exists := src["noExpiry"]; exists {
		noExpiry, ok := quotaBool(value)
		if !ok {
			return QuotaPackage{}, fmt.Errorf("qoder: invalid quota expiry flag")
		}
		pack.NoExpiry = &noExpiry
	}
	pack.PackageCode = firstString(src, "packageCode", "package_code")
	return pack, nil
}

func remainingPercent(total, remaining float64) float64 {
	if total <= 0 {
		return 0
	}
	return min(100, remaining/total*100)
}

func quotaBool(value any) (bool, bool) {
	switch v := value.(type) {
	case nil:
		return false, true
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off", "", "null", "none":
			return false, true
		}
	default:
		if n, ok := finiteNumber(value); ok && (n == 0 || n == 1) {
			return n == 1, true
		}
	}
	return false, false
}
