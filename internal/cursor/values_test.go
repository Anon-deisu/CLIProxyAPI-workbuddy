package cursor

import (
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func stringTool() *ToolDefinition {
	return &ToolDefinition{
		Name: "Bash",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
				"timeout": map[string]any{"type": "number"},
				"todos":   map[string]any{"type": "array"},
				"config":  map[string]any{"type": "object"},
				"nullable": map[string]any{
					"type": []any{"string", "null"},
				},
			},
		},
	}
}

func decode(t *testing.T, def *ToolDefinition, kv map[string]any) map[string]any {
	t.Helper()
	args := &agentv1.McpArgs{Args: map[string]*structpb.Value{}}
	for k, v := range kv {
		val, err := structpb.NewValue(v)
		if err != nil {
			t.Fatalf("structpb %s: %v", k, err)
		}
		args.Args[k] = val
	}
	return decodeMcpArguments(args, def)
}

func TestDecodeKeepsJSONLookingStringsAsStrings(t *testing.T) {
	def := stringTool()
	got := decode(t, def, map[string]any{
		"command":  "true",
		"content":  `{"a":1}`,
		"nullable": "null",
	})
	if v, ok := got["command"].(string); !ok || v != "true" {
		t.Fatalf(`command should stay the string "true", got %#v`, got["command"])
	}
	if v, ok := got["content"].(string); !ok || v != `{"a":1}` {
		t.Fatalf("JSON file content must stay a string, got %#v", got["content"])
	}
	if v, ok := got["nullable"].(string); !ok || v != "null" {
		t.Fatalf(`nullable should stay the string "null", got %#v`, got["nullable"])
	}
}

func TestDecodeUnwrapsStructuredStringsWhenSchemaAsks(t *testing.T) {
	def := stringTool()
	got := decode(t, def, map[string]any{
		"todos":   `["a","b"]`,
		"config":  `{"k":"v"}`,
		"timeout": "30",
	})
	if arr, ok := got["todos"].([]any); !ok || len(arr) != 2 {
		t.Fatalf("todos should unwrap to an array, got %#v", got["todos"])
	}
	if obj, ok := got["config"].(map[string]any); !ok || obj["k"] != "v" {
		t.Fatalf("config should unwrap to an object, got %#v", got["config"])
	}
	if n, ok := got["timeout"].(float64); !ok || n != 30 {
		t.Fatalf("timeout should become a number, got %#v", got["timeout"])
	}
}

func TestDecodeRejectsFractionalStringForIntegerSchema(t *testing.T) {
	def := &ToolDefinition{
		Name: "Counter",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count": map[string]any{"type": "integer"},
			},
		},
	}
	got := decode(t, def, map[string]any{"count": "1.5"})
	if v, ok := got["count"].(string); !ok || v != "1.5" {
		t.Fatalf("fractional integer input must stay a string, got %#v", got["count"])
	}
}

func TestDecodeLeavesUnknownPropertiesUntouched(t *testing.T) {
	got := decode(t, stringTool(), map[string]any{"surprise": "123"})
	if v, ok := got["surprise"].(string); !ok || v != "123" {
		t.Fatalf("unknown property must not be retyped, got %#v", got["surprise"])
	}
}

func TestDecodeWithoutSchemaKeepsStrings(t *testing.T) {
	got := decode(t, nil, map[string]any{"x": "[1,2]"})
	if v, ok := got["x"].(string); !ok || v != "[1,2]" {
		t.Fatalf("no schema means no coercion, got %#v", got["x"])
	}
}

func TestDecodeStringifiesScalarsWhenSchemaWantsString(t *testing.T) {
	def := &ToolDefinition{
		Name: "TaskUpdate",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"taskId": map[string]any{"type": "string"},
				"done":   map[string]any{"type": "string"},
				"ratio":  map[string]any{"type": "string"},
				"count":  map[string]any{"type": "number"},
			},
		},
	}
	got := decode(t, def, map[string]any{
		"taskId": float64(3),
		"done":   true,
		"ratio":  1.5,
		"count":  float64(7),
	})
	if v, ok := got["taskId"].(string); !ok || v != "3" {
		t.Fatalf(`taskId should become "3", got %#v`, got["taskId"])
	}
	if v, ok := got["done"].(string); !ok || v != "true" {
		t.Fatalf(`done should become "true", got %#v`, got["done"])
	}
	if v, ok := got["ratio"].(string); !ok || v != "1.5" {
		t.Fatalf(`ratio should become "1.5", got %#v`, got["ratio"])
	}
	if n, ok := got["count"].(float64); !ok || n != 7 {
		t.Fatalf("numeric schema must stay numeric, got %#v", got["count"])
	}
}
