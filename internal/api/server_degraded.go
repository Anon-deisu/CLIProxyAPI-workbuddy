package api

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
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

const (
	exoFreeMaxAttempts        = 5
	exoFreeFrameLimit         = 1 << 20
	exoFreeRequestLimit       = 16 << 20
	openCodeFreeStreamBuffer  = 32 << 10
	openCodeFreeChatPath      = "chat/completions"
	openCodeFreeResponsesPath = "responses"
)

// This independent catalog contains Zen's Chat and Responses free models checked on 2026-10-07.
var openCodeFreeModels = [...]struct {
	ID       string
	Endpoint string
}{
	{"big-pickle", openCodeFreeChatPath},
	{"exo-free", openCodeFreeChatPath},
	{"fledge-alpha-free", openCodeFreeChatPath},
	{"space-bunny-free", openCodeFreeChatPath},
	{"longcat-2.5-preview-free", openCodeFreeChatPath},
	{"mimo-v2.6-flash-free", openCodeFreeChatPath},
	{"ling-3.1-flash-free", openCodeFreeChatPath},
	{"ling-3.0-flash-fin-free", openCodeFreeChatPath},
	{"nemotron-3-ultra-free", openCodeFreeChatPath},
	{"nemotron-3.5-lightning-free", openCodeFreeChatPath},
	{"muse-spark-1.3-contributor-free", openCodeFreeResponsesPath},
	{"muse-spark-1.2-contributor-free", openCodeFreeResponsesPath},
}

func (s *Server) exoFreeAPIHandler() http.Handler {
	engine := gin.New()
	engine.Use(gin.Recovery(), corsMiddleware())
	v1 := engine.Group("/v1", AuthMiddleware(s.accessManager))
	v1.GET("/models", func(c *gin.Context) {
		models := make([]gin.H, 0, len(openCodeFreeModels))
		for _, model := range openCodeFreeModels {
			models = append(models, gin.H{"id": model.ID, "object": "model", "owned_by": "opencode"})
		}
		c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
	})
	v1.POST("/chat/completions", s.exoFreeChatCompletions)
	v1.POST("/responses", s.exoFreeResponses)
	return engine
}

func (s *Server) setExoFreePort(port int) error {
	s.exoFreeMu.Lock()
	defer s.exoFreeMu.Unlock()
	if s.exoFreeStopped {
		return fmt.Errorf("API server is stopping")
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("exo-free-port must be between 0 and 65535")
	}
	if port == s.exoFreePort {
		return nil
	}
	if port == 0 {
		s.closeExoFreeListener()
		return nil
	}
	if port == s.degradedMainPort {
		return fmt.Errorf("exo-free-port must differ from the main API port")
	}
	var tlsConfig *tls.Config
	if s.exoFreeTLS.Enable {
		certificate, errLoad := tls.LoadX509KeyPair(s.exoFreeTLS.Cert, s.exoFreeTLS.Key)
		if errLoad != nil {
			return fmt.Errorf("load exo-free listener TLS certificate: %w", errLoad)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h2", "http/1.1"}}
	}
	listener, errListen := net.Listen("tcp", net.JoinHostPort(s.exoFreeHost, strconv.Itoa(port)))
	if errListen != nil {
		return fmt.Errorf("listen on exo-free port %d: %w", port, errListen)
	}
	server := &http.Server{Handler: s.exoFreeAPIHandler(), TLSConfig: tlsConfig}
	if tlsConfig != nil {
		if errHTTP2 := http2.ConfigureServer(server, &http2.Server{}); errHTTP2 != nil {
			_ = listener.Close()
			return fmt.Errorf("configure exo-free listener HTTP/2: %w", errHTTP2)
		}
	}
	var serving net.Listener = listener
	if tlsConfig != nil {
		serving = tls.NewListener(listener, tlsConfig)
	}
	previousServer, previousListener := s.exoFreeServer, s.exoFreeListener
	s.exoFreeServer, s.exoFreeListener, s.exoFreePort = server, listener, port
	go func() {
		if errServe := server.Serve(serving); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) && !errors.Is(errServe, net.ErrClosed) {
			log.WithError(errServe).WithField("port", port).Error("exo-free API listener stopped")
		}
	}()
	if previousServer != nil {
		_ = previousListener.Close()
		if errClose := previousServer.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close previous exo-free API listener")
		}
	}
	log.WithField("port", port).Info("exo-free API listener enabled")
	return nil
}

func (s *Server) closeExoFreeListener() error {
	if s.exoFreeServer == nil {
		return nil
	}
	_ = s.exoFreeListener.Close()
	errClose := s.exoFreeServer.Close()
	s.exoFreeServer, s.exoFreeListener, s.exoFreePort = nil, nil, 0
	return errClose
}

func (s *Server) stopExoFreeListener() error {
	s.exoFreeMu.Lock()
	defer s.exoFreeMu.Unlock()
	s.exoFreeStopped = true
	return s.closeExoFreeListener()
}

func (s *Server) setExoFreePool(pool *config.CodexStaticISPConfig) {
	snapshot := config.CodexStaticISPConfig{}
	if pool != nil {
		snapshot.Enabled = pool.Enabled
		snapshot.Nodes = append([]config.StaticISPNode(nil), pool.Nodes...)
	}
	s.exoFreePool.Store(snapshot)
}

func (s *Server) exoFreeClientForAttempt() (*http.Client, *http.Transport, error) {
	pool := s.exoFreePool.Load().(config.CodexStaticISPConfig)
	if !pool.Enabled || len(pool.Nodes) == 0 {
		return s.exoFreeHTTPClient, nil, nil
	}
	node := pool.Nodes[s.exoFreeSelectNode(len(pool.Nodes))]
	proxyURL, errURL := node.ProxyURL()
	if errURL != nil {
		return nil, nil, fmt.Errorf("invalid exo-free static ISP node %q: %w", node.ID, errURL)
	}
	transport, _, errTransport := proxyutil.BuildHTTPTransport(proxyURL)
	if errTransport != nil {
		return nil, nil, fmt.Errorf("build exo-free static ISP transport for %q: %w", node.ID, errTransport)
	}
	return &http.Client{Transport: transport}, transport, nil
}

func exoFreeFrame(reader *bufio.Reader) ([]byte, []byte, error) {
	var frame, data []byte
	for {
		line, errRead := reader.ReadBytes('\n')
		if errRead != nil {
			return nil, nil, errRead
		}
		frame = append(frame, line...)
		if len(frame) > exoFreeFrameLimit {
			return nil, nil, fmt.Errorf("exo-free SSE frame exceeds %d bytes", exoFreeFrameLimit)
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			return frame, data, nil
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			value := bytes.TrimPrefix(trimmed, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
}

func exoFreeResponseID(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return "", io.EOF
	}
	var payload struct {
		ID string `json:"id"`
	}
	if errDecode := json.Unmarshal(data, &payload); errDecode != nil {
		return "", fmt.Errorf("invalid exo-free SSE payload: %w", errDecode)
	}
	return payload.ID, nil
}

func exoFreeRenameModel(frame, data []byte, model string) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil || payload["model"] == nil {
		return frame
	}
	payload["model"], _ = json.Marshal(model)
	encoded, _ := json.Marshal(payload)
	var result []byte
	for _, line := range bytes.Split(bytes.TrimRight(frame, "\r\n"), []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			result = append(result, bytes.TrimRight(line, "\r")...)
			result = append(result, '\n')
		}
	}
	result = append(result, []byte("data: ")...)
	result = append(result, encoded...)
	return append(result, '\n', '\n')
}

func prepareExoFreeAgentBody(payload map[string]json.RawMessage, responses bool) error {
	var tools []json.RawMessage
	if raw, exists := payload["tools"]; exists {
		if errDecode := json.Unmarshal(raw, &tools); errDecode != nil {
			return fmt.Errorf("tools must be an array: %w", errDecode)
		}
	}
	present := make(map[string]bool)
	for _, raw := range tools {
		var tool struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if errDecode := json.Unmarshal(raw, &tool); errDecode != nil {
			return fmt.Errorf("invalid tool definition: %w", errDecode)
		}
		if tool.Type == "function" {
			if responses {
				present[tool.Name] = true
			} else {
				present[tool.Function.Name] = true
			}
		}
	}
	for _, name := range []string{"bash", "edit", "glob", "grep", "read"} {
		if present[name] {
			continue
		}
		definition := map[string]any{
			"name": name, "description": "Agent tool " + name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}
		var tool map[string]any
		if responses {
			definition["type"] = "function"
			tool = definition
		} else {
			tool = map[string]any{"type": "function", "function": definition}
		}
		encoded, errMarshal := json.Marshal(tool)
		if errMarshal != nil {
			return errMarshal
		}
		tools = append(tools, encoded)
	}
	encodedTools, errMarshal := json.Marshal(tools)
	if errMarshal != nil {
		return errMarshal
	}
	payload["tools"] = encodedTools
	if responses {
		return nil
	}
	var options map[string]json.RawMessage
	if raw, exists := payload["stream_options"]; exists {
		if errDecode := json.Unmarshal(raw, &options); errDecode != nil {
			return fmt.Errorf("stream_options must be an object: %w", errDecode)
		}
	}
	if options == nil {
		options = make(map[string]json.RawMessage)
	}
	options["include_usage"] = json.RawMessage("true")
	encodedOptions, errMarshal := json.Marshal(options)
	if errMarshal != nil {
		return errMarshal
	}
	payload["stream_options"] = encodedOptions
	return nil
}

func (s *Server) exoFreeChatCompletions(c *gin.Context) {
	s.openCodeFreeStream(c, openCodeFreeChatPath, s.exoFreeUpstreamURL)
}

func (s *Server) exoFreeResponses(c *gin.Context) {
	s.openCodeFreeStream(c, openCodeFreeResponsesPath, s.exoFreeResponsesURL)
}

func (s *Server) openCodeFreeStream(c *gin.Context, endpoint, upstreamURL string) {
	body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, exoFreeRequestLimit+1))
	if errRead != nil || len(body) > exoFreeRequestLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	var model string
	var stream bool
	if json.Unmarshal(payload["model"], &model) != nil || json.Unmarshal(payload["stream"], &stream) != nil || !stream {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model and stream: true are required"})
		return
	}
	prefix := ""
	modelEndpoint := ""
	upstreamModel := model
	for _, freeModel := range openCodeFreeModels {
		if freeModel.ID == model {
			modelEndpoint = freeModel.Endpoint
			break
		}
	}
	switch model {
	case "gpt-6-sol":
		prefix, upstreamModel, modelEndpoint = "resp_", "exo-free", openCodeFreeChatPath
	case "claude-opus-5-5":
		prefix, upstreamModel, modelEndpoint = "msg_", "exo-free", openCodeFreeChatPath
	}
	if modelEndpoint == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is not available on this port"})
		return
	}
	if modelEndpoint != endpoint {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model requires /v1/" + modelEndpoint})
		return
	}
	payload["model"], _ = json.Marshal(upstreamModel)
	if errShape := prepareExoFreeAgentBody(payload, endpoint == openCodeFreeResponsesPath); errShape != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errShape.Error()})
		return
	}
	upstreamBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	projectID := "prj_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	for attempt := 0; attempt < exoFreeMaxAttempts; attempt++ {
		client, transport, errClient := s.exoFreeClientForAttempt()
		if errClient != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": errClient.Error()})
			return
		}
		if transport != nil {
			defer transport.CloseIdleConnections()
		}
		request, errNew := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
		if errNew != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "invalid OpenCode Free upstream URL"})
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Authorization", "Bearer public")
		request.Header.Set("User-Agent", "opencode/1.18.31")
		request.Header.Set("X-Opencode-Client", "cli")
		request.Header.Set("X-Opencode-Project", projectID)
		// A fresh canonical session avoids upstream provider affinity on a mismatched branch.
		sessionID := "ses_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:26]
		request.Header.Set("X-Opencode-Session", sessionID)
		request.Header.Set("X-Opencode-Session-Id", sessionID)
		request.Header.Set("X-Session-Affinity", sessionID)
		request.Header.Set("X-Session-Id", sessionID)
		request.Header.Set("X-Opencode-Request", "req_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
		response, errDo := client.Do(request)
		if errDo != nil {
			if c.Request.Context().Err() != nil {
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": "OpenCode Free upstream request failed"})
			return
		}
		if response.StatusCode != http.StatusOK {
			upstreamError, _ := io.ReadAll(io.LimitReader(response.Body, exoFreeFrameLimit))
			_ = response.Body.Close()
			c.Data(response.StatusCode, "application/json", upstreamError)
			return
		}
		if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
			_ = response.Body.Close()
			c.JSON(http.StatusBadGateway, gin.H{"error": "OpenCode Free upstream did not return an SSE stream"})
			return
		}
		reader := bufio.NewReader(response.Body)
		var buffered [][]byte
		bufferSize := 0
		matched, mismatch := prefix == "", false
		for !matched && !mismatch {
			frame, data, errFrame := exoFreeFrame(reader)
			if errFrame != nil {
				_ = response.Body.Close()
				c.JSON(http.StatusBadGateway, gin.H{"error": "exo-free stream ended before a model ID"})
				return
			}
			bufferSize += len(frame)
			if bufferSize > exoFreeFrameLimit {
				_ = response.Body.Close()
				c.JSON(http.StatusBadGateway, gin.H{"error": "exo-free stream exceeded bootstrap limit"})
				return
			}
			buffered = append(buffered, exoFreeRenameModel(frame, data, model))
			id, errID := exoFreeResponseID(data)
			if errID != nil {
				_ = response.Body.Close()
				c.JSON(http.StatusBadGateway, gin.H{"error": "exo-free stream has an invalid model ID"})
				return
			}
			if id != "" {
				matched, mismatch = strings.HasPrefix(id, prefix), !strings.HasPrefix(id, prefix)
			}
		}
		if mismatch {
			_ = response.Body.Close()
			continue
		}
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
		for _, frame := range buffered {
			if _, errWrite := c.Writer.Write(frame); errWrite != nil {
				_ = response.Body.Close()
				return
			}
		}
		c.Writer.Flush()
		if prefix == "" {
			// Native free models retain their complete SSE wire format, including Responses event names.
			buffer := make([]byte, openCodeFreeStreamBuffer)
			for {
				n, errReadStream := response.Body.Read(buffer)
				if n > 0 {
					if _, errWrite := c.Writer.Write(buffer[:n]); errWrite != nil {
						break
					}
					c.Writer.Flush()
				}
				if errReadStream != nil {
					break
				}
			}
			_ = response.Body.Close()
			return
		}
		for {
			frame, data, errFrame := exoFreeFrame(reader)
			if errFrame != nil {
				break
			}
			if _, errWrite := c.Writer.Write(exoFreeRenameModel(frame, data, model)); errWrite != nil {
				break
			}
			c.Writer.Flush()
			if bytes.Equal(data, []byte("[DONE]")) {
				break
			}
		}
		_ = response.Body.Close()
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("exo-free did not select %s after %d attempts", model, exoFreeMaxAttempts)})
}
