package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------- W1：CLI 管道过滤（FR-CLI-005） ----------

func pipeSetup(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system hostname pipe-node",
		"set interfaces ens2f0 mtu 9000",
		"set interfaces ens2f1 description uplink",
		"set virtual-switches vs-app type l2",
		"commit",
		"exit",
	)
	return x
}

func TestPipeMatch(t *testing.T) {
	x := pipeSetup(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | match hostname")
	if !strings.Contains(res.Output, "hostname pipe-node") || strings.Contains(res.Output, "mtu") {
		t.Fatalf("match 应只保留命中行:\n%s", res.Output)
	}
}

func TestPipeExcept(t *testing.T) {
	x := pipeSetup(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | except system")
	// except 按行过滤：system 块头被排除，块内行（不含 system 字样）保留
	if strings.Contains(res.Output, "system {") {
		t.Fatalf("except 应排除 system 头行:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "mtu 9000") {
		t.Fatalf("except 应保留其余行:\n%s", res.Output)
	}
}

func TestPipeCount(t *testing.T) {
	x := pipeSetup(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | count")
	if !strings.Contains(res.Output, "计数:") {
		t.Fatalf("count 应输出计数:\n%s", res.Output)
	}
	// 组合：match | count
	res = x.Execute("admin", aaaClassSU, "ssh", "show configuration | match mtu | count")
	if !strings.Contains(res.Output, "计数: 1") {
		t.Fatalf("match|count 组合应计 1:\n%s", res.Output)
	}
}

func TestPipeLast(t *testing.T) {
	x := pipeSetup(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | last 2")
	lines := strings.Split(strings.TrimRight(res.Output, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("last 2 应只余两行:\n%s", res.Output)
	}
}

func TestPipeBegin(t *testing.T) {
	x := pipeSetup(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | begin virtual-switches")
	if strings.Contains(res.Output, "hostname pipe-node") || !strings.Contains(res.Output, "vs-app") {
		t.Fatalf("begin 应从首个命中行输出到末尾:\n%s", res.Output)
	}
}

func TestPipeDisplayJSON(t *testing.T) {
	x := pipeSetup(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display json")
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Output), &m); err != nil {
		t.Fatalf("display json 输出应可解析: %v\n%s", err, res.Output)
	}
	if _, ok := m["system"]; !ok {
		t.Fatalf("json 应含 system:\n%s", res.Output)
	}

	// 配置模式子树 display json
	x.Execute("admin", aaaClassSU, "ssh", "configure")
	res = x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens2f0 | display json")
	m = nil
	if err := json.Unmarshal([]byte(res.Output), &m); err != nil {
		t.Fatalf("子树 display json 应可解析: %v\n%s", err, res.Output)
	}
}

func TestPipeDisplayXML(t *testing.T) {
	x := pipeSetup(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display xml")
	if !strings.Contains(res.Output, "<configuration>") || !strings.Contains(res.Output, "<hostname>pipe-node</hostname>") {
		t.Fatalf("display xml 渲染不符:\n%s", res.Output)
	}
}

func TestPipeErrorsAndRegression(t *testing.T) {
	x := pipeSetup(t)
	// 未知管道 / 非法正则
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | bogus")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("未知管道应报错:\n%s", res.Output)
	}
	res = x.Execute("admin", aaaClassSU, "ssh", "show configuration | match [bad")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("非法正则应报错:\n%s", res.Output)
	}
	// 无管道回归：行为不变
	res = x.Execute("admin", aaaClassSU, "ssh", "show configuration")
	if !strings.Contains(res.Output, "hostname pipe-node") || !strings.Contains(res.Output, "vs-app") {
		t.Fatalf("无管道输出回归:\n%s", res.Output)
	}
	// display 不支持命令
	res = x.Execute("admin", aaaClassSU, "ssh", "show version | display json")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("version 无结构化快照应报错:\n%s", res.Output)
	}
}

// TestDisplaySetPipeEndToEnd（决策 #155）：配置模式 edit 层级起步的
// `show | display set` 走真实 Execute 路径——语句带绝对路径前缀，可直接回放。
func TestDisplaySetPipeEndToEnd(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens2f0 mtu 9000",
		"set interfaces ens2f0 description to-TOR",
		"commit", "exit",
	)
	// 操作模式：show configuration | display set
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("display set 不应报错:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "set interfaces ens2f0 mtu 9000") {
		t.Fatalf("应反推出 mtu 语句:\n%s", res.Output)
	}
	// 配置模式：edit 层级起步，语句仍带绝对路径
	run(t, x, "admin", aaaClassSU, "ssh", "configure", "edit interfaces ens2f0")
	res = x.Execute("admin", aaaClassSU, "ssh", "show | display set")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("层级 display set 不应报错:\n%s", res.Output)
	}
	if !strings.HasPrefix(strings.TrimSpace(res.Output), "set interfaces ens2f0") {
		t.Fatalf("层级反推的语句应带绝对路径前缀:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "description to-TOR") {
		t.Fatalf("层级反推应含描述语句:\n%s", res.Output)
	}
	// 运行态 show 无配置快照：display set 如实报错
	res = x.Execute("admin", aaaClassSU, "ssh", "show version | display set")
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("运行态命令的 display set 应报错:\n%s", res.Output)
	}
}

// TestDisplaySetPassthroughBaseError（round81 真机实测回归）：命令本身已报错时，
// display 管道不得用「该命令不支持」盖住真因。
func TestDisplaySetPassthroughBaseError(t *testing.T) {
	x, _ := newCLIKit(t)
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration / | display set")
	if !strings.Contains(res.Output, "无效命令: show configuration /") {
		t.Fatalf("应透传命令自身的无效命令错误:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "该命令不支持 display set") {
		t.Fatalf("管道提示不得掩盖命令真因:\n%s", res.Output)
	}
}
