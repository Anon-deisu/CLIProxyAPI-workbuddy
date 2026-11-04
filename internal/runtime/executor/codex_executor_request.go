package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	// codexUserAgent mirrors openai/codex rust-v0.153.4 get_codex_user_agent():
	// "{originator}/{version} ({os_type} {os_version}; {arch}) {terminal}" with no
	// MCP suffix. Since 0.15x the CLI no longer overrides the originator, so the
	// default "codex_cli_rs" is what real clients send.
	codexUserAgent             = "codex_cli_rs/0.153.4 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10"
	codexOriginator            = "codex_cli_rs"
	codexDefaultImageToolModel = "gpt-image-2"
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
)

type codexPersona struct {
	userAgent  string
	originator string
}

var codexPersonas = []codexPersona{
	{userAgent: "codex_cli_rs/0.154.0 (Ubuntu 24.04; x86_64) kitty", originator: "codex_cli_rs"},
	{userAgent: "codex_cli_rs/0.154.0 (Fedora 41; x86_64) foot", originator: "codex_cli_rs"},
	{userAgent: "codex_cli_rs/0.154.0 (Debian 12.7; x86_64) alacritty", originator: "codex_cli_rs"},
	{userAgent: "codex_exec/0.154.0 (macOS 15.6; arm64) Terminal.app", originator: "codex_exec"},
}

func codexPersonaForAuth(auth *cliproxyauth.Auth) codexPersona {
	if auth == nil {
		return codexPersona{userAgent: codexUserAgent, originator: codexOriginator}
	}
	if auth.Metadata != nil {
		for _, key := range []string{"codex_fingerprint_seed", "account_id"} {
			if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				seed := strings.TrimSpace(value)
				hash := fnv.New32a()
				_, _ = hash.Write([]byte(seed))
				return codexPersonas[hash.Sum32()%uint32(len(codexPersonas))]
			}
		}
	}
	seed := strings.TrimSpace(auth.ID)
	if seed == "" {
		return codexPersona{userAgent: codexUserAgent, originator: codexOriginator}
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(seed))
	return codexPersonas[hash.Sum32()%uint32(len(codexPersonas))]
}

func effectiveCodexRoutingHint(cfg *config.Config) string {
	if cfg == nil || cfg.Routing.CodexRoutingHint == nil {
		return "europe"
	}
	return strings.TrimSpace(*cfg.Routing.CodexRoutingHint)
}

var dataTag = []byte("data:")

func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte) {
	original, body, _, _ := translateCodexRequestPairWithUpdateIntent(from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
	return original, body
}

func translateCodexRequestPairWithUpdateIntent(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool, error) {
	return translateCodexRequestPairWithUpdateIntentContext(context.Background(), from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
}

func translateCodexRequestPairWithUpdateIntentContext(ctx context.Context, from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool, error) {
	isCompat := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	if ctx == nil {
		ctx = context.Background()
	}
	translate := func(raw []byte) ([]byte, bool, error) {
		if isCompat && from == sdktranslator.FormatClaude && to == sdktranslator.FormatCodex {
			body, err := helps.TranslateRequestReturningError(ctx, nil, nil, from, to, model, raw, stream, true)
			return body, false, err
		}
		translated := sdktranslator.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{Format: from, Model: model, Stream: stream, Body: raw})
		return translated.Body, translated.ConfigurationUpdatesChanged, translated.Err
	}
	if bytes.Equal(originalPayload, payload) {
		body, changed, err := translate(payload)
		return body, body, changed, err
	}
	originalTranslated, _, _ := translate(originalPayload)
	body, changed, err := translate(payload)
	return originalTranslated, body, changed, err
}

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCredsForRequest(req.Context(), auth, req.Header)
	if codexUseSIWCForRequest(req.Context(), auth, req.Header) && req.URL != nil && req.URL.Scheme == "https" &&
		req.URL.Host == "chatgpt.com" && req.URL.Path == "/backend-api/codex/alpha/search" {
		// ChatGPT Alpha Search requires the original OAuth partition, not SIWC.
		apiKey, _ = auth.Metadata["access_token"].(string)
		if strings.TrimSpace(apiKey) == "" {
			return statusErr{code: http.StatusUnauthorized, msg: "codex executor: original OAuth access_token is required for ChatGPT Alpha Search"}
		}
	} else if errSIWC := ensureCodexSIWCCredentialsForRequest(req.Context(), auth, apiKey, req.Header); errSIWC != nil {
		return errSIWC
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Del("Authorization")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	alignCodexContentEncoding(req)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	auth, errBinding := e.bindEgress(auth)
	if errBinding != nil {
		return nil, errBinding
	}
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = e.egress.WrapHTTPClient(httpClient, auth)
	return httpClient.Do(httpReq)
}

// FetchQuota probes account usage without issuing a model request. It returns
// only validated quota headers; callers must retain their snapshot on error.
func (e *CodexExecutor) FetchQuota(ctx context.Context, auth *cliproxyauth.Auth) (http.Header, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	auth, errBinding := e.bindEgress(auth)
	if errBinding != nil {
		return nil, errors.New("codex quota: egress binding failed")
	}
	token, baseURL := codexCreds(auth)
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("codex quota: credentials unavailable")
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://chatgpt.com"
	}
	base, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" {
		return nil, errors.New("codex quota: invalid base URL")
	}
	endpoint := (&url.URL{Scheme: base.Scheme, Host: base.Host, Path: "/backend-api/wham/usage"}).String()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, errors.New("codex quota: could not create usage request")
	}
	applyCodexHeaders(req, auth, token, false, e.cfg)
	httpClient := helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = e.egress.WrapHTTPClient(httpClient, auth)
	// A redirect is a failed probe, not permission to call another endpoint.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		return nil, errors.New("codex quota: usage request failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			helps.LogWithRequestID(ctx).Debug("codex quota: response body close failed")
		}
	}()
	// Preserve the HTTP status even when the body cannot be read, and never
	// include upstream error bodies or transport diagnostics in returned errors.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newCodexStatusErr(resp.StatusCode, []byte(fmt.Sprintf("codex quota: upstream returned HTTP %d", resp.StatusCode)))
	}
	payload, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		return nil, errors.New("codex quota: could not read usage response")
	}
	headers := helps.ParseCodexUsageQuotaHeaders(payload)
	if headers == nil {
		return nil, errors.New("codex quota: usage response contains no valid quota windows")
	}
	return headers, nil
}

type codexIdentityConfuseState struct {
	enabled                bool
	convergence            bool
	authID                 string
	originalPromptCacheKey string
	promptCacheKey         string
	fingerprint            *helps.CodexFingerprint
	turnIDs                []codexIdentityReplacement
}

func (e *CodexExecutor) cacheHelperDoc(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, doc *ojson.Doc, auth *cliproxyauth.Auth, headerSets ...http.Header) (*http.Request, []byte, error) {
	var headers http.Header
	if len(headerSets) > 0 {
		headers = headerSets[0]
	}
	var cache helps.CodexCache
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		modelName := ""
		if doc != nil {
			modelName = strings.TrimSpace(doc.Get("model").String())
		}
		if modelName == "" {
			modelName = thinking.ParseSuffix(req.Model).ModelName
		}
		cached, ok, errCache := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, headers)
		if errCache != nil {
			return nil, nil, errCache
		}
		if ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		promptCacheKey := ojson.GetBytesNoCopy(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = strings.Clone(promptCacheKey.String())
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		if promptCacheKey := ojson.GetBytesNoCopy(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = strings.Clone(strings.TrimSpace(promptCacheKey.String()))
		}
		if cache.ID == "" {
			cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
		}
		if cache.ID == "" {
			if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
				cache.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
			}
		}
	}
	if cache.ID == "" {
		cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
	}

	if cache.ID != "" {
		helps.SetStringIfDifferentDoc(doc, "prompt_cache_key", cache.ID)
	}
	rawJSON = helps.SanitizeCodexInputItemIDs(rawJSON)
	var identityState codexIdentityConfuseState
	if strings.HasSuffix(url, "/responses") {
		rawJSON, identityState = applyCodexIdentityConfuseBody(e.cfg, auth, userPayload, rawJSON)
	} else {
		rawJSON, identityState = applyCodexLegacyIdentityConfuseBody(e.cfg, auth, userPayload, rawJSON)
	}
	if identityState.promptCacheKey != "" {
		cache.ID = identityState.promptCacheKey
	}
	wireBody := rawJSON
	if e.cfg != nil && e.cfg.Codex.UpstreamRequestCompression && auth != nil &&
		!codexAuthUsesAPIKey(auth) && !codexUseSIWCForRequest(ctx, auth, headers) &&
		len(rawJSON) >= e.cfg.Codex.RequestCompressionMinBytes() {
		parsedURL, errParse := neturl.Parse(url)
		if errParse == nil && strings.EqualFold(parsedURL.Hostname(), "chatgpt.com") && strings.HasPrefix(parsedURL.Path, "/backend-api/codex/") {
			encoder, errNewWriter := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
			if errNewWriter == nil {
				compressed := encoder.EncodeAll(rawJSON, nil)
				if errClose := encoder.Close(); errClose != nil {
					log.Errorf("codex executor: close request encoder error: %v", errClose)
				} else if len(compressed) < len(rawJSON) {
					wireBody = compressed
				}
			}
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(wireBody))
	if err != nil {
		return nil, nil, err
	}
	if len(wireBody) < len(rawJSON) {
		httpReq.Header.Set("Content-Encoding", "zstd")
	}
	alignCodexContentEncoding(httpReq)
	if httpReq.Header.Get("Accept-Encoding") == "" {
		httpReq.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	if cache.ID != "" {
		httpReq.Header.Set("Session-Id", cache.ID)
	}
	return httpReq, rawJSON, nil
}

func applyCodexIdentityConfuseBody(cfg *config.Config, auth *cliproxyauth.Auth, userPayload []byte, rawJSON []byte) ([]byte, codexIdentityConfuseState) {
	if codexFingerprintConvergenceEnabled(cfg, auth) {
		if !util.ParseGJSONBytesNoCopy(rawJSON).IsObject() {
			return rawJSON, codexIdentityConfuseState{}
		}
		fingerprint := helps.NewCodexFingerprint(strings.TrimSpace(auth.ID))
		return fingerprint.ApplyBody(rawJSON), codexIdentityConfuseState{
			enabled: true, convergence: true, authID: strings.TrimSpace(auth.ID), fingerprint: fingerprint,
		}
	}
	return applyCodexLegacyIdentityConfuseBody(cfg, auth, userPayload, rawJSON)
}

func applyCodexLegacyIdentityConfuseBody(cfg *config.Config, auth *cliproxyauth.Auth, userPayload []byte, rawJSON []byte) ([]byte, codexIdentityConfuseState) {
	if !codexIdentityConfuseEnabled(cfg) || auth == nil || strings.TrimSpace(auth.ID) == "" || len(rawJSON) == 0 {
		return rawJSON, codexIdentityConfuseState{}
	}
	body, errGetBody := req.GetBody()
	if errGetBody != nil || body == nil {
		return
	}
	defer func() {
		if errClose := body.Close(); errClose != nil {
			log.Errorf("codex executor: close request body copy error: %v", errClose)
		}
	}()
	prefix, errRead := io.ReadAll(io.LimitReader(body, 512))
	if errRead != nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	plain := bytes.TrimSpace(prefix)
	jsonPlaintext := len(plain) > 0 && (plain[0] == '{' || plain[0] == '[')
	multipartPlaintext := strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Type"))), "multipart/") && bytes.HasPrefix(plain, []byte("--"))
	if jsonPlaintext || multipartPlaintext {
		req.Header.Del("Content-Encoding")
		return
	}
	encodingCount := 0
	for _, value := range req.Header.Values("Content-Encoding") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" && !strings.EqualFold(token, "identity") {
				encodingCount++
			}
		}
	}
	// Magic identifies only the outermost layer, so trust declared encoding chains.
	if encodingCount > 1 {
		return
	}
	if state == nil || !state.enabled {
		return
	}
	if state.convergence {
		fingerprint := state.fingerprint
		setHeaderCasePreserved(headers, "X-Codex-Installation-Id", fingerprint.InstallationID)
		if codexSessionHeaderValue(headers) == "" {
			setCodexSessionHeaderCasePreserved(headers, "Session-Id", fingerprint.SessionID)
		}
		// Conversation_id is a user cache/session key when supplied by the client.
		// Preserve it while the account fingerprint remains in the identity fields.
		if headerValueCaseInsensitive(headers, "Conversation_id") == "" {
			setHeaderCasePreserved(headers, "Conversation_id", fingerprint.SessionID)
		}
		setHeaderCasePreserved(headers, "X-Client-Request-Id", fingerprint.SessionID)
		setHeaderCasePreserved(headers, "Thread-Id", fingerprint.SessionID)
		setHeaderCasePreserved(headers, "X-Codex-Window-Id", fingerprint.SessionID+":0")
		metadata := fingerprint.TurnMetadata(headerValueCaseInsensitive(headers, "X-Codex-Turn-Metadata"))
		setHeaderCasePreserved(headers, "X-Codex-Turn-Metadata", metadata)
		return
	}

	if rawTurnMetadata := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); rawTurnMetadata != "" {
		headers.Set("X-Codex-Turn-Metadata", applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata, state))
	}
	if state.promptCacheKey == "" {
		return
	}

	setCodexSessionHeaderCasePreserved(headers, "Session-Id", state.promptCacheKey)
	if headerValueCaseInsensitive(headers, "Conversation_id") != "" {
		setHeaderCasePreserved(headers, "Conversation_id", state.promptCacheKey)
	}
	headers.Set("X-Client-Request-Id", state.promptCacheKey)
	headers.Set("Thread-Id", state.promptCacheKey)
	headers.Set("X-Codex-Window-Id", state.promptCacheKey+":0")
}

func applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata string, state *codexIdentityConfuseState) string {
	updatedTurnMetadata := rawTurnMetadata
	if state == nil || !state.enabled {
		return updatedTurnMetadata
	}
	if state.promptCacheKey != "" && gjson.Get(rawTurnMetadata, "prompt_cache_key").Exists() {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "prompt_cache_key", state.promptCacheKey)
	} else if state.promptCacheKey != "" && state.originalPromptCacheKey != "" {
		updatedTurnMetadata = strings.ReplaceAll(updatedTurnMetadata, state.originalPromptCacheKey, state.promptCacheKey)
	}
	if turnID := strings.TrimSpace(gjson.Get(rawTurnMetadata, "turn_id").String()); turnID != "" {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "turn_id", state.confuseTurnID(turnID))
	}
	if state.promptCacheKey != "" && gjson.Get(rawTurnMetadata, "window_id").Exists() {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "window_id", state.promptCacheKey+":0")
	}
	return updatedTurnMetadata
}

func applyCodexIdentityConfuseResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	if state.convergence {
		return payload
	}
	payload = replaceCodexIdentityResponsePayload(payload, state.originalPromptCacheKey, state.promptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.original, turnID.confused)
	}
	return payload
}

func applyCodexIdentityExposeResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	if state.convergence {
		return state.fingerprint.RestoreResponse(payload)
	}
	payload = replaceCodexIdentityResponsePayload(payload, state.promptCacheKey, state.originalPromptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.confused, turnID.original)
	}
	return payload
}

func (state *codexIdentityConfuseState) confuseTurnID(turnID string) string {
	turnID = strings.TrimSpace(turnID)
	if state == nil || !state.enabled || strings.TrimSpace(state.authID) == "" || turnID == "" {
		return turnID
	}
	for _, replacement := range state.turnIDs {
		if replacement.original == turnID || replacement.confused == turnID {
			return replacement.confused
		}
	}
}

func decodeCodexUpstreamResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	decoded, errDecode := helps.DecodeResponseBody(resp.Body, resp.Header)
	if errDecode != nil {
		return fmt.Errorf("codex executor: decode upstream response: %w", errDecode)
	}
	resp.Body = decoded
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

func codexFingerprintConvergenceEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if cfg == nil || cfg.Routing.CodexFingerprintConvergence == nil || !*cfg.Routing.CodexFingerprintConvergence || auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && auth.AuthKind() == cliproxyauth.AuthKindOAuth && !codexAuthUsesAPIKey(auth) && strings.TrimSpace(auth.ID) != ""
}

func codexIdentityConfuseUUID(authID string, kind string, value string) string {
	name := strings.Join([]string{"cli-proxy-api", "codex", "identity-confuse", kind, strings.TrimSpace(authID), strings.TrimSpace(value)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0]
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}
	applyCodexHeadersWithSIWC(r, auth, codexUseSIWCForRequest(r.Context(), auth, ginHeaders), token, stream, cfg, ginHeaders)
}

// applyModelHeaderOverrides forces models.json config.override_header onto upstream headers.
func applyModelHeaderOverrides(headers http.Header, modelName string) {
	if headers == nil {
		return
	}
	overrides := registry.ModelOverrideHeaders(modelName)
	if len(overrides) == 0 {
		return
	}
	for key, value := range overrides {
		headers.Set(key, value)
	}
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") && codexSessionHeaderValue(headers) == "" {
		headers.Set("Session_id", uuid.NewString())
	}
}

// applyCodexDirectImageHeaders sets Codex upstream headers for direct /images/* calls.
// Downstream client User-Agent values are not forwarded to reduce Cloudflare 1010 blocks.
func applyCodexDirectImageHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	useSIWC := codexUseSIWCForRequest(r.Context(), auth, clientHeaders...)
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0].Clone()
		ginHeaders.Del("User-Agent")
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header.Clone()
		ginHeaders.Del("User-Agent")
	}
	applyCodexHeadersWithSIWC(r, auth, useSIWC, token, stream, cfg, ginHeaders)
}

func codexSIWCUserAgent(cfg *config.Config) string {
	agentName := config.DefaultSignInWithChatGPTIdentifier
	if cfg != nil {
		agentName = cfg.Codex.EffectiveSignInWithChatGPTIdentifier()
	}
	return fmt.Sprintf("%s (%s; %s)", agentName, runtime.GOOS, runtime.GOARCH)
}

func applyCodexSIWCRequestBodyRules(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	doc := ojson.NewDoc(body)
	if !applyCodexSIWCRequestBodyRulesDoc(doc, hasExplicitInstructions, opts...) {
		return body
	}
	return doc.Bytes()
}

func applyCodexSIWCRequestBodyRulesDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	changed := helps.SetBoolIfDifferentDoc(doc, "store", false)
	if helps.SetBoolIfDifferentDoc(doc, "stream", true) {
		changed = true
	}
	if stripCodexSIWCUnsupportedFieldsDoc(doc, hasExplicitInstructions, opts...) {
		changed = true
	}
	return changed
}

func applyCodexSIWCWebsocketBodyRules(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	doc := ojson.NewDoc(body)
	if !applyCodexSIWCWebsocketBodyRulesDoc(doc, hasExplicitInstructions, opts...) {
		return body
	}
	return doc.Bytes()
}

func applyCodexSIWCWebsocketBodyRulesDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	changed := helps.SetBoolIfDifferentDoc(doc, "store", false)
	if doc.Delete("stream") == nil {
		changed = true
	}
	if doc.Delete("background") == nil {
		changed = true
	}
	if stripCodexSIWCUnsupportedFieldsDoc(doc, hasExplicitInstructions, opts...) {
		changed = true
	}
	return changed
}

func applyCodexSIWCCompactBodyRules(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	doc := ojson.NewDoc(body)
	if !applyCodexSIWCCompactBodyRulesDoc(doc, hasExplicitInstructions, opts...) {
		return body
	}
	return doc.Bytes()
}

func applyCodexSIWCCompactBodyRulesDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	changed := false
	if doc.Delete("stream") == nil {
		changed = true
	}
	if doc.Delete("store") == nil {
		changed = true
	}
	if stripCodexSIWCUnsupportedFieldsDoc(doc, hasExplicitInstructions, opts...) {
		changed = true
	}
	return changed
}

func stripCodexSIWCUnsupportedFields(body []byte, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) []byte {
	doc := ojson.NewDoc(body)
	if !stripCodexSIWCUnsupportedFieldsDoc(doc, hasExplicitInstructions, opts...) {
		return body
	}
	return doc.Bytes()
}

func stripCodexSIWCUnsupportedFieldsDoc(doc *ojson.Doc, hasExplicitInstructions bool, opts ...cliproxyexecutor.Options) bool {
	if doc == nil {
		return false
	}
	// SIWC subscriptions reject configuration_update input items, so degrade
	// them to the classic top-level reasoning.effort and accept the cache loss.
	// An explicit model thinking suffix is an operator override: the thinking
	// layer has already resolved it into the top-level effort, so only the
	// unsupported items are removed in that case.
	thinking.CollapseConfigurationUpdatesDoc(doc, len(opts) > 0 && opts[0].KeepTopLevelEffort)
	_ = doc.Delete("prompt_cache_retention")
	_ = doc.Delete("prompt_cache_options")
	_ = doc.Delete("max_completion_tokens")
	_ = doc.Delete("temperature")
	if !hasExplicitInstructions {
		if inst := doc.Get("instructions"); inst.Exists() && strings.TrimSpace(inst.String()) == "" {
			_ = doc.Delete("instructions")
		}
	}
	return true
}

func applyCodexHeadersFromSources(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, ginHeaders http.Header) {
	applyCodexHeadersWithSIWC(r, auth, codexUseSIWCForRequest(r.Context(), auth, ginHeaders), token, stream, cfg, ginHeaders)
}

func applyCodexHeadersWithSIWC(r *http.Request, auth *cliproxyauth.Auth, useSIWC bool, token string, stream bool, cfg *config.Config, ginHeaders http.Header) {
	r.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	} else {
		r.Header.Del("Authorization")
	}

	if useSIWC {
		if sessionID := strings.TrimSpace(r.Header.Get("Session-Id")); sessionID != "" {
			r.Header.Del("Session-Id")
			setHeaderCasePreserved(r.Header, "session_id", sessionID)
			r.Header.Set("X-Client-Request-Id", sessionID)
		}
		r.Header.Set("User-Agent", codexSIWCUserAgent(cfg))
		if stream {
			r.Header.Set("Accept", "text/event-stream")
		} else {
			r.Header.Set("Accept", "application/json")
		}
		r.Header.Set("Connection", "Keep-Alive")
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
		return
	}

	if ginHeaders != nil && ginHeaders.Get("X-Codex-Beta-Features") != "" {
		r.Header.Set("X-Codex-Beta-Features", ginHeaders.Get("X-Codex-Beta-Features"))
	}
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-State", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Window-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Thread-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Session-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Openai-Internal-Codex-Responses-Lite", "")

	cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := codexAuthUsesAPIKey(auth)
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		r.Header.Set("Originator", originator)
	} else if !isAPIKey {
		r.Header.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
	applyCodexCloakingHeaders(r.Header, cfg, auth)
	if !isAPIKey {
		if hint := effectiveCodexRoutingHint(cfg); hint != "" {
			r.Header.Set("x-codex-routing-hint", hint)
		} else {
			r.Header.Del("x-codex-routing-hint")
		}
	}
}

func applyCodexCloakingHeaders(headers http.Header, cfg *config.Config, auth *cliproxyauth.Auth) {
	if headers == nil || cfg == nil || cfg.Codex.DisableCodexCloaking {
		return
	}
	persona := codexPersonaForAuth(auth)
	if codexFingerprintConvergenceEnabled(cfg, auth) && !codexAuthUsesAPIKey(auth) {
		headers.Set("User-Agent", persona.userAgent)
		headers.Set("Originator", persona.originator)
		return
	}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Originator", codexOriginator)
}

func normalizeCodexInstructions(body []byte, nativeRequest ...bool) []byte {
	if len(nativeRequest) > 0 && nativeRequest[0] {
		return body
	}
	doc := ojson.NewDoc(body)
	if !normalizeCodexInstructionsDoc(doc, nativeRequest...) {
		return body
	}
	return doc.Bytes()
}

func normalizeCodexInstructionsDoc(doc *ojson.Doc, nativeRequest ...bool) bool {
	if doc == nil || (len(nativeRequest) > 0 && nativeRequest[0]) {
		return false
	}
	instructions := doc.Get("instructions")
	if !instructions.Exists() || instructions.Type == ojson.Null {
		return doc.Set("instructions", "") == nil
	}
	return false
}

func isCodexResponsesLiteRequestDoc(doc *ojson.Doc, headers http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	if doc == nil {
		return false
	}
	value := doc.Get("client_metadata.ws_request_header_x_openai_internal_codex_responses_lite")
	return value.Type == ojson.True || (value.Type == ojson.String && strings.EqualFold(strings.TrimSpace(value.String()), "true"))
}

var imageGenToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)
var webSearchToolJSON = []byte(`{"type":"web_search"}`)
var webSearchToolArrayJSON = []byte(`[{"type":"web_search"}]`)

func isCodexWebSearchToolType(toolType string) bool {
	switch strings.TrimSpace(toolType) {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
		return true
	default:
		return false
	}
}

// ensureWebSearchToolForHistory reconciles replayed web_search_call input items
// with the request's tool declarations so upstream Codex does not fail with
// "response protection is unavailable". Standard Responses requests receive a
// {"type":"web_search"} tool declaration when missing, while Responses Lite
// requests (which omit top-level hosted tools) strip the replayed search items.
func ensureWebSearchToolForHistory(body []byte, headers http.Header) []byte {
	if !bytes.Contains(body, []byte("web_search_call")) {
		return body
	}
	doc := ojson.NewDoc(body)
	if !ensureWebSearchToolForHistoryDoc(doc, headers) {
		return body
	}
	return doc.Bytes()
}

func ensureWebSearchToolForHistoryDoc(doc *ojson.Doc, headers http.Header) bool {
	if doc == nil {
		return false
	}
	input := doc.Get("input")
	if !input.IsArray() || !strings.Contains(input.Raw, "web_search_call") {
		return false
	}
	hasWebSearchCall := false
	input.ForEach(func(_, item ojson.Result) bool {
		if strings.TrimSpace(item.Get("type").String()) == "web_search_call" {
			hasWebSearchCall = true
			return false
		}
		return true
	})
	if !hasWebSearchCall {
		return false
	}

	tools := doc.Get("tools")
	if tools.IsArray() {
		for _, t := range tools.Array() {
			if isCodexWebSearchToolType(t.Get("type").String()) {
				return false
			}
		}
	}

	if isCodexResponsesLiteRequestDoc(doc, headers) {
		items := input.Array()
		rebuilt := make([]string, 0, len(items))
		for _, item := range items {
			if strings.TrimSpace(item.Get("type").String()) == "web_search_call" {
				continue
			}
			rebuilt = append(rebuilt, item.Raw)
		}
		return doc.SetRawBytes("input", []byte("["+strings.Join(rebuilt, ",")+"]")) == nil
	}

	if !tools.Exists() || !tools.IsArray() {
		return doc.SetRawBytes("tools", webSearchToolArrayJSON) == nil
	}
	return doc.SetRawBytes("tools.-1", webSearchToolJSON) == nil
}

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func isImageGenerationFunctionTool(tool ojson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("name").String() == "image_gen.imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		tools := tool.Get("tools")
		if !tools.IsArray() {
			return false
		}
		for _, nestedTool := range tools.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func ensureImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	doc := ojson.NewDoc(body)
	if !ensureImageGenerationToolDoc(doc, baseModel, auth, headers) {
		return body
	}
	return doc.Bytes()
}

func ensureImageGenerationToolDoc(doc *ojson.Doc, baseModel string, auth *cliproxyauth.Auth, headers http.Header) bool {
	if doc == nil || isCodexResponsesLiteRequestDoc(doc, headers) {
		return false
	}
	if strings.HasSuffix(baseModel, "spark") {
		return false
	}
	if isCodexFreePlanAuth(auth) {
		return false
	}

	tools := doc.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return doc.SetRawBytes("tools", imageGenToolArrayJSON) == nil
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(t) {
			return false
		}
	}
	return doc.SetRawBytes("tools.-1", imageGenToolJSON) == nil
}

func normalizeCodexParallelToolCalls(body []byte, headers http.Header) []byte {
	doc := ojson.NewDoc(body)
	if !normalizeCodexParallelToolCallsDoc(doc, headers) {
		return body
	}
	return doc.Bytes()
}

func normalizeCodexParallelToolCallsDoc(doc *ojson.Doc, headers http.Header) bool {
	if doc == nil {
		return false
	}
	if isCodexResponsesLiteRequestDoc(doc, headers) {
		return helps.SetBoolIfDifferentDoc(doc, "parallel_tool_calls", false)
	}
	return normalizeCodexParallelToolCallsForToolsDoc(doc)
}

func normalizeCodexParallelToolCallsForTools(body []byte) []byte {
	doc := ojson.NewDoc(body)
	if !normalizeCodexParallelToolCallsForToolsDoc(doc) {
		return body
	}
	return doc.Bytes()
}

func normalizeCodexParallelToolCallsForToolsDoc(doc *ojson.Doc) bool {
	if doc == nil || !doc.Get("parallel_tool_calls").Exists() {
		return false
	}

	tools := doc.Get("tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return false
	}

	return doc.Delete("parallel_tool_calls") == nil
}

func publishCodexImageToolUsage(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte) {
	publishCodexImageToolUsageWithOutputItems(ctx, reporter, body, completedData, nil, nil)
}

func publishCodexImageToolUsageWithOutputItems(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte, itemsByIndex map[int64][]byte, fallback [][]byte) {
	detail, ok := helps.ParseCodexImageToolUsageWithOutputItems(body, completedData, itemsByIndex, fallback)
	if !ok {
		return
	}
	reporter.EnsurePublished(ctx)
	reporter.PublishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := ojson.GetBytesNoCopy(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return strings.Clone(model)
			}
			break
		}
	}
	return codexDefaultImageToolModel
}
