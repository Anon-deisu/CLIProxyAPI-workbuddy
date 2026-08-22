package cursor

import (
	"strings"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
)

// ModelParameter is one Cursor RequestedModel.parameters entry.
type ModelParameter struct {
	ID    string
	Value string
}

// ModelSelection is the wire model id + catalog-style parameters for AgentRun.
// Matches Cursor 3.17: base model_id + exact advertised parameters
// (including fast=true). Variant-string wire IDs are not used by default.
type ModelSelection struct {
	ModelID           string
	PublicID          string
	MaxMode           bool
	Parameters        []ModelParameter
	VariantStringRepr bool
}

var cursorSuffixTokens = map[string]struct{}{
	"thinking": {},
	"fast":     {},
	"standard": {}, // opt out of auto-fast
	"low":      {},
	"medium":   {},
	"high":     {},
	"xhigh":    {},
	"max":      {},
}

var effortAliases = map[string]map[string]struct{}{
	"low":    {"low": {}},
	"medium": {"medium": {}},
	"high":   {"high": {}},
	"xhigh":  {"xhigh": {}, "extra-high": {}},
	"max":    {"max": {}},
}

// BaseModelID strips Cursor hyphen variant suffixes (-fast, -thinking, …).
func BaseModelID(requested string) string {
	base, _ := splitCursorModelSuffix(strings.TrimSpace(requested))
	if base == "" {
		return strings.TrimSpace(requested)
	}
	return base
}

// ResolveRequestedModel maps a public/legacy Cursor model slug onto the base
// model id plus RequestedModel.parameters, matching cursor2api:
//
//   - Public / wire IDs stay base names (no -thinking / -fast in model_id)
//   - Bare base id uses the catalog default variant (prefers thinking+fast)
//   - Hyphen suffixes resolve against advertised variants only
func ResolveRequestedModel(requested string) ModelSelection {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = "default"
	}
	base, tokens := splitCursorModelSuffix(requested)
	sel := ModelSelection{ModelID: base, PublicID: base}

	entry, ok := catalogEntry(base)
	if !ok {
		// Cursor's built-in Auto/default selector explicitly supports Max Mode.
		// Keep maximum context enabled even before an account catalog is warm.
		if strings.EqualFold(base, "default") && len(tokens) == 0 {
			sel.MaxMode = true
		}
		// Invented params as last-resort when catalog is cold.
		if len(tokens) > 0 {
			sel.Parameters = parametersFromTokens(tokens)
			for _, t := range tokens {
				if t == "max" {
					sel.MaxMode = true
				}
			}
		}
		// Still try usable-wire / fallback — bare public ids are often rejected
		// with ERROR_BAD_MODEL_NAME even when listed in AvailableModels.
		return ensureAgentAcceptedModelID(sel)
	}

	wantStandard := false
	for _, t := range tokens {
		if t == "standard" {
			wantStandard = true
			break
		}
	}

	if len(tokens) == 0 || (len(tokens) == 1 && wantStandard) {
		sel.MaxMode = entry.MaxMode
		if len(entry.Parameters) > 0 {
			sel.Parameters = append([]ModelParameter(nil), entry.Parameters...)
		}
		if !wantStandard {
			sel = applyDefaultFast(sel, entry)
		} else {
			sel = applyExplicitStandard(sel, entry)
		}
		return ensureAgentAcceptedModelID(preferUsableWireID(sel, entry))
	}

	if variant := matchCatalogVariant(entry, requested, tokens); variant != nil {
		sel.Parameters = append([]ModelParameter(nil), variant.Parameters...)
		sel.MaxMode = variant.MaxMode
		return ensureAgentAcceptedModelID(preferUsableWireID(sel, entry))
	}

	// No advertised match — best-effort token params (cursor2api would refuse).
	sel.Parameters = parametersFromTokens(tokens)
	for _, t := range tokens {
		if t == "max" {
			sel.MaxMode = true
		}
	}
	return ensureAgentAcceptedModelID(preferUsableWireID(sel, entry))
}

// applyDefaultFast turns on Cursor fast for bare model ids when the account
// catalog / usable list advertises a fast variant. Models without fast stay unchanged.
func applyDefaultFast(sel ModelSelection, entry CatalogModel) ModelSelection {
	if !catalogSupportsFast(entry) {
		return sel
	}
	if paramValue(sel.Parameters, "fast") == "true" {
		return sel
	}
	desire := true
	if v := preferCatalogVariantForMode(entry.Variants, true, &desire, &sel.MaxMode); v != nil && paramValue(v.Parameters, "fast") == "true" {
		sel.Parameters = append([]ModelParameter(nil), v.Parameters...)
		sel.MaxMode = v.MaxMode
		return sel
	}
	if len(entry.Variants) > 0 {
		return sel
	}
	sel.Parameters = upsertModelParameter(sel.Parameters, "fast", "true")
	return sel
}

func applyExplicitStandard(sel ModelSelection, entry CatalogModel) ModelSelection {
	desire := false
	if v := preferCatalogVariantForMode(entry.Variants, true, &desire, &sel.MaxMode); v != nil && paramValue(v.Parameters, "fast") == "false" {
		sel.Parameters = append([]ModelParameter(nil), v.Parameters...)
		sel.MaxMode = v.MaxMode
		return sel
	}
	if len(entry.Variants) > 0 {
		return sel
	}
	sel.Parameters = upsertModelParameter(sel.Parameters, "fast", "false")
	return sel
}

func catalogSupportsFast(entry CatalogModel) bool {
	if paramValue(entry.Parameters, "fast") == "true" {
		return true
	}
	for _, v := range entry.Variants {
		if paramValue(v.Parameters, "fast") == "true" {
			return true
		}
	}
	publicFold := strings.ToLower(strings.TrimSpace(entry.ID))
	for _, item := range cachedUsableModels() {
		idFold := strings.ToLower(strings.TrimSpace(item.ID))
		if idFold == "" || !strings.Contains(idFold, "-fast") {
			continue
		}
		if strings.Contains(idFold, publicFold) || strings.HasPrefix(idFold, "cursor-"+publicFold) {
			return true
		}
	}
	return false
}

func upsertModelParameter(params []ModelParameter, id, value string) []ModelParameter {
	id = strings.TrimSpace(id)
	value = strings.TrimSpace(value)
	if id == "" || value == "" {
		return params
	}
	out := make([]ModelParameter, 0, len(params)+1)
	found := false
	for _, p := range params {
		if strings.EqualFold(p.ID, id) {
			out = append(out, ModelParameter{ID: id, Value: value})
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		out = append(out, ModelParameter{ID: id, Value: value})
	}
	return out
}

// preferUsableWireID always prefers GetUsableModels / catalog WireID over the
// public AvailableModels name. This account's Agent rejects bare public ids
// with ERROR_BAD_MODEL_NAME (e.g. "claude-fable-5").
func preferUsableWireID(sel ModelSelection, entry CatalogModel) ModelSelection {
	tmp := entry
	tmp.Parameters = sel.Parameters
	wantFast := paramValue(sel.Parameters, "fast") == "true"
	usable := cachedUsableModels()

	if wire := matchUsableWireID(tmp, usable); wire != "" && !strings.EqualFold(wire, sel.PublicID) {
		if wantFast && !strings.Contains(strings.ToLower(wire), "-fast") {
			if fastWire := anyUsableFastWireID(sel.PublicID, usable); fastWire != "" {
				wire = fastWire
			}
		}
		sel.ModelID = wire
		sel.VariantStringRepr = true
		sel.Parameters = nil
		return sel
	}
	if wantFast {
		if wire := anyUsableFastWireID(sel.PublicID, usable); wire != "" {
			sel.ModelID = wire
			sel.VariantStringRepr = true
			sel.Parameters = nil
			return sel
		}
	}
	if wire := strings.TrimSpace(entry.WireID); wire != "" && !strings.EqualFold(wire, sel.PublicID) {
		sel.ModelID = wire
		sel.VariantStringRepr = true
		sel.Parameters = nil
		return sel
	}
	if wire := anyUsableWireID(sel.PublicID, usable); wire != "" {
		sel.ModelID = wire
		sel.VariantStringRepr = true
		sel.Parameters = nil
	}
	return sel
}

// AlternateUsableModelID returns a different GetUsableModels wire id for the
// same public family when the current wire is failing with transient provider
// errors. Empty string means no alternate is available.
func AlternateUsableModelID(current string) string {
	current = strings.TrimSpace(current)
	if current == "" {
		return ""
	}
	usable := cachedUsableModels()
	if len(usable) == 0 {
		return ""
	}
	base := strings.ToLower(BaseModelID(current))
	// Prefer another usable id that shares the base/public family.
	for _, item := range usable {
		id := strings.TrimSpace(item.ID)
		if id == "" || strings.EqualFold(id, current) || strings.EqualFold(id, "default") {
			continue
		}
		idFold := strings.ToLower(id)
		if base != "" && (strings.Contains(idFold, base) || strings.Contains(base, idFold)) {
			return id
		}
		for _, alias := range item.Aliases {
			aliasFold := strings.ToLower(strings.TrimSpace(alias))
			if aliasFold != "" && base != "" && (strings.Contains(aliasFold, base) || strings.Contains(base, aliasFold)) {
				return id
			}
		}
	}
	if alt := fallbackUsableWireID(BaseModelID(current), usable); alt != "" && !strings.EqualFold(alt, current) {
		return alt
	}
	for _, item := range usable {
		id := strings.TrimSpace(item.ID)
		if id != "" && !strings.EqualFold(id, current) && !strings.EqualFold(id, "default") {
			return id
		}
	}
	return ""
}

// ensureAgentAcceptedModelID remaps models that are missing from GetUsableModels.
// Claude Code often hardcodes claude-haiku-4-5 for side tasks; this account has
// no haiku usable wire, so Agent returns ERROR_BAD_MODEL_NAME and tool loops die.
// Models that appear in usable (even as the bare public id) are left alone.
func ensureAgentAcceptedModelID(sel ModelSelection) ModelSelection {
	if sel.VariantStringRepr && !strings.EqualFold(sel.ModelID, sel.PublicID) {
		return sel
	}
	usable := cachedUsableModels()
	if len(usable) == 0 {
		return sel
	}
	if usableContainsID(usable, sel.PublicID) || usableContainsID(usable, sel.ModelID) {
		return sel
	}
	if entry, ok := catalogEntry(sel.PublicID); ok {
		if wire := strings.TrimSpace(entry.WireID); wire != "" && usableContainsID(usable, wire) {
			if !strings.EqualFold(wire, sel.ModelID) {
				sel.ModelID = wire
				sel.VariantStringRepr = true
				sel.Parameters = nil
			}
			return sel
		}
	}
	if wire := fuzzyUsableWireID(sel.PublicID, usable); wire != "" {
		log.Infof("cursor: remap model %q -> usable wire %q", sel.PublicID, wire)
		sel.ModelID = wire
		sel.VariantStringRepr = true
		sel.Parameters = nil
		return sel
	}
	if wire := fallbackUsableWireID(sel.PublicID, usable); wire != "" {
		log.Warnf("cursor: no usable wire for %q; falling back to %q", sel.PublicID, wire)
		sel.ModelID = wire
		sel.VariantStringRepr = true
		sel.Parameters = nil
		return sel
	}
	return sel
}

func usableContainsID(usable []usableModel, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, item := range usable {
		if strings.EqualFold(strings.TrimSpace(item.ID), id) {
			return true
		}
		for _, alias := range item.Aliases {
			if strings.EqualFold(strings.TrimSpace(alias), id) {
				return true
			}
		}
	}
	return false
}

func fuzzyUsableWireID(publicID string, usable []usableModel) string {
	publicFold := strings.ToLower(strings.TrimSpace(publicID))
	if publicFold == "" {
		return ""
	}
	// Significant tokens from public ids like claude-haiku-4-5 / claude-4.5-haiku.
	tokens := strings.FieldsFunc(publicFold, func(r rune) bool {
		return r == '-' || r == '.' || r == '_'
	})
	interesting := make([]string, 0, len(tokens))
	for _, t := range tokens {
		switch t {
		case "", "claude", "cursor", "gpt", "google", "gemini":
			continue
		}
		// Skip bare version digits ("4","5") — they match every Claude 4.5 model.
		if isNumericToken(t) {
			continue
		}
		interesting = append(interesting, t)
	}
	families := []string{"haiku", "fable", "sonnet", "opus", "composer", "grok"}
	var family string
	for _, f := range families {
		if strings.Contains(publicFold, f) {
			family = f
			break
		}
	}
	if family != "" {
		found := false
		for _, t := range interesting {
			if t == family {
				found = true
				break
			}
		}
		if !found {
			interesting = append([]string{family}, interesting...)
		}
	}
	if len(interesting) == 0 {
		return ""
	}
	var best string
	bestScore := 0
	for _, item := range usable {
		id := strings.TrimSpace(item.ID)
		idFold := strings.ToLower(id)
		if idFold == "" || idFold == publicFold {
			continue
		}
		score := 0
		matched := 0
		for _, t := range interesting {
			if strings.Contains(idFold, t) {
				matched++
				score += 10
			}
		}
		if matched < len(interesting) {
			continue
		}
		if strings.HasPrefix(idFold, "cursor-") {
			score += 20
		}
		if strings.Contains(idFold, "-fast") {
			score += 5
		}
		for _, alias := range item.Aliases {
			if strings.EqualFold(strings.TrimSpace(alias), publicID) {
				score += 40
			}
		}
		if score > bestScore {
			bestScore = score
			best = id
		}
	}
	return best
}

func isNumericToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func fallbackUsableWireID(publicID string, usable []usableModel) string {
	publicFold := strings.ToLower(strings.TrimSpace(publicID))
	// Claude Code side tasks (haiku) should land on a cheap fast wire.
	prefs := []string{"sonnet", "fable", "composer", "grok", "gpt", "flash"}
	if strings.Contains(publicFold, "haiku") || strings.Contains(publicFold, "mini") || strings.Contains(publicFold, "nano") {
		prefs = []string{"composer-2.5-fast", "nano", "mini", "flash", "composer", "sonnet", "gpt"}
	}
	for _, pref := range prefs {
		for _, item := range usable {
			id := strings.TrimSpace(item.ID)
			idFold := strings.ToLower(id)
			if idFold == "" || !strings.Contains(idFold, pref) {
				continue
			}
			if strings.EqualFold(idFold, "default") {
				continue
			}
			return id
		}
	}
	for _, item := range usable {
		id := strings.TrimSpace(item.ID)
		if id != "" && !strings.EqualFold(id, "default") {
			return id
		}
	}
	return ""
}

func anyUsableFastWireID(publicID string, usable []usableModel) string {
	publicFold := strings.ToLower(strings.TrimSpace(publicID))
	if publicFold == "" {
		return ""
	}
	var best string
	bestScore := -1
	for _, item := range usable {
		id := strings.TrimSpace(item.ID)
		idFold := strings.ToLower(id)
		if idFold == "" || idFold == publicFold || !strings.Contains(idFold, "-fast") {
			continue
		}
		score := 0
		switch {
		case strings.HasPrefix(idFold, "cursor-"+publicFold+"-"):
			score = 100
		case strings.HasPrefix(idFold, publicFold+"-"):
			score = 90
		case strings.Contains(idFold, publicFold):
			score = 50
		default:
			continue
		}
		if score > bestScore {
			bestScore = score
			best = id
		}
	}
	return best
}

func anyUsableWireID(publicID string, usable []usableModel) string {
	publicFold := strings.ToLower(strings.TrimSpace(publicID))
	if publicFold == "" {
		return ""
	}
	// Prefer fast wires first when both exist (auto-fast default).
	if wire := anyUsableFastWireID(publicID, usable); wire != "" {
		return wire
	}
	for _, item := range usable {
		id := strings.TrimSpace(item.ID)
		idFold := strings.ToLower(id)
		if idFold == "" || idFold == publicFold {
			continue
		}
		if strings.HasPrefix(idFold, "cursor-"+publicFold+"-") || strings.HasPrefix(idFold, publicFold+"-") {
			return id
		}
		for _, alias := range item.Aliases {
			if strings.EqualFold(strings.TrimSpace(alias), publicID) && idFold != publicFold {
				return id
			}
		}
	}
	return ""
}

func parametersFromTokens(tokens []string) []ModelParameter {
	seen := map[string]string{}
	out := make([]ModelParameter, 0, len(tokens))
	set := func(id, value string) {
		if id == "" || value == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = value
		out = append(out, ModelParameter{ID: id, Value: value})
	}
	for _, token := range tokens {
		switch token {
		case "thinking":
			set("thinking", "true")
		case "fast":
			set("fast", "true")
		case "low", "medium", "high", "xhigh":
			set("reasoning", token)
			set("effort", token)
		}
	}
	return out
}

func matchCatalogVariant(entry CatalogModel, requested string, tokens []string) *CatalogVariant {
	if len(entry.Variants) == 0 {
		return nil
	}
	reqFold := strings.ToLower(strings.TrimSpace(requested))
	tokenSet := map[string]struct{}{}
	for _, t := range tokens {
		tokenSet[t] = struct{}{}
	}

	for i := range entry.Variants {
		v := &entry.Variants[i]
		if slug := strings.ToLower(strings.TrimSpace(v.LegacySlug)); slug != "" && slug == reqFold {
			return v
		}
		if slug := strings.ToLower(strings.TrimSpace(v.VariantStringRepr)); slug != "" && slug == reqFold {
			return v
		}
	}

	valuesOf := func(v *CatalogVariant) map[string]string {
		out := make(map[string]string, len(v.Parameters))
		for _, p := range v.Parameters {
			out[strings.ToLower(p.ID)] = strings.ToLower(p.Value)
		}
		return out
	}

	for i := range entry.Variants {
		v := &entry.Variants[i]
		values := valuesOf(v)
		if _, want := tokenSet["thinking"]; want && values["thinking"] != "true" {
			continue
		}
		if _, want := tokenSet["fast"]; want && values["fast"] != "true" {
			continue
		}
		effortOK := true
		for token, aliases := range effortAliases {
			if _, want := tokenSet[token]; !want {
				continue
			}
			actual := values["effort"]
			if actual == "" {
				actual = values["reasoning"]
			}
			if _, ok := aliases[actual]; !ok {
				effortOK = false
				break
			}
		}
		if !effortOK {
			continue
		}
		// At least one requested token must positively match (cursor2api).
		matched := false
		if _, want := tokenSet["thinking"]; want && values["thinking"] == "true" {
			matched = true
		}
		if _, want := tokenSet["fast"]; want && values["fast"] == "true" {
			matched = true
		}
		for token := range effortAliases {
			if _, want := tokenSet[token]; !want {
				continue
			}
			actual := values["effort"]
			if actual == "" {
				actual = values["reasoning"]
			}
			if _, ok := effortAliases[token][actual]; ok {
				matched = true
			}
		}
		if matched {
			return v
		}
	}
	return nil
}

func splitCursorModelSuffix(requested string) (base string, tokens []string) {
	parts := strings.Split(requested, "-")
	if len(parts) <= 1 {
		return requested, nil
	}
	cut := len(parts)
	for cut > 1 {
		token := strings.ToLower(parts[cut-1])
		if _, ok := cursorSuffixTokens[token]; !ok {
			break
		}
		cut--
	}
	if cut == len(parts) {
		return requested, nil
	}
	base = strings.Join(parts[:cut], "-")
	if strings.TrimSpace(base) == "" {
		return requested, nil
	}
	rawTokens := parts[cut:]
	tokens = make([]string, 0, len(rawTokens))
	for _, part := range rawTokens {
		tokens = append(tokens, strings.ToLower(part))
	}
	return base, tokens
}

func toRequestedModelProto(sel ModelSelection) *agentv1.RequestedModel {
	out := &agentv1.RequestedModel{
		ModelId:                       sel.ModelID,
		MaxMode:                       sel.MaxMode,
		IsVariantStringRepresentation: sel.VariantStringRepr,
	}
	if sel.VariantStringRepr {
		return out
	}
	for _, p := range sel.Parameters {
		id := strings.TrimSpace(p.ID)
		value := strings.TrimSpace(p.Value)
		if id == "" || value == "" {
			continue
		}
		out.Parameters = append(out.Parameters, &agentv1.RequestedModel_ModelParameterValue{
			Id:    id,
			Value: value,
		})
	}
	return out
}
