package schema

import (
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
	// 连续参数（cross-connect <port-a> <port-b>）
	n, depth, err = Match(ConfigPathTree(), []string{"virtual-switches", "vs1", "cross-connect", "p1", "p2"})
	if err != nil || depth != 5 {
		t.Fatalf("连续参数: node=%+v depth=%d err=%v", n, depth, err)
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
