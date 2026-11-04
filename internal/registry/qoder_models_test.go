package registry

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestQoderRegionalCatalogIdentities(t *testing.T) {
	common := map[string]string{
		"auto": "Auto", "qmodel_38max": "Qwen3.8-Max", "qfmodel": "Qwen3.8-Flash",
		"qmodel_latest": "Qwen3.7-Max", "qmodel": "Qwen3.7-Plus", "dmodel": "DeepSeek-V4-Pro",
		"dfmodel": "DeepSeek-Flash", "gmodel": "GLM-5.3", "gfmodel": "GLM-5.3-Flash",
		"kmodel_latest": "Kimi-K3", "kmodel": "Kimi-K2.8-Preview",
	}
	for _, provider := range []string{"qoder", "qoder-cn"} {
		t.Run(provider, func(t *testing.T) {
			want := make(map[string]string)
			for key, name := range common {
				want[key] = name
			}
			if provider == "qoder-cn" {
				want["q37fmodel"], want["gm51model"], want["mmodel"] = "Qwen3.7-Flash", "GLM-5.2", "MiniMax-M2.7"
			} else {
				want["ultimate"], want["performance"], want["efficient"] = "Ultimate", "Performance", "Efficient"
				want["smodel"], want["cmodel"], want["mmodel"] = "Sonus", "Cantus", "MiniMax-M3"
			}
			models := GetQoderModels(provider)
			if len(models) != len(want) {
				t.Fatalf("got %d models, want %d", len(models), len(want))
			}
			seen := make(map[string]bool)
			for _, model := range models {
				name, exists := want[model.MetadataModelID]
				if !exists || seen[model.MetadataModelID] || model.ID != name || model.Name != name || model.DisplayName != name {
					t.Fatalf("unexpected identity: %+v", model)
				}
				seen[model.MetadataModelID] = true
				if model.Type != provider || model.OwnedBy != provider || model.Object != "model" {
					t.Fatalf("incorrect provider: %+v", model)
				}
				if model.Version != "" || model.OutputTokenLimit != 0 || model.MaxCompletionTokens != 0 {
					t.Fatalf("invented a version or output limit: %+v", model)
				}
				if !strings.Contains(model.Description, "snapshot") {
					t.Fatal("fallback was presented as live account entitlement")
				}
				restricted := provider == "qoder" && model.MetadataModelID != "qmodel_38max" && model.MetadataModelID != "qfmodel"
				if strings.Contains(model.Description, "enable=false") != restricted {
					t.Fatalf("lost restriction for %s", model.ID)
				}
			}
		})
	}
	if len(GetQoderModels("other")) != 0 {
		t.Fatal("unknown provider received a regional catalog")
	}
}

func TestQoderSnapshotCapabilitiesAndCloneIsolation(t *testing.T) {
	for _, test := range []struct {
		provider, key              string
		input, context             int
		vision, reasoning, disable bool
		levels                     []string
	}{
		{"qoder-cn", "dmodel", 96000, 96000, true, true, true, []string{"none", "high", "max"}},
		{"qoder", "dmodel", 1000000, 1000000, true, true, true, []string{"none", "high", "max"}},
		{"qoder-cn", "qmodel_38max", 180000, 180000, true, true, true, []string{"none", "low", "medium", "xhigh"}},
		{"qoder", "qmodel_latest", 1000000, 1000000, true, true, true, nil},
		{"qoder", "kmodel", 0, 200000, true, true, false, []string{"low", "high", "max"}},
		{"qoder", "cmodel", 180000, 180000, true, true, false, []string{"low", "medium", "high", "xhigh", "max"}},
		{"qoder", "mmodel", 180000, 180000, true, false, false, nil},
		{"qoder-cn", "mmodel", 180000, 180000, false, false, false, nil},
		{"qoder-cn", "auto", 200000, 200000, true, true, false, nil},
		{"qoder", "auto", 200000, 200000, true, false, false, nil},
	} {
		t.Run(test.provider+"/"+test.key, func(t *testing.T) {
			var model *ModelInfo
			for _, candidate := range GetQoderModels(test.provider) {
				if candidate.MetadataModelID == test.key {
					model = candidate
					break
				}
			}
			if model == nil {
				t.Fatal("missing model")
			}
			if model.InputTokenLimit != test.input || model.ContextLength != test.context || (len(model.SupportedInputModalities) == 2) != test.vision {
				t.Fatalf("incorrect limits or vision: %+v", model)
			}
			if (model.Thinking != nil) != test.reasoning {
				t.Fatal("reasoning capability lost")
			}
			if model.Thinking != nil && (model.Thinking.ZeroAllowed != test.disable || !reflect.DeepEqual(model.Thinking.Levels, test.levels) || model.Thinking.Min != 0 || model.Thinking.Max != 0) {
				t.Fatalf("incorrect reasoning capability: %+v", model.Thinking)
			}
			if test.reasoning && model.Thinking.DynamicAllowed {
				t.Fatal("inferred an unsupported dynamic thinking budget")
			}
		})
	}
	before := GetQoderModels("qoder")
	mutated := GetQoderModels("qoder")
	for _, model := range mutated {
		model.ID = "mutated"
		model.SupportedInputModalities[0] = "audio"
		model.SupportedOutputModalities[0] = "image"
		model.SupportedParameters[0] = "mutated"
		if model.Thinking != nil {
			model.Thinking.ZeroAllowed = !model.Thinking.ZeroAllowed
			if len(model.Thinking.Levels) > 0 {
				model.Thinking.Levels[0] = "mutated"
			}
		}
	}
	if !reflect.DeepEqual(before, GetQoderModels("qoder")) {
		t.Fatal("caller changed the fallback catalog")
	}
}

func TestQoderDynamicCatalogMetadataAndCapabilities(t *testing.T) {
	const provider = "qoder-cn"
	id := t.Name()
	t.Cleanup(func() { DeleteQoderModelsForClient(id, provider) })
	var entries []map[string]any
	if err := json.Unmarshal([]byte(`[
		{"key":"future","display_name":"Future-Model","enable":false,"is_vl":true,"disabledMultimodal":true,"supportsToolCall":false,"max_input_tokens":65536,"max_output_tokens":4096,"price_factor":0.2,"promotion":{"discount_factor":0.4},"aliases":["future-alias"],"context_config":{"64K":{"token_count":65536,"is_default":true},"1M":{"token_count":1000000}},"thinking_config":{"enabled":{"efforts":{"max":{"is_default":true},"low":{}}}}},
		{"key":"qmodel_38max","enable":false,"is_vl":false,"thinking_config":null,"context_config":null},
		{"key":"lite"},{"key":"codewise-hidden"},{"key":"completion-hidden"},{"key":"x-image-alpha"},{"key":"x-image-alpha-edit"},{"key":"x-taco-completion"}
	]`), &entries); err != nil {
		t.Fatal(err)
	}
	models, err := SetQoderModelsForClient(id, provider, entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "Qwen3.8-Max" || models[1].ID != "Future-Model" {
		t.Fatalf("unexpected membership/order: %+v", models)
	}
	qwen, future := models[0], models[1]
	if qwen.ContextLength != 180000 || len(qwen.SupportedInputModalities) != 1 || !reflect.DeepEqual(qwen.Thinking.Levels, []string{"none", "low", "medium", "xhigh"}) {
		t.Fatal("null metadata removed fallback capabilities or false failed to override")
	}
	if future.ContextLength != 65536 || future.InputTokenLimit != 65536 || future.OutputTokenLimit != 4096 || future.MaxCompletionTokens != 4096 || len(future.SupportedInputModalities) != 1 || future.MaxContextLength != 0 {
		t.Fatalf("incorrect dynamic capabilities: %+v", future)
	}
	if !reflect.DeepEqual(future.Thinking.Levels, []string{"low", "max"}) || future.Thinking.ZeroAllowed || !reflect.DeepEqual(future.SupportedParameters, []string{"reasoning_effort"}) {
		t.Fatalf("incorrect dynamic reasoning/tools: %+v", future)
	}
	if !strings.Contains(future.Description, "enable=false") {
		t.Fatal("restricted model was not labelled")
	}
	beforeModels, beforeMetadata, ok := GetQoderCatalogForClient(id, provider)
	if !ok || beforeMetadata[1]["key"] != "future" || beforeMetadata[1]["enable"] != false || beforeMetadata[1]["price_factor"] != 0.2 {
		t.Fatalf("upstream key, enable or multiplier lost: %+v", beforeMetadata)
	}
	entries[0]["promotion"].(map[string]any)["discount_factor"] = 999
	models[1].ID = "mutated"
	models[0].Thinking.Levels[0] = "mutated"
	copyModels, copyMetadata, _ := GetQoderCatalogForClient(id, provider)
	copyModels[0].SupportedInputModalities[0] = "audio"
	copyMetadata[1]["aliases"].([]any)[0] = "mutated"
	copyMetadata[1]["promotion"].(map[string]any)["discount_factor"] = 999
	afterModels, afterMetadata, _ := GetQoderCatalogForClient(id, provider)
	if !reflect.DeepEqual(beforeModels, afterModels) || !reflect.DeepEqual(beforeMetadata, afterMetadata) {
		t.Fatal("catalog snapshots share mutable state")
	}
}

func TestQoderDynamicReplacementIsolationAndEmpty(t *testing.T) {
	id := t.Name()
	for _, client := range []string{id, id + "-other"} {
		for _, provider := range []string{"qoder", "qoder-cn"} {
			t.Cleanup(func() { DeleteQoderModelsForClient(client, provider) })
			if _, err := SetQoderModelsForClient(client, provider, []map[string]any{{"key": "mmodel"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	cn, _, _ := GetQoderCatalogForClient(id, "qoder-cn")
	intl, _, _ := GetQoderCatalogForClient(id, "qoder")
	if cn[0].ID != "MiniMax-M2.7" || intl[0].ID != "MiniMax-M3" {
		t.Fatal("regional metadata leaked")
	}
	for _, invalid := range [][]map[string]any{
		{{"display_name": "missing-key"}},
		{{"key": "duplicate"}, {"key": "duplicate"}},
		{{"key": "first", "display_name": "duplicate"}, {"key": "second", "display_name": "duplicate"}},
		{{"key": "invalid", "extra": make(chan int)}},
	} {
		if _, err := SetQoderModelsForClient(id, "qoder-cn", invalid); err == nil {
			t.Fatal("accepted invalid catalog")
		}
		got, _, _ := GetQoderCatalogForClient(id, "qoder-cn")
		if !reflect.DeepEqual(cn, got) {
			t.Fatal("invalid catalog replaced the last successful one")
		}
	}
	empty, err := SetQoderModelsForClient(id, "qoder-cn", []map[string]any{})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("successful empty result: %v, %v", empty, err)
	}
	got, metadata, ok := GetQoderCatalogForClient(id, "qoder-cn")
	if !ok || got == nil || metadata == nil || len(got) != 0 || len(metadata) != 0 {
		t.Fatal("empty success became a cache miss")
	}
	other, _, _ := GetQoderCatalogForClient(id+"-other", "qoder-cn")
	stillIntl, _, _ := GetQoderCatalogForClient(id, "qoder")
	if !reflect.DeepEqual(cn, other) || !reflect.DeepEqual(intl, stillIntl) {
		t.Fatal("replacement affected another account/provider")
	}
	DeleteQoderModelsForClient(id, "qoder-cn")
	if _, _, exists := GetQoderCatalogForClient(id, "qoder-cn"); exists {
		t.Fatal("account cleanup retained the catalog")
	}
}
