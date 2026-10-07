package netkernel

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// 本文件是内核数据面**转发的前置条件**：内核模式下产品自己就是那台路由器，而
// 「能转发」不是配好接口/路由就自动成立的——真机走查（宿主作对端）实测到两条会**静默**
// 让转发全丢的前置：
//
//  1. **`net.ipv4.ip_forward` 必须为 1**。产品此前从不设置它：干净快照上该值恰好是 1，
//     但那是 **Docker 启动时顺手开的**（dockerd 会置 `net.ipv4.ip_forward=1`）。一旦机器上
//     没装 Docker、或 Docker 未启用，内核数据面**一条都转不出去**，而所有下发命令都报成功。
//  2. **netfilter 的 forward 链必须放行数据面之间的流量**。宿主上 libvirt/Docker 会把
//     `FORWARD` 策略置为 **DROP**（真机实测 `Chain FORWARD (policy DROP)`）——VPP 数据面
//     完全绕开内核 netfilter，所以这条在 vpp 模式下从不暴露；换成内核数据面后它会让
//     「接口起来了、路由也下了、ping 全丢」，且没有任何提示。
//
// 两条都按「产品只对自己的数据面负责」的口径处理：只放行**两侧都是产品数据面设备**的转发
// （绝不放行管理口），且用**更早的优先级**（-10 < filter 的 0）在自己独立的表里 accept，
// 不修改宿主的 FORWARD 策略、也不碰别人的规则。

// forwardTable 本产品的转发放行表（独立表，不与他人的规则混在一起）。
const forwardTable = "nfvis-forward"

// ipv4ForwardSysctl 内核 IPv4 转发开关（内核数据面下产品必须为 1）。
const ipv4ForwardSysctl = "net.ipv4.ip_forward"

// EnsureForwarding 收敛内核数据面的转发前置条件：转发开关 + 数据面之间的 forward 放行。
//
// cfg 用来取「产品数据面设备」清单（bridge / VRF / l3 接口 / vxlan）——只放行这些设备之间
// 的流量；管理口与宿主其它流量一概不碰。
func (p *Provider) EnsureForwarding(ctx context.Context, cfg model.Config) error {
	if err := p.ensureIPForward(ctx); err != nil {
		return err
	}
	return p.ensureForwardAccept(ctx, cfg)
}

// ensureIPForward 确保 `net.ipv4.ip_forward=1`（读回确认；写不动如实报错）。
func (p *Provider) ensureIPForward(ctx context.Context) error {
	out, err := p.run.Run(ctx, "sysctl", "-n", ipv4ForwardSysctl)
	if err == nil && strings.TrimSpace(out) == "1" {
		return nil
	}
	if _, err := p.run.Run(ctx, "sysctl", "-w", ipv4ForwardSysctl+"=1"); err != nil {
		return fmt.Errorf("内核数据面需要 IPv4 转发（%s=1）但设置失败: %w（%s）",
			ipv4ForwardSysctl, err, trimOut(out))
	}
	// 写成功 ≠ 已生效：回读确认（与链路 up 同口径）。
	out, err = p.run.Run(ctx, "sysctl", "-n", ipv4ForwardSysctl)
	if err != nil || strings.TrimSpace(out) != "1" {
		return fmt.Errorf("内核数据面需要 IPv4 转发（%s=1），写入后回读仍为 %q",
			ipv4ForwardSysctl, strings.TrimSpace(out))
	}
	return nil
}

// ensureForwardAccept 重建「数据面设备之间的转发放行」链（幂等：先删后建）。
//
// 链挂在 `hook forward priority -10`：比 iptables/nftables 的 filter（优先级 0）**更早**执行，
// 因此宿主上「FORWARD 策略 DROP」（libvirt/Docker 设的）不会把我们的流量吃掉；
// 同时我们只 accept **两侧都在数据面设备集合里**的包，别的流量照旧走宿主原有策略。
func (p *Provider) ensureForwardAccept(ctx context.Context, cfg model.Config) error {
	devs := dataplaneDevices(cfg)
	// 先清掉本产品的表（重建语义；表是我们自己的，清它不影响宿主规则）。
	if err := nftBestOn(ctx, p.run, "delete", "table", "inet", forwardTable); err != nil {
		return err
	}
	if len(devs) == 0 {
		return nil // 没有数据面设备 ⇒ 没有需要放行的转发
	}
	if err := nftIdemOn(ctx, p.run, "add", "table", "inet", forwardTable); err != nil {
		return err
	}
	spec := "type filter hook forward priority -10 ; policy accept ;"
	if err := nftIdemOn(ctx, p.run, "add", "chain", "inet", forwardTable, "accept-dp",
		"{", spec, "}"); err != nil {
		return err
	}
	set := "{ " + strings.Join(devs, ", ") + " }"
	return nftIdemOn(ctx, p.run, "add", "rule", "inet", forwardTable, "accept-dp",
		"iifname", set, "oifname", set, "accept")
}

// dataplaneDevices 产品在内核数据面下自持的设备名（转发放行的**唯一**依据）。
//
// 有意**不含管理口**：管理口的转发由宿主原有策略决定，产品不替它开洞。
func dataplaneDevices(cfg model.Config) []string {
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" {
			seen[LinkName(n)] = true
		}
	}
	for _, vs := range cfg.VirtualSwitches {
		add(vs.Name) // L2 交换机 → bridge；L3 交换机 → VRF 设备（同名）
		if vs.Type != "l3" && vs.Gateway != nil {
			if vs.Gateway.Vrf != "" {
				add(vs.Gateway.Vrf)
			} else {
				add(GatewayVRFName(vs.Name))
			}
		}
	}
	for _, vrf := range cfg.Vrfs {
		add(vrf.Name)
		for _, li := range vrf.L3Interfaces {
			add(l3DeviceName(li))
		}
	}
	for _, b := range cfg.Bonds {
		add(b.Name)
	}
	for _, vx := range cfg.VxlanTunnels {
		add(vx.Name)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	// 稳定顺序：nft 匿名集合的文本可复现（便于读视图与测试对照）。
	sort.Strings(out)
	return out
}
