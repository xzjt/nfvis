package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// cli_mapping_test.go —— CLI 语句↔模型映射守护。
//
// 由来（docs/reviews/2026-09-13.md 第三轮）：契约 `docs/NFViS-CLI命令树完整设计.md`
// §2.4/§2.5/§2.9 声明的网络语句，在 M3 交付时大部分**无法经 CLI 落进模型**
// （数组/嵌套语句缺 alias 映射）；已有守护只覆盖「API↔契约」（routes_contract_test）
// 与「命令树⇄模型字段」（internal/schema/gen_test），唯独没有「语句能否真正落地」。
// 本测试对契约语句逐条断言：configure → set → candidate 必须发生变化且无 %%；
// 任何未映射的语句都会在这里失败（CLI 版 route guard）。

// contractStatements 契约中网络/协议类语句的代表样本（FR-NET-010~018、FR-SYS-008）。
var contractStatements = []string{
	// L2 / L3 虚拟交换机（§2.4）
	"set virtual-switches vs-a type l2",
	"set virtual-switches vs-a vlan access 100",
	"set virtual-switches vs-a ports 1 interface ens224",
	"set virtual-switches vs-a ports 1 interface ens224 trunk vlans 100,200",
	"set virtual-switches vs-a gateway ip 192.168.100.1/24",
	"set virtual-switches vs-a gateway vrf vr-a",
	"set virtual-switches vs-a gateway acl-in acl-a",
	"set virtual-switches vs-l3 type l3",
	"set virtual-switches vs-l3 l3-interface ens192 ip address 192.168.155.200/24",
	"set virtual-switches vs-l3 static-routes 0.0.0.0/0 next-hop 192.168.155.2",
	"set virtual-switches vs-l3 static-routes 10.0.0.0/8 next-hop 192.168.155.3 distance 5",
	"set virtual-switches vs-l3 l3-interface ens192 acl-in acl-a",
	// ACL（§2.5）
	"set acls acl-a rule 10 source 192.168.155.0/24 destination any protocol icmp action deny",
	"set acls acl-a rule 10 direction ingress",
	"set acls acl-a rule 20 source any destination 10.0.0.0/8 protocol tcp source-port 1024-65535 action permit",
	// NAT（§2.5）
	"set nat source-pool pool-a address-range 192.168.155.220 to 192.168.155.225",
	"set nat rules 10 match source 10.10.0.0/24 virtual-switch vs-l3 action source-pool pool-a",
	"set nat static 10.10.0.10 to 192.168.155.230",
	// SPAN / QoS（§2.5）
	"set port-mirroring span-a source interface ens224 direction both",
	"set port-mirroring span-a analyzer interface ens192",
	"set qos policies lim-a cir 1000000 cbs 100000",
	"set interfaces ens192 ingress-policy lim-a",
	// LLDP（FR-NET-018）
	"set protocols lldp enable true",
	"set protocols lldp advertisement-interval 30",
	"set protocols lldp interface ens224 enable true",
	// 既有 M2 语义（回归保护）
	"set interfaces ens224 description demo-port",
	"set resource-pools hugepages page-size 1G count 32",
	// M4 计算/容器语句（§2.7/§2.8；M4-12 补齐映射——此前 CLI 声明但写不进模型）
	"set virtual-machine-functions fw-vm image base.qcow2",
	"set virtual-machine-functions fw-vm vcpu count 2 pin true",
	"set virtual-machine-functions fw-vm memory size-mb 1024",
	"set virtual-machine-functions fw-vm memory hugepage-size 1G",
	"set virtual-machine-functions fw-vm memory backing hugepage",
	"set virtual-machine-functions fw-vm interfaces eth0 type vhost-user",
	"set virtual-machine-functions fw-vm interfaces eth0 virtual-switch vs-a",
	"set virtual-machine-functions fw-vm serial console enable",
	"set virtual-machine-functions fw-vm autostart true",
	"set virtual-switches vs-a ports 1 vnf fw-vm interface eth0",
	"set container-functions sbc-ct1 image alpine:3.20",
	"set container-functions sbc-ct1 vcpu count 2",
	"set container-functions sbc-ct1 memory size-mb 256",
	"set container-functions sbc-ct1 interfaces eth0 type memif",
	"set container-functions sbc-ct1 interfaces eth0 virtual-switch vs-a",
	"set virtual-switches vs-a ports 2 container sbc-ct1 interface eth0",
	// 2026-09-14 全功能 CLI 测试补齐（决策 #76）：以下语句**契约已声明但此前落不进模型**，
	// 因未列入本清单而长期漏网——加进来守护。
	// vpp dpdk dev 全局默认（§2.9）：dev 下有 <ifname> 参数子节点，通用遍历会把它当数组容器，
	// 而模型 vpp.dpdk.dev 是对象（VppDevDefault）→ 原先报 cannot unmarshal array into ...
	"set vpp dpdk dev rx-queues 2",
	"set vpp dpdk dev tx-queues 2",
	"set vpp dpdk dev rx-descriptors 1024",
	"set vpp dpdk dev tx-descriptors 1024",
	// vpp dpdk dev 单网卡覆盖（§2.9）
	"set vpp dpdk dev ens224 rx-queues 4",
	// system ntp server（§2.2）：模型是对象数组 system.ntp[{server,prefer}]，
	// 原先既落不到 ntp 键，prefer flag 也无法结尾（报「未知语句」/「缺少取值」）
	"set system ntp server 192.168.155.1",
	"set system ntp server 192.168.155.1 prefer",
	// system dns server <ip> secondary <ip>（§2.2）：通用遍历消费完首个 IP 后下潜到参数节点，
	// 同级关键字 secondary 不可见（报「未知语句: "secondary"」）
	"set system dns server 8.8.8.8 secondary 8.8.4.4",
	// resource-pools cpu numa node <n> cores <list>（§2.6）：模型是对象数组 cpu.numa[{node,cores}]，
	// CLI 多一层 node 关键字 → 原先写成对象（cannot unmarshal object into []model.NumaNode）
	"set resource-pools cpu numa node 0 cores 1-4",
	// 2026-09-15（决策 #79）：待办 §2.4 的 8 处「契约已声明但经 CLI 用不了」——
	// 逐条补入本清单（此前正因为**不在清单里**而长期漏网）。
	"set system api tls cert-file /tmp/a.pem key-file /tmp/a.key",
	"set system login user u1 password Abc12345!x class operator",
	"set system login class c1 allow show",
	"set system login class c1 deny request",
	"set virtual-switches vs-xc type l2",
	"set virtual-switches vs-xc ports 1 interface ens224",
	"set virtual-switches vs-xc ports 2 interface ens192",
	"set virtual-switches vs-xc cross-connect 1 2",
	"set virtual-machine-functions fw-vm interfaces eth0 vlan 100",
	"set virtual-machine-functions fw-vm cloud-init ssh-key \"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEY nfvis@test\"",
	"set container-functions sbc-ct1 interfaces eth0 type memif virtual-switch vs-a",
	"set container-functions sbc-ct1 env TEST_KEY test_value",
}

func TestCLIStatementMappingGuard(t *testing.T) {
	for _, stmt := range contractStatements {
		stmt := stmt
		t.Run(stmt, func(t *testing.T) {
			x, engine := newCLIKit(t)
			// 每条语句独立会话；仅为其补齐必要的前置（交换机类型），
			// 且不在被测语句本身是 type 语句时重复前置。
			pre := []string{"configure"}
			switch {
			case strings.Contains(stmt, "virtual-switches vs-l3") && !strings.Contains(stmt, "type l3"):
				pre = append(pre, "set virtual-switches vs-l3 type l3")
			case strings.Contains(stmt, "virtual-switches vs-a") && !strings.Contains(stmt, "type l2"):
				pre = append(pre, "set virtual-switches vs-a type l2")
			}
			// M4 计算/容器语句：先建实体（与 type 前置同法），否则语句无宿主元素。
			switch {
			case strings.Contains(stmt, "virtual-machine-functions fw-vm") &&
				!strings.Contains(stmt, "set virtual-machine-functions fw-vm image"):
				pre = append(pre, "set virtual-machine-functions fw-vm image base.qcow2")
			case strings.Contains(stmt, "container-functions sbc-ct1") &&
				!strings.Contains(stmt, "set container-functions sbc-ct1 image"):
				pre = append(pre, "set container-functions sbc-ct1 image alpine:3.20")
			}
			// 端口成员语句：先建被引用的 VM/容器实体。
			if strings.Contains(stmt, "ports 1 vnf fw-vm") {
				pre = append(pre, "set virtual-machine-functions fw-vm image base.qcow2")
			}
			if strings.Contains(stmt, "ports 2 container sbc-ct1") {
				pre = append(pre, "set container-functions sbc-ct1 image alpine:3.20")
			}
			// cross-connect 引用的是**已声明的端口序号**（模型只有 cross_connect bool，
			// 端口身份由 ports 列表承担），故须先声明两个端口。
			if strings.Contains(stmt, "cross-connect") {
				pre = append(pre,
					"set virtual-switches vs-xc type l2",
					"set virtual-switches vs-xc ports 1 interface ens224",
					"set virtual-switches vs-xc ports 2 interface ens192",
				)
			}
			for _, s := range pre {
				if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", s); strings.Contains(res.Output, "%%") {
					t.Fatalf("前置语句失败 %q: %s", s, res.Output)
				}
			}
			before, _, err := engine.Candidate()
			if err != nil {
				t.Fatalf("读取 candidate: %v", err)
			}
			res := x.Execute("admin", aaa.ClassSuperUser, "ssh", stmt)
			if strings.Contains(res.Output, "%%") {
				t.Fatalf("契约语句未映射到模型: %s", strings.TrimSpace(res.Output))
			}
			after, _, err := engine.Candidate()
			if err != nil {
				t.Fatalf("读取 candidate: %v", err)
			}
			if model.Diff(before, after) == "" {
				t.Fatalf("语句执行成功但 candidate 未变化（静默丢弃）：%s", stmt)
			}
		})
	}
}

// TestCLIStatementMappingDelete 删除形态同样必须落模型（否则 delete 会静默失败）。
func TestCLIStatementMappingDelete(t *testing.T) {
	cases := []struct {
		setup []string
		del   string
	}{
		{[]string{"set virtual-switches vs-a type l2", "set virtual-switches vs-a gateway ip 10.9.0.1/24"},
			"delete virtual-switches vs-a gateway ip 10.9.0.1/24"},
		{[]string{"set virtual-switches vs-a type l2", "set virtual-switches vs-a gateway ip 10.9.0.1/24"},
			"delete virtual-switches vs-a gateway"},
		{[]string{"set virtual-switches vs-b type l3", "set virtual-switches vs-b l3-interface ens192 ip address 10.1.0.1/24"},
			"delete virtual-switches vs-b l3-interface ens192"},
		{[]string{"set virtual-switches vs-b type l3", "set virtual-switches vs-b static-routes 10.0.0.0/8 next-hop 10.1.0.2"},
			"delete virtual-switches vs-b static-routes 10.0.0.0/8"},
		{[]string{"set acls acl-b rule 10 source any destination any protocol icmp action deny"},
			"delete acls acl-b rule 10"},
		{[]string{"set nat source-pool pool-b address-range 10.0.0.1 to 10.0.0.5"},
			"delete nat source-pool pool-b"},
		{[]string{"set qos policies lim-b cir 1000000 cbs 100000"},
			"delete qos policies lim-b"},
		{[]string{"set interfaces ens192 ingress-policy lim-b"},
			"delete interfaces ens192 ingress-policy"},
		{[]string{"set protocols lldp enable true"},
			"delete protocols lldp enable"},
		// M4 计算/容器删除形态（M4-12：同样必须落模型）
		{[]string{"set virtual-machine-functions fw-vm image base.qcow2",
			"set virtual-machine-functions fw-vm memory size-mb 1024"},
			"delete virtual-machine-functions fw-vm memory size-mb"},
		{[]string{"set virtual-machine-functions fw-vm image base.qcow2",
			"set virtual-machine-functions fw-vm interfaces eth0 type vhost-user",
			"set virtual-machine-functions fw-vm interfaces eth0 virtual-switch vs-a"},
			"delete virtual-machine-functions fw-vm interfaces eth0 virtual-switch"},
		{[]string{"set container-functions sbc-ct1 image alpine:3.20",
			"set container-functions sbc-ct1 memory size-mb 256"},
			"delete container-functions sbc-ct1 memory size-mb"},
		{[]string{"set container-functions sbc-ct1 image alpine:3.20",
			"set container-functions sbc-ct1 vcpu count 2"},
			"delete container-functions sbc-ct1 vcpu count"},
		{[]string{"set virtual-switches vs-a type l2",
			"set virtual-machine-functions fw-vm image base.qcow2",
			"set virtual-switches vs-a ports 1 vnf fw-vm interface eth0"},
			"delete virtual-switches vs-a ports 1 vnf fw-vm"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.del, func(t *testing.T) {
			x, engine := newCLIKit(t)
			lines := append([]string{"configure"}, c.setup...)
			for _, s := range lines {
				if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", s); strings.Contains(res.Output, "%%") {
					t.Fatalf("前置语句失败 %q: %s", s, res.Output)
				}
			}
			before, _, err := engine.Candidate()
			if err != nil {
				t.Fatalf("读取 candidate: %v", err)
			}
			res := x.Execute("admin", aaa.ClassSuperUser, "ssh", c.del)
			if strings.Contains(res.Output, "%%") {
				t.Fatalf("删除语句失败: %s", strings.TrimSpace(res.Output))
			}
			after, _, err := engine.Candidate()
			if err != nil {
				t.Fatalf("读取 candidate: %v", err)
			}
			if model.Diff(before, after) == "" {
				t.Fatalf("删除成功但 candidate 未变化（静默丢弃）：%s", c.del)
			}
		})
	}
}

// TestCLIStatementMappingUnknownStillErrors 真正未建模的语句必须显式报错，
// 不允许“看似成功但模型不认”（宽松插入）——这是本轮缺陷的另一半。
func TestCLIStatementMappingUnknownStillErrors(t *testing.T) {
	x, _ := newCLIKit(t)
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure"); strings.Contains(res.Output, "%%") {
		t.Fatalf("configure 失败: %s", res.Output)
	}
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "set virtual-switches vs-a type l2"); strings.Contains(res.Output, "%%") {
		t.Fatalf("前置 set 失败: %s", res.Output)
	}
	for _, bad := range []string{
		"set virtual-switches vs-a nosuch-field 1",
		"set acls acl-z rule 10 nosuch-field x",
		"set nat rules 10 nosuch-field x",
	} {
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh", bad)
		if !strings.Contains(res.Output, "%%") {
			t.Fatalf("未建模语句应报错，实际输出: %q", strings.TrimSpace(res.Output))
		}
	}
}

// TestCLIL3SwitchCreatesVrf L3 交换机必须同步建立同名 Vrf，否则 commit 校验不通过
// （附录 B 映射；此前 CLI 无任何语句可创建 Vrf 条目，L3 交换机根本无法 commit）。
func TestCLIL3SwitchCreatesVrf(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 192.168.155.200/24",
		"set virtual-switches vs-l3 static-routes 0.0.0.0/0 next-hop 192.168.155.2",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	var found bool
	for _, v := range cfg.Vrfs {
		if v.Name == "vs-l3" {
			found = true
			if len(v.L3Interfaces) != 1 || len(v.L3Interfaces[0].Addresses) != 1 ||
				v.L3Interfaces[0].Addresses[0] != "192.168.155.200/24" {
				t.Fatalf("l3_interfaces 落点异常: %+v", v.L3Interfaces)
			}
			if len(v.Routes) != 1 || v.Routes[0].Prefix != "0.0.0.0/0" || v.Routes[0].NextHop != "192.168.155.2" {
				t.Fatalf("routes 落点异常: %+v", v.Routes)
			}
		}
	}
	if !found {
		t.Fatal("L3 交换机未同步建立同名 Vrf 条目")
	}
	// commit 必须通过（校验要求 l3 交换机有同名 VRF）
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "commit"); strings.Contains(res.Output, "%%") {
		t.Fatalf("L3 交换机 commit 失败: %s", res.Output)
	}
}
