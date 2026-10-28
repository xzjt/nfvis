package main

// round51 R51-M8：内核→VPP 切换的 teardown provider 拿不到配置 ⇒ 容器 veth 清理静默 no-op。
//
// 由来：`teardownOldKernelDataplane` 现场造一个**全新**的 `netkernel.New(nil)`（进程内簿记为空），
// 却不给它 `SetConfig(cfg)`；而 netkernel 的容器 veth 名单取自「簿记 ∪ 进程内配置快照
// configSnapshot()」——快照是零值 Config ⇒ 名单恒空 ⇒ `DeleteContainerVeths` 直接返回 nil：
// 产品自建的容器宿主端 veth 静默留在内核里（无日志、无告警），与新起来的 VPP 面双活。
// 运行实例（同一文件里内核数据面分支）本就调 SetConfig，这里漏了同一句。
//
// 本文件在**装配层**钉住这条接线（netkernel 内部的名单算法由它自己的用例覆盖）：
// 用假 provider 替换构造钩子，断言 teardown 收到的 provider ① 拿到 committed 配置，
// ② `TeardownDataplane` 拿到的也是同一份，③ 赋值发生在 teardown 之前。

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakeKernelTeardown 记录装配层送进来的东西与顺序。
type fakeKernelTeardown struct {
	alarms      *network.AlarmStore
	cfg         model.Config
	cfgSet      bool
	teardownCfg model.Config
	order       []string
}

func (f *fakeKernelTeardown) SetAlarms(a *network.AlarmStore) {
	f.alarms = a
	f.order = append(f.order, "set-alarms")
}

func (f *fakeKernelTeardown) SetConfig(c model.Config) {
	f.cfg, f.cfgSet = c, true
	f.order = append(f.order, "set-config")
}

func (f *fakeKernelTeardown) TeardownDataplane(_ context.Context, c model.Config) error {
	f.teardownCfg = c
	f.order = append(f.order, "teardown")
	return nil
}

func TestFixR51TeardownProviderReceivesConfig(t *testing.T) {
	f := &fakeKernelTeardown{}
	old := newKernelTeardownProvider
	newKernelTeardownProvider = func() kernelTeardowner { return f }
	defer func() { newKernelTeardownProvider = old }()

	// 现场里最要紧的一类对象：容器 vNIC（宿主端 veth 的名字只能由声明反查出来）。
	cfg := model.Config{
		ContainerFunctions: []model.ContainerFunction{{
			Name:  "ct1",
			Image: "alpine:3.20",
			Interfaces: []model.VnfInterface{
				{Name: "eth0", Type: "memif"},
				{Name: "eth1", Type: "memif"},
			},
		}},
	}
	alarms := network.NewAlarmStore()

	if err := teardownOldKernelDataplane(cfg, alarms, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("teardownOldKernelDataplane: %v", err)
	}
	if !f.cfgSet {
		t.Fatal("teardown provider 未收到配置快照：容器 veth 名单恒空 ⇒ 清理静默 no-op（R51-M8）")
	}
	if !reflect.DeepEqual(f.cfg, cfg) {
		t.Fatalf("provider 收到的配置与 committed 不一致:\ngot  %+v\nwant %+v", f.cfg, cfg)
	}
	if !reflect.DeepEqual(f.teardownCfg, cfg) {
		t.Fatalf("TeardownDataplane 的入参应仍是同一份配置: %+v", f.teardownCfg)
	}
	if f.alarms != alarms {
		t.Fatal("告警表应送达 provider（残留要能落进告警）")
	}
	// 顺序：先给配置，再 teardown（teardown 内部任何时候读快照都必须已经就位）。
	want := []string{"set-alarms", "set-config", "teardown"}
	if !reflect.DeepEqual(f.order, want) {
		t.Fatalf("调用顺序不符，实际 %v，期望 %v", f.order, want)
	}
}
