package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"

	evbus "github.com/asaskevich/EventBus"
)

// sseSession represents an active SSE connection.
type sseSession struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	done    chan struct{}
	evbus   evbus.Bus
}

// SSEServer implements a Server-Sent Events (SSE) based MCP server.
// It provides real-time communication capabilities over HTTP using the SSE protocol.
type SSEServer struct {
	server          *MCPServer
	baseURL         string
	messageEndpoint string
	sseEndpoint     string
	sessions        sync.Map
	srv             *http.Server
	// 进程管理器
	processManager *ProcessManager
	// Stdio cmd
	stdioCmd string
}

// Option defines a function type for configuring SSEServer
type Option func(*SSEServer)

// WithBaseURL sets the base URL for the SSE server
func WithBaseURL(baseURL string) Option {
	return func(s *SSEServer) {
		s.baseURL = baseURL
	}
}

// WithMessageEndpoint sets the message endpoint path
func WithMessageEndpoint(endpoint string) Option {
	return func(s *SSEServer) {
		s.messageEndpoint = endpoint
	}
}

// WithSSEEndpoint sets the SSE endpoint path
func WithSSEEndpoint(endpoint string) Option {
	return func(s *SSEServer) {
		s.sseEndpoint = endpoint
	}
}

// WithHTTPServer sets the HTTP server instance
func WithHTTPServer(srv *http.Server) Option {
	return func(s *SSEServer) {
		s.srv = srv
	}
}

// WithStdioCmd sets the stdio cmd
func WithStdioCmd(cmd string) Option {
	return func(s *SSEServer) {
		s.stdioCmd = cmd
	}
}

// NewSSEServer creates a new SSE server instance with the given MCP server and options.
func NewSSEServer(server *MCPServer, opts ...Option) *SSEServer {

	s := &SSEServer{
		server:          server,
		sseEndpoint:     "/sse",
		messageEndpoint: "/message",
	}

	// Apply all options
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// NewTestServer creates a test server for testing purposes
func NewTestServer(server *MCPServer) *httptest.Server {
	sseServer := NewSSEServer(server)

	testServer := httptest.NewServer(sseServer)
	sseServer.baseURL = testServer.URL
	return testServer
}

// Start begins serving SSE connections on the specified address.
// It sets up HTTP handlers for SSE and message endpoints.
func (s *SSEServer) Start(addr string) error {
	fmt.Fprintf(os.Stderr, "【调试】开始启动SSE服务器，地址: %s\n", addr)

	// 创建进程管理器
	s.processManager = NewProcessManager(s)
	fmt.Fprintf(os.Stderr, "【调试】创建进程管理器成功\n")

	// 启动子进程
	if s.stdioCmd != "" {
		fmt.Fprintf(os.Stderr, "【调试】准备启动子进程: %s\n", s.stdioCmd)
		if err := s.processManager.Start(s.stdioCmd); err != nil {
			fmt.Fprintf(os.Stderr, "【调试】启动子进程失败: %v\n", err)
			return fmt.Errorf("启动子进程失败: %v", err)
		}
		fmt.Fprintf(os.Stderr, "【调试】子进程启动成功\n")
	} else {
		fmt.Fprintf(os.Stderr, "【调试】未指定子进程命令，跳过启动子进程\n")
	}

	s.srv = &http.Server{
		Addr:    addr,
		Handler: s,
	}

	fmt.Fprintf(os.Stderr, "【调试】开始监听HTTP请求\n")
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the SSE server, closing all active sessions
// and shutting down the HTTP server.
func (s *SSEServer) Shutdown(ctx context.Context) error {
	fmt.Fprintf(os.Stderr, "【调试】开始关闭SSE服务器\n")

	// 停止进程管理器
	if s.processManager != nil {
		fmt.Fprintf(os.Stderr, "【调试】准备停止进程管理器\n")
		if err := s.processManager.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "【调试】停止进程管理器失败: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "【调试】进程管理器停止成功\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, "【调试】进程管理器不存在，跳过停止\n")
	}

	if s.srv != nil {
		fmt.Fprintf(os.Stderr, "【调试】准备关闭所有会话\n")
		sessionCount := 0
		s.sessions.Range(func(key, value interface{}) bool {
			sessionCount++
			if session, ok := value.(*sseSession); ok {
				close(session.done)
			}
			s.sessions.Delete(key)
			return true
		})
		fmt.Fprintf(os.Stderr, "【调试】已关闭 %d 个会话\n", sessionCount)

		fmt.Fprintf(os.Stderr, "【调试】准备关闭HTTP服务器\n")
		err := s.srv.Shutdown(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "【调试】关闭HTTP服务器失败: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "【调试】HTTP服务器关闭成功\n")
		}
		return err
	}
	fmt.Fprintf(os.Stderr, "【调试】HTTP服务器不存在，跳过关闭\n")
	return nil
}

// handleSSE handles incoming SSE connection requests.
// It sets up appropriate headers and creates a new session for the client.
func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Get or generate session ID
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	// Create a new session
	session := &sseSession{
		writer:  w,
		flusher: flusher,
		done:    make(chan struct{}),
		evbus:   evbus.New(),
	}

	// Store the session
	s.sessions.Store(sessionID, session)

	// 如果进程管理器存在，注册会话
	if s.processManager != nil {
		// 这里可以使用一个唯一的进程ID，或者简单地使用会话ID
		s.processManager.RegisterSession(sessionID, sessionID)
	}

	// Cleanup when the connection is closed
	defer func() {
		// 如果进程管理器存在，取消注册会话
		if s.processManager != nil {
			s.processManager.UnregisterSession(sessionID)
		}

		s.sessions.Delete(sessionID)
		close(session.done)
	}()

	// Start notification handler for this session
	go func() {
		for {
			select {
			case serverNotification := <-s.server.notifications:
				// Only forward notifications meant for this session
				if serverNotification.Context.SessionID == sessionID {
					eventData, err := json.Marshal(serverNotification.Notification)
					if err == nil {
						// 创建消息
						jsonEvent := string(eventData)

						session.evbus.Publish("message", jsonEvent)
					}
				}
			case <-session.done:
				return
			case <-r.Context().Done():
				return
			}
		}
	}()

	messageEndpoint := fmt.Sprintf(
		"%s%s?sessionId=%s",
		s.baseURL,
		s.messageEndpoint,
		sessionID,
	)

	// Send the initial endpoint event
	fmt.Fprintf(w, "event: endpoint\ndata: %s\r\n\r\n", messageEndpoint)
	flusher.Flush()

	session.evbus.Subscribe("message", func(msg string) {
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
		flusher.Flush()
	})

	// Main event loop - this runs in the HTTP handler goroutine
	select {
	case <-r.Context().Done():
		close(session.done)
		return
	}
}

// handleMessage processes incoming JSON-RPC messages from clients and sends responses
// back through both the SSE connection and HTTP response.
func (s *SSEServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "Method not allowed")
		return
	}

	if s.processManager == nil {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "Process manager not found")
		return
	}

	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		s.writeJSONRPCError(w, nil, mcp.INVALID_PARAMS, "Missing sessionId")
		return
	}

	fmt.Fprintf(os.Stderr, "【调试】收到会话 %s 的消息请求\n", sessionID)

	// Parse message as raw JSON
	var rawMessage json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&rawMessage); err != nil {
		fmt.Fprintf(os.Stderr, "【调试】解析请求体失败: %v\n", err)
		s.writeJSONRPCError(w, nil, mcp.PARSE_ERROR, "Parse error")
		return
	}

	fmt.Fprintf(os.Stderr, "【调试】解析的原始消息: %s\n", string(rawMessage))

	// 解析请求ID，用于匹配响应
	var requestID interface{}
	var requestObj map[string]interface{}
	if err := json.Unmarshal(rawMessage, &requestObj); err == nil {
		if id, ok := requestObj["id"]; ok {
			requestID = id
			fmt.Fprintf(os.Stderr, "【调试】请求ID: %v\n", requestID)
		}
	}

	// 如果进程管理器存在且子进程正在运行，将消息转发到子进程
	fmt.Fprintf(os.Stderr, "【调试】准备将消息转发到子进程\n")
	// 添加会话ID到消息中，以便子进程可以识别来源
	var msgObj map[string]interface{}
	if err := json.Unmarshal(rawMessage, &msgObj); err == nil {
		// 只有在成功解析JSON的情况下才添加会话ID
		msgObj["id"] = fmt.Sprintf("%s/%v", sessionID, msgObj["id"])
		if modifiedMsg, err := json.Marshal(msgObj); err == nil {
			// 使用修改后的消息
			fmt.Fprintf(os.Stderr, "【调试】修改后的消息: %s\n", string(modifiedMsg))
			if err := s.processManager.WriteMessage(modifiedMsg); err != nil {
				fmt.Fprintf(os.Stderr, "向子进程写入消息失败: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "【调试】成功将消息写入子进程\n")
			}
		} else {
			fmt.Fprintf(os.Stderr, "【调试】重新序列化JSON失败: %v\n", err)
		}
	} else {
		// 如果无法解析JSON，则直接发送原始消息
		fmt.Fprintf(os.Stderr, "【调试】无法解析JSON，发送原始消息: %v\n", err)
		if err := s.processManager.WriteMessage(rawMessage); err != nil {
			fmt.Fprintf(os.Stderr, "向子进程写入消息失败: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "【调试】成功将原始消息写入子进程\n")
		}
	}

	sessionI, ok := s.sessions.Load(sessionID)
	if !ok {
		s.writeJSONRPCError(w, nil, mcp.INVALID_PARAMS, "Session not found")
		return
	}
	session := sessionI.(*sseSession)

	event := make(chan string)
	defer close(event)

	session.evbus.SubscribeOnce("message", func(msg string) {
		event <- msg
	})

	msg := <-event

	s.writeJSONRPCResponse(w, msg)
}

// writeJSONRPCError writes a JSON-RPC error response with the given error details.
func (s *SSEServer) writeJSONRPCError(
	w http.ResponseWriter,
	id interface{},
	code int,
	message string,
) {
	response := createErrorResponse(id, code, message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(response)
}

func (s *SSEServer) writeJSONRPCResponse(
	w http.ResponseWriter,
	message string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(message))
}

// SendEventToSession sends an event to a specific SSE session identified by sessionID.
// Returns an error if the session is not found or closed.
func (s *SSEServer) SendEventToSession(
	sessionID string,
	event string,
) error {
	fmt.Fprintf(os.Stderr, "【调试】尝试向会话 %s 发送事件\n", sessionID)

	sessionI, ok := s.sessions.Load(sessionID)
	if !ok {
		fmt.Fprintf(os.Stderr, "【调试】会话未找到: %s\n", sessionID)
		return fmt.Errorf("session not found: %s", sessionID)
	}
	session := sessionI.(*sseSession)

	session.evbus.Publish("message", event)

	// Queue the event for sending via SSE
	select {
	case <-session.done:
		fmt.Fprintf(os.Stderr, "【调试】会话 %s 已关闭\n", sessionID)
		return fmt.Errorf("session closed")
	default:
		fmt.Fprintf(os.Stderr, "【调试】会话 %s 的事件队列已满\n", sessionID)
		return fmt.Errorf("event queue full")
	}
}

// ServeHTTP implements the http.Handler interface.
func (s *SSEServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case s.sseEndpoint:
		s.handleSSE(w, r)
	case s.messageEndpoint:
		s.handleMessage(w, r)
	default:
		http.NotFound(w, r)
	}
}
