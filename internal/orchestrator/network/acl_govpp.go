package network

// govpp ACL 客户端（M3-5 二）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/acl"
	"go.fd.io/govpp/binapi/acl_types"
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
