package executor

import (
	"net/http"
	"testing"
	"time"
)

// WorkBuddy's 6020 queue-waiting frame is an advisory throttle: the upstream is
// reachable and asks the client to retry after data.retry_after seconds. It must
// keep its 429 status and retry hint so the scheduler applies a short,
// request-scoped wait instead of treating the account as quota exhausted.
func TestWorkBuddyQueueWaitingKeepsRetryAfter(t *testing.T) {
	frame := []byte(`{"code":6020,"msg":"queue.waiting.title","requestId":"req-1","data":{"queue_position":1813,"queue_size":"999+","retry_after":40,"estimated_wait":9065,"client_user_type":"free"}}`)
	err := NewWorkBuddyExecutor(nil, "workbuddy").upstreamStatusError(http.StatusTooManyRequests, http.Header{}, frame)
	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
	}
	if err.IsCredentialScoped() {
		t.Fatal("queue waiting must not suspend the whole credential")
	}
	if err.RetryAfter() == nil {
		t.Fatal("queue waiting lost its retry_after hint")
	}
	if got := *err.RetryAfter(); got != 40*time.Second {
		t.Fatalf("retryAfter = %v, want 40s", got)
	}
}

// The same frame arrives inside an SSE data event with no status field. The
// stream decoder must not downgrade it to a generic 502, which would discard
// both the 429 classification and the retry hint.
func TestWorkBuddyQueueWaitingStreamFrameIsThrottle(t *testing.T) {
	frame := []byte(`{"code":6020,"msg":"queue.waiting.title","data":{"retry_after":40}}`)
	upstreamErr, failed := openAICompatStreamDataError(frame, "")
	if !failed {
		t.Fatal("queue waiting frame was not recognized as an error")
	}
	if got := upstreamErr.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("stream status = %d, want %d", got, http.StatusTooManyRequests)
	}
	if upstreamErr.IsCredentialScoped() {
		t.Fatal("stream queue waiting must not suspend the whole credential")
	}
	if upstreamErr.RetryAfter() == nil || *upstreamErr.RetryAfter() != 40*time.Second {
		t.Fatalf("stream retryAfter = %v, want 40s", upstreamErr.RetryAfter())
	}
}
