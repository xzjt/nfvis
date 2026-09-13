package compute

import (
	"context"
	"io"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// libvirtAPI provider 依赖的 libvirt 能力（真实实现 = Conn；单测用 mock）。
// 状态以 libvirt 原始枚举返回，exists=false 表示域未定义。
type libvirtAPI interface {
	Define(ctx context.Context, xml string) error
	Undefine(ctx context.Context, name string) error
	State(ctx context.Context, name string) (state int, exists bool, err error)
	Start(ctx context.Context, name string) error
	Shutdown(ctx context.Context, name string) error // ACPI 关机（优雅）
	Destroy(ctx context.Context, name string) error  // 立即断电（超时强杀）
	Reboot(ctx context.Context, name string) error
	// SetAutostart 设置 libvirt 域自启标志（FR-OPS-012：整机/libvirtd 重启后自启）。
	SetAutostart(ctx context.Context, name string, autostart bool) error
	// OpenConsole 打开域串口双向流（FR-CMP-014）。
	OpenConsole(ctx context.Context, name string) (io.ReadWriteCloser, error)
	// DumpDomainXML 返回域 XML（快照需据此取磁盘 target）。
	DumpDomainXML(ctx context.Context, name string) (string, error)
	// 快照（FR-CMP-015）：内部 qcow2 快照，含全部磁盘。
	SnapshotCreate(ctx context.Context, domain, snapshotXML string) error
	SnapshotList(ctx context.Context, domain string) ([]SnapshotInfo, error)
	SnapshotRevert(ctx context.Context, domain, snapshot string) error
	SnapshotDelete(ctx context.Context, domain, snapshot string) error
}

// storageAPI provider 依赖的磁盘/文件能力（真实实现 = qemuStorage；单测用 mock）。
type storageAPI interface {
	EnsureDir(path string) error
	Exists(path string) bool
	// Clone 从镜像克隆出 VM 主盘（qemu-img backing file，FR-CMP-010）。
	Clone(ctx context.Context, imagePath, diskPath string) error
	// CreateBlank 建空数据盘（FR-CMP-018）。
	CreateBlank(ctx context.Context, diskPath string, sizeGB int) error
	// RemoveAll 级联清理 VM 目录（盘/seed/日志）。
	RemoveAll(path string) error
}

// seedBuilder 生成 cloud-init NoCloud seed ISO（FR-CMP-016）。
type seedBuilder interface {
	Build(ctx context.Context, vm model.VMFunction, isoPath string) error
}

// Config 计算编排配置（路径与缺省值集中于此，真机以 M4-P0 记录为准）。
type Config struct {
	URI       string // libvirt URI，缺省 qemu:///system
	VMsDir    string // VM 落盘根目录
	VhostDir  string // vhost-user socket 目录（FR-NET-020）
	ImagesDir string // 镜像仓库目录（M4-8）
	Emulator  string // QEMU 可执行文件
	Machine   string // 机器类型
	CPUMode   string // CPU 模式

	// StopTimeout ACPI 关机等待上限，超时强杀（契约 stop 语义）。
	StopTimeout time.Duration
}

// DefaultConfig 生产缺省配置。
func DefaultConfig() Config {
	return Config{
		URI:         DefaultURI,
		VMsDir:      "/var/lib/nfvis/vms",
		VhostDir:    "/run/nfvis/vhost",
		ImagesDir:   "/var/lib/nfvis/images",
		StopTimeout: 30 * time.Second,
	}
}
