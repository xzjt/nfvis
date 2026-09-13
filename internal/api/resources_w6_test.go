package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- W6：网络配置层 handlers 第二组 ----------

// seedIface 预置物理口（引用校验的前置）。
func seedIface(t *testing.T, ts *httptest.Server, token string, name string) {
	t.Helper()
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/"+name, token,
		model.InterfaceConfig{Name: name},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("预置接口 %s: %d", name, status)
	}
}

func TestAclEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	acl := model.Acl{Name: "acl-web", Rules: []model.AclRule{{Seq: 10, Action: "permit"}}}
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/acls", token, acl,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建 ACL 应 201")
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/acls", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "acl-web") {
		t.Fatalf("ACL 列表: %d %s", status, data)
	}

	// 绑定到交换机网关后删除 → 409
	seedIface(t, ts, token, "ens2f0")
	vs := model.VirtualSwitch{Name: "vs-app", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"192.168.100.1/24"}, AclIn: "acl-web"}}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token, vs,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建交换机")
	}
	status, _, data = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/acls/acl-web", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusConflict || !strings.Contains(string(data), "网关") {
		t.Fatalf("被网关引用删除应 409: %d %s", status, data)
	}
}

func TestNatEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedIface(t, ts, token, "ens2f0")

	// L3 交换机（同名 VRF 承载数据，附录 B）
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token,
		model.Vrf{Name: "vs-l3"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 VRF")
	}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
		model.VirtualSwitch{Name: "vs-l3", Type: "l3"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 L3 交换机")
	}

	// 出接口须归属某 VRF 且带地址（NAT outside 转发域来源，决策 #52）
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token,
		model.Vrf{Name: "wan", L3Interfaces: []model.L3Interface{{Interface: "ens2f0", Addresses: []string{"203.0.113.1/24"}}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 wan VRF")
	}

	nat := model.NatConfig{Rules: []model.NatRule{{
		Seq: 1, MatchSource: "192.168.100.0/24", VirtualSwitch: "vs-l3",
		Action: model.NatAction{Interface: "ens2f0"},
	}}}
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/nat", token, nat,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT nat: %d", status)
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/nat", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "vs-l3") {
		t.Fatalf("NAT 回读: %d %s", status, data)
	}
}

func TestQosEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	pol := model.QosPolicy{Name: "pol-1", Cir: 1000000, Cbs: 100000}
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/qos/policies", token, pol,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建策略应 201")
	}

	// 接口绑定后删除 → 409
	seedIface(t, ts, token, "ens2f0")
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f0", token,
		model.InterfaceConfig{Name: "ens2f0", IngressPolicy: "pol-1"},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("绑定策略")
	}
	status, _, data := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/qos/policies/pol-1", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusConflict || !strings.Contains(string(data), "绑定") {
		t.Fatalf("被绑定删除应 409: %d %s", status, data)
	}
}

func TestPortMirroringEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedIface(t, ts, token, "ens2f0")
	seedIface(t, ts, token, "ens2f1")

	pm := model.PortMirroring{Name: "span1",
		Source:   model.PMSource{Interface: "ens2f0", Direction: "both"},
		Analyzer: "ens2f1"}
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/port-mirroring", token, pm,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建镜像会话应 201")
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/port-mirroring", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "span1") {
		t.Fatalf("镜像列表: %d %s", status, data)
	}
	status, _, _ = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/port-mirroring/span1", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("删除镜像会话应 200")
	}
}

func TestBondEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedIface(t, ts, token, "ens2f0")
	seedIface(t, ts, token, "ens2f1")

	bond := model.Bond{Name: "bond0", Members: []string{"ens2f0", "ens2f1"},
		Lacp: &model.Lacp{Mode: "active"}}
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/bonds", token, bond,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建 bond 应 201")
	}

	// 被交换机端口引用后删除 → 409（FR-NET-017：bond 名可在接口名处引用）
	vs := model.VirtualSwitch{Name: "vs-app", Type: "l2",
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "bond0"}}}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token, vs,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建交换机")
	}
	status, _, data := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/bonds/bond0", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusConflict || !strings.Contains(string(data), "引用") {
		t.Fatalf("被引用删除应 409: %d %s", status, data)
	}
}

func TestLldpEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedIface(t, ts, token, "ens2f0")

	lldp := model.LldpConfig{Enabled: true, AdvertisementInterval: 30,
		Interfaces: []model.LldpInterface{{Interface: "ens2f0", Enabled: true}}}
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/protocols/lldp", token, lldp,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT lldp: %d", status)
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/protocols/lldp", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "advertisement_interval") {
		t.Fatalf("LLDP 回读: %d %s", status, data)
	}
}

func TestW6Authorization(t *testing.T) {
	ts := newTestServer(t)
	// read-only：写 403；GET 正常
	status, vresp := login(t, ts, "viewer", "s3cret-Passw0rd!")
	if status != http.StatusOK {
		t.Fatalf("viewer 登录: %d", status)
	}
	acl := model.Acl{Name: "acl-x", Rules: []model.AclRule{{Seq: 10, Action: "deny"}}}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/acls", vresp.Token, acl, nil); status != http.StatusForbidden {
		t.Fatalf("read-only 创建 ACL 应 403: %d", status)
	}
	if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/acls", vresp.Token, nil, nil); status != http.StatusOK {
		t.Fatalf("read-only 查询 ACL 应 200: %d", status)
	}
}

// T0-1：GET /acls/{name}、GET /bonds/{name}（契约已声明，M2 仅实现列表/创建/删除）。
func TestAclBondDetailEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	acl := model.Acl{Name: "acl-detail", Rules: []model.AclRule{{Seq: 10, Action: "deny"}}}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/acls", token, acl,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 ACL 应 201")
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/acls/acl-detail", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "acl-detail") || !strings.Contains(string(data), "deny") {
		t.Fatalf("ACL 详情: %d %s", status, data)
	}
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/acls/no-such", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("未知 ACL 应 404: %d", status)
	}

	seedIface(t, ts, token, "ens2f0")
	seedIface(t, ts, token, "ens2f1")
	bond := model.Bond{Name: "bond0", Members: []string{"ens2f0", "ens2f1"}, Lacp: &model.Lacp{Mode: "active"}}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/bonds", token, bond,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 bond 应 201: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/bonds/bond0", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "bond0") || !strings.Contains(string(data), "active") {
		t.Fatalf("bond 详情: %d %s", status, data)
	}
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/bonds/no-such", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("未知 bond 应 404: %d", status)
	}
}
