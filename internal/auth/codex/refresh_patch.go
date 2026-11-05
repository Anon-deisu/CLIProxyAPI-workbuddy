package codex

import "github.com/router-for-me/CLIProxyAPI/v8/internal/codexdata"

// CredentialRefreshPatch preserves the historical Codex package API.
type CredentialRefreshPatch = codexdata.CredentialRefreshPatch

// PartitionRefreshError preserves the historical Codex package API.
type PartitionRefreshError = codexdata.PartitionRefreshError

func NewCredentialRefreshPatch(before, after map[string]any, outer, siwc bool) *CredentialRefreshPatch {
	return codexdata.NewCredentialRefreshPatch(before, after, outer, siwc)
}

func NewCredentialRefreshPatchWithDiskBaseline(before, diskBefore, after map[string]any, outer, siwc bool) *CredentialRefreshPatch {
	return codexdata.NewCredentialRefreshPatchWithDiskBaseline(before, diskBefore, after, outer, siwc)
}

func IsCodexCredentialKey(key string) bool {
	return codexdata.IsCodexCredentialKey(key)
}

func NewPartitionRefreshError(outerErr, siwcErr error) error {
	return codexdata.NewPartitionRefreshError(outerErr, siwcErr)
}
