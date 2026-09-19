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

// EnrichDesired 以真机事实补全期望基线（写盘前调用，保证安装器与 CLI 同源）：
//   - 按 CPU 厂商补默认参数；与显式设置（IOMMU 字段、params 逃生口）同名冲突时**以用户为准**；
//   - 隔离核非空时补 irqaffinity=<非隔离核>（中断落在非隔离核上；用户已给 irqaffinity 则不覆盖）；
//   - 探测 nohz_full 支持，不支持时标记省略 nohz_full/rcu_nocbs。
//
// 幂等：对已补全的结果重复调用，产出不变。
func EnrichDesired(d KernelDesired, root string) KernelDesired {
	out := d
	for _, p := range vendorParams(detectCPUVendor(root)) {
		if !paramTaken(p, out) {
			out.ExtraParams = append(out.ExtraParams, p)
		}
	}
	if out.IsolatedCores != "" && out.IRQAffinity == "" && !hasParam(out, "irqaffinity") {
		out.IRQAffinity = IRQAffinityFor(out.IsolatedCores, root)
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
