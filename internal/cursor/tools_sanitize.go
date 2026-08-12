package cursor

import (
	"math"
	"sort"
	"strings"
)

const (
	maxToolDescriptionRunes = 800
	// DefaultMaxCursorTools is the first-attempt cap. Provider 400s were traced
	// to tool-name collisions with Cursor built-ins (see tool_alias.go), not to
	// tool count, so Claude Code's full catalog can be forwarded.
	DefaultMaxCursorTools = 64
	// RetryMaxCursorTools is used after a provider end-stream failure.
	RetryMaxCursorTools = 24
)

// metaSchemaKeys are JSON Schema meta fields that Cursor/provider often reject
// when tools are projected into Agent MCP definitions (seen as provider 400).
var metaSchemaKeys = map[string]struct{}{
	"$schema":  {},
	"$id":      {},
	"$comment": {},
}

// SanitizeToolDefinitions cleans client tool definitions before they are sent
// to Cursor Agent Connect. It is idempotent and safe for nil/empty input.
func SanitizeToolDefinitions(tools []ToolDefinition) []ToolDefinition {
	if len(tools) == 0 {
		return tools
	}
	out := make([]ToolDefinition, 0, len(tools))
	// A Cursor native tool owns its public name. If an external MCP definition
	// uses the same name, keep the native definition and do not advertise an
	// ambiguous MCP leaf.
	nativeNames := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		name := strings.ToLower(strings.TrimSpace(tool.Name))
		if name != "" && hasNativeCursorToolMapping(tool) {
			nativeNames[name] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if !hasNativeCursorToolMapping(tool) {
			if _, conflicts := nativeNames[key]; conflicts {
				continue
			}
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ToolDefinition{
			Name:        name,
			Description: truncateRunes(strings.TrimSpace(tool.Description), maxToolDescriptionRunes),
			Parameters:  sanitizeToolSchema(tool.Parameters),
			Kind:        strings.ToLower(strings.TrimSpace(tool.Kind)),
			Namespace:   strings.TrimSpace(tool.Namespace),
		})
	}
	return out
}

func sanitizeToolSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	cleaned := sanitizeSchemaValue(schema)
	obj, ok := cleaned.(map[string]any)
	if !ok || obj == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	typ, hasType := obj["type"]
	if !hasType || typ != "object" {
		// Anthropic/OpenAI tools are JSON-object argument bags.
		obj["type"] = "object"
	}
	properties, ok := obj["properties"].(map[string]any)
	if !ok || properties == nil {
		properties = map[string]any{}
		obj["properties"] = properties
	}
	if required, ok := obj["required"].([]any); ok {
		seen := map[string]struct{}{}
		cleaned := make([]any, 0, len(required))
		for _, raw := range required {
			name, ok := raw.(string)
			if !ok {
				continue
			}
			if _, exists := properties[name]; !exists {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			cleaned = append(cleaned, name)
		}
		if len(cleaned) == 0 {
			delete(obj, "required")
		} else {
			obj["required"] = cleaned
		}
	}
	return obj
}

func sanitizeSchemaValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			if _, drop := metaSchemaKeys[key]; drop {
				continue
			}
			out[key] = sanitizeSchemaValue(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = sanitizeSchemaValue(child)
		}
		return out
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		return v
	default:
		return value
	}
}

func truncateRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

// coreClaudeCodeTools are kept first when capping — Claude Code's default
// catalog puts many MCP/helper tools ahead of Bash/Read, and a blind 12-tool
// slice would drop the tools the agent actually needs.
var coreClaudeCodeTools = []string{
	"Bash", "Read", "Write", "Edit", "Glob", "Grep",
	"Agent", "WebFetch", "WebSearch", "NotebookEdit",
	"TodoWrite", "Skill", "AskUserQuestion", "TaskOutput",
}

// PreferredToolNamesFromMessages returns tool names referenced by recent
// assistant tool_calls / tool results (most recent first, unique).
func PreferredToolNamesFromMessages(messages []ChatMessage) []string {
	return PreferredToolNames(messages, nil)
}

// PreferredToolNames returns names to keep when capping: recent tool-call
// history first, then names mentioned in the latest user text, then core
// Claude Code tools (so Bash/Read survive when MCP tools dominate the catalog).
func PreferredToolNames(messages []ChatMessage, tools []ToolDefinition) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "tool" {
			add(msg.Name)
		}
		for j := len(msg.ToolCalls) - 1; j >= 0; j-- {
			add(msg.ToolCalls[j].Name)
		}
	}
	if len(tools) > 0 {
		lastUser := ""
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
				lastUser = messages[i].Content
				break
			}
		}
		if lastUser != "" {
			// Longer names first so mcp__demo__tool_39 beats mcp__demo__tool_3.
			names := make([]string, 0, len(tools))
			for _, tool := range tools {
				if n := strings.TrimSpace(tool.Name); n != "" {
					names = append(names, n)
				}
			}
			sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
			for _, name := range names {
				if strings.Contains(lastUser, name) {
					add(name)
				}
			}
		}
	}
	for _, name := range coreClaudeCodeTools {
		add(name)
	}
	return out
}

// CapToolDefinitions keeps at most max tools, preferring names in prefer (in
// order), then filling with the remaining tools in original order.
func CapToolDefinitions(tools []ToolDefinition, max int, prefer []string) []ToolDefinition {
	tools = SanitizeToolDefinitions(tools)
	if max <= 0 || len(tools) <= max {
		return tools
	}
	byName := make(map[string]ToolDefinition, len(tools))
	order := make([]string, 0, len(tools))
	for _, tool := range tools {
		if _, ok := byName[tool.Name]; ok {
			continue
		}
		byName[tool.Name] = tool
		order = append(order, tool.Name)
	}
	out := make([]ToolDefinition, 0, max)
	picked := map[string]struct{}{}
	for _, name := range prefer {
		if len(out) >= max {
			break
		}
		tool, ok := byName[name]
		if !ok {
			continue
		}
		if _, ok := picked[name]; ok {
			continue
		}
		picked[name] = struct{}{}
		out = append(out, tool)
	}
	for _, name := range order {
		if len(out) >= max {
			break
		}
		if _, ok := picked[name]; ok {
			continue
		}
		picked[name] = struct{}{}
		out = append(out, byName[name])
	}
	return out
}
