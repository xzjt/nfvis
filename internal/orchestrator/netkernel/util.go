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
