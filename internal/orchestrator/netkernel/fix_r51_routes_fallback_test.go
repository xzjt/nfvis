package netkernel

// round51 P3：内核数据面 `Routes` 读视图把产品自建的域兜底路由当成配置的静态路由列出来。
//
// 缺陷形态（静态评审定位）：`ensureVRFFallback`（决策 #446）在每个转发域 VRF 表内自建两条
// `unreachable default metric 4278198272`（v4/v6 各一），而 `Runtime.Routes` 把 `ip route` 的
// 每一行都映射成 RouteRow（它本来就读了 row.Type，却没用）。于是
// `show virtual-switches <l3> routes` / `GET /vrfs/{名}/routes` 会多出一条 next-hop 为空、
// distance 4278198272 的 "default"（操作者从没配过它），而**同一份配置**在 VPP 数据面的读视图
// 里没有这一条（VPP 侧无此产物）——两个数据面对同一份配置给出不同的「已配置路由」清单。
//
// 判据取 `type == "unreachable"`（与 l3.go 的 vrfFallbackPresent 同一判别式）；配置模型里没有
// 「类型」字段（Route 只有 prefix/next_hop/distance，routeArgs 也从不发类型关键字），故不存在
// 「操作者声明的 unreachable 路由」被误杀的情形。

import (
	"context"
	"testing"
)

func TestKernelRoutesHidesProductFallback(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -j route show table ",
		out: `[{"dst":"10.9.0.0/24","gateway":"10.0.0.9","metric":10},` +
			`{"type":"unreachable","dst":"default","metric":4278198272}]`,
	}}}
	rt := NewRuntime(f)
	rows, err := rt.Routes(context.Background(), "vs-l3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Prefix != "10.9.0.0/24" {
		t.Fatalf("读视图只应给出配置过的静态路由，实际：%+v", rows)
	}
	for _, r := range rows {
		if r.Prefix == "default" || r.Distance == 4278198272 {
			t.Fatalf("产品自建的域兜底路由（unreachable default）不得混进「已配置路由」：%+v", r)
		}
	}
}

// 兜底与真实静态路由同前缀时同样只留配置的那条（不能因为「default 被兜底占了」而把操作者
// 声明的默认路由一起丢掉）。
func TestKernelRoutesKeepsConfiguredDefaultRoute(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -j route show table ",
		out: `[{"dst":"default","gateway":"10.0.0.254","metric":10},` +
			`{"type":"unreachable","dst":"default","metric":4278198272}]`,
	}}}
	rt := NewRuntime(f)
	rows, err := rt.Routes(context.Background(), "vs-l3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Prefix != "default" || rows[0].NextHop != "10.0.0.254" || rows[0].Distance != 10 {
		t.Fatalf("操作者声明的默认路由必须保留且只有一条，实际：%+v", rows)
	}
}
