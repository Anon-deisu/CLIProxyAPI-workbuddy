package cursor

import (
	"errors"
	"testing"
	"time"
)

func newPausedSession(id, conversation string) *Session {
	s := &Session{
		ID:             id,
		ConversationID: conversation,
		waitingTools:   true,
		pending:        map[string]*pendingExec{},
		events:         make(chan StreamEvent, 4),
		lastActivity:   time.Now(),
	}
	return s
}

// A session whose resume already failed is never coming back; it used to be
// exempt from sweeping forever, holding an upstream stream and an account
// slot — enough of them starved every account into 15-minute gateway 504s.
func TestSweepReapsAbandonedToolPausedSessions(t *testing.T) {
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	zombie := newPausedSession("s1", "conv1")
	zombie.manager = m
	m.Register(zombie)
	m.BindPending("call1", zombie)
	zombie.lastActivity = time.Now().Add(-pendingIdleTTL - time.Minute)

	m.Sweep(sessionIdleTTL)

	if _, alive := m.sessions["s1"]; alive {
		t.Fatal("abandoned tool-paused session survived the sweep")
	}
	if m.LookupPending("call1") != nil {
		t.Fatal("pending binding survived its session")
	}
}

// A parked session whose upstream response ended can only ever rebuild; it
// exists to route that rebuild and must not pin the conversation's blobs for
// the full pending TTL — under upstream reset storms those pinned gigabytes
// OOM-killed the process.
func TestSweepReapsParkedRebuildSessionsQuickly(t *testing.T) {
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	parked := newPausedSession("p1", "convP")
	parked.responseEnded = true
	parked.manager = m
	m.Register(parked)
	m.BindPending("call-p1", parked)
	parked.lastActivity = time.Now().Add(-parkedRebuildTTL - time.Minute)

	m.Sweep(sessionIdleTTL)

	if _, alive := m.sessions["p1"]; alive {
		t.Fatal("parked-for-rebuild session survived the short sweep")
	}
	_, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: "call-p1"}})
	if !errors.Is(err, ErrToolResumeRebuild) {
		t.Fatalf("swept parked session must leave a rebuild tombstone, got %v", err)
	}
}

func TestSweepKeepsFreshParkedRebuildSessions(t *testing.T) {
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	parked := newPausedSession("p2", "convP2")
	parked.responseEnded = true
	parked.manager = m
	m.Register(parked)
	m.BindPending("call-p2", parked)
	parked.lastActivity = time.Now().Add(-time.Minute)

	m.Sweep(sessionIdleTTL)

	if _, alive := m.sessions["p2"]; !alive {
		t.Fatal("freshly parked session must survive: its client is about to return")
	}
}

func TestSweepKeepsRecentToolPausedSessions(t *testing.T) {
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	waiting := newPausedSession("s2", "conv2")
	waiting.manager = m
	m.Register(waiting)
	m.BindPending("call2", waiting)
	// Long-running client tool on a live stream: idle within the pending
	// bound must survive.
	waiting.lastActivity = time.Now().Add(-pendingIdleTTL + time.Minute)

	m.Sweep(sessionIdleTTL)

	if _, alive := m.sessions["s2"]; !alive {
		t.Fatal("recent tool-paused session was wrongly reaped")
	}
}

// Agent clients run parallel branches (subagents) that share one conversation
// fingerprint. Registering a sibling's session must never displace the paused
// one — doing so turned every parallel tool round into a resume failure.
func TestRegisterKeepsParallelSiblingsOfOneConversation(t *testing.T) {
	m := &SessionManager{sessions: map[string]*Session{}, pending: map[string]*Session{}}
	paused := newPausedSession("old", "conv3")
	paused.manager = m
	m.Register(paused)
	m.BindPending("call-old", paused)

	sibling := newPausedSession("new", "conv3")
	sibling.waitingTools = false
	sibling.manager = m
	m.Register(sibling)

	if _, alive := m.sessions["old"]; !alive {
		t.Fatal("paused sibling was displaced by a parallel branch")
	}
	if paused.closed {
		t.Fatal("paused sibling was closed")
	}
	if m.LookupPending("call-old") == nil {
		t.Fatal("pending binding lost")
	}
}
