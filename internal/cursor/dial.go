package cursor

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// dialTLSContext establishes a TLS connection to addr. When HTTPS_PROXY is set
// and the destination is not loopback, the connection is tunneled through the
// proxy with HTTP CONNECT first (the TLS handshake still targets addr, so the
// proxy never sees plaintext). Loopback destinations always dial directly.
func dialTLSContext(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	if proxyURL := httpsProxyFromEnv(); proxyURL != nil && !isLoopbackAddr(addr) {
		return dialTLSViaConnectProxy(ctx, network, addr, cfg, proxyURL)
	}
	d := &tls.Dialer{Config: cfg}
	return d.DialContext(ctx, network, addr)
}

func httpsProxyFromEnv() *url.URL {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u
		}
	}
	return nil
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bufferedConn keeps bytes already read into the CONNECT response buffer.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func dialTLSViaConnectProxy(ctx context.Context, network, addr string, cfg *tls.Config, proxyURL *url.URL) (net.Conn, error) {
	if scheme := strings.ToLower(proxyURL.Scheme); scheme != "" && scheme != "http" {
		return nil, fmt.Errorf("cursor proxy: unsupported scheme %q (only http CONNECT proxies)", proxyURL.Scheme)
	}
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		host := proxyURL.Hostname()
		proxyAddr = net.JoinHostPort(host, "80")
	}

	d := &net.Dialer{}
	rawConn, err := d.DialContext(ctx, network, proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("cursor proxy dial %s: %w", proxyAddr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	} else {
		_ = rawConn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		creds := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		req.Header.Set("Proxy-Authorization", "Basic "+creds)
	}
	if err = req.Write(rawConn); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("cursor proxy connect write: %w", err)
	}
	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("cursor proxy connect read: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = rawConn.Close()
		return nil, fmt.Errorf("cursor proxy connect %s: HTTP %d", addr, resp.StatusCode)
	}

	tlsCfg := cfg
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	if tlsCfg.ServerName == "" {
		host, _, _ := net.SplitHostPort(addr)
		clone := tlsCfg.Clone()
		clone.ServerName = host
		tlsCfg = clone
	}
	tlsConn := tls.Client(&bufferedConn{Conn: rawConn, r: br}, tlsCfg)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("cursor proxy tls handshake %s: %w", addr, err)
	}
	// Streaming connections (agent runs) are long-lived; the dial deadline must not apply.
	_ = tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
}
