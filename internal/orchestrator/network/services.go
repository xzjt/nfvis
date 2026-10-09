package network

// M3-5：ACL/NAT/SPAN/QoS 生效（规格书 §4.3）。本文件先落 SPAN 镜像与端口限速
// （VPP span / policer），并补上接口层下发（MTU、ingress-policy 绑定）——
// 接口此前未纳入 apply 链路，QoS 绑定/MTU 无从生效。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// SvcClient VPP SPAN/QoS/接口 binary API 的最小能力集。
type SvcClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	SetMTU(swIfIndex, mtu uint32) error
	SetState(swIfIndex uint32, up bool) error
	SpanSet(from, to uint32, state string, isL2 bool) error
	PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error)
	PolicerInput(swIfIndex uint32, name string, apply bool) error
	PolicerOutput(swIfIndex uint32, name string, apply bool) error // 出向 policer（决策 #331）
	SpanDisable(from, to uint32) error
	Close()
}

// spanRec SPAN 会话的源/目的接口索引（关闭时需同时给出）。
type spanRec struct{ from, to uint32 }

// ServicesProvider SPAN（FR-NET-016/§4.3 端口镜像）与 QoS 限速（CIR/CBS）编排。
type ServicesProvider struct {
	client func() (SvcClient, error)

	mu          sync.Mutex
	spans       map[string]spanRec // 会话名 → 源/目的 sw_if_index
	policer     map[string]bool    // 已创建的 policer 名
	bound       map[string]string  // 接口名 → 入向绑定的 policer 名
	boundEgress map[string]string  // 接口名 → 出向绑定的 policer 名（决策 #331）
}

// NewServicesProvider 以固定客户端构造（测试）。
func NewServicesProvider(c SvcClient) *ServicesProvider {
	return &ServicesProvider{client: func() (SvcClient, error) { return c, nil },
		spans: map[string]spanRec{}, policer: map[string]bool{},
		bound: map[string]string{}, boundEgress: map[string]string{}}
}

// NewServicesProviderFunc 以客户端工厂构造（连接可重连）。
func NewServicesProviderFunc(f func() (SvcClient, error)) *ServicesProvider {
	return &ServicesProvider{client: f, spans: map[string]spanRec{},
		policer: map[string]bool{}, bound: map[string]string{}, boundEgress: map[string]string{}}
}

// reset 清空进程内登记表（恢复收敛前调用，SPAN/policer/绑定全量重放）。
func (p *ServicesProvider) reset() {
	p.mu.Lock()
	p.spans = map[string]spanRec{}
	p.policer = map[string]bool{}
	p.bound = map[string]string{}
	p.boundEgress = map[string]string{}
	p.mu.Unlock()
}

// ApplySpan 配置 SPAN：源口 → 分析口，方向 ingress|egress|both（缺省 both）。
//
// 源两形态：物理口/bond 按名解析；VNF vNIC 先解析为其**确定性 vhost-user 口名**
// （`vh-<vm>-<vnic>`，见 spanSourceIface），再与物理口走**同一条** span 原语。
// vNIC 口随 vNIC 声明在数据面建立（与 VM 是否运行无关：VM 未运行时该口 link down，
// 镜像是就绪的、只是没有流量），故无需为它加任何特殊下发路径。
func (p *ServicesProvider) ApplySpan(ctx context.Context, pm model.PortMirroring) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	src, err := spanSourceIface(c, pm.Source)
	if err != nil {
		return fmt.Errorf("SPAN 源口: %w", err)
	}
	dst, err := resolveIface(c, pm.Analyzer)
	if err != nil {
		return fmt.Errorf("SPAN 分析口: %w", err)
	}
	state := spanState(pm.Source.Direction)
	if err := c.SpanSet(src, dst, state, false); err != nil {
		return fmt.Errorf("SPAN %s: %w", pm.Name, err)
	}
	p.mu.Lock()
	p.spans[pm.Name] = spanRec{from: src, to: dst}
	p.mu.Unlock()
	return nil
}

// spanSourceIface 解析镜像源为 VPP sw_if_index：物理口/bond 按名；VNF vNIC 按其在数据面中的
// **确定性 vhost-user 口名**解析（命名规则唯一真源 internal/model/ifacename.go，经
// orchestrator.VnfIfaceName 转发——与建接口、交换机端口、l3-interface 校验同一份规则，
// 不在此另写一套）。
//
// 解析不到该口即**如实报错、不静默降级**（不给「镜像成功」的假象），错误里带照做路径：
// 先声明 vNIC 并下发、或改用物理口；sriov-vf 直通的 vNIC 不经过 VPP，点名它不能作镜像源。
func spanSourceIface(c SvcClient, src model.PMSource) (uint32, error) {
	if src.Vnf == "" {
		return resolveIface(c, src.Interface)
	}
	name := orchestrator.VnfIfaceName(src.Vnf, src.VnfInterface)
	idx, ok, err := c.SwInterfaceIndex(name)
	if err != nil {
		return 0, fmt.Errorf("解析 VNF %s 的 vNIC %s 的 vhost-user 接口 %s: %w", src.Vnf, src.VnfInterface, name, err)
	}
	if !ok {
		return 0, fmt.Errorf("%w: VNF %s 的 vNIC %s 在数据面不存在（VPP 中未见接口 %s）：%s",
			ErrIfaceUnavailable, src.Vnf, src.VnfInterface, name, vnicSpanMissingHint)
	}
	return idx, nil
}

// vnicSpanMissingHint VNF vNIC 作镜像源解析不到口时的照做提示。
// 不带内部引用，只写操作者可照做的一步与两种替代。
const vnicSpanMissingHint = "请先确认该 vNIC 已在 VNF 上声明并已下发" +
	"（vhost-user 口随 vNIC 声明在数据面建立，VM 未启动也应有；若刚重启过数据面，" +
	"等恢复收敛完成或执行 request vpp restart）；" +
	"sriov-vf 直通的 vNIC 不经过 VPP、不能作镜像源；也可以把源改为物理口或 bond"

// DeleteSpan 关闭 SPAN 会话。登记=最后一个成功下发的状态（决策 #363）：
// SpanDisable 成功才摘登记，失败保留（重试可再关；此前先摘登记，关失败后登记已丢、再也关不掉）。
func (p *ServicesProvider) DeleteSpan(ctx context.Context, name string) error {
	p.mu.Lock()
	rec, ok := p.spans[name]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SpanDisable(rec.from, rec.to); err != nil {
		return fmt.Errorf("关闭 SPAN %s: %w", name, err)
	}
	p.mu.Lock()
	delete(p.spans, name)
	p.mu.Unlock()
	return nil
}

// ApplyQos 创建 1R2C 限速策略（CIR bps → kbps，CBS 字节）；绑定经 ApplyInterface。
func (p *ServicesProvider) ApplyQos(ctx context.Context, q model.QosPolicy) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if q.Cir <= 0 {
		return fmt.Errorf("QoS 策略 %s 的 CIR 必须为正", q.Name)
	}
	kbps := uint32(q.Cir / 1000)
	if kbps == 0 {
		kbps = 1
	}
	if _, err := c.PolicerAddDel(q.Name, kbps, uint64(q.Cbs), true); err != nil {
		return fmt.Errorf("创建 policer %s: %w", q.Name, err)
	}
	p.mu.Lock()
	p.policer[q.Name] = true
	p.mu.Unlock()
	return nil
}

// DeleteQos 删除限速策略（先解绑再删；入向与出向绑定都要解，决策 #331）。
// 登记按「最后一个成功下发的状态」逐口摘除（决策 #363）：某口解绑成功才摘该口登记，
// 失败即返回且登记保留（重试会再解该口）；接口已不在 VPP 时按既有口径跳过（该口无从解绑，
// 登记随摘除动作清掉）。全部解绑成功后才删 policer，成功才摘 policer 登记。
//
// 查询失败与「接口不存在」必须分开（决策 #393，收口 round171 R171-6）：此前 `err != nil || !ok`
// 一并摘登记，`SwInterfaceIndex` 偶发失败即把绑定登记抹掉、重试不再解绑、VPP 绑定残留到重启；
// 与 #363「登记＝最后成功下发状态」及本仓「查不出来不许当已达成」（l3.go 的 resolveRegistered）
// 相悖。现口径：查询**失败**（err != nil）照实上抛并**保留**登记（可重试）；仅**确认接口不存在**
// （ok==false）才摘登记。
func (p *ServicesProvider) DeleteQos(ctx context.Context, name string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	// 锁内只读清单：两个方向的待解绑接口（VPP 调用一律在锁外）
	p.mu.Lock()
	var boundIn, boundOut []string
	for ifname, pol := range p.bound {
		if pol == name {
			boundIn = append(boundIn, ifname)
		}
	}
	for ifname, pol := range p.boundEgress {
		if pol == name {
			boundOut = append(boundOut, ifname)
		}
	}
	p.mu.Unlock()
	for _, ifname := range boundIn {
		idx, ok, err := c.SwInterfaceIndex(ifname)
		if err != nil {
			// 查询失败 ≠ 接口不存在：保留登记、照实上抛，重试会再解该口（决策 #393）。
			return fmt.Errorf("解析接口 %s（解绑入向 policer %s）: %w", ifname, name, err)
		}
		if !ok {
			// 确认接口已不存在：无从解绑，跳过该口的 VPP 调用；登记一并摘除——
			// 接口都不在了、绑定不可能还挂在上面，留着陈旧登记会让同名接口复现后
			// 的 ApplyInterface 误判「已在位」而静默跳过（决策 #363 要防的假成功）。
			p.setBoundPolicy(ifname, "", false)
			continue
		}
		if err := c.PolicerInput(idx, name, false); err != nil {
			return fmt.Errorf("解绑接口 %s 的入向 policer %s: %w", ifname, name, err)
		}
		p.setBoundPolicy(ifname, "", false)
	}
	for _, ifname := range boundOut {
		idx, ok, err := c.SwInterfaceIndex(ifname)
		if err != nil {
			return fmt.Errorf("解析接口 %s（解绑出向 policer %s）: %w", ifname, name, err)
		}
		if !ok {
			p.setBoundPolicy(ifname, "", true)
			continue
		}
		if err := c.PolicerOutput(idx, name, false); err != nil {
			return fmt.Errorf("解绑接口 %s 的出向 policer %s: %w", ifname, name, err)
		}
		p.setBoundPolicy(ifname, "", true)
	}
	if _, err := c.PolicerAddDel(name, 0, 0, false); err != nil {
		return fmt.Errorf("删除 policer %s: %w", name, err)
	}
	p.mu.Lock()
	delete(p.policer, name)
	p.mu.Unlock()
	return nil
}

// ApplyInterface 下发接口层配置：MTU、admin 状态，与 ingress-policy/egress-policy
// （policer 入向/出向绑定，决策 #331：两方向可并存、各自独立增删）。
// 绑定登记按「最后一个成功下发的状态」逐步骤推进（决策 #363）：解绑旧成功才清该向登记、
// 绑新成功才写该向登记——任一步失败即返回且登记停在最后成功态。此前「先改登记再下发」
// 会留下「登记说新、数据面是旧」的错位：重试因登记已是新值而静默跳过（假成功），
// 补偿 ApplyInterface(旧值) 也因登记已是新值而不下发。两方向登记互不影响。
func (p *ServicesProvider) ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx, ok, err := c.SwInterfaceIndex(iface.Name)
	if err != nil {
		return fmt.Errorf("解析接口 %s: %w", iface.Name, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, iface.Name)
	}
	if iface.MTU > 0 {
		if err := c.SetMTU(idx, uint32(iface.MTU)); err != nil {
			return fmt.Errorf("设置接口 %s MTU %d: %w", iface.Name, iface.MTU, err)
		}
	}
	// VPP 接口默认 admin-down：数据口必须显式 up，否则不转发（Enabled 缺省视为启用）
	up := iface.Enabled == nil || *iface.Enabled
	if err := c.SetState(idx, up); err != nil {
		return fmt.Errorf("设置接口 %s 状态 %v: %w", iface.Name, up, err)
	}
	// 入向（policer_input）
	if prevIn := p.boundPolicy(iface.Name, false); prevIn != iface.IngressPolicy {
		if prevIn != "" {
			if err := c.PolicerInput(idx, prevIn, false); err != nil {
				return fmt.Errorf("解绑接口 %s 原入向策略 %s: %w", iface.Name, prevIn, err)
			}
			// 解绑成功才清登记；失败时登记停在旧值，重试会重试解绑
			p.setBoundPolicy(iface.Name, "", false)
		}
		if iface.IngressPolicy != "" {
			// policer 须先由 ApplyQos 创建（apply 顺序保证）
			if err := c.PolicerInput(idx, iface.IngressPolicy, true); err != nil {
				return fmt.Errorf("绑定接口 %s 入向策略 %s: %w", iface.Name, iface.IngressPolicy, err)
			}
			p.setBoundPolicy(iface.Name, iface.IngressPolicy, false)
		}
	}
	// 出向（policer_output，决策 #331）
	if prevOut := p.boundPolicy(iface.Name, true); prevOut != iface.EgressPolicy {
		if prevOut != "" {
			if err := c.PolicerOutput(idx, prevOut, false); err != nil {
				return fmt.Errorf("解绑接口 %s 原出向策略 %s: %w", iface.Name, prevOut, err)
			}
			p.setBoundPolicy(iface.Name, "", true)
		}
		if iface.EgressPolicy != "" {
			if err := c.PolicerOutput(idx, iface.EgressPolicy, true); err != nil {
				return fmt.Errorf("绑定接口 %s 出向策略 %s: %w", iface.Name, iface.EgressPolicy, err)
			}
			p.setBoundPolicy(iface.Name, iface.EgressPolicy, true)
		}
	}
	return nil
}

// TeardownInterface 解绑一条已从声明里删除的接口元素的 QoS policer 绑定（入向/出向，决策 #390③）。
// 只动绑定、不碰 MTU/状态（接口即将随数据面重启消失，不应对它做接口层设置）。
// 登记按「最后一个成功下发的状态」逐向摘除（决策 #363 口径）：某向解绑成功才清该向登记；
// 接口已不在 VPP 时按既有容忍跳过并摘陈旧登记（同 DeleteQos）。
func (p *ServicesProvider) TeardownInterface(ctx context.Context, ifname string) error {
	prevIn := p.boundPolicy(ifname, false)
	prevOut := p.boundPolicy(ifname, true)
	if prevIn == "" && prevOut == "" {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx, ok, err := c.SwInterfaceIndex(ifname)
	if err != nil {
		return fmt.Errorf("解析接口 %s: %w", ifname, err)
	}
	if !ok {
		// 接口已不在 VPP：绑定随接口消失，摘陈旧登记（同 DeleteQos 的既有容忍）。
		if prevIn != "" {
			p.setBoundPolicy(ifname, "", false)
		}
		if prevOut != "" {
			p.setBoundPolicy(ifname, "", true)
		}
		return nil
	}
	if prevIn != "" {
		if err := c.PolicerInput(idx, prevIn, false); err != nil {
			return fmt.Errorf("解绑接口 %s 的入向 policer %s: %w", ifname, prevIn, err)
		}
		p.setBoundPolicy(ifname, "", false)
	}
	if prevOut != "" {
		if err := c.PolicerOutput(idx, prevOut, false); err != nil {
			return fmt.Errorf("解绑接口 %s 的出向 policer %s: %w", ifname, prevOut, err)
		}
		p.setBoundPolicy(ifname, "", true)
	}
	return nil
}

// InterfaceExists 查询接口当前是否存在于 VPP（决策 #333：恢复收敛告警的按来源廉价复核用，
// 与 ApplyInterface 判 ok 同一条 SwInterfaceIndex，dump 级代价；复用本 Provider 的既有
// 客户端工厂，不新建 VPP 连接通道）。
func (p *ServicesProvider) InterfaceExists(ifname string) (bool, error) {
	c, err := p.client()
	if err != nil {
		return false, err
	}
	defer c.Close()
	_, ok, err := c.SwInterfaceIndex(ifname)
	return ok, err
}

// boundPolicy 读某接口某方向的绑定登记（egress=false 入向 / true 出向），锁内取值。
// 登记语义=该方向**最后一个成功下发**的 policer 名（决策 #363）。
func (p *ServicesProvider) boundPolicy(ifname string, egress bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if egress {
		return p.boundEgress[ifname]
	}
	return p.bound[ifname]
}

// setBoundPolicy 推进某接口某方向的绑定登记（空串=解绑成功、登记清空）。只在对应
// VPP 调用成功后被调用，故登记恒为该方向最后一个成功下发的状态（决策 #363）。
func (p *ServicesProvider) setBoundPolicy(ifname, pol string, egress bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.bound
	if egress {
		m = p.boundEgress
	}
	if pol == "" {
		delete(m, ifname)
	} else {
		m[ifname] = pol
	}
}

func resolveIface(c SvcClient, ifname string) (uint32, error) {
	if ifname == "" {
		return 0, errors.New("接口名不能为空")
	}
	idx, ok, err := c.SwInterfaceIndex(ifname)
	if err != nil {
		return 0, fmt.Errorf("解析接口 %s: %w", ifname, err)
	}
	if !ok {
		return 0, fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, ifname)
	}
	return idx, nil
}

// spanState 方向 → VPP span 状态。
func spanState(direction string) string {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "ingress":
		return "rx"
	case "egress":
		return "tx"
	default:
		return "rx_tx"
	}
}
