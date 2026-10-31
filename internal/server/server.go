// Package server 是对外的 HTTP 层：三种入站协议、密钥鉴权、请求日志与面板 API。
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/today2api-panel/internal/config"
	"github.com/misakano7545/today2api-panel/internal/pool"
)

// Server 是网关本体。
type Server struct {
	store   *config.Store
	pool    *pool.Pool
	logs    *Log
	started time.Time
	version string

	mu    sync.Mutex
	sess  map[string]time.Time // 面板会话 token → 过期时间
	httpc *http.Client         // 面板侧直连上游（登录/探测）用

	modelsMu    sync.Mutex
	modelsCache []string
	modelsAt    time.Time
}

// New 组装 Server。
func New(store *config.Store, version string) *Server {
	return &Server{
		store:   store,
		pool:    pool.New(),
		logs:    newLog(300),
		started: time.Now(),
		version: version,
		sess:    map[string]time.Time{},
		httpc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.Handle("GET /v1/models", s.gate(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST /v1/chat/completions", s.gate(http.HandlerFunc(s.handleChatCompletions)))
	mux.Handle("POST /v1/responses", s.gate(http.HandlerFunc(s.handleResponses)))
	mux.Handle("POST /v1/messages", s.gate(http.HandlerFunc(s.handleMessages)))
	mux.Handle("POST /messages", s.gate(http.HandlerFunc(s.handleMessages)))

	s.panelRoutes(mux)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "today2api-panel",
		"version": s.version,
		"uptime":  int(time.Since(s.started).Seconds()),
	})
}

// gate 校验下游密钥；没配密钥时不拦（面板会提示）。
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.store.Snapshot()
		if len(cfg.Keys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		key := clientKey(r)
		if key == "" {
			writeError(w, http.StatusUnauthorized, "missing api key")
			return
		}
		sum := sha256.Sum256([]byte(key))
		hash := hex.EncodeToString(sum[:])
		for _, k := range cfg.Keys {
			if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(hash)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid api key")
	})
}

func clientKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// loopback 判断请求是否来自本机。
func loopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------- 请求日志 ----------

// Entry 是一行请求日志。
type Entry struct {
	Time     time.Time `json:"time"`
	Path     string    `json:"path"`
	Model    string    `json:"model"`
	Account  string    `json:"account,omitempty"`
	Status   int       `json:"status"`
	Duration int64     `json:"duration_ms"`
	Chars    int       `json:"chars"`
	Error    string    `json:"error,omitempty"`
}

// Log 是定长环形日志。
type Log struct {
	mu   sync.Mutex
	buf  []Entry
	head int
	n    int
}

func newLog(size int) *Log { return &Log{buf: make([]Entry, size)} }

func (l *Log) add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.head] = e
	l.head = (l.head + 1) % len(l.buf)
	if l.n < len(l.buf) {
		l.n++
	}
}

// Recent 返回最新的 n 条（新到旧）。
func (l *Log) Recent(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > l.n {
		n = l.n
	}
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, l.buf[(l.head-1-i+len(l.buf)*2)%len(l.buf)])
	}
	return out
}

// ---------- 小工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 输出 OpenAI 形状的错误体（Anthropic 客户端也能读出 message 字段）。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "gateway_error",
			"code":    status,
		},
	})
}

func writeSSE(w http.ResponseWriter, event string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if event != "" {
		if _, errWrite := w.Write([]byte("event: " + event + "\n")); errWrite != nil {
			return errWrite
		}
	}
	if _, errWrite := w.Write([]byte("data: ")); errWrite != nil {
		return errWrite
	}
	if _, errWrite := w.Write(buf); errWrite != nil {
		return errWrite
	}
	_, errWrite := w.Write([]byte("\n\n"))
	return errWrite
}

func sseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return len([]rune(text))/2 + 1
}
