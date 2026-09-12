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
| ACL | 五元组（src/dst IP、协议、端口）+ action(permit/deny)、优先级序；绑定对象：虚拟交换机端口、L3 接口；方向 ingress/egress；基于 VPP acl plugin |
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

tap 接口 VM 接入；动态路由协议（OSPF/BGP）；VXLAN overlay；SNMP；多节点集群与集中管理；VM 热迁移；VM 热升级（vCPU/内存）；外部镜像仓库对接；RADIUS/TACACS+；整机镜像 A/B 升级；企业级镜像治理（签名/漏洞扫描）；Web 控制面（本规格书 API 即为其数据契约）；sFlow/IPFIX 流量采样；VRRP 网关冗余；storm control / port security / MAC 数量限制；GPU 等通用 PCI 设备直通；qemu-guest-agent（VNF 内 IP/状态上报，`show` 展示 guest 视图）；容器 exec/交互式终端；CLI 批处理脚本（`nfvis-cli -f <file>`）；登录 banner；镜像仓库配额与版本 tag 管理；静态路由 ECMP（多下一跳）；自动定期配置备份；管理面主机防火墙策略配置。

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
| `system` | `/system/*` | 内核/sysctl/journald/sshd/api server |
