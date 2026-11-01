package today

import (
	"context"
	"encoding/json"
	"net/http"
)

// PricingStatus 是 GET /v1/pricing/status 里网关用得上的部分。
type PricingStatus struct {
	Plan struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"plan"`
	ModelOptions struct {
		SelectedOptionID string `json:"selectedOptionId"`
		AvailableOptions []struct {
			OptionID    string `json:"optionId"`
			DisplayName string `json:"displayName"`
			ModelID     string `json:"modelId"`
		} `json:"availableOptions"`
	} `json:"modelOptions"`
}

// Pricing 拉当前套餐与模型选项（客户端 PUT 前会先读它做校验）。
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

// LLMModels 读网关模型列表（GET /v1/llm/models）。
func (c *Client) LLMModels(ctx context.Context) ([]string, error) {
	var envelope struct {
		Data struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/llm/models", nil, "llm/models", &envelope); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(envelope.Data.Items))
	for _, it := range envelope.Data.Items {
		if it.ID != "" {
			out = append(out, it.ID)
		}
	}
	return out, nil
}

// EnsureModel 把账号切到指定模型选项；已经是该选项时直接返回。
//
// 上游模型选项是“账号全局”的（PUT /v1/pricing/model-option），
// 不是每条消息带参数，所以并发切模型会互相踩——调用方需要串行化。
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
	// 上游成功时会顺手回一份目录；把 selectedOptionId 记下来做本地去重。
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

// OptionID 把对外模型名转成上游 optionId。
//
// 对外暴露 `today-deepseek-v4.1-flash`，上游要 `model.deepseek-v4.1-flash`；
// 已经是完整 optionId 的原样透传。
func OptionID(model string) string {
	m := trimPrefixFold(model, "today-")
	if m == "" {
		return ""
	}
	if len(m) > 6 && m[:6] == "model." {
		return m
	}
	return "model." + m
}

func trimPrefixFold(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}
