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
