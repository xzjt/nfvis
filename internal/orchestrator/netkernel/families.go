package netkernel

// 本文件把各「接口级绑定族」（ACL / QoS / 端口镜像 / 风暴抑制 / 端口安全）接到 Provider 上。
//
// 与其余族的关键差别：这些族的**下发时机**由声明决定，而声明分散在不同位置——
//   - QoS 策略名写在 `interfaces[].ingress_policy|egress_policy`（绑定在接口上）；
//   - ACL 名写在 `vrfs[].l3_interfaces[].acl_in`（绑定在三层接口上）；
//   - 风暴抑制/端口安全写在 `interfaces[].storm_control|port_security`（绑定在接口上）。
// 提交编排的调用顺序保证 `ApplyACL`/`ApplyQos` 先于 `ApplyInterface`/`ApplyVRF`
// （见 internal/orchestrator/apply.go 的 plan 段序），故族对象在下发前已就位。

import (
	"context"
	"fmt"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// spanBinding 一条端口镜像会话的运行态（源口/分析口/方向）。
//
// 为什么需要它：tc 的 mirred filter 里没有「产品侧会话名」可存，而 `DeleteSpan` 只拿到会话名
// （编排层的调用签名如此）。恢复收敛会按声明重放所有会话，故重启后登记同样会被重建。
type spanBinding struct {
	srcDev      string
	analyzerDev string
	direction   string
}

// 族管理器的惰性构造与两处进程内登记（QoS 策略参数、SPAN 会话 → 端口）。
var (
	famMu       sync.Mutex
	qosPolicies map[string]model.QosPolicy
	spanNames   map[string]spanBinding
)

func (p *Provider) aclMgr() *aclManager {
	if p.acl == nil {
		p.acl = newACLManager(p.run)
	}
	return p.acl
}

func (p *Provider) qosMgr() *qosManager {
	if p.qos == nil {
		p.qos = newQoSManager(p.run)
	}
	return p.qos
}

func (p *Provider) spanMgr() *spanManager {
	if p.span == nil {
		p.span = newSpanManager(p.run)
	}
	return p.span
}

func (p *Provider) stormMgr() *stormManager {
	if p.storm == nil {
		p.storm = newStormManager(p.run)
	}
	return p.storm
}

func (p *Provider) portSecMgr() *portSecManager {
	if p.portSec == nil {
		p.portSec = newPortSecManager(p.run)
	}
	return p.portSec
}

// ---------- ACL ----------

// ApplyACL 收敛一条 ACL（own nftables 表；绑定另行由 ApplyVRF 的 acl-in 下发）。
func (p *Provider) ApplyACL(ctx context.Context, acl model.Acl) error {
	return p.aclMgr().Apply(ctx, acl)
}

// DeleteACL 撤销一条 ACL。仍被接口绑定时报错（不留下悬空的 jump）。
func (p *Provider) DeleteACL(ctx context.Context, name string) error {
	return p.aclMgr().Delete(ctx, name)
}

// UnbindL3IfaceACL 撤销一条 L3 接口的 acl-in 绑定（策略对象保留，接口不再受其约束）。
func (p *Provider) UnbindL3IfaceACL(ctx context.Context, _ string, iface model.L3Interface) error {
	if iface.AclIn == "" {
		return nil
	}
	return p.aclMgr().Unbind(ctx, l3DeviceName(iface))
}

// bindL3IfaceACL 在 ApplyVRF 里为声明了 acl-in 的三层接口下发绑定。
func (p *Provider) bindL3IfaceACL(ctx context.Context, iface model.L3Interface) error {
	if iface.AclIn == "" {
		return nil
	}
	acl, ok := p.aclByName(iface.AclIn)
	if !ok {
		return fmt.Errorf("三层接口 %s 绑定了未下发的 ACL %s", l3DeviceName(iface), iface.AclIn)
	}
	return p.aclMgr().Bind(ctx, l3DeviceName(iface), acl)
}

// aclByName 从最近一次收敛的配置快照里取 ACL（绑定只写名字，需要规则体）。
func (p *Provider) aclByName(name string) (model.Acl, bool) {
	for _, a := range p.config().Acls {
		if a.Name == name {
			return a, true
		}
	}
	return model.Acl{}, false
}

// ---------- QoS ----------

// ApplyQos 记录一条限速策略的参数。
//
// 内核侧没有「策略对象」可建：tc 的 rate/burst 内联在 filter 上，绑定发生在接口侧
// （`ApplyInterface` 读 `interfaces[].ingress_policy|egress_policy`）。这里只登记参数，
// 供绑定与恢复收敛解析；策略删除由模型校验保证「无引用」。
func (p *Provider) ApplyQos(_ context.Context, q model.QosPolicy) error {
	famMu.Lock()
	defer famMu.Unlock()
	if qosPolicies == nil {
		qosPolicies = map[string]model.QosPolicy{}
	}
	qosPolicies[q.Name] = q
	return nil
}

// DeleteQos 忘记一条限速策略的参数（绑定已由模型校验保证不存在）。
func (p *Provider) DeleteQos(ctx context.Context, name string) error {
	famMu.Lock()
	delete(qosPolicies, name)
	famMu.Unlock()
	// 兜底：若仍有残留绑定（例如带外改过内核），按接口逐个解绑代价高，故只清参数——
	// 残留的 filter 会在下一次该接口下发或数据面重启时按声明收敛。
	_ = ctx
	return nil
}

func qosPolicyByName(name string) (model.QosPolicy, bool) {
	famMu.Lock()
	defer famMu.Unlock()
	q, ok := qosPolicies[name]
	return q, ok
}

// applyInterfaceQoS 按接口声明收敛入向/出向限速绑定（两向各自独立）。
func (p *Provider) applyInterfaceQoS(ctx context.Context, iface model.InterfaceConfig) error {
	dev := LinkName(iface.Name)
	// 入向：先把两向都按"声明为空则解绑"处理——避免只改一向往返时留下另一向的陈旧绑定。
	for _, d := range []struct {
		dir  string
		name string
	}{{"ingress", iface.IngressPolicy}, {"egress", iface.EgressPolicy}} {
		if d.name == "" {
			if err := p.qosMgr().Unbind(ctx, dev, d.dir); err != nil {
				return err
			}
			continue
		}
		q, ok := qosPolicyByName(d.name)
		if !ok {
			return fmt.Errorf("接口 %s 绑定了未下发的 QoS 策略 %s", iface.Name, d.name)
		}
		if err := p.qosMgr().Bind(ctx, dev, d.dir, q.Cir, q.Cbs); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 端口镜像（SPAN） ----------

// ApplySpan 收敛一条镜像会话：源口（物理口/bond）→ 分析口。
//
// 内核数据面**不支持以 VNF 虚拟网卡为源**：宿主 tap 由 libvirt 在域启动时创建，产品侧没有
// 可靠的 tap 名映射（提交期已拒绝该形态）。
func (p *Provider) ApplySpan(ctx context.Context, pm model.PortMirroring) error {
	src, err := spanSourceDev(pm)
	if err != nil {
		return err
	}
	analyzer := LinkName(pm.Analyzer)
	dir := pm.Source.Direction
	if dir == "" {
		dir = "both"
	}
	if err := p.spanMgr().Apply(ctx, src, analyzer, dir); err != nil {
		return err
	}
	famMu.Lock()
	if spanNames == nil {
		spanNames = map[string]spanBinding{}
	}
	spanNames[pm.Name] = spanBinding{srcDev: src, analyzerDev: analyzer, direction: dir}
	famMu.Unlock()
	return nil
}

// DeleteSpan 撤销一条镜像会话（按登记解析源口；登记由声明重放重建）。
func (p *Provider) DeleteSpan(ctx context.Context, name string) error {
	famMu.Lock()
	b, ok := spanNames[name]
	famMu.Unlock()
	if !ok {
		// 没有登记 = 本进程从未下发过该会话（例如它是别的数据面实例建的）。
		// 如实报错而不是静默成功——静默会让操作者以为镜像已撤。
		return fmt.Errorf("镜像会话 %s 没有本进程的下发登记，无法定位源口；请重启数据面后重试", name)
	}
	return p.spanMgr().Delete(ctx, b.srcDev, b.direction)
}

// spanSourceDev 解析镜像源的设备名。
func spanSourceDev(pm model.PortMirroring) (string, error) {
	if pm.Source.Vnf != "" {
		return "", fmt.Errorf("镜像源 %s 是 VNF 虚拟网卡：内核数据面不支持以虚拟网卡为镜像源", pm.Source.Vnf)
	}
	if pm.Source.Interface == "" {
		return "", fmt.Errorf("镜像会话 %s 缺少源接口", pm.Name)
	}
	return LinkName(pm.Source.Interface), nil
}

// ---------- 风暴抑制 / 端口安全 ----------

// applyInterfaceStorm 按接口声明收敛入向风暴抑制（未声明即撤）。
func (p *Provider) applyInterfaceStorm(ctx context.Context, iface model.InterfaceConfig) error {
	dev := LinkName(iface.Name)
	if iface.StormControl == nil {
		return p.stormMgr().Teardown(ctx, dev)
	}
	return p.stormMgr().Apply(ctx, dev, iface.StormControl)
}

// applyInterfacePortSec 按接口声明收敛端口安全白名单（未声明即撤）。
func (p *Provider) applyInterfacePortSec(ctx context.Context, iface model.InterfaceConfig) error {
	dev := LinkName(iface.Name)
	if len(iface.PortSecurity) == 0 {
		return p.portSecMgr().Teardown(ctx, dev)
	}
	return p.portSecMgr().Apply(ctx, dev, iface.PortSecurity)
}

// ---------- 与既有装配面的对接 ----------

// StormDataplane 接口 detail 的风暴抑制实况块（读内核，不读进程内登记）。
func (p *Provider) StormDataplane(ctx context.Context, ifname string) (network.StormDataplane, bool) {
	attached, detail, err := p.stormMgr().Dataplane(ctx, LinkName(ifname))
	if err != nil {
		return network.StormDataplane{Available: false, Reason: err.Error()}, false
	}
	if !attached {
		return network.StormDataplane{Available: true, Reason: detail}, false
	}
	// Kinds 由 stormManager 的 detail 文本承载（哪几类在限速），这里按「在位」给空表——
	// 逐类明细由 detail 说明，避免编造读数。
	return network.StormDataplane{Available: true, Reason: detail, Attached: true}, true
}

// PortSecDataplane 接口 detail 的端口安全实况块（读内核）。
func (p *Provider) PortSecDataplane(ctx context.Context, ifname string) (network.PortSecDataplane, bool) {
	attached, detail, err := p.portSecMgr().Dataplane(ctx, LinkName(ifname))
	if err != nil {
		return network.PortSecDataplane{Available: false, Reason: err.Error()}, false
	}
	if !attached {
		return network.PortSecDataplane{Available: true, Reason: detail}, false
	}
	return network.PortSecDataplane{Available: true, Reason: detail, Bound: true}, true
}
