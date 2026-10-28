package netkernel

// round51 Q2：内核数据面 `DeleteL3Interface` 曾**忽略 vrfName**——按「master 是不是 VRF」解绑
// （不核对是不是**本** VRF），随后又对带点的设备无条件 `ip link del`。
//
// 缺陷形态（静态评审定位）：同一台设备 `ens224.100` 从 vs-a 改挂 vs-b（两台都还在声明里），
// vs-b 先被 ApplyVRF（B 先声明的计划序）装好归属/地址，随后 vs-a 按旧声明回收这条接口
// （迁移形态下旧 VRF 的差分会下发这次调用，见 apply.go 的 del-l3-if）⇒ 新归属被摘、新地址被清、
// 设备被删：配置说在 vs-b，内核里设备/地址/归属全没了，而提交报成功。与 VPP 侧 P1、以及
// #193/#196/#361 同一族。
//
// 判据是内核**终态**（master/地址/设备三样），不是命令序：本用例用能改状态的假内核驱动真实的
// ApplyVRF 与 DeleteL3Interface（provider 契约），断言设备仍在 vs-b 名下且地址完好。
//
// ⚠️ 可复现性如实说明：在**计划层**，同一 key 的 A→B 迁移已由 P1（决策 #511）不再下发 del-l3-if，
// 故这条路径在生产上由 P1 拦住；本用例钉的是 `DeleteL3Interface` 自身的契约（签名给了 vrfName，
// 就只该动**本** VRF 的那一份——任何调用方/调用序误用时都不得越权动别人的归属）。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// TestKernelDeleteL3InterfaceKeepsDeviceTakenOverByAnotherVrf A→B 迁移（B 先装好）后，旧 VRF
// 的回收必须是空操作：设备仍在、归属仍是 vs-b、地址仍在。
func TestKernelDeleteL3InterfaceKeepsDeviceTakenOverByAnotherVrf(t *testing.T) {
	ctx := context.Background()
	h := newDHCPFakeHost()
	h.addLink("ens224", "")   // l3-interface 的载体基口（物理口）
	h.addLink("vs-a", "vrf")  // 旧属主：设备仍在场（交换机没被删，只是接口挪走了）
	p := New(fixR51Kernel{h}) // vs-b 的 VRF 设备由 ApplyVRF 自己建（假内核按真语义登记类型）

	li := model.L3Interface{Interface: "ens224", Vlan: 100, Addresses: []string{"10.0.0.1/24"}}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-b", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF(vs-b): %v", err)
	}
	dev := l3DeviceName(li) // ens224.100
	if row := h.link(dev); row == nil || row.master != "vs-b" {
		t.Fatalf("用例前提：vs-b 的 ApplyVRF 应把 %s 挂到自己名下并配地址：%+v", dev, row)
	}

	// 旧属主按旧声明回收同一条接口（迁移形态下旧 VRF 的差分会下发的那次调用）。
	if err := p.DeleteL3Interface(ctx, "vs-a", li); err != nil {
		t.Fatalf("DeleteL3Interface(vs-a): %v", err)
	}

	row := h.link(dev)
	if row == nil {
		t.Fatalf("设备已被别的 VRF 接管，旧属主的回收不得删除它；实际已被 ip link del 掉：\n%s",
			strings.Join(h.lastCmds(), "\n"))
	}
	if row.master != "vs-b" {
		t.Fatalf("归属应保持 vs-b（旧属主不得摘掉接管方的 master）：%q\n%s",
			row.master, strings.Join(h.lastCmds(), "\n"))
	}
	if len(row.addrs) != 1 || row.addrs[0] != "10.0.0.1/24" {
		t.Fatalf("接管方下发的地址应保留（旧属主不得清掉它）：%v\n%s",
			row.addrs, strings.Join(h.lastCmds(), "\n"))
	}
	// 负面断言：不得对已被接管的设备下发任何改写命令（解绑/清地址/删设备）。
	for _, cmd := range h.lastCmds() {
		if !strings.Contains(cmd, dev) {
			continue
		}
		if strings.HasPrefix(cmd, "ip addr del") || strings.HasPrefix(cmd, "ip link del") ||
			strings.Contains(cmd, "nomaster") {
			t.Fatalf("不得对已被接管的设备下发 %q：\n%s", cmd, strings.Join(h.lastCmds(), "\n"))
		}
	}
}

// TestKernelDeleteVRFKeepsDottedNonVlanMember 兄弟判据（DeleteVRF 的成员清理）：只对**产品的
// vlan 子接口**删设备——名字带点但不是 vlan 的设备（如操作者名为 `lte.1` 的 bond，内核数据面的
// bond 设备名就是声明名本身）只清地址、只解归属，设备留给它的属主。
func TestKernelDeleteVRFKeepsDottedNonVlanMember(t *testing.T) {
	ctx := context.Background()
	h := newDHCPFakeHost()
	h.addLink("vs-l3", "vrf")
	h.addLink("lte.1", "bond", "10.0.0.1/24") // 名字带点、类型是 bond（不是产品的子接口）
	h.link("lte.1").master = "vs-l3"
	h.addLink("ens224.100", "vlan", "10.10.0.1/24") // 对照：真正的产品子接口照旧回收
	h.link("ens224.100").master = "vs-l3"

	p := New(fixR51Kernel{h})
	if err := p.DeleteVRF(ctx, "vs-l3"); err != nil {
		t.Fatalf("DeleteVRF: %v", err)
	}
	row := h.link("lte.1")
	if row == nil {
		t.Fatalf("名字带点但不是 vlan 子接口的设备不得被删（配置可能仍声明它）：\n%s",
			strings.Join(h.lastCmds(), "\n"))
	}
	if row.master != "" {
		t.Fatalf("成员口的归属仍应被摘除（设备交还宿主）：%q", row.master)
	}
	if len(row.addrs) != 0 {
		t.Fatalf("成员口地址仍应清理：%v", row.addrs)
	}
	if h.link("ens224.100") != nil {
		t.Fatalf("产品的 vlan 子接口应被回收：\n%s", strings.Join(h.lastCmds(), "\n"))
	}
}
