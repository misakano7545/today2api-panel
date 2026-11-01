package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/misakano7545/today2api-panel/internal/config"
)

// mockToday 是上游的替身：实现 token 换票、发消息、轮询回复三段。
type mockToday struct {
	mu              sync.Mutex
	polls           int
	texts           []string // assistant 正文的逐次快照，最后一次视为终稿
	prompt          string
	sent            []map[string]any
	clientMessageID string
	rapportReads    int
	reported        []string
}

func newMockToday(t *testing.T) (*httptest.Server, *mockToday) {
	t.Helper()
	m := &mockToday{texts: []string{"", "你好", "你好世界"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/token":
			if strings.Contains(r.Header.Get("Cookie"), "bad") {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid session"}`))
				return
			}
			if !strings.Contains(r.Header.Get("Origin"), "://") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"token":"jwt-test","expiresAt":"2099-01-01T00:00:00Z"}`))
		case r.URL.Path == "/v1/config":
			_, _ = w.Write([]byte(`{"defaultAvatars":[],"messageCapabilities":{"messageProtocolVersion":"canonical-v1","authSubject":"u1","mainConversationId":"main","supportsTypedBody":true,"supportsLongPoll":true,"longPollWaitMaxMs":25000}}`))
		case r.URL.Path == "/v1/users/me":
			_, _ = w.Write([]byte(`{"id":"u1","email":"u1@example.com","plan":"pro"}`))
		case r.URL.Path == "/v1/pricing/status":
			// 实测：plan 是字符串，选项是模式
			_, _ = w.Write([]byte(`{"success":true,"code":"common.ok","data":{"status":{"plan":"pro","modelOptions":{"selectedOptionId":"mode.balanced","options":[{"id":"mode.balanced","disabled":false},{"id":"mode.power","disabled":true}]}}}}`))
		case r.URL.Path == "/v1/llm/models":
			// 实测：{"models":[{"modelId":…}]}
			_, _ = w.Write([]byte(`{"models":[{"modelId":"claude-opus-5"},{"modelId":"gpt-5.5"}]}`))
		case r.URL.Path == "/v1/rapport" && r.Method == http.MethodGet:
			// 第一次读（签到前）与第二次读（签到后）分数不同，用来验证 delta。
			m.mu.Lock()
			m.rapportReads++
			points := 40
			if m.rapportReads > 1 {
				points = 90
			}
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": "common.ok", "data": map[string]any{
				"rapport": map[string]any{
					"currentPoints": points, "nextLevelPoints": 100, "progress": 0.2, "isMaxLevel": false,
					"currentLevel": map[string]any{"key": "origin", "points": 0, "name": "Origin"},
				},
				"levels": []any{map[string]any{"key": "origin", "name": "Origin", "points": 0}},
				"rules": []any{
					map[string]any{"key": "notification_enabled", "title": "Notification Enabled", "points": 25, "status": "completed", "scoreTrigger": "client_report"},
					map[string]any{"key": "discord_join_reported", "title": "Discord Joined", "points": 25, "status": "completed", "scoreTrigger": "client_report"},
					map[string]any{"key": "memory_update", "title": "Memory Update", "points": 2, "status": "available", "scoreTrigger": "server_event"},
				},
			}})
		case strings.HasPrefix(r.URL.Path, "/v1/rapport/actions/") && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer jwt-test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			m.mu.Lock()
			m.reported = append(m.reported, strings.TrimPrefix(r.URL.Path, "/v1/rapport/actions/"))
			m.mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
		case r.URL.Path == "/v1/tasks":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"task-1","type":"research","title":"整理周报","status":"running","createdAt":"2026-11-01T00:00:00Z"}],"nextCursor":null,"hasMore":false}`))
		case r.URL.Path == "/v1/tasks/summary":
			_, _ = w.Write([]byte(`{"data":{"running":1,"completed":3}}`))
		case r.URL.Path == "/v1/tasks/templates/types":
			_, _ = w.Write([]byte(`{"success":true,"data":{"items":[{"type":"general","typeName":"General"},{"type":"work","typeName":"Work"}]}}`))
		case r.URL.Path == "/v1/tasks/recommendations":
			_, _ = w.Write([]byte(`{"object":"list","batchId":null,"data":[{"id":"rec-1","title":"试试周报助手"}],"total":1}`))
		case r.URL.Path == "/v1/tasks/task-1/stop" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
		case r.URL.Path == "/v1/pricing/model-option" && r.Method == http.MethodPut:
			if r.Header.Get("Authorization") != "Bearer jwt-test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"modelOptions":{"selectedOptionId":"model.deepseek-v4.1-flash"}}}`))
		case r.URL.Path == "/v1/messages":
			if r.Header.Get("Authorization") != "Bearer jwt-test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			clientMessageID, _ := body["clientMessageId"].(string)
			prompt := mockPrompt(body)
			m.mu.Lock()
			m.sent = append(m.sent, body)
			m.prompt = prompt
			m.clientMessageID = clientMessageID
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"canonicalMessage": map[string]any{
					"id": "msg-user", "conversationId": "main", "role": "user",
					"clientMessageId": clientMessageID, "state": "completed", "isFinal": true,
					"body": map[string]any{"parts": []any{map[string]any{
						"messageType": "text", "content": map[string]any{"text": prompt},
					}}},
				},
			})
		case r.URL.Path == "/v1/conversations/messages":
			m.mu.Lock()
			idx := m.polls
			if idx >= len(m.texts) {
				idx = len(m.texts) - 1
			}
			m.polls++
			text := m.texts[idx]
			final := idx == len(m.texts)-1
			clientMessageID := m.clientMessageID
			m.mu.Unlock()
			state := "in_progress"
			if final {
				state = "completed"
			}
			payload := map[string]any{
				"data": map[string]any{"items": []any{map[string]any{
					"id": "msg-assistant", "conversationId": "main", "role": "assistant",
					"state": state, "isFinal": final,
					"originClientMessageId": clientMessageID,
					"body": map[string]any{"parts": []any{map[string]any{
						"messageType": "text", "content": map[string]any{"text": text},
					}}},
				}}},
			}
			_ = json.NewEncoder(w).Encode(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, m
}

// mockPrompt 从 canonical-v1 请求体里抽出正文，模拟上游“用户消息入库”的检查点。
func mockPrompt(body map[string]any) string {
	b, _ := body["body"].(map[string]any)
	parts, _ := b["parts"].([]any)
	var out strings.Builder
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if s, ok := part["text"].(string); ok {
			out.WriteString(s)
		}
	}
	return out.String()
}

// newTestServer 组装一个指向 mock 上游的网关。
func newTestServer(t *testing.T, upstream string, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Upstream = config.Endpoints{App: upstream, API: upstream, Auth: upstream, Audience: upstream}
	cfg.PollIntervalMs = 40
	cfg.ReplyTimeoutSec = 10
	cfg.Accounts = []config.Account{{ID: "acc_1", Label: "a1", Cookie: "__Secure-better-auth.session_token=ok", Enabled: true}}
	if mutate != nil {
		mutate(&cfg)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return New(store, "test")
}

// req 构造一个来自本机的请求（httptest 默认 RemoteAddr 非回环，面板会拒绝）。
func req(method, path string, body any) *http.Request {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case io.Reader:
		rd = b
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "127.0.0.1:54321"
	return r
}

func makeKey(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/keys", strings.NewReader(`{"name":"t"}`)))
	if rec.Code != 200 {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Key
}

func TestChatCompletionsNonStream(t *testing.T) {
	up, mock := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	key := makeKey(t, s)

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Choices[0].Message.Content; got != "你好世界" {
		t.Fatalf("content = %q", got)
	}
	if !strings.Contains(mock.prompt, "你是助手") || !strings.Contains(mock.prompt, "你好") {
		t.Fatalf("prompt = %q", mock.prompt)
	}
	if isMain, _ := mock.sent[0]["isMainChat"].(bool); !isMain {
		t.Fatalf("isMainChat = %v（缺了它上游会当成子会话，取不到主对话回复）", mock.sent[0]["isMainChat"])
	}
}

func TestChatCompletionsStream(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	key := makeKey(t, s)

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	text, done := sseText(t, rec.Body)
	if text != "你好世界" {
		t.Fatalf("stream text = %q", text)
	}
	if !done {
		t.Fatal("missing [DONE]")
	}
}

// sseText 拼接 OpenAI chunk 里的 delta.content。
func sseText(t *testing.T, r io.Reader) (string, bool) {
	t.Helper()
	var buf strings.Builder
	done := false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			buf.WriteString(c.Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), done
}

func TestAnthropicStream(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	key := makeKey(t, s)

	body := `{"model":"deepseek-v4.1-flash","system":"你是助手","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"你好"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var text strings.Builder
	var sawStart, sawStop bool
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			sawStart = true
		case "content_block_delta":
			text.WriteString(ev.Delta.Text)
		case "message_stop":
			sawStop = true
		}
	}
	if !sawStart || !sawStop {
		t.Fatalf("stream events incomplete: start=%v stop=%v", sawStart, sawStop)
	}
	if text.String() != "你好世界" {
		t.Fatalf("text = %q", text.String())
	}
}

func TestResponsesStream(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	key := makeKey(t, s)

	body := `{"model":"deepseek-v4.1-flash","instructions":"简洁回答","input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var text strings.Builder
	var completed bool
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "response.output_text.delta" {
			text.WriteString(ev.Delta)
		}
		if ev.Type == "response.completed" {
			completed = true
		}
	}
	if !completed {
		t.Fatal("missing response.completed")
	}
	if text.String() != "你好世界" {
		t.Fatalf("text = %q", text.String())
	}
}

func TestResponsesNonStream(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	key := makeKey(t, s)
	req := req(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4.1-flash","input":"你好"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"output_text"`) || !strings.Contains(rec.Body.String(), "你好世界") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestKeyRequired(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	makeKey(t, s)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodGet, "/v1/models", nil))
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestFailoverToSecondAccount(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, func(c *config.Config) {
		c.Accounts = []config.Account{
			{ID: "acc_bad", Label: "bad", Cookie: "__Secure-better-auth.session_token=bad", Enabled: true},
			{ID: "acc_ok", Label: "ok", Cookie: "__Secure-better-auth.session_token=fine", Enabled: true},
		}
	})
	key := makeKey(t, s)
	for i := 0; i < 3; i++ { // 轮转起点不同，两次都要能落到可用账号
		req := req(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好世界") {
			t.Fatalf("attempt %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
}

func TestPanelHidesCookie(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodGet, "/api/panel/state", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "better-auth.session_token") {
		t.Fatal("panel state leaked cookie")
	}
}

// 面板视图绝不下发凭证（Cookie 与静态 JWT 都是明文 secret）。
func TestPanelHidesCredentials(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, func(c *config.Config) {
		c.Accounts = []config.Account{{
			ID: "acc_1", Label: "a1", Enabled: true,
			Cookie: "__Secure-better-auth.session_token=ok",
			Token:  "eyJhbGciOiJIUzI1NiJ9.secretpayload.sig",
		}}
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodGet, "/api/panel/state", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"better-auth.session_token", "secretpayload"} {
		if strings.Contains(body, secret) {
			t.Fatalf("面板泄露凭证: %s", secret)
		}
	}
}
