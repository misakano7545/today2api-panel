// Package today 实现 Today（todayai.cn / today.ai）的上游协议。
//
// 协议链（以真实客户端为准：1.24.0 静态逆向 + 上游实测）：
//
//	登录 Cookie（__Secure-better-auth.session_token=…）
//	  └─ POST {app}/api/token  {audience, expiresIn:3600}
//	       → {token, expiresAt}          ← API JWT（小时级）
//	          └─ Authorization: Bearer *** + X-App-Version / X-Client-Platform
//	             → {api}/v1/*
//
// 桌面客户端另有一条 OAuth2 链（/api/auth/oauth2/token + PKCE + refresh_token），
// 适合没有 Cookie 的场景；网关默认走 Cookie 链。
//
// 对话是 canonical-v1 协议：POST /v1/messages 收下
//
//	{body:{parts:[{id,type:"text",text}]}, clientMessageId, isMainChat:true}
//
// 立刻回 {canonicalMessage:{…}}（用户消息本体），助手回复靠
// GET /v1/conversations/messages?conversationId=… 轮询增量；桌面端还能用
// /v3/conversations/{id}/message-changes 长轮询，由 GET /v1/config 协商。
// 上游没有 SSE，流式由网关把轮询增量伪流式下发。
package today

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 客户端身份头。上游按这两个头做版本/平台兼容分支，缺了会掉进 legacy 分支。
const (
	ClientVersion  = "1.24.0"
	ClientPlatform = "windows-client"
)

// Bases 是一个区域的上游三件套。
//
// Audience 留空时用官方 API 域名；API 指向中转（例如前端同源代理
// https://todayai.cn/api/cloud）时必须显式给官方 API，否则签出的 JWT
// 受众不匹配，上游以 “Token introspection failed” 拒绝。
type Bases struct {
	App      string
	API      string
	Auth     string
	Audience string
}

// OfficialBases 返回国内版官方地址（本项目只支持 todayai.cn）。
func OfficialBases() Bases {
	return Bases{App: "https://todayai.cn", API: "https://api.todayai.cn", Auth: "https://auth.todayai.cn"}
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
		u, ok := err.(interface{ Unwrap() error })
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
	// StaticToken 非空时直接用这张 JWT，跳过 /api/token 换票（临时验证用）。
	StaticToken string

	mu       sync.Mutex
	jwt      string
	jwtExp   time.Time
	option   string        // 该账号当前生效的模型选项，避免每次请求都 PUT
	cfg      *ClientConfig // /v1/config 协商结果
	cfgAt    time.Time
	deviceID string
}

// NewClient 构造客户端。bases 留空的字段回落到官方地址。
func NewClient(cookie string, bases Bases) *Client {
	def := OfficialBases()
	if bases.App == "" {
		bases.App = def.App
	}
	if bases.API == "" {
		bases.API = def.API
	}
	if bases.Auth == "" {
		bases.Auth = def.Auth
	}
	if bases.Audience == "" {
		bases.Audience = def.API
	}
	return &Client{
		Bases:    Bases{App: trimSlash(bases.App), API: trimSlash(bases.API), Auth: trimSlash(bases.Auth), Audience: trimSlash(bases.Audience)},
		Cookie:   cookie,
		Interval: 250 * time.Millisecond,
		HC:       &http.Client{Timeout: 60 * time.Second},
		deviceID: newUUID(),
	}
}

func trimSlash(s string) string { return strings.TrimRight(s, "/") }

// DeviceID 是本次进程的稳定设备标识（对齐桌面端 X-Device-Id）。
func (c *Client) DeviceID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deviceID
}

// Token 换出 API JWT（提前 60s 续期）。
func (c *Client) Token(ctx context.Context) (string, error) {
	if c.StaticToken != "" {
		return c.StaticToken, nil
	}
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
	token, exp, err := c.fetchToken(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.jwt = token
	c.jwtExp = exp
	c.mu.Unlock()
	return token, nil
}

// fetchToken 走 web 链：POST {app}/api/token，Cookie 鉴权。
// 上游返回 {token, expiresAt}；兼容只给 expiresIn 的老响应。
// {auth}/api/token 兜底——部分区域只在 auth 域名暴露该路由。
func (c *Client) fetchToken(ctx context.Context) (string, time.Time, error) {
	// audience 是 JWT 受众，恒为官方 API；c.API 只是实际调用地址（可能是中转）。
	body, _ := json.Marshal(map[string]any{"audience": c.Audience, "expiresIn": 3600})
	var lastErr error
	for _, endpoint := range []string{c.App + "/api/token", c.Auth + "/api/token"} {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if errReq != nil {
			return "", time.Time{}, errReq
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Language", "zh-CN")
		req.Header.Set("Origin", c.App)
		req.Header.Set("Cookie", c.Cookie)
		req.Header.Set("X-App-Version", ClientVersion)
		req.Header.Set("X-Client-Platform", ClientPlatform)
		resp, errDo := c.HC.Do(req)
		if errDo != nil {
			lastErr = &Error{Kind: KindNetwork, Msg: errDo.Error()}
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			lastErr = &Error{Kind: KindServer, Status: resp.StatusCode, Msg: "no token endpoint at " + endpoint}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			kind := KindServer
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				kind = KindAuth
			}
			return "", time.Time{}, &Error{Kind: kind, Status: resp.StatusCode, Msg: snippet(raw)}
		}
		var parsed struct {
			Token     string `json:"token"`
			ExpiresIn int    `json:"expiresIn"`
			ExpiresAt string `json:"expiresAt"`
		}
		if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil || parsed.Token == "" {
			return "", time.Time{}, &Error{Kind: KindServer, Msg: "token response missing token: " + snippet(raw)}
		}
		return parsed.Token, expiryOf(parsed.Token, parsed.ExpiresAt, parsed.ExpiresIn), nil
	}
	if lastErr == nil {
		lastErr = &Error{Kind: KindServer, Msg: "no token endpoint tried"}
	}
	return "", time.Time{}, lastErr
}

// expiryOf 依次用 JWT exp / expiresAt / expiresIn 推断过期时刻。
func expiryOf(token, expiresAt string, expiresIn int) time.Time {
	if claims, ok := jwtClaims(token); ok {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			return time.Unix(int64(exp), 0)
		}
	}
	if expiresAt != "" {
		if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			return t
		}
	}
	if expiresIn <= 0 {
		expiresIn = 540
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second)
}

// jwtClaims 不验签，只读 payload——签名由上游负责，这里只取过期时间。
func jwtClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

// apiHeaders 补齐桌面端身份头。
func (c *Client) apiHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("X-App-Version", ClientVersion)
	req.Header.Set("X-Client-Platform", ClientPlatform)
	req.Header.Set("X-Device-Id", c.DeviceID())
	req.Header.Set("X-Timezone", "Asia/Shanghai")
}

// do 发一个带 JWT 的 API 请求；401 时强制换票重试一次。
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
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
		target := c.API + path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		req, errReq := http.NewRequestWithContext(ctx, method, target, reader)
		if errReq != nil {
			return 0, nil, errReq
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", c.App)
		c.apiHeaders(req)
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

// getJSON 发 GET 并把 2xx 的 body 解到 out。
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, what string, out any) error {
	status, raw, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return classify(what, status, raw)
	}
	if out == nil {
		return nil
	}
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		return &Error{Kind: KindServer, Msg: "decode " + what + ": " + snippet(raw)}
	}
	return nil
}

// postJSON 发 POST，返回原始 body 供调用方按需解码。
func (c *Client) postJSON(ctx context.Context, path string, query url.Values, what string, body any) ([]byte, error) {
	status, raw, err := c.do(ctx, http.MethodPost, path, query, body)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
	default:
		return nil, classify(what, status, raw)
	}
	return raw, nil
}

// putJSON 发 PUT。
func (c *Client) putJSON(ctx context.Context, path string, what string, body any) (int, []byte, error) {
	status, raw, err := c.do(ctx, http.MethodPut, path, nil, body)
	if err != nil {
		return status, raw, err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return status, raw, nil
	}
	return status, raw, classify(what, status, raw)
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
	var out map[string]any
	if err := c.getJSON(ctx, "/v1/users/me", nil, "users/me", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// randomHex 返回 n 字节的十六进制串。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
