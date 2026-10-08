//go:build integration && linux

// netkernel 的 QoS / 端口镜像真机集成测试：用**真实内核**（真 tc / ip）验证下发与回收，
// 并逐条用**独立事实源**（`tc qdisc show` / `tc filter show`）核对，不复用被测代码的读视图。
//
// 对象名一律 `zqos*` / `zspa*`，用完即删；不动机器上既有的物理口/桥/表。
// 依赖缺失（非 root / 无 tc）如实跳过，不假绿。
package netkernel

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	zqosDevA = "zqos0" // 限速对象口（veth 一端）
	zqosDevB = "zqos1" // veth 另一端
	zspanSrc = "zspa0" // 镜像源口
	zspanAna = "zspa1" // 镜像分析口（veth 对端）
)

// zqosRequireRoot 复用既有 root 判定，再补一条 tc 存在性检查。
func zqosRequireRoot(t *testing.T) {
	t.Helper()
	ztRequireRoot(t)
	if _, err := exec.LookPath("tc"); err != nil {
		t.Skip("缺少 iproute2 的 tc")
	}
}

// zqosCleanup 清掉本测试可能留下的对象（幂等；失败也跑）。
func zqosCleanup() {
	for _, d := range []string{zqosDevA, zqosDevB, zspanSrc, zspanAna, "zqosn0", "zqosn1"} {
		_, _ = exec.Command("ip", "link", "del", d).CombinedOutput()
	}
	for _, ns := range []string{"zqnsa", "zqnsb"} {
		_, _ = exec.Command("ip", "netns", "del", ns).CombinedOutput()
	}
}

// zqosQdiscExists 独立读 tc 判断设备上是否有 clsact qdisc。
func zqosQdiscExists(t *testing.T, dev string) bool {
	t.Helper()
	out, _ := exec.Command("tc", "qdisc", "show", "dev", dev).CombinedOutput()
	return strings.Contains(string(out), "clsact")
}

// zqosActionCount 某方向上的 action 条数（一条 filter 一个 action；用于验证幂等不叠加）。
func zqosActionCount(t *testing.T, dev, dir string) int {
	t.Helper()
	out, _ := exec.Command("tc", "filter", "show", "dev", dev, dir).CombinedOutput()
	return strings.Count(string(out), "action order")
}

// zqosHasFilter 某方向是否有 filter（不看被测代码的返回值）。
func zqosHasFilter(t *testing.T, dev, dir string) bool {
	t.Helper()
	return zqosActionCount(t, dev, dir) > 0
}

// zspanHasMirror 源口某方向是否有一条 mirred 到指定分析口的 filter。
func zspanHasMirror(t *testing.T, srcDev, dir, analyzer string) bool {
	t.Helper()
	out, _ := exec.Command("tc", "filter", "show", "dev", srcDev, dir).CombinedOutput()
	s := string(out)
	return strings.Contains(s, "mirred") && strings.Contains(s, analyzer)
}

// zspanLinkUp 设备管理状态是否为 up（读 `ip -j link show` 的 flags）。
func zspanLinkUp(t *testing.T, dev string) bool {
	t.Helper()
	out, err := exec.Command("ip", "-j", "link", "show", "dev", dev).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), `"UP"`)
}

// TestKernelQoSBindIndependenceAndUnbind 真机验证：入/出向独立、幂等不叠加、解绑确实摘 filter、
// 两向都空才回收 clsact——每条断言都读内核（tc），不看被测代码的返回值。
func TestKernelQoSBindIndependenceAndUnbind(t *testing.T) {
	zqosRequireRoot(t)
	ctx := context.Background()
	zqosCleanup()
	defer zqosCleanup()

	ztRun(t, "ip", "link", "add", zqosDevA, "type", "veth", "peer", "name", zqosDevB)
	ztRun(t, "ip", "link", "set", zqosDevA, "up")
	ztRun(t, "ip", "link", "set", zqosDevB, "up")

	m := newQoSManager(NewExecRunner())

	// —— 入向绑定：clsact + ingress police filter 落到内核 ——
	if err := m.Bind(ctx, zqosDevA, "ingress", 8000, 1000); err != nil {
		t.Fatalf("Bind ingress: %v", err)
	}
	if !zqosQdiscExists(t, zqosDevA) {
		t.Fatal("clsact qdisc 未落到内核")
	}
	if !zqosHasFilter(t, zqosDevA, "ingress") {
		t.Fatal("ingress filter 未落到内核")
	}
	// R2-3：判决必须是"超限丢、未超限继续"（真机实测：不写 conform-exceed 时 police 对合规包
	// 返回 OK(0)，会把同 hook 上排在后面的族整族静默屏蔽）。
	if out, _ := exec.Command("tc", "filter", "show", "dev", zqosDevA, "ingress").CombinedOutput(); !strings.Contains(string(out), "drop/continue") {
		t.Fatalf("ingress police 判决不是 conform-exceed drop/continue（会短路同 hook 的其它族）：\n%s", out)
	}
	if zqosHasFilter(t, zqosDevA, "egress") {
		t.Fatal("只绑 ingress 时 egress 不应有 filter")
	}

	// —— 幂等：重复绑定收敛成一条（不叠加） ——
	if err := m.Bind(ctx, zqosDevA, "ingress", 8000, 1000); err != nil {
		t.Fatalf("重复 Bind: %v", err)
	}
	if n := zqosActionCount(t, zqosDevA, "ingress"); n != 1 {
		t.Fatalf("重复绑定后 ingress 上应恰好一条 filter，实际 %d 条", n)
	}

	// —— 出向独立：绑 egress 不动 ingress ——
	if err := m.Bind(ctx, zqosDevA, "egress", 16000, 2000); err != nil {
		t.Fatalf("Bind egress: %v", err)
	}
	if !zqosHasFilter(t, zqosDevA, "ingress") || !zqosHasFilter(t, zqosDevA, "egress") {
		t.Fatal("两向应各有一条 filter")
	}

	// —— 解绑 ingress：egress 必须存活、clsact 必须保留 ——
	if err := m.Unbind(ctx, zqosDevA, "ingress"); err != nil {
		t.Fatalf("Unbind ingress: %v", err)
	}
	if zqosHasFilter(t, zqosDevA, "ingress") {
		t.Fatal("ingress filter 未摘除")
	}
	if !zqosHasFilter(t, zqosDevA, "egress") {
		t.Fatal("解绑 ingress 误伤了 egress（方向不独立）")
	}
	if !zqosQdiscExists(t, zqosDevA) {
		t.Fatal("egress 仍绑定时不应回收 clsact")
	}

	// —— 解绑 egress：再无绑定 → clsact 回收 ——
	if err := m.Unbind(ctx, zqosDevA, "egress"); err != nil {
		t.Fatalf("Unbind egress: %v", err)
	}
	if zqosQdiscExists(t, zqosDevA) {
		t.Fatal("两向都解绑后 clsact 未回收")
	}

	// —— 读视图与内核事实一致（Bound） ——
	if err := m.Bind(ctx, zqosDevA, "ingress", 8000, 1000); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.Bound(ctx, zqosDevA, "ingress"); err != nil || !ok {
		t.Fatalf("Bound 应读到绑定，得到 ok=%v err=%v", ok, err)
	}
	if ok, _ := m.Bound(ctx, zqosDevA, "egress"); ok {
		t.Fatal("Bound 对未绑方向应返回 false")
	}
}

// TestKernelSpanMirrorAndDelete 真机验证：镜像 filter 落到源口两 hook、分析口被置 up、
// Delete 确实摘除两 hook 的 filter。
func TestKernelSpanMirrorAndDelete(t *testing.T) {
	zqosRequireRoot(t)
	ctx := context.Background()
	zqosCleanup()
	defer zqosCleanup()

	ztRun(t, "ip", "link", "add", zspanSrc, "type", "veth", "peer", "name", zspanAna)

	m := newSpanManager(NewExecRunner())

	// —— both：两 hook 各一条 mirred；分析口被置 up ——
	if err := m.Apply(ctx, zspanSrc, zspanAna, "both"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !zqosQdiscExists(t, zspanSrc) {
		t.Fatal("clsact qdisc 未落到内核")
	}
	if !zspanHasMirror(t, zspanSrc, "ingress", zspanAna) {
		t.Fatal("ingress 未装到分析口的 mirred filter")
	}
	// R2-3：mirred 必须尾随 `continue`（真机实测：默认 pipe 判决 ≥0 会短路同 hook 的后续 filter）。
	if out, _ := exec.Command("tc", "filter", "show", "dev", zspanSrc, "ingress").CombinedOutput(); !strings.Contains(string(out), "continue") {
		t.Fatalf("ingress mirred 未尾随 continue（会短路同 hook 的其它族）：\n%s", out)
	}
	if !zspanHasMirror(t, zspanSrc, "egress", zspanAna) {
		t.Fatal("egress 未装到分析口的 mirred filter")
	}
	if !zspanLinkUp(t, zspanAna) {
		t.Fatal("分析口未被置 up（镜像包发不出去）")
	}

	// 读视图与内核事实一致。
	if dev, ok := m.Bound(ctx, zspanSrc); !ok || dev != zspanAna {
		t.Fatalf("Bound 应读出分析口 %s，得到 %q ok=%v", zspanAna, dev, ok)
	}

	// —— 幂等：重复 Apply 不叠加 ——
	if err := m.Apply(ctx, zspanSrc, zspanAna, "both"); err != nil {
		t.Fatalf("重复 Apply: %v", err)
	}
	if n := zqosActionCount(t, zspanSrc, "ingress"); n != 1 {
		t.Fatalf("重复 Apply 后 ingress 应恰好一条 filter，实际 %d 条", n)
	}

	// —— Delete：两 hook 的 filter 都没了 ——
	if err := m.Delete(ctx, zspanSrc, "both"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if zqosHasFilter(t, zspanSrc, "ingress") || zqosHasFilter(t, zspanSrc, "egress") {
		t.Fatal("Delete 后仍有 mirred filter 残留")
	}
	if dev, ok := m.Bound(ctx, zspanSrc); ok || dev != "" {
		t.Fatalf("Delete 后 Bound 应为空，得到 %q ok=%v", dev, ok)
	}
}

// zqosLossRe 抓「N% packet loss」；丢包率可能是小数（如 83.5%），故整段抓下来再解析，
// 只抓 [0-9]+ 会把 "83.5%" 里的 "5" 当成丢包率（真机踩过）。
var zqosLossRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)% packet loss`)

// zqosPingLoss 在某 netns 里 ping 目标，返回丢包率（读 ping 自身的统计，独立事实源）。
func zqosPingLoss(t *testing.T, ns, dst string, count int, flood bool) int {
	t.Helper()
	args := []string{"netns", "exec", ns, "ping", "-c", itoa(count), "-W", "1"}
	if flood {
		// -f 即洪水：不指定 -i 时按内核允许的最快速率连续发（需 root）。
		args = append(args, "-f")
	} else {
		args = append(args, "-i", "0.05")
	}
	args = append(args, dst)
	out, _ := exec.Command("ip", args...).CombinedOutput()
	m := zqosLossRe.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("未能从 ping 输出解析丢包率：\n%s", out)
	}
	loss, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("丢包率 %q 无法解析：%v", m[1], err)
	}
	return int(loss + 0.5)
}

// TestKernelQoSPoliceDropsTraffic 真机流量证明：装上限速计量器后，超出的包确实被丢。
//
// 拓扑：受管口留在宿主 netns（管理器在此执行 tc），对端移入一个 netns 作为流量源；
// 这样流量会真正跨 veth 进入受管口的 ingress hook（两端同处一个 netns 会被内核本地短接、不经过 hook）。
// 判据两条独立事实源：ping 的丢包率 + `tc -s filter show` 的 police 丢包计数。
func TestKernelQoSPoliceDropsTraffic(t *testing.T) {
	zqosRequireRoot(t)
	ctx := context.Background()
	const (
		nsB  = "zqnsb"
		devA = "zqosn0" // 宿主 netns，受管口
		devB = "zqosn1" // 移入 nsB
		ipA  = "10.201.0.1"
	)
	zqosCleanup()
	defer zqosCleanup()

	if out, err := exec.Command("ip", "netns", "add", nsB).CombinedOutput(); err != nil {
		t.Skipf("内核不支持 network namespace（%v）：%s", err, out)
	}
	ztRun(t, "ip", "link", "add", devA, "type", "veth", "peer", "name", devB)
	ztRun(t, "ip", "link", "set", devB, "netns", nsB)
	ztRun(t, "ip", "addr", "add", ipA+"/24", "dev", devA)
	ztRun(t, "ip", "link", "set", devA, "up")
	ztRun(t, "ip", "netns", "exec", nsB, "ip", "addr", "add", "10.201.0.2/24", "dev", devB)
	ztRun(t, "ip", "netns", "exec", nsB, "ip", "link", "set", devB, "up")
	ztRun(t, "ip", "netns", "exec", nsB, "ip", "link", "set", "lo", "up")

	// 先让宿主侧 ping 一次对端，把宿主的三层邻居表焐热——否则宿主回包时对端 MAC 尚未解析，
	// 测出来的「基线丢包」是环境噪声而不是数据面行为（真机踩过：不焐热时基线恒 100%）。
	_, _ = exec.Command("ping", "-c", "3", "-W", "1", "-I", devA, "10.201.0.2").CombinedOutput()

	// 无绑定对照：应基本无丢包。
	_ = zqosPingLoss(t, nsB, ipA, 3, false)
	if loss := zqosPingLoss(t, nsB, ipA, 20, false); loss > 10 {
		t.Fatalf("无绑定时不应有明显丢包，实测 %d%%", loss)
	}

	// 装上极小速率计量器（8 kbit/s ≈ 1 kB/s，突发 1 kB），再洪水发包。
	m := newQoSManager(NewExecRunner())
	if err := m.Bind(ctx, devA, "ingress", 8000, 1000); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// 先确认计量器确实装进了内核（否则后面的丢包无从谈起）。
	if out := ztRun(t, "tc", "filter", "show", "dev", devA, "ingress"); !strings.Contains(out, "police") {
		t.Fatalf("计量器未落到内核：\n%s", out)
	}
	if loss := zqosPingLoss(t, nsB, ipA, 200, true); loss < 20 {
		t.Fatalf("限速后应大量丢包，实测仅 %d%%", loss)
	}

	// 独立事实源：police 的丢包计数必须非零。
	out := ztRun(t, "tc", "-s", "filter", "show", "dev", devA, "ingress")
	if !strings.Contains(out, "police") {
		t.Fatalf("内核里没有 police action：\n%s", out)
	}
	m2 := regexp.MustCompile(`dropped ([0-9]+)`).FindStringSubmatch(out)
	if m2 == nil || m2[1] == "0" {
		t.Fatalf("police 丢包计数为零（计量器没生效）：\n%s", out)
	}
}
