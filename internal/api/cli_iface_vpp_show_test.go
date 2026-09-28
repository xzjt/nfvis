package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// 多余/未知参数不得被静默忽略：`show port-mirroring bogus extra` 此前照常作答（args 未使用），
// 与决策 #153 同族。现须如实报「该 show 命令形式未支持」并列出可用形态，且**不回配置正文**。
func TestShowPortMirroringRejectsExtraArgs(t *testing.T) {
	x, _ := newCLIKit(t)
	// 先落一条真实会话：若命令静默忽略多余 token，输出里就会带上它
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens192 description span-src",
		"set interfaces ens224 description span-dst",
		"set port-mirroring span1 source interface ens192 direction both",
		"set port-mirroring span1 analyzer interface ens224",
		"commit", "exit",
	)
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show port-mirroring").Output; !strings.Contains(out, "span1") {
		t.Fatalf("前置：show port-mirroring 应列出 span1: %q", out)
	}
	for _, cmd := range []string{
		"show port-mirroring bogus extra",
		"show port-mirroring span1",
	} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd).Output
		if !strings.HasPrefix(out, "%") || !strings.Contains(out, "该 show 命令形式未支持") {
			t.Fatalf("%s 应如实报「该 show 命令形式未支持」: %q", cmd, out)
		}
		// 报错不得夹带配置正文（正文里会有 source/analyzer/direction 字段）
		if strings.Contains(out, "analyzer") || strings.Contains(out, "direction") {
			t.Fatalf("%s 报错时不得回配置正文（静默误答）: %q", cmd, out)
		}
	}
}

// R86-7：`show interfaces <名> detail` 的 MTU 列 = **有效 MTU**——配置显式值优先
// （`set interfaces <n> mtu`），否则运行态（VPP sw_interface_details 的 L3 MTU）；
// 两者都取不到显示 `-`（不编造 0/默认值）。列表表（`show interfaces physical`）的列
// **不变**：cli-semantic-check.sh 按列比对 vppctl，MTU 只进单口视图。
func TestShowInterfacesDetailMTUEffective(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vpp: []string{"bvi0", "ens2f0", "ens2f1", "ens2f2"}})
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{
		"ens2f0": {AdminUp: true, LinkUp: true, MTU: 1500}, // 配置显式 9000 → 显示 9000
		"ens2f1": {AdminUp: true, LinkUp: true, MTU: 1500}, // 配置未给 → 显示运行态 1500
		"ens2f2": {AdminUp: true, LinkUp: true},            // 两侧都没有 → 显示 -
		"bvi0":   {AdminUp: true, LinkUp: true, MTU: 9000}, // 未声明口 → 运行态 9000
	}})
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens2f0 mtu 9000",
		"set interfaces ens2f1 description no-cfg-mtu",
		"set interfaces ens2f2 description no-mtu",
		"commit", "exit",
	)

	rowOf := func(out, name string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, name) {
				return l
			}
		}
		return ""
	}
	for _, c := range []struct{ name, want, why string }{
		{"ens2f0", "9000", "配置显式值优先（运行态是 1500）"},
		{"ens2f1", "1500", "配置未给 → 取运行态 L3 MTU"},
		{"ens2f2", "-", "两侧都取不到 → 不编造"},
		{"bvi0", "9000", "未声明口 → 取运行态 L3 MTU"},
	} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces "+c.name+" detail").Output
		if !strings.Contains(out, "MTU") {
			t.Fatalf("%s detail 视图应有 MTU 列: %q", c.name, out)
		}
		row := rowOf(out, c.name)
		if row == "" {
			t.Fatalf("%s 应有数据行: %q", c.name, out)
		}
		// 列序：Interface Admin Link Speed MTU Driver RxPkts TxPkts Description
		// （Description 为空时行尾被裁剪，故只要求到 MTU 列为止）
		fs := strings.Fields(row)
		if len(fs) < 5 || fs[4] != c.want {
			t.Fatalf("%s 的 MTU 列应为 %q（%s），实际 %v —— 行 %q", c.name, c.want, c.why, fs, row)
		}
	}

	// 列表表列不得改动（语义校验按列比对 vppctl）
	listOut := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical").Output
	hdr := strings.SplitN(listOut, "\n", 2)[0]
	if strings.Contains(hdr, "MTU") {
		t.Fatalf("`show interfaces physical` 列表列不得改动（cli-semantic-check.sh 按列比对 vppctl）: %q", hdr)
	}
}

// 发现 #14：`show interfaces management` 的「没有配置」是**空态**不是**错误**——
// 带 %% 前缀会被真机冒烟按失败计（同一条命令在不同配置状态下结论不同）。
// 三种「没有」的形态必须给同一句普通提示：从未配置 / 空对象（配置过又删除）/ 无可显示内容。
func TestShowManagementInterfaceEmptyStates(t *testing.T) {
	x, _ := newCLIKit(t)
	empty := model.SystemConfig{}
	for _, tc := range []struct {
		name string
		cfg  model.Config
	}{
		{"从未配置", model.Config{}},
		{"空对象", model.Config{System: &empty}},
		{"空对象（零值管理口）", model.Config{System: &model.SystemConfig{Management: &model.MgmtConfig{}}}},
	} {
		out := x.showManagementInterface(tc.cfg)
		if strings.Contains(out, "%%") {
			t.Fatalf("[%s] 空态不得输出 %% 前缀（会被冒烟按失败计）: %q", tc.name, out)
		}
		if !strings.Contains(out, "未配置管理口") {
			t.Fatalf("[%s] 应说明未配置管理口: %q", tc.name, out)
		}
	}
	// 只配了地址没配口名：仍应显示表格 + 普通提示
	out := x.showManagementInterface(model.Config{System: &model.SystemConfig{
		Management: &model.MgmtConfig{Address: "192.168.1.10/24"},
	}})
	if strings.Contains(out, "%%") {
		t.Fatalf("提示不得带 %% 前缀: %q", out)
	}
	if !strings.Contains(out, "192.168.1.10/24") || !strings.Contains(out, "(未指定)") {
		t.Fatalf("应显示已配置的地址与「未指定」口名: %q", out)
	}
}
