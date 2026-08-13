package cursor

import (
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func claudeCodeTools() []ToolDefinition {
	obj := func(props ...string) map[string]any {
		properties := map[string]any{}
		for _, p := range props {
			properties[p] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "object", "properties": properties}
	}
	return []ToolDefinition{
		{Name: "Bash", Parameters: obj("command", "timeout", "description", "run_in_background")},
		{Name: "Read", Parameters: obj("file_path", "offset", "limit")},
		{Name: "Write", Parameters: obj("file_path", "content")},
		{Name: "Grep", Parameters: obj("pattern", "path", "glob", "output_mode", "-i", "-A", "-B", "-C", "-n", "type", "head_limit", "multiline")},
		{Name: "WebFetch", Parameters: obj("url", "prompt")},
	}
}

func execWith(msg any) *agentv1.ExecServerMessage {
	req := &agentv1.ExecServerMessage{Id: 7, ExecId: "exec-7"}
	switch m := msg.(type) {
	case *agentv1.ShellArgs:
		req.Message = &agentv1.ExecServerMessage_ShellArgs{ShellArgs: m}
	case *agentv1.ReadArgs:
		req.Message = &agentv1.ExecServerMessage_ReadArgs{ReadArgs: m}
	case *agentv1.WriteArgs:
		req.Message = &agentv1.ExecServerMessage_WriteArgs{WriteArgs: m}
	case *agentv1.GrepArgs:
		req.Message = &agentv1.ExecServerMessage_GrepArgs{GrepArgs: m}
	case *agentv1.FetchArgs:
		req.Message = &agentv1.ExecServerMessage_FetchArgs{FetchArgs: m}
	}
	return req
}

func TestShellExecBridgesToClientBash(t *testing.T) {
	req := execWith(&agentv1.ShellArgs{
		Command:          "go test ./...",
		WorkingDirectory: "/repo",
		ToolCallId:       "call_native_1",
		Description:      strPtr("run the tests"),
	})
	kind, nativeID, call, ok := nativeBridge(req, claudeCodeTools())
	if !ok || kind != "shell" || nativeID != "call_native_1" {
		t.Fatalf("bridge failed: kind=%s id=%s ok=%v", kind, nativeID, ok)
	}
	if call.Name != "Bash" {
		t.Fatalf("resolved tool = %q, want the client's Bash", call.Name)
	}
	if call.Arguments["command"] != "go test ./..." {
		t.Fatalf("command not carried over: %#v", call.Arguments)
	}
	if call.Arguments["description"] != "run the tests" {
		t.Fatalf("description not carried over: %#v", call.Arguments)
	}
}

func TestReadExecAdaptsToTheClientSchema(t *testing.T) {
	offset := int32(10)
	limit := uint32(40)
	req := execWith(&agentv1.ReadArgs{Path: "/repo/main.go", ToolCallId: "c2", Offset: &offset, Limit: &limit})
	_, _, call, ok := nativeBridge(req, claudeCodeTools())
	if !ok || call.Name != "Read" {
		t.Fatalf("bridge failed: %#v ok=%v", call, ok)
	}
	// Claude Code declares file_path, not path.
	if call.Arguments["file_path"] != "/repo/main.go" {
		t.Fatalf("path not adapted to file_path: %#v", call.Arguments)
	}
	if call.Arguments["offset"] != float64(10) || call.Arguments["limit"] != float64(40) {
		t.Fatalf("range not carried over: %#v", call.Arguments)
	}
}

func TestGrepExecUsesTheClientsFlagNames(t *testing.T) {
	caseInsensitive := true
	after := int32(2)
	mode := "content"
	req := execWith(&agentv1.GrepArgs{
		Pattern:         "func main",
		OutputMode:      &mode,
		CaseInsensitive: &caseInsensitive,
		ContextAfter:    &after,
		ToolCallId:      "c3",
	})
	_, _, call, ok := nativeBridge(req, claudeCodeTools())
	if !ok || call.Name != "Grep" {
		t.Fatalf("bridge failed: %#v", call)
	}
	if call.Arguments["-i"] != true || call.Arguments["-A"] != float64(2) {
		t.Fatalf("ripgrep flags not adapted: %#v", call.Arguments)
	}
	// Content mode needs line numbers or the text cannot be folded back into
	// the typed result.
	if call.Arguments["-n"] != true {
		t.Fatalf("line numbers not requested: %#v", call.Arguments)
	}
}

func TestBridgeReportsMissingClientCapability(t *testing.T) {
	req := execWith(&agentv1.ShellArgs{Command: "ls", ToolCallId: "c4"})
	kind, _, _, ok := nativeBridge(req, []ToolDefinition{{Name: "get_weather"}})
	if ok || kind != "shell" {
		t.Fatalf("expected a shell miss, got kind=%s ok=%v", kind, ok)
	}
	reply := nativeUnsupportedReply(req, "no shell capability")
	shell := reply.GetShellResult()
	if shell == nil || shell.GetFailure() == nil {
		t.Fatalf("unsupported shell must answer a typed failure, got %#v", reply)
	}
}

func TestShellResultCarriesOutputAndExitCode(t *testing.T) {
	args := &agentv1.ShellArgs{Command: "make", WorkingDirectory: "/repo"}
	success := buildShellResult(args, ToolResult{Content: "built ok\n"})
	if success.GetSuccess() == nil || success.GetSuccess().GetStdout() != "built ok\n" {
		t.Fatalf("success stdout lost: %#v", success)
	}
	if success.GetSuccess().GetCommand() != "make" || success.GetSuccess().GetWorkingDirectory() != "/repo" {
		t.Fatalf("command echo lost: %#v", success.GetSuccess())
	}
	failure := buildShellResult(args, ToolResult{Content: "Error: Exit code 2\nmake: *** missing target", IsError: true})
	if failure.GetFailure() == nil || failure.GetFailure().GetExitCode() != 2 {
		t.Fatalf("exit code not recovered from the client text: %#v", failure)
	}
}

func TestReadResultDistinguishesMissingFiles(t *testing.T) {
	args := &agentv1.ReadArgs{Path: "/repo/gone.go"}
	missing := buildReadResult(args, ToolResult{Content: "File does not exist.", IsError: true})
	if missing.GetFileNotFound() == nil {
		t.Fatalf("want file_not_found, got %#v", missing)
	}
	ok := buildReadResult(args, ToolResult{Content: "line1\nline2\nline3"})
	if ok.GetSuccess() == nil || ok.GetSuccess().GetTotalLines() != 3 {
		t.Fatalf("want 3 lines, got %#v", ok)
	}
	if ok.GetSuccess().GetContent() != "line1\nline2\nline3" {
		t.Fatalf("content lost: %#v", ok.GetSuccess())
	}
}

func TestGrepOutputReconstruction(t *testing.T) {
	files := parseGrepOutput("files_with_matches", "Found 2 files\nsrc/a.go\nsrc/b.go\n")
	if got := files.GetFiles(); got == nil || len(got.GetFiles()) != 2 || got.GetTotalFiles() != 2 {
		t.Fatalf("files mode: %#v", files)
	}

	content := parseGrepOutput("content", "src/a.go:12:func main() {\nsrc/a.go-13-\tstart()\nsrc/b.go:4:func helper() {}\n")
	c := content.GetContent()
	if c == nil || len(c.GetMatches()) != 2 {
		t.Fatalf("content mode grouping: %#v", c)
	}
	first := c.GetMatches()[0]
	if first.GetFile() != "src/a.go" || len(first.GetMatches()) != 2 {
		t.Fatalf("first file group wrong: %#v", first)
	}
	if !first.GetMatches()[1].GetIsContextLine() {
		t.Fatalf("context line not tagged: %#v", first.GetMatches()[1])
	}
	if c.GetTotalMatchedLines() != 2 {
		t.Fatalf("matched line count = %d, want 2", c.GetTotalMatchedLines())
	}

	counts := parseGrepOutput("count", "src/a.go:3\nsrc/b.go:1\n")
	if got := counts.GetCount(); got == nil || got.GetTotalMatches() != 4 || len(got.GetCounts()) != 2 {
		t.Fatalf("count mode: %#v", counts)
	}

	// Windows drive letters keep their colon.
	win := parseGrepOutput("content", `C:\repo\a.go:7:x := 1`)
	if win.GetContent().GetMatches()[0].GetFile() != `C:\repo\a.go` {
		t.Fatalf("windows path mangled: %#v", win.GetContent().GetMatches()[0])
	}

	empty := parseGrepOutput("files_with_matches", "No matches found")
	if empty.GetFiles() == nil || len(empty.GetFiles().GetFiles()) != 0 {
		t.Fatalf("no-match output must yield an empty result: %#v", empty)
	}
}

func TestFetchBridgeSuppliesTheRequiredPrompt(t *testing.T) {
	req := execWith(&agentv1.FetchArgs{Url: "https://example.com", ToolCallId: "c5"})
	_, _, call, ok := nativeBridge(req, claudeCodeTools())
	if !ok || call.Name != "WebFetch" {
		t.Fatalf("bridge failed: %#v", call)
	}
	if call.Arguments["url"] != "https://example.com" {
		t.Fatalf("url lost: %#v", call.Arguments)
	}
	if prompt, _ := call.Arguments["prompt"].(string); prompt == "" {
		t.Fatal("client WebFetch requires prompt; the bridge must supply one")
	}
}

func TestNativeResultReplyEchoesExecIdentity(t *testing.T) {
	req := execWith(&agentv1.WriteArgs{Path: "/repo/new.go", FileText: "package main\n", ToolCallId: "c6"})
	reply := nativeResultReply(req, ToolResult{Content: "File created successfully"})
	if reply.GetId() != 7 || reply.GetExecId() != "exec-7" {
		t.Fatalf("exec identity lost: id=%d exec_id=%q", reply.GetId(), reply.GetExecId())
	}
	write := reply.GetWriteResult()
	if write.GetSuccess() == nil || write.GetSuccess().GetLinesCreated() != 1 {
		t.Fatalf("write success shape: %#v", write)
	}
	// MCP requests are not the bridge's business.
	mcpReq := &agentv1.ExecServerMessage{Id: 8, Message: &agentv1.ExecServerMessage_McpArgs{McpArgs: &agentv1.McpArgs{}}}
	if nativeResultReply(mcpReq, ToolResult{}) != nil {
		t.Fatal("mcp exec must fall through to the mcp reply path")
	}
}

func TestSubagentExecBridgesToClientTask(t *testing.T) {
	taskTools := []ToolDefinition{{Name: "Task", Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":        map[string]any{"type": "string"},
			"description":   map[string]any{"type": "string"},
			"subagent_type": map[string]any{"type": "string"},
		},
	}}}
	req := &agentv1.ExecServerMessage{Id: 9, ExecId: "exec-9",
		Message: &agentv1.ExecServerMessage_SubagentArgs{SubagentArgs: &agentv1.SubagentArgs{
			ToolCallId: "call_sub_1", SubagentType: "explore", Prompt: "count the files",
		}}}
	kind, nativeID, call, ok := nativeBridge(req, taskTools)
	if !ok || kind != "task" || nativeID != "call_sub_1" || call.Name != "Task" {
		t.Fatalf("bridge failed: kind=%s id=%s call=%#v ok=%v", kind, nativeID, call, ok)
	}
	if call.Arguments["prompt"] != "count the files" || call.Arguments["subagent_type"] != "explore" {
		t.Fatalf("subagent args lost: %#v", call.Arguments)
	}
	reply := nativeResultReply(req, ToolResult{Content: "There are 42 files."})
	success := reply.GetSubagentResult().GetSuccess()
	if success == nil || success.GetFinalMessage() != "There are 42 files." {
		t.Fatalf("subagent result shape: %#v", reply.GetSubagentResult())
	}
	failed := nativeResultReply(req, ToolResult{Content: "boom", IsError: true})
	if failed.GetSubagentResult().GetError() == nil {
		t.Fatalf("error result shape: %#v", failed.GetSubagentResult())
	}
}

func TestListMcpResourcesAnswersEmptyWhenUndeclared(t *testing.T) {
	req := &agentv1.ExecServerMessage{Id: 10, ExecId: "exec-10",
		Message: &agentv1.ExecServerMessage_ListMcpResourcesExecArgs{
			ListMcpResourcesExecArgs: &agentv1.ListMcpResourcesExecArgs{}}}
	if _, _, _, ok := nativeBridge(req, nil); ok {
		t.Fatal("no client tool: bridge must miss")
	}
	// No MCP resources is a legitimate answer, not a failure the model has to
	// route around.
	reply := nativeUnsupportedReply(req, "ignored")
	success := reply.GetListMcpResourcesExecResult().GetSuccess()
	if success == nil || len(success.GetResources()) != 0 {
		t.Fatalf("want empty success, got %#v", reply.GetListMcpResourcesExecResult())
	}
}

func TestGlobShapedGrepUsesTheClientsGlobTool(t *testing.T) {
	tools := append(claudeCodeTools(), ToolDefinition{Name: "Glob", Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string"},
		},
	}})
	glob := "src/**/*.go"
	path := "/repo"
	req := execWith(&agentv1.GrepArgs{Glob: &glob, Path: &path, ToolCallId: "c9"})
	_, _, call, ok := nativeBridge(req, tools)
	if !ok || call.Name != "Glob" {
		t.Fatalf("glob-shaped grep must use the Glob tool: %#v ok=%v", call, ok)
	}
	if call.Arguments["pattern"] != "src/**/*.go" || call.Arguments["path"] != "/repo" {
		t.Fatalf("glob args lost: %#v", call.Arguments)
	}
	// Without a Glob tool it degrades to a match-anything grep bounded by the
	// glob filter.
	_, _, call2, ok2 := nativeBridge(req, claudeCodeTools())
	if !ok2 || call2.Name != "Grep" {
		t.Fatalf("fallback failed: %#v", call2)
	}
	if call2.Arguments["glob"] != "src/**/*.go" || call2.Arguments["output_mode"] != "files_with_matches" {
		t.Fatalf("fallback args: %#v", call2.Arguments)
	}
}

func TestStreamingShellAnswersWithLiveFrames(t *testing.T) {
	args := &agentv1.ShellArgs{Command: "uname -sr", WorkingDirectory: "/repo"}
	frames := shellStreamFrames(args, ToolResult{Content: "Linux 6.8\n"})
	if len(frames) != 3 {
		t.Fatalf("want start/stdout/exit, got %d frames", len(frames))
	}
	if frames[0].GetStart() == nil {
		t.Fatalf("first frame must be start: %#v", frames[0])
	}
	if frames[1].GetStdout().GetData() != "Linux 6.8\n" {
		t.Fatalf("stdout frame lost: %#v", frames[1])
	}
	exit := frames[2].GetExit()
	if exit == nil || exit.GetCode() != 0 || exit.GetCwd() != "/repo" {
		t.Fatalf("exit frame wrong: %#v", frames[2])
	}

	failed := shellStreamFrames(args, ToolResult{Content: "Error: Exit code 3\nboom", IsError: true})
	if failed[1].GetStderr() == nil || failed[2].GetExit().GetCode() != 3 {
		t.Fatalf("error frames wrong: %#v", failed)
	}
}

func TestPendNativeCallIndexesTheSession(t *testing.T) {
	s := &Session{pending: map[string]*pendingExec{}, clientTools: claudeCodeTools()}
	req := execWith(&agentv1.ShellArgs{Command: "pwd", ToolCallId: "call_pend_1"})
	pause, err := s.handleNativeExec(req)
	if err != nil {
		t.Fatalf("handleNativeExec: %v", err)
	}
	// Native calls have no checkpoint boundary to wait on; the segment must
	// pause right away so the tool_use reaches the client.
	if !pause {
		t.Fatal("a bridged native call must pause the segment")
	}
	item, id := s.lookupPendingTool("call_pend_1")
	if item == nil || id == "" {
		t.Fatal("native call not pended")
	}
	if item.call.Name != "Bash" || item.request.GetShellArgs() == nil {
		t.Fatalf("pended call shape: %#v", item.call)
	}
	if !item.native {
		t.Fatal("bridged call must be marked native for batch gating")
	}
	// And the batch is ready without any checkpoint state.
	batch, ready := s.orderedPendingToolBatch(false)
	if !ready || len(batch) != 1 {
		t.Fatalf("native-only batch not ready: ready=%v len=%d", ready, len(batch))
	}
}

func strPtr(s string) *string { return &s }
