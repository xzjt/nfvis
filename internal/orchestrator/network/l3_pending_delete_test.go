package network

// round87（决策 #192）：删表延后（NAT 用过的表带 VPP 引用）与接口换表的登记口径单测。
//
// 真机背景：
//   - NAT44 一旦把某张 IP 表当作 inside/outside，该表在 VPP 里永久带 `nat44-ei-hi` 引用，
//     删规则/关插件都不释放，只有重启数据面才干净 ⇒ 删表读回必然报「仍未收敛」；
//   - 同一提交里「改 NAT 出接口 + 删旧 L3 交换机」因此必然整体失败并回滚（R86-10），
//     且失败前那一步已把该交换机的接口解绑回默认表、地址清空（R86-9 的残渣形态之一）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// 删表读回未收敛 → 留档待清理；表一旦不在 VPP（数据面重启后的正常结果）→ 清登记。
func TestRetryPendingDeletesClearsWhenTableGone(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	name := "vs-stuck"
	tid := TableID(name)
	if err := p.ApplyVRF(ctx, model.Vrf{Name: name}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	f.tableKeepOnDelete[tid] = true // 模拟 NAT 用过的表：删了也不消失
	if err := p.DeleteVRF(ctx, name); !errors.Is(err, ErrVrfNotRemoved) {
		t.Fatalf("应报未收敛: %v", err)
	}
	// 仍在 VPP：复核不得清登记（残渣未消失，告警要留着）
	if cleared := p.RetryPendingDeletes(ctx, nil); len(cleared) != 0 {
		t.Fatalf("表仍在 VPP 时不得清登记: %v", cleared)
	}
	// 数据面重启后表随运行态消失：复核确认并清登记（调用方据此消警）
	delete(f.tables, tid)
	cleared := p.RetryPendingDeletes(ctx, nil)
	if len(cleared) != 1 || cleared[0] != name {
		t.Fatalf("表已消失应清登记并返回该交换机名: %v", cleared)
	}
	if again := p.RetryPendingDeletes(ctx, nil); len(again) != 0 {
		t.Fatalf("清登记应幂等: %v", again)
	}
}

// 配置又把该交换机声明回来 → 表是合法存在，登记与告警一并清除（不残留无关告警）。
func TestRetryPendingDeletesSkipsRedeclared(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	name := "vs-back"
	tid := TableID(name)
	if err := p.ApplyVRF(ctx, model.Vrf{Name: name}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	f.tableKeepOnDelete[tid] = true
	if err := p.DeleteVRF(ctx, name); !errors.Is(err, ErrVrfNotRemoved) {
		t.Fatalf("应报未收敛: %v", err)
	}
	cleared := p.RetryPendingDeletes(ctx, func(n string) bool { return n == name })
	if len(cleared) != 1 || cleared[0] != name {
		t.Fatalf("被重新声明的交换机应清登记（表合法存在）: %v", cleared)
	}
}

// 接口换表（改挂另一台 L3 交换机）后，旧表的登记必须摘掉：
// 否则旧表被删时会把该口解绑回默认表，把新配置的归属无声撤销。
func TestApplyVRFReassignsIfaceRegistration(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-a", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF vs-a: %v", err)
	}
	if got := p.AttachedIfaces("vs-a"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("前置：vs-a 应含 ens192(1): %v", got)
	}
	// 同一接口改挂 vs-b
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-b", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF vs-b: %v", err)
	}
	if got := p.AttachedIfaces("vs-a"); len(got) != 0 {
		t.Fatalf("换表后旧表登记必须摘掉（否则删旧表会把它解绑回默认表）: %v", got)
	}
	// 删旧表：不得再动这个口（它已在 vs-b）
	f.setTables = 0
	f.cleared = nil
	if err := p.DeleteVRF(ctx, "vs-a"); err != nil {
		t.Fatalf("DeleteVRF vs-a: %v", err)
	}
	if f.setTables != 0 || len(f.cleared) != 0 {
		t.Fatalf("删旧表不得解绑/清地址已改挂的口: setTables=%d cleared=%v", f.setTables, f.cleared)
	}
	if f.v4table[1] != TableID("vs-b") {
		t.Fatalf("该口应仍在 vs-b 表: %v", f.v4table[1])
	}
}

// 端到端（网络侧）：删表延后 → 告警留痕 → 数据面重启后复核 → 自动消警。
func TestRetryDeferredVRFDeletesResolvesAlarm(t *testing.T) {
	fx := newRecoveryFixture()
	ctx := context.Background()
	name := "vs-nat-used"
	tid := TableID(name)
	if err := fx.net.l3.ApplyVRF(ctx, model.Vrf{Name: name}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	fx.l3.tableKeepOnDelete[tid] = true
	if err := fx.net.DeleteVRF(ctx, name); !errors.Is(err, ErrVrfNotRemoved) {
		t.Fatalf("应报未收敛: %v", err)
	}
	// 提交编排按「延后收敛」处理并留告警（这里模拟 applier 的上报）
	fx.alarms.Raise(orchestrator.CommitScope, orchestrator.SeverityWarning,
		orchestrator.CommitVrfDeleteDeferred, "表仍留在数据面", name)
	if got := fx.alarms.List("active"); len(got) != 1 {
		t.Fatalf("前置：应有一条活动告警: %+v", got)
	}
	// 表仍在：复核不得消警
	fx.net.RetryDeferredVRFDeletes(ctx, model.Config{})
	if got := fx.alarms.List("active"); len(got) != 1 {
		t.Fatalf("表仍在数据面时不得消警: %+v", got)
	}
	// 数据面重启后表消失：复核清登记并消警
	delete(fx.l3.tables, tid)
	fx.net.RetryDeferredVRFDeletes(ctx, model.Config{})
	if got := fx.alarms.List("active"); len(got) != 0 {
		t.Fatalf("表已清理应自动消警: %+v", got)
	}
	if got := fx.alarms.List("all"); len(got) != 1 || got[0].State != AlarmResolved {
		t.Fatalf("应留下 resolved 记录（可追溯）: %+v", got)
	}
}

// 同族的补偿残渣告警（vrf[X]/del-vrf[X]）在表确认清理后一并消掉：
// 否则「表已不在」而告警还挂着，正是 round86 R86-6 那类陈旧告警。
func TestRetryDeferredVRFDeletesResolvesCompensationAlarm(t *testing.T) {
	fx := newRecoveryFixture()
	ctx := context.Background()
	name := "vs-residue"
	tid := TableID(name)
	if err := fx.net.l3.ApplyVRF(ctx, model.Vrf{Name: name}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	fx.l3.tableKeepOnDelete[tid] = true
	if err := fx.net.DeleteVRF(ctx, name); !errors.Is(err, ErrVrfNotRemoved) {
		t.Fatalf("应报未收敛: %v", err)
	}
	fx.alarms.Raise(orchestrator.CommitScope, orchestrator.SeverityError,
		orchestrator.CommitCompensationFailed, "补偿未完成", orchestrator.VrfOpDesc(name))
	delete(fx.l3.tables, tid)
	fx.net.RetryDeferredVRFDeletes(ctx, model.Config{})
	if got := fx.alarms.List("active"); len(got) != 0 {
		t.Fatalf("残渣已消失应消警: %+v", got)
	}
}

// 读回查询失败按「表仍在」处理：不清登记、不消警（把「问不出来」当「已清理」是假绿）。
func TestRetryPendingDeletesKeepsOnQueryFailure(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	name := "vs-q"
	tid := TableID(name)
	if err := p.ApplyVRF(ctx, model.Vrf{Name: name}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	f.tableKeepOnDelete[tid] = true
	if err := p.DeleteVRF(ctx, name); !errors.Is(err, ErrVrfNotRemoved) {
		t.Fatalf("应报未收敛: %v", err)
	}
	f.tableDumpErr = errors.New("dump 超时")
	if cleared := p.RetryPendingDeletes(ctx, nil); len(cleared) != 0 {
		t.Fatalf("读回失败时不得清登记: %v", cleared)
	}
	f.tableDumpErr = nil
	delete(f.tables, tid)
	if cleared := p.RetryPendingDeletes(ctx, nil); len(cleared) != 1 {
		t.Fatalf("读回恢复后应能确认清理: %v", cleared)
	}
}

// 未收敛文案仍须给出可照做的一步（决策 #187 的口径在 #192 下保留：说的是「重启后自动清理」）。
func TestVrfNotRemovedHintKeepsNextStep(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	tid := TableID("vs-hint")
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-hint"}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	f.tableKeepOnDelete[tid] = true
	err := p.DeleteVRF(ctx, "vs-hint")
	if err == nil || !strings.Contains(err.Error(), "request vpp restart") ||
		!strings.Contains(err.Error(), "NAT") {
		t.Fatalf("文案应含原因（NAT）与下一步（request vpp restart）: %v", err)
	}
}

// 未收敛项的**可复查口径**：VPP 里存在配置未声明的表 ⇒ 报未收敛并进告警（跨 nfvisd 重启
// 依然看得见——这一项不靠进程内登记）；表随数据面重启消失后自动消警。
func TestEnsureConsistentReportsLeftoverTable(t *testing.T) {
	fx := newRecoveryFixture()
	ctx := context.Background()
	name := "vs-leftover"
	tid := TableID(name)
	if err := fx.net.l3.ApplyVRF(ctx, model.Vrf{Name: name}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	fx.l3.tableKeepOnDelete[tid] = true
	if err := fx.net.DeleteVRF(ctx, name); !errors.Is(err, ErrVrfNotRemoved) {
		t.Fatalf("应报未收敛: %v", err)
	}
	// 配置里已无该交换机（模拟提交已删掉它）⇒ 表成为残留
	errs := fx.net.EnsureConsistent(ctx, model.Config{Vrfs: []model.Vrf{}})
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "ip-table/") && strings.Contains(e.Error(), "配置未声明") {
			found = true
		}
	}
	if !found {
		t.Fatalf("残留表必须进未收敛清单: %v", errs)
	}
	active := fx.alarms.List("active")
	if len(active) != 1 || active[0].Code != AlarmTableLeftover || active[0].Severity != SeverityWarning {
		t.Fatalf("残留表应产生 VRF_TABLE_LEFTOVER 告警: %+v", active)
	}
	if !strings.Contains(active[0].Message, "request vpp restart") {
		t.Fatalf("告警文案要给下一步: %+v", active[0])
	}
	// 数据面重启后表消失：对账不再命中 ⇒ 自动消警
	delete(fx.l3.tables, tid)
	if errs := fx.net.EnsureConsistent(ctx, model.Config{Vrfs: []model.Vrf{}}); len(errs) != 0 {
		t.Fatalf("表已消失不应再报未收敛: %v", errs)
	}
	if got := fx.alarms.List("active"); len(got) != 0 {
		t.Fatalf("残留消失应自动消警: %+v", got)
	}
}

// 声明里的交换机（含 BVI 网关专属 VRF）不算残留：正常收敛必须一条告警都不产生。
func TestEnsureConsistentNoLeftoverForDeclaredTables(t *testing.T) {
	fx := newRecoveryFixture()
	ctx := context.Background()
	cfg := model.Config{
		Vrfs: []model.Vrf{{Name: "vs-l3"}},
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-l2", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"10.9.9.1/24"}}},
		},
	}
	if errs := fx.net.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("正常收敛不应有未收敛项: %v", errs)
	}
	if got := fx.alarms.List("active"); len(got) != 0 {
		t.Fatalf("声明内的表不得报残留: %+v", got)
	}
	if _, err := fx.net.l3.LeftoverTables(cfg); err != nil {
		t.Fatalf("LeftoverTables: %v", err)
	}
}

// 读回失败不得被当成「没有残留」（问不出来 ≠ 没有）。
func TestLeftoverTablesQueryFailurePropagates(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	f.tableDumpErr = errors.New("dump 超时")
	if _, err := p.LeftoverTables(model.Config{}); err == nil {
		t.Fatal("读回失败必须上抛，不得按「无残留」处理")
	}
}
