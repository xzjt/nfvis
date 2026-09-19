package system

// 真机补全（EnrichDesired）与护栏（ValidateDesired）的单测：全部经临时目录构造
// /proc、/sys 事实，不依赖宿主。红-绿要点：护栏必须真的拒绝（把核全隔离/越界）、
// 厂商分支必须只加本厂商的参数、无 nohz_full 支持时必须真的省略。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectCPUVendor(t *testing.T) {
	cases := []struct {
		cpuinfo string
		want    CPUVendor
	}{
		{"processor : 0\nvendor_id\t: GenuineIntel\n", VendorIntel},
		{"processor : 0\nvendor_id\t: AuthenticAMD\n", VendorAMD},
		{"processor : 0\n", VendorUnknown},                         // 无 vendor_id
		{"vendor_id\t: OtherVendor\n", VendorUnknown},              // 不认识的厂商
		{"vendor_id\t: GenuineIntel\nflags\t: fpu\n", VendorIntel}, // 取第一条 vendor_id
	}
	for _, c := range cases {
		root := t.TempDir()
		writeProc(t, root, "/proc/cpuinfo", c.cpuinfo)
		if got := detectCPUVendor(root); got != c.want {
			t.Fatalf("vendor_id=%q: 期望 %q 实际 %q", c.cpuinfo, c.want, got)
		}
	}
	// 文件不存在（非 Linux）→ 未知，不猜测
	if got := detectCPUVendor(t.TempDir()); got != VendorUnknown {
		t.Fatalf("无 cpuinfo 应为 unknown，实际 %q", got)
	}
}

func TestEnrichVendorParams(t *testing.T) {
	intelRoot := t.TempDir()
	writeProc(t, intelRoot, "/proc/cpuinfo", "vendor_id\t: GenuineIntel\n")

	d := EnrichDesired(KernelDesired{IsolatedCores: "2-3"}, intelRoot)
	for _, want := range []string{"intel_iommu=on", "intel_pstate=disable", "iommu=pt"} {
		if !contains(d.ExtraParams, want) {
			t.Fatalf("Intel 机器应补 %q: %v", want, d.ExtraParams)
		}
	}

	// 用户显式设置同名参数时以用户为准（IOMMU 字段 / params 逃生口）
	d = EnrichDesired(KernelDesired{IOMMU: "off"}, intelRoot)
	if contains(d.ExtraParams, "iommu=pt") {
		t.Fatalf("用户已设 iommu=off，补全不得再写 iommu=pt: %v", d.ExtraParams)
	}
	if !contains(d.ExtraParams, "iommu=off") && d.IOMMU != "off" {
		t.Fatalf("用户设置应保留: %+v", d)
	}
	d = EnrichDesired(KernelDesired{ExtraParams: []string{"intel_iommu=off"}}, intelRoot)
	if contains(d.ExtraParams, "intel_iommu=on") {
		t.Fatalf("用户已给 intel_iommu=off，补全不得再写 intel_iommu=on: %v", d.ExtraParams)
	}

	// 厂商识别不了 → 不加任何参数（不猜测）
	if d := EnrichDesired(KernelDesired{}, t.TempDir()); len(d.ExtraParams) != 0 {
		t.Fatalf("厂商未知时不应补参数: %v", d.ExtraParams)
	}

	// 幂等：重复补全产出不变
	d1 := EnrichDesired(KernelDesired{IsolatedCores: "2-3"}, intelRoot)
	d2 := EnrichDesired(d1, intelRoot)
	if strings.Join(d1.ExtraParams, ",") != strings.Join(d2.ExtraParams, ",") || d1.IRQAffinity != d2.IRQAffinity {
		t.Fatalf("补全应幂等: %+v vs %+v", d1, d2)
	}
}

func TestEnrichIRQAffinity(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "/sys/devices/system/cpu/online", "0-5\n")

	d := EnrichDesired(KernelDesired{IsolatedCores: "1-3"}, root)
	if d.IRQAffinity != "0,4-5" {
		t.Fatalf("irqaffinity 应为隔离核的补集 0,4-5，实际 %q", d.IRQAffinity)
	}

	// 用户已给 irqaffinity → 不覆盖
	d = EnrichDesired(KernelDesired{IsolatedCores: "1-3", ExtraParams: []string{"irqaffinity=2"}}, root)
	if d.IRQAffinity != "" {
		t.Fatalf("用户已给 irqaffinity，补全不应再写: %q", d.IRQAffinity)
	}

	// 隔离核为空 / 在线核读不到 → 不写
	if d := EnrichDesired(KernelDesired{}, root); d.IRQAffinity != "" {
		t.Fatalf("无隔离核不应写 irqaffinity: %q", d.IRQAffinity)
	}
	if d := EnrichDesired(KernelDesired{IsolatedCores: "1-2"}, t.TempDir()); d.IRQAffinity != "" {
		t.Fatalf("在线核未知时不应写 irqaffinity: %q", d.IRQAffinity)
	}
}

func TestEnrichNoHZFullProbe(t *testing.T) {
	// sysfs 属性存在 → 支持
	root := t.TempDir()
	writeProc(t, root, "/sys/devices/system/cpu/nohz_full", "(null)\n")
	if d := EnrichDesired(KernelDesired{IsolatedCores: "2-3"}, root); d.NoHZFull == nil || !*d.NoHZFull {
		t.Fatalf("sysfs 有 nohz_full 应判定支持: %+v", d.NoHZFull)
	}
	// sysfs 没有、/boot/config 有 CONFIG_NO_HZ_FULL=y → 支持
	root = t.TempDir()
	writeProc(t, root, "/proc/sys/kernel/osrelease", "6.1.0-test\n")
	writeProc(t, root, "/boot/config-6.1.0-test", "CONFIG_NO_HZ_FULL=y\n")
	if d := EnrichDesired(KernelDesired{IsolatedCores: "2-3"}, root); d.NoHZFull == nil || !*d.NoHZFull {
		t.Fatalf("config 有 CONFIG_NO_HZ_FULL=y 应判定支持: %+v", d.NoHZFull)
	}
	// config 里没有 → 不支持（省略 nohz_full/rcu_nocbs）
	root = t.TempDir()
	writeProc(t, root, "/proc/sys/kernel/osrelease", "6.1.0-test\n")
	writeProc(t, root, "/boot/config-6.1.0-test", "CONFIG_SMP=y\n")
	if d := EnrichDesired(KernelDesired{IsolatedCores: "2-3"}, root); d.NoHZFull == nil || *d.NoHZFull {
		t.Fatalf("config 无 CONFIG_NO_HZ_FULL 应判定不支持: %+v", d.NoHZFull)
	}
	// 两者都拿不到 → 按支持处理（保持既有行为：漏写优化才是损失）
	if d := EnrichDesired(KernelDesired{IsolatedCores: "2-3"}, t.TempDir()); d.NoHZFull == nil || !*d.NoHZFull {
		t.Fatalf("探测不到时应按支持处理: %+v", d.NoHZFull)
	}
}

func TestGenerateBaselineNoHZSkipAndIRQAffinity(t *testing.T) {
	no := false
	d := KernelDesired{IsolatedCores: "2-3", NoHZFull: &no, IRQAffinity: "0,1,4-5"}
	grub, _ := GenerateBaseline(d)
	if strings.Contains(grub, "nohz_full=") || strings.Contains(grub, "rcu_nocbs=") {
		t.Fatalf("内核不支持 nohz_full 时应省略 nohz_full/rcu_nocbs:\n%s", grub)
	}
	if !strings.Contains(grub, "isolcpus=2-3") || !strings.Contains(grub, "irqaffinity=0,1,4-5") {
		t.Fatalf("isolcpus/irqaffinity 应照写:\n%s", grub)
	}
	// 未探测（nil）→ 保持既有行为，写 nohz_full/rcu_nocbs
	grub, _ = GenerateBaseline(KernelDesired{IsolatedCores: "2-3"})
	if !strings.Contains(grub, "nohz_full=2-3") || !strings.Contains(grub, "rcu_nocbs=2-3") {
		t.Fatalf("未探测时应保持既有行为:\n%s", grub)
	}
}

func TestValidateDesired(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "/sys/devices/system/cpu/online", "0-5\n")

	if err := ValidateDesired(KernelDesired{IsolatedCores: "2-3"}, root); err != nil {
		t.Fatalf("留 4 个非隔离核应通过: %v", err)
	}
	err := ValidateDesired(KernelDesired{IsolatedCores: "0-5"}, root)
	if err == nil || !strings.Contains(err.Error(), "至少需保留 2 个") {
		t.Fatalf("隔离全部核应被拒绝: %v", err)
	}
	err = ValidateDesired(KernelDesired{IsolatedCores: "1-5"}, root)
	if err == nil || !strings.Contains(err.Error(), "至少需保留 2 个") {
		t.Fatalf("只留 1 个非隔离核也应被拒绝: %v", err)
	}
	err = ValidateDesired(KernelDesired{IsolatedCores: "7"}, root)
	if err == nil || !strings.Contains(err.Error(), "不存在的核 7") {
		t.Fatalf("越界核应被拒绝: %v", err)
	}
	if err := ValidateDesired(KernelDesired{IsolatedCores: "abc"}, root); err == nil {
		t.Fatal("非法核列表应被拒绝")
	}
	// params 逃生口里的 isolcpus 同样要拦（它排后面、实际生效）
	err = ValidateDesired(KernelDesired{ExtraParams: []string{"isolcpus=0-5"}}, root)
	if err == nil || !strings.Contains(err.Error(), "至少需保留 2 个") {
		t.Fatalf("逃生口里的 isolcpus 也应被拦: %v", err)
	}
	// 在线核读不到 → 不下判断（非常规环境）
	if err := ValidateDesired(KernelDesired{IsolatedCores: "0-5"}, t.TempDir()); err != nil {
		t.Fatalf("在线核未知时不应下判断: %v", err)
	}
	// 重叠区间去重后计数（0-3,3-5 是 6 个核，不是 7 个）
	writeProc(t, root, "/sys/devices/system/cpu/online", "0-7\n")
	if err := ValidateDesired(KernelDesired{IsolatedCores: "0-3,3-5"}, root); err != nil {
		t.Fatalf("重叠区间应按去重计数: %v", err)
	}
}

func TestApplyValidatesAndEnriches(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "/proc/cpuinfo", "vendor_id\t: GenuineIntel\n")
	writeProc(t, root, "/sys/devices/system/cpu/online", "0-5\n")
	calls := 0
	a := &BaselineApplier{Root: root, Runner: func(string, ...string) error { calls++; return nil }}

	// 护栏：Apply 直接拒绝，且不落任何盘
	if _, err := a.Apply(KernelDesired{Hugepages1G: 2, IsolatedCores: "0-5"}); err == nil {
		t.Fatal("隔离全部核的 Apply 应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/default/grub.d/99-nfvis.cfg")); !os.IsNotExist(err) {
		t.Fatal("被拒绝的 Apply 不应写入片段")
	}
	if calls != 0 {
		t.Fatalf("被拒绝的 Apply 不应跑 update-grub: %d", calls)
	}

	// 补全：Apply 写出的片段含 vendor 参数与 irqaffinity（与安装器同源）
	if _, err := a.Apply(KernelDesired{Hugepages1G: 2, IsolatedCores: "1-3"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	frag, err := os.ReadFile(filepath.Join(root, "etc/default/grub.d/99-nfvis.cfg"))
	if err != nil {
		t.Fatalf("片段未写入: %v", err)
	}
	for _, want := range []string{"isolcpus=1-3", "irqaffinity=0,4-5", "intel_iommu=on", "intel_pstate=disable", "iommu=pt"} {
		if !strings.Contains(string(frag), want) {
			t.Fatalf("片段缺少 %q:\n%s", want, frag)
		}
	}
}

func contains(params []string, want string) bool {
	for _, p := range params {
		if p == want {
			return true
		}
	}
	return false
}
