package config

// BUG-25（v3.0.29 外部测试报告）：`resource-pools hugepages` 的声明与 **hugetlbfs 挂载点**
// 脱节时，提交期只提示「需 reboot 生效」——操作者据此认为「重启就好了」，而重启后池虽已建好，
// libvirt 仍找不到该页尺寸的挂载点，VM 起不来，报的是英文原文
// `Unable to find any usable hugetlbfs mount for 2048 KiB`（真机现场）。
//
// 本文件在**提交期**补一条挂载点覆盖预检：新配置声明的每个页尺寸，若当前挂载表里没有对应
// pagesize 的 hugetlbfs 挂载点，就点名缺哪个、并说清「重启后由产品按 fstab 重建」与「在此之前
// 该池的 VNF 起不来」。**只提示、不阻断**（提交本身是对的——fstab 会在重启后把它挂起来）。
//
// 为什么落在这里而不是校验里：缺挂载点是「尚未生效」而不是「配置非法」；把它做成校验失败会让
// 首次声明 2M 池（本机还没重启过）永远提交不了。与内核基线那族提示（决策 #459、
// `internal/api/cli_kernel_baseline.go` 的 hugepageMountDiffs）同族同口径，区别只在**落点**：
// 那边是 `show system kernel` 的差异视图，这里是**提交期**（操作者刚敲完 set 的那一刻）。
//
// 只读：本文件只解析 /proc/mounts 与 /proc/meminfo，不写任何东西、不 mount、不改 sysctl。

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// 挂载表与内核默认页尺寸的读取落点（测试可改，照 `var sysfsNetRoot` 的既有范式）。
var (
	hugepageMountsPath  = "/proc/mounts"
	hugepageMeminfoPath = "/proc/meminfo"
)

// hugepageMountState 一次挂载表读取的解析结果。
type hugepageMountState struct {
	mountedKB []int // 已挂载 hugetlbfs 的页尺寸（kB，按出现顺序）
	readErr   error // 读不到挂载表时的原因（nil = 读到了）
	// defaultKB 内核默认大页尺寸（kB，取 /proc/meminfo 的 Hugepagesize）；
	// 0 = 未知（此时未标注 pagesize 的挂载点无法归属，保守地不计入任何页尺寸）。
	defaultKB int
}

// readHugepageMountState 读并解析当前挂载状态（读失败时 readErr 非空，其余字段尽力而为）。
func readHugepageMountState() hugepageMountState {
	defaultKB := readKernelDefaultHugepageKB()
	raw, err := os.ReadFile(hugepageMountsPath)
	if err != nil {
		return hugepageMountState{readErr: err, defaultKB: defaultKB}
	}
	return parseHugepageMounts(string(raw), defaultKB)
}

// readKernelDefaultHugepageKB 取内核默认大页尺寸（/proc/meminfo 的 `Hugepagesize:` 行，kB）。
// 读不到返回 0（未知——调用方据此不对「未标注 pagesize 的挂载点」作归属猜测）。
func readKernelDefaultHugepageKB() int {
	raw, err := os.ReadFile(hugepageMeminfoPath)
	if err != nil {
		return 0
	}
	for _, ln := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if !ok || strings.TrimSpace(k) != "Hugepagesize" {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			return 0
		}
		n, err := strconv.Atoi(f[0])
		if err != nil || n <= 0 {
			return 0
		}
		return n // meminfo 的单位恒为 kB，不随字段值变化
	}
	return 0
}

// parseHugepageMounts 解析挂载表文本，取出每个 hugetlbfs 挂载点的页尺寸（kB）。
//
// 两处口径必须照实况（决策 #459 真机实测）：
//   - 内核把 `pagesize=` **规范化成最大整数单位**——1G 写作 `1024M`、2M 写作 `2M`。
//     故按解析出的 kB 值比较，不能按字面串比（按字面比会恒漏报 1G）。
//   - 挂载行**可以不写 `pagesize=`**：此时该挂载点属于**内核默认尺寸**池
//     （/proc/meminfo 的 `Hugepagesize:`）。defaultKB<=0（未知）时不计入任何尺寸——
//     不猜测，让上层如实报「无法核对」而不是假称覆盖。
//
// 纯函数（输入文本 + 默认尺寸），与文件系统解耦，便于单测覆盖各种实况形态。
func parseHugepageMounts(mounts string, defaultKB int) hugepageMountState {
	st := hugepageMountState{defaultKB: defaultKB}
	for _, ln := range strings.Split(mounts, "\n") {
		f := strings.Fields(ln)
		// /proc/mounts 的列：device mountpoint fstype options dump pass
		if len(f) < 4 || f[2] != "hugetlbfs" {
			continue
		}
		kb := 0
		for _, opt := range strings.Split(f[3], ",") {
			if v, ok := strings.CutPrefix(opt, "pagesize="); ok {
				kb = parseHugepagePagesizeKB(v)
				break
			}
		}
		if kb == 0 {
			// 未标注（或标了个解析不出的值）：按内核默认尺寸归属；默认尺寸未知则不归属。
			// 这条按「未标注」处理而不是「报缺」——它与 true 情况无法区分，宁可不少报。
			kb = defaultKB
		}
		if kb > 0 {
			st.mountedKB = append(st.mountedKB, kb)
		}
	}
	return st
}

// parseHugepagePagesizeKB 把 hugetlbfs 挂载选项 pagesize 的值解析成 kB。
// 支持内核会写的全部形态：`1024M`、`2M`、`1G`、`2048kB`、`1048576`（纯数按 kB）。
// 解析不出返回 0（调用方据此按「未标注」处理，不猜）。
func parseHugepagePagesizeKB(v string) int {
	v = strings.TrimSpace(v)
	mult := 1
	switch {
	case strings.HasSuffix(v, "kB"):
		v = strings.TrimSuffix(v, "kB")
	case strings.HasSuffix(v, "K"):
		v = strings.TrimSuffix(v, "K")
	case strings.HasSuffix(v, "M"):
		v, mult = strings.TrimSuffix(v, "M"), 1024
	case strings.HasSuffix(v, "G"):
		v, mult = strings.TrimSuffix(v, "G"), 1024*1024
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return n * mult
}

// hugepagePageSizeKB 产品托管的页尺寸名 → kB（未知尺寸返回 0：不参与判定）。
func hugepagePageSizeKB(pageSize string) int {
	switch pageSize {
	case "1G":
		return 1024 * 1024
	case "2M":
		return 2048
	}
	return 0
}

// missingHugepageMounts 已声明页尺寸里**没有**对应挂载点的那些（按声明顺序，去重）。
// 未知页尺寸（空/"4K" 之类）跳过——模型校验已拦，这里不制造噪声。
func missingHugepageMounts(declared []string, mountedKB []int) []string {
	have := make(map[int]bool, len(mountedKB))
	for _, kb := range mountedKB {
		have[kb] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, ps := range declared {
		kb := hugepagePageSizeKB(ps)
		if kb == 0 || seen[ps] {
			continue
		}
		seen[ps] = true
		if !have[kb] {
			out = append(out, ps)
		}
	}
	return out
}

// declaredHugepageSizes 新配置里**已声明且页数 > 0** 的页尺寸（顺序按配置声明顺序、去重）。
// count<=0 视为「不托管该池」（与 `internal/model` 账本、内核基线派生的口径一致）——
// 不托管的池不需要挂载点，不该为它报警。
func declaredHugepageSizes(pools *model.ResourcePool) []string {
	if pools == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, hp := range pools.Hugepages {
		if hp.Count <= 0 || seen[hp.PageSize] {
			continue
		}
		seen[hp.PageSize] = true
		out = append(out, hp.PageSize)
	}
	return out
}

// hugepageMountWarnings 提交期的大页挂载点覆盖预检（BUG-25；**只提示、不阻断**）。
//
// pools 是新配置（candidate 已提交生效的那份）的资源池。只在**本次声明的页尺寸**范围内判：
// 未被声明的池不提示（不制造噪声），已挂载的尺寸不提示。
//
// 三种形态分别给话：
//   - 读不到挂载表 → 如实说「无法核对」（不谎报通过）；
//   - 缺挂载点 → 点名缺哪个，并说清重启后由产品按 fstab 重建、在此之前该池的 VNF 起不来
//     （把 libvirt 的英文原文一并带上，操作者搜得到，也认得出这就是他要看的那条）；
//   - 都覆盖 → 不返回任何东西。
func hugepageMountWarnings(pools *model.ResourcePool) []string {
	declared := declaredHugepageSizes(pools)
	if len(declared) == 0 {
		return nil
	}
	st := readHugepageMountState()
	if st.readErr != nil {
		return []string{fmt.Sprintf(
			"警告: 无法核对大页 %s 的 hugetlbfs 挂载点（读取 %s 失败：%v）——"+
				"若本机尚未挂载该页尺寸，重启后由产品按 fstab 重建；在此之前该池的 VNF 起不来"+
				"（libvirt 报 unable to find any usable hugetlbfs mount）",
			strings.Join(declared, "/"), hugepageMountsPath, st.readErr)}
	}
	missing := missingHugepageMounts(declared, st.mountedKB)
	if len(missing) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"警告: 大页 %s 尚无对应的 hugetlbfs 挂载点（当前挂载表 %s 里没有该 pagesize）——"+
			"重启后由产品按 fstab 重建（request system kernel apply 写入基线 → request system reboot）；"+
			"在此之前该池的 VNF 起不来（libvirt 报 unable to find any usable hugetlbfs mount）",
		strings.Join(missing, "/"), hugepageMountsPath)}
}
