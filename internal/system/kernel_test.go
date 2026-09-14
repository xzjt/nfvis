package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProc(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestReadActualAndCompare(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "/proc/cmdline", "BOOT_IMAGE=/vmlinuz root=/dev/mapper/x default_hugepagesz=1G hugepagesz=1G hugepages=4 isolcpus=4-15\n")
	writeProc(t, root, "/proc/meminfo", "MemTotal:       6000000 kB\nHugePages_Total:       4\nHugePages_Free:        2\n")
	writeProc(t, root, "/proc/sys/kernel/nmi_watchdog", "0\n")
	writeProc(t, root, "/sys/kernel/mm/transparent_hugepage/enabled", "always [madvise] never\n")

	a := ReadActual(root)
	if a.Hugepages1G != 4 || a.Hugepages1GFr != 2 {
		t.Fatalf("大页读取异常: %+v", a)
	}
	if got := IsolatedFromCmdline(a.Cmdline); got != "4-15" {
		t.Fatalf("isolcpus 解析异常: %q", got)
	}
	if a.NMIWatchdog == nil || *a.NMIWatchdog {
		t.Fatalf("nmi_watchdog 解析异常: %+v", a.NMIWatchdog)
	}
	if a.THP != "madvise" {
		t.Fatalf("THP 解析异常: %q", a.THP)
	}

	// 期望与之一致 → 无差异
	off := false
	d := KernelDesired{Hugepages1G: 4, IsolatedCores: "4-15", NMIWatchdog: &off, THP: "madvise"}
	if diffs := Compare(d, a); len(diffs) != 0 {
		t.Fatalf("应无差异，实际: %v", diffs)
	}
	// 期望改大页数 → 报差异
	d2 := KernelDesired{Hugepages1G: 8, IsolatedCores: "4-15", NMIWatchdog: &off, THP: "madvise"}
	diffs := Compare(d2, a)
	if len(diffs) != 1 || !strings.Contains(diffs[0], "大页 1G") {
		t.Fatalf("应只报大页差异: %v", diffs)
	}
	// isolcpus 差异
	d3 := KernelDesired{Hugepages1G: 4, IsolatedCores: "4-11", NMIWatchdog: &off, THP: "madvise"}
	if diffs := Compare(d3, a); len(diffs) != 1 || !strings.Contains(diffs[0], "isolcpus") {
		t.Fatalf("应只报 isolcpus 差异: %v", diffs)
	}
}

func TestGenerateBaseline(t *testing.T) {
	off := false
	on := true
	d := KernelDesired{Hugepages1G: 8, IsolatedCores: "4-15", NMIWatchdog: &off, THP: "never",
		IOMMU: "pt", TunedProfile: "nfvis-throughput", ExtraParams: []string{"intel_iommu=on"}}
	grub, fstab := GenerateBaseline(d)
	// 必须追加到标准变量（自定义变量不被 grub-mkconfig 采纳）
	if !strings.Contains(grub, `GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX} `) {
		t.Fatalf("GRUB 片段必须以追加形式写标准变量: %s", grub)
	}
	for _, want := range []string{"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=8",
		"isolcpus=4-15", "nohz_full=4-15", "rcu_nocbs=4-15", "nmi_watchdog=0",
		"transparent_hugepage=never", "iommu=pt", "intel_iommu=on"} {
		if !strings.Contains(grub, want) {
			t.Fatalf("GRUB 片段缺少 %q:\n%s", want, grub)
		}
	}
	if !strings.Contains(fstab, "pagesize=1G") {
		t.Fatalf("fstab 行异常: %q", fstab)
	}
	// NMI watchdog 期望开启时不写 nmi_watchdog=0
	d2 := KernelDesired{NMIWatchdog: &on}
	if g, _ := GenerateBaseline(d2); strings.Contains(g, "nmi_watchdog=0") {
		t.Fatal("期望开启 NMI watchdog 时不应写 nmi_watchdog=0")
	}
}

func TestDesiredFromConfig(t *testing.T) {
	d := DesiredFromConfig("1G", 8, "4-15", "false", "never", "pt", "nfvis-throughput", []string{"b", "a"})
	if d.Hugepages1G != 8 || d.IsolatedCores != "4-15" || d.THP != "never" || d.IOMMU != "pt" {
		t.Fatalf("派生异常: %+v", d)
	}
	if d.NMIWatchdog == nil || *d.NMIWatchdog {
		t.Fatalf("nmi 派生异常: %+v", d.NMIWatchdog)
	}
	if strings.Join(d.ExtraParams, ",") != "a,b" {
		t.Fatalf("extra params 应排序: %v", d.ExtraParams)
	}
	// 2M 页：1G 项不托管（-1），仅 2M 生效
	d2 := DesiredFromConfig("2M", 1024, "", "", "", "", "", nil)
	if d2.Hugepages2M != 1024 || d2.Hugepages1G != -1 {
		t.Fatalf("2M 派生异常: %+v", d2)
	}
	if g, f := GenerateBaseline(d2); strings.Contains(g, "hugepagesz=1G") || !strings.Contains(g, "hugepages=1024") ||
		!strings.Contains(f, "pagesize=2M") {
		t.Fatalf("2M 基线生成异常: grub=%q fstab=%q", g, f)
	}
}
