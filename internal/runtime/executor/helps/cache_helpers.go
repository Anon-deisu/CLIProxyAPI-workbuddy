package helps

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/google/uuid"
	homekv "github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type CodexCache struct {
	ID     string
	Expire time.Time
}

// codexCacheMap stores prompt cache IDs keyed by model+user_id.
// Protected by codexCacheMu. Entries expire after 1 hour.
var (
	codexCacheMap = make(map[string]CodexCache)
	codexCacheMu  sync.RWMutex
)

// codexCacheCleanupInterval controls how often expired entries are purged.
const codexCacheCleanupInterval = 15 * time.Minute

// codexCacheCleanupOnce ensures the background cleanup goroutine starts only once.
var codexCacheCleanupOnce sync.Once

// startCodexCacheCleanup launches a background goroutine that periodically
// removes expired entries from codexCacheMap to prevent memory leaks.
func startCodexCacheCleanup() {
	go func() {
		ticker := time.NewTicker(codexCacheCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredCodexCache()
		}
	}()
}

// purgeExpiredCodexCache removes entries that have expired.
func purgeExpiredCodexCache() {
	now := time.Now()
	codexCacheMu.Lock()
	defer codexCacheMu.Unlock()
	for key, cache := range codexCacheMap {
		if cache.Expire.Before(now) {
			delete(codexCacheMap, key)
		}
	}
}

// GetCodexCache retrieves a cached entry, returning ok=false if not found or expired.
func GetCodexCache(key string) (CodexCache, bool) {
	cache, ok, err := GetCodexCacheRequired(context.Background(), key)
	if err == nil {
		return cache, ok
	}
	return CodexCache{}, false
}

// GetCodexCacheRequired retrieves a cached entry for request-time paths.
func GetCodexCacheRequired(ctx context.Context, key string) (CodexCache, bool, error) {
	var homeCache CodexCache
	homeMode, found, errGet := homekv.KVGetJSONRequired(ctx, key, &homeCache)
	if homeMode {
		if errGet != nil || !found {
			return CodexCache{}, false, errGet
		}
		if homeCache.Expire.Before(time.Now()) {
			_, _, _ = homekv.KVDelRequired(ctx, key)
			return CodexCache{}, false, nil
		}
		return homeCache, true, nil
	}

	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	codexCacheMu.RLock()
	cache, ok := codexCacheMap[key]
	codexCacheMu.RUnlock()
	if !ok || cache.Expire.Before(time.Now()) {
		return CodexCache{}, false, nil
	}
	return cache, true, nil
}

// SetCodexCache stores a cache entry.
func SetCodexCache(key string, cache CodexCache) {
	SetCodexCacheBestEffort(context.Background(), key, cache)
}

// SetCodexCacheRequired stores a cache entry for request-time paths.
func SetCodexCacheRequired(ctx context.Context, key string, cache CodexCache) error {
	ttl := time.Until(cache.Expire)
	if ttl <= 0 {
		return nil
	}
	if _, homeMode, _ := homekv.CurrentKVClient(); homeMode {
		_, errSet := homekv.KVSetJSONRequired(ctx, key, cache, ttl)
		return errSet
	}
	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	codexCacheMu.Lock()
	codexCacheMap[key] = cache
	codexCacheMu.Unlock()
	return nil
}

// SetCodexCacheBestEffort stores a cache entry without failing completed responses.
func SetCodexCacheBestEffort(ctx context.Context, key string, cache CodexCache) bool {
	ttl := time.Until(cache.Expire)
	if ttl <= 0 {
		return false
	}
	if _, homeMode, _ := homekv.CurrentKVClient(); homeMode {
		return homekv.KVSetJSONBestEffort(ctx, key, cache, ttl)
	}
	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	codexCacheMu.Lock()
	codexCacheMap[key] = cache
	codexCacheMu.Unlock()
	return true
}

// CodexPromptCacheKey builds the Home KV key for a model/user prompt cache.
func CodexPromptCacheKey(modelName string, userScope string) string {
	return "cpa:codex:prompt-cache:" + homekv.HashKeyPart(modelName) + ":" + homekv.HashKeyPart(userScope)
}

// CodexFingerprint holds one request's account-scoped identity. Headers and body
// share it so that the turn ID and timestamp agree on both transports.
type CodexFingerprint struct {
	InstallationID string
	SessionID      string
	TurnID         string
	StartedAt      int64

	originalPromptCacheKey string
	originalMetadata       string
}

// NewCodexFingerprint follows sub2api's full convergence semantics. CPA's stable
// auth ID replaces a persisted random seed; tokens and caller IDs never enter it.
func NewCodexFingerprint(authID string) *CodexFingerprint {
	return &CodexFingerprint{
		InstallationID: codexFingerprintUUID(authID, "installation"),
		SessionID:      codexFingerprintUUID(authID, "session"),
		TurnID:         uuid.NewString(),
		StartedAt:      time.Now().UnixMilli(),
	}
}

func codexFingerprintUUID(authID, kind string) string {
	digest := sha256.Sum256([]byte("cli-proxy-api:codex:fingerprint:v1:" + kind + ":" + authID))
	id := uuid.UUID(digest[:16])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

// TurnMetadata preserves unrelated fields while normalizing embedded identity.
func (f *CodexFingerprint) TurnMetadata(raw string) string {
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		raw = "{}"
	}
	raw, _ = sjson.Set(raw, "installation_id", f.InstallationID)
	raw, _ = sjson.Set(raw, "session_id", f.SessionID)
	raw, _ = sjson.Set(raw, "thread_id", f.SessionID)
	raw, _ = sjson.Set(raw, "turn_id", f.TurnID)
	raw, _ = sjson.Set(raw, "window_id", f.SessionID+":0")
	raw, _ = sjson.Set(raw, "turn_started_at_unix_ms", f.StartedAt)
	return raw
}

// ApplyBody changes only client metadata. Explicit prompt cache keys remain
// client-owned so upstream prompt caching survives account fingerprint convergence.
// It never decodes or copies the full input tree.
func (f *CodexFingerprint) ApplyBody(body []byte) []byte {
	root := util.ParseGJSONBytesNoCopy(body)
	metadata := root.Get("client_metadata")
	f.originalMetadata = metadata.Raw
	raw := metadata.Raw
	if !metadata.IsObject() {
		raw = "{}"
	}
	raw, _ = sjson.Set(raw, "x-codex-installation-id", f.InstallationID)
	raw, _ = sjson.Set(raw, "session_id", f.SessionID)
	raw, _ = sjson.Set(raw, "thread_id", f.SessionID)
	raw, _ = sjson.Set(raw, "turn_id", f.TurnID)
	raw, _ = sjson.Set(raw, "x-codex-window-id", f.SessionID+":0")
	raw, _ = sjson.Set(raw, "x-codex-turn-metadata", f.TurnMetadata(metadata.Get("x-codex-turn-metadata").String()))
	body, _ = sjson.SetRawBytes(body, "client_metadata", []byte(raw))
	return body
}

// RestoreResponse restores echoed request metadata without replacing matching
// strings inside generated text, tool output, or unrelated response fields.
func (f *CodexFingerprint) RestoreResponse(payload []byte) []byte {
	for _, prefix := range []string{"", "response."} {
		if f.originalPromptCacheKey != "" && gjson.GetBytes(payload, prefix+"prompt_cache_key").String() == f.SessionID {
			payload, _ = sjson.SetBytes(payload, prefix+"prompt_cache_key", f.originalPromptCacheKey)
		}
		if gjson.GetBytes(payload, prefix+"client_metadata").Exists() {
			if f.originalMetadata == "" {
				payload, _ = sjson.DeleteBytes(payload, prefix+"client_metadata")
			} else {
				payload, _ = sjson.SetRawBytes(payload, prefix+"client_metadata", []byte(f.originalMetadata))
			}
		}
	}
	return payload
}
