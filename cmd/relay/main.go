// Command relay 是给「被上游 AWS ALB 按源 IP 拦掉」的网络准备的本机中继。
//
// 背景（实测，见 docs/reverse-engineering.md 第 7 节）：
//   - api.todayai.cn / auth.todayai.cn / api.today.ai / auth.today.ai 前置 AWS ALB，
//     对部分出口 IP 直接回 403 text/html，请求根本到不了应用；
//   - 而前端站点 todayai.cn / today.ai 挂在别处且带 Next.js 服务端 rewrite：
//     /api/cloud/:path* → https://api.todayai.cn/:path*，
//     /api/auth/[...all] 就是 better-auth 本体。
//
// 所以只要把 *.todayai.cn / *.today.ai 的 API 与 Auth 请求改道到前端站点，
// 就能从被拦的网络里正常访问上游。
//
// relay 做三件事：
//  1. 在本地监听 TLS（自签 CA + 覆盖两个域名的叶子证书），按 SNI 分发；
//  2. API 域名补上 /api/cloud 前缀，Auth 域名原样转发；
//  3. 把响应原样回给客户端（含 Set-Cookie，但去掉 Domain 属性，避免被当跨域丢弃）。
//
// 用法：
//
//	relay -listen 127.0.0.1:8443 -dir ./relay-cert          # 先验证
//	relay -listen 127.0.0.1:443  -dir ./relay-cert          # 配合 hosts 正式用
//
// 配套：
//
//	certutil -addstore -user Root ./relay-cert/ca.pem       # 让 Node/Electron 信任
//	setx NODE_EXTRA_CA_CERTS "C:\path\relay-cert\ca.pem"    # Node 不走系统库，得显式给
//	# hosts: 127.0.0.1 api.todayai.cn auth.todayai.cn api.today.ai auth.today.ai
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// route 是一个 SNI 域名到上游的映射。前缀非空时拼到路径前面。
//
// Audience 与 App 只给「换票合成」用：桌面端拿的是
// `GET {authBaseUrl}/api/auth/token`（better-auth bearer 插件），
// 而前端站点只暴露 `POST {app}/api/token`（Cookie 鉴权）。中转层把
// Bearer 翻成 Cookie 再打一次，响应原样回给客户端即可。
type route struct {
	Upstream string
	Prefix   string
	Audience string
	App      string
}

// 只支持国内版：国际版（today.ai）已移除，不再为它做中转。
var routes = map[string]route{
	"api.todayai.cn":  {Upstream: "https://todayai.cn", Prefix: "/api/cloud", Audience: "https://api.todayai.cn", App: "https://todayai.cn"},
	"auth.todayai.cn": {Upstream: "https://todayai.cn", Audience: "https://api.todayai.cn", App: "https://todayai.cn"},
}

var sans = []string{
	"api.todayai.cn", "auth.todayai.cn",
}

func main() {
	listen := flag.String("listen", "127.0.0.1:443", "监听地址")
	dir := flag.String("dir", "relay-cert", "证书目录（CA + 叶子）")
	verbose := flag.Bool("v", true, "打印每条转发")
	dumpToken := flag.String("dump-token", "", "把客户端正在使用的 Bearer JWT 写到该文件（0600），用于离线复现请求")
	cookieFile := flag.String("cookie-file", "relay-cookies.json", "签名会话 Cookie 的落盘文件，重启后仍能换票")
	flag.Parse()

	cert, _, err := ensureCert(*dir)
	if err != nil {
		log.Fatalf("cert: %v", err)
	}
	log.Printf("relay listening on https://%s", *listen)
	log.Printf("CA 文件: %s", filepath.Join(*dir, "ca.pem"))
	for host, rt := range routes {
		log.Printf("  %-16s → %s%s", host, rt.Upstream, rt.Prefix)
	}

	upstreamClient := &http.Client{
		Timeout: 120 * time.Second,
		// 出站走向真实上游，必须用系统根证书；本地 CA 只用于入站监听。
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConnsPerHost: 16,
			ForceAttemptHTTP2:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	srv := &http.Server{
		Addr:    *listen,
		Handler: newHandler(upstreamClient, *verbose, *dumpToken, *cookieFile),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{*cert},
			NextProtos:   []string{"http/1.1"},
		},
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

type handler struct {
	client  *http.Client
	verbose bool
	mu      sync.Mutex

	// 登录时上游会下发签过名的会话 Cookie（<raw>.<hmac>），而桌面端手里
	// 只有库里存的 raw token。中转层把两者的对应关系缓存下来，
	// 换票时就能把 raw 还原成可用的 Cookie。
	cookieMu sync.Mutex
	signed   map[string]string // raw token -> 签名 Cookie 值
	lastSeen map[string]string // auth host -> 最近一次签名 Cookie 值

	// dumpToken 非空时，把客户端正在用的 Bearer JWT 落到该文件（0600），
	// 便于用同一张票离线复现请求、抓完整报文。
	dumpToken string

	// cookieFile 存签名 Cookie 映射：中继重启后仍能把 raw token 还原成
	// 可用的会话 Cookie，否则客户端下一次换票会 401。
	cookieFile string
}

func newHandler(client *http.Client, verbose bool, dumpToken, cookieFile string) *handler {
	h := &handler{
		client:     client,
		verbose:    verbose,
		signed:     map[string]string{},
		lastSeen:   map[string]string{},
		dumpToken:  dumpToken,
		cookieFile: cookieFile,
	}
	h.loadCookies()
	return h
}

// captureBearer 把客户端正在用的 JWT 落盘。
func (h *handler) captureBearer(r *http.Request) {
	if h.dumpToken == "" {
		return
	}
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	if token == "" {
		return
	}
	h.cookieMu.Lock()
	defer h.cookieMu.Unlock()
	if cur, err := os.ReadFile(h.dumpToken); err == nil && string(cur) == token {
		return
	}
	_ = os.WriteFile(h.dumpToken, []byte(token), 0o600)
}

// rememberCookie 从 Set-Cookie 里抽出签名值，按 raw token 建索引。
func (h *handler) rememberCookie(host string, setCookies []string, body []byte) {
	for _, sc := range setCookies {
		seg := strings.TrimSpace(strings.SplitN(sc, ";", 2)[0])
		name, value, ok := strings.Cut(seg, "=")
		if !ok || !strings.Contains(name, "session_token") || value == "" {
			continue
		}
		h.cookieMu.Lock()
		h.lastSeen[host] = value
		// 登录响应体里带 raw token，能和签名的对起来时建精确映射。
		var payload struct {
			Token string `json:"token"`
		}
		if len(body) > 0 && json.Unmarshal(body, &payload) == nil && payload.Token != "" {
			h.signed[payload.Token] = value
			// 签名值形如 <raw>.<sig>，反过来也能索引一次。
			if raw, _, found := strings.Cut(value, "."); found {
				h.signed[raw] = value
			}
		}
		h.cookieMu.Unlock()
		h.saveCookies()
	}
}

// loadCookies 启动时恢复映射。
func (h *handler) loadCookies() {
	if h.cookieFile == "" {
		return
	}
	raw, err := os.ReadFile(h.cookieFile)
	if err != nil {
		return
	}
	var out struct {
		Signed   map[string]string `json:"signed"`
		LastSeen map[string]string `json:"last_seen"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return
	}
	if out.Signed != nil {
		h.signed = out.Signed
	}
	if out.LastSeen != nil {
		h.lastSeen = out.LastSeen
	}
	h.logf("已恢复 %d 条会话 Cookie 映射", len(h.signed))
}

// saveCookies 落盘（0600）。Cookie 是登录态，权限必须收紧。
func (h *handler) saveCookies() {
	if h.cookieFile == "" {
		return
	}
	h.cookieMu.Lock()
	defer h.cookieMu.Unlock()
	raw, err := json.MarshalIndent(struct {
		Signed   map[string]string `json:"signed"`
		LastSeen map[string]string `json:"last_seen"`
	}{h.signed, h.lastSeen}, "", " ")
	if err != nil {
		return
	}
	_ = os.WriteFile(h.cookieFile, raw, 0o600)
}

// resolveCookie 把客户端手里的 raw token 换成签名 Cookie 值。
func (h *handler) resolveCookie(host, token string) string {
	h.cookieMu.Lock()
	defer h.cookieMu.Unlock()
	if v, ok := h.signed[token]; ok {
		return v
	}
	if v, ok := h.lastSeen[host]; ok {
		return v
	}
	return token
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Host)
	rt, ok := routes[host]
	if !ok {
		http.Error(w, "unknown host "+host, http.StatusBadGateway)
		return
	}

	h.captureBearer(r)

	// 桌面端换票：GET /api/auth/token + Bearer <session cookie 值>。
	// 前端站点没有 better-auth 的 bearer 插件，只有 POST /api/token（Cookie 鉴权），
	// 这里把 Bearer 翻成 Cookie 再打一次。
	if r.Method == http.MethodGet && r.URL.Path == "/api/auth/token" {
		h.exchangeToken(w, r, rt)
		return
	}

	// 实时通道：真实 WS 服务在 AWS ALB 后面（本机不可达），Vercel 的同源代理
	// 又建不了 Upgrade。这里由中继自己完成握手并保持连接，
	// 让客户端的 ws 重试循环停下来；业务数据本身就都走 HTTP。
	if r.URL.Path == "/ws" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		h.shimWebSocket(w, r)
		return
	}

	target := strings.TrimRight(rt.Upstream, "/") + rt.Prefix + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	outReq.ContentLength = r.ContentLength
	// 原样带上所有业务头；Host 与逐跳头交给 Go。
	for k, vs := range r.Header {
		switch k {
		case "Host", "Connection", "Proxy-Connection", "Keep-Alive",
			"Transfer-Encoding", "Upgrade", "Te", "Trailer":
			continue
		}
		for _, v := range vs {
			outReq.Header.Add(k, v)
		}
	}

	resp, err := h.client.Do(outReq)
	if err != nil {
		h.logf("%s %s %s → ERR %v", r.Method, host, r.URL.Path, err)
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	setCookies := resp.Header.Values("Set-Cookie")
	// auth 域名的响应要读下来，登录时用来把 raw token 和签名 Cookie 对上。
	var bodyBytes []byte
	if rt.Prefix == "" {
		bodyBytes, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		h.rememberCookie(host, setCookies, bodyBytes)
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			// 去掉 Domain：让浏览器/Electron 按请求主机存，而不是被判定为跨域拒收。
			if strings.EqualFold(k, "Set-Cookie") {
				v = stripCookieDomain(v)
			}
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	var n int64
	if bodyBytes != nil {
		written, _ := w.Write(bodyBytes)
		n = int64(written)
	} else {
		n, _ = io.Copy(w, resp.Body)
	}
	h.logf("%s %s%s → %s %d (%dB)", r.Method, host, r.URL.Path, target, resp.StatusCode, n)
}

func (h *handler) logf(format string, args ...any) {
	if !h.verbose {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	log.Printf(format, args...)
}

// exchangeToken 把桌面端的 bearer 换票请求翻成前端站点的 Cookie 换票。
//
// 客户端：GET {authBase}/api/auth/token，Authorization: Bearer <session token>
// 上游：  POST {app}/api/token，Cookie: __Secure-better-auth.session_token=<session token>
//
//	body {"audience": <官方 API>, "expiresIn": 3600}
//
// 响应原样回传（客户端只取 token 字段，过期时间自己解 JWT exp）。
func (h *handler) exchangeToken(w http.ResponseWriter, r *http.Request, rt route) {
	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if bearer == "" {
		bearer = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "bearer "))
	}
	if bearer == "" {
		http.Error(w, "missing session token", http.StatusUnauthorized)
		return
	}
	body := fmt.Sprintf(`{"audience":%q,"expiresIn":3600}`, rt.Audience)
	cookie := h.resolveCookie(stripPort(r.Host), bearer)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimRight(rt.App, "/")+"/api/token", strings.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", rt.App)
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+cookie)
	for _, k := range []string{"X-App-Version", "X-Client-Platform", "X-Device-Id", "X-Timezone", "Accept-Language"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.logf("GET %s/api/auth/token → 合成失败 %v", r.Host, err)
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	h.logf("GET %s/api/auth/token → POST %s/api/token %d (%dB)", r.Host, rt.App, resp.StatusCode, n)
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// shimWebSocket 在本地完成 WebSocket 握手并保持连接。
//
// 真实 /ws 在 AWS ALB 后面，本机出口被拦；Vercel 的同源代理只转发 HTTP，
// 建不了 Upgrade。客户端拿到 404 会按 socket watchdog 节奏无限重试，
// 界面上表现为「该功能不可用」。这里给它一个"已连接"的通道：
// 握手成功、应答 ping/pong、接收 close，业务数据继续走 HTTP。
func (h *handler) shimWebSocket(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n"
	// 客户端若带了子协议，服务端必须回显其中一个，否则 undici 会判定握手失败
	// （表现为“socket never opened”，然后无限重连）。
	if proto := r.Header.Get("Sec-WebSocket-Protocol"); proto != "" {
		first := strings.TrimSpace(strings.Split(proto, ",")[0])
		if first != "" {
			resp += "Sec-WebSocket-Protocol: " + first + "\r\n"
		}
	}
	resp += "\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}
	h.logf("WS  %s%s → 本地 shim 已接入（proto=%q ext=%q）", r.Host, r.URL.Path, r.Header.Get("Sec-WebSocket-Protocol"), r.Header.Get("Sec-WebSocket-Extensions"))

	for {
		op, payload, err := readWSFrame(brw.Reader)
		if err != nil {
			return
		}
		switch op {
		case 0x8: // close
			_ = writeWSFrame(brw.Writer, 0x8, payload)
			_ = brw.Flush()
			return
		case 0x9: // ping → pong
			_ = writeWSFrame(brw.Writer, 0xA, payload)
			_ = brw.Flush()
		case 0x1, 0x2: // text / binary
			if h.verbose {
				h.logf("WS  ← %s", truncateForLog(payload, 300))
			}
			h.replyHandshake(brw.Writer, payload)
			continue
		}
	}
}

// replyHandshake 按客户端的 cloud websocket 协议回应握手帧。
//
// 客户端（packages/socket/src/classic）状态机：
//
//	device.hello  → 等 cloud.hello_ack（必须带 messageProtocolVersion，
//	                否则判协议不兼容并主动断开）
//	device.resume → 等 cloud.resume_ok（带 replayedCount）
//
// 不回的话客户端在 SETUP/握手看门狗超时后重连，界面上就是一直"连接中"。
func (h *handler) replyHandshake(w *bufio.Writer, frame []byte) {
	var msg struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return
	}
	var out map[string]any
	switch msg.Type {
	case "device.hello":
		out = map[string]any{
			"id":                     newFrameID(),
			"timestamp":              time.Now().UTC().Format(time.RFC3339Nano),
			"type":                   "cloud.hello_ack",
			"messageProtocolVersion": "canonical-v1",
			"sessionId":              "relay-" + newFrameID(),
			"heartbeatIntervalMs":    30000,
			"replayedCount":          0,
		}
	case "device.resume":
		out = map[string]any{
			"id":                     newFrameID(),
			"timestamp":              time.Now().UTC().Format(time.RFC3339Nano),
			"type":                   "cloud.resume_ok",
			"messageProtocolVersion": "canonical-v1",
			"sessionId":              "relay-" + newFrameID(),
			"heartbeatIntervalMs":    30000,
			"replayedCount":          0,
		}
	default:
		return
	}
	body, err := json.Marshal(out)
	if err != nil {
		return
	}
	if err := writeWSFrame(w, 0x1, body); err != nil {
		return
	}
	_ = w.Flush()
	h.logf("WS  → %s", string(body))
}

// newFrameID 生成握手帧 id（客户端只要求有 id）。
func newFrameID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// truncateForLog 截断帧内容，避免日志被大帧淹没。
func truncateForLog(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// readWSFrame 读一个（被客户端掩码过的）WebSocket 帧。
func readWSFrame(r *bufio.Reader) (byte, []byte, error) {
	h0, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	h1, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	op := h0 & 0x0f
	masked := h1&0x80 != 0
	length := int(h1 & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int(ext[0])<<8 | int(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = 0
		for _, b := range ext {
			length = length<<8 | int(b)
		}
	}
	if length > 8<<20 {
		return 0, nil, fmt.Errorf("ws frame too large: %d", length)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

// writeWSFrame 写一个不掩码的服务端帧。
func writeWSFrame(w *bufio.Writer, op byte, payload []byte) error {
	if err := w.WriteByte(0x80 | op); err != nil {
		return err
	}
	n := len(payload)
	switch {
	case n < 126:
		if err := w.WriteByte(byte(n)); err != nil {
			return err
		}
	case n <= 0xffff:
		if err := w.WriteByte(126); err != nil {
			return err
		}
		if _, err := w.Write([]byte{byte(n >> 8), byte(n)}); err != nil {
			return err
		}
	default:
		if err := w.WriteByte(127); err != nil {
			return err
		}
		var ext [8]byte
		for i := 7; i >= 0; i-- {
			ext[i] = byte(n)
			n >>= 8
		}
		if _, err := w.Write(ext[:]); err != nil {
			return err
		}
	}
	_, err := w.Write(payload)
	return err
}

// stripCookieDomain 去掉 Set-Cookie 里的 Domain 属性。
func stripCookieDomain(v string) string {
	parts := strings.Split(v, ";")
	out := parts[:0]
	for _, p := range parts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), "domain=") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ";")
}

// ensureCert 准备 CA 与叶子证书；已存在则直接复用。
func ensureCert(dir string) (*tls.Certificate, *x509.CertPool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	caCertPath := filepath.Join(dir, "ca.pem")
	caKeyPath := filepath.Join(dir, "ca-key.pem")
	leafCertPath := filepath.Join(dir, "leaf.pem")
	leafKeyPath := filepath.Join(dir, "leaf-key.pem")

	if !exists(caCertPath) || !exists(caKeyPath) {
		if err := makeCA(caCertPath, caKeyPath); err != nil {
			return nil, nil, err
		}
		log.Printf("生成 CA: %s", caCertPath)
	}
	if !exists(leafCertPath) || !exists(leafKeyPath) {
		if err := makeLeaf(caCertPath, caKeyPath, leafCertPath, leafKeyPath); err != nil {
			return nil, nil, err
		}
		log.Printf("生成叶子证书: %s", leafCertPath)
	}

	cert, err := tls.LoadX509KeyPair(leafCertPath, leafKeyPath)
	if err != nil {
		return nil, nil, err
	}
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("CA 无法解析: %s", caCertPath)
	}
	return &cert, pool, nil
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

func makeCA(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "today2api local relay CA", Organization: []string{"today2api-panel"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(keyPath, "EC PRIVATE KEY", keyDER)
}

func makeLeaf(caCertPath, caKeyPath, certPath, keyPath string) error {
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return err
	}
	kb, _ := pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: sans[0], Organization: []string{"today2api-panel"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(keyPath, "EC PRIVATE KEY", keyDER)
}

func writePEM(path, blockType string, der []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}
