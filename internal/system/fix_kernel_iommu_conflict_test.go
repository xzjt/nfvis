package system

// IOMMU 语义族（iommu / intel_iommu / amd_iommu）不得自相矛盾（真机 v3.0.24）。
//
// 现场：`set system kernel iommu off` 生效后 /etc/default/grub.d/99-nfvis.cfg 里同时有
// `iommu=off intel_iommu=on` —— 配置说关、厂商默认说开，两套开关各自合法、合在一起是
// 操作者看不懂的产物（真正生效的语义取决于内核解析顺序，不是任何人写下的意图）。
// 口径：这三个参数名属**同一个语义族**，族内只允许出现一个成员；配置显式 off 时厂商开关必须让位，
// on/pt/未设置时的既有产出逐字不变。

import (
	"strings"
	"testing"
)

// iommuTokens 从 GRUB 片段的 cmdline 里挑出 IOMMU 族 token（按参数名，不按字符串包含）。
func iommuTokens(t *testing.T, grub string) []string {
	t.Helper()
	var out []string
	for _, ln := range strings.Split(grub, "\n") {
		if !strings.Contains(ln, "GRUB_CMDLINE_LINUX=") {
			continue
		}
		for _, tok := range strings.Fields(ln) {
			tok = strings.Trim(tok, "\"") // 行尾 token 带闭合引号
			name, _, ok := strings.Cut(tok, "=")
			if ok && isIOMMUFamilyName(name) {
				out = append(out, tok)
			}
		}
	}
	return out
}

// isIOMMUFamilyName 测试侧的族定义（与实现独立书写，避免「用实现证明实现」）。
func isIOMMUFamilyName(name string) bool {
	return name == "iommu" || name == "intel_iommu" || name == "amd_iommu"
}

func intelRootForIOMMU(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProc(t, root, "/proc/cpuinfo", "vendor_id\t: GenuineIntel\n")
	return root
}

func amdRootForIOMMU(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProc(t, root, "/proc/cpuinfo", "vendor_id\t: AuthenticAMD\n")
	return root
}

// 生成路径与产品一致：EnrichDesired（真机补全）→ GenerateBaseline。
func baselineFor(d KernelDesired, root string) string {
	grub, _ := GenerateBaseline(EnrichDesired(d, root))
	return grub
}

// IOMMU=off（Intel）：只有 iommu=off，没有 intel_iommu=on，也没有第二个 iommu 族 token。
func TestBaselineIOMMUOffIntelHasNoVendorSwitch(t *testing.T) {
	grub := baselineFor(KernelDesired{IOMMU: "off"}, intelRootForIOMMU(t))
	toks := iommuTokens(t, grub)
	if len(toks) != 1 || toks[0] != "iommu=off" {
		t.Fatalf("IOMMU 族应只含 iommu=off，实际 %v:\n%s", toks, grub)
	}
}

// IOMMU=off（AMD）：同理，amd_iommu=on 必须缺席。
func TestBaselineIOMMUOffAMDHasNoVendorSwitch(t *testing.T) {
	grub := baselineFor(KernelDesired{IOMMU: "off"}, amdRootForIOMMU(t))
	toks := iommuTokens(t, grub)
	if len(toks) != 1 || toks[0] != "iommu=off" {
		t.Fatalf("IOMMU 族应只含 iommu=off，实际 %v:\n%s", toks, grub)
	}
}

// params 逃生口里残留 intel_iommu=on、同时配置把 IOMMU 设为 off：
// 配置字段是更明确的意图，族内仍只留 iommu=off。
func TestBaselineIOMMUOffBeatsLeftoverParamsSwitch(t *testing.T) {
	d := KernelDesired{IOMMU: "off", ExtraParams: []string{"intel_iommu=on"}}
	grub := baselineFor(d, intelRootForIOMMU(t))
	toks := iommuTokens(t, grub)
	if len(toks) != 1 || toks[0] != "iommu=off" {
		t.Fatalf("残留的厂商开关不得与 iommu=off 并存，实际 %v:\n%s", toks, grub)
	}
}

// 只在 params 逃生口里写 off（字段未设）：同样是「关闭」意图，厂商默认的 iommu=pt/intel_iommu=on
// 不得与它并存——名字不同的两套开关合在一起还是同一个自相矛盾。
func TestBaselineIOMMUOffViaParamsAloneHasNoVendorSwitch(t *testing.T) {
	grub := baselineFor(KernelDesired{ExtraParams: []string{"intel_iommu=off"}}, intelRootForIOMMU(t))
	toks := iommuTokens(t, grub)
	if len(toks) != 1 || toks[0] != "intel_iommu=off" {
		t.Fatalf("params 里显式 off 时族内应只剩它，实际 %v:\n%s", toks, grub)
	}
	if !strings.Contains(grub, "intel_pstate=disable") {
		t.Fatalf("族外参数必须原样保留:\n%s", grub)
	}
}

// IOMMU=on：与今天一致（厂商开关 + iommu=on 都在，且只有一个 iommu= token）。
func TestBaselineIOMMUOnUnchanged(t *testing.T) {
	grub := baselineFor(KernelDesired{IOMMU: "on"}, intelRootForIOMMU(t))
	toks := iommuTokens(t, grub)
	want := map[string]bool{"iommu=on": true, "intel_iommu=on": true}
	if len(toks) != len(want) {
		t.Fatalf("IOMMU=on 的族内 token 应为 %v，实际 %v:\n%s", want, toks, grub)
	}
	for _, tok := range toks {
		if !want[tok] {
			t.Fatalf("IOMMU=on 不应出现 %q（族内只有 iommu=on + 厂商开关）:\n%s", tok, grub)
		}
	}
	if !strings.Contains(grub, "intel_pstate=disable") {
		t.Fatalf("无关参数不得被改动（intel_pstate=disable 应保留）:\n%s", grub)
	}
}

// IOMMU=pt：与今天一致（厂商开关 + iommu=pt 都在，且只有一个 iommu= token）。
func TestBaselineIOMMUPTUnchanged(t *testing.T) {
	grub := baselineFor(KernelDesired{IOMMU: "pt"}, intelRootForIOMMU(t))
	toks := iommuTokens(t, grub)
	want := map[string]bool{"iommu=pt": true, "intel_iommu=on": true}
	if len(toks) != len(want) {
		t.Fatalf("IOMMU=pt 的族内 token 应为 %v，实际 %v:\n%s", want, toks, grub)
	}
	for _, tok := range toks {
		if !want[tok] {
			t.Fatalf("IOMMU=pt 不应出现 %q:\n%s", tok, grub)
		}
	}
}

// IOMMU 未设置：与今天一致（厂商开关 + 厂商默认的 iommu=pt，族内不重复）。
func TestBaselineIOMMUUnsetUnchanged(t *testing.T) {
	grub := baselineFor(KernelDesired{}, intelRootForIOMMU(t))
	toks := iommuTokens(t, grub)
	want := map[string]bool{"iommu=pt": true, "intel_iommu=on": true}
	if len(toks) != len(want) {
		t.Fatalf("未设置 IOMMU 时族内 token 应为 %v，实际 %v:\n%s", want, toks, grub)
	}
	for _, tok := range toks {
		if !want[tok] {
			t.Fatalf("未设置 IOMMU 时不应出现 %q:\n%s", tok, grub)
		}
	}
	for _, keep := range []string{"intel_pstate=disable", "default_hugepagesz=2M"} {
		if !strings.Contains(grub, keep) {
			t.Fatalf("无关参数不得被改动（缺 %q）:\n%s", keep, grub)
		}
	}
}

// 生成器被**不经 EnrichDesired** 直接调用时（其它调用方/回归），
// IOMMU=off 也必须收敛成族内唯一成员。
func TestGenerateBaselineIOMMUOffDirectCall(t *testing.T) {
	grub, _ := GenerateBaseline(KernelDesired{IOMMU: "off", ExtraParams: []string{"iommu=pt", "intel_iommu=on", "intel_pstate=disable"}})
	toks := iommuTokens(t, grub)
	if len(toks) != 1 || toks[0] != "iommu=off" {
		t.Fatalf("直接调用也应只产 iommu=off，实际 %v:\n%s", toks, grub)
	}
	if !strings.Contains(grub, "intel_pstate=disable") {
		t.Fatalf("族外参数必须原样保留:\n%s", grub)
	}
}
