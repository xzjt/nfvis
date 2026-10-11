package main

// 数据面代际变化的启动处置（R12-7 / R12-8）：拆旧面与强制全量重建的计划与执行。
// 纯函数用例——不启动真实 nfvisd。

import (
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 计划：kernel→vpp 拆旧内核面并强制重建；vpp→kernel 只强制重建（旧面由停 VPP + 内核启动
// 重建承担）；同代不拆、不强制；内核启动恒需一次重建。
func TestPlanStartupDataplaneTransition(t *testing.T) {
	cases := []struct {
		name         string
		prev, cur    string
		changed      bool
		teardown     bool
		forceRebuild bool
	}{
		{"kernel->vpp", model.DataPlaneKernel, model.DataPlaneVPP, true, true, true},
		{"vpp->kernel", model.DataPlaneVPP, model.DataPlaneKernel, true, false, true},
		{"vpp->vpp", model.DataPlaneVPP, model.DataPlaneVPP, false, false, false},
		{"kernel->kernel", model.DataPlaneKernel, model.DataPlaneKernel, false, false, true},
		{"first-run-vpp", "", model.DataPlaneVPP, false, false, false},
		{"first-run-kernel", "", model.DataPlaneKernel, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planStartupDataplaneTransition(c.prev, c.cur)
			if got.Changed != c.changed || got.TeardownOldKernel != c.teardown || got.ForceRebuild != c.forceRebuild {
				t.Fatalf("计划不符：changed=%v teardown=%v force=%v；期望 changed=%v teardown=%v force=%v",
					got.Changed, got.TeardownOldKernel, got.ForceRebuild, c.changed, c.teardown, c.forceRebuild)
			}
		})
	}
}

// kernel→vpp 装配触发旧面 teardown（恰好一次）；同代（vpp→vpp）不触发。
func TestStartupTransitionRunsTeardownOnlyOnKernelToVPP(t *testing.T) {
	calls := 0
	teardown := func() error { calls++; return nil }

	if err := planStartupDataplaneTransition("vpp", "vpp").runTeardown(teardown); err != nil || calls != 0 {
		t.Fatalf("同代不应触发 teardown：calls=%d err=%v", calls, err)
	}
	if err := planStartupDataplaneTransition("kernel", "vpp").runTeardown(teardown); err != nil || calls != 1 {
		t.Fatalf("kernel→vpp 应触发一次 teardown：calls=%d err=%v", calls, err)
	}
	// vpp→kernel 不需要拆内核面（旧面是 VPP，由停 VPP 承担）。
	if err := planStartupDataplaneTransition("vpp", "kernel").runTeardown(teardown); err != nil || calls != 1 {
		t.Fatalf("vpp→kernel 不应触发内核 teardown：calls=%d err=%v", calls, err)
	}
}

// 数据面代际变化强制**恰好一次**全量重建；未变化不强制。
func TestModeChangeForcesExactlyOneRebuild(t *testing.T) {
	calls := 0
	rebuild := func() { calls++ }

	if !runStartupRebuild(planStartupDataplaneTransition("kernel", "vpp").ForceRebuild, rebuild) {
		t.Fatal("kernel→vpp 应触发一次强制重建")
	}
	if calls != 1 {
		t.Fatalf("应恰好一次全量重建，得到 %d", calls)
	}

	calls = 0
	if runStartupRebuild(planStartupDataplaneTransition("vpp", "vpp").ForceRebuild, rebuild) {
		t.Fatal("同代不应触发强制重建")
	}
	if calls != 0 {
		t.Fatalf("同代不应调用重建，得到 %d", calls)
	}
}
