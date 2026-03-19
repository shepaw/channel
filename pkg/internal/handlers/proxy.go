package handlers

import (
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/edenzou/channel-service/pkg/internal/services/proxy"
	"github.com/gorilla/websocket"
)

// ProxyHandler 负责 HTTP/HTTPS/WebSocket/TCP/UDP/隧道 代理转发
type ProxyHandler struct {
	channelSvc   *services.ChannelService
	rateLimitSvc *services.RateLimitService
	channelSvcV2 *services.ChannelServiceV2
	tunnelMgr    *services.TunnelManager // 隧道管理器（可为 nil，表示不支持隧道）

	httpProxies sync.Map // channelID -> *proxy.HTTPProxy
	wsProxies   sync.Map // channelID -> *proxy.WebSocketProxy
	tcpProxies  sync.Map // channelID -> *TCPProxy
	udpProxies  sync.Map // channelID -> *UDPProxy
}

func NewProxyHandler(channelSvc *services.ChannelService, rateLimitSvc *services.RateLimitService, v2 *services.ChannelServiceV2) *ProxyHandler {
	return &ProxyHandler{
		channelSvc:   channelSvc,
		rateLimitSvc: rateLimitSvc,
		channelSvcV2: v2,
	}
}

// SetTunnelManager 注入隧道管理器（在 main 中调用）
func (h *ProxyHandler) SetTunnelManager(tm *services.TunnelManager) {
	h.tunnelMgr = tm
}

// ServeHTTP 分发 HTTP/HTTPS/WebSocket 请求
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, channelID string) {
	channel, err := h.channelSvc.GetChannelByID(channelID)
	if err != nil {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}
	if !channel.IsActive {
		http.Error(w, "Channel is disabled", http.StatusServiceUnavailable)
		return
	}

	clientIP := extractClientIP(r)

	switch channel.Type {
	case "ws":
		h.handleWebSocket(w, r, channelID, channel.Target, clientIP)
	case "http", "https":
		h.handleHTTP(w, r, channelID, channel.Target, clientIP)
	case "tunnel-http":
		h.handleTunnelHTTP(w, r, channelID, clientIP)
	case "tunnel-tcp":
		http.Error(w, "tunnel-tcp 不支持 HTTP 访问，请直接使用 TCP 客户端", http.StatusBadRequest)
	default:
		http.Error(w, "TCP/UDP channels are not accessible via HTTP", http.StatusBadRequest)
	}
}

func (h *ProxyHandler) handleHTTP(w http.ResponseWriter, r *http.Request, channelID, target, clientIP string) {
	var p *proxy.HTTPProxy
	if cached, ok := h.httpProxies.Load(channelID); ok {
		p = cached.(*proxy.HTTPProxy)
	} else {
		np, err := proxy.NewHTTPProxy(target)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to create proxy: %v", err), http.StatusInternalServerError)
			return
		}
		h.httpProxies.Store(channelID, np)
		p = np
	}
	p.ServeHTTP(w, r, channelID, clientIP, h.rateLimitSvc, h.channelSvc)
}

func (h *ProxyHandler) handleWebSocket(w http.ResponseWriter, r *http.Request, channelID, target, clientIP string) {
	var p *proxy.WebSocketProxy
	if cached, ok := h.wsProxies.Load(channelID); ok {
		p = cached.(*proxy.WebSocketProxy)
	} else {
		np, err := proxy.NewWebSocketProxy(target)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to create WS proxy: %v", err), http.StatusInternalServerError)
			return
		}
		h.wsProxies.Store(channelID, np)
		p = np
	}
	p.ServeHTTP(w, r, channelID, clientIP, h.rateLimitSvc, h.channelSvc)
}

// ─── TCP ─────────────────────────────────────────────────────────────────────

type TCPProxy struct {
	target       string
	channelID    string
	rateLimitSvc *services.RateLimitService
	channelSvc   *services.ChannelService
	listener     net.Listener
}

// StartTCPProxy 启动 TCP 监听（实现 TCPUDPStarter 接口）
func (h *ProxyHandler) StartTCPProxy(channelID, target string, listenPort int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		return fmt.Errorf("TCP listen :%d failed: %v", listenPort, err)
	}

	tp := &TCPProxy{
		target:       target,
		channelID:    channelID,
		rateLimitSvc: h.rateLimitSvc,
		channelSvc:   h.channelSvc,
		listener:     listener,
	}
	h.tcpProxies.Store(channelID, tp)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("TCP %s accept error: %v", channelID, err)
				return
			}
			go tp.handleConn(conn)
		}
	}()

	return nil
}

func (tp *TCPProxy) handleConn(clientConn net.Conn) {
	defer clientConn.Close()

	clientIP := strings.Split(clientConn.RemoteAddr().String(), ":")[0]

	allowed, _, err := tp.rateLimitSvc.CheckConnections(tp.channelID)
	if err != nil || !allowed {
		log.Printf("TCP %s: connection limit exceeded", tp.channelID)
		return
	}
	defer tp.rateLimitSvc.DecrementConnections(tp.channelID)

	targetConn, err := net.Dial("tcp", tp.target)
	if err != nil {
		log.Printf("TCP %s: dial target %s failed: %v", tp.channelID, tp.target, err)
		return
	}
	defer targetConn.Close()

	var totalBytes int64
	var wg sync.WaitGroup
	wg.Add(2)

	pipe := func(src, dst net.Conn, trackBW bool) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if trackBW {
					ok, _, _ := tp.rateLimitSvc.CheckBandwidth(tp.channelID, clientIP, int64(n))
					if !ok {
						return
					}
				}
				totalBytes += int64(n)
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}

	go pipe(clientConn, targetConn, true)
	go pipe(targetConn, clientConn, false)
	wg.Wait()

	go tp.channelSvc.UpdateChannelStats(tp.channelID, totalBytes, 1, 0)
}

// ─── UDP ─────────────────────────────────────────────────────────────────────

type UDPProxy struct {
	target       string
	channelID    string
	rateLimitSvc *services.RateLimitService
	channelSvc   *services.ChannelService
	conn         *net.UDPConn
}

// StartUDPProxy 启动 UDP 监听（实现 TCPUDPStarter 接口）
func (h *ProxyHandler) StartUDPProxy(channelID, target string, listenPort int) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		return fmt.Errorf("UDP resolve :%d failed: %v", listenPort, err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("UDP listen :%d failed: %v", listenPort, err)
	}

	up := &UDPProxy{
		target:       target,
		channelID:    channelID,
		rateLimitSvc: h.rateLimitSvc,
		channelSvc:   h.channelSvc,
		conn:         conn,
	}
	h.udpProxies.Store(channelID, up)

	go up.listen()
	return nil
}

func (up *UDPProxy) listen() {
	defer up.conn.Close()

	targetAddr, err := net.ResolveUDPAddr("udp", up.target)
	if err != nil {
		log.Printf("UDP %s: cannot resolve target %s: %v", up.channelID, up.target, err)
		return
	}

	var clients sync.Map // clientAddr string -> *net.UDPConn
	buf := make([]byte, 65535)

	for {
		n, clientAddr, err := up.conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP %s: read error: %v", up.channelID, err)
			return
		}

		clientIP := clientAddr.IP.String()
		ok, _, _ := up.rateLimitSvc.CheckBandwidth(up.channelID, clientIP, int64(n))
		if !ok {
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		go func(data []byte, from *net.UDPAddr) {
			key := from.String()
			var tConn *net.UDPConn

			if v, ok := clients.Load(key); ok {
				tConn = v.(*net.UDPConn)
			} else {
				nc, err := net.DialUDP("udp", nil, targetAddr)
				if err != nil {
					log.Printf("UDP %s: dial target error: %v", up.channelID, err)
					return
				}
				clients.Store(key, nc)
				tConn = nc

				// 反向流量回传客户端
				go func() {
					defer nc.Close()
					defer clients.Delete(key)
					rbuf := make([]byte, 65535)
					for {
						n, rerr := nc.Read(rbuf)
						if rerr != nil {
							return
						}
						up.conn.WriteToUDP(rbuf[:n], from)
					}
				}()
			}

			tConn.Write(data)
		}(data, clientAddr)
	}
}

// ─── 隧道转发（tunnel-http） ──────────────────────────────────────────────────

// handleTunnelHTTP 通过 TunnelManager 将请求下发给本地 agent，再将响应回写
func (h *ProxyHandler) handleTunnelHTTP(w http.ResponseWriter, r *http.Request, channelID, clientIP string) {
	// Detect WebSocket upgrade requests and handle them separately
	if isWebSocketUpgrade(r) {
		h.handleTunnelWS(w, r, channelID, clientIP)
		return
	}

	if h.tunnelMgr == nil {
		http.Error(w, "Tunnel not supported", http.StatusInternalServerError)
		return
	}

	if !h.tunnelMgr.IsOnline(channelID) {
		http.Error(w, "Local agent is offline — please start the channel-agent on your machine", http.StatusServiceUnavailable)
		return
	}

	resp, err := h.tunnelMgr.ForwardHTTP(channelID, r)
	if err != nil {
		http.Error(w, fmt.Sprintf("Tunnel forward error: %v", err), http.StatusBadGateway)
		return
	}

	// 写回响应头
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if resp.Status > 0 {
		w.WriteHeader(resp.Status)
	}

	// 写回响应体（base64 解码）
	if resp.Body != "" {
		body, err := base64.StdEncoding.DecodeString(resp.Body)
		if err == nil {
			w.Write(body)
		}
	}
}

// isWebSocketUpgrade returns true if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

var tunnelWsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleTunnelWS upgrades the client connection and proxies the WebSocket
// through the tunnel to the local agent.
func (h *ProxyHandler) handleTunnelWS(w http.ResponseWriter, r *http.Request, channelID, clientIP string) {
	if h.tunnelMgr == nil {
		http.Error(w, "Tunnel not supported", http.StatusInternalServerError)
		return
	}
	if !h.tunnelMgr.IsOnline(channelID) {
		http.Error(w, "Local agent is offline", http.StatusServiceUnavailable)
		return
	}

	// Upgrade the connection with the external caller
	clientConn, err := tunnelWsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade failed for channel %s: %v", channelID, err)
		return
	}

	// Build full path+query (passed as-is to agent; agent strips /proxy/{channel_id} prefix)
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path = path + "?" + r.URL.RawQuery
	}

	// Collect headers to forward (including Authorization), skip WS handshake-only headers
	headers := make(map[string]string)
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "upgrade" || lk == "connection" ||
			lk == "sec-websocket-key" || lk == "sec-websocket-version" ||
			lk == "sec-websocket-extensions" || lk == "host" {
			continue
		}
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}

	streamID := h.tunnelMgr.NextStreamID()
	if err := h.tunnelMgr.ForwardWS(channelID, streamID, path, headers, clientConn); err != nil {
		log.Printf("WS tunnel forward error for channel %s: %v", channelID, err)
	}
}

// extractClientIP 获取真实客户端 IP。
// 只有在服务部署于可信反向代理（Nginx/LB）后面时才应信任 X-Forwarded-For，
// 否则客户端可以伪造该 header 绕过 IP 限流。
// 当前策略：直接使用 RemoteAddr，不信任任何客户端 header。
// 如需在反向代理后部署，请将 TRUSTED_PROXY=true 环境变量传入，届时取 XFF 最左侧 IP。
func extractClientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
