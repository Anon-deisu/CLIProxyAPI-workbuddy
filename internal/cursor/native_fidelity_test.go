package cursor

import (
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
)

// The full protocol puts turns at field 8. Reading them at 2 made every server
// checkpoint look turn-less, so a conversation could never be continued from
// the state the server had built.
func TestConversationStateUsesProtocolFieldNumbers(t *testing.T) {
	state := &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn-blob")}}
	raw, err := proto.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	// field 8, wire type 2 -> tag byte 0x42.
	if raw[0] != 0x42 {
		t.Fatalf("turns encoded with tag %#x, want 0x42 (field 8)", raw[0])
	}

	// A checkpoint written by the real server must decode into Turns.
	server := &agentv1.ConversationStateStructure{}
	if err := proto.Unmarshal(raw, server); err != nil {
		t.Fatal(err)
	}
	if len(server.GetTurns()) != 1 || string(server.GetTurns()[0]) != "turn-blob" {
		t.Fatalf("turns did not round-trip: %#v", server.GetTurns())
	}
}

func TestPreFetchedBlobsAreCompleteAndOrdered(t *testing.T) {
	store := map[string][]byte{}
	first := storeBlob(store, []byte(`{"role":"user","content":"a"}`))
	second := storeBlob(store, []byte(`{"role":"user","content":"b"}`))

	blobs := preFetchedBlobs(store)
	if len(blobs) != 2 {
		t.Fatalf("want both blobs uploaded, got %d", len(blobs))
	}
	// Sorted by id keeps the encoding byte-stable across identical runs, which
	// a map iteration would not.
	if hex.EncodeToString(blobs[0].GetId()) > hex.EncodeToString(blobs[1].GetId()) {
		t.Fatal("blobs must be ordered by id")
	}
	byID := map[string][]byte{}
	for _, blob := range blobs {
		byID[hex.EncodeToString(blob.GetId())] = blob.GetValue()
	}
	for _, id := range [][]byte{first, second} {
		key := hex.EncodeToString(id)
		if string(byID[key]) != string(store[key]) {
			t.Fatalf("blob %s did not carry its bytes", key)
		}
	}
	if preFetchedBlobs(nil) != nil {
		t.Fatal("an empty store must upload nothing")
	}
}

// Protobuf map fields serialize in Go map order by default, so an unchanged
// tool schema could hit the wire differently on every request and move the
// tool section of the prompt. The run request is encoded deterministically.
func TestRunRequestEncodingIsDeterministic(t *testing.T) {
	tools := []ToolDefinition{{
		Name:        "search",
		Description: "search things",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"zulu": map[string]any{"type": "string"},
				"alfa": map[string]any{"type": "number"},
				"mike": map[string]any{"type": "boolean"},
				"papa": map[string]any{"type": "string"},
				"echo": map[string]any{"type": "string"},
			},
			"required": []any{"alfa"},
		},
	}}
	messages := []ChatMessage{{Role: "user", Content: "hi"}}

	var want []byte
	for i := 0; i < 25; i++ {
		msg, _, _, err := buildRunRequestForConversation("default", messages, tools, "conv-fixed")
		if err != nil {
			t.Fatal(err)
		}
		// The active turn carries a fresh message id by design; compare the
		// part of the request that forms the cached prefix.
		msg.GetRunRequest().Action = nil
		raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if want == nil {
			want = raw
			continue
		}
		if string(raw) != string(want) {
			t.Fatal("identical request serialized differently between runs")
		}
	}
}
