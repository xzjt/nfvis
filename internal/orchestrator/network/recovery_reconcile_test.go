package network

// 决策 #333（修 R111-1）：恢复收敛告警族的按来源廉价复核——15s 巡检路径，**不做全量重放**。
// 用例编号对应交付说明：① interfaces 来源已删→消解；② IFACE_MISSING+口已存在→消解；
// ③ IFACE_MISSING+口仍缺→保留；④ UNCONVERGED 来源已删→消解、来源还在→保留；
// ⑤ family 级→保留；⑥ 不可解析 source→保留。

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// raiseRecovery 在 recovery 作用域放一条活动告警（模拟恢复收敛曾失败落下的告警）。
func raiseRecovery(f *recoveryFixture, code, source string) {
	f.alarms.Raise(recoveryScope, SeverityWarning, code, "测试告警:"+source, source)
}

// recoveryActive 返回当前活动的恢复收敛族告警 source 集合（不含残渣码等其它项）。
func recoveryActive(f *recoveryFixture) map[string]bool {
	out := map[string]bool{}
	for _, a := range f.alarms.ActiveOf(recoveryScope) {
		if a.Code == AlarmUnconverged || a.Code == AlarmIfaceMissing {
			out[a.Code+"/"+a.Source] = true
		}
	}
	return out
}

// ① interfaces 来源已删 → 消解（失败前提已消失）。
func TestReconcileRecoveryAlarmsIfaceSourceDeleted(t *testing.T) {
	f := newRecoveryFixture()
	raiseRecovery(f, AlarmIfaceMissing, "interfaces/ens192")
	raiseRecovery(f, AlarmUnconverged, "vrfs/vs-gone")

	errs := f.net.ReconcileRecoveryAlarms(context.Background(), model.Config{})
	if len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	if got := recoveryActive(f); len(got) != 0 {
		t.Fatalf("来源已删的两条都应消解，实际保留: %v", got)
	}
}

// ② RECOVERY_IFACE_MISSING + 来源 interfaces/<n> + 配置仍声明 + 口已出现在 VPP → 消解。
func TestReconcileRecoveryAlarmsIfaceMissingNowPresent(t *testing.T) {
	f := newRecoveryFixture()
	raiseRecovery(f, AlarmIfaceMissing, "interfaces/ens192") // fakeSvc 默认就有 ens192
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192"}}}

	if errs := f.net.ReconcileRecoveryAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("查询不应出错: %v", errs)
	}
	if got := recoveryActive(f); len(got) != 0 {
		t.Fatalf("口已在 VPP，告警应消解: %v", got)
	}
}

// ③ RECOVERY_IFACE_MISSING + 口仍缺 → 保留（不猜测）。
func TestReconcileRecoveryAlarmsIfaceMissingStillAbsent(t *testing.T) {
	f := newRecoveryFixture()
	raiseRecovery(f, AlarmIfaceMissing, "interfaces/bvi9") // 不在 fakeSvc.ifaces 里
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "bvi9"}}}

	if errs := f.net.ReconcileRecoveryAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("查询成功时不应上抛错误: %v", errs)
	}
	if got := recoveryActive(f); len(got) != 1 {
		t.Fatalf("口仍缺，告警应保留: %v", got)
	}
}

// ③b 存在性查询失败 → 保守保留并计入返回错误（问不出来 ≠ 已复原）。
func TestReconcileRecoveryAlarmsKeepsOnQueryFailure(t *testing.T) {
	f := newRecoveryFixture()
	raiseRecovery(f, AlarmIfaceMissing, "interfaces/ens192")
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192"}}}
	f.svc.err = errors.New("vpp api 超时")

	errs := f.net.ReconcileRecoveryAlarms(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("查询错误必须上抛: %v", errs)
	}
	if got := recoveryActive(f); len(got) != 1 {
		t.Fatalf("查询失败应保守留警: %v", got)
	}
}

// ④ UNCONVERGED：来源已删 → 消解；来源仍在声明集 → 保留（15s 路径证实不了
// 「现在能 apply 成功」，不猜——决策 #333 的核心保守口径）。
func TestReconcileRecoveryAlarmsUnconverged(t *testing.T) {
	f := newRecoveryFixture()
	raiseRecovery(f, AlarmUnconverged, "acls/gone")
	raiseRecovery(f, AlarmUnconverged, "qos/policies/kept")
	raiseRecovery(f, AlarmUnconverged, "virtual-switches/vs-kept")
	raiseRecovery(f, AlarmUnconverged, "vnf-ports/vm-a/eth0")
	cfg := model.Config{
		QosPolicies:     []model.QosPolicy{{Name: "kept"}},
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-kept", Type: "l2"}},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}}}},
	}

	if errs := f.net.ReconcileRecoveryAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	got := recoveryActive(f)
	if len(got) != 3 {
		t.Fatalf("仍声明的三条应保留、已删的一条应消解: %v", got)
	}
	for _, want := range []string{
		AlarmUnconverged + "/qos/policies/kept",
		AlarmUnconverged + "/virtual-switches/vs-kept",
		AlarmUnconverged + "/vnf-ports/vm-a/eth0",
	} {
		if !got[want] {
			t.Fatalf("仍声明应保留 %s: %v", want, got)
		}
	}
}

// ⑤ family 级来源（无对象名字段）→ 保守保留，即使配置为空也不消解。
func TestReconcileRecoveryAlarmsKeepsFamilySources(t *testing.T) {
	f := newRecoveryFixture()
	for _, src := range []string{"ip-tables", "nat", "bonds", "virtual-switches", "protocols/lldp", "residue-scan"} {
		raiseRecovery(f, AlarmUnconverged, src)
	}

	if errs := f.net.ReconcileRecoveryAlarms(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	if got := recoveryActive(f); len(got) != 6 {
		t.Fatalf("family 级来源应全部保守保留: %v", got)
	}
}

// ⑥ 不可解析 source → 保守保留（不认识的来源不猜测）。
func TestReconcileRecoveryAlarmsKeepsUnparsableSources(t *testing.T) {
	f := newRecoveryFixture()
	for _, src := range []string{"", "weird", "qos/policies/", "vnf-ports/vm-a", "acls-x/foo", "unknown/obj"} {
		raiseRecovery(f, AlarmIfaceMissing, src)
	}

	if errs := f.net.ReconcileRecoveryAlarms(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	if got := recoveryActive(f); len(got) != 6 {
		t.Fatalf("不可解析来源应全部保守保留: %v", got)
	}
}

// 复核只动恢复收敛族的两码：recovery 作用域里的残渣码有独立对账路径（residue.go），
// 不得被本方法触碰；来源映射也只对族码生效、互不串门。
func TestReconcileRecoveryAlarmsDoesNotTouchOtherCodes(t *testing.T) {
	f := newRecoveryFixture()
	f.alarms.Raise(recoveryScope, SeverityWarning, AlarmACLLeftover, "残渣", "acl/stray")
	f.acl.putACL("stray") // 残渣仍在数据面
	f.alarms.Raise(recoveryScope, SeverityError, AlarmIfaceMissing, "缺失", "acls/stray")

	// cfg 未声明 acls/stray：恢复族照映射消解；残渣码 acl/stray 保持活动（由残渣对账负责）。
	_ = f.net.ReconcileRecoveryAlarms(context.Background(), model.Config{})
	if got := recoveryActive(f); len(got) != 0 {
		t.Fatalf("恢复族告警应按映射消解: %v", got)
	}
	found := false
	for _, a := range f.alarms.List(AlarmActive) {
		if a.Code == AlarmACLLeftover && a.Source == "acl/stray" {
			found = true
		}
	}
	if !found {
		t.Fatal("残渣码不得被恢复复核触碰")
	}
}
