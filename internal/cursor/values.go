package cursor

import (
	"encoding/json"
	"strconv"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

func toProtobufValue(v any) (*structpb.Value, error) {
	if v == nil {
		return structpb.NewNullValue(), nil
	}
	return structpb.NewValue(v)
}

func fromProtobufValue(v *structpb.Value) any {
	if v == nil {
		return nil
	}
	raw, err := protojson.Marshal(v)
	if err != nil {
		return v.AsInterface()
	}
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return v.AsInterface()
	}
	// Never re-parse strings here. A JSON-looking argument value ("true",
	// "123", or file content that happens to be JSON) would change type and
	// the client rejects the call with "Invalid tool parameters". Structured
	// payloads that really arrive as strings are unwrapped by
	// coerceArgumentToSchema, which knows the declared type.
	return decoded
}

// coerceArgumentToSchema aligns one decoded argument with the type the tool
// declared. Only conversions the schema asks for are applied.
func coerceArgumentToSchema(value any, propSchema map[string]any) any {
	if propSchema == nil {
		return value
	}
	declared := schemaTypeName(propSchema)
	s, isString := value.(string)
	if !isString {
		// Cursor sometimes narrows an identifier to a number ("taskId": 3).
		// Clients validate against their own schema and reject the call.
		if declared == "string" {
			if text, ok := scalarToString(value); ok {
				return text
			}
		}
		return value
	}
	switch declared {
	case "object", "array":
		trimmed := trimSpaceJSON(s)
		if trimmed == "" {
			return value
		}
		var nested any
		if json.Unmarshal([]byte(trimmed), &nested) != nil {
			return value
		}
		switch nested.(type) {
		case map[string]any:
			if declared == "object" {
				return nested
			}
		case []any:
			if declared == "array" {
				return nested
			}
		}
		return value
	case "number", "integer", "boolean":
		trimmed := trimSpaceJSON(s)
		if trimmed == "" {
			return value
		}
		var nested any
		if json.Unmarshal([]byte(trimmed), &nested) != nil {
			return value
		}
		switch nested.(type) {
		case float64:
			if declared == "number" || (declared == "integer" && nested == float64(int64(nested.(float64)))) {
				return nested
			}
		case bool:
			if declared == "boolean" {
				return nested
			}
		}
		return value
	default:
		return value
	}
}

// scalarToString renders a JSON scalar the way the model would have written it,
// so integral numbers do not gain a decimal point.
func scalarToString(value any) (string, bool) {
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return v.String(), true
	default:
		return "", false
	}
}

// schemaTypeName reads "type" from a JSON Schema node, tolerating the
// ["string","null"] union form.
func schemaTypeName(propSchema map[string]any) string {
	switch t := propSchema["type"].(type) {
	case string:
		return t
	case []any:
		for _, item := range t {
			if name, ok := item.(string); ok && name != "null" {
				return name
			}
		}
	}
	return ""
}

func trimSpaceJSON(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func decodeMcpArguments(args *agentv1.McpArgs, def *ToolDefinition) map[string]any {
	out := map[string]any{}
	if args == nil {
		return out
	}
	props := toolSchemaProperties(def)
	for k, v := range args.GetArgs() {
		value := fromProtobufValue(v)
		if propSchema, ok := props[k]; ok {
			value = coerceArgumentToSchema(value, propSchema)
		}
		out[k] = value
	}
	return out
}

func toolSchemaProperties(def *ToolDefinition) map[string]map[string]any {
	if def == nil || def.Parameters == nil {
		return nil
	}
	raw, _ := def.Parameters["properties"].(map[string]any)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]map[string]any, len(raw))
	for name, node := range raw {
		if m, ok := node.(map[string]any); ok {
			out[name] = m
		}
	}
	return out
}
