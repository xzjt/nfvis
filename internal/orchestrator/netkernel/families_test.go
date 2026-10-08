package netkernel

// R2-15②：接口 detail 的读视图桥（families.go 的 StormDataplane/PortSecDataplane）必须把内核事实填进
// **消费方真正读的字段**——internal/api 的 storm.go/portsec.go 在 Available=true 时只读
// Kinds（storm）与 TagPresent/Tag/RuleCount/Bound（portsec），Reason 只在"不可核对"时打印。
//
// 这些用例走"假 Runner 给真 tc/nft 输出 → 桥 → 消费方字段"的整链；红-绿：还原成"事实只写进
// Reason、字段留空"的旧实现时逐条失败（读视图会把在位报成未收敛）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

func TestStormDataplaneFillsKindsFromKernelFacts(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens224 ingress",
		out: "filter protocol all pref 30 flower chain 0 \n" +
			"  dst_mac ff:ff:ff:ff:ff:ff\n" +
			"\taction order 1:  police 0x1 rate 1Mbit burst 1000000b mtu 2Kb action drop/continue overhead 0b \n" +
			"filter protocol all pref 40 flower chain 0 \n" +
			"  dst_mac 01:00:00:00:00:00/01:00:00:00:00:00\n" +
			"\taction order 1:  police 0x2 rate 2Mbit burst 2000000b mtu 2Kb action drop/continue overhead 0b ",
	}}}
	p := New(f)
	dp, ok := p.StormDataplane(context.Background(), "ens224")
	if !ok || !dp.Available || !dp.Attached {
		t.Fatalf("应报在位且可核对：%+v", dp)
	}
	bd, mcast := dp.Kinds[network.StormKindBroadcast], dp.Kinds[network.StormKindMulticast]
	if !bd.PolicerPresent || bd.CirKbps != 1000 {
		t.Fatalf("广播类的消费方字段应落内核实测速率（1000 kbps）：%+v", bd)
	}
	if !mcast.PolicerPresent || mcast.CirKbps != 2000 {
		t.Fatalf("组播类的消费方字段应落内核实测速率（2000 kbps）：%+v", mcast)
	}
	if bd.CountersReason == "" {
		t.Fatalf("内核 tc 计数与三档语义不对应，应如实给原因（消费方渲染为不可读+原因）")
	}
}

// 未下发：Available=true 但不在位（消费方据此渲染"未下发"，不渲染"未收敛"）。
func TestStormDataplaneNotAttachedKeepsHonestReason(t *testing.T) {
	p := New(&fakeRunner{})
	dp, ok := p.StormDataplane(context.Background(), "ens224")
	if ok || !dp.Available {
		t.Fatalf("空内核下应 Available=true、不在位：%+v", dp)
	}
	if dp.Attached || len(dp.Kinds) != 0 {
		t.Fatalf("未下发时不应报在位、不应有 Kinds：%+v", dp)
	}
}

// 读不到实况（设备不存在）时 Available=false：消费方据此渲染"不可核对（原因）"。
func TestStormDataplaneReadFailureIsUnavailable(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens224 ingress",
		out:    "Cannot find device \"ens224\"",
		err:    errors.New("exit status 1"),
	}}}
	p := New(f)
	dp, ok := p.StormDataplane(context.Background(), "ens224")
	if ok || dp.Available {
		t.Fatalf("读不到实况应 Available=false：%+v", dp)
	}
	if dp.Reason == "" {
		t.Fatalf("不可核对必须给原因")
	}
}

func TestPortSecDataplaneFillsConsumerFieldsFromKernelFacts(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list chain netdev nfvis-portsec ps_ens224",
			out: "table netdev nfvis-portsec {\n\tchain ps_ens224 {\n" +
				"\t\ttype filter hook ingress device \"ens224\" priority filter; policy accept;\n" +
				"\t\tether saddr != { aa:bb:cc:dd:ee:01 } drop\n\t}\n}"},
		{prefix: "ip -j -d link show dev ens224",
			out: `[{"ifname":"ens224","linkinfo":{"info_slave_data":{"learning":false}}}]`},
	}}
	p := New(f)
	dp, ok := p.PortSecDataplane(context.Background(), "ens224")
	// 第二个返回值 = 端口安全是否**完整生效**（白名单在位 且 学习已关）；消费方只读字段，
	// 这里两个口径都断言。
	if !ok || !dp.Available {
		t.Fatalf("应报在位且可核对：%+v", dp)
	}
	if !dp.TagPresent {
		t.Fatalf("消费方读的 TagPresent 必须落内核事实（nft 链在场）：%+v", dp)
	}
	if !strings.Contains(dp.Tag, portSecChainName("ens224")) {
		t.Fatalf("Tag 应是内核侧对象标识（含链名 %s）：%q", portSecChainName("ens224"), dp.Tag)
	}
	if dp.RuleCount != 1 || !dp.Bound {
		t.Fatalf("RuleCount（链内 1 条规则）与 Bound（链挂在该口上）都要落内核事实：%+v", dp)
	}
	if dp.ACLIndex != 0 || !strings.Contains(dp.Reason, "无 ACL 索引") {
		t.Fatalf("内核侧没有 ACL 索引这回事，应在 Reason 说明：%+v", dp)
	}
}

// 链不在场（未下发）：TagPresent=false、Bound=false，消费方据此渲染"未在场/未挂上"。
func TestPortSecDataplaneNotInPlaceLeavesFieldsFalse(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list chain netdev nfvis-portsec ps_ens224",
			out: "Error: No such file or directory", err: errors.New("exit status 1")},
		{prefix: "ip -j -d link show dev ens224",
			out: `[{"ifname":"ens224","linkinfo":{"info_slave_data":{"learning":true}}}]`},
	}}
	p := New(f)
	dp, ok := p.PortSecDataplane(context.Background(), "ens224")
	if ok || !dp.Available || dp.TagPresent || dp.Bound || dp.RuleCount != 0 {
		t.Fatalf("链不在场时应 Available=true、字段如实为否：%+v", dp)
	}
}
