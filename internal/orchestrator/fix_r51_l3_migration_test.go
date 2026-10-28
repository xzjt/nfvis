package orchestrator

// round51 P1：l3-interface 在**两台都还在声明**的 L3 交换机之间迁移时，旧交换机不得把它回收。
//
// 缺陷形态（静态评审定位，成因与真机现象同族）：plan 的删除段按「本 VRF 的旧声明 vs 本 VRF 的
// 新声明」求差（removedL3Ifaces），接口 X 从 vs-a 挪到 vs-b（两台都还在）时 vs-a 的差分会把 X
// 判成「整条消失」并下发 del-l3-if → DeleteL3Interface 清地址、解绑 ACL/伴随 macip、把口移回
// 默认表、删掉 `<口>.<vlan>` 子接口；而 vs-b 的 ApplyVRF 已经把 X 置入 b 表——谁先谁后由
// new.Vrfs 的**声明序**决定，于是「b 先声明」必现（配置说在 b、数据面在默认表且无地址），
// 「a 先声明」侥幸正常：同一份配置两个数据面结果。
//
// 判据必须是**最终归属**（不只是调用序列）：本用例的数据面假件因此记录「接口当前归属哪台
// VRF、带什么地址」，而不是只记调用过哪些方法——只看调用序列的假件会把缺陷喂绿（本仓常见
// 失效模式：假件把真实缺陷藏起来）。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fixR51L3Net 记录下发调用序（内嵌既有 recNet）**并**维护一份最小的数据面归属模型：
//   - ApplyVRF：声明里的每条 l3-interface 归属该 VRF、带声明里的地址（与真实现同口径——
//     只对**本声明里**的接口动手，声明里没有的接口不动）；
//   - DeleteL3Interface：清地址、把接口移回默认表（空归属）。
type fixR51L3Net struct {
	recNet
	where map[string]string // 接口键 → VRF 名（"" = 默认表）
	addrs map[string]string // 接口键 → 地址（逗号连接）
}

func newFixR51L3Net(calls *[]string) *fixR51L3Net {
	return &fixR51L3Net{
		recNet: recNet{calls: calls},
		where:  map[string]string{},
		addrs:  map[string]string{},
	}
}

func (n *fixR51L3Net) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	for _, li := range vrf.L3Interfaces {
		k := l3IfaceKeyOf(li)
		n.where[k] = vrf.Name
		n.addrs[k] = strings.Join(li.Addresses, ",")
	}
	return n.recNet.record("vrf:" + vrf.Name)
}

func (n *fixR51L3Net) DeleteL3Interface(ctx context.Context, vrfName string, li model.L3Interface) error {
	k := l3IfaceKeyOf(li)
	n.addrs[k] = ""
	n.where[k] = "" // 移回默认表
	return n.recNet.recordDel(ctx, "del-l3-if:"+vrfName+"/"+k)
}

// migrationCase 一次迁移用例：旧配置里 X 在 vs-a，新配置里 X 在 vs-b（两台都在），
// newVrfs 给出新配置的声明序（这正是缺陷的触发变量）。
func migrationCase(oldIf model.L3Interface, newVrfs []model.Vrf) (model.Config, model.Config) {
	oldCfg := model.Config{Vrfs: []model.Vrf{
		{Name: "vs-a", L3Interfaces: []model.L3Interface{oldIf}},
		{Name: "vs-b"},
	}}
	return oldCfg, model.Config{Vrfs: newVrfs}
}

// TestApplyL3IfaceMigrationKeepsInterfaceInTarget 迁移的两种声明序都必须把接口留在目标
// 交换机上并带其地址：这是本缺陷的**判据**（旧实现在「目标先声明」时把接口打回默认表且丢地址）。
func TestApplyL3IfaceMigrationKeepsInterfaceInTarget(t *testing.T) {
	oldIf := model.L3Interface{Interface: "ens224", Addresses: []string{"10.0.1.1/24"}}
	newA := model.Vrf{Name: "vs-a"}
	newB := model.Vrf{Name: "vs-b", L3Interfaces: []model.L3Interface{oldIf}}
	for _, tc := range []struct {
		name   string
		newCfg []model.Vrf
	}{
		{"目标交换机声明在后（vs-a 在前）", []model.Vrf{newA, newB}},
		{"目标交换机声明在前（vs-b 在前）", []model.Vrf{newB, newA}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := &[]string{}
			net := newFixR51L3Net(calls)
			ap := NewApplier(net, recCompute{calls: calls}, recContainer{calls: calls})
			oldCfg, newCfg := migrationCase(oldIf, tc.newCfg)
			if err := ap.Apply(context.Background(), oldCfg, newCfg); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if net.where["ens224"] != "vs-b" || net.addrs["ens224"] != "10.0.1.1/24" {
				t.Fatalf("迁移后接口应归属 vs-b 并带其地址，实际 归属=%q 地址=%q\n调用序：%v",
					net.where["ens224"], net.addrs["ens224"], *calls)
			}
			if hasCall(*calls, "del-l3-if:vs-a/ens224") {
				t.Fatalf("仍被 vs-b 声明的接口不得被 vs-a 的差分回收\n调用序：%v", *calls)
			}
		})
	}
}

// TestApplyL3IfaceMigrationReleasesOldAclBeforeTarget 旧声明的 ACL 绑定必须在新归属的
// ApplyVRF **之前**释放：
//   - 不释放 ⇒ 旧交换机的 deny ACL 继续拦在新归属的口上（#361/round142 的缺陷形态）；
//   - 顺序反了（排在新归属之后）⇒ 新归属刚下发的绑定被一起撤掉（BindIndex 的空绑定=解绑任意 ACL）。
func TestApplyL3IfaceMigrationReleasesOldAclBeforeTarget(t *testing.T) {
	oldIf := model.L3Interface{Interface: "ens224", Addresses: []string{"10.0.1.1/24"}, AclIn: "web"}
	newB := model.Vrf{Name: "vs-b", L3Interfaces: []model.L3Interface{oldIf}}
	for _, tc := range []struct {
		name   string
		newCfg []model.Vrf
	}{
		{"目标交换机声明在后", []model.Vrf{{Name: "vs-a"}, newB}},
		{"目标交换机声明在前", []model.Vrf{newB, {Name: "vs-a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := &[]string{}
			net := newFixR51L3Net(calls)
			ap := NewApplier(net, recCompute{calls: calls}, recContainer{calls: calls})
			oldCfg, newCfg := migrationCase(oldIf, tc.newCfg)
			if err := ap.Apply(context.Background(), oldCfg, newCfg); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if net.where["ens224"] != "vs-b" || net.addrs["ens224"] != "10.0.1.1/24" {
				t.Fatalf("迁移后接口应归属 vs-b 并带其地址，实际 归属=%q 地址=%q\n调用序：%v",
					net.where["ens224"], net.addrs["ens224"], *calls)
			}
			unbind := indexOf(*calls, "l3-acl-unbind:vs-a/ens224")
			target := indexOf(*calls, "vrf:vs-b")
			if unbind < 0 {
				t.Fatalf("旧声明的 ACL 绑定必须被释放（接口已迁走，旧绑定不该继续拦）\n调用序：%v", *calls)
			}
			if target < 0 || unbind > target {
				t.Fatalf("解绑必须排在新归属的 ApplyVRF 之前（unbind=%d target=%d）\n调用序：%v",
					unbind, target, *calls)
			}
		})
	}
}
