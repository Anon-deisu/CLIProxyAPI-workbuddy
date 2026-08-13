package cursor

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

const (
	sessionIdleTTL    = 30 * time.Minute
	sessionSweepEvery = 1 * time.Minute
	// pendingIdleTTL bounds sessions parked on client tool results whose
	// upstream stream is still open (a true same-stream resume is possible).
	// They used to be exempt from sweeping entirely, but a session whose
	// resume already failed is never coming back: each one held an upstream
	// H2 stream and an account slot forever, and enough of them starved every
	// account — requests then queued until the gateway's 15-minute 504.
	pendingIdleTTL = 20 * time.Minute
	// parkedRebuildTTL bounds sessions whose upstream response already ended
	// while tools were pending. Resuming those always rebuilds (the checkpoint
	// is remembered at Close, and the tool-tail continuation serves the
	// results), so the parked object only exists to route the rebuild — yet
	// each one pins the conversation's blob store. Under upstream reset storms
	// they accumulated gigabytes within the hour and OOM-killed the process.
	parkedRebuildTTL = 3 * time.Minute
)

// SessionManager tracks live Agent sessions waiting on client tool results.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session // session id
	pending  map[string]*Session // tool_call_id -> session
}

var defaultSessionManager = NewSessionManager()

// DefaultSessionManager returns the process-wide manager.
func DefaultSessionManager() *SessionManager { return defaultSessionManager }

// NewSessionManager creates an empty manager.
func NewSessionManager() *SessionManager {
	m := &SessionManager{
		sessions: map[string]*Session{},
		pending:  map[string]*Session{},
	}
	go m.reaper()
	return m
}

func (m *SessionManager) reaper() {
	ticker := time.NewTicker(sessionSweepEvery)
	defer ticker.Stop()
	for range ticker.C {
		m.Sweep(sessionIdleTTL)
	}
}

// Register stores a live session.
func (m *SessionManager) Register(session *Session) {
	if m == nil || session == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[session.ID] = session
	session.touch()
}

// BindPending indexes a tool_call_id to its owning session.
// Both the raw id and its Claude-safe form are indexed so clients that only
// see SanitizeClaudeToolID(id) can still resume the same session.
func (m *SessionManager) BindPending(toolCallID string, session *Session) {
	if m == nil || session == nil || toolCallID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[toolCallID] = session
	if safe := util.SanitizeClaudeToolID(toolCallID); safe != "" && safe != toolCallID {
		m.pending[safe] = session
	}
	session.touch()
}

// UnbindPending removes a tool_call_id index.
func (m *SessionManager) UnbindPending(toolCallID string) {
	if m == nil || toolCallID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, toolCallID)
	if safe := util.SanitizeClaudeToolID(toolCallID); safe != "" && safe != toolCallID {
		delete(m.pending, safe)
	}
}

// RebindSessionPending atomically replaces every tool_call_id index owned by a
// session. Lifecycle/checkpoint ids may arrive after Exec, so rebinding must
// never leave the process-wide resolver pointing at a provisional id only.
func (m *SessionManager) RebindSessionPending(session *Session, toolCallIDs []string) {
	if m == nil || session == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, owner := range m.pending {
		if owner == session || owner != nil && owner.ID == session.ID {
			delete(m.pending, id)
		}
	}
	for _, id := range toolCallIDs {
		if id == "" {
			continue
		}
		m.pending[id] = session
		if safe := util.SanitizeClaudeToolID(id); safe != "" && safe != id {
			m.pending[safe] = session
		}
	}
}

// LookupPending returns the session waiting for the given tool call.
func (m *SessionManager) LookupPending(toolCallID string) *Session {
	if m == nil || toolCallID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.lookupPendingLocked(toolCallID)
	if session != nil {
		session.touch()
	}
	return session
}

func (m *SessionManager) lookupPendingLocked(toolCallID string) *Session {
	if session := m.pending[toolCallID]; session != nil {
		return session
	}
	if safe := util.SanitizeClaudeToolID(toolCallID); safe != "" && safe != toolCallID {
		return m.pending[safe]
	}
	return nil
}

// ResolveForToolResults finds the single live session that owns all results.
func (m *SessionManager) ResolveForToolResults(results []ToolResult) (*Session, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("cursor: no tool results")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var owner *Session
	for _, result := range results {
		if result.ToolCallID == "" {
			return nil, fmt.Errorf("cursor: tool result missing tool_call_id")
		}
		session := m.lookupPendingLocked(result.ToolCallID)
		if session == nil {
			// Clients that retry a long-dead turn land here forever, so keep
			// this quiet; the caller already logs the rebuild it falls back to.
			log.Debugf("cursor: no live session for tool_call_id %q (%d bound)", result.ToolCallID, len(m.pending))
			return nil, fmt.Errorf("cursor: unknown or expired tool_call_id %s", result.ToolCallID)
		}
		if owner == nil {
			owner = session
			continue
		}
		if owner.ID != session.ID {
			return nil, fmt.Errorf("cursor: tool results belong to different sessions")
		}
	}
	if owner != nil {
		owner.touch()
	}
	return owner, nil
}

// Remove drops a session and all of its pending indexes.
func (m *SessionManager) Remove(session *Session) {
	if m == nil || session == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, session.ID)
	for id, owner := range m.pending {
		if owner != nil && owner.ID == session.ID {
			delete(m.pending, id)
		}
	}
}

// Sweep closes idle sessions older than ttl.
func (m *SessionManager) Sweep(ttl time.Duration) {
	if m == nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	m.mu.Lock()
	pendingOwners := make(map[*Session]struct{}, len(m.pending))
	for _, owner := range m.pending {
		if owner != nil {
			pendingOwners[owner] = struct{}{}
		}
	}
	pendingCutoff := time.Now().Add(-pendingIdleTTL)
	parkedCutoff := time.Now().Add(-parkedRebuildTTL)
	stale := make([]*Session, 0)
	for _, session := range m.sessions {
		// A pending tool result is durable protocol state: the client may
		// return after a long-running shell or approval workflow. It still
		// expires eventually — an abandoned one blocks an account slot.
		if _, pending := pendingOwners[session]; pending {
			deadline := pendingCutoff
			if session.parkedForRebuild() {
				deadline = parkedCutoff
			}
			if session.lastActivity.Before(deadline) {
				stale = append(stale, session)
			}
			continue
		}
		if session.lastActivity.Before(cutoff) {
			stale = append(stale, session)
		}
	}
	m.mu.Unlock()
	for _, session := range stale {
		_ = session.Close()
		m.Remove(session)
	}
}
