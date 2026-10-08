package netkernel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
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
	// ID 恒为 0：内核 bridge 没有 BD-ID 这个概念（VPP 专有），内核数据面**不使用**该字段
	// （旧实现填枚举序号，读视图会把它当成一个真实存在的数据面标识）。
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
//
// 必须带 **-d**：`ip -j link show`（不带 -d）不输出 `linkinfo`，DevType 会恒得「physical」
// （R2-24：真机实测 bridge/vrf/vlan/bond/veth 全都没有该键）。
func (r *Runtime) InterfaceStates(ctx context.Context) (map[string]InterfaceState, error) {
	out, err := r.run.Run(ctx, "ip", "-d", "-j", "link", "show")
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

// BridgeDomains 全部**配置声明**的 L2 交换机的运行态（含成员口）。
//
// 枚举以声明为准（R2-15①）：`bridge -j link show` 以端口为行，零成员的交换机根本不进列表
// ——旧实现据此对真实存在的交换机回「在数据面中不存在」。逐台按内核实况判定：设备的
// `linkinfo.info_kind=bridge` 存在才进列表（**配置声明了、内核没有**的如实不出现，
// 不编造空壳）；成员口取 `bridge -j link show` 的实况。
//
// 只报声明内的交换机（不报 virbr0/docker0 这类宿主自建 bridge）：内核没有「产品自持」标记，
// 与 VPPIfnames 同一判据（**配置声明集合**是唯一事实源）。
func (r *Runtime) BridgeDomains(ctx context.Context, cfg model.Config) ([]BridgeDomainState, error) {
	rows, err := r.bridgeLinkRows(ctx)
	if err != nil {
		return nil, err
	}
	members := map[string][]BridgeDomainPort{}
	for _, row := range rows {
		if row.Master == "" || row.Ifname == "" {
			continue
		}
		members[row.Master] = append(members[row.Master], BridgeDomainPort{Name: row.Ifname})
	}
	bridges, err := r.kernelBridges(ctx)
	if err != nil {
		return nil, err
	}
	res := make([]BridgeDomainState, 0, len(cfg.VirtualSwitches))
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" {
			continue // L3 交换机在内核侧是 VRF 设备，不是 bridge
		}
		br := LinkName(vs.Name)
		if !bridges[br] {
			continue
		}
		res = append(res, BridgeDomainState{Name: br, Ports: members[br]})
	}
	return res, nil
}

// bridgeLinkRows `bridge -j link show` 的全部行（失败如实返回错误）。
func (r *Runtime) bridgeLinkRows(ctx context.Context) ([]bridgeLinkRow, error) {
	out, err := r.run.Run(ctx, "bridge", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var rows []bridgeLinkRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// kernelBridges 内核里现存的 bridge 设备名集合（`ip -d -j link show` 的 info_kind=bridge）。
func (r *Runtime) kernelBridges(ctx context.Context) (map[string]bool, error) {
	out, err := r.run.Run(ctx, "ip", "-d", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var rows []ipLinkStateRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	bridges := map[string]bool{}
	for _, row := range rows {
		if row.Ifname != "" && row.LinkInfo != nil && row.LinkInfo.InfoKind == "bridge" {
			bridges[row.Ifname] = true
		}
	}
	return bridges, nil
}
