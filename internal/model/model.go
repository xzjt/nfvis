// Package model 定义 NFViS 的单一配置数据模型。
//
// 结构与 docs/NFViS-openapi.yaml 的 schemas 一一对应（契约先行），
// JSON 标签与 ConfigDocument 保持一致；committed 与 candidate 配置共用本模型。
// 语义校验见 validate.go，配置 diff 见 diff.go。
package model

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
	ResourcePools           *ResourcePool       `json:"resource_pools,omitempty"`
	Vpp                     *VppConfig          `json:"vpp,omitempty"`
	Protocols               *ProtocolsConfig    `json:"protocols,omitempty"`
	VirtualMachineFunctions []VMFunction        `json:"virtual_machine_functions,omitempty"`
	ContainerFunctions      []ContainerFunction `json:"container_functions,omitempty"`

	// Annotations 配置节点注释（FR-CFG-007 annotate；键=语句路径，决策 #27）
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SystemConfig 对应 OpenAPI SystemConfig。
type SystemConfig struct {
	Kernel             *KernelConfig     `json:"kernel,omitempty"` // 内核启动基线（FR-SYS-014）
	Hostname           string            `json:"hostname,omitempty"`
	Timezone           string            `json:"timezone,omitempty"`
	Ntp                []NtpServer       `json:"ntp,omitempty"`
	DNSServers         []string          `json:"dns_servers,omitempty"`
	Management         *MgmtConfig       `json:"management,omitempty"`
	Login              *SystemLogin      `json:"login,omitempty"` // 本地用户与 class（FR-SEC-002/003，附录 A #25）
	Syslog             *SyslogConfig     `json:"syslog,omitempty"`
	API                *APIConfig        `json:"api,omitempty"`
	IdleTimeoutMinutes int               `json:"idle_timeout_minutes,omitempty"`
	Health             *HealthThresholds `json:"health,omitempty"`  // 硬件健康告警阈值（FR-SYS-012）
	Metrics            *MetricsConfig    `json:"metrics,omitempty"` // 可观测性配置（决策 #356）
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

// InterfaceConfig 物理网卡的配置视图（OpenAPI InterfaceUpdate，契约补全后含 name/sriov/ingress_policy/egress_policy）。
type InterfaceConfig struct {
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	MTU           int             `json:"mtu,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	Sriov         *InterfaceSriov `json:"sriov,omitempty"`          // FR-NET-004
	IngressPolicy string          `json:"ingress_policy,omitempty"` // 入向 QoS 绑定（命令树 §2.5，决策 #331 前后并存）
	EgressPolicy  string          `json:"egress_policy,omitempty"`  // 出向 QoS 绑定（决策 #331；VPP policer output）
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
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	Image         string         `json:"image"`
	VCPU          VMCpu          `json:"vcpu"`
	Memory        VMMemory       `json:"memory"`
	Disks         []VMDisk       `json:"disks,omitempty"`
	Interfaces    []VnfInterface `json:"interfaces,omitempty"`
	CloudInit     *CloudInit     `json:"cloud_init,omitempty"`
	SerialConsole *bool          `json:"serial_console,omitempty"` // 缺省启用
	Autostart     bool           `json:"autostart,omitempty"`
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
