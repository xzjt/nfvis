package api

// round51 评审 H3：`delete` 落到「建元素」的别名路径上会把元素**建出来**（delete 变 add）。
//
// 三个别名共用 elemByField（不存在则**创建**）取元素，delete 分支再删一个本就不存在的键——
// removeStr/delete 对缺失键是空操作 ⇒ 表达式返回 nil ⇒ commitTree 看到「多出一个新元素」
// 报成功。l3-interface 一族尤其致命：条目被建出后，下一次提交会把该物理口搬进该 VRF 的 IP
// 表（`resolveL3Iface` 按接口清单驱动），即一条「删一条不存在的地址」的语句改变了数据面。
// static-routes 一族同族：凭空建出只有 prefix、没有 next-hop 的路由条目，提交被
// `checkRouteNextHops`（下一跳必须是有效 ip）拒绝，操作者还要先找到这条幽灵路由才能继续。
//
// 判据：delete 必须**不创建**、条目不存在时如实报「无匹配配置: <身份取值>」
//（与同级 aliasL3InterfaceDel / aliasStaticRoute 同口径）。set 分支语义不动。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// r51DelStmt 执行一条 delete 语句（fixture 用）。
func r51DelStmt(cfg *model.Config, line string) error {
	return deleteStatement(cfg, splitFieldsQuoted(line)[1:])
}

// r51SetStmt 执行一条 set 语句（fixture 用）。
func r51SetStmt(cfg *model.Config, line string) error {
	return applyStatement(cfg, splitFieldsQuoted(line)[1:])
}

// r51MustSet fixture 语句失败即 Fatal。
func r51MustSet(t *testing.T, cfg *model.Config, line string) {
	t.Helper()
	if err := r51SetStmt(cfg, line); err != nil {
		t.Fatalf("fixture 语句 %q 失败（fixture 本身必须合法）: %v", line, err)
	}
}

// TestFixR51DeleteL3InterfaceAddrDoesNotCreate：条目不存在时 `delete … ip address` 必须报
// 「无匹配配置」且**不得建出条目**（修复前：静默建出 {interface} 条目并报成功）。
func TestFixR51DeleteL3InterfaceAddrDoesNotCreate(t *testing.T) {
	var cfg model.Config
	r51MustSet(t, &cfg, "set virtual-switches vs-l3 type l3")

	err := r51DelStmt(&cfg, "delete virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24")
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 0 {
		t.Fatalf("delete 不得凭空建出 l3-interface 条目（否则提交会改变数据面）: %+v", cfg.Vrfs)
	}
	if err == nil || !strings.Contains(err.Error(), "无匹配配置") {
		t.Fatalf("条目不存在时删地址应报「无匹配配置」，实际: %v", err)
	}

	// 正控：条目存在时删地址照旧生效（只删地址叶、保留条目——#472 的残形态）
	r51MustSet(t, &cfg, "set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24")
	r51MustSet(t, &cfg, "set virtual-switches vs-l3 l3-interface ens192 ip address fd00::1/64")
	if err := r51DelStmt(&cfg, "delete virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24"); err != nil {
		t.Fatalf("条目存在时删地址应成功: %v", err)
	}
	li := cfg.Vrfs[0].L3Interfaces[0]
	if len(li.Addresses) != 1 || li.Addresses[0] != "fd00::1/64" {
		t.Fatalf("删除只应摘掉指定地址: %+v", li)
	}
}

// TestFixR51DeleteL3InterfaceAclInDoesNotCreate：ac-in 一族同判（修复前同样建条目）。
func TestFixR51DeleteL3InterfaceAclInDoesNotCreate(t *testing.T) {
	var cfg model.Config
	r51MustSet(t, &cfg, "set virtual-switches vs-l3 type l3")

	err := r51DelStmt(&cfg, "delete virtual-switches vs-l3 l3-interface ens192 acl-in acl-a")
	if err == nil || !strings.Contains(err.Error(), "无匹配配置") {
		t.Fatalf("条目不存在时解绑入向 ACL 应报「无匹配配置」，实际: %v", err)
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 0 {
		t.Fatalf("delete 不得凭空建出 l3-interface 条目: %+v", cfg.Vrfs)
	}

	// 正控：条目存在时解绑照旧生效（只清 acl_in 叶）
	r51MustSet(t, &cfg, "set virtual-switches vs-l3 l3-interface ens192 acl-in acl-a")
	if err := r51DelStmt(&cfg, "delete virtual-switches vs-l3 l3-interface ens192 acl-in acl-a"); err != nil {
		t.Fatalf("条目存在时解绑应成功: %v", err)
	}
	if cfg.Vrfs[0].L3Interfaces[0].AclIn != "" {
		t.Fatalf("解绑后 acl_in 应已清空: %+v", cfg.Vrfs[0].L3Interfaces[0])
	}
}

// TestFixR51DeleteStaticRouteDistanceDoesNotCreate：路由 distance 一族同判——
// 修复前会建出「只有 prefix、没有 next-hop」的幽灵路由条目并报成功。
func TestFixR51DeleteStaticRouteDistanceDoesNotCreate(t *testing.T) {
	var cfg model.Config
	r51MustSet(t, &cfg, "set virtual-switches vs-l3 type l3")

	err := r51DelStmt(&cfg, "delete virtual-switches vs-l3 static-routes 10.0.0.0/8 distance 5")
	if err == nil || !strings.Contains(err.Error(), "无匹配配置") {
		t.Fatalf("路由不存在时删 distance 应报「无匹配配置」，实际: %v", err)
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].Routes) != 0 {
		t.Fatalf("delete 不得凭空建出静态路由条目（幽灵路由提交必被校验拒）: %+v", cfg.Vrfs)
	}

	// 正控：路由存在时删 distance 照旧生效（只清 distance 叶，下一跳保留）
	r51MustSet(t, &cfg, "set virtual-switches vs-l3 static-routes 10.0.0.0/8 next-hop 10.1.0.2 distance 5")
	if err := r51DelStmt(&cfg, "delete virtual-switches vs-l3 static-routes 10.0.0.0/8 distance 5"); err != nil {
		t.Fatalf("路由存在时删 distance 应成功: %v", err)
	}
	rt := cfg.Vrfs[0].Routes[0]
	if rt.NextHop != "10.1.0.2" || rt.Distance != 0 {
		t.Fatalf("删除只应摘掉 distance: %+v", rt)
	}
}
