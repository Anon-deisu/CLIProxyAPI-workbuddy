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
