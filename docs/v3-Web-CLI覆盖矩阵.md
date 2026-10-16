# Web 控制台 ⇄ CLI 覆盖矩阵（v3，生成物）

> **本文件由 `contrib/scripts/web-cli-coverage-matrix.py` 生成，勿手改。**

> **口径（取代码真源，不取文档——文档会滞后）**：

> ① **CLI → REST** 取自 `internal/api/cli_rest_coverage_test.go` 的三张表；该表由
> `TestContractCLICommandsAreClassified` 强制「契约里每条 CLI 命令都必须被归类」，
> 故**不滞后**（新增命令不归类即测试红）。

> ② **REST → Web** 取自 `internal/api/ui/js/**` 的 `api('…')` 字面量 + `routes.json` 的 endpoints。

> ③ 端点归一化：路径参数 → `*`、去查询串与尾斜杠、`:action` → `:*`。


## 汇总

| 口径 | 条数 |
|---|---|
| CLI 命令（有 REST 端点者，代码守护表） | 146 |
| ├ **Web 已接**（端点被 Web 调用） | 142 |
| ├ 别名行（同端点、Web 走另一名字） | 0 |
| └ **Web 未接**（端点存在、Web 未调用） | 4 |
| CLI-only by design（守护例外表，不需要 API） | 25 |
| 真缺口（守护缺口表） | 0 |

**判读**：REST 层**零缺口**（守护表强制）；下面的 A 节＝**Web 前端待补清单**。

## A. Web 未接（REST 端点已有、Web 未调用）——**待补**

> 注：下列 4 条经查**都是「兼容别名」**（守护表里就写着「兼容别名（同一 handler）」）：
> `request vpp trace start|stop|export` 与 `show vpp capture` 是 `request capture *` /
> `show capture` 的旧名，Web 走的是**新名**（`/capture`）——功能上已覆盖、**不需另做**。

| CLI 命令 | 族 | 已有端点 |
|---|---|---|
| `request vpp trace export` | request vpp | `DELETE /vpp/capture`, `GET /vpp/capture/{file}` |
| `request vpp trace start` | request vpp | `POST /vpp/capture` |
| `request vpp trace stop` | request vpp | `DELETE /vpp/capture` |
| `show vpp capture` | show vpp | `GET /vpp/capture`, `GET /vpp/capture/{file}` |

## A2. 别名行（同端点已有别的命令被 Web 接上；Web 走另一个名字，不需另做）

| CLI 命令（别名） | 族 | 端点 |
|---|---|---|

## B. Web 已接

| CLI 命令 | 族 | Web 命中端点 |
|---|---|---|
| `clear interfaces statistics` | clear interfaces | `/interfaces:*` |
| `commit` | commit | `/configuration/commit` |
| `commit and-quit` | commit | `/configuration/commit` |
| `commit check` | commit | `/configuration/check` |
| `commit confirmed` | commit | `/configuration/commit`, `/configuration/commit:*` |
| `configure` | configure | `/configuration/candidate`, `/configuration/commit` |
| `discard` | discard | `/configuration/candidate` |
| `load merge` | load | `/configuration/candidate` |
| `load override` | load | `/configuration/candidate` |
| `ping <host>` | ping <host> | `/diagnostics/ping` |
| `request alarms clear` | request alarms | `/alarms:*` |
| `request alarms clear all` | request alarms | `/alarms:*` |
| `request alarms clear id <id>` | request alarms | `/alarms:*` |
| `request capture export` | request capture | `/capture` |
| `request capture start interface <ifname> count <n>` | request capture | `/capture` |
| `request capture stop` | request capture | `/capture` |
| `request container-functions <n> delete` | request container-functions | `/container-functions/*` |
| `request container-functions <n> log` | request container-functions | `/container-functions/*/logs` |
| `request container-functions <n> restart` | request container-functions | `/container-functions/*:*` |
| `request container-functions <n> start` | request container-functions | `/container-functions/*:*` |
| `request container-functions <n> stop` | request container-functions | `/container-functions/*:*` |
| `request images delete name <n>` | request images | `/images/*` |
| `request images download` | request images | `/images` |
| `request images upload` | request images | `/images` |
| `request interfaces <ifname> bind-dpdk` | request interfaces | `/interfaces/*/dpdk` |
| `request interfaces <ifname> disable` | request interfaces | `/interfaces/*` |
| `request interfaces <ifname> enable` | request interfaces | `/interfaces/*` |
| `request interfaces <ifname|pci> unbind-dpdk` | request interfaces | `/interfaces/*/dpdk` |
| `request sriov create-vfs <ifname> count <n>` | request sriov | `/interfaces/*/sriov` |
| `request sriov delete-vfs <ifname> vf <n>` | request sriov | `/interfaces/*/sriov` |
| `request system api tls regenerate` | request system | `/system/tls:*` |
| `request system api token revoke <token-id>` | request system | `/system/api-tokens/*:*` |
| `request system configuration backup` | request system | `/system/backup`, `/system/backup/*` |
| `request system configuration restore` | request system | `/system/restore` |
| `request system core-dumps delete` | request system | `/system/core-dumps` |
| `request system core-dumps export` | request system | `/system/core-dumps:*` |
| `request system hugepages reclaim` | request system | `/system/hugepages:*` |
| `request system kernel apply` | request system | `/system/kernel:*` |
| `request system kernel rollback` | request system | `/system/kernel:*` |
| `request system ntp sync` | request system | `/system/ntp:*` |
| `request system password change` | request system | `/system/login-users/*:*` |
| `request system poweroff` | request system | `/system:*` |
| `request system reboot` | request system | `/system:*` |
| `request system shutdown` | request system | `/system:*` |
| `request system software add` | request system | `/system/software` |
| `request system software rollback` | request system | `/system/software:*` |
| `request system ssh host-key regenerate` | request system | `/system/ssh-host-key:*` |
| `request system storage format-data` | request system | `/system:*` |
| `request system tech-support generate` | request system | `/system/tech-support` |
| `request system zeroize` | request system | `/system:*` |
| `request virtual-machine-functions <n> console` | request virtual-machine-functions | `/virtual-machine-functions/*/console`, `/virtual-machine-functions/*/console/ws` |
| `request virtual-machine-functions <n> delete` | request virtual-machine-functions | `/virtual-machine-functions/*` |
| `request virtual-machine-functions <n> restart` | request virtual-machine-functions | `/virtual-machine-functions/*:*` |
| `request virtual-machine-functions <n> snapshot create` | request virtual-machine-functions | `/virtual-machine-functions/*/snapshots` |
| `request virtual-machine-functions <n> snapshot delete` | request virtual-machine-functions | `/virtual-machine-functions/*/snapshots/*` |
| `request virtual-machine-functions <n> snapshot rollback` | request virtual-machine-functions | `/virtual-machine-functions/*/snapshots/*:*` |
| `request virtual-machine-functions <n> start` | request virtual-machine-functions | `/virtual-machine-functions/*:*` |
| `request virtual-machine-functions <n> stop` | request virtual-machine-functions | `/virtual-machine-functions/*:*` |
| `request vpp restart` | request vpp | `/vpp/restart` |
| `rollback` | rollback | `/configuration/rollback/*` |
| `save` | save | `/configuration/candidate`, `/system/backup` |
| `show` | show | `/configuration/candidate` |
| `show acls` | show acls | `/acls` |
| `show acls <name>` | show acls | `/acls/*` |
| `show acls <name> detail` | show acls | `/acls/*` |
| `show alarms` | show alarms | `/alarms` |
| `show bonds` | show bonds | `/bonds` |
| `show bonds <name> detail` | show bonds | `/bonds/*` |
| `show capture` | show capture | `/capture` |
| `show configuration` | show configuration | `/configuration` |
| `show configuration candidate` | show configuration | `/configuration/candidate` |
| `show configuration compare rollback <n>` | show configuration | `/configuration/diff`, `/configuration/rollback/*` |
| `show configuration history` | show configuration | `/configuration/history` |
| `show configuration permissions <class>` | show configuration | `/configuration/permissions` |
| `show configuration permissions <class> detail` | show configuration | `/configuration/permissions` |
| `show configuration sessions` | show configuration | `/system/configuration/sessions` |
| `show container-functions` | show container-functions | `/container-functions` |
| `show container-functions <name>` | show container-functions | `/container-functions/*` |
| `show container-functions <name> interfaces` | show container-functions | `/container-functions/*` |
| `show dns proxy` | show dns | `/dns/proxy` |
| `show images` | show images | `/images` |
| `show images <name> detail` | show images | `/images/*` |
| `show interfaces` | show interfaces | `/interfaces` |
| `show interfaces <ifname>` | show interfaces | `/interfaces/*` |
| `show interfaces <ifname> detail` | show interfaces | `/interfaces/*` |
| `show interfaces <ifname> sriov` | show interfaces | `/interfaces/*` |
| `show interfaces <ifname> statistics` | show interfaces | `/interfaces/*` |
| `show interfaces management` | show interfaces | `/interfaces` |
| `show interfaces physical` | show interfaces | `/interfaces` |
| `show interfaces physical <ifname>` | show interfaces | `/interfaces/*` |
| `show interfaces physical <ifname> detail` | show interfaces | `/interfaces/*` |
| `show interfaces physical <ifname> sriov` | show interfaces | `/interfaces/*` |
| `show interfaces physical <ifname> statistics` | show interfaces | `/interfaces/*` |
| `show lldp neighbors` | show lldp | `/protocols/lldp/neighbors` |
| `show lldp neighbors interface <ifname>` | show lldp | `/protocols/lldp/neighbors` |
| `show log audit` | show log | `/audit-logs` |
| `show log system` | show log | `/system/logs` |
| `show nat` | show nat | `/nat`, `/nat/sessions` |
| `show port-mirroring` | show port-mirroring | `/port-mirroring` |
| `show protocols lldp neighbors` | show protocols | `/protocols/lldp/neighbors` |
| `show qos policies` | show qos | `/qos/policies` |
| `show resource-pools` | show resource-pools | `/resource-pools` |
| `show system api tokens` | show system | `/system/api-tokens` |
| `show system configuration sessions` | show system | `/system/configuration/sessions` |
| `show system core-dumps` | show system | `/system/core-dumps` |
| `show system cpu` | show system | `/system/status` |
| `show system firewall` | show system | `/system/firewall` |
| `show system hardware` | show system | `/system/hardware` |
| `show system hugepages` | show system | `/system/hugepages` |
| `show system kernel` | show system | `/system/kernel` |
| `show system memory` | show system | `/system/status` |
| `show system storage` | show system | `/system/status` |
| `show system tech-support` | show system | `/system/tech-support` |
| `show system uptime` | show system | `/system/status` |
| `show tech-support` | show tech-support | `/system/tech-support` |
| `show users` | show users | `/system/login-users` |
| `show version` | show version | `/system/version` |
| `show virtual-machine-functions` | show virtual-machine-functions | `/virtual-machine-functions` |
| `show virtual-machine-functions <name> detail` | show virtual-machine-functions | `/virtual-machine-functions/*` |
| `show virtual-machine-functions <name> interfaces` | show virtual-machine-functions | `/virtual-machine-functions/*` |
| `show virtual-machine-functions <name> snapshots` | show virtual-machine-functions | `/virtual-machine-functions/*/snapshots` |
| `show virtual-machine-functions <name> statistics` | show virtual-machine-functions | `/virtual-machine-functions/*` |
| `show virtual-switches` | show virtual-switches | `/virtual-switches` |
| `show virtual-switches <name>` | show virtual-switches | `/virtual-switches/*` |
| `show virtual-switches <name> detail` | show virtual-switches | `/virtual-switches/*` |
| `show virtual-switches <name> dhcp-leases` | show virtual-switches | `/virtual-switches/*/dhcp-leases` |
| `show virtual-switches <name> mac-table` | show virtual-switches | `/virtual-switches/*/mac-table` |
| `show virtual-switches <name> ports` | show virtual-switches | `/virtual-switches/*/ports` |
| `show virtual-switches <name> statistics` | show virtual-switches | `/virtual-switches/*` |
| `show vm-image-profiles` | show vm-image-profiles | `/vm-image-profiles` |
| `show vm-image-profiles <image>` | show vm-image-profiles | `/vm-image-profiles` |
| `show vpp` | show vpp | `/vpp/status` |
| `show vpp buffers` | show vpp | `/vpp/status` |
| `show vpp memory` | show vpp | `/vpp/status` |
| `show vpp threads` | show vpp | `/vpp/status` |
| `show vrfs` | show vrfs | `/vrfs` |
| `show vrfs <name>` | show vrfs | `/vrfs/*` |
| `show vrfs <name> routes` | show vrfs | `/vrfs/*/routes` |
| `show vxlan tunnels` | show vxlan | `/vxlan-tunnels` |
| `traceroute <host>` | traceroute <host> | `/diagnostics/traceroute` |
| `| compare` | | | `/configuration/diff` |
| `| compare rollback <n>` | | | `/configuration/diff`, `/configuration/rollback/*` |

## C. CLI-only by design（守护例外表：REPL 交互形态 / CLI 侧文本渲染，不需要 API）

| CLI 命令 | 理由 |
|---|---|
| `?` | 上下文补全（按键即时）；Web 用表单与静态候选 |
| `annotate` | REPL 注释便利 |
| `edit` | 配置模式层级导航；数据操作已被 candidate API 覆盖 |
| `exit` | CLI 本地行为 |
| `help` | REPL 帮助；Web 用表单与静态候选 |
| `help <command>` | REPL 帮助；Web 用表单与静态候选 |
| `monitor interfaces <ifname>` | 决策 #92：CLI 轮询形态；Web 等价物是定时刷新 + GET /events 推送 |
| `monitor vnf <name>` | 决策 #92：同上 |
| `quit` | CLI 本地行为 |
| `run` | 配置模式内执行便利；被运行的命令本身都有端点 |
| `show log vnf <name>` | 指引型命令（指向容器 log / VM console），等价物已存在 |
| `show vpp runtime` | 决策 #200：数据源是本机 VPP stats segment（vpp_get_stats），不经 HTTP 端点；Web 等价形态是 /vpp/status |
| `show vpp runtime thread <id>` | 决策 #200：同上（thread 只是本机过滤参数） |
| `show | display set` | CLI 文本渲染；契约已登记未实现（决策 #84④） |
| `start shell` | 本地控制台 shell，Web 无对应形态 |
| `top` | 配置模式层级导航；同上 |
| `up` | 配置模式层级导航；同上 |
| `wizard` | 决策 #107：CLI 端交互编排，明确无 API 端点（Web 等价物是向导式页面） |
| `| begin` | 同上 |
| `| count` | 同上 |
| `| display json` | CLI 渲染；Web 原生消费 JSON |
| `| display xml` | CLI 渲染；Web 原生消费 JSON |
| `| except` | 同上 |
| `| last` | 同上 |
| `| match` | CLI 文本过滤；Web 以前端过滤 + 分页查询参数实现 |

## D. **本矩阵的局限（必读）**

**端点级已接 ≠ 界面级可用。** 本矩阵只能回答「Web 调不调这个端点」，回答不了
「操作者在界面上有没有一个控件做这件事」。两者差得最远的是 **`set` / `delete` 族**：
按 REST 事务模型，**所有配置语句都由 `PUT/DELETE /configuration/candidate` +
`POST /configuration/commit` 架构性覆盖**（这正是 CLI 语句在 REST 里的对应物），
所以它们在本矩阵里全部记为「已接」——但界面上有没有**对应的表单字段**，本矩阵看不见。
举例（人工核验）：`set interfaces <if> port-security mac <m>` 与 `monitor vnf` 在
`internal/api/ui/js/**` 里**一处引用都没有**，却不会被本矩阵判为缺口。

**结论**：要回答「Web 是否实现了 CLI 的全部功能」，必须在端点级之上再做一层
**界面级审计**（下一节）。

## E. 下一步：界面级审计（建议做法）

1. **配置语句族**：把 `internal/schema/tree_config.go` 的命令树叶子（`set`/`delete` 全部
   语句）与 `internal/api/ui/js/views/config.js` 里各节的 `fields[].path` / `sublists`
   逐条对照——每个叶子要么有对应控件，要么明确标注「只支持原始配置文本框」。
2. **动作族**：`request` 族按本矩阵 A 节逐条确认界面入口（按钮/表单）。
3. **读族**：`show` 族按页确认展示位置（列表/详情/诊断页）。
4. 产出物：在 **A 节基础上加一列「界面控件」**（有/无/仅原始配置），即真正的排期清单。

## F. 已知界面级缺口（人工核验，供审计起点）

| CLI 命令 | 端点级 | 界面级 | 证据 |
|---|---|---|---|
| `set interfaces <ifname> port-security mac <mac>` | 已接（candidate） | **无** | `port-security` 在 `ui/js/**` 零命中 |
| `monitor vnf` | CLI-only（例外表） | **无** | `monitor` 在 `ui/js/**` 零命中 |

