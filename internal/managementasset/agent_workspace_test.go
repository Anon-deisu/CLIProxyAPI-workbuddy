package managementasset

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestAgentWorkspaceAssetsAndInjection(t *testing.T) {
	input := []byte("<html><body><main>Management</main></body></html>")
	output, errAdd := AddAgentWorkspaceToManagementHTML(input)
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	if bytes.Index(output, []byte(agentWorkspaceMarker)) > bytes.Index(output, []byte("</body>")) {
		t.Fatal("agent workspace was injected outside body")
	}
	again, errAgain := AddAgentWorkspaceToManagementHTML(output)
	if errAgain != nil || !bytes.Equal(again, output) {
		t.Fatal("workspace injection is not idempotent")
	}
	for _, name := range []string{"workspace.js", "workspace.css", "marked.js", "purify.js", "lucide.js"} {
		data, contentType, ok := AgentWorkspaceAsset(name)
		if !ok || len(data) == 0 || contentType == "" {
			t.Fatalf("missing offline asset %s", name)
		}
	}
	if _, _, ok := AgentWorkspaceAsset("../agent_workspace.go"); ok {
		t.Fatal("asset loader exposed source files")
	}
}

func TestRollingTokenUsageInjection(t *testing.T) {
	input := []byte("<html><body><main>Management</main></body></html>")
	output, errAdd := AddRollingTokenUsageToManagementHTML(input)
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	if bytes.Count(output, []byte(rollingTokenUsageMarker)) != 1 {
		t.Fatalf("rolling token usage marker count = %d, want 1", bytes.Count(output, []byte(rollingTokenUsageMarker)))
	}
	if !bytes.Contains(output, []byte("/v8/management/observability/usage/token-summary")) {
		t.Fatal("rolling token usage endpoint missing")
	}
	for _, required := range []string{
		"models",
		"cpa-rolling-token-tooltip",
		"近' + minutes + ' 分钟模型 Token 汇总",
		"sortedModels(rollingTokenData.models)",
		"aria-describedby",
	} {
		if !bytes.Contains(output, []byte(required)) {
			t.Errorf("rolling token usage tooltip is missing %q", required)
		}
	}
	if bytes.Contains(output, []byte("auth_files")) || bytes.Contains(output, []byte("cpa-rolling-token-file-title")) {
		t.Fatal("rolling summary tooltip must not expose auth-file groups")
	}
	again, errAgain := AddRollingTokenUsageToManagementHTML(output)
	if errAgain != nil || !bytes.Equal(again, output) {
		t.Fatal("rolling token usage injection is not idempotent")
	}
}

func TestRollingTokenUsageScriptParses(t *testing.T) {
	node, errNode := exec.LookPath("node")
	if errNode != nil {
		t.Skip("Node.js is required for the inline JavaScript syntax test")
	}
	start := strings.Index(rollingTokenUsageScript, ">") + 1
	end := strings.LastIndex(rollingTokenUsageScript, "</script>")
	command := exec.Command(node, "-e", "new (require('node:vm').Script)(require('node:fs').readFileSync(0, 'utf8'))")
	command.Stdin = strings.NewReader(rollingTokenUsageScript[start:end])
	if output, errRun := command.CombinedOutput(); errRun != nil {
		t.Fatalf("dashboard token usage script syntax: %v\n%s", errRun, output)
	}
}
