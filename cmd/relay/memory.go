package main

// /memory 的 WebDAV 垫层 —— 只补「列目录」这一个坏掉的方法。
//
// 实测结论（2026-11-01，真实 pro 账号，通道 todayai.cn/api/cloud）：
//
//	GET    /memory/<path>   200  真实正文
//	PUT    /memory/<path>   201  新建与覆盖都行
//	DELETE /memory/<path>   204
//	PROPFIND（任意层级）     ✗   无 body → 边缘 400；带 body → 504
//	MKCOL / PROPPATCH        ✗   404 / 504
//
// 所以文件级读写本来就是通的。早期「DAV 结构性不可达」的判断是错的——
// 当时只试了根路径的 PROPFIND，把「列表坏了」误当成「整个 DAV 坏了」，
// 于是垫层对写请求回了 501，等于把本来能用的功能自己封掉。坏掉的只有列举。
//
// 现在的分工：
//   - GET/HEAD/PUT/DELETE 原样转发，内容一字节不改（PUT 之后 GET 回来的
//     就是写进去的东西，不经过 agent 改写）；
//   - PROPFIND 用 /v1/memories/pins 的真实数据拼 207 —— 列表只覆盖
//     「被钉住的目录」，想看别的目录先钉它；
//   - MKCOL/MOVE/COPY/PROPPATCH 明确回 501，别让客户端以为存上了。
//
// 渲染层页面在 http://today-desktop.localhost，打 api.todayai.cn 属跨域，
// 所以垫层回包必须自带 CORS；预检不带 Authorization，必须在鉴权前放行。

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type memoryFile struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	UpdatedAt   string `json:"updatedAt"`
}

type memoryPin struct {
	Path  string       `json:"path"`
	Name  string       `json:"name"`
	Files []memoryFile `json:"files"`
}

type memoryPins struct {
	Items []memoryPin `json:"items"`
}

// pinsCache 让列表与随后的取文件共用一个响应，避免每点一个文件就重拉一次。
type pinsCache struct {
	mu   sync.Mutex
	at   time.Time
	key  string
	pins *memoryPins
}

const pinsTTL = 10 * time.Second

func (h *handler) fetchPins(rt route, auth string) (*memoryPins, error) {
	h.pins.mu.Lock()
	if h.pins.pins != nil && h.pins.key == auth && time.Since(h.pins.at) < pinsTTL {
		cached := h.pins.pins
		h.pins.mu.Unlock()
		return cached, nil
	}
	h.pins.mu.Unlock()

	req, errReq := h.memoryRequest(rt, http.MethodGet, "/v1/memories/pins", auth, "", nil)
	if errReq != nil {
		return nil, errReq
	}
	resp, errDo := h.client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &davError{status: resp.StatusCode, body: body}
	}

	out := &memoryPins{}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, err
	}

	h.pins.mu.Lock()
	h.pins.pins, h.pins.key, h.pins.at = out, auth, time.Now()
	h.pins.mu.Unlock()
	return out, nil
}

// invalidatePins 在写操作之后清缓存，否则紧接着的刷新还是旧列表。
func (h *handler) invalidatePins() {
	h.pins.mu.Lock()
	h.pins.pins = nil
	h.pins.mu.Unlock()
}

type davError struct {
	status int
	body   []byte
}

func (e *davError) Error() string { return "upstream " + http.StatusText(e.status) }

// applyCORS 按请求 Origin 回 CORS 头。带凭证时不能用 *，必须回显。
func applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Add("Vary", "Origin")

	if r.Method == http.MethodOptions {
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			w.Header().Set("Access-Control-Allow-Headers", req)
		}
		w.Header().Set("Access-Control-Allow-Methods", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND")
		w.Header().Set("Access-Control-Max-Age", "600")
		return
	}
	w.Header().Set("Access-Control-Expose-Headers", "DAV, Content-Length, Content-Type")
}

const memoryAllow = "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND"

// serveMemoryDAV 处理 /memory 及其子路径。
func (h *handler) serveMemoryDAV(w http.ResponseWriter, r *http.Request, rt route) {
	applyCORS(w, r)

	// 预检请求不带 Authorization，必须在鉴权之前放行。
	if r.Method == http.MethodOptions {
		w.Header().Set("DAV", "1")
		w.Header().Set("Allow", memoryAllow)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case "PROPFIND":
		pins, err := h.fetchPins(rt, auth)
		if err != nil {
			code := http.StatusBadGateway
			if de, ok := err.(*davError); ok {
				code = de.status
			}
			h.logf("memory: 拉 pins 失败: %v", err)
			http.Error(w, err.Error(), code)
			return
		}
		h.logf("memory DAV PROPFIND %s → 本地拼装 207（%d 条 pin）", r.URL.Path, len(pins.Items))
		h.writeMultistatus(w, r, pins)
		return

	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		// 文件级操作上游原生支持，原样转发，不经过任何转换。
		h.forwardMemory(w, r, rt, auth)
		return

	default:
		// MKCOL / MOVE / COPY / PROPPATCH：上游确实没有，直接说。
		w.Header().Set("Allow", memoryAllow)
		http.Error(w, "memory DAV: "+r.Method+" 不受支持（上游只支持 GET/PUT/DELETE）", http.StatusNotImplemented)
		return
	}
}

// forwardMemory 把请求原样转发到上游，响应原样回给客户端。
func (h *handler) forwardMemory(w http.ResponseWriter, r *http.Request, rt route, auth string) {
	req, errReq := h.memoryRequest(rt, r.Method, r.URL.RequestURI(), auth,
		r.Header.Get("Content-Type"), r.Body)
	if errReq != nil {
		http.Error(w, errReq.Error(), http.StatusBadGateway)
		return
	}
	req.ContentLength = r.ContentLength

	resp, errDo := h.client.Do(req)
	if errDo != nil {
		h.logf("memory DAV %s %s → ERR %v", r.Method, r.URL.Path, errDo)
		http.Error(w, errDo.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.invalidatePins()
	}

	for k, vs := range resp.Header {
		switch k {
		case "Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, io.LimitReader(resp.Body, 8<<20))
	h.logf("memory DAV %s %s → %d (%dB)", r.Method, r.URL.Path, resp.StatusCode, n)
}

func (h *handler) writeMultistatus(w http.ResponseWriter, r *http.Request, pins *memoryPins) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<D:multistatus xmlns:D="DAV:">` + "\n")

	writeResp := func(href, display string, isDir bool, ct, modified string) {
		b.WriteString("  <D:response>\n")
		b.WriteString("    <D:href>")
		xml.EscapeText(&b, []byte(href))
		b.WriteString("</D:href>\n")
		b.WriteString("    <D:propstat><D:prop>\n")
		b.WriteString("      <D:displayname>")
		xml.EscapeText(&b, []byte(display))
		b.WriteString("</D:displayname>\n")
		if isDir {
			b.WriteString("      <D:resourcetype><D:collection/></D:resourcetype>\n")
		} else {
			b.WriteString("      <D:resourcetype/>\n")
		}
		if ct != "" {
			b.WriteString("      <D:getcontenttype>")
			xml.EscapeText(&b, []byte(ct))
			b.WriteString("</D:getcontenttype>\n")
		}
		if modified != "" {
			b.WriteString("      <D:getlastmodified>")
			xml.EscapeText(&b, []byte(modified))
			b.WriteString("</D:getlastmodified>\n")
		}
		b.WriteString("      <D:status>HTTP/1.1 200 OK</D:status>\n")
		b.WriteString("    </D:prop></D:propstat>\n")
		b.WriteString("  </D:response>\n")
	}

	writeResp("/memory", "Memory", true, "", "")

	for _, p := range pins.Items {
		if p.Path == "" {
			continue
		}
		writeResp(p.Path, p.Name, true, "", "")
		for _, f := range p.Files {
			if f.Path == "" {
				continue
			}
			writeResp(f.Path, f.Name, false, f.ContentType, f.UpdatedAt)
		}
	}

	b.WriteString("</D:multistatus>\n")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("DAV", "1")
	w.WriteHeader(http.StatusMultiStatus)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, b.String())
	}
}

func pathClean(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// memoryRequest 组装一个打向上游 API 的请求（路由前缀 + 认证头）。
func (h *handler) memoryRequest(rt route, method, path, auth, contentType string, body io.Reader) (*http.Request, error) {
	target := strings.TrimRight(rt.Upstream, "/") + rt.Prefix + path
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json, text/markdown, */*")
	req.Header.Set("Origin", rt.App)
	req.Header.Set("X-App-Version", "1.24.0")
	req.Header.Set("X-Client-Platform", "windows-client")
	return req, nil
}
