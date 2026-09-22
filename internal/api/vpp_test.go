package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

// ---------- M3-2：/vpp/status 与 /vpp/restart（FR-SYS-007/009） ----------

type fakeVppController struct {
	restarted  bool
	restartErr error
	status     VppStatus
}

func (f *fakeVppController) Status(*model.VppConfig) VppStatus { return f.status }
func (f *fakeVppController) Restart(context.Context, *model.VppConfig) error {
	f.restarted = true
	return f.restartErr
}
func (f *fakeVppController) Version() string { return f.status.Version }

func TestVppStatusEndpoint(t *testing.T) {
	fake := &fakeVppController{status: VppStatus{
		Version: "26.06-release", Connected: true, PendingRestart: true}}
	ts := newTestServerOpts(t, Options{VPP: fake})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/status", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /vpp/status: %d %s", status, data)
	}
	var got VppStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if got.Version != "26.06-release" || !got.Connected || !got.PendingRestart {
		t.Fatalf("状态不符: %+v", got)
	}
}

func TestVppRestartEndpoint(t *testing.T) {
	fake := &fakeVppController{}
	ts := newTestServerOpts(t, Options{VPP: fake})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vpp/restart", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("POST /vpp/restart: %d %s", status, data)
	}
	if !fake.restarted {
		t.Fatalf("应调用控制器 Restart")
	}
}

// read-only 用户不可重启（super-user 限定）。
func TestVppRestartForbiddenForViewer(t *testing.T) {
	fake := &fakeVppController{}
	ts := newTestServerOpts(t, Options{VPP: fake})
	token := loginViewer(t, ts)

	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vpp/restart", token, nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only 应 403: %d", status)
	}
	if fake.restarted {
		t.Fatalf("403 不应触发 Restart")
	}
}

// 未装配编排器时返回 503。
func TestVppEndpointsUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/status", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}

// ---------- M3-3：/virtual-switches/{name}/mac-table（FR-NET-015） ----------

type fakeL2Runtime struct {
	rows []MACTableRow
	err  error
}

func (f *fakeL2Runtime) MACTable(context.Context, string) ([]MACTableRow, error) {
	return f.rows, f.err
}

func TestMacTableEndpoint(t *testing.T) {
	fake := &fakeL2Runtime{rows: []MACTableRow{{MAC: "00:11:22:33:44:55", Port: "ens192", VLAN: 0}}}
	ts := newTestServerOpts(t, Options{L2: fake})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet,
		ts.URL+APIPrefix+"/virtual-switches/vs-app/mac-table", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "00:11:22:33:44:55") {
		t.Fatalf("mac-table: %d %s", status, data)
	}
}

func TestMacTableUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet,
		ts.URL+APIPrefix+"/virtual-switches/vs-app/mac-table", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}

// ---------- M3-4：/vrfs/{name}/routes 运行态（FR-NET-013） ----------

type fakeL3Runtime struct {
	rows []RouteRow
	err  error
}

func (f *fakeL3Runtime) Routes(context.Context, string) ([]RouteRow, error) { return f.rows, f.err }

func TestVrfRoutesEndpoint(t *testing.T) {
	fake := &fakeL3Runtime{rows: []RouteRow{{Prefix: "0.0.0.0/0", NextHop: "10.0.0.254"}}}
	ts := newTestServerOpts(t, Options{L3: fake})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vrfs/vs-l3/routes", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "0.0.0.0/0") {
		t.Fatalf("routes: %d %s", status, data)
	}
}

func TestVrfRoutesUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vrfs/vs-l3/routes", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}

// ---------- M3-6：/protocols/lldp/neighbors（FR-NET-018） ----------

type fakeLldpRuntime struct{ rows []LldpNeighborRow }

func (f *fakeLldpRuntime) Neighbors(context.Context) ([]LldpNeighborRow, error) { return f.rows, nil }

func TestLldpNeighborsEndpoint(t *testing.T) {
	fake := &fakeLldpRuntime{rows: []LldpNeighborRow{{Interface: "ens192", ChassisID: "sw1", PortID: "Gi0/1", TTL: 120}}}
	ts := newTestServerOpts(t, Options{LLDP: fake})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/protocols/lldp/neighbors", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "sw1") {
		t.Fatalf("lldp neighbors: %d %s", status, data)
	}
}

func TestLldpNeighborsUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/protocols/lldp/neighbors", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}

// ---------- M3-7：/vpp/status 运行态线程 + /vpp/config ----------

type fakeStateRuntime struct {
	rows []state.Thread
}

func (f *fakeStateRuntime) Threads(context.Context) ([]state.Thread, error) { return f.rows, nil }
func (f *fakeStateRuntime) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return state.InterfaceCounters{RxPackets: 42, TxPackets: 11, RxBytes: 4200}, true
}
func (f *fakeStateRuntime) Buffers(context.Context) (state.Buffers, bool) {
	return state.Buffers{Pools: []state.BufferPool{{Name: "default-numa-0", Used: 5, Available: 95}}}, true
}
func (f *fakeStateRuntime) Memory(context.Context) (state.Memory, bool) {
	return state.Memory{Total: 2048, Used: 512, Free: 1536}, true
}

func TestVppStatusIncludesThreads(t *testing.T) {
	fake := &fakeVppController{status: VppStatus{Version: "26.06-release", Connected: true}}
	st := state.New(&fakeStateRuntime{rows: []state.Thread{
		{ID: 0, Name: "vpp_main", Core: 4}, {ID: 1, Name: "vpp_wk_0", Type: "workers", Core: 5}}})
	ts := newTestServerOpts(t, Options{VPP: fake, State: st})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/status", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status: %d %s", status, data)
	}
	var got VppStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got.Threads) != 2 || got.Threads[1].Name != "vpp_wk_0" || got.Threads[1].Core != 5 {
		t.Fatalf("线程运行态: %+v", got.Threads)
	}
	if len(got.Buffers) != 1 || got.Buffers[0].Name != "default-numa-0" || got.Memory == nil || got.Memory.Used != 512 {
		t.Fatalf("buffer/内存运行态: %+v %+v", got.Buffers, got.Memory)
	}
}

// GET /interfaces/{name} 附带 statistics（契约 Interface.statistics）。
func TestInterfaceStatistics(t *testing.T) {
	st := state.New(&fakeStateRuntime{})
	ts := newTestServerOpts(t, Options{State: st})
	token := loginAdmin(t, ts)
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens192", token,
		model.InterfaceConfig{Name: "ens192"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("预置接口失败: %d", status)
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/interfaces/ens192", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "42") {
		t.Fatalf("接口统计: %d %s", status, data)
	}
}

func TestVppConfigEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	// 初始无 vpp 配置 → {}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/config", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "{") {
		t.Fatalf("vpp config: %d %s", status, data)
	}
}

// ---------- M3-7（三）：sriov PUT 与 NAT 会话端点 ----------

type fakeSRIOV struct {
	gotIf string
	gotN  int
	err   error
}

func (f *fakeSRIOV) SetVFCount(_ context.Context, ifname string, n int) error {
	f.gotIf, f.gotN = ifname, n
	return f.err
}

func TestPutSRIOVEndpoint(t *testing.T) {
	fake := &fakeSRIOV{}
	ts := newTestServerOpts(t, Options{SRIOV: fake})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens1f0/sriov", token,
		map[string]any{"vf_count": 4}, nil)
	if status != http.StatusOK || fake.gotN != 4 || fake.gotIf != "ens1f0" {
		t.Fatalf("sriov: %d %s (%+v)", status, data, fake)
	}
	// 缺 vf_count → 400
	status, _, _ = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens1f0/sriov", token,
		map[string]any{}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("缺 vf_count 应 400: %d", status)
	}
	// 后端错误 → 400（如不支持 SR-IOV）
	bad := &fakeSRIOV{err: errors.New("接口 ens1f0 不支持 SR-IOV")}
	ts2 := newTestServerOpts(t, Options{SRIOV: bad})
	token2 := loginAdmin(t, ts2)
	status, _, data = cfgRequest(t, http.MethodPut, ts2.URL+APIPrefix+"/interfaces/ens1f0/sriov", token2,
		map[string]any{"vf_count": 2}, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(data), "SR-IOV") {
		t.Fatalf("不支持应 400: %d %s", status, data)
	}
	// 未装配 → 503
	ts3 := newTestServer(t)
	token3 := loginAdmin(t, ts3)
	status, _, _ = cfgRequest(t, http.MethodPut, ts3.URL+APIPrefix+"/interfaces/ens1f0/sriov", token3,
		map[string]any{"vf_count": 1}, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}

type fakeNatSessions struct {
	rows []NatSessionRow
	err  error
}

func (f *fakeNatSessions) Sessions(context.Context) ([]NatSessionRow, error) { return f.rows, f.err }

func TestNatSessionsEndpoint(t *testing.T) {
	fake := &fakeNatSessions{rows: []NatSessionRow{{InsideIP: "10.0.0.5", InsidePort: 1234,
		OutsideIP: "203.0.113.1", OutsidePort: 5000, Protocol: 6, Packets: 3}}}
	ts := newTestServerOpts(t, Options{NAT: fake})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/nat/sessions", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "203.0.113.1") {
		t.Fatalf("nat sessions: %d %s", status, data)
	}
	// 未装配 → 503
	ts2 := newTestServer(t)
	token2 := loginAdmin(t, ts2)
	status, _, _ = cfgRequest(t, http.MethodGet, ts2.URL+APIPrefix+"/nat/sessions", token2, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}

// TestDeleteVFSExplainsCountSemantics（附录 A #94）：`delete-vfs` 收下 `vf <n>` 但**按数量**回收，
// 回显必须说明「编号不参与定位」——否则操作者会以为删的是自己指定的那一个（交互层的
// 「声明了但静默无效」）。真机无 PF/VF 无法走通该路径，故用注入的假 SRIOV 覆盖。
func TestDeleteVFSExplainsCountSemantics(t *testing.T) {
	x, _ := newCLIKit(t)
	fake := &fakeSRIOV{}
	x.setSRIOV(fake)
	// 先提交一条带 VF 数量的接口，再**退出配置模式**（request 是操作模式命令）
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens224 sriov vf-count 3",
		"commit",
		"exit",
	)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request sriov delete-vfs ens224 vf 1")
	if fake.gotIf != "ens224" || fake.gotN != 2 {
		t.Fatalf("应按数量回收（3→2）：gotIf=%q gotN=%d", fake.gotIf, fake.gotN)
	}
	for _, want := range []string{"按**数量**回收", "不参与定位", "vf 1"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("回显应说明 %q，实得: %q", want, res.Output)
		}
	}
}
