package cursor

import (
	"strings"
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

func TestUnbridgeableReservedNamesKeepTheAlias(t *testing.T) {
	tools := []ToolDefinition{
		{Name: "Task", Description: "Launch a subagent"},
		{Name: "TodoWrite", Description: "Track todos"},
		{Name: "Glob", Description: "Find files"},
		{Name: "Bash"},
		{Name: "mcp__demo__thing"},
	}
	out, alias := AliasToolsForCursor(tools, false)
	names := make([]string, 0, len(out))
	for _, tool := range out {
		names = append(names, tool.Name)
	}
	if names[0] != "cpa_Task" || names[1] != "cpa_TodoWrite" || names[2] != "cpa_Glob" {
		t.Fatalf("unbridgeable reserved names must stay aliased: %v", names)
	}
	if names[3] != "Bash" || names[4] != "mcp__demo__thing" {
		t.Fatalf("safe names should be untouched: %v", names)
	}
	if got := alias.ClientName("cpa_Task"); got != "Task" {
		t.Fatalf("reverse map got %q", got)
	}
	if !strings.Contains(out[0].Description, "cpa_Task") {
		t.Fatalf("aliased tool needs steering text, got %q", out[0].Description)
	}
	// Models relayed the old "the built-in tool is unavailable and returns no
	// result" wording to the user as a broken tool environment.
	for _, alarming := range []string{"unavailable", "returns no result", "IMPORTANT"} {
		if strings.Contains(out[0].Description, alarming) {
			t.Fatalf("steering text must not describe the environment as broken: %q", out[0].Description)
		}
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
	_, alias := AliasToolsForCursor([]ToolDefinition{{Name: "Task"}}, false)
	msgs := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "Task"}}},
		{Role: "tool", ToolCallID: "c1", Name: "Task", Content: "data"},
	}
	out := ApplyToolAliasToMessages(msgs, alias)
	if out[0].ToolCalls[0].Name != "cpa_Task" {
		t.Fatalf("tool_call name not aliased: %#v", out[0].ToolCalls[0])
	}
	if out[1].Name != "cpa_Task" {
		t.Fatalf("tool result name not aliased: %#v", out[1])
	}
	if msgs[0].ToolCalls[0].Name != "Task" {
		t.Fatal("input messages must not be mutated")
	}
}
