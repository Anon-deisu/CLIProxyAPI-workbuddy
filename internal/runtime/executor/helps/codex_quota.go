package helps

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

const (
	codexRateLimitsEventType      = "codex.rate_limits"
	codexQuotaAdditionalHeaderKey = "X-Codex-Additional-"
	maxCodexAdditionalRateLimits  = 8
)

// ParseCodexQuotaEventHeaders converts one Codex websocket quota event into the
// same bounded header representation used by HTTP usage observations. Codex
// sends additional_rate_limits as an object on the websocket path, while the
// /wham/usage probe exposes the equivalent data as an array; both forms are
// normalized into namespaced X-Codex headers here.
//
// The websocket payload identifies each additional limit only by limit name
// ("GPT-5.3-Codex-Spark"), while HTTP responses namespace the same limit by a
// short name (x-codex-bengalfox-*). The two forms therefore cannot produce
// identical header names; the X-Codex-Additional- prefix marks limits parsed
// from JSON, and the snapshot-replacement semantics in QuotaState keep the two
// spellings from accumulating side by side.
func ParseCodexQuotaEventHeaders(payload []byte) http.Header {
	if len(payload) == 0 {
		return nil
	}

	eventKind := codexQuotaEventKind(payload)
	if eventKind == codexQuotaEventOther {
		return nil
	}
	root := ojson.ParseBytes(payload)
	if eventKind == codexQuotaEventError {
		return parseCodexQuotaHeadersObject(root.Get("headers"))
	}
	return parseCodexQuotaSnapshotHeaders(root, false)
}

// ParseCodexUsageQuotaHeaders converts a /backend-api/wham/usage response into
// quota observation headers. Missing or invalid windows return nil so callers
// can retain the previous snapshot instead of treating absent usage as zero.
func ParseCodexUsageQuotaHeaders(payload []byte) http.Header {
	if !ojson.ValidBytes(payload) {
		return nil
	}
	root := ojson.ParseBytes(payload)
	if !root.IsObject() {
		return nil
	}
	return parseCodexQuotaSnapshotHeaders(root, true)
}

func parseCodexQuotaSnapshotHeaders(root ojson.Result, requireWindow bool) http.Header {
	headers := make(http.Header)
	hasQuotaData := false

	baseRateLimits := firstCodexQuotaResult(root, "rate_limit", "rate_limits", "rateLimit")
	if addCodexQuotaRateLimitHeaders(headers, "X-Codex-", baseRateLimits, requireWindow) {
		// The main rate-limit object has no user-facing name in the websocket
		// payload, so its fields retain the HTTP response header names.
		hasQuotaData = true
	}

	additional := firstCodexQuotaResult(root, "additional_rate_limits", "additionalRateLimits")
	additionalCount := 0
	if additional.Exists() && (additional.IsObject() || additional.IsArray()) {
		additional.ForEach(func(key, value ojson.Result) bool {
			if additionalCount >= maxCodexAdditionalRateLimits {
				return false
			}
			limitName := strings.TrimSpace(key.String())
			if additional.IsArray() {
				limitName = firstCodexQuotaResultString(value, "limit_name", "limitName", "name")
			}
			if limitName == "" {
				return true
			}
			identifier := normalizeCodexQuotaHeaderIdentifier(limitName)
			if identifier == "" {
				return true
			}
			rateInfo := firstCodexQuotaResult(value, "rate_limit", "rateLimit")
			if !rateInfo.Exists() {
				rateInfo = value
			}
			prefix := codexQuotaAdditionalHeaderKey + identifier + "-"
			if addCodexQuotaRateLimitHeaders(headers, prefix, rateInfo, requireWindow) {
				// The limit name is upstream-controlled and lands in the
				// plain-text request log, so reject control characters here the
				// same way the plan type is validated below.
				if validCodexQuotaEventText(limitName) {
					headers.Set(prefix+"Limit-Name", strings.Clone(limitName))
				}
				hasQuotaData = true
				additionalCount++
			}
			return true
		})
	}

	codeReview := firstCodexQuotaResult(root, "code_review_rate_limit", "code_review_rate_limits", "codeReviewRateLimits")
	if addCodexQuotaRateLimitHeaders(headers, "X-Codex-Code-Review-", codeReview, requireWindow) {
		hasQuotaData = true
	}

	credits := firstCodexQuotaResult(root, "credits")
	if credits.Exists() && credits.IsObject() {
		hasCredits := setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Has-Credits", credits, "has_credits", "hasCredits")
		hasCredits = setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Unlimited", credits, "unlimited") || hasCredits
		hasCredits = setCodexQuotaScalarHeader(headers, "X-Codex-Credits-Balance", credits, "balance") || hasCredits
		if !requireWindow {
			hasQuotaData = hasQuotaData || hasCredits
		}
	}

	if !hasQuotaData {
		return nil
	}
	// A malformed active-limit name only invalidates that one header; the
	// window percentages collected above stay valid and must not be discarded.
	activeLimit := firstCodexQuotaResultString(root, "metered_limit_name", "meteredLimitName", "limit_name", "limitName")
	if validCodexQuotaEventIdentifier(activeLimit) {
		headers.Set("X-Codex-Active-Limit", activeLimit)
	}
	if planType := firstCodexQuotaResultString(root, "plan_type", "planType"); validCodexQuotaEventText(planType) {
		headers.Set("X-Codex-Plan-Type", planType)
	}
	return headers
}

const (
	codexQuotaEventOther = iota
	codexQuotaEventError
	codexQuotaEventRateLimits
)

// codexQuotaEventKind performs an allocation-free discriminator check for the
// common non-quota websocket event path. Codex frames carry the top-level
// type field within their opening bytes (after sequence_number at most), so
// only a bounded prefix is scanned instead of the whole frame; full JSON
// parsing is reserved for quota and error events.
func codexQuotaEventKind(payload []byte) int {
	const scanLimit = 256
	end := len(payload)
	if end > scanLimit {
		end = scanLimit
	}
	for i := 0; i+6 < end; i++ {
		if payload[i] != '"' || payload[i+1] != 't' || payload[i+2] != 'y' || payload[i+3] != 'p' || payload[i+4] != 'e' || payload[i+5] != '"' {
			continue
		}
		j := i + 6
		for j < end && (payload[j] == ' ' || payload[j] == '\t' || payload[j] == '\r' || payload[j] == '\n') {
			j++
		}
		if j >= end || payload[j] != ':' {
			continue
		}
		j++
		for j < end && (payload[j] == ' ' || payload[j] == '\t' || payload[j] == '\r' || payload[j] == '\n') {
			j++
		}
		if j+7 <= end && payload[j] == '"' && payload[j+1] == 'e' && payload[j+2] == 'r' && payload[j+3] == 'r' && payload[j+4] == 'o' && payload[j+5] == 'r' && payload[j+6] == '"' {
			return codexQuotaEventError
		}
		const rateLimits = `"codex.rate_limits"`
		if j+len(rateLimits) <= end {
			matched := true
			for offset := range rateLimits {
				if payload[j+offset] != rateLimits[offset] {
					matched = false
					break
				}
			}
			if matched {
				return codexQuotaEventRateLimits
			}
		}
	}
	return codexQuotaEventOther
}

func addCodexQuotaRateLimitHeaders(headers http.Header, prefix string, rateInfo ojson.Result, requireWindow bool) bool {
	if !rateInfo.Exists() || !rateInfo.IsObject() {
		return false
	}

	changed := false
	for _, windowName := range []string{"primary", "secondary"} {
		window := firstCodexQuotaResult(rateInfo, windowName+"_window", windowName)
		windowPrefix := prefix + strings.ToUpper(windowName[:1]) + windowName[1:] + "-"
		changed = addCodexQuotaWindowHeaders(headers, windowPrefix, window) || changed
	}
	if requireWindow && !changed {
		return false
	}
	if setCodexQuotaScalarHeaderFromResult(headers, prefix+"Allowed", rateInfo, "allowed") {
		changed = true
	}
	if setCodexQuotaScalarHeaderFromResult(headers, prefix+"Limit-Reached", rateInfo, "limit_reached", "limitReached") {
		changed = true
	}
	return changed
}

func addCodexQuotaWindowHeaders(headers http.Header, prefix string, window ojson.Result) bool {
	if !window.IsObject() {
		return false
	}
	used, hasUsed := codexQuotaNumber(firstCodexQuotaResult(window, "used_percent", "usedPercent"))
	minutes, hasMinutes := codexQuotaNumber(firstCodexQuotaResult(window, "window_minutes", "windowMinutes"))
	if !hasMinutes {
		seconds, hasSeconds := codexQuotaNumber(firstCodexQuotaResult(window, "limit_window_seconds"))
		minutes, hasMinutes = seconds/60, hasSeconds
	}
	resetAfter, hasResetAfter := codexQuotaNumber(firstCodexQuotaResult(window, "reset_after_seconds", "resetAfterSeconds"))
	resetAt, hasResetAt := codexQuotaNumber(firstCodexQuotaResult(window, "reset_at", "resetAt"))
	hasResetAfter = hasResetAfter && resetAfter >= 0 && resetAfter < 1<<63 && math.Trunc(resetAfter) == resetAfter
	hasResetAt = hasResetAt && resetAt > 0 && resetAt < 1<<63 && math.Trunc(resetAt) == resetAt
	if !hasUsed || used < 0 || used > 100 || !hasMinutes || minutes <= 0 || minutes >= 1<<63 || (!hasResetAfter && !hasResetAt) {
		return false
	}
	headers.Set(prefix+"Used-Percent", strconv.FormatFloat(used, 'f', -1, 64))
	headers.Set(prefix+"Window-Minutes", strconv.FormatFloat(minutes, 'f', -1, 64))
	if hasResetAfter {
		headers.Set(prefix+"Reset-After-Seconds", strconv.FormatFloat(resetAfter, 'f', -1, 64))
	}
	if hasResetAt {
		headers.Set(prefix+"Reset-At", strconv.FormatFloat(resetAt, 'f', -1, 64))
	}
	return true
}

func codexQuotaNumber(value ojson.Result) (float64, bool) {
	if value.Type != ojson.Number && value.Type != ojson.String {
		return 0, false
	}
	number, errParse := strconv.ParseFloat(strings.TrimSpace(value.String()), 64)
	return number, errParse == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func parseCodexQuotaHeadersObject(headersNode ojson.Result) http.Header {
	if !headersNode.Exists() || !headersNode.IsObject() {
		return nil
	}
	headers := make(http.Header)
	headersNode.ForEach(func(key, value ojson.Result) bool {
		name := http.CanonicalHeaderKey(strings.TrimSpace(key.String()))
		if !isCodexQuotaHeaderName(name) {
			return true
		}
		if raw := codexQuotaScalarValue(value); raw != "" {
			headers.Set(name, raw)
		}
		return true
	})
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func isCodexQuotaHeaderName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
		return true
	}
	if lower == "x-codex-active-limit" || lower == "x-codex-plan-type" ||
		strings.HasPrefix(lower, "x-codex-credits-") {
		return true
	}
	if !strings.HasPrefix(lower, "x-codex-") {
		return false
	}
	for _, marker := range []string{
		"-allowed",
		"-limit-reached",
		"-limit-name",
		"-used-percent",
		"-window-minutes",
		"-reset-after-seconds",
		"-reset-at",
		"-over-secondary-limit-percent",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func setCodexQuotaScalarHeader(headers http.Header, name string, object ojson.Result, paths ...string) bool {
	return setCodexQuotaScalarHeaderFromResult(headers, name, object, paths...)
}

func setCodexQuotaScalarHeaderFromResult(headers http.Header, name string, object ojson.Result, paths ...string) bool {
	value := firstCodexQuotaResult(object, paths...)
	if !value.Exists() {
		return false
	}
	raw := codexQuotaScalarValue(value)
	if !validCodexQuotaEventText(raw) {
		return false
	}
	headers.Set(name, raw)
	return true
}

func codexQuotaScalarValue(value ojson.Result) string {
	switch value.Type {
	case ojson.String:
		return strings.Clone(strings.TrimSpace(value.String()))
	case ojson.Number, ojson.True, ojson.False:
		return strings.Clone(strings.TrimSpace(value.Raw))
	default:
		return ""
	}
}

func firstCodexQuotaResult(object ojson.Result, paths ...string) ojson.Result {
	for _, path := range paths {
		value := object.Get(path)
		if value.Exists() && value.Type != ojson.Null {
			return value
		}
	}
	return ojson.Result{}
}

func firstCodexQuotaResultString(object ojson.Result, paths ...string) string {
	for _, path := range paths {
		value := firstCodexQuotaResult(object, path)
		if raw := codexQuotaScalarValue(value); raw != "" {
			return raw
		}
	}
	return ""
}

func normalizeCodexQuotaHeaderIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(value))
	lastDash := false
	for _, char := range value {
		switch {
		case (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_':
			builder.WriteRune(char)
			lastDash = false
		case char == '-':
			builder.WriteRune(char)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	identifier := strings.Trim(builder.String(), "-_.")
	if identifier == "" || !validCodexQuotaEventIdentifier(identifier) {
		return ""
	}
	return identifier
}

func validCodexQuotaEventIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func validCodexQuotaEventText(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n")
}
