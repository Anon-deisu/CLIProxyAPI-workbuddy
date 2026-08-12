package cursor

import (
	"fmt"
	"strings"
)

// reservedCursorToolNames are names Cursor's Agent already uses for its own
// tools. Declaring a client tool with the same name makes Cursor's upstream
// model provider reject the entire request (end-stream ERROR_PROVIDER_ERROR
// with providerStatusCode 400), because the merged tool list then carries two
// entries with identical names.
//
// Verified against api2.cursor.sh one name at a time: these 400 on their own,
// while Bash/Edit/MultiEdit/WebFetch/WebSearch/NotebookEdit/Skill/... pass.
var reservedCursorToolNames = map[string]struct{}{
	"task":             {},
	"glob":             {},
	"grep":             {},
	"read":             {},
	"write":            {},
	"todowrite":        {},
	"listmcpresources": {},
	"shell":            {},
}

// cursorToolAliasPrefix keeps aliased names short so model instructions that
// mention the original tool still read naturally.
const cursorToolAliasPrefix = "cpa_"

// ToolAliasMap translates between the tool names a client declared and the
// names actually sent to Cursor.
type ToolAliasMap struct {
	toClient   map[string]string
	toUpstream map[string]string
}

// ClientName maps an upstream (Cursor-facing) tool name back to the client name.
func (m *ToolAliasMap) ClientName(upstream string) string {
	if m == nil || len(m.toClient) == 0 {
		return upstream
	}
	if name, ok := m.toClient[strings.ToLower(strings.TrimSpace(upstream))]; ok {
		return name
	}
	return upstream
}

// UpstreamName maps a client-declared tool name to the name advertised to
// Cursor. Names that were not aliased are returned unchanged.
func (m *ToolAliasMap) UpstreamName(client string) string {
	if m == nil || len(m.toUpstream) == 0 {
		return client
	}
	if name, ok := m.toUpstream[strings.ToLower(strings.TrimSpace(client))]; ok {
		return name
	}
	return client
}

// Empty reports whether any renaming happened.
func (m *ToolAliasMap) Empty() bool { return m == nil || len(m.toClient) == 0 }

// AliasToolsForCursor renames tools whose names collide with Cursor built-ins
// (or with each other, case-insensitively) and returns the reverse mapping.
// When forceAll is set every tool is prefixed, which is the safe fallback if
// Cursor introduces new built-in names we do not know about yet.
func AliasToolsForCursor(tools []ToolDefinition, forceAll bool) ([]ToolDefinition, *ToolAliasMap) {
	if len(tools) == 0 {
		return tools, &ToolAliasMap{}
	}
	alias := &ToolAliasMap{
		toClient:   map[string]string{},
		toUpstream: map[string]string{},
	}
	out := make([]ToolDefinition, 0, len(tools))
	used := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		client := strings.TrimSpace(tool.Name)
		if client == "" {
			continue
		}
		upstream := client
		if forceAll || isReservedCursorToolName(client) {
			upstream = cursorToolAliasPrefix + client
		}
		for i := 2; ; i++ {
			if _, clash := used[strings.ToLower(upstream)]; !clash {
				break
			}
			upstream = fmt.Sprintf("%s%s_%d", cursorToolAliasPrefix, client, i)
		}
		used[strings.ToLower(upstream)] = struct{}{}
		copied := tool
		if upstream != client {
			alias.toClient[strings.ToLower(upstream)] = client
			alias.toUpstream[strings.ToLower(client)] = upstream
			// Renaming alone is not enough: Cursor keeps its own built-in tool
			// of the original name, and a model told to "use Read" picks that
			// one, which cannot execute here ("No exec result"). Steering the
			// choice from the description is what actually works.
			copied.Description = steerToAlias(upstream, client, tool.Description)
		}
		copied.Name = upstream
		out = append(out, copied)
	}
	return out, alias
}

func steerToAlias(aliasName, clientName, description string) string {
	lead := fmt.Sprintf(
		"IMPORTANT: %s is the %s tool. Any built-in tool named %q is unavailable in this environment and "+
			"returns no result, so every %s operation must call %s instead.",
		aliasName, clientName, clientName, clientName, aliasName)
	if strings.TrimSpace(description) == "" {
		return lead
	}
	return lead + "\n\n" + description
}

func isReservedCursorToolName(name string) bool {
	_, ok := reservedCursorToolNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// ApplyToolAliasToMessages rewrites tool names inside conversation history so
// blobs sent to Cursor reference the same names as the tool definitions.
func ApplyToolAliasToMessages(messages []ChatMessage, alias *ToolAliasMap) []ChatMessage {
	if alias.Empty() || len(messages) == 0 {
		return messages
	}
	out := make([]ChatMessage, len(messages))
	copy(out, messages)
	for i := range out {
		if name, ok := alias.toUpstream[strings.ToLower(strings.TrimSpace(out[i].Name))]; ok {
			out[i].Name = name
		}
		if len(out[i].ToolCalls) == 0 {
			continue
		}
		calls := make([]ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if name, ok := alias.toUpstream[strings.ToLower(strings.TrimSpace(calls[j].Name))]; ok {
				calls[j].Name = name
			}
		}
		out[i].ToolCalls = calls
	}
	return out
}
