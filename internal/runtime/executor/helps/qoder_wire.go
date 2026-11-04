package helps

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const QoderChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
const qoderToolMarker = "[assistant \u8bf7\u6c42\u8c03\u7528\u5de5\u5177]"
const qoderToolResultMarker = "[\u5de5\u5177\u7ed3\u679c"

var qoderToolEchoMarker = regexp.MustCompile(`[\[<(]*\s*assistant\s*请求调用工具\s*[\]>)]?`)
var qoderDSMLClosingTag = regexp.MustCompile(`</[^>]*DSML[^>]*>\s*$`)
var qoderDSMLParameterEnd = regexp.MustCompile(`</(?:｜DSML｜|\|DSML\|)parameter\s*>`)

var qoderDSMLPrefixes = []string{"<｜DSML｜", "<|DSML|", "</｜DSML｜", "</|DSML|"}

// DoQoderGatewayRequest tries the official international gateways in upstream
// order (primary, api2, api1), at most once each. Unlike Python's broader retry
// loop, it requires a transient transport failure before any body was read or
// request/response completion was observed, preventing a paid generation replay.
// Explicit custom gateways and the CN provider are never redirected elsewhere.
func DoQoderGatewayRequest(client *http.Client, req *http.Request, auth *coreauth.Auth) (*http.Response, error) {
	hosts := []string{req.URL.Host}
	if auth != nil && auth.Provider == qoder.ProviderIntl && req.URL.Scheme == "https" && req.URL.User == nil {
		switch req.URL.Host {
		case "api3.qoder.sh", "api2.qoder.sh", "api1.qoder.sh":
			for _, host := range []string{"api2.qoder.sh", "api1.qoder.sh"} {
				if host != req.URL.Host {
					hosts = append(hosts, host)
				}
			}
		}
	}
	var lastErr error
	for index, host := range hosts {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		attempt := req.Clone(req.Context())
		attempt.URL.Host = host
		if index > 0 {
			if req.GetBody == nil {
				return nil, lastErr
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			attempt.Body = body
		}
		var sent, responding atomic.Bool
		attempt = attempt.WithContext(httptrace.WithClientTrace(attempt.Context(), &httptrace.ClientTrace{
			WroteRequest:         func(httptrace.WroteRequestInfo) { sent.Store(true) },
			GotFirstResponseByte: func() { responding.Store(true) },
		}))
		if attempt.Body != nil {
			attempt.Body = &qoderObservedBody{ReadCloser: attempt.Body, observed: &sent}
		}
		response, err := client.Do(attempt)
		if response != nil || err == nil {
			return response, err
		}
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if sent.Load() || responding.Load() || !qoderTransientTransport(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

type qoderObservedBody struct {
	io.ReadCloser
	observed *atomic.Bool
}

func (b *qoderObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.observed.Store(true)
	}
	return n, err
}

func qoderTransientTransport(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var network net.Error
	var errno syscall.Errno
	return errors.As(err, &network) || errors.As(err, &errno)
}

// BuildQoderBody converts a canonically translated OpenAI chat into the native
// agent_chat_generation schema. Encoding and signing happen only after this step.
func BuildQoderBody(chat []byte, auth *coreauth.Auth, modelKey string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(chat, &payload); err != nil || payload == nil {
		return nil, qoderProtocolError("invalid chat request", http.StatusBadRequest)
	}
	creds := qoder.CredentialsFromAuth(auth)
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, qoderProtocolError("messages must be a nonempty array", http.StatusBadRequest)
	}
	model, _ := payload["model"].(string)
	metadata, info := qoderRequestModel(auth, model, modelKey)
	displayName := qoderString(metadata["display_name"])
	if displayName == "" {
		displayName = modelKey
	}
	structured := qoderStructuredHistory(auth, []string{model, modelKey, displayName}, messages)
	deepseek := strings.Contains(strings.ToLower(model+" "+displayName), "deepseek") || modelKey == "dmodel" || modelKey == "dfmodel"
	var systems []string
	flat := make([]any, 0, len(messages)+1)
	prompt := ""
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, qoderProtocolError("invalid message object", http.StatusBadRequest)
		}
		role := qoderString(message["role"])
		content, text, _, err := qoderContent(message["content"])
		if err != nil {
			return nil, err
		}
		if role == "system" || role == "developer" {
			if text != "" {
				systems = append(systems, text)
			}
			continue
		}
		item := map[string]any{"role": role, "content": content}
		switch role {
		case "tool", "function":
			if structured && role == "tool" {
				item["tool_call_id"] = qoderString(message["tool_call_id"])
				item["content"] = text
			} else {
				name := qoderString(message["name"])
				if name != "" {
					name = " (" + name + ")"
				}
				item["role"], item["content"] = "user", qoderToolResultMarker+name+"]\n"+text
			}
		case "assistant":
			calls, _ := message["tool_calls"].([]any)
			if fc, ok := message["function_call"].(map[string]any); ok && len(calls) == 0 {
				calls = []any{map[string]any{"function": fc}}
			}
			if len(calls) > 0 {
				normalized := make([]any, 0, len(calls))
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						return nil, qoderProtocolError("invalid tool call", 400)
					}
					fn, ok := call["function"].(map[string]any)
					if !ok || qoderString(fn["name"]) == "" {
						return nil, qoderProtocolError("tool call is missing function name", 400)
					}
					args := qoderString(fn["arguments"])
					if fn["arguments"] != nil {
						if _, ok := fn["arguments"].(string); !ok {
							b, err := json.Marshal(fn["arguments"])
							if err != nil {
								return nil, err
							}
							args = string(b)
						}
					}
					function := map[string]any{"name": fn["name"], "arguments": args}
					if structured {
						normalized = append(normalized, map[string]any{"id": call["id"], "type": "function", "function": function})
					} else {
						normalized = append(normalized, function)
					}
				}
				if structured {
					item["tool_calls"] = normalized
					item["content"] = text
				} else {
					b, err := json.Marshal(normalized)
					if err != nil {
						return nil, err
					}
					item["content"] = text + "\n\n" + qoderToolMarker + "\n" + string(b)
				}
			}
			if deepseek {
				item["reasoning_content"] = qoderString(message["reasoning_content"])
				if message["reasoning_content"] == nil {
					item["reasoning_content"] = qoderString(message["reasoning"])
				}
			}
		case "user":
			if text != "" {
				prompt = text
			}
		default:
			return nil, qoderProtocolError("unsupported message role", 400)
		}
		flat = append(flat, item)
	}
	if len(systems) == 0 {
		systems = []string{"You are a helpful assistant."}
	}
	flat = append([]any{map[string]any{"role": "system", "content": strings.Join(systems, "\n\n")}}, flat...)
	ids := make([]string, 4)
	for i := range ids {
		var err error
		ids[i], err = qoderUUID()
		if err != nil {
			return nil, err
		}
	}
	userType := creds.UserType
	if userType == "" {
		userType = qoderDefaultUserType
	}
	// Only the fields selected by upstream build_qoder_body belong on the wire.
	// In particular, a context_config maximum is not the active input limit and
	// thinking_config can coexist with an explicit is_reasoning=false.
	vision, _ := metadata["is_vl"].(bool)
	reasoning, _ := metadata["is_reasoning"].(bool)
	inputLimit := qoderInt(metadata["max_input_tokens"])
	if inputLimit <= 0 {
		inputLimit = 180000
	}
	mc := map[string]any{"key": modelKey, "display_name": displayName, "model": "", "format": "openai", "is_vl": vision, "is_reasoning": reasoning, "api_key": "", "url": "", "source": "system", "max_input_tokens": inputLimit}
	params := map[string]any{"max_tokens": 32768}
	for _, key := range []string{"max_tokens", "temperature", "top_p", "stop", "reasoning_effort", "parallel_tool_calls", "tool_choice"} {
		if value, ok := payload[key]; ok {
			params[key] = value
		}
	}
	if value, ok := payload["max_completion_tokens"]; ok && payload["max_tokens"] == nil {
		params["max_tokens"] = value
	}
	if info != nil && info.MaxCompletionTokens > 0 && qoderInt(params["max_tokens"]) > info.MaxCompletionTokens {
		params["max_tokens"] = info.MaxCompletionTokens
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) == 0 {
		if functions, ok := payload["functions"].([]any); ok {
			for _, function := range functions {
				tools = append(tools, map[string]any{"type": "function", "function": function})
			}
		}
	}
	if tools == nil {
		tools = []any{}
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || tool["type"] != "function" {
			return nil, qoderProtocolError("only function tools are supported", 400)
		}
		if tool["function"] == nil {
			fn := map[string]any{}
			for _, key := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := tool[key]; ok {
					fn[key] = v
					delete(tool, key)
				}
			}
			tool["function"] = fn
		}
	}
	toolChoice := payload["tool_choice"]
	if toolChoice == nil {
		toolChoice = payload["function_call"]
	}
	if choice, ok := toolChoice.(map[string]any); ok {
		switch choice["type"] {
		case "auto", "none", "required":
			toolChoice = choice["type"]
		case "function":
			function, _ := choice["function"].(map[string]any)
			toolChoice = qoderString(function["name"])
			if toolChoice == "" {
				toolChoice = qoderString(choice["name"])
			}
		default:
			toolChoice = qoderString(choice["name"])
		}
	}
	if choice, ok := toolChoice.(string); ok && choice != "" {
		params["tool_choice"] = choice
	} else {
		delete(params, "tool_choice")
	}
	name := []rune(prompt)
	if len(name) > 30 {
		name = name[:30]
	}
	if len(name) == 0 {
		name = []rune("chat")
	}
	body := map[string]any{
		"request_id": ids[0], "chat_record_id": ids[0], "request_set_id": ids[1], "session_id": ids[2],
		"stream": true, "chat_task": "FREE_INPUT", "image_urls": nil, "is_reply": true, "is_retry": false,
		"code_language": "", "source": 1, "version": "3", "chat_prompt": "", "parameters": params,
		"aliyun_user_type": userType, "session_type": "qodercli", "agent_id": "agent_common", "task_id": "common",
		"model_config": mc, "messages": flat, "tools": tools,
		"chat_context": map[string]any{"chatPrompt": "", "features": []any{}, "imageUrls": nil, "text": map[string]any{"type": "text", "text": prompt}, "extra": map[string]any{"context": []any{}, "modelConfig": mc, "originalContent": map[string]any{"type": "text", "text": prompt}}},
		"business":     map[string]any{"id": ids[3], "begin_at": time.Now().UnixMilli(), "name": string(name), "product": "cli", "type": "agent", "stage": "start"},
	}
	if choice, ok := params["tool_choice"]; ok {
		body["tool_choice"] = choice
	}
	return json.Marshal(body)
}

func qoderRequestModel(auth *coreauth.Auth, model, key string) (map[string]any, *registry.ModelInfo) {
	if auth == nil {
		return nil, nil
	}
	clientID := auth.ID
	if clientID == "" {
		if imported := qoder.NewAuthRecord(auth.Provider, qoder.CredentialsFromAuth(auth)); imported != nil {
			clientID = imported.ID
		}
	}
	models, metadata, found := registry.GetQoderCatalogForClient(clientID, auth.Provider)
	if found {
		var info *registry.ModelInfo
		for _, candidate := range models {
			if candidate != nil && candidate.MetadataModelID == key {
				info = candidate
				break
			}
		}
		for _, entry := range metadata {
			if qoderString(entry["key"]) == key {
				return entry, info
			}
		}
		// A successfully fetched empty/replaced catalog is authoritative. A stale
		// registration or another account must not restore removed metadata.
		return nil, nil
	}
	models = registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	models = append(models, registry.GetQoderModels(auth.Provider)...)
	for _, info := range models {
		if info == nil || info.Type != auth.Provider || (info.MetadataModelID != key && info.ID != model && info.ID != key) {
			continue
		}
		vision := false
		for _, modality := range info.SupportedInputModalities {
			if strings.EqualFold(modality, "image") {
				vision = true
			}
		}
		return map[string]any{"display_name": info.DisplayName, "max_input_tokens": info.InputTokenLimit, "is_vl": vision, "is_reasoning": info.Thinking != nil}, info
	}
	return nil, nil
}

func qoderStructuredHistory(auth *coreauth.Auth, names []string, messages []any) bool {
	if auth == nil || auth.Provider != qoder.ProviderCN {
		return false
	}
	allowed := false
	for _, candidate := range names {
		for _, prefix := range []string{"qwen", "glm", "gm4", "gm5", "deepseek", "dfmodel"} {
			if strings.HasPrefix(strings.ToLower(candidate), prefix) {
				allowed = true
			}
		}
	}
	if !allowed {
		return false
	}
	for _, raw := range messages {
		m, _ := raw.(map[string]any)
		if m["role"] == "tool" && strings.TrimSpace(qoderString(m["tool_call_id"])) == "" {
			return false
		}
		if m["function_call"] != nil {
			return false
		}
		calls, _ := m["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			if strings.TrimSpace(qoderString(call["id"])) == "" {
				return false
			}
		}
	}
	return true
}

func qoderContent(value any) (any, string, bool, error) {
	if value == nil {
		return "", "", false, nil
	}
	if text, ok := value.(string); ok {
		return text, text, false, nil
	}
	parts, ok := value.([]any)
	if !ok {
		if part, yes := value.(map[string]any); yes {
			parts = []any{part}
		} else {
			return nil, "", false, qoderProtocolError("invalid message content", 400)
		}
	}
	var texts []string
	var normalized []any
	image := false
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			if text, yes := raw.(string); yes {
				part = map[string]any{"type": "text", "text": text}
			} else {
				return nil, "", false, qoderProtocolError("invalid content part", 400)
			}
		}
		switch part["type"] {
		case "text", "input_text", "output_text", "summary_text":
			text := qoderString(part["text"])
			texts = append(texts, text)
			normalized = append(normalized, map[string]any{"type": "text", "text": text})
		case "image_url", "input_image", "image":
			url := qoderString(part["image_url"])
			if nested, ok := part["image_url"].(map[string]any); ok {
				url = qoderString(nested["url"])
			}
			if url == "" {
				url = qoderString(part["url"])
			}
			if url == "" && part["data"] != nil {
				mime := qoderString(part["mime_type"])
				if mime == "" {
					mime = qoderString(part["mimeType"])
				}
				if mime == "" {
					mime = "image/png"
				}
				url = "data:" + mime + ";base64," + qoderString(part["data"])
			}
			if url == "" {
				return nil, "", false, qoderProtocolError("image content is missing its URL", 400)
			}
			normalized = append(normalized, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			image = true
		default:
			return nil, "", false, qoderProtocolError("unsupported content part", 400)
		}
	}
	text := strings.Join(texts, "\n")
	if image {
		return normalized, text, true, nil
	}
	return text, text, false, nil
}

func qoderString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	if n, ok := v.(float64); ok {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}
func qoderInt(v any) int {
	n, _ := strconv.Atoi(qoderString(v))
	if i, ok := v.(int); ok {
		return i
	}
	return n
}

// QoderError exposes scheduler scope without treating queue pressure as expired auth.
type QoderError struct {
	status                          int
	code, message                   string
	retryAfter                      *time.Duration
	credentialScoped, requestScoped bool
}

func (e *QoderError) Error() string              { return e.message }
func (e *QoderError) StatusCode() int            { return e.status }
func (e *QoderError) ResultCode() string         { return e.code }
func (e *QoderError) RetryAfter() *time.Duration { return e.retryAfter }
func (e *QoderError) IsCredentialScoped() bool   { return e.credentialScoped }
func (e *QoderError) IsRequestScoped() bool      { return e.requestScoped }

func qoderProtocolError(message string, status int) *QoderError {
	return &QoderError{status: status, message: "qoder: " + message, requestScoped: status == 400 || status == 413}
}

type qoderErrorDetails struct {
	code, message  string
	status, retry  int
	queued, failed bool
}

func (d *qoderErrorDetails) inspect(value any, depth int) {
	if depth > 8 {
		return
	}
	if s, ok := value.(string); ok {
		var nested any
		if json.Unmarshal([]byte(s), &nested) == nil {
			d.inspect(nested, depth+1)
		}
		return
	}
	m, ok := value.(map[string]any)
	if !ok {
		return
	}
	if status := qoderInt(m["statusCodeValue"]); status >= 400 && status <= 599 {
		d.status = status
		d.failed = true
	}
	if status := qoderInt(m["status"]); status >= 400 && status <= 599 {
		d.status = status
		d.failed = true
	}
	if code := qoderString(m["code"]); code != "" && code != "0" && code != "200" && !strings.EqualFold(code, "ok") && !strings.EqualFold(code, "success") {
		d.code = code
		d.failed = true
	}
	if m["success"] == false {
		d.failed = true
	}
	if m["isQueued"] == true {
		d.queued = true
		d.failed = true
	}
	for _, key := range []string{"retryAfterSeconds", "retry_after"} {
		if seconds := qoderInt(m[key]); seconds > 0 {
			d.retry = seconds
		}
	}
	for _, key := range []string{"msg", "message"} {
		if message, ok := m[key].(string); ok && message != "" {
			d.message = message
			d.inspect(message, depth+1)
		}
	}
	if value := m["error"]; value != nil && value != false && value != "" {
		d.failed = true
		if errObj, ok := value.(map[string]any); ok && errObj["code"] == nil {
			if kind := qoderString(errObj["type"]); kind != "" {
				d.code = kind
			}
		}
		d.inspect(value, depth+1)
	}
	for _, key := range []string{"body", "data"} {
		d.inspect(m[key], depth+1)
	}
}

// QoderUpstreamError unwraps HTTP and HTTP-200 business failures. It never includes
// the outer envelope headers (which may echo the encrypted Cosy credentials).
func QoderUpstreamError(status int, headers http.Header, payload []byte) *QoderError {
	d := qoderErrorDetails{}
	var value any
	if json.Unmarshal(payload, &value) == nil {
		d.inspect(value, 0)
	}
	if d.status != 0 {
		status = d.status
	}
	if status < 400 || status > 599 {
		status = 502
	}
	err := &QoderError{status: status, message: fmt.Sprintf("qoder: upstream request failed (HTTP %d)", status)}
	if d.code != "" {
		err.message = "qoder: upstream error " + d.code
	}
	switch {
	case d.code == "10605" || d.queued:
		err.status, err.code, err.requestScoped = 429, coreauth.ErrorCodeRequestScoped, true
		err.message = "qoder: upstream model is queued (10605)"
		if d.retry == 0 {
			d.retry = 30
		}
	case d.code == "112":
		err.status, err.code, err.credentialScoped = 429, "usage_limit_reached", true
		err.message = "qoder: account quota exhausted (112)"
	case d.code == "110":
		err.status, err.code = 429, "qoder_model_rate_limit"
		err.message = "qoder: model daily count exceeded (110)"
	case d.code == "401" || d.code == "unauthorized" || d.code == "authentication_error":
		err.status = 401
	case d.code == "429" || d.code == "rate_limit_error":
		err.status = 429
	case d.code == "invalid_request_error" || d.code == "invalid_parameter_error":
		err.status, err.requestScoped = 400, true
	}
	// Only a concise leaf message is retained. JSON messages may contain request
	// headers/tokens, so the structured code above is the useful safe diagnostic.
	if d.message != "" && !strings.ContainsAny(d.message, "{}") && !strings.Contains(strings.ToLower(d.message), "cosy") && !strings.Contains(strings.ToLower(d.message), "token") {
		message := []rune(d.message)
		if len(message) > 300 {
			message = message[:300]
		}
		err.message += "; " + string(message)
	}
	if d.retry > 0 && d.retry <= 86400 {
		delay := time.Duration(d.retry) * time.Second
		err.retryAfter = &delay
	} else if raw := headers.Get("Retry-After"); raw != "" {
		if seconds, errParse := strconv.Atoi(raw); errParse == nil && seconds >= 0 && seconds <= 86400 {
			delay := time.Duration(seconds) * time.Second
			err.retryAfter = &delay
		} else if date, errParse := http.ParseTime(raw); errParse == nil {
			delay := time.Until(date)
			if delay < 0 {
				delay = 0
			}
			err.retryAfter = &delay
		}
	}
	return err
}

type qoderChoiceState struct {
	finish    string
	tools     map[int]string
	arguments map[int]*strings.Builder
}
type qoderStreamState struct {
	model, id string
	created   int64
	choices   map[int]*qoderChoiceState
	usage     map[string]any
	done      bool
}

func (s *qoderStreamState) frame(choices []any, usage bool) []byte {
	m := map[string]any{"id": s.id, "created": s.created, "model": s.model, "object": "chat.completion.chunk", "choices": choices}
	if usage && s.usage != nil {
		m["usage"] = s.usage
	}
	b, _ := json.Marshal(m)
	return b
}

func (s *qoderStreamState) inner(payload []byte, consume func([]byte, bool) error) error {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil || m == nil {
		return qoderProtocolError("malformed inner stream JSON", 502)
	}
	d := qoderErrorDetails{}
	d.inspect(m, 0)
	if d.failed {
		return QoderUpstreamError(200, nil, payload)
	}
	if id := qoderString(m["id"]); id != "" {
		s.id = id
	}
	if created := qoderInt(m["created"]); created > 0 {
		s.created = int64(created)
	}
	if usage, ok := m["usage"].(map[string]any); ok {
		if s.usage == nil {
			s.usage = map[string]any{}
		}
		qoderMergeUsage(s.usage, usage)
		if err := consume(s.frame([]any{}, true), false); err != nil {
			return err
		}
	}
	rawChoices, ok := m["choices"].([]any)
	if !ok && m["usage"] == nil {
		return qoderProtocolError("inner stream has neither choices nor usage", 502)
	}
	choices := make([]any, 0, len(rawChoices))
	for _, raw := range rawChoices {
		choice, ok := raw.(map[string]any)
		if !ok {
			return qoderProtocolError("invalid stream choice", 502)
		}
		index := qoderInt(choice["index"])
		state := s.choices[index]
		if state == nil {
			state = &qoderChoiceState{tools: map[int]string{}, arguments: map[int]*strings.Builder{}}
			s.choices[index] = state
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta, _ = choice["message"].(map[string]any)
		}
		if delta == nil {
			delta = map[string]any{}
		}
		for _, key := range []string{"extra_fields", "refusal", "function_call", "tool_calls", "reasoning_content", "content", "role"} {
			if value, ok := delta[key]; ok && (value == nil || value == "") {
				delete(delta, key)
			}
		}
		if parts, ok := delta["content"].([]any); ok {
			var text strings.Builder
			for _, rawPart := range parts {
				if part, ok := rawPart.(map[string]any); ok {
					text.WriteString(qoderString(part["text"]))
				} else {
					text.WriteString(qoderString(rawPart))
				}
			}
			delta["content"] = text.String()
		}
		if reasoning := qoderString(delta["reasoning"]); reasoning != "" && delta["reasoning_content"] == nil {
			delta["reasoning_content"] = reasoning
		}
		delete(delta, "reasoning")
		calls, _ := delta["tool_calls"].([]any)
		if fc, ok := delta["function_call"].(map[string]any); ok && (fc["name"] != nil || fc["arguments"] != nil) && len(calls) == 0 {
			calls = []any{map[string]any{"index": 0, "function": fc}}
		}
		delete(delta, "function_call")
		cleanCalls := make([]any, 0, len(calls))
		for position, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				return qoderProtocolError("invalid tool delta", 502)
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			if qoderString(fn["name"]) == "" && qoderString(fn["arguments"]) == "" && qoderString(call["id"]) == "" {
				continue
			}
			toolIndex := qoderInt(call["index"])
			if call["index"] == nil {
				toolIndex = position
			}
			call["index"] = toolIndex
			if _, exists := state.tools[toolIndex]; !exists {
				if qoderString(call["id"]) == "" {
					id, err := qoderUUID()
					if err != nil {
						return err
					}
					call["id"] = "call_" + strings.ReplaceAll(id, "-", "")
				}
				call["type"] = "function"
				state.arguments[toolIndex] = &strings.Builder{}
			}
			state.tools[toolIndex] += qoderString(fn["name"])
			if value, exists := fn["arguments"]; exists && value != nil {
				arguments, ok := value.(string)
				if !ok {
					return qoderProtocolError("tool delta arguments must be a string", 502)
				}
				state.arguments[toolIndex].WriteString(arguments)
				if state.arguments[toolIndex].Len() > 50<<20 {
					return qoderProtocolError("tool arguments exceed stream buffer limit", 502)
				}
			}
			cleanCalls = append(cleanCalls, call)
		}
		if len(cleanCalls) > 0 {
			delta["tool_calls"] = cleanCalls
		} else {
			delete(delta, "tool_calls")
		}
		if state.finish != "" && len(delta) > 0 {
			return qoderProtocolError("content after terminal choice", 502)
		}
		finish := qoderString(choice["finish_reason"])
		if finish != "" {
			if state.finish != "" && state.finish != finish {
				return qoderProtocolError("conflicting finish reasons", 502)
			}
			state.finish = finish
		}
		// Shared translators may return immediately after role, text or reasoning.
		// Keep tool fragments in their own frame so a mixed delta loses no fields.
		for _, key := range []string{"role", "reasoning_content", "content", "tool_calls"} {
			if value, exists := delta[key]; exists {
				choices = append(choices, map[string]any{"index": index, "delta": map[string]any{key: value}, "finish_reason": nil})
				delete(delta, key)
			}
		}
		if len(delta) > 0 {
			choices = append(choices, map[string]any{"index": index, "delta": delta, "finish_reason": nil})
		}
	}
	for _, choice := range choices {
		if err := consume(s.frame([]any{choice}, false), false); err != nil {
			return err
		}
	}
	return nil
}

func qoderMergeUsage(dst, src map[string]any) {
	qoderMergeObject(dst, src)
	details, _ := dst["prompt_tokens_details"].(map[string]any)
	completion, _ := dst["completion_tokens_details"].(map[string]any)
	for _, value := range []any{dst["prompt_cache_hit_tokens"], completion["cached_tokens"], details["cached_tokens"]} {
		if n := qoderInt(value); n > 0 {
			if details == nil {
				details = map[string]any{}
			}
			details["cached_tokens"] = n
			dst["prompt_tokens_details"] = details
			break
		}
	}
	if dst["prompt_tokens"] == nil && dst["input_tokens"] != nil {
		dst["prompt_tokens"] = dst["input_tokens"]
	}
	if dst["completion_tokens"] == nil && dst["output_tokens"] != nil {
		dst["completion_tokens"] = dst["output_tokens"]
	}
	if src["total_tokens"] == nil {
		dst["total_tokens"] = qoderInt(dst["prompt_tokens"]) + qoderInt(dst["completion_tokens"])
	}
}

func qoderMergeObject(dst, src map[string]any) {
	for key, value := range src {
		if nested, ok := value.(map[string]any); ok {
			target, _ := dst[key].(map[string]any)
			if target == nil {
				target = map[string]any{}
				dst[key] = target
			}
			qoderMergeObject(target, nested)
		} else {
			dst[key] = value
		}
	}
}

func (s *qoderStreamState) finish(consume func([]byte, bool) error) error {
	if !s.done {
		if len(s.choices) == 0 {
			return qoderProtocolError("upstream stream closed without completion", 502)
		}
		for _, choice := range s.choices {
			if choice.finish == "" {
				return qoderProtocolError("upstream stream closed before a terminal frame", 502)
			}
		}
	}
	indices := make([]int, 0, len(s.choices))
	for index := range s.choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	terminal := make([]any, 0, len(indices))
	for _, index := range indices {
		choice := s.choices[index]
		finish := choice.finish
		if finish == "" {
			finish = "stop"
		}
		if len(choice.tools) > 0 {
			for toolIndex, name := range choice.tools {
				if name == "" {
					return qoderProtocolError("incomplete tool call name", 502)
				}
				arguments := choice.arguments[toolIndex].String()
				if arguments == "" {
					call := map[string]any{"index": toolIndex, "function": map[string]any{"arguments": "{}"}}
					if err := consume(s.frame([]any{map[string]any{"index": index, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": nil}}, false), false); err != nil {
						return err
					}
				} else if finish != "length" && finish != "content_filter" && !json.Valid([]byte(arguments)) {
					return qoderProtocolError("incomplete or invalid tool call arguments", 502)
				}
			}
			if finish == "stop" {
				finish = "tool_calls"
			}
		}
		terminal = append(terminal, map[string]any{"index": index, "delta": map[string]any{}, "finish_reason": finish})
	}
	if len(terminal) == 0 {
		terminal = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
	}
	// Usage has already been emitted before finish; include its final snapshot for
	// clients that only inspect the terminal chunk.
	if err := consume(s.frame(terminal, true), false); err != nil {
		return err
	}
	return consume([]byte("[DONE]"), true)
}

// ReadQoderStream owns body, closes it on return/cancellation, and unwraps the
// outer Cosy SSE envelope before delivering normalized OpenAI chunks.
func ReadQoderStream(ctx context.Context, body io.ReadCloser, model string, headers http.Header, consume func([]byte, bool) error) error {
	var closeOnce sync.Once
	closeBody := func() {
		closeOnce.Do(func() {
			if err := body.Close(); err != nil {
				log.WithError(err).Debug("qoder: close upstream response")
			}
		})
	}
	stop := context.AfterFunc(ctx, closeBody)
	defer stop()
	defer closeBody()
	state := qoderStreamState{model: model, id: "chatcmpl-qoder", created: time.Now().Unix(), choices: map[int]*qoderChoiceState{}}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 50<<20)
	var data []byte
	event := ""
	process := func() error {
		payload := bytes.TrimSpace(data)
		data = nil
		currentEvent := event
		event = ""
		if len(payload) == 0 {
			if currentEvent == "error" {
				return qoderProtocolError("empty upstream error event", 502)
			}
			return nil
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			state.done = true
			return nil
		}
		var outer map[string]any
		if err := json.Unmarshal(payload, &outer); err != nil || outer == nil {
			return qoderProtocolError("malformed upstream SSE envelope", 502)
		}
		d := qoderErrorDetails{}
		d.inspect(outer, 0)
		status := qoderString(outer["statusCodeValue"])
		if d.failed || currentEvent == "error" || (status != "" && status != "200") {
			return QoderUpstreamError(200, headers, payload)
		}
		inner, exists := outer["body"]
		if !exists {
			if strings.HasPrefix(currentEvent, "finish") {
				return nil
			}
			return qoderProtocolError("upstream SSE envelope is missing body", 502)
		}
		var decoded []byte
		switch value := inner.(type) {
		case string:
			decoded = []byte(value)
		case map[string]any:
			decoded, _ = json.Marshal(value)
		default:
			return qoderProtocolError("invalid upstream SSE body", 502)
		}
		decoded = bytes.TrimSpace(decoded)
		if bytes.Equal(decoded, []byte("[DONE]")) {
			state.done = true
			return nil
		}
		return state.inner(decoded, consume)
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := process(); err != nil {
				return err
			}
			if state.done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			if len(data) > 0 {
				if err := process(); err != nil {
					return err
				}
				if state.done {
					break
				}
			}
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			// Some gateway versions omit the blank separator between complete JSON
			// envelopes. A complete previous data value is already an unambiguous event.
			if len(data) > 0 && (json.Valid(data) || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]"))) {
				if err := process(); err != nil {
					return err
				}
				if state.done {
					break
				}
			}
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			payload := []byte(line)
			for !json.Valid(payload) && len(payload) < 2<<20 && scanner.Scan() {
				payload = append(payload, '\n')
				payload = append(payload, scanner.Bytes()...)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return QoderUpstreamError(200, headers, payload)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("qoder: read upstream stream: %w", err)
	}
	if len(data) > 0 || event == "error" {
		if err := process(); err != nil {
			return err
		}
	}
	return state.finish(consume)
}

type qoderToolEchoState struct {
	pending               string
	holding, dsml, native bool
	text                  qoderDSMLTextState
	tools                 map[int]*qoderToolNameState
}

type qoderToolNameState struct {
	name, id  string
	arguments strings.Builder
	started   bool
}

// QoderToolEchoConsumer restores DSML and legacy history envelopes only for
// declared function names. Ordinary text streams with at most a marker prefix
// held back. Recovery waits for the terminal choice so later native calls win.
func QoderToolEchoConsumer(chat []byte, consume func([]byte, bool) error) func([]byte, bool) error {
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(chat, &request)
	allowed := make(map[string]bool)
	for _, tool := range request.Tools {
		if tool.Function.Name != "" {
			allowed[tool.Function.Name] = true
		}
	}
	if len(allowed) == 0 {
		return consume
	}
	states := make(map[int]*qoderToolEchoState)
	return func(payload []byte, done bool) error {
		if done {
			return consume(payload, true)
		}
		var frame map[string]any
		if err := json.Unmarshal(payload, &frame); err != nil {
			return err
		}
		choices, _ := frame["choices"].([]any)
		emit := func(index int, delta map[string]any) error {
			copy := make(map[string]any, len(frame))
			for key, value := range frame {
				if key != "usage" {
					copy[key] = value
				}
			}
			copy["choices"] = []any{map[string]any{"index": index, "delta": delta, "finish_reason": nil}}
			out, err := json.Marshal(copy)
			if err != nil {
				return err
			}
			return consume(out, false)
		}
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			index := qoderInt(choice["index"])
			state := states[index]
			if state == nil {
				state = &qoderToolEchoState{tools: make(map[int]*qoderToolNameState)}
				states[index] = state
			}
			delta, _ := choice["delta"].(map[string]any)
			if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
				state.native = true
				if state.pending != "" {
					if err := emit(index, map[string]any{"content": state.pending}); err != nil {
						return err
					}
					state.pending = ""
				}
				ready := make([]any, 0, len(calls))
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					function, _ := call["function"].(map[string]any)
					toolIndex := qoderInt(call["index"])
					tool := state.tools[toolIndex]
					if tool == nil {
						tool = &qoderToolNameState{}
						state.tools[toolIndex] = tool
					}
					if id := qoderString(call["id"]); id != "" {
						tool.id = id
					}
					name := qoderString(function["name"])
					if tool.started {
						if name != "" {
							return qoderProtocolError("tool name changed after its start", 502)
						}
						ready = append(ready, call)
						continue
					}
					tool.name += name
					tool.arguments.WriteString(qoderString(function["arguments"]))
					if len(tool.name) > 1024 || tool.arguments.Len() > 50<<20 {
						return qoderProtocolError("tool metadata exceeds stream buffer limit", 502)
					}
					// Names can arrive in fragments. Claude fixes the name at block
					// start, so wait until a complete declared name is unambiguous.
					ambiguous := false
					for candidate := range allowed {
						if candidate != tool.name && strings.HasPrefix(candidate, tool.name) {
							ambiguous = true
							break
						}
					}
					if !allowed[tool.name] || (ambiguous && tool.arguments.Len() == 0) {
						continue
					}
					ready = append(ready, map[string]any{"index": toolIndex, "id": tool.id, "type": "function", "function": map[string]any{"name": tool.name, "arguments": tool.arguments.String()}})
					tool.arguments.Reset()
					tool.started = true
				}
				if len(ready) > 0 {
					delta["tool_calls"] = ready
				} else {
					delete(delta, "tool_calls")
				}
			}
			if text, ok := delta["content"].(string); ok && !state.native {
				state.pending += text
				if len(state.pending) > 4<<20 {
					return qoderProtocolError("tool envelope exceeds stream buffer limit", 502)
				}
				if !state.holding {
					span := qoderToolEchoMarker.FindStringIndex(state.pending)
					dsml := qoderDSMLStart(state.pending, state.text)
					if dsml >= 0 && (span == nil || dsml < span[0]) {
						delta["content"] = state.pending[:dsml]
						state.pending, state.holding, state.dsml = state.pending[dsml:], true, true
					} else if span != nil {
						head := strings.TrimSuffix(state.pending[:span[0]], "```json\n")
						delta["content"] = head
						state.pending, state.holding = state.pending[span[0]:], true
					} else {
						keep := qoderMarkerPrefixLength(state.pending)
						delta["content"] = state.pending[:len(state.pending)-keep]
						state.pending = state.pending[len(state.pending)-keep:]
					}
					for _, b := range []byte(qoderString(delta["content"])) {
						state.text.advance(b)
					}
				} else {
					delete(delta, "content")
				}
				if delta["content"] == "" {
					delete(delta, "content")
				}
			}
			if qoderString(choice["finish_reason"]) == "" {
				continue
			}
			// A terminal input may carry content as well. Emit it before buffered
			// suffixes or tools, keeping translators' content/tool branches separate.
			if len(delta) > 0 {
				if err := emit(index, delta); err != nil {
					return err
				}
				choice["delta"] = map[string]any{}
			}
			for toolIndex, tool := range state.tools {
				if tool.started {
					continue
				}
				if !allowed[tool.name] {
					return qoderProtocolError("tool call has an incomplete or undeclared name", 502)
				}
				call := map[string]any{"index": toolIndex, "id": tool.id, "type": "function", "function": map[string]any{"name": tool.name, "arguments": tool.arguments.String()}}
				if err := emit(index, map[string]any{"tool_calls": []any{call}}); err != nil {
					return err
				}
				tool.started = true
			}
			if state.holding && !state.native {
				var calls []any
				var clean string
				var err error
				if state.dsml {
					calls, clean, err = qoderRecoverDSML(state.pending, allowed)
				} else {
					calls, err = qoderRecoverToolEcho(state.pending, allowed)
				}
				if err != nil {
					return err
				}
				if clean != "" {
					if err = emit(index, map[string]any{"content": clean}); err != nil {
						return err
					}
				}
				for _, call := range calls {
					if err = emit(index, map[string]any{"tool_calls": []any{call}}); err != nil {
						return err
					}
				}
				if len(calls) > 0 {
					choice["finish_reason"] = "tool_calls"
				}
			} else if state.pending != "" && !state.holding {
				if err := emit(index, map[string]any{"content": state.pending}); err != nil {
					return err
				}
			}
			state.pending = ""
		}
		filtered := make([]any, 0, len(choices))
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if len(delta) > 0 || qoderString(choice["finish_reason"]) != "" {
				filtered = append(filtered, choice)
			}
		}
		if len(filtered) == 0 && frame["usage"] == nil {
			return nil
		}
		frame["choices"] = filtered
		out, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		return consume(out, false)
	}
}

func qoderMarkerPrefixLength(text string) int {
	// Hold only a suffix that could become one of the upstream marker spellings.
	for i := max(0, len(text)-96); i < len(text); i++ {
		for _, prefix := range qoderDSMLPrefixes {
			if strings.HasPrefix(prefix, text[i:]) {
				return len(text) - i
			}
		}
		if !strings.ContainsRune("[<(", rune(text[i])) {
			continue
		}
		suffix := strings.TrimLeft(text[i:], "[<( \t\r\n")
		key := strings.ReplaceAll(suffix, " ", "")
		if strings.HasPrefix("assistant请求调用工具", key) {
			return len(text) - i
		}
	}
	return 0
}

// Track Markdown code delimiters across content frames without buffering prose.
// Quoted DSML examples are text, even when their function names are declared.
type qoderDSMLTextState struct {
	marker, run  byte
	width, count int
}

func (s *qoderDSMLTextState) advance(b byte) bool {
	if s.count > 0 && b != s.run {
		if s.marker == 0 && (s.run == '`' || s.count >= 3) {
			s.marker, s.width = s.run, s.count
		} else if s.marker == s.run && (s.count == s.width || s.width >= 3 && s.count >= s.width) {
			s.marker, s.width = 0, 0
		}
		s.count = 0
	}
	if b == '`' || b == '~' {
		s.run, s.count = b, s.count+1
		return false
	}
	return s.marker == 0
}

func qoderDSMLStart(text string, state qoderDSMLTextState) int {
	for i := 0; i < len(text); i++ {
		if !state.advance(text[i]) || text[i] != '<' {
			continue
		}
		for _, prefix := range qoderDSMLPrefixes {
			if strings.HasPrefix(text[i:], prefix) {
				return i
			}
		}
	}
	return -1
}

type qoderDSMLReader struct {
	text string
	pos  int
}

// Only tag names are normalized for encoding/xml. Parameter bodies stay opaque:
// XML entities, CRLF, angle brackets and DSML-looking literals are argument data.
func (r *qoderDSMLReader) tag() (xml.Token, bool) {
	for r.pos < len(r.text) && strings.ContainsRune(" \t\r\n", rune(r.text[r.pos])) {
		r.pos++
	}
	start := r.pos
	if start >= len(r.text) || r.text[start] != '<' {
		return nil, false
	}
	name := start + 1
	closing := ""
	if name < len(r.text) && r.text[name] == '/' {
		closing = "/"
		name++
	}
	switch {
	case strings.HasPrefix(r.text[name:], "｜DSML｜"):
		name += len("｜DSML｜")
	case strings.HasPrefix(r.text[name:], "|DSML|"):
		name += len("|DSML|")
	default:
		return nil, false
	}
	var quote byte
	for end := name; end < len(r.text); end++ {
		b := r.text[end]
		if quote != 0 {
			if b == quote {
				quote = 0
			}
			continue
		}
		if b == '\'' || b == '"' {
			quote = b
			continue
		}
		if b != '>' {
			continue
		}
		decoder := xml.NewDecoder(strings.NewReader("<" + closing + r.text[name:end+1]))
		token, err := decoder.RawToken()
		if err != nil {
			return nil, false
		}
		if _, err = decoder.RawToken(); err != io.EOF {
			return nil, false
		}
		r.pos = end + 1
		return token, true
	}
	return nil, false
}

func qoderDSMLAttributes(tag xml.StartElement, names ...string) (map[string]string, bool) {
	if tag.Name.Space != "" {
		return nil, false
	}
	attrs := make(map[string]string, len(tag.Attr))
	for _, attr := range tag.Attr {
		known := false
		for _, name := range names {
			known = known || attr.Name.Local == name
		}
		if _, duplicate := attrs[attr.Name.Local]; duplicate || !known || attr.Name.Space != "" {
			return nil, false
		}
		attrs[attr.Name.Local] = attr.Value
	}
	return attrs, true
}

func (r *qoderDSMLReader) invoke(tag xml.StartElement, allowed map[string]bool) (map[string]any, bool) {
	attrs, ok := qoderDSMLAttributes(tag, "name")
	if !ok || tag.Name.Local != "invoke" || !allowed[attrs["name"]] {
		return nil, false
	}
	params := make(map[string]any)
	for {
		token, ok := r.tag()
		if !ok {
			return nil, false
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name != tag.Name {
				return nil, false
			}
			encoded, err := json.Marshal(params)
			return map[string]any{"name": attrs["name"], "arguments": string(encoded)}, err == nil
		}
		parameter, ok := token.(xml.StartElement)
		if !ok || parameter.Name.Local != "parameter" {
			return nil, false
		}
		fields, ok := qoderDSMLAttributes(parameter, "name", "string")
		if !ok || fields["name"] == "" {
			return nil, false
		}
		if _, duplicate := params[fields["name"]]; duplicate {
			return nil, false
		}
		span := qoderDSMLParameterEnd.FindStringIndex(r.text[r.pos:])
		if span == nil {
			return nil, false
		}
		value := r.text[r.pos : r.pos+span[0]]
		r.pos += span[1]
		switch fields["string"] {
		case "", "true":
			params[fields["name"]] = value
		case "false":
			if !json.Valid([]byte(value)) {
				return nil, false
			}
			params[fields["name"]] = json.RawMessage(value)
		default:
			return nil, false
		}
	}
}

func (r *qoderDSMLReader) block(allowed map[string]bool) ([]map[string]any, bool) {
	token, ok := r.tag()
	if !ok {
		return nil, false
	}
	root, ok := token.(xml.StartElement)
	if !ok {
		return nil, false
	}
	if root.Name.Local == "invoke" {
		call, ok := r.invoke(root, allowed)
		return []map[string]any{call}, ok
	}
	if _, ok := qoderDSMLAttributes(root); !ok || (root.Name.Local != "function_calls" && root.Name.Local != "calls") {
		return nil, false
	}
	var calls []map[string]any
	for {
		token, ok := r.tag()
		if !ok {
			return nil, false
		}
		if end, ok := token.(xml.EndElement); ok {
			return calls, end.Name == root.Name && len(calls) > 0
		}
		invoke, ok := token.(xml.StartElement)
		if !ok {
			return nil, false
		}
		call, ok := r.invoke(invoke, allowed)
		if !ok {
			return nil, false
		}
		calls = append(calls, call)
	}
}

func qoderRecoverDSML(text string, allowed map[string]bool) ([]any, string, error) {
	var functions []map[string]any
	var clean strings.Builder
	for offset := 0; offset < len(text); {
		tail := text[offset:]
		start := qoderDSMLStart(tail, qoderDSMLTextState{})
		if start < 0 {
			// A later truncated tag invalidates the whole candidate, including any
			// earlier complete invokes. Never manufacture a partial tool batch.
			for _, prefix := range qoderDSMLPrefixes {
				for n := 2; n < len(prefix); n++ {
					if strings.HasSuffix(tail, prefix[:n]) {
						return nil, text, nil
					}
				}
			}
			clean.WriteString(tail)
			break
		}
		reader := qoderDSMLReader{text: tail, pos: start}
		calls, ok := reader.block(allowed)
		if !ok {
			return nil, text, nil
		}
		functions = append(functions, calls...)
		clean.WriteString(tail[:start])
		offset += reader.pos
	}
	if len(functions) == 0 {
		return nil, text, nil
	}
	calls := make([]any, 0, len(functions))
	for index, function := range functions {
		id, err := qoderUUID()
		if err != nil {
			return nil, "", err
		}
		calls = append(calls, map[string]any{"index": index, "id": "call_" + strings.ReplaceAll(id, "-", ""), "type": "function", "function": function})
	}
	return calls, clean.String(), nil
}

func qoderRecoverToolEcho(text string, allowed map[string]bool) ([]any, error) {
	span := qoderToolEchoMarker.FindStringIndex(text)
	if span == nil {
		return nil, qoderProtocolError("invalid legacy tool envelope", 502)
	}
	text = strings.TrimSpace(text[span[1]:])
	for {
		previous := text
		text = strings.TrimSpace(qoderDSMLClosingTag.ReplaceAllString(text, ""))
		text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
		if previous == text {
			break
		}
	}
	var items []map[string]any
	if json.Unmarshal([]byte(text), &items) != nil || len(items) == 0 {
		return nil, qoderProtocolError("incomplete or invalid legacy tool envelope", 502)
	}
	calls := make([]any, 0, len(items))
	for index, item := range items {
		if function, ok := item["function"].(map[string]any); ok {
			item = function
		}
		name := qoderString(item["name"])
		if !allowed[name] {
			return nil, qoderProtocolError("legacy tool envelope names an undeclared function", 502)
		}
		arguments := item["arguments"]
		if raw, ok := arguments.(string); ok {
			if strings.TrimSpace(raw) == "" {
				raw = "{}"
			}
			if json.Unmarshal([]byte(raw), &arguments) != nil {
				return nil, qoderProtocolError("legacy tool envelope has invalid JSON arguments", 502)
			}
		}
		if arguments == nil {
			arguments = map[string]any{}
		}
		if _, ok := arguments.(map[string]any); !ok {
			return nil, qoderProtocolError("legacy tool arguments must be an object", 502)
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil, err
		}
		id, err := qoderUUID()
		if err != nil {
			return nil, err
		}
		calls = append(calls, map[string]any{"index": index, "id": "call_" + strings.ReplaceAll(id, "-", ""), "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}})
	}
	return calls, nil
}
