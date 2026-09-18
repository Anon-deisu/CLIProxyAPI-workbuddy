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
		switch {
		case r.format == "claude" || u.Get("cache_read_input_tokens").Exists() || u.Get("cache_creation_input_tokens").Exists():
			if v := u.Get("input_tokens"); v.Type == gjson.Number {
				r.claudeInput = v.Int()
				r.claudeKnown = true
			}
			if v := u.Get("cache_read_input_tokens"); v.Type == gjson.Number {
				r.claudeRead = v.Int()
			}
			if v := u.Get("cache_creation_input_tokens"); v.Type == gjson.Number {
				r.claudeWrite = v.Int()
			}
			if !r.claudeKnown {
				continue
			}
			total := r.claudeInput + r.claudeRead + r.claudeWrite
			cached := int64(float64(total) * r.ratio)
			set("input_tokens", total-cached)
			set("cache_read_input_tokens", cached)
			set("cache_creation_input_tokens", 0)
			zeroExisting("cache_creation.ephemeral_5m_input_tokens", "cache_creation.ephemeral_1h_input_tokens")
		case u.Get("prompt_tokens").Type == gjson.Number:
			set("prompt_tokens_details.cached_tokens", int64(float64(u.Get("prompt_tokens").Int())*r.ratio))
			zeroExisting("prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cached_creation_tokens")
		case u.Get("total_input_tokens").Type == gjson.Number:
			cached := int64(float64(u.Get("total_input_tokens").Int()) * r.ratio)
			set("total_cached_tokens", cached)
			if u.Get("cached_tokens").Exists() {
				set("cached_tokens", cached)
			}
		case u.Get("input_tokens").Type == gjson.Number:
			cached := int64(float64(u.Get("input_tokens").Int()) * r.ratio)
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
			set("cachedContentTokenCount", int64(float64(u.Get("promptTokenCount").Int())*r.ratio))
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
