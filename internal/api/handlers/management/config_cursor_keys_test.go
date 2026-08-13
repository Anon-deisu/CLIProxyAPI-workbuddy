package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetCursorKeysMasksCredential(t *testing.T) {
	t.Parallel()

	const apiKey = "crsr_0123456789abcdef"
	h := &Handler{cfg: &config.Config{CursorKey: []config.CursorKey{{
		APIKey:         apiKey,
		Prefix:         "team",
		ExcludedModels: []string{"gpt-old", "*"},
	}}}}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-api-key", nil)

	h.GetCursorKeys(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), apiKey) {
		t.Fatalf("response leaked raw Cursor API key: %s", rec.Body.String())
	}
	var payload struct {
		Items []cursorKeyView `json:"cursor-api-key"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(payload.Items))
	}
	item := payload.Items[0]
	if item.APIKey != "crsr_********cdef" || !item.APIKeySet {
		t.Fatalf("masked key = %q, set=%v", item.APIKey, item.APIKeySet)
	}
	if !item.Disabled {
		t.Fatal("disabled = false, want true")
	}
	if len(item.ExcludedModels) != 1 || item.ExcludedModels[0] != "gpt-old" {
		t.Fatalf("visible excluded models = %#v", item.ExcludedModels)
	}
}

func TestGetCursorKeysIncludesOAuthStyleRuntimeStats(t *testing.T) {
	t.Parallel()

	const apiKey = "crsr_runtime_stats"
	id, _ := synthesizer.NewStableIDGenerator().Next("cursor:apikey", apiKey, "")
	nextRetry := time.Date(2026, 8, 14, 0, 30, 0, 0, time.UTC)
	updatedAt := nextRetry.Add(-time.Minute)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:            id,
		Provider:      "cursor",
		Label:         "cursor-apikey",
		Status:        coreauth.StatusError,
		StatusMessage: "upstream rate limited",
		Unavailable:   true,
		LastError: &coreauth.Error{
			Code:       "rate_limit",
			Message:    "upstream rate limited",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
		NextRetryAfter: nextRetry,
		UpdatedAt:      updatedAt,
		Success:        7,
		Failed:         3,
	}); errRegister != nil {
		t.Fatal(errRegister)
	}

	h := &Handler{
		cfg:         &config.Config{CursorKey: []config.CursorKey{{APIKey: apiKey}}},
		authManager: manager,
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-api-key", nil)
	h.GetCursorKeys(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Items []cursorKeyView `json:"cursor-api-key"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(payload.Items))
	}
	item := payload.Items[0]
	if item.AuthIndex == "" || item.Success != 7 || item.Failed != 3 {
		t.Fatalf("runtime counts/index = %+v", item)
	}
	if item.Status != coreauth.StatusError || !item.Unavailable || item.StatusMessage != "upstream rate limited" {
		t.Fatalf("runtime status = %+v", item)
	}
	if item.LastError == nil || item.LastError.HTTPStatus != http.StatusTooManyRequests || !item.LastError.Retryable {
		t.Fatalf("last error = %+v", item.LastError)
	}
	if item.NextRetryAfter == nil || !item.NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("next retry = %v", item.NextRetryAfter)
	}
}

func TestCursorKeyManagementCreatePatchDelete(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: writeTestConfigFile(t),
	}

	rec := performCursorKeyHandlerRequest(t, http.MethodPost, "/v0/management/cursor-api-key", `{
		"api-key":"crsr_new-secret-value",
		"base-url":"https://api2.cursor.sh/",
		"prefix":" team ",
		"priority":7,
		"excluded-models":["old-model"],
		"disable-cooling":true
	}`, h.PostCursorKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.CursorKey) != 1 {
		t.Fatalf("keys = %d, want 1", len(h.cfg.CursorKey))
	}
	if h.cfg.CursorKey[0].BaseURL != "" {
		t.Fatalf("base URL = %q, want normalized default", h.cfg.CursorKey[0].BaseURL)
	}

	rec = performCursorKeyHandlerRequest(t, http.MethodPatch, "/v0/management/cursor-api-key", `{
		"index":0,
		"value":{
			"api-key":"",
			"prefix":"prod",
			"disabled":true
		}
	}`, h.PatchCursorKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := h.cfg.CursorKey[0].APIKey; got != "crsr_new-secret-value" {
		t.Fatalf("blank edit replaced stored key: %q", got)
	}
	if got := h.cfg.CursorKey[0].Prefix; got != "prod" {
		t.Fatalf("prefix = %q, want prod", got)
	}
	if !cursorKeyDisabled(h.cfg.CursorKey[0].ExcludedModels) {
		t.Fatalf("disabled marker missing: %#v", h.cfg.CursorKey[0].ExcludedModels)
	}

	rec = performCursorKeyHandlerRequest(t, http.MethodDelete, "/v0/management/cursor-api-key?index=0", "", h.DeleteCursorKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.CursorKey) != 0 {
		t.Fatalf("keys = %d, want 0", len(h.cfg.CursorKey))
	}
}

func TestPostCursorKeyRejectsDuplicateIdentity(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{CursorKey: []config.CursorKey{{
			APIKey: "crsr_same",
		}}},
		configFilePath: writeTestConfigFile(t),
	}
	rec := performCursorKeyHandlerRequest(t, http.MethodPost, "/v0/management/cursor-api-key", `{
		"api-key":"crsr_same",
		"base-url":"https://api2.cursor.sh"
	}`, h.PostCursorKey)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if len(h.cfg.CursorKey) != 1 {
		t.Fatalf("keys = %d, want unchanged list", len(h.cfg.CursorKey))
	}
}

func performCursorKeyHandlerRequest(
	t *testing.T,
	method string,
	target string,
	body string,
	handler func(*gin.Context),
) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	return rec
}
