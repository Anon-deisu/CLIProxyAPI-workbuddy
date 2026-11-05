package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestListAuthFiles_IncludesRecentRequestsBuckets(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       "runtime-only-auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
		Quota: coreauth.QuotaState{
			ObservedAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "58",
			},
		},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": {
				Quota: coreauth.QuotaState{
					ObservedAt: time.Date(2026, 8, 22, 0, 1, 0, 0, time.UTC),
					Signals: map[string]string{
						"Retry-After": "120",
					},
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID: "runtime-only-auth-1", Provider: "codex", Model: "gpt-5", Success: true,
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ginCtx.Request = req

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", payload)
	}
	if len(filesRaw) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(filesRaw))
	}

	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}

	if _, ok := fileEntry["success"].(float64); !ok {
		t.Fatalf("expected success number, got %#v", fileEntry["success"])
	}
	if _, ok := fileEntry["failed"].(float64); !ok {
		t.Fatalf("expected failed number, got %#v", fileEntry["failed"])
	}

	quota, ok := fileEntry["quota"].(map[string]any)
	if !ok {
		t.Fatalf("expected quota observation object, got %#v", fileEntry["quota"])
	}
	if _, ok := quota["observed_at"].(string); !ok {
		t.Fatalf("expected quota observed_at string, got %#v", quota["observed_at"])
	}
	quotaSignals, ok := quota["signals"].(map[string]any)
	if !ok || quotaSignals["X-Codex-Primary-Used-Percent"] != "58" {
		t.Fatalf("expected auth quota signals, got %#v", quota["signals"])
	}

	modelQuotas, ok := fileEntry["model_quotas"].(map[string]any)
	if !ok {
		t.Fatalf("expected model_quotas object, got %#v", fileEntry["model_quotas"])
	}
	modelQuota, ok := modelQuotas["gpt-5"].(map[string]any)
	if !ok {
		t.Fatalf("expected gpt-5 quota observation, got %#v", modelQuotas["gpt-5"])
	}
	modelSignals, ok := modelQuota["signals"].(map[string]any)
	if !ok || modelSignals["Retry-After"] != "120" {
		t.Fatalf("expected model quota signals, got %#v", modelQuota["signals"])
	}

	recentRaw, ok := fileEntry["recent_requests"].([]any)
	if !ok {
		t.Fatalf("expected recent_requests array, got %#v", fileEntry["recent_requests"])
	}
	if len(recentRaw) != 20 {
		t.Fatalf("expected 20 recent_requests buckets, got %d", len(recentRaw))
	}
	for idx, item := range recentRaw {
		bucket, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected bucket object at %d, got %#v", idx, item)
		}
		if _, ok := bucket["time"].(string); !ok {
			t.Fatalf("expected bucket time string at %d, got %#v", idx, bucket["time"])
		}
		if _, ok := bucket["success"].(float64); !ok {
			t.Fatalf("expected bucket success number at %d, got %#v", idx, bucket["success"])
		}
		if _, ok := bucket["failed"].(float64); !ok {
			t.Fatalf("expected bucket failed number at %d, got %#v", idx, bucket["failed"])
		}
	}
	newest, ok := recentRaw[len(recentRaw)-1].(map[string]any)
	if !ok {
		t.Fatalf("expected newest bucket object, got %#v", recentRaw[len(recentRaw)-1])
	}
	models, ok := newest["models"].([]any)
	if !ok || len(models) != 1 {
		t.Fatalf("expected one newest model entry, got %#v", newest["models"])
	}
	model, ok := models[0].(map[string]any)
	if !ok || model["model"] != "gpt-5" || model["success"] != float64(1) || model["failed"] != float64(0) {
		t.Fatalf("unexpected newest model entry: %#v", models[0])
	}
}

func TestMergeRecentRequestModelCosts(t *testing.T) {
	buckets := []coreauth.RecentRequestBucket{
		{BucketID: 10, Models: []coreauth.RecentRequestModelBucket{{Model: "gpt-5", Success: 2}}},
		{BucketID: 11},
	}
	costs := []coreusage.AuthModelCostBucket{
		{BucketID: 10, Model: "openai/gpt-5(max)", EstimatedCostUSD: 0.1234},
		{BucketID: 11, Model: "claude-4", EstimatedCostUSD: 0.5},
	}

	got := mergeRecentRequestModelCosts(buckets, costs)
	if got[0].Models[0].EstimatedCostUSD != 0.1234 {
		t.Fatalf("merged gpt-5 cost = %.4f, want 0.1234", got[0].Models[0].EstimatedCostUSD)
	}
	if len(got[1].Models) != 1 || got[1].Models[0].Model != "claude-4" || got[1].Models[0].EstimatedCostUSD != 0.5 {
		t.Fatalf("merged new model = %+v, want claude-4 $0.5", got[1].Models)
	}
}

func TestAuthFileModelTokenUsageIsCumulativeAndResetsByAccount(t *testing.T) {
	id := t.Name()
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: id, Provider: "openai", Attributes: map[string]string{"runtime_only": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coreusage.ResetAuthCost(id); coreusage.ResetAuthCost(id + "-other") })
	for range 2 {
		coreusage.PublishRecord(t.Context(), coreusage.Record{AuthID: id, Provider: "openai", Model: "unpriced-model", Detail: coreusage.Detail{InputTokens: 100, CacheReadTokens: 25, OutputTokens: 10}})
	}
	coreusage.PublishRecord(t.Context(), coreusage.Record{AuthID: id, Model: "failed-model", Failed: true})
	coreusage.PublishRecord(t.Context(), coreusage.Record{AuthID: id + "-other", Model: "unrelated", Detail: coreusage.Detail{InputTokens: 7}})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	entry := h.buildAuthFileEntry(auth, authFileQuotaContext{})
	models := entry["model_token_usage"].([]coreusage.AuthModelTokenUsage)
	if len(models) != 2 || models[0] != (coreusage.AuthModelTokenUsage{Model: "unpriced-model", InputTokens: 200, CachedInputTokens: 50, OutputTokens: 20}) || models[1].Model != "failed-model" {
		t.Fatalf("account model token totals = %+v", models)
	}
	if _, _, errReset := manager.ResetQuota(t.Context(), auth.ID); errReset != nil {
		t.Fatal(errReset)
	}
	entry = h.buildAuthFileEntry(auth, authFileQuotaContext{})
	if models := entry["model_token_usage"].([]coreusage.AuthModelTokenUsage); len(models) != 0 {
		t.Fatalf("manual reset retained models: %+v", models)
	}
	if models := coreusage.AuthModelTokens(id + "-other"); len(models) != 1 || models[0].InputTokens != 7 {
		t.Fatalf("manual reset changed another account: %+v", models)
	}
}
