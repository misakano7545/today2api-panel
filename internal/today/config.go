package today

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// ClientConfig 是 GET /v1/config 的响应。
//
// 实测形状（2026-11，国际版 free 账号）：
//
//	{"defaultAvatars":[…],"characters":[…],…
//	 "messageCapabilities":{"messageProtocolVersion":"canonical-v1",
//	   "authSubject":"…","mainConversationId":"…","supportsTypedBody":true,
//	   "messageReactionsVersion":"v1","supportsSequencePull":true,
//	   "supportsLongPoll":true,"longPollWaitMaxMs":25000,
//	   "supportsAtomicCardContinuation":false},
//	 "fileCapabilities":{…}}
//
// 注意：这是**站点公开配置**（头像 / 角色 / 输入框占位符都在里面），
// mainConversationId 嵌在 messageCapabilities 里，不在顶层。
type ClientConfig struct {
	MessageCapabilities MessageCapabilities `json:"messageCapabilities"`
	FileCapabilities    map[string]any      `json:"fileCapabilities"`
}

// MessageCapabilities 是 canonical-v1 的能力协商段。
type MessageCapabilities struct {
	MessageProtocolVersion         string `json:"messageProtocolVersion"`
	AuthSubject                    string `json:"authSubject"`
	MainConversationID             string `json:"mainConversationId"`
	SupportsTypedBody              bool   `json:"supportsTypedBody"`
	MessageReactionsVersion        string `json:"messageReactionsVersion"`
	SupportsSequencePull           bool   `json:"supportsSequencePull"`
	SupportsLongPoll               bool   `json:"supportsLongPoll"`
	LongPollWaitMaxMs              int    `json:"longPollWaitMaxMs"`
	SupportsAtomicCardContinuation bool   `json:"supportsAtomicCardContinuation"`
}

// configTTL 是能力协商结果的缓存时长——协议很少变，但账号切换要能生效。
const configTTL = 10 * time.Minute

// Config 读 /v1/config（带缓存）。失败时返回保守默认值，不阻塞对话。
func (c *Client) Config(ctx context.Context) *ClientConfig {
	c.mu.Lock()
	if c.cfg != nil && time.Since(c.cfgAt) < configTTL {
		cfg := *c.cfg
		c.mu.Unlock()
		return &cfg
	}
	c.mu.Unlock()

	var out ClientConfig
	if err := c.getJSON(ctx, "/v1/config", nil, "config", &out); err != nil {
		return &ClientConfig{MessageCapabilities: MessageCapabilities{
			MessageProtocolVersion: "canonical-v1",
			MainConversationID:     "main",
			SupportsTypedBody:      true,
		}}
	}
	if out.MessageCapabilities.MainConversationID == "" {
		out.MessageCapabilities.MainConversationID = "main"
	}
	c.mu.Lock()
	c.cfg = &out
	c.cfgAt = time.Now()
	c.mu.Unlock()
	copied := out
	return &copied
}

// History 读一页历史消息，oldest → newest。
func (c *Client) History(ctx context.Context, conversationID string, limit int) ([]CanonicalMessage, error) {
	if limit <= 0 {
		limit = 20
	}
	query := url.Values{}
	query.Set("conversationId", conversationID)
	query.Set("limit", strconv.Itoa(limit))
	var page struct {
		Data struct {
			Items []CanonicalMessage `json:"items"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/conversations/messages", query, "conversations/messages", &page); err != nil {
		return nil, err
	}
	return page.Data.Items, nil
}

// CanonicalMessage 是 canonical-v1 消息体，只保留网关用得上的字段。
type CanonicalMessage struct {
	ID                    string `json:"id"`
	ConversationID        string `json:"conversationId"`
	ThreadID              string `json:"threadId"`
	Role                  string `json:"role"`
	State                 string `json:"state"`
	IsFinal               bool   `json:"isFinal"`
	ClientMessageID       string `json:"clientMessageId"`
	OriginMessageID       string `json:"originMessageId"`
	OriginClientMessageID string `json:"originClientMessageId"`
	CreatedAt             string `json:"createdAt"`
	UpdatedAt             string `json:"updatedAt"`
	Body                  struct {
		Parts []MessagePart `json:"parts"`
	} `json:"body"`
}

// MessagePart 是消息体的一段。正文走 messageType=text 的 content.text，
// 流式累积段走 messageType=turn 的 content.text[]。
type MessagePart struct {
	MessageType string          `json:"messageType"`
	Content     json.RawMessage `json:"content"`
	Type        string          `json:"type"`
	Text        string          `json:"text"`
}

// Text 把消息正文拼成纯文本。
//
// 卡片（qa_* / attachment / avatar_candidates）和工具调用对网关没有文本价值，跳过。
func (m CanonicalMessage) Text() string {
	var out []byte
	for _, p := range m.Body.Parts {
		switch p.MessageType {
		case "text", "":
			if s := partText(p.Content); s != "" {
				out = append(out, s...)
			} else if p.Type == "text" {
				out = append(out, p.Text...)
			}
		case "turn":
			out = append(out, turnText(p.Content)...)
		}
	}
	return string(out)
}

// Done 表示这条消息不会再产生增量。
func (m CanonicalMessage) Done() bool {
	return m.IsFinal || m.State == "completed" || m.State == "interrupted"
}

func partText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Text != "" {
		return obj.Text
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func turnText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Text []string `json:"text"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var buf []byte
	for _, s := range obj.Text {
		buf = append(buf, s...)
	}
	return string(buf)
}
