package cursor

import (
	"regexp"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

// Native tool bridge.
//
// Cursor's Agent always exposes its built-in tools (Shell, Read, Write, Grep,
// LS, …) to the model, and executes them by asking the connected client over
// the exec channel. Instead of re-declaring the client's file/shell tools as
// MCP functions — which collides with the built-in names and used to draw
// provider 400s — the built-in calls are bridged: the exec request becomes a
// tool_use against whichever tool the downstream client actually declared, and
// the client's text result is folded back into the typed protobuf result the
// server expects.

// nativeClientToolCandidates maps each built-in to the client tool names that
// can serve it, in preference order. Matching is case-insensitive and exact.
var nativeClientToolCandidates = map[string][]string{
	"shell":   {"bash", "shell", "run_command", "run_terminal_cmd", "execute_command", "terminal"},
	"read":    {"read", "read_file", "readfile", "view_file", "cat"},
	"write":   {"write", "write_file", "create_file", "save_file"},
	"delete":  {"delete", "delete_file", "remove_file"},
	"grep":    {"grep", "search_file_content", "ripgrep", "rg"},
	"glob":    {"glob", "file_search", "find_files"},
	"ls":      {"ls", "list_dir", "list_directory", "list_files"},
	"fetch":   {"webfetch", "web_fetch", "fetch", "fetch_url"},
	"task":    {"task", "agent", "subagent"},
	"listmcp": {"listmcpresources", "list_mcp_resources"},
}

func findNativeClientTool(kind string, tools []ToolDefinition) *ToolDefinition {
	for _, candidate := range nativeClientToolCandidates[kind] {
		for i := range tools {
			if strings.EqualFold(strings.TrimSpace(tools[i].Name), candidate) {
				return &tools[i]
			}
		}
	}
	return nil
}

// schemaKey returns the first candidate that exists in the tool's declared
// properties, so arguments always match the schema the client validates
// against (file_path vs path, -i vs case_insensitive, and so on). When the
// schema declares none of them the first candidate is used as-is.
func schemaKey(def *ToolDefinition, candidates ...string) string {
	props, _ := def.Parameters["properties"].(map[string]any)
	for _, candidate := range candidates {
		for name := range props {
			if strings.EqualFold(name, candidate) {
				return name
			}
		}
	}
	return candidates[0]
}

func schemaHasKey(def *ToolDefinition, candidates ...string) (string, bool) {
	props, _ := def.Parameters["properties"].(map[string]any)
	for _, candidate := range candidates {
		for name := range props {
			if strings.EqualFold(name, candidate) {
				return name, true
			}
		}
	}
	return "", false
}

// nativeBridge translates a native exec request into an invocation of one of
// the client's declared tools. ok is false when the request is not a bridged
// native tool or the client declared nothing that can serve it.
func nativeBridge(req *agentv1.ExecServerMessage, clientTools []ToolDefinition) (kind string, nativeID string, call ToolCall, ok bool) {
	switch m := req.Message.(type) {
	case *agentv1.ExecServerMessage_ShellArgs:
		return bridgeShell(m.ShellArgs, clientTools)
	case *agentv1.ExecServerMessage_ShellStreamArgs:
		return bridgeShell(m.ShellStreamArgs, clientTools)
	case *agentv1.ExecServerMessage_ReadArgs:
		return bridgeRead(m.ReadArgs, clientTools)
	case *agentv1.ExecServerMessage_RedactedReadArgs:
		return bridgeRead(m.RedactedReadArgs, clientTools)
	case *agentv1.ExecServerMessage_WriteArgs:
		return bridgeWrite(m.WriteArgs, clientTools)
	case *agentv1.ExecServerMessage_DeleteArgs:
		return bridgeDelete(m.DeleteArgs, clientTools)
	case *agentv1.ExecServerMessage_GrepArgs:
		return bridgeGrep(m.GrepArgs, clientTools)
	case *agentv1.ExecServerMessage_LsArgs:
		return bridgeLs(m.LsArgs, clientTools)
	case *agentv1.ExecServerMessage_FetchArgs:
		return bridgeFetch(m.FetchArgs, clientTools)
	case *agentv1.ExecServerMessage_SubagentArgs:
		return bridgeSubagent(m.SubagentArgs, clientTools)
	case *agentv1.ExecServerMessage_ListMcpResourcesExecArgs:
		return bridgeListMcpResources(clientTools)
	}
	return "", "", ToolCall{}, false
}

// isNativeExec reports whether the exec request is one the bridge understands.
func isNativeExec(req *agentv1.ExecServerMessage) bool {
	switch req.Message.(type) {
	case *agentv1.ExecServerMessage_ShellArgs, *agentv1.ExecServerMessage_ShellStreamArgs,
		*agentv1.ExecServerMessage_ReadArgs, *agentv1.ExecServerMessage_RedactedReadArgs,
		*agentv1.ExecServerMessage_WriteArgs, *agentv1.ExecServerMessage_DeleteArgs,
		*agentv1.ExecServerMessage_GrepArgs, *agentv1.ExecServerMessage_LsArgs,
		*agentv1.ExecServerMessage_FetchArgs, *agentv1.ExecServerMessage_SubagentArgs,
		*agentv1.ExecServerMessage_ListMcpResourcesExecArgs:
		return true
	}
	return false
}

func bridgeShell(args *agentv1.ShellArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("shell", tools)
	if def == nil {
		return "shell", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "command", "cmd", "script"): args.GetCommand(),
	}
	if desc := strings.TrimSpace(args.GetDescription()); desc != "" {
		if key, exists := schemaHasKey(def, "description"); exists {
			arguments[key] = desc
		}
	}
	if args.GetTimeout() > 0 {
		if key, exists := schemaHasKey(def, "timeout"); exists {
			arguments[key] = float64(args.GetTimeout())
		}
	}
	if args.GetIsBackground() {
		if key, exists := schemaHasKey(def, "run_in_background"); exists {
			arguments[key] = true
		}
	}
	return "shell", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeRead(args *agentv1.ReadArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("read", tools)
	if def == nil {
		return "read", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "file_path", "path", "target_file", "filename", "file"): args.GetPath(),
	}
	if args.GetOffset() > 0 {
		if key, exists := schemaHasKey(def, "offset", "start_line"); exists {
			arguments[key] = float64(args.GetOffset())
		}
	}
	if args.GetLimit() > 0 {
		if key, exists := schemaHasKey(def, "limit", "line_count"); exists {
			arguments[key] = float64(args.GetLimit())
		}
	}
	return "read", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeWrite(args *agentv1.WriteArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("write", tools)
	if def == nil {
		return "write", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "file_path", "path", "target_file", "filename"): args.GetPath(),
		schemaKey(def, "content", "contents", "file_text", "text"):     args.GetFileText(),
	}
	return "write", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeDelete(args *agentv1.DeleteArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("delete", tools)
	if def == nil {
		return "delete", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "file_path", "path", "target_file"): args.GetPath(),
	}
	return "delete", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeGrep(args *agentv1.GrepArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	// The built-in Glob executes over this same exec arm. A pattern-less
	// request with a glob is a file search, which the client's own Glob tool
	// serves more faithfully than a regex match.
	if strings.TrimSpace(args.GetPattern()) == "" && strings.TrimSpace(args.GetGlob()) != "" {
		log.Infof("cursor: glob-shaped grep exec (glob=%q path=%q mode=%q)", args.GetGlob(), args.GetPath(), args.GetOutputMode())
		if def := findNativeClientTool("glob", tools); def != nil {
			arguments := map[string]any{
				schemaKey(def, "pattern", "glob_pattern", "glob"): args.GetGlob(),
			}
			if args.GetPath() != "" {
				if key, exists := schemaHasKey(def, "path", "target_directory", "directory"); exists {
					arguments[key] = args.GetPath()
				}
			}
			return "grep", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
		}
		// No glob tool: run it as a match-anything grep restricted by the glob.
		if def := findNativeClientTool("grep", tools); def != nil {
			arguments := map[string]any{
				schemaKey(def, "pattern", "query", "regex"): ".",
			}
			if key, exists := schemaHasKey(def, "glob", "include"); exists {
				arguments[key] = args.GetGlob()
			}
			if key, exists := schemaHasKey(def, "output_mode"); exists {
				arguments[key] = "files_with_matches"
			}
			if args.GetPath() != "" {
				if key, exists := schemaHasKey(def, "path", "directory"); exists {
					arguments[key] = args.GetPath()
				}
			}
			return "grep", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
		}
		return "grep", args.GetToolCallId(), ToolCall{}, false
	}
	def := findNativeClientTool("grep", tools)
	if def == nil {
		return "grep", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "pattern", "query", "regex"): args.GetPattern(),
	}
	setIf := func(present bool, value any, candidates ...string) {
		if !present {
			return
		}
		if key, exists := schemaHasKey(def, candidates...); exists {
			arguments[key] = value
		}
	}
	setIf(args.GetPath() != "", args.GetPath(), "path", "directory")
	setIf(args.GetGlob() != "", args.GetGlob(), "glob", "include")
	mode := grepMode(args)
	setIf(true, mode, "output_mode")
	setIf(args.GetCaseInsensitive(), true, "-i", "case_insensitive")
	setIf(args.GetType() != "", args.GetType(), "type")
	setIf(args.GetContextBefore() > 0, float64(args.GetContextBefore()), "-B", "context_before", "before_context")
	setIf(args.GetContextAfter() > 0, float64(args.GetContextAfter()), "-A", "context_after", "after_context")
	setIf(args.GetContext() > 0, float64(args.GetContext()), "-C", "context")
	setIf(args.GetHeadLimit() > 0, float64(args.GetHeadLimit()), "head_limit")
	setIf(args.GetMultiline(), true, "multiline")
	if mode == "content" {
		// Line numbers make the text output reconstructable into the typed
		// content result the server expects back.
		setIf(true, true, "-n", "line_numbers", "show_line_numbers")
	}
	return "grep", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeLs(args *agentv1.LsArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("ls", tools)
	if def == nil {
		return "ls", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "path", "directory", "dir"): args.GetPath(),
	}
	if len(args.GetIgnore()) > 0 {
		if key, exists := schemaHasKey(def, "ignore"); exists {
			values := make([]any, 0, len(args.GetIgnore()))
			for _, item := range args.GetIgnore() {
				values = append(values, item)
			}
			arguments[key] = values
		}
	}
	return "ls", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeFetch(args *agentv1.FetchArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("fetch", tools)
	if def == nil {
		return "fetch", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "url", "uri", "link"): args.GetUrl(),
	}
	if key, exists := schemaHasKey(def, "prompt"); exists {
		arguments[key] = "Return the full page content as markdown."
	}
	return "fetch", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeSubagent(args *agentv1.SubagentArgs, tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("task", tools)
	if def == nil {
		return "task", args.GetToolCallId(), ToolCall{}, false
	}
	arguments := map[string]any{
		schemaKey(def, "prompt", "task", "instructions"): args.GetPrompt(),
	}
	subagentType := strings.TrimSpace(args.GetSubagentType())
	if key, exists := schemaHasKey(def, "subagent_type", "agent_type", "type"); exists {
		if subagentType == "" {
			subagentType = "general-purpose"
		}
		arguments[key] = subagentType
	}
	if key, exists := schemaHasKey(def, "description"); exists {
		description := subagentType
		if description == "" {
			description = "Run a subtask"
		}
		arguments[key] = description
	}
	if args.GetRunInBackground() {
		if key, exists := schemaHasKey(def, "run_in_background"); exists {
			arguments[key] = true
		}
	}
	return "task", args.GetToolCallId(), ToolCall{Name: def.Name, Arguments: arguments}, true
}

func bridgeListMcpResources(tools []ToolDefinition) (string, string, ToolCall, bool) {
	def := findNativeClientTool("listmcp", tools)
	if def == nil {
		return "listmcp", "", ToolCall{}, false
	}
	return "listmcp", "", ToolCall{Name: def.Name, Arguments: map[string]any{}}, true
}

// grepMode normalises the requested output mode; Cursor leaves it empty for
// the default files listing.
func grepMode(args *agentv1.GrepArgs) string {
	mode := strings.TrimSpace(args.GetOutputMode())
	if mode == "" {
		return "files_with_matches"
	}
	return mode
}

// ===== result construction (client text -> typed protobuf) =====

// nativeResultReply folds a client's tool result back into the typed reply the
// exec request expects. nil means the request is not a bridged native tool.
func nativeResultReply(req *agentv1.ExecServerMessage, res ToolResult) *agentv1.ExecClientMessage {
	reply := &agentv1.ExecClientMessage{Id: req.GetId(), ExecId: req.GetExecId()}
	switch m := req.Message.(type) {
	case *agentv1.ExecServerMessage_ShellArgs:
		reply.Message = &agentv1.ExecClientMessage_ShellResult{ShellResult: buildShellResult(m.ShellArgs, res)}
	case *agentv1.ExecServerMessage_ReadArgs:
		reply.Message = &agentv1.ExecClientMessage_ReadResult{ReadResult: buildReadResult(m.ReadArgs, res)}
	case *agentv1.ExecServerMessage_RedactedReadArgs:
		reply.Message = &agentv1.ExecClientMessage_RedactedReadResult{RedactedReadResult: buildReadResult(m.RedactedReadArgs, res)}
	case *agentv1.ExecServerMessage_WriteArgs:
		reply.Message = &agentv1.ExecClientMessage_WriteResult{WriteResult: buildWriteResult(m.WriteArgs, res)}
	case *agentv1.ExecServerMessage_DeleteArgs:
		reply.Message = &agentv1.ExecClientMessage_DeleteResult{DeleteResult: buildDeleteResult(m.DeleteArgs, res)}
	case *agentv1.ExecServerMessage_GrepArgs:
		reply.Message = &agentv1.ExecClientMessage_GrepResult{GrepResult: buildGrepResult(m.GrepArgs, res)}
	case *agentv1.ExecServerMessage_LsArgs:
		reply.Message = &agentv1.ExecClientMessage_LsResult{LsResult: buildLsResult(m.LsArgs, res)}
	case *agentv1.ExecServerMessage_FetchArgs:
		reply.Message = &agentv1.ExecClientMessage_FetchResult{FetchResult: buildFetchResult(m.FetchArgs, res)}
	case *agentv1.ExecServerMessage_SubagentArgs:
		reply.Message = &agentv1.ExecClientMessage_SubagentResult{SubagentResult: buildSubagentResult(m.SubagentArgs, res)}
	case *agentv1.ExecServerMessage_ListMcpResourcesExecArgs:
		reply.Message = &agentv1.ExecClientMessage_ListMcpResourcesExecResult{ListMcpResourcesExecResult: buildListMcpResourcesResult(res)}
	default:
		return nil
	}
	return reply
}

// nativeUnsupportedReply answers an exec the connected client cannot serve
// with a typed error, so the model can adapt instead of the run dying on a
// protocol throw.
func nativeUnsupportedReply(req *agentv1.ExecServerMessage, reason string) *agentv1.ExecClientMessage {
	// Having no MCP resources is a legitimate answer, not a failure.
	if _, isList := req.Message.(*agentv1.ExecServerMessage_ListMcpResourcesExecArgs); isList {
		return &agentv1.ExecClientMessage{
			Id:     req.GetId(),
			ExecId: req.GetExecId(),
			Message: &agentv1.ExecClientMessage_ListMcpResourcesExecResult{
				ListMcpResourcesExecResult: &agentv1.ListMcpResourcesExecResult{
					Result: &agentv1.ListMcpResourcesExecResult_Success{
						Success: &agentv1.ListMcpResourcesSuccess{},
					},
				},
			},
		}
	}
	return nativeResultReply(req, ToolResult{Content: reason, IsError: true})
}

// shellStreamFrames renders a completed client command as the live event
// sequence the streaming shell exec expects: start, output, exit. Answering it
// with a one-shot ShellResult instead is silently discarded by the server and
// the model never sees the output.
func shellStreamFrames(args *agentv1.ShellArgs, res ToolResult) []*agentv1.ShellStream {
	frames := []*agentv1.ShellStream{
		{Event: &agentv1.ShellStream_Start{Start: &agentv1.ShellStreamStart{}}},
	}
	exitCode := uint32(0)
	if res.IsError {
		exitCode = 1
		if match := exitCodePattern.FindStringSubmatch(res.Content); len(match) == 2 {
			if parsed, err := strconv.Atoi(match[1]); err == nil && parsed > 0 {
				exitCode = uint32(parsed)
			}
		}
		if res.Content != "" {
			frames = append(frames, &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: res.Content}},
			})
		}
	} else if res.Content != "" {
		frames = append(frames, &agentv1.ShellStream{
			Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: res.Content}},
		})
	}
	frames = append(frames, &agentv1.ShellStream{
		Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{
			Code: exitCode,
			Cwd:  args.GetWorkingDirectory(),
		}},
	})
	return frames
}

var exitCodePattern = regexp.MustCompile(`(?i)exit(?: code|ed with)[: ]+(-?\d+)`)

func buildShellResult(args *agentv1.ShellArgs, res ToolResult) *agentv1.ShellResult {
	if res.IsError {
		exitCode := int32(1)
		if match := exitCodePattern.FindStringSubmatch(res.Content); len(match) == 2 {
			if parsed, err := strconv.Atoi(match[1]); err == nil {
				exitCode = int32(parsed)
			}
		}
		return &agentv1.ShellResult{
			Result: &agentv1.ShellResult_Failure{Failure: &agentv1.ShellFailure{
				Command:          args.GetCommand(),
				WorkingDirectory: args.GetWorkingDirectory(),
				ExitCode:         exitCode,
				Stderr:           res.Content,
			}},
		}
	}
	return &agentv1.ShellResult{
		Result: &agentv1.ShellResult_Success{Success: &agentv1.ShellSuccess{
			Command:          args.GetCommand(),
			WorkingDirectory: args.GetWorkingDirectory(),
			ExitCode:         0,
			Stdout:           res.Content,
		}},
	}
}

func buildReadResult(args *agentv1.ReadArgs, res ToolResult) *agentv1.ReadResult {
	if res.IsError {
		lowered := strings.ToLower(res.Content)
		if strings.Contains(lowered, "no such file") || strings.Contains(lowered, "does not exist") ||
			strings.Contains(lowered, "not found") {
			return &agentv1.ReadResult{
				Result: &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: args.GetPath()}},
			}
		}
		return &agentv1.ReadResult{
			Result: &agentv1.ReadResult_Error{Error: &agentv1.ReadError{Path: args.GetPath(), Error: res.Content}},
		}
	}
	return &agentv1.ReadResult{
		Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
			Path:         args.GetPath(),
			Output:       &agentv1.ReadSuccess_Content{Content: res.Content},
			TotalLines:   int32(countLines(res.Content)),
			FileSize:     int64(len(res.Content)),
			RangeApplied: args.GetOffset() > 0 || args.GetLimit() > 0,
		}},
	}
}

func buildWriteResult(args *agentv1.WriteArgs, res ToolResult) *agentv1.WriteResult {
	if res.IsError {
		return &agentv1.WriteResult{
			Result: &agentv1.WriteResult_Error{Error: &agentv1.WriteError{Path: args.GetPath(), Error: res.Content}},
		}
	}
	return &agentv1.WriteResult{
		Result: &agentv1.WriteResult_Success{Success: &agentv1.WriteSuccess{
			Path:         args.GetPath(),
			LinesCreated: int32(countLines(args.GetFileText())),
			FileSize:     int32(len(args.GetFileText())),
		}},
	}
}

func buildDeleteResult(args *agentv1.DeleteArgs, res ToolResult) *agentv1.DeleteResult {
	if res.IsError {
		return &agentv1.DeleteResult{
			Result: &agentv1.DeleteResult_Error{Error: &agentv1.DeleteError{Path: args.GetPath(), Error: res.Content}},
		}
	}
	return &agentv1.DeleteResult{
		Result: &agentv1.DeleteResult_Success{Success: &agentv1.DeleteSuccess{Path: args.GetPath()}},
	}
}

func buildGrepResult(args *agentv1.GrepArgs, res ToolResult) *agentv1.GrepResult {
	if res.IsError {
		return &agentv1.GrepResult{
			Result: &agentv1.GrepResult_Error{Error: &agentv1.GrepError{Error: res.Content}},
		}
	}
	mode := grepMode(args)
	workspaceKey := strings.TrimSpace(args.GetPath())
	if workspaceKey == "" {
		workspaceKey = "."
	}
	return &agentv1.GrepResult{
		Result: &agentv1.GrepResult_Success{Success: &agentv1.GrepSuccess{
			Pattern:          args.GetPattern(),
			Path:             args.GetPath(),
			OutputMode:       mode,
			WorkspaceResults: map[string]*agentv1.GrepUnionResult{workspaceKey: parseGrepOutput(mode, res.Content)},
		}},
	}
}

func buildLsResult(args *agentv1.LsArgs, res ToolResult) *agentv1.LsResult {
	if res.IsError {
		return &agentv1.LsResult{
			Result: &agentv1.LsResult_Error{Error: &agentv1.LsError{Path: args.GetPath(), Error: res.Content}},
		}
	}
	root := &agentv1.LsDirectoryTreeNode{
		AbsPath:               args.GetPath(),
		ChildrenWereProcessed: true,
	}
	for _, line := range strings.Split(res.Content, "\n") {
		name := strings.TrimRight(strings.TrimSpace(line), "\r")
		name = strings.TrimPrefix(name, "- ")
		if name == "" || strings.HasPrefix(name, "NOTE:") {
			continue
		}
		if strings.HasSuffix(name, "/") || strings.HasSuffix(name, "\\") {
			root.ChildrenDirs = append(root.ChildrenDirs, &agentv1.LsDirectoryTreeNode{
				AbsPath:               strings.TrimRight(name, "/\\"),
				ChildrenWereProcessed: false,
			})
			continue
		}
		root.ChildrenFiles = append(root.ChildrenFiles, &agentv1.LsDirectoryTreeNode_File{Name: name})
	}
	root.NumFiles = int32(len(root.ChildrenFiles))
	return &agentv1.LsResult{
		Result: &agentv1.LsResult_Success{Success: &agentv1.LsSuccess{DirectoryTreeRoot: root}},
	}
}

func buildFetchResult(args *agentv1.FetchArgs, res ToolResult) *agentv1.FetchResult {
	if res.IsError {
		return &agentv1.FetchResult{
			Result: &agentv1.FetchResult_Error{Error: &agentv1.FetchError{Url: args.GetUrl(), Error: res.Content}},
		}
	}
	return &agentv1.FetchResult{
		Result: &agentv1.FetchResult_Success{Success: &agentv1.FetchSuccess{
			Url:         args.GetUrl(),
			Content:     res.Content,
			StatusCode:  200,
			ContentType: "text/markdown",
		}},
	}
}

func buildSubagentResult(args *agentv1.SubagentArgs, res ToolResult) *agentv1.SubagentResult {
	if res.IsError {
		return &agentv1.SubagentResult{
			Result: &agentv1.SubagentResult_Error{Error: &agentv1.SubagentError{Error: res.Content}},
		}
	}
	final := res.Content
	agentID := strings.TrimSpace(args.GetToolCallId())
	if agentID == "" {
		agentID = "client-subagent"
	}
	return &agentv1.SubagentResult{
		Result: &agentv1.SubagentResult_Success{Success: &agentv1.SubagentSuccess{
			AgentId:      agentID,
			FinalMessage: &final,
		}},
	}
}

func buildListMcpResourcesResult(res ToolResult) *agentv1.ListMcpResourcesExecResult {
	if res.IsError {
		return &agentv1.ListMcpResourcesExecResult{
			Result: &agentv1.ListMcpResourcesExecResult_Error{Error: &agentv1.ListMcpResourcesError{Error: res.Content}},
		}
	}
	success := &agentv1.ListMcpResourcesSuccess{}
	for _, line := range strings.Split(res.Content, "\n") {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if trimmed == "" || !strings.Contains(trimmed, "://") {
			continue
		}
		fields := strings.Fields(trimmed)
		success.Resources = append(success.Resources, &agentv1.ListMcpResourcesExecResult_McpResource{
			Uri:    fields[0],
			Server: "client",
		})
	}
	return &agentv1.ListMcpResourcesExecResult{
		Result: &agentv1.ListMcpResourcesExecResult_Success{Success: success},
	}
}

// ===== grep text output reconstruction =====

var (
	grepMatchLine   = regexp.MustCompile(`^(.+?):(\d+):(.*)$`)
	grepContextLine = regexp.MustCompile(`^(.+?)-(\d+)-(.*)$`)
	grepCountLine   = regexp.MustCompile(`^(.+?):(\d+)$`)
	grepHeaderLine  = regexp.MustCompile(`^(Found \d+|No matches|No files)`)
)

// parseGrepOutput reconstructs ripgrep-style client text into the typed result
// the requested output mode calls for. Reconstruction is best-effort: lines
// that do not parse are kept in a way the model can still see.
func parseGrepOutput(mode, text string) *agentv1.GrepUnionResult {
	lines := splitGrepLines(text)
	switch mode {
	case "count":
		result := &agentv1.GrepCountResult{}
		total := int32(0)
		for _, line := range lines {
			if match := grepCountLine.FindStringSubmatch(line); len(match) == 3 {
				count, err := strconv.Atoi(match[2])
				if err != nil {
					continue
				}
				result.Counts = append(result.Counts, &agentv1.GrepFileCount{File: match[1], Count: int32(count)})
				total += int32(count)
			}
		}
		result.TotalFiles = int32(len(result.Counts))
		result.TotalMatches = total
		return &agentv1.GrepUnionResult{Result: &agentv1.GrepUnionResult_Count{Count: result}}
	case "content":
		result := &agentv1.GrepContentResult{}
		var current *agentv1.GrepFileMatch
		appendMatch := func(file string, lineNo int32, content string, isContext bool) {
			if current == nil || current.File != file {
				current = &agentv1.GrepFileMatch{File: file}
				result.Matches = append(result.Matches, current)
			}
			current.Matches = append(current.Matches, &agentv1.GrepContentMatch{
				LineNumber:    lineNo,
				Content:       content,
				IsContextLine: isContext,
			})
			result.TotalLines++
			if !isContext {
				result.TotalMatchedLines++
			}
		}
		for _, line := range lines {
			if match := grepMatchLine.FindStringSubmatch(line); len(match) == 4 {
				if lineNo, err := strconv.Atoi(match[2]); err == nil {
					appendMatch(match[1], int32(lineNo), match[3], false)
					continue
				}
			}
			if match := grepContextLine.FindStringSubmatch(line); len(match) == 4 {
				if lineNo, err := strconv.Atoi(match[2]); err == nil {
					appendMatch(match[1], int32(lineNo), match[3], true)
					continue
				}
			}
			// Unparseable but real output: keep it visible rather than drop it.
			appendMatch("output", int32(result.TotalLines+1), line, false)
		}
		return &agentv1.GrepUnionResult{Result: &agentv1.GrepUnionResult_Content{Content: result}}
	default: // files_with_matches
		result := &agentv1.GrepFilesResult{}
		for _, line := range lines {
			result.Files = append(result.Files, line)
		}
		result.TotalFiles = int32(len(result.Files))
		return &agentv1.GrepUnionResult{Result: &agentv1.GrepUnionResult_Files{Files: result}}
	}
}

func splitGrepLines(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || grepHeaderLine.MatchString(trimmed) {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	trimmed := strings.TrimSuffix(text, "\n")
	if trimmed == "" {
		return 1
	}
	return strings.Count(trimmed, "\n") + 1
}
