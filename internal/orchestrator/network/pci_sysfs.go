package network

// PCI 解析：startup.conf 的 dpdk dev 以 PCI 地址为键，而 committed 配置按物理口名
// 指定覆盖项（附录 A #30）。运行态经 sysfs 解析 ifname→PCI，配置保持可移植。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NewSysfsPCIResolver 返回从 sysfs 读取 PCI 地址的解析器（Linux）。
func NewSysfsPCIResolver() PCIResolver {
	return func(ifname string) (string, error) {
		p := filepath.Join("/sys/class/net", ifname, "device", "uevent")
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("读取 %s: %w", p, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PCI_SLOT_NAME="); ok && v != "" {
				return v, nil
			}
		}
		return "", fmt.Errorf("接口 %s 无 PCI_SLOT_NAME（非 PCI 设备或未绑定 DPDK 候选）", ifname)
	}
}

// NewSysfsVFResolver 返回 SR-IOV VF 的 PCI 地址解析器（FR-NET-021）：
// 读 `/sys/class/net/<pf>/device/virtfn<N>` 符号链接的目标名（即 VF 的 PCI 地址）。
// PF 不支持 SR-IOV（如 vmxnet3）时 virtfn 不存在，返回明确错误。
func NewSysfsVFResolver() func(pf string, vfID int) (string, error) {
	return func(pf string, vfID int) (string, error) {
		if vfID < 0 {
			return "", fmt.Errorf("VF 编号 %d 非法", vfID)
		}
		link := filepath.Join("/sys/class/net", pf, "device", fmt.Sprintf("virtfn%d", vfID))
		target, err := os.Readlink(link)
		if err != nil {
			return "", fmt.Errorf("物理口 %s 无 VF %d（%s：%w；是否不支持 SR-IOV？）", pf, vfID, link, err)
		}
		addr := filepath.Base(target)
		if addr == "" || addr == "." || addr == string(filepath.Separator) {
			return "", fmt.Errorf("VF %s 的 PCI 地址解析失败（link=%s）", link, target)
		}
		return addr, nil
	}
}
