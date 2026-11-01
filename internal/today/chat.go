package today

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Turn 是一次用户消息的凭据：拿它去轮询对应的助手回复。
type Turn struct {
	ConversationID  string
	ThreadID        string
	ClientMessageID string
	UserMessageID   string
	SentAt          time.Time
}

// Send 发一条用户消息（canonical-v1），返回轮询凭据。
//
// 请求体按客户端 appendCanonicalMessageRequestSchema 拼：
//
//	{"body":{"parts":[{"id":…,"type":"text","text":…}]},
//	 "clientMessageId":…,"isMainChat":true}
//
// 主对话用 isMainChat:true（不带 conversationId）；指定会话时用
// isMainChat:false + conversationId。
//
// 实测响应（国内版 pro 账号，2026-11）：
//
//	HTTP 202
//	{"threadId":"01a19522-ce0e-7115-8572-f1406b3ce073",
//	 "messageId":"01a19522-ce0e-7990-af2b-477684670f13",
//	 "clientMessageId":"…","createdAt":"…","isDuplicate":false,
//	 "canonicalMessage":{"id":"…","conversationId":"…","threadId":"…",
//	   "cursor":"…","role":"user","state":"completed","isFinal":true,
//	   "clientMessageId":"…","originClientMessageId":"…",
//	   "originTriggerType":"asst_response","body":{"parts":[…]}}}
//
// 即：顶层的 threadId / messageId 与 canonicalMessage 同时存在——
// 早期仓库只取 {threadId,messageId} 是对的，后来又只认 canonicalMessage
// 也不完整，两者都要。
func (c *Client) Send(ctx context.Context, conversationID, text string) (*Turn, error) {
	if conversationID == "" {
		conversationID = c.Config(ctx).MessageCapabilities.MainConversationID
	}
	clientMessageID := newUUID()
	partID := newUUID()

	body := map[string]any{
		"body": map[string]any{
			"parts": []any{map[string]any{
				"id":   partID,
				"type": "text",
				"text": text,
			}},
		},
		"clientMessageId": clientMessageID,
	}
	if conversationID == "" || conversationID == "main" {
		body["isMainChat"] = true
		conversationID = "main"
	} else {
		body["isMainChat"] = false
		body["conversationId"] = conversationID
	}

	raw, err := c.postJSON(ctx, "/v1/messages", nil, "messages", body)
	if err != nil {
		return nil, err
	}
	turn := &Turn{ConversationID: conversationID, ClientMessageID: clientMessageID, SentAt: time.Now()}

	var reply struct {
		ThreadID    string `json:"threadId"`
		MessageID   string `json:"messageId"`
		IsDuplicate bool   `json:"isDuplicate"`
		// 老形状：{canonicalMessage:{…}}
		CanonicalMessage *CanonicalMessage `json:"canonicalMessage"`
		// 信封形状：{data:{canonicalMessage:{…}}}
		Data *struct {
			CanonicalMessage *CanonicalMessage `json:"canonicalMessage"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &reply); errUnmarshal == nil {
		if reply.ThreadID != "" {
			turn.ThreadID = reply.ThreadID
		}
		msg := reply.CanonicalMessage
		if msg == nil && reply.Data != nil {
			msg = reply.Data.CanonicalMessage
		}
		if msg != nil {
			turn.UserMessageID = msg.ID
			if msg.ThreadID != "" {
				turn.ThreadID = msg.ThreadID
			}
			if msg.ConversationID != "" && msg.ConversationID != "main" {
				turn.ConversationID = msg.ConversationID
			}
		} else if reply.MessageID != "" {
			turn.UserMessageID = reply.MessageID
		}
	}
	return turn, nil
}

// Reply 轮询到本轮助手消息完成。
//
// onDelta 在正文增长时被调用；返回 false 表示下游已断开，停止轮询。
// 返回的字符串是完整正文（终稿）。
func (c *Client) Reply(ctx context.Context, turn *Turn, onDelta func(string) bool) (string, error) {
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			if last != "" {
				return last, nil
			}
			return "", &Error{Kind: KindNetwork, Msg: "reply timeout: " + err.Error()}
		}
		items, errHistory := c.History(ctx, turn.ConversationID, 20)
		if errHistory != nil {
			if ctx.Err() != nil && last != "" {
				return last, nil
			}
			return "", errHistory
		}
		if msg, ok := pickReply(items, turn); ok {
			text := msg.Text()
			switch {
			case len(text) > len(last) && strings.HasPrefix(text, last):
				if onDelta != nil && !onDelta(text[len(last):]) {
					return text, nil
				}
			case !strings.HasPrefix(text, last):
				// 正文被替换（不是追加）：整段重吐，避免下游拼接出错。
				if onDelta != nil && !onDelta(text) {
					return text, nil
				}
			}
			last = text
			if msg.Done() {
				return text, nil
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(c.Interval):
		}
	}
}

// pickReply 在历史页里挑出本轮助手消息。
//
// 归属判定按客户端语义：assistant 消息的 originClientMessageId 命中本轮
// clientMessageId 即为本轮回复。没有归属字段的老上游退化为
// “发送之后出现的最后一条 assistant”。
func pickReply(items []CanonicalMessage, turn *Turn) (CanonicalMessage, bool) {
	var fallback *CanonicalMessage
	for i := range items {
		m := items[i]
		if m.Role != "assistant" {
			continue
		}
		if m.OriginClientMessageID != "" && m.OriginClientMessageID == turn.ClientMessageID {
			return m, true
		}
		if m.ClientMessageID != "" && m.ClientMessageID == turn.ClientMessageID {
			return m, true
		}
		if m.OriginClientMessageID == "" && !createdBefore(m, turn.SentAt) {
			fallback = &m
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return CanonicalMessage{}, false
}

func createdBefore(m CanonicalMessage, t time.Time) bool {
	if m.CreatedAt == "" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return false
	}
	return ts.Before(t.Add(-2 * time.Second))
}
