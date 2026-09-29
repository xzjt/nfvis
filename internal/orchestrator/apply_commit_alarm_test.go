package orchestrator

// round87（决策 #191/#192）：提交期残渣与「删表延后」的告警口径单测。
//
// 两条缺陷的真机现象：
//   - R86-9：提交失败后的补偿若也失败，残渣只出现在当次提交输出里，`show alarms` 事后查不到；
//   - R86-10：NAT 用过的表带 VPP 的 nat44-ei-hi 引用（只有数据面重启才释放），同一提交里
//     「改 NAT 出接口 + 删旧 L3 交换机」必然整体失败并回滚。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// errFakeDel 一次**真错误**（区别于 ErrVrfNotRemoved 的「表还在」）：必须硬失败。
var errFakeDel = errors.New("模拟删表失败（非未收敛）")

// fakeAlarms 记录提交期告警的 raise/resolve（实现 CommitAlarmSink）。
type fakeAlarms struct {
	raised   []string // "scope|severity|code|source"
	resolved []string // "scope|code|source"
}

func (f *fakeAlarms) Raise(scope, severity, code, message, source string) {
	f.raised = append(f.raised, scope+"|"+severity+"|"+code+"|"+source)
}

func (f *fakeAlarms) Resolve(scope, code, source string) bool {
	f.resolved = append(f.resolved, scope+"|"+code+"|"+source)
	return true
}

func (f *fakeAlarms) hasRaised(want string) bool {
	for _, r := range f.raised {
		if r == want {
			return true
		}
	}
	return false
}

func (f *fakeAlarms) hasResolved(want string) bool {
	for _, r := range f.resolved {
		if r == want {
			return true
		}
	}
	return false
}

// errNet 在 recNet 之上给指定对象的删/建注入错误（残渣与延后场景用）。
type errNet struct {
	recNet
	delErr map[string]error // DeleteVRF 按名注入
	vrfErr map[string]error // ApplyVRF 按名注入
}

func (n errNet) DeleteVRF(ctx context.Context, name string) error {
	if err, ok := n.delErr[name]; ok {
		*n.calls = append(*n.calls, "del-vrf:"+name)
		return err
	}
	return n.recNet.DeleteVRF(ctx, name)
}

func (n errNet) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	if err, ok := n.vrfErr[vrf.Name]; ok {
		*n.calls = append(*n.calls, "vrf:"+vrf.Name)
		return err
	}
	return n.recNet.ApplyVRF(ctx, vrf)
}

func newErrApplier(net errNet) (Applier, *fakeAlarms) {
	al := &fakeAlarms{}
	ap := NewApplier(net, recCompute{calls: net.calls}, recContainer{calls: net.calls},
		WithCommitAlarms(al))
	return ap, al
}

// 补偿失败 → 残渣必须进告警（此前只出现在当次提交输出里，事后不可见）。
func TestCompensationFailureRaisesResidueAlarm(t *testing.T) {
	calls := &[]string{}
	al := &fakeAlarms{}
	net := errNet{recNet: recNet{calls: calls}, delErr: map[string]error{"vrf-a": ErrVrfNotRemoved}}
	// vm 失败触发回滚；已执行的 vrf[vrf-a] 撤销 = DeleteVRF(vrf-a) → 注入未收敛 ⇒ 补偿失败
	ap := NewApplier(net, recCompute{calls: calls, failOn: "vm:vm-1"}, recContainer{calls: calls},
		WithCommitAlarms(al))
	err := ap.Apply(context.Background(),
		model.Config{},
		model.Config{Vrfs: []model.Vrf{{Name: "vrf-a"}}, VirtualMachineFunctions: []model.VMFunction{vmOf("vm-1")}})
	if err == nil || !strings.Contains(err.Error(), "补偿失败 vrf[vrf-a]") {
		t.Fatalf("应报补偿失败: %v", err)
	}
	if !al.hasRaised(CommitScope + "|" + SeverityError + "|" + CommitCompensationFailed + "|vrf[vrf-a]") {
		t.Fatalf("补偿失败必须进告警（error 级、source 为该对象）: %+v", al.raised)
	}
}

// 同一对象的计划操作下一次成功执行 → 残渣告警自动消解（「重新提交即复原」的闭环）。
func TestCompensationAlarmResolvedOnNextSuccess(t *testing.T) {
	calls := &[]string{}
	net := errNet{recNet: recNet{calls: calls}, delErr: map[string]error{"vrf-a": ErrVrfNotRemoved}}
	ap, al := newErrApplier(net)
	// 第一次：失败并留下残渣告警
	is := errNet{recNet: recNet{calls: calls, failOn: "vm:vm-1"}, delErr: map[string]error{"vrf-a": ErrVrfNotRemoved}}
	apFail := NewApplier(is, recCompute{calls: calls, failOn: "vm:vm-1"}, recContainer{calls: calls},
		WithCommitAlarms(al))
	if err := apFail.Apply(context.Background(), model.Config{},
		model.Config{Vrfs: []model.Vrf{{Name: "vrf-a"}}, VirtualMachineFunctions: []model.VMFunction{vmOf("vm-1")}}); err == nil {
		t.Fatalf("首次提交应失败")
	}
	// 第二次：同一对象重新收敛成功 → 消警
	if err := ap.Apply(context.Background(), model.Config{}, model.Config{Vrfs: []model.Vrf{{Name: "vrf-a"}}}); err != nil {
		t.Fatalf("重试提交应成功: %v", err)
	}
	if !al.hasResolved(CommitScope + "|" + CommitCompensationFailed + "|vrf[vrf-a]") {
		t.Fatalf("同一对象重新收敛后应消警: %+v", al.resolved)
	}
}

// 删表读回未收敛（NAT 用过的表）→ **不阻断提交**，但必须留告警痕（R86-10）。
func TestVrfDeleteNotRemovedDeferredWithAlarm(t *testing.T) {
	calls := &[]string{}
	net := errNet{recNet: recNet{calls: calls}, delErr: map[string]error{"vrf-gone": ErrVrfNotRemoved}}
	ap, al := newErrApplier(net)
	err := ap.Apply(context.Background(), model.Config{Vrfs: []model.Vrf{{Name: "vrf-gone"}}}, model.Config{})
	if err != nil {
		t.Fatalf("删表未收敛应按「延后收敛」处理（提交成功）: %v", err)
	}
	if !al.hasRaised(CommitScope + "|" + SeverityWarning + "|" + CommitVrfDeleteDeferred + "|vrf-gone") {
		t.Fatalf("删表延后必须留告警痕: %+v", al.raised)
	}
}

// 删表**真错误**（不是「表还在」）照旧硬失败，并按旧配置复原（复合操作的失败补偿）。
func TestVrfDeleteHardErrorStillFailsAndRestores(t *testing.T) {
	calls := &[]string{}
	net := errNet{recNet: recNet{calls: calls}, delErr: map[string]error{"vrf-gone": errFakeDel}}
	ap, al := newErrApplier(net)
	err := ap.Apply(context.Background(), model.Config{Vrfs: []model.Vrf{{Name: "vrf-gone"}}}, model.Config{})
	if err == nil || !strings.Contains(err.Error(), "下发失败 del-vrf[vrf-gone]") {
		t.Fatalf("真错误必须冒泡: %v", err)
	}
	// 删除是复合操作（清地址→解绑→删表→读回）：失败后按旧配置把该 VRF 复原
	if !hasCall(*calls, "vrf:vrf-gone") {
		t.Fatalf("删表失败后应按旧配置复原该 VRF: %v", *calls)
	}
	if len(al.raised) != 0 {
		t.Fatalf("复原成功不应报残渣告警: %+v", al.raised)
	}
}

// 复原也失败 → 残渣告警（error 级，source 为 del-vrf[<名>]）。
func TestVrfDeleteRestoreFailureRaisesAlarm(t *testing.T) {
	calls := &[]string{}
	net := errNet{
		recNet: recNet{calls: calls},
		delErr: map[string]error{"vrf-gone": errFakeDel},
		vrfErr: map[string]error{"vrf-gone": errFakeDel},
	}
	ap, al := newErrApplier(net)
	err := ap.Apply(context.Background(), model.Config{Vrfs: []model.Vrf{{Name: "vrf-gone"}}}, model.Config{})
	if err == nil || !strings.Contains(err.Error(), "补偿失败 del-vrf[vrf-gone]") {
		t.Fatalf("复原失败应报补偿失败: %v", err)
	}
	if !al.hasRaised(CommitScope + "|" + SeverityError + "|" + CommitCompensationFailed + "|del-vrf[vrf-gone]") {
		t.Fatalf("复原失败必须进告警: %+v", al.raised)
	}
}

// 无告警落点时不得 panic（缺省丢弃，仅日志）——部署未注入 sink 的开发态仍要能跑。
func TestCommitAlarmSinkOptional(t *testing.T) {
	calls := &[]string{}
	net := errNet{recNet: recNet{calls: calls}, delErr: map[string]error{"vrf-gone": ErrVrfNotRemoved}}
	ap := NewApplier(net, recCompute{calls: calls}, recContainer{calls: calls})
	if err := ap.Apply(context.Background(), model.Config{Vrfs: []model.Vrf{{Name: "vrf-gone"}}}, model.Config{}); err != nil {
		t.Fatalf("延后收敛不依赖告警落点: %v", err)
	}
}
