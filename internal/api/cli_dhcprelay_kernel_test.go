package api

// 决策 #437：内核数据面 DHCP 中继的运行态读视图（dhcp_relay_note）——机制、客户端寻址依据与
// 运行态/计数如实呈现；VPP 数据面不加该注记（中继在 VPP 内运行，无进程内实例，读视图逐字不变）。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakeRelayRuntime 假的内核侧中继运行态读物（按交换机名给出状态）。
type fakeRelayRuntime struct {
	states map[string]network.DHCPRelayState
}

func (f *fakeRelayRuntime) DHCPRelayState(swName string) (network.DHCPRelayState, bool) {
	st, ok := f.states[swName]
	return st, ok
}

func relayedCfg(t *testing.T, x *cliExecutor) {
	t.Helper()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set virtual-switches vs-r type l2",
		"set virtual-switches vs-r gateway ip 192.168.99.1/24",
		"set virtual-switches vs-r dhcp-relay server 192.168.99.10",
		"commit", "exit",
	)
}

// CLI `show virtual-switches <n> detail`：内核数据面下带 dhcp_relay_note（机制 + 寻址依据 + 计数）。
func TestCLIVSwitchDHCPRelayKernelNote(t *testing.T) {
	x, _ := newCLIKit(t)
	relayedCfg(t, x)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	x.setVppState(fakeVppState{bds: []BridgeDomainState{{ID: 11, Name: "vs-r", Learn: true, Flood: true}}})
	x.setDHCPRelay(&fakeRelayRuntime{states: map[string]network.DHCPRelayState{
		"vs-r": {Running: true, Forwarded: 7, Injected: 6, DroppedNoClient: 1},
	}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-r detail").Output
	for _, want := range []string{
		"dhcp-relay-note", "192.168.99.10",
		"用户态实例", "源地址重写", "giaddr=0", // 机制
		"xid", "客户端 MAC", "登记表", // 寻址依据（(a)）
		"运行中", "已转发 7", "已回注 6", "丢弃应答 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核数据面详情应含 %q：\n%s", want, out)
		}
	}

	// 起不来时如实给出"未运行 + 原因"（不静默）。
	x.setDHCPRelay(&fakeRelayRuntime{states: map[string]network.DHCPRelayState{
		"vs-r": {Running: false, Reason: "打开内核 bridge vs-r 的收发套接字失败"},
	}})
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-r detail").Output
	if !strings.Contains(out, "未运行") || !strings.Contains(out, "收发套接字失败") {
		t.Fatalf("起不来时应如实给出未运行与原因：\n%s", out)
	}
}

// VPP 数据面不加该注记（读视图行为逐字不变；中继在 VPP 内运行，无进程内实例）。
func TestCLIVSwitchDHCPRelayVPPNoNote(t *testing.T) {
	x, _ := newCLIKit(t)
	relayedCfg(t, x)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}})
	x.setVppState(fakeVppState{bds: []BridgeDomainState{{ID: 11, Name: "vs-r", Learn: true, Flood: true}}})
	x.setDHCPRelay(&fakeRelayRuntime{states: map[string]network.DHCPRelayState{
		"vs-r": {Running: true, Forwarded: 7},
	}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-r detail").Output
	if !strings.Contains(out, "192.168.99.10") {
		t.Fatalf("VPP 详情应仍显示 dhcp-relay 声明：\n%s", out)
	}
	if strings.Contains(out, "dhcp-relay-note") {
		t.Fatalf("VPP 数据面不应加中继运行态注记：\n%s", out)
	}
}

// REST `GET /virtual-switches/{name}`：内核数据面下同源带 dhcp_relay_note。
func TestGetVSwitchDHCPRelayKernelNote(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		VPP: fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}},
		DHCPRelay: &fakeRelayRuntime{states: map[string]network.DHCPRelayState{
			"vs-r": {Running: true, Forwarded: 3, Injected: 3},
		}},
	})
	token := loginAdmin(t, ts)
	vs := model.VirtualSwitch{Name: "vs-r", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.99.1/24"}},
		DhcpRelayServer: "192.168.99.10"}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token, vs,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建带中继的交换机应 201: %d %s", status, data)
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-r", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 详情: %d %s", status, data)
	}
	body := string(data)
	if !strings.Contains(body, `"dhcp_relay":{"server":"192.168.99.10"}`) || !strings.Contains(body, "dhcp_relay_note") {
		t.Fatalf("内核数据面 REST 详情应带 dhcp_relay + dhcp_relay_note：%s", body)
	}
	for _, want := range []string{"用户态实例", "giaddr=0", "登记表", "运行中", "已转发 3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("REST 注记应含 %q：%s", want, body)
		}
	}

	// VPP 数据面：不加该字段（读视图逐字不变）。
	ts2 := newTestServerOpts(t, Options{VPP: fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}}})
	token2 := loginAdmin(t, ts2)
	if status, _, data := cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/virtual-switches", token2, vs,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建交换机应 201: %d %s", status, data)
	}
	_, _, data = cfgRequest(t, http.MethodGet, ts2.URL+APIPrefix+"/virtual-switches/vs-r", token2, nil, nil)
	if strings.Contains(string(data), "dhcp_relay_note") {
		t.Fatalf("VPP 数据面不应出现 dhcp_relay_note：%s", data)
	}
}

// 注记措辞：两种形态（运行中/未运行）都点明机制与寻址依据，未运行给原因。
func TestKernelDHCPRelayNoteWording(t *testing.T) {
	runNote := kernelDHCPRelayNote(network.DHCPRelayState{Running: true, Forwarded: 2, Injected: 1, DroppedNoClient: 3})
	for _, want := range []string{"用户态实例", "源地址重写", "BVI", "giaddr=0", "xid", "客户端 MAC", "登记表", "运行中", "2", "1", "3"} {
		if !strings.Contains(runNote, want) {
			t.Fatalf("运行中注记应含 %q：%s", want, runNote)
		}
	}
	downNote := kernelDHCPRelayNote(network.DHCPRelayState{Reason: "bridge 不在"})
	if !strings.Contains(downNote, "未运行") || !strings.Contains(downNote, "bridge 不在") {
		t.Fatalf("未运行注记应给出原因：%s", downNote)
	}
	// 用户可见文本不得出现决策号（archtest 的 user_text 守护在此提前拦住）。
	for _, note := range []string{runNote, downNote} {
		if strings.Contains(note, "#") {
			t.Fatalf("用户可见文本不应含决策号：%s", note)
		}
	}
}
