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
