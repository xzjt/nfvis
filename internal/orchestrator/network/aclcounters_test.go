package network

// 决策 #339：ACL 逐规则命中计数解析与映射单测（纯函数 + 假 StatsTool，跨平台）。

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fakeStatsTool 固定返回一段 dump 文本（或错误）。
type fakeStatsTool struct {
	out string
	err error
}

func (f fakeStatsTool) DumpMachine(context.Context, string) (string, error) { return f.out, f.err }

// 逐线程组合计数样例：type 3 = COUNTER_VECTOR_COMBINED，
// `3:<规则下标>:<线程索引>:<packets>:<bytes>:/acl/<acl-index>/matches`
// （字段顺序/语义依据见 aclcounters.go 顶部对 VPP 源码的引用）。
// 2 条规则 × 3 条线程 = 6 行；tags 分别是 packets:bytes。
const sampleACLDump = `3:0:0:5:500:/acl/7/matches
3:0:1:7:700:/acl/7/matches
3:0:2:1:100:/acl/7/matches
3:1:0:2:200:/acl/7/matches
3:1:1:0:0:/acl/7/matches
3:1:2:3:300:/acl/7/matches
3:0:0:11:1100:/acl/9/matches
2:0:0:0:/err/acl-plugin-ip4/foo
9:430184.00:/buffer-pools/default-numa-0/available
`

func TestParseACLCounters(t *testing.T) {
	got := ParseACLCounters(sampleACLDump)

	if len(got) != 2 {
		t.Fatalf("应解析出 2 个 ACL，得到 %v", got)
	}
	acl7 := got[7]
	if len(acl7) != 2 {
		t.Fatalf("ACL 7 应有 2 条规则，得到 %v", acl7)
	}
	// 跨 3 线程求和：规则 0 = 5+7+1，规则 1 = 2+0+3
	if acl7[0] != 13 {
		t.Errorf("规则 0 命中 = %d，want 13（跨线程求和）", acl7[0])
	}
	if acl7[1] != 5 {
		t.Errorf("规则 1 命中 = %d，want 5（跨线程求和）", acl7[1])
	}
	if got[9][0] != 11 {
		t.Errorf("ACL 9 规则 0 命中 = %d，want 11", got[9][0])
	}
	// 非 /acl/ 路径的行不得混入
	if _, ok := got[0]; ok {
		t.Errorf("不应有 acl 0（/err 与 buffer 行不得混入）: %v", got)
	}
}

// packets 与 bytes 是两个独立量：命中只取 packets，不得把 bytes 并进来。
func TestParseACLCountersUsesPacketsNotBytes(t *testing.T) {
	got := ParseACLCounters("3:0:0:5:999999:/acl/1/matches\n")
	if got[1][0] != 5 {
		t.Fatalf("命中应取 packets=5（不是 bytes 999999），得到 %d", got[1][0])
	}
}

// 汇总行（vpp_get_stats -s）：`3:<规则>:<packets>:<bytes>:<path>`（无线程维）。
func TestParseACLCountersAggregatedForm(t *testing.T) {
	got := ParseACLCounters("3:0:13:1300:/acl/7/matches\n3:1:5:500:/acl/7/matches\n")
	if got[7][0] != 13 || got[7][1] != 5 {
		t.Fatalf("汇总行解析错误: %v", got)
	}
}

func TestParseACLCountersMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"空输入", ""},
		{"仅空行", "\n\n  \n"},
		{"非 acl 路径", "3:0:0:5:500:/acl-plugin/x/matches\n"},
		{"缺 matches 后缀", "3:0:0:5:500:/acl/7/counters\n"},
		{"acl 索引非数字", "3:0:0:5:500:/acl/x/matches\n"},
		{"acl 索引带多余段", "3:0:0:5:500:/acl/7/8/matches\n"},
		{"字段数不符", "3:0:5:/acl/7/matches\n"},
		{"规则非数字", "3:x:0:5:500:/acl/7/matches\n"},
		{"packets 非数字", "3:0:0:x:500:/acl/7/matches\n"},
		{"类型非数字", "x:0:0:5:500:/acl/7/matches\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseACLCounters(c.in); len(got) != 0 {
				t.Fatalf("应无解析结果，得到 %v", got)
			}
		})
	}
}

// Manager.ACLHitCounters：无回退源/工具报错均如实上抛，不吞、不当作零命中。
func TestManagerACLHitCounters(t *testing.T) {
	m := &Manager{statsTool: fakeStatsTool{out: sampleACLDump}}
	raw, err := m.ACLHitCounters(context.Background())
	if err != nil {
		t.Fatalf("取数失败: %v", err)
	}
	if raw[7][0] != 13 {
		t.Fatalf("原始命中错误: %v", raw)
	}

	none := &Manager{}
	if _, err := none.ACLHitCounters(context.Background()); err == nil {
		t.Fatal("无回退源应报错（不吞）")
	}

	boom := &Manager{statsTool: fakeStatsTool{err: errors.New("无法连接 stats segment")}}
	if _, err := boom.ACLHitCounters(context.Background()); err == nil {
		t.Fatal("工具报错应上抛（不吞）")
	}
}

// ACLCounters：VPP acl-index → 产品 ACL 名（来源是 acl.go 的下发登记，不新造事实）；
// 未登记的 index（残渣）不呈现；取数失败如实上抛。
func TestACLCountersMapping(t *testing.T) {
	fake := newFakeAcl()
	prov := NewAclProvider(fake)
	for _, name := range []string{"acl-web", "acl-db"} {
		if err := prov.ApplyACL(context.Background(), model.Acl{Name: name,
			Rules: []model.AclRule{{Seq: 10, Action: "permit"}}}); err != nil {
			t.Fatalf("下发 %s: %v", name, err)
		}
	}
	idx := prov.ACLIndexes()
	dump := ""
	for _, name := range []string{"acl-web", "acl-db"} {
		dump += "3:0:0:5:500:/acl/" + strconv.Itoa(int(idx[name])) + "/matches\n"
	}
	// 一个未登记的 index（模拟残渣）：不应出现在结果里
	dump += "3:0:0:9:900:/acl/4242/matches\n"

	c := NewACLCounters(&Manager{statsTool: fakeStatsTool{out: dump}}, prov)
	got, err := c.ACLHitCounters(context.Background())
	if err != nil {
		t.Fatalf("映射失败: %v", err)
	}
	if len(got) != 2 || got["acl-web"][0] != 5 || got["acl-db"][0] != 5 {
		t.Fatalf("映射结果错误: %v", got)
	}

	// 取数失败必须上抛（调用方呈现原因，不静默当零命中）
	bad := NewACLCounters(&Manager{statsTool: fakeStatsTool{err: errors.New("boom")}}, prov)
	if _, err := bad.ACLHitCounters(context.Background()); err == nil {
		t.Fatal("取数失败应上抛")
	}
	// 未接入 VPP 的映射器也如实报错
	var nilC *ACLCounters
	if _, err := nilC.ACLHitCounters(context.Background()); err == nil {
		t.Fatal("未接入应报错")
	}
}
