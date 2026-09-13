package network

// vhost-user vNIC 接入（FR-NET-020/023）。
//
// VPP 侧为每个 vNIC 建 **server** socket（QEMU 侧为 client，见 M4-1 domain XML 的
// `<source type='unix' mode='client'>`），接口改名以便交换机端口按名引用
// （orchestrator.VnfIfaceName），并打 tag 供跨进程反查。链路状态由客户端连接驱动：
// VM 未启动/QEMU 未连 socket 时 link down，VM 关机自动 down（FR-NET-023，实测）。

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// VhostUserClient VPP vhost-user 操作能力（govpp 实现见 vhost_user_govpp.go）。
type VhostUserClient interface {
	// CreateVhostUser 建 vhost-user 接口（isServer=true：VPP 监听 socket）。
	CreateVhostUser(sockFilename string, isServer bool, tag string) (uint32, error)
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	// VhostUserSocket 返回已存在 vhost-user 接口绑定的 socket 路径（exists=false = 非 vhost-user 接口）。
	VhostUserSocket(swIfIndex uint32) (string, bool, error)
	SetInterfaceName(swIfIndex uint32, name string) error
	SetInterfaceMAC(swIfIndex uint32, mac string) error
	SetState(swIfIndex uint32, up bool) error
	// InterfaceStatus 返回 (adminUp, linkUp)；接口不存在时 exists=false。
	InterfaceStatus(swIfIndex uint32) (adminUp, linkUp, exists bool, err error)
	DeleteVhostUser(swIfIndex uint32) error
	Close()
}

// VhostUserProvider vhost-user 接口编排。
type VhostUserProvider struct {
	client func() (VhostUserClient, error)
}

// NewVhostUserProviderFunc 以「随连接获取客户端」的工厂构造（与 L2/L3 同约定）。
func NewVhostUserProviderFunc(f func() (VhostUserClient, error)) *VhostUserProvider {
	return &VhostUserProvider{client: f}
}

// ErrVhostUserUnavailable 未接入 VPP 客户端。
var ErrVhostUserUnavailable = errors.New("VPP 未连接，vhost-user 客户端不可用")

// ensureSocketAccessible 放宽 VPP 创建的 vhost-user socket 权限。
//
// VPP 以自身 umask 创建 socket（实测 0755，属主 root），而 QEMU 以 libvirt-qemu 运行、
// connect 该 unix socket 需写权限，否则启 VM 报 `Failed to connect ... Permission denied`。
// 统一放宽为 0777（socket 仅本机 /run/nfvis/vhost 下可见）；文件不存在（非本地/尚未创建）时跳过。
func ensureSocketAccessible(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return os.Chmod(path, 0o777)
}

// Apply 声明式建立 vNIC 接入（幂等）：接口已存在则校正属性，不存在则创建。
func (p *VhostUserProvider) Apply(ctx context.Context, port orchestrator.VnfPort) error {
	if port.Type != "vhost-user" {
		return fmt.Errorf("vNIC %s/%s 类型 %q 非 vhost-user，不应走 vhost-user 编排", port.VM, port.Interface, port.Type)
	}
	if port.Socket == "" {
		return fmt.Errorf("vNIC %s/%s 缺少 socket 路径", port.VM, port.Interface)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	name := orchestrator.VnfIfaceName(port.VM, port.Interface)
	idx, ok, err := c.SwInterfaceIndex(name)
	if err != nil {
		return fmt.Errorf("查询 vhost-user 接口 %s: %w", name, err)
	}
	// 已存在但 socket 不匹配（陈旧残留/迁移遗留）或 socket 文件已丢失 → 删除重建，
	// 避免「按名判存在」的假幂等把 VM 指向不可用/错误的 socket。
	if ok {
		bound, isVhost, err := c.VhostUserSocket(idx)
		if err != nil {
			return fmt.Errorf("查询 vhost-user 接口 %s 的 socket: %w", name, err)
		}
		stale := !isVhost || bound != port.Socket
		if stale {
			if err := c.DeleteVhostUser(idx); err != nil {
				return fmt.Errorf("清理陈旧 vhost-user 接口 %s: %w", name, err)
			}
			ok = false
		}
	}
	if !ok {
		idx, err = c.CreateVhostUser(port.Socket, true, orchestrator.VnfPortTag(port.VM, port.Interface))
		if err != nil {
			return fmt.Errorf("创建 vhost-user 接口 %s（socket %s）: %w", name, port.Socket, err)
		}
		if err := c.SetInterfaceName(idx, name); err != nil {
			return fmt.Errorf("重命名 vhost-user 接口 %d → %s: %w", idx, name, err)
		}
		if err := ensureSocketAccessible(port.Socket); err != nil {
			return fmt.Errorf("放宽 vhost-user socket %s 权限: %w", port.Socket, err)
		}
	}
	if port.MAC != "" {
		if err := c.SetInterfaceMAC(idx, port.MAC); err != nil {
			return fmt.Errorf("设置 vhost-user 接口 %s MAC %s: %w", name, port.MAC, err)
		}
	}
	if err := c.SetState(idx, true); err != nil {
		return fmt.Errorf("置 vhost-user 接口 %s up: %w", name, err)
	}
	return nil
}

// Delete 删除 vNIC 接入（接口不存在时幂等返回 nil；BD 成员随接口删除一并消失）。
func (p *VhostUserProvider) Delete(ctx context.Context, vmName, ifaceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	name := orchestrator.VnfIfaceName(vmName, ifaceName)
	idx, ok, err := c.SwInterfaceIndex(name)
	if err != nil {
		return fmt.Errorf("查询 vhost-user 接口 %s: %w", name, err)
	}
	if !ok {
		return nil
	}
	if err := c.DeleteVhostUser(idx); err != nil {
		return fmt.Errorf("删除 vhost-user 接口 %s: %w", name, err)
	}
	return nil
}

// LinkState 查询 vNIC 接口链路状态（FR-NET-023：VM 关机断连 → link down）。
// exists=false 表示接口不存在；up = admin up 且 link up。
func (p *VhostUserProvider) LinkState(ctx context.Context, vmName, ifaceName string) (exists, up bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	c, err := p.client()
	if err != nil {
		return false, false, err
	}
	defer c.Close()

	idx, ok, err := c.SwInterfaceIndex(orchestrator.VnfIfaceName(vmName, ifaceName))
	if err != nil || !ok {
		return false, false, err
	}
	adminUp, linkUp, exists, err := c.InterfaceStatus(idx)
	if err != nil {
		return false, false, err
	}
	return exists, exists && adminUp && linkUp, nil
}
