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
	emitted bool
	// native marks a bridged built-in tool call. Those never appear in the
	// checkpoint's pending tool-call JSON (that only carries MCP parts), so
	// batch readiness must not wait on checkpoint confirmation for them.
	native bool
}

// Session is a live Agent Connect run that can pause for client tools.
type Session struct {
	ID             string
	ConversationID string
	Model          string

	mu        sync.Mutex
	stream    *BidiStream
	blobStore map[string][]byte
	tools     []ToolDefinition
	// clientTools is the sanitized tool list exactly as the downstream client
	// declared it, before collision handling dropped or renamed anything. The
	// native bridge resolves built-in exec requests against these names.
	clientTools []ToolDefinition
	toolIndex   map[string]*ToolDefinition
	toolAlias   *ToolAliasMap
	pending      map[string]*pendingExec
	pendingOrder []*pendingExec
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
	done          <-chan struct{}
	lastActivity  time.Time
	manager       *SessionManager
	parallelTools bool
	// usageSeen carries TurnEnded counters that land after the caller already
	// stopped reading the segment, which is the normal ordering when a turn
	// pauses for client tools.
	usageSeen   *StreamEvent
	usageSignal chan struct{}

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
	terminalUsage       StreamEvent
	terminalUsageReady  bool
	segmentEnded        bool
	textFlushed         bool
	thinkingFlushed     bool
	textStreamLive      bool
	holdTimer           *time.Timer
	resumed             bool
	// usedContinuation marks a run opened from a stored checkpoint; if it
	// fails, the checkpoint is dropped so retries rebuild from history.
	usedContinuation bool
	// holdWindow overrides textHoldWindow. Continuation runs get a longer one:
	// they receive no early checkpoint to release the hold, their answers are
	// typically short, and a short answer that outruns the hold cannot have a
	// dropped opening repaired once it has streamed.
	holdWindow time.Duration
	// failed suppresses checkpoint persistence on Close after a stream error.
	failed bool
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
	conversationID string
	parallelTools  bool
}

// WithForceToolAlias prefixes every client tool name before sending it to
// Cursor. Used as a fallback when a provider 400 suggests an unknown built-in
// tool name collision.
func WithForceToolAlias() StartOption {
	return func(c *startConfig) { c.forceToolAlias = true }
}

// WithConversationID pins the Cursor conversation identity to an explicit
// downstream session. Without it every request starts an isolated conversation.
func WithConversationID(conversationID string) StartOption {
	return func(c *startConfig) { c.conversationID = strings.TrimSpace(conversationID) }
}

// WithParallelToolCalls controls whether a formal Cursor provider batch is
// exposed to the downstream client at once or one call per continuation.
func WithParallelToolCalls(enabled bool) StartOption {
	return func(c *startConfig) { c.parallelTools = enabled }
}

// StartSession opens a new Agent run for the given messages/tools.
func StartSession(ctx context.Context, creds AccountCredentials, model string, messages []ChatMessage, tools []ToolDefinition, opts ...StartOption) (*Session, error) {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("cursor: access_token is required")
	}
	cfg := startConfig{parallelTools: true}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	clientTools := SanitizeToolDefinitions(tools)
	tools, toolAlias := AliasToolsForCursor(clientTools, cfg.forceToolAlias)
	if !toolAlias.Empty() {
		messages = ApplyToolAliasToMessages(messages, toolAlias)
		log.Debugf("cursor: renamed %d tool(s) that collide with Cursor built-ins", len(toolAlias.toClient))
	}
	selection := ResolveRequestedModel(model)
	var clientMsg *agentv1.AgentClientMessage
	var blobStore map[string][]byte
	conversationID := strings.TrimSpace(cfg.conversationID)
	usedContinuation := false
	// The native path for a follow-up turn: send the server's own checkpoint
	// state back with just the new user message, instead of re-flattening the
	// client's whole history into fresh prompt blobs.
	if snapshot, ok := takeContinuation(conversationID); ok {
		if continuationMatchesHistory(snapshot, messages) {
			clientMsg, blobStore = buildContinuationRunRequest(model, messages, tools, conversationID, snapshot)
			usedContinuation = true
		} else {
			log.Debugf("cursor: stored checkpoint does not match client history; rebuilding conversation %s", conversationID)
		}
	}
	if clientMsg == nil {
		var err error
		clientMsg, blobStore, conversationID, err = buildRunRequestForConversation(model, messages, tools, cfg.conversationID)
		if err != nil {
			return nil, err
		}
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
		clientTools:         clientTools,
		toolIndex:           indexTools(tools),
		toolAlias:           toolAlias,
		pending:             map[string]*pendingExec{},
		pendingOrder:        make([]*pendingExec, 0),
		events:              make(chan StreamEvent, 256),
		errCh:               make(chan error, 1),
		pauseCh:             make(chan struct{}),
		cancel:              cancel,
		done:                runCtx.Done(),
		lastActivity:        time.Now(),
		manager:             DefaultSessionManager(),
		parallelTools:       cfg.parallelTools,
		seenAssistantMsgIDs: map[string]struct{}{},
		usedContinuation:    usedContinuation,
	}
	if usedContinuation {
		session.holdWindow = continuationTextHoldWindow
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
		responseEnded := s.responseEnded
		closed := s.closed
		stream := s.stream
		pauseCh := s.pauseCh
		s.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-ctx.Done():
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
		// Keep reading while a live run is waiting for client tools. Cursor may
		// send a trailing TurnEnded after the formal tool boundary; pausing here
		// discarded those authoritative cache counters. Once the response has
		// actually ended, wait for SubmitToolResults to rebuild or resume.
		if waiting && responseEnded {
			select {
			case <-ctx.Done():
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
				if s.emitPendingToolBatch(false) {
					s.emitTerminalUsage()
					s.beginWaitingTools()
					s.mu.Lock()
					s.segmentEnded = true
					s.mu.Unlock()
					s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
				}
			}
			if sawEndStream {
				if s.emitPendingToolBatch(true) {
					s.emitTerminalUsage()
					s.beginWaitingTools()
					s.mu.Lock()
					alreadyEnded := s.segmentEnded
					s.segmentEnded = true
					s.mu.Unlock()
					if !alreadyEnded {
						s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
					}
				}
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
				s.emitTerminalUsage()
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
			if s.emitPendingToolBatch(true) {
				s.emitTerminalUsage()
				s.beginWaitingTools()
				s.mu.Lock()
				alreadyEnded := s.segmentEnded
				s.segmentEnded = true
				s.mu.Unlock()
				if !alreadyEnded {
					s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
				}
			}
			if s.awaitingClientTools() {
				s.mu.Lock()
				s.responseEnded = true
				s.mu.Unlock()
				log.Warnf("cursor: EOF while awaiting client tool results (pending=%d); will rebuild after tool results", s.pendingToolCount())
				s.beginWaitingTools()
				continue
			}
			s.flushAssistantText(true)
			s.emitTerminalUsage()
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

func (s *Session) recordUsage(ev StreamEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	copied := ev
	s.usageSeen = &copied
	signal := s.usageSignal
	s.usageSignal = nil
	s.mu.Unlock()
	if signal != nil {
		close(signal)
	}
}

// TakeUsage returns the counters for the turn, waiting up to d for them.
//
// Cursor reports TurnEnded after it has already asked the client to run a tool,
// so a caller that stops at the tool call sees nothing unless it waits a moment
// for the trailing frame.
func (s *Session) TakeUsage(d time.Duration) (StreamEvent, bool) {
	if s == nil {
		return StreamEvent{}, false
	}
	s.mu.Lock()
	if s.usageSeen != nil {
		ev := *s.usageSeen
		s.mu.Unlock()
		return ev, true
	}
	if d <= 0 {
		s.mu.Unlock()
		return StreamEvent{}, false
	}
	if s.usageSignal == nil {
		s.usageSignal = make(chan struct{})
	}
	signal := s.usageSignal
	s.mu.Unlock()

	select {
	case <-signal:
	case <-time.After(d):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usageSeen == nil {
		return StreamEvent{}, false
	}
	return *s.usageSeen, true
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
	// Event order is part of the downstream streaming protocol. Never discard a
	// delta or terminal event merely because the consumer is temporarily slow.
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

func (s *Session) emitTerminalUsage() {
	s.mu.Lock()
	if !s.terminalUsageReady {
		s.mu.Unlock()
		return
	}
	ev := s.terminalUsage
	s.terminalUsageReady = false
	s.terminalUsage = StreamEvent{}
	s.mu.Unlock()
	s.emit(ev)
}

func (s *Session) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.failed = true
	usedContinuation := s.usedContinuation
	s.mu.Unlock()
	if usedContinuation {
		// The server would not run on the stored state; retries must rebuild
		// from client history instead of replaying the same rejection.
		dropContinuation(s.ConversationID)
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
	toolCalls := 0
	for {
		select {
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
				return nil, fmt.Errorf("%w: stream closed before a terminal boundary", ErrEmptyTerminal)
			}
			switch ev.Type {
			case "text_delta":
				text.WriteString(ev.Text)
			case "thinking_delta":
				thinking.WriteString(ev.Text)
			case "tool_call":
				if ev.ToolCall != nil {
					result.ToolCalls = append(result.ToolCalls, *ev.ToolCall)
					toolCalls++
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
				if result.FinishReason == "tool_calls" && toolCalls == 0 {
					return nil, fmt.Errorf("%w: tool boundary contained no tool call", ErrEmptyTerminal)
				}
				return result, nil
			}
		}
	}
}

func (s *Session) wasResumed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumed
}

// IterSegment invokes fn for each event until segment_end (inclusive).
func (s *Session) IterSegment(ctx context.Context, fn func(StreamEvent) error) error {
	toolCalls := 0
	for {
		select {
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
				return fmt.Errorf("%w: stream closed before a terminal boundary", ErrEmptyTerminal)
			}
			if ev.Type == "tool_call" && ev.ToolCall != nil {
				toolCalls++
			}
			if err := fn(ev); err != nil {
				return err
			}
			if ev.Type == "segment_end" {
				if ev.Reason == "tool_calls" && toolCalls == 0 {
					return fmt.Errorf("%w: tool boundary contained no tool call", ErrEmptyTerminal)
				}
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

// ErrEmptyTerminal is reserved for malformed stream termination or a formal
// tool boundary without any corresponding tool call. A clean Cursor
// TurnEnded/END_STREAM is authoritative even when the selected provider emits
// no text delta; rejecting such turns caused an account-wide outage in
// production because output text is not guaranteed on every Agent turn.
var ErrEmptyTerminal = errors.New("cursor: turn ended without final assistant text or tool calls")

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
	selected := make(map[*pendingExec]struct{}, len(results))
	for _, result := range results {
		item, id := s.lookupPendingTool(result.ToolCallID)
		if item == nil {
			s.mu.Unlock()
			return fmt.Errorf("cursor: no pending tool call %s", result.ToolCallID)
		}
		pending = append(pending, item)
		ids = append(ids, id)
		selected[item] = struct{}{}
	}
	if s.parallelTools {
		for _, item := range s.pendingOrder {
			if item == nil || !item.emitted {
				continue
			}
			if _, ok := selected[item]; !ok {
				s.mu.Unlock()
				_ = s.Close()
				return ErrToolResumeRebuild
			}
		}
	}
	s.mu.Unlock()

	if responseEnded {
		// Best-effort deliver results, then force history rebuild. Continuing
		// to Read on an END_STREAM response stalls the client indefinitely.
		for i, result := range results {
			if pending[i].request != nil && s.stream != nil {
				_ = s.sendToolResult(pending[i].request, result)
			}
			s.releasePendingExec(pending[i], ids[i], results[i].ToolCallID)
		}
		_ = s.Close()
		return ErrToolResumeRebuild
	}

	for i, result := range results {
		item := pending[i]
		if err := s.sendToolResult(item.request, result); err != nil {
			// Release remaining pending indexes so a rebuild does not keep
			// stale multi-tool bindings that block later ResolveForToolResults.
			for j := i; j < len(pending); j++ {
				s.releasePendingExec(pending[j], ids[j], results[j].ToolCallID)
			}
			return fmt.Errorf("cursor: submit tool result %s: %w", ids[i], err)
		}
		s.releasePendingExec(item, ids[i], result.ToolCallID)
	}
	// A downstream client that explicitly disabled parallel tool calls must see
	// one formal Cursor batch member per HTTP continuation. Keep the provider
	// read side paused, expose the next staged call, and only resume Cursor once
	// every member has received its result.
	if !s.parallelTools && s.emitNextPendingTool() {
		s.beginWaitingTools()
		s.mu.Lock()
		s.segmentEnded = true
		s.mu.Unlock()
		s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
		return nil
	}
	s.mu.Lock()
	s.segmentEnded = false
	s.textFlushed = false
	s.turnEndedSeen = false
	s.terminalUsageReady = false
	s.terminalUsage = StreamEvent{}
	s.usageSeen = nil
	s.thinkingFlushed = false
	s.textStreamLive = false
	if s.holdTimer != nil {
		s.holdTimer.Stop()
		s.holdTimer = nil
	}
	s.resumed = true
	s.visibleText.Reset()
	s.emittedText.Reset()
	s.visibleThinking.Reset()
	s.emittedThinking.Reset()
	s.mu.Unlock()
	s.resumeReading()
	return nil
}

// sendToolResult answers a pending exec with the reply shape its request used:
// live stream frames for the streaming shell, typed native results for the
// other bridged built-in tools, McpResult for MCP tools.
func (s *Session) sendToolResult(req *agentv1.ExecServerMessage, result ToolResult) error {
	if m, isStream := req.Message.(*agentv1.ExecServerMessage_ShellStreamArgs); isStream {
		for _, frame := range shellStreamFrames(m.ShellStreamArgs, result) {
			if err := s.sendExecClient(&agentv1.ExecClientMessage{
				Id:      req.Id,
				ExecId:  req.ExecId,
				Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: frame},
			}); err != nil {
				return err
			}
		}
		return sendExecStreamClose(s.stream, req.Id)
	}
	if reply := nativeResultReply(req, result); reply != nil {
		if err := s.sendExecClient(reply); err != nil {
			return err
		}
		return sendExecStreamClose(s.stream, req.Id)
	}
	return s.sendMcpResult(req, result)
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
	if err := s.sendExecClient(execClient); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

// sendExecClient marshals and writes one ExecClientMessage envelope.
func (s *Session) sendExecClient(execClient *agentv1.ExecClientMessage) error {
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return s.stream.WriteEnvelope(payload, false)
}

// Close tears down the Agent stream.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.holdTimer != nil {
		s.holdTimer.Stop()
		s.holdTimer = nil
	}
	stream := s.stream
	cancel := s.cancel
	failed := s.failed
	s.mu.Unlock()
	if !failed {
		// Keep the server's latest checkpoint so the conversation's next turn
		// can continue natively instead of rebuilding from client history.
		rememberContinuation(s.ConversationID, s.checkpointSnapshot())
	}
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
			// Keep an out-of-band copy for callers whose formal tool boundary
			// was reached in the same read batch. It is still only a candidate:
			// public usage is emitted at that boundary or at clean END_STREAM.
			s.recordUsage(ev)
			// TurnEnded is a candidate terminal update. Cursor can still deliver
			// the authoritative checkpoint or terminate this attempt with an
			// error, so usage is published only at a formal tool boundary or a
			// clean Connect END_STREAM/EOF.
			log.Debugf("cursor: turn_ended usage in=%d out=%d read=%d write=%d",
				ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheWriteTokens)
			s.mu.Lock()
			s.turnEndedSeen = true
			s.terminalUsage = ev
			s.terminalUsageReady = true
			s.mu.Unlock()
			// Do not segment_end here — checkpoint / trailing text follow.
		case *agentv1.InteractionUpdate_ToolCallStarted,
			*agentv1.InteractionUpdate_PartialToolCall,
			*agentv1.InteractionUpdate_ToolCallCompleted:
			// Client-visible tool calls are driven by Exec mcp_args for declared tools.
		}
	case *agentv1.AgentServerMessage_KvServerMessage:
		if err := handleKV(s.stream, m.KvServerMessage, s.blobStore, &s.mu); err != nil {
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
			} else {
				// The checkpoint is the authoritative copy of the opening, so
				// there is nothing left to wait for: release the held text now
				// instead of sitting out the rest of the hold window.
				s.releaseTextStream()
			}
			if s.pendingToolBatchReady() {
				return true, nil
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

type checkpointSnapshot struct {
	state *agentv1.ConversationStateStructure
	blobs map[string][]byte
}

func (s *Session) checkpointSnapshot() checkpointSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var state *agentv1.ConversationStateStructure
	if s.conversationState != nil {
		state = proto.Clone(s.conversationState).(*agentv1.ConversationStateStructure)
	}
	blobs := make(map[string][]byte, len(s.blobStore))
	for key, raw := range s.blobStore {
		blobs[key] = append([]byte(nil), raw...)
	}
	return checkpointSnapshot{state: state, blobs: blobs}
}

func checkpointBlobIDs(snapshot checkpointSnapshot) [][]byte {
	state := snapshot.state
	if state == nil {
		return nil
	}

	ids := make([][]byte, 0, len(state.GetRootPromptMessagesJson())+len(state.GetTurns()))
	seen := make(map[string]struct{})
	appendID := func(id []byte) {
		if len(id) == 0 {
			return
		}
		key := fmt.Sprintf("%x", id)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		ids = append(ids, append([]byte(nil), id...))
	}
	for _, id := range state.GetRootPromptMessagesJson() {
		appendID(id)
	}
	for _, turnID := range state.GetTurns() {
		raw := snapshot.blobs[fmt.Sprintf("%x", turnID)]
		if len(raw) == 0 {
			continue
		}
		turn := &agentv1.ConversationTurnStructure{}
		if err := proto.Unmarshal(raw, turn); err != nil {
			continue
		}
		agentTurn := turn.GetAgentConversationTurn()
		if agentTurn == nil {
			continue
		}
		for _, stepID := range agentTurn.GetSteps() {
			appendID(stepID)
		}
	}
	return ids
}

func (s *Session) checkpointAssistantMessages() []checkpointAssistantMessage {
	return snapshotAssistantMessages(s.checkpointSnapshot())
}

func parseAssistantBlob(key string, raw []byte) (checkpointAssistantMessage, bool) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err == nil {
		role, _ := payload["role"].(string)
		if role == "assistant" {
			identity := key
			if id, ok := payload["id"].(string); ok && strings.TrimSpace(id) != "" {
				identity = id
			} else {
				identity = "blob:" + key
			}
			content, _ := payload["content"].([]any)
			var text strings.Builder
			var thinking strings.Builder
			for _, part := range content {
				block, ok := part.(map[string]any)
				if !ok {
					continue
				}
				switch typ, _ := block["type"].(string); typ {
				case "text", "output_text":
					value, _ := block["text"].(string)
					text.WriteString(value)
				case "thinking", "reasoning", "redacted_thinking":
					value, _ := block["thinking"].(string)
					if value == "" {
						value, _ = block["text"].(string)
					}
					thinking.WriteString(value)
				default:
					log.Debugf("cursor: checkpoint assistant block type %q ignored", typ)
				}
			}
			if text.Len() > 0 || thinking.Len() > 0 {
				return checkpointAssistantMessage{
					identity: identity,
					text:     text.String(),
					thinking: thinking.String(),
				}, true
			}
		}
	}

	step := &agentv1.ConversationStep{}
	if err := proto.Unmarshal(raw, step); err != nil {
		return checkpointAssistantMessage{}, false
	}
	identity := "blob:" + key
	switch message := step.GetMessage().(type) {
	case *agentv1.ConversationStep_AssistantMessage:
		if text := message.AssistantMessage.GetText(); text != "" {
			return checkpointAssistantMessage{identity: identity, text: text}, true
		}
	case *agentv1.ConversationStep_ThinkingMessage:
		if thinking := message.ThinkingMessage.GetText(); thinking != "" {
			return checkpointAssistantMessage{identity: identity, thinking: thinking}, true
		}
	}
	return checkpointAssistantMessage{}, false
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

// textHoldWindow gives the checkpoint a short chance to repair Cursor's
// occasionally missing leading token. After this opening window, deltas stream
// immediately instead of buffering the whole response.
//
// The checkpoint is the only thing that can restore a dropped opening, so this
// is the deadline for waiting on one. Shortening it to 400ms to save latency
// cost claude-fable-5 the first nine characters of a reply in one run out of
// three. The checkpoint now releases the hold as soon as it lands, so a fast
// turn no longer pays the full window anyway.
const textHoldWindow = 1200 * time.Millisecond

func (s *Session) handleTextDelta(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	if s.textFlushed {
		s.mu.Unlock()
		return
	}
	s.visibleText.WriteString(text)
	live := s.textStreamLive
	if live {
		s.emittedText.WriteString(text)
	} else if s.holdTimer == nil {
		window := s.holdWindow
		if window <= 0 {
			window = textHoldWindow
		}
		s.holdTimer = time.AfterFunc(window, s.releaseTextStream)
	}
	s.mu.Unlock()
	if live {
		s.emit(StreamEvent{Type: "text_delta", Text: text})
	}
}

// releaseTextStream emits the withheld opening, using an already available
// checkpoint as the authoritative copy, then switches subsequent deltas to
// live streaming.
func (s *Session) releaseTextStream() {
	s.mu.Lock()
	if s.textStreamLive || s.textFlushed {
		s.mu.Unlock()
		return
	}
	emitted := s.emittedText.String()
	visible := s.visibleText.String()
	if s.seenAssistantMsgIDs == nil {
		s.seenAssistantMsgIDs = map[string]struct{}{}
	}
	s.mu.Unlock()

	final := visible
	var newIDs []string
	for _, msg := range s.checkpointAssistantMessages() {
		s.mu.Lock()
		_, seen := s.seenAssistantMsgIDs[msg.identity]
		s.mu.Unlock()
		if seen {
			continue
		}
		newIDs = append(newIDs, msg.identity)
		final = reconcileAssistantText(final, msg.text)
	}
	out := final
	if emitted != "" && strings.HasPrefix(final, emitted) {
		out = final[len(emitted):]
	}

	s.mu.Lock()
	if s.textStreamLive || s.textFlushed {
		s.mu.Unlock()
		return
	}
	for _, id := range newIDs {
		s.seenAssistantMsgIDs[id] = struct{}{}
	}
	s.textStreamLive = true
	s.holdTimer = nil
	s.emittedText.Reset()
	s.emittedText.WriteString(final)
	s.visibleText.Reset()
	s.visibleText.WriteString(final)
	s.mu.Unlock()

	for _, part := range splitTextForSSE(out, sseChunkBytes) {
		s.emit(StreamEvent{Type: "text_delta", Text: part})
	}
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
	if haveCheckpoint {
		s.mu.Lock()
		s.textStreamLive = true
		if s.holdTimer != nil {
			s.holdTimer.Stop()
			s.holdTimer = nil
		}
		s.mu.Unlock()
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
	default:
		// Once text is live the downstream already owns the emitted prefix.
		// Replaying a diverging checkpoint would duplicate the answer.
		log.Debugf("cursor: checkpoint text diverges from streamed text (streamed=%d checkpoint=%d); keeping streamed copy",
			len(emitted), len(final))
		out = ""
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
	if isNativeExec(req) {
		return s.handleNativeExec(req)
	}
	return false, rejectUnsupportedExec(s.stream, req)
}

// handleNativeExec bridges a built-in tool execution request onto whichever
// tool the downstream client declared for it. The pend/emit/submit lifecycle is
// exactly the MCP one; only the wire representation of the reply differs.
func (s *Session) handleNativeExec(req *agentv1.ExecServerMessage) (bool, error) {
	kind, nativeID, call, ok := nativeBridge(req, s.clientTools)
	if !ok {
		log.Warnf("cursor: built-in %s call but the client declared no matching tool; answering with a typed error", kind)
		// Having no MCP resources is a legitimate answer, not a failure.
		if reply := nativeUnsupportedReply(req, ""); reply != nil {
			if _, isList := req.Message.(*agentv1.ExecServerMessage_ListMcpResourcesExecArgs); isList {
				if err := s.sendExecClient(reply); err != nil {
					return false, err
				}
				return false, sendExecStreamClose(s.stream, req.Id)
			}
		}
		return false, s.sendToolResult(req, ToolResult{
			Content: "the connected client provides no " + kind + " capability; use another approach",
			IsError: true,
		})
	}
	log.Debugf("cursor: bridging built-in %s call to client tool %q", kind, call.Name)
	s.pendToolCall(req, nativeID, call)
	// Native calls have no checkpoint boundary to wait for: pause the segment
	// now so the client receives the tool_use and can answer the exec.
	return true, nil
}

// pendToolCall registers a server-initiated tool execution so the batch
// emitter can surface it and a later SubmitToolResults can answer it.
func (s *Session) pendToolCall(req *agentv1.ExecServerMessage, nativeID string, call ToolCall) {
	if strings.TrimSpace(nativeID) == "" {
		nativeID = uuid.NewString()
	}
	clientID := util.SanitizeClaudeToolID(nativeID)
	if clientID == "" {
		clientID = nativeID
	}
	call.ID = clientID
	copyReq := proto.Clone(req).(*agentv1.ExecServerMessage)
	pending := &pendingExec{request: copyReq, call: call, native: isNativeExec(req)}
	s.mu.Lock()
	s.pending[clientID] = pending
	if nativeID != clientID {
		s.pending[nativeID] = pending
	}
	s.pendingOrder = append(s.pendingOrder, pending)
	s.mu.Unlock()
	if s.manager != nil {
		s.manager.BindPending(clientID, s)
		if nativeID != clientID {
			s.manager.BindPending(nativeID, s)
		}
	}
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
	call := ToolCall{
		// Cursor sees the (possibly renamed) upstream name; clients must get
		// back the tool name they declared.
		Name:      s.toolAlias.ClientName(def.Name),
		Arguments: decodeMcpArguments(args, def),
	}
	s.pendToolCall(req, strings.TrimSpace(args.GetToolCallId()), call)
	// Exec frames can arrive before Cursor's checkpoint proves the complete
	// provider tool batch. Keep reading until that boundary is available.
	return false, nil
}

type checkpointToolCall struct {
	id   string
	name string
}

func (s *Session) emitPendingToolBatch(force bool) bool {
	pending, ready := s.orderedPendingToolBatch(force)
	if !ready {
		return false
	}
	if !s.parallelTools && len(pending) > 1 {
		pending = pending[:1]
	}
	for _, item := range pending {
		s.mu.Lock()
		if item.emitted {
			s.mu.Unlock()
			continue
		}
		item.emitted = true
		call := item.call
		s.mu.Unlock()
		s.emit(StreamEvent{Type: "tool_call", ToolCall: &call})
	}
	return len(pending) > 0
}

func (s *Session) emitNextPendingTool() bool {
	s.mu.Lock()
	var next *pendingExec
	for _, item := range s.pendingOrder {
		if item != nil && !item.emitted {
			item.emitted = true
			next = item
			break
		}
	}
	s.mu.Unlock()
	if next == nil {
		return false
	}
	call := next.call
	s.emit(StreamEvent{Type: "tool_call", ToolCall: &call})
	return true
}

func (s *Session) pendingToolBatchReady() bool {
	_, ready := s.orderedPendingToolBatch(false)
	return ready
}

func (s *Session) orderedPendingToolBatch(force bool) ([]*pendingExec, bool) {
	s.mu.Lock()
	pending := make([]*pendingExec, 0, len(s.pendingOrder))
	mcpPending := make([]*pendingExec, 0, len(s.pendingOrder))
	for _, item := range s.pendingOrder {
		if item != nil && !item.emitted {
			pending = append(pending, item)
			if !item.native {
				mcpPending = append(mcpPending, item)
			}
		}
	}
	state := s.conversationState
	s.mu.Unlock()
	if len(pending) == 0 {
		return nil, false
	}
	// Bridged built-in calls never show up in the checkpoint's pending
	// tool-call JSON, so a batch of only native calls is ready as it arrived.
	if len(mcpPending) == 0 {
		return pending, true
	}

	// The checkpoint gate confirms and orders the MCP part of the batch.
	checkpointCalls, checkpointPresent := checkpointToolCalls(state)
	if checkpointPresent {
		byID := make(map[string]*pendingExec, len(mcpPending))
		for _, item := range mcpPending {
			byID[item.call.ID] = item
		}
		ordered := make([]*pendingExec, 0, len(checkpointCalls))
		matched := true
		for _, call := range checkpointCalls {
			item := byID[util.SanitizeClaudeToolID(call.id)]
			if item == nil {
				item = byID[call.id]
			}
			if item == nil || !s.toolCallNameMatches(item.call.Name, call.name) {
				matched = false
				break
			}
			ordered = append(ordered, item)
		}
		if matched && len(ordered) == len(mcpPending) {
			if len(ordered) == len(pending) {
				pending = ordered
			} else {
				// Mixed batch: checkpoint-confirmed MCP calls first, bridged
				// native calls after them in arrival order.
				combined := make([]*pendingExec, 0, len(pending))
				combined = append(combined, ordered...)
				for _, item := range pending {
					if item.native {
						combined = append(combined, item)
					}
				}
				pending = combined
			}
			s.mu.Lock()
			s.pendingOrder = append([]*pendingExec(nil), pending...)
			s.mu.Unlock()
			return pending, true
		}
		// A checkpoint that does not line up must not wedge a forced boundary
		// (END_STREAM/EOF): fall back to arrival order there.
		if !force {
			return nil, false
		}
		return pending, true
	}
	if !force {
		return nil, false
	}
	return pending, true
}

func (s *Session) toolCallNameMatches(clientName, checkpointName string) bool {
	checkpointName = strings.TrimSpace(checkpointName)
	if strings.EqualFold(strings.TrimSpace(clientName), checkpointName) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(s.toolAlias.UpstreamName(clientName)), checkpointName)
}

func checkpointToolCalls(state *agentv1.ConversationStateStructure) ([]checkpointToolCall, bool) {
	if state == nil || len(state.GetPendingToolCalls()) == 0 {
		return nil, false
	}
	out := make([]checkpointToolCall, 0)
	seen := make(map[string]struct{})
	for _, raw := range state.GetPendingToolCalls() {
		var message struct {
			Role    string `json:"role"`
			Content []struct {
				Type       string `json:"type"`
				ToolCallID string `json:"toolCallId"`
				ToolName   string `json:"toolName"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(raw), &message) != nil || message.Role != "assistant" {
			continue
		}
		for _, part := range message.Content {
			if part.Type != "tool-call" || strings.TrimSpace(part.ToolCallID) == "" || strings.TrimSpace(part.ToolName) == "" {
				continue
			}
			if _, duplicate := seen[part.ToolCallID]; duplicate {
				return nil, false
			}
			seen[part.ToolCallID] = struct{}{}
			out = append(out, checkpointToolCall{id: part.ToolCallID, name: part.ToolName})
		}
	}
	return out, len(out) > 0
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
	for i, pending := range s.pendingOrder {
		if pending == item {
			s.pendingOrder = append(s.pendingOrder[:i], s.pendingOrder[i+1:]...)
			break
		}
	}
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
		// Everything else that maps tool names does so case-insensitively; a
		// miss here answers ToolNotFound and loses the call outright.
		lower := strings.ToLower(strings.TrimSpace(name))
		for indexed, candidate := range s.toolIndex {
			if strings.ToLower(indexed) == lower {
				def = candidate
				break
			}
		}
	}
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
	// The client's WebFetch is no longer declared as MCP; advertise the
	// built-in instead, whose fetch exec the bridge routes back to it.
	if findNativeClientTool("fetch", s.clientTools) != nil {
		trueVal := true
		ctx.WebFetchEnabled = &trueVal
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
		// The structured history is a complete resume input. A synthetic user
		// message changes the transcript and must not be added.
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
