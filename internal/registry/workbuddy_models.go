package registry

// GetWorkBuddyModels supplies a regional fallback when the account catalog is unavailable.
// The authenticated /v3/config catalog takes precedence during registration.
func GetWorkBuddyModels(provider string) []*ModelInfo {
	type definition struct {
		id, name string
		context  int
		levels   []string
	}
	definitions := []definition{
		{"auto", "Auto", 168000, nil},
		{"hy4-preview-f", "Hy4 preview", 1000000, []string{"high"}},
		{"hy3", "Hy3", 192000, []string{"low", "high"}},
		{"hy3-x", "Hy3-X", 192000, []string{"low", "high"}},
		{"deepseek-v4.1-flash", "Deepseek-V4.1-Flash", 1000000, nil},
		{"deepseek-v4-pro", "Deepseek-V4-Pro", 1000000, []string{"none", "high", "xhigh"}},
		{"glm-5.3", "GLM-5.3", 1000000, []string{"low", "high", "max"}},
		{"glm-5.3-flash", "GLM-5.3-Flash", 1000000, []string{"low", "high", "max"}},
		{"glm-5.2", "GLM-5.2", 1000000, []string{"high", "xhigh"}},
		{"glm-5.1", "GLM-5.1", 200000, nil},
		{"glm-5v-turbo", "GLM-5v-Turbo", 200000, nil},
		{"minimax-m3", "MiniMax-M3", 512000, nil},
		{"kimi-k3-1", "Kimi-K3", 1000000, []string{"low", "high", "xhigh"}},
		{"kimi-k2.7", "Kimi-K2.7-Code", 256000, nil},
		{"kimi-k2.6", "Kimi-K2.6", 256000, nil},
	}
	if provider == "workbuddy-ai" {
		definitions = []definition{
			{"default-model", "Default", 176000, nil},
			{"fast-model", "Fast", 200000, nil},
			{"balanced-model", "Balanced", 256000, nil},
			{"primary-model", "Primary", 272000, nil},
			{"deep-model", "Deep", 176000, nil},
			{"gpt-5.5", "GPT-5.5", 1000000, nil},
			{"gpt-5.4", "GPT-5.4", 272000, nil},
			{"gpt-5.3-codex", "GPT-5.3-Codex", 272000, nil},
			{"gemini-3.1-pro", "Gemini-3.1-Pro", 400000, nil},
			{"gemini-3.5-flash", "Gemini-3.5-Flash", 1000000, nil},
			{"glm-5.3", "GLM-5.3", 1000000, []string{"low", "high", "max"}},
			{"glm-5.2", "GLM-5.2", 1000000, []string{"high", "xhigh"}},
			{"hy3", "Hy3", 192000, []string{"low", "high"}},
			{"kimi-k3", "Kimi-K3", 1000000, nil},
			{"kimi-k2.6", "Kimi-K2.6", 256000, nil},
			{"minimax-m3", "MiniMax-M3", 512000, nil},
		}
	}
	models := make([]*ModelInfo, 0, len(definitions))
	for _, definition := range definitions {
		model := &ModelInfo{ID: definition.id, Name: definition.id, DisplayName: definition.name, Object: "model", OwnedBy: provider, Type: provider,
			ContextLength: definition.context, SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}}
		if len(definition.levels) > 0 {
			model.Thinking = &ThinkingSupport{Levels: definition.levels}
		}
		models = append(models, model)
	}
	return models
}
