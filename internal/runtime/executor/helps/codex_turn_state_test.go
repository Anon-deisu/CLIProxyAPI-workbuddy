package helps

import (
	"strings"
	"testing"
)

func TestRedactNativeCodexTurnStatePayload(t *testing.T) {
	for _, payload := range []string{
		`{"client_metadata":{"x-codex-turn-state":"gAAAAA-native-secret"},"model":"codex"}`,
		`{"events":[{"current_turn_state":"gAAAAA-native-secret"}],"model":"codex"}`,
		`{"headers":{"X-Codex-Turn-State":"gAAAAA-native-secret"},"model":"codex"}`,
	} {
		got := string(RedactCodexTurnStatePayload([]byte(payload)))
		if strings.Contains(got, "native-secret") || !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, `"model":"codex"`) {
			t.Fatalf("incorrect protocol token redaction: %s", got)
		}
	}
	for _, payload := range []string{`{"model":"codex"}`, `{"current_turn_state":"ordinary-value"}`, `invalid-json`} {
		if got := string(RedactCodexTurnStatePayload([]byte(payload))); got != payload {
			t.Fatalf("unrelated payload changed: %s", got)
		}
	}
}
