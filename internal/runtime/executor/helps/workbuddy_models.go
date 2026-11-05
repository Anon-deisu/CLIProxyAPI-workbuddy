package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// FetchWorkBuddyModels reads the CLI catalog under the selected account and enterprise.
func FetchWorkBuddyModels(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]*registry.ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, workbuddy.EndpointForAuth(auth)+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	workbuddy.SignRequest(req, auth.Provider, workbuddy.CredentialsFromAuth(auth))
	req.Header.Set("User-Agent", "CLI/"+workbuddy.AppVersion+" WorkBuddy/"+workbuddy.UserAgentVersion)
	req.Header.Set("Accept", "application/json")
	res, err := NewProxyAwareHTTPClient(ctx, cfg, auth, 0).Do(req)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: fetch model catalog: %w", err)
	}
	defer func() {
		if errClose := res.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("workbuddy: close model catalog")
		}
	}()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("workbuddy: model catalog returned HTTP %d", res.StatusCode)
	}
	var envelope struct {
		Code json.Number `json:"code"`
		Data struct {
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
			Models []struct {
				ID, Name        string
				MaxInputTokens  int
				MaxOutputTokens int
				SupportsImages  bool
				OnlyReasoning   bool
				Reasoning       struct {
					SupportedEfforts   []string
					CanDisableThinking *bool
				}
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("workbuddy: invalid model catalog: %w", err)
	}
	if code, _ := envelope.Code.Int64(); code != 0 {
		return nil, fmt.Errorf("workbuddy: model catalog error %d", code)
	}
	var models []*registry.ModelInfo
	seen := make(map[string]bool)
	for _, agent := range envelope.Data.Agents {
		if agent.Name != "cli" {
			continue
		}
		for _, id := range agent.Models {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			model := &registry.ModelInfo{ID: id, Name: id, DisplayName: id, Object: "model", OwnedBy: auth.Provider, Type: auth.Provider,
				SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}}
			for _, detail := range envelope.Data.Models {
				if detail.ID != id {
					continue
				}
				if detail.Name != "" {
					model.DisplayName = detail.Name
				}
				model.ContextLength, model.MaxCompletionTokens = detail.MaxInputTokens, detail.MaxOutputTokens
				if detail.SupportsImages && !(auth.Provider == workbuddy.ProviderAI && strings.HasPrefix(id, "glm-")) {
					model.SupportedInputModalities = append(model.SupportedInputModalities, "image")
				}
				if len(detail.Reasoning.SupportedEfforts) > 0 {
					levels := append([]string(nil), detail.Reasoning.SupportedEfforts...)
					canDisable := !detail.OnlyReasoning && (detail.Reasoning.CanDisableThinking == nil || *detail.Reasoning.CanDisableThinking)
					if canDisable && !containsWorkBuddyLevel(levels, "none") {
						levels = append([]string{"none"}, levels...)
					}
					model.Thinking = &registry.ThinkingSupport{Levels: levels, ZeroAllowed: canDisable}
				}
				break
			}
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("workbuddy: model catalog contains no CLI models")
	}
	return models, nil
}

func containsWorkBuddyLevel(levels []string, level string) bool {
	for _, candidate := range levels {
		if candidate == level {
			return true
		}
	}
	return false
}
