package api

// 决策 #383：VXLAN overlay v1 的语句 → 模型 → 读视图（CLI/REST 同源）与 display set 反推。
//
// 运行态用假读物注入（不依赖数据面）：按元组键命中/未命中两种行态都要能如实呈现，
// 读数不可用时**不得**把 in_vpp 报成 false 冒充「未收敛」（与 GET /vxlan-tunnels 的
// runtime_available 同口径）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakeVxlanRuntime 假运行态读物：states 为「实际存在」的隧道（元组键索引）。
type fakeVxlanRuntime struct {
	states map[string]network.VxlanState
	err    error
}

func (f fakeVxlanRuntime) VxlanStates(context.Context) (map[string]network.VxlanState, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.states, nil
}

func TestCLIVxlanStatementAndView(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-vx type l2",
		"set vxlan tunnels tun-a vni 100 local 10.99.0.1 remote 10.99.0.2",
		"set vxlan tunnels tun-b vni 200 local 10.99.0.1 remote 10.99.0.3 dst-port 5789 virtual-switch vs-vx",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.VxlanTunnels) != 2 {
		t.Fatalf("隧道应写入 vxlan_tunnels: %+v", cfg.VxlanTunnels)
	}
	if cfg.VxlanTunnels[1].DstPort != 5789 || cfg.VxlanTunnels[1].VirtualSwitch != "vs-vx" {
		t.Fatalf("可选叶子应落库: %+v", cfg.VxlanTunnels[1])
	}
	if cfg.VxlanTunnels[0].EffectiveDstPort() != model.VxlanDefaultDstPort {
		t.Fatalf("缺省端口应回落 4789: %+v", cfg.VxlanTunnels[0])
	}
	run(t, x, "admin", aaaClassSU, "ssh", "commit", "exit")

	// 运行态未注入：如实报「运行态不可用」，不把两条报成未收敛
	out := x.Execute("admin", aaaClassSU, "ssh", "show vxlan tunnels").Output
	if !strings.Contains(out, "tun-a") || !strings.Contains(out, "tun-b") ||
		!strings.Contains(out, "5789") || !strings.Contains(out, "vs-vx") {
		t.Fatalf("show vxlan tunnels 应列出声明字段:\n%s", out)
	}
	if !strings.Contains(out, "运行态不可用") {
		t.Fatalf("未注入运行态时应如实报不可用:\n%s", out)
	}

	// 注入假运行态：tun-a 命中（元组含缺省端口 4789），tun-b 未命中 ⇒ 未收敛
	x.vxlan = fakeVxlanRuntime{states: map[string]network.VxlanState{
		network.VxlanTupleKey(100, "10.99.0.1", "10.99.0.2", 4789): {Instance: 0, SwIfIndex: 6},
	}}
	out = x.Execute("admin", aaaClassSU, "ssh", "show vxlan tunnels").Output
	if !strings.Contains(out, "已在 VPP") || !strings.Contains(out, "sw_if_index 6") {
		t.Fatalf("命中元组的隧道应显示已在 VPP:\n%s", out)
	}
	if !strings.Contains(out, "未收敛（数据面中不存在）") {
		t.Fatalf("未命中的隧道应如实报未收敛:\n%s", out)
	}

	// 读数失败：同样如实报运行态不可用（不把自己说不清的事报成未收敛）
	x.vxlan = fakeVxlanRuntime{err: errors.New("数据面连接不可用")}
	out = x.Execute("admin", aaaClassSU, "ssh", "show vxlan tunnels").Output
	if !strings.Contains(out, "运行态不可用") || strings.Contains(out, "未收敛（数据面中不存在）") {
		t.Fatalf("读数失败时应报运行态不可用而不是未收敛:\n%s", out)
	}

	// 语法错误的分支
	if out := x.Execute("admin", aaaClassSU, "ssh", "show vxlan").Output; !strings.Contains(out, "语法") {
		t.Fatalf("show vxlan 应给出语法指引:\n%s", out)
	}

	// display set 反推（回放自校验在渲染内完成；任一语句含可选叶子）
	x.vxlan = nil
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	for _, want := range []string{
		"set vxlan tunnels tun-a vni 100 local 10.99.0.1 remote 10.99.0.2",
		"set vxlan tunnels tun-b vni 200 local 10.99.0.1 remote 10.99.0.3 dst-port 5789 virtual-switch vs-vx",
	} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("display set 应反推出 %q:\n%s", want, res.Output)
		}
	}
}

func TestRESTVxlanTunnelsView(t *testing.T) {
	rt := fakeVxlanRuntime{states: map[string]network.VxlanState{
		network.VxlanTupleKey(100, "10.99.0.1", "10.99.0.2", 4789): {Instance: 2, SwIfIndex: 7},
	}}
	ts := newTestServerOpts(t, Options{Vxlan: rt})
	token := loginAdmin(t, ts)

	rootCfg := withSuperUser(model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-vx", Type: "l2"}},
		VxlanTunnels: []model.VxlanTunnel{
			{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2", VirtualSwitch: "vs-vx"},
			{Name: "tun-b", Vni: 200, Local: "10.99.0.1", Remote: "10.99.0.3", DstPort: 5789},
		},
	})
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		rootCfg, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("提交隧道配置: %d %s", status, data)
	}

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vxlan-tunnels", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /vxlan-tunnels: %d %s", status, data)
	}
	var got vxlanTunnelsView
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, data)
	}
	if !got.RuntimeAvailable || got.RuntimeReason != "" || len(got.Tunnels) != 2 {
		t.Fatalf("读视图头不符: %+v", got)
	}
	a, b := got.Tunnels[0], got.Tunnels[1]
	if a.Name != "tun-a" || !a.InVPP || a.SwIfIndex != 7 || a.Instance != 2 || a.DstPort != 4789 {
		t.Fatalf("命中元组应带运行态: %+v", a)
	}
	if b.Name != "tun-b" || b.InVPP || b.DstPort != 5789 {
		t.Fatalf("未命中应为未收敛且端口取声明值: %+v", b)
	}
	// /configuration 同步包含 vxlan_tunnels
	status, _, cfgData := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(cfgData), "vxlan_tunnels") ||
		!strings.Contains(string(cfgData), "10.99.0.2") {
		t.Fatalf("GET /configuration 应含 vxlan_tunnels: %d %s", status, cfgData)
	}
}

// 读数不可用时 REST 的如实口径：runtime_available=false + 原因，且各条不谎报 in_vpp。
func TestRESTVxlanTunnelsRuntimeUnavailable(t *testing.T) {
	ts := newTestServerOpts(t, Options{Vxlan: fakeVxlanRuntime{err: errors.New("数据面连接不可用")}})
	token := loginAdmin(t, ts)
	rootCfg := withSuperUser(model.Config{
		VxlanTunnels: []model.VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2"}},
	})
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		rootCfg, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("提交隧道配置: %d %s", status, data)
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vxlan-tunnels", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /vxlan-tunnels: %d %s", status, data)
	}
	var got vxlanTunnelsView
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got.RuntimeAvailable || !strings.Contains(got.RuntimeReason, "运行态不可用") {
		t.Fatalf("运行态不可用应如实回报: %+v", got)
	}
	if len(got.Tunnels) != 1 || got.Tunnels[0].RuntimeAvailable || got.Tunnels[0].InVPP {
		t.Fatalf("不可用时每条也不得谎报 in_vpp: %+v", got.Tunnels)
	}
}

// TestVxlanViewFieldsMatchContract 字段级形状守护（#332 的教训：路径级覆盖挡不住字段级错误）：
// CLI 与 Web 卡消费的每个 `tunnels[]` 字段都必须在契约 schema 里声明（VxlanTunnelsView 的
// items 是 allOf：VxlanTunnel + 运行态字段），否则「端点对、字段错」会静默显示空/「—」。
func TestVxlanViewFieldsMatchContract(t *testing.T) {
	spec := loadEmbeddedSpec(t)
	paths, _ := spec["paths"].(map[string]any)
	node, _ := paths["/vxlan-tunnels"].(map[string]any)
	op, _ := node["get"].(map[string]any)
	resp, _ := op["responses"].(map[string]any)
	ok, _ := resp["200"].(map[string]any)
	view := schemaOf(t, spec, ok)
	if view == nil {
		t.Fatal("契约里取不到 GET /vxlan-tunnels 的 200 schema")
	}
	// 顶层：CLI/卡/读视图头共用的三个键
	props, _ := view["properties"].(map[string]any)
	for _, f := range []string{"tunnels", "runtime_available", "runtime_reason"} {
		if _, ok := props[f]; !ok {
			t.Errorf("VxlanTunnelsView 缺少字段 %s", f)
		}
	}
	// 元素层：allOf 合并 VxlanTunnel 与运行态两段
	items, _ := props["tunnels"].(map[string]any)
	items, _ = items["items"].(map[string]any)
	var branches []any
	switch {
	case items == nil:
		t.Fatal("tunnels.items 缺失")
	case items["allOf"] != nil:
		branches, _ = items["allOf"].([]any)
	default:
		branches = []any{items}
	}
	elem := map[string]any{}
	for _, br := range branches {
		bm, _ := br.(map[string]any)
		s := schemaOf(t, spec, bm)
		p, _ := s["properties"].(map[string]any)
		for k, v := range p {
			elem[k] = v
		}
	}
	// CLI `show vxlan tunnels` 与 Web 卡消费的字段（改卡/改 CLI 时必须与契约同源）。
	for _, f := range []string{
		"name", "vni", "local", "remote", "dst_port", "virtual_switch",
		"runtime_available", "in_vpp", "sw_if_index", "instance",
	} {
		if _, ok := elem[f]; !ok {
			t.Errorf("tunnels[] 元素 schema 缺少界面消费的字段 %s（端点对、字段错＝界面恒显示「—」）", f)
		}
	}
}
