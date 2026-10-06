package api

// 管理面主机防火墙读视图（决策 #388）——CLI `show system firewall` 与 REST
// `GET /system/firewall` **同一实现**（三面同源；Web 系统页只读卡消费同一端点）。
//
// 数据面实况经 FirewallRuntime 注入（真机实现 = internal/system.FirewallApplier）：
// 表缺失/规则集合不一致/检测到手工修改/nft 不可用等一律 applied=false + error 如实说明，
// **不把「读不出来」答成「没有」**；逐规则计数仅在可读时出现。

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// FirewallRuntime 主机防火墙数据面读数（决策 #388）。nil = 未接入（读视图如实说明）。
type FirewallRuntime interface {
	Read(ctx context.Context, cfg model.Config) ksys.FirewallState
}

// setFirewall 注入读视图构建器（CLI 渲染与 REST 同一实现，与 metricsHistory 同法）。
func (x *cliExecutor) setFirewall(view func() map[string]any) { x.firewallView = view }

// firewallView 构建主机防火墙读视图（形状对齐 openapi FirewallStatus）。
func (s *Server) firewallView() map[string]any {
	view := map[string]any{
		"enabled":        false,
		"default_policy": "accept",
		"applied":        false,
		"rules":          []any{},
		"reserved":       ksys.FirewallReserved,
		"counters_note":  ksys.FirewallCountersNote,
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		view["error"] = "读取配置失败：" + err.Error()
		return view
	}
	fw := cfg.FirewallOf()
	view["enabled"] = fw.FirewallEnabled()
	view["default_policy"] = fw.FirewallPolicy()
	if mgmt := cfg.MgmtInterfaceOf(); mgmt != "" {
		view["mgmt_interface"] = mgmt
	}
	var state ksys.FirewallState
	if s.firewall == nil {
		state.Error = "主机防火墙下发未接入（无法核对数据面实况）"
	} else {
		state = s.firewall.Read(context.Background(), cfg)
	}
	view["applied"] = state.Applied
	if state.Error != "" {
		view["error"] = state.Error
	}
	// 规则按 seq 升序呈现（数据面首命中顺序；与渲染脚本的排序同口径）。
	var rules []model.FirewallRule
	if fw != nil {
		rules = append(rules, fw.Rules...)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Seq < rules[j].Seq })
	items := make([]any, 0, len(rules))
	for _, r := range rules {
		item := map[string]any{"seq": r.Seq, "action": r.Action}
		if r.Source != "" {
			item["source"] = r.Source
		}
		if r.Protocol != "" {
			item["protocol"] = r.Protocol
		}
		if r.Port != 0 {
			item["port"] = r.Port
		}
		if c, ok := state.Counters[r.Seq]; ok { // 仅计数可读时出现（不编造 0）
			item["packets"] = c.Packets
			item["bytes"] = c.Bytes
		}
		items = append(items, item)
	}
	view["rules"] = items
	return view
}

// firewallViewFor 调用注入的读视图构建器；未注入（测试未接）时回一份如实的不可用视图。
func (x *cliExecutor) firewallViewFor() map[string]any {
	if x.firewallView == nil {
		return map[string]any{
			"enabled":        false,
			"default_policy": "accept",
			"applied":        false,
			"error":          "主机防火墙读视图未接入",
			"rules":          []any{},
			"reserved":       ksys.FirewallReserved,
			"counters_note":  ksys.FirewallCountersNote,
		}
	}
	return x.firewallView()
}

// handleGetFirewall GET /api/v1/system/firewall（决策 #388，read-only）。
// 未配置/未接入/未收敛**恒 200**：读取失败也是状态，由 applied=false + error 如实说明
// （与 /metrics/history 同口径，不把「读不出来」答成 5xx 之外的静默空表）。
func (s *Server) handleGetFirewall(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.firewallView())
}

// renderSystemFirewall `show system firewall`（决策 #388，R）。
//
// 未配置：如实报「未配置（管理口入向不做任何过滤）」+ 前置提示；已配置：默认策略、管理口、
// 下发状态（未收敛给原因）、规则表（含逐规则 packets/bytes，可读才显示）、保留项、计数口径。
func (x *cliExecutor) renderSystemFirewall() string {
	view := x.firewallViewFor()
	x.structured = view
	enabled, _ := view["enabled"].(bool)
	policy, _ := view["default_policy"].(string)
	applied, _ := view["applied"].(bool)
	errText, _ := view["error"].(string)

	var b strings.Builder
	if !enabled {
		b.WriteString("主机防火墙: 未配置（管理口入向不做任何过滤）\n")
		if !applied && errText != "" {
			// 未配置但数据面有残留表等不一致：如实说明（不把说不清的状态藏起来）。
			fmt.Fprintf(&b, "下发状态:   未收敛（%s）\n", errText)
		}
		b.WriteString("提示: 前置＝先声明管理口（set system management interface <ifname>）；" +
			"配规则/默认策略后用 show system firewall 复核，非 console 会话须 commit confirmed\n")
		b.WriteString("计数口径: " + ksys.FirewallCountersNote + "\n")
		return b.String()
	}
	pol := policy
	if pol == "drop" {
		pol = "drop（白名单模式）"
	} else {
		pol = "accept（缺省放行）"
	}
	fmt.Fprintf(&b, "主机防火墙: 已配置（默认策略 %s）\n", pol)
	if mgmt, _ := view["mgmt_interface"].(string); mgmt != "" {
		fmt.Fprintf(&b, "管理口:     %s（仅此口入向参与过滤）\n", mgmt)
	} else {
		b.WriteString("管理口:     （未声明：防火墙无从生效，请 set system management interface <ifname>）\n")
	}
	if applied {
		b.WriteString("下发状态:   已收敛\n")
	} else {
		fmt.Fprintf(&b, "下发状态:   未收敛（%s）\n", errText)
	}
	rules, _ := view["rules"].([]any)
	fmt.Fprintf(&b, "%-6s %-8s %-18s %-9s %-7s %s\n", "Seq", "动作", "来源", "协议", "端口", "命中(packets/bytes)")
	if len(rules) == 0 {
		b.WriteString("（无规则：仅默认策略与保留项生效）\n")
	}
	for _, it := range rules {
		r, ok := it.(map[string]any)
		if !ok {
			continue
		}
		counter := "-"
		if p, ok := r["packets"]; ok {
			counter = fmt.Sprintf("%v / %v", p, r["bytes"])
		}
		fmt.Fprintf(&b, "%-6v %-8v %-18v %-9v %-7v %s\n",
			r["seq"], r["action"], dashAny(r["source"]), dashAny(r["protocol"]), dashAny(r["port"]), counter)
	}
	b.WriteString("保留项（用户规则不可覆盖）:\n")
	for _, s := range ksys.FirewallReserved {
		b.WriteString("  - " + s + "\n")
	}
	b.WriteString("计数口径: " + ksys.FirewallCountersNote + "\n")
	return b.String()
}

// dashAny 读视图单元格的空值占位。
func dashAny(v any) string {
	if s, ok := v.(string); ok {
		if s == "" {
			return "-"
		}
		return s
	}
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%v", v)
}
