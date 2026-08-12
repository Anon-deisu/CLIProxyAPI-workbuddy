package cursor

import (
	"strings"
	"testing"
)

func TestAliasToolsForCursorRenamesShell(t *testing.T) {
	out, alias := AliasToolsForCursor([]ToolDefinition{
		{Name: "Shell", Description: "Run a shell command"},
		{Name: "PowerShell", Description: "Run a PowerShell command"},
	}, false)
	if out[0].Name != "cpa_Shell" {
		t.Fatalf("Shell must be aliased, got %q", out[0].Name)
	}
	if out[1].Name != "PowerShell" {
		t.Fatalf("PowerShell is safe and must be untouched, got %q", out[1].Name)
	}
	if alias.ClientName("cpa_Shell") != "Shell" {
		t.Fatal("reverse mapping missing for Shell")
	}
	if alias.ClientName("CPA_SHELL") != "Shell" {
		t.Fatal("reverse mapping must be case-insensitive")
	}
	if !strings.Contains(out[0].Description, `"Shell"`) {
		t.Fatalf("aliased tool needs steering text, got %q", out[0].Description)
	}
	if !strings.Contains(out[0].Description, "Run a shell command") {
		t.Fatal("original description must be preserved")
	}
	if out[1].Description != "Run a PowerShell command" {
		t.Fatalf("untouched tool description changed: %q", out[1].Description)
	}
}

func TestAliasToolsForCursorRenamesReserved(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "Read"},
		{Name: "Bash"},
		{Name: "TodoWrite"},
		{Name: "mcp__demo__thing"},
	}
	out, alias := AliasToolsForCursor(tools, false)
	names := make([]string, 0, len(out))
	for _, tool := range out {
		names = append(names, tool.Name)
	}
	if names[0] != "cpa_Read" || names[2] != "cpa_TodoWrite" {
		t.Fatalf("reserved names not aliased: %v", names)
	}
	if names[1] != "Bash" || names[3] != "mcp__demo__thing" {
		t.Fatalf("safe names should be untouched: %v", names)
	}
	if got := alias.ClientName("cpa_Read"); got != "Read" {
		t.Fatalf("reverse map got %q", got)
	}
	if got := alias.ClientName("Bash"); got != "Bash" {
		t.Fatalf("passthrough got %q", got)
	}
}

func TestAliasToolsForCursorForceAll(t *testing.T) {
	out, alias := AliasToolsForCursor([]ToolDefinition{{Name: "Bash"}}, true)
	if out[0].Name != "cpa_Bash" {
		t.Fatalf("force alias got %q", out[0].Name)
	}
	if alias.ClientName("cpa_Bash") != "Bash" {
		t.Fatal("reverse map missing under force alias")
	}
}

func TestAliasToolsForCursorBreaksCaseCollisions(t *testing.T) {
	out, _ := AliasToolsForCursor([]ToolDefinition{{Name: "thing"}, {Name: "Thing"}}, false)
	if len(out) != 2 {
		t.Fatalf("expected both tools, got %d", len(out))
	}
	if out[0].Name == out[1].Name {
		t.Fatalf("collision not broken: %v", out)
	}
}

func TestSanitizeToolDefinitionsDropsCaseDuplicates(t *testing.T) {
	out := SanitizeToolDefinitions([]ToolDefinition{
		{Name: "Read"},
		{Name: "read"},
		{Name: "Bash"},
	})
	if len(out) != 2 {
		t.Fatalf("expected dedupe to 2, got %d (%v)", len(out), out)
	}
}

func TestApplyToolAliasToMessages(t *testing.T) {
	_, alias := AliasToolsForCursor([]ToolDefinition{{Name: "Read"}}, false)
	msgs := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "Read"}}},
		{Role: "tool", ToolCallID: "c1", Name: "Read", Content: "data"},
	}
	out := ApplyToolAliasToMessages(msgs, alias)
	if out[0].ToolCalls[0].Name != "cpa_Read" {
		t.Fatalf("tool_call name not aliased: %#v", out[0].ToolCalls[0])
	}
	if out[1].Name != "cpa_Read" {
		t.Fatalf("tool result name not aliased: %#v", out[1])
	}
	if msgs[0].ToolCalls[0].Name != "Read" {
		t.Fatal("input messages must not be mutated")
	}
}
