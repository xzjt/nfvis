package cli

// setup 向导单测（决策 #107）：计划推导是纯函数（默认/边界/与服务端同口径的校验），
// 交互路径用 fake backend 脚本化事实与执行，捕获语句序列。

import (
	"io"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/pkg/cliclient"
)

// setupFake 记录执行过的语句，事实（metrics 文本 / committed JSON）可脚本化。
type setupFake struct {
	lines     []string
	metrics   string
	committed string
	failOn    string // 语句含该子串时返回 %% 错误（失败即停路径）
	cancelled bool
}

func (f *setupFake) Execute(line, source string) (cliclient.Result, error) {
	if strings.Contains(line, "%%") {
		return cliclient.Result{}, nil
	}
	f.lines = append(f.lines, line)
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		return cliclient.Result{Output: "%% 测试注入的失败\n", Mode: "config", Prompt: "[edit] nfvis# "}, nil
	}
	return cliclient.Result{Output: "[ok] " + line + "\n", Mode: "oper", Prompt: "nfvis> "}, nil
}

func (f *setupFake) DynamicCandidates(kind string) ([]string, error) { return nil, nil }
func (f *setupFake) Logout() error                                   { return nil }
func (f *setupFake) DialConsole(wsPath string) (io.ReadWriteCloser, error) {
	return nil, io.EOF
}
func (f *setupFake) MetricsText() (string, error) { return f.metrics, nil }

// committedJSON 6 核机器、尚无资源池的 committed 现状（向导解析它展示现状）。
const committedEmpty = `{
  "resource_pools": {"hugepages": [], "cpu": {}}
}`

func TestDeriveSetupPlanDefaults(t *testing.T) {
	f := SetupFacts{OnlineCPUs: 6, MemTotalGB: 7, Pools: map[string]int{}}
	p, err := deriveSetupPlan(f, setupAnswers{})
	if err != nil {
		t.Fatalf("默认推导: %v", err)
	}
	if p.IsolatedCores != "2-5" {
		t.Fatalf("隔离核默认应为 2-5，实际 %q", p.IsolatedCores)
	}
	if p.VPPMain != 5 || p.VPPWorker != 4 {
		t.Fatalf("VPP 线程默认应为 5/4，实际 %d/%d", p.VPPMain, p.VPPWorker)
	}
	if p.HP1G != 1 { // 7/4=1
		t.Fatalf("1G 页默认应为 1，实际 %d", p.HP1G)
	}
	if p.HP2M != 768 || p.HugepagePref != "2M" {
		t.Fatalf("2M 池/preference 默认应为 768/2M，实际 %d/%s", p.HP2M, p.HugepagePref)
	}
	// 语句清单：configure 开头、apply 收尾，顺序固定
	if len(p.Statements) < 8 || p.Statements[0] != "configure" ||
		p.Statements[len(p.Statements)-1] != "request system kernel apply" {
		t.Fatalf("语句清单异常: %v", p.Statements)
	}
}

func TestDeriveSetupPlanSmallMachine(t *testing.T) {
	// 3 核：隔离 1 个（核 2），宿主保留 2 个；VPP 主线程用核 2、无工作线程
	f := SetupFacts{OnlineCPUs: 3, Pools: map[string]int{}}
	p, err := deriveSetupPlan(f, setupAnswers{})
	if err != nil {
		t.Fatalf("3 核推导: %v", err)
	}
	if p.IsolatedCores != "2" || p.VPPMain != 2 || p.VPPWorker != 0 {
		t.Fatalf("3 核计划异常: %+v", p)
	}
	// 2 核：给不出默认（宿主至少 2 核），要求显式输入
	if _, err := deriveSetupPlan(SetupFacts{OnlineCPUs: 2, Pools: map[string]int{}}, setupAnswers{}); err == nil {
		t.Fatal("2 核不应给默认建议")
	}
}

func TestDeriveSetupPlanValidations(t *testing.T) {
	f := SetupFacts{OnlineCPUs: 6, Pools: map[string]int{}}
	// 隔离核把宿主挤到只剩 1 个 → 与服务端护栏同口径拒绝
	if _, err := deriveSetupPlan(f, setupAnswers{Isolated: "1-5"}); err == nil || !strings.Contains(err.Error(), "至少保留 2") {
		t.Fatalf("只留 1 个宿主核应被拒: %v", err)
	}
	// VPP 主线程不在隔离核内 → 提前给可读原因（commit 校验同样会拒）
	main := 1
	if _, err := deriveSetupPlan(f, setupAnswers{Isolated: "2-5", VPPMain: &main}); err == nil ||
		!strings.Contains(err.Error(), "不在隔离核") {
		t.Fatalf("VPP 线程不在隔离核内应被拒: %v", err)
	}
	// 越界核
	if _, err := deriveSetupPlan(f, setupAnswers{Isolated: "2-9"}); err == nil ||
		!strings.Contains(err.Error(), "超出在线核") {
		t.Fatalf("越界核应被拒: %v", err)
	}
	// 2M 池为 0 → preference 落到 1G（内存事实在，1G 默认 1 页）
	zero := 0
	fMem := SetupFacts{OnlineCPUs: 6, MemTotalGB: 7, Pools: map[string]int{}}
	p, err := deriveSetupPlan(fMem, setupAnswers{HP2M: &zero})
	if err != nil {
		t.Fatalf("仅 1G 池推导: %v", err)
	}
	if p.HugepagePref != "1G" {
		t.Fatalf("仅 1G 池时 preference 应为 1G，实际 %q", p.HugepagePref)
	}
}

func TestParseMetricsValue(t *testing.T) {
	txt := "# HELP nfvis_system_cpu_online_count 在线 CPU 核数\n# TYPE nfvis_system_cpu_online_count gauge\nnfvis_system_cpu_online_count 6\nnfvis_system_memory_total_bytes 8.589934592e+09\n"
	if v, ok := parseMetricsValue(txt, "nfvis_system_cpu_online_count"); !ok || v != 6 {
		t.Fatalf("cpu count 解析: %v %v", v, ok)
	}
	if v, ok := parseMetricsValue(txt, "nfvis_system_memory_total_bytes"); !ok || int(v/(1<<30)) != 8 {
		t.Fatalf("mem total 解析: %v %v", v, ok)
	}
	if _, ok := parseMetricsValue(txt, "nfvis_system_missing"); ok {
		t.Fatal("缺失指标不应命中")
	}
}

func TestRunWizardRefusesNonTTY(t *testing.T) {
	f := &setupFake{}
	sess := New(f, "ssh")
	var out strings.Builder
	if err := RunWizard(sess, false, strings.NewReader(""), &out); err != nil {
		t.Fatalf("非 TTY 应优雅返回: %v", err)
	}
	if !strings.Contains(out.String(), "需要在终端（TTY）中运行") || len(f.lines) != 0 {
		t.Fatalf("非 TTY 应给指引且不执行任何语句: out=%q lines=%v", out.String(), f.lines)
	}
}

func TestRunWizardFullFlow(t *testing.T) {
	f := &setupFake{
		metrics:   "nfvis_system_cpu_online_count 6\nnfvis_system_memory_total_bytes 7516192768\n",
		committed: committedEmpty,
	}
	sess := New(f, "ssh")
	// 问答输入：隔离核默认(回车)、VPP 主/工作默认(回车×2)、1G 默认(回车)、2M 默认(回车)、
	// 低延迟 false(回车)、确认默认 yes(回车)
	var out strings.Builder
	if err := RunWizard(sess, true, strings.NewReader("\n\n\n\n\n\n\n"), &out); err != nil {
		t.Fatalf("全流程: %v", err)
	}
	joined := strings.Join(f.lines, "\n")
	for _, want := range []string{
		"configure",
		"set resource-pools hugepages page-size 1G count 1",
		"set resource-pools hugepages page-size 2M count 768",
		"set resource-pools cpu isolated-cores 2-5",
		"set vpp cpu main-core 5",
		"set vpp cpu corelist-workers 4",
		"set vpp memory hugepage-preference 2M",
		"top", "commit", "request system kernel apply",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("执行序列缺少 %q:\n%s", want, joined)
		}
	}
	if strings.Contains(out.String(), "低延迟参数组已启用") {
		t.Fatal("默认不应启用低延迟")
	}
}

func TestRunWizardCancelAndFailure(t *testing.T) {
	// 第一个问答输入 q → 中止且不执行任何语句
	f := &setupFake{metrics: "nfvis_system_cpu_online_count 6\n", committed: committedEmpty}
	sess := New(f, "ssh")
	var out strings.Builder
	if err := RunWizard(sess, true, strings.NewReader("q\n"), &out); err != nil {
		t.Fatalf("中止路径: %v", err)
	}
	for _, ln := range f.lines { // 只读的事实拉取（display json）允许；变更语句不允许
		if strings.HasPrefix(ln, "configure") || strings.HasPrefix(ln, "set") {
			t.Fatalf("中止不应执行变更语句: %v", f.lines)
		}
	}
	if !strings.Contains(out.String(), "已中止") {
		t.Fatalf("中止应提示: %q", out.String())
	}
	// commit 失败 → 即停，candidate 保留提示
	f2 := &setupFake{metrics: "nfvis_system_cpu_online_count 6\n", committed: committedEmpty, failOn: "commit"}
	sess2 := New(f2, "ssh")
	out2 := &strings.Builder{}
	err := RunWizard(sess2, true, strings.NewReader("\n\n\n\n\n\n\n"), out2)
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("commit 失败应上抛: %v", err)
	}
	if !strings.Contains(out2.String(), "candidate 已保留") {
		t.Fatalf("失败时应提示 candidate 保留: %s", out2.String())
	}
}
