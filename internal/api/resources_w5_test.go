package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- W5：资源池 / 本地用户 / 系统状态 ----------

func TestResourcePoolsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// PUT 资源池（直提）
	pools := map[string]any{
		"hugepages": []map[string]any{{"page_size": "1G", "count": 32}},
		"cpu":       map[string]any{"isolated_cores": []int{4, 5, 6, 7}},
	}
	status, hdr, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/resource-pools", token, pools,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK || hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("PUT resource-pools: %d %s %s", status, hdr.Get("X-NFVIS-Committed"), data)
	}
	// FR-SYS-002：reboot 警告
	if !strings.Contains(string(data), "reboot") {
		t.Fatalf("资源池变更应有 reboot 警告: %s", data)
	}

	// GET：账本用量（总量/已分配/空闲 + vpp-reserved）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/resource-pools", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET resource-pools: %d", status)
	}
	var got struct {
		Hugepages []map[string]any `json:"hugepages"`
		CPU       map[string]any   `json:"cpu"`
	}
	if err := json.Unmarshal(data, &got); err != nil || len(got.Hugepages) != 1 {
		t.Fatalf("资源池响应不符: %s", data)
	}
	hp := got.Hugepages[0]
	if hp["total"] != float64(32) || hp["allocated"] != float64(0) || hp["free"] != float64(32) {
		t.Fatalf("大页用量不符: %v", hp)
	}
	if hp["count"] != float64(32) || hp["page_size"] != "1G" {
		t.Fatalf("大页配置项应含 count/page_size: %v", hp)
	}
	if _, ok := got.CPU["vpp_reserved"]; !ok {
		t.Fatalf("CPU 应含 vpp_reserved（FR-SYS-010）: %s", data)
	}
	if _, ok := got.CPU["isolated_cores"]; !ok {
		t.Fatalf("CPU 应含 isolated_cores（契约 ResourcePool.cpu）: %s", data)
	}
	if _, ok := got.CPU["allocated"]; !ok {
		t.Fatalf("CPU 应含 allocated（契约 ResourcePool.cpu）: %s", data)
	}
}

// 运行态视图与契约 ResourcePool 对齐：配置项 + 运行态（vpp_reserved/allocated/free），
// 未配置资源池时返回空视图而非 panic（FR-CMP-001~004、FR-SYS-010，决策 #39）。
func TestResourcePoolViewShape(t *testing.T) {
	// 未配置资源池：空视图（此前实现会 nil panic）。
	empty := resourcePoolView(model.Config{})
	if hps, ok := empty["hugepages"].([]map[string]any); !ok || len(hps) != 0 {
		t.Fatalf("未配置资源池应返回空 hugepages: %v", empty["hugepages"])
	}
	cpu := empty["cpu"].(map[string]any)
	for _, k := range []string{"isolated_cores", "vpp_reserved", "free", "allocated"} {
		if _, ok := cpu[k]; !ok {
			t.Fatalf("cpu 缺少 %s: %v", k, cpu)
		}
	}

	// 有池 + VPP 保留核 + 两台 VM（一台 normal 不占大页）。
	cfg := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 8}},
			CPU: &model.CPUSetup{
				IsolatedCores: []int{4, 5, 6, 7, 8, 9},
				Numa:          []model.NumaNode{{Node: 0, Cores: []int{4, 5, 6, 7, 8, 9}}},
			},
		},
		Vpp: &model.VppConfig{CPU: &model.VppCPU{MainCore: 4, CorelistWorkers: "5"}},
		VirtualMachineFunctions: []model.VMFunction{
			{Name: "fw-vm", Image: "img", VCPU: model.VMCpu{Count: 2},
				Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"}},
			{Name: "plain-vm", Image: "img", VCPU: model.VMCpu{Count: 1},
				Memory: model.VMMemory{SizeMB: 2048, Backing: "normal"}},
		},
	}
	view := resourcePoolView(cfg)

	hp := view["hugepages"].([]map[string]any)[0]
	if hp["total"] != 8 || hp["allocated"] != 1 || hp["free"] != 7 {
		t.Fatalf("normal VM 不应占大页：%v", hp)
	}

	cpu = view["cpu"].(map[string]any)
	if got := cpu["isolated_cores"].([]int); len(got) != 6 {
		t.Fatalf("isolated_cores 应为配置全集: %v", got)
	}
	if got := cpu["vpp_reserved"].([]int); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("vpp_reserved 应为 [4 5]: %v", got)
	}
	if got := cpu["free"].([]int); len(got) != 1 || got[0] != 9 {
		t.Fatalf("free 应为 [9]（6 核扣 VPP 2 + VNF 3）: %v", got)
	}
	alloc := cpu["allocated"].([]map[string]any)
	if len(alloc) != 2 || alloc[0]["vnf"] != "fw-vm" || alloc[1]["vnf"] != "plain-vm" {
		t.Fatalf("allocated 应按 VNF 名升序: %v", alloc)
	}
	if cores := alloc[0]["cores"].([]int); len(cores) != 2 || cores[0] != 6 || cores[1] != 7 {
		t.Fatalf("fw-vm 绑核应为 [6 7]: %v", cores)
	}
	if numa, ok := cpu["numa"].([]map[string]any); !ok || len(numa) != 1 {
		t.Fatalf("cpu 应含配置 numa: %v", cpu["numa"])
	}
}

func TestLoginUsersEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 创建用户（口令仅存哈希）
	nu := map[string]any{"name": "netop", "kind": "user", "password": "Net0p-Passw0rd!", "class": "operator"}
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token, nu, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建用户应 201")
	}
	// 弱口令 → 400
	nu2 := map[string]any{"name": "weak", "kind": "user", "password": "short"}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token, nu2, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("弱口令应 400: %d %s", status, data)
	}
	// 重名 → 409
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token, nu, nil)
	if status != http.StatusConflict {
		t.Fatalf("重名应 409")
	}

	// 列表：口令哈希永不回显（FR-SEC-007）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/login-users", token, nil, nil)
	if status != http.StatusOK || strings.Contains(string(data), "pbkdf2") {
		t.Fatalf("列表不应回显哈希: %s", data)
	}
	if !strings.Contains(string(data), "netop") {
		t.Fatalf("列表应含 netop: %s", data)
	}

	// 删除自己 → 409；删除最后一个 super-user → 409
	status, _, data = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/system/login-users/admin", token, nil, nil)
	if status != http.StatusConflict {
		t.Fatalf("删除自己应 409: %d %s", status, data)
	}
	// 删除 operator 用户 → 成功
	status, _, _ = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/system/login-users/netop", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("删除普通用户应 200")
	}

	// 修改 class
	status, _, _ = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-users/netop", token,
		map[string]any{"class": "read-only"}, nil)
	_ = status
}

func TestChangePasswordEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 旧口令错误 → 400
	status, _, _ := cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/system/login-users/admin:change-password", token,
		map[string]any{"old_password": "wrong", "new_password": "NewPassw0rd!x"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("旧口令错误应 400: %d", status)
	}

	// 正确修改 → 新口令可登录
	status, _, _ = cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/system/login-users/admin:change-password", token,
		map[string]any{"old_password": "s3cret-Passw0rd!", "new_password": "NewPassw0rd!x"}, nil)
	if status != http.StatusOK {
		t.Fatalf("改密应成功")
	}
	status, resp := login(t, ts, "admin", "NewPassw0rd!x")
	if status != http.StatusOK {
		t.Fatalf("新口令应可登录: %d", status)
	}
	_ = resp
}

func TestSystemStatusEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/status", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "uptime_seconds") {
		t.Fatalf("system status: %d %s", status, data)
	}
}

func TestW5Authorization(t *testing.T) {
	ts := newTestServer(t)
	// read-only 用户：写操作 403（FR-SEC-002）
	viewer := func() string {
		// viewer 在 testAAA 固定夹具中
		status, resp := login(t, ts, "viewer", "s3cret-Passw0rd!")
		if status != http.StatusOK {
			t.Fatalf("viewer 登录: %d", status)
		}
		return resp.Token
	}()
	pools := map[string]any{"hugepages": []map[string]any{{"page_size": "1G", "count": 8}}}
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/resource-pools", viewer, pools, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only 写资源池应 403: %d", status)
	}
}
