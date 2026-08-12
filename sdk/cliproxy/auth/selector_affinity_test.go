package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const affinityTestModel = "claude-opus-5"

func healthy(id string) *Auth { return &Auth{ID: id, Provider: "cursor", Status: StatusActive} }

func coolingDown(id string) *Auth {
	a := healthy(id)
	a.ModelStates = map[string]*ModelState{
		affinityTestModel: {Unavailable: true, NextRetryAfter: time.Now().Add(10 * time.Minute)},
	}
	return a
}

// orderedSelector mimics a real fallback: it only ever returns an auth that is
// actually selectable, walking its preference list in order.
type orderedSelector struct {
	prefer []string
	calls  int
}

func (o *orderedSelector) Pick(_ context.Context, provider, model string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	available, err := getAvailableAuths(auths, provider, model, time.Now())
	if err != nil {
		return nil, err
	}
	// Rotate the starting point so a stable answer can only come from the
	// affinity layer, never from the fallback happening to repeat itself.
	start := o.calls
	o.calls++
	for i := range o.prefer {
		want := o.prefer[(start+i)%len(o.prefer)]
		if auth := findAuthByID(available, want); auth != nil {
			return auth, nil
		}
	}
	return available[0], nil
}

func pickWithSession(t *testing.T, s *SessionAffinitySelector, auths []*Auth) string {
	t.Helper()
	headers := http.Header{}
	headers.Set("X-Session-ID", "chat-1234")
	got, err := s.Pick(context.Background(), "cursor", affinityTestModel,
		cliproxyexecutor.Options{Headers: headers}, auths)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	return got.ID
}

func TestBusyAccountDoesNotStealTheSessionPermanently(t *testing.T) {
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &orderedSelector{prefer: []string{"b"}},
		TTL:      time.Hour,
	})
	defer s.Stop()

	all := []*Auth{healthy("a"), healthy("b")}
	if got := pickWithSession(t, s, all); got != "b" {
		t.Fatalf("first pick bound %s, want b", got)
	}

	// "b" goes into cooldown, so this turn has to run somewhere else.
	if got := pickWithSession(t, s, []*Auth{healthy("a"), coolingDown("b")}); got != "a" {
		t.Fatalf("during cooldown got %s, want the stand-in a", got)
	}

	// Once it recovers the chat must return to the account that still holds
	// its cached prompt rather than staying on the stand-in.
	if got := pickWithSession(t, s, all); got != "b" {
		t.Fatalf("after recovery got %s, want the original binding b", got)
	}
}

func TestStandInIsStableForTheWholeOutage(t *testing.T) {
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &orderedSelector{prefer: []string{"a", "c"}},
		TTL:      time.Hour,
	})
	defer s.Stop()

	all := []*Auth{healthy("a"), healthy("b"), healthy("c")}
	bound := pickWithSession(t, s, all)

	degraded := []*Auth{healthy("a"), coolingDown("b"), healthy("c")}
	if bound == "b" {
		first := pickWithSession(t, s, degraded)
		second := pickWithSession(t, s, degraded)
		third := pickWithSession(t, s, degraded)
		if first != second || second != third {
			t.Fatalf("stand-in moved %s -> %s -> %s mid-outage; every hop rebuilds the prompt cache", first, second, third)
		}
	}
}

func TestSessionKeepsServingWhileItsAccountIsMissing(t *testing.T) {
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &orderedSelector{prefer: []string{"b"}},
		TTL:      time.Hour,
	})
	defer s.Stop()

	if got := pickWithSession(t, s, []*Auth{healthy("a"), healthy("b")}); got != "b" {
		t.Fatalf("first pick bound %s", got)
	}
	// "b" disappears entirely (removed, or filtered out before selection).
	if got := pickWithSession(t, s, []*Auth{healthy("a")}); got != "a" {
		t.Fatalf("with b missing got %s", got)
	}
	// When it comes back the chat returns to it: that is where its cached
	// prompt still lives.
	if got := pickWithSession(t, s, []*Auth{healthy("a"), healthy("b")}); got != "b" {
		t.Fatalf("did not return to the bound account, got %s", got)
	}
}

func TestProviderFilteredAffinityOnlyPinsCursor(t *testing.T) {
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:        &orderedSelector{prefer: []string{"a", "b"}},
		TTL:             time.Hour,
		ProviderFilters: []string{"cursor"},
	})
	defer s.Stop()
	headers := http.Header{"X-Session-Id": []string{"same-chat"}}
	opts := cliproxyexecutor.Options{Headers: headers}
	auths := []*Auth{healthy("a"), healthy("b")}

	first, err := s.Pick(context.Background(), "cursor", affinityTestModel, opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Pick(context.Background(), "cursor", affinityTestModel, opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("Cursor conversation moved accounts: %s -> %s", first.ID, second.ID)
	}

	// Non-Cursor providers still use the rotating fallback when universal
	// session affinity was not enabled.
	for _, auth := range auths {
		auth.Provider = "claude"
	}
	third, err := s.Pick(context.Background(), "claude", affinityTestModel, opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := s.Pick(context.Background(), "claude", affinityTestModel, opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == fourth.ID {
		t.Fatalf("non-Cursor routing was unexpectedly pinned to %s", third.ID)
	}
}
