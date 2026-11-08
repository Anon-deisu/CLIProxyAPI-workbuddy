package managementasset

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestPerformanceAssetsAndInjection(t *testing.T) {
	output, errAdd := AddPerformanceToManagementHTML([]byte("<html><body><main>Dashboard</main></body></html>"))
	if errAdd != nil {
		t.Fatal(errAdd)
	}
	if bytes.Index(output, []byte(performanceMarker)) > bytes.Index(output, []byte("</body>")) {
		t.Fatal("performance script was injected outside the body")
	}
	again, errAgain := AddPerformanceToManagementHTML(output)
	if errAgain != nil || !bytes.Equal(output, again) {
		t.Fatal("performance injection was duplicated")
	}
	for _, name := range []string{"performance.js", "performance.css"} {
		data, contentType, ok := AgentWorkspaceAsset(name)
		if !ok || len(data) == 0 || contentType == "" {
			t.Fatalf("missing performance asset: %s", name)
		}
	}
	if _, _, ok := AgentWorkspaceAsset("performance.go"); ok {
		t.Fatal("performance source was exposed")
	}
	node, errNode := exec.LookPath("node")
	if errNode != nil {
		t.Skip("Node.js is required for JavaScript syntax validation")
	}
	data, _, _ := AgentWorkspaceAsset("performance.js")
	command := exec.Command(node, "--check")
	command.Stdin = bytes.NewReader(data)
	if result, errRun := command.CombinedOutput(); errRun != nil {
		t.Fatalf("performance script syntax: %v\n%s", errRun, result)
	}
}
