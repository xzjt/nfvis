package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// ---------- 资源 handlers 第一组（system/interfaces/virtual-switches/vrfs） ----------

func TestSystemEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// GET 初始为空配置
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET system: %d %s", status, data)
	}

	// PUT system（candidate 写入，未提交）
	sys := model.SystemConfig{Hostname: "sys-1", IdleTimeoutMinutes: 15}
	status, hdr, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system", token, sys, nil)
	if status != http.StatusOK || hdr.Get("X-NFVIS-Committed") != "false" {
		t.Fatalf("PUT system 应写 candidate: %d %s", status, data)
	}

	// 直提后 GET 反映 committed
	status, hdr, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system", token,
		model.SystemConfig{Hostname: "sys-2"}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK || hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("直提应生效: %d %s %s", status, hdr.Get("X-NFVIS-Committed"), data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system", token, nil, nil)
	var got model.SystemConfig
	_ = json.Unmarshal(data, &got)
	if got.Hostname != "sys-2" {
		t.Fatalf("committed system 应为 sys-2: %s", data)
	}

	// login 节不被整体替换误伤：先引导 login 配置再 PUT system
	if err := putLoginUser(t, ts, token); err != nil {
		t.Fatalf("准备 login 配置: %v", err)
	}
	status, _, _ = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system", token,
		model.SystemConfig{Hostname: "sys-3"}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT system: %d", status)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system", token, nil, nil)
	if !strings.Contains(string(data), "op-user") {
		t.Fatalf("PUT system 不应清掉 login 节: %s", data)
	}
}

func putLoginUser(t *testing.T, ts *httptest.Server, token string) error {
	t.Helper()
	// 用户必须有口令：校验会拦下无口令账号（R44-1 的兜底），故这里用真实哈希。
	hash, err := aaa.HashPassword("Op-User-Passw0rd!")
	if err != nil {
		return err
	}
	// 整文档写入：保留 super-user（决策 #152），再补一个操作员账号——
	// 文档里一个 super-user 都没有会被自锁兜底拒掉。
	cfg := withSuperUser(model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
		Users: []model.LoginUserConfig{{Name: "op-user", Class: "operator", PasswordHash: hash}},
	}}})
	// 经配置事务端点写入并直提（引擎校验 class 引用：operator 为预置类）
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		cfg, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("写入 login 配置: %d %s", status, data)
	}
	return nil
}

func TestInterfacesEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	iface := model.InterfaceConfig{Name: "ens2f0", Description: "to-TOR", MTU: 9000}
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f0", token, iface,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT interface: %d", status)
	}

	// 列表与详情（committed）
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/interfaces", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "ens2f0") {
		t.Fatalf("接口列表应含 ens2f0: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/interfaces/ens2f0", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "to-TOR") {
		t.Fatalf("接口详情: %d %s", status, data)
	}
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/interfaces/ghost0", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("未知接口应 404: %d", status)
	}

	// 名称以路径为准（body 内 name 被覆盖）
	iface2 := model.InterfaceConfig{Name: "hack", MTU: 1500}
	status, _, _ = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f1", token, iface2,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT interface 2: %d", status)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/interfaces/ens2f1", token, nil, nil)
	if status != http.StatusOK || strings.Contains(string(data), "hack") {
		t.Fatalf("接口名应以路径为准: %s", data)
	}
}

func TestVirtualSwitchesEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 创建 → 201
	vs := model.VirtualSwitch{Name: "vs-app", Type: "l2", VlanAccess: 100}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token, vs,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建交换机应 201: %d %s", status, data)
	}
	// 重名 → 409
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token, vs, nil)
	if status != http.StatusConflict {
		t.Fatalf("重名应 409: %d %s", status, data)
	}
	// 缺名 → 400
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
		model.VirtualSwitch{Type: "l2"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("缺名应 400: %d", status)
	}

	// 先配置物理口（端口引用校验，FR-CFG-011 前置）
	iface := model.InterfaceConfig{Name: "ens2f0"}
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f0", token, iface,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("PUT interface: %d", status)
	}

	// 端口全量替换 + 引用校验在 commit 生效
	ports := []model.VSwitchPort{{Seq: 1, Interface: "ens2f0"}}
	status, _, _ = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/virtual-switches/vs-app/ports", token, ports,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT ports: %d", status)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-app/ports", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "ens2f0") {
		t.Fatalf("端口回读: %d %s", status, data)
	}

	// 删除被 VNF 引用的交换机 → 409（FR-NET-015）
	vmCfg := sampleCandidate()
	vmCfg.Interfaces = []model.InterfaceConfig{{Name: "ens2f0"}}
	vmCfg.VirtualSwitches = []model.VirtualSwitch{{Name: "vs-app", Type: "l2", VlanAccess: 100,
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens2f0"}}}}
	vmCfg.VirtualMachineFunctions = []model.VMFunction{{
		Name: "fw-vm", Image: "ubuntu22-vm",
		VCPU:       model.VMCpu{Count: 2},
		Memory:     model.VMMemory{SizeMB: 8192, HugepageSize: "1G"},
		Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-app"}},
	}}
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, vmCfg,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("准备 VNF 配置: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/virtual-switches/vs-app", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusConflict || !strings.Contains(string(data), "fw-vm") {
		t.Fatalf("被引用删除应 409: %d %s", status, data)
	}
}

func TestVrfsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 物理口（VLAN 子接口的父口须存在）
	iface := model.InterfaceConfig{Name: "ens2f0"}
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f0", token, iface,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("PUT interface: %d", status)
	}

	// 创建 VRF（独立 Vrf 条目合法；type=l3 交换机才要求同名映射）
	vrf := model.Vrf{Name: "vs-mgmt", L3Interfaces: []model.L3Interface{{
		Interface: "ens2f0.100", Addresses: []string{"10.10.0.1/24"},
	}}}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token, vrf,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建 VRF 应 201: %d %s", status, data)
	}
	// vs-mgmt 同时建为 L3 交换机（附录 B：type=l3 交换机 + 同名 VRF）
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
		model.VirtualSwitch{Name: "vs-mgmt", Type: "l3"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 vs-mgmt L3 交换机")
	}

	// 建 L3 交换机 vs-data（需同名 VRF 承载数据，决策 #24）
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token,
		model.Vrf{Name: "vs-data"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 vs-data VRF")
	}
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
		model.VirtualSwitch{Name: "vs-data", Type: "l3"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 vs-data L3 交换机")
	}

	// 与 L3 交换机重名 → 409（附录 B 映射，决策 #26 前置校验）
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token,
		model.Vrf{Name: "vs-data"}, nil)
	if status != http.StatusConflict {
		t.Fatalf("与 L3 交换机重名应 409: %d %s", status, data)
	}

	// 路由全量替换
	routes := []model.Route{{Prefix: "0.0.0.0/0", NextHop: "10.10.0.254"}}
	status, _, _ = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/vrfs/vs-mgmt/routes", token, routes,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT routes: %d", status)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vrfs/vs-mgmt", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "10.10.0.254") {
		t.Fatalf("VRF 详情应含路由: %d %s", status, data)
	}

	// NAT outside 要求出接口归属某 VRF 且带地址（决策 #52）
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token,
		model.Vrf{Name: "wan", L3Interfaces: []model.L3Interface{{Interface: "ens2f0", Addresses: []string{"203.0.113.1/24"}}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建 wan VRF")
	}

	// 被 NAT 引用的 VRF：删除时 L3 映射检查优先（vs-mgmt 是 L3 交换机数据）→ 409。
	// 底稿取**生效配置**（GET /configuration）：前面的直提都是一次性事务、提交后不再持锁
	// （决策 #151），会话里没有 candidate 可读。
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration: %d", status)
	}
	var cur struct {
		Configuration model.Config `json:"configuration"`
	}
	if err := json.Unmarshal(data, &cur); err != nil {
		t.Fatalf("解析 configuration: %v", err)
	}
	cur.Configuration.Nat = &model.NatConfig{Rules: []model.NatRule{{
		Seq: 1, MatchSource: "192.168.100.0/24", VirtualSwitch: "vs-mgmt", Action: model.NatAction{Interface: "ens2f0"},
	}}}
	status, _, _ = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, cur.Configuration,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("准备 NAT 配置: %d", status)
	}
	status, _, data = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/vrfs/vs-mgmt", token, nil,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusConflict || !strings.Contains(string(data), "L3 交换机") {
		t.Fatalf("删除 L3 交换机数据 VRF 应 409: %d %s", status, data)
	}

	// 列表
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vrfs", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "vs-data") {
		t.Fatalf("VRF 列表: %d %s", status, data)
	}
}
