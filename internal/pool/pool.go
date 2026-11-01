// Package pool 管理账号池的运行时状态：轮转选号、失败冷却、客户端复用。
//
// 账号本身（Cookie、启用开关）存在 config.json；这里只放进程内的易失状态，
// 重启后从零开始（冷却与失败计数没有跨重启的意义）。
package pool

import (
	"sort"
	"sync"
	"time"

	"github.com/misakano7545/today2api-panel/internal/config"
	"github.com/misakano7545/today2api-panel/internal/today"
)

// State 是一个账号的运行时状态。
type State struct {
	Client     *today.Client
	Cooldown   time.Time
	Fails      int
	LastErr    string
	LastUsed   time.Time
	LastModel  string
	LastStatus string
}

// Pool 是账号池。
type Pool struct {
	mu    sync.Mutex
	next  int
	state map[string]*State

	// cooldown 按错误归类给出冷却时长，测试可覆盖。
	cooldown map[today.Kind]time.Duration
}

// New 建一个空池。
func New() *Pool {
	return &Pool{
		state: map[string]*State{},
		cooldown: map[today.Kind]time.Duration{
			today.KindAuth:    10 * time.Minute,
			today.KindRate:    10 * time.Minute,
			today.KindServer:  60 * time.Second,
			today.KindNetwork: 30 * time.Second,
		},
	}
}

// SetCooldown 覆盖某类错误的冷却时长（测试用）。
func (p *Pool) SetCooldown(kind today.Kind, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldown[kind] = d
}

// Pick 按轮转选一个可用账号，并返回它的客户端。
//
// skip 里是本轮已试过的账号 id（故障转移）。全部冷却中时返回 remaining 提示。
func (p *Pool) Pick(accounts []config.Account, cfg config.Config, skip map[string]bool) (*config.Account, *today.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var candidates []config.Account
	for _, a := range accounts {
		if !a.Enabled || skip[a.ID] || (a.Cookie == "" && a.Token == "") {
			continue
		}
		if st := p.state[a.ID]; st != nil && time.Now().Before(st.Cooldown) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil, nil, ErrNoAccount
	}
	// 轮转起点：按 id 排序保证顺序稳定，再取模推进。
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	pick := candidates[p.next%len(candidates)]
	p.next++

	st := p.state[pick.ID]
	if st == nil || st.Client == nil || st.Client.Cookie != pick.Cookie || st.Client.StaticToken != pick.Token {
		ep := cfg.EndpointsFor(pick.Realm)
		client := today.NewClient(pick.Realm, pick.Cookie, today.Bases{App: ep.App, API: ep.API, Auth: ep.Auth, Audience: ep.Audience})
		client.StaticToken = pick.Token
		client.Interval = time.Duration(cfg.PollIntervalMs) * time.Millisecond
		if st == nil {
			st = &State{}
			p.state[pick.ID] = st
		}
		st.Client = client
	}
	st.LastUsed = time.Now()
	return &pick, st.Client, nil
}

// ClientOf 复用（或新建）某账号的客户端，不参与轮转（面板探测 / 模型列表用）。
func (p *Pool) ClientOf(a config.Account, cfg config.Config) *today.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[a.ID]
	if st == nil {
		st = &State{}
		p.state[a.ID] = st
	}
	if st.Client == nil || st.Client.Cookie != a.Cookie || st.Client.StaticToken != a.Token {
		ep := cfg.EndpointsFor(a.Realm)
		client := today.NewClient(a.Realm, a.Cookie, today.Bases{App: ep.App, API: ep.API, Auth: ep.Auth, Audience: ep.Audience})
		client.StaticToken = a.Token
		client.Interval = time.Duration(cfg.PollIntervalMs) * time.Millisecond
		st.Client = client
	}
	return st.Client
}

// Report 记一次结果：成功清冷却，失败按归类冷却。
func (p *Pool) Report(id string, err error, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[id]
	if st == nil {
		st = &State{}
		p.state[id] = st
	}
	if model != "" {
		st.LastModel = model
	}
	if err == nil {
		st.Fails = 0
		st.LastErr = ""
		st.LastStatus = "ok"
		st.Cooldown = time.Time{}
		return
	}
	kind := today.KindOf(err)
	st.Fails++
	st.LastErr = err.Error()
	st.LastStatus = string(kind)
	st.Cooldown = time.Now().Add(p.cooldown[kind])
}

// Snapshot 返回某账号的运行时状态副本。
func (p *Pool) Snapshot(id string) (State, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[id]
	if !ok || st == nil {
		return State{}, false
	}
	out := *st
	out.Client = nil
	return out, true
}

// Forget 丢弃某账号的运行时状态（删除账号时调用）。
func (p *Pool) Forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.state, id)
}

// Len 返回有运行时状态的账号数（面板统计用）。
func (p *Pool) Cooling() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := 0
	for _, st := range p.state {
		if st != nil && now.Before(st.Cooldown) {
			n++
		}
	}
	return n
}

// ErrNoAccount 表示没有可用账号。
var ErrNoAccount = &noAccountError{}

type noAccountError struct{}

func (*noAccountError) Error() string {
	return "no available today account (all disabled, cooling or skipped)"
}
