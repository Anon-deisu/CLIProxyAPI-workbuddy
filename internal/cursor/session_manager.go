package cursor

import (
	"fmt"
	"strings"
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
	// while tools were pending. Resuming those always rebuilds from client
	// history, so the parked object only exists to route that rebuild — yet
	// each one pins the conversation's blob store. Under upstream reset storms
	// they accumulated gigabytes within the hour and OOM-killed the process.
	parkedRebuildTTL = 3 * time.Minute
	// toolTombstoneTTL keeps tool_call_id indexes after a parked session is
	// closed so a client retry a few seconds later still takes the rebuild
	// path instead of "unknown or expired tool_call_id".
	toolTombstoneTTL = 20 * time.Minute
)

// SessionManager tracks live Agent sessions waiting on client tool results.
type SessionManager struct {
	mu         sync.Mutex
	sessions   map[string]*Session // session id
	pending    map[string]*Session // tool_call_id -> session
	tombstones map[string]time.Time
}

var defaultSessionManager = NewSessionManager()

// DefaultSessionManager returns the process-wide manager.
func DefaultSessionManager() *SessionManager { return defaultSessionManager }

// NewSessionManager creates an empty manager.
func NewSessionManager() *SessionManager {
	m := &SessionManager{
		sessions:   map[string]*Session{},
		pending:    map[string]*Session{},
		tombstones: map[string]time.Time{},
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
	m.clearTombstoneLocked(toolCallID)
	m.pending[toolCallID] = session
	if safe := util.SanitizeClaudeToolID(toolCallID); safe != "" && safe != toolCallID {
		m.clearTombstoneLocked(safe)
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
		m.clearTombstoneLocked(id)
		m.pending[id] = session
		if safe := util.SanitizeClaudeToolID(id); safe != "" && safe != id {
			m.clearTombstoneLocked(safe)
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

// scanSessionsForPendingLocked returns a compact "sessionID:key|callID" summary
// for every live session that still indexes the given tool_call_id by key or by
// item.call.ID, or "<none>" when nobody does. Caller must hold m.mu; this
// acquires each session's own mutex, which is the existing (manager->session)
// lock order used by syncManagerBindings.
func (m *SessionManager) scanSessionsForPendingLocked(toolCallID string) string {
	if toolCallID == "" {
		return "<none>"
	}
	live := false
	summary := ""
	for _, session := range m.sessions {
		if session == nil {
			continue
		}
		if session.PendingCallIDAlive(toolCallID) {
			live = true
			if summary != "" {
				summary += ","
			}
			summary += session.ID
		}
	}
	if !live {
		return "<none>"
	}
	return summary
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
			found := m.scanSessionsForPendingLocked(result.ToolCallID)
			log.Debugf("cursor: no live session for tool_call_id %q (%d bound; scan=%s)", result.ToolCallID, len(m.pending), found)
			// Missing ids are a rebuild, not a client-facing failure. The
			// structured tool history on the next StartSession is complete;
			// returning "unknown or expired" only produced 500s after a
			// stream reset.
			return nil, ErrToolResumeRebuild
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
	parked := session.parkedForRebuild()
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, session.ID)
	if m.tombstones == nil {
		m.tombstones = map[string]time.Time{}
	}
	expires := time.Now().Add(toolTombstoneTTL)
	for id, owner := range m.pending {
		if owner != nil && owner.ID == session.ID {
			if parked {
				m.tombstones[id] = expires
			}
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
	m.expireTombstonesLocked(time.Now())
	m.mu.Unlock()
	for _, session := range stale {
		_ = session.Close()
		m.Remove(session)
	}
}

// RememberToolTombstones keeps tool_call_id indexes after the live session is
// gone so the next client POST still rebuilds instead of treating the ids as
// unknown.
func (m *SessionManager) RememberToolTombstones(ids []string) {
	if m == nil || len(ids) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tombstones == nil {
		m.tombstones = map[string]time.Time{}
	}
	expires := time.Now().Add(toolTombstoneTTL)
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		m.tombstones[id] = expires
		if safe := util.SanitizeClaudeToolID(id); safe != "" && safe != id {
			m.tombstones[safe] = expires
		}
	}
}

func (m *SessionManager) hasTombstoneLocked(toolCallID string) bool {
	if m == nil || m.tombstones == nil || toolCallID == "" {
		return false
	}
	now := time.Now()
	if expires, ok := m.tombstones[toolCallID]; ok && now.Before(expires) {
		return true
	}
	if safe := util.SanitizeClaudeToolID(toolCallID); safe != "" && safe != toolCallID {
		if expires, ok := m.tombstones[safe]; ok && now.Before(expires) {
			return true
		}
	}
	return false
}

func (m *SessionManager) clearTombstoneLocked(toolCallID string) {
	if m == nil || m.tombstones == nil || toolCallID == "" {
		return
	}
	delete(m.tombstones, toolCallID)
}

func (m *SessionManager) expireTombstonesLocked(now time.Time) {
	if m == nil || m.tombstones == nil {
		return
	}
	for id, expires := range m.tombstones {
		if !expires.After(now) {
			delete(m.tombstones, id)
		}
	}
}
