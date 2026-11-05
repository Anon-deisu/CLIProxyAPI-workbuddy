package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestFailedRefreshKeepsTheCatalogAlreadyRegistered(t *testing.T) {
	const authID = "cursor-user_keepcatalog.json"
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	live := make([]*registry.ModelInfo, 0, 20)
	for _, id := range []string{
		"default", "claude-4.6-sonnet", "gpt-5.4", "claude-opus-5", "claude-fable-5",
		"grok-4.5", "gpt-5.6-sol", "composer-2.5-fast",
	} {
		live = append(live, &registry.ModelInfo{ID: id, Object: "model", Type: "cursor"})
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "cursor", live)

	// A refresh that could not reach Cursor must not downgrade the account to
	// the three builtin placeholders.
	got := cursorModelsOrPrevious(authID, nil)
	if len(got) != len(live) {
		t.Fatalf("kept %d models, want the %d already registered", len(got), len(live))
	}
}

func TestUnknownAuthStillGetsBuiltins(t *testing.T) {
	got := cursorModelsOrPrevious("cursor-user_never-registered.json", nil)
	if len(got) != len(registry.GetCursorModels()) {
		t.Fatalf("got %d models, want the builtin list", len(got))
	}
}

func TestSuccessfulFetchWins(t *testing.T) {
	const authID = "cursor-user_fetchwins.json"
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	registry.GetGlobalRegistry().RegisterClient(authID, "cursor", []*registry.ModelInfo{
		{ID: "stale-1", Object: "model", Type: "cursor"},
		{ID: "stale-2", Object: "model", Type: "cursor"},
		{ID: "stale-3", Object: "model", Type: "cursor"},
		{ID: "stale-4", Object: "model", Type: "cursor"},
	})
	fetched := []*registry.ModelInfo{{ID: "fresh-1", Object: "model", Type: "cursor"}}
	got := cursorModelsOrPrevious(authID, fetched)
	if len(got) != 1 || got[0].ID != "fresh-1" {
		t.Fatalf("a live catalog must win, got %#v", got)
	}
}
