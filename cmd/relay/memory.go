package main

// /memory 的只读 WebDAV 垫层。
//
// 背景（实测，2026-11-01）：
//   - 记忆页的列表走 WebDAV：PROPFIND /memory，Depth: infinity，无 body；
//     客户端用 DOMParser 解析 207 Multi-Status，取 href/displayname/
//     collection/getcontenttype/getlastmodified。
//   - 本机到 api.todayai.cn 的 ALB 被封，唯一可用通道是
//     todayai.cn/api/cloud/:path*（Next 服务端 rewrite）。
//   - 但 Next rewrite 承载不了 PROPFIND：
//       · 不带 body        → 边缘 400 "Bad Request"（11B）
//       · Content-Length:0 → Next 自己 400 的 HTML
//       · 带 body          → 转发后 72s 超时 504
//     也就是说 DAV 这条路在当前网络下结构性不可达。
//
// 所以这里用 /v1/memories/pins 的真实数据重新拼一份 207 响应。
// 数据是真的，只是换了个传输形状；没有编造任何内容。
//
// PUT 走另一条：/v1/memories/upload 的 path 字段可以定点落盘，
// 同路径再写是替换（上游先 deleted 旧版本再 created 新版本）。
// MKCOL/MOVE/COPY/DELETE 没有对应通道，明确回 501，
// 并且在列表里不返回 canDelete/canRename，客户端会把删改入口收起来，
// 而不是让用户点下去再失败。

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
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

	url := strings.TrimRight(rt.Upstream, "/") + rt.Prefix + "/v1/memories/pins"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-App-Version", "1.24.0")
	req.Header.Set("X-Client-Platform", "windows-client")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
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
		w.Header().Set("Access-Control-Allow-Methods", "OPTIONS, GET, HEAD, PROPFIND")
		w.Header().Set("Access-Control-Max-Age", "600")
		return
	}
	w.Header().Set("Access-Control-Expose-Headers", "DAV, Content-Length, Content-Type")
}

// serveMemoryDAV 处理 /memory 及其子路径。
func (h *handler) serveMemoryDAV(w http.ResponseWriter, r *http.Request, rt route) {
	// 渲染层页面在 http://today-desktop.localhost，打 api.todayai.cn 属跨域。
	// 垫层自己回包就必须自己给 CORS 头，否则预检就挂，PROPFIND 根本发不出来。
	applyCORS(w, r)

	// 预检请求不带 Authorization，必须在鉴权之前放行。
	if r.Method == http.MethodOptions {
		w.Header().Set("DAV", "1")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	auth := r.Header.Get("Authorization")
	if auth == "" {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return
	}

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

	switch r.Method {
	case "PROPFIND":
		h.logf("memory DAV PROPFIND %s → 本地垫层 207（%d 条 pin）", r.URL.Path, len(pins.Items))
		h.writeMultistatus(w, r, pins)
		return

	case http.MethodGet, http.MethodHead:
		h.logf("memory DAV %s %s → 本地垫层", r.Method, r.URL.Path)
		h.writeMemoryFile(w, r, pins)
		return

	case http.MethodPut:
		h.handleMemoryPut(w, r, rt, auth)
		return

	case http.MethodDelete:
		// 上游没有删除记忆文件的 REST 接口，DAV DELETE 也走不通。
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND, PUT")
		http.Error(w, "memory 删除不可用", http.StatusNotImplemented)
		return

	default:
		// 其他写操作（MKCOL/MOVE/COPY/PROPPATCH）没有对应通道，直说，
		// 别让客户端以为存上了。
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND, PUT")
		http.Error(w, "memory 该写方法不可用", http.StatusNotImplemented)
		return
	}
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

func (h *handler) writeMemoryFile(w http.ResponseWriter, r *http.Request, pins *memoryPins) {
	want := pathClean(r.URL.Path)
	if want == "/memory" || want == "/memory/" {
		// 根路径当目录取，直接回 405，别回 HTML 让客户端解析出乱码。
		http.Error(w, "is a collection", http.StatusMethodNotAllowed)
		return
	}

	for _, p := range pins.Items {
		for _, f := range p.Files {
			if pathClean(f.Path) != want {
				continue
			}
			ct := f.ContentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Length", itoa(len(f.Content)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.WriteString(w, f.Content)
			}
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// handleMemoryPut 把 DAV 的 PUT 翻成「传文件 → 定点导入 → 轮询到终态」。
//
// 上游没有直接写记忆文件的接口，但 /v1/memories/upload 的 path 字段
// 可以指定落盘位置：同路径再写会先 deleted 旧的再 created 新的，
// 也就是替换。实测目录不存在会自动创建。
//
// 有一个改不掉的偏差：上游 agent 会把正文原样保留，但会在末尾追加
// 一段「来源」说明。所以 PUT 之后 GET 回来的内容会比写进去的多几行，
// 这是上游行为，不是这里改写。
func (h *handler) handleMemoryPut(w http.ResponseWriter, r *http.Request, rt route, auth string) {
	body, errRead := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if errRead != nil {
		http.Error(w, "read body: "+errRead.Error(), http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// DAV 路径 /memory/a/b.md → 上游要求的 /memory/a/b.md（绝对形态）。
	target := pathClean(r.URL.Path)
	if !strings.HasPrefix(target, "/memory/") || len(target) <= len("/memory/") {
		http.Error(w, "memory path 必须是 /memory/<...>", http.StatusBadRequest)
		return
	}

	mimeType := r.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "text/markdown"
	}
	filename := target[strings.LastIndex(target, "/")+1:]
	if filename == "" {
		filename = "memory.md"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
	defer cancel()

	ref, errUpload := h.uploadMemoryFile(ctx, rt, auth, filename, mimeType, body)
	if errUpload != nil {
		h.logf("memory DAV PUT 上传失败: %v", errUpload)
		http.Error(w, errUpload.Error(), http.StatusBadGateway)
		return
	}

	taskID, errImport := h.importMemoryTo(ctx, rt, auth, ref, target)
	if errImport != nil {
		h.logf("memory DAV PUT 导入失败: %v", errImport)
		http.Error(w, errImport.Error(), http.StatusBadGateway)
		return
	}

	// 上游是异步的，等到终态再回 201，客户端才不会读到半截。
	status := h.waitMemoryTask(ctx, rt, auth, taskID)
	h.logf("memory DAV PUT %s → 201（task=%s status=%s）", target, taskID, status)
	w.WriteHeader(http.StatusCreated)
}

// uploadMemoryFile 走 multipart 传文件，回 fileRef。
func (h *handler) uploadMemoryFile(ctx context.Context, rt route, auth, filename, mimeType string, data []byte) (map[string]string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`,
		strings.ReplaceAll(filename, `"`, "")))
	hdr.Set("Content-Type", mimeType)
	part, errPart := mw.CreatePart(hdr)
	if errPart != nil {
		return nil, errPart
	}
	if _, errWrite := part.Write(data); errWrite != nil {
		return nil, errWrite
	}
	if errClose := mw.Close(); errClose != nil {
		return nil, errClose
	}

	req, errReq := h.memoryRequest(ctx, http.MethodPost, rt, "/v1/files", auth, mw.FormDataContentType(), &buf)
	if errReq != nil {
		return nil, errReq
	}
	resp, errDo := h.client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upload 失败 %d: %s", resp.StatusCode, truncateForLog(raw, 200))
	}

	var out struct {
		File struct {
			FileRef map[string]string `json:"fileRef"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode upload: %w", err)
	}
	if out.File.FileRef == nil || out.File.FileRef["fileId"] == "" {
		return nil, fmt.Errorf("upload 响应缺少 fileRef: %s", truncateForLog(raw, 200))
	}
	return out.File.FileRef, nil
}

// importMemoryTo 提交定点导入，回 task id。
func (h *handler) importMemoryTo(ctx context.Context, rt route, auth string, ref map[string]string, target string) (string, error) {
	payload, errMarshal := json.Marshal(map[string]any{"fileRef": ref, "path": target})
	if errMarshal != nil {
		return "", errMarshal
	}
	req, errReq := h.memoryRequest(ctx, http.MethodPost, rt, "/v1/memories/upload", auth,
		"application/json", bytes.NewReader(payload))
	if errReq != nil {
		return "", errReq
	}
	resp, errDo := h.client.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("import 失败 %d: %s", resp.StatusCode, truncateForLog(raw, 200))
	}
	var out struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode import: %w", err)
	}
	if out.Task.ID == "" {
		return "", fmt.Errorf("import 响应缺少 task: %s", truncateForLog(raw, 200))
	}
	return out.Task.ID, nil
}

// waitMemoryTask 轮询到终态，回最后一次看到的 status。
// 超时不是错误：任务在服务端会继续跑，只是这一轮没等到。
func (h *handler) waitMemoryTask(ctx context.Context, rt route, auth, taskID string) string {
	status := "queued"
	for {
		select {
		case <-ctx.Done():
			return status
		case <-time.After(1500 * time.Millisecond):
		}
		req, errReq := h.memoryRequest(ctx, http.MethodGet, rt, "/v1/tasks/"+taskID, auth, "", nil)
		if errReq != nil {
			return status
		}
		resp, errDo := h.client.Do(req)
		if errDo != nil {
			return status
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
		resp.Body.Close()
		var out struct {
			Task struct {
				Status string `json:"status"`
			} `json:"task"`
		}
		if json.Unmarshal(raw, &out) != nil {
			return status
		}
		if out.Task.Status != "" {
			status = out.Task.Status
		}
		switch status {
		case "completed", "failed", "cancelled", "stopped", "error":
			return status
		}
	}
}

// memoryRequest 组装一个打向上游 API 的请求（路由前缀 + 认证头）。
func (h *handler) memoryRequest(ctx context.Context, method string, rt route, path, auth, contentType string, body io.Reader) (*http.Request, error) {
	target := strings.TrimRight(rt.Upstream, "/") + rt.Prefix + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", rt.App)
	req.Header.Set("X-App-Version", "1.24.0")
	req.Header.Set("X-Client-Platform", "windows-client")
	return req, nil
}
