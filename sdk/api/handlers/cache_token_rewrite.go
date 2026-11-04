package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	if r == nil || !json.Valid(data) {
		return data
	}
	root := gjson.ParseBytes(data)
	if root.IsArray() {
		for index, item := range root.Array() {
			rewritten := r.rewriteJSON([]byte(item.Raw))
			if !bytes.Equal(rewritten, []byte(item.Raw)) {
				data, _ = sjson.SetRawBytes(data, jsonIndex(index), rewritten)
			}
		}
		return data
	}
	if root.Get("type").String() == "message_start" {
		r.claudeInput, r.claudeRead, r.claudeWrite, r.claudeKnown = 0, 0, 0, false
		r.claudeRewrite, r.claudeCached = false, 0
	}
	for _, path := range []string{"usage", "response.usage", "message.usage", "interaction.usage", "metadata.total_usage", "usageMetadata", "response.usageMetadata"} {
		u := root.Get(path)
		if !u.IsObject() {
			continue
		}
		set := func(key string, value int64) { data, _ = sjson.SetBytes(data, path+"."+key, value) }
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
				if value.Type == gjson.Number {
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
			if v := u.Get("input_tokens"); v.Type == gjson.Number {
				r.claudeInput = v.Int()
				r.claudeKnown = true
			}
			cacheRead := u.Get("cache_read_input_tokens")
			if cacheRead.Type == gjson.Number {
				r.claudeRead = cacheRead.Int()
			}
			if v := u.Get("cache_creation_input_tokens"); v.Type == gjson.Number {
				r.claudeWrite = v.Int()
			}
			if cacheRead.Type == gjson.Number && r.claudeKnown {
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
		case u.Get("prompt_tokens").Type == gjson.Number:
			cached, rewrite := targetCached(u.Get("prompt_tokens").Int())
			if !rewrite {
				continue
			}
			set("prompt_tokens_details.cached_tokens", cached)
			setCacheAliases(cached)
			zeroExisting("prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cached_creation_tokens")
		case u.Get("total_input_tokens").Type == gjson.Number:
			cached, rewrite := targetCached(u.Get("total_input_tokens").Int())
			if !rewrite {
				continue
			}
			for _, key := range []string{"total_cached_tokens", "cached_tokens"} {
				if u.Get(key).Type == gjson.Number {
					set(key, cached)
				}
			}
		case u.Get("input_tokens").Type == gjson.Number:
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
		case u.Get("promptTokenCount").Type == gjson.Number:
			cached, rewrite := targetCached(u.Get("promptTokenCount").Int())
			if !rewrite {
				continue
			}
			set("cachedContentTokenCount", cached)
		}
	}
	return data
}

func jsonIndex(index int) string {
	data, _ := json.Marshal(index)
	return string(data)
}

func (r *cacheTokenRewriter) rewriteFrame(frame []byte) []byte {
	payload, found := sseJSONValidationDataPayload(frame)
	if !found || !json.Valid(bytes.TrimSpace(payload)) {
		return frame
	}
	rewritten := r.rewriteJSON(payload)
	if bytes.Equal(payload, rewritten) {
		return frame
	}
	// SSE payload lines need their own data: prefix. Compact multiline JSON
	// before replacing several data lines with a single one.
	if bytes.IndexAny(rewritten, "\r\n") >= 0 {
		var compact bytes.Buffer
		_ = json.Compact(&compact, rewritten)
		rewritten = compact.Bytes()
	}
	lines := bytes.Split(frame, []byte("\n"))
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
			if bytes.HasSuffix(line, []byte("\r")) {
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

// Chunks may be standalone JSON, SSE frames, or arbitrary fragments of either.
func (r *cacheTokenRewriter) rewriteChunk(chunk []byte) []byte {
	// Native executors can pass scanner lines without their newline. Preserve
	// the boundary between SSE fields before buffering the JSON data fragment.
	if len(r.pending) > 0 && !bytes.HasSuffix(r.pending, []byte("\n")) && !bytes.HasPrefix(chunk, []byte("\n")) {
		first := bytes.TrimSpace(bytes.SplitN(chunk, []byte("\n"), 2)[0])
		if bytes.HasPrefix(first, []byte("data:")) || bytes.HasPrefix(first, []byte("event:")) {
			if _, found := sseJSONValidationDataPayload(r.pending); !found {
				r.pending = append(r.pending, '\n')
			}
		}
	}
	r.pending = append(r.pending, chunk...)
	if json.Valid(bytes.TrimSpace(r.pending)) {
		out := r.rewriteJSON(r.pending)
		r.pending = nil
		return out
	}
	var out []byte
	for {
		end, delimiter := -1, 0
		for _, separator := range [][]byte{[]byte("\n\n"), []byte("\r\n\r\n")} {
			if i := bytes.Index(r.pending, separator); i >= 0 && (end < 0 || i < end) {
				end, delimiter = i, len(separator)
			}
		}
		if end < 0 {
			break
		}
		end += delimiter
		out = append(out, r.rewriteFrame(r.pending[:end])...)
		r.pending = bytes.Clone(r.pending[end:])
	}
	payload, found := sseJSONValidationDataPayload(r.pending)
	if (found && (json.Valid(bytes.TrimSpace(payload)) || bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")))) || len(bytes.TrimSpace(r.pending)) == 0 {
		out = append(out, r.rewriteFrame(r.pending)...)
		r.pending = nil
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
