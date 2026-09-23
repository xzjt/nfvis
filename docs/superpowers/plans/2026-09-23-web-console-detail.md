# Web 控制台「详情刀」实施计划（对象详情页取代共享浮层）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 给每个对象一个**独立的、可深链的详情页**（取代 round51~55 的共享浮层），把三件有状态的功能（串口 console / VM 快照 / 容器日志）搬进页面，并让列表行可点进详情。

**Architecture:** 在既有骨架上加**参数化路由**（`#/compute/vms/:name`）：`routes.json` 用 `{name}` 占位（与 OpenAPI 的路径参数同名），router 匹配后把 `params` 传给 `VIEWS[view].render(data, params)`；详情页用 Tab 切「概览 / 接口 / 快照 / 串口」，Tab 也进 hash（`#/compute/vms/vnf-a?tab=snapshots` 之类不做——**Tab 用页内状态**，避免 URL 过长；只保证对象级深链）。

**Tech Stack:** 同骨架刀（原生 ES modules、免构建、产物入库；Go 守护 + Browser Use 操作验收）。

**Spec:** `docs/superpowers/specs/2026-09-23-web-console-ia-design.md` §5.3（详情页模板）、§11.2（验收口径）、§12 刀 2。

## Global Constraints

- 免构建、同源托管、不新增服务端路由（同骨架刀）。
- `routes.json` 新增的端点必须**逐字**匹配 OpenAPI 路径（含 `{name}`），由 `TestUIRoutesEndpointsExistInContract` 守护。
- 串口/快照/日志三件功能**不重写**：复用既有 `vmConsoleOpen/vmConsoleSend/vmConsoleClose`、`vmSnapLoad/vmSnapCreate/vmSnapAct`、`ctLogsLoad`，只把它们的容器从浮层改到详情页的 Tab 面板。
- 详情页的**生命周期动作按钮沿用既有状态判定**（`VM_ACTIONS` 的 `states`），不新写规则。
- 破坏性动作的确认强度本刀**不动**（留刀 4）。
- 每个功能都要在浏览器里真的操作一遍（决策 #141）：列表行 → 详情页、Tab 切换、串口真连、快照列表与"运行中拒绝创建"、容器日志真拉、深链直达与刷新保持。

## 新增路由（routes.json）

| 路由 | view | endpoints |
|---|---|---|
| `#/compute/vms/:name` | vmDetail | `/virtual-machine-functions/{name}`、`/virtual-machine-functions/{name}/snapshots` |
| `#/compute/containers/:name` | containerDetail | `/container-functions/{name}` |
| `#/compute/images/:name` | imageDetail | `/images/{name}` |
| `#/network/vrfs/:name` | vrfDetail | `/vrfs/{name}`、`/vrfs/{name}/routes` |
| `#/network/acls/:name` | aclDetail | `/acls/{name}` |
| `#/network/bonds/:name` | bondDetail | `/bonds/{name}` |
| `#/network/qos/:name` | qosDetail | `/qos/policies/{name}` |
| `#/network/span/:name` | spanDetail | `/port-mirroring/{name}` |

## Task 1: 参数化路由（router.js + 守护）

- [ ] `match(hash)` 支持 `:name`：把 `#/compute/vms/vnf-a` 匹配到 `#/compute/vms/:name` 并取出 `params.name`（`decodeURIComponent`）；**静态路由优先于参数化路由**（避免 `#/network` 被 `#/network/:x` 抢走）。
- [ ] `VIEWS[view].render(data, params)` 增加第二个入参；未使用 params 的 view 不受影响。
- [ ] 守护：`ui_routes_test.go` 增加"参数化路由的 path 段必须以 `:` 开头且唯一"；`TestUIRoutesEndpointsExistInContract` 把路由表里的 `:name` 归一成契约的 `{name}` 再比对（**归一映射写在测试里并注释**）。

## Task 2: 详情页（VM 优先，含三个 Tab）

- [ ] `index.html` 新增 `<section class="page" id="page-vmDetail" hidden>`：对象头（名称/状态/镜像/资源）+ Tab 条（概览/接口/快照/串口）+ 三个面板容器（`#vm-tab-overview`、`#vm-tab-ifaces`、`#vm-tab-snapshots`、`#vm-tab-console`）。
- [ ] 把现有浮层内容搬进来：快照面板（列表/创建/删除/回滚）与串口面板（连接/发送/断开）**DOM 原样搬**，只改它们所在容器 id 与显示方式（浮层 → Tab 面板）；浮层壳（`.panel-overlay`）与对应 CSS 删除或改为页内样式。
- [ ] 概览 Tab：状态/资源/统计（`statistics` 的 vhost-user 计数）+ 生命周期按钮（启停重启，按 `VM_ACTIONS` 启用）。
- [ ] 接口 Tab：该 VM 的 vNIC 列表（`interfaces[]`：类型/虚拟交换机/计数）。
- [ ] Tab 切换用页内状态（`location.hash` 不变），默认停在「概览」；进入页面时若带 `?tab=` 不做支持（保持简单）。
- [ ] 列表页 `#/compute/vms` 的**行可点**（点击行 → `#/compute/vms/<name>`；行内动作按钮保持 stopPropagation，不误触）。
- [ ] 离开详情页时若串口连着 → 断开（复用 `vmConsoleClose`），避免 WebSocket 泄漏。

## Task 3: 容器 / 镜像 / 网络对象详情页

- [ ] 容器详情页：对象头 + Tab（概览 / 日志）；日志复用 `ctLogsLoad`。
- [ ] 镜像详情页：完整元数据（sha256/format/size/ref_count/import_state/imported_at）。
- [ ] 网络对象详情页（vrf/acl/bond/qos/span）：把共享浮层 `objDetail` 的内容做成页面；VRF 详情页额外给**路由表**（复用 `bigRoutes` 的取数，按需拉取）。
- [ ] 各列表页行/「详情」按钮改为 `navigate('#/…/<name>')`；共享浮层与 `objDetail()` 保留给"暂无独立页"的对象（若有），否则删除并清理调用点。
- [ ] 列表页与详情页之间可返回（面包屑已可点；列表行不重复实现"返回"按钮）。

## Task 4: 验收与收尾

- [ ] `make check` 全绿；`node --check --input-type=module` 逐个新/改 JS 文件。
- [ ] 浏览器操作验收（决策 #141）：VM 详情（4 个 Tab 逐一点）、容器详情（日志真拉）、镜像/网络对象详情各一个、深链直达 + 刷新保持、离开页面串口自动断开（在页面里断言 WebSocket 已关）。
- [ ] 快照：在开发态实例里建一台探针 VM（关机态）→ 创建快照 → 列表可见 → 删除；对运行中的 `vnf-a` 点"创建快照"→ 断言界面显示"需关机态"的拒绝（与 CLI 同口径）。
- [ ] 证据文件 `docs/evidence/v1-closeout-round58-web-console-detail.txt` + 手册 §10.12 补"详情页与 Tab" + 交接 §0 记一笔。
