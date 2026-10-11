package api

// 决策 #450（LLDP 语句族收敛）与 #451（LLDP 全局启用语义）的守护。
//
// 由来（round12 真机走查 R12-2 / R12-5）：`protocols lldp` 族四处坏——
//   ① 值可选/裸形态落通用遍历，回「语句未产生配置变更」；
//   ② 逐口声明在全局开关为 false 时不下发；
//   ③ `delete protocols lldp interface <if> enable`（5 token）落通用遍历按单数键找、模型是复数；
//   ④ 通用遍历错误按遍历 token 报路径，归因与操作无关。
// 本文件逐条钉住修复（红-绿：修复前这些断言必失败）。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ---------- ① 值可选/裸形态 ----------

func TestLLDPBareEnableForms(t *testing.T) {
	x, eng := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens192",
		"set protocols lldp enable",
		"commit")
	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Protocols == nil || cfg.Protocols.LLDP == nil || !cfg.Protocols.LLDP.Enabled {
		t.Fatalf("裸 `enable` 应落 enabled=true（此前回「语句未产生配置变更」）: %+v", cfg.Protocols)
	}

	// 裸逐口 enable（值可选，5 token）：此前落通用遍历报「配置不完整，缺少取值」。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set protocols lldp interface ens192 enable",
		"commit")
	cfg, err = eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	li := cfg.Protocols.LLDP.Interfaces
	if len(li) != 1 || li[0].Interface != "ens192" || !li[0].Enabled {
		t.Fatalf("裸逐口 `enable` 应落该口 enabled=true: %+v", li)
	}

	// 幂等重跑：是**提示**（Warning）而非错误（脚本继续）——值已是目标值。
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "set protocols lldp interface ens192 enable")
	if !res.Warning || strings.Contains(res.Output, "%%") {
		t.Fatalf("幂等重跑应为提示而非错误: warning=%v out=%q", res.Warning, res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "set protocols lldp enable")
	if !res.Warning || strings.Contains(res.Output, "%%") {
		t.Fatalf("全局裸 enable 幂等重跑应为提示而非错误: warning=%v out=%q", res.Warning, res.Output)
	}
}

// ---------- ③ 叶子级 delete 幂等 ----------

func TestLLDPLeafDeleteIdempotent(t *testing.T) {
	x, eng := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens192",
		"set protocols lldp enable true",
		"set protocols lldp interface ens192 enable true",
		"commit")

	// 5-token 叶子级 delete：此前落通用遍历按单数键找、报「无匹配配置: interface」。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"delete protocols lldp interface ens192 enable",
		"commit")
	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Protocols == nil || cfg.Protocols.LLDP == nil {
		t.Fatal("删逐口覆盖后全局 lldp 段应仍在")
	}
	if len(cfg.Protocols.LLDP.Interfaces) != 0 {
		t.Fatalf("叶子级 delete 应移除该口覆盖条目: %+v", cfg.Protocols.LLDP.Interfaces)
	}

	// 重复删除（条目已不在）＝ 幂等，不报错。
	for _, line := range []string{
		"delete protocols lldp interface ens192 enable",
		"delete protocols lldp interface ens999 enable", // 从未声明的口
		"delete protocols lldp enable",
	} {
		if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", line); strings.Contains(res.Output, "%%") {
			t.Fatalf("%q 应幂等（键不存在＝已达成）: %q", line, res.Output)
		}
	}
}

// ---------- ④ 通用遍历错误引用完整语句 ----------

func TestLLDPGenericTraversalErrorQuotesFullStatement(t *testing.T) {
	x, _ := newCLIKit(t)
	x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure")
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete virtual-switches vs-x")
	if !strings.Contains(res.Output, "无匹配配置: virtual-switches vs-x") {
		t.Fatalf("通用遍历错误应引用完整语句而非遍历 token: %q", res.Output)
	}
}

// ---------- ⑤ 整段删除与同族形态一致幂等（决策 #450） ----------

// 无任何 LLDP 配置时，整段删与同族各叶子删都应幂等（不 `%%`、脚本可继续）——此前整段删
// `delete protocols lldp` 落通用遍历，段不存在时报「无匹配配置」而中止脚本。
func TestLLDPWholeSegmentAndSiblingsDeleteIdempotent(t *testing.T) {
	x, eng := newCLIKit(t)
	x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure")
	for _, line := range []string{
		"delete protocols lldp",
		"delete protocols lldp enable",
		"delete protocols lldp advertisement-interval",
		"delete protocols lldp interface ens192",
		"delete protocols lldp interface ens192 enable",
	} {
		if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", line); strings.Contains(res.Output, "%%") {
			t.Fatalf("%q 在无配置时应幂等（不报错）: %q", line, res.Output)
		}
	}

	// 建全套配置后逐条删除应**正常生效**（present ⇒ 正常删除语义）。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens192",
		"set protocols lldp enable true",
		"set protocols lldp advertisement-interval 30",
		"set protocols lldp interface ens192 enable true",
		"commit")

	// 无 enable 的逐口删（4 token）：移除该口覆盖条目。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete protocols lldp interface ens192", "commit")
	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Protocols == nil || cfg.Protocols.LLDP == nil || len(cfg.Protocols.LLDP.Interfaces) != 0 {
		t.Fatalf("delete ... interface <if> 应移除该口覆盖条目: %+v", cfg.Protocols)
	}

	// 无值删 advertisement-interval。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete protocols lldp advertisement-interval", "commit")
	cfg, err = eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Protocols.LLDP.AdvertisementInterval != 0 {
		t.Fatalf("delete advertisement-interval 应清除间隔: %+v", cfg.Protocols.LLDP)
	}

	// 整段删：protocols 归 nil（不留空壳）。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete protocols lldp", "commit")
	cfg, err = eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Protocols != nil {
		t.Fatalf("整段删后不应留下空 protocols 对象: %+v", *cfg.Protocols)
	}
}

// ---------- 读视图：实际在发广告的口 + 内核运行态（决策 #451） ----------

type fakeLLDPState struct {
	st network.LLDPState
	ok bool
}

func (f fakeLLDPState) LLDPState() (network.LLDPState, bool) { return f.st, f.ok }

func TestLldpReadViewAdvertisingAndRuntime(t *testing.T) {
	rt := network.LLDPState{Enabled: true, Interface: []network.LLDPInterfaceState{
		{Name: "ens2f0", Converged: true, MAC: "aa:bb:cc:dd:ee:01"},
		{Name: "ens2f1", Converged: false, Error: "打开收发 socket 失败: 注入"},
	}}
	ts := newTestServerOpts(t, Options{LLDPState: fakeLLDPState{st: rt, ok: true}})
	token := loginAdmin(t, ts)
	seedIface(t, ts, token, "ens2f0")
	seedIface(t, ts, token, "ens2f1")

	// 只开全局开关、不写逐口声明：作用集应是**全部已声明物理口**。
	lldp := model.LldpConfig{Enabled: true, AdvertisementInterval: 30}
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/protocols/lldp", token, lldp,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT lldp: %d", status)
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/protocols/lldp", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET lldp: %d", status)
	}
	var view struct {
		Enabled     bool     `json:"enabled"`
		Advertising []string `json:"advertising"`
		Runtime     *struct {
			Enabled    bool `json:"enabled"`
			Interfaces []struct {
				Name      string `json:"name"`
				Converged bool   `json:"converged"`
				Error     string `json:"error"`
			} `json:"interfaces"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("解析读视图: %v / %s", err, data)
	}
	if !view.Enabled {
		t.Fatal("读视图应回 enabled=true")
	}
	got := strings.Join(view.Advertising, ",")
	// 运行态可读时以运行态为准：只有已收敛的口才算「在发广告」。
	if got != "ens2f0" {
		t.Fatalf("运行态可读时 advertising 只应含已收敛的口: %q", got)
	}
	if view.Runtime == nil || len(view.Runtime.Interfaces) != 2 {
		t.Fatalf("内核运行态块应给出每口状态: %+v", view.Runtime)
	}
	if view.Runtime.Interfaces[1].Converged || !strings.Contains(view.Runtime.Interfaces[1].Error, "注入") {
		t.Fatalf("运行态应如实报未收敛口: %+v", view.Runtime.Interfaces[1])
	}
}

// VPP 侧：全局启用扫到被底座跳过（DPDK 物理口 -7）的口 ⇒ advertising 只列在发的口，
// runtime 如实给出被跳过口及原因（决策 #451 精化）。
func TestLldpReadViewVppSkippedPorts(t *testing.T) {
	rt := network.LLDPState{Enabled: true, Interface: []network.LLDPInterfaceState{
		{Name: "ens2f0", Converged: true},
		{Name: "ens2f1", Converged: false,
			Error: "sw_interface_set_lldp(if=ens2f1,enable=true) retval=-7：当前数据面底座不支持在该接口上启用 LLDP（DPDK 物理口；vhost-user/BVI 等虚拟接口可用，或改用内核数据面）"},
	}}
	ts := newTestServerOpts(t, Options{LLDPState: fakeLLDPState{st: rt, ok: true}})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/protocols/lldp", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET lldp: %d", status)
	}
	var view struct {
		Advertising []string `json:"advertising"`
		Runtime     *struct {
			Interfaces []struct {
				Name      string `json:"name"`
				Converged bool   `json:"converged"`
				Error     string `json:"error"`
			} `json:"interfaces"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("解析读视图: %v / %s", err, data)
	}
	if got := strings.Join(view.Advertising, ","); got != "ens2f0" {
		t.Fatalf("被底座跳过的口不该出现在 advertising（不谎报在发广告）: %q", got)
	}
	if view.Runtime == nil || len(view.Runtime.Interfaces) != 2 {
		t.Fatalf("runtime 应同时列出在发口与被跳过口: %+v", view.Runtime)
	}
	sk := view.Runtime.Interfaces[1]
	if sk.Name != "ens2f1" || sk.Converged || !strings.Contains(sk.Error, "-7") {
		t.Fatalf("被跳过口应如实给出原因: %+v", sk)
	}
}

// VPP 数据面（未注入 LLDPState）⇒ 无 runtime 块；advertising 为配置派生值（全局 enable
// 时＝全部已声明物理口按逐口覆盖后的结果）。
func TestLldpReadViewAdvertisingConfigDerived(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedIface(t, ts, token, "ens2f0")
	seedIface(t, ts, token, "ens2f1")

	lldp := model.LldpConfig{Enabled: true, AdvertisementInterval: 30}
	status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/protocols/lldp", token, lldp,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("PUT lldp: %d", status)
	}
	var view struct {
		Advertising []string `json:"advertising"`
	}
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/protocols/lldp", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET lldp: %d", status)
	}
	if strings.Contains(string(data), "\"runtime\"") {
		t.Fatalf("未注入运行态读物时不该出现 runtime 字段: %s", data)
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("解析读视图: %v / %s", err, data)
	}
	if got := strings.Join(view.Advertising, ","); got != "ens2f0,ens2f1" {
		t.Fatalf("全局 enable 时 advertising 应为全部已声明物理口（升序）: %q", got)
	}

	// 逐口 false 覆盖：ens2f1 从作用集移除。
	lldp.Interfaces = []model.LldpInterface{{Interface: "ens2f1", Enabled: false}}
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/protocols/lldp", token, lldp,
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("PUT lldp(覆盖): %d", status)
	}
	_, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/protocols/lldp", token, nil, nil)
	view.Advertising = nil
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(view.Advertising, ","); got != "ens2f0" {
		t.Fatalf("逐口 false 应把该口移出作用集: %q", got)
	}
}
