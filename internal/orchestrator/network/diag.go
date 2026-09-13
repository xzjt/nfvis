package network

// M3-9：CLI 操作命令的底座实现（ping / traceroute / clear interfaces statistics）。
//
// 底座能力边界（附录 A #36）：
//   - ping：VPP 26.06 的 ping 插件只提供 finished-event API、无发起接口，故经
//     `vppctl`（CLI socket）执行；source <ip> 先经 ip_address_dump 反查接口名。
//   - traceroute：VPP 26.06 无 traceroute 插件/CLI/API，改由宿主侧 raw ICMP 实现；
//     vrf 参数在经 VPP 的路径上不支持，非空即明确报错。
//   - clear interfaces statistics：VPP binary API sw_interface_clear_stats（~0 = 全部）。
//
// 底座调用藏在 DiagClient / VPPShell / TracerouteProber 接口后，单测注入假实现；
// govpp 实现见 diag_govpp.go，ICMP 实现见 traceroute.go。

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
}

// TracerouteRequest traceroute 参数。
type TracerouteRequest struct {
	Host string
	VRF  string
}

// Hop 一跳（traceroute 结果）。
type Hop struct {
	TTL     int
	Addr    string
	RTT     time.Duration
	Timeout bool
}

// TracerouteProber 宿主侧 ICMP 探测（真实实现见 traceroute.go，单测注入假实现）。
type TracerouteProber interface {
	Trace(ctx context.Context, host string, maxTTL int, timeout time.Duration) ([]Hop, error)
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
func (d *Diagnostics) Ping(ctx context.Context, req PingRequest) (string, error) {
	if strings.TrimSpace(req.Host) == "" {
		return "", errors.New("ping 目标地址不能为空")
	}
	if d.shell == nil {
		return "", errors.New("ping 不可用（VPP CLI 未装配）")
	}
	args := []string{"ping", req.Host}
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
	return out, nil
}

// Traceroute 宿主侧 ICMP 路径跟踪。vrf 非空明确报不支持（附录 A #36）。
func (d *Diagnostics) Traceroute(ctx context.Context, req TracerouteRequest) (string, error) {
	if strings.TrimSpace(req.Host) == "" {
		return "", errors.New("traceroute 目标地址不能为空")
	}
	if req.VRF != "" {
		return "", fmt.Errorf("traceroute 不支持 vrf %q：VPP 26.06 无 traceroute 能力，宿主侧 ICMP 无法经 VPP VRF 转发（附录 A #36）", req.VRF)
	}
	if d.prober == nil {
		return "", errors.New("traceroute 不可用（宿主侧 ICMP 未装配）")
	}
	hops, err := d.prober.Trace(ctx, req.Host, 30, time.Second)
	out := FormatTraceroute(req.Host, hops)
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
			return fmt.Errorf("%w: %s（是否未由 DPDK 接管？）", ErrIfaceUnavailable, ifname)
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

// FormatTraceroute 渲染 traceroute 结果（标准风格）。
func FormatTraceroute(host string, hops []Hop) string {
	var b strings.Builder
	fmt.Fprintf(&b, "traceroute to %s, 30 hops max, 宿主侧 ICMP\n", host)
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
