package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/qodercheckin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQoderFileLabels(t *testing.T) {
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, tc := range []struct {
			name, label, nickname, email, uid, want string
		}{
			{"saved label", "My account", "Nickname", "account@example.test", "9007199254740993", "My account"},
			{"nickname", "", "Nickname", "account@example.test", "9007199254740993", "Nickname"},
			{"email", "", "", "account@example.test", "9007199254740993", "account@example.test"},
			{"uid", "", "", "", "9007199254740993", "9007199254740993"},
			{"provider", "", "", "", "", provider},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				raw, err := json.Marshal(map[string]any{"type": provider, "label": tc.label, "nickname": tc.nickname, "email": tc.email, "uid": tc.uid})
				if err != nil {
					t.Fatal(err)
				}
				auths, err := SynthesizeAuthFile(&SynthesisContext{Config: &config.Config{}, AuthDir: dir}, filepath.Join(dir, "account.json"), raw)
				if err != nil || len(auths) != 1 {
					t.Fatalf("synthesize: count=%d err=%v", len(auths), err)
				}
				if auths[0].Label != tc.want || auths[0].Provider != provider || auths[0].AuthKind() != "oauth" {
					t.Fatalf("label/provider/kind = %q/%q/%q", auths[0].Label, auths[0].Provider, auths[0].AuthKind())
				}
			})
		}
	}
}

func TestQoderFileIdentityAndAutoCheckInReload(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: dir, Now: now, IDGenerator: NewStableIDGenerator()}
	records := make(map[string]*coreauth.Auth)
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, org := range []string{"", "org-test"} {
			record := qoder.NewAuthRecord(provider, qoder.Credentials{UID: "9007199254740993", Nickname: "Shared nickname", OrganizationID: org, AccessToken: "dt-synthetic", RefreshToken: "drt-synthetic"})
			if _, exists := records[record.ID]; exists {
				t.Fatal("region or organization identity collided")
			}
			record.Label = "Saved account label"
			record.Metadata["label"] = record.Label
			record.Metadata["prefix"] = "tenant"
			record.Metadata["proxy_url"] = "direct"
			records[record.ID] = record
		}
	}
	indexes := make(map[string]string)
	for _, step := range []struct {
		name               string
		setting            any
		disabled, eligible bool
	}{
		{"default off", nil, false, false},
		{"enable", true, false, true},
		{"disable", false, false, false},
		{"reenable", true, false, true},
		{"disabled account", true, true, false},
		{"string is not opt in", "true", false, false},
		{"remove setting", nil, false, false},
	} {
		t.Run(step.name, func(t *testing.T) {
			for _, record := range records {
				record.Metadata["nickname"] = "Renamed " + step.name
				record.Metadata["access_token"] = "dt-rotated-" + step.name
				record.Metadata["refresh_token"] = "drt-rotated-" + step.name
				record.Metadata["disabled"] = step.disabled
				delete(record.Metadata, qodercheckin.AutoCheckInKey)
				if step.setting != nil {
					record.Metadata[qodercheckin.AutoCheckInKey] = step.setting
				}
				raw, err := json.Marshal(record.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				if errWrite := os.WriteFile(filepath.Join(dir, record.FileName), raw, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
			}
			ctx.Now = ctx.Now.Add(time.Minute)
			ctx.IDGenerator = NewStableIDGenerator()
			auths, err := NewFileSynthesizer().Synthesize(ctx)
			if err != nil || len(auths) != len(records) {
				t.Fatalf("reload: count=%d err=%v", len(auths), err)
			}
			seenIndexes := make(map[string]bool)
			for _, auth := range auths {
				want := records[auth.ID]
				if want == nil {
					t.Fatalf("reload changed account ID to %q", auth.ID)
				}
				index := auth.EnsureIndex()
				if index == "" || seenIndexes[index] {
					t.Fatal("missing or duplicate account index")
				}
				seenIndexes[index] = true
				if previous := indexes[auth.ID]; previous != "" && previous != index {
					t.Fatal("reload changed stable account index")
				}
				indexes[auth.ID] = index
				if auth.Provider != want.Provider || auth.Label != want.Label || auth.Prefix != "tenant" || auth.ProxyURL != "direct" || auth.Disabled != step.disabled {
					t.Fatalf("reload lost account settings for %s", auth.ID)
				}
				for _, key := range []string{"uid", "realm", "organization_id", "nickname", "access_token", "refresh_token"} {
					if auth.Metadata[key] != want.Metadata[key] {
						t.Fatalf("reload lost %s", key)
					}
				}
				if auth.Metadata[qodercheckin.AutoCheckInKey] != step.setting {
					t.Fatal("reload retained a stale check-in setting")
				}
				if qodercheckin.Eligible(auth, now) != step.eligible {
					t.Fatal("reloaded setting did not change automatic eligibility")
				}
				if auth.Attributes[coreauth.AttributePath] != filepath.Join(dir, auth.ID) || auth.AuthSourceKind() != coreauth.AuthSourceFile {
					t.Fatal("reload lost file source identity")
				}
			}
		})
	}
}
