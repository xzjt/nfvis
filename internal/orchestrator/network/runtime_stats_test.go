package network

// 决策 #200：`show vpp runtime` 的线程级运行态解析。
//
// 用的 dump 文本是**真机现场原文**（VPP 26.06-release，nfvis-vm）：
//   - `/sys/loops_per_worker` 是组合计数，索引 0 = 主线程、1 = 工作线程
//   - 同一时刻 `vppctl show runtime` 的每线程汇总行是 `loops/sec 296130.91`（主）与
//     `6845707.23`（工作线程）——与本解析出的 444740 / 7792483 量级吻合（各自采样时刻不同）
// 这条对应关系就是「值在第 4 段」的判据；写死成用例，防解析器被改错。

import (
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/state"
)

// realSysDump 真机 `vpp_get_stats dump machine "/sys/*"` 输出（节选，含两类计数与错误计数）。
const realSysDump = `1:99.00:/sys/heartbeat
1:0.00:/sys/last_stats_clear
1:1790685040.00:/sys/boottime
9:0.00:/sys/vector_rate
2:0:0:0:/sys/vector_rate_per_worker
2:1:0:0:/sys/vector_rate_per_worker
2:0:0:444740:/sys/loops_per_worker
2:1:0:7792483:/sys/loops_per_worker
9:1.00:/sys/num_worker_threads
2:0:0:0:/err/af_xdp-input/syscall required
2:0:1:0:/err/af_xdp-input/syscall required
1:981.00:/sys/last_update
9:0.00:/sys/input_rate
`

func TestParseStatsPerThreadReadsValueColumn(t *testing.T) {
	got := ParseStatsPerThread(realSysDump)
	if v := got["/sys/loops_per_worker"]; v[0] != 444740 || v[1] != 7792483 {
		t.Fatalf("每线程 loops 解析错误（值在第 4 段）：%+v", v)
	}
	if v := got["/sys/vector_rate_per_worker"]; len(v) != 2 || v[0] != 0 || v[1] != 0 {
		t.Fatalf("每线程向量率解析错误：%+v", v)
	}
	// 简单计数（三段）不得被误当组合计数
	if _, ok := got["/sys/vector_rate"]; ok {
		t.Fatalf("简单计数不应进组合表：%+v", got["/sys/vector_rate"])
	}
	if _, ok := got["/sys/heartbeat"]; ok {
		t.Fatalf("简单计数不应进组合表：%+v", got["/sys/heartbeat"])
	}
}

func TestRuntimeStatsFromDump(t *testing.T) {
	now := time.Unix(1790685040+1234, 0) // 与 boottime 相差 1234s
	got, ok := RuntimeStatsFromDump(realSysDump, now)
	if !ok {
		t.Fatal("应解析成功")
	}
	if got.Source != state.StatsSourceTool {
		t.Errorf("source = %q，期望 %q", got.Source, state.StatsSourceTool)
	}
	if len(got.Threads) != 2 {
		t.Fatalf("线程数 = %d，期望 2（主 + 1 工作线程）：%+v", len(got.Threads), got.Threads)
	}
	if got.Threads[0].ID != 0 || got.Threads[0].LoopsRate != 444740 {
		t.Errorf("线程 0 解析错误：%+v", got.Threads[0])
	}
	if got.Threads[1].ID != 1 || got.Threads[1].LoopsRate != 7792483 {
		t.Errorf("线程 1 解析错误：%+v", got.Threads[1])
	}
	if got.WorkerThreads != 1 {
		t.Errorf("工作线程数 = %v，期望 1", got.WorkerThreads)
	}
	if got.UptimeSeconds != 1234 {
		t.Errorf("运行时长 = %v，期望 1234（now - /sys/boottime）", got.UptimeSeconds)
	}
	if got.VectorRate != 0 {
		t.Errorf("整机向量率 = %v，期望 0（该 dump 为空闲采样）", got.VectorRate)
	}
}

// 没有 /sys 运行态计数时宁缺勿错（ok=false，由调用方给原因，不编造空表）。
func TestRuntimeStatsFromDumpRejectsNoise(t *testing.T) {
	if _, ok := RuntimeStatsFromDump("1:1.00:/sys/heartbeat\n", time.Now()); ok {
		t.Fatal("只有心跳时应判为不可用")
	}
	if _, ok := RuntimeStatsFromDump("", time.Now()); ok {
		t.Fatal("空输入应判为不可用")
	}
}

// boottime 缺失或异常（未来时刻）时不给运行时长，而不是给负数。
func TestRuntimeStatsFromDumpUptimeGuards(t *testing.T) {
	dump := "2:0:0:10:/sys/loops_per_worker\n1:1790685040.00:/sys/boottime\n"
	if got, ok := RuntimeStatsFromDump(dump, time.Unix(1790685000, 0)); !ok || got.UptimeSeconds != 0 {
		t.Fatalf("boottime 在未来时不应给运行时长：%+v ok=%v", got, ok)
	}
	if got, ok := RuntimeStatsFromDump("2:0:0:10:/sys/loops_per_worker\n", time.Now()); !ok || got.UptimeSeconds != 0 {
		t.Fatalf("无 boottime 时不应给运行时长：%+v ok=%v", got, ok)
	}
}
