# Web 控制台信息架构重构设计（单页 → 资源域一级 + 对象详情二级）

| 属性 | 内容 |
|---|---|
| 状态 | 已定前提（用户裁决 6 项，见 §3）；**待用户过目本文档**，通过后转实施计划 |
| 日期 | 2026-09-23 |
| 范围 | V2 增量：Web 控制面（`internal/api/ui/`，同源托管、免构建） |
| 关联 | 规格书 §12（V2 候选）、附录 A 决策 **#115**（免构建/同源托管）、**#128**（界面广度）、**#141**（本文档的裁决） |
| 验收口径 | **每个功能都要在浏览器里真的操作一遍**（Browser Use 黑盒），并与独立事实源对照——见 §11.2 |

## 1. 背景与现状（事实）

- 控制台是**一个页面**、**16 张卡**：系统、资源池、数据面、接口、配置、虚拟机、虚拟交换机、容器、镜像、网络对象、运维动作、审计日志、告警、诊断、抓包、最近事件。
- 前端规模：`app.js` 1855 行、`index.html` 379 行、`style.css` 157 行（共 2391 行）；**免构建**（原生 HTML/CSS/JS，无 npm，产物入库），由 nfvisd 经 Go `embed` 同源托管在 `GET /api/v1/ui/`。
- 刷新：`loadAll()` 每 5 秒把全部卡片兜底刷新一遍；实时走 SSE `GET /events`。
- 覆盖：契约 98 条路径中，界面**已接 70**、**有意不接 28**（`uiNotWired` 逐条写明理由）。
- 规模定位：实验室 / POC 级（≤10 VM + ≤20 容器，决策 #11）。
- 已有可视验收先例：round39（只读总览）、round44~47（配置/诊断/确认范围）、round56 §8（环境复原后复核）——均为"截图 + 只读 DOM 双向印证"。

## 2. 问题（四个症状）

1. **只读信息与破坏性动作同屏**：`重启主机 / 关机 / 重启数据面 / 重签证书` 就长在总览下面，只靠二次确认兜底。
2. **没有深链**：URL 永远是一个地址——不能把"某台 VM"发给同事，刷新回顶部，前进/后退无效。
3. **详情没有位置**：round51~55 加的详情是**共享浮层**（点 ACL/QoS/镜像/容器都复用同一块），一次只能看一个、不能对照、刷新即丢。
4. **可发现性与权限**：靠 Ctrl-F 找"NAT 在哪"；所有卡都渲染、只按角色隐藏按钮，导航无法按角色呈现。

## 3. 已定前提（用户裁决，本文档不重开）

| # | 前提 | 含义 |
|---|---|---|
| 1 | **定位：CLI 的图形等价物** | 界面是主要操作面，覆盖 CLI 的能力（不只是"总览 + 少量运维"） |
| 2 | **全图形鼠标操作** | 操作者**永不需要知道 CLI 语句**；改配置是填表/表格内联编辑，动作是按钮 + 对话框 |
| 3 | **维持免构建** | 浏览器原生 ES modules + hash 路由 + 产物入库；CI 与发版仍不依赖 node |
| 4 | **分级确认** | 低/中/高危三档（§7） |
| 5 | **信息架构：资源域一级 + 对象详情二级** | §5 |
| 6 | **底线（设计方声明）** | 界面**不得绕过** CLI 守卫（管理口守卫、DPDK 守卫、关机态快照、commit confirmed、会话锁）；界面**不新增权限语义**（服务端按 class 判定） |

## 4. 目标 / 非目标

**目标**

- 每个 CLI 能力在界面上有图形对应物——机器判据：`uiNotWired` 从 28 条**收缩到 6 条**（只剩非界面读物）。
- 每个对象有可分享的深链；详情有独立位置（取代共享浮层）。
- 只读信息与破坏性动作分区；危险动作有分级确认与影响面预览。
- 每页只拉自己声明的端点；首屏与轮询开销随页而定（不再全量刷新）。

**非目标（本轮不做，附理由）**

- **命令面板 / 内嵌 CLI**：与"全图形"前提冲突，界面不出现语句输入（只读回显除外，见 §7）。
- **图表化历史曲线**：产品没有时序数据源（`/metrics` 是即时快照），要做需先有时序存储，单列。
- **拓扑可视化**：VPP 拓扑画图需要额外数据源与真机验证，单列。
- **多语言 / 主题**：面向中文单语运维环境，收益低。

## 5. 信息架构

### 5.1 路由与深链

- **hash 路由**：`#/`、`#/compute/vms`、`#/compute/vms/vnf-a`、`#/network/switches/vs-vnf`。
  服务端仍只托管同一份静态资源（`/api/v1/ui/` 不变），**不新增服务端路由** → 契约守护 `routes_contract` 不受影响。
- **深链语义**：任何页面 / 任何对象 / 任何 Tab 都可分享与刷新保持；浏览器前进/后退可用；未登录时先登录再回到目标地址（token 在 sessionStorage，刷新保持）。
- 未知路由 → 提示并回总览；**无权访问 → 只说"无权"，不泄露对象是否存在**。

### 5.2 路由表（声明式）

每条路由声明 `path / 标题 / 面包屑 / endpoints[] / 渲染函数 / 兜底轮询间隔`：

```js
{ path: '#/compute/vms/:name', title: '虚拟机',
  breadcrumb: ['计算', '虚拟机'],
  endpoints: ['/virtual-machine-functions/{name}', '/virtual-machine-functions/{name}/snapshots'],
  poll: 5000, render: vmDetail }
```

**额外收益**：界面覆盖守护（`ui_coverage_test.go`）改为**从路由表提取**已接路径（比现在从源码字面量正则提取更准），"哪个页面接了哪些端点"一目了然，分页后也不会误判。

### 5.3 五种页面模板

| 模板 | 用途 | 关键行为 |
|---|---|---|
| **总览页** | `#/`：健康、告警、关键计数、最近事件 | **不放破坏性动作** |
| **列表页** | 每个资源域一张表 | 筛选 + 行操作 + 「新建」；点行 → 详情 |
| **详情页** | 单对象：对象头 + Tab（按对象类型给不同 Tab） | 生命周期按钮按状态启用；**取代共享浮层** |
| **表单页/抽屉** | 配置域编辑（字段级校验 + 预校验 + 差异预览） | 只写 candidate，不直接生效 |
| **向导页** | 多步操作（新建 VM、内核基线 apply、软件升级、恢复配置…） | 分步可回退；最后一步汇总 = **影响面预览**，再确认 |

### 5.4 一级页面清单

| 一级 | 页面（路由） | 主要端点 |
|---|---|---|
| **总览** | `#/` | `/system/status`、`/system/version`、`/vpp/status`、`/alarms`、`/events`(SSE) |
| **计算** | `#/compute/vms` → `#/compute/vms/:name` | `/virtual-machine-functions`、`/{name}`、`/{name}:start|:stop|:restart`、`/{name}/console`(+`/ws`)、`/{name}/snapshots`(+`:rollback`) |
| | `#/compute/containers` → `/:name` | `/container-functions`、`/{name}`、`/{name}:start|:stop|:restart`、`/{name}/logs` |
| | `#/compute/images` → `/:name` | `/images`、`/images/{name}` |
| **网络** | `#/network/switches` → `/:name` | `/virtual-switches`、`/{name}`、`/{name}/mac-table`、`/{name}/ports`（配置编辑） |
| | `#/network/vrfs` → `/:name` | `/vrfs`、`/{name}`、`/{name}/routes`（大表按需） |
| | `#/network/acls`、`#/network/nat`、`#/network/qos`、`#/network/span`、`#/network/bonds`、`#/network/lldp` | `/acls`(+`/{name}`)、`/nat`(+`/nat/sessions`)、`/qos/policies`(+`/{name}`)、`/port-mirroring`(+`/{name}`)、`/bonds`(+`/{name}`)、`/protocols/lldp`、`/protocols/lldp/neighbors` |
| **系统** | `#/system`、`#/system/hardware`、`#/system/pools`、`#/system/interfaces` → `/:name`、`#/system/kernel`、`#/system/users`、`#/system/tls`、`#/system/ntp` | `/system/status`、`/system/hardware`、`/system/health/thresholds`、`/resource-pools`、`/interfaces`(+`/{name}`、`/interfaces:clear-statistics`、`/{name}/dpdk`、`/{name}/sriov`)、`/system/kernel`(+`:apply`/`:rollback`)、`/system/login-users`(+`/{name}`、`:change-password`)、`/system/tls`(+`:regenerate`)、`/system/ntp:sync` |
| **配置** | `#/config`、`#/config/history`、`#/config/sessions` | `/configuration`、`/configuration/candidate`、`/configuration/check`、`/configuration/diff`、`/configuration/commit`、`/configuration/commit:confirm`、`/configuration/rollback/{n}`、`/system`、`/vpp/config`、`/system/configuration/sessions` |
| **运维** | `#/ops/actions`、`#/ops/audit`、`#/ops/alarms`、`#/ops/diagnostics`、`#/ops/capture`、`#/ops/archives` | `/system/backup`、`/system/tech-support`、`/system/core-dumps`(+`:export`)、`/system/ssh-host-key:regenerate`、`/system/software`(+`:rollback`)、`/system/restore`、`/system:zeroize`、`/system:reboot`、`/system:shutdown`、`/vpp/restart`、`/audit-logs`、`/alarms:clear`、`/system/logs`、`/diagnostics/ping`、`/diagnostics/traceroute`、`/vpp/capture`(+`/{file}`)、`/system/backup/{file}`、`/system/tech-support/{file}` |

### 5.5 覆盖面口径（机器可验收）

`uiNotWired` 的 28 条中 **22 条转为接入**（用户管理、内核基线 apply/rollback、软件升级/回退、恢复配置、恢复出厂、TLS 上传、DPDK 绑定、SR-IOV、NTP 同步、硬件健康、健康阈值、历史回滚、LLDP 开关、MAC 表、持锁会话、`/system`、`/vpp/config`、`/virtual-switches/{name}/ports`…），**保留 6 条非界面读物**：`/metrics`、`/openapi.json`、`/ui`、`/ui/`、`/cli/execute`、`/cli/candidates`（后两条是 `x-internal`，纯图形方案下界面不碰）。

**契约缺口（必须先补契约，规则 1）**：CLI 有 `rollback [n]` 与 `show configuration | compare rollback <n>`，但**没有"列出历史快照"的命令**，REST 也没有对应端点——`#/config/history` 需要"rev → 时间/用户/注释"的列表。故本轮新增：

- `GET /configuration/history`（路径以此为准；返回 `[{rev, committed_at, user, comment, current}]`）；
- CLI 侧对应命令（如 `show system rollback`），**命令树 / OpenAPI / 附录 B 三处同步**；
- `cli_rest_coverage_test.go` 与 `ui_coverage_test.go` 的归类同步（新增端点未归类即红）。

## 6. 配置事务的图形化（`#/config`）

- **域表单**：每个配置域一张表单/表格（系统、接口、虚拟交换机、VRF、ACL、NAT、QoS、SPAN、聚合、资源池、VM、容器、镜像…），**表内增删改 = 对应的 `set/delete`**，操作者不需要知道语句。
- **双通道**：表单 + 原始 JSON（批量/复制粘贴场景）；两者都写 candidate。
- **事务流不变**：`开始编辑`（取会话锁）→ 改 → `预校验`（`/configuration/check`，问题逐条列，不落库不下发）→ `差异`（`/configuration/diff`）→ `提交` / `commit confirmed`（管理口变更强制）/ `丢弃`。
- **历史**（`#/config/history`）：列出历史 rev（时间/用户/注释），**先看差异再回滚**；回滚沿用 CLI 语义——**取历史快照为 candidate，再走提交**（不是直接生效），并落在"中危"确认档。
- **持锁会话**（`#/config/sessions`）：显示谁持 candidate 锁（排障）；释放走"丢弃 candidate"（与 CLI Teardown 对齐，不新增强制夺锁能力）。

## 7. 危险动作与分级确认

| 级别 | 动作 | 界面行为 |
|---|---|---|
| **低危** | 单台 VM 启停/重启、容器启停、清理接口计数、清告警、删快照、取消抓包 | 单击确认 |
| **中危** | 重启数据面、重启/关机主机、内核基线 apply/rollback、删 VM/容器/镜像、重签证书、重生成 SSH host key、配置回滚 | 对话框**列出影响面**（哪些 VNF 会断、是否需重启生效、可否回退）+ 主按钮标红 |
| **高危** | 恢复出厂 zeroize、软件升级/回退、恢复配置 restore、删用户/改口令策略、证书上传 | **输入对象名/确认词 + 10 秒倒计时可取消**；审计记"意图"与"结果"两条 |

三条共同底线：

1. 每个确认框里**只读回显**这条动作对应的 CLI 语句（便于工单与审计对照；操作者不需要输入语句，不违反 §3 前提 2）。
2. **界面不得绕过 CLI 守卫**：管理口守卫、DPDK 绑定守卫、关机态快照、commit confirmed、会话锁都留在两侧共同依赖的实现处（决策 #75），界面只做入口与确认。
3. 界面**不新增权限语义**：服务端按 class 判定，界面按角色隐藏写入口（隐藏不是安全边界）。

## 8. 权限与角色

- 导航与按钮按 class 渲染：`read-only` 只见只读页（总览/列表/详情/审计/告警/诊断的只读部分），写按钮**隐藏**（不是禁用）；`super-user` 全量。
- 与 CLI 权限矩阵**同源**（同一份 class 判定），界面不定义新的权限位。

## 9. 刷新与性能

- 每页只拉**路由表声明的那几个端点**；总览页不再全量刷新。
- 实时：SSE `/events` 事件驱动 + 当前页兜底轮询（默认 5 秒，可调）；大表（VRF 路由、NAT 会话、MAC 表、审计）分页/按需；抓包与日志按需。
- 首屏从"登录后拉全部卡片"降到"拉当前页"。

## 10. 代码结构（免构建下）

```
internal/api/ui/
├── index.html      骨架 + 登录视图 + <script type="module">
├── style.css       现有样式（扩展组件样式）
├── main.js         装配：登录态、路由启动、全局事件（SSE）
├── router.js       hash 路由 + 路由表 + 面包屑 + 404/无权
├── api.js          fetch 封装 / token / 错误归一
├── ui.js           组件函数：el / table / dialog / confirm（分级）/ toast / form 字段
├── console.js      串口 WebSocket（复用现有 stripANSI/termAppend）
└── pages/          compute.js / network.js / system.js / config.js / ops.js / overview.js
```

浏览器原生 ES modules（同源托管、无需打包），产物直接入库；CI 与发版仍不需要 node；发布校验"随包前端与仓库源码逐字节一致"不变。

## 11. 守护与验收

### 11.1 守护改造

- `ui_coverage_test.go`：已接路径**从路由表提取**；`uiNotWired` 收缩到 6 条（§5.5）；新增端点未归类即红。
- `cli_rest_coverage_test.go`：新增的"历史列表"命令/端点按现有纪律归类。
- `user_text` 守护：自动覆盖新增 `.js/.html/.css`（现有实现按后缀扫描，无需改）。
- 发布校验：随包前端与仓库源码逐字节一致（口径不变）；手册 §10.12 重写为新 IA。

### 11.2 Browser Use 操作验收（**本轮起的新口径**）

**每个交付的功能都要在浏览器里真的操作一遍**，并留三类证据：

1. **操作前后 DOM 快照**（可定位、可复核）；
2. **关键步骤截图**（布局/遮挡类问题只有截图能抓——round39 的 CSS `hidden` 事故就是这么抓到的）；
3. **与独立事实源对照**（"界面显示对了"不算完，要证明它真的改到了底座）：

| 界面操作 | 必须对照的独立事实源 |
|---|---|
| 停止/启动/重启 VM | `virsh domstate <vm>` 真的变 shut off/running |
| 提交配置 | 配置库 rev+1、`show configuration` 与界面逐项一致、审计留记录 |
| 开始/停止抓包 | `vppctl` 侧 trace 真的在跑/已导出，导出的 pcap 可下载 |
| 删除镜像 | 引用计数非 0 时被拒（409）、审计留记录、仓库里文件真的没了 |
| 内核基线 apply | `/system/kernel` 的"期望 vs 实际"变化、GRUB 片段真的写了 |
| 高危动作 | 确认词/倒计时真的生效（不输入词不能执行）、审计有"意图"与"结果"两条 |

这条口径同时写入 `AGENTS.md` 与交接文档（随本设计文档同一个 PR 落地）。

## 12. 分刀计划（每刀独立发布、独立验收）

| 刀 | 交付物 | 验收（Browser Use 操作） | 风险 |
|---|---|---|---|
| **1. 骨架刀** | hash 路由 + 路由表 + 导航/面包屑 + 现有 16 张卡原样搬进各页（零新能力）+ 总览瘦身（破坏性动作移出） | 逐页点开、深链可分享、刷新保持、前进/后退；总览页不再出现破坏性动作 | 低（无新能力，纯搬迁） |
| **2. 详情刀** | 对象详情页取代共享浮层（Tab：概览/接口/快照/串口；容器：概览/日志） | 点列表行进详情、Tab 切换、串口真连、快照真建/删、深链直达 | 中（串口/快照是有状态操作） |
| **3. 配置刀** | 域表单覆盖 128 条语句 + 预校验/差异/提交 + 历史（含新端点）+ 持锁会话 | 表单改值 → 预校验正反例 → 差异 → 提交 → 配置库与 CLI 对照；历史页回滚两段式 | 中高（128 条语句的覆盖与表单一致性） |
| **4. 危险动作刀** | 分级确认组件 + 逐个接入 22 条"有意不接" | 每个动作走一遍：影响面预览内容正确、高危确认词/倒计时真拦得住、底座状态真的变 | **高**（破坏性动作进界面） |
> 实施计划**先覆盖第 1 刀**；后续每刀各自成计划与 PR（每刀独立发布、独立验收）。

| **5. 收尾刀** | `uiNotWired` 收到 6 条、手册 §10.12 重写、发布校验口径更新、全套 Browser Use 验收 | 全套操作验收 + `make check` + 真机三件套 | 中 |

## 13. 风险与取舍

- **工作量**：全图形覆盖 CLI 能力是"再造一个操作面"，估计 6~8k 行前端（现 2.4k）。免构建下靠模块化与组件函数自律。
- **一致性维护**：CLI 与界面必须同源（同一 Provider/事务引擎），新增能力要同时想两边——这正是 `ui_coverage_test.go` 存在的意义，本设计把它从"路径引用"升级为"路由声明"。
- **破坏性动作进界面的安全面**：靠 §7 的三条底线 + 分级确认 + 审计双写兜底；**界面不放松任何守卫**。
- **免构建的代价**：没有框架的响应式，页面状态要手写；换来发布链路（deb 可复现、无 npm 供应链、CI 无 node）保持不变。
- **可回退性**：分五刀，每刀独立可发布；骨架刀不改任何能力，最坏情况下可直接回到单页版本（旧 `app.js` 保留在 git 历史里）。
