package netkernel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestPortSecApplyDisablesLearningAndAddsIngressDropRule(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","master":"br0","linkinfo":{"info_slave_data":{"learning":true}}}]`},
	}}
	m := newPortSecManager(f)
	macs := []model.PortSecMAC{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}
	if err := m.Apply(context.Background(), "ens192", macs); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"bridge link set dev ens192 learning off",
		"nft add table netdev nfvis-portsec",
		"nft add chain netdev nfvis-portsec ps_ens192 { type filter hook ingress device ens192 priority filter ; }",
		"nft flush chain netdev nfvis-portsec ps_ens192",
		"nft add rule netdev nfvis-portsec ps_ens192 ether saddr != { aa:bb:cc:dd:ee:01, aa:bb:cc:dd:ee:02 } drop",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, f.joined())
		}
	}
	// 关学习要排在白名单规则之前（设备不是桥成员时更早暴露配置错误）。
	learn := portSecTestCallIndex(f, "bridge link set dev ens192 learning off")
	rule := portSecTestCallIndex(f, "nft add rule")
	if learn < 0 || rule < 0 || learn > rule {
		t.Fatalf("应先关学习再加白名单规则；实际：\n%s", f.joined())
	}
}

func TestPortSecApplyEmptyTearsDownAndRestoresLearning(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","master":"br0","linkinfo":{"info_slave_data":{"learning":true}}}]`},
		{prefix: "nft list table netdev nfvis-portsec",
			out: "table netdev nfvis-portsec {}"},
	}}
	m := newPortSecManager(f)
	if err := m.Apply(context.Background(), "ens192", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"nft delete chain netdev nfvis-portsec ps_ens192",
		"nft delete table netdev nfvis-portsec",
		"bridge link set dev ens192 learning on",
	} {
		if !f.has(want) {
			t.Fatalf("撤除路径缺少 %q；实际：\n%s", want, f.joined())
		}
	}
	if f.has("nft add rule") {
		t.Fatalf("空白名单不应下发规则；实际：\n%s", f.joined())
	}
}

func TestPortSecTeardownKeepsTableWhenOtherChainsRemain(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft list table netdev nfvis-portsec",
		out:    "table netdev nfvis-portsec {\n\tchain ps_ens224 {\n\t}\n}",
	}}}
	m := newPortSecManager(f)
	if err := m.Teardown(context.Background(), "ens192"); err != nil {
		t.Fatal(err)
	}
	if !f.has("nft delete chain netdev nfvis-portsec ps_ens192") {
		t.Fatalf("应删本接口的链；实际：\n%s", f.joined())
	}
	if f.has("nft delete table") {
		t.Fatalf("还有别的接口的链在用表时不得删表；实际：\n%s", f.joined())
	}
}

func TestPortSecApplyPropagatesLearningError(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","master":"br0","linkinfo":{"info_slave_data":{"learning":true}}}]`},
		{prefix: "bridge link set dev ens192 learning off",
			out: "RTNETLINK answers: Operation not supported",
			err: errors.New("exit status 255")},
	}}
	m := newPortSecManager(f)
	err := m.Apply(context.Background(), "ens192", []model.PortSecMAC{"aa:bb:cc:dd:ee:01"})
	if err == nil || !strings.Contains(err.Error(), "learning off") {
		t.Fatalf("关学习失败应如实上报，得到 %v", err)
	}
}

func TestPortSecDataplaneAttachedWhenRuleAndLearningOff(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list chain netdev nfvis-portsec ps_ens192",
			out: "table netdev nfvis-portsec {\n\tchain ps_ens192 {\n" +
				"\t\ttype filter hook ingress device \"ens192\" priority filter; policy accept;\n" +
				"\t\tether saddr != { aa:bb:cc:dd:ee:01, aa:bb:cc:dd:ee:02 } drop\n\t}\n}"},
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","linkinfo":{"info_slave_data":{"learning":false}}}]`},
	}}
	m := newPortSecManager(f)
	attached, detail, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if !attached {
		t.Fatalf("应报已下发，detail=%q", detail)
	}
	if !strings.Contains(detail, "白名单 2 条") {
		t.Fatalf("detail 应报白名单条数，得到 %q", detail)
	}
}

func TestPortSecDataplaneNotAttachedWhenLearningStillOn(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list chain netdev nfvis-portsec ps_ens192",
			out: "\t\tether saddr != { aa:bb:cc:dd:ee:01 } drop"},
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","linkinfo":{"info_slave_data":{"learning":true}}}]`},
	}}
	m := newPortSecManager(f)
	attached, detail, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if attached {
		t.Fatalf("学习未关闭时不应报已下发，detail=%q", detail)
	}
	if !strings.Contains(detail, "学习未关闭") {
		t.Fatalf("detail 应说明学习未关闭，得到 %q", detail)
	}
}

func TestPortSecDataplaneNotAttachedWhenChainAbsent(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list chain netdev nfvis-portsec ps_ens192",
			out: "Error: No such file or directory", err: errors.New("exit status 1")},
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","linkinfo":{"info_slave_data":{"learning":false}}}]`},
	}}
	m := newPortSecManager(f)
	attached, detail, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if attached {
		t.Fatalf("链不在时不应报已下发，detail=%q", detail)
	}
}

func TestPortSecDataplanePropagatesMissingDevice(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list chain netdev nfvis-portsec ps_ens192",
			out: "Error: No such file or directory", err: errors.New("exit status 1")},
		{prefix: "ip -j -d link show dev ens192",
			out: `Device "ens192" does not exist.`, err: errors.New("exit status 1")},
	}}
	m := newPortSecManager(f)
	if _, _, err := m.Dataplane(context.Background(), "ens192"); err == nil {
		t.Fatalf("设备不存在应返回错误")
	}
}

func TestPortSecRuleMACCountParsesSetForm(t *testing.T) {
	if got := portSecRuleMACCount("\t\tether saddr != { aa:bb:cc:dd:ee:01, aa:bb:cc:dd:ee:02 } drop"); got != 2 {
		t.Fatalf("应数出 2 条，得到 %d", got)
	}
	if got := portSecRuleMACCount("\t\tether saddr != { aa:bb:cc:dd:ee:01 } drop"); got != 1 {
		t.Fatalf("应数出 1 条，得到 %d", got)
	}
	if got := portSecRuleMACCount("\t\ttype filter hook ingress device \"ens192\" priority filter;"); got != 0 {
		t.Fatalf("无规则应返回 0，得到 %d", got)
	}
}

// portSecTestCallIndex 返回第一条包含 substr 的命令下标（-1 = 未出现）。
func portSecTestCallIndex(f *fakeRunner, substr string) int {
	for i, c := range f.calls {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

// 非桥成员口：Apply 明确拒绝（端口安全需要先挂到交换机上），Teardown 静默跳过恢复学习。
// 真机背景：接口声明早于交换机下发时必然走到这条路，`bridge link set` 对非成员口报
// `Operation not supported`——那不是失败，而是"没有学习可恢复"。
func TestPortSecNonBridgeMemberIsRejectedOnApplyAndToleratedOnTeardown(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j -d link show dev ens192", out: `[{"ifname":"ens192"}]`},
		{prefix: "nft list table netdev nfvis-portsec", out: "table netdev nfvis-portsec {}"},
	}}
	m := newPortSecManager(f)
	err := m.Apply(context.Background(), "ens192", []model.PortSecMAC{"aa:bb:cc:dd:ee:01"})
	if err == nil || !strings.Contains(err.Error(), "二层交换机成员口") {
		t.Fatalf("非桥成员口应被拒并说明原因，得到 %v", err)
	}
	if f.has("bridge link set") {
		t.Fatalf("非桥成员口不应去设 learning；实际：\n%s", f.joined())
	}
	f.calls = nil
	if err := m.Teardown(context.Background(), "ens192"); err != nil {
		t.Fatalf("非桥成员口撤除不应报错，得到 %v", err)
	}
	if f.has("bridge link set") {
		t.Fatalf("非桥成员口没有学习可恢复，不应去设 learning；实际：\n%s", f.joined())
	}
}
