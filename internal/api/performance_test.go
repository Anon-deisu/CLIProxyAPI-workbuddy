package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestPerformanceCountsArrivalsBeforeCompletionAndExcludesPolling(t *testing.T) {
	server := &Server{}
	router := gin.New()
	router.Use(server.trackInferenceArrivals())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		if got := server.requestRate.Count(time.Now()); got != 1 {
			t.Errorf("in-flight arrival absent from RPM: %d", got)
		}
		c.Status(http.StatusBadRequest)
	})
	router.POST("/v1/responses", func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	router.POST("/v1beta/models/*action", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.GET("/v1/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.GET("/v8/management/observability/performance", func(c *gin.Context) { c.Status(http.StatusOK) })
	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/chat/completions"}, {"POST", "/v1/responses"},
		{"POST", "/v1beta/models/gemini:streamGenerateContent"},
		{"POST", "/v1beta/models/gemini:countTokens"},
		{"POST", "/v1/unknown"}, {"GET", "/v1/models"},
		{"GET", "/v8/management/observability/performance"},
	} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(route.method, route.path, nil))
	}
	if got := server.requestRate.Count(time.Now()); got != 3 {
		t.Fatalf("RPM = %d, want 3 arrivals including failures", got)
	}
}

func TestPerformanceWebsocketCountsOperationsInsteadOfHandshake(t *testing.T) {
	server := &Server{}
	router := gin.New()
	router.Use(server.trackInferenceArrivals())
	router.GET("/v1/responses", func(c *gin.Context) {
		if server.requestRate.Count(time.Now()) != 0 {
			t.Error("websocket handshake counted as an inference")
		}
		coreusage.RecordRequestArrival(c.Request.Context(), time.Now())
		coreusage.RecordRequestArrival(c.Request.Context(), time.Now())
		c.Status(http.StatusOK)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if server.requestRate.Count(time.Now()) != 2 {
		t.Fatal("WebSocket operations did not share the HTTP RPM counter")
	}
}
