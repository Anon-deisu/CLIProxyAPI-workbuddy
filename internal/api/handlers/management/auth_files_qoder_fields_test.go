package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/qodercheckin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	fileauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func qoderFieldsRecord(provider string) *coreauth.Auth {
	auth := qoder.NewAuthRecord(provider, qoder.Credentials{
		UID: "9007199254740993", Nickname: "Visible nickname", Email: "account@example.test", OrganizationID: "org-test", OrganizationName: "Visible organization",
		AccessToken: "secret-marker-access", RefreshToken: "secret-marker-refresh", PersonalToken: "secret-marker-pat", MachineID: "secret-marker-machine",
	})
	auth.Label = "Saved label"
	auth.Metadata["label"] = auth.Label
	auth.Metadata[qoder.MachineIdentityMetadataKey] = map[string]any{"machineToken": "secret-marker-native", "machineCode": "secret-marker-code"}
	for _, key := range []string{"id_token", "password", "client_secret", "verifier", "nonce", "qoder_private", "openapi_url", "gateway_url"} {
		auth.Metadata[key] = "secret-marker-" + key
	}
	auth.Metadata["nested_private"] = map[string]any{"token": "secret-marker-nested"}
	return auth
}

func qoderFieldsRequest(t *testing.T, handler gin.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/v0/management/auth-files", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	if strings.Contains(w.Body.String(), "secret-marker") {
		t.Fatal("management response leaked a credential or private field")
	}
	return w
}

func qoderFieldsList(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	w := qoderFieldsRequest(t, h.ListAuthFiles, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("list count=%d, want 1", len(payload.Files))
	}
	return payload.Files[0]
}

func TestQoderAuthFileListMemoryAndDiskAllowlist(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, storage := range []string{"memory", "disk"} {
			for _, setting := range []any{nil, false, true} {
				t.Run(fmt.Sprintf("%s/%s/checkin=%v", provider, storage, setting), func(t *testing.T) {
					auth := qoderFieldsRecord(provider)
					if setting != nil {
						auth.Metadata[qodercheckin.AutoCheckInKey] = setting
					}
					auth.Metadata[qodercheckin.LastAttemptKey] = float64(1793502000)
					auth.Metadata[qodercheckin.LastSuccessKey] = float64(1793502001)
					auth.Metadata[qodercheckin.NextAttemptKey] = float64(1793588400)
					auth.Metadata[qodercheckin.LastResultKey] = map[string]any{"status": "success", "claimed": float64(1), "credits": float64(100), "already": float64(0), "message": "Claimed"}
					h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
					if storage == "memory" {
						auth.Attributes = map[string]string{"runtime_only": "true"}
						h.authManager = coreauth.NewManager(nil, nil, nil)
						if _, err := h.authManager.Register(t.Context(), auth); err != nil {
							t.Fatal(err)
						}
					} else {
						raw, err := json.Marshal(auth.Metadata)
						if err != nil {
							t.Fatal(err)
						}
						if errWrite := os.WriteFile(filepath.Join(h.cfg.AuthDir, auth.FileName), raw, 0o600); errWrite != nil {
							t.Fatal(errWrite)
						}
					}
					entry := qoderFieldsList(t, h)
					allowed := map[string]bool{"type": true, "label": true, "nickname": true, "uid": true, "email": true, "realm": true, "organization_id": true, "organization_name": true, qodercheckin.AutoCheckInKey: true}
					for _, key := range []string{qodercheckin.LastAttemptKey, qodercheckin.LastSuccessKey, qodercheckin.NextAttemptKey, qodercheckin.LastResultKey} {
						allowed[key] = true
					}
					if storage == "memory" {
						if entry["source"] != "memory" || entry["auth_index"] == "" || entry["provider"] != provider {
							t.Fatal("memory entry lost provider/source identity")
						}
					}
					for key, value := range auth.Metadata {
						got, exists := entry[key]
						if allowed[key] {
							if !exists || !reflect.DeepEqual(got, value) {
								t.Fatalf("public field %s = %#v, want %#v", key, got, value)
							}
						} else if exists {
							t.Fatalf("unlisted metadata field %q escaped the allowlist", key)
						}
					}
					if entry[qodercheckin.AutoCheckInKey] != (setting == true) {
						t.Fatal("check-in flag must be an explicit boolean, default false")
					}
					if entry["name"] != auth.FileName {
						t.Fatal("list changed the stable file identity")
					}
				})
			}
		}
	}
}

func TestQoderAuthFileFieldsBooleanPatchRoundTrip(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, storage := range []string{"memory", "disk"} {
			t.Run(provider+"/"+storage, func(t *testing.T) {
				dir := t.TempDir()
				var store coreauth.Store = &memoryAuthStore{}
				if storage == "disk" {
					disk := fileauth.NewFileTokenStore()
					disk.SetBaseDir(dir)
					store = disk
				}
				manager := coreauth.NewManager(store, nil, nil)
				auth := qoderFieldsRecord(provider)
				auth.Attributes = map[string]string{"path": filepath.Join(dir, auth.FileName)}
				auth.Prefix, auth.ProxyURL = "tenant", "direct"
				auth.Metadata["prefix"], auth.Metadata["proxy_url"] = auth.Prefix, auth.ProxyURL
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
				h := &Handler{cfg: &config.Config{AuthDir: dir}, authManager: manager, tokenStore: store}
				index := auth.EnsureIndex()
				patch := func(fields string) *httptest.ResponseRecorder {
					t.Helper()
					return qoderFieldsRequest(t, h.PatchAuthFileFields, http.MethodPatch, fmt.Sprintf(`{"name":%q,"auth_index":%q,%s}`, auth.FileName, index, fields))
				}
				for _, enabled := range []bool{true, false, true} {
					w := patch(fmt.Sprintf(`"qoder_auto_checkin":%t`, enabled))
					if w.Code != http.StatusOK {
						t.Fatalf("patch status=%d body=%s", w.Code, w.Body.String())
					}
					live, ok := manager.GetByID(auth.ID)
					if !ok || live.Metadata[qodercheckin.AutoCheckInKey] != enabled || live.Provider != provider || live.Label != auth.Label || live.Index != index || live.Prefix != auth.Prefix || live.ProxyURL != auth.ProxyURL {
						t.Fatal("patch did not update the boolean while preserving account settings")
					}
					for _, key := range []string{"access_token", "refresh_token", "personal_token", qoder.MachineIdentityMetadataKey} {
						if !reflect.DeepEqual(live.Metadata[key], auth.Metadata[key]) {
							t.Fatalf("patch changed private field %s", key)
						}
					}
					saved, err := store.List(t.Context())
					if err != nil || len(saved) != 1 || saved[0].Metadata[qodercheckin.AutoCheckInKey] != enabled {
						t.Fatalf("boolean did not persist through %s store: %v", storage, err)
					}
					if qoderFieldsList(t, h)[qodercheckin.AutoCheckInKey] != enabled {
						t.Fatal("runtime list retained a stale setting")
					}
					if storage == "disk" {
						if qoderFieldsList(t, &Handler{cfg: h.cfg})[qodercheckin.AutoCheckInKey] != enabled {
							t.Fatal("disk list retained a stale setting")
						}
						raw, errRead := os.ReadFile(auth.Attributes["path"])
						if errRead != nil {
							t.Fatal(errRead)
						}
						now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
						reloaded, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{Config: h.cfg, AuthDir: dir, Now: now}, auth.Attributes["path"], raw)
						if err != nil || len(reloaded) != 1 || reloaded[0].ID != auth.ID || reloaded[0].Label != auth.Label || reloaded[0].Metadata[qodercheckin.AutoCheckInKey] != enabled || qodercheckin.Eligible(reloaded[0], now) != enabled {
							t.Fatalf("patched boolean did not survive watcher reload: %v", err)
						}
					}
				}
				// Invalid patches must not partially commit unrelated edits.
				for _, fields := range []string{
					`"qoder_auto_checkin":"true"`, `"qoder_auto_checkin":"false"`, `"qoder_auto_checkin":null`, `"qoder_auto_checkin":0`, `"qoder_auto_checkin":1`,
					`"qoder_auto_checkin":[]`, `"qoder_auto_checkin":{}`, `"qoder_auto_checkin.enabled":true`, `" qoder_auto_checkin . enabled ":false`,
				} {
					before, _ := manager.GetByID(auth.ID)
					w := patch(fields + `,"note":"must not commit"`)
					if w.Code != http.StatusBadRequest {
						t.Fatalf("invalid patch %s returned %d", fields, w.Code)
					}
					after, _ := manager.GetByID(auth.ID)
					if !reflect.DeepEqual(before.Metadata, after.Metadata) || !reflect.DeepEqual(before.Attributes, after.Attributes) {
						t.Fatal("invalid patch mutated runtime state")
					}
					saved, err := store.List(t.Context())
					if err != nil || len(saved) != 1 || saved[0].Metadata[qodercheckin.AutoCheckInKey] != true || saved[0].Metadata["note"] != nil {
						t.Fatal("invalid patch persisted partial changes")
					}
				}
				if w := patch(`"note":"unrelated update"`); w.Code != http.StatusOK {
					t.Fatal("unrelated patch failed")
				}
				if qoderFieldsList(t, h)[qodercheckin.AutoCheckInKey] != true {
					t.Fatal("omitted flag reset the opt-in")
				}
			})
		}
	}
}

func TestQoderAuthFileFieldsRejectOtherProviders(t *testing.T) {
	for _, provider := range []string{"codex", "workbuddy"} {
		t.Run(provider, func(t *testing.T) {
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			auth := &coreauth.Auth{ID: "other.json", FileName: "other.json", Provider: provider, Attributes: map[string]string{"path": filepath.Join(t.TempDir(), "other.json")}, Metadata: map[string]any{"type": provider, "nickname": "Other", "uid": "other", "realm": "intl", "organization_id": "other", "organization_name": "Other"}}
			if _, err := manager.Register(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
			h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}, authManager: manager}
			for _, value := range []string{"true", "false"} {
				w := qoderFieldsRequest(t, h.PatchAuthFileFields, http.MethodPatch, `{"name":"other.json","qoder_auto_checkin":`+value+`}`)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("non-Qoder patch returned %d", w.Code)
				}
			}
			entry := qoderFieldsList(t, h)
			for _, key := range []string{"qoder_auto_checkin", "nickname", "uid", "realm", "organization_id", "organization_name"} {
				if _, exists := entry[key]; exists {
					t.Fatalf("non-Qoder list exposes %s", key)
				}
			}
			saved, err := store.List(t.Context())
			if err != nil || len(saved) != 1 {
				t.Fatalf("store count=%d err=%v", len(saved), err)
			}
			if _, exists := saved[0].Metadata[qodercheckin.AutoCheckInKey]; exists {
				t.Fatal("rejected non-Qoder setting was persisted")
			}
		})
	}
}
