package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// QoderExecutor implements both regional Cosy gateways without shared credentials.
type QoderExecutor struct {
	cfg      *config.Config
	provider string
}

var _ coreauth.ProviderExecutor = (*QoderExecutor)(nil)

func NewQoderExecutor(cfg *config.Config, provider string) *QoderExecutor {
	return &QoderExecutor{cfg: cfg, provider: provider}
}
func (e *QoderExecutor) Identifier() string { return e.provider }
func (e *QoderExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

func (e *QoderExecutor) validateAuth(auth *coreauth.Auth) error {
	if auth == nil || (e.provider != qoder.ProviderIntl && e.provider != qoder.ProviderCN) || auth.Provider != e.provider {
		return statusErr{code: 401, msg: "qoder: credential belongs to another provider"}
	}
	creds := qoder.CredentialsFromAuth(auth)
	wantRealm := "intl"
	if e.provider == qoder.ProviderCN {
		wantRealm = "cn"
	}
	if creds.Realm != "" && creds.Realm != wantRealm {
		return statusErr{code: 401, msg: "qoder: credential region does not match provider"}
	}
	if creds.UID == "" || creds.AccessToken == "" {
		return statusErr{code: 401, msg: "qoder: missing account credentials"}
	}
	return nil
}

// PrepareRequest is for ordinary account OpenAPI calls only. Cosy signing needs
// the immutable encoded body and is dispatched explicitly by HttpRequest/open.
func (e *QoderExecutor) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if err := e.validateAuth(auth); err != nil {
		return err
	}
	if req == nil || req.URL == nil || !e.endpointMatches(req.URL, qoder.EndpointForAuth(auth)) || strings.HasPrefix(req.URL.Path, "/algo/") {
		return statusErr{code: 400, msg: "qoder: ordinary API request must target the account OpenAPI endpoint"}
	}
	creds := qoder.CredentialsFromAuth(auth)
	req.Header = helps.QoderSafeHeaders(req.Header)
	qoder.SignAPIRequest(req, e.provider, creds)
	return nil
}

func (e *QoderExecutor) endpointMatches(target *url.URL, endpoint string) bool {
	base, err := url.Parse(endpoint)
	return err == nil && target != nil && target.User == nil && strings.EqualFold(target.Scheme, base.Scheme) && strings.EqualFold(target.Host, base.Host) && (base.Path == "" || base.Path == "/" || target.Path == strings.TrimRight(base.Path, "/") || strings.HasPrefix(target.Path, strings.TrimRight(base.Path, "/")+"/"))
}

func (e *QoderExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if err := e.validateAuth(auth); err != nil {
		return nil, err
	}
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("qoder: missing HTTP request")
	}
	clone := req.Clone(ctx)
	modelRequest := clone.Method == http.MethodGet && clone.URL.Path == "/algo/api/v2/model/list"
	inferenceRequest := clone.Method == http.MethodPost && clone.URL.Path == strings.SplitN(helps.QoderChatPath, "?", 2)[0]
	if modelRequest || inferenceRequest {
		if !e.endpointMatches(clone.URL, qoder.GatewayForAuth(auth)) {
			return nil, statusErr{code: 400, msg: "qoder: Cosy request must target the account gateway"}
		}
		var body []byte
		if clone.Body != nil {
			if req.GetBody == nil {
				return nil, statusErr{code: 400, msg: "qoder: Cosy HttpRequest requires a replayable, already encoded body"}
			}
			copyBody, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			body, err = io.ReadAll(copyBody)
			errClose := copyBody.Close()
			if err != nil {
				return nil, err
			}
			if errClose != nil {
				return nil, errClose
			}
		}
		if modelRequest && len(body) == 0 {
			body = helps.EncodeQoderBody([]byte("{}"))
		}
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
		clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		if err := helps.SignQoderRequest(clone, auth, body, clone.Header.Get("X-Model-Key"), inferenceRequest); err != nil {
			return nil, err
		}
	} else if err := e.PrepareRequest(clone, auth); err != nil {
		return nil, err
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var response *http.Response
	var err error
	if modelRequest || inferenceRequest {
		response, err = helps.DoQoderGatewayRequest(client, clone, auth)
	} else {
		response, err = client.Do(clone)
	}
	if response != nil {
		response.Header = helps.QoderSafeHeaders(response.Header)
	}
	return response, err
}

func (e *QoderExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth == nil || auth.Provider != e.provider {
		return nil, fmt.Errorf("qoder: credential belongs to another provider")
	}
	creds := qoder.CredentialsFromAuth(auth)
	wantRealm := "intl"
	if e.provider == qoder.ProviderCN {
		wantRealm = "cn"
	}
	if creds.Realm != "" && creds.Realm != wantRealm {
		return nil, fmt.Errorf("qoder: credential region does not match provider")
	}
	service := qoder.NewService(e.provider, helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 30*time.Second))
	service.SetEndpoint(qoder.EndpointForAuth(auth))
	creds, err := service.Refresh(ctx, creds)
	if err != nil {
		return nil, err
	}
	updated := auth.Clone()
	qoder.ApplyCredentials(updated, creds)
	updated.LastRefreshedAt = time.Now()
	return updated, nil
}

func (e *QoderExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return NewOpenAICompatExecutor(e.provider, e.cfg).CountTokens(ctx, auth, req, opts)
}

func (e *QoderExecutor) open(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, reporter *helps.UsageReporter) (*http.Response, []byte, error) {
	if err := e.validateAuth(auth); err != nil {
		return nil, nil, err
	}
	if opts.Alt == "responses/compact" {
		return nil, nil, statusErr{code: 501, msg: "qoder: compact responses are not supported"}
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	to := sdktranslator.FormatOpenAI
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, opts.SourceFormat, to, model, original, true)
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, opts.SourceFormat, to, model, req.Payload, true)
	body, err := helps.ApplyRequestThinking(body, req, opts, opts.SourceFormat.String(), to.String(), e.provider)
	if err != nil {
		return nil, nil, err
	}
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, model, to.String(), opts.SourceFormat.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, nil, err
	}
	reporter.SetTranslatedReasoningEffort(body, to.String())
	modelKey := helps.ResolveQoderModel(auth, model)
	wire, err := helps.BuildQoderBody(body, auth, modelKey)
	if err != nil {
		return nil, nil, err
	}
	encoded := helps.EncodeQoderBody(wire)
	if len(encoded) > 29<<20 {
		return nil, nil, coreauth.NewRequestScopedError("qoder: encoded request exceeds gateway body limit", 413)
	}
	endpoint := strings.TrimRight(qoder.GatewayForAuth(auth), "/") + helps.QoderChatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, err
	}
	if err = helps.SignQoderRequest(httpReq, auth, encoded, modelKey, true); err != nil {
		return nil, nil, err
	}
	authType, authValue := auth.AccountInfo()
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: endpoint, Method: http.MethodPost, Headers: helps.QoderSafeHeaders(httpReq.Header), Body: wire, Provider: e.provider, AuthID: auth.ID, AuthLabel: auth.Label, AuthType: authType, AuthValue: authValue})
	client := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := helps.DoQoderGatewayRequest(client, httpReq, auth)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, helps.QoderSafeHeaders(response.Header))
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		var closeOnce sync.Once
		closeBody := func() { closeOnce.Do(func() { e.closeResponse(response) }) }
		defer closeBody()
		stop := context.AfterFunc(ctx, closeBody)
		defer stop()
		payload, errRead := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if errRead != nil {
			return nil, nil, errRead
		}
		upstreamErr := helps.QoderUpstreamError(response.StatusCode, response.Header, payload)
		helps.RecordAPIResponseError(ctx, e.cfg, upstreamErr)
		return nil, nil, upstreamErr
	}
	return response, body, nil
}

func (e *QoderExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (result cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	reporter := helps.NewExecutorUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	defer reporter.TrackFailure(ctx, &err)
	response, body, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		return result, err
	}
	var aggregate helps.WorkBuddyAggregate
	var usage helps.StreamUsageBuffer
	err = helps.ReadQoderStream(ctx, response.Body, req.Model, response.Header, helps.QoderToolEchoConsumer(body, func(payload []byte, done bool) error {
		if done {
			return nil
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, payload)
		usage.ObserveOpenAIStream(payload)
		return aggregate.Add(payload)
	}))
	if err != nil {
		usage.PublishFailure(ctx, reporter, err)
		return result, err
	}
	payload, err := aggregate.Response(req.Model)
	if err != nil {
		return result, err
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(payload))
	reporter.EnsurePublished(ctx)
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	format := cliproxyexecutor.ResponseFormatOrSource(opts)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, format, req.Model, original, body, payload, &param)
	if format == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	headers := helps.QoderSafeHeaders(response.Header)
	headers.Set("Content-Type", "application/json")
	headers.Del("Content-Length")
	headers.Del("Content-Encoding")
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *QoderExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
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
	response, body, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var param any
		var usage helps.StreamUsageBuffer
		defer usage.Publish(ctx, reporter)
		format := cliproxyexecutor.ResponseFormatOrSource(opts)
		claudeTokens := helps.NewClaudeInputTokenState(opts.SourceFormat, sdktranslator.FormatOpenAI, format, original)
		errRead := helps.ReadQoderStream(ctx, response.Body, req.Model, response.Header, helps.QoderToolEchoConsumer(body, func(payload []byte, done bool) error {
			line := append([]byte("data: "), payload...)
			if !done {
				usage.ObserveOpenAIStream(line)
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
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
		}))
		if errRead != nil {
			usage.PublishFailure(ctx, reporter, errRead)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
			case <-ctx.Done():
			}
			return
		}
		usage.Publish(ctx, reporter)
		reporter.EnsurePublished(ctx)
	}()
	headers := helps.QoderSafeHeaders(response.Header)
	headers.Set("Content-Type", "text/event-stream")
	headers.Del("Content-Length")
	headers.Del("Content-Encoding")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func (e *QoderExecutor) closeResponse(response *http.Response) {
	if err := response.Body.Close(); err != nil {
		log.WithError(err).Debug("qoder: close upstream response")
	}
}
