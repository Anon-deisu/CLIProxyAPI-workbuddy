package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// FetchQoderModels fetches the selected account's chat catalog. Successful
// responses, including an empty chat array, replace only that account/provider's
// retained registry catalog. Errors leave it intact for the caller's fallback.
// The caller still owns availability registration and account-removal cleanup.
func FetchQoderModels(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]*registry.ModelInfo, error) {
	if auth == nil || (auth.Provider != "qoder" && auth.Provider != "qoder-cn") {
		return nil, fmt.Errorf("qoder: model discovery requires a Qoder account")
	}
	body := EncodeQoderBody([]byte("{}"))
	endpoint := strings.TrimRight(qoder.GatewayForAuth(auth), "/") + "/algo/api/v2/model/list?Encode=1"
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return nil, fmt.Errorf("qoder: create model catalog request: %w", errRequest)
	}
	if errSign := SignQoderRequest(req, auth, body, "", false); errSign != nil {
		return nil, fmt.Errorf("qoder: sign model catalog request: %w", errSign)
	}
	req.Header.Set("Accept", "application/json")
	client := NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, errFetch := DoQoderGatewayRequest(client, req, auth)
	if errFetch != nil {
		return nil, fmt.Errorf("qoder: fetch model catalog: %w", errFetch)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("qoder: close model catalog response")
		}
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("qoder: model catalog returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Chat    json.RawMessage `json:"chat"`
		Code    json.RawMessage `json:"code"`
		Error   json.RawMessage `json:"error"`
		Success *bool           `json:"success"`
	}
	decoder := json.NewDecoder(response.Body)
	if errDecode := decoder.Decode(&payload); errDecode != nil {
		return nil, fmt.Errorf("qoder: invalid model catalog: %w", errDecode)
	}
	code := strings.Trim(string(bytes.TrimSpace(payload.Code)), `"`)
	hasError := len(payload.Error) > 0 && !bytes.Equal(bytes.TrimSpace(payload.Error), []byte("null"))
	if (payload.Success != nil && !*payload.Success) || hasError || (code != "" && code != "null" && code != "0" && code != "200") {
		return nil, fmt.Errorf("qoder: model catalog returned an upstream error")
	}
	// A missing/null chat field can be an upstream error envelope, not an empty
	// catalog. Do not erase the last good catalog on a nominal HTTP 200 error.
	chat := bytes.TrimSpace(payload.Chat)
	if len(chat) == 0 || chat[0] != '[' {
		return nil, fmt.Errorf("qoder: model catalog has no chat array")
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return nil, fmt.Errorf("qoder: model catalog contains trailing data")
	}
	var rows []json.RawMessage
	if errDecode := json.Unmarshal(chat, &rows); errDecode != nil {
		return nil, fmt.Errorf("qoder: invalid chat catalog: %w", errDecode)
	}
	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var entry map[string]any
		if errEntry := json.Unmarshal(row, &entry); errEntry != nil {
			continue
		}
		if key, ok := entry["key"].(string); ok && strings.TrimSpace(key) != "" {
			entries = append(entries, entry)
		}
	}
	if len(rows) > 0 && len(entries) == 0 {
		return nil, fmt.Errorf("qoder: model catalog contains no valid entries")
	}
	return registry.SetQoderModelsForClient(qoderCatalogClientID(auth), auth.Provider, entries)
}

// ResolveQoderModel resolves a public ID, configured alias, catalog alias or
// upstream key within the selected account and provider. Unknown names pass
// through unchanged. Once a dynamic catalog exists, removed models are never
// resolved through the static snapshot, even when the new catalog is empty.
func ResolveQoderModel(auth *coreauth.Auth, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "auto"
	}
	if auth == nil || (auth.Provider != "qoder" && auth.Provider != "qoder-cn") {
		return model
	}
	name := model
	if auth.Prefix != "" {
		name = strings.TrimPrefix(name, strings.Trim(auth.Prefix, "/")+"/")
	}
	if strings.HasPrefix(strings.ToLower(name), auth.Provider+"/") {
		name = name[len(auth.Provider)+1:]
	}
	models, metadata, dynamic := registry.GetQoderCatalogForClient(qoderCatalogClientID(auth), auth.Provider)
	var registered []*registry.ModelInfo
	if auth.ID != "" {
		for _, info := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
			if info != nil && info.Type == auth.Provider {
				registered = append(registered, info)
			}
		}
	}
	if !dynamic {
		if len(registered) > 0 {
			models = registered
		} else {
			models = registry.GetQoderModels(auth.Provider)
		}
	}
	if key := qoderFindModel(models, name); key != "" {
		return key
	}
	// OAuth aliases preserve MetadataModelID. Only accept an alias if its target
	// remains in the active catalog, so a stale registration cannot revive a model.
	registeredKey := qoderFindModel(registered, model)
	if registeredKey == "" {
		registeredKey = qoderFindModel(registered, name)
	}
	if key := registeredKey; key != "" {
		if active := qoderFindModel(models, key); active != "" {
			return active
		}
	}
	for _, entry := range metadata {
		key, _ := entry["key"].(string)
		for _, field := range []string{"name", "id", "name_local"} {
			if alias, ok := entry[field].(string); ok && strings.EqualFold(name, alias) {
				return key
			}
		}
		if aliases, ok := entry["aliases"].([]any); ok {
			for _, raw := range aliases {
				if alias, ok := raw.(string); ok && strings.EqualFold(name, alias) {
					return key
				}
			}
		}
	}
	// Accept the upstream's legacy "key (Display Name)" form without confusing
	// a provider's key with a different region's version of that model.
	if head, tail, ok := strings.Cut(name, " ("); ok && strings.HasSuffix(tail, ")") {
		name = strings.TrimSuffix(tail, ")")
		if headKey := qoderFindModel(models, strings.TrimSpace(head)); headKey != "" {
			tailKey := qoderFindModel(models, name)
			if tailKey == "" {
				tailKey = qoderFindModel(models, qoderModelAlias(auth.Provider, name))
			}
			if tailKey == headKey {
				return headKey
			}
			return model
		}
		if key := qoderFindModel(models, name); key != "" {
			return key
		}
	}
	if target := qoderModelAlias(auth.Provider, name); target != "" {
		if key := qoderFindModel(models, target); key != "" {
			return key
		}
	}
	return model
}

func qoderCatalogClientID(auth *coreauth.Auth) string {
	if auth.ID != "" {
		return auth.ID
	}
	// Reuse the auth package's account identity, including organization scope,
	// when discovery runs before an imported record is assigned its ID.
	if imported := qoder.NewAuthRecord(auth.Provider, qoder.CredentialsFromAuth(auth)); imported != nil {
		return imported.ID
	}
	return ""
}

func qoderFindModel(models []*registry.ModelInfo, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, info := range models {
		if info == nil {
			continue
		}
		key := info.MetadataModelID
		if key == "" {
			continue
		}
		for _, alias := range []string{info.ID, info.Name, info.DisplayName, key} {
			if strings.EqualFold(name, alias) {
				return key
			}
		}
	}
	return ""
}

func qoderModelAlias(provider, name string) string {
	// Resolve historical aliases through a current public name, not a frozen
	// key. This also handles a newly assigned key for the same public model.
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "qoder-auto":
		return "Auto"
	case "qwen3.8-max-preview", "qmodel_preview":
		return "Qwen3.8-Max"
	case "qwen3.6-flash":
		return "q36fmodel"
	case "deepseek-v4-flash":
		return "DeepSeek-Flash"
	case "kimi-k2.7", "kimi-k2.7-code":
		return "Kimi-K2.8-Preview"
	case "minimax-m2.5":
		if provider == "qoder-cn" {
			return "MiniMax-M2.7"
		}
	case "极致":
		return "Ultimate"
	case "经济":
		return "Efficient"
	case "性能":
		return "Performance"
	}
	return ""
}
