package cursor

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	sessionIdleTTL    = 30 * time.Minute
	sessionSweepEvery = 2 * time.Minute
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
func (m *SessionManager) BindPending(toolCallID string, session *Session) {
	if m == nil || session == nil || toolCallID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[toolCallID] = session
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
	return m.pending[toolCallID]
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
	stale := make([]*Session, 0)
	for _, session := range m.sessions {
		// A pending tool result is durable protocol state, not idle traffic. The
		// client may return after a long-running shell or approval workflow.
		if _, pending := pendingOwners[session]; pending {
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
