package server

import (
	"encoding/json"
	"net/http"
	"time"
)

type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Stream       bool            `json:"stream"`
}

// handleResponses 是 OpenAI Responses 兼容入口（Codex CLI 等）。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	cfg := s.store.Snapshot()
	model := resolveModel(req.Model, cfg.DefaultModel)
	turns := []Turn{}
	if req.Instructions != "" {
		turns = append(turns, Turn{Role: "system", Text: req.Instructions})
	}
	turns = append(turns, responsesTurns(req.Input)...)
	prompt := buildPrompt(turns, cfg.ContextTurns)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "input is empty")
		return
	}

	respID := "resp_" + newID()
	itemID := "msg_" + newID()
	ch := s.Start(r.Context(), model, prompt)
	started := time.Now()

	full := func(text, status string) map[string]any {
		return map[string]any{
			"id": respID, "object": "response", "created_at": started.Unix(),
			"status": status, "model": model,
			"output": []any{map[string]any{
				"type": "message", "id": itemID, "status": status, "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			}},
			"usage": map[string]any{
				"input_tokens": estimateTokens(prompt), "output_tokens": estimateTokens(text),
				"total_tokens": estimateTokens(prompt) + estimateTokens(text),
			},
		}
	}

	if !req.Stream {
		text, account, chars, err := collect(ch)
		s.logs.add(Entry{Time: started, Path: "/v1/responses", Model: model, Account: account, Status: statusOf(err), Duration: time.Since(started).Milliseconds(), Chars: chars, Error: errText(err)})
		if err != nil && text == "" {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, full(text, "completed"))
		return
	}

	first, ok := <-ch
	if !ok || (first.Err != nil && first.Delta == "") {
		err := first.Err
		if err == nil {
			err = errNoAccountValue
		}
		s.logs.add(Entry{Time: started, Path: "/v1/responses", Model: model, Status: 502, Duration: time.Since(started).Milliseconds(), Error: err.Error()})
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
	seq := 0
	send := func(event string, payload map[string]any) bool {
		payload["type"] = event
		payload["sequence_number"] = seq
		seq++
		if err := writeSSE(w, event, payload); err != nil {
			return false
		}
		flush()
		return true
	}

	send("response.created", map[string]any{"response": full("", "in_progress")})
	send("response.output_item.added", map[string]any{
		"output_index": 0,
		"item":         map[string]any{"type": "message", "id": itemID, "status": "in_progress", "role": "assistant", "content": []any{}},
	})
	send("response.content_part.added", map[string]any{
		"item_id": itemID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})

	text := ""
	account := first.Account
	emit := func(delta string) bool {
		text += delta
		return send("response.output_text.delta", map[string]any{
			"item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta,
		})
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
			send("response.failed", map[string]any{
				"response": map[string]any{
					"id": respID, "object": "response", "status": "failed", "model": model,
					"error": map[string]any{"code": "upstream_error", "message": ev.Err.Error()},
				},
			})
		}
	}
	if streamErr == "" {
		send("response.output_text.done", map[string]any{
			"item_id": itemID, "output_index": 0, "content_index": 0, "text": text,
		})
		send("response.content_part.done", map[string]any{
			"item_id": itemID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		send("response.output_item.done", map[string]any{
			"output_index": 0,
			"item":         map[string]any{"type": "message", "id": itemID, "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}},
		})
		send("response.completed", map[string]any{"response": full(text, "completed")})
	}
	s.logs.add(Entry{Time: started, Path: "/v1/responses", Model: model, Account: account, Status: 200, Duration: time.Since(started).Milliseconds(), Chars: len([]rune(text)), Error: streamErr})
}

// responsesTurns 解析 Responses 的 input：字符串，或 [{role,content}] 消息数组。
func responsesTurns(raw json.RawMessage) []Turn {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []Turn{{Role: "user", Text: text}}
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]Turn, 0, len(items))
	for _, it := range items {
		role := it.Role
		if role == "" {
			role = "user"
		}
		if text := contentText(it.Content); text != "" {
			out = append(out, Turn{Role: role, Text: text})
		}
	}
	return out
}
