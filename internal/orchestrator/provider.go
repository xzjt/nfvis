// Package orchestrator 定义底座适配层接口（网络/计算/容器）。
//
// 依赖方向：config(事务引擎) → orchestrator(接口) → model；业务逻辑只依赖本包接口，
// 具体实现（govpp/libvirt/docker）在 M3/M4 落地，单元测试用 mock（骨架 §3.2/§4）。
package orchestrator

import (
	"context"
	"errors"

	"github.com/xzjt/nfvis/internal/model"
)

// ErrVMNotFound 目标 VM/domain 未定义（生命周期动作返回，API 层映射 404）。
var ErrVMNotFound = errors.New("VM 未定义")

// NetworkProvider VPP 侧编排接口。L2 虚拟交换机 → bridge domain，
// L3 虚拟交换机 → VRF（规格书附录 B 映射）。实现需声明是否并发安全。
type NetworkProvider interface {
	ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error
	ApplyBond(ctx context.Context, bond model.Bond) error
	DeleteBond(ctx context.Context, name string) error
	ApplyLLDP(ctx context.Context, lldp *model.LldpConfig) error
	ApplyACL(ctx context.Context, acl model.Acl) error
	DeleteACL(ctx context.Context, name string) error
	ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error
	DeleteBridgeDomain(ctx context.Context, name string) error
	ApplyVRF(ctx context.Context, vrf model.Vrf) error
	DeleteVRF(ctx context.Context, name string) error
	ApplyNAT(ctx context.Context, nat model.NatConfig) error
	ApplySpan(ctx context.Context, pm model.PortMirroring) error
	DeleteSpan(ctx context.Context, name string) error
	ApplyQos(ctx context.Context, q model.QosPolicy) error
	DeleteQos(ctx context.Context, name string) error

	// ApplyVnfInterface 建立 VNF vNIC 的接入（FR-NET-020/021/023）：vhost-user 时
	// 在 VPP 侧建 socket 接口并命名（交换机端口随后按名挂接）；sriov-vf 不经 VPP。
	// 幂等：接口已存在则仅校正属性。
	ApplyVnfInterface(ctx context.Context, port VnfPort) error
	// DeleteVnfInterface 删除 vNIC 接入（VM 删除/vNIC 移除/迁移时同步 VPP 侧接口）。
	DeleteVnfInterface(ctx context.Context, vmName, ifaceName string) error

	// EnsureConsistent 恢复收敛（FR-OPS-010/011）：对比 committed 配置与 VPP 实际
	// 状态并补齐/修正，无法收敛的项以错误返回（由调用方转告警，不阻塞启动）。
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
}

// VM 运行态（与 OpenAPI VMFunction.state 枚举一致；absent = 未定义，用于
// 恢复收敛与删除前判定）。libvirt 状态到本枚举的映射见 compute.VMStateFromLibvirt。
const (
	VMStateRunning = "running"
	VMStateShutoff = "shutoff"
	VMStateCrashed = "crashed"
	VMStatePaused  = "paused"
	VMStateAbsent  = "absent"
)

// ComputeProvider libvirt/KVM 侧编排接口。
//
// DefineVM 为声明式且幂等：按 (vm, alloc) 组装 domain XML 并 DomainDefineXML
// （已存在则重定义）；autostart=true 时定义后启动（FR-CMP-010）。
// 生命周期动作（Start/Stop/Restart）不改变 committed 配置，供 request 族命令直调
// （FR-CMP-011）。运行中修改 vCPU/内存/vNIC 由 API 层按 FR-CMP-012 拒绝（409）。
// 资源分配（alloc）由 M4-2 账本从 committed 配置确定性重算后传入。
type ComputeProvider interface {
	DefineVM(ctx context.Context, vm model.VMFunction, alloc model.AllocatedResources) error
	DeleteVM(ctx context.Context, name string) error
	StartVM(ctx context.Context, name string) error
	StopVM(ctx context.Context, name string) error
	RestartVM(ctx context.Context, name string) error
	VMState(ctx context.Context, name string) (string, error)

	// EnsureConsistent 恢复收敛（FR-OPS-010/012）：对比 committed 配置与实际
	// domain，补建缺失对象；无法收敛项以错误返回（调用方转告警，不阻塞启动）。
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
}

// ContainerProvider Docker 侧编排接口（memif socket 挂载，FR-NET-022）。
type ContainerProvider interface {
	ApplyContainer(ctx context.Context, ct model.ContainerFunction) error
	DeleteContainer(ctx context.Context, name string) error
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
}

// NewNoopNetwork M2/M3 过渡用空网络实现：所有下发成功、恢复收敛为空集。
// M3 以 network.L2Network 等装饰器覆盖已实现的方法（先 noop 再逐层替换）。
func NewNoopNetwork() NetworkProvider { return noopNetwork{} }

type noopNetwork struct{}

func (noopNetwork) ApplyInterface(context.Context, model.InterfaceConfig) error  { return nil }
func (noopNetwork) ApplyACL(context.Context, model.Acl) error                    { return nil }
func (noopNetwork) ApplyBond(context.Context, model.Bond) error                  { return nil }
func (noopNetwork) DeleteBond(context.Context, string) error                     { return nil }
func (noopNetwork) ApplyLLDP(context.Context, *model.LldpConfig) error           { return nil }
func (noopNetwork) DeleteACL(context.Context, string) error                      { return nil }
func (noopNetwork) ApplyBridgeDomain(context.Context, model.VirtualSwitch) error { return nil }
func (noopNetwork) DeleteBridgeDomain(context.Context, string) error             { return nil }
func (noopNetwork) ApplyVRF(context.Context, model.Vrf) error                    { return nil }
func (noopNetwork) DeleteVRF(context.Context, string) error                      { return nil }
func (noopNetwork) ApplyNAT(context.Context, model.NatConfig) error              { return nil }
func (noopNetwork) ApplySpan(context.Context, model.PortMirroring) error         { return nil }
func (noopNetwork) DeleteSpan(context.Context, string) error                     { return nil }
func (noopNetwork) ApplyQos(context.Context, model.QosPolicy) error              { return nil }
func (noopNetwork) DeleteQos(context.Context, string) error                      { return nil }
func (noopNetwork) ApplyVnfInterface(context.Context, VnfPort) error             { return nil }
func (noopNetwork) DeleteVnfInterface(context.Context, string, string) error     { return nil }
func (noopNetwork) EnsureConsistent(context.Context, model.Config) []error       { return nil }

// NewNoopCompute 空计算编排（M4 替换为 libvirt 实现；M4-3 起真实实现接入 nfvisd）。
func NewNoopCompute() ComputeProvider { return noopCompute{} }

type noopCompute struct{}

func (noopCompute) DefineVM(context.Context, model.VMFunction, model.AllocatedResources) error {
	return nil
}
func (noopCompute) DeleteVM(context.Context, string) error          { return nil }
func (noopCompute) StartVM(context.Context, string) error           { return nil }
func (noopCompute) StopVM(context.Context, string) error            { return nil }
func (noopCompute) RestartVM(context.Context, string) error         { return nil }
func (noopCompute) VMState(context.Context, string) (string, error) { return VMStateAbsent, nil }
func (noopCompute) EnsureConsistent(context.Context, model.Config) []error {
	return nil
}

// NewNoopContainer 空容器编排（M4 替换为 Docker 实现）。
func NewNoopContainer() ContainerProvider { return noopContainer{} }

type noopContainer struct{}

func (noopContainer) ApplyContainer(context.Context, model.ContainerFunction) error { return nil }
func (noopContainer) DeleteContainer(context.Context, string) error                 { return nil }
func (noopContainer) EnsureConsistent(context.Context, model.Config) []error        { return nil }
