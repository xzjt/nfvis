package api

// 由 committed 配置派生「内核启动基线期望」（FR-SYS-014）：
//   resource-pools 是大页与隔离核的唯一真源；system kernel 承载其余启动参数
//   （nmi-watchdog / transparent-hugepages / iommu / tuned-profile / params）。
// 派生结果供 show system kernel 的三方对照与 GRUB 基线生成（安装器同源）使用。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	ksys "github.com/xzjt/nfvis/internal/system"
)

func (x *cliExecutor) desiredKernelBaseline() (ksys.KernelDesired, error) {
	cfg, err := x.engine.Committed()
	if err != nil {
		return ksys.KernelDesired{}, err
	}
	var pageSize string
	count := 0
	if cfg.ResourcePools != nil {
		for _, hp := range cfg.ResourcePools.Hugepages {
			// 只托管一种页大小：优先 1G（vhost-user 场景），否则 2M
			if hp.PageSize == "1G" {
				pageSize, count = "1G", hp.Count
				break
			}
			if pageSize == "" {
				pageSize, count = hp.PageSize, hp.Count
			}
		}
	}
	isolated := ""
	if cfg.ResourcePools != nil && cfg.ResourcePools.CPU != nil && len(cfg.ResourcePools.CPU.IsolatedCores) > 0 {
		isolated = coreListText(cfg.ResourcePools.CPU.IsolatedCores)
	}
	nmi, thp, iommu, tuned := "", "", "", ""
	lowLatency := false
	var extra []string
	if sys := cfg.System; sys != nil && sys.Kernel != nil {
		if sys.Kernel.NMIWatchdog != nil {
			nmi = strconv.FormatBool(*sys.Kernel.NMIWatchdog)
		}
		thp, iommu, tuned = sys.Kernel.TransparentHugepages, sys.Kernel.IOMMU, sys.Kernel.TunedProfile
		lowLatency = sys.Kernel.LowLatency
		extra = sys.Kernel.Params
	}
	d := ksys.DesiredFromConfig(pageSize, count, isolated, nmi, thp, iommu, tuned, extra)
	d.LowLatency = lowLatency
	return d, nil
}

// coreListText 把核号列表压成紧凑区间文本（4,5,6,7,9 → "4-7,9"）。
func coreListText(cores []int) string {
	if len(cores) == 0 {
		return ""
	}
	var parts []string
	start, prev := cores[0], cores[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
			return
		}
		parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
	}
	for _, c := range cores[1:] {
		if c == prev+1 {
			prev = c
			continue
		}
		flush()
		start, prev = c, c
	}
	flush()
	return strings.Join(parts, ",")
}

// kernelBaselineWarnings 在 commit 后给出「配置期望 vs 运行实际」的差异提示
// （FR-CMP-005：资源池调整时联动内核参数并明确指引）。
func (x *cliExecutor) kernelBaselineWarnings() []string {
	desired, err := x.desiredKernelBaseline()
	if err != nil {
		return nil
	}
	actual := ksys.ReadActual("/")
	diffs := ksys.Compare(desired, actual)
	if len(diffs) == 0 {
		return nil
	}
	out := []string{"警告: 内核启动基线与配置不一致，需写入 GRUB 并重启生效："}
	for _, d := range diffs {
		out = append(out, "  - "+d)
	}
	out = append(out, "  处理：request system kernel apply（写入基线）→ request system reboot")
	return out
}

// 编译期保持 context 引用（后续 apply/rollback 使用），避免未使用导入。
var _ = context.Background

// requestKernelBaseline：request system kernel apply|rollback（FR-SYS-014）。
//
// apply：由 committed 配置派生期望基线 → 写 GRUB 片段/fstab/tuned → update-grub；
//
//	生效需重启，故输出明确的 pending_reboot 提示（show system kernel 可查一致性）。
//
// rollback：恢复上一次片段（或删除片段），同样需重启生效。
func (x *cliExecutor) requestKernelBaseline(user string, args []string) string {
	if x.kernel == nil {
		return "%% 内核基线落地不可用（编排器未装配）\n"
	}
	if len(args) == 0 {
		return "%% 语法: request system kernel apply|rollback\n"
	}
	switch args[0] {
	case "apply":
		desired, err := x.desiredKernelBaseline()
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		actual := ksys.ReadActual("/")
		backup, err := x.kernel.Apply(desired)
		if err != nil {
			return "%% 写入内核基线失败: " + err.Error() + "\n"
		}
		var b strings.Builder
		b.WriteString("内核基线已写入（GRUB 片段 /etc/default/grub.d/99-nfvis.cfg + fstab 大页挂载）\n")
		if backup != "" {
			b.WriteString("上一版本已备份：" + backup + "（request system kernel rollback 可回退）\n")
		}
		if diffs := ksys.Compare(desired, actual); len(diffs) > 0 {
			b.WriteString("待重启生效（pending_reboot）：\n")
			for _, d := range diffs {
				b.WriteString("  - " + d + "\n")
			}
			b.WriteString("执行 request system reboot 应用新基线；重启后 show system kernel 应显示一致\n")
		} else {
			b.WriteString("当前运行实际已与期望一致（无需重启）\n")
		}
		return b.String()
	case "rollback":
		msg, err := x.kernel.Rollback()
		if err != nil {
			return "%% 回退内核基线失败: " + err.Error() + "\n"
		}
		return msg + "；需重启生效（request system reboot）\n"
	}
	return fmt.Sprintf("%% 无效命令: request system kernel %s（可用：apply|rollback）\n", strings.Join(args, " "))
}
