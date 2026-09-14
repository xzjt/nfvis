# NFViS CLI 完整命令树设计

| 文档属性 | 内容 |
|---|---|
| 版本 | V1.0 |
| 上游文档 | 《NFViS 系统产品需求与目标架构需求规格书》V1.0 |
| 日期 | 2026-09-12 |

## 0. 阅读约定

- `<>` 必填参数，`[]` 可选，`...` 可重复；参数类型：`name`=标识符（字母数字`-_.`，≤64字符）、`ifname`=接口名、`uint`、`ip-prefix`（CIDR）、`ip`、`mac`、`vlan`（1-4094）、`string`=自由文本。
- 权限列：S=super-user，O=operator，R=read-only（`O` 含 R 能力，`S` 含全部）。
- API 列为该命令消费的端点（与《OpenAPI 草案》对应）。
- 补全列：`?`/Tab 在该位置可提供的动态候选来源。

---

## 1. 操作模式命令树（提示符 `nfvis>`）

### 1.1 `show`（查询，R）

```
show version                                        # 版本汇总（nfvis/ubuntu/vpp/dpdk/libvirt/qemu/docker）
                                                    # API: GET /system/version

show system
  ├─ uptime                                         # API: GET /system/status
  ├─ cpu                                            # 总核/隔离核/每核占用
  ├─ memory                                         # 内存与大页使用（池内/池外）
  ├─ storage                                        # 磁盘与镜像仓库占用
  ├─ hugepages                                      # 大页内核参数与池状态
  ├─ kernel                                         # 内核启动基线三方对照（cmdline/运行实际/配置期望，FR-SYS-014）
  ├─ hardware                                       # 硬件健康：CPU 温度/风扇/电源（IPMI/Redfish/lm-sensors）、磁盘 SMART
  ├─ core-dumps                                     # 崩溃转储清单（VPP/QEMU/nfvisd）
  ├─ tech-support                                   # 诊断归档清单
  └─ configuration sessions                        # candidate 持锁会话列表

show interfaces                                     # 全部接口摘要（API: GET /interfaces）
show interfaces physical                            # DPDK 物理口（驱动、链接状态、速率、VF 数）
show interfaces physical <ifname>
  ├─ detail                                         # 驱动、MAC、MTU、队列、NUMA
  ├─ statistics                                     # 收发包/字节/错误/drop（实时 stats）
  └─ sriov                                          # VF 列表与占用状态
show interfaces management                          # 管理口（内核侧，IP/链路）

show virtual-switches                               # 全部虚拟交换机摘要（GET /virtual-switches）
show virtual-switches <name>
  ├─ detail                                         # 类型、成员端口、VLAN/VRF 配置
  ├─ ports                                          # 成员端口及状态/计数
  ├─ mac-table                                      # MAC 学习表（仅 L2；govpp bridge-domain-dump）
  └─ statistics                                     # 每端口收发计数

show vrfs                                           # GET /vrfs
show vrfs <name>                                    # detail：L3 接口、地址、路由数
show vrfs <name> routes                             # FIB 路由表（govpp vrf dump）

show acls                                           # GET /acls（含命中计数）
show acls <name> detail
show nat                                            # NAT 池、规则、转换会话计数
show port-mirroring                                 # SPAN 会话状态
show qos policies                                   # 限速策略与绑定

show vpp                                            # VPP 数据面概览：版本、线程/worker、buffer、内存（GET /vpp/status）
show vpp threads                                    # main/worker 线程清单与绑核（govpp threads dump）
show vpp runtime [thread <id>]                      # 每线程向量率/指令周期/clock（govpp runtime）
show vpp buffers                                    # buffer 池（每 NUMA）使用量；打印统计来源
                                                    #   （statsclient | vpp_get_stats，决策 #68），
                                                    #   不可用时打印来源与原因（不静默省略）
show vpp memory                                     # main-heap 与 hugepage 占用
show vpp capture                                    # 抓包会话状态与已导出 pcap 清单

show bonds                                          # 链路聚合列表（成员口、LACP 状态、聚合口聚合状态）
show bonds <name> detail                            # 成员口各自 link/LACP actor-partner 信息
show lldp neighbors [interface <ifname>]            # LLDP 邻居表（chassis/port/系统名/TTL）

show virtual-machine-functions                      # VM 列表（名称/状态/vCPU/内存/镜像）（GET /virtual-machine-functions）
show virtual-machine-functions <name>
  ├─ detail                                         # 域 XML 摘要、资源分配、NUMA
  ├─ interfaces                                     # vNIC：类型、MAC、vhost-user socket/VF、虚拟交换机
  ├─ statistics                                     # vhost-user 口计数（经 VPP）
  └─ snapshots                                      # 快照列表
show container-functions                            # 容器列表（GET /container-functions）
show container-functions <name> [detail|interfaces]

show images                                         # 镜像仓库列表（GET /images）
show images <name> detail                           # 元数据：类型/大小/sha256/引用计数
show resource-pools                                 # 大页池/隔离核：总量、已分配、空闲（GET /resource-pools）

show alarms [active|all]                            # GET /alarms
show log
  ├─ system [level <debug|info|warn|error>] [last <n>]
  ├─ audit [last <n>]                               # GET /audit-logs
  └─ vnf <name> [last <n>]                          # VNF 控制台/事件日志
show users                                          # 本地用户与 class
show configuration [permissions <class>]            # 当前 committed 配置（下详 §3）
show tech-support                                   # 诊断包清单预览（日志+版本+配置+状态）

# 通用管道（所有 show 输出可用）：
#   | match <regex> | except <regex> | count | last <n> | begin <regex>
#   | display xml | display json
```

### 1.2 `request`（运维动作，O；破坏性动作为 S）

```
request virtual-machine-functions <name>
  ├─ start                                          # POST /vmf/{n}:start
  ├─ stop                                           # POST /vmf/{n}:stop
  ├─ restart
  ├─ console                                        # 进入串口（Ctrl-] 退出；POST /vmf/{n}/console）
  ├─ snapshot create|rollback|delete [name <name>]
  └─ delete                                         # S；CLI 交互确认 "Delete VNF 'x'? [yes,no]"
request container-functions <name>
  ├─ start | stop | restart
  ├─ log [last <n>]                                 # 容器 stdout/stderr
  └─ delete                                         # S；确认
request images
  ├─ upload name <name> type <vm-image|container-image> file <path>
  │      # path 须位于 /data/incoming/（先经 scp/sftp 传入管理网卡），导入成功自动清理
  ├─ download name <name> type <...> url <url> sha256 <hex>
  │      # URL 拉取必填 sha256（FR-SEC-004 默认强制校验，缺省即拒绝）
  └─ delete name <name>                             # 引用检查；确认
request interfaces <ifname> enable | disable         # PUT /interfaces/{n}
request interfaces <ifname> bind-dpdk [uio-driver <vfio-pci|igb-uio>]
request interfaces <ifname|pci> unbind-dpdk          # PUT /interfaces/{n}/dpdk（确认；FR-NET-001）
                                                     # 已由 DPDK 接管的网卡在内核中无 netdev，解绑须给 PCI 地址
                                                     # 绑定会中断该网卡现有流量，且该网卡不得正被 VPP 使用
request sriov create-vfs <ifname> count <uint> | delete-vfs <ifname> vf <uint>
request vpp restart                                 # S；确认。按 committed 配置重新生成 startup.conf 并重启 VPP，
                                                    # 随后 recovery 收敛重放网络配置、vhost-user 重连（影响业务转发）
request vpp trace
  ├─ start interface <ifname> [count <n>] [filter <acl>]   # 开始数据面抓包（达到报文数自动停止）
  ├─ stop                                            # 停止抓包
  └─ export [name <name>]                            # 导出 pcap 到诊断目录，供 API 下载
request system
  ├─ software add <deb包/URL> [sha256 <hex>]        # S；确认。校验→升级→重启 nfvisd→报告
  ├─ software rollback [to <version>]
  ├─ reboot | shutdown | poweroff                   # S；确认
  ├─ kernel apply | rollback                        # S；确认。按 committed 配置写 GRUB 基线/回退，需重启生效（FR-SYS-014）
  ├─ configuration backup [to <path>] | restore <path>   # S；确认
  ├─ tech-support generate                          # 生成诊断归档 tar.gz，CLI/API 下载
  ├─ core-dumps export <url> | delete [file <name>]
  ├─ zeroize                                        # S；双重确认，恢复出厂（FR-OPS-007）
  ├─ api tls regenerate                             # 重签自签证书（或经配置安装外部证书）
  ├─ ssh host-key regenerate                        # 重新生成 SSH host key
  ├─ password change                                # 登录者自助改密（验证旧口令）
  ├─ storage format-data                            # S；危险，双确认（V1 仅重置数据分区）
  └─ ntp sync
request alarms clear [id <id> | all]                # 确认后清除已 resolved 告警
request api token revoke <token-id>                 # S
```

> 实现说明（M4-12，附录 A #49~#51）：VNF/容器/镜像三条 `request` 族由 CLI 执行器**直连运行态接口**（与 `show` 族同源），不经自身 HTTP；动作成功/失败均入审计（FR-OPS-031），console 记打开/关闭两条（FR-OPS-032）。
> - `… delete` 交互确认：提问 `Delete VNF '<name>'? [yes,no]`（容器/镜像同格式），应答非 `yes` 即中止；确认后仍走与 HTTP 端点同一条删除路径（级联 vNIC/VPP 端口/快照）。非交互会话拒绝执行破坏性删除。
> - `… console`：申请一次性 ticket 后经 WebSocket 桥接串口，本地终端接管（Ctrl-] 退出）；非 TTY 环境不做终端接管并明确提示。
> - `request images download` **异步受理**：打印"已受理"，进度经 `show images <name> detail` 的 `import_state` 查看（事件流随 M5）。
> - `show images` / `show resource-pools` 分别读镜像仓库运行态与资源池账本视图，与对应 GET 端点输出同源。

### 1.3 其余操作命令

```
configure                                           # 进入配置模式（S/O；被 class 拒绝时提示）
exit | quit                                         # 退出 CLI
ping <host> [source <ip>] [count <n>] [vrf <name>]  # 经 VPP L3（vppctl ping；source 按接口地址反查接口）
traceroute <host> [vrf <name>]                      # 宿主侧 ICMP；vrf 经 VPP 路径不支持（明确报错）
monitor interfaces <ifname> [interval <sec>]        # 实时刷新计数，Ctrl-C 退出（CLI 端轮询）
monitor vnf <name>                                  # 跟踪 VNF 状态/事件
clear interfaces statistics [<ifname>]              # S
start shell                                         # S；仅 local console 允许（SSH 登录禁用）
help [command]
```

> 实现说明（M3-9，附录 A #36）：VPP 26.06 的 ping 插件仅提供 finished-event API、无发起接口，故 `ping` 经 `vppctl`（CLI socket）执行；`source <ip>` 经 VPP 接口地址反查接口名后作为 `vppctl ping source <iface>`。VPP 26.06 无 traceroute 插件/CLI/API，`traceroute` 由 nfvisd 宿主侧 raw ICMP 实现，`vrf` 参数在经 VPP 的路径上不支持并明确报错。`monitor interfaces` 服务端返回单次快照，nfvis-cli REPL 按 interval 本地轮询、Ctrl-C 退出。

---

## 2. 配置模式命令树（提示符 `nfvis#`，全部 S；class 授权到节点）

### 2.1 导航与事务（固定命令）

```
configure 后：  edit <path> | up | top | exit          # 层级导航，提示符显示 [edit path]
set / delete / show / annotate <path> "text"
commit [confirmed [minutes]] | commit check | commit and-quit
rollback [n]           # n 缺省=1；取历史快照为 candidate（需再 commit）
load override|merge <path>        # JSON 配置导入
save <path>                       # candidate 导出 JSON
run <oper-command>                # 配置模式内执行操作命令
discard | exit                    # discard 丢弃 candidate；exit 有未提交变更时提示确认
```

### 2.2 `system`

```
[edit system]
set hostname <string>
set timezone <tz>
set ntp server <ip|host> [prefer]
set dns server <ip> [secondary <ip>]
set api
  ├─ port <uint>                       # HTTPS 端口，默认 443
  ├─ token-ttl-minutes <uint>          # 默认 60
  ├─ max-sessions <uint>
  └─ tls
      ├─ cert-file <path> key-file <path>   # 安装外部证书（PEM），立即生效
      └─ self-signed regenerate             # 或重签自签证书
set management
  ├─ interface <ifname>                # 管理网卡（内核驱动；不得用于任何数据面，FR-NET-002/FR-SEC-001）
  ├─ ip address <ip-prefix>            # 独立管理网卡静态地址（IPv4/IPv6）
  └─ gateway <ip>                      # 管理口默认网关
set kernel                                    # 内核启动基线（大页/隔离核由 resource-pools 派生，唯一真源）
  ├─ nmi-watchdog <true|false>                # NMI watchdog（VPP 场景通常 false）
  ├─ transparent-hugepages <always|madvise|never>
  ├─ iommu <on|off|pt>
  ├─ tuned-profile <name>
  └─ params <param>                           # 附加内核参数（逃生口，可多条）
set health thresholds
  ├─ cpu-temp-celsius <uint> | disk-temp-celsius <uint>   # 硬件告警阈值（FR-SYS-012）
  └─ disk-used-percent <uint>                             # API: PUT /system/health/thresholds
set syslog
  ├─ host <ip> [port <uint>] [facility <facility>] [severity <severity>]
  └─ local
      ├─ level <debug|info|warn|error>
      ├─ retention-days <uint>                # 本地日志保留天数（FR-SYS-013）
      └─ max-size-mb <uint>                   # 本地日志容量上限，滚动覆盖
# 管理口地址/网关变更：commit 时若当前会话来自 SSH，强制要求使用
# commit confirmed 并输出自锁警告（FR-CFG-012）
set login
  ├─ user <name> password <string> class <class-name>
  ├─ class <name>                      # 自定义 class
  │   ├─ allow <command-path>          # 允许的命令树节点
  │   └─ deny <command-path>
  └─ password-policy
      ├─ min-length <uint> | complexity <bool> | expire-days <uint>
      └─ lockout-threshold <uint> lockout-minutes <uint>
set idle-timeout-minutes <uint>         # CLI 会话空闲超时
```

### 2.2b `protocols`（顶级层级）

```
[edit protocols lldp]                   # 独立顶级层级（非 system 子节点）
set enable <bool> | advertisement-interval <uint>
set interface <ifname> enable <bool>    # 按接口覆盖（基于 VPP lldp plugin）
# API: GET/PUT /protocols/lldp，邻居表 GET /protocols/lldp/neighbors
```

### 2.3 `interfaces` 与 `bonds`（物理口、链路聚合）

```
[edit interfaces]
set <ifname> description <string> | disable | mtu <uint>
set <ifname> sriov vf-count <uint>                   # 创建 VF（S）
delete <ifname> [sriov vf-count]
# 说明：驱动接管/解管由安装器与 recovery 维护，运行期不提供（避免管理面失联）

[edit bonds <name>]                                  # 链路聚合（VPP bonding plugin）
set members [<seq>] <ifname>                         # 成员物理口（须未被虚拟交换机引用）
set lacp mode <active|passive> [interval <fast|slow>] | lacp disable   # 缺省静态聚合
set mtu <uint> | description <string>
# bond 名 <name> 可在虚拟交换机端口、l3-interface、dpdk dev 等一切接受
# 接口名处引用，与物理口等价（FR-NET-017）
```

### 2.4 `virtual-switches`

```
[edit virtual-switches <name>]
set type <l2|l3>                                     # 类型创建后不可改
# —— L2 ——
set vlan access <vlan>                               # 交换机级默认 untag VLAN
set gateway ip <ip-prefix>                           # BVI 三层网关（IPv4/IPv6 可配多条，FR-NET-014）
set gateway vrf <name>                               # 网关所属 VRF；缺省为专属 VRF vr-<name>
set gateway acl-in <acl> | acl-out <acl>
set ports [<seq>] interface <ifname> [trunk vlans <vlan-list> | native <vlan>]
set ports [<seq>] vnf <vm-name> interface <vnic-name> [trunk vlans <vlan-list>]
set ports [<seq>] container <ct-name> interface <vnic-name>
set cross-connect <port-a> <port-b>                  # 两端口直通模式（与 ports/gateway 互斥）
# —— L3 ——
set l3-interface <ifname|vlan <v>> ip address <ip-prefix>    # IPv4/IPv6，可配多条
set static-routes <ip-prefix> next-hop <ip> [distance <uint>]  # 目的与下一跳支持 v4/v6
set static-routes default next-hop <ip>
```

### 2.5 高级网络功能

```
[edit acls <name>]
set rule <seq> source <ip-prefix|any> destination <ip-prefix|any> \
    protocol <tcp|udp|icmp|any> [source-port <port|range>] [destination-port <port|range>] \
    action <permit|deny>
set rule <seq> direction <ingress|egress>
# 绑定（在端口/接口下）：
#   set virtual-switches <n> ports <seq> acl-in <acl> / acl-out <acl>
#   set virtual-switches <n> l3-interface ... acl-in <acl>

[edit nat]
set source-pool <name> address-range <ip> to <ip>    # 外部地址池（可选；未用时以出接口地址作外部地址）
set rules <seq> match source <ip-prefix> virtual-switch <name> \
    action interface <ifname> [source-pool <name>]   # 出接口必填（决策 #38/#52）
set static <inside-ip> to <outside-ip>               # 1:1 发布
# inside 转发域由 virtual-switch（须 l3）派生；outside 转发域由出接口所属 VRF 派生
# （出接口须为某 l3 交换机的 l3-interface 且已配地址）。两者可同表或跨 VRF，
# 但 VPP NAT44 单实例仅一对 (inside, outside)，故多规则的 virtual-switch / 出接口 VRF 必须各自一致（决策 #52）

[edit port-mirroring <name>]
set source interface <ifname|vnf <vm> interface <vnic>> direction <ingress|egress|both>
set analyzer interface <ifname>

[edit qos]
set policies <name> cir <uint> cbs <uint>            # bps / bytes
# 绑定：set interfaces <ifname> ingress-policy <name>
```

### 2.6 `resource-pools`

```
[edit resource-pools]
set hugepages page-size <2M|1G> count <uint>         # 变更需 reboot，commit 时提示
set cpu isolated-cores <core-list>                   # 如 "4-15"；变更需 reboot
set cpu numa node <uint> cores <core-list>           # NUMA 亲和声明（校验与拓扑一致）
```

### 2.7 `virtual-machine-functions`

```
[edit virtual-machine-functions <name>]
set image <image-name>                               # 引用 show images 中的 vm-image
set vcpu count <uint> [pin <bool>]                   # 从隔离核池分配；pin 默认 true
set memory
  ├─ size-mb <uint>                                  # 从大页池分配
  ├─ hugepage-size <2M|1G>                           # 指定从哪个大页池分配（默认取资源池主池）
  ├─ numa node <uint>                                # 可选；校验与 vcpu NUMA 一致
  └─ backing <hugepage|normal>                       # 默认 hugepage；normal 时禁止 vhost-user vNIC
set disks <disk-name> size-gb <uint>                 # 附加数据盘（virtio，空盘，FR-CMP-018）
set disks <disk-name> image <image-name>             # 或从 vm-image 克隆创建
delete disks <disk-name>
set interfaces <vnic-name>
  ├─ type <vhost-user|sriov-vf>                      # sriov-vf 需指定物理口与 VF 号
  │     set sriov physical-interface <ifname> vf <uint>
  ├─ mac <mac>                                       # 可选，缺省自动生成
  ├─ vlan <vlan>                                     # 可选 tag
  └─ virtual-switch <name>                           # 引用 L2 交换机；sriov-vf 时仅做登记
set cloud-init
  ├─ user-data <path|string>                         # YAML 文本或文件
  ├─ ssh-key <string>                                # 可多条
  └─ hostname <string>
set serial console enable                            # 默认启用
set autostart <bool>
set description <string>
```

### 2.8 `container-functions`

```
[edit container-functions <name>]
set image <image-name>                               # container-image
set vcpu count <uint> | memory size-mb <uint>        # cgroup 限制（大页可选，memif 需共享内存时必配）
set interfaces <vnic-name> type memif virtual-switch <name> [mac <mac>] [vlan <vlan>]
set env <key> <value> | command <string> | args <string>
set restart-policy <no|on-failure>
set autostart <bool>
set description <string>
```

### 2.9 `vpp`（VPP 数据面运行时配置）

```
[edit vpp]
# 语义：nfvisd 依据本层级生成 VPP startup.conf（受事务引擎管理，可 compare/rollback）。
# cpu / memory / dpdk 变更需重启数据面生效：commit 成功但输出警告
#   "%% 警告: vpp 变更需 request vpp restart（或整机 reboot）后生效"，
# 并产生 warning 告警直至数据面按新配置重启（FR-SYS-009）。

set cpu main-core <uint>                             # 主线程绑核（必须位于隔离核池内）
set cpu corelist-workers <list>                      # worker 核列表，如 "5,7,9-11"（同上）
set cpu workers-per-numa <uint>                      # 可选：按 NUMA 分配 worker（与 corelist-workers 互斥）

set memory main-heap-size <size>                     # 主堆，如 1G（默认 1G）
set memory buffers-per-numa <uint>                   # 每 NUMA buffer 数（默认 16385，按吞吐调优）
set memory hugepage-preference <2M|1G>               # 与 resource-pools hugepages 页大小一致（commit 校验）

set dpdk dev rx-queues <uint> | tx-queues <uint> | rx-descriptors <uint> | tx-descriptors <uint>
                                                     # 全局默认值，作用于所有未单独配置的物理 NIC
set dpdk dev <ifname> rx-queues <uint> | tx-queues <uint> | rx-descriptors <uint> | tx-descriptors <uint>
                                                     # 单网卡覆盖；<ifname> 须为 DPDK 接管的物理口（commit 校验），
                                                     # 未覆盖的参数继承全局默认
delete dpdk dev <ifname>                             # 删除该网卡全部覆盖项，整体回落全局默认
delete dpdk dev <ifname> rx-queues                   # 仅删除单项，该参数回落全局默认
set dpdk uio-driver <vfio-pci|igb-uio>               # 安装器默认 vfio-pci，一般不改

set plugins <name> state <enable|disable>            # 插件开关：acl/nat/span/dpdk/linux-cp...
delete plugins <name>                                # 恢复默认（启用）
delete cpu | memory | dpdk                           # 整组恢复默认值
```

**与资源池的联动约束（commit 语义校验）**：

1. `vpp cpu main-core/corelist-workers` 的核必须包含于 `resource-pools cpu isolated-cores`，否则 commit 失败；
2. VPP 保留核与 VNF vCPU 绑核互斥：资源池账本先扣减 VPP 保留核，剩余才是 VNF 可分配额度（`show resource-pools` 中单独展示 "vpp-reserved"）；
3. `vpp memory hugepage-preference` 必须与 `resource-pools hugepages page-size` 一致；
4. `dpdk dev <ifname>` 的 `<ifname>` 必须是 DPDK 接管的物理口（`show interfaces physical` 中的接口），否则 commit 校验失败；
5. 修改 worker 核数后，各 NIC 的 `rx-queues` 应相应调整（建议值 = worker 数，RSS 按队列散列到 worker），不强制校验但 commit 输出提示。生效值 = 单网卡覆盖 > 全局默认 > VPP 默认。

### 2.10 `show configuration` 输出样例（JunOS 风格）

```
system {
    hostname nfvis-node1;
    ntp {
        server 10.0.0.1 prefer;
    }
    management {
        ip address 192.168.1.10/24;
        gateway 192.168.1.1;
    }
}
vpp {
    cpu {
        main-core 2;
        corelist-workers 5,7;
    }
    memory {
        hugepage-preference 1G;
    }
    dpdk {
        dev rx-queues 2;
    }
    plugins {
        acl state enable;
        nat state enable;
    }
}
virtual-switches {
    vs-app {
        type l2;
        vlan access 100;
        gateway {
            ip 192.168.100.1/24;
        }
        ports {
            1 interface ens2f0;
            2 vnf fw-vm interface eth0;
        }
    }
    vs-underlay {
        type l3;
        l3-interface vlan100 {
            ip address 10.10.0.1/24;
        }
        static-routes 0.0.0.0/0 next-hop 10.10.0.254;
    }
}
virtual-machine-functions {
    fw-vm {
        image ubuntu22-vm;
        vcpu count 4;
        memory {
            size-mb 8192;
            backing hugepage;
        }
        disks {
            data1 size-gb 100;
        }
        interfaces eth0 {
            type vhost-user;
            virtual-switch vs-app;
        }
        cloud-init {
            ssh-key "ssh-ed25519 AAAA...";
        }
    }
}
```

---

## 3. 配置显示语义（对照表）

| 命令上下文 | 显示对象 |
|---|---|
| 操作模式 `show configuration` | **committed** 配置 |
| 操作模式 `show configuration candidate` | 当前持锁会话的 candidate |
| 操作模式 `show configuration \| compare rollback <n>` | committed ⇄ 第 n 个历史快照 diff |
| 配置模式 `show` | candidate（当前层级） |
| 配置模式 `show \| display set` | 以 `set` 语句展开显示（便于复制） |
| 配置模式 `show \| compare` | candidate ⇄ committed diff |

## 4. class 权限矩阵（预置）

| 命令域 | super-user | operator | read-only |
|---|---|---|---|
| `show *` | ✔ | ✔ | ✔ |
| `configure`（配置模式全部） | ✔ | ✘ | ✘ |
| `request`（生命周期/镜像/接口） | ✔ | ✔ | ✘ |
| `request`（software/reboot/configuration） | ✔ | ✘ | ✘ |
| `clear` / `start shell` | ✔ | ✘ | ✘ |

## 5. 补全行为细则（供补全引擎实现）

1. 任意位置输入 `?`：列出当前 token 位置所有候选（关键字=名称+描述；参数=类型提示+动态值来源），并回显已输入部分。若 `?` 前有部分字符，只列以此为前缀的候选。
2. Tab：唯一匹配→补全并附空格；唯一匹配但需更多字符（如接口名前缀）→补全到公共前缀；多匹配→响铃并列出（与 `?` 同）。
3. 动态候选来源（实时向 nfvisd 查询，失败则退化为仅关键字）：`<ifname>`→接口清单、`<name>`→对应资源清单、`<image-name>`→镜像清单、`<class-name>`→class 清单。
4. 配置模式下 `?` 还会提示当前 `[edit]` 层级下可 `set/delete` 的直接子节点。
5. 命令缩写：无歧义前缀即合法（`sh vi` = `show virtual-machine-functions` 前缀匹配按树节点逐级消歧）。
