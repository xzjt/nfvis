package cli

// M3-9：monitor 命令识别与刷新间隔解析（本地轮询，附录 A #36）。

import (
	"testing"
	"time"
)

func TestMonitorSpec(t *testing.T) {
	cases := []struct {
		line     string
		wantCmd  string
		interval time.Duration
		ok       bool
	}{
		{"monitor interfaces ens192", "monitor interfaces ens192", time.Second, true},
		{"monitor interfaces ens192 interval 2", "monitor interfaces ens192", 2 * time.Second, true},
		{"mon int ens192 interval 5", "mon int ens192", 5 * time.Second, true}, // 无歧义缩写
		{"monitor vnf fw1", "", 0, false},                                      // 第二个 token 非 interfaces
		{"monitor interfaces", "", 0, false},                                   // 缺接口名
		{"monitor interfaces ens192 interval 0", "", 0, false},
		{"monitor interfaces ens192 interval abc", "", 0, false},
		{"monitor interfaces ens192 x 2", "", 0, false},
		{"show interfaces", "", 0, false},
		{"", "", 0, false},
	}
	for _, c := range cases {
		cmd, interval, ok := monitorSpec(c.line)
		if ok != c.ok || cmd != c.wantCmd || interval != c.interval {
			t.Errorf("monitorSpec(%q) = (%q, %v, %v)，期望 (%q, %v, %v)",
				c.line, cmd, interval, ok, c.wantCmd, c.interval, c.ok)
		}
	}
}

func TestPrefixOf(t *testing.T) {
	if !prefixOf("mon", "monitor") || !prefixOf("monitor", "monitor") {
		t.Fatal("合法前缀应命中")
	}
	if prefixOf("mo", "monitor") || prefixOf("monitors", "monitor") || prefixOf("xyz", "monitor") {
		t.Fatal("过短/过长/不匹配不应命中")
	}
}
