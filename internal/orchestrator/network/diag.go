package network

// M3-9：CLI 操作命令的底座实现（ping / traceroute / clear interfaces statistics）。
//
// 底座能力边界（附录 A #36/#330）：
//   - ping：VPP 26.06 的 ping 插件只提供 finished-event API、无发起接口，故经
//     `vppctl`（CLI socket）执行；source <ip> 先经 ip_address_dump 反查接口名。
//     IPv6 经 `vppctl ping ipv6 <addr>`（决策 #330）；未显式给族时照旧由 vppctl 按字面判族。
//   - traceroute：VPP 26.06 无 traceroute 插件/CLI/API，改由宿主侧 raw ICMP 实现
//     （v4=ICMP、v6=ICMPv6，决策 #330）；vrf 参数在经 VPP 的路径上不支持，非空即明确报错。
//   - clear interfaces statistics：VPP binary API sw_interface_clear_stats（~0 = 全部）。
//
// 底座调用藏在 DiagClient / VPPShell / TracerouteProber 接口后，单测注入假实现；
// govpp 实现见 diag_govpp.go，ICMP 实现见 traceroute.go。

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ping 超时。
const pingTimeout = 30 * time.Second

// IfaceAddr 一条接口地址（ip_address_dump）。
type IfaceAddr struct {
	SwIfIndex uint32
	Prefix    string // ip-prefix（CIDR）
}

// DiagClient VPP binary API 诊断能力的最小集合（govpp 适配/单测假实现）。
type DiagClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	SwInterfaceNames() (map[uint32]string, error)
	InterfaceAddresses(isIPv6 bool) ([]IfaceAddr, error)
	ClearInterfaceStats(swIfIndex uint32) error // ~0 = 全部接口
	Close()
}

// VPPShell 执行 VPP CLI 命令（vppctl）；ping 只能由 CLI 发起（附录 A #36）。
type VPPShell interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// PingRequest ping 参数。
type PingRequest struct {
	Host   string
	Source string // 源 IP（按接口地址反查接口）
	VRF    string // VRF 名（映射 IP table id）
	Count  int    // 缺省 defaultPingCount
	IPv6   bool   // 显式 IPv6（vppctl ping ipv6 <addr>；决策 #330）
}

// TracerouteRequest traceroute 参数。
type TracerouteRequest struct {
	Host string
	VRF  string
	IPv6 bool // 显式 IPv6（宿主侧 raw ICMPv6；决策 #330）
}

// Hop 一跳（traceroute 结果）。
type Hop struct {
	TTL     int
	Addr    string
	RTT     time.Duration
	Timeout bool
}

// TracerouteProber 宿主侧 ICMP 探测（真实实现见 traceroute.go，单测注入假实现）。
// ipv6 选择协议族（v4=ICMP / v6=ICMPv6）；决策 #330。
type TracerouteProber interface {
	Trace(ctx context.Context, host string, ipv6 bool, maxTTL int, timeout time.Duration) ([]Hop, error)
}

// Diagnostics 诊断操作（ping/traceroute/clear stats）。
type Diagnostics struct {
	client func() (DiagClient, error)
	shell  VPPShell
	prober TracerouteProber
}

// NewDiagnostics 构造（测试可注入任一部分；client 为 nil 时相关操作报不可用）。
func NewDiagnostics(client func() (DiagClient, error), shell VPPShell, prober TracerouteProber) *Diagnostics {
	return &Diagnostics{client: client, shell: shell, prober: prober}
}

// Diagnostics 返回绑定当前 VPP 连接的诊断操作（nfvisd 装配）。
func (m *Manager) Diagnostics() *Diagnostics {
	return NewDiagnostics(m.DiagClientFunc(), NewVppctlShell(""), NewICMPProber())
}

// Ping 经 VPP L3 发 ICMP echo。返回 vppctl 原始输出（含统计行）。
//
// **平面口径（附录 A #89）**：ping 只覆盖 **VPP 数据面**——目标要能经 VPP 的路由/接口到达。
// 管理口 ens160 属于**内核平面**，VPP 看不到它，于是 VPP 会打印
// `Failed: no egress interface` 并给出 `Statistics: 0 sent, 0 received, 0% packet loss`。
// 这句「0% 丢包」读起来像成功，而判定侧（CLI 的 `%/%%`、`cli-fulltest.sh` 的 `_is_fail`）
// 只看错误行与退出码——**一个包都没发出去却被算作通过**（真机实测：`ping <管理口网关>`
// 返回码 0、无 `%`，冒烟脚本判 ✓）。故此处不再原样放行：
//   - 一个包都没发出去（sent=0）→ 返回错误，让上层给 `%%` 与非零结果；
//   - 输出里明确点出「只覆盖 VPP 数据面」与「管理口请用宿主 ping」，把话说到能照着做。
func (d *Diagnostics) Ping(ctx context.Context, req PingRequest) (string, error) {
	if strings.TrimSpace(req.Host) == "" {
		return "", errors.New("ping 目标地址不能为空")
	}
	if d.shell == nil {
		return "", errors.New("ping 不可用（VPP CLI 未装配）")
	}
	// 显式 IPv6（决策 #330）：VPP CLI 的族选择写在目标之前（`ping ipv6 <addr>`）。
	// 未显式给时照旧只写目标——vppctl 会按地址字面自动判族（IPv6 字面也能走通，
	// 见 test/integration/a3_ipv6_test.go），故不改变既有 IPv4 行为。
	args := []string{"ping"}
	if req.IPv6 {
		args = append(args, "ipv6")
	}
	args = append(args, req.Host)
	if req.Count > 0 {
		args = append(args, "repeat", strconv.Itoa(req.Count))
	}
	if req.VRF != "" {
		args = append(args, "table-id", strconv.FormatUint(uint64(TableID(req.VRF)), 10))
	}
	if req.Source != "" {
		name, err := d.ifaceByAddr(req.Source)
		if err != nil {
			return "", err
		}
		args = append(args, "source", name)
	}
	cctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	out, err := d.shell.Run(cctx, args...)
	if err != nil {
		return out, fmt.Errorf("vppctl ping: %w", err)
	}
	// isV6 供文案区分族：显式 ipv6 或目标字面本身是 IPv6（含冒号）。
	isV6 := req.IPv6 || strings.Contains(req.Host, ":")
	// 未通即失败（附录 A #89/#93）——两条支路都要拦（v4/v6 同口径，汇总行格式一致）：
	//   ① 一个包都没发出去（VPP 无到达目标的接口/路由）：原文的 `0% packet loss` 读起来像通了；
	//   ② 发出了但没有一个应答（`100% packet loss`）：ping 是连通性测试，**没通就是失败**，
	//      否则 `ping <不可达>` 返回 0 会让调用方与判定侧都以为通了（真机实测：VPP ping
	//      自己的回环地址也是 `2 sent, 0 received`）。
	// 判不出汇总行时**不判失败**（输出格式一变就误报，比漏报更糟）。
	if sent, ok := vppPingSent(out); ok {
		if sent == 0 {
			return out + pingNoEgressNote(req.Host, isV6), fmt.Errorf(
				"ping 未发出任何报文（%s 不可经 VPP 到达）：ping 只覆盖 VPP 数据面", req.Host)
		}
		if recv, ok2 := vppPingReceived(out); ok2 && recv == 0 {
			return out + pingUnreachableNote(req.Host, sent, isV6), fmt.Errorf(
				"ping 已发出 %d 个报文但无应答：%s 不可达", sent, req.Host)
		}
	}
	return out, nil
}

// vppPingSent 取 vppctl ping 汇总行里的发包数（`Statistics: N sent, ...`）。
// 判不出（输出格式变了）时返回 ok=false——**判不出就不当作失败**，避免格式一变就误报。
func vppPingSent(out string) (int, bool) {
	m := vppPingSentRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// vppPingReceived 取汇总行里的收包数（`Statistics: N sent, M received, ...`）。
func vppPingReceived(out string) (int, bool) {
	m := vppPingReceivedRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

var (
	vppPingSentRe     = regexp.MustCompile(`Statistics:\s*(\d+)\s+sent`)
	vppPingReceivedRe = regexp.MustCompile(`Statistics:\s*\d+\s+sent,\s*(\d+)\s+received`)
)

// pingNoEgressNote 给操作者的补充说明：解释为什么「没发出去」以及该用什么（附录 A #89）。
// isV6 时按 IPv6 口径说明（决策 #330）：VPP 侧要有 v6 地址/路由与邻居（NDP）才发得出去；
// 宿主侧替代为 `ping -6` / `traceroute ipv6`。
func pingNoEgressNote(host string, isV6 bool) string {
	if isV6 {
		return "\n（nfvis：以上是 VPP 数据面的 IPv6 结果。VPP 里没有能到达 " + host + " 的 v6 接口/路由——\n" +
			" 请确认该 VPP 接口已配 v6 地址、v6 FIB 里有对应路由（`show vrfs <n> routes` 含 v6 前缀）；\n" +
			" 若目标在管理网（内核平面），请用宿主 IPv6 ping（如 `ping -6 -c 4 " + host + "`）；\n" +
			" 若要经 VPP 测，目标须在 VPP 侧有 v6 L3 可达面（邻居需先经 NDP 解析）。`traceroute ipv6` 走宿主侧 ICMPv6。）\n"
	}
	return "\n（nfvis：以上是 VPP 数据面的结果。VPP 里没有能到达 " + host + " 的接口/路由——\n" +
		" 若这是管理口网关，它属于**内核平面**，请用宿主 ping（如 `ping -c 4 " + host + "`）；\n" +
		" 若要经 VPP 测，目标须在 VPP 侧有 L3 可达面。`traceroute` 走宿主侧 ICMP，可直接测管理口。）\n"
}

// pingUnreachableNote 报文发出去了但无人应答时的说明（附录 A #93）：
// 区分「没发出去」与「发了没回」，并给出下一步（isV6 时按 IPv6 口径，决策 #330）。
func pingUnreachableNote(host string, sent int, isV6 bool) string {
	if isV6 {
		return fmt.Sprintf("\n（nfvis：%d 个 IPv6 报文已从 VPP 发出但没有任何应答——%s 不可达。\n"+
			" 请确认对端是否在线、是否放行 ICMPv6、以及 VPP 侧 v6 路由/网关与邻居（NDP）是否正确；\n"+
			" 若目标在管理网（内核平面），请用宿主 IPv6 ping 或 `traceroute ipv6`（宿主侧 ICMPv6）。）\n", sent, host)
	}
	return fmt.Sprintf("\n（nfvis：%d 个报文已从 VPP 发出但没有任何应答——%s 不可达。\n"+
		" 请确认对端是否在线、是否放行 ICMP、以及 VPP 侧的路由/网关是否正确；\n"+
		" 若目标在管理网（内核平面），请用宿主 ping 或 `traceroute`（宿主侧 ICMP）。）\n", sent, host)
}

// Traceroute 宿主侧 ICMP 路径跟踪（v4=ICMP / v6=ICMPv6，决策 #330）。
// vrf 在**两个族**都明确报不支持（附录 A #36/#330）：VPP 26.06 无 traceroute 能力，
// 宿主侧 ICMP 无法经 VPP VRF 转发——不静默降级、不假装支持。
func (d *Diagnostics) Traceroute(ctx context.Context, req TracerouteRequest) (string, error) {
	if strings.TrimSpace(req.Host) == "" {
		return "", errors.New("traceroute 目标地址不能为空")
	}
	fam := "ICMP"
	if req.IPv6 {
		fam = "ICMPv6"
	}
	if req.VRF != "" {
		return "", fmt.Errorf("traceroute 不支持 vrf %q（%s）：VPP 26.06 无 traceroute 能力，宿主侧 ICMP 无法经 VPP VRF 转发；"+
			"若要经 VRF 测 v4 连通性请用 `ping <host> vrf <name>`", req.VRF, fam)
	}
	if d.prober == nil {
		return "", errors.New("traceroute 不可用（宿主侧 ICMP 未装配）")
	}
	hops, err := d.prober.Trace(ctx, req.Host, req.IPv6, 30, time.Second)
	out := FormatTraceroute(req.Host, req.IPv6, hops)
	if err != nil {
		return out, err
	}
	return out, nil
}

// ClearInterfaceStats 清零接口统计；ifname 为空清全部接口。
func (d *Diagnostics) ClearInterfaceStats(ctx context.Context, ifname string) error {
	if d.client == nil {
		return ErrL2Unavailable
	}
	c, err := d.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx := ^uint32(0) // ~0 = 全部接口
	if ifname != "" {
		i, ok, err := c.SwInterfaceIndex(ifname)
		if err != nil {
			return fmt.Errorf("解析接口 %s: %w", ifname, err)
		}
		if !ok {
			return fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, ifname)
		}
		idx = i
	}
	if err := c.ClearInterfaceStats(idx); err != nil {
		return fmt.Errorf("清零接口统计: %w", err)
	}
	return nil
}

// ifaceByAddr 按 IP（可带前缀）反查 VPP 接口名（ping source <ip> 语义，附录 A #36）。
func (d *Diagnostics) ifaceByAddr(ip string) (string, error) {
	if d.client == nil {
		return "", ErrL2Unavailable
	}
	c, err := d.client()
	if err != nil {
		return "", err
	}
	defer c.Close()
	want := strings.TrimSpace(ip)
	if i := strings.IndexByte(want, '/'); i >= 0 {
		want = want[:i]
	}
	var found uint32
	var hit bool
	for _, is6 := range []bool{false, true} {
		addrs, err := c.InterfaceAddresses(is6)
		if err != nil {
			return "", fmt.Errorf("查询 VPP 接口地址: %w", err)
		}
		for _, a := range addrs {
			host := a.Prefix
			if i := strings.IndexByte(host, '/'); i >= 0 {
				host = host[:i]
			}
			if host == want {
				found, hit = a.SwIfIndex, true
				break
			}
		}
		if hit {
			break
		}
	}
	if !hit {
		return "", fmt.Errorf("源地址 %s 不属于任何 VPP 接口（ping source 需为已配置的接口地址）", ip)
	}
	names, err := c.SwInterfaceNames()
	if err != nil {
		return "", fmt.Errorf("查询 VPP 接口名: %w", err)
	}
	name, ok := names[found]
	if !ok || name == "" {
		return "", fmt.Errorf("源地址 %s 对应接口 %d 无名称", ip, found)
	}
	return name, nil
}

// FormatTraceroute 渲染 traceroute 结果（标准风格）；ipv6 时标注协议族（决策 #330）。
func FormatTraceroute(host string, ipv6 bool, hops []Hop) string {
	fam := "宿主侧 ICMP"
	if ipv6 {
		fam = "宿主侧 ICMPv6"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "traceroute to %s, 30 hops max, %s\n", host, fam)
	for _, h := range hops {
		if h.Timeout || h.Addr == "" {
			fmt.Fprintf(&b, "%2d  *\n", h.TTL)
			continue
		}
		fmt.Fprintf(&b, "%2d  %s  %.3f ms\n", h.TTL, h.Addr, float64(h.RTT.Microseconds())/1000)
	}
	return b.String()
}

// vppctlShell 经 vppctl 执行 CLI 命令（socket 缺省 /run/vpp/cli.sock）。
type vppctlShell struct{ socket string }

// NewVppctlShell 返回 vppctl 执行实现（socket 为空用 vppctl 缺省）。
func NewVppctlShell(socket string) VPPShell { return vppctlShell{socket: socket} }

func (s vppctlShell) Run(ctx context.Context, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	full := args
	if s.socket != "" {
		full = append([]string{"-s", s.socket}, args...)
	}
	cmd := exec.CommandContext(ctx, "vppctl", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
