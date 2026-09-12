// Package orchestrator 定义底座适配层接口（网络/计算/容器）。
//
// 依赖方向：config(事务引擎) → orchestrator(接口) → model；业务逻辑只依赖本包接口，
// 具体实现（govpp/libvirt/docker）在 M3/M4 落地，单元测试用 mock（骨架 §3.2/§4）。
package orchestrator

import (
	"context"

	"github.com/xzjt/nfvis/internal/model"
)

// NetworkProvider VPP 侧编排接口。L2 虚拟交换机 → bridge domain，
// L3 虚拟交换机 → VRF（规格书附录 B 映射）。实现需声明是否并发安全。
type NetworkProvider interface {
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

	// EnsureConsistent 恢复收敛（FR-OPS-010/011）：对比 committed 配置与 VPP 实际
	// 状态并补齐/修正，无法收敛的项以错误返回（由调用方转告警，不阻塞启动）。
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
}

// ComputeProvider libvirt/KVM 侧编排接口。
// 资源账本分配（FR-CMP-002）由事务引擎 commit 阶段完成后传入 M3 实现。
type ComputeProvider interface {
	DefineVM(ctx context.Context, vm model.VMFunction) error
	DeleteVM(ctx context.Context, name string) error
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
}

// ContainerProvider Docker 侧编排接口（memif socket 挂载，FR-NET-022）。
type ContainerProvider interface {
	ApplyContainer(ctx context.Context, ct model.ContainerFunction) error
	DeleteContainer(ctx context.Context, name string) error
	EnsureConsistent(ctx context.Context, cfg model.Config) []error
}
