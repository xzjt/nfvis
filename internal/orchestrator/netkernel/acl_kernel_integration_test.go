//go:build integration && linux

// ACL 的真机集成测试：用**真实内核**（真 ip/nft 命令）验证下发、绑定、读视图与撤销。
//
// 设计要点：
//   - 自备 throwaway veth 对（`zacl0`/`zacl1`，对端放入一次性 netns，使 ICMP 真正经 veth 入向），
//     用完即删，不动机器上既有对象；
//   - 断言一律取**独立事实源**（`nft list table` / 真实 ping 丢包 / `ip neigh`），不复用被测代码
//     的返回值（读视图 Bound 另有单独断言，两者互为对照）；
//   - 依赖缺失（非 root、无 nft、无法建 netns、veth 基线不通）时如实跳过，不假绿。
package netkernel

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

const (
	zaclVeth0  = "zacl0"
	zaclVeth1  = "zacl1"
	zaclNS     = "zacl-ns"
	zaclHostIP = "10.203.0.1"
	zaclPeerIP = "10.203.0.2"
)

// zaclCleanup 清掉本测试可能留下的对象（幂等；测试失败也跑）。
func zaclCleanup() {
	_, _ = exec.Command("ip", "netns", "del", zaclNS).CombinedOutput()
	_, _ = exec.Command("ip", "link", "del", zaclVeth0).CombinedOutput()
	_, _ = exec.Command("nft", "delete", "table", aclTableFamily, aclTableName).CombinedOutput()
}

// zaclPingOK 从宿主 ping 对端，返回是否**至少收到一个应答**（基线/恢复判定）。
// 注意不能用 `Contains(out, "0% packet loss")`——`100% packet loss` 也含该子串。
func zaclPingOK() bool {
	out, _ := exec.Command("ping", "-c", "2", "-W", "1", zaclPeerIP).CombinedOutput()
	s := string(out)
	return strings.Contains(s, "packet loss") && !strings.Contains(s, "100% packet loss")
}

// zaclPingBlocked 从宿主 ping 对端，返回是否**全部丢失**（ACL 生效判定）。
func zaclPingBlocked() bool {
	out, _ := exec.Command("ping", "-c", "2", "-W", "1", zaclPeerIP).CombinedOutput()
	return strings.Contains(string(out), "100% packet loss")
}

// zaclListTable 读本产品 ACL 表的实况（表不存在时返回错误文本，调用方按字符串判定）。
func zaclListTable() string {
	out, _ := exec.Command("nft", "list", "table", aclTableFamily, aclTableName).CombinedOutput()
	return string(out)
}

// zaclSetup 建 throwaway veth 对，并把对端放入一次性 netns（保证入向真的经过 veth）。
func zaclSetup(t *testing.T) {
	t.Helper()
	zaclCleanup()
	ztRun(t, "ip", "link", "add", zaclVeth0, "type", "veth", "peer", "name", zaclVeth1)
	ztRun(t, "ip", "addr", "add", zaclHostIP+"/24", "dev", zaclVeth0)
	ztRun(t, "ip", "link", "set", zaclVeth0, "up")
	ztRun(t, "ip", "link", "set", zaclVeth1, "up")
	if out, err := exec.Command("ip", "netns", "add", zaclNS).CombinedOutput(); err != nil {
		t.Skipf("无法创建网络命名空间（环境受限），跳过：%v\n%s", err, out)
	}
	ztRun(t, "ip", "link", "set", zaclVeth1, "netns", zaclNS)
	ztRun(t, "ip", "netns", "exec", zaclNS, "ip", "addr", "add", zaclPeerIP+"/24", "dev", zaclVeth1)
	ztRun(t, "ip", "netns", "exec", zaclNS, "ip", "link", "set", zaclVeth1, "up")
	ztRun(t, "ip", "netns", "exec", zaclNS, "ip", "link", "set", "lo", "up")
}

// TestKernelACLRealKernel ACL：下发 → 绑定 → 独立读 nft 核对 → 真发流量看丢弃（且 ARP 不受影响）
// → 读视图 → 解绑 → 删除。
func TestKernelACLRealKernel(t *testing.T) {
	ztRequireRoot(t)
	// 机器上已有本产品的 ACL 表（说明另有实例在跑）：如实跳过，不覆盖别人的现场。
	if _, err := exec.Command("nft", "list", "table", aclTableFamily, aclTableName).Output(); err == nil {
		t.Skip("机器上已存在 netdev " + aclTableName + "（另有实例在用），跳过以免覆盖")
	}
	ctx := context.Background()
	zaclSetup(t)
	defer zaclCleanup()

	// 基线连通性：veth 上必须先能通，否则「被 ACL 丢弃」无从判定。
	if !zaclPingOK() {
		t.Skip("veth 上基线 ping 不通（环境受限），跳过 ACL 实测")
	}

	m := newACLManager(NewExecRunner())
	acl := model.Acl{Name: "zt-acl-web", Rules: []model.AclRule{
		{Seq: 10, Direction: "ingress", Protocol: "icmp", Source: zaclPeerIP, Action: "deny"},
	}}
	if err := m.Apply(ctx, acl); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 幂等：同一声明再下一次不应报错，且规则不重复堆积。
	if err := m.Apply(ctx, acl); err != nil {
		t.Fatalf("Apply 幂等重放: %v", err)
	}
	// 独立事实源①：ACL 链、规则与链尾兜底 drop。
	out := zaclListTable()
	for _, want := range []string{
		"chain " + aclChainName("zt-acl-web"),
		"ip saddr " + zaclPeerIP,
		"ip protocol icmp",
		"drop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nft 表里找不到 %q：\n%s", want, out)
		}
	}

	if err := m.Bind(ctx, zaclVeth0, acl); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// 幂等：重复绑定不应报错（内部先撤旧绑定再重建）。
	if err := m.Bind(ctx, zaclVeth0, acl); err != nil {
		t.Fatalf("Bind 幂等重放: %v", err)
	}
	// 独立事实源②：绑定链含非 IP 放行 + jump（comment 记录原名）。
	out = zaclListTable()
	for _, want := range []string{
		`hook ingress device "` + zaclVeth0 + `"`,
		"meta protocol != { ip, ip6 } accept",
		"jump " + aclChainName("zt-acl-web"),
		`comment "zt-acl-web"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("绑定后 nft 表里找不到 %q：\n%s", want, out)
		}
	}

	// 读视图（与独立事实源互为对照）。
	if got, ok := m.Bound(ctx, zaclVeth0); !ok || got != "zt-acl-web" {
		t.Errorf("Bound 应报绑定 zt-acl-web，得到 %q/%v", got, ok)
	}

	// 执行实证：deny 规则在场时 ICMP 必须被丢（清邻居后重新解析，同时考察 ARP）。
	_, _ = exec.Command("ip", "neigh", "flush", "dev", zaclVeth0).CombinedOutput()
	if !zaclPingBlocked() {
		t.Errorf("deny 规则在场时 ping 仍通（ACL 未生效）")
	}
	neigh, _ := exec.Command("ip", "neigh", "show", "dev", zaclVeth0).CombinedOutput()
	if !strings.Contains(string(neigh), "lladdr") {
		t.Errorf("绑定 ACL 后 ARP 未能解析（非 IP 放行未生效，域内 ARP 被误伤）：\n%s", neigh)
	}

	// 仍被绑定时删除必须被拒绝（不留悬空 jump）。
	if err := m.Delete(ctx, "zt-acl-web"); err == nil {
		t.Errorf("ACL 仍被绑定时删除应报错")
	}

	// Unbind：本设备的绑定链与规则都消失，且不扰其它对象；流量恢复。
	if err := m.Unbind(ctx, zaclVeth0); err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	if out := zaclListTable(); strings.Contains(out, aclBindChainName(zaclVeth0)) {
		t.Errorf("Unbind 后仍留有该设备的绑定链：\n%s", out)
	}
	if _, ok := m.Bound(ctx, zaclVeth0); ok {
		t.Errorf("Unbind 后 Bound 应报未绑定")
	}
	if !zaclPingOK() {
		t.Errorf("Unbind 后 ping 应恢复")
	}

	// Delete：链被回收（表随之清空）。
	if err := m.Delete(ctx, "zt-acl-web"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if out := zaclListTable(); strings.Contains(out, aclChainName("zt-acl-web")) {
		t.Errorf("Delete 后 ACL 链仍在：\n%s", out)
	}
}
