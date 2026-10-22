package api

// 真机缺陷：`set vm-image-profiles <img> cpu mode|model <v>` 无法**新建**画像元素
// （兄弟语句 `set vm-image-profiles <img> accel kvm` 走通用树遍历，会自动建元素）。
//
// 根因：aliasProfileCPUScalar 用 elemByID 取元素，元素缺席即报「无匹配配置」；而同族的
// aliasStartupCPUScalar 在 set 时按需建对象。判据差异纯属实现疏漏——语句语义上
// 「set 一个字段」本身就该把承载它的对象建出来（与 accel/machine 等兄弟语句产出同一模型）。
//
// 本文件钉住三条验收：
//  1. cpu mode 在全新画像上生效（candidate 树里出现该元素且 cpu_mode 已落）；
//  2. cpu model 同上（落 cpu_model）；
//  3. delete cpu mode 对**不存在**的画像不建空壳（仍报无匹配配置，树里不出现该元素）。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// profileElemOf 取 candidate 树里 image 对应的 vm_image_profiles 元素；不存在返回 nil。
func profileElemOf(t *testing.T, tree map[string]any, image string) map[string]any {
	t.Helper()
	arr, _ := tree["vm_image_profiles"].([]any)
	em, _ := selectElement(arr, "image", image)
	return em
}

// TestProfileCPUSetCreatesElement：全新画像上的 `cpu mode` 必须建出元素并落 cpu_mode。
func TestProfileCPUSetCreatesElement(t *testing.T) {
	x, engine := newCLIKit(t)
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure"); res.Output != "" {
		t.Fatalf("configure: %q", res.Output)
	}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set vm-image-profiles newprof.qcow2 cpu mode host-passthrough")
	if strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("全新画像上 set cpu mode 应成功（兄弟语句 accel 能建元素，本条同族）：%s", res.Output)
	}
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.VMImageProfiles) != 1 {
		t.Fatalf("应建出 1 个画像元素，实得 %+v", cfg.VMImageProfiles)
	}
	if p := cfg.VMImageProfiles[0]; p.Image != "newprof.qcow2" || p.CPUMode != "host-passthrough" {
		t.Fatalf("元素身份/字段不符：%+v", p)
	}
	// 与「树里有这个元素」同义：模型往返后树里应能按 image 取到该元素（不是只在强类型侧有效）。
	em := profileElemOf(t, toJSONTree(cfg), "newprof.qcow2")
	if em == nil {
		t.Fatalf("candidate 树应含 image=newprof.qcow2 的元素: %v", toJSONTree(cfg)["vm_image_profiles"])
	}
	if v, _ := em["cpu_mode"].(string); v != "host-passthrough" {
		t.Fatalf("树里 cpu_mode 应为 host-passthrough: %+v", em)
	}
}

// TestProfileCPUSetModelCreatesElement：`cpu model` 同族同修。
func TestProfileCPUSetModelCreatesElement(t *testing.T) {
	x, engine := newCLIKit(t)
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure"); res.Output != "" {
		t.Fatalf("configure: %q", res.Output)
	}
	// mode=custom 是 model 的合法前提（提交期校验要求 custom 必须给 model）。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"set vm-image-profiles newprof2.qcow2 cpu mode custom")
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set vm-image-profiles newprof2.qcow2 cpu model Skylake-Client")
	if strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("全新画像上 set cpu model 应成功：%s", res.Output)
	}
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.VMImageProfiles) != 1 || cfg.VMImageProfiles[0].CPUModel != "Skylake-Client" {
		t.Fatalf("cpu_model 未落模型：%+v", cfg.VMImageProfiles)
	}
	em := profileElemOf(t, toJSONTree(cfg), "newprof2.qcow2")
	if em == nil {
		t.Fatalf("candidate 树应含 image=newprof2.qcow2 的元素")
	}
	if v, _ := em["cpu_model"].(string); v != "Skylake-Client" {
		t.Fatalf("树里 cpu_model 应为 Skylake-Client: %+v", em)
	}
}

// TestProfileCPUDeleteMissingDoesNotCreate：delete 形态保持既有口径——影像不存在即报
// 无匹配配置，**不**凭空建一个只有 image 的空壳（空壳会让「删了不存在的画像」看起来成功）。
func TestProfileCPUDeleteMissingDoesNotCreate(t *testing.T) {
	x, engine := newCLIKit(t)
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure"); res.Output != "" {
		t.Fatalf("configure: %q", res.Output)
	}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"delete vm-image-profiles ghost.qcow2 cpu mode")
	if !strings.Contains(res.Output, "无匹配配置") {
		t.Fatalf("删除不存在的画像应报无匹配配置：%q", res.Output)
	}
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.VMImageProfiles) != 0 {
		t.Fatalf("删除不存在的画像不得建出元素：%+v", cfg.VMImageProfiles)
	}
	if _, ok := toJSONTree(cfg)["vm_image_profiles"]; ok {
		t.Fatalf("candidate 树不得出现 vm_image_profiles 键: %v", toJSONTree(cfg))
	}
}
