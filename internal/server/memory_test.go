package server

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/misakano7545/today2api-panel/internal/config"
)

// 记忆读写的面板 API 行为，对齐实测的上游语义：
//
//	POST /v1/files            multipart → 201 {file:{fileRef:{fileId,versionId}}}
//	POST /v1/memories/upload  {"fileRef":{…}} → 202 {task:{id,status:queued}}
//	GET  /v2/tasks/{id}       轮询到 completed
//	GET/POST/DELETE /v1/memories/pins

type memoryMock struct {
	mu        sync.Mutex
	uploaded  int
	imports   int
	fileRef   map[string]any
	pins      []map[string]any
	pinned    []string
	unpinned  []string
	taskPolls int
}

func newMemoryMock(t *testing.T) (*httptest.Server, *memoryMock) {
	t.Helper()
	m := &memoryMock{pins: []map[string]any{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/token":
			_, _ = w.Write([]byte(`{"token":"jwt-test","expiresAt":"2099-01-01T00:00:00Z"}`))

		case r.URL.Path == "/v1/files" && r.Method == http.MethodPost:
			if err := r.ParseMultipartForm(4 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"bad multipart"}`))
				return
			}
			f, hdr, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing file"}`))
				return
			}
			body, _ := io.ReadAll(f)
			f.Close()

			m.mu.Lock()
			m.uploaded++
			m.fileRef = map[string]any{
				"fileId":    "nd_test",
				"versionId": "ver_test",
			}
			ref := m.fileRef
			m.mu.Unlock()

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"file": map[string]any{
				"id": "nd_test", "filename": hdr.Filename,
				"mimeType": hdr.Header.Get("Content-Type"),
				"size":     len(body), "fileRef": ref,
			}})

		case r.URL.Path == "/v1/memories/upload" && r.Method == http.MethodPost:
			var body struct {
				FileRef map[string]any `json:"fileRef"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.FileRef == nil || body.FileRef["fileId"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"Invalid request"}`))
				return
			}
			m.mu.Lock()
			m.imports++
			n := m.imports
			m.mu.Unlock()

			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{
				"id": "task_1", "status": "queued", "name": "将文件导入 Memory", "n": n,
			}})

		case strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
			m.mu.Lock()
			m.taskPolls++
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{
				"id": "task_1", "status": "completed",
				"completionSummary": "已读取并保存。保存位置：notes/test.md",
			}})

		case r.URL.Path == "/v1/memories/pins":
			switch r.Method {
			case http.MethodGet:
				m.mu.Lock()
				items := append([]map[string]any{}, m.pins...)
				m.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"items": items})

			case http.MethodPost:
				var body struct {
					Path string `json:"path"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if !strings.HasPrefix(body.Path, "/memory/") {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"Invalid request"}`))
					return
				}
				m.mu.Lock()
				m.pinned = append(m.pinned, body.Path)
				name := strings.TrimPrefix(body.Path, "/memory/")
				m.pins = append(m.pins, map[string]any{
					"path": body.Path, "name": name, "status": "active",
					"pinnedAt": "2026-11-01T07:31:25Z",
					"files": []any{map[string]any{
						"path": body.Path + "/test-memory.md", "name": "test-memory.md",
						"contentType": "text/markdown", "content": "这是一条由网关写入的测试记忆。",
					}},
				})
				m.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"pin": map[string]any{
					"path": body.Path, "name": name, "status": "active",
				}})

			case http.MethodDelete:
				var body struct {
					Path string `json:"path"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				m.mu.Lock()
				m.unpinned = append(m.unpinned, body.Path)
				kept := m.pins[:0]
				for _, p := range m.pins {
					if p["path"] != body.Path {
						kept = append(kept, p)
					}
				}
				m.pins = kept
				m.mu.Unlock()
				_, _ = w.Write([]byte(`{"ok":true}`))

			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	return srv, m
}

func newMemoryServer(t *testing.T, upstream string) *Server {
	t.Helper()
	return newTestServer(t, upstream, func(c *config.Config) {
		c.Accounts = []config.Account{
			{ID: "acc_1", Label: "main", Cookie: "__Secure-better-auth.session_token=fine", Enabled: true},
		}
	})
}

func TestPanelMemoryListEmpty(t *testing.T) {
	up, _ := newMemoryMock(t)
	s := newMemoryServer(t, up.URL)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodGet, "/api/panel/accounts/acc_1/memories", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Pins  []any  `json:"pins"`
		Files int    `json:"files"`
		Note  string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Pins == nil {
		t.Error("pins 应该是空数组而不是 null")
	}
	if !strings.Contains(out.Note, "导入") {
		t.Errorf("必须说明写入是导入语义，得到 %q", out.Note)
	}
}

func TestPanelMemoryImportWaitsForCompletion(t *testing.T) {
	up, mock := newMemoryMock(t)
	s := newMemoryServer(t, up.URL)

	body := `{"filename":"note.md","content":"# 测试记忆\n\n这是一条由网关写入的测试记忆。","wait_sec":10}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/memories/import", body))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		File struct {
			FileRef struct {
				FileID    string `json:"fileId"`
				VersionID string `json:"versionId"`
			} `json:"fileRef"`
		} `json:"file"`
		Task struct {
			ID                string `json:"id"`
			Status            string `json:"status"`
			CompletionSummary string `json:"completionSummary"`
		} `json:"task"`
		Terminal bool `json:"terminal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	if mock.uploaded != 1 || mock.imports != 1 {
		t.Fatalf("上传 %d 次、导入 %d 次，都应该是 1", mock.uploaded, mock.imports)
	}
	if out.File.FileRef.FileID != "nd_test" || out.File.FileRef.VersionID != "ver_test" {
		t.Errorf("fileRef 没透传: %+v", out.File)
	}
	if !out.Terminal || out.Task.Status != "completed" {
		t.Errorf("wait_sec>0 应轮询到终态，得到 %q terminal=%v", out.Task.Status, out.Terminal)
	}
	if !strings.Contains(out.Task.CompletionSummary, "notes/test.md") {
		t.Errorf("完成摘要应透传，得到 %q", out.Task.CompletionSummary)
	}
	if mock.taskPolls == 0 {
		t.Error("应该真的轮询过 /v1/tasks/{id}")
	}
}

func TestPanelMemoryImportRejectsEmptyContent(t *testing.T) {
	up, mock := newMemoryMock(t)
	s := newMemoryServer(t, up.URL)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/memories/import", `{"content":"  "}`))
	if rec.Code != 400 {
		t.Fatalf("空内容想要 400，得到 %d", rec.Code)
	}
	if mock.uploaded != 0 {
		t.Error("校验失败不应该碰上游")
	}
}

func TestPanelMemoryPinRoundTrip(t *testing.T) {
	up, mock := newMemoryMock(t)
	s := newMemoryServer(t, up.URL)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/memories/pin", `{"path":"/memory/notes"}`))
	if rec.Code != 200 {
		t.Fatalf("pin status %d: %s", rec.Code, rec.Body.String())
	}
	if len(mock.pinned) != 1 || mock.pinned[0] != "/memory/notes" {
		t.Fatalf("上游收到的 pin = %v", mock.pinned)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodGet, "/api/panel/accounts/acc_1/memories", nil))
	var out struct {
		Pins []struct {
			Path  string `json:"path"`
			Files []struct {
				Content string `json:"content"`
			} `json:"files"`
		} `json:"pins"`
		Files int `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pins) != 1 || out.Pins[0].Path != "/memory/notes" {
		t.Fatalf("pins = %+v", out.Pins)
	}
	if out.Files != 1 || !strings.Contains(out.Pins[0].Files[0].Content, "测试记忆") {
		t.Errorf("列表应带正文，得到 %+v", out.Pins)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/memories/unpin", `{"path":"/memory/notes"}`))
	if rec.Code != 200 || len(mock.unpinned) != 1 {
		t.Fatalf("unpin status %d unpinned=%v", rec.Code, mock.unpinned)
	}
}

func TestPanelMemoryPinRejectsBadPath(t *testing.T) {
	up, mock := newMemoryMock(t)
	s := newMemoryServer(t, up.URL)

	for _, body := range []string{`{"path":""}`, `{"path":"/etc/passwd"}`, `{"path":"memory/notes"}`} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/memories/pin", body))
		if rec.Code != 400 {
			t.Errorf("body %s 想要 400，得到 %d", body, rec.Code)
		}
	}
	if len(mock.pinned) != 0 {
		t.Error("非法路径不应该打到上游")
	}
}

// 上传必须真的用 multipart，不能用 application/json 硬塞。
func TestMemoryUploadSendsMultipart(t *testing.T) {
	var gotCT string
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/token" {
			_, _ = w.Write([]byte(`{"token":"jwt-test","expiresAt":"2099-01-01T00:00:00Z"}`))
			return
		}
		if r.URL.Path == "/v1/files" {
			gotCT = r.Header.Get("Content-Type")
			if _, _, err := r.FormFile("file"); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"no file"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"file":{"fileRef":{"fileId":"nd_x","versionId":"ver_x"}}}`))
			return
		}
		if r.URL.Path == "/v1/memories/upload" {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task":{"id":"t1","status":"queued"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer probe.Close()

	s2 := newMemoryServer(t, probe.URL)
	rec := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/memories/import",
		`{"filename":"x.md","content":"hi"}`))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type = %q，必须是 multipart", gotCT)
	}
	_ = multipart.ErrMessageTooLarge
}
