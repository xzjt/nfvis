package compute

// PCI 设备清单（BUG-24）：PCI 设备 / IOMMU 组 / 当前驱动 / 直通可用性的事实源。
//
// 由来：产品此前只有 `set virtual-machine-functions <name> pci-device <bdf>`，**没有任何命令
// 能列出**可选 BDF、它们的 IOMMU 组或当前驱动——运维必须 ssh 进 shell 跑 `lspci` 才知道该
// 填什么，且无法判断某个 BDF 是否与无关设备同组（真机现场：`0000:0b:00.0` 落在 group 7，
// 同组还有 8 个 PCIe root port）。本文件提供**只读**的事实源，供 `show system pci` 渲染。
//
// 事实源口径（与 internal/orchestrator/network 的 `KernelIfFactsAll` 同法）：
//   - 枚举 `/sys/bus/pci/devices/*`（目录名即 BDF）；
//   - `vendor`/`device`/`class`/`numa_node`/`enable` 读同名文件（去掉 `0x` 前缀）；
//   - `driver` 取 `driver` 符号链接的 basename（未绑驱动则没有该链接 ⇒ 空）；
//   - IOMMU 组**反查** `/sys/kernel/iommu_groups/<n>/devices/*`：先建 BDF→组 映射，
//     再逐设备填（不用「每个设备反推组号」，组目录才是内核的权威清单）。
//
// **如实**（与 #329/#353 的读视图同一纪律）：
//   - PCI 目录不可读 ⇒ 返回 error（调用方渲染「取不到 + 原因」），**不谎报空清单**；
//   - IOMMU 组目录不存在（内核未启用 IOMMU / 无该目录）⇒ 不算读取失败，按「无 IOMMU 组」呈现
//     （这正是「不可直通」的判定依据）；其它读取失败 ⇒ 记原因，调用方如实标注；
//   - 单设备某字段读不到 ⇒ 零值/`-1`（调用方渲染 `-`），不编造。
//
// **不做** vendor/device 的**人类可读名称**（`15ad:07b0` → "VMware VMXNET3"）：那张表是
// pci.ids，产品不打包、也不联网取——读视图因此只显示 ID 形态，并在口径行里明说。
// 也**不做** vfio 绑定/解绑（与 pci_devices.go 的既有边界一致）。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// IOMMUGroupsDir 内核 IOMMU 组目录（sysfs）。检查器与文案共用同一常量。
const IOMMUGroupsDir = "/sys/kernel/iommu_groups"

// sysfsPCIRoot / sysfsIOMMURoot 读视图的落点（测试可改，照仓库 `var sysfsNetRoot` 范式）。
var (
	sysfsPCIRoot   = PCIDevicesDir
	sysfsIOMMURoot = IOMMUGroupsDir
)

// VFIOPCIDriver 直通设备在宿主的典型绑定驱动（绑定即「已交给 vfio」，可被 libvirt 直通）。
const VFIOPCIDriver = "vfio-pci"

// 直通可用性的判定类别（**事实分类**，用户可见文案在 API 读视图层渲染）。
const (
	// PassthroughBound 已绑定 vfio-pci：设备已交给直通驱动。
	PassthroughBound = "bound"
	// PassthroughOK 有 IOMMU 组、组内只有端点设备且无桥。
	PassthroughOK = "ok"
	// PassthroughGroupHasBridge 有 IOMMU 组，但组内含 PCI 桥/root port ⇒ 直通需整组隔离。
	PassthroughGroupHasBridge = "group-has-bridge"
	// PassthroughNoGroup 无 IOMMU 组 ⇒ 不能安全直通。
	PassthroughNoGroup = "no-iommu-group"
	// PassthroughUnknown 有 IOMMU 组，但组内设备的 class 有读不到的 ⇒ 桥判定不完整，不猜「安全」。
	PassthroughUnknown = "unknown"
)

// PCIDeviceFacts 一台 PCI 设备的事实（sysfs 单一事实源；读不到的字段如实标为未知）。
type PCIDeviceFacts struct {
	BDF    string // 规范 BDF（sysfs 目录名，如 0000:0b:00.0）
	Vendor string // 4 位十六进制（如 15ad）；取不到为空
	Device string // 4 位十六进制（如 07b0）；取不到为空
	Class  string // 6 位十六进制（如 020000）；取不到为空
	Driver string // driver 符号链接的 basename（如 vmxnet3 / vfio-pci）；未绑驱动为空
	Label  string // `label` 文件内容（少数平台有，如 "SLOT 3"）；无为空

	// IOMMU 组：-1 = 无组/取不到（两者在 sysfs 里都表现为「不在任何组的 devices 清单里」）。
	IOMMUGroup int
	// GroupDevices 同组设备 BDF（含自身，已排序）；无组为空。
	GroupDevices []string
	// GroupSize 组内设备数（含桥/root port——它们同样占组，直通须整组隔离）；无组为 0。
	GroupSize int
	// GroupHasBridge 组内任一设备 class 前两位为 06（PCI 桥 / root port）。
	GroupHasBridge bool
	// GroupClassesKnown 组内全部设备的 class 都读到了；false ⇒ 桥判定不完整（不据此断言「安全」）。
	GroupClassesKnown bool

	NUMA    int // `numa_node`（取不到为 -1）
	Enabled int // `enable` 文件（1 = 已启用；取不到为 -1）
}

// PassthroughKind 直通可用性的**事实分类**（见 Passthrough* 常量）。
//
// 判据只看两件 sysfs 事实（组的存否、组内是否含桥/root port）与当前驱动；
// **最终能否直通以 libvirt 报错为准**——ACS 隔离、group 内 hidden device 等细节不在这里断言。
func (f PCIDeviceFacts) PassthroughKind() string {
	if f.Driver == VFIOPCIDriver {
		return PassthroughBound
	}
	if f.IOMMUGroup < 0 {
		return PassthroughNoGroup
	}
	if !f.GroupClassesKnown {
		return PassthroughUnknown
	}
	if f.GroupHasBridge {
		return PassthroughGroupHasBridge
	}
	return PassthroughOK
}

// PCIInventory PCI 设备清单（按 BDF 升序）。
type PCIInventory struct {
	Devices []PCIDeviceFacts
	// IOMMUAvailable IOMMU 组信息是否可读（目录读到即为 true，哪怕是空的——空目录＝内核未分组）。
	IOMMUAvailable bool
	// IOMMUError IOMMU 组目录**读取失败**的原因（IOMMUAvailable=false 时非空，调用方如实标注）。
	IOMMUError string
}

// ReadPCIInventory 读本机 PCI 设备清单（sysfs 单一事实源）。PCI 目录不可读时返回 error。
func ReadPCIInventory() (PCIInventory, error) { return readPCIInventory(sysfsPCIRoot, sysfsIOMMURoot) }

// readPCIInventory 按给定落点读取（单测注入临时目录；与生产走同一份实现）。
func readPCIInventory(pciRoot, iommuRoot string) (PCIInventory, error) {
	entries, err := os.ReadDir(pciRoot)
	if err != nil {
		return PCIInventory{}, fmt.Errorf("PCI 设备目录 %s 不可读: %w", pciRoot, err)
	}

	inv := PCIInventory{Devices: make([]PCIDeviceFacts, 0, len(entries))}
	// IOMMU 组：BDF → 组号 + 组号 → 组内 BDF 清单（**组目录是权威清单**，不以设备侧反推为准）。
	byBDF := map[string]int{}
	groupDevices := map[int][]string{}
	if g, gerr := readIOMMUGroups(iommuRoot, byBDF, groupDevices); gerr != nil {
		inv.IOMMUError = gerr.Error()
	} else {
		inv.IOMMUAvailable = g
	}
	// 组内设备 class 的桥判定（先读一遍 class，避免对每个设备重复读同组伙伴）。
	classes := map[string]string{}
	classOf := func(bdf string) (string, bool) {
		if c, ok := classes[bdf]; ok {
			return c, c != ""
		}
		c := readHexField(filepath.Join(pciRoot, bdf, "class"), 6)
		classes[bdf] = c
		return c, c != ""
	}

	for _, e := range entries {
		bdf := e.Name()
		if !e.IsDir() {
			continue // sysfs 里都是目录；非常规条目如实跳过（不是设备）
		}
		f := PCIDeviceFacts{
			BDF:        bdf,
			Vendor:     readHexField(filepath.Join(pciRoot, bdf, "vendor"), 4),
			Device:     readHexField(filepath.Join(pciRoot, bdf, "device"), 4),
			Class:      readHexField(filepath.Join(pciRoot, bdf, "class"), 6),
			Driver:     readLinkBase(filepath.Join(pciRoot, bdf, "driver")),
			Label:      readTrimmed(filepath.Join(pciRoot, bdf, "label")),
			NUMA:       readIntField(filepath.Join(pciRoot, bdf, "numa_node")),
			Enabled:    readIntField(filepath.Join(pciRoot, bdf, "enable")),
			IOMMUGroup: -1,
		}
		classes[bdf] = f.Class
		if g, ok := byBDF[bdf]; ok {
			f.IOMMUGroup = g
			peers := append([]string{}, groupDevices[g]...)
			sort.Strings(peers)
			f.GroupDevices = peers
			f.GroupSize = len(peers)
			f.GroupClassesKnown = true
			for _, peer := range peers {
				c, ok := classOf(peer)
				if !ok {
					f.GroupClassesKnown = false
					continue
				}
				if strings.HasPrefix(c, "06") { // 0600/0604/0608…：PCI 桥与 PCIe root port
					f.GroupHasBridge = true
				}
			}
		}
		inv.Devices = append(inv.Devices, f)
	}
	sort.Slice(inv.Devices, func(i, j int) bool { return inv.Devices[i].BDF < inv.Devices[j].BDF })
	return inv, nil
}

// readIOMMUGroups 读 /sys/kernel/iommu_groups 的组清单，填 BDF→组号 与 组号→BDF 清单。
//
// 返回 (可用, error)：目录不存在 ⇒ (false, nil)——内核未启用 IOMMU，这就是「无 IOMMU 组」的事实；
// 其它读取失败 ⇒ (false, error)，由调用方如实标注「取不到」（不当成「不可直通」）。
func readIOMMUGroups(root string, byBDF map[string]int, groupDevices map[int][]string) (bool, error) {
	groups, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("IOMMU 组目录 %s 不可读: %w", root, err)
	}
	for _, g := range groups {
		if !g.IsDir() {
			continue
		}
		n, err := strconv.Atoi(g.Name())
		if err != nil {
			continue // 非数字条目不是组（sysfs 里不会出现，防御性跳过）
		}
		devs, err := os.ReadDir(filepath.Join(root, g.Name(), "devices"))
		if err != nil {
			// 单组 devices 读不到：不猜组成，交「组内 class 未知」那条口径（该组成员也不登记）。
			continue
		}
		list := make([]string, 0, len(devs))
		for _, d := range devs {
			list = append(list, d.Name())
			byBDF[d.Name()] = n
		}
		sort.Strings(list)
		groupDevices[n] = list
	}
	return true, nil
}

// readHexField 读 sysfs 的十六进制字段并去掉 `0x` 前缀（vendor/device/class）。
// 值不合预期（位数不足/非十六进制）一律返回空——调用方渲染 `-`，不猜。
func readHexField(path string, digits int) string {
	s := readTrimmed(path)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" || len(s) > digits {
		return ""
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return strings.ToLower(s)
}

// readTrimmed 读文件并去空白；读不到返回空串。
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readIntField 读整数字段；读不到返回 -1（「取不到」与「0」不是一回事）。
func readIntField(path string) int {
	if s := readTrimmed(path); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return -1
}

// readLinkBase 读符号链接并取 basename（`driver` → `vmxnet3`）；不存在/读不到返回空。
func readLinkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
