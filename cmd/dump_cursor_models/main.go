package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
)

func main() {
	dir := "/opt/cli-proxy/auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "readdir: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasPrefix(name, "cursor-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(raw, &meta); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			continue
		}
		creds := cursorlib.CredentialsFromMetadata(meta)
		models, err := cursorlib.FetchAvailableModels(ctx, creds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: fetch %v\n", name, err)
			continue
		}
		fmt.Printf("=== %s models=%d ===\n", name, len(models))
		for _, m := range models {
			sel := cursorlib.ResolveRequestedModel(m.ID)
			fmt.Printf("avail id=%-28s wire=%-42s resolve=%-42s variant=%v\n", m.ID, m.WireID, sel.ModelID, sel.VariantStringRepr)
		}
		fmt.Printf("\n--- resolve probes ---\n")
		for _, id := range []string{"claude-haiku-4-5", "claude-fable-5", "default", "claude-sonnet-4-5"} {
			sel := cursorlib.ResolveRequestedModel(id)
			fmt.Printf("%s -> model_id=%q public=%q variant=%v params=%v\n", id, sel.ModelID, sel.PublicID, sel.VariantStringRepr, sel.Parameters)
		}
		break
	}
}
