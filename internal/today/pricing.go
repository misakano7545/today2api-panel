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

// EnsureModel 把账号切到指定模式；已经是该模式时直接返回。
//
// 上游的模式选择是「账号全局」的（PUT /v1/pricing/model-option），
// 不是每条消息带参数，所以并发切模式会互相踩——调用方需要串行化。
func (c *Client) EnsureModel(ctx context.Context, optionID string) error {
	if optionID == "" {
		return nil
	}
	c.mu.Lock()
	current := c.option
	c.mu.Unlock()
	if current == optionID {
		return nil
	}

	status, raw, err := c.putJSON(ctx, "/v1/pricing/model-option", "pricing/model-option",
		map[string]any{"optionId": optionID})
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
	selected := optionID
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

// OptionID 把对外模型名转成上游的选项 id。
//
// 上游有两套名字：
//   - 模式（可 PUT 切换）：mode.balanced / mode.power
//   - 模型 id（只读清单）：claude-opus-5 / gpt-5.5 …
//
// 对外暴露 `today-` 前缀，去掉后若本来就是 mode.* 或 model.* 则原样透传；
// 其余按模式名处理（balanced → mode.balanced）。
func OptionID(model string) string {
	m := strings.TrimSpace(model)
	m = strings.TrimPrefix(m, "today-")
	if m == "" {
		return ""
	}
	if strings.HasPrefix(m, "mode.") || strings.HasPrefix(m, "model.") {
		return m
	}
	return "mode." + m
}
