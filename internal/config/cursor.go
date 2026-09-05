package config

import "strings"

// DefaultCursorBaseURL is the canonical Cursor exchange endpoint used when a
// credential does not override base-url.
const DefaultCursorBaseURL = "https://api2.cursor.sh"

// NormalizeCursorBaseURL canonicalizes the default endpoint to an empty
// override so omitted and explicitly configured defaults produce one auth ID.
func NormalizeCursorBaseURL(raw string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	if strings.EqualFold(baseURL, DefaultCursorBaseURL) {
		return ""
	}
	return baseURL
}
