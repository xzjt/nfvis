package netkernel

import (
	"context"
	"encoding/json"
	"fmt"
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
				// 派生名超长（>15）的声明下不到数据面，读视图也没有它可报（不编造名字）。
				if dev, err := VlanSubifName(LinkName(li.Interface), li.Vlan); err == nil {
					managed[dev] = true
				}
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

// IfaceInUse 判定某网口此刻是否正被内核数据面使用（决策 #426②的解绑前守卫）。
//
// 与 VPP 侧「问 VPP 这个口还在你手里吗」对应，这里问内核——判据是两条**可独立核对**的
// 数据面事实（任一命中即「在用」，并返回可读原因）：
//
//   - 是某个 master 的成员口（`ip -j link show` 的 master 字段：bridge / bond / VRF 都算）；
//   - 自身带 IP 地址（产品的 l3-interface 地址就下发在口上；Vlan>0 时落在子接口）。
//
// 口在内核里不存在（例如仍被 DPDK 驱动接管）时，`ip link show dev <n>` 报错返回——
// 由调用方按「探测不到不拦」处理（与 VPP 侧同取向：探测通道不通不代表口在被使用，
// 而 DPDK 残留恰恰是解绑要解决的情形）。
func (r *Runtime) IfaceInUse(ctx context.Context, ifname string) (bool, string, error) {
	name := strings.TrimSpace(ifname)
	if name == "" {
		return false, "", fmt.Errorf("接口名不能为空")
	}
	out, err := r.run.Run(ctx, "ip", "-j", "link", "show", "dev", name)
	if err != nil {
		return false, "", err
	}
	var links []struct {
		Ifname string `json:"ifname"`
		Master string `json:"master"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return false, "", fmt.Errorf("解析 %s 的链路信息失败: %w", name, err)
	}
	if len(links) == 0 {
		return false, "", fmt.Errorf("内核中查不到 %s（ip link show 无该设备）", name)
	}
	if m := strings.TrimSpace(links[0].Master); m != "" {
		return true, "是 " + m + " 的成员口", nil
	}
	aout, err := r.run.Run(ctx, "ip", "-j", "addr", "show", "dev", name)
	if err != nil {
		return false, "", err
	}
	var addrs []struct {
		AddrInfo []struct {
			Family string `json:"family"`
			Local  string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(aout), &addrs); err != nil {
		return false, "", fmt.Errorf("解析 %s 的地址信息失败: %w", name, err)
	}
	n := 0
	for _, a := range addrs {
		for _, ai := range a.AddrInfo {
			if ai.Local != "" {
				n++
			}
		}
	}
	if n > 0 {
		return true, fmt.Sprintf("带 IP 地址（%d 个）", n), nil
	}
	return false, "", nil
}
