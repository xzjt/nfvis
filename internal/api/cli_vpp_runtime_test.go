package api

// 决策 #200：`show vpp runtime [thread <id>]` 的渲染与边界。
//
// 这条命令此前是「%% 未接入」占位（附录 A #34），也是 cli-fulltest 里唯一一条 ✗；
// 现在改为真数据（线程级运行态），因此这里守住三件事：
//   ① 有数据时**不得**出现 `%` 前缀（否则套件仍判失败、操作者以为没接入）；
//   ② 不可用时给 `%%` + 原因（不静默省略，与 buffer 同口径）；
//   ③ `thread <id>` 过滤与非法用法都有明确答复。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/state"
)

// fakeRuntimeStats 只关心 RuntimeStats 的运行态假实现（线程名走 Threads）。
type fakeRuntimeStats struct {
	rs     state.RuntimeStats
	ok     bool
	reason string
	rows   []state.Thread
}

func (f *fakeRuntimeStats) Threads(context.Context) ([]state.Thread, error) { return f.rows, nil }
func (f *fakeRuntimeStats) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return state.InterfaceCounters{}, false
}
func (f *fakeRuntimeStats) Buffers(context.Context) (state.Buffers, bool) {
	return state.Buffers{}, false
}
func (f *fakeRuntimeStats) Memory(context.Context) (state.Memory, bool) {
	return state.Memory{}, false
}
func (f *fakeRuntimeStats) RuntimeStats(context.Context) (state.RuntimeStats, bool) {
	rs := f.rs
	if f.reason != "" {
		rs.Reason = f.reason
	}
	return rs, f.ok
}

func withRuntimeStats(t *testing.T, f *fakeRuntimeStats) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	x.state = state.New(f)
	return x
}

func TestCLIShowVppRuntimeRendersThreadRows(t *testing.T) {
	x := withRuntimeStats(t, &fakeRuntimeStats{
		ok: true,
		rows: []state.Thread{
			{ID: 0, Name: "vpp_main", Core: 5},
			{ID: 1, Name: "vpp_wk_0", Type: "workers", Core: 4},
		},
		rs: state.RuntimeStats{
			Source: state.StatsSourceTool, VectorRate: 12.5, WorkerThreads: 1, UptimeSeconds: 1234,
			Threads: []state.RuntimeThread{
				{ID: 0, VectorRate: 0, LoopsRate: 444740},
				{ID: 1, VectorRate: 12.5, LoopsRate: 7792483},
			},
		},
	})
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp runtime").Output
	if strings.Contains(out, "%") {
		t.Fatalf("有数据时不得出现 %% 前缀（套件按行首 %% 判失败）：%q", out)
	}
	for _, want := range []string{"source: vpp_get_stats", "worker threads: 1", "uptime: 1234s",
		"vpp_main", "vpp_wk_0", "444740", "7792483"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	// 按节点明细无结构化来源：必须如实说明，免得操作者以为「运行态没接入」
	if !strings.Contains(out, "按节点明细") {
		t.Fatalf("应说明按节点明细不可得：\n%s", out)
	}
}

func TestCLIShowVppRuntimeThreadFilter(t *testing.T) {
	mk := func() *cliExecutor {
		return withRuntimeStats(t, &fakeRuntimeStats{
			ok:   true,
			rows: []state.Thread{{ID: 0, Name: "vpp_main", Core: 5}, {ID: 1, Name: "vpp_wk_0", Core: 4}},
			rs: state.RuntimeStats{Source: state.StatsSourceTool, UptimeSeconds: 10,
				Threads: []state.RuntimeThread{{ID: 0, LoopsRate: 100}, {ID: 1, LoopsRate: 200}}},
		})
	}
	out := mk().Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp runtime thread 1").Output
	if !strings.Contains(out, "vpp_wk_0") || strings.Contains(out, "vpp_main") {
		t.Fatalf("thread 1 应只列该线程：\n%s", out)
	}
	// 不存在的线程：点名可用线程号，不静默给空表
	out = mk().Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp runtime thread 9").Output
	if !strings.HasPrefix(out, "%") || strings.HasPrefix(out, "%%") || !strings.Contains(out, "可用线程") {
		t.Fatalf("不存在的线程应报错并点名可用线程：%q", out)
	}
	// 非法用法 / 非法线程号
	if out := mk().Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp runtime thread").Output; !strings.Contains(out, "用法") {
		t.Fatalf("缺线程号应给用法：%q", out)
	}
	if out := mk().Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp runtime thread xx").Output; !strings.Contains(out, "不合法") {
		t.Fatalf("非法线程号应报错：%q", out)
	}
}

func TestCLIShowVppRuntimeUnavailable(t *testing.T) {
	// 不可用 → %% + 原因（不静默省略）
	x := withRuntimeStats(t, &fakeRuntimeStats{ok: false, reason: "stats segment 仅在 Linux 可用"})
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp runtime").Output
	if !strings.HasPrefix(out, "%") || !strings.Contains(out, "stats segment 仅在 Linux 可用") {
		t.Fatalf("不可用应报 %%%% 与原因：%q", out)
	}
	// 无原因时给缺省说明而不是空串
	x2 := withRuntimeStats(t, &fakeRuntimeStats{ok: false})
	if out := x2.Execute("admin", aaa.ClassSuperUser, "ssh", "show vpp runtime").Output; !strings.Contains(out, "不可用") || strings.HasSuffix(strings.TrimSpace(out), "：") {
		t.Fatalf("缺省原因不得为空：%q", out)
	}
}
