package model

import (
	"strings"
	"testing"
)

func TestDiffAddChangeDelete(t *testing.T) {
	old := Config{
		System:          &SystemConfig{Hostname: "old-node"},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-a", Type: "l2", VlanAccess: 100}},
	}
	newCfg := Config{
		System:          &SystemConfig{Hostname: "new-node"},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-a", Type: "l2", VlanAccess: 200}},
		Vrfs:            []Vrf{{Name: "vs-b", Routes: []Route{{Prefix: "0.0.0.0/0", NextHop: "10.0.0.1"}}}},
	}

	got := Diff(old, newCfg)
	for _, want := range []string{
		"[edit system]",
		"-   hostname old-node;",
		"+   hostname new-node;",
		"[edit virtual-switches vs-a]",
		"-   vlan-access 100;",
		"+   vlan-access 200;",
		"[edit vrfs vs-b routes 0.0.0.0/0]",
		"+   next-hop 10.0.0.1;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff 输出缺少 %q，实际:\n%s", want, got)
		}
	}

	// 删除整个对象：交换机 vs-a 消失
	got2 := Diff(newCfg, Config{System: newCfg.System})
	if !strings.Contains(got2, "-   vlan-access 200;") {
		t.Errorf("删除对象应产生 - 行:\n%s", got2)
	}
}

func TestDiffNoDifference(t *testing.T) {
	c := validBase()
	if got := Diff(c, c); got != "" {
		t.Errorf("相同配置应无差异，实际:\n%s", got)
	}
}

func TestDiffDeterministicOrdering(t *testing.T) {
	a := validBase()
	b := validBase()
	b.VirtualMachineFunctions[0].Memory.SizeMB = 4096
	b.Acls[0].Rules[0].Action = "deny"
	g1 := Diff(a, b)
	g2 := Diff(a, b)
	if g1 != g2 {
		t.Errorf("diff 必须确定性:\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", g1, g2)
	}
}

func TestDiffScalarListElements(t *testing.T) {
	old := Config{System: &SystemConfig{DNSServers: []string{"8.8.8.8"}}}
	newCfg := Config{System: &SystemConfig{DNSServers: []string{"8.8.8.8", "1.1.1.1"}}}
	got := Diff(old, newCfg)
	if !strings.Contains(got, "+   dns-servers 1.1.1.1;") {
		t.Errorf("标量列表新增元素应为 + 行:\n%s", got)
	}
}

func TestFlattenNamedArrays(t *testing.T) {
	c := validBase()
	stmts := Flatten(c)
	byPath := map[string]string{}
	for _, s := range stmts {
		byPath[strings.Join(s.Path, " ")] = s.Value
	}
	for path, want := range map[string]string{
		"virtual-switches vs-app type":               "l2",
		"virtual-machine-functions fw-vm image":      "ubuntu22-vm",
		"virtual-machine-functions fw-vm vcpu count": "4",
		"container-functions sbc-ct1 image":          "alpine-ct",
		"resource-pools hugepages 1G count":          "32",
		"vpp cpu main-core":                          "4",
		"acls acl-web rules 10 action":               "permit",
	} {
		if got, ok := byPath[path]; !ok || got != want {
			t.Errorf("flatten %q = %q (存在=%v)，期望 %q", path, got, ok, want)
		}
	}
}
