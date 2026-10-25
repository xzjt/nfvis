package compute

// BUG-25（v3.0.29 外部测试报告）第二处：VM **启动前**的大页可用量预检。
//
// 由来（真机现场）：`set resource-pools hugepages page-size 1G count 2` 提交后只提示
// 「需 reboot 生效」，重启后池建好了，但 VM 起不来，操作者看到的是一句 libvirt 英文原文
//   Unable to find any usable hugetlbfs mount for 2048 KiB
// ——既没说「哪个池不够」，也没说「差多少页」，更没给照做路径。本文件的预检把这条失败
// **提前到进入 api.Start 之前**（不产生半成品域、不必等 QEMU 起来），并给中文、可操作的话。
//
// 与既有前置判定的关系（**不重复既有行为**）：
//   - `checkDataPlaneForStart`（#314）判「数据面进程在不在」——VPP 没起来时 vhost-user 会
//     在 api.Start 内阻塞；那是进程连通性问题，与大页数量无关。
//   - `checkConsolePortForStart`（#485 复审）判「telnet 派生端口被谁占了」。
//   - 本预检判「**空闲大页够不够**」。此前没有任何一层判它：model 账本（PoolLedger）按
//     **配置声明**做配额演练，与**内核当下空闲页**是两回事（声明 2 页、内核只分配到 1 页时
//     账本仍通过），libvirt 只在真正分配失败时以英文原文报错。
// 三者落点相同（都在 api.Start 之前）、互不替代。
//
// 判定口径：
//   - 只看**该 VM 实际使用的页尺寸**（`AllocatedResources.HugepageSize`，缺省取资源池主池，
//     与账本/域组装单源）；backing=normal（普通内存）不占大页池 ⇒ 不判。
//   - 需求页数 = memory.size-mb / 页大小，**向上取整**（与 model 账本同口径）。
//   - 空闲页数取 `/sys/kernel/mm/hugepages/hugepages-<kB>kB/free_hugepages`（按尺寸，比
//     `/proc/meminfo` 的 HugePages_Free 更精确——后者只反映内核默认尺寸池）。
//   - **读不到就不判**（不阻断）：拿不到事实时宁可让 libvirt 去尝试，也不误拒一台本该能起的 VM。
//     与 checkConsolePortForStart 的「探测失败不阻断」同口径。
//
// 注入：probe 由装配层注入（`Config.HugepageProbe`，nil = 不判）——与 `DataPlaneProbe` 同一
// 范式。为什么不直接读文件：本包的单测在任意宿主上跑，直接读 `/sys` 会让「起 VM 成功/失败」
// 随宿主的空闲大页数而变（历史教训：`readMounts` 就是为了这个提成可替换变量）。生产装配见
// cmd/nfvisd（`compute.SysfsHugepageProbe`），行为与直接读文件一致。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// hugepageSysfsRoot sysfs 大页目录（`SysfsHugepageProbe` 的读取根；单测可改）。
var hugepageSysfsRoot = "/sys/kernel/mm/hugepages"

// hugepageMeminfoPath /proc/meminfo（`SysfsHugepageProbe` 的回退读点；单测可改）。
var hugepageMeminfoPath = "/proc/meminfo"

// SysfsHugepageProbe 生产用的大页空闲量探针：优先读
// `/sys/kernel/mm/hugepages/hugepages-<kB>kB/free_hugepages`（**按尺寸**，最精确）。
//
// 读不到时回退到 `/proc/meminfo` 的 `HugePages_Free`——但那只是**内核默认尺寸**池的空闲数，
// 故仅当 meminfo 的 `Hugepagesize:` 与请求的页尺寸一致时才用它（否则会用别的池的数字冒充）。
//
// ok=false 表示「读不到/解析不出」（内核未提供该尺寸池、或 sysfs 与 meminfo 都不可用）——
// 调用方据此**不判定**，不把「查不到」当成「没有」。
func SysfsHugepageProbe(pageSize string, needPages int) (freePages int, ok bool) {
	_ = needPages // 读数与需求无关（留参数以便将来做「按需求预占」类探针）
	kb, okKB := hugepagePageSizeKB(pageSize)
	if !okKB {
		return 0, false
	}
	if raw, err := os.ReadFile(fmt.Sprintf("%s/hugepages-%dkB/free_hugepages", hugepageSysfsRoot, kb)); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n >= 0 {
			return n, true
		}
	}
	free, defaultKB, okMem := readMeminfoHugepages()
	if !okMem || defaultKB != kb {
		return 0, false
	}
	return free, true
}

// readMeminfoHugepages 取 /proc/meminfo 里的 `HugePages_Free`（页数）与 `Hugepagesize`（kB）。
// 两个字段任一读不到即 ok=false——它们的含义只在**成对**时成立（空闲数属于那个尺寸的池）。
func readMeminfoHugepages() (free int, defaultKB int, ok bool) {
	raw, err := os.ReadFile(hugepageMeminfoPath)
	if err != nil {
		return 0, 0, false
	}
	free, defaultKB = -1, 0
	for _, ln := range strings.Split(string(raw), "\n") {
		k, v, found := strings.Cut(ln, ":")
		if !found {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.Atoi(f[0])
		if err != nil || n < 0 {
			continue
		}
		switch strings.TrimSpace(k) {
		case "HugePages_Free":
			free = n
		case "Hugepagesize":
			defaultKB = n
		}
	}
	if free < 0 || defaultKB <= 0 {
		return 0, 0, false
	}
	return free, defaultKB, true
}

// hugepagePageSizeKB 产品页尺寸名 → kB；未知尺寸 ok=false（调用方据此不判）。
func hugepagePageSizeKB(pageSize string) (int, bool) {
	switch pageSize {
	case "1G":
		return 1024 * 1024, true
	case "2M":
		return 2048, true
	}
	return 0, false
}

// hugepagePageSizeMB 产品页尺寸名 → 每页 MB；未知尺寸 0。
func hugepagePageSizeMB(pageSize string) int {
	switch pageSize {
	case "1G":
		return 1024
	case "2M":
		return 2
	}
	return 0
}

// hugepageShortfall 纯函数：判定「空闲页够不够」，不够时给出**可操作**的中文错误。
//
// 输入全部是事实（页尺寸名、每页 MB、需求页数、空闲页数），不做 IO——便于表驱动单测。
// free < 0 或 need <= 0 或未知页尺寸 ⇒ 不判（返回 nil）：不拿不成立的前提去拒合法启动。
func hugepageShortfall(vmName, pageSize string, needPages, freePages int) error {
	perMB := hugepagePageSizeMB(pageSize)
	if perMB <= 0 || needPages <= 0 || freePages < 0 {
		return nil
	}
	if freePages >= needPages {
		return nil
	}
	totalMB := needPages * perMB
	return fmt.Errorf("大页不足：%s 池空闲 %d 页，本 VM 需要 %d 页（每页 %dMB × %d = %dMB）；"+
		"请释放或扩大资源池（set resource-pools hugepages page-size %s count <n>，需 reboot 生效），"+
		"或把该 VM 改到有空闲页的池 / 改用普通内存（memory backing normal）——"+
		"未启动虚拟机 %s（在此之前不会碰 libvirt，不留半成品域）",
		pageSize, freePages, needPages, perMB, needPages, totalMB, pageSize, vmName)
}

// checkHugepagesForStart 启动路径的大页可用量前置判定（BUG-25）。
//
// 落点与另两个前置判定相同：**在进入 api.Start 之前**，命中即立即失败。调用方持 p.mu
// （与 checkConsolePortForStart 一致），但本函数只读 spec 快照与注入的探针，不碰底座。
//
// 保守边界（正常路径与既有语义不变——先判「该不该判」，判不了就不判）：
//   - 未注入探针（单测裸启动、或未装配）⇒ 不判；
//   - 拿不到该 VM 的当前声明（provider 从未收到过它，如直接调用编排的裸启动）⇒ 不判；
//   - backing=normal（普通内存）⇒ 不占大页池，不判；
//   - 页尺寸为空/未知（配置非法，define 期就会以明确错误拒绝）⇒ 不判；
//   - 探针读不到空闲页（ok=false）⇒ **不判**（没有事实就不拒）。
//
// ⚠️ **陈旧 paused 域不判**（调用方在 state==paused 时跳过本判定，见 StartVM/RestartVM）：
// 这类域的**大页已经被它自己持有**（paused 属活动态，内存已分配），此刻读到的空闲页里
// 不含它需要的那几页——按空闲页判会把它自己的占用算成「不够」，从而误拒一次本来能成的
// 重试（决策 #455 的恢复路径正是「销毁陈旧 paused 域后重建」）。销毁后页会回到池里，
// 故这一路交给 libvirt 去尝试；真正不够时它的报错仍会如实冒出来。
func (p *Provider) checkHugepagesForStart(name string) error {
	if p.cfg.HugepageProbe == nil {
		return nil
	}
	spec, ok := p.lookupVMSpec(name)
	if !ok {
		return nil
	}
	vm := spec.vm
	if !useHugepages(vm) {
		return nil
	}
	pageSize := spec.alloc.HugepageSize
	perMB := hugepagePageSizeMB(pageSize)
	if perMB <= 0 || vm.Memory.SizeMB <= 0 {
		return nil
	}
	// 需求页数向上取整（与 model 账本的 pagesNeeded 同口径）。
	need := (vm.Memory.SizeMB + perMB - 1) / perMB
	free, known := p.cfg.HugepageProbe(pageSize, need)
	if !known {
		return nil
	}
	return hugepageShortfall(name, pageSize, need, free)
}
