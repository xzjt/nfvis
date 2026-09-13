package network

import (
	"context"
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
