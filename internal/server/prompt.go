package server

import (
	"encoding/json"
	"strings"
)

// Turn 是一轮对话。
type Turn struct {
	Role string // user | assistant | system
	Text string
}

// buildPrompt 把 OpenAI 风格的消息压成 Today 能收的一段文本。
//
// Today 只有一个输入框，没有角色概念：系统指令和历史都要折叠进正文。
// contextTurns=0 只带当前这条（上游按会话记忆，网关不重放历史）。
func buildPrompt(turns []Turn, contextTurns int) string {
	var system, history, current string
	var convo []Turn
	for _, t := range turns {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		switch strings.ToLower(t.Role) {
		case "system", "developer":
			system += text + "\n\n"
		case "assistant":
			convo = append(convo, Turn{Role: "助手", Text: text})
		default:
			convo = append(convo, Turn{Role: "用户", Text: text})
		}
	}
	if len(convo) == 0 {
		return strings.TrimSpace(system)
	}
	keep := contextTurns + 1
	if contextTurns <= 0 || len(convo) < keep {
		keep = 1
	}
	window := convo[len(convo)-keep:]
	current = window[len(window)-1].Text
	for _, t := range window[:len(window)-1] {
		history += t.Role + "：" + t.Text + "\n"
	}
	var buf strings.Builder
	if s := strings.TrimSpace(system); s != "" {
		buf.WriteString("[系统指令]\n")
		buf.WriteString(s)
		buf.WriteString("\n\n")
	}
	if history != "" {
		buf.WriteString("[历史对话]\n")
		buf.WriteString(history)
		buf.WriteString("\n")
	}
	if history != "" {
		buf.WriteString("[当前消息]\n用户：")
	}
	buf.WriteString(current)
	return buf.String()
}

// contentText 把 content 字段转成纯文本：string、或 []{type:"text",text} 都可以。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var buf strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text", "":
			buf.WriteString(p.Text)
		}
	}
	return buf.String()
}

// resolveModel 处理默认模型与 today- 前缀。
func resolveModel(requested, fallback string) string {
	m := strings.TrimSpace(requested)
	m = strings.TrimPrefix(m, "today-")
	if m == "" {
		return fallback
	}
	return m
}
