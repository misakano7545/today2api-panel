package config

import "testing"

// 从文件加载的配置，裸 session 值也必须被归一成完整 Cookie 头。
func TestNormalizeWrapsBareCookie(t *testing.T) {
	c := Config{Accounts: []Account{
		{ID: "a", Cookie: "  ABCdef123  "},
		{ID: "b", Cookie: "__Secure-better-auth.session_token=XYZ.abc%3D"},
		{ID: "c", Cookie: ""},
	}}
	c.Normalize()
	if got, want := c.Accounts[0].Cookie, "__Secure-better-auth.session_token=ABCdef123"; got != want {
		t.Errorf("裸值未归一: got %q want %q", got, want)
	}
	if got, want := c.Accounts[1].Cookie, "__Secure-better-auth.session_token=XYZ.abc%3D"; got != want {
		t.Errorf("整段头被改动: got %q want %q", got, want)
	}
	if c.Accounts[2].Cookie != "" {
		t.Errorf("空 Cookie 应保持空，得到 %q", c.Accounts[2].Cookie)
	}
}
