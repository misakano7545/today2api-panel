package main

import (
	"encoding/xml"
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

func TestWriteMemoryFileMatching(t *testing.T) {
	h := &handler{}
	pins := &memoryPins{Items: []memoryPin{
		{Path: "/memory/about-user", Files: []memoryFile{
			{Path: "/memory/about-user/profile.md", Content: "hi", ContentType: "text/markdown"},
		}},
	}}

	req := httptest.NewRequest("GET", "/memory/about-user/profile.md", nil)
	rec := httptest.NewRecorder()
	h.writeMemoryFile(rec, req, pins)
	if rec.Code != 200 || rec.Body.String() != "hi" {
		t.Fatalf("取文件失败: %d %q", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.writeMemoryFile(rec2, httptest.NewRequest("GET", "/memory/nope.md", nil), pins)
	if rec2.Code != 404 {
		t.Fatalf("缺失文件想要 404，得到 %d", rec2.Code)
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
