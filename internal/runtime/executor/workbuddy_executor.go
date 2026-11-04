package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// WorkBuddyExecutor serves both regional plans using their stream-only gateway.
type WorkBuddyExecutor struct {
	cfg                           *config.Config
	provider                      string
	fingerprintConvergenceEnabled bool
}

func NewWorkBuddyExecutor(cfg *config.Config, provider string) *WorkBuddyExecutor {
	return &WorkBuddyExecutor{
		cfg: cfg, provider: provider,
		fingerprintConvergenceEnabled: cfg != nil && cfg.Routing.WorkBuddyFingerprintConvergence != nil && *cfg.Routing.WorkBuddyFingerprintConvergence,
	}
}

func (e *WorkBuddyExecutor) Identifier() string { return e.provider }

func (e *WorkBuddyExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

func (e *WorkBuddyExecutor) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if auth.Provider != "" && auth.Provider != e.provider {
		return statusErr{code: http.StatusUnauthorized, msg: "workbuddy: credential belongs to another site"}
	}
	creds := workbuddy.CredentialsFromAuth(auth)
	if creds.AccessToken == "" || creds.UID == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "workbuddy: missing account credentials; sign in again"}
	}
	workbuddy.SignRequest(req, e.provider, creds)
	return nil
}

func (e *WorkBuddyExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	req = req.Clone(ctx)
	if err := e.PrepareRequest(req, auth); err != nil {
		return nil, err
	}
	return helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(req)
}

func (e *WorkBuddyExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth.Provider != e.provider {
		return nil, fmt.Errorf("workbuddy: credential belongs to another site")
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 30*time.Second)
	service := workbuddy.NewService(e.provider, client)
	service.SetEndpoint(workbuddy.EndpointForAuth(auth))
	creds, err := service.Refresh(ctx, workbuddy.CredentialsFromAuth(auth))
	if err != nil {
		return nil, err
	}
	updated := auth.Clone()
	workbuddy.ApplyCredentials(updated, creds)
	updated.LastRefreshedAt = time.Now()
	return updated, nil
}

func (e *WorkBuddyExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return NewOpenAICompatExecutor(e.provider, e.cfg).CountTokens(ctx, auth, req, opts)
}

func (e *WorkBuddyExecutor) open(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, reporter *helps.UsageReporter) (*http.Response, []byte, map[string]string, error) {
	if opts.Alt == "responses/compact" {
		return nil, nil, nil, statusErr{code: http.StatusNotImplemented, msg: "workbuddy: compact responses are not supported"}
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	to := sdktranslator.FormatOpenAI
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, opts.SourceFormat, to, model, original, true)
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, opts.SourceFormat, to, model, req.Payload, true)
	body, err := helps.ApplyRequestThinking(body, req, opts, opts.SourceFormat.String(), to.String(), e.provider)
	if err != nil {
		return nil, nil, nil, err
	}
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, model, to.String(), opts.SourceFormat.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, nil, nil, err
	}
	body, names, err := helps.NormalizeWorkBuddyRequest(body)
	if err != nil {
		return nil, nil, nil, err
	}
	reporter.SetTranslatedReasoningEffort(body, to.String())
	endpoint := workbuddy.EndpointForAuth(auth) + "/v2/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, err
	}
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if e.fingerprintConvergenceEnabled {
		helps.ApplyWorkBuddyFingerprintHeaders(httpReq, auth, body, opts)
	} else {
		helps.ApplyWorkBuddyConversationHeaders(httpReq, body, opts)
	}
	authType, authValue := auth.AccountInfo()
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: endpoint, Method: http.MethodPost, Headers: httpReq.Header.Clone(), Body: body,
		Provider: e.provider, AuthID: auth.ID, AuthLabel: auth.Label, AuthType: authType, AuthValue: authValue})
	client := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	response, err := client.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, response.Header.Clone())
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		defer e.closeResponse(response)
		payload, errRead := io.ReadAll(response.Body)
		if errRead != nil {
			return nil, nil, nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, payload)
		return nil, nil, nil, e.upstreamStatusError(response.StatusCode, response.Header, payload)
	}
	return response, body, names, nil
}

func (e *WorkBuddyExecutor) upstreamStatusError(status int, headers http.Header, payload []byte) statusErr {
	if helps.IsWorkBuddyAccountBanned(payload) {
		err := newOpenAICompatStatusError(http.StatusForbidden, headers, payload)
		err.credentialScoped = true
		err.resultCode = coreauth.ErrorCodeWorkBuddyAccountBanned
		return err
	}
	if helps.IsWorkBuddyQuotaExhausted(payload) {
		err := newOpenAICompatStatusError(http.StatusTooManyRequests, headers, payload)
		err.credentialScoped = true
		err.resultCode = "usage_limit_reached"
		return err
	}
	if helps.IsWorkBuddyModelRateLimit(payload) {
		err := newOpenAICompatStatusError(http.StatusTooManyRequests, headers, payload)
		err.resultCode = coreauth.ErrorCodeWorkBuddyModelRateLimit
		if retryAfter := helps.WorkBuddyRateLimitRetryAfter(payload, time.Now()); retryAfter != nil {
			err.retryAfter = retryAfter
		}
		return err
	}
	if helps.IsWorkBuddyRateLimit(payload) {
		err := newOpenAICompatStatusError(http.StatusTooManyRequests, headers, payload)
		err.credentialScoped = true
		err.resultCode = coreauth.ErrorCodeWorkBuddyRateLimit
		if retryAfter := helps.WorkBuddyRateLimitRetryAfter(payload, time.Now()); retryAfter != nil {
			err.retryAfter = retryAfter
		}
		return err
	}
	if helps.IsWorkBuddyQueueWaiting(payload) {
		// Admission-queue backpressure: the credential stays usable, so keep the
		// 429 request-scoped and honour the queue's own retry delay.
		err := newOpenAICompatStatusError(http.StatusTooManyRequests, headers, payload)
		if retryAfter := helps.WorkBuddyQueueWaitingRetryAfter(payload, time.Now()); retryAfter != nil {
			err.retryAfter = retryAfter
		}
		return err
	}
	if status >= 200 && status < 300 {
		status = http.StatusBadGateway
	}
	return newOpenAICompatStatusError(status, headers, payload)
}

func (e *WorkBuddyExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (result cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	reporter := helps.NewExecutorUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	defer reporter.TrackFailure(ctx, &err)
	response, body, names, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		return result, err
	}
	defer e.closeResponse(response)
	var aggregate helps.WorkBuddyAggregate
	err = e.readStream(ctx, response.Body, response.Header, req.Model, names, func(payload []byte, done bool) error {
		if done {
			return nil
		}
		return aggregate.Add(payload)
	})
	if err != nil {
		return result, err
	}
	payload, err := aggregate.Response(req.Model)
	if err != nil {
		return result, err
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(payload))
	reporter.EnsurePublished(ctx)
	format := cliproxyexecutor.ResponseFormatOrSource(opts)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, format, req.Model, opts.OriginalRequest, body, payload, &param)
	if format == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	headers := response.Header.Clone()
	headers.Set("Content-Type", "application/json")
	headers.Del("Content-Length")
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *WorkBuddyExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	reporter := helps.NewExecutorUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	defer reporter.TrackFailure(ctx, &err)
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	original, err = sjson.SetBytes(original, "stream", true)
	if err != nil {
		return nil, err
	}
	response, body, names, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer e.closeResponse(response)
		var param any
		var usage helps.StreamUsageBuffer
		defer usage.Publish(ctx, reporter)
		format := cliproxyexecutor.ResponseFormatOrSource(opts)
		claudeTokens := helps.NewClaudeInputTokenState(opts.SourceFormat, sdktranslator.FormatOpenAI, format, original)
		errRead := e.readStream(ctx, response.Body, response.Header, req.Model, names, func(payload []byte, done bool) error {
			line := append([]byte("data: "), payload...)
			if !done {
				usage.ObserveOpenAIStream(line)
			}
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, sdktranslator.FormatOpenAI, format, req.Model, original, body, line, &param, claudeTokens)
			for _, chunk := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		if errRead != nil {
			reporter.PublishFailure(ctx, errRead)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
			case <-ctx.Done():
			}
			return
		}
		usage.Publish(ctx, reporter)
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: response.Header.Clone(), Chunks: out}, nil
}

func (e *WorkBuddyExecutor) readStream(ctx context.Context, body io.Reader, headers http.Header, model string, names map[string]string, consume func([]byte, bool) error) error {
	const maxSSEFrameSize = 50 << 20
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, maxSSEFrameSize)
	var data [][]byte
	var event string
	var done bool
	process := func() error {
		payload := bytes.TrimSpace(bytes.Join(data, []byte("\n")))
		data = nil
		currentEvent := event
		event = ""
		if len(payload) == 0 {
			if currentEvent == "error" {
				return statusErr{code: http.StatusBadGateway, msg: "workbuddy: empty upstream error event"}
			}
			return nil
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			done = true
			return consume(payload, true)
		}
		if helps.IsWorkBuddyAccountBanned(payload) || helps.IsWorkBuddyQuotaExhausted(payload) || helps.IsWorkBuddyModelRateLimit(payload) || helps.IsWorkBuddyRateLimit(payload) {
			return e.upstreamStatusError(http.StatusTooManyRequests, headers, payload)
		}
		if upstreamErr, failed := openAICompatStreamDataError(payload, currentEvent); failed {
			return upstreamErr
		}
		normalized, err := helps.NormalizeWorkBuddyChunk(payload, model, names)
		if err != nil {
			return statusErr{code: http.StatusBadGateway, msg: "workbuddy: malformed upstream stream"}
		}
		for _, frame := range helps.SplitWorkBuddyFinishChunks(normalized) {
			if err := consume(frame, false); err != nil {
				return err
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		helps.AppendAPIResponseChunk(ctx, e.cfg, line)
		if len(line) == 0 {
			if err := process(); err != nil {
				return err
			}
			if done {
				return nil
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.Clone(bytes.TrimSpace(line[len("data:"):])))
		} else if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[len("event:"):]))
		} else if bytes.HasPrefix(line, []byte("{")) {
			return e.upstreamStatusError(http.StatusBadGateway, headers, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(data) > 0 {
		if err := process(); err != nil {
			return err
		}
	}
	if !done {
		return statusErr{code: http.StatusBadGateway, msg: "workbuddy: upstream stream closed before [DONE]"}
	}
	return nil
}

func (e *WorkBuddyExecutor) closeResponse(response *http.Response) {
	if err := response.Body.Close(); err != nil {
		log.WithError(err).Debug("workbuddy: close upstream response")
	}
}
