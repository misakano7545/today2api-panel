package today

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// Rapport 是 GET /v1/rapport 的 data 段。
//
// 实测（2026-11，国际版 free 账号）：
//
//	{"success":true,"code":"common.ok","message":"OK","data":{
//	  "rapport":{"currentPoints":20,"nextLevelPoints":100,"progress":0.2,
//	             "isMaxLevel":false,
//	             "currentLevel":{"key":"origin","points":0,"name":"Origin","description":"…"},
//	             "updatedAt":"2026-11-01T02:24:46.232Z"},
//	  "levels":[{"key":"origin","points":0,"name":"Origin","description":"…"}, …],
//	  "rules":[{"key":"memory_update","title":"Memory Update","description":"…",
//	            "note":"Up to 3 times per day · 6 points/day max","points":2,
//	            "status":"available","scoreTrigger":"server_event",
//	            "action":{"type":"deeplink","url":"today://memories"}}, …]}}
//
// 关键：规则是 **server_event** 驱动（status=available/completed），
// 客户端点不动；能主动申报的只有 actions/ 下那两条。
type Rapport struct {
	Rapport RapportSummary `json:"rapport"`
	Levels  []RapportLevel `json:"levels"`
	Rules   []RapportRule  `json:"rules"`
}

// RapportSummary 是当前积分与等级。
type RapportSummary struct {
	CurrentPoints   int          `json:"currentPoints"`
	NextLevelPoints int          `json:"nextLevelPoints"`
	Progress        float64      `json:"progress"`
	IsMaxLevel      bool         `json:"isMaxLevel"`
	CurrentLevel    RapportLevel `json:"currentLevel"`
	UpdatedAt       string       `json:"updatedAt"`
}

// RapportLevel 是等级轨道里的一档。
type RapportLevel struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Points      int    `json:"points"`
	Description string `json:"description"`
}

// RapportRule 是一条可完成项。Status 取值 available / completed。
type RapportRule struct {
	Key          string `json:"key"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Note         string `json:"note"`
	Points       int    `json:"points"`
	Status       string `json:"status"`
	ScoreTrigger string `json:"scoreTrigger"`
	Action       struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"action"`
}

// Done 表示这条规则已经入账。
func (r RapportRule) Done() bool { return r.Status == "completed" }

// 客户端可以主动上报的两条规则：上游注释写明“服务端持有规则与分值，
// 客户端不能提供 rule key 或点数”，所以只能打这两个端点，不能伪造别的。
const (
	RapportActionNotificationEnabled = "notification-enabled"
	RapportActionDiscordJoined       = "discord-joined"
)

// ClientReportActions 是客户端有权上报的动作全集。
func ClientReportActions() []string {
	return []string{RapportActionNotificationEnabled, RapportActionDiscordJoined}
}

// Rapport 读亲密度快照（积分 + 等级 + 规则）。
//
// 响应是信封 {success,code,message,data:{rapport,levels,rules}}。
func (c *Client) Rapport(ctx context.Context) (*Rapport, error) {
	var envelope struct {
		Data Rapport `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/rapport", nil, "rapport", &envelope); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// ReportRapportAction 上报一条客户端可申报的动作（幂等）。
func (c *Client) ReportRapportAction(ctx context.Context, action string) error {
	_, err := c.postJSON(ctx, "/v1/rapport/actions/"+action, nil, "rapport/actions/"+action, map[string]any{})
	return err
}

// CheckInResult 是一次签到的结果快照。
type CheckInResult struct {
	Before   *Rapport          `json:"before"`
	After    *Rapport          `json:"after"`
	Delta    int               `json:"delta"`
	Reported []string          `json:"reported"`
	Failed   map[string]string `json:"failed,omitempty"`
	Claimed  []string          `json:"claimed"`
}

// CheckIn 执行一次“签到”。
//
// Today 没有独立的签到端点：每日互动由 /v1/rapport 的规则卡片承载，
// 客户端能做的是读取快照 + 上报两条可申报动作（通知已开、Discord 已加入）。
// 这里按客户端的顺序做，并把积分变化作为签到结果返回。
func (c *Client) CheckIn(ctx context.Context) (*CheckInResult, error) {
	before, err := c.Rapport(ctx)
	if err != nil {
		return nil, err
	}
	res := &CheckInResult{Before: before, Claimed: []string{}}
	for _, action := range ClientReportActions() {
		if errAction := c.ReportRapportAction(ctx, action); errAction != nil {
			if res.Failed == nil {
				res.Failed = map[string]string{}
			}
			res.Failed[action] = errAction.Error()
			continue
		}
		res.Reported = append(res.Reported, action)
	}
	after, errAfter := c.Rapport(ctx)
	if errAfter != nil {
		return res, nil
	}
	res.After = after
	res.Delta = after.Rapport.CurrentPoints - before.Rapport.CurrentPoints
	for _, rule := range after.Rules {
		if rule.Done() {
			res.Claimed = append(res.Claimed, rule.Key)
		}
	}
	return res, nil
}

// Task 是 /v1/tasks 里的任务条目（只保留网关用得上的字段，其余原样留 raw）。
type Task struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    string         `json:"status"`
	CreatedAt string         `json:"createdAt"`
	UpdatedAt string         `json:"updatedAt"`
	Raw       map[string]any `json:"-"`
}

// Tasks 列任务。
//
// 实测响应：{"object":"list","data":[],"nextCursor":null,"hasMore":false}
func (c *Client) Tasks(ctx context.Context) ([]Task, error) {
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/tasks", nil, "tasks", &envelope); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		out = append(out, toTask(item))
	}
	return out, nil
}

// TaskSummary 读任务汇总。
//
// 上游要求 scope 或 from 参数（缺了回 400 "Either scope or from is required"），
// 这里交给调用方传 query。
func (c *Client) TaskSummary(ctx context.Context, query url.Values) (map[string]any, error) {
	if len(query) == 0 {
		return map[string]any{}, nil
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/tasks/summary", query, "tasks/summary", &envelope); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// TaskTemplates 读任务模板类型。
//
// 实测响应：{"data":{"items":[{"type":"general","typeName":"General"}, …]}}
func (c *Client) TaskTemplates(ctx context.Context) ([]TaskTemplate, error) {
	var envelope struct {
		Data struct {
			Items []TaskTemplate `json:"items"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/tasks/templates/types", nil, "tasks/templates/types", &envelope); err != nil {
		return nil, err
	}
	return envelope.Data.Items, nil
}

// TaskTemplate 是一个任务模板类型。
type TaskTemplate struct {
	Type     string `json:"type"`
	TypeName string `json:"typeName"`
}

// TaskRecommendations 读推荐任务。
//
// 实测响应：{"object":"list","batchId":null,"generatedAt":null,"data":[],"total":0}
func (c *Client) TaskRecommendations(ctx context.Context) ([]map[string]any, error) {
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/tasks/recommendations", nil, "tasks/recommendations", &envelope); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// ApplyRecommendation 采纳一条推荐任务（POST /v1/tasks/recommendations/{id}/apply）。
func (c *Client) ApplyRecommendation(ctx context.Context, recommendationID string) (map[string]any, error) {
	raw, err := c.postJSON(ctx, "/v1/tasks/recommendations/"+recommendationID+"/apply", nil,
		"tasks/recommendations/apply", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// FollowUpTask 给已完成任务追加一条消息并重新排队。
func (c *Client) FollowUpTask(ctx context.Context, taskID, text string) (map[string]any, error) {
	raw, err := c.postJSON(ctx, "/v1/tasks/"+taskID+"/follow-up", nil, "tasks/follow-up",
		map[string]any{"message": text})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// StopTask 终止一个排队/运行中的任务。
func (c *Client) StopTask(ctx context.Context, taskID string) error {
	_, err := c.postJSON(ctx, "/v1/tasks/"+taskID+"/stop", nil, "tasks/stop", map[string]any{})
	return err
}

// MarkTaskSeen 确认任务结果已读（传 unseenResultAt 原值）。
func (c *Client) MarkTaskSeen(ctx context.Context, taskID, unseenResultAt string) error {
	body := map[string]any{}
	if unseenResultAt != "" {
		body["unseenResultAt"] = unseenResultAt
	}
	_, err := c.postJSON(ctx, "/v1/tasks/"+taskID+"/seen", nil, "tasks/seen", body)
	return err
}

// TaskRunEvents 读任务运行事件流（GET /v1/tasks/{id}/run-events）。
func (c *Client) TaskRunEvents(ctx context.Context, taskID string) ([]map[string]any, error) {
	var envelope struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/tasks/"+taskID+"/run-events", nil, "tasks/run-events", &envelope); err != nil {
		return nil, err
	}
	return envelope.Data.Items, nil
}

func toTask(m map[string]any) Task {
	t := Task{Raw: m}
	if s, ok := m["id"].(string); ok {
		t.ID = s
	}
	if s, ok := m["type"].(string); ok {
		t.Type = s
	}
	if s, ok := m["title"].(string); ok {
		t.Title = s
	}
	if s, ok := m["status"].(string); ok {
		t.Status = s
	}
	if s, ok := m["createdAt"].(string); ok {
		t.CreatedAt = s
	}
	if s, ok := m["updatedAt"].(string); ok {
		t.UpdatedAt = s
	}
	return t
}

// statusIsOK 判断上游是否受理（growth 内部用）。
func statusIsOK(status int) bool {
	return status == http.StatusOK || status == http.StatusAccepted || status == http.StatusNoContent
}
