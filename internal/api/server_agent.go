package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentworkspace"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	agentJSONLimit      = 1 << 20
	agentUploadLimit    = 32 << 20
	agentEventHeartbeat = 15 * time.Second
)

type agentModel struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Provider         string   `json:"provider"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
}

func (s *Server) registerAgentRoutes(group *gin.RouterGroup) {
	group.GET("/agent/models", s.agentListModels)
	group.GET("/agent/status", s.agentStatus)
	group.GET("/agent/conversations", s.agentConversations)
	group.POST("/agent/conversations", s.agentConversations)
	group.GET("/agent/conversations/:conversationID", s.agentConversation)
	group.PATCH("/agent/conversations/:conversationID", s.agentConversation)
	group.DELETE("/agent/conversations/:conversationID", s.agentConversation)
	group.POST("/agent/conversations/:conversationID/messages", s.agentSendMessage)
	group.POST("/agent/conversations/:conversationID/cancel", s.agentCancel)
	group.GET("/agent/conversations/:conversationID/events", s.agentEvents)
	group.GET("/agent/conversations/:conversationID/files", s.agentFiles)
	group.POST("/agent/conversations/:conversationID/files", s.agentFiles)
	group.DELETE("/agent/conversations/:conversationID/files", s.agentFiles)
}

func (s *Server) agentWorkspace(c *gin.Context) *agentworkspace.Store {
	s.agentStoreOnce.Do(func() {
		root := strings.TrimSpace(os.Getenv("CPA_AGENT_WORKSPACE_DIR"))
		if root == "" {
			root = filepath.Join(filepath.Dir(s.configFilePath), "runtime", "agent-workspace")
		}
		s.agentStore, s.agentStoreErr = agentworkspace.NewStore(root, agentworkspace.ModelRunnerFunc(s.agentComplete), agentworkspace.NewDockerRunner(os.Getenv("CPA_AGENT_SANDBOX_IMAGE")))
	})
	if s.agentStoreErr != nil {
		log.WithError(s.agentStoreErr).Error("agent workspace unavailable")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Agent workspace storage is unavailable"})
		return nil
	}
	return s.agentStore
}

func (s *Server) agentModels() []agentModel {
	reg := registry.GetGlobalRegistry()
	var infos []*registry.ModelInfo
	for _, auth := range s.handlers.AuthManager.List() {
		if auth.IsDispatchDisabled() || auth.AuthSourceKind() != coreauth.AuthSourceFile {
			continue
		}
		infos = append(infos, reg.GetModelsForClient(auth.ID)...)
	}
	models := make([]agentModel, 0, len(infos))
	seen := make(map[string]bool)
	for _, info := range infos {
		if info.Type == registry.OpenAIImageModelType || seen[info.ID] {
			continue
		}
		name := info.DisplayName
		if name == "" {
			name = info.ID
		}
		model := agentModel{ID: info.ID, Name: name, Provider: info.OwnedBy, ReasoningEfforts: []string{}}
		if thinking := info.Thinking; thinking != nil {
			model.ReasoningEfforts = append(model.ReasoningEfforts, thinking.Levels...)
			if len(thinking.Levels) == 0 && thinking.Max > 0 {
				model.ReasoningEfforts = append(model.ReasoningEfforts, "low", "medium", "high")
			}
			if thinking.ZeroAllowed && !containsAgentEffort(model.ReasoningEfforts, "none") {
				model.ReasoningEfforts = append([]string{"none"}, model.ReasoningEfforts...)
			}
			if thinking.DynamicAllowed && !containsAgentEffort(model.ReasoningEfforts, "auto") {
				model.ReasoningEfforts = append(model.ReasoningEfforts, "auto")
			}
		}
		models = append(models, model)
		seen[info.ID] = true
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func (s *Server) agentListModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"models": s.agentModels()})
}

func (s *Server) agentStatus(c *gin.Context) {
	store := s.agentWorkspace(c)
	if store == nil {
		return
	}
	c.JSON(http.StatusOK, gin.H{"sandbox": store.SandboxStatus(c.Request.Context()), "shared": true})
}

func (s *Server) agentConversations(c *gin.Context) {
	store := s.agentWorkspace(c)
	if store == nil {
		return
	}
	if c.Request.Method == http.MethodGet {
		c.JSON(http.StatusOK, gin.H{"conversations": store.List()})
		return
	}
	var options agentworkspace.ConversationOptions
	if !bindAgentJSON(c, &options) {
		return
	}
	if options.Model != "" && !s.validateAgentModel(c, options.Model, options.ReasoningEffort) {
		return
	}
	conversation, errCreate := store.Create(options)
	if errCreate != nil {
		writeAgentError(c, errCreate)
		return
	}
	c.JSON(http.StatusCreated, conversation)
}

func (s *Server) agentConversation(c *gin.Context) {
	store := s.agentWorkspace(c)
	if store == nil {
		return
	}
	id := c.Param("conversationID")
	switch c.Request.Method {
	case http.MethodDelete:
		if errDelete := store.Delete(id); errDelete != nil {
			writeAgentError(c, errDelete)
			return
		}
		c.Status(http.StatusNoContent)
	case http.MethodPatch:
		var update agentworkspace.ConversationUpdate
		if !bindAgentJSON(c, &update) {
			return
		}
		snapshot, errGet := store.Get(id)
		if errGet != nil {
			writeAgentError(c, errGet)
			return
		}
		model, effort := snapshot.Model, snapshot.ReasoningEffort
		if update.Model != nil {
			model = *update.Model
		}
		if update.ReasoningEffort != nil {
			effort = *update.ReasoningEffort
		}
		if model != "" && !s.validateAgentModel(c, model, effort) {
			return
		}
		conversation, errUpdate := store.Update(id, update)
		if errUpdate != nil {
			writeAgentError(c, errUpdate)
			return
		}
		c.JSON(http.StatusOK, conversation)
	default:
		conversation, errGet := store.Get(id)
		if errGet != nil {
			writeAgentError(c, errGet)
			return
		}
		c.JSON(http.StatusOK, conversation)
	}
}

func (s *Server) agentSendMessage(c *gin.Context) {
	store := s.agentWorkspace(c)
	if store == nil {
		return
	}
	var body struct {
		Message         string `json:"message"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if !bindAgentJSON(c, &body) {
		return
	}
	id := c.Param("conversationID")
	conversation, errGet := store.Get(id)
	if errGet != nil {
		writeAgentError(c, errGet)
		return
	}
	if body.Model == "" {
		body.Model, body.ReasoningEffort = conversation.Model, conversation.ReasoningEffort
	}
	if !s.validateAgentModel(c, body.Model, body.ReasoningEffort) {
		return
	}
	if errStart := store.Start(id, body.Message, agentworkspace.ConversationOptions{Model: body.Model, ReasoningEffort: body.ReasoningEffort}); errStart != nil {
		writeAgentError(c, errStart)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"id": id, "status": "running"})
}

func (s *Server) agentCancel(c *gin.Context) {
	store := s.agentWorkspace(c)
	if store == nil {
		return
	}
	if errCancel := store.Cancel(c.Param("conversationID")); errCancel != nil {
		writeAgentError(c, errCancel)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "canceling"})
}

func (s *Server) agentEvents(c *gin.Context) {
	store := s.agentWorkspace(c)
	if store == nil {
		return
	}
	id := c.Param("conversationID")
	changed, unsubscribe, errSubscribe := store.Subscribe(id)
	if errSubscribe != nil {
		writeAgentError(c, errSubscribe)
		return
	}
	defer unsubscribe()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	flush := func() bool {
		conversation, errGet := store.Get(id)
		if errGet != nil {
			return false
		}
		encoded, errEncode := json.Marshal(conversation)
		if errEncode != nil {
			return false
		}
		if _, errWrite := fmt.Fprintf(c.Writer, "event: snapshot\ndata: %s\n\n", encoded); errWrite != nil {
			return false
		}
		c.Writer.Flush()
		return true
	}
	if !flush() {
		return
	}
	heartbeat := time.NewTicker(agentEventHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-changed:
			if !flush() {
				return
			}
		case <-heartbeat.C:
			if _, errWrite := io.WriteString(c.Writer, ": keep-alive\n\n"); errWrite != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

func (s *Server) agentFiles(c *gin.Context) {
	store := s.agentWorkspace(c)
	if store == nil {
		return
	}
	id, path := c.Param("conversationID"), c.Query("path")
	switch c.Request.Method {
	case http.MethodPost:
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, agentUploadLimit)
		data, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds upload limit"})
			return
		}
		if errWrite := store.WriteFile(id, path, data); errWrite != nil {
			writeAgentError(c, errWrite)
			return
		}
		c.Status(http.StatusNoContent)
	case http.MethodDelete:
		conversation, errGet := store.Get(id)
		if errGet != nil {
			writeAgentError(c, errGet)
			return
		}
		if conversation.Status == agentworkspace.ConversationRunning {
			writeAgentError(c, agentworkspace.ErrConversationRunning)
			return
		}
		arguments, _ := json.Marshal(map[string]string{"path": path})
		if _, errDelete := store.ExecuteTool(c.Request.Context(), id, agentworkspace.ToolCall{Name: "delete_file", Arguments: string(arguments)}); errDelete != nil {
			writeAgentError(c, errDelete)
			return
		}
		c.Status(http.StatusNoContent)
	default:
		if path == "" {
			files, errFiles := store.ListFiles(id)
			if errFiles != nil {
				writeAgentError(c, errFiles)
				return
			}
			c.JSON(http.StatusOK, gin.H{"files": files})
			return
		}
		file, errOpen := store.OpenFile(id, path)
		if errOpen != nil {
			writeAgentError(c, errOpen)
			return
		}
		defer func() {
			if errClose := file.Close(); errClose != nil {
				log.WithError(errClose).Warn("failed to close agent artifact")
			}
		}()
		info, errStat := file.Stat()
		if errStat != nil {
			writeAgentError(c, errStat)
			return
		}
		c.Header("Content-Type", "application/octet-stream")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")
		c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
		http.ServeContent(c.Writer, c.Request, filepath.Base(path), info.ModTime(), file)
	}
}

func containsAgentEffort(levels []string, effort string) bool {
	for _, level := range levels {
		if level == effort {
			return true
		}
	}
	return false
}

func (s *Server) validateAgentModel(c *gin.Context, id, effort string) bool {
	for _, model := range s.agentModels() {
		if model.ID != id {
			continue
		}
		if effort != "" && !containsAgentEffort(model.ReasoningEfforts, effort) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "reasoning effort is not supported by this model"})
			return false
		}
		return true
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "model is not available"})
	return false
}

func bindAgentJSON(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, agentJSONLimit)
	if errBind := c.ShouldBindJSON(target); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent request body"})
		return false
	}
	return true
}

func writeAgentError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, agentworkspace.ErrConversationNotFound), errors.Is(err, fs.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, agentworkspace.ErrConversationRunning):
		status = http.StatusConflict
	case errors.Is(err, agentworkspace.ErrInvalidWorkspacePath), errors.Is(err, agentworkspace.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, agentworkspace.ErrStoreClosed):
		status = http.StatusServiceUnavailable
	}
	if status == http.StatusInternalServerError {
		log.WithError(err).Error("agent workspace request failed")
		c.JSON(status, gin.H{"error": "Agent workspace operation failed"})
		return
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

func (s *Server) agentComplete(ctx context.Context, request agentworkspace.ModelRequest, emit func(agentworkspace.ModelDelta)) (agentworkspace.ModelResponse, error) {
	messages := make([]map[string]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		entry := map[string]any{"role": message.Role, "content": message.Content}
		if message.Role == "tool" {
			entry["tool_call_id"] = message.ToolCallID
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				calls = append(calls, map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.Arguments}})
			}
			entry["tool_calls"] = calls
		}
		messages = append(messages, entry)
	}
	tools := make([]map[string]any, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters}})
	}
	payload := map[string]any{"model": request.Model, "messages": messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}, "tools": tools}
	if request.ToolChoice != "" {
		payload["tool_choice"] = request.ToolChoice
	}
	if request.ReasoningEffort != "" {
		payload["reasoning_effort"] = request.ReasoningEffort
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return agentworkspace.ModelResponse{}, errMarshal
	}
	headers := make(http.Header)
	headers.Set("X-Session-Id", request.ConversationID)
	headers.Set("User-Agent", "CPA-Agent-Workspace")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, errStream := s.handlers.ExecuteProtocolStreamWithAuthManager(runCtx, handlers.ProtocolExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: request.Model, Stream: true, Body: body, Headers: headers})
	if errStream != nil {
		return agentworkspace.ModelResponse{}, errStream.Error
	}
	accumulator := agentStreamAccumulator{calls: make(map[int]*agentworkspace.ToolCall)}
	for {
		select {
		case <-runCtx.Done():
			return accumulator.response(), runCtx.Err()
		case chunk, ok := <-stream.Chunks:
			if !ok {
				if !accumulator.finished {
					return accumulator.response(), fmt.Errorf("model stream ended before completion")
				}
				return accumulator.response(), nil
			}
			if chunk.Err != nil {
				return accumulator.response(), chunk.Err
			}
			if errAdd := accumulator.add(chunk.Payload, emit); errAdd != nil {
				return accumulator.response(), errAdd
			}
		}
	}
}

type agentStreamAccumulator struct {
	content  strings.Builder
	calls    map[int]*agentworkspace.ToolCall
	finished bool
}

func (a *agentStreamAccumulator) add(raw []byte, emit func(agentworkspace.ModelDelta)) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if bytes.HasPrefix(raw, []byte("data:")) {
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				if errAdd := a.add(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), emit); errAdd != nil {
					return errAdd
				}
			}
		}
		return nil
	}
	if bytes.Equal(raw, []byte("[DONE]")) {
		a.finished = true
		return nil
	}
	var chunk struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if errDecode := json.Unmarshal(raw, &chunk); errDecode != nil {
		return fmt.Errorf("invalid agent model stream: %w", errDecode)
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		return fmt.Errorf("model returned an error: %s", chunk.Error)
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		if choice.Delta.Content != "" {
			a.content.WriteString(choice.Delta.Content)
			emit(agentworkspace.ModelDelta{Content: choice.Delta.Content})
		}
		if a.content.Len() > agentUploadLimit {
			return fmt.Errorf("model response exceeds size limit")
		}
		for _, delta := range choice.Delta.ToolCalls {
			call := a.calls[delta.Index]
			if call == nil {
				call = &agentworkspace.ToolCall{}
				a.calls[delta.Index] = call
			}
			if delta.ID != "" {
				call.ID = delta.ID
			}
			call.Name += delta.Function.Name
			call.Arguments += delta.Function.Arguments
			if len(call.Arguments) > agentJSONLimit {
				return fmt.Errorf("tool arguments exceed size limit")
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			a.finished = true
		}
	}
	return nil
}

func (a *agentStreamAccumulator) response() agentworkspace.ModelResponse {
	indexes := make([]int, 0, len(a.calls))
	for index := range a.calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	result := agentworkspace.ModelResponse{Content: a.content.String()}
	for _, index := range indexes {
		result.ToolCalls = append(result.ToolCalls, *a.calls[index])
	}
	return result
}
