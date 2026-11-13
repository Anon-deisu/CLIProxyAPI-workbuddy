package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestReadRequestBodyEncodingAndLimits(t *testing.T) {
	payload := []byte(`{"input":"request body"}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encoder.Close() }()
	zstdBody := encoder.EncodeAll(payload, nil)
	for _, tc := range []struct {
		name, encoding string
		raw            []byte
		limit          int64
		wantErr        bool
		wantLimit      bool
	}{
		{"identity", "identity", payload, 0, false, false},
		{"zstd", "zstd", zstdBody, int64(len(payload)), false, false},
		{"stacked", "zstd, identity, zstd", encoder.EncodeAll(zstdBody, nil), 0, false, false},
		{"decoded-limit", "zstd", zstdBody, 3, true, true},
		{"plain-json-fallback", "zstd", payload, 0, false, false},
		{"unsupported-json-fallback", "br", payload, 0, false, false},
		{"malformed", "zstd", []byte("broken"), 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(tc.raw))
			c.Request.Header.Set("Content-Encoding", tc.encoding)
			c.Set(MaxRequestBodyBytesContextKey, tc.limit)
			got, errRead := ReadRequestBody(c)
			if (errRead != nil) != tc.wantErr {
				t.Fatalf("ReadRequestBody error = %v", errRead)
			}
			var maxErr *http.MaxBytesError
			if errors.As(errRead, &maxErr) != tc.wantLimit {
				t.Fatalf("limit error = %v", errRead)
			}
			if !tc.wantErr && !bytes.Equal(got, payload) {
				t.Fatalf("payload = %q", got)
			}
		})
	}
}

type requestBodyReadError struct{ err error }

func (b requestBodyReadError) Read([]byte) (int, error) { return 0, b.err }
func (b requestBodyReadError) Close() error             { return nil }

func TestReadRequestBodyPropagatesReadErrors(t *testing.T) {
	want := io.ErrUnexpectedEOF
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", requestBodyReadError{err: want})
	if _, errRead := ReadRequestBody(c); !errors.Is(errRead, want) {
		t.Fatalf("read error = %v, want %v", errRead, want)
	}
}

func TestReadRequestBodySnapshotRespectsChangedDecodeLimit(t *testing.T) {
	payload := []byte(`{"input":"request body"}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encoder.Close() }()
	raw := encoder.EncodeAll(payload, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("Content-Encoding", "zstd")
	RestoreCapturedRequestBody(c.Request, raw, payload, int64(len(payload)))
	c.Set(MaxRequestBodyBytesContextKey, int64(2))
	var maxErr *http.MaxBytesError
	if _, errRead := ReadRequestBody(c); !errors.As(errRead, &maxErr) || maxErr.Limit != 2 {
		t.Fatalf("snapshot bypassed tightened decode limit: %v", errRead)
	}
}

func TestReadRequestBodySnapshotDoesNotTrustGinBodyCache(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	raw := []byte("original")
	RestoreCapturedRequestBody(c.Request, raw, raw, 32<<20)
	c.Set(gin.BodyBytesKey, raw)
	c.Request.Body = io.NopCloser(strings.NewReader("replacement"))
	got, errRead := ReadRequestBody(c)
	if errRead != nil || string(got) != "replacement" {
		t.Fatalf("stale context bytes reused: %q, %v", got, errRead)
	}
}

func TestReadRequestBodySnapshotConcurrentClose(t *testing.T) {
	for range 64 {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		raw := []byte(`{"input":"request body"}`)
		RestoreCapturedRequestBody(c.Request, raw, raw, 32<<20)
		body := c.Request.Body
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Go(func() {
			<-start
			got, errRead := ReadRequestBody(c)
			if errRead != nil || !bytes.Equal(got, raw) {
				t.Errorf("body changed during concurrent close: %q, %v", got, errRead)
			}
		})
		workers.Go(func() {
			<-start
			if errClose := body.Close(); errClose != nil {
				t.Error(errClose)
			}
		})
		close(start)
		workers.Wait()
	}
}

func TestReadRequestBodySnapshotEditsPreserveOriginal(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encoder.Close() }()
	for _, tc := range []struct {
		name, encoding  string
		compressed      bool
		decoded         bool
		changedEncoding string
	}{
		{name: "identity", encoding: "identity", decoded: true},
		{name: "zstd", encoding: "zstd", compressed: true, decoded: true},
		{name: "identity-raw-only", encoding: "identity"},
		{name: "zstd-raw-only", encoding: "zstd", compressed: true},
		{name: "zstd-json-fallback", encoding: "zstd"},
		{name: "unsupported-json-fallback", encoding: "br"},
		{name: "changed-encoding", encoding: "identity", decoded: true, changedEncoding: "identity, identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"input":"old"}`)
			raw := payload
			if tc.compressed {
				raw = encoder.EncodeAll(payload, nil)
			}
			wantRaw := bytes.Clone(raw)
			var decoded []byte
			if tc.decoded {
				decoded = payload
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Request.Header.Set("Content-Encoding", tc.encoding)
			RestoreCapturedRequestBody(c.Request, raw, decoded, 32<<20)
			if tc.changedEncoding != "" {
				c.Request.Header.Set("Content-Encoding", tc.changedEncoding)
			}
			got, errRead := ReadRequestBody(c)
			if errRead != nil || !bytes.Equal(got, payload) {
				t.Fatalf("ReadRequestBody = %q, %v", got, errRead)
			}
			copy(got, []byte(`{"input":"new"}`))
			if string(payload) != `{"input":"old"}` {
				t.Errorf("SDK caller mutated the captured payload: %q", payload)
			}
			if !bytes.Equal(raw, wantRaw) {
				t.Errorf("SDK caller mutated the original request bytes: %q", raw)
			}
		})
	}
}

func TestGetRequestDetails_PreservesSuffix(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()

	modelRegistry.RegisterClient("test-request-details-gemini", "gemini", []*registry.ModelInfo{
		{ID: "gemini-2.5-pro", Created: now + 30},
		{ID: "gemini-2.5-flash", Created: now + 25},
	})
	modelRegistry.RegisterClient("test-request-details-openai", "openai", []*registry.ModelInfo{
		{ID: "gpt-5.2", Created: now + 20},
	})
	modelRegistry.RegisterClient("test-request-details-claude", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-5", Created: now + 5},
	})

	// Ensure cleanup of all test registrations.
	clientIDs := []string{
		"test-request-details-gemini",
		"test-request-details-openai",
		"test-request-details-claude",
	}
	for _, clientID := range clientIDs {
		id := clientID
		t.Cleanup(func() {
			modelRegistry.UnregisterClient(id)
		})
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	tests := []struct {
		name          string
		inputModel    string
		wantProviders []string
		wantModel     string
		wantErr       bool
	}{
		{
			name:          "numeric suffix preserved",
			inputModel:    "gemini-2.5-pro(8192)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(8192)",
			wantErr:       false,
		},
		{
			name:          "level suffix preserved",
			inputModel:    "gpt-5.2(high)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5.2(high)",
			wantErr:       false,
		},
		{
			name:          "no suffix unchanged",
			inputModel:    "claude-sonnet-4-5",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5",
			wantErr:       false,
		},
		{
			name:          "unknown model with suffix",
			inputModel:    "unknown-model(8192)",
			wantProviders: nil,
			wantModel:     "",
			wantErr:       true,
		},
		{
			name:          "auto suffix resolved",
			inputModel:    "auto(high)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(high)",
			wantErr:       false,
		},
		{
			name:          "special suffix none preserved",
			inputModel:    "gemini-2.5-flash(none)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-flash(none)",
			wantErr:       false,
		},
		{
			name:          "special suffix auto preserved",
			inputModel:    "claude-sonnet-4-5(auto)",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5(auto)",
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, model, errMsg := handler.getRequestDetails(tt.inputModel)
			if (errMsg != nil) != tt.wantErr {
				t.Fatalf("getRequestDetails() error = %v, wantErr %v", errMsg, tt.wantErr)
			}
			if errMsg != nil {
				return
			}
			if !reflect.DeepEqual(providers, tt.wantProviders) {
				t.Fatalf("getRequestDetails() providers = %v, want %v", providers, tt.wantProviders)
			}
			if model != tt.wantModel {
				t.Fatalf("getRequestDetails() model = %v, want %v", model, tt.wantModel)
			}
		})
	}
}

func TestGetRequestDetails_RoutesCursorFableBuiltin(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	const clientID = "test-request-details-cursor-fable"
	modelRegistry.RegisterClient(clientID, "cursor", registry.GetCursorModels())
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	providers, model, errMsg := handler.getRequestDetails("claude-fable-5")
	if errMsg != nil {
		t.Fatalf("getRequestDetails() error = %v", errMsg)
	}
	if !containsProvider(providers, "cursor") {
		t.Fatalf("getRequestDetails() providers = %v, want cursor", providers)
	}
	if model != "claude-fable-5" {
		t.Fatalf("getRequestDetails() model = %q, want claude-fable-5", model)
	}
}

func containsProvider(providers []string, want string) bool {
	for _, provider := range providers {
		if provider == want {
			return true
		}
	}
	return false
}

// TestGetRequestDetails_UnknownModelErrorResistsJSONInjection pins the unroutable
// model error body against client-controlled model names. The name is echoed into
// the body, so formatting it into a JSON literal would let a caller corrupt the
// payload or overwrite the error code that clients branch on.
func TestGetRequestDetails_UnknownModelErrorResistsJSONInjection(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	for _, model := range []string{
		"unroutable-model",
		`foo"bar`,
		`x","code":"insufficient_quota","x":"`,
		`x"}}`,
		`foo\bar`,
		"foo\nbar",
	} {
		t.Run(model, func(t *testing.T) {
			_, _, errMsg := handler.getRequestDetails(model)
			if errMsg == nil || errMsg.Error == nil {
				t.Fatal("expected an error for an unroutable model")
			}
			if errMsg.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusBadRequest)
			}
			body := errMsg.Error.Error()
			if !json.Valid([]byte(body)) {
				t.Fatalf("error body is not valid JSON: %s", body)
			}
			if got := ojson.Get(body, "error.code").String(); got != "model_not_found" {
				t.Fatalf("error code = %q, want model_not_found; the caller controlled the body: %s", got, body)
			}
			if got, want := ojson.Get(body, "error.message").String(), "unknown provider for model "+model; got != want {
				t.Fatalf("error message = %q, want %q", got, want)
			}
		})
	}
}

func TestGetRequestDetails_ImageModelReturns503(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	imageOnlyModels := []string{
		"gpt-image-1.5",
		"gpt-image-2",
		"codex/gpt-image-2",
		"gpt-image-2.5-flare",
		"codex/gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"codex/gpt-image-2.5-sunburst",
		"gpt-image-2.5",
		"codex/gpt-image-2.5",
		"grok-imagine-image",
		"xai/grok-imagine-image",
		"grok-imagine-image-quality",
		"xai/grok-imagine-image-quality",
		"grok-imagine-image-2.0",
		"xai/grok-imagine-image-2.0",
	}
	for _, model := range imageOnlyModels {
		t.Run(model, func(t *testing.T) {
			_, _, errMsg := handler.getRequestDetails(model)
			if errMsg == nil {
				t.Fatalf("expected error for %s, got nil", model)
			}
			if errMsg.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("unexpected status code: got %d want %d", errMsg.StatusCode, http.StatusServiceUnavailable)
			}
			if errMsg.Error == nil {
				t.Fatalf("expected error message, got nil")
			}
			msg := errMsg.Error.Error()
			if !strings.Contains(msg, "/v1/images/generations") || !strings.Contains(msg, "/v1/images/edits") {
				t.Fatalf("unexpected error message: %q", msg)
			}
		})
	}
}

func TestValidateImageOnlyModel_AllowsImageEndpoints(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	imageOnlyModels := []string{
		"gpt-image-1.5",
		"gpt-image-2",
		"codex/gpt-image-2",
		"gpt-image-2.5-flare",
		"codex/gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"codex/gpt-image-2.5-sunburst",
		"gpt-image-2.5",
		"codex/gpt-image-2.5",
		"grok-imagine-image",
		"xai/grok-imagine-image",
		"grok-imagine-image-quality",
		"xai/grok-imagine-image-quality",
		"grok-imagine-image-2.0",
		"xai/grok-imagine-image-2.0",
	}
	for _, model := range imageOnlyModels {
		t.Run(model, func(t *testing.T) {
			if errMsg := handler.validateImageOnlyModel(model, true); errMsg != nil {
				t.Fatalf("validateImageOnlyModel(%q, true) = %+v, want nil", model, errMsg)
			}
			if errMsg := handler.validateImageOnlyModel(model, false); errMsg == nil {
				t.Fatalf("validateImageOnlyModel(%q, false) = nil, want image-only error", model)
			} else if errMsg.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("unexpected status code: got %d want %d", errMsg.StatusCode, http.StatusServiceUnavailable)
			}
		})
	}
}

func TestIsOpenAIImageOnlyModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "gpt-image-1.5", want: true},
		{model: "gpt-image-2", want: true},
		{model: "codex/gpt-image-1.5", want: true},
		{model: "gpt-image-2.5-flare", want: true},
		{model: "codex/gpt-image-2.5-flare", want: true},
		{model: "gpt-image-2.5-sunburst", want: true},
		{model: "codex/gpt-image-2.5-sunburst", want: true},
		{model: "gpt-image-2.5", want: true},
		{model: "codex/gpt-image-2.5", want: true},
		{model: "grok-imagine-image", want: true},
		{model: "xai/grok-imagine-image", want: true},
		{model: "XAI/Grok-Imagine-Image-Quality", want: true},
		{model: "grok-imagine-image-quality", want: true},
		{model: "grok-imagine-image-2.0", want: true},
		{model: "xai/grok-imagine-image-2.0", want: true},
		{model: "grok-3", want: false},
		{model: "gpt-5.2", want: false},
		{model: "grok-imagine-video", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := isOpenAIImageOnlyModel(tt.model); got != tt.want {
				t.Fatalf("isOpenAIImageOnlyModel(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestExecuteImageWithAuthManager_AllowsImageOnlyModels(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	imageOnlyModels := []string{
		"gpt-image-1.5",
		"gpt-image-2",
		"gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"gpt-image-2.5",
		"grok-imagine-image",
		"grok-imagine-image-quality",
		"xai/grok-imagine-image-quality",
		"grok-imagine-image-2.0",
		"xai/grok-imagine-image-2.0",
	}
	for _, model := range imageOnlyModels {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"model":"` + model + `","prompt":"draw"}`)
			_, _, errMsg := handler.ExecuteImageWithAuthManager(context.Background(), "openai-image", model, body, "")
			if errMsg == nil {
				t.Fatal("expected auth selection error, got nil")
			}
			if errMsg.Error == nil {
				t.Fatal("expected error message, got nil")
			}
			msg := errMsg.Error.Error()
			if strings.Contains(msg, "only supported on /v1/images/generations") {
				t.Fatalf("ExecuteImageWithAuthManager rejected image-only model: %q", msg)
			}

			_, _, errMsg = handler.ExecuteWithAuthManager(context.Background(), "openai-image", model, body, "")
			if errMsg == nil {
				t.Fatal("expected image-only rejection for non-image execution path, got nil")
			}
			if errMsg.Error == nil || !strings.Contains(errMsg.Error.Error(), "only supported on /v1/images/generations") {
				t.Fatalf("unexpected non-image execution error: %+v", errMsg)
			}
		})
	}
}
