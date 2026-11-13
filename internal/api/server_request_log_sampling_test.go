package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestServerRequestLogSamplingReload(t *testing.T) {
	dir := t.TempDir()
	rate := 0.0
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true, RequestLogSampleRate: &rate}}
	s := newTestServerWithConfig(t, cfg, WithRequestLoggerFactory(func(cfg *config.Config, _ string) logging.RequestLogger {
		return logging.NewFileRequestLogger(cfg.RequestLog, dir, "", 0)
	}))
	s.engine.POST("/sampling-test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	for i, nextRate := range []*float64{&rate, nil, &rate} {
		if i > 0 {
			next := s.cfg.CloneForRuntime()
			next.RequestLogSampleRate = nextRate
			s.UpdateClients(next)
		}
		s.engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/sampling-test", nil))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if i == 0 {
			want = 0
		}
		if len(entries) != want {
			t.Fatalf("reload step %d: logs = %d, want %d", i, len(entries), want)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
				t.Fatalf("leaked source %s", entry.Name())
			}
		}
	}
}
