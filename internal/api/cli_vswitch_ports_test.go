package api

// 决策 #326（收口 R84-16 / v2 待做 二.8）：交换机成员端口**读视图**——配置里静态声明的
// `ports` 与 VNF/容器声明（`interfaces <nic> virtual-switch <vs>`）派生出的 vNIC 成员并集，
// 逐条带 source（config|vnf|container）。三面同源：CLI `show virtual-switches <n> ports`、
// REST `GET /virtual-switches/{n}/ports`、Web 交换机详情页（消费 REST）都按同一份派生结果；
// 派生条目只读，`delete … ports` 对派生条目的删除被拒并指向 VNF/容器侧。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// switchPortsFixture 一份可提交的配置：一台 L2 交换机（静态口 ens224）+ 一个 VNF 的 vNIC
// 声明挂到它（**不在** vs.Ports）+ 一个容器的 memif 声明挂到它（同样不在 vs.Ports）。
func switchPortsFixture() model.Config {
	return withSuperUser(model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 8}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
		Interfaces: []model.InterfaceConfig{{Name: "ens224"}},
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-a", Type: "l2",
			Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens224"}},
		}},
		VirtualMachineFunctions: []model.VMFunction{{
			Name: "fw-vm", Image: "base.qcow2",
			VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 512, HugepageSize: "1G"},
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-a"}},
		}},
		ContainerFunctions: []model.ContainerFunction{{
			Name: "ct-b", Image: "alpine:3.20",
			Interfaces: []model.VnfInterface{{Name: "m0", Type: "memif", VirtualSwitch: "vs-a"}},
		}},
	})
}

// REST 读视图：静态口 + VNF/容器派生，逐条带 source；且与 model.DerivedSwitchPorts 逐字一致。
func TestGetVSwitchPortsDerivedUnionREST(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		switchPortsFixture(), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交配置: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-a/ports", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET ports: %d %s", status, data)
	}
	var got []model.SwitchPortView
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("响应不是 SwitchPortView 数组: %v %s", err, data)
	}
	bySource := map[string]int{}
	for _, p := range got {
		bySource[p.Source]++
	}
	if bySource[model.PortSourceConfig] != 1 || bySource[model.PortSourceVNF] != 1 || bySource[model.PortSourceContainer] != 1 {
		t.Fatalf("应按来源各出 1 条（config/vnf/container），实得 %v（%+v）", bySource, got)
	}
	var sawStatic, sawVNF, sawCT bool
	for _, p := range got {
		switch p.Source {
		case model.PortSourceConfig:
			sawStatic = p.Interface == "ens224" && p.Seq == 1
		case model.PortSourceVNF:
			sawVNF = p.VNF == "fw-vm" && p.VNFInterface == "eth0"
		case model.PortSourceContainer:
			sawCT = p.Container == "ct-b" && p.ContainerInterface == "m0"
		}
	}
	if !sawStatic || !sawVNF || !sawCT {
		t.Fatalf("读视图内容不符 static=%v vnf=%v container=%v：%+v", sawStatic, sawVNF, sawCT, got)
	}

	// 与派生实现的**同源**证明：REST 响应 == model.DerivedSwitchPorts(committed)。
	status, _, cdata := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration: %d %s", status, cdata)
	}
	var wrap struct {
		Configuration model.Config `json:"configuration"`
	}
	if err := json.Unmarshal(cdata, &wrap); err != nil {
		t.Fatalf("解析配置: %v", err)
	}
	want, _ := json.Marshal(model.DerivedSwitchPorts(wrap.Configuration, "vs-a"))
	if string(want) != strings.TrimSpace(string(data)) {
		t.Fatalf("REST 读视图应与 model.DerivedSwitchPorts 同源:\n  REST: %s\n  want: %s", data, want)
	}
}

// 交换机未在配置中声明 → 404（不把派生条目单独发出来，与其它详情端点同口径）。
func TestGetVSwitchPortsUnknownSwitch404(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/nope/ports", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("未知交换机应 404，实得 %d", status)
	}
}

// CLI 读视图：派生条目带来源与「去 VNF 侧删」的指引；VNF 侧增删后随之变化；空态如实说明。
func TestShowVSwitchPortsDerivedUnionCLI(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set interfaces ens224",
		"set virtual-switches vs-a type l2",
		"set virtual-switches vs-a ports 1 interface ens224",
		"set virtual-machine-functions fw-vm image base.qcow2",
		"set virtual-machine-functions fw-vm vcpu count 1",
		"set virtual-machine-functions fw-vm memory size-mb 512",
		"set virtual-machine-functions fw-vm interfaces eth0 type vhost-user",
		"set virtual-machine-functions fw-vm interfaces eth0 virtual-switch vs-a",
		"commit",
		"exit",
	)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-a ports").Output
	for _, want := range []string{"ens224", "config", "vh-fw-vm-eth0", "vnf"} {
		if !strings.Contains(out, want) {
			t.Fatalf("读视图应含 %q（静态口 + VNF 派生 + 来源）:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "delete virtual-machine-functions fw-vm interfaces eth0 virtual-switch vs-a") {
		t.Fatalf("派生条目应给出去 VNF 侧删除的指引:\n%s", out)
	}

	// 配置库形状不变：VNF 声明**没有**被物化进 vs.Ports。
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatal(err)
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == "vs-a" && len(vs.Ports) != 1 {
			t.Fatalf("配置库 vs.Ports 不应被派生条目改变（应仍为 1 条），实得 %+v", vs.Ports)
		}
	}

	// VNF 侧增一个 vNIC → 读视图随之变化；删掉 eth0 的挂接 → 派生条目消失。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set virtual-machine-functions fw-vm interfaces eth1 type vhost-user",
		"set virtual-machine-functions fw-vm interfaces eth1 virtual-switch vs-a",
		"commit",
		"exit",
	)
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-a ports").Output
	if !strings.Contains(out, "vh-fw-vm-eth1") {
		t.Fatalf("新增 vNIC 后读视图应含 vh-fw-vm-eth1:\n%s", out)
	}
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"delete virtual-machine-functions fw-vm interfaces eth0 virtual-switch vs-a",
		"commit",
		"exit",
	)
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-a ports").Output
	if strings.Contains(out, "vh-fw-vm-eth0") {
		t.Fatalf("删除 VNF 侧挂接后不该再有派生条目:\n%s", out)
	}
	if !strings.Contains(out, "vh-fw-vm-eth1") || !strings.Contains(out, "ens224") {
		t.Fatalf("静态口与其余派生条目不受影响:\n%s", out)
	}
}

// 空端口不报错：交换机在配置里、但既无静态 ports 也无派生成员 → 表头 + 如实说明。
func TestShowVSwitchPortsEmptyCLI(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set virtual-switches vs-empty type l2", "commit", "exit",
	)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-empty ports").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("空端口不应报错:\n%s", out)
	}
	if !strings.Contains(out, "无成员端口") {
		t.Fatalf("空端口应如实说明:\n%s", out)
	}
	if !strings.Contains(out, "RxPkts") {
		t.Fatalf("即便无端口也应打印状态/计数列表头:\n%s", out)
	}
}

// 删除边界：派生端口不可在交换机侧删（给出 VNF 侧指引），且不得凭空建空端口元素。
func TestDeleteDerivedSwitchPortRefused(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set virtual-switches vs-a type l2",
		"set virtual-machine-functions fw-vm image base.qcow2",
		"set virtual-machine-functions fw-vm vcpu count 1",
		"set virtual-machine-functions fw-vm memory size-mb 512",
		"set virtual-machine-functions fw-vm interfaces eth0 type vhost-user",
		"set virtual-machine-functions fw-vm interfaces eth0 virtual-switch vs-a",
		"commit",
	)
	// 序号 9 不在静态 ports 里，但 fw-vm 确有此 vNIC 挂到 vs-a → 拒绝并指向 VNF 侧。
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete virtual-switches vs-a ports 9 vnf fw-vm").Output
	if !strings.Contains(out, "%%") || !strings.Contains(out, "VNF 侧") {
		t.Fatalf("删派生端口应被拒并给指引:\n%s", out)
	}
	// 不得凭空建空端口（此前 portElem 会建 {"seq":9} 空壳）。
	if out2 := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-a ports").Output; strings.Contains(out2, "9") {
		t.Fatalf("被拒的删除不得留下空端口元素:\n%s", out2)
	}
	// 无 vNIC 声明的陌生 VM → 如实报「序号不存在」而非编造指引。
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete virtual-switches vs-a ports 9 vnf other-vm").Output
	if !strings.Contains(out, "%%") || !strings.Contains(out, "不存在") {
		t.Fatalf("删不存在的静态端口应如实报错:\n%s", out)
	}
	// 配置里不得出现 seq=9 的空端口。
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatal(err)
	}
	for _, vs := range cfg.VirtualSwitches {
		for _, p := range vs.Ports {
			if p.Seq == 9 {
				t.Fatalf("被拒的删除污染了配置（出现空端口元素）: %+v", p)
			}
		}
	}
}

// 形状守护（决策 #326）：GET /virtual-switches/{name}/ports 的响应必须逐条带**非空 source**，
// 且契约声明了它。
//
// 与其它端点的「逐字段必现」守护不同，本端点**有意不逐字段必现**——白名单与理由如下：
//   - seq：只有配置静态端口有（vnf/container/runtime 派生条目无序号）；
//   - vnf/vnf_interface 与 container/container_interface：按来源二选一，另一组必然缺席；
//   - trunk/native/acl_in/acl_out：未配置时省略。
//
// 故这里只守护「来源标注」这一读视图新增的、语义上必然存在的字段；照契约开发的前端据此
// 区分静态声明与派生条目。
func TestSwitchPortsResponseShapeMatchesContract(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		switchPortsFixture(), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交配置: %d %s", status, data)
	}
	spec := loadEmbeddedSpec(t)
	props := declaredProps(t, spec, "/virtual-switches/{name}/ports", "GET")
	hasSource := false
	for _, p := range props {
		if p == "source" {
			hasSource = true
		}
	}
	if !hasSource {
		t.Fatalf("契约未声明 source（读视图来源字段）——照契约开发的前端取不到它：%v", props)
	}

	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-a/ports", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET ports: %d %s", status, data)
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("响应不是对象数组: %v %s", err, data)
	}
	if len(items) == 0 {
		t.Fatal("响应为空数组——什么都验不到（应先种对象）")
	}
	valid := map[string]bool{model.PortSourceConfig: true, model.PortSourceVNF: true,
		model.PortSourceContainer: true, model.PortSourceRuntime: true}
	for i, it := range items {
		src, _ := it["source"].(string)
		if src == "" {
			t.Errorf("第 %d 条缺 source（读视图必须逐条标注来源）: %+v", i, it)
			continue
		}
		if !valid[src] {
			t.Errorf("第 %d 条 source=%q 不在枚举 config|vnf|container|runtime 内", i, src)
		}
	}
}
