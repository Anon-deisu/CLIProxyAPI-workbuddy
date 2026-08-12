package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// MCPProviderIdentifier is the provider_identifier advertised for OpenAI tools.
const MCPProviderIdentifier = "cliproxyapi"

// ToolDefinition is an OpenAI-style function tool projected into Cursor MCP tools.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// ToolResult is a client-provided tool response.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

// StreamEvent is one Cursor→OpenAI segment event.
type StreamEvent struct {
	Type             string
	Text             string
	ToolCall         *ToolCall
	Reason           string
	Message          string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

type pendingExec struct {
	request *agentv1.ExecServerMessage
	call    ToolCall
}

// Session is a live Agent Connect run that can pause for client tools.
type Session struct {
	ID             string
	ConversationID string
	Model          string

	mu           sync.Mutex
	stream       *BidiStream
	blobStore    map[string][]byte
	tools        []ToolDefinition
	toolIndex    map[string]*ToolDefinition
	toolAlias    *ToolAliasMap
	pending      map[string]*pendingExec
	events       chan StreamEvent
	errCh        chan error
	closed       bool
	waitingTools bool
	pauseCh      chan struct{}
	// responseEnded is set when Connect END_STREAM/EOF arrived while paused
	// for client tools. The request side may still accept mcp_result writes,
	// but no further InteractionUpdates will arrive — resume must rebuild.
	responseEnded bool
	cancel        context.CancelFunc
	lastActivity  time.Time
	manager       *SessionManager

	// Checkpoint / text recovery (cursor2api): Cursor wire text_deltas often
	// miss a leading token; the authoritative assistant text lands in a later
	// conversation_checkpoint_update. We buffer deltas and flush once.
	conversationState   *agentv1.ConversationStateStructure
	visibleText         strings.Builder // all text seen from deltas
	emittedText         strings.Builder // text already sent to the client
	visibleThinking     strings.Builder // reasoning seen from deltas
	emittedThinking     strings.Builder // reasoning already sent to the client
	seenAssistantMsgIDs map[string]struct{}
	turnEndedSeen       bool
	segmentEnded        bool
	textFlushed         bool
	thinkingFlushed     bool
	resumed             bool
	// lastFrame is the last time anything arrived from Cursor. Thinking and
	// text deltas are buffered rather than emitted, so consumer-side idle
	// detection must not rely on client-visible events alone.
	lastFrame time.Time
}

// ChatResult is the collected text response from one Agent segment / run.
type ChatResult struct {
	Text             string
	Thinking         string
	ToolCalls        []ToolCall
	FinishReason     string
	ConversationID   string
	SessionID        string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// StartOption tweaks how a Cursor Agent run is opened.
type StartOption func(*startConfig)

type startConfig struct {
	forceToolAlias bool
}

// WithForceToolAlias prefixes every client tool name before sending it to
// Cursor. Used as a fallback when a provider 400 suggests an unknown built-in
// tool name collision.
func WithForceToolAlias() StartOption {
	return func(c *startConfig) { c.forceToolAlias = true }
}

// StartSession opens a new Agent run for the given messages/tools.
func StartSession(ctx context.Context, creds AccountCredentials, model string, messages []ChatMessage, tools []ToolDefinition, opts ...StartOption) (*Session, error) {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("cursor: access_token is required")
	}
	cfg := startConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	tools = SanitizeToolDefinitions(tools)
	tools, toolAlias := AliasToolsForCursor(tools, cfg.forceToolAlias)
	if !toolAlias.Empty() {
		messages = ApplyToolAliasToMessages(messages, toolAlias)
		log.Debugf("cursor: renamed %d tool(s) that collide with Cursor built-ins", len(toolAlias.toClient))
	}
	selection := ResolveRequestedModel(model)
	clientMsg, blobStore, conversationID, err := buildRunRequest(model, messages, tools)
	if err != nil {
		return nil, err
	}
	first, err := proto.Marshal(clientMsg)
	if err != nil {
		return nil, err
	}

	profile := ProfileFromCredentials(creds)
	requestID := uuid.NewString()
	headers, err := profile.Headers(creds.AccessToken, requestID, "")
	if err != nil {
		return nil, err
	}
	headers["x-original-request-id"] = requestID

	// Detach the Agent H2 stream from the inbound HTTP request context.
	// Tool round-trips span multiple client requests; cancelling with the
	// first response would close the pipe before mcp_result can be written.
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	stream, err := OpenAgentRun(runCtx, creds.BaseURL, headers, first)
	if err != nil {
		cancel()
		return nil, err
	}
	if profile.CookieJar != nil {
		profile.CookieJar.RememberResponse(creds.BaseURL, stream.ResponseHeader())
	}

	session := &Session{
		ID:                  uuid.NewString(),
		ConversationID:      conversationID,
		Model:               selection.ModelID,
		stream:              stream,
		blobStore:           blobStore,
		tools:               append([]ToolDefinition(nil), tools...),
		toolIndex:           indexTools(tools),
		toolAlias:           toolAlias,
		pending:             map[string]*pendingExec{},
		events:              make(chan StreamEvent, 256),
		errCh:               make(chan error, 1),
		pauseCh:             make(chan struct{}),
		cancel:              cancel,
		lastActivity:        time.Now(),
		manager:             DefaultSessionManager(),
		seenAssistantMsgIDs: map[string]struct{}{},
	}
	session.seedSeenAssistantMessages()
	session.manager.Register(session)
	go session.heartbeatLoop(runCtx)
	go session.readLoop(runCtx)
	return session, nil
}

func indexTools(tools []ToolDefinition) map[string]*ToolDefinition {
	out := make(map[string]*ToolDefinition, len(tools))
	for i := range tools {
		name := strings.TrimSpace(tools[i].Name)
		if name == "" {
			continue
		}
		out[name] = &tools[i]
	}
	return out
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *Session) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			closed := s.closed
			stream := s.stream
			s.mu.Unlock()
			if closed || stream == nil {
				return
			}
			hb := &agentv1.AgentClientMessage{
				Message: &agentv1.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &agentv1.ClientHeartbeat{}},
			}
			payload, err := proto.Marshal(hb)
			if err != nil {
				continue
			}
			_ = stream.WriteEnvelope(payload, false)
		}
	}
}

func (s *Session) readLoop(ctx context.Context) {
	defer close(s.events)
	decoder := NewDecoder()
	if s.stream != nil {
		if enc := strings.TrimSpace(s.stream.ResponseHeader().Get("Connect-Content-Encoding")); enc != "" {
			decoder.SetCompression(enc)
		} else if enc := strings.TrimSpace(s.stream.ResponseHeader().Get("connect-content-encoding")); enc != "" {
			decoder.SetCompression(enc)
		}
	}
	readBuf := make([]byte, 32*1024)
	for {
		s.mu.Lock()
		waiting := s.waitingTools
		closed := s.closed
		stream := s.stream
		pauseCh := s.pauseCh
		s.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-ctx.Done():
			// runCtx is normally independent of the HTTP request; if it is
			// canceled while tools are pending, keep the session indexed.
			if s.awaitingClientTools() {
				log.Warnf("cursor: run context canceled while awaiting tools; keeping session (pending=%d)", s.pendingToolCount())
				select {
				case <-pauseCh:
					continue
				case <-time.After(10 * time.Minute):
					_ = s.Close()
					return
				}
			}
			s.mu.Lock()
			alreadyClosed := s.closed
			s.mu.Unlock()
			if !alreadyClosed {
				s.emit(StreamEvent{Type: "error", Message: ctx.Err().Error()})
				s.emit(StreamEvent{Type: "segment_end", Reason: "error"})
			}
			return
		default:
		}
		if waiting {
			select {
			case <-ctx.Done():
				if s.awaitingClientTools() {
					log.Warnf("cursor: run context canceled while paused for tools; keeping session (pending=%d)", s.pendingToolCount())
					select {
					case <-pauseCh:
						continue
					case <-time.After(10 * time.Minute):
						_ = s.Close()
						return
					}
				}
				return
			case <-pauseCh:
				continue
			}
		}

		n, errRead := stream.Read(readBuf)
		if n > 0 {
			s.mu.Lock()
			s.lastFrame = time.Now()
			s.mu.Unlock()
			envelopes, errFeed := decoder.Feed(readBuf[:n])
			if errFeed != nil {
				s.fail(errFeed)
				return
			}
			endForTools := false
			sawEndStream := false
			for _, env := range envelopes {
				if env.EndStream() {
					if len(env.Payload) > 0 {
						var trailer struct {
							Error json.RawMessage `json:"error"`
						}
						_ = json.Unmarshal(env.Payload, &trailer)
						if len(trailer.Error) > 0 && string(trailer.Error) != "null" {
							s.fail(fmt.Errorf("cursor connect end-stream error: %s", string(trailer.Error)))
							return
						}
					}
					// Keep scanning this batch / briefly reading: checkpoint
					// frames often arrive after the Connect end-stream flag.
					sawEndStream = true
					continue
				}
				serverMsg := &agentv1.AgentServerMessage{}
				if err := proto.Unmarshal(env.Payload, serverMsg); err != nil {
					head := env.Payload
					if len(head) > 32 {
						head = head[:32]
					}
					log.Warnf("cursor: decode server message failed len=%d head=%x: %v", len(env.Payload), head, err)
					s.fail(fmt.Errorf("cursor decode server message: %w", err))
					return
				}
				pause, err := s.handleServerMessage(serverMsg)
				if err != nil {
					s.fail(err)
					return
				}
				if pause {
					endForTools = true
				}
			}
			if endForTools {
				s.flushAssistantText(true)
				s.beginWaitingTools()
				s.mu.Lock()
				s.segmentEnded = true
				s.mu.Unlock()
				s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
			}
			if sawEndStream {
				// Cursor often half-closes the Connect response when pausing for
				// client tools. Do NOT Close() yet — that drops BindPending and
				// forces a blind rebuild — but mark responseEnded so
				// SubmitToolResults refuses same-stream resume (reading after
				// END_STREAM blocks forever → Claude Code "Cascading…").
				if s.awaitingClientTools() {
					s.mu.Lock()
					s.responseEnded = true
					s.mu.Unlock()
					log.Warnf("cursor: response END_STREAM while awaiting tools (pending=%d); will rebuild after tool results", s.pendingToolCount())
					continue
				}
				s.flushAssistantText(true)
				s.mu.Lock()
				alreadyEnded := s.segmentEnded
				s.segmentEnded = true
				s.mu.Unlock()
				if !alreadyEnded {
					s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
				}
				_ = s.Close()
				return
			}
		}
		if errRead == io.EOF {
			if s.awaitingClientTools() {
				s.mu.Lock()
				s.responseEnded = true
				s.mu.Unlock()
				log.Warnf("cursor: EOF while awaiting client tool results (pending=%d); will rebuild after tool results", s.pendingToolCount())
				s.beginWaitingTools()
				continue
			}
			s.flushAssistantText(true)
			s.mu.Lock()
			alreadyEnded := s.segmentEnded
			s.segmentEnded = true
			s.mu.Unlock()
			if !alreadyEnded {
				s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
			}
			_ = s.Close()
			return
		}
		if errRead != nil {
			s.fail(errRead)
			return
		}
		if n == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (s *Session) beginWaitingTools() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waitingTools {
		return
	}
	s.waitingTools = true
	s.pauseCh = make(chan struct{})
}

// awaitingClientTools reports whether the Agent turn is paused for mcp_result.
func (s *Session) awaitingClientTools() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitingTools || len(s.pending) > 0
}

// AwaitingClientTools is the exported form used by the executor when the
// inbound HTTP/SSE context is canceled after tool_use.
func (s *Session) AwaitingClientTools() bool { return s.awaitingClientTools() }

func (s *Session) pendingToolCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// pending may hold both native and sanitized ids for the same exec.
	seen := map[*pendingExec]struct{}{}
	for _, p := range s.pending {
		if p != nil {
			seen[p] = struct{}{}
		}
	}
	return len(seen)
}

func (s *Session) resumeReading() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.waitingTools {
		return
	}
	s.waitingTools = false
	close(s.pauseCh)
	s.pauseCh = make(chan struct{})
}

func (s *Session) emit(ev StreamEvent) {
	s.touch()
	// Never drop deltas: a full buffer previously timed out and silently
	// discarded leading text/thinking, which showed up as mid-sentence replies.
	select {
	case s.events <- ev:
		return
	default:
	}
	select {
	case s.events <- ev:
	case <-time.After(30 * time.Second):
		log.Warnf("cursor: event delivery stalled; dropping %s", ev.Type)
	}
}

func (s *Session) fail(err error) {
	if err == nil {
		return
	}
	select {
	case s.errCh <- err:
	default:
	}
	s.emit(StreamEvent{Type: "error", Message: err.Error()})
	s.emit(StreamEvent{Type: "segment_end", Reason: "error"})
	_ = s.Close()
}

// Events returns the live event stream for the current segment consumer.
func (s *Session) Events() <-chan StreamEvent { return s.events }

// CollectSegment drains events until segment_end.
func (s *Session) CollectSegment(ctx context.Context) (*ChatResult, error) {
	result := &ChatResult{
		ConversationID: s.ConversationID,
		SessionID:      s.ID,
		FinishReason:   "stop",
	}
	var text strings.Builder
	var thinking strings.Builder
	sawEvent := false
	idle := time.NewTimer(s.firstEventTimeout())
	defer idle.Stop()
	for {
		select {
		case <-idle.C:
			if s.awaitingClientTools() || s.upstreamActiveWithin(streamIdleTimeout) {
				resetIdleTimer(idle, streamIdleTimeout)
				continue
			}
			if !sawEvent && s.wasResumed() {
				log.Warnf("cursor: resumed run produced no events within %s; rebuilding turn from history", resumeFirstEventTimeout)
				_ = s.Close()
				return nil, ErrToolResumeRebuild
			}
			log.Warnf("cursor: no upstream event for %s; aborting run", streamIdleTimeout)
			_ = s.Close()
			return nil, fmt.Errorf("cursor: upstream idle for %s", streamIdleTimeout)
		case <-ctx.Done():
			if !s.awaitingClientTools() {
				_ = s.Close()
			}
			return nil, ctx.Err()
		case err := <-s.errCh:
			if err != nil {
				return nil, err
			}
		case ev, ok := <-s.events:
			if !ok {
				result.Text = text.String()
				result.Thinking = thinking.String()
				return result, nil
			}
			sawEvent = true
			resetIdleTimer(idle, streamIdleTimeout)
			switch ev.Type {
			case "text_delta":
				text.WriteString(ev.Text)
			case "thinking_delta":
				thinking.WriteString(ev.Text)
			case "tool_call":
				if ev.ToolCall != nil {
					result.ToolCalls = append(result.ToolCalls, *ev.ToolCall)
				}
			case "usage_final":
				result.InputTokens = ev.InputTokens
				result.OutputTokens = ev.OutputTokens
				result.CacheReadTokens = ev.CacheReadTokens
				result.CacheWriteTokens = ev.CacheWriteTokens
				result.ReasoningTokens = ev.ReasoningTokens
			case "error":
				return nil, fmt.Errorf("cursor: %s", ev.Message)
			case "segment_end":
				result.Text = text.String()
				result.Thinking = thinking.String()
				if ev.Reason != "" {
					result.FinishReason = ev.Reason
				}
				if result.FinishReason == "tool_calls" && len(result.ToolCalls) == 0 {
					result.FinishReason = "stop"
				}
				return result, nil
			}
		}
	}
}

// streamIdleTimeout bounds how long a segment may go without any upstream
// event. Cursor occasionally leaves a run open without ever completing it,
// which used to stall clients until their own (10 minute) timeout.
const streamIdleTimeout = 90 * time.Second

// resumeFirstEventTimeout bounds how long a resumed run may stay silent. Cursor
// frequently accepts the mcp_result write and then never continues that run;
// waiting out streamIdleTimeout turned those into 90s stalls and 500s.
const resumeFirstEventTimeout = 20 * time.Second

func (s *Session) firstEventTimeout() time.Duration {
	if s.wasResumed() {
		return resumeFirstEventTimeout
	}
	return streamIdleTimeout
}

func (s *Session) wasResumed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumed
}

// upstreamActiveWithin reports whether Cursor sent anything recently, even if
// nothing was surfaced to the client (long thinking phases buffer silently).
func (s *Session) upstreamActiveWithin(d time.Duration) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.lastFrame.IsZero() && time.Since(s.lastFrame) < d
}

func resetIdleTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// IterSegment invokes fn for each event until segment_end (inclusive).
func (s *Session) IterSegment(ctx context.Context, fn func(StreamEvent) error) error {
	sawEvent := false
	idle := time.NewTimer(s.firstEventTimeout())
	defer idle.Stop()
	for {
		select {
		case <-idle.C:
			if s.awaitingClientTools() || s.upstreamActiveWithin(streamIdleTimeout) {
				resetIdleTimer(idle, streamIdleTimeout)
				continue
			}
			if !sawEvent && s.wasResumed() {
				// A resumed run that never produces an event is dead; rebuild
				// from history instead of burning the full idle timeout.
				log.Warnf("cursor: resumed run produced no events within %s; rebuilding turn from history", resumeFirstEventTimeout)
				_ = s.Close()
				return ErrToolResumeRebuild
			}
			log.Warnf("cursor: no upstream event for %s; aborting segment", streamIdleTimeout)
			_ = s.Close()
			return fmt.Errorf("cursor: upstream idle for %s", streamIdleTimeout)
		case <-ctx.Done():
			// Keep tool-paused sessions alive across the inbound HTTP cancel
			// (clients disconnect SSE after tool_use and resume on the next request).
			if !s.awaitingClientTools() {
				_ = s.Close()
			}
			return ctx.Err()
		case err := <-s.errCh:
			if err != nil {
				return err
			}
		case ev, ok := <-s.events:
			if !ok {
				return nil
			}
			sawEvent = true
			resetIdleTimer(idle, streamIdleTimeout)
			if err := fn(ev); err != nil {
				return err
			}
			if ev.Type == "segment_end" {
				return nil
			}
			if ev.Type == "error" {
				return fmt.Errorf("cursor: %s", ev.Message)
			}
		}
	}
}

// ErrToolResumeRebuild tells the executor to start a fresh Agent turn from
// history instead of reading more events on a half-closed Connect stream.
var ErrToolResumeRebuild = fmt.Errorf("cursor: tool resume requires rebuild")

// SubmitToolResults replies on the live exec stream and resumes reading.
func (s *Session) SubmitToolResults(results []ToolResult) error {
	if s == nil {
		return fmt.Errorf("cursor: nil session")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("cursor: session closed")
	}
	responseEnded := s.responseEnded
	pending := make([]*pendingExec, 0, len(results))
	ids := make([]string, 0, len(results))
	for _, result := range results {
		item, id := s.lookupPendingTool(result.ToolCallID)
		if item == nil {
			s.mu.Unlock()
			return fmt.Errorf("cursor: no pending tool call %s", result.ToolCallID)
		}
		pending = append(pending, item)
		ids = append(ids, id)
	}
	s.mu.Unlock()

	if responseEnded {
		// Best-effort deliver results, then force history rebuild. Continuing
		// to Read on an END_STREAM response stalls the client indefinitely.
		for i, result := range results {
			if pending[i].request != nil && s.stream != nil {
				_ = s.sendMcpResult(pending[i].request, result)
			}
			s.releasePendingExec(pending[i], ids[i], results[i].ToolCallID)
		}
		_ = s.Close()
		return ErrToolResumeRebuild
	}

	for i, result := range results {
		item := pending[i]
		if err := s.sendMcpResult(item.request, result); err != nil {
			// Release remaining pending indexes so a rebuild does not keep
			// stale multi-tool bindings that block later ResolveForToolResults.
			for j := i; j < len(pending); j++ {
				s.releasePendingExec(pending[j], ids[j], results[j].ToolCallID)
			}
			return fmt.Errorf("cursor: submit tool result %s: %w", ids[i], err)
		}
		s.releasePendingExec(item, ids[i], result.ToolCallID)
	}
	s.mu.Lock()
	s.segmentEnded = false
	s.textFlushed = false
	s.turnEndedSeen = false
	s.thinkingFlushed = false
	s.resumed = true
	s.visibleText.Reset()
	s.emittedText.Reset()
	s.visibleThinking.Reset()
	s.emittedThinking.Reset()
	s.mu.Unlock()
	s.resumeReading()
	return nil
}

func (s *Session) sendMcpResult(req *agentv1.ExecServerMessage, result ToolResult) error {
	mcp := &agentv1.McpResult{
		Result: &agentv1.McpResult_Success{
			Success: &agentv1.McpSuccess{
				Content: []*agentv1.McpToolResultContentItem{
					{
						Content: &agentv1.McpToolResultContentItem_Text{
							Text: &agentv1.McpTextContent{Text: result.Content},
						},
					},
				},
				IsError: result.IsError,
			},
		},
	}
	execClient := &agentv1.ExecClientMessage{
		Id:     req.Id,
		ExecId: req.ExecId,
		Message: &agentv1.ExecClientMessage_McpResult{
			McpResult: mcp,
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

// Close tears down the Agent stream.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stream := s.stream
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.manager != nil {
		s.manager.Remove(s)
	}
	if stream != nil {
		return stream.Close()
	}
	return nil
}

func (s *Session) handleServerMessage(msg *agentv1.AgentServerMessage) (pauseForTools bool, err error) {
	switch m := msg.Message.(type) {
	case *agentv1.AgentServerMessage_InteractionUpdate:
		switch u := m.InteractionUpdate.Message.(type) {
		case *agentv1.InteractionUpdate_TextDelta:
			text := u.TextDelta.GetText()
			if text == "" {
				return false, nil
			}
			s.handleTextDelta(text)
		case *agentv1.InteractionUpdate_ThinkingDelta:
			s.handleThinkingDelta(u.ThinkingDelta.GetText())
		case *agentv1.InteractionUpdate_TurnEnded:
			ev := StreamEvent{Type: "usage_final"}
			if u.TurnEnded.InputTokens != nil {
				ev.InputTokens = *u.TurnEnded.InputTokens
			}
			if u.TurnEnded.OutputTokens != nil {
				ev.OutputTokens = *u.TurnEnded.OutputTokens
			}
			if u.TurnEnded.CacheReadTokens != nil {
				ev.CacheReadTokens = *u.TurnEnded.CacheReadTokens
			}
			if u.TurnEnded.CacheWriteTokens != nil {
				ev.CacheWriteTokens = *u.TurnEnded.CacheWriteTokens
			}
			if u.TurnEnded.ReasoningTokens != nil {
				ev.ReasoningTokens = *u.TurnEnded.ReasoningTokens
			}
			// cursor2api: some transports emit turn_ended for the pre-tool
			// segment before mcp_result is returned. Keep the bidi run open
			// while client tools are still pending, but still report usage —
			// dropping it left every tool-calling turn with zero tokens and no
			// cache numbers on the client.
			s.mu.Lock()
			pendingCount := len(s.pending)
			waiting := s.waitingTools
			s.mu.Unlock()
			if pendingCount > 0 || waiting {
				s.emit(ev)
				return false, nil
			}
			log.Debugf("cursor: turn_ended usage in=%d out=%d read=%d write=%d",
				ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheWriteTokens)
			s.emit(ev)
			s.mu.Lock()
			s.turnEndedSeen = true
			s.mu.Unlock()
			// Do not segment_end here — checkpoint / trailing text follow.
		case *agentv1.InteractionUpdate_ToolCallStarted,
			*agentv1.InteractionUpdate_PartialToolCall,
			*agentv1.InteractionUpdate_ToolCallCompleted:
			// Client-visible tool calls are driven by Exec mcp_args for declared tools.
		}
	case *agentv1.AgentServerMessage_KvServerMessage:
		if err := handleKV(s.stream, m.KvServerMessage, s.blobStore); err != nil {
			return false, err
		}
		// Blobs often arrive after conversation_checkpoint_update. Retry flush
		// once turn has ended so repaired assistant text becomes available.
		s.mu.Lock()
		turnEnded := s.turnEndedSeen
		s.mu.Unlock()
		if turnEnded {
			s.flushAssistantText(false)
		}
		return false, nil
	case *agentv1.AgentServerMessage_ExecServerMessage:
		return s.handleExec(m.ExecServerMessage)
	case *agentv1.AgentServerMessage_ConversationCheckpointUpdate:
		if m.ConversationCheckpointUpdate != nil {
			state := proto.Clone(m.ConversationCheckpointUpdate).(*agentv1.ConversationStateStructure)
			s.mu.Lock()
			s.conversationState = state
			turnEnded := s.turnEndedSeen
			s.mu.Unlock()
			if turnEnded {
				s.flushAssistantText(false)
			}
		}
	case *agentv1.AgentServerMessage_ServerMetrics:
		// Four unnamed doubles. Logged while we determine whether any of them
		// carry token counts for turns where TurnEnded never arrives.
		metrics := m.ServerMetrics
		log.Debugf("cursor: server_metrics %.0f %.0f %.0f %.0f",
			metrics.GetMetric_1(), metrics.GetMetric_2(), metrics.GetMetric_3(), metrics.GetMetric_4())
	}
	return false, nil
}

func (s *Session) seedSeenAssistantMessages() {
	for _, msg := range s.checkpointAssistantMessages() {
		s.seenAssistantMsgIDs[msg.identity] = struct{}{}
	}
}

type checkpointAssistantMessage struct {
	identity string
	text     string
	thinking string
}

func (s *Session) checkpointBlobIDs() [][]byte {
	s.mu.Lock()
	state := s.conversationState
	s.mu.Unlock()
	if state == nil {
		return nil
	}
	return append([][]byte(nil), state.GetRootPromptMessagesJson()...)
}

func (s *Session) checkpointAssistantMessages() []checkpointAssistantMessage {
	ids := s.checkpointBlobIDs()
	if len(ids) == 0 {
		// Fall back to whatever blobs we already hold (initial prompt roots).
		s.mu.Lock()
		keys := make([]string, 0, len(s.blobStore))
		for k := range s.blobStore {
			keys = append(keys, k)
		}
		s.mu.Unlock()
		out := make([]checkpointAssistantMessage, 0)
		for _, key := range keys {
			if msg, ok := parseAssistantBlob(key, s.blobStore[key]); ok {
				out = append(out, msg)
			}
		}
		return out
	}
	out := make([]checkpointAssistantMessage, 0)
	s.mu.Lock()
	store := s.blobStore
	s.mu.Unlock()
	for _, id := range ids {
		key := fmt.Sprintf("%x", id)
		raw, ok := store[key]
		if !ok || len(raw) == 0 {
			continue
		}
		if msg, ok := parseAssistantBlob(key, raw); ok {
			out = append(out, msg)
		}
	}
	return out
}

func parseAssistantBlob(key string, raw []byte) (checkpointAssistantMessage, bool) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return checkpointAssistantMessage{}, false
	}
	role, _ := payload["role"].(string)
	if role != "assistant" {
		return checkpointAssistantMessage{}, false
	}
	identity := key
	if id, ok := payload["id"].(string); ok && strings.TrimSpace(id) != "" {
		identity = id
	} else {
		identity = "blob:" + key
	}
	content, _ := payload["content"].([]any)
	var b strings.Builder
	var think strings.Builder
	for _, part := range content {
		m, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch typ, _ := m["type"].(string); typ {
		case "text", "output_text":
			text, _ := m["text"].(string)
			b.WriteString(text)
		case "thinking", "reasoning", "redacted_thinking":
			// Cursor names the payload "thinking" on some blobs and "text" on
			// others; the wire deltas drop the first token, so the checkpoint
			// copy is the only authoritative source.
			text, _ := m["thinking"].(string)
			if text == "" {
				text, _ = m["text"].(string)
			}
			think.WriteString(text)
		default:
			log.Debugf("cursor: checkpoint assistant block type %q ignored", typ)
		}
	}
	text := b.String()
	thinking := think.String()
	if text == "" && thinking == "" {
		return checkpointAssistantMessage{}, false
	}
	return checkpointAssistantMessage{identity: identity, text: text, thinking: thinking}, true
}

// handleThinkingDelta buffers reasoning for the same reason text is buffered:
// Cursor's first delta of a segment arrives without its leading token
// ("I'm calculating" → "'m calculating"), and only the conversation checkpoint
// carries the authoritative copy.
func (s *Session) handleThinkingDelta(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	if s.thinkingFlushed {
		s.mu.Unlock()
		return
	}
	s.visibleThinking.WriteString(text)
	s.emittedThinking.WriteString(text)
	s.mu.Unlock()
	// Stream reasoning live. Buffering it to repair the dropped leading token
	// left slow models (opus with high effort) silent for minutes, which reads
	// as a hang; a clipped first token is the lesser problem. Any reasoning the
	// checkpoint adds beyond what was streamed is appended by flushThinking.
	s.emit(StreamEvent{Type: "thinking_delta", Text: text})
}

func (s *Session) handleTextDelta(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.textFlushed {
		return
	}
	// Always buffer. Emitting live races the checkpoint and shows truncated
	// leading tokens (MARKER→ARKER).
	s.visibleText.WriteString(text)
}

// flushAssistantText reconciles buffered deltas with the conversation
// checkpoint and emits the authoritative assistant text. When force is false,
// wait until at least one new checkpoint blob is available.
func (s *Session) flushAssistantText(force bool) {
	s.mu.Lock()
	if s.textFlushed && s.thinkingFlushed {
		s.mu.Unlock()
		return
	}
	textDone := s.textFlushed
	emitted := s.emittedText.String()
	visible := s.visibleText.String()
	emittedThink := s.emittedThinking.String()
	visibleThink := s.visibleThinking.String()
	if s.seenAssistantMsgIDs == nil {
		s.seenAssistantMsgIDs = map[string]struct{}{}
	}
	s.mu.Unlock()

	final := visible
	finalThink := visibleThink
	var newIDs []string
	haveCheckpoint := false
	for _, msg := range s.checkpointAssistantMessages() {
		s.mu.Lock()
		_, seen := s.seenAssistantMsgIDs[msg.identity]
		s.mu.Unlock()
		if seen {
			continue
		}
		haveCheckpoint = true
		newIDs = append(newIDs, msg.identity)
		final = reconcileAssistantText(final, msg.text)
		if msg.thinking != "" {
			finalThink = reconcileAssistantText(finalThink, msg.thinking)
		}
	}
	if !haveCheckpoint && !force {
		return
	}
	s.flushThinking(finalThink, emittedThink)
	if force {
		s.mu.Lock()
		s.thinkingFlushed = true
		s.mu.Unlock()
	}
	if textDone {
		return
	}
	if final == "" {
		if force {
			s.mu.Lock()
			s.textFlushed = true
			s.mu.Unlock()
		}
		return
	}

	var out string
	switch {
	case final == emitted:
		out = ""
	case emitted == "":
		out = final
	case strings.HasPrefix(final, emitted):
		out = final[len(emitted):]
	case strings.HasSuffix(final, emitted):
		// Live/partial emit lost a prefix; replace with the full checkpoint
		// text so buffering clients (Claude Code) still see a correct answer.
		out = final
	default:
		out = final
	}

	s.mu.Lock()
	for _, id := range newIDs {
		s.seenAssistantMsgIDs[id] = struct{}{}
	}
	if out != "" {
		s.emittedText.Reset()
		s.emittedText.WriteString(final)
		s.visibleText.Reset()
		s.visibleText.WriteString(final)
	}
	s.textFlushed = true
	s.mu.Unlock()

	if out == "" {
		return
	}
	// Chunk for SSE clients that expect incremental deltas.
	for _, part := range splitTextForSSE(out, sseChunkBytes) {
		s.emit(StreamEvent{Type: "text_delta", Text: part})
	}
}

// flushThinking emits the reconciled reasoning before the assistant text so the
// client renders the thinking block first, as Anthropic does.
func (s *Session) flushThinking(final, emitted string) {
	if final == "" || final == emitted {
		return
	}
	out := final
	if emitted != "" && strings.HasPrefix(final, emitted) {
		out = final[len(emitted):]
	}
	if out == "" {
		return
	}
	s.mu.Lock()
	s.emittedThinking.Reset()
	s.emittedThinking.WriteString(final)
	s.visibleThinking.Reset()
	s.visibleThinking.WriteString(final)
	s.mu.Unlock()
	for _, part := range splitTextForSSE(out, sseChunkBytes) {
		s.emit(StreamEvent{Type: "thinking_delta", Text: part})
	}
}

const sseChunkBytes = 64

// splitTextForSSE cuts text into chunks of at most maxBytes, never splitting a
// rune. Slicing by raw byte offsets used to cut multi-byte characters in half,
// and JSON encoding then turned each orphaned byte into U+FFFD (visible as
// "???" inside CJK text).
func splitTextForSSE(s string, maxBytes int) []string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return []string{s}
	}
	out := make([]string, 0, len(s)/maxBytes+1)
	start, size := 0, 0
	for i, r := range s {
		runeLen := utf8.RuneLen(r)
		if runeLen < 0 {
			runeLen = 1
		}
		if size > 0 && size+runeLen > maxBytes {
			out = append(out, s[start:i])
			start, size = i, 0
		}
		size += runeLen
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func reconcileAssistantText(streamed, checkpoint string) string {
	if checkpoint == "" {
		return streamed
	}
	if streamed == "" {
		return checkpoint
	}
	if streamed == checkpoint {
		return streamed
	}
	if strings.HasPrefix(checkpoint, streamed) || strings.HasSuffix(checkpoint, streamed) {
		return checkpoint
	}
	if strings.Contains(checkpoint, streamed) {
		return checkpoint
	}
	if strings.Contains(streamed, checkpoint) {
		return streamed
	}
	if len(checkpoint) > len(streamed) {
		return checkpoint
	}
	return streamed
}

func (s *Session) handleExec(req *agentv1.ExecServerMessage) (bool, error) {
	switch m := req.Message.(type) {
	case nil:
		return false, sendExecStreamClose(s.stream, req.Id)
	case *agentv1.ExecServerMessage_RequestContextArgs:
		result := &agentv1.RequestContextResult{
			Result: &agentv1.RequestContextResult_Success{
				Success: &agentv1.RequestContextSuccess{
					RequestContext: s.requestContext(),
				},
			},
		}
		execClient := &agentv1.ExecClientMessage{
			Id:     req.Id,
			ExecId: req.ExecId,
			Message: &agentv1.ExecClientMessage_RequestContextResult{
				RequestContextResult: result,
			},
		}
		client := &agentv1.AgentClientMessage{
			Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
		}
		payload, err := proto.Marshal(client)
		if err != nil {
			return false, err
		}
		if err = s.stream.WriteEnvelope(payload, false); err != nil {
			return false, err
		}
		return false, sendExecStreamClose(s.stream, req.Id)
	case *agentv1.ExecServerMessage_McpArgs:
		return s.handleMcpArgs(req, m.McpArgs)
	}
	return false, rejectUnsupportedExec(s.stream, req)
}

func (s *Session) handleMcpArgs(req *agentv1.ExecServerMessage, args *agentv1.McpArgs) (bool, error) {
	toolName := strings.TrimSpace(args.GetToolName())
	if toolName == "" {
		toolName = strings.TrimSpace(args.GetName())
	}
	provider := strings.TrimSpace(args.GetProviderIdentifier())
	if provider == "" {
		provider = strings.TrimSpace(args.GetServerIdentifier())
	}
	def := s.lookupTool(toolName, provider)
	if def == nil {
		result := &agentv1.McpResult{
			Result: &agentv1.McpResult_ToolNotFound{
				ToolNotFound: &agentv1.McpToolNotFound{
					Name:           toolName,
					AvailableTools: s.availableToolNames(),
				},
			},
		}
		return false, s.replyMcp(req, result)
	}
	nativeID := strings.TrimSpace(args.GetToolCallId())
	if nativeID == "" {
		nativeID = uuid.NewString()
	}
	// Emit the Claude-safe id to clients so /v1/messages round-trips match BindPending.
	clientID := util.SanitizeClaudeToolID(nativeID)
	if clientID == "" {
		clientID = nativeID
	}
	call := ToolCall{
		ID: clientID,
		// Cursor sees the (possibly renamed) upstream name; clients must get
		// back the tool name they declared.
		Name:      s.toolAlias.ClientName(def.Name),
		Arguments: decodeMcpArguments(args, def),
	}
	copyReq := proto.Clone(req).(*agentv1.ExecServerMessage)
	pending := &pendingExec{request: copyReq, call: call}
	s.mu.Lock()
	s.pending[clientID] = pending
	if nativeID != clientID {
		s.pending[nativeID] = pending
	}
	s.mu.Unlock()
	if s.manager != nil {
		s.manager.BindPending(clientID, s)
		if nativeID != clientID {
			s.manager.BindPending(nativeID, s)
		}
	}
	s.emit(StreamEvent{Type: "tool_call", ToolCall: &call})
	return true, nil
}

func (s *Session) replyMcp(req *agentv1.ExecServerMessage, result *agentv1.McpResult) error {
	execClient := &agentv1.ExecClientMessage{
		Id:      req.Id,
		ExecId:  req.ExecId,
		Message: &agentv1.ExecClientMessage_McpResult{McpResult: result},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

func (s *Session) lookupPendingTool(toolCallID string) (*pendingExec, string) {
	if s == nil || toolCallID == "" {
		return nil, ""
	}
	if item := s.pending[toolCallID]; item != nil {
		return item, toolCallID
	}
	if safe := util.SanitizeClaudeToolID(toolCallID); safe != "" && safe != toolCallID {
		if item := s.pending[safe]; item != nil {
			return item, safe
		}
	}
	// Match against the client-facing id stored on the pending call.
	for id, item := range s.pending {
		if item != nil && item.call.ID == toolCallID {
			return item, id
		}
	}
	return nil, ""
}

func (s *Session) clearPendingTool(keys ...string) {
	if s == nil {
		return
	}
	var targets []*pendingExec
	seenTarget := map[*pendingExec]struct{}{}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if item := s.pending[key]; item != nil {
			if _, ok := seenTarget[item]; !ok {
				seenTarget[item] = struct{}{}
				targets = append(targets, item)
			}
		}
		if safe := util.SanitizeClaudeToolID(key); safe != "" && safe != key {
			if item := s.pending[safe]; item != nil {
				if _, ok := seenTarget[item]; !ok {
					seenTarget[item] = struct{}{}
					targets = append(targets, item)
				}
			}
		}
	}
	if len(targets) == 0 {
		for _, key := range keys {
			delete(s.pending, key)
		}
		return
	}
	for id, item := range s.pending {
		if _, hit := seenTarget[item]; hit {
			delete(s.pending, id)
		}
	}
}

// releasePendingExec drops every session+manager index that points at the same
// pending exec (native id, Claude-safe alias, and call.ID).
func (s *Session) releasePendingExec(item *pendingExec, keys ...string) {
	if s == nil || item == nil {
		return
	}
	s.mu.Lock()
	aliasKeys := make([]string, 0, 4)
	for id, p := range s.pending {
		if p == item {
			aliasKeys = append(aliasKeys, id)
		}
	}
	for _, key := range keys {
		if key != "" {
			aliasKeys = append(aliasKeys, key)
			if safe := util.SanitizeClaudeToolID(key); safe != "" {
				aliasKeys = append(aliasKeys, safe)
			}
		}
	}
	if item.call.ID != "" {
		aliasKeys = append(aliasKeys, item.call.ID)
		if safe := util.SanitizeClaudeToolID(item.call.ID); safe != "" {
			aliasKeys = append(aliasKeys, safe)
		}
	}
	s.clearPendingTool(aliasKeys...)
	s.mu.Unlock()
	if s.manager == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, key := range aliasKeys {
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		s.manager.UnbindPending(key)
	}
}

func (s *Session) lookupTool(name, provider string) *ToolDefinition {
	if name == "" {
		return nil
	}
	def := s.toolIndex[name]
	if def == nil {
		return nil
	}
	if provider == "" || provider == MCPProviderIdentifier {
		return def
	}
	return nil
}

func (s *Session) availableToolNames() []string {
	names := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return names
}

func (s *Session) requestContext() *agentv1.RequestContext {
	ctx := headlessRequestContext()
	if len(s.tools) > 0 {
		ctx.Tools = buildMcpToolDefinitions(s.tools)
	}
	return ctx
}

func rejectUnsupportedExec(stream *BidiStream, req *agentv1.ExecServerMessage) error {
	// Cursor asks the client to run one of its own built-in tools. The MVP
	// schema only decodes request_context and mcp_args, so log which field the
	// server used: that is exactly the variant a model reached for instead of
	// the forwarded MCP tools.
	log.Warnf("cursor: unsupported exec variant, refusing (unknown proto fields=%v)", unknownExecFieldNumbers(req))
	code := "exec_variant_unsupported"
	control := &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_Throw{
			Throw: &agentv1.ExecClientThrow{
				Id:        req.Id,
				Error:     "No handler for Cursor exec message",
				ErrorCode: &code,
			},
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: control},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(stream, req.Id)
}

// unknownExecFieldNumbers lists the protobuf field numbers carried in an exec
// message the MVP schema cannot decode.
func unknownExecFieldNumbers(req *agentv1.ExecServerMessage) []int {
	if req == nil {
		return nil
	}
	raw := req.ProtoReflect().GetUnknown()
	var fields []int
	seen := map[int]struct{}{}
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			break
		}
		raw = raw[n:]
		if _, ok := seen[int(num)]; !ok {
			seen[int(num)] = struct{}{}
			fields = append(fields, int(num))
		}
		n = protowire.ConsumeFieldValue(num, typ, raw)
		if n < 0 {
			break
		}
		raw = raw[n:]
	}
	return fields
}

func buildMcpToolDefinitions(tools []ToolDefinition) []*agentv1.McpToolDefinition {
	tools = SanitizeToolDefinitions(tools)
	out := make([]*agentv1.McpToolDefinition, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		schema := tool.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		value, err := toProtobufValue(schema)
		if err != nil {
			value, _ = toProtobufValue(map[string]any{"type": "object", "properties": map[string]any{}})
		}
		out = append(out, &agentv1.McpToolDefinition{
			Name:               name,
			Description:        tool.Description,
			InputSchema:        value,
			ProviderIdentifier: MCPProviderIdentifier,
			ToolName:           name,
		})
	}
	return out
}

// RunChat performs a text/tool Cursor Agent segment and returns the collected result.
func RunChat(ctx context.Context, creds AccountCredentials, model string, messages []ChatMessage, tools []ToolDefinition, opts ...StartOption) (*ChatResult, error) {
	results := extractToolResults(messages)
	if len(results) > 0 {
		session, err := DefaultSessionManager().ResolveForToolResults(results)
		if err == nil {
			if err = session.SubmitToolResults(results); err == nil {
				result, collectErr := session.CollectSegment(ctx)
				if collectErr == nil {
					return result, nil
				}
				if !errors.Is(collectErr, ErrToolResumeRebuild) {
					return nil, collectErr
				}
				// Resumed run went silent; fall through to a history rebuild.
			} else if !errors.Is(err, ErrToolResumeRebuild) {
				return nil, err
			}
		}
		// Session was lost (process restart / TTL / id mismatch) or the resumed
		// run went silent. Rebuild a new Agent run that keeps the tool history
		// in conversation state instead of hard-failing the client.
		log.Warnf("cursor: resume tool session unusable (%v); rebuilding turn from history", err)
		messages = append(append([]ChatMessage(nil), messages...), ChatMessage{
			Role:    "user",
			Content: "Tool results have been provided above. Continue the task based on those results.",
		})
	}
	session, err := StartSession(ctx, creds, model, messages, tools, opts...)
	if err != nil {
		return nil, err
	}
	result, err := session.CollectSegment(ctx)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if result.FinishReason != "tool_calls" {
		_ = session.Close()
	}
	return result, nil
}

func extractToolResults(messages []ChatMessage) []ToolResult {
	// Only trailing tool messages after the latest assistant tool_calls are
	// treated as a live round-trip resume; older history tool rows are ignored.
	start := -1
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Role {
		case "tool":
			start = i
			continue
		case "assistant":
			if len(messages[i].ToolCalls) > 0 && start >= 0 {
				out := make([]ToolResult, 0, len(messages)-start)
				for _, msg := range messages[start:] {
					if msg.Role != "tool" || strings.TrimSpace(msg.ToolCallID) == "" {
						continue
					}
					out = append(out, ToolResult{
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
