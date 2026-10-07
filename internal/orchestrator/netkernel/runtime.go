package netkernel

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// Runtime 内核数据面的运行态读视图（与 api.L2Runtime / api.L3Runtime 同形）。
//
// 与 VPP 实现的关键差别：内核是唯一事实源，读视图**直接查内核**（bridge fdb / ip route），
// 不读进程内登记、也不需要恢复重放来重建读视图。
type Runtime struct{ run Runner }

// NewRuntime 构造内核运行态读物。
func NewRuntime(run Runner) *Runtime {
	if run == nil {
		run = NewExecRunner()
	}
	return &Runtime{run: run}
}

// MACTableRow 一条 MAC 表项（与 network 侧同形）。
type MACTableRow struct {
	MAC  string
	Port string
	VLAN int
}

// RouteRow 一条 FIB 路由（与 network 侧同形）。
type RouteRow struct {
	Prefix   string
	NextHop  string
	Distance int
}

// fdbRow `bridge -j fdb show` 的一行。
type fdbRow struct {
	MAC    string   `json:"mac"`
	Dev    string   `json:"dev"`
	VLAN   int      `json:"vlan"`
	Master string   `json:"master"`
	Flags  []string `json:"flags"`
}

// MACTable 返回一台 L2 交换机（内核 bridge）的 MAC 表。
func (r *Runtime) MACTable(ctx context.Context, swName string) ([]MACTableRow, error) {
	br := LinkName(swName)
	out, err := r.run.Run(ctx, "bridge", "-j", "fdb", "show", "br", br)
	if err != nil {
		return nil, err
	}
	var rows []fdbRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	res := make([]MACTableRow, 0, len(rows))
	for _, row := range rows {
		if row.MAC == "" || row.Dev == "" || row.Dev == br {
			continue // 跳过 bridge 自身条目（非学习到的成员口表项）
		}
		if hasFlag(row.Flags, "self") {
			continue
		}
		res = append(res, MACTableRow{MAC: row.MAC, Port: row.Dev, VLAN: row.VLAN})
	}
	return res, nil
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// ipRouteRow `ip -j route show` 的一行。
type ipRouteRow struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Metric  int    `json:"metric"`
	Type    string `json:"type"`
}

// Routes 返回一台 L3 交换机（内核 VRF 表）的静态路由。
func (r *Runtime) Routes(ctx context.Context, vrfName string) ([]RouteRow, error) {
	out, err := r.run.Run(ctx, "ip", "-j", "route", "show", "table", strconv.Itoa(VRFTableID(LinkName(vrfName))))
	if err != nil {
		return nil, err
	}
	var rows []ipRouteRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	res := make([]RouteRow, 0, len(rows))
	for _, row := range rows {
		dst := row.Dst
		if dst == "" {
			dst = "default"
		}
		res = append(res, RouteRow{Prefix: dst, NextHop: row.Gateway, Distance: row.Metric})
	}
	return res, nil
}

// ipLinkKindRow `ip -j link show` 的一行（含 linkinfo 以判设备类型）。
type ipLinkKindRow struct {
	Ifname   string `json:"ifname"`
	LinkInfo *struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
}

// DataplaneIfnames 内核数据面自持的虚拟设备名（bridge / VRF / vxlan / bond / vlan 子接口）。
//
// 用于端口读视图与 `<ifname>` 候选：内核数据面下「已交数据面的口」就是这些产品建的虚拟设备，
// 物理口则仍在 `KernelIfnames` 里（内核数据面不接管物理口）。
func (r *Runtime) DataplaneIfnames(ctx context.Context, cfg model.Config) ([]string, error) {
	out, err := r.run.Run(ctx, "ip", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var rows []ipLinkKindRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	managed := map[string]bool{}
	add := func(n string) { managed[LinkName(n)] = true }
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" {
			add(vs.Name)
			continue
		}
		add(vs.Name)
		if vs.Gateway != nil {
			if vs.Gateway.Vrf != "" {
				add(vs.Gateway.Vrf)
			} else {
				add(GatewayVRFName(vs.Name))
			}
		}
	}
	for _, vrf := range cfg.Vrfs {
		add(vrf.Name)
		for _, li := range vrf.L3Interfaces {
			if li.Vlan > 0 {
				managed[VlanSubifName(LinkName(li.Interface), li.Vlan)] = true
			}
		}
	}
	for _, b := range cfg.Bonds {
		add(b.Name)
	}
	for _, vx := range cfg.VxlanTunnels {
		add(vx.Name)
	}
	var names []string
	for _, row := range rows {
		if row.Ifname != "" && managed[row.Ifname] {
			names = append(names, row.Ifname)
		}
	}
	return names, nil
}
