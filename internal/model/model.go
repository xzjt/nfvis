// Package model 定义 NFViS 的单一配置数据模型。
//
// 结构与 docs/NFViS-openapi.yaml 的 schemas 一一对应（契约先行），
// JSON 标签与 ConfigDocument 保持一致；committed 与 candidate 配置共用本模型。
// 语义校验见 validate.go，配置 diff 见 diff.go。
package model

import (
	"encoding/json"
	"net"
	"strings"
)

// Config 整份配置文档（OpenAPI ConfigDocument）。
// 单例子结构用指针以区分「未配置」与零值。
type Config struct {
	System                  *SystemConfig       `json:"system,omitempty"`
	Interfaces              []InterfaceConfig   `json:"interfaces,omitempty"`
	Bonds                   []Bond              `json:"bonds,omitempty"`
	VirtualSwitches         []VirtualSwitch     `json:"virtual_switches,omitempty"`
	Vrfs                    []Vrf               `json:"vrfs,omitempty"`
	Acls                    []Acl               `json:"acls,omitempty"`
	Nat                     *NatConfig          `json:"nat,omitempty"`
	PortMirroring           []PortMirroring     `json:"port_mirroring,omitempty"`
	QosPolicies             []QosPolicy         `json:"qos_policies,omitempty"`
	VxlanTunnels            []VxlanTunnel       `json:"vxlan_tunnels,omitempty"`
	ResourcePools           *ResourcePool       `json:"resource_pools,omitempty"`
	Vpp                     *VppConfig          `json:"vpp,omitempty"`
	Protocols               *ProtocolsConfig    `json:"protocols,omitempty"`
	VirtualMachineFunctions []VMFunction        `json:"virtual_machine_functions,omitempty"`
	ContainerFunctions      []ContainerFunction `json:"container_functions,omitempty"`

	// Annotations 配置节点注释（FR-CFG-007 annotate；键=语句路径，决策 #27）
	Annotations map[string]string `json:"annotations,omitempty"`
}

// 数据面实现的选择（system.dataplane，v3 决策 #404）。
//
// 缺省（未配置）= DataPlaneVPP：v3 之前的产品只有 VPP 一种数据面，未配置必须落到既有行为，
// 不能让既有安装「升级后换个数据面跑」。变更需重启 nfvisd 生效（整机单数据面）。
const (
	DataPlaneVPP    = "vpp"    // VPP（DPDK 接管物理口；vhost-user/memif 接入 VNF/容器）
	DataPlaneKernel = "kernel" // Linux 内核网络（bridge/VRF/nftables/tc；virtio+vhost-net+tap 接入 VNF）
)

// DataPlaneMode 生效的数据面实现：配置值，缺省/未配置回落 vpp（单一事实源）。
// 非法取值在提交期由 Validate 拒绝，这里只做兜底（不把非法值当 kernel 用）。
func (c *Config) DataPlaneMode() string {
	if c == nil || c.System == nil {
		return DataPlaneVPP
	}
	if c.System.DataPlane == DataPlaneKernel {
		return DataPlaneKernel
	}
	return DataPlaneVPP
}

// SystemConfig 对应 OpenAPI SystemConfig。
type SystemConfig struct {
	Kernel *KernelConfig `json:"kernel,omitempty"` // 内核启动基线（FR-SYS-014）
	// DataPlane 数据面实现（FR-NET-001，v3 决策 #404）：vpp|kernel，缺省 vpp。
	// 决定 nfvisd 装配哪一套网络编排实现，以及 VPP 专有命令是否可用；变更需重启 nfvisd 生效。
	DataPlane          string            `json:"dataplane,omitempty"`
	Hostname           string            `json:"hostname,omitempty"`
	Timezone           string            `json:"timezone,omitempty"`
	Ntp                []NtpServer       `json:"ntp,omitempty"`
	DNSServers         []string          `json:"dns_servers,omitempty"`
	Management         *MgmtConfig       `json:"management,omitempty"`
	Login              *SystemLogin      `json:"login,omitempty"` // 本地用户与 class（FR-SEC-002/003，附录 A #25）
	Syslog             *SyslogConfig     `json:"syslog,omitempty"`
	API                *APIConfig        `json:"api,omitempty"`
	IdleTimeoutMinutes int               `json:"idle_timeout_minutes,omitempty"`
	Health             *HealthThresholds `json:"health,omitempty"`   // 硬件健康告警阈值（FR-SYS-012）
	Metrics            *MetricsConfig    `json:"metrics,omitempty"`  // 可观测性配置（决策 #356）
	Firewall           *FirewallConfig   `json:"firewall,omitempty"` // 管理面主机防火墙（决策 #388）
}

// HealthThresholds 硬件健康告警阈值（0 = 未设置该阈值，不产生告警）。
type HealthThresholds struct {
	CPUTempCelsius  int `json:"cpu_temp_celsius,omitempty"`
	DiskTempCelsius int `json:"disk_temp_celsius,omitempty"`
	DiskUsedPercent int `json:"disk_used_percent,omitempty"`
}

// MetricsConfig 可观测性配置（决策 #356）。
type MetricsConfig struct {
	History *MetricsHistoryConfig `json:"history,omitempty"`
}

// MetricsHistoryConfig 历史时序存储（决策 #356）。字段缺省 = 用默认值
// （默认值**不在配置里写常数**；`delete system metrics history <字段>` 即回落默认）。
type MetricsHistoryConfig struct {
	IntervalSeconds int `json:"interval_seconds,omitempty"` // 采样间隔（秒）
	RetentionDays   int `json:"retention_days,omitempty"`   // 保留天数
}

// 历史时序存储的默认值与界（决策 #356）。**单一事实源**——其它任何处不得重定义；
// 缺省/越界一律回落默认（生效值由下面的访问器给出）。
const (
	MetricsIntervalDefaultSeconds = 60   // 采样间隔默认（秒）
	MetricsIntervalMinSeconds     = 10   // 采样间隔下界
	MetricsIntervalMaxSeconds     = 3600 // 采样间隔上界
	MetricsRetentionDaysDefault   = 7    // 保留天数默认
	MetricsRetentionDaysMin       = 1    // 保留天数下界
	MetricsRetentionDaysMax       = 365  // 保留天数上界
)

// MetricsHistoryIntervalSeconds 生效的采样间隔（秒）：配置值，缺省/越界回落默认。
func (c *Config) MetricsHistoryIntervalSeconds() int {
	if c == nil || c.System == nil || c.System.Metrics == nil || c.System.Metrics.History == nil {
		return MetricsIntervalDefaultSeconds
	}
	n := c.System.Metrics.History.IntervalSeconds
	if n < MetricsIntervalMinSeconds || n > MetricsIntervalMaxSeconds {
		return MetricsIntervalDefaultSeconds
	}
	return n
}

// MetricsHistoryRetentionDays 生效的保留天数：配置值，缺省/越界回落默认。
func (c *Config) MetricsHistoryRetentionDays() int {
	if c == nil || c.System == nil || c.System.Metrics == nil || c.System.Metrics.History == nil {
		return MetricsRetentionDaysDefault
	}
	n := c.System.Metrics.History.RetentionDays
	if n < MetricsRetentionDaysMin || n > MetricsRetentionDaysMax {
		return MetricsRetentionDaysDefault
	}
	return n
}

// SystemLogin 本地 AAA 配置：登录横幅/用户/class/口令策略，声明式存于配置文档
// （可 compare/rollback）；口令仅存加盐哈希，明文永不回显（附录 A #25）。
type SystemLogin struct {
	// Banner 登录横幅（决策 #303）：显示在 Web 登录页与 CLI 登录提示之前——
	// 未认证即可见，不要存放敏感信息。单行文本、最长 512 字节（校验见 validate.go）。
	Banner         string            `json:"banner,omitempty"`
	Users          []LoginUserConfig `json:"users,omitempty"`
	Classes        []ClassDef        `json:"classes,omitempty"`
	PasswordPolicy *PasswordPolicy   `json:"password_policy,omitempty"`
}

type LoginUserConfig struct {
	Name         string `json:"name"`
	PasswordHash string `json:"password_hash,omitempty"` // pbkdf2$sha256$<iter>$<b64salt>$<b64hash>
	Class        string `json:"class,omitempty"`         // 缺省 read-only
}

// ClassDef 自定义 class：命令树路径前缀的 allow/deny（deny 优先）；预置类
// super-user/operator/read-only 由 schema.Class 定义，不经此表。
type ClassDef struct {
	Name  string   `json:"name"`
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type PasswordPolicy struct {
	MinLength        int  `json:"min_length,omitempty"`        // 默认 8
	Complexity       bool `json:"complexity,omitempty"`        // ≥3/4 类字符
	ExpireDays       int  `json:"expire_days,omitempty"`       // 0 = 不过期
	LockoutThreshold int  `json:"lockout_threshold,omitempty"` // 默认 5
	LockoutMinutes   int  `json:"lockout_minutes,omitempty"`   // 默认 10
}

type NtpServer struct {
	Server string `json:"server"`
	Prefer bool   `json:"prefer,omitempty"`
}

// MgmtConfig 管理口（FR-SYS-001；变更受 FR-CFG-012 自锁保护）。
//
// Interface 为管理网卡名——管理面与业务面隔离的**锚点**（FR-NET-002/FR-SEC-001）：
// 指定后数据面（vpp.dpdk.dev / bond 成员 / 交换机端口 / L3 接口）不得引用它。
type MgmtConfig struct {
	Interface string `json:"interface,omitempty"` // 管理网卡名（内核驱动）
	Address   string `json:"address,omitempty"`   // ip-prefix，IPv4/IPv6
	Gateway   string `json:"gateway,omitempty"`   // ip
}

type SyslogConfig struct {
	RemoteHost    string `json:"remote_host,omitempty"`
	RemotePort    int    `json:"remote_port,omitempty"`
	Facility      string `json:"facility,omitempty"` // RFC 5424 facility 名（缺省 user，FR-SYS-004）
	Severity      string `json:"severity,omitempty"` // 远程转发最低级别（缺省 info，FR-SYS-004）
	Level         string `json:"level,omitempty"`    // debug|info|warn|error（本地/守护进程级别，FR-OPS-030）
	RetentionDays int    `json:"retention_days,omitempty"`
	MaxSizeMB     int    `json:"max_size_mb,omitempty"`
}

type APIConfig struct {
	Port            int    `json:"port,omitempty"`
	TokenTTLMinutes int    `json:"token_ttl_minutes,omitempty"`
	MaxSessions     int    `json:"max_sessions,omitempty"`
	CertFile        string `json:"cert_file,omitempty"`       // 外部证书 PEM 路径（FR-SYS-011）
	KeyFile         string `json:"key_file,omitempty"`        // 外部私钥 PEM 路径
	TLSSelfSigned   bool   `json:"tls_self_signed,omitempty"` // 声明使用自签证书（缺证书时由 nfvisd 生成）
}

// FirewallConfig 管理面主机防火墙（FR-NET-002/FR-SEC-001，决策 #388）。
//
// 作用面＝**管理口（system.management.interface）入向**：由 nfvisd 渲染成宿主 nftables 的
// 独立表 `table inet nfvis-firewall`（不动系统上任何其它表）；数据面（VPP/Docker/libvirt）不受影响。
// 配置全空（无规则且默认策略未设/accept）＝未配置＝不下发过滤表。
type FirewallConfig struct {
	// DefaultPolicy 默认策略：""/"accept" = accept（缺省）、"drop" = 白名单模式。
	DefaultPolicy string         `json:"default_policy,omitempty"`
	Rules         []FirewallRule `json:"rules,omitempty"`
}

// FirewallRule 一条主机防火墙规则（按 Seq 升序首命中生效）。
//
// 至少给一条匹配条件（Source/Protocol/Port 之一）；Port 仅 tcp/udp 可设（目的端口）；
// Source 为 v4/v6 前缀（裸 IP 在渲染时按 /32、/128 归一）。
type FirewallRule struct {
	Seq      int    `json:"seq"`
	Action   string `json:"action"` // accept|drop
	Source   string `json:"source,omitempty"`
	Protocol string `json:"protocol,omitempty"` // tcp|udp|icmp|any（""/any = 任意）
	Port     int    `json:"port,omitempty"`     // 目的端口，仅 tcp/udp
}

// FirewallEnabled 防火墙是否处于启用态：有规则，或默认策略为 drop（决策 #388 的启用判据，单一事实源）。
func (f *FirewallConfig) FirewallEnabled() bool {
	return f != nil && (len(f.Rules) > 0 || f.DefaultPolicy == "drop")
}

// FirewallPolicy 生效默认策略（缺省 accept）。
func (f *FirewallConfig) FirewallPolicy() string {
	if f != nil && f.DefaultPolicy == "drop" {
		return "drop"
	}
	return "accept"
}

// FirewallOf 取配置的防火墙段（可能为 nil；nil 安全）。
func (c *Config) FirewallOf() *FirewallConfig {
	if c == nil || c.System == nil {
		return nil
	}
	return c.System.Firewall
}

// MgmtInterfaceOf 管理口名（未声明返回空串）。
func (c *Config) MgmtInterfaceOf() string {
	if c == nil || c.System == nil || c.System.Management == nil {
		return ""
	}
	return c.System.Management.Interface
}

// InterfaceConfig 物理网卡的配置视图（OpenAPI InterfaceUpdate，契约补全后含 name/sriov/ingress_policy/egress_policy）。
type InterfaceConfig struct {
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	MTU           int             `json:"mtu,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	Sriov         *InterfaceSriov `json:"sriov,omitempty"`          // FR-NET-004
	IngressPolicy string          `json:"ingress_policy,omitempty"` // 入向 QoS 绑定（命令树 §2.5，决策 #331 前后并存）
	EgressPolicy  string          `json:"egress_policy,omitempty"`  // 出向 QoS 绑定（决策 #331；VPP policer output）
	// StormControl 入向广播/组播风暴抑制（FR-NET-019，决策 #385）：按目的 MAC 分类限速，
	// 超速丢弃；nil = 未配置。unknown-unicast 有意不做（L2 掩码表达不了「目的 MAC 未学习」）。
	StormControl *StormControl `json:"storm_control,omitempty"`
	// PortSecurity 端口安全的允许源 MAC 白名单（决策 #389）：**非空即启用**——该接口入向
	// （L2）只放行白名单源 MAC（IP 帧与非 IP 帧 alike），其余丢弃。元素是归一小写形态
	// （PortSecMAC 在解码层归一，CLI/REST/候选合并等一切 JSON 写路径经同一入口）。
	// 前置与互斥的校验口径见 model.Validate（L2 交换机静态成员、bond 成员拒绝、
	// 与 L3 接口 ACL 的 macip 绑定槽互斥）。
	PortSecurity []PortSecMAC `json:"port_security,omitempty"`
}

// PortSecMAC 端口安全白名单里的一条源 MAC（决策 #389）。
//
// 归一化放在**模型解码入口**而不是只做在 CLI 别名层：REST PUT /interfaces/{name}、候选
// 整体写入与 CLI 语句树回放走的是同一份 JSON 解码——只在别名层归一，大写形态会从 REST
// 落库，`| display set` 的回放自校验（语句层恒产出小写）随即对不上账。可解析的取值一律
// 归一为小写冒号形式；**不可解析的原样保留**，交由 model.Validate 给出带路径的报错
// （解码层不报错——单一报错出口比 JSON 解码错误更可操作）。
type PortSecMAC string

// UnmarshalJSON 实现 json.Unmarshaler：可解析即归一小写（net.HardwareAddr.String() 的
// 形式即 `aa:bb:cc:dd:ee:ff`），不可解析原样保留（Validate 兜底报错）。
func (m *PortSecMAC) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if hw, err := net.ParseMAC(strings.TrimSpace(s)); err == nil {
		*m = PortSecMAC(hw.String())
	} else {
		*m = PortSecMAC(s)
	}
	return nil
}

// StormControl 接口的入向风暴抑制声明（决策 #385）。
//
// 单位 **kbps**（底座 policer 的 1R2C 即 kbps；不做 pps 换算），0/缺省 = 未配置该类。
// 两类各自独立：只配一类时另一类不限。语义与数据面结构见 network.StormProvider。
type StormControl struct {
	BroadcastKbps int `json:"broadcast_kbps,omitempty"` // 广播（目的 MAC ff:ff:ff:ff:ff:ff）
	MulticastKbps int `json:"multicast_kbps,omitempty"` // 组播（目的 MAC I/G 位 = 1，含广播之外的组播）
}

// InterfaceSriov 物理口上的 SR-IOV VF 数量配置。
type InterfaceSriov struct {
	VFCount int `json:"vf_count"`
}

// Bond 链路聚合（FR-NET-017）。
type Bond struct {
	Name        string   `json:"name"`
	Members     []string `json:"members"`
	Lacp        *Lacp    `json:"lacp,omitempty"` // nil = 静态聚合
	MTU         int      `json:"mtu,omitempty"`
	Description string   `json:"description,omitempty"`
}

type Lacp struct {
	Mode     string `json:"mode"`               // active|passive
	Interval string `json:"interval,omitempty"` // fast|slow
}

// KernelConfig 内核启动基线托管（FR-SYS-014；决策 #66）。
// 大页与隔离核的唯一真源是 resource-pools（本结构只管其余启动参数）。
type KernelConfig struct {
	NMIWatchdog          *bool    `json:"nmi_watchdog,omitempty"`          // nil = 不托管
	TransparentHugepages string   `json:"transparent_hugepages,omitempty"` // always|madvise|never
	IOMMU                string   `json:"iommu,omitempty"`                 // on|off|pt
	LowLatency           bool     `json:"low_latency,omitempty"`           // 低延迟参数组（显式选择；机型相关项由真机补全决定）
	TunedProfile         string   `json:"tuned_profile,omitempty"`         // 写入 /etc/nfvis/tuned-profile
	Params               []string `json:"params,omitempty"`                // 附加内核参数（逃生口）
}

// VirtualSwitch 虚拟交换机（L2 = bridge domain，见附录 B 映射）。
// L3 交换机（type=l3）的 l3-interface 与静态路由数据按附录 B 映射存放在同名 Vrf 条目中。
type VirtualSwitch struct {
	Name         string     `json:"name"`
	Type         string     `json:"type"` // l2|l3，创建后不可改
	Description  string     `json:"description,omitempty"`
	VlanAccess   int        `json:"vlan_access,omitempty"`   // 仅 L2
	CrossConnect bool       `json:"cross_connect,omitempty"` // 仅 L2，与 ports/gateway 互斥
	Gateway      *VSGateway `json:"gateway,omitempty"`       // 仅 L2：BVI 三层网关（FR-NET-014）
	// DhcpRelayServer DHCP 中继的服务器地址（决策 #335）：仅已配网关（BVI）的 L2 交换机可配，
	// 中继源地址自动取 BVI 的 IPv4 网关地址；读视图（REST 详情/列表）以 dhcp_relay:{server} 形状给出。
	DhcpRelayServer string `json:"dhcp_relay_server,omitempty"`
	// —— DHCP 服务器（决策 #359）：仅 L2 且已配 IPv4 BVI 网关的交换机可配，与 dhcp_relay_server 互斥。
	// 5 个平铺键与 DhcpRelayServer 同族：pool 两键是启用要件（同时给才有效），其余为可选叶子；
	// 读视图（REST 详情/列表）以 dhcp_server:{pool_start,pool_end,lease_time_seconds,dns,domain_name} 形状给出。
	// 与 #356 的 metrics.db 同口径：租约是运行态，不进配置备份/恢复语义。
	DhcpServerPoolStart        string `json:"dhcp_server_pool_start,omitempty"`
	DhcpServerPoolEnd          string `json:"dhcp_server_pool_end,omitempty"`
	DhcpServerLeaseTimeSeconds int    `json:"dhcp_server_lease_time_seconds,omitempty"` // 0=未配置（生效值 86400）
	DhcpServerDNS              string `json:"dhcp_server_dns,omitempty"`                // 缺省下发 BVI 地址
	DhcpServerDomainName       string `json:"dhcp_server_domain_name,omitempty"`
	// LearnLimit MAC 学习条数上限（决策 #337）：仅 L2；>0 时下发 VPP
	// `bridge_domain_set_learn_limit`（缓解环路/广播风暴的第二道防线，非阻断）。0=未配置（VPP 默认）。
	LearnLimit int `json:"learn_limit,omitempty"`
	// DNSProxyServers 数据面 DNS 代理的**按域上游**（决策 #345，FR-NET-010）：只对该交换机转发域
	// （L2＝网关 BVI；L3＝其 l3-interface）的入向查询生效。本域非空优先，否则回落全局
	// （VppConfig.DNSProxyServers）；两者皆空时对该域查询回 SERVFAIL。逐条须为合法 IP（v4/v6）。
	DNSProxyServers []string      `json:"dns_proxy_servers,omitempty"`
	Ports           []VSwitchPort `json:"ports,omitempty"`
}

// Vrf L3 虚拟交换机的配置数据（FR-NET-013；CLI `virtual-switches <n> type l3` 映射为同名条目）。
type Vrf struct {
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	L3Interfaces []L3Interface `json:"l3_interfaces,omitempty"`
	Routes       []Route       `json:"routes,omitempty"`
}

type VSGateway struct {
	Addresses []string `json:"addresses,omitempty"` // ip-prefix，可多条（IPv4/IPv6）
	Vrf       string   `json:"vrf,omitempty"`       // 缺省为专属 VRF vr-<name>
	AclIn     string   `json:"acl_in,omitempty"`
	AclOut    string   `json:"acl_out,omitempty"`
}

type VSwitchPort struct {
	Seq                int    `json:"seq"`
	Interface          string `json:"interface,omitempty"` // 物理口/bond 名
	Vnf                string `json:"vnf,omitempty"`       // 与 interface/container 三选一
	VnfInterface       string `json:"vnf_interface,omitempty"`
	Container          string `json:"container,omitempty"`
	ContainerInterface string `json:"container_interface,omitempty"`
	TrunkVlans         []int  `json:"trunk,omitempty"`  // trunk 模式 tagged VID 列表
	NativeVlan         int    `json:"native,omitempty"` // trunk native / access 由交换机级 VlanAccess 决定
	AclIn              string `json:"acl_in,omitempty"`
	AclOut             string `json:"acl_out,omitempty"`
}

// L3Interface L3 交换机的三层接口（FR-NET-013）。
type L3Interface struct {
	Interface string   `json:"interface"` // 物理口、bond 或 vlan <v> 子接口
	Vlan      int      `json:"vlan,omitempty"`
	Addresses []string `json:"addresses,omitempty"` // ip-prefix，可多条
	AclIn     string   `json:"acl_in,omitempty"`
}

type Route struct {
	Prefix   string `json:"prefix"` // CIDR，支持 0.0.0.0/0 与 IPv6
	NextHop  string `json:"next_hop"`
	Distance int    `json:"distance,omitempty"`
}

// Acl 五元组 ACL（VPP acl plugin）。
type Acl struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Rules       []AclRule `json:"rules"`
}

type AclRule struct {
	Seq             int    `json:"seq"`
	Direction       string `json:"direction,omitempty"` // ingress|egress
	Source          string `json:"source,omitempty"`    // ip-prefix 或 any
	Destination     string `json:"destination,omitempty"`
	Protocol        string `json:"protocol,omitempty"` // tcp|udp|icmp|any
	SourcePort      string `json:"source_port,omitempty"`
	DestinationPort string `json:"destination_port,omitempty"`
	Action          string `json:"action"` // permit|deny
}

// NatConfig NAT44（VPP nat44 plugin，仅作用于 L3 交换机）。
type NatConfig struct {
	SourcePools []NatSourcePool `json:"source_pools,omitempty"`
	Rules       []NatRule       `json:"rules,omitempty"`
	Static      []NatStatic     `json:"static,omitempty"`
}

type NatSourcePool struct {
	Name         string `json:"name"`
	AddressRange string `json:"address_range"` // "<ip> to <ip>"
}

type NatRule struct {
	Seq           int       `json:"seq"`
	MatchSource   string    `json:"match_source"` // ip-prefix
	VirtualSwitch string    `json:"virtual_switch"`
	Action        NatAction `json:"action"`
}

type NatAction struct {
	SourcePool string `json:"source_pool,omitempty"`
	Interface  string `json:"interface,omitempty"`
}

type NatStatic struct {
	InsideIP  string `json:"inside_ip"`
	OutsideIP string `json:"outside_ip"`
}

// PortMirroring SPAN 会话（VPP span）。
type PortMirroring struct {
	Name     string   `json:"name"`
	Source   PMSource `json:"source"`
	Analyzer string   `json:"analyzer"` // 分析端口（物理口）
}

type PMSource struct {
	Interface    string `json:"interface,omitempty"`
	Vnf          string `json:"vnf,omitempty"`
	VnfInterface string `json:"vnf_interface,omitempty"`
	Direction    string `json:"direction,omitempty"` // ingress|egress|both
}

// QosPolicy 端口限速（VPP policer，CIR/CBS）。
type QosPolicy struct {
	Name string `json:"name"`
	Cir  int    `json:"cir"` // bps
	Cbs  int    `json:"cbs"` // bytes
}

// VxlanTunnel VXLAN overlay 隧道（单播 remote；v1 只做 L2 成员）。
//
// Name 是产品侧标识——VPP 分配的接口名与 instance 产品**都不依赖**；数据面身份是平台打在
// 隧道口上的**接口 tag**（DataPlaneTag()，见下）：VPP 26.06 的 vxlan dump 恒空（真机实证），
// 恢复重放按 tag 判存量、变更撤旧按**旧声明的元组**（不靠 dump）。VirtualSwitch 可选：
// 给了就把隧道口加入该 L2 交换机的 BD。v1 边界（如实）：不做组播/BUM 复制、ARP/ND 代理与
// Bypass、VXLAN-GPE、IPv6 下垫层、dst-port 以外的封装参数、隧道作 L3 接口、跨 VRF 建隧。
type VxlanTunnel struct {
	Name          string `json:"name"`
	Vni           int    `json:"vni"`
	Local         string `json:"local"`  // 本地下垫地址（IPv4）
	Remote        string `json:"remote"` // 远端下垫地址（IPv4，单播）
	DstPort       int    `json:"dst_port,omitempty"`
	VirtualSwitch string `json:"virtual_switch,omitempty"`
}

// VXLAN 取值域与缺省（校验与数据面共用，单一事实源）。
const (
	VxlanMinVni         = 1
	VxlanMaxVni         = 16777215 // 24 位
	VxlanDefaultDstPort = 4789     // IANA 分配的 VXLAN UDP 端口
)

// 数据面接口 tag：隧道身份（`sw_interface_tag_add_del` 打标、`sw_interface_dump` 的 tag 字段
// 回读）。VPP 26.06 的 vxlan dump 恒空（真机实证），故不依赖它、也不依赖 VPP 分配的接口名
// ——与 DHCP tap 按 HostIfName 识别同族。tag 是固定宽度 string[64]（含 NUL 至多 63 字节），
// 超长会被静默截断、读回对不上，校验层据此拒绝过长的隧道名。
const (
	VxlanTagPrefix = "nfvis-vxlan:"
	VxlanTagMaxLen = 63
)

// DataPlaneTag 该隧道在数据面上的接口 tag（"nfvis-vxlan:<name>"）。
func (t VxlanTunnel) DataPlaneTag() string { return VxlanTagPrefix + t.Name }

// SameTuple 两条声明的**数据面元组**是否相同（vni/local/remote/dst_port）。
// name 不参与：改名的语义是「删旧建新」，由调用方以新旧两条声明给出（提交 diff 里都有）。
func (t VxlanTunnel) SameTuple(o VxlanTunnel) bool {
	return t.Vni == o.Vni && t.Local == o.Local && t.Remote == o.Remote &&
		t.EffectiveDstPort() == o.EffectiveDstPort()
}

// EffectiveDstPort 生效的目的端口：缺省（0/未配置）回落 4789。
func (t VxlanTunnel) EffectiveDstPort() int {
	if t.DstPort == 0 {
		return VxlanDefaultDstPort
	}
	return t.DstPort
}

// ResourcePool 全局资源池（FR-CMP-001）。count 为配置项；total/allocated/free 为运行态视图。
type ResourcePool struct {
	Hugepages []HPool   `json:"hugepages,omitempty"`
	CPU       *CPUSetup `json:"cpu,omitempty"`
}

type HPool struct {
	PageSize string `json:"page_size"` // 2M|1G
	Count    int    `json:"count"`
}

type CPUSetup struct {
	IsolatedCores []int      `json:"isolated_cores,omitempty"`
	Numa          []NumaNode `json:"numa,omitempty"`
}

type NumaNode struct {
	Node  int   `json:"node"`
	Cores []int `json:"cores"`
}

// VppConfig VPP 数据面运行时配置（FR-SYS-008，nfvisd 生成 startup.conf）。
type VppConfig struct {
	CPU     *VppCPU     `json:"cpu,omitempty"`
	Memory  *VppMemory  `json:"memory,omitempty"`
	DPDK    *VppDPDK    `json:"dpdk,omitempty"`
	Plugins []VppPlugin `json:"plugins,omitempty"`
	// DNSProxyServers 数据面 DNS 代理的**全局上游**（决策 #345，FR-NET-010）：全局或任一交换机
	// （VirtualSwitch.DNSProxyServers）非空即启用——注册 punt socket 并起自研域内转发器；全空即注销
	// （VPP 恢复默认处理）。上游由 nfvisd 用**宿主网络栈**发起（不是 VPP FIB；`set system dns server`
	// 的宿主解析器与之互不影响）。逐条须为合法 IP（v4/v6），条数不设上限。纯 UDP 转发：不缓存。
	DNSProxyServers []string `json:"dns_proxy_servers,omitempty"`
}

// VppStartupKey vpp 配置段中**真正进入 startup.conf** 的字段子集（决策 #400）。
// DNSProxyServers 不属于其中：它只影响 nfvisd 侧自研 DNS 转发器（决策 #345），生成器不输出它
// ⇒ 改它不需要重启数据面。pending_restart 与提交期「需重启」提示都按本子集判定。
type VppStartupKey struct {
	CPU     *VppCPU     `json:"cpu,omitempty"`
	Memory  *VppMemory  `json:"memory,omitempty"`
	DPDK    *VppDPDK    `json:"dpdk,omitempty"`
	Plugins []VppPlugin `json:"plugins,omitempty"`
}

// StartupKey 提取影响 startup.conf 的字段子集（nil 安全；决策 #400）。
func (v *VppConfig) StartupKey() VppStartupKey {
	if v == nil {
		return VppStartupKey{}
	}
	return VppStartupKey{CPU: v.CPU, Memory: v.Memory, DPDK: v.DPDK, Plugins: v.Plugins}
}

type VppCPU struct {
	MainCore        int    `json:"main_core,omitempty"`
	CorelistWorkers string `json:"corelist_workers,omitempty"` // 如 "5,7,9-11"
	WorkersPerNuma  int    `json:"workers_per_numa,omitempty"` // 与 corelist_workers 互斥
}

type VppMemory struct {
	MainHeapSize       string `json:"main_heap_size,omitempty"`
	BuffersPerNuma     int    `json:"buffers_per_numa,omitempty"`
	HugepagePreference string `json:"hugepage_preference,omitempty"` // 2M|1G
}

type VppDPDK struct {
	Dev       VppDevDefault    `json:"dev,omitempty"`        // 全局默认
	PerDev    []VppDevOverride `json:"per_dev,omitempty"`    // 单网卡覆盖
	UIODriver string           `json:"uio_driver,omitempty"` // vfio-pci|igb-uio
}

type VppDevDefault struct {
	RxQueues      int `json:"rx_queues,omitempty"`
	TxQueues      int `json:"tx_queues,omitempty"`
	RxDescriptors int `json:"rx_descriptors,omitempty"`
	TxDescriptors int `json:"tx_descriptors,omitempty"`
}

type VppDevOverride struct {
	Interface     string `json:"interface"`
	RxQueues      int    `json:"rx_queues,omitempty"`
	TxQueues      int    `json:"tx_queues,omitempty"`
	RxDescriptors int    `json:"rx_descriptors,omitempty"`
	TxDescriptors int    `json:"tx_descriptors,omitempty"`
}

type VppPlugin struct {
	Name  string `json:"name"`
	State string `json:"state"` // enable|disable
}

// ProtocolsConfig 顶级协议层级（当前仅 LLDP，FR-NET-018）。
type ProtocolsConfig struct {
	LLDP *LldpConfig `json:"lldp,omitempty"`
}

type LldpConfig struct {
	Enabled               bool            `json:"enabled,omitempty"`
	AdvertisementInterval int             `json:"advertisement_interval,omitempty"`
	Interfaces            []LldpInterface `json:"interfaces,omitempty"`
}

type LldpInterface struct {
	Interface string `json:"interface"`
	Enabled   bool   `json:"enabled"`
}

// VMFunction VM VNF（FR-CMP-010~019）。
type VMFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Image       string         `json:"image"`
	VCPU        VMCpu          `json:"vcpu"`
	Memory      VMMemory       `json:"memory"`
	Disks       []VMDisk       `json:"disks,omitempty"`
	Interfaces  []VnfInterface `json:"interfaces,omitempty"`
	// PCIDevices 直通给该 VM 的通用 PCI 设备地址（BDF，如 0000:03:00.0，可多条；FR-CMP-023）。
	// 语法校验见 NormalizeBDF；**归一后**由计算编排层用于 hostdev 组装与存在性检查。
	PCIDevices    []string   `json:"pci_devices,omitempty"`
	CloudInit     *CloudInit `json:"cloud_init,omitempty"`
	SerialConsole *bool      `json:"serial_console,omitempty"` // 缺省启用
	Autostart     bool       `json:"autostart,omitempty"`
}

type VMCpu struct {
	Count int   `json:"count"`
	Pin   *bool `json:"pin,omitempty"` // 缺省 true
}

type VMMemory struct {
	SizeMB       int    `json:"size_mb"`
	HugepageSize string `json:"hugepage_size,omitempty"` // 2M|1G，缺省取资源池主池
	NumaNode     *int   `json:"numa_node,omitempty"`     // 可选；未声明与 node 0 需可区分（账本 NUMA 亲和分配）
	Backing      string `json:"backing,omitempty"`       // hugepage(默认)|normal；normal 禁止 vhost-user（FR-CMP-019）
}

type VMDisk struct {
	Name   string `json:"name"`
	SizeGB int    `json:"size_gb,omitempty"` // 空盘容量（与 image 二选一）
	Image  string `json:"image,omitempty"`   // 从 vm-image 克隆创建
}

// VnfInterface VNF 虚拟网卡（VM：vhost-user/sriov-vf；容器：memif，FR-NET-020~022）。
type VnfInterface struct {
	Name          string     `json:"name"`
	Type          string     `json:"type"`                     // vhost-user|sriov-vf|memif
	VirtualSwitch string     `json:"virtual_switch,omitempty"` // sriov-vf 时仅登记
	MAC           string     `json:"mac,omitempty"`
	Vlan          int        `json:"vlan,omitempty"`
	Sriov         *SriovBind `json:"sriov,omitempty"`
}

type SriovBind struct {
	PhysicalInterface string `json:"physical_interface"`
	VFID              int    `json:"vf_id"`
}

type CloudInit struct {
	UserData string   `json:"user_data,omitempty"`
	SSHKeys  []string `json:"ssh_keys,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
}

// ContainerFunction 容器 VNF（FR-CMP-020~022）。
type ContainerFunction struct {
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	Image         string            `json:"image"`
	VCPU          int               `json:"vcpu,omitempty"`
	MemoryMB      int               `json:"memory_mb,omitempty"`
	Interfaces    []VnfInterface    `json:"interfaces,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	RestartPolicy string            `json:"restart_policy,omitempty"` // no|on-failure
	Autostart     bool              `json:"autostart,omitempty"`
}
