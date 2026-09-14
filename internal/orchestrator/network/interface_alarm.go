package network

// 物理业务口链路状态告警（FR-NET-003）。
//
// 此前只有 vhost-user 的 VNF_PORT_DOWN（FR-NET-023），**物理口 link down/up 无任何告警**
// ——规格明确要求「业务网卡支持启用/禁用、MTU、描述配置；**链路状态变化产生告警事件**」。
// 本文件对比「配置中启用（enabled）的物理口」与「VPP 实际 admin/link 状态」并维护告警，
// 复用 AlarmStore（与 /alarms、/events 同源）。

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
)

// ifLinkScope 告警作用域（与 vnf-port / recovery 各自 Sync 互不影响）。
const ifLinkScope = "interface-link"

// AlarmIfaceLinkDown 物理业务口未 up（链路 down 或未启用）。
const AlarmIfaceLinkDown = "INTERFACE_LINK_DOWN"

// CheckInterfaceLinks 检查配置中「启用」的物理口链路状态并维护告警。
//
// 语义（与 VNF_PORT_DOWN 同口径：admin 与 link 双标志）：
//   - 仅检查 `interfaces[]` 中**启用**（Enabled 未显式置 false）的口；
//   - VPP 中不存在该口 → 交由恢复收敛处理（RECOVERY_IFACE_MISSING），此处不重复告警；
//   - 存在但 `!(AdminUp && LinkUp)` → warning 告警，消息区分 admin/link 哪一侧未起；
//   - 恢复 up 后自动消警。
func (n *L2Network) CheckInterfaceLinks(ctx context.Context, cfg model.Config) []error {
	if n.l2 == nil {
		return nil
	}
	c, err := n.l2.client()
	if err != nil {
		return []error{err}
	}
	defer c.Close()
	names, err := c.SwInterfaceNames()
	if err != nil {
		return []error{fmt.Errorf("查询接口状态: %w", err)}
	}
	if n.alarms == nil {
		return nil
	}
	for _, iface := range cfg.Interfaces {
		if iface.Enabled != nil && !*iface.Enabled {
			continue // 显式禁用：不下发也不告警（用户意图）
		}
		info, exists := infoByName(names, iface.Name)
		if !exists {
			continue // 缺口由恢复收敛告警（RECOVERY_IFACE_MISSING）
		}
		if info.AdminUp && info.LinkUp {
			n.alarms.Resolve(ifLinkScope, AlarmIfaceLinkDown, iface.Name)
			continue
		}
		reason := "链路 down（对端/网线/交换机端口）"
		switch {
		case !info.AdminUp && !info.LinkUp:
			reason = "管理态未启用且链路 down"
		case !info.AdminUp:
			reason = "管理态未启用（set interfaces " + iface.Name + " disable 或下发未生效）"
		}
		n.alarms.Raise(ifLinkScope, SeverityWarning, AlarmIfaceLinkDown,
			fmt.Sprintf("物理口 %s 未就绪：%s（FR-NET-003）", iface.Name, reason), iface.Name)
	}
	return nil
}

// infoByName 按名反查接口信息（VPP 侧子接口名带 VLAN 后缀，故按精确名匹配）。
func infoByName(names map[uint32]SwIfInfo, ifname string) (SwIfInfo, bool) {
	for _, info := range names {
		if info.Name == ifname {
			return info, true
		}
	}
	return SwIfInfo{}, false
}
