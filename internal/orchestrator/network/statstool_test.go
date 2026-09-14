package network

// D-1 第一层（决策 #68）：vpp_get_stats dump machine 解析单测（纯函数，跨平台）。
// 样例取自 nfvis-vm 实测输出。

import (
	"testing"
)

// 实测输出（VPP 26.06，含组合计数的 /err 行与真实 buffer 池行）。
const sampleMachineDump = `9:0.00:/buffer-pools/default-numa-0/cached
9:0.00:/buffer-pools/default-numa-0/used
9:430184.00:/buffer-pools/default-numa-0/available
2:0:0:0:/err/wg6-output-tun/No buffers
2:0:1:0:/err/wg6-output-tun/No buffers
9:1.00:/buffer-pools/default-numa-1/cached
9:2048.50:/buffer-pools/default-numa-1/used
9:998.25:/buffer-pools/default-numa-1/available
`

func TestParseStatsMachine(t *testing.T) {
	got := ParseStatsMachine(sampleMachineDump)

	// 标量行（<type>:<value>:<path>）
	if got["/buffer-pools/default-numa-0/available"] != 430184 {
		t.Fatalf("available 未解析: %v", got["/buffer-pools/default-numa-0/available"])
	}
	if got["/buffer-pools/default-numa-1/used"] != 2048.5 {
		t.Fatalf("小数未解析: %v", got["/buffer-pools/default-numa-1/used"])
	}
	// 组合计数行（2:0:0:0:/err/...）字段数不符，不应出现
	for k := range got {
		if len(k) >= 4 && k[:4] == "/err" {
			t.Fatalf("组合计数行不应被解析为标量: %q", k)
		}
	}
}

func TestParseStatsMachineMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"空输入", ""},
		{"仅空行", "\n\n  \n"},
		{"缺 path", "9:1.00"},
		{"type 非数字", "x:1.00:/buffer-pools/p/used"},
		{"value 非数字", "9:abc:/buffer-pools/p/used"},
		{"path 非绝对", "9:1.00:buffer-pools/p/used"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseStatsMachine(c.in); len(got) != 0 {
				t.Fatalf("应无解析结果，得到 %v", got)
			}
		})
	}
}

func TestBufferPoolsFromDump(t *testing.T) {
	pools, ok := BufferPoolsFromDump(sampleMachineDump)
	if !ok {
		t.Fatal("应解析出 buffer 池")
	}
	if len(pools) != 2 {
		t.Fatalf("池数 = %d，want 2（%+v）", len(pools), pools)
	}
	// 按名排序：default-numa-0 在前
	p0 := pools[0]
	if p0.Name != "default-numa-0" || p0.Used != 0 || p0.Available != 430184 || p0.Cached != 0 {
		t.Fatalf("池 0 字段错误: %+v", p0)
	}
	p1 := pools[1]
	if p1.Name != "default-numa-1" || p1.Used != 2048.5 || p1.Available != 998.25 || p1.Cached != 1 {
		t.Fatalf("池 1 字段错误: %+v", p1)
	}
}

// 全零也是合法观测值（空载 VPP 的 used/cached 即为 0），不得被当作"未解析"丢弃。
func TestBufferPoolsKeepsAllZeroPool(t *testing.T) {
	pools, ok := BufferPoolsFromDump("9:0.00:/buffer-pools/p0/used\n9:0.00:/buffer-pools/p0/available\n")
	if !ok || len(pools) != 1 {
		t.Fatalf("全零池应保留: ok=%v pools=%+v", ok, pools)
	}
	if pools[0].Name != "p0" || pools[0].Available != 0 {
		t.Fatalf("字段错误: %+v", pools[0])
	}
}

func TestBufferPoolsFromDumpNoPools(t *testing.T) {
	for _, in := range []string{"", "2:0:0:0:/err/foo/bar\n", "9:1.00:/memstat/total\n"} {
		if pools, ok := BufferPoolsFromDump(in); ok || pools != nil {
			t.Fatalf("输入 %q 应无 buffer 池，得到 %+v", in, pools)
		}
	}
}
