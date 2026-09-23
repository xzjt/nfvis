package api

// 增量 3 剩余缺口 ①（决策 #124）：VS / VM 的**详情**端点必须附带运行态 `statistics` 字段，
// 且与 CLI 的 `show virtual-switches <name> statistics` / `show virtual-machine-functions
// <name> statistics` **同源**（BD 与接口状态取 VppStateRuntime、计数取 state）。
// 运行态未接入、或对象不在数据面时**不返回该字段**——不退回配置视图（决策 #84 的口径）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

// vsStatsServer 造一台带运行态的服务器：配置里有 vs-dmz，VPP 里有同名 BD + 两个成员口。
func vsStatsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := newTestServerOpts(t, Options{
		State: state.New(&fakeStateRuntime{}),
		VppState: fakeVppState{
			bds: []BridgeDomainState{{
				ID: 6252701, Name: "vs-dmz",
				Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}, {Name: "vh-vm1-eth0", SwIfIndex: 3}},
			}},
			ifs: map[string]InterfaceState{
				"ens192":      {AdminUp: true, LinkUp: true},
				"vh-vm1-eth0": {AdminUp: true, LinkUp: false},
			},
		},
	})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
		model.VirtualSwitch{Name: "vs-dmz", Type: "l2"}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("建虚拟交换机: %d %s", status, data)
	}
	return ts, token
}

// 详情带 statistics：bd_id 与逐口计数（计数与 fakeStateRuntime 的固定值一致），
// 且 admin/link 取自运行态（vh-vm1-eth0 在 fake 里 link=false）。
func TestVSwitchDetailIncludesStatistics(t *testing.T) {
	ts, token := vsStatsServer(t)

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-dmz", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 详情: %d %s", status, data)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, data)
	}
	// 配置字段仍在（追加式，不是替换）
	if got["name"] != "vs-dmz" || got["type"] != "l2" {
		t.Fatalf("配置字段丢失: %s", data)
	}
	st, ok := got["statistics"].(map[string]any)
	if !ok {
		t.Fatalf("详情响应缺 statistics 字段: %s", data)
	}
	if bd, _ := st["bd_id"].(float64); int(bd) != 6252701 {
		t.Fatalf("bd_id 应为 6252701，得到 %v", st["bd_id"])
	}
	ports, _ := st["ports"].([]any)
	if len(ports) != 2 {
		t.Fatalf("成员口应为 2 个，得到 %d（%s）", len(ports), data)
	}
	first, _ := ports[0].(map[string]any)
	if first["port"] != "ens192" || first["sw_if_index"].(float64) != 1 {
		t.Fatalf("第一口应为 ens192/1，得到 %v", first)
	}
	if first["admin"] != true || first["link"] != true {
		t.Fatalf("ens192 运行态应 up/up，得到 admin=%v link=%v", first["admin"], first["link"])
	}
	if first["rx_packets"].(float64) != 42 || first["tx_packets"].(float64) != 11 {
		t.Fatalf("计数应取 state（42/11），得到 %v/%v", first["rx_packets"], first["tx_packets"])
	}
	second, _ := ports[1].(map[string]any)
	if second["link"] != false {
		t.Fatalf("vh-vm1-eth0 运行态 link 应为 false（取自运行态而非配置），得到 %v", second["link"])
	}

	// 列表端点**不带** statistics（契约：仅详情附带；列表不该逐台查计数）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 列表: %d %s", status, data)
	}
	if strings.Contains(string(data), "statistics") {
		t.Fatalf("列表端点不应带 statistics: %s", data)
	}
}

// 运行态未接入 → 不返回 statistics（不编造），配置字段照旧。
func TestVSwitchDetailOmitsStatisticsWithoutRuntime(t *testing.T) {
	ts := newTestServer(t) // 不注入 VppState / State
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
		model.VirtualSwitch{Name: "vs-dmz", Type: "l2"}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("建虚拟交换机: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-dmz", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 详情: %d %s", status, data)
	}
	if strings.Contains(string(data), "statistics") {
		t.Fatalf("运行态未接入时不应有 statistics: %s", data)
	}
	if !strings.Contains(string(data), "vs-dmz") {
		t.Fatalf("配置字段仍应返回: %s", data)
	}
}

// VM 详情带 statistics：只含 vhost-user vNIC，计数取 state（42/11/4200），
// 接口名按 `vh-<vm>-<vnic>` 组装（与 CLI 同源）。
func TestVMDetailIncludesStatistics(t *testing.T) {
	ts := newTestServerOpts(t, Options{State: state.New(&fakeStateRuntime{})})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token) // VM 校验要求大页池与隔离核
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f0", token,
		map[string]any{"name": "ens2f0"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("声明物理口: %d %s", status, data)
	}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
		model.VirtualSwitch{Name: "vs-dmz", Type: "l2"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建虚拟交换机: %d %s", status, data)
	}
	vm := model.VMFunction{
		Name: "vm1", Image: "img.qcow2",
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 512, HugepageSize: "1G"},
		Interfaces: []model.VnfInterface{
			{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-dmz"},
			{Name: "eth1", Type: "sriov-vf", Sriov: &model.SriovBind{PhysicalInterface: "ens2f0", VFID: 0}}, // 非 vhost-user：不进 statistics
		},
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vm, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}

	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/vm1", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 详情: %d %s", status, data)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, data)
	}
	rows, ok := got["statistics"].([]any)
	if !ok {
		t.Fatalf("详情响应缺 statistics 字段: %s", data)
	}
	if len(rows) != 1 {
		t.Fatalf("应只有 1 个 vhost-user vNIC 的统计，得到 %d（%s）", len(rows), data)
	}
	row, _ := rows[0].(map[string]any)
	if row["vnic"] != "eth0" || row["interface"] != "vh-vm1-eth0" {
		t.Fatalf("vNIC 行应为 eth0 / vh-vm1-eth0，得到 %v", row)
	}
	if row["available"] != true {
		t.Fatalf("available 应为 true，得到 %v", row["available"])
	}
	if row["rx_packets"].(float64) != 42 || row["rx_bytes"].(float64) != 4200 {
		t.Fatalf("计数应取 state（42/4200），得到 %v/%v", row["rx_packets"], row["rx_bytes"])
	}

	// 列表端点不带 statistics
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 列表: %d %s", status, data)
	}
	if strings.Contains(string(data), "statistics") {
		t.Fatalf("列表端点不应带 statistics: %s", data)
	}
}

// 运行态未接入 → VM 详情不返回 statistics（state 仍照常给）。
func TestVMDetailOmitsStatisticsWithoutRuntime(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	vm := model.VMFunction{
		Name: "vm1", Image: "img.qcow2",
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 512, HugepageSize: "1G"},
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vm, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/vm1", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 详情: %d %s", status, data)
	}
	if strings.Contains(string(data), "statistics") {
		t.Fatalf("运行态未接入时不应有 statistics: %s", data)
	}
}
