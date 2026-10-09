package system

import (
	"context"
	"os"
	"os/exec"
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

// 决策 #347：默认尺寸判据取**即将生效的内核基线**——优先产品写的 GRUB 片段，回退当前 cmdline。
func TestEffectiveDefaultHugepageSize(t *testing.T) {
	// 片段存在且含 default_hugepagesz：以片段为准（apply 后重启前的关键形态：片段=2M、cmdline=1G）
	root := t.TempDir()
	writeProc(t, root, grubFragmentRel, "GRUB_CMDLINE_LINUX=\"${GRUB_CMDLINE_LINUX} default_hugepagesz=2M hugepagesz=2M hugepages=768 hugepagesz=1G hugepages=2\"\n")
	writeProc(t, root, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro default_hugepagesz=1G hugepagesz=1G hugepages=2 hugepagesz=2M hugepages=768\n")
	if got := EffectiveDefaultHugepageSize(root); got != "2M" {
		t.Fatalf("有片段时应以片段为准（2M），实际 %q", got)
	}
	// 片段 default=1G、cmdline=2M：仍以片段为准
	writeProc(t, root, grubFragmentRel, "GRUB_CMDLINE_LINUX=\"${GRUB_CMDLINE_LINUX} default_hugepagesz=1G hugepagesz=1G hugepages=2\"\n")
	if got := EffectiveDefaultHugepageSize(root); got != "1G" {
		t.Fatalf("有片段时应以片段为准（1G），实际 %q", got)
	}
	// 片段不存在：回退 cmdline
	root2 := t.TempDir()
	writeProc(t, root2, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro default_hugepagesz=1G hugepagesz=1G hugepages=4\n")
	if got := EffectiveDefaultHugepageSize(root2); got != "1G" {
		t.Fatalf("无片段时应回退 cmdline（1G），实际 %q", got)
	}
	// 片段存在但不含 default_hugepagesz：回退 cmdline
	writeProc(t, root2, grubFragmentRel, "GRUB_TIMEOUT=5\n")
	if got := EffectiveDefaultHugepageSize(root2); got != "1G" {
		t.Fatalf("片段无 default 时应回退 cmdline（1G），实际 %q", got)
	}
	// 两者都取不到：空
	root3 := t.TempDir()
	writeProc(t, root3, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro quiet\n")
	if got := EffectiveDefaultHugepageSize(root3); got != "" {
		t.Fatalf("都取不到应为空，实际 %q", got)
	}
}

// 决策 #347 关键回归（真机 round127 事故形态）：GRUB 片段（即将生效的新基线）=default 2M，
// 而当前运行 cmdline=default 1G 时，须按 **2M 池**的声明值钉——不能按运行 cmdline 的 1G 池取值
// （后者会把旧尺寸池的值写到重启后已是 2M 的默认尺寸池上，把 2M 池收小、VPP 起不来）。
func TestEnsureHugepageSysctlFromCmdlinePrefersGrubFragment(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, grubFragmentRel, "GRUB_CMDLINE_LINUX=\"${GRUB_CMDLINE_LINUX} default_hugepagesz=2M hugepagesz=2M hugepages=768 hugepagesz=1G hugepages=2\"\n")
	writeProc(t, root, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz ro default_hugepagesz=1G hugepagesz=1G hugepages=2 hugepagesz=2M hugepages=768\n")

	changed, err := EnsureHugepageSysctlFromCmdline(root)
	if err != nil || !changed {
		t.Fatalf("应写入（changed=%v err=%v）", changed, err)
	}
	b, err := os.ReadFile(root + hugepageSysctlRel)
	if err != nil {
		t.Fatalf("读取 sysctl 片段: %v", err)
	}
	if !strings.Contains(string(b), "vm.nr_hugepages = 768") {
		t.Fatalf("应按即将生效基线（default 2M）钉 2M 池声明值 768：%q", b)
	}
	if strings.Contains(string(b), "vm.nr_hugepages = 2\n") {
		t.Fatalf("不得按当前运行 cmdline 的 1G 池声明值 2 钉（池身份错位）：%q", b)
	}
}

// 决策 #423：90 号文件的注释必须**与实况一致**——不单方面声称「vpp 的 80 号文件已由
// dpkg-divert 挪走」（round3 走查现场：divert 列表为空、80 号原样在场，而注释却写已接管，
// 操作者据此误判），改为「接管状态以 divert 记录为准」并给出查证方式。
func TestGenerateHugepageSysctlCommentTruthful(t *testing.T) {
	got := GenerateHugepageSysctl(KernelDesired{DefaultHugepageSize: "2M", Hugepages2M: 768})
	if got == "" {
		t.Fatal("默认尺寸 2M 且声明 768 时应产出片段")
	}
	if !strings.Contains(got, "以 divert 记录为准") {
		t.Fatalf("注释应写明接管状态以 divert 记录为准：%q", got)
	}
	if !strings.Contains(got, "dpkg-divert --list "+VppSysctlPath) {
		t.Fatalf("注释应给出查证方式（dpkg-divert --list 原路径）：%q", got)
	}
	if strings.Contains(got, "已由 dpkg-divert 挪到") {
		t.Fatalf("不得单方面声称已接管（与实况可能不符）：%q", got)
	}
}

// 决策 #423：运行期接管 vpp 包的 80 号 conffile（dpkg-divert → .vpp-disabled），接管状态以
// **divert 记录**为准（原路径在场也不重复接管）；接管成功同批次按当前声明重写 90 号文件。
// 五处幂等/边界：已接管不重复执行、未接管且文件在场则接管一次、文件不在则跳过（连查询都不做）、
// 无 dpkg-divert 则不动作、查询失败如实报错。
func TestVppSysctlTakeoverEnsure(t *testing.T) {
	lookOK := func(string) (string, error) { return "/usr/bin/dpkg-divert", nil }
	// 记录命令的假 Runner：--list 按 *diverted 返回接管记录，--add（--package 开头）模拟
	// 真实的 divert 生效（此后 --list 有记录），其余命令回空。
	newFake := func(diverted *bool) (*[]string, Runner) {
		var calls []string
		runner := func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, strings.Join(append([]string{name}, args...), " "))
			if len(args) == 0 {
				return "", nil
			}
			switch args[0] {
			case "--list":
				if *diverted {
					return "diversion of " + args[1] + " to " + args[1] + ".vpp-disabled by " +
						vppSysctlDivertPkg + "\n", nil
				}
				return "", nil
			case "--package":
				*diverted = true
			}
			return "", nil
		}
		return &calls, runner
	}
	cmdline := "BOOT_IMAGE=/vmlinuz ro default_hugepagesz=2M hugepagesz=2M hugepages=768\n"
	no := func() *bool { f := false; return &f }

	t.Run("已接管：不重复执行", func(t *testing.T) {
		root := t.TempDir()
		// 即便原路径仍在（例如手工把文件放回来），有 divert 记录即视为已接管——判据不是存在性。
		writeProc(t, root, VppSysctlPath, "vm.nr_hugepages=1024\n")
		writeProc(t, root, "/proc/cmdline", cmdline)
		calls, runner := newFake(func() *bool { y := true; return &y }())
		changed, err := (&VppSysctlTakeover{Root: root, Runner: runner, LookPath: lookOK}).
			Ensure(context.Background())
		if err != nil || changed {
			t.Fatalf("已接管不应报告接管动作（changed=%v err=%v）", changed, err)
		}
		if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "dpkg-divert --list ") {
			t.Fatalf("已接管只允许查询一次，不得再执行接管：%v", *calls)
		}
		if _, err := os.Stat(root + hugepageSysctlRel); !os.IsNotExist(err) {
			t.Fatalf("已接管时不应改动 90 号文件（%v）", err)
		}
	})

	t.Run("未接管且文件在场：接管一次并按声明写 90 号；再次调用幂等", func(t *testing.T) {
		root := t.TempDir()
		writeProc(t, root, VppSysctlPath, "vm.nr_hugepages=1024\nvm.hugetlb_shm_group=0\n")
		writeProc(t, root, "/proc/cmdline", cmdline)
		diverted := no()
		calls, runner := newFake(diverted)
		d := &VppSysctlTakeover{Root: root, Runner: runner, LookPath: lookOK}
		changed, err := d.Ensure(context.Background())
		if err != nil || !changed {
			t.Fatalf("未接管且文件在场应执行接管（changed=%v err=%v）", changed, err)
		}
		if len(*calls) != 2 {
			t.Fatalf("应恰好执行一次接管（查询 + 接管）：%v", *calls)
		}
		wantCmd := "dpkg-divert --package " + vppSysctlDivertPkg + " --add --rename --divert " +
			join(root, VppSysctlDisabledPath) + " " + join(root, VppSysctlPath)
		if (*calls)[1] != wantCmd {
			t.Fatalf("接管命令应与安装期口径逐字一致\n  期望: %s\n  实得: %s", wantCmd, (*calls)[1])
		}
		b, err := os.ReadFile(root + hugepageSysctlRel)
		if err != nil {
			t.Fatalf("接管后应写 90 号文件: %v", err)
		}
		if !strings.Contains(string(b), "vm.nr_hugepages = 768") ||
			!strings.Contains(string(b), "vm.hugetlb_shm_group = 0") {
			t.Fatalf("90 号文件应按当前声明重写：%q", b)
		}
		// 幂等：接管记录已出现，第二次调用不再执行接管命令。
		before := len(*calls)
		if changed, err := d.Ensure(context.Background()); err != nil || changed {
			t.Fatalf("第二次调用不应再接管（changed=%v err=%v）", changed, err)
		}
		if len(*calls) != before+1 || !strings.HasPrefix((*calls)[before], "dpkg-divert --list ") {
			t.Fatalf("第二次调用只应查询一次：%v", *calls)
		}
	})

	t.Run("文件不在：跳过（非错误，留给下一次）", func(t *testing.T) {
		root := t.TempDir()
		writeProc(t, root, "/proc/cmdline", cmdline)
		calls, runner := newFake(no())
		changed, err := (&VppSysctlTakeover{Root: root, Runner: runner, LookPath: lookOK}).
			Ensure(context.Background())
		if err != nil || changed {
			t.Fatalf("文件不在应静默跳过（changed=%v err=%v）", changed, err)
		}
		if len(*calls) != 0 {
			t.Fatalf("文件不在时不得执行任何命令（稳态每轮只做一次 stat）：%v", *calls)
		}
		if _, err := os.Stat(root + hugepageSysctlRel); !os.IsNotExist(err) {
			t.Fatalf("未发生接管时不应写 90 号文件（%v）", err)
		}
	})

	t.Run("无 dpkg-divert：跳过且不执行任何命令", func(t *testing.T) {
		root := t.TempDir()
		writeProc(t, root, VppSysctlPath, "vm.nr_hugepages=1024\n")
		calls, runner := newFake(no())
		d := &VppSysctlTakeover{Root: root, Runner: runner,
			LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
		changed, err := d.Ensure(context.Background())
		if err != nil || changed {
			t.Fatalf("无 dpkg-divert 应跳过（changed=%v err=%v）", changed, err)
		}
		if len(*calls) != 0 {
			t.Fatalf("无 dpkg-divert 时不得执行任何命令：%v", *calls)
		}
	})

	t.Run("查询失败：如实报错（不静默吞）", func(t *testing.T) {
		root := t.TempDir()
		writeProc(t, root, VppSysctlPath, "vm.nr_hugepages=1024\n")
		runner := func(context.Context, string, ...string) (string, error) {
			return "dpkg-divert: error: 读取数据库失败\n", exec.ErrNotFound
		}
		changed, err := (&VppSysctlTakeover{Root: root, Runner: runner, LookPath: lookOK}).
			Ensure(context.Background())
		if err == nil || changed {
			t.Fatalf("查询失败须返回错误（changed=%v err=%v）", changed, err)
		}
	})
}
