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
	"strings"
)

const (
	grubFragmentRel = "/etc/default/grub.d/99-nfvis.cfg"
	grubBackupRel   = "/var/lib/nfvis/kernel-baseline.bak"
	fstabRel        = "/etc/fstab"
	fstabMarker     = "# nfvis-hugepages"
	tunedRel        = "/var/lib/nfvis/tuned-profile"
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
