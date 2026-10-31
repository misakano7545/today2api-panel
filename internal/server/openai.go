package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/misakano7545/today2api-panel/internal/config"
)

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// handleChatCompletions 是 OpenAI 兼容入口。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	cfg := s.store.Snapshot()
	model := resolveModel(req.Model, cfg.DefaultModel)
	turns := make([]Turn, 0, len(req.Messages))
	for _, m := range req.Messages {
		turns = append(turns, Turn{Role: m.Role, Text: contentText(m.Content)})
	}
	prompt := buildPrompt(turns, cfg.ContextTurns)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "messages is empty")
		return
	}

	ch := s.Start(r.Context(), model, prompt)
	started := time.Now()

	if !req.Stream {
		text, account, chars, err := collect(ch)
		s.logs.add(Entry{Time: started, Path: "/v1/chat/completions", Model: model, Account: account, Status: statusOf(err), Duration: time.Since(started).Milliseconds(), Chars: chars, Error: errText(err)})
		if err != nil && text == "" {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-" + newID(),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
			"usage": usageOf(prompt, text),
		})
		return
	}

	// 流式：等到第一个增量再写响应头，这样上游的首字节前错误还能返回正常 HTTP 错误码。
	first, ok := <-ch
	if !ok || (first.Err != nil && first.Delta == "") {
		err := first.Err
		if err == nil {
			err = errNoAccountValue
		}
		s.logs.add(Entry{Time: started, Path: "/v1/chat/completions", Model: model, Status: 502, Duration: time.Since(started).Milliseconds(), Error: err.Error()})
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	sseHeaders(w)
	flusher, _ := w.(http.Flusher)
	account := first.Account
	chars := 0
	emit := func(delta string) bool {
		chars += len([]rune(delta))
		if err := writeSSE(w, "", map[string]any{
			"id":      "chatcmpl-" + newID(),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}},
		}); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	// 首个 chunk 带 role，符合 OpenAI 客户端预期。
	_ = writeSSE(w, "", map[string]any{
		"id": "chatcmpl-" + newID(), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
	})
	if first.Delta != "" && !emit(first.Delta) {
		return
	}
	var streamErr string
	for ev := range ch {
		if ev.Account != "" {
			account = ev.Account
		}
		if ev.Delta != "" {
			if !emit(ev.Delta) {
				streamErr = "client disconnected"
				break
			}
		}
		if ev.Err != nil {
			streamErr = ev.Err.Error()
			_ = writeSSE(w, "", map[string]any{"error": map[string]any{"message": ev.Err.Error(), "type": "upstream_error"}})
		}
	}
	if streamErr == "" {
		_ = writeSSE(w, "", map[string]any{
			"id": "chatcmpl-" + newID(), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		})
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	s.logs.add(Entry{Time: started, Path: "/v1/chat/completions", Model: model, Account: account, Status: statusOf(nil), Duration: time.Since(started).Milliseconds(), Chars: chars, Error: streamErr})
}

func statusOf(err error) int {
	if err == nil {
		return 200
	}
	return 502
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func usageOf(prompt, text string) map[string]any {
	in := estimateTokens(prompt)
	out := estimateTokens(text)
	return map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
}

func newID() string {
	return config.RandomHex(12)
}
