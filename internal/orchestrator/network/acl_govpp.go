package network

// govpp ACL 客户端（M3-5 二）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/acl"
	"go.fd.io/govpp/binapi/acl_types"
	"go.fd.io/govpp/binapi/ethernet_types"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/ip_types"
)

// AclClientFunc 返回随当前连接获取 ACL 客户端的工厂。
func (m *Manager) AclClientFunc() func() (ACLClient, error) {
	return func() (ACLClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppAclClient{ch: ch}, nil
	}
}

type govppAclClient struct{ ch api.Channel }

func (g *govppAclClient) Close() { g.ch.Close() }

// ACLIndexByTag 经 acl_dump（~0 = 全量）按 tag 反查已存在 ACL 的索引。
// 恢复收敛用：nfvisd 重启后登记表为空，但 VPP 侧可能已有同名 ACL，据此走 replace。
func (g *govppAclClient) ACLIndexByTag(tag string) (uint32, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&acl.ACLDump{ACLIndex: ^uint32(0)})
	for {
		d := &acl.ACLDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return 0, false, err
		}
		if stop {
			return 0, false, nil
		}
		if d.Tag == tag {
			return d.ACLIndex, true, nil
		}
	}
}

// ACLTags 经 acl_dump（~0 = 全量）列出 VPP 里全部 ACL 的 tag。
//
// 残渣对账用（决策 #321）：tag 不在配置里即补偿失败留下的 ACL 残渣。
func (g *govppAclClient) ACLTags() ([]string, error) {
	reqCtx := g.ch.SendMultiRequest(&acl.ACLDump{ACLIndex: ^uint32(0)})
	var out []string
	for {
		d := &acl.ACLDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			return out, nil
		}
		out = append(out, d.Tag)
	}
}

func (g *govppAclClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

func (g *govppAclClient) ACLAddReplace(index uint32, tag string, rules []ACLRuleSpec) (uint32, error) {
	binRules := make([]acl_types.ACLRule, 0, len(rules))
	for _, r := range rules {
		src, err := ip_types.ParsePrefix(r.Src)
		if err != nil {
			return 0, fmt.Errorf("源前缀 %q: %w", r.Src, err)
		}
		dst, err := ip_types.ParsePrefix(r.Dst)
		if err != nil {
			return 0, fmt.Errorf("目的前缀 %q: %w", r.Dst, err)
		}
		action := acl_types.ACL_ACTION_API_DENY
		if r.Permit {
			action = acl_types.ACL_ACTION_API_PERMIT
		}
		binRules = append(binRules, acl_types.ACLRule{
			IsPermit:               action,
			SrcPrefix:              src,
			DstPrefix:              dst,
			Proto:                  ip_types.IPProto(r.Proto),
			SrcportOrIcmptypeFirst: r.SPortFrom,
			SrcportOrIcmptypeLast:  r.SPortTo,
			DstportOrIcmpcodeFirst: r.DPortFrom,
			DstportOrIcmpcodeLast:  r.DPortTo,
		})
	}
	reply := &acl.ACLAddReplaceReply{}
	if err := g.ch.SendRequest(&acl.ACLAddReplace{
		ACLIndex: index, Tag: tag, Count: uint32(len(binRules)), R: binRules,
	}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("acl_add_replace(%s,index=%d) retval=%d", tag, index, reply.Retval)
	}
	return reply.ACLIndex, nil
}

func (g *govppAclClient) ACLDel(index uint32) error {
	reply := &acl.ACLDelReply{}
	if err := g.ch.SendRequest(&acl.ACLDel{ACLIndex: index}).ReceiveReply(reply); err != nil {
		// 已不存在视为幂等成功
		if vppErrIs(err, vppValueExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("acl_del(index=%d) retval=%d", index, reply.Retval)
	}
	return nil
}

func (g *govppAclClient) ACLInterfaceSet(swIfIndex, inAcl, outAcl uint32, inSet, outSet bool) error {
	// VPP 约定：acls 向量前 n_input 个为入向，其余为出向。
	// 索引 0 是合法 ACL，故用 inSet/outSet 而非 !=0 判断是否存在。
	acls := make([]uint32, 0, 2)
	nInput := uint8(0)
	if inSet {
		acls = append(acls, inAcl)
		nInput = 1
	}
	if outSet {
		acls = append(acls, outAcl)
	}
	reply := &acl.ACLInterfaceSetACLListReply{}
	if err := g.ch.SendRequest(&acl.ACLInterfaceSetACLList{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		Count:     uint8(len(acls)),
		NInput:    nInput,
		Acls:      acls,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("acl_interface_set_acl_list(if=%d,in=%d,out=%d) retval=%d", swIfIndex, inAcl, outAcl, reply.Retval)
	}
	return nil
}

// MacipACLAddReplace 创建/替换伴随的 macip ACL（决策 #341）——薄封装：
// 单规则集（permit 任意 ip/mac，mask 0）经 MacipACLAddReplaceRules 下发，行为不变。
//
// 唯一规则：`permit ip 0.0.0.0/0 mac 00:00:00:00:00:00 mask 0`——mask 0 表示不比较 MAC、
// 前缀 0.0.0.0/0 表示任意源，等价于「放行全部非 IP 帧」。round120 实验室验证（vppctl 同语句）
// 加在绑 ACL 的 L3 接口上后 ARP 立即通、drops 停止增长。本 ACL 只作用于非 IP 帧，
// IPv4/IPv6 仍走已绑的 IP ACL，故 IP 过滤语义不受影响。
func (g *govppAclClient) MacipACLAddReplace(index uint32, tag string) (uint32, error) {
	return g.MacipACLAddReplaceRules(index, tag, []MacipRuleSpec{{
		Permit:     true,
		SrcPrefix:  "0.0.0.0/0",
		SrcMAC:     "00:00:00:00:00:00",
		SrcMACMask: "00:00:00:00:00:00", // mask 0：不比较 MAC（permit 任意）
	}})
}

// MacipACLAddReplaceRules 创建/整体替换一张**自定义规则集**的 macip ACL（决策 #389：
// 把 #341 的单规则伴随 ACL 泛化为任意规则集——端口安全白名单用它下发）。
// index=aclIndexNew（~0）新建，否则按索引整体替换；返回实际索引。
func (g *govppAclClient) MacipACLAddReplaceRules(index uint32, tag string, rules []MacipRuleSpec) (uint32, error) {
	binRules := make([]acl_types.MacipACLRule, 0, len(rules))
	for i, r := range rules {
		src, err := ip_types.ParsePrefix(r.SrcPrefix)
		if err != nil {
			return 0, fmt.Errorf("macip 规则 %d 源前缀 %q: %w", i, r.SrcPrefix, err)
		}
		mac, err := ethernet_types.ParseMacAddress(r.SrcMAC)
		if err != nil {
			return 0, fmt.Errorf("macip 规则 %d 源 MAC %q: %w", i, r.SrcMAC, err)
		}
		mask, err := ethernet_types.ParseMacAddress(r.SrcMACMask)
		if err != nil {
			return 0, fmt.Errorf("macip 规则 %d MAC 掩码 %q: %w", i, r.SrcMACMask, err)
		}
		action := acl_types.ACL_ACTION_API_DENY
		if r.Permit {
			action = acl_types.ACL_ACTION_API_PERMIT
		}
		binRules = append(binRules, acl_types.MacipACLRule{
			IsPermit:   action,
			SrcMac:     mac,
			SrcMacMask: mask,
			SrcPrefix:  src,
		})
	}
	reply := &acl.MacipACLAddReplaceReply{}
	if err := g.ch.SendRequest(&acl.MacipACLAddReplace{
		ACLIndex: index, Tag: tag, Count: uint32(len(binRules)), R: binRules,
	}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("macip_acl_add_replace(%s,index=%d) retval=%d", tag, index, reply.Retval)
	}
	return reply.ACLIndex, nil
}

// MacipACLInterfaceAddDel 绑定/解绑接口的 macip ACL（决策 #341）。
func (g *govppAclClient) MacipACLInterfaceAddDel(swIfIndex, aclIndex uint32, isAdd bool) error {
	reply := &acl.MacipACLInterfaceAddDelReply{}
	if err := g.ch.SendRequest(&acl.MacipACLInterfaceAddDel{
		IsAdd:     isAdd,
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		ACLIndex:  aclIndex,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("macip_acl_interface_add_del(if=%d,acl=%d,add=%v) retval=%d",
			swIfIndex, aclIndex, isAdd, reply.Retval)
	}
	return nil
}

// MacipACLIndexByTag 经 macip_acl_dump（~0 = 全量）按 tag 反查已存在 macip ACL 的索引。
// 恢复收敛用：nfvisd 重启后登记表为空，但 VPP 侧可能已有伴随 macip ACL，据此复用而非重复创建。
func (g *govppAclClient) MacipACLIndexByTag(tag string) (uint32, bool, error) {
	idx, _, found, err := g.MacipACLByTag(tag)
	return idx, found, err
}

// MacipACLByTag 经 macip_acl_dump（~0 = 全量）按 tag 反查：返回索引、**实测规则数**与是否在场。
// 规则数供端口安全的读视图展示（决策 #389；macip 无逐规则命中计数，规则数是 dump 能给出的
// 全部实况）。
func (g *govppAclClient) MacipACLByTag(tag string) (uint32, int, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&acl.MacipACLDump{ACLIndex: ^uint32(0)})
	for {
		d := &acl.MacipACLDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return 0, 0, false, err
		}
		if stop {
			return 0, 0, false, nil
		}
		if d.Tag == tag {
			return d.ACLIndex, int(d.Count), true, nil
		}
	}
}

// MacipBoundACL 读接口当前绑定的 macip ACL 索引（macip_acl_interface_list_dump，
// **绑定实况的唯一事实源**——登记只做增量判据，同 storm 的 AttachedL2Table 口径）。
// macip 每接口只有一个绑定槽；count=0/无应答返回 ok=false——这不区分「本就没绑」与
// 「查询被拒」（两者对本层的动作相同，真出错会在随后的绑定上暴露，不静默）。
func (g *govppAclClient) MacipBoundACL(swIfIndex uint32) (uint32, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&acl.MacipACLInterfaceListDump{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	})
	for {
		d := &acl.MacipACLInterfaceListDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return 0, false, err
		}
		if stop {
			return 0, false, nil
		}
		if len(d.Acls) > 0 {
			return d.Acls[0], true, nil
		}
	}
}

// 编译期断言：govpp 客户端实现可选的反查能力（决策 #341）。
var _ MacipIndexLookup = (*govppAclClient)(nil)
