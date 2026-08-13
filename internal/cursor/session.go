package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// MCPProviderIdentifier is the provider_identifier advertised for OpenAI tools.
const MCPProviderIdentifier = "cliproxyapi"

// ToolDefinition preserves the public client contract while selecting either a
// Cursor native typed bridge or the Cursor MCP carrier. Kind is the original
// API tool type (function/custom/shell/local_shell/apply_patch); Namespace is
// kept separate so an MCP leaf named Read is never mistaken for Cursor Read.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
	Kind        string
	Namespace   string
}

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	// Kind and Namespace retain the downstream API contract. They are not
	// Cursor MCP metadata: native Cursor Exec variants are projected directly to
	// shell/local_shell/apply_patch/function calls, while true custom leaves use
	// MCP and keep their original namespace/name.
	Kind      string
	Namespace string
}

// ToolResult is a client-provided tool response.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
	Kind       string
	Namespace  string
}

// StreamEvent is one Cursor→OpenAI segment event.
type StreamEvent struct {
	Type             string
	SegmentID        uint64
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
	request     *agentv1.ExecServerMessage
	call        ToolCall
	variant     string
	emitted     bool
	group       *nativeExecGroup
	index       int
	modelCallID string
	batch       *pendingToolBatch
	// provisional is true only while an Exec frame lacks every authoritative
	// lifecycle/checkpoint id. Such ids are internal and are rebound before a
	// formal batch is exposed whenever Cursor subsequently supplies one.
	provisional bool
}

// pendingToolBatch is the formal provider boundary. A model_call_id is the
// primary identity; a checkpoint is the authoritative member/order snapshot.
// Batches without either signal stay isolated by Exec identity and are never
// merged merely because END_STREAM/EOF arrived.
type pendingToolBatch struct {
	id              string
	modelCallID     string
	ordered         []*pendingExec
	checkpointCalls []checkpointToolCall
	checkpoint      bool
	terminal        bool
	emitted         bool
}

// nativeExecGroup represents one Cursor Exec that must be projected as more
// than one downstream call (currently PiEdit for single-edit clients). Cursor
// receives exactly one typed result after every public call has completed.
type nativeExecGroup struct {
	request *agentv1.ExecServerMessage
	variant string
	items   []*pendingExec
	results map[int]ToolResult
	sent    bool
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
	pending      map[string]*pendingExec
	pendingOrder []*pendingExec
	batches      map[string]*pendingToolBatch
	batchOrder   []*pendingToolBatch
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
	// Cursor can send TurnEnded after the public tool boundary. Keep counters
	// keyed by the Agent segment they belong to so a late previous-turn update
	// can never be charged to the next HTTP continuation.
	segmentID     uint64
	usageByID     map[uint64]StreamEvent
	usageSignals  map[uint64]chan struct{}
	usageRecorded map[uint64]struct{}
	// usageConsumed marks segments whose authoritative usage was already handed
	// out via the event channel (emitTerminalUsage) or TakeUsageForSegment. A
	// consumed segment must never be re-emitted or re-served.
	usageConsumed map[uint64]struct{}
	usagePending  []uint64

	// Checkpoint / text recovery (cursor2api): Cursor wire text_deltas often
	// miss a leading token; the authoritative assistant text lands in a later
	// conversation_checkpoint_update. We buffer deltas and flush once.
	conversationState   *agentv1.ConversationStateStructure
	visibleText         strings.Builder // all text seen from deltas
	emittedText         strings.Builder // text already sent to the client
	visibleThinking     strings.Builder // reasoning seen from deltas
	emittedThinking     strings.Builder // reasoning already sent to the client
	seenAssistantMsgIDs map[string]struct{}
	// The server may rewrite blob/message ids while preserving the ordered
	// assistant history. Content at the same history position is therefore the
	// stable identity used to suppress checkpoint replay.
	seenAssistantHistory []checkpointAssistantMessage
	turnEndedSeen        bool
	terminalUsage        StreamEvent
	terminalUsageReady   bool
	segmentEnded         bool
	textFlushed          bool
	thinkingFlushed      bool
	textStreamLive       bool
	holdTimer            *time.Timer
	resumed              bool
	// usedContinuation marks a run opened from a server checkpoint. If that
	// run fails, the stale checkpoint is dropped and the retry rebuilds from
	// the caller's complete history.
	usedContinuation bool
	// failed prevents a broken or rejected run from replacing a known-good
	// continuation checkpoint during Close.
	failed bool
	// lastFrame is the last time anything arrived from Cursor. Thinking and
	// text deltas are buffered rather than emitted, so consumer-side idle
	// detection must not rely on client-visible events alone.
	lastFrame time.Time
	// InteractionUpdate is the authoritative lifecycle envelope for native
	// tool ids and provider batch identity. Exec may arrive before or after it.
	lifecyclesByID   map[string]*nativeLifecycle
	lifecycleOrder   []*nativeLifecycle
	serverOwnedTodos map[string]*nativeLifecycle
}

type nativeLifecycle struct {
	callID      string
	envelopeID  string
	nestedID    string
	modelCallID string
	name        string
	variant     string
	completed   bool
	serverOwned bool
	pending     *pendingExec
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
	conversationID string
	parallelTools  bool
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
	tools = SanitizeToolDefinitions(tools)
	selection := ResolveRequestedModel(model)
	var clientMsg *agentv1.AgentClientMessage
	var blobStore map[string][]byte
	conversationID := strings.TrimSpace(cfg.conversationID)
	usedContinuation := false
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
	// Tool schemas are backed by protobuf maps. Deterministic encoding keeps an
	// unchanged cached prefix byte-identical across requests.
	first, err := (proto.MarshalOptions{Deterministic: true}).Marshal(clientMsg)
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
		segmentID:           1,
		usageByID:           map[uint64]StreamEvent{},
		usageSignals:        map[uint64]chan struct{}{},
		usageRecorded:       map[uint64]struct{}{},
		usageConsumed:       map[uint64]struct{}{},
		seenAssistantMsgIDs: map[string]struct{}{},
		usedContinuation:    usedContinuation,
		batches:             map[string]*pendingToolBatch{},
		batchOrder:          make([]*pendingToolBatch, 0),
		lifecyclesByID:      map[string]*nativeLifecycle{},
		lifecycleOrder:      make([]*nativeLifecycle, 0),
		serverOwnedTodos:    map[string]*nativeLifecycle{},
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
		name := toolWireName(tools[i])
		if name == "" {
			continue
		}
		out[name] = &tools[i]
	}
	return out
}

func toolWireName(tool ToolDefinition) string {
	return strings.TrimSpace(tool.Name)
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
					s.emitTerminalUsage(s.SegmentID())
					s.beginWaitingTools()
					s.mu.Lock()
					s.segmentEnded = true
					s.mu.Unlock()
					s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
				}
			}
			if sawEndStream {
				s.markPendingBatchesTerminal()
				if s.emitPendingToolBatch(true) {
					s.emitTerminalUsage(s.SegmentID())
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
				s.emitTerminalUsage(s.SegmentID())
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
			s.markPendingBatchesTerminal()
			if s.emitPendingToolBatch(true) {
				s.emitTerminalUsage(s.SegmentID())
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
			s.emitTerminalUsage(s.SegmentID())
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
			// A dying stream while the run is paused for client tools is the
			// EOF situation with a different error string — Cursor's side can
			// drop the idle stream with RST_STREAM (INTERNAL_ERROR) instead of
			// a clean close. The tool calls were already delivered; failing
			// here would kill the session, drop the continuation checkpoint
			// and strand the client. Park like EOF does and let
			// SubmitToolResults rebuild.
			s.markPendingBatchesTerminal()
			if s.emitPendingToolBatch(true) {
				s.emitTerminalUsage(s.SegmentID())
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
				log.Warnf("cursor: stream error while awaiting client tool results (pending=%d): %v; will rebuild after tool results",
					s.pendingToolCount(), errRead)
				s.beginWaitingTools()
				continue
			}
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

func (s *Session) recordUsage(ev StreamEvent) StreamEvent {
	if s == nil {
		return ev
	}
	s.mu.Lock()
	if s.segmentID == 0 {
		s.segmentID = 1
	}
	if s.usageByID == nil {
		s.usageByID = map[uint64]StreamEvent{}
	}
	if s.usageSignals == nil {
		s.usageSignals = map[uint64]chan struct{}{}
	}
	if s.usageRecorded == nil {
		s.usageRecorded = map[uint64]struct{}{}
	}
	if len(s.usagePending) > 0 {
		ev.SegmentID = s.usagePending[0]
		s.usagePending = s.usagePending[1:]
	} else {
		ev.SegmentID = s.segmentID
	}
	s.usageByID[ev.SegmentID] = ev
	s.usageRecorded[ev.SegmentID] = struct{}{}
	signal := s.usageSignals[ev.SegmentID]
	delete(s.usageSignals, ev.SegmentID)
	s.mu.Unlock()
	if signal != nil {
		close(signal)
	}
	return ev
}

// SegmentID returns the current public Agent segment identity.
func (s *Session) SegmentID() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.segmentID == 0 {
		s.segmentID = 1
	}
	return s.segmentID
}

// TakeUsage returns the counters for the current turn, waiting up to d for
// them. New code should capture SegmentID before consuming a segment and call
// TakeUsageForSegment so a later continuation cannot observe older counters.
//
// Cursor reports TurnEnded after it has already asked the client to run a tool,
// so a caller that stops at the tool call sees nothing unless it waits a moment
// for the trailing frame.
func (s *Session) TakeUsage(d time.Duration) (StreamEvent, bool) {
	if s == nil {
		return StreamEvent{}, false
	}
	return s.TakeUsageForSegment(s.SegmentID(), d)
}

// TakeUsageForSegment returns exactly one authoritative TurnEnded update for
// segmentID. It never falls through to counters from another segment, and it
// never returns counters already handed out through the event channel for the
// same segment (emitTerminalUsage).
func (s *Session) TakeUsageForSegment(segmentID uint64, d time.Duration) (StreamEvent, bool) {
	if s == nil || segmentID == 0 {
		return StreamEvent{}, false
	}
	s.mu.Lock()
	if s.usageConsumed != nil {
		if _, consumed := s.usageConsumed[segmentID]; consumed {
			s.mu.Unlock()
			return StreamEvent{}, false
		}
	}
	if ev, ok := s.usageByID[segmentID]; ok {
		delete(s.usageByID, segmentID)
		s.markConsumed(segmentID)
		s.mu.Unlock()
		return ev, true
	}
	if d <= 0 {
		s.mu.Unlock()
		return StreamEvent{}, false
	}
	if s.usageSignals == nil {
		s.usageSignals = map[uint64]chan struct{}{}
	}
	signal := s.usageSignals[segmentID]
	if signal == nil {
		signal = make(chan struct{})
		s.usageSignals[segmentID] = signal
	}
	s.mu.Unlock()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usageConsumed != nil {
		if _, consumed := s.usageConsumed[segmentID]; consumed {
			return StreamEvent{}, false
		}
	}
	ev, ok := s.usageByID[segmentID]
	if !ok {
		return StreamEvent{}, false
	}
	delete(s.usageByID, segmentID)
	s.markConsumed(segmentID)
	return ev, true
}

// markConsumed records that a segment's authoritative usage has been handed out
// exactly once. A nil map is tolerated so bare &Session{} unit instances and
// legacy call paths never panic.
func (s *Session) markConsumed(segmentID uint64) {
	if s.usageConsumed == nil {
		s.usageConsumed = map[uint64]struct{}{}
	}
	s.usageConsumed[segmentID] = struct{}{}
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

// parkedForRebuild reports a session whose upstream response already ended
// while client tools were pending: resuming it always rebuilds, so it only
// needs to live long enough to route that rebuild.
func (s *Session) parkedForRebuild() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return (s.waitingTools || len(s.pending) > 0) && s.responseEnded
}

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

// emitTerminalUsage publishes the authoritative TurnEnded counters for the
// segment currently being ended (segmentID). Cursor may deliver a previous
// turn's TurnEnded after the public tool boundary, so the pending single slot
// is only surfaced when it actually belongs to this segment. Once delivered,
// the segment is marked consumed: a later TakeUsageForSegment can never return
// the same counters, and a later segment cannot inherit them.
func (s *Session) emitTerminalUsage(segmentID uint64) {
	s.mu.Lock()
	if !s.terminalUsageReady {
		s.mu.Unlock()
		return
	}
	ev := s.terminalUsage
	// Guard against cross-segment leakage: if the pending slot belongs to a
	// different (older) segment than the one being ended now, leave it for that
	// segment's own boundary or its TailSegment wait, never this one.
	if ev.SegmentID != 0 && segmentID != 0 && ev.SegmentID != segmentID {
		s.mu.Unlock()
		return
	}
	s.terminalUsageReady = false
	s.terminalUsage = StreamEvent{}
	// The event channel now owns delivery for this segment; remove the
	// out-of-band fallback copy so it cannot leak into a continuation.
	if ev.SegmentID != 0 {
		delete(s.usageByID, ev.SegmentID)
		if s.usageConsumed == nil {
			s.usageConsumed = map[uint64]struct{}{}
		}
		s.usageConsumed[ev.SegmentID] = struct{}{}
	}
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
		selectedBatches := map[*pendingToolBatch]struct{}{}
		for _, item := range pending {
			if item != nil && item.batch != nil {
				selectedBatches[item.batch] = struct{}{}
			}
		}
		for _, item := range s.pendingOrder {
			if item == nil || !item.emitted {
				continue
			}
			if len(selectedBatches) > 0 {
				if _, sameBatch := selectedBatches[item.batch]; !sameBatch {
					continue
				}
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
				if pending[i].variant == "mcp_args" {
					_ = s.sendMcpResult(pending[i].request, result)
				} else {
					_, _ = s.submitNativeGroupResult(pending[i], result)
				}
			}
			s.releasePendingExec(pending[i], ids[i], results[i].ToolCallID)
		}
		_ = s.Close()
		return ErrToolResumeRebuild
	}

	for i, result := range results {
		item := pending[i]
		var err error
		completed := true
		if item.variant == "mcp_args" {
			err = s.sendMcpResult(item.request, result)
		} else {
			completed, err = s.submitNativeGroupResult(item, result)
		}
		if err != nil {
			// The Cursor request stream may fail transiently after the client has
			// already executed the tool. Keep every pending binding intact so the
			// exact structured result can be retried or rebuilt from history.
			return fmt.Errorf("cursor: submit tool result %s: %w", ids[i], err)
		}
		if completed {
			s.releaseNativeGroup(item, ids[i], result.ToolCallID)
		}
	}
	// If Cursor produced another formal provider batch before pausing the
	// connection, expose that next boundary without resuming the read side. This
	// is distinct from parallel members inside the batch just completed.
	if s.nextPendingBatchReady() && s.emitPendingToolBatch(true) {
		s.beginWaitingTools()
		s.mu.Lock()
		s.segmentEnded = true
		s.mu.Unlock()
		s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
		return nil
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
	if s.segmentID == 0 {
		s.segmentID = 1
	}
	if s.usageRecorded == nil {
		s.usageRecorded = map[uint64]struct{}{}
	}
	if _, recorded := s.usageRecorded[s.segmentID]; !recorded {
		alreadyPending := false
		for _, pendingID := range s.usagePending {
			if pendingID == s.segmentID {
				alreadyPending = true
				break
			}
		}
		if !alreadyPending {
			s.usagePending = append(s.usagePending, s.segmentID)
		}
	}
	s.segmentID++
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

func (s *Session) submitNativeGroupResult(item *pendingExec, result ToolResult) (bool, error) {
	if item == nil || item.group == nil || len(item.group.items) <= 1 {
		return true, s.sendNativeResult(item.request, item.variant, result)
	}
	group := item.group
	s.mu.Lock()
	previous, hadPrevious := group.results[item.index]
	previousSent := group.sent
	group.results[item.index] = result
	if group.sent || len(group.results) < len(group.items) {
		s.mu.Unlock()
		return false, nil
	}
	group.sent = true
	ordered := make([]ToolResult, 0, len(group.items))
	for i := range group.items {
		ordered = append(ordered, group.results[i])
	}
	s.mu.Unlock()
	combined := combineToolResults(ordered)
	if err := s.sendNativeResult(group.request, group.variant, combined); err != nil {
		s.mu.Lock()
		group.sent = previousSent
		if hadPrevious {
			group.results[item.index] = previous
		} else {
			delete(group.results, item.index)
		}
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}

func combineToolResults(results []ToolResult) ToolResult {
	combined := ToolResult{}
	parts := make([]string, 0, len(results))
	for _, result := range results {
		if combined.ToolCallID == "" {
			combined.ToolCallID = result.ToolCallID
		}
		if combined.Name == "" {
			combined.Name = result.Name
		}
		combined.IsError = combined.IsError || result.IsError
		if result.Content != "" {
			parts = append(parts, result.Content)
		}
	}
	combined.Content = strings.Join(parts, "\n")
	return combined
}

func (s *Session) releaseNativeGroup(item *pendingExec, keys ...string) {
	if item == nil || item.group == nil || len(item.group.items) <= 1 {
		s.releasePendingExec(item, keys...)
		return
	}
	for _, member := range item.group.items {
		s.releasePendingExec(member, member.call.ID)
	}
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

func (s *Session) sendNativeResult(req *agentv1.ExecServerMessage, variant string, result ToolResult) error {
	execClient := &agentv1.ExecClientMessage{Id: req.Id, ExecId: req.ExecId}
	content := result.Content
	switch variant {
	case "shell_stream_args":
		return s.sendShellStreamResult(req, result)
	case "shell_args", "mini_swe_agent_bash_args":
		args := nativeShellArgs(req, variant)
		value := &agentv1.ShellResult{}
		if result.IsError {
			value.Result = &agentv1.ShellResult_Failure{Failure: &agentv1.ShellFailure{
				Command: args.GetCommand(), WorkingDirectory: args.GetWorkingDirectory(),
				ExitCode: 1, Stderr: content,
			}}
		} else {
			value.Result = &agentv1.ShellResult_Success{Success: &agentv1.ShellSuccess{
				Command: args.GetCommand(), WorkingDirectory: args.GetWorkingDirectory(),
				Stdout: content,
			}}
		}
		if variant == "mini_swe_agent_bash_args" {
			execClient.Message = &agentv1.ExecClientMessage_MiniSweAgentBashResult{MiniSweAgentBashResult: value}
		} else {
			execClient.Message = &agentv1.ExecClientMessage_ShellResult{ShellResult: value}
		}
	case "read_args":
		args := req.GetReadArgs()
		value := &agentv1.ReadResult{}
		if result.IsError {
			value.Result = &agentv1.ReadResult_Error{Error: &agentv1.ReadError{Path: args.GetPath(), Error: content}}
		} else {
			value.Result = &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
				Path: args.GetPath(), Content: content, TotalLines: int32(lineCount(content)),
				FileSize: int64(len([]byte(content))), RangeApplied: args.Offset != nil || args.Limit != nil,
			}}
		}
		execClient.Message = &agentv1.ExecClientMessage_ReadResult{ReadResult: value}
	case "write_args":
		args := req.GetWriteArgs()
		value := &agentv1.WriteResult{}
		if result.IsError {
			value.Result = &agentv1.WriteResult_Error{Error: &agentv1.WriteError{Path: args.GetPath(), Error: content}}
		} else {
			success := &agentv1.WriteSuccess{Path: args.GetPath(), LinesCreated: int32(lineCount(args.GetFileText())), FileSize: int32(len(args.GetFileBytes()) + len([]byte(args.GetFileText())))}
			if args.GetReturnFileContentAfterWrite() {
				updated := args.GetFileText()
				success.FileContentAfterWrite = &updated
			}
			value.Result = &agentv1.WriteResult_Success{Success: success}
		}
		execClient.Message = &agentv1.ExecClientMessage_WriteResult{WriteResult: value}
	case "delete_args":
		args := req.GetDeleteArgs()
		value := &agentv1.DeleteResult{}
		if result.IsError {
			value.Result = &agentv1.DeleteResult_Error{Error: &agentv1.DeleteError{Path: args.GetPath(), Error: content}}
		} else {
			value.Result = &agentv1.DeleteResult_Success{Success: &agentv1.DeleteSuccess{Path: args.GetPath(), DeletedFile: args.GetPath()}}
		}
		execClient.Message = &agentv1.ExecClientMessage_DeleteResult{DeleteResult: value}
	case "grep_args":
		args := req.GetGrepArgs()
		value := &agentv1.GrepResult{}
		if result.IsError {
			value.Result = &agentv1.GrepResult_Error{Error: &agentv1.GrepError{Error: content}}
		} else {
			value.Result = &agentv1.GrepResult_Success{Success: buildGrepSuccess(args, content)}
		}
		execClient.Message = &agentv1.ExecClientMessage_GrepResult{GrepResult: value}
	case "ls_args":
		args := req.GetLsArgs()
		value := &agentv1.LsResult{}
		if result.IsError {
			value.Result = &agentv1.LsResult_Error{Error: &agentv1.LsError{Path: args.GetPath(), Error: content}}
		} else {
			value.Result = &agentv1.LsResult_Success{Success: &agentv1.LsSuccess{DirectoryTreeRoot: buildLsTree(args.GetPath(), content)}}
		}
		execClient.Message = &agentv1.ExecClientMessage_LsResult{LsResult: value}
	case "diagnostics_args":
		args := req.GetDiagnosticsArgs()
		value := &agentv1.DiagnosticsResult{}
		if result.IsError {
			value.Result = &agentv1.DiagnosticsResult_Error{Error: &agentv1.DiagnosticsError{Path: args.GetPath(), Error: content}}
		} else {
			value.Result = &agentv1.DiagnosticsResult_Success{Success: buildDiagnosticsSuccess(args.GetPath(), content)}
		}
		execClient.Message = &agentv1.ExecClientMessage_DiagnosticsResult{DiagnosticsResult: value}
	case "fetch_args":
		args := req.GetFetchArgs()
		value := &agentv1.FetchResult{}
		if result.IsError {
			value.Result = &agentv1.FetchResult_Error{Error: &agentv1.FetchError{Url: args.GetUrl(), Error: content}}
		} else {
			value.Result = &agentv1.FetchResult_Success{Success: buildFetchSuccess(args.GetUrl(), content)}
		}
		execClient.Message = &agentv1.ExecClientMessage_FetchResult{FetchResult: value}
	case "pi_read_args":
		value := &agentv1.PiReadExecResult{}
		if result.IsError {
			value.Result = &agentv1.PiReadExecResult_Error{Error: &agentv1.PiReadExecError{Error: content}}
		} else {
			value.Result = &agentv1.PiReadExecResult_Success{Success: &agentv1.PiReadExecSuccess{Output: content}}
		}
		execClient.Message = &agentv1.ExecClientMessage_PiReadResult{PiReadResult: value}
	case "pi_bash_args":
		value := &agentv1.PiBashExecResult{}
		if result.IsError {
			value.Result = &agentv1.PiBashExecResult_Error{Error: &agentv1.PiBashExecError{Error: content}}
		} else {
			value.Result = &agentv1.PiBashExecResult_Success{Success: &agentv1.PiBashExecSuccess{Output: content}}
		}
		execClient.Message = &agentv1.ExecClientMessage_PiBashResult{PiBashResult: value}
	case "pi_edit_args":
		value := &agentv1.PiEditExecResult{}
		if result.IsError {
			value.Result = &agentv1.PiEditExecResult_Error{Error: &agentv1.PiEditExecError{Error: content}}
		} else {
			value.Result = &agentv1.PiEditExecResult_Success{Success: &agentv1.PiEditExecSuccess{Output: content}}
		}
		execClient.Message = &agentv1.ExecClientMessage_PiEditResult{PiEditResult: value}
	case "pi_write_args":
		value := &agentv1.PiWriteExecResult{}
		if result.IsError {
			value.Result = &agentv1.PiWriteExecResult_Error{Error: &agentv1.PiWriteExecError{Error: content}}
		} else {
			value.Result = &agentv1.PiWriteExecResult_Success{Success: &agentv1.PiWriteExecSuccess{Output: content}}
		}
		execClient.Message = &agentv1.ExecClientMessage_PiWriteResult{PiWriteResult: value}
	case "pi_grep_args":
		value := &agentv1.PiGrepExecResult{}
		if result.IsError {
			value.Result = &agentv1.PiGrepExecResult_Error{Error: &agentv1.PiGrepExecError{Error: content}}
		} else {
			value.Result = &agentv1.PiGrepExecResult_Success{Success: &agentv1.PiGrepExecSuccess{Output: content}}
		}
		execClient.Message = &agentv1.ExecClientMessage_PiGrepResult{PiGrepResult: value}
	case "pi_find_args":
		value := &agentv1.PiFindExecResult{}
		if result.IsError {
			value.Result = &agentv1.PiFindExecResult_Error{Error: &agentv1.PiFindExecError{Error: content}}
		} else {
			value.Result = &agentv1.PiFindExecResult_Success{Success: &agentv1.PiFindExecSuccess{Output: content}}
		}
		execClient.Message = &agentv1.ExecClientMessage_PiFindResult{PiFindResult: value}
	case "pi_ls_args":
		value := &agentv1.PiLsExecResult{}
		if result.IsError {
			value.Result = &agentv1.PiLsExecResult_Error{Error: &agentv1.PiLsExecError{Error: content}}
		} else {
			value.Result = &agentv1.PiLsExecResult_Success{Success: &agentv1.PiLsExecSuccess{Output: content}}
		}
		execClient.Message = &agentv1.ExecClientMessage_PiLsResult{PiLsResult: value}
	case "subagent_args":
		args := req.GetSubagentArgs()
		value := &agentv1.SubagentResult{}
		agentID := firstNonEmpty(args.GetResumeAgentId(), args.GetToolCallId(), req.GetExecId(), fmt.Sprintf("subagent-%d", req.GetId()))
		if result.IsError {
			value.Result = &agentv1.SubagentResult_Error{Error: &agentv1.SubagentError{AgentId: &agentID, Error: content}}
		} else {
			final := nativeFinalMessage(content)
			value.Result = &agentv1.SubagentResult_Success{Success: &agentv1.SubagentSuccess{AgentId: agentID, FinalMessage: &final}}
		}
		execClient.Message = &agentv1.ExecClientMessage_SubagentResult{SubagentResult: value}
	default:
		return fmt.Errorf("cursor: unsupported native result variant %s", variant)
	}
	client := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient}}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

// nativeFinalMessage accepts the structured result shapes emitted by Codex,
// Claude Code and common Task tools. It prefers a real final payload over
// status/progress text while preserving arbitrary plain-text results.
func nativeFinalMessage(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	var value any
	if json.Unmarshal([]byte(trimmed), &value) != nil {
		return content
	}
	if final := findNativeFinalMessage(value); final != "" {
		return final
	}
	return content
}

func findNativeFinalMessage(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		var parts []string
		for _, item := range typed {
			if text := findNativeFinalMessage(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"final_message", "finalMessage", "result", "output", "content", "message", "summary"} {
			if candidate, ok := typed[key]; ok {
				if text := findNativeFinalMessage(candidate); text != "" {
					return text
				}
			}
		}
		for _, key := range []string{"data", "response", "success"} {
			if candidate, ok := typed[key]; ok {
				if text := findNativeFinalMessage(candidate); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func buildDiagnosticsSuccess(path, content string) *agentv1.DiagnosticsSuccess {
	success := &agentv1.DiagnosticsSuccess{Path: path}
	payload, structured := toolResultObject(content)
	if nested, ok := payload["success"].(map[string]any); ok {
		payload = nested
		structured = true
	}
	if value := stringField(payload, "path", "file", "file_path"); value != "" {
		success.Path = value
	}
	rawDiagnostics, found := firstField(payload, "diagnostics", "items", "problems")
	if found {
		switch values := rawDiagnostics.(type) {
		case []any:
			for _, raw := range values {
				if diagnostic := diagnosticFromValue(raw); diagnostic != nil {
					success.Diagnostics = append(success.Diagnostics, diagnostic)
				}
			}
		case map[string]any:
			if diagnostic := diagnosticFromValue(values); diagnostic != nil {
				success.Diagnostics = append(success.Diagnostics, diagnostic)
			}
		case string:
			if strings.TrimSpace(values) != "" {
				success.Diagnostics = append(success.Diagnostics, &agentv1.Diagnostic{Message: values})
			}
		}
	} else if diagnostic := diagnosticFromValue(payload); diagnostic != nil && diagnostic.GetMessage() != "" {
		success.Diagnostics = append(success.Diagnostics, diagnostic)
	} else if strings.TrimSpace(content) != "" && !structured {
		// A text-only callback cannot express exact LSP ranges, but it is still a
		// successful client execution. Preserve its useful output in an
		// unspecified diagnostic instead of turning the whole tool round into an
		// error.
		success.Diagnostics = append(success.Diagnostics, &agentv1.Diagnostic{Message: content})
	}
	if total, ok := integerField(payload, "total_diagnostics", "totalDiagnostics", "total", "count"); ok {
		success.TotalDiagnostics = int32(total)
	} else {
		success.TotalDiagnostics = int32(len(success.Diagnostics))
	}
	return success
}

func diagnosticFromValue(value any) *agentv1.Diagnostic {
	payload, ok := value.(map[string]any)
	if !ok || payload == nil {
		return nil
	}
	diagnostic := &agentv1.Diagnostic{
		Severity: diagnosticSeverity(firstValue(payload, "severity", "level")),
		Message:  stringField(payload, "message", "text", "description"),
		Source:   stringField(payload, "source", "provider"),
		Code:     stringField(payload, "code", "rule", "rule_id", "ruleId"),
	}
	if stale, exists := boolField(payload, "is_stale", "isStale", "stale"); exists {
		diagnostic.IsStale = stale
	}
	if rawRange, exists := firstField(payload, "range", "location"); exists {
		diagnostic.Range = diagnosticRange(rawRange)
	}
	return diagnostic
}

func diagnosticSeverity(value any) agentv1.DiagnosticSeverity {
	switch typed := value.(type) {
	case float64:
		if typed >= 0 && typed <= 4 {
			return agentv1.DiagnosticSeverity(int32(typed))
		}
	case int:
		if typed >= 0 && typed <= 4 {
			return agentv1.DiagnosticSeverity(typed)
		}
	case int32:
		if typed >= 0 && typed <= 4 {
			return agentv1.DiagnosticSeverity(typed)
		}
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		normalized = strings.TrimPrefix(normalized, "diagnostic_severity_")
		switch normalized {
		case "1", "error", "err":
			return agentv1.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_ERROR
		case "2", "warning", "warn":
			return agentv1.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_WARNING
		case "3", "information", "info":
			return agentv1.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_INFORMATION
		case "4", "hint":
			return agentv1.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_HINT
		}
	}
	return agentv1.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_UNSPECIFIED
}

func diagnosticRange(value any) *agentv1.Range {
	payload, ok := value.(map[string]any)
	if !ok || payload == nil {
		return nil
	}
	start, startOK := diagnosticPosition(payload["start"])
	end, endOK := diagnosticPosition(payload["end"])
	if !startOK && !endOK {
		line, hasLine := integerField(payload, "line", "row")
		column, hasColumn := integerField(payload, "column", "character", "col")
		if !hasLine && !hasColumn {
			return nil
		}
		start = &agentv1.Position{Line: uint32(maxInt64(line, 0)), Column: uint32(maxInt64(column, 0))}
		end = proto.Clone(start).(*agentv1.Position)
		return &agentv1.Range{Start: start, End: end}
	}
	if !startOK {
		start = &agentv1.Position{}
	}
	if !endOK {
		end = proto.Clone(start).(*agentv1.Position)
	}
	return &agentv1.Range{Start: start, End: end}
}

func diagnosticPosition(value any) (*agentv1.Position, bool) {
	payload, ok := value.(map[string]any)
	if !ok || payload == nil {
		return nil, false
	}
	line, hasLine := integerField(payload, "line", "row")
	column, hasColumn := integerField(payload, "column", "character", "col")
	if !hasLine && !hasColumn {
		return nil, false
	}
	return &agentv1.Position{
		Line:   uint32(maxInt64(line, 0)),
		Column: uint32(maxInt64(column, 0)),
	}, true
}

func buildFetchSuccess(url, content string) *agentv1.FetchSuccess {
	success := &agentv1.FetchSuccess{
		Url:         url,
		Content:     content,
		StatusCode:  200,
		ContentType: "text/plain; charset=utf-8",
	}
	payload, structured := toolResultObject(content)
	if nested, ok := payload["success"].(map[string]any); ok {
		payload = nested
		structured = true
	}
	_, hasContent := firstField(payload, "content", "body", "text")
	_, hasStatus := firstField(payload, "status_code", "statusCode", "status")
	_, hasType := firstField(payload, "content_type", "contentType", "mime_type", "mimeType")
	_, hasURL := firstField(payload, "url", "final_url", "finalUrl")
	if !structured && !hasContent && !hasStatus && !hasType && !hasURL {
		return success
	}
	if value := stringField(payload, "url", "final_url", "finalUrl"); value != "" {
		success.Url = value
	}
	if value, exists := firstField(payload, "content", "body", "text"); exists {
		success.Content = stringifyToolResultValue(value)
	}
	if status, exists := integerField(payload, "status_code", "statusCode", "status"); exists && status > 0 {
		success.StatusCode = int32(status)
	}
	if value := stringField(payload, "content_type", "contentType", "mime_type", "mimeType"); value != "" {
		success.ContentType = value
	}
	return success
}

func toolResultObject(content string) (map[string]any, bool) {
	if strings.TrimSpace(content) == "" {
		return map[string]any{}, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil || payload == nil {
		return map[string]any{}, false
	}
	return payload, true
}

func firstValue(payload map[string]any, keys ...string) any {
	value, _ := firstField(payload, keys...)
	return value
}

func firstField(payload map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, exists := payload[key]; exists {
			return value, true
		}
	}
	return nil, false
}

func stringField(payload map[string]any, keys ...string) string {
	value, exists := firstField(payload, keys...)
	if !exists || value == nil {
		return ""
	}
	return stringifyToolResultValue(value)
}

func stringifyToolResultValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(typed)
		if err == nil {
			return string(encoded)
		}
		return fmt.Sprint(typed)
	}
}

func integerField(payload map[string]any, keys ...string) (int64, bool) {
	value, exists := firstField(payload, keys...)
	if !exists {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed <= uint64(^uint64(0)>>1) {
			return int64(typed), true
		}
	case string:
		var parsed int64
		if _, err := fmt.Sscan(strings.TrimSpace(typed), &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func boolField(payload map[string]any, keys ...string) (bool, bool) {
	value, exists := firstField(payload, keys...)
	if !exists {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	case float64:
		return typed != 0, true
	}
	return false, false
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (s *Session) sendShellStreamResult(req *agentv1.ExecServerMessage, result ToolResult) error {
	if result.Content != "" {
		if result.IsError {
			if err := s.sendShellStreamEvent(req, shellStreamStderr(result.Content)); err != nil {
				return err
			}
		} else if err := s.sendShellStreamEvent(req, shellStreamStdout(result.Content)); err != nil {
			return err
		}
	}
	code := uint32(0)
	if result.IsError {
		code = 1
	}
	if err := s.sendShellStreamEvent(req, shellStreamExit{code: code, cwd: req.GetShellStreamArgs().GetWorkingDirectory()}); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

func (s *Session) sendShellStreamEvent(req *agentv1.ExecServerMessage, event isShellStreamEvent) error {
	stream := &agentv1.ShellStream{}
	switch value := event.(type) {
	case shellStreamStart:
		stream.Event = &agentv1.ShellStream_Start{Start: &agentv1.ShellStreamStart{}}
	case shellStreamStdout:
		stream.Event = &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: string(value)}}
	case shellStreamStderr:
		stream.Event = &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: string(value)}}
	case shellStreamExit:
		stream.Event = &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: value.code, Cwd: value.cwd, Aborted: value.aborted}}
	}
	client := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: &agentv1.ExecClientMessage{
		Id: req.Id, ExecId: req.ExecId, Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: stream},
	}}}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return s.stream.WriteEnvelope(payload, false)
}

type isShellStreamEvent interface{ shellStreamEvent() }
type shellStreamStart struct{}
type shellStreamStdout string
type shellStreamStderr string
type shellStreamExit struct {
	code    uint32
	cwd     string
	aborted bool
}

func (shellStreamStart) shellStreamEvent()  {}
func (shellStreamStdout) shellStreamEvent() {}
func (shellStreamStderr) shellStreamEvent() {}
func (shellStreamExit) shellStreamEvent()   {}

func buildGrepSuccess(args *agentv1.GrepArgs, content string) *agentv1.GrepSuccess {
	mode := strings.TrimSpace(args.GetOutputMode())
	if mode == "" {
		mode = "content"
	}
	path := strings.TrimSpace(args.GetPath())
	if path == "" {
		path = "."
	}
	result := &agentv1.GrepSuccess{Pattern: args.GetPattern(), Path: path, OutputMode: mode, WorkspaceResults: map[string]*agentv1.GrepUnionResult{}}
	workspace := "default"
	switch mode {
	case "files_with_matches", "files":
		files := nonEmptyLines(content)
		result.WorkspaceResults[workspace] = &agentv1.GrepUnionResult{Result: &agentv1.GrepUnionResult_Files{Files: &agentv1.GrepFilesResult{Files: files, TotalFiles: int32(len(files))}}}
	case "count":
		counts := make([]*agentv1.GrepFileCount, 0)
		var total int32
		for _, line := range nonEmptyLines(content) {
			file, rawCount, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			var count int32
			if _, err := fmt.Sscan(strings.TrimSpace(rawCount), &count); err != nil {
				continue
			}
			counts = append(counts, &agentv1.GrepFileCount{File: strings.TrimSpace(file), Count: count})
			total += count
		}
		result.WorkspaceResults[workspace] = &agentv1.GrepUnionResult{Result: &agentv1.GrepUnionResult_Count{Count: &agentv1.GrepCountResult{Counts: counts, TotalFiles: int32(len(counts)), TotalMatches: total}}}
	default:
		byFile := map[string][]*agentv1.GrepContentMatch{}
		for _, line := range nonEmptyLines(content) {
			file, lineNumber, text := parseGrepLine(line)
			byFile[file] = append(byFile[file], &agentv1.GrepContentMatch{LineNumber: lineNumber, Content: text})
		}
		files := make([]string, 0, len(byFile))
		for file := range byFile {
			files = append(files, file)
		}
		sort.Strings(files)
		matches := make([]*agentv1.GrepFileMatch, 0, len(files))
		var total int32
		for _, file := range files {
			matches = append(matches, &agentv1.GrepFileMatch{File: file, Matches: byFile[file]})
			total += int32(len(byFile[file]))
		}
		result.WorkspaceResults[workspace] = &agentv1.GrepUnionResult{Result: &agentv1.GrepUnionResult_Content{Content: &agentv1.GrepContentResult{Matches: matches, TotalLines: total, TotalMatchedLines: total}}}
	}
	return result
}

func parseGrepLine(line string) (string, int32, string) {
	parts := strings.SplitN(line, ":", 3)
	if len(parts) < 3 {
		return "<output>", 0, line
	}
	var number int32
	if _, err := fmt.Sscan(strings.TrimSpace(parts[1]), &number); err != nil {
		return strings.TrimSpace(parts[0]), 0, strings.Join(parts[1:], ":")
	}
	return strings.TrimSpace(parts[0]), number, parts[2]
}

func nonEmptyLines(content string) []string {
	lines := make([]string, 0)
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func buildLsTree(root, content string) *agentv1.LsDirectoryTreeNode {
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	node := &agentv1.LsDirectoryTreeNode{AbsPath: root, ChildrenWereProcessed: true, FullSubtreeExtensionCounts: map[string]int32{}}
	for _, line := range nonEmptyLines(content) {
		line = strings.TrimPrefix(line, root)
		line = strings.TrimLeft(line, "\\/")
		if line == "" {
			continue
		}
		parts := strings.FieldsFunc(line, func(r rune) bool { return r == '/' || r == '\\' })
		if len(parts) == 0 {
			continue
		}
		insertLsPath(node, parts, strings.HasSuffix(line, "/") || strings.HasSuffix(line, "\\"))
	}
	finalizeLsTree(node)
	return node
}

func insertLsPath(root *agentv1.LsDirectoryTreeNode, parts []string, directory bool) {
	current := root
	for i, part := range parts {
		last := i == len(parts)-1
		if last && !directory {
			current.ChildrenFiles = append(current.ChildrenFiles, &agentv1.LsDirectoryTreeNode_File{Name: part})
			return
		}
		var child *agentv1.LsDirectoryTreeNode
		for _, candidate := range current.ChildrenDirs {
			if strings.EqualFold(basePath(candidate.AbsPath), part) {
				child = candidate
				break
			}
		}
		if child == nil {
			child = &agentv1.LsDirectoryTreeNode{AbsPath: strings.TrimRight(current.AbsPath, "\\/") + "/" + part, ChildrenWereProcessed: true, FullSubtreeExtensionCounts: map[string]int32{}}
			current.ChildrenDirs = append(current.ChildrenDirs, child)
		}
		current = child
	}
}

func basePath(value string) string {
	value = strings.TrimRight(value, "\\/")
	if index := strings.LastIndexAny(value, "\\/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

func finalizeLsTree(node *agentv1.LsDirectoryTreeNode) map[string]int32 {
	sort.Slice(node.ChildrenDirs, func(i, j int) bool { return node.ChildrenDirs[i].AbsPath < node.ChildrenDirs[j].AbsPath })
	sort.Slice(node.ChildrenFiles, func(i, j int) bool { return node.ChildrenFiles[i].Name < node.ChildrenFiles[j].Name })
	counts := map[string]int32{}
	node.NumFiles = int32(len(node.ChildrenFiles))
	for _, file := range node.ChildrenFiles {
		ext := strings.ToLower(strings.TrimPrefix(pathExtension(file.Name), "."))
		counts[ext]++
	}
	for _, child := range node.ChildrenDirs {
		for ext, count := range finalizeLsTree(child) {
			counts[ext] += count
		}
		node.NumFiles += child.NumFiles
	}
	node.FullSubtreeExtensionCounts = counts
	return counts
}

func pathExtension(value string) string {
	name := basePath(value)
	if index := strings.LastIndex(name, "."); index >= 0 {
		return name[index:]
	}
	return ""
}

func nativeShellArgs(req *agentv1.ExecServerMessage, variant string) *agentv1.ShellArgs {
	switch variant {
	case "shell_stream_args":
		return req.GetShellStreamArgs()
	case "mini_swe_agent_bash_args":
		return req.GetMiniSweAgentBashArgs()
	default:
		return req.GetShellArgs()
	}
}

func lineCount(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
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
			ev = s.recordUsage(ev)
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
		case *agentv1.InteractionUpdate_ToolCallStarted:
			s.handleToolLifecycle(u.ToolCallStarted.GetCallId(), u.ToolCallStarted.GetModelCallId(), u.ToolCallStarted.GetToolCall(), false)
		case *agentv1.InteractionUpdate_PartialToolCall:
			s.handleToolLifecycle(u.PartialToolCall.GetCallId(), u.PartialToolCall.GetModelCallId(), u.PartialToolCall.GetToolCall(), false)
		case *agentv1.InteractionUpdate_ToolCallCompleted:
			s.handleToolLifecycle(u.ToolCallCompleted.GetCallId(), u.ToolCallCompleted.GetModelCallId(), u.ToolCallCompleted.GetToolCall(), true)
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
			s.applyCheckpointToolBatches(state)
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
	messages := s.checkpointAssistantMessages()
	s.seenAssistantHistory = append([]checkpointAssistantMessage(nil), messages...)
	for _, msg := range messages {
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

// unseenCheckpointAssistantMessages compares the checkpoint as an ordered
// history. Cursor is allowed to rewrite content-addressed blob ids, so identity
// alone is insufficient: every matching historical position is marked seen
// under both its old and new ids, and only the appended suffix is returned.
func (s *Session) unseenCheckpointAssistantMessages() []checkpointAssistantMessage {
	messages := s.checkpointAssistantMessages()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenAssistantMsgIDs == nil {
		s.seenAssistantMsgIDs = map[string]struct{}{}
	}
	prefix := 0
	for prefix < len(messages) && prefix < len(s.seenAssistantHistory) {
		current := messages[prefix]
		seen := s.seenAssistantHistory[prefix]
		if current.text != seen.text || current.thinking != seen.thinking {
			break
		}
		s.seenAssistantMsgIDs[current.identity] = struct{}{}
		prefix++
	}
	if prefix == len(messages) {
		return nil
	}
	out := make([]checkpointAssistantMessage, 0, len(messages)-prefix)
	for index := prefix; index < len(messages); index++ {
		msg := messages[index]
		if _, seen := s.seenAssistantMsgIDs[msg.identity]; seen {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func (s *Session) markCheckpointAssistantMessages(messages []checkpointAssistantMessage) {
	if len(messages) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenAssistantMsgIDs == nil {
		s.seenAssistantMsgIDs = map[string]struct{}{}
	}
	for _, msg := range messages {
		s.seenAssistantMsgIDs[msg.identity] = struct{}{}
	}
	latest := s.checkpointAssistantMessagesLocked()
	if len(latest) > 0 {
		s.seenAssistantHistory = append([]checkpointAssistantMessage(nil), latest...)
	}
}

func (s *Session) checkpointAssistantMessagesLocked() []checkpointAssistantMessage {
	var state *agentv1.ConversationStateStructure
	if s.conversationState != nil {
		state = proto.Clone(s.conversationState).(*agentv1.ConversationStateStructure)
	}
	blobs := make(map[string][]byte, len(s.blobStore))
	for key, raw := range s.blobStore {
		blobs[key] = append([]byte(nil), raw...)
	}
	return snapshotAssistantMessages(checkpointSnapshot{state: state, blobs: blobs})
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
	case *agentv1.ConversationStep_ToolCall:
		// A completed inline Task is a subagent Cursor already executed
		// server-side. Its concluding text lives inside TaskToolCall.result's
		// nested conversation steps. Without this branch the agent visibly "turns
		// on" but its answer never reaches the client ("瀛愪唬鐞嗗紑涓嶈捣鏉?); surface
		// the final conclusion as assistant text, never a status word.
		if text := taskToolCallFinalText(message.ToolCall.GetTaskToolCall()); text != "" {
			return checkpointAssistantMessage{identity: identity + ":task", text: text}, true
		}
	}
	return checkpointAssistantMessage{}, false
}

// taskToolCallFinalText extracts the concluding text of a server-executed
// inline Task subagent. Cursor stores the result as an error string or, on
// success, as a sequence of nested conversation steps; the last non-empty
// assistant message is the subagent's final conclusion. Status shells and
// empty results yield "" so the caller keeps waiting for real content.
func taskToolCallFinalText(task *agentv1.TaskToolCall) string {
	if task == nil {
		return ""
	}
	result := task.GetResult()
	if result == nil {
		return ""
	}
	if success := result.GetSuccess(); success != nil {
		steps := success.GetConversationSteps()
		for i := len(steps) - 1; i >= 0; i-- {
			if text := steps[i].GetAssistantMessage().GetText(); strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		return ""
	}
	return strings.TrimSpace(result.GetError().GetError())
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
		s.holdTimer = time.AfterFunc(textHoldWindow, s.releaseTextStream)
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
	if s.seenAssistantMsgIDs == nil {
		s.seenAssistantMsgIDs = map[string]struct{}{}
	}
	s.mu.Unlock()

	var checkpointFinal string
	newMessages := s.unseenCheckpointAssistantMessages()
	for _, msg := range newMessages {
		checkpointFinal = reconcileAssistantText(checkpointFinal, msg.text)
	}

	s.mu.Lock()
	if s.textStreamLive || s.textFlushed {
		s.mu.Unlock()
		return
	}
	// Re-capture the live buffer now: text_deltas can keep arriving while the
	// checkpoint messages above were being read. Merging the snapshot we took
	// before that read into the buffers (the previous behaviour) would wipe
	// those in-flight deltas from the opening — source of intermittent lost
	// first characters. reconcileAssistantText never shrinks the live copy, so
	// the checkpoint can still restore a missing opening token on top of it.
	final := reconcileAssistantText(s.visibleText.String(), checkpointFinal)
	out := final
	if emitted != "" {
		switch {
		case strings.HasPrefix(final, emitted):
			out = final[len(emitted):]
		case strings.HasSuffix(final, emitted):
			// Same live text, possibly prefixed by a recovered opening token.
			out = final
		default:
			// Once text is live the downstream already owns the emitted prefix.
			// Replaying a diverging checkpoint would duplicate the answer.
			log.Debugf("cursor: release checkpoint text diverges from streamed text (streamed=%d checkpoint=%d); keeping streamed copy",
				len(emitted), len(final))
			out = ""
		}
	}
	s.textStreamLive = true
	s.holdTimer = nil
	s.emittedText.Reset()
	s.emittedText.WriteString(final)
	s.visibleText.Reset()
	s.visibleText.WriteString(final)
	s.mu.Unlock()
	s.markCheckpointAssistantMessages(newMessages)

	if out == "" {
		return
	}
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
	var newMessages []checkpointAssistantMessage
	haveCheckpoint := false
	newMessages = s.unseenCheckpointAssistantMessages()
	for _, msg := range newMessages {
		haveCheckpoint = true
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
	if out != "" {
		s.emittedText.Reset()
		s.emittedText.WriteString(final)
		s.visibleText.Reset()
		s.visibleText.WriteString(final)
	}
	s.textFlushed = true
	s.mu.Unlock()
	s.markCheckpointAssistantMessages(newMessages)

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
		// The streamed copy already owns the checkpoint text (the checkpoint is
		// lagging or partial). Never shrink the live copy back down to it.
		return streamed
	}
	// Total divergence. The client already owns the streamed bytes (or they are
	// about to be emitted), so a longer/rewritten checkpoint can never be
	// replayed without duplicating or relocating already-sent text. The live
	// copy wins, byte for byte.
	return streamed
}

func (s *Session) handleToolLifecycle(envelopeID, modelCallID string, toolCall *agentv1.ToolCall, completed bool) {
	if s == nil || toolCall == nil {
		return
	}
	variant, name, nestedID, serverOwned := lifecycleToolIdentity(toolCall)
	callID := firstNonEmpty(toolCall.GetToolCallId(), nestedID, envelopeID)
	if callID == "" {
		// An empty lifecycle identity cannot safely correlate an Exec, but it also
		// must not reject an otherwise usable turn.
		return
	}
	modelCallID = strings.TrimSpace(modelCallID)
	envelopeID = strings.TrimSpace(envelopeID)

	s.mu.Lock()
	if s.lifecyclesByID == nil {
		s.lifecyclesByID = map[string]*nativeLifecycle{}
	}
	lifecycle := s.findLifecycleLocked(callID, nestedID, envelopeID)
	if lifecycle == nil {
		lifecycle = &nativeLifecycle{callID: callID, envelopeID: envelopeID}
		s.lifecycleOrder = append(s.lifecycleOrder, lifecycle)
	}
	if name != "" {
		lifecycle.name = name
	}
	if variant != "" {
		lifecycle.variant = variant
	}
	if nestedID != "" {
		lifecycle.nestedID = nestedID
	}
	if modelCallID != "" {
		lifecycle.modelCallID = modelCallID
	}
	lifecycle.completed = lifecycle.completed || completed
	lifecycle.serverOwned = lifecycle.serverOwned || serverOwned
	s.indexLifecycleLocked(lifecycle)

	if lifecycle.serverOwned {
		if s.serverOwnedTodos == nil {
			s.serverOwnedTodos = map[string]*nativeLifecycle{}
		}
		s.serverOwnedTodos[callID] = lifecycle
		// Todo calls are executed by Cursor itself. They never become downstream
		// pending work and therefore cannot block a mixed provider batch.
		s.mu.Unlock()
		return
	}

	pending := s.findPendingForLifecycleLocked(lifecycle)
	if pending != nil {
		lifecycle.pending = pending
		pending.modelCallID = firstNonEmpty(lifecycle.modelCallID, pending.modelCallID)
		s.rebindPendingLocked(pending, lifecycle.callID)
		s.assignPendingBatchLocked(pending, pending.modelCallID, "")
	}
	s.mu.Unlock()
	s.syncManagerBindings()
}

func lifecycleToolIdentity(toolCall *agentv1.ToolCall) (variant, name, nestedID string, serverOwned bool) {
	if toolCall == nil {
		return "", "", "", false
	}
	switch value := toolCall.Tool.(type) {
	case *agentv1.ToolCall_ShellToolCall:
		return "shell_args", "Shell", nativeToolCallID(value.ShellToolCall.GetArgs()), false
	case *agentv1.ToolCall_DeleteToolCall:
		return "delete_args", "Delete", nativeToolCallID(value.DeleteToolCall.GetArgs()), false
	case *agentv1.ToolCall_GlobToolCall:
		return "glob_tool_call", "Glob", "", false
	case *agentv1.ToolCall_GrepToolCall:
		return "grep_args", "Grep", nativeToolCallID(value.GrepToolCall.GetArgs()), false
	case *agentv1.ToolCall_ReadToolCall:
		return "read_tool_call", "Read", "", false
	case *agentv1.ToolCall_UpdateTodosToolCall:
		return "update_todos_tool_call", "TodoWrite", "", true
	case *agentv1.ToolCall_ReadTodosToolCall:
		return "read_todos_tool_call", "ReadTodos", "", true
	case *agentv1.ToolCall_EditToolCall:
		return "edit_tool_call", "Edit", "", false
	case *agentv1.ToolCall_LsToolCall:
		return "ls_args", "Ls", nativeToolCallID(value.LsToolCall.GetArgs()), false
	case *agentv1.ToolCall_FetchToolCall:
		return "fetch_args", "WebFetch", nativeToolCallID(value.FetchToolCall.GetArgs()), false
	case *agentv1.ToolCall_McpToolCall:
		args := value.McpToolCall.GetArgs()
		return "mcp_args", firstNonEmpty(args.GetToolName(), args.GetName()), args.GetToolCallId(), false
	case *agentv1.ToolCall_TaskToolCall:
		return "task_tool_call", "Task", "", false
	case *agentv1.ToolCall_PiReadToolCall:
		return "pi_read_args", "Read", nativeToolCallID(value.PiReadToolCall.GetArgs()), false
	case *agentv1.ToolCall_PiBashToolCall:
		return "pi_bash_args", "Bash", nativeToolCallID(value.PiBashToolCall.GetArgs()), false
	case *agentv1.ToolCall_PiEditToolCall:
		return "pi_edit_args", "Edit", nativeToolCallID(value.PiEditToolCall.GetArgs()), false
	case *agentv1.ToolCall_PiWriteToolCall:
		return "pi_write_args", "Write", nativeToolCallID(value.PiWriteToolCall.GetArgs()), false
	case *agentv1.ToolCall_PiGrepToolCall:
		return "pi_grep_args", "Grep", nativeToolCallID(value.PiGrepToolCall.GetArgs()), false
	case *agentv1.ToolCall_PiFindToolCall:
		return "pi_find_args", "Glob", nativeToolCallID(value.PiFindToolCall.GetArgs()), false
	case *agentv1.ToolCall_PiLsToolCall:
		return "pi_ls_args", "Ls", nativeToolCallID(value.PiLsToolCall.GetArgs()), false
	default:
		return "", "", "", false
	}
}

func (s *Session) findLifecycleLocked(ids ...string) *nativeLifecycle {
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			if lifecycle := s.lifecyclesByID[id]; lifecycle != nil {
				return lifecycle
			}
		}
	}
	return nil
}

func (s *Session) indexLifecycleLocked(lifecycle *nativeLifecycle) {
	if lifecycle == nil {
		return
	}
	for _, id := range []string{lifecycle.callID, lifecycle.nestedID, lifecycle.envelopeID} {
		if id = strings.TrimSpace(id); id != "" {
			s.lifecyclesByID[id] = lifecycle
		}
	}
}

func (s *Session) findPendingForLifecycleLocked(lifecycle *nativeLifecycle) *pendingExec {
	if lifecycle == nil {
		return nil
	}
	for _, id := range []string{lifecycle.callID, lifecycle.nestedID, lifecycle.envelopeID} {
		if id != "" {
			if pending := s.pending[id]; pending != nil {
				return pending
			}
		}
	}
	// Only fall back to variant/name order when Cursor omitted every common id.
	for _, pending := range s.pendingOrder {
		if pending == nil || pending.emitted || pending.modelCallID != "" && lifecycle.modelCallID != "" && pending.modelCallID != lifecycle.modelCallID {
			continue
		}
		if pending.variant == lifecycle.variant || s.pendingToolNameMatches(pending, lifecycle.name) {
			if pending.provisional || nativeToolCallIDFromRequest(pending.request, pending.variant) == "" {
				return pending
			}
		}
	}
	return nil
}

func nativeToolCallIDFromRequest(req *agentv1.ExecServerMessage, variant string) string {
	if req == nil {
		return ""
	}
	switch variant {
	case "mcp_args":
		return req.GetMcpArgs().GetToolCallId()
	case "shell_args":
		return nativeToolCallID(req.GetShellArgs())
	case "shell_stream_args":
		return nativeToolCallID(req.GetShellStreamArgs())
	case "write_args":
		return nativeToolCallID(req.GetWriteArgs())
	case "delete_args":
		return nativeToolCallID(req.GetDeleteArgs())
	case "grep_args":
		return nativeToolCallID(req.GetGrepArgs())
	case "read_args":
		return nativeToolCallID(req.GetReadArgs())
	case "ls_args":
		return nativeToolCallID(req.GetLsArgs())
	case "diagnostics_args":
		return nativeToolCallID(req.GetDiagnosticsArgs())
	case "fetch_args":
		return nativeToolCallID(req.GetFetchArgs())
	case "subagent_args":
		return nativeToolCallID(req.GetSubagentArgs())
	case "mini_swe_agent_bash_args":
		return nativeToolCallID(req.GetMiniSweAgentBashArgs())
	}
	return ""
}

func (s *Session) rebindPendingLocked(item *pendingExec, authoritativeID string) {
	authoritativeID = strings.TrimSpace(authoritativeID)
	if item == nil || authoritativeID == "" {
		return
	}
	items := []*pendingExec{item}
	if item.group != nil && len(item.group.items) > 0 {
		items = item.group.items
	}
	if s.pending == nil {
		s.pending = map[string]*pendingExec{}
	}
	for _, member := range items {
		for id, pending := range s.pending {
			if pending == member {
				delete(s.pending, id)
			}
		}
	}
	for index, member := range items {
		id := authoritativeID
		if index > 0 {
			id = fmt.Sprintf("%s_%d", authoritativeID, index+1)
		}
		member.call.ID = id
		member.provisional = false
		s.pending[id] = member
	}
}

func (s *Session) syncManagerBindings() {
	if s == nil || s.manager == nil {
		return
	}
	s.manager.RebindSessionPending(s, s.pendingIDs())
}

func (s *Session) pendingIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		if strings.TrimSpace(id) != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Session) ensureBatchLocked(batchID, modelCallID string) *pendingToolBatch {
	batchID = strings.TrimSpace(batchID)
	modelCallID = strings.TrimSpace(modelCallID)
	if batchID == "" {
		batchID = modelCallID
	}
	if batchID == "" {
		return nil
	}
	if s.batches == nil {
		s.batches = map[string]*pendingToolBatch{}
	}
	if batch := s.batches[batchID]; batch != nil {
		if batch.modelCallID == "" {
			batch.modelCallID = modelCallID
		}
		return batch
	}
	batch := &pendingToolBatch{id: batchID, modelCallID: modelCallID}
	s.batches[batchID] = batch
	s.batchOrder = append(s.batchOrder, batch)
	return batch
}

func (s *Session) assignPendingBatchLocked(item *pendingExec, modelCallID, fallbackID string) *pendingToolBatch {
	if item == nil {
		return nil
	}
	modelCallID = strings.TrimSpace(modelCallID)
	if modelCallID == "" {
		modelCallID = strings.TrimSpace(item.modelCallID)
	}
	batchID := modelCallID
	if batchID == "" {
		batchID = strings.TrimSpace(fallbackID)
	}
	if batchID == "" {
		return item.batch
	}
	target := s.ensureBatchLocked(batchID, modelCallID)
	if target == nil {
		return nil
	}
	if item.batch != nil && item.batch != target {
		removePendingFromBatchLocked(item.batch, item)
	}
	item.batch = target
	item.modelCallID = modelCallID
	if !containsPending(target.ordered, item) {
		target.ordered = append(target.ordered, item)
	}
	return target
}

func removePendingFromBatchLocked(batch *pendingToolBatch, item *pendingExec) {
	if batch == nil || item == nil {
		return
	}
	for index, candidate := range batch.ordered {
		if candidate == item {
			batch.ordered = append(batch.ordered[:index], batch.ordered[index+1:]...)
			return
		}
	}
}

func containsPending(items []*pendingExec, target *pendingExec) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func (s *Session) applyCheckpointToolBatches(state *agentv1.ConversationStateStructure) {
	groups, present := checkpointToolCallGroups(state)
	if !present {
		return
	}
	s.mu.Lock()
	for groupIndex, calls := range groups {
		if len(calls) == 0 {
			continue
		}
		matched := make([]*pendingExec, 0, len(calls))
		used := map[*pendingExec]struct{}{}
		modelCallID := ""
		for _, checkpoint := range calls {
			item := s.pending[checkpoint.id]
			if item != nil && !s.pendingToolNameMatches(item, checkpoint.name) {
				item = nil
			}
			if item == nil {
				if lifecycle := s.findLifecycleLocked(checkpoint.id); lifecycle != nil {
					item = lifecycle.pending
					if modelCallID == "" {
						modelCallID = lifecycle.modelCallID
					}
				}
			}
			if item == nil {
				for _, candidate := range s.pendingOrder {
					if candidate == nil {
						continue
					}
					if _, exists := used[candidate]; exists {
						continue
					}
					if s.pendingToolNameMatches(candidate, checkpoint.name) {
						item = candidate
						break
					}
				}
			}
			if item == nil {
				continue
			}
			members := []*pendingExec{item}
			if item.group != nil && len(item.group.items) > 0 {
				members = item.group.items
			}
			s.rebindPendingLocked(item, checkpoint.id)
			for _, member := range members {
				if _, duplicate := used[member]; duplicate {
					continue
				}
				used[member] = struct{}{}
				matched = append(matched, member)
				if modelCallID == "" {
					modelCallID = member.modelCallID
				}
			}
		}
		batchID := modelCallID
		if batchID == "" {
			batchID = fmt.Sprintf("checkpoint:%s:%d", checkpointGroupDigest(calls), groupIndex)
		}
		batch := s.ensureBatchLocked(batchID, modelCallID)
		batch.checkpoint = true
		batch.checkpointCalls = append([]checkpointToolCall(nil), calls...)
		if len(matched) == 0 {
			continue
		}
		batch.ordered = append(batch.ordered[:0], matched...)
		for _, member := range matched {
			if member.batch != nil && member.batch != batch {
				removePendingFromBatchLocked(member.batch, member)
			}
			member.batch = batch
			member.modelCallID = modelCallID
		}
	}
	s.mu.Unlock()
	s.syncManagerBindings()
}

func checkpointGroupDigest(calls []checkpointToolCall) string {
	var builder strings.Builder
	for _, call := range calls {
		builder.WriteString(call.id)
		builder.WriteByte(0)
		builder.WriteString(call.name)
		builder.WriteByte(0)
	}
	return fmt.Sprintf("%x", uuid.NewSHA1(uuid.NameSpaceOID, []byte(builder.String())))[:24]
}

func (s *Session) handleExec(req *agentv1.ExecServerMessage) (bool, error) {
	switch m := req.Message.(type) {
	case nil:
		if len(unknownExecFieldNumbers(req)) > 0 {
			return false, s.rejectUnsupportedExec(s.stream, req)
		}
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
	case *agentv1.ExecServerMessage_ShellArgs:
		return s.handleNativeArgs(req, "shell_args", m.ShellArgs)
	case *agentv1.ExecServerMessage_ShellStreamArgs:
		return s.handleNativeArgs(req, "shell_stream_args", m.ShellStreamArgs)
	case *agentv1.ExecServerMessage_WriteArgs:
		return s.handleNativeArgs(req, "write_args", m.WriteArgs)
	case *agentv1.ExecServerMessage_DeleteArgs:
		return s.handleNativeArgs(req, "delete_args", m.DeleteArgs)
	case *agentv1.ExecServerMessage_GrepArgs:
		return s.handleNativeArgs(req, "grep_args", m.GrepArgs)
	case *agentv1.ExecServerMessage_ReadArgs:
		return s.handleNativeArgs(req, "read_args", m.ReadArgs)
	case *agentv1.ExecServerMessage_LsArgs:
		return s.handleNativeArgs(req, "ls_args", m.LsArgs)
	case *agentv1.ExecServerMessage_DiagnosticsArgs:
		return s.handleNativeArgs(req, "diagnostics_args", m.DiagnosticsArgs)
	case *agentv1.ExecServerMessage_FetchArgs:
		return s.handleNativeArgs(req, "fetch_args", m.FetchArgs)
	case *agentv1.ExecServerMessage_PiReadArgs:
		return s.handleNativeArgs(req, "pi_read_args", m.PiReadArgs)
	case *agentv1.ExecServerMessage_PiBashArgs:
		return s.handleNativeArgs(req, "pi_bash_args", m.PiBashArgs)
	case *agentv1.ExecServerMessage_PiEditArgs:
		return s.handleNativeArgs(req, "pi_edit_args", m.PiEditArgs)
	case *agentv1.ExecServerMessage_PiWriteArgs:
		return s.handleNativeArgs(req, "pi_write_args", m.PiWriteArgs)
	case *agentv1.ExecServerMessage_PiGrepArgs:
		return s.handleNativeArgs(req, "pi_grep_args", m.PiGrepArgs)
	case *agentv1.ExecServerMessage_PiFindArgs:
		return s.handleNativeArgs(req, "pi_find_args", m.PiFindArgs)
	case *agentv1.ExecServerMessage_PiLsArgs:
		return s.handleNativeArgs(req, "pi_ls_args", m.PiLsArgs)
	case *agentv1.ExecServerMessage_MiniSweAgentBashArgs:
		return s.handleNativeArgs(req, "mini_swe_agent_bash_args", m.MiniSweAgentBashArgs)
	case *agentv1.ExecServerMessage_SubagentArgs:
		return s.handleNativeArgs(req, "subagent_args", m.SubagentArgs)
	}
	return false, s.rejectUnsupportedExec(s.stream, req)
}

var nativeExecToolNames = map[string][]string{
	"shell_args":               {"Bash", "Shell"},
	"shell_stream_args":        {"Bash", "Shell"},
	"write_args":               {"Write", "apply_patch"},
	"delete_args":              {"Delete", "apply_patch"},
	"grep_args":                {"Grep"},
	"read_args":                {"Read"},
	"ls_args":                  {"Ls", "Glob", "Read"},
	"diagnostics_args":         {"Lsp", "Diagnostics"},
	"fetch_args":               {"WebFetch", "Fetch"},
	"pi_read_args":             {"Read"},
	"pi_bash_args":             {"Bash", "Shell"},
	"pi_edit_args":             {"Edit", "MultiEdit", "apply_patch"},
	"pi_write_args":            {"Write", "apply_patch"},
	"pi_grep_args":             {"Grep"},
	"pi_find_args":             {"Glob", "Find"},
	"pi_ls_args":               {"Ls", "Glob", "Read"},
	"mini_swe_agent_bash_args": {"Bash", "Shell"},
	"subagent_args":            {"Task", "Agent", "Subagent"},
}

func (s *Session) handleNativeArgs(req *agentv1.ExecServerMessage, variant string, args proto.Message) (bool, error) {
	def := s.lookupNativeTool(variant)
	if def == nil {
		toolName := variant
		if names := nativeExecToolNames[variant]; len(names) > 0 {
			toolName = names[0]
		}
		return false, s.sendNativeResult(req, variant, ToolResult{
			Name:    toolName,
			Content: fmt.Sprintf("%s is not available in this client request", toolName),
			IsError: true,
		})
	}
	arguments, err := nativeToolArgumentBags(variant, args, def)
	if err != nil {
		return false, err
	}
	copyReq := proto.Clone(req).(*agentv1.ExecServerMessage)
	if variant == "shell_stream_args" {
		if err := s.sendShellStreamEvent(req, shellStreamStart{}); err != nil {
			return false, err
		}
	}
	authoritativeID, modelCallID := s.resolveNativeLifecycle(args, variant, def.Name)
	callID := authoritativeID
	provisional := false
	if callID == "" {
		callID = uuid.NewString()
		provisional = true
	}
	group := &nativeExecGroup{request: copyReq, variant: variant, results: map[int]ToolResult{}}
	pendingItems := make([]*pendingExec, 0, len(arguments))
	for index, argumentBag := range arguments {
		id := callID
		if index > 0 {
			id = fmt.Sprintf("%s_%d", callID, index+1)
		}
		kind := firstNonEmpty(def.Kind, "function")
		call := ToolCall{ID: id, Name: def.Name, Arguments: argumentBag, Kind: kind, Namespace: def.Namespace}
		pendingItems = append(pendingItems, &pendingExec{request: copyReq, call: call, variant: variant, group: group, index: index, modelCallID: modelCallID, provisional: provisional})
	}
	group.items = pendingItems
	for _, pending := range pendingItems {
		s.bindPendingExec(pending, pending.call.ID, authoritativeID)
	}
	return false, nil
}

func (s *Session) bindPendingExec(pending *pendingExec, ids ...string) {
	if pending == nil {
		return
	}
	s.mu.Lock()
	for _, id := range ids {
		if id != "" {
			s.pending[id] = pending
		}
	}
	s.pendingOrder = append(s.pendingOrder, pending)
	fallbackBatchID := ""
	if pending.request != nil {
		fallbackBatchID = fmt.Sprintf("exec:%s:%d", pending.request.GetExecId(), pending.request.GetId())
	}
	s.assignPendingBatchLocked(pending, pending.modelCallID, fallbackBatchID)
	if lifecycle := s.findLifecycleLocked(ids...); lifecycle != nil {
		lifecycle.pending = pending
		if pending.modelCallID == "" {
			pending.modelCallID = lifecycle.modelCallID
			s.assignPendingBatchLocked(pending, pending.modelCallID, fallbackBatchID)
		}
	}
	s.attachPendingToCheckpointLocked(pending)
	s.mu.Unlock()
	if s.manager != nil {
		for _, id := range ids {
			if id != "" {
				s.manager.BindPending(id, s)
			}
		}
	}
}

func (s *Session) attachPendingToCheckpointLocked(pending *pendingExec) {
	if pending == nil {
		return
	}
	for _, batch := range s.batchOrder {
		if batch == nil || !batch.checkpoint || len(batch.checkpointCalls) == 0 {
			continue
		}
		for _, checkpoint := range batch.checkpointCalls {
			if !s.pendingToolNameMatches(pending, checkpoint.name) {
				continue
			}
			if existing := s.pending[checkpoint.id]; existing != nil && existing != pending {
				continue
			}
			s.rebindPendingLocked(pending, checkpoint.id)
			if pending.batch != nil && pending.batch != batch {
				removePendingFromBatchLocked(pending.batch, pending)
			}
			pending.batch = batch
			pending.modelCallID = batch.modelCallID
			// Rebuild checkpoint order from every currently bound member. Missing
			// later Exec frames keep the batch incomplete instead of rejecting it.
			ordered := make([]*pendingExec, 0, len(batch.checkpointCalls))
			seen := map[*pendingExec]struct{}{}
			for _, call := range batch.checkpointCalls {
				item := s.pending[call.id]
				if item == nil {
					continue
				}
				members := []*pendingExec{item}
				if item.group != nil && len(item.group.items) > 0 {
					members = item.group.items
				}
				for _, member := range members {
					if _, duplicate := seen[member]; duplicate {
						continue
					}
					seen[member] = struct{}{}
					member.batch = batch
					member.modelCallID = batch.modelCallID
					ordered = append(ordered, member)
				}
			}
			batch.ordered = ordered
			return
		}
	}
}

func (s *Session) resolveNativeLifecycle(args proto.Message, variant, toolName string) (callID, modelCallID string) {
	if id := strings.TrimSpace(nativeToolCallID(args)); id != "" {
		callID = id
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if lifecycle := s.findLifecycleLocked(callID); lifecycle != nil {
		return firstNonEmpty(lifecycle.callID, callID), lifecycle.modelCallID
	}
	for _, lifecycle := range s.lifecycleOrder {
		if lifecycle == nil || lifecycle.serverOwned || lifecycle.pending != nil {
			continue
		}
		if lifecycle.variant == variant || s.toolCallNameMatches(toolName, lifecycle.name) {
			return firstNonEmpty(lifecycle.callID, callID), lifecycle.modelCallID
		}
	}
	return callID, ""
}

func (s *Session) resolveNativeToolCallID(args proto.Message, toolName string) string {
	if id := strings.TrimSpace(nativeToolCallID(args)); id != "" {
		return id
	}
	_, modelCallID := s.resolveNativeLifecycle(args, "", toolName)
	if modelCallID != "" {
		s.mu.Lock()
		for _, lifecycle := range s.lifecycleOrder {
			if lifecycle != nil && lifecycle.modelCallID == modelCallID && s.toolCallNameMatches(toolName, lifecycle.name) {
				id := lifecycle.callID
				s.mu.Unlock()
				return id
			}
		}
		s.mu.Unlock()
	}
	checkpointCalls, ok := checkpointToolCalls(s.conversationState)
	if ok {
		for _, checkpoint := range checkpointCalls {
			if s.toolCallNameMatches(toolName, checkpoint.name) {
				if _, occupied := s.pending[checkpoint.id]; !occupied {
					return checkpoint.id
				}
			}
		}
	}
	// Legacy native Exec variants genuinely omit the id. Keep the generated id
	// stable for this pending execution and later reconcile it to the checkpoint
	// before exposing the formal batch.
	return uuid.NewString()
}

func (s *Session) lookupNativeTool(variant string) *ToolDefinition {
	for _, name := range nativeExecToolNames[variant] {
		if def := s.lookupTool(name); def != nil && hasNativeCursorToolMapping(*def) {
			return def
		}
	}
	return nil
}

func nativeToolCallID(args proto.Message) string {
	if args == nil {
		return ""
	}
	field := args.ProtoReflect().Descriptor().Fields().ByName("tool_call_id")
	if field == nil || field.Kind() != protoreflect.StringKind {
		return ""
	}
	return args.ProtoReflect().Get(field).String()
}

func nativeToolArgumentBags(variant string, raw proto.Message, def *ToolDefinition) ([]map[string]any, error) {
	value, err := nativeToolArguments(variant, raw, def)
	if err != nil {
		return nil, err
	}
	if variant != "pi_edit_args" || strings.EqualFold(def.Kind, "apply_patch") {
		return []map[string]any{value}, nil
	}
	args := raw.(*agentv1.PiEditExecArgs)
	properties, _ := def.Parameters["properties"].(map[string]any)
	if _, supportsBatch := properties["edits"]; supportsBatch {
		edits := make([]any, 0, len(args.GetEdits()))
		for _, edit := range args.GetEdits() {
			edits = append(edits, map[string]any{"old_string": edit.GetOldText(), "new_string": edit.GetNewText()})
		}
		return []map[string]any{{nativeArgumentName(def, "file_path", "path"): args.GetPath(), "edits": edits}}, nil
	}
	if len(args.GetEdits()) <= 1 {
		return []map[string]any{value}, nil
	}
	out := make([]map[string]any, 0, len(args.GetEdits()))
	for _, edit := range args.GetEdits() {
		out = append(out, map[string]any{
			nativeArgumentName(def, "file_path", "path"):      args.GetPath(),
			nativeArgumentName(def, "old_string", "old_text"): edit.GetOldText(),
			nativeArgumentName(def, "new_string", "new_text"): edit.GetNewText(),
		})
	}
	return out, nil
}

func nativeToolArguments(variant string, raw proto.Message, def *ToolDefinition) (map[string]any, error) {
	pathKey := nativeArgumentName(def, "file_path", "path")
	switch variant {
	case "shell_args", "shell_stream_args", "mini_swe_agent_bash_args":
		args := raw.(*agentv1.ShellArgs)
		if strings.EqualFold(def.Kind, "shell") {
			out := map[string]any{"commands": []string{args.GetCommand()}}
			if args.GetTimeout() > 0 {
				out["timeout_ms"] = args.GetTimeout()
			}
			return out, nil
		}
		if strings.EqualFold(def.Kind, "local_shell") {
			out := map[string]any{"type": "exec", "command": localShellCommand(args.GetCommand()), "env": map[string]string{}}
			if args.GetWorkingDirectory() != "" {
				out["working_directory"] = args.GetWorkingDirectory()
			}
			if args.GetTimeout() > 0 {
				out["timeout_ms"] = args.GetTimeout()
			}
			return out, nil
		}
		out := map[string]any{"command": args.GetCommand()}
		if args.GetWorkingDirectory() != "" {
			out[nativeArgumentName(def, "cwd", "working_directory")] = args.GetWorkingDirectory()
		}
		if args.GetTimeout() > 0 {
			out["timeout"] = args.GetTimeout()
		}
		return out, nil
	case "read_args":
		args := raw.(*agentv1.ReadArgs)
		out := map[string]any{pathKey: args.GetPath()}
		if args.Offset != nil {
			out["offset"] = args.GetOffset()
		}
		if args.Limit != nil {
			out["limit"] = args.GetLimit()
		}
		return out, nil
	case "write_args":
		args := raw.(*agentv1.WriteArgs)
		content := args.GetFileText()
		if content == "" && len(args.GetFileBytes()) > 0 {
			content = string(args.GetFileBytes())
		}
		if strings.EqualFold(def.Kind, "apply_patch") {
			return applyPatchArguments("create_file", args.GetPath(), content), nil
		}
		return map[string]any{pathKey: args.GetPath(), nativeArgumentName(def, "content", "file_text"): content}, nil
	case "delete_args":
		args := raw.(*agentv1.DeleteArgs)
		if strings.EqualFold(def.Kind, "apply_patch") {
			return applyPatchArguments("delete_file", args.GetPath(), ""), nil
		}
		return map[string]any{pathKey: args.GetPath()}, nil
	case "grep_args":
		args := raw.(*agentv1.GrepArgs)
		out := map[string]any{"pattern": args.GetPattern()}
		putNativeOptional(out, "path", args.Path)
		putNativeOptional(out, "glob", args.Glob)
		putNativeOptional(out, "output_mode", args.OutputMode)
		putNativeOptional(out, "head_limit", args.HeadLimit)
		putNativeOptional(out, "offset", args.Offset)
		if args.CaseInsensitive != nil {
			out[nativeArgumentName(def, "-i", "case_insensitive")] = args.GetCaseInsensitive()
		}
		return out, nil
	case "ls_args":
		args := raw.(*agentv1.LsArgs)
		path := args.GetPath()
		if path == "" {
			path = "."
		}
		if strings.EqualFold(def.Name, "Glob") {
			return map[string]any{"pattern": "**/*", "path": path}, nil
		}
		return map[string]any{nativeArgumentName(def, "path", "file_path"): path}, nil
	case "diagnostics_args":
		args := raw.(*agentv1.DiagnosticsArgs)
		out := map[string]any{nativeArgumentName(def, "file", "file_path", "path"): args.GetPath()}
		if nativeToolHasProperty(def, "action") {
			out["action"] = "diagnostics"
		} else if nativeToolHasProperty(def, "operation") {
			out["operation"] = "diagnostics"
		}
		return out, nil
	case "fetch_args":
		args := raw.(*agentv1.FetchArgs)
		out := map[string]any{"url": args.GetUrl()}
		if nativeToolHasProperty(def, "prompt") {
			out["prompt"] = "Return the response content without summarizing it."
		}
		return out, nil
	case "pi_read_args":
		args := raw.(*agentv1.PiReadExecArgs)
		out := map[string]any{pathKey: args.GetPath()}
		putNativeOptional(out, "offset", args.Offset)
		putNativeOptional(out, "limit", args.Limit)
		return out, nil
	case "pi_bash_args":
		args := raw.(*agentv1.PiBashExecArgs)
		out := map[string]any{"command": args.GetCommand()}
		putNativeOptional(out, "timeout", args.Timeout)
		return out, nil
	case "pi_edit_args":
		args := raw.(*agentv1.PiEditExecArgs)
		if strings.EqualFold(def.Kind, "apply_patch") {
			return applyPatchArguments("update_file", args.GetPath(), piEditUnifiedDiff(args)), nil
		}
		if len(args.GetEdits()) == 0 {
			return map[string]any{pathKey: args.GetPath()}, nil
		}
		edit := args.GetEdits()[0]
		return map[string]any{pathKey: args.GetPath(), nativeArgumentName(def, "old_string", "old_text"): edit.GetOldText(), nativeArgumentName(def, "new_string", "new_text"): edit.GetNewText()}, nil
	case "pi_write_args":
		args := raw.(*agentv1.PiWriteExecArgs)
		if strings.EqualFold(def.Kind, "apply_patch") {
			return applyPatchArguments("create_file", args.GetPath(), args.GetContent()), nil
		}
		return map[string]any{pathKey: args.GetPath(), "content": args.GetContent()}, nil
	case "pi_grep_args":
		args := raw.(*agentv1.PiGrepExecArgs)
		out := map[string]any{"pattern": args.GetPattern()}
		putNativeOptional(out, "path", args.Path)
		putNativeOptional(out, "glob", args.Glob)
		putNativeOptional(out, nativeArgumentName(def, "-i", "ignore_case"), args.IgnoreCase)
		putNativeOptional(out, nativeArgumentName(def, "-C", "context"), args.Context)
		putNativeOptional(out, "head_limit", args.Limit)
		return out, nil
	case "pi_find_args":
		args := raw.(*agentv1.PiFindExecArgs)
		out := map[string]any{"pattern": args.GetPattern()}
		putNativeOptional(out, "path", args.Path)
		putNativeOptional(out, "head_limit", args.Limit)
		return out, nil
	case "pi_ls_args":
		args := raw.(*agentv1.PiLsExecArgs)
		path := args.GetPath()
		if path == "" {
			path = "."
		}
		if strings.EqualFold(def.Name, "Glob") {
			return map[string]any{"pattern": "**/*", "path": path}, nil
		}
		return map[string]any{nativeArgumentName(def, "path", "file_path"): path}, nil
	case "subagent_args":
		args := raw.(*agentv1.SubagentArgs)
		out := map[string]any{"prompt": args.GetPrompt()}
		if args.GetSubagentType() != "" {
			out[nativeArgumentName(def, "subagent_type", "subagent_type_name")] = args.GetSubagentType()
		}
		if args.GetModelId() != "" {
			out["model"] = args.GetModelId()
		}
		if args.GetResumeAgentId() != "" {
			out["resume"] = args.GetResumeAgentId()
		}
		if args.RunInBackground != nil {
			out["run_in_background"] = args.GetRunInBackground()
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cursor: unsupported native exec mapping %s", variant)
	}
}

func localShellCommand(command string) []string {
	// Cursor supplies a shell command line; Responses local_shell requires argv.
	// Use the desktop platform contract of this Go deployment rather than
	// pretending the entire command is an executable name.
	if strings.EqualFold(runtime.GOOS, "windows") {
		return []string{"powershell", "-NoProfile", "-Command", command}
	}
	return []string{"/bin/sh", "-lc", command}
}

func applyPatchArguments(operationType, path, diff string) map[string]any {
	operation := map[string]any{"type": operationType, "path": path}
	if operationType != "delete_file" {
		operation["diff"] = diff
	}
	return map[string]any{"operation": operation}
}

func piEditUnifiedDiff(args *agentv1.PiEditExecArgs) string {
	if args == nil {
		return ""
	}
	var builder strings.Builder
	path := strings.ReplaceAll(args.GetPath(), "\\", "/")
	builder.WriteString("--- a/")
	builder.WriteString(path)
	builder.WriteString("\n+++ b/")
	builder.WriteString(path)
	builder.WriteByte('\n')
	for _, edit := range args.GetEdits() {
		oldLines := splitDiffLines(edit.GetOldText())
		newLines := splitDiffLines(edit.GetNewText())
		builder.WriteString(fmt.Sprintf("@@ -1,%d +1,%d @@\n", len(oldLines), len(newLines)))
		for _, line := range oldLines {
			builder.WriteByte('-')
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
		for _, line := range newLines {
			builder.WriteByte('+')
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func splitDiffLines(value string) []string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSuffix(value, "\n")
	if value == "" {
		return nil
	}
	return strings.Split(value, "\n")
}

func nativeArgumentName(def *ToolDefinition, candidates ...string) string {
	if def != nil {
		if properties, ok := def.Parameters["properties"].(map[string]any); ok {
			for _, candidate := range candidates {
				if _, ok := properties[candidate]; ok {
					return candidate
				}
			}
		}
	}
	return candidates[0]
}

func nativeToolHasProperty(def *ToolDefinition, name string) bool {
	if def == nil || name == "" {
		return false
	}
	properties, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = properties[name]
	return ok
}

func putNativeOptional[T comparable](out map[string]any, key string, value *T) {
	if value != nil {
		out[key] = *value
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
	def := s.lookupMcpTool(toolName, provider)
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
	nativeID, modelCallID := s.resolveNativeLifecycle(args, "mcp_args", def.Name)
	provisional := false
	if nativeID == "" {
		nativeID = uuid.NewString()
		provisional = true
	}
	call := ToolCall{
		ID:        nativeID,
		Name:      def.Name,
		Arguments: decodeMcpArguments(args, def),
		Kind:      firstNonEmpty(def.Kind, "function"),
		Namespace: def.Namespace,
	}
	copyReq := proto.Clone(req).(*agentv1.ExecServerMessage)
	pending := &pendingExec{request: copyReq, call: call, variant: "mcp_args", modelCallID: modelCallID, provisional: provisional}
	s.bindPendingExec(pending, nativeID)
	// Exec frames can arrive before Cursor's checkpoint proves the complete
	// provider tool batch. Keep reading until that boundary is available.
	return false, nil
}

type checkpointToolCall struct {
	id   string
	name string
}

func (s *Session) emitPendingToolBatch(force bool) bool {
	pending, batch, ready := s.orderedPendingToolBatchWithOwner(force)
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
	if batch != nil {
		s.mu.Lock()
		batch.emitted = true
		s.mu.Unlock()
	}
	return len(pending) > 0
}

func (s *Session) nextPendingBatchReady() bool {
	_, _, ready := s.orderedPendingToolBatchWithOwner(true)
	return ready
}

func (s *Session) emitNextPendingTool() bool {
	s.mu.Lock()
	var next *pendingExec
	for _, batch := range s.batchOrder {
		if batch == nil {
			continue
		}
		// Serial continuation only advances within a batch already made public.
		// A later provider batch is exposed by emitPendingToolBatch instead.
		if !batch.emitted {
			continue
		}
		for _, item := range batch.ordered {
			if item != nil && !item.emitted {
				item.emitted = true
				next = item
				break
			}
		}
		if next != nil {
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
	pending, _, ready := s.orderedPendingToolBatchWithOwner(force)
	return pending, ready
}

func (s *Session) orderedPendingToolBatchWithOwner(force bool) ([]*pendingExec, *pendingToolBatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, batch := range s.batchOrder {
		if batch == nil || batch.emitted {
			continue
		}
		pending := make([]*pendingExec, 0, len(batch.ordered))
		for _, item := range batch.ordered {
			if item != nil && !item.emitted {
				pending = append(pending, item)
			}
		}
		if len(pending) == 0 {
			continue
		}
		// A checkpoint is the strongest boundary. model_call_id is sufficient
		// once the transport terminates; an Exec-only batch is emitted alone on
		// force and can never absorb calls from another Exec/model call.
		if batch.checkpoint {
			expected := 0
			for _, checkpoint := range batch.checkpointCalls {
				if lifecycle := s.findLifecycleLocked(checkpoint.id); lifecycle != nil && lifecycle.serverOwned {
					continue
				}
				expected++
			}
			if expected > 0 && len(pending) < expected {
				continue
			}
			return pending, batch, true
		}
		if force && (batch.modelCallID != "" || batch.terminal || strings.HasPrefix(batch.id, "exec:")) {
			return pending, batch, true
		}
	}
	return nil, nil, false
}

func (s *Session) markPendingBatchesTerminal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	for _, batch := range s.batchOrder {
		if batch == nil {
			continue
		}
		for _, item := range batch.ordered {
			if item != nil && !item.emitted {
				batch.terminal = true
				break
			}
		}
	}
	s.mu.Unlock()
}

func (s *Session) rebindPendingGroup(items []*pendingExec, checkpointID string) {
	checkpointID = strings.TrimSpace(checkpointID)
	if checkpointID == "" || len(items) == 0 {
		return
	}
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]*pendingExec{}
	}
	oldIDs := make([]string, 0, len(items)*2)
	newIDs := make([]string, 0, len(items))
	for index, item := range items {
		for id, pending := range s.pending {
			if pending == item {
				oldIDs = append(oldIDs, id)
				delete(s.pending, id)
			}
		}
		id := checkpointID
		if index > 0 {
			id = fmt.Sprintf("%s_%d", checkpointID, index+1)
		}
		item.call.ID = id
		s.pending[id] = item
		newIDs = append(newIDs, id)
	}
	s.mu.Unlock()
	if s.manager != nil {
		for _, id := range oldIDs {
			s.manager.UnbindPending(id)
		}
		for _, id := range newIDs {
			s.manager.BindPending(id, s)
		}
	}
}

func (s *Session) pendingToolNameMatches(item *pendingExec, checkpointName string) bool {
	if item == nil {
		return false
	}
	if s.toolCallNameMatches(item.call.Name, checkpointName) {
		return true
	}
	for _, nativeName := range nativeExecToolNames[item.variant] {
		if strings.EqualFold(strings.TrimSpace(nativeName), strings.TrimSpace(checkpointName)) {
			return true
		}
	}
	return false
}

func (s *Session) toolCallNameMatches(clientName, checkpointName string) bool {
	checkpointName = strings.TrimSpace(checkpointName)
	clientName = strings.TrimSpace(clientName)
	if strings.EqualFold(clientName, checkpointName) {
		return true
	}
	// Cursor persists MCP leaves with its provider prefix, while Exec mcp_args
	// carries the public leaf name. This is namespacing, not a renamed tool.
	return strings.EqualFold("mcp_"+MCPProviderIdentifier+"_"+clientName, checkpointName)
}

func checkpointToolCallGroups(state *agentv1.ConversationStateStructure) ([][]checkpointToolCall, bool) {
	if state == nil || len(state.GetPendingToolCalls()) == 0 {
		return nil, false
	}
	groups := make([][]checkpointToolCall, 0)
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
		group := make([]checkpointToolCall, 0)
		for _, part := range message.Content {
			if part.Type != "tool-call" || strings.TrimSpace(part.ToolCallID) == "" || strings.TrimSpace(part.ToolName) == "" {
				continue
			}
			if _, duplicate := seen[part.ToolCallID]; duplicate {
				return nil, false
			}
			seen[part.ToolCallID] = struct{}{}
			group = append(group, checkpointToolCall{id: part.ToolCallID, name: part.ToolName})
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, len(groups) > 0
}

func checkpointToolCalls(state *agentv1.ConversationStateStructure) ([]checkpointToolCall, bool) {
	groups, ok := checkpointToolCallGroups(state)
	if !ok {
		return nil, false
	}
	out := make([]checkpointToolCall, 0)
	for _, group := range groups {
		out = append(out, group...)
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

// releasePendingExec drops every raw session id and protocol-boundary manager
// lookup key that points at the same pending exec.
func (s *Session) releasePendingExec(item *pendingExec, keys ...string) {
	if s == nil || item == nil {
		return
	}
	s.mu.Lock()
	lookupKeys := make([]string, 0, 4)
	for id, p := range s.pending {
		if p == item {
			lookupKeys = append(lookupKeys, id)
		}
	}
	for _, key := range keys {
		if key != "" {
			lookupKeys = append(lookupKeys, key)
		}
	}
	if item.call.ID != "" {
		lookupKeys = append(lookupKeys, item.call.ID)
	}
	s.clearPendingTool(lookupKeys...)
	for i, pending := range s.pendingOrder {
		if pending == item {
			s.pendingOrder = append(s.pendingOrder[:i], s.pendingOrder[i+1:]...)
			break
		}
	}
	if item.batch != nil {
		removePendingFromBatchLocked(item.batch, item)
	}
	s.mu.Unlock()
	if s.manager == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, key := range lookupKeys {
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

func (s *Session) lookupTool(name string) *ToolDefinition {
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
	return def
}

func (s *Session) lookupMcpTool(name, provider string) *ToolDefinition {
	def := s.lookupTool(name)
	if def == nil || hasNativeCursorToolMapping(*def) {
		return nil
	}
	if provider == "" || provider == MCPProviderIdentifier {
		return def
	}
	return nil
}

func (s *Session) availableToolNames() []string {
	names := make([]string, 0, len(s.toolIndex))
	for name, tool := range s.toolIndex {
		if name != "" && tool != nil && !hasNativeCursorToolMapping(*tool) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *Session) requestContext() *agentv1.RequestContext {
	ctx := headlessRequestContext()
	if len(s.tools) > 0 {
		ctx.Tools = buildMcpToolDefinitions(customMcpTools(s.tools))
	}
	return ctx
}

func (s *Session) rejectUnsupportedExec(stream *BidiStream, req *agentv1.ExecServerMessage) error {
	// Preserve an explicit refusal for genuinely unknown future oneof variants.
	// Known native variants use their typed result schema, including when the
	// current downstream request did not expose a compatible tool.
	log.Warnf("cursor: unsupported exec variant, refusing (unknown proto fields=%v)", unknownExecFieldNumbers(req))
	code := "exec_variant_unsupported"
	// The error text is read by the model. A bare "no handler" was relayed to
	// users as a broken tool environment and made models retry the same
	// built-in until the turn timed out; naming the declared tools steers the
	// next attempt somewhere that works.
	hint := "This built-in tool is not available in this environment."
	if names := s.availableToolNames(); len(names) > 0 {
		hint += " Use the declared tools instead: " + strings.Join(names, ", ") + "."
	}
	control := &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_Throw{
			Throw: &agentv1.ExecClientThrow{
				Id:        req.Id,
				Error:     hint,
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

func customMcpTools(tools []ToolDefinition) []ToolDefinition {
	tools = SanitizeToolDefinitions(tools)
	nativeNames := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if hasNativeCursorToolMapping(tool) {
			nativeNames[strings.ToLower(strings.TrimSpace(tool.Name))] = struct{}{}
		}
	}
	out := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if hasNativeCursorToolMapping(tool) {
			continue
		}
		if _, conflicts := nativeNames[strings.ToLower(strings.TrimSpace(tool.Name))]; conflicts {
			continue
		}
		out = append(out, tool)
	}
	return out
}

func hasNativeCursorToolMapping(tool ToolDefinition) bool {
	// Namespaced and Responses custom/freeform tools are always external MCP
	// leaves, even if their public name happens to be Read/Bash/etc.
	if strings.TrimSpace(tool.Namespace) != "" || strings.EqualFold(tool.Kind, "custom") {
		return false
	}
	kind := strings.ToLower(strings.TrimSpace(tool.Kind))
	if kind == "shell" || kind == "local_shell" || kind == "apply_patch" {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(tool.Name))
	switch name {
	case "bash", "shell", "read", "write", "delete", "grep", "glob", "find", "ls", "edit", "multiedit", "apply_patch", "task", "agent", "subagent", "todowrite", "lsp", "diagnostics", "webfetch", "web_fetch", "fetch":
		return true
	default:
		return false
	}
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
