package api

// R2-15②：接口 detail 的风暴抑制 / 端口安全块在内核数据面下的渲染。
//
// 内核适配（netkernel.Provider 的 StormDataplane/PortSecDataplane）已按**消费方真正读的字段**
// 填事实（storm 的 Kinds、portsec 的 TagPresent/Tag/RuleCount/Bound）；这里钉住渲染口径：
//   - 内核侧没有 VPP 的"分类表 / 接口 L2 槽"，也没有 macip ACL 索引——不套 VPP 话术；
//   - 两套形态的期望规则条数不同（内核整段白名单一条 nftables 规则）；
//   - VPP 侧原样不回归（索引/分类表/期望条数照旧）。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakePortSecRuntime PortSecRuntime 的假实现（渲染测试注入用）。
type fakePortSecRuntime struct {
	dp network.PortSecDataplane
	ok bool
}

func (f fakePortSecRuntime) PortSecDataplane(context.Context, string) (network.PortSecDataplane, bool) {
	return f.dp, f.ok
}

// kernelFamilyFixture 声明内核数据面下的一台最小 L2 交换机 + 两个口：
// ens224 配风暴抑制（不要求交换机成员）、ens256 配端口安全（要求它是交换机静态成员口）。
func kernelFamilyFixture(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system dataplane kernel",
		"set interfaces ens224 description storm-port",
		"set interfaces ens256 description sec-port",
		"set virtual-switches vs-lan type l2",
		"set virtual-switches vs-lan ports 1 interface ens256",
		"set interfaces ens224 storm-control broadcast 8000",
		"set interfaces ens256 port-security mac aa:bb:cc:dd:ee:01",
		"commit",
		"exit",
	)
	return x
}

// detailEntryOf 从最近一次 show 的结构化输出里取出给定接口的条目。
func detailEntryOf(t *testing.T, x *cliExecutor, name string) map[string]any {
	t.Helper()
	top, ok := x.structured.(map[string]any)
	if !ok {
		t.Fatalf("结构化输出形状不符: %#v", x.structured)
	}
	for _, it := range top["interfaces"].([]any) {
		e, _ := it.(map[string]any)
		if e["name"] == name {
			return e
		}
	}
	t.Fatalf("结构化输出里没有接口 %s", name)
	return nil
}

// sameNumber 结构化字段直接是 Go 原生类型（未过 JSON），整数可能是 int/uint32/uint64——
// 一律按数值比较（`interface{}` 里 uint32(8000) 与 int(8000) 不相等，直接 != 会假红）。
func sameNumber(v any, want int64) bool {
	switch n := v.(type) {
	case int:
		return int64(n) == want
	case int64:
		return n == want
	case uint32:
		return int64(n) == want
	case uint64:
		return int64(n) == want
	case float64:
		return int64(n) == want
	}
	return false
}

func TestKernelStormDetailRendersTcFacts(t *testing.T) {
	x := kernelFamilyFixture(t)
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Attached: true,
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {
				PolicerPresent: true, CirKbps: 8000,
				// 内核适配给的如实说明（tc 只有 dropped/overlimits 两项，不映射三档）
				CountersReason: "内核 tc 的 police 计数为 dropped/overlimits，与 conform/exceed/violate 三档语义不对应，如实不映射",
			},
		},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	for _, want := range []string{
		"Storm control: 广播 8000 kbps",
		"广播 policer 在（cir 8000 kbps）",
		"限速落在内核 tc 入向过滤器上",
		"不可读（内核 tc", // 计数如实说明，不显示 0
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核侧 detail 应含 %q：\n%s", want, out)
		}
	}
	for _, bad := range []string{"未在数据面（未收敛）", "分类表"} {
		if strings.Contains(out, bad) {
			t.Fatalf("内核侧 detail 不应出现 VPP 话术 %q：\n%s", bad, out)
		}
	}
	rt := detailEntryOf(t, x, "ens224")["storm_control_runtime"].(map[string]any)
	if _, ok := rt["attached_l2_table"]; ok {
		t.Fatalf("内核侧不应发射 L2 槽表索引（恒 0 的假读数）：%v", rt)
	}
	bd := rt["kinds"].(map[string]any)["broadcast"].(map[string]any)
	if bd["policer_present"] != true || !sameNumber(bd["cir_kbps"], 8000) {
		t.Fatalf("结构化 kinds 应与内核事实一致：%v", bd)
	}
}

// VPP 侧不回归：分类表照常打印、L2 槽索引照常发射（内核分支不能把 VPP 的读数吞掉）。
func TestVPPStormDetailStillRendersClassifyTable(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 description storm-port",
		"set interfaces ens224 storm-control broadcast 8000",
		"commit",
		"exit",
	)
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Attached: true, AttachedL2Table: 24,
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {PolicerPresent: true, CirKbps: 8000,
				Table:    &network.StormTableInfo{Index: 24, Mask: "ffff", Sessions: 1},
				Counters: &network.StormCounters{ConformPackets: 5}},
		},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	if !strings.Contains(out, "分类表 #24") {
		t.Fatalf("VPP 侧应照常打印分类表：\n%s", out)
	}
	if strings.Contains(out, "内核 tc") {
		t.Fatalf("VPP 侧不应出现内核话术：\n%s", out)
	}
	rt := detailEntryOf(t, x, "ens224")["storm_control_runtime"].(map[string]any)
	if !sameNumber(rt["attached_l2_table"], 24) {
		t.Fatalf("VPP 侧应照常发射 attached_l2_table：%v", rt)
	}
}

func TestKernelPortSecDetailRendersKernelChain(t *testing.T) {
	x := kernelFamilyFixture(t)
	x.setPortSec(fakePortSecRuntime{ok: true, dp: network.PortSecDataplane{
		Available:  true,
		Reason:     "内核侧无 ACL 索引概念（白名单规则落在 nftables 链 ps_ens256 上）；白名单 1 条源 MAC；桥口学习已关闭（入向丢弃白名单外源 MAC）",
		Tag:        "内核 nftables 链 ps_ens256",
		TagPresent: true,
		RuleCount:  1,
		Bound:      true,
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens256 detail").Output
	for _, want := range []string{
		"内核 nftables 链 ps_ens256 在场（规则 1 条）",
		"白名单链已挂在接口上",
		"不读命中数（内核 nftables 白名单规则未挂 counter",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核侧端口安全 detail 应含 %q：\n%s", want, out)
		}
	}
	for _, bad := range []string{"ACL 未在数据面", "索引 0", "macip", "未绑定"} {
		if strings.Contains(out, bad) {
			t.Fatalf("内核侧不应出现 VPP 话术/矛盾文案 %q：\n%s", bad, out)
		}
	}
	rt := detailEntryOf(t, x, "ens256")["port_security_runtime"].(map[string]any)
	if !sameNumber(rt["expected_rule_count"], 1) || !sameNumber(rt["rule_count"], 1) || rt["bound"] != true {
		t.Fatalf("内核侧期望条数/实况条数/绑定应为 1/1/true：%v", rt)
	}
	if _, ok := rt["acl_index"]; ok {
		t.Fatalf("内核侧没有 ACL 索引这个概念，不应发射 acl_index：%v", rt)
	}
}

// 内核侧链不在场：如实说"链未在场/未挂上"，不冒出 macip 的槽冲突话术。
func TestKernelPortSecDetailNotInPlace(t *testing.T) {
	x := kernelFamilyFixture(t)
	x.setPortSec(fakePortSecRuntime{ok: true, dp: network.PortSecDataplane{
		Available: true, Tag: "内核 nftables 链 ps_ens256", TagPresent: false, Bound: false,
		Reason: "内核侧无 ACL 索引概念（白名单规则落在 nftables 链 ps_ens256 上）；未下发端口安全",
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens256 detail").Output
	if !strings.Contains(out, "白名单链未在场（未收敛）") || !strings.Contains(out, "白名单链未挂上（未收敛）") {
		t.Fatalf("链不在场时应如实报未在场/未挂上：\n%s", out)
	}
	if strings.Contains(out, "macip") {
		t.Fatalf("内核侧不应出现 macip 话术：\n%s", out)
	}
}

// VPP 侧不回归：索引照常打印、期望条数仍是「每 MAC 两条 permit + 两条 deny-all」。
func TestVPPPortSecDetailKeepsIndexOfMacipACL(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens256 description sec-port",
		"set virtual-switches vs-lan type l2",
		"set virtual-switches vs-lan ports 1 interface ens256",
		"set interfaces ens256 port-security mac aa:bb:cc:dd:ee:01",
		"commit",
		"exit",
	)
	x.setPortSec(fakePortSecRuntime{ok: true, dp: network.PortSecDataplane{
		Available: true, Tag: "nfvis-ps-ens256", TagPresent: true,
		ACLIndex: 3, RuleCount: 4, Bound: true, BoundIndex: 3,
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens256 detail").Output
	if !strings.Contains(out, "ACL 在场（索引 3，规则 4 条）") || !strings.Contains(out, "接口已绑定") {
		t.Fatalf("VPP 侧应照常打印 macip 索引与规则数：\n%s", out)
	}
	rt := detailEntryOf(t, x, "ens256")["port_security_runtime"].(map[string]any)
	if !sameNumber(rt["acl_index"], 3) || !sameNumber(rt["expected_rule_count"], 4) || !sameNumber(rt["bound_index"], 3) {
		t.Fatalf("VPP 侧结构化字段不应变：%v", rt)
	}
}
