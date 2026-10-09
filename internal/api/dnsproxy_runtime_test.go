package api

// 决策 #439：内核数据面 DNS 代理的运行态读视图（runtime 块）——机制、每落点一行与计数如实
// 呈现；VPP 数据面不加运行态块（转发器以 punt 形态运行，读视图逐字不变）。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakeDNSProxyRuntime 假的内核侧 DNS 代理运行态读物（ok=false = 没有可报的运行态）。
type fakeDNSProxyRuntime struct {
	st network.DNSProxyState
	ok bool
}

func (f *fakeDNSProxyRuntime) DNSProxyState() (network.DNSProxyState, bool) { return f.st, f.ok }

// kernelDNSProxyCfg 提交内核数据面 + 全局上游（数据面中立配置，内核侧同样生效）。
func kernelDNSProxyCfg(t *testing.T, x *cliExecutor) {
	t.Helper()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system dataplane kernel",
		"set system dns proxy server 8.8.8.8",
		"commit", "exit",
	)
}

// CLI `show dns proxy`：内核数据面 + 可读运行态 ⇒ 追加运行态块（机制 + 每落点一行 + 计数），
// 说明块换成内核口径；未收敛落点如实给原因、无可用上游如实标「无（回 SERVFAIL）」。
func TestCLIDNSProxyKernelRuntimeBlock(t *testing.T) {
	x, _ := newCLIKit(t)
	kernelDNSProxyCfg(t, x)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	x.setDNSProxy(&fakeDNSProxyRuntime{ok: true, st: network.DNSProxyState{
		Domains: []network.DNSProxyDomainState{
			{Name: "vs-dns", Addresses: []string{"192.168.99.1"}, Upstreams: []string{"10.0.0.53"}, VRFDevice: "vr-vs-dns"},
		},
		Answered: 5, Servfail: 2, SendFail: 1,
	}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show dns proxy").Output
	for _, want := range []string{
		"启用", "8.8.8.8",
		// 内核口径的说明块（替换 VPP 的 punt/独占措辞）
		"内核数据面（Linux 内核网络）：服务落点＝各域内的 IPv4 地址",
		// 运行态块：机制句 + 每落点一行 + 计数
		"运行态:",
		"nfvisd 内的转发器", "UDP/53", "按域优先", "回 SERVFAIL",
		"域 vs-dns（vr-vs-dns）：192.168.99.1 ← 上游 10.0.0.53",
		"已应答 5 / 已回 SERVFAIL 2 / 回包失败 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核数据面读视图应含 %q：\n%s", want, out)
		}
	}

	// 未收敛的落点如实给原因（不静默）；无可用上游如实标「无（回 SERVFAIL）」。
	x.setDNSProxy(&fakeDNSProxyRuntime{ok: true, st: network.DNSProxyState{
		Domains: []network.DNSProxyDomainState{
			{Name: "vs-x", Addresses: []string{"192.168.99.2"}, VRFDevice: "vr-vs-x", Error: "地址未就绪"},
			{Name: "vs-y", VRFDevice: "vr-vs-y"},
		},
	}})
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show dns proxy").Output
	for _, want := range []string{
		"域 vs-x（vr-vs-x）：192.168.99.2 ← 上游 无（回 SERVFAIL）；未收敛：地址未就绪",
		"域 vs-y（vr-vs-y）：（无） ← 上游 无（回 SERVFAIL）",
		"已应答 0 / 已回 SERVFAIL 0 / 回包失败 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("未收敛/无上游落点应如实呈现 %q：\n%s", want, out)
		}
	}
}

// REST GET /dns/proxy：内核数据面 + 可读运行态 + 已启用 ⇒ 附 runtime（与 CLI 同一读物、
// 同一计数）。
func TestGetDNSProxyKernelRuntimeBlock(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		VPP: fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}},
		DNSProxy: &fakeDNSProxyRuntime{ok: true, st: network.DNSProxyState{
			Domains: []network.DNSProxyDomainState{
				{Name: "vs-dns", Addresses: []string{"192.168.99.1"}, Upstreams: []string{"10.0.0.53"}, VRFDevice: "vr-vs-dns"},
			},
			Answered: 5, Servfail: 2, SendFail: 1,
		}},
	})
	token := loginAdmin(t, ts)
	enableDNSProxyViaSwitch(t, ts.URL, token, "vs-dns", "192.168.99.1/24")

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/dns/proxy", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /dns/proxy: %d %s", status, data)
	}
	body := string(data)
	if !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"runtime"`) {
		t.Fatalf("内核数据面 REST 读视图应启用并带 runtime：%s", body)
	}
	for _, want := range []string{
		`"domains"`, `"name":"vs-dns"`, `"addresses":["192.168.99.1"]`,
		`"upstreams":["10.0.0.53"]`, `"vrf_device":"vr-vs-dns"`,
		`"answered":5`, `"servfail":2`, `"send_fail":1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("REST runtime 应含 %q 且与 CLI 同源：%s", want, body)
		}
	}
}

// VPP 数据面：CLI 读视图逐字保持原样（punt/独占措辞在、无内核机制句/运行态块），
// REST 不发 runtime——即使读物被注入也不出现（运行态块是内核数据面专属）。
func TestDNSProxyVPPNoRuntimeBlock(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set system dns proxy server 8.8.8.8", "commit", "exit")
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}})
	x.setDNSProxy(&fakeDNSProxyRuntime{ok: true, st: network.DNSProxyState{Answered: 9}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show dns proxy").Output
	if !strings.Contains(out, "8.8.8.8") || !strings.Contains(out, "由 nfvisd 独占") ||
		!strings.Contains(out, "VPP punt 节点丢弃") {
		t.Fatalf("VPP 数据面读视图应保持原措辞：\n%s", out)
	}
	// 逐字金样：VPP 数据面的输出与引入运行态块**之前**的实现在本用例现场逐字节一致
	// （golden 即当时措辞；改它应当是有意的用户可见变更，不在本次改动范围内）。
	if want := "数据面 DNS 代理: 启用（域内客户端把 resolver 指向网关即可解析；上游经宿主网络栈发起）\n" +
		"全局上游:\n  8.8.8.8\n" +
		"说明: 本视图只读产品配置声明。边界：纯 UDP 转发（客户端用 TCP 查 DNS 不生效）；不缓存；\n" +
		"      不预检上游可达性（上游不可达/超时运行期回 SERVFAIL 并计数）；启用期间指向产品地址的\n" +
		"      UDP/53 由 nfvisd 独占，nfvisd 不在时这些包被 VPP punt 节点丢弃（域内 DNS 中断）；\n" +
		"      覆盖 IPv4/UDP/53（IPv6 的解析查询尚未覆盖——punt 注册按地址族）。\n"; out != want {
		t.Fatalf("VPP 数据面读视图应与既有实现逐字一致，实得：\n%s", out)
	}
	for _, bad := range []string{"运行态:", "nfvisd 内的转发器", "内核数据面（Linux 内核网络）"} {
		if strings.Contains(out, bad) {
			t.Fatalf("VPP 数据面不应出现内核侧内容 %q：\n%s", bad, out)
		}
	}

	ts := newTestServerOpts(t, Options{
		VPP:      fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}},
		DNSProxy: &fakeDNSProxyRuntime{ok: true, st: network.DNSProxyState{Answered: 9}},
	})
	token := loginAdmin(t, ts)
	enableDNSProxyViaSwitch(t, ts.URL, token, "vs-vpp", "192.168.99.1/24")
	_, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/dns/proxy", token, nil, nil)
	if strings.Contains(string(data), `"runtime"`) {
		t.Fatalf("VPP 数据面不应出现 runtime：%s", data)
	}
}

// 读物已注入但代理未启用 ⇒ 不出现运行态块/字段（配置视图如实「未配置」）。
func TestDNSProxyKernelDisabledNoRuntimeBlock(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set system dataplane kernel", "commit", "exit")
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	x.setDNSProxy(&fakeDNSProxyRuntime{ok: true, st: network.DNSProxyState{Answered: 9}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show dns proxy").Output
	if !strings.Contains(out, "未配置") {
		t.Fatalf("未配置时应如实报未配置：\n%s", out)
	}
	if strings.Contains(out, "运行态:") {
		t.Fatalf("未启用时不应出现运行态块：\n%s", out)
	}

	ts := newTestServerOpts(t, Options{
		VPP:      fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}},
		DNSProxy: &fakeDNSProxyRuntime{ok: true, st: network.DNSProxyState{Answered: 9}},
	})
	token := loginAdmin(t, ts)
	_, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/dns/proxy", token, nil, nil)
	if strings.Contains(string(data), `"runtime"`) {
		t.Fatalf("未启用时不应出现 runtime：%s", data)
	}
}

// 未注入读物（VPP 装配的常态）：内核口径说明块照常，但无运行态块——行为与引入运行态块前一致；
// REST 只有配置视图三字段（enabled/servers/switches）。
func TestDNSProxyKernelNoRuntimeInjected(t *testing.T) {
	x, _ := newCLIKit(t)
	kernelDNSProxyCfg(t, x)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show dns proxy").Output
	if !strings.Contains(out, "8.8.8.8") || !strings.Contains(out, "内核数据面（Linux 内核网络）") {
		t.Fatalf("未注入读物时配置视图应照常：\n%s", out)
	}
	if strings.Contains(out, "运行态:") {
		t.Fatalf("未注入读物时不应出现运行态块：\n%s", out)
	}

	ts := newTestServerOpts(t, Options{VPP: fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}}})
	token := loginAdmin(t, ts)
	enableDNSProxyViaSwitch(t, ts.URL, token, "vs-plain", "192.168.99.1/24")
	_, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/dns/proxy", token, nil, nil)
	if strings.Contains(string(data), `"runtime"`) {
		t.Fatalf("未注入读物时不应出现 runtime：%s", data)
	}
}

// 注记措辞：机制句 + 每落点一行 + 计数；落点集为空时如实说「当前无服务落点」；
// 用户可见文本不得出现决策号（archtest 的 user_text 守护在此提前拦住）。
func TestKernelDNSProxyNoteWording(t *testing.T) {
	note := kernelDNSProxyNote(network.DNSProxyState{
		Domains: []network.DNSProxyDomainState{{
			Name: "vs-a", Addresses: []string{"10.0.0.1", "10.0.0.2"},
			Upstreams: []string{"1.1.1.1"}, VRFDevice: "vr-vs-a",
		}},
		Answered: 11, Servfail: 4, SendFail: 2,
	})
	for _, want := range []string{
		"nfvisd 内的转发器", "UDP/53", "按域优先", "宿主网络栈", "SERVFAIL",
		"域 vs-a（vr-vs-a）：10.0.0.1、10.0.0.2 ← 上游 1.1.1.1",
		"已应答 11 / 已回 SERVFAIL 4 / 回包失败 2",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("注记应含 %q：%s", want, note)
		}
	}
	if empty := kernelDNSProxyNote(network.DNSProxyState{}); !strings.Contains(empty, "当前无服务落点") {
		t.Fatalf("无落点时应如实说明：%s", empty)
	}
	for _, n := range []string{note, kernelDNSProxyNote(network.DNSProxyState{})} {
		if strings.Contains(n, "#") {
			t.Fatalf("用户可见文本不应含决策号：%s", n)
		}
	}
}

// enableDNSProxyViaSwitch 经 REST 建一台带按域上游的 L2 交换机（其网关即内核侧 IPv4 落点），
// 使 DNS 代理读视图的 enabled 为真。
func enableDNSProxyViaSwitch(t *testing.T, baseURL, token, name, gw string) {
	t.Helper()
	vs := model.VirtualSwitch{Name: name, Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{gw}},
		DNSProxyServers: []string{"10.0.0.53"}}
	status, _, data := cfgRequest(t, http.MethodPost, baseURL+APIPrefix+"/virtual-switches", token, vs,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建带按域上游的交换机应 201: %d %s", status, data)
	}
}
