# NFViS 系统产品需求与目标架构需求规格书

| 文档属性 | 内容 |
|---|---|
| 版本 | V1.0（草案） |
| 日期 | 2026-09-12 |
| 状态 | 经需求问答收敛后产出 |
| 目标平台 | Ubuntu 26.04 LTS 底座 / VPP 26.06 网络底座 / Libvirt + QEMU + KVM |
| 开发语言 | Go |

---

## 1. 概述

### 1.1 产品定位

NFViS（NFVi System）是一套部署于单台 x86 服务器上的网络功能虚拟化基础设施（NFVi）一体机软件。它将 Ubuntu + VPP + KVM 封装为可运维产品，提供：

- **JunOS 风格 CLI**（`nfvis-cli`）作为主管理入口，运维人员以熟悉的多层级命令方式完成全部管理操作；
- **REST API**（OpenAPI 规范）作为程序化管理入口，为后续 Web 控制面提供完整数据模型；
- 基于 **VPP** 的用户态虚拟交换网络（虚拟交换机、L2/L3 转发、ACL、NAT 等）；
- 基于 **KVM + Docker** 的 VNF（虚拟机、容器）生命周期与镜像管理。

### 1.2 V1 范围摘要

| 维度 | 决策 |
|---|---|
| 部署形态 | 单节点一体机；架构上（API/模型/状态存储独立于数据面）预留多节点扩展 |
| 规模定位 | 实验室/POC 级：≤10 个 VM VNF、≤20 个容器 VNF、≤4×10GbE 业务网卡 |
| 虚拟交换 | L2 bridge domain + L3 VRF/静态路由；L2 交换机可选 BVI 三层网关；ACL / NAT / 端口镜像 / QoS 限速进 V1 |
| 链路与协议 | LACP 链路聚合（bond）、LLDP 邻居发现进 V1 |
| IP 版本 | 管理面与数据面 L3 支持 IPv4 + IPv6（静态路由）；动态路由协议列为 V2 |
| VM 网络接入 | vhost-user（主力）、SR-IOV VF 直通（高性能场景）；tap 接口列为 V2 |
| 容器网络接入 | memif（配合 Docker 运行时） |
| 动态路由 | V1 不做，仅静态路由；架构预留 Linux Control Plane + FRR 扩展路径 |
| CLI 配置模型 | 完整 JunOS 事务模型（candidate / commit / commit confirmed / rollback / compare） |
| 软件架构 | 单守护进程 `nfvisd` + 薄客户端 `nfvis-cli` |
| 状态存储 | SQLite（配置/事务/回滚历史）；运行态实时从 VPP/libvirt/Docker 查询 |
| API | REST + OpenAPI 3.0 + Bearer Token |
| 升级 | deb 包级升级（`request system software`） |
| AAA | 本地用户 + JunOS 风格 login class；外部 AAA 接口预留 |
| 观测 | Prometheus metrics、远程 syslog、事件告警、硬件健康监控（IPMI/SMART）；SNMP 列为 V2 |

### 1.3 术语

| 术语 | 定义 |
|---|---|
| VNF | 虚拟网络功能，本系统中为虚拟机或容器形态的租户工作负载 |
| 虚拟交换机 | 由 VPP bridge domain（L2）或 VRF（L3）构成的逻辑交换实体 |
| candidate 配置 | 已编辑、尚未 commit 的候选配置 |
| committed 配置 | 当前生效并持久化的配置 |
| 资源池 | 系统级预分配的绑核（CPU）、大页内存资源集合 |
| memif | VPP 共享内存接口，用于容器接入 VPP 转发面 |

---

## 2. 总体架构

### 2.1 进程与组件模型

```
                        管理面（独立管理网卡，不接 VPP）
   ┌──────────┐   ┌───────────────────────────────────────────────┐
   │ 运维终端  │SSH│  sshd ──► nfvis-cli（薄客户端，也支持独立运行）  │
   │/HTTP客户 │──►│                    │ REST / 本地 socket        │
   └──────────┘   │                    ▼                           │
                  │              ┌──────────────┐                   │
                  │              │   nfvisd     │  ← 单一 Go 守护进程 │
                  │              │ ┌──────────┐ │                   │
                  │              │ │ 配置事务引擎│ │ candidate/commit/ │
                  │              │ │ (SQLite) │ │ rollback/审计     │
                  │              │ └──────────┘ │                   │
                  │              │ ┌──────────┐ │                   │
                  │              │ │ REST API │ │ /api/v1/...       │
                  │              │ └──────────┘ │                   │
                  │              │ ┌────────────────────────────┐  │
                  │              │ │ 网络编排器（govpp 客户端）    │  │
                  │              │ │ 计算编排器（libvirt-go）     │  │
                  │              │ │ 容器编排器（Docker client）  │  │
                  │              │ │ 镜像管理 / 资源池管理         │  │
                  │              │ │ 恢复收敛器 / 事件告警 / 监控  │  │
                  │              │ └────────────────────────────┘  │
                  └──────────────┴────────────┬────────────────────┘
                                              │
        ┌─────────────────────────────────────┼──────────────────────────┐
        ▼                        ▼             ▼                          ▼
   ┌─────────┐           ┌────────────┐  ┌──────────┐             ┌─────────────┐
   │  VPP    │ binary API│   KVM/QEMU │  │  Docker  │             │ Prometheus/ │
   │ 26.06   │◄──────────┼────────────┼──┴──────────┴─────────────┼─ 远程syslog │
   └────┬────┘  govpp   └─────┬──────┘   docker.sock               └─────────────┘
        │                     │ vhost-user / SR-IOV VF
        ▼                     ▼
   业务网卡(DPDK)         VM VNF / 容器 VNF（memif）
```

### 2.2 组件职责

| 组件 | 技术选型 | 职责 |
|---|---|---|
| `nfvisd` | Go 单守护进程（systemd 托管） | 承载全部业务逻辑：配置事务、编排、API、恢复、监控 |
| `nfvis-cli` | Go 薄客户端 | CLI 交互（行编辑、`?`/Tab 补全、层级命令），通过本地 socket/REST 与 `nfvisd` 通信；sshd 将用户 shell 设为 `nfvis-cli` |
| 配置事务引擎 | Go + SQLite | candidate/committed/rollback 历史的存储与事务、配置 diff、schema 校验 |
| 网络编排器 | govpp（VPP binary API） | VPP 接口、bridge domain、VRF、ACL、NAT、SPAN、QoS 的下发与状态查询 |
| 计算编排器 | libvirt-go | VM 域定义/生命周期、vhost-user/vhostuser-SR-IOV 接口挂载、串口、快照 |
| 容器编排器 | Docker 客户端（docker.sock） | 容器生命周期、镜像 pull、memif 接入参数注入 |
| 恢复收敛器 | nfvisd 内模块 | 启动/运行时对比 committed 配置与实际运行态并收敛 |

### 2.3 关键架构原则

1. **单一数据模型**：CLI、REST API、未来 Web 控制面共享同一套配置/状态模型与同一事务引擎，禁止出现两套逻辑。
2. **配置与运行态分离**：SQLite 只存"期望配置"（intended state）；运行态（接口计数、VM 电源状态等）实时从 VPP/libvirt/Docker 读取，不落库。
3. **声明式收敛**：所有配置变更表达为期望状态，由编排器转换为各底座操作；守护进程重启后按 committed 配置重放收敛（见 8.3）。
4. **版本锁定**：`nfvisd` 启动时校验 Ubuntu 版本、VPP 版本（26.06）、libvirt/qemu 版本，不满足则拒绝启动并在 CLI/API 给出明确错误。
5. **API 先行**：每个 CLI 命令必须映射到明确的 API 端点；命令树与 API 资源模型在规格层面一一对应（附录 B）。

---

## 3. CLI 需求

### 3.1 接入与模式

- 通过 SSH 登录（管理网卡 IP）或本地控制台进入，shell 即 `nfvis-cli`；也可在系统 shell 中执行 `nfvis-cli` 进入 CLI。
- 提示符：操作模式 `nfvis>`，配置模式 `nfvis#`（顶层），层级进入后显示路径（如 `nfvis# edit interfaces` 后为 `[edit interfaces] nfvis#`）。

| 模式 | 说明 | 对应 JunOS |
|---|---|---|
| 操作模式（Operational） | 查看、运维动作 | `show`、`request`、`ping` 等顶级命令 |
| 配置模式（Configuration） | 编辑 candidate 配置 | `configure` 进入，`edit/up/top` 层级导航，`set/delete/show/commit` |

### 3.2 交互行为（必备）

| 需求 ID | 需求 |
|---|---|
| FR-CLI-001 | 操作模式下输入 `?` 显示当前上下文所有可用命令及简要描述 |
| FR-CLI-002 | 命令输入过程中任意位置输入 `?`，显示该位置所有可能的补全项（含参数值提示，如接口名、VNF 名列表） |
| FR-CLI-003 | Tab 补全：命令关键字与枚举型参数值均可 Tab 补全；唯一匹配自动补全，多匹配列出候选 |
| FR-CLI-004 | 支持命令缩写（无歧义前缀即可执行），JunOS 风格 |
| FR-CLI-005 | 支持管道过滤：`| match`、`| except`、`| count`、`| last <n>`、`| display xml/json` |
| FR-CLI-006 | 命令历史（上下键）、Ctrl-R 反查、会话空闲超时（默认 10 分钟，可配）自动登出 |
| FR-CLI-007 | 配置模式下 `?`/Tab 依据配置 schema 提供 set/delete 语句补全 |

### 3.3 配置事务模型（核心语义）

完整还原 JunOS 事务语义：

| 需求 ID | 需求 |
|---|---|
| FR-CFG-001 | `configure` 进入配置模式后，所有 `set/delete` 作用于 candidate 配置副本，不影响运行 |
| FR-CFG-002 | `commit` 时做 schema 校验 + 语义校验（资源存在性、资源池配额、地址冲突等），全部通过才下发底座并落库；失败则列出所有错误并保留 candidate |
| FR-CFG-003 | `commit confirmed <minutes>`（默认 10 分钟）：超时未确认自动回滚到变更前配置并告警 |
| FR-CFG-004 | `commit` 后进入 confirmed 等待期，`commit`（或 `commit check`）确认；超时回滚记录入审计日志 |
| FR-CFG-005 | `rollback <n>` 回退到第 n 个历史 committed 配置（保留最近 50 个），`rollback` 后需 `commit` 生效 |
| FR-CFG-006 | `show configuration`（操作模式）与 `show`（配置模式）显示配置；`show configuration | compare rollback <n>` 输出 JunOS 风格 diff（`[edit ...]` + `+`/`-` 行） |
| FR-CFG-007 | `delete` 支持层级子树删除；`annotate` 支持配置节点注释 |
| FR-CFG-008 | `load override/merge <file>` 与 `save <file>`：配置以 JSON 格式存取（内部存储为 SQLite 记录，导入导出为 JSON） |
| FR-CFG-009 | 并发写保护：同一时刻仅一个会话可持有未提交的 candidate 变更；其他会话 `configure` 时只读，`show system configuration sessions` 可查看持有者 |
| FR-CFG-010 | 配置变更全程审计：user、time、变更 diff、commit 结果 |
| FR-CFG-011 | **commit 语义校验规则集**（校验失败须逐条列出）：① vhost-user vNIC 的 VM 必须使用大页内存（backing=hugepage）；② IP 地址不得与既有 L3 接口/网关/管理口冲突或网段重叠；③ VNF MAC 不得重复；④ 交换机端口 VLAN 配置一致（access VLAN 在 trunk 允许列表内等）；⑤ 镜像格式与 VNF 类型匹配（vm-image↔VM、container-image↔容器）；⑥ vpp 线程核 ⊆ 隔离核池且与 VNF 绑核互斥；⑦ vpp 大页偏好与资源池页大小一致；⑧ per-NIC dpdk dev 接口必须是 DPDK 物理口；⑨ 资源池配额不足时给出缺口明细（含所选页大小池的余量）；⑩ NUMA 亲和不一致（vNIC 物理 NIC 与 VM 内存不同 NUMA）给出性能警告；⑪ VM 指定的 hugepage-size 对应页池必须有足够余量 |
| FR-CFG-012 | **管理口自锁保护**：管理口 IP/网关变更时，若变更请求来自 SSH 会话，强制要求以 `commit confirmed` 方式提交（确认期内旧配置可自动回滚恢复连通）并输出警告；管理口变更全程入审计 |

### 3.4 命令树（顶层设计，完整命令树见附录 B）

**操作模式顶级命令**：

```
clear     configure  exit      help      monitor   ping      quit
request   restart    set       show      start     traceroute
```

**show 命令族（示例）**：

```
show version                       show system  (uptime,cpu,memory,hugepage,storage)
show interfaces   (physical|vpp|management, statistics, detail)
show virtual-switches            show vrfs                show routes
show acls                        show nat                 show spanning? (无)
show virtual-machine-functions   (list/detail/interface/statistics)
show container-functions
show images                      show resource-pools
show alarms                      show log(vnf | system | audit)
show configuration | compare rollback n
```

**request 命令族（运维动作，示例）**：

```
request virtual-machine-functions <name> start|stop|restart|console|snapshot
request container-functions <name> start|stop|restart
request system software add|rollback|reboot|shutdown
request system configuration backup|restore
request interfaces <name> enable|disable
```

**配置模式层级（示例）**：

```
[edit]
  system                  # hostname, ntp, dns, syslog, login(user/class), api, management
  interfaces              # 物理网卡: 描述、启用/禁用、MTU（驱动接管由安装期完成）
  virtual-switches <name> # type l2|l3; l2: bridge-domain, vlan access/trunk, ports[]
                          #             l3: vrf, l3-interface ip/, static-routes
  acls <name>             # rules: 五元组 + action, 绑定到虚拟交换机端口或 L3 接口
  nat                     # source-pool, rules (nat44)
  port-mirroring <name>   # source-port, direction, analyzer-port
  qos                     # 限速策略（cir/cbs），绑定端口
  resource-pools          # hugepages(size,count), cpu-isolated-cores[], numa 映射
  virtual-machine-functions <name>
        # image <repo内镜像>, vcpu <count> (从资源池绑定), memory-mb <hugepage>
        # interfaces: {name, type vhost-user|sriov-vf, virtual-switch, vlan, mac}
        # cloud-init: user-data, ssh-keys;  serial console enable
  container-functions <name>
        # image, vcpu, memory-mb, interfaces(type memif, virtual-switch), env, command
```

### 3.5 权限（login class）

- 预置 class：`super-user`（全部）、`operator`（show + request 生命周期 + ping 等，禁配置）、`read-only`（仅 show）。
- class 定义为命令树节点的允许/拒绝位图，可自定义新 class；用户归属 class。
- 本地用户口令加盐哈希存储；外部 AAA（RADIUS/TACACS+）V1 不实现，AAA 模块以接口抽象预留。

---

## 4. 网络功能需求（VPP）

### 4.1 物理网卡接入

| 需求 ID | 需求 |
|---|---|
| FR-NET-001 | 业务网卡在安装/初始化阶段由 DPDK 兼容驱动（ice/i40e/ixgbe/mlx5 等，按支持矩阵）接管，纳入 VPP 管理；CLI `show interfaces physical` 展示 |
| FR-NET-002 | 独立管理网卡不交 VPP（保留内核驱动），专用于 SSH/API/syslog/Prometheus；管理面与业务面完全隔离 |
| FR-NET-003 | 业务网卡支持启用/禁用、MTU、描述配置；链路状态变化产生告警事件 |
| FR-NET-004 | SR-IOV VF 管理：在物理口上创建/回收 VF，VF 可直通指定 VM（其余 VF 与 PF 流量仍可进 VPP） |

### 4.2 虚拟交换机（L2/L3）

| 需求 ID | 需求 |
|---|---|
| FR-NET-010 | L2 虚拟交换机 = VPP bridge domain + MAC 学习；端口成员：物理口、VM vhost-user 口、容器 memif 口、VLAN 子接口 |
| FR-NET-011 | 端口 VLAN 模式：access（untag VID）/ trunk（tagged VID 列表/native） |
| FR-NET-012 | L2 cross-connect（两端口直通，无 MAC 学习）作为轻量选项 |
| FR-NET-013 | L3 虚拟交换机 = VRF + L3 接口（IP/掩码）+ 静态路由（含默认路由，IPv4/IPv6）；L3 接口可挂物理口 VLAN 子接口或作为 VM 网关 |
| FR-NET-014 | **L2 交换机 BVI 三层网关**：L2 虚拟交换机可配三层网关（`gateway ip <prefix>`，实现为 VPP BVI 接口接入 bridge domain 并归入 VRF——缺省为该交换机专属 VRF，可显式指定），VM 经 L2 交换机即可跨网段互通、可叠加 ACL/NAT |
| FR-NET-015 | 虚拟交换机/端口按名称引用，删除前校验无成员引用 |
| FR-NET-016 | 虚拟交换机/端口计数与状态通过 `show` 与 API 实时呈现（govpp stats） |
| FR-NET-017 | **链路聚合（bond）**：支持 LACP active/passive 与静态聚合，bond 名可在虚拟交换机端口、L3 接口等一切接受接口名处引用；基于 VPP bonding plugin |
| FR-NET-018 | **LLDP**：可全局/按接口启停、通告间隔可配；`show lldp neighbors` 展示邻居（基于 VPP lldp plugin） |

### 4.3 高级网络功能（V1）

| 功能 | 需求 |
|---|---|
| ACL | 五元组（src/dst IP、协议、端口）+ action(permit/deny)、优先级序；绑定对象：虚拟交换机端口、L3 接口；方向 ingress/egress；基于 VPP acl plugin。**V1 不含逐规则命中计数**（govpp v0.13.0 无法安全启用 VPP 26.06 的按接口计数，见附录 A #68；降 V2） |
| NAT | NAT44 源地址转换（地址池 + 规则：内网段 → 出接口/池），支持静态 1:1 发布（VPP nat44 plugin）；仅作用于 L3 交换机 |
| 端口镜像 | 将源端口 ingress/egress 流量复制到分析端口（VPP span），供外部抓包 |
| QoS/限速 | 端口级限速（CIR/CBS，VPP policer），入方向优先 |

### 4.4 VM / 容器接入

| 需求 ID | 需求 |
|---|---|
| FR-NET-020 | vhost-user（VM 默认）：VPP 创建 `VirtualEthernetHostvpp` socket，VM virtio-net网卡经共享大页内存直连 VPP；VM 必须使用大页内存 |
| FR-NET-021 | SR-IOV VF 直通：VM 可绑定 VF，绕过 VPP；占用该 VF 时禁止其 PF 端口进 bridge domain（驱动一致性约束） |
| FR-NET-022 | memif（容器）：VPP 为每个容器 vNIC 创建 memif endpoint，容器侧经 virtio-user/memif socket 接入；socket 文件挂载进容器 |
| FR-NET-023 | VNF vNIC 删除/迁移虚拟交换机时，VPP 侧接口与虚拟交换机成员关系同步更新；vhost-user 断连（VM 关机）时端口置 down 并告警 |
| FR-NET-024 | tap 接口接入方式列入 V2（兜底兼容非大页 VM） |

---

## 5. 计算功能需求（KVM + Docker）

### 5.1 资源池

| 需求 ID | 需求 |
|---|---|
| FR-CMP-001 | 全局资源池：大页内存池（页大小 2M/1G，数量）、隔离核列表（isolated cores，NUMA 感知）；配置在 `[edit resource-pools]` |
| FR-CMP-002 | 创建/修改 VNF 时从池中分配 vCPU（核绑定 + NUMA 亲和）与大页内存（VM 可指定 2M/1G 页池，见 FR-CMP-019）；配额不足时 commit 失败并给出明细（缺多少核/内存/哪个 NUMA） |
| FR-CMP-003 | VNF 删除后资源自动归还池；`show resource-pools` 展示总量/已分配/空闲 |
| FR-CMP-004 | 资源池变更（缩减）前校验不被存量 VNF 占用 |
| FR-CMP-005 | 系统底座优化由安装器预置（isolcpus、内核大页参数、NMI watchdog、tuned 等），CLI 侧可查看并在资源池调整时联动内核参数 |

### 5.2 VM VNF 生命周期

| 需求 ID | 需求 |
|---|---|
| FR-CMP-010 | 创建：指定镜像、vCPU/内存（资源池分配）、vNIC 列表（见 4.4）、cloud-init 用户数据/SSH 密钥；落库后经 libvirt 定义并按需启动 |
| FR-CMP-011 | 操作：start/stop/restart（request 命令 + API）；状态实时查询（libvirt） |
| FR-CMP-012 | 修改：vCPU/内存/vNIC 调整（关机状态下生效；热调整列 V2） |
| FR-CMP-013 | 删除：级联删除 vNIC、VPP 端口、快照；二次确认（CLI 需确认提示，API 用参数确认） |
| FR-CMP-014 | 串口 console：`request virtual-machine-functions <name> console` 进入串口（telnet-to-serial），`Ctrl-]` 退出 |
| FR-CMP-015 | 快照：创建/列出/回滚/删除（qcow2 内部或外部快照）；快照操作产生事件 |
| FR-CMP-016 | cloud-init：以 NoCloud seed ISO 注入 user-data 与 SSH 公钥 |
| FR-CMP-017 | VM 异常退出（QEMU crash/OOM）产生 critical 告警事件 |
| FR-CMP-018 | **附加数据盘**：VM 可挂载额外 virtio 数据盘（创建空盘指定容量或引用镜像），生命周期随 VM；快照可含数据盘 |
| FR-CMP-019 | **内存类型（backing）**：hugepage（默认，vhost-user 必需）/ normal（普通内存，仅限无 vhost-user vNIC 的 VM）；commit 校验见 FR-CFG-011① |

### 5.3 容器 VNF 生命周期

| 需求 ID | 需求 |
|---|---|
| FR-CMP-020 | 基于 Docker 运行时；创建指定镜像（本地仓库或 registry 拉取）、CPU/内存限制、memif vNIC、env/command |
| FR-CMP-021 | start/stop/restart/删除；`show container-functions` 展示状态与日志访问入口（`request container-functions <name> log`） |
| FR-CMP-022 | 容器异常退出产生告警事件；重启策略（no/on-failure）可配 |

### 5.4 镜像管理

| 需求 ID | 需求 |
|---|---|
| FR-CMP-030 | 本地镜像仓库目录（VM：qcow2/ISO；容器：经 Docker 分层存储），仓库容量与使用率可查 |
| FR-CMP-031 | 上传：镜像文件经 scp/sftp 传入 `/data/incoming/` 后由 CLI/API 引用导入（导入成功自动清理），或经 API multipart 直接上传；下载：从 HTTP(S) URL 拉取，支持进度显示、sha256 校验、断点续传（URL 拉取） |
| FR-CMP-032 | 镜像元数据：名称、类型（vm-image/container-image）、大小、sha256、格式、备注、导入时间 |
| FR-CMP-033 | 删除前校验无 VNF 引用；容器镜像经 Docker API 删除 |
| FR-CMP-034 | 外部仓库对接（Docker Registry 协议）列为 V2 候选 |

---

## 6. API 需求

### 6.1 总则

| 需求 ID | 需求 |
|---|---|
| FR-API-001 | REST over HTTPS（自签证书，可换）、Bearer Token 认证（登录换 token，TTL 可配，可吊销） |
| FR-API-002 | OpenAPI 3.0 规范文件随产品发布（`GET /api/v1/openapi.json`），Web 控制面据此开发 |
| FR-API-003 | 资源模型与 CLI 配置层级一一对应（附录 B 映射表）；CLI 每条配置/查询命令必须能落到具体端点 |
| FR-API-004 | 配置类写操作遵循与 CLI 相同的事务语义：API 提供 candidate 编辑 + commit 端点（或默认"单请求即提交"模式，二选一，见 6.3） |
| FR-API-005 | 错误响应统一 JSON 结构：code、message、detail[]（校验错误逐条列出） |
| FR-API-006 | 事件订阅：SSE/WebSocket 推送告警与状态变化事件 |
| FR-API-007 | 版本化路径 `/api/v1/`；分页（limit/offset）用于列表端点 |

### 6.2 资源模型（主要端点）

| 资源 | 端点示例（GET/POST/PUT/DELETE） |
|---|---|
| 会话/认证 | `/api/v1/login`、`/api/v1/logout` |
| 系统 | `/api/v1/system/{status,version,hugepages,ntp,dns,syslog,users,classes}` |
| 接口 | `/api/v1/interfaces`、`/api/v1/interfaces/{name}` |
| 虚拟交换机 | `/api/v1/virtual-switches`、`/{name}`、`/{name}/ports` |
| VRF/路由 | `/api/v1/vrfs`、`/api/v1/vrfs/{name}/routes` |
| ACL/NAT/SPAN/QoS | `/api/v1/acls`、`/api/v1/nat/rules`、`/api/v1/port-mirroring`、`/api/v1/qos/policies` |
| 资源池 | `/api/v1/resource-pools` |
| VM VNF | `/api/v1/virtual-machine-functions`、`/{name}`、`/{name}:start`、`:stop`、`/{name}/console`（token 化的 websocket/telnet 代理）、`/{name}/snapshots` |
| 容器 VNF | `/api/v1/container-functions`、`/{name}`、`/{name}:start` 等 |
| 镜像 | `/api/v1/images`（POST multipart 或 `{url, sha256}`） |
| 配置事务 | `/api/v1/configuration/{candidate,commit,rollback,diff}` |
| 告警/事件 | `/api/v1/alarms`、`/api/v1/events`（SSE） |
| 指标 | `/metrics`（Prometheus 格式） |

### 6.3 事务模式（已定，见附录 A #22）

API 采用 **candidate 端点显式化**：写操作默认写入 candidate，调用 `POST /configuration/commit` 生效；同时支持请求头 `X-NFVIS-Auto-Commit: true` 让单请求直接提交，便于脚本与未来 Web 的"向导式"交互。CLI 内部即消费同一组端点。

---

## 7. 系统级配置需求

| 需求 ID | 需求 |
|---|---|
| FR-SYS-001 | `[edit system]`：hostname、时区、NTP、DNS、管理口 IP（独立管理网卡静态配置） |
| FR-SYS-002 | 大页内存：安装期预留 + 运行期通过资源池调整（1G/2M 页），调整需 reboot 生效并明确提示 |
| FR-SYS-003 | 绑核/隔离核：安装期 isolcpus 基线 + 资源池动态划分（cpuset cgroup），CLI 可查当前隔离核与占用 |
| FR-SYS-004 | syslog：本地 journald 必备；远程 syslog（RFC 5424）可配目标/级别/facility |
| FR-SYS-005 | Prometheus 指标端点：系统（CPU/内存/磁盘/大页）、VPP（接口收发包/错误/drop、bridge domain）、VNF（状态、vCPU 占用）、告警计数 |
| FR-SYS-006 | API 服务：HTTPS 端口、token TTL、并发连接限制可配 |
| FR-SYS-007 | `show version` 汇报 nfvis 版本、Ubuntu 版本、VPP/DPDK/libvirt/qemu/docker 版本 |
| FR-SYS-008 | **VPP 数据面配置（`[edit vpp]`）**：main-core、worker 核列表、buffer/主堆内存、DPDK 队列与描述符（支持全局默认 + 单网卡覆盖两级，覆盖项删除后回落默认）、插件开关；由 nfvisd 依据 committed 配置生成 VPP startup.conf，纳入事务引擎（可 compare/rollback），CLI 语法见《CLI 命令树完整设计》§2.9 |
| FR-SYS-009 | VPP cpu/memory/dpdk 类变更需重启数据面生效：commit 成功但输出警告并产生 warning 告警（pending-restart），`request vpp restart` 按新配置重建并经恢复收敛重放网络配置、vhost-user 重连 |
| FR-SYS-010 | VPP 主线程/worker 核从 `resource-pools cpu isolated-cores` 中**保留分配**，与 VNF vCPU 绑核互斥（资源池账本先扣 VPP 保留核）；`vpp memory hugepage-preference` 必须与 `resource-pools hugepages page-size` 一致，否则 commit 校验失败 |
| FR-SYS-011 | **TLS 证书管理**：API 服务证书/私钥可安装（文件导入）与自助重签（自签），SSH host key 可重新生成；证书临近过期产生告警 |
| FR-SYS-012 | **硬件健康监控**：CPU 温度、风扇、电源（IPMI/Redfish，无 BMC 时降级 lm-sensors）、磁盘 SMART；指标入 Prometheus，越限产生告警，`show system hardware` 展示；告警阈值可配（`set health thresholds`，API `PUT /system/health/thresholds`） |
| FR-SYS-014 | **内核启动基线托管（FR-CMP-005 的落地）**：大页与隔离核的唯一真源为 `resource-pools`，`[edit system] kernel` 承载其余启动参数（nmi-watchdog / transparent-hugepages / iommu / tuned-profile / params）；由同一生成器产出 GRUB 片段（`/etc/default/grub.d/99-nfvis.cfg`，不改主文件）与 fstab 大页挂载，安装器首次应用、`request system kernel apply` 后续调整、`rollback` 回退；变更后 commit 给警告并提示 `request system reboot`，`show system kernel` 展示「内核基线（cmdline）/ 运行实际 / 配置期望」三方对照与差异（一致性即 pending_reboot 判定），重启后收敛 |
| FR-SYS-013 | **日志轮转与保留策略**：本地日志（含审计）保留天数与容量上限可配（`set system syslog local retention-days/max-size-mb`），超限滚动覆盖；审计日志 rolling 前自动包含于最近一次配置备份提示，避免静默丢失 |

---

## 8. 运维需求

### 8.1 软件升级（deb 包级）

| 需求 ID | 需求 |
|---|---|
| FR-OPS-001 | `request system software add <deb包/URL>`：校验签名/依赖 → 升级 nfvis 包 → 自动重启 nfvisd → 校验版本 → 报告结果；失败自动回退到旧版本运行 |
| FR-OPS-002 | `request system software rollback`：dpkg 回退到上一版本 |
| FR-OPS-003 | 升级不触碰 committed 配置（schema 兼容迁移由事务引擎负责）；整机 reboot/shutdown 需 CLI 确认 |

### 8.2 备份与恢复

| 需求 ID | 需求 |
|---|---|
| FR-OPS-004 | `request system configuration backup`：导出 committed 配置 + 用户/镜像清单为单个 JSON 归档，可下载 |
| FR-OPS-005 | `restore`：导入归档并作为 candidate 提交 |
| FR-OPS-006 | 镜像文件本体不进配置备份；`show system storage` 监控磁盘占用并告警 |
| FR-OPS-007 | **恢复出厂（zeroize）**：`request system zeroize` 清空数据分区配置/镜像/VNF 并重置本地账号，需 super-user 双重确认；系统重启后进入初始化向导状态 |

### 8.3 恢复收敛（必备）

| 需求 ID | 需求 |
|---|---|
| FR-OPS-010 | nfvisd 启动时：读取 committed 配置 → 对比 VPP/libvirt/Docker 实际状态 → 收敛差异（补建/修正缺失对象）；无法收敛的项转入告警而非阻塞启动 |
| FR-OPS-011 | nfvisd 运行中检测到 VPP 重启：自动重放网络配置；VM vhost-user 重连恢复 |
| FR-OPS-012 | 整机重启后由 systemd 拉起 nfvisd，按 8.3/FR-OPS-010 恢复全部业务（含按配置自启的 VNF） |
| FR-OPS-013 | **nfvisd 自守护**：systemd Restart=always + 崩溃自动拉起；连续快速崩溃进入退避并产生告警；sd_notify watchdog 集成 |

### 8.4 事件与告警

| 需求 ID | 需求 |
|---|---|
| FR-OPS-020 | 事件模型：severity（info/warning/error/critical）、source、code、message、time、clear 条件 |
| FR-OPS-021 | 触发源：接口 link down/up、VNF 异常退出、资源池耗尽、磁盘/内存阈值、commit confirmed 超时回滚、版本校验失败等 |
| FR-OPS-022 | `show alarms`（含 active/resolved）、API 查询与 SSE 订阅、可转发 syslog |

### 8.5 日志与审计

| 需求 ID | 需求 |
|---|---|
| FR-OPS-030 | nfvisd 分级日志（debug/info/warn/error）入 journald，级别可配 |
| FR-OPS-031 | 审计日志独立通道：登录/登出、配置变更 diff、commit/rollback、生命周期操作、API 调用（user、time、source IP、动作） |
| FR-OPS-032 | VNF 控制台/串口访问记录入审计 |

### 8.6 诊断与抓包

| 需求 ID | 需求 |
|---|---|
| FR-OPS-040 | **tech-support 一键打包**：`request system tech-support generate` 生成诊断归档（版本、committed 配置、日志、审计、状态快照、core dump 清单），CLI/API 可列出与下载 |
| FR-OPS-041 | **core dump 管理**：VPP/QEMU/nfvisd 崩溃转储收集至专用目录（容量上限+滚动清理），可列出/导出/删除，纳入 tech-support 归档 |
| FR-OPS-042 | **数据面抓包**：在 VPP 接口上按报文数/时长启停抓包，导出 pcap 文件供下载（`request vpp trace ...`）；与 port mirroring（SPAN）互补，用于远程排障 |

---

## 9. 安全需求

| 需求 ID | 需求 |
|---|---|
| FR-SEC-001 | 管理面仅监听管理网卡；业务网卡上不得出现管理服务端口 |
| FR-SEC-002 | CLI/API 全部操作经 login class 授权；API token 与用户绑定且继承其权限 |
| FR-SEC-003 | 口令策略（长度/复杂度/有效期可配）、口令加盐哈希存储、连续失败锁定（阈值/时长可配） |
| FR-SEC-004 | HTTPS/TLS 1.2+；镜像 URL 拉取默认要求 sha256 校验 |
| FR-SEC-005 | 会话空闲超时（CLI 默认 10 分钟）与 API token TTL |
| FR-SEC-006 | 底座最小化：安装器基于 Ubuntu 最小集 + 固定版本组件清单，关闭无关服务；SSH 禁 root 口令登录 |
| FR-SEC-007 | 敏感信息（token、口令）在日志与 `show` 输出中脱敏 |
| FR-SEC-008 | **口令自助修改**：登录用户可修改自身口令（CLI 与 API），需验证旧口令；修改入审计 |

---

## 10. 性能与容量目标（实验室/POC 级）

| 指标 | 目标 |
|---|---|
| VNF 数量 | ≤10 个 VM + ≤20 个容器同时运行 |
| 业务网卡 | ≤4×10GbE（DPDK 接管） |
| VPP 转发 | 10GbE 单跳 L2 转发接近线速（≥14 Mpps/口，64B），具体以实测报告为准 |
| vhost-user VM 吞吐 | 单 VM vNIC ≥ 8 Gbps（TCP，MTU 1500，实测口径） |
| CLI 命令响应 | show 类 P95 ≤ 1s；commit（≤50 条变更）P95 ≤ 5s |
| API 响应 | 列表/详情 P95 ≤ 500ms（不含长任务） |
| 恢复时间 | nfvisd 重启后网络收敛 ≤ 30s；整机重启到业务恢复 ≤ 3 分钟 |
| 镜像拉取 | 支持 ≥1GB 文件断点续传 |

> 正式指标在 POC 验收时以基准测试报告为准；规格书指标为设计目标下限。

---

## 11. 非功能需求

| 需求 ID | 需求 |
|---|---|
| NFR-001 | 可维护性：nfvisd 单二进制 + systemd 单元；结构化日志；核心模块（事务引擎、govpp 封装、libvirt 封装）单元测试覆盖率 ≥ 70% |
| NFR-002 | 可移植性：nfvis-cli 与 API 模型不依赖 VPP 具体实现细节，底座版本升级只需适配编排器层 |
| NFR-003 | 兼容性：支持矩阵（NIC 驱动、VM 镜像要求 vhost-user 大页、容器基镜像要求）随产品文档发布 |
| NFR-004 | 可扩展性：V2 方向——多节点集群、tap 接入、动态路由（Linux-CP + FRR）、外部镜像仓库、SNMP、VM 热迁移——均不要求破坏 V1 数据模型 |
| NFR-005 | 国际化：CLI/API 错误消息英文为主，中文文档 |
| NFR-006 | 时间同步依赖：所有审计/告警时间戳依赖 NTP，未同步时事件带未同步标记 |

---

## 12. V1 明确范围外（V2 候选清单）

tap 接口 VM 接入；动态路由协议（OSPF/BGP）；VXLAN overlay；SNMP；多节点集群与集中管理；VM 热迁移；VM 热升级（vCPU/内存）；外部镜像仓库对接；RADIUS/TACACS+；整机镜像 A/B 升级；企业级镜像治理（签名/漏洞扫描）；Web 控制面（本规格书 API 即为其数据契约）；sFlow/IPFIX 流量采样；VRRP 网关冗余；storm control / port security / MAC 数量限制；GPU 等通用 PCI 设备直通；qemu-guest-agent（VNF 内 IP/状态上报，`show` 展示 guest 视图）；容器 exec/交互式终端；CLI 批处理脚本（`nfvis-cli -f <file>`）；登录 banner；镜像仓库配额与版本 tag 管理；静态路由 ECMP（多下一跳）；自动定期配置备份；管理面主机防火墙策略配置；**ACL 逐规则命中计数**（需 govpp 升级或自解析 stats segment，附录 A #68）；**物理业务口 link 状态告警**；**告警转发远程 syslog 与日志级别联动**；`GET /api/v1/openapi.json` 运行时端点与列表分页 offset；管理口强制隔离/sha256 默认强制/SSH 强化等 FR-SEC 强制力项；**容器镜像目录名与 Docker tag 的一致性**（经 CLI `request images upload` 上传的容器镜像，其目录项名若与 tar 内嵌 tag 不一致，则 `set container-functions image` 无论用哪个名字都不可用——用目录名则下发 Docker API 404（`docker: not found`），用 tag 则被配置校验拒为「仓库中不存在镜像」；唯一可用组合是上传时 name 恰好等于 tag。需决策：目录是否记录 tar 内嵌镜像引用，或在校验/下发时给出明确指引，决策 #76 §4①）。

---

## 附录 A：需求问答决策记录

| # | 决策点 | 结论 |
|---|---|---|
| 1 | 部署形态 | 单节点一体机，预留多节点扩展 |
| 2 | VM 网络接入 | vhost-user + SR-IOV VF 直通 + memif（容器）；tap 后置 V2 |
| 3 | CLI 配置模型 | 完整 JunOS 事务模型（candidate/commit/confirmed/rollback/compare） |
| 4 | 容器运行时 | Docker |
| 5 | 交换功能范围 | L2 + L3 静态路由（VRF）；ACL/NAT/SPAN/QoS 进 V1 |
| 6 | 动态路由 | V1 不做，仅静态路由 |
| 7 | 管理口 | 独立管理网卡，与业务面隔离 |
| 8 | 镜像管理 | 本地仓库 + URL 拉取（sha256 校验、断点续传） |
| 9 | 生命周期特性 | cloud-init 注入、VM 串口 console、VM 快照/回滚 |
| 10 | 资源模型 | 全局资源池（大页/隔离核）+ 按 VNF 分配 |
| 11 | 规模定位 | 实验室/POC 级（≤10 VM + ≤20 容器，≤4×10GbE） |
| 12 | 进程架构 | 单守护进程 nfvisd + 薄客户端 nfvis-cli |
| 13 | 状态存储 | SQLite（配置/事务/回滚历史） |
| 14 | API | REST + OpenAPI 3.0 + Bearer Token（+ SSE 事件） |
| 15 | 升级机制 | deb 包级升级，失败自动回退 |
| 16 | AAA | 本地用户 + login class，外部 AAA 接口预留 |
| 17 | 观测 | Prometheus + 远程 syslog + 事件告警；SNMP 后置 |
| 18 | DPDK 队列 | 全局默认 + 单网卡覆盖两级；覆盖项删除回落默认 |
| 19 | 深度检查补齐（进 V1） | L2 交换机 BVI 三层网关、VM 附加数据盘、内存 backing、TLS 证书管理、commit 校验规则集（FR-CFG-011）、LACP bond、LLDP、IPv4+IPv6 静态路由、数据面抓包、tech-support 打包、core dump 管理、硬件健康监控、zeroize、口令自助修改、nfvisd 自守护 |
| 20 | 深度检查补齐（列 V2） | 见 §12 追加项（sFlow/IPFIX、VRRP、storm control、GPU 直通、guest-agent、容器 exec 等） |
| 21 | 二次检查修复 | VM 大页页大小选择（hugepage-size + ⑪号校验）、管理口自锁保护（FR-CFG-012）、LLDP 定为顶级 `[edit protocols]` 层级（API `/protocols/lldp`）、日志轮转保留（FR-SYS-013）、镜像导入传输机制（/data/incoming）、system 动作族 API 端点补齐（reboot/shutdown/zeroize/software/ntp:sync）、健康阈值 API；V2 补 ECMP/自动备份/管理面防火墙 |
| 22 | API 事务模式确认 | 写操作默认进 candidate + 显式 commit 端点；`X-NFVIS-Auto-Commit: true` 支持单请求直提（§6.3 定稿） |
| 23 | 交付就绪检查 | 分支定名 main；干净克隆构建/测试通过；OpenAPI 引用全部自洽；文档无未决标记；开发环境（Win10+Git Bash+WSL / nfvis-vm）与初始化脚本就绪 |
| 24 | ConfigDocument 契约补全（M1 事务引擎建模时发现） | OpenAPI ConfigDocument 补入 `vpp`（FR-SYS-008 要求纳入事务引擎可 compare/rollback）、`bonds`（FR-NET-017）、`protocols.lldp`（FR-NET-018）三个 CLI 已有但契约漏列的层级；InterfaceUpdate 补 `name`、`sriov.vf_count`（FR-NET-004）、`ingress_policy`（QoS 绑定）；ResourcePool 大页池补配置项 `count`（total/allocated/free 为 GET 运行态视图）。L3 交换机的 l3-interface/静态路由数据按附录 B 映射存于同名 Vrf 条目。login-users 与 health-thresholds 层级随 M2 AAA/系统模块补入 |
| 25 | 本地用户存储与 AAA 运行态（M2 AAA 开工确认） | 本地用户/login class/口令策略存于配置文档 `system.login`（与 CLI §2.2 层级一致，声明式，可 compare/rollback）；口令仅存加盐 PBKDF2-SHA256 哈希（`pbkdf2$sha256$<iter>$<salt>$<hash>`，标准库 crypto/pbkdf2，≥600k 轮），明文口令与哈希不回显于任何 show/API 输出；连续失败锁定计数与 API Token 为运行态（内存），nfvisd 重启后 token 失效需重新登录、锁定状态清零（实验室定位可接受，V2 可持久化）；首次启动无本地用户时由 nfvisd `-init-admin-password` 参数或随机口令（打印 stdout 一次）引导创建 admin（super-user），无任何用户时拒绝登录 |
| 26 | API 会话列表端点（M2 配置事务 API 开工确认） | 补 `GET /system/configuration/sessions`（FR-CFG-009 会话锁列表的 API 落点，CLI 已有 `show system configuration sessions`）；API 会话以 `user@api` 为持有者标识，与 CLI 会话（`user@ssh`/`user@console`）互相独立，同用户跨接入方式并发编辑按会话锁规则互斥 |
| 27 | 配置节点注释存储（W4 annotate 落地确认） | annotate 语句的注释以语句路径（空格连接的 CLI token，如 "system hostname"）为键存于配置文档顶层 `annotations` 字段——注释随事务引擎可 compare/rollback，随 load/save 导入导出；show configuration 以注释行渲染；delete annotate <path> 清除 |
| 28 | 内部端点入契约（W8） | `/cli/execute` 与 `/cli/candidates` 登记进 OpenAPI 并标注 `x-internal: true`——仅供 nfvis-cli 使用（CLI 专用通道，命令树契约是其真正的接口），不承诺第三方兼容；W9 一致性测试据此校验「注册路由 ⊆ 契约端点」 |
| 29 | prototype 与 internal/schema 漂移处置（M3 T0-2） | 采用「薄演示」方案：删除 `prototype/` 自带的命令树/事务/编辑器副本（`tree.go`/`engine.go`/`editor.go` 及其独立 `go.mod`），prototype 并入主模块并直接引用 `internal/schema` 的命令树，仅演示 `?`/Tab 补全与无歧义缩写消歧；事务/历史/空闲超时等一律以 `nfvis-cli` 为准。既消除同一语义两套实现持续漂移的根因，又保留命令树的离线教学价值 |
| 30 | startup.conf 生成与 pending-restart（M3-2 落地确认） | nfvisd 依据 committed `vpp` 段生成 `/etc/vpp/startup.conf`，键名以实装 VPP 26.06 的默认 startup.conf 为准：`hugepage-preference` 映射 `memory.default-hugepage-size`；`buffers-per-numa` 属独立 `buffers` 段而非 `memory`；`workers-per-numa` 映射 `cpu.workers`。`dpdk dev` 以 **PCI 地址**为键，单网卡覆盖按 `interfaces` 名配置、由编排器运行态经 sysfs 解析 PCI（PCI 地址不入 committed 配置，保持配置可移植）；覆盖项缺省回落 `dev default`。FR-SYS-010 在生成前校验 worker/main 核 ⊆ `resource-pools cpu isolated-cores`、`hugepage-preference` ∈ 资源池页大小。pending-restart 判定为**已应用 vpp 段哈希 ≠ 当前 committed vpp 段哈希**（committed 变更即置位，不依赖引擎钩子），经 `GET /vpp/status` 暴露、`POST /vpp/restart` 重建并重启后清除；两端点本次登记入 OpenAPI 并标 `vpp` tag |
| 31 | L2 编排映射（M3-3 落地确认） | 虚拟交换机名 → bridge domain：**BD ID 由名字 FNV-1a 哈希取低 24 位确定性派生**（0 保留），同一名字恒等，便于恢复收敛重放且不依赖配置顺序，无需在 committed 配置中持久化 ID。端口映射：普通端口 → `sw_interface_set_l2_bridge`（物理口/bond 解析 sw_if_index）；access VLAN → `create_subif`（one_tag + 外层 VID）后挂接子接口；trunk → 每个 tagged VID 建子接口（exact_match），`native` 非零时另挂物理口；cross-connect → `sw_interface_set_l2_xconnect` 对称挂接且限两端口（FR-NET-012）。VNF/容器端口在 M4 前不支持（下发报错，不静默跳过）。成员摘除：govpp v0.13.0 的 `sw_interface_details` 无 `bd_id`，无法直接 dump BD 成员，故编排器维护进程内挂接登记表用于「删端口后同步」；跨进程完整收敛由 M3-8 恢复收敛负责。`GET /virtual-switches/{name}/mac-table` 由 `l2_fib_table_details` 提供并按 sw_if_index 反查端口名/VLAN |
| 32 | L3/VRF 与 BVI 网关映射（M3-4 落地确认） | VRF 名 → IP table ID 由名字 FNV-1a 取低 24 位确定性派生（0 保留），IPv4/IPv6 共用同一 ID 分别建表（`ip_table_add_del`）。L3 接口：`vlan > 0` 时先 `create_subif` 建子接口再置表配地址；地址经 `sw_interface_add_del_address`（v4/v6 由前缀自动区分）。静态路由经 `ip_route_add_del`（下一跳递归解析，`SwIfIndex = ~0`），含默认路由 `0.0.0.0/0`。BVI 网关（FR-NET-014）：`bvi_create` 建 BVI → `sw_interface_set_l2_bridge`（port type **BVI**）挂入 BD → 配地址并置于 `gateway.vrf`（缺省专属 `vr-<交换机名>`，删除交换机时一并删表；引用共享 VRF 时不删表）。`GET /vrfs/{name}/routes` 经 `ip_route_dump` 提供运行态 FIB。适配层 `*_govpp.go` 是薄 binary-API 封装，本地单测无法真实驱动（govpp mock adapter 的 Connect 阻塞），由 nfvis-vm 集成测试覆盖，故 `make check` 覆盖率门槛排除之（`COVER_EXCLUDE=_govpp.go`） |
| 33 | bond 与 LLDP 映射（M3-6 落地确认） | bond（FR-NET-017）：`bond_create` 创建后经 `sw_interface_set_interface_name` 改名为配置名，使交换机端口/L3 接口可按名引用；`bond.lacp == nil` → 静态聚合映射 VPP `BOND_API_MODE_XOR`（哈希分发），`lacp.mode = active|passive` → `BOND_API_MODE_LACP` 且成员 `is_passive` 按 mode 设置；成员经 `bond_add_member`/`bond_detach_member` 增删，模式不可原地修改、变更即重建；bond 与成员均置 admin-up。LLDP（FR-NET-018）：全局经 `lldp_config`（tx-interval，tx-hold 缺省 4），按接口经 `sw_interface_set_lldp` 启停，`Enabled=false`/配置移除时关闭已启用接口；邻居表经 `lldp_dump` 提供并反查接口名，标识字节去尾部 NUL。bond/LLDP 本次一并纳入 `NetworkProvider` 与 apply 计划（此前 Bond/Protocols 仅校验未下发） |
| 34 | 运行态端点补齐与统计来源（M3-7 增量三） | SR-IOV：`PUT /interfaces/{name}/sriov` 的 VF 创建/回收是内核 PF 驱动操作，经 sysfs `sriov_numvfs` 写入（路径与写入可注入；PF 不支持时返回明确错误，vmxnet3 即此类）。NAT 会话：新增 `GET /nat/sessions`（运行态空表返回 `[]`），数据经 `nat44_ei_user_session_dump`。统计来源：接口计数/内存经 VPP **stats segment**（`/run/vpp/stats.sock`，由 API sock 同目录推导；govpp `statsclient` 依赖 Linux，非 Linux 编译为桩）。**已知限制**：VPP 26.06 的 buffer 池与 ACL 插件计数项，govpp v0.13.0 无法解码其 stats 项类型（`/buffer-pools/<pool>/{used,available,cached` 存在但类型为空 → 值为 0），故 buffer 全零池不上报、ACL 命中计数暂缺；解决办法为升级 govpp 或按 26.06 stats 格式自解析，列入后续。**2026-09-14 更新（见 #68）**：buffer 池已由 `vpp_get_stats` 同版本工具回退源解决并加来源标注；ACL 命中计数因 `acl_stats_intf_counters_enable` 在 v0.13.0 下**不可安全调用**（请求被当作 `acl_del` 执行）而正式降 V2 |
| 35 | 恢复收敛落地与告警来源（M3-8，FR-OPS-010/011） | **落点**：`L2Network.EnsureConsistent(ctx,cfg)`（`internal/orchestrator/network`），复用各 Provider 的 `Apply*` 作为声明式重放；调用点在 `cmd/nfvisd`——`Manager.OnConnect` 每次连接成功触发（首连=启动收敛、重连=VPP 重启重放），30s 超时、进程内串行。骨架 §2 规划的独立 `internal/recovery` 包本里程碑不新建（M4 计算/容器收敛一并抽取）。**收敛语义**：运行前清空各 Provider 进程内登记表，使其按 VPP 实况重新判定对象存在性（BD 经 `bridge_domain_dump`、ACL 经 `acl_dump` 按 tag 反查后走 replace、bond 按接口名查询后删除重建、IP table `add` 幂等），再按事务 apply 的依赖顺序全量补齐；单个对象失败不阻塞其余对象。**告警**：`AlarmStore`（进程内，M5 事件总线/持久化告警表就绪后替换）；配置引用接口缺失（`ErrIfaceUnavailable`）→ `RECOVERY_IFACE_MISSING`（error），其余失败 → `RECOVERY_UNCONVERGED`（warning）；经新增 `GET /alarms`（契约已声明，本次注册实现；`state=active/resolved/all`，缺省 active）暴露。**已知限制**：govpp v0.13.0 的 `sw_interface_details` 无 `bd_id`，无法从 VPP 反查 BD 成员与 BVI 归属，故「跨进程删除端口」的残留成员与已存在 BVI 只做补齐不做摘除（nfvisd 重启而 VPP 未重启时可能重复创建一个 BVI）；消除办法为删 BD 重建或重启 VPP |
| 36 | CLI 操作命令接入与底座能力边界（M3-9） | `ping`：VPP 26.06 的 ping 插件只提供 `want_ping_finished_events`（结果通知），**无发起 ping 的 binary API**，故经 `vppctl`（CLI socket `/run/vpp/cli.sock`）执行，参数映射 `count→repeat`、`vrf→table-id`（VRF 名经 `TableID` 派生）；`source <ip>` 按契约保留 IP 语义，先经 VPP `ip_address_dump` 反查该地址所属接口名再作为 `vppctl ping source <iface>`，查不到明确报错。`traceroute`：VPP 26.06 **无 traceroute 插件/CLI/binary API**（`show plugins` 与命令树均无），改由 nfvisd **宿主侧 raw ICMP + 递增 TTL** 实现（`golang.org/x/net/icmp`，prober 接口可注入以便单测）；`vrf` 参数在经 VPP 的路径上无法生效，非空即明确报不支持（记入已知限制），`[vrf]` 仅为契约占位。`clear interfaces statistics [<ifname>]`：经 `sw_interface_clear_stats`，缺省 `~0` 清全部。`monitor interfaces <ifname> [interval <sec>]`：服务端只返回**单次计数快照**（`internal/state` stats segment），nfvis-cli REPL 检测该命令后临时退出 raw 模式、按 interval 本地轮询并在 Ctrl-C 时退出（非 TTY/管道模式退化为单次快照）。三条命令均从占位分支改为真实执行；命令树与 `gen_test` 不变 |
| 38 | NAT44 出接口语义（M3 遗留缺陷修复时确认） | VPP NAT44 的 outside 必须是显式接口，**不支持自动推断**：CLI 语法定为 `action source-pool <name> interface <ifname>`（原「出接口推断」表述作废）；inside 取规则所引用 L3 交换机的成员接口；同一接口内外冲突或未指定 out/inside 时 commit 必须报错（此前静默跳过导致 NAT 完全不生效且无任何提示）。跨 VRF 的 NAT 拓扑语义（inside 与 outside 分属不同 VRF）留待 M4 网络增强时评估 |
| 37 | 集成测试落点与验收主链路（M3-10） | 新增 `test/integration`（build tag `integration` + `NFVIS_VPP_SOCK`，无 socket 自动跳过；CI 不跑），`Makefile integration` 扩展为 `./test/integration/... ./internal/orchestrator/...`。测试**直接装配 `store + config.Engine + 各 Provider`**（不经 nfvisd HTTP），用 `it-` 前缀对象避免与守护进程配置冲突（仍建议先 `pkill -x nfvisd` 避免同口争用）；主链路用例断言「建交换机（BD/地址落地）→ 通流（经 VRF `vppctl ping` 对端收包 > 0）→ 改配置（L3 地址更新）→ 收敛（手工删 BD 后 `EnsureConsistent` 补建）」。真机坑（附录 A 已并入交接文档）：VPP 26.06 `ip_address_dump` **不支持 `~0` 全量**（传 `~0`/`0` 均空，须逐口 dump）；ens192/ens224 同挂一个 BD 会形成 L2 环路致 NAT 不回 ICMP。验收记录见 `docs/M3-10-集成测试验收记录.md` |
| 39 | 资源池运行态视图与账本语义（M4-2 落地确认） | `/resource-pools` GET 返回「配置 + 运行态」合并视图：`hugepages[].{page_size,count}` 为配置项，`{total,allocated,free}` 为运行态（total=count）；`cpu.{isolated_cores,numa}` 为配置项，`cpu.{vpp_reserved,allocated[{vnf,cores}],free}` 为运行态。**账本无持久化表**，由 committed 配置按「VM 名升序、核号升序」确定性重算（事务 rollback 后必与配置一致），删除 VNF 即自动归还（FR-CMP-003）。占用顺序**先扣** `vpp cpu main-core + corelist-workers`（FR-SYS-010），剩余隔离核才可分配给 VNF 绑核；池缩减致存量 VNF 配额不足时 commit 失败并逐条给出缺口（FR-CMP-004、FR-CFG-011⑨）。`memory.backing=normal` 的 VM 使用普通内存、**不占用大页池**（FR-CMP-019），故未配置资源池时仍可创建无 vhost-user 的 VM；`backing=hugepage`（缺省）按 `memory.hugepage_size`（缺省取资源池首个页池）扣减 `ceil(size/页大小)` 页（FR-CFG-011⑪）。契约同步：OpenAPI `ResourcePool.cpu` 补运行态 `free`/`vpp_reserved` 字段并标注 `allocated` 语义（此前处理器返回的 `vm_allocated` 映射与契约漂移，一并修正为 `allocated[{vnf,cores}]`） |
| 40 | VNF 生命周期落地（M4-3） | **接口**：`ComputeProvider` 扩为 `DefineVM(vm, alloc)`/`DeleteVM`/`StartVM`/`StopVM`/`RestartVM`/`VMState`/`EnsureConsistent`；`alloc`（绑核+页大小）由 `model.AllocationFor` 从 committed 配置确定性重算后传入，编排层不依赖事务引擎（依赖方向 config→orchestrator→model 不破）。**落盘**：`<vmsDir>/<vm>/{disk.qcow2,seed.iso,data-<disk>.qcow2}` 确定性推导（`<vmsDir>` 缺省 `/var/lib/nfvis/vms`）；主盘经 `qemu-img create -b` 从 `<imagesDir>/<image>`（缺省 `/var/lib/nfvis/images`）backing 克隆；`.iso` 镜像直接作只读 cdrom 引导、不克隆。**cloud-init**：`DefineVM` 生成 `user-data`/`meta-data` 后经 `cloud-localds` 产卷标 `cidata` 的 NoCloud seed ISO（FR-CMP-016；串口 console 访问与审计在 M4-5）。**状态映射**（libvirt→契约）：RUNNING/BLOCKED→running、PAUSED/PMSUSPENDED→paused、CRASHED→crashed、其余/未定义→shutoff/absent；`on_crash=preserve` 保留 crashed 供告警（FR-CMP-017）。**动作语义**：stop=ACPI 关机、超时（缺省 30s）强杀；restart=运行中 ACPI 重启、关机态启动；`autostart=true` 定义后启动（FR-CMP-010/011）。**FR-CMP-012**：修改仅关机态生效，running/paused/crashed 一律 409 并提示先关机（热调整列 V2）。**FR-CMP-013**：删除二次确认——API `DELETE ?confirm=true`（缺省/ false → 400 `CONFIRM_REQUIRED`），CLI 交互确认（M4-12）；级联顺序「强停→undefine（含快照元数据）→清理落盘」，VPP vhost-user 端口/BD 成员摘除在 M4-4。**契约同步**：新增 `POST /virtual-machine-functions/{name}:restart`；`PUT` 补 400/409；`DELETE` 补 `confirm` 参数与 400。**审计**：生命周期动作记 `vm.start`/`vm.stop`/`vm.restart`（FR-OPS-031） |
| 41 | vNIC 接入映射与链路语义（M4-4，FR-NET-020/021/023） | **VPP 侧**：`create_vhost_user_if_v2{IsServer=true, SockFilename, Tag="nfvis:vnf:<vm>:<vnic>"}` 建 server socket（QEMU 侧 domain XML `<source type='unix' mode='client'>`），随后 `sw_interface_set_interface_name` 改名为 `vh-<vm>-<vnic>`（>63 字节回退 FNV 哈希名，确定性）——交换机端口据此按名解析，复用既有 access/trunk/default 挂接逻辑；L3 交换机（VRF）场景置 IPv4/IPv6 表、不配 IP。**socket 权限**：VPP 以自身 umask 创建 socket，而 QEMU 以 `libvirt-qemu` 运行需写权限，**必须 chmod 0777**，否则启 VM 报 `Failed to connect ... Permission denied`。**队列**：QEMU 侧 `<driver queues='2'/>`（2 对队列）；真机实测 1 对队列时握手停在 protocol features。**链路语义（决定性实测）**：vhost-user 接口 link up 需 **guest 加载 virtio-net 驱动并置 DRIVER_OK**（QEMU 此时才发 `SET_MEM_TABLE`，VPP `show vhost-user` 的 `Memory regions` 才非零）；空白盘无 driver 永不 up。故 vNIC 状态判定用 `ADMIN_UP && LINK_UP` 双标志。**断连检测与告警（FR-NET-023）**：`CheckVnfPorts` 对比「配置应有 vNIC」与 VPP 实际（接口缺失/link down）→ `VNF_PORT_DOWN`（warning），恢复自动消警；触发点为 VPP 连接收敛与 VM 生命周期动作后；事件驱动实时告警随 M5 `/events` 接入。**SR-IOV（FR-NET-021）**：VM 侧 `hostdev`（M4-1）；VF PCI 由 sysfs `/sys/class/net/<pf>/device/virtfn<N>` 解析（无 VF 明确报错），并校验「VF 直通占用 PF 时禁止该 PF 进 bridge domain」。**自愈**：VPP 侧按名存在但 socket 不匹配（陈旧残留）时删除重建，避免假幂等。**真机验收**：alpine cloud 镜像引导后 link up、停止后 link down 并产生告警、删除后 VPP 接口与 socket 消失（`vppctl show interface` 全程可见 `vh-<vm>-<vnic>`）；与 VPP 通流（ping 对端）需 guest IP 配置，随 M4-5/M4-11 |
| 42 | 串口 console 传输与鉴权（M4-5，FR-CMP-014/016、FR-OPS-032） | 契约 console 端点语义落实：`POST /virtual-machine-functions/{name}/console` 经 Bearer 鉴权后签发**一次性 ticket**（24 字节随机、TTL 60s、绑定 VM 名，进程内表），返回相对 `ws_url={APIPrefix}/virtual-machine-functions/<name>/console/ws?ticket=<tok>` 与 `expires_in`；`GET .../console/ws` 以 ticket 鉴权（WebSocket 握手无法带 Bearer）并升级为 WebSocket，与域串口透传。**串口接入方式（实测决定）**：不用 go-libvirt 的 `DomainOpenConsoleBidirectional`——该流式 RPC 在本环境实测阻塞在内部 channel 发送（console 一打开即挂起，5 分钟超时）；改为从 domain XML 取 libvirt 分配的串口 pty（`<serial type='pty'><source path='/dev/pts/N'/>`，纯函数 `SerialPtyPath` 解析）并直接以 O_RDWR 打开（nfvisd 以 root 运行，可开 libvirt-qemu 所属 pty；QEMU 持 master、客户端开 slave，语义等价）。ticket **一次性消费**（无论成败即删除，防重放）。**审计（FR-OPS-032）**：console 打开/关闭各记一条 `vm.console`（含用户；打开失败记 failure 与原因）。`serial_console=false` 的 VM 拒绝（409）；域非运行中由 Provider 明确报错并经 ws 首帧文本回显。**cloud-init 组合语义**：`user-data` 与 SSH 公钥须**同时**注入（FR-CMP-016）——仅 user_data 时原样使用；仅 hostname/ssh_keys 时生成最小 `#cloud-config`；两者都有时输出 cloud-init 支持的 **MIME multipart**（`multipart/mixed`：一份托管的 #cloud-config 承载 hostname/SSH 公钥/users + 一份用户 user_data，按 `#cloud-config` 前缀判定 part 类型），避免同份 YAML 出现重复顶层键；`meta-data` 用 VM 名作稳定 instance-id。真机验证：cloud-init `runcmd` 向 `/dev/ttyS0` 写标记 → 经 console 回读断言（同时证明注入生效与串口双向）。CLI `request ... console` 的终端原始模式（Ctrl-] 退出）在 M4-12 接线 |
| 43 | 快照与数据盘语义（M4-6，FR-CMP-015/018） | 快照采用 qcow2 **内部快照**（`DomainSnapshotCreateXML`，`<disks><disk name='vda' snapshot='internal'/>…` 显式列出域 XML 中全部 `device='disk'`，**数据盘一并纳入**；cdrom（seed ISO）不纳入）。快照是**运行态对象**（不进 committed 配置、不参与 rollback），端点：`GET/POST /virtual-machine-functions/{name}/snapshots`、`POST .../snapshots/{snapshot}:rollback`、`DELETE .../snapshots/{snapshot}`。`created_at` 取快照 XML `creationTime`（Unix 秒）；`size_bytes` 因 libvirt/qemu-img 不提供**按快照粒度**大小而记 0（契约可选字段，不得据此做容量判断）。回滚用 `DomainRevertToSnapshot`；删除 VM 时经 `DomainUndefineFlags(SnapshotsMetadata)` 一并清理快照元数据（M4-3 已启用）。数据盘（FR-CMP-018）」空盘按 `size-gb` 建、引用镜像则克隆，路径 `<vmsDir>/<vm>/data-<name>.qcow2`，生命周期随 VM（删除 VM 目录时一并删除）。**已知行为（实测）**：宿主侧直接写 qcow2（如 `qemu-img dd -O qcow2 of=<盘>`）会破坏内部快照的 L1/refcount，之后 libvirt `qemu-img snapshot -a` 报 `Failed to load snapshot: No such file or directory`；因此磁盘**内容级**回滚证明必须在 guest 内写入后进行，随 M4-11。快照操作记审计 `vm.snapshot.create|rollback|delete`（FR-OPS-031；FR-CMP-015「快照操作产生事件」的审计通道，/events 推送随 M5）。
| 44 | 容器编排与 memif 接入（M4-7，FR-CMP-020~022、FR-NET-022） | **Docker**：经 Docker Engine API（unix socket，29.x 实测）创建/启停/重启/删除容器与取日志；创建规格映射 `memory_mb→HostConfig.Memory`、`vcpu→NanoCpus`、`restart_policy∈{no,on-failure}`、`command→Entrypoint`、`args→Cmd`、`env`（键排序保证可复现）、**`NetworkMode=none`**（容器网络一律经 memif 接入 VPP，不用 Docker 默认网桥）。**memif**：VPP 侧由 `network.MemifProvider` 创建——`memif_socket_filename_add_del` 注册 `socket-id→路径`，`memif_create{ROLE=MASTER, MODE=ETHERNET, 1 rx/tx queue}` 建端点，接口改名 `mf-<ct>-<vnic>`（>63 字节回退哈希），路径 `/run/nfvis/memif/<ct>-<vnic>.sock`，socket-id/memif-id 由名字确定性派生；socket 权限 0777；容器侧把该 socket 挂载到 `/run/memif/<vnic>.sock`（Binds）并授予 `NET_ADMIN`（有 memif vNIC 时 privileged）。**apply 顺序**：memif 接入与 VM vhost-user 同属「vNIC 段」，先于 bridge-domain、删除在容器/VM 之后。**状态映射**（Docker→契约）：running/restarting/paused→running、created→exited（尚未启动）、dead→dead、exited→exited、不存在→absent。**审计**：动作记 `container.start|stop|restart`（FR-OPS-031）；删除二次确认（API `confirm=true`，缺省 400）。**契约同步**：新增 `POST /container-functions/{name}:restart`；DELETE 补 `confirm` 与 400。**已知限制（如实标注）**：容器内 memif 客户端需镜像自带 memif 支持，离线环境无此镜像，故**容器侧 memif 通流未真机验证**；已验证 VPP 侧 endpoint/接口/socket 创建与容器生命周期/日志（真机 alpine 镜像）。异常退出告警（FR-CMP-022）随 M4-10 |
| 45 | 镜像仓库落地（M4-8，FR-CMP-030~033） | 仓库 = 目录 + `index.json` 元数据索引（`Name/Type/SizeBytes/SHA256/Format/Description/ImportedAt/ImportState`），缺省 `/var/lib/nfvis/images`。**导入路径三选一**：① `POST /images` 多部分文件上传（契约保留）；② `POST /images` JSON `{url,sha256?}` **异步拉取**——先登记 `import_state=downloading`，HTTP(S) 拉取支持**断点续传**（存在 `<dest>.part` 时带 `Range`，校验 `Content-Range` 起点与本地一致；续传时先对已下载部分重建 sha256 摘要），完成后 `ready`、失败 `failed` 并清理 `.part`；③ `POST /images` JSON `{incoming_file}` 从 `/data/incoming` 导入（**路径必须在 incoming 目录内**，成功后 rename 进仓库并清理源文件，返回 201）。`type ∈ {vm-image, container-image}`；容器镜像不经 incoming/URL 落本地文件，经 Docker 管理。**删除（FR-CMP-033）**：`DELETE /images/{name}` 前校验引用（committed 配置中 VM/容器 `image` 名计数，经 `GET /images/{name}` 的 `ref_count` 暴露），被引用返回 **409**；`vm-image` 删文件+索引，`container-image` 经 Docker API（注入 remover）删除。**接入**：`images.Store` 实现 `config.ImageResolver`，nfvisd 注入事务引擎，使 **FR-CFG-011⑤**（镜像存在且类型与 VNF 形态匹配）在 commit 校验生效。**契约同步**：POST /images JSON 补 `incoming_file` 与 201 响应，schema `required` 放宽为 `[name,type]`（url/incoming_file 二选一由校验保证）|
| 46 | 计算/容器恢复收敛落地（M4-9，FR-OPS-010/011/012） | **语义**：`ComputeProvider.EnsureConsistent` 按 committed 补建/重定义缺失 domain（`autostart=true` 时启动）、`ContainerProvider.EnsureConsistent` 补建缺失容器；逐对象处理，单对象失败不阻塞其余（行为同 M3-8 网络收敛）。**告警落点**：新增 `orchestrator.AlarmSink`（`Raise/Resolve`，由 `network.AlarmStore` 实现，避免计算/容器包依赖网络包）；provider 经 `SetAlarms` 注入，未收敛项 `Raise` `RECOVERY_UNCONVERGED`（warning，作用域 `recovery-compute`/`recovery-container`、source=VM/容器名），恢复后 `Resolve`，经 `GET /alarms` 暴露。**FR-OPS-011（VPP 重启恢复）**：网络收敛重放新增**vNIC 接口重放**——`orchestrator.VnfPortsOf(cfg)` 统一派生 VM vhost-user 与容器 memif 端口（路径/VRF 归属同一实现，事务 apply 与恢复收敛共用），`L2Network.EnsureConsistent` 在 BD 之前 `ApplyVnfInterface` 重建接口；domain XML 的 vhost-user chardev 增 `<reconnect enabled='yes' timeout='5'/>`，VPP 重启后 socket 重建时 QEMU 自动重连。**FR-OPS-012（整机重启自启）**：`DefineVM` 同步 `DomainSetAutostart`，libvirtd 重启亦按配置自启；nfvisd 启动收敛补建并按 autostart 启动。**runRecovery 顺序**：网络 → 计算 → 容器 → vNIC 断连检查（30s 超时、进程内串行）。**已知限制**：不从 VPP/libvirt 反查「配置中已不存在」的多余对象（不自动删除，避免误删），与 M3-8 一致 |
| 47 | 运行态异常退出告警（M4-10，FR-CMP-017/022） | **VM crash（FR-CMP-017）**：状态判定改为 **reason 感知**——libvirt `DomainGetState` 的 reason 参与映射，`SHUTOFF/DOWN + reason=CRASHED(3)`（外部 kill QEMU 的实测路径）与 `CRASHED` 状态均映射为契约 `crashed`；`ComputeProvider.CheckVMAlarms` 对 crashed 抛 **critical `VM_CRASHED`**，恢复 running/shutoff 则消警。**容器异常退出（FR-CMP-022）**：`ContainerProvider.CheckContainerAlarms` 对 `dead` 或 `exited` 且 **退出码非零**（经 Docker inspect `State.ExitCode`）抛 **critical `CONTAINER_EXITED`**。**巡检触发**：nfvisd 周期（15s）巡检 + 每次恢复收敛后执行（与收敛共用锁避免并发使用 VPP API）；事件驱动实时告警随 M5 `/events`。**落点**：`orchestrator.AlarmSink`（M4-9 引入）→ `GET /alarms`。**运行态端点**：console/snapshots（M4-5/6）、容器 logs（M4-7）、VM/容器 state 字段、资源池分配视图（M4-2）均已注册，由 W9「注册路由 ⊆ 契约」测试守护；本任务不新增端点（severity 常量 `warning`/`critical` 归 orchestrator） |
| 48 | 主链路集成测试落地与三处缺陷修复（M4-11，FR-CMP/FR-NET/FR-OPS-012） | 新增 `test/integration/m4_main_chain_test.go`：经**真实事务引擎 + applier**下发「建交换机（L2 + BVI 网关）→ 建 VM（vhost-user, alpine 引导 + cloud-init 配 eth0）→ 通流（VPP BVI `ping` guest，**table-id 取 gateway 专属 VRF**）→ 改配置（关机态 vCPU 1→2，domain vcpupin 与账本绑核同步）→ 删除（domain/VPP 端口清理 + 资源归还）」。**修复三处缺陷**：① VNF vNIC 必须在 `virtual-switches[].ports[]` 以 `vnf`+`vnf_interface` 声明才挂接 BD，仅配 `interfaces` 则 vhost 接口非 BD 成员（ping 不通）；② domain XML 增**由 VM 名派生的确定性 UUIDv5**（`DeterministicUUID`），否则 `DomainDefineXML` 重定义同名域报 `already exists with uuid`（改 vCPU 步骤暴露）；③ 删除顺序：BVI 网关是 BD 成员须**先删 BVI 再删 BD**、l2 摘除成员对**已失效 sw_if_index(-2)**按已摘除处理，且 VPP 侧 vNIC 接口删除须**在 BD 删除之后**。验收记录 `docs/M4-11-集成测试验收记录.md`；`make integration` 在 nfvis-vm 全绿 |
| 49 | CLI 计算/容器/镜像命令接入（M4-12，FR-CMP-011/013/015/021/031、FR-OPS-031/032） | **接线原则**：CLI 执行器（`internal/api` 守护进程侧）**直连已注入的运行态接口**（`VMRuntime`/`VMConsoleRuntime`/`VMSnapshotRuntime`/`ContainerRuntime`/`ImagesRuntime`），与既有 M3 网络 show（`cli_net_runtime_show.go`）同源同法——不经自身 HTTP，避免自环且复用同一实现。`cliExecutor` 增 `setComputeRuntime(vm, console, snaps, ct, images)`，`Server.New` 装配（nil = 命令报"未接入"，与相应端点 503 语义一致）。**审计**：CLI 直连运行态不会经 HTTP handler 自动入审计，故 `request` 动作族在**执行器内显式 `engine.Audit`**（FR-OPS-031），动作名与端点侧一致（`vm.start|stop|restart`、`vm.snapshot.create|rollback|delete`、`container.start|stop|restart`、`vm.console`、`images.*`），成功/失败均记；console 记打开/关闭两条（FR-OPS-032）。**delete 交互确认（FR-CMP-013）**：`request … delete` 在 CLI 侧提问 `Delete VNF 'x'? [yes,no]`（容器 `Delete container 'x'? [yes,no]`、镜像 `Delete image 'x'? [yes,no]`），应答非 yes → 中止不动作；yes → 仍走**与 HTTP 端点同一条删除路径**，确认结果经 `CLIEResult.Confirm` 字段回传客户端；非交互会话（无确认能力）直接拒绝（避免脚本误删）。**落点**：命令树/执行器同源不变（`gen_test` 守护），实现文件 `internal/api/cli_compute_oper.go`（request 动作）、`cli_compute_show.go`（show 渲染）、`cli_console.go`（console 终端接管） |
| 50 | CLI console 终端接管（M4-12，FR-CMP-014/FR-OPS-032） | `request virtual-machine-functions <n> console` 经 `POST /virtual-machine-functions/{n}/console` 申请一次性 ticket，再以 WebSocket 连 `ws_url` 桥接串口。**WS 客户端落点**：`pkg/cliclient`（`ConsoleWSURL`/`DialConsole`）——CLI 前端不得 import `internal/api`（archtest 守护），ticket/URL 语义经客户端 SDK 表达。**终端接管**：REPL 检出 console 命令后临时退出 raw 模式（复用 `Editor.Suspend/Resume`，同 M3-9 monitor），本地进入 raw 并把 stdin 逐字节转发、stdout 回显服务端帧；**Ctrl-]（0x1d）退出**并恢复原终端状态，随后按契约续打提示符。非 TTY（管道/脚本）退化：不接管终端，打印明确提示（不得静默挂死）。服务端已是双向桥接（M4-5），本任务只补客户端接管与审计闭合 |
| 51 | 镜像动态候选与 show 视图（M4-12，FR-CMP-030~033） | `schema.DynImages`（`images`）动态候选此前未实现 → `Server.dynamicValues` 补 `case schema.DynImages`，读**镜像仓库运行态**（`ImagesRuntime.List()`，非 committed 配置；镜像不在配置模型中）返回镜像名清单。`show images` 列表/detail 同源读运行态并合并 `ref_count`（经 `images.RefCount(committed)`），与 `GET /images` 视图一致。`show resource-pools` 复用 `resourcePoolView(committed)` 纯函数（M4-2），保证 CLI 与 `GET /resource-pools` 输出同一视图、无第二份账本逻辑。`request images download` 为**异步**（端点返回 202 且进度经 `import_state` 观察），CLI 亦为受理语义（打印"已受理，进度经 show images <n> detail 查看"），不阻塞等待完成 |
| 52 | NAT44 跨 VRF 拓扑语义（T0-1，FR-NET-016） | **VPP 26.06 实测**：NAT44-EI 支持跨 VRF，但**全实例仅一对 inside/outside VRF**（`nat44 ei plugin enable ... inside-vrf <id> outside-vrf <id>`；API `nat44_ei_plugin_enable_disable.inside_vrf/outside_vrf`；接口特性 `set interface nat44 ei in <i> out <o>` 无 VRF 参数）。真机实测两种跨域：① inside 非默认表（tap 10.20.0.2 在 table 100）→ ens224 默认表：`i2o 10.20.0.2 fib 2` ↔ `o2i 192.168.155.240 fib 0`；② ens224 置于 table 200（带地址 192.168.155.240/24）：`i2o 10.20.0.2 fib 0` ↔ `o2i 192.168.155.240 fib 2`，ping 通、会话可见。**V1 语义**：① **inside 转发域**由规则 `virtual-switch`（须 L3 交换机）派生 `TableID(<vs名>)`（L3 交换机与同名 Vrf 条目一一对应，决策 #31/#38），多条规则必须一致；② **outside 转发域**由 `action interface` 出接口所属 VRF 派生（该接口须作为某 L3 交换机/`Vrf` 的 l3-interface 且已配地址——默认表无配置地址途径，NAT 回程不可达），多条规则的出接口必须属于同一 VRF；③ 因此 inside 与 outside 可同表或分属不同 VRF（真正的跨 VRF NAT），受 VPP 单实例约束：全局仅一对 (inside_vrf, outside_vrf)；④ `action interface <ifname>` **必填**（决策 #38），`action source-pool <name>` **可选**——给定时该池地址为外部地址（`nat44_ei_add_del_address_range`，不再额外下发 `add interface address`），未给定时以出接口地址为外部地址（`nat44_ei_add_del_interface_addr`）；⑤ **校验修正**：作废旧规则「action 必须且只能指定 source-pool 或 interface 之一」（与决策 #38 及编排实现互相矛盾，导致 #38 声明的 `action source-pool X interface Y` 语法被校验拒绝）；⑥ 插件开关幂等：状态或转发域变化才下发，转发域切换时先关后开。落点：`NatProvider.SetOutsideResolver`（`L3Provider.TableOfIface`）解析出接口转发域。验收见 `docs/M5-验收记录.md` T0-1 节 |
| 53 | 容器镜像导入路径（T0-2，FR-CMP-030/031） | 修订 #45 中「容器镜像不经 incoming/URL 落本地文件」的表述：容器镜像本体仍由 Docker 分层存储承载、仓库仅登记元数据，但**导入入口统一**——`docker save` 归档经 `POST /images` 的 `incoming_file`（或 URL）或 CLI `request images upload name <n> type container-image file <incoming路径>` 导入，nfvisd 侧执行 `docker image load`（`container.Provider.LoadImage` → Docker Engine API `POST /images/load`，body 为 tar 流）后**清理源文件/临时文件**并登记 `Meta{Type=container-image, Format=docker-archive, SizeBytes, SHA256}`；仓库目录不留镜像文件。**name 必须等于归档内的镜像引用**（如 `alpine:3.20`），因为容器 VNF 的 `image` 字段直接作为 Docker ref 交给 `ContainerCreate`。未接入 Docker 时导入显式报错。删除仍经 Docker API（#45）。真机验收见 `docs/M5-验收记录.md` T0-2 节 |
| 54 | 事件总线与 SSE（M5-1，FR-API-006/FR-OPS-020~022） | 新增 `internal/events`（进程内广播，订阅者缓冲 64，满则丢弃不阻塞发布方；保留最近 256 条供 `Last-Event-ID` 断线补发）。事件源：① 告警变更——`network.AlarmStore.SetNotifier` 在 Raise/重新激活/Resolve/`Sync` 消警时回调，映射 `alarm-raised`/`alarm-resolved`（payload 含 id/severity/code/message/source/state）；② `config-committed`——引擎 `Options.OnCommitted(revision,user)`（锁内回调）；③ `vnf-state-changed`——VM/容器 start|stop|restart 动作经 API 发布（`publishVNFState`）；④ `image-import-progress`——`images.Store.SetProgressSink/SetStateSink`（下载字节进度与 downloading/ready/failed）。传输：`GET /api/v1/events`（SSE，ReadOnly 鉴权，逐帧 Flush，20s 心跳注释帧，`id/event/data` 帧）。`show alarms` 与 `/alarms` 仍读同一 `AlarmStore`（FR-OPS-022）。syslog 转发沿用 journald（远程 syslog 目标为配置项，转发实现随 M5-8）。 |
| 55 | Prometheus 指标端点（M5-2，FR-SYS-005） | 新增 `internal/metrics`（`Sample` + 纯函数 `Render` 输出 text exposition 0.0.4，同名 HELP/TYPE 只一次；不引入外部客户端库）；主机指标（`/proc/meminfo` 内存与大页、`/proc/stat` CPU、`statfs` 根盘）按平台分文件（`host_linux.go`/`host_other.go`，Windows 本地开发返回空）。`GET /api/v1/metrics` **无鉴权**（契约 `security: []`，抓取端为 Prometheus 不经登录）。指标族：`nfvis_system_*`（内存/大页/CPU/磁盘）、`nfvis_vpp_*`（线程、主堆、buffer 池、每接口 rx/tx 包/字节/错误/drop）、`nfvis_config_*`（接口/交换机(BD)/VRF/VM/容器计数）、`nfvis_vnf_running`/`nfvis_vnf_vcpu_allocated`（按 kind 标签）、`nfvis_alarms_active{severity}`。VPP 接口计数与 `GET /interfaces` 同源（state.InterfaceCounters）。 |
| 56 | 配置备份/恢复/恢复出厂落地（M5-6，FR-OPS-004~007） | 新增 `internal/system`（`Manager`：`Backup/List/Path/Restore/Zeroize`）。**备份** = `committed 配置（含 login-users/class/口令策略）+ 镜像清单（元数据，FR-OPS-006 不含文件本体）` 打包为 JSON 归档 `nfvis-backup-<UTC>.json`（缺省目录 `/var/lib/nfvis/backup`，0600），归档头 `format=nfvis-config-backup`/`archive_version`/`version` 便于跨版本识别。**恢复** = 解析归档（校验 format 与版本）→ 经事务引擎 Edit/UpdateCandidate/Commit 提交（FR-OPS-005，全量校验与底座下发同普通 commit；镜像文件不在归档内，恢复后需另行导入，响应返回归档内镜像清单作对照）。**恢复出厂** = 提交空配置（applier 逆序补偿级联删除网络对象/VNF/容器）→ 删除全部镜像（文件/Docker 层）→ 账号随空配置复位（下次启动重新引导 admin）。**端点**：`GET/POST /system/backup`、`GET /system/backup/{file}`（octet-stream，路径限定备份目录内防穿越）、`POST /system/restore`（multipart）、`POST /system:zeroize`（JSON `confirm=true`，否则 400）。**CLI**：`request system configuration backup [to <path>]`、`request system configuration restore <path>`、`request system zeroize`（**双重确认**：首次与二次均返回 `[yes,no]`，REPL 逐次追加 `--yes`，执行期剥离全部尾部 `--yes` 再校验命令树）。全部动作入审计。 |

| 57 | 诊断归档与 core dump 管理落地（M5-4，FR-OPS-040/041） | 新增 `internal/system` 两组件：**`CoreDumps`**（扫描缺省 `/var/lib/nfvis/coredumps`，从文件名推断进程名 `core.<process>.<pid>.<ts>`/`<process>.core`/`core-<process>-*`；`List/Path/Delete(file 空=全部)/Prune(按总字节上限滚动保留最新，缺省 2 GiB)`）；**`TechSupport`**（`Generate` 产出 tar.gz，分节 `version.json`（nfvis/VPP/内核/主机名）、`config.json`（committed）、`audit.json`、`status.json`（VPP 状态视图）、`logs.txt`（`journalctl -u nfvisd` 尾部，回退系统日志）、`core-dumps.json`（清单）、`README.txt`；单节失败仅写入 `section error` 而**不中断归档**；`List/Path`）。**端点**：`GET/POST /system/tech-support`、`GET /system/tech-support/{file}`（octet-stream）、`GET /system/core-dumps`、`DELETE /system/core-dumps?file=`（缺省全部，204）；生成时顺带 `Prune` 转储。**CLI**：`request system tech-support generate`、`show system tech-support`、`show tech-support`、`show system core-dumps`、`request system core-dumps delete [file <name>]`。**生产采集**：内核 `kernel.core_pattern` 需指向 coredumps 目录（或启用 systemd-coredump 后由收集器落盘）——**nfvis-vm 实测** Ubuntu 缺省 core_pattern 走 apport 管道，故验收时临时改为 `/var/lib/nfvis/coredumps/core.%e.%p.%t` 触发真实 SEGV 验证后恢复；安装期固化随 M5-10 的 postinst。core dump 清单纳入 tech-support 归档（FR-OPS-041）。 |

| 58 | CLI 剩余契约命令接线（M5-9 增量） | 新增接线：`show system uptime|cpu|memory|storage|hugepages`（数据源 `internal/metrics` 主机采集——Linux 读 `/proc`/statfs、非 Linux 为空；`uptime` 由 `/proc/uptime` 转为 `nfvis_system_uptime_seconds`）；`show users`（committed `system.login.users`，口令哈希不外显）；`show log audit`（`Engine.AuditTrail`，与 `GET /audit-logs` 同源）、`show log system`（经 `Options.LogSource` 注入的 `journalctl` 尾部）、`show log vnf`（指引到容器 `log` / VM `console`）；`show nat` 增补 committed 配置渲染（池/规则/静态映射行 + 运行态会话表）；`show system core-dumps|tech-support`、`show tech-support`（M5-4）。新增 `POST /alarms:clear`（`AlarmStore.Clear` 仅删已 resolved，`all=true` 或不删活动告警；204）+ CLI `request alarms clear [id <id> | all]`（FR-OPS-022）。**仍余占位**（随对应里程碑）：`show system hardware`（M5-5）、`show vpp capture` 与 `request vpp trace …`（M5-3）、`monitor vnf`、`request system software|reboot|shutdown|ntp|api tls`（M5-7/8）。 |

| 59 | 数据面抓包落地（M5-3，FR-OPS-042） | **VPP 26.06 实测**：govpp v0.13.0 无 `pcap` binapi（库内仅有 `pcap_trace_on/off` 二进制消息但未生成绑定），故与 ping 同法（决策 #36）经 `vppctl`（CLI socket）：`pcap trace rx tx intfc <if> max <n>` 开始、`pcap trace off` 停止并**固定写 `/tmp/rxtx.pcap`**（CLI 形式无 filename 参数；实测输出 `Write N packets to /tmp/rxtx.pcap`）。**V1 语义**：① 单会话（有活动会话时 `POST /vpp/capture` 返回 409）；② `count` 映射 VPP `max_packets`（**缓冲深度**，环形，超出丢最旧）——VPP 无"已抓包计数"接口，故**不实现"达到 count 自动停止"**，需显式 stop/export（契约 `count` 描述已按实测调整）；③ `filter_acl` **不支持**（VPP 26.06 pcap trace 仅支持按 error 过滤），非空返回 400（不静默忽略）；④ `DELETE /vpp/capture` = 停止**不导出**（丢弃），CLI `request vpp trace export` = 停止并搬入导出目录（改名 `nfvis-cap-<if>-<UTC>.pcap`）后可经 `GET /vpp/capture/{file}` 下载。**端点**：`GET/POST/DELETE /vpp/capture`、`GET /vpp/capture/{file}`；**CLI**：`request vpp trace start interface <if> [count <n>]`、`stop`、`export`、`show vpp capture`（并接线 `request vpp restart`）。落点 `internal/orchestrator/network/capture.go`（`VPPShell` 注入，单测假实现）。 |

| 60 | deb 打包与自守护落地（M5-10，FR-OPS-013、骨架 §2 deploy） | **systemd 单元** `deploy/nfvis.service`：`Type=notify`（sd_notify 就绪）、`Restart=always`/`RestartSec=3`、`StartLimitIntervalSec=60`+`StartLimitBurst=5`（连续快速崩溃退避）、`WatchdogSec=30`、`Environment=NFVIS_DB/NFVIS_LISTEN/NFVIS_VPP_SOCK`、`ExecStart=/usr/bin/nfvisd`。**自守护** `internal/systemd`（Linux/非 Linux 分文件）：`Notify(state)` 向 `$NOTIFY_SOCKET`（AF_UNIX datagram，未由 systemd 管理时空操作）、`WatchdogInterval()` 取 `WATCHDOG_USEC/2` 周期喂狗；nfvisd 就绪时发 `READY=1` 并按周期发 `WATCHDOG=1`（FR-OPS-013）。**打包** `deploy/debian/{postinst,prerm,postrm}` + `Makefile deb`（Linux 上 `dpkg-deb`）：内容 `/usr/bin/{nfvisd,nfvis-cli}`、`/lib/systemd/system/nfvis.service`、`/usr/share/doc/nfvis/`（OpenAPI + 命令树 + 规格书 + M5 验收记录）。`postinst` 幂等创建 `/var/lib/nfvis/{images,backup,captures,coredumps,tech-support,vms}`、`/data/incoming`、`/run/nfvis/{vhost,memif}`(0777)，校验大页/isolcpus/vpp/libvirtd/dockerd 与 core_pattern 并**仅提示不阻断**（避免底座差异致 dpkg 失败），`daemon-reload`+`enable` 但**不自动 start**（避免安装期抢占网卡）。版本注入：`api.VersionStr` 由 `const` 改 `var`，打包经 `-ldflags "-X …/internal/api.VersionStr=<ver>"`。 |
| 61 | 软件升级/回退与电源/ NTP 落地（M5-7，FR-OPS-001~003） | 新增 `internal/system.SoftwareManager`（宿主命令经 `Runner` 注入，单测假实现）：**add** = 本地 `.deb` 或 http(s) URL（可选 sha256 强校验，流式下载）→ `dpkg-deb -f Package/Version` 校验（包名必须是 `nfvis`，否则拒绝）→ 归档到 `/var/lib/nfvis/software/nfvis_<ver>_amd64.deb` → `dpkg -i`（postinst 负责重启 nfvisd）；报告版本以**包内 Version** 为准。**rollback** = 从归档目录取「版本最高且非当前」的 `.deb` 重新 `dpkg -i`（V1 约定：仅能回退到经 add 安装过的版本；无候选明确报错，FR-OPS-002）。**reboot/shutdown** = `systemctl reboot|poweroff`（调用方须已完成确认，FR-OPS-003）。**ntp sync** = `chronyc makestep` → `ntpdate -u <committed ntp server>` → `systemctl restart systemd-timesyncd` 依次回退。**端点**：`POST /system/software`、`POST /system/software:rollback`、`POST /system:reboot`、`POST /system:shutdown`、`POST /system/ntp:sync`；**CLI**：`request system software add <deb|URL> [sha256 <hex>]`、`request system software rollback`、`request system reboot|shutdown|poweroff`、`request system ntp sync`（add/rollback/reboot/shutdown 均需交互确认，脚本经 `--yes`）。全部动作入审计。 |
| 62 | 硬件健康与阈值落地（M5-5，FR-SYS-012） | 新增 `internal/system.HardwareProvider`（`Runner` 注入，路径可覆盖）：采集**逐级降级**——BMC（`/dev/ipmi0` 且 `ipmitool sdr -j`）→ 无 BMC 时 `sensors -j`（lm-sensors）→ 再不行读内核自带 `/sys/class/thermal/*/temp`；磁盘经 `/sys/block` 枚举 + `smartctl -j -H -A`（无 smartctl 则 `smart_status=unknown`）；根盘使用率由 statfs（`statfs_linux.go`/`statfs_other.go` 分平台）计算。**阈值**（`system.health`，`cpu_temp_celsius`/`disk_temp_celsius`/`disk_used_percent`，0=未设置）在 `Evaluate` 中标注传感器 `ok|warning|critical`（≥阈值 warning、≥阈值×1.1 critical）并给出越限清单。**端点**：`GET /system/hardware`（含 sensors/disks/root_used_percent/thresholds/violations）、`GET/PUT /system/health/thresholds`（配置层 candidate 提交）；**CLI**：`show system hardware`、`set system health thresholds <cpu-temp-celsius|disk-temp-celsius|disk-used-percent> <n>`（新增 `cli_aliases_system.go` 映射嵌套→扁平模型）。**告警**：nfvisd 60s 巡检按 committed 阈值评估，越限 `HARDWARE_THRESHOLD`(warning, scope `hardware`) 经 `AlarmStore`（同 `/alarms` 与 `/events` 通道），恢复即消警。**nfvis-vm 实测限制**：无 IPMI 设备、无 ipmitool/lm-sensors/smartctl（无 `thermal_zone*` 温度节点）→ `bmc_present=false`、传感器为空、SMART `unknown`（降级路径按设计返回空集合而非报错）；阈值→告警链路以「磁盘使用率阈值为 1%」触发真实越限验证。 |
| 63 | TLS 证书与日志保留落地（M5-8，FR-SYS-011/013） | **证书** `internal/system.TLSManager`（Runner 注入）：`Info` 解析 PEM（subject/issuer/有效期/自签/SHA-256 指纹）、`Install` 安装外部证书（校验证书与私钥**配对**后落盘：证书 0644、私钥 0600）、`Regenerate` 生成自签证书（RSA2048、SAN 含主机名与 IP、1 年）、`RegenerateSSHHostKeys`（ssh-keygen -A）、`ExpiryAlarm`（<30 天告警）。**即时生效**：`Server.ListenAndServe` 改用 `tls.Config.GetCertificate` 每次握手读盘 + `MinVersion=TLS1.2`（FR-SEC-004），故换证/重签无需重启。**端点**：`GET/PUT /system/tls`（PEM 文本安装）、`POST /system/tls:regenerate`；**CLI**：`request system api tls regenerate`、`request system ssh host-key regenerate`；**配置声明**：`set system api tls cert-file <path>|key-file <path>`（外部证书）、`set system api tls self-signed regenerate`（声明自签，缺证书时由 nfvisd 在 commit 后生成）——经 `cli_aliases_system.go` 映射嵌套→扁平模型，commit 后由 nfvisd `OnCommitted` 落实（失败仅告警不阻塞）。**日志保留（FR-SYS-013）**：`ApplyLogRetention` 按 committed `system.syslog.local.retention-days/max-size-mb` 写 `/etc/systemd/journald.conf.d/nfvis-retention.conf`（SystemMaxUse/MaxRetentionSec）并 `systemctl kill -s HUP systemd-journald` 热重载（不中断日志）；同批新增 `system syslog local|host` 语句的别名映射。证书临近过期与硬件阈值共用 nfvisd 60s 巡检：`CERT_EXPIRING`(warning, scope `tls`) 入 AlarmStore（同 `/alarms`、`/events`）。 |
| 64 | 端到端验收与基准落地（M5-11，规格书 §10） | 新增 `test/e2e`（build tag `e2e` + 环境变量 `NFVIS_API`/`NFVIS_E2E_PASSWORD`，无环境自动跳过，CI 不跑；`make e2e`）。`TestE2EMainChain` **经 HTTP API** 覆盖「装完即用」：登录 → 配置事务（接口/L2 交换机+BVI 网关/L3+l3-interface/健康阈值/日志保留）→ commit → 配置回读 → **SSE 订阅后收到 `config-committed`**（验证实时推送非轮询）→ `/metrics` 关键序列 → 备份/下载/恢复（multipart）→ tech-support 生成 + core-dump 清单 → 抓包启停 → TLS 信息 → 审计含 `config.commit`。**可重复性**：对象名/地址/阈值带 run 唯一后缀 + 前置清理，同一实例重复运行不因「值未变化」失败。`TestE2EBenchmark`（n=30，p50/p95）对 §10 响应类指标出报告：列表/详情/`/metrics` p95 ≤5ms（目标 500ms）、CLI show p95 ≤2ms（目标 1s）、commit（1 变更，含底座下发）p95 ≤13ms（目标 5s）——**全部 PASS**。§10 吞吐/容量类指标（10GbE 线速、vhost-user ≥8Gbps、≤10VM/≤20 容器、恢复时间）需流量发生器与规模压测，**本轮未测**并如实登记。报告见 `docs/M5-11-端到端与基准报告.md`，证据 `docs/evidence/m5/m511-e2e-bench.txt`。 |
| 65 | CLI 契约命令收尾（M5-9 补齐，V1 收尾） | 补齐此前无分发分支的契约命令：`show interfaces[physical|management|<if> [detail|statistics|sriov]]`、`show port-mirroring`、`show qos policies`、`show vpp [threads|buffers|memory|runtime]`、`show lldp neighbors`（契约写法，等价 `show protocols lldp neighbors`）、`help [command]`、`monitor vnf <name>`、`request interfaces <if> enable|disable`（candidate+commit 一步事务）、`request sriov create-vfs|delete-vfs`（sysfs `sriov_numvfs`）。**仍延期 V2**：`request system storage format-data`（破坏性，待数据分区定义）、`request system password change`（需前端交互式口令输入）。新增守护 `internal/api/cli_contract_coverage_test.go`：契约 §1.1/§1.2/§1.3 命令逐条断言不得命中通用 fallback，延期项须显式白名单并注明原因 |
| 66 | 内核启动基线托管（FR-SYS-014，用户要求"内核基线也做进 CLI"） | 大页/隔离核**不新增配置节点**，仍以 `resource-pools` 为唯一真源（避免双源）；新增 `[edit system] kernel` 只放其余启动参数（nmi-watchdog/transparent-hugepages/iommu/tuned-profile/params）。同一生成器（`internal/system.GenerateBaseline`）产出 GRUB 片段与 fstab 行，安装器（deploy/installer）首次应用、CLI `request system kernel apply|rollback` 后续管理。生效语义：内核参数不可运行时改，故 apply 后明确 pending_reboot（`show system kernel` 差异即 pending 判定）并提示 `request system reboot`；写入独立片段（不动 /etc/default/grub）、写入前备份、update-grub 失败自动回退片段，避免"改坏 GRUB 起不来"。**不做**：一次性启动项 + 超时自动回退（JunOS commit confirmed 的完整等价语义）留作 V2，V1 以"备份 + 显式 rollback"兜底 |
| 67 | 数组型标量字段的语句映射（第六轮缺陷修复） | `dns_servers` / `kernel params` / `bond members` 等 `[]string` 字段此前被通用遍历按标量写入：首值类型不符（JSON unmarshal 失败）或静默无变化（`set system dns server` 报"尚未映射"、`set bonds … members` 报"配置不完整"）。修复：新增数组别名表 `statementAliasesArray`（追加 + 按值删除语义），关键字名与字段名不同名时按字段键写入（如 `dns server` → `dns_servers`）；新增 `internal/api/cli_array_scalar_test.go`。通用遍历的 "SP/SPA" 区分保留在 schema 层（`ScalarIsArray` 标注），执行期以别名兜底 |
| 68 | **D-1 技术债落地**：stats 来源标注 + buffer 池回退源；**ACL 命中计数正式降 V2**（FR-SYS-005、§4.3 ACL） | **实测事实（nfvis-vm，VPP 26.06-release + govpp v0.13.0）**：① **buffer 池**——statsclient `GetBufferStats` 能解析池名 `default-numa-0` 但 `used/available/cached` **全为 0**（26.06 的 buffer 池值类型 v0.13.0 不识别），而 VPP 自带**同版本**工具 `vpp_get_stats socket-name <stats.sock> dump machine '/buffer-pools/*'` 输出正确（`9:430184.00:/buffer-pools/default-numa-0/available`；格式 `<type>:<value>:<path>`，连接失败时退出码 1 且原因在 stderr）。② **ACL 命中计数**——`acl_stats_intf_counters_enable` **无法经 govpp v0.13.0 调用**：VPP 26.06 消息表 `135 = acl_stats_intf_counters_enable`、`136 = …_reply`，govpp 解析应答 ID 正确（136），但实际应答为 `acl_del_reply`（ID 108）——二者 CRC 同为 `e8d4e804`（均为仅含 retval 的应答），请求被 VPP 当作 `acl_del`（107）执行。**该调用不仅无效且不安全（可能误删 ACL），故不接线**；另实测绑定/下发后 stats 段仅有 `/err/acl-plugin-*` 错误计数（默认存在），无 `/acl-plugin/<名>/...` 逐规则命中计数。**落地**：① **回退源**——`internal/orchestrator/network` 新增 `StatsTool` 接口 + 纯函数解析器，`Buffers()` 在 statsclient 失败或全零时改调 `vpp_get_stats dump machine '/buffer-pools/*'`；② **来源标注（不静默省略）**——`state.Buffers` 增 `Source`（`statsclient\|vpp_get_stats`）与 `Reason`，`/vpp/status` 增 `buffers_source`/`buffers_unavailable`，`/metrics` 增 `source` 标签与 `nfvis_vpp_buffer_stats_available`，`show vpp [buffers]` 打印来源；③ **ACL 命中计数按 D-1 规则正式降 V2**（V1 不实现、不解析 `vppctl` 文本；见 §12），#34 的"后续解决"表述以本条为准 |
| 69 | **V1 收尾增补**：`GET /api/v1/openapi.json` 端点、远程 syslog 转发、日志级别联动（FR-API-002、FR-SYS-004、FR-OPS-022、FR-OPS-030、FR-SYS-006） | ① **openapi.json（FR-API-002）**：`make docscheck` 由 `docs/NFViS-openapi.yaml` 生成 JSON 并**嵌入二进制**（`internal/api/openapi.json` + `go:embed`），运行时 `GET /api/v1/openapi.json` 直接返回；**无鉴权**（与 `/metrics` 同：该文档亦随 deb 安装于 `/usr/share/doc/nfvis/`，不含敏感信息）；新增 `contrib/scripts/check_openapi_json_sync.sh` 守护「嵌入副本 == YAML 转换结果」，避免契约与运行时双源漂移。② **远程 syslog（FR-SYS-004/FR-OPS-022）**：**在 nfvisd 内实现** RFC 5424 转发（不依赖 rsyslog/syslog-ng，避免安装期依赖与 journald drop-in 无法直连远端的限制）——新增 `internal/system.SyslogForwarder`（UDP/TCP，facility/severity 可配，dialer 可注入以便单测），nfvisd 日志经 `slog.Handler` 包装**同时**落本地与转发；**告警变更**（AlarmStore notifier）经同一转发器发出（FR-OPS-022「可转发 syslog」）。SyslogConfig 补 `facility`/`severity` 两字段（命令树早已声明 `set system syslog host <ip> [port] [facility] [severity]`，此前**执行器未映射**——契约与实现漂移，本次补齐）。③ **日志级别联动（FR-OPS-030）**：`slog.LevelVar` 由 committed `system.syslog.local.level` 驱动，启动与每次 commit 后重载（此前硬编码 `slog.LevelInfo`）。④ **FR-NET-013 v6 路由不可见（A-3 真机验证时发现的缺陷）**：VPP `ip_route_dump` 不显式传 `IsIP6` 时**只返回 IPv4 路由**，而路由下发侧 `IPRouteAddDel` 已按前缀正确设置 v6 下一跳协议——故 v6 静态路由**实际已进 FIB**（真机 `show ip6 fib <table>` 可见 `2001:db8:aaaa::/64` 与 `::/0`）但 `GET /vrfs/{name}/routes`（及 `show routes`）看不到。修复：`L3Provider.Routes` 分别以 v4/v6 dump 后合并（接口签名 `Routes(tableID, isIP6)`）；单测 `TestL3RoutesIncludeIPv6` 与真机 `test/integration/a3_ipv6_test.go` 守护。⑤ **FR-SYS-006 并发连接限制**：`APIConfig.MaxSessions` 此前仅声明未被使用，本次如实改判为**降级**（见 `docs/V1-验收检查表.md` §5.2），不在本决策范围内实现 |
| 70 | **V1 收尾复核（第二轮）**：口令哈希泄露修复、声明式 vf-count 落地、LLDP subtype 解码、审计分页 offset（FR-SEC-007、FR-NET-004/018/021、FR-API-007） | **复核方式**：对剩余 10 条降级 + 4 条未验逐条回代码**证伪**，发现 6 处断言不准，其中 1 处为安全缺陷。① **口令哈希泄露（已修复，真机复现）**——设置/重置用户口令经 commit 落库，而 commit 审计详情是**完整 diff**（`engine.go:456/430` 的 `Detail: model.Diff(...)`），`model.Flatten` 遍历整棵配置树把 `password_hash` 写入 diff；`GET /audit-logs` 仅需 `ClassReadOnly` 且原样返回 detail → **operator/read-only 可取得他人 PBKDF2 哈希**（真机复现 `pbkdf2$sha256$600000$…`）。修复：脱敏置于 **model 渲染层**（`sensitiveLeaf` + `maskSensitive`，键名判定 `IsSensitiveKey` 与 API 共用），**变更检测仍用原值**（否则仅改口令会被判为「无变更」）；`GET /configuration/candidate`/rollback 回显经 `redactConfigView` 移除敏感键。**有意保留的例外**（非「show/API 输出」语义）：`save <file>`（FR-CFG-008，0600，需可 `load` 往返）与备份归档（决策 #56，0600 且 super-user，恢复账号所必需）。② **声明式 `interfaces[].sriov.vf_count` 静默无操作（已修复）**——该字段被持久化、commit 亦成功，但`SetVFCount` 只被命令式 CLI/API 调用，applier 与恢复收敛都不执行它（配置承诺 4 个 VF 而实际 0 个且无提示）。修复：`L2Network.ApplyInterface` 在 `iface.Sriov != nil` 时调用 `SetVFCount`（含 0 = 回收全部），未装配或 PF 不支持即**明确报错**（不静默跳过）；真机 `show configuration` 可见 `vf-count` 已被声明式落实路径接管。③ **LLDP 标识解码忽略 subtype（已修复）**——`ChassisID`/`PortID` 为二进制，旧实现仅去尾部 NUL 就当字符串，真实交换机普遍用 MAC 型 chassis-ID（subtype 4）→ 输出乱码；且 chassis MAC=4 与 port MAC=3 编号不同。修复：`lldpIDBySubtype(subtype, macSubtype, bytes)`（MAC 型格式化、可打印 ASCII 取文本、其余十六进制）。同时**移除契约中无法填充的 `system_name`**（govpp v0.13 的 `LldpDetails` 无系统名字段）。④ **`/audit-logs` 的 `offset` 契约漂移（已修复）**——契约声明 limit/offset，实现静默忽略 offset（返回同一页）；现 `Store.ListAudit(limit, offset)` → `Engine.AuditTrail(limit, offset)` 真正分页，负 offset 按 0、超范围返回空页。⑤ **复核确认成立、暂不改动（登记待决）**：`deploy/nfvis.service` 默认 `NFVIS_LISTEN=:443`（监听全部网卡，与 FR-SEC-001「管理面仅监听管理网卡」相反，且 `MgmtConfig` 无接口名字段、管理口身份在模型中不存在）；TLS 1.2+ 仅在**配置了证书**时成立，**默认以明文 HTTP 提供 :443**（与 FR-API-001「REST over HTTPS（自签证书，可换）」不符，改默认需连带调整 CLI/e2e/deploy，故单列 V2）；SSH 强化（FR-SEC-006）确认完全未实现，且原验收表的证据指针（`postinst:28-36`）实为内核基线提示块、属误标（已在验收表修正）；FR-CMP-014 存在未记录的缺陷（WS/串口断开时空闲阻塞在 `Read`，需按键才退出）、FR-CMP-015 存在未记录风险（`SnapshotRevert` flags=0 且不要求关机，运行中 VM 回滚可能失败），均登记待 V2 处理 |
| 71 | **管理网卡身份与数据面隔离 + 三处「声明了但无实现」清理**（FR-SYS-001、FR-NET-001/002、FR-SEC-001/004、FR-SYS-006） | **起因**：用户提问「现在能否通过 CLI 把网卡绑到 VPP？将来实机能否手动指定某网卡为管理/数据面网卡？」——核查后发现三处问题。① **管理网卡身份此前不存在**：`MgmtConfig` 只有 address/gateway、无接口名字段，管理口无法被声明，隔离无从强制；`show interfaces management` 还硬编码显示 `mgmt0`（系统中并无此接口）。**新增 `system.management.interface`**（CLI/OpenAPI/命令树三处同步），并新增 commit 校验 **`checkManagementIsolation`**：管理网卡不得出现在 `interfaces`（会被 VPP 接管）、`vpp.dpdk.per_dev`、bond 成员、虚拟交换机端口、VRF L3 接口、端口镜像源/分析口中（逐条报错，FR-NET-002/FR-SEC-001）；`show interfaces management` 改为显示真实网卡名。该字段变更受 FR-CFG-012 自锁保护（`sysMgmtChanged` 比对整份 MgmtConfig）——改管理网卡可能切断当前 SSH 会话，须 `commit confirmed`。② **`set system management ip address|gateway` 此前是未映射语句**：树路径为 `management ip address` 而模型是 `management.address`（多出 `ip` 段），执行时报「语句未产生配置变更」——管理口 IP 在 CLI 上根本设不了（FR-SYS-001 长期未真正可用）。本次补齐三条别名映射。③ **修复 Flatten/Diff 的嵌套对象语义缺陷**：`identityKeys` 含 `interface`/`name`/`prefix` 等，而 `walkValue` 对**嵌套对象**（非数组元素）也套用了「身份字段入路径」的跳过逻辑，使「仅以身份键为内容」的对象在扁平化中**整体消失**、set 随即被判「未产生配置变更」。受影响者：新引入的 `system.management.interface`，以及**既有缺陷** `port-mirroring.source.interface`（diff 一直看不见）。修正：身份语义只对数组元素成立。④ **FR-SYS-006 `max-sessions` 真正生效**：该字段在命令树与 OpenAPI 均有声明却全仓无人使用（同 sriov.vf-count 一类）；现 `Server.listener` 按 committed `system.api.max_sessions` 以 `netutil.LimitListener` 限并发连接（0 = 不限），并校验不得为负。⑤ **FR-SEC-004 镜像 URL 拉取默认强制 sha256**：原实现 `opts.SHA256 != ""` 才校验（缺省静默跳过），与规格「默认要求 sha256 校验」不符；现缺省即拒绝并校验 64 位十六进制，契约与 CLI 文档同步（`sha256` 由可选变必填）。⑥ **如实登记（本次未实现，建议单列 V2）**：**FR-NET-001「业务网卡由 DPDK 驱动接管」产品侧完全未实现**——全仓无 `vfio-pci`/`driver_override`/`dpdk-devbind` 任何写入，`vpp.dpdk.dev` 仅是 startup.conf 声明，实机绑定需人工/安装期带外完成（M3 交接文档即为手敲命令）；**TLS 默认明文**（未配置证书时 `:443` 走明文 HTTP）仍待决 |
| 72 | **实机部署闭环**：网卡 DPDK 驱动接管、管理面仅监听管理网卡、默认 HTTPS（FR-NET-001、FR-SEC-001、FR-SEC-004） | **起因**：沿用第三轮的提问（「CLI 能否绑网卡 / 能否指定管理网卡」）推进「装完即用且管理面隔离」闭环。① **FR-NET-001 驱动接管落地**：新增 `network.DPDKBinder`——按 sysfs 标准流程实现 `driver_override` + `bind`/`unbind`（bind：解绑原驱动 → 写 `driver_override=<driver>` → 绑到目标驱动；unbind：解绑 → 清空 override → 触发 `/sys/bus/pci/rescan` 交还内核重新探测），已绑到目标驱动时幂等，目标驱动目录缺失（模块未加载）时明确报错；暴露为 `PUT /interfaces/{name}/dpdk`（`{bound, uio_driver}`，`confirm=true` 必填——绑定会中断该网卡流量）与 CLI `request interfaces <ifname> bind-dpdk [uio-driver <vfio-pci\|igb-uio>]` / `unbind-dpdk`，动作入审计（`interfaces.dpdk`）。**不做配置自动绑定**：绑定必须在 VPP 使用该卡之前完成，属安装/初始化阶段动作（规格原文），自动绑定会引入启动次序风险。**真机实测两点平台行为**（均已在实现中处理）：① 绑定后内核网卡即消失，故**已接管的网卡只能按 PCI 地址定位**（解绑的常态），且绑定结果必须**按 PCI 回读驱动**——按接口名回读会失败并把结果误报为「无驱动」；② 清空 `driver_override` + `rescan` **不足以**让内核重新探测原生驱动（设备停留在无驱动状态），故 `unbind-dpdk` 增 `to-driver <驱动名>` 显式交还（未给出且未自动绑定时返回可操作错误，不静默留下无驱动网卡）。② **FR-SEC-001 管理面仅监听管理网卡**：新增 `system.ResolveListenAddr`——`-listen` 为通配（`""`/`0.0.0.0`/`::`）且已配置管理口地址时，**收敛为管理口地址**；管理口地址未配置在本机时**不收敛**（否则 `net.Listen` 会因 cannot assign requested address 直接启动失败）；显式指定了其它地址时保留并告警。③ **FR-SEC-004 默认 HTTPS**：新增 `TLSManager.EnsureSelfSigned`——未给 `-tls-cert` 时，已装证书再用，否则**自动生成自签证书**（SAN 含主机名与监听地址 IP，`system.ListenSANs`）；仅显式 `-allow-plaintext` 才退化为明文（开发/测试），并记警告。此前缺省即明文，与 FR-API-001「REST over HTTPS（自签证书，可换）」相反 | |
| 73 | **物理业务口链路状态告警**（FR-NET-003） | 规格要求「业务网卡支持启用/禁用、MTU、描述配置；**链路状态变化产生告警事件**」，但此前**只有 vhost-user 的 `VNF_PORT_DOWN`（FR-NET-023），物理口 link down/up 无任何告警**（决策 #71 复核时确认：全仓仅 5 个告警码，无物理口链路巡检）。落地：① `SwIfInfo` 补 `AdminUp`/`LinkUp`（复用既有 `sw_interface_dump`，不新增客户端方法）；② 新增 `L2Network.CheckInterfaceLinks`——对 `interfaces[]` 中**启用**的口，若在 VPP 中存在但 `!(adminUp && linkUp)` 则抛 `INTERFACE_LINK_DOWN`（warning，source=接口名，scope `interface-link`），消息区分「管理态未启用」与「链路 down」；恢复 up 即自动消警；**显式 disable 的口不告警**（用户意图）；VPP 中缺失的口不在此处告警（由恢复收敛的 `RECOVERY_IFACE_MISSING` 负责，避免重复）。③ 接入 nfvisd 的恢复收敛后检查与 15s 周期巡检。**真机验证**（`vppctl set interface state ens192 down` → 巡检 15s → 1 条 `INTERFACE_LINK_DOWN` warning；恢复 up → 18s 内消警，`state=all` 可见 `resolved` 与时间戳；同期 up 的 ens224 无误报）。**未做**：物理口链路状态在 `show interfaces physical` 的运行态列（该列现取自配置，与实测可能不一致）——补它需扩展 `L2Runtime` 与契约，留作后续 | |
| 74 | **列表端点分页补齐**（FR-API-007） | 规格要求「分页（limit/offset）用于列表端点」，但此前**仅 `/audit-logs` 支持 limit**（且 offset 被静默忽略，决策 #70④ 已修），其余列表端点完全没有分页参数。落地：新增 `api.paginate[T]` 纯函数（`limit` 缺省/≤0 = **不截断**——保持既有调用方行为，客户端显式传参才分页；`offset` 缺省 0、负数按 0；**越界返回空列表而非 null**），应用于 14 个列表端点：`/interfaces`、`/virtual-switches`、`/vrfs`、`/virtual-machine-functions`、`/container-functions`、`/images`、`/alarms`、`/virtual-machine-functions/{name}/snapshots`、`/virtual-switches/{name}/mac-table`、`/acls`、`/qos/policies`、`/port-mirroring`、`/bonds`、`/protocols/lldp/neighbors`；契约 14 条路径同步声明 `limit`/`offset`。**注意**：`/protocols/lldp`（LLDP **配置对象**，非列表）与 `/nat`（对象）**不加**分页——泛型函数会让编译期报错，已在代码注释中标注，避免后续误加 | |
| 75 | **快照内容级回滚验证 + 运行中快照改为显式拒绝**（FR-CMP-015） | **① 内容级回滚（T0-5）真机证实**：M4-6 只能在宿主侧 `qemu-img dd` 打标记（直写会破坏 qcow2 内部快照 L1/refcount，revert 报 `Failed to load snapshot`），证不到「内容真的回滚了」。新增 `test/integration/m5_snapshot_content_test.go`：cloud-init `write_files` 写 KNOWN → **串口登录 guest** 改 LATER → 关机建快照 → 启动改 CHANGED → 关机回滚 → 启动后串口回读 = **LATER**（非平凡证据：CHANGED=没回滚、KNOWN=guest 写入没生效）。**② 证伪原交接文档的猜测**：原文记为「运行中回滚**可能撞 libvirt 报错**」——实测**不报错**，但 `DomainRevertToSnapshot(flags=0)` 对运行中域会**替换 QEMU 进程**（virsh 手工确认 PID 137327→137639；固化为 `TestSnapshotRevertOnRunningVMRealLibvirt`，独立复现 138445→138491），即**静默重启该 VM**。静默重启生产 VNF 不可接受，故产品侧 **create/rollback 显式要求关机态**（与 FR-CMP-012「关机态生效」同构）：运行中（running/paused/crashed）返回 409 并提示先关机。**③ 守卫落点的教训（一次真实的「声明了但无实现」）**：初版守卫只加在 HTTP handler，**真机实测 CLI 仍能对运行中 VM 建快照并回滚、VM 被静默重启**——因 CLI 有独立执行路径（`api.cliExecutor.requestVMSnapshot` 直接调 `VMSnapshotRuntime`，不经 handler）。修正为放在两侧**共同依赖**的实现处 `cmd/nfvisd/main.go` 的 `snapshotController`（`requirePoweredOff` 前置），handler 层保留同检查作纵深防御；**这正是 AGENTS.md「命令树与执行器必须同源」的又一实例**。契约：`NFViS-openapi.yaml` 两处快照端点补 `409` 与说明，`NFViS-CLI命令树完整设计.md` 命令行标注需关机态。单测 `internal/api/vm_snapshot_test.go:TestSnapshotRequiresPoweredOff`（三状态 create+rollback 均 409 且**不触达编排器**、关机态放行）。证据 `docs/evidence/v1-closeout-round7.txt` | |
| 76 | **全功能 CLI 测试：9 处缺陷修复（7 处「契约已声明但经 CLI 不可用」+ 1 处契约漂移 + 1 处错误文案）**（FR-SYS-001、FR-SYS-008、FR-CFG-007、FR-NET-013、FR-CMP-011） | 对契约 §1/§2 的命令树做**逐条真机 CLI 测试**（最终 **通过 197 / 失败 1 / 预期报错 2**；唯一失败为已知的 `show vpp runtime` 未接入。脚本 `contrib/scripts/cli-fulltest.sh` 可反复运行；证据 `docs/evidence/v1-closeout-round8.txt`），发现并修复 7 处「声明了但经 CLI 用不了」，全部为**测试清单未覆盖**而长期漏网者：① **`set vpp dpdk dev rx-queues\|tx-queues\|rx-descriptors\|tx-descriptors <n>`（全局默认，§2.9）完全不可用**——`dev` 关键字下有 `<ifname>` 参数子节点，通用遍历据此把 `dev` 当**数组容器**（写 JSON 数组），而模型 `vpp.dpdk.dev` 是**对象**（`VppDevDefault`）→ `cannot unmarshal array into Go struct field VppDPDK.vpp.dpdk.dev`。修：加 4/5-token 别名规则（`dpdkDevDefault`/`dpdkDevDefaultKey`）写对象字段；`delete` 同修（原先 4-token 一律当 ifname 处理）。② **`set system ntp server <ip\|host> [prefer]`（§2.2）不可用**——模型是对象数组 `system.ntp[{server,prefer}]`，CLI 多一层 `server` 关键字且 `prefer` 是无值 flag，通用遍历既落不到 `ntp` 键（报 `缺少取值`）、flag 也无法结尾（报 `未知语句: "prefer"`）。修：加别名规则 + `ntpServer` 助手（flag 语义：置/清 `prefer`）。③ **VM `stop` 成功率报错（FR-CMP-011）**——客户端 HTTP 超时 30s **等于**服务端 ACPI 等待上限（`compute.StopTimeout` 默认 30s），故凡走「guest 不响应 ACPI → 超时强杀」路径的 stop **必定**先触发客户端超时，用户看到误导性的 `连接 nfvisd 失败: context deadline exceeded`，而 VM 实际已停成功（真机实测：31s、`state shutoff`）。修：`cliclient.RequestTimeout` 提至 90s（>服务端上限），并把超时与真正连不上**分开报**（`请求超时（…）：操作可能已在服务端完成，请用 show 确认`）。④ **`show vrfs <不存在> routes` 静默为空**（FR-NET-013）——VPP 对不存在的 VRF 返回空表，原先渲染成 `（FIB 无路由）`，把「VRF 不存在」误报成「无路由」，与 `show vrfs <name>` 的报错口径不一致。修：先校验 VRF 在 committed 配置中存在。⑤ **`annotate` 只认绝对路径**（FR-CFG-007）——与同级 `set/delete/show` 不一致，`edit system` 后 `annotate hostname "x"` 报 `未知语句: "hostname"`。修：`resolveAnnotatePath` 先按当前 edit 层级相对解析，再退回绝对（保留既有写法）。 ⑥ **`set system dns server <ip> secondary <ip>` 不可用**（§2.2）——通用遍历消费完首个 IP 后会下潜到参数节点（为支持「参数的子关键字」），致该参数的同级关键字 `secondary` 不可见（报 `未知语句: "secondary"`）。修：别名规则 `dnsServers` 一次写入两个地址。⑦ **`set resource-pools cpu numa node <n> cores <list>` 不可用**（§2.6）——模型是对象数组 `cpu.numa[{node,cores}]`，CLI 多一层 `node` 关键字 → 原先写成对象（`cannot unmarshal object into ... []model.NumaNode`）。修：别名规则 `numaNode`（cores 经 `expandCores` 展开，与 isolated-cores 同源）。⑥⑦ 由**整理成可复用脚本**（`contrib/scripts/cli-fulltest.sh`，阶段 2 逐条独立会话）后暴露——原阶段脚本「首条失败即中止」把它们掩盖了。 ⑧ **契约漂移：`request api token revoke <token-id>` 位置写错**（§1.2）——契约把它列在**顶级 `request`** 下，而 schema/实现只在 `request system api token revoke` 下（`api` 与 `tls regenerate` 同级）；照契约写会得到 `% 无效命令`。修：**改契约**（该命令 V1 本就只回「请经 API DELETE /login 吊销当前会话，逐 token 吊销随 V2」）。⑨ **core dump 不存在的报错说成「备份归档不存在」**——`coredump.go`/`techsupport.go` 复用了 `backup.go` 的 `ErrNotFound`（文案「备份归档不存在」），排查时被误导。修：各自独立错误（`ErrCoreNotFound`/`ErrDiagNotFound`）。⑨ 加单测 `TestNotFoundMessagesAreContextSpecific`（并要求三者文案互不串味）。**守护**：①②⑥⑦ 补入 `cli_mapping_test.go:contractStatements`（该清单此前漏列即漏网原因，已加注释说明）；③④⑤ 各加单测（`TestCLIAnnotateRelativePath`、`TestCLIShowVrfRoutesValidatesExistence`）；全部经**红-绿**验证（撤修复必失败）。**⑩ 为编制《命令全表》补测时又发现 8 处同类缺陷（尚未修复，已如实登记）**：`set system api tls cert-file <p> key-file <p>`（`未知语句: "key-file"`）、`set system login user <n> password <s> class <c>`（`未知语句: "password"`——语法树把 `<name>` 参数置于关键字之前）、`set system login class <n> allow|deny <path>`（`语句未产生配置变更`，未映射到 `[]string`）、`set virtual-switches <n> cross-connect <a> <b>`（`未知语句: "2"`；且模型仅有 `cross_connect bool`，**CLI 两端口语义与模型不一致，需设计决策**）、`set virtual-machine-functions <n> interfaces <vnic> vlan <n>`（写入字符串而模型为 `int`）、`set … cloud-init ssh-key <key>`（**SSH 公钥必含空格**，而 `set` 无多词取值/引号机制 → FR-CMP-016 的 CLI 注入路径不可用）、`set container-functions <n> interfaces <vnic> type memif virtual-switch <n>` 与 `set container-functions <n> env <key> <value>`。其中「login user 口令哈希落地」「cross-connect 模型语义」需决策，「ssh-key 多词取值」需 CLI 前端引号机制（影响面较广），建议独立一批修复并补入 `contractStatements`。**另：修复 ① 时一度引入回归**——5-token 的「单网卡单项 delete」（`delete vpp dpdk dev <ifname> <参数>`）与全局默认同形，曾被一律按全局默认处理；已按关键字名区分并加单测 `TestCLIDpdkDevDeleteForms`（红-绿验证）。全表见 `docs/NFViS-CLI命令全表.md`。**未修（如实登记）**：容器镜像**目录名须等于 Docker tag** 否则报 `docker: not found`（`request images upload name X` 的目录项名与 `docker load` 落地的 tag 不一致；用 Docker tag 名又会被校验拒为「仓库中不存在镜像」）——需产品决策（是否在目录中记录 tar 内嵌镜像引用），见 §12 V2 候选；`set system login password-policy lockout-threshold <n> lockout-minutes <n>` 单行连写不被支持（两条独立语句可用）——**改契约**（§2.2 已同步）。**已知且未变**：`show vpp runtime` 未接入（govpp runtime，附录 A #34，CLI 已明确提示） |
| 77 | **备份导出件权限缺陷（安全）修复**（FR-OPS-004、FR-SEC-007） | 编写《用户手册》时实测发现：`request system configuration backup to <path>` 的导出件权限为 **0644**，而自动命名的归档是 **0600** —— 且归档内容**含 `password_hash`（pbkdf2$…）**（真机实证 `ls -l` 0644 + `grep password_hash` 命中）。本地任意用户即可读取导出件并获得口令哈希，与决策 #56/FR-SEC-007 记录的既有例外口径（0600、仅 super-user）矛盾。**根因**：CLI 导出用 `copyFile(src,dst)`，其 `os.Create(dst)` 以 0666&~umask 创建；而 `system.Manager.Backup()` 的自动命名归档用 `os.WriteFile(..., 0o600)`，两者不一致。**修**：`copyFile` 改 `os.OpenFile(dst, O_WRONLY|O_CREATE|O_TRUNC, 0o600)`（该函数当前唯一调用方就是备份导出）。单测 `TestCopyFileExportsSecretsAs0600`（Windows 跳过——无 POSIX 权限位），红-绿已验证；真机复验导出件 0600。**同批发现的另 2 处（未修，另立）**：① `deploy/debian/postinst` 第 63 行 `exit 0` 使第 65–80 行的**内核基线代码块不可达**——安装期实际并未应用内核基线（与注释「安装期预置」不符），需手工执行 `nfvis-baseline.sh --defaults` 或 `request system kernel apply`；② `nfvis-cli` 的 `-server` 默认 `http://127.0.0.1:8443` 与 nfvisd 默认 `:443`(HTTPS) **不匹配**，默认参数下连不上（e2e 亦靠显式 `NFVIS_API` 绕过）。三者均记入 `docs/NFViS-用户手册.md` 显式标注 |
## 附录 B：CLI 命令树 ⇄ API 资源映射（摘要，实施期展开为完整文档）

| CLI 配置层级 | API 资源 | VPP/底座落点 |
|---|---|---|
| `virtual-switches <n> type l2` | `/virtual-switches/{n}` | bridge-domain + BD-VRF 绑定 |
| `virtual-switches <n> ports` | `/virtual-switches/{n}/ports` | interface l2 挂接 / VLAN sub-interface |
| `virtual-switches <n> type l3` | `/vrfs/{n}` | VRF table + l3 interface + FIB 静态路由 |
| `acls` | `/acls` | acl plugin + 接口绑定 |
| `nat` | `/nat/rules` | nat44 plugin |
| `port-mirroring` | `/port-mirroring` | VPP span |
| `qos` | `/qos/policies` | policer |
| `resource-pools` | `/resource-pools` | hugepages/cpuset（内核）+ 校验逻辑 |
| `vpp`（startup.conf 生成） | `/vpp/config`、`/vpp/status`、`/vpp:restart`、`/vpp/capture` | startup.conf + govpp 连接/重放；VPP trace/pcap |
| `bonds`（链路聚合） | `/bonds`、`/bonds/{name}` | VPP bonding plugin（LACP/静态） |
| `protocols lldp` | `/protocols/lldp`、`/protocols/lldp/neighbors` | VPP lldp plugin |
| `system`（诊断/硬件/证书/软件/电源） | `/system/{hardware,tls,tech-support,core-dumps,backup,restore,health/thresholds}`、`/system:{reboot,shutdown,zeroize,ntp:sync}`、`/system/software(:rollback)` | ipmitool/smartctl/打包/证书安装/dpkg/systemd |
| `virtual-machine-functions` | `/virtual-machine-functions` | libvirt domain（vhost-user/sriov/hostdev） |
| `container-functions` | `/container-functions` | Docker + memif socket 挂载 |
| `images` | `/images`、`/images/{name}` | 本地仓库目录 + index.json；容器镜像经 Docker |
| `system` | `/system/*` | 内核/sysctl/journald/sshd/api server |
