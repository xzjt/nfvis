package main

import (
	"sort"
	"strings"
)

// ---------- 命令树 schema（对应工程骨架中的 internal/schema） ----------
// 编译期共享给 CLI（补全）与守护进程（校验）——原型中同进程演示。
// 参数节点（KindParam）可直接携带子树：<ifname> 下挂接口配置，<name> 下挂
// 对应资源类型的配置，补全与校验统一走 Children 遍历。

type NodeKind int

const (
	KindKeyword NodeKind = iota // 固定关键字
	KindValue                   // 关键字后接一个自由值（如 hostname <string>）
	KindParam                   // 动态候选参数（<ifname>、<name>...）
)

type Node struct {
	Name     string // 关键字名或参数占位符
	Kind     NodeKind
	Help     string
	Dynamic  string // KindParam 的动态候选来源（engine.dynamic 提供列表）
	Children []*Node
	Run      func(e *Engine, tokens []string) string // 顶级命令执行器（nil = 纯路径节点）
}

func K(name, help string, children ...*Node) *Node {
	return &Node{Name: name, Kind: KindKeyword, Help: help, Children: children}
}

// KV：需要一个值的关键字，如 hostname <string>
func KV(name, help string) *Node {
	return &Node{Name: name, Kind: KindValue, Help: help}
}

// P：参数节点（动态候选），可携带子树
func P(placeholder, help, dynamic string, children ...*Node) *Node {
	return &Node{Name: placeholder, Kind: KindParam, Help: help, Dynamic: dynamic, Children: children}
}

func (n *Node) child(word string) *Node {
	for _, c := range n.Children {
		if c.Name == word {
			return c
		}
	}
	return nil
}

// candidates 返回当前上下文的补全候选：[]{候选文本, 帮助}
func (n *Node) candidates(prefix string, e *Engine) [][2]string {
	var out [][2]string
	for _, c := range n.Children {
		switch c.Kind {
		case KindParam:
			for _, v := range e.dynamic(c.Dynamic) {
				if strings.HasPrefix(v, prefix) {
					out = append(out, [2]string{v, c.Help})
				}
			}
		default:
			if strings.HasPrefix(c.Name, prefix) {
				out = append(out, [2]string{c.Name, c.Help})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// ---------- 操作模式命令树 ----------

func operTree() *Node {
	return K("", "root",
		K("show", "显示系统状态与配置",
			K("version", "软件版本"),
			K("system", "系统信息",
				K("uptime", "运行时长"),
				K("cpu", "CPU 与隔离核"),
				K("hugepages", "大页内存"),
				K("storage", "存储使用"),
			),
			K("interfaces", "接口", P("<ifname>", "接口名", "ifnames")),
			K("virtual-switches", "虚拟交换机", P("<name>", "虚拟交换机名", "vswitches")),
			K("vrfs", "L3 虚拟交换机 (VRF)"),
			K("vpp", "VPP 数据面",
				K("threads", "线程清单与绑核"),
				K("runtime", "每线程运行时统计"),
				K("buffers", "buffer 池使用"),
				K("memory", "内存与大页"),
				K("capture", "抓包会话与 pcap 清单"),
			),
			K("virtual-machine-functions", "VM VNF", P("<name>", "VNF 名", "vmnames")),
			K("container-functions", "容器 VNF", P("<name>", "VNF 名", "ctnames")),
			K("images", "镜像仓库"),
			K("resource-pools", "资源池（大页/隔离核）"),
			K("alarms", "告警"),
			K("configuration", "配置",
				K("candidate", "当前 candidate"),
				K("rollback", "与第 n 个历史快照比较", P("<n>", "快照编号", "revisions")),
			),
			K("users", "本地用户"),
		),
		K("request", "运维动作",
			K("virtual-machine-functions", "VM VNF 操作",
				P("<name>", "VNF 名", "vmnames",
					K("start", "启动"),
					K("stop", "停止"),
					K("restart", "重启"),
					K("console", "进入串口"),
				),
			),
			K("container-functions", "容器 VNF 操作",
				P("<name>", "VNF 名", "ctnames",
					K("start", "启动"),
					K("stop", "停止"),
				),
			),
			K("vpp", "VPP 数据面操作",
				K("restart", "重启数据面（按 committed 配置重建）"),
			),
			K("system", "系统操作",
				K("reboot", "重启系统"),
				K("backup", "备份配置"),
			),
		),
		K("configure", "进入配置模式"),
		K("ping", "连通性测试", P("<host>", "目标地址", "")),
		K("exit", "退出 CLI"),
		K("help", "帮助"),
	)
}

// ---------- 配置模式：顶级命令树 + 配置路径树 ----------

func configTopTree() *Node {
	return K("", "root",
		K("set", "设置配置语句"),
		K("delete", "删除配置语句/子树"),
		K("show", "显示 candidate（当前层级）"),
		K("commit", "提交 candidate",
			K("confirmed", "超时未确认自动回滚（原型 15s）"),
		),
		K("rollback", "回退到历史快照为 candidate（需再 commit）",
			P("<n>", "快照编号", "revisions"),
		),
		K("compare", "candidate 与 committed 差异"),
		K("edit", "进入层级"),
		K("up", "返回上一级"),
		K("top", "返回顶层"),
		K("run", "执行操作模式命令"),
		K("exit", "退出配置模式（有未提交变更时确认）"),
		K("discard", "丢弃 candidate"),
	)
}

func configPathTree() *Node {
	return K("", "cfgroot",
		K("system", "系统配置",
			K("hostname", "主机名", KV("<string>", "名称")),
			K("ntp", "NTP",
				K("server", "服务器", P("<ip>", "地址", "")),
			),
			K("syslog", "日志",
				K("host", "远程 syslog",
					P("<ip>", "地址", "",
						K("severity", "级别", KV("<level>", "debug|info|warn|error")),
					),
				),
			),
			K("management", "管理口",
				K("ip", "IP", KV("address", "地址/掩码")),
			),
		),
		K("interfaces", "物理接口",
			P("<ifname>", "接口名", "ifnames",
				K("description", "描述", KV("<string>", "文本")),
				K("disable", "禁用接口"),
				K("mtu", "MTU", KV("<uint>", "字节数")),
				K("sriov", "SR-IOV",
					K("vf-count", "VF 数量", KV("<uint>", "数量")),
				),
			),
		),
		K("virtual-switches", "虚拟交换机",
			P("<name>", "虚拟交换机名", "vswitches",
				K("type", "类型", KV("<type>", "l2|l3")),
				K("vlan", "VLAN（仅 L2）",
					K("access", "access VLAN", KV("<vlan>", "1-4094")),
				),
				K("gateway", "BVI 三层网关（仅 L2，FR-NET-014）",
					K("ip", "网关地址", KV("<ip-prefix>", "如 192.168.100.1/24")),
					K("vrf", "所属 VRF（缺省 vr-<交换机名>）", P("<name>", "VRF 名", "vswitches")),
				),
				K("ports", "成员端口",
					P("<seq>", "序号", "",
						K("interface", "物理口成员", P("<ifname>", "接口名", "ifnames")),
						K("vnf", "VM 成员", P("<vm>", "VNF 名", "vmnames")),
						K("container", "容器成员", P("<ct>", "VNF 名", "ctnames")),
					),
				),
				K("l3-interface", "三层接口（仅 L3）",
					K("ip", "IP 配置", KV("address", "地址/掩码")),
				),
				K("static-routes", "静态路由（仅 L3）",
					P("<prefix>", "目的网段", "",
						K("next-hop", "下一跳", KV("<ip>", "地址")),
					),
				),
			),
		),
		K("virtual-machine-functions", "VM VNF",
			P("<name>", "VNF 名", "vmnames",
				K("image", "镜像", P("<image>", "镜像名", "images")),
				K("vcpu", "vCPU",
					K("count", "数量", KV("<uint>", "个数")),
				),
				K("memory", "内存",
					K("size-mb", "大小 MB", KV("<uint>", "MB")),
					K("hugepage-size", "页大小（从对应页池分配）", KV("<size>", "2M|1G")),
					K("backing", "内存类型（默认 hugepage，normal 时禁止 vhost-user）", KV("<type>", "hugepage|normal")),
				),
				K("disks", "附加数据盘（FR-CMP-018）",
					P("<disk>", "盘名", "",
						K("size-gb", "空盘容量", KV("<uint>", "GB")),
						K("image", "从 vm-image 克隆", P("<image>", "镜像名", "images")),
					),
				),
				K("interfaces", "vNIC",
					P("<vnic>", "vNIC 名", "",
						K("type", "接入类型", KV("<type>", "vhost-user|sriov-vf")),
						K("mac", "MAC", KV("<mac>", "地址")),
						K("vlan", "VLAN", KV("<vlan>", "1-4094")),
						K("virtual-switch", "所属交换机", P("<name>", "虚拟交换机名", "vswitches")),
					),
				),
				K("cloud-init", "初始化注入",
					K("ssh-key", "SSH 公钥", KV("<key>", "公钥")),
				),
				K("autostart", "自启动", KV("<bool>", "true|false")),
				K("description", "描述", KV("<string>", "文本")),
			),
		),
		K("container-functions", "容器 VNF",
			P("<name>", "VNF 名", "ctnames",
				K("image", "镜像", P("<image>", "镜像名", "images")),
				K("vcpu", "vCPU", KV("count", "数量")),
				K("memory", "内存", KV("size-mb", "MB")),
				K("interfaces", "vNIC (memif)", P("<vnic>", "vNIC 名", "")),
			),
		),
		K("resource-pools", "资源池",
			K("hugepages", "大页",
				K("page-size", "页大小", KV("<size>", "2M|1G")),
			),
			K("cpu", "隔离核",
				K("isolated-cores", "核列表", KV("<list>", "如 4-15")),
			),
		),
		K("vpp", "VPP 数据面配置（生成 startup.conf，变更需重启数据面生效）",
			K("cpu", "CPU 绑核",
				K("main-core", "主线程绑核", KV("<uint>", "核号（须在隔离核池内）")),
				K("corelist-workers", "worker 核列表", KV("<list>", "如 5,7")),
				K("workers-per-numa", "按 NUMA 分配 worker", KV("<uint>", "个数")),
			),
			K("memory", "内存",
				K("main-heap-size", "主堆大小", KV("<size>", "如 1G")),
				K("buffers-per-numa", "每 NUMA buffer 数", KV("<uint>", "个数")),
				K("hugepage-preference", "大页偏好", KV("<size>", "2M|1G")),
			),
			K("dpdk", "DPDK 设备参数",
				K("dev", "物理 NIC 队列/描述符（全局默认）",
					K("rx-queues", "收队列数", KV("<uint>", "个数")),
					K("tx-queues", "发队列数", KV("<uint>", "个数")),
					K("rx-descriptors", "收描述符", KV("<uint>", "个数")),
					K("tx-descriptors", "发描述符", KV("<uint>", "个数")),
					P("<ifname>", "单网卡覆盖（须为 DPDK 物理口）", "ifnames",
						K("rx-queues", "收队列数", KV("<uint>", "个数")),
						K("tx-queues", "发队列数", KV("<uint>", "个数")),
						K("rx-descriptors", "收描述符", KV("<uint>", "个数")),
						K("tx-descriptors", "发描述符", KV("<uint>", "个数")),
					),
				),
				K("uio-driver", "UIO 驱动", KV("<driver>", "vfio-pci|igb-uio")),
			),
			K("plugins", "插件开关",
				P("<name>", "插件名", "vppplugins", KV("state", "enable|disable")),
			),
		),
	)
}
