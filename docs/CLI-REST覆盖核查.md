# CLI ⇄ REST 覆盖核查（Web 控制台“覆盖 CLI 全部功能”的核查）

| 文档属性 | 内容 |
|---|---|
| 用途 | 回答“Web 控制台能否实现 CLI 的全部功能”：把 **261 行 CLI 命令**逐一对照 **141 个 REST 端点**，分出**已有类型化端点 / 需补 API / CLI-only by design** 三张清单 |
| 日期 | 2026-09-25（round80）；2026-09-30（v2 决策 #301/#303/#304 复核） |
| CLI 侧权威清单 | `docs/NFViS-CLI命令全表.md`（**261 行**，分族：show 68 / request 46 / 其余操作 11 / 通用管道 9 / 配置模式 127；该表由 CLI 批次维护，本核查不修改它） |
| REST 侧权威清单 | `docs/NFViS-openapi.yaml`（104 个路径 / **141 个操作**）+ `internal/api/server.go` 的 137 条路由注册（其中 5 条 `{tail...}` 通配展开：4 条各展开为 2 条、1 条为 1 条 → 合计 **141 个端点**） |
| 机器守护 | `internal/api/cli_rest_coverage_test.go`（覆盖表 / 例外表 / 缺口表三张 + 三条断言：端点必须存在、契约命令必须归类、缺口与例外必须写理由） |
| 依据 | 规格书 §12 与附录 A 决策 #65/#76⑧/#107/#115/#119/#122/#123/#124/#125/#126/#127/#142/#153/#301/#303；round80 真机实测（`contrib/scripts/cli-fulltest.sh` 通过 195 / 失败 1 / 预期报错 5，pty 冒烟 `cli-pty-smoke.sh` 通过 10 / 失败 0） |

## 0. 结论

**261 行命令中：240 行已有类型化 REST 端点（可直接写页面）、1 行是真缺口（需补 API 或不补）、20 行是 CLI-only by design（交互形态差异，不需要 API）。240 + 1 + 20 = 261（行口径；本节为**当前口径**，历轮数字见下方更新记录）。**

REST 侧现状（本轮复核）：

- **141 个端点** = 104 个路径 / 141 个操作（GET 63 / POST 48 / DELETE 15 / PUT 15；免鉴权 7）。
  round80 复核时的 class 分布为 R 53 / O 3 / S 73 / 免鉴权 6——本核查只维护 CLI 行口径与端点存在性，
  class 分布以 `internal/api/server.go` 的注册为准，不逐轮重算。
  其中 `x-internal` 2 条（`POST /cli/execute`、`GET /cli/candidates`）**不计入覆盖**（round37 口径：契约明言仅 nfvis-cli 使用，前端不碰）。
  另有 8 条端点不对应任何命令行形态（`/login`、`/login-banner`、`/logout`、`/events`、`/metrics`、`/openapi.json`、`/ui`、`/ui/`：会话 / 运行 / 控制台自身读物）。
- **契约零漂移**：server.go 注册的 141 个端点**全部**在契约里（无注册缺契约，也无契约缺注册）；round80 已把契约里唯一的幽灵声明
  **`PUT /vpp/config` 删除**（决策 #153——该 `put:` 从未注册，VPP 配置的写入路径是配置模式 `set vpp …` + `commit`，即
  `PUT /configuration/candidate` + `POST /configuration/commit`，不另设写端点）。
- 覆盖的主体：**配置模式 127 行里 124 行计入覆盖**，其中 **114 条 `set`/`delete` 语句整族由 candidate API 架构性覆盖**
  （`PUT`/`DELETE /configuration/candidate` + `POST /configuration/commit`，可带 `X-NFVIS-Auto-Commit`）——这正是 REST 事务模型
  相对 CLI 语句的本质，不需要逐条端点；**show 族 68 行里 65 行**有对应 GET；**request 族 46 行里 45 行**有对应动作端点。
- **1 个缺口**：V1 登记的延期（`request system storage format-data`）。
- **20 个例外分三类**：REPL 交互形态（`wizard` / `monitor` / `?` / `help` / 层级导航）、CLI 侧文本渲染（通用管道 / `display set`）、
  CLI 本地行为（`exit` / `quit` / `start shell`）。
- **口径换算**：若把 ` / ` 并列的两行拆开（`exit` / `quit`；`edit <path>` / `up` / `top` / `exit`），则 **265 条 = 覆盖 240 / 缺口 1 / 例外 24**
  （多出的 4 条全在例外桶：本地行为与层级导航）。**本核查全篇用“行”口径，合计 261**。

### 历史更新记录

保留既有各轮收口记录（可追溯；每轮的数字是**当时那张表**的口径，本轮已按新表重算，见下方说明）：

| 车次 | 收口内容 | 当时的变化 |
|---|---|---|
| round43 | 缺口 #1 `show configuration`（committed 全量读取）→ 新增 `GET /configuration`（`{configuration, revision}`，决策 #119；同批收口候选形状声明与「登出释放 candidate 锁」缺陷 #19） | 缺口 14→13、覆盖 225→226 |
| round46 | 缺口 #10 `commit check`（仅校验不下发）→ `POST /configuration/check`（决策 #122） | — |
| round47 | 缺口 #2/#3/#4/#7 日志 / ping / traceroute / 清零统计 → `GET /system/logs`、`POST /diagnostics/ping`（未通即 502）、`POST /diagnostics/traceroute`、`POST /interfaces:clear-statistics`（决策 #123） | 覆盖 226→231、缺口 13→8 |
| round49 | 缺口 #5/#6 VS / VM 详情的 `statistics` 字段（决策 #124）、#8 `ssh host-key regenerate`（决策 #125）、#9 `core-dumps export`（决策 #126，同时修掉 CLI 侧「只打印已受理」的假成功）、#11 `load merge`（决策 #127，`X-NFVIS-Merge: true`） | 覆盖 231→236、缺口 8→3 |
| 决策 #142 前置 | 新增命令行 `show configuration history` 与端点 `GET /configuration/history`（配置提交历史——此前只有 `rollback [n]` / `compare rollback <n>`，没有「列出历史快照」的读物） | 形态 258→259、覆盖 236→237 |
| **round80（本轮）** | 以《命令全表》按**实际行数**重算的 259 行为新基数逐行重算；`show configuration [permissions <class>]` 由覆盖桶改判**缺口**（该行 `permissions` 分支本轮改为明确提示未实现，决策 #153）；本轮新落地的等价写法行（`show configuration sessions`、`show protocols lldp neighbors`、`show interfaces <ifname> detail\|statistics\|sriov`）全部有端点承载，计入覆盖 | **覆盖 236 / 缺口 4 / 例外 19（新表行口径，合计 259）** |
| v2 决策 #301（2026-09-30） | ① 新增 `show system api tokens` 行（形态 259→260），端点 `GET /system/api-tokens`；② 缺口 #3「逐 token 吊销」收口：`request system api token revoke <token-id>` → `POST /system/api-tokens/{id}:revoke`（Web 控制台用户与权限页同步实现）；③ 顺带按既成事实把缺口 #1 `show vpp runtime` 移入例外桶（决策 #200 已实现：数据源是本机 stats segment、CLI 侧渲染，Web 等价形态 `/vpp/status`） | **覆盖 236→238 / 缺口 4→2 / 例外 19→20（合计 260）** |
| v2 决策 #303（2026-09-30） | 新增 1 行 `set system login banner <text>`（配置语句 → candidate API **架构性覆盖**，无需新端点；`delete system login banner` 同族）。该能力的**两个展示面**由新端点承载：未认证只读 `GET /login-banner`（登录页/CLI 登录前展示，`security: []`、只回 banner 本身）与 `PUT`/`DELETE /system/login-banner`（Web「用户与权限」页的「登录横幅」卡，S 级、一次性事务直提入审计）——登录页与 CLI 前置横幅不对应任何命令行形态，故不计入 CLI 行覆盖 | **覆盖 238→239 / 缺口 2 / 例外 20（合计 261）；REST 侧本决策 +3 操作 / +2 路径（连同 #301 的 +2 操作 / +2 路径，当前 140 操作 / 103 路径）** |
| v2 决策 #304（2026-09-30） | 缺口 #2「`show configuration [permissions <class>]` 的 `permissions` 分支」收口：语义定义为**生效权限视图**（逐路径判定 + 依据，判定单源在 aaa），端点 `GET /configuration/permissions?class=<name>`；Web「用户与权限」页「权限类」表每行「生效权限」按钮同源消费 | **覆盖 239→240 / 缺口 2→1 / 例外 20（合计 261）；REST 侧本决策 +1 操作 / +1 路径（当前 141 操作 / 104 路径）** |

> **与上轮绝对值对不上的原因**（口径变化，不是能力增减）：①《命令全表》本轮按实际行数重算分族（67/46/11/9/126，此前表内
> 统计与实际行数不符）；② `show configuration [permissions <class>]` 从覆盖桶移到缺口桶；③ round80 新增的等价写法行全部计入覆盖。
> REST 侧本轮只有一次**删除**（`PUT /vpp/config` 幽灵声明），没有丢失任何能力。

## 1. 口径与方法

- **行口径（261）**：以《命令全表》§1/§2 中**以反引号命令起始的表行**计，与该表 §3 的“一行一个命令行”一致
  （同一命令的二级子命令各计一行；`a` / `b` 并列写在同一行只算 1 行）。复核方法：
  `grep -c '^| \`' docs/NFViS-CLI命令全表.md` → **263**（= 261 + §3 统计表自身的 `show`、`request` 两行）。
  本轮已按实际行数重算分族：**show 68 / request 46 / 其余操作 11 / 通用管道 9 / 配置模式 127 = 261**
  （其余操作含 §1.1 的 `help [command]`；§1.3 的 `Tab` 补全行与 `?` 行同属 REPL 补全形态，按一行归类）。
- **分类定义**：
  - **A 覆盖**：存在承载该行功能的类型化 REST 端点。`/cli/execute` 与 `/cli/candidates` 是 `x-internal`，
    按 round37 口径**不计入**覆盖（前端不碰）；`/ui`、`/metrics`、`/openapi.json` 等控制台/运行自身端点同理。
  - **B 缺口**：CLI 有该行，REST 没有端点或响应缺字段；补法一律**契约先行**（OpenAPI + 附录 A 决策 + routes/shape 守护），
    不走 `/cli/execute`。
  - **C 例外**：CLI-only by design——REPL 交互形态（导航 / 补全 / 持续跟踪 / 问答向导）或 CLI 侧渲染（管道），
    Web 的等价物是表单、定时刷新 + `GET /events`、原生 JSON，不需要 API。
- **归类规则**：一行恰好落一个桶。行的**主形态**有端点即计入覆盖；若一行声明的某个子形态无端点而其余可用
  （round80 只有 `show configuration [permissions <class>]` 一例，已由 v2 决策 #304 收口），**整行计入缺口**并在 §3 写明可用端点——
  与《命令全表》把该行实测列标 `⚠️ 已知缺口`一致。
- **方法**：逐族对照《命令全表》与 `openapi.yaml` 的路径 / 方法 / 响应 schema；响应缺字段的以契约 schema 为准绳核实
  （round49 的 VS / VM `statistics` 字段就是这样定的）。端点引用必须真实存在于契约，由 §5 的守护断言 A 机器盯着。

## 2. A 覆盖矩阵（240 行）

### 2.1 show 族（65/68）

| 命令族（行数） | REST 端点 |
|---|---|
| `show version`（1） | `GET /system/version` |
| `show system uptime\|cpu\|memory\|storage`（4） | `GET /system/status`（R37-1 收口后同源字段） |
| `show system hugepages`（1） | `GET /system/status` + `GET /resource-pools` |
| `show system kernel`（1） | `GET /system/kernel` |
| `show system hardware`（1） | `GET /system/hardware` |
| `show system core-dumps` / `show system tech-support`（2） | `GET /system/core-dumps` / `GET /system/tech-support` |
| `show system configuration sessions`（1） | `GET /system/configuration/sessions` |
| `show configuration sessions`（1，**round80 等价写法**：同一实现、输出逐字相同） | 同上 |
| `show interfaces` / `show interfaces physical` / `show interfaces management`（3） | `GET /interfaces`（`kind=physical\|management\|vpp`） |
| `show interfaces physical <ifname> detail\|statistics\|sriov`（3） | `GET /interfaces/{name}`（详情含 statistics） |
| `show interfaces <ifname> detail\|statistics\|sriov`（3，**round80 等价写法**：`physical` 可省，两种写法走同一实现） | 同上 |
| `show virtual-switches`（1） | `GET /virtual-switches` |
| `show virtual-switches <name> detail` / `statistics`（2） | `GET /virtual-switches/{name}`（**statistics 是详情响应字段**，round49 决策 #124） |
| `show virtual-switches <name> ports` / `mac-table`（2） | `GET /virtual-switches/{name}/ports` / `.../mac-table` |
| `show vrfs`（1） / `show vrfs <name>`（1） / `show vrfs <name> routes`（1） | `GET /vrfs` / `GET /vrfs/{name}` / `GET /vrfs/{name}/routes` |
| `show acls`（1） / `show acls <name> detail`（1） | `GET /acls` / `GET /acls/{name}` |
| `show nat`（1） | `GET /nat`（运行态会话表另有 `GET /nat/sessions`） |
| `show port-mirroring`（1） / `show qos policies`（1） | `GET /port-mirroring` / `GET /qos/policies` |
| `show vpp` / `threads` / `buffers` / `memory`（4） | `GET /vpp/status` |
| `show vpp capture`（1） | `GET /vpp/capture`（导出文件下载 `GET /vpp/capture/{file}`） |
| `show bonds`（1） / `show bonds <name> detail`（1） | `GET /bonds` / `GET /bonds/{name}` |
| `show lldp neighbors [interface <ifname>]`（1） | `GET /protocols/lldp/neighbors`（round80 起按口过滤真的生效） |
| `show protocols lldp neighbors`（1，**round80 等价写法**：同一读物、同一实现） | 同上 |
| `show virtual-machine-functions`（1） / `<name> detail` / `interfaces` / `snapshots`（3） | `GET /virtual-machine-functions`、`/{name}`、`/{name}/snapshots` |
| `show virtual-machine-functions <name> statistics`（1） | `GET /virtual-machine-functions/{name}`（**statistics 是详情响应字段**，round49 决策 #124） |
| `show container-functions`（1） / `<name> [detail]`（1） / `<name> interfaces`（1） | `GET /container-functions`、`/{name}` |
| `show images`（1） / `show images <name> detail`（1） | `GET /images` / `GET /images/{name}` |
| `show resource-pools`（1） / `show alarms [active\|all]`（1） / `show users`（1） | `GET /resource-pools` / `GET /alarms` / `GET /system/login-users` |
| `show system api tokens`（1，v2 决策 #301） | `GET /system/api-tokens`（super-user 见全部、其他 class 仅自己；范围在服务端按身份裁定） |
| `show log audit [last <n>]`（1） | `GET /audit-logs` |
| `show log system [level <lvl>] [last <n>]`（1） | `GET /system/logs`（`text/plain`、`?last=<n>`；round47 收口） |
| `show configuration candidate`（1） | `GET /configuration/candidate` |
| `show configuration history`（1） | `GET /configuration/history`（决策 #142） |
| `show configuration compare rollback <n>`（1） | `GET /configuration/diff` + `POST /configuration/rollback/{n}`（两步组合） |
| `show tech-support`（1，顶级等价写法） | `GET /system/tech-support` |
| `show`（1，配置模式：candidate 当前层级） | `GET /configuration/candidate` |
| `show configuration permissions <class> [detail]`（1，v2 决策 #304 收口） | `GET /configuration/permissions?class=<name>`（生效权限视图：该 class 逐路径判定 + 依据；无参 = 调用者自己 class；非 super 查他人 403、未知 404；`detail` 的判定依据由同一响应字段承载，CLI 渲染） |

未计入本表的 3 行：`show vpp runtime [thread <id>]`、`show log vnf <name> [last <n>]`、`show | display set`（**例外**，§4；
`show vpp runtime` 自 v2 决策 #301 起按既成事实归例外——CLI 侧渲染，Web 等价形态 `/vpp/status`）。
（`show configuration [permissions <class>]` 原为缺口 §3 #2，v2 决策 #304 落地后已移入本表上行。）

### 2.2 request 族（45/46）

| 命令族（行数） | REST 端点 |
|---|---|
| `request virtual-machine-functions <n> start\|stop\|restart\|delete`（4） | `POST /virtual-machine-functions/{name}:start\|:stop\|:restart`、`DELETE /virtual-machine-functions/{name}` |
| `request virtual-machine-functions <n> console`（1） | `POST /virtual-machine-functions/{name}/console`（一次性 ticket）+ `GET /virtual-machine-functions/{name}/console/ws` |
| `request … snapshot create\|rollback\|delete`（3） | `POST /virtual-machine-functions/{name}/snapshots`、`POST /virtual-machine-functions/{name}/snapshots/{snapshot}:rollback`、`DELETE /virtual-machine-functions/{name}/snapshots/{snapshot}` |
| `request container-functions <n> start\|stop\|restart\|log\|delete`（5） | `POST /container-functions/{name}:start\|:stop\|:restart`、`GET /container-functions/{name}/logs`、`DELETE /container-functions/{name}` |
| `request images upload\|download\|delete name <n>`（3） | `POST /images`（multipart / URL / incoming 三模式）、`DELETE /images/{name}` |
| `request interfaces <n> enable\|disable`（2） | `PUT /interfaces/{name}`（`enabled` 字段） |
| `request interfaces <n> bind-dpdk\|unbind-dpdk`（2） | `PUT /interfaces/{name}/dpdk`（body `bound` 布尔；`confirm=true`） |
| `request sriov create-vfs\|delete-vfs`（2） | `PUT /interfaces/{name}/sriov`（设 VF 数量即创建 / 回收） |
| `request vpp restart`（1） | `POST /vpp/restart` |
| `request vpp trace start\|stop\|export`（3） | `POST /vpp/capture`、`DELETE /vpp/capture`（`export=true` 时同时导出 pcap）、`GET /vpp/capture/{file}` |
| `request system software add\|rollback`（2） | `POST /system/software`、`POST /system/software:rollback` |
| `request system reboot\|shutdown\|poweroff`（3） | `POST /system:reboot`、`POST /system:shutdown`（契约注明含 poweroff） |
| `request system kernel apply\|rollback`（2） | `POST /system/kernel:apply\|:rollback` |
| `request system configuration backup\|restore`（2） | `POST /system/backup`（+ 只读清单 `GET /system/backup`、下载 `GET /system/backup/{file}`）、`POST /system/restore` |
| `request system tech-support generate` / `core-dumps delete`（2） | `POST /system/tech-support` / `DELETE /system/core-dumps` |
| `request system core-dumps export <url>`（1） | `POST /system/core-dumps:export`（round49 决策 #126） |
| `request system zeroize` / `api tls regenerate`（2） | `POST /system:zeroize` / `POST /system/tls:regenerate` |
| `request system ssh host-key regenerate`（1） | `POST /system/ssh-host-key:regenerate`（round49 决策 #125） |
| `request system password change`（1） | `POST /system/login-users/{name}:change-password`（与 REST 同源） |
| `request system ntp sync` / `request alarms clear [id <id> \| all]`（2） | `POST /system/ntp:sync` / `POST /alarms:clear` |
| `request system api token revoke <token-id>`（1，v2 决策 #301 收口） | `POST /system/api-tokens/{id}:revoke`（super-user 任意、其他 class 仅自己的；不存在/无权统一 404 不泄露存在性） |

未计入本表的 1 行：`request system storage format-data`（**缺口**，§3 #4）。

### 2.3 其余操作与通用管道（4/11 + 2/9）

| 命令（行数） | REST 落点 |
|---|---|
| `configure`（1） | 配置事务端点族（§2.4） |
| `ping <host> [source <ip>] [count <n>] [vrf <name>]`（1） | `POST /diagnostics/ping`（**未通即 502**；round47 决策 #123） |
| `traceroute <host> [vrf <name>]`（1） | `POST /diagnostics/traceroute`（round47 收口） |
| `clear interfaces statistics [<ifname>]`（1） | `POST /interfaces:clear-statistics`（204；round47 收口） |
| `\| compare`（1） | `GET /configuration/diff`（差异数据；渲染形态见 §4） |
| `\| compare rollback <n>`（1） | `GET /configuration/diff` + `POST /configuration/rollback/{n}` |

未计入本表的 14 行：其余操作 7 行（`exit` / `quit`、`monitor interfaces <ifname> [interval <sec>]`、`wizard`、
`monitor vnf <name>`、`start shell`、`?`（含 `Tab` 补全）、`help [command]`）与通用管道 7 行
（`\| match` / `\| except` / `\| count` / `\| last` / `\| begin` / `\| display json` / `\| display xml`）——全为**例外**（§4）。

### 2.4 配置模式（124/127；末行 `show` 属 show 族，仅交叉引用、不计入本节的 124）

| 命令族（行数） | REST 落点 |
|---|---|
| `set <path> …` / `delete <path> …`（2 行通用形态 + §2.2~§2.9 的 **114 条语句** = 116） | `PUT`/`DELETE /configuration/candidate`（`X-NFVIS-Auto-Commit: true` 时校验 + 下发 + 落库一次完成；删除走 candidate 上的键删除）。含 v2 决策 #303 的 `set system login banner <text>` / `delete system login banner`（单行、≤512 字节，提交校验拒绝超限） |
| `commit`（1） | `POST /configuration/commit` |
| `commit check`（1） | `POST /configuration/check`（同一份校验、不落库不下发；round46 决策 #122） |
| `commit confirmed [min]`（1） | `POST /configuration/commit`（`confirmed_minutes>0`）+ `POST /configuration/commit:confirm` |
| `commit and-quit`（1） | `POST /configuration/commit`（提交后退出配置模式由前端等价实现） |
| `rollback [n]`（1） | `POST /configuration/rollback/{n}`（取快照为 candidate，需再 commit） |
| `load override\|merge <path>`（1） | `PUT /configuration/candidate`（override 缺省；merge 用 `X-NFVIS-Merge: true`，round49 决策 #127） |
| `save <path>`（1） | `GET /configuration/candidate`（取 JSON 自行落盘；配置归档另有 `POST /system/backup`） |
| `discard`（1） | `DELETE /configuration/candidate` |
| `show`（1，见 §2.1） | `GET /configuration/candidate` |

未计入本表的 3 行：`edit <path>` / `up` / `top` / `exit`、`annotate <path> "text"`、`run <oper-command>`——**例外**（§4）。

## 3. B 缺口清单（1 行未收口；补法一律契约先行，不碰 `/cli/execute`）

| # | 命令（行） | CLI 实测 | REST 现状 | 理由与归属 |
|---|---|---|---|---|
| 1 | ~~`show vpp runtime [thread <id>]`~~ | **已收口**（决策 #200）：CLI 给线程级运行态（stats segment），按节点明细无结构化来源、CLI 如实说明 | 无类型化端点（Web 等价形态 `/vpp/status` 同源） | 归入**例外**桶（§4）：CLI 侧渲染，Web 走 `/vpp/status`。v2 决策 #301 起按既成事实移出缺口 |
| 2 | ~~`show configuration [permissions <class>]`~~ | **已收口**（v2 决策 #304）：`permissions <class> [detail]` 落地为**生效权限视图**（逐路径判定 + 依据；判定单源在 aaa） | `GET /configuration/permissions?class=<name>`（无参 = 调用者自己；非 super 查他人 403、未知 404） | 移入**覆盖**桶（§2.1）；Web「用户与权限」页「权限类」表每行「生效权限」按钮同源消费该端点 |
| 4 | `request system storage format-data` | 🚫 破坏性（契约已登记延期） | 无 | **V1 有意延期**：破坏性，待数据分区定义后再开放（决策 #65）。归属：V2 |

## 4. C 例外清单（20 行，CLI-only by design，不需要 API）

| 命令（行） | 理由 |
|---|---|
| `show log vnf <name> [last <n>]` | 指引型命令（指向容器 `log` / VM console），等价物已存在 |
| `show \| display set` | CLI 文本渲染；未实现且明确提示（附录 A #84 /《命令全表》§4⑧）——需 model→CLI **反向映射**，而 Web 不存在这个需求（原生消费 JSON / `save` 导出） |
| `wizard` | 决策 #107：CLI 端交互编排，明确无 API 端点（Web 等价物是向导式页面） |
| `monitor interfaces <ifname> [interval <sec>]` | 决策 #92：CLI 轮询形态；Web 等价物是视图定时刷新 + `GET /events` 推送 |
| `monitor vnf <name>` | 决策 #92：同上（CLI 侧已由「单次快照」改为真跟踪） |
| `exit` / `quit` | CLI 本地行为（关会话；服务端侧的等人物是 `POST /logout`，不对应命令行形态） |
| `help [command]` | REPL 帮助；Web 用表单与静态候选（`/cli/candidates` 为 x-internal，按 round37 口径前端不碰） |
| `?`（含 `Tab` 补全） | 上下文补全（按键即时）；Web 用表单与静态候选 |
| `start shell` | 本地控制台 shell（SSH 会话禁用），Web 无对应形态 |
| `\| match` / `\| except` / `\| count` / `\| last` / `\| begin`（5 行） | CLI 文本过滤；Web 以前端过滤 + 分页查询参数（`limit`/`offset`，FR-API-007）实现 |
| `\| display json` / `\| display xml`（2 行） | CLI 渲染；Web 原生消费 JSON |
| `edit <path>` / `up` / `top` / `exit`（1 行） | 配置模式层级导航；数据操作已被 candidate API 覆盖 |
| `annotate <path> "text"` | REPL 注释便利（candidate annotations） |
| `run <oper-command>` | 配置模式内执行便利；被运行的命令本身都有端点 |

## 5. 机器守护（`internal/api/cli_rest_coverage_test.go`）

三张表 + 三条断言，沿用仓库既有的 `contractCLICommands` / `deferred` 模式：

1. **`cliRESTCoverage`**（命令形态 → `METHOD /path`）：覆盖矩阵的机器可读形式；配置语句用 `set `/`delete ` 前缀规则整体归入
   candidate API（架构性覆盖，无需逐条）。本轮补入 `show lldp neighbors interface <ifname>`（round80 起过滤真的生效），
   并把 `request vpp trace export` 的落点补全为 `DELETE /vpp/capture + GET /vpp/capture/{file}`（与《命令全表》§1.2 的落点一致）。
2. **`cliRESTExceptions`**（CLI-only by design → 理由）：按命令粒度登记（`exit`、`quit` 分开；`edit`/`up`/`top` 分开），
   故条目数多于本文档 §4 的“行”数。
3. **`cliRESTGaps`**（缺口 → 理由）：缺口因此**机器可见**，补一个划掉一个。v2 决策 #304 把
   `show configuration permissions <class>` 从缺口移入覆盖桶（`GET /configuration/permissions`），
   缺口表当前只剩 `request system storage format-data`（登记延期 V2）。
4. 断言 A：覆盖表声明的每个端点必须真实存在于契约（端点改名/删除 → 红，防映射过期；`routes_contract` 只判
   “路由 ⊆ 契约”的反方向，判不出“CLI 还指向一个已不存在的端点”）。因此覆盖表**不得**引用已删除的 `PUT /vpp/config`。
5. 断言 B：`contractCLICommands`（既有契约命令清单）里每条都必须落在三张表之一——**新增契约命令忘记归类即红**。
6. 断言 C：缺口与例外必须写明理由（登记不写理由等于没登记）。

纪律（写在测试头部注释里）：新增 CLI 命令须同步补《命令全表》、`contractCLICommands` 与本覆盖表/例外表/缺口表之一；
本测试只认这三张表，文档策展的完整 261 行清单以《命令全表》为准。

## 6. 对控制台的输入（现状）

- **API 侧够用**：计入覆盖的 239 行全部有类型化端点——配置读写（candidate / check / commit / confirm / diff / rollback / history）、
  生命周期、诊断、抓包、备份恢复、用户与告警都在位，**不需要为任何一条配置语句新增端点**（114 条语句走同一套事务 API）。
- **缺口的归属（v2 决策 #304 后）**：未收口的只剩 1 项——`format-data`（登记延期 V2）。`show vpp runtime` 归例外桶
  （Web 走 `/vpp/status`），逐 token 吊销已收口（活动会话卡片落在「用户与权限」页），
  `permissions` 子形态已由决策 #304 落地为**生效权限视图**（端点 `GET /configuration/permissions?class=<name>`，
  控制台「用户与权限」页「权限类」表每行的「生效权限」按钮同源消费）。
- **v2 决策 #303 的界面落点**：`set system login banner` 的图形等价物是「用户与权限」页的「登录横幅」卡
  （`GET /login-banner` 读当前值、`PUT`/`DELETE /system/login-banner` 保存/清除，低危档确认）；登录横幅本身
  另有两个展示面（Web 登录页表单上方、`nfvis-cli` 交互模式口令提示之前）——都消费同一个未认证只读端点。
- **v2 决策 #304 的界面落点**：「用户与权限」页「权限类（class）」表每行加「生效权限」按钮，就地展开该 class 的
  生效权限摘要（允许/拒绝条数 + 前若干条允许路径 + 说明完整列表见 CLI / REST），数据取自 `GET /configuration/permissions`；
  非 super 用户该按钮只看得到自己的 class（服务端 403 兜底，界面只是不摆必被拒的入口）。
- **20 个例外由 Web 形态等价承担**：表单 + 静态候选（`help`/`?`/`wizard`）、定时刷新 + `GET /events`（`monitor`）、
  前端过滤与分页（管道）、原生 JSON（`display`）、向导页（`wizard`）；高危动作的确认语义照搬 CLI 的
  `--yes` / `confirm` / `commit confirmed` 体系。
- **边界**：本核查只回答**API 够不够**，不回答**界面接没接**——界面侧清单与验收另见控制台 IA 设计
  （`docs/superpowers/specs/2026-09-23-web-console-ia-design.md`）与 round80 控制台验收证据。
