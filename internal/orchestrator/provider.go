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

// ErrIfaceUnavailable 配置引用的接口在 VPP 中不存在（未由 DPDK 接管、或已被 DPDK
// 接管但尚未加载进数据面、或被移除）。属**不可收敛项**：恢复收敛据此转 error 级告警
// （FR-OPS-010），提交阶段则据此判断能否**延后收敛**（决策 #100）。
//
// 定义在接口所在的本包（而非 network 包）：提交编排（apply.go）需要识别该状态来决定
// 「延后而不整体回滚」，而依赖方向不允许 orchestrator 反向 import network。
// network 包以 `ErrIfaceUnavailable = orchestrator.ErrIfaceUnavailable` 别名复用同一实例，
// 故各 provider 与 API 层原有的 `errors.Is` 判定不受影响（同一个 error 值）。
var ErrIfaceUnavailable = errors.New("接口在 VPP 中不存在")

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

// 容器运行态（契约 ContainerFunction.state；absent = 不存在）。
const (
	CTStateRunning = "running"
	CTStateExited  = "exited"
	CTStateDead    = "dead"
	CTStateAbsent  = "absent"
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
	// CheckVMAlarms 异常退出巡检（FR-CMP-017）：crashed → critical 告警，恢复则消警。
	CheckVMAlarms(ctx context.Context, cfg model.Config) []error
}

// ContainerProvider Docker 侧编排接口（memif socket 挂载，FR-NET-022）。
//
// ApplyContainer 声明式且幂等：按 committed 配置创建/重建容器（镜像、CPU/内存限制、
// env/command/args、重启策略、memif socket 挂载）；DeleteContainer 级联删除容器与
// 其 VPP 侧 memif 接口由网络 Provider 负责。生命周期动作供 request 族命令直调。
type ContainerProvider interface {
	ApplyContainer(ctx context.Context, ct model.ContainerFunction) error
	DeleteContainer(ctx context.Context, name string) error
	StartContainer(ctx context.Context, name string) error
	StopContainer(ctx context.Context, name string) error
	RestartContainer(ctx context.Context, name string) error
	// ContainerState 返回契约枚举 running/exited/dead（不存在返回 absent）。
	ContainerState(ctx context.Context, name string) (string, error)
	// ContainerLogs 返回最近 tail 行 stdout/stderr。
	ContainerLogs(ctx context.Context, name string, tail int) (string, error)
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
	// CheckContainerAlarms 异常退出巡检（FR-CMP-022）：dead/非零退出 → critical 告警。
	CheckContainerAlarms(ctx context.Context, cfg model.Config) []error
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
func (noopCompute) CheckVMAlarms(context.Context, model.Config) []error { return nil }

// NewNoopContainer 空容器编排（M4 替换为 Docker 实现）。
func NewNoopContainer() ContainerProvider { return noopContainer{} }

type noopContainer struct{}

func (noopContainer) ApplyContainer(context.Context, model.ContainerFunction) error { return nil }
func (noopContainer) DeleteContainer(context.Context, string) error                 { return nil }
func (noopContainer) StartContainer(context.Context, string) error                  { return nil }
func (noopContainer) StopContainer(context.Context, string) error                   { return nil }
func (noopContainer) RestartContainer(context.Context, string) error                { return nil }
func (noopContainer) ContainerState(context.Context, string) (string, error) {
	return CTStateAbsent, nil
}
func (noopContainer) ContainerLogs(context.Context, string, int) (string, error) { return "", nil }
func (noopContainer) EnsureConsistent(context.Context, model.Config) []error     { return nil }
func (noopContainer) CheckContainerAlarms(context.Context, model.Config) []error { return nil }
