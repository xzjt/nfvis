package api

// 缺陷回归（真机低危）：`show system hardware` 只打 `SMART unknown` 时，操作者无法区分
// 「本机没装 smartctl」「盘不支持 SMART」「读取失败」——三者处置完全不同。
// 本用例钉住 CLI 侧的呈现：unknown 必须带原因，且**正常盘的行与历史逐字节一致**。

import (
	"context"
	"strings"
	"testing"

	ksys "github.com/xzjt/nfvis/internal/system"
)

// reasonHardware 直接返回预置快照（原因的判定在 internal/system，这里只验渲染）。
type reasonHardware struct{ hh ksys.HardwareHealth }

func (r *reasonHardware) Collect(context.Context) ksys.HardwareHealth { return r.hh }

func (r *reasonHardware) Evaluate(*ksys.HardwareHealth, int, int, int) []string { return nil }

// 用例 1：SMART 取不到且带原因 ⇒ 表格下方给出「设备: 原因」，表格列宽不变。
func TestCLIHardwareShowsSmartUnknownReason(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setHardware(&reasonHardware{hh: ksys.HardwareHealth{
		Disks: []ksys.HealthDisk{{
			Device: "/dev/sda", SmartStatus: "unknown",
			SmartReason: "未安装 smartctl（安装 smartmontools 后重新查看）",
		}},
	}})

	out := run(t, x, "admin", "super-user", "ssh", "show system hardware")
	if !strings.Contains(out, "/dev/sda         unknown    -          -") {
		t.Fatalf("表格行必须与历史列宽一致（原因不塞进窄列）：%s", out)
	}
	if !strings.Contains(out, "SMART unknown 原因:") || !strings.Contains(out, "未安装 smartctl") {
		t.Fatalf("unknown 必须带原因（操作者据此决定装工具/换盘/查权限）：%s", out)
	}
	if !strings.Contains(out, "/dev/sda: 未安装 smartctl") {
		t.Fatalf("原因须点名是哪块盘：%s", out)
	}
}

// 用例 2（回归）：SMART 读得正常时不得多出原因段落——输出与改动前逐字节相同。
func TestCLIHardwareHealthyOutputUnchanged(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setHardware(&reasonHardware{hh: ksys.HardwareHealth{
		Disks: []ksys.HealthDisk{{
			Device: "/dev/sda", SmartStatus: "passed", TempCelsius: 38, WearPercent: 3,
		}},
	}})

	out := run(t, x, "admin", "super-user", "ssh", "show system hardware")
	if strings.Contains(out, "SMART unknown 原因") {
		t.Fatalf("SMART 正常时不得出现原因段落：%s", out)
	}
	if !strings.Contains(out, "/dev/sda         passed     38 C       3%") {
		t.Fatalf("健康读数行应与历史逐字节一致：%s", out)
	}
}
