package util

import "strings"

// Cursor variant hyphen suffixes used by desktop / cursor2api. They are never
// public registry model IDs; strip them when looking up providers/auth shards.
var cursorVariantSuffixTokens = map[string]struct{}{
	"thinking": {},
	"fast":     {},
	"standard": {},
	"low":      {},
	"medium":   {},
	"high":     {},
	"xhigh":    {},
	"max":      {},
}

// RegistryModelID returns the model id that should be used for CPA registry /
// provider / auth lookup. Thinking parentheses suffixes and Cursor hyphen
// variant suffixes (e.g. -fast, -thinking-high) are stripped.
func RegistryModelID(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	return stripCursorVariantSuffix(stripModelValueSuffix(modelName))
}

func stripModelValueSuffix(modelName string) string {
	lastOpen := strings.LastIndex(modelName, "(")
	if lastOpen < 0 || !strings.HasSuffix(modelName, ")") {
		return modelName
	}

	suffix := strings.TrimSpace(modelName[lastOpen+1 : len(modelName)-1])
	if suffix == "" {
		return modelName
	}

	base := strings.TrimSpace(modelName[:lastOpen])
	if base == "" {
		return modelName
	}
	return base
}

func stripCursorVariantSuffix(requested string) string {
	parts := strings.Split(requested, "-")
	if len(parts) <= 1 {
		return requested
	}
	cut := len(parts)
	for cut > 1 {
		token := strings.ToLower(parts[cut-1])
		if _, ok := cursorVariantSuffixTokens[token]; !ok {
			break
		}
		cut--
	}
	if cut == len(parts) {
		return requested
	}
	base := strings.Join(parts[:cut], "-")
	if strings.TrimSpace(base) == "" {
		return requested
	}
	return base
}
