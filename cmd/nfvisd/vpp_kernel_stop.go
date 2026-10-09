package main

// 数据面切到内核后的运行期收尾：把不再被产品使用的 VPP 停掉（决策 #426①）。
//
// 由来（真机走查）：`set system dataplane kernel` + 重启 nfvis 只换了 nfvisd 的装配，
// **不会**停 `vpp.service`——它会继续以 vfio-pci 握着两块业务口。此时操作者按手册
// 手工把网卡交还内核（对 sysfs 写 driver_override/bind）会**挂死**（VPP 持着 vfio group），
// 只有 `systemctl stop vpp` 之后网卡才回到内核驱动。产品不再使用 VPP，就该在切换生效
// （nfvisd 启动、装配判定 dataplane=kernel）时把它停掉。
//
// 口径与安装器 `stop_vpp_for_kernel`（deploy/offline/install.sh）同源：
//   - **只停不卸**、不改启用状态（不 mask、不 disable、不 purge）；
//   - VPP 未装 / 未运行 → 安静跳过（一行 INFO，不动手）；
//   - 停完**回读确认**（写成功 ≠ 收敛）；失败如实 Warn 并给自查路径。
//
// 不引入新的启动依赖：该分支只在 dataplane=kernel 下执行，而该模式下 nfvisd 本就连不上、
// 也不需要 VPP（连通路径全部走内核），停 VPP 只是收尾。整个动作有界（best-effort），
// 失败不阻塞启动。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/system"
)

// vppKernelStopTimeout 「内核数据面下停 VPP」的硬上界。
//
// 这一步属启动收尾：systemd 停一个用户态进程正常应在秒级完成；给 30s 是防御异常
// （stop job 卡住）时不在启动序列里长等——超时即按失败如实告警，不阻塞启动。
const vppKernelStopTimeout = 30 * time.Second

// vppKernelStopOutcome 一次「内核数据面下停 VPP」的如实结局（供装配处按档落日志）。
//
// 四档互斥：未执行（mode 非 kernel）／单元不存在／未在运行（两者都安静跳过）／
// 停止已确认（Stopped）／未确认（Err != nil）。
type vppKernelStopOutcome struct {
	Mode       string // 数据面实现（非 kernel 时不做任何动作）
	UnitExists bool   // 系统里是否有 vpp.service
	WasActive  bool   // 停之前是否在运行
	Stopped    bool   // 停之后是否**回读确认**已不再 active
	Err        error  // 未能确认停止的原因（含 stop 命令失败/回读仍 active）
}

// skipped 未走到「停」这个动作（mode 非 kernel / 未装 / 未运行）。
func (o vppKernelStopOutcome) skipped() bool {
	return o.Mode != model.DataPlaneKernel || !o.UnitExists || !o.WasActive
}

// stopVPPForKernelDataplane 在 dataplane=kernel 时停掉 vpp.service（只停不卸）。
//
// 与安装器 stop_vpp_for_kernel 逐条同源：
//  1. mode 非 kernel → 直接返回（不做任何 systemctl 调用）；
//  2. `systemctl list-unit-files vpp.service` 判单元是否存在（未装 → 安静跳过）；
//  3. `systemctl is-active vpp.service` 判是否在运行（未运行 → 安静跳过，不发 stop）；
//  4. `systemctl stop vpp.service` 后**回读** is-active：不再 active 才算停成
//     （命令成功 ≠ 收敛——systemd 的 stop 也可能落到仍 active 的残局）。
func stopVPPForKernelDataplane(ctx context.Context, mode string, run system.Runner) vppKernelStopOutcome {
	out := vppKernelStopOutcome{Mode: mode}
	if mode != model.DataPlaneKernel || run == nil {
		return out
	}
	// 单元是否存在：list-unit-files 查不到时退出码非零（同安装器判据）。
	if _, err := run(ctx, "systemctl", "list-unit-files", "vpp.service"); err != nil {
		return out
	}
	out.UnitExists = true
	if !vppUnitActive(ctx, run) {
		return out
	}
	out.WasActive = true
	if _, err := run(ctx, "systemctl", "stop", "vpp.service"); err != nil {
		out.Err = fmt.Errorf("systemctl stop vpp.service: %w", err)
		// stop 报错但回读已不在运行：按「已停成」如实记（systemd 的退出码可能来自 job 的
		// 收尾动作，而目标态已达成）——回读是唯一判据，与「写成功 ≠ 收敛」同一纪律。
		if !vppUnitActive(ctx, run) {
			out.Stopped, out.Err = true, nil
		}
		return out
	}
	if vppUnitActive(ctx, run) {
		out.Err = fmt.Errorf("systemctl stop vpp.service 执行后单元仍为 active")
		return out
	}
	out.Stopped = true
	return out
}

// vppUnitActive systemctl is-active vpp.service 是否为 active（读取失败按「不是 active」）。
func vppUnitActive(ctx context.Context, run system.Runner) bool {
	out, err := run(ctx, "systemctl", "is-active", "vpp.service")
	return err == nil && strings.TrimSpace(out) == "active"
}

// logVPPKernelStop 把收尾结局按档落日志（口径与安装器的 best-effort 一行提示同源）。
//
// 成功/跳过一律 INFO（这是预期路径，不是故障）；未能停成 Warn 并给自查路径
// （systemctl status vpp）——VPP 不参与内核数据面转发，但它握着业务口的 vfio group，
// 不交还内核前网卡没法用，操作者必须知道。
func logVPPKernelStop(log *slog.Logger, o vppKernelStopOutcome) {
	if log == nil || o.Mode != model.DataPlaneKernel {
		return
	}
	switch {
	case !o.UnitExists:
		log.Info("未找到 vpp.service，无需停止 VPP（本机数据面为 Linux 内核网络）", "unit", "vpp.service")
	case !o.WasActive:
		log.Info("VPP 未在运行，无需停止（本机数据面为 Linux 内核网络）", "unit", "vpp.service")
	case o.Stopped:
		log.Info("数据面已切到内核，产品不再管理 VPP，已停 vpp.service",
			"unit", "vpp.service", "note", "只停不卸：单元状态与启用状态均未改动；业务口需再执行 request interfaces <口> unbind-dpdk 交还内核")
	default:
		log.Warn("停止 vpp.service 未完成——它不参与内核数据面转发，但会以 vfio-pci 持有业务口，"+
			"网卡在它停止前无法交还内核",
			"unit", "vpp.service", "err", o.Err, "自查", "systemctl status vpp / journalctl -u vpp",
			"处置", "手工执行 systemctl stop vpp.service")
	}
}
