package network

// FR-NET-003（决策 #73）：物理业务口链路状态告警。
// 此前仅有 vhost-user 的 VNF_PORT_DOWN，物理口 link down/up 无任何告警。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// linkFixture 造一个含物理口的配置与假 L2（names 由调用方给定状态）。
func linkFixture(t *testing.T, ifaces []model.InterfaceConfig, names map[uint32]SwIfInfo) (*L2Network, *AlarmStore) {
	t.Helper()
	return linkFixtureErr(t, names, nil)
}

// linkFixtureErr 同上，另可注入 SwInterfaceNames 查询失败（「查询失败不清警」单测）。
func linkFixtureErr(t *testing.T, names map[uint32]SwIfInfo, namesErr error) (*L2Network, *AlarmStore) {
	t.Helper()
	f := &fakeL2{ifaces: map[string]uint32{}, names: names, namesErr: namesErr}
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

// round86 缺陷 1：接口从 interfaces[] 删除后，其链路告警必须被对账清掉，其余对象不受影响。
func TestInterfaceRemovedFromConfigResolvesAlarm(t *testing.T) {
	both := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens192"), enabledIface("ens224")}}
	names := map[uint32]SwIfInfo{
		1: {Name: "ens192", AdminUp: true, LinkUp: false},
		2: {Name: "ens224", AdminUp: true, LinkUp: false},
	}
	n, alarms := linkFixture(t, both.Interfaces, names)
	n.CheckInterfaceLinks(context.Background(), both)
	if got := alarms.List("active"); len(got) != 2 {
		t.Fatalf("前置：应 2 条告警，实际 %+v", got)
	}

	// 从配置删除 ens224（VPP 里接口仍在且仍 down）→ 仅它被清
	only192 := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens192")}}
	n.CheckInterfaceLinks(context.Background(), only192)
	active := alarms.List("active")
	if len(active) != 1 || active[0].Source != "ens192" {
		t.Fatalf("只应保留 ens192 的告警，实际 %+v", active)
	}
}

// 显式 disable（用户意图）→ 其既有链路告警也应被对账清掉（语义：不告警）。
func TestInterfaceDisabledResolvesAlarm(t *testing.T) {
	cfg := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens224")}}
	n, alarms := linkFixture(t, cfg.Interfaces, map[uint32]SwIfInfo{
		2: {Name: "ens224", AdminUp: false, LinkUp: false},
	})
	n.CheckInterfaceLinks(context.Background(), cfg)
	if got := alarms.List("active"); len(got) != 1 {
		t.Fatalf("前置：应 1 条告警，实际 %+v", got)
	}

	v := false
	cfg.Interfaces[0].Enabled = &v
	n.CheckInterfaceLinks(context.Background(), cfg)
	if got := alarms.List("active"); len(got) != 0 {
		t.Fatalf("显式禁用后其链路告警应自消: %+v", got)
	}
}

// 接口状态查询失败 → 报错且不对账（不得因取不到运行态而清掉既有告警）。
func TestInterfaceQueryFailureKeepsAlarm(t *testing.T) {
	n, alarms := linkFixtureErr(t, nil, fmt.Errorf("VPP 未连接"))
	alarms.Raise(ifLinkScope, SeverityWarning, AlarmIfaceLinkDown, "旧告警", "ens224")
	cfg := model.Config{Interfaces: []model.InterfaceConfig{enabledIface("ens224")}}

	errs := n.CheckInterfaceLinks(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("查询失败应上报错误: %v", errs)
	}
	got := alarms.List("active")
	if len(got) != 1 || got[0].Source != "ens224" {
		t.Fatalf("查询失败时不应清警: %+v", got)
	}
}
