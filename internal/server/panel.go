package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/today2api-panel/internal/config"
	"github.com/misakano7545/today2api-panel/internal/panel"
	"github.com/misakano7545/today2api-panel/internal/today"
)

const sessionCookie = "t2a_sess"

func (s *Server) panelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/panel/login", s.handlePanelLogin)
	mux.HandleFunc("POST /api/panel/logout", s.handlePanelLogout)

	mux.Handle("GET /api/panel/state", s.panelAuth(http.HandlerFunc(s.handleState)))
	mux.Handle("GET /api/panel/logs", s.panelAuth(http.HandlerFunc(s.handleLogs)))
	mux.Handle("PUT /api/panel/config", s.panelAuth(http.HandlerFunc(s.handleConfigPut)))
	mux.Handle("POST /api/panel/accounts", s.panelAuth(http.HandlerFunc(s.handleAccountAdd)))
	mux.Handle("PATCH /api/panel/accounts/{id}", s.panelAuth(http.HandlerFunc(s.handleAccountPatch)))
	mux.Handle("DELETE /api/panel/accounts/{id}", s.panelAuth(http.HandlerFunc(s.handleAccountDelete)))
	mux.Handle("POST /api/panel/accounts/{id}/check", s.panelAuth(http.HandlerFunc(s.handleAccountCheck)))
	mux.Handle("POST /api/panel/keys", s.panelAuth(http.HandlerFunc(s.handleKeyCreate)))
	mux.Handle("DELETE /api/panel/keys/{name}", s.panelAuth(http.HandlerFunc(s.handleKeyDelete)))
	mux.Handle("POST /api/panel/otp/send", s.panelAuth(http.HandlerFunc(s.handleOTPSend)))
	mux.Handle("POST /api/panel/otp/verify", s.panelAuth(http.HandlerFunc(s.handleOTPVerify)))
	mux.Handle("POST /api/panel/probe", s.panelAuth(http.HandlerFunc(s.handleProbe)))

	mux.Handle("GET /", panel.Handler())
}

// ---------- 会话 ----------

func (s *Server) panelAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.store.Snapshot()
		if cfg.PanelPassword == "" {
			if loopback(r) {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, http.StatusForbidden, "panel_password 未设置：只有本机可以管理面板")
			return
		}
		s.mu.Lock()
		exp, ok := s.sess[sessionToken(r)]
		now := time.Now()
		for token, at := range s.sess { // 顺手清过期会话
			if now.After(at) {
				delete(s.sess, token)
			}
		}
		s.mu.Unlock()
		if ok && now.Before(exp) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "login required")
	})
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) handlePanelLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	cfg := s.store.Snapshot()
	if cfg.PanelPassword == "" {
		writeError(w, http.StatusBadRequest, "面板密码未设置，本机直接访问即可")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(cfg.PanelPassword)) != 1 {
		time.Sleep(300 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "密码错误")
		return
	}
	token := config.RandomHex(24)
	exp := time.Now().Add(12 * time.Hour)
	s.mu.Lock()
	s.sess[token] = exp
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Expires: exp,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePanelLogout(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	s.mu.Lock()
	delete(s.sess, token)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 概览 ----------

type accountView struct {
	config.Account
	CooldownUntil string `json:"cooldown_until,omitempty"`
	Fails         int    `json:"fails"`
	LastErr       string `json:"last_err,omitempty"`
	LastStatus    string `json:"last_status,omitempty"`
	LastUsed      string `json:"last_used,omitempty"`
	LastModel     string `json:"last_model,omitempty"`
}

// keyView 是面板可见的密钥信息：不下发 hash。
type keyView struct {
	Name    string    `json:"name"`
	Prefix  string    `json:"prefix"`
	Created time.Time `json:"created"`
}

func keyViews(keys []config.Key) []keyView {
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyView{Name: k.Name, Prefix: k.Prefix, Created: k.Created})
	}
	return out
}

// viewOf 转成面板用的账号视图，顺手抹掉 Cookie 明文。
func viewOf(a config.Account) accountView {
	a.Cookie = ""
	return accountView{Account: a}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	now := time.Now()
	accounts := make([]accountView, 0, len(cfg.Accounts))
	enabled := 0
	for _, a := range cfg.Accounts {
		view := viewOf(a)
		if a.Enabled {
			enabled++
		}
		if st, ok := s.pool.Snapshot(a.ID); ok {
			view.Fails = st.Fails
			view.LastErr = st.LastErr
			view.LastStatus = st.LastStatus
			view.LastModel = st.LastModel
			if now.Before(st.Cooldown) {
				view.CooldownUntil = st.Cooldown.Format(time.RFC3339)
			}
			if !st.LastUsed.IsZero() {
				view.LastUsed = st.LastUsed.Format(time.RFC3339)
			}
		}
		accounts = append(accounts, view)
	}
	logs := s.logs.Recent(50)
	errs := 0
	for _, e := range logs {
		if e.Error != "" {
			errs++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.version,
		"uptime":  int(now.Sub(s.started).Seconds()),
		"listen":  cfg.Listen,
		"config": map[string]any{
			"default_model":      cfg.DefaultModel,
			"reply_timeout_sec":  cfg.ReplyTimeoutSec,
			"poll_interval_ms":   cfg.PollIntervalMs,
			"context_turns":      cfg.ContextTurns,
			"panel_password_set": cfg.PanelPassword != "",
			"upstream":           cfg.Upstream,
		},
		"accounts": accounts,
		"keys":     keyViews(cfg.Keys),
		"stats": map[string]any{
			"accounts": len(cfg.Accounts),
			"enabled":  enabled,
			"cooling":  s.pool.Cooling(),
			"errors":   errs,
		},
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 100
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.logs.Recent(n)})
}

// ---------- 配置 ----------

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DefaultModel    *string `json:"default_model"`
		ReplyTimeoutSec *int    `json:"reply_timeout_sec"`
		PollIntervalMs  *int    `json:"poll_interval_ms"`
		ContextTurns    *int    `json:"context_turns"`
		PanelPassword   *string `json:"panel_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	err := s.store.Update(func(c *config.Config) error {
		if body.DefaultModel != nil && strings.TrimSpace(*body.DefaultModel) != "" {
			c.DefaultModel = strings.TrimSpace(*body.DefaultModel)
		}
		if body.ReplyTimeoutSec != nil && *body.ReplyTimeoutSec > 0 {
			c.ReplyTimeoutSec = *body.ReplyTimeoutSec
		}
		if body.PollIntervalMs != nil && *body.PollIntervalMs >= 50 {
			c.PollIntervalMs = *body.PollIntervalMs
		}
		if body.ContextTurns != nil && *body.ContextTurns >= 0 {
			c.ContextTurns = *body.ContextTurns
		}
		if body.PanelPassword != nil {
			c.PanelPassword = *body.PanelPassword
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.modelsMu.Lock()
	s.modelsAt = time.Time{}
	s.modelsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 账号 ----------

func (s *Server) handleAccountAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label  string `json:"label"`
		Realm  string `json:"realm"`
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	acct, err := s.addAccount(r.Context(), body.Label, body.Realm, body.Cookie)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": viewOf(*acct)})
}

// addAccount 校验 Cookie 真的能换到 JWT，然后落盘。
func (s *Server) addAccount(ctx context.Context, label, realm, cookie string) (*config.Account, error) {
	if realm != "cn" && realm != "global" {
		realm = "global"
	}
	cookie = config.NormalizeCookie(cookie)
	if cookie == "" {
		return nil, errTextf("请粘贴 __Secure-better-auth.session_token 或整段 Cookie")
	}
	cfg := s.store.Snapshot()
	ep := cfg.EndpointsFor(realm)
	client := today.NewClient(realm, cookie, today.Bases{App: ep.App, API: ep.API, Auth: ep.Auth})
	client.HC = s.httpc
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := client.Token(verifyCtx); err != nil {
		return nil, errTextf("Cookie 无法换取 JWT：" + err.Error())
	}
	me, errMe := client.Me(verifyCtx)
	plan := ""
	if errMe == nil {
		if p := planOf(me); p != "" {
			plan = p
		}
	}
	if st, errStatus := client.PricingStatus(verifyCtx); errStatus == nil {
		if p := planOf(st); p != "" {
			plan = p
		}
	}
	if strings.TrimSpace(label) == "" {
		for _, key := range []string{"email", "preferredName", "name", "id"} {
			if v, ok := me[key].(string); ok && v != "" {
				label = v
				break
			}
		}
	}
	if label == "" {
		label = "Today-" + config.RandomHex(3)
	}
	acct := config.Account{
		ID:      "acc_" + config.RandomHex(6),
		Label:   label,
		Realm:   realm,
		Cookie:  cookie,
		Enabled: true,
		AddedAt: time.Now(),
		Plan:    plan,
	}
	errSave := s.store.Update(func(c *config.Config) error {
		for _, existing := range c.Accounts {
			if existing.Cookie == cookie {
				return errTextf("这个账号已经在池子里了")
			}
		}
		c.Accounts = append(c.Accounts, acct)
		return nil
	})
	if errSave != nil {
		return nil, errSave
	}
	return &acct, nil
}

func planOf(m map[string]any) string {
	if v, ok := m["plan"].(string); ok {
		return v
	}
	if block, ok := m["plan"].(map[string]any); ok {
		if id, okID := block["id"].(string); okID {
			return id
		}
	}
	return ""
}

func (s *Server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled *bool   `json:"enabled"`
		Label   *string `json:"label"`
		Cookie  *string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	err := s.store.Update(func(c *config.Config) error {
		for i := range c.Accounts {
			if c.Accounts[i].ID != id {
				continue
			}
			if body.Enabled != nil {
				c.Accounts[i].Enabled = *body.Enabled
			}
			if body.Label != nil && strings.TrimSpace(*body.Label) != "" {
				c.Accounts[i].Label = strings.TrimSpace(*body.Label)
			}
			if body.Cookie != nil && strings.TrimSpace(*body.Cookie) != "" {
				c.Accounts[i].Cookie = config.NormalizeCookie(*body.Cookie)
			}
			return nil
		}
		return errTextf("账号不存在")
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.pool.Report(id, nil, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(func(c *config.Config) error {
		for i := range c.Accounts {
			if c.Accounts[i].ID == id {
				c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
				return nil
			}
		}
		return errTextf("账号不存在")
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.pool.Forget(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAccountCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg := s.store.Snapshot()
	var acct *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			acct = &cfg.Accounts[i]
			break
		}
	}
	if acct == nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client := s.pool.ClientOf(*acct, cfg)
	client.HC = s.httpc
	if _, err := client.Token(ctx); err != nil {
		s.pool.Report(id, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	me, errMe := client.Me(ctx)
	if errMe != nil {
		s.pool.Report(id, errMe, "")
		writeError(w, http.StatusBadGateway, errMe.Error())
		return
	}
	plan := planOf(me)
	models, _ := client.Models(ctx)
	if st, errStatus := client.PricingStatus(ctx); errStatus == nil {
		if p := planOf(st); p != "" {
			plan = p
		}
	}
	s.pool.Report(id, nil, "")
	_ = s.store.Update(func(c *config.Config) error {
		for i := range c.Accounts {
			if c.Accounts[i].ID == id {
				c.Accounts[i].Plan = plan
			}
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plan": plan, "models": models, "me": me})
}

// ---------- 密钥 ----------

func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "key-" + config.RandomHex(2)
	}
	plain := "sk-today-" + config.RandomHex(16)
	sum := sha256.Sum256([]byte(plain))
	key := config.Key{
		Name:    name,
		Hash:    hex.EncodeToString(sum[:]),
		Prefix:  plain[:14] + "…",
		Created: time.Now(),
	}
	err := s.store.Update(func(c *config.Config) error {
		for _, existing := range c.Keys {
			if existing.Name == name {
				return errTextf("同名密钥已存在")
			}
		}
		c.Keys = append(c.Keys, key)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": plain, "name": key.Name, "prefix": key.Prefix})
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.store.Update(func(c *config.Config) error {
		for i := range c.Keys {
			if c.Keys[i].Name == name {
				c.Keys = append(c.Keys[:i], c.Keys[i+1:]...)
				return nil
			}
		}
		return errTextf("密钥不存在")
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 邮箱验证码登录 ----------

func (s *Server) handleOTPSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Email) == "" {
		writeError(w, http.StatusBadRequest, "email 不能为空")
		return
	}
	if body.Realm != "cn" {
		body.Realm = "global"
	}
	cfg := s.store.Snapshot()
	ep := cfg.EndpointsFor(body.Realm)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := today.SendOTP(ctx, s.httpc, today.Bases{App: ep.App, API: ep.API, Auth: ep.Auth}, body.Email); err != nil {
		writeError(w, http.StatusBadGateway, "发送验证码失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleOTPVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
		Email string `json:"email"`
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.Realm != "cn" {
		body.Realm = "global"
	}
	cfg := s.store.Snapshot()
	ep := cfg.EndpointsFor(body.Realm)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cookie, err := today.SignInOTP(ctx, s.httpc, today.Bases{App: ep.App, API: ep.API, Auth: ep.Auth}, body.Email, body.Code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "验证码登录失败："+err.Error())
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = body.Email
	}
	acct, errAdd := s.addAccount(r.Context(), label, body.Realm, cookie)
	if errAdd != nil {
		writeError(w, http.StatusBadGateway, errAdd.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": viewOf(*acct)})
}

// ---------- 探针 ----------

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	cfg := s.store.Snapshot()
	model := resolveModel(body.Model, cfg.DefaultModel)
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		prompt = "用一句话介绍你自己。"
	}
	started := time.Now()
	ch := s.Start(r.Context(), model, prompt)
	text, account, _, err := collect(ch)
	result := map[string]any{
		"ok":          err == nil,
		"model":       model,
		"account":     account,
		"reply":       text,
		"duration_ms": time.Since(started).Milliseconds(),
	}
	if err != nil {
		result["error"] = err.Error()
	}
	s.logs.add(Entry{Time: started, Path: "panel:probe", Model: model, Account: account, Status: statusOf(err), Duration: time.Since(started).Milliseconds(), Chars: len([]rune(text)), Error: errText(err)})
	writeJSON(w, http.StatusOK, result)
}

// errTextf 是带消息的普通错误。
func errTextf(msg string) error { return &plainError{msg} }

type plainError struct{ msg string }

func (e *plainError) Error() string { return e.msg }
