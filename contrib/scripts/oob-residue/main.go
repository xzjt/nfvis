// oob-residue —— 残渣对账的**带外对象**验证工具（手动，非产品功能）。
//
// 验证用途：为决策 #321 的残渣对账（ACL_LEFTOVER / BRIDGE_DOMAIN_LEFTOVER）**造带外对象**。
// 产品建 bridge-domain 时把交换机名写进 binapi 的 BdTag 字段（见
// internal/orchestrator/network/l2_govpp.go 的 BridgeDomainAddDel），残渣对账据此区分
// 「产品的 BD」与「手工/残留的 BD」——无名 BD 不参与对账。要复验这条路径，必须造出
// **带 tag 但配置未声明**的 BD/ACL，而 vppctl 的 create/set bridge-domain 在本版本写不出 BdTag，
// 故本工具直接用 govpp/binapi 调 bridge_domain_add_del / acl_add_replace（与产品同一条 API 路径，
// 因此就是「带外」手段）。
//
// 非产品功能，不进任何发布件：它只在真机上手工运行，用来构造与清理验证用的带外对象。
//
// 用法（在 nfvis-vm 上，root）：
//
//	go run ./contrib/scripts/oob-residue -action create-bd      # 建带 tag 的 BD
//	go run ./contrib/scripts/oob-residue -action delete-bd      # 删该 BD
//	go run ./contrib/scripts/oob-residue -action create-acl     # 建带 tag 的 ACL
//	go run ./contrib/scripts/oob-residue -action delete-acl     # 删该 ACL
//	go run ./contrib/scripts/oob-residue -action list           # 列出数据面 BD/ACL 及其 tag
//
// 参数：-socket（缺省 /run/vpp/api.sock）、-bd-id（缺省 4094）、-bd-tag（缺省 oob-verify-residue）、
// -acl-tag（缺省 oob-verify-acl）。构建（开发机交叉编译后可 scp 上机）：
//
//	GOOS=linux GOARCH=amd64 go build -o oob-residue ./contrib/scripts/oob-residue
package main

import (
	"flag"
	"fmt"
	"os"

	"go.fd.io/govpp/adapter/socketclient"
	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/acl"
	"go.fd.io/govpp/binapi/acl_types"
	"go.fd.io/govpp/binapi/ip_types"
	"go.fd.io/govpp/binapi/l2"
	"go.fd.io/govpp/core"
)

func main() {
	var (
		socket = flag.String("socket", "/run/vpp/api.sock", "VPP binary API socket")
		action = flag.String("action", "", "list | create-bd | delete-bd | create-acl | delete-acl")
		bdID   = flag.Uint("bd-id", 4094, "bridge-domain id")
		bdTag  = flag.String("bd-tag", "oob-verify-residue", "bridge-domain tag (BD-Tag)")
		aclTag = flag.String("acl-tag", "oob-verify-acl", "ACL tag")
	)
	flag.Parse()
	if *action == "" {
		fmt.Fprintln(os.Stderr, "缺少 -action（list | create-bd | delete-bd | create-acl | delete-acl）")
		flag.Usage()
		os.Exit(2)
	}

	conn, err := core.Connect(socketclient.NewVppClient(*socket))
	if err != nil {
		fatal("连接 VPP binary API %s: %v", *socket, err)
	}
	defer conn.Disconnect()
	ch, err := conn.NewAPIChannel()
	if err != nil {
		fatal("打开 API channel: %v", err)
	}
	defer ch.Close()

	switch *action {
	case "list":
		listBDs(ch)
		listACLs(ch)
	case "create-bd":
		createBD(ch, uint32(*bdID), *bdTag)
	case "delete-bd":
		deleteBD(ch, uint32(*bdID))
	case "create-acl":
		createACL(ch, *aclTag)
	case "delete-acl":
		deleteACL(ch, *aclTag)
	default:
		fatal("未知 -action %q", *action)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// createBD 用与产品相同的 binapi 字段（BdTag）建一个带 tag 的 bridge-domain。
func createBD(ch api.Channel, id uint32, tag string) {
	reply := &l2.BridgeDomainAddDelReply{}
	err := ch.SendRequest(&l2.BridgeDomainAddDel{
		BdID: id, Flood: true, UuFlood: true, Forward: true, Learn: false,
		BdTag: tag, IsAdd: true,
	}).ReceiveReply(reply)
	if err != nil {
		fatal("bridge_domain_add_del(add, bd=%d, tag=%q): %v", id, tag, err)
	}
	if reply.Retval != 0 {
		fatal("bridge_domain_add_del(add, bd=%d, tag=%q): retval=%d", id, tag, reply.Retval)
	}
	fmt.Printf("已建 bridge-domain bd_id=%d bd_tag=%q\n", id, tag)
}

// deleteBD 按 bd_id 删 bridge-domain。
func deleteBD(ch api.Channel, id uint32) {
	reply := &l2.BridgeDomainAddDelReply{}
	err := ch.SendRequest(&l2.BridgeDomainAddDel{
		BdID: id, IsAdd: false,
	}).ReceiveReply(reply)
	if err != nil {
		fatal("bridge_domain_add_del(del, bd=%d): %v", id, err)
	}
	if reply.Retval != 0 {
		fatal("bridge_domain_add_del(del, bd=%d): retval=%d", id, reply.Retval)
	}
	fmt.Printf("已删 bridge-domain bd_id=%d\n", id)
}

// createACL 建一个带 tag 的 ACL（单条 permit-any 规则，仅为让 tag 出现在数据面）。
func createACL(ch api.Channel, tag string) {
	any4, err := ip_types.ParsePrefix("0.0.0.0/0")
	if err != nil {
		fatal("解析前缀: %v", err)
	}
	rule := acl_types.ACLRule{
		IsPermit:               acl_types.ACL_ACTION_API_PERMIT,
		SrcPrefix:              any4,
		DstPrefix:              any4,
		Proto:                  0, // any
		SrcportOrIcmptypeFirst: 0, SrcportOrIcmptypeLast: 65535,
		DstportOrIcmpcodeFirst: 0, DstportOrIcmpcodeLast: 65535,
	}
	reply := &acl.ACLAddReplaceReply{}
	err = ch.SendRequest(&acl.ACLAddReplace{
		ACLIndex: ^uint32(0), // ~0 = 新建
		Tag:      tag,
		Count:    1,
		R:        []acl_types.ACLRule{rule},
	}).ReceiveReply(reply)
	if err != nil {
		fatal("acl_add_replace(tag=%q): %v", tag, err)
	}
	if reply.Retval != 0 {
		fatal("acl_add_replace(tag=%q): retval=%d", tag, reply.Retval)
	}
	fmt.Printf("已建 ACL index=%d tag=%q\n", reply.ACLIndex, tag)
}

// deleteACL 按 tag 反查索引后删除（acl_dump 全量，与产品 ACLIndexByTag 同路径）。
func deleteACL(ch api.Channel, tag string) {
	idx, found, err := aclIndexByTag(ch, tag)
	if err != nil {
		fatal("acl_dump: %v", err)
	}
	if !found {
		fmt.Printf("ACL tag=%q 不存在，无需删除\n", tag)
		return
	}
	reply := &acl.ACLDelReply{}
	if err := ch.SendRequest(&acl.ACLDel{ACLIndex: idx}).ReceiveReply(reply); err != nil {
		fatal("acl_del(index=%d): %v", idx, err)
	}
	if reply.Retval != 0 {
		fatal("acl_del(index=%d): retval=%d", idx, reply.Retval)
	}
	fmt.Printf("已删 ACL index=%d tag=%q\n", idx, tag)
}

func aclIndexByTag(ch api.Channel, tag string) (uint32, bool, error) {
	reqCtx := ch.SendMultiRequest(&acl.ACLDump{ACLIndex: ^uint32(0)})
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

// listBDs 打印数据面全部 bridge-domain 的 bd_id 与 BdTag（残渣对账的判据字段）。
func listBDs(ch api.Channel) {
	reqCtx := ch.SendMultiRequest(&l2.BridgeDomainDump{
		BdID:      ^uint32(0),
		SwIfIndex: 0xFFFFFFFF,
	})
	fmt.Println("bridge-domain:")
	for {
		d := &l2.BridgeDomainDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			fatal("bridge_domain_dump: %v", err)
		}
		if stop {
			break
		}
		fmt.Printf("  bd_id=%-10d bd_tag=%q\n", d.BdID, d.BdTag)
	}
}

// listACLs 打印数据面全部 ACL 的 index 与 tag。
func listACLs(ch api.Channel) {
	reqCtx := ch.SendMultiRequest(&acl.ACLDump{ACLIndex: ^uint32(0)})
	fmt.Println("acl:")
	for {
		d := &acl.ACLDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			fatal("acl_dump: %v", err)
		}
		if stop {
			break
		}
		fmt.Printf("  acl_index=%-4d tag=%q\n", d.ACLIndex, d.Tag)
	}
}
