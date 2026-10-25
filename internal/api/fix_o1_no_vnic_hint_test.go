package api

// O-1 守护（v3.0.29 外部测试报告，观察项但用户要求修）：VM **未声明任何 vNIC** 时 start
// 照常成功，起来后没有任何网络——guest 里的 cloud-init/DHCP 会无限重试，而 CLI/REST 与
// 日志都不给一句提示。
//
// 修法（**不阻断启动**——那是行为变更）：start **成功**的返回里追加一条明确提示，
// 说明「无网络连通性」与「要网络该怎么声明」。CLI 与 REST 共用同一个判定（`vmNoVNICHint`），
// 避免两处各写一份而漂移。
//
// 判据：
//   - 无 vNIC 的 VM start 成功后，CLI 输出 / REST 响应**含**「未声明任何 vNIC」；
//   - 有 vNIC 的 VM **不含**该提示（不制造噪声），且 CLI 的正常文案逐字不变
//     （既有守护 TestCLIRequestVMStartSurfacesDiagnosis 依赖这一点）；
//   - start 仍然成功（提示是附加信息，不是失败）。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// seedNoNICVMConfig 建一台**不声明任何 vNIC** 的 VM 并 commit（其余字段与 seedVMConfig 同）。
func seedNoNICVMConfig(t *testing.T, x *cliExecutor) {
	t.Helper()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-7",
		"set virtual-machine-functions nonic-vm image base.qcow2",
		"set virtual-machine-functions nonic-vm vcpu count 1",
		"set virtual-machine-functions nonic-vm memory size-mb 1024",
		"commit",
		"exit",
	)
}

// noVNICHintText 提示里必须出现的关键词（用户点名的措辞，必须逐字出现）。
const noVNICHintText = "未声明任何 vNIC"

// CLI：无 vNIC 的 VM start 成功后，输出在「已受理」之外追加提示；start 仍算成功（无 %%）。
func TestO1CLIStartHintsMissingVNIC(t *testing.T) {
	x, _ := newCLIKit(t)
	seedNoNICVMConfig(t, x)
	vmRT := newFakeVM()
	x.setComputeRuntime(vmRT, nil, nil, nil, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request virtual-machine-functions nonic-vm start")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("无 vNIC 不得让 start 失败（提示不是阻断）：%s", res.Output)
	}
	if !strings.Contains(res.Output, "启动 VNF nonic-vm 已受理") {
		t.Fatalf("既有成功文案必须保留：%s", res.Output)
	}
	if !strings.Contains(res.Output, noVNICHintText) {
		t.Fatalf("无 vNIC 的 VM 启动成功必须提示「%s」：%s", noVNICHintText, res.Output)
	}
	// 提示要给出照做路径（否则操作者仍不知道怎么写）。
	if !strings.Contains(res.Output, "set virtual-machine-functions nonic-vm interfaces") {
		t.Fatalf("提示应给出照做的配置命令：%s", res.Output)
	}
}

// CLI：有 vNIC 的 VM **不含**该提示（不制造噪声），普通成功文案逐字不变。
func TestO1CLIStartSilentWhenVNICDeclared(t *testing.T) {
	x, _ := newCLIKit(t)
	seedVMConfig(t, x) // 带 interfaces eth0 type vhost-user
	vmRT := newFakeVM()
	x.setComputeRuntime(vmRT, nil, nil, nil, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request virtual-machine-functions fw-vm start")
	if res.Output != "启动 VNF fw-vm 已受理\n" {
		t.Fatalf("有 vNIC 时成功文案必须逐字不变，实得 %q", res.Output)
	}
}

// REST：无 vNIC 的 VM start 仍 202，但响应体带 notes（同一句提示）；有 vNIC 的不带 notes 键。
func TestO1RESTStartNotesMissingVNIC(t *testing.T) {
	fake := newFakeVM()
	ts := newTestServerOpts(t, Options{VM: fake})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	body := map[string]any{
		"name":   "nonic-vm",
		"image":  "img.qcow2",
		"vcpu":   map[string]any{"count": 1},
		"memory": map[string]any{"size_mb": 1024, "hugepage_size": "1G"},
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建无 vNIC 的 VM: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/virtual-machine-functions/nonic-vm:start", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("无 vNIC 的 start 仍应 202（提示不阻断）：%d %s", status, data)
	}
	if !strings.Contains(string(data), "notes") || !strings.Contains(string(data), noVNICHintText) {
		t.Fatalf("REST 成功响应应带 notes 提示「%s」：%s", noVNICHintText, data)
	}
	// 既有形状（status/name）必须保留——客户端按它刷新状态。
	if !strings.Contains(string(data), `"status":"starting"`) || !strings.Contains(string(data), `"name":"nonic-vm"`) {
		t.Fatalf("既有响应字段不得丢失：%s", data)
	}

	// 有 vNIC：不带 notes（不制造噪声）。
	withNIC := map[string]any{
		"name":       "nic-vm",
		"image":      "img.qcow2",
		"vcpu":       map[string]any{"count": 1},
		"memory":     map[string]any{"size_mb": 1024, "hugepage_size": "1G"},
		"interfaces": []map[string]any{{"name": "eth0", "type": "vhost-user"}},
	}
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		withNIC, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建有 vNIC 的 VM: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/virtual-machine-functions/nic-vm:start", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("有 vNIC 的 start 应 202：%d %s", status, data)
	}
	if strings.Contains(string(data), "notes") || strings.Contains(string(data), noVNICHintText) {
		t.Fatalf("有 vNIC 时不得出现该提示：%s", data)
	}
}

// 判定单源：`vmNoVNICHint` 是有 vNIC/无 vNIC 的唯一判据（CLI 与 REST 都调它）。
func TestO1NoVNICHintHelper(t *testing.T) {
	noNIC := model.VMFunction{Name: "nonic-vm", Image: "base.qcow2"}
	hint := vmNoVNICHint(noNIC)
	if !strings.Contains(hint, noVNICHintText) {
		t.Fatalf("无 vNIC 应给提示：%q", hint)
	}
	if !strings.Contains(hint, "set virtual-machine-functions nonic-vm interfaces") {
		t.Fatalf("提示应含该 VM 名的照做命令：%q", hint)
	}
	if !strings.Contains(hint, "cloud-init") {
		t.Fatalf("提示应说明 guest 内会持续重试的现象：%q", hint)
	}

	withNIC := noNIC
	withNIC.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}}
	if got := vmNoVNICHint(withNIC); got != "" {
		t.Fatalf("有 vNIC 时不得返回提示：%q", got)
	}
}
