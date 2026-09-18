package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// performCursorKeyHandlerRequest drives one cursor-api-key management handler
// (GET/POST/PATCH/DELETE) through an in-memory gin context and returns the
// response recorder, so tests can assert on status and JSON payload.
func performCursorKeyHandlerRequest(t *testing.T, method, path, body string, handler func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	handler(ctx)
	return rec
}

// against a handler with an isolated temp auth dir (beyond the seeded records).
func cursorKeyHandler(t *testing.T) (*Handler, *cursor.APIKeyStore) {
	t.Helper()
	dir := t.TempDir()
	h := &Handler{cfg: &config.Config{AuthDir: dir}}
	return h, cursor.NewAPIKeyStore(dir)
}

func TestGetCursorKeysMasksCredential(t *testing.T) {
	t.Parallel()

	const apiKey = "crsr_0123456789abcdef"
	h, store := cursorKeyHandler(t)
	if _, err := store.Create(&cursor.APIKeyStorage{
		APIKey:         apiKey,
		Prefix:         "team",
		ExcludedModels: []string{"gpt-old", "*"},
	}); err != nil {
		t.Fatal(err)
	}

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
	if len(item.ExcludedModels) != 1 || item.ExcludedModels[0] != "gpt-old" {
		t.Fatalf("visible excluded models = %#v", item.ExcludedModels)
	}
	// The "*" sentinel must never surface in the API view.
	for _, model := range item.ExcludedModels {
		if model == configAPIKeyDisablePattern {
			t.Fatalf("view leaked disable sentinel: %#v", item.ExcludedModels)
		}
	}
}

func TestGetCursorKeysIncludesOAuthStyleRuntimeStats(t *testing.T) {
	t.Parallel()

	h, store := cursorKeyHandler(t)
	name, err := store.Create(&cursor.APIKeyStorage{
		APIKey: "crsr_runtime_stats",
		Email:  "runtime@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The watcher promotes each cursor-api-key file to an Auth whose ID equals
	// the file's base name; index the runtime table by that same ID.
	id := filepath.Base(name)

	nextRetry := time.Date(2026, 8, 14, 0, 30, 0, 0, time.UTC)
	updatedAt := nextRetry.Add(-time.Minute)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:            id,
		Provider:      cursor.StoredAPIKeyType,
		Label:         "runtime@example.com",
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
	h.authManager = manager

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
	if item.Email != "runtime@example.com" {
		t.Fatalf("email = %q", item.Email)
	}
}

func TestCursorKeyManagementCreatePatchDelete(t *testing.T) {
	t.Parallel()

	h, store := cursorKeyHandler(t)

	rec := performCursorKeyHandlerRequest(t, http.MethodPost, "/v0/management/cursor-api-key", `{
		"api-key":"crsr_new-secret-value",
		"base-url":"https://api2.cursor.sh/",
		"prefix":"team",
		"priority":7,
		"excluded-models":["old-model"],
		"disable-cooling":true,
		"email":"ops@example.com"
	}`, h.PostCursorKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}
	list, errList := store.List()
	if errList != nil || len(list) != 1 {
		t.Fatalf("list = %d, err=%v; want 1", len(list), errList)
	}
	got := list[0]
	if got.APIKey != "crsr_new-secret-value" {
		t.Fatalf("stored key = %q", got.APIKey)
	}
	if got.BaseURL != "https://api2.cursor.sh/" {
		t.Fatalf("stored base url = %q", got.BaseURL)
	}
	if got.Type != cursor.StoredAPIKeyType || got.AuthKind != "apikey" {
		t.Fatalf("discriminators = type:%q auth_kind:%q", got.Type, got.AuthKind)
	}

	// Patch by index: blank api-key keeps the stored secret, custom base-url is
	// trimmed of trailing slashes in the view, disabled flips the real bool.
	rec = performCursorKeyHandlerRequest(t, http.MethodPatch, "/v0/management/cursor-api-key", `{
		"index":0,
		"value":{
			"api-key":"",
			"prefix":"prod",
			"base-url":"https://api2.cursor.sh/",
			"disabled":true
		}
	}`, h.PatchCursorKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body=%s", rec.Code, rec.Body.String())
	}
	patched, errGet := store.Get(0)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if patched.APIKey != "crsr_new-secret-value" {
		t.Fatalf("blank edit replaced stored key: %q", patched.APIKey)
	}
	if patched.Prefix != "prod" {
		t.Fatalf("prefix = %q, want prod", patched.Prefix)
	}
	if !patched.Disabled {
		t.Fatalf("disabled = false, want true")
	}

	// The view reflects base-url trimmed of the trailing slash and disabled=true.
	rec = performCursorKeyHandlerRequest(t, http.MethodGet, "/v0/management/cursor-api-key", "", h.GetCursorKeys)
	var payload struct {
		Items []cursorKeyView `json:"cursor-api-key"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(payload.Items) != 1 || payload.Items[0].BaseURL != "https://api2.cursor.sh" || !payload.Items[0].Disabled {
		t.Fatalf("view = %+v", payload.Items)
	}

	rec = performCursorKeyHandlerRequest(t, http.MethodDelete, "/v0/management/cursor-api-key?index=0", "", h.DeleteCursorKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d; body=%s", rec.Code, rec.Body.String())
	}
	list, errList = store.List()
	if errList != nil || len(list) != 0 {
		t.Fatalf("list = %d, err=%v; want 0", len(list), errList)
	}
	entries, errDir := os.ReadDir(store.Dir())
	if errDir != nil {
		t.Fatal(errDir)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("leftover credential file: %s", e.Name())
		}
	}
}

// TestPostCursorKeyDedupsByIdentity verifies the JSON store's identity rule:
// re-posting the same key + base-url (without an email/prefix) lands on the same
// digest-named file instead of growing the credential list.
func TestPostCursorKeyDedupsByIdentity(t *testing.T) {
	t.Parallel()

	h, store := cursorKeyHandler(t)
	post := func() {
		t.Helper()
		rec := performCursorKeyHandlerRequest(t, http.MethodPost, "/v0/management/cursor-api-key", `{
			"api-key":"crsr_same",
			"base-url":"https://api2.cursor.sh"
		}`, h.PostCursorKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("post status = %d; body=%s", rec.Code, rec.Body.String())
		}
	}
	post()
	post()

	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("keys = %d, want 1 (identity dedup)", len(list))
	}
	// A distinct email assigns a distinct file, so both should coexist.
	rec := performCursorKeyHandlerRequest(t, http.MethodPost, "/v0/management/cursor-api-key", `{
		"api-key":"crsr_same",
		"email":"alt@example.com"
	}`, h.PostCursorKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("post status = %d; body=%s", rec.Code, rec.Body.String())
	}
	list, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("keys = %d, want 2 after distinct-email create", len(list))
	}
}
