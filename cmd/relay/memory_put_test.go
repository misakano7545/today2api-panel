package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// memoryPutMock 模拟上游三件套：传文件 → 定点导入 → 任务轮询。
type memoryPutMock struct {
	srv       *httptest.Server
	gotPath   string
	gotTarget string
	gotAuth   string
	uploads   int
	polls     int
}

func newMemoryPutMock(t *testing.T) *memoryPutMock {
	t.Helper()
	m := &memoryPutMock{}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/files", func(w http.ResponseWriter, r *http.Request) {
		m.uploads++
		m.gotAuth = r.Header.Get("Authorization")
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("上传必须是 multipart，得到 %q", ct)
		}
		if _, _, err := r.FormFile("file"); err != nil {
			t.Errorf("缺少 file 字段: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"file":{"id":"nd_1","fileRef":{"fileId":"nd_1","versionId":"ver_1"}}}`))
	})

	mux.HandleFunc("/v1/memories/upload", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path    string `json:"path"`
			FileRef struct {
				FileID string `json:"fileId"`
			} `json:"fileRef"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.gotTarget = body.Path
		if body.FileRef.FileID == "" {
			t.Errorf("import 缺少 fileRef")
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task":{"id":"task_1","status":"queued"}}`))
	})

	mux.HandleFunc("/v1/tasks/task_1", func(w http.ResponseWriter, r *http.Request) {
		// 第一次回 running，之后 completed —— 覆盖轮询循环。
		m.polls++
		status := "running"
		if m.polls > 1 {
			status = "completed"
		}
		_, _ = w.Write([]byte(`{"task":{"id":"task_1","status":"` + status + `"}}`))
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func TestMemoryPutWritesToExactPath(t *testing.T) {
	mock := newMemoryPutMock(t)
	h := &handler{client: mock.srv.Client()}
	rt := route{Upstream: mock.srv.URL, Prefix: "", App: "https://todayai.cn"}

	req := httptest.NewRequest(http.MethodPut, "/memory/notes/x.md", strings.NewReader("# 标题\n\n正文\n"))
	req.Header.Set("Authorization", "Bearer jwt-test")
	req.Header.Set("Content-Type", "text/markdown")
	rec := httptest.NewRecorder()

	h.handleMemoryPut(rec, req, rt, "Bearer jwt-test")

	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，想要 201（body=%q）", rec.Code, rec.Body.String())
	}
	if mock.gotTarget != "/memory/notes/x.md" {
		t.Errorf("导入路径 = %q，想要 /memory/notes/x.md", mock.gotTarget)
	}
	if mock.gotAuth != "Bearer jwt-test" {
		t.Errorf("上传丢了 Authorization，得到 %q", mock.gotAuth)
	}
	if mock.uploads != 1 {
		t.Errorf("上传次数 = %d，想要 1", mock.uploads)
	}
	if mock.polls < 2 {
		t.Errorf("轮询次数 = %d，应当一直转到终态", mock.polls)
	}
}

func TestMemoryPutRejectsRelativePath(t *testing.T) {
	h := &handler{client: http.DefaultClient}
	rt := route{Upstream: "http://127.0.0.1:1", Prefix: "", App: "https://todayai.cn"}

	req := httptest.NewRequest(http.MethodPut, "/memory/", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	h.handleMemoryPut(rec, req, rt, "Bearer t")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空路径应 400，得到 %d", rec.Code)
	}
}

func TestMemoryPutRejectsEmptyBody(t *testing.T) {
	h := &handler{client: http.DefaultClient}
	rt := route{Upstream: "http://127.0.0.1:1", Prefix: "", App: "https://todayai.cn"}

	req := httptest.NewRequest(http.MethodPut, "/memory/notes/x.md", strings.NewReader(""))
	rec := httptest.NewRecorder()
	h.handleMemoryPut(rec, req, rt, "Bearer t")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 body 应 400，得到 %d", rec.Code)
	}
}

func TestWaitMemoryTaskStopsOnTerminal(t *testing.T) {
	mock := newMemoryPutMock(t)
	h := &handler{client: mock.srv.Client(), verbose: false}
	rt := route{Upstream: mock.srv.URL, Prefix: "", App: "https://todayai.cn"}

	done := make(chan string, 1)
	go func() { done <- h.waitMemoryTask(t.Context(), rt, "Bearer t", "task_1") }()

	select {
	case st := <-done:
		if st != "completed" {
			t.Fatalf("终态 = %q，想要 completed", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("轮询没有在终态停下")
	}
}
