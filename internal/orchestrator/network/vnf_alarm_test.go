package network

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// FR-NET-023：vNIC 断连 → 告警；恢复 → 消警；运行态可查。
func TestCheckVnfPortsAlarmsOnLinkDown(t *testing.T) {
	f := newFakeVhost()
	net := NewL2Network(nil, nil)
	net.SetVhostUser(vhostProvider(f))
	alarms := NewAlarmStore()
	net.SetAlarms(alarms)

	// 下发 vNIC 接入（admin up，但无客户端 → link down）。
	if err := net.ApplyVnfInterface(context.Background(), vnfPortFor("fw-vm", "eth0", "/s")); err != nil {
		t.Fatalf("ApplyVnfInterface: %v", err)
	}
	cfg := vmCfg("fw-vm", "eth0")

	if errs := net.CheckVnfPorts(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("状态查询不应报错: %v", errs)
	}
	active := alarms.List("active")
	if len(active) != 1 || active[0].Code != AlarmVnfPortDown || active[0].Severity != SeverityWarning {
		t.Fatalf("应产生 VNF_PORT_DOWN warning 告警: %+v", active)
	}

	reports, err := net.VnfPorts(context.Background(), cfg)
	if err != nil || len(reports) != 1 || !reports[0].Exists || reports[0].Up {
		t.Fatalf("运行态应 exists=true up=false: %+v err=%v", reports, err)
	}

	// 模拟 QEMU 连接 → link up → 消警。
	idx := f.byName[orchestrator.VnfIfaceName("fw-vm", "eth0")]
	f.links[idx] = true
	net.CheckVnfPorts(context.Background(), cfg)
	if got := alarms.List("active"); len(got) != 0 {
		t.Fatalf("链路恢复后活动告警应为空: %+v", got)
	}
	reports, _ = net.VnfPorts(context.Background(), cfg)
	if !reports[0].Up {
		t.Fatalf("链路恢复后应 up: %+v", reports)
	}
}

func vnfPortFor(vm, iface, sock string) orchestrator.VnfPort {
	return orchestrator.VnfPort{VM: vm, Interface: iface, Type: "vhost-user", Socket: sock}
}

func vmCfg(vm, iface string) model.Config {
	return model.Config{VirtualMachineFunctions: []model.VMFunction{{
		Name: vm, Image: "img", VCPU: model.VMCpu{Count: 1},
		Memory:     model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		Interfaces: []model.VnfInterface{{Name: iface, Type: "vhost-user"}},
	}}}
}

// mergeVms 把多个单 VNF 配置合并（对账清警用例需要「配置里删掉一个对象」）。
func mergeVms(cfgs ...model.Config) model.Config {
	out := model.Config{}
	for _, c := range cfgs {
		out.VirtualMachineFunctions = append(out.VirtualMachineFunctions, c.VirtualMachineFunctions...)
	}
	return out
}

// addVhostIface 在假 VPP 里登记一个已接入的 vhost-user 接口（admin up，link 由参数给）。
func addVhostIface(f *fakeVhost, vm, iface string, linkUp bool) {
	idx := f.next + 1
	f.next = idx
	name := orchestrator.VnfIfaceName(vm, iface)
	f.byName[name] = idx
	f.names[idx] = name
	f.up[idx] = true
	f.links[idx] = linkUp
}

// vnfAlarmFixture 造一个带假 vhost 与告警表的 L2Network。
func vnfAlarmFixture(f *fakeVhost) (*L2Network, *AlarmStore) {
	net := NewL2Network(nil, nil)
	net.SetVhostUser(vhostProvider(f))
	alarms := NewAlarmStore()
	net.SetAlarms(alarms)
	return net, alarms
}

func activeSources(alarms *AlarmStore) map[string]bool {
	out := map[string]bool{}
	for _, a := range alarms.List(AlarmActive) {
		out[a.Source] = true
	}
	return out
}

// round86 缺陷 1：VNF 从配置删除后，其 VNF_PORT_DOWN 告警必须被对账清掉
// （此前检查只遍历当前配置里的对象，删掉的对象永远无人 Resolve）。
func TestCheckVnfPortsResolvesRemovedVnf(t *testing.T) {
	f := newFakeVhost()
	net, alarms := vnfAlarmFixture(f)
	addVhostIface(f, "vnf-a", "eth0", false)
	addVhostIface(f, "vnf-b", "eth0", false)

	if errs := net.CheckVnfPorts(context.Background(), mergeVms(vmCfg("vnf-a", "eth0"), vmCfg("vnf-b", "eth0"))); len(errs) != 0 {
		t.Fatalf("状态查询不应报错: %v", errs)
	}
	if got := activeSources(alarms); len(got) != 2 {
		t.Fatalf("前置：应 2 条活动告警，实际 %v", got)
	}

	// 配置里删掉 vnf-b（VPP 里的接口还在，甚至仍 link down）→ 其告警必须自消
	if errs := net.CheckVnfPorts(context.Background(), vmCfg("vnf-a", "eth0")); len(errs) != 0 {
		t.Fatalf("状态查询不应报错: %v", errs)
	}
	active := alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Source != "vnf-a/eth0" {
		t.Fatalf("只应保留 vnf-a 的告警，实际 %+v", active)
	}

	// 配置已无任何 VNF → 全部自消（空配置不是查询失败）
	if errs := net.CheckVnfPorts(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("状态查询不应报错: %v", errs)
	}
	if got := alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("配置清空后应无活动告警，实际 %+v", got)
	}
}

// 查询失败（VPP 未连接）时不对账：连源已不在配置的旧告警也保持原样，避免运行态未知时误清。
func TestCheckVnfPortsQueryFailureKeepsAlarm(t *testing.T) {
	f := newFakeVhost()
	net, alarms := vnfAlarmFixture(f)
	alarms.Raise(vnfScope, SeverityWarning, AlarmVnfPortDown, "旧告警", "vnf-gone/eth0")
	f.err = fmt.Errorf("VPP 未连接")

	errs := net.CheckVnfPorts(context.Background(), vmCfg("vnf-a", "eth0"))
	if len(errs) != 1 {
		t.Fatalf("查询失败应上报错误: %v", errs)
	}
	got := alarms.List(AlarmActive)
	if len(got) != 1 || got[0].Source != "vnf-gone/eth0" {
		t.Fatalf("查询失败时不应清警: %+v", got)
	}
}

// vhost 编排未装配（VPP 未接入）→ 不查询也不对账。
func TestCheckVnfPortsNoProviderKeepsAlarm(t *testing.T) {
	net := NewL2Network(nil, nil)
	alarms := NewAlarmStore()
	net.SetAlarms(alarms)
	alarms.Raise(vnfScope, SeverityWarning, AlarmVnfPortDown, "旧告警", "vnf-gone/eth0")

	if errs := net.CheckVnfPorts(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("未装配 vhost 时不应报错: %v", errs)
	}
	if got := alarms.List(AlarmActive); len(got) != 1 {
		t.Fatalf("未装配 vhost 时不应清警: %+v", got)
	}
}

// 决策 #470（round24 R24-D③）：告警原因文案如实化——运行中的域尚未包含该 vNIC（新增/改配
// 后需 stop+start）与「VM 未启动」是两种事实，旧文案「VM 未启动或已关机」在 VM 正运行时
// 与事实相反；两条分支都要给出 stop+start 的照做路径。告警码/作用域/自动消解不变。
func TestCheckVnfPortsAlarmReasonTexts(t *testing.T) {
	// ① link down 分支（接口在数据面、客户端未连接）。
	f := newFakeVhost()
	net, alarms := vnfAlarmFixture(f)
	if err := net.ApplyVnfInterface(context.Background(), vnfPortFor("vm-a", "eth0", "/s")); err != nil {
		t.Fatalf("ApplyVnfInterface: %v", err)
	}
	if errs := net.CheckVnfPorts(context.Background(), vmCfg("vm-a", "eth0")); len(errs) != 0 {
		t.Fatalf("状态查询不应报错: %v", errs)
	}
	active := alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmVnfPortDown {
		t.Fatalf("前置：应 1 条 VNF_PORT_DOWN 告警，实际 %+v", active)
	}
	msg := active[0].Message
	if !strings.Contains(msg, "接口 link down（客户端未连接：VM 未启动，或运行中的域尚未包含该 vNIC——新增/改配后需 stop+start 才会生效）") {
		t.Fatalf("link down 原因文案应如实并给出 stop+start 路径: %q", msg)
	}
	if strings.Contains(msg, "VM 未启动或已关机") {
		t.Fatalf("旧文案「VM 未启动或已关机」与运行中事实相反，不得再出现: %q", msg)
	}

	// ② 不存在分支（VPP 里没有对应 vhost-user 接口——刚新增/改配 vNIC、域未重建）。
	f2 := newFakeVhost()
	net2, alarms2 := vnfAlarmFixture(f2)
	if errs := net2.CheckVnfPorts(context.Background(), vmCfg("vm-b", "nic1")); len(errs) != 0 {
		t.Fatalf("状态查询不应报错: %v", errs)
	}
	active2 := alarms2.List(AlarmActive)
	if len(active2) != 1 || active2[0].Code != AlarmVnfPortDown {
		t.Fatalf("前置：应 1 条 VNF_PORT_DOWN 告警，实际 %+v", active2)
	}
	msg2 := active2[0].Message
	if !strings.Contains(msg2, "VPP 中不存在对应 vhost-user 接口（若刚新增/改配 vNIC：需 stop+start 重建域）") {
		t.Fatalf("不存在分支应给出重建域照做路径: %q", msg2)
	}

	// ③ 接口出现且 link up 后照常自动消解（文案改动不影响对账清警）。
	idx := f.byName[orchestrator.VnfIfaceName("vm-a", "eth0")]
	f.links[idx] = true
	if errs := net.CheckVnfPorts(context.Background(), vmCfg("vm-a", "eth0")); len(errs) != 0 {
		t.Fatalf("状态查询不应报错: %v", errs)
	}
	if got := alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("链路恢复后应消警（自动消解逻辑不动）: %+v", got)
	}
}
