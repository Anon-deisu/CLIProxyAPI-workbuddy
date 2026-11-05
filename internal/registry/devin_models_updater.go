package registry

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

const maxDevinModelsSize = 8 << 20

var devinModelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/devin_models.json",
	"https://models.router-for.me/devin_models.json",
}

// StartDevinModelsUpdater starts a background updater that fetches the
// Devin model catalog immediately and refreshes it every 3 hours.
// Safe to call multiple times; only one updater runs.
func StartDevinModelsUpdater(ctx context.Context) {
	startLegacyCatalogUpdater(ctx, "devin-catalog")
}

func runDevinModelsUpdater(ctx context.Context) {
	tryRefreshDevinModels(ctx, "startup Devin model refresh")

	ticker := time.NewTicker(ModelsRefreshInterval)
	defer ticker.Stop()
	log.Infof("periodic Devin model refresh started (interval=%s)", ModelsRefreshInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tryRefreshDevinModels(ctx, "periodic Devin model refresh")
		}
	}
}

func tryRefreshDevinModels(ctx context.Context, label string) {
	data, sourceURL := fetchDevinModelsFromRemote(ctx)
	if data == nil {
		log.Warnf("%s: fetch failed from all URLs, keeping current data (embedded or cached fallback)", label)
		return
	}

	changed, err := loadDevinModelsFromBytes(data, sourceURL)
	if err != nil {
		log.Warnf("%s: fetched catalog rejected, keeping current data: %v", label, err)
		return
	}
	if !changed {
		log.Infof("%s completed from %s, no changes detected", label, sourceURL)
		return
	}
	log.Infof("%s completed from %s, catalog updated", label, sourceURL)
}

func fetchDevinModelsFromRemote(ctx context.Context) ([]byte, string) {
	for _, sourceURL := range devinModelsURLs {
		data, errRead := readCatalogSource(ctx, sourceURL)
		if errRead == nil && validateDevinCatalogBytes(data) == nil {
			return data, sourceURL
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, ""
}
