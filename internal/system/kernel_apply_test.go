package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaselineApplyRollback(t *testing.T) {
	root := t.TempDir()
	calls := 0
	a := &BaselineApplier{Root: root, Runner: func(string, ...string) error { calls++; return nil }}

	off := false
	d := KernelDesired{Hugepages1G: 8, IsolatedCores: "4-15", NMIWatchdog: &off, THP: "never", TunedProfile: "nfvis-throughput"}
	backup, err := a.Apply(d)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if backup != "" {
		t.Fatalf("首次应用应无备份: %q", backup)
	}
	frag, err := os.ReadFile(filepath.Join(root, "etc/default/grub.d/99-nfvis.cfg"))
	if err != nil {
		t.Fatalf("片段未写入: %v", err)
	}
	for _, want := range []string{"hugepages=8", "isolcpus=4-15", "nmi_watchdog=0", "transparent_hugepage=never"} {
		if !strings.Contains(string(frag), want) {
			t.Fatalf("片段缺少 %q:\n%s", want, frag)
		}
	}
	fstab, _ := os.ReadFile(filepath.Join(root, "etc/fstab"))
	if !strings.Contains(string(fstab), "pagesize=1G") || !strings.Contains(string(fstab), fstabMarker) {
		t.Fatalf("fstab 未维护大页行: %s", fstab)
	}
	tuned, _ := os.ReadFile(filepath.Join(root, "var/lib/nfvis/tuned-profile"))
	if strings.TrimSpace(string(tuned)) != "nfvis-throughput" {
		t.Fatalf("tuned profile 未写入: %q", tuned)
	}
	if calls != 1 {
		t.Fatalf("应调用一次 update-grub，实际 %d", calls)
	}

	// 第二次应用：应产生备份（内容为上一次片段）
	backup, err = a.Apply(KernelDesired{Hugepages1G: 4, IsolatedCores: "4-7"})
	if err != nil {
		t.Fatalf("二次 Apply: %v", err)
	}
	if backup == "" {
		t.Fatal("二次应用应产生备份")
	}
	bak, _ := os.ReadFile(filepath.Join(root, "var/lib/nfvis/kernel-baseline.bak"))
	if !strings.Contains(string(bak), "hugepages=8") {
		t.Fatalf("备份内容应为上一版: %s", bak)
	}

	// 回退：片段应恢复到上一版
	if _, err := a.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	frag2, _ := os.ReadFile(filepath.Join(root, "etc/default/grub.d/99-nfvis.cfg"))
	if !strings.Contains(string(frag2), "hugepages=8") {
		t.Fatalf("回退后片段应恢复: %s", frag2)
	}
}

func TestBaselineApplyUpdateGrubFailureRollsBackFragment(t *testing.T) {
	root := t.TempDir()
	a := &BaselineApplier{Root: root, Runner: func(string, ...string) error { return os.ErrPermission }}
	if _, err := a.Apply(KernelDesired{Hugepages1G: 8}); err == nil {
		t.Fatal("update-grub 失败应报错")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/default/grub.d/99-nfvis.cfg")); !os.IsNotExist(err) {
		t.Fatal("update-grub 失败时应回退（删除）片段")
	}
}

func TestBaselineFstabIdempotent(t *testing.T) {
	root := t.TempDir()
	a := &BaselineApplier{Root: root, Runner: func(string, ...string) error { return nil }}
	for i := 0; i < 3; i++ {
		if _, err := a.Apply(KernelDesired{Hugepages1G: 4}); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}
	fstab, _ := os.ReadFile(filepath.Join(root, "etc/fstab"))
	if n := strings.Count(string(fstab), fstabMarker); n != 1 {
		t.Fatalf("fstab 大页行应幂等（1 条），实际 %d 条:\n%s", n, fstab)
	}
}

func TestStripLegacyGrubParams(t *testing.T) {
	root := t.TempDir()
	a := &BaselineApplier{Root: root, Runner: func(string, ...string) error { return nil }}
	dir := filepath.Join(root, "etc/default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	grub := "GRUB_TIMEOUT=5\nGRUB_CMDLINE_LINUX=\"quiet splash default_hugepagesz=1G hugepagesz=1G hugepages=4 isolcpus=4-15\"\n"
	if err := os.WriteFile(filepath.Join(dir, "grub"), []byte(grub), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(KernelDesired{Hugepages1G: 4, IsolatedCores: "4-15"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cleaned, _ := os.ReadFile(filepath.Join(dir, "grub"))
	if strings.Contains(string(cleaned), "default_hugepagesz") || strings.Contains(string(cleaned), "isolcpus") {
		t.Fatalf("主 grub 文件应已摘除 nfvis 参数: %s", cleaned)
	}
	if !strings.Contains(string(cleaned), "quiet splash") {
		t.Fatalf("其它参数应保留: %s", cleaned)
	}
	if _, err := os.Stat(filepath.Join(dir, "grub.nfvis-bak")); err != nil {
		t.Fatal("应保留 /etc/default/grub.nfvis-bak 备份")
	}
}

// R88-1 / 决策 #347：大页池 sysctl 片段钉**当前内核默认尺寸池**的声明值。
//
// 真机 round88 现场（1.1.49，干净快照首装）：VPP 包自带的 /etc/sysctl.d/80-vpp.conf
// （vm.nr_hugepages=1024，本意给 2M 池）落到**默认尺寸**池上，开机被撑到 nr=4——
// `show system kernel` 里「内核基线 1 / 运行实际 4」长期不一致。`vm.nr_hugepages` 只作用于
// 默认尺寸池，而默认尺寸由当前内核 cmdline 的 default_hugepagesz 决定（#347 后产品基线恒 2M，
// 但旧基线尚未重启时仍是 1G）——故判据取自 d.DefaultHugepageSize，不硬编码。
func TestGenerateHugepageSysctlPinsDeclaredCount(t *testing.T) {
	// ① 旧基线（默认尺寸 1G）：钉 1G 池声明值，不写 2M 池数
	got := GenerateHugepageSysctl(KernelDesired{DefaultHugepageSize: "1G", Hugepages1G: 4, Hugepages2M: 768})
	if !strings.Contains(got, "vm.nr_hugepages = 4") {
		t.Fatalf("旧基线应钉 1G 声明值 4：%q", got)
	}
	if strings.Contains(got, "vm.nr_hugepages = 768") {
		t.Fatalf("不得把 2M 池数写给 1G 默认尺寸池：%q", got)
	}
	// 接管 vpp 包那条 conffile 时必须把它另一个生效键一起带上（决策 #201），
	// 否则挪走文件就顺手丢了「root 组可访问大页」。
	if !strings.Contains(got, "vm.hugetlb_shm_group = 0") {
		t.Fatalf("应接管 vpp 的 vm.hugetlb_shm_group：%q", got)
	}
	// ② 新基线（默认尺寸 2M）：钉 2M 池声明值，不写 1G 池数
	got = GenerateHugepageSysctl(KernelDesired{DefaultHugepageSize: "2M", Hugepages1G: 4, Hugepages2M: 768})
	if !strings.Contains(got, "vm.nr_hugepages = 768") {
		t.Fatalf("新基线应钉 2M 声明值 768：%q", got)
	}
	if strings.Contains(got, "vm.nr_hugepages = 4") {
		t.Fatalf("不得把 1G 池数写给 2M 默认尺寸池：%q", got)
	}
	// 单 2M 池、新基线：钉 2M
	if got := GenerateHugepageSysctl(KernelDesired{DefaultHugepageSize: "2M", Hugepages2M: 768}); !strings.Contains(got, "vm.nr_hugepages = 768") {
		t.Fatalf("单 2M 池应钉 2M 声明值：%q", got)
	}
	// ③ 取不到默认尺寸（DefaultHugepageSize 空）：不产出（调用方据此不动文件）
	if got := GenerateHugepageSysctl(KernelDesired{Hugepages1G: 4, Hugepages2M: 768}); got != "" {
		t.Fatalf("默认尺寸未知时不应产出：%q", got)
	}
	// ④ 旧基线且未声明 1G 池：不产出（调用方删文件）
	if got := GenerateHugepageSysctl(KernelDesired{DefaultHugepageSize: "1G", Hugepages2M: 768}); got != "" {
		t.Fatalf("旧基线未声明 1G 池时不应产出：%q", got)
	}
	// 该尺寸池未声明（==0）：不产出
	if got := GenerateHugepageSysctl(KernelDesired{DefaultHugepageSize: "2M"}); got != "" {
		t.Fatalf("该尺寸池未声明时不应产出：%q", got)
	}
}

// R88-1 / 决策 #347：Apply 落盘 sysctl 片段、Rollback 撤除；启动期按 cmdline 补写（首装路径），
// 判据跟随当前内核默认尺寸池（旧基线 default 1G 钉 1G、新基线 default 2M 钉 2M）。
func TestEnsureHugepageSysctlFromCmdline(t *testing.T) {
	root := t.TempDir()

	// 旧基线（default 1G）：钉 1G 池声明值 4，不写 2M 池数
	writeProc(t, root, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro default_hugepagesz=1G hugepagesz=1G hugepages=4 hugepagesz=2M hugepages=768 intel_iommu=on\n")
	changed, err := EnsureHugepageSysctlFromCmdline(root)
	if err != nil || !changed {
		t.Fatalf("旧基线首次应写入（changed=%v err=%v）", changed, err)
	}
	b, err := os.ReadFile(root + hugepageSysctlRel)
	if err != nil {
		t.Fatalf("读取 sysctl 片段: %v", err)
	}
	if !strings.Contains(string(b), "vm.nr_hugepages = 4") || strings.Contains(string(b), "vm.nr_hugepages = 768") {
		t.Fatalf("旧基线应钉 1G 声明值 4：%q", b)
	}
	// 幂等：内容未变不再报告变更
	if changed, err := EnsureHugepageSysctlFromCmdline(root); err != nil || changed {
		t.Fatalf("重复调用不应再变更（changed=%v err=%v）", changed, err)
	}

	// 切到新基线（default 2M）：重写为 2M 池声明值 768
	writeProc(t, root, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro default_hugepagesz=2M hugepagesz=2M hugepages=768 hugepagesz=1G hugepages=4 intel_iommu=on\n")
	if changed, err := EnsureHugepageSysctlFromCmdline(root); err != nil || !changed {
		t.Fatalf("切新基线应重写（changed=%v err=%v）", changed, err)
	}
	b, _ = os.ReadFile(root + hugepageSysctlRel)
	if !strings.Contains(string(b), "vm.nr_hugepages = 768") {
		t.Fatalf("新基线应钉 2M 声明值 768：%q", b)
	}

	// 默认尺寸已知但该尺寸池未声明 → 撤除片段
	writeProc(t, root, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro default_hugepagesz=2M intel_iommu=on\n")
	if changed, err := EnsureHugepageSysctlFromCmdline(root); err != nil || !changed {
		t.Fatalf("该尺寸池未声明时应撤除（changed=%v err=%v）", changed, err)
	}
	if _, err := os.Stat(root + hugepageSysctlRel); !os.IsNotExist(err) {
		t.Fatalf("sysctl 片段应已删除: %v", err)
	}
}

// 决策 #347：取不到内核默认大页尺寸时**保守不动**该文件（不写不删），不猜。
func TestEnsureHugepageSysctlFromCmdlineUnknownDefaultNoTouch(t *testing.T) {
	root := t.TempDir()
	// 预置一份既有片段，期望它被原样保留
	writeProc(t, root, hugepageSysctlRel, "# 既有内容\nvm.nr_hugepages = 1\n")
	// cmdline 无 default_hugepagesz：取不到默认尺寸
	writeProc(t, root, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro hugepages=768 quiet\n")

	changed, err := EnsureHugepageSysctlFromCmdline(root)
	if err != nil {
		t.Fatalf("取不到默认尺寸不应报错: %v", err)
	}
	if changed {
		t.Fatal("取不到默认尺寸不应报告变更（保守不动）")
	}
	b, err := os.ReadFile(root + hugepageSysctlRel)
	if err != nil {
		t.Fatalf("既有片段不应被删除: %v", err)
	}
	if string(b) != "# 既有内容\nvm.nr_hugepages = 1\n" {
		t.Fatalf("既有片段不应被改写: %q", b)
	}
}
