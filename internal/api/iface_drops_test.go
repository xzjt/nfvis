package api

// 决策 #504：接口丢弃计数如实化——**四出口同源**守护。
//
// 由来（真机 round47）：`state.InterfaceCounters` 声明了 RxDrops/TxDrops 但全仓没有填充者，
// 于是 CLI `show interfaces <i> statistics`、CLI `monitor interfaces`、REST
// `GET /interfaces/{n}` 的 statistics、`/metrics` 的 `nfvis_vpp_interface_{rx,tx}_drops`
// **四处恒 0**，而同一时刻 `vpp_get_stats /interfaces/ens192/drops=148`。
//
// 本文件钉三件事：
//  1. **同一个假计数**喂给四个出口，丢弃数字逐字一致（四出口同源，不是各算一遍）；
//  2. **VPP 口径**只发 `drops`（`rx_drops`/`tx_drops` 键缺席，不是 0）；
//  3. **内核口径**反过来（两方向键、无 `drops`）——两数据面不混口径。

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

// dropsFixtureRuntime VPP 口径的假运行态：丢弃是**方向无关的总数**。
type dropsFixtureRuntime struct{ drops uint64 }

func (f dropsFixtureRuntime) Threads(context.Context) ([]state.Thread, error) { return nil, nil }
func (f dropsFixtureRuntime) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return state.InterfaceCounters{RxPackets: 85, TxPackets: 3, Drops: f.drops}, true
}
func (f dropsFixtureRuntime) Buffers(context.Context) (state.Buffers, bool) {
	return state.Buffers{}, false
}
func (f dropsFixtureRuntime) Memory(context.Context) (state.Memory, bool) {
	return state.Memory{}, false
}
func (f dropsFixtureRuntime) RuntimeStats(context.Context) (state.RuntimeStats, bool) {
	return state.RuntimeStats{}, false
}

// kernelFixtureCounters 内核口径的假计数：丢弃按方向。
type kernelFixtureCounters struct{ rx, tx uint64 }

func (f kernelFixtureCounters) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return state.InterfaceCounters{
		RxPackets: 10, TxPackets: 20, RxDrops: f.rx, TxDrops: f.tx, DirectionalDrops: true,
	}, true
}

// TestInterfaceDropsFourExitsSameValue 同一个假计数 ⇒ CLI 两处 + REST + /metrics 数字逐字一致，
// 且 VPP 口径下四处**都不出现**方向丢弃数字（13/14 这类编造值一个都不能漏出去）。
func TestInterfaceDropsFourExitsSameValue(t *testing.T) {
	const drops = 148
	tsv := newTestServerOpts(t, Options{State: state.New(dropsFixtureRuntime{drops: drops})})
	token := loginAdmin(t, tsv)
	if status, _, body := cfgRequest(t, http.MethodPut, tsv.URL+APIPrefix+"/interfaces/ens192", token,
		model.InterfaceConfig{Name: "ens192"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("预置接口: %d %s", status, body)
	}
	want := strconv.Itoa(drops)

	// ① REST GET /interfaces/ens192 的 statistics.drops
	status, _, data := cfgRequest(t, http.MethodGet, tsv.URL+APIPrefix+"/interfaces/ens192", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 接口: %d %s", status, data)
	}
	var view struct {
		Statistics map[string]any `json:"statistics"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("解析接口视图: %v %s", err, data)
	}
	if view.Statistics == nil {
		t.Fatalf("详情端点应附带 statistics：%s", data)
	}
	if got, ok := view.Statistics["drops"].(float64); !ok || got != drops {
		t.Fatalf("REST statistics.drops = %v（应为 %d）：%s", view.Statistics["drops"], drops, data)
	}
	for _, k := range []string{"rx_drops", "tx_drops"} {
		if _, ok := view.Statistics[k]; ok {
			t.Fatalf("VPP 口径下 REST statistics 不得含 %q（该口径不适用）：%s", k, data)
		}
	}

	// ② /metrics 的单条 drops 序列（旧实现是两条恒 0 的 rx_drops/tx_drops）
	body := getMetricsBody(t, tsv.URL)
	wantLine := `nfvis_vpp_interface_drops{interface="ens192"} ` + want
	if !strings.Contains(body, wantLine) {
		t.Fatalf("/metrics 应含 %q：\n%s", wantLine, body)
	}
	for _, name := range []string{"nfvis_vpp_interface_rx_drops", "nfvis_vpp_interface_tx_drops"} {
		if strings.Contains(body, name+"{") {
			t.Fatalf("VPP 口径下 /metrics 不得再发 %q（方向不可知，且它是恒 0 的老缺陷）：\n%s", name, body)
		}
	}

	// ③④ CLI 两处（statistics / monitor）——CLI 与 REST/metrics 共用同一个 state，
	// 故三处数字必然同源；此处显式逐字核对，防将来有人给某一路单独取数。
	x, _ := newCLIKit(t)
	x.setRuntime(nil, state.New(dropsFixtureRuntime{drops: drops}))
	statsOut := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens192 statistics").Output
	if !strings.Contains(statsOut, want) {
		t.Fatalf("CLI statistics 应含 %s：\n%s", want, statsOut)
	}
	monOut := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces ens192").Output
	if !strings.Contains(monOut, want) {
		t.Fatalf("CLI monitor 应含 %s：\n%s", want, monOut)
	}
	// 四处的**列/键名**也同一口径：没有方向丢弃列。
	for name, out := range map[string]string{"statistics": statsOut, "monitor": monOut} {
		if strings.Contains(out, "rx-drops") || strings.Contains(out, "tx-drops") {
			t.Fatalf("CLI %s 不得再有方向丢弃列（VPP 口径不适用）：\n%s", name, out)
		}
	}
}

// TestInterfaceDropsKernelShape REST 在内核口径下：发 rx_drops/tx_drops、不发 drops。
//
// 这条走的是**服务端装配路径**（state.NewCounters + 内核计数源），不是只测 struct：
// 若有人把内核侧的计数接错（比如仍装配 state.New(mgr.Runtime())），这里会红。
func TestInterfaceDropsKernelShape(t *testing.T) {
	tsv := newTestServerOpts(t, Options{State: state.NewCounters(kernelFixtureCounters{rx: 7, tx: 8})})
	token := loginAdmin(t, tsv)
	if status, _, body := cfgRequest(t, http.MethodPut, tsv.URL+APIPrefix+"/interfaces/ens192", token,
		model.InterfaceConfig{Name: "ens192"}, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("预置接口: %d %s", status, body)
	}
	status, _, data := cfgRequest(t, http.MethodGet, tsv.URL+APIPrefix+"/interfaces/ens192", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET 接口: %d %s", status, data)
	}
	var view struct {
		Statistics map[string]any `json:"statistics"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("解析接口视图: %v %s", err, data)
	}
	if view.Statistics == nil {
		t.Fatalf("详情端点应附带 statistics：%s", data)
	}
	for k, want := range map[string]float64{"rx_drops": 7, "tx_drops": 8} {
		if got, ok := view.Statistics[k].(float64); !ok || got != want {
			t.Fatalf("内核口径下 REST statistics.%s = %v（应为 %v）：%s", k, view.Statistics[k], want, data)
		}
	}
	if _, ok := view.Statistics["drops"]; ok {
		t.Fatalf("内核口径下 REST statistics 不得含 drops（两方向不相加冒充总数）：%s", data)
	}

	// /metrics 同口径：两条方向序列，无 drops。
	body := getMetricsBody(t, tsv.URL)
	if !strings.Contains(body, `nfvis_vpp_interface_rx_drops{interface="ens192"} 7`) {
		t.Fatalf("/metrics 应含内核口径的 rx_drops=7：\n%s", body)
	}
	if !strings.Contains(body, `nfvis_vpp_interface_tx_drops{interface="ens192"} 8`) {
		t.Fatalf("/metrics 应含内核口径的 tx_drops=8：\n%s", body)
	}
	if strings.Contains(body, `nfvis_vpp_interface_drops{`) {
		t.Fatalf("内核口径下 /metrics 不得发 nfvis_vpp_interface_drops（不相加冒充总数）：\n%s", body)
	}
}

// TestInterfaceDropsCLIKernelShape CLI 在内核口径下表头与列值按方向（两列）。
func TestInterfaceDropsCLIKernelShape(t *testing.T) {
	x := kernelDPKit(t)
	x.setRuntime(nil, state.NewCounters(kernelFixtureCounters{rx: 7, tx: 8}))
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens192 statistics").Output
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("应为「标题 + 表头 + 数据行」：%q", out)
	}
	wantHead := "Interface rx-pkts tx-pkts rx-bytes tx-bytes rx-errors tx-errors rx-drops tx-drops"
	if got := strings.Join(strings.Fields(lines[1]), " "); got != wantHead {
		t.Fatalf("内核口径表头应为 %q，实得 %q", wantHead, got)
	}
	row := strings.Join(strings.Fields(lines[2]), " ")
	if row != "ens192 10 20 0 0 0 0 7 8" {
		t.Fatalf("内核口径数据行应为 %q，实得 %q", "ens192 10 20 0 0 0 0 7 8", row)
	}
	// 结构化输出同口径（`| display json` 与 REST Web 同源消费）。
	js := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens192 statistics | display json").Output
	for _, k := range []string{"rx_drops", "tx_drops"} {
		if !strings.Contains(js, `"`+k+`"`) {
			t.Fatalf("内核口径结构化输出应含 %q：%q", k, js)
		}
	}
	if strings.Contains(js, `"drops"`) {
		t.Fatalf("内核口径结构化输出不得含 drops：%q", js)
	}
}
