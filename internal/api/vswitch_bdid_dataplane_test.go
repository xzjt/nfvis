package api

// round40 R40-O1：`bd_id` 只在 **VPP** 数据面有意义。
//
// 由来（发布件走查）：内核数据面下 `show virtual-switches` 列表显示 `BD-ID 0`、Web 列表/详情
// 「数据面 BD 0」——而内核 bridge 根本没有 BD-ID 概念（`netkernel.BridgeDomainState.ID` 的注释
// 早已写明「恒为 0、内核侧不使用」）。数字 `0` 会被读成真实的数据面标识。
//
// 口径：内核数据面下 CLI 列表该列渲染「—」、CLI `statistics` 与 REST `statistics` **省略
// `bd_id` 键**（不编造 0）；VPP 侧三处逐字不变（真实 BD-ID）。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 内核数据面：列表 BD-ID 列「—」，structured 无 bd_id；statistics structured 无 bd_id。
func TestVSwitchBDIDHiddenOnKernelDataplane(t *testing.T) {
	x := l3Fixture(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 0, Name: "vs-lan", Learn: true, Flood: true, Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}}},
	}})

	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches").Output
	if strings.Contains(out, "0          vs-lan") {
		t.Fatalf("内核数据面下不得把 bd_id 0 当真实标识显示：\n%s", out)
	}
	if !strings.Contains(out, "—          vs-lan") {
		t.Fatalf("内核数据面下 BD-ID 列应为「—」：\n%s", out)
	}
	// 结构化输出同样不带 bd_id（不编造）。
	if structuredHasBDID(x.structured) {
		t.Fatalf("内核数据面下 structured 不应含 bd_id：%+v", x.structured)
	}

	// statistics：structured 无 bd_id，文本里不出现 `bd-id 0`。
	out = x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-lan statistics").Output
	if strings.Contains(out, "bd-id 0") {
		t.Fatalf("内核数据面下 statistics 不得显示 bd-id 0：\n%s", out)
	}
	if structuredHasBDID(x.structured) {
		t.Fatalf("内核数据面下 statistics structured 不应含 bd_id：%+v", x.structured)
	}
}

// VPP 数据面：三处逐字不变（真实 BD-ID 照常出现）。
func TestVSwitchBDIDShownOnVPPDataplane(t *testing.T) {
	x := l3Fixture(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}})
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 42, Name: "vs-lan", Learn: true, Flood: true, Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}}},
	}})

	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches").Output
	if !strings.Contains(out, "42         vs-lan") {
		t.Fatalf("VPP 数据面下 BD-ID 列应照旧显示真实值：\n%s", out)
	}
	if !structuredHasBDID(x.structured) {
		t.Fatalf("VPP 数据面下 structured 应含 bd_id：%+v", x.structured)
	}
}

// REST statistics：内核数据面省略 bd_id 键；VPP 照旧给出。
func TestRestVSwitchStatisticsBDIDPerDataplane(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		wantBDID bool
	}{
		{"kernel", model.DataPlaneKernel, false},
		{"vpp", model.DataPlaneVPP, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServerOpts(t, Options{
				VPP: &fakeVppController{status: VppStatus{Connected: true, Mode: tc.mode}},
				VppState: fakeVppState{bds: []BridgeDomainState{
					{ID: 42, Name: "vs-s", Learn: true, Flood: true,
						Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}}},
				}},
			})
			token := loginAdmin(t, ts)
			status, _, raw := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-switches", token,
				map[string]any{"name": "vs-s", "type": "l2"},
				map[string]string{"X-NFVIS-Auto-Commit": "true"})
			if status != http.StatusCreated && status != http.StatusOK {
				t.Fatalf("写入交换机: %d %s", status, raw)
			}
			status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-switches/vs-s", token, nil, nil)
			if status != http.StatusOK {
				t.Fatalf("读交换机详情: %d %s", status, data)
			}
			var body map[string]any
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatalf("解析详情: %v（%s）", err, data)
			}
			stats, ok := body["statistics"].(map[string]any)
			if !ok {
				t.Fatalf("详情应带 statistics：%s", body)
			}
			_, has := stats["bd_id"]
			if has != tc.wantBDID {
				t.Fatalf("bd_id 存在性应为 %v（实际 %v）：%v", tc.wantBDID, has, stats)
			}
			if tc.wantBDID {
				if got, _ := stats["bd_id"].(float64); int(got) != 42 {
					t.Fatalf("VPP 下 bd_id 应为真实值 42，实际 %v", stats["bd_id"])
				}
			}
		})
	}
}

// structuredHasBDID 在 structured 的任意层级里找 `bd_id`（列表：virtual_switches[].bd_id；
// statistics：顶层 bd_id）。
func structuredHasBDID(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["bd_id"]; ok {
			return true
		}
		for _, x := range t {
			if structuredHasBDID(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if structuredHasBDID(x) {
				return true
			}
		}
	}
	return false
}
