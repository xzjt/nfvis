package netkernel

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// 接口入向风暴抑制的内核数据面实现。
//
// 与 VPP 侧（policer + L2 分类表）语义对齐，底座换成 tc：
//   - 接口入向挂 clsact qdisc，按**目的 MAC 分类**加 flower 过滤器；
//   - 每类一个 `action police`（rate 即 kbps；conform 放行、exceed 丢弃）。
//
// 分类口径（真机实测，见交付说明）：
//   - 广播 = 目的 MAC 精确 `ff:ff:ff:ff:ff:ff`；
//   - 组播 = 目的 MAC 的 I/G 位（首字节最低位）= 1，用掩码写法 `01:00:00:00:00:00/01:...`。
//     tc 分类**首个命中即终止**，故两类并存时广播过滤器优先级更高（pref 小）：广播帧先被广播
//     过滤器命中、不再落到组播过滤器——与 VPP 侧「两类同配时广播走自己的精确表」一致。只配
//     组播时广播帧会命中 I/G 过滤器（I/G 口径包含广播）——同样与 VPP 侧的如实口径一致。
//
// 幂等：Apply 一律「先删本接口自己的过滤器（pref 10/20）、再按声明加」；Teardown 删过滤器，
// 且本接口 ingress/egress 都不再有过滤器时才回收 clsact qdisc（clsact 与 QoS/镜像等族共用，
// 贸然删除会连带清掉别的族挂在同一条链上的过滤器）。

const (
	// 过滤器优先级：数值小者先评估（广播在前，见文件头「首个命中即终止」）。
	stormPrefBroadcast = tcPrefStormBcast
	stormPrefMulticast = tcPrefStormMcast
	// 目的 MAC 匹配写法（flower dst_mac；组播用掩码写法表达 I/G 位）。
	stormBroadcastMAC = "ff:ff:ff:ff:ff:ff"
	stormMulticastMAC = "01:00:00:00:00:00/01:00:00:00:00:00"
)

// stormManager 接口入向风暴抑制的内核下发与读视图。
type stormManager struct{ run Runner }

// newStormManager 以给定 Runner 构造（run 为 nil 时用真实宿主命令）。
func newStormManager(run Runner) *stormManager {
	if run == nil {
		run = NewExecRunner()
	}
	return &stormManager{run: run}
}

// stormBurstBytes police 的突发桶容量（字节）。
//
// 按 8 秒 CIR 估算（kbps → 字节/秒 = kbps*125，再乘 8）——与 VPP 侧 policer 的 cb 同一口径，
// 让两数据面在「短时突发允许多少」上相称。不作为用户配置项暴露（多一个没人会调的旋钮）。
func stormBurstBytes(kbps int) int {
	if kbps < 1 {
		return 1
	}
	return kbps * 1000
}

// Apply 把接口的风暴抑制收敛到声明；sc 为空（或两类都未给值）表示「未声明」——撤除。
func (m *stormManager) Apply(ctx context.Context, dev string, sc *model.StormControl) error {
	if sc == nil || (sc.BroadcastKbps <= 0 && sc.MulticastKbps <= 0) {
		return m.Teardown(ctx, dev)
	}
	if dev == "" {
		return fmt.Errorf("风暴抑制接口名不能为空")
	}
	if err := m.ensureClsact(ctx, dev); err != nil {
		return err
	}
	if err := m.delOwnFilters(ctx, dev); err != nil {
		return err
	}
	// 广播在前（pref 小）：首个命中即终止，保证广播不被组播 I/G 过滤器二次命中。
	if sc.BroadcastKbps > 0 {
		if err := m.addFilter(ctx, dev, stormPrefBroadcast, stormBroadcastMAC, sc.BroadcastKbps); err != nil {
			return err
		}
	}
	if sc.MulticastKbps > 0 {
		if err := m.addFilter(ctx, dev, stormPrefMulticast, stormMulticastMAC, sc.MulticastKbps); err != nil {
			return err
		}
	}
	return nil
}

// Teardown 撤除该接口的风暴抑制（幂等：对象本就不在按已达成）。
func (m *stormManager) Teardown(ctx context.Context, dev string) error {
	if dev == "" {
		return fmt.Errorf("风暴抑制接口名不能为空")
	}
	if err := m.delOwnFilters(ctx, dev); err != nil {
		return err
	}
	return m.dropClsactIfIdle(ctx, dev)
}

// Dataplane 读该接口风暴抑制的内核实况：是否存在本产品的入向限速过滤器，以及各类的实测速率。
// err != nil 表示「读不到实况」（例如设备不存在），而不是「未下发」。
func (m *stormManager) Dataplane(ctx context.Context, dev string) (attached bool, detail string, err error) {
	out, err := tcRun(ctx, m.run, "filter", "show", "dev", dev, "ingress")
	if err != nil {
		return false, "", fmt.Errorf("tc filter show dev %s ingress: %w（%s）", dev, err, trimOut(out))
	}
	var kinds []string
	for _, block := range stormFilterBlocks(out) {
		// 只有带 police 动作、且目的 MAC 是本产品两类之一的块才算我们的过滤器
		// （同一条链上可能有别的族挂的 flower 过滤器）。
		if !strings.Contains(block, "police") {
			continue
		}
		switch {
		case strings.Contains(block, stormBroadcastMAC):
			kinds = append(kinds, "广播"+stormRateSuffix(block))
		case strings.Contains(block, stormMulticastMAC):
			kinds = append(kinds, "组播"+stormRateSuffix(block))
		}
	}
	if len(kinds) == 0 {
		return false, "未下发入向限速", nil
	}
	return true, strings.Join(kinds, "、") + "（入向 tc 限速）", nil
}

// ensureClsact 确保接口挂上 clsact qdisc（幂等：已在则不动）。
//
// `tc qdisc add` 对已存在的 clsact 报 `Exclusivity flag on, cannot modify`（真机实测），
// 该文案不在 tcIdem 的「已存在」判据里，故先查后加而不是直接 add 后容错。
func (m *stormManager) ensureClsact(ctx context.Context, dev string) error {
	out, err := tcRun(ctx, m.run, "qdisc", "show", "dev", dev)
	if err != nil {
		return fmt.Errorf("tc qdisc show dev %s: %w（%s）", dev, err, trimOut(out))
	}
	if strings.Contains(out, "clsact") {
		return nil
	}
	return tcReq(ctx, m.run, "qdisc", "add", "dev", dev, "clsact")
}

// addFilter 加一条按目的 MAC 分类的入向限速过滤器。
func (m *stormManager) addFilter(ctx context.Context, dev string, pref int, dstMAC string, kbps int) error {
	return tcReq(ctx, m.run,
		"filter", "add", "dev", dev, "ingress", "protocol", "all",
		"pref", strconv.Itoa(pref),
		"flower", "dst_mac", dstMAC,
		"action", "police", "rate", fmt.Sprintf("%dkbit", kbps),
		"burst", strconv.Itoa(stormBurstBytes(kbps)), "drop")
}

// delOwnFilters 删掉本接口自己的两类过滤器（幂等：本就不在按已达成）。
func (m *stormManager) delOwnFilters(ctx context.Context, dev string) error {
	for _, pref := range []int{stormPrefBroadcast, stormPrefMulticast} {
		if err := m.delFilter(ctx, dev, pref); err != nil {
			return err
		}
	}
	return nil
}

// delFilter 按 pref 删一条入向过滤器。
func (m *stormManager) delFilter(ctx context.Context, dev string, pref int) error {
	out, err := tcRun(ctx, m.run, "filter", "del", "dev", dev, "ingress", "pref", strconv.Itoa(pref))
	if err == nil || stormFilterAbsent(out, err) {
		return nil
	}
	return fmt.Errorf("tc filter del dev %s ingress pref %d: %w（%s）", dev, pref, err, trimOut(out))
}

// stormFilterAbsent 判断过滤器删除失败是否只是「对象本就不在」：真机实测两种文案——
//   - clsact qdisc 不在：`Parent Qdisc doesn't exists.`；
//   - qdisc 在、该 pref 的过滤器不在：`Cannot find specified filter chain.`；
//   - 设备不在：`Cannot find device`（由 notFound 覆盖）。
func stormFilterAbsent(out string, err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(out)
	return notFound(out, err) ||
		strings.Contains(s, "parent qdisc") ||
		strings.Contains(s, "specified filter chain")
}

// dropClsactIfIdle 本接口 ingress/egress 都不再有过滤器时回收 clsact qdisc。
//
// 两条链都看：clsact 同时承载 ingress 与 egress，QoS/端口镜像等族可能正用着 egress，
// 只清自己的 ingress 就删 qdisc 会连带清掉它们的过滤器。
func (m *stormManager) dropClsactIfIdle(ctx context.Context, dev string) error {
	// clsact 是本包各绑定族共用的：判据必须是"设备上再无任何 filter"，而不是"没有本族的
	// filter"（后者会把别的族正在用的 hook 连带撤掉）。
	return tcDropClsactIfIdle(ctx, m.run, dev)
}

// stormHasFilters 判断 `tc filter show` 输出里是否有过滤器条目。
func stormHasFilters(out string) bool { return strings.Contains(out, "filter protocol") }

// stormFilterBlocks 把 `tc filter show` 输出按过滤器切成块（每块含 dst_mac 与 police 速率），
// 供读视图逐块判定「是不是本产品的风暴抑制过滤器」。
func stormFilterBlocks(out string) []string {
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "filter protocol") {
			flush()
		}
		cur = append(cur, line)
	}
	flush()
	return blocks
}

// stormRateSuffix 渲染该过滤器块的实测速率后缀（` 1000 kbps`）；解析不到就留空。
func stormRateSuffix(block string) string {
	if kbps := stormParseRateKbps(block); kbps >= 0 {
		return fmt.Sprintf(" %d kbps", kbps)
	}
	return ""
}

// stormParseRateKbps 从一块 tc 输出里解析 police 的速率（`rate <n><unit>`）为 kbps；
// 解析不到返回 -1。tc 的单位输出（真机实测）：kbps 为 1000 的整数倍时用 Mbit/Gbit，
// 否则 Kbit，极小值时用 bit。
func stormParseRateKbps(block string) int {
	fields := strings.Fields(block)
	for i, f := range fields {
		if f == "rate" && i+1 < len(fields) {
			return stormUnitToKbps(fields[i+1])
		}
	}
	return -1
}

// stormUnitToKbps 把 tc 的速率单位写法转成 kbps；无法解析返回 -1。
func stormUnitToKbps(tok string) int {
	// 长后缀先判：`Kbit` 也以 `bit` 结尾，顺序不能反。
	for _, u := range []struct {
		suffix string
		scale  int // kbps = n*scale；scale==0 表示 bit（按 /1000 处理）
	}{
		{"Gbit", 1000000},
		{"Mbit", 1000},
		{"Kbit", 1},
		{"bit", 0},
	} {
		if !strings.HasSuffix(tok, u.suffix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(tok, u.suffix))
		if err != nil {
			return -1
		}
		if u.scale == 0 {
			return n / 1000
		}
		return n * u.scale
	}
	return -1
}
