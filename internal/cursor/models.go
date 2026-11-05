package cursor

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
	aiserverv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/aiserver/v1"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	usableModelsAiServicePath    = "/aiserver.v1.AiService/GetUsableModels"
	usableModelsAgentServicePath = "/agent.v1.AgentService/GetUsableModels"
)

// CatalogVariant is one AvailableModels variant (thinking/effort/fast combo).
type CatalogVariant struct {
	Parameters        []ModelParameter
	MaxMode           bool
	IsDefaultMax      bool
	IsDefaultNonMax   bool
	LegacySlug        string
	VariantStringRepr string
}

// CatalogModel is one account-visible Cursor model from AvailableModels.
type CatalogModel struct {
	ID           string
	DisplayName  string
	DisplayModel string
	Aliases      []string
	Thinking     bool
	SupportsImg  bool
	MaxMode      bool
	SupportsMax  bool
	ContextLimit int
	Parameters   []ModelParameter
	Variants     []CatalogVariant
	// WireID is optional GetUsableModels id (legacy). cursor2api sends the
	// public base id + parameters instead; WireID is kept for diagnostics.
	WireID string
}

type catalogCache struct {
	mu      sync.RWMutex
	byModel map[string]CatalogModel
	usable  []usableModel
	fetched time.Time
}

var globalCatalogCache = &catalogCache{byModel: map[string]CatalogModel{}}

// RememberCatalog merges fetched models into the process-wide catalog cache
// used by ResolveRequestedModel for default parameters.
func RememberCatalog(models []CatalogModel) {
	if len(models) == 0 {
		return
	}
	globalCatalogCache.mu.Lock()
	defer globalCatalogCache.mu.Unlock()
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		globalCatalogCache.byModel[strings.ToLower(id)] = model
	}
	globalCatalogCache.fetched = time.Now()
}

func rememberUsableModels(usable []usableModel) {
	globalCatalogCache.mu.Lock()
	defer globalCatalogCache.mu.Unlock()
	globalCatalogCache.usable = append([]usableModel(nil), usable...)
}

func cachedUsableModels() []usableModel {
	globalCatalogCache.mu.RLock()
	defer globalCatalogCache.mu.RUnlock()
	return append([]usableModel(nil), globalCatalogCache.usable...)
}

func catalogEntry(modelID string) (CatalogModel, bool) {
	globalCatalogCache.mu.RLock()
	defer globalCatalogCache.mu.RUnlock()
	entry, ok := globalCatalogCache.byModel[strings.ToLower(strings.TrimSpace(modelID))]
	return entry, ok
}

// FetchAvailableModels calls AiService/AvailableModels for one account.
func FetchAvailableModels(ctx context.Context, creds AccountCredentials) ([]CatalogModel, error) {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, ErrMissingAccessToken
	}
	if creds.BaseURL == "" {
		creds.BaseURL = cursorauth.DefaultBaseURL
	}
	if creds.ClientVersion == "" {
		creds.ClientVersion = cursorauth.DefaultClientVersion
	}
	if creds.MachineID == "" {
		creds.MachineID = DesktopMachineID()
	}
	if creds.ClientOS == "" {
		creds.ClientOS = DesktopClientOS()
	}
	if creds.ClientArch == "" {
		creds.ClientArch = DesktopClientArch()
	}
	if creds.SessionID == "" {
		creds.SessionID = uuid.NewString()
	}

	profile := ProfileFromCredentials(creds)
	requestID := uuid.NewString()
	headers, err := profile.Headers(creds.AccessToken, requestID, "")
	if err != nil {
		return nil, err
	}
	headers["content-type"] = "application/proto"
	headers["accept-encoding"] = "gzip, br"
	delete(headers, "x-cursor-streaming")

	useParams := true
	usePicker := true
	req := &aiserverv1.AvailableModelsRequest{
		IsNightly:                strings.Contains(strings.ToLower(creds.ClientVersion), "nightly"),
		IncludeLongContextModels: true,
		ExcludeMaxNamedModels:    true,
		UseModelParameters:       &useParams,
		UseReactModelPicker:      &usePicker,
	}
	resp := &aiserverv1.AvailableModelsResponse{}
	respHdr, err := UnaryPOSTWithHeader(ctx, creds.BaseURL, availableModelsPath, headers, req, resp)
	if err != nil {
		return nil, err
	}
	if profile.CookieJar != nil {
		profile.CookieJar.RememberResponse(creds.BaseURL, respHdr)
		// Rebuild cookie header for follow-up usable-models calls.
		if jarCookie := profile.CookieJar.Header(creds.BaseURL); jarCookie != "" {
			headers["cookie"] = jarCookie
		}
	}
	models := normalizeAvailableModels(resp)
	if usable, errUsable := fetchUsableModels(ctx, creds.BaseURL, headers, profile.CookieJar); errUsable == nil {
		rememberUsableModels(usable)
		attachWireIDs(models, usable)
	}
	RememberCatalog(models)
	return models, nil
}

type usableModel struct {
	ID          string
	DisplayName string
	Aliases     []string
}

func fetchUsableModels(ctx context.Context, baseURL string, headers map[string]string, jar *CookieJar) ([]usableModel, error) {
	paths := []string{usableModelsAiServicePath, usableModelsAgentServicePath}
	var lastErr error
	for i, path := range paths {
		req := &agentv1.GetUsableModelsRequest{}
		resp := &agentv1.GetUsableModelsResponse{}
		respHdr, err := UnaryPOSTWithHeader(ctx, baseURL, path, headers, req, resp)
		if err != nil {
			lastErr = err
			errText := err.Error()
			if i == 0 && (strings.Contains(errText, "HTTP 404") || strings.Contains(errText, "HTTP 501")) {
				continue
			}
			return nil, err
		}
		if jar != nil {
			jar.RememberResponse(baseURL, respHdr)
		}
		out := make([]usableModel, 0, len(resp.GetModels()))
		for _, model := range resp.GetModels() {
			id := strings.TrimSpace(model.GetModelId())
			if id == "" {
				continue
			}
			out = append(out, usableModel{
				ID:          id,
				DisplayName: strings.TrimSpace(model.GetDisplayName()),
				Aliases:     append([]string(nil), model.GetAliases()...),
			})
		}
		return out, nil
	}
	return nil, lastErr
}

func attachWireIDs(models []CatalogModel, usable []usableModel) {
	if len(models) == 0 || len(usable) == 0 {
		return
	}
	for i := range models {
		models[i].WireID = matchUsableWireID(models[i], usable)
	}
}

func matchUsableWireID(model CatalogModel, usable []usableModel) string {
	publicID := strings.TrimSpace(model.ID)
	if publicID == "" {
		return ""
	}
	publicFold := strings.ToLower(publicID)
	param := map[string]string{}
	for _, p := range model.Parameters {
		param[strings.ToLower(strings.TrimSpace(p.ID))] = strings.ToLower(strings.TrimSpace(p.Value))
	}
	wantFast := param["fast"] == "true"
	wantEffort := firstNonEmpty(param["effort"], param["reasoning"])

	type candidate struct {
		id    string
		score int
	}
	best := candidate{}
	for _, item := range usable {
		id := strings.TrimSpace(item.ID)
		idFold := strings.ToLower(id)
		score := 0
		switch {
		case strings.HasPrefix(idFold, "cursor-"+publicFold+"-"):
			// Prefer Agent-accepted variant strings over the public catalog name
			// (bare public ids often yield ERROR_BAD_MODEL_NAME).
			score = 100
		case strings.HasPrefix(idFold, publicFold+"-"):
			score = 90
		case idFold == publicFold:
			score = 40
		default:
			for _, alias := range item.Aliases {
				if strings.EqualFold(strings.TrimSpace(alias), publicID) {
					score = 60
					break
				}
			}
			if score == 0 && model.DisplayName != "" && strings.EqualFold(item.DisplayName, model.DisplayName) {
				score = 50
			}
			if score == 0 && model.DisplayName != "" && strings.Contains(strings.ToLower(item.DisplayName), strings.ToLower(model.DisplayName)) {
				score = 40
			}
		}
		if score == 0 {
			continue
		}
		hasFast := strings.Contains(idFold, "-fast")
		if wantFast && hasFast {
			score += 30
		} else if wantFast && !hasFast {
			score -= 25
		} else if !wantFast && hasFast {
			score -= 10
		} else if !wantFast && !hasFast {
			score += 5
		}
		if wantEffort != "" {
			if strings.Contains(idFold, "-"+wantEffort) || strings.HasSuffix(idFold, wantEffort) {
				score += 15
			}
			// Cursor sometimes encodes xhigh as high for grok wire ids.
			if wantEffort == "xhigh" && strings.Contains(idFold, "-high") {
				score += 8
			}
		}
		if score > best.score {
			best = candidate{id: id, score: score}
		}
	}
	if best.score >= 40 {
		return best.id
	}
	return ""
}

// ErrMissingAccessToken is returned when credentials lack an access token.
var ErrMissingAccessToken = errString("cursor: access_token is required")

type errString string

func (e errString) Error() string { return string(e) }

func normalizeAvailableModels(resp *aiserverv1.AvailableModelsResponse) []CatalogModel {
	if resp == nil {
		return nil
	}
	byID := map[string]CatalogModel{}
	for _, model := range resp.GetModels() {
		id := strings.TrimSpace(model.GetName())
		if id == "" {
			continue
		}
		display := id
		if v := strings.TrimSpace(model.GetClientDisplayName()); v != "" {
			display = v
		}
		displayModel := id
		if v := strings.TrimSpace(model.GetServerModelName()); v != "" {
			displayModel = v
		}
		variants := collectCatalogVariants(model)
		variant := preferCatalogVariant(variants, false, nil)
		params := []ModelParameter(nil)
		if variant != nil {
			params = append([]ModelParameter(nil), variant.Parameters...)
		} else if raw := defaultVariant(model); raw != nil {
			params = validVariantParameters(model, raw)
		}
		nonMaxDisabled := model.SupportsNonMaxMode != nil && !model.GetSupportsNonMaxMode()
		supportsMax := model.SupportsMaxMode != nil && model.GetSupportsMaxMode()
		for _, candidate := range variants {
			supportsMax = supportsMax || candidate.MaxMode
		}
		supportsMax = supportsMax || nonMaxDisabled
		maxMode := supportsMax
		contextLimit := int(model.GetContextTokenLimit())
		if maxMode && model.ContextTokenLimitForMaxMode != nil && model.GetContextTokenLimitForMaxMode() > 0 {
			contextLimit = int(model.GetContextTokenLimitForMaxMode())
		}
		entry := CatalogModel{
			ID:           id,
			DisplayName:  display,
			DisplayModel: displayModel,
			Aliases:      append([]string(nil), model.GetIdAliases()...),
			Thinking:     model.GetSupportsThinking(),
			SupportsImg:  model.GetSupportsImages(),
			MaxMode:      maxMode,
			SupportsMax:  supportsMax,
			ContextLimit: contextLimit,
			Variants:     variants,
		}
		if resp.GetUseModelParameters() && params != nil {
			entry.Parameters = params
		}
		byID[id] = entry
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return strings.ToLower(ids[i]) < strings.ToLower(ids[j])
	})
	out := make([]CatalogModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

func collectCatalogVariants(model *aiserverv1.AvailableModelsResponse_AvailableModel) []CatalogVariant {
	raw := model.GetVariants()
	if len(raw) == 0 {
		return nil
	}
	out := make([]CatalogVariant, 0, len(raw))
	for _, variant := range raw {
		if variant == nil {
			continue
		}
		params := variantParametersLoose(variant)
		if params == nil {
			// Fall back to strict validation used for wire safety.
			params = validVariantParameters(model, variant)
		}
		if params == nil {
			continue
		}
		out = append(out, CatalogVariant{
			Parameters:        params,
			MaxMode:           variant.GetIsMaxMode(),
			IsDefaultMax:      variant.IsDefaultMaxConfig != nil && variant.GetIsDefaultMaxConfig(),
			IsDefaultNonMax:   variant.IsDefaultNonMaxConfig != nil && variant.GetIsDefaultNonMaxConfig(),
			LegacySlug:        strings.TrimSpace(variant.GetLegacySlug()),
			VariantStringRepr: strings.TrimSpace(variant.GetVariantStringRepresentation()),
		})
	}
	return out
}

// variantParametersLoose keeps server-advertised parameter_values even when
// they don't cover every parameter_definition (needed for fast/thinking picks).
func variantParametersLoose(variant *aiserverv1.AvailableModelsResponse_ModelVariantConfig) []ModelParameter {
	if variant == nil {
		return nil
	}
	values := variant.GetParameterValues()
	if len(values) == 0 {
		return []ModelParameter{}
	}
	out := make([]ModelParameter, 0, len(values))
	seen := map[string]struct{}{}
	for _, item := range values {
		id := strings.TrimSpace(item.GetId())
		value := strings.TrimSpace(item.GetValue())
		if id == "" || value == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, ModelParameter{ID: id, Value: value})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// preferCatalogVariant mirrors cursor2api's maximum-context default:
// max mode -> thinking-on -> fast=true -> mode-specific advertised default.
func preferCatalogVariant(variants []CatalogVariant, explicitFast bool, desireFast *bool) *CatalogVariant {
	return preferCatalogVariantForMode(variants, explicitFast, desireFast, nil)
}

func preferCatalogVariantForMode(variants []CatalogVariant, explicitFast bool, desireFast *bool, desiredMax *bool) *CatalogVariant {
	if len(variants) == 0 {
		return nil
	}
	bestIdx := -1
	bestScore := [4]int{-1, -1, -1, -1}
	for i := range variants {
		v := variants[i]
		if desiredMax != nil && v.MaxMode != *desiredMax {
			continue
		}
		if explicitFast && desireFast != nil {
			fastVal := paramValue(v.Parameters, "fast") == "true"
			if fastVal != *desireFast {
				continue
			}
			if paramValue(v.Parameters, "fast") == "" {
				continue
			}
		}
		valuesFast := paramValue(v.Parameters, "fast") == "true"
		thinkingOn := paramValue(v.Parameters, "thinking") == "true" ||
			(paramValue(v.Parameters, "thinking") == "" &&
				(paramValue(v.Parameters, "effort") != "" || paramValue(v.Parameters, "reasoning") != ""))
		score := [4]int{
			boolToInt(v.MaxMode),
			boolToInt(thinkingOn),
			boolToInt(!explicitFast && valuesFast),
			boolToInt((v.MaxMode && v.IsDefaultMax) || (!v.MaxMode && v.IsDefaultNonMax)),
		}
		if bestIdx < 0 || compareScore(score, bestScore) > 0 {
			bestIdx = i
			bestScore = score
		}
	}
	if bestIdx < 0 {
		if desiredMax != nil {
			return nil
		}
		return &variants[0]
	}
	return &variants[bestIdx]
}

func paramValue(params []ModelParameter, id string) string {
	for _, p := range params {
		if strings.EqualFold(p.ID, id) {
			return strings.ToLower(strings.TrimSpace(p.Value))
		}
	}
	return ""
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func compareScore(a, b [4]int) int {
	for i := 0; i < 4; i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return 0
}

func defaultVariant(model *aiserverv1.AvailableModelsResponse_AvailableModel) *aiserverv1.AvailableModelsResponse_ModelVariantConfig {
	variants := model.GetVariants()
	if len(variants) == 0 {
		return nil
	}
	thinkingSupported := false
	for _, def := range model.GetParameterDefinitions() {
		if def.GetId() != "thinking" {
			continue
		}
		for _, value := range booleanParamValues(def) {
			if value == "true" {
				thinkingSupported = true
				break
			}
		}
	}
	maxVariants := make([]*aiserverv1.AvailableModelsResponse_ModelVariantConfig, 0, len(variants))
	nonMax := make([]*aiserverv1.AvailableModelsResponse_ModelVariantConfig, 0, len(variants))
	for _, variant := range variants {
		if variant.GetIsMaxMode() {
			maxVariants = append(maxVariants, variant)
		} else {
			nonMax = append(nonMax, variant)
		}
	}
	parameterValue := func(variant *aiserverv1.AvailableModelsResponse_ModelVariantConfig, id string) string {
		for _, item := range variant.GetParameterValues() {
			if item.GetId() == id {
				return item.GetValue()
			}
		}
		return ""
	}
	preferredMode := maxVariants
	if len(preferredMode) == 0 {
		preferredMode = nonMax
	}
	if thinkingSupported {
		thinkingVariants := make([]*aiserverv1.AvailableModelsResponse_ModelVariantConfig, 0)
		for _, variant := range preferredMode {
			if parameterValue(variant, "thinking") == "true" {
				thinkingVariants = append(thinkingVariants, variant)
			}
		}
		// Prefer advertised fast=true (cursor2api desktop default).
		for _, variant := range thinkingVariants {
			if parameterValue(variant, "fast") == "true" {
				return variant
			}
		}
		for _, variant := range thinkingVariants {
			if (variant.GetIsMaxMode() && variant.IsDefaultMaxConfig != nil && variant.GetIsDefaultMaxConfig()) ||
				(!variant.GetIsMaxMode() && variant.IsDefaultNonMaxConfig != nil && variant.GetIsDefaultNonMaxConfig()) {
				return variant
			}
		}
		if len(thinkingVariants) > 0 {
			return thinkingVariants[0]
		}
	}
	for _, variant := range preferredMode {
		if parameterValue(variant, "fast") == "true" {
			return variant
		}
	}
	for _, variant := range variants {
		if variant.GetIsMaxMode() && variant.IsDefaultMaxConfig != nil && variant.GetIsDefaultMaxConfig() {
			return variant
		}
	}
	if len(maxVariants) > 0 {
		return maxVariants[0]
	}
	for _, variant := range variants {
		if variant.IsDefaultNonMaxConfig != nil && variant.GetIsDefaultNonMaxConfig() {
			return variant
		}
	}
	if len(preferredMode) > 0 {
		return preferredMode[0]
	}
	return variants[0]
}

func booleanParamValues(def *aiserverv1.ModelParameterDefinition) []string {
	if def == nil || def.GetParameterType() == nil {
		return nil
	}
	boolDef := def.GetParameterType().GetBooleanParameter()
	if boolDef == nil {
		return nil
	}
	out := make([]string, 0, len(boolDef.GetValues()))
	for _, value := range boolDef.GetValues() {
		out = append(out, value.GetValue())
	}
	return out
}

func allowedParameterValues(model *aiserverv1.AvailableModelsResponse_AvailableModel) map[string]map[string]struct{} {
	out := map[string]map[string]struct{}{}
	for _, def := range model.GetParameterDefinitions() {
		id := strings.TrimSpace(def.GetId())
		if id == "" || def.GetParameterType() == nil {
			continue
		}
		values := map[string]struct{}{}
		if boolDef := def.GetParameterType().GetBooleanParameter(); boolDef != nil {
			for _, value := range boolDef.GetValues() {
				if v := strings.TrimSpace(value.GetValue()); v != "" {
					values[v] = struct{}{}
				}
			}
		}
		if enumDef := def.GetParameterType().GetEnumParameter(); enumDef != nil {
			for _, value := range enumDef.GetValues() {
				if v := strings.TrimSpace(value.GetValue()); v != "" {
					values[v] = struct{}{}
				}
			}
		}
		if len(values) > 0 {
			out[id] = values
		}
	}
	return out
}

func validVariantParameters(model *aiserverv1.AvailableModelsResponse_AvailableModel, variant *aiserverv1.AvailableModelsResponse_ModelVariantConfig) []ModelParameter {
	if variant == nil {
		return nil
	}
	allowed := allowedParameterValues(model)
	values := variant.GetParameterValues()
	if len(allowed) == 0 {
		if len(values) == 0 {
			return []ModelParameter{}
		}
		return nil
	}
	if len(values) != len(allowed) {
		return nil
	}
	out := make([]ModelParameter, 0, len(values))
	seen := map[string]struct{}{}
	for _, item := range values {
		id := strings.TrimSpace(item.GetId())
		value := strings.TrimSpace(item.GetValue())
		if id == "" || value == "" {
			return nil
		}
		allowedValues, ok := allowed[id]
		if !ok {
			return nil
		}
		if _, ok = allowedValues[value]; !ok {
			return nil
		}
		if _, dup := seen[id]; dup {
			return nil
		}
		seen[id] = struct{}{}
		out = append(out, ModelParameter{ID: id, Value: value})
	}
	if len(seen) != len(allowed) {
		return nil
	}
	return out
}

// CatalogToModelInfos converts Cursor catalog rows into CPA registry models.
func CatalogToModelInfos(models []CatalogModel) []*registry.ModelInfo {
	out := make([]*registry.ModelInfo, 0, len(models)+1)
	seen := map[string]struct{}{}
	// Keep the Agent "default" selector visible even when the catalog is rich.
	out = append(out, &registry.ModelInfo{
		ID:          "default",
		Object:      "model",
		Created:     time.Now().Unix(),
		OwnedBy:     "cursor",
		Type:        "cursor",
		DisplayName: "Cursor Default",
		Name:        "default",
		Description: "Cursor Agent default model selector.",
	})
	seen["default"] = struct{}{}
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[strings.ToLower(id)]; ok {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		info := &registry.ModelInfo{
			ID:          id,
			Object:      "model",
			Created:     time.Now().Unix(),
			OwnedBy:     "cursor",
			Type:        "cursor",
			DisplayName: model.DisplayName,
			Name:        id,
		}
		if model.ContextLimit > 0 {
			info.ContextLength = model.ContextLimit
		}
		if model.Thinking {
			info.SupportedParameters = append(info.SupportedParameters, "reasoning")
		}
		if model.SupportsImg {
			info.SupportedInputModalities = []string{"TEXT", "IMAGE"}
		} else {
			info.SupportedInputModalities = []string{"TEXT"}
		}
		out = append(out, info)
	}
	return out
}
