package api

// 真机缺陷：交换机端口的 trunk VLAN 列表**分两步**写不进去。
//
// 一步写法（9 token）有别名规则，可用：
//
//	set virtual-switches vs-lan ports 5 interface ens192 trunk vlans 300
//
// 但先声明成员、再补 trunk（8 token）没有规则，落到通用树遍历：`trunk` 被当成对象容器，
// 写成 `trunk:{vlans:…}`，而模型字段是 TrunkVlans []int —— 整条语句被 JSON 解码拒绝
// （cannot unmarshal object into … ports.trunk of type []int）。分步构建是操作者的自然写法
// （先 interface、后 trunk/native），必须与一步形态产出同一模型。
//
// 本文件钉住四条验收：分步生效、一步不回归、delete 仍可用、端口不存在时明确报错且不建元素。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// trunkFixture 建好一台 L2 交换机与声明口（后续语句都作用在它上面）。
func trunkFixture(t *testing.T) (*cliExecutor, string) {
	t.Helper()
	x, _ := newCLIKit(t)
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure"); res.Output != "" {
		t.Fatalf("configure: %q", res.Output)
	}
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"set interfaces ens192",
		"set virtual-switches vs-lan type l2")
	return x, "vs-lan"
}

// candidatePorts 取 candidate 里交换机 vs 的静态端口列表（强类型，断言落模型）。
func candidatePorts(t *testing.T, x *cliExecutor, vs string) []model.VSwitchPort {
	t.Helper()
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	for _, sw := range cfg.VirtualSwitches {
		if sw.Name == vs {
			return sw.Ports
		}
	}
	return nil
}

// TestPortTrunkStagedAfterInterface：两步写法必须与一步写法产出同一模型。
func TestPortTrunkStagedAfterInterface(t *testing.T) {
	x, vs := trunkFixture(t)
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 5 interface ens192"); strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("先声明成员口应成功：%s", res.Output)
	}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 5 interface trunk vlans 300")
	if strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("分步补 trunk 应成功（一步形态可用，分步同族）：%s", res.Output)
	}
	ports := candidatePorts(t, x, vs)
	if len(ports) != 1 {
		t.Fatalf("应只有 1 个端口条目，实得 %+v", ports)
	}
	p := ports[0]
	if p.Seq != 5 || p.Interface != "ens192" {
		t.Fatalf("分步补 trunk 不得改动成员口：%+v", p)
	}
	if len(p.TrunkVlans) != 1 || p.TrunkVlans[0] != 300 {
		t.Fatalf("trunk VLAN 列表应为 [300]：%+v", p)
	}
}

// TestPortTrunkMultiVlanStaged：取值为 VLAN 列表（逗号分隔）时同样落成 []int。
func TestPortTrunkMultiVlanStaged(t *testing.T) {
	x, vs := trunkFixture(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 5 interface ens192")
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 5 interface trunk vlans 100,200")
	if strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("分步补多 VLAN trunk 应成功：%s", res.Output)
	}
	ports := candidatePorts(t, x, vs)
	if len(ports) != 1 || len(ports[0].TrunkVlans) != 2 ||
		ports[0].TrunkVlans[0] != 100 || ports[0].TrunkVlans[1] != 200 {
		t.Fatalf("trunk VLAN 列表应为 [100 200]：%+v", ports)
	}
}

// TestPortTrunkOneLineStillWorks：一步写法（9 token）不得回归。
func TestPortTrunkOneLineStillWorks(t *testing.T) {
	x, vs := trunkFixture(t)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 5 interface ens192 trunk vlans 300")
	if strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("一步写法应照旧可用：%s", res.Output)
	}
	ports := candidatePorts(t, x, vs)
	if len(ports) != 1 || ports[0].Interface != "ens192" ||
		len(ports[0].TrunkVlans) != 1 || ports[0].TrunkVlans[0] != 300 {
		t.Fatalf("一步写法结果不符：%+v", ports)
	}
}

// TestPortTrunkStagedDelete：delete 形态（不带成员口）清掉 trunk 列表，成员口保留。
func TestPortTrunkStagedDelete(t *testing.T) {
	x, vs := trunkFixture(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 5 interface ens192 trunk vlans 300")
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"delete virtual-switches vs-lan ports 5 interface trunk vlans 300")
	if strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("delete 分步形态应成功：%s", res.Output)
	}
	ports := candidatePorts(t, x, vs)
	if len(ports) != 1 || ports[0].Interface != "ens192" {
		t.Fatalf("delete trunk 不得删掉端口条目/成员口：%+v", ports)
	}
	if len(ports[0].TrunkVlans) != 0 {
		t.Fatalf("delete 后 trunk 列表应为空：%+v", ports[0])
	}
}

// TestPortTrunkStagedMissingPort：端口条目不存在时给明确报错，且**不**建出无成员口的壳
// （无成员口的端口条目提交必被拒，静默建壳只会把问题推到 commit）。
func TestPortTrunkStagedMissingPort(t *testing.T) {
	x, vs := trunkFixture(t)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 9 interface trunk vlans 300")
	if !strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("端口不存在时应报错：%q", res.Output)
	}
	if !strings.Contains(res.Output, "interface") {
		t.Fatalf("报错应告诉操作者先声明成员口（interface <ifname>）：%q", res.Output)
	}
	if ports := candidatePorts(t, x, vs); len(ports) != 0 {
		t.Fatalf("端口不存在时不得建出条目：%+v", ports)
	}
}
