// Package today 实现 Today（todayai.cn / today.ai）的上游协议。
//
// 协议链：
//
//	登录 Cookie（__Secure-better-auth.session_token=…）
//	  └─ POST {app}/api/token {audience, expiresIn}
//	       → {token, expiresIn}            ← API JWT（分钟级过期）
//	          └─ Authorization: Bearer …   → {api}/v1/*
//
// 对话不是 OpenAI 协议：POST /v1/messages 只回 202（用户消息已入库），
// 回答要靠轮询 GET /v1/conversations/messages 取回并拼增量。
// 上游没有 SSE，流式由网关把轮询增量伪流式下发。
package today

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Bases 是一个区域的上游三件套。
type Bases struct {
	App  string
	API  string
	Auth string
}

// OfficialBases 返回官方地址。
func OfficialBases(realm string) Bases {
	if realm == "cn" {
		return Bases{App: "https://todayai.cn", API: "https://api.todayai.cn", Auth: "https://auth.todayai.cn"}
	}
	return Bases{App: "https://today.ai", API: "https://api.today.ai", Auth: "https://auth.today.ai"}
}

// Kind 是错误归类，决定账号池怎么冷却。
type Kind string

const (
	KindAuth    Kind = "auth"    // Cookie 失效 / JWT 被拒
	KindRate    Kind = "rate"    // 429 / 额度用尽
	KindServer  Kind = "server"  // 5xx 或协议不符
	KindNetwork Kind = "network" // 连不上、超时
)

// Error 是带归类的上游错误。
type Error struct {
	Kind   Kind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("today %s: HTTP %d %s", e.Kind, e.Status, e.Msg)
	}
	return fmt.Sprintf("today %s: %s", e.Kind, e.Msg)
}

// KindOf 归类任意错误。
func KindOf(err error) Kind {
	var e *Error
	if err != nil && asError(err, &e) {
		return e.Kind
	}
	return KindNetwork
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Client 持有一个账号的登录态与 JWT 缓存。
type Client struct {
	Bases
	Cookie   string
	Interval time.Duration // 轮询间隔
	HC       *http.Client

	mu     sync.Mutex
	jwt    string
	jwtExp time.Time
	option string // 该账号当前生效的模型选项，避免每次请求都 PUT
}

// NewClient 构造客户端。bases 留空的字段回落到该区域的官方地址。
func NewClient(realm, cookie string, bases Bases) *Client {
	def := OfficialBases(realm)
	if bases.App == "" {
		bases.App = def.App
	}
	if bases.API == "" {
		bases.API = def.API
	}
	if bases.Auth == "" {
		bases.Auth = def.Auth
	}
	return &Client{
		Bases:    Bases{App: strings.TrimRight(bases.App, "/"), API: strings.TrimRight(bases.API, "/"), Auth: strings.TrimRight(bases.Auth, "/")},
		Cookie:   cookie,
		Interval: 250 * time.Millisecond,
		HC:       &http.Client{Timeout: 60 * time.Second},
	}
}

// Token 换出 API JWT（提前 60s 续期）。
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.jwt != "" && time.Now().Before(c.jwtExp.Add(-60*time.Second)) {
		token := c.jwt
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	if strings.TrimSpace(c.Cookie) == "" {
		return "", &Error{Kind: KindAuth, Msg: "account has no cookie"}
	}
	body, _ := json.Marshal(map[string]any{"audience": c.API, "expiresIn": 3600})
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, c.App+"/api/token", bytes.NewReader(body))
	if errReq != nil {
		return "", errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.App)
	req.Header.Set("Cookie", c.Cookie)
	resp, errDo := c.HC.Do(req)
	if errDo != nil {
		return "", &Error{Kind: KindNetwork, Msg: errDo.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		kind := KindServer
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			kind = KindAuth
		}
		return "", &Error{Kind: kind, Status: resp.StatusCode, Msg: snippet(raw)}
	}
	var parsed struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil || parsed.Token == "" {
		return "", &Error{Kind: KindServer, Msg: "token response missing token: " + snippet(raw)}
	}
	ttl := parsed.ExpiresIn
	if ttl <= 0 {
		ttl = 540
	}
	c.mu.Lock()
	c.jwt = parsed.Token
	c.jwtExp = time.Now().Add(time.Duration(ttl) * time.Second)
	c.mu.Unlock()
	return parsed.Token, nil
}

// do 发一个带 JWT 的 API 请求；401 时强制换票重试一次。
func (c *Client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	call := func() (int, []byte, error) {
		token, errToken := c.Token(ctx)
		if errToken != nil {
			return 0, nil, errToken
		}
		var reader io.Reader
		if body != nil {
			buf, err := json.Marshal(body)
			if err != nil {
				return 0, nil, err
			}
			reader = bytes.NewReader(buf)
		}
		req, errReq := http.NewRequestWithContext(ctx, method, c.API+path, reader)
		if errReq != nil {
			return 0, nil, errReq
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, errDo := c.HC.Do(req)
		if errDo != nil {
			return 0, nil, &Error{Kind: KindNetwork, Msg: errDo.Error()}
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return resp.StatusCode, raw, nil
	}
	status, raw, err := call()
	if err != nil || status != http.StatusUnauthorized {
		return status, raw, err
	}
	c.mu.Lock()
	c.jwt = ""
	c.mu.Unlock()
	return call()
}

// classify 把非 2xx 响应转成带归类的错误。
func classify(what string, status int, raw []byte) error {
	kind := KindServer
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = KindAuth
	case status == http.StatusTooManyRequests:
		kind = KindRate
	case status == http.StatusPaymentRequired:
		kind = KindRate
	}
	return &Error{Kind: kind, Status: status, Msg: what + ": " + snippet(raw)}
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Me 拉取当前账号信息。
func (c *Client) Me(ctx context.Context) (map[string]any, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/v1/users/me", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, classify("users/me", status, raw)
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		return nil, &Error{Kind: KindServer, Msg: "decode users/me: " + snippet(raw)}
	}
	return out, nil
}

// PricingStatus 返回套餐状态（含模型选项与用量）。
func (c *Client) PricingStatus(ctx context.Context) (map[string]any, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/v1/pricing/status", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, classify("pricing/status", status, raw)
	}
	var wrapper struct {
		Data struct {
			Status map[string]any `json:"status"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal != nil {
		return nil, &Error{Kind: KindServer, Msg: "decode pricing/status: " + snippet(raw)}
	}
	return wrapper.Data.Status, nil
}

// Models 从套餐状态里读出账号可用模型 id（形如 deepseek-v4.1-flash）。
func (c *Client) Models(ctx context.Context) ([]string, error) {
	status, err := c.PricingStatus(ctx)
	if err != nil {
		return nil, err
	}
	options, _ := status["modelOptions"].(map[string]any)
	if options == nil {
		return nil, nil
	}
	var ids []string
	collect := func(list []any) {
		for _, item := range list {
			switch v := item.(type) {
			case string:
				ids = append(ids, TrimOption(v))
			case map[string]any:
				for _, key := range []string{"optionId", "id", "value"} {
					if s, ok := v[key].(string); ok && s != "" {
						ids = append(ids, TrimOption(s))
						break
					}
				}
			}
		}
	}
	for _, key := range []string{"availableOptions", "options", "available"} {
		if list, ok := options[key].([]any); ok {
			collect(list)
		}
	}
	return compact(ids), nil
}

// EnsureModel 把账号的模型选项切到 optionID；已经是它就不重复 PUT。
func (c *Client) EnsureModel(ctx context.Context, optionID string) error {
	c.mu.Lock()
	current := c.option
	c.mu.Unlock()
	if current == optionID {
		return nil
	}
	status, raw, err := c.do(ctx, http.MethodPut, "/v1/pricing/model-option", map[string]any{"optionId": optionID})
	if err != nil {
		return err
	}
	// 404/405 说明该区域没有档位接口（CN 早期版本），不影响对话。
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return nil
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return classify("pricing/model-option", status, raw)
	}
	c.mu.Lock()
	c.option = optionID
	c.mu.Unlock()
	return nil
}

// TrimOption 去掉 "model." 前缀。
func TrimOption(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "model.") }

// OptionID 把模型 id 还原成上游的 optionId。
func OptionID(model string) string {
	m := strings.TrimSpace(model)
	if strings.HasPrefix(m, "model.") {
		return m
	}
	return "model." + m
}

func compact(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// Send 发一条用户消息，返回 threadId。
//
// channel=main 必填：缺了它消息不进主对话，/v1/conversations/messages?isMainChat=true
// 永远查不到这个 thread（实测）。
func (c *Client) Send(ctx context.Context, text string) (string, error) {
	body := map[string]any{
		"content":         text,
		"messageType":     "text",
		"clientMessageId": "msg_" + randomHex(12),
		"channel":         "main",
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/messages", body)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
	default:
		return "", classify("messages", status, raw)
	}
	var sent struct {
		ThreadID string `json:"threadId"`
	}
	if errUnmarshal := json.Unmarshal(raw, &sent); errUnmarshal != nil || sent.ThreadID == "" {
		return "", &Error{Kind: KindServer, Msg: "messages response missing threadId: " + snippet(raw)}
	}
	return sent.ThreadID, nil
}

type wireMessage struct {
	ThreadID string `json:"threadId"`
	Role     string `json:"role"`
	State    string `json:"state"`
	IsFinal  bool   `json:"isFinal"`
	Body     struct {
		Parts []struct {
			MessageType string `json:"messageType"`
			Content     struct {
				Text string `json:"text"`
			} `json:"content"`
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"body"`
}

func (m wireMessage) text() string {
	var buf strings.Builder
	for _, p := range m.Body.Parts {
		if p.MessageType == "text" {
			buf.WriteString(p.Content.Text)
			if p.Content.Text == "" {
				buf.WriteString(p.Text)
			}
		}
	}
	return buf.String()
}

// Reply 轮询到该 thread 的 assistant 消息完成。
//
// onDelta 在正文增长时被调用；返回 false 表示调用方已断开，应停止轮询。
func (c *Client) Reply(ctx context.Context, threadID string, onDelta func(string) bool) (string, error) {
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			if last != "" {
				return last, nil
			}
			return "", &Error{Kind: KindNetwork, Msg: "reply timeout: " + err.Error()}
		}
		status, raw, err := c.do(ctx, http.MethodGet, "/v1/conversations/messages?isMainChat=true&limit=5", nil)
		if err != nil {
			if ctx.Err() != nil {
				if last != "" {
					return last, nil
				}
			}
			return "", err
		}
		if status != http.StatusOK {
			return "", classify("conversations/messages", status, raw)
		}
		var wrapper struct {
			Data struct {
				Items []wireMessage `json:"items"`
			} `json:"data"`
		}
		if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal != nil {
			return "", &Error{Kind: KindServer, Msg: "decode conversations/messages: " + snippet(raw)}
		}
		for _, m := range wrapper.Data.Items {
			if m.ThreadID != threadID || m.Role != "assistant" {
				continue
			}
			text := m.text()
			if text == "" {
				continue
			}
			if len(text) > len(last) && strings.HasPrefix(text, last) {
				if onDelta != nil && !onDelta(text[len(last):]) {
					return text, nil
				}
			} else if !strings.HasPrefix(text, last) {
				// 正文被替换（不是追加）：重新吐一遍，避免下游拼接出错。
				if onDelta != nil && !onDelta(text) {
					return text, nil
				}
			}
			last = text
			if m.State == "completed" || m.IsFinal {
				return text, nil
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(c.Interval):
		}
	}
}
