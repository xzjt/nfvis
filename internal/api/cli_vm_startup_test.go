package api

// 决策 #485：镜像启动画像（vm-image-profiles）与实例级 startup 覆盖的执行器接线。
//
// Task 2/5 已加模型字段与命令树，本测试钉住「set/delete 语句经通用树遍历 + 别名规则
// 真正落进 Config.VMImageProfiles / VMFunction.Startup」：
//   - 具名数组身份字段是 image（而非默认 name）——否则元素建出的键为 name、
//     fromJSONTree 的 DisallowUnknownFields 直接报错（set 假失败）；
//   - cpu mode/cpu model 在树里是两层（cpu → mode），模型却是平铺 cpu_mode/cpu_model，
//     需别名规则显式映射，否则落成模型不认识的 cpu:{mode:…}。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// TestSetVMImageProfileViaGenericPath：vm-image-profiles 的 set/delete 经通用路径落模型。
func TestSetVMImageProfileViaGenericPath(t *testing.T) {
	x, engine := newCLIKit(t)
	exec := func(line string) CLIEResult {
		t.Helper()
		return x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
	}
	if res := exec("configure"); res.Output != "" || res.Warning {
		t.Fatalf("configure: %q warning=%v", res.Output, res.Warning)
	}

	for _, line := range []string{
		"set vm-image-profiles img1 machine pc",
		`set vm-image-profiles img1 extra-args "-global x"`,
		"set vm-image-profiles img1 cpu mode custom",
	} {
		if res := exec(line); strings.HasPrefix(res.Output, "%%") {
			t.Fatalf("%s failed: %s", line, res.Output)
		}
	}

	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.VMImageProfiles) != 1 {
		t.Fatalf("期望 1 个画像，实得 %+v", cfg.VMImageProfiles)
	}
	p := cfg.VMImageProfiles[0]
	if p.Image != "img1" {
		t.Fatalf("元素身份字段应为 image=img1：%+v", p)
	}
	if p.Machine != "pc" {
		t.Fatalf("machine 未落模型：%+v", p)
	}
	if len(p.ExtraArgs) != 1 || p.ExtraArgs[0] != "-global x" {
		t.Fatalf("extra-args 未落模型：%+v", p.ExtraArgs)
	}
	if p.CPUMode != "custom" {
		t.Fatalf("cpu mode 应落平铺 cpu_mode：%+v", p)
	}

	// delete cpu mode（不带取值）清掉平铺 cpu_mode，machine/extra-args 不动。
	if res := exec("delete vm-image-profiles img1 cpu mode"); strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("delete cpu mode failed: %s", res.Output)
	}
	cfgDel, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfgDel.VMImageProfiles) != 1 || cfgDel.VMImageProfiles[0].CPUMode != "" {
		t.Fatalf("delete cpu mode 未清平铺字段：%+v", cfgDel.VMImageProfiles)
	}
	if cfgDel.VMImageProfiles[0].Machine != "pc" {
		t.Fatalf("delete cpu mode 不应动其它字段：%+v", cfgDel.VMImageProfiles[0])
	}

	// 删除整个画像：一次提交清空数组。
	if res := exec("delete vm-image-profiles img1"); strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("delete failed: %s", res.Output)
	}
	cfg2, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg2.VMImageProfiles) != 0 {
		t.Fatalf("delete not applied: %+v", cfg2.VMImageProfiles)
	}
}

// TestSetVMStartupOverrideViaAlias：实例级 startup 覆盖的 cpu mode/model 落进 startup 对象。
func TestSetVMStartupOverrideViaAlias(t *testing.T) {
	x, engine := newCLIKit(t)
	exec := func(line string) CLIEResult {
		t.Helper()
		return x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
	}
	if res := exec("configure"); res.Output != "" || res.Warning {
		t.Fatalf("configure: %q warning=%v", res.Output, res.Warning)
	}
	if res := exec("set virtual-machine-functions v1 image img1"); strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("建 VM 元素失败: %s", res.Output)
	}
	for _, line := range []string{
		"set virtual-machine-functions v1 startup cpu mode custom",
		"set virtual-machine-functions v1 startup cpu model Skylake-Client",
	} {
		if res := exec(line); strings.HasPrefix(res.Output, "%%") {
			t.Fatalf("%s failed: %s", line, res.Output)
		}
	}

	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.VirtualMachineFunctions) != 1 || cfg.VirtualMachineFunctions[0].Startup == nil {
		t.Fatalf("startup 覆盖未落模型：%+v", cfg.VirtualMachineFunctions)
	}
	st := cfg.VirtualMachineFunctions[0].Startup
	if st.CPUMode != "custom" || st.CPUModel != "Skylake-Client" {
		t.Fatalf("startup cpu_mode/cpu_model 未落平铺字段：%+v", st)
	}
}
