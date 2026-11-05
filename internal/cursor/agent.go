package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// ChatMessage is a minimal OpenAI-style chat message.
type ChatMessage struct {
	Role       string
	Content    string
	Name       string
	ToolCallID string
	ToolKind   string
	Namespace  string
	ToolCalls  []ToolCall
	Images     []ChatImage
}

// AccountCredentials are the fields required to open an Agent run.
type AccountCredentials struct {
	AccessToken   string
	RefreshToken  string
	AuthClientID  string
	BaseURL       string
	ClientVersion string
	MachineID     string
	MacMachineID  string
	SessionID     string
	ClientOS      string
	ClientArch    string
	Timezone      string
	Email         string
	GhostMode     string
	CookieJar     *CookieJar
}

// CredentialsFromMetadata extracts Cursor account fields from auth metadata.
func CredentialsFromMetadata(meta map[string]any) AccountCredentials {
	get := func(keys ...string) string {
		for _, key := range keys {
			if meta == nil {
				continue
			}
			if v, ok := meta[key]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	creds := AccountCredentials{
		AccessToken:   get("access_token"),
		RefreshToken:  get("refresh_token"),
		AuthClientID:  get("auth_client_id"),
		BaseURL:       get("base_url"),
		ClientVersion: get("client_version"),
		MachineID:     get("machine_id"),
		MacMachineID:  get("mac_machine_id"),
		SessionID:     get("session_id"),
		ClientOS:      get("client_os"),
		ClientArch:    get("client_arch"),
		Timezone:      get("timezone"),
		Email:         get("email"),
		GhostMode:     get("ghost_mode"),
	}
	if creds.BaseURL == "" {
		creds.BaseURL = cursorauth.DefaultBaseURL
	}
	if creds.ClientVersion == "" {
		creds.ClientVersion = cursorauth.DefaultClientVersion
	}
	if creds.AuthClientID == "" {
		creds.AuthClientID = cursorauth.DefaultAuthClientID
	}
	if creds.MachineID == "" {
		creds.MachineID = DesktopMachineID()
	}
	if creds.MacMachineID == "" {
		creds.MacMachineID = DesktopMacMachineID()
	}
	if creds.ClientOS == "" {
		creds.ClientOS = DesktopClientOS()
	}
	if creds.ClientArch == "" {
		creds.ClientArch = DesktopClientArch()
	}
	if creds.SessionID == "" {
		creds.SessionID = uuid.NewString()
	}
	if creds.GhostMode == "" {
		creds.GhostMode = "implicit-false"
	}
	jarKey := creds.Email
	if jarKey == "" {
		jarKey = creds.MachineID
	}
	creds.CookieJar = CookieJarForAccount(jarKey)
	return creds
}

func storeBlob(store map[string][]byte, data []byte) []byte {
	sum := sha256.Sum256(data)
	id := sum[:]
	store[hex.EncodeToString(id)] = data
	return id
}

func buildRunRequest(model string, messages []ChatMessage, tools []ToolDefinition) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	return buildRunRequestForConversation(model, messages, tools, "")
}

func buildRunRequestForConversation(model string, messages []ChatMessage, tools []ToolDefinition, conversationID string) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	selection := ResolveRequestedModel(model)
	model = selection.ModelID
	blobStore := map[string][]byte{}

	// Pull client system prompts out so we don't bury them behind a dummy
	// "You are a helpful assistant." blob (that was a major context-loss source).
	systemPrompt, conversation := splitSystemMessages(messages)
	systemBlob := storeBlob(blobStore, systemPromptBlob(systemPrompt))

	var activeUser *ChatMessage
	historyEnd := len(conversation)
	if len(conversation) > 0 {
		last := &conversation[len(conversation)-1]
		if last.Role == "user" && (strings.TrimSpace(last.Content) != "" || len(last.Images) > 0) {
			activeUser = last
			historyEnd = len(conversation) - 1
		}
	}
	if activeUser == nil {
		hasUserHistory := false
		for i := range conversation {
			if conversation[i].Role == "user" {
				hasUserHistory = true
				break
			}
		}
		if !hasUserHistory {
			return nil, nil, "", fmt.Errorf("cursor: request has no user message or tool continuation")
		}
	}

	history := trimConversationHistory(conversation[:historyEnd])

	// Track tool names so tool-result parts can include toolName (cursor2api).
	toolNames := map[string]string{}
	toolKinds := map[string]string{}
	toolNamespaces := map[string]string{}
	rootIDs := [][]byte{systemBlob}
	for _, msg := range history {
		switch msg.Role {
		case "user":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role": "user",
				"content": []map[string]string{
					{"type": "text", "text": msg.Content},
				},
			})))
		case "assistant":
			if strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			content := make([]map[string]any, 0, 1+len(msg.ToolCalls))
			if strings.TrimSpace(msg.Content) != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(tc.ID) != "" && strings.TrimSpace(tc.Name) != "" {
					toolNames[tc.ID] = tc.Name
					toolKinds[tc.ID] = cursorHistoryToolKind(tc.Kind)
					toolNamespaces[tc.ID] = tc.Namespace
				}
				args := any(tc.Arguments)
				if args == nil {
					args = map[string]any{}
				}
				content = append(content, map[string]any{
					"type":       "tool-call",
					"toolCallId": tc.ID,
					"toolName":   tc.Name,
					"args":       args,
				})
			}
			if len(content) == 0 {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role":    "assistant",
				"content": content,
			})))
		case "tool":
			if strings.TrimSpace(msg.ToolCallID) == "" {
				continue
			}
			toolName := strings.TrimSpace(msg.Name)
			if toolName == "" {
				toolName = toolNames[msg.ToolCallID]
			}
			toolKind := cursorHistoryToolKind(firstNonEmpty(msg.ToolKind, toolKinds[msg.ToolCallID]))
			resultPart := map[string]any{
				"type":       "tool-result",
				"toolName":   toolName,
				"toolCallId": msg.ToolCallID,
				"result":     msg.Content,
				"toolKind":   toolKind,
			}
			if namespace := firstNonEmpty(msg.Namespace, toolNamespaces[msg.ToolCallID]); namespace != "" {
				resultPart["namespace"] = namespace
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role": "tool",
				"id":   msg.ToolCallID,
				"content": []map[string]any{
					resultPart,
				},
			})))
		}
	}

	if activeUser != nil && isResumeUserText(activeUser.Content) &&
		(conversationEndsWithToolResults(conversation[:historyEnd]) || repeatedReadWithoutWrite(conversation[:historyEnd])) {
		// "继续" is not a new task. Treating it as the whole action dropped
		// the tool-result nudge and the model just repeated Read for 20+ minutes.
		activeUser = nil
	}

	// Keep the Agent's user message as the user's actual instruction; client
	// injected context rides along as a preceding history blob instead.
	var activeUserMessage *agentv1.UserMessage
	if activeUser == nil && conversationEndsWithToolResults(conversation) {
		// A rebuilt run whose history ends in tool results has no plain user
		// message left, and a bare ResumeAction dies silently: the server has
		// no state for this fresh conversation id and answers with an
		// immediate empty END_STREAM. Those were the dashboard's "N tokens in,
		// 0 out" rows, and each client retry repeated the loop. The results
		// themselves already sit in the history blobs; this only gives the
		// turn an action to execute. Constant wording keeps the prompt prefix
		// cache-stable.
		nudge := toolContinuationNudge
		if repeatedReadWithoutWrite(conversation) {
			nudge = writeLoopBreakerNudge
		}
		activeUserMessage = &agentv1.UserMessage{
			Text:      nudge,
			MessageId: uuid.NewString(),
		}
	}
	if activeUser != nil {
		// The live turn bypasses trimConversationHistory, so clamp it too: a
		// huge paste in the current message overflows the model on its own and
		// no retry tier can shrink it.
		activeText := clampMessageContent(activeUser.Content)
		if injected, ask := splitInjectedContext(activeText); ask != "" {
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role": "user",
				"content": []map[string]string{
					{"type": "text", "text": injected},
				},
			})))
			activeText = ask
		}

		activeUserMessage = &agentv1.UserMessage{
			Text:      activeText,
			MessageId: uuid.NewString(),
		}
		// Images ride on the active turn; without this they were dropped and the
		// model answered as if no picture had been sent.
		images := collectImages(conversation[historyEnd:])
		if len(images) > 0 {
			attachImages(activeUserMessage, images)
			log.Debugf("cursor: attached %d inline image(s) to the user turn", len(images))
		}
	}

	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		conversationID = uuid.NewString()
	}
	// Desktop / cursor2api do not set exclude_workspace_context by default;
	// forcing it true is rejected for many accounts ("Workspace context
	// exclusion is not allowed…").
	supportsImages := true
	details, selection := resolveModelDetails(model, selection)
	run := &agentv1.AgentRunRequest{
		ConversationId:             &conversationID,
		ConversationState:          &agentv1.ConversationStateStructure{RootPromptMessagesJson: rootIDs},
		ModelDetails:               details,
		RequestedModel:             toRequestedModelProto(selection),
		ClientSupportsInlineImages: &supportsImages,
		Action:                     &agentv1.ConversationAction{},
	}
	if activeUserMessage == nil {
		// Continuation of a partial assistant message (client prefill /
		// "continue" pattern). Same failure class as the tool tail: a bare
		// ResumeAction on a conversation id the server has no state for gets
		// an immediate empty END_STREAM. Every fresh run request therefore
		// carries an executable user action, always.
		activeUserMessage = &agentv1.UserMessage{
			Text:      assistantContinuationNudge,
			MessageId: uuid.NewString(),
		}
	}
	run.Action.Action = &agentv1.ConversationAction_UserMessageAction{
		UserMessageAction: &agentv1.UserMessageAction{UserMessage: activeUserMessage},
	}
	if defs := buildMcpToolDefinitions(customMcpTools(tools)); len(defs) > 0 {
		run.McpTools = &agentv1.McpTools{McpTools: defs}
	}
	run.PreFetchedBlobs = preFetchedBlobs(blobStore)
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run},
	}
	return client, blobStore, conversationID, nil
}

func preFetchedBlobs(store map[string][]byte) []*agentv1.PreFetchedBlob {
	if len(store) == 0 {
		return nil
	}
	keys := make([]string, 0, len(store))
	for key := range store {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*agentv1.PreFetchedBlob, 0, len(keys))
	for _, key := range keys {
		id, err := hex.DecodeString(key)
		if err != nil {
			continue
		}
		out = append(out, &agentv1.PreFetchedBlob{Id: id, Value: store[key]})
	}
	return out
}

func resolveModelDetails(model string, selection ModelSelection) (*agentv1.ModelDetails, ModelSelection) {
	publicID := selection.PublicID
	if publicID == "" {
		publicID = model
	}
	modelID := selection.ModelID
	if modelID == "" {
		modelID = publicID
	}
	displayID := publicID
	displayName := publicID
	if entry, ok := catalogEntry(publicID); ok {
		if entry.DisplayModel != "" {
			displayID = entry.DisplayModel
		}
		if entry.DisplayName != "" {
			displayName = entry.DisplayName
		}
		// Do NOT promote entry.MaxMode here. ResolveRequestedModel already sets
		// selection.MaxMode from the catalog for bare ids (and from the matched
		// variant for explicit variant requests). Re-ORing entry.MaxMode would
		// promote an explicitly requested non-max variant (e.g. "-thinking"
		// without max) back up to max mode.
		if !selection.VariantStringRepr && len(selection.Parameters) == 0 && len(entry.Parameters) > 0 {
			selection.Parameters = append([]ModelParameter(nil), entry.Parameters...)
		}
		if selection.VariantStringRepr && strings.TrimSpace(selection.ModelID) != "" {
			modelID = selection.ModelID
		}
	}
	details := &agentv1.ModelDetails{ModelId: modelID, DisplayModelId: displayID, DisplayName: displayName}
	if selection.MaxMode {
		maxMode := true
		details.MaxMode = &maxMode
	}
	return details, selection
}

func cursorHistoryToolKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "shell", "local_shell", "apply_patch", "custom", "function":
		return kind
	case "":
		return "function"
	default:
		return kind
	}
}

const (
	maxHistoryChars      = 120_000
	maxSystemPromptRunes = 40_000
	// maxMessageChars bounds a single turn. Dropping whole messages cannot
	// save a history whose *last* message is a 500 KB tool result, and the
	// provider answers the oversized prompt with a 400 that no retry tier can
	// undo.
	maxMessageChars = 60_000
)

// clampMessageContent keeps the head and tail of an oversized turn: the request
// is stated at the top and the newest output sits at the bottom, so cutting the
// middle preserves what the model actually needs.
// toolContinuationNudge is the executable action for a rebuilt turn whose
// history ends in tool results. Wording is constant so the prompt prefix stays
// cache-stable, and it forbids re-running finished calls because a rebuilt
// model otherwise sometimes repeated the previous tool round.
const toolContinuationNudge = "The tool results above have been delivered. " +
	"Continue the turn using them and finish the pending request. " +
	"Do not re-run tool calls that already have results."

// writeLoopBreakerNudge is used when the rebuilt history is a Read-only
// spin (the model promised to Write, only Read arrived, the local file
// never appeared). A bare "continue" made that loop last tens of minutes.
const writeLoopBreakerNudge = "The file was never written. Previous turns only called Read. " +
	"Call Write now with the complete file contents. Do not Read that file again."

// assistantContinuationNudge drives the client prefill / "continue" pattern,
// where the newest message is a partial assistant turn to extend.
const assistantContinuationNudge = "Continue exactly from where the assistant " +
	"message above stopped. Do not repeat any text that was already written."

func isResumeUserText(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "继续", "繼續", "continue", "go on", "keep going", "please continue", "go ahead", "cont":
		return true
	default:
		return false
	}
}

func repeatedReadWithoutWrite(messages []ChatMessage) bool {
	return repeatedReadCount(messages, "") >= 3
}

func isReadToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read", "read_file", "readfile":
		return true
	default:
		return false
	}
}

func isReadExecVariant(variant string) bool {
	return variant == "read_args" || variant == "pi_read_args"
}

func isMutatingToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write", "edit", "multiedit", "apply_patch":
		return true
	default:
		return false
	}
}

func normalizeToolPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, "\\", "/")
	return strings.ToLower(path)
}

func toolCallPath(args map[string]any) string {
	if args == nil {
		return ""
	}
	for _, key := range []string{"path", "file_path", "filePath", "filename"} {
		if value, ok := args[key].(string); ok {
			if path := normalizeToolPath(value); path != "" {
				return path
			}
		}
	}
	return ""
}

func repeatedReadCount(messages []ChatMessage, path string) int {
	path = normalizeToolPath(path)
	reads := 0
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "user" && !isResumeUserText(msg.Content) && strings.TrimSpace(msg.Content) != "" {
			break
		}
		if msg.Role != "assistant" {
			continue
		}
		for _, call := range msg.ToolCalls {
			if isMutatingToolName(call.Name) {
				return reads
			}
			if !isReadToolName(call.Name) {
				continue
			}
			if path != "" && toolCallPath(call.Arguments) != path {
				continue
			}
			reads++
		}
	}
	return reads
}

func lastReadResultForPath(messages []ChatMessage, path string) string {
	path = normalizeToolPath(path)
	if path == "" {
		return ""
	}
	ids := map[string]struct{}{}
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, call := range msg.ToolCalls {
			if isReadToolName(call.Name) && toolCallPath(call.Arguments) == path && strings.TrimSpace(call.ID) != "" {
				ids[call.ID] = struct{}{}
			}
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "tool" {
			continue
		}
		if _, ok := ids[msg.ToolCallID]; ok {
			return msg.Content
		}
	}
	return ""
}

const duplicateReadReplyPrefix = "This file was already read in this turn. Do not Read it again. Call Write now with the complete file contents.\n"

func duplicateReadReplyText(messages []ChatMessage, path string) string {
	return duplicateReadReplyTextFrom(lastReadResultForPath(messages, path))
}

func duplicateReadReplyTextFrom(prev string) string {
	if strings.TrimSpace(prev) != "" {
		return duplicateReadReplyPrefix + "Previous Read result:\n" + clampMessageContent(prev)
	}
	return strings.TrimSpace(duplicateReadReplyPrefix)
}

func seedTurnReadState(messages []ChatMessage) (map[string]int, map[string]string) {
	counts := map[string]int{}
	last := map[string]string{}
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "user" && !isResumeUserText(msg.Content) && strings.TrimSpace(msg.Content) != "" {
			break
		}
		if msg.Role != "assistant" {
			continue
		}
		hitMut := false
		for _, call := range msg.ToolCalls {
			if isMutatingToolName(call.Name) {
				hitMut = true
				break
			}
			if !isReadToolName(call.Name) {
				continue
			}
			path := toolCallPath(call.Arguments)
			if path == "" {
				continue
			}
			counts[path]++
		}
		if hitMut {
			break
		}
	}
	for path := range counts {
		if text := lastReadResultForPath(messages, path); text != "" {
			last[path] = text
		}
	}
	return counts, last
}

// conversationEndsWithToolResults reports whether the newest substantive
// message is a tool result, i.e. the request is a tool continuation.
func conversationEndsWithToolResults(conversation []ChatMessage) bool {
	sawTool := false
	for i := len(conversation) - 1; i >= 0; i-- {
		switch conversation[i].Role {
		case "tool":
			sawTool = true
		case "user":
			if isResumeUserText(conversation[i].Content) {
				continue
			}
			return sawTool
		case "assistant":
			return sawTool
		}
	}
	return sawTool
}

func clampMessageContent(content string) string {
	if len(content) <= maxMessageChars {
		return content
	}
	runes := []rune(content)
	if len(runes) <= maxMessageChars {
		return content
	}
	head := maxMessageChars * 2 / 3
	tail := maxMessageChars - head
	omitted := len(runes) - head - tail
	return string(runes[:head]) +
		fmt.Sprintf("\n\n…[CLIProxyAPI truncated %d characters]…\n\n", omitted) +
		string(runes[len(runes)-tail:])
}

// trimConversationHistory bounds the prompt by size only. Capping the number of
// turns dropped context that easily fit, so a long chat of short messages is
// kept whole and only genuinely oversized history is cut, newest turns first.
func trimConversationHistory(history []ChatMessage) []ChatMessage {
	if len(history) == 0 {
		return history
	}
	out := make([]ChatMessage, 0, len(history))
	for _, msg := range history {
		msg.Content = clampMessageContent(msg.Content)
		out = append(out, msg)
	}
	total := 0
	for _, msg := range out {
		total += len(msg.Content)
		for _, tc := range msg.ToolCalls {
			total += len(tc.Name) + 32
		}
	}
	for total > maxHistoryChars && len(out) > 2 {
		drop := 1
		// Drop a whole assistant+tool group when possible.
		if out[0].Role == "assistant" {
			drop = 1
			for drop < len(out) && out[drop].Role == "tool" {
				drop++
			}
		} else if out[0].Role == "tool" {
			for drop < len(out) && out[drop].Role == "tool" {
				drop++
			}
		}
		for i := 0; i < drop && i < len(out); i++ {
			total -= len(out[i].Content)
			for _, tc := range out[i].ToolCalls {
				total -= len(tc.Name) + 32
			}
		}
		out = out[drop:]
		for len(out) > 0 && out[0].Role == "tool" {
			total -= len(out[0].Content)
			out = out[1:]
		}
	}
	return out
}

func splitSystemMessages(messages []ChatMessage) (systemPrompt string, conversation []ChatMessage) {
	parts := make([]string, 0)
	conversation = make([]ChatMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "system" {
			if text := strings.TrimSpace(msg.Content); text != "" {
				parts = append(parts, text)
			}
			continue
		}
		conversation = append(conversation, msg)
	}
	return truncateRunes(strings.Join(parts, "\n\n"), maxSystemPromptRunes), conversation
}

// systemInstructionLabel introduces a relayed system prompt. Wording is
// constant so the prompt prefix stays byte-stable and the provider cache keeps
// hitting across turns.
const systemInstructionLabel = "The following are system instructions for this conversation. " +
	"Follow them for every subsequent reply.\n\n"

// systemPromptBlob renders the client's system prompt as a root-prompt message.
//
// Cursor's Agent owns the system prompt: it accepts a blob with role "system"
// and then discards it, so a client system message reached the model as nothing
// at all — the same text scored identical prompt_tokens whether it was sent or
// omitted, and instructions in it were never followed. A user-role blob does
// arrive, so the prompt travels as one, labelled to be read as standing
// instruction rather than a question to answer. With no client prompt the
// historical placeholder shape is kept so the root prompt is never empty.
func systemPromptBlob(systemPrompt string) []byte {
	if strings.TrimSpace(systemPrompt) == "" {
		return mustJSON(map[string]any{
			"role": "system",
			"content": []map[string]string{
				{"type": "text", "text": "You are a helpful assistant."},
			},
		})
	}
	return mustJSON(map[string]any{
		"role": "user",
		"content": []map[string]string{
			{"type": "text", "text": systemInstructionLabel + systemPrompt},
		},
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func handleKV(stream *BidiStream, req *agentv1.KvServerMessage, blobStore map[string][]byte, blobMu *sync.Mutex) error {
	resp := &agentv1.KvClientMessage{Id: req.Id}
	switch m := req.Message.(type) {
	case *agentv1.KvServerMessage_GetBlobArgs:
		key := hex.EncodeToString(m.GetBlobArgs.GetBlobId())
		blobMu.Lock()
		data, ok := blobStore[key]
		data = append([]byte(nil), data...)
		blobMu.Unlock()
		if ok {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{
				GetBlobResult: &agentv1.GetBlobResult{BlobData: data},
			}
		} else {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: &agentv1.GetBlobResult{}}
		}
	case *agentv1.KvServerMessage_SetBlobArgs:
		key := hex.EncodeToString(m.SetBlobArgs.GetBlobId())
		blobMu.Lock()
		blobStore[key] = append([]byte(nil), m.SetBlobArgs.GetBlobData()...)
		blobMu.Unlock()
		resp.Message = &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}}
	default:
		return nil
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_KvClientMessage{KvClientMessage: resp},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return stream.WriteEnvelope(payload, false)
}

func sendExecStreamClose(stream *BidiStream, id uint32) error {
	control := &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_StreamClose{
			StreamClose: &agentv1.ExecClientStreamClose{Id: id},
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: control},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return stream.WriteEnvelope(payload, false)
}

func headlessRequestContext() *agentv1.RequestContext {
	trueVal := true
	falseVal := false
	ctx := &agentv1.RequestContext{
		EnvInfoComplete:             &trueVal,
		RulesInfoComplete:           &trueVal,
		RepositoryInfoComplete:      &trueVal,
		GitRepoInfoComplete:         &trueVal,
		GitStatusInfoComplete:       &trueVal,
		CustomSubagentsInfoComplete: &trueVal,
		AgentSkillsInfoComplete:     &trueVal,
		McpInfoComplete:             &trueVal,
		McpFileSystemInfoComplete:   &trueVal,
		WebFetchEnabled:             &falseVal,
		WebSearchEnabled:            &falseVal,
		SupportsMcpAuth:             &falseVal,
		ReadLintsEnabled:            &falseVal,
	}
	return ctx
}
