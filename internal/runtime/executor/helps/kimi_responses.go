package helps

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	kimiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ResolveKimiBaseURL respects explicit endpoints before selecting the domain default.
func ResolveKimiBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/"); raw != "" {
			return raw
		}
	}
	if auth != nil {
		if raw, ok := auth.Metadata["base_url"].(string); ok && strings.TrimSpace(raw) != "" {
			return strings.TrimRight(strings.TrimSpace(raw), "/")
		}
	}
	return kimiauth.ResolveKimiAPIBaseURL(kimiauth.ResolveKimiDomainFromAuth(auth))
}

// ResolveKimiResponsesURL resolves the domain-specific Responses endpoint.
func ResolveKimiResponsesURL(auth *cliproxyauth.Auth) string {
	base := ResolveKimiBaseURL(auth)
	if strings.HasSuffix(base, "/v1") {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

// ResolveKimiChatURL resolves the domain-specific Chat Completions endpoint.
func ResolveKimiChatURL(auth *cliproxyauth.Auth) string {
	base := ResolveKimiBaseURL(auth)
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// ResolveKimiClaudeBaseURL resolves the domain-specific Messages delegation endpoint.
func ResolveKimiClaudeBaseURL(auth *cliproxyauth.Auth) string {
	return strings.TrimSuffix(ResolveKimiBaseURL(auth), "/v1")
}

func isResponsesToolCall(item ojson.Result) bool {
	t := strings.TrimSpace(item.Get("type").String())
	return t == "function_call" || t == "custom_tool_call"
}

func isResponsesToolOutput(item ojson.Result) bool {
	t := strings.TrimSpace(item.Get("type").String())
	return t == "function_call_output" || t == "custom_tool_call_output"
}

// NormalizeKimiResponsesInput keeps outputs for parallel calls contiguous.
func NormalizeKimiResponsesInput(body []byte) ([]byte, error) {
	if len(body) == 0 || !ojson.ValidBytes(body) {
		return body, nil
	}
	doc := ojson.NewDoc(body)
	changed, errNorm := NormalizeKimiResponsesInputDoc(doc)
	if errNorm != nil {
		return body, errNorm
	}
	if !changed {
		return body, nil
	}
	return doc.Bytes(), nil
}

// NormalizeKimiResponsesInputDoc defers intervening messages until all tool outputs
// for a batch of parallel calls have been emitted, without crossing another batch.
func NormalizeKimiResponsesInputDoc(doc *ojson.Doc) (bool, error) {
	if doc == nil || doc.Len() == 0 {
		return false, nil
	}
	input := doc.Get("input")
	if !input.Exists() || !input.IsArray() {
		return false, nil
	}
	items := input.Array()
	if len(items) == 0 {
		return false, nil
	}
	reordered := false
	result := make([]ojson.Result, 0, len(items))
	for i := 0; i < len(items); {
		if !isResponsesToolCall(items[i]) {
			result = append(result, items[i])
			i++
			continue
		}
		startCalls := i
		endCalls := i
		callIDs := make(map[string]int)
		callIDCount := 0
		for endCalls < len(items) && isResponsesToolCall(items[endCalls]) {
			callID := translatorcommon.ExtractResponsesCallID(items[endCalls])
			if callID != "" {
				callIDs[callID]++
				callIDCount++
			}
			endCalls++
		}
		result = append(result, items[startCalls:endCalls]...)
		if callIDCount == 0 {
			i = endCalls
			continue
		}
		needed := make(map[string]int, len(callIDs))
		for k, v := range callIDs {
			needed[k] = v
		}
		remainingNeeded := callIDCount
		lastMatchingIdx := -1
		for j := endCalls; j < len(items) && remainingNeeded > 0; j++ {
			it := items[j]
			if isResponsesToolCall(it) {
				break
			}
			if isResponsesToolOutput(it) {
				cid := translatorcommon.ExtractResponsesCallID(it)
				if count := needed[cid]; count > 0 {
					needed[cid]--
					remainingNeeded--
					lastMatchingIdx = j
				}
			}
		}
		if remainingNeeded == 0 && lastMatchingIdx >= endCalls {
			matchingOutputs := make([]ojson.Result, 0, callIDCount)
			interveningItems := make([]ojson.Result, 0)
			consumed := make(map[string]int, len(callIDs))
			for k, v := range callIDs {
				consumed[k] = v
			}
			for j := endCalls; j <= lastMatchingIdx; j++ {
				it := items[j]
				if isResponsesToolOutput(it) {
					cid := translatorcommon.ExtractResponsesCallID(it)
					if count := consumed[cid]; count > 0 {
						consumed[cid]--
						matchingOutputs = append(matchingOutputs, it)
						continue
					}
				}
				interveningItems = append(interveningItems, it)
			}
			if len(interveningItems) > 0 {
				reordered = true
			}
			result = append(result, matchingOutputs...)
			result = append(result, interveningItems...)
			i = lastMatchingIdx + 1
		} else {
			i = endCalls
		}
	}
	if !reordered {
		return false, nil
	}
	rebuilt := make([]string, 0, len(result))
	for _, it := range result {
		rebuilt = append(rebuilt, it.Raw)
	}
	if errSet := doc.SetRaw("input", "["+strings.Join(rebuilt, ",")+"]"); errSet != nil {
		return false, fmt.Errorf("normalize kimi responses input: %w", errSet)
	}
	return true, nil
}
