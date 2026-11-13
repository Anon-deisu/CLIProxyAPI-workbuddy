package logging

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

func (l *FileRequestLogger) writeNonStreamingLog(
	w io.Writer,
	url, method string,
	requestHeaders map[string][]string,
	requestBody []byte,
	requestBodyPath string,
	websocketTimeline []byte,
	websocketTimelineSource *FileBodySource,
	apiRequest []byte,
	apiRequestSource *FileBodySource,
	apiResponse []byte,
	apiResponseSource *FileBodySource,
	apiWebsocketTimeline []byte,
	apiWebsocketTimelineSource *FileBodySource,
	apiResponseErrors []*interfaces.ErrorMessage,
	statusCode int,
	responseHeaders map[string][]string,
	response []byte,
	requestTimestamp time.Time,
	apiResponseTimestamp time.Time,
) error {
	if requestTimestamp.IsZero() {
		requestTimestamp = time.Now()
	}
	isWebsocketTranscript := hasSectionPayload(websocketTimeline) || hasFileBodySourcePayload(websocketTimelineSource)
	downstreamTransport := inferDownstreamTransport(requestHeaders, websocketTimeline, websocketTimelineSource)
	upstreamTransport := inferUpstreamTransport(apiRequest, apiRequestSource, apiResponse, apiResponseSource, apiWebsocketTimeline, apiWebsocketTimelineSource, apiResponseErrors)
	if errWrite := writeRequestInfoWithBody(w, url, method, requestHeaders, requestBody, requestBodyPath, requestTimestamp, downstreamTransport, upstreamTransport, !isWebsocketTranscript); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(w, "=== WEBSOCKET TIMELINE ===\n", "=== WEBSOCKET TIMELINE", websocketTimeline, websocketTimelineSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPISectionWithSource(w, "=== API WEBSOCKET TIMELINE ===\n", "=== API WEBSOCKET TIMELINE", apiWebsocketTimeline, apiWebsocketTimelineSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(w, "=== API REQUEST ===\n", "=== API REQUEST", apiRequest, apiRequestSource, time.Time{}); errWrite != nil {
		return errWrite
	}
	if errWrite := writeAPIErrorResponses(w, apiResponseErrors); errWrite != nil {
		return errWrite
	}
	if errWrite := writePreformattedAPISectionWithSource(w, "=== API RESPONSE ===\n", "=== API RESPONSE", apiResponse, apiResponseSource, apiResponseTimestamp); errWrite != nil {
		return errWrite
	}
	if isWebsocketTranscript {
		// Intentionally omit the generic downstream HTTP response section for websocket
		// transcripts. The durable session exchange is captured in WEBSOCKET TIMELINE,
		// and appending a one-off upgrade response snapshot would dilute that transcript.
		return nil
	}
	return writeDecodedResponseSection(w, statusCode, responseHeaders, response)
}

func writeRequestInfoWithBody(
	w io.Writer,
	url, method string,
	headers map[string][]string,
	body []byte,
	bodyPath string,
	timestamp time.Time,
	downstreamTransport string,
	upstreamTransport string,
	includeBody bool,
) error {
	if _, errWrite := io.WriteString(w, "=== REQUEST INFO ===\n"); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Version: %s\n", buildinfo.Version)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("URL: %s\n", url)); errWrite != nil {
		return errWrite
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Method: %s\n", method)); errWrite != nil {
		return errWrite
	}
	if strings.TrimSpace(downstreamTransport) != "" {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Downstream Transport: %s\n", downstreamTransport)); errWrite != nil {
			return errWrite
		}
	}
	if strings.TrimSpace(upstreamTransport) != "" {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Upstream Transport: %s\n", upstreamTransport)); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
		return errWrite
	}
	if errWrite := writeSectionSpacing(w, 1); errWrite != nil {
		return errWrite
	}

	if _, errWrite := io.WriteString(w, "=== HEADERS ===\n"); errWrite != nil {
		return errWrite
	}
	for key, values := range headers {
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, masked)); errWrite != nil {
				return errWrite
			}
		}
	}
	if errWrite := writeSectionSpacing(w, 1); errWrite != nil {
		return errWrite
	}

	if !includeBody {
		return nil
	}

	if _, errWrite := io.WriteString(w, "=== REQUEST BODY ===\n"); errWrite != nil {
		return errWrite
	}

	bodyTrailingNewlines := 1
	if bodyPath != "" {
		bodyFile, errOpen := os.Open(bodyPath)
		if errOpen != nil {
			return errOpen
		}
		tracker := &trailingNewlineTrackingWriter{writer: w}
		written, errCopy := io.Copy(tracker, bodyFile)
		if errCopy != nil {
			_ = bodyFile.Close()
			return errCopy
		}
		if written > 0 {
			bodyTrailingNewlines = tracker.trailingNewlines
		}
		if errClose := bodyFile.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close request body temp file")
		}
	} else if _, errWrite := w.Write(body); errWrite != nil {
		return errWrite
	} else if len(body) > 0 {
		bodyTrailingNewlines = countTrailingNewlinesBytes(body)
	}
	if errWrite := writeSectionSpacing(w, bodyTrailingNewlines); errWrite != nil {
		return errWrite
	}
	return nil
}

func countTrailingNewlinesBytes(payload []byte) int {
	count := 0
	for i := len(payload) - 1; i >= 0; i-- {
		if payload[i] != '\n' {
			break
		}
		count++
	}
	return count
}

func writeSectionSpacing(w io.Writer, trailingNewlines int) error {
	missingNewlines := 3 - trailingNewlines
	if missingNewlines <= 0 {
		return nil
	}
	_, errWrite := io.WriteString(w, strings.Repeat("\n", missingNewlines))
	return errWrite
}

type trailingNewlineTrackingWriter struct {
	writer           io.Writer
	trailingNewlines int
}

func (t *trailingNewlineTrackingWriter) Write(payload []byte) (int, error) {
	written, errWrite := t.writer.Write(payload)
	if written > 0 {
		writtenPayload := payload[:written]
		trailingNewlines := countTrailingNewlinesBytes(writtenPayload)
		if trailingNewlines == len(writtenPayload) {
			t.trailingNewlines += trailingNewlines
		} else {
			t.trailingNewlines = trailingNewlines
		}
	}
	return written, errWrite
}

func hasSectionPayload(payload []byte) bool {
	return len(bytes.TrimSpace(payload)) > 0
}

func hasFileBodySourcePayload(source *FileBodySource) bool {
	return source != nil && source.HasPayload()
}

func inferDownstreamTransport(headers map[string][]string, websocketTimeline []byte, websocketTimelineSource *FileBodySource) string {
	if hasSectionPayload(websocketTimeline) || hasFileBodySourcePayload(websocketTimelineSource) {
		return "websocket"
	}
	for key, values := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "Upgrade") {
			for _, value := range values {
				if strings.EqualFold(strings.TrimSpace(value), "websocket") {
					return "websocket"
				}
			}
		}
	}
	return "http"
}

func inferUpstreamTransport(apiRequest []byte, apiRequestSource *FileBodySource, apiResponse []byte, apiResponseSource *FileBodySource, apiWebsocketTimeline []byte, apiWebsocketTimelineSource *FileBodySource, _ []*interfaces.ErrorMessage) string {
	hasHTTP := hasSectionPayload(apiRequest) || hasFileBodySourcePayload(apiRequestSource) || hasSectionPayload(apiResponse) || hasFileBodySourcePayload(apiResponseSource)
	hasWS := hasSectionPayload(apiWebsocketTimeline) || hasFileBodySourcePayload(apiWebsocketTimelineSource)
	switch {
	case hasHTTP && hasWS:
		return "websocket+http"
	case hasWS:
		return "websocket"
	case hasHTTP:
		return "http"
	default:
		return ""
	}
}

func writeLogPart(w io.Writer, payload []byte, prependNewline bool) error {
	if w == nil {
		return nil
	}
	if prependNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := w.Write(payload); errWrite != nil {
		return errWrite
	}
	if !bytes.HasSuffix(payload, []byte("\n")) {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeAPISection(w io.Writer, sectionHeader string, sectionPrefix string, payload []byte, timestamp time.Time) error {
	if len(payload) == 0 {
		return nil
	}

	if bytes.HasPrefix(payload, []byte(sectionPrefix)) {
		if _, errWrite := w.Write(payload); errWrite != nil {
			return errWrite
		}
	} else {
		if _, errWrite := io.WriteString(w, sectionHeader); errWrite != nil {
			return errWrite
		}
		if !timestamp.IsZero() {
			if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
				return errWrite
			}
		}
		if _, errWrite := w.Write(payload); errWrite != nil {
			return errWrite
		}
	}

	if errWrite := writeSectionSpacing(w, countTrailingNewlinesBytes(payload)); errWrite != nil {
		return errWrite
	}
	return nil
}

func writeAPISectionWithSource(w io.Writer, sectionHeader string, sectionPrefix string, payload []byte, source *FileBodySource, timestamp time.Time) error {
	if !hasFileBodySourcePayload(source) {
		return writeAPISection(w, sectionHeader, sectionPrefix, payload, timestamp)
	}
	if len(payload) > 0 {
		if errWrite := writeAPISection(w, sectionHeader, sectionPrefix, payload, timestamp); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := io.WriteString(w, sectionHeader); errWrite != nil {
		return errWrite
	}
	if !timestamp.IsZero() {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Timestamp: %s\n", timestamp.Format(time.RFC3339Nano))); errWrite != nil {
			return errWrite
		}
	}
	tracker := &trailingNewlineTrackingWriter{writer: w}
	if _, errWrite := source.WriteTo(tracker); errWrite != nil {
		return errWrite
	}
	if errWrite := writeSectionSpacing(w, tracker.trailingNewlines); errWrite != nil {
		return errWrite
	}
	return nil
}

func writePreformattedAPISectionWithSource(w io.Writer, sectionHeader string, sectionPrefix string, payload []byte, source *FileBodySource, timestamp time.Time) error {
	if !hasFileBodySourcePayload(source) {
		return writeAPISection(w, sectionHeader, sectionPrefix, payload, timestamp)
	}
	if len(payload) > 0 {
		if errWrite := writeAPISection(w, sectionHeader, sectionPrefix, payload, timestamp); errWrite != nil {
			return errWrite
		}
	}
	tracker := &trailingNewlineTrackingWriter{writer: w}
	if _, errWrite := source.WriteTo(tracker); errWrite != nil {
		return errWrite
	}
	if errWrite := writeSectionSpacing(w, tracker.trailingNewlines); errWrite != nil {
		return errWrite
	}
	return nil
}

func writeAPIErrorResponses(w io.Writer, apiResponseErrors []*interfaces.ErrorMessage) error {
	for i := 0; i < len(apiResponseErrors); i++ {
		if apiResponseErrors[i] == nil {
			continue
		}
		if _, errWrite := io.WriteString(w, "=== API ERROR RESPONSE ===\n"); errWrite != nil {
			return errWrite
		}
		if _, errWrite := io.WriteString(w, fmt.Sprintf("HTTP Status: %d\n", apiResponseErrors[i].StatusCode)); errWrite != nil {
			return errWrite
		}
		trailingNewlines := 1
		if apiResponseErrors[i].Error != nil {
			errText := apiResponseErrors[i].Error.Error()
			if _, errWrite := io.WriteString(w, errText); errWrite != nil {
				return errWrite
			}
			if errText != "" {
				trailingNewlines = countTrailingNewlinesBytes([]byte(errText))
			}
		}
		if errWrite := writeSectionSpacing(w, trailingNewlines); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeResponseSection(w io.Writer, statusCode int, statusWritten bool, responseHeaders map[string][]string, responseReader io.Reader, decompressErr error, trailingNewline bool) error {
	if _, errWrite := io.WriteString(w, "=== RESPONSE ===\n"); errWrite != nil {
		return errWrite
	}
	if statusWritten {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("Status: %d\n", statusCode)); errWrite != nil {
			return errWrite
		}
	}

	if responseHeaders != nil {
		for key, values := range responseHeaders {
			for _, value := range values {
				if _, errWrite := io.WriteString(w, fmt.Sprintf("%s: %s\n", key, value)); errWrite != nil {
					return errWrite
				}
			}
		}
	}

	var bufferedReader *bufio.Reader
	if responseReader != nil {
		bufferedReader = bufio.NewReader(responseReader)
	}
	if !responseBodyStartsWithLeadingNewline(bufferedReader) {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}

	if bufferedReader != nil {
		if _, errCopy := io.Copy(w, bufferedReader); errCopy != nil {
			return errCopy
		}
	}
	if decompressErr != nil {
		if _, errWrite := io.WriteString(w, fmt.Sprintf("\n[DECOMPRESSION ERROR: %v]", decompressErr)); errWrite != nil {
			return errWrite
		}
	}

	if trailingNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func responseBodyStartsWithLeadingNewline(reader *bufio.Reader) bool {
	if reader == nil {
		return false
	}
	if peeked, _ := reader.Peek(2); len(peeked) >= 2 && peeked[0] == '\r' && peeked[1] == '\n' {
		return true
	}
	if peeked, _ := reader.Peek(1); len(peeked) >= 1 && peeked[0] == '\n' {
		return true
	}
	return false
}

// formatLogContent creates the complete log content for non-streaming requests.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - headers: The request headers
//   - body: The request body
//   - websocketTimeline: The downstream websocket event timeline
//   - apiRequest: The API request data
//   - apiResponse: The API response data
//   - response: The raw response data
//   - status: The response status code
//   - responseHeaders: The response headers
//
// Returns:
//   - string: The formatted log content
func (l *FileRequestLogger) formatLogContent(url, method string, headers map[string][]string, body, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline, response []byte, status int, responseHeaders map[string][]string, apiResponseErrors []*interfaces.ErrorMessage) string {
	var content strings.Builder
	isWebsocketTranscript := hasSectionPayload(websocketTimeline)
	downstreamTransport := inferDownstreamTransport(headers, websocketTimeline, nil)
	upstreamTransport := inferUpstreamTransport(apiRequest, nil, apiResponse, nil, apiWebsocketTimeline, nil, apiResponseErrors)

	// Request info
	content.WriteString(l.formatRequestInfo(url, method, headers, body, downstreamTransport, upstreamTransport, !isWebsocketTranscript))

	if len(websocketTimeline) > 0 {
		if bytes.HasPrefix(websocketTimeline, []byte("=== WEBSOCKET TIMELINE")) {
			content.Write(websocketTimeline)
			if !bytes.HasSuffix(websocketTimeline, []byte("\n")) {
				content.WriteString("\n")
			}
		} else {
			content.WriteString("=== WEBSOCKET TIMELINE ===\n")
			content.Write(websocketTimeline)
			content.WriteString("\n")
		}
		content.WriteString("\n")
	}

	if len(apiWebsocketTimeline) > 0 {
		if bytes.HasPrefix(apiWebsocketTimeline, []byte("=== API WEBSOCKET TIMELINE")) {
			content.Write(apiWebsocketTimeline)
			if !bytes.HasSuffix(apiWebsocketTimeline, []byte("\n")) {
				content.WriteString("\n")
			}
		} else {
			content.WriteString("=== API WEBSOCKET TIMELINE ===\n")
			content.Write(apiWebsocketTimeline)
			content.WriteString("\n")
		}
		content.WriteString("\n")
	}

	if len(apiRequest) > 0 {
		if bytes.HasPrefix(apiRequest, []byte("=== API REQUEST")) {
			content.Write(apiRequest)
			if !bytes.HasSuffix(apiRequest, []byte("\n")) {
				content.WriteString("\n")
			}
		} else {
			content.WriteString("=== API REQUEST ===\n")
			content.Write(apiRequest)
			content.WriteString("\n")
		}
		content.WriteString("\n")
	}

	for i := 0; i < len(apiResponseErrors); i++ {
		content.WriteString("=== API ERROR RESPONSE ===\n")
		content.WriteString(fmt.Sprintf("HTTP Status: %d\n", apiResponseErrors[i].StatusCode))
		content.WriteString(apiResponseErrors[i].Error.Error())
		content.WriteString("\n\n")
	}

	if len(apiResponse) > 0 {
		if bytes.HasPrefix(apiResponse, []byte("=== API RESPONSE")) {
			content.Write(apiResponse)
			if !bytes.HasSuffix(apiResponse, []byte("\n")) {
				content.WriteString("\n")
			}
		} else {
			content.WriteString("=== API RESPONSE ===\n")
			content.Write(apiResponse)
			content.WriteString("\n")
		}
		content.WriteString("\n")
	}

	if isWebsocketTranscript {
		// Mirror writeNonStreamingLog: websocket transcripts end with the dedicated
		// timeline sections instead of a generic downstream HTTP response block.
		return content.String()
	}

	// Response section
	content.WriteString("=== RESPONSE ===\n")
	content.WriteString(fmt.Sprintf("Status: %d\n", status))

	if responseHeaders != nil {
		for key, values := range responseHeaders {
			for _, value := range values {
				content.WriteString(fmt.Sprintf("%s: %s\n", key, value))
			}
		}
	}

	content.WriteString("\n")
	content.Write(response)
	content.WriteString("\n")

	return content.String()
}

// responseLogDecoder distinguishes decoding failures from destination failures.
// Only decoding failures trigger the legacy raw-response fallback.
type responseLogDecoder struct {
	reader   io.Reader
	close    func() error
	encoding string
	err      error
}

func (r *responseLogDecoder) Read(p []byte) (int, error) {
	n, errRead := r.reader.Read(p)
	if errRead != nil && errRead != io.EOF {
		r.err = fmt.Errorf("failed to decompress %s data: %w", r.encoding, errRead)
		return n, r.err
	}
	return n, errRead
}

func (r *responseLogDecoder) Close() {
	if r.close != nil {
		if errClose := r.close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close response decoder in request logger")
		}
	}
}

func newResponseLogDecoder(headers map[string][]string, response []byte) (*responseLogDecoder, error) {
	if len(response) == 0 {
		return nil, nil
	}
	var encoding string
	for key, values := range headers {
		if strings.EqualFold(key, "Content-Encoding") && len(values) > 0 {
			encoding = strings.ToLower(values[0])
			break
		}
	}
	raw := bytes.NewReader(response)
	decoder := &responseLogDecoder{encoding: encoding}
	switch encoding {
	case "gzip":
		reader, errNew := gzip.NewReader(raw)
		if errNew != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", errNew)
		}
		decoder.reader, decoder.close = reader, reader.Close
	case "deflate":
		reader := flate.NewReader(raw)
		decoder.reader, decoder.close = reader, reader.Close
	case "br":
		decoder.reader = brotli.NewReader(raw)
		decoder.encoding = "brotli"
	case "zstd":
		reader, errNew := zstd.NewReader(raw, zstd.WithDecoderConcurrency(1))
		if errNew != nil {
			return nil, fmt.Errorf("failed to create zstd reader: %w", errNew)
		}
		decoder.reader = reader
		decoder.close = func() error { reader.Close(); return nil }
	default:
		return nil, nil
	}
	return decoder, nil
}

// responseLogRollback checkpoints only the local log destination. It never
// operates on the downstream HTTP writer or changes compression on the wire.
func responseLogRollback(w io.Writer) (func() error, error) {
	switch dst := w.(type) {
	case *os.File:
		offset, errSeek := dst.Seek(0, io.SeekCurrent)
		if errSeek != nil {
			return nil, errSeek
		}
		return func() error {
			if _, errRewind := dst.Seek(offset, io.SeekStart); errRewind != nil {
				return errRewind
			}
			return dst.Truncate(offset)
		}, nil
	case *bytes.Buffer:
		offset := dst.Len()
		return func() error { dst.Truncate(offset); return nil }, nil
	default:
		return nil, nil
	}
}

func writeDecodedResponseSection(w io.Writer, status int, headers map[string][]string, response []byte) error {
	writeRaw := func(errDecode error) error {
		return writeResponseSection(w, status, true, headers, bytes.NewReader(response), errDecode, true)
	}
	decoder, errNew := newResponseLogDecoder(headers, response)
	if errNew != nil || decoder == nil {
		return writeRaw(errNew)
	}
	rollback, errCheckpoint := responseLogRollback(w)
	if errCheckpoint != nil {
		decoder.Close()
		return errCheckpoint
	}
	if rollback == nil {
		// Arbitrary writers cannot discard a partially decoded response. Validate
		// with bounded memory first; file and Home log destinations use one pass.
		_, errValidate := io.Copy(io.Discard, decoder)
		decoder.Close()
		if errValidate != nil {
			return writeRaw(errValidate)
		}
		decoder, errNew = newResponseLogDecoder(headers, response)
		if errNew != nil {
			return writeRaw(errNew)
		}
	}
	defer decoder.Close()
	errWrite := writeResponseSection(w, status, true, headers, decoder, nil, true)
	if errWrite != nil && errWrite == decoder.err && rollback != nil {
		if errRollback := rollback(); errRollback != nil {
			return fmt.Errorf("failed to roll back decoded response log (%v): %w", errWrite, errRollback)
		}
		return writeRaw(errWrite)
	}
	return errWrite
}

// formatRequestInfo creates the request information section of the log.
//
// Parameters:
//   - url: The request URL
//   - method: The HTTP method
//   - headers: The request headers
//   - body: The request body
//
// Returns:
//   - string: The formatted request information
func (l *FileRequestLogger) formatRequestInfo(url, method string, headers map[string][]string, body []byte, downstreamTransport string, upstreamTransport string, includeBody bool) string {
	var content strings.Builder

	content.WriteString("=== REQUEST INFO ===\n")
	content.WriteString(fmt.Sprintf("Version: %s\n", buildinfo.Version))
	content.WriteString(fmt.Sprintf("URL: %s\n", url))
	content.WriteString(fmt.Sprintf("Method: %s\n", method))
	if strings.TrimSpace(downstreamTransport) != "" {
		content.WriteString(fmt.Sprintf("Downstream Transport: %s\n", downstreamTransport))
	}
	if strings.TrimSpace(upstreamTransport) != "" {
		content.WriteString(fmt.Sprintf("Upstream Transport: %s\n", upstreamTransport))
	}
	content.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	content.WriteString("\n")

	content.WriteString("=== HEADERS ===\n")
	for key, values := range headers {
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			content.WriteString(fmt.Sprintf("%s: %s\n", key, masked))
		}
	}
	content.WriteString("\n")

	if !includeBody {
		return content.String()
	}

	content.WriteString("=== REQUEST BODY ===\n")
	content.Write(body)
	content.WriteString("\n\n")

	return content.String()
}
