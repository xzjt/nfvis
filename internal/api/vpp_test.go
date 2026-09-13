package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
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
