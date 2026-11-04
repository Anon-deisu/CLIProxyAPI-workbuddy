package helps

import (
	"bytes"
	"encoding/json"
	"strings"
)

const CodexTurnStateHeader = "X-Codex-Turn-State"

func codexTurnStateTokens(payload []byte) []string {
	if !bytes.Contains(bytes.ToLower(payload), []byte("turn-state")) && !bytes.Contains(payload, []byte("current_turn_state")) {
		return nil
	}
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return nil
	}
	var tokens []string
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if strings.EqualFold(key, CodexTurnStateHeader) || key == "current_turn_state" {
					if token, ok := child.(string); ok {
						tokens = append(tokens, token)
					}
				} else {
					visit(child)
				}
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(value)
	return tokens
}

// RedactCodexTurnStatePayload removes native protocol tokens from request logs.
func RedactCodexTurnStatePayload(payload []byte) []byte {
	for _, token := range codexTurnStateTokens(payload) {
		if strings.HasPrefix(token, "gAAAAA") {
			payload = bytes.ReplaceAll(payload, []byte(token), []byte("[REDACTED]"))
		}
	}
	return payload
}
