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

func TestReadActualSysfsPerSize(t *testing.T) {
	// 决策 #106：双池按尺寸各读各的 sysfs（meminfo 只反映缺省尺寸）
	root := t.TempDir()
	writeProc(t, root, "/proc/cmdline", "default_hugepagesz=1G hugepagesz=1G hugepages=2 hugepagesz=2M hugepages=768\n")
	writeProc(t, root, "/proc/meminfo", "HugePages_Total:       9\nHugePages_Free:       8\n") // 干扰项，不应被采用
	writeProc(t, root, "/sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages", "2\n")
	writeProc(t, root, "/sys/kernel/mm/hugepages/hugepages-1048576kB/free_hugepages", "2\n")
	writeProc(t, root, "/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages", "768\n")
	writeProc(t, root, "/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages", "700\n")

	a := ReadActual(root)
	if a.Hugepages1G != 2 || a.Hugepages1GFr != 2 || a.Hugepages2M != 768 || a.Hugepages2MFr != 700 {
		t.Fatalf("双池应按 sysfs 各尺寸读取: %+v", a)
	}
}

func TestReadActualMeminfoFallback(t *testing.T) {
	// sysfs 目录不存在（非常规环境）→ 回退 meminfo，只填缺省尺寸
	root := t.TempDir()
	writeProc(t, root, "/proc/cmdline", "default_hugepagesz=1G hugepagesz=1G hugepages=4\n")
	writeProc(t, root, "/proc/meminfo", "HugePages_Total:       4\nHugePages_Free:       2\n")
	a := ReadActual(root)
	if a.Hugepages1G != 4 || a.Hugepages1GFr != 2 {
		t.Fatalf("回退路径应读 meminfo: %+v", a)
	}
}

func TestHugepageFromCmdline(t *testing.T) {
	dual := []string{"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=2", "hugepagesz=2M", "hugepages=768"}
	if got := HugepageFromCmdline(dual, "1G"); got != "2" {
		t.Fatalf("双池 1G=%q", got)
	}
	if got := HugepageFromCmdline(dual, "2M"); got != "768" {
		t.Fatalf("双池 2M=%q", got)
	}
	// 单 2M：无 hugepagesz 前缀 → 归缺省尺寸
	single := []string{"hugepages=1024"}
	if got := HugepageFromCmdline(single, "2M"); got != "1024" {
		t.Fatalf("单池 2M=%q", got)
	}
	if got := HugepageFromCmdline(single, "1G"); got != "" {
		t.Fatalf("单池 1G 应为空: %q", got)
	}
}

func TestGenerateBaselineDualHugepages(t *testing.T) {
	grub, fstab := GenerateBaseline(KernelDesired{Hugepages1G: 2, Hugepages2M: 768})
	for _, want := range []string{"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=2", "hugepagesz=2M", "hugepages=768"} {
		if !strings.Contains(grub, want) {
			t.Fatalf("双池片段缺少 %q:\n%s", want, grub)
		}
	}
	if !strings.Contains(fstab, "pagesize=1G") {
		t.Fatalf("双池 fstab 主池应为 1G: %q", fstab)
	}
	// Compare：2M 期望与实际不符 → 报「大页 2M」
	a := KernelActual{Hugepages2M: 700}
	diffs := Compare(KernelDesired{Hugepages1G: 2, Hugepages2M: 768}, a)
	found := false
	for _, d := range diffs {
		if strings.Contains(d, "大页 2M") {
			found = true
		}
	}
	if !found {
		t.Fatalf("2M 不符应报差异: %v", diffs)
	}
	// 双池一致 → 无差异（大页按 cmdline 基线比对，故 cmdline 必须同时给出）
	a2 := KernelActual{
		Cmdline:     []string{"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=2", "hugepagesz=2M", "hugepages=768"},
		Hugepages1G: 2, Hugepages2M: 768,
	}
	if diffs := Compare(KernelDesired{Hugepages1G: 2, Hugepages2M: 768}, a2); len(diffs) != 0 {
		t.Fatalf("双池一致应无差异: %v", diffs)
	}
}

// TestCompareHugepageRuntimePoolGrowth R84-5：大页一致性按 cmdline 基线判定。
// 运行期被顶大的池（VPP 早期用 1G 页）在 cmdline 与期望一致时不得报「需重启」，否则指引不可达。
func TestCompareHugepageRuntimePoolGrowth(t *testing.T) {
	off := false
	// ① cmdline=期望、运行实际更大 → 无差异（运行期占用），仅中性说明
	d := KernelDesired{Hugepages1G: 1, NMIWatchdog: &off}
	a := KernelActual{
		Cmdline:     []string{"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=1", "isolcpus=2-5"},
		Hugepages1G: 2, NMIWatchdog: &off,
	}
	if diffs := Compare(d, a); len(diffs) != 0 {
		t.Fatalf("cmdline 与期望一致时运行期池增长不应报差异: %v", diffs)
	}
	notes := HugepageRuntimeNotes(d, a)
	if len(notes) != 1 || !strings.Contains(notes[0], "运行期占用") || !strings.Contains(notes[0], "不需要重启") {
		t.Fatalf("应给出「运行期占用、不需要重启」的中性说明: %v", notes)
	}
	// 基线未生效（cmdline 与期望不符）时不给中性说明——偏差由 Compare 报，别掩盖
	aBase := a
	aBase.Cmdline = []string{"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=4"}
	if notes := HugepageRuntimeNotes(d, aBase); len(notes) != 0 {
		t.Fatalf("cmdline 基线未生效时不应给运行期占用说明: %v", notes)
	}

	// ② cmdline=期望、运行实际更小（内核没按 cmdline 分配够）→ 告警且指向「分配不足」
	aShort := a
	aShort.Hugepages1G = 0
	diffs := Compare(d, aShort)
	if len(diffs) != 1 || !strings.Contains(diffs[0], "大页 1G") || !strings.Contains(diffs[0], "分配不足") {
		t.Fatalf("运行实际少于声明应报「分配不足」: %v", diffs)
	}
	if !strings.Contains(diffs[0], "期望 1") || !strings.Contains(diffs[0], "实际 0") {
		t.Fatalf("分配不足文案应带期望/实际数值: %v", diffs)
	}
	if strings.Contains(diffs[0], "需写入 GRUB 基线") {
		t.Fatalf("cmdline 已一致时不应再引导写 GRUB 基线: %v", diffs)
	}
	if notes := HugepageRuntimeNotes(d, aShort); len(notes) != 0 {
		t.Fatalf("分配不足不应降级成中性说明: %v", notes)
	}

	// ③ cmdline 与期望不符（含未声明）→ 仍报「需写入 GRUB 基线并重启生效」
	for name, cmdline := range map[string][]string{
		"值不符":  {"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=4"},
		"未声明":  {"default_hugepagesz=2M", "hugepages=768"},
		"基线为空": {},
	} {
		diffs := Compare(d, KernelActual{Cmdline: cmdline, Hugepages1G: 1, NMIWatchdog: &off})
		if len(diffs) != 1 || !strings.Contains(diffs[0], "需写入 GRUB 基线并重启生效") {
			t.Fatalf("%s：应报「需写入 GRUB 基线并重启生效」: %v", name, diffs)
		}
	}

	// 双池各按各的尺寸判定：2M 运行实际更大不报，1G 未声明照报
	aDual := KernelActual{
		Cmdline:     []string{"default_hugepagesz=1G", "hugepagesz=1G", "hugepages=1", "hugepagesz=2M", "hugepages=768"},
		Hugepages1G: 2, Hugepages2M: 800,
	}
	if diffs := Compare(KernelDesired{Hugepages1G: 1, Hugepages2M: 768}, aDual); len(diffs) != 0 {
		t.Fatalf("双池运行实际均高于声明且 cmdline 一致时不应报差异: %v", diffs)
	}
	if notes := HugepageRuntimeNotes(KernelDesired{Hugepages1G: 1, Hugepages2M: 768}, aDual); len(notes) != 2 {
		t.Fatalf("双池各给一条运行期占用说明: %v", notes)
	}
}
