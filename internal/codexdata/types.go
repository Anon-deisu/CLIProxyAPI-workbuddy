package codexdata

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

// CodexTokenData holds the OAuth token information obtained from OpenAI.
type CodexTokenData struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Expire       string `json:"expired"`
	PlanType     string `json:"plan_type,omitempty"`
}

// CodexSIWCTokenData holds credentials obtained from the Sign in with ChatGPT flow.
type CodexSIWCTokenData struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	IDToken      string   `json:"id_token,omitempty"`
	ClientID     string   `json:"client_id"`
	Scope        string   `json:"scope,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	Expire       string   `json:"expired"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	LastRefresh  string   `json:"last_refresh"`
	DeviceID     string   `json:"device_id,omitempty"`
}

// ToMap converts SIWC token data into a map suitable for credential persistence.
func (d *CodexSIWCTokenData) ToMap() map[string]any {
	if d == nil {
		return nil
	}
	out := map[string]any{
		"access_token":  d.AccessToken,
		"refresh_token": d.RefreshToken,
		"client_id":     d.ClientID,
		"expired":       d.Expire,
		"last_refresh":  d.LastRefresh,
	}
	if d.ExpiresAt != "" {
		out["expires_at"] = d.ExpiresAt
	}
	if d.IDToken != "" {
		out["id_token"] = d.IDToken
	}
	if d.Scope != "" {
		out["scope"] = d.Scope
	}
	if len(d.Scopes) > 0 {
		out["scopes"] = append([]string(nil), d.Scopes...)
	}
	if d.DeviceID != "" {
		out["device_id"] = d.DeviceID
	}
	return out
}

// CodexAuthBundle aggregates token data and refresh information after OAuth completes.
type CodexAuthBundle struct {
	APIKey      string         `json:"api_key"`
	TokenData   CodexTokenData `json:"token_data"`
	LastRefresh string         `json:"last_refresh"`
}

// CodexTokenStorage stores OAuth2 token information for Codex authentication.
type CodexTokenStorage struct {
	IDToken      string         `json:"id_token"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	AccountID    string         `json:"account_id"`
	LastRefresh  string         `json:"last_refresh"`
	Email        string         `json:"email"`
	Type         string         `json:"type"`
	Expire       string         `json:"expired"`
	PlanType     string         `json:"plan_type,omitempty"`
	UseSIWC      *bool          `json:"use_siwc,omitempty"`
	SIWC         map[string]any `json:"siwc,omitempty"`

	ReplaceCredentials bool                    `json:"-"`
	RefreshPatch       *CredentialRefreshPatch `json:"-"`
	Metadata           map[string]any          `json:"-"`
}

// SetMetadata injects metadata into the storage before saving.
func (ts *CodexTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes token storage under a per-file and inter-process lock,
// merging an existing credential file before atomically replacing it.
func (ts *CodexTokenStorage) SaveTokenToFile(authFilePath string) error {
	if authFilePath != "" {
		fmt.Printf("Saving credentials to %s\n", filepath.Clean(authFilePath))
	}
	ts.Type = "codex"

	return WithFileLock(authFilePath, func() error {
		data := tokenStorageMap(ts, ts.Metadata)

		existingBytes, errRead := os.ReadFile(authFilePath)
		if errRead != nil && !os.IsNotExist(errRead) {
			return fmt.Errorf("failed to read existing credentials: %w", errRead)
		}
		if errRead == nil {
			diskMap, errUnmarshal := parseObject(existingBytes)
			if errUnmarshal != nil {
				return fmt.Errorf("invalid existing credential JSON")
			}
			if ts.RefreshPatch != nil {
				return WriteJSONFileAtomic(authFilePath, ts.RefreshPatch.Apply(diskMap))
			}
			merged := MergeWithDiskMap(diskMap, data)
			if ts.ReplaceCredentials {
				for _, key := range codexOuterCredentialKeys {
					if value, exists := data[key]; exists {
						merged[key] = value
					}
				}
			}
			data = merged
		}

		return WriteJSONFileAtomic(authFilePath, data)
	})
}

func tokenStorageMap(storage *CodexTokenStorage, metadata map[string]any) map[string]any {
	data := map[string]any{
		"id_token":      storage.IDToken,
		"access_token":  storage.AccessToken,
		"refresh_token": storage.RefreshToken,
		"account_id":    storage.AccountID,
		"last_refresh":  storage.LastRefresh,
		"email":         storage.Email,
		"type":          storage.Type,
		"expired":       storage.Expire,
	}
	if storage.PlanType != "" {
		data["plan_type"] = storage.PlanType
	}
	if storage.UseSIWC != nil {
		data["use_siwc"] = storage.UseSIWC
	}
	if storage.SIWC != nil {
		data["siwc"] = storage.SIWC
	}
	for key, value := range metadata {
		data[key] = value
	}
	return data
}

// WriteJSONFileAtomic writes data to a temporary file and atomically replaces the target.
func WriteJSONFileAtomic(authFilePath string, data map[string]any) error {
	dir := filepath.Dir(authFilePath)
	tmpFile, errTmp := os.CreateTemp(dir, filepath.Base(authFilePath)+".tmp.*")
	if errTmp != nil {
		return fmt.Errorf("failed to create temp credential file: %w", errTmp)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if errChmod := tmpFile.Chmod(0o600); errChmod != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to chmod temp credential file: %w", errChmod)
	}
	doc := ojson.NewDoc([]byte("{}"))
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := data[key]
		if errSet := doc.Set(ojson.Escape(key), value); errSet != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("failed to encode token data: %w", errSet)
		}
	}
	raw := doc.Bytes()
	if _, errWrite := tmpFile.Write(raw); errWrite != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write token to temp file: %w", errWrite)
	}
	if errClose := tmpFile.Close(); errClose != nil {
		log.Errorf("codex token storage: close temp token file error: %v", errClose)
		return fmt.Errorf("failed to close temp credential file: %w", errClose)
	}
	if errRename := os.Rename(tmpPath, authFilePath); errRename != nil {
		return fmt.Errorf("failed to replace credential file: %w", errRename)
	}
	cleanupTmp = false
	return nil
}

func parseObject(raw []byte) (map[string]any, error) {
	if !ojson.ValidBytes(raw) {
		return nil, fmt.Errorf("invalid JSON")
	}
	value := ojson.ParseBytes(raw).Value()
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("JSON value is not an object")
	}
	return object, nil
}

var codexOuterCredentialKeys = []string{
	"id_token",
	"access_token",
	"refresh_token",
	"account_id",
	"last_refresh",
	"email",
	"expired",
	"plan_type",
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

// MergeWithDiskMap preserves disk credentials when saving a non-refresh snapshot.
func MergeWithDiskMap(diskMap, incomingMap map[string]any) map[string]any {
	merged := cloneStringAnyMap(diskMap)
	if merged == nil {
		merged = make(map[string]any)
	}
	for key, value := range incomingMap {
		if value == nil {
			continue
		}
		if key == "siwc" || key == "use_siwc" {
			if _, exists := diskMap[key]; exists {
				continue
			}
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			if _, exists := diskMap[key]; exists {
				continue
			}
		}
		merged[key] = value
	}
	for _, key := range codexOuterCredentialKeys {
		if value, exists := diskMap[key]; exists {
			merged[key] = value
		} else if len(diskMap) > 0 {
			delete(merged, key)
		}
	}
	if _, exists := diskMap["siwc"]; !exists && len(diskMap) > 0 {
		delete(merged, "siwc")
	}
	return merged
}

func hasDiskOAuthOuter(diskMap map[string]any) bool {
	if len(diskMap) == 0 {
		return false
	}
	if refreshToken, ok := diskMap["refresh_token"].(string); ok && strings.TrimSpace(refreshToken) != "" {
		return true
	}
	if lastRefresh, ok := diskMap["last_refresh"].(string); ok && strings.TrimSpace(lastRefresh) != "" {
		return true
	}
	return false
}

// MergeCodexCredentialsFromDisk protects disk credentials during configuration saves.
func MergeCodexCredentialsFromDisk(diskMap, incomingMetadata map[string]any) map[string]any {
	merged := cloneStringAnyMap(incomingMetadata)
	if merged == nil {
		merged = make(map[string]any)
	}
	if hasDiskOAuthOuter(diskMap) {
		for _, key := range codexOuterCredentialKeys {
			if value, exists := diskMap[key]; exists {
				merged[key] = value
			} else {
				delete(merged, key)
			}
		}
	} else {
		for _, key := range codexOuterCredentialKeys {
			if value, exists := diskMap[key]; exists {
				if incoming, hadIncoming := merged[key]; !hadIncoming || incoming == nil || (func() bool {
					text, ok := incoming.(string)
					return ok && strings.TrimSpace(text) == ""
				}()) {
					merged[key] = value
				}
			}
		}
	}
	if value, exists := diskMap["siwc"]; exists {
		if siwcMap, ok := value.(map[string]any); ok {
			merged["siwc"] = cloneStringAnyMap(siwcMap)
		} else {
			merged["siwc"] = value
		}
	}
	if value, exists := diskMap["use_siwc"]; exists {
		if _, specified := incomingMetadata["use_siwc"]; !specified {
			merged["use_siwc"] = value
		}
	}
	return merged
}
