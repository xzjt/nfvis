# v3 · Web 控制台对齐 CLI 全部功能 —— 现状、缺口排期与配置页重构设计

> 用途：回答「Web 控制台还差多少、按什么顺序补、配置页怎么改」。
> 生成依据：三份真源审计（见 §1），全部取自**代码**而非文档（文档会滞后）。
> 日期：2026-10-16 ｜ 线上构建：`3.0.21~dev2`（含 R37-1 修复 + 本轮两处新增界面）。

---

## 0. 结论先行

**能力层（REST）已基本齐；缺口在界面层。** 三层口径：

| 层 | 口径 | 现状 |
|---|---|---|
| ① 端点 | CLI 命令 → REST 端点 | **146 条有端点，守护表 0 缺口**（`docs/v3-Web-CLI覆盖矩阵.md`） |
| ② 端点级界面接入 | Web 是否调用该端点 | **142 / 146**（余 4 条是兼容别名，Web 走新名，不需另做） |
| ③ **界面级**（有没有控件/入口） | 操作者在界面上能不能做 | **配置语句 197 叶子：151 有控件 / 46 只有原始 JSON；动作 52 条：47 有入口 / 3 无入口；读 84 条：70 有入口 / 10 部分** |

**所以「差得远」差在③。** ②之所以看着很高，是因为 `set`/`delete` 全族按 REST 事务模型由
candidate API 架构性覆盖，端点级一律记「已接」——但界面上有没有对应控件，那一层看不见。

**⚠️ 一个必须记住的口径**：配置页那个「原始配置」文本框实际是 **JSON 编辑器**（`index.html`
标注「原始 JSON（整体替换 candidate）」），**不是 CLI 语句框**。所以「46 条只有原始 JSON」
的真实含义是「**只能手工写 candidate JSON，绕过表单的本地校验**」——这比「有个地方能手输」
要严重。

---

## 1. 三份真源审计

| 文档 | 覆盖 | 关键数字 |
|---|---|---|
| `docs/v3-Web-CLI覆盖矩阵.md`（生成物） | CLI→REST→Web 端点级 | 146 / 142 / 4 别名；REST 0 缺口 |
| `docs/v3-Web界面覆盖审计-配置族.md` | `set`/`delete` 全部语句 | 叶子 **197**：有控件 151 / 仅原始 JSON 46 / 无入口 0 |
| `docs/v3-Web界面覆盖审计-动作与读族.md` | `request` + `show` 族 | `request` 52：47/2/3；`show` 84：70/10/0（+4 设计如此） |

生成器（可复现）：`contrib/scripts/web-cli-coverage-matrix.py`；
审计文档内含各自的枚举脚本与数法说明。

---

## 2. 缺口排期（建议批次）

### P0 —— 阻断类：3 条「界面上删不掉对象」——**本轮已补**

`request virtual-machine-functions <n> delete`、`request container-functions <n> delete`、
`request system core-dumps delete`。端点都在、此前**界面缺控件**：VM/容器列表与详情的动作区只挂
「启动/停止/重启」；运维动作页的 core dump 只有「列出/导出清单」。

**已实现**：VM 列表行内 + 详情页「删除虚拟机」；容器列表行内 + 详情页「删除」；运维动作页
core dump 每行「删除」。三处均经 `wbtn`（`data-write`）门禁 + **中危确认框**（列影响面 + CLI
回显 + 取消/确定）。真机实测（`3.0.21~dev5`）：造临时容器 `del-probe` → 界面上走「删除 → 确认框
→ 确定」→ `已删除 del-probe。`；机器侧独立核实（产品视图无、`docker ps -a` 无、审计落
`config.commit` 带 diff，与 CLI `commitMutate` 同口径）；core dump 删除亦留审计
（`system.core-dumps.delete`）。现场已还原。

### P1 —— 配置控件缺口（46 条里价值最高的族）

**第一批已落地（本轮）**——三族 15 条叶子：

| 族 | 落点 | 覆盖的 CLI 叶子 |
|---|---|---|
| **主机防火墙** | 配置页新增「系统 · 主机防火墙」节 | `firewall default-policy` + `firewall rule <seq>`（子表：action/source/protocol/port） |
| **VXLAN** | 配置页新增「VXLAN overlay」节 | `tunnels <n>` 的 `vni` / `local` / `remote` / `dst-port` / `virtual-switch` |
| **DHCP 服务器与中继** | 并入现有「虚拟交换机」节 | `dhcp-relay server`、`dhcp-server pool` / `lease-time` / `dns` / `domain-name` |

校验口径全部取自服务端实现（`internal/model/validate.go`）并做成前端提示，**最终以服务端 `detail[]` 为准**；
含本地互斥组（DHCP 中继 ⇄ 服务器）。真机实证：6 条服务端拒绝文案逐条命中（防火墙裸 action、
同/跨交换机 DHCP 互斥、L3 交换机配 relay、L3 交换机作 VXLAN 成员、租约越界）；
防火墙提交须 `commit confirmed`（400 `CONFIRM_REQUIRED`）——与 FR-CFG-012 一致。

**剩余待补（第二批，本轮已补完）**——26 条叶子：

| 族 | 落点 | 覆盖的 CLI 叶子 |
|---|---|---|
| **数据面 DNS 代理** | 新节「系统 · 数据面 DNS 代理」+「虚拟交换机」节的按域上游 | 全局主/备 + 每交换机主/备（4） |
| **入向风暴抑制** | 「接口」节每口广播/组播（kbps） | `storm-control broadcast\|multicast`（2） |
| **环路缓解** | 「虚拟交换机」节 MAC 学习上限 | `learn-limit`（1） |
| **VM 启动画像 + PCI 直通** | 「虚拟机」节 `startup` 9 项 + `pci_devices` | `startup *` + `pci-device`（10） |
| **镜像启动画像** | 新节「镜像启动画像」 | `vm-image-profiles *`（9） |

真机独立事实源：VM 启动画像下发后用 `virsh dumpxml` 逐项印证（machine/firmware/boot/磁盘总线/
console/`<qemu:arg>`/hostdev PCI，并补证 `domain type=qemu`=tcg 与 `cpu mode=custom` 型号）。

> **至此 P1 收口**：审计里「46 条只能手工写 candidate JSON」的配置语句叶子全部补完。
> 注：**端口安全**、**防火墙/VXLAN/DHCP** 分别在前两批已补。

### P2 —— 读类补全（10 条「部分覆盖」）

`show interfaces management`（缺管理口 IP）、`show interfaces <if> detail`（契约有 `mac`/
`numa_node`，前端零渲染）、`show acls <name>[ detail]`（规则表**没有 action 列**）、
`show vpp threads`（只给线程数）、`show vm-image-profiles`（无汇总列表）、
`show configuration permissions <class>[ detail]`、`show configuration compare rollback <n>`
（「看差异」只弹引导、不显示差异）。

> 这批要**先定裁**：是产品有意取舍还是漏展示？建议按决策 #344 的口径逐条契约如实化。

### P3 —— 配置页去中心化（你已裁定，见 §3）—— **四刀已全部落地**

### P4 —— 守护补强

- **已修（本轮）**：`show system metrics history` 此前**既不在** `cliRESTCoverage` **也不在**
  `contractCLICommands`——树里有、端点也有（`server.go:396` 第三参数就写着它），两张守护表
  都没收。已补进覆盖表（`GET /metrics/history`）。
- **结构性空洞已关闭（本轮）**：新增 `internal/api/cli_tree_coverage_test.go` 的
  `TestCLITreeLeavesAreClassified`——**直接遍历命令树**（`schema.OperRoot()` + 配置树）而不是
  只看手工清单，逐条断言每个叶子都落进 `cliRESTCoverage` / `cliRESTExceptions` / `cliRESTGaps`
  之一，未归类即逐条点名报红。首次运行**揪出 60 条未归类**（操作树 55 + 配置树 5），已逐条
  核实后归类：覆盖表 +58（端点全部从 `server.go` 现读）、例外表 +2（写明理由）、**真缺口仍为 0**。
  口径与数字（实测）：操作树 **177** 片形态 / 配置树 **19** 片形态 / 配置语句 **196** 条，
  未归类 0。另修掉 `contractCLICommands` 里 `show vpp capture` 的重复项。
  **独立红-绿已验**：摘掉 `show system metrics history` 一行 → 报红并点名声明确切；恢复即绿。

---

## 3. 配置页去中心化 —— 设计

### 3.1 目标形态（你的裁定）

**取消集中配置页。** 每个对象的增删改回到**它自己的列表页/详情页**；「配置」页只保留
**原始配置（JSON）编辑 + 预校验 + 查看差异 + 提交**，作为高级通道兜底。

### 3.2 现状与可复用的东西

配置页现在是**一页渲染三组节**（`CFG_SYS_SECTIONS` + `CFG_VPP_SECTIONS` + `CFG_SECTIONS`），
节定义里每个字段是 `{ label, path: [...], kind }`，子表是 `sublists: [{ list, key, fields }]`，
并且每节已支持 `add`（新增对象）。**写路径是现成的**：`PUT /configuration/candidate`（取锁+写
候选）→ `POST /configuration/check` → `POST /configuration/commit` → 释放会话。

**所以这不是重写，是「把节定义从一页拆到各自的对象页」。** 表单引擎本身可原样复用。

### 3.3 分步（每步都能独立验证，避免大爆炸）

1. **抽出表单引擎为可复用模块**（纯搬运，零行为变更）：把节渲染/写路径从 `views/config.js`
   抽到 `js/core/forms.js`，`config.js` 改为调用它。**验收：现有前端自校准 + 真机配置页行为逐字不变。**
2. **网络对象族落地（本轮已完成）**：交换机 / VRF / ACL / NAT / QoS / 聚合 / 端口镜像 / VXLAN —— 各自列表页加
   「+ 新建」，详情页加「编辑 / 删除」。写路径复用 `forms.js` 的 candidate 事务；新建/编辑/删除
   均走中危确认（列影响面 + CLI 回显）。
   **真机 CRUD 三步走通**（`3.0.21~dev9`）：新建 `vs-probe` ⇒ 产品视图见 `28429 vs-probe`、
   `vppctl show bridge-domain` 见 `BD 28429`；编辑 ⇒ `revision 85`；删除 ⇒ `revision 86`，
   两侧视图里 `28429` 均消失、`grep -c probe` = 0。
3. **计算族落地（已完成）**：VM 列表「+ 新建」+ 详情「编辑/删除」；容器列表/详情「编辑/删除」（创建指向部署向导）；镜像列表「画像」+ 详情「编辑画像/删除」。真机 CRUD 三步走通（`virsh dumpxml` 确认 `<vcpu placement='static'>2</vcpu>`、容器 docker 重建、画像三方一致）。
4. **系统族落地（已完成）**：`#/system`（系统基础+NTP+DNS 代理+日志+健康阈值+管理口+API/TLS+防火墙）、`#/system/kernel`、`#/system/users`、`#/system/interfaces/:name`、`#/system/pools`、`#/ops/actions`（VPP 段）。真机逐页「改→提交→机器侧读数→改回」，宿主四项已还原。
5. **配置页瘦身（已完成）**：配置页只剩「原始 JSON 编辑 + 完整提交链七件」；节表单接线与 `#cfg-forms` 容器已删，**节定义留在原处、内容一字未改、继续导出**（对象页/系统页仍 import 它们）。**继承的约束**：`cfgRenderForms` 及其助手**不能删**——`web-console-config-selftest.js` 的桩沿 bundle 顶层符号直接调用它（改名即整份报 ReferenceError，已实证），该约束写在代码注释里。

> **整合副产物（值得记）**：第二批补的 DHCP/DNS/learn-limit 等字段是加在**节定义**上的，因此
> **自动出现在对象页的就地编辑面板里**——「同一份节数据，两个入口渲染」，这正是引擎抽取的收益。

### 3.4 必须一并想清楚的两点

- **编辑锁与并发**：现有模型是「取锁 → 编辑候选 → 提交」。就地编辑也要走同一套语义——
  进入某个对象的编辑态即取锁、提交或取消即释放；否则会出现「两个页各改一半候选」的错乱。
  决策 #317/#318 的会话锁语义是现成的，**复用它，不要另造**。
- **角色门禁**：就地编辑的写控件必须经 `wbtn`（`data-write`），super-user 级动作加 `data-op`；
  并同步 `internal/api/ui_ops_writes_gating_test.go` 的控件清单（该守护会从 `server.go` 现读端点
  class 交叉核对，漏登记即红）。

---

## 4. 本轮已落地

| 项 | 内容 | 状态 |
|---|---|---|
| R37-1 | `js/core/dom.js` 动态 import 路径 `../router.js` → `../../router.js`（**详情按钮/行点击全站失效**的根因） | 已修 + 真机复验 |
| 守护 | `web-console-esm-link-check.mjs` 新增 **DYN** 层（解析动态 `import()` 路径），红-绿验证 | 已加 |
| 覆盖矩阵 | `docs/v3-Web-CLI覆盖矩阵.md` + 生成器 `contrib/scripts/web-cli-coverage-matrix.py` | 已产出 |
| 界面审计 | 配置族 + 动作/读族两份审计 | 已产出 |
| 守护空洞 | `show system metrics history` 补进 `cliRESTCoverage` | 已补 |
| **接口详情 · 端口安全** | 白名单列表 + 添加/删除 MAC（走 candidate 事务、`wbtn` 门禁、确认框） | 已实现 + 真机复验 |
| **运维 · 实时监视页** | 新增 `#/ops/monitor`（VM/容器状态，SSE 推送 + 定时兜底） | 已实现 + 真机复验 |
| **P0 · 三个删除入口** | VM / 容器 / core dump 三处删除（中危确认框 + CLI 回显；经 `wbtn` 门禁） | 已实现 + 真机复验 |
| **P1 第一批 · 三族控件** | 主机防火墙 / VXLAN / DHCP 服务器与中继（15 条叶子） | 已实现 + 真机复验 |
| **表单引擎抽取** | `views/config.js` 的引擎抽到 `js/core/forms.js`（1894→1201 行；零行为变更），为去中心化铺路 | 已抽取 + 守护全绿 |
| **P4 · 树遍历守护** | `cli_tree_coverage_test.go`：遍历命令树强制逐条归类（操作树 177 / 配置树 19 / 语句 196） | 已实现 + 独立红-绿 |

**未验证（如实）**：端口安全在真机上的**成功提交**未走通可视路径——现场唯一 L2 成员口 ens192
已配 storm-control（与之单槽互斥，服务端正确拒绝），UI 只走到「确认 → 事务 → 校验拒绝 → 如实
显示错误」；成功提交由 REST 路径验证（rev 32 + 读回）。实时监视页未验证「SSE 事件到达时即时
刷新」的动态过程。

---

## 4.5 本轮顺带抓出的**产品缺陷 R38-1**（已修复并真机复验）

**现象**：只要某个虚拟交换机带 `description`，**整条 `show configuration | display set` 整体失败**
（不是少一行，是整份配置读不出来）：

```
%% display set 内部错误：生成语句回放失败（未知语句: "description"）
```

**一条 Web 侧编辑就能让 CLI 的全局读命令失效**——而 Web 的「虚拟交换机」表单**恰恰提供「描述」字段**。

**根因**：`model.VirtualSwitch` 有 `Description`（`model.go:341`）；命令树的 `virtual-switches`
节点没有 `description` 叶子（而 `interfaces`/`bonds`/VM/容器都有）；发射器不发射它、留给回放校验报错。

**修复时发现更深一层**：仓内那些 `w.note(...)` 先例（ACL 描述、口令哈希、空 gateway、serial
console 关闭态）**全是半成品**——回放自校验只剥敏感值与 `enabled=true`，**从不剥「不入 display set」
的字段**，所以**每个被注释的字段照样把整条命令打回**（实测：连 ACL 描述本身也失败）。
故修法不只是把一个字段改成 note，而是**把「已注释 ⇒ 两侧同剥」补成真机制**（`stripNonEmittable`）。

**影响面（比初判大）**：已修 **6 项**，其中**两项是 CLI 自己就能造出来的**——
`virtual_switches[].gateway={}`（`delete … gateway ip` 留空对象）与
`virtual_machine_functions[].serial_console=false`（`delete … serial console enable`）⇒
**不用 Web，光用 CLI 也可能触发本缺陷**。

**真机复验**（`3.0.21~dev10`，主会话亲造现场 + 单测红-绿）：用 Web 的候选路径给 `vs-lan` 加
`description` ⇒ `display set` **成功**并输出 `# virtual-switches vs-lan：描述字段无语句关键字，
不入 display set`；换 `acl-lab` 的 `description` 复测 ⇒ 同样成功 + 对应注释（证明机制对所有字段
生效）；探针字段已清、`grep -c 内部错误` = 0、现场完好。

**新登记 2 处「静默丢字段」**（同症状但**无注释**，属发射器疏漏，需产品口径三选一定裁：补语句形态
（契约先行）/ 提交期拒绝 / 注释放行）：`ports[]` 的 vnf 成员 + `native` VLAN；
`vrfs[].l3_interfaces[].vlan`。

> 详细定位、同类清单与复验证据见 `docs/evidence/v3-round38-web-alignment-batch2.txt` §5/§6。

---

## 5. 验收口径（建议钉住）

1. **三层数字都要能复现**：端点级跑生成器；界面级跑两份审计里的枚举脚本。
2. **界面级审计必须含「真机点一遍」**：静态判定看不见「控件在但点了没反应」（R37-1 就是这么
   逃掉的）。建议对每个新增入口至少做一次 Browser Use 黑盒点击。
3. **守护**：`web-console-esm-link-check.mjs` 三层（LINK/XREF/DYN）全绿 + 两个前端自校准 +
   `ui_ops_writes_gating_test.go`（新增写控件必须入册）。
