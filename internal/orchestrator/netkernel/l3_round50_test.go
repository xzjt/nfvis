package netkernel

// round50（只读评审，决策 #519）：内核数据面 DeleteVRF 不得删除内核自动生成的链路本地地址。
//
// 缺陷形态（与 l2.go 的 bridgeAddresses 同族）：同族网关撤销路径已显式排除 `scope=link`
// （注释写明删掉不会自动重生、会静默打瘸该口的 v6 邻居发现），而 DeleteVRF 走的
// deviceAddresses 只过滤空 local、不看 scope——删 L3 交换机（清成员口地址）时会把成员口的
// `fe80::`（IPv6 链路本地）与 `169.254.0.0/16`（IPv4 链路本地）一并下发删除。
//
// 本文件钉住：scope=link（大小写不敏感）不删、声明/显式地址照常删；宽容语义（读失败/解析失败
// 返回 nil，不误删）不变。

import (
	"context"
	"testing"
)

// 链路本地地址（scope=link）绝不能被 DeleteVRF 当成「成员口上的旧地址」删掉。
func TestKernelDeleteVRFKeepsLinkLocalAddresses(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j link show master vs-l3", out: `[{"ifname":"ens192.100"}]`},
		{prefix: "ip -j addr show dev ens192.100", out: `[{"addr_info":[` +
			`{"local":"10.0.0.1","prefixlen":24,"scope":"global"},` +
			`{"local":"fe80::c4a:11ff:fe2d:1a3f","prefixlen":64,"scope":"link"},` +
			`{"local":"169.254.10.20","prefixlen":16,"scope":"link"},` +
			`{"local":"fe80::1","prefixlen":64,"scope":"LINK"}]}]`},
		{prefix: "ip -d -j link show dev ens192.100", out: `[{"ifname":"ens192.100","master":"vs-l3",` +
			`"linkinfo":{"info_kind":"vlan","info_slave_kind":"vrf"}}]`},
	}}
	p := New(f)
	if err := p.DeleteVRF(context.Background(), "vs-l3"); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip addr del 10.0.0.1/24 dev ens192.100") {
		t.Fatalf("声明/显式地址仍须清理；实际：\n%s", f.joined())
	}
	for _, linkLocal := range []string{
		"fe80::c4a:11ff:fe2d:1a3f/64",
		"169.254.10.20/16",
		"fe80::1/64", // scope 大小写不敏感（"LINK" 同样排除）
	} {
		if f.has("ip addr del " + linkLocal) {
			t.Fatalf("链路本地地址（scope=link）不得删除：%s；实际：\n%s", linkLocal, f.joined())
		}
	}
}

// 回归：非 scope=link 的地址（含**无 scope 字段**的老版 iproute2 输出、显式 scope=global/host）
// 照常清理——本次过滤不得把正常地址一并漏掉。
func TestKernelDeleteVRFStillRemovesDeclaredAddresses(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j link show master vs-l3", out: `[{"ifname":"ens192.100"}]`},
		{prefix: "ip -j addr show dev ens192.100", out: `[{"addr_info":[` +
			`{"local":"10.0.0.1","prefixlen":24},` + // 老版输出：无 scope 字段
			`{"local":"10.0.0.2","prefixlen":24,"scope":"global"},` +
			`{"local":"2001:db8::1","prefixlen":128,"scope":"host"}]}]`},
		{prefix: "ip -d -j link show dev ens192.100", out: `[{"ifname":"ens192.100","master":"vs-l3",` +
			`"linkinfo":{"info_kind":"vlan","info_slave_kind":"vrf"}}]`},
	}}
	p := New(f)
	if err := p.DeleteVRF(context.Background(), "vs-l3"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip addr del 10.0.0.1/24 dev ens192.100",
		"ip addr del 10.0.0.2/24 dev ens192.100",
		"ip addr del 2001:db8::1/128 dev ens192.100",
	} {
		if !f.has(want) {
			t.Fatalf("非 scope=link 的地址应清理：%q；实际：\n%s", want, f.joined())
		}
	}
}
