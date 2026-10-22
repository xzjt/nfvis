package system

// 缺陷回归（真机低危）：`show system hardware` 对没有 smartctl 的机器只打一行
// `SMART unknown`，操作者无法区分三种完全不同的事实与处置：
//   - 本机没装 smartctl（装 smartmontools 即可）；
//   - smartctl 跑了，但盘不支持/没有 SMART 数据（换盘或忽略）；
//   - smartctl 跑了但读失败（查权限、总线、设备）。
//
// 判据：unknown 必须**带原因**；SMART 读得正常时字段为零值（JSON 里被 omitempty 省掉），
// 输出与历史逐字节一致。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// smartRunner 按设备返回一组预设的 (输出, 错误)；未匹配到设备的调用返回空输出。
type smartRunner struct {
	byDev map[string]struct {
		out string
		err error
	}
	calls []string
}

func (s *smartRunner) run(_ context.Context, name string, args ...string) (string, error) {
	s.calls = append(s.calls, name+" "+strings.Join(args, " "))
	if name != "smartctl" {
		return "", errors.New("unexpected command")
	}
	for dev, r := range s.byDev {
		for _, a := range args {
			if a == dev {
				return r.out, r.err
			}
		}
	}
	return "", nil
}

// collectOneDisk 在临时 /sys/block 里造一个块设备并采集。
func collectOneDisk(t *testing.T, r *smartRunner, dev string) HealthDisk {
	t.Helper()
	sysBlock := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysBlock, strings.TrimPrefix(dev, "/dev/")), 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewHardwareProvider(r.run)
	h.SetPaths(sysBlock, t.TempDir(), filepath.Join(t.TempDir(), "no-ipmi"), t.TempDir())
	hh := h.Collect(context.Background())
	if len(hh.Disks) != 1 {
		t.Fatalf("应枚举到 1 块磁盘：%+v", hh.Disks)
	}
	return hh.Disks[0]
}

// 用例 1：smartctl 不存在 ⇒ 原因必须点名「未安装 smartctl」，而不是笼统的读取失败。
func TestHardwareSmartReasonSmartctlMissing(t *testing.T) {
	r := &smartRunner{byDev: map[string]struct {
		out string
		err error
	}{"/dev/sda": {"", errors.New(`exec: "smartctl": executable file not found in $PATH`)}}}
	d := collectOneDisk(t, r, "/dev/sda")
	if d.SmartStatus != "unknown" {
		t.Fatalf("读不到 SMART 时状态应为 unknown：%+v", d)
	}
	if !strings.Contains(d.SmartReason, "未安装 smartctl") {
		t.Fatalf("原因要点名 smartctl 未安装（操作者据此装 smartmontools）：%q", d.SmartReason)
	}
}

// 用例 2：smartctl 跑了并给出 unsupported 应答 ⇒ 原因必须指向「设备不支持/无数据」，
// 不能被当成工具缺失或一般读取失败。
func TestHardwareSmartReasonDeviceUnsupported(t *testing.T) {
	raw := `{"device":{"name":"/dev/sda","type":"scsi"},"smartctl":{"exit_status":2},
	         "messages":[{"string":"Device does not support SMART","severity":"error"}]}`
	r := &smartRunner{byDev: map[string]struct {
		out string
		err error
	}{"/dev/sda": {raw, errors.New("exit status 2")}}}
	d := collectOneDisk(t, r, "/dev/sda")
	if !strings.Contains(d.SmartReason, "不支持") {
		t.Fatalf("原因要点出设备不支持/无 SMART 数据：%q", d.SmartReason)
	}
	if strings.Contains(d.SmartReason, "未安装") {
		t.Fatalf("设备不支持不是工具缺失：%q", d.SmartReason)
	}
}

// 用例 3：其它读取失败 ⇒ 原因须带出底层错误文本，便于操作者据实排查。
func TestHardwareSmartReasonReadFailureCarriesCause(t *testing.T) {
	raw := `{"messages":[{"string":"Smartctl open device: /dev/sda failed: Permission denied","severity":"error"}]}`
	r := &smartRunner{byDev: map[string]struct {
		out string
		err error
	}{"/dev/sda": {raw, errors.New("exit status 2")}}}
	d := collectOneDisk(t, r, "/dev/sda")
	if !strings.Contains(d.SmartReason, "exit status 2") {
		t.Fatalf("原因须含底层错误文本：%q", d.SmartReason)
	}
	if !strings.Contains(d.SmartReason, "Permission denied") {
		t.Fatalf("原因须带出 smartctl 自己的说明：%q", d.SmartReason)
	}
	if strings.Contains(d.SmartReason, "不支持") || strings.Contains(d.SmartReason, "未安装") {
		t.Fatalf("权限问题不得归因成设备不支持或工具缺失：%q", d.SmartReason)
	}
}

// 用例 2b：smartctl 用结构化字段表态（smart_support.available=false）⇒ 归「设备不支持」，
// 这条比 messages 的措辞可靠：不同版本/不同桥的文案会变，字段语义不会。
func TestHardwareSmartReasonStructuredUnsupported(t *testing.T) {
	raw := `{"device":{"name":"/dev/sdb"},"smart_support":{"available":false},
	         "smartctl":{"exit_status":0}}`
	r := &smartRunner{byDev: map[string]struct {
		out string
		err error
	}{"/dev/sdb": {raw, nil}}}
	d := collectOneDisk(t, r, "/dev/sdb")
	if !strings.Contains(d.SmartReason, "不支持") {
		t.Fatalf("smart_support.available=false 应归「设备不支持」：%q", d.SmartReason)
	}
}

// 用例 2c：盘自检**失败**（smart_status.passed=false 且 JSON 完整）仍是 failed——
// 原因字段是为 unknown 服务的，别把「读到且判失败」降级成 unknown。
func TestHardwareSmartFailedIsNotUnknown(t *testing.T) {
	raw := `{"smart_status":{"passed":false},"temperature":{"current":41}}`
	r := &smartRunner{byDev: map[string]struct {
		out string
		err error
	}{"/dev/sda": {raw, nil}}}
	d := collectOneDisk(t, r, "/dev/sda")
	if d.SmartStatus != "failed" {
		t.Fatalf("自检失败应报 failed：%+v", d)
	}
	if d.SmartReason != "" {
		t.Fatalf("failed 不是 unknown，不该带原因：%q", d.SmartReason)
	}
}

// 用例 4（回归）：SMART 读得正常时不得多出原因字段，读数与历史一致；
// JSON 形状也必须与历史逐字节相同（omitempty）。
func TestHardwareSmartHealthyPathUnchanged(t *testing.T) {
	raw := `{"smart_status":{"passed":true},"temperature":{"current":38},
	         "nvme_smart_health_information_log":{"percentage_used":3}}`
	r := &smartRunner{byDev: map[string]struct {
		out string
		err error
	}{"/dev/nvme0n1": {raw, nil}}}
	d := collectOneDisk(t, r, "/dev/nvme0n1")
	if d.SmartStatus != "passed" || d.TempCelsius != 38 || d.WearPercent != 3 {
		t.Fatalf("健康读数应与历史一致：%+v", d)
	}
	if d.SmartReason != "" {
		t.Fatalf("SMART 正常时不该有原因：%q", d.SmartReason)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "smart_reason") {
		t.Fatalf("健康读数不得新增字段（响应须与历史一致）：%s", b)
	}
}
