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
	"encoding/json"
	"net/http"
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

// TestDisplaySetVMStartupEndToEnd（决策 #485，Task 7）：真实 Execute 路径下
// `show configuration | display set` 能反推出镜像画像与 startup 覆盖的等价语句
// （cpu 平铺字段 → `cpu mode`/`cpu model` 两层语句）。
func TestDisplaySetVMStartupEndToEnd(t *testing.T) {
	x, _ := newCLIKit(t)
	exec := func(line string) CLIEResult {
		t.Helper()
		return x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
	}
	for _, line := range []string{
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set vm-image-profiles img1 machine pc",
		"set vm-image-profiles img1 cpu mode custom",
		"set vm-image-profiles img1 cpu model Skylake-Client",
		"set virtual-machine-functions v1 image img1",
		"set virtual-machine-functions v1 vcpu count 2",
		"set virtual-machine-functions v1 memory size-mb 1024",
		"set virtual-machine-functions v1 startup cpu mode custom",
		"set virtual-machine-functions v1 startup cpu model Skylake-Client",
		"commit",
		"exit",
	} {
		res := exec(line)
		if strings.HasPrefix(res.Output, "%%") || strings.Contains(res.Output, "校验失败") {
			t.Fatalf("%s failed: %s", line, res.Output)
		}
	}
	res := exec("show configuration | display set")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("display set 不应报错:\n%s", res.Output)
	}
	for _, want := range []string{
		"set vm-image-profiles img1 machine pc",
		"set vm-image-profiles img1 cpu mode custom",
		"set vm-image-profiles img1 cpu model Skylake-Client",
		"set virtual-machine-functions v1 startup cpu mode custom",
		"set virtual-machine-functions v1 startup cpu model Skylake-Client",
	} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("display set 缺少 %q:\n%s", want, res.Output)
		}
	}
}

// TestShowVMImageProfilesAndEffective（决策 #485，Task 12）：镜像画像读视图（CLI）
// 与 VNF 详情的「有效启动画像」块——逐项标来源（实例覆盖 > 镜像画像 > 产品默认）。
//
// 三面同源：`show vm-image-profiles` 与 REST `GET /vm-image-profiles`（TestGetVMImageProfilesEndpoint）
// 都读 committed 配置；VNF 详情的有效画像由 `model.EffectiveStartup` 合并得出（与域映射同一函数）。
func TestShowVMImageProfilesAndEffective(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set vm-image-profiles img1 machine pc",
		"set vm-image-profiles img1 firmware uefi",
		"set virtual-machine-functions v1 image img1",
		"set virtual-machine-functions v1 vcpu count 1",
		"set virtual-machine-functions v1 memory size-mb 1024",
		"set virtual-machine-functions v1 startup machine q35", // 实例覆盖（覆盖画像的 machine=pc）
		"commit",
		"exit",
	)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vm-image-profiles").Output
	if !strings.Contains(out, "img1") || !strings.Contains(out, "pc") {
		t.Fatalf("show vm-image-profiles wrong: %s", out)
	}
	if !strings.Contains(out, "uefi") {
		t.Fatalf("show vm-image-profiles 应显示 firmware 列: %s", out)
	}

	// 单画像形态：`show vm-image-profiles <image>` 只列该画像；不存在的画像如实报错。
	single := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vm-image-profiles img1").Output
	if !strings.Contains(single, "img1") {
		t.Fatalf("show vm-image-profiles img1 应列出该画像: %s", single)
	}
	missing := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vm-image-profiles nope").Output
	if !strings.HasPrefix(missing, "%%") {
		t.Fatalf("不存在的画像应如实报错: %s", missing)
	}

	det := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions v1 detail").Output
	if !strings.Contains(det, "有效启动画像") || !strings.Contains(det, "uefi") {
		t.Fatalf("detail effective profile missing: %s", det)
	}
	// 逐项来源：machine/firmware 来自画像；firmware 被实例覆盖；accel 未声明 → 产品默认。
	for _, want := range []string{"镜像画像", "实例覆盖", "产品默认"} {
		if !strings.Contains(det, want) {
			t.Fatalf("有效启动画像应标出来源 %q:\n%s", want, det)
		}
	}
}

// TestGetVMImageProfilesEndpoint（决策 #485，Task 12）：REST `GET /vm-image-profiles`
// 返回 committed 配置里的镜像画像数组（与 CLI `show vm-image-profiles` 同源）。
func TestGetVMImageProfilesEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	patch := map[string]any{"vm_image_profiles": []map[string]any{{"image": "img1", "machine": "pc"}}}
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, patch,
		map[string]string{"X-NFVIS-Merge": "true", "X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("写画像: %d %s", status, data)
	}

	status, data := getWithToken(t, ts.URL+APIPrefix+"/vm-image-profiles", token)
	if status != http.StatusOK {
		t.Fatalf("GET /vm-image-profiles: %d %s", status, data)
	}
	var got []map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, data)
	}
	if len(got) != 1 || got[0]["image"] != "img1" || got[0]["machine"] != "pc" {
		t.Fatalf("画像列表不符: %s", data)
	}
}

// 决策 #485 契约 §4.4 / 用户手册 §9.6：读视图必须显式标注 extra-args「已绕过资源账本」——
// `show vm-image-profiles` 的表格列与 VNF 详情的「有效启动画像」块**共用**同一助手
// （vmProfileExtraArgs），保证两面标注一致。
func TestShowVMImageProfilesExtraArgsBypassNote(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		`set vm-image-profiles img1 extra-args "-global x"`,
		"set virtual-machine-functions v1 image img1",
		"set virtual-machine-functions v1 vcpu count 1",
		"set virtual-machine-functions v1 memory size-mb 1024",
		"commit",
		"exit",
	)
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vm-image-profiles").Output; !strings.Contains(out, "已绕过资源账本") {
		t.Fatalf("show vm-image-profiles 应标注已绕过资源账本: %s", out)
	}
	if det := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions v1 detail").Output; !strings.Contains(det, "已绕过资源账本") {
		t.Fatalf("有效启动画像块应标注已绕过资源账本: %s", det)
	}
}
