package compute

import (
	"path"
	"strings"
)

// Layout 一台 VM 的落盘布局。路径由 (vmsDir, name) 确定性推导，无持久化状态，
// 恢复收敛可重算，避免「配置 + 磁盘清单」双源漂移（同 BD ID 派生思路，决策 #31）。
type Layout struct {
	Dir       string // VM 专属目录
	DiskPath  string // 主盘 qcow2（从镜像克隆）
	SeedISO   string // cloud-init NoCloud seed ISO
	SerialLog string // 串口日志（预留）
}

// NewLayout 生成 VM 落盘布局。
func NewLayout(vmsDir, name string) Layout {
	dir := path.Join(vmsDir, name)
	return Layout{
		Dir:       dir,
		DiskPath:  path.Join(dir, "disk.qcow2"),
		SeedISO:   path.Join(dir, "seed.iso"),
		SerialLog: path.Join(dir, "console.log"),
	}
}

// DataDiskPath 附加数据盘路径（FR-CMP-018）。
func DataDiskPath(vmsDir, vmName, diskName string) string {
	return path.Join(vmsDir, vmName, "data-"+diskName+".qcow2")
}

// VhostSocketPath VM vNIC 对应的 VPP 侧 vhost-user socket 路径（FR-NET-020）。
// 命名确定：<vhostDir>/<vm>-<vnic>.sock，VM 与 vNIC 名均已通过配置校验（≤64 字符、无斜杠）。
func VhostSocketPath(vhostDir, vmName, ifaceName string) string {
	return path.Join(vhostDir, vmName+"-"+ifaceName+".sock")
}

// ImageIsISO 判断镜像是否光盘镜像（ISO 直接作为只读 cdrom 引导，不克隆）。
func ImageIsISO(image string) bool {
	return strings.EqualFold(path.Ext(image), ".iso")
}
