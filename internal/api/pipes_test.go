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
