package system

// FR-SEC-001（决策 #72）：管理面仅监听管理网卡——监听地址推导。

import (
	"strings"
	"testing"
)

func TestResolveListenAddr(t *testing.T) {
	allLocal := func(string) bool { return true }
	noneLocal := func(string) bool { return false }

	cases := []struct {
		name     string
		listen   string
		mgmt     string
		isLocal  func(string) bool
		wantAddr string
		wantNote string // 子串；空表示期望无 note
	}{
		{
			"通配 + 管理口 IPv4 → 收敛到管理口地址",
			":443", "192.168.1.10/24", allLocal, "192.168.1.10:443", "已收敛为管理口地址",
		},
		{
			"显式 0.0.0.0 + 管理口 → 同样收敛",
			"0.0.0.0:18443", "192.168.1.10/24", allLocal, "192.168.1.10:18443", "已收敛",
		},
		{
			"管理口 IPv6 → 正确加方括号",
			":443", "2001:db8::1/64", allLocal, "[2001:db8::1]:443", "已收敛",
		},
		{
			"管理口地址未配置在本机 → 不收敛（仅提示）",
			":443", "192.168.1.10/24", noneLocal, ":443", "未配置在本机",
		},
		{
			"显式指定了别的地址 → 保留并告警",
			"127.0.0.1:443", "192.168.1.10/24", allLocal, "127.0.0.1:443", "不一致",
		},
		{
			"显式就是管理口地址 → 原样且无告警",
			"192.168.1.10:443", "192.168.1.10/24", allLocal, "192.168.1.10:443", "",
		},
		{
			"未配置管理口 → 保持既有行为（无 note）",
			":443", "", allLocal, ":443", "",
		},
		{
			"管理口值为裸 IP（无掩码） → 同样可用",
			":443", "10.0.0.5", allLocal, "10.0.0.5:443", "已收敛",
		},
		{
			"监听地址无端口 → 原样返回",
			"localhost", "192.168.1.10/24", allLocal, "localhost", "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, note := ResolveListenAddr(c.listen, c.mgmt, c.isLocal)
			if addr != c.wantAddr {
				t.Fatalf("addr = %q，期望 %q", addr, c.wantAddr)
			}
			if c.wantNote == "" {
				if note != "" {
					t.Fatalf("不应有 note，得到 %q", note)
				}
				return
			}
			if !strings.Contains(note, c.wantNote) {
				t.Fatalf("note = %q，期望含 %q", note, c.wantNote)
			}
		})
	}
}

func TestMgmtAddrIP(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.10/24": "192.168.1.10",
		"10.0.0.5":        "10.0.0.5",
		"2001:db8::1/64":  "2001:db8::1",
		"":                "",
		"不是地址":            "",
	} {
		if got := mgmtAddrIP(in); got != want {
			t.Fatalf("mgmtAddrIP(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestListenSANs(t *testing.T) {
	cases := []struct {
		listen string
		want   []string
	}{
		{"127.0.0.1:18443", []string{"127.0.0.1", "::1", "127.0.0.1"}},
		{":443", []string{"127.0.0.1", "::1"}},
		{"192.168.1.10:443", []string{"127.0.0.1", "::1", "192.168.1.10"}},
		{"[2001:db8::1]:443", []string{"127.0.0.1", "::1", "2001:db8::1"}},
		{"无端口", []string{"127.0.0.1", "::1"}},
	}
	for _, c := range cases {
		got := ListenSANs(c.listen)
		if len(got) != len(c.want) {
			t.Fatalf("ListenSANs(%q) = %v，期望 %v", c.listen, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("ListenSANs(%q) = %v，期望 %v", c.listen, got, c.want)
			}
		}
	}
}
