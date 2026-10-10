package main

// 数据面相关的装配选择（v3 决策 #404）。
//
// 大部分运行态读物由 netRuntime 的两种实现**同形提供**（VPP 给真值、内核给如实不可用），
// 装配处直接传 netProvider 即可。本文件只处理**方法面不同**的几处：VPP 控制面、DPDK 接管、
// 诊断、运行态聚合——内核数据面下它们要么换成如实报「不支持」的实现，要么退化为空。
//
// 例外是 DPDK 接管（决策 #426②）：内核数据面下**绑定**拒绝，但**解绑**照常可用——
// 它是「把网卡交还内核」的唯一产品内路径（VPP 时代接管过的口会留在 vfio-pci）。

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

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

// dpdkSetterFor 选择 DPDK 接管实现：内核数据面下物理口留在内核，**绑定**没有意义；
// 但**解绑**照常可用（决策 #426②：它正是「把网卡交还内核」的动作——VPP 时代接管过的口
// 在切到内核数据面后仍留在 vfio-pci，不交还内核里就没有它）。
//
// kernelInUse 内核数据面的「该口仍在用吗」探测（nil = 不拦），装配处接 netkernel 的
// IfaceInUse；VPP 数据面走自己的 VPP 侧探测，二者互斥、由本函数按 mode 分派。
func dpdkSetterFor(mode string, binder *network.DPDKBinder, rec *network.Bindings,
	log *slog.Logger, facts func() network.ManagementFacts, mgr *network.Manager,
	kernelInUse func(ctx context.Context, ifname string) (bool, string, error)) api.DPDKSetter {
	if mode == model.DataPlaneKernel {
		return kernelDPDKSetter{b: binder, rec: rec, log: log, facts: facts, inUse: kernelInUse}
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

// errKernelDPDKBind 内核数据面下不支持 bind-dpdk（决策 #426②）。
//
// 内核数据面不使用 DPDK 接管（物理口留在内核），绑定只会把网卡从内核里拿走、把数据面打瘸；
// 方向必须与 unbind 分开处置——后者在内核下恰恰是需要的动作。
var errKernelDPDKBind = errors.New("当前数据面为 Linux 内核网络，不支持 bind-dpdk（内核数据面不使用 DPDK 接管，绑定会把网卡从内核里拿走）；" +
	"如需 DPDK 接管请改回 VPP 数据面（set system dataplane vpp，提交后重启 nfvis 服务）")

// kernelDPDKSetter 内核数据面下的 DPDK 驱动接管面（决策 #426②）。
//
// 只有一处与 VPP 侧不同：bind 拒绝（见 errKernelDPDKBind）。**unbind 照常执行**——
// 写路径与守卫与 VPP 侧同源（管理口一律拒、解绑前「仍在用」检查、绑定记录按 PCI 清理、
// 按 PCI 回读驱动），差别只在「仍在用」的判据问的是内核而不是 VPP。
type kernelDPDKSetter struct {
	b     *network.DPDKBinder
	rec   *network.Bindings
	log   *slog.Logger
	facts func() network.ManagementFacts
	// inUse 探测「该口此刻是否正被内核数据面使用」；nil = 无法探测（不拦，与 VPP 侧同取向）。
	inUse func(ctx context.Context, ifname string) (bool, string, error)
}

// SetDPDKBound 绑定一律拒绝；解绑执行（交还内核驱动）。
func (k kernelDPDKSetter) SetDPDKBound(ctx context.Context, ifname string, bound bool, driver string) (string, string, error) {
	// 管理口守卫（两方向，与 VPP 侧同一实现）：绑定/解绑都会中断 SSH 与管理 API。
	if err := checkManagementIface(k.facts, k.rec, ifname); err != nil {
		return "", "", err
	}
	if bound {
		return "", "", errKernelDPDKBind
	}
	return dpdkUnbind(ctx, k.b, k.rec, k.log, ifname, driver, func() error {
		return k.checkNotInKernelDataplane(ctx, ifname)
	})
}

// checkNotInKernelDataplane 解绑前确认该口已不被内核数据面使用（决策 #426②）。
//
// 目标与 VPP 侧同口径：可能是 PCI 地址（接管后内核已无 netdev）→ 先解析成口名
// （内核 netdev 名或绑定记录），解析不出按「未知」处理、不拦。
//
// 探测不到（口在内核里不存在——DPDK 残留的正常形态、或 ip 工具不可用）**不拦**：
// 与 VPP 侧同取向，探测通道不通不代表口在被使用；而「口还在被 bridge/bond/VRF 用着」
// 这种真占用会由探测明确报出。
func (k kernelDPDKSetter) checkNotInKernelDataplane(ctx context.Context, ifname string) error {
	if k.inUse == nil {
		return nil
	}
	name, ok := network.ResolveIfaceName(ifname, k.rec)
	if !ok {
		return nil
	}
	inUse, why, err := k.inUse(ctx, name)
	if err != nil {
		if k.log != nil {
			k.log.Warn("无法探测接口是否被内核数据面使用，跳过解绑守卫", "ifname", name, "err", err)
		}
		return nil
	}
	return network.CheckKernelUnbindAllowed(name, inUse, why)
}

// checkManagementIface 管理口守卫（发现 #7 / 决策 #101）：**先于任何 sysfs 动作**判定，
// 命中即拒绝。目标可能是 PCI 地址（接管后内核已无 netdev）→ 先解析成口名（内核 netdev 名
// 或绑定记录），解析不出按「未知」处理、不拦（理由见 ResolveIfaceName 注释）。
//
// 抽成自由函数供两种数据面的接管实现共用：守卫必须与数据面无关（管理口就是管理口）。
func checkManagementIface(facts func() network.ManagementFacts, rec *network.Bindings, target string) error {
	if facts == nil {
		return nil
	}
	name, ok := network.ResolveIfaceName(target, rec)
	if !ok {
		return nil
	}
	return network.CheckManagementPort(name, facts())
}

// dpdkUnbind DPDK 解绑的公共写路径（两种数据面共用）：
// 解绑前守卫 → sysfs 解绑交还内核驱动 → 按 PCI 清理绑定记录 → 按 PCI 回读驱动。
//
// guard 为解绑前的「仍在用」守卫（nil = 不判）；它返回错误即中断（不触任何 sysfs 写）。
func dpdkUnbind(ctx context.Context, b *network.DPDKBinder, rec *network.Bindings, log *slog.Logger,
	ifname, toDriver string, guard func() error) (string, string, error) {
	if guard != nil {
		if err := guard(); err != nil {
			return "", "", err
		}
	}
	// driver 在解绑语义下表示「交还给哪个内核驱动」（缺省由内核自动探测）
	pci, err := b.Unbind(ctx, ifname, toDriver)
	if err != nil {
		return "", "", err
	}
	// 绑定记录（决策 #100）：按 PCI 删除（操作者给的常是 PCI 地址）。记录失败只影响后续生成。
	if rec != nil {
		if rerr := rec.DeleteByPCI(pci); rerr != nil && log != nil {
			log.Warn("更新 DPDK 绑定记录失败（数据面重启可能需要重新解析端口）",
				"ifname", ifname, "pci", pci, "err", rerr)
		}
	}
	return pci, readbackDPDKDriver(b, pci), nil
}

// readbackDPDKDriver 按 **PCI** 回读驱动名：绑定到 DPDK 后内核网卡即消失，
// 按接口名解析会失败并把结果误报为「无驱动」（真机实测踩到）；解绑后内核驱动
// 重新探测需要一点时间，故轮询等待。
func readbackDPDKDriver(b *network.DPDKBinder, pci string) string {
	var cur string
	for i := 0; i < 15; i++ {
		if cur, _ = b.DriverOf(pci); cur != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return cur
}

// dpdkHeldPortProbe 「该口仍被某个驱动占用」的探测（决策 #426③的装配实现）。
//
// 判据三连：口**不在**内核网卡清单里（在内核里的口没有「交还」这回事）→ 能按口名/PCI
// 解析到设备地址（无 netdev 时靠绑定记录，决策 #100）→ 当前绑定着某个驱动。三者都成立
// 才点名；任何一步取不到就 ok=false（不猜——恢复收敛退回底座原文）。
//
// kernelIfnames 可注入（装配传 network.KernelIfnamesAll，单测传假清单）。
func dpdkHeldPortProbe(b *network.DPDKBinder, kernelIfnames func() ([]string, error)) func(ifname string) (string, string, bool) {
	return func(ifname string) (string, string, bool) {
		if b == nil || strings.TrimSpace(ifname) == "" {
			return "", "", false
		}
		if names, err := kernelIfnames(); err == nil {
			for _, n := range names {
				if n == ifname {
					return "", "", false // 在内核里：没有「交还」这回事
				}
			}
		} else {
			return "", "", false // 内核清单读不到：不给结论
		}
		pci, err := b.PCIAddrOf(ifname)
		if err != nil {
			return "", "", false
		}
		drv, err := b.DriverOf(pci)
		if err != nil || drv == "" {
			return "", "", false
		}
		return drv, pci, true
	}
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

// attachSRIOV 构造 SR-IOV VF 数量编排并注入 VPP 侧网络实现（v3 round2 体检 R2-7）。
//
// 构造与注入**一体**是刻意的：旧装配在 VPP 分支里先 `l2net.SetSRIOV(sriovProvider)`
// （此时变量还是 nil），到 if/else 之后才 `sriovProvider = network.NewSRIOVProvider()`
// ——VPP（缺省）数据面拿到 nil，声明式 `interfaces … sriov vf-count N` 从「可下发」变成
// 硬失败并回滚（v2 回归）。收敛到本函数后「先构造后注入」由控制流保证，装配处不再有裸调用。
//
// l2net 为 nil（内核数据面）时只返回实例：API 层的命令式 VF 路径与内核 Provider 共用同一
// sysfs 实现（VF 数量与数据面无关，内核数据面下 VF 直通本就是内核能力）。
func attachSRIOV(l2net *network.L2Network) *network.SRIOVProvider {
	p := network.NewSRIOVProvider()
	if l2net != nil {
		l2net.SetSRIOV(p)
	}
	return p
}

// netReconcileFor 选择「按已提交声明整段重放」的收敛入口（决策 #449 扩展）。
//
// 内核数据面注入 netProvider：它的 EnsureConsistent 就是 nfvisd 启动时的恢复收敛本身
// （按 committed 全量重放、幂等），故 `request interfaces <n> enable|disable` 与
// `unbind-dpdk` 在成功后调它一次，把「启用/交还后按声明收敛」落到**引用该口的整段**
// （L2 交换机段：成员 + VLAN；L3 交换机 VRF 段：master + 地址 + 域兜底路由），而不是只
// 补一句 master——真机缺口：稳态下 enable 是空修订、apply 段根本不跑，口起来了而地址与
// 兜底仍缺（命令报成功、数据面没效果）。
//
// VPP 数据面**不注入**（nil = 不触发），理由三条：
//   - VPP 侧的恢复收敛是「重连/数据面重启」语义（先 resetProviders 再按 committed 全量重下发）；
//   - VPP 模式下 enable 的值变更本就由提交 apply 下发，值未变时口已在 VPP 且状态即声明态；
//   - 逐口调用 VPP 侧 ApplyInterface 在「口已交还内核」时会以 ErrIfaceUnavailable 打断一个
//     合法动作（交还后 VPP 里本来就没有该口）。
//
// 若将来 VPP 侧也要这一步，注入同一个对象即可（api 侧只按「能力是否接入」行事，不认数据面）。
func netReconcileFor(mode string, rec api.NetReconcileRuntime) api.NetReconcileRuntime {
	if mode != model.DataPlaneKernel || rec == nil {
		return nil
	}
	return rec
}

// configSource 「当前 committed 配置」的来源（*config.Engine 的 Committed 即实现）。
type configSource interface {
	Committed() (model.Config, error)
}

// attachConfigSource 把当前 committed 配置的来源接到内核 Provider 的读视图上（真机 3.0.5~dev1 回归）。
//
// 内核数据面的读视图（BridgeDomains / VPPIfnames / 产品自持设备判定）按**配置声明**枚举，
// 而进程内快照只在装配与恢复收敛时写入——提交路径不经过 Provider（Apply* 只拿到单个对象），
// 于是提交后的读视图滞留旧快照：新建交换机提交成功、`show virtual-switches` 恒空，15s 巡检
// 也不刷新（它只更新 EnsureForwarding 的入参）。接上来源后每次读视图都取当前 committed。
//
// 调用点必须在**引擎构造之后**：Provider 先于引擎创建，此前无处取 committed。
// 来源读失败时 Provider 回落自己的快照（不把「读不到配置」显示成「没有配置」）。
func attachConfigSource(kp *netkernel.Provider, src configSource) {
	if kp == nil || src == nil {
		return
	}
	kp.SetConfigSource(src.Committed)
}
