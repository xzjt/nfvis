package network

// FR-NET-003（决策 #73）：物理业务口链路状态告警。
// 此前仅有 vhost-user 的 VNF_PORT_DOWN，物理口 link down/up 无任何告警。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// linkFixture 造一个含物理口的配置与假 L2（names 由调用方给定状态）。
func linkFixture(t *testing.T, ifaces []model.InterfaceConfig, names map[uint32]SwIfInfo) (*L2Network, *AlarmStore) {
	t.Helper()
	f := &fakeL2{ifaces: map[string]uint32{}, names: names}
	n := NewL2Network(nil, nil)
	n.l2 = NewL2Provider(f)
	alarms := NewAlarmStore()
	n.SetAlarms(alarms)
	return n, alarms
}

func enabledIface(name string) model.InterfaceConfig {
	v := true
	return model.InterfaceConfig{Name: name, Enabled: &v, MTU: 9000}
}

// admin+link 均 up → 不告警；消警（若有）。
func TestInterfaceLinkUpNoAlarm(t *testing.T) {
	cfg := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens192")}}
	n, alarms := linkFixture(t, cfg.Interfaces, map[uint32]SwIfInfo{
		1: {Name: "ens192", AdminUp: true, LinkUp: true},
	})
	if errs := n.CheckInterfaceLinks(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("不应有错误: %v", errs)
	}
	if got := alarms.List("active"); len(got) != 0 {
		t.Fatalf("链路正常不应告警: %+v", got)
	}
}

// link down（admin up）→ warning 告警；恢复后自动消警。
func TestInterfaceLinkDownAlarmAndResolve(t *testing.T) {
	cfg := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens192")}}
	names := map[uint32]SwIfInfo{1: {Name: "ens192", AdminUp: true, LinkUp: false}}
	n, alarms := linkFixture(t, cfg.Interfaces, names)

	if errs := n.CheckInterfaceLinks(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("不应有错误: %v", errs)
	}
	active := alarms.List("active")
	if len(active) != 1 {
		t.Fatalf("应产生 1 条告警: %+v", active)
	}
	a := active[0]
	if a.Code != AlarmIfaceLinkDown || a.Severity != SeverityWarning || a.Source != "ens192" {
		t.Fatalf("告警内容不符: %+v", a)
	}
	if !strings.Contains(a.Message, "链路 down") {
		t.Fatalf("告警消息应说明原因: %q", a.Message)
	}

	// 恢复 link → 消警
	names[1] = SwIfInfo{Name: "ens192", AdminUp: true, LinkUp: true}
	n.CheckInterfaceLinks(context.Background(), cfg)
	if got := alarms.List("active"); len(got) != 0 {
		t.Fatalf("恢复后应消警: %+v", got)
	}
}

// 管理态未启用也视为未就绪（消息区分管理态）。
func TestInterfaceAdminDownAlarm(t *testing.T) {
	cfg := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens224")}}
	n, alarms := linkFixture(t, cfg.Interfaces, map[uint32]SwIfInfo{
		2: {Name: "ens224", AdminUp: false, LinkUp: true},
	})
	n.CheckInterfaceLinks(context.Background(), cfg)
	active := alarms.List("active")
	if len(active) != 1 || !strings.Contains(active[0].Message, "管理态未启用") {
		t.Fatalf("应报管理态未启用: %+v", active)
	}
}

// 显式 disable 的接口是用户意图 → 不告警。
func TestInterfaceDisabledNoAlarm(t *testing.T) {
	v := false
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192", Enabled: &v}}}
	n, alarms := linkFixture(t, cfg.Interfaces, map[uint32]SwIfInfo{
		1: {Name: "ens192", AdminUp: false, LinkUp: false},
	})
	n.CheckInterfaceLinks(context.Background(), cfg)
	if got := alarms.List("active"); len(got) != 0 {
		t.Fatalf("显式禁用不应告警: %+v", got)
	}
}

// VPP 中不存在该口 → 交由恢复收敛告警，此处不重复。
func TestInterfaceMissingNotAlarmedHere(t *testing.T) {
	cfg := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens999")}}
	n, alarms := linkFixture(t, cfg.Interfaces, map[uint32]SwIfInfo{
		1: {Name: "ens192", AdminUp: true, LinkUp: true},
	})
	n.CheckInterfaceLinks(context.Background(), cfg)
	if got := alarms.List("active"); len(got) != 0 {
		t.Fatalf("缺失接口不应在此处告警（由 RECOVERY_IFACE_MISSING 负责）: %+v", got)
	}
}
