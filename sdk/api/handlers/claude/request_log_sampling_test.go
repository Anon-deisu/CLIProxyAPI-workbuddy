package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type samplingClaudeExecutor struct {
	preFTKeepAliveClaudeExecutor
	payloads    []string
	trailingErr error
	cancel      bool
	flushed     <-chan struct{}
	handlerDone <-chan struct{}
	done        chan struct{}
}

func (e *samplingClaudeExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(e.done)
		defer close(chunks)
		for _, payload := range e.payloads {
			select {
			case chunks <- coreexecutor.StreamChunk{Payload: []byte(payload)}:
			case <-ctx.Done():
				return
			}
			select {
			case <-e.flushed:
			case <-ctx.Done():
				if e.cancel {
					<-e.handlerDone
				}
				return
			}
		}
		if e.cancel {
			<-ctx.Done()
			<-e.handlerDone
			return
		}
		if e.trailingErr != nil {
			select {
			case chunks <- coreexecutor.StreamChunk{Err: e.trailingErr}:
			case <-ctx.Done():
			}
		}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

type samplingClaudeRecorder struct {
	*httptest.ResponseRecorder
	flushed chan<- struct{}
	cancel  context.CancelFunc
	once    sync.Once
}

func (r *samplingClaudeRecorder) Flush() {
	r.ResponseRecorder.Flush()
	if r.cancel != nil {
		r.once.Do(r.cancel)
	}
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

func TestRequestLogSamplingClaudeErrorFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const start = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"sampling\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n"
	const failure = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"sampling Claude failure\"}}\n\n"
	const stop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name        string
		payloads    []string
		trailingErr bool
		cancel      bool
		wantError   bool
	}{
		{name: "first-frame", payloads: []string{failure}, wantError: true},
		{name: "later-frame", payloads: []string{start, failure}, wantError: true},
		{name: "first-frame-and-err", payloads: []string{failure}, trailingErr: true, wantError: true},
		{name: "later-frame-and-err", payloads: []string{start, failure}, trailingErr: true, wantError: true},
		{name: "named-frame", payloads: []string{start, strings.Replace(failure, `"type":"error",`, "", 1)}, wantError: true},
		{name: "unterminated-frame", payloads: []string{start, strings.TrimRight(failure, "\n")}, wantError: true},
		{name: "unterminated-frame-and-err", payloads: []string{start, strings.TrimRight(failure, "\n")}, trailingErr: true, wantError: true},
		{name: "split-frame", payloads: []string{start, failure[:20], failure[20:]}, wantError: true},
		{name: "success", payloads: []string{start, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"type:error\"}}\n\n", stop}},
		{name: "cancel", payloads: []string{start}, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flushed, handlerDone := make(chan struct{}, len(tc.payloads)+2), make(chan struct{})
			exec := &samplingClaudeExecutor{payloads: tc.payloads, cancel: tc.cancel, flushed: flushed, handlerDone: handlerDone, done: make(chan struct{})}
			if tc.trailingErr {
				exec.trailingErr = errors.New("sampling Claude failure")
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(exec)
			authID, model := "sampling-claude-"+tc.name, "sampling-claude-model"
			if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: exec.Identifier(), Status: coreauth.StatusActive}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, exec.Identifier(), []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
			h := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, manager))
			dir := t.TempDir()
			router := gin.New()
			router.Use(middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(true, dir, "", 0), middleware.WithRequestLogSampling(func() float64 { return 0 }, nil)))
			var loggedErrors []*interfaces.ErrorMessage
			router.POST("/v1/messages", func(c *gin.Context) {
				defer close(handlerDone)
				h.ClaudeMessages(c)
				value, _ := c.Get("API_RESPONSE_ERROR")
				loggedErrors, _ = value.([]*interfaces.ErrorMessage)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recorder := &samplingClaudeRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: flushed}
			if tc.cancel {
				recorder.cancel = cancel
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"sampling-claude-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			<-exec.done
			if recorder.Code != http.StatusOK || !recorder.Flushed {
				t.Fatalf("expected committed HTTP 200 SSE, got %d: %s", recorder.Code, recorder.Body.String())
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.wantError {
				want = 1
			}
			if got := strings.Count(recorder.Body.String(), "event: error\n"); got != want {
				t.Errorf("downstream error frames = %d, want %d: %s", got, want, recorder.Body.String())
			}
			if len(loggedErrors) != want {
				t.Errorf("structured errors = %d, want %d", len(loggedErrors), want)
			}
			if len(entries) != want {
				t.Fatalf("log files = %d, want %d", len(entries), want)
			}
			if tc.wantError {
				if entries[0].IsDir() || !strings.HasSuffix(entries[0].Name(), ".log") {
					t.Fatalf("leaked temporary source: %s", entries[0].Name())
				}
				body, errRead := os.ReadFile(filepath.Join(dir, entries[0].Name()))
				if errRead != nil {
					t.Fatal(errRead)
				}
				if !strings.Contains(string(body), "sampling Claude failure") {
					t.Fatalf("missing error-frame diagnostic: %s", body)
				}
			}
		})
	}
}
