package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	responsesconverter "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CursorExecutor executes chat completions through Cursor's Agent Connect protocol.
type CursorExecutor struct {
	cfg *config.Config
	svc *cursorauth.AuthService
}

// NewCursorExecutor creates a Cursor executor.
func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{cfg: cfg, svc: cursorauth.NewAuthService()}
}

// Identifier returns the executor identifier.
func (e *CursorExecutor) Identifier() string { return "cursor" }

// Execute performs a non-streaming Cursor Agent chat completion.
func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	body := translateCursorRequest(from, baseModel, req.Payload, false)

	// Keep hyphen variant suffixes (-thinking/-xhigh/…) for Cursor resolution.
	// Public wire model id is the base name; parameters ride on RequestedModel.
	upstreamModel := strings.TrimPrefix(baseModel, "cursor-")
	if upstreamModel == "" {
		upstreamModel = "default"
	}
	upstreamModel = applyCursorReasoningEffort(upstreamModel, body)
	upstreamModel = applyCursorSpeed(upstreamModel, body)
	resolved := cursorlib.ResolveRequestedModel(upstreamModel)
	body, _ = sjson.SetBytes(body, "model", resolved.ModelID)

	messages, err := extractChatMessages(body)
	if err != nil {
		return resp, err
	}
	tools := prepareCursorTools(messages, extractTools(body), 0)
	messages, tools, toolConstraint, err := applyCursorToolChoice(body, messages, tools)
	if err != nil {
		return resp, err
	}

	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return resp, err
	}

	conversationID := resolveCursorConversationID(ctx, auth, req, opts, originalPayloadSource, body)
	parallelTools := true
	if value := gjson.GetBytes(body, "parallel_tool_calls"); value.Exists() {
		parallelTools = value.Bool()
	}
	result, err := runCursorChatWithRetries(
		ctx,
		creds,
		upstreamModel,
		messages,
		tools,
		cursorlib.WithConversationID(conversationID),
		cursorlib.WithParallelToolCalls(parallelTools),
	)
	if err != nil {
		return resp, humanizeCursorError(err)
	}
	if err = toolConstraint.Validate(result.ToolCalls); err != nil {
		return resp, err
	}

	outPayload := buildOpenAIChatCompletion(req.Model, result)
	reporter.Publish(ctx, usage.Detail{
		InputTokens:         result.InputTokens,
		OutputTokens:        result.OutputTokens,
		CachedTokens:        result.CacheReadTokens,
		CacheReadTokens:     result.CacheReadTokens,
		CacheCreationTokens: result.CacheWriteTokens,
		ReasoningTokens:     result.ReasoningTokens,
		TotalTokens:         result.InputTokens + result.OutputTokens,
	})

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, outPayload, &param)
	resp = cliproxyexecutor.Response{Payload: out}
	return resp, nil
}

// ExecuteStream streams OpenAI-compatible SSE chunks from Cursor Agent events.
func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	body := translateCursorRequest(from, baseModel, req.Payload, true)

	upstreamModel := strings.TrimPrefix(baseModel, "cursor-")
	if upstreamModel == "" {
		upstreamModel = "default"
	}
	upstreamModel = applyCursorReasoningEffort(upstreamModel, body)
	upstreamModel = applyCursorSpeed(upstreamModel, body)
	resolved := cursorlib.ResolveRequestedModel(upstreamModel)
	body, _ = sjson.SetBytes(body, "model", resolved.ModelID)

	messages, err := extractChatMessages(body)
	if err != nil {
		return nil, err
	}
	tools := prepareCursorTools(messages, extractTools(body), 0)
	messages, tools, toolConstraint, err := applyCursorToolChoice(body, messages, tools)
	if err != nil {
		return nil, err
	}

	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return nil, err
	}

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	conversationID := resolveCursorConversationID(ctx, auth, req, opts, originalPayloadSource, body)
	parallelTools := true
	if value := gjson.GetBytes(body, "parallel_tool_calls"); value.Exists() {
		parallelTools = value.Bool()
	}
	startOpts := []cursorlib.StartOption{
		cursorlib.WithConversationID(conversationID),
		cursorlib.WithParallelToolCalls(parallelTools),
	}
	session, err := openCursorSessionWithRetries(ctx, creds, upstreamModel, messages, tools, startOpts...)
	if err != nil {
		return nil, humanizeCursorError(err)
	}

	out := make(chan cliproxyexecutor.StreamChunk, 64)
	go func() {
		defer close(out)
		completionID := "chatcmpl-" + uuid.NewString()
		created := time.Now().Unix()
		var param any
		toolIndex := 0
		finishReason := "stop"
		var usageFinal cursorlib.StreamEvent
		observedToolCalls := make([]cursorlib.ToolCall, 0)

		emitLine := func(line []byte) bool {
			chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, line, &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					// Claude Code often closes the SSE after tool_use. Do not
					// tear down a session that is still waiting on tool_result.
					if session != nil && !session.AwaitingClientTools() {
						_ = session.Close()
					}
					return false
				}
			}
			return true
		}

		roleChunk, _ := json.Marshal(map[string]any{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   req.Model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil},
			},
		})
		if !emitLine([]byte("data: " + string(roleChunk))) {
			return
		}

		contentEmitted := false
		iterate := func(active *cursorlib.Session) error {
			return active.IterSegment(ctx, func(ev cursorlib.StreamEvent) error {
				var delta map[string]any
				switch ev.Type {
				case "text_delta":
					contentEmitted = true
					delta = map[string]any{"content": ev.Text}
				case "thinking_delta":
					contentEmitted = true
					delta = map[string]any{"reasoning_content": ev.Text}
				case "tool_call":
					if ev.ToolCall == nil {
						return nil
					}
					contentEmitted = true
					observedToolCalls = append(observedToolCalls, *ev.ToolCall)
					args, _ := json.Marshal(ev.ToolCall.Arguments)
					if args == nil {
						args = []byte("{}")
					}
					delta = map[string]any{
						"tool_calls": []map[string]any{
							{
								"index": toolIndex,
								"id":    ev.ToolCall.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      ev.ToolCall.Name,
									"arguments": string(args),
								},
							},
						},
					}
					toolIndex++
				case "usage_final":
					usageFinal = ev
					reporter.Publish(ctx, usage.Detail{
						InputTokens:         ev.InputTokens,
						OutputTokens:        ev.OutputTokens,
						CachedTokens:        ev.CacheReadTokens,
						CacheReadTokens:     ev.CacheReadTokens,
						CacheCreationTokens: ev.CacheWriteTokens,
						ReasoningTokens:     ev.ReasoningTokens,
						TotalTokens:         ev.InputTokens + ev.OutputTokens,
					})
					return nil
				case "error":
					return fmt.Errorf("%s", ev.Message)
				case "segment_end":
					if ev.Reason != "" {
						finishReason = ev.Reason
					}
					return nil
				default:
					return nil
				}
				if delta == nil {
					return nil
				}
				chunk, _ := json.Marshal(map[string]any{
					"id":      completionID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   req.Model,
					"choices": []map[string]any{
						{"index": 0, "delta": delta, "finish_reason": nil},
					},
				})
				if !emitLine([]byte("data: " + string(chunk))) {
					return context.Canceled
				}
				return nil
			})
		}

		errIter := iterate(session)
		if errors.Is(errIter, cursorlib.ErrToolResumeRebuild) && !contentEmitted {
			// The resumed run went silent. Closing it above unbound the tool
			// ids, so opening again naturally rebuilds from history.
			_ = session.Close()
			rebuilt, rebuildErr := openCursorSessionWithRetries(ctx, creds, upstreamModel, messages, tools, startOpts...)
			if rebuildErr == nil {
				session = rebuilt
				errIter = iterate(session)
			} else {
				errIter = rebuildErr
			}
		}
		// Provider connectivity failures usually arrive in Connect trailers
		// while reading, after the HTTP stream itself opened successfully. Retry
		// the complete attempt only while no model content has reached the client.
		for attempt := 0; errIter != nil && isCursorTransientProviderError(errIter) && !contentEmitted; attempt++ {
			_ = session.Close()
			if attempt >= len(cursorTransientBackoff) {
				break
			}
			if sleepErr := sleepCtx(ctx, cursorTransientBackoff[attempt]); sleepErr != nil {
				errIter = sleepErr
				break
			}
			retrySession, retryErr := openCursorSessionWithRetries(ctx, creds, upstreamModel, messages, tools, startOpts...)
			if retryErr != nil {
				errIter = retryErr
				break
			}
			session = retrySession
			errIter = iterate(session)
		}
		// A provider 400 caused by a tool-name collision gets exactly one retry
		// with the same messages and complete tool catalog, changing names only.
		if errIter != nil && isCursorHardProviderError(errIter) && !contentEmitted && len(tools) > 0 {
			_ = session.Close()
			aliasOpts := append(append([]cursorlib.StartOption(nil), startOpts...), cursorlib.WithForceToolAlias())
			aliased, aliasErr := openCursorSession(ctx, creds, upstreamModel, messages, tools, aliasOpts...)
			if aliasErr == nil {
				session = aliased
				errIter = iterate(session)
			} else {
				errIter = aliasErr
			}
		}
		if errIter != nil {
			errIter = humanizeCursorError(errIter)
			reporter.PublishFailure(ctx, errIter)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errIter}:
			case <-ctx.Done():
			}
			_ = session.Close()
			return
		}
		if constraintErr := toolConstraint.Validate(observedToolCalls); constraintErr != nil {
			reporter.PublishFailure(ctx, constraintErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: constraintErr}:
			case <-ctx.Done():
			}
			_ = session.Close()
			return
		}

		if finishReason == "tool_calls" && toolIndex == 0 {
			finishReason = "stop"
		}
		// TurnEnded can land after the segment loop has already stopped
		// reading, which leaves the counters sitting unread. Collect them if
		// they are there, but never wait: when a turn pauses for a client tool
		// Cursor sends no TurnEnded at all, and blocking for it would tax every
		// step of an agent loop for nothing.
		if usageFinal.InputTokens == 0 && usageFinal.OutputTokens == 0 &&
			usageFinal.CacheReadTokens == 0 && usageFinal.CacheWriteTokens == 0 {
			if late, ok := session.TakeUsage(0); ok {
				usageFinal = late
				reporter.Publish(ctx, usage.Detail{
					InputTokens:         late.InputTokens,
					OutputTokens:        late.OutputTokens,
					CachedTokens:        late.CacheReadTokens,
					CacheReadTokens:     late.CacheReadTokens,
					CacheCreationTokens: late.CacheWriteTokens,
					ReasoningTokens:     late.ReasoningTokens,
					TotalTokens:         late.InputTokens + late.OutputTokens,
				})
			}
		}
		// Do not Close() on stop here: TurnEnded can precede trailing
		// checkpoint/text frames. readLoop EndStream/EOF owns teardown;
		// the session manager reaper cleans any stranded runs.

		endChunk, _ := json.Marshal(map[string]any{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   req.Model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason},
			},
		})
		if !emitLine([]byte("data: " + string(endChunk))) {
			return
		}
		// OpenAI stream usage belongs in a trailing chunk with an empty choices
		// array. Responses and Anthropic translators retain the finish reason
		// from the preceding chunk and merge these authoritative counters before
		// producing response.completed/message_stop.
		if usageFinal.InputTokens > 0 || usageFinal.OutputTokens > 0 || usageFinal.CacheReadTokens > 0 || usageFinal.CacheWriteTokens > 0 {
			usageChunk, _ := json.Marshal(map[string]any{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   req.Model,
				"choices": []map[string]any{},
				"usage": cursorUsagePayload(usageFinal.InputTokens, usageFinal.OutputTokens,
					usageFinal.CacheReadTokens, usageFinal.CacheWriteTokens, usageFinal.ReasoningTokens),
			})
			if !emitLine([]byte("data: " + string(usageChunk))) {
				return
			}
		}
		// Claude translator requires the SSE "data:" prefix; bare "[DONE]"
		// is dropped and leaves Anthropic clients without message_stop
		// (Claude Code then looks non-streaming / truncates context).
		doneChunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, []byte("data: [DONE]"), &param)
		for i := range doneChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

func openCursorSession(ctx context.Context, creds cursorlib.AccountCredentials, model string, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, opts ...cursorlib.StartOption) (*cursorlib.Session, error) {
	results := trailingToolResults(messages)
	if len(results) > 0 {
		session, err := cursorlib.DefaultSessionManager().ResolveForToolResults(results)
		hadLiveRun := err == nil
		if err == nil {
			if err = session.SubmitToolResults(results); err == nil {
				return session, nil
			}
			if errors.Is(err, cursorlib.ErrToolResumeRebuild) {
				log.Warnf("cursor: tool resume requires rebuild (upstream response ended); rebuilding turn from history")
			} else {
				log.Warnf("cursor: stream submit tool results failed (%v); rebuilding turn from history", err)
			}
			_ = session.Close()
		} else {
			log.Warnf("cursor: stream resume tool session failed (%v); rebuilding turn from history", err)
		}
		// The structured assistant tool calls and tool result messages already
		// carry the complete continuation. Do not add a synthetic user message:
		// it changes role ordering and can make the provider reject the request.
		// Rotate only if a live run actually died while owning this result. A
		// missing in-memory session (restart/cross-instance routing) must keep
		// the explicit conversation id or every agent step rewrites the cache.
		if hadLiveRun {
			opts = append(opts, cursorlib.WithConversationID(uuid.NewString()))
		}
	}
	return cursorlib.StartSession(ctx, creds, model, messages, tools, opts...)
}

// translateCursorRequest gives the Responses entrypoint a deterministic
// conversion dependency. Relying on a process-wide init side effect allowed
// embedded builds to pass the original `input` payload to the chat parser.
func translateCursorRequest(from sdktranslator.Format, model string, payload []byte, stream bool) []byte {
	if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		return responsesconverter.ConvertOpenAIResponsesRequestToOpenAIChatCompletions(
			model,
			bytes.Clone(payload),
			stream,
		)
	}
	return sdktranslator.TranslateRequest(
		from,
		sdktranslator.FormatOpenAI,
		model,
		bytes.Clone(payload),
		stream,
	)
}

type cursorToolConstraint struct {
	required bool
	name     string
}

func (c cursorToolConstraint) Validate(calls []cursorlib.ToolCall) error {
	if !c.required {
		return nil
	}
	if c.name == "" {
		if len(calls) == 0 {
			return fmt.Errorf("cursor: tool_choice required at least one declared tool call")
		}
		return nil
	}
	for _, call := range calls {
		if strings.EqualFold(strings.TrimSpace(call.Name), c.name) {
			return nil
		}
	}
	return fmt.Errorf("cursor: tool_choice required tool %s", c.name)
}

func applyCursorToolChoice(body []byte, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition) ([]cursorlib.ChatMessage, []cursorlib.ToolDefinition, cursorToolConstraint, error) {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.Exists() {
		return messages, tools, cursorToolConstraint{}, nil
	}
	if choice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(choice.String())) {
		case "", "auto":
			return messages, tools, cursorToolConstraint{}, nil
		case "none":
			return messages, nil, cursorToolConstraint{}, nil
		case "required", "any":
			if len(tools) == 0 {
				return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: tool_choice=%s requires tools", choice.String())
			}
			return appendCursorToolInstruction(messages, "At least one declared tool must be called before answering the user."), tools, cursorToolConstraint{required: true}, nil
		default:
			return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: unsupported tool_choice %q", choice.String())
		}
	}

	typ := strings.ToLower(strings.TrimSpace(choice.Get("type").String()))
	if typ == "auto" || typ == "" {
		return messages, tools, cursorToolConstraint{}, nil
	}
	if typ == "none" {
		return messages, nil, cursorToolConstraint{}, nil
	}
	if typ == "required" || typ == "any" {
		if len(tools) == 0 {
			return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: tool_choice=%s requires tools", typ)
		}
		return appendCursorToolInstruction(messages, "At least one declared tool must be called before answering the user."), tools, cursorToolConstraint{required: true}, nil
	}
	if typ == "allowed_tools" {
		selectors := choice.Get("tools")
		if !selectors.IsArray() || len(selectors.Array()) == 0 {
			return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: allowed_tools requires a non-empty tools array")
		}
		selected := make(map[string]struct{})
		for _, selector := range selectors.Array() {
			name := cursorToolChoiceName(selector)
			if name == "" {
				return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: allowed_tools selector is missing a tool name")
			}
			selected[strings.ToLower(name)] = struct{}{}
		}
		filtered := make([]cursorlib.ToolDefinition, 0, len(selected))
		for _, tool := range tools {
			if _, ok := selected[strings.ToLower(strings.TrimSpace(tool.Name))]; ok {
				filtered = append(filtered, tool)
			}
		}
		if len(filtered) != len(selected) {
			return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: allowed_tools selected an unknown tool")
		}
		mode := strings.ToLower(strings.TrimSpace(choice.Get("mode").String()))
		if mode == "" || mode == "auto" {
			return messages, filtered, cursorToolConstraint{}, nil
		}
		if mode != "required" {
			return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: unsupported allowed_tools mode %q", mode)
		}
		return appendCursorToolInstruction(messages, "At least one declared tool must be called before answering the user."), filtered, cursorToolConstraint{required: true}, nil
	}

	name := cursorToolChoiceName(choice)
	if name == "" {
		return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: named tool_choice is missing a tool name")
	}
	matched := make([]cursorlib.ToolDefinition, 0, 1)
	for _, tool := range tools {
		if strings.EqualFold(strings.TrimSpace(tool.Name), name) {
			matched = append(matched, tool)
			name = tool.Name
			break
		}
	}
	if len(matched) == 0 {
		return nil, nil, cursorToolConstraint{}, fmt.Errorf("cursor: tool_choice selected unknown tool %s", name)
	}
	instruction := fmt.Sprintf("Call the declared tool %s before producing a final answer.", name)
	return appendCursorToolInstruction(messages, instruction), matched, cursorToolConstraint{required: true, name: name}, nil
}

func cursorToolChoiceName(choice gjson.Result) string {
	for _, path := range []string{"function.name", "name"} {
		if name := strings.TrimSpace(choice.Get(path).String()); name != "" {
			return name
		}
	}
	typ := strings.ToLower(strings.TrimSpace(choice.Get("type").String()))
	if typ == "shell" || typ == "local_shell" || typ == "apply_patch" {
		return typ
	}
	return ""
}

func appendCursorToolInstruction(messages []cursorlib.ChatMessage, instruction string) []cursorlib.ChatMessage {
	out := append([]cursorlib.ChatMessage(nil), messages...)
	for i := range out {
		if out[i].Role != "system" {
			continue
		}
		if strings.TrimSpace(out[i].Content) == "" {
			out[i].Content = instruction
		} else {
			out[i].Content += "\n\n" + instruction
		}
		return out
	}
	return append([]cursorlib.ChatMessage{{Role: "system", Content: instruction}}, out...)
}

func resolveCursorConversationID(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, originalBody, translatedBody []byte) string {
	if explicit := cursorConversationIDFromPayload(originalBody); explicit != "" {
		return scopeCursorConversationID(auth, "cursor:"+explicit)
	}
	if explicit := cursorConversationIDFromPayload(translatedBody); explicit != "" {
		return scopeCursorConversationID(auth, "cursor:"+explicit)
	}
	if value := metadataString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return scopeCursorConversationID(auth, "execution:"+value)
	}
	if value := metadataString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return scopeCursorConversationID(auth, "execution:"+value)
	}
	if value := cursorSessionIDFromHeaders(opts.Headers); value != "" {
		return scopeCursorConversationID(auth, value)
	}
	if sourceFormatEqual(opts.SourceFormat, sdktranslator.FormatClaude) {
		if value := helps.ExtractClaudeCodeSessionID(ctx, originalBody, opts.Headers); value != "" {
			return scopeCursorConversationID(auth, "claude:"+value)
		}
	}
	// Plenty of clients send no identifier at all. Minting a fresh id for them
	// would hand Cursor a new prompt prefix on every turn, which costs a full
	// cache write each time, so fall back to what the conversation itself
	// looks like.
	if fingerprint := cursorlib.ConversationFingerprint(req.Model, originalBody, translatedBody); fingerprint != "" {
		return scopeCursorConversationID(auth, "content:"+fingerprint)
	}
	return uuid.NewString()
}

func cursorConversationIDFromPayload(body []byte) string {
	for _, path := range []string{"cursor_conversation_id", "metadata.cursor_conversation_id", "conversation.id", "conversation"} {
		value := gjson.GetBytes(body, path)
		if value.Type == gjson.String {
			if id := strings.TrimSpace(value.String()); id != "" {
				return id
			}
		}
	}
	return ""
}

func cursorSessionIDFromHeaders(headers http.Header) string {
	for _, name := range []string{"X-Claude-Code-Session-Id", "Session_id", "Session-Id", "Conversation_id"} {
		if id := strings.TrimSpace(headers.Get(name)); id != "" {
			return strings.ToLower(name) + ":" + id
		}
	}
	return ""
}

func scopeCursorConversationID(auth *cliproxyauth.Auth, sessionKey string) string {
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:cursor:"+authID+":"+sessionKey)).String()
}

func openCursorSessionWithRetries(ctx context.Context, creds cursorlib.AccountCredentials, model string, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, opts ...cursorlib.StartOption) (*cursorlib.Session, error) {
	session, err := openCursorSession(ctx, creds, model, messages, tools, opts...)
	if err == nil || !isCursorProviderError(err) {
		return session, err
	}
	return retryOpenCursorSession(ctx, creds, model, messages, tools, err, opts...)
}

func runCursorChatWithRetries(ctx context.Context, creds cursorlib.AccountCredentials, model string, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, opts ...cursorlib.StartOption) (*cursorlib.ChatResult, error) {
	result, err := cursorlib.RunChat(ctx, creds, model, messages, tools, opts...)
	if err == nil || !isCursorProviderError(err) {
		return result, err
	}
	return retryRunCursorChat(ctx, creds, model, messages, tools, err, opts...)
}

func trailingToolResults(messages []cursorlib.ChatMessage) []cursorlib.ToolResult {
	start := -1
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Role {
		case "tool":
			start = i
			continue
		case "assistant":
			if len(messages[i].ToolCalls) > 0 && start >= 0 {
				out := make([]cursorlib.ToolResult, 0, len(messages)-start)
				for _, msg := range messages[start:] {
					if msg.Role != "tool" || strings.TrimSpace(msg.ToolCallID) == "" {
						continue
					}
					out = append(out, cursorlib.ToolResult{
						ToolCallID: msg.ToolCallID,
						Name:       msg.Name,
						Content:    msg.Content,
					})
				}
				return out
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

// CountTokens returns a best-effort character-based estimate.
func (e *CursorExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = opts
	chars := len(req.Payload)
	tokens := chars / 4
	if tokens < 1 {
		tokens = 1
	}
	payload := fmt.Appendf(nil, `{"input_tokens":%d}`, tokens)
	return cliproxyexecutor.Response{Payload: payload}, nil
}

// HttpRequest is unused for Cursor Agent protocol.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	return nil, fmt.Errorf("cursor executor: HttpRequest is not supported for Agent Connect")
}

func (e *CursorExecutor) ensureCredentials(ctx context.Context, auth *cliproxyauth.Auth) (cursorlib.AccountCredentials, error) {
	creds := cursorlib.CredentialsFromMetadata(authMetadata(auth))
	if creds.AccessToken == "" {
		return creds, fmt.Errorf("cursor: auth missing access_token")
	}
	storage := &cursorauth.TokenStorage{
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		Expired:      stringFromMeta(authMetadata(auth), "expired"),
	}
	if !storage.NeedsRefresh() {
		return creds, nil
	}
	refreshed, err := e.svc.RefreshToken(ctx, creds.RefreshToken, creds.AuthClientID, creds.BaseURL)
	if err != nil {
		log.Warnf("cursor token refresh failed, using existing token: %v", err)
		return creds, nil
	}
	creds.AccessToken = refreshed.AccessToken
	creds.RefreshToken = refreshed.RefreshToken
	if auth != nil && auth.Metadata != nil {
		auth.Metadata["access_token"] = refreshed.AccessToken
		auth.Metadata["refresh_token"] = refreshed.RefreshToken
		if !refreshed.ExpiresAt.IsZero() {
			auth.Metadata["expired"] = refreshed.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	return creds, nil
}

func authMetadata(auth *cliproxyauth.Auth) map[string]any {
	if auth == nil {
		return nil
	}
	return auth.Metadata
}

func stringFromMeta(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func extractChatMessages(body []byte) ([]cursorlib.ChatMessage, error) {
	arr := gjson.GetBytes(body, "messages")
	if !arr.IsArray() || len(arr.Array()) == 0 {
		return nil, fmt.Errorf("cursor: request messages are required")
	}
	out := make([]cursorlib.ChatMessage, 0, len(arr.Array())+1)
	// Anthropic top-level `system` may still be present if a translator left it
	// behind, or when callers hit the executor with Claude-shaped JSON.
	if sys := extractTopLevelSystem(body); sys != "" {
		hasSystem := false
		for _, item := range arr.Array() {
			if item.Get("role").String() == "system" {
				hasSystem = true
				break
			}
		}
		if !hasSystem {
			out = append(out, cursorlib.ChatMessage{Role: "system", Content: sys})
		}
	}
	for _, item := range arr.Array() {
		role := item.Get("role").String()
		content := item.Get("content")
		text := ""
		var images []cursorlib.ChatImage
		if content.IsArray() {
			var parts []string
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" || part.Get("text").Exists() {
					parts = append(parts, part.Get("text").String())
					continue
				}
				if img, ok := extractInlineImage(part); ok {
					images = append(images, img)
				}
			}
			text = strings.Join(parts, "\n")
		} else {
			text = content.String()
		}
		if role == "" {
			continue
		}
		msg := cursorlib.ChatMessage{
			Role:       role,
			Content:    text,
			Name:       item.Get("name").String(),
			ToolCallID: item.Get("tool_call_id").String(),
			Images:     images,
		}
		if toolCalls := item.Get("tool_calls"); toolCalls.IsArray() {
			for _, tc := range toolCalls.Array() {
				argsRaw := tc.Get("function.arguments").String()
				var args map[string]any
				if argsRaw != "" {
					_ = json.Unmarshal([]byte(argsRaw), &args)
				}
				if args == nil {
					args = map[string]any{}
				}
				msg.ToolCalls = append(msg.ToolCalls, cursorlib.ToolCall{
					ID:        tc.Get("id").String(),
					Name:      tc.Get("function.name").String(),
					Arguments: args,
				})
			}
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cursor: no usable chat messages")
	}
	return out, nil
}

// extractInlineImage reads both content-block shapes clients send: Anthropic
// `image` blocks and OpenAI `image_url` data URLs.
func extractInlineImage(part gjson.Result) (cursorlib.ChatImage, bool) {
	switch part.Get("type").String() {
	case "image":
		source := part.Get("source")
		if !source.Exists() {
			return cursorlib.ChatImage{}, false
		}
		// Remote URLs cannot be inlined; Cursor needs the bytes.
		return cursorlib.DecodeInlineImage(source.Get("media_type").String(), source.Get("data").String())
	case "image_url":
		return cursorlib.DecodeInlineImage("", part.Get("image_url.url").String())
	default:
		return cursorlib.ChatImage{}, false
	}
}

func extractTools(body []byte) []cursorlib.ToolDefinition {
	arr := gjson.GetBytes(body, "tools")
	if !arr.IsArray() {
		return nil
	}
	out := make([]cursorlib.ToolDefinition, 0, len(arr.Array()))
	for _, item := range arr.Array() {
		typ := item.Get("type").String()
		if typ != "" && typ != "function" {
			continue
		}
		fn := item.Get("function")
		name := fn.Get("name").String()
		if name == "" {
			name = item.Get("name").String()
		}
		if name == "" {
			continue
		}
		var params map[string]any
		paramsRaw := fn.Get("parameters").Raw
		if paramsRaw == "" {
			paramsRaw = item.Get("parameters").Raw
		}
		// Anthropic-style tools (or partially translated payloads) use input_schema.
		if paramsRaw == "" {
			paramsRaw = fn.Get("input_schema").Raw
		}
		if paramsRaw == "" {
			paramsRaw = item.Get("input_schema").Raw
		}
		if paramsRaw != "" {
			_ = json.Unmarshal([]byte(paramsRaw), &params)
		}
		desc := fn.Get("description").String()
		if desc == "" {
			desc = item.Get("description").String()
		}
		out = append(out, cursorlib.ToolDefinition{
			Name:        name,
			Description: desc,
			Parameters:  params,
		})
	}
	return cursorlib.SanitizeToolDefinitions(out)
}

func prepareCursorTools(messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, max int) []cursorlib.ToolDefinition {
	prefer := cursorlib.PreferredToolNames(messages, tools)
	return cursorlib.CapToolDefinitions(tools, max, prefer)
}

func extractTopLevelSystem(body []byte) string {
	sys := gjson.GetBytes(body, "system")
	if !sys.Exists() {
		return ""
	}
	if sys.IsArray() {
		parts := make([]string, 0, len(sys.Array()))
		for _, part := range sys.Array() {
			if part.Type == gjson.String {
				if t := strings.TrimSpace(part.String()); t != "" {
					parts = append(parts, t)
				}
				continue
			}
			if t := strings.TrimSpace(part.Get("text").String()); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return strings.TrimSpace(sys.String())
}

func isCursorProviderError(err error) bool {
	if isCursorPolicyBlock(err) || isCursorRateLimited(err) {
		// A usage-policy refusal is permanent: retrying, shrinking tools or
		// switching wires cannot help, and every attempt is billed.
		return false
	}
	return isCursorTransientProviderError(err) || isCursorHardProviderError(err) ||
		(err != nil && strings.Contains(err.Error(), "resource_exhausted"))
}

// isCursorRateLimited detects account throttling. Replaying the same request
// only deepens the throttle; returning 429 lets the caller select another
// account without entering the provider retry path.
func isCursorRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "RATE_LIMIT_EXCEEDED") ||
		strings.Contains(msg, "Rate limit exceeded") ||
		strings.Contains(msg, "ERROR_RESOURCE_EXHAUSTED")
}

// isCursorPolicyBlock detects Anthropic's content-policy refusal, which Cursor
// forwards as invalid_argument/ERROR_OPENAI.
func isCursorPolicyBlock(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Request blocked by Anthropic") ||
		strings.Contains(msg, "cyber_policy_review") ||
		strings.Contains(msg, "Usage Policy")
}

// humanizeCursorError replaces the raw Connect trailer with something a client
// (and the agent reading it) can act on.
func humanizeCursorError(err error) error {
	switch {
	case isCursorPolicyBlock(err):
		return fmt.Errorf("cursor: upstream refused this request under Anthropic's usage policy (cyber content review). " +
			"Rephrase or remove the flagged content; retrying the same prompt will not help")
	case isCursorRateLimited(err):
		return statusErr{code: http.StatusTooManyRequests, msg: "cursor: account rate limit exceeded " +
			"(ERROR_PRO_USER_RATE_LIMIT_EXCEEDED). Wait for the window to reset or add another Cursor account"}
	}
	return err
}

// isCursorTransientProviderError is Cursor→model-provider connectivity failure.
// Same request shape can succeed after a short backoff.
func isCursorTransientProviderError(err error) bool {
	if err == nil || isCursorPolicyBlock(err) || isCursorRateLimited(err) {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "ERROR_PROVIDER_ERROR") || strings.Contains(msg, "providerStatusCode") {
		// Hard provider rejection (usually HTTP 400), not a blip.
		return false
	}
	return strings.Contains(msg, "ERROR_OPENAI") ||
		strings.Contains(msg, "Unable to reach the model provider") ||
		strings.Contains(msg, `"code":"unavailable"`)
}

// isCursorHardProviderError is a request-shape / provider 400 class failure.
func isCursorHardProviderError(err error) bool {
	if err == nil || isCursorPolicyBlock(err) || isCursorRateLimited(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "ERROR_PROVIDER_ERROR") ||
		strings.Contains(msg, "providerStatusCode") ||
		(strings.Contains(msg, "Provider Error") && strings.Contains(msg, "400"))
}

var cursorTransientBackoff = []time.Duration{time.Second, 3 * time.Second, 6 * time.Second}

// cursorRetryBudget caps how long the whole degrade ladder may run. Each tier
// is a fresh upstream turn, so an unbounded ladder used to leave Claude Code
// "thinking" for well over a minute before any token arrived.
const cursorRetryBudget = 40 * time.Second

func retryBudgetLeft(start time.Time) bool { return time.Since(start) < cursorRetryBudget }

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryRunCursorChat(ctx context.Context, creds cursorlib.AccountCredentials, model string, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, err error, opts ...cursorlib.StartOption) (*cursorlib.ChatResult, error) {
	start := time.Now()
	if isCursorTransientProviderError(err) {
		for i, delay := range cursorTransientBackoff {
			if !retryBudgetLeft(start) {
				break
			}
			log.Warnf("cursor: transient retry #%d (same tools=%d) after %v: %v", i+1, len(tools), delay, err)
			if sleepErr := sleepCtx(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
			result, nextErr := cursorlib.RunChat(ctx, creds, model, messages, tools, opts...)
			if nextErr == nil {
				return result, nil
			}
			err = nextErr
			if !isCursorTransientProviderError(err) {
				break
			}
		}
	}

	// A tool name that collides with a Cursor built-in is the most common hard
	// 400, so try aliasing before giving up any capability.
	if isCursorHardProviderError(err) && len(tools) > 0 && retryBudgetLeft(start) {
		log.Warnf("cursor: hard400 with %d tools; retrying with all tool names aliased: %v", len(tools), err)
		aliasOpts := append(append([]cursorlib.StartOption(nil), opts...), cursorlib.WithForceToolAlias())
		result, nextErr := cursorlib.RunChat(ctx, creds, model, messages, tools, aliasOpts...)
		if nextErr == nil {
			return result, nil
		}
		err = nextErr
	}

	return nil, err
}

func retryOpenCursorSession(ctx context.Context, creds cursorlib.AccountCredentials, model string, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, err error, opts ...cursorlib.StartOption) (*cursorlib.Session, error) {
	start := time.Now()
	if isCursorTransientProviderError(err) {
		for i, delay := range cursorTransientBackoff {
			if !retryBudgetLeft(start) {
				break
			}
			log.Warnf("cursor: transient retry #%d open (same tools=%d) after %v: %v", i+1, len(tools), delay, err)
			if sleepErr := sleepCtx(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
			session, nextErr := openCursorSession(ctx, creds, model, messages, tools, opts...)
			if nextErr == nil {
				return session, nil
			}
			err = nextErr
			if !isCursorTransientProviderError(err) {
				break
			}
		}
	}

	// A tool name that collides with a Cursor built-in is the most common hard
	// 400, so try aliasing before giving up any capability.
	if isCursorHardProviderError(err) && len(tools) > 0 && retryBudgetLeft(start) {
		log.Warnf("cursor: stream hard400 with %d tools; retrying with all tool names aliased: %v", len(tools), err)
		aliasOpts := append(append([]cursorlib.StartOption(nil), opts...), cursorlib.WithForceToolAlias())
		session, nextErr := openCursorSession(ctx, creds, model, messages, tools, aliasOpts...)
		if nextErr == nil {
			return session, nil
		}
		err = nextErr
	}

	return nil, err
}

// applyCursorSpeed maps Anthropic/cursor2api `speed` (fast|standard) onto a
// Cursor hyphen suffix so ResolveRequestedModel can pick the advertised variant.
func applyCursorSpeed(model string, body []byte) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	speed := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "speed").String()))
	if speed == "" {
		speed = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "metadata.anthropic_extensions.speed").String()))
	}
	lower := strings.ToLower(model)
	switch speed {
	case "fast":
		if strings.HasSuffix(lower, "-fast") || strings.Contains(lower, "-fast-") {
			return model
		}
		return model + "-fast"
	case "standard":
		if strings.HasSuffix(lower, "-standard") {
			return model
		}
		return model + "-standard"
	default:
		return model
	}
}

// applyCursorReasoningEffort maps OpenAI-style reasoning_effort (including values
// produced from Claude thinking.adaptive + output_config.effort) onto Cursor's
// hyphen suffix form so ResolveRequestedModel can attach catalog parameters.
func applyCursorReasoningEffort(model string, body []byte) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String()))
	switch effort {
	case "low", "medium", "high", "xhigh":
	case "auto", "max":
		effort = "high"
	default:
		return model
	}
	lower := strings.ToLower(model)
	if strings.Contains(lower, "-thinking") || strings.HasSuffix(lower, "-"+effort) {
		return model
	}
	if !strings.Contains(lower, "-thinking") {
		model += "-thinking"
	}
	return model + "-" + effort
}

// cursorUsagePayload renders Cursor's TurnEnded counters as OpenAI usage.
//
// Cursor's input_tokens is the complete processed input. OpenAI and Responses
// define cache details as subsets of that total, so keep the authoritative
// number intact here. The Anthropic translator separates cache read/write from
// uncached input when it maps this OpenAI-shaped usage block.
func cursorUsagePayload(input, output, cacheRead, cacheWrite, reasoning int64) map[string]any {
	usagePayload := map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"total_tokens":      input + output,
	}
	details := map[string]any{}
	if cacheRead > 0 {
		details["cached_tokens"] = cacheRead
	}
	if cacheWrite > 0 {
		details["cache_creation_tokens"] = cacheWrite
	}
	if len(details) > 0 {
		usagePayload["prompt_tokens_details"] = details
	}
	if reasoning > 0 {
		usagePayload["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return usagePayload
}

func buildOpenAIChatCompletion(model string, result *cursorlib.ChatResult) []byte {
	id := "chatcmpl-" + uuid.NewString()
	finish := result.FinishReason
	if finish == "" {
		finish = "stop"
	}
	message := map[string]any{
		"role":    "assistant",
		"content": result.Text,
	}
	if result.Thinking != "" {
		message["reasoning_content"] = result.Thinking
	}
	if len(result.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(result.ToolCalls))
		for _, tc := range result.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			if args == nil {
				args = []byte("{}")
			}
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(args),
				},
			})
		}
		message["tool_calls"] = calls
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
		"usage": cursorUsagePayload(result.InputTokens, result.OutputTokens, result.CacheReadTokens, result.CacheWriteTokens, result.ReasoningTokens),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":"marshal failed"}`)
	}
	return b
}
