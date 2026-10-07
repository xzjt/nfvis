//go:build integration && linux

// storm/portsec 的真机集成测试：用**真实内核**（真 tc/nft/bridge 命令）验证下发与读视图。
//
// 设计要点：
//   - 自备 throwaway 桥 + veth 对（`zstm`/`zpsc` 前缀），用完即删，不动机器上既有对象；
//   - 断言一律取**独立事实源**（`tc filter show` / `nft list table` / `bridge -j link show` /
//     `ip -j -d link show`），不复用被测代码的返回值；
//   - 依赖缺失（无 tc/bridge/python3）时如实跳过，不假绿。
package netkernel

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

const (
	zstmBr = "zstmbr"
	zstm0  = "zstm0"
	zstm1  = "zstm1"
	zpscBr = "zpscbr"
	zpsc0  = "zpsc0"
	zpsc1  = "zpsc1"
)

// zstmRequireTools 复用的 root/ip/nft 前置之外，再要求 tc 与 bridge（本测试的两把主工具）。
func zstmRequireTools(t *testing.T) {
	t.Helper()
	ztRequireRoot(t)
	for _, tool := range []string{"tc", "bridge"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("缺少 " + tool)
		}
	}
}

// zstmNetCleanup 清掉本测试的网络对象（幂等；不碰 nft 表，避免误删别的实例在用）。
func zstmNetCleanup() {
	for _, c := range [][]string{
		{"tc", "qdisc", "del", "dev", zstm0, "clsact"},
		{"ip", "link", "del", zstmBr},
		{"ip", "link", "del", zstm0},
		{"ip", "link", "del", zpscBr},
		{"ip", "link", "del", zpsc0},
	} {
		_, _ = exec.Command(c[0], c[1:]...).CombinedOutput()
	}
}

// zpscNftCleanup 删本产品的端口安全表（仅在测试确认过「开跑前不存在」后才调用）。
func zpscNftCleanup() {
	_, _ = exec.Command("nft", "delete", "table", portSecTableFamily, portSecTableName).CombinedOutput()
}

// zstmPositiveCount 判断 out 里 label 后跟的整数是否 > 0（如 `dropped 398`）。
func zstmPositiveCount(out, label string) bool {
	i := strings.Index(out, label)
	if i < 0 {
		return false
	}
	rest := strings.TrimLeft(out[i+len(label):], " ")
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return false
	}
	n, _ := strconv.Atoi(rest[:end])
	return n > 0
}

// zstmSendFrames 从 iface 发 n 个原始以太帧（dst/src 为冒号 MAC，ethertype 为十六进制，
// payload 为每帧负载字节数）。依赖 python3 的 AF_PACKET 原始套接字；缺 python3 时返回 false，
// 调用方据此跳过该实测项。
func zstmSendFrames(t *testing.T, iface, dst, src, ethertype string, payload, n int) bool {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Log("缺少 python3，跳过基于真实流量的限速实测")
		return false
	}
	script := `import socket,sys
s=socket.socket(socket.AF_PACKET,socket.SOCK_RAW)
s.bind((sys.argv[1],0))
d=bytes.fromhex(sys.argv[2]); sr=bytes.fromhex(sys.argv[3]); et=bytes.fromhex(sys.argv[4])
payload=int(sys.argv[5]); n=int(sys.argv[6])
frame=d+sr+et+b"X"*payload
for _ in range(n):
    s.send(frame)
`
	out, err := exec.Command("python3", "-c", script, iface,
		strings.ReplaceAll(dst, ":", ""), strings.ReplaceAll(src, ":", ""), ethertype,
		strconv.Itoa(payload), strconv.Itoa(n)).CombinedOutput()
	if err != nil {
		t.Fatalf("发送原始帧失败: %v\n%s", err, out)
	}
	return true
}

// TestKernelStormRealKernel 风暴抑制：tc 入向 police 下发 → 独立读 tc 事实核对 → 真发帧看丢弃 → 撤除。
func TestKernelStormRealKernel(t *testing.T) {
	zstmRequireTools(t)
	ctx := context.Background()
	zstmNetCleanup()
	defer zstmNetCleanup()

	ztRun(t, "ip", "link", "add", zstmBr, "type", "bridge")
	ztRun(t, "ip", "link", "add", zstm0, "type", "veth", "peer", "name", zstm1)
	ztRun(t, "ip", "link", "set", zstm0, "master", zstmBr)
	ztRun(t, "ip", "link", "set", zstm0, "up")
	ztRun(t, "ip", "link", "set", zstm1, "up")
	ztRun(t, "ip", "link", "set", zstmBr, "up")

	m := newStormManager(NewExecRunner())
	if err := m.Apply(ctx, zstm0, &model.StormControl{BroadcastKbps: 1000, MulticastKbps: 2000}); err != nil {
		t.Fatalf("Apply 风暴抑制: %v", err)
	}

	// 独立事实源①：tc 过滤器（广播精确 + 组播 I/G 掩码 + police 动作）与 clsact qdisc。
	out := ztRun(t, "tc", "filter", "show", "dev", zstm0, "ingress")
	for _, want := range []string{
		"dst_mac ff:ff:ff:ff:ff:ff",
		"dst_mac 01:00:00:00:00:00/01:00:00:00:00:00",
		"police",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tc ingress 过滤器缺 %q：\n%s", want, out)
		}
	}
	if q := ztRun(t, "tc", "qdisc", "show", "dev", zstm0); !strings.Contains(q, "clsact") {
		t.Errorf("clsact qdisc 未挂上：\n%s", q)
	}

	// 独立事实源②：police 计数器——真发广播帧，超速应被丢弃。
	// 突发桶按 8 秒 CIR 估算（1000 kbps ⇒ 1 MB），故发足 ~2 MB（1500 帧 × 1414 字节）才能越过桶、
	// 看到丢弃（发得太少会整批落在突发窗口内、计数为 0，那是限速器的正常行为而非缺陷）。
	srcMAC := strings.TrimSpace(ztRun(t, "cat", "/sys/class/net/"+zstm1+"/address"))
	if zstmSendFrames(t, zstm1, "ff:ff:ff:ff:ff:ff", srcMAC, "88b5", 1400, 1500) {
		stats := ztRun(t, "tc", "-s", "filter", "show", "dev", zstm0, "ingress")
		if !zstmPositiveCount(stats, "dropped") {
			t.Errorf("1000 kbps 限速下 ~2MB 广播未被丢弃（police 计数为 0）：\n%s", stats)
		}
	}

	// 读视图（与上面独立事实源互为对照）。
	if attached, detail, err := m.Dataplane(ctx, zstm0); err != nil {
		t.Errorf("Dataplane: %v", err)
	} else if !attached {
		t.Errorf("Dataplane 应报已下发，得到 attached=false（%s）", detail)
	} else {
		t.Logf("风暴抑制读视图：%s", detail)
	}

	// 幂等：同一声明再下一次不应报错。
	if err := m.Apply(ctx, zstm0, &model.StormControl{BroadcastKbps: 1000, MulticastKbps: 2000}); err != nil {
		t.Fatalf("Apply 幂等重放失败: %v", err)
	}

	// Teardown：过滤器与 clsact 都应清掉。
	if err := m.Teardown(ctx, zstm0); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if out := ztRun(t, "tc", "filter", "show", "dev", zstm0, "ingress"); strings.Contains(out, "dst_mac") {
		t.Errorf("Teardown 后仍有风暴抑制过滤器：\n%s", out)
	}
	if q := ztRun(t, "tc", "qdisc", "show", "dev", zstm0); strings.Contains(q, "clsact") {
		t.Errorf("Teardown 后 clsact 未回收：\n%s", q)
	}
	if attached, _, err := m.Dataplane(ctx, zstm0); err != nil {
		t.Errorf("Dataplane（Teardown 后）: %v", err)
	} else if attached {
		t.Errorf("Teardown 后 Dataplane 仍报已下发")
	}
}

// TestKernelPortSecRealKernel 端口安全：bridge 关学习 + nftables 入向丢帧 → 独立读 nft/桥口事实核对 → 撤除。
func TestKernelPortSecRealKernel(t *testing.T) {
	zstmRequireTools(t)
	// 机器上已有本产品的端口安全表（说明另有实例在跑）：如实跳过，不覆盖别人的现场。
	if _, err := exec.Command("nft", "list", "table", portSecTableFamily, portSecTableName).Output(); err == nil {
		t.Skip("机器上已存在 netdev " + portSecTableName + "（另有实例在用），跳过以免覆盖")
	}
	ctx := context.Background()
	zstmNetCleanup()
	zpscNftCleanup()
	defer func() {
		zstmNetCleanup()
		zpscNftCleanup()
	}()

	ztRun(t, "ip", "link", "add", zpscBr, "type", "bridge")
	ztRun(t, "ip", "link", "add", zpsc0, "type", "veth", "peer", "name", zpsc1)
	ztRun(t, "ip", "link", "set", zpsc0, "master", zpscBr)
	ztRun(t, "ip", "link", "set", zpsc0, "up")
	ztRun(t, "ip", "link", "set", zpsc1, "up")
	ztRun(t, "ip", "link", "set", zpscBr, "up")

	m := newPortSecManager(NewExecRunner())
	macs := []model.PortSecMAC{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}
	if err := m.Apply(ctx, zpsc0, macs); err != nil {
		t.Fatalf("Apply 端口安全: %v", err)
	}

	// 独立事实源①：nft 表/链/规则。
	out := ztRun(t, "nft", "list", "table", portSecTableFamily, portSecTableName)
	for _, want := range []string{
		"chain " + portSecChainName(zpsc0),
		"ether saddr != { aa:bb:cc:dd:ee:01, aa:bb:cc:dd:ee:02 }",
		"drop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nft 端口安全规则缺 %q：\n%s", want, out)
		}
	}
	// 独立事实源②：桥口已挂进交换机（bridge -j link show 在 iproute2 6.19 不输出 learning 字段，
	// 学习状态以 `ip -j -d link show` 的 bridge_slave.learning 为准，见下）。
	if b := ztRun(t, "bridge", "-j", "link", "show", "dev", zpsc0); !strings.Contains(b, `"master":"`+zpscBr+`"`) {
		t.Errorf("端口未挂到交换机：\n%s", b)
	}
	// 独立事实源③：桥口学习已关闭。
	if l := ztRun(t, "ip", "-j", "-d", "link", "show", "dev", zpsc0); !strings.Contains(l, `"learning":false`) {
		t.Errorf("桥口学习未关闭：\n%s", l)
	}

	// 读视图。
	if attached, detail, err := m.Dataplane(ctx, zpsc0); err != nil {
		t.Errorf("Dataplane: %v", err)
	} else if !attached {
		t.Errorf("Dataplane 应报已下发，得到 attached=false（%s）", detail)
	} else {
		t.Logf("端口安全读视图：%s", detail)
	}

	// 幂等：同一声明再下一次不应报错。
	if err := m.Apply(ctx, zpsc0, macs); err != nil {
		t.Fatalf("Apply 幂等重放失败: %v", err)
	}

	// Teardown：链与表都应清掉，学习恢复。
	if err := m.Teardown(ctx, zpsc0); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := exec.Command("nft", "list", "table", portSecTableFamily, portSecTableName).Output(); err == nil {
		t.Errorf("Teardown 后 nft 表仍在")
	}
	if l := ztRun(t, "ip", "-j", "-d", "link", "show", "dev", zpsc0); !strings.Contains(l, `"learning":true`) {
		t.Errorf("Teardown 后学习未恢复：\n%s", l)
	}
	if attached, _, err := m.Dataplane(ctx, zpsc0); err != nil {
		t.Errorf("Dataplane（Teardown 后）: %v", err)
	} else if attached {
		t.Errorf("Teardown 后 Dataplane 仍报已下发")
	}
}
