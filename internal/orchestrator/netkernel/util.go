package netkernel

import (
	"context"
	"encoding/json"
	"strings"
)

// joinArgs 命令参数拼接（错误文案用）。
func joinArgs(args []string) string { return strings.Join(args, " ") }

// trimOut 去掉命令输出的首尾空白（错误文案用）。
func trimOut(out string) string { return strings.TrimSpace(out) }

// bridgeLinkRow `bridge -j link show` 的一行（只取需要的字段）。
type bridgeLinkRow struct {
	Ifname string `json:"ifname"`
	Master string `json:"master"`
}

// bridgeMembers 内核 bridge 的当前成员口名（读失败返回 nil，调用方按「读不到」保守处理）。
func (p *Provider) bridgeMembers(ctx context.Context, br string) []string {
	out, err := p.run.Run(ctx, "bridge", "-j", "link", "show", "master", br)
	if err != nil {
		return nil
	}
	var rows []bridgeLinkRow
	if json.Unmarshal([]byte(out), &rows) != nil {
		return nil
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Ifname != "" {
			names = append(names, r.Ifname)
		}
	}
	return names
}

// ipLinkInfo `ip -d -j link show` 的 linkinfo 对象。
type ipLinkInfo struct {
	// InfoKind 设备自身的类型（bridge/vrf/vlan/vxlan/tun/bond/…；物理口无此项）。
	InfoKind string `json:"info_kind"`
	// SlaveKind 该设备**所属 master** 的类型（vrf/bridge/bond/…；未 enslave 时无此项）。
	SlaveKind string `json:"info_slave_kind"`
	// InfoData 设备类型专属参数（bond 的 mode/xmit_hash_policy，vxlan 的 id/local/remote/dstport…），
	// 由各族按自己的形状解析（RawMessage：不替它们编造形状）。
	InfoData json.RawMessage `json:"info_data"`
}

// ipLinkDetailRow `ip -d -j link show` 的一行（设备类型、归属与类型参数所需字段）。
type ipLinkDetailRow struct {
	Ifname   string      `json:"ifname"`
	Master   string      `json:"master"`
	LinkInfo *ipLinkInfo `json:"linkinfo"`
}

// linkDetail 读一条设备的 `ip -d -j link show` 详情；ok=false 表示读不到/解析不了。
//
// 必须带 **-d**：真机实测（round2 体检 R2-24）`ip -j link show`（不带 -d）对任何设备都不输出
// `linkinfo` 键，据此判设备类型会恒得「physical」。
func (p *Provider) linkDetail(ctx context.Context, dev string) (ipLinkDetailRow, bool) {
	out, err := p.ip(ctx, "-d", "-j", "link", "show", "dev", dev)
	if err != nil {
		return ipLinkDetailRow{}, false
	}
	var rows []ipLinkDetailRow
	if json.Unmarshal([]byte(out), &rows) != nil || len(rows) == 0 {
		return ipLinkDetailRow{}, false
	}
	return rows[0], true
}

// linkMaster 设备当前的 master 名与其归属类型（linkinfo.info_slave_kind）。
// ok=true 且 master=="" 表示确认为「没有 master」；ok=false 表示读不到（不猜）。
func (p *Provider) linkMaster(ctx context.Context, dev string) (master, slaveKind string, ok bool) {
	row, ok := p.linkDetail(ctx, dev)
	if !ok {
		return "", "", false
	}
	if row.LinkInfo != nil {
		slaveKind = row.LinkInfo.SlaveKind
	}
	return row.Master, slaveKind, true
}

// detachMasterIf 仅当设备当前 master 等于 master 时摘除归属；已被别的对象（bridge/bond/vrf）
// 接管时保持不动——「换归属」不得误摘（R2-14/#193/#196/#361 同族）。
// 读不到归属时同样不动：猜测式 nomaster 比留下一个解绑动作更危险。
func (p *Provider) detachMasterIf(ctx context.Context, dev, master string) error {
	cur, _, ok := p.linkMaster(ctx, dev)
	if !ok || cur != master {
		return nil
	}
	return p.ipBest(ctx, "link", "set", "dev", dev, "nomaster")
}

// linkMembers 当前挂在给定 master 下的接口名（读失败返回 nil，调用方按「读不到」保守处理）。
func (p *Provider) linkMembers(ctx context.Context, master string) []string {
	out, err := p.run.Run(ctx, "ip", "-j", "link", "show", "master", master)
	if err != nil {
		return nil
	}
	var rows []ipLinkRow
	if json.Unmarshal([]byte(out), &rows) != nil {
		return nil
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Ifname != "" {
			names = append(names, r.Ifname)
		}
	}
	return names
}

// bridgeVlanRow `bridge -j vlan show` 的一行。
type bridgeVlanRow struct {
	Ifname string `json:"ifname"`
	Vlans  []struct {
		Vlan   int  `json:"vlan"`
		Pvid   bool `json:"pvid"`
		Tagged bool `json:"tagged"`
		// iproute2 在不同版本用 flags 数组或布尔字段表达 untagged/pvid，二者都读。
		Flags []string `json:"flags"`
	} `json:"vlans"`
}

// bridgePortVlans 某成员口当前的 VLAN 条目（vid 列表）。
func (p *Provider) bridgePortVlans(ctx context.Context, port string) []int {
	out, err := p.run.Run(ctx, "bridge", "-j", "vlan", "show", "dev", port)
	if err != nil {
		return nil
	}
	var rows []bridgeVlanRow
	if json.Unmarshal([]byte(out), &rows) != nil {
		return nil
	}
	var vids []int
	for _, r := range rows {
		for _, v := range r.Vlans {
			vids = append(vids, v.Vlan)
		}
	}
	return vids
}
