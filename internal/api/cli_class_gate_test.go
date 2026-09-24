package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/schema"
)

// 决策 #144：CLI 运行期的 class 判定必须与**命令树声明**一致。
//
// 背景（round65/66 实测出来的缺陷）：命令树把 Su()/Op() 标在**子节点**上
// （`request system reboot`、`request vpp`、`request images delete`…），而 RequiredClass()
// 取"自身与祖先的最大值"——分发器若只把**域节点**（如 `request system`，只继承到 O）交给判定，
// 整棵子树上的 Su 标记在运行期就形同虚设：operator 实测能过 `zeroize`/`reboot`/`software add`/
// `configuration backup`（全部**声明为 super-user**），叠加 `configuration backup to <path>`
// 的目标路径无校验 + nfvisd 以 root 运行 ⇒ operator 可把任意文件覆盖成归档 JSON。
//
// 本文件是这条口径的守护，两层：
//  ① **穷举树**：遍历操作命令树里每个可执行节点，断言「按该路径判定的 class」== 「该节点声明的
//     RequiredClass()」——不是手挑几条命令，而是整棵树（将来给任何子命令加 Op()/Su() 都自动生效）；
//  ② **端到端**：经 `Execute()` 真跑几条代表性命令，断言 operator 被拒（证明**分发器真的用了**
//     这条判定，而不只是助手函数写对了——①只能证明助手对，②才挡得住"分发器退回域节点"）。
//
// 变异验证（手工做过，见提交信息/证据）：把 `requestSystem` 的 `allowTokens` 改回
// `allow`（域节点）→ ②立刻报「operator 不该通过 request system reboot」。

// walkOperCommands 遍历操作树里的可执行命令节点（关键字层 + 实例参数层），
// 把路径（参数位置放占位取值）交给 fn。
func walkOperCommands(n *schema.Node, path []string, fn func(path []string, node *schema.Node)) {
	for _, c := range n.Children {
		var p []string
		switch c.Kind {
		case schema.Keyword:
			p = append(append([]string{}, path...), c.Name)
		case schema.Param:
			// 实例参数：命令里该位置是对象名，判定与取值无关（Match 会落到参数节点本身）。
			p = append(append([]string{}, path...), "probe")
		default:
			continue // 取值叶子（如 <path>）不是可执行命令
		}
		fn(p, c)
		walkOperCommands(c, p, fn)
	}
}

// TestCommandTreeClassGateMatchesDeclaration ①：穷举操作树，判定 == 声明。
func TestCommandTreeClassGateMatchesDeclaration(t *testing.T) {
	x, _ := newCLIKit(t)
	domain := schema.OperRoot()

	checked := 0
	walkOperCommands(domain, nil, func(path []string, n *schema.Node) {
		want := n.RequiredClass()
		// 判定按路径来（与运行期同一条路径：分发器把 "request system …" 整串交给判定）。
		got := x.allowTokens(aaa.ClassSuperUser, domain, path...)
		if !got {
			t.Errorf("路径 %v：super-user 应可通过（声明 %v）", path, want)
		}
		checked++

		// 用"逐级降档"反推判定用的 class：super-user 过、read-only 不过 ⇒ 至少 O；
		// 只有 super-user 过 ⇒ S。这样即使断言写错也能被下面的期望值抓住。
		opOK := x.allowTokens(aaa.ClassOperator, domain, path...)
		roOK := x.allowTokens(aaa.ClassReadOnly, domain, path...)
		var gotClass schema.Class
		switch {
		case roOK:
			gotClass = schema.ClassReadOnly
		case opOK:
			gotClass = schema.ClassOperator
		default:
			gotClass = schema.ClassSuperUser
		}
		if gotClass != want {
			t.Errorf("路径 %v：运行期判定为 %v，命令树声明为 %v（判定必须与声明一致）",
				path, gotClass, want)
		}
	})
	if checked < 50 {
		t.Fatalf("只走了 %d 个节点——遍历本身可能失效（树变小或 Kind 判据写错）", checked)
	}
	t.Logf("已按声明校验 %d 个可执行命令节点", checked)
}

// TestDeclaredSuperUserCommandsRejectOperator ②：端到端——声明为 S 的命令，operator 一律被拒。
// 每条都同时验 super-user 不被"权限"挡住（证明拒绝来自 class 判定，不是命令坏了）。
func TestDeclaredSuperUserCommandsRejectOperator(t *testing.T) {
	cases := []struct {
		cmd  string
		note string
	}{
		{"request system reboot", "重启系统（声明 S；不带 --yes 只到确认问询）"},
		{"request system shutdown", "关机（声明 S）"},
		{"request system poweroff", "断电（声明 S）"},
		{"request system zeroize", "恢复出厂（声明 S）"},
		{"request system software add /tmp/nonexistent.deb", "软件升级（声明 S）"},
		{"request system software rollback", "软件回退（声明 S）"},
		{"request system configuration backup", "配置备份导出（声明 S）"},
		{"request system configuration restore /tmp/nonexistent.json", "配置恢复（声明 S）"},
		{"request system api tls regenerate", "重签自签证书（声明 S）"},
		{"request system ssh host-key regenerate", "重生成 SSH host key（声明 S）"},
		{"request vpp restart", "重启数据面（声明 S）"},
		{"request virtual-machine-functions vnf-a delete", "删除 VNF（声明 S）"},
		{"request container-functions c1 delete", "删除容器（声明 S）"},
	}
	for _, c := range cases {
		x, _ := newCLIKit(t)
		out := x.Execute("admin", aaa.ClassOperator, "ssh", c.cmd).Output
		if !strings.Contains(out, "无权限") {
			t.Errorf("%s：operator 应被拒（%s），实际输出 %q", c.cmd, c.note, out)
		}
		su := x.Execute("admin", aaa.ClassSuperUser, "ssh", c.cmd).Output
		if strings.Contains(su, "无权限") {
			t.Errorf("%s：super-user 不该被权限挡住，实际输出 %q", c.cmd, su)
		}
		ro := x.Execute("admin", aaa.ClassReadOnly, "ssh", c.cmd).Output
		if !strings.Contains(ro, "无权限") {
			t.Errorf("%s：read-only 应被拒，实际输出 %q", c.cmd, ro)
		}
	}
}

// TestDeclaredOperatorCommandsStillWorkForOperator：反向——声明为 O 的命令，operator **仍然**可用。
// 这条防的是"收紧过头"：把 operator 该有的能力一起挡掉同样是缺陷。
func TestDeclaredOperatorCommandsStillWorkForOperator(t *testing.T) {
	cases := []string{
		"request system tech-support generate",                        // 声明 O（诊断归档）
		"request system ntp sync",                                     // 声明 O（时间同步）
		"request system core-dumps export http://127.0.0.1:1/collect", // 声明 O（转储清单导出）
		"request system password change",                              // 声明 O（自助改口令）
	}
	for _, cmd := range cases {
		x, _ := newCLIKit(t)
		out := x.Execute("admin", aaa.ClassOperator, "ssh", cmd).Output
		if strings.Contains(out, "无权限") {
			t.Errorf("%s：operator 应可用（声明 O），实际输出 %q", cmd, out)
		}
		ro := x.Execute("admin", aaa.ClassReadOnly, "ssh", cmd).Output
		if !strings.Contains(ro, "无权限") {
			t.Errorf("%s：read-only 应被拒（O 级命令），实际输出 %q", cmd, ro)
		}
	}
}
