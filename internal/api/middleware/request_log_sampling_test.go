package middleware

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

func samplingLogContents(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			t.Errorf("leaked log source: %s", entry.Name())
			continue
		}
		body, errRead := os.ReadFile(filepath.Join(dir, entry.Name()))
		if errRead != nil {
			t.Fatal(errRead)
		}
		contents = append(contents, string(body))
	}
	return contents
}

func TestRequestLogSamplingSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		rate        float64
		draw        float64
		enabled     bool
		selected    bool
		defaultRate bool
	}{
		{name: "default", enabled: true, selected: true, defaultRate: true},
		{name: "zero", enabled: true},
		{name: "one", rate: 1, enabled: true, selected: true},
		{name: "selected", rate: 0.5, draw: 0.49, enabled: true, selected: true},
		{name: "boundary", rate: 0.5, draw: 0.5, enabled: true},
		{name: "not-selected", rate: 0.5, draw: 0.9, enabled: true},
		{name: "disabled", rate: 1},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/http", true: "/stream"}[stream], func(t *testing.T) {
				dir := t.TempDir()
				logger := logging.NewFileRequestLogger(tc.enabled, dir, "", 0)
				var draws int
				var rate func() float64
				if !tc.defaultRate {
					rate = func() float64 { return tc.rate }
				}
				router := gin.New()
				router.Use(RequestLoggingMiddleware(logger, WithRequestLogSampling(rate, func() float64 { draws++; return tc.draw })))
				router.POST("/v1/responses", func(c *gin.Context) {
					CaptureAuthenticatedRequestBody(c)
					if _, err := io.Copy(io.Discard, c.Request.Body); err != nil {
						t.Error(err)
					}
					executorCtx := context.WithValue(context.Background(), "gin", c)
					cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: tc.enabled}}
					helps.RecordAPIRequest(executorCtx, cfg, helps.UpstreamRequestLog{Body: []byte("upstream-request-marker")})
					helps.RecordAPIResponseMetadata(executorCtx, cfg, 200, nil)
					helps.AppendAPIResponseChunk(executorCtx, cfg, []byte("upstream-response-marker"))
					if tc.selected {
						for _, key := range []string{logging.APIRequestSourceContextKey, logging.APIResponseSourceContextKey} {
							source := extractFileBodySource(c, key)
							if source == nil || !source.HasPayload() {
								t.Errorf("selected request lost file source %s", key)
							}
						}
					} else {
						for _, key := range []string{logging.APIRequestSourceContextKey, logging.APIResponseSourceContextKey, "API_REQUEST", "API_RESPONSE"} {
							if _, exists := c.Get(key); exists {
								t.Errorf("unselected request captured %s eagerly", key)
							}
						}
						if _, exists := c.Get(logging.DeferredAPIRequestContextKey); !exists {
							t.Error("unselected request lost deferred upstream capture")
						}
					}
					if stream {
						c.Header("Content-Type", "text/event-stream")
						c.Writer.WriteHeader(http.StatusOK)
					}
					_, _ = c.Writer.WriteString("downstream-response-marker")
				})
				payload := strings.Repeat("x", int(maxErrorOnlyCapturedRequestBodyBytes)+1)
				router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)))
				logs := samplingLogContents(t, dir)
				want := 0
				if tc.selected {
					want = 1
				}
				if len(logs) != want {
					t.Fatalf("logs = %d, want %d", len(logs), want)
				}
				if want == 1 {
					for _, marker := range []string{payload, "upstream-request-marker", "upstream-response-marker", "downstream-response-marker"} {
						if !strings.Contains(logs[0], marker) {
							t.Errorf("selected log missing payload (%d bytes)", len(marker))
						}
					}
				}
				wantDraws := 0
				if tc.enabled && tc.rate > 0 && tc.rate < 1 {
					wantDraws = 1
				}
				if draws != wantDraws {
					t.Errorf("draws = %d, want %d", draws, wantDraws)
				}
			})
		}
	}
}

func TestRequestLogSamplingRetainsErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"redirect", "http-error", "stream-error", "panic", "committed-panic", "client-cancel", "abort"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			router := gin.New()
			router.Use(logging.GinLogrusRecovery())
			router.Use(RequestLoggingMiddleware(logging.NewFileRequestLogger(true, dir, "", 0), WithRequestLogSampling(func() float64 { return 0 }, nil)))
			router.POST("/v1/responses", func(c *gin.Context) {
				_, _ = io.Copy(io.Discard, c.Request.Body)
				executorCtx := context.WithValue(c.Request.Context(), "gin", c)
				helps.RecordAPIRequest(executorCtx, &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}, helps.UpstreamRequestLog{Body: []byte("deferred-upstream-marker")})
				switch mode {
				case "redirect":
					c.Status(http.StatusTemporaryRedirect)
				case "http-error":
					c.String(http.StatusBadGateway, "upstream failed")
				case "stream-error":
					c.Header("Content-Type", "text/event-stream")
					c.Writer.WriteHeader(http.StatusOK)
					_, _ = c.Writer.WriteString("data: partial\n\n")
					c.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{{StatusCode: 502, Error: errors.New("stream failed")}})
				case "panic":
					panic("sampling panic")
				case "committed-panic":
					_, _ = c.Writer.WriteString("partial")
					panic("sampling committed panic")
				case "client-cancel":
					c.Status(499)
				case "abort":
					panic(http.ErrAbortHandler)
				}
			})
			payload := strings.Repeat("large-error-body", 100000)
			func() {
				defer func() {
					if recovered := recover(); recovered != nil && recovered != http.ErrAbortHandler {
						panic(recovered)
					}
				}()
				router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)))
			}()
			logs := samplingLogContents(t, dir)
			if mode == "client-cancel" || mode == "abort" {
				if len(logs) != 0 {
					t.Fatal("existing cancellation exclusion was lost")
				}
				return
			}
			if len(logs) != 1 {
				t.Fatalf("error logs = %d, want 1", len(logs))
			}
			if !strings.Contains(logs[0], payload) || !strings.Contains(logs[0], "deferred-upstream-marker") {
				t.Fatal("error log lost deferred request capture")
			}
		})
	}
}

func TestRequestLogSamplingDecisionSurvivesReload(t *testing.T) {
	for _, initial := range []float64{0, 1} {
		dir := t.TempDir()
		var rate atomic.Uint64
		rate.Store(math.Float64bits(initial))
		started, finish := make(chan struct{}), make(chan struct{})
		router := gin.New()
		router.Use(RequestLoggingMiddleware(logging.NewFileRequestLogger(true, dir, "", 0), WithRequestLogSampling(func() float64 { return math.Float64frombits(rate.Load()) }, nil)))
		var requests int
		router.POST("/v1/responses", func(c *gin.Context) {
			requests++
			if requests == 1 {
				close(started)
				<-finish
			}
			c.String(http.StatusOK, "ok")
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		}()
		<-started
		rate.Store(math.Float64bits(1 - initial))
		close(finish)
		<-done
		if got := len(samplingLogContents(t, dir)); got != int(initial) {
			t.Fatalf("in-flight logs = %d, want %v", got, initial)
		}
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if got := len(samplingLogContents(t, dir)); got != 1 {
			t.Fatalf("after reload logs = %d, want 1", got)
		}
	}
}

func TestRequestLogSamplingGlobalToggleKeepsInflightDecision(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, phase := range []string{"before-stream", "after-chunk", "nonstream", "cancel", "panic"} {
			t.Run(map[bool]string{true: "enabled/", false: "disabled/"}[initial]+phase, func(t *testing.T) {
				dir := t.TempDir()
				logger := logging.NewFileRequestLogger(initial, dir, "", 0)
				started, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var wrapper *ResponseWriterWrapper
				router := gin.New()
				router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) { c.AbortWithStatus(500) }))
				router.Use(RequestLoggingMiddleware(logger))
				router.POST("/v1/next", func(c *gin.Context) { c.String(http.StatusOK, "next request") })
				router.POST("/v1/responses", func(c *gin.Context) {
					wrapper = c.Writer.(*ResponseWriterWrapper)
					if phase != "nonstream" {
						c.Header("Content-Type", "text/event-stream")
					}
					if phase != "before-stream" && phase != "nonstream" {
						c.Writer.WriteHeader(200)
						_, _ = c.Writer.WriteString("data: first\n\n")
					}
					close(started)
					<-resume
					if phase == "panic" {
						panic("toggle panic")
					}
					if phase == "cancel" {
						return
					}
					c.Writer.WriteHeader(200)
					_, _ = c.Writer.WriteString("data: final\n\n")
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					defer close(done)
					router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx))
				}()
				<-started
				logger.SetEnabled(!initial)
				if phase == "cancel" {
					cancel()
				}
				close(resume)
				<-done
				// Clean up the pre-fix leak after asserting, so a Red run leaves no worker behind.
				t.Cleanup(func() {
					if wrapper.chunkChannel != nil {
						close(wrapper.chunkChannel)
					}
					if wrapper.streamDone != nil {
						<-wrapper.streamDone
					}
					if wrapper.streamWriter != nil {
						if err := wrapper.streamWriter.Close(); err != nil {
							t.Error(err)
						}
					}
				})
				if wrapper.streamWriter != nil || wrapper.chunkChannel != nil || wrapper.streamDone != nil {
					t.Error("in-flight stream writer was not finalized")
				}
				logs := samplingLogContents(t, dir)
				want := 0
				if initial || phase == "panic" {
					want = 1
				}
				if len(logs) != want {
					t.Errorf("in-flight logs=%d want %d", len(logs), want)
				}
				if initial && phase != "panic" && phase != "cancel" && len(logs) == 1 && !strings.Contains(logs[0], "data: final") {
					t.Error("lost chunks after reload")
				}
				router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/next", nil))
				if !initial {
					want++
				}
				if got := len(samplingLogContents(t, dir)); got != want {
					t.Errorf("next request logs=%d want %d", got, want)
				}
			})
		}
	}
}

func TestRequestLogSamplingClosesWriterOnSourceError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	writer := &testStreamingLogWriter{}
	wrapper := NewResponseWriterWrapper(c.Writer, &testRequestLogger{enabled: true}, &RequestInfo{URL: "/v1/responses"})
	wrapper.isStreaming, wrapper.streamWriter = true, writer
	source, err := logging.NewFileBodySourceInDir(t.TempDir(), "broken")
	if err != nil {
		t.Fatal(err)
	}
	if err = source.AppendBytes([]byte("upstream")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(source.Paths()[0]); err != nil {
		t.Fatal(err)
	}
	c.Set(logging.APIRequestSourceContextKey, source)
	if err = wrapper.Finalize(c); err == nil {
		t.Error("expected source read error")
	}
	if !writer.closed {
		t.Error("source error skipped writer.Close")
	}
}

type samplingEmbeddedLogger struct {
	*logging.FileRequestLogger
	enabled      bool
	enabledCalls int
	streamCalls  int
	sourceCalls  int
	requestCalls int
	writer       samplingEmbeddedStreamWriter
}

func (l *samplingEmbeddedLogger) IsEnabled() bool {
	l.enabledCalls++
	return l.enabled
}

func (l *samplingEmbeddedLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	l.streamCalls++
	return &l.writer, nil
}

func (l *samplingEmbeddedLogger) NewFileBodySource(prefix string) (*logging.FileBodySource, error) {
	l.sourceCalls++
	return l.FileRequestLogger.NewFileBodySource(prefix)
}

func (l *samplingEmbeddedLogger) LogRequestWithOptionsAndAllSources(url, method string, requestHeaders map[string][]string, body []byte, status int, responseHeaders map[string][]string, response, timeline []byte, timelineSource *logging.FileBodySource, apiRequest []byte, apiRequestSource *logging.FileBodySource, apiResponse []byte, apiResponseSource *logging.FileBodySource, apiTimeline []byte, apiTimelineSource *logging.FileBodySource, apiErrors []*interfaces.ErrorMessage, force bool, id string, requestTime, responseTime time.Time) error {
	l.requestCalls++
	return l.FileRequestLogger.LogRequestWithOptionsAndAllSources(url, method, requestHeaders, body, status, responseHeaders, response, timeline, timelineSource, apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiTimeline, apiTimelineSource, apiErrors, force, id, requestTime, responseTime)
}

type samplingEmbeddedStreamWriter struct {
	testStreamingLogWriter
	requestSources  int
	responseSources int
}

func (w *samplingEmbeddedStreamWriter) WriteAPIRequestSource(*logging.FileBodySource) error {
	w.requestSources++
	return nil
}

func (w *samplingEmbeddedStreamWriter) WriteAPIResponseSource(*logging.FileBodySource) error {
	w.responseSources++
	return nil
}

func TestRequestLogSamplingPreservesEmbeddedLoggerOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled]+map[bool]string{false: "/http", true: "/stream"}[stream], func(t *testing.T) {
				dir := t.TempDir()
				logger := &samplingEmbeddedLogger{FileRequestLogger: logging.NewFileRequestLogger(true, dir, "", 0), enabled: enabled}
				router := gin.New()
				router.Use(RequestLoggingMiddleware(logger))
				router.POST("/v1/responses", func(c *gin.Context) {
					for _, key := range []string{logging.APIRequestSourceContextKey, logging.APIResponseSourceContextKey} {
						if source := extractFileBodySource(c, key); source != nil {
							if err := source.AppendBytes([]byte("source payload")); err != nil {
								t.Error(err)
							}
						}
					}
					if stream {
						c.Header("Content-Type", "text/event-stream")
					}
					c.Writer.WriteHeader(http.StatusOK)
					_, _ = c.Writer.WriteString("response payload")
					// An existing custom writer still needs closing if its global state changes.
					if stream {
						logger.SetEnabled(false)
					}
				})
				router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
				if logger.enabledCalls == 0 {
					t.Error("embedded IsEnabled override was bypassed")
				}
				logs := samplingLogContents(t, dir)
				if !enabled {
					if len(logs) != 0 || logger.streamCalls != 0 || logger.sourceCalls != 0 || logger.requestCalls != 0 {
						t.Fatal("disabled override did not prevent successful request capture")
					}
					return
				}
				if logger.sourceCalls != 2 {
					t.Errorf("custom source factory calls = %d, want 2", logger.sourceCalls)
				}
				if stream {
					if logger.streamCalls != 1 || !logger.writer.closed {
						t.Error("custom streaming writer was bypassed or not closed")
					}
					if logger.writer.requestSources != 1 || logger.writer.responseSources != 1 {
						t.Error("custom writer lost source-aware callbacks")
					}
					if len(logs) != 0 {
						t.Error("base logger wrote a file instead of invoking custom streaming writer")
					}
				} else if logger.requestCalls != 1 || len(logs) != 1 {
					t.Errorf("custom source-aware log calls = %d, files = %d, want 1 each", logger.requestCalls, len(logs))
				}
			})
		}
	}
}
