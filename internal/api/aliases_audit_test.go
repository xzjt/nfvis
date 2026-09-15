package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

const aaaClassSU = aaa.ClassSuperUser

// ---------- 别名表 / 审计查询 / 动态候选 ----------

func TestCLIVlanAccessAlias(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-app type l2",
		"set virtual-switches vs-app vlan access 100",
	)

	res := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-app")
	if !strings.Contains(res.Output, "vlan-access 100") {
		t.Fatalf("别名语句应落到 vlan_access:\n%s", res.Output)
	}

	out := run(t, x, "admin", aaaClassSU, "ssh", "commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("提交: %s", out)
	}

	// 删除别名语句
	run(t, x, "admin", aaaClassSU, "ssh", "delete virtual-switches vs-app vlan access")
	res = x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-app")
	if strings.Contains(res.Output, "vlan-access") {
		t.Fatalf("删除后不应再有 vlan-access:\n%s", res.Output)
	}
}

func TestCLITrunkVlansAlias(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens2f0 mtu 9000",
		"set virtual-switches vs-app type l2",
		"set virtual-switches vs-app ports 1 interface ens2f0 trunk vlans 100,200",
	)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-app")
	if !strings.Contains(res.Output, "trunk [ 100 200 ]") || !strings.Contains(res.Output, "interface ens2f0") {
		t.Fatalf("trunk 别名应落库并渲染:\n%s", res.Output)
	}
	out := run(t, x, "admin", aaaClassSU, "ssh", "commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("提交: %s", out)
	}
}

func TestCLINumaNodeAndSerialConsoleAliases(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 32",
		"set resource-pools cpu isolated-cores 4-7",
		"set virtual-machine-functions fw-vm image ubuntu22-vm",
		"set virtual-machine-functions fw-vm vcpu count 2",
		"set virtual-machine-functions fw-vm memory size-mb 8192",
		"set virtual-machine-functions fw-vm memory numa node 1",
		"set virtual-machine-functions fw-vm serial console enable",
	)

	res := x.Execute("admin", aaaClassSU, "ssh", "show virtual-machine-functions fw-vm")
	if !strings.Contains(res.Output, "numa-node 1") || !strings.Contains(res.Output, "serial-console true") {
		t.Fatalf("numa node / serial console 别名应落库:\n%s", res.Output)
	}
	out := run(t, x, "admin", aaaClassSU, "ssh", "commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("提交: %s", out)
	}
}

func TestAuditLogsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 产生一条 commit 审计
	cfg := sampleCandidate()
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, cfg,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("准备提交")
	}

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=10", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET audit-logs: %d %s", status, data)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) == 0 {
		t.Fatalf("审计记录不应为空: %s", data)
	}
	if entries[0]["action"] != "config.commit" {
		t.Fatalf("最新审计应为 config.commit: %v", entries[0])
	}

	// user 过滤
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?user=ghost", token, nil, nil)
	if status != http.StatusOK || strings.Contains(string(data), "config.commit") {
		t.Fatalf("user 过滤应排除记录: %s", data)
	}
}

func TestDynamicCandidatesEndpoint(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 准备配置：接口 + 交换机
	cfg := sampleCandidate()
	cfg.Interfaces = []model.InterfaceConfig{{Name: "ens2f0"}}
	cfg.VirtualSwitches = []model.VirtualSwitch{{Name: "vs-app", Type: "l2"}}
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, cfg,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("准备配置")
	}

	// kind 模式：动态候选清单。
	// 注意：接口名一族（ifnames/vpp-ifnames/kernel-ifnames）自决策 #83 起取自**运行态端口清单**
	//（见 port_inventory_test.go），不再来自配置；此处用仍属配置来源的 vswitches 验证 kind 模式本身。
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/cli/candidates?kind=vswitches", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "vs-app") {
		t.Fatalf("vswitches 候选应含 vs-app: %d %s", status, data)
	}

	// 位置模式：show virtual-machine-functions 位置的动态候选
	status, _, data = cfgRequest(t, http.MethodGet,
		ts.URL+APIPrefix+"/cli/candidates?tokens=show,virtual-machine-functions&partial=", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("位置候选: %d", status)
	}
	var cs []struct {
		Token string `json:"Token"`
	}
	_ = json.Unmarshal(data, &cs)
	found := false
	for _, c := range cs {
		if c.Token == "vs-app" {
			found = true
		}
	}
	// vs-app 是交换机不是 VM——此处应无 vmnames 候选（sampleCandidate 无 VM）
	if found {
		t.Fatalf("不应出现交换机名候选")
	}
}
