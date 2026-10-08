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
	// warnOn 语句含该子串时返回**提示**（Warning=true，无 `%%`）——「值未变化」的空操作
	// （round86 R86-8）：重跑向导属预期，不算失败。
	warnOn string
	// singlePctOn 语句含该子串时返回**单个 %** 的错误输出（`% 无效命令` 一类）——
	// 决策 #113 前向导只认 "%%"，这类失败会被漏判成成功（假绿）。
	singlePctOn string
	cancelled   bool
}

func (f *setupFake) Execute(line, source string) (cliclient.Result, error) {
	if strings.Contains(line, "%%") {
		return cliclient.Result{}, nil
	}
	f.lines = append(f.lines, line)
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		return cliclient.Result{Output: "%% 测试注入的失败\n", Mode: "config", Prompt: "[edit] nfvis# "}, nil
	}
	if f.singlePctOn != "" && strings.Contains(line, f.singlePctOn) {
		return cliclient.Result{Output: "% 无效命令: " + line + "（输入 ? 查看可用命令）\n", Mode: "config", Prompt: "nfvis# "}, nil
	}
	if f.warnOn != "" && strings.Contains(line, f.warnOn) {
		return cliclient.Result{
			Output:  "警告: 语句未产生配置变更（值未变化或尚未映射到模型），已继续：" + line + "\n",
			Mode:    "config",
			Prompt:  "[edit] nfvis# ",
			Warning: true,
		}, nil
	}
	return cliclient.Result{Output: "[ok] " + line + "\n", Mode: "oper", Prompt: "nfvis> "}, nil
}

func (f *setupFake) DynamicCandidates(kind string) ([]string, error) { return nil, nil }
func (f *setupFake) Logout() error                                   { return nil }
func (f *setupFake) DialConsole(wsPath, what string) (io.ReadWriteCloser, error) {
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
	// 问答输入：数据面默认 vpp(回车)、隔离核默认(回车)、VPP 主/工作默认(回车×2)、
	// 1G 默认(回车)、2M 默认(回车)、低延迟 false(回车)、确认默认 yes(回车)
	var out strings.Builder
	if err := RunWizard(sess, true, strings.NewReader("\n\n\n\n\n\n\n\n"), &out); err != nil {
		t.Fatalf("全流程: %v", err)
	}
	joined := strings.Join(f.lines, "\n")
	for _, want := range []string{
		"configure",
		"set system dataplane vpp",
		"set resource-pools hugepages page-size 1G count 1",
		"set resource-pools hugepages page-size 2M count 768",
		"set resource-pools cpu isolated-cores 2-5",
		"set vpp cpu main-core 5",
		"set vpp cpu corelist-workers 4",
		"set vpp memory hugepage-preference 2M",
		"commit", "exit", "request system kernel apply",
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
	err := RunWizard(sess2, true, strings.NewReader("\n\n\n\n\n\n\n\n"), out2)
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("commit 失败应上抛: %v", err)
	}
	if !strings.Contains(out2.String(), "candidate 已保留") {
		t.Fatalf("失败时应提示 candidate 保留: %s", out2.String())
	}
}

// 单 % 错误必须判失败（决策 #113）：`% 无效命令` 此前被漏判，向导会打印「向导完成」返回成功。
func TestRunWizardDetectsSinglePercentError(t *testing.T) {
	f := &setupFake{
		metrics:     "nfvis_system_cpu_online_count 6\nnfvis_system_memory_total_bytes 7516192768\n",
		committed:   committedEmpty,
		singlePctOn: "request system kernel apply",
	}
	sess := New(f, "ssh")
	var out strings.Builder
	err := RunWizard(sess, true, strings.NewReader("\n\n\n\n\n\n\n\n"), &out)
	if err == nil {
		t.Fatalf("单 %% 错误应上抛，实际返回成功；输出：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "request system kernel apply") {
		t.Fatalf("错误应指向失败语句: %v", err)
	}
	if strings.Contains(out.String(), "向导完成") {
		t.Fatalf("失败不应打印「向导完成」:\n%s", out.String())
	}
}

// 决策 #353：1G 池「数据面固定占用 1 页」的向导口径——0 < 1G < 2 的计划预览必须
// 给出告警行；正常值（>=2 / 0=不配）不出现。
func TestSetupPlanWarnings(t *testing.T) {
	w := setupPlanWarnings(SetupPlan{HP1G: 1})
	if len(w) != 1 || !strings.Contains(w[0], "1G 池仅 1 页") || !strings.Contains(w[0], "无法再起 VNF") {
		t.Fatalf("1G=1 页应给告警行，实得 %v", w)
	}
	if !strings.Contains(w[0], "装 VNF 建议 ≥2") {
		t.Fatalf("告警行应给出建议值：%q", w[0])
	}
	for _, n := range []int{0, 2, 3, 8} {
		if got := setupPlanWarnings(SetupPlan{HP1G: n}); len(got) != 0 {
			t.Fatalf("HP1G=%d 不该有告警行：%v", n, got)
		}
	}
}

// 同口径的端到端路径：向导问答与计划预览都要出现 1G 固定占用提示/告警；声明 2 页时不出现。
func TestRunWizardWarnsSingleHP1G(t *testing.T) {
	newFake := func() *setupFake {
		return &setupFake{
			metrics:   "nfvis_system_cpu_online_count 6\nnfvis_system_memory_total_bytes 7516192768\n",
			committed: committedEmpty,
		}
	}
	// 默认 1G = 1（7GB 内存按 min(RAM/4,8)）：问句带 N−1 提示、计划预览给告警行。
	f := newFake()
	var out strings.Builder
	if err := RunWizard(New(f, "ssh"), true, strings.NewReader("\n\n\n\n\n\n\n\n"), &out); err != nil {
		t.Fatalf("全流程: %v", err)
	}
	if !strings.Contains(out.String(), "数据面固定占用其中 1 页 ⇒ 可起 VNF 数 ≈ N−1") {
		t.Fatalf("1G 数量问句应写明固定占用口径：\n%s", out.String())
	}
	if !strings.Contains(out.String(), "⚠ 1G 池仅 1 页：数据面占 1，无法再起 VNF（装 VNF 建议 ≥2）") {
		t.Fatalf("计划预览应给 1G 池告警行：\n%s", out.String())
	}
	// 显式声明 2 页：无告警行，问句仍在（口径统一）。
	f2 := newFake()
	var out2 strings.Builder
	if err := RunWizard(New(f2, "ssh"), true, strings.NewReader("\n\n\n\n2\n\n\n\n"), &out2); err != nil {
		t.Fatalf("2 页全流程: %v", err)
	}
	if strings.Contains(out2.String(), "⚠ 1G 池仅") {
		t.Fatalf("声明 2 页不该出现告警行：\n%s", out2.String())
	}
	if !strings.Contains(strings.Join(f2.lines, "\n"), "set resource-pools hugepages page-size 1G count 2") {
		t.Fatalf("应提交 1G=2 的语句：%v", f2.lines)
	}
}

// 语句清单的导航顺序（决策 #113）：commit 在配置模式下执行，exit 必须紧随其后
// （top 只回配置层级顶层、不离开配置模式；exit 若在 commit 前会因未提交变更被拒）。
func TestSetupPlanNavigationOrder(t *testing.T) {
	p, err := deriveSetupPlan(SetupFacts{OnlineCPUs: 6, MemTotalGB: 7, Pools: map[string]int{}}, setupAnswers{})
	if err != nil {
		t.Fatal(err)
	}
	var ci, ei, ai = -1, -1, -1
	for i, st := range p.Statements {
		switch st {
		case "commit":
			ci = i
		case "exit":
			ei = i
		case "request system kernel apply":
			ai = i
		case "top":
			t.Fatalf("计划不应含 top（它不离开配置模式）: %v", p.Statements)
		}
	}
	if ci < 0 || ei < 0 || ai < 0 || !(ci < ei && ei < ai) {
		t.Fatalf("应为 commit → exit → request system kernel apply，实际 %v", p.Statements)
	}
}

// round86 R86-8：向导重跑时「值未变化」的语句由服务端的**结构化标记**（Warning）判定——
// 属预期、不算失败，且不再重复打印服务端提示（只留 round83 定下的 [跳过] 一行）。
func TestRunWizardSkipsNoChangeByWarningMark(t *testing.T) {
	f := &setupFake{
		metrics:   "nfvis_system_cpu_online_count 6\nnfvis_system_memory_total_bytes 7516192768\n",
		committed: committedEmpty,
		warnOn:    "set vpp cpu main-core",
	}
	sess := New(f, "ssh")
	var out strings.Builder
	if err := RunWizard(sess, true, strings.NewReader("\n\n\n\n\n\n\n\n"), &out); err != nil {
		t.Fatalf("空操作（值未变化）不算失败：%v", err)
	}
	if !strings.Contains(out.String(), "[跳过] set vpp cpu main-core") {
		t.Fatalf("应给出跳过说明：\n%s", out.String())
	}
	if strings.Contains(out.String(), "向导在上述步骤失败") {
		t.Fatalf("空操作不得判失败：\n%s", out.String())
	}
	if !strings.Contains(out.String(), "向导完成") {
		t.Fatalf("空操作之后应继续走完向导：\n%s", out.String())
	}
	if strings.Contains(out.String(), "警告: 语句未产生配置变更") {
		t.Fatalf("服务端提示与 [跳过] 说明重复打印：\n%s", out.String())
	}
}

// TestOutputFailedJudgement 锁定失败判据（决策 #113 同源、决策 #320 起由脚本模式共用）：
// 行首单个或双个 % 与行首「校验失败」算失败；提示行与普通输出不算。
func TestOutputFailedJudgement(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"双 % 错误", "%% 配置不完整，缺少取值: hostname\n", true},
		{"单 % 错误（真实服务端形态）", "% 无效命令: show system no-such-subcommand-xyz（可用：uptime|cpu）\n", true},
		{"独行 %", "%\n", true},
		{"校验失败", "校验失败（candidate 保留在本会话内；会话/进程结束即释放，show configuration candidate 可查看）:\n  - x\n", true},
		{"多行里有一行是错误", "NFViS 1.0.0\n% 无效命令: show vpp bogus\n", true},
		{"空操作提示不算失败", "警告: 语句未产生配置变更（值未变化或尚未映射到模型），已继续：set x y\n", false},
		{"普通输出", "NFViS 1.0.0\nUbuntu 26.04\n", false},
		{"空输出", "", false},
		{"句中的百分号不是错误", "packet loss 50%\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OutputFailed(tc.out); got != tc.want {
				t.Fatalf("OutputFailed(%q) = %v，期望 %v", tc.out, got, tc.want)
			}
		})
	}
}

// 内核数据面路径（v3 决策 #404）：选 kernel 后跳过 VPP 线程问答、不产生任何 vpp 语句，
// 但隔离核与大页池照常规划（VM 仍需要它们）。
func TestRunWizardKernelDataPlane(t *testing.T) {
	f := &setupFake{
		metrics:   "nfvis_system_cpu_online_count 6\nnfvis_system_memory_total_bytes 7516192768\n",
		committed: committedEmpty,
	}
	sess := New(f, "ssh")
	// 问答输入：数据面 kernel、其余全部默认。
	var out strings.Builder
	if err := RunWizard(sess, true, strings.NewReader("kernel\n\n\n\n\n\n"), &out); err != nil {
		t.Fatalf("内核数据面全流程: %v", err)
	}
	joined := strings.Join(f.lines, "\n")
	if !strings.Contains(joined, "set system dataplane kernel") {
		t.Fatalf("应提交数据面语句：\n%s", joined)
	}
	for _, unwanted := range []string{"set vpp cpu", "set vpp memory"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("内核数据面不应产生 %q 语句：\n%s", unwanted, joined)
		}
	}
	if !strings.Contains(joined, "set resource-pools cpu isolated-cores 2-5") {
		t.Fatalf("隔离核仍应规划（VM 绑核用）：\n%s", joined)
	}
	if !strings.Contains(joined, "set resource-pools hugepages page-size 1G count 1") {
		t.Fatalf("大页池仍应规划（VM 内存用）：\n%s", joined)
	}
	// 预览里不该出现 VPP 线程问句
	if strings.Contains(out.String(), "VPP 主线程核") {
		t.Fatalf("内核数据面不应问 VPP 线程：\n%s", out.String())
	}
	// 收尾提示改为内核口径（不再提示 bind-dpdk / request vpp restart）
	if strings.Contains(out.String(), "bind-dpdk") {
		t.Fatalf("内核数据面不应提示绑定 DPDK：\n%s", out.String())
	}
	if !strings.Contains(out.String(), "内核数据面") {
		t.Fatalf("收尾应说明内核数据面口径：\n%s", out.String())
	}
}

// 非法数据面取值在向导阶段即被拒（与提交期校验同口径）。
func TestRunWizardRejectsBadDataPlane(t *testing.T) {
	f := &setupFake{metrics: "nfvis_system_cpu_online_count 6\n", committed: committedEmpty}
	var out strings.Builder
	err := RunWizard(New(f, "ssh"), true, strings.NewReader("dpdk\n\n\n\n\n\n\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "vpp|kernel") {
		t.Fatalf("非法数据面应被拒: %v", err)
	}
	for _, ln := range f.lines {
		if strings.HasPrefix(ln, "configure") || strings.HasPrefix(ln, "set ") {
			t.Fatalf("被拒时不应执行变更语句: %v", f.lines)
		}
	}
}

// R2-8：2/5（隔离核）的答案就是**核列表**——按提示语与默认值输入 `2-5` 必须能继续。
// 红-绿：修复前该问题里错位地放了一段「数据面取值」校验，输入核列表立即报
// 「数据面 "2-5" 不合法（vpp|kernel）」，只有空行能过；而输入 `vpp`/`kernel` 反被放行到
// parseCoreListText 才报错。核列表的解析与校验只应发生在第 3 问与 deriveSetupPlan。
func TestRunWizardAcceptsCoreListAtIsolatedQuestion(t *testing.T) {
	f := &setupFake{
		metrics:   "nfvis_system_cpu_online_count 6\nnfvis_system_memory_total_bytes 7516192768\n",
		committed: committedEmpty,
	}
	// 问答输入：数据面默认 vpp(回车)、隔离核显式 2-5、VPP 主/工作默认(回车×2)、
	// 1G 默认(回车)、2M 默认(回车)、低延迟默认(回车)、确认默认(回车)
	var out strings.Builder
	if err := RunWizard(New(f, "ssh"), true, strings.NewReader("\n2-5\n\n\n\n\n\n\n"), &out); err != nil {
		t.Fatalf("2/5 输入核列表应能继续，实际报错: %v", err)
	}
	joined := strings.Join(f.lines, "\n")
	if !strings.Contains(joined, "set resource-pools cpu isolated-cores 2-5") {
		t.Fatalf("应按 2/5 的核列表作答下发隔离核：\n%s", joined)
	}
	if !strings.Contains(joined, "set vpp cpu main-core 5") || !strings.Contains(joined, "set vpp cpu corelist-workers 4") {
		t.Fatalf("VPP 线程默认应取自隔离核答案（2-5 末两核 = 5/4）：\n%s", joined)
	}
	if strings.Contains(out.String(), "数据面 \"2-5\" 不合法") {
		t.Fatalf("核列表答案不得被当成数据面取值校验：\n%s", out.String())
	}
}

// R2-8 的反面：2/5 也不是「数据面取值」的入口——输 `kernel` 必须被判为**核列表**不合法
// （修复前它恰好能过 2/5，改由 parseCoreListText 报错），且不产生任何变更语句。
func TestRunWizardCoreListQuestionRejectsDataPlaneWord(t *testing.T) {
	f := &setupFake{metrics: "nfvis_system_cpu_online_count 6\n", committed: committedEmpty}
	var out strings.Builder
	err := RunWizard(New(f, "ssh"), true, strings.NewReader("\nkernel\n\n\n\n\n\n\n"), &out)
	if err == nil {
		t.Fatalf("2/5 输数据面词应报核列表不合法，实际成功；输出：\n%s", out.String())
	}
	if strings.Contains(err.Error(), "vpp|kernel") {
		t.Fatalf("2/5 的答案不参与数据面取值校验，不该报「数据面 … 不合法」: %v", err)
	}
	if !strings.Contains(err.Error(), "kernel") {
		t.Fatalf("报错应点名不合法输入: %v", err)
	}
	for _, ln := range f.lines {
		if strings.HasPrefix(ln, "configure") || strings.HasPrefix(ln, "set ") {
			t.Fatalf("被拒时不应执行变更语句: %v", f.lines)
		}
	}
}
