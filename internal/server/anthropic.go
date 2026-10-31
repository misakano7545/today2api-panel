package server

import (
	"encoding/json"
	"net/http"
	"time"
)

type anthropicRequest struct {
	Model     string          `json:"model"`
	System    json.RawMessage `json:"system"`
	Messages  []chatMessage   `json:"messages"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens"`
}

// handleMessages 是 Anthropic Messages 兼容入口（Claude Code 等）。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	cfg := s.store.Snapshot()
	model := resolveModel(req.Model, cfg.DefaultModel)
	turns := make([]Turn, 0, len(req.Messages)+1)
	if sys := contentText(req.System); sys != "" {
		turns = append(turns, Turn{Role: "system", Text: sys})
	}
	for _, m := range req.Messages {
		turns = append(turns, Turn{Role: m.Role, Text: contentText(m.Content)})
	}
	prompt := buildPrompt(turns, cfg.ContextTurns)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "messages is empty")
		return
	}

	msgID := "msg_" + newID()
	ch := s.Start(r.Context(), model, prompt)
	started := time.Now()

	if !req.Stream {
		text, account, chars, err := collect(ch)
		s.logs.add(Entry{Time: started, Path: "/v1/messages", Model: model, Account: account, Status: statusOf(err), Duration: time.Since(started).Milliseconds(), Chars: chars, Error: errText(err)})
		if err != nil && text == "" {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{map[string]any{"type": "text", "text": text}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         anthropicUsage(prompt, text),
		})
		return
	}

	first, ok := <-ch
	if !ok || (first.Err != nil && first.Delta == "") {
		err := first.Err
		if err == nil {
			err = errNoAccountValue
		}
		s.logs.add(Entry{Time: started, Path: "/v1/messages", Model: model, Status: 502, Duration: time.Since(started).Milliseconds(), Error: err.Error()})
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	sseHeaders(w)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	_ = writeSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": estimateTokens(prompt), "output_tokens": 0},
		},
	})
	_ = writeSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	flush()

	account := first.Account
	chars := 0
	emit := func(delta string) bool {
		chars += len([]rune(delta))
		if err := writeSSE(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		}); err != nil {
			return false
		}
		flush()
		return true
	}
	if first.Delta != "" && !emit(first.Delta) {
		return
	}
	var streamErr string
	for ev := range ch {
		if ev.Account != "" {
			account = ev.Account
		}
		if ev.Delta != "" && !emit(ev.Delta) {
			streamErr = "client disconnected"
			break
		}
		if ev.Err != nil {
			streamErr = ev.Err.Error()
			_ = writeSSE(w, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": ev.Err.Error()},
			})
		}
	}
	_ = writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	_ = writeSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": estimateTokens("") + chars/2},
	})
	_ = writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
	flush()
	s.logs.add(Entry{Time: started, Path: "/v1/messages", Model: model, Account: account, Status: 200, Duration: time.Since(started).Milliseconds(), Chars: chars, Error: streamErr})
}

func anthropicUsage(prompt, text string) map[string]any {
	return map[string]any{"input_tokens": estimateTokens(prompt), "output_tokens": estimateTokens(text)}
}
