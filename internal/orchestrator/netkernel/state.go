package netkernel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// InterfaceState 接口运行态（与 api.InterfaceState 同形）。
type InterfaceState struct {
	AdminUp   bool
	LinkUp    bool
	LinkSpeed uint32 // kbps
	DevType   string // 设备类型/驱动名
	MTU       uint32
}

// BridgeDomainState bridge-domain 运行态（与 api.BridgeDomainState 同形）。
type BridgeDomainState struct {
	ID    uint32
	Name  string
	Ports []BridgeDomainPort
}

// BridgeDomainPort BD 成员口。
type BridgeDomainPort struct {
	SwIfIndex uint32
	Name      string
	Shg       uint8
}

// ipLinkStateRow `ip -j link show` 的一行（含状态与 MTU）。
type ipLinkStateRow struct {
	Ifname    string   `json:"ifname"`
	Flags     []string `json:"flags"`
	OperState string   `json:"operstate"`
	MTU       int      `json:"mtu"`
	LinkInfo  *struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
}

// InterfaceStates 全部内核接口的运行态（接口名 → 状态）。
func (r *Runtime) InterfaceStates(ctx context.Context) (map[string]InterfaceState, error) {
	out, err := r.run.Run(ctx, "ip", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var rows []ipLinkStateRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	res := make(map[string]InterfaceState, len(rows))
	for _, row := range rows {
		if row.Ifname == "" {
			continue
		}
		st := InterfaceState{
			AdminUp: hasFlag(row.Flags, "UP"),
			LinkUp:  strings.EqualFold(row.OperState, "up") || strings.EqualFold(row.OperState, "unknown"),
			DevType: linkKind(row),
			MTU:     uint32(row.MTU),
		}
		st.LinkSpeed = readLinkSpeedKbps(row.Ifname)
		res[row.Ifname] = st
	}
	return res, nil
}

func linkKind(row ipLinkStateRow) string {
	if row.LinkInfo != nil && row.LinkInfo.InfoKind != "" {
		return row.LinkInfo.InfoKind
	}
	return "physical"
}

// sysfsNetRoot 内核网卡目录（与 internal/orchestrator/network 的同一落点；测试可改）。
var sysfsNetRoot = "/sys/class/net"

// readLinkSpeedKbps 读 sysfs 的链路速率（Mbps → kbps）；读不到返回 0（如实为「取不到」）。
func readLinkSpeedKbps(ifname string) uint32 {
	b, err := os.ReadFile(filepath.Join(sysfsNetRoot, ifname, "speed"))
	if err != nil {
		return 0
	}
	mbps, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || mbps <= 0 {
		return 0
	}
	return uint32(mbps) * 1000
}

// BridgeDomains 全部内核 bridge 的运行态（含成员口）。
func (r *Runtime) BridgeDomains(ctx context.Context) ([]BridgeDomainState, error) {
	out, err := r.run.Run(ctx, "bridge", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var rows []bridgeLinkRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	order := []string{}
	byName := map[string]*BridgeDomainState{}
	for _, row := range rows {
		if row.Master == "" {
			continue
		}
		bd, ok := byName[row.Master]
		if !ok {
			bd = &BridgeDomainState{Name: row.Master}
			byName[row.Master] = bd
			order = append(order, row.Master)
		}
		bd.Ports = append(bd.Ports, BridgeDomainPort{Name: row.Ifname})
	}
	res := make([]BridgeDomainState, 0, len(order))
	for i, name := range order {
		bd := byName[name]
		bd.ID = uint32(i + 1)
		res = append(res, *bd)
	}
	return res, nil
}
