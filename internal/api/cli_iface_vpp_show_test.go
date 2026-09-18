package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

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
