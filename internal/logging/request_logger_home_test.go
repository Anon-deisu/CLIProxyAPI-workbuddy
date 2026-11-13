package logging

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func TestFileBodySourceOrderedLifecycle(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Cleanup() })
	for _, chunk := range []string{"first", "-tail"} {
		if errAppend := source.AppendBytes([]byte(chunk)); errAppend != nil {
			t.Fatal(errAppend)
		}
	}
	paths := source.Paths()
	if got, errRead := os.ReadFile(paths[0]); errRead != nil || string(got) != "first-tail" {
		t.Fatalf("Paths must publish pending bytes: %q, %v", got, errRead)
	}
	if errAppend := source.AppendPart([]byte("  second  ")); errAppend != nil {
		t.Fatal(errAppend)
	}
	if errAppend := source.AppendBytes([]byte("second-tail")); errAppend != nil {
		t.Fatal(errAppend)
	}
	part, errPart := source.CreatePart("external")
	if errPart != nil {
		t.Fatal(errPart)
	}
	if _, errWrite := part.WriteString("third"); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := part.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errAppend := source.AppendBytes([]byte("-tail")); errAppend != nil {
		t.Fatal(errAppend)
	}
	got, errBytes := source.Bytes()
	if errBytes != nil || string(got) != "first-tail\nsecond\nsecond-tail\nthird-tail" {
		t.Fatalf("ordered body = %q, %v", got, errBytes)
	}
	paths = source.Paths()
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	assertFileBodySourceCleaned(t, paths)
	if errAppend := source.AppendBytes([]byte("late")); errAppend == nil {
		t.Fatal("append after cleanup succeeded")
	}
}

func TestFileBodySourceCleanupClosesOutstandingPart(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "external")
	if err != nil {
		t.Fatal(err)
	}
	part, errPart := source.CreatePart("unfinished")
	if errPart != nil {
		t.Fatal(errPart)
	}
	defer func() { _ = part.Close() }()
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	if _, errWrite := part.WriteString("late"); !errors.Is(errWrite, os.ErrClosed) {
		t.Fatalf("outstanding handle survived cleanup: %v", errWrite)
	}
}

type fileBodyLifecycleWriter struct {
	source *FileBodySource
	t      *testing.T
}

func (w fileBodyLifecycleWriter) Write(p []byte) (int, error) {
	if w.source.mu.TryLock() {
		w.source.mu.Unlock()
		w.t.Error("WriteTo must protect its part files from concurrent cleanup")
	}
	return len(p), nil
}

func TestFileBodySourceWriteToProtectsLifecycle(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "write")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("payload")); errAppend != nil {
		t.Fatal(errAppend)
	}
	if _, errWrite := source.WriteTo(fileBodyLifecycleWriter{source: source, t: t}); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func TestFileBodySourceConcurrentLifecycle(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			<-start
			for range 20 {
				switch i % 4 {
				case 0:
					_ = source.AppendBytes([]byte("chunk"))
				case 1:
					_ = source.AppendPart([]byte("part"))
				case 2:
					_, _ = source.WriteTo(io.Discard)
				case 3:
					_ = source.Cleanup()
				}
			}
		})
	}
	close(start)
	workers.Wait()
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	if _, errStat := os.Stat(source.dir); !os.IsNotExist(errStat) {
		t.Fatalf("source directory survived cleanup: %v", errStat)
	}
}

func TestFileBodySourceBoundedBufferAndFlushFailure(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "flush")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 257)
	for range 1000 {
		if errAppend := source.AppendBytes(chunk); errAppend != nil {
			t.Fatal(errAppend)
		}
	}
	stat, errStat := source.appendFile.Stat()
	if errStat != nil || stat.Size() < int64(1000*len(chunk)-fileBodyBufferSize) {
		t.Fatalf("pending data exceeded the fixed buffer: %v, %v", stat, errStat)
	}
	path := source.appendFile.Name()
	if errClose := source.appendFile.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errFlush := source.Flush(); !errors.Is(errFlush, os.ErrClosed) {
		t.Fatalf("flush failure = %v", errFlush)
	}
	if errAppend := source.AppendBytes([]byte("late")); !errors.Is(errAppend, os.ErrClosed) {
		t.Fatalf("write failure was lost: %v", errAppend)
	}
	if _, errBytes := source.Bytes(); !errors.Is(errBytes, os.ErrClosed) {
		t.Fatalf("observation failure was lost: %v", errBytes)
	}
	for range 2 {
		if errCleanup := source.Cleanup(); !errors.Is(errCleanup, os.ErrClosed) {
			t.Fatalf("cleanup failure was lost: %v", errCleanup)
		}
	}
	assertFileBodySourceCleaned(t, []string{path})
}

func TestFileBodySourceConcurrentAppendsPreserveAllBytes(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "append")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Cleanup() }()
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 128 {
				if errAppend := source.AppendBytes([]byte("chunk\n")); errAppend != nil {
					t.Error(errAppend)
				}
			}
		})
	}
	workers.Wait()
	body, errBytes := source.Bytes()
	if errBytes != nil || !bytes.Equal(body, bytes.Repeat([]byte("chunk\n"), 8*128)) {
		t.Fatalf("concurrent append lost bytes: len=%d, %v", len(body), errBytes)
	}
}

func TestFileBodySourceReportsRemovedActivePart(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "removed")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("pending")); errAppend != nil {
		t.Fatal(errAppend)
	}
	if errRemove := os.Remove(source.appendFile.Name()); errRemove != nil {
		t.Skipf("platform does not allow removing an open file: %v", errRemove)
	}
	if _, errBytes := source.Bytes(); !os.IsNotExist(errBytes) {
		t.Fatalf("removed active log part silently lost bytes: %v", errBytes)
	}
}

func TestFileStreamingLogWriterCleansOwnedSourcesOnCloseFailure(t *testing.T) {
	source, err := NewFileBodySourceInDir(t.TempDir(), "stream-source")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Cleanup() }()
	if errAppend := source.AppendBytes([]byte("pending API response")); errAppend != nil {
		t.Fatal(errAppend)
	}
	paths := source.Paths()
	w := &FileStreamingLogWriter{logsDir: filepath.Join(t.TempDir(), "missing"), logFilename: "test.log"}
	if errSource := w.WriteAPIResponseSource(source); errSource != nil {
		t.Fatal(errSource)
	}
	if errClose := w.Close(); errClose == nil {
		t.Fatal("expected log creation error")
	}
	assertFileBodySourceCleaned(t, paths)
}

func BenchmarkFileBodySourceAppendBytes(b *testing.B) {
	dir := b.TempDir()
	chunk := bytes.Repeat([]byte("x"), 256)
	b.ReportAllocs()
	b.SetBytes(int64(1024 * len(chunk)))
	for b.Loop() {
		source, err := NewFileBodySourceInDir(dir, "bench")
		if err != nil {
			b.Fatal(err)
		}
		for range 1024 {
			if errAppend := source.AppendBytes(chunk); errAppend != nil {
				b.Fatal(errAppend)
			}
		}
		if _, errWrite := source.WriteTo(io.Discard); errWrite != nil {
			b.Fatal(errWrite)
		}
		if errCleanup := source.Cleanup(); errCleanup != nil {
			b.Fatal(errCleanup)
		}
	}
}

func gzipLogPayload(t testing.TB, payload []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func logResponseFixture(logger *FileRequestLogger, encoding string, response []byte) error {
	return logger.LogRequest("/v1/responses", "POST", nil, nil, 200, map[string][]string{"Content-Encoding": {encoding}}, response, nil, nil, nil, nil, nil, "decode-test", time.Time{}, time.Time{})
}

func TestRequestLogCompressedResponse(t *testing.T) {
	payload := []byte("\nplain response\n")
	gz := gzipLogPayload(t, payload)
	badChecksum := bytes.Clone(gz)
	badChecksum[len(badChecksum)-8] ^= 1
	var deflated, brotliBody bytes.Buffer
	deflater, errDeflate := flate.NewWriter(&deflated, flate.DefaultCompression)
	if errDeflate != nil {
		t.Fatal(errDeflate)
	}
	_, _ = deflater.Write(payload)
	if errClose := deflater.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	brotliWriter := brotli.NewWriter(&brotliBody)
	_, _ = brotliWriter.Write(payload)
	if errClose := brotliWriter.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encoder.Close() }()
	for _, tc := range []struct {
		name, encoding string
		raw            []byte
		malformed      bool
	}{
		{"identity", "identity", payload, false},
		{"gzip", "gzip", gz, false},
		{"gzip-multistream", "gzip", append(gzipLogPayload(t, payload[:5]), gzipLogPayload(t, payload[5:])...), false},
		{"zstd", "zstd", encoder.EncodeAll(payload, nil), false},
		{"deflate", "deflate", deflated.Bytes(), false},
		{"brotli", "br", brotliBody.Bytes(), false},
		{"gzip-header", "gzip", []byte("invalid gzip"), true},
		{"gzip-truncated", "gzip", gz[:len(gz)-4], true},
		{"gzip-checksum", "gzip", badChecksum, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if errLog := logResponseFixture(NewFileRequestLogger(true, dir, "", 0), tc.encoding, tc.raw); errLog != nil {
				t.Fatal(errLog)
			}
			entries, errRead := os.ReadDir(dir)
			if errRead != nil || len(entries) != 1 {
				t.Fatalf("log files: %v, %v", entries, errRead)
			}
			logged, errRead := os.ReadFile(filepath.Join(dir, entries[0].Name()))
			if errRead != nil {
				t.Fatal(errRead)
			}
			body := bytes.SplitN(logged, []byte("=== RESPONSE ===\n"), 2)[1]
			if tc.malformed {
				if !bytes.Contains(body, tc.raw) || !bytes.Contains(body, []byte("[DECOMPRESSION ERROR:")) || bytes.Contains(body, payload) {
					t.Fatalf("malformed response must retain raw bytes with an error and no partial plaintext: %q", body)
				}
			} else if !bytes.HasSuffix(body, append(bytes.Clone(payload), '\n')) || bytes.Contains(body, []byte("[DECOMPRESSION ERROR:")) {
				t.Fatalf("decoded body: %q", body)
			}
		})
	}
}

func TestRequestLogMalformedGzipDiscardsExpandedBody(t *testing.T) {
	raw := gzipLogPayload(t, bytes.Repeat([]byte("x"), 1<<20))
	raw[len(raw)-8] ^= 1
	for _, destination := range []string{"file", "home-buffer", "arbitrary-writer"} {
		t.Run(destination, func(t *testing.T) {
			var body bytes.Buffer
			var w io.Writer = &body
			var file *os.File
			if destination == "file" {
				var errCreate error
				file, errCreate = os.CreateTemp(t.TempDir(), "response-*.log")
				if errCreate != nil {
					t.Fatal(errCreate)
				}
				defer func() { _ = file.Close() }()
				w = file
			} else if destination == "arbitrary-writer" {
				w = struct{ io.Writer }{&body}
			}
			_, _ = io.WriteString(w, "request prefix\n")
			if errWrite := writeDecodedResponseSection(w, 200, map[string][]string{"Content-Encoding": {"gzip"}}, raw); errWrite != nil {
				t.Fatal(errWrite)
			}
			logged := body.Bytes()
			if file != nil {
				var errRead error
				logged, errRead = os.ReadFile(file.Name())
				if errRead != nil {
					t.Fatal(errRead)
				}
			}
			if !bytes.HasPrefix(logged, []byte("request prefix\n")) || !bytes.Contains(logged, raw) || !bytes.Contains(logged, []byte("[DECOMPRESSION ERROR:")) || len(logged) > len(raw)+512 {
				t.Fatalf("failed to roll back partial plaintext: log=%d bytes, raw=%d bytes", len(logged), len(raw))
			}
		})
	}
}

type failingResponseLogWriter struct{ err error }

func (w failingResponseLogWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRequestLogPropagatesDestinationError(t *testing.T) {
	want := errors.New("log storage failed")
	raw := gzipLogPayload(t, []byte("body"))
	if errWrite := writeDecodedResponseSection(failingResponseLogWriter{err: want}, 200, map[string][]string{"Content-Encoding": {"gzip"}}, raw); !errors.Is(errWrite, want) {
		t.Fatalf("write error = %v, want %v", errWrite, want)
	}
}

func TestRequestLogGzipDoesNotAllocateExpandedBody(t *testing.T) {
	raw := gzipLogPayload(t, bytes.Repeat([]byte("x"), 8<<20))
	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if errLog := logResponseFixture(logger, "gzip", raw); errLog != nil {
		t.Fatal(errLog)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("logging an 8 MiB gzip response allocated %d bytes; decoded output must be streamed", allocated)
	}
}

func BenchmarkRequestLogGzipResponse(b *testing.B) {
	raw := gzipLogPayload(b, bytes.Repeat([]byte("x"), 1<<20))
	dir := b.TempDir()
	logger := NewFileRequestLogger(true, dir, "", 0)
	b.ReportAllocs()
	b.SetBytes(1 << 20)
	for b.Loop() {
		if errLog := logResponseFixture(logger, "gzip", raw); errLog != nil {
			b.Fatal(errLog)
		}
		entries, errRead := os.ReadDir(dir)
		if errRead != nil {
			b.Fatal(errRead)
		}
		for _, entry := range entries {
			if errRemove := os.Remove(filepath.Join(dir, entry.Name())); errRemove != nil {
				b.Fatal(errRemove)
			}
		}
	}
}

type stubHomeRequestLogClient struct {
	heartbeatOK bool
	pushed      [][]byte
}

func (c *stubHomeRequestLogClient) HeartbeatOK() bool { return c.heartbeatOK }

func (c *stubHomeRequestLogClient) RPushRequestLog(_ context.Context, payload []byte) error {
	c.pushed = append(c.pushed, bytes.Clone(payload))
	return nil
}

func assertFileBodySourceCleaned(t *testing.T, partPaths []string) {
	t.Helper()

	dirs := make(map[string]struct{}, len(partPaths))
	for _, path := range partPaths {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("expected part %s to be removed, stat err=%v", path, errStat)
		}
		dirs[filepath.Dir(path)] = struct{}{}
	}
	for dir := range dirs {
		if _, errStat := os.Stat(dir); !os.IsNotExist(errStat) {
			t.Fatalf("expected part dir %s to be removed, stat err=%v", dir, errStat)
		}
	}
}

func TestFileBodySource_RecreatesPartDirAfterManualCleanup(t *testing.T) {
	logsDir := t.TempDir()
	source, errSource := NewFileBodySourceInDir(logsDir, "websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	if errAppend := source.AppendPart([]byte("before manual cleanup")); errAppend != nil {
		t.Fatalf("AppendPart before cleanup: %v", errAppend)
	}
	if errRemove := os.RemoveAll(logsDir); errRemove != nil {
		t.Fatalf("RemoveAll logs dir: %v", errRemove)
	}
	if errAppend := source.AppendPart([]byte("after manual cleanup")); errAppend != nil {
		t.Fatalf("AppendPart after cleanup: %v", errAppend)
	}

	raw, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("Bytes after cleanup: %v", errBytes)
	}
	if bytes.Contains(raw, []byte("before manual cleanup")) {
		t.Fatalf("expected manually removed part to be skipped, got %q", string(raw))
	}
	if !bytes.Contains(raw, []byte("after manual cleanup")) {
		t.Fatalf("expected recreated part content, got %q", string(raw))
	}

	partPaths := source.Paths()
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("Cleanup: %v", errCleanup)
	}
	assertFileBodySourceCleaned(t, partPaths)
}

func TestFileRequestLogger_HomeEnabled_ForwardsWhenRequestLogEnabled(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	requestHeaders := map[string][]string{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer secret"},
	}

	errLog := logger.LogRequest(
		"/v1/chat/completions",
		http.MethodPost,
		requestHeaders,
		[]byte(`{"input":"hello"}`),
		http.StatusOK,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"ok":true}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		"req-1",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequest error: %v", errLog)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no local request log files, got entries: %+v", entries)
	}

	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		Headers    map[string][]string `json:"headers"`
		RequestID  string              `json:"request_id"`
		RequestLog string              `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.Headers == nil || got.Headers["Content-Type"][0] != "application/json" {
		t.Fatalf("headers.content-type = %+v, want application/json", got.Headers["Content-Type"])
	}
	if got.Headers == nil || got.Headers["Authorization"][0] != "Bearer secret" {
		t.Fatalf("headers.authorization = %+v, want Bearer secret", got.Headers["Authorization"])
	}
	if got.RequestID != "req-1" {
		t.Fatalf("request_id = %q, want req-1", got.RequestID)
	}
	if got.RequestLog == "" {
		t.Fatalf("request_log empty, want non-empty")
	}
}

func TestFileRequestLogger_LogRequestWithSourcesWritesLocalLogAndCleansParts(t *testing.T) {
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)

	timelineSource, errSource := logger.NewFileBodySource("websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("logger.NewFileBodySource: %v", errSource)
	}
	if errAppend := timelineSource.AppendPart([]byte("Timestamp: 2026-05-25T12:00:00Z\nEvent: websocket.request\n{}")); errAppend != nil {
		t.Fatalf("AppendPart request: %v", errAppend)
	}
	if errAppend := timelineSource.AppendPart([]byte("Timestamp: 2026-05-25T12:00:01Z\nEvent: websocket.response\n{}")); errAppend != nil {
		t.Fatalf("AppendPart response: %v", errAppend)
	}
	partPaths := timelineSource.Paths()
	for _, path := range partPaths {
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("part path %s is not under logs dir %s", path, logsDir)
		}
	}

	errLog := logger.LogRequestWithOptionsAndSources(
		"/v1/responses/ws",
		http.MethodGet,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		http.StatusSwitchingProtocols,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		nil,
		timelineSource,
		nil,
		nil,
		nil,
		nil,
		nil,
		false,
		"ws-req-1",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndSources error: %v", errLog)
	}

	assertFileBodySourceCleaned(t, partPaths)

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	var logPath string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		logPath = logsDir + string(os.PathSeparator) + entry.Name()
		break
	}
	if logPath == "" {
		t.Fatal("expected local request log file")
	}
	raw, errReadLog := os.ReadFile(logPath)
	if errReadLog != nil {
		t.Fatalf("read log file: %v", errReadLog)
	}
	if !bytes.Contains(raw, []byte("=== WEBSOCKET TIMELINE ===")) {
		t.Fatalf("websocket timeline section missing: %s", string(raw))
	}
	if !bytes.Contains(raw, []byte("Event: websocket.request")) || !bytes.Contains(raw, []byte("Event: websocket.response")) {
		t.Fatalf("merged websocket events missing: %s", string(raw))
	}
}

func TestFileRequestLogger_HomeEnabled_ForwardsSourceLogAndCleansParts(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	timelineSource, errSource := logger.NewFileBodySource("home-websocket-timeline-test")
	if errSource != nil {
		t.Fatalf("logger.NewFileBodySource: %v", errSource)
	}
	if errAppend := timelineSource.AppendPart([]byte("Timestamp: 2026-05-25T12:00:00Z\nEvent: websocket.request\n{}")); errAppend != nil {
		t.Fatalf("AppendPart request: %v", errAppend)
	}
	partPaths := timelineSource.Paths()
	for _, path := range partPaths {
		if !strings.HasPrefix(path, logsDir+string(os.PathSeparator)) {
			t.Fatalf("part path %s is not under logs dir %s", path, logsDir)
		}
	}

	errLog := logger.LogRequestWithOptionsAndSources(
		"/v1/responses/ws",
		http.MethodGet,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		http.StatusSwitchingProtocols,
		map[string][]string{"Upgrade": {"websocket"}},
		nil,
		nil,
		timelineSource,
		nil,
		nil,
		nil,
		nil,
		nil,
		false,
		"home-ws-req-1",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptionsAndSources error: %v", errLog)
	}
	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		RequestID  string `json:"request_id"`
		RequestLog string `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.RequestID != "home-ws-req-1" {
		t.Fatalf("request_id = %q, want home-ws-req-1", got.RequestID)
	}
	if !strings.Contains(got.RequestLog, "Event: websocket.request") {
		t.Fatalf("forwarded request_log missing websocket request: %s", got.RequestLog)
	}
	assertFileBodySourceCleaned(t, partPaths)
}

func TestFileRequestLogger_HomeEnabled_ForwardsStreamingRequestID(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	writer, errLog := logger.LogStreamingRequest(
		"/v1/responses",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		"stream-req-1",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}

	if errStatus := writer.WriteStatus(http.StatusOK, map[string][]string{"Content-Type": {"text/event-stream"}}); errStatus != nil {
		t.Fatalf("WriteStatus error: %v", errStatus)
	}
	writer.WriteChunkAsync([]byte("data: ok\n\n"))
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close error: %v", errClose)
	}

	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got struct {
		RequestID  string `json:"request_id"`
		RequestLog string `json:"request_log"`
	}
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v payload=%s", errUnmarshal, string(stub.pushed[0]))
	}
	if got.RequestID != "stream-req-1" {
		t.Fatalf("request_id = %q, want stream-req-1", got.RequestID)
	}
	if got.RequestLog == "" {
		t.Fatalf("request_log empty, want non-empty")
	}
}

func TestFileRequestLogger_HomeEnabled_DoesNotForwardForcedErrorLogsWhenRequestLogDisabled(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	errLog := logger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		true,
		"req-2",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequestWithOptions error: %v", errLog)
	}

	if len(stub.pushed) != 0 {
		t.Fatalf("home pushed records = %d, want 0", len(stub.pushed))
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("failed to read logs dir: %v", errRead)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Name() != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected local forced error log file when request-log disabled")
	}
}

func TestHomeStreamingLogWriter_CloseTerminatesWhenClientUnhealthy(t *testing.T) {
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()

	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)
	logger.SetHomeEnabled(true)

	writer, errLog := logger.LogStreamingRequest(
		"/v1/responses",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		"stream-req-unhealthy",
	)
	if errLog != nil {
		t.Fatalf("LogStreamingRequest error: %v", errLog)
	}

	hw, ok := writer.(*homeStreamingLogWriter)
	if !ok {
		t.Fatalf("expected *homeStreamingLogWriter, got %T", writer)
	}

	hw.WriteChunkAsync([]byte("chunk-1"))

	// Mark client unhealthy before calling Close()
	stub.heartbeatOK = false

	done := make(chan struct{})
	go func() {
		_ = hw.Close()
		// If Close properly closes chunkChan and waits for doneChan, doneChan must be closed.
		<-hw.doneChan
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("homeStreamingLogWriter leaked writer goroutine after Close with unhealthy client")
	}
}

func TestFileRequestLogger_UsesShortRequestIDForLocalAndFullForHome(t *testing.T) {
	fullUUID := "018f3a5b-1234-7abc-def0-12345678abcd"
	expectedShortID := "5678abcd"

	// 1. Local file log filename ends with short request ID
	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 0)

	errLog := logger.LogRequest(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusOK,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"ok":true}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		fullUUID,
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("LogRequest failed: %v", errLog)
	}

	entries, errRead := os.ReadDir(logsDir)
	if errRead != nil {
		t.Fatalf("ReadDir failed: %v", errRead)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 log file, got %d", len(entries))
	}
	filename := entries[0].Name()
	expectedSuffix := "-" + expectedShortID + ".log"
	if !strings.HasSuffix(filename, expectedSuffix) {
		t.Fatalf("filename %q does not have expected suffix %q", filename, expectedSuffix)
	}
	if strings.Contains(filename, fullUUID) {
		t.Fatalf("filename %q should not contain full UUID", filename)
	}

	// 2. Home forwarded payload preserves full UUID across internal communication
	original := currentHomeRequestLogClient
	defer func() {
		currentHomeRequestLogClient = original
	}()
	stub := &stubHomeRequestLogClient{heartbeatOK: true}
	currentHomeRequestLogClient = func() homeRequestLogClient {
		return stub
	}

	homeLogger := NewFileRequestLogger(true, logsDir, "", 0)
	homeLogger.SetHomeEnabled(true)

	errHomeLog := homeLogger.LogRequest(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusOK,
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"ok":true}`),
		nil,
		nil,
		nil,
		nil,
		nil,
		fullUUID,
		time.Now(),
		time.Now(),
	)
	if errHomeLog != nil {
		t.Fatalf("home LogRequest failed: %v", errHomeLog)
	}

	if len(stub.pushed) != 1 {
		t.Fatalf("home pushed records = %d, want 1", len(stub.pushed))
	}

	var got homeRequestLogPayload
	if errUnmarshal := json.Unmarshal(stub.pushed[0], &got); errUnmarshal != nil {
		t.Fatalf("unmarshal payload failed: %v", errUnmarshal)
	}
	if got.RequestID != fullUUID {
		t.Fatalf("home payload request_id = %q, want full UUID %q", got.RequestID, fullUUID)
	}
}
