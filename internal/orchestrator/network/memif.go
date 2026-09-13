package network

// 容器 memif vNIC 接入（FR-NET-022）。
//
// VPP 为每个容器 vNIC 建 memif endpoint：先注册 socket 文件名（socket-id → 路径），
// 再以 MASTER 角色建 memif 接口（VPP 创建 socket 文件，容器侧为 slave 客户端），
// 接口改名 `mf-<ct>-<vnic>` 以便交换机端口按名挂接，socket 权限放宽供容器访问。

import (
	"context"
	"fmt"
	"os"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// MemifClient VPP memif 操作能力（govpp 实现见 memif_govpp.go）。
type MemifClient interface {
	AddSocketFilename(socketID uint32, path string, add bool) error
	CreateMemif(id, socketID uint32, mac string) (uint32, error)
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	SetInterfaceName(swIfIndex uint32, name string) error
	SetInterfaceMAC(swIfIndex uint32, mac string) error
	SetState(swIfIndex uint32, up bool) error
	DeleteMemif(swIfIndex uint32) error
	Close()
}

// MemifProvider 容器 memif 接口编排。
type MemifProvider struct {
	client func() (MemifClient, error)
}

// NewMemifProviderFunc 以客户端工厂构造。
func NewMemifProviderFunc(f func() (MemifClient, error)) *MemifProvider {
	return &MemifProvider{client: f}
}

// Apply 声明式建立容器 vNIC 的 memif 接入（幂等）。
func (p *MemifProvider) Apply(ctx context.Context, port orchestrator.VnfPort) error {
	if port.Type != "memif" {
		return fmt.Errorf("vNIC %s/%s 类型 %q 非 memif", port.VM, port.Interface, port.Type)
	}
	if port.Socket == "" {
		return fmt.Errorf("容器 vNIC %s/%s 缺少 memif socket 路径", port.VM, port.Interface)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	name := orchestrator.MemifIfaceName(port.VM, port.Interface)
	idx, ok, err := c.SwInterfaceIndex(name)
	if err != nil {
		return fmt.Errorf("查询 memif 接口 %s: %w", name, err)
	}
	if !ok {
		socketID := orchestrator.MemifSocketID(port.VM, port.Interface)
		if err := c.AddSocketFilename(socketID, port.Socket, true); err != nil {
			return fmt.Errorf("注册 memif socket %s（id=%d）: %w", port.Socket, socketID, err)
		}
		idx, err = c.CreateMemif(orchestrator.MemifID(port.VM, port.Interface), socketID, port.MAC)
		if err != nil {
			return fmt.Errorf("创建 memif 接口 %s: %w", name, err)
		}
		if err := c.SetInterfaceName(idx, name); err != nil {
			return fmt.Errorf("重命名 memif 接口 %d → %s: %w", idx, name, err)
		}
		if err := ensureSocketAccessible(port.Socket); err != nil {
			return fmt.Errorf("放宽 memif socket %s 权限: %w", port.Socket, err)
		}
	}
	if port.MAC != "" {
		if err := c.SetInterfaceMAC(idx, port.MAC); err != nil {
			return fmt.Errorf("设置 memif 接口 %s MAC %s: %w", name, port.MAC, err)
		}
	}
	if err := c.SetState(idx, true); err != nil {
		return fmt.Errorf("置 memif 接口 %s up: %w", name, err)
	}
	return nil
}

// Delete 删除容器 vNIC 的 memif 接入（不存在时幂等）。
func (p *MemifProvider) Delete(ctx context.Context, owner, ifaceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	idx, ok, err := c.SwInterfaceIndex(orchestrator.MemifIfaceName(owner, ifaceName))
	if err != nil || !ok {
		return err
	}
	if err := c.DeleteMemif(idx); err != nil {
		return fmt.Errorf("删除 memif 接口 %s: %w", orchestrator.MemifIfaceName(owner, ifaceName), err)
	}
	_ = c.AddSocketFilename(orchestrator.MemifSocketID(owner, ifaceName), "", false)
	_ = os.Remove(orchestrator.MemifSocketPath(orchestrator.DefaultMemifDir, owner, ifaceName))
	return nil
}
