package orchestrator

// 决策 #100（发现 #8）：接口声明了 DPDK 单网卡覆盖项、但该口还没进数据面时，
// 提交必须**延后收敛**而不是整体回滚——否则「先声明端口、绑定后再提交」这条被支持的
// 流程会死锁：提交要端口先在 VPP 里，端口进 VPP 要 startup.conf 先更新（`request vpp restart`），
// 而重启要 committed 已落库。
//
// 判据取**配置意图**（vpp.dpdk.per-dev 的接口集合），故真正的错口名仍照旧失败。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// pendingNet 让指定接口在下发时报「接口在 VPP 中不存在」，其余行为同 recNet。
type pendingNet struct {
	recNet
	missing map[string]bool
}

func (n pendingNet) ApplyInterface(_ context.Context, iface model.InterfaceConfig) error {
	*n.calls = append(*n.calls, "iface:"+iface.Name)
	if n.missing[iface.Name] {
		return fmt.Errorf("%w: %s", ErrIfaceUnavailable, iface.Name)
	}
	if n.failOn == "iface:"+iface.Name {
		return fmt.Errorf("模拟失败: iface:%s", iface.Name)
	}
	return nil
}

func dpdkCfg(iface string, managed bool) model.Config {
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: iface}}}
	if managed {
		cfg.Vpp = &model.VppConfig{DPDK: &model.VppDPDK{PerDev: []model.VppDevOverride{{Interface: iface}}}}
	}
	return cfg
}

// 已声明为 DPDK 口的接口尚不在数据面 → 不阻断提交，并给出可读告警。
func TestApplyDefersIfaceDeclaredAsDPDK(t *testing.T) {
	calls := &[]string{}
	var warns []string
	a := NewApplier(pendingNet{recNet{calls: calls}, map[string]bool{"ens224": true}},
		recCompute{calls: calls}, recContainer{calls: calls},
		WithWarn(func(msg string) { warns = append(warns, msg) }))

	if err := a.Apply(context.Background(), model.Config{}, dpdkCfg("ens224", true)); err != nil {
		t.Fatalf("已声明为 DPDK 口的接口未进数据面时应延后收敛，实际失败: %v", err)
	}
	if !containsStr(calls, "iface:ens224") {
		t.Fatalf("仍应尝试下发该接口: %v", *calls)
	}
	joined := strings.Join(warns, " | ")
	if !strings.Contains(joined, "ens224") {
		t.Fatalf("延后必须有可读告警（否则就是静默失败）: %q", joined)
	}
	// 告警要指向「下一步做什么」，而不是只说「成功了」
	if !strings.Contains(joined, "重启") && !strings.Contains(joined, "数据面") {
		t.Fatalf("告警应说明如何完成收敛: %q", joined)
	}
}

// 未声明为 DPDK 的口：同样的失败**照旧硬失败**（不能把真错误吞掉）。
func TestApplyStillFailsForUndeclaredIface(t *testing.T) {
	calls := &[]string{}
	a := NewApplier(pendingNet{recNet{calls: calls}, map[string]bool{"ens999": true}},
		recCompute{calls: calls}, recContainer{calls: calls},
		WithWarn(func(string) {}))

	err := a.Apply(context.Background(), model.Config{}, dpdkCfg("ens999", false))
	if err == nil {
		t.Fatal("未声明为 DPDK 口的接口缺失必须照旧失败（否则错口名会被静默吞掉）")
	}
	if !strings.Contains(err.Error(), "ens999") {
		t.Fatalf("错误应指明接口: %v", err)
	}
}

// 其它失败（非「接口不在数据面」）不得被延后逻辑放过。
func TestApplyDoesNotDeferOtherFailures(t *testing.T) {
	calls := &[]string{}
	a := NewApplier(pendingNet{recNet{calls: calls, failOn: "iface:ens224"}, map[string]bool{}},
		recCompute{calls: calls}, recContainer{calls: calls},
		WithWarn(func(string) {}))
	if err := a.Apply(context.Background(), model.Config{}, dpdkCfg("ens224", true)); err == nil {
		t.Fatal("非接口缺失类失败必须冒泡（延后只针对「接口不在数据面」）")
	}
}

func containsStr(list *[]string, want string) bool {
	for _, s := range *list {
		if s == want {
			return true
		}
	}
	return false
}
