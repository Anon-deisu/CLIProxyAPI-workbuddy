package cursor

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// Native conversation continuation.
//
// After every turn the server pushes an authoritative
// ConversationStateStructure checkpoint. The protocol expects the next turn of
// the same conversation to send that state back with only the new user message
// as the action; blobs are content-addressed and fetched over the KV channel
// only when the server is missing one. Re-flattening the client's whole
// history into fresh prompt blobs every turn — the previous behaviour — worked,
// but rebuilt the prompt from scratch each time and left cache stability
// hanging on our reconstruction being byte-identical. Round-tripping the
// server's own state removes that dependency: the prompt prefix is the
// server's, by construction.
//
// Unknown checkpoint fields survive the round trip: proto3 keeps them on the
// decoded message and re-encodes them verbatim, so a partial MVP schema does
// not lose state the full protocol carries.

const (
	continuationTTL  = 2 * time.Hour
	maxContinuations = 64
)

type continuationEntry struct {
	snapshot checkpointSnapshot
	lastSeen time.Time
}

var conversationContinuations = struct {
	sync.Mutex
	byID map[string]*continuationEntry
}{byID: map[string]*continuationEntry{}}

// rememberContinuation stores the latest checkpoint for a conversation so the
// next turn can continue it natively.
func rememberContinuation(conversationID string, snapshot checkpointSnapshot) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || snapshot.state == nil {
		return
	}
	// Without at least one recorded assistant message there is nothing to
	// validate a continuation against, and nothing worth continuing.
	if len(snapshotAssistantMessages(snapshot)) == 0 {
		return
	}
	now := time.Now()
	conversationContinuations.Lock()
	defer conversationContinuations.Unlock()
	if len(conversationContinuations.byID) >= maxContinuations {
		evictContinuationsLocked(now)
	}
	conversationContinuations.byID[conversationID] = &continuationEntry{snapshot: snapshot, lastSeen: now}
}

func evictContinuationsLocked(now time.Time) {
	for id, entry := range conversationContinuations.byID {
		if now.Sub(entry.lastSeen) >= continuationTTL {
			delete(conversationContinuations.byID, id)
		}
	}
	for len(conversationContinuations.byID) >= maxContinuations {
		oldestID := ""
		var oldest time.Time
		for id, entry := range conversationContinuations.byID {
			if oldestID == "" || entry.lastSeen.Before(oldest) {
				oldestID, oldest = id, entry.lastSeen
			}
		}
		delete(conversationContinuations.byID, oldestID)
	}
}

// takeContinuation returns an isolated copy of the stored checkpoint for the
// conversation, if one is fresh enough to continue from.
func takeContinuation(conversationID string) (checkpointSnapshot, bool) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return checkpointSnapshot{}, false
	}
	now := time.Now()
	conversationContinuations.Lock()
	defer conversationContinuations.Unlock()
	entry, ok := conversationContinuations.byID[conversationID]
	if !ok || now.Sub(entry.lastSeen) >= continuationTTL {
		return checkpointSnapshot{}, false
	}
	entry.lastSeen = now
	copied := checkpointSnapshot{
		state: proto.Clone(entry.snapshot.state).(*agentv1.ConversationStateStructure),
		blobs: make(map[string][]byte, len(entry.snapshot.blobs)),
	}
	for key, raw := range entry.snapshot.blobs {
		copied.blobs[key] = append([]byte(nil), raw...)
	}
	return copied, true
}

// DropContinuation forgets a conversation's stored state, forcing the next
// turn to rebuild from client history.
func DropContinuation(conversationID string) {
	dropContinuation(conversationID)
}

// dropContinuation forgets a conversation's stored state, forcing the next
// turn to rebuild from client history.
func dropContinuation(conversationID string) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return
	}
	conversationContinuations.Lock()
	defer conversationContinuations.Unlock()
	delete(conversationContinuations.byID, conversationID)
}

// snapshotAssistantMessages lists the assistant messages recorded in a
// checkpoint, oldest first.
func snapshotAssistantMessages(snapshot checkpointSnapshot) []checkpointAssistantMessage {
	ids := checkpointBlobIDs(snapshot)
	if len(ids) == 0 {
		keys := make([]string, 0, len(snapshot.blobs))
		for key := range snapshot.blobs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]checkpointAssistantMessage, 0)
		for _, key := range keys {
			if msg, ok := parseAssistantBlob(key, snapshot.blobs[key]); ok {
				out = append(out, msg)
			}
		}
		return out
	}
	out := make([]checkpointAssistantMessage, 0)
	for _, id := range ids {
		key := fmt.Sprintf("%x", id)
		raw := snapshot.blobs[key]
		if len(raw) == 0 {
			continue
		}
		if msg, ok := parseAssistantBlob(key, raw); ok {
			out = append(out, msg)
		}
	}
	return out
}

// trailingToolResultMessages returns the run of tool-result messages at the
// end of the conversation, oldest first, or nil when the tail is not a tool
// continuation.
func trailingToolResultMessages(messages []ChatMessage) []ChatMessage {
	end := len(messages)
	for end > 0 && messages[end-1].Role == "system" {
		end--
	}
	start := end
	for start > 0 && messages[start-1].Role == "tool" {
		start--
	}
	if start == end {
		return nil
	}
	return messages[start:end]
}

// snapshotMentionsToolCall reports whether the checkpoint's blobs reference a
// tool call id. Cursor's checkpoint at a tool boundary records the assistant
// turn including its tool-call parts, so the ids of the calls the client is
// answering must appear there — if they do not, the checkpoint belongs to a
// different point of the conversation and continuing from it would corrupt
// the transcript.
func snapshotMentionsToolCall(snapshot checkpointSnapshot, toolCallID string) bool {
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return false
	}
	needles := [][]byte{[]byte(toolCallID)}
	if safe := util.SanitizeClaudeToolID(toolCallID); safe != "" && safe != toolCallID {
		needles = append(needles, []byte(safe))
	}
	for _, raw := range snapshot.blobs {
		for _, needle := range needles {
			if bytes.Contains(raw, needle) {
				return true
			}
		}
	}
	return false
}

// continuationMatchesHistory decides whether the stored server state and the
// history the client sent describe the same conversation at the same point.
// The check is deliberately strict — on any doubt the caller falls back to a
// full rebuild, which is always correct.
func continuationMatchesHistory(snapshot checkpointSnapshot, messages []ChatMessage) bool {
	// Tool continuation: the checkpoint was written when the run paused (or
	// its stream died) on exactly these calls. The pending exec frames are
	// gone with the session, but the server-side conversation state is not.
	// Continuing it keeps the prompt cache and stops the model from replaying
	// the turn's opening — the full rebuild used to answer twice and pay the
	// whole prompt again. The results ride inside the user action.
	if len(trailingToolResultMessages(messages)) > 0 {
		// Native tool-tail continuation (checkpoint + tool outputs as a user
		// message) is rejected by Cursor as provider 400 / resource_exhausted
		// once the Agent run has already ended. Always rebuild from the
		// client's structured tool history instead.
		return false
	}
	// Otherwise only the clean text-turn shape continues natively: a fresh
	// user ask on top of a completed assistant turn.
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "system" {
			continue
		}
		last = i
		break
	}
	if last < 0 || messages[last].Role != "user" || strings.TrimSpace(messages[last].Content) == "" {
		return false
	}
	var lastAssistant string
scan:
	for i := last - 1; i >= 0; i-- {
		switch messages[i].Role {
		case "user", "system":
			continue
		case "assistant":
			// An assistant still holding tool calls means the turn the
			// checkpoint recorded did not finish as plain text; rebuild.
			if len(messages[i].ToolCalls) > 0 {
				return false
			}
			lastAssistant = strings.TrimSpace(messages[i].Content)
			break scan
		default: // "tool" or anything unexpected
			return false
		}
	}
	if lastAssistant == "" {
		return false
	}
	recorded := snapshotAssistantMessages(snapshot)
	if len(recorded) == 0 {
		return false
	}
	return strings.TrimSpace(recorded[len(recorded)-1].text) == lastAssistant
}

// renderToolResultsForContinuation folds tool outputs into the continuation
// action's text. Each result is clamped on its own so one oversized output
// cannot sink the whole turn.
func renderToolResultsForContinuation(results []ChatMessage) string {
	var b strings.Builder
	b.WriteString("Tool results:\n")
	for i := range results {
		name := strings.TrimSpace(results[i].Name)
		if name == "" {
			name = "tool"
		}
		b.WriteString("\n[")
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(strings.TrimSpace(results[i].ToolCallID))
		b.WriteString("]\n")
		b.WriteString(clampMessageContent(results[i].Content))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(toolContinuationNudge)
	return b.String()
}

// buildContinuationRunRequest sends the server's own conversation state back
// with just the new user message as the action.
func buildContinuationRunRequest(model string, messages []ChatMessage, tools []ToolDefinition, conversationID string, snapshot checkpointSnapshot) (*agentv1.AgentClientMessage, map[string][]byte) {
	selection := ResolveRequestedModel(model)
	details, selection := resolveModelDetails(model, selection)

	var text string
	imagesFrom := len(messages)
	if trailing := trailingToolResultMessages(messages); len(trailing) > 0 {
		// The action delivers the tool outputs as message content: the exec
		// frames of the dead session cannot be replayed, but the server-side
		// conversation is still parked right at these calls.
		text = renderToolResultsForContinuation(trailing)
	} else {
		var newest *ChatMessage
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
				newest = &messages[i]
				imagesFrom = i
				break
			}
		}
		text = clampMessageContent(newest.Content)
		if injected, ask := splitInjectedContext(text); ask != "" && injected != "" {
			// The state blobs are the server's; context the client re-renders
			// each turn stays inside the user message rather than mutating them.
			text = injected + "\n\n" + ask
		}
	}
	userMessage := &agentv1.UserMessage{Text: text, MessageId: uuid.NewString()}
	if images := collectImages(messages[imagesFrom:]); len(images) > 0 {
		attachImages(userMessage, images)
	}

	supportsImages := true
	run := &agentv1.AgentRunRequest{
		ConversationId:             &conversationID,
		ConversationState:          snapshot.state,
		ModelDetails:               details,
		RequestedModel:             toRequestedModelProto(selection),
		ClientSupportsInlineImages: &supportsImages,
		Action: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_UserMessageAction{
				UserMessageAction: &agentv1.UserMessageAction{UserMessage: userMessage},
			},
		},
	}
	if defs := buildMcpToolDefinitions(tools); len(defs) > 0 {
		run.McpTools = &agentv1.McpTools{McpTools: defs}
	}
	// The state we are sending back references blobs from earlier turns whose
	// sessions are gone; ship them so the server never has to ask.
	run.PreFetchedBlobs = preFetchedBlobs(snapshot.blobs)
	// Turns typically live in fields the MVP schema does not name; they ride
	// along as preserved unknown fields, so report size, not GetTurns().
	log.Infof("cursor: continuing conversation natively (state=%dB, blobs=%d)",
		proto.Size(snapshot.state), len(snapshot.blobs))
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run},
	}, snapshot.blobs
}
