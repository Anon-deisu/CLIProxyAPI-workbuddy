package handlers

import (
	"bytes"
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// cacheTokenRewriter belongs to one downstream request, not to an upstream attempt.
type cacheTokenRewriter struct {
	ratio                                float64
	format                               string
	pending                              []byte
	claudeInput, claudeRead, claudeWrite int64
	claudeKnown                          bool
	claudeRewrite                        bool
	claudeCached                         int64
}

func newCacheTokenRewriter(cfg *config.SDKConfig, format string) *cacheTokenRewriter {
	if cfg == nil || cfg.CacheTokenRewrite == nil || !cfg.CacheTokenRewrite.Enabled {
		return nil
	}
	settings := cfg.CacheTokenRewrite
	if err := settings.Validate(); err != nil {
		return nil
	}
	percent := *settings.MinPercent
	if settings.MaxPercent != nil {
		percent += rand.Float64() * (*settings.MaxPercent - percent)
	}
	return &cacheTokenRewriter{ratio: percent / 100, format: format}
}

func (r *cacheTokenRewriter) rewriteJSON(data []byte) []byte {
	if r == nil || !ojson.ValidBytes(data) {
		return data
	}
	return r.rewriteValidJSON(data)
}

// rewriteValidJSON is the exit for one validated payload, including arrays.
// Read-only views share the input; a Doc is created only for an actual mutation.
func (r *cacheTokenRewriter) rewriteValidJSON(data []byte) []byte {
	var doc *ojson.Doc
	r.rewriteJSONValue(ojson.GetBytesNoCopy(data, "@this"), "", data, &doc)
	if doc == nil {
		return data
	}
	return doc.Bytes()
}

func (r *cacheTokenRewriter) rewriteJSONValue(root ojson.Result, prefix string, data []byte, doc **ojson.Doc) {
	if root.IsArray() {
		root.ForEach(func(index, item ojson.Result) bool {
			r.rewriteJSONValue(item, prefix+strconv.FormatInt(index.Int(), 10)+".", data, doc)
			return true
		})
		return
	}
	if !root.IsObject() {
		return
	}
	var hasUsage, messageStart, seenType bool
	root.ForEach(func(key, value ojson.Result) bool {
		switch key.Str {
		case "type":
			if !seenType {
				messageStart = value.String() == "message_start"
				seenType = true
			}
		case "usage", "usageMetadata":
			hasUsage = hasUsage || value.IsObject()
		case "response":
			hasUsage = hasUsage || (value.IsObject() && (value.Get("usage").IsObject() || value.Get("usageMetadata").IsObject()))
		case "message", "interaction":
			hasUsage = hasUsage || (value.IsObject() && value.Get("usage").IsObject())
		case "metadata":
			hasUsage = hasUsage || (value.IsObject() && value.Get("total_usage").IsObject())
		}
		return true
	})
	if messageStart {
		r.claudeInput, r.claudeRead, r.claudeWrite, r.claudeKnown = 0, 0, 0, false
		r.claudeRewrite, r.claudeCached = false, 0
	}
	if !hasUsage {
		return
	}
	for _, path := range []string{"usage", "response.usage", "message.usage", "interaction.usage", "metadata.total_usage", "usageMetadata", "response.usageMetadata"} {
		u := root.Get(path)
		if !u.IsObject() {
			continue
		}
		set := func(key string, value int64) {
			if *doc == nil {
				*doc = ojson.NewDoc(data)
			}
			_ = (*doc).Set(prefix+path+"."+key, value)
		}
		zeroExisting := func(keys ...string) {
			for _, key := range keys {
				if u.Get(key).Exists() {
					set(key, 0)
				}
			}
		}
		setCacheAliases := func(cached int64) {
			for _, key := range []string{"cached_tokens", "cache_read_input_tokens"} {
				if u.Get(key).Exists() {
					set(key, cached)
				}
			}
			zeroExisting("cache_creation_input_tokens", "cache_creation.ephemeral_5m_input_tokens", "cache_creation.ephemeral_1h_input_tokens")
		}
		cachedRead := func() (int64, bool) {
			for _, key := range []string{"input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens", "cached_tokens", "cache_read_input_tokens", "cachedContentTokenCount", "total_cached_tokens"} {
				value := u.Get(key)
				if value.Type == ojson.Number {
					return value.Int(), true
				}
			}
			return 0, false
		}
		targetCached := func(total int64) (int64, bool) {
			original, exists := cachedRead()
			target := int64(float64(total) * r.ratio)
			return target, exists && original > target
		}
		switch {
		case r.format == "claude":
			if v := u.Get("input_tokens"); v.Type == ojson.Number {
				r.claudeInput = v.Int()
				r.claudeKnown = true
			}
			cacheRead := u.Get("cache_read_input_tokens")
			if cacheRead.Type == ojson.Number {
				r.claudeRead = cacheRead.Int()
			}
			if v := u.Get("cache_creation_input_tokens"); v.Type == ojson.Number {
				r.claudeWrite = v.Int()
			}
			if cacheRead.Type == ojson.Number && r.claudeKnown {
				total := r.claudeInput + r.claudeRead + r.claudeWrite
				r.claudeCached = int64(float64(total) * r.ratio)
				r.claudeRewrite = cacheRead.Int() > r.claudeCached
			}
			if !r.claudeKnown || !r.claudeRewrite {
				continue
			}
			total := r.claudeInput + r.claudeRead + r.claudeWrite
			cached := r.claudeCached
			set("input_tokens", total-cached)
			set("cache_read_input_tokens", cached)
			set("cache_creation_input_tokens", 0)
			zeroExisting("cache_creation.ephemeral_5m_input_tokens", "cache_creation.ephemeral_1h_input_tokens")
		case u.Get("prompt_tokens").Type == ojson.Number:
			cached, rewrite := targetCached(u.Get("prompt_tokens").Int())
			if !rewrite {
				continue
			}
			set("prompt_tokens_details.cached_tokens", cached)
			setCacheAliases(cached)
			zeroExisting("prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cached_creation_tokens")
		case u.Get("total_input_tokens").Type == ojson.Number:
			cached, rewrite := targetCached(u.Get("total_input_tokens").Int())
			if !rewrite {
				continue
			}
			for _, key := range []string{"total_cached_tokens", "cached_tokens"} {
				if u.Get(key).Type == ojson.Number {
					set(key, cached)
				}
			}
		case u.Get("input_tokens").Type == ojson.Number:
			cached, rewrite := targetCached(u.Get("input_tokens").Int())
			if !rewrite {
				continue
			}
			setCacheAliases(cached)
			if r.format == "interactions" {
				set("cached_tokens", cached)
			} else {
				set("input_tokens_details.cached_tokens", cached)
			}
			if u.Get("input_token_details").Exists() {
				set("input_token_details.cached_tokens", cached)
			}
			zeroExisting("input_tokens_details.cache_write_tokens", "input_tokens_details.cache_creation_tokens", "input_tokens_details.cached_creation_tokens")
		case u.Get("promptTokenCount").Type == ojson.Number:
			cached, rewrite := targetCached(u.Get("promptTokenCount").Int())
			if !rewrite {
				continue
			}
			set("cachedContentTokenCount", cached)
		}
	}
}

func (r *cacheTokenRewriter) rewriteFrame(frame []byte) []byte {
	payload, found := cacheTokenSSEPayload(frame)
	if !found || !ojson.ValidBytes(payload) {
		return frame
	}
	return r.rewriteValidFrame(frame, payload)
}

func (r *cacheTokenRewriter) rewriteValidFrame(frame, payload []byte) []byte {
	rewritten := r.rewriteValidJSON(payload)
	if bytes.Equal(payload, rewritten) {
		return frame
	}
	// SSE payload lines need their own data: prefix. Compact multiline JSON
	// before replacing several data lines with a single one.
	if bytes.IndexAny(rewritten, "\r\n") >= 0 {
		rewritten = []byte(ojson.GetBytesNoCopy(rewritten, "@ugly").Raw)
	}
	lines := bytes.Split(frame, []byte("\n"))
	// The collapsed data line inherits the last line's ending. A chunk may end
	// before that line's CRLF even when earlier data lines already include CR.
	lastDataCR := false
	for _, line := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			lastDataCR = bytes.HasSuffix(line, []byte("\r"))
		}
	}
	out := make([][]byte, 0, len(lines))
	inserted := false
	for _, line := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			if inserted {
				continue
			}
			prefix := line[:bytes.Index(line, []byte("data:"))+5]
			newLine := append(bytes.Clone(prefix), ' ')
			newLine = append(newLine, rewritten...)
			if lastDataCR {
				newLine = append(newLine, '\r')
			}
			out = append(out, newLine)
			inserted = true
		} else {
			out = append(out, line)
		}
	}
	return bytes.Join(out, []byte("\n"))
}

// cacheTokenSSEPayload borrows single-line data and joins only explicitly framed
// data fields. It never treats an unframed continuation as another SSE field.
func cacheTokenSSEPayload(frame []byte) ([]byte, bool) {
	var payload, joined []byte
	found := false
	for line := range bytes.Lines(frame) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if found {
			if joined == nil {
				joined = make([]byte, 0, len(payload)+len(data)+1)
				joined = append(joined, payload...)
			}
			joined = append(joined, '\n')
			joined = append(joined, data...)
			payload = joined
		} else {
			payload = data
			found = true
		}
	}
	return payload, found
}

func cacheTokenSSEFrameEnd(data []byte) int {
	for i, c := range data {
		if c == '\n' && ((i > 0 && data[i-1] == '\n') ||
			(i >= 3 && data[i-1] == '\r' && data[i-2] == '\n' && data[i-3] == '\r')) {
			return i + 1
		}
	}
	return -1
}

// Chunks may be standalone JSON, SSE frames, or arbitrary fragments of either.
func (r *cacheTokenRewriter) rewriteChunk(chunk []byte) []byte {
	// Native executors can pass scanner lines without their newline. Preserve
	// the boundary between SSE fields before buffering the JSON data fragment.
	if len(r.pending) > 0 && !bytes.HasSuffix(r.pending, []byte("\n")) && !bytes.HasPrefix(chunk, []byte("\n")) {
		first, _, _ := bytes.Cut(chunk, []byte("\n"))
		first = bytes.TrimSpace(first)
		if bytes.HasPrefix(first, []byte("data:")) || bytes.HasPrefix(first, []byte("event:")) {
			if _, found := cacheTokenSSEPayload(r.pending); !found {
				r.pending = append(r.pending, '\n')
			}
		}
	}
	r.pending = append(r.pending, chunk...)
	trimmed := bytes.TrimSpace(r.pending)
	if len(trimmed) > 0 {
		switch trimmed[0] {
		case '{', '[', '"', 't', 'f', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if ojson.ValidBytes(trimmed) {
				out := r.rewriteValidJSON(r.pending)
				r.pending = nil
				return out
			}
		}
	}
	var out []byte
	consumed := 0
	for {
		end := cacheTokenSSEFrameEnd(r.pending[consumed:])
		if end < 0 {
			break
		}
		if out == nil {
			out = make([]byte, 0, len(r.pending))
		}
		end += consumed
		out = append(out, r.rewriteFrame(r.pending[consumed:end])...)
		consumed = end
	}
	tail := r.pending[consumed:]
	payload, found := cacheTokenSSEPayload(tail)
	if len(bytes.TrimSpace(tail)) == 0 || (found && bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]"))) {
		out = append(out, tail...)
		r.pending = nil
	} else if found && ojson.ValidBytes(payload) {
		out = append(out, r.rewriteValidFrame(tail, payload)...)
		r.pending = nil
	} else if consumed > 0 {
		// Detach the unfinished suffix once, after all complete frames have been
		// consumed. Returned bytes never alias a buffer reused by a later chunk.
		r.pending = bytes.Clone(tail)
	}
	return out
}

func (r *cacheTokenRewriter) finish() []byte {
	data := r.pending
	r.pending = nil
	return r.rewriteFrame(data)
}

func (r *cacheTokenRewriter) downstreamHeaders(headers http.Header) http.Header {
	if r == nil {
		return headers
	}
	cloned := headers.Clone()
	cloned.Del("Content-Length")
	return cloned
}

func (r *cacheTokenRewriter) wrapStream(ctx context.Context, data <-chan []byte, errs <-chan *interfaces.ErrorMessage) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
	if r == nil || data == nil {
		return data, errs
	}
	out := make(chan []byte)
	errorOut := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		defer close(out)
		defer close(errorOut)
		var terminal *interfaces.ErrorMessage
		send := func(payload []byte) bool {
			if len(payload) == 0 {
				return true
			}
			select {
			case out <- payload:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for data != nil || errs != nil {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-data:
				if !ok {
					data = nil
					continue
				}
				if !send(r.rewriteChunk(payload)) {
					return
				}
			case errMsg, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if errMsg != nil {
					terminal = errMsg
				}
			}
		}
		if !send(r.finish()) {
			return
		}
		if terminal != nil {
			errorOut <- terminal
		}
	}()
	return out, errorOut
}
