package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type PrismExecutor struct {
	cfg     *config.Config
	mu      sync.Mutex
	clients map[string]*helps.PrismClient
	history map[string][]byte
}

func NewPrismExecutor(cfg *config.Config) *PrismExecutor {
	return &PrismExecutor{cfg: cfg.CloneForRuntime(), clients: make(map[string]*helps.PrismClient), history: make(map[string][]byte)}
}

func (e *PrismExecutor) Identifier() string { return "prism" }

func prismRequestSelected(opts cliproxyexecutor.Options) bool {
	enabled, _ := opts.Metadata[cliproxyexecutor.PrismRouteMetadataKey].(bool)
	return enabled
}

func prismCallerScope(opts cliproxyexecutor.Options) string {
	scope, _ := opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey].(string)
	return scope
}

func prismHistoryScope(opts cliproxyexecutor.Options) string {
	session, _ := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
	return prismCallerScope(opts) + "\x00" + session
}

func (e *PrismExecutor) client(ctx context.Context, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) *helps.PrismClient {
	session, _ := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
	if session == "" {
		session, _ = opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string)
	}
	if session == "" {
		session = uuid.NewString()
	}
	digest := sha256.Sum256([]byte(auth.ID + "\x00" + prismCallerScope(opts) + "\x00" + session))
	key := hex.EncodeToString(digest[:])
	e.mu.Lock()
	defer e.mu.Unlock()
	if client := e.clients[key]; client != nil {
		return client
	}
	const maxClients = 256
	if len(e.clients) >= maxClients {
		for oldest := range e.clients {
			delete(e.clients, oldest)
			break
		}
	}
	baseURL := helps.PrismDefaultBaseURL
	if e.cfg != nil && e.cfg.Codex.PrismBaseURL != "" {
		baseURL = e.cfg.Codex.PrismBaseURL
	}
	client := helps.NewPrismClient(helps.NewCodexHTTPClient(ctx, e.cfg, auth, 0), baseURL, "CLIProxyAPI Prism "+key)
	e.clients[key] = client
	return client
}

func (e *PrismExecutor) prepare(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]byte, string, error) {
	if opts.Alt != "" {
		return nil, "", statusErr{code: http.StatusBadRequest, msg: "Prism does not support this alternate endpoint"}
	}
	suffix := thinking.ParseSuffix(req.Model)
	model := suffix.ModelName
	from, to := opts.SourceFormat, sdktranslator.FormatCodex
	body := sdktranslator.TranslateRequest(from, to, model, req.Payload, false)
	// Keep canonical thinking extraction; only this transport applies the Prism capability mapping.
	modelInfo := &registry.ModelInfo{ID: model, UserDefined: true}
	var err error
	body, err = thinking.ApplyThinkingWithModelInfo(body, req.Payload, req.Model, from.String(), to.String(), "prism", modelInfo)
	if err != nil {
		return nil, "", err
	}
	effort, errEffort := helps.PrismReasoningEffort(gjson.GetBytes(body, "reasoning.effort").String())
	if errEffort != nil {
		return nil, "", errEffort
	}
	if !suffix.HasSuffix {
		path := "reasoning.effort"
		switch from.String() {
		case "claude":
			path = "output_config.effort"
		case "openai":
			path = "reasoning_effort"
		}
		explicit := gjson.GetBytes(req.Payload, path)
		if !explicit.Exists() {
			explicit = gjson.GetBytes(opts.OriginalRequest, path)
		}
		if explicit.String() == "max" {
			effort = "xhigh"
		}
	}
	body, err = sjson.SetBytes(body, "reasoning.effort", effort)
	if err != nil {
		return nil, "", err
	}
	if input := gjson.GetBytes(body, "input"); input.Type == gjson.String {
		body, err = sjson.SetBytes(body, "input", []map[string]any{{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": input.String()}}}})
		if err != nil {
			return nil, "", err
		}
	}
	if previous := gjson.GetBytes(body, "previous_response_id").String(); previous != "" {
		key := prismHistoryScope(opts) + "\x00" + previous
		e.mu.Lock()
		history, exists := e.history[key]
		e.mu.Unlock()
		if !exists {
			return nil, "", statusErr{code: http.StatusBadRequest, msg: "Prism previous_response_id is unavailable for this caller; resend complete conversation history"}
		}
		var items []json.RawMessage
		if err = json.Unmarshal(history, &items); err != nil {
			return nil, "", err
		}
		for _, item := range gjson.GetBytes(body, "input").Array() {
			items = append(items, json.RawMessage(item.Raw))
		}
		body, err = sjson.SetBytes(body, "input", items)
		if err != nil {
			return nil, "", err
		}
	}
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	return body, effort, nil
}

func (e *PrismExecutor) remember(opts cliproxyexecutor.Options, body, response []byte) {
	var items []json.RawMessage
	for _, item := range gjson.GetBytes(body, "input").Array() {
		items = append(items, json.RawMessage(item.Raw))
	}
	for _, item := range gjson.GetBytes(response, "output").Array() {
		items = append(items, json.RawMessage(item.Raw))
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	const maxResponses = 256
	if len(e.history) >= maxResponses {
		for key := range e.history {
			delete(e.history, key)
			break
		}
	}
	e.history[prismHistoryScope(opts)+"\x00"+gjson.GetBytes(response, "id").String()] = encoded
}

func (e *PrismExecutor) start(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*helps.PrismRun, []byte, error) {
	body, effort, err := e.prepare(req, opts)
	if err != nil {
		return nil, nil, err
	}
	input, errInput := helps.BuildPrismInput(body)
	if errInput != nil {
		return nil, nil, errInput
	}
	token, _ := auth.Metadata["access_token"].(string)
	if token == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "Prism requires an OpenAI access token"}
	}
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	run, errStart := e.client(ctx, auth, opts).Start(ctx, token, thinking.ParseSuffix(req.Model).ModelName, effort, input)
	return run, body, errStart
}

func (e *PrismExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	model := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	run, body, err := e.start(ctx, auth, req, opts)
	if err != nil {
		return resp, err
	}
	defer stopCancelledPrismRun(ctx, run)
	payload, errWait := run.Wait(ctx)
	if errWait != nil {
		return resp, errWait
	}
	response, errResponse := helps.PrismResponse(payload, body, model, "resp_"+uuid.NewString())
	if errResponse != nil {
		return resp, errResponse
	}
	e.remember(opts, body, response)
	reporter.SetTranslatedReasoningEffort(body, "codex")
	reporter.EnsurePublished(ctx)
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": json.RawMessage(response)})
	var param any
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	resp.Payload = sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, original, body, completed, &param)
	resp.Headers = http.Header{"Content-Type": {"application/json"}}
	return resp, nil
}

func (e *PrismExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	model := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	run, body, err := e.start(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	responseID := "resp_" + uuid.NewString()
	output := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(output)
		defer stopCancelledPrismRun(ctx, run)
		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			select {
			case <-ctx.Done():
				return false
			case output <- chunk:
				return true
			}
		}
		var param any
		original := opts.OriginalRequest
		if len(original) == 0 {
			original = req.Payload
		}
		target := cliproxyexecutor.ResponseFormatOrSource(opts)
		translate := func(event []byte) bool {
			for _, chunk := range sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, target, req.Model, original, body, event, &param) {
				if !send(cliproxyexecutor.StreamChunk{Payload: chunk}) {
					return false
				}
			}
			return true
		}
		for i, eventType := range []string{"response.created", "response.in_progress"} {
			event, _ := json.Marshal(map[string]any{"type": eventType, "sequence_number": i, "response": map[string]any{"id": responseID, "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "in_progress", "output": []any{}}})
			if !translate(append(append([]byte("data: "), event...), []byte("\n\n")...)) {
				return
			}
		}
		payload, errWait := run.Wait(ctx)
		if errWait != nil {
			reporter.PublishFailure(ctx, errWait)
			send(cliproxyexecutor.StreamChunk{Err: errWait})
			return
		}
		response, errResponse := helps.PrismResponse(payload, body, model, responseID)
		if errResponse != nil {
			reporter.PublishFailure(ctx, errResponse)
			send(cliproxyexecutor.StreamChunk{Err: errResponse})
			return
		}
		e.remember(opts, body, response)
		events, errEvents := helps.PrismResponseEvents(response)
		if errEvents != nil {
			send(cliproxyexecutor.StreamChunk{Err: fmt.Errorf("Prism event conversion: %w", errEvents)})
			return
		}
		reporter.SetTranslatedReasoningEffort(body, "codex")
		reporter.EnsurePublished(ctx)
		for _, event := range events {
			if !translate(event) {
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"Content-Type": {"text/event-stream"}, "X-Prism-Transport": {"polling"}}, Chunks: output}, nil
}

func stopCancelledPrismRun(ctx context.Context, run *helps.PrismRun) {
	if ctx.Err() != nil {
		go func() { _ = run.Stop(context.WithoutCancel(ctx)) }()
	}
}
