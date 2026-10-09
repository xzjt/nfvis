package netkernel

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 内核数据面下「已声明却没进数据面」的原因事实（决策 #431）。
//
// 由来（真机走查）：数据面切到内核后，VPP 时代接管过的口仍留在 vfio-pci（内核里没有它），
// 或被宿主链路策略（netplan 的 `activation-mode: off` → systemd-networkd 的
// `ActivationPolicy=always-down`）压成 down——`show interfaces physical` 此前只标
// 「已声明未生效」，真正原因只在恢复收敛告警文案与 journal 里，读视图看不到。
//
// 本文件提供**可独立核对的事实**：驱动占用（vfio 残留）与 networkd 链路策略。
// 只给探测到的事实，不给用户可见文案（文案在 API 读视图层渲染）。

// networkdNetDir systemd-networkd 生成的网络单元目录（`netplan apply` 写这里；测试可改）。
var networkdNetDir = "/run/systemd/network"

// KernelIfNotInDPReason 内核数据面下，某物理口「已声明却未进数据面」的原因（决策 #431）。
//
// 判据两条（任一命中即给出原因；都取不到返回 ok=false，读视图沿用既有「已声明未生效」）：
//
//   - vfio 残留：口内核里没有、仍被某个 DPDK 驱动占用（复用决策 #426③ 的探测，含 PCI）；
//   - networkd 持有为 down：networkd 对该口施加了强制 down 的链路策略。
//
// 探测通道不通（未注入 heldPort、读不到 networkd 单元）一律不猜——返回「取不到」。
func (p *Provider) KernelIfNotInDPReason(name string) (network.KernelIfReason, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return network.KernelIfReason{}, false
	}
	if p.heldPort != nil {
		if drv, pci, ok := p.heldPort(name); ok && drv != "" {
			return network.KernelIfReason{Kind: network.KernelIfReasonVFIO, Driver: drv, PCI: pci}, true
		}
	}
	if held, err := NetworkdHoldsDown(name); err == nil && held {
		return network.KernelIfReason{Kind: network.KernelIfReasonNetworkdDown}, true
	}
	return network.KernelIfReason{}, false
}

// NetworkdHoldsDown 判定 systemd-networkd 是否对该口施加了「强制 down」的链路策略。
//
// 事实源是 networkd 生成的 `.network` 单元（netplan 的 `activation-mode: off` 会写成
// `[Link] ActivationPolicy=always-down`）——这是真正把链路压回 down 的运行时事实。
// 目录/文件读不到时返回错误（调用方按「取不到」处理，不猜）。
func NetworkdHoldsDown(ifname string) (bool, error) {
	if ifname == "" {
		return false, nil
	}
	entries, err := os.ReadDir(networkdNetDir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".network") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(networkdNetDir, e.Name()))
		if err != nil {
			continue
		}
		if networkdUnitHoldsDown(string(b), ifname) {
			return true, nil
		}
	}
	return false, nil
}

// networkdUnitHoldsDown 解析单个 networkd 单元（纯函数）：`[Match] Name=<if>` 命中该口、
// 且 `[Link] ActivationPolicy` 为 always-down/down 时返回 true。
//
// 只认这两项事实：匹配段命中 + 策略为强制 down。其余（`[Network]` 等）一概不看——
// 本判据要回答的是「是不是宿主链路策略把它压着」，不是解析整个单元。
func networkdUnitHoldsDown(content, ifname string) bool {
	section := ""
	matched := false
	holds := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch section {
		case "match":
			if strings.EqualFold(k, "Name") {
				for _, n := range strings.Fields(v) {
					if n == ifname {
						matched = true
					}
				}
			}
		case "link":
			if strings.EqualFold(k, "ActivationPolicy") {
				switch strings.ToLower(v) {
				case "always-down", "down":
					holds = true
				}
			}
		}
	}
	return matched && holds
}
