package api

// 决策 #495（收口 R41-1）：交换机「成员端口」的**两个数**必须并列且可解释。
//
// 由来（round41 真机）：同一事实「这台交换机有几个成员口」读出三个数字——列表 `ports`
// 是**配置声明的原始端口**（`handleGetVSwitches` 只序列化 committed 配置），
// `GET /virtual-switches/{name}/ports` 与 CLI `show virtual-switches <n> ports` 是
// **配置派生 ∪ VNF/容器声明派生 ∪ 运行态**的成员读视图，CLI 列表则是纯运行态。契约此前
// 对 `ports` 无一字说明，读者无从分辨。
//
// 本文件钉住 REST 侧的口径（CLI/Web 两侧各有自己的守护）：
//   - 列表条目恒有 `ports_declared_count`（＝`ports` 条数，nil 时为 0 而非省略）与
//     `ports_total_count`（＝`/ports` 的条目数）；
//   - `ports_total_count` 与 `/ports` 条目数**逐值相等**（同一实现：switchPortViews）；
//   - 运行态不可用（未注入 / 查询报错）时 `ports_total_count` 退化为配置派生条数，
//     **不报错、不省略**（不编造运行态条目）。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakeDHCPTapRuntime 只回答「哪些 sw_if_index 是产品自持的内置 DHCP tap」的运行态假实现
// （端口读视图据此过滤；租约相关方法在本守护里不被调用，如实返回空）。
type fakeDHCPTapRuntime struct{ taps map[uint32]bool }

func (f fakeDHCPTapRuntime) DHCPServerLeases(string) ([]network.DHCPLease, bool) {
	return nil, false
}
func (f fakeDHCPTapRuntime) DHCPServerActiveLeases(string) (int, bool) { return 0, false }
func (f fakeDHCPTapRuntime) DHCPTapIndexes() map[uint32]bool           { return f.taps }

// vswitchFromList GET /virtual-switches 并按名字取一条（不存在即失败——空响应什么都验不到）。
func vswitchFromList(t *testing.T, ts *httptest.Server, token, name string) map[string]any {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /virtual-switches: %d %s", status, data)
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("列表响应不是对象数组: %v %s", err, data)
	}
	for _, it := range items {
		if it["name"] == name {
			return it
		}
	}
	t.Fatalf("列表里没有交换机 %s: %s", name, data)
	return nil
}

// portsEndpointLen GET /virtual-switches/{name}/ports 的条目数（成员读视图形状）。
func portsEndpointLen(t *testing.T, ts *httptest.Server, token, name string) int {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/"+name+"/ports", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /virtual-switches/%s/ports: %d %s", name, status, data)
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("/ports 响应不是对象数组: %v %s", err, data)
	}
	return len(items)
}

// countField 取计数键（必须存在且是整数；缺失即失败——「省略该字段」正是本守护要拦的）。
func countField(t *testing.T, view map[string]any, key string) int {
	t.Helper()
	v, ok := view[key]
	if !ok {
		t.Fatalf("列表条目缺 %s（契约 VirtualSwitch 已声明；不得省略，nil 时要给 0）: %+v", key, view)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s 不是数值: %#v", key, v)
	}
	return int(f)
}

// 有派生/运行态成员时两数不等，且 ports_total_count 与 /ports 条目数逐值相等；
// 内置 DHCP tap 在两侧都被剔除（同一过滤实现），静态口与运行态同名时不重复计数。
func TestGetVSwitchesPortCountsMatchPortsEndpoint(t *testing.T) {
	ts := newTestServerOpts(t, Options{
		VppState: fakeVppState{bds: []BridgeDomainState{{ID: 9, Name: "vs-a", Ports: []BridgeDomainPort{
			{Name: "ens224", SwIfIndex: 1},    // 与配置静态口同名 ⇒ 只出一条（source=config）
			{Name: "ens300", SwIfIndex: 2},    // 仅运行态存在 ⇒ 补 source=runtime
			{Name: "dhcp-tap0", SwIfIndex: 3}, // 产品自持内置 tap ⇒ 两侧都不计（决策 #359）
		}}}},
		DHCPServer: fakeDHCPTapRuntime{taps: map[uint32]bool{3: true}},
	})
	token := loginAdmin(t, ts)
	seedSwitchPortsFixture(t, ts, token, "")

	view := vswitchFromList(t, ts, token, "vs-a")
	declared := countField(t, view, "ports_declared_count")
	total := countField(t, view, "ports_total_count")

	// 夹具：静态 1 条 → 派生 VNF 1 条 + 容器 1 条 + 运行态补条目 1 条（tap 与静态口不计）。
	if declared != 1 {
		t.Fatalf("ports_declared_count 应＝配置声明条数 1，得到 %d: %+v", declared, view)
	}
	if total != 4 {
		t.Fatalf("ports_total_count 应＝成员读视图条数 4（config+vnf+container+runtime），得到 %d: %+v", total, view)
	}
	if declared == total {
		t.Fatalf("有派生/运行态成员时两数应不等（declared=%d total=%d）——相等说明 total 取的仍是配置视图", declared, total)
	}
	// `ports` 语义不变：仍是配置声明的原始端口（写路径的回写来源）。
	ports, _ := view["ports"].([]any)
	if len(ports) != declared {
		t.Fatalf("ports 仍是配置声明（%d 条），与 ports_declared_count 不符", len(ports))
	}

	// 同源证明：与 /ports 端点条目数逐值相等（两者都走 switchPortViews）。
	if got := portsEndpointLen(t, ts, token, "vs-a"); got != total {
		t.Fatalf("ports_total_count(%d) 必须与 GET …/vs-a/ports(%d) 逐值相等（同一实现）", total, got)
	}
}

// 运行态不可用：未注入 VppState / BridgeDomains 报错两种现场，都退化为配置派生条数——
// 200、不省略字段、不编造运行态条目。
func TestGetVSwitchesPortCountsRuntimeUnavailable(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"未注入运行态", Options{}},
		{"运行态查询失败", Options{VppState: fakeVppState{bdErr: errors.New("VPP socket 不可用")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServerOpts(t, tc.opts)
			token := loginAdmin(t, ts)
			seedSwitchPortsFixture(t, ts, token, "")

			view := vswitchFromList(t, ts, token, "vs-a")
			declared := countField(t, view, "ports_declared_count")
			total := countField(t, view, "ports_total_count")
			// 配置派生 = 静态 1 + VNF 派生 1 + 容器派生 1（无运行态补条目）。
			if declared != 1 || total != 3 {
				t.Fatalf("运行态不可用时应为 declared=1 / total=3（配置派生条数），得到 %d/%d: %+v", declared, total, view)
			}
			if got := portsEndpointLen(t, ts, token, "vs-a"); got != total {
				t.Fatalf("运行态不可用时 ports_total_count(%d) 也应与 /ports(%d) 相等", total, got)
			}
		})
	}
}

// nil ports（交换机没声明任何端口）：两个计数都必须是 0 且**出现在响应里**（不省略、
// 不发 null），否则调用方分不清「0 个」与「没这个字段」。
func TestGetVSwitchesPortCountsZeroWhenNoPorts(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	cfg := withSuperUser(model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-empty", Type: "l2"}},
	})
	status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		cfg, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交空端口交换机: %d %s", status, data)
	}

	view := vswitchFromList(t, ts, token, "vs-empty")
	if got := countField(t, view, "ports_declared_count"); got != 0 {
		t.Fatalf("未声明端口时 ports_declared_count 应为 0，得到 %d", got)
	}
	if got := countField(t, view, "ports_total_count"); got != 0 {
		t.Fatalf("未声明端口且无派生/运行态时 ports_total_count 应为 0，得到 %d", got)
	}
}
