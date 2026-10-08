package model

import (
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
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
// （①vhost-user 大页、②地址重叠、③MAC 重复、⑧dpdk dev 为物理口）、
// FR-SYS-010 的 vpp 核/大页一致性，以及数据面角色互斥
// （一个网口只能出现在 bond 成员/交换机端口/L3 接口/镜像口之一，用户手册 §8.9）。
// 依赖外部状态的规则（⑤镜像类型匹配、⑨⑪资源配额余量、⑩NUMA 警告等）由
// 事务引擎 commit 阶段结合资源账本与镜像仓库执行。
func Validate(c Config) []ValidateError {
	v := &validator{}
	v.collect(c)
	v.checkSystem(c)
	v.checkFirewall(c)
	v.checkHealth(c)
	v.checkInterfaces(c)
	v.checkBonds(c)
	v.checkVirtualSwitches(c)
	v.checkVrfs(c)
	v.checkAcls(c)
	v.checkNat(c)
	v.checkPortMirroring(c)
	v.checkQos(c)
	v.checkVxlanTunnels(c)
	v.checkResourcePools(c)
	v.checkVpp(c)
	v.checkProtocols(c)
	v.checkVMFunctions(c)
	v.checkContainerFunctions(c)
	v.checkAddressOverlap(c)
	v.checkPortRoleExclusivity(c)
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
	vnicNames  map[string]bool // 配置已声明 vNIC 的 VPP 侧确定性名（vh-/mf-，见 ifacename.go）
	hpSizes    map[string]bool
	macOwner   map[string]string // MAC -> 首个占用者（③：跨全部 VNF 的 MAC 命名空间）

	// 端口安全（决策 #389）的成员账本（collect 填充，见该处注释）
	l2Members     map[string]bool
	bondMembers   map[string]bool
	portSecIfaces map[string]bool
	// bondOf：成员口 -> 所属 bond 名（决策 #398；storm-control 拒绝 bond 成员时给照做路径要点名 bond）
	bondOf map[string]string
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

// checkIP4 严格 IPv4（决策 #335：dhcp-relay 的 server 与 BVI 中继源地址都只走 v4；
// 决策 #352 扩展到 NAT44 的地址范围与静态映射——下发层 ParseIP4Address 严格 v4，
// 校验层放行 v6 只会把失败推迟到 commit 期的 invalid IP4 address）。
func checkIP4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// checkCIDR4 严格 IPv4 CIDR（决策 #352：NAT44 的 match-source 校验——网络地址须为 v4）。
func checkCIDR4(s string) bool {
	ip, _, err := net.ParseCIDR(s)
	return err == nil && ip.To4() != nil
}

// prefixFamily 显式前缀的地址族："ipv4"/"ipv6"；any/空/无法解析返回 ""（不判族——
// 非法值由 ip-prefix 校验单独报错，此处不重复计数）。
// 决策 #352：ACL 规则的 any/空一侧由编排层跟随显式侧家族，校验层只拦「两侧都写明了、
// 却一族 v4 一族 v6」的混族规则。
func prefixFamily(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return ""
	}
	if ip, _, err := net.ParseCIDR(s); err == nil {
		if ip.To4() != nil {
			return "ipv4"
		}
		return "ipv6"
	}
	if ip := net.ParseIP(s); ip != nil {
		if ip.To4() != nil {
			return "ipv4"
		}
		return "ipv6"
	}
	return ""
}

// gatewayHasV4 网关声明里是否带 IPv4 网关地址（dhcp-relay 的中继源取它）。
func gatewayHasV4(gw *VSGateway) bool {
	if gw == nil {
		return false
	}
	for _, a := range gw.Addresses {
		ip, _, err := net.ParseCIDR(a)
		if err == nil && ip.To4() != nil {
			return true
		}
	}
	return false
}

func (v *validator) anyIface(n string) bool { return v.ifaceNames[n] || v.bondNames[n] }

// l3IfaceExists 判断 L3 接口引用：物理口/bond、物理口上的 VLAN 子接口（如 ens2f0.100），
// 或**配置里已声明的 vNIC**（vhost-user 的 vh-<vm>-<vnic>、容器 memif 的 mf-<ct>-<vnic>，
// 名字由 ifacename.go 的规则派生，与编排层同源）。
//
// vNIC 可承载 L3 地址的由来（round84 证据 §14）：guest 的网关必须落在 guest 自己的口上——
// VPP 只为「接收接口自己拥有的地址」作答 ARP，同 VRF 但配在别的接口上的地址不代答。
// 网关配在物理口时 guest 100% Destination Host Unreachable。
//
// 判据只认「配置里确实声明过的 vNIC 名」：任意 vh-/mf- 前缀的随机名字、tap、以及一切
// 未声明的接口仍旧拒绝；物理口/bond/VLAN 子接口的既有行为不变。
func (v *validator) l3IfaceExists(n string) bool {
	if v.anyIface(n) || v.vnicNames[n] {
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
	v.vmNames, v.vmVnics, v.vnicNames = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range c.VirtualMachineFunctions {
		v.vmNames[m.Name] = true
		for _, nic := range m.Interfaces {
			v.vmVnics[m.Name+"/"+nic.Name] = true
			// VPP 侧接口名只对 vhost-user 类型的 vNIC 存在（与 orchestrator.VnfPortsOf
			// 的过滤口径一致：type 必须显式为 vhost-user，sriov-vf 不进 VPP）。
			if nic.Type == "vhost-user" {
				v.vnicNames[VnfIfaceName(m.Name, nic.Name)] = true
			}
		}
	}
	v.ctNames, v.ctVnics = map[string]bool{}, map[string]bool{}
	for _, ct := range c.ContainerFunctions {
		v.ctNames[ct.Name] = true
		for _, nic := range ct.Interfaces {
			v.ctVnics[ct.Name+"/"+nic.Name] = true
			if nic.Type == "memif" {
				v.vnicNames[MemifIfaceName(ct.Name, nic.Name)] = true
			}
		}
	}
	v.macOwner = map[string]string{}
	v.hpSizes = map[string]bool{}
	if c.ResourcePools != nil {
		for _, hp := range c.ResourcePools.Hugepages {
			v.hpSizes[hp.PageSize] = true
		}
	}
	// 端口安全（决策 #389）的三份成员账本：
	//   - l2Members：L2 交换机的**静态**端口成员（白名单是 L2 入向语义，前置条件以此为准；
	//     VNF/容器 vNIC 派生成员不在此列——端口安全语句只作用于已声明的物理口）；
	//   - bondMembers：bond 成员口（聚合口不支持端口安全，配在 bond 上是 v1 的有意取舍）；
	//   - portSecIfaces：配了白名单的接口（L3 接口 ACL 的 macip 绑定槽互斥反向检查用）。
	v.l2Members, v.bondMembers, v.portSecIfaces = map[string]bool{}, map[string]bool{}, map[string]bool{}
	v.bondOf = map[string]string{}
	for _, s := range c.VirtualSwitches {
		if s.Type != "l2" {
			continue
		}
		for _, pt := range s.Ports {
			if pt.Interface != "" {
				v.l2Members[pt.Interface] = true
			}
		}
	}
	for _, b := range c.Bonds {
		for _, m := range b.Members {
			v.bondMembers[m] = true
			if _, dup := v.bondOf[m]; !dup {
				v.bondOf[m] = b.Name
			}
		}
	}
	for _, i := range c.Interfaces {
		if len(i.PortSecurity) > 0 {
			v.portSecIfaces[i.Name] = true
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
	// 数据面实现（v3 决策 #404）：只认 vpp|kernel；空 = 未配置（回落 vpp）。
	if s.DataPlane != "" && s.DataPlane != DataPlaneVPP && s.DataPlane != DataPlaneKernel {
		v.errf("system.dataplane", "数据面必须为 vpp|kernel（当前 %q）；vpp = VPP 数据面，kernel = Linux 内核网络数据面", s.DataPlane)
	}
	if s.DataPlane == DataPlaneKernel {
		v.checkKernelDataPlane(c)
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
	// 决策 #356：历史时序存储的采样间隔/保留天数——仅在设置（非零）时校验范围；
	// 0 = 未设置，用默认值（默认值见 model 常量，越界在此拒绝而不是静默回落）。
	if s.Metrics != nil && s.Metrics.History != nil {
		h := s.Metrics.History
		if h.IntervalSeconds != 0 && (h.IntervalSeconds < MetricsIntervalMinSeconds || h.IntervalSeconds > MetricsIntervalMaxSeconds) {
			v.errf("system.metrics.history.interval_seconds",
				"采样间隔 %d 秒超出范围 %d-%d", h.IntervalSeconds, MetricsIntervalMinSeconds, MetricsIntervalMaxSeconds)
		}
		if h.RetentionDays != 0 && (h.RetentionDays < MetricsRetentionDaysMin || h.RetentionDays > MetricsRetentionDaysMax) {
			v.errf("system.metrics.history.retention_days",
				"保留天数 %d 超出范围 %d-%d", h.RetentionDays, MetricsRetentionDaysMin, MetricsRetentionDaysMax)
		}
	}
	v.checkSystemLogin(s)
}

// checkKernelDataPlane 内核数据面（system.dataplane = kernel）下**提交期拒绝**的配置。
//
// 口径（v3 决策 #404）：内核数据面尚不实现的族一律在**提交期拒绝**，不留「配置在、数据面不生效」
// 的假功能——尤其是安全相关的 ACL（拒规则不生效＝静默放行）。拒绝文案点名对象与替代路径。
// VPP 调优配置（vpp 段）不拒绝：它在内核数据面下不生效，但读视图会如实报出当前数据面，
// 且它是「切回 vpp 时立刻可用」的既有配置，故只在提交时给提示（见 config 引擎的告警）。
func (v *validator) checkKernelDataPlane(c Config) {
	const alt = "；如需该能力请先切换回 VPP 数据面（set system dataplane vpp）"
	// 已实现（不下发拒绝）：ACL、QoS 端口限速、端口镜像、风暴抑制、端口安全——
	// 内核侧分别落在 nftables（独立表）与 tc（clsact + police/mirred）上。
	if c.Protocols != nil && c.Protocols.LLDP != nil {
		v.errf("protocols.lldp", "当前数据面为 Linux 内核网络，LLDP 尚未实现%s", alt)
	}
	for _, vs := range c.VirtualSwitches {
		path := "virtual_switches[" + vs.Name + "]"
		// cross-connect（无学习点对点直通）：VPP 侧是 SwInterfaceSetL2Xconnect（点对点，
		// >2 端口即报错），内核侧只有 bridge 一种 L2 形态——按普通 bridge 静默处理会**改变语义**
		// （学习/泛洪/VLAN 语义不同，端口数也不校验）。属「命令成功、语义变了」一类，提交期拒绝。
		if vs.CrossConnect {
			v.errf(path+".cross_connect", "当前数据面为 Linux 内核网络，cross-connect（无学习点对点直通）尚未实现；"+
				"内核侧只有学习/泛洪形态的桥。请改用 L2 交换机 + 端口%s", alt)
		}
		if vs.DhcpRelayServer != "" {
			v.errf(path+".dhcp_relay_server", "当前数据面为 Linux 内核网络，DHCP 中继尚未实现%s", alt)
		}
		if vs.DhcpServerPoolStart != "" || vs.DhcpServerPoolEnd != "" {
			v.errf(path+".dhcp_server_pool_start", "当前数据面为 Linux 内核网络，DHCP 服务器尚未实现%s", alt)
		}
		if len(vs.DNSProxyServers) > 0 {
			v.errf(path+".dns_proxy_servers", "当前数据面为 Linux 内核网络，数据面 DNS 代理尚未实现%s", alt)
		}
		if vs.LearnLimit > 0 {
			v.errf(path+".learn_limit", "当前数据面为 Linux 内核网络，MAC 学习条数上限尚未实现%s", alt)
		}
		// 网关/端口 ACL 绑定与数据面无关地一律拒绝（VPP 26.06 不评估 BVI 域内流量，
		// 内核侧沿用同一口径以保持"同一份配置、同一语义"）：唯一可用的绑定点是三层接口。
		if vs.Gateway != nil && (vs.Gateway.AclIn != "" || vs.Gateway.AclOut != "") {
			v.errf(path+".gateway.acl_in", "当前数据面为 Linux 内核网络，网关 ACL 尚未实现%s", alt)
		}
		for _, p := range vs.Ports {
			if p.AclIn != "" || p.AclOut != "" {
				v.errf(path+".ports", "当前数据面为 Linux 内核网络，端口 ACL 尚未实现%s", alt)
			}
			if p.Container != "" {
				v.errf(path+".ports", "当前数据面为 Linux 内核网络，容器 vNIC 接入尚未实现%s", alt)
			}
		}
	}
	// 端口镜像：内核侧以 tc mirred 实现，但**源不能是 VNF 虚拟网卡**——宿主 tap 由 libvirt
	// 在域启动时创建，产品侧没有可靠的 tap 名映射。
	for _, pm := range c.PortMirroring {
		if pm.Source.Vnf != "" {
			v.errf("port_mirroring["+pm.Name+"]", "当前数据面为 Linux 内核网络，镜像源不能是 VNF 虚拟网卡（请改用物理口或 bond）%s", alt)
		}
	}
	// 三层接口：ACL 绑定已支持；「vNIC 作三层接口」仍不支持（要把宿主 tap 挂进 VRF，
	// 而 tap 由 libvirt 在域启动时创建，产品侧没有可靠的挂载时点）。
	vnics := vnicNames(c)
	for _, vrf := range c.Vrfs {
		for _, li := range vrf.L3Interfaces {
			if vnics[li.Interface] {
				v.errf("vrfs["+vrf.Name+"].l3_interfaces["+li.Interface+"]",
					"当前数据面为 Linux 内核网络，VNF 虚拟网卡不能直接作为三层接口；请改为接入一台已配网关的二层交换机%s", alt)
			}
			// vlan 子接口的派生名（`<接口名>.<vid>`）同样直接用作内核设备名：总长超过
			// IFNAMSIZ-1 时 `ip link add … type vlan` 必失败（VPP 侧没有这个限制）。
			v.checkKernelVlanSubif("vrfs["+vrf.Name+"].l3_interfaces["+li.Interface+"]", li)
		}
		// 静态路由的多下一跳（ECMP，`next-hop a,b`）：内核侧未实现——`via a,b` 会被 iproute2
		// 当成非法参数，整次提交以原始报错收场（文案不指向「ECMP 未实现」）。提交期拒绝。
		for _, rt := range vrf.Routes {
			if !strings.Contains(rt.NextHop, ",") {
				continue
			}
			v.errf(fmt.Sprintf("vrfs[%s].routes[%s].next_hop", vrf.Name, rt.Prefix),
				"当前数据面为 Linux 内核网络，多下一跳（ECMP）尚未实现；请拆成多条单跳路由（各条给 distance 决定优先级）%s", alt)
		}
	}
	for _, vm := range c.VirtualMachineFunctions {
		for _, nic := range vm.Interfaces {
			if nic.Type == "memif" {
				v.errf("virtual_machine_functions["+vm.Name+"].interfaces["+nic.Name+"]",
					"当前数据面为 Linux 内核网络，memif 接入尚未实现；VM 请用 virtio 网卡（宿主 tap + vhost-net）%s", alt)
			}
			// vNIC 接入 type=l3 的交换机：VPP 侧是受支持形态（vNIC 作 L3 接口进同名 VRF），
			// 内核侧 L3 交换机**不建 bridge**（只有 VRF 设备）⇒ 域定义把 tap 指向不存在的桥，
			// 提交期全绿、**起 VM 时才失败**。接入点应为已配网关的 L2 交换机。
			if nic.VirtualSwitch != "" && v.l3vs[nic.VirtualSwitch] {
				v.errf("virtual_machine_functions["+vm.Name+"].interfaces["+nic.Name+"].virtual_switch",
					"当前数据面为 Linux 内核网络，VNF 虚拟网卡不能接入 type=l3 的交换机 %q："+
						"内核侧 L3 交换机不建桥，tap 无处可挂（直到起 VM 才会失败）。"+
						"请改为接入一台已配网关的 L2 交换机%s", nic.VirtualSwitch, alt)
			}
		}
	}
	for _, ct := range c.ContainerFunctions {
		if len(ct.Interfaces) > 0 {
			v.errf("container_functions["+ct.Name+"].interfaces",
				"当前数据面为 Linux 内核网络，容器 vNIC（memif）接入尚未实现%s", alt)
		}
	}
	// 内核接口名上限 15 字符（IFNAMSIZ-1）：交换机/bond/隧道/L3 交换机名直接用作内核设备名，
	// 超长会被内核拒绝，故在提交期就拦下（VPP 侧没有这个限制）。
	for _, vs := range c.VirtualSwitches {
		v.checkKernelLinkName("virtual_switches["+vs.Name+"]", vs.Name)
	}
	for _, b := range c.Bonds {
		v.checkKernelLinkName("bonds["+b.Name+"]", b.Name)
	}
	for _, vx := range c.VxlanTunnels {
		v.checkKernelLinkName("vxlan_tunnels["+vx.Name+"]", vx.Name)
	}
	for _, vrf := range c.Vrfs {
		v.checkKernelLinkName("vrfs["+vrf.Name+"]", vrf.Name)
	}
	// 派生设备名互撞：两个不同对象（或对象与派生的网关 VRF 名）映射到同一个内核设备名时
	// 提交期拒绝——内核设备名全局唯一，撞名的代价是后者下发失败/配置与数据面错位。
	v.checkKernelDerivedNameCollisions(c)
	// NAT 跨转发域（inside 交换机派生的 VRF ≠ 出接口所属 VRF）：内核侧的 nft 规则**不区分
	// 转发域**（`ip saddr … oifname … masquerade` 对所有表生效），也没有跨表 leaking——规则在
	// 场却一个包都不命中（`ping` 零通、计数零），反向还会把别的 VRF 里的同源前缀一并 NAT
	// （VPP 侧按 inside 转发域作用域）。故对「两侧都落在具名 VRF 且不同」的形态提交期拒绝；
	// 两侧都为「默认表/未归属 VRF」时视为一致（不在此判）。
	if c.Nat != nil {
		for _, r := range c.Nat.Rules {
			inside := r.VirtualSwitch
			outside := vrfOfInterface(c, r.Action.Interface)
			if !kernelNatDomainsCross(inside, outside) {
				continue
			}
			v.errf(fmt.Sprintf("nat.rules[%d]", r.Seq),
				"当前数据面为 Linux 内核网络，NAT 规则不支持跨转发域：inside 交换机 %q 的转发域与出接口 %q 所属转发域 %q 不一致"+
					"（内核侧 NAT 不区分转发域、也没有跨域 leaking，规则不会命中任何包）。"+
					"请改为同一转发域（inside 交换机即出接口所属的 L3 交换机）%s", inside, r.Action.Interface, outside, alt)
		}
	}
	// ACL 的 `protocol icmp` + 端口字段：两个数据面语义不同——VPP 侧把端口当 **ICMP type/code**
	// 解读，内核侧（nftables）没有对应表达、端口字段会被静默忽略 ⇒ 规则看着在、匹配范围却不是
	// 用户写的意思。提交期拒绝，让用户显式选择（删掉端口字段，或切回 VPP）。
	for _, acl := range c.Acls {
		for _, r := range acl.Rules {
			if r.Protocol != "icmp" || (r.SourcePort == "" && r.DestinationPort == "") {
				continue
			}
			v.errf(fmt.Sprintf("acls[%s].rules[%d]", acl.Name, r.Seq),
				"当前数据面为 Linux 内核网络，protocol icmp 的端口字段（source-port/destination-port）会被忽略："+
					"内核侧端口匹配不了 ICMP type/code（VPP 侧按 type/code 解读）。请删掉端口字段%s", alt)
		}
	}
}

// checkKernelVlanSubif 内核数据面下 vlan 子接口的派生名（`<接口名>.<vid>`）直接用作内核设备名，
// 总长超过 IFNAMSIZ-1 时下发必失败（VPP 侧无此限制），故在提交期拦下。
//
// 名字长度按编排层 netkernel.LinkName 的同一条规则折算（model 不能 import 编排层）：
// ≤15 字节原样，超长一律映射成 15 字节（截断 + 派生后缀）——故这里只关心映射后的**长度**。
func (v *validator) checkKernelVlanSubif(path string, li L3Interface) {
	if li.Vlan <= 0 {
		return
	}
	derivedLen := kernelLinkNameLen(li.Interface) + 1 + len(strconv.Itoa(li.Vlan))
	if derivedLen > kernelLinkNameMax {
		v.errf(path, "当前数据面为 Linux 内核网络，vlan 子接口的派生接口名（%q + vlan %d ⇒ %d 个字符）"+
			"超过内核接口名上限 %d 个字符：请缩短接口名或改用更小的 vlan id",
			li.Interface, li.Vlan, derivedLen, kernelLinkNameMax)
	}
}

// kernelLinkNameLen 编排层 netkernel.LinkName 映射后的名字长度：≤15 字节原样，超长映射成 15 字节。
func kernelLinkNameLen(name string) int {
	if n := len(strings.TrimSpace(name)); n <= kernelLinkNameMax {
		return n
	}
	return kernelLinkNameMax
}

// vrfOfInterface 接口名（l3-interface 的 interface 字段值）所属的 VRF 名；未归属任何 VRF 返回 ""。
func vrfOfInterface(c Config, iface string) string {
	if iface == "" {
		return ""
	}
	owner := ""
	for _, vrf := range c.Vrfs {
		for _, li := range vrf.L3Interfaces {
			if li.Interface == iface {
				owner = vrf.Name
			}
		}
	}
	return owner
}

// kernelNatDomainsCross 两侧转发域是否跨域：都落在**具名** VRF 且不同才算跨域；
// 「默认表/未归属 VRF」（空串）视为一致——不在此判（其合法性由 checkNat 的其它规则负责）。
func kernelNatDomainsCross(inside, outside string) bool {
	return inside != "" && outside != "" && inside != outside
}

// checkKernelLinkName 内核数据面下对象名直接用作内核接口名，须满足内核的长度限制。
func (v *validator) checkKernelLinkName(path, name string) {
	if len(name) > kernelLinkNameMax {
		v.errf(path, "当前数据面为 Linux 内核网络，对象名 %q 超过内核接口名上限 %d 个字符，请缩短",
			name, kernelLinkNameMax)
	}
}

// kernelDevNameOwner 内核数据面下"一个内核设备名"及归属它的配置对象。
type kernelDevNameOwner struct {
	dev  string // 本配置会创建的内核设备名（派生名）
	path string // 配置对象路径（errf 的定位）
	what string // 对象描述（报错文案要点名**配置对象**，只给设备名操作者对不上是哪个声明）
	// explicitGW 该设备名来自**显式**的 `gateway vrf <名>` 引用，而不是按对象名派生的。
	// 多个网关显式引用同一台 VRF 是受支持形态（编排侧 ensureVRF 幂等、内核允许多个 bridge
	// 入同一 VRF）：两边都是显式引用时不算冲突；与其它对象/派生名撞名仍要报。
	explicitGW bool
}

// checkKernelDerivedNameCollisions 内核数据面下**派生设备名互撞**的提交期拒绝。
//
// 为什么必须在提交期拦：内核设备名全局唯一（`ip link add` 撞名报 `File exists`），而本产品的
// 设备名由用户对象名派生——两个不同对象撞进同一个名字时，先下发的对象占住设备、后下发的直接
// 失败（或更糟：把前者当成"已存在"复用），配置与数据面从此错位，而失败文案只给设备名、指不回
// 是哪个声明。已知可达形态：交换机 `lan` 的网关 VRF 派生名是 `vr-lan`，而一个**名叫 `vr-lan`
// 的交换机/bond/隧道/VRF** 的内核设备名也是 `vr-lan`。
//
// 集合口径与编排层一致（netkernel 的 dataplaneDevices / runtime 设备集合）：只收**本配置会
// 创建**的设备——网关 VRF 仅在交换机声明了网关地址时才建；设备名按 netkernel.LinkName 的同一
// 条映射规则折算（model 不能 import 编排层，规则与 netkernel/naming.go 单源同步）。
func (v *validator) checkKernelDerivedNameCollisions(c Config) {
	first := map[string]kernelDevNameOwner{}
	for _, o := range kernelDerivedNameOwners(c) {
		prev, dup := first[o.dev]
		if !dup {
			first[o.dev] = o
			continue
		}
		if prev.explicitGW && o.explicitGW {
			continue // 两台网关显式引用同一台 VRF：受支持形态，不算冲突
		}
		v.errf(o.path, "当前数据面为 Linux 内核网络：%s 与 %s 会派生同一个内核设备名 %q"+
			"（内核设备名全局唯一，先下发的对象占住设备、后下发的直接失败，配置与数据面从此不一致）。"+
			"请把其中一个对象改名", prev.what, o.what, o.dev)
	}
}

// kernelDerivedNameOwners 归集内核数据面下本配置会创建的设备名及归属对象（顺序固定，报错可复现）。
func kernelDerivedNameOwners(c Config) []kernelDevNameOwner {
	var out []kernelDevNameOwner
	for _, vs := range c.VirtualSwitches {
		// type=l3 的交换机**不建** bridge（它的设备是 vrfs 条目那条 VRF，见下）——两处都收会
		// 把同一个 L3 交换机报成撞名自己。
		if vs.Type == "l3" {
			continue
		}
		out = append(out, kernelDevNameOwner{
			dev:  kernelDerivedLinkName(vs.Name),
			path: "virtual_switches[" + vs.Name + "]",
			what: fmt.Sprintf("交换机 %q", vs.Name),
		})
		// 网关 VRF：仅在声明了网关地址时才创建（与编排层同一判据）。
		if vs.Gateway == nil || len(vs.Gateway.Addresses) == 0 {
			continue
		}
		o := kernelDevNameOwner{path: "virtual_switches[" + vs.Name + "].gateway.vrf"}
		if vs.Gateway.Vrf != "" {
			o.dev, o.explicitGW = kernelDerivedLinkName(vs.Gateway.Vrf), true
			o.what = fmt.Sprintf("交换机 %q 网关显式引用的 VRF %q", vs.Name, vs.Gateway.Vrf)
		} else {
			o.dev = kernelDerivedGatewayVRFName(vs.Name)
			o.what = fmt.Sprintf("交换机 %q 的网关 VRF（派生名 %s）", vs.Name, o.dev)
		}
		out = append(out, o)
	}
	for _, b := range c.Bonds {
		out = append(out, kernelDevNameOwner{
			dev:  kernelDerivedLinkName(b.Name),
			path: "bonds[" + b.Name + "]",
			what: fmt.Sprintf("bond %q", b.Name),
		})
	}
	for _, t := range c.VxlanTunnels {
		out = append(out, kernelDevNameOwner{
			dev:  kernelDerivedLinkName(t.Name),
			path: "vxlan_tunnels[" + t.Name + "]",
			what: fmt.Sprintf("隧道 %q", t.Name),
		})
	}
	for _, vrf := range c.Vrfs {
		out = append(out, kernelDevNameOwner{
			dev:  kernelDerivedLinkName(vrf.Name),
			path: "vrfs[" + vrf.Name + "]",
			what: fmt.Sprintf("L3 交换机 %q", vrf.Name),
		})
		for _, li := range vrf.L3Interfaces {
			if li.Vlan <= 0 {
				continue
			}
			dev := fmt.Sprintf("%s.%d", kernelDerivedLinkName(li.Interface), li.Vlan)
			if len(dev) > kernelLinkNameMax {
				continue // 派生名超长的已由 checkKernelVlanSubif 报错（下不到数据面，不参与撞名）
			}
			out = append(out, kernelDevNameOwner{
				dev:  dev,
				path: fmt.Sprintf("vrfs[%s].l3_interfaces[%s]", vrf.Name, li.Interface),
				what: fmt.Sprintf("三层接口 %q 的 VLAN %d 子接口", li.Interface, li.Vlan),
			})
		}
	}
	return out
}

// kernelDerivedLinkName 编排层 netkernel.LinkName 的等价映射（model 不能 import 编排层）：
// ≤15 字节原样；超长按「前缀 + FNV-1a 32 位全宽哈希（8 位十六进制）」截断，前缀也占长度预算。
// ⚠ 单一真源在 internal/orchestrator/netkernel/naming.go，那边改了必须同步这里。
func kernelDerivedLinkName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) <= kernelLinkNameMax {
		return name
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(name))
	prefix := strings.TrimRight(name[:kernelLinkNameMax-1-kernelLinkNameHashDigits], "-")
	return fmt.Sprintf("%s-%08x", prefix, sum.Sum32())
}

// kernelDerivedGatewayVRFName 编排层 netkernel.GatewayVRFName 的等价映射：
// L2 交换机无显式网关 VRF 时的专属 VRF 设备名（`vr-<交换机名>` 再走同一套映射）。
func kernelDerivedGatewayVRFName(swName string) string {
	return kernelDerivedLinkName("vr-" + swName)
}

// kernelLinkNameMax 内核接口名上限（IFNAMSIZ-1）。
const kernelLinkNameMax = 15

// kernelLinkNameHashDigits 截断名里的哈希位数（与编排层 netkernel 的 linkNameHashDigits 同步）。
const kernelLinkNameHashDigits = 8

// vnicNames 收集配置里所有 VNF 虚拟网卡名（用于判定 l3-interface 是否引用了 vNIC）。
func vnicNames(c Config) map[string]bool {
	out := map[string]bool{}
	for _, vm := range c.VirtualMachineFunctions {
		for _, nic := range vm.Interfaces {
			if nic.Name != "" {
				out[nic.Name] = true
			}
		}
	}
	for _, ct := range c.ContainerFunctions {
		for _, nic := range ct.Interfaces {
			if nic.Name != "" {
				out[nic.Name] = true
			}
		}
	}
	return out
}

// checkFirewall 校验管理面主机防火墙（决策 #388）：序号/动作/来源/协议/端口/至少一条匹配条件/
// 重复规则/默认策略，以及「配置了防火墙但未声明管理口」这条前置。
//
// 为什么「未声明管理口」在提交期拒绝而不是下发期报错：防火墙的作用面就是管理口入向——
// 管理口未声明时规则没有锚点，静默接受只会留下「配置说在过滤、实际无从生效」的假保护。
func (v *validator) checkFirewall(c Config) {
	fw := c.FirewallOf()
	if fw == nil {
		return
	}
	switch fw.DefaultPolicy {
	case "", "accept", "drop":
	default:
		v.errf("system.firewall.default_policy", "默认策略必须为 accept|drop（缺省 accept）")
	}
	dupCheck(v, fw.Rules, "system.firewall.rules", func(r FirewallRule) string { return strconv.Itoa(r.Seq) }, "规则")
	seen := map[string]int{}
	for _, r := range fw.Rules {
		p := fmt.Sprintf("system.firewall.rules[%d]", r.Seq)
		if r.Seq < 1 || r.Seq > 9999 {
			v.errf(p, "规则序号 %d 超出范围 1-9999", r.Seq)
		}
		switch r.Action {
		case "accept", "drop":
		default:
			v.errf(p+".action", "action 必须为 accept 或 drop")
		}
		if r.Source != "" && !firewallSourceOK(r.Source) {
			v.errf(p+".source", "来源 %q 必须为 IPv4/IPv6 前缀（也接受单个 IP，按 /32、/128 处理）", r.Source)
		}
		proto := r.Protocol
		switch proto {
		case "", "tcp", "udp", "icmp", "any":
		default:
			v.errf(p+".protocol", "protocol 必须为 tcp|udp|icmp|any")
		}
		if r.Port != 0 {
			if proto != "tcp" && proto != "udp" {
				v.errf(p+".port", "port 仅 tcp/udp 规则可配（当前 protocol=%s）", orUnset(proto))
			}
			if r.Port < 1 || r.Port > 65535 {
				v.errf(p+".port", "端口 %d 超出 1-65535", r.Port)
			}
		}
		// 至少一条匹配条件：防手滑写出「裸 action」规则（那种规则会匹配全部管理口入向流量）。
		// protocol 的 ""、any 都表示「任意协议」，不算匹配条件。
		if r.Source == "" && (proto == "" || proto == "any") && r.Port == 0 {
			v.errf(p, "规则至少给一条匹配条件（source/protocol/port 之一）；只写 action 的规则会匹配全部管理入向流量")
		}
		// 完全重复规则（匹配条件与动作全同，含 any⇄未设的归一）拒绝——seq 不同但语义全同的
		// 重复项只会掩盖「我以为改了」的编辑失误。
		normProto := proto
		if normProto == "any" {
			normProto = ""
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", r.Action, r.Source, normProto, r.Port)
		if prev, dup := seen[key]; dup {
			v.errf(p, "与规则 %d 完全重复（匹配条件与动作相同），请删除其一或改为不同的匹配条件", prev)
		} else {
			seen[key] = r.Seq
		}
	}
	if fw.FirewallEnabled() && c.MgmtInterfaceOf() == "" {
		v.errf("system.firewall", "配置主机防火墙前请先声明管理口：set system management interface <ifname>（防火墙作用于管理口入向，未声明管理口时规则没有作用对象）")
	}
}

// firewallSourceOK 来源取值：v4/v6 前缀（本渲染按前缀下发）或**单个 IP**（渲染时按 /32、/128 归一）。
// 与 ACL 的「必须 ip-prefix」相比有意放宽一档：写单个主机地址是最常见的意图，nft 的前缀语义对
// 裸地址与 /32 完全一致，拒绝它只会让操作者多写几位而没有任何安全增益。
func firewallSourceOK(s string) bool {
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}

// orUnset 空值的人读占位（错误文案用；避免出现 "protocol=" 这种半截话）。
func orUnset(s string) string {
	if s == "" {
		return "未设置"
	}
	return s
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
	// 登录横幅（决策 #303）：单行文本、最长 512 字节。单行约束的由来：CLI 对多行
	// 引号值只取首行（已登记的解析缺陷），banner 在语义上只该是一行，约束单行
	// 从根上规避；上限的报错文案必须带上限值与当前字节数，操作者才知道怎么改。
	if l.Banner != "" {
		const bannerMax = 512
		if n := len(l.Banner); n > bannerMax {
			v.errf("system.login.banner", "登录横幅超过长度上限 %d 字节（当前 %d 字节），请缩短后重试", bannerMax, n)
		}
		if strings.ContainsAny(l.Banner, "\n\r") {
			v.errf("system.login.banner", "登录横幅须为单行文本（不能包含换行）")
		}
	}
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
		// 出向绑定（决策 #331）：与入向同一份策略账本校验（删除被引用的策略即在此被拒）。
		if i.EgressPolicy != "" && !v.qosNames[i.EgressPolicy] {
			v.errf(p+".egress_policy", "限速策略 %q 不存在", i.EgressPolicy)
		}
		// 入向风暴抑制（决策 #385，FR-NET-019）：值域 1-100000000 kbps；对象出现但两类
		// 都没给值等于「配了个空声明」——它不下发任何限速，却会让读视图/display set 把它
		// 当成已配置，按无效配置拒绝（不静默留空壳）。
		// bond 成员口拒绝（决策 #398，R171-9）：成员口入向被 bond-input 吃掉（见本文件
		// checkPortRoleExclusivity 注释与用户手册 §8.9），挂上去的 L2 分类表/policer 不会被
		// 查到 ⇒ 保护假象。与 portsec 同判据；此处额外给照做路径（点名 bond，便于先移出）。
		if sc := i.StormControl; sc != nil {
			if v.bondMembers[i.Name] {
				v.errf(p+".storm_control", "接口 %s 是 bond %s 的成员口，不支持风暴抑制（成员口入向被 bond-input 吃掉，"+
					"挂上的 policer 不会被查到）——先 delete bonds %s members %s 把该口移出聚合再配",
					i.Name, v.bondOf[i.Name], v.bondOf[i.Name], i.Name)
			}
			// 与端口安全白名单的 L2 入向分类槽互斥（决策 #401，R176-2）：storm 走 classify、
			// portsec 走 macip，底座同一接口只有一个 L2 入向分类槽——并存时后挂者胜、另一项
			// 静默不生效（限速假象），且先配者的删除会撞上「槽被占用」而失败。按 #340/#389 的
			// 互斥先例提交期硬拒、双向校验（反向在 checkPortSecurity），文案点名两者 + 机理 + 照做路径。
			if len(i.PortSecurity) > 0 {
				v.errf(p+".storm_control", "接口 %s 同时配了风暴抑制与端口安全白名单：两者都绑该口唯一的 L2 入向分类槽"+
					"（风暴抑制走 classify、端口安全走 macip，单槽互斥），不能并存——请两者留其一："+
					"delete interfaces %s storm-control 或 delete interfaces %s port-security",
					i.Name, i.Name, i.Name)
			}
			if sc.BroadcastKbps == 0 && sc.MulticastKbps == 0 {
				v.errf(p+".storm_control",
					"风暴抑制未给出任何类别：broadcast_kbps / multicast_kbps 至少给一个（两类各自独立，单位 kbps）")
			}
			if sc.BroadcastKbps != 0 && !checkStormKbps(sc.BroadcastKbps) {
				v.errf(p+".storm_control.broadcast_kbps", "广播抑制 %d 超出范围：须为 1-100000000（kbps）", sc.BroadcastKbps)
			}
			if sc.MulticastKbps != 0 && !checkStormKbps(sc.MulticastKbps) {
				v.errf(p+".storm_control.multicast_kbps", "组播抑制 %d 超出范围：须为 1-100000000（kbps）", sc.MulticastKbps)
			}
		}
		v.checkPortSecurity(c, p, i)
	}
}

// maxPortSecMACs 每接口的端口安全白名单上限（决策 #389）。
const maxPortSecMACs = 32

// checkPortSecurity 端口安全白名单校验（决策 #389）。
//
// 条目：合法 MAC（net.ParseMAC；归一小写由 PortSecMAC 解码层保证，此处按小写比较）；
// 重复大小写不敏感地拒绝；上限 32 条。前置（白名单是 L2 入向语义，缺一即拒）：
//   - 接口须是某 L2 交换机的**静态**成员端口——「把带白名单的口移出交换机」「删所在
//     交换机」后新配置里该口不再有 L2 成员身份，同被此处拒绝（引用守卫同族：指向先删
//     白名单）。整条删除接口元素时白名单随元素消失；该路径**可达**（round171 §1.3 实测：
//     同一次提交既删接口元素、又删引用它的交换机端口/交换机即可），其数据面 macip 绑定的
//     撤销由提交编排的接口级删除计划（del-interface，决策 #390③）负责，不在此另设校验守卫；
//   - bond 成员口拒绝（聚合口的白名单语义不在 v1 范围；照 storm-control 同例不做）；
//   - macip 绑定槽互斥（正向）：该口已作为某 VRF 的 l3-interface 绑定 ACL 时拒绝——
//     同一接口只能有一个 macip 绑定，L3 接口 ACL 的伴随 macip（放行非 IP 帧）与白名单
//     争同一槽位。反向检查在 checkVrfs。
func (v *validator) checkPortSecurity(c Config, p string, i InterfaceConfig) {
	if len(i.PortSecurity) == 0 {
		return
	}
	if len(i.PortSecurity) > maxPortSecMACs {
		v.errf(p+".port_security", "白名单最多 %d 条 MAC，实际 %d 条（先 delete interfaces %s port-security mac <mac> 移除多余的）",
			maxPortSecMACs, len(i.PortSecurity), i.Name)
	}
	seen := map[string]int{}
	for j, m := range i.PortSecurity {
		pp := fmt.Sprintf("%s.port_security[%d]", p, j)
		hw, err := net.ParseMAC(string(m))
		if err != nil {
			v.errf(pp, "白名单 MAC %q 非法：%v（形如 b0:b0:00:00:00:01）", string(m), err)
			continue
		}
		norm := strings.ToLower(hw.String())
		if prev, dup := seen[norm]; dup {
			v.errf(pp, "白名单 MAC %q 重复（与第 %d 条相同，大小写不敏感）", string(m), prev+1)
		}
		seen[norm] = j
	}
	if !v.l2Members[i.Name] {
		v.errf(p+".port_security", "接口 %s 不是任何 L2 交换机的静态成员端口：端口安全是 L2 入向语义——"+
			"先 set virtual-switches <交换机> ports <序号> interface %s 把该口挂进交换机；"+
			"若要移出口或删交换机，请先删除该口的白名单再提交", i.Name, i.Name)
	}
	if v.bondMembers[i.Name] {
		v.errf(p+".port_security", "接口 %s 是 bond 成员口，不支持端口安全（聚合口的白名单不在本版本范围）", i.Name)
	}
	// 与风暴抑制的 L2 入向分类槽互斥（决策 #401，R176-2）：portsec 走 macip、storm 走 classify，
	// 底座同一接口只有一个 L2 入向分类槽——并存时后挂者胜、另一项静默不生效（白名单假象），
	// 且先配者的删除会撞上「槽被占用」而失败。与上面 macip 槽互斥同口径：提交期硬拒、双向校验
	// （反向在 checkInterfaces 的 storm-control 段），文案点名两者 + 机理 + 照做路径。
	if i.StormControl != nil {
		v.errf(p+".port_security", "接口 %s 同时配了端口安全白名单与风暴抑制：两者都绑该口唯一的 L2 入向分类槽"+
			"（端口安全走 macip、风暴抑制走 classify，单槽互斥），不能并存——请两者留其一："+
			"delete interfaces %s port-security 或 delete interfaces %s storm-control",
			i.Name, i.Name, i.Name)
	}
	for _, r := range c.Vrfs {
		for _, li := range r.L3Interfaces {
			if li.Interface == i.Name && li.AclIn != "" {
				v.errf(p+".port_security", "接口 %s 已作为 VRF %s 的 L3 接口绑定 ACL（acl-in %s）：同一接口只有一个 macip 绑定槽，"+
					"端口安全白名单与 L3 接口 ACL 不能并存——请两者留其一", i.Name, r.Name, li.AclIn)
			}
		}
	}
}

// checkStormKbps 风暴抑制取值域（决策 #385）：1 kbps..100000000 kbps（100 Gbps 量级）。
// 上界与物理口线速相称——再大只可能是笔误（与 learn-limit 的「上界即无意义值」同一取舍）。
func checkStormKbps(n int) bool { return n >= 1 && n <= 100000000 }

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
	// 决策 #368（收口 R142-5）：DHCP 中继与服务器的**跨交换机全局互斥**。
	// UDP/67 的本地处理归属是全局单槽资源：server 的 punt 注册（全局、每 15s 重申）与
	// relay 的 per-FIB 代理共用同一个 UDP/67 查找钩子，跨交换机并存会使 relay 域的
	// DHCP 包被 server 的注册静默接管（round150 真机定性：并存后 punt-socket TX error
	// 持续增长、relay 域包被黑洞、server 侧零租约）。按 #340 口径硬拒、不给 force 出口。
	// 同交换机双配由下方 #359 的既有检查负责——本预检只在两侧**不同交换机**时补报，避免重复。
	relays, servers := []string{}, []string{}
	for _, s := range c.VirtualSwitches {
		if s.DhcpRelayServer != "" {
			relays = append(relays, s.Name)
		}
		if s.DhcpServerPoolStart != "" {
			servers = append(servers, s.Name)
		}
	}
	crossConflict := false
	for _, r := range relays {
		for _, sv := range servers {
			if r != sv {
				crossConflict = true
			}
		}
	}
	if crossConflict {
		v.errf(fmt.Sprintf("virtual-switches[%s].dhcp_relay_server", relays[0]),
			"跨交换机不能同时配置 DHCP 中继（%s）与 DHCP 服务器（%s）：UDP/67 的本地处理归属是全局的，"+
				"服务器的注册会接管中继域的 DHCP 包（真机实测静默黑洞）——请两者留其一",
			strings.Join(relays, "、"), strings.Join(servers, "、"))
	}
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
			if s.VlanAccess != 0 || s.CrossConnect || len(s.Ports) > 0 || s.Gateway != nil || s.LearnLimit != 0 {
				v.errf(p, "type=l3 交换机不允许 L2 专属配置（vlan_access/ports/gateway/cross-connect/learn_limit）")
			}
		default:
			v.errf(p+".type", "type 必须为 l2 或 l3")
		}
		if s.VlanAccess != 0 && !checkVlan(s.VlanAccess) {
			v.errf(p+".vlan_access", "vlan %d 必须在 1-4094", s.VlanAccess)
		}
		// 决策 #337：MAC 学习条数上限（仅 L2）。VPP bridge_domain_set_learn_limit 的取值域是
		// uint32，但报文 MAC 表实际有意义的上界就是 VPP 默认值 16777216（0x1000000）——超过它
		// 等于「不设限」却写成显式值，属无效配置，直接拒绝并说明。
		if s.LearnLimit != 0 && (s.LearnLimit < 1 || s.LearnLimit > 16777216) {
			v.errf(p+".learn_limit", "学习上限 %d 超出范围：须为 1-16777216（VPP 默认 16777216 即不设限）", s.LearnLimit)
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
			// 网关上绑 ACL 直接拒绝：真机实证 VPP 26.06 不评估 BVI（网关）上的域内流量——
			// deny 规则在场时转发流量照走、计数恒 0，绑定给不出任何保护（既不拦也不计）。
			// 与其留一个静默失效的保护手段（同管理口守卫先例），不如在提交期硬拒并指向已实证
			// 生效的 L3 接口形态。模型字段保留仅为解析兼容（见规格书本条决策）。
			if s.Gateway.AclIn != "" {
				v.errf(p+".gateway.acl_in", "网关（BVI）上不支持 acl-in：VPP 26.06 不评估 BVI 上的域内流量，"+
					"既不拦截也不计数（真机实证），绑定给不出任何保护。入向过滤请改用 L3 接口形态："+
					"set virtual-switches %s l3-interface <ifname> acl-in <acl>（该形态已实测生效）", s.Name)
			}
			if s.Gateway.AclOut != "" {
				v.errf(p+".gateway.acl_out", "网关（BVI）上不支持 acl-out：VPP 26.06 不评估 BVI 上的域内流量，"+
					"既不拦截也不计数（真机实证），绑定给不出任何保护。入向过滤请改用 L3 接口形态："+
					"set virtual-switches %s l3-interface <ifname> acl-in <acl>（该形态已实测生效）", s.Name)
			}
		}
		// 决策 #335：DHCP 中继只挂在「有 BVI 网关」的交换机域上——中继源地址自动取 BVI 的
		// IPv4 网关地址，rx 域就是该网关的转发域。type=l3 交换机没有 BVI（L2 专属校验也拦下
		// gateway），配 relay 只会得到一个永远发不出去的 proxy，故在配置层直接拒绝并给出可照做的下一步。
		if s.DhcpRelayServer != "" {
			if !checkIP4(s.DhcpRelayServer) {
				v.errf(p+".dhcp_relay_server", "DHCP 服务器地址 %q 必须是 IPv4 地址", s.DhcpRelayServer)
			}
			switch {
			case s.Type == "l3":
				v.errf(p+".dhcp_relay_server", "type=l3 交换机没有 BVI 网关，DHCP 中继仅支持已配置网关的 L2 交换机")
			case !gatewayHasV4(s.Gateway):
				v.errf(p+".dhcp_relay_server", "配置 DHCP 中继前须先 set virtual-switches %s gateway ip <ip-prefix>"+
					"（中继源地址自动取 BVI 的 IPv4 网关地址）", s.Name)
			}
		}
		// 决策 #359：DHCP 服务器（用户态服务器 + 每交换机一条内置 L2 tap）。仅 L2 且已配
		// **IPv4** BVI 网关（服务器在网关域里收广播、以 BVI 地址作 server-id/下发网关与缺省 DNS）；
		// pool 是启用要件，两键须同时给出。**与 dhcp-relay 互斥**：两者争抢 UDP/67 的处理权
		// （round140 真机实测 relay 的 proxy 会夺走 punt 注册），同一交换机同时配二者必然互相踩。
		if s.DhcpServerPoolStart != "" || s.DhcpServerPoolEnd != "" || s.DhcpServerLeaseTimeSeconds != 0 ||
			s.DhcpServerDNS != "" || s.DhcpServerDomainName != "" {
			switch {
			case s.Type == "l3":
				v.errf(p+".dhcp_server_pool_start", "type=l3 交换机没有 BVI 网关，DHCP 服务器仅支持已配置网关的 L2 交换机")
			case !gatewayHasV4(s.Gateway):
				v.errf(p+".dhcp_server_pool_start", "配置 DHCP 服务器前须先 set virtual-switches %s gateway ip <ip-prefix>"+
					"（服务器以 BVI 地址作 server-id 与下发网关）", s.Name)
			}
			if (s.DhcpServerPoolStart == "") != (s.DhcpServerPoolEnd == "") {
				v.errf(p+".dhcp_server_pool_start", "DHCP 服务器租约池的起始与结束地址必须同时给出"+
					"（set virtual-switches %s dhcp-server pool <start> <end>）", s.Name)
			}
			if s.DhcpServerPoolStart != "" && s.DhcpServerPoolEnd != "" {
				v.checkDHCPServerPool(p, s)
			}
			if s.DhcpServerLeaseTimeSeconds != 0 &&
				(s.DhcpServerLeaseTimeSeconds < MinDHCPServerLeaseSeconds || s.DhcpServerLeaseTimeSeconds > MaxDHCPServerLeaseSeconds) {
				v.errf(p+".dhcp_server_lease_time_seconds", "租约时长 %d 秒超出范围：须为 %d-%d 秒（缺省 %d）",
					s.DhcpServerLeaseTimeSeconds, MinDHCPServerLeaseSeconds, MaxDHCPServerLeaseSeconds,
					DefaultDHCPServerLeaseSeconds)
			}
			if s.DhcpServerDNS != "" && !checkIP4(s.DhcpServerDNS) {
				v.errf(p+".dhcp_server_dns", "下发的 DNS 地址 %q 必须是 IPv4 地址", s.DhcpServerDNS)
			}
			if s.DhcpServerDomainName != "" {
				if len(s.DhcpServerDomainName) > 255 || strings.ContainsAny(s.DhcpServerDomainName, " \t") {
					v.errf(p+".dhcp_server_domain_name", "下发的域名 %q 非法：不超过 255 字节且不含空白字符", s.DhcpServerDomainName)
				}
			}
			if s.DhcpServerPoolStart != "" && s.DhcpRelayServer != "" {
				v.errf(p+".dhcp_server_pool_start", "同一交换机不能同时配置 DHCP 服务器与 DHCP 中继"+
					"（两者争抢 UDP/67 的处理权，真机实测中继的代理会夺走注册）：请删除 dhcp-relay 或 dhcp-server 之一")
			}
		}
		// 决策 #345：数据面 DNS 代理的**按域上游**——逐条须为合法 IP（v4/v6），空串拒绝；
		// 条数不设上限（多上游即多备份，按声明序尝试）。L3 交换机的转发域是其 l3-interface，
		// 故不要求 BVI 网关（与 dhcp-relay 不同）。
		for i, srv := range s.DNSProxyServers {
			if !checkIP(srv) {
				v.errf(fmt.Sprintf("%s.dns_proxy_servers[%d]", p, i),
					"DNS 代理上游 %q 必须是有效 IP（IPv4/IPv6）", srv)
			}
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
			// 决策 #340（round119 补验）：交换机端口级 ACL 同样在提交期硬拒——真机实证
			// VPP 26.06 不评估 L2 路径（成员端口）上的 ACL，既不拦截也不计数；与网关（BVI）
			// 同机制。模型字段保留仅为解析兼容（见规格书本条决策），绑定一律拒绝。
			if pt.AclIn != "" {
				v.errf(pp+".acl_in", "交换机端口上不支持 acl-in：VPP 26.06 不评估 L2 路径（成员端口）上的 ACL，"+
					"既不拦截也不计数（真机实证），绑定给不出任何保护。入向过滤请改用 L3 接口形态："+
					"set virtual-switches %s l3-interface <ifname> acl-in <acl>（该形态已实测生效）", s.Name)
			}
			if pt.AclOut != "" {
				v.errf(pp+".acl_out", "交换机端口上不支持 acl-out：VPP 26.06 不评估 L2 路径（成员端口）上的 ACL，"+
					"既不拦截也不计数（真机实证），绑定给不出任何保护。入向过滤请改用 L3 接口形态："+
					"set virtual-switches %s l3-interface <ifname> acl-in <acl>（该形态已实测生效）", s.Name)
			}
		}
	}
}

// checkDHCPServerPool 校验租约池语义（决策 #359）：与 BVI 的 IPv4 地址**同子网**（按 BVI
// 前缀长度）、start ≤ end、不含 BVI 地址与子网网络/广播地址、地址数 ≤ 4096（超限说明上限）。
// 纯边界计算与数据面共用 model.DHCPServerPoolRange（单一事实源）。
func (v *validator) checkDHCPServerPool(p string, s VirtualSwitch) {
	start, end := s.DhcpServerPoolStart, s.DhcpServerPoolEnd
	lo, hi, ok := DHCPServerPoolRange(start, end)
	if !ok {
		v.errf(p+".dhcp_server_pool_start", "租约池 %q-%q 非法：两端都必须是合法 IPv4 地址且 start ≤ end", start, end)
		return
	}
	if size := int(hi-lo) + 1; size > MaxDHCPServerPoolSize {
		v.errf(p+".dhcp_server_pool_start", "租约池 %q-%q 共 %d 个地址，超出上限：最多 %d 个地址（含两端）",
			start, end, size, MaxDHCPServerPoolSize)
	}
	bvi, bviNet, hasBVI := s.GatewayIPv4()
	if !hasBVI {
		return // 网关前置缺失已由调用方报错，这里不重复
	}
	// 同子网（按 BVI 前缀长度）：两端都必须落在 BVI 地址所属网段内——池是连续区间，
	// 两端都在网段内即整段都在（网段是连续范围）。
	if !bviNet.Contains(Uint32ToIPv4(lo)) || !bviNet.Contains(Uint32ToIPv4(hi)) {
		v.errf(p+".dhcp_server_pool_start", "租约池 %q-%q 与 BVI 网关地址 %s 不在同一子网（%s）：池须与网关同子网",
			start, end, bvi.String(), bviNet.String())
	}
	// 池不含 BVI 地址与子网的网络/广播地址（服务器自身不能把网关/网络地址租出去）。
	netAddr := bviNet.IP.To4()
	if netAddr == nil {
		return
	}
	netU := uint32(netAddr[0])<<24 | uint32(netAddr[1])<<16 | uint32(netAddr[2])<<8 | uint32(netAddr[3])
	ones, bits := bviNet.Mask.Size()
	if bits != 32 || ones < 0 {
		return
	}
	bcastU := netU | (^uint32(0) >> uint(ones))
	bviU32, _ := IPv4ToUint32(bvi.String())
	for _, excl := range []struct {
		v    uint32
		name string
	}{{bviU32, "BVI 网关地址"}, {netU, "子网网络地址"}, {bcastU, "子网广播地址"}} {
		if excl.v >= lo && excl.v <= hi {
			v.errf(p+".dhcp_server_pool_start", "租约池 %q-%q 包含 %s %s：请缩小池范围避开它",
				start, end, excl.name, Uint32ToIPv4(excl.v).String())
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
				v.errf(lp, "L3 接口 %q 不存在或不是物理口/bond/vlan 子接口/已声明的 vNIC", li.Interface)
			}
			for i, a := range li.Addresses {
				if !checkCIDR(a) {
					v.errf(fmt.Sprintf("%s.addresses[%d]", lp, i), "地址 %q 必须是 ip-prefix（CIDR）", a)
				}
			}
			v.checkACLRef(lp+".acl_in", li.AclIn)
			// 端口安全白名单与 L3 接口 ACL 的 macip 绑定槽互斥——**反向**（给 l3-interface
			// 绑 ACL 时查白名单；正向在 checkPortSecurity）。同一接口只能有一个 macip 绑定：
			// 白名单与 L3 ACL 的伴随 macip（放行非 IP 帧）争同一槽位，并存必然互相顶掉。
			if li.AclIn != "" && v.portSecIfaces[li.Interface] {
				v.errf(lp+".acl_in", "接口 %s 已配置端口安全白名单：同一接口只有一个 macip 绑定槽，"+
					"L3 接口 ACL 与白名单不能并存——请两者留其一", li.Interface)
			}
		}
		for _, rt := range r.Routes {
			rp := fmt.Sprintf("%s.routes[%s]", p, rt.Prefix)
			if rt.Prefix == "" || !checkCIDR(rt.Prefix) {
				v.errf(rp+".prefix", "路由前缀 %q 必须是 ip-prefix（CIDR）", rt.Prefix)
			}
			v.checkRouteNextHops(rp+".next_hop", rt.NextHop)
			v.checkRouteNextHopFamily(rp+".next_hop", rt.Prefix, rt.NextHop)
			if rt.Distance < 0 || rt.Distance > 255 {
				v.errf(rp+".distance", "distance %d 必须在 0-255", rt.Distance)
			}
		}
	}
}

// maxRouteNextHops 静态路由多下一跳的数量上限（决策 #381：防误配；单值不受影响）。
const maxRouteNextHops = 8

// checkRouteNextHops 校验静态路由的下一跳（决策 #381：逗号分隔的多下一跳＝ECMP）。
//
// 单值写法与语义完全不变——单元素时沿用既有文案（逐字节等价），只做有效 IP 校验。
// 多值时按 `,` 切分后**逐元素**校验：非空、有效 IP、去重、同族（不得混 IPv4/IPv6）、
// 数量上限 maxRouteNextHops。路径仍为 rp+".next_hop"（错误定位到该路由字段）。
func (v *validator) checkRouteNextHops(path, spec string) {
	hops := strings.Split(spec, ",")
	if len(hops) == 1 {
		if !checkIP(hops[0]) {
			v.errf(path, "下一跳 %q 必须是有效 ip", spec)
		}
		return
	}
	if len(hops) > maxRouteNextHops {
		v.errf(path, "多下一跳最多 %d 个，实际 %d 个", maxRouteNextHops, len(hops))
	}
	seen := make(map[string]bool, len(hops))
	family := ""
	for i, h := range hops {
		if h == "" {
			v.errf(path, "第 %d 个下一跳为空", i+1)
			continue
		}
		if !checkIP(h) {
			v.errf(path, "第 %d 个下一跳 %q 不是有效 ip", i+1, h)
			continue
		}
		if f := prefixFamily(h); family == "" {
			family = f
		} else if f != family {
			v.errf(path, "多下一跳不得混用 IPv4/IPv6（第 %d 个 %q 与前一跳不同族）", i+1, h)
		}
		if seen[h] {
			v.errf(path, "下一跳重复 %q", h)
		}
		seen[h] = true
	}
}

// checkRouteNextHopFamily 校验静态路由的**前缀与每个下一跳同族**（决策 #393）。
//
// 此前只比「多个下一跳彼此同族」（checkRouteNextHops），前缀与下一跳的族不查——
// `static-routes 10.0.0.0/24 next-hop 2001:db8::1`（v4 前缀 + v6 下一跳）提交成功、
// 读视图正常，而 VPP FIB 里该前缀装成 `dpo-drop`（静默黑洞；round171 §1.5 真机复现），
// 与项目史上最严重的「命令成功、数据面全丢」家族同类。此处补提交期拒绝，文案点名前缀与
// **首个**异族跳。任一侧无法判族（any/空/非法 IP）时不判——非法值由 prefix/ip 校验单独
// 报错，不重复计数（与 #352 ACL 混族校验同口径）。
func (v *validator) checkRouteNextHopFamily(path, prefix, spec string) {
	pf := prefixFamily(prefix)
	if pf == "" {
		return
	}
	for _, h := range strings.Split(spec, ",") {
		nf := prefixFamily(h)
		if nf != "" && nf != pf {
			v.errf(path, "前缀 %q 与下一跳 %q 不同族（IPv4/IPv6 混用；数据面会装成 dpo-drop 静默黑洞）", prefix, h)
			return
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
			// 决策 #352：一条规则只匹配单一地址族——两侧都是显式前缀时必须同族
			// （VPP 的 ACL 规则按单族下发；any/空一侧由编排层跟随显式侧家族）。
			if srcFam, dstFam := prefixFamily(r.Source), prefixFamily(r.Destination); srcFam != "" && dstFam != "" && srcFam != dstFam {
				v.errf(rp+".source", "source/destination 地址族不一致（%s 与 %s）：一条规则仅匹配单族，双族需两条规则",
					r.Source, r.Destination)
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
		// 决策 #352：两端都严格 v4——下发层 ParseIP4Address 只收 v4，v6 在这里拒
		// （而不是 commit 期的 invalid IP4 address）。
		lo, hi, ok := strings.Cut(pool.AddressRange, " to ")
		if !ok || !checkIP4(strings.TrimSpace(lo)) || !checkIP4(strings.TrimSpace(hi)) {
			v.errf(pp+".address_range", "地址范围 %q 必须为 \"<IPv4> to <IPv4>\"（NAT44 仅支持 IPv4）", pool.AddressRange)
		}
	}
	dupCheck(v, n.Rules, "nat.rules", func(r NatRule) string { return strconv.Itoa(r.Seq) }, "NAT 规则")
	insideVS, outsideVRF := "", ""
	for _, r := range n.Rules {
		rp := fmt.Sprintf("nat.rules[%d]", r.Seq)
		// 决策 #352：match-source 严格 v4 CIDR（NAT44 下发层只收 v4）。
		if !checkCIDR4(r.MatchSource) {
			v.errf(rp+".match_source", "匹配源 %q 必须为 IPv4 CIDR（NAT44 仅支持 IPv4）", r.MatchSource)
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
	// 决策 #352：静态映射两端严格 v4（NAT44 下发层只收 v4，v6 在校验期拒绝）。
	for i, st := range n.Static {
		if !checkIP4(st.InsideIP) {
			v.errf(fmt.Sprintf("nat.static[%d].inside_ip", i), "内部地址 %q 必须为 IPv4 地址（NAT44 仅支持 IPv4）", st.InsideIP)
		}
		if !checkIP4(st.OutsideIP) {
			v.errf(fmt.Sprintf("nat.static[%d].outside_ip", i), "外部地址 %q 必须为 IPv4 地址（NAT44 仅支持 IPv4）", st.OutsideIP)
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

// checkVxlanTunnels 校验 VXLAN 隧道（FR-NET-019，决策 #383）。
//
// 口径（与数据面实现同源）：name 走既有命名规则（且 `nfvis-vxlan:<名>` 须装进数据面接口
// tag 的 63 字节上限）；vni 1..16777215；local/remote 为**有效 IPv4**（v1 不做 IPv6 下垫层）
// 且不相等（VPP 的 encap 查找不允许自环）；dst-port 0/缺省视为 4789、否则 1..65535；
// name、vni、(vni,local,remote) 三元组重复拒绝；virtual-switch 给了就必须**存在且为 L2**
// （L3 交换机没有 BD，隧道口无处可入）。
//
// 「被隧道引用的交换机不得删除」由本检查的引用缺失分支承担（与既有「先解引用、后删被引用」
// 口径一致）：交换机被删后仍留在配置里的隧道会命中 virtual-switch 不存在，报错并给出照做路径。
func (v *validator) checkVxlanTunnels(c Config) {
	dupCheck(v, c.VxlanTunnels, "vxlan-tunnels", func(t VxlanTunnel) string { return t.Name }, "VXLAN 隧道")
	seenVni := map[int]string{}
	seenTuple := map[string]string{}
	for _, t := range c.VxlanTunnels {
		p := fmt.Sprintf("vxlan-tunnels[%s]", t.Name)
		if !v.checkName(p, t.Name, "VXLAN 隧道") {
			continue
		}
		if t.Vni < VxlanMinVni || t.Vni > VxlanMaxVni {
			v.errf(p+".vni", "VNI %d 超出范围：须为 %d-%d", t.Vni, VxlanMinVni, VxlanMaxVni)
		} else if prev, dup := seenVni[t.Vni]; dup {
			v.errf(p+".vni", "VNI %d 已被隧道 %q 使用：同一 VNI 只能有一条隧道", t.Vni, prev)
		} else {
			seenVni[t.Vni] = t.Name
		}
		if !checkIP4(t.Local) {
			v.errf(p+".local", "本地下垫地址 %q 必须是 IPv4 地址（当前版本不支持 IPv6 下垫层）", t.Local)
		}
		if !checkIP4(t.Remote) {
			v.errf(p+".remote", "远端下垫地址 %q 必须是 IPv4 单播地址（当前版本不支持 IPv6 下垫层与组播 remote）", t.Remote)
		}
		if t.Local == t.Remote && t.Local != "" {
			v.errf(p+".remote", "本地下垫地址与远端下垫地址不能相同（%s）", t.Local)
		}
		if t.DstPort != 0 && (t.DstPort < 1 || t.DstPort > 65535) {
			v.errf(p+".dst_port", "目的端口 %d 超出范围：须为 1-65535（缺省 %d）", t.DstPort, VxlanDefaultDstPort)
		}
		// 数据面身份是接口 tag（"nfvis-vxlan:<名>"，sw_interface_tag_add_del 的 string[64] 含
		// NUL 至多 63 字节）——超长会被**静默截断**、读回对不上（运行态识别失灵），
		// 故在提交期按数据面上限拒绝（与运行态判存量的实现同源）。
		if tag := t.DataPlaneTag(); len(tag) > VxlanTagMaxLen {
			v.errf(p+".name", "隧道名过长：数据面接口标记 %q 须不超过 %d 字节（当前 %d 字节，名字最长 %d 字节）",
				tag, VxlanTagMaxLen, len(tag), VxlanTagMaxLen-len(VxlanTagPrefix))
		}
		if t.Local != "" && t.Remote != "" {
			key := fmt.Sprintf("%d|%s|%s", t.Vni, t.Local, t.Remote)
			if prev, dup := seenTuple[key]; dup {
				v.errf(p, "与隧道 %q 的 (vni, local, remote) 相同：同一元组只能声明一条隧道", prev)
			} else {
				seenTuple[key] = t.Name
			}
		}
		if t.VirtualSwitch != "" {
			switch {
			case !v.vsNames[t.VirtualSwitch]:
				// 交换机被删而隧道仍引用它——「被隧道引用的交换机不得删除」的落点：
				// 拒绝提交并给出照做路径（先解引用、再删被引用）。
				v.errf(p+".virtual_switch", "虚拟交换机 %q 不存在：若刚删除该交换机，"+
					"它仍被本 VXLAN 隧道引用——请先解引用（delete vxlan tunnels %s virtual-switch），再删交换机",
					t.VirtualSwitch, t.Name)
			case !v.l2vs[t.VirtualSwitch]:
				v.errf(p+".virtual_switch", "虚拟交换机 %q 是 L3 交换机（没有 bridge-domain）：隧道口只能加入 L2 交换机",
					t.VirtualSwitch)
			}
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
	// 决策 #345：数据面 DNS 代理的**全局上游**——逐条须为合法 IP（v4/v6），空串拒绝；
	// 条数不设上限（与既有 system.dns_servers 同风格）。
	for i, srv := range vp.DNSProxyServers {
		if !checkIP(srv) {
			v.errf(fmt.Sprintf("vpp.dns_proxy_servers[%d]", i), "DNS 代理上游 %q 必须是有效 IP（IPv4/IPv6）", srv)
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

// NormalizeBDF 校验 PCI 地址（BDF）语法并归一为 `dddd:bb:ss.f`（小写、定宽）——
// 通用 PCI 直通（FR-CMP-023）配置值的语法与归一单源：
//   - 接受 `[domain:]bus:slot.function`（domain 可省略，缺省 0000），各段十六进制、
//     大小写不敏感，可带 0x/0X 前缀（与 compute.ParsePCI 的接受形态一致）；
//   - 长度/范围约束：domain ≤ 4 位（≤0xffff）、bus ≤ 2 位（≤0xff）、slot ≤ 2 位且 ≤ 0x1f、
//     function 1 位且 ≤ 7（真实 PCI 的位宽，8-15 不是合法 function）；
//   - 归一结果恒为 `%04x:%02x:%02x.%x`——sysfs 目录名（/sys/bus/pci/devices/<BDF>）
//     与 libvirt 地址都以此为准，便于去重与存在性检查。
//
// 与 compute.ParsePCI 的关系：本函数是**纯语法**校验（model 不 import compute，保持依赖方向），
// 接受形态与 ParsePCI 一致并**额外接受**「省略 domain」的写法；同时按真实 PCI 位宽收紧了部分
// 长度/范围（如 function 0-7）——超界写法在此即拒，不会推迟到组装层。归一后的字符串可被
// ParsePCI 原样解析（ParsePCI 的两个语义未变）。设备的**存在性**检查不在 model 层——属计算
// 编排层（apply/define 前查 /sys/bus/pci/devices/<归一BDF>）。
func NormalizeBDF(bdf string) (string, error) {
	parts := strings.Split(strings.TrimSpace(bdf), ":")
	var domainPart, busPart, slotFnPart string
	switch len(parts) {
	case 2: // 省略 domain：bus:slot.function
		busPart, slotFnPart = parts[0], parts[1]
	case 3: // domain:bus:slot.function
		domainPart, busPart, slotFnPart = parts[0], parts[1], parts[2]
	default:
		return "", fmt.Errorf("PCI 地址 %q 格式非法（应为 [domain:]bus:slot.function，如 0000:03:00.0）", bdf)
	}
	slotFn := strings.Split(slotFnPart, ".")
	if len(slotFn) != 2 {
		return "", fmt.Errorf("PCI 地址 %q 的 slot.function 非法（应为 slot.function，如 00.0）", bdf)
	}
	var domain uint64
	if domainPart != "" {
		v, err := parseBDFHex(domainPart, 4)
		if err != nil {
			return "", fmt.Errorf("PCI 地址 %q 的 domain 非法（%v）", bdf, err)
		}
		domain = v
	}
	bus, err := parseBDFHex(busPart, 2)
	if err != nil {
		return "", fmt.Errorf("PCI 地址 %q 的 bus 非法（%v）", bdf, err)
	}
	slot, err := parseBDFHex(slotFn[0], 2)
	if err != nil {
		return "", fmt.Errorf("PCI 地址 %q 的 slot 非法（%v）", bdf, err)
	}
	if slot > 0x1f {
		return "", fmt.Errorf("PCI 地址 %q 的 slot 非法（须在 00-1f）", bdf)
	}
	fn, err := parseBDFHex(slotFn[1], 1)
	if err != nil {
		return "", fmt.Errorf("PCI 地址 %q 的 function 非法（%v）", bdf, err)
	}
	if fn > 7 {
		return "", fmt.Errorf("PCI 地址 %q 的 function 非法（须在 0-7）", bdf)
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", domain, bus, slot, fn), nil
}

// parseBDFHex 解析 BDF 的一段十六进制（可带 0x/0X 前缀）：非空、≤maxDigits 位。
func parseBDFHex(s string, maxDigits int) (uint64, error) {
	raw := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if raw == "" {
		return 0, fmt.Errorf("不能为空")
	}
	if len(raw) > maxDigits {
		return 0, fmt.Errorf("应为 ≤%d 位十六进制", maxDigits)
	}
	v, err := strconv.ParseUint(raw, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("非十六进制")
	}
	return v, nil
}

func (v *validator) checkVMFunctions(c Config) {
	dupCheck(v, c.VirtualMachineFunctions, "virtual-machine-functions", func(m VMFunction) string { return m.Name }, "VM")
	// pciOwner 归一 BDF → 首个声明者 VM 名（FR-CMP-023 跨 VM 冲突：同一 PCI 设备不能
	// 同时直通给两台 VM——否则第二台域定义/启动必失败）。
	pciOwner := map[string]string{}
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
		// FR-CMP-023：通用 PCI 直通设备（BDF）——语法、同 VM 去重、跨 VM 冲突。
		// 归一后比较（`03:00.0` 与 `0000:03:00.0` 是同一设备）；存在性属编排层。
		seenPCI := map[string]bool{}
		for i, bdf := range m.PCIDevices {
			dp := fmt.Sprintf("%s.pci_devices[%d]", p, i)
			norm, err := NormalizeBDF(bdf)
			if err != nil {
				v.errf(dp, "%v", err)
				continue
			}
			if seenPCI[norm] {
				v.errf(dp, "PCI 设备 %s 在本 VM 内重复声明", norm)
				continue
			}
			seenPCI[norm] = true
			if owner, taken := pciOwner[norm]; taken {
				v.errf(dp, "PCI 设备 %s 已声明给 VM %s：同一设备不能同时直通给两台 VM（%s 与 %s）", norm, owner, owner, m.Name)
				continue
			}
			pciOwner[norm] = m.Name
		}
		dupCheck(v, m.Interfaces, p+".interfaces", func(n VnfInterface) string { return n.Name }, "vNIC")
		for _, nic := range m.Interfaces {
			np := fmt.Sprintf("%s.interfaces[%s]", p, nic.Name)
			if !v.checkName(np, nic.Name, "vNIC") {
				continue
			}
			switch nic.Type {
			case "vhost-user":
				// FR-CFG-011①：vhost-user 要求大页内存——因为 VPP 的 vhost-user 是**共享内存**
				// 形态（guest RAM 与 VPP 共享，必须大页）。**内核数据面下该约束不成立**：
				// 同名类型落成 virtio 网卡 + 宿主 tap + vhost-net，没有共享内存对端，普通内存
				// 即可（真机走查暴露：内核数据面下想用 backing normal 建 VNF 会被这条挡住，
				// 而它本不需要大页 ⇒ 被迫声明资源池 + 重启）。
				if m.Memory.Backing == "normal" && c.DataPlaneMode() != DataPlaneKernel {
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

// checkPortRoleExclusivity 强制一个网口在数据面角色中最多出现一次。
//
// 数据面角色：bond 成员、虚拟交换机端口、L3 接口（l3-interface）、端口镜像的源/分析口。
// 四者在 VPP 里对应**互斥**的接管方式——成员口入向被 bond-input 吃掉、交换机端口进
// bridge-domain、L3 接口承载地址/路由、镜像口被 SPAN 占用。同一物理口被两条声明绑定时
// VPP 只兑现其中一种，配置侧却全部接受：实测 ens192 同时作为 bond0 成员与 vs-l3 的
// l3-interface 时，该口入向被 bond-input 吃光、不进 bridge-domain，BD 转发整体失效
// （learned=0），连锁到 VNF guest 拿不到 DHCP，而产品全程零报错。
// 用户手册 §8.9 早已写明「bond 成员口须未被虚拟交换机引用」——本检查把该约束落到 commit。
//
// 判据是**接口名精确匹配**（与 checkManagementIsolation 同口径）：VLAN 子接口
// （如 ens2f0.100）与其父口是两个接口，各自独立计数。未声明的引用由各角色自身的
// 引用校验报错，此处不重复。
func (v *validator) checkPortRoleExclusivity(c Config) {
	type roleUse struct{ path, role string }
	seen := map[string]roleUse{}
	// claim 登记一次角色占用；exists 为假表示该引用本身不是合法接口（由别的检查报错），
	// 不参与角色冲突判定，避免在错误清单里叠噪声。
	claim := func(name, path, role string, exists bool) {
		if name == "" || !exists {
			return
		}
		prev, taken := seen[name]
		if !taken {
			seen[name] = roleUse{path: path, role: role}
			return
		}
		v.errf(path, "接口 %q 的角色冲突：已作为%s（%s），不能再作为%s——"+
			"同一网口在 bond 成员/交换机端口/L3 接口/镜像源或分析口 中只能出现一次",
			name, prev.role, prev.path, role)
	}

	for _, b := range c.Bonds {
		for i, m := range b.Members {
			claim(m, fmt.Sprintf("bonds[%s].members[%d]", b.Name, i),
				fmt.Sprintf("bond %s 的成员口", b.Name), v.ifaceNames[m])
		}
	}
	for _, s := range c.VirtualSwitches {
		for _, pt := range s.Ports {
			claim(pt.Interface, fmt.Sprintf("virtual-switches[%s].ports[%d].interface", s.Name, pt.Seq),
				fmt.Sprintf("虚拟交换机 %s 的端口", s.Name), v.anyIface(pt.Interface))
		}
	}
	for _, r := range c.Vrfs {
		for _, li := range r.L3Interfaces {
			claim(li.Interface, fmt.Sprintf("vrfs[%s].l3_interfaces[%s].interface", r.Name, li.Interface),
				fmt.Sprintf("VRF %s 的 L3 接口", r.Name), v.l3IfaceExists(li.Interface))
		}
	}
	for _, pm := range c.PortMirroring {
		claim(pm.Source.Interface, fmt.Sprintf("port-mirroring[%s].source.interface", pm.Name),
			fmt.Sprintf("镜像会话 %s 的源端口", pm.Name), v.anyIface(pm.Source.Interface))
		claim(pm.Analyzer, fmt.Sprintf("port-mirroring[%s].analyzer", pm.Name),
			fmt.Sprintf("镜像会话 %s 的分析端口", pm.Name), v.ifaceNames[pm.Analyzer])
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
