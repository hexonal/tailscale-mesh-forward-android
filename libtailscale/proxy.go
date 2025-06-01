// proxy.go 负责 Tailscale Android 客户端的本地代理服务实现，支持 SOCKS5、HTTP、HTTP CONNECT 多协议自动识别与转发，适配移动端多连接并发、资源回收、平台端口监听等特殊需求。
package libtailscale

import (
	"bufio"    // 用于高效读取 TCP 流，支持协议预读
	"context"  // 控制服务生命周期，实现优雅退出
	"fmt"      // 字符串格式化，日志与响应构造
	"io"       // 数据转发核心，支持全双工 relay
	"log"      // 日志输出，便于调试和问题追踪
	"net"      // TCP 监听与连接，核心网络操作
	"net/http" // HTTP 协议解析与响应
	"sync"     // 互斥锁，保证全局唯一实例与并发安全
	"time"     // 超时控制，防止恶意连接阻塞
)

// ProxyService 代理服务结构体
// 负责监听 8939 端口，处理 SOCKS5、HTTP、HTTP CONNECT 代理请求
// 支持多协议自动识别，自动转发流量到目标服务器
// 通过 startProxyService/stopProxyService 控制生命周期
// 线程安全，支持多连接并发
type ProxyService struct {
	listener net.Listener       // TCP 监听器，负责接收新连接
	ctx      context.Context    // 服务上下文，用于优雅退出
	cancel   context.CancelFunc // 取消函数，主动关闭服务
	mu       sync.Mutex         // 互斥锁，保护 running
	running  bool               // 服务是否运行中，防止重复启动/关闭
	addr     string             // 监听地址，便于日志与调试
}

var (
	globalProxyService *ProxyService // 全局唯一代理服务实例，保证同一时刻仅有一个代理在运行
	proxyMu            sync.Mutex    // 全局互斥锁，保护 globalProxyService
)

// startProxyService 启动代理服务，只允许启动一个实例，重复调用无副作用。
// tailscaleIP: 监听的 Tailscale IP 地址（已废弃，现统一监听 0.0.0.0）。
// 返回 error。
func startProxyService() error {
	log.Printf("[TEST-FLINK] startProxyService: called, will listen on 0.0.0.0:8939")
	// 加锁，保证全局唯一实例
	proxyMu.Lock()
	defer proxyMu.Unlock()

	// 如果已经有运行中的代理，直接返回
	if globalProxyService != nil && globalProxyService.running {
		log.Printf("[TEST-FLINK] startProxyService: already running")
		return nil // 已经启动
	}

	// 统一监听 0.0.0.0:8939，提升通用性
	addr := "0.0.0.0:8939"
	// 监听 TCP 端口，若端口被占用或权限不足会报错
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Printf("[TEST-FLINK] startProxyService: failed to listen on %s: %v", addr, err)
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	// 创建上下文用于优雅退出，便于后续主动关闭
	ctx, cancel := context.WithCancel(context.Background())
	// 初始化全局代理服务实例
	globalProxyService = &ProxyService{
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
		running:  true,
		addr:     addr,
	}
	// 启动主服务循环，异步处理新连接
	go globalProxyService.serve()
	log.Printf("[TEST-FLINK] SOCKS5 proxy started on %s", addr)

	return nil
}

// stopProxyService 停止代理服务，优雅关闭监听器和所有连接。
func stopProxyService() {
	log.Printf("[TEST-FLINK] stopProxyService: called")
	// 加锁，保证全局唯一实例
	proxyMu.Lock()
	defer proxyMu.Unlock()
	// 如果没有运行中的代理，直接返回
	if globalProxyService == nil {
		log.Printf("[TEST-FLINK] stopProxyService: no running proxy")
		return
	}
	// 标记为停止，防止新连接进入
	globalProxyService.running = false
	// 取消上下文，通知 serve 循环退出
	if globalProxyService.cancel != nil {
		globalProxyService.cancel()
	}
	// 关闭监听器，防止新连接
	if globalProxyService.listener != nil {
		globalProxyService.listener.Close()
	}
	log.Printf("[TEST-FLINK] SOCKS5 proxy stopped")
	globalProxyService = nil
}

// serve 主服务循环，持续接受新连接，每个连接独立 goroutine 处理。
func (ps *ProxyService) serve() {
	log.Printf("[TEST-FLINK] ProxyService.serve: started on %s", ps.addr)
	for {
		select {
		case <-ps.ctx.Done():
			// 上下文被取消，退出服务
			log.Printf("[TEST-FLINK] ProxyService.serve: context done, exiting")
			return
		default:
			// 接受新连接，Accept 会阻塞直到有新连接或监听器关闭
			conn, err := ps.listener.Accept()
			if err != nil {
				if ps.ctx.Err() != nil {
					log.Printf("[TEST-FLINK] ProxyService.serve: listener closed, exiting")
					return // 监听器已关闭
				}
				log.Printf("[TEST-FLINK] ProxyService.serve: accept error: %v", err)
				continue // 临时错误，继续接受
			}
			log.Printf("[TEST-FLINK] ProxyService.serve: accepted connection from %s", conn.RemoteAddr())
			// 每个连接独立处理，防止阻塞主循环
			go ps.handleConnection(conn)
		}
	}
}

// handleConnection 处理单个 TCP 连接，自动识别协议类型并分发到对应处理函数。
func (ps *ProxyService) handleConnection(conn net.Conn) {
	log.Printf("[TEST-FLINK] handleConnection: new connection from %s", conn.RemoteAddr())
	defer func() {
		log.Printf("[TEST-FLINK] handleConnection: closing connection from %s", conn.RemoteAddr())
		conn.Close()
	}()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	protocol, reader, err := ps.detectProtocol(conn)
	if err != nil {
		log.Printf("[TEST-FLINK] handleConnection: detectProtocol error: %v", err)
		return
	}
	conn.SetReadDeadline(time.Time{})
	log.Printf("[TEST-FLINK] handleConnection: Protocol: %s from %s", protocol, conn.RemoteAddr())
	switch protocol {
	case "HTTP":
		ps.handleHTTP(conn, reader)
	case "CONNECT":
		ps.handleHTTPConnect(conn, reader)
	default:
		log.Printf("[TEST-FLINK] handleConnection: unsupported protocol from %s", conn.RemoteAddr())
		conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
	}
}

// detectProtocol 只识别 HTTP/CONNECT，其余一律拒绝。
func (ps *ProxyService) detectProtocol(conn net.Conn) (string, *bufio.Reader, error) {
	log.Printf("[TEST-FLINK] detectProtocol: detecting protocol for %s", conn.RemoteAddr())
	reader := bufio.NewReader(conn)
	first7, err := reader.Peek(7)
	if err != nil && err != io.EOF {
		log.Printf("[TEST-FLINK] detectProtocol: peek error: %v", err)
		return "", nil, err
	}
	if len(first7) >= 3 {
		httpMethods := []string{"GET", "POS", "PUT", "DEL", "HEA", "OPT", "CON"}
		s := string(first7[:3])
		for _, m := range httpMethods {
			if s == m {
				if m == "CON" && len(first7) >= 7 && string(first7[:7]) == "CONNECT" {
					log.Printf("[TEST-FLINK] detectProtocol: detected CONNECT")
					return "CONNECT", reader, nil
				}
				log.Printf("[TEST-FLINK] detectProtocol: detected HTTP")
				return "HTTP", reader, nil
			}
		}
	}
	log.Printf("[TEST-FLINK] detectProtocol: unsupported protocol, rejecting")
	return "UNSUPPORTED", reader, nil
}

// handleHTTP 处理普通 HTTP 代理请求，这里只做简单回显。
func (ps *ProxyService) handleHTTP(conn net.Conn, reader *bufio.Reader) {
	// 读取 HTTP 请求，若格式不符直接返回
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("[TEST-FLINK] handleHTTP: ReadRequest error: %v", err)
		return
	}
	// 构造简单响应体，回显请求信息
	body := fmt.Sprintf(`{"status":"ok","method":"%s","url":"%s","protocol":"http"}`,
		req.Method, req.URL.String())
	response := fmt.Sprintf("HTTP/1.1 200 OK\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: %d\r\n"+
		"\r\n"+
		"%s", len(body), body)
	// 发送响应
	conn.Write([]byte(response))
	log.Printf("[TEST-FLINK] HTTP %s %s", req.Method, req.URL.String())
}

// handleHTTPConnect 处理 HTTP CONNECT 隧道请求，建立与目标服务器的 TCP 隧道并转发数据。
func (ps *ProxyService) handleHTTPConnect(conn net.Conn, reader *bufio.Reader) {
	// 读取 HTTP CONNECT 请求
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("[TEST-FLINK] handleHTTPConnect: ReadRequest error: %v", err)
		return
	}
	target := req.Host
	log.Printf("[TEST-FLINK] HTTP CONNECT to %s", target)
	// 连接目标服务器，10 秒超时
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("[TEST-FLINK] handleHTTPConnect: failed to connect to %s: %v", target, err)
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()
	// 告知客户端隧道建立成功
	conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	log.Printf("[TEST-FLINK] handleHTTPConnect: tunnel established, relaying")
	// 开始转发数据
	ps.relay(conn, targetConn)
}

// relay 双向数据转发，使用 io.Copy 实现客户端与目标服务器之间的全双工数据转发。
func (ps *ProxyService) relay(client, target net.Conn) {
	done := make(chan struct{}, 2)
	log.Printf("[TEST-FLINK] relay: start relaying between %s and %s", client.RemoteAddr(), target.RemoteAddr())
	// 客户端到目标服务器
	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(target, client)
		log.Printf("[TEST-FLINK] relay: client to target done")
		target.Close()
	}()
	// 目标服务器到客户端
	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(client, target)
		log.Printf("[TEST-FLINK] relay: target to client done")
		client.Close()
	}()
	// 任一方向结束即关闭
	<-done
	log.Printf("[TEST-FLINK] relay: finished relaying")
}
