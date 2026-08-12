package cliproxy

import (
	"context"
	"strings"
	"time"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// cursorModelFetchBackoff paces retries of the AvailableModels unary call. The
// call fails with a bare "unexpected EOF" often enough that a single attempt
// left accounts advertising only the builtin placeholders.
var cursorModelFetchBackoff = []time.Duration{500 * time.Millisecond, 2 * time.Second}

func (s *Service) fetchCursorModelsForAuth(ctx context.Context, auth *coreauth.Auth) []*registry.ModelInfo {
	if auth == nil {
		return nil
	}
	creds := cursorlib.CredentialsFromMetadata(auth.Metadata)
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		models, err := fetchCursorCatalogOnce(ctx, creds)
		if err == nil && len(models) > 0 {
			log.Infof("cursor: AvailableModels loaded %d models for auth %s", len(models), auth.ID)
			return cursorlib.CatalogToModelInfos(models)
		}
		if err == nil {
			log.Warnf("cursor: AvailableModels returned an empty catalog for auth %s", auth.ID)
			return nil
		}
		lastErr = err
		if attempt >= len(cursorModelFetchBackoff) || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(cursorModelFetchBackoff[attempt]):
		case <-ctx.Done():
			return nil
		}
	}
	log.WithError(lastErr).Warnf("cursor: AvailableModels fetch failed for auth %s after %d attempts",
		auth.ID, len(cursorModelFetchBackoff)+1)
	return nil
}

func fetchCursorCatalogOnce(ctx context.Context, creds cursorlib.AccountCredentials) ([]cursorlib.CatalogModel, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return cursorlib.FetchAvailableModels(fetchCtx, creds)
}

// cursorModelsOrPrevious keeps the catalog an auth is already advertising when a
// refresh cannot reach Cursor. Auth files are rewritten on every token refresh,
// each rewrite re-registers the account, and replacing a live 33-model catalog
// with the three builtin placeholders is far worse than serving a slightly
// stale list.
func cursorModelsOrPrevious(authID string, fetched []*registry.ModelInfo) []*registry.ModelInfo {
	if len(fetched) > 0 {
		return fetched
	}
	builtin := registry.GetCursorModels()
	previous := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(previous) > len(builtin) {
		log.Warnf("cursor: keeping the %d models already registered for auth %s", len(previous), authID)
		return previous
	}
	return builtin
}
