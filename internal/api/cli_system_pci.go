package api

// BUG-24：`show system pci [detail]`——PCI 设备 / IOMMU 组 / 当前驱动 / 直通可用性的读视图。
//
// 由来：产品此前只有 `set virtual-machine-functions <name> pci-device <bdf>`，没有任何命令能
// **列出**可选 BDF、它们的 IOMMU 组或当前驱动：运维先 ssh 进 shell 跑 `lspci` 才知道填什么，
// 且无法判断某个 BDF 是否与无关设备同组（真机现场：`0000:0b:00.0` 落在 group 7，同组还有
// 8 个 PCIe root port）。本文件只做**渲染**——事实全取 `internal/orchestrator/compute`
// 的 `ReadPCIInventory`（sysfs 单一事实源，只读），不在这里二次推断。
//
// 事实源注入方案（本实现取**改动最小**的一档）：读函数直接调用 compute 包的导出入口，
// 以**包级变量** `pciInventorySource` 供单测替换，**不**扩大 cliExecutor 字段 /
// `server.go` Options / `cliexec.go` 的注入面——本命令不随数据面/编排器状态变化，
// 与 `pciExists`（要按运行态注入）的取舍不同。
//
// 如实口径（与本仓库既有读视图一致）：
//   - PCI 目录不可读 ⇒ 报 `%% 取不到 …（原因）`，**不谎报空清单**；
//   - 单设备字段读不到 ⇒ 该列 `-`；IOMMU 组目录不存在（内核未启用 IOMMU）⇒ 全部设备按
//     「无 IOMMU 组」呈现（这正是「不可直通」的事实），并在页脚如实说明 IOMMU 状态；
//   - 厂商/设备**名称**（`15ad:07b0` → "VMware VMXNET3"）**不做**——那张表是 pci.ids，
//     产品不打包、也不联网取，故只显示 ID 形态；这一点写进口径行，不承诺做不到的。

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/orchestrator/compute"
)

// pciInventorySource PCI 清单事实源（测试可替换；生产＝读 sysfs）。
var pciInventorySource = compute.ReadPCIInventory

// pciSourceLine 读视图的来源行（与 compute 包的实际落点同一常量，避免文案与实现漂移）。
func pciSourceLine() string {
	return fmt.Sprintf("PCI 设备（来源：%s + %s）\n", compute.PCIDevicesDir, compute.IOMMUGroupsDir)
}

// renderSystemPCI `show system pci [detail]`（R）。
func (x *cliExecutor) renderSystemPCI(args []string) string {
	detail := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "detail":
		detail = true
	default:
		return "%% 无效命令: show system pci " + strings.Join(args, " ") + "（可用：detail）\n"
	}
	inv, err := pciInventorySource()
	if err != nil {
		// 取不到 ≠ 没有设备：如实报原因，不返回空清单（空清单会被读成「本机没有 PCI 设备」）。
		return "%% 取不到 PCI 设备清单：" + err.Error() + "\n"
	}

	var b strings.Builder
	b.WriteString(pciSourceLine())
	if len(inv.Devices) == 0 {
		b.WriteString("（未枚举到 PCI 设备）\n")
		b.WriteString(pciFooter(inv))
		x.structured = pciStructured(inv)
		return b.String()
	}

	// 列按**显示宽度**对齐（中文/全角算 2 列）——纯 `%-Ns` 按 rune 数补，混排中文会错列。
	fmt.Fprintf(&b, "%s %s %s %s %s %s %s\n",
		pciPad("BDF", 15), pciPad("厂商:设备", 11), pciPad("类型", 14),
		pciPad("IOMMU 组", 10), pciPad("组内设备", 10), pciPad("驱动", 14), "直通")
	for _, d := range inv.Devices {
		fmt.Fprintf(&b, "%s %s %s %s %s %s %s\n",
			pciPad(d.BDF, 15), pciPad(pciVendorDevice(d), 11), pciPad(pciClassText(d.Class), 14),
			pciPad(pciGroupNum(d.IOMMUGroup), 10), pciPad(pciGroupSize(d), 10),
			pciPad(pciDash(d.Driver), 14), pciPassthroughText(d))
	}
	if detail {
		for _, d := range inv.Devices {
			b.WriteString(pciDetailBlock(d))
		}
	}
	b.WriteString(pciFooter(inv))
	x.structured = pciStructured(inv)
	return b.String()
}

// pciFooter 口径行：三件事必须写清——组的口径（含桥）、名称为什么只有 ID、最终判定权在谁。
func pciFooter(inv compute.PCIInventory) string {
	var b strings.Builder
	if !inv.IOMMUAvailable {
		reason := inv.IOMMUError
		if reason == "" {
			reason = "内核未启用 IOMMU 或无 IOMMU 组目录"
		}
		fmt.Fprintf(&b, "IOMMU：取不到组信息（%s）——下表全部按「无 IOMMU 组」呈现，不据此断言可直通\n", reason)
	}
	b.WriteString("口径：IOMMU 组取 " + compute.IOMMUGroupsDir + "/<n>/devices；「组内设备」含桥/root port" +
		"（直通需整组隔离）；「驱动」取设备 driver 符号链接（未绑驱动显示 -）；" +
		"直通建议以 IOMMU 组是否存在、组内是否含非端点设备为依据，最终以 libvirt 报错为准。\n")
	b.WriteString("名称：厂商/设备只显示 ID 形态（如 15ad:07b0）——人类可读名称需 pci.ids，产品不打包也不联网取，故不显示。\n")
	return b.String()
}

// pciDetailBlock 单设备明细块（缩进两格，字段逐项对齐；读不到的显示 -）。
func pciDetailBlock(d compute.PCIDeviceFacts) string {
	var b strings.Builder
	b.WriteString("\n" + d.BDF + "\n")
	vd := pciVendorDevice(d)
	if d.Label != "" {
		vd += "（label: " + d.Label + "）"
	}
	fmt.Fprintf(&b, "  %-14s %s\n", "vendor:device", vd)
	fmt.Fprintf(&b, "  %-14s %s（%s）\n", "class", pciDash(d.Class), pciClassText(d.Class))
	fmt.Fprintf(&b, "  %-14s %s\n", "enable", pciIntNum(d.Enabled))
	fmt.Fprintf(&b, "  %-14s %s\n", "numa_node", pciIntNum(d.NUMA))
	fmt.Fprintf(&b, "  %-14s %s\n", "driver", pciDash(d.Driver))
	if d.IOMMUGroup < 0 {
		fmt.Fprintf(&b, "  %-14s %s\n", "iommu_group", "-（无组）")
	} else if len(d.GroupDevices) == 0 {
		// 有组号但没有成员清单（不该出现，防御性）：只说台数，不拼出「9 台：」这种半截句。
		fmt.Fprintf(&b, "  %-14s %d（%d 台；成员清单取不到）\n", "iommu_group", d.IOMMUGroup, d.GroupSize)
	} else {
		fmt.Fprintf(&b, "  %-14s %d（%d 台：%s）\n", "iommu_group", d.IOMMUGroup, d.GroupSize,
			strings.Join(d.GroupDevices, ", "))
	}
	fmt.Fprintf(&b, "  %-14s %s\n", "passthrough", pciPassthroughText(d))
	return b.String()
}

// pciStructured `| display json/xml` 的树形态（字段与列同源，不另立一套）。
func pciStructured(inv compute.PCIInventory) map[string]any {
	items := make([]any, 0, len(inv.Devices))
	for _, d := range inv.Devices {
		row := map[string]any{
			"bdf": d.BDF, "vendor": d.Vendor, "device": d.Device, "class": d.Class,
			"class_text": pciClassText(d.Class), "driver": d.Driver,
			"iommu_group": d.IOMMUGroup, "group_devices": d.GroupDevices,
			"group_size": d.GroupSize, "group_has_bridge": d.GroupHasBridge,
			"passthrough": pciPassthroughText(d), "passthrough_kind": d.PassthroughKind(),
			"numa_node": d.NUMA, "enabled": d.Enabled,
		}
		if d.Label != "" {
			row["label"] = d.Label
		}
		items = append(items, row)
	}
	out := map[string]any{
		"pci_devices":     items,
		"iommu_available": inv.IOMMUAvailable,
		"source":          compute.PCIDevicesDir + " + " + compute.IOMMUGroupsDir,
	}
	if inv.IOMMUError != "" {
		out["iommu_error"] = inv.IOMMUError
	}
	return out
}

// pciVendorDevice 厂商:设备（任一侧取不到即 -，不拼出「15ad:」这种半截值）。
func pciVendorDevice(d compute.PCIDeviceFacts) string {
	v, de := pciDash(d.Vendor), pciDash(d.Device)
	if v == "-" && de == "-" {
		return "-"
	}
	return v + ":" + de
}

// pciClassText class code → 类型列。只映射常见基类；未收录的如实答「其它」（不猜具体型号）。
func pciClassText(class string) string {
	if len(class) < 2 {
		return "-"
	}
	switch class[:2] {
	case "01":
		return "存储控制器"
	case "02":
		if strings.HasPrefix(class, "0200") {
			return "Ethernet"
		}
		return "网络控制器"
	case "03":
		return "显示控制器"
	case "04":
		return "多媒体控制器"
	case "05":
		return "内存控制器"
	case "06":
		return "PCI 桥"
	case "07":
		return "通信控制器"
	case "08":
		return "系统外设"
	case "0c":
		return "串行总线"
	case "0d":
		return "无线控制器"
	case "10":
		return "加密控制器"
	case "12":
		return "加速器"
	}
	return "其它"
}

// pciPassthroughText 直通列：只按**事实**给建议，并保留「最终以 libvirt 报错为准」的口径。
func pciPassthroughText(d compute.PCIDeviceFacts) string {
	switch d.PassthroughKind() {
	case compute.PassthroughBound:
		return "已在直通（" + compute.VFIOPCIDriver + "）"
	case compute.PassthroughOK:
		return "可用"
	case compute.PassthroughGroupHasBridge:
		return "可用（同组含桥，需整组直通）"
	case compute.PassthroughNoGroup:
		return "不可用（无 IOMMU 组）"
	case compute.PassthroughUnknown:
		return "待定（同组设备 class 取不到）"
	}
	return "-"
}

// pciGroupNum 组号列（-1 = 无组/取不到）。
func pciGroupNum(g int) string { return pciIntNum(g) }

// pciGroupSize 组内设备数列（无组为 -；有组但清单取不到时也如实 -，不给 0）。
func pciGroupSize(d compute.PCIDeviceFacts) string {
	if d.IOMMUGroup < 0 || d.GroupSize <= 0 {
		return "-"
	}
	return strconv.Itoa(d.GroupSize)
}

// pciIntNum 整数字段：负数（取不到）显示 -。
func pciIntNum(v int) string {
	if v < 0 {
		return "-"
	}
	return strconv.Itoa(v)
}

// pciDash 空值占位（与既有读视图的 `-` 口径一致）。
func pciDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pciPad 按**显示宽度**右补齐到 w 列（中文/全角算 2 列）。
//
// 为什么不用 `%-Ns`：Go 的宽度按 rune 数计，"通信控制器"（6 rune）会占 12 个终端列，
// 与该表其它 ASCII 列混排时后面全部错列——读起来就像数据串了行。宽度不足时原样返回
// （宁可长出一列，也不截断内容）。
func pciPad(s string, w int) string {
	n := 0
	for _, r := range s {
		n += pciRuneWidth(r)
	}
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// pciRuneWidth 单个字符的终端显示宽度（CJK/全角/宽字符算 2，其余算 1）。
//
// 只覆盖读视图里实际会出现的字符集：ASCII、中日韩统一表意文字、全角标点/符号。
// 更宽的算法（EastAsianWidth 全表）在这里没有收益，反而要引入依赖。
func pciRuneWidth(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首…彝文（含汉字、假名、注音）
		r >= 0xA960 && r <= 0xA97F, // Hangul Jamo Extended-A
		r >= 0xAC00 && r <= 0xD7A3, // Hangul 音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意文字
		r >= 0xFE10 && r <= 0xFE19, // 竖排标点
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // 表情（读视图里目前没有，防御性覆盖）
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B 及以后
		return 2
	}
	return 1
}
