package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const affinityTestModel = "claude-opus-5"

func healthy(id string) *Auth { return &Auth{ID: id, Provider: "cursor", Status: StatusActive} }

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
