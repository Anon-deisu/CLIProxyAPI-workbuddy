package helps

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestRequestLogSamplingGatesAllUpstreamRecords(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(logging.WithRequestLogCapture(context.Background(), false))
	ctx := context.WithValue(context.Background(), "gin", c)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	logger := logging.NewFileRequestLogger(true, t.TempDir(), "", 0)
	for _, key := range []string{logging.APIRequestSourceContextKey, logging.APIResponseSourceContextKey, logging.APIWebsocketTimelineSourceContextKey} {
		source, err := logger.NewFileBodySource("sample-test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if errCleanup := source.Cleanup(); errCleanup != nil {
				t.Error(errCleanup)
			}
		})
		c.Set(key, source)
	}
	info := UpstreamRequestLog{Body: []byte("upstream")}
	RecordAPIRequest(ctx, cfg, info)
	RecordAPIResponseMetadata(ctx, cfg, 200, nil)
	AppendAPIResponseChunk(ctx, cfg, []byte("response"))
	RecordAPIResponseError(ctx, cfg, errors.New("error"))
	RecordAPIWebsocketRequest(ctx, cfg, info)
	RecordAPIWebsocketHandshake(ctx, cfg, 101, nil)
	RecordAPIWebsocketUpgradeRejection(ctx, cfg, info, 400, nil, []byte("rejected"))
	AppendAPIWebsocketResponse(ctx, cfg, []byte("frame"))
	RecordAPIWebsocketError(ctx, cfg, "read", errors.New("socket error"))
	for _, key := range []string{logging.APIRequestSourceContextKey, logging.APIResponseSourceContextKey, logging.APIWebsocketTimelineSourceContextKey} {
		value, _ := c.Get(key)
		if value.(*logging.FileBodySource).HasPayload() {
			t.Errorf("unselected upstream wrote %s", key)
		}
	}
	if _, exists := c.Get(logging.DeferredAPIRequestContextKey); !exists {
		t.Error("lost deferred HTTP request")
	}
}

func TestRequestLogSamplingPreservesDeferredCaptureOptOut(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", c)
	ctx = logging.WithDeferredAPIRequestCaptureDisabled(logging.WithRequestLogCapture(ctx, false))
	RecordAPIRequest(ctx, &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}, UpstreamRequestLog{Body: []byte("ignored")})
	if _, exists := c.Get(logging.DeferredAPIRequestContextKey); exists {
		t.Fatal("deferred capture opt-out was ignored")
	}
	if _, exists := c.Get(apiRequestKey); exists {
		t.Fatal("unselected request captured eagerly")
	}
}
