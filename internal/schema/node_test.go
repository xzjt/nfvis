package schema

import (
	"reflect"
	"strings"
	"testing"
)

func testDyn(kind string) []string {
	switch kind {
	case DynIfnames:
		return []string{"ens2f0", "ens2f1"}
	case DynVSwitches:
		return []string{"vs-app", "vs-underlay"}
	case DynVrfs:
		return []string{"vs-mgmt"}
	case DynVMs:
		return []string{"fw-vm"}
	case DynContainers:
		return []string{"sbc-ct1"}
	case DynImages:
		return []string{"ubuntu22-vm", "alpine-ct"}
	case DynClasses:
		return []string{"super-user", "operator", "read-only"}
	case DynRevisions:
		return []string{"1", "2", "3"}
	case DynVppPlugins:
		return []string{"acl", "nat"}
	case DynAcls:
		return []string{"acl-web"}
	case DynQos:
		return []string{"pol-1"}
	}
	return nil
}

func TestMatchExactAndParams(t *testing.T) {
	n, depth, err := Match(OperRoot(), []string{"show", "version"})
	if err != nil || n == nil || depth != 2 {
		t.Fatalf("show version: node=%v depth=%d err=%v", n, depth, err)
	}
	if n.Kind != Keyword || n.Name != "version" {
		t.Fatalf("应为 version 关键字节点: %+v", n)
	}

	// 参数节点按 token 消耗（实例名任意，运行期再校验存在性）
	n, depth, err = Match(OperRoot(), []string{"show", "virtual-machine-functions", "fw-vm", "interfaces"})
	if err != nil || depth != 4 || n.Name != "interfaces" {
		t.Fatalf("参数消耗失败: node=%v depth=%d err=%v", n, depth, err)
	}

	// 未知关键字报错
	if _, _, err := Match(OperRoot(), []string{"show", "no-such"}); err == nil {
		t.Fatalf("未知关键字应报错")
	}
}

func TestMatchAbbreviation(t *testing.T) {
	// FR-CLI-004 / 命令树 §5.5：无歧义前缀即合法
	n, _, err := Match(OperRoot(), []string{"sh", "ver"})
	if err != nil || n.Name != "version" {
		t.Fatalf("缩写 sh ver 应命中 show version: %v %v", n, err)
	}
	n, _, err = Match(OperRoot(), []string{"show", "virtual-m"})
	if err != nil || n.Name != "virtual-machine-functions" {
		t.Fatalf("无歧义前缀应消歧: %v %v", n, err)
	}
	// "vir" 同时匹配 virtual-machine-functions 与 virtual-switches → 歧义报错
	if _, _, err := Match(OperRoot(), []string{"show", "vir"}); err == nil || !strings.Contains(err.Error(), "歧义") {
		t.Fatalf("歧义前缀应报错: %v", err)
	}
}

func TestMatchValuesAndOptional(t *testing.T) {
	// 值叶子消耗 token
	n, depth, err := Match(ConfigPathTree(), []string{"system", "hostname", "node1"})
	if err != nil || depth != 3 || n.Kind != Value {
		t.Fatalf("值消耗: node=%+v depth=%d err=%v", n, depth, err)
	}
	// 连续位置参数（bonds … members [<seq>] <ifname>）按首参重复匹配——
	// 这条机制仍被 bonds members / dhcp-server pool 等形态使用
	// （历史用例是 cross-connect <a> <b>，该叶子已与模型 bool 对齐为显式取值开关）。
	n, depth, err = Match(ConfigPathTree(), []string{"bonds", "bond0", "members", "1", "ens2f0"})
	if err != nil || depth != 5 {
		t.Fatalf("连续参数: node=%+v depth=%d err=%v", n, depth, err)
	}
	// cross-connect 是**单个取值叶子**：值位置就是树的中枢（?/Tab 在此列 true|false），
	// 不再有两个位置参数（曾与模型 bool 不一致，见 tree_config.go 注释）。
	cc, _, err := Match(ConfigPathTree(), []string{"virtual-switches", "vs1", "cross-connect"})
	if err != nil || cc.Kind != Keyword || cc.singleValue() == nil || cc.singleValue().ParamType != "bool" {
		t.Fatalf("cross-connect 应是带 bool 取值叶子的关键字: node=%+v err=%v", cc, err)
	}
	for _, c := range cc.Children {
		if c.Kind == Param {
			t.Fatalf("cross-connect 不应再挂位置参数: %+v", c)
		}
	}
	toks := candidateNames(Candidates(ConfigPathTree(), []string{"virtual-switches", "vs1", "cross-connect"}, "", testDyn))
	if !contains(toks, "true") || !contains(toks, "false") {
		t.Fatalf("cross-connect 值位置候选应为 true|false: %v", toks)
	}
	// commit confirmed [minutes] 可选值
	if _, _, err := Match(ConfigRoot(), []string{"commit", "confirmed"}); err != nil {
		t.Fatalf("confirmed 不带分钟数应合法: %v", err)
	}
	if _, _, err := Match(ConfigRoot(), []string{"commit", "confirmed", "15"}); err != nil {
		t.Fatalf("confirmed 15 应合法: %v", err)
	}
}

func TestCandidatesKeywords(t *testing.T) {
	cs := Candidates(OperRoot(), nil, "", testDyn)
	names := candidateNames(cs)
	for _, want := range []string{"show", "request", "configure", "ping", "exit", "help"} {
		if !contains(names, want) {
			t.Fatalf("根候选缺少 %s: %v", want, names)
		}
	}
	// 前缀过滤（FR-CLI-002：? 前有部分字符只列以此为前缀的候选）
	cs = Candidates(OperRoot(), []string{"show"}, "vi", testDyn)
	names = candidateNames(cs)
	if !contains(names, "virtual-machine-functions") || !contains(names, "virtual-switches") {
		t.Fatalf("前缀 vi 应列出两个 virtual-* 候选: %v", names)
	}
	if contains(names, "show") {
		t.Fatalf("前缀过滤不应包含无关候选: %v", names)
	}
}

func TestCandidatesDynamicAndEnum(t *testing.T) {
	// 参数位置的动态候选（FR-CLI-002/003）
	cs := Candidates(OperRoot(), []string{"show", "virtual-machine-functions"}, "", testDyn)
	if names := candidateNames(cs); !contains(names, "fw-vm") {
		t.Fatalf("动态候选应含 fw-vm: %v", names)
	}
	// 动态候选也按前缀过滤
	cs = Candidates(OperRoot(), []string{"show", "virtual-machine-functions"}, "fw", testDyn)
	if names := candidateNames(cs); len(names) != 1 || names[0] != "fw-vm" {
		t.Fatalf("前缀 fw 应只余 fw-vm: %v", names)
	}
	// 枚举值候选：set virtual-switches <n> type <l2|l3>
	cs = Candidates(ConfigPathTree(), []string{"virtual-switches", "vs1", "type"}, "", testDyn)
	names := candidateNames(cs)
	if !contains(names, "l2") || !contains(names, "l3") {
		t.Fatalf("枚举候选应含 l2/l3: %v", names)
	}
	// 枚举值前缀过滤
	cs = Candidates(ConfigPathTree(), []string{"virtual-switches", "vs1", "type"}, "l2", testDyn)
	if names := candidateNames(cs); len(names) != 1 || names[0] != "l2" {
		t.Fatalf("枚举 l2 前缀应唯一命中: %v", names)
	}
}

func TestCandidatesConfigMode(t *testing.T) {
	// FR-CLI-007：配置模式下 ?/Tab 依据配置 schema 提供 set/delete 语句补全
	cs := Candidates(ConfigRoot(), []string{"set"}, "", testDyn)
	names := candidateNames(cs)
	for _, want := range []string{"system", "interfaces", "virtual-switches", "vpp", "resource-pools"} {
		if !contains(names, want) {
			t.Fatalf("set 下应列出配置分支 %s: %v", want, names)
		}
	}
	// run 挂操作树
	cs = Candidates(ConfigRoot(), []string{"run"}, "", testDyn)
	if !contains(candidateNames(cs), "show") {
		t.Fatalf("run 下应挂操作树: %v", candidateNames(cs))
	}
}

func TestValidateTree(t *testing.T) {
	for name, root := range map[string]*Node{"oper": OperRoot(), "config": ConfigRoot(), "cfgpath": ConfigPathTree()} {
		if errs := ValidateTree(root); len(errs) != 0 {
			t.Fatalf("%s 树校验失败: %v", name, errs)
		}
	}
	// 构造坏树：重复子节点
	bad := K("", "root", K("a", "重复"), K("a", "重复"))
	if errs := ValidateTree(bad); len(errs) == 0 {
		t.Fatalf("重复子节点应报错")
	}
	// 无描述节点
	bad2 := K("", "root", K("a", ""))
	if errs := ValidateTree(bad2); len(errs) == 0 {
		t.Fatalf("缺描述应报错")
	}
}

func TestClassPermissionMatrix(t *testing.T) {
	// 命令树 §4 预置 class 权限矩阵
	cases := []struct {
		path []string
		want Class
	}{
		{[]string{"show"}, ClassReadOnly},
		{[]string{"show", "version"}, ClassReadOnly},
		{[]string{"configure"}, ClassSuperUser},
		{[]string{"request", "virtual-machine-functions", "<name>", "start"}, ClassOperator},
		{[]string{"request", "container-functions", "<name>", "log"}, ClassOperator},
		{[]string{"request", "images", "upload"}, ClassOperator},
		{[]string{"request", "interfaces", "<ifname>", "enable"}, ClassOperator},
		{[]string{"request", "vpp", "restart"}, ClassSuperUser},
		{[]string{"request", "system", "software", "add"}, ClassSuperUser},
		{[]string{"request", "system", "reboot"}, ClassSuperUser},
		{[]string{"request", "system", "zeroize"}, ClassSuperUser},
		{[]string{"request", "system", "configuration", "restore"}, ClassSuperUser},
		{[]string{"clear", "interfaces", "statistics"}, ClassSuperUser},
		{[]string{"start", "shell"}, ClassSuperUser},
	}
	for _, c := range cases {
		n, err := Find(OperRoot(), c.path...)
		if err != nil {
			t.Fatalf("路径 %v 不存在: %v", c.path, err)
		}
		if got := n.RequiredClass(); got != c.want {
			t.Errorf("路径 %v 权限应为 %v，实际 %v", c.path, c.want, got)
		}
	}
	// 配置模式全部 S（§2 头注）
	for _, p := range [][]string{{"set"}, {"commit"}, {"rollback"}, {"load", "override"}} {
		n, err := Find(ConfigRoot(), p...)
		if err != nil {
			t.Fatalf("路径 %v 不存在: %v", p, err)
		}
		if got := n.RequiredClass(); got != ClassSuperUser {
			t.Errorf("配置命令 %v 应为 S，实际 %v", p, got)
		}
	}
}

func TestPipeKeywords(t *testing.T) {
	// FR-CLI-005：通用管道关键字由 schema 单一来源提供
	want := []string{"match", "except", "count", "last", "begin", "display"}
	for _, w := range want {
		if !contains(PipeKeywords, w) {
			t.Fatalf("管道关键字缺少 %s: %v", w, PipeKeywords)
		}
	}
}

func candidateNames(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Token)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------- W3：命令缩写（FR-CLI-004，命令树 §5.5） ----------

func TestAbbreviationExpand(t *testing.T) {
	// 无歧义前缀即合法：conf → configure
	n, _, err := Match(OperRoot(), []string{"conf"})
	if err != nil || n.Name != "configure" {
		t.Fatalf("conf 应消歧到 configure: %v %v", n, err)
	}
	// 多级缩写：sh vir 歧义、sh virtu... 逐级消歧
	if _, _, err := Match(OperRoot(), []string{"sh", "vir"}); err == nil || !strings.Contains(err.Error(), "virtual-switches") {
		t.Fatalf("歧义报错应列出候选: %v", err)
	}
	if _, _, err := Match(OperRoot(), []string{"sh", "vir"}); err == nil || !strings.Contains(err.Error(), "virtual-machine-functions") {
		t.Fatalf("歧义报错应列出全部候选: %v", err)
	}
	// 配置语句树缩写：set vir...
	n, _, err = Match(ConfigRoot(), []string{"set", "vir"})
	if err == nil {
		t.Fatalf("set vir 应歧义")
	}
	if !strings.Contains(err.Error(), "virtual") {
		t.Fatalf("歧义应列出 virtual 候选: %v", err)
	}
	_ = n
}

func TestCanonicalizeAbbrev(t *testing.T) {
	// 操作命令：无歧义前缀替换为规范关键字
	got, err := Canonicalize(OperRoot(), []string{"conf"})
	if err != nil || len(got) != 1 || got[0] != "configure" {
		t.Fatalf("conf 应规整为 configure: %v %v", got, err)
	}
	got, err = Canonicalize(OperRoot(), []string{"sh", "conf"})
	if err != nil || len(got) != 2 || got[0] != "show" || got[1] != "configuration" {
		t.Fatalf("sh conf 应规整: %v %v", got, err)
	}
	// 参数/取值原样保留
	got, err = Canonicalize(ConfigRoot(), []string{"set", "sys", "hostn", "demo-node"})
	if err != nil || strings.Join(got, " ") != "set system hostname demo-node" {
		t.Fatalf("set sys hostn 应规整且值保留: %v %v", got, err)
	}
	// 歧义仍报错并列出候选
	if _, err := Canonicalize(OperRoot(), []string{"sh", "vir"}); err == nil || !strings.Contains(err.Error(), "virtual-switches") {
		t.Fatalf("歧义应报错列候选: %v", err)
	}
	// 树未建模的语法原样透传（执行器自行支持）
	toks := []string{"show", "configuration", "compare", "rollback", "2"}
	got, err = Canonicalize(OperRoot(), toks)
	if err != nil || strings.Join(got, " ") != strings.Join(toks, " ") {
		t.Fatalf("未建模语法应原样保留: %v %v", got, err)
	}
}

// TestCandidatesAfterConsumingParam：无子树的参数（实例名/标量取值）消耗一个 token 后，
// 候选必须来自**父层关键字**——与包注释写明的匹配语义（「值叶子与无子树参数消耗一个 token
// 后回到父关键字层继续匹配」）以及 Match 每个 token 开头的 `consumesToken() → parent` 一致。
//
// 由来（附录 A #90①）：这些位置此前**一个候选都列不出来**，操作者只能手打子关键字。
// 同一形态在树里有两种建模，掩盖了缺陷——`login user` 把子关键字放成**兄弟**（坏），
// `login class` / `interfaces` 把子节点**挂**在参数上（好）。故此处两种建模都要断言。
func TestCandidatesAfterConsumingParam(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
		want   []string
		absent []string
	}{
		{
			name:   "实例名之后（兄弟式建模）",
			tokens: []string{"set", "system", "login", "user", "admin"},
			want:   []string{"password", "class"},
			absent: []string{"<name>"}, // 同一位置再给一个用户名没有意义
		},
		{
			name:   "标量取值之后（需跨一层透明包装）",
			tokens: []string{"set", "system", "management", "interface", "ens160"},
			want:   []string{"ip", "gateway"},
			absent: []string{"interface"}, // 来路不再列
		},
		{
			name:   "标量取值之后（同级兄弟）",
			tokens: []string{"set", "system", "ntp", "server", "1.2.3.4"},
			want:   []string{"prefer"},
		},
		{
			// 挂载式建模一直是对的，不能被这次改动弄坏
			name:   "实例名之后（挂载式建模）",
			tokens: []string{"set", "system", "login", "class", "foo"},
			want:   []string{"allow", "deny"},
		},
		{
			name:   "裸声明端口仍应列出子关键字",
			tokens: []string{"set", "interfaces", "ens224"},
			want:   []string{"description", "mtu", "disable"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			names := candidateNames(Candidates(ConfigRoot(), c.tokens, "", testDyn))
			for _, w := range c.want {
				if !contains(names, w) {
					t.Errorf("缺少候选 %q，实得 %v", w, names)
				}
			}
			for _, a := range c.absent {
				if contains(names, a) {
					t.Errorf("不应出现候选 %q，实得 %v", a, names)
				}
			}
		})
	}
	// 位置前缀过滤仍生效（层级由结构定，不由输入定）
	names := candidateNames(Candidates(ConfigRoot(), []string{"set", "system", "login", "user", "admin"}, "pa", testDyn))
	if len(names) != 1 || names[0] != "password" {
		t.Errorf("前缀 pa 应唯一命中 password，实得 %v", names)
	}
}

// TestCandidateLabelUsesPlaceholderType：候选描述里的类型标签取自占位符（`<ip>` → ip），
// 不是 Node.ParamType——后者对 P/SP/SPA/SPD 统一是 "name"（供 scalarForNode 做取值转换），
// 拿它当标签会写出「服务器地址（name）」这种自相矛盾的候选（附录 A #90④）。
func TestCandidateLabelUsesPlaceholderType(t *testing.T) {
	var got string
	for _, c := range Candidates(ConfigRoot(), []string{"set", "system", "dns", "server"}, "", testDyn) {
		if c.Token == "<ip>" {
			got = c.Desc
		}
	}
	if got == "" {
		t.Fatal("dns server 位置应给出 <ip> 候选")
	}
	if strings.Contains(got, "（name）") {
		t.Errorf("类型标签不得写死成 name: %q", got)
	}
	if !strings.Contains(got, "（ip）") {
		t.Errorf("类型标签应取自占位符 <ip>: %q", got)
	}
}

// TestLoginUserRequiresSubKeyword：`login user <name>` 标了 RQ——单独成句会落库出
// 「有名字、无口令、无 class」的账号（真机实测 CLI 回 [ok] 且 commit 成功），
// 与本项目「不静默建无口令账号」的既有口径冲突（附录 A #90②）。
func TestLoginUserRequiresSubKeyword(t *testing.T) {
	n, _, err := Match(ConfigRoot(), []string{"set", "system", "login", "user", "someone"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !n.RequireSub {
		t.Fatal("login user 的 <name> 参数应标记 RequireSub")
	}
	// 裸声明本身有意义的节点**不得**被误标（决策 #72：先声明端口、绑定后再提交）
	n, _, err = Match(ConfigRoot(), []string{"set", "interfaces", "ens224"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if n.RequireSub {
		t.Fatal("interfaces <ifname> 裸声明是受支持流程，不应标记 RequireSub")
	}
}

// TestPipeCandidates（决策 #155 补充三，FR-CLI-002）：管道段内的 ?/Tab 候选——
// 段首列管道关键字、display/compare 列取值枚举、自由取值位无候选、前缀过滤生效。
func TestPipeCandidates(t *testing.T) {
	tokens := func(cs []Candidate) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Token)
		}
		return out
	}
	// 段首：全部管道关键字（含描述）
	cs := PipeCandidates(nil, "")
	got := tokens(cs)
	want := []string{"begin", "compare", "count", "display", "except", "last", "match"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("段首候选应为全部管道关键字（排序）: %v", got)
	}
	for _, c := range cs {
		if c.Desc == "" {
			t.Fatalf("管道关键字候选应带描述: %v", c)
		}
	}
	// 前缀过滤
	if got := tokens(PipeCandidates(nil, "dis")); len(got) != 1 || got[0] != "display" {
		t.Fatalf("前缀 dis 应只列 display: %v", got)
	}
	// display 取值枚举
	if got := tokens(PipeCandidates([]string{"display"}, "")); !reflect.DeepEqual(got, []string{"json", "set", "xml"}) &&
		!reflect.DeepEqual(got, []string{"json", "xml", "set"}) {
		t.Fatalf("display 取值应列 json/xml/set: %v", got)
	}
	// compare 取值枚举
	if got := tokens(PipeCandidates([]string{"compare"}, "r")); len(got) != 1 || got[0] != "rollback" {
		t.Fatalf("compare 取值应列 rollback: %v", got)
	}
	// 自由取值位（正则/行数）无候选
	if got := PipeCandidates([]string{"match"}, ""); len(got) != 0 {
		t.Fatalf("match 取值位为自由正则、应无候选: %v", got)
	}
	// 取值给全后本段无候选
	if got := PipeCandidates([]string{"display", "json"}, ""); len(got) != 0 {
		t.Fatalf("display json 之后本段应无候选: %v", got)
	}
	// 未知关键字段无候选
	if got := PipeCandidates([]string{"bogus"}, ""); len(got) != 0 {
		t.Fatalf("未知管道关键字段应无候选: %v", got)
	}
}

// TestOperCommandPaths 决策 #304：命令路径枚举与命令树同源、稳定、含代表性路径。
func TestOperCommandPaths(t *testing.T) {
	paths := OperCommandPaths()
	if len(paths) < 100 {
		t.Fatalf("只枚举到 %d 条命令路径（枚举器可能失效）", len(paths))
	}
	seen := map[string]bool{}
	for _, cp := range paths {
		if len(cp.Path) == 0 {
			t.Fatalf("空路径")
		}
		key := strings.Join(cp.Path, " ")
		if seen[key] {
			t.Fatalf("重复路径: %s", key)
		}
		seen[key] = true
		// 与 Match 同源：每条路径都能解析到，且等级一致
		n, _, err := Match(OperRoot(), cp.Path)
		if err != nil {
			t.Fatalf("路径 %v 解析失败: %v", cp.Path, err)
		}
		if got := n.RequiredClass(); got != cp.Required {
			t.Fatalf("路径 %v 等级不一致: 枚举 %v / Match %v", cp.Path, cp.Required, got)
		}
	}
	// 代表性路径与等级：show 全 R；request 破坏性动作 S；request 生命周期 O
	cases := []struct {
		path string
		want Class
	}{
		{"show version", ClassReadOnly},
		{"show configuration permissions <class>", ClassReadOnly},
		{"show configuration permissions <class> detail", ClassReadOnly},
		{"configure", ClassSuperUser},
		{"request vpp restart", ClassSuperUser},
		{"request system reboot", ClassSuperUser},
		{"request virtual-machine-functions <name> start", ClassOperator},
	}
	for _, c := range cases {
		if !seen[c.path] {
			t.Errorf("枚举结果里应有路径 %q", c.path)
			continue
		}
		if _, _, err := Match(OperRoot(), strings.Fields(c.path)); err != nil {
			t.Errorf("路径 %q 应可解析: %v", c.path, err)
		}
	}
	// 稳定：两次调用逐字一致
	again := OperCommandPaths()
	if len(again) != len(paths) {
		t.Fatalf("两次枚举条数不同: %d / %d", len(paths), len(again))
	}
	for i := range paths {
		if strings.Join(paths[i].Path, " ") != strings.Join(again[i].Path, " ") {
			t.Fatalf("第 %d 条不稳定: %v / %v", i, paths[i].Path, again[i].Path)
		}
	}
}

// 决策 #324：预置 class 名 → 等级映射与覆盖判定的单一事实源（internal/aaa 与客户端共用）。
func TestPresetClassLevelAndCovers(t *testing.T) {
	cases := []struct {
		name string
		want Class
		ok   bool
	}{
		{ClassNameSuperUser, ClassSuperUser, true},
		{ClassNameOperator, ClassOperator, true},
		{ClassNameReadOnly, ClassReadOnly, true},
		{"my-custom", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := PresetClassLevel(c.name)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("PresetClassLevel(%q) = (%v,%v)，期望 (%v,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
	// R ⊂ O ⊂ S
	if !ClassSuperUser.Covers(ClassSuperUser) || !ClassSuperUser.Covers(ClassReadOnly) {
		t.Error("super-user 应覆盖全部等级")
	}
	if !ClassOperator.Covers(ClassReadOnly) || ClassOperator.Covers(ClassSuperUser) {
		t.Error("operator 覆盖 R 但不覆盖 S")
	}
	if ClassReadOnly.Covers(ClassOperator) || ClassReadOnly.Covers(ClassSuperUser) {
		t.Error("read-only 只覆盖 R")
	}
}

// CandidatesFiltered：过滤谓词按节点等级剔除无权入口，且不误伤同级允许项。
func TestCandidatesFiltered(t *testing.T) {
	ro, _ := PresetClassLevel(ClassNameReadOnly)
	allow := func(_ []string, n *Node) bool { return ro.Covers(n.RequiredClass()) }

	// read-only：顶层 request/configure 被剔除，show 保留。
	cs := CandidatesFiltered(OperRoot(), nil, "", nil, allow)
	if !candidateHas(cs, "show") {
		t.Fatalf("过滤后应保留 show: %v", cs)
	}
	for _, absent := range []string{"request", "configure", "ping", "clear", "start"} {
		if candidateHas(cs, absent) {
			t.Errorf("read-only 过滤后不应含 %q: %v", absent, cs)
		}
	}

	// nil 过滤 == 不过滤（Candidates 与 CandidatesFiltered 行为一致）。
	all := Candidates(OperRoot(), nil, "", nil)
	filtered := CandidatesFiltered(OperRoot(), nil, "", nil, nil)
	if !reflect.DeepEqual(all, filtered) {
		t.Fatalf("nil 过滤应与 Candidates 逐条一致:\n %v\n %v", all, filtered)
	}

	// 谓词若一律拒绝，则一个候选都不给（判据真的生效，不是摆设）。
	none := CandidatesFiltered(OperRoot(), nil, "", nil, func([]string, *Node) bool { return false })
	if len(none) != 0 {
		t.Fatalf("全拒谓词应得空候选: %v", none)
	}

	// NodePath：show interfaces 下参数的规范路径与 OperCommandPaths 同形。
	n, _, err := Match(OperRoot(), []string{"show", "interfaces"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := strings.Join(NodePath(n), " "); got != "show interfaces" {
		t.Fatalf("NodePath = %q，期望 show interfaces", got)
	}
}

func candidateHas(cs []Candidate, tok string) bool {
	for _, c := range cs {
		if c.Token == tok {
			return true
		}
	}
	return false
}

// 决策 #359：dhcp-server 的 `?`/Tab 候选——pool 是两个连续位置值（先补 <start>、给完一个值
// 再补 <end>，与语句位置一一对应）；dhcp-server 下列出全部四个叶子。
func TestCandidatesDhcpServerPoolTwoPositionals(t *testing.T) {
	root := ConfigRoot()
	base := []string{"set", "virtual-switches", "vs1", "dhcp-server"}
	if names := candidateNames(Candidates(root, base, "", testDyn)); !contains(names, "pool") {
		t.Fatalf("dhcp-server 下应列出 pool: %v", names)
	}
	// pool 后先补 <start>
	first := Candidates(root, append(append([]string{}, base...), "pool"), "", testDyn)
	if names := candidateNames(first); len(names) != 1 || names[0] != "<start>" {
		t.Fatalf("pool 后应只补 <start>: %v", names)
	}
	// 给了起始地址后补 <end>
	second := Candidates(root, append(append([]string{}, base...), "pool", "192.168.100.10"), "", testDyn)
	if names := candidateNames(second); len(names) != 1 || names[0] != "<end>" {
		t.Fatalf("给完 <start> 后应只补 <end>: %v", names)
	}
}
