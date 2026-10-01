package network

// 决策 #321：残渣对账——非 VRF 表类残渣（ACL / bridge-domain）纳入 #192 的同一份对账视野，
// 按数据面事实重建/消解告警（跨 nfvisd 重启仍可见），并消解已复原对象的提交期补偿告警。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// activeCompensation 返回当前活动的提交期补偿告警（供残渣对账用例断言）。
func activeCompensation(f *recoveryFixture) []Alarm {
	var out []Alarm
	for _, a := range f.alarms.List(AlarmActive) {
		if a.Code == orchestrator.CommitCompensationFailed {
			out = append(out, a)
		}
	}
	return out
}

// ACL 残渣：对账按数据面事实报出 ACL_LEFTOVER，对象消失/声明回来即自动消警；
// 换一张空告警表模拟 nfvisd 重启后，残渣仍按数据面重建（不靠进程内记忆）。
func TestReconcileResidueACLRebuiltAndResolved(t *testing.T) {
	f := newRecoveryFixture()
	ctx := context.Background()
	f.acl.putACL("stray-acl") // 数据面已有、配置未声明的 ACL（提交补偿残渣）

	errs := f.net.ReconcileResidue(ctx, model.Config{})
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "acl/stray-acl") && strings.Contains(e.Error(), "配置未声明") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ACL 残渣必须进未收敛清单: %v", errs)
	}
	active := f.alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmACLLeftover || active[0].Source != "acl/stray-acl" {
		t.Fatalf("应产生 ACL_LEFTOVER 告警: %+v", active)
	}
	if !strings.Contains(active[0].Message, "对账") || !strings.Contains(active[0].Message, "不可回溯") {
		t.Fatalf("文案应如实说明由对账重建、原始提交不可回溯: %+v", active[0])
	}

	// 配置声明回来 ⇒ 该对象已对得上配置，不再算残渣，自动消警
	if errs := f.net.ReconcileResidue(ctx, model.Config{Acls: []model.Acl{{Name: "stray-acl"}}}); len(errs) != 0 {
		t.Fatalf("已声明不应再报残渣: %v", errs)
	}
	if got := f.alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("残渣消失应自动消警: %+v", got)
	}

	// 跨「重启」：换一张空告警表（进程内历史全丢），数据面残渣仍在 ⇒ 对账把它重建出来
	f.alarms = NewAlarmStore()
	f.net.SetAlarms(f.alarms)
	if errs := f.net.EnsureConsistent(ctx, model.Config{}); len(errs) == 0 {
		t.Fatal("重启后残渣仍在，恢复收敛对账应报出来")
	}
	active = f.alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmACLLeftover {
		t.Fatalf("重启后 ACL 残渣必须仍可见（按数据面对账重建）: %+v", active)
	}
}

// bridge-domain 残渣：BD-Tag 不在配置里即报 BRIDGE_DOMAIN_LEFTOVER，声明回来即消警。
func TestReconcileResidueBridgeDomain(t *testing.T) {
	f := newRecoveryFixture()
	ctx := context.Background()
	f.l2.bdRuntimes = []BDRuntime{{ID: 4242, Name: "vs-stray"}}

	errs := f.net.ReconcileResidue(ctx, model.Config{})
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "bridge-domain/vs-stray") {
			found = true
		}
	}
	if !found {
		t.Fatalf("BD 残渣必须进未收敛清单: %v", errs)
	}
	active := f.alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmBDLeftover || active[0].Source != "bridge-domain/vs-stray" {
		t.Fatalf("应产生 BRIDGE_DOMAIN_LEFTOVER 告警: %+v", active)
	}
	// 声明回来 ⇒ 消警
	if errs := f.net.ReconcileResidue(ctx, model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-stray", Type: "l2"}}}); len(errs) != 0 {
		t.Fatalf("已声明不应再报残渣: %v", errs)
	}
	if got := f.alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("残渣消失应自动消警: %+v", got)
	}
}

// 提交期补偿告警的消解路径：对象仍残留时不消，残渣消失后由对账消解。
func TestReconcileResidueResolvesCompensationAlarmWhenGone(t *testing.T) {
	f := newRecoveryFixture()
	ctx := context.Background()
	f.alarms.Raise(orchestrator.CommitScope, SeverityError, orchestrator.CommitCompensationFailed,
		"补偿失败", "acl[stray]")
	f.acl.putACL("stray")

	// 残渣仍在数据面且配置未声明 ⇒ 告警保持
	f.net.ReconcileResidue(ctx, model.Config{})
	if got := activeCompensation(f); len(got) != 1 {
		t.Fatalf("残渣仍在，提交期告警不得消解: %+v", got)
	}

	// 残渣被清掉（手工/重启） ⇒ 对象已对得上配置，对账消警
	f.acl.ACLDel(f.acl.tags["stray"])
	f.net.ReconcileResidue(ctx, model.Config{})
	if got := activeCompensation(f); len(got) != 0 {
		t.Fatalf("残渣已消失，提交期告警应由对账消解: %+v", got)
	}
}

// 对象已由配置声明（配置说它该在，重放负责收敛）⇒ 提交期补偿告警即消解。
func TestReconcileResidueResolvesCompensationAlarmWhenDeclared(t *testing.T) {
	f := newRecoveryFixture()
	ctx := context.Background()
	f.alarms.Raise(orchestrator.CommitScope, SeverityError, orchestrator.CommitCompensationFailed,
		"补偿失败", "acl[a1]")
	f.net.ReconcileResidue(ctx, model.Config{Acls: []model.Acl{{Name: "a1"}}})
	if got := activeCompensation(f); len(got) != 0 {
		t.Fatalf("对象已声明，提交期告警应消解: %+v", got)
	}
}

// VRF 表路径不回归：表不在数据面且未声明 ⇒ 提交期 vrf[...] 告警消解；残渣表仍在 ⇒ 保持。
func TestReconcileResidueVrfCompensationAlarm(t *testing.T) {
	f := newRecoveryFixture()
	ctx := context.Background()
	f.alarms.Raise(orchestrator.CommitScope, SeverityError, orchestrator.CommitCompensationFailed,
		"补偿失败", "vrf[vs-gone]")
	// 表不在数据面、配置也未声明 ⇒ 视为已复原
	f.net.ReconcileResidue(ctx, model.Config{})
	if got := activeCompensation(f); len(got) != 0 {
		t.Fatalf("表不在数据面且未声明，应消警: %+v", got)
	}

	// 残渣表仍在数据面：保持告警，且报出 VRF_TABLE_LEFTOVER
	name := "vs-leftover"
	tid := TableID(name)
	if err := f.net.l3.ApplyVRF(ctx, model.Vrf{Name: name}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	f.alarms.Raise(orchestrator.CommitScope, SeverityError, orchestrator.CommitCompensationFailed,
		"补偿失败", "vrf["+name+"]")
	f.net.ReconcileResidue(ctx, model.Config{})
	if got := activeCompensation(f); len(got) != 1 {
		t.Fatalf("残渣表仍在，提交期告警不得消解: %+v", got)
	}
	hasTable := false
	for _, a := range f.alarms.List(AlarmActive) {
		if a.Code == AlarmTableLeftover && a.Source == "ip-table/"+strconv.FormatUint(uint64(tid), 10) {
			hasTable = true
		}
	}
	if !hasTable {
		t.Fatalf("残渣表应报 VRF_TABLE_LEFTOVER: %+v", f.alarms.List(AlarmActive))
	}
}

// 查询失败不得被当成「残渣已消失」：该类型扫描失败时保守留警（问不出来 ≠ 已复原）。
func TestReconcileResidueKeepsAlarmOnScanFailure(t *testing.T) {
	f := newRecoveryFixture()
	ctx := context.Background()
	f.alarms.Raise(recoveryScope, SeverityWarning, AlarmACLLeftover, "旧告警", "acl/stray")
	f.acl.err = errors.New("acl_dump 超时")

	errs := f.net.ReconcileResidue(ctx, model.Config{})
	if len(errs) == 0 {
		t.Fatal("对账查询失败必须上抛，不得当作无残渣")
	}
	if got := f.alarms.List(AlarmActive); len(got) != 1 || got[0].Code != AlarmACLLeftover {
		t.Fatalf("扫描失败时应保守留警: %+v", got)
	}
}
