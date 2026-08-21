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

// Cursor normally sends TurnEnded immediately behind the formal tool batch.
// Missing TurnEnded is expensive: downstream billing then reports cache_read=0
// and locally recounts the entire prompt. The wait applies only when the
// authoritative frame was absent from the segment, never to normal turns.
const cursorTrailingUsageWait = 2 * time.Second

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

	messages, err := extractCursorChatMessages(body, originalPayloadSource, from)
	if err != nil {
		return resp, cursorInvalidRequest(err)
	}
	tools := prepareCursorTools(messages, extractCursorTools(body, originalPayloadSource, from), 0)
	messages, tools, toolConstraint, err := applyCursorToolChoice(body, messages, tools)
	if err != nil {
		return resp, cursorInvalidRequest(err)
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
	if !toolConstraint.Satisfied(result.ToolCalls) {
		// Cursor Agent has no named-tool-choice field on AgentRunRequest. The
		// constraint is therefore best-effort steering, not a reason to discard a
		// valid model response after the upstream turn already completed.
		log.Warnf("cursor: upstream completed without satisfying advisory tool_choice %q; returning the successful response", toolConstraint.name)
	}
	fillMissingChatResultUsage(result, conversationID, messages, tools)
	cursorlib.RememberPromptTotal(conversationID,
		promptTotalOf(result.InputTokens, result.CacheReadTokens, result.CacheWriteTokens))

	outPayload := buildOpenAIChatCompletion(req.Model, result)
	reporter.Publish(ctx, cursorUsageDetail(result.InputTokens, result.OutputTokens,
		result.CacheReadTokens, result.CacheWriteTokens, result.ReasoningTokens))

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
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	body := translateCursorRequest(from, baseModel, req.Payload, true)

	upstreamModel := strings.TrimPrefix(baseModel, "cursor-")
	if upstreamModel == "" {
		upstreamModel = "default"
	}
	upstreamModel = applyCursorReasoningEffort(upstreamModel, body)
	upstreamModel = applyCursorSpeed(upstreamModel, body)
	resolved := cursorlib.ResolveRequestedModel(upstreamModel)
	body, _ = sjson.SetBytes(body, "model", resolved.ModelID)

	messages, err := extractCursorChatMessages(body, originalPayloadSource, from)
	if err != nil {
		return nil, cursorInvalidRequest(err)
	}
	tools := prepareCursorTools(messages, extractCursorTools(body, originalPayloadSource, from), 0)
	messages, tools, toolConstraint, err := applyCursorToolChoice(body, messages, tools)
	if err != nil {
		return nil, cursorInvalidRequest(err)
	}

	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return nil, err
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
		var generated strings.Builder

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

		// emitRaw forwards a pre-translated SSE frame verbatim, bypassing the
		// translator. Used for keep-alive pings (already in the target wire
		// format) that a chunk translator would silently drop.
		emitRaw := func(line []byte) bool {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: line}:
				return true
			case <-ctx.Done():
				return false
			}
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
		contentEmitted := false
		roleEmitted := false
		pendingDeltas := make([]map[string]any, 0, 4)
		emitRole := func() bool {
			if roleEmitted {
				return true
			}
			if !emitLine([]byte("data: " + string(roleChunk))) {
				return false
			}
			roleEmitted = true
			return true
		}
		emitDelta := func(delta map[string]any) error {
			if !emitRole() {
				return context.Canceled
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
			// Reasoning alone does not close the door on a retry. The recovery
			// paths all key off contentEmitted, and a turn that has only shown
			// its thinking so far can still be restarted; the cost is at worst
			// some repeated reasoning text, against never retrying again once
			// a thinking model starts.
			if !deltaIsReasoningOnly(delta) {
				contentEmitted = true
			}
			return nil
		}
		flushPending := func() error {
			for _, delta := range pendingDeltas {
				if errEmit := emitDelta(delta); errEmit != nil {
					return errEmit
				}
			}
			pendingDeltas = pendingDeltas[:0]
			return nil
		}
		iterate := func(active *cursorlib.Session) error {
			return active.IterSegment(ctx, func(ev cursorlib.StreamEvent) error {
				var delta map[string]any
				flush := contentEmitted
				switch ev.Type {
				case "text_delta":
					delta = map[string]any{"content": ev.Text}
					generated.WriteString(ev.Text)
					flush = flush || strings.TrimSpace(ev.Text) != ""
				case "thinking_delta":
					delta = map[string]any{"reasoning_content": ev.Text}
					generated.WriteString(ev.Text)
					// Reasoning goes out as it arrives. Holding it until the
					// first text or tool call left thinking models silent for
					// as long as they reasoned — measured first-token times of
					// 8 to 51 seconds, all of them equal to the total time
					// because the whole answer landed in one burst.
					flush = true
				case "tool_call":
					if ev.ToolCall == nil {
						return nil
					}
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
					flush = true
				case "usage_final":
					usageFinal = ev
					return nil
				case "heartbeat":
					// Official Claude ping (plus an SSE comment for other
					// clients) resets idle timers without injecting tokens.
					// Must bypass the translator, which drops unknown frames.
					if !emitRaw(cursorDownstreamPing(responseFormat)) {
						return context.Canceled
					}
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
				pendingDeltas = append(pendingDeltas, delta)
				if !flush {
					return nil
				}
				return flushPending()
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
		if errIter != nil && !contentEmitted && shouldRetryToolTailWithoutContinuation(session.UsedContinuation(), messages, errIter) {
			log.Warnf("cursor: continuation tool rebuild rejected; retrying from full history: %v", errIter)
			cursorlib.DropContinuation(conversationID)
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
		// A provider 400 that survives the ladders is almost always a dead
		// wire id: the catalog still lists the model but its provider rejects
		// every call (claude-opus-4-7 died upstream this way — every account,
		// even one-line prompts with no tools). This deliberately also covers
		// isRetryable:false, which the hard-error classifier excludes: the
		// retry is on a different wire, not a replay of the rejected one.
		if errIter != nil && isCursorProviderBadRequest(errIter) && !isCursorPolicyBlock(errIter) && !contentEmitted {
			if alt := cursorlib.AlternateUsableModelID(upstreamModel); alt != "" && !strings.EqualFold(alt, upstreamModel) {
				log.Warnf("cursor: hard 400 persists on %q; serving via sibling wire %q: %v", upstreamModel, alt, errIter)
				_ = session.Close()
				altSession, altErr := openCursorSessionWithRetries(ctx, creds, alt, messages, tools, startOpts...)
				if altErr == nil {
					session = altSession
					errIter = iterate(session)
				} else {
					errIter = altErr
				}
			}
		}
		// Empty-stream gate, deliberately after every retry ladder above so a
		// retry that comes back clean-but-empty is caught too. A stream that
		// ended without error yet delivered no text and no tool call gave the
		// client nothing — with no usage reported, gateways then count the
		// prompt locally and record rows like "389k in / 0 out". Tool calls
		// and text count as content; reasoning alone does not, a turn that
		// only thought out loud still answered nothing.
		if (errIter == nil || errors.Is(errIter, cursorlib.ErrEmptyTerminal)) &&
			!contentEmitted && toolIndex == 0 {
			log.Warnf("cursor: stream ended empty (usage in=%d out=%d read=%d write=%d); retrying once",
				usageFinal.InputTokens, usageFinal.OutputTokens,
				usageFinal.CacheReadTokens, usageFinal.CacheWriteTokens)
			_ = session.Close()
			rebuilt, rebuildErr := openCursorSessionWithRetries(ctx, creds, upstreamModel, messages, tools, startOpts...)
			if rebuildErr == nil {
				session = rebuilt
				pendingDeltas = pendingDeltas[:0]
				finishReason = "stop"
				usageFinal = cursorlib.StreamEvent{}
				errIter = iterate(session)
				if errIter == nil && !contentEmitted && toolIndex == 0 {
					// Handing back an empty stream would bill the client for a
					// reply that does not exist; say so instead.
					errIter = statusErr{code: http.StatusBadGateway,
						msg: "cursor: the model generated no output for this turn; retry or shorten the conversation"}
				}
			} else {
				errIter = rebuildErr
			}
		}
		if errIter != nil {
			errIter = humanizeCursorError(errIter)
			reporter.PublishFailure(ctx, errIter)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errIter}:
			case <-ctx.Done():
			}
			// A canceled SSE right after tool_use is Claude Code's normal
			// pattern — the tool results arrive on the next HTTP request.
			// Closing here killed the paused session, so every resume failed
			// and each tool round rebuilt into a fresh conversation: cache
			// gone, and the model replayed its opening ("answers twice").
			clientGone := errors.Is(errIter, context.Canceled) ||
				strings.Contains(errIter.Error(), "context canceled")
			if !(clientGone && session.AwaitingClientTools()) {
				_ = session.Close()
			}
			return
		}
		if !toolConstraint.Satisfied(observedToolCalls) {
			// Do not append an artificial stream error after text was already
			// delivered. Cursor has no exact wire equivalent for a named
			// tool_choice, so the request remains a best-effort constraint.
			log.Warnf("cursor: upstream stream completed without satisfying advisory tool_choice %q; preserving the completed stream", toolConstraint.name)
		}

		// TurnEnded can land after the segment loop has already stopped
		// reading, which leaves the counters sitting unread. Collect them if
		// they are there. A bounded 150 ms wait is long enough for the adjacent
		// frame while keeping tool latency effectively unchanged when a Cursor
		// version emits no TurnEnded at a pause.
		//
		// A usage_final seen on the event stream is the same record that
		// TakeUsageForSegment would return, so it is authoritative on its own.
		// Re-home only when the segment produced no usage_final event at all, or
		// when its counter set is empty despite a segment having run — both mean
		// the trailing frame never crossed the event channel.
		segmentID := session.SegmentID()
		if usageFinal.SegmentID > 0 {
			segmentID = usageFinal.SegmentID
		}
		if usageFinal.SegmentID == 0 && usageFinal.InputTokens == 0 && usageFinal.OutputTokens == 0 &&
			usageFinal.CacheReadTokens == 0 && usageFinal.CacheWriteTokens == 0 && usageFinal.ReasoningTokens == 0 {
			if late, ok := session.TakeUsageForSegment(segmentID, cursorTrailingUsageWait); ok {
				usageFinal = late
			}
		}
		// Without this the tool-calling step of every agent loop reports 0 in
		// and 0 out despite having sent a full prompt and produced a tool call.
		usageFinal = fillMissingCursorUsage(usageFinal, conversationID, messages, tools, generated.String(), observedToolCalls)
		// Feed billed turns into the ledger too, so estimates for the next
		// tool round split prefix from suffix against real numbers.
		cursorlib.RememberPromptTotal(conversationID,
			promptTotalOf(usageFinal.InputTokens, usageFinal.CacheReadTokens, usageFinal.CacheWriteTokens))
		if usageFinal.InputTokens > 0 || usageFinal.OutputTokens > 0 ||
			usageFinal.CacheReadTokens > 0 || usageFinal.CacheWriteTokens > 0 || usageFinal.ReasoningTokens > 0 {
			reporter.Publish(ctx, cursorUsageDetail(usageFinal.InputTokens, usageFinal.OutputTokens,
				usageFinal.CacheReadTokens, usageFinal.CacheWriteTokens, usageFinal.ReasoningTokens))
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
		if err == nil {
			if err = session.SubmitToolResults(results); err == nil {
				return session, nil
			}
			if errors.Is(err, cursorlib.ErrToolResumeRebuild) {
				log.Warnf("cursor: tool resume requires rebuild (upstream response ended); rebuilding turn from history")
			} else {
				log.Warnf("cursor: stream submit tool results failed (%v); rebuilding turn from history", err)
			}
			// ErrToolResumeRebuild can mean this request supplied only part of
			// a parallel batch. SubmitToolResults deliberately parks that
			// shared session so sibling requests can submit the remaining
			// results; closing it here would undo that protection and close the
			// sibling's event channel. Other submit failures still tear down the
			// unusable live run before rebuilding.
			if shouldCloseCursorSessionAfterToolSubmit(err) {
				_ = session.Close()
			}
		} else if errors.Is(err, cursorlib.ErrToolResumeRebuild) {
			log.Warnf("cursor: tool resume requires rebuild (session already torn down); rebuilding turn from history")
		} else {
			log.Warnf("cursor: stream resume tool session failed (%v); rebuilding turn from history", err)
		}
		// The structured assistant tool calls and tool result messages already
		// carry the complete continuation. Do not add a synthetic user message:
		// it changes role ordering and can make the provider reject the request.
		//
		// Keep the deterministic conversation ID on every rebuild. Rotating it
		// here orphans Cursor's server-side pending tool state and prevents the
		// checkpoint continuation cache from matching, forcing every subsequent
		// tool step to rewrite the full history (large input with zero terminal
		// output). A restart or cross-instance resume needs the stable ID for the
		// same reason.
	}
	return cursorlib.StartSession(ctx, creds, model, messages, tools, opts...)
}

func shouldCloseCursorSessionAfterToolSubmit(err error) bool {
	return err != nil && !errors.Is(err, cursorlib.ErrToolResumeRebuild)
}

// shouldRetryToolTailWithoutContinuation is the recovery for a dead Agent run
// whose stored checkpoint was replayed as a user-message tool continuation and
// then rejected by the provider (400 / resource_exhausted). Drop the
// checkpoint and rebuild from the client's full tool history once.
func shouldRetryToolTailWithoutContinuation(usedContinuation bool, messages []cursorlib.ChatMessage, err error) bool {
	if !usedContinuation || err == nil {
		return false
	}
	if !isCursorProviderBadRequest(err) || isCursorPolicyBlock(err) {
		return false
	}
	return len(trailingToolResults(messages)) > 0
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

func (c cursorToolConstraint) Satisfied(calls []cursorlib.ToolCall) bool {
	if !c.required {
		return true
	}
	if c.name == "" {
		return len(calls) > 0
	}
	for _, call := range calls {
		if strings.EqualFold(strings.TrimSpace(call.Name), c.name) {
			return true
		}
	}
	return false
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
				log.Warnf("cursor: ignoring tool_choice=%s because the request declares no tools", choice.String())
				return messages, tools, cursorToolConstraint{}, nil
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
			log.Warnf("cursor: ignoring tool_choice=%s because the request declares no tools", typ)
			return messages, tools, cursorToolConstraint{}, nil
		}
		return appendCursorToolInstruction(messages, "At least one declared tool must be called before answering the user."), tools, cursorToolConstraint{required: true}, nil
	}
	if typ == "allowed_tools" {
		selectors := choice.Get("tools")
		if !selectors.IsArray() || len(selectors.Array()) == 0 {
			// No exact Cursor wire equivalent for an empty allowed_tools. Keep
			// the request advisory instead of rejecting a normal request.
			log.Warnf("cursor: ignoring allowed_tools with an empty tools array")
			return messages, tools, cursorToolConstraint{}, nil
		}
		selected := make(map[string]struct{})
		for _, selector := range selectors.Array() {
			name := cursorToolChoiceName(selector)
			if name == "" {
				log.Warnf("cursor: ignoring allowed_tools selector without a tool name")
				continue
			}
			selected[strings.ToLower(name)] = struct{}{}
		}
		if len(selected) == 0 {
			return messages, nil, cursorToolConstraint{}, nil
		}
		filtered := make([]cursorlib.ToolDefinition, 0, len(selected))
		for _, tool := range tools {
			if _, ok := selected[strings.ToLower(strings.TrimSpace(tool.Name))]; ok {
				filtered = append(filtered, tool)
			}
		}
		// Some selected tools are not in the declared set (e.g. a Cursor native
		// tool name the client assumed was available). Cursor cannot faithfully
		// enforce an exact named tool, so fall back to the declared tools rather
		// than rejecting the request.
		if len(filtered) != len(selected) {
			log.Warnf("cursor: allowed_tools selected undeclared tools; keeping the request advisory with declared tools")
			return messages, tools, cursorToolConstraint{}, nil
		}
		mode := strings.ToLower(strings.TrimSpace(choice.Get("mode").String()))
		if mode == "" || mode == "auto" {
			return messages, filtered, cursorToolConstraint{}, nil
		}
		if mode != "required" {
			log.Warnf("cursor: ignoring unsupported allowed_tools mode %q (advisory)", mode)
			return messages, tools, cursorToolConstraint{}, nil
		}
		return appendCursorToolInstruction(messages, "At least one declared tool must be called before answering the user."), filtered, cursorToolConstraint{required: true}, nil
	}

	name := cursorToolChoiceName(choice)
	if name == "" {
		// Unknown tool_choice shape. Cursor has no exact equivalent, so degrade
		// to an advisory no-op instead of turning a normal request into a 400.
		log.Warnf("cursor: ignoring unrecognized tool_choice shape")
		return messages, tools, cursorToolConstraint{}, nil
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
		// Named tool_choice for a tool Cursor owns but that was not declared in
		// this request (e.g. `Read`) or that the client named differently. Keep
		// the request running; the constraint is advisory and cannot be honored
		// exactly on the Cursor wire anyway.
		log.Warnf("cursor: tool_choice requested undeclared tool %q; keeping the request advisory", name)
		return messages, tools, cursorToolConstraint{}, nil
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
	if errors.Is(err, cursorlib.ErrEmptyTerminal) || generatedNothing(result, err) {
		log.Warnf("cursor: retrying once after a turn that generated nothing")
		result, err = cursorlib.RunChat(ctx, creds, model, messages, tools, opts...)
		if generatedNothing(result, err) {
			// Returning the empty result would bill the client for a reply
			// that does not exist; say so instead.
			return nil, statusErr{code: http.StatusBadGateway,
				msg: "cursor: the model generated no output for this turn; retry or shorten the conversation"}
		}
	}
	// Provider bad-requests (even isRetryable:false) enter the ladder too:
	// its final tier retries on a sibling wire, not a replay of the rejected
	// request. Policy blocks stay out — a different wire cannot unblock them.
	if err == nil || isCursorPolicyBlock(err) ||
		!(isCursorProviderError(err) || isCursorProviderBadRequest(err)) {
		return result, err
	}
	return retryRunCursorChat(ctx, creds, model, messages, tools, err, opts...)
}

// generatedNothing reports a turn whose result carries no text, no reasoning
// and no tool call — nothing for the client, whatever the token counters say.
// Turns that fail with an error keep their own handling.
func generatedNothing(result *cursorlib.ChatResult, err error) bool {
	if err != nil || result == nil {
		return false
	}
	if len(result.ToolCalls) > 0 {
		return false
	}
	return strings.TrimSpace(result.Text) == "" && strings.TrimSpace(result.Thinking) == ""
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
						Kind:       msg.ToolKind,
						Namespace:  msg.Namespace,
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
	if auth != nil && auth.AuthKind() == cliproxyauth.AuthKindAPIKey {
		apiKey := ""
		baseURL := ""
		if auth.Attributes != nil {
			apiKey = strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeAPIKey])
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		}
		return cursorlib.CredentialsFromAPIKey(ctx, apiKey, baseURL, auth.Metadata)
	}

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
				if part.Get("type").String() == "text" || part.Get("type").String() == "input_text" || part.Get("text").Exists() {
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

func extractCursorChatMessages(body, original []byte, from sdktranslator.Format) ([]cursorlib.ChatMessage, error) {
	messages, err := extractChatMessages(body)
	if err != nil || !sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) || !gjson.ValidBytes(original) {
		return messages, err
	}
	descriptors := responseHistoryToolDescriptors(original)
	for messageIndex := range messages {
		message := &messages[messageIndex]
		for callIndex := range message.ToolCalls {
			call := &message.ToolCalls[callIndex]
			if descriptor, ok := descriptors[call.ID]; ok {
				call.Kind = descriptor.kind
				call.Namespace = descriptor.namespace
			}
		}
		if message.Role == "tool" {
			if descriptor, ok := descriptors[message.ToolCallID]; ok {
				message.ToolKind = descriptor.kind
				message.Namespace = descriptor.namespace
				if message.Name == "" {
					message.Name = descriptor.name
				}
			}
		}
	}
	return messages, nil
}

type cursorHistoryToolDescriptor struct {
	kind      string
	name      string
	namespace string
}

func responseHistoryToolDescriptors(body []byte) map[string]cursorHistoryToolDescriptor {
	out := map[string]cursorHistoryToolDescriptor{}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return out
	}
	input.ForEach(func(_, item gjson.Result) bool {
		kind := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			return true
		}
		descriptor := cursorHistoryToolDescriptor{name: strings.TrimSpace(item.Get("name").String()), namespace: strings.TrimSpace(item.Get("namespace").String())}
		switch kind {
		case "function_call":
			descriptor.kind = "function"
		case "custom_tool_call":
			descriptor.kind = "custom"
		case "shell_call":
			descriptor.kind, descriptor.name = "shell", "shell"
		case "local_shell_call":
			descriptor.kind, descriptor.name = "local_shell", "local_shell"
		case "apply_patch_call":
			descriptor.kind, descriptor.name = "apply_patch", "apply_patch"
		default:
			return true
		}
		out[callID] = descriptor
		return true
	})
	return out
}

// extractInlineImage reads the image block shapes that reach this boundary
// from Anthropic, Chat Completions and Responses translators. Cursor needs the
// bytes, so URL/file-id references remain non-inline instead of being silently
// mislabeled as image data.
func extractInlineImage(part gjson.Result) (cursorlib.ChatImage, bool) {
	typ := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
	switch typ {
	case "image":
		source := part.Get("source")
		if !source.Exists() {
			return decodeInlineImageFields(part)
		}
		return cursorlib.DecodeInlineImage(firstNonEmptyString(source.Get("media_type"), source.Get("mime_type")), firstNonEmptyString(source.Get("data"), source.Get("file_data"), source.Get("url")))
	case "image_url", "input_image":
		return decodeInlineImageFields(part)
	default:
		// Some gateways strip the type but preserve image_url/file_data.
		return decodeInlineImageFields(part)
	}
}

func decodeInlineImageFields(part gjson.Result) (cursorlib.ChatImage, bool) {
	mediaType := firstNonEmptyString(part.Get("media_type"), part.Get("mime_type"), part.Get("image_url.media_type"), part.Get("image_url.mime_type"))
	data := firstNonEmptyString(
		part.Get("image_url.url"),
		part.Get("image_url.data"),
		part.Get("image_url.file_data"),
		part.Get("image_url"),
		part.Get("file_data"),
		part.Get("data"),
		part.Get("url"),
	)
	return cursorlib.DecodeInlineImage(mediaType, data)
}

func firstNonEmptyString(values ...gjson.Result) string {
	for _, value := range values {
		if value.Type == gjson.String {
			if text := strings.TrimSpace(value.String()); text != "" {
				return text
			}
		}
	}
	return ""
}

func extractCursorTools(body, original []byte, from sdktranslator.Format) []cursorlib.ToolDefinition {
	tools := extractTools(body)
	if !sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) || !gjson.ValidBytes(original) {
		return tools
	}
	descriptors := responseToolDescriptors(original)
	for i := range tools {
		if descriptor, ok := descriptors[strings.ToLower(strings.TrimSpace(tools[i].Name))]; ok {
			tools[i].Kind = descriptor.kind
			tools[i].Namespace = descriptor.namespace
		}
	}
	return tools
}

type cursorResponseToolDescriptor struct {
	kind      string
	namespace string
}

func responseToolDescriptors(body []byte) map[string]cursorResponseToolDescriptor {
	out := map[string]cursorResponseToolDescriptor{}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return out
	}
	tools.ForEach(func(_, item gjson.Result) bool {
		kind := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if kind == "namespace" {
			namespace := strings.TrimSpace(item.Get("name").String())
			children := item.Get("tools")
			if children.IsArray() {
				children.ForEach(func(_, child gjson.Result) bool {
					childKind := strings.ToLower(strings.TrimSpace(child.Get("type").String()))
					if childKind == "" {
						childKind = "function"
					}
					name := strings.TrimSpace(child.Get("name").String())
					if name == "" {
						name = strings.TrimSpace(child.Get("function.name").String())
					}
					qualified := name
					if namespace != "" && !strings.HasPrefix(name, "mcp__") {
						qualified = namespace + "__" + name
					}
					out[strings.ToLower(qualified)] = cursorResponseToolDescriptor{kind: childKind, namespace: namespace}
					return true
				})
			}
			return true
		}
		if kind == "" {
			kind = "function"
		}
		name := strings.TrimSpace(item.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(item.Get("function.name").String())
		}
		if kind == "shell" || kind == "local_shell" || kind == "apply_patch" {
			name = kind
		}
		if name != "" {
			out[strings.ToLower(name)] = cursorResponseToolDescriptor{kind: kind}
		}
		return true
	})
	return out
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
			Kind:        "function",
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
	return isCursorTransientProviderError(err) || isCursorHardProviderError(err)
}

// isCursorRateLimited detects account throttling. Replaying the same request
// only deepens the throttle; returning 429 lets the caller select another
// account without entering the provider retry path.
func isCursorRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Cursor also wraps permanent downstream-provider rejections in a
	// resource_exhausted envelope. The typed provider reason takes precedence.
	if strings.Contains(msg, "error_provider_error") {
		return false
	}
	return strings.Contains(msg, "rate_limit_exceeded") ||
		strings.Contains(msg, "rate limit exceeded") ||
		strings.Contains(msg, "error_resource_exhausted") ||
		strings.Contains(msg, "resource_exhausted") ||
		strings.Contains(msg, "grpc status 8")
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
	case isCursorProviderBadRequest(err):
		return cursorInvalidRequest(fmt.Errorf("cursor model provider rejected the request: %w", err))
	case errors.Is(err, cursorlib.ErrEmptyTerminal):
		return statusErr{code: http.StatusBadGateway, msg: err.Error()}
	case isCursorRateLimited(err):
		return statusErr{code: http.StatusTooManyRequests, msg: "cursor: rate limit or upstream resource exhaustion. " +
			"Retry with another Cursor account or wait for the cooldown window"}
	case isCursorTransportReset(err):
		return statusErr{code: http.StatusBadGateway,
			msg: "cursor: upstream closed the connection mid-stream (transient); the request was retried and kept failing — try again"}
	}
	return err
}

func cursorInvalidRequest(err error) error {
	if err == nil {
		return nil
	}
	return statusErr{code: http.StatusBadRequest, msg: "invalid_request_error: " + err.Error()}
}

func isCursorProviderBadRequest(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	providerError := strings.Contains(msg, "error_provider_error") || strings.Contains(msg, "provider error")
	if !providerError {
		return false
	}
	return strings.Contains(msg, `"providerstatuscode":"400"`) ||
		strings.Contains(msg, `"providerstatuscode": "400"`) ||
		strings.Contains(msg, "providerstatuscode=400") ||
		strings.Contains(msg, "isretryable=false") ||
		strings.Contains(msg, `"isretryable":false`) ||
		strings.Contains(msg, `"isretryable": false`)
}

// isCursorTransientProviderError is Cursor→model-provider connectivity failure.
// Same request shape can succeed after a short backoff.
func isCursorTransientProviderError(err error) bool {
	if err == nil || isCursorPolicyBlock(err) || isCursorRateLimited(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "error_provider_error") || strings.Contains(msg, "providerstatuscode") {
		// Hard provider rejection (usually HTTP 400), not a blip.
		return false
	}
	return strings.Contains(msg, "error_openai") ||
		strings.Contains(msg, "unable to reach the model provider") ||
		strings.Contains(msg, `"code":"unavailable"`) ||
		isCursorTransportReset(err)
}

// isCursorTransportReset detects the transport tearing down under the Connect
// stream rather than Cursor answering with an error envelope: the server (or
// its load balancer) sends RST_STREAM / GOAWAY / a TCP reset, and Go's HTTP/2
// client surfaces strings like "stream error: stream ID 1; INTERNAL_ERROR;
// received from peer". Nothing about the request caused it — request-shape
// problems come back as error envelopes, not stream resets — so it retries
// like any other connectivity blip instead of reaching the client raw.
func isCursorTransportReset(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "stream error") && strings.Contains(msg, "received from peer") {
		return true
	}
	return strings.Contains(msg, "http2: server sent goaway") ||
		strings.Contains(msg, "http2: response body closed") ||
		strings.Contains(msg, "response body closed") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "unexpected eof")
}

// isCursorHardProviderError is a request-shape / provider 400 class failure.
func isCursorHardProviderError(err error) bool {
	if err == nil || isCursorPolicyBlock(err) || isCursorRateLimited(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if isCursorProviderBadRequest(err) && (strings.Contains(msg, `"isretryable":false`) ||
		strings.Contains(msg, `"isretryable": false`) ||
		strings.Contains(msg, "isretryable=false")) {
		return false
	}
	return strings.Contains(msg, "error_provider_error") ||
		strings.Contains(msg, "providerstatuscode") ||
		(strings.Contains(msg, "provider error") && strings.Contains(msg, "400"))
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

	// A provider 400 that survives everything above is almost always a dead
	// wire id — the catalog lists the model but its provider rejects every
	// call (claude-opus-4-7 died upstream this way). This deliberately also
	// covers isRetryable:false, which the hard-error classifier excludes: the
	// retry is on a different wire, not a replay of the rejected one.
	if isCursorProviderBadRequest(err) && !isCursorPolicyBlock(err) && retryBudgetLeft(start) {
		if len(trailingToolResults(messages)) > 0 {
			if id := cursorlib.ConversationIDFromStartOptions(opts); id != "" {
				log.Warnf("cursor: continuation tool rebuild rejected; retrying from full history: %v", err)
				cursorlib.DropContinuation(id)
				result, nextErr := cursorlib.RunChat(ctx, creds, model, messages, tools, opts...)
				if nextErr == nil {
					return result, nil
				}
				err = nextErr
			}
		}
		if alt := cursorlib.AlternateUsableModelID(model); alt != "" && !strings.EqualFold(alt, model) {
			log.Warnf("cursor: hard 400 persists on %q; serving via sibling wire %q: %v", model, alt, err)
			result, nextErr := cursorlib.RunChat(ctx, creds, alt, messages, tools, opts...)
			if nextErr == nil {
				return result, nil
			}
			err = nextErr
		}
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

// cursorDownstreamPing is the keep-alive frame written while Cursor is silent.
// Claude Code only reliably resets its stream idle timer on an official
// `event: ping`; SSE comments are still included for OpenAI-compatible clients
// and for proxies that key off any bytes.
func cursorDownstreamPing(format sdktranslator.Format) []byte {
	name := strings.ToLower(strings.TrimSpace(format.String()))
	switch name {
	case "claude", "anthropic":
		return []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	default:
		return []byte(": keep-alive (cursor upstream thinking)\n\n")
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
// cursorUsageDetail splits Cursor's counters into the shape the rest of the
// proxy accounts in: input is the uncached remainder, with cache read and cache
// write in their own buckets.
//
// Cursor reports an input figure that already contains both cache buckets
// (measured: prompt_tokens 25249 = cached 25117 + written 130 + 2 fresh), so
// publishing it verbatim alongside the buckets made every consumer that sums
// them — the relay's own total among them — count the cache twice. A long agent
// session then showed millions of input tokens for a few hundred output.
// deltaIsReasoningOnly reports whether a streamed chunk carries nothing but
// the model's thinking.
func deltaIsReasoningOnly(delta map[string]any) bool {
	if len(delta) != 1 {
		return false
	}
	_, ok := delta["reasoning_content"]
	return ok
}

// splitCursorInput decides how much of a reported input figure is genuinely
// uncached (fresh) input. Cursor's counters follow two different shapes that
// share the same TurnEnded fields:
//
//   - Own-account Cursor: input_tokens already contains the cache buckets
//     (measured: prompt_tokens 25249 = cached 25117 + written 130 + 2 fresh).
//     Subtracting the buckets leaves the true fresh remainder.
//   - Anthropic-official / some cross-account and third-party upstreams:
//     input_tokens is the fresh portion only, with cache read/write reported
//     as separate buckets parallel to it.
//
// The shape is distinguishable locally: if the cache buckets fit inside the
// reported input, the input is inclusive; if they exceed it (or the input is
// smaller than their sum), they are separate and the input is already fresh.
// This removes the hard-coded assumption that cost the updated statistics:
// subtracting parallel buckets when they do not live inside the input drove the
// uncached figure to zero on the inclusive path's opposite.
func splitCursorInput(input, cacheRead, cacheWrite int64) int64 {
	if cacheRead+cacheWrite <= input {
		return input - cacheRead - cacheWrite
	}
	return input
}

func cursorUsageDetail(input, output, cacheRead, cacheWrite, reasoning int64) usage.Detail {
	uncached := splitCursorInput(input, cacheRead, cacheWrite)
	if uncached < 0 {
		uncached = 0
	}
	return usage.Detail{
		InputTokens:         uncached,
		OutputTokens:        output,
		CachedTokens:        cacheRead,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheWrite,
		ReasoningTokens:     reasoning,
		TotalTokens:         uncached + output + cacheRead + cacheWrite,
	}
}

// cursorUsagePayload renders usage for OpenAI-shaped responses.
//
// prompt_tokens carries only the uncached remainder, with the cache in its own
// details. The OpenAI spec defines prompt_tokens as inclusive of cached_tokens,
// but downstream gateways read prompt_tokens as the billable input and show the
// cache separately: reporting the inclusive figure made every request look like
// tens of thousands of fresh input tokens with no cache hit, and priced it that
// way. The Anthropic surface already reports the split; this matches it.
func cursorUsagePayload(input, output, cacheRead, cacheWrite, reasoning int64) map[string]any {
	uncached := splitCursorInput(input, cacheRead, cacheWrite)
	if uncached < 0 {
		uncached = 0
	}
	usagePayload := map[string]any{
		"prompt_tokens":     uncached,
		"completion_tokens": output,
		"total_tokens":      uncached + output + cacheRead + cacheWrite,
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
