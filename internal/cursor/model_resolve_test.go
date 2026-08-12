package cursor

import (
	"strings"
	"testing"
)

func TestResolveRequestedModelUsesCatalogParams(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID:         "claude-4.6-sonnet",
		WireID:     "cursor-claude-4.6-sonnet-thinking",
		Parameters: []ModelParameter{{ID: "thinking", Value: "true"}, {ID: "fast", Value: "true"}},
	}})
	sel := ResolveRequestedModel("claude-4.6-sonnet")
	if sel.PublicID != "claude-4.6-sonnet" {
		t.Fatalf("public = %q", sel.PublicID)
	}
	if sel.ModelID != "cursor-claude-4.6-sonnet-thinking" || !sel.VariantStringRepr {
		t.Fatalf("expected wire id, got %#v", sel)
	}
}

func TestResolveRequestedModelUsesMatchingWireID(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID:         "grok-4.5",
		WireID:     "cursor-grok-4.5-high-fast",
		Parameters: []ModelParameter{{ID: "effort", Value: "high"}, {ID: "fast", Value: "true"}},
	}})
	sel := ResolveRequestedModel("grok-4.5")
	if sel.ModelID != "cursor-grok-4.5-high-fast" || !sel.VariantStringRepr {
		t.Fatalf("expected matching wire variant id, got %#v", sel)
	}
}

func TestResolveRequestedModelDefaultSelector(t *testing.T) {
	sel := ResolveRequestedModel("default")
	if sel.ModelID != "default" || len(sel.Parameters) != 0 {
		t.Fatalf("default selector should have no parameters, got %#v", sel)
	}
}

func TestResolveRequestedModelFastSuffix(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID:     "claude-fable-5",
		WireID: "cursor-claude-fable-5-thinking-medium",
		Parameters: []ModelParameter{
			{ID: "thinking", Value: "true"},
			{ID: "effort", Value: "medium"},
			{ID: "fast", Value: "false"},
		},
		Variants: []CatalogVariant{
			{
				Parameters: []ModelParameter{
					{ID: "thinking", Value: "true"},
					{ID: "effort", Value: "medium"},
					{ID: "fast", Value: "false"},
				},
			},
			{
				Parameters: []ModelParameter{
					{ID: "thinking", Value: "true"},
					{ID: "effort", Value: "medium"},
					{ID: "fast", Value: "true"},
				},
			},
		},
	}})
	rememberUsableModels([]usableModel{
		{ID: "cursor-claude-fable-5-thinking-medium"},
		{ID: "cursor-claude-fable-5-thinking-medium-fast"},
	})
	sel := ResolveRequestedModel("claude-fable-5-fast")
	if !strings.Contains(sel.ModelID, "fast") || !sel.VariantStringRepr {
		t.Fatalf("expected fast wire id, got %#v", sel)
	}
}

func TestResolveRequestedModelSuffix(t *testing.T) {
	RememberCatalog([]CatalogModel{{ID: "claude-4.6-sonnet"}})
	rememberUsableModels(nil)
	sel := ResolveRequestedModel("claude-4.6-sonnet-thinking-xhigh")
	if sel.PublicID != "claude-4.6-sonnet" {
		t.Fatalf("public = %q", sel.PublicID)
	}
	if sel.VariantStringRepr {
		t.Fatalf("no wire available; expected parameter form, got %#v", sel)
	}
	got := map[string]string{}
	for _, p := range sel.Parameters {
		got[p.ID] = p.Value
	}
	if got["thinking"] != "true" || got["reasoning"] != "xhigh" || got["effort"] != "xhigh" {
		t.Fatalf("unexpected parameters: %#v", sel.Parameters)
	}
}

func TestPreferCatalogVariantDefaultsToFast(t *testing.T) {
	variants := []CatalogVariant{
		{Parameters: []ModelParameter{
			{ID: "thinking", Value: "true"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "false"},
		}, IsDefaultNonMax: true},
		{Parameters: []ModelParameter{
			{ID: "thinking", Value: "true"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "true"},
		}},
	}
	got := preferCatalogVariant(variants, false, nil)
	if got == nil || paramValue(got.Parameters, "fast") != "true" {
		t.Fatalf("expected fast=true preference, got %#v", got)
	}
}

func TestResolveRequestedModelAutoFastBareID(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID: "claude-fable-5",
		Parameters: []ModelParameter{
			{ID: "thinking", Value: "true"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "false"},
		},
		Variants: []CatalogVariant{
			{Parameters: []ModelParameter{
				{ID: "thinking", Value: "true"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "false"},
			}},
			{Parameters: []ModelParameter{
				{ID: "thinking", Value: "true"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "true"},
			}},
		},
	}})
	rememberUsableModels([]usableModel{
		{ID: "cursor-claude-fable-5-thinking-medium"},
		{ID: "cursor-claude-fable-5-thinking-medium-fast"},
	})
	sel := ResolveRequestedModel("claude-fable-5")
	if !strings.Contains(sel.ModelID, "fast") || !sel.VariantStringRepr {
		t.Fatalf("bare id should auto-pick fast wire, got %#v", sel)
	}

	std := ResolveRequestedModel("claude-fable-5-standard")
	if strings.Contains(std.ModelID, "fast") {
		t.Fatalf("standard should opt out of fast, got %#v", std)
	}
}

func TestResolveRequestedModelHaikuUsesFamilyWire(t *testing.T) {
	RememberCatalog([]CatalogModel{{ID: "claude-haiku-4-5"}})
	rememberUsableModels([]usableModel{
		{ID: "cursor-claude-4.5-haiku"},
		{ID: "cursor-claude-sonnet-4-5"},
	})
	sel := ResolveRequestedModel("claude-haiku-4-5")
	if !strings.Contains(strings.ToLower(sel.ModelID), "haiku") || !sel.VariantStringRepr {
		t.Fatalf("expected haiku family wire, got %#v", sel)
	}
}

func TestResolveRequestedModelHaikuFallsBackWithoutFamilyWire(t *testing.T) {
	RememberCatalog([]CatalogModel{{ID: "claude-haiku-4-5"}})
	rememberUsableModels([]usableModel{
		{ID: "cursor-claude-sonnet-4-5-thinking-medium-fast"},
		{ID: "cursor-composer-2.5-fast"},
	})
	sel := ResolveRequestedModel("claude-haiku-4-5")
	if strings.EqualFold(sel.ModelID, "claude-haiku-4-5") || !sel.VariantStringRepr {
		t.Fatalf("expected remap away from bare haiku, got %#v", sel)
	}
	if !strings.Contains(strings.ToLower(sel.ModelID), "sonnet") && !strings.Contains(strings.ToLower(sel.ModelID), "composer") {
		t.Fatalf("expected sonnet/composer fallback, got %#v", sel)
	}
}

func TestResolveRequestedModelKeepsBareUsablePublicID(t *testing.T) {
	RememberCatalog([]CatalogModel{{ID: "gemini-3-flash", WireID: "gemini-3-flash"}})
	rememberUsableModels([]usableModel{
		{ID: "gemini-3-flash"},
		{ID: "gemini-3.6-flash-minimal"},
	})
	sel := ResolveRequestedModel("gemini-3-flash")
	if !strings.EqualFold(sel.ModelID, "gemini-3-flash") {
		t.Fatalf("usable bare public id should be kept, got %#v", sel)
	}
}

func TestAlternateUsableModelID(t *testing.T) {
	rememberUsableModels([]usableModel{
		{ID: "cursor-claude-sonnet-4-5"},
		{ID: "cursor-composer-2.5-fast"},
	})
	alt := AlternateUsableModelID("cursor-claude-sonnet-4-5")
	if alt == "" || strings.EqualFold(alt, "cursor-claude-sonnet-4-5") {
		t.Fatalf("expected a different usable wire, got %q", alt)
	}
}

func TestBuildRunRequestSendsParameters(t *testing.T) {
	RememberCatalog([]CatalogModel{{ID: "gpt-5.4"}})
	rememberUsableModels(nil)
	msg, _, _, err := buildRunRequest("gpt-5.4-thinking", []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	req := run.GetRequestedModel()
	if req.GetModelId() != "gpt-5.4" {
		t.Fatalf("requested model id = %q", req.GetModelId())
	}
	if req.GetIsVariantStringRepresentation() {
		t.Fatal("must not use variant string representation without wire id")
	}
	if len(req.GetParameters()) == 0 || req.GetParameters()[0].GetId() != "thinking" {
		t.Fatalf("expected thinking parameter, got %#v", req.GetParameters())
	}
}

func TestBaseModelID(t *testing.T) {
	if got := BaseModelID("grok-4.5-high-fast"); got != "grok-4.5" {
		t.Fatalf("got %q", got)
	}
}
