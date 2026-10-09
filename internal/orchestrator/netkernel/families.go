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

// 族管理器的惰性构造与三处进程内登记。
//
// 为什么必须登记而不是只读配置快照：提交编排**先下发族对象、再下发引用它的绑定**
// （ACL → 三层接口；QoS 策略 → 接口），而下发发生在一次提交之内——此刻 `p.config()`
// 还是上一次收敛的快照，刚建的 ACL/策略不在里面。真机走查实测过这个缺陷：
// 建 ACL 并在同一次提交里绑到三层接口，报「绑定了未下发的 ACL」。
// 三处登记都由恢复收敛的声明重放重建（EnsureConsistent 按同一段序重放）。
var (
	famMu       sync.Mutex
	qosPolicies map[string]model.QosPolicy
	aclDefs     map[string]model.Acl
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

// ApplyACL 收敛一条 ACL（own nftables 表；绑定另行由 ApplyVRF 的 acl-in 下发），
// 并登记规则体供同一次提交里的绑定解析。
func (p *Provider) ApplyACL(ctx context.Context, acl model.Acl) error {
	if err := p.aclMgr().Apply(ctx, acl); err != nil {
		return err
	}
	famMu.Lock()
	if aclDefs == nil {
		aclDefs = map[string]model.Acl{}
	}
	aclDefs[acl.Name] = acl
	famMu.Unlock()
	return nil
}

// DeleteACL 撤销一条 ACL。仍被接口绑定时报错（不留下悬空的 jump）。
func (p *Provider) DeleteACL(ctx context.Context, name string) error {
	if err := p.aclMgr().Delete(ctx, name); err != nil {
		return err
	}
	famMu.Lock()
	delete(aclDefs, name)
	famMu.Unlock()
	return nil
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

// aclByName 取 ACL 的规则体（绑定只写名字）。先查本次提交已下发的登记，再回落配置快照
// （恢复收敛路径下两者一致；提交路径下只有前者有）。
//
// ⚠️ 兜底必须读**非阻塞快照**（`configSnapshot()`），**不能**读 `config()`——本函数在
// `ApplyL3Interface` 的**应用路径**上，而提交期间配置发动机的锁由这次提交自己持有，
// 读活配置即**重入自死锁**（决策 #438 真机实证；R3-11）。快照与登记都没有时，
// 如实按「找不到该 ACL」返回（即「绑定了未下发的 ACL」），而不是去读活配置。
func (p *Provider) aclByName(name string) (model.Acl, bool) {
	famMu.Lock()
	a, ok := aclDefs[name]
	famMu.Unlock()
	if ok {
		return a, true
	}
	for _, x := range p.configSnapshot().Acls {
		if x.Name == name {
			return x, true
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
//
// 内核事实要落进**消费方真正读的字段**（R2-15②）：internal/api/storm.go 在 Available=true 时
// 只看 Kinds（Reason 只在"不可核对"时打印）——Kinds 留空会把"tc 过滤器正在位限速"报成
// "policer 未在数据面（未收敛）"。逐类给 PolicerPresent + 实测 CIR；读不到实测速率的类不出现
// 在 Kinds 里（调用方按"该类未在位"渲染，不编造 0）。
func (p *Provider) StormDataplane(ctx context.Context, ifname string) (network.StormDataplane, bool) {
	fact, err := p.stormMgr().Dataplane(ctx, LinkName(ifname))
	if err != nil {
		return network.StormDataplane{Available: false, Reason: err.Error()}, false
	}
	if !fact.Attached {
		return network.StormDataplane{Available: true, Reason: fact.Detail}, false
	}
	kinds := map[string]network.StormKindDataplane{}
	for kind, kbps := range map[string]int{
		network.StormKindBroadcast: fact.BroadcastKbps,
		network.StormKindMulticast: fact.MulticastKbps,
	} {
		if kbps < 0 {
			continue // 该类过滤器不在位（不是"限速 0"）
		}
		kinds[kind] = network.StormKindDataplane{
			PolicerPresent: true,
			CirKbps:        uint32(kbps),
			// 内核 tc 的 police 计数是 dropped/overlimits 两项，与 VPP 的 conform/exceed/violate
			// 三档不对应——如实说明（消费方会把它渲染成"该类计数不可读（原因）"），不映射、不编 0。
			CountersReason: "内核 tc 的 police 计数为 dropped/overlimits，与 conform/exceed/violate 三档语义不对应，如实不映射",
		}
	}
	return network.StormDataplane{
		Available: true, Reason: fact.Detail, Attached: true, Kinds: kinds,
	}, len(kinds) > 0
}

// PortSecDataplane 接口 detail 的端口安全实况块（读内核）。
//
// 同 R2-15②：消费方 internal/api/portsec.go 在 Available=true 时只读 TagPresent/Tag/RuleCount/
// ACLIndex/Bound。内核侧没有 macip ACL 索引这回事（ACLIndex 恒 0——不是"读到索引 0"，故 Reason
// 里把这条说明白），白名单按 nftables 链定位，规则条数照实给（整段白名单一条规则）。
func (p *Provider) PortSecDataplane(ctx context.Context, ifname string) (network.PortSecDataplane, bool) {
	fact, err := p.portSecMgr().Dataplane(ctx, LinkName(ifname))
	if err != nil {
		return network.PortSecDataplane{Available: false, Reason: err.Error()}, false
	}
	// Reason 只在"不可核对"时打印；正常的在位/不在位由上面那些字段表达。
	reason := fmt.Sprintf("内核侧无 ACL 索引概念（白名单规则落在 nftables 链 %s 上）；%s", fact.ChainName, fact.Detail)
	return network.PortSecDataplane{
		Available: true,
		Reason:    reason,
		// Tag 是内核侧的对象标识（VPP 侧是 macip ACL 的 tag）：链名带上前缀，读视图直接可读。
		Tag:        "内核 nftables 链 " + fact.ChainName,
		TagPresent: fact.ChainPresent,
		RuleCount:  fact.RuleCount,
		Bound:      fact.Bound,
	}, fact.Attached
}
