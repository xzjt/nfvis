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
	// StateReason 返回 libvirt 状态与 reason（外部 kill 致 SHUTOFF+CRASHED 的判定需要）。
	StateReason(ctx context.Context, name string) (state, reason int, exists bool, err error)
	Start(ctx context.Context, name string) error
	Shutdown(ctx context.Context, name string) error // ACPI 关机（优雅）
	Destroy(ctx context.Context, name string) error  // 立即断电（超时强杀）
	Reboot(ctx context.Context, name string) error
	// SetAutostart 设置 libvirt 域自启标志（FR-OPS-012：整机/libvirtd 重启后自启）。
	SetAutostart(ctx context.Context, name string, autostart bool) error
	// OpenConsole 打开域串口双向流（FR-CMP-014）。
	OpenConsole(ctx context.Context, name string) (io.ReadWriteCloser, error)
	// OpenVNC 打开域 VNC 图形控制台的 TCP 连接（决策 #485/Task 8；仅回环）。
	OpenVNC(ctx context.Context, name string) (io.ReadWriteCloser, error)
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
	// ReadTail 读取文件末尾若干行（决策 #311：libvirt 域日志摘录，供启动失败诊断）。
	// 文件不存在/不可读返回 error——调用方如实说「取不到」并给出路径，不编造。
	ReadTail(path string, maxLines int) (string, error)
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

	// StartProbeWindow 启动受理后回读域状态的探测窗口（决策 #311）。
	// <=0 取 DefaultStartProbeWindow；窗口只在**非运行态**路径上耗满——达到 running 即刻返回，
	// 故正常启动不因它多等。
	StartProbeWindow time.Duration
	// LibvirtLogDir 域日志目录（决策 #311：启动失败诊断读 <dir>/<name>.log）。
	// 空取 DefaultLibvirtLogDir；读不到只如实说「未取到」，不编造内容。
	LibvirtLogDir string

	// DataPlane 数据面实现（vpp|kernel，v3 决策 #404）：决定 vNIC 的落地形态——
	// vpp 走 vhost-user socket（QEMU 作 client 连 VPP），kernel 走 virtio 网卡 + 宿主 tap +
	// vhost-net（libvirt 按 `<interface type='bridge'>` 自建 tap 并挂内核 bridge）。
	// 缺省（空）= vpp，与引入该字段之前的行为逐字一致。
	DataPlane string

	// VMImageProfiles 镜像级启动画像（决策 #485）：定义 VM 时按**有效画像**
	// （实例 startup 覆盖 > 镜像画像 > 产品默认，见 model.EffectiveStartup）填 domain。
	// 由装配层从 committed 配置注入；改动画像需**重定义 VM**（start/restart/提交）才生效
	// ——与 FR-CMP-012 的「域定义随配置」一致，恢复收敛与 15s 巡检会按当次 committed 刷新。
	VMImageProfiles []model.VMImageProfile

	// ImageExists 镜像仓库存在性探针（决策 #485）：报告某镜像名是否在仓库中（装配层接
	// images.Store.Lookup）。恢复收敛/15s 巡检据此对账「配置有画像、仓库无镜像」的孤儿画像
	// （告警 VM_IMAGE_PROFILE_ORPHAN，镜像回来即消解）。nil = 不判（既不建也不消，保守不猜测）。
	ImageExists func(name string) bool

	// DataPlaneProbe 数据面（VPP）可用性前置判定（决策 #314）：返回 nil = 可用；返回 error =
	// 不可用（error 文本作为原因透出）。由装配层注入、复用**既有**的 VPP 连接状态查询
	// （cmd/nfvisd 用 network.Manager.StatusView，与 /vpp/status 同源）——本包不另写探测，
	// 判定单一事实源。启动（start 及 restart 的 off→start 分支）在进入 api.Start 之前调用：
	// 命中即立即失败，不进入会阻塞的 vhost-user/socket 准备阶段，也不产生 paused 残域。
	// **nil = 不做判定**——正常路径与既有语义逐字/逐秒不变（单测与无 VPP 的环境即此情形）。
	DataPlaneProbe func() error
}

// 启动结果回读的缺省参数（决策 #311）。
const (
	// DefaultStartProbeWindow 缺省探测窗口：宽到能覆盖 vhost-user 握手/后端就绪的常见时延，
	// 又不至于让「必然失败」的启动等太久。
	DefaultStartProbeWindow = 3 * time.Second
	// DefaultLibvirtLogDir libvirt 域日志目录（Ubuntu/libvirt 缺省；`virsh` 亦写此处）。
	DefaultLibvirtLogDir = "/var/log/libvirt/qemu"
)

// DefaultConfig 生产缺省配置。
func DefaultConfig() Config {
	return Config{
		URI:              DefaultURI,
		VMsDir:           "/var/lib/nfvis/vms",
		VhostDir:         "/run/nfvis/vhost",
		ImagesDir:        "/var/lib/nfvis/images",
		StopTimeout:      30 * time.Second,
		StartProbeWindow: DefaultStartProbeWindow,
		LibvirtLogDir:    DefaultLibvirtLogDir,
	}
}
