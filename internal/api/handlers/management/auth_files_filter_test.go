package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFilesFiltersByNameAndAuthIndex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "shared-codex.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:       "auth-a",
		Index:    "idx-a",
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
	})
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:       "auth-b",
		Index:    "idx-b",
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?name=shared-codex.json&auth_index=idx-b", nil)
	ctx.Request = req

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1 payload=%s", len(payload.Files), rec.Body.String())
	}
	if got := payload.Files[0]["id"]; got != "auth-b" {
		t.Fatalf("id = %#v, want auth-b", got)
	}
	if got := payload.Files[0]["auth_index"]; got != "idx-b" {
		t.Fatalf("auth_index = %#v, want idx-b", got)
	}
}

func TestListAuthFilesFromDiskFiltersByNameAndRejectsAuthIndex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	for _, file := range []struct {
		name string
		body string
	}{
		{name: "alpha.json", body: `{"type":"codex","email":"alpha@example.com"}`},
		{name: "beta.json", body: `{"type":"codex","email":"beta@example.com"}`},
	} {
		if errWrite := os.WriteFile(filepath.Join(authDir, file.name), []byte(file.body), 0o600); errWrite != nil {
			t.Fatalf("failed to write auth file %s: %v", file.name, errWrite)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?name=beta.json", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Files) != 1 || payload.Files[0]["name"] != "beta.json" {
		t.Fatalf("files = %#v, want only beta.json", payload.Files)
	}

	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?name=beta.json&auth_index=idx-b", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload.Files = nil
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode auth_index response: %v", errDecode)
	}
	if len(payload.Files) != 0 {
		t.Fatalf("files = %#v, want no disk fallback matches for auth_index", payload.Files)
	}
}

func TestPatchAuthFileStatusVerifiesAuthIndex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:       "auth-a",
		Index:    "idx-a",
		FileName: "shared-codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	})
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:       "auth-b",
		Index:    "idx-b",
		FileName: "shared-codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"shared-codex.json","auth_index":"idx-b","disabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	authA, okA := manager.GetByID("auth-a")
	authB, okB := manager.GetByID("auth-b")
	if !okA || !okB {
		t.Fatalf("expected both auth records to exist")
	}
	if authA.Disabled || authA.Status == coreauth.StatusDisabled {
		t.Fatalf("auth-a was modified: %+v", authA)
	}
	if !authB.Disabled || authB.Status != coreauth.StatusDisabled {
		t.Fatalf("auth-b was not disabled: %+v", authB)
	}
}

func TestPatchAuthFileStatusRejectsMismatchedAuthIndex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:       "auth-a",
		Index:    "idx-a",
		FileName: "shared-codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"shared-codex.json","auth_index":"idx-missing","disabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	authA, ok := manager.GetByID("auth-a")
	if !ok {
		t.Fatalf("expected auth-a to exist")
	}
	if authA.Disabled || authA.Status == coreauth.StatusDisabled {
		t.Fatalf("auth-a was modified: %+v", authA)
	}
}

func TestAuthFileLookupAndEntryBuildConcurrentEnsureIndex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "concurrent-codex.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	auth := &coreauth.Auth{
		ID:       "auth-concurrent",
		Index:    "idx-concurrent",
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if !matchesAuthFileLookup(auth, fileName, "idx-concurrent") {
					t.Errorf("auth lookup did not match")
				}
				entry := h.buildAuthFileEntry(auth)
				if entry == nil {
					t.Errorf("entry is nil")
					continue
				}
				if got := entry["auth_index"]; got != "idx-concurrent" {
					t.Errorf("auth_index = %#v, want idx-concurrent", got)
				}
			}
		}()
	}
	wg.Wait()
}

func TestBuildAuthFileEntryExposesAutomaticDisableReason(t *testing.T) {
	auth := &coreauth.Auth{
		ID:                "auto-disabled",
		FileName:          "auto-disabled.json",
		Provider:          "codex",
		Status:            coreauth.StatusError,
		AutoDisabled:      true,
		AutoDisableReason: coreauth.AutoDisableReasonUnauthorized,
		AutoDisabledAt:    time.Unix(123, 0),
		StatusMessage:     "automatically disabled: unauthorized",
		Attributes:        map[string]string{"path": filepath.Join(t.TempDir(), "auto-disabled.json")},
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("entry is nil")
	}
	if entry["disabled"] != true || entry["auto_disabled"] != true || entry["manual_disabled"] != false {
		t.Fatalf("disable fields = %#v", entry)
	}
	if entry["auto_disable_reason"] != coreauth.AutoDisableReasonUnauthorized {
		t.Fatalf("auto disable reason = %#v", entry["auto_disable_reason"])
	}
}

func TestBuildAuthFileEntryExposesConcurrencyState(t *testing.T) {
	auth := &coreauth.Auth{
		ID:               "concurrency-auth",
		FileName:         "concurrency-auth.json",
		Provider:         "codex",
		Status:           coreauth.StatusActive,
		ConcurrencyLimit: 2,
		Attributes:       map[string]string{"path": filepath.Join(t.TempDir(), "concurrency-auth.json")},
	}
	if !auth.TryAcquireDispatchSlot() {
		t.Fatal("failed to acquire test dispatch slot")
	}
	defer auth.ReleaseDispatchSlot()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("entry is nil")
	}
	if entry["concurrency_limit"] != 2 || entry["active_requests"] != 1 {
		t.Fatalf("concurrency fields = %#v, want limit=2 active=1", entry)
	}
}

func TestPatchAuthFileFieldsRejectsPerAccountConcurrency(t *testing.T) {
	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "concurrency-auth.json")
	original := `{"type":"codex"}`
	if errWrite := os.WriteFile(filePath, []byte(original), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:       "concurrency-auth",
		FileName: "concurrency-auth.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
	})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(`{"name":"concurrency-auth.json","concurrency_limit":3}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	updated, ok := manager.GetByID("concurrency-auth")
	if !ok || updated == nil || updated.EffectiveConcurrencyLimit() != 0 {
		t.Fatalf("updated concurrency state = %#v, want unchanged limit=0", updated)
	}
	data, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("failed to read auth file: %v", errRead)
	}
	if string(data) != original {
		t.Fatalf("auth file changed: got %q, want %q", string(data), original)
	}
}

func TestPatchAuthFileStatusResumesAutomaticDisable(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:                "auto-resume",
		FileName:          "auto-resume.json",
		Provider:          "codex",
		Status:            coreauth.StatusError,
		AutoDisabled:      true,
		AutoDisableReason: coreauth.AutoDisableReasonUnauthorized,
		ModelStates:       map[string]*coreauth.ModelState{"gpt-5": {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}},
	})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"auto-resume.json","disabled":false}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileStatus(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	updated, ok := manager.GetByID("auto-resume")
	if !ok || updated == nil {
		t.Fatal("resumed auth was not retained")
	}
	if updated.AutoDisabled || updated.Status != coreauth.StatusActive {
		t.Fatalf("resumed auth state = %+v", updated)
	}
	if state := updated.ModelStates["gpt-5"]; state != nil && state.Unavailable {
		t.Fatalf("resumed auth retained model cooldown: %+v", state)
	}
}

func registerAuthForLookupTest(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth %q: %v", auth.ID, errRegister)
	}
}
