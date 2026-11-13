package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// MaxRequestBodyBytesContextKey stores the configured request body size limit (in bytes) on the Gin context.
const MaxRequestBodyBytesContextKey = "maxRequestBodyBytes"

// capturedRequestBody owns immutable snapshots for a single restored body. Any
// ordinary Read or Close invalidates reuse, even when the body is later restored
// by a plugin. Wrapping or replacing Request.Body also bypasses this fast path.
type capturedRequestBody struct {
	mu           sync.Mutex
	reader       *bytes.Reader
	raw          []byte
	decoded      []byte
	encoding     string
	decodedLimit int64
	untouched    bool
}

// RestoreCapturedRequestBody transfers ownership of raw and decoded to the
// request. Callers must not mutate either slice afterwards. decoded must be a
// complete, successfully decoded body under decodedLimit (including intermediate
// encodings), or nil when decoding failed or was truncated. Raw bytes and
// Content-Encoding remain available to ordinary middleware readers.
// Only call this after authentication and a successful, size-limited body read.
func RestoreCapturedRequestBody(req *http.Request, raw, decoded []byte, decodedLimit int64) {
	req.Body = &capturedRequestBody{
		reader:       bytes.NewReader(raw),
		raw:          raw,
		decoded:      decoded,
		encoding:     strings.TrimSpace(req.Header.Get("Content-Encoding")),
		decodedLimit: decodedLimit,
		untouched:    true,
	}
}

func (b *capturedRequestBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.invalidateLocked()
	return b.reader.Read(p)
}

func (b *capturedRequestBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.invalidateLocked()
	return nil
}

func (b *capturedRequestBody) invalidateLocked() {
	b.untouched = false
	b.raw = nil
	b.decoded = nil
}

func (b *capturedRequestBody) takeSnapshot(encoding string, maxBytes int64) (raw, decoded []byte, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.untouched {
		return nil, nil, false
	}
	raw = b.raw
	if encoding == b.encoding && (maxBytes <= 0 || (b.decodedLimit > 0 && maxBytes >= b.decodedLimit)) {
		decoded = b.decoded
	}
	b.invalidateLocked()
	b.reader.Reset(nil)
	return raw, decoded, true
}

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields. The caller owns
// the returned slice and may mutate it without changing captured request logs
// or the original request bytes.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	maxBytes := int64(0)
	if c != nil {
		maxBytes = c.GetInt64(MaxRequestBodyBytesContextKey)
	}
	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	var raw []byte
	var captured bool
	if snapshot, ok := c.Request.Body.(*capturedRequestBody); ok {
		var decoded []byte
		raw, decoded, captured = snapshot.takeSnapshot(encoding, maxBytes)
		if decoded != nil {
			return bytes.Clone(decoded), nil
		}
		if captured {
			// Identity and JSON fallback paths can return raw directly. Detach
			// it from the snapshot without another ReadAll and its buffer growth.
			raw = bytes.Clone(raw)
		}
	}
	if !captured {
		var errRead error
		raw, errRead = c.GetRawData()
		if errRead != nil {
			return nil, errRead
		}
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	decoded, err := decodeRequestBody(raw, encoding, maxBytes)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, err
		}
		if ojson.ValidBytes(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

func decodeRequestBody(raw []byte, encoding string, maxBytes int64) ([]byte, error) {
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBody(body, maxBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte, maxBytes int64) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	var reader io.Reader = decoder
	if maxBytes > 0 && maxBytes < math.MaxInt64 {
		reader = io.LimitReader(decoder, maxBytes+1)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	if maxBytes > 0 && int64(len(decoded)) > maxBytes {
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}
	return decoded, nil
}
