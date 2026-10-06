package schema

// 操作模式命令树（提示符 nfvis>）。
// 内容对应《NFViS-CLI命令树完整设计》§1——show/request/其余操作命令，
// 权限按 §4 预置 class 矩阵：show 全 R；request 生命周期/镜像/接口 O；
// request 破坏性动作（software/reboot/configuration/zeroize 等）与
// clear/start shell 为 S。
// pingOperArgs 返回 `ping` 的参数/选项子树（每次新建节点——节点在 finalize 时回填父指针，
// 不可跨父共享）。
//
// 决策 #330：`ipv6` 是**可选的无值族选择器关键字**（`ping [ipv6] <host> …`，取 VPP CLI
// `ping ipv6 <addr>` 的写法）。它建模为一个**叶子关键字**、与 `<host>` 平级且在其前——
// 这样「参数组」枚举器（cli_table_cover_test 的 coverForms）恰好产出
// `ping <host>` 与 `ping ipv6 <host>` 两族完整形态，不会把 `<host>` 与 v6 子参数错序拼接。
func pingOperArgs() []*Node {
	return []*Node{
		Opt(K("ipv6", "IPv6（v6 平面；目标为 IPv6 地址）")),
		PT("<host>", "string", "目标地址"),
		Opt(K("source", "源地址", V("ip", "IP"))),
		Opt(K("count", "次数", PT("<n>", "uint", "次数"))),
		Opt(K("vrf", "经指定 VRF", P("<name>", "VRF 名", DynVrfs))),
	}
}

// tracerouteOperArgs 返回 `traceroute` 的参数/选项子树（同 pingOperArgs 的建模约定）。
func tracerouteOperArgs() []*Node {
	return []*Node{
		Opt(K("ipv6", "IPv6（宿主侧 ICMPv6；需 root/CAP_NET_RAW）")),
		PT("<host>", "string", "目标地址"),
		Opt(K("vrf", "经指定 VRF", P("<name>", "VRF 名", DynVrfs))),
	}
}

func OperRoot() *Node {
	root := K("", "root",
		K("show", "显示系统状态与配置",
			K("version", "版本汇总（nfvis/ubuntu/vpp/dpdk/libvirt/qemu/docker）"),
			K("system", "系统信息",
				K("uptime", "运行时长"),
				K("cpu", "总核/隔离核/每核占用"),
				K("memory", "内存与大页使用（池内/池外）"),
				K("storage", "磁盘与镜像仓库占用"),
				K("hugepages", "大页内核参数与池状态"),
				// 决策 #356：历史时序读视图（`/metrics` 的历史底座，数据源＝需鉴权的
				// `GET /metrics/history`——`/metrics` 无鉴权是为 Prometheus 抓取）。
				// 省略参数＝概览：存储状态（可用/不可用 + 原因）、生效采样间隔与保留天数、库大小、
				// 序列数、样本数、时间范围（最旧/最新）、上次采样时刻与是否停滞（stale）；
				// `name <metric>` ＝该指标**各序列**（按标签分组）的时间点，指标名走动态候选
				// （来源＝存储内已知指标名），`last <duration>`（如 1h/30m/2d，默认 1h）与
				// `step <duration>`（降采样步长，省略=自动）是**具名查询的修饰项**——故挂在
				// `<metric>` 之下（《命令全表》§1.1 同写 `name <metric> [last …] [step …]`，
				// 「未收录形态」守护据此判定）。存储不可用/无数据时如实说明、不编造（v1 不设告警码）。
				K("metrics", "历史时序存储读视图",
					K("history", "历史时序（省略参数＝概览；数据源 GET /metrics/history）",
						Opt(K("name", "指定指标（动态候选＝存储内已知指标名）",
							P("<metric>", "指标名", DynMetricNames,
								Opt(K("last", "时间窗（如 1h / 30m / 2d，默认 1h）", V("duration", "如 1h / 30m / 2d"))),
								Opt(K("step", "降采样步长（省略=自动）", V("duration", "如 1m"))),
							))),
					),
				),
				K("kernel", "内核启动基线（cmdline / 运行实际 / 配置期望 三方对照）"),
				K("hardware", "硬件健康：温度/风扇/电源/SMART"),
				K("core-dumps", "崩溃转储清单（VPP/QEMU/nfvisd）"),
				K("tech-support", "诊断归档清单"),
				// 决策 #301：活动会话 / API Token 清单——super-user 见全部、其他 class 仅自己的
				// （数据范围判定在执行器调用的 aaa 实现处，树只声明命令级权限 R）。
				K("api", "API 服务",
					K("tokens", "活动会话 / API Token 清单"),
				),
				// 只挂 `sessions`（契约 §1.1）：此处**没有** `candidate`——「当前持锁会话的
				// candidate」的写法是顶层 `show configuration candidate`（唯一实现），
				// 这里再挂一份既重复又无实现（执行器只认前者；决策 #153 收口）。
				K("configuration", "配置查看",
					K("sessions", "candidate 持锁会话列表"),
				),
			),
			K("interfaces", "接口",
				K("physical", "DPDK 物理口",
					P("<ifname>", "接口名", DynVppIfnames,
						K("detail", "驱动/MAC/MTU/队列/NUMA"),
						K("statistics", "收发包/字节/错误/drop"),
						K("sriov", "VF 列表与占用状态"),
					),
				),
				K("management", "管理口（内核侧，IP/链路）"),
				// `physical` 可省（契约 §1.1，决策 #153）：`show interfaces <ifname> [detail|statistics|sriov]`
				// 与 `show interfaces physical <ifname> …` 等价。执行器一直支持这种写法、树里却没有，
				// 于是 `?`/Tab 补不出来（补全漂移）——两种写法必须同源。
				P("<ifname>", "接口名", DynVppIfnames,
					K("detail", "驱动/MAC/MTU/队列/NUMA"),
					K("statistics", "收发包/字节/错误/drop"),
					K("sriov", "VF 列表与占用状态"),
				),
			),
			K("virtual-switches", "虚拟交换机",
				P("<name>", "虚拟交换机名", DynVSwitches,
					K("detail", "类型、成员端口、VLAN/VRF 配置"),
					K("ports", "成员端口及状态/计数"),
					K("mac-table", "MAC 学习表（仅 L2）"),
					// 决策 #359：DHCP 服务器租约表（运行态；未配置 dhcp-server 时如实报未配置）。
					K("dhcp-leases", "DHCP 租约表（仅配置了 dhcp-server 的交换机）"),
					K("statistics", "每端口收发计数"),
				),
			),
			K("vrfs", "L3 虚拟交换机（VRF）",
				P("<name>", "VRF 名", DynVrfs,
					K("routes", "FIB 路由表"),
				),
			),
			K("acls", "ACL（含命中计数）",
				P("<name>", "ACL 名", DynAcls,
					K("detail", "规则与绑定详情"),
				),
			),
			K("nat", "NAT 池、规则与转换会话计数"),
			// 决策 #383：VXLAN 隧道读视图（配置声明 × 数据面实况，与 GET /vxlan-tunnels 同源）。
			K("vxlan", "VXLAN overlay",
				K("tunnels", "隧道读视图（名/VNI/下垫地址/端口/交换机/是否已在 VPP）"),
			),
			K("port-mirroring", "SPAN 会话状态"),
			// 决策 #345：数据面 DNS 代理读视图（启用态 + 全局上游 + 各域覆盖，与 GET /dns/proxy 同源）。
			K("dns", "DNS",
				K("proxy", "数据面 DNS 代理（启用态 + 全局上游 + 各域覆盖）"),
			),
			K("qos", "限速策略",
				K("policies", "策略与绑定列表"),
			),
			K("vpp", "VPP 数据面",
				K("threads", "main/worker 线程清单与绑核"),
				K("runtime", "每线程向量率/指令周期",
					Opt(K("thread", "指定线程", PT("<id>", "uint", "线程 ID"))),
				),
				K("buffers", "buffer 池（每 NUMA）使用量"),
				K("memory", "main-heap 与 hugepage 占用"),
				K("capture", "抓包会话状态与已导出 pcap 清单"),
			),
			K("bonds", "链路聚合列表",
				P("<name>", "bond 名", "",
					K("detail", "成员口各自 link/LACP actor-partner 信息"),
				),
			),
			K("lldp", "LLDP",
				K("neighbors", "邻居表",
					Opt(K("interface", "按接口过滤", P("<ifname>", "接口名", DynVppIfnames))),
				),
			),
			// `show protocols lldp neighbors` 与 `show lldp neighbors` 是同一读物的两种写法
			// （契约 §1.1，决策 #153）：执行器早有该分支、树里没有 → `?`/Tab 补不出 `protocols`。
			K("protocols", "协议运行态",
				K("lldp", "LLDP",
					K("neighbors", "邻居表（等价于 show lldp neighbors；同一读物）"),
				),
			),
			K("virtual-machine-functions", "VM VNF",
				P("<name>", "VNF 名", DynVMs,
					K("detail", "域 XML 摘要、资源分配、NUMA"),
					K("interfaces", "vNIC：类型/MAC/socket/交换机"),
					K("statistics", "vhost-user 口计数（经 VPP）"),
					K("snapshots", "快照列表"),
				),
			),
			K("container-functions", "容器 VNF",
				P("<name>", "容器名", DynContainers,
					K("detail", "容器详情"),
					K("interfaces", "memif vNIC 列表"),
				),
			),
			K("images", "镜像仓库",
				P("<name>", "镜像名", DynImages,
					K("detail", "类型/大小/sha256/引用计数"),
				),
			),
			K("resource-pools", "资源池：总量/已分配/空闲（含 vpp-reserved）"),
			K("alarms", "告警列表",
				Opt(VE("state", "告警状态过滤", "active", "all")),
			),
			K("log", "日志",
				K("system", "系统日志",
					Opt(K("level", "按级别过滤", VE("level", "日志级别", "debug", "info", "warn", "error"))),
					Opt(K("last", "最近 N 条", PT("<n>", "uint", "条数"))),
				),
				K("audit", "审计日志",
					Opt(K("last", "最近 N 条", PT("<n>", "uint", "条数"))),
				),
				K("vnf", "VNF 控制台/事件日志",
					P("<name>", "VNF 名", DynVMs,
						Opt(K("last", "最近 N 条", PT("<n>", "uint", "条数"))),
					),
				),
			),
			K("users", "本地用户与 class"),
			K("configuration", "配置显示",
				K("candidate", "当前持锁会话的 candidate"),
				K("history", "提交历史快照：rev/时间/用户/注释/是否当前（不含配置正文）"),
				// 决策 #304：`permissions <class> [detail]` 的「按 class 视角显示」落地为
				// **生效权限视图**（逐路径判定 + 依据；判定单源在 internal/aaa）。这里的
				// 描述如实说明它给的是什么，`?` 菜单同样是面向操作者的承诺。
				K("permissions", "某 class 的生效权限视图（逐路径判定；权限 R）",
					P("<class>", "class 名", DynClasses,
						Opt(K("detail", "逐路径附带判定依据")),
					),
				),
				// 契约 §1.1/§3（决策 #153）：`show configuration sessions` 等价于
				// `show system configuration sessions`（同一读物）；`show configuration compare
				// rollback <n>` 等价于管道形态 `| compare rollback <n>`。两者执行器都支持，
				// 此前树里没有 → `?`/Tab 补不出来。
				K("sessions", "candidate 持锁会话列表（等价于 show system configuration sessions）"),
				K("compare", "与历史快照比对（等价于管道形态 | compare rollback <n>）",
					K("rollback", "回滚点比对",
						PT("<n>", "uint", "历史快照编号"),
					),
				),
			),
			K("tech-support", "诊断包清单预览"),
		),
		Op(K("request", "运维动作",
			K("virtual-machine-functions", "VM VNF 操作",
				P("<name>", "VNF 名", DynVMs,
					K("start", "启动"),
					K("stop", "停止（ACPI 关机，超时强杀）"),
					K("restart", "重启"),
					K("console", "进入串口（Ctrl-] 退出）"),
					K("snapshot", "快照操作",
						K("create", "创建快照", Opt(K("name", "快照名", PT("<name>", "name", "名称")))),
						K("rollback", "回滚快照", Opt(K("name", "快照名", PT("<name>", "name", "名称")))),
						K("delete", "删除快照", Opt(K("name", "快照名", PT("<name>", "name", "名称")))),
					),
					Su(K("delete", "删除 VNF（级联 vNIC/VPP 端口/快照）")),
				),
			),
			K("container-functions", "容器 VNF 操作",
				P("<name>", "容器名", DynContainers,
					K("start", "启动"),
					K("stop", "停止"),
					K("restart", "重启"),
					K("log", "容器 stdout/stderr",
						Opt(K("last", "最近 N 条", PT("<n>", "uint", "条数"))),
					),
					// 决策 #357：容器内执行命令（非交互）。**S 档**：与 VM 串口 console 的关键差别是
					// console 进 guest 串口仍需 guest 凭据，而 exec 是免凭据的容器内命令执行（等价 root）；
					// operator 本不能创建容器（配置模式 S），若 exec 为 O 即等于绕过该限制。
					Su(K("exec", "在容器内执行命令（非交互；含空格请加引号）",
						P("<command>", "命令", ""),
						Opt(K("timeout", "超时秒数（1..300，缺省 30）", PT("<n>", "uint", "秒"))),
					)),
					// 决策 #358：容器交互式终端（与 VM 串口 console 同一套 ticket + WS 管线）。
					// 同 exec 为 **S 档**：免凭据的容器内命令执行＝等价 root。
					Su(K("shell", "交互式终端（进容器里的 sh；Ctrl-] 退出）")),
					Su(K("delete", "删除容器")),
				),
			),
			K("images", "镜像管理",
				K("upload", "从 /data/incoming/ 导入",
					K("name", "镜像名", PT("<name>", "name", "名称")),
					K("type", "镜像类型", VE("type", "vm-image|container-image", "vm-image", "container-image")),
					K("file", "incoming 内的文件路径", V("path", "路径")),
				),
				K("download", "从 HTTP(S) URL 拉取（sha256 校验/断点续传）",
					K("name", "镜像名", PT("<name>", "name", "名称")),
					K("type", "镜像类型", VE("type", "vm-image|container-image", "vm-image", "container-image")),
					K("url", "下载地址", V("url", "URL")),
					// sha256 **必填**（决策 #153 收口）：`images.ValidateDownloadOptions` 在受理前
					// 同步强制要求（默认强制校验，缺省即拒），契约 §1.2 与《命令全表》也写必填——
					// 树里标 `Opt` 会让 `?`/Tab 告诉操作者「可以不给」，照敲却被拒。
					K("sha256", "校验和", V("hex", "十六进制")),
				),
				Su(K("delete", "删除镜像（引用检查后）",
					// 键值形态 `delete name <n>`（非位置参数）：执行器 `imagesDelete` 按
					// `kvArgs(rest, "name")` 解析，契约 §1.2 与《命令全表》同写 `delete name <n>`。
					K("name", "镜像名", P("<name>", "镜像名", DynImages)),
				)),
			),
			K("interfaces", "接口启停与驱动接管",
				// 候选取 VPP ∪ 内核：同层的 enable/disable 作用于数据面口（VPP），
				// bind-dpdk 作用于尚未接管的内核口，unbind-dpdk 常需 PCI 地址——
				// 参数位置在动作之前、无法按动作区分来源，故取并集（决策 #83）。
				P("<ifname>", "接口名", DynIfnames,
					K("enable", "启用"),
					K("disable", "禁用"),
					K("bind-dpdk", "绑定到 DPDK 驱动（缺省 vfio-pci；中断流量，需确认）",
						Opt(K("uio-driver", "用户态驱动", VE("driver", "驱动", "vfio-pci", "igb-uio"))),
					),
					K("unbind-dpdk", "解绑并交还内核驱动（中断流量，需确认）",
						Opt(K("to-driver", "交还的内核驱动（缺省交内核自动探测；实测常需显式给出）", V("string", "如 vmxnet3"))),
					),
				),
			),
			K("sriov", "SR-IOV VF 管理",
				K("create-vfs", "在物理口上创建 VF",
					P("<ifname>", "物理口", DynKernelIfnames),
					K("count", "VF 数量", PT("<uint>", "uint", "数量")),
				),
				K("delete-vfs", "回收 VF",
					P("<ifname>", "物理口", DynKernelIfnames),
					K("vf", "VF 编号", PT("<uint>", "uint", "VF ID")),
				),
			),
			Su(K("vpp", "VPP 数据面操作",
				K("restart", "按 committed 配置重建数据面并恢复收敛"),
				K("trace", "数据面抓包",
					K("start", "开始抓包",
						K("interface", "抓包接口", P("<ifname>", "接口名", DynVppIfnames)),
						Opt(K("count", "达到报文数自动停止", PT("<n>", "uint", "报文数"))),
						Opt(K("filter", "ACL 过滤", P("<acl>", "ACL 名", DynAcls))),
					),
					K("stop", "停止抓包"),
					K("export", "导出 pcap 到诊断目录", Opt(K("name", "文件名", PT("<name>", "name", "名称")))),
				),
			)),
			K("system", "系统操作",
				// 决策 #146：kernel apply/rollback 会改写 GRUB 启动参数（需重启生效）——命令树契约
				// （§3 该行注 S）与《命令全表》都写 S，此前代码树漏了 Su() 标记，运行期按 request 域
				// 的 O 级放行（operator 能写启动项）。此处补上，与契约一致。
				Su(K("kernel", "内核启动基线",
					K("apply", "按 committed 配置写入 GRUB 基线（需重启生效）"),
					K("rollback", "回退上一次内核基线（需重启生效）"),
				)),
				// 决策 #329：回收空闲的多余大页，收敛到**已声明**值（在用页不动、不改声明值）。
				// 无主占用页不可回收（#346 真机实测撤回），只作可见性告警。
				// 与 kernel apply/rollback 同档（S）：都写宿主机内核侧状态。
				Su(K("hugepages", "大页池回收",
					K("reclaim", "回收空闲的多余页，收敛到声明值（在用页不动；不改声明值）"),
				)),
				Su(K("software", "软件升级",
					K("add", "安装 deb 包/URL",
						PT("<deb>", "path", "deb 包路径或 URL"),
						Opt(K("sha256", "校验和", V("hex", "十六进制"))),
					),
					K("rollback", "回退到上一版本", Opt(K("to", "指定版本", V("string", "版本号")))),
				)),
				Su(K("reboot", "重启系统")),
				Su(K("shutdown", "关机")),
				Su(K("poweroff", "断电")),
				Su(K("configuration", "配置备份与恢复",
					K("backup", "导出 committed 配置归档", Opt(K("to", "目标路径", V("path", "路径")))),
					K("restore", "导入归档为 candidate 并提交", PT("<path>", "path", "归档路径")),
				)),
				K("tech-support", "诊断归档",
					K("generate", "生成诊断归档 tar.gz"),
				),
				K("core-dumps", "崩溃转储管理",
					K("export", "导出到 URL", V("url", "URL")),
					K("delete", "删除转储", Opt(K("file", "指定文件", PT("<name>", "name", "文件名")))),
				),
				Su(K("zeroize", "恢复出厂（双重确认）")),
				// 决策 #301：S 不再压在 api 域节点上——tls regenerate 保持 S（写证书是敏感动作），
				// token revoke 为 R（任何登录 class 可执行；super-user 吊销任意、其他 class 仅自己的，
				// 数据范围判定在 aaa 实现处，与 show system api tokens 同一权限矩阵）。
				K("api", "API 服务管理",
					Su(K("tls", "TLS 证书",
						K("regenerate", "重签自签证书"),
					)),
					K("token", "Token 管理",
						K("revoke", "吊销指定会话（super-user 任意、其他仅自己的）", PT("<token-id>", "name", "会话 ID")),
					),
				),
				Su(K("ssh", "SSH 管理",
					K("host-key", "Host Key",
						K("regenerate", "重新生成"),
					),
				)),
				K("password", "口令",
					K("change", "登录者自助改密（验证旧口令）"),
				),
				Su(K("storage", "存储管理",
					K("format-data", "重置数据分区：恢复出厂数据状态（保留管理面可达）"),
				)),
				K("ntp", "时间同步",
					K("sync", "立即触发一次 NTP 同步"),
				),
			),
			K("alarms", "告警管理",
				K("clear", "清除已 resolved 告警",
					Opt(K("id", "指定告警", PT("<id>", "name", "告警 ID"))),
					Opt(VE("scope", "清除范围", "all")),
				),
			),
		)),
		Op(K("wizard", "初始化向导（CLI 端交互式：问答规划资源池与内核基线并提交；非 TTY 不可用）")),
		Su(K("configure", "进入配置模式（仅 super-user）")),
		Op(K("ping", "连通性测试", pingOperArgs()...)),
		Op(K("traceroute", "路径跟踪", tracerouteOperArgs()...)),
		Op(K("monitor", "实时监控",
			K("interfaces", "实时刷新接口计数（Ctrl-C 退出）",
				P("<ifname>", "接口名", DynVppIfnames),
				Opt(K("interval", "刷新间隔秒", PT("<sec>", "uint", "秒"))),
			),
			K("vnf", "跟踪 VNF 状态/事件（Ctrl-C 退出）",
				P("<name>", "VNF 名", DynVMs),
			),
		)),
		Su(K("clear", "清除",
			K("interfaces", "接口",
				K("statistics", "清零统计计数", Opt(P("<ifname>", "接口名", DynVppIfnames))),
			),
		)),
		Su(K("start", "启动",
			K("shell", "进入系统 shell（仅本地控制台，SSH 禁用）"),
		)),
		K("help", "帮助", Opt(PT("<command>", "string", "命令"))),
		K("exit", "退出 CLI"),
		K("quit", "退出 CLI"),
	)
	finalize(root, nil)
	return root
}
