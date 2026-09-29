package system

// 内核基线的落地与回退（FR-SYS-014 / 决策 #66）。
//
// 写入目标（幂等、可回退）：
//   /etc/default/grub.d/99-nfvis.cfg   —— GRUB 片段（独立文件，不动主文件）
//   /etc/fstab                         —— 大页挂载行（按注释标记替换）
//   /var/lib/nfvis/tuned-profile       —— tuned 性能档（非 cmdline）
//   /var/lib/nfvis/kernel-baseline.bak —— 上一次片段备份（rollback 用）
//
// Root 可注入：单测用临时目录，不触碰宿主。update-grub 通过 Runner 注入（测试不执行）。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	grubFragmentRel = "/etc/default/grub.d/99-nfvis.cfg"
	grubBackupRel   = "/var/lib/nfvis/kernel-baseline.bak"
	fstabRel        = "/etc/fstab"
	fstabMarker     = "# nfvis-hugepages"
	tunedRel        = "/var/lib/nfvis/tuned-profile"
	// 大页池 sysctl 落点（R88-1）。序号 **大于** VPP 包自带的 /etc/sysctl.d/80-vpp.conf，
	// systemd-sysctl 按文件名序执行，故本文件后执行、最后生效。
	hugepageSysctlRel = "/etc/sysctl.d/90-nfvis-hugepages.conf"
)

// KernelApplier 内核基线落地能力（CLI/API 注入；实现见 BaselineApplier）。
type KernelApplier interface {
	Apply(d KernelDesired) (backupPath string, err error)
	Rollback() (message string, err error)
}

// BaselineApplier 默认实现：生成基线 → 写片段/fstab/tuned → update-grub。
type BaselineApplier struct {
	Root   string                                  // 配置根（默认 "/"）
	Runner func(name string, args ...string) error // update-grub 执行（nil = 真实执行）
}

// NewBaselineApplier 构造真实系统上的落地器。
func NewBaselineApplier() *BaselineApplier { return &BaselineApplier{Root: "/"} }

func (a *BaselineApplier) path(rel string) string {
	if a.Root == "" || a.Root == "/" {
		return rel
	}
	return filepath.Join(a.Root, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
}

// Apply 写入基线；返回备份路径（空表示此前无片段）。update-grub 失败时保留已写片段
// 并回滚片段内容，避免半成品基线留在系统里。
// 写入前先 ValidateDesired（护栏，两条写路径共用）再 EnrichDesired（真机补全）——
// 保证经 CLI 写出的片段与安装器产出同源。
func (a *BaselineApplier) Apply(d KernelDesired) (string, error) {
	if err := ValidateDesired(d, a.Root); err != nil {
		return "", err
	}
	frag, fstabLine := GenerateBaseline(EnrichDesired(d, a.Root))
	fragPath := a.path(grubFragmentRel)
	backup := a.path(grubBackupRel)

	if err := os.MkdirAll(filepath.Dir(fragPath), 0o755); err != nil {
		return "", fmt.Errorf("创建 GRUB 片段目录: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return "", fmt.Errorf("创建状态目录: %w", err)
	}
	prev, prevErr := os.ReadFile(fragPath)
	hadPrev := prevErr == nil
	if hadPrev {
		if err := os.WriteFile(backup, prev, 0o644); err != nil {
			return "", fmt.Errorf("备份现有片段: %w", err)
		}
	}
	// 一次性迁移：主 grub 文件里若有 nfvis 托管的参数（老装机方式写入），先摘除，
	// 避免片段与主文件重复注入同一参数（与 deploy/installer/nfvis-baseline.sh 同行为）。
	if err := a.stripLegacyGrubParams(); err != nil {
		return "", err
	}
	if err := os.WriteFile(fragPath, []byte(frag), 0o644); err != nil {
		return "", fmt.Errorf("写入 GRUB 片段: %w", err)
	}
	if err := a.setFstabLine(fstabLine); err != nil {
		return "", err
	}
	// 大页池 sysctl 落点：把 vm.nr_hugepages 钉回本次基线声明的页数（R88-1，见函数注释）。
	if err := a.ensureHugepageSysctl(d); err != nil {
		return "", err
	}
	if d.TunedProfile != "" {
		if err := writeFile(a.path(tunedRel), d.TunedProfile+"\n"); err != nil {
			return "", err
		}
	}
	if err := a.updateGrub(); err != nil {
		// 回退片段，保持系统与配置一致（不留下未生效的 GRUB 片段）
		if hadPrev {
			_ = os.WriteFile(fragPath, prev, 0o644)
		} else {
			_ = os.Remove(fragPath)
		}
		return "", fmt.Errorf("update-grub 失败（已回退片段）: %w", err)
	}
	if hadPrev {
		return backup, nil
	}
	return "", nil
}

// Rollback 恢复上一次片段；无备份则删除片段（回到系统原始状态）。
func (a *BaselineApplier) Rollback() (string, error) {
	fragPath := a.path(grubFragmentRel)
	backup := a.path(grubBackupRel)
	// 大页池 sysctl 片段同属本基线产物：回退时一并撤除（否则它会继续把池钉在旧声明值上）。
	if err := os.Remove(a.path(hugepageSysctlRel)); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("删除大页池 sysctl 片段: %w", err)
	}
	prev, err := os.ReadFile(backup)
	if err != nil {
		if rmErr := os.Remove(fragPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return "", fmt.Errorf("删除 GRUB 片段: %w", rmErr)
		}
		if err := a.updateGrub(); err != nil {
			return "", fmt.Errorf("update-grub 失败: %w", err)
		}
		return "已删除 NFViS 内核基线片段（无历史备份）", nil
	}
	if err := os.WriteFile(fragPath, prev, 0o644); err != nil {
		return "", fmt.Errorf("恢复 GRUB 片段: %w", err)
	}
	if err := a.updateGrub(); err != nil {
		return "", fmt.Errorf("update-grub 失败: %w", err)
	}
	return "已回退到上一次内核基线（备份 " + grubBackupRel + "）", nil
}

// setFstabLine 幂等维护大页挂载行（按注释标记整行替换；空行则移除）。
func (a *BaselineApplier) setFstabLine(line string) error {
	p := a.path(fstabRel)
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取 fstab: %w", err)
	}
	var kept []string
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(l), fstabMarker) || strings.Contains(l, "/dev/hugepages") {
			continue
		}
		kept = append(kept, l)
	}
	if line != "" {
		kept = append(kept, fstabMarker, line)
	}
	return writeFile(p, strings.Join(kept, "\n")+"\n")
}

// GenerateHugepageSysctl 产出「把默认尺寸大页池钉回声明值」的 sysctl 片段内容。
//
// 为什么需要（R88-1，真机 round88 定位）：VPP 的 deb 装了 /etc/sysctl.d/80-vpp.conf
// （`vm.nr_hugepages=1024`，注释写明是给 **2M** 池留的），而 `vm.nr_hugepages` 只作用于
// **默认尺寸**池。产品内核基线一旦设了 `default_hugepagesz=1G`（1G 池 > 0 时必设，见
// GenerateBaseline），这条 sysctl 就落到 **1G** 池上：开机时 systemd-sysctl 按可用内存
// 尽量分配，1G 池因此**大于**内核基线声明的页数（真机现场：cmdline `hugepages=1`，
// 运行实际 nr=4）。此前被记作「1G 池无主占用，未做回收」，机制其实在这里。
//
// 本文件按同一规则写回声明值：序号 90 > 80 ⇒ 后执行者生效，多余的空闲页随之释放。
// 默认尺寸判据与 GenerateBaseline 完全同源：1G 池 > 0 时才写 default_hugepagesz=1G。
// GenerateHugepageSysctl 产出大页池 sysctl 片段内容——**唯一真源**（决策 #199 引入、#201 收口）。
//
// 为什么需要（真机 round88 定位）：VPP 的 deb 装了 /etc/sysctl.d/80-vpp.conf
// （`vm.nr_hugepages=1024`，注释写明是给 **2M** 池留的），而 `vm.nr_hugepages` 只作用于
// **默认尺寸**池。产品内核基线一旦设了 `default_hugepagesz=1G`（1G 池 > 0 时必设，见
// GenerateBaseline），这条 sysctl 就落到 **1G** 池上：开机时 systemd-sysctl 按可用内存
// 尽量分配，1G 池因此**大于**内核基线声明的页数（真机现场：cmdline `hugepages=1`，
// 运行实际 nr=4）。此前被记作「1G 池无主占用，未做回收」，机制其实在这里。
//
// 单一事实源：安装期由 postinst 用 dpkg-divert 把 vpp 那个 conffile 挪到
// `<同名>.vpp-disabled`（决策 #201），此后 `vm.nr_hugepages` 只由本文件声明——
// 不再依赖「90 号文件名序在 80 号之后」的排序约定，也没有开机期「先撑大再回缩」的抖动。
// vpp 原文件里另一个生效键 `vm.hugetlb_shm_group=0`（root 组可访问大页）由本文件接管保持原值，
// 免得挪走文件顺手丢掉它。回退内核基线时本文件一并撤除（见 Rollback）。
//
// 默认尺寸判据与 GenerateBaseline 完全同源：1G 池 > 0 时才写 default_hugepagesz=1G。
func GenerateHugepageSysctl(d KernelDesired) string {
	n, size := 0, ""
	switch {
	case d.Hugepages1G > 0:
		n, size = d.Hugepages1G, "1G"
	case d.Hugepages2M > 0:
		n, size = d.Hugepages2M, "2M"
	default:
		return ""
	}
	return "# 由 NFViS 生成：大页池 sysctl 的**唯一真源**\n" +
		"# vpp 包自带的 /etc/sysctl.d/80-vpp.conf 已由 dpkg-divert 挪到 .vpp-disabled\n" +
		"# （它的 vm.nr_hugepages=1024 本意给 2M 池，而这枚 sysctl 只作用于**默认尺寸**池；\n" +
		"#   产品基线设了 default_hugepagesz=1G 时它会落到 1G 池上、把池撑过声明值）\n" +
		fmt.Sprintf("# 默认页尺寸 %s，声明 %d 页；hugetlb_shm_group 沿用 vpp 包原值\n", size, n) +
		fmt.Sprintf("vm.nr_hugepages = %d\n", n) +
		"vm.hugetlb_shm_group = 0\n"
}

// ensureHugepageSysctl 落盘大页池 sysctl 片段（幂等；无声明时删除该文件）。
func (a *BaselineApplier) ensureHugepageSysctl(d KernelDesired) error {
	p := a.path(hugepageSysctlRel)
	content := GenerateHugepageSysctl(d)
	if content == "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除大页池 sysctl 片段: %w", err)
		}
		return nil
	}
	prev, err := os.ReadFile(p)
	if err == nil && string(prev) == content {
		return nil // 内容未变：不重写（保持 mtime，避免无意义的改动）
	}
	return writeFile(p, content)
}

func (a *BaselineApplier) updateGrub() error {
	if a.Runner != nil {
		return a.Runner("update-grub")
	}
	out, err := exec.Command("update-grub").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeFile(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", p, err)
	}
	return nil
}

// stripLegacyGrubParams 从 /etc/default/grub 摘除 nfvis 托管的启动参数（一次性迁移）。
// 仅当存在时才改写，并保留 /etc/default/grub.nfvis-bak 备份；文件不存在则跳过。
func (a *BaselineApplier) stripLegacyGrubParams() error {
	p := a.path("/etc/default/grub")
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s: %w", p, err)
	}
	content := string(raw)
	if !legacyParamRe.MatchString(content) {
		return nil
	}
	if err := os.WriteFile(a.path("/etc/default/grub.nfvis-bak"), raw, 0o644); err != nil {
		return fmt.Errorf("备份 /etc/default/grub: %w", err)
	}
	cleaned := legacyParamRe.ReplaceAllString(content, "")
	cleaned = strings.ReplaceAll(cleaned, "  ", " ")
	if err := os.WriteFile(p, []byte(cleaned), 0o644); err != nil {
		return fmt.Errorf("改写 %s: %w", p, err)
	}
	return nil
}

// legacyParamRe 匹配 nfvis 托管的启动参数（含前导空格）。
// 覆盖 GenerateBaseline 可能产出的全部参数名：旧片段/主 grub 里若残留同名参数，
// 不摘除会与片段重复注入（cmdline 同名参数以最后一个为准）。
var legacyParamRe = regexp.MustCompile(` ?(default_hugepagesz|hugepagesz|hugepages|isolcpus|nohz_full|rcu_nocbs|irqaffinity|nmi_watchdog|transparent_hugepage|iommu|intel_iommu|amd_iommu|intel_pstate|amd_pstate)=[^ "]*`)

// EnsureHugepageSysctlFromCmdline 按**当前内核基线声明**（/proc/cmdline）落盘大页池
// sysctl 片段，返回是否发生了变更。
//
// 启动时调用（nfvisd 单源保证，与决策 #182 的 AppArmor 放行同一思路）：安装期由
// postinst → nfvis-baseline.sh 直接写 GRUB 片段，不经过 BaselineApplier.Apply，因此
// 只靠 Apply 挂 sysctl 会漏掉「首装即被 80-vpp.conf 撑大」这条路径。cmdline 是那次
// 基线真正生效的声明值，正是要钉住的数。
func EnsureHugepageSysctlFromCmdline(root string) (bool, error) {
	raw, err := os.ReadFile(join(root, "/proc/cmdline"))
	if err != nil {
		return false, err
	}
	args := strings.Fields(string(raw))
	var d KernelDesired
	if v := HugepageFromCmdline(args, "1G"); v != "" {
		d.Hugepages1G, _ = strconv.Atoi(v)
	}
	if v := HugepageFromCmdline(args, "2M"); v != "" {
		d.Hugepages2M, _ = strconv.Atoi(v)
	}
	p := join(root, hugepageSysctlRel)
	content := GenerateHugepageSysctl(d)
	prev, readErr := os.ReadFile(p)
	if content == "" {
		if readErr == nil {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return false, fmt.Errorf("删除大页池 sysctl 片段: %w", err)
			}
			return true, nil
		}
		return false, nil
	}
	if readErr == nil && string(prev) == content {
		return false, nil
	}
	if root == "" || root == "/" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return false, fmt.Errorf("写入 %s: %w", p, err)
		}
		return true, nil
	}
	// 注入 root（单测）：root 下不一定有 /etc/sysctl.d，MkdirAll 会建出来。
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("写入 %s: %w", p, err)
	}
	return true, nil
}
