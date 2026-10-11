package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 决策 #461（收口 round12/13 观察①）：VM 读视图给**有效值**（pin/backing/页尺寸未显式声明时
// 补产品的确定缺省）+ 运行态派生的 `vcpu.cores_assigned`（资源池账本单源）；详情与列表同源。
func TestVMReadViewEffectiveAndAssignedCores(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	cfg := withSuperUser(model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 8}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
		VirtualMachineFunctions: []model.VMFunction{{
			Name: "fw-vm", Image: "base.qcow2",
			VCPU:   model.VMCpu{Count: 2},
			Memory: model.VMMemory{SizeMB: 2048},
		}},
	})
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		cfg, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交配置: %d %s", status, data)
	}

	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 详情: %d %s", status, data)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析详情: %v", err)
	}
	vcpu, _ := got["vcpu"].(map[string]any)
	mem, _ := got["memory"].(map[string]any)
	if vcpu == nil || mem == nil {
		t.Fatalf("读视图缺 vcpu/memory: %s", data)
	}
	if vcpu["pin"] != true {
		t.Fatalf("pin 未声明时应给有效值 true: %v", vcpu["pin"])
	}
	if cores, _ := vcpu["cores_assigned"].([]any); len(cores) != 2 {
		t.Fatalf("cores_assigned 应按账本给 2 核: %v", vcpu["cores_assigned"])
	}
	if mem["backing"] != "hugepage" {
		t.Fatalf("backing 未声明时应给有效值 hugepage: %v", mem["backing"])
	}
	if mem["hugepage_size"] != "1G" {
		t.Fatalf("页尺寸未声明时应给主池 1G: %v", mem["hugepage_size"])
	}

	// 列表端点同一读视图（同源）。
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 列表: %d %s", status, data)
	}
	if !strings.Contains(string(data), `"cores_assigned"`) || !strings.Contains(string(data), `"hugepage_size":"1G"`) {
		t.Fatalf("列表应与详情同源（含有效值/绑核）: %s", data)
	}

	// GET→PUT 往返：把读视图（含 vcpu.cores_assigned）原样 PUT 回去——必须被接受（该字段被忽略），
	// 且**不得落进配置**：`cores_assigned` 刻意不在 model.VMCpu 上。
	status, _, data = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm", token,
		got, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT（读视图往返）应被接受: %d %s", status, data)
	}
	// 类型级守护：配置模型反序列化带该字段的 body 后不回吐它（运行态不进配置）。
	var cpu model.VMCpu
	if err := json.Unmarshal([]byte(`{"count":2,"pin":true,"cores_assigned":[4,5]}`), &cpu); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(cpu); strings.Contains(string(b), "cores_assigned") {
		t.Fatalf("cores_assigned 不得进配置模型: %s", b)
	}
}
