package netkernel

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// NAT 表/链名（产品自持的独立 nftables 表，不与宿主既有规则互相污染）。
const (
	natTable      = "nfvis-nat"
	natChainPost  = "postrouting"
	natChainPre   = "prerouting"
	natFamilyName = "inet"
)

// nftRun 执行一条 nft 命令并返回输出与错误。
func (p *Provider) nftRun(ctx context.Context, args ...string) (string, error) {
	return p.nft(ctx, args...)
}

// nftIdem 执行一条「对象可能已存在」的 nft 命令（幂等）。
func (p *Provider) nftIdem(ctx context.Context, args ...string) error {
	out, err := p.nftRun(ctx, args...)
	if err == nil || alreadyExists(out, err) {
		return nil
	}
	return fmt.Errorf("nft %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// nftTolerant 执行一条「对象可能不存在」的 nft 命令。
func (p *Provider) nftTolerant(ctx context.Context, args ...string) error {
	out, err := p.nftRun(ctx, args...)
	if err == nil || notFound(out, err) || alreadyExists(out, err) {
		return nil
	}
	return fmt.Errorf("nft %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// ApplyNAT 收敛 NAT44 声明为 nftables 规则（声明式全量：先清本产品的两条链，再按配置重建）。
//
// 映射：NAT 规则 → postrouting 链的 `ip saddr <匹配源>` + `oifname <出接口> masquerade`
// 或 `snat ip to <地址池>`；静态映射 → prerouting 链的 `ip daddr <外部地址> dnat ip to <内部地址>`。
// 出接口/地址池两者按声明取一（校验层保证互斥）。
func (p *Provider) ApplyNAT(ctx context.Context, nat model.NatConfig) error {
	if err := p.nftIdem(ctx, "add", "table", natFamilyName, natTable); err != nil {
		return err
	}
	if err := p.nftIdem(ctx, "add", "chain", natFamilyName, natTable, natChainPost,
		"{", "type", "nat", "hook", "postrouting", "priority", "srcnat", ";", "}"); err != nil {
		return err
	}
	if err := p.nftIdem(ctx, "add", "chain", natFamilyName, natTable, natChainPre,
		"{", "type", "nat", "hook", "prerouting", "priority", "dstnat", ";", "}"); err != nil {
		return err
	}
	// 全量重建：本产品的表由产品独占，先清空再按声明写，删除规则自然收敛。
	if err := p.nftTolerant(ctx, "flush", "chain", natFamilyName, natTable, natChainPost); err != nil {
		return err
	}
	if err := p.nftTolerant(ctx, "flush", "chain", natFamilyName, natTable, natChainPre); err != nil {
		return err
	}
	pools := map[string]string{}
	for _, sp := range nat.SourcePools {
		pools[sp.Name] = sp.AddressRange
	}
	for _, r := range nat.Rules {
		rule := natRuleArgs(r, pools)
		if len(rule) == 0 {
			continue
		}
		if err := p.nftIdem(ctx, rule...); err != nil {
			return err
		}
	}
	for _, s := range nat.Static {
		if s.InsideIP == "" || s.OutsideIP == "" {
			continue
		}
		if err := p.nftIdem(ctx, "add", "rule", natFamilyName, natTable, natChainPre,
			"ip", "daddr", s.OutsideIP, "dnat", "ip", "to", s.InsideIP); err != nil {
			return err
		}
	}
	return nil
}

// natRuleArgs 把一条 NAT 规则翻译成 nft 参数（无可表达的动作时返回空）。
//
// pools 把地址池**名**解析为地址区间：配置里 Action.SourcePool 存的是池名，
// 直接写进 nft 规则会把池名当地址，规则必然无效（数据面静默不通）。
func natRuleArgs(r model.NatRule, pools map[string]string) []string {
	if r.MatchSource == "" {
		return nil
	}
	args := []string{"add", "rule", natFamilyName, natTable, natChainPost, "ip", "saddr", r.MatchSource}
	switch {
	case r.Action.SourcePool != "":
		pool, ok := pools[r.Action.SourcePool]
		if !ok {
			return nil // 引用了不存在的地址池（提交期校验会拦，这里不生成无效规则）
		}
		args = append(args, "snat", "ip", "to", poolRange(pool))
	case r.Action.Interface != "":
		args = append(args, "oifname", r.Action.Interface, "masquerade")
	default:
		return nil
	}
	return args
}

// poolRange 把产品侧地址池写法（`<ip> to <ip>`）转成 nft 的地址区间写法。
func poolRange(rangeSpec string) string {
	s := strings.TrimSpace(rangeSpec)
	lower := strings.SplitN(s, " to ", 2)
	if len(lower) == 2 {
		return strings.TrimSpace(lower[0]) + "-" + strings.TrimSpace(lower[1])
	}
	return s
}
