// Package config 保存网关配置（config.json）：账号池、下游密钥、上游地址与运行参数。
//
// 配置即状态：面板的所有写操作都走 Store.Update，落盘用临时文件 + rename，
// 避免写到一半崩溃把账号表弄坏。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Endpoints 是一个区域的上游三件套；留空则用官方默认值。
type Endpoints struct {
	App  string `json:"app,omitempty"`
	API  string `json:"api,omitempty"`
	Auth string `json:"auth,omitempty"`
}

// Upstream 分国内版（todayai.cn）与国际版（today.ai）。两边的 Cookie 不通用。
type Upstream struct {
	CN     Endpoints `json:"cn"`
	Global Endpoints `json:"global"`
}

// Account 是一个 Today 账号。Cookie 是登录态原文，只存服务端。
type Account struct {
	ID      string    `json:"id"`
	Label   string    `json:"label,omitempty"`
	Realm   string    `json:"realm"` // cn | global
	Cookie  string    `json:"cookie"`
	Enabled bool      `json:"enabled"`
	AddedAt time.Time `json:"added_at"`
	Plan    string    `json:"plan,omitempty"` // 最近一次探测到的套餐
}

// Key 是下游客户端密钥；只存 sha256，明文仅在创建时返回一次。
type Key struct {
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Prefix  string    `json:"prefix"`
	Created time.Time `json:"created"`
}

// Config 是 config.json 的结构。
type Config struct {
	Listen          string    `json:"listen"`
	PanelPassword   string    `json:"panel_password"`
	DefaultModel    string    `json:"default_model"`
	ReplyTimeoutSec int       `json:"reply_timeout_sec"`
	PollIntervalMs  int       `json:"poll_interval_ms"`
	ContextTurns    int       `json:"context_turns"`
	Upstream        Upstream  `json:"upstream"`
	Checkin         Checkin   `json:"checkin"`
	Keys            []Key     `json:"keys"`
	Accounts        []Account `json:"accounts"`
}

// Checkin 是后台自动签到的配置。
//
// Today 的签到 = 读 /v1/rapport + 上报两条客户端可申报动作，
// 两条动作上游都是幂等的，所以重复跑不会重复计分。
type Checkin struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

func Defaults() Config {
	return Config{
		Listen:          "0.0.0.0:8690",
		DefaultModel:    "deepseek-v4.1-flash",
		ReplyTimeoutSec: 240,
		PollIntervalMs:  250,
		Upstream: Upstream{
			CN:     Endpoints{App: "https://todayai.cn", API: "https://api.todayai.cn", Auth: "https://auth.todayai.cn"},
			Global: Endpoints{App: "https://today.ai", API: "https://api.today.ai", Auth: "https://auth.today.ai"},
		},
		Keys:     []Key{},
		Accounts: []Account{},
	}
}

// Normalize 补齐缺省值并修正非法取值；在加载与每次保存前都会跑一遍。
func (c *Config) Normalize() {
	if c.Checkin.Enabled && c.Checkin.IntervalMinutes <= 0 {
		c.Checkin.IntervalMinutes = 720
	}
	if c.Checkin.IntervalMinutes < 0 {
		c.Checkin.IntervalMinutes = 0
	}
	d := Defaults()
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = d.Listen
	}
	if strings.TrimSpace(c.DefaultModel) == "" {
		c.DefaultModel = d.DefaultModel
	}
	if c.ReplyTimeoutSec <= 0 {
		c.ReplyTimeoutSec = d.ReplyTimeoutSec
	}
	if c.PollIntervalMs < 50 {
		c.PollIntervalMs = d.PollIntervalMs
	}
	for _, pair := range []struct {
		dst *Endpoints
		def Endpoints
	}{
		{&c.Upstream.CN, d.Upstream.CN},
		{&c.Upstream.Global, d.Upstream.Global},
	} {
		if pair.dst.App == "" {
			pair.dst.App = pair.def.App
		}
		if pair.dst.API == "" {
			pair.dst.API = pair.def.API
		}
		if pair.dst.Auth == "" {
			pair.dst.Auth = pair.def.Auth
		}
	}
	for i := range c.Accounts {
		if c.Accounts[i].Realm != "cn" {
			c.Accounts[i].Realm = "global"
		}
	}
}

// EndpointsFor 返回某区域的上游地址。
func (c *Config) EndpointsFor(realm string) Endpoints {
	if realm == "cn" {
		return c.Upstream.CN
	}
	return c.Upstream.Global
}

// NormalizeCookie 接受「整段 Cookie 头」或「只有 session 值」两种粘贴方式。
func NormalizeCookie(raw string) string {
	v := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(raw), "\r", ""), "\n", " ")
	v = strings.Join(strings.Fields(v), " ")
	if v == "" {
		return ""
	}
	if strings.Contains(v, "=") {
		return v
	}
	return "__Secure-better-auth.session_token=" + v
}

// Store 是并发安全的配置容器。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Load 读取配置文件；文件不存在时写入默认配置。
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.cfg = Defaults()
		if errSave := s.saveLocked(); errSave != nil {
			return nil, errSave
		}
		return s, nil
	case err != nil:
		return nil, err
	}
	if errUnmarshal := json.Unmarshal(raw, &s.cfg); errUnmarshal != nil {
		return nil, fmt.Errorf("parse %s: %w", path, errUnmarshal)
	}
	s.cfg.Normalize()
	return s, nil
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Snapshot 返回配置副本（切片已复制，调用方可随意读写）。
func (s *Store) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.cfg
	out.Keys = append([]Key{}, s.cfg.Keys...)
	out.Accounts = append([]Account{}, s.cfg.Accounts...)
	return out
}

// Update 在写锁内修改配置并落盘；fn 返回错误时不改动任何内容。
func (s *Store) Update(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	next.Keys = append([]Key{}, s.cfg.Keys...)
	next.Accounts = append([]Account{}, s.cfg.Accounts...)
	if err := fn(&next); err != nil {
		return err
	}
	next.Normalize()
	prev := s.cfg
	s.cfg = next
	if err := s.saveLocked(); err != nil {
		s.cfg = prev
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	buf, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
			return errMkdir
		}
	}
	tmp := s.path + ".tmp"
	if errWrite := os.WriteFile(tmp, append(buf, '\n'), 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, s.path)
}

// RandomHex 返回 n 字节随机数的十六进制串。
func RandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand 失败等于环境不可用，没有可降级路径
	}
	return hex.EncodeToString(buf)
}
