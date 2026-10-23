package cliclient

// 修 round44：升级复核读到 `pending` 时必须**继续等**，不能就此判失败。
//
// 真机实证：服务端终判要等「同一状态行连续 6 次一致」（约 10s+），而 CLI 重连通常在 16s 内
// 发生——那一刻读到的是 pending（升级进行中）。把它当失败会让用户对一次**正在成功**的升级
// 执行回退。

import (
	"testing"
)

// pending 不是结论：解析结果必须让调用方看得出「还没定」。
func TestParseSoftwareResultPendingIsNotVerdict(t *testing.T) {
	ok, ver, status := parseSoftwareResult("结果: 未定（升级进行中或判定未完成）\n状态: pending\n")
	if ok {
		t.Fatal("pending 不得被判为成功")
	}
	if status != statusPending {
		t.Fatalf("应解析出 pending，实得 %q", status)
	}
	if ver != "" {
		t.Fatalf("pending 时版本应空缺，实得 %q", ver)
	}
}

// 成功/失败两种终态不受影响（回归）。
func TestParseSoftwareResultTerminalStates(t *testing.T) {
	ok, ver, status := parseSoftwareResult("结果: 成功\n版本: 3.0.27\n状态: install ok installed\n")
	if !ok || ver != "3.0.27" || status != "install ok installed" {
		t.Fatalf("成功态解析不符: ok=%v ver=%q status=%q", ok, ver, status)
	}
	ok2, _, status2 := parseSoftwareResult("结果: 失败\n状态: install ok half-configured\n")
	if ok2 || status2 != "install ok half-configured" {
		t.Fatalf("失败态解析不符: ok=%v status=%q", ok2, status2)
	}
}

// 判不出「结果」行时不当作成功（宁可报未知）。
func TestParseSoftwareResultMissingVerdictLine(t *testing.T) {
	ok, _, _ := parseSoftwareResult("（无升级/回退结论记录：…）\n")
	if ok {
		t.Fatal("没有结果行时不得判成功")
	}
}
