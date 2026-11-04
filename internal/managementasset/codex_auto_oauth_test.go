package managementasset

import (
	"bytes"
	"testing"
)

func TestAddCodexAutoOAuthToManagementHTML(t *testing.T) {
	input := []byte(`<html><body><button>Start Codex Login</button></body></html>`)
	output, err := AddCodexAutoOAuthToManagementHTML(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte(codexAutoOAuthMarker)) || !bytes.Contains(output, []byte("codex-auto-oauth")) {
		t.Fatal("Codex automatic OAuth script was not injected")
	}
	second, err := AddCodexAutoOAuthToManagementHTML(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, second) {
		t.Fatal("Codex automatic OAuth injection is not idempotent")
	}
}
