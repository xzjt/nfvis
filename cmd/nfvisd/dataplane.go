package main

// 数据面相关的装配选择（v3 决策 #404）。
//
// 大部分运行态读物由 netRuntime 的两种实现**同形提供**（VPP 给真值、内核给如实不可用），
// 装配处直接传 netProvider 即可。本文件只处理**方法面不同**的四处：VPP 控制面、DPDK 接管、
// 诊断、运行态聚合——内核数据面下它们要么换成如实报「不支持」的实现，要么退化为空。

import (
	"context"
	"errors"
	"log/slog"

	"github.com/xzjt/nfvis/internal/api"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/netkernel"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
	"github.com/xzjt/nfvis/internal/state"
)

// ErrKernelDataPlane 该能力在 Linux 内核网络数据面下不可用。
//
// 与 netkernel.ErrUnsupported 同口径（不静默成功、不假装「VPP 未连接」），
// 只是面向的是 API 层而非编排层。
var ErrKernelDataPlane = errors.New("当前数据面为 Linux 内核网络，该能力不可用")

// vppControllerFor 选择 VPP 控制面实现：VPP 数据面取真实控制器；内核数据面取如实回报的实现。
func vppControllerFor(mode string, mgr *network.Manager, applier *network.Applier,
	engine *config.Engine, socket string) api.VppController {
	if mode == model.DataPlaneKernel {
		return kernelVppController{}
	}
	return &vppController{mgr: mgr, applier: applier, engine: engine, socket: socket}
}

// kernelVppController 内核数据面下的 VPP 控制面占位实现。
//
// 口径（决策 #404）：`show vpp` / `GET /vpp/status` 必须明确回答「当前数据面是内核网络」，
// 而不是「VPP 未连接」——后者会把操作者引向「去把 VPP 起来」这条无效路径。
type kernelVppController struct{}

// Status 如实回报：数据面为内核网络，未使用 VPP。
func (kernelVppController) Status(*model.VppConfig) api.VppStatus {
	return api.VppStatus{
		Connected: false,
		Mode:      model.DataPlaneKernel,
		LastError: "当前数据面为 Linux 内核网络，未使用 VPP；如需 VPP 请改回数据面并重启服务",
	}
}

// Restart 内核数据面下无 VPP 可重启，如实报错（不静默成功）。
func (kernelVppController) Restart(context.Context, *model.VppConfig) error {
	return ErrKernelDataPlane
}

// Version 未使用 VPP，版本为空。
func (kernelVppController) Version() string { return "" }

// dpdkSetterFor 选择 DPDK 接管实现：内核数据面下物理口留在内核，没有 DPDK 接管这回事。
func dpdkSetterFor(mode string, binder *network.DPDKBinder, rec *network.Bindings,
	log *slog.Logger, facts func() network.ManagementFacts, mgr *network.Manager) api.DPDKSetter {
	if mode == model.DataPlaneKernel {
		return kernelDPDKSetter{}
	}
	return &dpdkController{b: binder, rec: rec, logger: log, facts: facts,
		// 数据面占用探测（发现 #13）：解绑前问 VPP「这个口还在你手里吗」
		dataplane: func(ifname string) (bool, error) {
			c, err := mgr.SvcClientFunc()()
			if err != nil {
				return false, err
			}
			defer c.Close()
			_, ok, err := c.SwInterfaceIndex(ifname)
			return ok, err
		}}
}

// kernelDPDKSetter 内核数据面下的 DPDK 接管占位实现（如实报不可用）。
type kernelDPDKSetter struct{}

func (kernelDPDKSetter) SetDPDKBound(context.Context, string, bool, string) (string, string, error) {
	return "", "", ErrKernelDataPlane
}

// stateRuntimeFor 选择运行态聚合的数据源：内核数据面无 VPP stats segment，返回 nil
// （state.New(nil) 的方法安全返回空，读视图如实省略运行态字段）。
func stateRuntimeFor(mode string, mgr *network.Manager) state.Runtime {
	if mode == model.DataPlaneKernel {
		return nil
	}
	return mgr.Runtime()
}

// diagRuntimeFor 选择诊断实现：内核数据面用宿主网络栈的 ping/traceroute（经 `ip vrf exec`
// 支持 VRF 作用域）。
func diagRuntimeFor(mode string, mgr *network.Manager) api.DiagRuntime {
	if mode == model.DataPlaneKernel {
		return netkernel.NewDiag(nil)
	}
	return &diagController{diag: mgr.Diagnostics()}
}
