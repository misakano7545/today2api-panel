package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/today2api-panel/internal/today"
)

// 记忆的读与写。
//
// 读：记忆内容的宿主是 WebDAV（桌面端 PROPFIND /memory），但那条路由挂在
// api.todayai.cn 上、本机出口被 ALB 403；能通的 /api/cloud 是 Next 的 rewrite，
// 实测承载不了 DAV。所以读改成走 /v1/memories/pins —— 它返回被钉住目录下
// 最近三个文件的**正文**，足够渲染。
//
// 写：没有 PUT 记忆的 REST 接口，能用的只有「导入」这条：
//
//	POST /v1/files             multipart      → 201 {file:{fileRef}}
//	POST /v1/memories/upload   {"fileRef":{}}  → 202 {task:{id}}
//	GET  /v2/tasks/{id}        轮询到 completed → agent 已把内容写进 Memory
//
// 语义是导入不是覆写：上游 agent 会抽取内容、自己选目录、重写格式。
// 接口文案必须照实说，不然用起来会以为能改某一行。

func (s *Server) memoryRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/panel/accounts/{id}/memories", s.panelAuth(http.HandlerFunc(s.handleMemories)))
	mux.Handle("POST /api/panel/accounts/{id}/memories/pin", s.panelAuth(http.HandlerFunc(s.handleMemoryPin)))
	mux.Handle("POST /api/panel/accounts/{id}/memories/unpin", s.panelAuth(http.HandlerFunc(s.handleMemoryUnpin)))
	mux.Handle("POST /api/panel/accounts/{id}/memories/import", s.panelAuth(http.HandlerFunc(s.handleMemoryImport)))
	mux.Handle("GET /api/panel/accounts/{id}/memories/import/{taskId}", s.panelAuth(http.HandlerFunc(s.handleMemoryImportTask)))
}

func (s *Server) handleMemories(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	pins, err := client.Pins(ctx)
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if pins == nil {
		pins = []today.MemoryPin{}
	}

	total := 0
	for _, p := range pins {
		total += len(p.Files)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":  acct.ID,
		"pins":     pins,
		"files":    total,
		"writable": "import",
		"note":     "读取走 /v1/memories/pins（DAV 在本机网络不可达）；写入是「导入」语义，由上游 agent 决定落盘路径与格式。",
	})
}

func (s *Server) handleMemoryPin(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	path := strings.TrimSpace(body.Path)
	if path == "" {
		writeError(w, http.StatusBadRequest, "path 不能为空，例如 /memory/notes")
		return
	}
	if !strings.HasPrefix(path, "/memory/") {
		writeError(w, http.StatusBadRequest, "path 必须以 /memory/ 开头")
		return
	}

	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	pin, err := client.Pin(ctx, path)
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": acct.ID, "pin": pin})
}

func (s *Server) handleMemoryUnpin(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := client.Unpin(ctx, strings.TrimSpace(body.Path)); err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": acct.ID, "unpinned": body.Path})
}

// handleMemoryImport 两步走：先传文件拿到 fileRef，再让上游导入。
//
// wait 秒数大于 0 时顺带轮询到终态，省掉客户端自己再发一次请求；
// 上限 60 秒，超时就返回当前状态（导入本身已经在上游排队，不会丢）。
func (s *Server) handleMemoryImport(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	var body struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
		WaitSec  int    `json:"wait_sec"`
		// Path 非空即定点写入：必须是 /memory/<...>，同路径再写是替换。
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, "content 不能为空")
		return
	}
	name := strings.TrimSpace(body.Filename)
	if name == "" {
		name = "memory.md"
	}
	wait := body.WaitSec
	if wait < 0 {
		wait = 0
	}
	if wait > 60 {
		wait = 60
	}

	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	base, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	file, err := client.UploadFile(base, name, "text/markdown", []byte(body.Content))
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	target := strings.TrimSpace(body.Path)
	var task *today.MemoryImportTask
	var idempotent bool
	if target != "" {
		task, idempotent, err = client.ImportMemoryTo(base, file.FileRef, target)
	} else {
		task, idempotent, err = client.ImportMemory(base, file.FileRef)
	}
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// 记忆导入是异步的，落在请求日志里方便事后对账。
	// 轮询后的终态不在返回值里也能从 /memories/import/{taskId} 再取。

	// 轮询到终态；失败不影响已经排上的任务。
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for wait > 0 && time.Now().Before(deadline) {
		if isTerminalTaskStatus(task.Status) {
			break
		}
		time.Sleep(1500 * time.Millisecond)
		latest, errPoll := client.MemoryImportTask(base, task.ID)
		if errPoll != nil {
			break
		}
		task = latest
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"account":    acct.ID,
		"file":       file,
		"task":       task,
		"idempotent": idempotent,
		"path":       target,
		"terminal":   isTerminalTaskStatus(task.Status),
	})
}

func (s *Server) handleMemoryImportTask(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	task, err := client.MemoryImportTask(ctx, r.PathValue("taskId"))
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":  acct.ID,
		"task":     task,
		"terminal": isTerminalTaskStatus(task.Status),
	})
}

func isTerminalTaskStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "completed", "failed", "cancelled", "canceled", "stopped":
		return true
	}
	return false
}
