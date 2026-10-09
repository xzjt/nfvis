package main

import (
	"fmt"
	"os"
	"time"

	"go.fd.io/govpp/adapter/socketclient"
	"go.fd.io/govpp/binapi/classify"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/policer"
	"go.fd.io/govpp/core"
)

func main() {
	sock := "/run/vpp/api.sock"
	if len(os.Args) > 1 {
		sock = os.Args[1]
	}
	client := socketclient.NewVppClient(sock)
	conn, ev, err := core.AsyncConnect(client, 3, time.Second)
	if err != nil {
		fmt.Println("connect err:", err)
		os.Exit(1)
	}
	// 等连接就绪（govpp 异步连接：必须等 Connected 事件后才可用）
	ready := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case e := <-ev:
			if e.State == core.Connected {
				ready = true
			} else {
				fmt.Printf("  event: state=%v err=%v\n", e.State, e.Error)
			}
		case <-time.After(500 * time.Millisecond):
		}
		if ready {
			break
		}
	}
	if !ready {
		fmt.Println("connect timeout")
		os.Exit(1)
	}
	defer conn.Disconnect()
	ch, err := conn.NewAPIChannel()
	if err != nil {
		fmt.Println("channel err:", err)
		os.Exit(1)
	}
	defer ch.Close()

	fmt.Println("== classify_table_ids ==")
	ids := &classify.ClassifyTableIdsReply{}
	if err := ch.SendRequest(&classify.ClassifyTableIds{}).ReceiveReply(ids); err != nil {
		fmt.Println("  err:", err)
	} else {
		fmt.Printf("  retval=%d ids=%v\n", ids.Retval, ids.Ids)
		for _, id := range ids.Ids {
			ti := &classify.ClassifyTableInfoReply{}
			if err := ch.SendRequest(&classify.ClassifyTableInfo{TableID: id}).ReceiveReply(ti); err != nil {
				fmt.Printf("  table %d info err: %v\n", id, err)
				continue
			}
			fmt.Printf("  table %d: retval=%d mask=%x nvec=%d next=%d activeSessions=%d\n",
				id, ti.Retval, ti.Mask, ti.MatchNVectors, ti.NextTableIndex, ti.ActiveSessions)
		}
	}

	fmt.Println("== sw_interface_dump (name->index) ==")
	idxByName := map[string]uint32{}
	req := ch.SendMultiRequest(&ifapi.SwInterfaceDump{})
	for {
		d := &ifapi.SwInterfaceDetails{}
		stop, err := req.ReceiveReply(d)
		if err != nil {
			fmt.Println("  dump err:", err)
			break
		}
		if stop {
			break
		}
		idxByName[d.InterfaceName] = uint32(d.SwIfIndex)
	}
	for n, i := range idxByName {
		fmt.Printf("  %s = %d\n", n, i)
	}

	fmt.Println("== classify_table_by_interface (per iface) ==")
	for n, i := range idxByName {
		r := &classify.ClassifyTableByInterfaceReply{}
		err := ch.SendRequest(&classify.ClassifyTableByInterface{
			SwIfIndex: interface_types.InterfaceIndex(i),
		}).ReceiveReply(r)
		if err != nil {
			fmt.Printf("  %s(%d): err=%v\n", n, i, err)
			continue
		}
		fmt.Printf("  %s(%d): retval=%d l2=%d ip4=%d ip6=%d\n", n, i, r.Retval, r.L2TableID, r.IP4TableID, r.IP6TableID)
	}

	fmt.Println("== policer_classify_dump (all types) ==")
	for _, t := range []classify.PolicerClassifyTable{
		classify.POLICER_CLASSIFY_API_TABLE_L2,
		classify.POLICER_CLASSIFY_API_TABLE_IP4,
		classify.POLICER_CLASSIFY_API_TABLE_IP6,
	} {
		creq := ch.SendMultiRequest(&classify.PolicerClassifyDump{
			Type:      t,
			SwIfIndex: interface_types.InterfaceIndex(^uint32(0)),
		})
		n := 0
		for {
			d := &classify.PolicerClassifyDetails{}
			stop, err := creq.ReceiveReply(d)
			if err != nil {
				fmt.Printf("  type=%v dump err: %v\n", t, err)
				break
			}
			if stop {
				break
			}
			n++
			fmt.Printf("  type=%v swIfIndex=%d tableIndex=%d\n", t, d.SwIfIndex, d.TableIndex)
		}
		fmt.Printf("  type=%v entries=%d\n", t, n)
	}

	fmt.Println("== policer_dump ==")
	preq := ch.SendMultiRequest(&policer.PolicerDump{})
	for {
		d := &policer.PolicerDetails{}
		stop, err := preq.ReceiveReply(d)
		if err != nil {
			fmt.Println("  dump err:", err)
			break
		}
		if stop {
			break
		}
		fmt.Printf("  name=%s cir=%d type=%v\n", d.Name, d.Cir, d.Type)
	}
}
