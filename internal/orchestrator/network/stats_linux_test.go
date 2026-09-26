//go:build linux

package network

// stats_linux.go 的接线单测（R84-3）：取数路径确实经连接缓存走「失效→重连→重试」。
// 假 connect 不碰真 VPP（返回的句柄在读取函数里不被使用），故可在 CI（Linux）跑；
// 策略本身（含并发、重连失败）在 vpp_govpp_test.go 里跨平台覆盖。

import (
	"errors"
	"testing"

	"go.fd.io/govpp/core"
)

// TestStatsReadReconnectsStaleConn：第一次读取失败（陈旧连接）→ 丢弃重连 → 重试成功。
func TestStatsReadReconnectsStaleConn(t *testing.T) {
	m := &Manager{cfg: Config{Socket: "/tmp/nfvis-test-absent/api.sock"}}
	// 先让 statsOnce 落地，避免 statsConnRef 用真实 connect 覆盖下面注入的假连接
	m.statsOnce.Do(func() {})
	connects, closed, reads := 0, 0, 0
	m.statsConn = newConnCache(func() (*core.StatsConnection, error) {
		connects++
		return nil, nil
	}, func(*core.StatsConnection) { closed++ })

	err := m.statsRead(func(*core.StatsConnection) error {
		reads++
		if reads == 1 {
			return errors.New("stats segment 已失效")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("重连后重试应成功: %v", err)
	}
	if connects != 2 || closed != 1 || reads != 2 {
		t.Errorf("应失效重连一次并重试：connect=%d close=%d read=%d（期望 2/1/2）", connects, closed, reads)
	}
}

// TestStatsReadReconnectFailure：重连建不起来时如实报错，且下次调用仍会重新尝试。
func TestStatsReadReconnectFailure(t *testing.T) {
	m := &Manager{cfg: Config{Socket: "/tmp/nfvis-test-absent/api.sock"}}
	m.statsOnce.Do(func() {})
	connects := 0
	m.statsConn = newConnCache(func() (*core.StatsConnection, error) {
		connects++
		if connects > 1 {
			return nil, errors.New("连接 stats segment 失败")
		}
		return nil, nil
	}, func(*core.StatsConnection) {})

	read := func(*core.StatsConnection) error { return errors.New("取数失败") }
	if err := m.statsRead(read); err == nil {
		t.Fatal("重连失败应报错")
	}
	if err := m.statsRead(read); err == nil {
		t.Fatal("后续调用仍应报错")
	}
	if connects != 3 {
		t.Errorf("每次取数都应重新尝试建立连接，实际 %d 次", connects)
	}
}
