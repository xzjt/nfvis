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
			out: `[{"ifname":"ens192","master":"br0","linkinfo":{"info_slave_kind":"bridge","info_slave_data":{"learning":true}}}]`},
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
			out: `[{"ifname":"ens192","master":"br0","linkinfo":{"info_slave_kind":"bridge","info_slave_data":{"learning":true}}}]`},
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
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list table netdev nfvis-portsec",
			out: "table netdev nfvis-portsec {\n\tchain ps_ens224 {\n\t}\n}"},
		// 撤除路径要读桥成员状态决定是否恢复学习（R2-22：读得到才行——读不到会如实上抛）。
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","master":"br0","linkinfo":{"info_slave_kind":"bridge","info_slave_data":{"learning":true}}}]`},
	}}
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
			out: `[{"ifname":"ens192","master":"br0","linkinfo":{"info_slave_kind":"bridge","info_slave_data":{"learning":true}}}]`},
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
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if !fact.Attached {
		t.Fatalf("应报已下发，detail=%q", fact.Detail)
	}
	if !strings.Contains(fact.Detail, "白名单 2 条") {
		t.Fatalf("detail 应报白名单条数，得到 %q", fact.Detail)
	}
	// 消费方读的字段（R2-15②）：链在场/链名/链内规则条数（内核形态 1 条规则承载整段白名单）/
	// 绑定在位——缺一个，读视图就会把"已下发"报成"未收敛"。
	if !fact.ChainPresent || fact.ChainName != "ps_ens192" || fact.RuleCount != 1 || !fact.Bound {
		t.Fatalf("消费方字段不符：%+v", fact)
	}
}

// 链内多条规则（手工加过规则）时 rule_count 要照实报——不按"白名单永远一条"硬编。
func TestPortSecDataplaneCountsActualRules(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list chain netdev nfvis-portsec ps_ens192",
			out: "table netdev nfvis-portsec {\n\tchain ps_ens192 {\n" +
				"\t\ttype filter hook ingress device \"ens192\" priority filter; policy accept;\n" +
				"\t\tether saddr != { aa:bb:cc:dd:ee:01 } drop\n" +
				"\t\tether type ip drop\n\t}\n}"},
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","linkinfo":{"info_slave_data":{"learning":false}}}]`},
	}}
	m := newPortSecManager(f)
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if fact.RuleCount != 2 {
		t.Fatalf("链内两条规则应报 2，得到 %d", fact.RuleCount)
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
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if fact.Attached {
		t.Fatalf("学习未关闭时不应报已下发，detail=%q", fact.Detail)
	}
	if !strings.Contains(fact.Detail, "学习未关闭") {
		t.Fatalf("detail 应说明学习未关闭，得到 %q", fact.Detail)
	}
	// 链本身在场——消费方据此渲染"链在场但未完全生效"，而不是"链不在场"。
	if !fact.ChainPresent || !fact.Bound {
		t.Fatalf("链在场的事实不应丢：%+v", fact)
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
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if fact.Attached {
		t.Fatalf("链不在时不应报已下发，detail=%q", fact.Detail)
	}
	if fact.ChainPresent || fact.Bound {
		t.Fatalf("链不在场时 ChainPresent/Bound 应为 false：%+v", fact)
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
	if _, err := m.Dataplane(context.Background(), "ens192"); err == nil {
		t.Fatalf("设备不存在应返回错误")
	}
}

func TestPortSecRuleCountExcludesChainDeclaration(t *testing.T) {
	chain := "table netdev nfvis-portsec {\n\tchain ps_ens192 {\n" +
		"\t\ttype filter hook ingress device \"ens192\" priority filter; policy accept;\n" +
		"\t\tether saddr != { aa:bb:cc:dd:ee:01 } drop\n\t}\n}"
	if got := portSecRuleCount(chain); got != 1 {
		t.Fatalf("标准链应数出 1 条规则，得到 %d", got)
	}
	empty := "table netdev nfvis-portsec {\n\tchain ps_ens192 {\n" +
		"\t\ttype filter hook ingress device \"ens192\" priority filter; policy accept;\n\t}\n}"
	if got := portSecRuleCount(empty); got != 0 {
		t.Fatalf("无规则的链应数出 0，得到 %d", got)
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
		// master 非空但 slave_kind=vrf：三层接口的 VRF 从属口，**不是** bridge 成员。
		{prefix: "ip -j -d link show dev ens192",
			out: `[{"ifname":"ens192","master":"vs-l3","linkinfo":{"info_slave_kind":"vrf"}}]`},
		{prefix: "nft list table netdev nfvis-portsec", out: "table netdev nfvis-portsec {}"},
	}}
	m := newPortSecManager(f)
	err := m.Apply(context.Background(), "ens192", []model.PortSecMAC{"aa:bb:cc:dd:ee:01"})
	if err == nil || !strings.Contains(err.Error(), "二层交换机（bridge）的成员口") {
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

// Teardown 里"读桥成员失败"必须如实上抛（R2-22）：
// 把"读不到"与"不是成员"合并成静默成功，会在读取故障下留下 learning off 残渣——该口的 MAC
// 学习从此不再恢复（交换机的学习能力被永久关掉），而操作者看不到任何迹象。
//
// 红-绿：修复前（`if err != nil || !member { return nil }`）本用例收不到错误。
func TestPortSecTeardownPropagatesBridgeMemberReadFailure(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list table netdev nfvis-portsec", out: "table netdev nfvis-portsec {}"},
		{prefix: "ip -j -d link show dev ens192",
			out: "RTNETLINK answers: Permission denied", err: errors.New("exit status 2")},
	}}
	m := newPortSecManager(f)
	err := m.Teardown(context.Background(), "ens192")
	if err == nil || !strings.Contains(err.Error(), "ip -j -d link show dev ens192") {
		t.Fatalf("读桥成员失败应如实上抛（而不是静默当'不是成员'），得到 %v", err)
	}
	if f.has("bridge link set") {
		t.Fatalf("读不到成员状态时不应猜着去设 learning；实际：\n%s", f.joined())
	}
}

// 设备不存在（`does not exist`）走的是 isBridgeMember 的 (false,nil) 分支：撤除按已达成，不报错。
// 与上一条的分界就在这里——"设备不在"是事实，"读不到"是故障。
func TestPortSecTeardownToleratesAbsentDevice(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "nft list table netdev nfvis-portsec", out: "table netdev nfvis-portsec {}"},
		{prefix: "ip -j -d link show dev ens192",
			out: `Device "ens192" does not exist.`, err: errors.New("exit status 1")},
	}}
	m := newPortSecManager(f)
	if err := m.Teardown(context.Background(), "ens192"); err != nil {
		t.Fatalf("设备不存在时撤除应按已达成，得到 %v", err)
	}
}
