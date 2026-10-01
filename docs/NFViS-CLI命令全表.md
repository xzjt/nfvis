# NFViS CLI 命令全表

| 文档属性 | 内容 |
|---|---|
| 用途 | **命令参考全表**：把 CLI 命令树逐条列出，附权限、落点与**真机实测状态** |
| 来源 | 命令树取自实现（`internal/schema/tree_oper.go`、`internal/schema/tree_config.go`，即 `?` 补全与 `cli_bridge` 前置校验的真实来源）；契约见 `docs/NFViS-CLI命令树完整设计.md`；REST 落点对照见 `docs/NFViS-openapi.yaml` 与 `internal/api/server.go` 的路由注册 |
| 实测状态 | 来自 **2026-09-29 round88 真机实测**（nfvis-vm，已装发布件走查 + 修复版复验）：`contrib/scripts/cli-fulltest.sh`（全功能 CLI 套件，**通过 195 / 失败 0 / 预期报错 12** —— `show vpp runtime` 已实现，全表**首次无失败**）+ `contrib/scripts/cli-pty-smoke.sh`（pty 交互冒烟，**通过 10 / 失败 0**）+ `cli-semantic-check.sh` **12 / 0 / 1**、`cli-lifecycle-check.sh` **21 / 0 / 3**。**更早轮次的状态已过期**，本表**上一轮（2026-09-14）的状态已过期**，本表一律以 round80 为准；**v2 开发线的当前基线见 §3 末尾**（决策 #319 后：fulltest 210/0/13（实测修正，round101）、语义 24/0/1、生命周期 21/0/3、pty 10/10，真机复跑待执行） |
| 基线 | main（round88 修复版工作树）；决策 **201** 项 |

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
| ✅ | 真机 CLI 实测通过（round80 套件直接覆盖，或本轮等价的同一实现已被逐条覆盖） |
| ⚠️ | **已知缺口**：未实现但**明确提示**（非静默空值，也不是静默返回别的正文） |
| ⊘ | **预期报错**：环境受限或防呆守卫正确拒绝——报错即正确行为 |
| 🚫 | **本轮未执行**：破坏性/需交互（测试机不宜执行）或环境上做不了——**不是「未实现」**，真值以代码与单测为准 |

**实测列的覆盖面（如实登记）**：round80 套件覆盖的是「非破坏性、可脚本化」的那部分命令——
`show` 族（§1.1）逐条覆盖；配置语句（§2）覆盖 **58 条**契约语句（+1 批前置对象，每条语句在独立会话里执行，
避免一条失败掩盖其余）；`request` 族覆盖 VM/容器生命周期、快照（含「运行中拒绝」的守卫用例）、
接口启停、抓包、诊断归档/备份/ntp/tls/ssh-key 回退、镜像删除、告警清除。
**未进套件**的命令分三类，其记号沿用上一轮并已按本轮代码核对：破坏性命令（`reboot`/`shutdown`/`poweroff`/
`zeroize`/`format-data`/`software add`/`configuration restore`/`kernel apply`/`kernel rollback`；
其中 `zeroize` 与 `format-data` 均为**已实现的破坏性命令**，标记 ✅ 表示「破坏性但已验」——实现与单测逐条覆盖，
真机执行按交付报告步骤在测试机单独走查，不进全功能套件）、
需交互输入者（VM/容器删除确认、改密）、本机环境不具备者（SR-IOV 无 PF/VF、LLDP 无对端、
无 IPMI/温度传感器/SMART、离线无自带 memif 的容器镜像）；
其余行（§2 中未被套件逐条执行的配置语句，以及 `request … console`、`bind-dpdk`/`unbind-dpdk`、
`request vpp restart`、`wizard`、`start shell` 等）也沿用上一轮的真机结论，本轮按代码与单测复核、未发现回归。

**这道「覆盖面」现在受守护约束（决策 #319）**：可执行命令要么出现在 `cli-fulltest-phase*.sh` 的命令清单里，
要么逐条登记进 `contrib/scripts/cli-fulltest-exemptions.tsv`（命令/类别/理由三列，理由须写明**改由谁覆盖**）；
守护 `contrib/scripts/check_suite_contract_sync.sh`（自带桩式自校准）随 `make check` 的 `toolcheck` 跑。
本表 §2 有 **70 条**命令因此登记为豁免（类别与理由见该文件；多数是「配置语句未逐条执行」，替代覆盖是
schema/api 单测、lifecycle 套件或真机单独走查）。**全局 CLI 选项**（`-c`/`-f`/`--yes`/`-server` …）不是
命令表里的命令，**不受该守护管辖**——`-f` 的覆盖写在 `cli-fulltest-phase7.sh`（决策 #309/#319，如实登记）。

**落点**：`POST /cli/execute` 是所有 CLI 命令的统一入口；表中「落点」列给出该命令**实际作用的**
等价 REST 端点或底座子系统。说明理由：CLI 执行器对 `show`/`request` 族**直连运行态 Provider**
（与对应 GET 端点同源，不经自身 HTTP，决策 #49~#51）；配置模式经**事务引擎**（candidate → commit → Applier）。
`show` 族里有若干**等价写法**（`physical` 可省、`protocols lldp` ≡ `lldp`、空格形态 ≡ 管道形态、`sessions` 两处写法），
它们**共用同一实现**、输出逐字相同（决策 #153），表里各自列出但落点相同。

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
| `show system configuration sessions` | candidate 持锁会话列表（FR-CFG-009）；列 Holder/Session/User/Acquired/Last-Activity/Dirty（会话标识与所属用户，决策 #317） | `GET /system/configuration/sessions` | ✅ |
| `show configuration sessions` | **等价写法**（与上一条**同一实现**、输出逐字相同；本轮起多余 token 会报错，不再静默回配置正文） | 同上 | ✅（round80 真机复验：与上一条输出一致） |
| `show interfaces` | 接口运行态清单：行 = 配置声明 ∪ VPP 运行态口 ∪ **内核未接管口**（决策 #302，收口 round81 F1：首装在接管前也能看见网卡；内核口在备注列标注「未接管」，不编造 VPP 侧事实），Admin/Link/Speed/Driver/计数全取运行态（决策 #155；仅声明未生效的行状态列 - 并标注，纯运行态口标注「未声明」） | `GET /interfaces` | ✅ |
| `show interfaces physical` | **与上一条完全等价**（决策 #155：`physical` 选择器退役为等价写法；原「仅声明口聚合+空态提示」口径废止） | `GET /interfaces` | ✅ |
| `show interfaces physical <ifname> detail` | 运行态单口视图（与裸写法同一实现，决策 #155） | 运行态（VPP） | ✅ |
| `show interfaces physical <ifname> statistics` | 收发包/字节/错误/drop | VPP 运行态统计 | ✅ |
| `show interfaces physical <ifname> sriov` | VF 列表与占用状态 | sysfs SR-IOV | ✅（无 PF/VF 时为空列表） |
| `show interfaces management` | 管理口（内核侧，IP/链路） | 运行态（内核） | ✅ |
| `show interfaces <ifname> detail` | **≡ `show interfaces physical <ifname> detail`（全形态等价、同一实现）**；回**运行态单口视图**——已声明与未声明但在 VPP 清单里的口（派生口 bvi0/vh-* 等）都答，候选 advertise 的名字必须答得上来（决策 #154/#155）；内核侧未接管的物理口回**内核事实视图**（驱动/MAC/速率/Admin/Link/MTU 取 sysfs，注明「未被 VPP 接管」；决策 #302）；接口配置视图在配置模式 `edit interfaces <ifname>` + `show` | 运行态（VPP）；内核口取 sysfs；配置视图走配置模式层级 show | ✅ |
| `show interfaces <ifname> statistics` | 同上（等价写法）；未接管口如实说明无数据面统计（决策 #302） | VPP 运行态统计 | ✅（round80 逐条比对：两侧输出逐字相同） |
| `show interfaces <ifname> sriov` | 同上（等价写法） | sysfs SR-IOV | ✅ |
| `show virtual-switches` | 全部虚拟交换机摘要 | `GET /virtual-switches` | ✅ |
| `show virtual-switches <name> detail` | 类型/成员端口/VLAN/VRF | `GET /virtual-switches/{name}` | ✅ |
| `show virtual-switches <name> ports` | 成员端口及状态/计数（配置静态 ports ∪ VNF/容器声明派生，标注 source） | `GET /virtual-switches/{name}/ports` | ✅ |
| `show virtual-switches <name> mac-table` | MAC 学习表（仅 L2） | `GET /virtual-switches/{name}/mac-table` | ✅ |
| `show virtual-switches <name> statistics` | 每端口收发计数 | `GET /virtual-switches/{name}`（statistics 字段） | ✅ |
| `show vrfs` | L3 交换机（VRF）列表 | `GET /vrfs` | ✅ |
| `show vrfs <name>` | detail：L3 接口/地址/路由 | `GET /vrfs/{name}` | ✅ |
| `show vrfs <name> routes` | FIB 路由表 | `GET /vrfs/{name}/routes` | ✅（VRF 不存在时明确报错，决策 #76④） |
| `show acls` | ACL 列表 | `GET /acls` | ✅ |
| `show acls <name> detail` | 规则与绑定详情 | `GET /acls/{name}` | ✅ |
| `show nat` | NAT 池/规则/转换会话计数 | `GET /nat` | ✅ |
| `show port-mirroring` | SPAN 会话状态 | `GET /port-mirroring` | ✅ |
| `show qos policies` | 限速策略与绑定 | `GET /qos/policies` | ✅ |
| `show vpp` | 数据面概览：**版本/连接/待重启**/线程/buffer/内存 | `GET /vpp/status` | ✅（发现 #11 补齐前三项） |
| `show vpp threads` | main/worker 线程清单与绑核 | 运行态（govpp threads） | ✅ |
| `show vpp runtime [thread <id>]` | **线程级**运行态：每线程向量率/主循环速率 + 整机向量率 + 工作线程数 + 数据面运行时长 | 运行态（stats segment，经 `vpp_get_stats` 解码，与 buffer/接口计数同源） | ✅（决策 #200；按节点明细无结构化来源，CLI 如实说明需 `vppctl show runtime`） |
| `show vpp buffers` | buffer 池（每 NUMA）用量；打印统计来源 | 运行态（statsclient ‖ `vpp_get_stats`，决策 #68） | ✅ |
| `show vpp memory` | main-heap 与 hugepage 占用 | 运行态 | ✅ |
| `show vpp capture` | 抓包会话状态与已导出 pcap 清单 | `GET /vpp/capture` | ✅ |
| `show bonds` | 链路聚合列表 | `GET /bonds` | ✅ |
| `show bonds <name> detail` | 成员口 link/LACP actor-partner | `GET /bonds/{name}` | ✅ |
| `show lldp neighbors [interface <ifname>]` | LLDP 邻居表；`interface <ifname>` **按口过滤真的生效**（本轮起）：无匹配给「接口 X 无 LLDP 邻居」、接口未知给明确报错，都不再回全量表 | `GET /protocols/lldp/neighbors` | ✅（本机无对端 → 空表/无匹配文案） |
| `show protocols lldp neighbors` | **等价写法**（与上一条**同一读物、同一实现**，输出逐字相同） | 同上 | ✅ |
| `show virtual-machine-functions` | VM 列表 | `GET /virtual-machine-functions` | ✅ |
| `show virtual-machine-functions <name> detail` | 域 XML 摘要/资源分配/NUMA | `GET /virtual-machine-functions/{name}` | ✅ |
| `show virtual-machine-functions <name> interfaces` | vNIC：类型/MAC/socket/交换机 | 同上 | ✅ |
| `show virtual-machine-functions <name> statistics` | vhost-user 口计数（经 VPP） | `GET /virtual-machine-functions/{name}`（statistics 字段） | ✅ |
| `show virtual-machine-functions <name> snapshots` | 快照列表 | `GET /virtual-machine-functions/{name}/snapshots` | ✅ |
| `show container-functions` | 容器列表 | `GET /container-functions` | ✅ |
| `show container-functions <name> [detail]` | 容器详情 | `GET /container-functions/{name}` | ✅ |
| `show container-functions <name> interfaces` | memif vNIC 列表 | 同上 | ✅ |
| `show images` | 镜像仓库列表 | `GET /images` | ✅ |
| `show images <name> detail` | 类型/大小/sha256/引用计数 | `GET /images/{name}` | ✅ |
| `show resource-pools` | 大页池/隔离核：总量、已分配、空闲（含 vpp-reserved） | `GET /resource-pools` | ✅ |
| `show alarms [active\|all]` | 告警列表（未同步时消息后标 `[时钟未同步]`） | `GET /alarms` | ✅ |
| `show log system [level <lvl>] [last <n>]` | 系统日志 | 服务端日志文件 | ✅ |
| `show log audit [last <n>]` | 审计日志 | `GET /audit-logs` | ✅ |
| `show log vnf <name> [last <n>]` | VNF 控制台/事件日志 | 运行态 | ✅ |
| `show users` | 本地用户与 class | `GET /system/login-users` | ✅ |
| `show system api tokens` | 活动会话 / API Token 清单：token-id、用户、权限类、签发时间、过期时间、是否当前会话（super-user 列**全部用户**的会话，其他 class 只列自己的；token 为内存态，重启后清空） | `GET /system/api-tokens` | 🚫 本轮新增（决策 #301）：单测覆盖；已入 fulltest（`show system api tokens` 见阶段 1；吊销不存在的 id 见阶段 4）与 semantic S10-8（吊销真实会话 ⇒ 其下一个请求 401），真机复跑待执行（决策 #319） |
| `show configuration [permissions <class> [detail]]` | 省略子命令 = 当前 committed 配置（JunOS 风格）；`permissions <class>` = 该 class 的**生效权限视图**（决策 #304）——按顶层命令族列出**允许路径** + 末行汇总（class、来源＝预置/自定义、允许/拒绝条数），`detail` 逐路径附判定依据（预置等级满足 / allow 前缀命中 / deny 前缀命中 / 默认拒绝）；判定单源在 `internal/aaa`，与运行期授权同一实现（不在 show 层另写一套） | 省略子命令：`GET /configuration`（committed 视图）；`permissions <class>`：`GET /configuration/permissions?class=<name>` | ✅ 本轮落地（决策 #304）：默认/detail/display set 三形态；R 类，read-only 仅可查自己所属 class，非 super-user 查他人拒绝、未知 class 报错。单测覆盖；真机覆盖已在 fulltest 阶段 5（三种形态各一条），真机复跑待执行（决策 #319） |
| `show configuration candidate` | 当前持锁会话的 candidate | `GET /configuration/candidate` | ✅ |
| `show configuration history` | 提交历史快照列表：rev/时间/用户/注释/是否当前（**不含配置正文**） | `GET /configuration/history` | ✅ |
| `show configuration compare rollback <n>` | 与第 n 个历史快照比对；与管道形态 `\| compare rollback <n>` **等价**（同一实现） | `GET /configuration/diff` | ✅ |
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
| `\| compare` | candidate ⇄ committed 差异（配置模式 `show \| compare`） | ✅（发现 #4 接线） |
| `\| compare rollback <n>` | committed ⇄ 第 n 个历史快照差异（与空格形态 `show configuration compare rollback <n>` **等价**，同一实现） | ✅（发现 #4 接线） |

### 1.2 `request`（运维动作，O；标 S 者为破坏性）

| 命令 | 说明 | 权限 | 落点 | 实测 |
|---|---|---|---|---|
| `request virtual-machine-functions <n> start` | 启动 VM；受理后回读域状态（决策 #311）；VPP 不可用时秒级前置失败（决策 #314） | O | `POST /vmf/{n}:start` | ✅（停在 `paused`/`crashed` 等非预期态时以 `%%` 报出域状态、libvirt 日志摘录与恢复建议；VPP 停时秒级 `%%` 报「数据面（VPP）当前不可用…」+ `request vpp restart`，不再阻塞到超时） |
| `request virtual-machine-functions <n> stop` | 停止（ACPI 关机，超时强杀） | O | `POST /vmf/{n}:stop` | ✅（**决策 #76③** 修超时误报） |
| `request virtual-machine-functions <n> restart` | 重启（运行中 ACPI；已关机的 off→start 分支同 `start` 前置判定，决策 #314） | O | `POST /vmf/{n}:restart` | ✅ |
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
| `request images upload name <n> type <t> file <path>` | 从 `/data/incoming/` 导入（成功自动清理源文件）；容器镜像读出 tar 内嵌 tag 并回显（决策 #312） | O | `POST /images` | ✅ |
| `request images download name <n> type <t> url <u> sha256 <hex>` | 从 HTTP(S) 拉取；**`sha256` 必填**（键值形态，非可选——校验层在受理前同步强制，缺省即拒） | O | `POST /images` | ✅（FR-SEC-004） |
| `request images delete name <n>` | 删除镜像（引用检查） | S | `DELETE /images/{n}` | ✅ |
| `request interfaces <ifname> enable` | 启用接口 | O | `PUT /interfaces/{n}` | ✅ |
| `request interfaces <ifname> disable` | 禁用接口 | O | `PUT /interfaces/{n}` | ✅ |
| `request interfaces <ifname> bind-dpdk [uio-driver <d>]` | 绑定 DPDK 驱动（中断流量，需确认） | O | `PUT /interfaces/{n}/dpdk` | ✅（真机周期见决策 #72） |
| `request interfaces <ifname\|pci> unbind-dpdk [to-driver <d>]` | 解绑交还内核驱动 | O | `PUT /interfaces/{n}/dpdk` | ✅（提示确认；接管后须按 PCI） |
| `request sriov create-vfs <ifname> count <n>` | 创建 VF | O | `PUT /interfaces/{n}/sriov` | ⊘ 本机无 PF/VF，明确报错（round80 实测：报「不支持 SR-IOV」，未静默成功） |
| `request sriov delete-vfs <ifname> vf <n>` | 回收 VF | O | `PUT /interfaces/{n}/sriov` | ⊘ 同上（另：`vf <n>` 不参与定位——按数量回收，回显已明确说明，附录 A #94） |
| `request vpp restart` | 按 committed 配置重建数据面 + 恢复收敛 | S | `POST /vpp/restart` | ✅（返回成功即代表数据面可查询：等 VPP 起来**且**连接管理器换成新连接才返回；未重建则如实报「数据面连接在重启窗口内不可用」+ 指引，附录 A #315） |
| `request vpp trace start interface <if> [count <n>] [filter <acl>]` | 开始抓包 | S | `POST /vpp/capture` | ✅ |
| `request vpp trace stop` | 停止抓包（不导出） | S | `DELETE /vpp/capture` | ✅ |
| `request vpp trace export [name <n>]` | 导出 pcap（**隐含 stop**） | S | `DELETE /vpp/capture` | ✅ |
| `request system software add <deb\|url> [sha256 <hex>]` | 安装升级包（校验→升级→重启 nfvisd） | S | `POST /system/software` | 🚫 破坏性 |
| `request system software rollback [to <v>]` | 回退版本 | S | `POST /system/software:rollback` | ✅ |
| `request system reboot` | 重启系统 | S | `POST /system:reboot` | 🚫 破坏性 |
| `request system shutdown` | 关机 | S | `POST /system:shutdown` | 🚫 破坏性 |
| `request system poweroff` | 断电 | S | `POST /system:shutdown` | 🚫 破坏性 |
| `request system kernel apply` | 写入 GRUB 内核基线（需重启生效） | S | `POST /system/kernel:apply` | 🚫 会改启动项，本轮不执行 |
| `request system kernel rollback` | 回退内核基线 | S | `POST /system/kernel:rollback` | 🚫 同上 |
| `request system configuration backup [to <path>]` | 导出 committed 配置归档（`to <path>` 是另存一份：须绝对路径、目标不得已存在、父目录须已存在；已存在即如实拒绝） | S | `POST /system/backup`（生成归档；只读清单是 `GET /system/backup`） | ✅ |
| `request system configuration restore <path>` | 导入归档为 candidate 并提交 | S | `POST /system/restore`（与 REST 同源，高危档审计两条，决策 #150） | 🚫 会覆盖现网配置 |
| `request system tech-support generate` | 生成诊断归档 tar.gz（归档里的配置是**脱敏视图**：口令哈希等已隐藏，不能用于恢复；要可恢复的完整配置用 `configuration backup`） | O | `POST /system/tech-support` | ✅ |
| `request system core-dumps export <url>` | 导出转储清单到 URL（POST JSON） | O | `POST /system/core-dumps:export` | ✅（决策 #126 修掉此前的假成功） |
| `request system core-dumps delete [file <n>]` | 删除转储 | O | `DELETE /system/core-dumps` | ✅（**决策 #76⑨** 修错误文案） |
| `request system zeroize` | 恢复出厂（双重确认） | S | `POST /system:zeroize` | 🚫 破坏性 |
| `request system api tls regenerate` | 重签自签证书 | S | `POST /system/tls:regenerate` | ✅ |
| `request system api token revoke <token-id>` | 吊销指定会话（O；范围按身份裁定——super-user 可吊销任意会话，其他 class 仅自己的；不存在的/他人的 id 统一报「会话不存在或无权操作」，不泄露存在性。token-id 见 `show system api tokens`。request 域以 O 为基线，read-only 的自助结束会话走 POST /logout；REST 侧同能力对全部登录 class 开放） | O | `POST /system/api-tokens/{id}:revoke` | 🚫 本轮新增（决策 #301）：单测覆盖；已入 fulltest（`show system api tokens` 见阶段 1；吊销不存在的 id 见阶段 4）与 semantic S10-8（吊销真实会话 ⇒ 其下一个请求 401），真机复跑待执行（决策 #319） |
| `request system ssh host-key regenerate` | 重新生成 SSH host key | S | `POST /system/ssh-host-key:regenerate` | ✅ |
| `request system password change` | 登录者自助改密（验证旧口令） | O | `POST /system/login-users/{n}:change-password`（与 REST 同源） | 🚫 需交互输入（契约已登记延期） |
| `request system storage format-data` | 恢复出厂数据状态（保留管理面可达）：收敛删全部受管 VNF/容器与网络对象、清受管数据目录、配置库重置为保留节（决策 #305） | S | `POST /system:format-data`（宿主编排；双确认照搬 zeroize） | ✅ 破坏性但已验（决策 #305 落地：收敛/保留/清数据与 zeroize 同一确认与审计口径，单测逐条覆盖；真机按交付报告步骤在测试机执行）。**fulltest 侧只做结构检查**（阶段 4：非交互下必须被双重确认问询挡住——命令已接线且破坏性闸门在位，**不真执行**），并在 `contrib/scripts/cli-fulltest-exemptions.tsv` 登记豁免（决策 #319） |
| `request system ntp sync` | 立即触发一次 NTP 同步 | O | `POST /system/ntp:sync`（宿主 chrony/ntpd） | ✅ |
| `request alarms clear [id <id> \| all]` | 清除已 resolved 告警 | O | `POST /alarms:clear` | ✅ |

### 1.3 其余操作命令

| 命令 | 说明 | 权限 | 落点 | 实测 |
|---|---|---|---|---|
| `configure` | 进入配置模式 | S | 本地（会话模式切换） | ✅ |
| `exit` / `quit` | 退出 CLI | R | 本地 | ✅ |
| `ping <host> [source <ip>] [count <n>] [vrf <name>]` | 经 VPP L3 连通性测试 | O | `vppctl ping`（CLI socket） | ✅（`source` 须为 **VPP 接口**地址；`vrf` 经 VPP 路径）。round80 套件里三条 ping 用例均记 ⊘：该实例到 `192.168.155.1` 不通（「0 发包 / 100% 丢包即报失败」的**判定自洽**通过，决策 #89——0 发包不再被算作通过） |
| `traceroute <host> [vrf <name>]` | 路径跟踪 | O | 宿主侧 raw ICMP | ✅（`vrf` **不支持**并明确报错，附录 A #36） |
| `monitor interfaces <ifname> [interval <sec>]` | 实时刷新计数（Ctrl-C 退出） | O | 服务端单次快照 + 前端轮询 | ✅（round80 实测不带 `interval` 的写法） |
| `wizard` | 初始化向导：问答规划资源池+内核基线并提交（CLI 端交互） | O | 本地（CLI 端交互编排，无 REST 端点；Web 等价物是向导式页面） | ✅（非 TTY 明确拒绝） |
| `monitor vnf <name>` | 跟踪 VNF 状态/事件（**真跟踪**，决策 #92） | O | 运行态 | ✅ |
| `clear interfaces statistics [<ifname>]` | 清零统计计数 | S | 运行态（VPP） | ✅（round80 实测：无参与带 `<ifname>` 两种写法都过） |
| `start shell` | 进入系统 shell（仅本地控制台） | S | 宿主 shell | ✅（SSH 登录禁用） |
| `?` | 上下文命令/补全项（**按键即时**，无需回车） | R | 本地（命令树） | ✅（REPL 内；`-c` 脚本模式不适用）——round80 pty 冒烟 10/10 覆盖按键即时、前缀过滤、候选行 CRLF |
| Tab | 补全（唯一自动/多匹配响铃并列出） | R | 本地 | ✅（pty 冒烟覆盖「多匹配响铃并列出」） |

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
| `show \| display set` | 展开为 `set` 语句（**已实现**，决策 #155：语句带绝对路径、敏感值不输出（注释说明）、生成后回放自校验；`show configuration \| display set` 同管道同实现） | candidate | ✅ |
| `annotate <path> "text"` | 节点注释（**路径相对当前层级**） | candidate annotations | ✅（**决策 #76⑤** 修相对路径） |
| `commit` | 提交（FR-CFG-002/003） | 事务引擎 → Applier | ✅ |
| `commit check` | 仅校验不下发 | 事务引擎 | ✅ |
| `commit confirmed [min]` | 超时未确认自动回滚（默认 10 分钟）；**管理口任何变更（含首次声明）必须走它** | 事务引擎 | ✅ |
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
| `set system api tls cert-file <p> key-file <p>` | 安装外部证书（立即生效） | nfvisd TLS | ✅（决策 #79 修复） |
| `set system api tls self-signed regenerate` | 重签自签证书 | nfvisd TLS | ✅ |
| `set system management interface <ifname>` | 管理网卡（不得用于数据面） | 宿主 + 数据面隔离校验 | ✅（决策 #71/72；**变更须 `commit confirmed`**，含首次声明） |
| `set system management ip address <ip-prefix>` | 管理口静态地址 | 宿主 netplan | ✅ |
| `set system management gateway <ip>` | 管理口默认网关 | 宿主 netplan | ✅ |
| `set system kernel nmi-watchdog <bool>` | NMI watchdog | GRUB 基线 | ✅ |
| `set system kernel transparent-hugepages <mode>` | THP 模式 | GRUB 基线 | ✅ |
| `set system kernel iommu <on\|off\|pt>` | IOMMU | GRUB 基线 | ✅ |
| `set system kernel low-latency <bool>` | 低延迟参数组（mitigations=off 等；显式选择；VM 上自动省略 idle=poll/tsc=reliable） | GRUB 基线 | ✅（2026-09-19 真机 apply/rollback） |
| `set system kernel tuned-profile <name>` | tuned profile | 宿主 tuned | ✅ |
| `set system kernel params <param>` | 附加内核参数（可多条） | GRUB 基线 | ✅ |
| `set system health thresholds cpu-temp-celsius <n>` | CPU 温度阈值（FR-SYS-012） | 告警巡检 | ✅ |
| `set system health thresholds disk-temp-celsius <n>` | 磁盘温度阈值 | 告警巡检 | ✅ |
| `set system health thresholds disk-used-percent <n>` | 磁盘使用率阈值 | 告警巡检 | ✅ |
| `set system syslog host <ip> [port <p>] [facility <f>] [severity <s>]` | 远程 syslog（RFC 5424） | 宿主 rsyslog | ✅（决策 #69） |
| `set system syslog local level <lvl>` | 本地日志级别 | 宿主日志 | ✅ |
| `set system syslog local retention-days <n>` | 日志保留天数（FR-SYS-013） | 宿主 logrotate | ✅ |
| `set system syslog local max-size-mb <n>` | 日志容量上限 | 宿主 logrotate | ✅ |
| `set system login banner <text>` | 登录横幅（显示在 Web 登录页与 CLI 登录提示之前，未认证即可见；单行，最长 512 字节，超限/含换行拒绝；`delete system login banner` 清除） | 配置库 | 🚫 本轮新增（决策 #303）：单测覆盖；已入 fulltest 阶段 2（`set`/`delete` 语句 + 提交→回读→清除往返，回读用内容断言），真机复跑待执行（决策 #319） |
| `set system login user <n> password <s> class <c>` | 本地用户（口令**加盐哈希**落库、回显脱敏） | 配置库（PBKDF2） | ✅（决策 #79 修复；**`<n>` 不可省**，把 `password`/`class` 写在名字位会被拒并提示正确写法，决策 #82） |
| `set system login class <n> allow <path>` | 自定义 class 允许项（可多条） | 配置库 | ✅（决策 #79 修复） |
| `set system login class <n> deny <path>` | 自定义 class 拒绝项（可多条） | 配置库 | ✅（决策 #79 修复） |
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
| `set interfaces <ifname> description <s>` | 描述（`<ifname>` 的 Tab 候选 = **内核未接管 ∪ 配置已声明 ∪ VPP 运行态**（决策 #302：首装在接管前也能补全到内核网卡名）；此前只取 VPP 中的接口，决策 #83） | 配置库 | ✅ |
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
| `set virtual-switches <n> cross-connect <a> <b>` | 两端口直通（与 ports/gateway 互斥） | VPP | ✅（决策 #79 修复：置 `cross_connect` 并校验两端口已声明） |
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
| `set … interfaces <vnic> vlan <vlan>` | VLAN tag | libvirt | ✅（决策 #79 修复：按 int 落库） |
| `set … interfaces <vnic> virtual-switch <name>` | 所属 L2 交换机 | VPP | ✅ |
| `set … cloud-init user-data <path\|text>` | user-data 注入（FR-CMP-016） | seed ISO | ✅ |
| `set … cloud-init ssh-key <key>` | SSH 公钥（可多条；**用双引号包住含空格的公钥**） | seed ISO | ✅（决策 #79 修复：引号感知切分 + `ssh_keys[]`） |
| `set … cloud-init hostname <s>` | guest 主机名 | seed ISO | ✅ |
| `set … serial console enable` | 串口控制台（默认启用） | libvirt | ✅ |
| `set … autostart <bool>` | 随系统自启 | libvirt | ✅ |
| `set … description <s>` | 描述 | 配置库 | ✅ |

### 2.9 `container-functions`（§2.8，FR-CMP-020~022）

| 命令 | 说明 | 落点 | 实测 |
|---|---|---|---|
| `set container-functions <n> image <img>` | 引用 container-image（候选名 = 导入时按目录项名重打标签的名字，决策 #312） | Docker | ✅ |
| `set container-functions <n> vcpu count <n>` | cgroup CPU 限制 | Docker | ✅ |
| `set container-functions <n> memory size-mb <n>` | cgroup 内存限制 | Docker | ✅ |
| `set container-functions <n> interfaces <vnic> type memif virtual-switch <n> [mac <m>] [vlan <v>]` | memif vNIC | VPP + Docker | ✅（决策 #79 修复） |
| `set container-functions <n> env <key> <value>` | 环境变量（多条，模型为 map） | Docker | ✅（决策 #79 修复） |
| `set container-functions <n> command <s>` | 入口命令 | Docker | ✅ |
| `set container-functions <n> args <s>` | 命令参数 | Docker | ✅ |
| `set container-functions <n> restart-policy <no\|on-failure>` | 重启策略 | Docker | ✅ |
| `set container-functions <n> autostart <bool>` | 随系统自启 | Docker | ✅ |
| `set container-functions <n> description <s>` | 描述 | 配置库 | ✅ |

---

## 3. 统计

> **口径**：**一行一个命令行**——同一命令的二级子命令各计一行（如 `show interfaces <if> detail|statistics|sriov` 计 3 行；
> `a` / `b` 并列写在**同一行**里只算 1 行）。数字全部由本表**实际行数**数出来，不含估算。
>
> **复核方法**（下面每个数字都可这样复算）：
>
> ```bash
> grep -c '^| `' docs/NFViS-CLI命令全表.md          # → 263（§1/§2 的命令行 261 行 + §3 本表的 `show`、`request` 两行）
> ```
>
> 即 §1/§2 合计 **261 行**；把 ` / ` 并列的写法各拆成一条后为 **265 条**命令
> （`exit` / `quit` +1；§2.1 的 `edit <path>` / `up` / `top` / `exit` +3）。

**分族**（族 = 该行**首个 token**；§2.1 的裸 `show` 与 `show | display set` 因此计入 `show` 族，`help` 计入其余操作）：

| 族 | 行数 | 明细 |
|---|---|---|
| `show` | 68 | §1.1 show 表 66 行 + §2.1 的 `show`、`show \| display set` 2 行 |
| `request` | 46 | §1.2 全部（VM/容器/镜像/接口/SR-IOV/VPP/系统/告警） |
| 其余操作命令 | 11 | §1.3 的 10 行（`exit` / `quit` 一行两命令）+ §1.1 的 `help [command]` 1 行 |
| 通用管道 | 9 | `match` / `except` / `count` / `last` / `begin` / `display json` / `display xml` / `compare` / `compare rollback <n>`（后两者是差异渲染，非文本过滤；发现 #4 接线） |
| 配置模式 | 127 | §2.1 余下 13 行 + §2.2~§2.9 共 114 行 |
| **合计** | **261** | 不含管道则为 **252**；按 ` / ` 拆开后 **265 条** |

**分节**（行数）：

| 节 | 行数 | 节 | 行数 |
|---|---|---|---|
| §1.1 `show`（含通用管道 9） | 76 | §2.2b `protocols` | 3 |
| §1.2 `request` | 46 | §2.3 `interfaces` 与 `bonds` | 10 |
| §1.3 其余操作命令 | 10 | §2.4 `virtual-switches` | 13 |
| §2.1 导航与事务 | 15 | §2.5 高级网络功能 | 9 |
| §2.2 `system` | 35 | §2.6 `resource-pools` | 3 |
| §2.7 `vpp` | 11 | §2.8 `virtual-machine-functions` | 20 |
| §2.9 `container-functions` | 10 | **合计** | **261** |

**按实测状态分布**（共 261 行）：

| 状态 | 行数 | 逐条 |
|---|---|---|
| ✅ 实测通过 | 243 | round80 套件直接覆盖的命令逐条执行通过；未进套件的行沿用上一轮真机结论，本轮按代码与单测复核（无回归）。`show configuration [permissions <class> [detail]]` 由决策 #304 落地（原「已知缺口」），移入本桶；`request system storage format-data` 由决策 #305 落地（原 🚫 破坏性、契约已登记延期），按「破坏性但已验」移入本桶 |
| ⚠️ 已知缺口 | 0 | 无——`show configuration permissions <class>` 已由决策 #304 落地；`show \| display set`（决策 #155）、`show vpp runtime`（决策 #200）、`request system api token revoke`（决策 #301）此前均已移出缺口 |
| ⊘ 预期报错 | 4 | SR-IOV 4 条环境受限项：`request sriov create-vfs`、`request sriov delete-vfs`、`set interfaces <ifname> sriov vf-count`、`set … interfaces <vnic> sriov physical-interface <if> vf <n>` |
| 🚫 本轮未执行 | 14 | 破坏性（`reboot`/`shutdown`/`poweroff`/`zeroize`/`software add`/`configuration restore`/`kernel apply`/`kernel rollback`）、需交互者（VM/容器删除确认、改密），以及本轮新增、单测已覆盖、**已入 fulltest 套件但真机复跑待执行**的 3 行（`show system api tokens` 阶段 1、`request system api token revoke <token-id>` 阶段 4、`set system login banner <text>` 阶段 2；决策 #319） |

round88 全功能 CLI 套件（`contrib/scripts/cli-fulltest.sh`）的逐阶段结果为
**通过 195 / 失败 0 / 预期报错 12**（阶段 1 的 42/0/0、阶段 2 的 59/0/0、阶段 3 的 8/0/0、
阶段 4 的 33/0/8、阶段 5 的 42/0/4、阶段 6 的 11/0/0）——**首次全阶段零失败**：
`show vpp runtime` 已实现（线程级运行态；按节点明细无结构化来源，CLI 如实说明），
round80 以来那条唯一 ✗ 归零。
**12 条预期报错**都是「环境受限或防呆守卫正确拒绝」：运行中快照被拒（1）、
无 PF/VF 时 `request sriov create-vfs`（1）、非交互下的高危动作需 `--yes`/确认词
（`reboot`/`shutdown`/`zeroize`/`software rollback`/`unbind-dpdk`，5）、镜像被引用（1）、
目标不可达时 `ping` 报失败（3）、`show configuration permissions <class>` 暂未实现（1——该条已由决策 #304 落地为**生效权限视图**，此后不再走预期报错，`contrib/scripts/cli-fulltest-phase5.sh` 同步由 `expect_fail` 改为 `run`；上面的 195/0/12 是 round88 当时的现场，保留为历史基线）。
pty 交互冒烟（`contrib/scripts/cli-pty-smoke.sh`）**通过 10 / 失败 0**；
语义校验 **12 / 0 / 1**、生命周期与组合 **21 / 0 / 3**（有业务现场时跑）。

**决策 #319 之后的基线（v2 开发线；数字为「旧基线 + 本轮增量」，真机复跑待执行）**：
`cli-fulltest` **212 / 0 / 13**（旧 198/0/11；增量逐阶段 = 阶段 1 `+1` `show system api tokens`、
阶段 2 `+4`（登录横幅语句 1 + 提交→回读→清除 3）、阶段 4 `+2`（`format-data` 结构检查、
吊销不存在的 token id，两条都进「预期报错」桶 ⇒ 11→13）、**新增阶段 7** `+7`（CLI 脚本文件模式 `-f`））、
`cli-semantic-check` **24 / 0 / 1**（旧 12/0/1；新增 **S10 配置编辑锁语义** 12 项，含 5 项正向控制，
覆盖 #317/#318 的排他/接管/`ErrLockLost`/登出释放）、`cli-lifecycle-check` **21 / 0 / 3**（不变）、
`cli-pty-smoke` **10 / 10**（不变）。逐阶段差异与原因见规格书附录 A #319⑤；
命令清单与契约的对账由 `contrib/scripts/check_suite_contract_sync.sh` 守护（`make check` 的 `toolcheck`）。

---

## 4. 已知限制与缺口（务必先读）

① **容器镜像命名：目录项名是唯一可用名（已收口，决策 #160 + #312）**：此前文档记的是
   「目录名须等于 Docker tag 否则不可用」——该**运行时缺陷已由决策 #160 修掉**：`docker load`
   之后按仓库目录项名**重打标签 `<名>:latest`**，故配置/候选里用目录项名恒可用；决策 #312
   进一步**读出 tar 内嵌 tag 并记入 `Image.source_tags`**、在导入输出与 `show images <名> detail`
   里回显（重命名不再静默），并加守护测试保证「候选 ↔ 实际可用」同源。**已知限制**：删除容器镜像
   只删 `<名>:latest`，归档内嵌的原始 tag（如 `alpine:3.20`）可能作为悬空引用留在 Docker。

② `show vpp runtime` **已实现（决策 #200）**：给**线程级**运行态（每线程向量率/主循环速率、整机向量率、工作线程数、数据面运行时长），数据源为 stats segment（经 `vpp_get_stats` 解码，与 buffer/接口计数同源）；VPP 26.06 的**按节点**明细无结构化来源，CLI 如实说明需 `vppctl show runtime`。round80 那条唯一 ✗ 随之关闭。
   round80 套件里**唯一一条 ✗**，属已登记缺口，不是本轮回归。

③ `request sriov delete-vfs` 收下 `vf <n>` 但**按数量回收**（编号不参与定位），
   回显已明确说明这一点（附录 A #94）；容器/VM 侧的 VF 直通项登记 V2。

④ 快照 `create`/`rollback` **需关机态**（决策 #75）：对运行中 VM 回滚实测会**替换 QEMU 进程**
   （静默重启），故显式拒绝并提示先关机。

⑤ **已收口**：`request system api token revoke <token-id>` 已实现逐 token 吊销（决策 #301，配套 `show system api tokens` 与 REST `GET /system/api-tokens`、`POST /system/api-tokens/{id}:revoke`；Web 控制台「用户与权限」页同步提供活动会话卡片）；会话级吊销（`POST /logout`）照旧。两条新命令的状态列标 🚫：真机四套件待跑。

⑥ 环境受限（非实现问题）：SR-IOV（本机无 PF/VF）、LLDP 邻居（无对端，邻居表恒空）、
   硬件健康（无 IPMI/温度传感器/SMART）、容器侧 memif 通流（离线无自带 memif 的镜像）、
   `ping` 目标不可达（round80 的实例环境到 `192.168.155.1` 不通——此时命令**正确地报失败**，
   而不是被算作通过，决策 #89）。

⑦ **本表编制过程中补测发现的 8 处「契约已声明但经 CLI 用不了」——已全部修复**（决策 #79）。
   这 8 处此前都在 `cli_mapping_test.go:contractStatements` 清单之外，故长期漏网；现已补入守护。
   根因与修法（逐条）：

| # | 语句 | 根因 → 修法 |
|---|---|---|
| 1 | `set system api tls cert-file <p> key-file <p>` | CLI 多一层 `tls`、模型扁平 → 7-token 别名 |
| 2 | `set system login user <n> password <s> class <c>` | 三层键名不一致（`user/class/password` vs `users/classes/password_hash`）且口令**必须哈希** → 别名 + `aaa.HashPassword`/`CheckPasswordPolicy`（与 REST 同源），回显脱敏为 `«已隐藏»` |
| 3/4 | `set system login class <n> allow\|deny <path>` | 模型是 `[]string`，原先写标量 → 按值追加/删除 |
| 5 | `set virtual-switches <n> cross-connect <a> <b>` | 模型只有 `cross_connect bool`（端口身份由 `ports` 承担）→ **保持模型不变**，置位 + 校验被引用端口已声明且恰为两个 |
| 6 | `set virtual-machine-functions <n> interfaces <vnic> vlan <n>` | ParamType `vlan` 让值保持字符串、模型是 `int` → `valueTransforms` 转数值 |
| 7 | `set … cloud-init ssh-key <key>` | SSH 公钥必含空格，`strings.Fields` 会拆开 → 解析入口改**引号感知切分** + `SPA(ssh_keys)` |
| 8 | 容器 `interfaces … type memif virtual-switch …` / `env <k> <v>` | 前者：取值关键字后的同级关键字不可达 → 解析器**就近向上回退**（`Node.parent`）+ vNIC 改 `SPD`；后者：模型是 map → 别名直接落 map |

同时做了三处**通用加固**（比单点修复更重要）：

- **`fromJSONTree`/`validateTreeJSON` 启用 `DisallowUnknownFields`**：此前 schema 键与模型键不一致时
  `encoding/json` **静默忽略**，语句"看似成功却没生效"（第 3 条正是这样被掩盖的）。
  现在整类「CLI 声明了但落不进模型」第一次执行即显式报错。
- **解析器末位实例参数即语句结束**（原先误报「缺少取值」）；**`SPA` 首值落数组**（`ssh_keys` 等以前首值被写成字符串）。
- **口令回显脱敏**：`set … password <pw>` 回显为 `«已隐藏»`——配置只存哈希、展示层已脱敏（决策 #70），回显不该例外。

⑧ **`show configuration permissions <class>` 已落地为「生效权限视图」（决策 #304）**：

- `show configuration permissions <class> [detail]`：给出该 class 在 CLI 命令树上的**有效判定**
  （默认按顶层命令族列允许路径 + 汇总；`detail` 逐路径附判定依据）。判定**单源在 `internal/aaa`**
  （与运行期授权同一实现）。R 类：read-only 仅可查自己所属 class，非 super-user 查他人拒绝（不泄露他人规则），
  未知 class 明确报错。`| display set` 对自定义 class 输出等价 `set system login class …` 语句，
  预置 class 由等级判定、无路径表，如实说明不编造语句。REST 等价 `GET /configuration/permissions?class=<name>`。
  原「语义未定义、不做 lossy 版本」的处置（决策 #153）由此收口——本命令**不渲染配置**，与「按 class 视角显示配置」
  不是一回事。
- `show | display set`：需要 model→CLI 的**反向映射**（别名语句无法由配置树反推），做 lossy 版本会在「复制配置」上
  制造静默错误（附录 A #84），故**明说仅支持 `json|xml`**。替代：`save <file>`（JSON）/ `show configuration`（块状）/
  `| display json`。

⑨ **round80 真机对拍收口的三处「声明与实际不同源」——已修复**（决策 #153）：

| # | 现象 | 修法 |
|---|---|---|
| 1 | `show configuration <未知子命令>`（含 `sessions`）**静默返回 committed 配置正文** | 按白名单判定子命令（`sessions`/`compare rollback <n>`/`history`/`candidate`），其余一律 `% 无效命令: …（可用：…）`，不再吐配置正文 |
| 2 | **补全漂移**：`show interfaces <ifname> detail\|statistics\|sriov`、`show protocols lldp neighbors`、`show configuration compare rollback <n>` 执行器能跑、命令树里没有（`?`/Tab 补不出来） | 三条补进命令树，并声明为**等价写法**（`physical` 可省、`protocols lldp` ≡ `lldp`） |
| 3 | 同批：`show interfaces physical <ifname> <子命令>` 把子命令整段丢掉（与「两种写法等价」矛盾）；`show lldp neighbors interface <ifname>` 的过滤参数被静默丢弃（问某口却回全量表） | 前者与省略 `physical` 的形态走**同一实现**；后者按口过滤，且「无匹配」与「接口未知」给**不同**的明确文案 |

⚠️ **运维告警（本批验证时踩到）**：`cross-connect` 是**二层直通、无 MAC 学习/无环路保护**。
若把**同一广播域**内的两个端口做直通（例如同一 VMware 虚拟交换机上的两块网卡），
会形成物理二层环路 → 广播风暴。做直通的两个端口必须属于**不同**广播域。

