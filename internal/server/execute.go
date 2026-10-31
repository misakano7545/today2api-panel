package server

import (
	"context"
	"strings"
	"time"

	"github.com/misakano7545/today2api-panel/internal/today"
)

// Event 是从上游流出来的一个增量或终态。
type Event struct {
	Delta   string
	Err     error
	Done    bool
	Account string
	Model   string
}

// Start 发起一轮对话，返回增量通道。
//
// 选号 → 切模型 → 发送 → 轮询，全部在后台 goroutine 里做；
// 第一个增量到达之前出错会自动换号重试（下游还没看到任何字节）。
// 调用方负责 ctx 超时，并必须把通道读到关闭。
func (s *Server) Start(ctx context.Context, modelID, prompt string) <-chan Event {
	ch := make(chan Event, 4)
	go func() {
		defer close(ch)
		cfg := s.store.Snapshot()
		deadline := time.Duration(cfg.ReplyTimeoutSec) * time.Second
		workCtx, cancel := context.WithTimeout(ctx, deadline)
		defer cancel()

		optionID := today.OptionID(modelID)
		skip := map[string]bool{}
		var lastErr error

		for attempt := 0; attempt < len(cfg.Accounts)+1; attempt++ {
			acct, client, errPick := s.pool.Pick(cfg.Accounts, cfg, skip)
			if errPick != nil {
				break
			}
			skip[acct.ID] = true

			if errModel := client.EnsureModel(workCtx, optionID); errModel != nil {
				lastErr = errModel
				s.pool.Report(acct.ID, errModel, modelID)
				s.logs.add(Entry{Time: time.Now(), Path: "upstream:model", Model: modelID, Account: acct.Label, Status: 502, Error: errModel.Error()})
				if today.KindOf(errModel) == today.KindNetwork && ctx.Err() != nil {
					break
				}
				continue
			}
			threadID, errSend := client.Send(workCtx, prompt)
			if errSend != nil {
				lastErr = errSend
				s.pool.Report(acct.ID, errSend, modelID)
				s.logs.add(Entry{Time: time.Now(), Path: "upstream:send", Model: modelID, Account: acct.Label, Status: 502, Error: errSend.Error()})
				continue
			}

			emitted := false
			_, errReply := client.Reply(workCtx, threadID, func(delta string) bool {
				emitted = true
				select {
				case ch <- Event{Delta: delta, Account: acct.Label, Model: modelID}:
					return true
				case <-ctx.Done():
					return false
				}
			})
			if errReply != nil && !emitted {
				lastErr = errReply
				s.pool.Report(acct.ID, errReply, modelID)
				s.logs.add(Entry{Time: time.Now(), Path: "upstream:reply", Model: modelID, Account: acct.Label, Status: 502, Error: errReply.Error()})
				continue
			}
			if errReply != nil {
				// 已经吐过正文：不再换号（换号会让下游看到两段不同的回答）。
				s.pool.Report(acct.ID, errReply, modelID)
				ch <- Event{Err: errReply, Done: true, Account: acct.Label, Model: modelID}
				return
			}
			s.pool.Report(acct.ID, nil, modelID)
			ch <- Event{Done: true, Account: acct.Label, Model: modelID}
			return
		}

		if lastErr == nil {
			lastErr = errNoAccountError()
		}
		ch <- Event{Err: lastErr, Done: true, Model: modelID}
	}()
	return ch
}

// collect 把通道读成完整文本（非流式用）。
func collect(ch <-chan Event) (string, string, int, error) {
	var buf strings.Builder
	account := ""
	chars := 0
	for ev := range ch {
		if ev.Delta != "" {
			buf.WriteString(ev.Delta)
			chars += len([]rune(ev.Delta))
		}
		if ev.Account != "" {
			account = ev.Account
		}
		if ev.Err != nil {
			return buf.String(), account, chars, ev.Err
		}
	}
	return buf.String(), account, chars, nil
}

func errNoAccountError() error {
	return errNoAccountValue
}

var errNoAccountValue = &noAccountErr{}

type noAccountErr struct{}

func (*noAccountErr) Error() string {
	return "no available today account (all disabled, cooling or skipped)"
}
