package cursor

import (
	"fmt"
	"testing"
)

func TestSanitizeToolDefinitionsStripsSchemaMetaAndTruncatesDescription(t *testing.T) {
	long := make([]byte, 1200)
	for i := range long {
		long[i] = 'a'
	}
	in := []ToolDefinition{
		{
			Name:        "  Agent  ",
			Description: string(long),
			Parameters: map[string]any{
				"$schema":              "https://json-schema.org/draft/2020-12/schema",
				"$id":                  "tool",
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string"},
				},
			},
		},
		{Name: "", Description: "skip me"},
	}
	out := SanitizeToolDefinitions(in)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1", len(out))
	}
	if out[0].Name != "Agent" {
		t.Fatalf("name=%q", out[0].Name)
	}
	if got := []rune(out[0].Description); len(got) != maxToolDescriptionRunes {
		t.Fatalf("desc runes=%d want %d", len(got), maxToolDescriptionRunes)
	}
	if _, ok := out[0].Parameters["$schema"]; ok {
		t.Fatalf("$schema should be stripped: %#v", out[0].Parameters)
	}
	if _, ok := out[0].Parameters["$id"]; ok {
		t.Fatalf("$id should be stripped: %#v", out[0].Parameters)
	}
	if out[0].Parameters["type"] != "object" {
		t.Fatalf("type=%v", out[0].Parameters["type"])
	}
	props, _ := out[0].Parameters["properties"].(map[string]any)
	if props == nil || props["prompt"] == nil {
		t.Fatalf("properties missing: %#v", out[0].Parameters)
	}
}

func TestSanitizeToolDefinitionsFillsEmptyObjectProperties(t *testing.T) {
	out := SanitizeToolDefinitions([]ToolDefinition{{
		Name:       "noop",
		Parameters: map[string]any{"type": "object"},
	}})
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
	props, _ := out[0].Parameters["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("expected properties map, got %#v", out[0].Parameters)
	}
}

func TestCapToolDefinitionsPrefersRecentNames(t *testing.T) {
	tools := make([]ToolDefinition, 0, 30)
	for i := 0; i < 30; i++ {
		tools = append(tools, ToolDefinition{
			Name:       fmt.Sprintf("t%d", i),
			Parameters: map[string]any{"type": "object"},
		})
	}
	prefer := []string{"t29", "t15", "missing"}
	out := CapToolDefinitions(tools, 5, prefer)
	if len(out) != 5 {
		t.Fatalf("len=%d want 5", len(out))
	}
	if out[0].Name != "t29" || out[1].Name != "t15" {
		t.Fatalf("prefer order got %q %q", out[0].Name, out[1].Name)
	}
	if out[2].Name != "t0" || out[3].Name != "t1" || out[4].Name != "t2" {
		t.Fatalf("fill order got %q %q %q", out[2].Name, out[3].Name, out[4].Name)
	}
}

func TestPreferredToolNamesFromMessages(t *testing.T) {
	names := PreferredToolNamesFromMessages([]ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "a"}, {Name: "b"}}},
		{Role: "tool", Name: "b"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "c"}}},
		{Role: "tool", Name: "c"},
	})
	if len(names) < 2 || names[0] != "c" {
		t.Fatalf("names=%v", names)
	}
}

func TestPreferredToolNamesMentionsInUserText(t *testing.T) {
	tools := make([]ToolDefinition, 0, 40)
	for i := 0; i < 40; i++ {
		tools = append(tools, ToolDefinition{Name: fmt.Sprintf("mcp__demo__tool_%d", i)})
	}
	names := PreferredToolNames([]ChatMessage{
		{Role: "user", Content: "调用工具 mcp__demo__tool_39，参数 q=hello"},
	}, tools)
	if len(names) == 0 || names[0] != "mcp__demo__tool_39" {
		t.Fatalf("names=%v", names)
	}
	capped := CapToolDefinitions(tools, 24, names)
	if capped[0].Name != "mcp__demo__tool_39" {
		t.Fatalf("capped[0]=%q", capped[0].Name)
	}
}
