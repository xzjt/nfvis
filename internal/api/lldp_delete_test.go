package api

// 决策 #440 真机暴露（R4-3）：删光 LLDP 后不得留下空 `protocols` 对象。
//
// 因：空壳（`protocols: {}`）里没有可发射的语句，`show configuration | display set` 的
// 回放自校验还原不出它，如实报「内部错误：生成语句未能完整还原配置」——运维突然读不到
// 配置（真机实证：`delete protocols lldp` 之后 display set 直接失败，display json 仍可用）。
// 与 NAT（R84-26）/防火墙（#388）同一因，同一处置：**删除合流点不留空壳**。
//
// 两条删除路径都要收口：① 逐叶子删（别名路径）；② 整节点删（通用树遍历路径）。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// lldpFixture 最简 LLDP 配置（接口已声明 + 全局开关 + 间隔 + 按口开关）。
func lldpFixture() []string {
	return []string{
		"set interfaces ens192",
		"set protocols lldp enable true",
		"set protocols lldp advertisement-interval 5",
		"set protocols lldp interface ens192 enable true",
	}
}

// TestLLDPDeleteAllLeavesNoEmptyNode：逐叶子删光 ⇒ committed 里 protocols 归 nil，
// 且 display set 必须成功、不再出现 lldp 语句。
func TestLLDPDeleteAllLeavesNoEmptyNode(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, lldpFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"delete protocols lldp interface ens192 enable true",
		"delete protocols lldp advertisement-interval",
		"delete protocols lldp enable",
		"commit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.Protocols != nil {
		t.Fatalf("删光 LLDP 后不应留下空 protocols 对象: %+v", *cfg.Protocols)
	}
	out := execOK(t, x, "show configuration | display set")
	if strings.Contains(out, "lldp") {
		t.Fatalf("删光 LLDP 后 display set 不应出现 lldp 语句:\n%s", out)
	}
}

// TestLLDPDeleteWholeNodeNoEmptyNode：整节点删（通用遍历路径）同样不留空壳，
// 且 display set 必须成功（真机就是在这一形态上暴露的）。
func TestLLDPDeleteWholeNodeNoEmptyNode(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, lldpFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure", "delete protocols lldp", "commit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.Protocols != nil {
		t.Fatalf("整节点删后不应留下空 protocols 对象: %+v", *cfg.Protocols)
	}
	execOK(t, x, "show configuration | display set")
}
