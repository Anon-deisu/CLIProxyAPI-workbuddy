package api

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
)

type degradedAPIListener struct {
	server      *http.Server
	listener    net.Listener
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
}

type degradedAPIConn struct {
	net.Conn
	owner *degradedAPIListener
}

func (c *degradedAPIConn) Close() error {
	errClose := c.Conn.Close()
	c.owner.mu.Lock()
	delete(c.owner.connections, c)
	c.owner.mu.Unlock()
	return errClose
}

func (l *degradedAPIListener) Accept() (net.Conn, error) {
	conn, errAccept := l.listener.Accept()
	if errAccept != nil {
		return nil, errAccept
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	tracked := &degradedAPIConn{Conn: conn, owner: l}
	l.connections[tracked] = struct{}{}
	return tracked, nil
}

func (l *degradedAPIListener) Addr() net.Addr { return l.listener.Addr() }
func (l *degradedAPIListener) Close() error   { return l.listener.Close() }

func (l *degradedAPIListener) close() error {
	l.mu.Lock()
	l.closed = true
	connections := make([]net.Conn, 0, len(l.connections))
	for conn := range l.connections {
		connections = append(connections, conn)
	}
	clear(l.connections)
	l.mu.Unlock()
	if errListener := l.listener.Close(); errListener != nil && !errors.Is(errListener, net.ErrClosed) {
		log.WithError(errListener).Warn("failed to close degraded TCP listener")
	}
	errClose := l.server.Close()
	// HTTP Server.Close does not close hijacked WebSocket connections.
	for _, conn := range connections {
		_ = conn.Close()
	}
	return errClose
}

func (s *Server) degradedPoolMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		degraded := coreexecutor.DegradedRouting(c.Request.Context())
		_, modelRoute := s.degradedRoutes[c.Request.Method+" "+c.FullPath()]
		if degraded {
			if !modelRoute {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			if s.handlers.AuthManager.HomeEnabled() {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "degraded listener requires local account scheduling"})
				return
			}
		}
		if modelRoute {
			c.Request = c.Request.WithContext(coreexecutor.WithDegradedRouting(c.Request.Context(), degraded))
		}
		c.Next()
	}
}

func (s *Server) degradedAPIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.engine.ServeHTTP(w, r.WithContext(coreexecutor.WithDegradedRouting(r.Context(), true)))
	})
}

// setDegradedPort binds before replacing the previous listener, preserving it on failure.
func (s *Server) setDegradedPort(port int) error {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	if s.degradedStopped {
		return fmt.Errorf("API server is stopping")
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("degraded-port must be between 0 and 65535")
	}
	if port == s.degradedPort {
		return nil
	}
	if port == 0 {
		if s.degradedListener != nil {
			if errClose := s.degradedListener.close(); errClose != nil {
				log.WithError(errClose).Warn("failed to close degraded API listener")
			}
		}
		s.degradedListener, s.degradedPort = nil, 0
		return nil
	}
	if s.degradedHomeEnabled {
		return fmt.Errorf("degraded listener requires local account scheduling")
	}
	if port == s.degradedMainPort {
		return fmt.Errorf("degraded-port must differ from the main API port")
	}
	var tlsConfig *tls.Config
	if s.degradedTLS.Enable {
		certificate, errLoad := tls.LoadX509KeyPair(s.degradedTLS.Cert, s.degradedTLS.Key)
		if errLoad != nil {
			return fmt.Errorf("load degraded listener TLS certificate: %w", errLoad)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h2", "http/1.1"}}
	}
	listener, errListen := net.Listen("tcp", net.JoinHostPort(s.degradedHost, strconv.Itoa(port)))
	if errListen != nil {
		return fmt.Errorf("listen on degraded port %d: %w", port, errListen)
	}
	server := &http.Server{Handler: s.degradedAPIHandler(), TLSConfig: tlsConfig}
	if tlsConfig != nil {
		if errHTTP2 := http2.ConfigureServer(server, &http2.Server{}); errHTTP2 != nil {
			_ = listener.Close()
			return fmt.Errorf("configure degraded listener HTTP/2: %w", errHTTP2)
		}
	}
	next := &degradedAPIListener{server: server, listener: listener, connections: make(map[net.Conn]struct{})}
	var servingListener net.Listener = next
	if tlsConfig != nil {
		servingListener = tls.NewListener(next, tlsConfig)
	}
	previous := s.degradedListener
	s.degradedListener, s.degradedPort = next, port
	go func() {
		if errServe := server.Serve(servingListener); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.WithError(errServe).WithField("port", port).Error("degraded API listener stopped")
		}
	}()
	if previous != nil {
		if errClose := previous.close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close previous degraded API listener")
		}
	}
	log.WithField("port", port).Info("degraded account API listener enabled")
	return nil
}

func (s *Server) stopDegradedListener() error {
	s.degradedMu.Lock()
	defer s.degradedMu.Unlock()
	s.degradedStopped = true
	if s.degradedListener == nil {
		return nil
	}
	errClose := s.degradedListener.close()
	s.degradedListener, s.degradedPort = nil, 0
	return errClose
}
