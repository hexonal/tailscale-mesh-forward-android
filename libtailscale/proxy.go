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
// 负责监听 8339 端口，处理 SOCKS5、HTTP、HTTP CONNECT 代理请求
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
func startProxyService(_ string) error {
	log.Printf("startProxyService: called, will listen on 0.0.0.0:8339")
	// 加锁，保证全局唯一实例
	proxyMu.Lock()
	defer proxyMu.Unlock()

	// 如果已经有运行中的代理，直接返回
	if globalProxyService != nil && globalProxyService.running {
		log.Printf("startProxyService: already running")
		return nil // 已经启动
	}

	// 统一监听 0.0.0.0:8339，提升通用性
	addr := "0.0.0.0:8339"
	// 监听 TCP 端口，若端口被占用或权限不足会报错
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Printf("startProxyService: failed to listen on %s: %v", addr, err)
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
	log.Printf("SOCKS5 proxy started on %s", addr)

	return nil
}

// stopProxyService 停止代理服务，优雅关闭监听器和所有连接。
func stopProxyService() {
	log.Printf("stopProxyService: called")
	// 加锁，保证全局唯一实例
	proxyMu.Lock()
	defer proxyMu.Unlock()
	// 如果没有运行中的代理，直接返回
	if globalProxyService == nil {
		log.Printf("stopProxyService: no running proxy")
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
	log.Printf("SOCKS5 proxy stopped")
	globalProxyService = nil
}

// serve 主服务循环，持续接受新连接，每个连接独立 goroutine 处理。
func (ps *ProxyService) serve() {
	log.Printf("ProxyService.serve: started on %s", ps.addr)
	for {
		select {
		case <-ps.ctx.Done():
			// 上下文被取消，退出服务
			log.Printf("ProxyService.serve: context done, exiting")
			return
		default:
			// 接受新连接，Accept 会阻塞直到有新连接或监听器关闭
			conn, err := ps.listener.Accept()
			if err != nil {
				if ps.ctx.Err() != nil {
					log.Printf("ProxyService.serve: listener closed, exiting")
					return // 监听器已关闭
				}
				log.Printf("ProxyService.serve: accept error: %v", err)
				continue // 临时错误，继续接受
			}
			log.Printf("ProxyService.serve: accepted connection from %s", conn.RemoteAddr())
			// 每个连接独立处理，防止阻塞主循环
			go ps.handleConnection(conn)
		}
	}
}

// handleConnection 处理单个 TCP 连接，自动识别协议类型并分发到对应处理函数。
func (ps *ProxyService) handleConnection(conn net.Conn) {
	log.Printf("handleConnection: new connection from %s", conn.RemoteAddr())
	defer func() {
		log.Printf("handleConnection: closing connection from %s", conn.RemoteAddr())
		conn.Close()
	}()
	// 设置初始读取超时，防止恶意连接阻塞
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	// 协议检测，自动识别 HTTP/CONNECT/SOCKS5
	protocol, reader, err := ps.detectProtocol(conn)
	if err != nil {
		log.Printf("handleConnection: detectProtocol error: %v", err)
		return
	}
	// 协议识别后，取消超时，避免后续长连接被误杀
	conn.SetReadDeadline(time.Time{})
	log.Printf("handleConnection: Protocol: %s from %s", protocol, conn.RemoteAddr())
	// 分发到对应协议处理
	switch protocol {
	case "HTTP":
		ps.handleHTTP(conn, reader)
	case "CONNECT":
		ps.handleHTTPConnect(conn, reader)
	case "SOCKS5":
		ps.handleSOCKS5(conn, reader)
	default:
		ps.handleSOCKS5(conn, reader)
	}
}

// detectProtocol 协议检测，读取前 10 字节判断协议类型。
// 返回协议类型、带缓存的 reader、错误。
func (ps *ProxyService) detectProtocol(conn net.Conn) (string, *bufio.Reader, error) {
	log.Printf("detectProtocol: detecting protocol for %s", conn.RemoteAddr())
	reader := bufio.NewReader(conn)
	// 预读前 10 字节，避免破坏后续流
	firstBytes, err := reader.Peek(10)
	if err != nil {
		log.Printf("detectProtocol: peek error: %v", err)
		return "", nil, err
	}
	if len(firstBytes) < 1 {
		return "SOCKS5", reader, nil
	}
	// 检查是否为 HTTP 方法
	if len(firstBytes) >= 3 {
		httpMethods := []string{"GET ", "POST ", "PUT ", "DELETE ", "HEAD ", "OPTIONS ", "CONNECT "}
		for _, method := range httpMethods {
			if len(firstBytes) >= len(method) && string(firstBytes[:len(method)]) == method {
				if method == "CONNECT " {
					log.Printf("detectProtocol: detected CONNECT")
					return "CONNECT", reader, nil
				}
				log.Printf("detectProtocol: detected HTTP")
				return "HTTP", reader, nil
			}
		}
	}
	// 检查 SOCKS5 协议头
	if firstBytes[0] == 0x05 {
		log.Printf("detectProtocol: detected SOCKS5")
		return "SOCKS5", reader, nil
	}
	log.Printf("detectProtocol: defaulting to SOCKS5")
	return "SOCKS5", reader, nil // 默认按 SOCKS5 处理
}

// handleSOCKS5 完整的 SOCKS5 代理实现，先协商认证，再处理 CONNECT 请求，最后转发数据。
func (ps *ProxyService) handleSOCKS5(conn net.Conn, reader *bufio.Reader) {
	// 认证协商，Android 客户端仅支持无认证
	if !ps.socks5Auth(conn, reader) {
		return // 认证失败
	}
	// 处理 CONNECT 请求
	if !ps.socks5Connect(conn, reader) {
		return // 连接目标失败
	}
}

// socks5Auth SOCKS5 认证协商，只支持无认证（0x00）。
func (ps *ProxyService) socks5Auth(conn net.Conn, reader *bufio.Reader) bool {
	buf := make([]byte, 2)
	// 读取版本号和方法数量
	if _, err := io.ReadFull(reader, buf); err != nil {
		return false
	}
	version, nmethods := buf[0], buf[1]
	if version != 0x05 {
		return false // 只支持 SOCKS5
	}
	methods := make([]byte, nmethods)
	// 读取所有方法
	if _, err := io.ReadFull(reader, methods); err != nil {
		return false
	}
	// 回复 0x00 表示无需认证
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return false
	}
	return true
}

// socks5Connect SOCKS5 连接处理，只支持 CONNECT 命令，支持 IPv4 和域名，不支持 IPv6。
func (ps *ProxyService) socks5Connect(conn net.Conn, reader *bufio.Reader) bool {
	buf := make([]byte, 4)
	// 读取请求头
	if _, err := io.ReadFull(reader, buf); err != nil {
		log.Printf("socks5Connect: read header error: %v", err)
		return false
	}
	version, cmd, _, atyp := buf[0], buf[1], buf[2], buf[3]
	if version != 0x05 {
		log.Printf("socks5Connect: unsupported version: %v", version)
		return false
	}
	if cmd != 0x01 {
		log.Printf("socks5Connect: unsupported cmd: %v", cmd)
		ps.socks5Reply(conn, 0x07) // 不支持的命令
		return false
	}
	var targetAddr string
	// 解析目标地址
	switch atyp {
	case 0x01: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(reader, addr); err != nil {
			log.Printf("socks5Connect: read IPv4 error: %v", err)
			return false
		}
		targetAddr = net.IP(addr).String()
	case 0x03: // 域名
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			log.Printf("socks5Connect: read domain length error: %v", err)
			return false
		}
		domain := make([]byte, length[0])
		if _, err := io.ReadFull(reader, domain); err != nil {
			log.Printf("socks5Connect: read domain error: %v", err)
			return false
		}
		targetAddr = string(domain)
	case 0x04: // IPv6 不支持
		log.Printf("socks5Connect: unsupported address type: IPv6")
		ps.socks5Reply(conn, 0x08)
		return false
	default:
		log.Printf("socks5Connect: unknown address type: %v", atyp)
		ps.socks5Reply(conn, 0x08)
		return false
	}
	// 读取端口
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		log.Printf("socks5Connect: read port error: %v", err)
		return false
	}
	port := int(portBytes[0])<<8 + int(portBytes[1])
	target := fmt.Sprintf("%s:%d", targetAddr, port)
	log.Printf("SOCKS5 connecting to %s", target)
	// 连接目标服务器，10 秒超时，防止阻塞
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("Failed to connect to %s: %v", target, err)
		ps.socks5Reply(conn, 0x05) // 连接失败
		return false
	}
	defer targetConn.Close()
	// 回复成功，通知客户端可以开始转发
	ps.socks5Reply(conn, 0x00)
	log.Printf("socks5Connect: connection established, relaying")
	// 开始转发数据，支持全双工
	ps.relay(conn, targetConn)
	return true
}

// socks5Reply 发送 SOCKS5 响应。
// rep: 响应码，0x00=成功，0x05=连接失败，0x07=命令不支持，0x08=地址类型不支持。
func (ps *ProxyService) socks5Reply(conn net.Conn, rep byte) {
	response := []byte{
		0x05, rep, 0x00, 0x01, // VER, REP, RSV, ATYP(IPv4)
		0x00, 0x00, 0x00, 0x00, // BND.ADDR (0.0.0.0)
		0x00, 0x00, // BND.PORT (0)
	}
	conn.Write(response)
}

// relay 双向数据转发，使用 io.Copy 实现客户端与目标服务器之间的全双工数据转发。
func (ps *ProxyService) relay(client, target net.Conn) {
	done := make(chan struct{}, 2)
	log.Printf("relay: start relaying between %s and %s", client.RemoteAddr(), target.RemoteAddr())
	// 客户端到目标服务器
	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(target, client)
		log.Printf("relay: client to target done")
		target.Close()
	}()
	// 目标服务器到客户端
	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(client, target)
		log.Printf("relay: target to client done")
		client.Close()
	}()
	// 任一方向结束即关闭
	<-done
	log.Printf("relay: finished relaying")
}

// handleHTTP 处理普通 HTTP 代理请求，这里只做简单回显。
func (ps *ProxyService) handleHTTP(conn net.Conn, reader *bufio.Reader) {
	// 读取 HTTP 请求，若格式不符直接返回
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("handleHTTP: ReadRequest error: %v", err)
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
	log.Printf("HTTP %s %s", req.Method, req.URL.String())
}

// handleHTTPConnect 处理 HTTP CONNECT 隧道请求，建立与目标服务器的 TCP 隧道并转发数据。
func (ps *ProxyService) handleHTTPConnect(conn net.Conn, reader *bufio.Reader) {
	// 读取 HTTP CONNECT 请求
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("handleHTTPConnect: ReadRequest error: %v", err)
		return
	}
	target := req.Host
	log.Printf("HTTP CONNECT to %s", target)
	// 连接目标服务器，10 秒超时
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("handleHTTPConnect: failed to connect to %s: %v", target, err)
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()
	// 告知客户端隧道建立成功
	conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	log.Printf("handleHTTPConnect: tunnel established, relaying")
	// 开始转发数据
	ps.relay(conn, targetConn)
}
