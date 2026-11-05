package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// GetQoderModels returns an independent regional fallback snapshot. The source is
// qoder_catalog_cn.json / qoder_catalog_intl.json at qoder2api-hub 45e76f9,
// not the older frozen copies in qoder_catalog.py. Snapshot enable flags describe
// the source account, not the current account's entitlement.
func GetQoderModels(provider string) []*ModelInfo {
	provider = strings.ToLower(strings.TrimSpace(provider))
	entries := qoderStaticCatalog(provider)
	models := make([]*ModelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(models, qoderModelInfo(provider, entry, true))
	}
	return models
}

type qoderCatalogKey struct {
	clientID string
	provider string
}

type qoderClientCatalog struct {
	models   []*ModelInfo
	metadata []map[string]any
}

// Keep the last successful catalog, including an explicitly empty one, separate
// from availability registrations: RegisterClient removes empty registrations.
// There is no TTL and no network cache; each fetch asks the upstream again.
var qoderClientCatalogs = struct {
	sync.RWMutex
	entries map[qoderCatalogKey]qoderClientCatalog
}{entries: make(map[qoderCatalogKey]qoderClientCatalog)}

// SetQoderModelsForClient atomically replaces one account/provider catalog. Only
// entries in the new catalog survive; static entries only fill missing or null
// metadata for matching keys. It does not register model availability.
// Unknown metadata (including enable, strategies and price multipliers) is kept
// as JSON data. Invalid input leaves the previous snapshot intact.
func SetQoderModelsForClient(clientID, provider string, entries []map[string]any) ([]*ModelInfo, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if strings.TrimSpace(clientID) == "" || (provider != "qoder" && provider != "qoder-cn") {
		return nil, fmt.Errorf("qoder: model catalog requires an account and a Qoder provider")
	}
	// A JSON round trip both validates the metadata and detaches all nested values.
	raw, errMarshal := json.Marshal(entries)
	if errMarshal != nil {
		return nil, fmt.Errorf("qoder: invalid model metadata: %w", errMarshal)
	}
	var detached []map[string]any
	if errUnmarshal := json.Unmarshal(raw, &detached); errUnmarshal != nil {
		return nil, fmt.Errorf("qoder: invalid model metadata: %w", errUnmarshal)
	}
	static := qoderStaticCatalog(provider)
	byKey := make(map[string]map[string]any, len(static))
	for _, entry := range static {
		byKey[qoderString(entry, "key")] = entry
	}
	models := make([]*ModelInfo, 0, len(detached))
	metadata := make([]map[string]any, 0, len(detached))
	seenKeys, seenIDs := make(map[string]bool), make(map[string]bool)
	for _, entry := range detached {
		key := qoderString(entry, "key")
		if key == "" {
			return nil, fmt.Errorf("qoder: model catalog entry has no key")
		}
		if !qoderIsChatModel(key) {
			continue
		}
		if seenKeys[strings.ToLower(key)] {
			return nil, fmt.Errorf("qoder: model catalog contains duplicate keys")
		}
		seenKeys[strings.ToLower(key)] = true
		base := qoderCloneMetadata(byKey[key])
		if base == nil {
			base = make(map[string]any)
		}
		for field, value := range entry {
			if value != nil || base[field] == nil {
				base[field] = value
			}
		}
		base["key"] = key
		model := qoderModelInfo(provider, base, false)
		if seenIDs[strings.ToLower(model.ID)] {
			return nil, fmt.Errorf("qoder: model catalog contains duplicate public names")
		}
		seenIDs[strings.ToLower(model.ID)] = true
		models = append(models, model)
		metadata = append(metadata, base)
	}
	// Preserve the official regional UI order, then append newly discovered keys.
	order := make(map[string]int, len(static))
	for index, entry := range static {
		order[qoderString(entry, "key")] = index
	}
	sort.SliceStable(metadata, func(i, j int) bool {
		left, leftKnown := order[qoderString(metadata[i], "key")]
		right, rightKnown := order[qoderString(metadata[j], "key")]
		if leftKnown != rightKnown {
			return leftKnown
		}
		return leftKnown && left < right
	})
	for index, entry := range metadata {
		models[index] = qoderModelInfo(provider, entry, false)
	}
	qoderClientCatalogs.Lock()
	qoderClientCatalogs.entries[qoderCatalogKey{clientID, provider}] = qoderClientCatalog{models, metadata}
	qoderClientCatalogs.Unlock()
	return qoderCloneModels(models), nil
}

// GetQoderCatalogForClient returns detached model and metadata snapshots. The
// boolean distinguishes a successful empty catalog from no successful fetch.
// Metadata retains upstream keys, enable flags and provider-native pricing;
// price_factor is a credit multiplier, never a USD token price.
func GetQoderCatalogForClient(clientID, provider string) ([]*ModelInfo, []map[string]any, bool) {
	key := qoderCatalogKey{clientID, strings.ToLower(strings.TrimSpace(provider))}
	qoderClientCatalogs.RLock()
	catalog, ok := qoderClientCatalogs.entries[key]
	qoderClientCatalogs.RUnlock()
	if !ok {
		return nil, nil, false
	}
	// Published catalogs are immutable; cloning does not need to hold the lock.
	raw, _ := json.Marshal(catalog.metadata)
	var metadata []map[string]any
	_ = json.Unmarshal(raw, &metadata)
	return qoderCloneModels(catalog.models), metadata, true
}

// DeleteQoderModelsForClient releases the retained catalog when an account is removed.
func DeleteQoderModelsForClient(clientID, provider string) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	qoderClientCatalogs.Lock()
	if provider == "" {
		delete(qoderClientCatalogs.entries, qoderCatalogKey{clientID, "qoder"})
		delete(qoderClientCatalogs.entries, qoderCatalogKey{clientID, "qoder-cn"})
	} else {
		delete(qoderClientCatalogs.entries, qoderCatalogKey{clientID, provider})
	}
	qoderClientCatalogs.Unlock()
}

func qoderCloneModels(models []*ModelInfo) []*ModelInfo {
	cloned := make([]*ModelInfo, len(models))
	for index, model := range models {
		cloned[index] = cloneModelInfo(model)
	}
	return cloned
}

func qoderIsChatModel(key string) bool {
	if key == "lite" || strings.HasPrefix(key, "codewise-") || strings.HasPrefix(key, "completion-") {
		return false
	}
	for _, suffix := range []string{"-image-alpha", "-image-alpha-edit", "-taco-completion"} {
		if strings.HasSuffix(key, suffix) {
			return false
		}
	}
	return true
}

func qoderModelInfo(provider string, entry map[string]any, snapshot bool) *ModelInfo {
	key := qoderString(entry, "key")
	name := qoderString(entry, "display_name", "name", "key")
	model := &ModelInfo{
		ID: name, MetadataModelID: key, Name: name, DisplayName: name,
		Object: "model", OwnedBy: provider, Type: provider,
		Description:              qoderString(entry, "description"),
		SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"},
	}
	if snapshot {
		model.Description = "Qoder catalog snapshot; account access requires a live catalog."
	}
	if enabled, ok := entry["enable"].(bool); ok && !enabled {
		model.Description = strings.TrimSpace(model.Description + " Restricted in the source catalog (enable=false); account or plan access is required.")
	}
	if (qoderBool(entry, "is_vl") || qoderBool(entry, "supportsImages")) && !qoderBool(entry, "disabledMultimodal") {
		model.SupportedInputModalities = append(model.SupportedInputModalities, "image")
	}
	model.InputTokenLimit = qoderPositiveInt(entry, "maxInputTokens", "max_input_tokens")
	model.ContextLength = model.InputTokenLimit
	contexts, _ := entry["context_config"].(map[string]any)
	var defaultContext int
	for _, raw := range contexts {
		window, _ := raw.(map[string]any)
		tokens := qoderPositiveInt(window, "token_count")
		if qoderBool(window, "is_default") && (defaultContext == 0 || tokens < defaultContext) {
			defaultContext = tokens
		}
	}
	// max_input_tokens is the upstream's advertised default input limit, which
	// can be smaller than the selected context window (e.g. CN DeepSeek: 96K).
	// Optional larger windows remain in metadata and are not advertised as active.
	if model.ContextLength == 0 {
		model.ContextLength = defaultContext
	}
	model.OutputTokenLimit = qoderPositiveInt(entry, "maxOutputTokens", "max_output_tokens")
	model.MaxCompletionTokens = model.OutputTokenLimit
	if tools, ok := entry["supportsToolCall"].(bool); !ok || tools {
		model.SupportedParameters = []string{"tools", "tool_choice"}
	}
	thinking, _ := entry["thinking_config"].(map[string]any)
	enabled, hasEnabled := thinking["enabled"].(map[string]any)
	if hasEnabled || qoderBool(entry, "is_reasoning") || qoderBool(entry, "supportsReasoning") {
		_, canDisable := thinking["disabled"]
		model.Thinking = &ThinkingSupport{ZeroAllowed: canDisable}
		efforts, _ := enabled["efforts"].(map[string]any)
		if len(efforts) > 0 {
			if canDisable {
				model.Thinking.Levels = append(model.Thinking.Levels, "none")
			}
			for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
				if _, exists := efforts[level]; exists {
					model.Thinking.Levels = append(model.Thinking.Levels, level)
				}
			}
			model.SupportedParameters = append(model.SupportedParameters, "reasoning_effort")
		}
		// No token budget or effort is invented for on/off-only reasoning.
	}
	return model
}

func qoderString(entry map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := entry[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func qoderBool(entry map[string]any, key string) bool {
	value, _ := entry[key].(bool)
	return value
}

func qoderCloneMetadata(entry map[string]any) map[string]any {
	if entry == nil {
		return nil
	}
	raw, errMarshal := json.Marshal(entry)
	if errMarshal != nil {
		return nil
	}
	var cloned map[string]any
	if errUnmarshal := json.Unmarshal(raw, &cloned); errUnmarshal != nil {
		return nil
	}
	return cloned
}

func qoderPositiveInt(entry map[string]any, keys ...string) int {
	for _, key := range keys {
		var value int
		switch number := entry[key].(type) {
		case int:
			value = number
		case float64:
			if number > 0 && number < float64(int(^uint(0)>>1)) && number == float64(int(number)) {
				value = int(number)
			}
		}
		if value > 0 {
			return value
		}
	}
	return 0
}

// Only fields used by CPA's capability projection are transcribed here. Live
// catalogs retain every upstream field. In particular Sonus/Cantus are opaque
// upstream names, and no hidden model version or output limit is inferred.
func qoderStaticCatalog(provider string) []map[string]any {
	type definition struct {
		key, name                 string
		enable, vision, reasoning bool
		input                     int
		context                   string
		efforts, defaultEffort    string
		disable                   bool
	}
	var definitions []definition
	switch provider {
	case "qoder-cn":
		definitions = []definition{
			{"auto", "Auto", true, true, true, 200000, "", "", "", false},
			{"qmodel_38max", "Qwen3.8-Max", true, true, true, 180000, "200K", "low medium xhigh", "medium", true},
			{"qfmodel", "Qwen3.8-Flash", true, true, true, 180000, "200K", "low medium xhigh", "medium", true},
			{"qmodel_latest", "Qwen3.7-Max", true, true, true, 180000, "200K", "on", "", true},
			{"qmodel", "Qwen3.7-Plus", true, true, true, 180000, "200K", "on", "", true},
			{"q37fmodel", "Qwen3.7-Flash", true, true, true, 180000, "200K", "", "", false},
			{"dmodel", "DeepSeek-V4-Pro", true, true, true, 96000, "200K", "high max", "max", true},
			{"dfmodel", "DeepSeek-Flash", true, true, false, 180000, "200K", "low high max", "max", true},
			{"gmodel", "GLM-5.3", true, true, true, 180000, "200K", "low high max", "max", false},
			{"gfmodel", "GLM-5.3-Flash", true, true, true, 1000000, "200K", "high max", "max", false},
			{"gm51model", "GLM-5.2", true, true, true, 180000, "200K", "high max", "max", true},
			{"kmodel_latest", "Kimi-K3", true, true, false, 180000, "200K", "low high max", "max", false},
			{"kmodel", "Kimi-K2.8-Preview", true, true, true, 180000, "200K", "low high max", "max", false},
			{"mmodel", "MiniMax-M2.7", true, false, false, 180000, "200K-only", "", "", false},
		}
	case "qoder":
		definitions = []definition{
			{"auto", "Auto", false, true, false, 200000, "", "", "", false},
			{"ultimate", "Ultimate", false, true, true, 1000000, "200K", "low medium high xhigh max", "high", true},
			{"performance", "Performance", false, true, false, 1000000, "272K", "low medium high xhigh max", "medium", true},
			{"efficient", "Efficient", false, true, false, 200000, "", "", "", false},
			{"smodel", "Sonus", false, true, true, 180000, "200K", "low medium high xhigh max", "high", false},
			{"cmodel", "Cantus", false, true, true, 180000, "200K", "low medium high xhigh max", "high", false},
			{"qmodel_38max", "Qwen3.8-Max", true, true, true, 180000, "200K", "low medium xhigh", "xhigh", true},
			{"qfmodel", "Qwen3.8-Flash", true, true, true, 180000, "200K", "low medium xhigh", "medium", true},
			{"qmodel_latest", "Qwen3.7-Max", false, true, false, 1000000, "200K", "on", "", true},
			{"qmodel", "Qwen3.7-Plus", false, true, false, 1000000, "200K", "on", "", true},
			{"kmodel_latest", "Kimi-K3", false, true, false, 180000, "200K", "low high max", "max", false},
			{"kmodel", "Kimi-K2.8-Preview", false, true, false, 0, "200K", "low high max", "max", false},
			{"gmodel", "GLM-5.3", false, true, true, 180000, "200K", "low high max", "max", false},
			{"gfmodel", "GLM-5.3-Flash", false, true, true, 1000000, "200K", "high max", "max", false},
			{"dmodel", "DeepSeek-V4-Pro", false, true, true, 1000000, "200K", "high max", "max", true},
			{"dfmodel", "DeepSeek-Flash", false, true, true, 1000000, "200K", "low high max", "max", true},
			{"mmodel", "MiniMax-M3", false, true, false, 180000, "200K", "", "", false},
		}
	}
	entries := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		entry := map[string]any{
			"key": definition.key, "display_name": definition.name, "enable": definition.enable,
			"is_vl": definition.vision, "is_reasoning": definition.reasoning,
		}
		if definition.input > 0 {
			entry["max_input_tokens"] = definition.input
		}
		if definition.context != "" {
			label, tokens := "200K", 200000
			if definition.context == "272K" {
				label, tokens = "272K", 272000
			}
			contexts := map[string]any{label: map[string]any{"token_count": tokens, "is_default": true}}
			if definition.context != "200K-only" {
				contexts["400K"] = map[string]any{"token_count": 400000}
				contexts["1M"] = map[string]any{"token_count": 1000000}
			}
			entry["context_config"] = contexts
		}
		if definition.efforts != "" {
			enabled := map[string]any{"is_default": true}
			if definition.efforts != "on" {
				efforts := make(map[string]any)
				for _, level := range strings.Fields(definition.efforts) {
					value := map[string]any{}
					if level == definition.defaultEffort {
						value["is_default"] = true
					}
					efforts[level] = value
				}
				enabled["efforts"] = efforts
			}
			thinking := map[string]any{"enabled": enabled}
			if definition.disable {
				thinking["disabled"] = map[string]any{}
			}
			entry["thinking_config"] = thinking
		}
		entries = append(entries, entry)
	}
	return entries
}
