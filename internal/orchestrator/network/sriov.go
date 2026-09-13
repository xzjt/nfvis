package network

// M3-7（三）：SR-IOV VF 数量设置（契约 PUT /interfaces/{name}/sriov）。
//
// VF 创建/回收是内核 PF 驱动操作（写 sysfs sriov_numvfs），不属于 VPP binary API。
// 路径与写入函数可注入，便于单测；真机若 PF 不支持 SR-IOV（如 vmxnet3）会返回明确错误。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SRIOVProvider SR-IOV VF 数量编排。
type SRIOVProvider struct {
	// PathFor 返回 PF 的 sriov_numvfs 路径（缺省 /sys/class/net/<ifname>/device/sriov_numvfs）。
	PathFor func(ifname string) string
	// Write 写入实现（缺省 os.WriteFile）。
	Write func(path string, data []byte) error
}

// NewSRIOVProvider 构造缺省（sysfs）实现。
func NewSRIOVProvider() *SRIOVProvider {
	return &SRIOVProvider{
		PathFor: func(ifname string) string {
			return filepath.Join("/sys/class/net", ifname, "device", "sriov_numvfs")
		},
		Write: func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
	}
}

// SetVFCount 设置 VF 数量（0 = 回收全部）。
func (p *SRIOVProvider) SetVFCount(ctx context.Context, ifname string, count int) error {
	if ifname == "" {
		return fmt.Errorf("接口名不能为空")
	}
	if count < 0 {
		return fmt.Errorf("VF 数量不能为负: %d", count)
	}
	pathFor := p.PathFor
	if pathFor == nil {
		pathFor = NewSRIOVProvider().PathFor
	}
	write := p.Write
	if write == nil {
		write = NewSRIOVProvider().Write
	}
	path := pathFor(ifname)
	if err := write(path, []byte(strconv.Itoa(count))); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("接口 %s 不支持 SR-IOV（无 %s）", ifname, path)
		}
		return fmt.Errorf("设置 %s VF 数量 %d: %w", ifname, count, err)
	}
	return nil
}
