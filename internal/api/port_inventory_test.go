package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 决策 #83：`<ifname>` 的候选取自**运行态端口清单**，而不是「已写进配置的接口名」。
//
// 缺陷复现（真机）：空配置时 `set interfaces ?` 零候选；把 ens224 写进配置并 commit 后
// 候选才出现 `["ens224"]`——即候选只反映「已配置的名字」，既漏掉未声明的 DPDK 口
//（已接管的口在内核中已无 netdev），又会列出根本不存在的名字。

// fakePorts 假的运行态端口清单（单测不碰底座）。
type fakePorts struct {
	vpp     []string
	kernel  []string
	vppErr  error
	kernErr error
}

func (f fakePorts) VPPIfnames() ([]string, error) { return f.vpp, f.vppErr }

func (f fakePorts) KernelIfnames() ([]string, error) { return f.kernel, f.kernErr }

// kindCandidates 取某 kind 的动态候选清单。
func kindCandidates(t *testing.T, ts *httptest.Server, token, kind string) []string {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/cli/candidates?kind="+kind, token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("kind=%s 候选: %d %s", kind, status, data)
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析候选: %v (%s)", err, data)
	}
	return out
}

func hasName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestIfnameCandidatesComeFromRuntimeInventory(t *testing.T) {
	ts := newTestServerOpts(t, Options{Ports: fakePorts{
		vpp:    []string{"ens224", "ens192"},
		kernel: []string{"ens160"},
	}})
	token := loginAdmin(t, ts)

	// 配置里放一个「运行态不存在」的名字：它不得再自产自销变成候选（这正是原缺陷）
	cfg := sampleCandidate()
	cfg.Interfaces = []model.InterfaceConfig{{Name: "ghost0"}}
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, cfg,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("准备配置失败: %d", status)
	}

	vpp := kindCandidates(t, ts, token, "vpp-ifnames")
	if !hasName(vpp, "ens192") || !hasName(vpp, "ens224") {
		t.Fatalf("vpp-ifnames 应含 VPP 口（已由 DPDK 接管）: %v", vpp)
	}
	if hasName(vpp, "ghost0") {
		t.Fatalf("已配置但运行态不存在的名字不应成为候选（决策 #83）: %v", vpp)
	}
	if hasName(vpp, "ens160") {
		t.Fatalf("内核口不应出现在 vpp-ifnames: %v", vpp)
	}

	kern := kindCandidates(t, ts, token, "kernel-ifnames")
	if !hasName(kern, "ens160") {
		t.Fatalf("kernel-ifnames 应含内核物理口: %v", kern)
	}
	if hasName(kern, "ens192") {
		t.Fatalf("已接管的口不在内核清单: %v", kern)
	}

	all := kindCandidates(t, ts, token, "ifnames")
	if !hasName(all, "ens192") || !hasName(all, "ens160") {
		t.Fatalf("ifnames 应是 VPP ∪ 内核: %v", all)
	}
}

// 位置模式：`set interfaces <TAB>`（用户报告的那处）应给出运行态 DPDK 口。
func TestSetInterfacesCompletionListsVPPPorts(t *testing.T) {
	ts := newTestServerOpts(t, Options{Ports: fakePorts{vpp: []string{"ens224", "ens192"}}})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodGet,
		ts.URL+APIPrefix+"/cli/candidates?tokens=set,interfaces&partial=", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("位置候选: %d %s", status, data)
	}
	var cs []struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &cs); err != nil {
		t.Fatalf("解析候选: %v (%s)", err, data)
	}
	got := make([]string, 0, len(cs))
	for _, c := range cs {
		got = append(got, c.Token)
	}
	if !hasName(got, "ens224") || !hasName(got, "ens192") {
		t.Fatalf("`set interfaces ?` 应列出已被 DPDK 接管的口: %v", got)
	}
}

// 清单不可用时退化为「仅关键字」（契约 §5.3），**不得**退回「已配置接口名」。
func TestIfnameCandidatesDegradeWhenInventoryUnavailable(t *testing.T) {
	ts := newTestServerOpts(t, Options{Ports: fakePorts{vppErr: errors.New("VPP 未接入")}})
	token := loginAdmin(t, ts)

	cfg := sampleCandidate()
	cfg.Interfaces = []model.InterfaceConfig{{Name: "ghost0"}}
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, cfg,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("准备配置失败: %d", status)
	}
	if got := kindCandidates(t, ts, token, "vpp-ifnames"); len(got) != 0 {
		t.Fatalf("清单不可用时应为空（不得退回已配置名）: %v", got)
	}
}

// 未装配 PortInventory（nil）同样只是「没有动态候选」，不得 panic。
func TestIfnameCandidatesWithoutInventory(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	token := loginAdmin(t, ts)
	for _, kind := range []string{"ifnames", "vpp-ifnames", "kernel-ifnames"} {
		if got := kindCandidates(t, ts, token, kind); len(got) != 0 {
			t.Fatalf("%s 应为空: %v", kind, got)
		}
	}
}

// 决策 #155：无参/physical 聚合本身就是运行态清单——配置为空时运行态口以行的形式
// 出现（来源列「未声明」），不再是空态提示文本（#83 时代的提示口径废止）。
func TestShowPhysicalEmptyStateListsRuntimePorts(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vpp: []string{"ens224", "ens192"}})

	out := x.Execute("admin", "super-user", "ssh", "show interfaces physical").Output
	for _, want := range []string{"ens224", "ens192", "未声明"} {
		if !strings.Contains(out, want) {
			t.Fatalf("运行态清单应以行形式列出运行态端口（含 %q）: %q", want, out)
		}
	}
}

// 清单不可用时清单仍要说明，而不是静默给一句老提示。
func TestShowPhysicalEmptyStateWhenInventoryUnavailable(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vppErr: errors.New("VPP 未接入")})
	out := x.Execute("admin", "super-user", "ssh", "show interfaces physical").Output
	if !strings.Contains(out, "VPP 运行态不可用") {
		t.Fatalf("应说明运行态不可用: %q", out)
	}
}
