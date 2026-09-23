# CLI ⇄ REST 覆盖核查（Web 控制台“覆盖 CLI 全部功能”的前置核查）

| 文档属性 | 内容 |
|---|---|
| 用途 | 回答“Web 控制台能否实现 CLI 的全部功能”：把 258 个 CLI 命令形态逐一对照 91 个 REST 端点，分出**已有类型化端点 / 需补 API / CLI-only by design** 三张清单 |
| 日期 | 2026-09-22（round42） |
| 依据 | `docs/NFViS-CLI命令全表.md`（命令契约 + 实测状态）、`docs/NFViS-openapi.yaml`（91 个路径）、规格书 §12 与决策 #115/#107/#92/#76⑧/#34 |
| 机器守护 | `internal/api/cli_rest_coverage_test.go`（映射表 + 例外表 + 缺口表三张，端点改名/删改即红；新增契约命令忘记归类即红） |

## 0. 结论

**258 个命令形态中：236 个已有类型化 REST 端点（可直接写页面）、3 个是真缺口（需补 API）、19 个是 CLI-only by design（交互形态差异，不需要 API）。**

> 更新记录：缺口 #1（`show configuration` committed 全量读取）已于 **round43 收口**（新增 `GET /configuration`，决策 #119）——覆盖 225→226、缺口 14→13；缺口 #2/#3/#4/#7（日志 / ping / traceroute / 清零统计）已于 **round47 收口**（决策 #123），缺口 #10（`commit check` 预校验）已于 **round46 收口**（决策 #122）——覆盖 226→**231**、缺口 13→**8**；缺口 #5/#6（VS/VM 详情的 statistics 响应字段）已于 **round49 收口**（决策 #124）——覆盖 **231→233**、缺口 **8→6**；缺口 #8（`ssh host-key regenerate`）已于 **round49 收口**（决策 #125）——覆盖 **233→234**、缺口 **6→5**；缺口 #9（`core-dumps export`）已于 **round49 收口**（决策 #126，**并修掉 CLI 侧的假成功**）——覆盖 **234→235**、缺口 **5→4**；缺口 #11（`load merge`）已于 **round49 收口**（决策 #127，`X-NFVIS-Merge: true`）——覆盖 **235→236**、缺口 **4→3**。**本表之外**：命令全表新增 `show configuration history`（决策 #142 的契约前置——此前只有 `rollback [n]` 与 `compare rollback <n>`，**没有「列出历史快照」的读写物**），端点 `GET /configuration/history` 同步落地——**形态 258→259、覆盖 236→237**（新增命令与端点均已进 `cli_rest_coverage_test.go` 的归类表）。下表保留原始条目并标注状态，便于追溯。

配置模式的 113 条 `set`/`delete` 语句是最大的一块，**一整轮 candidate API 全覆盖**（`GET/PUT/DELETE /configuration/candidate` + `X-NFVIS-Auto-Commit`）；show 族 65 条里 58 条有对应 GET；request 族 46 条里 42 条有对应动作端点。真正的缺口集中在三类：**整配置读取、日志、连通性诊断（ping/traceroute）**——前者已于 round43 收口，后两者决策 #115 已列入后续增量。

## 1. 口径与方法

- **命令形态数**（命令全表 §3 口径：同一命令的二级子命令各计一行）：**258**。按实际行数复核：show 65、request 46、其余操作 10、通用管道 9、配置模式 128。
  - ⚠️ **附带发现（文档统计错误）**：命令全表 §3 统计表的分类数字与实际行数不符——show 实为 **65**（表写 72）、其余操作实为 **10**（表写 9）、配置模式实为 **128**（表写 129，其中 `system` 实为 34、`container-functions` 实为 10）、"共 256 行"实为 **258** 行。合计 258 正确。本轮只登记不改（改它会动发布件随包文档，留作下一次文档轮次）。
- **分类定义**：
  - **A 覆盖**：存在承载该命令功能的类型化 REST 端点（`/cli/execute` 与 `/cli/candidates` 是 `x-internal`，按 round37 口径**不计入**覆盖）。
  - **B 缺口**：CLI 有（且多数已实测通过），REST 没有对应端点或响应缺字段。
  - **C 例外**：CLI-only by design——REPL 交互形态（导航/补全/持续跟踪/问答向导）或 CLI 侧渲染（管道），Web 的等价物是表单、定时刷新、原生 JSON，不需要 API。
- **方法**：逐族对照命令全表与 openapi.yaml 的路径/方法/响应 schema；响应缺字段的（如 VS 统计）以契约 schema 为准绳核实。

## 2. A 覆盖矩阵（236 个形态）

### 2.1 show 族（61/65）

| 命令族 | REST 端点 |
|---|---|
| `show version` | `GET /system/version` |
| `show system uptime\|cpu\|memory\|storage` | `GET /system/status`（R37-1 收口后同源字段） |
| `show system hugepages` | `GET /system/status` + `GET /resource-pools` |
| `show system kernel` | `GET /system/kernel` |
| `show system hardware` | `GET /system/hardware` |
| `show system core-dumps` / `tech-support` | `GET /system/core-dumps` / `GET /system/tech-support` |
| `show system configuration sessions`（含等价写法） | `GET /system/configuration/sessions` |
| `show interfaces`（含 `physical`/`management`） | `GET /interfaces` |
| `show interfaces <ifname> detail\|statistics` | `GET /interfaces/{name}` |
| `show interfaces <ifname> sriov` | `GET /interfaces/{name}`（详情含 sriov 字段） |
| `show virtual-switches`（detail/ports/mac-table/statistics） | `GET /virtual-switches`、`/{name}`、`/{name}/ports`、`/{name}/mac-table`；**statistics 为详情响应的字段**（round49 收口，决策 #124） |
| `show vrfs`（detail/routes） | `GET /vrfs`、`/{name}`、`/{name}/routes` |
| `show acls`（detail） | `GET /acls`、`/{name}` |
| `show nat` | `GET /nat`（会话计数另有 `GET /nat/sessions`） |
| `show port-mirroring` / `show qos policies` | `GET /port-mirroring` / `GET /qos/policies` |
| `show vpp`（threads/buffers/memory） | `GET /vpp/status` |
| `show vpp capture` | `GET /vpp/capture`（导出文件 `GET /vpp/capture/{file}`） |
| `show bonds`（detail） | `GET /bonds`、`/{name}` |
| `show lldp neighbors`（含 `protocols lldp` 写法） | `GET /protocols/lldp/neighbors` |
| `show virtual-machine-functions`（detail/interfaces/snapshots/statistics） | `GET /virtual-machine-functions`、`/{name}`、`/{name}/snapshots`；**statistics 为详情响应的字段**（round49 收口，决策 #124） |
| `show container-functions`（detail/interfaces） | `GET /container-functions`、`/{name}` |
| `show images`（detail） | `GET /images`、`/{name}` |
| `show resource-pools` / `show alarms` / `show users` | `GET /resource-pools` / `GET /alarms` / `GET /system/login-users` |
| `show log audit` | `GET /audit-logs` |
| `show log system [level] [last]` | `GET /system/logs`（`text/plain`、`?last=<n>`；round47 收口） |
| `show configuration`（committed 全量） | `GET /configuration`（`{configuration, revision}`，round43 收口） |
| `show configuration candidate` | `GET /configuration/candidate` |
| `show configuration compare rollback <n>` | `GET /configuration/diff`（配合 `POST /configuration/rollback/{n}` 的两步组合） |
| `show tech-support`（顶级等价写法） | `GET /system/tech-support` |

### 2.2 request 族（44/46）

| 命令族 | REST 端点 |
|---|---|
| `request virtual-machine-functions <n> start\|stop\|restart\|delete` | `POST /virtual-machine-functions/{name}:start\|:stop\|:restart`、`DELETE /virtual-machine-functions/{name}` |
| `request … console` | `POST /virtual-machine-functions/{name}/console`（一次性 ticket）+ `GET /console/ws` |
| `request … snapshot create\|rollback\|delete` | `POST /{name}/snapshots`、`POST /{name}/snapshots/{snapshot}:rollback`、`DELETE /{name}/snapshots/{snapshot}` |
| `request container-functions <n> start\|stop\|restart\|log\|delete` | `POST …:start\|:stop\|:restart`、`GET …/logs`、`DELETE …/{name}` |
| `request images upload\|download\|delete` | `POST /images`（multipart / URL / incoming 三模式）、`DELETE /images/{name}` |
| `request interfaces <n> enable\|disable` | `PUT /interfaces/{name}`（`enabled` 字段） |
| `request interfaces <n> bind-dpdk\|unbind-dpdk` | `PUT /interfaces/{name}/dpdk`（body `bound` 布尔：true=绑定、false=解绑；`confirm=true`） |
| `request sriov create-vfs\|delete-vfs` | `PUT /interfaces/{name}/sriov`（设 VF 数量即创建/回收） |
| `request vpp restart` | `POST /vpp/restart` |
| `request vpp trace start\|stop\|export` | `POST/DELETE /vpp/capture`、`GET /vpp/capture/{file}` |
| `request system software add\|rollback` | `POST /system/software`、`POST /system/software:rollback` |
| `request system reboot\|shutdown\|poweroff` | `POST /system:reboot`、`POST /system:shutdown`（契约注明含 poweroff） |
| `request system kernel apply\|rollback` | `POST /system/kernel:apply\|:rollback` |
| `request system configuration backup\|restore` | `POST /system/backup`、`POST /system/restore`（+ `GET /system/backup/{file}`） |
| `request system tech-support generate` / `core-dumps delete` | `POST /system/tech-support` / `DELETE /system/core-dumps` |
| `request system core-dumps export <url>` | `POST /system/core-dumps:export`（清单 JSON；round49 收口，决策 #126） |
| `request system zeroize` / `api tls regenerate` | `POST /system:zeroize` / `POST /system/tls:regenerate` |
| `request system ssh host-key regenerate` | `POST /system/ssh-host-key:regenerate`（round49 收口，决策 #125） |
| `request system password change` | `POST /system/login-users/{name}:change-password` |
| `request system ntp sync` / `alarms clear` | `POST /system/ntp:sync` / `POST /alarms:clear` |

### 2.3 其余操作与管道（7/19）

| 命令 | REST 落点 |
|---|---|
| `configure`（进入配置模式） | 配置事务端点族（§2.4） |
| `\| compare` / `\| compare rollback <n>` | `GET /configuration/diff`（差异数据；渲染形态见例外） |
| `ping <host> [source] [count] [vrf]` | `POST /diagnostics/ping`（**未通即 502**；round47 收口） |
| `traceroute <host> [vrf]` | `POST /diagnostics/traceroute`（round47 收口） |
| `clear interfaces statistics [<ifname>]` | `POST /interfaces:clear-statistics`（204；round47 收口） |
| `commit check` | `POST /configuration/check`（不落库不下发；round46 收口） |

### 2.4 配置模式（124/128）

| 命令族 | REST 落点 |
|---|---|
| `set <path> …` / `delete <path> …`（**113 条语句全族**） | `PUT /configuration/candidate`（`X-NFVIS-Auto-Commit: true` 时校验+下发+落库一次完成）；删除走 candidate 上的键删除 |
| `show`（candidate 当前层级） | `GET /configuration/candidate` |
| `commit check`（仅校验） | `POST /configuration/check`（round46 收口；与 `commit` 同一份校验器） |
| `commit` / `commit confirmed [min]` / `commit and-quit` | `POST /configuration/commit`（`confirmed_minutes>0` 即 confirmed）/ `POST /configuration/commit:confirm` |
| `rollback [n]` | `POST /configuration/rollback/{n}` |
| `discard` | `DELETE /configuration/candidate` |
| `load override <path>` | `PUT /configuration/candidate`（override 语义，缺省） |
| `load merge <path>` | `PUT /configuration/candidate` + `X-NFVIS-Merge: true`（round49 收口，决策 #127） |
| `save <path>` | `GET /configuration/candidate`（取 JSON 自行落盘；配置归档另有 `POST /system/backup`） |

## 3. B 缺口清单（**3 个形态**，需补 API；另 10 个已于 round46/47/49 收口，保留原行便于追溯）

按价值排序；**补法一律契约先行**（openapi + 附录 A 决策 + routes/shape 守护），不碰 `/cli/execute`。

| # | 命令 | CLI 实测 | REST 现状 | 价值 / 建议归属 |
|---|---|---|---|---|
| 1 | ~~`show configuration`（committed 全量读取）~~ | ✅ | ✅ **已收口（round43，决策 #119）**：`GET /configuration` 返回 `{configuration, revision}`，`ClassReadOnly` + 脱敏 | 已解决 |
| 2 | ~~`show log system [level] [last]`~~ | ✅ | ✅ **已收口（round47，决策 #123）**：`GET /system/logs`（`text/plain`、`?last=<n>`，与 `show log system` 同源） | 已解决 |
| 3 | ~~`ping <host> [source] [count] [vrf]`~~ | ✅ | ✅ **已收口（round47，决策 #123）**：`POST /diagnostics/ping`（**未通即 502**，失败带原始回显） | 已解决 |
| 4 | ~~`traceroute <host> [vrf]`~~ | ✅ | ✅ **已收口（round47，决策 #123）**：`POST /diagnostics/traceroute` | 已解决 |
| 5 | ~~`show virtual-switches <name> statistics`~~ | ✅ | ✅ **已收口（round49，决策 #124）**：`GET /virtual-switches/{name}` 附带 `statistics`（`{bd_id, ports:[…]}`，与 CLI 同源） | 已解决 |
| 6 | ~~`show virtual-machine-functions <name> statistics`~~ | ✅ | ✅ **已收口（round49，决策 #124）**：`GET /virtual-machine-functions/{name}` 附带 `statistics`（只含 vhost-user vNIC） | 已解决 |
| 7 | ~~`clear interfaces statistics [<ifname>]`~~ | ✅ | ✅ **已收口（round47，决策 #123）**：`POST /interfaces:clear-statistics`（204） | 已解决 |
| 8 | ~~`request system ssh host-key regenerate`~~ | ✅ | ✅ **已收口（round49，决策 #125）**：`POST /system/ssh-host-key:regenerate`（与 CLI 同一实现、审计同码、失败 500 不谎报） | 已解决 |
| 9 | ~~`request system core-dumps export <url>`~~ | ✅ | ✅ **已收口（round49，决策 #126）**：`POST /system/core-dumps:export`（清单 JSON POST；**同时修掉 CLI 侧只打印「已受理」的假成功**） | 已解决 |
| 10 | ~~`commit check`（仅校验不下发）~~ | ✅ | ✅ **已收口（round46，决策 #122）**：`POST /configuration/check`（同一份校验、不落库不下发） | 已解决 |
| 11 | ~~`load merge <path>`（增量合并）~~ | ✅ | ✅ **已收口（round49，决策 #127）**：`PUT /configuration/candidate` + `X-NFVIS-Merge: true`（回显为合并后的 candidate） | 已解决 |
| 12 | `request system api token revoke <token-id>` | ⚠️ V1 明确延期（决策 #76⑧） | 仅 `DELETE /login`（当前会话） | 低——已登记；逐 token 吊销随 V2 |
| 13 | `request system storage format-data` | 🚫 V1 有意延期（破坏性） | 无 | 低——已登记 |
| 14 | `show vpp runtime [thread <id>]` | ⚠️ 两边都未接入（附录 A #34） | 无 | 低——补它等于补 CLI 自己也没做的能力 |

## 4. C CLI-only by design（19 个形态，不需要 API）

| 命令 | 理由 |
|---|---|
| `wizard` | 决策 #107：CLI 端交互编排，明确无 API 端点（Web 等价物是向导式页面，属增量 2+ 的交互设计） |
| `monitor interfaces <ifname>` / `monitor vnf <name>` | 决策 #92：CLI 轮询形态；Web 等价物是视图定时刷新 + `GET /events` 推送（增量 1 已实现该模式） |
| `start shell` | 本地控制台 shell，Web 无对应形态 |
| `exit` / `quit` | CLI 本地行为 |
| `help [command]`、`?`、Tab 补全 | REPL 帮助/补全；Web 用表单与静态候选（`/cli/candidates` 为 x-internal，按 round37 口径前端不碰） |
| `edit <path>` / `up` / `top` | 配置模式层级导航；数据操作已被 candidate API 覆盖 |
| `annotate <path> "text"` | REPL 注释便利 |
| `run <oper-command>` | 配置模式内执行便利；被运行的命令本身都有端点 |
| `show log vnf <name>` | 指引型命令（指向容器 log / VM console），等价物已存在 |
| `\| match/except/count/last/begin` | CLI 文本过滤；Web 以前端过滤 + 分页查询参数（`limit`/`offset`，FR-API-007）实现 |
| `\| display json` / `\| display xml` | CLI 渲染；Web 原生消费 JSON |
| `show \| display set` | CLI 文本渲染；契约已登记未实现（决策 #84④） |

## 5. 机器守护（`internal/api/cli_rest_coverage_test.go`）

三张表 + 两条断言，沿用仓库既有的 `contractCLICommands`/`deferred` 模式：

1. **`cliRESTCoverage`**（命令形态 → `METHOD /path`）：覆盖矩阵的机器可读形式；配置语句用 `set `/`delete ` 前缀规则整体归入 candidate API（架构性覆盖，无需逐条）。
2. **`cliRESTExceptions`**（CLI-only by design → 理由）。
3. **`cliRESTGaps`**（缺口 → 理由）：缺口因此**机器可见**，补一个划掉一个。
4. 断言 A：覆盖表声明的每个端点必须真实存在于契约（端点改名/删除 → 红，防映射过期）。
5. 断言 B：`contractCLICommands`（既有契约命令清单）里每条都必须落在三张表之一——**新增契约命令忘记归类即红**。

纪律（写入测试头部注释）：新增 CLI 命令须同步补命令全表、`contractCLICommands` 与本覆盖表/例外/缺口表；本测试只认这三张表，文档策展的完整 258 形态清单以命令全表为准。

## 6. 对增量 2 的输入

- 增量 2（配置读写）的 API 侧**已齐备**：candidate/commit/confirm/diff/rollback 全部在位，113 条配置语句无需新端点；**唯一阻塞项（缺口 #1，committed 全量读取）已于 round43 收口**（`GET /configuration`，决策 #119），表单回显与配置总览可以直接用它。
- 高危动作的 Web 确认语义照搬 CLI 的 `--yes`/`confirm`/`commit confirmed` 体系；#19（logout 不释放 candidate 锁）**已于 round43 收口**（决策 #119）。
- 缺口 #2/#3/#4（日志、ping、traceroute）**已作为增量 3 第一刀收口**（round47，决策 #123）；VS/VM 统计字段**已收口**（round49，决策 #124）；剩余 1 项可做功能补（`load merge`），另 2 项已登记延期（token revoke、format-data），1 项两边都未接（`show vpp runtime`）。
- 增量顺序（**已完成**）：增量 2 = 配置读写（round43~46）→ 增量 3 第一刀 = 诊断（round47）→ 剩余缺口里 **4 项可做功能补已全部收口**（统计字段 #5/#6、ssh host-key #8、core-dumps 导出 #9、`load merge` #11，round49）；**余 3 项**：token revoke（延期 V2）、format-data（有意延期）、`show vpp runtime`（两边都未接）。
