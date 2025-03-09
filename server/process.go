package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ProcessManager 管理与子进程的交互
type ProcessManager struct {
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	stdout            io.ReadCloser
	stderr            io.ReadCloser
	sseServer         *SSEServer
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	isRunning         bool
	sessionToID       map[string]string // 映射会话ID到进程ID
	responseListeners map[string]map[string]chan json.RawMessage
}

// NewProcessManager 创建一个新的进程管理器
func NewProcessManager(sseServer *SSEServer) *ProcessManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &ProcessManager{
		sseServer:         sseServer,
		ctx:               ctx,
		cancel:            cancel,
		sessionToID:       make(map[string]string),
		responseListeners: make(map[string]map[string]chan json.RawMessage),
	}
}

// Start 启动子进程
func (pm *ProcessManager) Start(cmdPath string, args ...string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.isRunning {
		return fmt.Errorf("进程已经在运行")
	}

	// 创建命令
	pm.cmd = exec.Command(cmdPath, args...)

	fmt.Println("启动子进程", pm.cmd.String())

	// 获取标准输入输出
	var err error
	pm.stdin, err = pm.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("无法获取标准输入管道: %v", err)
	}

	pm.stdout, err = pm.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("无法获取标准输出管道: %v", err)
	}

	pm.stderr, err = pm.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("无法获取标准错误管道: %v", err)
	}

	// 启动命令
	if err := pm.cmd.Start(); err != nil {
		return fmt.Errorf("启动子进程失败: %v", err)
	}

	pm.isRunning = true

	// 处理标准输出
	go pm.handleStdout()

	// 处理标准错误
	go pm.handleStderr()

	// 监控进程退出
	go func() {
		err := pm.cmd.Wait()
		pm.mu.Lock()
		pm.isRunning = false
		pm.mu.Unlock()
		if err != nil {
			fmt.Fprintf(os.Stderr, "子进程退出，错误: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "子进程正常退出")
		}
	}()

	return nil
}

// Stop 停止子进程
func (pm *ProcessManager) Stop() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.isRunning {
		return nil
	}

	pm.cancel()

	// 关闭标准输入
	if pm.stdin != nil {
		pm.stdin.Close()
	}

	// 尝试优雅地终止进程
	if err := pm.cmd.Process.Signal(os.Interrupt); err != nil {
		fmt.Fprintf(os.Stderr, "发送中断信号失败: %v\n", err)
		// 强制终止
		if err := pm.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("终止子进程失败: %v", err)
		}
	}

	pm.isRunning = false
	return nil
}

// WriteMessage 将消息写入子进程的标准输入
func (pm *ProcessManager) WriteMessage(message json.RawMessage) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.isRunning {
		return fmt.Errorf("子进程未运行")
	}

	// 调试: 打印发送到子进程的消息
	fmt.Fprintf(os.Stderr, "【调试】发送到子进程: %s\n", string(message))

	// 将消息写入标准输入
	_, err := pm.stdin.Write(append(message, '\n'))
	if err != nil {
		return fmt.Errorf("写入子进程失败: %v", err)
	}

	return nil
}

// RegisterSession 注册会话ID与进程的关联
func (pm *ProcessManager) RegisterSession(sessionID string, processID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.sessionToID[sessionID] = processID
}

// UnregisterSession 取消会话ID与进程的关联
func (pm *ProcessManager) UnregisterSession(sessionID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.sessionToID, sessionID)
}

// GetProcessIDForSession 获取会话对应的进程ID
func (pm *ProcessManager) GetProcessIDForSession(sessionID string) (string, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	id, ok := pm.sessionToID[sessionID]
	return id, ok
}

// handleStdout 处理子进程的标准输出
func (pm *ProcessManager) handleStdout() {
	scanner := bufio.NewScanner(pm.stdout)
	for scanner.Scan() {
		line := scanner.Text()

		// 调试: 打印从子进程接收的输出
		fmt.Fprintf(os.Stderr, "【调试】从子进程接收(stdout): %s\n", line)

		// 尝试解析JSON
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// 如果不是有效的JSON，仍然发送原始文本
			fmt.Fprintf(os.Stderr, "【调试】无法解析JSON: %v，发送原始文本\n", err)
			pm.broadcastToAllSessions(line)
			continue
		}

		// 检查是否有目标会话ID
		targetID, hasTargetID := event["id"].(string)

		if !hasTargetID {
			fmt.Fprintf(os.Stderr, "【调试】未找到目标会话ID，广播到所有会话\n")
			pm.broadcastToAllSessions(line)
			continue
		}

		parts := strings.Split(targetID, "/")
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "【调试】目标会话ID格式错误: %s\n", targetID)
			pm.broadcastToAllSessions(line)
			continue
		}

		sessionID := parts[0]
		requestID := parts[1]

		event["id"] = requestID

		eventJSON, err := json.Marshal(event)
		if err != nil {
			fmt.Fprintf(os.Stderr, "【调试】重新序列化JSON失败: %v，使用原始消息\n", err)
			pm.sseServer.SendEventToSession(sessionID, line)
		} else {
			fmt.Fprintf(os.Stderr, "【调试】重新序列化后的消息: %s\n", string(eventJSON))
			pm.sseServer.SendEventToSession(sessionID, string(eventJSON))
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "读取子进程标准输出错误: %v\n", err)
	}
}

// handleStderr 处理子进程的标准错误
func (pm *ProcessManager) handleStderr() {
	scanner := bufio.NewScanner(pm.stderr)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(os.Stderr, "【调试】从子进程接收(stderr): %s\n", line)

		// 尝试解析JSON，看是否包含会话ID
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err == nil {
			// 如果是有效的JSON，检查是否有目标会话ID
			if targetID, ok := event["sessionId"].(string); ok && targetID != "" {
				fmt.Fprintf(os.Stderr, "【调试】发现错误消息的目标会话ID: %s\n", targetID)

				// 确保有错误类型标记
				if _, hasType := event["type"]; !hasType {
					event["type"] = "error"
				}

				// 如果没有消息字段，添加一个
				if _, hasMessage := event["message"]; !hasMessage {
					event["message"] = line
				}

				// 删除会话ID，避免客户端混淆
				delete(event, "sessionId")

				// 发送到特定会话
				pm.sseServer.SendEventToSession(targetID, line)
				continue
			}
		}

		// 如果不是有效的JSON或没有目标会话ID，创建错误事件并广播
		errorEvent := map[string]interface{}{
			"type":    "error",
			"message": line,
		}

		errorEventJSON, _ := json.Marshal(errorEvent)
		pm.broadcastToAllSessions(string(errorEventJSON))
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "读取子进程标准错误输出错误: %v\n", err)
	}
}

// broadcastToAllSessions 将消息广播到所有活跃的SSE会话
// excludeSessionID 如果不为空，则排除该会话ID
func (pm *ProcessManager) broadcastToAllSessions(event string, excludeSessionID ...string) {

	// 调试: 打印广播的消息
	fmt.Fprintf(os.Stderr, "【调试】广播消息: %s\n", event)

	// 创建排除会话的集合，便于快速查找
	excludeMap := make(map[string]bool)
	for _, id := range excludeSessionID {
		if id != "" {
			excludeMap[id] = true
		}
	}

	// 遍历所有会话并发送事件
	sessionCount := 0
	pm.sseServer.sessions.Range(func(key, value interface{}) bool {
		sessionID := key.(string)

		// 如果会话ID在排除列表中，则跳过
		if excludeMap[sessionID] {
			fmt.Fprintf(os.Stderr, "【调试】跳过排除的会话: %s\n", sessionID)
			return true
		}

		sessionCount++
		pm.sseServer.SendEventToSession(sessionID, event)
		return true
	})

	fmt.Fprintf(os.Stderr, "【调试】消息已发送到 %d 个会话\n", sessionCount)
}

// WaitForResponse 等待特定会话ID的响应，带有超时
func (pm *ProcessManager) WaitForResponse(sessionID string, requestID interface{}, timeout time.Duration) (json.RawMessage, error) {
	// 创建一个通道用于接收响应
	responseChan := make(chan json.RawMessage, 1)

	// 创建一个上下文，用于取消监听
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 创建一个唯一的监听器ID
	listenerID := fmt.Sprintf("%s-%v-%d", sessionID, requestID, time.Now().UnixNano())

	// 注册响应监听器
	pm.mu.Lock()
	if pm.responseListeners == nil {
		pm.responseListeners = make(map[string]map[string]chan json.RawMessage)
	}
	if pm.responseListeners[sessionID] == nil {
		pm.responseListeners[sessionID] = make(map[string]chan json.RawMessage)
	}
	pm.responseListeners[sessionID][listenerID] = responseChan
	pm.mu.Unlock()

	// 确保在函数返回时删除监听器
	defer func() {
		pm.mu.Lock()
		if listeners, ok := pm.responseListeners[sessionID]; ok {
			delete(listeners, listenerID)
			if len(listeners) == 0 {
				delete(pm.responseListeners, sessionID)
			}
		}
		pm.mu.Unlock()
	}()

	fmt.Fprintf(os.Stderr, "【调试】等待会话 %s 的请求 %v 的响应\n", sessionID, requestID)

	// 等待响应或超时
	select {
	case response := <-responseChan:
		fmt.Fprintf(os.Stderr, "【调试】收到会话 %s 的请求 %v 的响应\n", sessionID, requestID)
		return response, nil
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "【调试】等待会话 %s 的请求 %v 的响应超时\n", sessionID, requestID)
		return nil, fmt.Errorf("等待响应超时")
	}
}

// NotifyResponseListeners 通知特定会话ID的所有响应监听器
func (pm *ProcessManager) NotifyResponseListeners(sessionID string, response json.RawMessage) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.responseListeners == nil || pm.responseListeners[sessionID] == nil {
		return false
	}

	listeners := pm.responseListeners[sessionID]
	if len(listeners) == 0 {
		return false
	}

	fmt.Fprintf(os.Stderr, "【调试】通知会话 %s 的 %d 个响应监听器\n", sessionID, len(listeners))

	// 通知所有监听器
	for id, ch := range listeners {
		select {
		case ch <- response:
			fmt.Fprintf(os.Stderr, "【调试】成功通知监听器 %s\n", id)
		default:
			fmt.Fprintf(os.Stderr, "【调试】监听器 %s 的通道已满或已关闭\n", id)
		}
	}

	return true
}
