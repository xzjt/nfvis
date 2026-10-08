package netkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 本文件是内核数据面**转发的前置条件**：内核模式下产品自己就是那台路由器，而
// 「能转发」不是配好接口/路由就自动成立的——真机走查（宿主作对端）实测到两条会**静默**
// 让转发全丢的前置：
//
//  1. **转发开关必须为 1**：`net.ipv4.ip_forward` 与 `net.ipv6.conf.all.forwarding`。
//     干净快照上 v4 值恰好是 1，但那是 **Docker 启动时顺手开的**（dockerd 会置
//     `net.ipv4.ip_forward=1`）；一旦机器上没装 Docker、或 Docker 未启用，内核数据面
//     **一条都转不出去**，而所有下发命令都报成功。v6 更彻底：Ubuntu 缺省就是 0，
//     而产品在内核数据面下支持 v6 三层（规格 FR-NET-013 的静态路由 v4+v6）——
//     少了这条，v6 地址/路由/ACL 全都「下发成功、一个包不转发」（R2-10）。
//  2. **宿主 FORWARD 链策略不能把数据面流量丢掉**。宿主上 libvirt/Docker 会把
//     `FORWARD` 策略置为 **DROP**（真机实测 `Chain FORWARD (policy DROP)`）——VPP 数据面
//     完全绕开内核 netfilter，所以这条在 vpp 模式下从不暴露；换成内核数据面后它会让
//     「接口起来了、路由也下了、ping 全丢」，且没有任何提示（R2-2）。
//
// ⚠️ **归因更正（R2-2）**：本文件原先自建的 `inet nfvis-forward`（hook forward priority -10）
// **不能**豁免宿主的 FORWARD 策略 DROP——netfilter 语义上同一 hook 的多个 base chain 是
// **相互独立**的 hook 项，accept 只让包继续走下一个 hook 项，后续链（iptables 的 FORWARD /
// nft 的 filter）的 drop/策略 drop 仍然生效。本仓库自己的结论
// （`internal/system/firewall.go` 注释与 `docs/evidence/v2-round169-d388-host-firewall.txt`
// 的真机 A/B：另一张表 `-10` accept tcp/22，本表 drop-all 仍把 SSH 拦死）已证过同一件事。
// 自建链因此**保留但不再是「修复」**：它只表达「产品数据面设备对之间的意图」，真正生效的
// 放行必须写进宿主那条链——产品能做的、也是本文件做的，是**如实检测并告警**（见
// checkHostForwardPolicy），把照做命令给出去，而不是假装已经放行。

// forwardTable 本产品的转发放行表（独立表，不与他人的规则混在一起）。
const forwardTable = "nfvis-forward"

// 内核转发开关（内核数据面下产品必须为 1；v4/v6 缺一不可）。
const (
	ipv4ForwardSysctl = "net.ipv4.ip_forward"
	ipv6ForwardSysctl = "net.ipv6.conf.all.forwarding"
)

// EnsureForwarding 收敛内核数据面的转发前置条件：转发开关（v4+v6）+ 数据面之间的 forward 放行
// + 宿主 FORWARD 策略检测。
//
// cfg 用来取「产品数据面设备」清单（bridge / VRF / l3 接口 / vxlan）——只放行这些设备之间
// 的流量；管理口与宿主其它流量一概不碰。
func (p *Provider) EnsureForwarding(ctx context.Context, cfg model.Config) error {
	if err := p.ensureIPForward(ctx, ipv4ForwardSysctl); err != nil {
		return err
	}
	if err := p.ensureIPForward(ctx, ipv6ForwardSysctl); err != nil {
		return err
	}
	if err := p.ensureForwardAccept(ctx, cfg); err != nil {
		return err
	}
	p.checkHostForwardPolicy(ctx, cfg)
	return nil
}

// ensureIPForward 确保给定转发开关为 1（读回确认；写不动如实报错）。
//
// 写成功 ≠ 已生效：配置了 `net.ipv6.conf.*.forwarding=0` 的机器上 sysctl -w 会「成功」，
// 回读仍是 0（内核按 per-interface 缺省收敛），故必须回读。
func (p *Provider) ensureIPForward(ctx context.Context, key string) error {
	out, err := p.run.Run(ctx, "sysctl", "-n", key)
	if err == nil && strings.TrimSpace(out) == "1" {
		return nil
	}
	if _, err := p.run.Run(ctx, "sysctl", "-w", key+"=1"); err != nil {
		return fmt.Errorf("内核数据面需要转发开关 %s=1 但设置失败: %w（%s）", key, err, trimOut(out))
	}
	out, err = p.run.Run(ctx, "sysctl", "-n", key)
	if err != nil || strings.TrimSpace(out) != "1" {
		return fmt.Errorf("内核数据面需要转发开关 %s=1，写入后回读仍为 %q", key, strings.TrimSpace(out))
	}
	return nil
}

// ensureForwardAccept 收敛「数据面设备之间的转发放行」链。两条口径：
//
//   - **无数据面设备 ⇒ 回收整张表**（`nft delete table inet nfvis-forward`，不存在按已达成）：
//     设备清空后（配置里已无交换机/接口/VRF）还留一张空表/空链就是「配置已清空、数据面还有
//     产品对象」的残留——与 #410 F3 的 `delete nat` 空声明整表回收同一口径。
//   - **有设备 ⇒ 幂等重写**（表/链在位 + flush + 按当前设备集重写规则）：R2-16 的窗口要求
//     ——不删表重建，15s 巡检不制造「表不存在」的空窗。
//
// 链挂在 `hook forward priority -10`；**它不能豁免宿主别的链的 drop**（见文件头 R2-2 的归因
// 更正），保留它的意义是把「产品打算放行哪些设备对」表达成一个可见、可复核的对象，真正生效
// 的放行由宿主那条链负责（产品检测到 DROP 策略时如实告警并给照做命令）。
func (p *Provider) ensureForwardAccept(ctx context.Context, cfg model.Config) error {
	devs := dataplaneDevices(cfg)
	if len(devs) == 0 {
		return nftBestOn(ctx, p.run, "delete", "table", "inet", forwardTable)
	}
	if err := nftIdemOn(ctx, p.run, "add", "table", "inet", forwardTable); err != nil {
		return err
	}
	spec := "type filter hook forward priority -10 ; policy accept ;"
	if err := nftIdemOn(ctx, p.run, "add", "chain", "inet", forwardTable, "accept-dp",
		"{", spec, "}"); err != nil {
		return err
	}
	if err := nftReqOn(ctx, p.run, "flush", "chain", "inet", forwardTable, "accept-dp"); err != nil {
		return err
	}
	set := "{ " + strings.Join(devs, ", ") + " }"
	return nftIdemOn(ctx, p.run, "add", "rule", "inet", forwardTable, "accept-dp",
		"iifname", set, "oifname", set, "accept")
}

// checkHostForwardPolicy 检测宿主 FORWARD 链策略并维护告警（R2-2）。
//
// 产品不再假装自建链能豁免宿主 drop：策略为 DROP 且**确有数据面设备需要互转**时落 warning
// 告警（照做命令写进文案）；策略不再是 DROP、或没有数据面设备时同 source 消解。
// 两条读取路径（iptables / nft）都读不到时什么都不做——不猜、不误报。
func (p *Provider) checkHostForwardPolicy(ctx context.Context, cfg model.Config) {
	if p.alarms == nil {
		return
	}
	policy, known := p.hostForwardPolicy(ctx)
	if !known {
		return
	}
	devs := dataplaneDevices(cfg)
	if policy != "drop" || len(devs) == 0 {
		p.alarms.Resolve(alarmScopeForwarding, AlarmForwardPolicyDrop, forwardPolicySource)
		return
	}
	p.alarms.Raise(alarmScopeForwarding, network.SeverityWarning, AlarmForwardPolicyDrop,
		forwardPolicyDropMessage(devs), forwardPolicySource)
}

// hostForwardPolicy 读宿主 FORWARD 链的策略（小写）。known=false 表示两条路径都读不到。
//
// 优先 iptables（libvirt/Docker 设策略的常见形态就是 iptables 的 `-P FORWARD DROP`）；
// 机器上没有 iptables 时读 nft 的 `ip filter FORWARD` 链的 policy。两者都读不到就不猜。
func (p *Provider) hostForwardPolicy(ctx context.Context) (string, bool) {
	if out, err := p.run.Run(ctx, "iptables", "-S", "FORWARD"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			// 形如 `-P FORWARD DROP`
			if len(fields) >= 3 && fields[0] == "-P" {
				return strings.ToLower(fields[2]), true
			}
		}
	}
	if out, err := p.run.Run(ctx, "nft", "-j", "list", "chain", "ip", "filter", "FORWARD"); err == nil {
		var doc struct {
			Nftables []struct {
				Chain *struct {
					Policy string `json:"policy"`
				} `json:"chain"`
			} `json:"nftables"`
		}
		if json.Unmarshal([]byte(out), &doc) == nil {
			for _, item := range doc.Nftables {
				if item.Chain != nil && item.Chain.Policy != "" {
					return strings.ToLower(item.Chain.Policy), true
				}
			}
		}
	}
	return "", false
}

// forwardPolicyDropMessage 宿主 FORWARD 策略 DROP 的告警文案（含照做命令）。
//
// 逐对放行是必须的：宿主链的放行按 src/dst 设备对生效（`-i/-o`），一对写反都不通；
// 设备多时只给前几对示例 + 总数，避免告警文案过长以至于读不完。
func forwardPolicyDropMessage(devs []string) string {
	var b strings.Builder
	b.WriteString("宿主 FORWARD 链策略为 DROP：同 hook 的 base chain 相互独立，本产品自建链的 accept " +
		"不能豁免它——内核数据面下数据面设备之间的转发会被宿主整体丢掉（接口/路由都对、ping 全丢）。" +
		"请把放行写进宿主真正生效的 FORWARD 链（逐对执行，双向都要）：")
	total, shown := 0, 0
	for _, a := range devs {
		for _, o := range devs {
			if a == o {
				continue
			}
			total++
			if shown < 3 {
				if shown > 0 {
					b.WriteString("；")
				}
				fmt.Fprintf(&b, "iptables -I FORWARD 1 -i %s -o %s -j ACCEPT", a, o)
				shown++
			}
		}
	}
	if total == 0 {
		b.WriteString("（当前没有数据面设备）")
	} else if total > shown {
		fmt.Fprintf(&b, "；……共 %d 对", total)
	}
	b.WriteString("（nft 等价写法：nft insert rule ip filter FORWARD iifname <a> oifname <b> accept）")
	return b.String()
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
			// 派生名超长（>15）的声明压根下不到数据面（下发路径会如实报错），
			// 这里也就没有「设备之间的转发」可放行——跳过而不是编造一个名字。
			if dev, err := l3DeviceNameChecked(li); err == nil {
				add(dev)
			}
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
