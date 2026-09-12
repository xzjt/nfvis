package model

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// ValidateError 校验错误，Path 采用 OpenAPI Error.detail 的寻址风格
// （如 virtual-machine-functions[fw-vm].memory.size-mb）。
type ValidateError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e ValidateError) Error() string { return e.Path + ": " + e.Message }

// Validate 对配置做结构校验与自包含的语义校验（FR-CFG-002 要求失败时逐条列出全部错误）。
//
// 覆盖：名称语法/重复、枚举与格式（ip-prefix/ip/mac/vlan/端口段）、必填项、
// 配置内引用存在性，以及 FR-CFG-011 中可由配置文档自身判定的规则
// （①vhost-user 大页、②地址重叠、③MAC 重复、⑧dpdk dev 为物理口）与
// FR-SYS-010 的 vpp 核/大页一致性。
// 依赖外部状态的规则（⑤镜像类型匹配、⑨⑪资源配额余量、⑩NUMA 警告等）由
// 事务引擎 commit 阶段结合资源账本与镜像仓库执行。
func Validate(c Config) []ValidateError {
	v := &validator{}
	v.collect(c)
	v.checkSystem(c)
	v.checkInterfaces(c)
	v.checkBonds(c)
	v.checkVirtualSwitches(c)
	v.checkVrfs(c)
	v.checkAcls(c)
	v.checkNat(c)
	v.checkPortMirroring(c)
	v.checkQos(c)
	v.checkResourcePools(c)
	v.checkVpp(c)
	v.checkProtocols(c)
	v.checkVMFunctions(c)
	v.checkContainerFunctions(c)
	v.checkAddressOverlap(c)
	return v.errs
}

type validator struct {
	errs []ValidateError

	ifaceNames map[string]bool
	bondNames  map[string]bool
	vsNames    map[string]bool
	l2vs       map[string]bool
	l3vs       map[string]bool
	vrfNames   map[string]bool
	aclNames   map[string]bool
	qosNames   map[string]bool
	vmNames    map[string]bool
	vmVnics    map[string]bool // "vm/vnic"
	ctNames    map[string]bool
	ctVnics    map[string]bool // "ct/vnic"
	hpSizes    map[string]bool
}

func (v *validator) errf(path, format string, args ...any) {
	v.errs = append(v.errs, ValidateError{Path: path, Message: fmt.Sprintf(format, args...)})
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var portSpecRe = regexp.MustCompile(`^\d+(-\d+)?$`)

func (v *validator) checkName(path, name, what string) bool {
	if name == "" {
		v.errf(path, "%s名称缺失", what)
		return false
	}
	if !nameRe.MatchString(name) {
		v.errf(path, "%s名称 %q 不合法（字母数字-_.，≤64 字符）", what, name)
		return false
	}
	return true
}

func checkCIDR(s string) bool { _, _, err := net.ParseCIDR(s); return err == nil }
func checkIP(s string) bool   { return net.ParseIP(s) != nil }
func checkMAC(s string) bool  { _, err := net.ParseMAC(s); return err == nil }
func checkVlan(n int) bool    { return n >= 1 && n <= 4094 }

func (v *validator) anyIface(n string) bool { return v.ifaceNames[n] || v.bondNames[n] }

// l3IfaceExists 判断 L3 接口引用：物理口/bond，或物理口上的 VLAN 子接口（如 ens2f0.100）。
func (v *validator) l3IfaceExists(n string) bool {
	if v.anyIface(n) {
		return true
	}
	if parent, _, ok := strings.Cut(n, "."); ok {
		return v.anyIface(parent)
	}
	return false
}

func (v *validator) checkACLRef(path, acl string) {
	if acl != "" && !v.aclNames[acl] {
		v.errf(path, "ACL %q 不存在", acl)
	}
}

func (v *validator) checkVSwitchRef(path, name string) {
	if name != "" && !v.vsNames[name] {
		v.errf(path, "虚拟交换机 %q 不存在", name)
	}
}

// parseCoreList 解析 "5,7,9-11" 形式的核列表，非法 token 返回 error。
func parseCoreList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, err1 := strconv.Atoi(a)
			hi, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil || lo > hi {
				return nil, fmt.Errorf("核区间 %q 不合法", part)
			}
			for x := lo; x <= hi; x++ {
				out = append(out, x)
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("核编号 %q 不合法", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func (v *validator) collect(c Config) {
	v.ifaceNames, v.bondNames = map[string]bool{}, map[string]bool{}
	for _, i := range c.Interfaces {
		v.ifaceNames[i.Name] = true
	}
	for _, b := range c.Bonds {
		v.bondNames[b.Name] = true
	}
	v.vsNames, v.l2vs, v.l3vs = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range c.VirtualSwitches {
		v.vsNames[s.Name] = true
		if s.Type == "l2" {
			v.l2vs[s.Name] = true
		} else if s.Type == "l3" {
			v.l3vs[s.Name] = true
		}
	}
	v.vrfNames = map[string]bool{}
	for _, r := range c.Vrfs {
		v.vrfNames[r.Name] = true
	}
	v.aclNames = map[string]bool{}
	for _, a := range c.Acls {
		v.aclNames[a.Name] = true
	}
	v.qosNames = map[string]bool{}
	for _, q := range c.QosPolicies {
		v.qosNames[q.Name] = true
	}
	v.vmNames, v.vmVnics = map[string]bool{}, map[string]bool{}
	for _, m := range c.VirtualMachineFunctions {
		v.vmNames[m.Name] = true
		for _, nic := range m.Interfaces {
			v.vmVnics[m.Name+"/"+nic.Name] = true
		}
	}
	v.ctNames, v.ctVnics = map[string]bool{}, map[string]bool{}
	for _, ct := range c.ContainerFunctions {
		v.ctNames[ct.Name] = true
		for _, nic := range ct.Interfaces {
			v.ctVnics[ct.Name+"/"+nic.Name] = true
		}
	}
	v.hpSizes = map[string]bool{}
	if c.ResourcePools != nil {
		for _, hp := range c.ResourcePools.Hugepages {
			v.hpSizes[hp.PageSize] = true
		}
	}
}

func dupCheck[T any](v *validator, items []T, basePath string, name func(T) string, what string) {
	seen := map[string]bool{}
	for _, it := range items {
		n := name(it)
		if seen[n] {
			v.errf(fmt.Sprintf("%s[%s]", basePath, n), "%s %q 重复定义", what, n)
		}
		seen[n] = true
	}
}

func (v *validator) checkSystem(c Config) {
	if c.System == nil {
		return
	}
	s := c.System
	if s.Hostname != "" {
		v.checkName("system.hostname", s.Hostname, "主机")
	}
	for i, n := range s.Ntp {
		if n.Server == "" {
			v.errf(fmt.Sprintf("system.ntp[%d].server", i), "NTP 服务器地址缺失")
		}
	}
	if s.Management != nil {
		if s.Management.Address != "" && !checkCIDR(s.Management.Address) {
			v.errf("system.management.address", "管理口地址 %q 必须是 ip-prefix（CIDR）", s.Management.Address)
		}
		if s.Management.Gateway != "" && !checkIP(s.Management.Gateway) {
			v.errf("system.management.gateway", "管理口网关 %q 必须是有效 ip", s.Management.Gateway)
		}
	}
	if s.Syslog != nil && s.Syslog.Level != "" {
		switch s.Syslog.Level {
		case "debug", "info", "warn", "error":
		default:
			v.errf("system.syslog.level", "syslog level 必须为 debug|info|warn|error")
		}
	}
}

func (v *validator) checkInterfaces(c Config) {
	dupCheck(v, c.Interfaces, "interfaces", func(i InterfaceConfig) string { return i.Name }, "接口")
	for _, i := range c.Interfaces {
		p := fmt.Sprintf("interfaces[%s]", i.Name)
		if !v.checkName(p, i.Name, "接口") {
			continue
		}
		if i.MTU != 0 && (i.MTU < 68 || i.MTU > 9216) {
			v.errf(p+".mtu", "mtu %d 超出合理范围 68-9216", i.MTU)
		}
		if i.IngressPolicy != "" && !v.qosNames[i.IngressPolicy] {
			v.errf(p+".ingress_policy", "限速策略 %q 不存在", i.IngressPolicy)
		}
	}
}

func (v *validator) checkBonds(c Config) {
	dupCheck(v, c.Bonds, "bonds", func(b Bond) string { return b.Name }, "bond")
	for _, b := range c.Bonds {
		p := fmt.Sprintf("bonds[%s]", b.Name)
		if !v.checkName(p, b.Name, "bond") {
			continue
		}
		if len(b.Members) == 0 {
			v.errf(p+".members", "bond 至少需要一个成员口")
		}
		for _, m := range b.Members {
			if !v.ifaceNames[m] {
				v.errf(p+".members", "成员口 %q 不是物理口", m)
			}
		}
		if b.Lacp != nil {
			switch b.Lacp.Mode {
			case "active", "passive":
			default:
				v.errf(p+".lacp.mode", "lacp mode 必须为 active 或 passive")
			}
			if b.Lacp.Interval != "" && b.Lacp.Interval != "fast" && b.Lacp.Interval != "slow" {
				v.errf(p+".lacp.interval", "lacp interval 必须为 fast 或 slow")
			}
		}
	}
}

func (v *validator) checkVirtualSwitches(c Config) {
	dupCheck(v, c.VirtualSwitches, "virtual-switches", func(s VirtualSwitch) string { return s.Name }, "虚拟交换机")
	for _, s := range c.VirtualSwitches {
		p := fmt.Sprintf("virtual-switches[%s]", s.Name)
		if !v.checkName(p, s.Name, "虚拟交换机") {
			continue
		}
		switch s.Type {
		case "l2":
		case "l3":
			// 附录 B：L3 交换机的 l3-interface/静态路由映射为同名 VRF 条目
			if !v.vrfNames[s.Name] {
				v.errf(p, "type=l3 交换机需要同名 VRF 条目承载 L3 配置（附录 B 映射）")
			}
			if s.VlanAccess != 0 || s.CrossConnect || len(s.Ports) > 0 || s.Gateway != nil {
				v.errf(p, "type=l3 交换机不允许 L2 专属配置（vlan_access/ports/gateway/cross-connect）")
			}
		default:
			v.errf(p+".type", "type 必须为 l2 或 l3")
		}
		if s.VlanAccess != 0 && !checkVlan(s.VlanAccess) {
			v.errf(p+".vlan_access", "vlan %d 必须在 1-4094", s.VlanAccess)
		}
		if s.Gateway != nil {
			for i, a := range s.Gateway.Addresses {
				if !checkCIDR(a) {
					v.errf(fmt.Sprintf("%s.gateway.addresses[%d]", p, i), "网关地址 %q 必须是 ip-prefix（CIDR）", a)
				}
			}
			if s.Gateway.Vrf != "" && !v.vrfNames[s.Gateway.Vrf] {
				v.errf(p+".gateway.vrf", "VRF %q 不存在", s.Gateway.Vrf)
			}
			v.checkACLRef(p+".gateway.acl_in", s.Gateway.AclIn)
			v.checkACLRef(p+".gateway.acl_out", s.Gateway.AclOut)
		}
		dupCheck(v, s.Ports, p+".ports", func(pt VSwitchPort) string { return strconv.Itoa(pt.Seq) }, "端口")
		for _, pt := range s.Ports {
			pp := fmt.Sprintf("%s.ports[%d]", p, pt.Seq)
			kinds := 0
			if pt.Interface != "" {
				kinds++
				if !v.anyIface(pt.Interface) {
					v.errf(pp, "接口 %q 不存在或不是物理口/bond", pt.Interface)
				}
			}
			if pt.Vnf != "" {
				kinds++
				if !v.vmVnics[pt.Vnf+"/"+pt.VnfInterface] {
					v.errf(pp, "VNF %q 的 vNIC %q 不存在", pt.Vnf, pt.VnfInterface)
				}
			}
			if pt.Container != "" {
				kinds++
				if !v.ctVnics[pt.Container+"/"+pt.ContainerInterface] {
					v.errf(pp, "容器 %q 的 vNIC %q 不存在", pt.Container, pt.ContainerInterface)
				}
			}
			if kinds != 1 {
				v.errf(pp, "端口必须且只能指定 interface/vnf/container 之一")
			}
			for _, tv := range pt.TrunkVlans {
				if !checkVlan(tv) {
					v.errf(pp+".trunk", "vlan %d 必须在 1-4094", tv)
				}
			}
			if pt.NativeVlan != 0 && !checkVlan(pt.NativeVlan) {
				v.errf(pp+".native", "vlan %d 必须在 1-4094", pt.NativeVlan)
			}
			v.checkACLRef(pp+".acl_in", pt.AclIn)
			v.checkACLRef(pp+".acl_out", pt.AclOut)
		}
	}
}

func (v *validator) checkVrfs(c Config) {
	dupCheck(v, c.Vrfs, "vrfs", func(r Vrf) string { return r.Name }, "VRF")
	for _, r := range c.Vrfs {
		p := fmt.Sprintf("vrfs[%s]", r.Name)
		if !v.checkName(p, r.Name, "VRF") {
			continue
		}
		dupCheck(v, r.L3Interfaces, p+".l3_interfaces", func(li L3Interface) string { return li.Interface }, "L3 接口")
		for _, li := range r.L3Interfaces {
			lp := fmt.Sprintf("%s.l3_interfaces[%s]", p, li.Interface)
			if !v.l3IfaceExists(li.Interface) {
				v.errf(lp, "L3 接口 %q 不存在或不是物理口/bond/vlan 子接口", li.Interface)
			}
			for i, a := range li.Addresses {
				if !checkCIDR(a) {
					v.errf(fmt.Sprintf("%s.addresses[%d]", lp, i), "地址 %q 必须是 ip-prefix（CIDR）", a)
				}
			}
			v.checkACLRef(lp+".acl_in", li.AclIn)
		}
		for _, rt := range r.Routes {
			rp := fmt.Sprintf("%s.routes[%s]", p, rt.Prefix)
			if rt.Prefix == "" || !checkCIDR(rt.Prefix) {
				v.errf(rp+".prefix", "路由前缀 %q 必须是 ip-prefix（CIDR）", rt.Prefix)
			}
			if !checkIP(rt.NextHop) {
				v.errf(rp+".next_hop", "下一跳 %q 必须是有效 ip", rt.NextHop)
			}
			if rt.Distance < 0 || rt.Distance > 255 {
				v.errf(rp+".distance", "distance %d 必须在 0-255", rt.Distance)
			}
		}
	}
}

func (v *validator) checkAcls(c Config) {
	dupCheck(v, c.Acls, "acls", func(a Acl) string { return a.Name }, "ACL")
	for _, a := range c.Acls {
		p := fmt.Sprintf("acls[%s]", a.Name)
		if !v.checkName(p, a.Name, "ACL") {
			continue
		}
		if len(a.Rules) == 0 {
			v.errf(p+".rules", "ACL 至少需要一条规则")
		}
		dupCheck(v, a.Rules, p+".rules", func(r AclRule) string { return strconv.Itoa(r.Seq) }, "规则")
		for _, r := range a.Rules {
			rp := fmt.Sprintf("%s.rules[%d]", p, r.Seq)
			switch r.Direction {
			case "", "ingress", "egress":
			default:
				v.errf(rp+".direction", "direction 必须为 ingress 或 egress")
			}
			for field, val := range map[string]string{"source": r.Source, "destination": r.Destination} {
				if val != "" && val != "any" && !checkCIDR(val) {
					v.errf(rp+"."+field, "%s %q 必须为 ip-prefix 或 any", field, val)
				}
			}
			switch r.Protocol {
			case "", "tcp", "udp", "icmp", "any":
			default:
				v.errf(rp+".protocol", "protocol 必须为 tcp|udp|icmp|any")
			}
			for field, val := range map[string]string{"source_port": r.SourcePort, "destination_port": r.DestinationPort} {
				if val != "" && !portSpecRe.MatchString(val) {
					v.errf(rp+"."+field, "端口 %q 必须为 <port> 或 <low>-<high>", val)
				}
			}
			switch r.Action {
			case "permit", "deny":
			default:
				v.errf(rp+".action", "action 必须为 permit 或 deny")
			}
		}
	}
}

func (v *validator) checkNat(c Config) {
	if c.Nat == nil {
		return
	}
	n := c.Nat
	dupCheck(v, n.SourcePools, "nat.source-pools", func(p NatSourcePool) string { return p.Name }, "NAT 池")
	for _, pool := range n.SourcePools {
		pp := fmt.Sprintf("nat.source-pools[%s]", pool.Name)
		if !v.checkName(pp, pool.Name, "NAT 池") {
			continue
		}
		lo, hi, ok := strings.Cut(pool.AddressRange, " to ")
		if !ok || !checkIP(strings.TrimSpace(lo)) || !checkIP(strings.TrimSpace(hi)) {
			v.errf(pp+".address_range", "地址范围 %q 必须为 \"<ip> to <ip>\"", pool.AddressRange)
		}
	}
	dupCheck(v, n.Rules, "nat.rules", func(r NatRule) string { return strconv.Itoa(r.Seq) }, "NAT 规则")
	for _, r := range n.Rules {
		rp := fmt.Sprintf("nat.rules[%d]", r.Seq)
		if !checkCIDR(r.MatchSource) {
			v.errf(rp+".match_source", "匹配源 %q 必须是 ip-prefix（CIDR）", r.MatchSource)
		}
		// NAT 仅作用于 L3 交换机（规格书 §4.3）
		if !v.l3vs[r.VirtualSwitch] {
			v.errf(rp+".virtual_switch", "virtual-switch %q 不是 L3 交换机", r.VirtualSwitch)
		}
		if (r.Action.SourcePool == "") == (r.Action.Interface == "") {
			v.errf(rp+".action", "action 必须且只能指定 source-pool 或 interface 之一")
		}
		if r.Action.SourcePool != "" {
			found := false
			for _, pool := range n.SourcePools {
				if pool.Name == r.Action.SourcePool {
					found = true
				}
			}
			if !found {
				v.errf(rp+".action.source_pool", "NAT 池 %q 不存在", r.Action.SourcePool)
			}
		}
	}
	for i, st := range n.Static {
		if !checkIP(st.InsideIP) {
			v.errf(fmt.Sprintf("nat.static[%d].inside_ip", i), "内部地址 %q 必须是有效 ip", st.InsideIP)
		}
		if !checkIP(st.OutsideIP) {
			v.errf(fmt.Sprintf("nat.static[%d].outside_ip", i), "外部地址 %q 必须是有效 ip", st.OutsideIP)
		}
	}
}

func (v *validator) checkPortMirroring(c Config) {
	dupCheck(v, c.PortMirroring, "port-mirroring", func(p PortMirroring) string { return p.Name }, "镜像会话")
	for _, pm := range c.PortMirroring {
		p := fmt.Sprintf("port-mirroring[%s]", pm.Name)
		if !v.checkName(p, pm.Name, "镜像会话") {
			continue
		}
		src := pm.Source
		kinds := 0
		if src.Interface != "" {
			kinds++
			if !v.anyIface(src.Interface) {
				v.errf(p+".source", "源接口 %q 不存在或不是物理口/bond", src.Interface)
			}
		}
		if src.Vnf != "" {
			kinds++
			if !v.vmVnics[src.Vnf+"/"+src.VnfInterface] {
				v.errf(p+".source", "源 VNF %q 的 vNIC %q 不存在", src.Vnf, src.VnfInterface)
			}
		}
		if kinds != 1 {
			v.errf(p+".source", "源必须且只能指定 interface 或 vnf 之一")
		}
		switch src.Direction {
		case "ingress", "egress", "both":
		default:
			v.errf(p+".source.direction", "direction 必须为 ingress|egress|both")
		}
		if pm.Analyzer == "" {
			v.errf(p+".analyzer", "分析端口缺失")
		} else if !v.ifaceNames[pm.Analyzer] {
			v.errf(p+".analyzer", "分析端口 %q 不是物理口", pm.Analyzer)
		}
	}
}

func (v *validator) checkQos(c Config) {
	dupCheck(v, c.QosPolicies, "qos-policies", func(q QosPolicy) string { return q.Name }, "限速策略")
	for _, q := range c.QosPolicies {
		p := fmt.Sprintf("qos-policies[%s]", q.Name)
		if !v.checkName(p, q.Name, "限速策略") {
			continue
		}
		if q.Cir <= 0 {
			v.errf(p, "cir 必须大于 0")
		}
		if q.Cbs <= 0 {
			v.errf(p+".cbs", "cbs 必须大于 0")
		}
	}
}

func (v *validator) checkResourcePools(c Config) {
	if c.ResourcePools == nil {
		return
	}
	rp := c.ResourcePools
	dupCheck(v, rp.Hugepages, "resource-pools.hugepages", func(h HPool) string { return h.PageSize }, "大页池")
	for _, hp := range rp.Hugepages {
		pp := fmt.Sprintf("resource-pools.hugepages[%s]", hp.PageSize)
		if hp.PageSize != "2M" && hp.PageSize != "1G" {
			v.errf(pp+".page_size", "page-size 必须为 2M 或 1G")
		}
		if hp.Count <= 0 {
			v.errf(pp+".count", "count 必须大于 0")
		}
	}
	if rp.CPU != nil {
		dupCheck(v, rp.CPU.Numa, "resource-pools.cpu.numa", func(n NumaNode) string { return strconv.Itoa(n.Node) }, "NUMA 节点")
		iso := map[int]bool{}
		for _, core := range rp.CPU.IsolatedCores {
			iso[core] = true
		}
		for _, n := range rp.CPU.Numa {
			np := fmt.Sprintf("resource-pools.cpu.numa[%d]", n.Node)
			if n.Node < 0 {
				v.errf(np+".node", "NUMA 节点号不能为负")
			}
			for _, core := range n.Cores {
				if !iso[core] {
					v.errf(np+".cores", "核 %d 不在隔离核池内，NUMA 声明必须与隔离核一致", core)
				}
			}
		}
	}
}

func (v *validator) checkVpp(c Config) {
	if c.Vpp == nil {
		return
	}
	vp := c.Vpp
	iso := map[int]bool{}
	hasIso := c.ResourcePools != nil && c.ResourcePools.CPU != nil
	if hasIso {
		for _, core := range c.ResourcePools.CPU.IsolatedCores {
			iso[core] = true
		}
	}
	if vp.CPU != nil {
		if vp.CPU.MainCore != 0 {
			if !hasIso {
				v.errf("vpp.cpu.main-core", "必须先配置 resource-pools cpu isolated-cores")
			} else if !iso[vp.CPU.MainCore] {
				v.errf("vpp.cpu.main-core", "核 %d 不在隔离核池内（FR-SYS-010）", vp.CPU.MainCore)
			}
		}
		if vp.CPU.CorelistWorkers != "" {
			cores, err := parseCoreList(vp.CPU.CorelistWorkers)
			if err != nil {
				v.errf("vpp.cpu.corelist_workers", "%v", err)
			} else if !hasIso {
				v.errf("vpp.cpu.corelist_workers", "必须先配置 resource-pools cpu isolated-cores")
			} else {
				for _, core := range cores {
					if !iso[core] {
						v.errf("vpp.cpu.corelist_workers", "核 %d 不在隔离核池内（FR-SYS-010）", core)
					}
				}
			}
			if vp.CPU.WorkersPerNuma != 0 {
				v.errf("vpp.cpu.workers_per_numa", "workers-per-numa 与 corelist-workers 互斥")
			}
		}
	}
	if vp.Memory != nil && vp.Memory.HugepagePreference != "" {
		if !v.hpSizes[vp.Memory.HugepagePreference] {
			v.errf("vpp.memory.hugepage_preference", "大页偏好 %s 必须与 resource-pools 页大小一致（FR-SYS-010）", vp.Memory.HugepagePreference)
		}
	}
	if vp.DPDK != nil {
		for _, d := range vp.DPDK.PerDev {
			// FR-CFG-011⑧：必须是 DPDK 接管的物理口（bond 不允许）
			if !v.ifaceNames[d.Interface] {
				v.errf(fmt.Sprintf("vpp.dpdk.per-dev[%s]", d.Interface), "%q 不是 DPDK 接管的物理口（FR-CFG-011⑧）", d.Interface)
			}
		}
		if u := vp.DPDK.UIODriver; u != "" && u != "vfio-pci" && u != "igb-uio" {
			v.errf("vpp.dpdk.uio_driver", "uio-driver 必须为 vfio-pci 或 igb-uio")
		}
	}
	for _, pl := range vp.Plugins {
		if pl.Name == "" {
			v.errf("vpp.plugins", "插件名缺失")
		}
		if pl.State != "enable" && pl.State != "disable" {
			v.errf(fmt.Sprintf("vpp.plugins[%s].state", pl.Name), "state 必须为 enable 或 disable")
		}
	}
}

func (v *validator) checkProtocols(c Config) {
	if c.Protocols == nil || c.Protocols.LLDP == nil {
		return
	}
	for _, li := range c.Protocols.LLDP.Interfaces {
		if !v.ifaceNames[li.Interface] {
			v.errf(fmt.Sprintf("protocols.lldp.interfaces[%s]", li.Interface), "接口 %q 不是物理口", li.Interface)
		}
	}
}

func (v *validator) checkVMFunctions(c Config) {
	dupCheck(v, c.VirtualMachineFunctions, "virtual-machine-functions", func(m VMFunction) string { return m.Name }, "VM")
	for _, m := range c.VirtualMachineFunctions {
		p := fmt.Sprintf("virtual-machine-functions[%s]", m.Name)
		if !v.checkName(p, m.Name, "VM") {
			continue
		}
		if m.Image == "" {
			v.errf(p+".image", "image 必填（引用仓库中的 vm-image）")
		}
		if m.VCPU.Count <= 0 {
			v.errf(p+".vcpu.count", "vcpu count 必填且大于 0")
		}
		if m.Memory.SizeMB <= 0 {
			v.errf(p+".memory.size_mb", "memory size-mb 必填且大于 0")
		}
		if m.Memory.HugepageSize != "" && !v.hpSizes[m.Memory.HugepageSize] {
			// FR-CFG-011⑪（池存在性部分）：余量校验由 commit 阶段结合账本执行
			v.errf(p+".memory.hugepage_size", "页大小 %s 无对应资源池", m.Memory.HugepageSize)
		}
		if m.Memory.Backing != "" && m.Memory.Backing != "hugepage" && m.Memory.Backing != "normal" {
			v.errf(p+".memory.backing", "backing 必须为 hugepage 或 normal")
		}
		dupCheck(v, m.Disks, p+".disks", func(d VMDisk) string { return d.Name }, "数据盘")
		for _, d := range m.Disks {
			if (d.SizeGB > 0) == (d.Image != "") {
				v.errf(fmt.Sprintf("%s.disks[%s]", p, d.Name), "数据盘必须且只能指定 size-gb 或 image 之一")
			}
		}
		dupCheck(v, m.Interfaces, p+".interfaces", func(n VnfInterface) string { return n.Name }, "vNIC")
		macs := map[string]bool{}
		for _, nic := range m.Interfaces {
			np := fmt.Sprintf("%s.interfaces[%s]", p, nic.Name)
			if !v.checkName(np, nic.Name, "vNIC") {
				continue
			}
			switch nic.Type {
			case "vhost-user":
				// FR-CFG-011①：vhost-user 必须大页内存
				if m.Memory.Backing == "normal" {
					v.errf(p, "vhost-user vNIC 要求 memory backing=hugepage（FR-CFG-011①），当前为 normal")
				}
			case "sriov-vf":
				if nic.Sriov == nil {
					v.errf(np+".sriov", "sriov-vf 类型必须指定 physical-interface 与 vf")
				} else if !v.ifaceNames[nic.Sriov.PhysicalInterface] {
					v.errf(np+".sriov.physical_interface", "物理口 %q 不存在", nic.Sriov.PhysicalInterface)
				}
			case "memif":
				v.errf(np+".type", "memif 仅用于容器 vNIC")
			default:
				v.errf(np+".type", "vNIC type 必须为 vhost-user 或 sriov-vf")
			}
			v.checkVSwitchRef(np+".virtual_switch", nic.VirtualSwitch)
			if nic.MAC != "" {
				if !checkMAC(nic.MAC) {
					v.errf(np+".mac", "MAC %q 格式不合法", nic.MAC)
				} else if macs[nic.MAC] {
					v.errf(np+".mac", "MAC %s 重复（FR-CFG-011③）", nic.MAC)
				}
				macs[nic.MAC] = true
			}
			if nic.Vlan != 0 && !checkVlan(nic.Vlan) {
				v.errf(np+".vlan", "vlan %d 必须在 1-4094", nic.Vlan)
			}
		}
	}
}

func (v *validator) checkContainerFunctions(c Config) {
	dupCheck(v, c.ContainerFunctions, "container-functions", func(ct ContainerFunction) string { return ct.Name }, "容器")
	for _, ct := range c.ContainerFunctions {
		p := fmt.Sprintf("container-functions[%s]", ct.Name)
		if !v.checkName(p, ct.Name, "容器") {
			continue
		}
		if ct.Image == "" {
			v.errf(p+".image", "image 必填（引用仓库中的 container-image）")
		}
		if ct.RestartPolicy != "" && ct.RestartPolicy != "no" && ct.RestartPolicy != "on-failure" {
			v.errf(p+".restart_policy", "restart-policy 必须为 no 或 on-failure")
		}
		dupCheck(v, ct.Interfaces, p+".interfaces", func(n VnfInterface) string { return n.Name }, "vNIC")
		for _, nic := range ct.Interfaces {
			np := fmt.Sprintf("%s.interfaces[%s]", p, nic.Name)
			if !v.checkName(np, nic.Name, "vNIC") {
				continue
			}
			if nic.Type != "memif" {
				v.errf(np+".type", "容器 vNIC type 必须为 memif（FR-NET-022）")
			}
			v.checkVSwitchRef(np+".virtual_switch", nic.VirtualSwitch)
		}
	}
}

// checkAddressOverlap 实现 FR-CFG-011②：全部 L3 地址（管理口、L3 接口、L2 网关）
// 两两不得同网段重叠。
func (v *validator) checkAddressOverlap(c Config) {
	type addr struct {
		path  string
		ipnet *net.IPNet
	}
	var all []addr
	if c.System != nil && c.System.Management != nil && c.System.Management.Address != "" {
		if _, ipnet, err := net.ParseCIDR(c.System.Management.Address); err == nil {
			all = append(all, addr{"system.management.address", ipnet})
		}
	}
	for _, r := range c.Vrfs {
		for _, li := range r.L3Interfaces {
			for i, a := range li.Addresses {
				if _, ipnet, err := net.ParseCIDR(a); err == nil {
					all = append(all, addr{fmt.Sprintf("vrfs[%s].l3_interfaces[%s].addresses[%d]", r.Name, li.Interface, i), ipnet})
				}
			}
		}
	}
	for _, s := range c.VirtualSwitches {
		if s.Gateway == nil {
			continue
		}
		for i, a := range s.Gateway.Addresses {
			if _, ipnet, err := net.ParseCIDR(a); err == nil {
				all = append(all, addr{fmt.Sprintf("virtual-switches[%s].gateway.addresses[%d]", s.Name, i), ipnet})
			}
		}
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			a, b := all[i], all[j]
			if a.ipnet.Contains(b.ipnet.IP) || b.ipnet.Contains(a.ipnet.IP) {
				v.errf(b.path, "地址网段与 %s 重叠（FR-CFG-011②）", a.path)
			}
		}
	}
}
