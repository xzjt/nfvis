package network

// R88-2 守护：VRF 内静态路由的下一跳必须**在路由所属表里**解析。
//
// 真机 round88 现场（1.1.49）：vs-wan（表 9726253）里 `10.0.0.0/8 via 192.168.155.2`
// 下发成功、`show vrfs vs-wan routes` 也读得到，但 VPP FIB 里该路由恒为 dpo-drop：
//   `show ip fib index 2 10.0.0.0/8` → path: recursive: via 192.168.155.2 **in fib:0**
// 即下一跳被拿到默认表去解析（FibPath.table_id 缺省 0 = 默认表），而 192.168.155.2
// 的邻居只在 vs-wan 表里（arping 后 `show ip fib index 2 192.168.155.2/32` 是 resolved
// adjacency，默认表里始终是 drop）。报文因此全丢，而控制面（IPRouteDump）一切正常——
// 「命令成功但答非所问」。修法：构造 FibPath 时填 TableID = 路由所属表。

import (
	"testing"

	"go.fd.io/govpp/binapi/fib_types"
)

func TestRecursiveNextHopPathsResolveInOwnTable(t *testing.T) {
	const tableID = uint32(9726253) // 真机现场里 vs-wan 的表号
	paths, err := recursiveNextHopPaths(tableID, "192.168.155.2")
	if err != nil {
		t.Fatalf("构造 FIB path: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("路径数 = %d，期望 1", len(paths))
	}
	p := paths[0]
	if p.TableID != tableID {
		t.Errorf("FibPath.TableID = %d，期望 %d（缺省 0 会让下一跳去默认表解析，路由恒 drop）", p.TableID, tableID)
	}
	if p.Proto != fib_types.FIB_API_PATH_NH_PROTO_IP4 {
		t.Errorf("Proto = %v，期望 IPv4", p.Proto)
	}
	if got := p.Nh.Address.GetIP4().String(); got != "192.168.155.2" {
		t.Errorf("下一跳 = %s，期望 192.168.155.2", got)
	}
	// 递归路径：出接口为 ~0（由下一跳解析决定）。
	if p.SwIfIndex != ^uint32(0) {
		t.Errorf("SwIfIndex = %d，期望 ^uint32(0)（经下一跳递归解析）", p.SwIfIndex)
	}
}

func TestRecursiveNextHopPathsIPv6(t *testing.T) {
	paths, err := recursiveNextHopPaths(42, "2001:db8::1")
	if err != nil {
		t.Fatalf("构造 FIB path: %v", err)
	}
	if paths[0].Proto != fib_types.FIB_API_PATH_NH_PROTO_IP6 {
		t.Errorf("Proto = %v，期望 IPv6", paths[0].Proto)
	}
	if paths[0].TableID != 42 {
		t.Errorf("TableID = %d，期望 42", paths[0].TableID)
	}
}

func TestRecursiveNextHopPathsRejectsBadAddress(t *testing.T) {
	if _, err := recursiveNextHopPaths(1, "不是地址"); err == nil {
		t.Fatal("非法下一跳必须报错")
	}
}

// 决策 #381：静态路由多下一跳（ECMP）——nextHopPaths 按逗号切分构造 N 条递归路径。
//
// 单值 ⇒ N=1（与既有行为一致）；多值 ⇒ N 条、各自 Weight=1 等权、TableID 同表、
// SwIfIndex=~0（递归解析）——TableID 必填是 round88 的教训，多路径同样不能省。
func TestNextHopPathsECMP(t *testing.T) {
	const tableID = uint32(9726253)

	// 单值：N=1。
	one, err := nextHopPaths(tableID, "192.168.155.2")
	if err != nil {
		t.Fatalf("单值构造: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("单值应构造 1 条路径，得 %d", len(one))
	}
	if one[0].Weight != 1 {
		t.Errorf("单值 Weight = %d，期望 1", one[0].Weight)
	}
	if one[0].TableID != tableID {
		t.Errorf("单值 TableID = %d，期望 %d", one[0].TableID, tableID)
	}

	// 多值：N=2，逐条断言。
	two, err := nextHopPaths(tableID, "192.168.155.2,192.168.155.3")
	if err != nil {
		t.Fatalf("多值构造: %v", err)
	}
	if len(two) != 2 {
		t.Fatalf("两条下一跳应构造 2 条路径，得 %d", len(two))
	}
	want := []string{"192.168.155.2", "192.168.155.3"}
	for i, p := range two {
		if p.Weight != 1 {
			t.Errorf("路径 %d Weight = %d，期望 1（等权 ECMP）", i, p.Weight)
		}
		if p.TableID != tableID {
			t.Errorf("路径 %d TableID = %d，期望 %d（须在路由所属表内解析）", i, p.TableID, tableID)
		}
		if p.SwIfIndex != ^uint32(0) {
			t.Errorf("路径 %d SwIfIndex = %d，期望 ^uint32(0)（经下一跳递归解析）", i, p.SwIfIndex)
		}
		if p.Proto != fib_types.FIB_API_PATH_NH_PROTO_IP4 {
			t.Errorf("路径 %d Proto = %v，期望 IPv4", i, p.Proto)
		}
		if got := p.Nh.Address.GetIP4().String(); got != want[i] {
			t.Errorf("路径 %d 下一跳 = %s，期望 %s", i, got, want[i])
		}
	}
}

func TestNextHopPathsRejectsBadElement(t *testing.T) {
	if _, err := nextHopPaths(1, "10.0.0.1,不是地址"); err == nil {
		t.Fatal("多下一跳中任一非法即须报错")
	}
}
