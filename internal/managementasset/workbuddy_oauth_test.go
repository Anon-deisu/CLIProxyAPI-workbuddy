package managementasset

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestAddWorkBuddyOAuthPreservesNativeProvidersAndLocales(t *testing.T) {
	input := []byte("<script>const p=[{kind:`builtin`,id:`kimi`,titleKey:`auth_login.kimi_oauth_title`,icon:{light:G,dark:D}}];" +
		"const zh={kimi_oauth_polling_error:`检查认证状态失败:`,other:`kept`};" +
		"const en={kimi_oauth_polling_error:`Failed to check authentication status:`};" +
		"const ru=JSON.parse(`" + `{"kimi_oauth_polling_error":"Failed to check authentication status:","other":"kept"}` + "`);</script>")
	got, err := AddWorkBuddyOAuthToManagementHTML(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"id:`workbuddy`", "id:`workbuddy-ai`", "WorkBuddy 国内站", "WorkBuddy 国际站", "workbuddy_ai_oauth_button"} {
		if !bytes.Contains(got, []byte(marker)) {
			t.Fatalf("missing %s", marker)
		}
	}
	again, err := AddWorkBuddyOAuthToManagementHTML(got)
	if err != nil || !bytes.Equal(got, again) {
		t.Fatal("OAuth injection is not idempotent")
	}
	start := bytes.Index(got, []byte("JSON.parse(`")) + len("JSON.parse(`")
	end := bytes.Index(got[start:], []byte("`);"))
	var locale map[string]string
	if err := json.Unmarshal(got[start:start+end], &locale); err != nil || locale["workbuddy_ai_oauth_title"] != "WorkBuddy AI OAuth" || locale["other"] != "kept" {
		t.Fatal("serialized locale was corrupted")
	}
}

func TestAddWorkBuddyQuotaRegistersNativePageAndAuthFileCards(t *testing.T) {
	input, err := os.ReadFile("../../static/management.html")
	if err != nil {
		t.Fatal(err)
	}
	got, err := AddWorkBuddyQuotaToManagementHTML(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"function cpaWorkBuddyQuota(", "function cpaWorkBuddyQuotaBody(",
		"workbuddyQuota:{}", "workbuddyAIQuota:{}", "workbuddyQuota:i(e.workbuddyQuota)", "workbuddyAIQuota:i(e.workbuddyAIQuota)",
		"workbuddy:cpaWorkBuddyQuota", `"workbuddy-ai":cpaWorkBuddyQuota`,
		"l.storeSelector(e)[u]", "/credentials/workbuddy-quota", "workbuddy_quota", "WorkBuddy 国内站", "WorkBuddy 国际站",
	} {
		if !bytes.Contains(got, []byte(marker)) {
			t.Fatalf("missing native quota integration: %s", marker)
		}
	}
	again, err := AddWorkBuddyQuotaToManagementHTML(got)
	if err != nil || !bytes.Equal(got, again) {
		t.Fatal("quota injection is not idempotent")
	}
}
