package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestModelQuotaObservationPayloadOmitsUnsupportedProviders(t *testing.T) {
	states := map[string]*coreauth.ModelState{
		"grok-4": {
			Quota: coreauth.QuotaState{
				ObservedAt: time.Unix(10, 0),
				Signals:    map[string]string{"X-Ratelimit-Remaining-Requests": "1"},
			},
		},
	}
	if got := modelQuotaObservationPayload("grok", states); len(got) != 0 {
		t.Fatalf("unsupported provider returned model observations: %#v", got)
	}
	for _, provider := range []string{"gemini", "gemini-interactions", "openai", "openai-compatibility", "plugin-provider"} {
		if got := modelQuotaObservationPayload(provider, states); len(got) != 0 {
			t.Fatalf("provider %q returned model observations: %#v", provider, got)
		}
	}
}

func TestModelQuotaObservationPayloadSkipsNilAndEmptyStates(t *testing.T) {
	states := map[string]*coreauth.ModelState{
		"nil":   nil,
		"empty": &coreauth.ModelState{},
		"observed": &coreauth.ModelState{Quota: coreauth.QuotaState{
			ObservedAt: time.Unix(10, 0),
			Signals:    map[string]string{"X-Codex-Plan-Type": "pro"},
		}},
	}
	got := modelQuotaObservationPayload("codex", states)
	if len(got) != 1 {
		t.Fatalf("model observations = %#v, want only observed state", got)
	}
	if _, ok := got["observed"]; !ok {
		t.Fatalf("observed model quota missing: %#v", got)
	}
}

func TestQuotaObservationPayloadExcludesCooldownState(t *testing.T) {
	payload := quotaObservationPayload(coreauth.QuotaState{
		Exceeded:      true,
		Reason:        "credential_quota",
		NextRecoverAt: time.Unix(20, 0),
		BackoffLevel:  3,
		ObservedAt:    time.Unix(10, 0),
		Signals:       map[string]string{"X-Codex-Plan-Type": "pro"},
	})
	if _, ok := payload["exceeded"]; ok {
		t.Fatalf("cooldown exceeded leaked: %#v", payload)
	}
	if _, ok := payload["reason"]; ok {
		t.Fatalf("cooldown reason leaked: %#v", payload)
	}
	if _, ok := payload["next_recover_at"]; ok {
		t.Fatalf("cooldown recovery leaked: %#v", payload)
	}
	if _, ok := payload["backoff_level"]; ok {
		t.Fatalf("cooldown backoff leaked: %#v", payload)
	}
}

func TestCooldownSnapshotForAuthIsReadOnlyAndSanitized(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:             "codex-auth",
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Minute),
		ModelStates: map[string]*coreauth.ModelState{
			"model-a": {
				Unavailable:    true,
				NextRetryAfter: now.Add(20 * time.Second),
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: now.Add(20 * time.Second),
					BackoffLevel:  3,
				},
				LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "private upstream body"},
			},
			"model-b": {
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
			},
		},
	}
	before := auth.Clone()
	views := coreauth.CooldownSnapshotForAuth(auth, now)
	if len(views) != 2 {
		t.Fatalf("cooldown views = %+v, want two model cooldowns", views)
	}
	if views[0].Scope != "model" || views[0].ModelKey != "model-a" || views[0].Reason != "quota" || views[0].HTTPStatus != http.StatusTooManyRequests || views[0].BackoffLevel == nil || *views[0].BackoffLevel != 3 {
		t.Fatalf("unexpected first cooldown view: %+v", views[0])
	}
	if views[0].RemainingSeconds != 20 || !views[0].RetryAt.Equal(now.Add(20*time.Second)) {
		t.Fatalf("unexpected cooldown timing: %+v", views[0])
	}
	encoded, errMarshal := json.Marshal(views)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if string(encoded) == "" || containsPrivateCooldownText(string(encoded)) {
		t.Fatalf("cooldown view leaked raw error data: %s", encoded)
	}
	if !reflect.DeepEqual(before, auth) {
		t.Fatal("cooldown projection mutated auth state")
	}
}

func TestListAuthFilesIncludesCooldownContract(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:          "codex-runtime",
		FileName:    "codex-runtime.json",
		Provider:    "codex",
		Status:      coreauth.StatusActive,
		Attributes:  map[string]string{"runtime_only": "true"},
		ModelStates: map[string]*coreauth.ModelState{},
	})
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		ObservedAt time.Time `json:"observed_at"`
		Files      []struct {
			Cooldowns []coreauth.CooldownView `json:"cooldowns"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	if payload.ObservedAt.IsZero() || payload.ObservedAt.Location() != time.UTC {
		t.Fatalf("observed_at = %v", payload.ObservedAt)
	}
	if len(payload.Files) != 1 || payload.Files[0].Cooldowns == nil {
		t.Fatalf("cooldown contract = %+v", payload)
	}
}

func containsPrivateCooldownText(value string) bool {
	return strings.Contains(value, "private upstream body")
}
