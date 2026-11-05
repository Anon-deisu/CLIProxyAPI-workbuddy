package helps

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestWorkBuddyAccountCatalogIsRegionalAndEnterpriseScoped(t *testing.T) {
	for _, provider := range []string{workbuddy.ProviderCN, workbuddy.ProviderAI} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v3/config" || r.Header.Get("X-Enterprise-Id") != "team" || r.Header.Get("User-Agent") != "CLI/"+workbuddy.AppVersion+" WorkBuddy/"+workbuddy.UserAgentVersion {
					t.Error("catalog did not use the CLI and enterprise identity")
				}
				_, _ = io.WriteString(w, `{"code":0,"data":{"agents":[{"name":"ide","models":["hidden"]},{"name":"cli","models":["glm-test","gpt-test"]}],"models":[{"id":"glm-test","name":"GLM","maxInputTokens":123456,"maxOutputTokens":8192,"supportsImages":true,"reasoning":{"supportedEfforts":["low","high"],"canDisableThinking":true}},{"id":"gpt-test","supportsImages":true}]}}`)
			}))
			defer server.Close()
			auth := workbuddy.NewAuthRecord(provider, workbuddy.Credentials{UID: "uid", AccessToken: "token", EnterpriseID: "team"})
			auth.Metadata["base_url"] = server.URL
			models, err := FetchWorkBuddyModels(t.Context(), &config.Config{}, auth)
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 2 || models[0].ID != "glm-test" || models[0].ContextLength != 123456 || models[0].MaxCompletionTokens != 8192 || !reflect.DeepEqual(models[0].Thinking.Levels, []string{"none", "low", "high"}) {
				t.Fatal("catalog lost CLI filtering, model limits or reasoning levels")
			}
			expectedGLMInputs := []string{"text", "image"}
			if provider == workbuddy.ProviderAI {
				expectedGLMInputs = []string{"text"}
			}
			if !reflect.DeepEqual(models[0].SupportedInputModalities, expectedGLMInputs) || len(models[1].SupportedInputModalities) != 2 {
				t.Fatal("regional GLM image capability was not preserved")
			}
		})
	}
}
