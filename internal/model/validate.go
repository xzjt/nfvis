package model

import (
	"fmt"
	"net"
	"regexp"
	"slices"
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
	v.checkHealth(c)
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
	v.checkManagementIsolation(c)
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
	macOwner   map[string]string // MAC -> 首个占用者（③：跨全部 VNF 的 MAC 命名空间）
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
	v.macOwner = map[string]string{}
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
		if s.Management.Interface != "" && !nameRe.MatchString(s.Management.Interface) {
			v.errf("system.management.interface", "管理网卡名 %q 非法", s.Management.Interface)
		}
		if s.Management.Address != "" && !checkCIDR(s.Management.Address) {
			v.errf("system.management.address", "管理口地址 %q 必须是 ip-prefix（CIDR）", s.Management.Address)
		}
		if s.Management.Gateway != "" && !checkIP(s.Management.Gateway) {
			v.errf("system.management.gateway", "管理口网关 %q 必须是有效 ip", s.Management.Gateway)
		}
	}
	// FR-SYS-006：并发连接上限须非负（0 = 不限）
	if s.API != nil && s.API.MaxSessions < 0 {
		v.errf("system.api.max_sessions", "并发连接上限不能为负: %d（0 表示不限）", s.API.MaxSessions)
	}
	if s.Syslog != nil && s.Syslog.Level != "" {
		switch s.Syslog.Level {
		case "debug", "info", "warn", "error":
		default:
			v.errf("system.syslog.level", "syslog level 必须为 debug|info|warn|error")
		}
	}
	// 兜底（R44-1）：口令哈希是**敏感叶子**，配置视图不返回它（决策 #25）；写路径对
	// "缺失的哈希"会从 committed 继承（同名用户），但**新用户**无从继承——没有口令的
	// 账号登不进去，必须在这里拦住（任何写路径都落不下无口令用户）。
	if s.Login != nil {
		for i, u := range s.Login.Users {
			if u.PasswordHash == "" {
				v.errf(fmt.Sprintf("system.login.users[%d]", i),
					"用户 %q 没有口令：请先用设置口令的语句或端点给它设口令", u.Name)
			}
		}
	}
	// FR-SYS-004：facility/severity 须为契约枚举内取值（失败在 commit 时逐条列出）
	if s.Syslog != nil && s.Syslog.Severity != "" {
		switch s.Syslog.Severity {
		case "debug", "info", "warn", "error":
		default:
			v.errf("system.syslog.severity", "syslog severity 必须为 debug|info|warn|error")
		}
	}
	if s.Syslog != nil && s.Syslog.Facility != "" {
		if _, ok := FacilityCode(s.Syslog.Facility); !ok {
			v.errf("system.syslog.facility", "syslog facility %q 不是合法的 RFC 5424 facility", s.Syslog.Facility)
		}
	}
	if s.Syslog != nil && s.Syslog.RemotePort != 0 && (s.Syslog.RemotePort < 1 || s.Syslog.RemotePort > 65535) {
		v.errf("system.syslog.remote_port", "远程 syslog 端口 %d 超出 1-65535", s.Syslog.RemotePort)
	}
	v.checkSystemLogin(s)
}

// ClassSuperUser 预置最高权限 class（与 aaa.ClassSuperUser 同值；model 是最内层，
// 不能反向 import aaa，故在此声明字面量）。
const ClassSuperUser = "super-user"

// EffectiveClass 解析用户的**有效 class**：Class 为空按 read-only 算。
//
// 与 service 侧同一判据（aaa.effectiveClass / api.effectiveClassOf）：缺省只读，
// 于是「没写 class 的账号」不算 super-user——守卫与授权判定必须同源，否则会出现
// 「守卫认为还有 super-user，实际没人有权限」的假通过。
func EffectiveClass(u LoginUserConfig) string {
	if u.Class != "" {
		return u.Class
	}
	return "read-only"
}

// CheckSuperUserPresent 检出「提交后本机将无人可登录」的整文档提交（自锁兜底）。
//
// 本地用户是唯一登录途径，一个 super-user 都不剩就只能带外恢复。单用户删除那条路
// 已有等价守卫（api.handleDeleteLoginUser），但整文档提交（PUT /configuration/candidate
// + commit、load override、配置恢复）能一次把用户表清空，故在**事务引擎的 commit**上
// 再兜一层。
//
// 判据：candidate 里**至少一个**有效 class 为 super-user 的用户即通过（有效 class 见
// EffectiveClass）。本函数**不在 Validate 的默认链里**——引擎的 Commit 直接调用它
// （决策 #152），自定义校验链（Options.Validate）漏不掉它。
func CheckSuperUserPresent(c Config) []ValidateError {
	if c.System != nil && c.System.Login != nil {
		for _, u := range c.System.Login.Users {
			if EffectiveClass(u) == ClassSuperUser {
				return nil
			}
		}
	}
	return []ValidateError{{
		Path: "system.login.users",
		Message: "提交被拒：配置里至少要保留一个 super-user 账号，否则本机将无人可登录——" +
			"请先建一个 super-user 账号；若要复位账号，请用 request system zeroize（恢复出厂）",
	}}
}

// checkSystemLogin 本地 AAA 配置校验：用户名语法、class 引用存在（预置类或
// 自定义定义）、策略数值合理；口令哈希格式由 aaa 写入侧保证。
func (v *validator) checkSystemLogin(s *SystemConfig) {
	if s.Login == nil {
		return
	}
	l := s.Login
	preset := map[string]bool{"super-user": true, "operator": true, "read-only": true}
	defined := map[string]bool{}
	dupCheck(v, l.Classes, "system.login.classes", func(c ClassDef) string { return c.Name }, "class")
	for _, c := range l.Classes {
		if v.checkName(fmt.Sprintf("system.login.classes[%s]", c.Name), c.Name, "class") {
			defined[c.Name] = true
		}
	}
	dupCheck(v, l.Users, "system.login.users", func(u LoginUserConfig) string { return u.Name }, "用户")
	for _, u := range l.Users {
		up := fmt.Sprintf("system.login.users[%s]", u.Name)
		if !v.checkName(up, u.Name, "用户") {
			continue
		}
		if u.Class != "" && !preset[u.Class] && !defined[u.Class] {
			v.errf(up+".class", "class %q 不存在（预置：super-user/operator/read-only 或自定义）", u.Class)
		}
	}
	if p := l.PasswordPolicy; p != nil {
		if p.MinLength != 0 && (p.MinLength < 4 || p.MinLength > 128) {
			v.errf("system.login.password_policy.min_length", "min-length %d 超出合理范围 4-128", p.MinLength)
		}
		if p.LockoutThreshold != 0 && (p.LockoutThreshold < 1 || p.LockoutThreshold > 100) {
			v.errf("system.login.password_policy.lockout_threshold", "lockout-threshold %d 超出合理范围 1-100", p.LockoutThreshold)
		}
		if p.LockoutMinutes != 0 && (p.LockoutMinutes < 1 || p.LockoutMinutes > 10080) {
			v.errf("system.login.password_policy.lockout_minutes", "lockout-minutes %d 超出合理范围 1-10080", p.LockoutMinutes)
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
				v.errf(p, "type=l3 交换机需要同名 VRF 条目承载 L3 配置（L3 虚拟交换机映射为同名 VRF）")
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
			// FR-CFG-011④：端口 VLAN 配置一致
			if pt.NativeVlan != 0 && slices.Contains(pt.TrunkVlans, pt.NativeVlan) {
				v.errf(pp+".native", "native VLAN %d 与 trunk 允许列表冲突", pt.NativeVlan)
			}
			if s.VlanAccess != 0 && len(pt.TrunkVlans) > 0 && !slices.Contains(pt.TrunkVlans, s.VlanAccess) {
				v.errf(pp, "交换机 access VLAN %d 不在端口 trunk 允许列表内", s.VlanAccess)
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

// checkHealth 校验硬件健康阈值范围（FR-SYS-012）。
func (v *validator) checkHealth(c Config) {
	if c.System == nil || c.System.Health == nil {
		return
	}
	h := c.System.Health
	if h.CPUTempCelsius < 0 || h.CPUTempCelsius > 120 {
		v.errf("system.health.cpu_temp_celsius", "CPU 温度阈值须在 0-120 摄氏度，实际 %d", h.CPUTempCelsius)
	}
	if h.DiskTempCelsius < 0 || h.DiskTempCelsius > 100 {
		v.errf("system.health.disk_temp_celsius", "磁盘温度阈值须在 0-100 摄氏度，实际 %d", h.DiskTempCelsius)
	}
	if h.DiskUsedPercent < 0 || h.DiskUsedPercent > 100 {
		v.errf("system.health.disk_used_percent", "磁盘使用率阈值须在 0-100 百分比，实际 %d", h.DiskUsedPercent)
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
	insideVS, outsideVRF := "", ""
	for _, r := range n.Rules {
		rp := fmt.Sprintf("nat.rules[%d]", r.Seq)
		if !checkCIDR(r.MatchSource) {
			v.errf(rp+".match_source", "匹配源 %q 必须是 ip-prefix（CIDR）", r.MatchSource)
		}
		// NAT 仅作用于 L3 交换机（规格书 §4.3）
		if !v.l3vs[r.VirtualSwitch] {
			v.errf(rp+".virtual_switch", "virtual-switch %q 不是 L3 交换机", r.VirtualSwitch)
		}
		// inside 转发域由 virtual-switch 派生；VPP NAT44 单实例仅一对 inside/outside VRF（决策 #52）。
		if r.VirtualSwitch != "" {
			if insideVS == "" {
				insideVS = r.VirtualSwitch
			} else if r.VirtualSwitch != insideVS {
				v.errf(rp+".virtual_switch",
					"V1 仅支持单一 inside 转发域：%q 与前一条规则的 %q 不一致（VPP NAT44 单实例仅一对 inside/outside VRF）",
					r.VirtualSwitch, insideVS)
			}
		}
		// 出接口必填（决策 #38）：VPP NAT44 EI 的 outside 不支持自动推断。
		if r.Action.Interface == "" {
			v.errf(rp+".action.interface",
				"必须指定出接口 interface（VPP NAT44 的 outside 不支持自动推断；source-pool 仅提供外部地址）")
		} else {
			// outside 转发域 = 出接口所属 VRF（决策 #52）。V1 要求出接口作为某 Vrf 的
			// l3-interface 且已配地址：默认表无配置地址的途径，NAT 回程不可达。
			owner, hasAddr := "", false
			for _, vrf := range c.Vrfs {
				for _, li := range vrf.L3Interfaces {
					if li.Interface == r.Action.Interface {
						owner = vrf.Name
						hasAddr = len(li.Addresses) > 0
					}
				}
			}
			switch {
			case owner == "":
				v.errf(rp+".action.interface",
					"出接口 %q 不在任何 VRF：V1 要求出接口作为某 L3 交换机（Vrf）的 l3-interface 并配置地址",
					r.Action.Interface)
			case !hasAddr:
				v.errf(rp+".action.interface",
					"出接口 %q 未配置地址：NAT 需以该地址（或源池）作外部地址并建立回程路由", r.Action.Interface)
			case outsideVRF == "":
				outsideVRF = owner
			case outsideVRF != owner:
				v.errf(rp+".action.interface",
					"出接口 %q 所属 VRF %q 与其它规则的 outside 转发域 %q 不一致：V1 仅支持单一 outside VRF",
					r.Action.Interface, owner, outsideVRF)
			}
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
				v.errf("vpp.cpu.main-core", "核 %d 不在隔离核池内", vp.CPU.MainCore)
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
						v.errf("vpp.cpu.corelist_workers", "核 %d 不在隔离核池内", core)
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
			v.errf("vpp.memory.hugepage_preference", "大页偏好 %s 必须与 resource-pools 页大小一致", vp.Memory.HugepagePreference)
		}
	}
	if vp.DPDK != nil {
		for _, d := range vp.DPDK.PerDev {
			// FR-CFG-011⑧：必须是 DPDK 接管的物理口（bond 不允许）。
			// 报错要点明**下一步**：单网卡覆盖项只能引用已在 interfaces 里声明的物理口，
			// 而操作者最常见的顺序错误正是「先写 dev 覆盖、后声明接口」（发现 #8）。
			if !v.ifaceNames[d.Interface] {
				v.errf(fmt.Sprintf("vpp.dpdk.per-dev[%s]", d.Interface),
					"%q 未在 interfaces 中声明：单网卡覆盖项只能引用已声明的物理口（先 set interfaces <口名>，再 set vpp dpdk dev <口名>）", d.Interface)
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
		for _, nic := range m.Interfaces {
			np := fmt.Sprintf("%s.interfaces[%s]", p, nic.Name)
			if !v.checkName(np, nic.Name, "vNIC") {
				continue
			}
			switch nic.Type {
			case "vhost-user":
				// FR-CFG-011①：vhost-user 必须大页内存
				if m.Memory.Backing == "normal" {
					v.errf(p, "vhost-user vNIC 要求 memory backing=hugepage，当前为 normal")
				}
			case "sriov-vf":
				if nic.Sriov == nil {
					v.errf(np+".sriov", "sriov-vf 类型必须指定 physical-interface 与 vf")
				} else if !v.ifaceNames[nic.Sriov.PhysicalInterface] {
					v.errf(np+".sriov.physical_interface", "物理口 %q 不存在", nic.Sriov.PhysicalInterface)
				} else if where := pfInDataPath(c, nic.Sriov.PhysicalInterface); where != "" {
					// FR-NET-021：占用该 VF 时禁止其 PF 端口进 bridge domain（驱动一致性约束）
					v.errf(np+".sriov.physical_interface",
						"VF 直通占用物理口 %s，其 PF 端口禁止进入 bridge-domain %s",
						nic.Sriov.PhysicalInterface, where)
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
				} else if owner, taken := v.macOwner[nic.MAC]; taken {
					v.errf(np+".mac", "MAC %s 重复，已由 %s 占用", nic.MAC, owner)
				} else {
					v.macOwner[nic.MAC] = m.Name + "/" + nic.Name
				}
			}
			if nic.Vlan != 0 && !checkVlan(nic.Vlan) {
				v.errf(np+".vlan", "vlan %d 必须在 1-4094", nic.Vlan)
			}
		}
	}
}

// pfInDataPath 判断物理口是否已被 L2 交换机端口引用（FR-NET-021 用），
// 返回 bridge-domain 名，未引用返回空串。
func pfInDataPath(c Config, pf string) string {
	for _, vs := range c.VirtualSwitches {
		if vs.Type != "l2" {
			continue
		}
		for _, p := range vs.Ports {
			if p.Interface == pf {
				return vs.Name
			}
		}
	}
	return ""
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
				v.errf(np+".type", "容器 vNIC type 必须为 memif")
			}
			v.checkVSwitchRef(np+".virtual_switch", nic.VirtualSwitch)
			if nic.MAC != "" {
				if !checkMAC(nic.MAC) {
					v.errf(np+".mac", "MAC %q 格式不合法", nic.MAC)
				} else if owner, taken := v.macOwner[nic.MAC]; taken {
					v.errf(np+".mac", "MAC %s 重复，已由 %s 占用", nic.MAC, owner)
				} else {
					v.macOwner[nic.MAC] = ct.Name + "/" + nic.Name
				}
			}
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
				v.errf(b.path, "地址网段与 %s 重叠", a.path)
			}
		}
	}
}

// checkManagementIsolation 强制管理网卡与数据面隔离（FR-NET-002 / FR-SEC-001）。
//
// 管理网卡须保留内核驱动、专供 SSH/API/syslog/Prometheus，**不得被任何数据面引用**：
// 一旦被 VPP 接管（vpp.dpdk.dev / interfaces）或成为交换机端口 / L3 接口 / bond 成员 /
// 镜像端口，管理面就可能与业务面同口——这正是 FR-NET-002 禁止的拓扑。
// 未指定 system.management.interface 时无从判定，不产生错误。
func (v *validator) checkManagementIsolation(c Config) {
	if c.System == nil || c.System.Management == nil {
		return
	}
	mgmt := c.System.Management.Interface
	if mgmt == "" {
		return
	}
	conflict := func(path, what string) {
		v.errf(path, "管理网卡 %q 不得用于数据面（%s）：管理面须与业务面隔离", mgmt, what)
	}

	// 业务网卡清单（会被 VPP 接管）
	for i, iface := range c.Interfaces {
		if iface.Name == mgmt {
			conflict(fmt.Sprintf("interfaces[%d].name", i), "interfaces 声明为业务网卡")
		}
	}
	// VPP DPDK 单网卡覆盖
	if c.Vpp != nil && c.Vpp.DPDK != nil {
		for i, d := range c.Vpp.DPDK.PerDev {
			if d.Interface == mgmt {
				conflict(fmt.Sprintf("vpp.dpdk.per_dev[%d].interface", i), "DPDK 设备参数覆盖")
			}
		}
	}
	// bond 成员
	for i, b := range c.Bonds {
		for j, m := range b.Members {
			if m == mgmt {
				conflict(fmt.Sprintf("bonds[%d].members[%d]", i, j), "bond "+b.Name+" 成员")
			}
		}
	}
	// 虚拟交换机端口
	for i, vs := range c.VirtualSwitches {
		for j, p := range vs.Ports {
			if p.Interface == mgmt {
				conflict(fmt.Sprintf("virtual-switches[%d].ports[%d].interface", i, j), "交换机 "+vs.Name+" 端口")
			}
		}
	}
	// L3 接口
	for i, vrf := range c.Vrfs {
		for j, l3 := range vrf.L3Interfaces {
			if l3.Interface == mgmt {
				conflict(fmt.Sprintf("vrfs[%d].l3_interfaces[%d].interface", i, j), "VRF "+vrf.Name+" 的 L3 接口")
			}
		}
	}
	// 端口镜像（源/分析口）
	for i, pm := range c.PortMirroring {
		if pm.Source.Interface == mgmt {
			conflict(fmt.Sprintf("port-mirroring[%d].source.interface", i), "镜像源端口")
		}
		if pm.Analyzer == mgmt {
			conflict(fmt.Sprintf("port-mirroring[%d].analyzer", i), "镜像分析端口")
		}
	}
}
