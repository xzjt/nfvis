package netkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// ensureVRF 确保内核 VRF 设备存在且已 up（路由表号由名字确定性派生）。
func (p *Provider) ensureVRF(ctx context.Context, vrfName string) error {
	if err := p.ipIdem(ctx, "link", "add", "name", vrfName, "type", "vrf", "table", fmt.Sprint(VRFTableID(vrfName))); err != nil {
		return err
	}
	return p.ipReq(ctx, "link", "set", "dev", vrfName, "up")
}

// ApplyVRF 收敛一台 L3 虚拟交换机为内核 VRF：VRF 设备 + 各 l3-interface 的归属与地址 +
// 声明里的静态路由（与 VPP 侧同口径：只加不撤，撤销由 DeleteRoute/DeleteL3Interface 负责）。
func (p *Provider) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	if vrf.Name == "" {
		return fmt.Errorf("L3 交换机名不能为空")
	}
	vrfName := LinkName(vrf.Name)
	if err := p.ensureVRF(ctx, vrfName); err != nil {
		return err
	}
	if vrf.Description != "" {
		if err := p.ipReq(ctx, "link", "set", "dev", vrfName, "alias", vrf.Description); err != nil {
			return err
		}
	}
	for _, li := range vrf.L3Interfaces {
		if err := p.applyL3Interface(ctx, vrfName, li); err != nil {
			return err
		}
		// 三层接口的入向 ACL 绑定（声明写 `acl-in <名>`；策略对象由 ApplyACL 先行下发）。
		if err := p.bindL3IfaceACL(ctx, li); err != nil {
			return err
		}
	}
	for _, rt := range vrf.Routes {
		if err := p.ApplyRoute(ctx, vrf.Name, rt); err != nil {
			return err
		}
	}
	return nil
}

// applyL3Interface 把一条 l3-interface 挂进 VRF 并下发地址（vlan 子接口按需创建）。
func (p *Provider) applyL3Interface(ctx context.Context, vrfName string, li model.L3Interface) error {
	if li.Interface == "" {
		return fmt.Errorf("l3-interface 的接口名不能为空")
	}
	dev := l3DeviceName(li)
	base := LinkName(li.Interface)
	// 基口必须先 up：vlan 子接口的 carrier 跟随基口，基口 down 时 `ip link set <子接口> up`
	// 会直接报 `RTNETLINK answers: Network is down`（真机实测）。三层接口本就要求可用，
	// 故这里显式置 up（幂等；这条口径写进设计文档与用户手册）。
	if err := p.ipReq(ctx, "link", "set", "dev", base, "up"); err != nil {
		return err
	}
	if li.Vlan > 0 {
		if err := p.ipIdem(ctx, "link", "add", "link", base, "name", dev, "type", "vlan", "id", fmt.Sprint(li.Vlan)); err != nil {
			return err
		}
	}
	if err := p.ipReq(ctx, "link", "set", "dev", dev, "master", vrfName); err != nil {
		return err
	}
	for _, cidr := range li.Addresses {
		if err := p.ipIdem(ctx, "addr", "replace", cidr, "dev", dev); err != nil {
			return err
		}
	}
	return p.ipReq(ctx, "link", "set", "dev", dev, "up")
}

// l3DeviceName l3-interface 对应的内核设备名。
func l3DeviceName(li model.L3Interface) string {
	if li.Vlan > 0 {
		return VlanSubifName(LinkName(li.Interface), li.Vlan)
	}
	return LinkName(li.Interface)
}

// ApplyRoute 下发一条静态路由到该 VRF 的表（replace 语义，幂等）。
func (p *Provider) ApplyRoute(ctx context.Context, vrfName string, route model.Route) error {
	args := routeArgs("replace", vrfName, route)
	return p.ipIdem(ctx, args...)
}

// DeleteRoute 撤销一条静态路由。
func (p *Provider) DeleteRoute(ctx context.Context, vrfName string, route model.Route) error {
	args := routeArgs("del", vrfName, route)
	return p.ipBest(ctx, args...)
}

func routeArgs(verb, vrfName string, route model.Route) []string {
	args := []string{"route", verb, "vrf", LinkName(vrfName), route.Prefix}
	if route.NextHop != "" {
		args = append(args, "via", route.NextHop)
	}
	if route.Distance > 0 {
		args = append(args, "metric", fmt.Sprint(route.Distance))
	}
	return args
}

// DeleteL3Interface 回收一条已从声明里删除的 L3 接口：清地址 → 出 VRF → （vlan 子接口）删设备。
func (p *Provider) DeleteL3Interface(ctx context.Context, vrfName string, iface model.L3Interface) error {
	dev := l3DeviceName(iface)
	// 先解引用：接口要撤，它上面的 ACL 绑定必须一起摘（否则 nft 里留着指向该口的 jump）。
	if iface.AclIn != "" {
		if err := p.aclMgr().Unbind(ctx, dev); err != nil {
			return err
		}
	}
	for _, cidr := range iface.Addresses {
		if err := p.ipBest(ctx, "addr", "del", cidr, "dev", dev); err != nil {
			return err
		}
	}
	if err := p.ipBest(ctx, "link", "set", "dev", dev, "nomaster"); err != nil {
		return err
	}
	if iface.Vlan > 0 {
		return p.ipBest(ctx, "link", "del", dev)
	}
	return nil
}

// DeleteVRF 删除一台 L3 交换机：先回收其成员口（清地址、出 VRF、删 vlan 子接口），再删 VRF 设备。
func (p *Provider) DeleteVRF(ctx context.Context, name string) error {
	vrfName := LinkName(name)
	for _, m := range p.vrfMembers(ctx, vrfName) {
		for _, cidr := range p.deviceAddresses(ctx, m) {
			if err := p.ipBest(ctx, "addr", "del", cidr, "dev", m); err != nil {
				return err
			}
		}
		if err := p.ipBest(ctx, "link", "set", "dev", m, "nomaster"); err != nil {
			return err
		}
		if strings.Contains(m, ".") {
			if err := p.ipBest(ctx, "link", "del", m); err != nil {
				return err
			}
		}
	}
	return p.ipBest(ctx, "link", "del", vrfName)
}

// ipLinkRow `ip -j link show` 的一行（只取接口名）。
type ipLinkRow struct {
	Ifname string `json:"ifname"`
}

// vrfMembers 当前挂在给定 VRF 下的接口名（读失败返回 nil）。
func (p *Provider) vrfMembers(ctx context.Context, vrfName string) []string {
	out, err := p.run.Run(ctx, "ip", "-j", "link", "show", "master", vrfName)
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

// ipAddrRow `ip -j addr show` 的一行。
type ipAddrRow struct {
	AddrInfo []struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// deviceAddresses 某设备当前的地址（CIDR 形态；读失败返回 nil）。
func (p *Provider) deviceAddresses(ctx context.Context, dev string) []string {
	out, err := p.run.Run(ctx, "ip", "-j", "addr", "show", "dev", dev)
	if err != nil {
		return nil
	}
	var rows []ipAddrRow
	if json.Unmarshal([]byte(out), &rows) != nil {
		return nil
	}
	var cidrs []string
	for _, r := range rows {
		for _, a := range r.AddrInfo {
			if a.Local != "" {
				cidrs = append(cidrs, fmt.Sprintf("%s/%d", a.Local, a.PrefixLen))
			}
		}
	}
	return cidrs
}
