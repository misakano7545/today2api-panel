package today

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// PricingStatus 是 GET /v1/pricing/status 的 data.status。
//
// 实测（2026-11，free 账号）：
//
//	{"success":true,"code":"common.ok","message":"OK","data":{"status":{
//	  "schemaVersion":1,"plan":"free","subscriptionState":"none",
//	  "accessPhase":"none","accessSource":"none","serviceState":"normal",
//	  "trialEndsAt":null,"currentPeriodEndAt":null,"graceEndsAt":null,
//	  "display":{"title":"Free","subtitle":"…","cta":{…}},
//	  "usage":null,
//	  "modelOptions":{"schemaVersion":1,"display":{"title":"Mode",…},
//	    "selectedOptionId":"mode.balanced",
//	    "options":[{"id":"mode.balanced","disabled":false,
//	                "display":{"title":"Balanced","description":"…","availabilityLabel":"…"}},
//	               {"id":"mode.power","disabled":true,"deepLink":"today://plan?…"}]}}}}
//
// 两个要点：
//  1. plan 是字符串，不是对象；
//  2. 可切的选项是「模式」（mode.balanced / mode.power），不是具体模型 id。
//     具体模型清单在 GET /v1/llm/models。
type PricingStatus struct {
	SchemaVersion     int    `json:"schemaVersion"`
	Plan              string `json:"plan"`
	SubscriptionState string `json:"subscriptionState"`
	AccessPhase       string `json:"accessPhase"`
	ServiceState      string `json:"serviceState"`
	Display           struct {
		Title    string `json:"title"`
		Subtitle string `json:"subtitle"`
	} `json:"display"`
	ModelOptions struct {
		SelectedOptionID string          `json:"selectedOptionId"`
		Options          []PricingOption `json:"options"`
	} `json:"modelOptions"`
}

// PricingOption 是一个可选模式。
type PricingOption struct {
	ID       string `json:"id"`
	Disabled bool   `json:"disabled"`
	DeepLink string `json:"deepLink"`
	Display  struct {
		Title             string `json:"title"`
		Description       string `json:"description"`
		AvailabilityLabel string `json:"availabilityLabel"`
	} `json:"display"`
}

// Pricing 拉当前套餐与模式选项。
func (c *Client) Pricing(ctx context.Context) (*PricingStatus, error) {
	var envelope struct {
		Data struct {
			Status PricingStatus `json:"status"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/pricing/status", nil, "pricing/status", &envelope); err != nil {
		return nil, err
	}
	return &envelope.Data.Status, nil
}

// LLMModels 读可用模型清单。
//
// 实测响应：{"models":[{"modelId":"claude-haiku-4-5"},{"modelId":"gpt-5.5"},…]}
// 注意不是 {"data":{"items":[{"id":…}]}}。
func (c *Client) LLMModels(ctx context.Context) ([]string, error) {
	var envelope struct {
		Models []struct {
			ModelID string `json:"modelId"`
		} `json:"models"`
	}
	if err := c.getJSON(ctx, "/v1/llm/models", nil, "llm/models", &envelope); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(envelope.Models))
	for _, m := range envelope.Models {
		if m.ModelID != "" {
			out = append(out, m.ModelID)
		}
	}
	return out, nil
}

// EnsureModel 把账号切到指定模型/模式；已经是目标值时直接返回。
//
// 实测两个区域的选项体系**不一样**：
//
//	国内版 pro: model.deepseek-v4.1-flash / model.minimax-m31-flash / model.glm-5.3
//	国际版 free: mode.balanced / mode.power
//
// 所以不能靠拼前缀猜 id，必须拿账号自己的 available options 去匹配；
// 匹配不到时保持当前选择（不能因此让整轮请求失败）。
//
// 另外：对已选中的选项再 PUT 会被拒（403 auth.forbidden），
// 所以先读 pricing/status 对齐，只有真不一样才 PUT。
func (c *Client) EnsureModel(ctx context.Context, requested string) error {
	if requested == "" {
		return nil
	}
	want := normalizeOptionName(requested)

	st, err := c.Pricing(ctx)
	if err != nil {
		// 读不到目录就保守处理：不动账号设置，直接用当前选择。
		return nil
	}
	selected := st.ModelOptions.SelectedOptionID
	c.mu.Lock()
	c.option = selected
	c.mu.Unlock()
	if normalizeOptionName(selected) == want {
		return nil
	}

	target := ""
	for _, opt := range st.ModelOptions.Options {
		if normalizeOptionName(opt.ID) == want {
			target = opt.ID
			break
		}
	}
	if target == "" {
		// 该账号没有这个选项：不切，继续用当前选择。
		return nil
	}

	status, raw, err := c.putJSON(ctx, "/v1/pricing/model-option", "pricing/model-option",
		map[string]any{"optionId": target})
	if err != nil {
		return err
	}
	var echoed struct {
		Data struct {
			ModelOptions struct {
				SelectedOptionID string `json:"selectedOptionId"`
			} `json:"modelOptions"`
		} `json:"data"`
	}
	selected = target
	if status == http.StatusOK && len(raw) > 0 && json.Unmarshal(raw, &echoed) == nil {
		if echoed.Data.ModelOptions.SelectedOptionID != "" {
			selected = echoed.Data.ModelOptions.SelectedOptionID
		}
	}
	c.mu.Lock()
	c.option = selected
	c.mu.Unlock()
	return nil
}

// OptionID 把对外模型名归一成上游选项名（不做前缀猜测）。
//
// 两个区域的选项体系不同（model.* vs mode.*），真正的 id 必须从
// 账号的 pricing/status 里取；这里只负责把对外的 `today-` 前缀去掉。
func OptionID(model string) string {
	m := strings.TrimSpace(model)
	m = strings.TrimPrefix(m, "today-")
	return m
}

// normalizeOptionName 抽出可比对的核心名：
// `model.deepseek-v4.1-flash` / `today-deepseek-v4.1-flash` / `deepseek-v4.1-flash`
// 都归一成 `deepseek-v4.1-flash`。
func normalizeOptionName(name string) string {
	n := strings.TrimSpace(strings.ToLower(name))
	n = strings.TrimPrefix(n, "today-")
	for _, prefix := range []string{"model.", "mode."} {
		if strings.HasPrefix(n, prefix) {
			n = strings.TrimPrefix(n, prefix)
			break
		}
	}
	return n
}
