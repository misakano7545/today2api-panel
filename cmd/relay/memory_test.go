package main

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// memoryClientDoc 是客户端 DOMParser 实际读的那几层，用同形状解析一遍，
// 保证改实现时不会把 href/displayname/collection 弄丢。
type memoryClientDoc struct {
	XMLName   xml.Name `xml:"multistatus"`
	Responses []struct {
		Href string `xml:"href"`
		Prop struct {
			DisplayName string    `xml:"displayname"`
			ContentType string    `xml:"getcontenttype"`
			Modified    string    `xml:"getlastmodified"`
			Collection  *struct{} `xml:"resourcetype>collection"`
		} `xml:"propstat>prop"`
	} `xml:"response"`
}

func TestWriteMultistatus(t *testing.T) {
	pins := &memoryPins{Items: []memoryPin{
		{Path: "/memory/about-user", Name: "about-user", Files: []memoryFile{
			{Path: "/memory/about-user/profile.md", Name: "profile.md",
				ContentType: "text/markdown", UpdatedAt: "2026-11-01T00:32:01Z"},
		}},
		{Path: "/memory/agent-character", Name: "agent-character", Files: []memoryFile{
			{Path: "/memory/agent-character/character.md", Name: "character.md",
				ContentType: "text/markdown", UpdatedAt: "2026-11-01T00:32:02Z"},
		}},
	}}

	req := httptest.NewRequest("PROPFIND", "/memory", nil)
	rec := httptest.NewRecorder()
	(&handler{}).writeMultistatus(rec, req, pins)

	if rec.Code != 207 {
		t.Fatalf("状态码 = %d，想要 207", rec.Code)
	}

	var doc memoryClientDoc
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("XML 解析失败: %v\n%s", err, rec.Body.String())
	}

	// 根 + 2 个目录 + 2 个文件
	if len(doc.Responses) != 5 {
		t.Fatalf("response 条数 = %d，想要 5", len(doc.Responses))
	}

	want := map[string]bool{
		"/memory":                              true,
		"/memory/about-user":                   true,
		"/memory/about-user/profile.md":        true,
		"/memory/agent-character":              true,
		"/memory/agent-character/character.md": true,
	}
	for _, r := range doc.Responses {
		if !want[r.Href] {
			t.Errorf("意外的 href %q", r.Href)
		}
		delete(want, r.Href)
	}
	if len(want) != 0 {
		t.Errorf("缺少 href: %v", want)
	}

	if !strings.Contains(rec.Body.String(), "<D:collection/>") {
		t.Error("目录缺少 collection 标记，客户端会当成文件")
	}
	if doc.Responses[2].Prop.ContentType != "text/markdown" {
		t.Errorf("文件缺少 contentType: %+v", doc.Responses[2].Prop)
	}
}

// GET/PUT/DELETE 必须原样转发到上游：内容不经任何改写。
// 旧实现把 PUT 翻成「传文件 → 导入」，正文会被 agent 重写；
// 现在上游原生支持 PUT，必须直通。
func TestMemoryForwardsVerbatim(t *testing.T) {
	var gotMethod, gotPath, gotBody, gotCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		if r.URL.Path == "/api/cloud/memory/gone.md" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("上游回包"))
	}))
	defer upstream.Close()

	rt := route{Upstream: upstream.URL, Prefix: "/api/cloud", App: "https://todayai.cn"}
	h := &handler{client: upstream.Client()}

	req := httptest.NewRequest("PUT", "/memory/notes/a.md", strings.NewReader("# 正文\n"))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "text/markdown")
	rec := httptest.NewRecorder()
	h.serveMemoryDAV(rec, req, rt)

	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT 转发失败: %d %s", rec.Code, rec.Body.String())
	}
	if gotMethod != "PUT" || gotPath != "/api/cloud/memory/notes/a.md" {
		t.Fatalf("上游收到 %s %s", gotMethod, gotPath)
	}
	if gotBody != "# 正文\n" {
		t.Fatalf("正文被改写: %q", gotBody)
	}
	if gotCT != "text/markdown" {
		t.Fatalf("Content-Type 丢了: %q", gotCT)
	}
	if rec.Body.String() != "上游回包" {
		t.Fatalf("响应未原样回传: %q", rec.Body.String())
	}

	// DELETE 也要直通，不能回 501。
	reqDel := httptest.NewRequest("DELETE", "/memory/gone.md", nil)
	reqDel.Header.Set("Authorization", "Bearer tok")
	recDel := httptest.NewRecorder()
	h.serveMemoryDAV(recDel, reqDel, rt)
	if recDel.Code != http.StatusNotFound {
		t.Fatalf("DELETE 上游 404 应原样透传，得到 %d", recDel.Code)
	}
}

// 真正做不到的只有 MKCOL/PROPPATCH，必须明确 501，不能假装成功。
func TestMemoryUnsupportedMethods(t *testing.T) {
	h := &handler{client: http.DefaultClient}
	rt := route{Prefix: "/api/cloud", App: "https://todayai.cn"}
	for _, m := range []string{"MKCOL", "PROPPATCH", "MOVE", "COPY"} {
		req := httptest.NewRequest(m, "/memory/x", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.serveMemoryDAV(rec, req, rt)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s 期望 501，得到 %d", m, rec.Code)
		}
	}
}

// 预检不带 Authorization，必须在鉴权之前放行，且要带 CORS 头。
func TestMemoryPreflightWithoutAuth(t *testing.T) {
	h := &handler{client: http.DefaultClient}
	req := httptest.NewRequest("OPTIONS", "/memory", nil)
	req.Header.Set("Origin", "http://today-desktop.localhost")
	req.Header.Set("Access-Control-Request-Method", "PROPFIND")
	rec := httptest.NewRecorder()
	h.serveMemoryDAV(rec, req, route{Prefix: "/api/cloud"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("预检期望 204，得到 %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://today-desktop.localhost" {
		t.Fatalf("CORS 未回显 Origin: %q", got)
	}
}

func TestPathClean(t *testing.T) {
	cases := map[string]string{
		"":               "/",
		"/memory/":       "/memory",
		"memory/a.md":    "/memory/a.md",
		"/memory//b.md":  "/memory/b.md",
		"/memory/c/d.md": "/memory/c/d.md",
	}
	for in, want := range cases {
		if got := pathClean(in); got != want {
			t.Errorf("pathClean(%q) = %q，想要 %q", in, got, want)
		}
	}
}
