package api

// D-1（决策 #68）：buffer 池统计的来源标注与"不可用+原因"。
// 守护点：来源必须透出到 /vpp/status 与 /metrics，且不可用不得静默省略。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/state"
)

// fakeBufRuntime 只关心 Buffers 的运行态假实现。
type fakeBufRuntime struct{ bufs state.Buffers }

func (f *fakeBufRuntime) Threads(context.Context) ([]state.Thread, error) { return nil, nil }
func (f *fakeBufRuntime) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return state.InterfaceCounters{}, false
}
func (f *fakeBufRuntime) Buffers(context.Context) (state.Buffers, bool) {
	return f.bufs, len(f.bufs.Pools) > 0
}
func (f *fakeBufRuntime) Memory(context.Context) (state.Memory, bool) { return state.Memory{}, false }

func getVppStatus(t *testing.T, st *state.State) VppStatus {
	t.Helper()
	ts := newTestServerOpts(t, Options{VPP: &fakeVppController{status: VppStatus{Version: "26.06-release", Connected: true}}, State: st})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vpp/status", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status: %d %s", status, data)
	}
	var got VppStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	return got
}

// 回退源成功时须标注 source=vpp_get_stats（且不出现 unavailable）。
func TestVppStatusBufferSourceAnnotation(t *testing.T) {
	st := state.New(&fakeBufRuntime{bufs: state.Buffers{
		Pools:  []state.BufferPool{{Name: "default-numa-0", Available: 430184}},
		Source: state.StatsSourceTool,
	}})
	got := getVppStatus(t, st)
	if got.BuffersSource != state.StatsSourceTool {
		t.Fatalf("buffers_source = %q，want %q", got.BuffersSource, state.StatsSourceTool)
	}
	if got.BuffersUnavailable != "" {
		t.Fatalf("有数据时不应给 unavailable: %q", got.BuffersUnavailable)
	}
	if len(got.Buffers) != 1 || got.Buffers[0].Available != 430184 {
		t.Fatalf("buffer 数据错误: %+v", got.Buffers)
	}
}

// 不可用时必须给出原因（不静默省略）。
func TestVppStatusBufferUnavailableReason(t *testing.T) {
	st := state.New(&fakeBufRuntime{bufs: state.Buffers{Reason: "stats client 解码失败"}})
	got := getVppStatus(t, st)
	if got.BuffersUnavailable == "" {
		t.Fatal("不可用时应给出 buffers_unavailable 原因")
	}
	if got.BuffersSource != "" {
		t.Fatalf("不可用时不应有 source: %q", got.BuffersSource)
	}
}

// /metrics 必须带 source 标签与可用性序列。
func TestMetricsBufferSourceLabel(t *testing.T) {
	st := state.New(&fakeBufRuntime{bufs: state.Buffers{
		Pools:  []state.BufferPool{{Name: "default-numa-0", Used: 3, Available: 430184}},
		Source: state.StatsSourceTool,
	}})
	ts := newTestServerOpts(t, Options{State: st})
	body := getMetricsBody(t, ts.URL)
	for _, want := range []string{
		`nfvis_vpp_buffer_stats_available{source="vpp_get_stats"} 1`,
		`nfvis_vpp_buffer_pool_available{pool="default-numa-0",source="vpp_get_stats"} 430184`,
		`nfvis_vpp_buffer_pool_used{pool="default-numa-0",source="vpp_get_stats"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q\n%s", want, body)
		}
	}
}

// 不可用时 /metrics 仍须有序列（含原因），不得静默消失。
func TestMetricsBufferUnavailableSeries(t *testing.T) {
	st := state.New(&fakeBufRuntime{bufs: state.Buffers{Reason: "vpp_get_stats: Couldn't connect to vpp"}})
	ts := newTestServerOpts(t, Options{State: st})
	body := getMetricsBody(t, ts.URL)
	if !strings.Contains(body, "nfvis_vpp_buffer_stats_available{") || !strings.Contains(body, "} 0") {
		t.Fatalf("不可用时应输出 available=0 序列\n%s", body)
	}
	if !strings.Contains(body, `source="unavailable"`) {
		t.Fatalf("不可用时应标注 source=unavailable\n%s", body)
	}
	if !strings.Contains(body, "Couldn't connect to vpp") {
		t.Fatalf("应带原因标签\n%s", body)
	}
}

func getMetricsBody(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + APIPrefix + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// CLI `show vpp buffers` 与概览 `show vpp` 必须打印统计来源（决策 #68）。
func TestCLIShowVppBuffersSource(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setRuntime(nil, state.New(&fakeBufRuntime{bufs: state.Buffers{
		Pools:  []state.BufferPool{{Name: "default-numa-0", Available: 430184}},
		Source: state.StatsSourceTool,
	}}))

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp buffers").Output
	if !strings.Contains(out, "source: "+state.StatsSourceTool) {
		t.Fatalf("应打印来源: %q", out)
	}
	if !strings.Contains(out, "430184") {
		t.Fatalf("应打印数值: %q", out)
	}
	if ov := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp").Output; !strings.Contains(ov, "source="+state.StatsSourceTool) {
		t.Fatalf("概览应标注来源: %q", ov)
	}

	// 不可用时必须给原因，不得静默省略
	x.setRuntime(nil, state.New(&fakeBufRuntime{bufs: state.Buffers{Reason: "stats segment 不可读"}}))
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp buffers").Output
	if !strings.Contains(out, "stats segment 不可读") {
		t.Fatalf("不可用应给原因: %q", out)
	}
}
