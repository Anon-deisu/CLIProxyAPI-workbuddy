package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func (s *Server) trackInferenceArrivals() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if c.Request.Method == http.MethodGet && (path == "/v1/responses" || path == "/backend-api/codex/responses") {
			c.Request = c.Request.WithContext(coreusage.WithRequestRate(c.Request.Context(), &s.requestRate))
		} else if c.Request.Method == http.MethodPost && inferenceRoute(path, c.Request.URL.Path) {
			s.requestRate.Record(time.Now())
		}
		c.Next()
	}
}

func inferenceRoute(route, path string) bool {
	switch route {
	case "/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses", "/v1/responses/compact",
		"/v1/images/generations", "/v1/images/edits", "/v1/audio/speech", "/v1/tts",
		"/v1/videos", "/v1/videos/generations", "/v1/videos/edits", "/v1/videos/extensions", "/openai/v1/videos",
		"/v1/alpha/search", "/v1/live", "/v1/realtime", "/v1/realtime/calls",
		"/backend-api/codex/responses", "/backend-api/codex/responses/compact", "/backend-api/codex/alpha/search",
		"/v1beta/interactions":
		return true
	case "/v1beta/models/*action":
		return strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
	default:
		return false
	}
}
