package api

// 决策 #389：端口安全（per-port 允许源 MAC 白名单）的 CLI 读视图。
//
// 三面同源：配置字段走模型（`interfaces[].port_security`，REST 同源）；数据面实况走
// `network.PortSecProvider` 的实测读数（macip ACL 按 tag 反查 + 接口绑定实况）；
// 本文件只做渲染，不另查一遍。
//
// 如实口径：
//   - macip **无逐规则命中计数**（插件 err 族为空，round170 真机实证）——块内固定如实
//     说明「不提供命中数」，不编造 0；
//   - 数据面读数不可核对（未接入/接口不在数据面/查询失败）时写明原因；
//   - 「已绑定」以绑定实况与 tag 反查**同索引**为准：槽被别的 macip ACL（如 #341 的
//     伴随 ACL）占用时如实显示未绑定——这本身就是需要处置的实况。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// PortSecRuntime 端口安全的数据面实况读物（决策 #389）。nil = 未接入（块内如实说明）。
type PortSecRuntime interface {
	PortSecDataplane(ctx context.Context, ifname string) (network.PortSecDataplane, bool)
}

// setPortSec 注入端口安全数据面读物（CLI detail 块用）。
func (x *cliExecutor) setPortSec(s PortSecRuntime) { x.portSec = s }

// portSecConfigSummary 配置侧摘要：白名单 MAC 列表（空格分隔，归一小写形态）。
func portSecConfigSummary(macs []model.PortSecMAC) string {
	parts := make([]string, 0, len(macs))
	for _, m := range macs {
		parts = append(parts, string(m))
	}
	return strings.Join(parts, " ")
}

// portSecBlock 渲染接口的端口安全块（配置 + 数据面实况）。返回文本行与结构化视图
// （配置数组与 REST 同形；数据面实况另置 port_security_runtime）。
func (x *cliExecutor) portSecBlock(ifc model.InterfaceConfig) (string, map[string]any) {
	macs := make([]string, 0, len(ifc.PortSecurity))
	for _, m := range ifc.PortSecurity {
		macs = append(macs, string(m))
	}
	structCfg := map[string]any{"allowed_macs": macs}

	var b strings.Builder
	fmt.Fprintf(&b, "端口安全: 白名单 %d 条（%s）——非白名单源 MAC 一律丢弃（L2 入向）\n",
		len(macs), portSecConfigSummary(ifc.PortSecurity))

	out := map[string]any{"port_security_runtime": nil}
	if x.portSec == nil {
		b.WriteString("端口安全 数据面: 未接入（无法核对实测值）\n")
		out["port_security"] = structCfg
		return b.String(), out
	}
	dp, _ := x.portSec.PortSecDataplane(context.Background(), ifc.Name)
	if !dp.Available {
		fmt.Fprintf(&b, "端口安全 数据面: 不可核对（%s）\n", dp.Reason)
		out["port_security"] = structCfg
		out["port_security_runtime"] = map[string]any{"available": false, "reason": dp.Reason}
		return b.String(), out
	}
	// 内核数据面（nftables 链）与 VPP（macip ACL）是两套形态：期望规则条数、命名与索引的有无
	// 都不同，按数据面口径分叉渲染，避免拿 VPP 的账去对内核的现场（R2-15②）。
	kernel := x.dpMode() == model.DataPlaneKernel
	rt := map[string]any{
		"available":   true,
		"tag":         dp.Tag,
		"tag_present": dp.TagPresent,
	}
	if kernel {
		// 内核侧整段白名单就是**一条** nftables 规则（`ether saddr != { … } drop`）：
		// 期望条数按内核形态给（套 VPP 的 2×白名单+2 会凭空报出"少了 4 条规则"的假错位）。
		rt["expected_rule_count"] = 1
	} else {
		rt["expected_rule_count"] = 2*len(macs) + 2 // 每 MAC 两条 permit + 显式 deny-all 两条
	}
	line := "端口安全 数据面: "
	switch {
	case !dp.TagPresent:
		if kernel {
			line += "白名单链未在场（未收敛）"
		} else {
			line += "ACL 未在数据面（未收敛）"
		}
	default:
		rt["rule_count"] = dp.RuleCount
		if kernel {
			// 内核侧没有 macip ACL 索引这回事（ACLIndex 恒 0 不是"读到索引 0"）：不打印索引，
			// 用链名定位对象。
			line += fmt.Sprintf("%s 在场（规则 %d 条）", dp.Tag, dp.RuleCount)
		} else {
			rt["acl_index"] = dp.ACLIndex
			line += fmt.Sprintf("ACL 在场（索引 %d，规则 %d 条）", dp.ACLIndex, dp.RuleCount)
		}
	}
	if dp.Bound {
		rt["bound"] = true
		if kernel {
			line += "；白名单链已挂在接口上"
		} else {
			rt["bound_index"] = dp.BoundIndex
			line += "；接口已绑定"
		}
	} else {
		rt["bound"] = false
		switch {
		case kernel:
			line += "；白名单链未挂上（未收敛）"
		case dp.BoundIndex != 0:
			rt["bound_index"] = dp.BoundIndex
			line += fmt.Sprintf("；接口**未绑定**（绑定槽上是其它 macip ACL 索引 %d——与 L3 接口 ACL 的槽冲突需处置）", dp.BoundIndex)
		default:
			line += "；接口**未绑定**（未收敛）"
		}
	}
	b.WriteString(line + "\n")
	// 命中计数：两套数据面都没有可读的逐规则计数——如实说明，不显示 0 冒充。
	if kernel {
		b.WriteString("端口安全 计数: 不读命中数（内核 nftables 白名单规则未挂 counter，如实不编造 0）\n")
	} else {
		b.WriteString("端口安全 计数: 不提供命中数（macip ACL 无逐规则计数，插件不发布该类计数——如实说明）\n")
	}

	out["port_security"] = structCfg
	out["port_security_runtime"] = rt
	return b.String(), out
}
