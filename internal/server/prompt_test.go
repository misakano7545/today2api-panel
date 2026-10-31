package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	turns := []Turn{
		{Role: "system", Text: "你是助手"},
		{Role: "user", Text: "第一问"},
		{Role: "assistant", Text: "第一答"},
		{Role: "user", Text: "第二问"},
	}
	got := buildPrompt(turns, 0)
	if strings.Contains(got, "第一问") && strings.Contains(got, "第一答") {
		t.Fatalf("contextTurns=0 不应重放历史：%q", got)
	}
	if !strings.Contains(got, "你是助手") || !strings.Contains(got, "第二问") {
		t.Fatalf("prompt = %q", got)
	}
	got = buildPrompt(turns, 2)
	if !strings.Contains(got, "第一答") || !strings.Contains(got, "[历史对话]") {
		t.Fatalf("contextTurns=2 应带历史：%q", got)
	}
}

func TestContentTextShapes(t *testing.T) {
	cases := map[string]string{
		`"纯文本"`: `纯文本`,
		`[{"type":"text","text":"a"},{"type":"image","text":""},{"type":"text","text":"b"}]`: "ab",
		`[{"type":"input_text","text":"x"}]`:                                                 "x",
	}
	for raw, want := range cases {
		if got := contentText(json.RawMessage(raw)); got != want {
			t.Fatalf("%s → %q, want %q", raw, got, want)
		}
	}
}

func TestResolveModel(t *testing.T) {
	if got := resolveModel("today-glm-5.3", "deepseek-v4.1-flash"); got != "glm-5.3" {
		t.Fatalf("got %q", got)
	}
	if got := resolveModel("", "deepseek-v4.1-flash"); got != "deepseek-v4.1-flash" {
		t.Fatalf("got %q", got)
	}
}
