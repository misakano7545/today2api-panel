package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/misakano7545/today2api-panel/internal/config"
	"github.com/misakano7545/today2api-panel/internal/today"
)

// 签到与任务：Today 没有独立的 daily-checkin 端点，每日互动由
// GET /v1/rapport 里的规则卡片承载，客户端能主动申报的只有两条动作
// （notification-enabled / discord-joined，见 today.ClientReportActions）。
// 这里把它们包装成"签到"，并把 /v1/tasks 系列包装成"做任务"。

func (s *Server) growthRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/panel/accounts/{id}/rapport", s.panelAuth(http.HandlerFunc(s.handleRapport)))
	mux.Handle("POST /api/panel/accounts/{id}/checkin", s.panelAuth(http.HandlerFunc(s.handleCheckin)))
	mux.Handle("POST /api/panel/checkin", s.panelAuth(http.HandlerFunc(s.handleCheckinAll)))
	mux.Handle("GET /api/panel/accounts/{id}/tasks", s.panelAuth(http.HandlerFunc(s.handleTasks)))
	mux.Handle("GET /api/panel/tasks", s.panelAuth(http.HandlerFunc(s.handleTasksAll)))
	mux.Handle("POST /api/panel/accounts/{id}/tasks/{taskId}/stop", s.panelAuth(http.HandlerFunc(s.handleTaskStop)))
	mux.Handle("POST /api/panel/accounts/{id}/tasks/{taskId}/follow-up", s.panelAuth(http.HandlerFunc(s.handleTaskFollowUp)))
	mux.Handle("POST /api/panel/accounts/{id}/tasks/recommendations/{recId}/apply", s.panelAuth(http.HandlerFunc(s.handleTaskApply)))
}

// accountOf 按 id 取账号；不存在时写 404 并返回 nil。
func (s *Server) accountOf(w http.ResponseWriter, id string) *config.Account {
	cfg := s.store.Snapshot()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			acct := cfg.Accounts[i]
			return &acct
		}
	}
	writeError(w, http.StatusNotFound, "账号不存在")
	return nil
}

func (s *Server) handleRapport(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	rapport, err := client.Rapport(ctx)
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": acct.ID,
		"label":   acct.Label,
		"rapport": rapport,
		"actions": today.ClientReportActions(),
	})
}

func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := client.CheckIn(ctx)
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		s.logs.add(Entry{Time: time.Now(), Path: "upstream:checkin", Account: acct.Label, Status: 502, Error: err.Error()})
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.pool.Report(acct.ID, nil, "")
	s.logs.add(Entry{Time: time.Now(), Path: "upstream:checkin", Account: acct.Label, Status: 200})
	writeJSON(w, http.StatusOK, map[string]any{"account": acct.ID, "label": acct.Label, "result": res})
}

// checkinRow 是批量签到时单账号的结果行。
type checkinRow struct {
	Account string `json:"account"`
	Label   string `json:"label"`
	Status  string `json:"status"` // ok | skipped | failed
	Delta   int    `json:"delta"`
	Detail  string `json:"detail,omitempty"`
}

func (s *Server) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	rows := s.checkinAll(r.Context())
	ok, failed := 0, 0
	for _, row := range rows {
		if row.Status == "failed" {
			failed++
			continue
		}
		ok++
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "ok": ok, "failed": failed})
}

// checkinAll 并发给所有启用账号做一次签到（并发度 4，避免上游风控）。
func (s *Server) checkinAll(ctx context.Context) []checkinRow {
	cfg := s.store.Snapshot()
	var targets []config.Account
	for _, a := range cfg.Accounts {
		if a.Enabled && a.Cookie != "" {
			targets = append(targets, a)
		}
	}
	rows := make([]checkinRow, len(targets))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, acct := range targets {
		wg.Add(1)
		go func(i int, acct config.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			row := checkinRow{Account: acct.ID, Label: acct.Label}
			client := s.pool.ClientOf(acct, cfg)
			workCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			res, err := client.CheckIn(workCtx)
			if err != nil {
				row.Status = "failed"
				row.Detail = err.Error()
				s.pool.Report(acct.ID, err, "")
				rows[i] = row
				return
			}
			s.pool.Report(acct.ID, nil, "")
			row.Status = "ok"
			row.Delta = res.Delta
			rows[i] = row
		}(i, acct)
	}
	wg.Wait()
	return rows
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	tasks, err := client.Tasks(ctx)
	if err != nil {
		s.pool.Report(acct.ID, err, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	summary, _ := client.TaskSummary(ctx, nil)
	templates, _ := client.TaskTemplates(ctx)
	recs, _ := client.TaskRecommendations(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"account":       acct.ID,
		"label":         acct.Label,
		"tasks":         tasks,
		"summary":       summary,
		"templates":     templates,
		"recommended":   recs,
	})
}

func (s *Server) handleTasksAll(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	out := []map[string]any{}
	for _, acct := range cfg.Accounts {
		if !acct.Enabled || acct.Cookie == "" {
			continue
		}
		client := s.pool.ClientOf(acct, cfg)
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		tasks, err := client.Tasks(ctx)
		cancel()
		row := map[string]any{"account": acct.ID, "label": acct.Label}
		if err != nil {
			row["error"] = err.Error()
		} else {
			row["tasks"] = tasks
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := client.StopTask(ctx, r.PathValue("taskId")); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTaskFollowUp(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		writeError(w, http.StatusBadRequest, "message 不能为空")
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := client.FollowUpTask(ctx, r.PathValue("taskId"), body.Message)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": out})
}

func (s *Server) handleTaskApply(w http.ResponseWriter, r *http.Request) {
	acct := s.accountOf(w, r.PathValue("id"))
	if acct == nil {
		return
	}
	cfg := s.store.Snapshot()
	client := s.pool.ClientOf(*acct, cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := client.ApplyRecommendation(ctx, r.PathValue("recId"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": out})
}

// StartCheckinLoop 起后台签到循环：按配置间隔跑一次批量签到。
// 只在上游可用时留日志，不阻塞主流程；ctx 取消即退出。
func (s *Server) StartCheckinLoop(ctx context.Context) {
	go func() {
		// 启动后先等一个间隔，避免和冷启动的模型探测抢配额。
		for {
			cfg := s.store.Snapshot()
			if !cfg.Checkin.Enabled || cfg.Checkin.IntervalMinutes <= 0 {
				sleep(ctx, time.Minute)
				continue
			}
			sleep(ctx, time.Duration(cfg.Checkin.IntervalMinutes)*time.Minute)
			if ctx.Err() != nil {
				return
			}
			cfg = s.store.Snapshot()
			if !cfg.Checkin.Enabled {
				continue
			}
			rows := s.checkinAll(ctx)
			for _, row := range rows {
				s.logs.add(Entry{
					Time:    time.Now(),
					Path:    "auto:checkin",
					Account: row.Label,
					Status:  200,
					Error:   row.Detail,
				})
			}
		}
	}()
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
