package codex

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexdata"
)

// CodexTokenStorage stores Codex credentials while preserving the historical package API.
type CodexTokenStorage = codexdata.CodexTokenStorage

// WithFileLock executes fn while holding the shared Codex credential file lock.
func WithFileLock(authFilePath string, fn func() error) error {
	return codexdata.WithFileLock(authFilePath, fn)
}

// WriteJSONFileAtomic atomically replaces a Codex credential file.
func WriteJSONFileAtomic(authFilePath string, data map[string]any) error {
	return codexdata.WriteJSONFileAtomic(authFilePath, data)
}

// MergeWithDiskMap preserves credentials already stored on disk.
func MergeWithDiskMap(diskMap, incomingMap map[string]any) map[string]any {
	return codexdata.MergeWithDiskMap(diskMap, incomingMap)
}

// MergeCodexCredentialsFromDisk protects credential fields during config saves.
func MergeCodexCredentialsFromDisk(diskMap, incomingMetadata map[string]any) map[string]any {
	return codexdata.MergeCodexCredentialsFromDisk(diskMap, incomingMetadata)
}

func cloneStringAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		switch typed := value.(type) {
		case map[string]any:
			dst[key] = cloneStringAnyMap(typed)
		case []string:
			dst[key] = append([]string(nil), typed...)
		case []any:
			dst[key] = append([]any(nil), typed...)
		default:
			dst[key] = value
		}
	}
	return dst
}
