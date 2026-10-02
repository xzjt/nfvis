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
  ├─ hugepages                                      # 大页池数字：声明 / 内核实际 / 在用 / **实际持有** /
  │                                                 #   **无主占用**（决策 #329 起、#346 细分）：
  │                                                 #   在用 ≠ 有持有者——内核收缩池、或被进程**预留（reserve）未
  │                                                 #   fault** 的页（如数据面 DPDK 预留）都算「在用」却无可见持有者；
  │                                                 #   这类页只作**可见性**（含 `HUGEPAGE_POOL_ORPHAN` 告警），产品侧
  │                                                 #   **不可回收**（写 nr_hugepages 释放不了）；持有值取不到就如实说取不到
  ├─ kernel                                         # 内核启动基线三方对照（cmdline/运行实际/配置期望，FR-SYS-014）
  ├─ hardware                                       # 硬件健康：CPU 温度/风扇/电源（IPMI/Redfish/lm-sensors）、磁盘 SMART
  ├─ core-dumps                                     # 崩溃转储清单（VPP/QEMU/nfvisd）
  ├─ tech-support                                   # 诊断归档清单
  ├─ api tokens                                     # 活动会话 / API Token 清单（R；决策 #301）：token-id、
  │                                                 #   用户、class、签发、过期、是否当前会话；
  │                                                 #   super-user 列全部、其他 class 只列自己的
  │                                                 #   API: GET /system/api-tokens
  └─ configuration sessions                        # candidate 持锁会话列表（**此处只有 sessions**：
                                                    #   `candidate` 的写法是顶层 `show configuration candidate`，
                                                    #   唯一的实现也在那里；`show system configuration candidate`
                                                    #   不存在，别在树里再加一份重复且无实现的形态——决策 #153）
                                                    #   **列 Holder/Session/User/Acquired/Last-Activity/Dirty**——
                                                    #   会话标识与所属用户如实列出、不同会话不合并（决策 #317）

show interfaces                                     # 接口运行态清单：行 = 配置声明 ∪ VPP 运行态口 ∪ 内核未接管口（决策 #155；
                                                    #   Admin/Link/Speed/Driver/计数全取运行态——#84 字段级残留收口；
                                                    #   仅声明未生效的行状态列 - 并标注，纯运行态口标注「未声明」；
                                                    #   内核侧未被 VPP 接管的物理口并入清单并标注「未接管」（决策 #302，
                                                    #   收口 round81 F1：首装在接管前也能看见网卡）——枚举口径：
                                                    #   /sys/class/net − 无 device 链接的虚拟口（lo/veth/docker0…，
                                                    #   lo 与 VPP 内置 local0 再防御性剔除）− VPP 已接管 − 配置已声明，
                                                    #   去重按名排序；未接管口不编造 VPP 侧事实，状态列 -）
show interfaces physical                            # **与上一条完全等价**（决策 #155：`physical` 选择器退役为等价写法）
show interfaces physical <ifname>
  ├─ detail                                         # 运行态单口视图（与裸写法同一实现）
  ├─ statistics                                     # 收发包/字节/错误/drop（实时 stats）
  └─ sriov                                          # VF 列表与占用状态
show interfaces <ifname> [detail|statistics|sriov]   # **≡ `show interfaces physical <ifname> …`（全形态等价、同一实现）**
                                                    #   运行态单口视图（Admin/Link/Speed/Driver/计数）。`?`/Tab 在此位置补
                                                    #   `<ifname>`（vpp-ifnames，决策 #83）：**候选里的名字必须答得上来**
                                                    #   （决策 #154/#155）——已声明的口回运行态视图（声明但运行态不可得 →
                                                    #   状态列 - 并注明），未声明但在清单里的口（派生口 bvi0/vh-*、未声明的
                                                    #   DPDK 口）同回运行态视图并注明；内核侧未接管的物理口（不在上述两侧，
                                                    #   决策 #302）回内核事实视图（驱动/MAC/速率/Admin/Link/MTU 取 sysfs，
                                                    #   注明「未被 VPP 接管」，不编造数据面统计）；三侧都不在（VPP 清单
                                                    #   查询成功才可判）→ `% 接口 … 未在配置中声明、也不在 VPP 接口清单中`。
                                                    #   接口的**配置视图**在配置模式：`configure` → `edit interfaces <ifname>`
                                                    #   → `show`（层级子树），或 `show configuration | display set`（#155）
show interfaces management                          # 管理口（内核侧，IP/链路）
                                                    #   ※ VM/容器家族例外（决策 #155 登记）：`show virtual-machine-functions
                                                    #   <name>` 保留「声明 + state 增补」合并视图——其运行态贫乏（仅 state），
                                                    #   拆分后信息净损失；配置子树同样可用配置模式层级 show 查看

show virtual-switches                               # 全部虚拟交换机摘要（GET /virtual-switches）
show virtual-switches <name>
  ├─ detail                                         # 类型、成员端口、VLAN/VRF 配置、DHCP 中继（决策 #335：
                                                    #   配置了 dhcp-relay 才显示「DHCP 中继」行，与 REST
                                                    #   GET /virtual-switches/{n} 的 dhcp_relay 同源）、
                                                    #   MAC 学习上限（决策 #337：配置了 learn-limit 才显示
                                                    #   「学习上限」行，与 REST 的 learn_limit 同源）
  ├─ ports                                          # 成员端口及状态/计数：配置静态 ports ∪ VNF/容器声明派生，
                                                    #   逐条标注 source（config|vnf|container|runtime，附录 A #326）；
                                                    #   与 REST GET /virtual-switches/{n}/ports 同源；派生条目只读
  ├─ mac-table                                      # MAC 学习表（仅 L2；govpp bridge-domain-dump）
  └─ statistics                                     # 每端口收发计数（同 ports 的读视图）

show vrfs                                           # GET /vrfs
show vrfs <name>                                    # detail：L3 接口、地址、路由数
show vrfs <name> routes                             # FIB 路由表（govpp vrf dump）

show acls                                           # GET /acls（含命中计数）
show acls <name> detail
show nat                                            # NAT 池、规则、转换会话计数
show port-mirroring                                 # SPAN 会话状态
show qos policies                                   # 限速策略与绑定
show dns proxy                                      # 数据面 DNS 代理（决策 #345）：启用态 + 全局上游 + 各域覆盖
                                                    #   （GET /dns/proxy；只读产品配置声明——vpp.dns_proxy_servers
                                                    #   与各 virtual-switches[].dns_proxy_servers）

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
                                                    #   `interface <ifname>` **真的按口过滤**（无匹配时
                                                    #   `（接口 X 无 LLDP 邻居）`；接口名不在配置/VPP 清单中时
                                                    #   按既有风格报「未在配置中声明」，不回全量）
show protocols lldp neighbors                       # **等价写法**（同一读物：`GET /protocols/lldp/neighbors`，
                                                    #   即 `show lldp neighbors` 的输出；两种写法都在树里，
                                                    #   执行器同源——`show protocols lldp neighbors` 直接委托前者）
                                                    #   注意：**过滤参数只声明在前者**，故这里写
                                                    #   `… neighbors interface <ifname>` 会报错并指向等价写法

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

show alarms [active|all]                            # GET /alarms；未同步时消息后标 [时钟未同步]（NFR-006）
show log
  ├─ system [level <debug|info|warn|error>] [last <n>]
  ├─ audit [last <n>]                               # GET /audit-logs；
  │                                                 # 未同步记录带「[时钟未同步]」标记（NFR-006）
  └─ vnf <name> [last <n>]                          # VNF 控制台/事件日志
show users                                          # 本地用户与 class
show configuration [permissions <class> [detail]] # 省略子命令 = 当前 committed 配置（下详 §3）
                                                    #   `permissions <class>` = **该 class 的生效权限视图**（决策 #304）：
                                                    #   把该 class 在命令树上的有效判定逐路径算出来（判定单源在 aaa，
                                                    #   与运行期授权同一实现）。默认按顶层命令族列出**允许路径**，
                                                    #   末行汇总（class、来源、允许/拒绝条数）；`detail` 逐路径附判定依据。
                                                    #   依据四类：预置等级满足 / allow 前缀命中 / deny 前缀命中 / 默认拒绝。
                                                    #   `| display set`：自定义 class 输出等价 `set system login class …` 语句；
                                                    #   预置 class 由等级判定、无路径表，如实说明不编造语句。
                                                    #   权限 R：read-only 仅可查自己所属 class；非 super-user 查他人一律拒绝
                                                    #   （不泄露他人规则），未知 class 明确报错。
show configuration candidate                        # 当前持锁会话的 candidate
show configuration sessions                         # candidate 持锁会话列表；**等价于 `show system configuration sessions`**
                                                    #   （同一读物：`Engine.Sessions`，与 `GET /system/configuration/sessions` 同源）
                                                    #   列 Holder/Session/User/Acquired/Last-Activity/Dirty；会话标识 = 持锁会话的
                                                    #   稳定 ID（决策 #317），与所属用户一并如实列出、不合并
show configuration history                          # 提交历史快照列表：rev/时间/用户/注释/是否当前
                                                    #   （GET /configuration/history；**不含配置正文**）
show configuration compare rollback <n>             # committed ⇄ 第 n 个历史快照 diff；**等价于管道形态
                                                    #   `show configuration | compare rollback <n>`**（同一 `Engine.Compare(n)`）
show configuration <其它 token>                     # **必须报错**：`show configuration` 的子命令只有
                                                    #   `candidate|history|permissions|sessions|compare rollback <n>`
                                                    #   （省略 = 读 committed）。**未知子命令不得静默返回 committed 配置正文**，
                                                    #   须回 `% 无效命令: show configuration <x>（可用：…）`（可操作提示，同 §1.1 其它族）
show tech-support                                   # 诊断包清单预览（日志+版本+配置+状态）

# 通用管道（所有 show 输出可用）：
#   | match <regex> | except <regex> | count | last <n> | begin <regex>
#   | display xml | display json | display set
#     display set：把配置（整树或 edit 层级子树）反推为逐行 `set` 语句——
#     语句带**绝对路径**、敏感值不输出（# 注释说明）、每行可独立回放；生成后经回放自校验
#     （语句回放进空配置必须还原原配置，不等即报内部错误，决策 #155）。
#     仅配置类输出可用（show configuration / 配置模式 show）；运行态 show 无配置可反推。
```

### 1.2 `request`（运维动作，O；破坏性动作为 S）

```
request virtual-machine-functions <name>
  ├─ start                                          # POST /vmf/{n}:start
  │      # 受理后在探测窗口内回读域状态（缺省 3s、可配置）；达到运行态即刻返回，
  │      # 落在 paused/crashed/shutoff 一类非预期态时报失败，并给出域状态 + reason、
  │      # libvirt 域日志摘录与恢复建议（如 request vpp restart 后重试 start）
  │      # 数据面（VPP）不可用时在进入会阻塞的 vhost-user 准备之前即判定，
  │      # 秒级报「数据面（VPP）当前不可用，未启动虚拟机」+ request vpp restart 指引（决策 #314）
  ├─ stop                                           # POST /vmf/{n}:stop
  ├─ restart                                        # 运行中 ACPI 重启；已关机的 off→start 分支同 start 数据面前置判定（决策 #314）
  ├─ console                                        # 进入串口（Ctrl-] 退出；POST /vmf/{n}/console）
  ├─ snapshot create|rollback|delete [name <name>]   # create/rollback 需关机态（运行中 409，决策 #75）
  └─ delete                                         # S；CLI 交互确认 "Delete VNF 'x'? [yes,no]"
request container-functions <name>
  ├─ start | stop | restart
  ├─ log [last <n>]                                 # 容器 stdout/stderr
  └─ delete                                         # S；确认
request images
  ├─ upload name <name> type <vm-image|container-image> file <path>
  │      # path 须位于 /data/incoming/（先经 scp/sftp 传入管理网卡），导入成功自动清理
  │      # 容器镜像：docker load 后按目录项名重打标签 `<name>:latest`；tar 内嵌 tag 记入
  │      #   source_tags 并在输出/详情里回显，配置里唯一的可用名就是 `<name>`
  ├─ download name <name> type <...> url <url> sha256 <hex>
  │      # URL 拉取必填 sha256（FR-SEC-004 默认强制校验，缺省即拒绝）；
  │      #   树里 **不得** 把 sha256 标成可选（`[...]`）——`?`/Tab 会据此告诉操作者「可以不给」，
  │      #   照敲却被校验层拒（树、执行器、校验三方同源，决策 #153）
  └─ delete name <name>                             # 引用检查；确认。
                                                    #   `name` 是**关键字**（键值形态 `delete name <n>`，
                                                    #   执行器按 key/value 解析）：不得写成位置参数
                                                    #   `delete <name>`（决策 #153）
request interfaces <ifname> enable | disable         # PUT /interfaces/{n}
request interfaces <ifname> bind-dpdk [uio-driver <vfio-pci|igb-uio>]
request interfaces <ifname|pci> unbind-dpdk [to-driver <驱动名>]
                                                     # PUT /interfaces/{n}/dpdk（确认；FR-NET-001）
                                                     # 已由 DPDK 接管的网卡在内核中无 netdev，解绑须给 PCI 地址
                                                     # 实测：清空 override + rescan 不足以让内核重新探测，
                                                     # 故建议带 to-driver（如 to-driver vmxnet3）
                                                     # 绑定会中断该网卡现有流量，且该网卡不得正被 VPP 使用
request sriov create-vfs <ifname> count <uint> | delete-vfs <ifname> vf <uint>
   # delete-vfs 的 vf <n> **不参与定位**：V1 的 VF 是数量型配置，按数量回收一个（回显会明确说明）
request vpp restart                                 # S；确认。按 committed 配置重新生成 startup.conf 并重启 VPP，
                                                    # 随后 recovery 收敛重放网络配置、vhost-user 重连（影响业务转发）
                                                    # nfvisd 启动时会自动确保 VPP 运行（未运行即发起拉起、不阻塞自身启动）并按 committed 配置重放——重启后数据面自动恢复；
                                                    # 但 VNF/容器需在配置里声明 `autostart true` 才会随系统自启，未声明则需手工 request … start
request vpp trace
  ├─ start interface <ifname> [count <n>] [filter <acl>]   # 开始数据面抓包（达到报文数自动停止）
  ├─ stop                                            # 停止抓包
  └─ export [name <name>]                            # 导出 pcap 到诊断目录，供 API 下载
request system
  ├─ software add <deb包/URL> [sha256 <hex>]        # S；确认。校验→升级→重启 nfvisd→报告
  ├─ software rollback [to <version>]
  ├─ reboot | shutdown | poweroff                   # S；确认
  ├─ kernel apply | rollback                        # S；确认。按 committed 配置写 GRUB 基线/回退，需重启生效（FR-SYS-014）
  ├─ hugepages reclaim                              # S。只回收**空闲**的多余大页，收敛到声明值（决策 #329）：
  │                                                 #   ① 回收「内核实际 > 声明且空闲」的多余页，**在用页一律不动**；
  │                                                 #   ② 写后**回读**内核实际值确认（写成功 ≠ 收敛）——回读与目标不一致如实报错，
  │                                                 #      无变化即如实报「无可回收的空闲多余页」；
  │                                                 #   ③ **不改声明值**——改声明是 set resource-pools hugepages … count <n>（需 reboot）；
  │                                                 #   ④ 自动收敛在既有 60s 巡检里做（对账式，不新造定时器），本命令是手动入口；
  │                                                 #   ⑤ ⚠️ **无主占用页（在用 > 实际持有）不在回收范围**（决策 #346 真机实测撤回）：
  │                                                 #      这类页多为被进程**预留（reserve）但未 fault** 的大页（如数据面 DPDK 预留），
  │                                                 #      **不在空闲链表上**，写 nr_hugepages **释放不了**；只能从预留者一侧释放。
  │                                                 #      持续存在时以 `HUGEPAGE_POOL_ORPHAN`（warning）如实告警，收敛后自动消解。
  │                                                 # API: POST /system/hugepages:reclaim（REST 侧同实现）
  ├─ configuration backup [to <path>] | restore <path>   # S；确认。to <path> 另存一份归档（0600）：
  │                                                 #   须绝对路径、目标不得已存在、父目录须已存在；
  │                                                 #   目标已存在即如实拒绝，不覆盖既有文件
  ├─ tech-support generate                          # 生成诊断归档 tar.gz，CLI/API 下载
  │                                                 #   （归档里的配置是脱敏视图：口令哈希等已隐藏，
  │                                                 #    不能用于恢复；要可恢复的完整配置用 configuration backup）
  ├─ core-dumps export <url> | delete [file <name>]
  ├─ zeroize                                        # S；双重确认，恢复出厂（FR-OPS-007）
  ├─ api
  │   ├─ tls regenerate                             # S；重签自签证书（或经配置安装外部证书）
  │   └─ token revoke <token-id>                    # O（request 域基线；决策 #301）；**在 api 之下**
  │                                                 # （决策 #76：原文档误置于顶级 request 下）。逐 token
  │                                                 # 吊销：super-user 可吊销任意会话、其他 class 仅自己的；
  │                                                 # 不存在的/他人的 token id 统一报「会话不存在或无权
  │                                                 # 操作」（不泄露存在性），吊销后该会话下一个请求即 401。
  │                                                 # REST 侧同能力以最低 class 开放（自服务例），read-only
  │                                                 # 的自助结束会话另有 POST /logout
  │                                                 # API: POST /system/api-tokens/{id}:revoke
  ├─ ssh host-key regenerate                        # 重新生成 SSH host key
  ├─ password change                                # 登录者自助改密（验证旧口令）
  ├─ storage format-data                            # S；双重确认（照搬 zeroize 口径），恢复出厂**数据状态**（决策 #305）
  │                                                 #   一句话：把受管数据恢复到出厂状态，但保证管理面仍然可达。
  │                                                 #   ① 收敛（复用事务引擎 applier 级联，不另写删对象逻辑）：
  │                                                 #     停并删全部受管容器/VNF（含 qcow2 内部快照）+ 全部网络配置
  │                                                 #     对象（交换机/VRF/静态路由/ACL/NAT/QoS/端口镜像/LAG(bond)/LLDP）。
  │                                                 #   ② 保留（保命条款）：system.management / system.api / system.login
  │                                                 #     三节逐字段原样保留；物理口声明 set interfaces … 与
  │                                                 #     set vpp dpdk dev …（vpp.dpdk）原样保留（绑定状态在 sysfs，不触碰）；
  │                                                 #     底座与身份（VPP/libvirt/docker、二进制、TLS 证书、SSH host key、
  │                                                 #     systemd 单元）一律不动。不保留 vpp.cpu/vpp.memory/resource-pools
  │                                                 #     （与保留节校验自相矛盾，属业务面容量配置）。
  │                                                 #   ③ 清数据：images/backup/captures/coredumps/tech-support/vms 下
  │                                                 #     受管数据清空（目录本体与属主/权限保留），配置库重置为保留节最小配置。
  │                                                 #   ⑤ 幂等 + 部分失败如实报告：残留逐条列出、非空即返回失败（不假成功）。
  │                                                 #   API: POST /system:format-data（JSON confirm=true）
  └─ ntp sync
request alarms clear [id <id> | all]                # 确认后清除已 resolved 告警
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
ping [ipv6] <host> [source <ip>] [count <n>] [vrf <name>]   # **仅 VPP 数据面**（vppctl ping；source 按接口地址反查接口）；`ipv6` 显式走 v6 平面（vppctl ping ipv6）；**未通即报错**（0 发包 / 无应答）
traceroute [ipv6] <host> [vrf <name>]               # 宿主侧 ICMP/ICMPv6（`ipv6` 走 ICMPv6）；vrf 对 v4/v6 都不支持（明确报错）
monitor interfaces <ifname> [interval <sec>]        # 实时刷新计数，Ctrl-C 退出（CLI 端轮询）
monitor vnf <name>                                  # 跟踪 VNF 状态/事件（CLI 端轮询，Ctrl-C 退出）
wizard                                              # 初始化向导（CLI 端交互式：问答规划资源池+内核基线并提交；非 TTY 拒绝）
clear interfaces statistics [<ifname>]              # S
start shell                                         # S；仅 local console 允许（SSH 登录禁用）
help [command]
```

> 实现说明（决策 #107）：`wizard` 是 **CLI 端交互编排**（与 monitor 的「CLI 端轮询」同类，无独立 API 端点）：问答推导资源池/VPP 线程/大页/低延迟计划，展示将要提交的语句清单，确认后经既有语句（configure/set/commit/request system kernel apply）执行——commit 校验与内核基线护栏（决策 #104）原样生效，向导不绕过任何校验。非 TTY（管道/脚本）打印指引即返回，不挂起。数据口的绑定/声明不在向导范围内（运行期动作，见 §3.2 手册流程），向导结束时打印重启后动作清单。

> 实现说明（M3-9，附录 A #36）：VPP 26.06 的 ping 插件仅提供 finished-event API、无发起接口，故 `ping` 经 `vppctl`（CLI socket）执行；`source <ip>` 经 VPP 接口地址反查接口名后作为 `vppctl ping source <iface>`。VPP 26.06 无 traceroute 插件/CLI/API，`traceroute` 由 nfvisd 宿主侧 raw ICMP 实现，`vrf` 参数在经 VPP 的路径上不支持并明确报错。`monitor interfaces` 服务端返回单次快照，nfvis-cli REPL 按 interval 本地轮询、Ctrl-C 退出。
>
> **`ping` 的平面口径（附录 A #89）**：`ping` **只覆盖 VPP 数据面**——目标要能经 VPP 的路由/接口到达。管理口属**内核平面**，VPP 看不到它，因此 `ping <管理口网关>` 必然失败。此前它把 vppctl 的原始输出（含 `Statistics: 0 sent, 0 received, 0% packet loss`）原样返回且**不报错**，而判定只看 `^%` 与退出码——「一个包都没发出去」被算作通过，`cli-fulltest.sh` 里那条 `ping` 长期是假绿。现规则：**未通即失败**——① **一个包都没发出去**（sent=0，VPP 无到达目标的接口/路由）② **发出了但无任何应答**（`100% packet loss`）**都返回错误**（给 `%` 与非零结果），并在输出里区分两种情况、点明平面归属与替代手段（管理口用宿主 `ping`，或 `traceroute`——它走宿主侧 ICMP）。理由：`ping` 是**连通性测试**，没通就是失败；否则 `ping <不可达>` 返回 0，调用方与判定侧都会以为通了（真机实测 VPP ping 自己的回环地址也是 `2 sent, 0 received`）。判不出汇总行时**不**判失败（格式一变就误报比漏报更糟）。
>
> **IPv6 诊断（附录 A #330）与支持矩阵**：`ping ipv6 <addr>` 经 `vppctl ping ipv6 <addr>`（族选择器写在目标之前，与 VPP CLI 同形；不写 `ipv6` 时照旧按地址字面判族，既有 IPv4 写法一字不变）。`traceroute ipv6 <addr>` 由 nfvisd 宿主侧 **raw ICMPv6**（`ip6:ipv6-icmp` 原生套接字，需 root/CAP_NET_RAW；与 v4 同构，差别只在协议族/常量）。**未通即失败对 v4/v6 同口径**，并区分「没发出去」与「发出了没应答」两种情况（v6 的提示语改按 v6 口径：v6 接口/路由/NDP 邻居）。`traceroute` 的 `vrf` 对 **v4 与 v6 都明确报不支持**（VPP 26.06 无 traceroute 能力、宿主侧 ICMP 无法经 VPP VRF 转发），**不静默降级成 v4**；要经 VRF 测 v4 连通性请用 `ping <host> vrf <name>`。
>
> | 命令 | 平面 | v4 | v6 | vrf | 说明 |
> |---|---|---|---|---|---|
> | `ping <host> …` | VPP 数据面 | ✅ | ✅（地址字面 / `ping ipv6`） | ✅（VPP table-id） | 未通即失败；管理口（内核平面）不可达 |
> | `traceroute <host>` | 宿主侧 raw ICMP | ✅ | ✅（`traceroute ipv6`） | ❌ 明确拒绝 + 替代（`ping … vrf`） | 需 root/CAP_NET_RAW；可测管理口 |
>
> 三面同源：CLI（`?` 候选含 `ipv6`）、REST（`POST /diagnostics/ping|traceroute` 请求体 `ipv6` 布尔）、Web 诊断页（`diag-ipv6` 勾选，走既有 `data-write data-op` 门禁）。

---

## 2. 配置模式命令树（提示符 `nfvis#`，全部 S；class 授权到节点）

### 2.1 导航与事务（固定命令）

```
configure 后：  edit <path> | up | top | exit          # 层级导航，提示符显示 [edit path]
set / delete / show / annotate <path> "text"
commit [confirmed [minutes]] | commit check | commit and-quit
rollback [n]           # n 缺省=1；取历史快照为 candidate（需再 commit）
                       # 历史清单见操作模式 `show configuration history`（rev ←→ n 的对应关系看 rev 差）
load override|merge <path>        # JSON 配置导入
save <path>                       # candidate 导出 JSON
run <oper-command>                # 配置模式内执行操作命令
discard | exit                    # discard 丢弃 candidate；exit 有未提交变更时提示确认
```

**commit 的校验（决策 #152 补一条自锁兜底）**：schema/语义/资源配额/镜像存在性之外，提交的文档
必须**至少保留一个 super-user 账号**（class 为 `super-user`；**class 缺省按 read-only 算**）。
本地账号是唯一登录途径，一个 super-user 都不剩就等于把本机提交成「无人可登录」，只能带外恢复；
守卫落在事务引擎的 `Commit` 上（不在可注入的校验链里，装配方换校验器也漏不掉），失败语义与
既有校验一致：返回校验失败、**候选与编辑锁保留**，操作者补一个 super-user 后可直接重提。
这条对整文档替换的几条路一视同仁：`commit`、`load override`（= REST `PUT /configuration/candidate`）
与 `request system configuration restore`。唯一例外是恢复出厂 `request system zeroize`——
它的目的就是复位账号（提交空配置后由下次启动的引导重建 admin）。

**配置会话锁的排他口径（决策 #317 / #318）**：`configure` 取得的是一把「candidate 会话锁」，
锁按**会话稳定标识**归属（配置会话 = 身份键 `user@source` + token 稳定 ID，决策 #317），
排他性**只为保护未提交的候选**而存在：
- 持锁会话有**未提交改动**（Dirty=yes）时严格排他——别的会话（含同一用户的另一会话）`configure`
  报 `%% candidate 会话锁被占用: 由 <holder> 持有`，其候选不被丢弃、不被抢占；
- 持锁会话的候选**无未提交改动**（Dirty=no，即「干净锁」）时，**同一用户**的任何新会话可直接
  **接管**该锁（原持有者失去锁、新会话成为持有者；审计记一条 `config.lock-takeover`，不报错）；
  跨用户**不可**接管（仍报被占用）。被接管后原会话再操作得到明确错误
  （`本会话已失去 candidate 编辑权 …`），不会静默变成别人；接管者释放锁后原会话可重新进入。
- **释放路径**：`discard`、配置模式 `exit`、`commit and-quit`（提交成功即释放）、CLI 一次性
  命令收尾（决策 #151）、`POST /logout`（按会话标识清掉本会话的锁，**不区分接入源**——CLI 侧
  的 `user@ssh` 锁由登出请求一并释放）都会释放本会话的锁；干净锁另有更短的空闲回收阈值
  （复用既有空闲巡检，不新造定时器）。

### 2.2 `system`

```
[edit system]
set hostname <string>
set timezone <tz>
set ntp server <ip|host> [prefer]
set dns server <ip> [secondary <ip>]                 # 宿主解析器（本机 resolv/systemd-resolved；不改）
set dns proxy server <ip> [secondary <ip>]           # 数据面 DNS 代理——**全局上游**（决策 #345）：域内 VNF/容器把
                                                     #   resolver 指向**产品自己的地址**（交换机网关 BVI / L3 接口
                                                     #   地址）即可解析。实现＝**自研域内转发器**：VPP 经 `punt socket`
                                                     #   （startup.conf 的 `punt { socket … }`，由产品生成）把
                                                     #   「目的地址 ∈ VPP 本机」的 UDP/53 交给 nfvisd，nfvisd 用
                                                     #   **宿主网络栈**向上游解析后按原域回注（`PUNT_IP4_ROUTED`）。
                                                     #   ⚠️ 边界更正（决策 #338 撤回记录那条「上游须在 VPP FIB 内可达」
                                                     #   只适用于 VPP dns 插件形态）：本轮上游是**宿主侧可达**。
                                                     #   启用判据：全局或任一交换机非空即注册并启用；全空即注销
                                                     #   （VPP 恢复默认处理）。纯 UDP 转发：不缓存、不解析内容。
                                                     #   ⚠️ 代价（如实告知）：启用期间指向产品地址的 UDP/53 由 nfvisd
                                                     #   独占——nfvisd 不在（崩溃/被停）时这些包被 VPP punt 节点
                                                     #   **丢弃**（节点 IS_DROP），域内 DNS 中断；故停用必须注销。
                                                     #   客户端用 **TCP** 查 DNS 不在覆盖内（punt 只注册 UDP 53）；
                                                     #   上游不可达/超时按运行期如实回 SERVFAIL 并计数。
                                                     #   ⚠️ 覆盖 **IPv4/UDP/53**（punt 注册按地址族）：IPv6 的解析
                                                     #   查询尚未注册与回注，不在覆盖内（属后续：v6 回注路径真机验证
                                                     #   后再启用，不先做成假能力）。
delete dns proxy server [<ip> | secondary <ip>]      # 撤销全局上游；不带取值即清空全部
                                                     #   （清空后若各域也空 → 停用并注销 punt）
                                                     #   注：`set dns server`（宿主解析器）与之各管一路，互不影响
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
  │                                           #   默认大页尺寸 default_hugepagesz 恒为 2M（数据面/VPP 默认尺寸，决策 #347）：
  │                                           #   池以 hugepagesz=<size> hugepages=N 显式声明（2M 池给 VPP、1G 池给 VNF）
  ├─ nmi-watchdog <true|false>                # NMI watchdog（VPP 场景通常 false）
  ├─ transparent-hugepages <always|madvise|never>
  ├─ iommu <on|off|pt>
  ├─ low-latency <true|false>                 # 低延迟参数组（显式选择，代价见 enable 时的输出；VM 上自动省略 idle=poll/tsc=reliable）
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
set login                               # 权限：S（配置模式既有权限位，本节全部语句同）
  ├─ banner <text>                     # 登录横幅：显示在 Web 登录页与 CLI 登录提示之前（未认证即可见，
  │                                    #   请勿写入敏感信息）；单行、最长 512 字节，超限/含换行时
  │                                    #   commit 校验拒绝并说明上限；delete system login banner 清除
  │                                    #   API: PUT/DELETE /system/login-banner（管理面，一次性事务）、
  │                                    #        GET /login-banner（未认证只读，登录页展示）
  ├─ user <name> password <string> class <class-name>
  ├─ class <name>                      # 自定义 class
  │   ├─ allow <command-path>          # 允许的命令树节点
  │   └─ deny <command-path>
  └─ password-policy
      ├─ min-length <uint> | complexity <bool> | expire-days <uint>
      ├─ lockout-threshold <uint>          # 连续失败锁定阈值（两条**独立**语句，
      └─ lockout-minutes <uint>            # 决策 #76：同一行连写不被支持）
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
                                                     # ⊘ 设计拒绝（VPP 26.06 限制，见规格书附录 A #340）：
                                                     #   真机实证 VPP 26.06 不评估 BVI（网关）上的域内流量——
                                                     #   既不拦截也不计数，绑定给不出任何保护。提交期直接拒绝。
                                                     #   替代：`set virtual-switches <vs> l3-interface <ifname>
                                                     #   acl-in <acl>`（vNIC/物理口作 L3 接口，该形态已实证生效）。
set dns proxy server <ip> [secondary <ip>]           # 数据面 DNS 代理——**按域上游**（决策 #345）：只对该交换机
                                                     #   转发域的入向查询生效（L2＝网关 BVI；L3＝其 l3-interface 地址）。
                                                     #   优先级：本域非空 → 用本域；否则回落全局
                                                     #   （`set system dns proxy server`）；两者皆空 → 对来自本域的
                                                     #   查询如实回 **SERVFAIL**（不静默超时——那比未启用代理时 VPP
                                                     #   回 ICMP unreachable 更差）。
delete dns proxy server [<ip> | secondary <ip>]      # 撤销本域上游（不带取值即清空本域；回落全局）
set dhcp-relay server <ip>                           # DHCP 中继（决策 #335）：把该交换机转发域（网关 VRF，
                                                     #   缺省专属 vr-<name>）里的 DHCP 广播中继到 <ip>。
                                                     #   前置校验：仅 L2 且已 `set gateway ip` 的交换机可配——
                                                     #   src 地址自动取 BVI 的 IPv4 网关地址（用户不填），
                                                     #   未配网关/无 IPv4 网关地址即拒绝并指向 `set gateway ip`；
                                                     #   server 必填、IPv4，且须在该转发域内可达（跨 VRF 的 server 不在 v1）
delete dhcp-relay                                    # 撤销中继（发 dhcp_proxy_config IsAdd=false，幂等；
                                                     #   随交换机删除一并撤）
set learn-limit <n>                                  # MAC 学习条数上限（决策 #337，仅 L2）：下发
                                                     #   bridge_domain_set_learn_limit，环路/广播风暴的
                                                     #   缓解手段（**只缓解不阻断**）。取值 1-16777216
                                                     #   （超限拒绝并说明；VPP 默认 16777216 即不设限）。
                                                     #   候选为取值（无枚举）；校验在模型 validate。
delete learn-limit                                   # 清上限（恢复 VPP 默认 16777216，幂等）
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
#       ⊘ 设计拒绝（同网关口径，决策 #340 修订）：真机实证（round119）VPP 26.06
#          不评估 L2 路径（成员端口）上的 ACL——既不拦也不计，提交期直接拒绝。
#          替代：`set virtual-switches <n> l3-interface <ifname> acl-in <acl>`。
#          ⚠️ 命令树**有意不提供**该语句（`ports <seq> acl-in` 报「未知命令」；决策 #344）——
#             该形态不支持，故不建叶子；字段仅 REST 可达且置非空必被拒（契约已如实标注）。
#   set virtual-switches <n> l3-interface ... acl-in <acl>
#       ✅ 已实证生效（round118）：vNIC/物理口作 L3 接口时 ACL 确实在拦。
#       🔁 绑定时产品**自动伴随**一条放行全部非 IP（含 ARP）的 macip 白名单（决策 #341），
#          故对端无需预置静态邻居；IP 流量仍受 ACL，解绑时伴随白名单一并解绑。

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
# 绑定（决策 #331）：set interfaces <ifname> ingress-policy <name>   # VPP policer input
#                    set interfaces <ifname> egress-policy  <name>   # VPP policer output
# 两个方向是同一族语句、各自独立；同一接口可同时绑入向与出向（可为不同策略），
# 删除任一绑定（delete interfaces <if> ingress-policy|egress-policy）不影响另一方向；
# 策略被任一方向引用时删除策略即被拒绝（读视图逐条标注 接口:in / 接口:out）。
```

### 2.6 `resource-pools`

```
[edit resource-pools]
# 内核基线里 default_hugepagesz 恒为 2M（数据面/VPP 默认尺寸）：2M 池显式 hugepagesz=2M hugepages=N、
# 1G 池显式 hugepagesz=1G hugepages=N（只给 VNF）——决策 #347；hugepages= 归属其前最近的 hugepagesz=。
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
| 操作模式 `show configuration history` | 保留的历史提交快照**列表**（rev/时间/用户/注释/是否当前；**不含配置正文**。`Engine.History`，与 `GET /configuration/history` 同源，决策 #142） |
| 操作模式 `show configuration sessions` | 与 `show system configuration sessions` **同一读物**（`Engine.Sessions`，与 `GET /system/configuration/sessions` 同源）——等价写法，不复制渲染逻辑（决策 #153）。输出列 Holder/Session/User/Acquired/Last-Activity/Dirty：**会话标识**（持锁会话的稳定 ID）与**所属用户**如实列出，同一用户的多个会话不被合并（决策 #317） |
| 操作模式 `show system configuration candidate` | **不存在该形态**（不是「等价写法」）：`show system` 下只挂 `configuration sessions`，`candidate` 的唯一写法是上表 `show configuration candidate` 那一行。真敲该形态，执行器回 `% 该 show 命令形式未支持…`，**不会**静默当「读配置」作答（决策 #153） |
| 操作模式 `show configuration \| compare rollback <n>` | committed ⇄ 第 n 个历史快照 diff（**已实现**：`Engine.Compare(n)`） |
| 操作模式 `show configuration compare rollback <n>` | 与上一行的**管道形态等价**（同一 `Engine.Compare(n)`；两种写法都在命令树里，`?`/Tab 均可补出） |
| 操作模式 `show configuration <未知子命令>` | **报错**（`% 无效命令: show configuration <x>（可用：…）`），**不显示配置正文**——`show configuration` 省略子命令才是"读 committed"（决策 #153） |
| 操作模式 `show configuration permissions <class> [detail]` | **该 class 的生效权限视图**（决策 #304）：把该 class 在 CLI 命令树上的有效判定**逐路径**算出来并展示——默认按顶层命令族列出**允许路径** + 末行汇总（class 名、来源＝预置/自定义、允许/拒绝条数）；`detail` 附**判定依据**列（预置等级满足 / allow 前缀命中 / deny 前缀命中 / 默认拒绝）。判定**单源在 `internal/aaa`**（与运行期 `Authorize` 同一实现，show 层只枚举命令树路径 + 调用判定 + 渲染）：预置三档按 §4 权限矩阵等级；自定义 class 按 allow/deny 路径前缀（**deny 优先、allow 放行、其余默认拒绝**）。`\| display set` 对自定义 class 输出等价 `set system login class <name> allow/deny <prefix>` 语句（经 display set 反推机制）；预置 class 由等级判定、无 allow/deny 路径表，如实给出基等级说明、不编造语句。权限 R：read-only 可查自己所属 class；**非 super-user 查他人 class 一律拒绝**（不泄露他人规则）；super-user 可查任意 class（含自定义）；未知 class 名明确报错。REST 等价 `GET /configuration/permissions?class=<name>`（无参 = 调用者自己；非 super 查他人 403、未知 404） |
| 配置模式 `show` | candidate（当前层级） |
| 配置模式 `show \| display set` | **已实现**（决策 #155，推翻 #84 的搁置）：把当前层级（含顶层）配置反推为逐行 `set` 语句，语句带**绝对路径**、敏感值**不输出**并以 `#` 注释说明（`model.IsSensitiveKey` 单一真源；掩码占位符回放会静默替换凭据，故省略）、含空格取值按语句分词器同规则加引号；**生成后回放自校验**（语句经真实 `applyStatement` 回放进空配置必须还原原配置，不等即报内部错误——#84 担心的「复制配置静默错误」在结构上被排除）。实现 = 通用逆走器（`jsonKeyOf` 机械双射 + `identityFields` + IVK + ScalarParam）+ 13 个别名家族的逆映射发射器（与别名 apply 同源对照维护）。`show configuration \| display set`（操作模式）同管道同实现 |
| 配置模式 `show \| compare` | candidate ⇄ committed diff（**已实现**：`Engine.CompareCandidate`，2026-09-18 接线，发现 #4） |

## 4. class 权限矩阵（预置）

| 命令域 | super-user | operator | read-only |
|---|---|---|---|
| `show *` | ✔ | ✔ | ✔ |
| `configure`（配置模式全部） | ✔ | ✘ | ✘ |
| `request`（生命周期/镜像/接口） | ✔ | ✔ | ✘ |
| `request`（software/reboot/configuration） | ✔ | ✘ | ✘ |
| `clear` / `start shell` | ✔ | ✘ | ✘ |
| `show system api tokens`（决策 #301） | ✔（全部用户的会话） | ✔（仅自己的） | ✔（仅自己的） |
| `show configuration permissions <class> [detail]`（决策 #304） | ✔（任意 class，含自定义） | ✔（仅自己所属 class） | ✔（仅自己所属 class） |
| `request system api token revoke <token-id>`（决策 #301） | ✔（任意会话） | ✔（仅自己的） | ✘（request 域以 O 为基线；read-only 的自助结束会话用 `POST /logout`，REST 侧同能力开放） |

## 5. 补全行为细则（供补全引擎实现）

1. 任意位置输入 `?`：列出当前 token 位置所有候选（关键字=名称+描述；参数=类型提示+动态值来源），并回显已输入部分（提示符 + 已输入文本）。若 `?` 前有部分字符，只列以此为前缀的候选。
   **`?` 是按键即时行为（键入即列出，不需要回车）**，且 `?` 本身不进入行文本——行文本只由回车提交执行。
   因此 CLI **不接受把 `?` 当作字面量取值**（与 Cisco IOS 同构；已知限制，见附录 A #81）。
2. Tab：唯一匹配→补全并附空格；唯一匹配但需更多字符（如接口名前缀）→补全到公共前缀；多匹配→响铃并列出（与 `?` 同）。
   补到公共前缀或唯一匹配时**只改写行文本、不列候选**；多匹配且公共前缀无进展时才响铃并列出。
3. 动态候选来源（实时向 nfvisd 查询，失败则退化为仅关键字）：`<ifname>`→接口清单、`<name>`→对应资源清单、`<image-name>`→镜像清单、`<class-name>`→class 清单。
   **接口名一族分三种来源，不是同一个清单（附录 A #83）**——此前三者共用一个「已写进配置的接口名」，
   既漏掉未声明的 DPDK 口（已接管的口在内核中已无 netdev），又会列出根本不存在的名字；
   决策 #302 为 `set interfaces` 增设第四种（配置声明位的全量并集，首装可见内核网卡）：

   | kind | 含义 | 用它的位置 |
   |---|---|---|
   | `vpp-ifnames` | VPP 中的接口 = **已被 DPDK 接管的数据面端口** | `show interfaces physical <ifname>`、`show interfaces <ifname>`（`physical` 可省的等价写法）、`virtual-switches … l3-interface`、`set vpp dpdk dev <ifname>`、`request vpp trace start interface <ifname>`、`monitor interfaces <ifname>`、`clear interfaces statistics [<ifname>]`、`set protocols lldp interface <ifname>`、`show lldp neighbors interface <ifname>` |
   | `kernel-ifnames` | 内核网卡（**未被接管**的物理口；有 `/sys/class/net/<n>/device` 的才算） | `set system management interface <ifname>`、`request interfaces <ifname> bind-dpdk`、`request sriov create-vfs/delete-vfs <ifname>` |
   | `ifnames` | 两者**并集** | `request interfaces <ifname> enable\|disable\|bind-dpdk\|unbind-dpdk`（动作混合、参数位置在动作之前，无法按动作区分来源） |
   | `all-ifnames` | **内核未接管 ∪ 配置已声明 ∪ VPP 运行态**（决策 #302） | `set interfaces <ifname>`（声明是接管流程的第一步，首装在接管前也能补全到内核网卡名；声明口在「已绑定 + VPP 未起」等生命周期各态都可能暂时缺席其它清单，故三源取并） |

   实现：`internal/orchestrator/network/port_inventory.go`（VPP 侧 `sw_interface_dump`、内核侧 sysfs），
   经 `api.PortInventory` 接口注入，与 `show interfaces physical` 的空态同源。
   **失败（VPP 未接入/查询失败）时退化为「仅关键字」，不退回「已配置接口名」**——那正是本决策要修的错误来源
   （`all-ifnames` 是有意把声明名并入的例外：声明位候选答的是「配置里正在编辑哪个口」，见 #302；
   每个来源独立退化——VPP 不可用时内核侧与声明名照常给值）。
4. 配置模式下 `?` 还会提示当前 `[edit]` 层级下可 `set/delete` 的直接子节点。
5. 命令缩写：无歧义前缀即合法（`sh vi` = `show virtual-machine-functions` 前缀匹配按树节点逐级消歧）。
6. 候选列表渲染：每条一行、行首两空格；列宽取「最长候选 token 宽度 + 2 空格」与 24 的较大者。
   固定 24 会让 25 字符的 `virtual-machine-functions` 与描述粘连（附录 A #81④）。
7. raw 模式下的输出换行：行编辑器进入 raw 模式即关闭终端 `OPOST`（`ONLCR` 随之失效），
   **裸 `\n` 只下移光标不回车**。故进入 raw 后**命令输出与服务端文本必须经 CRLF 转换再写出**，
   否则多行输出逐行右移（候选列表呈阶梯状、提示符错位）。非 raw（管道/脚本、monitor 挂起期）
   **不得转换**——终端自身会做 NL→CRNL，重复转换会多出空行。实现见 `internal/cli/output.go`。
   该转换同样作用于**串口接管**（`request … console`）：该路径不减 `Suspend` raw 模式，
   guest 以裸 LF 输出时会被补 CR（等价于终端 cooked 模式的 `ONLCR`）；裸 LF（0x0A）
   不可能是多字节字符的续字节，故对 UTF-8/控制序列安全。
8. **说明文本口径（附录 A #86、#87）**：命令树的 `Desc`（`?`/Tab 候选列表与 `help` 输出里的那列说明）
   是**给操作者看的**，**不得包含内部引用**——`FR-xxx`、`§x`、`决策 #nn`、`附录 A #nn` 一律不写。
   需求可追溯（AGENTS 规则 2）写在**代码注释与设计类 `docs/`** 里。
   由 `internal/archtest/user_text_test.go` 守护（同时拒绝剥掉引用后留下的残渣：圈号 `①-⑳`、`/#nn`、
   空标点括号、连续标点）。示例：`commit` 的说明是 `提交 candidate`，不是 `提交 candidate（FR-CFG-002/003）`。
9. 管道位置同样可补全（FR-CLI-002「任意位置」，决策 #155 补充三）：行内最后一个未引用 `|`
   之后，`?`/Tab 列出**管道关键字**（match/except/count/last/begin/display/compare，含描述）；
   `display` 的取值位列 json/xml/set，`compare` 的取值位列 rollback；match/except/begin（正则）
   与 last（行数）为自由取值、无候选；取值给全后本段无候选（下一个 `|` 开新段、再次列关键字）。
   双引号内的 `|` 不视为管道分隔（与守护进程 splitPipes 同语义）。
   实现：`schema.PipeCandidates`（与 `PipeKeywords` 同一单一来源），CLI 会话把含未引用 `|`
   的行的补全上下文切到管道段。
10. CLI 词法（决策 #313）：**引号内的一切不参与切分**——双引号内的 `|` 不是管道分隔、
   `#` 不是注释（CLI 本就没有注释语法，`#` 在任何位置都是普通字符，以便 user-data 的
   `#!/bin/sh`/`#cloud-config` 能内联）；**引号可跨行**——脚本（`-c`/`-f`）与交互 REPL
   都按「引号未闭合则并入后续行」切逻辑语句，值内**保留换行**（多行 user-data 可内联，
   不必只走文件路径）。转义 `\"` 与 `\\` 在引号内有效（管道切分与分词器同源，不再两套词法）。
   值里的换行随 token 进入配置；只该单行的值（如登录横幅）由提交校验明确拒绝，
   **不静默取首行**。实现 = `internal/cliparse`（纯函数，管道切分与分词共用）。
8. **说明文本口径（附录 A #86、#87）**：命令树的 `Desc`（`?`/Tab 候选列表与 `help` 输出里的那列说明）
   是**给操作者看的**，**不得包含内部引用**——`FR-xxx`、`§x`、`决策 #nn`、`附录 A #nn` 一律不写。
   需求可追溯（AGENTS 规则 2）写在**代码注释与设计类 `docs/`** 里。
   由 `internal/archtest/user_text_test.go` 守护（同时拒绝剥掉引用后留下的残渣：圈号 `①-⑳`、`/#nn`、
   空标点括号、连续标点）。示例：`commit` 的说明是 `提交 candidate`，不是 `提交 candidate（FR-CFG-002/003）`。
   **守护范围（附录 A #87 扩展）**——判据是「**这段文本会到达操作者吗**」，不是文件后缀：

   | 被扫 | 扫哪部分 | 理由 |
   |---|---|---|
   | `.go` | 字符串字面量（跳过注释、`_test.go`、`prototype/`） | 报错/告警/帮助/日志文案（#86） |
   | `.sh` | **非注释行**（引号感知的 `#` 内联注释亦豁免） | 安装脚本与运维脚本的 `echo`/`log` 输出 |
   | `.service` | 非注释行（行首 `#`/`;` 豁免；systemd 无内联注释） | `Description=`/`Documentation=` 会进 `systemctl status` |
   | `Makefile`（含 `*.mk`） | 非注释行 | 构建期的 `echo` 直接打在操作者终端 |
   | `docs/NFViS-用户手册.md` | **全文** | 随 deb 装到 `/usr/share/doc/nfvis/`，是操作者说明书 |

   仍在 `docs/` **保留**引用的是**设计/契约/验收类**文档——规格书（FR 的定义处）、命令树设计、
   `NFViS-openapi.yaml` 的 description、验收检查表、`NFViS-CLI命令全表.md`、`M5-验收记录.md`：
   它们的引用本身就是需求可追溯的落点，清掉会削弱验收证据链。
   故本规则**按「是否操作者读物」逐个文件判定，不做 `docs/` 整体豁免**；新增随包发布的读物时须同步此表。
9. **实例名位置的解析优先级（附录 A #82）**：语句树里形如
   `K("user", …, P("<name>"), K("password", …), K("class", …))` 的节点，其**首个 token 必须先按实例名消费**，
   不得先按子关键字解释。此前解析器先做子关键字匹配，导致 `set system login user password Admin@123`
   把 `password` 当成子关键字、把取值写到了**祖先容器**（`system.login.password`）上，
   产出模型无法接受的树，用户看到的是 `json: unknown field "password"`。
   规则：**实例名位置上与子关键字同名的 token 一律视为实例名**；若该名字确实与子关键字冲突，
   语句树应改用其它名字（避免歧义）。
10. **子语句必须成完整形**：`set system login user <name> <子关键字> <取值>` 中
   `password`/`class` 只能出现在 **`<name>` 之后**；把子关键字直接放在名字位（如
   `set system login user password`）会被当作**用户名**。为避免「打错字静默建出一个无口令账号」，
   `system login user` 的别名层显式拒绝与子关键字同名的用户名（仅 `set`，`delete` 不受限，
   以便清理历史误建账号），报错时给出正确写法（附录 A #82）。
   同一条口径的**补全**（附录 A #90②）：`set system login user <name>` **不能单独成句**——
   只给名字会落库出「有名字、无 `password_hash`、无 class」的账号（真机实测 CLI 回 `[ok]` 且
   `commit` 报成功）。此约束写在**命令树**上（`Node.RequireSub`，由 `RQ()` 标记），
   在别名派发**之前**统一判定，故 `set` 走别名表还是走通用遍历都拦得住；
   `interfaces <ifname>` 那种「裸声明本身有意义」的节点（决策 #72：先声明端口、绑定后再提交）
   **不得**标记。
11. **补全的层级回退（附录 A #90①）**：无子树的参数（实例名/标量取值）消耗掉一个 token 后，
   下一位置的候选来自**父层关键字**——这正是包注释写明的匹配语义（「值叶子与无子树参数消耗
   一个 token 后回到父关键字层继续匹配」），也是 `Match` 每个 token 开头做的事。此前候选侧漏了
   这步，于是 `set system login user admin `、`set system management interface ens160 `、
   `set system ntp server 1.2.3.4 ` 这些位置**一个候选都列不出来**（操作者只能手打子关键字）；
   而 `login class <name>` 恰好是对的，因为那个参数**把子节点挂在自己身上**——同一形态在树里
   两种建模，把缺陷掩了很久。规则：**向上找最近的一层关键字**（层级由结构决定，不随输入前缀
   漂移），并**排除来路**（否则 `management interface ens160 ` 会把 `interface` 再列一遍）。
   **只对参数回退、不对值叶子回退**：值叶子之后的同级关键字（如 `api tls cert-file <p>` 之后的
   `key-file`）与 oper 树（`show`）的同级关键字都不在此列，一律列出会把 `?` 变成噪声；
   连续位置参数（`cross-connect <a> <b>`）的同级参数同样暂不列出——如实登记为已知局限。
12. **必需取值不得被同级关键字抢位（附录 A #91）**：`system dns server` 的首个子节点是**必需**的
   `<ip>`（`SPA`），若直接把兄弟关键字 `secondary` 匹配掉，取值位就永远空着，语句最后以
   「配置中不存在字段 "dns"」收场——而 `dns` 明明是合法关键字，操作者会被引向错误方向。
   规则：**直接命中的关键字**若其同级存在未给值的必需标量参数，则报「语句不完整：… 需要先给
   `<ip>` 取值，再跟 `secondary`」。只用「直接命中」判（层级回退不算），故
   `vmf x interfaces eth0 type memif …` 这类合法续写不受影响。
13. **候选描述里的类型取自占位符**（附录 A #90③）：`<ip>` → `（ip）`，不得用 `Node.ParamType`
   ——后者对 `P`/`SP`/`SPA`/`SPD` 统一是 `"name"`（那是给 `scalarForNode` 做取值类型转换用的），
   拿它当标签会写出 `<ip>  服务器地址（name）` 这种自相矛盾的候选。
