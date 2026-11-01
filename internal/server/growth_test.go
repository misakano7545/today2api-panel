package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/today2api-panel/internal/config"
)

// 签到与任务的面板 API 行为，对齐 today/growth.go 的上游语义。

func TestPanelCheckinReportsActions(t *testing.T) {
	up, mock := newMockToday(t)
	s := newTestServer(t, up.URL, func(c *config.Config) {
		c.Accounts = []config.Account{
			{ID: "acc_1", Label: "main", Realm: "cn", Cookie: "__Secure-better-auth.session_token=fine", Enabled: true},
		}
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/checkin", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Result struct {
			Delta    int      `json:"delta"`
			Reported []string `json:"reported"`
			Before   struct {
				Points int `json:"points"`
			} `json:"before"`
			After struct {
				Points int `json:"points"`
			} `json:"after"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// mock 第一次读 40 分，第二次读 90 分。
	if out.Result.Delta != 50 {
		t.Fatalf("delta = %d, want 50", out.Result.Delta)
	}
	if len(out.Result.Reported) != 2 {
		t.Fatalf("reported = %v（两条客户端可申报动作都要打）", out.Result.Reported)
	}
	joined := strings.Join(mock.reported, ",")
	if !strings.Contains(joined, "notification-enabled") || !strings.Contains(joined, "discord-joined") {
		t.Fatalf("upstream 收到的动作 = %v", mock.reported)
	}
}

func TestPanelCheckinAllSkipsDisabled(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, func(c *config.Config) {
		c.Accounts = []config.Account{
			{ID: "acc_on", Label: "on", Realm: "cn", Cookie: "__Secure-better-auth.session_token=fine", Enabled: true},
			{ID: "acc_off", Label: "off", Realm: "cn", Cookie: "__Secure-better-auth.session_token=fine", Enabled: false},
		}
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/checkin", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK   int `json:"ok"`
		Rows []struct {
			Account string `json:"account"`
			Status  string `json:"status"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 1 || out.Rows[0].Account != "acc_on" {
		t.Fatalf("rows = %+v（禁用账号不应参与签到）", out.Rows)
	}
	if out.Rows[0].Status != "ok" {
		t.Fatalf("status = %q", out.Rows[0].Status)
	}
}

func TestPanelTasksList(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, func(c *config.Config) {
		c.Accounts = []config.Account{
			{ID: "acc_1", Label: "main", Realm: "cn", Cookie: "__Secure-better-auth.session_token=fine", Enabled: true},
		}
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodGet, "/api/panel/accounts/acc_1/tasks", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tasks []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"tasks"`
		Templates []string `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].ID != "task-1" || out.Tasks[0].Status != "running" {
		t.Fatalf("tasks = %+v", out.Tasks)
	}
	if len(out.Templates) != 2 {
		t.Fatalf("templates = %+v", out.Templates)
	}
}

func TestPanelTaskStop(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, func(c *config.Config) {
		c.Accounts = []config.Account{
			{ID: "acc_1", Label: "main", Realm: "cn", Cookie: "__Secure-better-auth.session_token=fine", Enabled: true},
		}
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodPost, "/api/panel/accounts/acc_1/tasks/task-1/stop", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPanelRapportUnknownAccount(t *testing.T) {
	up, _ := newMockToday(t)
	s := newTestServer(t, up.URL, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req(http.MethodGet, "/api/panel/accounts/nope/rapport", nil))
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
