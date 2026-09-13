package network

// vNIC 断连检测与告警（FR-NET-023）。
//
// vhost-user 接口的 link 状态由 QEMU 客户端连接驱动：VM 未启动/关机即断连 → 接口
// link down。此处对比「配置中应有 vNIC」与「VPP 实际接口链路状态」，down/缺失记
// warning 告警；恢复后自动 resolve。进程内告警表（M3-8）；M5 事件总线就绪后改为
// 事件驱动实时告警（本检查由恢复收敛与 VM 生命周期动作后触发）。

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// vnfScope 告警作用域（与 recoveryScope 区分：Sync 各自收敛互不影响）。
const vnfScope = "vnf-port"

// 告警码：vhost-user 端口 down（VM 关机/未连接）。
const AlarmVnfPortDown = "VNF_PORT_DOWN"

// CheckVnfPorts 检查配置中所有 vhost-user vNIC 的链路状态并维护告警。
// 返回不可查询项的错误（不阻塞调用方）。
func (n *L2Network) CheckVnfPorts(ctx context.Context, cfg model.Config) []error {
	if n.vhost == nil {
		return nil
	}
	var errs []error
	for _, vm := range cfg.VirtualMachineFunctions {
		for _, nic := range vm.Interfaces {
			if nic.Type != "vhost-user" {
				continue
			}
			source := vm.Name + "/" + nic.Name
			exists, up, err := n.vhost.LinkState(ctx, vm.Name, nic.Name)
			if err != nil {
				errs = append(errs, fmt.Errorf("vNIC %s 链路状态查询: %w", source, err))
				continue
			}
			if n.alarms == nil {
				continue
			}
			if exists && up {
				n.alarms.Resolve(vnfScope, AlarmVnfPortDown, source)
				continue
			}
			reason := "接口 link down（VM 未启动或已关机，客户端未连接）"
			if !exists {
				reason = "VPP 中不存在对应 vhost-user 接口"
			}
			n.alarms.Raise(vnfScope, SeverityWarning, AlarmVnfPortDown,
				fmt.Sprintf("VNF %s 的 vNIC %s %s（FR-NET-023）", vm.Name, nic.Name, reason), source)
		}
	}
	return errs
}

// VnfPortReport 供运行态展示：vNIC → (接口是否存在, link 是否 up)。
type VnfPortReport struct {
	VM        string `json:"vm"`
	Interface string `json:"interface"`
	Exists    bool   `json:"exists"`
	Up        bool   `json:"up"`
}

// VnfPorts 返回配置中全部 vhost-user vNIC 的链路状态（FR-NET-023 运行态）。
func (n *L2Network) VnfPorts(ctx context.Context, cfg model.Config) ([]VnfPortReport, error) {
	if n.vhost == nil {
		return nil, nil
	}
	out := make([]VnfPortReport, 0, len(cfg.VirtualMachineFunctions))
	for _, vm := range cfg.VirtualMachineFunctions {
		for _, nic := range vm.Interfaces {
			if nic.Type != "vhost-user" {
				continue
			}
			exists, up, err := n.vhost.LinkState(ctx, vm.Name, nic.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, VnfPortReport{VM: vm.Name, Interface: nic.Name, Exists: exists, Up: up})
		}
	}
	return out, nil
}

// VnfPortTagOf 供外部按 tag 反查（恢复收敛用）。
func VnfPortTagOf(vmName, ifaceName string) string { return orchestrator.VnfPortTag(vmName, ifaceName) }
