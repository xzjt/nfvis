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
)

// SvcClient VPP SPAN/QoS/接口 binary API 的最小能力集。
type SvcClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	SetMTU(swIfIndex, mtu uint32) error
	SetState(swIfIndex uint32, up bool) error
	SpanSet(from, to uint32, state string, isL2 bool) error
	PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error)
	PolicerInput(swIfIndex uint32, name string, apply bool) error
	SpanDisable(from, to uint32) error
	Close()
}

// spanRec SPAN 会话的源/目的接口索引（关闭时需同时给出）。
type spanRec struct{ from, to uint32 }

// ServicesProvider SPAN（FR-NET-016/§4.3 端口镜像）与 QoS 限速（CIR/CBS）编排。
type ServicesProvider struct {
	client func() (SvcClient, error)

	mu      sync.Mutex
	spans   map[string]spanRec // 会话名 → 源/目的 sw_if_index
	policer map[string]bool    // 已创建的 policer 名
	bound   map[string]string  // 接口名 → 绑定的 policer 名
}

// NewServicesProvider 以固定客户端构造（测试）。
func NewServicesProvider(c SvcClient) *ServicesProvider {
	return &ServicesProvider{client: func() (SvcClient, error) { return c, nil },
		spans: map[string]spanRec{}, policer: map[string]bool{}, bound: map[string]string{}}
}

// NewServicesProviderFunc 以客户端工厂构造（连接可重连）。
func NewServicesProviderFunc(f func() (SvcClient, error)) *ServicesProvider {
	return &ServicesProvider{client: f, spans: map[string]spanRec{},
		policer: map[string]bool{}, bound: map[string]string{}}
}

// reset 清空进程内登记表（恢复收敛前调用，SPAN/policer/绑定全量重放）。
func (p *ServicesProvider) reset() {
	p.mu.Lock()
	p.spans = map[string]spanRec{}
	p.policer = map[string]bool{}
	p.bound = map[string]string{}
	p.mu.Unlock()
}

// ApplySpan 配置 SPAN：源口 → 分析口，方向 ingress|egress|both（缺省 both）。
func (p *ServicesProvider) ApplySpan(ctx context.Context, pm model.PortMirroring) error {
	if pm.Source.Vnf != "" {
		return fmt.Errorf("SPAN 源 %s 为 VNF 接口，属 M4", pm.Source.Vnf)
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	src, err := resolveIface(c, pm.Source.Interface)
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

// DeleteSpan 关闭 SPAN 会话。
func (p *ServicesProvider) DeleteSpan(ctx context.Context, name string) error {
	p.mu.Lock()
	rec, ok := p.spans[name]
	delete(p.spans, name)
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

// DeleteQos 删除限速策略（先解绑再删）。
func (p *ServicesProvider) DeleteQos(ctx context.Context, name string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	p.mu.Lock()
	var boundIfaces []string
	for ifname, pol := range p.bound {
		if pol == name {
			boundIfaces = append(boundIfaces, ifname)
		}
	}
	for _, ifname := range boundIfaces {
		delete(p.bound, ifname)
	}
	delete(p.policer, name)
	p.mu.Unlock()
	for _, ifname := range boundIfaces {
		if idx, ok, err := c.SwInterfaceIndex(ifname); err == nil && ok {
			if err := c.PolicerInput(idx, name, false); err != nil {
				return fmt.Errorf("解绑接口 %s 的 policer %s: %w", ifname, name, err)
			}
		}
	}
	if _, err := c.PolicerAddDel(name, 0, 0, false); err != nil {
		return fmt.Errorf("删除 policer %s: %w", name, err)
	}
	return nil
}

// ApplyInterface 下发接口层配置：MTU 与 ingress-policy（policer 入向绑定）。
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
	p.mu.Lock()
	prev := p.bound[iface.Name]
	if prev != iface.IngressPolicy {
		if iface.IngressPolicy != "" {
			p.bound[iface.Name] = iface.IngressPolicy
		} else {
			delete(p.bound, iface.Name)
		}
	}
	p.mu.Unlock()
	if prev != iface.IngressPolicy {
		if prev != "" {
			if err := c.PolicerInput(idx, prev, false); err != nil {
				return fmt.Errorf("解绑接口 %s 原策略 %s: %w", iface.Name, prev, err)
			}
		}
		if iface.IngressPolicy != "" {
			// policer 须先由 ApplyQos 创建（apply 顺序保证）
			if err := c.PolicerInput(idx, iface.IngressPolicy, true); err != nil {
				return fmt.Errorf("绑定接口 %s 策略 %s: %w", iface.Name, iface.IngressPolicy, err)
			}
		}
	}
	return nil
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
