package system

// 内核基线的真机补全与护栏（FR-SYS-014）。
//
// GenerateBaseline 是纯函数；vendor 参数、irqaffinity 与 nohz_full 支持探测依赖**真机事实**，
// 故由 EnrichDesired 在两条写路径上统一补全——安装器（nfvisd -print-kernel-baseline）与
// CLI（request system kernel apply → BaselineApplier.Apply）产出同源，安装器不另起一套参数来源。
// ValidateDesired 则在写盘前拦截会「重启后进不了系统」的隔离核配置，同样两条路径共用。
//
// show system kernel 的三方对照**不**做补全：irqaffinity 与 vendor 参数是真机派生默认，
// 不是配置期望，参与对照会把主机差异误报成「配置与基线不一致」。
//
// 所有读取都以 root 为前缀，便于单测用临时目录构造 /proc、/sys 内容（非 Linux 亦安全）；
// 读不到的事实一律**保持原样**（识别不了厂商就不加参数、读不到在线核就不写 irqaffinity），
// 不猜测、不降级成另一套行为。

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// CPUVendor 从 /proc/cpuinfo 识别的 CPU 厂商。
type CPUVendor string

const (
	VendorIntel   CPUVendor = "intel"
	VendorAMD     CPUVendor = "amd"
	VendorUnknown CPUVendor = "" // 识别不了（非常规环境/单测临时目录）
)

// detectCPUVendor 读 /proc/cpuinfo 的 vendor_id（GenuineIntel / AuthenticAMD）。
func detectCPUVendor(root string) CPUVendor {
	b, err := os.ReadFile(join(root, "/proc/cpuinfo"))
	if err != nil {
		return VendorUnknown
	}
	for _, ln := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if !ok || strings.TrimSpace(k) != "vendor_id" {
			continue
		}
		switch strings.TrimSpace(v) {
		case "GenuineIntel":
			return VendorIntel
		case "AuthenticAMD":
			return VendorAMD
		}
		return VendorUnknown
	}
	return VendorUnknown
}

// iommuFamilyNames IOMMU 语义族：这三个参数名都作用于同一件事（是否启用/如何使用 IOMMU），
// 只按参数名去重会漏掉它们之间的冲突——`iommu=off` 与 `intel_iommu=on` 名字不同、都合法，
// 合在一起却是自相矛盾的产物（真机 v3.0.24：`set system kernel iommu off` 之后片段里同时出现）。
// 族内判据一律走本表，别处不再各自列名字。
var iommuFamilyNames = map[string]bool{"iommu": true, "intel_iommu": true, "amd_iommu": true}

// isIOMMUFamilyParam 判断 p（形如 "intel_iommu=on"，也容忍无值的裸参数）是否属 IOMMU 语义族。
func isIOMMUFamilyParam(p string) bool {
	name, _, _ := strings.Cut(p, "=")
	return iommuFamilyNames[name]
}

// paramValue 取 "name=value" 的 value（无 "=" 时返回空串）。
func paramValue(p string) string {
	_, v, ok := strings.Cut(p, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// iommuOff 托管字段是否**明确关闭** IOMMU（只认字段）。
func iommuOff(d KernelDesired) bool {
	return strings.EqualFold(strings.TrimSpace(d.IOMMU), "off")
}

// iommuDisabled 期望基线是否表达「关闭 IOMMU」——**任一**族内成员写 off 都算：
// 托管字段（`set system kernel iommu off`）与 params 逃生口（`intel_iommu=off`/`iommu=off`…）
// 虽然入口不同，表达的仍是同一个意图。厂商默认的开启开关必须让位给这个意图：
// 两者并存正是本缺陷的产物（真机 v3.0.24 的 `iommu=off intel_iommu=on`，两套开关各自合法、
// 合起来无人能读懂，实际生效语义取决于内核解析顺序）。
//
// 只认可识别的 off；on/pt/未设置一律不动（见 EnrichDesired 与 iommuFamilyParams）。
func iommuDisabled(d KernelDesired) bool {
	if iommuOff(d) {
		return true
	}
	for _, p := range d.ExtraParams {
		if isIOMMUFamilyParam(p) && strings.EqualFold(paramValue(p), "off") {
			return true
		}
	}
	return false
}

// iommuFamilyParams 输出进入 cmdline 的 IOMMU 族参数（保证族内至多一个 `iommu=` 成员）。
//
// 处置口径（真机 v3.0.24 的 `iommu=off intel_iommu=on` 现场）：
//   - 托管字段非空时它是**最明确的意图**（也进 `show system kernel` 的三方对照）：
//     族内 `iommu=` 只由该字段产出，params 里同名的成员丢弃——两个 `iommu=` 并存时内核对同名
//     参数取最后一个，实际生效的既不是配置意图也不是操作者写下的那条；
//   - 托管字段明确为 off 时，params 里残留的厂商开关（intel_iommu/amd_iommu）一并丢弃：
//     「关」与厂商的「开」并存是自相矛盾的产物；
//   - 其余情形（on/pt）params 里的**厂商**开关原样保留（既有产出逐字不变）。
//
// 字段为空时 params 原样透传：此时族内参数全是操作者亲手写的，产品不替人删自己的话
// （产品自己引入的矛盾已由 EnrichDesired 的 iommuDisabled 抑制掉）。
func iommuFamilyParams(d KernelDesired) []string {
	if strings.TrimSpace(d.IOMMU) == "" {
		return d.ExtraParams
	}
	off := iommuOff(d)
	out := make([]string, 0, len(d.ExtraParams))
	for _, p := range d.ExtraParams {
		name, _, _ := strings.Cut(p, "=")
		if !iommuFamilyNames[name] {
			out = append(out, p)
			continue
		}
		if off || name == "iommu" {
			// off：族内成员全部让位给 iommu=off；非 off：字段产出的 iommu= 是唯一的那一个。
			continue
		}
		out = append(out, p) // on/pt：厂商开关照旧
	}
	return out
}

// vendorParams 每厂商的默认启动参数（NFV 底座常规基线）：
// IOMMU 开 + DMA 直通（iommu=pt 对未分配给 VM 的设备近似零开销）+ 关掉 cpufreq 驱动
// （intel_pstate/amd_pstate 的动态调频是抖动来源）。厂商识别不了时不加。
func vendorParams(v CPUVendor) []string {
	switch v {
	case VendorIntel:
		return []string{"intel_iommu=on", "intel_pstate=disable", "iommu=pt"}
	case VendorAMD:
		return []string{"amd_iommu=on", "amd_pstate=disable", "iommu=pt"}
	}
	return nil
}

// nohzFullSupported 探测内核是否编入 CONFIG_NO_HZ_FULL（nohz_full/rcu_nocbs 的前提）。
// 以 /sys/devices/system/cpu/nohz_full 是否存在为准（编入才会创建该属性，与 cmdline 无关）；
// 探测不到再看 /boot/config-$(uname -r)；两者都拿不到时按「支持」处理——
// 写了不支持只是被内核忽略，漏写了支持的优化才是损失。
func nohzFullSupported(root string) bool {
	if _, err := os.Stat(join(root, "/sys/devices/system/cpu/nohz_full")); err == nil {
		return true
	}
	rel, err := os.ReadFile(join(root, "/proc/sys/kernel/osrelease"))
	if err != nil {
		return true
	}
	cfg, err := os.ReadFile(join(root, "/boot/config-"+strings.TrimSpace(string(rel))))
	if err != nil {
		return true
	}
	return strings.Contains(string(cfg), "CONFIG_NO_HZ_FULL=y")
}

// parseCoreList 解析 "1,4-7" 形式的核列表（与 model.parseCoreList 同语义；
// 不复用以避免 system 依赖上层包）。返回有序去重结果。
func parseCoreList(s string) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, err1 := strconv.Atoi(a)
			hi, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil || lo > hi {
				return nil, fmt.Errorf("核区间 %q 不合法", part)
			}
			for x := lo; x <= hi; x++ {
				if !seen[x] {
					seen[x] = true
					out = append(out, x)
				}
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("核编号 %q 不合法", part)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

// compressCoreList 把核号压成紧凑区间文本（[0,4,5,6] → "0,4-6"）。
func compressCoreList(cores []int) string {
	if len(cores) == 0 {
		return ""
	}
	c := append([]int(nil), cores...)
	sort.Ints(c)
	var parts []string
	start, prev := c[0], c[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
			return
		}
		parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(prev))
	}
	for _, x := range c[1:] {
		if x == prev+1 {
			prev = x
			continue
		}
		flush()
		start, prev = x, x
	}
	flush()
	return strings.Join(parts, ",")
}

// readOnlineCores 读本机在线核（/sys/devices/system/cpu/online，形如 "0-5"）。
// 读不到返回 nil（在线核未知）。
func readOnlineCores(root string) []int {
	b, err := os.ReadFile(join(root, "/sys/devices/system/cpu/online"))
	if err != nil {
		return nil
	}
	cores, err := parseCoreList(strings.TrimSpace(string(b)))
	if err != nil {
		return nil
	}
	return cores
}

// IRQAffinityFor 返回隔离核在**在线核**中的补集（即应承接中断的核）。
// 在线核读不到、或补集为空时返回空串（调用方不写 irqaffinity）。
func IRQAffinityFor(isolated, root string) string {
	online := readOnlineCores(root)
	if len(online) == 0 {
		return ""
	}
	iso, err := parseCoreList(isolated)
	if err != nil {
		return ""
	}
	isoSet := map[int]bool{}
	for _, c := range iso {
		isoSet[c] = true
	}
	var rest []int
	for _, c := range online {
		if !isoSet[c] {
			rest = append(rest, c)
		}
	}
	return compressCoreList(rest)
}

// minHousekeepingCores 隔离核之外必须保留的最少核数：
// 内核自身与不可迁移线程、中断处理、管理面（nfvisd/libvirt/docker/SSH）都需要非隔离核，
// 且 irqaffinity 需要非隔离核作落点。1 个核既当中断又当全部管理面是病态配置。
const minHousekeepingCores = 2

// ValidateDesired 在写基线前校验隔离核配置（两条写路径共用——isolcpus 写错要到**重启后**
// 才暴露，且是「进不了系统」级别，必须在落盘前拦下）：
//   - 核号必须都在本机在线核内；
//   - 非隔离核至少保留 2 个。
//
// 除 IsolatedCores 字段外，也校验 params 逃生口里的 isolcpus=（它同样会进 cmdline，
// 且排在后面、实际生效）。在线核读不到时不下判断（非常规环境/单测）。
func ValidateDesired(d KernelDesired, root string) error {
	online := readOnlineCores(root)
	if len(online) == 0 {
		return nil
	}
	type check struct{ src, list string }
	checks := []check{}
	if d.IsolatedCores != "" {
		checks = append(checks, check{"isolcpus", d.IsolatedCores})
	}
	for _, p := range d.ExtraParams {
		if v, ok := strings.CutPrefix(p, "isolcpus="); ok {
			checks = append(checks, check{"params", v})
		}
	}
	for _, c := range checks {
		iso, err := parseCoreList(c.list)
		if err != nil {
			return fmt.Errorf("隔离核列表不合法（%s=%s）：%v", c.src, c.list, err)
		}
		onlineSet := map[int]bool{}
		for _, x := range online {
			onlineSet[x] = true
		}
		var missing []int
		for _, x := range iso {
			if !onlineSet[x] {
				missing = append(missing, x)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("隔离核含本机不存在的核 %s（本机在线核：%s）",
				compressCoreList(missing), compressCoreList(online))
		}
		if keep := len(online) - len(iso); keep < minHousekeepingCores {
			return fmt.Errorf("隔离核 %s 会把本机 %d 个核几乎全部隔离（只剩 %d 个非隔离核），"+
				"至少需保留 %d 个给内核/中断/管理面；请缩小隔离核范围",
				compressCoreList(iso), len(online), keep, minHousekeepingCores)
		}
	}
	return nil
}

// detectVirtualized 判断本机是否运行在虚拟机里（低延迟组的 idle=poll/tsc=reliable 与机型强相关）。
// 以 /proc/cpuinfo 的 hypervisor CPUID 标志为准（主流 hypervisor 都会暴露），
// 回退 /sys/class/dmi/id/sys_vendor 的常见虚拟厂商；两者都读不到按裸机处理。
func detectVirtualized(root string) bool {
	if b, err := os.ReadFile(join(root, "/proc/cpuinfo")); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(ln, ":"); ok && strings.TrimSpace(k) == "flags" {
				if slices.Contains(strings.Fields(v), "hypervisor") {
					return true
				}
			}
		}
	}
	if b, err := os.ReadFile(join(root, "/sys/class/dmi/id/sys_vendor")); err == nil {
		v := string(b)
		for _, vendor := range []string{"QEMU", "VMware", "innotek", "Microsoft", "Xen", "KVM"} {
			if strings.Contains(v, vendor) {
				return true
			}
		}
	}
	return false
}

// EnrichDesired 以真机事实补全期望基线（写盘前调用，保证安装器与 CLI 同源）：
//   - 按 CPU 厂商补默认参数；与显式设置（IOMMU 字段、params 逃生口）同名冲突时**以用户为准**；
//   - 隔离核非空时补 irqaffinity=<非隔离核>（中断落在非隔离核上；用户已给 irqaffinity 则不覆盖）；
//   - 探测 nohz_full 支持，不支持时标记省略 nohz_full/rcu_nocbs；
//   - 低延迟 profile 开启且**非虚拟机**时补 idle=poll/tsc=reliable（VM 里 idle=poll 是反作用、
//     tsc=reliable 未必成立；机型无关的一半由 GenerateBaseline 产出）。
//
// 幂等：对已补全的结果重复调用，产出不变。
func EnrichDesired(d KernelDesired, root string) KernelDesired {
	out := d
	// 期望基线表达了「关闭 IOMMU」时（字段或 params 逃生口任一处说 off），厂商默认的开启开关
	// 必须让位（见 iommuDisabled）：两者并存会让「关」这个意图在 cmdline 上自相矛盾，
	// 实际生效语义取决于内核解析顺序，不是任何人写下的意图。
	disableIOMMU := iommuDisabled(d)
	for _, p := range vendorParams(detectCPUVendor(root)) {
		if disableIOMMU && isIOMMUFamilyParam(p) {
			continue
		}
		if !paramTaken(p, out) {
			out.ExtraParams = append(out.ExtraParams, p)
		}
	}
	if out.IsolatedCores != "" && out.IRQAffinity == "" && !hasParam(out, "irqaffinity") {
		out.IRQAffinity = IRQAffinityFor(out.IsolatedCores, root)
	}
	if out.LowLatency && !detectVirtualized(root) {
		for _, p := range []string{"idle=poll", "tsc=reliable"} {
			if !paramTaken(p, out) {
				out.ExtraParams = append(out.ExtraParams, p)
			}
		}
	}
	v := nohzFullSupported(root)
	out.NoHZFull = &v
	return out
}

// paramTaken 判断参数 p（形如 "name=value"）是否已被显式设置占位——
// 占位时补全不得再写同名参数（内核对同名参数取最后一个，显式在后会让补全值「看起来」生效，
// 而实为用户值被覆盖，两边都得避免）。
func paramTaken(p string, d KernelDesired) bool {
	name, _, _ := strings.Cut(p, "=")
	return hasParam(d, name)
}

// hasParam 判断某参数名是否已被显式设置（IOMMU/IRQAffinity 字段或 params 逃生口）。
func hasParam(d KernelDesired, name string) bool {
	for _, p := range d.ExtraParams {
		if n, _, _ := strings.Cut(p, "="); n == name {
			return true
		}
	}
	switch name {
	case "iommu":
		return d.IOMMU != ""
	case "irqaffinity":
		return d.IRQAffinity != ""
	}
	return false
}
