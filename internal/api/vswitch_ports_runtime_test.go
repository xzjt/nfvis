package api

// 决策 #444（收口 R7-2）：交换机成员端口读视图的运行态列三面同源。
//
// 由来（round7 真机登记）：内核数据面下 Web 交换机详情页的容器派生端口（nfvisct…）
// 运行态列显示「—」，而同刻 CLI `show virtual-switches <n> ports` 同一行显示 up/up——
// 运行态列此前只在 CLI 的渲染里按展示名合并，REST `/ports` 只发配置派生行，Web 只能改从
// `statistics.ports` 合并，而该列表按决策 #441 有意隐藏产品自持的容器宿主端 veth。
//
// 本文件守护三件事：① 共享解析器 resolveSwitchPortRuntime 的四态判定（与 CLI 改前逐字一致）；
// ② REST `/ports` 逐行补运行态列（有则出现、取不到不出现该键——条件字段不编造）；
// ③ CLI 结构化输出键/类型与改前一致（同键同类型：admin/link 布尔、rx_packets/tx_packets 数字）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

// boolRef / u64Ref 取地址（表驱动断言用）。
func boolRef(v bool) *bool    { return &v }
func u64Ref(v uint64) *uint64 { return &v }

// assertPortRuntimePtr 断言运行态列指针：都 nil（取不到）或都非 nil 且值相符。
func assertPortRuntimePtr[T comparable](t *testing.T, field string, got, want *T) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil:
		t.Errorf("%s：应取到 %v，实际 nil（取不到）", field, *want)
	case want == nil:
		t.Errorf("%s：应取不到（nil），实际 %v", field, *got)
	case *got != *want:
		t.Errorf("%s：应为 %v，实际 %v", field, *want, *got)
	}
}

// resolveSwitchPortRuntime 四态：有状态有计数 / 无状态有计数 / 有状态无计数 / counters == nil
// （外加 states == nil 不 panic）。两类事实独立判定——状态取不到不牵连计数，反之亦然。
func TestResolveSwitchPortRuntimeFourStates(t *testing.T) {
	states := map[string]InterfaceState{"ens1": {AdminUp: true, LinkUp: false}}
	countersOK := func(string) (state.InterfaceCounters, bool) {
		return state.InterfaceCounters{RxPackets: 42, TxPackets: 11}, true
	}
	countersMiss := func(string) (state.InterfaceCounters, bool) {
		return state.InterfaceCounters{}, false
	}
	cases := []struct {
		name     string
		label    string
		states   map[string]InterfaceState
		counters func(string) (state.InterfaceCounters, bool)
		admin    *bool
		link     *bool
		rx       *uint64
		tx       *uint64
	}{
		{"有状态有计数", "ens1", states, countersOK,
			boolRef(true), boolRef(false), u64Ref(42), u64Ref(11)},
		{"无状态有计数", "ghost", states, countersOK,
			nil, nil, u64Ref(42), u64Ref(11)},
		{"有状态无计数", "ens1", states, countersMiss,
			boolRef(true), boolRef(false), nil, nil},
		{"counters 为 nil", "ens1", states, nil,
			boolRef(true), boolRef(false), nil, nil},
		{"states 为 nil 不 panic", "ens1", nil, nil, nil, nil, nil, nil},
	}
	for _, tc := range cases {
		rt := resolveSwitchPortRuntime(tc.label, tc.states, tc.counters)
		assertPortRuntimePtr(t, tc.name+".AdminUp", rt.AdminUp, tc.admin)
		assertPortRuntimePtr(t, tc.name+".LinkUp", rt.LinkUp, tc.link)
		assertPortRuntimePtr(t, tc.name+".RxPackets", rt.RxPackets, tc.rx)
		assertPortRuntimePtr(t, tc.name+".TxPackets", rt.TxPackets, tc.tx)
	}
}

// seedSwitchPortsFixture 提交端口读视图夹具（switchPortsFixture）；dataPlane 非空时改数据面。
func seedSwitchPortsFixture(t *testing.T, ts *httptest.Server, token, dataPlane string) {
	t.Helper()
	cfg := switchPortsFixture()
	if dataPlane != "" {
		cfg.System.DataPlane = dataPlane
	}
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		cfg, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交端口夹具配置: %d %s", status, data)
	}
}

// getVSwitchPortsRows GET /virtual-switches/{name}/ports 并解析为行表（按展示名索引）。
func getVSwitchPortsRows(t *testing.T, ts *httptest.Server, token, name string) map[string]map[string]any {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/"+name+"/ports", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET ports: %d %s", status, data)
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("响应不是对象数组: %v %s", err, data)
	}
	byLabel := map[string]map[string]any{}
	for _, it := range items {
		if label, _ := it["port"].(string); label != "" {
			byLabel[label] = it
		}
	}
	return byLabel
}

// REST `/ports` 逐行合并运行态列（R7-2 的修复点）：内核数据面容器派生条目（宿主端 veth 名）
// 按展示名拿到 up/up 与计数；运行态里没有的名字**不出现该键**（不编造）；statistics 不覆盖的
// source=runtime 补条目同样被合并。
func TestGetVSwitchPortsMergesRuntimeColumns(t *testing.T) {
	host, _ := model.ContainerVethNames("ct-b", "m0") // 内核数据面容器派生条目的展示名＝veth 宿主端
	ts := newTestServerOpts(t, Options{
		State: state.New(&fakeStateRuntime{}), // 计数源：任意口 42/11
		VppState: fakeVppState{
			bds: []BridgeDomainState{{ID: 9, Name: "vs-a",
				Ports: []BridgeDomainPort{{Name: "ens300", SwIfIndex: 7}}}},
			ifs: map[string]InterfaceState{
				host:     {AdminUp: true, LinkUp: true},
				"ens300": {AdminUp: true, LinkUp: false},
			},
		},
	})
	token := loginAdmin(t, ts)
	seedSwitchPortsFixture(t, ts, token, model.DataPlaneKernel)
	rows := getVSwitchPortsRows(t, ts, token, "vs-a")

	// ① 派生容器条目（宿主端名）⇒ 运行态列与计数都在（#441 的宿主端过滤只影响 statistics
	//    端点，不影响本端点——这正是 R7-2 的修复点）。
	ct := rows[host]
	if ct == nil {
		t.Fatalf("派生容器条目 %s 不在响应里（应逐行带运行态列）: %+v", host, rows)
	}
	if ct["source"] != model.PortSourceContainer {
		t.Fatalf("%s 的来源应为 container: %+v", host, ct)
	}
	if ct["admin"] != true || ct["link"] != true {
		t.Fatalf("%s 的运行态列应为 up/up（按展示名合并），得到 admin=%v link=%v", host, ct["admin"], ct["link"])
	}
	if ct["rx_packets"] != float64(42) || ct["tx_packets"] != float64(11) {
		t.Fatalf("计数应取 state（42/11），得到 %v/%v", ct["rx_packets"], ct["tx_packets"])
	}

	// ② 运行态里没有的名字（静态口 ens224）⇒ 状态键**缺席**（不写零值、不编造）；
	//    计数独立于状态合并，仍在（与 CLI 改前两个 if 独立判定一致）。
	static := rows["ens224"]
	if static == nil {
		t.Fatalf("静态口 ens224 不在响应里: %+v", rows)
	}
	if _, ok := static["admin"]; ok {
		t.Fatalf("运行态里没有 ens224 时不得出现 admin 键（不编造，缺席即如实）: %+v", static)
	}
	if _, ok := static["link"]; ok {
		t.Fatalf("运行态里没有 ens224 时不得出现 link 键: %+v", static)
	}
	if static["rx_packets"] != float64(42) {
		t.Fatalf("计数应独立于状态合并（42），得到 %v", static["rx_packets"])
	}

	// ③ statistics 不覆盖的 source=runtime 补条目同样合并运行态列。
	runtimeRow := rows["ens300"]
	if runtimeRow == nil || runtimeRow["source"] != model.PortSourceRuntime {
		t.Fatalf("运行态补条目 ens300 应在响应里（source=runtime）: %+v", rows)
	}
	if runtimeRow["admin"] != true || runtimeRow["link"] != false {
		t.Fatalf("ens300 应合并 up/down，得到 admin=%v link=%v", runtimeRow["admin"], runtimeRow["link"])
	}
}

// 计数源未装配（State 为 nil）⇒ admin/link 照常合并，rx_packets/tx_packets **不出现该键**
// （条件字段，不编造）。
func TestGetVSwitchPortsOmitsCounterKeysWithoutCounters(t *testing.T) {
	host, _ := model.ContainerVethNames("ct-b", "m0")
	ts := newTestServerOpts(t, Options{
		VppState: fakeVppState{ifs: map[string]InterfaceState{host: {AdminUp: true, LinkUp: true}}},
	}) // 不注入 State：计数源未装配
	token := loginAdmin(t, ts)
	seedSwitchPortsFixture(t, ts, token, model.DataPlaneKernel)
	rows := getVSwitchPortsRows(t, ts, token, "vs-a")

	ct := rows[host]
	if ct == nil {
		t.Fatalf("派生容器条目 %s 不在响应里: %+v", host, rows)
	}
	if ct["admin"] != true || ct["link"] != true {
		t.Fatalf("计数源未装配不应牵连状态列（应 up/up），得到 admin=%v link=%v", ct["admin"], ct["link"])
	}
	if _, ok := ct["rx_packets"]; ok {
		t.Fatalf("计数源未装配时不得出现 rx_packets 键（不编造）: %+v", ct)
	}
	if _, ok := ct["tx_packets"]; ok {
		t.Fatalf("计数源未装配时不得出现 tx_packets 键（不编造）: %+v", ct)
	}
	// 四个运行态字段是**契约声明的条件字段**：shape 守护的白名单须登记（取不到不出现该键、
	// 不编造）——字段名改动而漏改白名单/契约时，这里与 shape 守护一起报。
	allow := shapeConditional["GET /virtual-switches/{name}/ports"]
	for _, f := range []string{"admin", "link", "rx_packets", "tx_packets"} {
		if allow[f] == "" {
			t.Errorf("shape 条件字段白名单未登记 %s（决策 #444：运行态取不到时不出现该键）", f)
		}
	}
}

// CLI 结构化输出逐键不变（决策 #444 的「CLI 形状不动」）：`show virtual-switches <n> ports`
// 的 x.structured 仍是 {name, ports:[{port, source, ...}]}，状态存在时 admin/link 是**布尔**、
// rx_packets/tx_packets 是**数字**；运行态里没有的行不出现状态键（不编造）。
func TestShowVSwitchPortsStructuredShapeUnchanged(t *testing.T) {
	x, _ := newCLIKit(t)
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
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{
		"vh-fw-vm-eth0": {AdminUp: true, LinkUp: true},
	}})
	x.setRuntime(nil, state.New(&fakeStateRuntime{}))

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-a ports").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("show 失败:\n%s", out)
	}
	rowFields := func(label string) []string {
		for _, l := range strings.Split(out, "\n") {
			if fs := strings.Fields(l); len(fs) > 0 && fs[0] == label {
				return fs
			}
		}
		return nil
	}
	// 文本列：有运行态的行 up/up；运行态里没有的行仍是缺省 "-"（不编造）。
	if fs := rowFields("vh-fw-vm-eth0"); fs == nil || len(fs) < 4 || fs[2] != "up" || fs[3] != "up" {
		t.Fatalf("VNF 派生行应有 up/up（运行态按展示名合并）: %v\n%s", fs, out)
	}
	if fs := rowFields("ens224"); fs == nil || len(fs) < 4 || fs[2] != "-" || fs[3] != "-" {
		t.Fatalf("运行态里没有的行状态列应为 '-'（不编造）: %v\n%s", fs, out)
	}

	// 结构化输出：键集合与类型逐项核对（admin/link 布尔、rx_packets/tx_packets 数字）。
	got, ok := x.structured.(map[string]any)
	if !ok {
		t.Fatalf("structured 输出应为 map，得到 %T", x.structured)
	}
	if got["name"] != "vs-a" {
		t.Fatalf("structured.name 应为 vs-a，得到 %v", got["name"])
	}
	ports, ok := got["ports"].([]any)
	if !ok || len(ports) != 2 {
		t.Fatalf("structured.ports 应为 2 条（静态 + VNF 派生），得到 %#v", got["ports"])
	}
	byLabel := map[string]map[string]any{}
	for _, raw := range ports {
		row, _ := raw.(map[string]any)
		if row == nil || row["port"] == nil || row["source"] == nil {
			t.Fatalf("结构化行的 port/source 键必须存在: %#v", raw)
		}
		byLabel[row["port"].(string)] = row
	}
	vnf := byLabel["vh-fw-vm-eth0"]
	if vnf == nil {
		t.Fatalf("结构化输出缺 VNF 派生行: %#v", ports)
	}
	if a, ok := vnf["admin"].(bool); !ok || !a {
		t.Fatalf("admin 必须是布尔 true（与改前同键同类型），得到 %#v", vnf["admin"])
	}
	if l, ok := vnf["link"].(bool); !ok || !l {
		t.Fatalf("link 必须是布尔 true（与改前同键同类型），得到 %#v", vnf["link"])
	}
	if rx, ok := vnf["rx_packets"].(uint64); !ok || rx != 42 {
		t.Fatalf("rx_packets 必须是 uint64 42（与改前同键同类型），得到 %#v", vnf["rx_packets"])
	}
	if tx, ok := vnf["tx_packets"].(uint64); !ok || tx != 11 {
		t.Fatalf("tx_packets 必须是 uint64 11，得到 %#v", vnf["tx_packets"])
	}
	static := byLabel["ens224"]
	if static == nil {
		t.Fatalf("结构化输出缺静态行: %#v", ports)
	}
	if _, exists := static["admin"]; exists {
		t.Fatalf("运行态里没有 ens224 时不得出现 admin 键（不编造）: %#v", static)
	}
	if _, exists := static["link"]; exists {
		t.Fatalf("运行态里没有 ens224 时不得出现 link 键（不编造）: %#v", static)
	}
}
