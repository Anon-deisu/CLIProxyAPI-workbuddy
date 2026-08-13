package cursor

import (
	"testing"
)

func TestNativeBridgedNamesAreNotDeclared(t *testing.T) {
	out, alias := AliasToolsForCursor([]ToolDefinition{
		{Name: "Shell", Description: "Run a shell command"},
		{Name: "Read", Description: "Read a file"},
		{Name: "Write", Description: "Write a file"},
		{Name: "Grep", Description: "Search files"},
		{Name: "PowerShell", Description: "Run a PowerShell command"},
	}, false)
	// The model uses Cursor's built-in Shell/Read/Write/Grep; declaring a
	// renamed MCP copy alongside them only made models pick between two
	// identical tools.
	if len(out) != 1 || out[0].Name != "PowerShell" {
		t.Fatalf("bridged names must not be declared, got %v", out)
	}
	if out[0].Description != "Run a PowerShell command" {
		t.Fatalf("untouched tool description changed: %q", out[0].Description)
	}
	// Nothing was renamed, so history rewriting has nothing to do.
	if !alias.Empty() {
		t.Fatalf("no alias expected for bridged names, got %#v", alias.toClient)
	}
}

func TestEveryNativelyServedNameIsUndeclared(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "Task", Description: "Launch a subagent"},
		{Name: "TodoWrite", Description: "Track todos"},
		{Name: "Glob", Description: "Find files"},
		{Name: "ListMcpResources"},
		{Name: "Bash"},
		{Name: "WebFetch"},
		{Name: "LS"},
		{Name: "Edit", Description: "Edit a file"},
		{Name: "Write", Description: "Write a file"},
		{Name: "mcp__demo__thing"},
		{Name: "NotebookEdit"},
	}
	out, alias := AliasToolsForCursor(tools, false)
	names := make([]string, 0, len(out))
	for _, tool := range out {
		names = append(names, tool.Name)
	}
	// Only tools without a native counterpart are declared, under their own
	// names; nothing is renamed any more in the default path.
	if len(names) != 2 || names[0] != "mcp__demo__thing" || names[1] != "NotebookEdit" {
		t.Fatalf("declared set should be the unmapped remainder, got %v", names)
	}
	if !alias.Empty() {
		t.Fatalf("no renames expected, got %#v", alias.toClient)
	}
}

func TestEditStaysDeclaredWithoutAWriteTool(t *testing.T) {
	// The built-in edit lands as a full-content write exec; without a
	// write-capable client tool there is nothing to bridge it onto, so the
	// client's Edit must stay available as MCP.
	out, _ := AliasToolsForCursor([]ToolDefinition{
		{Name: "Edit"},
		{Name: "mcp__x__y"},
	}, false)
	if len(out) != 2 || out[0].Name != "Edit" {
		t.Fatalf("Edit must stay declared without Write, got %v", out)
	}
}

func TestAliasToolsForCursorForceAll(t *testing.T) {
	// The force fallback is the hard-400 escape hatch: everything gets a
	// prefix, including names that would normally ride the native bridge.
	out, alias := AliasToolsForCursor([]ToolDefinition{{Name: "Bash"}, {Name: "Read"}}, true)
	if len(out) != 2 || out[0].Name != "cpa_Bash" || out[1].Name != "cpa_Read" {
		t.Fatalf("force alias got %v", out)
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
	// Renaming only exists under the forceAll hard-400 fallback now; history
	// must follow the renamed declarations there too.
	_, alias := AliasToolsForCursor([]ToolDefinition{{Name: "my_tool"}}, true)
	msgs := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "my_tool"}}},
		{Role: "tool", ToolCallID: "c1", Name: "my_tool", Content: "data"},
	}
	out := ApplyToolAliasToMessages(msgs, alias)
	if out[0].ToolCalls[0].Name != "cpa_my_tool" {
		t.Fatalf("tool_call name not aliased: %#v", out[0].ToolCalls[0])
	}
	if out[1].Name != "cpa_my_tool" {
		t.Fatalf("tool result name not aliased: %#v", out[1])
	}
	if msgs[0].ToolCalls[0].Name != "my_tool" {
		t.Fatal("input messages must not be mutated")
	}
}
