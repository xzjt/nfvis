package api

// 决策 #425：SPAN 的 vNIC 源在**读视图**里如实呈现。
//
// 覆盖面：CLI `show port-mirroring`（JunOS 风格配置树 + `| display json` 结构化输出）与
// REST `GET /port-mirroring`（契约字段不变：source.vnf / source.vnf_interface / source.direction）。
// 读视图按配置字段渲染（源= `vnf fw-vm` + `vnf-interface eth0`），不把它伪装成物理口名，
// 也不新增/改名任何 REST 字段——数据面落到哪个口（`vh-<vm>-<vnic>`）由 `show virtual-switches`
// 的派生端口条目与 VM 详情呈现。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// spanVnicFixture 一份可提交的配置：已声明 vhost-user vNIC 的 VNF + 物理分析口 +
// 源为该 vNIC 的镜像会话。
func spanVnicFixture() model.Config {
	return withSuperUser(model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 8}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
		Interfaces: []model.InterfaceConfig{{Name: "ens224"}},
		VirtualMachineFunctions: []model.VMFunction{{
			Name: "fw-vm", Image: "base.qcow2",
			VCPU:   model.VMCpu{Count: 1},
			Memory: model.VMMemory{SizeMB: 512, HugepageSize: "1G"},
			Interfaces: []model.VnfInterface{
				{Name: "eth0", Type: "vhost-user"},
			},
		}},
		PortMirroring: []model.PortMirroring{{
			Name:     "span-v",
			Source:   model.PMSource{Vnf: "fw-vm", VnfInterface: "eth0", Direction: "both"},
			Analyzer: "ens224",
		}},
	})
}

// CLI 读视图：vNIC 源逐字段可见（源 VNF / 源 vNIC / 方向），结构化输出同样带这些字段。
func TestShowPortMirroringVnicSourceCLI(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set interfaces ens224",
		"set virtual-machine-functions fw-vm image base.qcow2",
		"set virtual-machine-functions fw-vm vcpu count 1",
		"set virtual-machine-functions fw-vm memory size-mb 512",
		"set virtual-machine-functions fw-vm interfaces eth0 type vhost-user",
		"set port-mirroring span-v source vnf fw-vm interface eth0 direction both",
		"set port-mirroring span-v analyzer interface ens224",
		"commit",
		"exit",
	)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show port-mirroring").Output
	for _, want := range []string{"span-v", "vnf fw-vm", "vnf-interface eth0", "direction both", "analyzer ens224"} {
		if !strings.Contains(out, want) {
			t.Fatalf("读视图应含 %q（vNIC 源逐字段可见）:\n%s", want, out)
		}
	}
	// 结构化输出（Web/REST 同源消费）：字段名与 REST 契约一致，不得别名化。
	js := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show port-mirroring | display json").Output
	for _, k := range []string{`"vnf"`, `"vnf_interface"`, `"direction"`, `"analyzer"`} {
		if !strings.Contains(js, k) {
			t.Fatalf("结构化输出应含契约字段 %s：%q", k, js)
		}
	}
}

// REST 读视图：source 对象原样返回（vnf / vnf_interface / direction），与分析口一起；
// 不把 vNIC 源改写成物理口名（契约字段不动）。
func TestGetPortMirroringVnicSourceREST(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		spanVnicFixture(), map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交配置: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/port-mirroring", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /port-mirroring: %d %s", status, data)
	}
	var got []model.PortMirroring
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("响应不是 PortMirroring 数组: %v %s", err, data)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 条镜像会话，实得 %d: %s", len(got), data)
	}
	s := got[0].Source
	if got[0].Name != "span-v" || s.Vnf != "fw-vm" || s.VnfInterface != "eth0" ||
		s.Direction != "both" || got[0].Analyzer != "ens224" {
		t.Fatalf("REST 读视图应原样带 vNIC 源字段，实得 %+v", got[0])
	}
	// 源**不得**被改写成物理口名（解析只发生在下发层；配置/读视图仍是 vnf + vnic）。
	if s.Interface != "" {
		t.Fatalf("vNIC 源的 source.interface 应为空（不得改写成物理口名），实得 %q", s.Interface)
	}
}
