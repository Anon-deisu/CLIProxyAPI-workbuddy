package logging

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestLogSamplingCaptureDecision(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		if got := RequestLogCaptureEnabled(context.Background(), fallback); got != fallback {
			t.Fatalf("missing decision = %v, want fallback %v", got, fallback)
		}
		for _, selected := range []bool{false, true} {
			ctx := WithRequestLogCapture(context.Background(), selected)
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			if got := RequestLogCaptureEnabled(ctx, fallback); got != selected {
				t.Fatalf("inherited decision = %v, want %v", got, selected)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
			executorCtx := context.WithValue(context.Background(), "gin", c)
			if got := RequestLogCaptureEnabled(executorCtx, fallback); got != selected {
				t.Fatalf("Gin decision = %v, want %v", got, selected)
			}
			// An inherited decision must win even if a pooled Gin context has been reused.
			inherited := context.WithValue(ctx, "gin", c)
			c.Request = c.Request.WithContext(WithRequestLogCapture(context.Background(), !selected))
			if got := RequestLogCaptureEnabled(inherited, fallback); got != selected {
				t.Fatal("pooled Gin context changed an inherited decision")
			}
		}
	}
}

func TestRequestLogSamplingLoggerConcurrentToggle(t *testing.T) {
	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			logger.SetEnabled(i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 1000 {
			_ = logger.IsEnabled()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 32 {
			if err := logger.LogRequest("/sampling", "POST", nil, nil, 200, nil, nil, nil, nil, nil, nil, nil, "", time.Time{}, time.Time{}); err != nil {
				t.Error(err)
			}
			writer, err := logger.LogStreamingRequest("/sampling", "POST", nil, nil, "")
			if err != nil {
				t.Error(err)
				return
			}
			writer.WriteChunkAsync([]byte("chunk"))
			if err = writer.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	close(start)
	wg.Wait()
	// Keep same-package literal construction compatible with the synchronization change.
	literal := &FileRequestLogger{enabled: true}
	if !literal.IsEnabled() {
		t.Fatal("literal enabled state was lost")
	}
	literal.SetEnabled(false)
	if literal.IsEnabled() {
		t.Fatal("literal state did not update")
	}
}
