package container

// 容器侧 vNIC 接入的生命周期胶水（v3 决策 #441；契约：规格书附录 A #441、设计 §3.2f）。
//
// 分工（与设计文档逐字一致）：
//   - **宿主端**（确定性名的 veth、入交换机内核 bridge）由**网络编排**负责（内核 Provider 的
//     ApplyVnfInterface：按名建/删、入桥、按名核对复用）；
//   - **容器端**由**本包在容器生命周期里**驱动接入：start/restart 成功后按 PID 把容器端移入其
//     网络命名空间（容器 restart 会重建 netns，必须重新接入——否则容器网络静默失效）、
//     stop/delete 清宿主端。宿主端 veth 是内核对象：重放只按名核对/复用，**绝不重建**
//     （重建会打断运行中容器的网络）。
//
// 本文件只经注入的 ContainerNICHook 与既有 dockerAPI 缝隙动作：VPP 数据面**不注入钩子**
// ⇒ 所有接入路径都以「钩子在位且容器声明了 vNIC」为前置，memif 路径逐字不变。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ContainerNICHook 容器编排在容器生命周期里驱动「容器侧网络接入」的钩子（内核数据面注入；
// VPP 数据面 nil ⇒ memif 路径逐字不变）。
type ContainerNICHook interface {
	// Attach 确保该容器声明的 vNIC 全部就绪并进入 pid 的网络命名空间（幂等；容器 restart
	// 重建 netns 后必须重新调用）。
	Attach(ctx context.Context, owner string, ifaces []model.VnfInterface, pid int) error
	// Delete 删掉该容器的全部宿主端 veth（连结对端；幂等）。
	Delete(ctx context.Context, owner string) error
}

// SetNICHook 注入容器侧网络接入钩子（装配处：内核数据面接内核网络 Provider；VPP 数据面不调用）。
func (p *Provider) SetNICHook(h ContainerNICHook) { p.nicHook = h }

// SetConfigSource 注入「当前 committed 配置」的来源（装配处接 engine.Committed）。
//
// 由来：start/restart 两个运行态动作只带容器名（既有 API 形状不动），而「该容器声明了哪些
// vNIC」只有配置是单一真源——与内核读视图接 committed 同一口径。钩子为 nil（VPP 数据面）时
// 本来源不会被读取，memif 路径零行为变化。
func (p *Provider) SetConfigSource(src func() (model.Config, error)) { p.cfgSrc = src }

// containerHasNICs 该容器是否需要走钩子接入：有 vNIC 声明且钩子在位（VPP 下恒 false）。
func (p *Provider) containerHasNICs(ct model.ContainerFunction) bool {
	return len(ct.Interfaces) > 0 && p.nicHook != nil
}

// declaredContainerByName 从 committed 配置取该容器的声明（start/restart 只带名字）。
// 配置里没有该容器（带外/陈旧容器）⇒ ok=false：没有可接入的声明，按空操作处理。
func (p *Provider) declaredContainerByName(name string) (model.ContainerFunction, bool, error) {
	if p.cfgSrc == nil {
		// 接线缺口如实报错：内核数据面必须接 committed 来源，否则无法确定声明 vNIC——
		// 静默跳过会让容器起了却不通网，比报错更难排查。
		return model.ContainerFunction{}, false, fmt.Errorf("容器编排未接线配置来源，无法确定容器 %s 的 vNIC 声明", name)
	}
	cfg, err := p.cfgSrc()
	if err != nil {
		return model.ContainerFunction{}, false, fmt.Errorf("读取配置以确定容器 %s 的 vNIC 声明: %w", name, err)
	}
	for _, ct := range cfg.ContainerFunctions {
		if ct.Name == name {
			return ct, true, nil
		}
	}
	return model.ContainerFunction{}, false, nil
}

// attachNICs 取容器 PID 并驱动钩子把其 vNIC 接进该 netns（幂等；纯步骤，不取 p.mu——
// 调用方（生命周期方法已持锁、巡检不持锁）各自负责串行）。
func (p *Provider) attachNICs(ctx context.Context, ct model.ContainerFunction) error {
	pid, err := p.api.ContainerPID(ctx, ct.Name)
	if err != nil {
		return fmt.Errorf("读取容器 %s 的 PID（容器 vNIC 接入）: %w", ct.Name, err)
	}
	if pid <= 0 {
		// 诚实报错，不猜：PID 取不到就没有可移入的 netns（容器未运行/刚退出/底座未给该字段）。
		return fmt.Errorf("容器 %s 的 PID 无效（%d），无法把 vNIC 移入其网络命名空间", ct.Name, pid)
	}
	return p.nicHook.Attach(ctx, ct.Name, ct.Interfaces, pid)
}

// detachNICs 清该容器的宿主端 veth（钩子未注入 ⇒ 空操作；幂等——容器端随 netns 销毁）。
func (p *Provider) detachNICs(ctx context.Context, name string) error {
	if p.nicHook == nil {
		return nil
	}
	return p.nicHook.Delete(ctx, name)
}

// attachAfterStart 启动成功后的接入（有声明 vNIC 且钩子在位才动作）。
// 失败如实返回：容器**已在运行**而网络未接好，不能静默当成功；巡检会按声明补接。
func (p *Provider) attachAfterStart(ctx context.Context, ct model.ContainerFunction) error {
	if !p.containerHasNICs(ct) {
		return nil
	}
	if err := p.attachNICs(ctx, ct); err != nil {
		return fmt.Errorf("容器 %s 已启动，但容器 vNIC 接入失败（容器内网络暂不可用；可重启容器重试，巡检也会自动补接）: %w", ct.Name, err)
	}
	return nil
}

// attachDeclaredNICs 按容器名接入：start/restart 路径只带名字，声明从 committed 配置取。
func (p *Provider) attachDeclaredNICs(ctx context.Context, name string) error {
	if p.nicHook == nil {
		return nil
	}
	ct, ok, err := p.declaredContainerByName(name)
	if err != nil {
		return fmt.Errorf("容器 %s 已启动，但容器 vNIC 接入失败：%v", name, err)
	}
	if !ok {
		return nil
	}
	return p.attachAfterStart(ctx, ct)
}

// attachRunningNICs 恢复重放（EnsureConsistent）的幂等补接：只对**运行中**容器做——
// 未运行容器没有可移入的 netns。宿主端 veth 是内核对象，重放按名核对/复用、绝不重建
// （重建会打断运行中容器的网络）。
func (p *Provider) attachRunningNICs(ctx context.Context, ct model.ContainerFunction) error {
	if !p.containerHasNICs(ct) {
		return nil
	}
	state, exists, err := p.api.State(ctx, ct.Name)
	if err != nil {
		return err
	}
	if !exists || state != orchestrator.CTStateRunning {
		return nil
	}
	return p.attachNICs(ctx, ct)
}

// nicAlarmScope 容器 vNIC 接入未收敛告警的**作用域**：与 network/netkernel 的恢复收敛告警
// 同一域（取值 "recovery"，同族见它们的 ReconcileDNSProxy/ReconcileProxy）。**不能**用
// orchestrator.RecoveryScopeContainer——CheckContainerAlarms 会在该域按容器名对账清警
// （ResolveStale），会把这个固定 source 的活动告警当陈旧项清掉。
const nicAlarmScope = "recovery"

// nicAlarmSource 巡检未收敛告警的 source（固定一条：一轮内全部容器的失败合并上报，
// 同 ReconcileDNSProxy 的 "dns-proxy"）。
const nicAlarmSource = "container-nics"

// CheckContainerNICs 容器侧 vNIC 接入的 15s 巡检对账（决策 #441）：
//   - 运行中容器 ⇒ 幂等补接（容器 restart 换 netns 后未重接、宿主端被带外删掉、上次接入
//     失败——都由这一路径自愈）；
//   - 容器**已不存在**（inspect 报 not found；声明已删的残渣由网络侧巡检兜底）⇒ 清宿主端残留；
//   - 已停/已创建（容器对象还在、只是没跑）⇒ **不动**：宿主端随声明存在，下一次 start 由
//     Attach 幂等重接——停容器不删宿主端，与网络侧「按声明确保宿主端」的巡检不互相打架。
//
// 钩子未注入（VPP 数据面）⇒ 空操作，memif 路径零行为变化。失败除返回错误（巡检日志）外，
// 按恢复收敛同一 scope/code 建告警（`show alarms` 事后可查）；下一轮全部成功即自动消解。
func (p *Provider) CheckContainerNICs(ctx context.Context, cfg model.Config) []error {
	if p.nicHook == nil {
		return nil
	}
	var errs []error
	for _, ct := range cfg.ContainerFunctions {
		if !p.containerHasNICs(ct) {
			continue
		}
		state, exists, err := p.api.State(ctx, ct.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("容器 %s vNIC 接入巡检: %w", ct.Name, err))
			continue
		}
		switch {
		case exists && state == orchestrator.CTStateRunning:
			// 幂等补接：容器 restart 换 netns 后未重接、宿主端被带外删掉、上次接入失败——都自愈。
			if err := p.attachNICs(ctx, ct); err != nil {
				errs = append(errs, fmt.Errorf("容器 %s: %w", ct.Name, err))
			}
		case !exists:
			// 容器已不存在：宿主端残留按名清掉（幂等）。
			if err := p.detachNICs(ctx, ct.Name); err != nil {
				errs = append(errs, fmt.Errorf("容器 %s: %w", ct.Name, err))
			}
		default:
			// 已停/已创建（容器对象还在、只是没跑）：宿主端**保留**——不清，下一次 start 由
			// Attach 幂等重接（停容器不删宿主端，见 StopContainer）。
		}
	}
	if p.alarms != nil {
		if len(errs) == 0 {
			p.alarms.Resolve(nicAlarmScope, orchestrator.RecoveryUnconverged, nicAlarmSource)
		} else {
			p.alarms.Raise(nicAlarmScope, orchestrator.SeverityWarning, orchestrator.RecoveryUnconverged,
				"容器 vNIC 接入未收敛: "+joinNICErrs(errs), nicAlarmSource)
		}
	}
	return errs
}

// joinNICErrs 多容器的失败合并成一行告警文案（分号分隔）。
func joinNICErrs(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "；")
}
