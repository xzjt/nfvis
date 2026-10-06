package network

// 决策 #394②：DHCP 中继（VPP dhcp proxy）的**登记漂移**与**双向对账**单测。
//
// 口径：登记＝最后一个成功下发的状态（逐步骤推进）；ReconcileProxy 双向（补声明缺失 + 清未声明）。
// 假客户端是一台**有状态**的 proxy 表（记在场的 (rx,server)，支持按 server 注入加新失败），
// 因此能断言「撤旧成功后加新失败 ⇒ 登记不再谎称旧值在场」这一真实时序。

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// relayFake 有状态的 DHCP proxy 假客户端（记录在场的 (rx,server)；支持按 server 注入加新失败）。
type relayFake struct {
	mu      sync.Mutex
	byRx    map[uint32]map[string]bool
	calls   []proxyCall
	failAdd map[string]bool
	errAll  error // 非空 ⇒ 所有 ProxySet 失败（撤旧失败注入）
}

func newRelayFake() *relayFake {
	return &relayFake{byRx: map[uint32]map[string]bool{}, failAdd: map[string]bool{}}
}

func (f *relayFake) ProxySet(rx, srv uint32, isAdd bool, server, src string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, proxyCall{rx: rx, srvVrf: srv, isAdd: isAdd, server: server, src: src})
	if f.errAll != nil {
		return f.errAll
	}
	if isAdd && f.failAdd[server] {
		return fmt.Errorf("注入：加新失败 %s", server)
	}
	set := f.byRx[rx]
	if set == nil {
		set = map[string]bool{}
		f.byRx[rx] = set
	}
	if isAdd {
		set[server] = true
	} else {
		delete(set, server)
	}
	return nil
}

func (f *relayFake) ProxyDump() ([]ProxyEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ProxyEntry
	for rx, set := range f.byRx {
		e := ProxyEntry{RxVrfID: rx, Src: "192.168.100.1"}
		for s := range set {
			e.Servers = append(e.Servers, ProxyServer{VrfID: rx, Server: s})
		}
		if len(e.Servers) > 0 {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *relayFake) Close() {}

func relayVS(server string) model.VirtualSwitch {
	return model.VirtualSwitch{Name: "vs-a", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.100.1/24"}},
		DhcpRelayServer: server}
}

// TestDhcpRelaySyncRegistrationTracksLastApplied（决策 #394②）：撤旧成功、加新失败后，登记必须停在
// 「最后成功下发的状态」——操作者改回旧值再提交时应如实重新下发（旧实现登记仍旧值 ⇒「值未变」早退，
// 数据面永久失中继）。
func TestDhcpRelaySyncRegistrationTracksLastApplied(t *testing.T) {
	ctx := context.Background()
	f := newRelayFake()
	p := NewDhcpProvider(f)
	vsA := relayVS("192.168.100.2")
	if err := p.SyncRelay(ctx, vsA); err != nil {
		t.Fatalf("初次 apply: %v", err)
	}
	// 改 server B：撤旧 A 成功、加新 B 失败（注入）。
	f.failAdd["10.0.0.99"] = true
	if err := p.SyncRelay(ctx, relayVS("10.0.0.99")); err == nil {
		t.Fatal("加新失败应上抛")
	}
	// 此刻数据面 A/B 均不在；登记必须已随「撤旧成功」清空（不再谎称 A 在场）。
	f.mu.Lock()
	if len(f.byRx[TableID(GatewayVRFName("vs-a"))]) != 0 {
		f.mu.Unlock()
		t.Fatalf("撤旧成功 + 加新失败后数据面应无条目: %+v", f.byRx)
	}
	f.mu.Unlock()
	// 操作者改回旧值 A 再提交：必须重新下发（旧实现此处 had=true 且 rec==want ⇒ 早退，0 调用）。
	delete(f.failAdd, "10.0.0.99")
	f.calls = nil
	if err := p.SyncRelay(ctx, vsA); err != nil {
		t.Fatalf("改回旧值重试: %v", err)
	}
	if len(f.calls) != 1 || !f.calls[0].isAdd || f.calls[0].server != "192.168.100.2" {
		t.Fatalf("撤旧成功后加新失败 ⇒ 改回旧值应重新下发，实际 %v", f.calls)
	}
}

// TestDhcpRelaySyncRetryAfterAddFailure（决策 #394②）：加新失败后原样重试同一新值即成功
// （登记已随撤旧清空，重试走「新增」路径而非因登记说已下发而跳过）。
func TestDhcpRelaySyncRetryAfterAddFailure(t *testing.T) {
	ctx := context.Background()
	f := newRelayFake()
	p := NewDhcpProvider(f)
	if err := p.SyncRelay(ctx, relayVS("192.168.100.2")); err != nil {
		t.Fatalf("初次 apply: %v", err)
	}
	f.failAdd["10.0.0.99"] = true
	if err := p.SyncRelay(ctx, relayVS("10.0.0.99")); err == nil {
		t.Fatal("加新失败应上抛")
	}
	delete(f.failAdd, "10.0.0.99")
	f.calls = nil
	if err := p.SyncRelay(ctx, relayVS("10.0.0.99")); err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if len(f.calls) != 1 || !f.calls[0].isAdd || f.calls[0].server != "10.0.0.99" {
		t.Fatalf("重试应下发新值: %v", f.calls)
	}
}

// TestDhcpRelaySyncTeardownFailureKeepsRegistration（决策 #394②）：撤旧**失败**时登记保留旧值，
// 重试会再次尝试撤旧（不因登记说已下发而静默跳过）。
func TestDhcpRelaySyncTeardownFailureKeepsRegistration(t *testing.T) {
	ctx := context.Background()
	f := newRelayFake()
	p := NewDhcpProvider(f)
	if err := p.SyncRelay(ctx, relayVS("192.168.100.2")); err != nil {
		t.Fatalf("初次 apply: %v", err)
	}
	// 撤旧失败：注入 add 失败不触发（走的是 del），改用整体 err。
	f.mu.Lock()
	f.failAdd = map[string]bool{}
	f.mu.Unlock()
	f.errAll = fmt.Errorf("注入：撤销失败")
	if err := p.SyncRelay(ctx, relayVS("10.0.0.99")); err == nil {
		t.Fatal("撤旧失败应上抛")
	}
	// 登记应仍为旧值：再次提交同一新值会重试撤旧（而非早退）。
	f.errAll = nil
	f.calls = nil
	if err := p.SyncRelay(ctx, relayVS("10.0.0.99")); err != nil {
		t.Fatalf("重试: %v", err)
	}
	if len(f.calls) != 2 || f.calls[0].isAdd || f.calls[0].server != "192.168.100.2" ||
		!f.calls[1].isAdd || f.calls[1].server != "10.0.0.99" {
		t.Fatalf("撤旧失败后重试应先撤旧后加新: %v", f.calls)
	}
}

// TestDhcpRelayReconcileProxyAddsMissing（决策 #394②）：声明了 relay 但数据面无 ⇒ 巡检补发；
// 已在场则幂等不重复补发。
func TestDhcpRelayReconcileProxyAddsMissing(t *testing.T) {
	f := newRelayFake() // 数据面空（带外删除/提交失败残留/进程重启的现场）
	p := NewDhcpProvider(f)
	declared := []model.VirtualSwitch{relayVS("192.168.100.2")}
	if err := p.ReconcileProxy(declared); err != nil {
		t.Fatalf("对账应成功: %v", err)
	}
	if len(f.calls) != 1 || !f.calls[0].isAdd || f.calls[0].server != "192.168.100.2" {
		t.Fatalf("声明缺失应补发: %v", f.calls)
	}
	f.calls = nil
	if err := p.ReconcileProxy(declared); err != nil {
		t.Fatalf("再次对账: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("已在场不应重复补发: %v", f.calls)
	}
}
