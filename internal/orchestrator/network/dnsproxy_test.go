package network

// 数据面 DNS 代理（决策 #338）数据面编排单测：非空 → name-server + enable、清空 → del 全部 +
// disable、幂等、登记重置（恢复重放口径）。

import (
	"context"
	"errors"
	"testing"
)

type dnsCall struct {
	server string
	add    bool
}

type fakeDNS struct {
	calls   []dnsCall
	enabled []bool
	err     error
}

func (f *fakeDNS) NameServerAddDel(server string, add bool) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, dnsCall{server: server, add: add})
	return nil
}

func (f *fakeDNS) EnableDisable(enable bool) error {
	if f.err != nil {
		return f.err
	}
	f.enabled = append(f.enabled, enable)
	return nil
}

func (f *fakeDNS) Close() {}

func TestDNSProxyApplyIdempotentAndClear(t *testing.T) {
	f := &fakeDNS{}
	p := NewDNSProxyProvider(f)

	// 非空：逐条 add + enable
	if err := p.Sync(context.Background(), []string{"8.8.8.8", "2001:4860:4860::8888"}); err != nil {
		t.Fatalf("Sync(apply): %v", err)
	}
	if len(f.calls) != 2 || !f.calls[0].add || !f.calls[1].add {
		t.Fatalf("应逐条 add 两条上游: %v", f.calls)
	}
	if len(f.enabled) != 1 || !f.enabled[0] {
		t.Fatalf("应恰好 dns enable 一次: %v", f.enabled)
	}

	// 声明未变：幂等跳过（不再下发）
	if err := p.Sync(context.Background(), []string{"8.8.8.8", "2001:4860:4860::8888"}); err != nil {
		t.Fatalf("Sync(幂等): %v", err)
	}
	if len(f.calls) != 2 || len(f.enabled) != 1 {
		t.Fatalf("声明未变不应重复下发: calls=%v enabled=%v", f.calls, f.enabled)
	}

	// 改上游：删旧（登记里没有的）+ 加新，不重复 enable
	if err := p.Sync(context.Background(), []string{"8.8.8.8", "1.1.1.1"}); err != nil {
		t.Fatalf("Sync(改上游): %v", err)
	}
	if len(f.calls) != 4 {
		t.Fatalf("改上游应一删一加: %v", f.calls)
	}
	if f.calls[2].add || f.calls[2].server != "2001:4860:4860::8888" {
		t.Fatalf("应先按登记删旧上游: %+v", f.calls[2])
	}
	if !f.calls[3].add || f.calls[3].server != "1.1.1.1" {
		t.Fatalf("应补新增上游: %+v", f.calls[3])
	}
	if len(f.enabled) != 1 {
		t.Fatalf("已启用时不应重复 enable: %v", f.enabled)
	}

	// 清空：按登记 del 全部 + disable
	if err := p.Sync(context.Background(), nil); err != nil {
		t.Fatalf("Sync(clear): %v", err)
	}
	if len(f.enabled) != 2 || f.enabled[1] {
		t.Fatalf("清空应 dns disable: %v", f.enabled)
	}
	wantDels := map[string]bool{"8.8.8.8": true, "1.1.1.1": true}
	for _, c := range f.calls[4:] {
		if c.add || !wantDels[c.server] {
			t.Fatalf("清空应按登记撤销全部上游: %+v", f.calls[4:])
		}
		delete(wantDels, c.server)
	}
	if len(wantDels) != 0 {
		t.Fatalf("清空漏删上游: %v", wantDels)
	}

	// 已无登记：再次清空是空操作（幂等）
	n := len(f.calls)
	if err := p.Sync(context.Background(), nil); err != nil {
		t.Fatalf("Sync(再清): %v", err)
	}
	if len(f.calls) != n || len(f.enabled) != 2 {
		t.Fatalf("无登记时清空不应下发: calls=%d enabled=%v", len(f.calls), f.enabled)
	}
}

func TestDNSProxyResetForcesReplay(t *testing.T) {
	f := &fakeDNS{}
	p := NewDNSProxyProvider(f)
	if err := p.Sync(context.Background(), []string{"8.8.8.8"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// reset（恢复收敛前调用）：登记清空后，同一声明会重新下发（VPP 重启后 dns 配置已消失）
	p.reset()
	if err := p.Sync(context.Background(), []string{"8.8.8.8"}); err != nil {
		t.Fatalf("Sync(重放): %v", err)
	}
	if len(f.calls) != 2 || !f.calls[1].add || f.calls[1].server != "8.8.8.8" {
		t.Fatalf("reset 后应重新下发同一条上游: %v", f.calls)
	}
	if len(f.enabled) != 2 || !f.enabled[1] {
		t.Fatalf("reset 后应重新 enable: %v", f.enabled)
	}
}

func TestDNSProxyErrorPropagates(t *testing.T) {
	f := &fakeDNS{err: errors.New("boom")}
	p := NewDNSProxyProvider(f)
	if err := p.Sync(context.Background(), []string{"8.8.8.8"}); err == nil {
		t.Fatal("下发失败应返回错误，不谎称成功")
	}
}
