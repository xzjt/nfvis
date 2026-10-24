package api

// 修 BUG-17（读口这一半）：`show system software result` 必须能与**当前事实**交叉核对。
//
// 背景（复测报告实证）：升级判定过早导致落盘结论 `{"status":"install ok installed","ok":true}`，
// 而宿主实际 `half-configured`；且该错误结论**跨重启不自愈**——操作者再跑一次读口，看到的
// 仍是「成功」。故读口不能只复述记录：能实时读到 dpkg 状态就以它为准，不一致必须明说。
//
// 本用例钉住三种形态：记录=成功而现状不成功（过期）、记录=失败而现状已成功（事后修复）、
// 读不到现状（如实说无法核对，不假装一致）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// fakeSoftwareRuntime 同时实现 SoftwareRuntime 与可选读口 packageStateReader。
type fakeSoftwareRuntime struct {
	status string
	ver    string
	err    error

	addCalled bool
}

func (f *fakeSoftwareRuntime) Add(context.Context, string, string) (ksys.SoftwareResult, error) {
	f.addCalled = true
	return ksys.SoftwareResult{}, errors.New("not used")
}
func (f *fakeSoftwareRuntime) Rollback(context.Context) (ksys.SoftwareResult, error) {
	return ksys.SoftwareResult{}, errors.New("not used")
}
func (f *fakeSoftwareRuntime) Reboot(context.Context) error   { return nil }
func (f *fakeSoftwareRuntime) Shutdown(context.Context) error { return nil }
func (f *fakeSoftwareRuntime) NTPSync(context.Context, []string) (string, error) {
	return "", nil
}
func (f *fakeSoftwareRuntime) PackageState(context.Context) (string, string, error) {
	return f.status, f.ver, f.err
}

// 记录说成功、现状是 half-configured：必须点破，并按当前状态判「未落定为成功」。
func TestShowSoftwareResultDetectsStaleSuccessRecord(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","version":"3.0.26","status":"install ok installed","ok":true}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{status: "install ok half-configured", ver: "3.0.26"})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	for _, want := range []string{"记录与当前状态不符", "half-configured", "以当前状态为准", "未落定为成功"} {
		if !strings.Contains(out, want) {
			t.Fatalf("过期成功记录必须点破并改按当前状态判定，应含 %q：\n%s", want, out)
		}
	}
}

// 记录说失败、现状已 installed：如实说明现状与可能的原因，但**该次尝试仍按记录为失败**——
// 修 BUG-18 后不再翻案成「结论（按当前状态）: 成功」：「包没坏」与「那次升级成了」是两件事
// （同版本坏包失败后，包本来就好好的；旧翻案逻辑会把那次失败显示成成功）。
func TestShowSoftwareResultDetectsStaleFailureRecord(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","version":"3.0.26","status":"安装失败: x","ok":false}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{status: "install ok installed", ver: "3.0.26"})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	if !strings.Contains(out, "记录为「失败」") {
		t.Fatalf("过期的失败记录应说明现状：\n%s", out)
	}
	if !strings.Contains(out, "事后修复") {
		t.Fatalf("现状完好时应给出「可能是事后修复/其后升级」的解释：\n%s", out)
	}
	if !strings.Contains(out, "该次尝试仍按记录为失败") {
		t.Fatalf("不得把失败的尝试翻案成成功（BUG-18 读口边界）：\n%s", out)
	}
	if strings.Contains(out, "结论（按当前状态）: 成功") {
		t.Fatalf("失败记录不得翻案成成功结论：\n%s", out)
	}
}

// 修 BUG-18（版本维度交叉核对）：失败记录的目标版本不在机上（跨版本坏包——目标 3.0.26
// 没装上、当前仍是 3.0.28）⇒ 明说「该次升级确实未发生」，不得翻案成成功。
func TestShowSoftwareResultFailureWithTargetNotInstalled(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","attempt":"42","time":"2026-10-24T01:04:55Z","version":"3.0.26","reason":"安装未完成：dpkg -i 失败（退出码 2）","ok":false}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{status: "install ok installed", ver: "3.0.28"})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	for _, want := range []string{"目标版本未在机上", "该次升级确实未发生", "当前已装 3.0.28"} {
		if !strings.Contains(out, want) {
			t.Fatalf("目标版本未装机时应明说（缺 %q）：\n%s", want, out)
		}
	}
	if strings.Contains(out, "结论（按当前状态）: 成功") {
		t.Fatalf("失败记录不得翻案成成功：\n%s", out)
	}
}

// 结论带时间与尝试标识（修 BUG-18 的 P2 项）：渲染给出「时间」，结构化视图带 attempt/time。
func TestShowSoftwareResultRendersAttemptAndTime(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","attempt":"42","time":"2026-10-24T01:04:55Z","version":"3.0.26","status":"install ok installed","ok":true}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{status: "install ok installed", ver: "3.0.26"})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	if !strings.Contains(out, "时间: 2026-10-24T01:04:55Z") {
		t.Fatalf("记录时间必须渲染（防旧记录冒充新结论）：\n%s", out)
	}
	st, isMap := x.structured.(map[string]any)
	if !isMap || st["attempt"] != "42" || st["time"] != "2026-10-24T01:04:55Z" {
		t.Fatalf("结构化视图应带 attempt/time：%v", x.structured)
	}
}

// 记录与现状一致：明说一致（不制造噪声）。
func TestShowSoftwareResultConsistentSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","version":"3.0.26","status":"install ok installed","ok":true}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{status: "install ok installed", ver: "3.0.26"})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	if !strings.Contains(out, "与记录一致") {
		t.Fatalf("一致时应明说：\n%s", out)
	}
	if strings.Contains(out, "不符") {
		t.Fatalf("一致时不得报不符：\n%s", out)
	}
}

// 读不到现状：如实说「无法核对」，不假装一致（不猜）。
func TestShowSoftwareResultUnreadableStateIsHonest(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","version":"3.0.26","status":"install ok installed","ok":true}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{err: errors.New("dpkg 数据库不可读")})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	if !strings.Contains(out, "无法与记录交叉核对") {
		t.Fatalf("读不到现状时必须如实说明：\n%s", out)
	}
	if strings.Contains(out, "与记录一致") {
		t.Fatalf("读不到现状时不得声称一致：\n%s", out)
	}
}

// 未注入读口（未装配）：同样如实说无法核对，不 panic。
func TestShowSoftwareResultWithoutReaderIsHonest(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","version":"3.0.26","status":"install ok installed","ok":true}`)

	x, _ := newCLIKit(t)
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))
	if !strings.Contains(out, "无法与记录交叉核对") {
		t.Fatalf("无读口时应如实说明：\n%s", out)
	}
}

// 命令仍可从 CLI 走到（读口存在且不是错误输出）。
func TestShowSoftwareResultCommandRuns(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system software result").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("读口不应报错：\n%s", out)
	}
}

func writeResultFixture(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "last-result.json"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
}

// pending 记录是「未定」，不是「失败」——两者混报会让操作者误判。
func TestShowSoftwareResultPendingIsUndetermined(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","version":"","status":"pending","ok":false}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{status: "install ok installed", ver: "3.0.26"})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	if !strings.Contains(out, "未定") {
		t.Fatalf("pending 应报「未定」：\n%s", out)
	}
	if !strings.Contains(out, "升级进行中或判定未完成") {
		t.Fatalf("pending 应说明原因：\n%s", out)
	}
}

// 失败记录的原因要看得见（Reason 与 Status 分开存，渲染必须都给出）。
func TestShowSoftwareResultRendersReason(t *testing.T) {
	dir := t.TempDir()
	writeResultFixture(t, dir, `{"action":"add","version":"3.0.26","status":"","reason":"包未收敛：等待 60s 后仍不稳定","ok":false}`)

	x, _ := newCLIKit(t)
	x.setSoftware(&fakeSoftwareRuntime{status: "install ok half-configured", ver: "3.0.26"})
	out := x.renderSoftwareResultPath(filepath.Join(dir, "last-result.json"))

	if !strings.Contains(out, "原因: 包未收敛") {
		t.Fatalf("失败/未定的原因必须渲染：\n%s", out)
	}
}
