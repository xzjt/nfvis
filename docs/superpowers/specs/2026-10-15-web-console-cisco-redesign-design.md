# NFViS Web 控制台重构设计（参考 Cisco NFVIS 门户）

日期：2026-10-15　状态：已获用户方向批准（范围=重构+部署向导；vanilla JS 分模块；浅色主题；按 Cisco 分组重排；守护测试全部适配保留；单次重构方案）

## 1. 背景与目标

当前 Web 控制台是顶栏平铺导航 + 深色运维风的 vanilla JS 单页（app.js 6300 行单文件、index.html 1100+ 行、style.css 18K）。参考 Cisco NFVIS Getting Started Guide 的门户信息架构（左侧导航树、以「VM 生命周期」为中心、镜像库/部署向导/生命周期管理、浅色主题），重构为本仓库的 Web 控制台。

**目标**：

1. 布局与信息架构对齐 Cisco NFVIS 门户：左侧可折叠导航树、浅色主题、以虚拟机生命周期为中心的分组。
2. 新增「部署向导」页：Cisco 式分步流（选类型 → 选镜像与资源 → 网络映射 → 摘要确认 → 进度），全部走现有 REST 端点（`POST /virtual-machine-functions` 写候选 + 既有提交/确认管线），不新造服务端端点。
3. app.js 按域拆成 core + views 模块，消除 6300 行单文件。
4. 16 个 Go 守护测试文件全部适配保留（权限门禁、字段级对账、路由一致性的守护价值不回退）。
5. 气隙纪律不变：无外部字体/图标/CDN 依赖、无构建步骤、浏览器直接加载 ES modules。

**非目标**：不改任何 REST 端点与服务端行为；不做前端框架/构建链；不做拖拽交互（降级为点击选择，Cisco 拓扑图以静态 SVG 预览替代）；不新造命令树/CLI 变更；不改路由语义（hash 路由、routes.json 前后端共用真源的机制保留）。

## 2. 现状事实（已核实）

- 路由真源 `internal/api/ui/routes.json`：32 条路由，每条含 path/title/view/breadcrumb/endpoints/poll/write/detail。前端 router.js fetch + JSON.parse；Go 侧 `ui_routes_test.go` 用真解析器守护。
- 页面容器模式：index.html 每路由一个 `<section class="page" id="page-<view>">`，router 按 `#page-<view>` 显隐。
- 视图注册表：app.js `export const VIEWS`（33 个 view），render(data, params) 模式；`NET_OBJECT_VIEWS` 数据驱动网络对象卡。
- 权限门禁机制：`wbtn()` 写按钮带 `data-write`；`body.role-readonly`/`role-nonsuper` class；`ui_ops_writes_gating_test.go` 钉「控件 id → 期望 class → 服务端 class」交叉核对；`ui_coverage_test.go` 路径级覆盖；`ui_qos_card_fields_test.go`/`ui_span_card_fields_test.go`/`ui_switch_ports_fields_test.go` 字段级守护；`ui_tabs_test.go` 分栏一致性。
- VM 创建：`POST /api/v1/virtual-machine-functions` → `model.VMFunction`（name/image/vcpu/memory/disks/interfaces/cloud_init/autostart/startup）写候选；`mutateCandidate` 返回 201。
- 镜像与画像：`GET /images`、`GET /vm-image-profiles`（决策 #485）。
- 提交管线：既有「配置页候选 → commit（可选 confirmed）」全流程，向导只写候选不代提交。

## 3. 布局与导航设计

```
┌──────────────────────────────────────────────────────────────┐
│ 顶栏: NFViS 控制台 │ 面包屑        │ 实时通道 · 用户 · 我的账号 · 刷新 · 退出 │
├──────────┬───────────────────────────────────────────────────┤
│ 左侧树    │                                                   │
│ ▾ 仪表盘  │              内容区（一次一页，卡片栅格沿用）          │
│ ▾ 虚拟机  │                                                   │
│ ▾ 网络    │                                                   │
│ ▾ 系统    │                                                   │
│ ▾ 运维    │                                                   │
└──────────┴───────────────────────────────────────────────────┘
```

- **分组（按 Cisco 重排）**：
  - 仪表盘：总览（#/）
  - 虚拟机（VM Life Cycle）：镜像库（#/compute/images）、部署向导（新 #/deploy）、虚拟机（#/compute/vms）、容器（#/compute/containers）
  - 网络：虚拟交换机、网络对象、LLDP
  - 系统：系统（#/system 汇总页）、资源池、接口、硬件健康、历史趋势、内核基线、用户与权限、证书
  - 运维：运维动作、审计日志、诊断、抓包、配置（#/config）、提交历史、编辑锁会话
  
  分组名与顺序落为 routes.json 新增 `"group"` 列；`#/config` 家族归入运维组（配置编辑是写路径，Cisco 门户无对应项，归运维最贴切）。
- routes.json 只增列不改语义：现有 path/title/view/breadcrumb/endpoints/poll/write/detail 全部保留；Go 守护测试同步适配新列（group 必填校验、nav 校验）。
- 详情页（detail: true）不进导航树（现状口径），在树中高亮其列表项。
- 左侧树两层级联：组头可折叠，展开状态存 sessionStorage；当前路由项高亮（沿用 aria-current）。
- 顶栏保留：实时通道状态、用户名、「我的账号」（全角色可见的既有纪律）、刷新、退出。移动端/窄屏：左侧树可折叠为汉堡抽屉（纯 CSS class 切换）。
- 角色门禁：read-only 不列写页（write: true 过滤，#145 口径）；role-notice 文案机制不变；`body.role-nonsuper` 切换、`data-write`/`wbtn` 机制原样保留，只挪位置。

## 4. 文件与模块结构（方案一：单次重构）

```
internal/api/ui/
  index.html          # 重写：顶栏 + 左侧树 + 内容区骨架；.page 容器模式保留（新增 page-deploy）
  style.css           # 重写：浅色主题（Cisco 蓝白），CSS 变量换套；布局类重写；组件类（卡片/表格/表单/对话框/pill/代码块）对齐新主题
  router.js           # 保留为调度中枢：renderNav 改为渲染左侧树（含折叠）；其余逻辑不动
  routes.json         # 增列 group + 新增部署向导路由
  vncview.js          # 不动
  novnc/              # 不动
  js/
    core/             # 共享件（从 app.js 抽出，零业务知识）
      dom.js          #   $、el、fill、table、dash、list、wbtn、uiDialog/uiConfirm（CONFIRM_COUNTDOWN）
      api.js          #   api()、soft、apiErrText、httpErrText、rowsOf、pageWarn、API/TOKEN_KEY 常量
      format.js       #   uptime/bytes/pct/mb/fmtTime/fmtClockHint/portSecText/stormControlText
      state.js        #   token/user、SSE 实时通道（startStream/stopStream）、轮询 hook、alarms/events 缓存
    views/            # 每域一文件，export 该域的 VIEWS 片段
      overview.js     vms.js  vmDetail.js  containers.js  containerDetail.js
      images.js       deploy.js（新） network.js  switches.js  lldp.js
      system.js（system/hardware/pools/metrics/interfaces/kernel/users/tls 合一文件）
      config.js       ops.js（ops/audit/diagnostics/capture 合一文件）
    boot.js           # 原 app.js 尾部：登录/enterApp/signOut、事件绑定、window.nfvis、boot()
  app.js              # 缩为聚合入口：import 各 views 片段组装 VIEWS，export 给 router.js（保持 router.js 的 import 不变）
```

拆分规则：

- 模块边界以「路由表 view 名」为准——一个 view 只属于一个文件；跨视图共享的辅助（dialog、format、api）全部下沉 core。
- app.js 聚合入口保持 `export const VIEWS`、`export softLoad/setPollRoute/showGlobalError` 等既有导出签名，router.js 的 import 零改动。
- 拆分是**移动为主**：函数体不改逻辑，只改 import/export 与跨引用；拆分过程中每完成一个视图跑一次 `node --check` + Go 守护测试，防漂移。
- index.html 内联脚本/事件绑定随所属视图迁入对应模块；全局一次性绑定（beforeunload、登录表单）归 boot.js。

## 5. 浅色主题设计

- CSS 变量换套（Cisco NFVIS 门户取色）：
  - 背景 `--bg:#f4f6f9`，面板 `--panel:#ffffff`，面板次级 `--panel-2:#f8fafc`
  - 线条 `--line:#d9dee5`，正文 `--fg:#1a2b3c`，弱化 `--muted:#5f7183`
  - 主色 `--accent:#049fd9`（Cisco 蓝），hover `--accent-hover:#0281b0`
  - 状态色：ok `#03804b`、warn `#9a6a00`、err `#b32d21`（浅色底上加深保证对比度）
- 深色变量保留在 `:root` 注释块（记录换套事实），便于将来做双主题；本轮不做切换按钮。
- 组件级对齐：卡片白底浅阴影、表格条纹线、表单控件浅色、对话框/pill/代码块同步换色；登录页改为浅色卡片居中。
- 无障碍：正文对比度 ≥ 4.5:1（#1a2b3c on #fff ≈ 13:1），状态色文字加深处理。

## 6. 部署向导页设计（新 view：deploy）

Cisco 式分步流，四步 + 进度反馈；全部走现有端点，**只写候选、不代提交**（提交仍在配置页/CLI 完成，符合产品「候选→提交」模型）。

- **Step 1 选择类型**：VM / 容器 两张类型卡（容器为加分项，同一向导复用容器表单，走 `POST /container-functions`）。
- **Step 2 镜像与资源**：镜像下拉（GET /images 实时拉取，含名称/大小/状态）；
  - VM：vCPU 数、内存 MB（提示资源池可用量——GET /resource-pools + /system/hugepages 的既有可分配口径）、磁盘（从镜像克隆或空盘）、serial/autostart 开关；
  - 容器：vCPU/内存/命令/环境变量。
- **Step 3 网络映射**：接口列表（可增删行），每行 name + 类型（vhost-user/memif/sriov-vf）+ 挂接的虚拟交换机下拉（GET /virtual-switches）；Cisco 拓扑预览用**静态 SVG**：左侧画新 VM 图标，右侧列所选交换机/接口连线（只读预览，非编辑器）。
- **Step 4 摘要与提交**：全部字段复核表 + 「写入候选」按钮；成功后展示候选提示（指向配置页提交），失败按字段展示校验错误（服务端 VALIDATION_FAILED 的 detail 数组逐条列）。
- 进度反馈：写入候选是同步请求，无 Cisco 式长任务进度条；用既有 uiDialog/全局错误机制呈现结果。
- 客户端预校验（必填/数字范围/名称规则）与服务端校验**双保险**，但不复制服务端规则全集——客户端只做提示级校验，权威在服务端。
- 权限：页声明 `write: true`（只读角色不进导航；operator 经 class 门禁由服务端裁决）。

## 7. 守护测试适配清单（16 文件全部保留）

| 测试文件 | 适配内容 |
|---|---|
| ui_routes_test.go | 增 group/nav 列校验；deploy 路由的 endpoints 与 write:true 核对 |
| ui_coverage_test.go | 路由↔endpoints↔index.html 容器 id 映射不变，容器新增 page-deploy |
| ui_ops_writes_gating_test.go | 控件 id→class 清单不变；DOM 查询仍指向新 index.html（id 全保留） |
| ui_qos_card_fields_test.go / ui_span_card_fields_test.go / ui_switch_ports_fields_test.go | 字段对账不变；DOM 结构变化处同步更新选择器 |
| ui_tabs_test.go | 分栏一致性（CT_TABS/VM_TABS）不变；新增 deploy 页不涉及 |
| 其余（login_banner/alarm_clock/time/switch_ports/vms 等） | 断言的 id/class/文案保留；布局类名变化处逐一适配 |

适配原则：**id 与数据属性是守护契约**——重构中所有被测试断言的元素 id、`data-write`/`data-op`/`data-nav` 属性、pill/表格 class 语义全部保留；只有纯布局类名可换。适配完成的判据是 `go test ./internal/api/ -run 'UI|ui_'` 全绿。

## 8. 测试与验收

1. **模块级**：`node --check` 每个新 js 文件；`web-console-config-selftest.sh`（toolcheck 内）继续拦 ESM 字符串真换行类错误。
2. **Go 守护**：`go test ./internal/api/ -run 'UI|ui_'` 全绿；`make check` 七项全绿（含 archtest 对决策号注释的拦截）。
3. **浏览器黑盒（本机 dev server 或真机）**：登录 → 左侧树导航全 32+1 页 → 浅色主题视觉走查 → 部署向导四步全流程（VM 候选写入成功、校验错误逐字段呈现）→ read-only/operator 角色矩阵（写页隐藏、role-notice、我的账号可见）→ noVNC/串口/容器 shell 三个有状态页面回归。
4. **回退预案**：单分支顺序提交（骨架→拆分→向导→测试适配分批 commit），任一批次可独立回退。

## 9. 风险与对策

- **6300 行拆分引入回归**：移动为主的纪律 + 每视图 node --check + 守护测试随时跑。
- **守护测试量大**：16 文件先跑一遍基线（重构前确认全绿），拆分过程每批跑一次，问题当场定位。
- **浅色主题对比度**：状态色取浅色底加深的 WCAG 对比度已核算；截图验收时人眼复核。
- **向导写入候选的权限**：operator 是否可写 VM 候选由服务端 class 裁决（cfgAPI），前端如实呈现 403；不前端放水。
