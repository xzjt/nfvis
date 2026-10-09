package api

// 决策 #435：内核数据面下 learn-limit 的读视图——如实给出「已学条数 / 声明上限」并注明
// 「阈值告警、非强制上限」（内核 bridge 没有学习条数上限原语）。VPP 侧不加该注记（硬性上限）。

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// CLI `show virtual-switches <n> detail`：内核数据面下带 learn_limit_note（含已学/上限与口径）；
// 计数取不到时如实说不可读，且不编造 mac_table_entries。
func TestCLIVSwitchLearnLimitKernelNote(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set virtual-switches vs-ll type l2",
		"set virtual-switches vs-ll learn-limit 8192",
		"commit", "exit",
	)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	x.setVppState(fakeVppState{bds: []BridgeDomainState{{ID: 11, Name: "vs-ll", Learn: true, Flood: true}}})
	x.setNetRuntime(&fakeL2Runtime{rows: []MACTableRow{
		{MAC: "02:00:00:00:00:01", Port: "ens192"},
		{MAC: "02:00:00:00:00:02", Port: "ens192"},
	}}, nil, nil, nil, nil)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-ll detail").Output
	for _, want := range []string{"learn-limit 8192", "learn-limit-note", "阈值告警", "非强制上限", "已学 2 条"} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核数据面详情应含 %q：\n%s", want, out)
		}
	}

	// 计数取不到：如实说不可读，不编造 mac-table-entries。
	x.setNetRuntime(&fakeL2Runtime{err: errors.New("bridge: command failed")}, nil, nil, nil, nil)
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-ll detail").Output
	if !strings.Contains(out, "已学条数不可读") {
		t.Fatalf("计数取不到应如实说明：\n%s", out)
	}
	if strings.Contains(out, "mac-table-entries") {
		t.Fatalf("计数取不到时不得编造 mac-table-entries：\n%s", out)
	}
}

// VPP 数据面下不加该注记（读视图行为逐字不变）：只有 learn_limit，没有 learn_limit_note。
func TestCLIVSwitchLearnLimitVPPNoNote(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set virtual-switches vs-ll type l2",
		"set virtual-switches vs-ll learn-limit 8192",
		"commit", "exit",
	)
	x.setVppState(fakeVppState{bds: []BridgeDomainState{{ID: 11, Name: "vs-ll", Learn: true, Flood: true}}})
	x.setNetRuntime(&fakeL2Runtime{rows: []MACTableRow{{MAC: "02:00:00:00:00:01", Port: "ens192"}}}, nil, nil, nil, nil)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-ll detail").Output
	if !strings.Contains(out, "learn-limit 8192") {
		t.Fatalf("VPP 详情应含 learn-limit：\n%s", out)
	}
	if strings.Contains(out, "learn-limit-note") {
		t.Fatalf("VPP 数据面不应加 learn-limit-note：\n%s", out)
	}
}

// REST `GET /virtual-switches/{name}`：内核数据面下同源带 learn_limit_note。
func TestGetVSwitchLearnLimitKernelNote(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		VPP: fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}},
		L2: &fakeL2Runtime{rows: []MACTableRow{
			{MAC: "02:00:00:00:00:01", Port: "ens192"},
			{MAC: "02:00:00:00:00:02", Port: "ens192"},
			{MAC: "02:00:00:00:00:03", Port: "ens192"},
		}},
	})
	token := loginAdmin(t, ts)
	vs := model.VirtualSwitch{Name: "vs-ll", Type: "l2", LearnLimit: 100}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token, vs,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("创建带 learn-limit 的交换机应 201: %d %s", status, data)
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-ll", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 详情: %d %s", status, data)
	}
	body := string(data)
	if !strings.Contains(body, `"learn_limit":100`) || !strings.Contains(body, "learn_limit_note") {
		t.Fatalf("内核数据面 REST 详情应带 learn_limit + learn_limit_note：%s", body)
	}
	if !strings.Contains(body, "阈值告警") || !strings.Contains(body, "已学 3 条") {
		t.Fatalf("REST 注记应含口径与已学条数：%s", body)
	}
}
