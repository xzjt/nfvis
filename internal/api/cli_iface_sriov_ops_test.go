package api

// 决策 #449 扩展：`request interfaces <n> enable|disable` 与 `unbind-dpdk` 在成功之后触发一次
// 数据面**整段收敛**（「request = 确保声明状态」：值变更由提交下发、值未变（稳态）由这一步补）。
//
// 由来（真机缺口 A）：commit 只下发**变更**——稳态下 `set Enabled=&true` 无 diff、apply 段根本
// 不跑，「命令成功、什么也没发生」；`unbind-dpdk` 交还内核后设备才出现，引用该口的交换机段/VRF 段
// （成员、VLAN、地址、域兜底路由）的重放早在它出现之前跑完了。注入面用既有 fake 风格
// （x.setNetReconcile / Options.NetReconcile），与 x.setDPDK 同法。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// fakeNetReconcile 记录整段收敛调用（次数、收到的已提交配置、可预置的未收敛项）。
type fakeNetReconcile struct {
	calls int
	cfgs  []model.Config
	errs  []error
}

func (f *fakeNetReconcile) EnsureConsistent(_ context.Context, cfg model.Config) []error {
	f.calls++
	f.cfgs = append(f.cfgs, cfg)
	return f.errs
}

func (f *fakeNetReconcile) reset() { f.calls, f.cfgs = 0, nil }

// declareIface 声明一个接口并提交，随后退出配置模式（交还编辑锁）。
func declareIface(t *testing.T, x *cliExecutor, name string) {
	t.Helper()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces "+name+" description api-test",
		"commit",
		"exit")
}

// 缺口 A（红-绿核心）：稳态（模型 enabled 本就是 true，提交是空修订）下 enable 也必须触发整段
// 收敛；收敛收到的是**提交后的模型值**。
func TestRequestInterfacesEnableReconcilesInSteadyState(t *testing.T) {
	x, engine := newCLIKit(t)
	rec := &fakeNetReconcile{}
	x.setNetReconcile(rec)
	declareIface(t, x, "ens224")

	// 首次 enable：值真实变更（nil→true）——提交下发之外同样补一次收敛（幂等，可接受）。
	first := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 enable")
	if strings.Contains(first.Output, "%%") {
		t.Fatalf("enable 不应失败：%s", first.Output)
	}
	if rec.calls != 1 {
		t.Fatalf("值变更时也应收敛一次，得到 %d 次：%s", rec.calls, first.Output)
	}

	// 稳态：值已是 true ⇒ 空修订、apply 段不跑——收敛必须仍发生。
	rec.reset()
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 enable")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("稳态 enable 不应失败：%s", res.Output)
	}
	if rec.calls != 1 {
		t.Fatalf("稳态 enable 应触发一次整段收敛（缺口 A），得到 %d 次：%s", rec.calls, res.Output)
	}
	if !strings.Contains(res.Output, "收敛") {
		t.Fatalf("输出应说明收敛动作：%s", res.Output)
	}

	// 收敛用的是**提交后的模型值**：ens224 的 enabled=true 已落库，且传给收敛的就是它。
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatal(err)
	}
	enabledOf := func(c model.Config) (found, on bool) {
		for _, ifc := range c.Interfaces {
			if ifc.Name == "ens224" {
				return true, ifc.Enabled != nil && *ifc.Enabled
			}
		}
		return false, false
	}
	if found, on := enabledOf(cfg); !found || !on {
		t.Fatalf("提交后模型里 ens224 的 enabled 应为 true：%+v", cfg.Interfaces)
	}
	if len(rec.cfgs) != 1 {
		t.Fatalf("收敛应恰收到一份已提交配置，得到 %d 份", len(rec.cfgs))
	}
	if found, on := enabledOf(rec.cfgs[0]); !found || !on {
		t.Fatalf("传给收敛的必须是提交后的模型（enabled=true）：%+v", rec.cfgs[0].Interfaces)
	}
}

// 值真实变更（false→true）同样恰触发一次收敛；disable 方向对称。
func TestRequestInterfacesReconcilesOnRealChangeBothDirections(t *testing.T) {
	x, _ := newCLIKit(t)
	rec := &fakeNetReconcile{}
	x.setNetReconcile(rec)
	declareIface(t, x, "ens224")

	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 enable")
	rec.reset()
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 disable")
	if rec.calls != 1 {
		t.Fatalf("disable（true→false）应触发一次收敛，得到 %d 次", rec.calls)
	}
}

// 收敛失败：如实追加原因，但不把已成功的提交说成失败（输出保留 commit 成功与动作结论）。
func TestRequestInterfacesReconcileFailureIsHonest(t *testing.T) {
	x, _ := newCLIKit(t)
	rec := &fakeNetReconcile{errs: []error{
		errors.New(`virtual-switches/vs-lan: Cannot find device "vx-a"`),
		errors.New("interfaces/ens224: 收敛失败（注入）"),
		errors.New("vrfs/vs-wan: 第 3 项"),
		errors.New("第 4 项（应被折叠计数）"),
	}}
	x.setNetReconcile(rec)
	declareIface(t, x, "ens224")
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 enable")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("收敛失败不得把已成功的提交说成失败：%s", res.Output)
	}
	for _, want := range []string{
		"已置为 enable", "commit 成功",
		"收敛未完全成功", "Cannot find device", "收敛失败（注入）", "第 3 项", "共 4 项",
	} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("输出缺少 %q：%s", want, res.Output)
		}
	}
}

// 未接入收敛能力（VPP 数据面 / 未装配）：输出保持原样，不得凭空宣称已收敛。
func TestRequestInterfacesNoReconcilerKeepsOutput(t *testing.T) {
	x, _ := newCLIKit(t)
	declareIface(t, x, "ens224")
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 enable").Output
	if strings.Contains(out, "收敛") {
		t.Fatalf("未接入收敛能力时不得宣称已收敛：%s", out)
	}
	if !strings.Contains(out, "已置为 enable") {
		t.Fatalf("原动作结论应保持：%s", out)
	}
}

// `unbind-dpdk` 交还内核成功后触发一次整段收敛（手册「交还后按声明自动收敛」的落点）；
// 未确认（只问不做）与 bind-dpdk 方向都不触发。
func TestRequestInterfacesUnbindDPDKTriggersReconcile(t *testing.T) {
	x, _ := newCLIKit(t)
	dp := &fakeDPDK{}
	x.setDPDK(dp)
	rec := &fakeNetReconcile{}
	x.setNetReconcile(rec)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 unbind-dpdk --yes").Output
	if dp.calls != 1 || dp.lastBound {
		t.Fatalf("应执行解绑：%q（%+v）", out, dp)
	}
	if rec.calls != 1 {
		t.Fatalf("交还内核后应触发一次整段收敛，得到 %d 次：%s", rec.calls, out)
	}

	// 未确认：sysfs 都没动，收敛无从谈起。
	rec.reset()
	if got := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 unbind-dpdk").Output; !strings.Contains(got, "yes,no") {
		t.Fatalf("应先问询：%q", got)
	}
	if rec.calls != 0 {
		t.Fatalf("未确认不得触发收敛，得到 %d 次", rec.calls)
	}

	// bind-dpdk：口交 DPDK，内核数据面下本就拒绝，收敛无对象可补。
	rec.reset()
	if got := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request interfaces ens224 bind-dpdk --yes").Output; strings.Contains(got, "%%") {
		t.Fatalf("测试夹具的假底座应允许绑定：%q", got)
	}
	if rec.calls != 0 {
		t.Fatalf("bind-dpdk 不应触发收敛，得到 %d 次", rec.calls)
	}
}

// 装配面接通（Options.NetReconcile → 执行器）：走真实 HTTP 命令路径验证一次。
func TestRequestInterfacesReconcileWiredThroughOptions(t *testing.T) {
	rec := &fakeNetReconcile{}
	ts := newTestServerOpts(t, Options{NetReconcile: rec})
	token := loginAdmin(t, ts)
	cliRun(t, ts, token, "ssh", "configure")
	cliRun(t, ts, token, "ssh", "set interfaces ens224 description api-test")
	cliRun(t, ts, token, "ssh", "commit")
	cliRun(t, ts, token, "ssh", "exit")

	rec.reset()
	out := cliRun(t, ts, token, "ssh", "request interfaces ens224 enable")
	if rec.calls != 1 {
		t.Fatalf("Options.NetReconcile 应接到执行器，得到 %d 次调用：%s", rec.calls, out)
	}
	if !strings.Contains(out, "收敛") {
		t.Fatalf("输出应说明收敛动作：%s", out)
	}
}
