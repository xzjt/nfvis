package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// 决策 #356：历史时序存储配置面回归。
//
// 三条硬口径：
//  1. set 后落库到 system.metrics.history.interval_seconds / retention_days；
//  2. `show configuration | display set` 能反推这些语句且回放自校验通过（决策 #155）；
//  3. **删空之后不留空壳**——否则 display set 反推为空语句、回放对不上而报内部错误
//     （round84 R84-26 同族：空 `nat` 对象就是这么露出来的）。
func TestMetricsHistoryConfigRoundTripAndPrune(t *testing.T) {
	x, eng := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system metrics history interval 30",
		"set system metrics history retention-days 14",
		"commit",
	)

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.System == nil || cfg.System.Metrics == nil || cfg.System.Metrics.History == nil {
		t.Fatalf("配置应落库 system.metrics.history: %+v", cfg.System)
	}
	if got := cfg.MetricsHistoryIntervalSeconds(); got != 30 {
		t.Fatalf("采样间隔应为 30，得 %d", got)
	}
	if got := cfg.MetricsHistoryRetentionDays(); got != 14 {
		t.Fatalf("保留天数应为 14，得 %d", got)
	}

	out := execOK(t, x, "show configuration | display set")
	if !strings.Contains(out, "set system metrics history interval 30") {
		t.Fatalf("display set 应反推采样间隔语句:\n%s", out)
	}
	if !strings.Contains(out, "set system metrics history retention-days 14") {
		t.Fatalf("display set 应反推保留天数语句:\n%s", out)
	}

	// 逐叶删除：配置里不得留下空壳（否则回放自校验会炸）。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"delete system metrics history interval",
		"delete system metrics history retention-days",
		"commit",
	)
	cfg, err = eng.Committed()
	if err != nil {
		t.Fatalf("Committed(删除后): %v", err)
	}
	if cfg.System != nil && cfg.System.Metrics != nil {
		t.Fatalf("删空后不应留下 metrics 空壳（display set 回放会失败）: %+v", cfg.System.Metrics)
	}
	execOK(t, x, "show configuration | display set")
}

// 范围校验：越界取值在**提交期**被拒并说明范围。
// 与既有 `set system syslog local retention-days` 同口径：set 只落 candidate，
// 范围由 model.Validate 在 commit 时判（校验失败则 candidate 保留）。
// 0 表示「未设置」= 用默认值（本仓库既有约定，见 syslog/health 的 `0 = 未设置`）。
func TestMetricsHistoryConfigRangeRejected(t *testing.T) {
	x, _ := newCLIKit(t)
	for _, tc := range []struct{ line, want string }{
		{"set system metrics history interval 5", "采样间隔"},
		{"set system metrics history interval 4000", "采样间隔"},
		{"set system metrics history retention-days 400", "保留天数"},
	} {
		run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure", tc.line)
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "commit").Output
		if !strings.Contains(out, "校验失败") || !strings.Contains(out, tc.want) {
			t.Fatalf("越界取值应在提交期被拒并点名字段: %q => %q", tc.line, out)
		}
		execOK(t, x, "discard") // 清掉 candidate，避免污染下一条用例
	}
}

// `set … retention-days 0` / `interval 0`＝未设置（用默认值），提交成功且不留空壳。
func TestMetricsHistoryConfigZeroMeansDefault(t *testing.T) {
	x, eng := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system metrics history interval 0",
		"set system metrics history retention-days 0",
		"commit",
	)
	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if got := cfg.MetricsHistoryIntervalSeconds(); got != 60 {
		t.Fatalf("0 应为未设置、生效值回落默认 60，得 %d", got)
	}
	if got := cfg.MetricsHistoryRetentionDays(); got != 7 {
		t.Fatalf("0 应为未设置、生效值回落默认 7，得 %d", got)
	}
	// 全 0 等同空壳：反推无语句、回放自校验必须仍成立。
	if cfg.System != nil && cfg.System.Metrics != nil {
		t.Fatalf("全 0 的 metrics 不应留下空壳: %+v", cfg.System.Metrics)
	}
	execOK(t, x, "show configuration | display set")
}
