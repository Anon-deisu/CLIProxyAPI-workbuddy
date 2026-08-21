package executor

import (
	"errors"
	"net/http"
	"testing"
)

// The exact string Go's HTTP/2 client produces when Cursor's side sends
// RST_STREAM mid-run. It carries no Cursor error envelope, so before the
// transport-reset classifier it matched nothing, was never retried, and
// reached clients raw.
var errPeerReset = errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer")

func TestPeerResetIsRetriedAsTransient(t *testing.T) {
	if !isCursorTransportReset(errPeerReset) {
		t.Fatal("peer reset not recognized as a transport reset")
	}
	if !isCursorTransientProviderError(errPeerReset) {
		t.Fatal("peer reset must enter the transient retry path")
	}
	if !isCursorProviderError(errPeerReset) {
		t.Fatal("peer reset must count as a provider-side failure")
	}
	if isCursorHardProviderError(errPeerReset) || isCursorRateLimited(errPeerReset) || isCursorPolicyBlock(errPeerReset) {
		t.Fatal("peer reset misclassified as hard/rate-limit/policy")
	}
}

func TestOtherTransportTeardownsAreCovered(t *testing.T) {
	for _, msg := range []string{
		"http2: server sent GOAWAY and closed the connection; LastStreamID=5, ErrCode=NO_ERROR",
		"http2: response body closed",
		"read tcp 10.0.0.2:44412->104.18.2.1:443: connection reset by peer",
		"cursor proxy connect read: unexpected EOF",
	} {
		if !isCursorTransportReset(errors.New(msg)) {
			t.Errorf("not recognized as transport reset: %s", msg)
		}
	}
}

func TestRealErrorEnvelopesAreNotMistakenForResets(t *testing.T) {
	for _, msg := range []string{
		`cursor connect end-stream error: {"code":"resource_exhausted","details":[{"debug":{"error":"ERROR_PROVIDER_ERROR","details":{"additionalInfo":{"providerStatusCode":"400"}}}}]}`,
		"cursor: request has no user message",
	} {
		if isCursorTransportReset(errors.New(msg)) {
			t.Errorf("wrongly treated as transport reset: %s", msg)
		}
	}
}

func TestExhaustedResetRetriesSurfaceAsCleanBadGateway(t *testing.T) {
	humanized := humanizeCursorError(errPeerReset)
	var se statusErr
	if !errors.As(humanized, &se) || se.code != http.StatusBadGateway {
		t.Fatalf("expected 502 statusErr, got %v", humanized)
	}
	if se.msg == errPeerReset.Error() {
		t.Fatal("raw transport error must not reach the client verbatim")
	}
}
