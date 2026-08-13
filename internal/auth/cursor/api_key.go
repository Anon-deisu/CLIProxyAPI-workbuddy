package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const apiKeyExchangePath = "/auth/exchange_user_api_key"

type cachedAPIKeyToken struct {
	result    RefreshResult
	refreshAt time.Time
}

var (
	apiKeyExchangeService = NewAuthService()
	apiKeyExchangeGroup   singleflight.Group
	apiKeyTokenCache      = struct {
		sync.RWMutex
		entries map[string]cachedAPIKeyToken
	}{entries: make(map[string]cachedAPIKeyToken)}
)

// ExchangeAPIKey exchanges a Cursor user or service-account API key for the
// short-lived JWT accepted by Agent Connect.
func (s *AuthService) ExchangeAPIKey(ctx context.Context, apiKey, baseURL string) (*RefreshResult, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("cursor: API key is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+apiKeyExchangePath, strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("cursor API key exchange request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursor API key exchange: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("cursor API key exchange response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("cursor API key exchange failed: HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		AccessToken string `json:"accessToken"`
	}
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("cursor API key exchange decode: %w", err)
	}
	parsed.AccessToken = strings.TrimSpace(parsed.AccessToken)
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("cursor API key exchange response missing accessToken")
	}
	return &RefreshResult{
		AccessToken: parsed.AccessToken,
		ExpiresAt:   TokenExpiry(parsed.AccessToken),
	}, nil
}

// ResolveAPIKeyToken returns a cached Agent Connect JWT and re-exchanges the
// original API key before the JWT expires. The API key itself is never stored
// in the cache or used as the Agent Connect bearer token.
func ResolveAPIKeyToken(ctx context.Context, apiKey, baseURL string) (*RefreshResult, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("cursor: API key is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	cacheKey := apiKeyCacheKey(apiKey, baseURL)
	if cached, ok := loadCachedAPIKeyToken(cacheKey, time.Now()); ok {
		return &cached, nil
	}

	value, err, _ := apiKeyExchangeGroup.Do(cacheKey, func() (any, error) {
		if cached, ok := loadCachedAPIKeyToken(cacheKey, time.Now()); ok {
			return &cached, nil
		}
		result, errExchange := apiKeyExchangeService.ExchangeAPIKey(context.WithoutCancel(ctx), apiKey, baseURL)
		if errExchange != nil {
			return nil, errExchange
		}
		refreshAt := result.ExpiresAt
		if refreshAt.IsZero() {
			refreshAt = time.Now().Add(15 * time.Minute)
		}
		apiKeyTokenCache.Lock()
		apiKeyTokenCache.entries[cacheKey] = cachedAPIKeyToken{result: *result, refreshAt: refreshAt}
		apiKeyTokenCache.Unlock()
		copyResult := *result
		return &copyResult, nil
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*RefreshResult)
	if !ok || result == nil {
		return nil, fmt.Errorf("cursor API key exchange returned an invalid result")
	}
	return result, nil
}

func apiKeyCacheKey(apiKey, baseURL string) string {
	sum := sha256.Sum256([]byte(baseURL + "\x00" + apiKey))
	return hex.EncodeToString(sum[:])
}

func loadCachedAPIKeyToken(key string, now time.Time) (RefreshResult, bool) {
	apiKeyTokenCache.RLock()
	entry, ok := apiKeyTokenCache.entries[key]
	apiKeyTokenCache.RUnlock()
	if !ok || strings.TrimSpace(entry.result.AccessToken) == "" || !now.Before(entry.refreshAt) {
		return RefreshResult{}, false
	}
	return entry.result, true
}
