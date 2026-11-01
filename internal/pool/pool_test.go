package pool

import (
	"testing"
	"time"

	"github.com/misakano7545/today2api-panel/internal/config"
	"github.com/misakano7545/today2api-panel/internal/today"
)

func testConfig(ids ...string) ([]config.Account, config.Config) {
	cfg := config.Defaults()
	var accounts []config.Account
	for _, id := range ids {
		accounts = append(accounts, config.Account{ID: id, Label: id, Cookie: "c=" + id, Enabled: true})
	}
	cfg.Accounts = accounts
	return accounts, cfg
}

func TestPickRotates(t *testing.T) {
	accounts, cfg := testConfig("a", "b")
	p := New()
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		acct, _, err := p.Pick(accounts, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[acct.ID] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("轮转没有覆盖两个账号：%v", seen)
	}
}

func TestCooldownExcludesAccount(t *testing.T) {
	accounts, cfg := testConfig("a", "b")
	p := New()
	p.SetCooldown(today.KindAuth, time.Hour)
	p.Report("a", &today.Error{Kind: today.KindAuth, Msg: "401"}, "")

	for i := 0; i < 4; i++ {
		acct, _, err := p.Pick(accounts, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if acct.ID != "b" {
			t.Fatalf("冷却中的账号仍被选中：%s", acct.ID)
		}
	}
	p.Report("a", nil, "")
	if acct, _, err := p.Pick(accounts, cfg, map[string]bool{"b": true}); err != nil || acct.ID != "a" {
		t.Fatalf("成功后应恢复：%v %v", acct, err)
	}
}

func TestPickSkipsDisabledAndExcluded(t *testing.T) {
	accounts, cfg := testConfig("a", "b")
	accounts[0].Enabled = false
	p := New()
	if acct, _, err := p.Pick(accounts, cfg, nil); err != nil || acct.ID != "b" {
		t.Fatalf("应跳过停用账号：%v %v", acct, err)
	}
	if _, _, err := p.Pick(accounts, cfg, map[string]bool{"b": true}); err == nil {
		t.Fatal("全部跳过时应报错")
	}
}
