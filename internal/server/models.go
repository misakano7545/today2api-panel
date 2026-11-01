package server

import (
	"context"
	"net/http"
	"time"

	"github.com/misakano7545/today2api-panel/internal/today"
)

// knownModels 是官方在售的常见模型，账号套餐里探到的会并进来。
var knownModels = []string{"deepseek-v4.1-flash", "glm-5.3", "minimax-m31-flash"}

// modelList 返回模型清单，60s 内复用上次结果（避免每个客户端启动都打一次上游）。
func (s *Server) modelList(ctx context.Context) []string {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	if len(s.modelsCache) > 0 && time.Since(s.modelsAt) < 60*time.Second {
		return append([]string(nil), s.modelsCache...)
	}
	cfg := s.store.Snapshot()
	seen := map[string]bool{}
	var out []string
	for _, m := range knownModels {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	tried := 0
	for _, a := range cfg.Accounts {
		if !a.Enabled || tried >= 3 {
			continue
		}
		tried++
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		list, err := s.pool.ClientOf(a, cfg).LLMModels(probeCtx)
		cancel()
		if err != nil {
			continue
		}
		for _, m := range list {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	s.modelsCache = out
	s.modelsAt = time.Now()
	return append([]string(nil), out...)
}

// handleModels 输出 OpenAI 形状的模型清单。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	created := s.started.Unix()
	for _, m := range s.modelList(r.Context()) {
		data = append(data, map[string]any{
			"id": m, "object": "model", "created": created, "owned_by": "today",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// planFromPricing 从套餐状态里取套餐标识（typed 版，替代早先的 map 版）。
func planFromPricing(p *today.PricingStatus) string {
	if p == nil {
		return ""
	}
	if p.Plan.Name != "" {
		return p.Plan.Name
	}
	return p.Plan.ID
}
