package cursor

import (
	"errors"
	"testing"
)

func TestProxyHandshakeFailuresAreRetryable(t *testing.T) {
	for _, msg := range []string{
		"cursor proxy connect read: unexpected EOF",
		"cursor proxy connect write: broken pipe",
		"cursor proxy dial 10.0.0.1:8080: connection refused",
	} {
		if !isRetryableProxyConnectError(errors.New(msg)) {
			t.Errorf("should retry a handshake failure: %s", msg)
		}
	}
}

func TestProxyRejectionsAreNotRetried(t *testing.T) {
	// Repeating these only multiplies the failure: the proxy will answer the
	// same way every time.
	for _, msg := range []string{
		"cursor proxy connect api2.cursor.sh:443: HTTP 407",
		"cursor proxy connect api2.cursor.sh:443: HTTP 403",
		`cursor proxy: unsupported scheme "socks5" (only http CONNECT proxies)`,
		"cursor proxy tls handshake api2.cursor.sh:443: x509: certificate signed by unknown authority",
	} {
		if isRetryableProxyConnectError(errors.New(msg)) {
			t.Errorf("should not retry: %s", msg)
		}
	}
}
