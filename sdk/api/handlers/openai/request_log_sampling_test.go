package openai

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

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type samplingStreamExecutor struct {
	mockStreamExecutor
	mode    string
	flushed <-chan struct{}
}

func (e *samplingStreamExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		first := `{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`
		if e.mode == "first-error" {
			first = `{"error":{"message":"sampling first error","type":"server_error"}}`
		}
		select {
		case chunks <- coreexecutor.StreamChunk{Payload: []byte(first)}:
		case <-ctx.Done():
			return
		}
		select {
		case <-e.flushed:
		case <-ctx.Done():
			return
		}
		var last coreexecutor.StreamChunk
		switch e.mode {
		case "error":
			last.Err = errors.New("sampling upstream failure")
		case "chunk-error":
			last.Payload = []byte(`{"error":{"message":"sampling chunk error","type":"server_error"}}`)
		case "success":
			last.Payload = []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		case "cancel":
			<-ctx.Done()
			return
		default:
			return
		}
		select {
		case chunks <- last:
		case <-ctx.Done():
		}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

type samplingFlushRecorder struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
	cancel  context.CancelFunc
}

func (r *samplingFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		close(r.flushed)
	})
}

func samplingReadLogs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			t.Errorf("leaked source %s", entry.Name())
			continue
		}
		body, errRead := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errRead != nil {
			t.Fatal(errRead)
		}
		logs = append(logs, string(body))
	}
	return logs
}

func TestRequestLogSamplingRealSSEErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{true, false} {
		for _, mode := range []string{"error", "missing-finish", "chunk-error", "first-error", "cancel", "success"} {
			t.Run(map[bool]string{true: "sample-zero/", false: "disabled/"}[enabled]+mode, func(t *testing.T) {
				flushed := make(chan struct{})
				exec := &samplingStreamExecutor{mode: mode, flushed: flushed}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(exec)
				authID := "sampling-sse-" + mode
				if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: exec.Identifier(), Status: coreauth.StatusActive}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(authID, exec.Identifier(), []*registry.ModelInfo{{ID: truncatedStreamChatModel}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
				base := handlers.NewBaseAPIHandlers(&config.SDKConfig{RequestLog: enabled}, manager)
				h := NewOpenAIAPIHandler(base)
				dir := t.TempDir()
				router := gin.New()
				router.Use(middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(enabled, dir, "", 0), middleware.WithRequestLogSampling(func() float64 { return 0 }, nil)))
				var captured []*interfaces.ErrorMessage
				router.POST("/v1/chat/completions", func(c *gin.Context) {
					h.ChatCompletions(c)
					value, _ := c.Get("API_RESPONSE_ERROR")
					captured, _ = value.([]*interfaces.ErrorMessage)
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				recorder := &samplingFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: flushed}
				if mode == "cancel" {
					recorder.cancel = cancel
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"truncated-stream-chat-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK || !recorder.Flushed {
					t.Fatalf("expected committed SSE, got %d", recorder.Code)
				}
				logs := samplingReadLogs(t, dir)
				if mode == "success" || mode == "cancel" {
					if len(logs) != 0 {
						t.Fatalf("%s created %d logs", mode, len(logs))
					}
					return
				}
				if len(captured) != 1 {
					t.Errorf("structured errors = %d, want 1", len(captured))
				}
				if len(logs) != 1 {
					t.Fatalf("failed HTTP 200 stream logs = %d, want 1", len(logs))
				}
				marker := "sampling"
				if mode == "missing-finish" {
					marker = "finish_reason"
				}
				if !strings.Contains(logs[0], marker) {
					t.Fatalf("missing structured failure in log: %s", logs[0])
				}
			})
		}
	}
}

func TestRequestLogSamplingWebsocketNoTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"success", "upstream-error", "error-only"} {
		t.Run(mode, func(t *testing.T) {
			enabled := mode != "error-only"
			exec := &websocketDirectCaptureExecutor{}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(exec)
			authID := "sampling-ws-" + mode
			if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: exec.Identifier(), Status: coreauth.StatusActive}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, exec.Identifier(), []*registry.ModelInfo{{ID: "sampling-ws-model"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
			h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{RequestLog: enabled}, manager))
			dir := t.TempDir()
			done := make(chan struct{})
			var timelineBytes, errorCount int
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Next(); close(done) })
			router.Use(middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(enabled, dir, "", 0), middleware.WithRequestLogSampling(func() float64 { return 0 }, nil)))
			router.GET("/v1/responses", func(c *gin.Context) {
				h.ResponsesWebsocket(c)
				value, _ := c.Get(wsTimelineBodyKey)
				body, _ := value.([]byte)
				timelineBytes = len(body)
				value, _ = c.Get("API_RESPONSE_ERROR")
				errs, _ := value.([]*interfaces.ErrorMessage)
				errorCount = len(errs)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			payload := []byte(`{"type":"response.create","model":"sampling-ws-model","input":[{"role":"user","content":"` + strings.Repeat("payload-not-captured-", 4096) + `"}]}`)
			for range 8 {
				if err = conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					t.Fatal(err)
				}
				_, data, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Fatal(errRead)
				}
				if ojson.GetBytesNoCopy(data, "type").String() != "response.completed" {
					t.Fatalf("unexpected event: %s", data)
				}
			}
			if mode != "success" {
				exec.mu.Lock()
				exec.failStatus = http.StatusBadGateway
				exec.mu.Unlock()
				if err = conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					t.Fatal(err)
				}
				_, _, _ = conn.ReadMessage()
			}
			_ = conn.Close()
			<-done
			if timelineBytes != 0 {
				t.Errorf("unselected socket accumulated %d timeline bytes", timelineBytes)
			}
			logs := samplingReadLogs(t, dir)
			if mode == "success" {
				if len(logs) != 0 {
					t.Fatal("successful unselected socket was logged")
				}
			} else {
				if errorCount == 0 || len(logs) != 1 {
					t.Fatalf("socket failure lost: structured=%d logs=%d", errorCount, len(logs))
				}
				if strings.Contains(logs[0], "payload-not-captured-") {
					t.Fatal("unselected socket retained complete payloads")
				}
			}
		})
	}
}
