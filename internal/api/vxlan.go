package api

// VXLAN 隧道读视图（决策 #383，FR-NET-019）。
//
// 三面同源：
//   · CLI  `show vxlan tunnels`
//   · REST `GET /vxlan-tunnels`
//   · Web  控制台网络对象页「VXLAN 隧道」卡（只读）
//
// 口径：**配置声明 × 数据面实况**逐条对照——配置给名/VNI/下垫地址/端口/交换机，
// 运行态按 (vni, local, remote, dst_port) 元组键与 `vxlan_tunnel_dump` 匹配（不依赖
// VPP 接口名——接口名由 instance 生成、产品不用它）。运行态不可用（数据面未连接/未装配）
// 时如实报「运行态不可用」，**不**把 in_vpp 报成 false 冒充「未收敛」。

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// VxlanRuntime VXLAN 隧道运行态读物（决策 #383；编排器装配注入，nil = 读视图如实报未接入）。
// *network.L2Network 天然满足本接口（装配处直接传入）；单测注入假实现。
type VxlanRuntime interface {
	// VxlanStates 实际存在的隧道，按元组键（network.VxlanTupleKey）索引。
	VxlanStates(ctx context.Context) (map[string]network.VxlanState, error)
}

// vxlanTunnelView 一条隧道的读视图（配置 + 运行态）。
type vxlanTunnelView struct {
	Name          string `json:"name"`
	Vni           int    `json:"vni"`
	Local         string `json:"local"`
	Remote        string `json:"remote"`
	DstPort       int    `json:"dst_port"`
	VirtualSwitch string `json:"virtual_switch,omitempty"`
	// RuntimeAvailable=false 时 InVPP/SwIfIndex/Instance 不可信（运行态不可用，不是「不在 VPP」）。
	RuntimeAvailable bool   `json:"runtime_available"`
	InVPP            bool   `json:"in_vpp"`
	SwIfIndex        uint32 `json:"sw_if_index,omitempty"`
	Instance         uint32 `json:"instance,omitempty"`
}

// vxlanTunnelsView 列表读视图：tunnels 恒为数组（空时为空数组，不发 null）。
type vxlanTunnelsView struct {
	Tunnels          []vxlanTunnelView `json:"tunnels"`
	RuntimeAvailable bool              `json:"runtime_available"`
	RuntimeReason    string            `json:"runtime_reason,omitempty"`
}

// vxlanTunnelsViewOf 从配置声明与运行态构建读视图（CLI 与 REST 共用，形状一致）。
// states/runtimeOK/reason 由调用方取一次运行态（CLI 与 REST 同一实现）。
func vxlanTunnelsViewOf(cfg model.Config, states map[string]network.VxlanState, runtimeOK bool, reason string) vxlanTunnelsView {
	view := vxlanTunnelsView{Tunnels: []vxlanTunnelView{}, RuntimeAvailable: runtimeOK, RuntimeReason: reason}
	for _, t := range cfg.VxlanTunnels {
		port := t.EffectiveDstPort()
		v := vxlanTunnelView{
			Name: t.Name, Vni: t.Vni, Local: t.Local, Remote: t.Remote, DstPort: port,
			VirtualSwitch: t.VirtualSwitch, RuntimeAvailable: runtimeOK,
		}
		if runtimeOK {
			if st, ok := states[network.VxlanTupleKey(uint32(t.Vni), t.Local, t.Remote, uint16(port))]; ok {
				v.InVPP, v.SwIfIndex, v.Instance = true, st.SwIfIndex, st.Instance
			}
		}
		view.Tunnels = append(view.Tunnels, v)
	}
	return view
}

// handleGetVxlanTunnels GET /api/v1/vxlan-tunnels：VXLAN 隧道读视图（配置 × 数据面实况）。
func (s *Server) handleGetVxlanTunnels(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	states, ok, reason := s.vxlanStates(r.Context())
	writeJSON(w, http.StatusOK, vxlanTunnelsViewOf(cfg, states, ok, reason))
}

// vxlanStates 服务端取一次运行态（REST 与 CLI 的 CLI 侧共用同一口径）。
func (s *Server) vxlanStates(ctx context.Context) (map[string]network.VxlanState, bool, string) {
	if s.vxlan == nil {
		return nil, false, "VXLAN 编排未接入（数据面未连接或编排器未装配）"
	}
	states, err := s.vxlan.VxlanStates(ctx)
	if err != nil {
		return nil, false, "VXLAN 运行态不可用：" + err.Error()
	}
	return states, true, ""
}

// vxlanStates 执行器侧取一次运行态（nil 注入 = 未接入；读数失败 = 如实带回原因）。
func (x *cliExecutor) vxlanStates() (map[string]network.VxlanState, bool, string) {
	if x.vxlan == nil {
		return nil, false, "VXLAN 编排未接入（数据面未连接或编排器未装配）"
	}
	states, err := x.vxlan.VxlanStates(context.Background())
	if err != nil {
		return nil, false, "VXLAN 运行态不可用：" + err.Error()
	}
	return states, true, ""
}

// execShowVxlan：`show vxlan tunnels`（操作树里只有 tunnels 一条）。
func (x *cliExecutor) execShowVxlan(args []string) string {
	if len(args) != 1 || args[0] != "tunnels" {
		return "%% 语法: show vxlan tunnels\n"
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(cfg.VxlanTunnels) == 0 {
		return "（无 VXLAN 隧道）\n"
	}
	states, runtimeOK, reason := x.vxlanStates()
	view := vxlanTunnelsViewOf(cfg, states, runtimeOK, reason)

	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-9s %-16s %-16s %-6s %-12s %s\n",
		"Name", "VNI", "Local", "Remote", "Port", "Switch", "State")
	items := make([]any, 0, len(view.Tunnels))
	for _, t := range view.Tunnels {
		vs := t.VirtualSwitch
		if vs == "" {
			vs = "-"
		}
		state := "未收敛（数据面中不存在）"
		switch {
		case !view.RuntimeAvailable:
			state = "运行态不可用（" + view.RuntimeReason + "）"
		case t.InVPP:
			state = fmt.Sprintf("已在 VPP（sw_if_index %d，instance %d）", t.SwIfIndex, t.Instance)
		}
		items = append(items, anyToTree(t))
		fmt.Fprintf(&b, "%-14s %-9d %-16s %-16s %-6d %-12s %s\n",
			t.Name, t.Vni, t.Local, t.Remote, t.DstPort, vs, state)
	}
	x.structured = map[string]any{"tunnels": items}
	return b.String()
}
