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
	rt := map[string]any{
		"available":           true,
		"tag":                 dp.Tag,
		"tag_present":         dp.TagPresent,
		"expected_rule_count": 2*len(macs) + 2, // 每 MAC 两条 permit + 显式 deny-all 两条
	}
	line := "端口安全 数据面: "
	switch {
	case !dp.TagPresent:
		line += "ACL 未在数据面（未收敛）"
	default:
		rt["acl_index"] = dp.ACLIndex
		rt["rule_count"] = dp.RuleCount
		line += fmt.Sprintf("ACL 在场（索引 %d，规则 %d 条）", dp.ACLIndex, dp.RuleCount)
	}
	if dp.Bound {
		rt["bound"] = true
		rt["bound_index"] = dp.BoundIndex
		line += "；接口已绑定"
	} else {
		rt["bound"] = false
		if dp.BoundIndex != 0 {
			rt["bound_index"] = dp.BoundIndex
			line += fmt.Sprintf("；接口**未绑定**（绑定槽上是其它 macip ACL 索引 %d——与 L3 接口 ACL 的槽冲突需处置）", dp.BoundIndex)
		} else {
			line += "；接口**未绑定**（未收敛）"
		}
	}
	b.WriteString(line + "\n")
	// macip 无逐规则命中计数：如实说明（插件不提供；不显示 0 冒充）。
	b.WriteString("端口安全 计数: 不提供命中数（macip ACL 无逐规则计数，插件不发布该类计数——如实说明）\n")

	out["port_security"] = structCfg
	out["port_security_runtime"] = rt
	return b.String(), out
}
