# NFViS CLI 命令全表

| 文档属性 | 内容 |
|---|---|
| 用途 | **命令参考全表**：把 CLI 命令树逐条列出，附权限、落点与**真机实测状态** |
| 来源 | 命令树取自实现（`internal/schema/tree_oper.go`、`tree_config.go`，即 `?` 补全的真实来源）；契约见 `docs/NFViS-CLI命令树完整设计.md` |
| 实测状态 | 来自 **2026-09-14 全功能 CLI 测试**（决策 #76，证据 `docs/evidence/v1-closeout-round8.txt`）。测试工具：`contrib/scripts/cli-fulltest.sh` |
| 基线 | main + PR #69/#70；决策 77 项 |

## 0. 阅读约定

**权限**（预置 class，契约 §4）：

| 记号 | 含义 |
|---|---|
| R | `show *`，三个 class 均可用 |
| O | `request` 生命周期/镜像/接口，super-user 与 operator 可用 |
| S | 破坏性/敏感（`request software/reboot/configuration/zeroize`、`clear`、`start shell`、**配置模式全部**），仅 super-user |

**实测状态**：

| 记号 | 含义 |
|---|---|
| ✅ | 真机 CLI 实测通过 |
| ⚠️ | **已知缺口**：未实现但**明确提示**（非静默空值） |
| ⊘ | **预期报错**：环境受限或防呆守卫正确拒绝——报错即正确行为 |
| ❌ | **确认不可用**：契约已声明、但经 CLI 用不了（缺陷，待修，见 §4⑦） |
| 🚫 | **本轮未执行**：破坏性/需交互，测试机不宜执行（非「未实现」） |

**落点**：`POST /cli/execute` 是所有 CLI 命令的统一入口；表中「落点」列给出该命令**实际作用的**
等价 REST 端点或底座子系统。说明理由：CLI 执行器对 `show`/`request` 族**直连运行态 Provider**
（与对应 GET 端点同源，不经自身 HTTP，决策 #49~#51）；配置模式经**事务引擎**（candidate → commit → Applier）。

---

## 1. 操作模式命令树（提示符 `nfvis>`）

### 1.1 `show`（查询，R）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `show version` | 版本汇总（nfvis/ubuntu/vpp/dpdk/libvirt/qemu/docker） | `GET /system/version` | ✅ |
| `show system uptime` | 运行时长 | `GET /system/status` | ✅ |
| `show system cpu` | 总核/隔离核/每核占用 | 运行态（宿主 `/proc`） | ✅ |
| `show system memory` | 内存与大页使用（池内/池外） | 运行态 | ✅ |
| `show system storage` | 磁盘与镜像仓库占用 | 运行态 + 镜像仓库 | ✅ |
| `show system hugepages` | 大页内核参数与池状态 | 运行态（`/proc/meminfo`） | ✅ |
| `show system kernel` | 内核启动基线三方对照（cmdline/运行实际/配置期望，FR-SYS-014） | 配置 + 运行态 | ✅ |
| `show system hardware` | 硬件健康：温度/风扇/电源/SMART（FR-SYS-012） | `GET /system/hardware` | ✅（本机无 IPMI/传感器，走降级路径） |
| `show system core-dumps` | 崩溃转储清单（VPP/QEMU/nfvisd） | `GET /system/core-dumps` | ✅ |
| `show system tech-support` | 诊断归档清单 | `GET /system/tech-support` | ✅ |
| `show system configuration sessions` | candidate 持锁会话列表（FR-CFG-009） | `GET /system/configuration/sessions` | ✅ |
| `show configuration sessions` | 同上（等价写法） | 同上 | ✅ |
| `show interfaces` | 全部接口摘要 | `GET /interfaces` | ✅ |
| `show interfaces physical` | DPDK 物理口（驱动/链路/速率/VF 数） | `GET /interfaces` | ✅ |
| `show interfaces physical <ifname> detail` | 驱动/MAC/MTU/队列/NUMA | `GET /interfaces/{name}` | ✅ |
| `show interfaces physical <ifname> statistics` | 收发包/字节/错误/drop | 运行态（VPP stats） | ✅ |
| `show interfaces physical <ifname> sriov` | VF 列表与占用状态 | sysfs `sriov_numvfs` | ✅（无 PF/VF 时为空列表） |
| `show interfaces management` | 管理口（内核侧，IP/链路） | 运行态（内核） | ✅ |
| `show interfaces <ifname> detail` | 同上（`physical` 可省） | `GET /interfaces/{name}` | ✅ |
| `show interfaces <ifname> statistics` | 同上 | 运行态 | ✅ |
| `show interfaces <ifname> sriov` | 同上 | sysfs | ✅ |
| `show virtual-switches` | 全部虚拟交换机摘要 | `GET /virtual-switches` | ✅ |
| `show virtual-switches <name> detail` | 类型/成员端口/VLAN/VRF | `GET /virtual-switches/{name}` | ✅ |
| `show virtual-switches <name> ports` | 成员端口及状态/计数 | `GET /virtual-switches/{name}/ports` | ✅ |
| `show virtual-switches <name> mac-table` | MAC 学习表（仅 L2） | `GET /virtual-switches/{name}/mac-table` | ✅ |
| `show virtual-switches <name> statistics` | 每端口收发计数 | 运行态（VPP） | ✅ |
| `show vrfs` | L3 交换机（VRF）列表 | `GET /vrfs` | ✅ |
| `show vrfs <name>` | detail：L3 接口/地址/路由 | `GET /vrfs/{name}` | ✅ |
| `show vrfs <name> routes` | FIB 路由表 | `GET /vrfs/{name}/routes` | ✅（VRF 不存在时明确报错，决策 #76④） |
| `show acls` | ACL 列表 | `GET /acls` | ✅ |
| `show acls <name> detail` | 规则与绑定详情 | `GET /acls/{name}` | ✅ |
| `show nat` | NAT 池/规则/转换会话计数 | `GET /nat` | ✅ |
| `show port-mirroring` | SPAN 会话状态 | `GET /port-mirroring` | ✅ |
| `show qos policies` | 限速策略与绑定 | `GET /qos/policies` | ✅ |
| `show vpp` | 数据面概览：版本/线程/buffer/内存 | `GET /vpp/status` | ✅ |
| `show vpp threads` | main/worker 线程清单与绑核 | 运行态（govpp threads） | ✅ |
| `show vpp runtime [thread <id>]` | 每线程向量率/指令周期 | 运行态（govpp runtime） | ⚠️ **未接入**（附录 A #34；CLI 明确提示） |
| `show vpp buffers` | buffer 池（每 NUMA）用量；打印统计来源 | 运行态（statsclient ‖ `vpp_get_stats`，决策 #68） | ✅ |
| `show vpp memory` | main-heap 与 hugepage 占用 | 运行态 | ✅ |
| `show vpp capture` | 抓包会话状态与已导出 pcap 清单 | `GET /vpp/capture` | ✅ |
| `show bonds` | 链路聚合列表 | `GET /bonds` | ✅ |
| `show bonds <name> detail` | 成员口 link/LACP actor-partner | `GET /bonds/{name}` | ✅ |
| `show lldp neighbors [interface <ifname>]` | LLDP 邻居表 | `GET /protocols/lldp/neighbors` | ✅（无对端时为空表） |
| `show protocols lldp neighbors` | 同上 | 同上 | ✅ |
| `show virtual-machine-functions` | VM 列表 | `GET /virtual-machine-functions` | ✅ |
| `show virtual-machine-functions <name> detail` | 域 XML 摘要/资源分配/NUMA | `GET /virtual-machine-functions/{name}` | ✅ |
| `show virtual-machine-functions <name> interfaces` | vNIC：类型/MAC/socket/交换机 | 同上 | ✅ |
| `show virtual-machine-functions <name> statistics` | vhost-user 口计数（经 VPP） | 运行态（VPP） | ✅ |
| `show virtual-machine-functions <name> snapshots` | 快照列表 | `GET /virtual-machine-functions/{name}/snapshots` | ✅ |
| `show container-functions` | 容器列表 | `GET /container-functions` | ✅ |
| `show container-functions <name> [detail]` | 容器详情 | `GET /container-functions/{name}` | ✅ |
| `show container-functions <name> interfaces` | memif vNIC 列表 | 同上 | ✅ |
| `show images` | 镜像仓库列表 | `GET /images` | ✅ |
| `show images <name> detail` | 类型/大小/sha256/引用计数 | `GET /images/{name}` | ✅ |
| `show resource-pools` | 大页池/隔离核：总量、已分配、空闲（含 vpp-reserved） | `GET /resource-pools` | ✅ |
| `show alarms [active\|all]` | 告警列表 | `GET /alarms` | ✅ |
| `show log system [level <lvl>] [last <n>]` | 系统日志 | 服务端日志文件 | ✅ |
| `show log audit [last <n>]` | 审计日志 | `GET /audit-logs` | ✅ |
| `show log vnf <name> [last <n>]` | VNF 控制台/事件日志 | 运行态 | ✅ |
| `show users` | 本地用户与 class | `GET /system/login-users` | ✅ |
| `show configuration [permissions <class>]` | 当前 committed 配置（JunOS 风格） | `GET /configuration/candidate`（committed 视图） | ✅ |
| `show configuration candidate` | 当前持锁会话的 candidate | `GET /configuration/candidate` | ✅ |
| `show configuration compare rollback <n>` | 与历史快照比对 | `GET /configuration/diff` | ✅ |
| `show tech-support` | 诊断包清单预览 | `GET /system/tech-support` | ✅ |
| `help [command]` | 帮助 | 本地（命令树） | ✅ |

**通用管道**（所有 `show` 输出可用，本地处理）：

| 管道 | 说明 | 实测 |
|---|---|---|
| `\| match <regex>` | 仅保留匹配行 | ✅ |
| `\| except <regex>` | 排除匹配行 | ✅ |
| `\| count` | 计数 | ✅ |
| `\| last <n>` | 末尾 n 行 | ✅ |
| `\| begin <regex>` | 从首个匹配行开始 | ✅ |
| `\| display json` | JSON 渲染 | ✅ |
| `\| display xml` | XML 渲染 | ✅（结构性输出） |

### 1.2 `request`（运维动作，O；标 S 者为破坏性）

| 命令 | 说明 | 权限 | 落点 | 实测 |
|---|---|---|---|---|
| `request virtual-machine-functions <n> start` | 启动 VM | O | `POST /vmf/{n}:start` | ✅ |
| `request virtual-machine-functions <n> stop` | 停止（ACPI 关机，超时强杀） | O | `POST /vmf/{n}:stop` | ✅（**决策 #76③** 修超时误报） |
| `request virtual-machine-functions <n> restart` | 重启 | O | `POST /vmf/{n}:restart` | ✅ |
| `request virtual-machine-functions <n> console` | 进入串口（Ctrl-] 退出） | O | `POST /vmf/{n}/console` + WS | ✅（非 TTY 明确提示；真人 Ctrl-] 见 T0-4） |
| `request virtual-machine-functions <n> snapshot create [name <s>]` | 创建快照 | O | `POST /vmf/{n}/snapshots` | ✅ **需关机态**（决策 #75） |
| `request virtual-machine-functions <n> snapshot rollback [name <s>]` | 回滚快照 | O | `POST .../snapshots/{s}:rollback` | ✅ **需关机态**（决策 #75） |
| `request virtual-machine-functions <n> snapshot delete [name <s>]` | 删除快照 | O | `DELETE .../snapshots/{s}` | ✅ |
| `request virtual-machine-functions <n> delete` | 删除 VNF（级联 vNIC/VPP 端口/快照） | S | `DELETE /virtual-machine-functions/{n}` | 🚫 交互确认（`Delete VNF 'x'? [yes,no]`；非交互拒绝） |
| `request container-functions <n> start` | 启动容器 | O | `POST /container-functions/{n}:start` | ✅ |
| `request container-functions <n> stop` | 停止容器 | O | `POST /container-functions/{n}:stop` | ✅ |
| `request container-functions <n> restart` | 重启容器 | O | `POST /container-functions/{n}:restart` | ✅ |
| `request container-functions <n> log [last <n>]` | 容器 stdout/stderr | O | `GET /container-functions/{n}/logs` | ✅ |
| `request container-functions <n> delete` | 删除容器 | S | `DELETE /container-functions/{n}` | 🚫 交互确认 |
| `request images upload name <n> type <t> file <path>` | 从 `/data/incoming/` 导入（成功自动清理源文件） | O | `POST /images` | ✅（见 §4 已知限制①） |
| `request images download name <n> type <t> url <u> sha256 <hex>` | 从 HTTP(S) 拉取（**sha256 强制**） | O | `POST /images` | ✅（FR-SEC-004；缺 sha256 即拒） |
| `request images delete name <n>` | 删除镜像（引用检查） | S | `DELETE /images/{n}` | ✅ |
| `request interfaces <ifname> enable` | 启用接口 | O | `PUT /interfaces/{n}` | ✅ |
| `request interfaces <ifname> disable` | 禁用接口 | O | `PUT /interfaces/{n}` | ✅ |
| `request interfaces <ifname> bind-dpdk [uio-driver <d>]` | 绑定 DPDK 驱动（中断流量，需确认） | O | `PUT /interfaces/{n}/dpdk` | ✅（真机周期见决策 #72） |
| `request interfaces <ifname\|pci> unbind-dpdk [to-driver <d>]` | 解绑交还内核驱动 | O | `PUT /interfaces/{n}/dpdk` | ✅（提示确认；接管后须按 PCI） |
| `request sriov create-vfs <ifname> count <n>` | 创建 VF | O | `PUT /interfaces/{n}/sriov` | ⊘ 本机无 PF/VF，明确报错 |
| `request sriov delete-vfs <ifname> vf <n>` | 回收 VF | O | `PUT /interfaces/{n}/sriov` | ⊘ 同上（另：`vf <n>` 参数被忽略，登记 V2） |
| `request vpp restart` | 按 committed 配置重建数据面 + 恢复收敛 | S | `POST /vpp/restart` | ✅ |
| `request vpp trace start interface <if> [count <n>] [filter <acl>]` | 开始抓包 | S | `POST /vpp/capture` | ✅ |
| `request vpp trace stop` | 停止抓包（不导出） | S | `DELETE /vpp/capture` | ✅ |
| `request vpp trace export [name <n>]` | 导出 pcap（**隐含 stop**） | S | `DELETE /vpp/capture` | ✅ |
| `request system software add <deb\|url> [sha256 <hex>]` | 安装升级包（校验→升级→重启 nfvisd） | S | `POST /system/software` | 🚫 破坏性 |
| `request system software rollback [to <v>]` | 回退版本 | S | `POST /system/software:rollback` | ✅ |
| `request system reboot` | 重启系统 | S | `POST /system:reboot` | 🚫 破坏性 |
| `request system shutdown` | 关机 | S | `POST /system:shutdown` | 🚫 破坏性 |
| `request system poweroff` | 断电 | S | `POST /system:shutdown` | 🚫 破坏性 |
| `request system kernel apply` | 写入 GRUB 内核基线（需重启生效） | S | 宿主 `/etc/default/grub` | 🚫 会改启动项，本轮不执行 |
| `request system kernel rollback` | 回退内核基线 | S | 同上 | 🚫 同上 |
| `request system configuration backup [to <path>]` | 导出 committed 配置归档 | S | `GET /system/backup` | ✅ |
| `request system configuration restore <path>` | 导入归档为 candidate 并提交 | S | `PUT /configuration/candidate` | 🚫 会覆盖现网配置 |
| `request system tech-support generate` | 生成诊断归档 tar.gz | O | `POST /system/tech-support` | ✅ |
| `request system core-dumps export <url>` | 导出转储到 URL | O | 运行态 | ✅ |
| `request system core-dumps delete [file <n>]` | 删除转储 | O | `DELETE /system/core-dumps` | ✅（**决策 #76⑨** 修错误文案） |
| `request system zeroize` | 恢复出厂（双重确认） | S | `POST /system:zeroize` | 🚫 破坏性 |
| `request system api tls regenerate` | 重签自签证书 | S | `POST /system/tls:regenerate` | ✅ |
| `request system api token revoke <token-id>` | 吊销 token | S | `DELETE /login` 等价 | ⚠️ V1 仅提示「经 API DELETE /login 吊销当前会话，逐 token 随 V2」（**决策 #76⑧** 修正契约位置） |
| `request system ssh host-key regenerate` | 重新生成 SSH host key | S | 宿主 sshd | ✅ |
| `request system password change` | 登录者自助改密（验证旧口令） | S | `PUT /system/login-users/{n}` | 🚫 需交互输入（契约已登记延期） |
| `request system storage format-data` | 重置数据分区（危险，双确认） | S | 宿主 | 🚫 破坏性（契约已登记延期） |
| `request system ntp sync` | 立即触发一次 NTP 同步 | O | 宿主 chrony/ntpd | ✅ |
| `request alarms clear [id <id> \| all]` | 清除已 resolved 告警 | O | `POST /alarms:clear` | ✅ |

### 1.3 其余操作命令

| 命令 | 说明 | 权限 | 落点 | 实测 |
|---|---|---|---|---|
| `configure` | 进入配置模式 | S | 本地（会话模式切换） | ✅ |
| `exit` / `quit` | 退出 CLI | R | 本地 | ✅ |
| `ping <host> [source <ip>] [count <n>] [vrf <name>]` | 经 VPP L3 连通性测试 | O | `vppctl ping`（CLI socket） | ✅（`source` 须为 **VPP 接口**地址；`vrf` 经 VPP 路径） |
| `traceroute <host> [vrf <name>]` | 路径跟踪 | O | 宿主侧 raw ICMP | ✅（`vrf` **不支持**并明确报错，附录 A #36） |
| `monitor interfaces <ifname> [interval <sec>]` | 实时刷新计数（Ctrl-C 退出） | O | 服务端单次快照 + 前端轮询 | ✅ |
| `monitor vnf <name>` | 跟踪 VNF 状态/事件 | O | 运行态 | ✅ |
| `clear interfaces statistics [<ifname>]` | 清零统计计数 | S | 运行态（VPP） | ✅ |
| `start shell` | 进入系统 shell（仅本地控制台） | S | 宿主 shell | ✅（SSH 登录禁用） |
| `?` | 上下文命令/补全项 | R | 本地（命令树） | ✅（REPL 内；`-c` 脚本模式不适用） |
| Tab | 补全（唯一自动/多匹配列出） | R | 本地 | ✅ |

---

## 2. 配置模式命令树（提示符 `nfvis#`，全部 S）

> 配置命令经**事务引擎**：`candidate` 累积 → `commit` 校验并下发（失败自动补偿）→ `/events` 推送。
> 下表「落点」为 commit 时的**下发子系统**。

### 2.1 导航与事务（固定命令）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `edit <path>` / `up` / `top` / `exit` | 层级导航（提示符 `[edit path]`） | 会话态 | ✅ |
| `set <path> …` | 设置配置语句 | 事务引擎 | ✅ |
| `delete <path> …` | 删除语句/子树 | 事务引擎 | ✅ |
| `show` | 显示 candidate（当前层级） | candidate | ✅ |
| `show \| display set` | 展开为 set 语句 | candidate | ✅ |
| `annotate <path> "text"` | 节点注释（**路径相对当前层级**） | candidate annotations | ✅（**决策 #76⑤** 修相对路径） |
| `commit` | 提交（FR-CFG-002/003） | 事务引擎 → Applier | ✅ |
| `commit check` | 仅校验不下发 | 事务引擎 | ✅ |
| `commit confirmed [min]` | 超时未确认自动回滚（默认 10 分钟） | 事务引擎 | ✅ |
| `commit and-quit` | 提交成功后退出配置模式 | 事务引擎 | ✅ |
| `rollback [n]` | 取历史快照为 candidate（需再 commit） | 配置历史 | ✅ |
| `load override\|merge <path>` | JSON 配置导入 | 事务引擎 | ✅ |
| `save <path>` | candidate 导出 JSON（0600） | 本地文件 | ✅ |
| `run <oper-command>` | 配置模式内执行操作命令 | 操作树 | ✅ |
| `discard` | 丢弃 candidate 并释放会话锁 | 会话态 | ✅ |

### 2.2 `system`（§2.2，FR-SYS-001/004/011/012/013、FR-SEC-003/008）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set system hostname <s>` | 主机名 | 宿主 hostname | ✅ |
| `set system timezone <tz>` | 时区 | 宿主 timedatectl | ✅ |
| `set system ntp server <ip\|host> [prefer]` | NTP 服务器（`prefer` 为无值 flag） | 宿主 NTP | ✅（**决策 #76②** 修复） |
| `set system dns server <ip> [secondary <ip>]` | DNS（主/备） | 宿主 resolv | ✅（**决策 #76⑥** 修复 secondary） |
| `set system api port <n>` | HTTPS 端口（默认 443） | nfvisd | ✅ |
| `set system api token-ttl-minutes <n>` | Token 有效期（默认 60） | nfvisd | ✅ |
| `set system api max-sessions <n>` | 并发会话上限（真限流） | nfvisd | ✅（决策 #71） |
| `set system api tls cert-file <p> key-file <p>` | 安装外部证书（立即生效） | nfvisd TLS | ❌ `未知语句: "key-file"` |
| `set system api tls self-signed regenerate` | 重签自签证书 | nfvisd TLS | ✅ |
| `set system management interface <ifname>` | 管理网卡（不得用于数据面） | 宿主 + 数据面隔离校验 | ✅（决策 #71/72） |
| `set system management ip address <ip-prefix>` | 管理口静态地址 | 宿主 netplan | ✅ |
| `set system management gateway <ip>` | 管理口默认网关 | 宿主 netplan | ✅ |
| `set system kernel nmi-watchdog <bool>` | NMI watchdog | GRUB 基线 | ✅ |
| `set system kernel transparent-hugepages <mode>` | THP 模式 | GRUB 基线 | ✅ |
| `set system kernel iommu <on\|off\|pt>` | IOMMU | GRUB 基线 | ✅ |
| `set system kernel tuned-profile <name>` | tuned profile | 宿主 tuned | ✅ |
| `set system kernel params <param>` | 附加内核参数（可多条） | GRUB 基线 | ✅ |
| `set system health thresholds cpu-temp-celsius <n>` | CPU 温度阈值（FR-SYS-012） | 告警巡检 | ✅ |
| `set system health thresholds disk-temp-celsius <n>` | 磁盘温度阈值 | 告警巡检 | ✅ |
| `set system health thresholds disk-used-percent <n>` | 磁盘使用率阈值 | 告警巡检 | ✅ |
| `set system syslog host <ip> [port <p>] [facility <f>] [severity <s>]` | 远程 syslog（RFC 5424） | 宿主 rsyslog | ✅（决策 #69） |
| `set system syslog local level <lvl>` | 本地日志级别 | 宿主日志 | ✅ |
| `set system syslog local retention-days <n>` | 日志保留天数（FR-SYS-013） | 宿主 logrotate | ✅ |
| `set system syslog local max-size-mb <n>` | 日志容量上限 | 宿主 logrotate | ✅ |
| `set system login user <n> password <s> class <c>` | 本地用户 | 配置库（哈希） | ❌ `未知语句: "password"`（语法树把 `<name>` 参数置于关键字之前） |
| `set system login class <n> allow <path>` | 自定义 class 允许项 | 配置库 | ❌ `语句未产生配置变更`（未映射到 `[]string`） |
| `set system login class <n> deny <path>` | 自定义 class 拒绝项 | 配置库 | ❌ 同上 |
| `set system login password-policy min-length <n>` | 口令最小长度 | 配置库 | ✅ |
| `set system login password-policy complexity <bool>` | 复杂度要求 | 配置库 | ✅ |
| `set system login password-policy expire-days <n>` | 口令有效期 | 配置库 | ✅ |
| `set system login password-policy lockout-threshold <n>` | 连续失败锁定阈值 | 配置库 | ✅ |
| `set system login password-policy lockout-minutes <n>` | 锁定时长（**独立语句**，决策 #76） | 配置库 | ✅ |
| `set system idle-timeout-minutes <n>` | CLI 空闲超时（默认 10，FR-SEC-005） | 会话态 | ✅ |

### 2.2b `protocols`（顶级层级，FR-NET-018）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set protocols lldp enable <bool>` | LLDP 全局启停 | VPP lldp 插件 | ✅ |
| `set protocols lldp advertisement-interval <n>` | 通告间隔 | VPP lldp 插件 | ✅ |
| `set protocols lldp interface <ifname> enable <bool>` | 按接口覆盖 | VPP lldp 插件 | ✅ |

### 2.3 `interfaces` 与 `bonds`（§2.3）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set interfaces <ifname> description <s>` | 描述 | 配置库 | ✅ |
| `set interfaces <ifname> disable` | 禁用接口 | VPP | ✅ |
| `set interfaces <ifname> mtu <n>` | MTU | VPP | ✅ |
| `set interfaces <ifname> sriov vf-count <n>` | 创建/回收 VF（FR-NET-004） | sysfs `sriov_numvfs` | ⊘ 无 PF/VF 时 commit 明确报错（不再静默无效，决策 #70） |
| `set interfaces <ifname> ingress-policy <name>` | 入向限速策略绑定 | VPP policer | ✅ |
| `set bonds <name> members [<seq>] <ifname>` | 聚合成员 | VPP bonding | ✅ |
| `set bonds <name> lacp mode <active\|passive> [interval <fast\|slow>]` | LACP 模式 | VPP bonding | ✅ |
| `set bonds <name> lacp disable` | 关闭 LACP（转静态聚合） | VPP bonding | ✅ |
| `set bonds <name> mtu <n>` | MTU | VPP bonding | ✅ |
| `set bonds <name> description <s>` | 描述 | 配置库 | ✅ |

### 2.4 `virtual-switches`（§2.4，FR-NET-010~015）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set virtual-switches <n> type <l2\|l3>` | 类型（创建后不可改） | VPP（BD / VRF） | ✅ |
| `set virtual-switches <n> vlan access <vlan>` | L2 默认 untag VLAN | VPP BD | ✅ |
| `set virtual-switches <n> gateway ip <ip-prefix>` | BVI 三层网关（可多条） | VPP BVI | ✅ |
| `set virtual-switches <n> gateway vrf <name>` | 网关所属 VRF | VPP | ✅ |
| `set virtual-switches <n> gateway acl-in\|acl-out <acl>` | 网关 ACL | VPP acl | ✅ |
| `set virtual-switches <n> ports [<seq>] interface <if> [trunk vlans <l>\|native <v>]` | 物理口成员 | VPP BD | ✅ |
| `set virtual-switches <n> ports [<seq>] vnf <vm> interface <vnic> [trunk vlans <l>]` | vhost-user 成员 | VPP + libvirt | ✅ |
| `set virtual-switches <n> ports [<seq>] container <ct> interface <vnic>` | 容器 memif 成员 | VPP + Docker | ✅ |
| `set virtual-switches <n> cross-connect <a> <b>` | 两端口直通（与 ports/gateway 互斥） | VPP | ❌ `未知语句: "2"`；且模型只有 bool 字段（CLI/模型语义不一致） |
| `set virtual-switches <n> l3-interface <if> ip address <p>` | L3 接口地址（v4/v6 多条） | VPP | ✅ |
| `set virtual-switches <n> l3-interface <if> acl-in <acl>` | L3 接口 ACL 绑定 | VPP acl | ✅ |
| `set virtual-switches <n> static-routes <prefix> next-hop <ip> [distance <n>]` | 静态路由（v4/v6） | VPP FIB | ✅ |
| `set virtual-switches <n> static-routes default next-hop <ip>` | 默认路由 | VPP FIB | ✅ |

### 2.5 高级网络功能（§2.5）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set acls <n> rule <seq> source <s> destination <d> protocol <p> [source-port <sp>] [destination-port <dp>] action <a>` | ACL 规则（五元组） | VPP acl | ✅ |
| `set acls <n> rule <seq> direction <ingress\|egress>` | 规则方向 | VPP acl | ✅ |
| `set nat source-pool <n> address-range <ip> to <ip>` | NAT 地址池 | VPP nat44 | ✅ |
| `set nat rules <seq> match source <p> virtual-switch <n> action interface <if> [source-pool <n>]` | 转换规则（出接口必填，决策 #38/#52） | VPP nat44 | ✅ |
| `set nat static <inside> to <outside>` | 1:1 静态发布 | VPP nat44 | ✅ |
| `set port-mirroring <n> source interface <if> direction <i\|e\|both>` | SPAN 源（物理口） | VPP span | ✅ |
| `set port-mirroring <n> source vnf <vm> interface <vnic> direction <…>` | SPAN 源（vNIC） | VPP span | ✅ |
| `set port-mirroring <n> analyzer interface <if>` | 分析口 | VPP span | ✅ |
| `set qos policies <n> cir <n> cbs <n>` | 限速策略（bps/bytes） | VPP policer | ✅ |

### 2.6 `resource-pools`（§2.6，FR-CMP-001/005、FR-SYS-002/003）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set resource-pools hugepages page-size <2M\|1G> count <n>` | 大页池（变更需 reboot） | GRUB + 宿主 | ✅ |
| `set resource-pools cpu isolated-cores <list>` | 隔离核（变更需 reboot） | GRUB + 宿主 | ✅ |
| `set resource-pools cpu numa node <n> cores <list>` | NUMA 亲和声明 | 配置库（校验拓扑） | ✅（**决策 #76⑦** 修复） |

### 2.7 `vpp`（§2.9，FR-SYS-008/009）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set vpp cpu main-core <n>` | 主线程绑核（须在隔离核池内） | VPP startup.conf | ✅ |
| `set vpp cpu corelist-workers <list>` | worker 核列表 | VPP startup.conf | ✅ |
| `set vpp cpu workers-per-numa <n>` | 按 NUMA 分配 worker（与上互斥） | VPP startup.conf | ✅ |
| `set vpp memory main-heap-size <size>` | 主堆（默认 1G） | VPP startup.conf | ✅ |
| `set vpp memory buffers-per-numa <n>` | 每 NUMA buffer 数（默认 16385） | VPP startup.conf | ✅ |
| `set vpp memory hugepage-preference <2M\|1G>` | 大页偏好（须与资源池一致） | VPP startup.conf | ✅ |
| `set vpp dpdk dev rx-queues\|tx-queues\|rx-descriptors\|tx-descriptors <n>` | **全局默认**队列/描述符 | VPP startup.conf | ✅（**决策 #76①** 修复，此前完全不可用） |
| `set vpp dpdk dev <ifname> rx-queues\|… <n>` | 单网卡覆盖（须为 DPDK 物理口） | VPP startup.conf | ✅ |
| `delete vpp dpdk dev <ifname> [<参数>]` | 删除覆盖项（整体/单项回落全局默认） | VPP startup.conf | ✅ |
| `set vpp dpdk uio-driver <vfio-pci\|igb-uio>` | UIO 驱动 | VPP startup.conf | ✅ |
| `set vpp plugins <name> state <enable\|disable>` | 插件开关 | VPP startup.conf | ✅ |

### 2.8 `virtual-machine-functions`（§2.7，FR-CMP-010~019）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set virtual-machine-functions <n> image <img>` | 引用 vm-image | libvirt | ✅ |
| `set virtual-machine-functions <n> vcpu count <n> [pin <bool>]` | vCPU（隔离核池分配） | libvirt | ✅ |
| `set virtual-machine-functions <n> memory size-mb <n>` | 内存（大页池分配） | libvirt | ✅ |
| `set virtual-machine-functions <n> memory hugepage-size <2M\|1G>` | 页大小 | libvirt | ✅ |
| `set virtual-machine-functions <n> memory numa node <n>` | NUMA 亲和 | libvirt | ✅ |
| `set virtual-machine-functions <n> memory backing <hugepage\|normal>` | 内存类型（normal 禁 vhost-user） | libvirt | ✅ |
| `set virtual-machine-functions <n> disks <d> size-gb <n>` | 附加空数据盘（FR-CMP-018） | libvirt | ✅ |
| `set virtual-machine-functions <n> disks <d> image <img>` | 从镜像克隆数据盘 | libvirt | ✅ |
| `delete virtual-machine-functions <n> disks <d>` | 删除数据盘 | libvirt | ✅ |
| `set virtual-machine-functions <n> interfaces <vnic> type <vhost-user\|sriov-vf>` | vNIC 类型 | libvirt + VPP | ✅ |
| `set … interfaces <vnic> sriov physical-interface <if> vf <n>` | SR-IOV 直通 | libvirt | ⊘ 无 PF/VF 环境 |
| `set … interfaces <vnic> mac <mac>` | MAC（缺省自动生成） | libvirt | ✅ |
| `set … interfaces <vnic> vlan <vlan>` | VLAN tag | libvirt | ❌ 类型错（写入字符串，模型为 `int`） |
| `set … interfaces <vnic> virtual-switch <name>` | 所属 L2 交换机 | VPP | ✅ |
| `set … cloud-init user-data <path\|text>` | user-data 注入（FR-CMP-016） | seed ISO | ✅ |
| `set … cloud-init ssh-key <key>` | SSH 公钥（可多条） | seed ISO | ❌ `未知语句`：公钥含空格，而 `set` 不支持多词取值/引号 |
| `set … cloud-init hostname <s>` | guest 主机名 | seed ISO | ✅ |
| `set … serial console enable` | 串口控制台（默认启用） | libvirt | ✅ |
| `set … autostart <bool>` | 随系统自启 | libvirt | ✅ |
| `set … description <s>` | 描述 | 配置库 | ✅ |

### 2.9 `container-functions`（§2.8，FR-CMP-020~022）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set container-functions <n> image <img>` | 引用 container-image | Docker | ✅（**须与 Docker tag 同名**，见 §4①） |
| `set container-functions <n> vcpu count <n>` | cgroup CPU 限制 | Docker | ✅ |
| `set container-functions <n> memory size-mb <n>` | cgroup 内存限制 | Docker | ✅ |
| `set container-functions <n> interfaces <vnic> type memif virtual-switch <n> [mac <m>] [vlan <v>]` | memif vNIC | VPP + Docker | ❌ `未知语句: "virtual-switch"`（`type` 取值后同级关键字不可达） |
| `set container-functions <n> env <key> <value>` | 环境变量（可多条） | Docker | ❌ `未知语句: "<值>"`（取值节点未被消费） |
| `set container-functions <n> command <s>` | 入口命令 | Docker | ✅ |
| `set container-functions <n> args <s>` | 命令参数 | Docker | ✅ |
| `set container-functions <n> restart-policy <no\|on-failure>` | 重启策略 | Docker | ✅ |
| `set container-functions <n> autostart <bool>` | 随系统自启 | Docker | ✅ |
| `set container-functions <n> description <s>` | 描述 | 配置库 | ✅ |

---

## 3. 统计

> 口径：**命令形态**——同一命令的二级子命令各计一行（如 `show interfaces <if> detail|statistics|sriov` 计 3 行）。
> 计数由本表实际行数得出，可用 `grep -c '^| \`' docs/NFViS-CLI命令全表.md` 复核。

| 类别 | 命令形态数 | 明细 |
|---|---|---|
| `show` | 72 | 含二级子命令 |
| `request` | 46 | 含 VM/容器/镜像/接口/SR-IOV/VPP/系统/告警 |
| 其余操作命令 | 9 | `configure`、`exit`/`quit`、`ping`、`traceroute`、`monitor`×2、`clear`、`start shell`、`help`（`?`/Tab 为交互行为，另计） |
| 通用管道 | 7 | `match`/`except`/`count`/`last`/`begin`/`display json`/`display xml` |
| 配置模式 | 129 | 导航与事务 15、system 33、protocols 3、interfaces&bonds 10、virtual-switches 13、高级网络 9、resource-pools 3、vpp 11、vmf 20、container-functions 12 |
| **合计** | **256** | 不含管道则为 249 |

**按实测状态分布**（共 256 行）：

| 状态 | 行数 | 说明 |
|---|---|---|
| ✅ 实测通过 | 227 | 真机 CLI 逐条执行通过 |
| ❌ 确认不可用 | 9 | 契约已声明但经 CLI 用不了——**缺陷，待修**（见 §4⑦） |
| ⚠️ 已知缺口 | 2 | `show vpp runtime` 未接入；`request system api token revoke` 为 V1 明确延期 |
| ⊘ 预期报错 | 4 | SR-IOV 2 条 + VNF 端口/`ping source` 类环境受限项 |
| 🚫 本轮未执行 | 12 | 破坏性（reboot/shutdown/zeroize/format-data/software add/restore/kernel apply 等）与需交互者（删除确认、改密） |

另有全功能 CLI 冒烟（`contrib/scripts/cli-fulltest.sh`）结果：**通过 197 / 失败 1 / 预期报错 2**
（该脚本按阶段组织，覆盖其可执行子集）。

---

## 4. 已知限制与缺口（务必先读）

① **容器镜像目录名须等于 Docker tag**：`request images upload name X` 的目录项名与
   `docker load` 落地的 tar 内嵌 tag 不是一回事。二者不一致时该镜像**用哪个名字都不可用**——
   用目录名则下发 Docker API 404（`docker: not found`），用 tag 则被校验拒为「仓库中不存在镜像」。
   唯一可用组合是上传时 `name` 恰好写成 tag（如 `alpine:3.20`）。**待产品决策**（规格书 §12 V2）。

② `show vpp runtime` 未接入（govpp runtime 解码，附录 A #34）——CLI 明确提示「未接入」。

③ `request sriov delete-vfs` **忽略 `vf <n>` 参数**（按计数递减），登记 V2。

④ 快照 `create`/`rollback` **需关机态**（决策 #75）：对运行中 VM 回滚实测会**替换 QEMU 进程**
   （静默重启），故显式拒绝并提示先关机。

⑤ `request system api token revoke` 仅提示「经 API DELETE /login 吊销当前会话」，逐 token 吊销随 V2。

⑥ 环境受限（非实现问题）：SR-IOV（本机无 PF/VF）、LLDP 邻居（无对端）、
   硬件健康（无 IPMI/温度传感器/SMART）、容器侧 memif 通流（离线无自带 memif 的镜像）。

⑦ **本表编制过程中补测发现的 8 处「契约已声明但经 CLI 用不了」**（同 §4①~⑤ 一类，
   均在 `cli_mapping_test.go:contractStatements` 清单之外，故此前漏网）。**尚未修复**，
   表中对应行标 ❌：

| # | 语句 | 现象 |
|---|---|---|
| 1 | `set system api tls cert-file <p> key-file <p>` | `未知语句: "key-file"` |
| 2 | `set system login user <n> password <s> class <c>` | `未知语句: "password"`（`<name>` 参数被置于关键字之前） |
| 3 | `set system login class <n> allow <path>` | `语句未产生配置变更`（未映射到 `[]string`） |
| 4 | `set system login class <n> deny <path>` | 同上 |
| 5 | `set virtual-switches <n> cross-connect <a> <b>` | `未知语句: "2"`；且模型仅有 `cross_connect bool`（CLI 两端口语义与模型不一致，**需设计决策**） |
| 6 | `set virtual-machine-functions <n> interfaces <vnic> vlan <n>` | 类型错：写入字符串，模型 `Vlan int` |
| 7 | `set … cloud-init ssh-key <key>` | `未知语句`：**SSH 公钥必含空格**，而 `set` 不支持多词取值/引号（影响 FR-CMP-016 的 CLI 注入路径） |
| 8 | `set container-functions <n> interfaces <vnic> type memif virtual-switch <n>` / `set container-functions <n> env <key> <value>` | 前者 `未知语句: "virtual-switch"`；后者 `未知语句: "<值>"` |

其中 **2/5 需要设计决策**（口令哈希的 CLI 落地路径、cross-connect 的模型语义），
**7 需要 CLI 前端的取值引号机制**（影响面较广）；其余 4 处为同类映射补齐。
建议作为**独立一批**修复并补入 `contractStatements` 守护。
