package network

// 决策 #389：端口安全 v1（per-port 允许 MAC 白名单，FR-NET-011）。
//
// 数据面＝VPP macip ACL——与 #341 的「放行非 IP 帧伴随 ACL」同一机制、同一绑定槽
// （每接口只能有一个 macip 绑定；互斥由提交校验双向拒绝，本层不做二次判定）：
//   - 每接口一张 ACL，tag `nfvis-port-sec-<if>`；
//   - 规则＝每条白名单 MAC 两条 permit（v4 any + MAC 精确、v6 any + MAC 精确；v4 条目
//     同时承载 ARP 等非 IP 帧——同 #341 的 macip 语义）+ 末尾**显式 deny-all**（v4/v6 各一）。
//     显式 deny 必不可少：macip **无匹配默认＝放行**（round170 真机对照实证——单 permit
//     不带 deny 时伪造源照样通过），它是白名单语义成立的必要条件；
//   - 下发走 macip_acl_add_replace 整体替换（tag 反查复用索引，#341 同法），白名单变更
//     即整体替换（无逐条增量）；清空＝解绑。
//
// 生命周期：白名单随接口声明走（L2Network.ApplyInterface 调本 Provider）——提交编排的
// interface[<n>] 计划操作 apply(new)/undo=apply(old) 天然覆盖；恢复重放（EnsureConsistent
// 逐接口）按 tag 反查：有则整体替换 + 确保已绑、无则建 + 绑。
//
// 登记（进程内）语义＝**最后一个成功下发的状态**（决策 #363 口径）：任何一步失败即返回，
// 登记停在最后成功态，重试不会因「登记说已下发」而静默跳过；失效（VPP 重启/重连）走
// reset 清空，由恢复收敛按声明全量重放（幂等）。
//
// 如实边界（v1）：
//   - macip ACL **没有 delete 消息**——解绑后 ACL 对象残留在 VPP（tag 仍在 macip_acl_dump）
//     直到数据面重启；读视图/手册如实说明。该残留不进 #321 的 ACL 残渣对账（那是
//     acl_dump 的视野，macip 属另一类对象）；
//   - 解绑以进程内登记为准：配置在 nfvisd 停机窗口被带外替换（不走产品提交路径）留下的
//     绑定不在本路径，随 request vpp restart 消失（与 #341 伴随 macip 的解绑同一口径）；
//   - macip **无逐规则命中计数**（插件 err 族为空，真机实证）——读视图不给命中数，不编造。

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// macExactMask MAC 精确匹配掩码（逐位比较）；zeroMAC/mask 0 ＝不比较 MAC（匹配任意）。
const (
	macExactMask = "ff:ff:ff:ff:ff:ff"
	zeroMAC      = "00:00:00:00:00:00"
)

// PortSecTagPrefix 端口安全 macip ACL 的 tag 前缀（完整 tag＝前缀 + 接口名；
// VPP tag 为 string[64]，本产品仅物理口可配白名单，不会超）。
const PortSecTagPrefix = "nfvis-port-sec-"

// PortSecTag 该接口的端口安全 macip ACL tag。
func PortSecTag(ifname string) string { return PortSecTagPrefix + ifname }

// MacipRuleSpec 与底座解耦的 macip 规则形态（#341 的单规则与 #389 的白名单规则共用；
// 便于单测钉住规则形状而不触 govpp 类型）。
type MacipRuleSpec struct {
	Permit     bool
	SrcPrefix  string // "0.0.0.0/0"（v4 any，含 ARP 等非 IP 帧）/ "::/0"（v6 any）
	SrcMAC     string // permit＝精确匹配的白名单 MAC；deny-all 用全零（配合 mask 0）
	SrcMACMask string // "ff:ff:ff:ff:ff:ff"＝逐位比较；"00:00:00:00:00:00"＝不比较（任意 MAC）
}

// BuildPortSecRules 白名单 → macip 规则集（纯函数，单测钉形状）：
// 每条 MAC 两条 permit（v4/v6 any + MAC 精确）+ 末尾显式 deny-all（v4/v6 各一）。
// 顺序＝先全部 permit 后 deny（首个命中生效；deny 在前会让白名单永不命中）。
func BuildPortSecRules(macs []string) []MacipRuleSpec {
	rules := make([]MacipRuleSpec, 0, 2*len(macs)+2)
	for _, m := range macs {
		rules = append(rules,
			MacipRuleSpec{Permit: true, SrcPrefix: "0.0.0.0/0", SrcMAC: m, SrcMACMask: macExactMask},
			MacipRuleSpec{Permit: true, SrcPrefix: "::/0", SrcMAC: m, SrcMACMask: macExactMask},
		)
	}
	// deny-all：macip 无匹配默认＝放行（真机对照实证）——白名单外的源 MAC 一律丢弃
	// 全靠这两条。mask 0＝不比较 MAC ⇒ 匹配任意源；SrcMAC 取值无关，取全零。
	return append(rules,
		MacipRuleSpec{Permit: false, SrcPrefix: "0.0.0.0/0", SrcMAC: zeroMAC, SrcMACMask: zeroMAC},
		MacipRuleSpec{Permit: false, SrcPrefix: "::/0", SrcMAC: zeroMAC, SrcMACMask: zeroMAC},
	)
}

// PortSecClient 端口安全的 VPP 能力集（macip 消息属 acl 插件——govpp 实现与 ACL 客户端
// 同一通道、同一结构体，独立接口只为让本 Provider 依赖最小能力集）。
type PortSecClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	// MacipACLAddReplaceRules 创建/整体替换自定义规则集的 macip ACL（见 acl_govpp.go）。
	MacipACLAddReplaceRules(index uint32, tag string, rules []MacipRuleSpec) (uint32, error)
	// MacipACLInterfaceAddDel 绑定/解绑接口的 macip ACL（isAdd=true 绑、false 解）。
	MacipACLInterfaceAddDel(swIfIndex, aclIndex uint32, isAdd bool) error
	// MacipACLByTag 按 tag 反查：索引、实测规则数、是否在场（恢复重放 + 读视图）。
	MacipACLByTag(tag string) (uint32, int, bool, error)
	// MacipBoundACL 读接口当前绑定的 macip ACL 索引（绑定实况唯一事实源，读视图用）。
	MacipBoundACL(swIfIndex uint32) (uint32, bool, error)
	Close()
}

// PortSecDataplane 接口端口安全的数据面实况（读视图，全部为实测值；取不到就给原因）。
type PortSecDataplane struct {
	// Available 能否核对数据面（接口可解析 + 底座可查）；false 时 Reason 说明原因。
	Available bool
	Reason    string
	Tag       string
	// TagPresent 该接口的 macip ACL 是否在数据面（tag 反查命中）。
	TagPresent bool
	ACLIndex   uint32
	// RuleCount 实测规则数（应为 2×白名单条数 + 2；与配置不一致即未收敛）。
	RuleCount int
	// Bound 本接口的 macip 绑定槽上是否正是本产品的 ACL（绑定实况与 tag 反查**同索引**才算）。
	Bound      bool
	BoundIndex uint32
}

// portSecRT 接口级登记＝最后一个成功下发的状态（决策 #363）。
type portSecRT struct {
	aclIndex uint32
	macs     []string
	bound    bool
}

// PortSecProvider 接口端口安全编排（决策 #389）。
type PortSecProvider struct {
	client func() (PortSecClient, error)

	mu sync.Mutex
	rt map[string]*portSecRT
}

// NewPortSecProvider 以固定客户端构造（测试）。
func NewPortSecProvider(c PortSecClient) *PortSecProvider {
	return &PortSecProvider{client: func() (PortSecClient, error) { return c, nil }, rt: map[string]*portSecRT{}}
}

// NewPortSecProviderFunc 以客户端工厂构造（连接可重连）。
func NewPortSecProviderFunc(f func() (PortSecClient, error)) *PortSecProvider {
	return &PortSecProvider{client: f, rt: map[string]*portSecRT{}}
}

// reset 清空进程内登记（恢复收敛/重连前调用：登记清空后 ApplyInterface 按 tag 反查
// 重建/复用，幂等）。
func (p *PortSecProvider) reset() {
	p.mu.Lock()
	p.rt = map[string]*portSecRT{}
	p.mu.Unlock()
}

// portSecDesired 白名单的期望形态（归一小写；解码层已归一，此处防御同一份代码直构配置）。
func portSecDesired(iface model.InterfaceConfig) []string {
	out := make([]string, 0, len(iface.PortSecurity))
	for _, m := range iface.PortSecurity {
		out = append(out, strings.ToLower(string(m)))
	}
	return out
}

// applyDone 判断期望是否与登记一致（一致＝幂等重跑，零 VPP 调用）。
func (p *PortSecProvider) applyDone(ifname string, want []string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rt[ifname]
	if len(want) == 0 {
		return rt == nil // 有登记（哪怕 bound=false）也要走 teardown 清掉
	}
	if rt == nil || len(rt.macs) != len(want) || !rt.bound {
		return false
	}
	for i := range want {
		if rt.macs[i] != want[i] {
			return false
		}
	}
	return true
}

// ApplyInterface 把接口的端口安全收敛到声明：白名单非空＝建/替换 ACL + 绑定；
// 空＝解绑（停用）。接口不在数据面时按 ErrIfaceUnavailable 返回（由提交编排决定
// 「延后收敛」与否，同 storm/接口层口径）。
func (p *PortSecProvider) ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error {
	want := portSecDesired(iface)
	if p.applyDone(iface.Name, want) {
		return nil // 幂等：状态已一致，零 VPP 调用
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	swIf, ok, err := c.SwInterfaceIndex(iface.Name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, iface.Name)
	}
	if len(want) == 0 {
		return p.teardown(c, iface.Name, swIf)
	}
	return p.apply(c, iface.Name, swIf, want)
}

// apply 下发白名单：索引取登记 → tag 反查兜底（恢复重放/登记丢失时复用既有 ACL，
// 不重复创建）→ 整体替换 → 绑定。登记按步骤推进（#363）。
func (p *PortSecProvider) apply(c PortSecClient, ifname string, swIf uint32, want []string) error {
	tag := PortSecTag(ifname)
	p.mu.Lock()
	rt := p.rt[ifname]
	known, idx := rt != nil, uint32(0)
	if known {
		idx = rt.aclIndex
	}
	p.mu.Unlock()

	if !known {
		got, _, found, err := c.MacipACLByTag(tag)
		if err != nil {
			return err
		}
		if found {
			known, idx = true, got
		}
	}
	if !known {
		idx = aclIndexNew // ~0：新建（0 是合法索引，不能当「未下发」）
	}

	// 整体替换（macip_acl_add_replace 同索引覆盖旧规则集——白名单变更无逐条增量）。
	newIdx, err := c.MacipACLAddReplaceRules(idx, tag, BuildPortSecRules(want))
	if err != nil {
		return err
	}
	// 先登记索引与规则集：即便随后绑定失败，重试/重放也能复用同一 ACL 而不重复创建
	//（登记=最后一个成功下发的状态；replace 成功即「规则集已是新值」是事实）。
	p.setRT(ifname, newIdx, want, false)

	if err := c.MacipACLInterfaceAddDel(swIf, newIdx, true); err != nil {
		// 已绑定＝目标状态（跨 nfvisd 重启时 VPP 侧绑定可能仍在；同 #341 容忍口径）。
		if !vppErrIs(err, vppValueExist) {
			return err
		}
	}
	p.setRT(ifname, newIdx, want, true)
	return nil
}

// teardown 解绑接口的端口安全白名单（清空＝停用）。幂等：无登记＝本进程从未下发过，
// 无对象可解（边界见文件头）；解绑方向的「本就不在/接口已消失」按已达成处理（同
// MacipDisallowNonIP / #342 口径）。
func (p *PortSecProvider) teardown(c PortSecClient, ifname string, swIf uint32) error {
	p.mu.Lock()
	rt := p.rt[ifname]
	p.mu.Unlock()
	if rt == nil {
		return nil
	}
	if rt.bound {
		if err := c.MacipACLInterfaceAddDel(swIf, rt.aclIndex, false); err != nil {
			if !vppErrIs(err, vppNoSuchEntry, vppValueExist) && !isMissingIfaceErr(err) {
				return err
			}
		}
	}
	p.clearRT(ifname)
	return nil
}

// 登记读写（锁内小步；VPP 调用一律在锁外）。

func (p *PortSecProvider) setRT(ifname string, aclIndex uint32, macs []string, bound bool) {
	p.mu.Lock()
	p.rt[ifname] = &portSecRT{aclIndex: aclIndex, macs: append([]string{}, macs...), bound: bound}
	p.mu.Unlock()
}

func (p *PortSecProvider) clearRT(ifname string) {
	p.mu.Lock()
	delete(p.rt, ifname)
	p.mu.Unlock()
}

// Dataplane 读该接口端口安全的数据面实况（读视图用，全部为实测值）：
//   - tag 反查：ACL 是否在场与实测规则数（macip_acl_dump）；
//   - 绑定实况：接口 macip 绑定槽上的索引是否正是本产品的 ACL（macip_acl_interface_list_dump）。
//
// 不提供命中计数：macip 无逐规则计数且插件 err 族为空（真机实证）——如实说明，不编造 0。
func (p *PortSecProvider) Dataplane(ctx context.Context, ifname string) (PortSecDataplane, error) {
	c, err := p.client()
	if err != nil {
		return PortSecDataplane{Reason: err.Error()}, nil
	}
	defer c.Close()
	swIf, ok, err := c.SwInterfaceIndex(ifname)
	if err != nil {
		return PortSecDataplane{}, err
	}
	if !ok {
		return PortSecDataplane{Reason: "接口不在数据面（未由 DPDK 接管或名称不一致）"}, nil
	}
	out := PortSecDataplane{Available: true, Tag: PortSecTag(ifname)}
	aclIdx, ruleCount, found, err := c.MacipACLByTag(out.Tag)
	if err != nil {
		return PortSecDataplane{Reason: "读取 macip ACL 失败：" + err.Error()}, nil
	}
	if found {
		out.TagPresent, out.ACLIndex, out.RuleCount = true, aclIdx, ruleCount
	}
	boundIdx, bound, err := c.MacipBoundACL(swIf)
	if err != nil {
		return PortSecDataplane{Reason: "读取接口 macip 绑定失败：" + err.Error()}, nil
	}
	if bound {
		out.BoundIndex = boundIdx
		// 槽上正是本产品的 ACL 才算「已绑定」：槽被别的 macip ACL（如 #341 伴随）
		// 占用时如实显示未绑定——这本身就是需要解决的实况。
		out.Bound = found && boundIdx == aclIdx
	}
	return out, nil
}
