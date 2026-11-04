package helps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestWorkBuddyNormalizesDSHPromptPrefix(t *testing.T) {
	const signature = "You are an AI agent powered by DeepSeek Harness"
	const replacement = "You are an AI agent built with DeepSeek Harness"
	for _, test := range []struct {
		name, input, want string
	}{
		{"first-line", signature + ".", replacement + "."},
		{"no-period", signature, replacement},
		{"uppercase", strings.ToUpper(signature) + ".", replacement + "."},
		{"lowercase", strings.ToLower(signature) + ".", replacement + "."},
		{"full-prompt", signature + ".\n\nKeep all other instructions.", replacement + ".\n\nKeep all other instructions."},
		{"punctuation", signature + "!", replacement + "!"},
		{"already-rewritten", replacement + ".", replacement + "."},
		{"quoted-signature", "Quoted text: " + signature + ".", "Quoted text: " + signature + "."},
		{"leading-space", " " + signature + ".", " " + signature + "."},
		{"later-line", "Context\n" + signature + ".", "Context\n" + signature + "."},
		{"ordinary-prompt", "You are a helpful assistant.", "You are a helpful assistant."},
		{"other-product", "You are an AI agent powered by DeepSeek.", "You are an AI agent powered by DeepSeek."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized prompt = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHGUIContext(t *testing.T) {
	const signature = "You are interacting with the user through the DeepSeek Harness Web GUI"
	const replacement = "You interact with the user through the DeepSeek Harness Web GUI"
	const suffix = " whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI. The browser provides no implicit DOM, route, or screenshot context. The client-plugin HMR receiver is active, but client-plugin changes reload without a refresh only while `pnpm run dev:web` is also running from this same checkout to rebuild their bundles; verify that watcher before promising automatic updates. Every other change requires rebuilding the affected Web artifacts and verifying this existing URL after a page refresh. Starting another server does not update this GUI. The apps/web Vite entry builds the shell but is not a standalone application because only dsh web injects window.__DSH_BOOT__. Do not start a replacement server unless the user asks; if one is needed, use a managed background job and verify its exact URL."
	for _, test := range []struct {
		name, input, want string
	}{
		{"gui-paragraph", signature + suffix, replacement + suffix},
		{"after-first-line", "You are an AI agent powered by DeepSeek Harness.\n\n" + signature + suffix, "You are an AI agent built with DeepSeek Harness.\n\n" + replacement + suffix},
		{"uppercase", strings.ToUpper(signature) + suffix, replacement + suffix},
		{"lowercase", strings.ToLower(signature) + suffix, replacement + suffix},
		{"multiple-blocks", signature + suffix + "\n\n" + signature + suffix, replacement + suffix + "\n\n" + replacement + suffix},
		{"already-rewritten", replacement + suffix, replacement + suffix},
		{"runtime-context-unchanged", "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815", "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815"},
		{"other-gui-unchanged", "You are interacting with the user through another Web GUI.", "You are interacting with the user through another Web GUI."},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized GUI context = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("GUI normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHPathReferenceFingerprint(t *testing.T) {
	const signature = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	const replacement = "Tokens starting with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	for _, test := range []struct {
		name, input, want string
	}{
		{"exact-fingerprint", signature, replacement},
		{"full-paragraph", signature + "; absolute paths identify files or directories on the host.", replacement + "; absolute paths identify files or directories on the host."},
		{"uppercase", strings.ToUpper(signature), replacement},
		{"changed-word", strings.Replace(signature, "referenced", "mentioned", 1), strings.Replace(signature, "referenced", "mentioned", 1)},
		{"line-break", strings.Replace(signature, ". Relative", ".\nRelative", 1), strings.Replace(signature, ". Relative", ".\nRelative", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized path reference = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHPathReferenceMessageForms(t *testing.T) {
	const signature = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	const replacement = "Tokens starting with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	for _, block := range []bool{false, true} {
		t.Run(fmt.Sprintf("text-block=%t", block), func(t *testing.T) {
			var content any = signature + "."
			path := "messages.0.content"
			if block {
				content = []any{
					map[string]any{"type": "text", "text": signature + "."},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}},
				}
				path += ".0.text"
			}
			body, err := json.Marshal(map[string]any{
				"model":    "deepseek-v4.1-flash",
				"messages": []any{map[string]any{"role": "developer", "content": content}},
			})
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := NormalizeWorkBuddyRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, path).String(); got != replacement+"." {
				t.Fatalf("normalized path reference = %q, want %q", got, replacement+".")
			}
			if got := gjson.GetBytes(out, "messages.0.role").String(); got != "system" {
				t.Fatalf("message role = %q, want system", got)
			}
			if block && gjson.GetBytes(out, "messages.0.content.1.image_url.url").String() != "https://example.test/image.png" {
				t.Fatalf("image content changed: %s", out)
			}
		})
	}
}

func TestWorkBuddyNormalizesDSHMessageForms(t *testing.T) {
	const prompt = "You are an AI agent powered by DeepSeek Harness.\nKeep the remaining instructions.\n\nYou are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	const want = "You are an AI agent built with DeepSeek Harness.\nKeep the remaining instructions.\n\nYou interact with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	for _, role := range []string{"system", "developer", "assistant", "user"} {
		for _, block := range []bool{false, true} {
			name := role + "/string"
			var content any = prompt
			if block {
				name = role + "/text-block"
				content = []any{
					map[string]any{"type": "text", "text": prompt},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}},
				}
			}
			t.Run(name, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{
					"model": "deepseek-v4.1-flash",
					"messages": []any{
						map[string]any{"role": "system", "content": "Keep this instruction."},
						map[string]any{"role": role, "content": content},
						map[string]any{"role": "user", "content": "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815"},
					},
					"tools": []any{map[string]any{"type": "function", "function": map[string]any{
						"name": "lookup", "parameters": map[string]any{"type": "object"},
					}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				out, _, err := NormalizeWorkBuddyRequest(body)
				if err != nil {
					t.Fatal(err)
				}
				path := "messages.1.content"
				if block {
					path += ".0.text"
					if got := gjson.GetBytes(out, "messages.1.content.1.image_url.url").String(); got != "https://example.test/image.png" {
						t.Fatalf("image content changed: %s", out)
					}
				}
				if got := gjson.GetBytes(out, path).String(); got != want {
					t.Fatalf("DSH prompt = %q, want %q", got, want)
				}
				wantRole := role
				if role == "developer" {
					wantRole = "system"
				}
				if gjson.GetBytes(out, "messages.1.role").String() != wantRole ||
					gjson.GetBytes(out, "messages.0.content").String() != "Keep this instruction." ||
					gjson.GetBytes(out, "messages.2.content").String() != "Current DeepSeek Harness Web GUI URL: http://127.0.0.1:4815" ||
					gjson.GetBytes(out, "tools.0.function.name").String() != "lookup" {
					t.Fatalf("unrelated request fields changed: %s", out)
				}
			})
		}
	}
}

func TestWorkBuddyNormalizesClaudeFeedbackNotice(t *testing.T) {
	const url = "https://github.com/anthropics/claude-code/issues"
	const signature = "To give feedback, users should report the issue at " + url
	const replacement = "Users can report issues and share feedback at " + url
	for _, test := range []struct {
		name, input, want string
	}{
		{"feedback-line", "  - " + signature, "  - " + replacement},
		{"prompt-context", "# Doing tasks\nKeep these instructions.\n" + signature + "\nNext instruction.", "# Doing tasks\nKeep these instructions.\n" + replacement + "\nNext instruction."},
		{"uppercase", strings.ToUpper(signature), "Users can report issues and share feedback at " + strings.ToUpper(url)},
		{"lowercase", strings.ToLower(signature), replacement},
		{"multiple-notices", signature + "\n" + signature, replacement + "\n" + replacement},
		{"already-normalized", replacement, replacement},
		{"url-only", url, url},
		{"phrase-only", "To give feedback, users should report the issue at", "To give feedback, users should report the issue at"},
		{"another-project", "To give feedback, users should report the issue at https://github.com/example/project/issues", "To give feedback, users should report the issue at https://github.com/example/project/issues"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("normalized feedback notice = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("feedback normalization is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyNormalizesClaudeFeedbackMessageForms(t *testing.T) {
	const notice = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	const replacement = "Users can report issues and share feedback at https://github.com/anthropics/claude-code/issues"
	for _, role := range []string{"system", "developer", "assistant", "user", "tool"} {
		for _, block := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/block=%t", role, block), func(t *testing.T) {
				var content any = notice
				if block {
					content = []any{
						map[string]any{"type": "text", "text": notice},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png"}},
					}
				}
				body, err := json.Marshal(map[string]any{
					"model": "glm-5.3-flash",
					"messages": []any{
						map[string]any{"role": "system", "content": "Keep this instruction."},
						map[string]any{"role": role, "content": content},
					},
					"tools": []any{map[string]any{"type": "function", "function": map[string]any{
						"name": "lookup", "description": notice, "parameters": map[string]any{"type": "object"},
					}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				original := string(body)
				out, _, err := NormalizeWorkBuddyRequest(body)
				if err != nil {
					t.Fatal(err)
				}
				path := "messages.1.content"
				if block {
					path += ".0.text"
					if got := gjson.GetBytes(out, "messages.1.content.1.image_url.url").String(); got != "https://example.test/image.png" {
						t.Fatalf("image content changed: %s", out)
					}
				}
				if got := gjson.GetBytes(out, path).String(); got != replacement {
					t.Fatalf("feedback notice = %q, want %q", got, replacement)
				}
				if string(body) != original || gjson.GetBytes(out, "messages.0.content").String() != "Keep this instruction." ||
					gjson.GetBytes(out, "tools.0.function.description").String() != notice {
					t.Fatal("normalization changed the original request or unrelated fields")
				}
			})
		}
	}
}

func TestWorkBuddyNormalizesStopSequences(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
	}{
		{"string", `"</block>"`, `["</block>"]`},
		{"single-array", `["</block>"]`, `["</block>"]`},
		{"multiple-array", `["</block>","END"]`, `["</block>","END"]`},
		{"empty-string", `""`, `[""]`},
		{"empty-array", `[]`, `[]`},
		{"null", `null`, `null`},
		{"absent", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := map[string]any{
				"messages":   []any{map[string]any{"role": "system", "content": "Keep the monitor instructions."}},
				"max_tokens": 2112,
			}
			if test.input != "" {
				request["stop"] = json.RawMessage(test.input)
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			original := string(body)
			out, _, err := NormalizeWorkBuddyRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			var normalized map[string]any
			if err := json.Unmarshal(out, &normalized); err != nil {
				t.Fatal(err)
			}
			got, exists := normalized["stop"]
			var want any
			if test.want != "" {
				if err := json.Unmarshal([]byte(test.want), &want); err != nil {
					t.Fatal(err)
				}
			}
			if exists != (test.want != "") || !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized stop = %#v (present=%t), want %s", got, exists, test.want)
			}
			if string(body) != original || gjson.GetBytes(out, "messages.0.content").String() != "Keep the monitor instructions." || gjson.GetBytes(out, "max_tokens").Int() != 2112 {
				t.Fatal("stop normalization changed the source request or unrelated fields")
			}
			twice, _, err := NormalizeWorkBuddyRequest(out)
			if err != nil || string(twice) != string(out) {
				t.Fatalf("stop normalization is not idempotent: %v", err)
			}
		})
	}
}

func TestWorkBuddyBillingHeaderPreservesPrompt(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=2.1.306.c1c; cc_entrypoint=claude-vscode;"
	const prompt = "You are a security monitor for autonomous AI coding agents.\n\n## Context\nKeep every instruction."
	for _, test := range []struct {
		name, input, want string
	}{
		{"same-line", billing + prompt, prompt},
		{"next-line", billing + "\n" + prompt, prompt},
		{"crlf", billing + "\r\n" + prompt, prompt},
		{"header-only", billing, ""},
		{"uppercase-header", strings.ToUpper(billing) + prompt, prompt},
		{"extra-fields", billing + " cch=00000; cc_prev_req=req_test;" + prompt, prompt},
		{"instruction-assignment", billing + "decision=deny; " + prompt, "decision=deny; " + prompt},
		{"plain-prompt", prompt, prompt},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeWorkBuddyClientText(test.input)
			if got != test.want {
				t.Fatalf("billing cleanup = %q, want %q", got, test.want)
			}
			if twice := normalizeWorkBuddyClientText(got); twice != got {
				t.Fatalf("billing cleanup is not idempotent: %q", twice)
			}
		})
	}
}

func TestWorkBuddyPreservesExistingClientRewrites(t *testing.T) {
	input := "x-anthropic-billing-header: cc_version=test;\nYou are Claude Code, Anthropic's official CLI for Claude.\nMain branch (you will usually use this for PRs): main\nCodex CLI is an open source project led by OpenAI."
	want := "You are Claude Code, Anthropic's CLI for Claude.\nMain branch (usually the base for PRs): main\nThe Codex CLI is an open source project led by OpenAI."
	if got := normalizeWorkBuddyClientText(input); got != want {
		t.Fatalf("existing client rewrites changed: %q", got)
	}
}

func TestParseWorkBuddyErrorQuotaExhausted(t *testing.T) {
	const message = "\u989d\u5ea6\u5df2\u7528\u5c3d"
	for _, payload := range []string{
		`{"code":14018,"msg":"` + message + `"}`,
		`{"error":{"code":"14018","message":"` + message + `"}}`,
		`{"error":{"data":{"code":14018,"msg":"` + message + `"}}}`,
		`{"msg":"` + message + `","error":{"data":{"code":14018}}}`,
		`{"code":500,"error":{"code":502,"message":"gateway error","data":{"code":"14018","message":"` + message + `"}}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			code, gotMessage, ok := parseWorkBuddyError([]byte(payload))
			if !ok || code != "14018" || gotMessage != message {
				t.Fatalf("parsed error = (%q, %q, %t), want (14018, %q, true)", code, gotMessage, ok, message)
			}
		})
	}
}

func TestWorkBuddyRateLimitResetTime(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		payload   string
		wantDelay *time.Duration
		limited   bool
	}{
		{"reported-cn-reset", `{"code":6004,"msg":"\u60a8\u7684\u4f7f\u7528\u91cf\u5df2\u8d85\u51fa\u9891\u7387\u9650\u5236\uff0c\u5c06\u5728 2026-10-21 00:14:52 UTC+8 \u91cd\u7f6e"}`, new(8*time.Hour + 14*time.Minute + 52*time.Second), true},
		{"string-code-offset-minutes", `{"error":{"code":"6004","message":"reset 2026-10-20 12:00:00 UTC+03:00"}}`, new(time.Hour), true},
		{"nested-data-reset", `{"error":{"data":{"code":6004,"msg":"reset 2026-10-20 12:00:00 UTC+03:00"}}}`, new(time.Hour), true},
		{"negative-offset", `{"code":6004,"message":"reset 2026-10-20 03:30:00 UTC-05:30"}`, new(time.Hour), true},
		{"at-reset", `{"code":6004,"msg":"reset 2026-10-20 16:00:00 UTC+8"}`, new(time.Duration(0)), true},
		{"past-reset", `{"code":6004,"msg":"reset 2026-10-19 16:00:00 UTC+8"}`, new(time.Duration(0)), true},
		{"no-reset-time", `{"code":6004,"msg":"rate limit"}`, nil, true},
		{"invalid-date", `{"code":6004,"msg":"reset 2026-02-30 16:00:00 UTC+8"}`, nil, true},
		{"invalid-zone", `{"code":6004,"msg":"reset 2026-10-20 16:00:00 UTC+88"}`, nil, true},
		{"invalid-zone-minutes", `{"code":6004,"msg":"reset 2026-10-20 16:00:00 UTC+08:99"}`, nil, true},
		{"other-code", `{"code":429,"msg":"reset 2026-10-20 16:00:00 UTC+8"}`, nil, false},
		{"success-text", `{"choices":[{"delta":{"content":"error code 6004"}}]}`, nil, false},
		{"invalid-json", `{"code":6004`, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(test.payload)
			if got := IsWorkBuddyRateLimit(payload); got != test.limited {
				t.Fatalf("IsWorkBuddyRateLimit() = %t, want %t", got, test.limited)
			}
			got := WorkBuddyRateLimitRetryAfter(payload, now)
			if (got == nil) != (test.wantDelay == nil) {
				t.Fatalf("retry delay = %v, want %v", got, test.wantDelay)
			}
			if got != nil && *got != *test.wantDelay {
				t.Fatalf("retry delay = %v, want %v", *got, *test.wantDelay)
			}
		})
	}
}

func TestWorkBuddyModelRateLimitResetTime(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		payload   string
		wantDelay *time.Duration
		limited   bool
	}{
		{"root-code", `{"code":6000,"msg":"model limit reset 2026-10-21 00:14:52 UTC+8"}`, new(8*time.Hour + 14*time.Minute + 52*time.Second), true},
		{"nested-string-code", `{"error":{"data":{"code":"6000","message":"reset 2026-10-20 12:00:00 UTC+03:00"}}}`, new(time.Hour), true},
		{"no-reset", `{"error":{"code":6000,"message":"model rate limit"}}`, nil, true},
		{"account-code-is-not-model-limit", `{"code":6004,"msg":"account rate limit"}`, nil, false},
		{"other-code", `{"code":429,"msg":"model limit reset 2026-10-20 12:00:00 UTC+8"}`, nil, false},
		{"success-text", `{"choices":[{"delta":{"content":"error code 6000"}}]}`, nil, false},
		{"invalid-json", `{"code":6000`, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(test.payload)
			if got := IsWorkBuddyModelRateLimit(payload); got != test.limited {
				t.Fatalf("IsWorkBuddyModelRateLimit() = %t, want %t", got, test.limited)
			}
			if got := IsWorkBuddyRateLimit(payload); got != (test.name == "account-code-is-not-model-limit") {
				t.Fatalf("IsWorkBuddyRateLimit() = %t for %s", got, test.name)
			}
			got := WorkBuddyRateLimitRetryAfter(payload, now)
			if (got == nil) != (test.wantDelay == nil) {
				t.Fatalf("retry delay = %v, want %v", got, test.wantDelay)
			}
			if got != nil && *got != *test.wantDelay {
				t.Fatalf("retry delay = %v, want %v", *got, *test.wantDelay)
			}
		})
	}
}

func TestWorkBuddyNormalizesMCPSchemasAndPreservesToolIdentity(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"mcp.search","arguments":"{}"}}]},{"role":"tool","name":"mcp.search","tool_call_id":"1","content":"ok"}],"tools":[{"type":"function","function":{"name":"mcp_search","parameters":{}}},{"type":"function","function":{"name":"mcp.search","parameters":{"properties":{"format":{"enum":["json","text"]},"literal":{"enum":[1,"one",null]},"allow":true,"deny":false,"labels":{"type":"object","additionalProperties":false}}}}}],"tool_choice":{"type":"function","function":{"name":"mcp.search"}}}`)
	out, names, err := NormalizeWorkBuddyRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if names["mcp_search_2"] != "mcp.search" || gjson.GetBytes(out, "messages.1.tool_calls.0.function.name").String() != "mcp_search_2" || gjson.GetBytes(out, "tool_choice").String() != "mcp_search_2" {
		t.Fatalf("tool name collision or history was not preserved: %s", out)
	}
	for path, expected := range map[string]string{
		"tools.1.function.parameters.type": "object", "tools.1.function.parameters.properties.format.type": "string",
		"tools.1.function.parameters.properties.literal.anyOf.0.type": "number", "tools.1.function.parameters.properties.allow": "{}",
		"tools.1.function.parameters.properties.deny.not": "{}", "tools.1.function.parameters.properties.labels.additionalProperties": "false",
	} {
		if got := gjson.GetBytes(out, path).String(); got != expected {
			t.Errorf("%s = %s, want %s", path, got, expected)
		}
	}
	none, _, err := NormalizeWorkBuddyRequest([]byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"tool"}}],"tool_choice":"none"}`))
	if err != nil || gjson.GetBytes(none, "tools").Exists() || gjson.GetBytes(none, "tool_choice").Exists() {
		t.Fatal("tool_choice none still exposed tools to the model")
	}
}

func TestWorkBuddyToolStepsShareTurnAndConversationIDs(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"Search"}]}`)
	step := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","tool_calls":[{"id":"1"}]},{"role":"tool","content":"found"}]}`)
	next := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","content":"found"},{"role":"user","content":"Next"}]}`)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Conversation-Id": []string{"client-session"}}}
	requests := make([]*http.Request, 0, 3)
	for _, body := range [][]byte{first, step, next} {
		req, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		ApplyWorkBuddyConversationHeaders(req, body, opts)
		requests = append(requests, req)
		if req.Header.Get("X-Conversation-ID") != "client-session" || strings.Contains(req.Header.Get("X-Conversation-Request-ID"), "Search") {
			t.Fatal("conversation identity lost or message text exposed")
		}
	}
	if requests[0].Header.Get("X-Conversation-Request-ID") != requests[1].Header.Get("X-Conversation-Request-ID") || requests[0].Header.Get("X-Conversation-Request-ID") == requests[2].Header.Get("X-Conversation-Request-ID") || requests[0].Header.Get("X-Request-ID") == requests[1].Header.Get("X-Request-ID") {
		t.Fatal("tool steps or the next user turn have incorrect request identities")
	}
}

func TestWorkBuddyFingerprintToolTurnsAndConcurrentRequests(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"Search"}]}`)
	step := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","tool_calls":[{"id":"1"}]},{"role":"tool","content":"found"}]}`)
	next := []byte(`{"messages":[{"role":"user","content":"Search"},{"role":"assistant","content":"found"},{"role":"user","content":"Next"}]}`)
	auth := &coreauth.Auth{Provider: "workbuddy", Metadata: map[string]any{"uid": "uid-a", "enterprise_id": "team-a"}}
	for _, headers := range []http.Header{
		nil,
		{"X-Conversation-Id": {"client-session"}},
		{"X-Session-Id": {"client-session"}},
	} {
		opts := cliproxyexecutor.Options{Headers: headers}
		var requests []*http.Request
		for _, body := range [][]byte{first, step, next} {
			req := &http.Request{Header: make(http.Header)}
			ApplyWorkBuddyFingerprintHeaders(req, auth, body, opts)
			requests = append(requests, req)
		}
		for _, request := range requests {
			if request.Header.Get("X-Conversation-ID") != requests[0].Header.Get("X-Conversation-ID") {
				t.Fatal("tool continuation or next turn changed the account identity")
			}
		}
		if requests[0].Header.Get("X-Conversation-Request-ID") != requests[1].Header.Get("X-Conversation-Request-ID") {
			t.Fatal("tool continuation changed its upstream turn")
		}
		if requests[0].Header.Get("X-Conversation-Request-ID") == requests[2].Header.Get("X-Conversation-Request-ID") {
			t.Fatal("a new user turn reused the previous turn ID")
		}
	}
	const count = 64
	observed := make(chan http.Header, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Conversation-Id": {fmt.Sprintf("session-%d", i)}, "X-Conversation-Request-Id": {"same-caller-turn"}}}
			req := &http.Request{Header: make(http.Header)}
			ApplyWorkBuddyFingerprintHeaders(req, auth, first, opts)
			observed <- req.Header
		})
	}
	wg.Wait()
	close(observed)
	identities, turns, messages := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for headers := range observed {
		identities[headers.Get("X-Conversation-ID")] = true
		turns[headers.Get("X-Conversation-Request-ID")] = true
		messages[headers.Get("X-Request-ID")] = true
	}
	if len(identities) != 1 || len(turns) != count || len(messages) != count {
		t.Fatalf("concurrent requests: identities=%d turns=%d messages=%d", len(identities), len(turns), len(messages))
	}
}
