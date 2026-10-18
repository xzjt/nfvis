package api

// R39-2（round39 真机）：`set virtual-switches <n> l3-interface <if> ip address <p>` 在交换机
// **尚未声明**时只建了同名 VRF 条目、没建交换机条目 —— 留下「半个对象」：
//   - `show virtual-switches` 不列它、`show virtual-switches <n>` 报「在数据面中不存在」；
//   - `show vrfs` / `display set` 里看得见，数据面真的建了表并配了地址；
//   - CLI 再无语句能删它（`delete virtual-switches <n>` / `… type l3` 都报「无匹配配置」），
//     只能整份 REST 候选文档改写（真机实测就是靠这个绕出来的）。
//
// 修法：l3VrfOf(create=true) 一并建出同名交换机（type=l3），与 `set … type l3` 别名的成对
// 关系一致；本文件钉住「一次 set 即得完整对象」与「两个删除形态都能收掉它」。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 一次 set 即建出「交换机(type=l3) + 同名 VRF + l3-interface 地址」三件，不留半对象。
func TestL3InterfaceAutoCreatesSwitch(t *testing.T) {
	var cfg model.Config
	// 先声明接口（校验要求 l3-interface 指向已声明的物理口）——本用例只关心「交换机是否被建出」。
	if err := applyStatement(&cfg, splitFieldsQuoted("set interfaces ens192 description lab")[1:]); err != nil {
		t.Fatalf("fixture 失败: %v", err)
	}
	if err := applyStatement(&cfg, splitFieldsQuoted(
		"set virtual-switches vs-z l3-interface ens192 ip address 192.168.96.1/24")[1:]); err != nil {
		t.Fatalf("set 应成功: %v", err)
	}
	if len(cfg.VirtualSwitches) != 1 || cfg.VirtualSwitches[0].Name != "vs-z" || cfg.VirtualSwitches[0].Type != "l3" {
		t.Fatalf("应一并建出 type=l3 的交换机条目，实际: %+v", cfg.VirtualSwitches)
	}
	if len(cfg.Vrfs) != 1 || cfg.Vrfs[0].Name != "vs-z" {
		t.Fatalf("同名 VRF 条目应存在: %+v", cfg.Vrfs)
	}
	if len(cfg.Vrfs[0].L3Interfaces) != 1 || cfg.Vrfs[0].L3Interfaces[0].Interface != "ens192" {
		t.Fatalf("l3-interface 条目应存在: %+v", cfg.Vrfs[0].L3Interfaces)
	}
	// 模型校验必须放行（半对象形态此前是「能提交、但删不掉」）。
	if errs := model.Validate(cfg); len(errs) != 0 {
		t.Fatalf("建出的配置应通过校验，实际: %v", errs)
	}
	// 两个删除形态都能收掉它（此前都报「无匹配配置」）。
	for _, del := range []string{"delete virtual-switches vs-z", "delete virtual-switches vs-z type l3"} {
		c := cfg
		c.VirtualSwitches = append([]model.VirtualSwitch{}, cfg.VirtualSwitches...)
		c.Vrfs = append([]model.Vrf{}, cfg.Vrfs...)
		if err := deleteStatement(&c, splitFieldsQuoted(del)[1:]); err != nil {
			t.Fatalf("%q 应能删掉自动建出的交换机: %v", del, err)
		}
		if len(c.VirtualSwitches) != 0 || len(c.Vrfs) != 0 {
			t.Fatalf("%q 后应清空交换机与同名 VRF，实际 vs=%+v vrfs=%+v", del, c.VirtualSwitches, c.Vrfs)
		}
	}
}

// 已存在同名 L2 交换机时**不改它的 type**：让模型校验如实拒绝（不在别名层猜操作者意图）。
func TestL3InterfaceDoesNotRetagExistingL2(t *testing.T) {
	var cfg model.Config
	if err := applyStatement(&cfg, splitFieldsQuoted("set interfaces ens192 description lab")[1:]); err != nil {
		t.Fatalf("fixture 失败: %v", err)
	}
	if err := applyStatement(&cfg, splitFieldsQuoted("set virtual-switches vs-l2 type l2")[1:]); err != nil {
		t.Fatalf("fixture 失败: %v", err)
	}
	if err := applyStatement(&cfg, splitFieldsQuoted(
		"set virtual-switches vs-l2 l3-interface ens192 ip address 10.0.0.1/24")[1:]); err != nil {
		t.Fatalf("语句本身应被受理（校验在提交期）: %v", err)
	}
	if cfg.VirtualSwitches[0].Type != "l2" {
		t.Fatalf("不应把既有 L2 交换机改成 l3，实际: %q", cfg.VirtualSwitches[0].Type)
	}
	// 如实登记（round39 观察）：模型**不禁止**「L2 交换机 + 同名 VRF」这一形态——校验只要求
	// type=l3 有同名 VRF，反向没有约束。它与 L2 交换机的派生网关域（vr-<名>）不是同一张表，
	// 故不会互相覆盖；此处只钉住「别名层不擅自改 type」，是否要拒绝该形态留给口径定裁。
	if errs := model.Validate(cfg); len(errs) != 0 {
		for _, e := range errs {
			if strings.Contains(e.Error(), "type") {
				t.Fatalf("L2 + 同名 VRF 不该被类型校验拒绝: %v", errs)
			}
		}
	}
}
