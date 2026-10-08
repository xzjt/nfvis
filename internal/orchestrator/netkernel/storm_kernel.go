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
//   - 每类一个 `action police`（rate 即 kbps；超限丢弃；未超限"要不要继续遍历后续 filter"
//     按档不同，见 stormVerdictBroadcast / stormVerdictMulticast）。
//
// 分类口径（真机实测，见交付说明）：
//   - 广播 = 目的 MAC 精确 `ff:ff:ff:ff:ff:ff`；
//   - 组播 = 目的 MAC 的 I/G 位（首字节最低位）= 1，用掩码写法 `01:00:00:00:00:00/01:...`。
//     广播过滤器 pref 更小（先评估）且判决是**裸 `drop`（终止遍历）**：广播帧只落广播档，不被
//     组播（I/G）过滤器二次计量——与 VPP 侧「两类各有独立分类表、广播帧只落广播档」同一语义
//     （判决取舍与前提见 stormVerdictBroadcast 的注释）。只配组播档时广播帧会命中 I/G 过滤器
//     （I/G 口径包含广播）——与 VPP 侧没有广播档时的行为一致。
//
// 幂等：Apply 一律「先删本接口自己的过滤器（pref 10/20）、再按声明加」；Teardown 删过滤器，
// 且本接口 ingress/egress 都不再有过滤器时才回收 clsact qdisc（clsact 与 QoS/镜像等族共用，
// 贸然删除会连带清掉别的族挂在同一条链上的过滤器）。

const (
	// 过滤器优先级：数值小者先评估（广播在前）。
	stormPrefBroadcast = tcPrefStormBcast
	stormPrefMulticast = tcPrefStormMcast
	// 目的 MAC 匹配写法（flower dst_mac；组播用掩码写法表达 I/G 位）。
	stormBroadcastMAC = "ff:ff:ff:ff:ff:ff"
	stormMulticastMAC = "01:00:00:00:00:00/01:00:00:00:00:00"
)

// 两档的 police 判决（单一真源：argv 在 stormVerdict* 里定；读视图的 tcPoliceDrops 恰好认
// 这两种形态——同一接口上两档会同时出现 `action drop` 与 `action drop/continue` 两种打印）。
//
// stormVerdictBroadcast 广播档（pref 30，本族第一条）：裸 `drop`——超限丢，且**终止遍历**，
// 故意**不写** `conform-exceed drop/continue`。三条理由：
//
//	① 本族两条是 pref 最大的两条，广播档短路只会影响紧随其后的组播档，不会饿死别的族
//	   （QoS pref 10 / 镜像 pref 20 都排在它之前，已各自用"不吞包"的判决让包走到这里）；
//	② VPP 侧广播与组播是两套独立分类表、广播帧只落广播档——内核侧让广播帧在此终止遍历，
//	   才做到「同一份配置、同一语义」；
//	③ 若不终止（写成 drop/continue），`broadcast 10000 + multicast 8` 会把广播帧未超广播档时
//	   继续交给组播档再量一次，实际压到 8 kbps（比配置更紧）——是可观察的语义差。
//
// ⚠ 前提：本档短路只对"排在它之后的族"有影响。若将来在 pref 40 之后再挂族（新增族要按
// tc.go 的 pref 表取号段），需要重新审视这条判决——那时广播档会变成新族的隐形墙。
//
// stormVerdictMulticast 组播档（pref 40，本族最后一条）：`conform-exceed drop/continue`
// （超限丢、未超限继续遍历同 hook 的后续 filter）——本族后面当前没有别的过滤器，语义与裸 drop
// 等效；用 slash 形态避免它成为未来更高 pref 族的隐形墙（与 QoS/镜像的"不吞包"口径一致）。
var (
	stormVerdictBroadcast = []string{"drop"}
	stormVerdictMulticast = []string{"conform-exceed", "drop/continue"}
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
	// 广播在前（pref 小）：判决是裸 drop（终止遍历），保证广播帧不被组播 I/G 过滤器二次计量
	// ——与 VPP 侧「广播帧只落广播档」同一语义（取舍见 stormVerdictBroadcast 的注释）。
	if sc.BroadcastKbps > 0 {
		if err := m.addFilter(ctx, dev, stormPrefBroadcast, stormBroadcastMAC, sc.BroadcastKbps, stormVerdictBroadcast); err != nil {
			return err
		}
	}
	if sc.MulticastKbps > 0 {
		if err := m.addFilter(ctx, dev, stormPrefMulticast, stormMulticastMAC, sc.MulticastKbps, stormVerdictMulticast); err != nil {
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

// stormDataplaneFact 风暴抑制的内核实况（读视图用）。
//
// 为什么单独给一份结构而不是只给一段文本（R2-15②）：消费方 internal/api/storm.go 在
// Available=true 时**只读 Kinds**（Reason 只在"不可核对"时打印）——把事实写成一句话塞进
// Reason，读视图就会把"tc 过滤器在位工作"报成"policer 未在数据面（未收敛）"。
// 各类速率取 -1 表示该类过滤器不在位（不是 0——0 会被当成一个真实的限速值）。
type stormDataplaneFact struct {
	Attached      bool
	Detail        string
	BroadcastKbps int
	MulticastKbps int
}

// Dataplane 读该接口风暴抑制的内核实况：入向是否挂着本产品的限速过滤器，以及各类的实测速率。
// err != nil 表示「读不到实况」（例如设备不存在、过滤器在但速率解析不出来），而不是「未下发」——
// 读不到速率时如实报错，不猜一个值（猜出来的 CIR 会掩盖"数据面与配置不一致"）。
func (m *stormManager) Dataplane(ctx context.Context, dev string) (stormDataplaneFact, error) {
	out, err := tcRun(ctx, m.run, "filter", "show", "dev", dev, "ingress")
	if err != nil {
		return stormDataplaneFact{}, fmt.Errorf("tc filter show dev %s ingress: %w（%s）", dev, err, trimOut(out))
	}
	fact := stormDataplaneFact{BroadcastKbps: -1, MulticastKbps: -1}
	var kinds []string
	for _, block := range tcFilterBlocks(out) {
		// 只有带 police 动作、超限丢弃判决、且目的 MAC 是本产品两类之一的块才算我们的过滤器
		// （同一条链上可能有别的族挂的 flower 过滤器）。判决的两种打印写法都认，且**必须都认**：
		// 本产品的广播档是裸 drop（打印 `action drop`）、组播档是 slash 形态（打印
		// `action drop/continue`）——同一接口上两档会同时出现两种形态（见 tcPoliceDrops）。
		if !tcPoliceDrops(block) {
			continue
		}
		switch {
		case strings.Contains(block, stormBroadcastMAC):
			kbps := stormParseRateKbps(block)
			if kbps < 0 {
				return stormDataplaneFact{}, fmt.Errorf(
					"广播入向过滤器在位，但读不到它的限速速率（tc 输出形态无法解析）：%s", trimOut(block))
			}
			fact.BroadcastKbps = kbps
			kinds = append(kinds, fmt.Sprintf("广播 %d kbps", kbps))
		case strings.Contains(block, stormMulticastMAC):
			kbps := stormParseRateKbps(block)
			if kbps < 0 {
				return stormDataplaneFact{}, fmt.Errorf(
					"组播入向过滤器在位，但读不到它的限速速率（tc 输出形态无法解析）：%s", trimOut(block))
			}
			fact.MulticastKbps = kbps
			kinds = append(kinds, fmt.Sprintf("组播 %d kbps", kbps))
		}
	}
	if len(kinds) == 0 {
		fact.Detail = "未下发入向限速"
		return fact, nil
	}
	fact.Attached = true
	fact.Detail = strings.Join(kinds, "、") + "（入向 tc 限速）"
	return fact, nil
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

// addFilter 加一条按目的 MAC 分类的入向限速过滤器；判决由调用方按档给出（stormVerdictBroadcast /
// stormVerdictMulticast，取舍见那里的注释）。
//
// 共同背景（R2-3，真机 spike 实测）：同一条 clsact hook 上内核**按 pref 升序评估、首个判决 ≥0
// 的 filter 命中即返回**；不写判决时 police 对合规包返回 TC_ACT_OK(0)，会把排在后面的整族静默
// 屏蔽。故每一档的判决都要明确写出来（"要不要让包继续走"是有语义的一步，不能靠默认）。
func (m *stormManager) addFilter(ctx context.Context, dev string, pref int, dstMAC string, kbps int, verdict []string) error {
	args := []string{
		"filter", "add", "dev", dev, "ingress", "protocol", "all",
		"pref", strconv.Itoa(pref),
		"flower", "dst_mac", dstMAC,
		"action", "police", "rate", fmt.Sprintf("%dkbit", kbps),
		"burst", strconv.Itoa(stormBurstBytes(kbps)),
	}
	return tcReq(ctx, m.run, append(args, verdict...)...)
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
