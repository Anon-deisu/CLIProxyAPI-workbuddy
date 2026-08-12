package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// ChatMessage is a minimal OpenAI-style chat message.
type ChatMessage struct {
	Role       string
	Content    string
	Name       string
	ToolCallID string
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
	selection := ResolveRequestedModel(model)
	model = selection.ModelID
	blobStore := map[string][]byte{}

	// Pull client system prompts out so we don't bury them behind a dummy
	// "You are a helpful assistant." blob (that was a major context-loss source).
	systemPrompt, conversation := splitSystemMessages(messages)
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = "You are a helpful assistant."
	}
	systemBlob := storeBlob(blobStore, mustJSON(map[string]any{
		"role": "system",
		"content": []map[string]string{
			{"type": "text", "text": systemPrompt},
		},
	}))

	var activeUser *ChatMessage
	historyEnd := len(conversation)
	for i := len(conversation) - 1; i >= 0; i-- {
		if conversation[i].Role == "user" && strings.TrimSpace(conversation[i].Content) != "" {
			activeUser = &conversation[i]
			historyEnd = i
			break
		}
	}
	if activeUser == nil {
		return nil, nil, "", fmt.Errorf("cursor: request has no user message")
	}

	history := trimConversationHistory(conversation[:historyEnd])

	// Track tool names so tool-result parts can include toolName (cursor2api).
	toolNames := map[string]string{}
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
			resultPart := map[string]any{
				"type":       "tool-result",
				"toolName":   toolName,
				"toolCallId": msg.ToolCallID,
				"result":     msg.Content,
				"toolKind":   "mcp",
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

	// Keep the Agent's user message as the user's actual instruction; client
	// injected context rides along as a preceding history blob instead.
	activeText := activeUser.Content
	if injected, ask := splitInjectedContext(activeText); ask != "" {
		rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
			"role": "user",
			"content": []map[string]string{
				{"type": "text", "text": injected},
			},
		})))
		activeText = ask
	}

	activeUserMessage := &agentv1.UserMessage{
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

	conversationID := reuseConversationID(conversationKeyFor(model, systemPrompt, conversation))
	// Desktop / cursor2api do not set exclude_workspace_context by default;
	// forcing it true is rejected for many accounts ("Workspace context
	// exclusion is not allowed…").
	supportsImages := true
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
		selection.MaxMode = selection.MaxMode || entry.MaxMode
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
	run := &agentv1.AgentRunRequest{
		ConversationId:             &conversationID,
		ConversationState:          &agentv1.ConversationStateStructure{RootPromptMessagesJson: rootIDs},
		ModelDetails:               details,
		RequestedModel:             toRequestedModelProto(selection),
		ClientSupportsInlineImages: &supportsImages,
		Action: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_UserMessageAction{
				UserMessageAction: &agentv1.UserMessageAction{
					UserMessage: activeUserMessage,
				},
			},
		},
	}
	if defs := buildMcpToolDefinitions(tools); len(defs) > 0 {
		run.McpTools = &agentv1.McpTools{McpTools: defs}
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run},
	}
	return client, blobStore, conversationID, nil
}

const (
	maxHistoryMessages   = 48
	maxHistoryChars      = 120_000
	maxSystemPromptRunes = 40_000
)

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

// trimConversationHistory keeps the newest turns when history is huge so Cursor
// does not silently drop the head (which looked like "context truncation").
func trimConversationHistory(history []ChatMessage) []ChatMessage {
	if len(history) == 0 {
		return history
	}
	start := 0
	if len(history) > maxHistoryMessages {
		start = len(history) - maxHistoryMessages
		// Don't start in the middle of a tool-result suffix.
		for start < len(history) && history[start].Role == "tool" {
			start++
		}
	}
	out := history[start:]
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

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func handleKV(stream *BidiStream, req *agentv1.KvServerMessage, blobStore map[string][]byte) error {
	resp := &agentv1.KvClientMessage{Id: req.Id}
	switch m := req.Message.(type) {
	case *agentv1.KvServerMessage_GetBlobArgs:
		key := hex.EncodeToString(m.GetBlobArgs.GetBlobId())
		if data, ok := blobStore[key]; ok {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{
				GetBlobResult: &agentv1.GetBlobResult{BlobData: data},
			}
		} else {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: &agentv1.GetBlobResult{}}
		}
	case *agentv1.KvServerMessage_SetBlobArgs:
		key := hex.EncodeToString(m.SetBlobArgs.GetBlobId())
		blobStore[key] = append([]byte(nil), m.SetBlobArgs.GetBlobData()...)
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
