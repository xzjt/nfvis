package compute

// 通用 PCI 直通设备的存在性检查（FR-CMP-023）。
//
// 配置层（internal/model）只做 BDF 语法与冲突校验——「设备是否真在本机」是运行环境事实，
// 只能在计算编排层查，sysfs 是单一事实源：/sys/bus/pci/devices/<归一BDF>。
//
// 与 SR-IOV 的 VF 解析（network.NewSysfsVFResolver）同法：真实实现读绝对路径的 sysfs，
// Provider 经 SetPCIDeviceChecker 注入；单测注入假实现（不在非 Linux/无 sysfs 环境落真路径）。
//
// **不做** vfio 驱动的绑定/解绑：预绑定由操作者/安装器负责（用户手册 §9.2 写明），
// 产品只拒绝「不存在/未归内核或 vfio 管理」的设备，不代替操作者改驱动。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PCIDevicesDir 内核 PCI 设备目录（sysfs）。检查器与错误文案共用同一常量。
const PCIDevicesDir = "/sys/bus/pci/devices"

// NewSysfsPCIDeviceChecker 返回 PCI 设备存在性检查器。bdf 须已归一（model.NormalizeBDF）。
//   - 设备在 sysfs 中出现 ⇒ (true, nil)；
//   - sysfs 可读但设备不在 ⇒ (false, nil)；
//   - sysfs 本身不可读（非 Linux/容器内未挂载）⇒ (false, error)——调用方如实上报，
//     不把「查不了」当成「设备不存在」（两者原因不同）。
func NewSysfsPCIDeviceChecker() func(bdf string) (bool, error) {
	return func(bdf string) (bool, error) {
		if _, err := os.Stat(PCIDevicesDir); err != nil {
			return false, fmt.Errorf("PCI 设备目录 %s 不可读: %w", PCIDevicesDir, err)
		}
		if _, err := os.Stat(filepath.Join(PCIDevicesDir, bdf)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("读取 %s: %w", filepath.Join(PCIDevicesDir, bdf), err)
		}
		return true, nil
	}
}
