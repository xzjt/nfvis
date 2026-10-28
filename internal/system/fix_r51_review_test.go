package system

// round51 评审：GRUB 片段与 chrony 源文件渲染侧的兜底过滤守护（每条都先红后绿）。
//
// 覆盖：F2b 内核附加参数即便在提交期漏网（生成层是第二道），也绝不拼进 GRUB 片段——该片段会被
// grub-mkconfig 当 shell source、由 root（nfvisd）执行 update-grub；F3b chrony 源文件同样由 root
// 拥有、按行解析，非法取值（换行/引号/空白）绝不写进去，并如实报告被忽略的取值。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// F2b：不安全的附加参数被丢弃（绝不出现在片段里），安全的照旧写入，被丢弃的如实报告（报条数）。
func TestFixR51GenerateBaselineDropsUnsafeParam(t *testing.T) {
	grub, _ := GenerateBaseline(KernelDesired{ExtraParams: []string{
		"mitigations=off",
		`x"; touch /tmp/nfvis-pwn; echo "`,
	}})
	if strings.Contains(grub, "touch") {
		t.Fatalf("不安全的附加参数不得出现在片段任何位置（含注释，免得读回路径被自扰）:\n%s", grub)
	}
	wantLine := `GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX} default_hugepagesz=2M mitigations=off"`
	if !strings.Contains(grub, wantLine) {
		t.Fatalf("赋值行应只含安全参数 %q:\n%s", wantLine, grub)
	}
	if !strings.Contains(grub, "已忽略 1 个不安全的附加参数") {
		t.Fatalf("被丢弃的取值应如实报告（不静默丢弃）:\n%s", grub)
	}

	// 换行注入：丢弃含换行的取值后，片段里不得出现任何既非注释、也非赋值行的行（那些行会被 root 执行）。
	g2, _ := GenerateBaseline(KernelDesired{ExtraParams: []string{"a\nEVIL=1"}})
	for _, ln := range strings.Split(strings.TrimRight(g2, "\n"), "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "GRUB_CMDLINE_LINUX=") {
			continue
		}
		t.Fatalf("片段出现了非注释/非赋值行（会被 root 执行）:\n%s", g2)
	}
}

// F3b：chrony 源渲染过滤非法取值（换行能注入任意 chrony 指令），合法取值字面不变。
func TestFixR51RenderChronySourcesFiltersInjection(t *testing.T) {
	got := RenderChronySources([]model.NtpServer{
		{Server: "ntp.example", Prefer: true},
		{Server: "evil.example\nserver attacker.example"},
	})
	if strings.Contains(got, "attacker.example") {
		t.Fatalf("非法取值不得写进 chrony 源文件:\n%s", got)
	}
	if !strings.Contains(got, "pool ntp.example prefer") {
		t.Fatalf("合法取值应照旧写入:\n%s", got)
	}
}

// F3b：应用路径下非法取值既不进文件、又有如实说明；全部非法时如实报失败且不动既有 drop-in。
func TestFixR51ChronyApplierReportsIgnoredAndFailsWhenAllInvalid(t *testing.T) {
	a, _, root := newBaselineFixture(t)
	res, err := a.Apply(context.Background(), &model.SystemConfig{
		Ntp: []model.NtpServer{{Server: "ntp.example"}, {Server: "bad value\nserver evil"}},
	})
	if err != nil {
		t.Fatalf("部分合法不应整体失败: %v", err)
	}
	b, readErr := os.ReadFile(filepath.Join(root, "etc", "chrony", "sources.d", "nfvis.sources"))
	if readErr != nil {
		t.Fatalf("合法值应落盘: %v", readErr)
	}
	if strings.Contains(string(b), "evil") {
		t.Fatalf("非法取值不得写进 drop-in:\n%s", b)
	}
	if !hasNote(res.Notes, "忽略非法取值") {
		t.Fatalf("被忽略的取值应有如实说明: %v", res.Notes)
	}

	// 全部非法：如实报失败，且不产出解析不了的文件（NTP 静默失效比直接拒绝危险得多）。
	a2, _, root2 := newBaselineFixture(t)
	res2, err2 := a2.Apply(context.Background(), &model.SystemConfig{
		Ntp: []model.NtpServer{{Server: "bad\nvalue"}},
	})
	if err2 == nil {
		t.Fatal("声明的服务器全部非法应如实报失败")
	}
	if res2.NTP != BaselineFailed {
		t.Fatalf("应 failed：%+v", res2)
	}
	if _, statErr := os.Stat(filepath.Join(root2, "etc", "chrony", "sources.d", "nfvis.sources")); !os.IsNotExist(statErr) {
		t.Fatalf("不得写下解析不了的 drop-in：%v", statErr)
	}
}
