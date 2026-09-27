package network

// govpp NAT44 客户端（M3-5 三）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/ip_types"
	"go.fd.io/govpp/binapi/nat44_ei"
)

// NatClientFunc 返回随当前连接获取 NAT44 客户端的工厂。
func (m *Manager) NatClientFunc() func() (NatClient, error) {
	return func() (NatClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppNatClient{ch: ch}, nil
	}
}

type govppNatClient struct{ ch api.Channel }

func (g *govppNatClient) Close() { g.ch.Close() }

func (g *govppNatClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

// NATAddressRange 增删一个地址池地址段。vrfID 是地址池所属转发域，必须传**inside（租户）
// 转发域**——即规则 virtual-switch 的那张表：VPP 用该 vrf_id 折算池地址的 FIB 索引
// （nat44_ei_add_address），而 in2out 慢路径只从「与入接口同一张表」的池地址里分配端口
// （nat44_ei_alloc_default_cb），或 fib_index == ~0 的 VRF independent 地址（接口地址形态）。
// 传 outside 表或 0 时池与入接口的表对不上：包进了 NAT 却分配不出端口（round84 R84-24
// 实测：`show errors` 见 nat44-ei-in2out-slowpath out of ports、会话恒为 0），全程无报错。
// 删除方向同样带登记时所用的 VRF，保证 add/del 成对（换域先删旧再 add 新）。
func (g *govppNatClient) NATAddressRange(add bool, first, last string, vrfID uint32) error {
	f, err := ip_types.ParseIP4Address(first)
	if err != nil {
		return fmt.Errorf("解析起始地址 %q: %w", first, err)
	}
	l, err := ip_types.ParseIP4Address(last)
	if err != nil {
		return fmt.Errorf("解析结束地址 %q: %w", last, err)
	}
	reply := &nat44_ei.Nat44EiAddDelAddressRangeReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiAddDelAddressRange{
		FirstIPAddress: f, LastIPAddress: l, VrfID: vrfID, IsAdd: add,
	}).ReceiveReply(reply); err != nil {
		// add 方向「已存在」即已达目标状态：重放（恢复收敛、连接重建后的全量下发）必然重复
		// 下发同一地址池。此前只容忍 del 方向，于是重放收到 -81 后**整个 ApplyNAT 中止**，
		// 表现为一条 WARN + 未收敛项，而配置与 show nat 看似完全正常（round84 R84-20）。
		if add && vppErrIs(err, vppValueExist) {
			return nil
		}
		// 移除方向：对象本就不在（-6）/ 形式已存在（-81）/ 插件已关（-169）都属「已是目标状态」，
		// 按成功处理，避免一次无害的重复删除把整批 apply 打回滚（round84 缺陷 B）。
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppValueExist {
			return nil
		}
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_add_del_address_range(%s-%s,vrf=%d,add=%v) retval=%d", first, last, vrfID, add, reply.Retval)
	}
	return nil
}

// NATAddressVRFs 读回 VPP 侧各 NAT 地址所在的转发域（地址 → tenant VRF；~0 = 与 VRF 无关，
// 即 nat44_ei_add_del_interface_addr 那种接口地址形态）。地址池的 vrf_id 就是租户（inside）
// 转发域的表 id，用它核对「同一地址是否已被按别的转发域下发过」——池地址按地址唯一，
// add 命中「已存在」被幂等容忍，不读回就发现不了旧副本（NAT 会一直分配不出端口）。
func (g *govppNatClient) NATAddressVRFs() (map[string]uint32, error) {
	reqCtx := g.ch.SendMultiRequest(&nat44_ei.Nat44EiAddressDump{})
	out := map[string]uint32{}
	for {
		d := &nat44_ei.Nat44EiAddressDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		out[d.IPAddress.String()] = d.VrfID
	}
	return out, nil
}

func (g *govppNatClient) NATFeature(swIfIndex uint32, inside, add bool) error {
	flags := nat44_ei.NAT44_EI_IF_OUTSIDE
	if inside {
		flags = nat44_ei.NAT44_EI_IF_INSIDE
	}
	reply := &nat44_ei.Nat44EiInterfaceAddDelFeatureReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiInterfaceAddDelFeature{
		IsAdd: add, Flags: flags, SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply); err != nil {
		// 移除方向容忍「已是目标状态」：接口被重建/特性已被别处摘除时 VPP 报 -6，
		// 插件已关时报 -169；二者都不改变数据面状态（round84 实测 -6 曾把整批 apply 打回滚）。
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_interface_add_del_feature(if=%d,inside=%v,add=%v) retval=%d", swIfIndex, inside, add, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATEnable(enable bool, insideVRF, outsideVRF uint32) error {
	reply := &nat44_ei.Nat44EiPluginEnableDisableReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiPluginEnableDisable{
		Enable: enable, InsideVrf: insideVRF, OutsideVrf: outsideVRF,
	}).ReceiveReply(reply); err != nil {
		if vppErrIs(err, vppFeatureAlreadyEnabled, vppFeatureAlreadyDisabled) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		// 已是目标状态同样按成功处理（非零 retval 也可能落在 Reply 上，两条路径都要判）。
		if reply.Retval == vppFeatureAlreadyEnabled || reply.Retval == vppFeatureAlreadyDisabled {
			return nil
		}
		return fmt.Errorf("nat44_ei_plugin_enable_disable(enable=%v,inside-vrf=%d,outside-vrf=%d) retval=%d",
			enable, insideVRF, outsideVRF, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATInterfaceAddr(add bool, swIfIndex uint32) error {
	reply := &nat44_ei.Nat44EiAddDelInterfaceAddrReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiAddDelInterfaceAddr{
		IsAdd: add, SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply); err != nil {
		// VPP 对已在自动地址列表里的接口返回 -81（Value already exists）：目标状态已达成，
		// 按成功处理（同 NATAddressRange；只容忍 del 会让重放中止，R84-20）。
		if add && vppErrIs(err, vppValueExist) {
			return nil
		}
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppValueExist {
			return nil
		}
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_add_del_interface_addr(if=%d,add=%v) retval=%d", swIfIndex, add, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATStatic(add bool, inside, outside string) error {
	in, err := ip_types.ParseIP4Address(inside)
	if err != nil {
		return fmt.Errorf("解析内网地址 %q: %w", inside, err)
	}
	out, err := ip_types.ParseIP4Address(outside)
	if err != nil {
		return fmt.Errorf("解析外网地址 %q: %w", outside, err)
	}
	reply := &nat44_ei.Nat44EiAddDelStaticMappingReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiAddDelStaticMapping{
		IsAdd:          add,
		Flags:          nat44_ei.NAT44_EI_STATIC_MAPPING,
		LocalIPAddress: in, ExternalIPAddress: out,
		Protocol:     ^uint8(0), // ~0 = 任意协议
		ExternalPort: ^uint16(0),
		LocalPort:    ^uint16(0),
	}).ReceiveReply(reply); err != nil {
		// 同 NATAddressRange：add 方向「已存在」= 该静态映射已在位，按成功处理（重放安全）；
		// 移除方向的本就不在/插件已关同样按已达成处理。
		if add && vppErrIs(err, vppValueExist) {
			return nil
		}
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppValueExist {
			return nil
		}
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_add_del_static_mapping(%s→%s,add=%v) retval=%d", inside, outside, add, reply.Retval)
	}
	return nil
}

// natUserVPP 一个 NAT44 用户：内网地址 + 其所属租户转发域。
type natUserVPP struct {
	ip    ip_types.IP4Address
	vrfID uint32
}

// NATSessions 汇总 NAT44 EI 会话（运行态）。
//
// VPP 26.06 的 nat44_ei_user_session_dump 是**按用户** dump，不是全表 dump：请求要带具体
// 内网用户地址 + 该用户的租户 VRF，VPP 用它定位用户后再遍历其会话。此前发的是
// 0.0.0.0 / vrf 0——VPP 只当「查地址 0.0.0.0 那个用户」，找不到即结束应答，于是**恒 0 条**：
// NAT 明明在转发（`vppctl show nat44 ei sessions detail` 有十几条转换），`show nat` 却报
// 「无 NAT 会话」（round84 R84-27 真机实测）。故：先 nat44_ei_user_dump 列用户，再逐用户按其
// 地址 + **user_dump 返回的**租户 VRF 查会话并汇总——用户表是全局的，漏掉任何一个用户都会少报会话。
//
// 用户表为空（还没有任何用户）如实返回空——那是「无会话」，不是错误；读取失败原样上抛，
// **不做静默降级**：退回 0.0.0.0/vrf 0 只会重新变成「恒报无会话」这种答非所问。
func (g *govppNatClient) NATSessions() ([]NATSession, error) {
	users, err := g.natUsers()
	if err != nil {
		return nil, err
	}
	var out []NATSession
	for _, u := range users {
		rows, err := g.natUserSessions(u)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// natUsers 列出 NAT44 用户（nat44_ei_user_dump）：每条带内网地址与租户 VRF。
func (g *govppNatClient) natUsers() ([]natUserVPP, error) {
	reqCtx := g.ch.SendMultiRequest(&nat44_ei.Nat44EiUserDump{})
	var users []natUserVPP
	for {
		d := &nat44_ei.Nat44EiUserDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, fmt.Errorf("列出 NAT44 用户: %w", err)
		}
		if stop {
			break
		}
		users = append(users, natUserVPP{ip: d.IPAddress, vrfID: d.VrfID})
	}
	return users, nil
}

// natUserSessions 读一个用户的会话（nat44_ei_user_session_dump）。
//
// VRF 必须取自 user_dump 的返回（写死 0 或默认表都会查不到用户而静默返回空），
// 该值就是会话所在租户转发域：VPP 侧该用户会话的 i2o 侧 fib 即由此 VRF 折算而来。
func (g *govppNatClient) natUserSessions(u natUserVPP) ([]NATSession, error) {
	reqCtx := g.ch.SendMultiRequest(&nat44_ei.Nat44EiUserSessionDump{IPAddress: u.ip, VrfID: u.vrfID})
	var out []NATSession
	for {
		d := &nat44_ei.Nat44EiUserSessionDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, fmt.Errorf("读取 NAT44 用户 %s（转发域 %d）的会话: %w", u.ip, u.vrfID, err)
		}
		if stop {
			break
		}
		out = append(out, NATSession{
			InsideIP:    d.InsideIPAddress.String(),
			InsidePort:  int(d.InsidePort),
			OutsideIP:   d.OutsideIPAddress.String(),
			OutsidePort: int(d.OutsidePort),
			Protocol:    int(d.Protocol),
			Bytes:       d.TotalBytes,
			Packets:     d.TotalPkts,
		})
	}
	return out, nil
}
