package main

// 底座编排的动态持有层（决策 #351）。
//
// nfvisd 装配期对 libvirt/Docker 各做一次既有 10s 有界尝试，失败即降级——此前的降级是
// **永久性**的（Noop 语义接到进程退出，决策 #349）：开机后 libvirtd 完成 autostart 的
// ≈90s 窗口（round130 实测）里每一次 nfvisd 启动落到降级，VM/容器编排都要人工
// `systemctl restart nfvis` 才恢复（R129-2）。#351 在 cmd/nfvisd 内加一层读写锁持有层：
// 未接入时按 Noop 语义应答，后台接入循环成功后**原子换装**为真实 Provider——消费方
//（applier/巡检/恢复收敛/API 控制器）装配期一次性接线，运行期调用即转发当前实现。
//
// 未接入语义表（与今天 nil/Noop 行为逐法对齐；逐法断言见 dynamic_provider_test.go）：
//   - DefineVM/DeleteVM、ApplyContainer/DeleteContainer ⇒ 静默成功（提交路径在降级期
//     照常工作，接入成功后由 EnsureConsistent 补建）；
//   - EnsureConsistent/CheckVMAlarms/CheckContainerAlarms ⇒ 空结果（巡检/恢复收敛不产生噪声）；
//   - VMState/ContainerState ⇒ 错误（show 路径经既有错误分支照旧渲染「-」）；
//   - 其余生命周期动作/快照/日志/console ⇒ 与 internal/api nil 分支逐字同文案的错误。
//
// 决策 #354 在持有层之上加了常驻探活/复连状态机（async_connect.go）；持有层新增
// Probe（经既有廉价 RPC 探活，见其注释），Swap 记账同时服务首接与复连两种换装。

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/xzjt/nfvis/internal/api"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
)

// 未接入时的操作者可见正文（与 internal/api 的 nil 分支文案逐字对齐——api 侧以
// "%% " + 正文 + "\n" 渲染，REST 侧 writeError 透传正文）：
//   - VM 生命周期/快照：internal/api 的 errComputeUnavailable
//     = "%% 计算编排未接入（libvirt 未装配），运行态不可用\n"（快照 nil 分支同用该常量）；
//   - console：requestVMConsole 的 nil 分支 = "%% 串口 console 不可用（libvirt 未装配）\n"；
//   - 容器：requestContainer/containerLog 的 nil 分支
//     = "%% 容器编排未接入（Docker 未装配），运行态不可用\n"。
const (
	errComputeNotConnectedText   = "计算编排未接入（libvirt 未装配），运行态不可用"
	errConsoleNotConnectedText   = "串口 console 不可用（libvirt 未装配）"
	errContainerNotConnectedText = "容器编排未接入（Docker 未装配），运行态不可用"
)

var (
	errComputeNotConnected   = errors.New(errComputeNotConnectedText)
	errConsoleNotConnected   = errors.New(errConsoleNotConnectedText)
	errContainerNotConnected = errors.New(errContainerNotConnectedText)
)

// computeFacade 持有层所持的 libvirt 编排能力集合：orchestrator.ComputeProvider 全部 9 法，
// 加上 api 控制器消费的超集——StartVMChecked（决策 #311 的启动回读）、Console（M4-5）、
// 快照 4 法（M4-6）。抽成小接口是为了单测能注入假件（*compute.Provider 是绑定真实
// libvirt 连接的具体类型，测试环境造不出）；生产路径持有的仍是 *compute.Provider。
type computeFacade interface {
	orchestrator.ComputeProvider
	StartVMChecked(ctx context.Context, name string) (orchestrator.VMStartProbe, error)
	Console(ctx context.Context, name string) (io.ReadWriteCloser, error)
	SnapshotCreate(ctx context.Context, domain, name, description string) error
	Snapshots(ctx context.Context, domain string) ([]compute.SnapshotInfo, error)
	SnapshotRevert(ctx context.Context, domain, name string) error
	SnapshotDelete(ctx context.Context, domain, name string) error
}

// containerFacade 容器侧同理（orchestrator.ContainerProvider 的 9 法已含
// api.ContainerRuntime 的全部 5 法，无须再加方法）。
type containerFacade interface {
	orchestrator.ContainerProvider
}

// dynamicCompute libvirt 编排的动态持有层。零值即「未接入」——按语义表应答；
// Swap 后全部方法转发真实实现。
type dynamicCompute struct {
	mu   sync.RWMutex
	p    computeFacade
	conn *compute.Conn // 持有层记账的 libvirt 连接（进程优雅停机时统一关闭）
}

func newDynamicCompute() *dynamicCompute { return &dynamicCompute{} }

// Swap 原子换装：写入真实 Provider 并接管其连接的记账（含关闭职责）。生产路径换装
// 至多一次（本决策只服务「从未接入」状态）；防御性地先关旧连接，避免覆盖泄漏。
func (h *dynamicCompute) Swap(p computeFacade, conn *compute.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		_ = h.conn.Close()
	}
	h.p, h.conn = p, conn
}

// Connected 是否已接入真实编排。
func (h *dynamicCompute) Connected() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.p != nil
}

// Close 关闭持有层记账的 libvirt 连接（同步接入与后台接入两条路统一由此关闭，
// 取代原先只在同步成功路径上的 defer libvirtConn.Close()）。未接入时无连接，返回 nil。
func (h *dynamicCompute) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return nil
	}
	err := h.conn.Close()
	h.conn = nil
	return err
}

// current 取当前实现（nil = 未接入）。各方法经它取读锁快照，保证与 Swap 互斥。
func (h *dynamicCompute) current() computeFacade {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.p
}

// computeProbeVMName 探活使用的保留域名（决策 #354）：libvirt 对查无此域返回
// 「不存在」而非错误——正常返回即证明连接可用（与 Docker 侧 "__nfvis_probe__" 同一手法）。
const computeProbeVMName = "__nfvis_probe__"

// Probe 探活（决策 #354 契约②）：经当前实现的一个既有廉价 RPC 判定 libvirt 连接是否
// 健康；nil = 健康，非 nil = 本次探活失败（调用方按连续失败阈值判「连接中断」）。
//
// 方法选择：*compute.Provider 没有 Conn.Version() 的直接透传（Conn 由持有层记账、
// Provider 不暴露），本决策又不改 compute 包内部——故取既有最廉价 RPC 之一
// VMState(ctx, 保留域名)：一次 DomainLookupByName（查无此域 ⇒ absent/nil，正常返回），
// 与 Version() 同级开销、只读、无副作用。未接入态经 VMState 返回既有「未接入」错误，
// 同样计为探活失败（防御性——探活只在已接入态被调用）。
func (h *dynamicCompute) Probe(ctx context.Context) error {
	if _, err := h.VMState(ctx, computeProbeVMName); err != nil {
		return err
	}
	return nil
}

// DefineVM 未接入 ⇒ 静默成功：提交路径在降级期照常工作（配置入库、网络侧照常下发），
// 接入成功后的 EnsureConsistent 会补建缺失 domain。
func (h *dynamicCompute) DefineVM(ctx context.Context, vm model.VMFunction, alloc model.AllocatedResources) error {
	if p := h.current(); p != nil {
		return p.DefineVM(ctx, vm, alloc)
	}
	return nil
}

// DeleteVM 未接入 ⇒ 静默成功（同 DefineVM：提交路径照常，补建/清理由接入后的收敛负责）。
func (h *dynamicCompute) DeleteVM(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.DeleteVM(ctx, name)
	}
	return nil
}

// StartVM 未接入 ⇒ 与 internal/api nil 分支同文案的错误。
func (h *dynamicCompute) StartVM(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.StartVM(ctx, name)
	}
	return errComputeNotConnected
}

// StopVM 未接入 ⇒ 与 internal/api nil 分支同文案的错误。
func (h *dynamicCompute) StopVM(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.StopVM(ctx, name)
	}
	return errComputeNotConnected
}

// RestartVM 未接入 ⇒ 与 internal/api nil 分支同文案的错误。
func (h *dynamicCompute) RestartVM(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.RestartVM(ctx, name)
	}
	return errComputeNotConnected
}

// StartVMChecked 未接入 ⇒ 与 nil 分支同文案的错误（零值 probe 一并返回）。
func (h *dynamicCompute) StartVMChecked(ctx context.Context, name string) (orchestrator.VMStartProbe, error) {
	if p := h.current(); p != nil {
		return p.StartVMChecked(ctx, name)
	}
	return orchestrator.VMStartProbe{}, errComputeNotConnected
}

// RefreshSeed 未接入 ⇒ 与 nil 分支同文案的错误（CLI start/restart 前置重建 seed）。
func (h *dynamicCompute) RefreshSeed(ctx context.Context, vm model.VMFunction) error {
	if p := h.current(); p != nil {
		return p.RefreshSeed(ctx, vm)
	}
	return errComputeNotConnected
}

// VMState 未接入 ⇒ 错误：show 路径（vmStateOf/vmStateSafe）经既有错误分支渲染「-」/空，
// 与今天 x.vm == nil 的显示一致。
func (h *dynamicCompute) VMState(ctx context.Context, name string) (string, error) {
	if p := h.current(); p != nil {
		return p.VMState(ctx, name)
	}
	return "", errComputeNotConnected
}

// Console 未接入 ⇒ 与 requestVMConsole nil 分支同文案的错误
// （WebSocket 侧经 handleConsoleWS 渲染「console 打开失败: …」）。
func (h *dynamicCompute) Console(ctx context.Context, name string) (io.ReadWriteCloser, error) {
	if p := h.current(); p != nil {
		return p.Console(ctx, name)
	}
	return nil, errConsoleNotConnected
}

// SnapshotCreate 未接入 ⇒ 与快照 nil 分支（errComputeUnavailable 正文）同文案的错误。
func (h *dynamicCompute) SnapshotCreate(ctx context.Context, domain, name, description string) error {
	if p := h.current(); p != nil {
		return p.SnapshotCreate(ctx, domain, name, description)
	}
	return errComputeNotConnected
}

// Snapshots 未接入 ⇒ 与快照 nil 分支同文案的错误。
func (h *dynamicCompute) Snapshots(ctx context.Context, domain string) ([]compute.SnapshotInfo, error) {
	if p := h.current(); p != nil {
		return p.Snapshots(ctx, domain)
	}
	return nil, errComputeNotConnected
}

// SnapshotRevert 未接入 ⇒ 与快照 nil 分支同文案的错误。
func (h *dynamicCompute) SnapshotRevert(ctx context.Context, domain, name string) error {
	if p := h.current(); p != nil {
		return p.SnapshotRevert(ctx, domain, name)
	}
	return errComputeNotConnected
}

// SnapshotDelete 未接入 ⇒ 与快照 nil 分支同文案的错误。
func (h *dynamicCompute) SnapshotDelete(ctx context.Context, domain, name string) error {
	if p := h.current(); p != nil {
		return p.SnapshotDelete(ctx, domain, name)
	}
	return errComputeNotConnected
}

// EnsureConsistent 未接入 ⇒ 空结果：恢复收敛在降级期不产生噪声。
func (h *dynamicCompute) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	if p := h.current(); p != nil {
		return p.EnsureConsistent(ctx, cfg)
	}
	return nil
}

// CheckVMAlarms 未接入 ⇒ 空结果：15s 巡检在降级期不产生噪声。
func (h *dynamicCompute) CheckVMAlarms(ctx context.Context, cfg model.Config) []error {
	if p := h.current(); p != nil {
		return p.CheckVMAlarms(ctx, cfg)
	}
	return nil
}

// containerHolder Docker 编排的动态持有层（容器侧没有独立连接对象，Provider 即全部）。
type containerHolder struct {
	mu sync.RWMutex
	p  containerFacade
}

func newContainerHolder() *containerHolder { return &containerHolder{} }

// Swap 原子换装（同 dynamicCompute.Swap 的口径；容器侧无连接记账）。
func (h *containerHolder) Swap(p containerFacade) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p = p
}

// Connected 是否已接入真实编排。
func (h *containerHolder) Connected() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.p != nil
}

// current 取当前实现（nil = 未接入）。
func (h *containerHolder) current() containerFacade {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.p
}

// ApplyContainer 未接入 ⇒ 静默成功（提交路径照常工作；noopContainer 现状即如此）。
func (h *containerHolder) ApplyContainer(ctx context.Context, ct model.ContainerFunction) error {
	if p := h.current(); p != nil {
		return p.ApplyContainer(ctx, ct)
	}
	return nil
}

// DeleteContainer 未接入 ⇒ 静默成功（同 ApplyContainer）。
func (h *containerHolder) DeleteContainer(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.DeleteContainer(ctx, name)
	}
	return nil
}

// StartContainer 未接入 ⇒ 与 internal/api nil 分支同文案的错误。
func (h *containerHolder) StartContainer(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.StartContainer(ctx, name)
	}
	return errContainerNotConnected
}

// StopContainer 未接入 ⇒ 与 internal/api nil 分支同文案的错误。
func (h *containerHolder) StopContainer(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.StopContainer(ctx, name)
	}
	return errContainerNotConnected
}

// RestartContainer 未接入 ⇒ 与 internal/api nil 分支同文案的错误。
func (h *containerHolder) RestartContainer(ctx context.Context, name string) error {
	if p := h.current(); p != nil {
		return p.RestartContainer(ctx, name)
	}
	return errContainerNotConnected
}

// ContainerState 未接入 ⇒ 错误：ctStateOf 经既有错误分支渲染「-」，与今天 x.ct == nil 一致。
func (h *containerHolder) ContainerState(ctx context.Context, name string) (string, error) {
	if p := h.current(); p != nil {
		return p.ContainerState(ctx, name)
	}
	return "", errContainerNotConnected
}

// ContainerLogs 未接入 ⇒ 与 containerLog nil 分支同文案的错误。
func (h *containerHolder) ContainerLogs(ctx context.Context, name string, tail int) (string, error) {
	if p := h.current(); p != nil {
		return p.ContainerLogs(ctx, name, tail)
	}
	return "", errContainerNotConnected
}

// EnsureConsistent 未接入 ⇒ 空结果（恢复收敛不产生噪声）。
func (h *containerHolder) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	if p := h.current(); p != nil {
		return p.EnsureConsistent(ctx, cfg)
	}
	return nil
}

// CheckContainerAlarms 未接入 ⇒ 空结果（15s 巡检不产生噪声）。
func (h *containerHolder) CheckContainerAlarms(ctx context.Context, cfg model.Config) []error {
	if p := h.current(); p != nil {
		return p.CheckContainerAlarms(ctx, cfg)
	}
	return nil
}

// 编译期断言：生产实现满足能力面、消费方满足各自契约。
var (
	_ computeFacade                  = (*compute.Provider)(nil)
	_ containerFacade                = (*container.Provider)(nil)
	_ orchestrator.ComputeProvider   = (*dynamicCompute)(nil)
	_ orchestrator.ContainerProvider = (*containerHolder)(nil)
	_ api.VMRuntime                  = (*vmController)(nil)
	_ api.VMConsoleRuntime           = (*dynamicCompute)(nil)
	_ api.VMSnapshotRuntime          = (*snapshotController)(nil)
	_ api.ContainerRuntime           = (*containerHolder)(nil)
)
