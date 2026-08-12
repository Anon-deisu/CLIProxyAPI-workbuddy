package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertResponsesToolToOpenAIChatTools(tool gjson.Result) [][]byte {
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "", "function":
		if tJSON, ok := convertResponsesFunctionToolToOpenAIChat(tool, ""); ok {
			return [][]byte{tJSON}
		}
	case "namespace":
		return convertResponsesNamespaceToolToOpenAIChat(tool)
	case "custom", "shell", "local_shell", "apply_patch":
		if tJSON, ok := convertResponsesTypedToolToOpenAIChat(tool, ""); ok {
			return [][]byte{tJSON}
		}
	default:
		return nil
	}
	return nil
}

func convertResponsesNamespaceToolToOpenAIChat(tool gjson.Result) [][]byte {
	namespaceName := strings.TrimSpace(tool.Get("name").String())
	children := tool.Get("tools")
	if !children.Exists() || !children.IsArray() {
		return nil
	}

	var out [][]byte
	children.ForEach(func(_, child gjson.Result) bool {
		childName := responsesToolName(child)
		qualifiedName := qualifyResponsesNamespaceToolName(namespaceName, childName)
		var tJSON []byte
		var ok bool
		if strings.TrimSpace(child.Get("type").String()) == "custom" {
			tJSON, ok = convertResponsesTypedToolToOpenAIChat(child, qualifiedName)
		} else {
			tJSON, ok = convertResponsesFunctionToolToOpenAIChat(child, qualifiedName)
		}
		if ok {
			out = append(out, tJSON)
		}
		return true
	})
	return out
}

func convertResponsesTypedToolToOpenAIChat(tool gjson.Result, overrideName string) ([]byte, bool) {
	kind := strings.TrimSpace(tool.Get("type").String())
	name := strings.TrimSpace(overrideName)
	if name == "" {
		if kind == "shell" || kind == "local_shell" || kind == "apply_patch" {
			name = kind
		} else {
			name = responsesToolName(tool)
		}
	}
	if name == "" {
		return nil, false
	}
	description := responsesToolDescription(tool)
	if description == "" {
		switch kind {
		case "custom":
			description = "Accept raw text input for this custom tool."
		case "shell":
			description = "Execute one or more shell commands in the client environment."
		case "local_shell":
			description = "Execute a command in the client's local shell."
		case "apply_patch":
			description = "Create, delete, or update a file using a structured patch operation."
		}
	}
	parameters := responsesTypedToolSchema(kind)
	if parameters == nil {
		return nil, false
	}
	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	raw, _ := json.Marshal(parameters)
	chatTool, _ = sjson.SetRawBytes(chatTool, "function.parameters", raw)
	return chatTool, true
}

func responsesTypedToolSchema(kind string) map[string]any {
	switch kind {
	case "custom":
		return map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"input": map[string]any{"type": "string"}},
			"required":             []string{"input"},
			"additionalProperties": false,
		}
	case "shell":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"commands":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"max_output_length": map[string]any{"type": "integer", "minimum": 1},
				"timeout_ms":        map[string]any{"type": "integer", "minimum": 1},
			},
			"required":             []string{"commands"},
			"additionalProperties": false,
		}
	case "local_shell":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type":              map[string]any{"type": "string", "const": "exec"},
				"command":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"env":               map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"timeout_ms":        map[string]any{"type": "integer", "minimum": 1},
				"user":              map[string]any{"type": "string"},
				"working_directory": map[string]any{"type": "string"},
			},
			"required":             []string{"command", "env", "type"},
			"additionalProperties": false,
		}
	case "apply_patch":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{"type": "string", "enum": []string{"create_file", "delete_file", "update_file"}},
						"path": map[string]any{"type": "string"},
						"diff": map[string]any{"type": "string"},
					},
					"required": []string{"type", "path"},
				},
			},
			"required":             []string{"operation"},
			"additionalProperties": false,
		}
	default:
		return nil
	}
}

func convertResponsesFunctionToolToOpenAIChat(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := strings.TrimSpace(overrideName)
	if name == "" {
		name = responsesToolName(tool)
	}
	if name == "" {
		return nil, false
	}

	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	if description := responsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	if parameters := responsesToolParameters(tool); parameters.Exists() {
		chatTool, _ = sjson.SetRawBytes(chatTool, "function.parameters", []byte(parameters.Raw))
	}
	return chatTool, true
}

func responsesToolName(tool gjson.Result) string {
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		return name
	}
	return strings.TrimSpace(tool.Get("function.name").String())
}

func responsesToolDescription(tool gjson.Result) string {
	if description := tool.Get("description").String(); description != "" {
		return description
	}
	return tool.Get("function.description").String()
}

func responsesToolParameters(tool gjson.Result) gjson.Result {
	for _, path := range []string{
		"parameters",
		"parametersJsonSchema",
		"input_schema",
		"function.parameters",
		"function.parametersJsonSchema",
	} {
		if parameters := tool.Get(path); parameters.Exists() {
			return parameters
		}
	}
	return gjson.Result{}
}

func qualifyResponsesNamespaceToolName(namespaceName, childName string) string {
	childName = strings.TrimSpace(childName)
	if childName == "" || namespaceName == "" || strings.HasPrefix(childName, "mcp__") {
		return childName
	}
	if strings.HasPrefix(childName, namespaceName) {
		return childName
	}
	if strings.HasSuffix(namespaceName, "__") {
		return namespaceName + childName
	}
	return namespaceName + "__" + childName
}

func splitResponsesQualifiedFunctionCallFromRequest(requestRawJSON []byte, qualifiedName string) (name, namespace string) {
	qualifiedName = strings.TrimSpace(qualifiedName)
	if qualifiedName == "" {
		return "", ""
	}

	tools := gjson.GetBytes(requestRawJSON, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return qualifiedName, ""
	}

	var bestNamespace string
	var bestChild string
	tools.ForEach(func(_, tool gjson.Result) bool {
		if strings.TrimSpace(tool.Get("type").String()) != "namespace" {
			return true
		}
		namespaceName := strings.TrimSpace(tool.Get("name").String())
		if namespaceName == "" {
			return true
		}
		children := tool.Get("tools")
		if !children.Exists() || !children.IsArray() {
			return true
		}
		children.ForEach(func(_, child gjson.Result) bool {
			childName := responsesToolName(child)
			if childName == "" {
				return true
			}
			if qualifyResponsesNamespaceToolName(namespaceName, childName) == qualifiedName {
				bestNamespace = namespaceName
				bestChild = childName
			}
			return true
		})
		return true
	})

	if bestNamespace == "" || bestChild == "" {
		return qualifiedName, ""
	}
	return bestChild, bestNamespace
}

func pickRequestJSON(originalRequestRawJSON, requestRawJSON []byte) []byte {
	if len(originalRequestRawJSON) > 0 && gjson.ValidBytes(originalRequestRawJSON) {
		return originalRequestRawJSON
	}
	if len(requestRawJSON) > 0 && gjson.ValidBytes(requestRawJSON) {
		return requestRawJSON
	}
	return nil
}

func applyResponsesFunctionCallNamespaceFields(item []byte, requestRawJSON []byte, qualifiedName string, itemPath string) []byte {
	name, namespace := splitResponsesQualifiedFunctionCallFromRequest(requestRawJSON, qualifiedName)
	namePath := "name"
	namespacePath := "namespace"
	if itemPath != "" {
		namePath = itemPath + ".name"
		namespacePath = itemPath + ".namespace"
	}
	item, _ = sjson.SetBytes(item, namePath, name)
	if namespace != "" {
		item, _ = sjson.SetBytes(item, namespacePath, namespace)
	} else {
		item, _ = sjson.DeleteBytes(item, namespacePath)
	}
	return item
}

type responsesToolDescriptor struct {
	kind      string
	name      string
	namespace string
}

func describeResponsesToolFromRequest(requestRawJSON []byte, qualifiedName string) responsesToolDescriptor {
	descriptor := responsesToolDescriptor{kind: "function", name: qualifiedName}
	tools := gjson.GetBytes(requestRawJSON, "tools")
	if !tools.IsArray() {
		return descriptor
	}
	tools.ForEach(func(_, tool gjson.Result) bool {
		kind := strings.TrimSpace(tool.Get("type").String())
		if kind == "namespace" {
			namespace := strings.TrimSpace(tool.Get("name").String())
			tool.Get("tools").ForEach(func(_, child gjson.Result) bool {
				childName := responsesToolName(child)
				if qualifyResponsesNamespaceToolName(namespace, childName) != qualifiedName {
					return true
				}
				descriptor = responsesToolDescriptor{
					kind:      strings.TrimSpace(child.Get("type").String()),
					name:      childName,
					namespace: namespace,
				}
				if descriptor.kind == "" {
					descriptor.kind = "function"
				}
				return false
			})
			return descriptor.namespace == ""
		}
		name := responsesToolName(tool)
		if kind == "shell" || kind == "local_shell" || kind == "apply_patch" {
			name = kind
		}
		if name != qualifiedName {
			return true
		}
		descriptor = responsesToolDescriptor{kind: kind, name: name}
		if descriptor.kind == "" {
			descriptor.kind = "function"
		}
		return false
	})
	return descriptor
}

func responsesToolCallItem(requestRawJSON []byte, qualifiedName, callID, arguments, status string) []byte {
	descriptor := describeResponsesToolFromRequest(requestRawJSON, qualifiedName)
	prefix := "fc"
	switch descriptor.kind {
	case "custom":
		prefix = "ctc"
	case "shell":
		prefix = "sh"
	case "local_shell":
		prefix = "lsh"
	case "apply_patch":
		prefix = "ap"
	}
	itemID := fmt.Sprintf("%s_%s", prefix, callID)
	switch descriptor.kind {
	case "custom":
		item := []byte(`{"id":"","type":"custom_tool_call","status":"completed","call_id":"","name":"","input":""}`)
		item, _ = sjson.SetBytes(item, "id", itemID)
		item, _ = sjson.SetBytes(item, "status", status)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		item, _ = sjson.SetBytes(item, "name", descriptor.name)
		item, _ = sjson.SetBytes(item, "input", gjson.Get(arguments, "input").String())
		if descriptor.namespace != "" {
			item, _ = sjson.SetBytes(item, "namespace", descriptor.namespace)
		}
		return item
	case "shell", "local_shell":
		itemType := descriptor.kind + "_call"
		item := []byte(`{"id":"","type":"","status":"completed","call_id":"","action":{}}`)
		item, _ = sjson.SetBytes(item, "id", itemID)
		item, _ = sjson.SetBytes(item, "type", itemType)
		item, _ = sjson.SetBytes(item, "status", status)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if gjson.Valid(arguments) {
			item, _ = sjson.SetRawBytes(item, "action", []byte(arguments))
		}
		return item
	case "apply_patch":
		item := []byte(`{"id":"","type":"apply_patch_call","status":"completed","call_id":"","operation":{}}`)
		item, _ = sjson.SetBytes(item, "id", itemID)
		item, _ = sjson.SetBytes(item, "status", status)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		if operation := gjson.Get(arguments, "operation"); operation.Exists() {
			item, _ = sjson.SetRawBytes(item, "operation", []byte(operation.Raw))
		}
		return item
	default:
		item := []byte(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
		item, _ = sjson.SetBytes(item, "id", itemID)
		item, _ = sjson.SetBytes(item, "status", status)
		item, _ = sjson.SetBytes(item, "arguments", arguments)
		item, _ = sjson.SetBytes(item, "call_id", callID)
		return applyResponsesFunctionCallNamespaceFields(item, requestRawJSON, qualifiedName, "")
	}
}
