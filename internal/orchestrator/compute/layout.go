package compute

import (
	"errors"
	"fmt"
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
// 注意：本函数**只拼路径、不复核**——凡要落盘或写进 domain XML 的地方都用 DataDiskPathChecked
// （round51 G1：盘名是用户输入，`..` 能把 root 的 CreateBlank 引到 VM 目录之外）。
func DataDiskPath(vmsDir, vmName, diskName string) string {
	return path.Join(vmsDir, vmName, "data-"+diskName+".qcow2")
}

// ---------- 派生落盘路径的越界复核（round51 G1） ----------

// ErrDerivedPathEscape 由配置字段拼出的落盘路径越出基准目录。取独立错误值（同
// images.ErrInvalidName 的做法）：调用方/守护据此断言「拒绝的原因正是路径越界」，不靠匹配文案。
var ErrDerivedPathEscape = errors.New("派生的落盘路径越界")

// DerivedPathError 一次越界：点名**哪台 VM、哪一项（哪个盘/哪个镜像）、越界到哪**。
//
// 为什么 compute 也要自己判一遍（纵深防御，model 层的格式校验是第一层、这里是第二层）：
// 盘名与镜像名都是**用户输入**，中间只隔一层格式校验，而真正把名字拼成路径、并以 **root**
// 去 EnsureDir / qemu-img create / 克隆的是本包——`disks[].name` 写成 `../../../etc/cron.d/x`、
// `disks[].image` 写成 `../vms/victim/disk`，都能让这些动作落到 VM 目录/镜像仓库之外。故凡
// 「由配置字段拼出的落盘路径」在使用前一律复核前缀，且复核点必须在**动作之前**（不只校验一处）。
type DerivedPathError struct {
	VM   string // 所属 VM
	Item string // 被解析的项（如「数据盘 data0」「数据盘 data0 引用的镜像 x」）
	Path string // 拼出的、越界的最终路径
	Base string // 它必须落在其内的基准目录
}

func (e *DerivedPathError) Error() string {
	return fmt.Sprintf("VM %s：%s 解析出的落盘路径 %q 越界（必须落在 %s 之内）——名字里的 `..` 会引出上级目录，已拒绝",
		e.VM, e.Item, e.Path, e.Base)
}

func (e *DerivedPathError) Unwrap() error { return ErrDerivedPathEscape }

// containedUnder 判定 target（Clean 后）是否**严格**落在 base 目录之内（与镜像仓库
// images.containedPath 同一口径：前缀必须带分隔符，base 自身不算「之内」）。
func containedUnder(base, target string) bool {
	b, t := path.Clean(base), path.Clean(target)
	return t != b && strings.HasPrefix(t, b+"/")
}

// DataDiskPathChecked DataDiskPath 的越界复核版本：数据盘必须落在 <vmsDir>/<vmName> 之内。
// specFor（算路径）与 prepare（真落盘）都用它——两道都要有，见 DerivedPathError 注释。
func DataDiskPathChecked(vmsDir, vmName, diskName string) (string, error) {
	p := DataDiskPath(vmsDir, vmName, diskName)
	if err := assertUnderVMDir(vmsDir, vmName, p, "数据盘 "+diskName); err != nil {
		return "", err
	}
	return p, nil
}

// assertUnderVMDir 复核**已算好的**数据盘路径仍落在 <vmsDir>/<vmName> 之内（item 供报错点名）。
func assertUnderVMDir(vmsDir, vmName, diskPath, item string) error {
	base := path.Join(vmsDir, vmName)
	if !containedUnder(base, diskPath) {
		return &DerivedPathError{VM: vmName, Item: item, Path: diskPath, Base: base}
	}
	return nil
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
