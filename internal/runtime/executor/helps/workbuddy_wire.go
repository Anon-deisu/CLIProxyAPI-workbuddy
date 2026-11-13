package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	openaithinking "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

var workBuddyToolNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)
var workBuddyBillingHeader = regexp.MustCompile(`(?im)^x-anthropic-billing-header:[ \t]*(?:(?:cc_[a-z0-9_]+|cch)=[^;\r\n]*;[ \t]*)+(?:\r?\n)?`)
var workBuddyClaudeSignature = regexp.MustCompile(`(?i)You are Claude Code, Anthropic['\x{2019}]s official CLI for Claude`)
var workBuddyClaudeFeedback = regexp.MustCompile(`(?i)To give feedback, users should report the issue at (https://github\.com/anthropics/claude-code/issues)\b`)
var workBuddyDSHSignature = regexp.MustCompile(`(?i)^You are an AI agent powered by DeepSeek Harness`)
var workBuddyDSHGUIContext = regexp.MustCompile(`(?i)You are interacting with the user through the DeepSeek Harness Web GUI`)
var workBuddyDSHPathReference = regexp.MustCompile(`(?i)Tokens prefixed with @ are paths the user explicitly referenced\. Relative paths resolve from the workspace root`)
var workBuddyResetTime = regexp.MustCompile(`(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\s+UTC([+-])(\d{1,2})(?::(\d{2}))?`)

type workBuddyErrorDetails struct {
	Code    json.Number `json:"code"`
	Msg     string      `json:"msg"`
	Message string      `json:"message"`
}

type workBuddyErrorEnvelope struct {
	workBuddyErrorDetails
	Error struct {
		workBuddyErrorDetails
		Data workBuddyErrorDetails `json:"data"`
	} `json:"error"`
}

// WorkBuddyError retains classification and retry information from one decode.
type WorkBuddyError struct {
	Code, Message string
	retrySeconds  int64
}

// ParseWorkBuddyError preserves the gateway's nested-code and message precedence.
func ParseWorkBuddyError(payload []byte) WorkBuddyError {
	var envelope workBuddyErrorEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return WorkBuddyError{}
	}
	code := envelope.Code.String()
	message := envelope.Msg
	if message == "" {
		message = envelope.Message
	}
	for _, detail := range []workBuddyErrorDetails{envelope.Error.workBuddyErrorDetails, envelope.Error.Data} {
		if detail.Code == "" {
			continue
		}
		code = detail.Code.String()
		if detail.Msg != "" {
			message = detail.Msg
		} else if detail.Message != "" {
			message = detail.Message
		}
	}
	parsed := WorkBuddyError{Code: code, Message: message}
	if code == "6020" {
		parsed.retrySeconds = ojson.GetBytesNoCopy(payload, "data.retry_after").Int()
	}
	return parsed
}

func parseWorkBuddyError(payload []byte) (string, string, bool) {
	parsed := ParseWorkBuddyError(payload)
	return parsed.Code, parsed.Message, parsed.Code != ""
}

// Recognized reports gateway errors with provider-specific status or retry scope.
func (e WorkBuddyError) Recognized() bool {
	switch e.Code {
	case "11140", "14018", "6000", "6004", "6020":
		return true
	default:
		return false
	}
}

// WorkBuddyStreamErrorStatus handles generic OpenAI-compatible error envelopes
// after provider-specific classification, without decoding WorkBuddy codes again.
func WorkBuddyStreamErrorStatus(payload []byte, event string) int {
	if !ojson.ValidBytes(payload) {
		return 0
	}
	doc := ojson.NewDoc(payload)
	isError := func(value string) bool {
		return strings.EqualFold(value, "error") || strings.EqualFold(value, "response.error") || strings.EqualFold(value, "response.failed")
	}
	hasError := isError(event) || isError(doc.Get("type").String())
	for _, path := range []string{"error", "response.error"} {
		if value := doc.Get(path); value.Exists() && value.Type != ojson.Null {
			hasError = true
		}
	}
	if !hasError && !(doc.Get("code").Exists() && doc.Get("message").Exists()) {
		return 0
	}
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code", "response.error.status", "response.error.status_code"} {
		if status := int(doc.Get(path).Int()); status >= 400 && status <= 599 {
			return status
		}
	}
	return 502
}

// IsWorkBuddyAccountBanned reports the WorkBuddy 11140 account ban.
func IsWorkBuddyAccountBanned(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "11140"
}

// IsWorkBuddyQuotaExhausted reports the WorkBuddy 14018 account credit limit.
func IsWorkBuddyQuotaExhausted(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "14018"
}

// IsWorkBuddyRateLimit reports the WorkBuddy 6004 account usage limit.
func IsWorkBuddyRateLimit(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "6004"
}

// IsWorkBuddyModelRateLimit reports the WorkBuddy 6000 model usage limit.
func IsWorkBuddyModelRateLimit(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "6000"
}

// IsWorkBuddyQueueWaiting reports the WorkBuddy 6020 admission-queue throttle.
// The account is healthy and the upstream only asks for a short wait, so this
// must stay a request-scoped 429 instead of a credential quota cooldown.
func IsWorkBuddyQueueWaiting(payload []byte) bool {
	code, _, ok := parseWorkBuddyError(payload)
	return ok && code == "6020"
}

// WorkBuddyQueueWaitingRetryAfter returns the queue's advertised retry delay.
func WorkBuddyQueueWaitingRetryAfter(payload []byte, now time.Time) *time.Duration {
	parsed := ParseWorkBuddyError(payload)
	if parsed.Code != "6020" {
		return nil
	}
	return parsed.RetryAfter(now)
}

// WorkBuddyRateLimitRetryAfter returns the advertised reset delay for a
// recognized WorkBuddy usage limit. A nil result means the payload is not a
// recognized limit or does not contain a parseable reset time.
func WorkBuddyRateLimitRetryAfter(payload []byte, now time.Time) *time.Duration {
	parsed := ParseWorkBuddyError(payload)
	if parsed.Code != "6004" && parsed.Code != "6000" {
		return nil
	}
	return parsed.RetryAfter(now)
}

// RetryAfter interprets the already decoded queue delay or usage reset message.
func (e WorkBuddyError) RetryAfter(now time.Time) *time.Duration {
	if e.Code == "6020" {
		if e.retrySeconds <= 0 {
			return nil
		}
		delay := time.Duration(e.retrySeconds) * time.Second
		return &delay
	}
	if e.Code != "6004" && e.Code != "6000" {
		return nil
	}
	match := workBuddyResetTime.FindStringSubmatch(e.Message)
	if len(match) != 5 {
		return nil
	}
	hourOffset, err := strconv.Atoi(match[3])
	if err != nil || hourOffset > 23 {
		return nil
	}
	minuteOffset := 0
	if match[4] != "" {
		minuteOffset, err = strconv.Atoi(match[4])
		if err != nil || minuteOffset > 59 {
			return nil
		}
	}
	if match[2] == "-" {
		hourOffset = -hourOffset
		minuteOffset = -minuteOffset
	}
	zoneOffset := (hourOffset*60 + minuteOffset) * 60
	zone := time.FixedZone("WorkBuddy", zoneOffset)
	resetAt, err := time.ParseInLocation("2006-01-02 15:04:05", match[1], zone)
	if err != nil {
		return nil
	}
	delay := resetAt.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return &delay
}

// NormalizeWorkBuddyRequest adapts Chat Completions to the desktop gateway.
func NormalizeWorkBuddyRequest(body []byte) ([]byte, map[string]string, error) {
	if !ojson.ValidBytes(body) || bytes.TrimSpace(body)[0] != '{' {
		return nil, nil, fmt.Errorf("workbuddy: invalid chat request")
	}
	doc := ojson.NewDoc(body)
	names := NormalizeWorkBuddyRequestDoc(doc)
	return doc.Bytes(), names, nil
}

// PrepareWorkBuddyRequest reuses immutable, unconfigured preparation within one
// Manager invocation. Every attempt owns its document and runs final rules once.
func PrepareWorkBuddyRequest(ctx context.Context, cfg *config.Config, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]byte, map[string]string, error) {
	model := thinking.ParseSuffix(req.Model).ModelName
	to := sdktranslator.FormatOpenAI
	original := opts.OriginalRequest
	if len(original) == 0 || bytes.Equal(original, req.Payload) {
		original = req.Payload
	}
	cache := cliproxyexecutor.RequestPreparationCacheFromContext(ctx)
	key, cacheable := workBuddyPreparationKeyFor(ctx, cache, cfg, provider, req, opts)
	var prepared *workBuddyPreparedBase
	if cacheable {
		stored, _ := cache.Load("workbuddy")
		prepared, _ = stored.(*workBuddyPreparedBase)
		if !key.registered && prepared != nil && prepared.key == key {
			return prepared.finalize(cfg, provider, req, opts)
		}
	} else if cache != nil {
		cache.Store("workbuddy", nil)
	}
	// The registry has no purity/version contract for registered transforms.
	// Always invoke them (including replacements with observable side effects),
	// then reuse thinking/normalization only if their actual outputs also match.
	originalTranslated, body, err := TranslateRequestPairReturningError(ctx, opts.Headers, cfg, opts.SourceFormat, to, model, original, req.Payload, true, false)
	if err != nil {
		return nil, nil, err
	}
	if !ojson.ValidBytes(body) || bytes.TrimSpace(body)[0] != '{' {
		return nil, nil, fmt.Errorf("workbuddy: invalid chat request")
	}
	if cacheable && !key.registered && (!workBuddyPureFallback(model, req.Payload, body) || !workBuddyPureFallback(model, original, originalTranslated)) {
		// A route/hook may have appeared and disappeared during translation, or
		// client compatibility may have rewritten the input. Only certify the
		// registry's model-only fallback for skipping translation on later hits.
		cacheable = false
		cache.Store("workbuddy", nil)
	}
	inputKey := key
	if cacheable && key.registered {
		key.translated = sha256.Sum256(body)
		key.translatedOriginal = sha256.Sum256(originalTranslated)
		if prepared != nil && prepared.key == key {
			if after, allowed := workBuddyPreparationKeyFor(ctx, cache, cfg, provider, req, opts); allowed && after == inputKey {
				return prepared.finalize(cfg, provider, req, opts)
			}
		}
	}
	doc := ojson.NewDoc(body)
	canonical := normalizeWorkBuddyDuplicateKeysDoc(doc)
	if bytes.Equal(originalTranslated, body) {
		if canonical != "" {
			originalTranslated = []byte(canonical)
		}
	} else if ojson.ValidBytes(originalTranslated) {
		originalDoc := ojson.NewDoc(originalTranslated)
		if normalized := normalizeWorkBuddyDuplicateKeysDoc(originalDoc); normalized != "" {
			originalTranslated = []byte(normalized)
		}
	}
	if err = ApplyRequestThinkingDoc(doc, req, opts, opts.SourceFormat.String(), to.String(), provider); err != nil {
		return nil, nil, err
	}
	_ = doc.Set("model", model)
	names := normalizeWorkBuddyRequestDoc(doc)
	if cacheable {
		// Do not publish a snapshot if a callback/reload changed its inputs while
		// preparing it. In particular, plugin-produced output is never retained.
		after, allowed := workBuddyPreparationKeyFor(ctx, cache, cfg, provider, req, opts)
		if allowed && after == inputKey {
			prepared = &workBuddyPreparedBase{key: key, body: doc.String(), original: string(originalTranslated)}
			for safe, name := range names {
				prepared.names = append(prepared.names, workBuddyPreparedName{safe, name})
			}
			cache.Store("workbuddy", prepared)
			return prepared.finalize(cfg, provider, req, opts)
		}
		cache.Store("workbuddy", nil)
	}
	// This is the final semantic barrier, once per attempt, after all built-ins.
	NewPayloadFinalizer(cfg, provider, model, to.String(), "", originalTranslated, req, opts).FinalizeDoc(doc)
	return doc.Bytes(), names, nil
}

type workBuddyPreparationKey struct {
	config                         *config.Config
	inputs, payload, original      [32]byte
	translated, translatedOriginal [32]byte
	registered                     bool
}

type workBuddyPreparedName struct{ safe, original string }

func workBuddyPureFallback(model string, input, translated []byte) bool {
	if model == "" || ojson.GetBytesNoCopy(input, "model").String() == model {
		return bytes.Equal(input, translated)
	}
	doc := ojson.NewDoc(input)
	if err := doc.Set("model", model); err != nil {
		return false
	}
	return bytes.Equal(doc.Bytes(), translated)
}

// Strings and this private, write-once slice never expose a mutable doc or map.
type workBuddyPreparedBase struct {
	key            workBuddyPreparationKey
	body, original string
	names          []workBuddyPreparedName
}

func (p *workBuddyPreparedBase) finalize(cfg *config.Config, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]byte, map[string]string, error) {
	doc := ojson.NewDocString(p.body)
	names := make(map[string]string, len(p.names))
	for _, name := range p.names {
		names[name.safe] = name.original
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	NewPayloadFinalizer(cfg, provider, model, sdktranslator.FormatOpenAI.String(), "", []byte(p.original), req, opts).FinalizeDoc(doc)
	return doc.Bytes(), names, nil
}

func workBuddyPreparationCacheAllowed(cfg *config.Config, provider string) bool {
	// Even an installed but currently empty plugin host has no purity contract.
	// A future plugin activation must not skip normalizers or thinking appliers.
	if sdktranslator.HasPluginHooks() || (cfg != nil && cfg.Plugins.Enabled) || thinking.GetProviderApplier(provider) != nil {
		return false
	}
	applier, native := thinking.GetProviderApplier("openai").(*openaithinking.Applier)
	return native && applier != nil && (provider == "workbuddy" || provider == "workbuddy-ai")
}

type workBuddyModelInputs struct {
	Info                       *registry.ModelInfo
	MetadataModelID            string
	ExplicitThinking           bool
	ExplicitInputModalities    bool
	MaxContextLength           int
	SupportConfigurationUpdate bool
	NativeCapabilities         *registry.NativeCapabilities
	UserDefined, IsCompat      bool
}

func workBuddyModelPreparationInputs(info *registry.ModelInfo) workBuddyModelInputs {
	if info == nil {
		return workBuddyModelInputs{}
	}
	// Include internal fields omitted by ModelInfo's public JSON representation.
	return workBuddyModelInputs{info, info.MetadataModelID, info.ExplicitThinking, info.ExplicitInputModalities,
		info.MaxContextLength, info.SupportConfigurationUpdate, info.NativeCapabilities, info.UserDefined, info.IsCompat}
}

func workBuddyPreparationKeyFor(ctx context.Context, cache *cliproxyexecutor.RequestPreparationCache, cfg *config.Config, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (workBuddyPreparationKey, bool) {
	if cache == nil || !workBuddyPreparationCacheAllowed(cfg, provider) {
		return workBuddyPreparationKey{}, false
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	resolved, hasResolved := coreauth.ResolvedModelInfo(req)
	effective := resolved
	if !hasResolved {
		// The static catalog has no cheap revision token. Its fallback must keep
		// resolving on the uncached path instead of pinning an obsolete snapshot
		// or walking/cloning the entire catalog merely to construct a cache key.
		effective = registry.GetGlobalRegistry().GetModelInfo(model, provider)
		if effective == nil {
			return workBuddyPreparationKey{}, false
		}
	}
	var ginHeaders http.Header
	if ctx != nil {
		if g, ok := ctx.Value("gin").(*gin.Context); ok && g != nil && g.Request != nil {
			ginHeaders = g.Request.Header
		}
	}
	// Hash values, never addresses/lengths of mutable payloads or model metadata.
	// Only selected model capabilities from req.Metadata affect built-in thinking;
	// scheduling IDs/callbacks in opts.Metadata affect signing/final rules, not this
	// base. Request interceptors have already run and their body/headers are keyed.
	inputs := struct {
		Provider, Model                  string
		RequestFormat, Source, Response  sdktranslator.Format
		Stream, KeepTopLevelEffort       bool
		Alt, Query, RequestedModel, Path string
		Headers, GinHeaders              http.Header
		PayloadRules                     config.PayloadConfig
		SourceHash                       string
		CodexMultiAgent, CodexOrphan     bool
		Resolved                         bool
		SelectedModel                    workBuddyModelInputs
	}{
		Provider: provider, Model: req.Model, RequestFormat: req.Format, Source: opts.SourceFormat, Response: opts.ResponseFormat,
		Stream: opts.Stream, KeepTopLevelEffort: opts.KeepTopLevelEffort, Alt: opts.Alt, Query: opts.Query.Encode(),
		RequestedModel: PayloadRequestedModel(opts, req.Model), Path: PayloadRequestPath(opts), Headers: opts.Headers, GinHeaders: ginHeaders,
		Resolved: hasResolved, SelectedModel: workBuddyModelPreparationInputs(effective),
	}
	if cfg != nil {
		// Runtime reloads publish a new cfg pointer/SourceHash. For in-place SDK
		// updates also include every config value read by this preparation path.
		inputs.SourceHash, inputs.PayloadRules = cfg.SourceHash, cfg.Payload
		inputs.CodexMultiAgent, inputs.CodexOrphan = cfg.Client.Codex.OptimizeMultiAgentV2, cfg.Codex.OrphanDelegationCompatibility
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		// Unrepresentable custom config values are safe on the uncached path.
		return workBuddyPreparationKey{}, false
	}
	return workBuddyPreparationKey{
		config: cfg, inputs: sha256.Sum256(encoded), payload: sha256.Sum256(req.Payload), original: sha256.Sum256(opts.OriginalRequest),
		registered: sdktranslator.HasRequestTransformer(opts.SourceFormat, sdktranslator.FormatOpenAI),
	}, true
}

// NormalizeWorkBuddyRequestDoc performs built-in gateway normalization in place.
// Callers must provide an object document and apply payload rules after this stage.
func NormalizeWorkBuddyRequestDoc(doc *ojson.Doc) map[string]string {
	normalizeWorkBuddyDuplicateKeysDoc(doc)
	return normalizeWorkBuddyRequestDoc(doc)
}

func normalizeWorkBuddyRequestDoc(doc *ojson.Doc) map[string]string {
	_ = doc.Set("stream", true)
	if value := doc.Get("max_completion_tokens"); value.Exists() {
		if !doc.Get("max_tokens").Exists() {
			_ = doc.SetRaw("max_tokens", value.Raw)
		}
		_ = doc.Delete("max_completion_tokens")
	}
	if stop := doc.Get("stop"); stop.Type == ojson.String {
		_ = doc.Set("stop", []string{stop.String()})
	}
	tools := workBuddyArray(doc.Get("tools"))
	taken := make(map[string]bool)
	for _, tool := range tools {
		name := workBuddyString(tool.Get("function.name"))
		if name != "" && !workBuddyToolNameChars.MatchString(name) {
			taken[name] = true
		}
	}
	names, upstreamNames := make(map[string]string), make(map[string]string)
	for i, tool := range tools {
		fn := tool.Get("function")
		if !fn.IsObject() {
			continue
		}
		path := "tools." + strconv.Itoa(i) + ".function"
		name := workBuddyString(fn.Get("name"))
		if workBuddyToolNameChars.MatchString(name) {
			base := workBuddyToolNameChars.ReplaceAllString(name, "_")
			safe := base
			for suffix := 2; taken[safe]; suffix++ {
				safe = fmt.Sprintf("%s_%d", base, suffix)
			}
			taken[safe] = true
			names[safe], upstreamNames[name] = name, safe
			_ = doc.Set(path+".name", safe)
		}
		path += ".parameters"
		if !fn.Get("parameters").IsObject() {
			_ = doc.SetRaw(path, "{}")
		}
		normalizeWorkBuddySchemaDoc(doc, path)
		if !doc.Get(path + ".type").Exists() {
			_ = doc.Set(path+".type", "object")
		}
		if doc.Get(path+".type").String() == "object" && doc.Get(path+".properties").Type == ojson.Null {
			_ = doc.SetRaw(path+".properties", "{}")
		}
	}
	messages := workBuddyArray(doc.Get("messages"))
	for i, message := range messages {
		if !message.IsObject() {
			continue
		}
		path := "messages." + strconv.Itoa(i)
		if message.Get("role").String() == "developer" {
			_ = doc.Set(path+".role", "system")
		}
		content := message.Get("content")
		if content.Type == ojson.String {
			normalizeWorkBuddyTextDoc(doc, path+".content", content)
		} else if content.IsArray() {
			for j, part := range content.Array() {
				normalizeWorkBuddyTextDoc(doc, path+".content."+strconv.Itoa(j)+".text", part.Get("text"))
			}
		}
		if message.Get("role").String() == "tool" {
			if safe := upstreamNames[workBuddyString(message.Get("name"))]; safe != "" {
				_ = doc.Set(path+".name", safe)
			}
		}
		for j, call := range workBuddyArray(message.Get("tool_calls")) {
			if safe := upstreamNames[workBuddyString(call.Get("function.name"))]; safe != "" {
				_ = doc.Set(path+".tool_calls."+strconv.Itoa(j)+".function.name", safe)
			}
		}
	}
	if len(messages) == 0 {
		_ = doc.SetRaw("messages", `[{"role":"system","content":"You are a helpful assistant."}]`)
	} else if doc.Get("messages.0.role").String() != "system" {
		messagesRaw := doc.Get("messages").Raw
		_ = doc.SetRaw("messages", `[{"role":"system","content":"You are a helpful assistant."},`+strings.TrimSpace(messagesRaw)[1:])
	}
	if choice := doc.Get("tool_choice"); choice.IsObject() {
		switch choice.Get("type").String() {
		case "function":
			name := workBuddyString(choice.Get("function.name"))
			if safe := upstreamNames[name]; safe != "" {
				name = safe
			}
			_ = doc.Set("tool_choice", name)
		case "auto", "required", "none":
			_ = doc.Set("tool_choice", choice.Get("type").String())
		default:
			_ = doc.Delete("tool_choice")
		}
	}
	if doc.Get("tool_choice").String() == "none" {
		_ = doc.Delete("tools")
		_ = doc.Delete("functions")
		_ = doc.Delete("tool_choice")
	}
	return names
}

// Keep the former typed-field semantics rather than coercing scalars to strings
// or treating a non-array Result as a one-element array.
func workBuddyArray(value ojson.Result) []ojson.Result {
	if !value.IsArray() {
		return nil
	}
	return value.Array()
}

func workBuddyString(value ojson.Result) string {
	if value.Type != ojson.String {
		return ""
	}
	return value.String()
}

// normalizeWorkBuddyDuplicateKeysDoc restores the former map decoder's recursive
// last-key-wins semantics before any built-in reads/edits. The ordinary path only
// scans structure and member names; the slow path retains raw numeric values and
// rebuilds only objects containing duplicates or a changed descendant.
// The returned canonical input is non-empty only when a duplicate was removed.
func normalizeWorkBuddyDuplicateKeysDoc(doc *ojson.Doc) string {
	root := doc.Get("@this")
	scan := workBuddyKeyScanner{raw: root.Raw}
	if !scan.value() {
		return ""
	}
	canonical, _ := workBuddyLastObjectKeys(root)
	*doc = *ojson.NewDocString(canonical)
	return canonical
}

// This scanner consumes valid JSON and compares decoded keys within each object.
// It skips value strings without unescaping/copying prompts or tool arguments.
type workBuddyKeyScanner struct {
	raw string
	pos int
}

func (s *workBuddyKeyScanner) space() {
	for s.pos < len(s.raw) && s.raw[s.pos] <= ' ' {
		s.pos++
	}
}

func (s *workBuddyKeyScanner) stringEnd() {
	s.pos++
	for s.pos < len(s.raw) {
		index := strings.IndexByte(s.raw[s.pos:], '"')
		if index < 0 {
			s.pos = len(s.raw)
			return
		}
		end := s.pos + index
		back := end - 1
		for back >= 0 && s.raw[back] == '\\' {
			back--
		}
		s.pos = end + 1
		if (end-back)%2 == 1 {
			return
		}
	}
}

func (s *workBuddyKeyScanner) value() bool {
	s.space()
	if s.pos == len(s.raw) {
		return false
	}
	switch s.raw[s.pos] {
	case '{':
		s.pos++
		var small [8]string
		count := 0
		var seen map[string]struct{}
		for {
			s.space()
			if s.pos == len(s.raw) {
				return false
			}
			if s.raw[s.pos] == '}' {
				s.pos++
				return false
			}
			start := s.pos
			s.stringEnd()
			name := s.raw[start+1 : s.pos-1]
			if strings.IndexByte(name, '\\') >= 0 {
				name = ojson.Parse(s.raw[start:s.pos]).String()
			}
			if seen != nil {
				if _, exists := seen[name]; exists {
					return true
				}
				seen[name] = struct{}{}
			} else {
				for _, previous := range small[:count] {
					if previous == name {
						return true
					}
				}
				if count < len(small) {
					small[count] = name
					count++
				} else {
					seen = make(map[string]struct{}, 2*len(small))
					for _, previous := range small {
						seen[previous] = struct{}{}
					}
					seen[name] = struct{}{}
				}
			}
			s.space()
			s.pos++ // colon
			if s.value() {
				return true
			}
			s.space()
			if s.pos < len(s.raw) && s.raw[s.pos] == ',' {
				s.pos++
			}
		}
	case '[':
		s.pos++
		for {
			s.space()
			if s.pos == len(s.raw) {
				return false
			}
			if s.raw[s.pos] == ']' {
				s.pos++
				return false
			}
			if s.value() {
				return true
			}
			s.space()
			if s.pos < len(s.raw) && s.raw[s.pos] == ',' {
				s.pos++
			}
		}
	case '"':
		s.stringEnd()
	default:
		for s.pos < len(s.raw) && s.raw[s.pos] > ' ' && s.raw[s.pos] != ',' && s.raw[s.pos] != ']' && s.raw[s.pos] != '}' {
			s.pos++
		}
	}
	return false
}

func workBuddyLastObjectKeys(value ojson.Result) (string, bool) {
	var doc *ojson.Doc
	if value.IsObject() {
		members := make(map[string]ojson.Result)
		var order []string
		duplicate := false
		value.ForEach(func(key, child ojson.Result) bool {
			name := key.String()
			if _, exists := members[name]; exists {
				duplicate = true
			} else {
				order = append(order, name)
			}
			members[name] = child
			return true
		})
		if duplicate {
			doc = ojson.NewDocString("{}")
		}
		for _, name := range order {
			raw, changed := workBuddyLastObjectKeys(members[name])
			if duplicate || changed {
				if doc == nil {
					doc = ojson.NewDocString(value.Raw)
				}
				// Force a literal object member, including empty/numeric keys.
				_ = doc.SetRaw(":"+ojson.Escape(name), raw)
			}
		}
	} else if value.IsArray() {
		value.ForEach(func(index, child ojson.Result) bool {
			raw, changed := workBuddyLastObjectKeys(child)
			if changed {
				if doc == nil {
					doc = ojson.NewDocString(value.Raw)
				}
				_ = doc.SetRaw(strconv.FormatInt(index.Int(), 10), raw)
			}
			return true
		})
	}
	if doc == nil {
		return value.Raw, false
	}
	return doc.String(), true
}

func normalizeWorkBuddyTextDoc(doc *ojson.Doc, path string, value ojson.Result) {
	if value.Type == ojson.String {
		text := value.String()
		if normalized := normalizeWorkBuddyClientText(text); normalized != text {
			_ = doc.Set(path, normalized)
		}
	}
}

// WorkBuddy rejects these client boilerplates before routing to the model.
func normalizeWorkBuddyClientText(text string) string {
	text = replaceWorkBuddyClientText(text, "x-anthropic-billing-header:", "xX", workBuddyBillingHeader, "")
	text = replaceWorkBuddyClientText(text, "You are Claude Code,", "yY", workBuddyClaudeSignature, "You are Claude Code, Anthropic's CLI for Claude")
	text = replaceWorkBuddyClientText(text, "To give feedbac", "tT", workBuddyClaudeFeedback, "Users can report issues and share feedback at ${1}")
	text = replaceWorkBuddyClientText(text, "You are an AI agent powered by", "yY", workBuddyDSHSignature, "You are an AI agent built with DeepSeek Harness")
	text = replaceWorkBuddyClientText(text, "You are interacting", "yY", workBuddyDSHGUIContext, "You interact with the user through the DeepSeek Harness Web GUI")
	text = replaceWorkBuddyClientText(text, "prefixed with @", "pP", workBuddyDSHPathReference, "Tokens starting with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root")
	text = strings.ReplaceAll(text, "Main branch (you will usually use this for PRs)", "Main branch (usually the base for PRs)")
	const codexSignature = "Codex CLI is an open source project led by OpenAI."
	if index := strings.Index(text, codexSignature); index >= 0 && (index < 4 || !strings.EqualFold(text[index-4:index], "the ")) {
		text = text[:index] + "The " + text[index:]
	}
	return text
}

func replaceWorkBuddyClientText(text, literal, initials string, pattern *regexp.Regexp, replacement string) string {
	// These ASCII anchors deliberately exclude K/S: their SimpleFold cycles also
	// contain Kelvin sign/long s, whose UTF-8 lengths differ. The full regex still
	// handles all Unicode folds, boundaries and capture groups after this filter.
	for remaining := text; len(remaining) >= len(literal); {
		index := strings.IndexAny(remaining, initials)
		if index < 0 || len(remaining)-index < len(literal) {
			break
		}
		if strings.EqualFold(remaining[index:index+len(literal)], literal) {
			// ReplaceAllString allocates even on a miss; near matches must also
			// retain the original string without allocating.
			if pattern.MatchString(text) {
				return pattern.ReplaceAllString(text, replacement)
			}
			break
		}
		remaining = remaining[index+1:]
	}
	return text
}

// The gateway rejects boolean schemas, untyped enums and tuple-style items.
// Only schema positions are visited; property names and literal values stay intact.
func normalizeWorkBuddySchemaDoc(doc *ojson.Doc, path string) {
	schema := doc.Get(path)
	if schema.Type == ojson.True || schema.Type == ojson.False {
		if schema.Bool() {
			_ = doc.SetRaw(path, "{}")
		} else {
			_ = doc.SetRaw(path, `{"not":{}}`)
		}
		return
	}
	if !schema.IsObject() {
		return
	}
	if enum := workBuddyArray(schema.Get("enum")); schema.Get("type").Type == ojson.Null && len(enum) > 0 {
		groups := make(map[string][]json.RawMessage)
		var order []string
		for _, item := range enum {
			kind := "null"
			switch item.Type {
			case ojson.String:
				kind = "string"
			case ojson.Number:
				kind = "number"
			case ojson.True, ojson.False:
				kind = "boolean"
			case ojson.JSON:
				if item.IsArray() {
					kind = "array"
				} else {
					kind = "object"
				}
			}
			if _, exists := groups[kind]; !exists {
				order = append(order, kind)
			}
			groups[kind] = append(groups[kind], json.RawMessage(item.Raw))
		}
		if len(order) == 1 {
			_ = doc.Set(path+".type", order[0])
		} else {
			branches := make([]any, 0, len(order))
			for _, kind := range order {
				branches = append(branches, map[string]any{"type": kind, "enum": groups[kind]})
			}
			_ = doc.Delete(path + ".enum")
			if existing := schema.Get("anyOf"); existing.Exists() {
				if !schema.Get("allOf").IsArray() {
					_ = doc.SetRaw(path+".allOf", "[]")
				}
				_ = doc.Set(path+".allOf.-1", map[string]any{"anyOf": json.RawMessage(existing.Raw)})
				_ = doc.Set(path+".allOf.-1", map[string]any{"anyOf": branches})
				_ = doc.Delete(path + ".anyOf")
			} else {
				_ = doc.Set(path+".anyOf", branches)
			}
		}
	}
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if properties := doc.Get(path + "." + key); properties.IsObject() {
			properties.ForEach(func(name, _ ojson.Result) bool {
				normalizeWorkBuddySchemaDoc(doc, path+"."+key+"."+ojson.Escape(name.String()))
				return true
			})
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		for i := range workBuddyArray(doc.Get(path + "." + key)) {
			normalizeWorkBuddySchemaDoc(doc, path+"."+key+"."+strconv.Itoa(i))
		}
	}
	for _, key := range []string{"items", "additionalItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"} {
		child := doc.Get(path + "." + key)
		if !child.Exists() {
			continue
		}
		if (child.Type == ojson.True || child.Type == ojson.False) && (key == "additionalProperties" || key == "unevaluatedProperties") {
			continue
		}
		if child.IsArray() && key == "items" {
			_ = doc.SetRaw(path+".items", `{"anyOf":`+child.Raw+`}`)
			_ = doc.Delete(path + ".additionalItems")
		}
		normalizeWorkBuddySchemaDoc(doc, path+"."+key)
	}
}

// NormalizeWorkBuddyChunk restores caller tool names and removes empty call lists.
func NormalizeWorkBuddyChunk(payload []byte, model string, names map[string]string) ([]byte, error) {
	frames, err := normalizeWorkBuddyChunks(payload, model, names, false)
	if err != nil {
		return nil, err
	}
	return frames[0], nil
}

// NormalizeWorkBuddyChunks normalizes and splits in one choices walk, serializing
// ordinary frames once and creating a second document only for a combined finish.
func NormalizeWorkBuddyChunks(payload []byte, model string, names map[string]string) ([][]byte, error) {
	return normalizeWorkBuddyChunks(payload, model, names, true)
}

func normalizeWorkBuddyChunks(payload []byte, model string, names map[string]string, split bool) ([][]byte, error) {
	if !ojson.ValidBytes(payload) || bytes.TrimSpace(payload)[0] != '{' {
		return nil, fmt.Errorf("workbuddy: invalid stream chunk")
	}
	doc := ojson.NewDoc(payload)
	canonical := normalizeWorkBuddyDuplicateKeysDoc(doc)
	_ = doc.Set("model", model)
	_ = doc.Set("object", "chat.completion.chunk")
	finished := workBuddyChunkChoices(doc, names, true, split)
	if len(finished) == 0 {
		return [][]byte{doc.Bytes()}, nil
	}
	terminal := ojson.NewDoc(payload)
	if canonical != "" {
		terminal = ojson.NewDocString(canonical)
	}
	_ = terminal.Set("model", model)
	_ = terminal.Set("object", "chat.completion.chunk")
	return splitWorkBuddyFinishDocs(doc, terminal, finished), nil
}

type workBuddyFinishedChoice struct {
	Index  json.RawMessage `json:"index"`
	Delta  struct{}        `json:"delta"`
	Finish string          `json:"finish_reason"`
}

func workBuddyChunkChoices(doc *ojson.Doc, names map[string]string, normalize, split bool) []workBuddyFinishedChoice {
	var finished []workBuddyFinishedChoice
	for i, choice := range workBuddyArray(doc.Get("choices")) {
		path := "choices." + strconv.Itoa(i)
		if normalize {
			for _, key := range []string{"delta", "message"} {
				calls := choice.Get(key + ".tool_calls")
				if !calls.IsArray() {
					continue
				}
				items := calls.Array()
				if len(items) == 0 {
					_ = doc.Delete(path + "." + key + ".tool_calls")
				}
				for j, call := range items {
					if name := names[workBuddyString(call.Get("function.name"))]; name != "" {
						_ = doc.Set(path+"."+key+".tool_calls."+strconv.Itoa(j)+".function.name", name)
					}
				}
			}
		}
		finish := choice.Get("finish_reason")
		if !split || finish.Type != ojson.String || finish.String() == "" {
			continue
		}
		// Inspect the original delta, ignoring the empty calls removed above.
		hasDelta := false
		if delta := choice.Get("delta"); delta.IsObject() {
			delta.ForEach(func(key, value ojson.Result) bool {
				if normalize && key.String() == "tool_calls" && value.IsArray() && len(value.Array()) == 0 {
					return true
				}
				hasDelta = true
				return false
			})
		}
		if !hasDelta {
			continue
		}
		index := choice.Get("index").Raw
		if index == "" {
			index = "null"
		}
		finished = append(finished, workBuddyFinishedChoice{Index: json.RawMessage(index), Finish: finish.String()})
		_ = doc.Delete(path + ".finish_reason")
	}
	return finished
}

func splitWorkBuddyFinishDocs(content, terminal *ojson.Doc, finished []workBuddyFinishedChoice) [][]byte {
	_ = terminal.Set("choices", finished)
	_ = content.Delete("usage")
	return [][]byte{content.Bytes(), terminal.Bytes()}
}

// SplitWorkBuddyFinishChunks keeps final text/tool deltas separate from the
// terminal event, which the shared Gemini translator processes independently.
func SplitWorkBuddyFinishChunks(payload []byte) [][]byte {
	if !ojson.ValidBytes(payload) {
		return [][]byte{payload}
	}
	doc := ojson.NewDoc(payload)
	canonical := normalizeWorkBuddyDuplicateKeysDoc(doc)
	finished := workBuddyChunkChoices(doc, nil, false, true)
	if len(finished) == 0 {
		if canonical != "" {
			return [][]byte{doc.Bytes()}
		}
		return [][]byte{payload}
	}
	terminal := ojson.NewDoc(payload)
	if canonical != "" {
		terminal = ojson.NewDocString(canonical)
	}
	return splitWorkBuddyFinishDocs(doc, terminal, finished)
}

type workBuddyToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type workBuddyChoice struct {
	content, reasoning strings.Builder
	finish             string
	tools              map[int]*workBuddyAggregateToolCall
}

type workBuddyAggregateToolCall struct {
	id              string
	name, arguments strings.Builder
}

// WorkBuddyAggregate collects an SSE completion for clients requesting JSON.
type WorkBuddyAggregate struct {
	id      string
	created int64
	usage   json.RawMessage
	choices map[int]*workBuddyChoice
}

func (a *WorkBuddyAggregate) Add(payload []byte) error {
	var frame struct {
		ID      string          `json:"id"`
		Created int64           `json:"created"`
		Usage   json.RawMessage `json:"usage"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content   string              `json:"content"`
				Reasoning string              `json:"reasoning_content"`
				ToolCalls []workBuddyToolCall `json:"tool_calls"`
			} `json:"delta"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return err
	}
	if a.choices == nil {
		a.choices = make(map[int]*workBuddyChoice)
	}
	if frame.ID != "" {
		a.id = frame.ID
	}
	if frame.Created != 0 {
		a.created = frame.Created
	}
	if len(frame.Usage) > 0 && string(frame.Usage) != "null" {
		a.usage = frame.Usage
	}
	for _, incoming := range frame.Choices {
		choice := a.choices[incoming.Index]
		if choice == nil {
			choice = &workBuddyChoice{tools: make(map[int]*workBuddyAggregateToolCall)}
			a.choices[incoming.Index] = choice
		}
		choice.content.WriteString(incoming.Delta.Content)
		choice.reasoning.WriteString(incoming.Delta.Reasoning)
		if incoming.Finish != "" {
			choice.finish = incoming.Finish
		}
		for _, fragment := range incoming.Delta.ToolCalls {
			call := choice.tools[fragment.Index]
			if call == nil {
				call = &workBuddyAggregateToolCall{}
				choice.tools[fragment.Index] = call
			}
			if fragment.ID != "" {
				call.id = fragment.ID
			}
			call.name.WriteString(fragment.Function.Name)
			call.arguments.WriteString(fragment.Function.Arguments)
		}
	}
	return nil
}

func (a *WorkBuddyAggregate) Response(model string) ([]byte, error) {
	indices := make([]int, 0, len(a.choices))
	for index := range a.choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	doc := ojson.NewDocString(`{"object":"chat.completion","choices":[]}`)
	_ = doc.Set("id", a.id)
	_ = doc.Set("created", a.created)
	_ = doc.Set("model", model)
	for i, index := range indices {
		choice := a.choices[index]
		path := "choices." + strconv.Itoa(i)
		_ = doc.SetRaw(path, `{"message":{"role":"assistant"}}`)
		_ = doc.Set(path+".index", index)
		_ = doc.Set(path+".finish_reason", choice.finish)
		path += ".message"
		_ = doc.Set(path+".content", choice.content.String())
		if choice.reasoning.Len() > 0 {
			_ = doc.Set(path+".reasoning_content", choice.reasoning.String())
		}
		if len(choice.tools) > 0 {
			if choice.content.Len() == 0 {
				_ = doc.Set(path+".content", nil)
			}
			toolIndices := make([]int, 0, len(choice.tools))
			for toolIndex := range choice.tools {
				toolIndices = append(toolIndices, toolIndex)
			}
			sort.Ints(toolIndices)
			for j, toolIndex := range toolIndices {
				call := choice.tools[toolIndex]
				callPath := path + ".tool_calls." + strconv.Itoa(j)
				_ = doc.SetRaw(callPath, `{"type":"function","function":{}}`)
				_ = doc.Set(callPath+".id", call.id)
				_ = doc.Set(callPath+".function.name", call.name.String())
				_ = doc.Set(callPath+".function.arguments", call.arguments.String())
			}
		}
	}
	if len(a.usage) > 0 {
		_ = doc.SetRawBytes("usage", a.usage)
	}
	return doc.Bytes(), nil
}
