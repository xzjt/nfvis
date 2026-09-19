package system

// 内核启动基线（FR-SYS-014 / FR-CMP-005）：把内核侧的底座优化参数
// （大页、isolcpus、NMI watchdog、THP、IOMMU 等）纳入「配置期望 / 内核基线（cmdline）
// / 运行实际（meminfo 等）」三方对照，并由同一生成器产出 GRUB 片段与 fstab 行
// ——安装器首次应用、CLI 后续调整共用，避免双源。
//
// 所有读取都以 root 为前缀，便于单测用临时目录构造 /proc、/sys 内容（非 Linux 亦安全）。

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// KernelDesired 期望的内核基线（由 committed 配置派生）。
// 数值项的语义：< 0 = 不托管该项（保留现状）；0 = 托管且为 0；> 0 = 期望值。
type KernelDesired struct {
	Hugepages1G   int    // default_hugepagesz=1G hugepagesz=1G hugepages=N
	Hugepages2M   int    // hugepages=N（2M 默认页）
	IsolatedCores string // isolcpus=<list>
	IRQAffinity   string // irqaffinity=<非隔离核>（EnrichDesired 按真机在线核派生；空 = 不写）
	NoHZFull      *bool  // nil = 未探测（按支持处理）；false = 内核无 CONFIG_NO_HZ_FULL，省略 nohz_full/rcu_nocbs
	LowLatency    bool   // 低延迟参数组（显式选择；idle=poll/tsc=reliable 由 EnrichDesired 按是否虚拟化决定）
	NMIWatchdog   *bool  // nil = 不托管（保留现状）
	THP           string // always|madvise|never；空 = 不托管
	IOMMU         string // on|off|pt；空 = 不托管
	TunedProfile  string // 非 cmdline：写入 /etc/nfvis/tuned-profile
	ExtraParams   []string
}

// KernelActual 运行实际（从 /proc、/sys 读取）。
type KernelActual struct {
	Cmdline       []string // /proc/cmdline 原始 token
	Hugepages1G   int      // /proc/meminfo HugePages_Total（1G 页时即为 1G 数量）
	Hugepages1GFr int
	Hugepages2M   int
	NMIWatchdog   *bool
	THP           string
}

// procRoot 读取根（"/" 为真实系统；单测传临时目录）。
func ReadActual(root string) KernelActual {
	a := KernelActual{}
	if b, err := os.ReadFile(join(root, "/proc/cmdline")); err == nil {
		a.Cmdline = strings.Fields(strings.TrimSpace(string(b)))
	}
	mem := map[string]int{}
	if b, err := os.ReadFile(join(root, "/proc/meminfo")); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			f := strings.Fields(ln)
			if len(f) >= 2 && strings.HasSuffix(f[0], ":") {
				if v, err := strconv.Atoi(f[1]); err == nil {
					mem[strings.TrimSuffix(f[0], ":")] = v
				}
			}
		}
	}
	pageSize1G := false
	for _, p := range a.Cmdline {
		if p == "hugepagesz=1G" || p == "default_hugepagesz=1G" {
			pageSize1G = true
		}
	}
	hp := mem["HugePages_Total"]
	if pageSize1G {
		a.Hugepages1G = hp
		a.Hugepages1GFr = mem["HugePages_Free"]
	}
	if b, err := os.ReadFile(join(root, "/proc/sys/kernel/nmi_watchdog")); err == nil {
		v := strings.TrimSpace(string(b)) == "1"
		a.NMIWatchdog = &v
	}
	if b, err := os.ReadFile(join(root, "/sys/kernel/mm/transparent_hugepage/enabled")); err == nil {
		// 形如 "always [madvise] never"：中括号内为当前值
		s := string(b)
		if i := strings.Index(s, "["); i >= 0 {
			if j := strings.Index(s[i:], "]"); j > 0 {
				a.THP = s[i+1 : i+j]
			}
		}
	}
	return a
}

// IsolatedFromCmdline 从 cmdline 提取 isolcpus 值（无则空）。
func IsolatedFromCmdline(cmdline []string) string {
	for _, p := range cmdline {
		if v, ok := strings.CutPrefix(p, "isolcpus="); ok {
			return v
		}
	}
	return ""
}

// ParamValueFromCmdline 从 cmdline 提取 name=value 参数的值（无则空）。
func ParamValueFromCmdline(cmdline []string, name string) string {
	prefix := name + "="
	for _, p := range cmdline {
		if v, ok := strings.CutPrefix(p, prefix); ok {
			return v
		}
	}
	return ""
}

// Compare 返回「配置期望 vs 运行实际」的差异项（空 = 一致）。
// 仅比较已托管的项：大页（1G 页数）、isolcpus、NMI watchdog、THP。
func Compare(d KernelDesired, a KernelActual) []string {
	var out []string
	if d.Hugepages1G > 0 && a.Hugepages1G != d.Hugepages1G {
		out = append(out, fmt.Sprintf("大页 1G：期望 %d，实际 %d（cmdline 启动参数需生效并重启）", d.Hugepages1G, a.Hugepages1G))
	}
	if d.IsolatedCores != "" {
		cur := IsolatedFromCmdline(a.Cmdline)
		if cur != d.IsolatedCores {
			out = append(out, fmt.Sprintf("isolcpus：期望 %q，cmdline 为 %q", d.IsolatedCores, cur))
		}
	}
	if d.NMIWatchdog != nil {
		if a.NMIWatchdog == nil {
			out = append(out, "nmi_watchdog：运行实际不可读，无法比对")
		} else if *a.NMIWatchdog != *d.NMIWatchdog {
			out = append(out, fmt.Sprintf("nmi_watchdog：期望 %v，实际 %v", *d.NMIWatchdog, *a.NMIWatchdog))
		}
	}
	if d.THP != "" && a.THP != "" && a.THP != d.THP {
		out = append(out, fmt.Sprintf("transparent_hugepage：期望 %s，实际 %s", d.THP, a.THP))
	}
	if d.LowLatency && ParamValueFromCmdline(a.Cmdline, "mitigations") != "off" {
		// 低延迟组不逐项对照（与 vendor/irqaffinity 同口径），但要用代表项把
		// 「apply/commit 时的待重启提示」撑起来——否则开了 profile 却提示"无需重启"。
		out = append(out, "低延迟参数组：期望启用（mitigations=off 等），cmdline 未见（需写入 GRUB 并重启）")
	}
	return out
}

// GenerateBaseline 由期望基线生成 GRUB 片段与 fstab 行（安装器与 CLI 共用，
// 保证"首次装机"与"后续调整"同源）。tuned profile 另写文件，不出现在 cmdline。
func GenerateBaseline(d KernelDesired) (grubFragment string, fstabLine string) {
	var params []string
	if d.Hugepages1G > 0 {
		params = append(params, "default_hugepagesz=1G", "hugepagesz=1G", fmt.Sprintf("hugepages=%d", d.Hugepages1G))
	} else if d.Hugepages2M > 0 {
		params = append(params, fmt.Sprintf("hugepages=%d", d.Hugepages2M))
	}
	if d.IsolatedCores != "" {
		params = append(params, "isolcpus="+d.IsolatedCores)
		// nohz_full/rcu_nocbs 依赖内核编入 CONFIG_NO_HZ_FULL（探测见 kernel_enrich.go）：
		// 不支持时省略——写了也只是被内核忽略的白噪音，但不给操作者「已优化」的错觉。
		if d.NoHZFull == nil || *d.NoHZFull {
			params = append(params, "nohz_full="+d.IsolatedCores, "rcu_nocbs="+d.IsolatedCores)
		}
	}
	if d.IRQAffinity != "" {
		// 中断默认亲和到非隔离核（与 isolcpus 成对；补集由 EnrichDesired 按在线核算出）
		params = append(params, "irqaffinity="+d.IRQAffinity)
	}
	if d.LowLatency {
		// 低延迟参数组（显式可选 profile）：机型无关的一半在这里产出；
		// idle=poll/tsc=reliable 与机型强相关，由 EnrichDesired 按真机是否虚拟化补。
		// 每项都有代价（mitigations=off 是安全缓解回退、mce/nosoftlockup 关掉的是排查手段），
		// 故只随显式选择写入，不做默认。
		params = append(params, "mitigations=off", "audit=0", "mce=off", "nosoftlockup", "numa_balancing=disable")
		if d.NMIWatchdog == nil {
			// nmi-watchdog 有独立字段托管；未托管且 profile 开启时按组补上（用户显式设置一律优先）
			params = append(params, "nmi_watchdog=0")
		}
	}
	if d.NMIWatchdog != nil && !*d.NMIWatchdog {
		params = append(params, "nmi_watchdog=0")
	}
	if d.THP != "" {
		params = append(params, "transparent_hugepage="+d.THP)
	}
	if d.IOMMU != "" {
		params = append(params, "iommu="+d.IOMMU)
	}
	params = append(params, d.ExtraParams...)

	var b strings.Builder
	b.WriteString("# 由 NFViS 生成：请勿手工编辑；变更经 CLI set system kernel / resource-pools 后由 nfvisd 重写\n")
	// 必须以「追加到标准变量」的形式写：/etc/default/grub.d/*.cfg 在主文件之后被 source，
	// 而 grub-mkconfig 只采用 GRUB_CMDLINE_LINUX(_DEFAULT)，自定义变量不会生效。
	if len(params) > 0 {
		b.WriteString(`GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX} ` + strings.Join(params, " ") + "\"\n")
	}
	if d.TunedProfile != "" {
		b.WriteString("# tuned profile: " + d.TunedProfile + "\n")
	}
	fstab := ""
	if d.Hugepages1G > 0 {
		fstab = "nodev /dev/hugepages hugetlbfs defaults,pagesize=1G 0 0"
	} else if d.Hugepages2M > 0 {
		fstab = "nodev /dev/hugepages hugetlbfs defaults,pagesize=2M 0 0"
	}
	return b.String(), fstab
}

// DesiredFromConfig 由 committed 配置派生期望基线（resource-pools 为唯一真源）。
func DesiredFromConfig(hpPageSize string, hpCount int, isolated string, nmi, thp, iommu, tuned string, extra []string) KernelDesired {
	d := KernelDesired{Hugepages1G: -1, IsolatedCores: isolated}
	switch hpPageSize {
	case "1G":
		if hpCount > 0 {
			d.Hugepages1G = hpCount
		} else {
			d.Hugepages1G = 0
		}
	case "2M":
		d.Hugepages2M = hpCount
	}
	if nmi != "" {
		v := nmi == "true" || nmi == "on" || nmi == "1"
		d.NMIWatchdog = &v
	}
	d.THP, d.IOMMU, d.TunedProfile = thp, iommu, tuned
	d.ExtraParams = append([]string{}, extra...)
	sort.Strings(d.ExtraParams)
	return d
}

func join(root, p string) string {
	if root == "" || root == "/" {
		return p
	}
	return strings.TrimSuffix(root, "/") + p
}
