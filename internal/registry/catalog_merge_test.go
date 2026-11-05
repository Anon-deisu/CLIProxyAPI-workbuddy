package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

func TestCatalogPublicationPreservesLocalProviders(t *testing.T) {
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})
	local := *original
	local.Cursor = []*ModelInfo{{ID: "local-cursor"}}
	local.Devin = []*ModelInfo{{ID: "devin/local"}}
	local.Meta = []*ModelInfo{{ID: "local-muse"}}
	for _, empty := range []string{"omitted", "null", "array"} {
		t.Run(empty, func(t *testing.T) {
			modelsCatalogStore.mu.Lock()
			modelsCatalogStore.data = &local
			modelsCatalogStore.mu.Unlock()
			doc := ojson.NewDoc(embeddedModelsJSON)
			for _, provider := range []string{"cursor", "devin", "meta"} {
				var err error
				switch empty {
				case "omitted":
					err = doc.Delete(provider)
				case "null":
					err = doc.Set(provider, nil)
				case "array":
					err = doc.Set(provider, []any{})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			changed, err := publishCatalogBytes(doc.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			got := getModels()
			if !reflect.DeepEqual(got.Cursor, local.Cursor) || !reflect.DeepEqual(got.Devin, local.Devin) || !reflect.DeepEqual(got.Meta, local.Meta) {
				t.Fatal("publication discarded a local provider")
			}
			for _, provider := range []string{"cursor", "devin", "meta"} {
				if slices.Contains(changed, provider) {
					t.Fatalf("unchanged local provider %s reported as changed", provider)
				}
			}
		})
	}
}

func TestCatalogPublicationRejectsInvalidLocalProviders(t *testing.T) {
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})
	for _, provider := range []string{"meta", "devin"} {
		for name, raw := range map[string]string{"null model": `[null]`, "empty ID": `[{"id":" "}]`, "duplicate ID": `[{"id":"same"},{"id":"same"}]`} {
			t.Run(provider+"/"+name, func(t *testing.T) {
				doc := ojson.NewDoc(embeddedModelsJSON)
				if err := doc.SetRaw(provider, raw); err != nil {
					t.Fatal(err)
				}
				data := doc.Bytes()
				if err := validateCatalogBytes(data); err == nil {
					t.Fatal("invalid provider accepted by validation")
				}
				if _, err := publishCatalogBytes(data); err == nil || getModels() != original {
					t.Fatal("invalid provider replaced the last valid catalog")
				}
			})
		}
	}
}

func TestLegacyCatalogEntrypointsRespectServicePolicy(t *testing.T) {
	oldGeneral, oldCodex, oldDevin := generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater
	catalogRuntime.Lock()
	oldCtx, oldLocal, oldSources, oldHome := catalogRuntime.ctx, catalogRuntime.local, catalogRuntime.sources, catalogRuntime.home
	catalogRuntime.ctx, catalogRuntime.local, catalogRuntime.sources, catalogRuntime.home = nil, false, CatalogSources{}, false
	catalogRuntime.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		for _, updater := range []*catalogUpdater{generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater} {
			updater.mu.Lock()
			if updater.cancel != nil {
				updater.cancel()
			}
			updater.mu.Unlock()
		}
		generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater = oldGeneral, oldCodex, oldDevin
		catalogRuntime.Lock()
		catalogRuntime.ctx, catalogRuntime.local, catalogRuntime.sources, catalogRuntime.home = oldCtx, oldLocal, oldSources, oldHome
		catalogRuntime.Unlock()
	})
	newUpdater := func() (*catalogUpdater, <-chan string) {
		published := make(chan string, 8)
		return &catalogUpdater{
			fetch:   func(_ context.Context, source string) ([]byte, error) { return []byte(source), nil },
			publish: func(data []byte) ([]string, error) { published <- string(data); return nil, nil },
		}, published
	}
	var general, codex, devin <-chan string
	generalCatalogUpdater, general = newUpdater()
	codexCatalogUpdater, codex = newUpdater()
	devinCatalogUpdater, devin = newUpdater()
	await := func(ch <-chan string, want string) {
		t.Helper()
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("source = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("catalog entrypoint did not use the shared updater")
		}
	}
	// A local server prevents a broken legacy implementation from reaching the network.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	oldURLs, oldDevinURLs := modelsURLs, devinModelsURLs
	modelsURLs, devinModelsURLs = []string{server.URL}, []string{server.URL}
	t.Cleanup(func() { modelsURLs, devinModelsURLs = oldURLs, oldDevinURLs })
	StartModelsUpdater(ctx)
	StartCodexClientModelsUpdater(ctx)
	StartDevinModelsUpdater(ctx)
	await(general, "")
	await(codex, "")
	await(devin, "")
	sources := CatalogSources{Catalog: "https://example.com/general", CodexCatalog: "https://example.com/codex", DevinCatalog: "https://example.com/devin"}
	StartModelCatalogUpdaters(ctx, sources, false)
	await(general, sources.Catalog)
	await(codex, sources.CodexCatalog)
	await(devin, sources.DevinCatalog)
	for _, home := range []bool{false, true} {
		UpdateModelCatalogSources(sources, home)
		StartModelsUpdater(ctx)
		StartCodexClientModelsUpdater(ctx)
		StartDevinModelsUpdater(ctx)
		want := effectiveCatalogSources(sources, false, home)
		for updater, source := range map[*catalogUpdater]string{generalCatalogUpdater: want.Catalog, codexCatalogUpdater: want.CodexCatalog, devinCatalogUpdater: want.DevinCatalog} {
			updater.mu.Lock()
			got := updater.source
			updater.mu.Unlock()
			if got != source {
				t.Fatalf("legacy entrypoint replaced service source %q with %q", source, got)
			}
		}
	}
}
