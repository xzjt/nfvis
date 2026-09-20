package cli

// `wizard` 初始化向导（决策 #107）：CLI 端交互编排——问答规划资源池/VPP 线程/大页/
// 低延迟，展示将要提交的语句清单，确认后**经既有语句**执行（configure/set/commit/
// request system kernel apply）。向导只是语句的生成器：commit 校验与内核基线护栏
// （决策 #104）原样生效，不绕过任何校验。
//
// 与 `monitor` 同类的「CLI 端编排」先例：无独立 API 端点，openapi 不动。
// 非 TTY 打印指引即返回、不挂起（决策 #80⑤ 同款口径）。
// 主机事实取自 /api/v1/metrics（prometheus 文本，机器可读；解析它不属于
// 「解析 show 表格文本」的脆弱类——决策 #85 的教训）；committed 现状取自
// `show configuration | display json`。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// isWizard 识别顶级 `wizard`。**精确匹配**：命令名本身已避开 set 家族的前缀纠葛
// （FR-CLI-004 的无歧义前缀规全会把 "set" 展开成任何 set* 开头的操作词）。
func isWizard(line string) bool {
	return strings.TrimSpace(line) == "wizard"
}

// SetupFacts 向导依据的主机事实与服务端现状。
type SetupFacts struct {
	OnlineCPUs int            // 在线核数（0 = 事实不可用）
	MemTotalGB int            // 物理内存总量（0 = 不可用）
	Pools      map[string]int // committed 现有大页池（page_size → count）
	HasVPPCPU  bool           // committed 已配置 vpp cpu
}

// SetupPlan 向导推导出的计划（含将要提交的语句清单）。
type SetupPlan struct {
	IsolatedCores string
	VPPMain       int // 0 = 不设 vpp cpu
	VPPWorker     int // 0 = 不设工作线程
	HP1G          int // 0 = 不配 1G 池
	HP2M          int // 0 = 不配 2M 池
	HugepagePref  string
	LowLatency    bool
	Statements    []string
}

// setupAnswers 操作者在问答里给出的原始输入（已解析；零值 = 采用默认）。
type setupAnswers struct {
	Isolated   string // "" = 用默认
	VPPMain    *int
	VPPWorker  *int
	HP1G       *int
	HP2M       *int
	LowLatency *bool
}

// parseMetricsValue 从 prometheus 文本里取某指标的值（行首精确匹配指标名）。
func parseMetricsValue(text, name string) (float64, bool) {
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(ln, name+" "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return 0, false
			}
			return f, true
		}
	}
	return 0, false
}

// fetchSetupFacts 拉取主机事实与 committed 现状。任一来源失败即降级（字段留零值），
// 向导在对应问题上不给默认、要求显式输入，而不是猜测。
func (s *Session) fetchSetupFacts() SetupFacts {
	f := SetupFacts{Pools: map[string]int{}}
	if txt, err := s.MetricsText(); err == nil {
		if v, ok := parseMetricsValue(txt, "nfvis_system_cpu_online_count"); ok {
			f.OnlineCPUs = int(v)
		}
		if v, ok := parseMetricsValue(txt, "nfvis_system_memory_total_bytes"); ok {
			f.MemTotalGB = int(v / (1 << 30))
		}
	}
	if out, _ := s.ExecuteLine("show configuration | display json"); out != "" {
		var committed struct {
			ResourcePools *struct {
				Hugepages []struct {
					PageSize string `json:"page_size"`
					Count    int    `json:"count"`
				} `json:"hugepages"`
				CPU *struct {
					IsolatedCores []int `json:"isolated_cores"`
				} `json:"cpu"`
			} `json:"resource_pools"`
			Vpp *struct {
				CPU *struct {
					MainCore int `json:"main_core"`
				} `json:"cpu"`
			} `json:"vpp"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(out)), &committed) == nil {
			if committed.ResourcePools != nil {
				for _, hp := range committed.ResourcePools.Hugepages {
					f.Pools[hp.PageSize] = hp.Count
				}
			}
			f.HasVPPCPU = committed.Vpp != nil && committed.Vpp.CPU != nil && committed.Vpp.CPU.MainCore > 0
		}
	}
	return f
}

// defaultIsolatedCores 隔离核默认建议：保留 0-1 给宿主/管理面，隔离 2-末核。
// 核数不足以保留 2 个宿主核时返回空（向导要求显式输入）。
func defaultIsolatedCores(online int) string {
	switch {
	case online >= 4:
		return "2-" + strconv.Itoa(online-1)
	case online == 3:
		return "2"
	}
	return ""
}

// parseCoreListText 解析 "1,4-7" 形式核列表（向导输入校验用；与服务端 parseCoreList 同语义）。
func parseCoreListText(s string) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, e1 := strconv.Atoi(a)
			hi, e2 := strconv.Atoi(b)
			if e1 != nil || e2 != nil || lo > hi {
				return nil, fmt.Errorf("核区间 %q 不合法", part)
			}
			for x := lo; x <= hi; x++ {
				if !seen[x] {
					seen[x] = true
					out = append(out, x)
				}
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("核编号 %q 不合法", part)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

// deriveSetupPlan 由事实与答案推导计划（纯函数，单测覆盖）。
// 校验与服务端同口径：宿主/管理面至少保留 2 核（决策 #104 护栏）、VPP 线程须在隔离核内
// （commit 校验 FR-CMP-001）。错误在向导阶段就给可读原因，不等 commit 才报。
func deriveSetupPlan(f SetupFacts, a setupAnswers) (SetupPlan, error) {
	p := SetupPlan{}

	// —— 隔离核 ——
	isoText := a.Isolated
	if isoText == "" {
		isoText = defaultIsolatedCores(f.OnlineCPUs)
		if isoText == "" {
			return p, fmt.Errorf("在线核数 %d 不足以给出默认隔离核建议（宿主/管理面至少保留 2 核），请显式输入隔离核范围", f.OnlineCPUs)
		}
	}
	iso, err := parseCoreListText(isoText)
	if err != nil {
		return p, err
	}
	if f.OnlineCPUs > 0 {
		for _, c := range iso {
			if c < 0 || c >= f.OnlineCPUs {
				return p, fmt.Errorf("隔离核 %d 超出在线核范围（本机 %d 个核：0-%d）", c, f.OnlineCPUs, f.OnlineCPUs-1)
			}
		}
		if keep := f.OnlineCPUs - len(iso); keep < 2 {
			return p, fmt.Errorf("隔离 %d 个核后宿主/管理面只剩 %d 个核，至少保留 2 个；请缩小隔离核范围", len(iso), keep)
		}
	}
	p.IsolatedCores = compressCoreText(iso)
	p.HP1G, p.HP2M = f.Pools["1G"], f.Pools["2M"]

	// —— VPP 线程：默认取隔离范围最大的两核（主=最大、工作=次大），须在隔离核内 ——
	if a.VPPMain != nil {
		p.VPPMain = *a.VPPMain
	} else if len(iso) > 0 {
		p.VPPMain = iso[len(iso)-1]
	}
	if a.VPPWorker != nil {
		p.VPPWorker = *a.VPPWorker
	} else if len(iso) > 1 {
		p.VPPWorker = iso[len(iso)-2]
	}
	inISO := func(c int) bool {
		for _, x := range iso {
			if x == c {
				return true
			}
		}
		return false
	}
	if p.VPPMain != 0 && !inISO(p.VPPMain) {
		return p, fmt.Errorf("VPP 主线程核 %d 不在隔离核 %s 内（commit 校验同样会拒绝）", p.VPPMain, p.IsolatedCores)
	}
	if p.VPPWorker != 0 && !inISO(p.VPPWorker) {
		return p, fmt.Errorf("VPP 工作线程核 %d 不在隔离核 %s 内（commit 校验同样会拒绝）", p.VPPWorker, p.IsolatedCores)
	}
	if p.VPPWorker != 0 && p.VPPWorker == p.VPPMain {
		return p, fmt.Errorf("VPP 工作线程核 %d 与主线程相同", p.VPPWorker)
	}

	// —— 大页池：默认沿用既有池；未配置时 1G 按内存配比（min(RAM_GB/4,8) 钳 1~8）、2M 给 768 ——
	if a.HP1G != nil {
		p.HP1G = *a.HP1G
	} else if f.Pools["1G"] == 0 {
		p.HP1G = defaultHP1G(f.MemTotalGB)
	}
	if a.HP2M != nil {
		p.HP2M = *a.HP2M
	} else if f.Pools["2M"] == 0 {
		p.HP2M = 768
	}
	if p.HP1G < 0 || p.HP2M < 0 {
		return p, fmt.Errorf("大页数量不能为负")
	}

	// —— preference 须与池一致（commit 校验 FR-CMP-001）：有 2M 池用 2M（1G 页让给 VM），否则 1G ——
	switch {
	case p.HP2M > 0:
		p.HugepagePref = "2M"
	case p.HP1G > 0:
		p.HugepagePref = "1G"
	}

	if a.LowLatency != nil {
		p.LowLatency = *a.LowLatency
	}

	p.Statements = planStatements(p)
	return p, nil
}

// defaultHP1G 与安装器 --defaults 同公式：min(RAM_GB/4, 8)，至少 1。
func defaultHP1G(memGB int) int {
	if memGB <= 0 {
		return 0 // 事实不可用 → 不给默认
	}
	n := memGB / 4
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}

// compressCoreText 把核号压成紧凑区间文本（[2,3,4,5] → "2-5"；与语句树里
// isolated-cores 的取值格式一致）。cli 是薄客户端不得 import internal/system，故本地实现。
func compressCoreText(cores []int) string {
	if len(cores) == 0 {
		return ""
	}
	var parts []string
	start, prev := cores[0], cores[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
			return
		}
		parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(prev))
	}
	for _, x := range cores[1:] {
		if x == prev+1 {
			prev = x
			continue
		}
		flush()
		start, prev = x, x
	}
	flush()
	return strings.Join(parts, ",")
}

// planStatements 计划对应的语句清单（向导确认后逐条执行的正是它）。
func planStatements(p SetupPlan) []string {
	var out []string
	out = append(out, "configure")
	if p.HP1G > 0 {
		out = append(out, fmt.Sprintf("set resource-pools hugepages page-size 1G count %d", p.HP1G))
	}
	if p.HP2M > 0 {
		out = append(out, fmt.Sprintf("set resource-pools hugepages page-size 2M count %d", p.HP2M))
	}
	if p.IsolatedCores != "" {
		out = append(out, "set resource-pools cpu isolated-cores "+p.IsolatedCores)
	}
	if p.VPPMain > 0 {
		out = append(out, fmt.Sprintf("set vpp cpu main-core %d", p.VPPMain))
	}
	if p.VPPWorker > 0 {
		out = append(out, fmt.Sprintf("set vpp cpu corelist-workers %d", p.VPPWorker))
	}
	if p.HugepagePref != "" {
		out = append(out, "set vpp memory hugepage-preference "+p.HugepagePref)
	}
	if p.LowLatency {
		out = append(out, "set system kernel low-latency true")
	}
	out = append(out, "commit", "exit", "request system kernel apply")
	return out
}

// RunWizard 交互式初始化向导。interactive=false（管道/脚本）时打印指引并返回——不读输入、不挂起。
func RunWizard(sess *Session, interactive bool, in io.Reader, out io.Writer) error {
	if !interactive {
		fmt.Fprintln(out, "%% wizard 是交互式向导，需要在终端（TTY）中运行；当前输入不是终端。")
		fmt.Fprintln(out, "%% 请在交互式 nfvis-cli 中执行 wizard，或按用户手册「3.1 内核基线」用 set/request 语句手工完成。")
		return nil
	}
	rd := bufio.NewScanner(in)

	fmt.Fprintln(out, "NFViS 初始化向导（wizard）——规划资源池与内核基线（Enter 取 [默认]；输入 q 中止）。")
	f := sess.fetchSetupFacts()
	if f.OnlineCPUs > 0 {
		fmt.Fprintf(out, "本机事实：在线核 %d 个；内存 %d GB；committed 现有大页池 %v。\n", f.OnlineCPUs, f.MemTotalGB, poolSummary(f.Pools))
	} else {
		fmt.Fprintln(out, "本机事实不可用（/metrics 读取失败），以下问题需显式输入。")
	}

	// —— 1/4 隔离核 ——
	def := defaultIsolatedCores(f.OnlineCPUs)
	fmt.Fprintf(out, "\n1/4 隔离核（供 VPP 与 VM 使用，宿主/管理面保留其余至少 2 个） [默认 %s]： ", orNone(def))
	line := ask(rd)
	if line == "q" {
		fmt.Fprintln(out, "已中止（未做任何变更）。")
		return nil
	}
	// —— 2/4 VPP 线程（默认值依赖隔离核答案，先解析隔离核再问） ——
	ans := setupAnswers{Isolated: line}
	iso, err := parseCoreListText(orDefault(line, def))
	if err != nil {
		return err
	}
	mainDef, workerDef := 0, 0
	if len(iso) > 0 {
		mainDef = iso[len(iso)-1]
	}
	if len(iso) > 1 {
		workerDef = iso[len(iso)-2]
	}
	fmt.Fprintf(out, "2/4 VPP 主线程核 [默认 %s]（输入 - 表示不配置 VPP CPU）： ", orInt(mainDef))
	line = ask(rd)
	if line == "q" {
		fmt.Fprintln(out, "已中止（未做任何变更）。")
		return nil
	}
	if line == "-" {
		zero := 0
		ans.VPPMain, ans.VPPWorker = &zero, &zero
	} else if line != "" {
		v, err := strconv.Atoi(line)
		if err != nil {
			return fmt.Errorf("核编号 %q 不合法", line)
		}
		ans.VPPMain = &v
	}
	if ans.VPPMain == nil || *ans.VPPMain != 0 {
		fmt.Fprintf(out, "    VPP 工作线程核 [默认 %s]（输入 - 表示不设工作线程）： ", orInt(workerDef))
		line = ask(rd)
		if line == "q" {
			fmt.Fprintln(out, "已中止（未做任何变更）。")
			return nil
		}
		if line == "-" {
			zero := 0
			ans.VPPWorker = &zero
		} else if line != "" {
			v, err := strconv.Atoi(line)
			if err != nil {
				return fmt.Errorf("核编号 %q 不合法", line)
			}
			ans.VPPWorker = &v
		}
	}
	// —— 3/4 大页 ——
	hp1Def := f.Pools["1G"]
	if hp1Def == 0 {
		hp1Def = defaultHP1G(f.MemTotalGB)
	}
	fmt.Fprintf(out, "3/4 1G 大页数量（VM 内存从 1G 池分配） [默认 %s]： ", orInt(hp1Def))
	line = ask(rd)
	if line == "q" {
		fmt.Fprintln(out, "已中止（未做任何变更）。")
		return nil
	}
	if line != "" {
		v, err := strconv.Atoi(line)
		if err != nil || v < 0 {
			return fmt.Errorf("数量 %q 不合法", line)
		}
		ans.HP1G = &v
	}
	hp2Def := f.Pools["2M"]
	if hp2Def == 0 {
		hp2Def = 768
	}
	fmt.Fprintf(out, "    2M 大页数量（VPP 缓冲用；0 = 不配 2M 池） [默认 %d]： ", hp2Def)
	line = ask(rd)
	if line == "q" {
		fmt.Fprintln(out, "已中止（未做任何变更）。")
		return nil
	}
	if line != "" {
		v, err := strconv.Atoi(line)
		if err != nil || v < 0 {
			return fmt.Errorf("数量 %q 不合法", line)
		}
		ans.HP2M = &v
	}
	// —— 4/4 低延迟 ——
	fmt.Fprintln(out, "4/4 低延迟参数组（mitigations=off 等：降低安全缓解与可诊断性；虚拟机上自动省略 idle=poll/tsc=reliable） [默认 false]：")
	fmt.Fprintf(out, "    启用？true/false [默认 false]： ")
	line = ask(rd)
	if line == "q" {
		fmt.Fprintln(out, "已中止（未做任何变更）。")
		return nil
	}
	if line != "" {
		if line != "true" && line != "false" {
			return fmt.Errorf("布尔值 %q 不合法（true/false）", line)
		}
		v := line == "true"
		ans.LowLatency = &v
	}

	plan, err := deriveSetupPlan(f, ans)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\n—— 将提交以下语句 ——")
	for _, st := range plan.Statements {
		fmt.Fprintln(out, "  "+st)
	}
	fmt.Fprintf(out, "确认提交？[yes/no]（默认 yes）： ")
	line = ask(rd)
	if line == "q" || strings.EqualFold(line, "no") {
		fmt.Fprintln(out, "已中止（未做任何变更）。")
		return nil
	}

	// —— 执行：逐条经既有语句；任一步报错即停（candidate 保留，操作者可修正或 discard）。
	// 「语句未产生配置变更」例外：在已按相同值配置过的机器上重跑向导属预期，不算失败。 ——
	for _, st := range plan.Statements {
		o, _ := sess.ExecuteLine(st)
		if strings.TrimSpace(o) != "" {
			fmt.Fprint(out, o)
			if !strings.HasSuffix(o, "\n") {
				fmt.Fprintln(out)
			}
		}
		if strings.Contains(o, "语句未产生配置变更") {
			continue
		}
		if stepFailed(o) {
			fmt.Fprintln(out, "向导在上述步骤失败：candidate 已保留，可修正后重新 commit，或执行 discard 放弃。")
			return fmt.Errorf("语句执行失败: %s", st)
		}
	}
	fmt.Fprintln(out, "\n向导完成。内核基线需重启生效：request system reboot。")
	fmt.Fprintln(out, "重启后的固定动作（数据口绑定不跨重启）：request interfaces <数据口> bind-dpdk --yes → request vpp restart。")
	fmt.Fprintln(out, "（vfio 模块由绑定命令自动加载并持久化开机加载，无需手工 modprobe）")
	fmt.Fprintln(out, "数据口的声明（set interfaces / set vpp dpdk dev）不在向导范围内，见用户手册「3.2 业务网卡交 DPDK」。")
	return nil
}

// stepFailed 判定一条语句的输出是否失败——与 contrib/scripts/cli-fulltest 的判定模式同源：
// 行首单个或双个 %（`% 无效命令`、`%% 底座下发失败`…）与行首「校验失败」都算失败。
//
// 此前只查 "%%"，漏掉单 % 错误（如 `% 无效命令: request system kernel apply`），
// 于是向导在最后一步失败的情况下照样打印「向导完成」并返回成功——典型假绿
// （真机 round34 实测：计划里 top 未离开配置模式，apply 被判无效命令而向导报成功）。
func stepFailed(o string) bool {
	for _, line := range strings.Split(o, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "%") || strings.HasPrefix(t, "校验失败") {
			return true
		}
	}
	return false
}

// ask 读一行输入（去空白）；Scanner 出错按空行处理（上层用默认值或中止）。
func ask(rd *bufio.Scanner) string {
	if !rd.Scan() {
		return "q"
	}
	return strings.TrimSpace(rd.Text())
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orNone(s string) string {
	if s == "" {
		return "无"
	}
	return s
}

func orInt(n int) string {
	if n == 0 {
		return "无"
	}
	return strconv.Itoa(n)
}

func poolSummary(pools map[string]int) string {
	keys := make([]string, 0, len(pools))
	for k := range pools {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, pools[k]))
	}
	if len(parts) == 0 {
		return "（无）"
	}
	return strings.Join(parts, " ")
}
