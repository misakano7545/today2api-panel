package today

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

// SendOTP 发邮箱验证码。
//
// 只有国际版支持邮箱登录：CN 版返回 403 auth.email_sign_in_disabled（实测）。
func SendOTP(ctx context.Context, hc *http.Client, bases Bases, email string) error {
	return postAuth(ctx, hc, bases, "/api/auth/email-otp/send-verification-otp",
		map[string]any{"email": email, "type": "sign-in"}, nil)
}

// SignInOTP 用验证码登录，返回可用的 Cookie 头。
func SignInOTP(ctx context.Context, hc *http.Client, bases Bases, email, code string) (string, error) {
	var cookie string
	err := postAuth(ctx, hc, bases, "/api/auth/sign-in/email-otp",
		map[string]any{"email": email, "otp": code}, func(h http.Header) { cookie = sessionCookies(h) })
	if err != nil {
		return "", err
	}
	if cookie == "" {
		return "", &Error{Kind: KindServer, Msg: "login succeeded but no session cookie returned"}
	}
	return cookie, nil
}

// postAuth 打 better-auth 接口。Origin 必须带上，缺了会被 403。
func postAuth(ctx context.Context, hc *http.Client, bases Bases, path string, body any, onResp func(http.Header)) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(bases.Auth, "/")+path, bytes.NewReader(buf))
	if errReq != nil {
		return errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", strings.TrimRight(bases.App, "/"))
	if hc == nil {
		hc = &http.Client{}
	}
	resp, errDo := hc.Do(req)
	if errDo != nil {
		return &Error{Kind: KindNetwork, Msg: errDo.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if onResp != nil {
		onResp(resp.Header)
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{Kind: KindAuth, Status: resp.StatusCode, Msg: snippet(raw)}
	}
	return nil
}

func sessionCookies(h http.Header) string {
	var parts []string
	for _, line := range h.Values("Set-Cookie") {
		segment := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		if segment == "" {
			continue
		}
		name := strings.SplitN(segment, "=", 2)[0]
		if strings.Contains(name, "session_token") || strings.Contains(name, "better-auth") {
			parts = append(parts, segment)
		}
	}
	return strings.Join(parts, "; ")
}
