# Web 界面覆盖审计 —— `request` 族（动作）与 `show` 族（读）

> **本文件是审计报告，不是生成物，也不改任何产品代码。**
> 口径：**端点级已接 ≠ 界面级可用**（见 `docs/v3-Web-CLI覆盖矩阵.md` §D）。
> 本文回答的是界面级问题：**每条 CLI 命令在 Web 控制台里有没有入口 / 展示位**。

## 0. 快照与只读声明

- **快照时间**：2026-10-16（本机时区）。
- **代码基线**：`git HEAD = c172068`，但**工作树有并发未提交改动**（另一个会话正在改前端）：
  `internal/api/ui/app.js`、`index.html`、`routes.json`、`js/core/dom.js`、`js/views/system.js`
  （新增端口安全写入口 + 新增 `#/ops/monitor` 实时监视页）、新增文件 `js/views/monitor.js`。
  ⚠️ **本报告的「路由/控件/行号」按上述快照读取**；并发会话若继续改动，行号会漂移，
  结论需按 §7 的复核方法重跑一遍。
- **只读**：本轮未修改任何产品代码/配置；仓内唯一新增文件就是本报告。
- **未执行**：未跑 `make check`、未构建、未连真机（按任务约束）。故本文的「有入口」是**静态代码级**判定
  （控件存在、绑定了对应端点、展示位存在），**不含真机点击验证**。

## 1. 口径与方法

### 1.1 数据来源（仓内真源，不凭印象）

| 用途 | 真源 |
|---|---|
| CLI 命令清单（动作族 + 读族） | `internal/schema/tree_oper.go` 的 `OperRoot()`（操作模式命令树；节点字段见 `internal/schema/node.go` 的 `type Node struct`） |
| 路由与页面 | `internal/api/ui/routes.json`（每条路由的 path/view/endpoints/poll） |
| 页面 DOM 与控件 id | `internal/api/ui/index.html` |
| 视图逻辑（取数 + 控件接线） | `internal/api/ui/js/views/*.js`、`internal/api/ui/js/boot.js`、`internal/api/ui/js/core/api.js` |
| 交叉参考（只作线索，不取数） | `docs/NFViS-CLI命令全表.md`、`docs/CLI-REST覆盖核查.md`、`docs/v3-Web-CLI覆盖矩阵.md` |
| 端点语义（判定「等价物」时用） | `internal/api/cli_rest_coverage_test.go`（`cliRESTCoverage` / `cliRESTExceptions`）、`docs/NFViS-openapi.yaml` |

### 1.2 数法（可复现）

`OperRoot()` 是嵌套的 `*Node` 调用树（`K(关键字…)` / `P(参数…)` / `PT(取值参数)` / `V`/`VE`（取值叶子）/
`Opt`（可选）/ `Op`/`Su`（权限档））。枚举规则（**必须与报告数字一起引用**）：

1. 把 `OperRoot()` 的表达式解析成树（解析前先剥掉 `//` 行注释；`pingOperArgs()`/`tracerouteOperArgs()`
   两个返回切片在调用处展开）。
2. **一条语句 = 一个「命令终点」节点**，含三类：
   - 叶子关键字（`K` 且无子节点），如 `show version`；
   - 叶子参数（`PT`，或 `P` 无子树），如 `request system software add <deb>`；
   - **带子树的参数节点 `P`**（参数名入路径、其本身即是一条可执行读物），如 `show interfaces <ifname>`。
3. **选项关键字不单独成句**（它们是父语句的参数）：`name, type, file, url, sha256, count, last, level,
   interface, filter, uio-driver, to-driver, to, vf, id, timeout, state, ipv6, source, vrf`
   （该集合写死在解析器里；取值叶子 `V`/`VE` 一律不入路径）。
4. `Op`/`Su` 只影响**权限档**（O/S），不产生路径段；`Opt` 透明。
5. **额外补 11 条「容器关键字本身即可执行」的读物**（树里它们不是叶子，但 CLI 与 REST 契约都当独立命令）：
   `show interfaces`、`show virtual-switches`、`show vrfs`、`show acls`、`show vpp`、`show bonds`、
   `show images`、`show virtual-machine-functions`、`show container-functions`、`show configuration`、
   `show vpp runtime`。

**结果（可复现校验）**：

| 族 | 树中语句 | 说明 | 计入审计 |
|---|---|---|---|
| `show` | **73** | 含 1 条同命令参数形态（`show log vnf <name> last <n>`，与 `show log vnf <name>` 同命令） | **73 + 11 = 84** |
| `request` | **55** | 其中 **3 条是无动作的纯容器节点**（`request virtual-machine-functions <name>`、`request container-functions <name>`、`request interfaces <ifname>`） | **55 − 3 = 52（动作）** |

> 复现要点：`show` 的 73 条是「树中命令终点」；与《命令全表》行口径（round80 记 show 67 / request 46）
> 与守护表口径（146 条有端点）**都不相等**——三者是**三种不同的数法**（树终点 / 文档行 / 契约策展行）。
> 本文按任务要求以**树**为准，并在 §7.4 说明其中一处差异的后果。

### 1.3 四态判定

| 判定 | 含义 |
|---|---|
| **有入口** | 界面上有可点控件或明确展示位，功能与命令等价（可含参数形态差异，写在备注）。必须给**路由 + 控件/DOM id（或展示位）**。 |
| **部分** | 只能看不能做，或只能做/看一部分（差在哪逐条写）。 |
| **无入口** | 界面上没有。 |
| **设计如此（CLI-only）** | REPL 交互形态 / CLI 文本渲染 / 本机数据源不经 HTTP，不需要界面入口；但必须写明 **Web 的等价形态**。 |

判定细则（避免歧义，先行声明）：

- **读族**的「入口」= 展示位；参数形态（如 `interface <ifname>` 过滤）若可由同页其它列/控件达成，记**有入口**并注明。
- **动作族**若功能可达但走的是**另一条语义路径**（如「写候选 + 手工提交」而非「取锁直改 + 立即提交」），
  记**部分**并写明差异（本报告只对 `request interfaces <n> enable|disable` 用到该细则）。
- 「端点被 Web 调用」不等于「有入口」——本文逐条看的是 **DOM 控件/展示位**，不看端点。

## 2. 汇总

### 2.1 `request` 族（动作，52 条）

| 四态 | 条数 |
|---|---|
| 有入口 | **47** |
| 部分 | **2** |
| 无入口 | **3** |
| 设计如此（CLI-only） | **0** |
| 合计 | **52** |

### 2.2 `show` 族（读，84 条 = 树终点 73 + 容器读物 11）

| 四态 | 条数 |
|---|---|
| 有入口 | **70** |
| 部分 | **10** |
| 无入口 | **0** |
| 设计如此（CLI-only） | **4** |
| 合计 | **84** |

**一句话结论**：动作族缺口集中在**删除类**（VM / 容器 / core dump 各 1 条无入口）；
读族**无完全缺失**，但 10 条「只能看一部分」（接口 MAC/NUMA、ACL 规则的动作与命中、VPP 线程清单、
画像汇总、权限 detail、历史版本差异）。

## 3. A. 无入口清单（待补）

### 3.1 `request` 族（3 条）

| CLI 命令 | 族 | 备注 |
|---|---|---|
| `request virtual-machine-functions <name> delete` | request virtual-machine-functions | 虚拟机列表（`#/compute/vms`）与详情（`#/compute/vms/:name`）**都没有删除按钮**；全前端无 `DELETE /virtual-machine-functions/*` 调用（`grep -rn "method: 'DELETE'" internal/api/ui/js/` 的命中只有 images / configuration / login-users / login-banner / capture / snapshots / qos / port-mirroring）。详情页 `#vmd-actions` 只挂 `VM_ACTIONS`（启动/停止/重启，`js/views/vms.js:28-32`）。树里该命令标 `Su`（`tree_oper.go:241`）。 |
| `request container-functions <name> delete` | request container-functions | 同上：容器列表（`#/compute/containers`）与详情（`#/compute/containers/:name`）的 `#ctd-actions` 同样只有启动/停止/重启，无删除（`js/views/containers.js:48-53`、`js/views/containerDetail.js:236-246`）。树里标 `Su`（`tree_oper.go:262`）。 |
| `request system core-dumps delete` | request system | `#/ops/actions` 的「诊断归档」组只有 **列出 core dump**（`#ops-coredumps-btn`，只读）与 **导出清单**（`#ops-export-btn` → `POST /system/core-dumps:export`）；**没有删除入口**，全前端无 `DELETE /system/core-dumps`。树里标 `Su`（`tree_oper.go:371`）。 |

> 三条同族：**S 档删除动作在界面上都没有入口**。界面缺的**不是**端点（端点都在），而是控件。

### 3.2 `show` 族

**0 条**——84 条读命令在界面上都能找到展示位（含 `show system core-dumps` 这种以「按钮 + 输出区」
形态呈现的，见 §5.1）。

## 4. B. 部分覆盖清单（10 条，全在 `show` 族）

| CLI 命令 | 已有 | 缺什么（依据） |
|---|---|---|
| `show interfaces management` | `#/system/interfaces` 列表里**有**该管理口的行（名称/说明/MTU/Rx/Tx/Rx 错误/Tx 丢弃，`index.html:231-234`），详情页头部有**链路/速率/管理状态**（`js/views/system.js:53-58`） | 缺**管理口 IP/地址**——CLI 该命令口径是「管理口（内核侧，IP/链路）」。接口页任何位置都不渲染地址（`Interface` schema 也**没有** address 字段，见 `docs/NFViS-openapi.yaml` 的 `Interface:`），地址只在「配置页 → 系统配置段 JSON」里以原文可见。 |
| `show interfaces <ifname> detail` | `#/system/interfaces/:name` 展示 驱动 / 链路 / 速率 / 管理状态 / 说明 / MTU / 入向·出向限速策略 / 风暴抑制 / 端口安全 / SR-IOV VF 数（`js/views/system.js:53-72`） | 缺 **MAC、队列数、NUMA**（CLI 口径「驱动/MAC/MTU/队列/NUMA」）。契约 `Interface` schema **有** `mac`、`numa_node`（`docs/NFViS-openapi.yaml` 的 `Interface:`），但前端一处未渲染（`grep -n "numa" internal/api/ui/js/views/*.js` 的命中只有 VM 内存 NUMA 与 VPP 配置项）。 |
| `show interfaces physical <ifname> detail` | 同上（同一详情页） | 同上（同一缺口；`physical` 只是等价写法）。 |
| `show acls <name>` | `#/network/acls/:name` 有规则表（`#acd-rule-table`） | 规则表**只有 6 列**：序号 / 方向 / 源 / 目的 / 协议 / 源端口（`index.html:930-931`）。缺 **动作（permit/deny）**——`action` 是 `Acl.rules[].action` 的必填字段（`docs/NFViS-openapi.yaml` 的 `Acl:`），界面不显示它，操作者看不到一条规则是放行还是拒绝。 |
| `show acls <name> detail` | 同上 | 同上（缺动作）**再加**：缺**绑定信息**（CLI 口径「规则与绑定详情」；REST `Acl` schema **无** bindings 字段，界面拿不到）；缺**命中计数** `rules[].hits`（schema 有，界面不渲染）；缺 `destination_port`。 |
| `show vpp threads` | `#/` 总览「数据面」卡有「线程数」一行（`js/views/overview.js:135-162`，取 `vpp.threads.length`） | 缺**线程清单与绑核**（CLI 口径「main/worker 线程清单与绑核」）——卡里只给条数，不给逐线程与绑核；`GET /vpp/status` 已带 `threads[]`，界面未展开。 |
| `show vm-image-profiles` | `#/compute/images/:name`「启动画像」块展示**该镜像**的画像（`js/views/images.js:85-105`） | 缺**汇总列表**：CLI 无参形态一次列出全部键，界面只能逐个镜像进详情页看（`/vm-image-profiles` 只在该详情页路由与 `#/deploy` 的 endpoints 里被消费，见 `routes.json`）。 |
| `show configuration permissions <class>` | `#/system/users` 权限类表每行「生效权限」按钮 → `#usr-perm-out` 摘要（`js/views/system.js:692-714`，`GET /configuration/permissions?class=`） | 只给**摘要**：允许/拒绝**条数** + **前 12 条**允许路径（`USR_PERM_PREVIEW = 12`，`system.js:687`），其余明确指向命令行。 |
| `show configuration permissions <class> detail` | 同上 | 缺**逐路径判定依据**（`detail` 形态）；界面文案自己写明「完整列表与逐路径判定依据见命令行…detail」（`system.js:709-710`）。 |
| `show configuration compare rollback <n>` | `#/config/history` 每行「看差异」按钮（`#cfgh-table` 行动态生成，`js/views/config.js:1735-1738`） | **只给引导、不显示差异**：`cfghDiffGuide` 弹的是说明框（`config.js:1748-1764`），写明服务端**没有**「按历史版本比对」的读取接口，要看差异须先「回滚到这一版」取为候选、再到配置页看（且方向相反）。即：该命令的**输出内容**在界面上取不到。 |

## 5. C. 有入口清单

> 格式：`CLI 命令` ｜ **路由** ｜ 控件 / 展示位（DOM id 或函数）｜ 证据。
> `index.html` 行号按 §0 快照（该文件在快照期内被并发会话插入过 15 行，勿沿用旧号）。

### 5.1 `show` 族 —— 系统与版本（`#/`、`#/system`）

| CLI 命令 | 路由 | 控件 / 展示位 | 证据 |
|---|---|---|---|
| `show version` | `#/`、`#/system` | 组件版本 `#ver-list` / `#sysp-ver` | `js/views/overview.js:50-56,61,87` |
| `show system uptime` | `#/`、`#/system` | 系统卡 `#sys-list` / `#sysp-list`「运行时长」 | `overview.js:36` |
| `show system cpu` | 同上 | 「CPU」「CPU 使用率」 | `overview.js:38-39` |
| `show system memory` | 同上 | 「内存」 | `overview.js:40-42` |
| `show system storage` | 同上 | 「根文件系统」 | `overview.js:43-46` |
| `show system hugepages` | `#/system/pools` | 「大页池」表 `#hp-table`（声明/内核实际/在用/实际持有/数据面占用/无主占用/空闲/可回收/状态）+ 回收按钮 `#hp-reclaim-btn` | `index.html:182-189`、`overview.js:221-240` |
| `show system metrics history` | `#/system/metrics` | 概览 `#mh-store` + 指标选择器 `#mh-metric` + 窗口 `#mh-window` + 折线 `#mh-chart` | `index.html:205-223`、`system.js:357-445` |
| `show system kernel` | `#/system/kernel` | 运行实际 `#knl-actual` / 配置期望 `#knl-desired` / 差异 `#knl-diffs` | `index.html:310-314`、`system.js:566-592` |
| `show system hardware` | `#/system/hardware` | `#hw-list` + 传感器表 `#hw-sensor-table` + 磁盘 SMART `#hw-disk-table` | `index.html:134-144`、`system.js:1237-1266` |
| `show system firewall` | `#/system` | 「主机防火墙」卡：`#fw-list` + 规则表 `#fw-table`（含命中 packets/bytes）+ `#fw-reserved`/`#fw-counters` | `index.html:121-127`、`overview.js:98-128` |
| `show system core-dumps` | `#/ops/actions` | 按钮「列出 core dump」`#ops-coredumps-btn` → 输出区 `#ops-out` | `index.html:1034`、`js/boot.js:329-337` |
| `show system tech-support` | `#/ops/actions` | 「诊断归档（tech-support）」表 `#ops-tech-table`（+ 每行「下载」） | `index.html:1050`、`js/views/ops.js:211-253` |
| `show tech-support` | 同上 | 同上 | 同上 |
| `show system api tokens` | `#/system/users` | 「活动会话 / API Token」表 `#usr-token-table`（+ 行内「吊销」） | `index.html:356-357`、`system.js:805-836` |
| `show system configuration sessions` | `#/config/sessions` | 「编辑锁会话」表 `#cfgs-table` | `config.js:1843-1866`、`routes.json` 的 `#/config/sessions` |
| `show configuration sessions` | 同上 | 同上（同一读物） | 同上 |
| `show users` | `#/system/users` | 用户表 `#usr-table` + 权限类表 `#usr-class-table` | `index.html:335-340`、`system.js:745-800` |
| `show configuration` | `#/config` | 摘要 `#cfg-summary` + 「展开完整配置（committed）」`#cfg-json` | `index.html:444-447`、`config.js:1441-1450` |
| `show configuration candidate` | `#/config` | 编辑态「原始 JSON（整体替换 candidate）」`#cfg-text`（进编辑会话即由 `GET /configuration/candidate` 回填） | `index.html:470`、`config.js:1486-1508`（`cand = await api('/configuration/candidate')` → `cfgWriteText(doc)`） |
| `show configuration history` | `#/config/history` | 提交历史表 `#cfgh-table`（Rev/时间/用户/注释/当前/操作） | `config.js:1712-1745`、`routes.json` 的 `#/config/history` |
| `show configuration permissions <class>` | `#/system/users` | 「生效权限」按钮 → `#usr-perm-out`（**部分**，见 §4） | `system.js:692-714` |
| `show configuration permissions <class> detail` | 同上 | 同上（**部分**，见 §4） | 同上 |
| `show configuration compare rollback <n>` | `#/config/history` | 行内「看差异」（**部分**，见 §4） | `config.js:1735,1748-1764` |
| `show resource-pools` | `#/system/pools` | 资源池卡 `#pools`（页大小/声明/内核实际/已分配/可分配 + 隔离核/VPP 保留核/空闲核/已分配） | `index.html:165`、`overview.js:168-197` |
| `show alarms` | `#/` | 告警卡 `#alarms`（只列未解决） | `index.html:81`、`overview.js:342-355` |
| `show log system` | `#/ops/diagnostics` | 「刷新服务端日志（末 100 行）」`#diag-log-btn` → `#diag-log` | `index.html:1153-1154`、`ops.js:86-97` |
| `show log audit` | `#/ops/audit` | 审计表 `#audit-table`（+ `#audit-refresh-btn`） | `index.html:1116-1117`、`ops.js:133-141` |

### 5.2 `show` 族 —— 接口与数据面

| CLI 命令 | 路由 | 控件 / 展示位 | 证据 |
|---|---|---|---|
| `show interfaces` | `#/system/interfaces` | 接口表 `#iface-table`（名称/说明/MTU/Rx/Tx/Rx 错误/Tx 丢弃） | `index.html:231-234`、`overview.js:298-318` |
| `show interfaces physical` | 同上 | 同上（DPDK 物理口同列） | 同上 |
| `show interfaces <ifname>` | `#/system/interfaces/:name` | 详情页对象头 `#ifd-head` + 配置与运行态 `#ifd-info` | `index.html:243-247`、`system.js:47-80` |
| `show interfaces physical <ifname>` | 同上 | 同上 | 同上 |
| `show interfaces <ifname> statistics` | `#/system/interfaces/:name` | 「收发计数」`#ifd-stat`（Rx/Tx 包、字节、错误、丢弃） | `index.html:250`、`system.js:73-78` |
| `show interfaces physical <ifname> statistics` | 同上 | 同上 | 同上 |
| `show interfaces <ifname> sriov` | `#/system/interfaces/:name` | `#ifd-info` 的「SR-IOV VF 数（配置）」与「VF 占用」（逐 VF + 归属 VNF） | `system.js:68-71` |
| `show interfaces physical <ifname> sriov` | 同上 | 同上 | 同上 |
| `show interfaces <ifname> detail` | 同上 | **部分**（缺 MAC/队列/NUMA，见 §4） | `system.js:53-72` |
| `show interfaces physical <ifname> detail` | 同上 | **部分**（同上） | 同上 |
| `show interfaces management` | `#/system/interfaces` | **部分**（缺 IP，见 §4） | 同上 |
| `show vpp` | `#/` | 「数据面」卡 `#vpp-list`（数据面/版本/连接状态/待重启生效/线程数/内存/buffer 池） | `index.html:76`、`overview.js:135-162` |
| `show vpp buffers` | `#/` | 同上「buffer 池」行（逐 NUMA 用/可用） | `overview.js:158-160` |
| `show vpp memory` | `#/` | 同上「内存」行（used/total） | `overview.js:157` |
| `show vpp threads` | `#/` | **部分**（只线程数，见 §4） | 同上 |
| `show capture` | `#/ops/capture` | 会话态 `#cap-active` + 已导出 pcap 表 `#cap-table`（+ 行内「下载」）+ 控件 `#cap-start-btn`/`#cap-stop-btn`/`#cap-export-btn` | `index.html:1165-1183`、`ops.js:152-189` |
| `show vpp capture` | 同上 | 同上（兼容别名；界面走 `/capture`） | 同上 |
| `show bonds <name>` | `#/network` | 网络对象「链路聚合（bond）」卡 | `network.js:48-50,88-137` |
| `show bonds <name> detail` | `#/network/bonds/:name` | 链路聚合详情 `#bnd-head` + `#bnd-info`（成员/LACP/MTU/链路状态/活动成员数） | `index.html:941-944`、`network.js:213-232` |

### 5.3 `show` 族 —— 网络对象

| CLI 命令 | 路由 | 控件 / 展示位 | 证据 |
|---|---|---|---|
| `show virtual-switches` | `#/network/switches` | 虚拟交换机表 `#vs-table`（名称/类型/成员端口/BD） | `index.html:574`、`switches.js:28-70` |
| `show virtual-switches <name>` | `#/network/switches/:name` | 详情头 `#vsd-head`（类型/成员端口数/BD/DHCP 中继/学习上限） | `index.html:587`、`switches.js:96-136` |
| `show virtual-switches <name> detail` | 同上 | 同上 | 同上 |
| `show virtual-switches <name> ports` | `#/network/switches/:name` | 「成员端口」表 `#vsd-port-table`（端口/来源/管理状态/链路/Rx/Tx） | `index.html:591`、`switches.js:102-145` |
| `show virtual-switches <name> statistics` | 同上 | 同上表的 Rx/Tx 列（+ 列表页 `#vs-stat` 汇总） | `switches.js:56-69,137-145` |
| `show virtual-switches <name> mac-table` | `#/network/switches/:name` | 「拉取 MAC 表」`#vsd-mac-btn` + 条数 `#vsd-mac-limit` + 表 `#vsd-mac-table` | `index.html:600`、`switches.js:163-180` |
| `show virtual-switches <name> dhcp-leases` | `#/network/switches/:name` | 「DHCP 服务器」块 `#vsd-dhcp` + 「拉取租约」`#vsd-dhcp-btn` + 租约表 `#vsd-dhcp-lease-table` | `index.html:614-620`、`switches.js:203-232` |
| `show vrfs <name>` | `#/network` | 网络对象「VRF（L3 虚拟交换机）」卡（名称/L3 接口/路由数） | `network.js:38-44` |
| `show vrfs <name> routes` | `#/network/vrfs/:name` | 「路由表（运行态 FIB）」`#vrd-routes` + 「刷新路由表」`#vrd-routes-btn` | `index.html:916-917`、`network.js:155-182` |
| `show acls <name>` | `#/network` + `#/network/acls/:name` | 网络对象 ACL 卡（名称/规则数）+ 详情页规则表 `#acd-rule-table`（**部分**，见 §4） | `network.js:45,200-211` |
| `show acls <name> detail` | `#/network/acls/:name` | 同上（**部分**，见 §4） | 同上 |
| `show nat` | `#/network` | 网络对象 NAT 卡（地址池/规则/静态映射逐行）+ 「查看 NAT 会话」`#big-nat-btn` → `#big-out` | `index.html:894-900`、`network.js:70-77`、`ops.js:115-129` |
| `show port-mirroring` | `#/network` + `#/network/span/:name` | SPAN 卡（名称/源/目的）+ 详情 `#spd-head`/`#spd-info` | `network.js:55-57,300-319` |
| `show dns proxy` | `#/system` | 系统卡「数据面 DNS 代理 / 全局上游 / 按域上游（/ 运行态）」 | `overview.js:70-85` |
| `show qos policies` | `#/network` + `#/network/qos/:name` | QoS 卡（名称/CIR/绑定 接口:in|out）+ 详情 `#qsd-head`/`#qsd-info` | `network.js:54,234-251` |
| `show vxlan tunnels` | `#/network` | 网络对象「VXLAN 隧道」卡（名称/VNI/本地→远端/端口/交换机/状态） | `network.js:58-66,82-86` |
| `show lldp neighbors` | `#/network/lldp` | 「邻居（运行态）」表 `#lldp-nbr-table`（本地接口/对端标识/对端端口/TTL） | `index.html:642`、`lldp.js:52-54` |
| `show protocols lldp neighbors` | 同上 | 同上（同一读物） | 同上 |

> `show lldp neighbors interface <ifname>` 的过滤形态：本表已含 `local_interface` 列，页面**无过滤控件**，
> 靠该列自行筛（参数形态记入父语句 `show lldp neighbors`）。

### 5.4 `show` 族 —— 计算域

| CLI 命令 | 路由 | 控件 / 展示位 | 证据 |
|---|---|---|---|
| `show virtual-machine-functions` | `#/compute/vms` | 虚拟机表 `#vm-table`（名称/状态/vCPU/内存/镜像/操作） | `index.html:490`、`vms.js:34-61` |
| `show virtual-machine-functions <name>` | `#/compute/vms/:name` | 详情头 `#vmd-head` + 资源 `#vmd-info` | `index.html:502-514`、`vmDetail.js:57-86,137-138` |
| `show virtual-machine-functions <name> detail` | 同上 | 同上（域摘要/资源/绑定核/内存后端/磁盘/vNIC/PCI/串口/自启） | `vmDetail.js:66-86` |
| `show virtual-machine-functions <name> interfaces` | `#/compute/vms/:name` | 「接口」Tab `#vm-tab-ifaces` → `#vmd-iface-table` | `index.html:522-524`、`vmDetail.js:102-123` |
| `show virtual-machine-functions <name> statistics` | `#/compute/vms/:name` | 概览 Tab「vhost-user 口计数」`#vmd-stat` | `index.html:519`、`vms.js:92-108` |
| `show virtual-machine-functions <name> snapshots` | `#/compute/vms/:name` | 「快照」Tab `#vm-tab-snapshots` → `#vm-snap-table` | `index.html:531-538`、`vmDetail.js:176-216` |
| `show container-functions` | `#/compute/containers` | 容器表 `#ct-table` | `index.html:656`、`containers.js:28-57` |
| `show container-functions <name>` | `#/compute/containers/:name` | 详情头 `#ctd-head`（状态/重启次数/镜像/vCPU/内存） | `index.html:668`、`containerDetail.js:203-213` |
| `show container-functions <name> detail` | 同上 | 「配置」`#ctd-info`（命令/参数/环境变量/重启策略/自启/vNIC 数） | `index.html:680`、`containerDetail.js:221-234` |
| `show container-functions <name> interfaces` | 同上 | 「vNIC」表 `#ctd-iface-table` | `index.html:685`、`containerDetail.js:248-265` |
| `show images <name>` | `#/compute/images` + `#/compute/images/:name` | 镜像表 `#img-table`（名称/类型/大小/引用/状态）+ 详情 `#imd-head`/`#imd-info` | `index.html:743,790-793`、`images.js:25-83` |
| `show images <name> detail` | `#/compute/images/:name` | `#imd-info`（格式/sha256/引用计数/导入状态与时间/归档内嵌 tag） | `images.js:67-81` |
| `show vm-image-profiles` | `#/compute/images/:name` | 「启动画像」`#imd-profile`（**部分**：只能逐个镜像看，见 §4） | `index.html:795`、`images.js:85-105` |

### 5.5 `request` 族 —— 计算域与镜像（有入口 15 条）

| CLI 命令 | 路由 | 控件 / 展示位 | 证据 |
|---|---|---|---|
| `request virtual-machine-functions <name> start` | `#/compute/vms`、`#/compute/vms/:name` | 列表行内「启动」/ 详情 `#vmd-actions` | `vms.js:28-32,52-56`、`vmDetail.js:89-99` |
| `request virtual-machine-functions <name> stop` | 同上 | 「停止」 | 同上 |
| `request virtual-machine-functions <name> restart` | 同上 | 「重启」 | 同上 |
| `request virtual-machine-functions <name> console` | `#/compute/vms/:name` | 「串口」Tab `#vm-tab-console` → 「连接串口」`#vm-console-connect`（+ 输入 `#vm-console-in`/发送/断开） | `index.html:546-562`、`vmDetail.js:341-381` |
| `request virtual-machine-functions <name> snapshot create` | `#/compute/vms/:name` | 「快照」Tab → 名字输入 `#vm-snap-new` + 「创建快照」`#vm-snap-create` | `index.html:533-534`、`vmDetail.js:257-278` |
| `request virtual-machine-functions <name> snapshot rollback` | 同上 | 快照表行内「回滚」 | `vmDetail.js:207-208,218-255` |
| `request virtual-machine-functions <name> snapshot delete` | 同上 | 快照表行内「删除」 | 同上 |
| `request container-functions <name> start` | `#/compute/containers`、`#/compute/containers/:name` | 列表行内「启动」/ 详情 `#ctd-actions` | `containers.js:48-53`、`containerDetail.js:236-246` |
| `request container-functions <name> stop` | 同上 | 「停止」 | 同上 |
| `request container-functions <name> restart` | 同上 | 「重启」 | 同上 |
| `request container-functions <name> log` | `#/compute/containers/:name` | 「日志」Tab → `#ct-logs`（+ `#ct-logs-refresh`，进 Tab 自动拉） | `index.html:693-694`、`containerDetail.js:294-315` |
| `request container-functions <name> exec <command>` | `#/compute/containers/:name` | 「执行命令」Tab → `#ct-exec-cmd` + `#ct-exec-timeout` + 「执行」`#ct-exec-run` | `index.html:701-706`、`containerDetail.js:166-194` |
| `request container-functions <name> shell` | `#/compute/containers/:name` | 「交互终端」Tab → 「连接容器终端」`#ct-shell-connect` | `index.html:718`、`containerDetail.js:89-118` |
| `request images download` | `#/compute/images` | 「方式 A：URL 拉取」`#img-url`+`#img-sha`+「拉取并导入」`#img-url-btn` | `index.html:762-765`、`images.js:173-198` |
| `request images upload` | `#/compute/images` | 「方式 B：incoming 文件」`#img-incoming-btn`；「方式 C：本地上传」`#img-file-btn` | `index.html:771-777`、`images.js:200-254` |
| `request images delete` | `#/compute/images` | 镜像表行内「删除」（`wbtn` 带 `data-write`，super-user 级） | `images.js:47-49,142-164` |

### 5.6 `request` 族 —— 接口 / 数据面 / 抓包 / 系统（有入口 32 条）

| CLI 命令 | 路由 | 控件 / 展示位 | 证据 |
|---|---|---|---|
| `request interfaces <ifname> bind-dpdk` | `#/system/interfaces/:name` | 「绑定到 DPDK」`#ifd-dpdk-bind`（+ 驱动 `#ifd-uio`） | `index.html:261-265`、`system.js:230-266` |
| `request interfaces <ifname> unbind-dpdk` | 同上 | 「解绑交还内核」`#ifd-dpdk-unbind`（+ `#ifd-todrv`） | `index.html:271`、`system.js:230-266` |
| `request sriov create-vfs <ifname> count <n>` | `#/system/interfaces/:name` | 「SR-IOV（VF 数量）」`#ifd-vfs` + 「设置 VF 数量」`#ifd-sriov-btn`（同一控件，0=回收） | `index.html:277-278`、`system.js:269-296` |
| `request sriov delete-vfs <ifname> vf <n>` | 同上 | 同上（`vf_count: 0`） | 同上 |
| `request vpp restart` | `#/ops/actions` | 「重启数据面（VPP）」`#ops-vpprestart-btn` → `POST /vpp/restart` | `index.html:1102`、`boot.js:355-365` |
| `request capture start` | `#/ops/capture` | 「开始抓包」`#cap-start-btn`（+ `#cap-iface`/`#cap-count`） | `index.html:1165-1169`、`boot.js:261-274` |
| `request capture stop` | `#/ops/capture` | 「停止（保留数据）」`#cap-stop-btn` | `index.html:1170`、`boot.js:275-283` |
| `request capture export` | `#/ops/capture` | 「停止并导出 pcap」`#cap-export-btn` + 导出表行内「下载」 | `index.html:1171`、`boot.js:284-293`、`ops.js:177-188` |
| `request vpp trace start` | `#/ops/capture` | 同 `request capture start`（兼容别名；界面走 `/capture`） | `boot.js:270`、`cli_rest_coverage_test.go` 别名注 |
| `request vpp trace stop` | `#/ops/capture` | 同 `request capture stop` | `boot.js:280` |
| `request vpp trace export` | `#/ops/capture` | 同 `request capture export` | `boot.js:289` |
| `request system kernel apply` | `#/system/kernel` | 「写入基线（按配置期望）」`#knl-apply-btn` | `index.html:317`、`system.js:594-625` |
| `request system kernel rollback` | `#/system/kernel` | 「回退上一次基线」`#knl-rollback-btn` | `index.html:318`、同上 |
| `request system hugepages reclaim` | `#/system/pools` | 「回收空闲的多余大页」`#hp-reclaim-btn` | `index.html:188`、`overview.js:262-288` |
| `request system software add <deb>` | `#/ops/actions` | 「软件版本」`#ops-sw-pkg`+`#ops-sw-sha`+「安装升级包」`#ops-sw-add-btn` | `index.html:1058-1062`、`ops.js:292-323` |
| `request system software rollback` | `#/ops/actions` | 「回退到上一版本」`#ops-sw-rollback-btn` | `index.html:1063`、`ops.js:325-348` |
| `request system reboot` | `#/ops/actions` | 「重启主机」`#ops-reboot-btn` → `POST /system:reboot` | `index.html:1103`、`boot.js:366-375` |
| `request system shutdown` | `#/ops/actions` | 「关机」`#ops-shutdown-btn` → `POST /system:shutdown` | `index.html:1104`、`boot.js:376-385` |
| `request system poweroff` | `#/ops/actions` | 同一「关机」按钮：CLI 的 `shutdown`/`poweroff` 走**同一实现**（`internal/api/cli_compute_oper.go:1082` → `x.sw.Shutdown()` → `systemctl poweroff`，`internal/system/software.go:262-266`），REST 同为 `POST /system:shutdown`（openapi 该路由 summary 即「关机（或 poweroff）」）——界面上无可区分项，也无需区分 | 同上 |
| `request system configuration backup` | `#/ops/actions` | 「生成配置备份」`#ops-backup-btn` + 归档表 `#ops-backup-table`（每行「下载」） | `index.html:1020,1045`、`boot.js:307-314`、`ops.js:211-253` |
| `request system configuration restore <path>` | `#/ops/actions` | 「从本机备份归档恢复」选择框 `#ops-restore-file` + 「从所选归档恢复配置」`#ops-restore-btn` | `index.html:1073-1076`、`ops.js:356-391` |
| `request system tech-support generate` | `#/ops/actions` | 「生成 tech-support 归档」`#ops-techsupport-btn` | `index.html:1033`、`boot.js:324-328` |
| `request system core-dumps export` | `#/ops/actions` | 「core dump 清单导出到」`#ops-export-url` + 「导出清单」`#ops-export-btn` | `index.html:1038-1039`、`boot.js:343-354` |
| `request system zeroize` | `#/ops/actions` | 「恢复出厂（清空配置与镜像）」`#ops-zeroize-btn` | `index.html:1084`、`ops.js:398-426` |
| `request system api tls regenerate` | `#/system/tls` | 「重签自签证书」`#tls-regen-btn` | `index.html:426`、`system.js:1183-1207` |
| `request system api token revoke <token-id>` | `#/system/users` | 会话表行内「吊销」（`wbtn` 动态写入口） | `system.js:841-867`（`usrTokenRevoke`） |
| `request system ssh host-key regenerate` | `#/ops/actions` | 「重新生成 SSH host key」`#ops-sshkey-btn` | `index.html:1021`、`boot.js:315-323` |
| `request system password change` | `#/system/users`、顶栏 | 「修改我的口令」`#usr-mypw-btn`；顶栏「我的账号」`#account-btn` → `#acct` 对话框 | `index.html:44,393`、`system.js:988-1013`、`boot.js:414-435` |
| `request system storage format-data` | `#/ops/actions` | 「重置数据分区（保留管理面可达）」`#ops-formatdata-btn` | `index.html:1092`、`ops.js:434-467` |
| `request system ntp sync` | `#/ops/actions` | 「立即同步时间」`#ops-ntp-btn` | `index.html:1028`、`boot.js:297-306` |
| `request alarms clear` | `#/ops/actions` | 「清除已恢复告警」`#ops-alarms-btn` → `POST /alarms:clear` | `index.html:1035`、`boot.js:338-342` |

## 6. 设计如此（CLI-only）—— 本两族中的 4 条

| CLI 命令 | 族 | 理由（对照 `cliRESTExceptions`） | Web 等价形态 |
|---|---|---|---|
| `show vpp runtime` | show vpp | 决策 #200：数据源是本机 VPP stats segment（`vpp_get_stats`），不经 HTTP 端点 | `#/` 总览「数据面」卡（`GET /vpp/status`：版本/线程数/内存/buffer） |
| `show vpp runtime thread <id>` | show vpp | 同上（`thread` 只是本机过滤参数） | 同上 |
| `show log vnf <name>` | show log | 指引型命令（指向容器日志 / VM 串口），等价物已存在 | 容器详情「日志」Tab（`#/compute/containers/:name`）；VM 详情「串口」Tab（`#/compute/vms/:name`） |
| `show log vnf <name> last <n>` | show log | 同上（同一命令的参数形态） | 同上 |

> 附注（不属本两族，但同一例外表相关）：例外表里 `monitor vnf <name>` / `monitor interfaces <ifname>` 的
> 「等价物＝定时刷新 + `/events` 推送」在本快照里已**落地为独立页面** `#/ops/monitor`
> （新增文件 `js/views/monitor.js` + `routes.json` 新路由；并发会话正在加），底座是
> `js/core/state.js` 的 `/events` SSE + 断线兜底轮询两条既有机制。

## 7. 局限、可能误判处与复核方法

1. **保守/宽松的分界**（本次采用）：
   - 只把**功能不可达**或**输出内容取不到**记为「无入口/部分」。功能可达但路径不同（如
     `request system configuration restore` 只能从本机备份归档里选、CLI 可给任意路径）**记有入口**并在
     §5.6 备注——这类「同一功能、入口收窄」的项不逐条计为部分，否则清单会被 UX 差异淹没。
   - 唯一破例：`request interfaces <ifname> enable|disable` 记**部分**——因为界面上的等价控件
     （`#/config` 接口节该口的「启用」下拉，`js/views/config.js:178-189`）走的是**候选 → 手工提交**，
     而该命令的语义是**取锁改候选并立即提交**（`internal/api/cli_iface_sriov_ops.go:65-95` 的
     `requestInterfaces` 调 `commitMutate`）。若评审认为「多一步提交」不算差异，可把它移到「有入口」
     （动作族部分态即归零，无入口仍为 3）。
2. **静态判定，未真机点击**：本报告基于 DOM 与 JS 接线（控件存在 + 绑定端点 + 展示位存在），
   **没有**在真机上点过每个控件。可能误判的情形：控件被 `body.role-*` 隐藏（那属于权限门禁，不是缺入口）、
   控件在某个数据形态下不渲染（如 `#vpp-list` 的行按字段有无动态生成——`show vpp threads` 的「线程数」
   在 `threads` 字段缺席时不显示）。复核时以**登录后逐页点一遍 + 对照 `vppctl`/REST 原始响应**为准。
3. **读族「部分」的口径敏感**：§4 的 10 条里，`show interfaces <ifname> detail`（缺 MAC/NUMA/队列）、
   `show vpp threads`（缺逐线程清单）这类，判据是「CLI 输出里有、界面不渲染，且契约字段存在」。
   若产品认为这些字段不需要在界面呈现，则应把它们降级为「有入口（有意不展示）」，并在契约里如实标注
   （参考决策 #344 的「契约如实化」做法）。**建议逐条走一次「契约字段 → 界面渲染」对账**，
   可复用 `internal/api/ui/ui_qos_card_fields_test.go` 的字段级守护思路（该守护正是为
   `show qos policies` 卡片的字段漂移而加的）。
4. **两处数字不一致（顺带发现，值得单独跟进）**：`show system metrics history` 是**真实实现**的命令
   （`internal/api/cli_system_show.go:71`、`internal/api/cliexec.go:145` 的渲染器、树里在
   `tree_oper.go:53-61`），但**既不在** `cliRESTCoverage`（`internal/api/cli_rest_coverage_test.go`）
   **也不在** `contractCLICommands`（`internal/api/cli_contract_coverage_test.go:31`）——
   即端点级覆盖矩阵（146 条）里没有它，守护表也没强制它归类。这与 `AGENTS.md` 的走查纪律
   （「同一事实存在两个视图时必须并排读」）同族：**树里有、守护表里没有**，属于守护覆盖面的空洞，
   建议补进两张表之一（或明确登记为不需要端点）。
5. **基线会漂移**：本快照的工作树正被并发会话修改（新增 `#/ops/monitor`、端口安全写入口）。
   复核时请先 `git status` 确认工作树状态，再按 §1.2 的规则重跑枚举（或按 §5 的 id 逐个
   `grep -n` 校验），不要直接沿用本报告的行号。
6. **可复现的复核命令**（示例，全部只读）：

   ```bash
   # ① 命令清单（树）——确认两族条数
   grep -n '"show"\|"request"' internal/schema/tree_oper.go | head
   # ② 路由与页面端点
   python -c "import json,io;d=json.load(io.open('internal/api/ui/routes.json',encoding='utf-8'));[print(r['path'],r['view'],r['endpoints']) for r in d['routes']]"
   # ③ 某条命令的展示位是否还在（示例：大页池 + 回收按钮）
   grep -n 'hp-table\|hp-reclaim-btn' internal/api/ui/index.html
   # ④ 写入口是否还在（示例：VM 删除——预期零命中）
   grep -rn "method: 'DELETE'" internal/api/ui/js/ | grep -i "virtual-machine-functions"
   # ⑤ 端点级与界面级的差：矩阵只证明「调不调端点」
   grep -n "show system metrics history" docs/v3-Web-CLI覆盖矩阵.md   # 预期零命中（见 §7.4）
   ```
