# Web 界面覆盖审计（配置语句族）

> **范围**：CLI **配置语句族**（`set` / `delete` 的全部叶子语句）逐条对照 Web 控制台**界面控件**。
> 本文件是**只读审计产物**，不修改任何代码；判定基于仓内真源（命令树 + Web 前端），不取文档口径。
>
> **为什么要这份审计**：已有端点级矩阵 `docs/v3-Web-CLI覆盖矩阵.md` 证明 REST 层零缺口，但它把
> 151 条 `set` + 18 条 `delete` 按「candidate API 架构性覆盖」整族记为「已接」——**端点级看不见
> 「界面上有没有一个控件做这件事」**。本审计补上这一层：**叶子级**判定「界面上有没有对应表单字段/按钮」。

---

## 1. 口径与方法

### 1.1 数据来源（都是仓内真源）

| 侧 | 真源 | 说明 |
|---|---|---|
| CLI 语句 | `internal/schema/tree_config.go` 的 `ConfigPathTree()` | 命令树真源；`set`/`delete`（以及 `show`/`edit`/`annotate`）**共用同一棵路径树**（`ConfigRoot()` 里 `Su(K("set", …, pathChildren()...))` 与 `Su(K("delete", …, pathChildren()...))` 挂同一份 `ConfigPathTree().Children`）。故 `set` 与 `delete` 的**叶子集合逐条相同**。 |
| Web 控件 | `internal/api/ui/js/**`（主要是 `views/config.js`，另含 `system.js` / `overview.js` / `switches.js` / `network.js` / `vmDetail.js` / `images.js` / `deploy.js`） | 表单引擎的节定义（`CFG_SYS_SECTIONS` / `CFG_VPP_SECTIONS` / `CFG_SECTIONS`）、`addField(...)`（接口 / 资源池两节）、以及各详情/系统页的写控件（`api(..., {method:'PUT'|'POST'|'DELETE'})`）。 |
| 参考（线索，非判据） | `docs/CLI-REST覆盖核查.md`、`docs/v3-Web-CLI覆盖矩阵.md` | 可能滞后，只作交叉印证。 |

### 1.2 怎么数（可复现）

1. **枚举 CLI 叶子**：取 `ConfigPathTree()` 函数体，按 Go 源码语法解析嵌套构造调用
   （`K`=关键字、`V`/`VE`=取值叶子、`P`/`PT`=实例参数、`SP`/`SPA`/`SPD`=标量参数、`Opt`/`Su`/`IV`/`RQ`=包装器），
   重建 `*Node` 树后 DFS；**「叶子」= 没有子节点的节点**，路径 = 从根到该节点的关键字/占位符串接。
   （脚本见 §6 附录，`python` 直接跑，无需构建、无需连真机。）
2. **枚举 Web 控件**：抽取 `views/config.js` 里**全部** `path: [...]`（字段级 + `sublists[].fields[].path`）
   与 `addField(...)` 两节的字段；再把各视图里带写方法的 `api(...)` 调用（PUT/POST/DELETE）按其
   作用对象归入候选控件。
3. **映射**：按**模型字段语义**把 CLI 叶子与 Web 控件对齐（如 `set interfaces <ifname> mtu <uint>` →
   接口节 `MTU` → `interfaces[].mtu`）。无法在**任一**视图找到写入口者判「仅原始 JSON」。

### 1.3 判定三态

| 判定 | 含义 | 判据 |
|---|---|---|
| **有控件** | 界面上有字段/按钮能设这个值 | 写明是哪个节 + 哪个 label + 文件行号（`config.js` 行号；非 `config.js` 的另注文件）。 |
| **仅原始配置** | 界面只支持在「原始 JSON」文本区手写该字段 | 该叶子在 `internal/api/ui/js/**` 全量控件里找不到写入口；且「配置」页的 `#cfg-text` 文本区（`index.html:454` 标注「原始 JSON（整体替换 candidate）」，`config.js` 的 `cfgText()` 对其实施 `JSON.parse`）接受任意 candidate JSON。 |
| **无入口** | 控件与原始 JSON 都表达不了 | 需要该叶子没有对应模型字段。**本审计结果：0 条**（见 §5 局限）。 |

> 注：`#cfg-text` 是**JSON** 编辑器（不是 CLI 语句框）。故「仅原始配置」= 手工往 candidate JSON 里
> 写字段——**绕过表单的本地校验**，与「有一个控件」不是一回事。

---

## 2. 汇总

**CLI 配置语句叶子总数 = 197**（`set`/`delete` 同形，见 §1.1）。

| 口径 | 条数 |
|---|---|
| CLI 配置语句叶子（`set` / `delete` 同形） | **197** |
| ├ **有控件**（任一界面视图可写该字段） | **151** |
| ├ **仅原始配置**（无控件，可在「原始 JSON」文本区手写） | **46** |
| └ **无入口**（控件与原始 JSON 都表达不了） | **0** |

按族分布（有控件 / 仅原始 JSON）：

| 族 | 叶子数 | 有控件 | 仅原始 JSON |
|---|---:|---:|---:|
| `system` | 53 | 43 | 10 |
| `protocols`（LLDP） | 3 | 3 | 0 |
| `interfaces` | 9 | 6 | 3 |
| `bonds` | 7 | 7 | 0 |
| `virtual-switches` | 26 | 18 | 8 |
| `acls` | 7 | 7 | 0 |
| `nat` | 7 | 7 | 0 |
| `port-mirroring` | 5 | 5 | 0 |
| `qos` | 2 | 2 | 0 |
| `resource-pools` | 4 | 3 | 1 |
| `vpp` | 16 | 16 | 0 |
| `vm-image-profiles` | 9 | 0 | 9 |
| `virtual-machine-functions` | 31 | 21 | 10 |
| `container-functions` | 13 | 13 | 0 |
| `vxlan` | 5 | 0 | 5 |
| **合计** | **197** | **151** | **46** |

---

## 3. A. 无控件清单（待补）

> 判定为「仅原始 JSON」：该叶子在 `internal/api/ui/js/**` 全量控件里找不到写入口（下表「现状」）。
> 每个叶子的模型字段都存在于 candidate JSON，故可在「配置」页的「原始 JSON」文本区手工写入——这不是控件。
> **这是排期的主清单**，按族分组。

| # | CLI 语句（set/delete 同形） | 族 | 现状 |
|---|---|---|---|
| 1 | `set system dataplane <dataplane>` | system · 数据面实现 | 仅原始 JSON |
| 2 | `set system dns proxy server <ip>` | system · DNS 代理 | 仅原始 JSON |
| 3 | `set system dns proxy server secondary <ip>` | system · DNS 代理 | 仅原始 JSON |
| 4 | `set system metrics history interval <uint>` | system · 历史时序存储 | 仅原始 JSON |
| 5 | `set system metrics history retention-days <uint>` | system · 历史时序存储 | 仅原始 JSON |
| 6 | `set system firewall default-policy <policy>` | system · 主机防火墙 | 仅原始 JSON |
| 7 | `set system firewall rule <seq> action <action>` | system · 主机防火墙 | 仅原始 JSON |
| 8 | `set system firewall rule <seq> source <ip-prefix>` | system · 主机防火墙 | 仅原始 JSON |
| 9 | `set system firewall rule <seq> protocol <protocol>` | system · 主机防火墙 | 仅原始 JSON |
| 10 | `set system firewall rule <seq> port <uint>` | system · 主机防火墙 | 仅原始 JSON |
| 11 | `set interfaces <ifname> storm-control broadcast <uint>` | interfaces · 风暴抑制 | 仅原始 JSON |
| 12 | `set interfaces <ifname> storm-control multicast <uint>` | interfaces · 风暴抑制 | 仅原始 JSON |
| 13 | `set interfaces <ifname> port-security mac <mac>` | interfaces · 端口安全 | 仅原始 JSON |
| 14 | `set virtual-switches <name> dhcp-relay server <ip>` | virtual-switches · DHCP 中继 | 仅原始 JSON |
| 15 | `set virtual-switches <name> dhcp-server pool <start> <end>` | virtual-switches · DHCP 服务器 | 仅原始 JSON |
| 16 | `set virtual-switches <name> dhcp-server lease-time <uint>` | virtual-switches · DHCP 服务器 | 仅原始 JSON |
| 17 | `set virtual-switches <name> dhcp-server dns <ip>` | virtual-switches · DHCP 服务器 | 仅原始 JSON |
| 18 | `set virtual-switches <name> dhcp-server domain-name <string>` | virtual-switches · DHCP 服务器 | 仅原始 JSON |
| 19 | `set virtual-switches <name> learn-limit <uint>` | virtual-switches · 环路缓解 | 仅原始 JSON |
| 20 | `set virtual-switches <name> dns proxy server <ip>` | virtual-switches · DNS 代理 | 仅原始 JSON |
| 21 | `set virtual-switches <name> dns proxy server secondary <ip>` | virtual-switches · DNS 代理 | 仅原始 JSON |
| 22 | `set vxlan tunnels <name> vni <uint>` | vxlan | 仅原始 JSON |
| 23 | `set vxlan tunnels <name> local <ip>` | vxlan | 仅原始 JSON |
| 24 | `set vxlan tunnels <name> remote <ip>` | vxlan | 仅原始 JSON |
| 25 | `set vxlan tunnels <name> dst-port <uint>` | vxlan | 仅原始 JSON |
| 26 | `set vxlan tunnels <name> virtual-switch <name>` | vxlan | 仅原始 JSON |
| 27 | `set resource-pools cpu numa node <node> cores <core-list>` | resource-pools · NUMA | 仅原始 JSON |
| 28 | `set vm-image-profiles <image-name> machine <type>` | vm-image-profiles | 仅原始 JSON |
| 29 | `set vm-image-profiles <image-name> cpu mode <mode>` | vm-image-profiles | 仅原始 JSON |
| 30 | `set vm-image-profiles <image-name> cpu model <string>` | vm-image-profiles | 仅原始 JSON |
| 31 | `set vm-image-profiles <image-name> firmware <type>` | vm-image-profiles | 仅原始 JSON |
| 32 | `set vm-image-profiles <image-name> disk-interface <iface>` | vm-image-profiles | 仅原始 JSON |
| 33 | `set vm-image-profiles <image-name> boot-order <order>` | vm-image-profiles | 仅原始 JSON |
| 34 | `set vm-image-profiles <image-name> accel <type>` | vm-image-profiles | 仅原始 JSON |
| 35 | `set vm-image-profiles <image-name> console <type>` | vm-image-profiles | 仅原始 JSON |
| 36 | `set vm-image-profiles <image-name> extra-args <arg>` | vm-image-profiles | 仅原始 JSON |
| 37 | `set virtual-machine-functions <name> pci-device <bdf>` | virtual-machine-functions · PCI 直通 | 仅原始 JSON |
| 38 | `set virtual-machine-functions <name> startup machine <type>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 39 | `set virtual-machine-functions <name> startup cpu mode <mode>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 40 | `set virtual-machine-functions <name> startup cpu model <string>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 41 | `set virtual-machine-functions <name> startup firmware <type>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 42 | `set virtual-machine-functions <name> startup disk-interface <iface>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 43 | `set virtual-machine-functions <name> startup boot-order <order>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 44 | `set virtual-machine-functions <name> startup accel <type>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 45 | `set virtual-machine-functions <name> startup console <type>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |
| 46 | `set virtual-machine-functions <name> startup extra-args <arg>` | virtual-machine-functions · startup 覆盖 | 仅原始 JSON |

**小计：46 条（仅原始 JSON）。**

依据（这些字段只在**只读**视图里出现，无任何写控件）：

- 主机防火墙：`overview.js:94-96` 明确「本页不加写控件——写路径走「配置」页与命令行」；而 `config.js`
  里**没有** firewall 节（`grep firewall internal/api/ui/js/views/config.js` 无命中）。
- 数据面实现：`dataplane` 仅出现在 `overview.js:141`（装配事实读数）与 `ops.js:52/487`（说明文案），
  全 `js/**` 无写端点（无 `/system/dataplane` 调用）。
- DNS 代理：`overview.js:70-75` 为只读两行（`GET /dns/proxy`）。
- 历史时序存储：`system.js:157-160` 页头注明「只读页（无任何写控件）」。
- 风暴抑制 / 端口安全：`system.js:66-67` 仅在接口详情页**只读展示**（`stormControlText` / `portSecText`），
  无写控件（`grep 'storm|port_security' js/**` 除格式化函数外无命中）。
- DHCP 中继 / DHCP 服务器 / 学习上限：`switches.js:125-135`、`182-204` 均为只读读视图与租约表。
- VXLAN：`network.js:58-85` 仅 `GET /vxlan-tunnels` 只读列。
- 镜像启动画像：`images.js:87-103`（`renderImageProfile`）与 `deploy.js:62-66` 只读取用。
- VM `startup` 覆盖 / PCI 直通：`vmDetail.js:77-82`、`147` 只读展示。
- 资源池 NUMA：`config.js` 资源池节只有「隔离核 + 1G/2M 大页数量」（`L203/207`），无 NUMA 节点字段。

---

## 4. B. 有控件清单

> `set`/`delete` 同形。行号：除特别注明外均为 `internal/api/ui/js/views/config.js`；
> 标「system.js」者在该文件。一个控件可覆盖多个叶子（如 DNS 服务器 iplist 同时覆盖主/备）。

| CLI 语句（set/delete 同形） | 对应节 | 控件 label（行号） |
|---|---|---|
| **system（系统）** | | |
| `set system hostname <string>` | 系统（基础） | 主机名 L962 |
| `set system timezone <string>` | 系统（基础） | 时区 L963 |
| `set system ntp server <ip\|host>` | 系统（基础）· NTP 子表 | 服务器（新增行）L968 |
| `set system ntp server prefer` | 系统（基础）· NTP 子表 | 首选 L971 |
| `set system dns server <ip>` | 系统（基础） | DNS 服务器 L964 |
| `set system dns server secondary <ip>` | 系统（基础） | DNS 服务器（同一 iplist）L964 |
| `set system api port <uint>` | 系统 · API 与 TLS | HTTPS 端口 L981 |
| `set system api token-ttl-minutes <uint>` | 系统 · API 与 TLS | 令牌有效期（分钟）L982 |
| `set system api max-sessions <uint>` | 系统 · API 与 TLS | 并发会话上限 L983 |
| `set system api tls cert-file <path>` | 系统 · API 与 TLS | 证书文件（PEM 路径）L984 |
| `set system api tls key-file <path>` | 系统 · API 与 TLS | 私钥文件（PEM 路径）L985 |
| `set system api tls self-signed regenerate` | 系统 · 证书页（system.js） | 重签自签证书 L1050 |
| `set system management interface <ifname>` | 系统 · 管理口 | 管理网卡 L997 |
| `set system management ip address <ip-prefix>` | 系统 · 管理口 | 管理口地址（CIDR）L998 |
| `set system management gateway <ip>` | 系统 · 管理口 | 管理口网关 L999 |
| `set system kernel nmi-watchdog <bool>` | 系统 · 内核启动参数 | NMI watchdog L1008 |
| `set system kernel transparent-hugepages <thp>` | 系统 · 内核启动参数 | 透明大页 L1009 |
| `set system kernel iommu <iommu>` | 系统 · 内核启动参数 | IOMMU L1011 |
| `set system kernel low-latency <bool>` | 系统 · 内核启动参数 | 低延迟参数组 L1013 |
| `set system kernel tuned-profile <name>` | 系统 · 内核启动参数 | tuned 性能档 L1014 |
| `set system kernel params <param>` | 系统 · 内核启动参数 | 附加内核参数（每行一条）L1015 |
| `set system health thresholds cpu-temp-celsius <uint>` | 系统 · 健康阈值 | CPU 温度 L1041 |
| `set system health thresholds disk-temp-celsius <uint>` | 系统 · 健康阈值 | 磁盘温度 L1042 |
| `set system health thresholds disk-used-percent <uint>` | 系统 · 健康阈值 | 磁盘使用率 L1043 |
| `set system syslog host <ip>` | 系统 · 日志（syslog） | 远程主机 L1024 |
| `set system syslog host port <uint>` | 系统 · 日志（syslog） | 远程端口 L1025 |
| `set system syslog host facility <facility>` | 系统 · 日志（syslog） | facility L1026 |
| `set system syslog host severity <severity>` | 系统 · 日志（syslog） | 远程转发级别 L1027 |
| `set system syslog local level <level>` | 系统 · 日志（syslog） | 本地日志级别 L1029 |
| `set system syslog local retention-days <uint>` | 系统 · 日志（syslog） | 本地保留天数 L1031 |
| `set system syslog local max-size-mb <uint>` | 系统 · 日志（syslog） | 本地容量上限 L1032 |
| `set system login banner <string>` | 系统 · 用户页（system.js） | 登录横幅 L902 |
| `set system login user <name>` | 系统 · 登录与口令策略 · 本地用户 | 用户名（新增行）L1064 |
| `set system login user password <string>` | 系统 · 登录与口令策略 · 本地用户 | 设置口令 L1070 |
| `set system login user class <class-name>` | 系统 · 登录与口令策略 · 本地用户 | 归属 class L1068 |
| `set system login class <name> allow <command-path>` | 系统 · 登录与口令策略 · 自定义 class | 允许的节点 L1080 |
| `set system login class <name> deny <command-path>` | 系统 · 登录与口令策略 · 自定义 class | 拒绝的节点 L1081 |
| `set system login password-policy min-length <uint>` | 系统 · 登录与口令策略 | 口令最小长度 L1056 |
| `set system login password-policy complexity <bool>` | 系统 · 登录与口令策略 | 口令复杂度 L1057 |
| `set system login password-policy expire-days <uint>` | 系统 · 登录与口令策略 | 口令有效期 L1058 |
| `set system login password-policy lockout-threshold <uint>` | 系统 · 登录与口令策略 | 连续失败锁定阈值 L1059 |
| `set system login password-policy lockout-minutes <uint>` | 系统 · 登录与口令策略 | 锁定时长 L1060 |
| `set system idle-timeout-minutes <uint>` | 系统（基础） | 会话空闲超时 L965 |
| **protocols（LLDP）** | | |
| `set protocols lldp enable <bool>` | LLDP | 全局启用 L1323 |
| `set protocols lldp advertisement-interval <uint>` | LLDP | 通告间隔（秒）L1324 |
| `set protocols lldp interface <ifname> enable <bool>` | LLDP · 按接口覆盖 | 启用 L1329 |
| **interfaces（物理网卡）** | | |
| `set interfaces <ifname> description <string>` | 接口 | 描述 L186 |
| `set interfaces <ifname> disable` | 接口 | 启用（置 false）L188 |
| `set interfaces <ifname> mtu <uint>` | 接口 | MTU L187 |
| `set interfaces <ifname> sriov vf-count <uint>` | 接口详情页（system.js） | SR-IOV VF 数量 L122 |
| `set interfaces <ifname> ingress-policy <name>` | 接口 | 入向限速策略 L190 |
| `set interfaces <ifname> egress-policy <name>` | 接口 | 出向限速策略 L192 |
| **bonds（链路聚合）** | | |
| `set bonds <name> members <seq>` | 链路聚合（bond） | 成员口 L1310 |
| `set bonds <name> members <ifname>` | 链路聚合（bond） | 成员口 L1310 |
| `set bonds <name> lacp mode <mode>` | 链路聚合（bond） | LACP 模式 L1311 |
| `set bonds <name> lacp mode interval <interval>` | 链路聚合（bond） | LACP 速率 L1312 |
| `set bonds <name> lacp disable` | 链路聚合（bond） | 清空 LACP 模式/速率 L1311/1312 |
| `set bonds <name> mtu <uint>` | 链路聚合（bond） | MTU L1313 |
| `set bonds <name> description <string>` | 链路聚合（bond） | 描述 L1314 |
| **virtual-switches（虚拟交换机 / VRF / 路由）** | | |
| `set virtual-switches <name> type <type>` | 虚拟交换机 | 类型 L1153 |
| `set virtual-switches <name> vlan access <vlan>` | 虚拟交换机 | access VLAN L1155 |
| `set virtual-switches <name> gateway ip <ip-prefix>` | 虚拟交换机 | 网关地址 L1159 |
| `set virtual-switches <name> gateway vrf <name>` | 虚拟交换机 | 网关所属 VRF L1160 |
| `set virtual-switches <name> gateway acl-in <acl>` | 虚拟交换机 | 网关入向 ACL L1161 |
| `set virtual-switches <name> gateway acl-out <acl>` | 虚拟交换机 | 网关出向 ACL L1162 |
| `set virtual-switches <name> ports <seq> interface <ifname>` | 虚拟交换机 · 成员端口 | 物理口 / bond L1169 |
| `set virtual-switches <name> ports <seq> interface trunk vlans <vlan-list>` | 虚拟交换机 · 成员端口 | trunk VLAN 列表 L1176 |
| `set virtual-switches <name> ports <seq> interface native <vlan>` | 虚拟交换机 · 成员端口 | native VLAN L1177 |
| `set virtual-switches <name> ports <seq> vnf <vm-name> interface <vnic-name>` | 虚拟交换机 · 成员端口 | VNF（VM 名）+ VNF 的 vNIC L1170/1172 |
| `set virtual-switches <name> ports <seq> vnf <vm-name> interface trunk vlans <vlan-list>` | 虚拟交换机 · 成员端口 | trunk VLAN 列表 L1176 |
| `set virtual-switches <name> ports <seq> container <ct-name> interface <vnic-name>` | 虚拟交换机 · 成员端口 | 容器名 + 容器的 vNIC L1173/1175 |
| `set virtual-switches <name> cross-connect <bool>` | 虚拟交换机 | 端口直通 L1156 |
| `set virtual-switches <name> l3-interface <ifname\|vlan> ip address <ip-prefix>` | VRF / 路由 · 三层接口 | 地址（CIDR）L1201 |
| `set virtual-switches <name> l3-interface <ifname\|vlan> acl-in <acl>` | VRF / 路由 · 三层接口 | 入向 ACL L1202 |
| `set virtual-switches <name> static-routes <prefix> next-hop <ip>` | VRF / 路由 · 静态路由 | 下一跳 L1209 |
| `set virtual-switches <name> static-routes <prefix> distance <uint>` | VRF / 路由 · 静态路由 | 管理距离 L1210 |
| `set virtual-switches <name> static-routes default next-hop <ip>` | VRF / 路由 · 静态路由 | 下一跳（prefix=0.0.0.0/0）L1209 |
| **acls（ACL）** | | |
| `set acls <name> rule <seq> source <ip-prefix\|any>` | ACL · 规则 | 源 L1230 |
| `set acls <name> rule <seq> destination <ip-prefix\|any>` | ACL · 规则 | 目的 L1231 |
| `set acls <name> rule <seq> protocol <protocol>` | ACL · 规则 | 协议 L1232 |
| `set acls <name> rule <seq> source-port <port\|range>` | ACL · 规则 | 源端口 L1233 |
| `set acls <name> rule <seq> destination-port <port\|range>` | ACL · 规则 | 目的端口 L1234 |
| `set acls <name> rule <seq> action <action>` | ACL · 规则 | 动作 L1235 |
| `set acls <name> rule <seq> direction <direction>` | ACL · 规则 | 方向 L1229 |
| **nat（NAT44）** | | |
| `set nat source-pool <name> address-range <ip>` | NAT · 源地址池 | 地址范围 L1249 |
| `set nat source-pool <name> address-range to <ip>` | NAT · 源地址池 | 地址范围 L1249 |
| `set nat rules <seq> match source <ip-prefix>` | NAT · 转换规则 | 匹配源 L1255 |
| `set nat rules <seq> match source virtual-switch <name>` | NAT · 转换规则 | L3 交换机 L1256 |
| `set nat rules <seq> action source-pool <name>` | NAT · 转换规则 | 源地址池 L1258 |
| `set nat rules <seq> action interface <ifname>` | NAT · 转换规则 | 出接口 L1257 |
| `set nat static <inside-ip> to <outside-ip>` | NAT · 静态 1:1 映射 | 外部 IP L1264 |
| **port-mirroring（端口镜像）** | | |
| `set port-mirroring <name> source interface <ifname>` | 端口镜像（SPAN） | 源：物理口 / bond L1292 |
| `set port-mirroring <name> source interface direction <direction>` | 端口镜像（SPAN） | 方向 L1297 |
| `set port-mirroring <name> source vnf <vm> interface <vnic>` | 端口镜像（SPAN） | 源：VNF + 源：VNF 的 vNIC L1294/1296 |
| `set port-mirroring <name> source vnf <vm> interface direction <direction>` | 端口镜像（SPAN） | 方向 L1297 |
| `set port-mirroring <name> analyzer interface <ifname>` | 端口镜像（SPAN） | 分析端口 L1298 |
| **qos（限速）** | | |
| `set qos policies <name> cir <uint>` | QoS（限速） | CIR（bps）L1278 |
| `set qos policies <name> cbs <uint>` | QoS（限速） | CBS（字节）L1279 |
| **resource-pools（资源池）** | | |
| `set resource-pools hugepages page-size <size>` | 资源池 | 大页 1G / 2M 数量（行存在）L207 |
| `set resource-pools hugepages page-size count <uint>` | 资源池 | 大页 1G / 2M 数量 L207 |
| `set resource-pools cpu isolated-cores <core-list>` | 资源池 | 隔离核 L203 |
| **vpp（数据面）** | | |
| `set vpp cpu main-core <uint>` | VPP（数据面） | CPU 主核 L1098 |
| `set vpp cpu corelist-workers <core-list>` | VPP（数据面） | worker 核列表 L1099 |
| `set vpp cpu workers-per-numa <uint>` | VPP（数据面） | 每 NUMA worker 数 L1101 |
| `set vpp memory main-heap-size <size>` | VPP（数据面） | 内存主堆大小 L1103 |
| `set vpp memory buffers-per-numa <uint>` | VPP（数据面） | 每 NUMA buffer 数 L1104 |
| `set vpp memory hugepage-preference <size>` | VPP（数据面） | 大页偏好 L1105 |
| `set vpp dpdk dev rx-queues <uint>` | VPP（数据面） | DPDK 全局收队列 L1107 |
| `set vpp dpdk dev tx-queues <uint>` | VPP（数据面） | DPDK 全局发队列 L1108 |
| `set vpp dpdk dev rx-descriptors <uint>` | VPP（数据面） | DPDK 全局收描述符 L1109 |
| `set vpp dpdk dev tx-descriptors <uint>` | VPP（数据面） | DPDK 全局发描述符 L1110 |
| `set vpp dpdk dev <ifname> rx-queues <uint>` | VPP（数据面）· 单网卡覆盖 | 收队列 L1122 |
| `set vpp dpdk dev <ifname> tx-queues <uint>` | VPP（数据面）· 单网卡覆盖 | 发队列 L1123 |
| `set vpp dpdk dev <ifname> rx-descriptors <uint>` | VPP（数据面）· 单网卡覆盖 | 收描述符 L1124 |
| `set vpp dpdk dev <ifname> tx-descriptors <uint>` | VPP（数据面）· 单网卡覆盖 | 发描述符 L1125 |
| `set vpp dpdk uio-driver <driver>` | VPP（数据面） | UIO 驱动 L1111 |
| `set vpp plugins <name> state <state>` | VPP（数据面）· 插件开关 | 状态 L1132 |
| **virtual-machine-functions（VM VNF）** | | |
| `set virtual-machine-functions <name> image <image-name>` | 虚拟机（VM VNF） | 镜像 L1343 |
| `set virtual-machine-functions <name> vcpu count <uint>` | 虚拟机（VM VNF） | vCPU 数量 L1345 |
| `set virtual-machine-functions <name> vcpu pin <bool>` | 虚拟机（VM VNF） | vCPU 绑核 L1346 |
| `set virtual-machine-functions <name> memory size-mb <uint>` | 虚拟机（VM VNF） | 内存（MB）L1347 |
| `set virtual-machine-functions <name> memory hugepage-size <size>` | 虚拟机（VM VNF） | 大页大小 L1348 |
| `set virtual-machine-functions <name> memory numa node <uint>` | 虚拟机（VM VNF） | NUMA 节点 L1349 |
| `set virtual-machine-functions <name> memory backing <type>` | 虚拟机（VM VNF） | 内存后端 L1350 |
| `set virtual-machine-functions <name> disks <disk-name> size-gb <uint>` | 虚拟机（VM VNF）· 附加数据盘 | 容量（GB，空盘）L1363 |
| `set virtual-machine-functions <name> disks <disk-name> image <image-name>` | 虚拟机（VM VNF）· 附加数据盘 | 来源镜像 L1364 |
| `set virtual-machine-functions <name> interfaces <vnic-name> type <type>` | 虚拟机（VM VNF）· vNIC | 类型 L1372 |
| `set virtual-machine-functions <name> interfaces <vnic-name> sriov physical-interface <ifname>` | 虚拟机（VM VNF）· vNIC | SR-IOV 物理口 L1376 |
| `set virtual-machine-functions <name> interfaces <vnic-name> sriov physical-interface vf <uint>` | 虚拟机（VM VNF）· vNIC | SR-IOV VF 号 L1377 |
| `set virtual-machine-functions <name> interfaces <vnic-name> mac <mac>` | 虚拟机（VM VNF）· vNIC | MAC L1374 |
| `set virtual-machine-functions <name> interfaces <vnic-name> vlan <vlan>` | 虚拟机（VM VNF）· vNIC | VLAN L1375 |
| `set virtual-machine-functions <name> interfaces <vnic-name> virtual-switch <name>` | 虚拟机（VM VNF）· vNIC | 虚拟交换机 L1373 |
| `set virtual-machine-functions <name> cloud-init user-data <string>` | 虚拟机（VM VNF） | user-data（YAML 文本）L1355 |
| `set virtual-machine-functions <name> cloud-init ssh-key <key>` | 虚拟机（VM VNF） | SSH 公钥 L1354 |
| `set virtual-machine-functions <name> cloud-init hostname <string>` | 虚拟机（VM VNF） | VM guest 主机名 L1353 |
| `set virtual-machine-functions <name> serial console enable` | 虚拟机（VM VNF） | 串口控制台 L1351 |
| `set virtual-machine-functions <name> autostart <bool>` | 虚拟机（VM VNF） | 开机自启 L1352 |
| `set virtual-machine-functions <name> description <string>` | 虚拟机（VM VNF） | 描述 L1344 |
| **container-functions（容器 VNF）** | | |
| `set container-functions <name> image <image-name>` | 容器（container VNF） | 镜像 L1392 |
| `set container-functions <name> vcpu count <uint>` | 容器（container VNF） | vCPU 限制 L1394 |
| `set container-functions <name> memory size-mb <uint>` | 容器（container VNF） | 内存限制（MB）L1395 |
| `set container-functions <name> interfaces <vnic-name> type <type>` | 容器（container VNF）· memif vNIC | 类型 L1406 |
| `set container-functions <name> interfaces <vnic-name> virtual-switch <name>` | 容器（container VNF）· memif vNIC | 虚拟交换机 L1407 |
| `set container-functions <name> interfaces <vnic-name> mac <mac>` | 容器（container VNF）· memif vNIC | MAC L1408 |
| `set container-functions <name> interfaces <vnic-name> vlan <vlan>` | 容器（container VNF）· memif vNIC | VLAN L1409 |
| `set container-functions <name> env <key> <string>` | 容器（container VNF） | 环境变量 L1400 |
| `set container-functions <name> command <string>` | 容器（container VNF） | 入口命令 L1396 |
| `set container-functions <name> args <string>` | 容器（container VNF） | 命令参数 L1397 |
| `set container-functions <name> restart-policy <policy>` | 容器（container VNF） | 重启策略 L1398 |
| `set container-functions <name> autostart <bool>` | 容器（container VNF） | 开机自启 L1399 |
| `set container-functions <name> description <string>` | 容器（container VNF） | 描述 L1393 |

**小计：151 条（有控件）。**

---

## 5. 局限

1. **映射是「语义对齐」，不是机械字符串匹配**。CLI 关键字与模型 JSON 键不一一对应（例如
   `port-security mac` → `port_security` 数组；`dhcp-server pool` → 嵌套对象），对齐靠模型语义判断。
   个别条目可能有误判，见下。
2. **「有控件」一侧偏宽松**（同一个界面控件可能被计成多条叶子的控件）：
   - 一个控件覆盖多个叶子：`系统（基础）` 的 DNS 服务器 iplist 同时覆盖 `dns server` 与
     `dns server secondary`；成员端口的 trunk VLAN 覆盖「物理口 / VNF」两种形态；资源池的大页数量行
     同时覆盖 `page-size <size>`（身份）与 `page-size count`。
   - **控件不在「配置」页**：`set interfaces <ifname> sriov vf-count`（接口详情页 `system.js`）、
     `set system api tls self-signed regenerate`（证书页 `system.js`）、
     `set system login banner`（用户页 `system.js`）的写入口在**系统/运维页**，不是配置表单。
     若口径收窄为「配置页表单字段」，这三条应改判「仅原始 JSON」。
   - **动作型叶子的间接映射**：`interfaces <ifname> disable`（→ 接口「启用」select 置 false）、
     `bonds <name> lacp disable`（→ 清空 LACP 模式/速率 = 静态聚合）没有「同名」控件，是**等价控件**。
3. **「仅原始 JSON」一侧偏保守**：只有在该字段于 `internal/api/ui/js/**` **任何视图都找不到写入口**
   时才判缺口。因此 **46 条是缺口的下界**——不会漏报，但也不会把「控件在别的页、只是不好找」算进来。
4. **树叶子 ≠ 独立语句**。197 条里有若干是命令树建模产生的**非独立叶子**，已按同一控件归类：
   - `set system ntp server prefer` 与 `set system ntp server <ip|host>` 是同一语句的两个叶子
     （`prefer` 是 server 的同级可选关键字，裸写无意义）；
   - `set bonds <name> members <seq>` 与 `… members <ifname>` 是 `members [<seq>] <ifname>` 的
     两个叶子（`<seq>` 是可选成员序号）；
   - `set system login user <name>` 是 `RequireSub` 参数（不能单独成句），对应「本地用户」子表的**新增行标识**；
   - `set resource-pools hugepages page-size <size>` 是身份取值叶子（1G/2M），对应大页数量行。
5. **「无入口」= 0** 依赖「原始 JSON 文本区接受任意 candidate 字段」这一前提。凡模型里有字段者都能
   手写；本轮未发现「有 CLI 语句但模型里没有对应字段」的叶子。若将来出现纯动作型叶子（无模型落点），
   需单列「无入口」。
6. **`delete` 的粒度差异**：`delete` 可作用于**非叶子子树**（如 `delete virtual-switches <name>` 整段删），
   本审计只统计**叶子**语句（与 `set` 同形）。子树级删除由各节的「删除」按钮 / `cfgRemove` 覆盖
   （`config.js:739`、`cfgClearSection` `config.js:761`），未逐条入表。

### 下一步复核建议

- 对 46 条「仅原始 JSON」，逐条 `grep` 其模型 JSON 键（如 `storm_control`、`port_security`、`firewall`、
  `dhcp_server`、`vxlan`、`startup`、`vm_image_profiles`）于 `internal/api/ui/js/**`，确认确无写控件——
  本审计已按此做过一轮（§3 依据），复核时可再跑一次。
- 明确口径：**「有控件」是否只认配置页表单**。若只认配置页，则 §3 清单应 +3 条
  （sriov vf-count、tls regenerate、login banner），总数变为「有控件 148 / 仅原始 JSON 49」。
- 建议把「无控件清单」按**功能族**（而非命令树族）归并排期：整族缺失的（主机防火墙、VXLAN、
  DHCP server/relay、DNS 代理、镜像启动画像、VM startup 覆盖）优先于零散单条。

---

## 6. 附录：可复现的枚举脚本

把下面脚本存为任意 `.py`（如 `/tmp/enum_tree.py`）后 `python /tmp/enum_tree.py` 即可打印
**197 条叶子**（无需构建、无需连真机、不改任何仓内文件）。它解析 `tree_config.go` 的
`ConfigPathTree()` 函数体，按 `K`/`V`/`VE`/`P`/`PT`/`SP`/`SPA`/`SPD` 构造 + `Opt`/`Su`/`IV`/`RQ`
包装重建树，DFS 输出所有**无子节点**的叶子路径。

```python
import re
from collections import defaultdict
BS = chr(92)
src = open("internal/schema/tree_config.go", encoding="utf-8").read()
i = src.index("func ConfigPathTree() *Node {"); j = src.index("{", i)
d = 0; k = j
while True:
    c = src[k]
    if c == '{': d += 1
    elif c == '}':
        d -= 1
        if d == 0: break
    k += 1
body = src[j+1:k]
toks = []; p = 0; n = len(body)
while p < n:
    c = body[p]
    if c == '"':
        q = p+1; b = ''
        while body[q] != '"':
            if body[q] == BS: b += body[q:q+2]; q += 2; continue
            b += body[q]; q += 1
        toks.append(('s', b)); p = q+1; continue
    if c == '/' and p+1 < n and body[p+1] == '/':
        while p < n and body[p] != chr(10): p += 1
        continue
    if c == '/' and p+1 < n and body[p+1] == '*':
        p += 2
        while p+1 < n and not (body[p] == '*' and body[p+1] == '/'): p += 1
        p += 2; continue
    if c in '(),': toks.append((c, c)); p += 1; continue
    if c.isalpha() or c == '_':
        q = p
        while q < n and (body[q].isalnum() or body[q] == '_'): q += 1
        toks.append(('i', body[p:q])); p = q; continue
    p += 1
pos = 0
def pk(): return toks[pos] if pos < len(toks) else (None, None)
def eat(t=None):
    global pos
    tk = toks[pos]; pos += 1
    if t and tk[0] != t: raise SystemExit("want %s got %s" % (t, tk))
    return tk
def pexpr():
    _, nm = eat('i'); eat('('); args = []
    if pk()[0] != ')':
        while True:
            tk = pk()
            if tk[0] == 's': eat('s'); args.append(('s', tk[1]))
            elif tk[0] == 'i':
                nx = toks[pos+1] if pos+1 < len(toks) else (None, None)
                if nx[0] == '(': args.append(('e', pexpr()))
                else: eat('i'); args.append(('i', tk[1]))
            if pk()[0] == ',':
                eat(',')
                if pk()[0] == ')': break
                continue
            break
    eat(')'); return (nm, args)
WRAP = {'Opt', 'Su', 'Op', 'IV', 'RQ'}
def bld(nm, args):
    if nm in WRAP:
        for a in args:
            if a[0] == 'e': return bld(a[1][0], a[1][1])
    ch = []
    if nm == 'K':
        for a in args[2:]:
            if a[0] == 'e': ch.append(bld(a[1][0], a[1][1]))
        return {'n': args[0][1], 'c': ch}
    if nm in ('V', 'VE'): return {'n': '<'+args[0][1]+'>', 'c': []}
    if nm in ('P', 'PT'):
        for a in args[3:]:
            if a[0] == 'e': ch.append(bld(a[1][0], a[1][1]))
        return {'n': args[0][1], 'c': ch}
    if nm in ('SP', 'SPA', 'SPD'): return {'n': args[0][1], 'c': []}
    raise SystemExit("ctor " + nm)
root = None
while pos < len(toks):
    if pk() == ('i', 'K'): root = bld(*pexpr()); break
    pos += 1
leaves = []
def dfs(nd, path):
    p = path + [nd['n']]
    if not nd['c']: leaves.append(' '.join(p)); return
    for c in nd['c']: dfs(c, p)
for c in root['c']: dfs(c, [])
print("TOTAL=%d" % len(leaves))
for l in leaves: print("set " + l)
```

---

*本文件为只读审计产物；除本文件外未改动仓库任何文件。*
