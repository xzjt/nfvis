# NFViS Web 控制台重构（参考 Cisco NFVIS 门户）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Web 控制台重构为 Cisco NFVIS 门户式信息架构（左侧导航树 + 浅色主题 + VM 生命周期中心），新增部署向导页，app.js 拆分为 core+views 模块，16 个 Go 守护测试与 2 个桩式自校准全部适配保留。

**Architecture:** 单分支单次重构（方案一）。路由真源 routes.json 只增 `group` 列 + 部署向导路由；index.html 重构为「顶栏 + 左侧树 + 内容区」骨架（.page 容器模式保留）；app.js 按 view 域拆到 `js/core/`+`js/views/`+`js/boot.js`，app.js 缩为聚合入口（router.js 的 import 零改动）；两个 vm 桩自校准改为「沿静态 import 递归拼接」的加载器。部署向导走既有 REST 写候选，不新造服务端端点。

**Tech Stack:** vanilla JS（ES modules，无构建无外部依赖）、Go 守护测试、Node vm 桩自校准、browser-use 黑盒验收。

**Spec:** `docs/superpowers/specs/2026-10-15-web-console-cisco-redesign-design.md`

## Global Constraints

- **气隙纪律**：任何 UI 文件不得出现绝对外部地址（`http://`/`https://`，`ui_test.go` 现有断言扩展到全部模块文件）；无构建步骤，浏览器直接加载 ES modules；每个新 js 文件过 `node --check`（node 缺失时如实跳过并记录）。
- **守护契约**：所有被 Go 测试/桩断言的元素 **id**、`data-write`/`data-op` 属性、`body.role-readonly`/`role-nonsuper`/`role-notice-on` class、`st-ok`/`st-warn`/`st-crit` 状态类、`tabs` 分栏类、文案锚点**全部保留**；只有纯布局类名可换。
- **入口不变**：index.html 保持 `src="app.js"`（ui_test.go 断言）；app.js 保持 `export const VIEWS`、`export softLoad/setPollRoute/showGlobalError/reload` 等签名，router.js 的 `import { VIEWS, softLoad, setPollRoute, showGlobalError } from './app.js'` 零改动。
- **routes.json 是前后端共用真源**：只增列（`group`）与新增路由，不改既有路由的 path/title/view/breadcrumb/endpoints/poll/write/detail 语义。
- **写路径纪律**：所有写控件经 `wbtn`（data-write）+ `uiConfirm` 分级确认；部署向导**只写候选不代提交**。
- **拆分是移动为主**：函数体不改逻辑，只加 import/export；每完成一批跑一次守护测试防漂移。
- **工作分支**：`feat/web-console-redesign`（自 v3 切出）；每 Task 一 commit。
- **go:embed**：`//go:embed ui` 递归包含子目录，`ui/js/**` 自动入包，无需改 ui.go 的 MIME 表（FileServer 按 .js 后缀出 text/javascript）。

## 前置事实（执行者必读）

1. **两个桩式自校准**（`contrib/scripts/web-console-config-selftest.js`、`web-console-confirm-selftest.js`）把 `internal/api/ui/app.js` 整份当**经典脚本**跑进 `vm`：显式拒绝静态 `import`（`/^\s*import\s/m` 命中即 throw）、剥离 `export` 后求值，并直接访问顶层符号：`cfg`、`cfgSave`、`cfgRenderForms`、`cfgWriteText`、`cfgPwGate`、`cfg.editing`、`snapVM`、`currentUser`、`dialogTimer`、`fmtTime`、`fmtTimeOpt`、`usrDelete`、`closeDialog`。**拆分后它们必须改为沿 app.js 的静态 import 递归拼接加载**（Task 4）。
2. **读 app.js 的 Go 测试**（13 个文件）：`ui_test.go`（HTTP 资产 + 禁外部地址/禁 x-internal）、`ui_coverage_test.go`（`uiUsedPaths` 只读 ui/app.js 提取 `api('...')` 字面量）、`ui_time_test.go`（全文件扫时间字面量 + `uiFuncSpan` 按函数签名定位）、`ui_tabs_test.go`（index.html tab 条 + app.js 常量）、`ui_permissions_test.go`、`ui_pools_card_test.go`、`ui_login_banner_test.go`、`ui_alarm_clock_test.go`、`ui_qos_card_fields_test.go`、`ui_span_card_fields_test.go`、`ui_switch_ports_fields_test.go`、`system_status_dataplane_test.go`、`ui_routes_test.go`（routes.json）。拆分后凡「搜 app.js」的都改为**搜全部 UI js 文件**（共享 helper，Task 4 建）。
3. **app.js 顶层符号分布**（行号基于当前 6650 行版本，Task 5 的搬移地图）：
   - 共享件：24-47 常量与 `$`/`el`/`dash`/`wbtn`，48-88 `el/fill/table`，89-188 format 族，189-331 dialog 族，332-465 api/softLoad/load* 族，3926-4103 reload/stream/poll/globalError，1316-1334 renderEvents，6021-6085 downloadFile/archiveTable（ops 专用，留在 ops）。
   - 域视图：466-1159 系统/大页/接口/告警/历史趋势；4168-4700 VM 列表/详情/快照/串口+VNC；4714-4963 交换机+LLDP；4964-5018 硬件；5019-5366 容器列表/详情/日志/exec/shell；5367-5602 镜像；5603-5930 网络对象+四类详情；5931-6136+6350-6636 运维/审计/诊断/抓包/软件/恢复；1992-3924 配置页全域（cfg*/CFG_*）；1335-1991 内核/用户/账号/TLS；3927-4167 登录/登出/角色/流通道。
4. **部署向导的数据形状**：`POST /api/v1/virtual-machine-functions` 请求体 = `model.VMFunction`（`internal/model/model.go:634`：name/image/vcpu{count,pin}/memory{size_mb,hugepage_size,backing}/disks[{name,size_gb|image}]/interfaces[{name,type,virtual_switch,mac,vlan}]/cloud_init/autostart/serial_console/startup），经 `mutateCandidate`（`resources.go:40`）写候选并**隐式取会话编辑锁**；成功 201 + `{committed:false,revision}`，校验失败 400 `VALIDATION_FAILED`（detail 数组）、冲突 409。容器 `POST /container-functions` = `ContainerFunction`（model.go:706）。镜像清单 `GET /images` → `[{name,...,ref_count}]`；画像 `GET /vm-image-profiles`；交换机 `GET /virtual-switches`；资源池 `GET /resource-pools` + `GET /system/hugepages`。
5. **分步确认桩的动作表**：`web-console-confirm-selftest.js` 自带动作清单（pre 置 `cfg.editing`/`snapVM` 等 → 断言确认前零写请求、确认后命中正则 URL）。部署向导的「写入候选」要登记进该表（Task 6）。

---

### Task 1: routes.json 增 group 列 + 部署向导路由（守护先行）

**Files:**
- Modify: `internal/api/ui/routes.json`
- Modify: `internal/api/ui_routes_test.go`
- Modify: `internal/api/ui/index.html`（仅追加空的 `page-deploy` section，保 id 映射守护绿）

**Interfaces:**
- Produces: routes.json 每条路由新增 `"group"`（"仪表盘"|"虚拟机"|"网络"|"系统"|"运维"）；新路由 `{path:"#/deploy", title:"部署向导", view:"deploy", breadcrumb:["虚拟机","部署向导"], group:"虚拟机", endpoints:["/images","/vm-image-profiles","/virtual-switches","/resource-pools","/system/hugepages"], poll:0, write:true}`。Task 3 的 router 按 group 渲染树，Task 6 的 deploy view 消费这些端点。

- [ ] **Step 1: 先改守护测试（红）**

在 `ui_routes_test.go` 的 uiRoute struct 加 `Group string \`json:"group"\``，并新增断言（放在现有校验循环之后）：

```go
	// group 列（Cisco 式左侧树分组）：必填且取值闭合——导航树按它分组渲染，
	// 拼错/漏填会让该路由在左侧树里消失。
	groups := map[string]bool{"仪表盘": true, "虚拟机": true, "网络": true, "系统": true, "运维": true}
	for _, r := range routes {
		if !groups[r.Group] {
			t.Errorf("路由 %s 的 group=%q 不在许可集合内（漏填或拼错）", r.Path, r.Group)
		}
	}
	// 部署向导（Cisco VM Life Cycle > Deploy 对应物）：写页 + 端点必须只含既有读端点。
	deployFound := false
	for _, r := range routes {
		if r.Path != "#/deploy" {
			continue
		}
		deployFound = true
		if !r.Write {
			t.Error("部署向导路由必须 write:true（写候选页对只读角色隐藏）")
		}
	}
	if !deployFound {
		t.Error("routes.json 缺 #/deploy 部署向导路由")
	}
```

（uiRoute struct 现有字段名以文件为准；若 struct 是匿名内联，按同风格加 Group。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestUIRoutes -count=1`
Expected: FAIL（group 不在许可集合 / 缺 #/deploy）

- [ ] **Step 3: 改 routes.json**

32 条既有路由各加 `"group"`（映射：`#/`→仪表盘；`#/compute/vms|containers|images` 及其 detail→虚拟机；`#/network*`→网络；`#/system*`→系统；`#/config*`→运维；`#/ops*`→运维），并在 `#/compute/images` 之后插入 deploy 路由（Step 1 的对象）。detail 路由的 group 与所属列表页一致。

- [ ] **Step 4: index.html 追加 page-deploy 空壳**

在 `page-images` 段之后插入（保住「路由 view ↔ page-<view> 容器」映射守护）：

```html
    <!-- 部署向导（Cisco VM Life Cycle > Deploy 对应物）：分步表单在 Task 6 落地，先占位保住路由↔容器映射。 -->
    <section class="page" id="page-deploy" hidden>
      <article class="card wide">
        <h2>部署向导</h2>
        <p class="muted">分步部署表单将在本页提供（选类型 → 镜像与资源 → 网络映射 → 摘要确认）。</p>
      </article>
    </section>
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/api/ -run 'TestUI' -count=1 && go test ./internal/api/ -run 'UI' -count=1`
Expected: PASS（全部 UI 守护绿）

- [ ] **Step 6: Commit**

```bash
git checkout -b feat/web-console-redesign
git add internal/api/ui/routes.json internal/api/ui_routes_test.go internal/api/ui/index.html
git commit -m "feat(web): routes.json 增 group 列 + 部署向导路由占位（守护先行）"
```

---

### Task 2: 骨架重构——顶栏 + 左侧树容器 + 浅色主题

**Files:**
- Modify: `internal/api/ui/index.html`（重排骨架；.page 段原样搬移，id 全保留）
- Rewrite: `internal/api/ui/style.css`（浅色主题 + 新布局类）

**Interfaces:**
- Consumes: 无（纯静态骨架）。
- Produces: 骨架元素 `#sidebar`（aside）、`#nav-tree`（树容器）、`#sb-toggle`（☰ 按钮）；`.page` 段与全部既有 id（`modal`、`acct`、`login-view`、`main-view`、`nav`（保留但改藏——router.js Task 3 前仍写它）、`crumb`、`stream-state`、`user-line`、`account-btn`、`refresh-btn`、`logout-btn`、`global-error`、`route-notice`、`role-notice` 等）原位保留。Task 3 的 renderNav 往 `#nav-tree` 里渲染。

- [ ] **Step 1: index.html 骨架重排**

`<section id="main-view">` 内部改为：

```html
<section id="main-view" hidden>
  <header class="topbar">
    <button id="sb-toggle" type="button" class="ghost" aria-label="折叠/展开导航">☰</button>
    <div class="brand"><strong>NFViS 控制台</strong><span id="host-line" class="muted"></span></div>
    <div id="crumb" class="crumb" aria-label="面包屑"></div>
    <div class="topbar-right">
      <span id="stream-state" class="pill pill-idle">实时通道：连接中</span>
      <span id="user-line" class="muted"></span>
      <!-- 「我的账号」不带 data-write：全角色可见（决策 #147 既有口径，注释原文保留）。 -->
      <button id="account-btn" type="button" class="ghost">我的账号</button>
      <button id="refresh-btn" type="button" class="ghost">刷新</button>
      <button id="logout-btn" type="button" class="ghost">退出</button>
    </div>
  </header>
  <div class="shell">
    <aside id="sidebar" class="sidebar">
      <nav id="nav-tree" aria-label="主导航"></nav>
    </aside>
    <main class="content">
      <nav id="nav" class="nav" aria-label="主导航" hidden></nav>
      <p id="global-error" class="error" hidden></p>
      <p id="route-notice" class="error" hidden></p>
      <p id="role-notice" class="muted small"></p>
      <!-- 32 个 <section class="page" id="page-*"> 段**原样搬进 main.content**，内容一字不改。 -->
    </main>
  </div>
</section>
```

要点：① 原 topbar 里的 `<nav id="nav">` 挪进 content 并 `hidden`（Task 3 切到 #nav-tree 前 router 仍往 #nav 写，hidden 免得双导航闪现）；② 原 `<div class="grid">` 包裹改为 `main.content` 直接包 .page（.page 自身是 grid，样式等价）；③ 登录视图、modal、acct 等既有顶层元素不动；④ **逐段搬移 .page 时不得改动任何 id/class/文案**。

- [ ] **Step 2: style.css 重写（浅色主题）**

变量块与关键布局（组件类名沿用：card/table/kv/pill/code/console-out/tabs/events/net-object/error/muted/small/ghost/st-ok/st-warn/st-crit；逐类把深色配色换成浅色，其余规则按需微调但选择器不变）：

```css
/* NFViS 控制台样式（浅色主题，参考 Cisco NFVIS 门户取色）。
   无外部字体/图标依赖：页面在气隙环境也要能看。
   历史备注：v3 前为深色运维风（--bg:#0f1720 族），重构时整体换套；深色变量不再保留。 */
:root {
  --bg: #f4f6f9;
  --panel: #ffffff;
  --panel-2: #f8fafc;
  --line: #d9dee5;
  --fg: #1a2b3c;
  --muted: #5f7183;
  --accent: #049fd9;
  --accent-hover: #0281b0;
  --ok: #03804b;
  --warn: #9a6a00;
  --err: #b32d21;
}
body { margin: 0; background: var(--bg); color: var(--fg);
  font: 14px/1.5 "Segoe UI", "Microsoft YaHei", system-ui, sans-serif; }
[hidden] { display: none !important; }
.topbar { display: flex; align-items: center; gap: 12px; padding: 8px 16px;
  background: var(--panel); border-bottom: 1px solid var(--line); position: sticky; top: 0; z-index: 20; }
.topbar-right { margin-left: auto; display: flex; align-items: center; gap: 8px; }
.shell { display: flex; min-height: calc(100vh - 49px); }
.sidebar { width: 220px; flex: 0 0 220px; background: var(--panel);
  border-right: 1px solid var(--line); padding: 10px 8px; }
body.sb-collapsed .sidebar { display: none; }
.content { flex: 1; padding: 14px 18px; min-width: 0; }
.page { display: grid; gap: 14px; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); }
.page[hidden] { display: none; }
/* 左侧树（Task 3 渲染进 #nav-tree） */
.nav-tree-group { margin: 2px 0 10px; }
.nav-tree-head { display: flex; width: 100%; align-items: center; justify-content: space-between;
  background: none; border: 0; color: var(--muted); font-size: 12px; font-weight: 600;
  padding: 6px 8px; cursor: pointer; letter-spacing: .04em; }
.nav-tree-head .caret { transition: transform .12s; }
.nav-tree-head[aria-expanded="false"] .caret { transform: rotate(-90deg); }
.nav-tree-items a { display: block; color: var(--fg); text-decoration: none;
  font-size: 13.5px; padding: 6px 10px; border-radius: 6px; }
.nav-tree-items a:hover { background: var(--panel-2); color: var(--accent); }
.nav-tree-items a[aria-current="page"] { background: #e6f7fd; color: var(--accent); font-weight: 600; }
.card { background: var(--panel); border: 1px solid var(--line); border-radius: 10px;
  padding: 14px 16px; box-shadow: 0 1px 2px rgba(16,35,47,.05); }
```

其余组件类（table/pill/kv/code/console-out/tabs/login-card/dialog/acct 等）逐类换成浅色取色（表格斑马 `#f8fafc`、pill 状态底色 `#e7f6ee/#fdf4e0/#fdecea` 配深字、输入框白底 `--line` 边、code 块 `#0f1720` 深底浅字保留终端观感）；`body.role-readonly`/`role-nonsuper`/`role-notice-on` 的显示规则**原样保留**（只把涉及配色的值换掉）。

- [ ] **Step 3: 静态校验**

Run: `node --check internal/api/ui/app.js && node --check internal/api/ui/router.js`（node 缺失则记录跳过）
Run: `go test ./internal/api/ -run 'TestUI|UI' -count=1`
Expected: 全绿（本 Task 不改任何 JS 行为；`#nav` hidden 不影响 router 写入）
人眼检查：`grep -c 'id="page-' internal/api/ui/index.html` 仍为 33（32+deploy）。

- [ ] **Step 4: Commit**

```bash
git add internal/api/ui/index.html internal/api/ui/style.css
git commit -m "feat(web): 骨架重构——顶栏+左侧树容器+浅色主题（Cisco 门户风）"
```

---

### Task 3: router.js 左侧树渲染

**Files:**
- Modify: `internal/api/ui/router.js`

**Interfaces:**
- Consumes: routes.json 的 `group` 列（Task 1）；#nav-tree/#sb-toggle（Task 2）。
- Produces: `renderNav(route)` 改名不改签名——继续由 render() 调用；内部往 `#nav-tree` 渲染两层级联树；`#nav` 旧容器停止使用（元素保留在 index.html 以防守护锚点，hidden）。

- [ ] **Step 1: renderNav 改造**

替换现有 renderNav 函数（navLink/navBasePath/renderCrumb/renderRoleNotice 不动）：

```js
// 左侧树（Cisco 门户式信息架构）：按 routes.json 的 group 分组的两层级联。
// 单项组（仪表盘）不渲染组头、直接一层链接；多项组渲染可折叠组头（aria-expanded +
// sessionStorage 记忆展开态）；详情页不进树，只让所属列表项高亮（navBasePath 既有口径）。
// read-only 角色不列写页（write:true，决策 #145 口径不变）。
const SB_OPEN_KEY = 'nfvis.sidebar.open';
function sbOpenSet() {
  try { return new Set(JSON.parse(sessionStorage.getItem(SB_OPEN_KEY) || '[]')); }
  catch (e) { return new Set(); }
}
function renderNav(route) {
  const nav = document.getElementById('nav-tree');
  if (!nav) return;
  nav.textContent = '';
  const active = navBasePath(route);
  const open = sbOpenSet();
  const readonly = isReadOnlyRole();
  const groups = [];
  for (const r of routes) {
    if (r.detail) continue;
    if (r.write && readonly) continue;
    const g = r.group || '';
    let bucket = groups.find((x) => x.name === g);
    if (!bucket) { bucket = { name: g, items: [] }; groups.push(bucket); }
    bucket.items.push(r);
  }
  for (const g of groups) {
    if (g.name && g.items.length > 1) {
      const isOpen = open.has(g.name) || g.items.some((r) => r.path === active);
      const wrap = el('div', { class: 'nav-tree-group' });
      const head = el('button', {
        class: 'nav-tree-head', type: 'button',
        'aria-expanded': isOpen ? 'true' : 'false',
      });
      head.appendChild(el('span', { text: g.name }));
      head.appendChild(el('span', { class: 'caret', text: '▾' }));
      const items = el('div', { class: 'nav-tree-items' });
      if (!isOpen) items.hidden = true;
      head.addEventListener('click', () => {
        const now = items.hidden;
        items.hidden = !now;
        head.setAttribute('aria-expanded', now ? 'true' : 'false');
        const cur = sbOpenSet();
        if (now) cur.add(g.name); else cur.delete(g.name);
        try { sessionStorage.setItem(SB_OPEN_KEY, JSON.stringify([...cur])); } catch (e) { /* 隐私模式忽略 */ }
      });
      for (const r of g.items) items.appendChild(navLink(r, active));
      wrap.appendChild(head); wrap.appendChild(items);
      nav.appendChild(wrap);
    } else {
      for (const r of g.items) nav.appendChild(navLink(r, active));
    }
  }
}
```

`el()` 从 app.js 导入（`import { VIEWS, softLoad, setPollRoute, showGlobalError } from './app.js'` 现有 import 行追加 `el`；app.js 需 `export` 它——本 Task 在 app.js 的 `function el(` 前加 `export `，这是拆分前唯一改动）。同函数尾部追加折叠按钮接线：

```js
// ☰ 折叠左侧树（窄屏/专注模式）；状态记 sessionStorage，与树展开态同生。
const tgl = document.getElementById('sb-toggle');
if (tgl && !tgl.dataset.bound) {
  tgl.dataset.bound = '1';
  tgl.addEventListener('click', () => {
    document.body.classList.toggle('sb-collapsed');
    try { sessionStorage.setItem('nfvis.sidebar.collapsed',
      document.body.classList.contains('sb-collapsed') ? '1' : ''); } catch (e) { /* 同上 */ }
  });
  try { if (sessionStorage.getItem('nfvis.sidebar.collapsed')) document.body.classList.add('sb-collapsed'); } catch (e) { /* 同上 */ }
}
```

- [ ] **Step 2: 校验**

Run: `node --check internal/api/ui/router.js && go test ./internal/api/ -run 'UI' -count=1`
Expected: 全绿。手测（Task 7 浏览器验收兜底）：树分组、折叠记忆、当前页高亮。

- [ ] **Step 3: Commit**

```bash
git add internal/api/ui/router.js internal/api/ui/app.js
git commit -m "feat(web): 左侧导航树（按 group 分组、可折叠、会话记忆）"
```

---

### Task 4: 模块拆分阶段一——core 四件 + vm 桩加载器改造 + 共享 Go helper

**Files:**
- Create: `internal/api/ui/js/core/dom.js`、`js/core/api.js`、`js/core/format.js`、`js/core/state.js`
- Modify: `internal/api/ui/app.js`（删走的部分，顶部加静态 import，`export` 共享符号）
- Modify: `contrib/scripts/web-console-config-selftest.js`、`web-console-confirm-selftest.js`（加载器）
- Modify: `internal/api/ui_test.go`、`ui_time_test.go`（共享 walk helper + 改用）
- Create: `internal/api/ui_jsfiles_test.go`（共享 helper 所在）

**Interfaces:**
- Produces（Task 5 的 views 全部 import 这些名字）:
  - `dom.js`：`$(id)`、`el(tag,attrs,children)`、`fill(dl,pairs)`、`table(tbody,cols,rows)`、`dash`、`list(v)`、`wbtn(opts)`、`uiDialog(opts)`、`closeDialog(value)`、`uiConfirm(label,opts)`、`CONFIRM_COUNTDOWN`、`rowButton(btn)`、`rowClickable(tr,path)`、`goDetail(path)`
  - `api.js`：`API`、`TOKEN_KEY`、`USER_KEY`、`api(path,opts)`、`soft(p)`、`apiErrText(e,hint)`、`httpErrText(res)`、`errTextOf(text,status)`、`rowsOf(v)`、`pageWarn(d)`、`softLoad(paths,params)`、`expandEndpoint(path,params)`、`loadVSwitchStats(vss)`、`loadNetworkObjects(pre)`、`loadInterfaceStats(ifaces)`
  - `format.js`：`uptime`、`bytes`、`pct`、`mb`、`fmtTime`、`fmtClockHint`、`fmtTimeOpt`、`fmtBytes`、`portSecText`、`stormControlText`、`FMT_TIME_NAIVE_RE`
  - `state.js`：`token`（export let，经 `getToken()/setToken()` 访问更稳——**用访问器**：`getToken()`/`setToken(v)`/`getUser()/setUser(v)/setCurrentClass(v)`）、`MAX_EVENTS`、`events`（`pushEvent(ev)/getEvents()`）、`setStream(text,cls)`、`startStream()`、`stopStream()`、`armPolling()`、`setPollRoute(route)`、`startPolling()`、`stopPolling()`、`scheduleReload()`、`pollPath`（读经 `isPollArmed(path)`）、`summaryOf(ev)`、`renderEvents()`
  - 注意：ES 模块的顶层 `let` 不会被 import 方看到重赋值——**所有跨模块可变状态一律访问器化**（上表已列）；单向常量才直接 export。

- [ ] **Step 1: 建 Go 共享 helper（先红后绿不适用——helper 本身无断言，先建供两测试改用）**

`internal/api/ui_jsfiles_test.go`：

```go
package api

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// uiJSFiles 返回前端**自有** JS 源（ui/*.js 与 ui/js/**，不含 novnc/ 第三方库）。
// 拆分后前端源码散在多文件里，凡「搜 app.js」的守护一律改搜这份集合——
// 断言内容不变，搜索面从单文件变为全量，防止逻辑搬走后守护静默空转。
func uiJSFiles(t *testing.T) map[string]string {
	t.Helper()
	root := "ui"
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "novnc" {
				return filepath.SkipDir // 第三方库：注释里本就含 URL，不属守护面
			}
			return nil
		}
		if filepath.Ext(path) != ".js" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 ui/ 下的 js: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("ui/ 下没有任何 js 文件——守护的搜索面空了")
	}
	return out
}

// uiJSFile 在集合里取单文件（守护确实只关心某一份时用，锚点变更要同步本函数调用处）。
func uiJSFile(t *testing.T, name string) string {
	t.Helper()
	m := uiJSFiles(t)
	src, ok := m[name]
	if !ok {
		t.Fatalf("ui/ 下找不到 %s", name)
	}
	return src
}

// uiJSSortedNames 稳定序文件名清单（报错信息可读）。
func uiJSSortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
```

- [ ] **Step 2: ui_time_test.go / ui_test.go 改用 helper**

`ui_time_test.go`：`os.ReadFile("ui/app.js")` 处改为对 `uiJSFiles(t)` 每个文件跑同样的扫描（行号报错带文件名）；`uiFuncSpan` 的定位改为在**每份文件**里找签名（找到即返回 {file, body, offset}）。
`ui_test.go`：外部地址断言的资产循环改为遍历 `uiJSFiles(t)`（index.html 仍单独断言 `src="app.js"` 与 `href="style.css"`）；x-internal 断言改为对全部 js 逐份 stripJSComments 后检查。

- [ ] **Step 3: 抽 core 四件**

按前置事实 §3 的行号区间把函数**逐字搬移**到四个 core 文件（每处搬移：源文件删除函数体 → 目标文件加 `export` → app.js 顶部 `import { ... } from './js/core/xxx.js'`）。可变状态按访问器化清单改造（token/currentUser/currentClass/events/poll*/stream 状态）。app.js 内部所有原直接引用改为经访问器/导入名（`token` → `getToken()`，共约几十处，逐一改）。router.js 需要的 `el` 已在 Task 3 export。
搬移清单（行号为当前版本）：
- dom.js ← 43-47($), 48-65(el), 59-63(dash), 66-73(fill), 74-88(table), 165-188(list), 189-331(dialog 全族+CONFIRM_COUNTDOWN), 4176-4192(rowButton/rowClickable/goDetail)
- api.js ← 24-26(API/TOKEN_KEY/USER_KEY), 332-365(api), 361-365(soft), 366-391(apiErrText/httpErrText/errTextOf), 373-377(rowsOf), 392-427(pageWarn/softLoad/expandEndpoint), 428-465(loadVSwitchStats/loadNetworkObjects/loadInterfaceStats)
- format.js ← 89-164(uptime/bytes/pct/portSecText/stormControlText/mb/FMT_TIME_NAIVE_RE/fmtTime/fmtClockHint), 3835-3850(fmtTimeOpt), 6534(fmtBytes)
- state.js ← 27(MAX_EVENTS), 30-41(token/currentUser/currentClass/poll*/stream*/events 声明→访问器), 3930-3953(setStream/scheduleReload/summaryOf), 3954-4047(startStream/stopStream/armPolling/setPollRoute/startPolling/stopPolling/showGlobalError), 1316-1334(renderEvents/pushEvent)
- `showGlobalError` 留在 state.js（router.js 从 app.js re-import，见 Step 4）。

- [ ] **Step 4: app.js 头部接线**

```js
import { $, el, fill, table, dash, list, wbtn, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from './js/core/dom.js';
import { API, api, soft, apiErrText, httpErrText, errTextOf, rowsOf, pageWarn,
  softLoad, expandEndpoint, loadVSwitchStats, loadNetworkObjects, loadInterfaceStats } from './js/core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from './js/core/format.js';
import { getToken, setToken, getUser, setUser, setCurrentClass, pushEvent, getEvents,
  setStream, startStream, stopStream, armPolling, setPollRoute, isPollArmed,
  scheduleReload, renderEvents, showGlobalError } from './js/core/state.js';
export { softLoad, setPollRoute, showGlobalError, reload };   // router.js 消费（setPollRoute 来自 state）
```

（export 列表以 router.js 实际 import 为准：`VIEWS, softLoad, setPollRoute, showGlobalError`；VIEWS 本 Task 仍定义在 app.js。）

- [ ] **Step 5: 两个 vm 桩改「沿静态 import 递归拼接」**

两个 stub 的加载段（现读单文件 + 拒 import + 剥 export + vm.runInContext）统一改为共用算法（各自文件内实现，约 40 行）：

```js
// ---- 多模块加载：沿 app.js 的静态 import 递归拼接为一份经典脚本 ----
// 拆分后的前端是 ES modules，但本桩要在 vm 里以经典脚本求值并直接摸顶层符号
// （cfg/cfgSave/...），故把依赖闭包**按后序**拼接（被依赖者先出现），剥掉
// import/export 语句后整份求值——测的仍是真实实现，不是复刻。
function bundle(APP) {
  const seen = new Set();
  const order = [];
  const base = path.dirname(APP);
  (function walk(file) {
    if (seen.has(file)) return;
    seen.add(file);
    let src = fs.readFileSync(file, 'utf8');
    if (/^\s*import\s/m.test(src)) {
      src = src.replace(/^\s*import\s[^;]*?from\s+['"]([^'"]+)['"];?[^\S\n]*$/gm, (m, spec) => {
        const dep = path.normalize(path.join(path.dirname(file), spec));
        walk(dep);
        return ''; // 名字经拼接共享同一作用域，import 语句本身可删
      });
      if (/^\s*import\s/m.test(src)) throw new Error(file + ' 残留未处理的 import（动态 import 或裸 import）——本桩不支持');
    }
    // export 剥离：export const/let/function/async function/class → 保留声明本体
    src = src.replace(/^export\s+(?=(const|let|var|function|async\s+function|class)\b)/gm, '');
    src = src.replace(/^export\s*\{[^}]*\};?[^\S\n]*$/gm, ''); // re-export 行删除
    src = src.replace(/^export\s+default\s+/gm, '');
    order.push({ file, src });
  })(APP);
  return order.map((o) => '// ===== ' + o.file + ' =====\n' + o.src).join('\n');
}
```

- 原「拒绝静态 import」的断言**保留语义但换位置**：bundle 后若残留 import 即 throw（上面已含）。
- 原 export 剥离逻辑若与上不同，以 bundle 版本为准替换；红-绿变异锚点（`await cfgPwGate()`、`cfg.editing`、档位表）**不变**——拼接是逐字保留声明本体。
- 跨文件可变状态的访问器化已把「桩直接赋值顶层变量」的用法改掉：桩里 `vm.runInContext('cfg.editing = true')`/`snapVM = ...`/`currentUser = "admin"` 仍可用（cfg/snapVM/currentUser 仍是各自文件的顶层 let，拼接后同作用域）；`currentUser` 挪进 state.js 后桩改 `setUser("admin")`。
- 两个 .sh 不改（仍指向同名 .js）。

- [ ] **Step 6: 全量校验**

Run: `for f in internal/api/ui/js/core/*.js internal/api/ui/app.js internal/api/ui/router.js; do node --check "$f" || exit 1; done`
Run: `bash contrib/scripts/web-console-config-selftest.sh && bash contrib/scripts/web-console-confirm-selftest.sh`
Run: `go test ./internal/api/ -count=1`（全包）
Expected: 全绿。⚠️ 若桩报「顶层符号缺失」，优先检查访问器化是否漏改桩引用。

- [ ] **Step 7: Commit**

```bash
git add internal/api/ui internal/api/ui_jsfiles_test.go internal/api/ui_test.go internal/api/ui_time_test.go contrib/scripts/
git commit -m "refactor(web): 拆分 core 四件（dom/api/format/state）+ vm 桩递归拼接加载器"
```

---

### Task 5: 模块拆分阶段二——views 全域搬移 + 聚合入口 + 守护测试全面适配

**Files:**
- Create: `internal/api/ui/js/views/{overview,vms,vmDetail,containers,containerDetail,images,network,switches,lldp,system,config,ops,deploy-stub}.js`、`internal/api/ui/js/boot.js`
- Modify: `internal/api/ui/app.js`（缩为聚合入口）
- Modify: 13 个 Go 守护测试中「搜 app.js」的调用点 → `uiJSFiles`/`uiJSFile`

**Interfaces:**
- Consumes: Task 4 的 core 四件全部导出。
- Produces: 每个 views 文件 `export const views = { <viewName>: {...} }`；app.js 聚合：

```js
import { views as overviewViews } from './js/views/overview.js';
/* ...13 行... */
import { boot } from './js/boot.js';
export const VIEWS = {
  ...overviewViews, ...vmsViews, ...vmDetailViews, ...containersViews, ...containerDetailViews,
  ...imagesViews, ...networkViews, ...switchesViews, ...lldpViews, ...systemViews,
  ...configViews, ...opsViews, ...deployViews,
};
boot();
```

（router.js 的 import 不变。）

- [ ] **Step 1: 按域搬移（搬移地图，行号=当前版本）**

| 目标文件 | 搬入符号（源行号） |
|---|---|
| views/overview.js | systemPairs/versionPairs/renderSystem/renderVPP/renderAlarms/alarmClockMark(466-533,571-603,887-915) + `views={overview}` |
| views/vms.js | VM_ACTIONS/renderVMRows/VM_ACTION_IMPACT/vmAction/renderVMStats(4168-4275) |
| views/vmDetail.js | VM_TABS/vmTab/vmDetailName/vmTabShow/vmDetailMsg/vmDetailHead/Info/Actions/Ifaces/renderVMDetail/snapVM/snapMsg/vmSnapOpen/Load/Act/Create/termWS 族/vmConsole*(4276-4700) |
| views/containers.js | renderContainerRows/CT_ACTION_IMPACT/ctAction(5019-5081) |
| views/containerDetail.js | CT_TABS/ctTab 族/ctShell 族/ctExec 族/ctDetail 族/envText/ctLogs 族(5082-5366) |
| views/images.js | renderImages/renderImageDetail/renderImageProfile/imgIsFailed/imgOutcome/imgMsg/imgDelete/imgCommon/imgImportURL/Incoming/File(5367-5602) |
| views/network.js | qosBindingsText/NET_OBJECT_VIEWS/natRows/vxlanRows/renderNetworkObjects/pickByName/notFoundText/routesText/vrfRoutesShow/Load/renderVrfDetail/renderAclDetail/renderBondDetail/renderQosDetail/qsdMsg/qsdDelete/spanSourceText/renderSpanDetail/spdMsg/spdDelete(5603-5930) |
| views/switches.js | renderVSwitches/vsdMacFor/VSD_MAC_LIMIT/vsdMsg/vsdMacClear/VSD_SOURCE_TEXT/renderSwitchDetail/vsdMacLoad/vsdDhcp 族(4714-4924) |
| views/lldp.js | renderLldp(4925-4963) |
| views/system.js | renderSystemPage/renderFirewallCard/renderPools/hp 族/renderInterfaces/ifdMsg/renderIfaceDetail/dpdkAct/sriovSet(503-570,604-886)/HW_ 族/renderHardware(4964-5018)/knl 族/renderKernel(1335-1430)/USR_ 族/usr 族/renderPerm 族/renderUsers/renderTokens/renderBannerCard/acct 族(1431-1900)/tls 族/renderTLS(1901-1991)/mh 族/svgNode/renderMetricsHistoryPage(916-1165) |
| views/config.js | cfg 全族/CFG_ 全族/parseCores/formatCores/renderCfgVpp/loadConfig/cfgLockNote/renderHistory/cfgh 族/renderSessions/sessionSource(1992-3834) |
| views/ops.js | pingBody/renderDiagPlaneNote/diagPing/diagLogs/bigMsg/bigOut/bigNat/renderAudit/capMsg/renderCapture/downloadFile/renderArchives/archiveTable/opsMsg/opsOut/opsRun/swAdd/swRollback/restoreRun/zeroizeRun/formatDataRun/fmtDataCount(3851-3866,3868-3876,3877-3925,5931-6136,6350-6636) |
| js/boot.js | loadLoginBanner/showLogin/applyRole/enterApp/signOut/doLogin/doLogout/reload(3926-3929,4048-4167) + 全局事件接线（keydown 6137-6141、beforeunload、window.nfvis、boot() 本体与登录表单绑定）+ **各视图自己的控件接线搬进对应 views 文件**（6142-6349 与 6350-6636 里的 `$('...').addEventListener` 按 id 前缀归属：audit-/big-/ops-/sw-/restore-/zeroize-/fmt-/diag- → ops.js；vmd-/vm-snap-/vm-console- → vmDetail.js；ctd-/ct- → containerDetail.js；vrd- → network.js；vsd- → switches.js；img- → images.js；usr-/banner-/perm- → system.js；cfg- → config.js；knl- → system.js；tls- → system.js；hp- → system.js；mh- → system.js；dpdk-/sriov- → system.js；acct- → boot.js） |
| views/deploy-stub.js | Task 6 重写为 deploy.js；本 Task 先给 `export const views = { deploy: { render() { /* Task 6 */ } } }` 保聚合完整 |

跨视图小工具归属：`qosBindingsText`→network.js（switches 不用）；`sessionSource`→config.js；`envText`→containerDetail.js；`svgNode`→system.js。同域互引用相对 import（如 vmDetail 用 vms 的 `renderVMStats`→改为 vmDetail 自带或上移 core——**上移 core/dom.js**（表格渲染件），避免 views 互引）。

- [ ] **Step 2: app.js 缩为聚合入口**（见 Interfaces 代码块），router.js 不动。

- [ ] **Step 3: Go 守护适配（改搜索面，断言内容不变）**

统一改法：`os.ReadFile("ui/app.js")` + 单文件搜索 → `m := uiJSFiles(t)` 后逐文件搜索（命中任一即过；报错列出 `uiJSSortedNames(m)`）。逐文件：
- `ui_permissions_test.go`（usr 族→system.js）
- `ui_pools_card_test.go`（hp/renderPools→system.js）
- `ui_login_banner_test.go`（showLogin/loadLoginBanner→boot.js）
- `ui_alarm_clock_test.go`（alarmClockMark/renderAlarms→overview.js）
- `ui_qos_card_fields_test.go`（qosBindingsText→network.js）
- `ui_span_card_fields_test.go`（spanSourceText→network.js）
- `ui_switch_ports_fields_test.go`（VSD_→switches.js）
- `ui_tabs_test.go`（VM_TABS→vmDetail.js、CT_TABS→containerDetail.js；index.html 侧不变）
- `system_status_dataplane_test.go`、`ui_coverage_test.go`（uiUsedPaths 遍历全部 js 提取字面量）
- `ui_test.go`、`ui_time_test.go`（Task 4 已改，复核）

- [ ] **Step 4: 全量校验**

Run: `for f in $(find internal/api/ui -name '*.js' -not -path '*novnc*'); do node --check "$f" || exit 1; done`
Run: `bash contrib/scripts/web-console-config-selftest.sh && bash contrib/scripts/web-console-confirm-selftest.sh`
Run: `go test ./internal/api/ -count=1 && go build ./...`
Expected: 全绿。任何「函数未定义/重复声明」都在拼接域里排查（同名顶层符号冲突——拆分时如发现重名，改名的那个要同步其全部引用与守护锚点）。

- [ ] **Step 5: Commit**

```bash
git add internal/api/ui internal/api
git commit -m "refactor(web): app.js 按 view 域拆分（13 views + boot）+ 守护测试搜索面适配"
```

---

### Task 6: 部署向导（deploy.js + page-deploy 表单 + 分级确认桩登记）

**Files:**
- Rewrite: `internal/api/ui/js/views/deploy.js`（替换 deploy-stub）
- Modify: `internal/api/ui/index.html`（page-deploy 段填实）
- Modify: `contrib/scripts/web-console-confirm-selftest.js`（动作表登记）

**Interfaces:**
- Consumes: core 四件；routes.json deploy 端点（/images /vm-image-profiles /virtual-switches /resource-pools /system/hugepages）。
- Produces: `POST /virtual-machine-functions`（VM）与 `POST /container-functions`（容器）写候选；成功后提示指向 #/config 提交。

- [ ] **Step 1: page-deploy 表单骨架（index.html）**

```html
    <section class="page" id="page-deploy" hidden>
      <article class="card wide">
        <h2>部署向导 <span id="dep-step-note" class="muted small"></span></h2>
        <p class="muted small">分四步生成部署候选：只写候选、不代提交——提交仍在
          <a href="#/config">配置</a>页复核后进行（与命令行同一管线）。</p>
        <div id="dep-steps" class="tabs">
          <button type="button" data-step="1" aria-current="step">① 类型</button>
          <button type="button" data-step="2" aria-current="step">② 镜像与资源</button>
          <button type="button" data-step="3" aria-current="step">③ 网络映射</button>
          <button type="button" data-step="4" aria-current="step">④ 摘要确认</button>
        </div>
        <div id="dep-step-1">
          <div class="dep-type-row">
            <button type="button" id="dep-type-vm" class="dep-type">虚拟机（VM）</button>
            <button type="button" id="dep-type-ct" class="dep-type">容器</button>
          </div>
        </div>
        <div id="dep-step-2" hidden>
          <label for="dep-name">名称</label>
          <input id="dep-name" autocomplete="off" placeholder="如 vnf-web-1">
          <label for="dep-image">镜像</label>
          <select id="dep-image"></select>
          <p id="dep-image-note" class="muted small"></p>
          <label for="dep-vcpu">vCPU 数</label>
          <input id="dep-vcpu" type="number" min="1" value="1">
          <label for="dep-mem">内存 (MB)</label>
          <input id="dep-mem" type="number" min="128" value="1024">
          <p id="dep-pool-note" class="muted small"></p>
          <!-- VM 专属 -->
          <div id="dep-vm-only">
            <label for="dep-disk-mode">磁盘</label>
            <select id="dep-disk-mode">
              <option value="image">从镜像克隆（推荐）</option>
              <option value="empty">空盘</option>
            </select>
            <label for="dep-disk-size">空盘容量 (GB)</label>
            <input id="dep-disk-size" type="number" min="1" value="8">
            <label><input id="dep-autostart" type="checkbox"> 提交后自动启动</label>
            <label><input id="dep-serial" type="checkbox" checked> 串口控制台</label>
          </div>
          <!-- 容器专属 -->
          <div id="dep-ct-only" hidden>
            <label for="dep-cmd">启动命令（可选）</label>
            <input id="dep-cmd" placeholder="如 /bin/sh -c">
            <label for="dep-restart">重启策略</label>
            <select id="dep-restart">
              <option value="">（缺省）</option><option value="no">no</option><option value="on-failure">on-failure</option>
            </select>
          </div>
          <button type="button" id="dep-next-2" class="ghost">下一步 →</button>
        </div>
        <div id="dep-step-3" hidden>
          <p class="muted small">为每个虚拟网卡选择挂接的虚拟交换机；类型按平台给可选值
            （VM：vhost-user / sriov-vf；容器：memif）。拓扑预览为静态示意（右侧），不可拖拽。</p>
          <table id="dep-nic-table">
            <thead><tr><th>网卡名</th><th>类型</th><th>挂接交换机</th><th></th></tr></thead>
            <tbody></tbody>
          </table>
          <button type="button" id="dep-add-nic" class="ghost">+ 加一块网卡</button>
          <div class="dep-topo">
            <svg id="dep-topo" width="520" height="180" role="img" aria-label="部署拓扑预览"></svg>
          </div>
          <button type="button" id="dep-back-3" class="ghost">← 上一步</button>
          <button type="button" id="dep-next-3" class="ghost">下一步 →</button>
        </div>
        <div id="dep-step-4" hidden>
          <dl id="dep-summary" class="kv"></dl>
          <p id="dep-msg" class="muted"></p>
          <!-- 写入候选：wbtn（data-write）+ uiConfirm 低危档；只写候选不提交。 -->
          <button type="button" id="dep-submit" class="primary">写入部署候选</button>
          <button type="button" id="dep-back-4" class="ghost">← 上一步</button>
          <pre id="dep-result" class="code console-out" hidden></pre>
        </div>
      </article>
    </section>
```

- [ ] **Step 2: deploy.js 实现（完整逻辑）**

```js
// 部署向导（Cisco VM Life Cycle > Deploy 的对应物）：四步生成 VM/容器部署候选。
// 口径：① 只写候选不代提交——提交是配置页/CLI 的职责，向导成功文案必须指向 #/config；
// ② 客户端只做提示级预校验（必填/数字/名称形状），权威校验在服务端（400 的 detail
// 数组逐条回显）；③ 拓扑预览是静态 SVG 示意（选中交换机 × 网卡的连线），非编辑器。

import { $, el, wbtn, uiConfirm, dash } from '../core/dom.js';
import { api, rowsOf, pageWarn, apiErrText } from '../core/api.js';
import { bytes } from '../core/format.js';

let depKind = '';        // 'vm' | 'ct'（Step 1 选定）
let depStep = 1;
let depData = null;      // { images, profiles, vss, pools, hp }（render 时一次拉取）

const depMsg = (text, isErr) => {
  const m = $('dep-msg');
  m.textContent = text || '';
  m.classList.toggle('error', !!isErr);
};

function depShow(step) {
  depStep = step;
  for (let i = 1; i <= 4; i++) $('dep-step-' + i).hidden = i !== step;
  for (const b of $('dep-steps').querySelectorAll('button')) {
    const n = Number(b.dataset.step);
    b.style.opacity = n <= step ? '1' : '.5';
  }
  $('dep-step-note').textContent = '第 ' + step + ' / 4 步';
  if (step === 3) depDrawTopo();
  if (step === 4) depFillSummary();
}

function depRequireKind() {
  if (depKind) return true;
  depMsg('请先在第 ① 步选择部署类型。', true);
  return false;
}

function depFillImages() {
  const sel = $('dep-image');
  sel.textContent = '';
  for (const img of depData.images) {
    const o = el('option', { value: img.name,
      text: img.name + (img.size_bytes ? '（' + bytes(img.size_bytes) + '）' : '') });
    sel.appendChild(o);
  }
  const prof = depData.profiles.find((p) => p.image === sel.value);
  $('dep-image-note').textContent = prof
    ? '镜像画像：machine=' + dash(prof.machine) + ' firmware=' + dash(prof.firmware) +
      ' console=' + dash(prof.console) + (prof.extra_args && prof.extra_args.length ? ' 等（详情见镜像页）' : '')
    : '该镜像没有启动画像（按缺省画像启动）。';
}

function depFillPools() {
  const pools = depData.pools;
  const hp = depData.hp;
  const lines = [];
  if (pools && pools.hugepages) {
    for (const p of pools.hugepages) {
      lines.push(p.size + ' 大页：声明 ' + (p.declared != null ? p.declared : '—') +
        '，空闲 ' + (p.free != null ? p.free : '取不到'));
    }
  }
  if (hp && hp.pools) {
    for (const p of hp.pools) {
      lines.push(p.size + ' 池空闲页 ' + (p.free != null ? p.free : '取不到') +
        '（VNF 可起页数以此为准）');
    }
  }
  $('dep-pool-note').textContent = lines.length
    ? '资源池现状：' + lines.join('；') + '——起 VNF 前确认空闲量足够。'
    : '资源池读数取不到，起 VNF 前请到「系统 · 资源池」核对。';
}

function depCollect() {
  const name = $('dep-name').value.trim();
  const image = $('dep-image').value;
  const vcpu = Number($('dep-vcpu').value);
  const mem = Number($('dep-mem').value);
  if (depKind === 'vm') {
    const out = {
      name, image,
      vcpu: { count: vcpu },
      memory: { size_mb: mem },
      interfaces: depNicRows().map((r) => ({ name: r.nic, type: r.type, virtual_switch: r.vs })),
      autostart: $('dep-autostart').checked,
    };
    if ($('dep-serial').checked) out.serial_console = true;
    if ($('dep-disk-mode').value === 'image') out.disks = [{ name: 'disk0', image }];
    else out.disks = [{ name: 'disk0', size_gb: Number($('dep-disk-size').value) }];
    return out;
  }
  const out = {
    name, image,
    vcpu,
    memory_mb: mem,
    interfaces: depNicRows().map((r) => ({ name: r.nic, type: r.type, virtual_switch: r.vs })),
  };
  const cmd = $('dep-cmd').value.trim();
  if (cmd) out.command = cmd;
  const rp = $('dep-restart').value;
  if (rp) out.restart_policy = rp;
  return out;
}

// 提示级预校验：只拦明显缺漏，规则全集在服务端。
function depPrecheck(body) {
  const errs = [];
  if (!body.name) errs.push('名称必填');
  else if (!/^[a-zA-Z][a-zA-Z0-9_-]*$/.test(body.name)) errs.push('名称以字母开头，只含字母/数字/-/_（与命令行一致）');
  if (!body.image) errs.push('请选择镜像（「镜像库」页可导入）');
  if (depKind === 'vm') {
    if (!(body.vcpu.count >= 1)) errs.push('vCPU 至少 1');
    if (!(body.memory.size_mb >= 128)) errs.push('内存至少 128 MB');
  } else if (!(body.memory_mb >= 0)) errs.push('内存不合法');
  return errs;
}

function depNicRows() {
  const rows = [];
  for (const tr of $('dep-nic-table').querySelectorAll('tbody tr')) {
    const [nic, type, vs] = tr.querySelectorAll('select,input');
    if (!nic) continue;
    const nicIn = tr.querySelector('input');
    const sels = tr.querySelectorAll('select');
    rows.push({ nic: nicIn.value.trim(), type: sels[0].value, vs: sels[1].value });
  }
  return rows.filter((r) => r.nic || r.vs);
}

function depAddNicRow(pre) {
  const tbody = $('dep-nic-table').querySelector('tbody');
  const tr = document.createElement('tr');
  const nic = el('input', { value: (pre && pre.nic) || '', placeholder: 'eth0' });
  const type = el('select', {});
  const types = depKind === 'vm' ? ['vhost-user', 'sriov-vf'] : ['memif'];
  for (const t of types) type.appendChild(el('option', { value: t, text: t }));
  const vs = el('select', {});
  vs.appendChild(el('option', { value: '', text: '（未挂接）' }));
  for (const s of depData.vss) vs.appendChild(el('option', { value: s.name, text: s.name }));
  if (pre) { nic.value = pre.nic; type.value = pre.type; vs.value = pre.vs; }
  const del = el('button', { class: 'ghost', text: '删', onclick: () => tr.remove() });
  for (const c of [nic, type, vs, del]) {
    const td = document.createElement('td');
    td.appendChild(c);
    tr.appendChild(td);
  }
  tbody.appendChild(tr);
}

// 静态拓扑预览：左侧新节点，右侧每个挂接交换机一个图标，连线 = 网卡→交换机。
function depDrawTopo() {
  const svg = $('dep-topo');
  svg.textContent = '';
  const NS = 'http://www.w3.org/2000/svg';
  const node = (tag, attrs) => {
    const n = document.createElementNS(NS, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    return n;
  };
  const nics = depNicRows();
  svg.appendChild(node('rect', { x: 20, y: 60, width: 120, height: 60, rx: 8,
    fill: '#e6f7fd', stroke: '#049fd9' }));
  const label = node('text', { x: 80, y: 95, 'text-anchor': 'middle', fill: '#1a2b3c', 'font-size': 13 });
  label.textContent = ($('dep-name').value || '新部署') + '（' + (depKind === 'vm' ? 'VM' : '容器') + '）';
  svg.appendChild(label);
  const vssList = [...new Set(nics.filter((n) => n.vs).map((n) => n.vs))];
  vssList.forEach((vsName, i) => {
    const y = 30 + i * 45;
    svg.appendChild(node('rect', { x: 340, y, width: 150, height: 34, rx: 6,
      fill: '#f8fafc', stroke: '#d9dee5' }));
    const t = node('text', { x: 415, y: y + 22, 'text-anchor': 'middle', fill: '#1a2b3c', 'font-size': 12 });
    t.textContent = '虚拟交换机 ' + vsName;
    svg.appendChild(t);
    const attached = nics.filter((n) => n.vs === vsName).map((n) => n.nic || 'nic');
    svg.appendChild(node('line', { x1: 140, y1: 90, x2: 340, y2: y + 17,
      stroke: '#049fd9', 'stroke-width': 1.5 }));
    const edge = node('text', { x: 240, y: (90 + y + 17) / 2 - 4, fill: '#5f7183', 'font-size': 11 });
    edge.textContent = attached.join(', ');
    svg.appendChild(edge);
  });
  if (!vssList.length) {
    const t = node('text', { x: 260, y: 95, 'text-anchor': 'middle', fill: '#5f7183', 'font-size': 12 });
    t.textContent = '（未挂接任何交换机——部署后无业务网络）';
    svg.appendChild(t);
  }
}

function depFillSummary() {
  const body = depCollect();
  const pairs = [
    ['类型', depKind === 'vm' ? '虚拟机（VM）' : '容器'],
    ['名称', body.name],
    ['镜像', body.image],
  ];
  if (depKind === 'vm') {
    pairs.push(['vCPU', body.vcpu.count], ['内存 (MB)', body.memory.size_mb],
      ['磁盘', body.disks[0].image ? '从镜像 ' + body.disks[0].image + ' 克隆' : '空盘 ' + body.disks[0].size_gb + ' GB'],
      ['自动启动', body.autostart ? '是' : '否'],
      ['串口控制台', body.serial_console ? '启用' : '禁用']);
  } else {
    pairs.push(['vCPU', body.vcpu || '（缺省）'], ['内存 (MB)', body.memory_mb || '（缺省）'],
      ['命令', body.command || '（镜像缺省）'], ['重启策略', body.restart_policy || '（缺省）']);
  }
  pairs.push(['网卡', body.interfaces.length
    ? body.interfaces.map((n) => n.name + ' → ' + n.type + ' @ ' + (n.virtual_switch || '未挂接')).join('；')
    : '（无）']);
  const dl = $('dep-summary');
  dl.textContent = '';
  for (const [k, v] of pairs) {
    dl.appendChild(el('dt', { text: k }));
    dl.appendChild(el('dd', { text: String(v) }));
  }
}

async function depSubmit() {
  const body = depCollect();
  const errs = depPrecheck(body);
  if (errs.length) {
    depMsg('请先修正：' + errs.join('；'), true);
    return;
  }
  const what = depKind === 'vm' ? '虚拟机 ' + body.name : '容器 ' + body.name;
  return uiConfirm('部署 ' + what, {
    tier: 'low',
    paragraphs: ['把 ' + what + ' 的部署声明写入候选配置？本操作只写候选，' +
      '不会立即创建实例——请到「配置」页复核后提交生效。'],
    cli: (depKind === 'vm' ? 'set virtual-machine-functions ' : 'set container-functions ') + body.name + ' …',
  }, async () => {
    try {
      const r = await api(depKind === 'vm' ? '/virtual-machine-functions' : '/container-functions',
        { method: 'POST', body: JSON.stringify(body) });
      const out = $('dep-result');
      out.hidden = false;
      out.textContent = '已写入部署候选（revision ' + (r && r.revision != null ? r.revision : '—') +
        '）。\n下一步：到「配置」页复核并提交；提交后实例才会创建' +
        (depKind === 'vm' && body.autostart ? '并自动启动。' : '。');
      depMsg('部署候选已写入。', false);
      return out.textContent;
    } catch (e) {
      depMsg('写入失败：' + apiErrText(e, '请按提示修正后重试。'), true);
      throw e;
    }
  });
}

export const views = {
  deploy: {
    async render(d) {
      pageWarn(d);
      depKind = '';
      depData = {
        images: rowsOf(d['/images']),
        profiles: rowsOf(d['/vm-image-profiles']),
        vss: rowsOf(d['/virtual-switches']),
        pools: d['/resource-pools'] || null,
        hp: d['/system/hugepages'] || null,
      };
      depFillImages();
      depFillPools();
      $('dep-nic-table').querySelector('tbody').textContent = '';
      $('dep-result').hidden = true;
      depMsg('', false);
      depShow(1);
    },
  },
};

// ---- 接线（模块求值期执行；type=module 在 DOM 解析后运行，元素必已存在）----
$('dep-type-vm').addEventListener('click', () => { depKind = 'vm'; $('dep-vm-only').hidden = false; $('dep-ct-only').hidden = true; depMsg('已选：虚拟机。继续第 ② 步。', false); depShow(2); });
$('dep-type-ct').addEventListener('click', () => { depKind = 'ct'; $('dep-vm-only').hidden = true; $('dep-ct-only').hidden = false; depMsg('已选：容器。继续第 ② 步。', false); depShow(2); });
$('dep-next-2').addEventListener('click', () => { if (depRequireKind()) depShow(3); });
$('dep-back-3').addEventListener('click', () => depShow(2));
$('dep-next-3').addEventListener('click', () => depShow(4));
$('dep-back-4').addEventListener('click', () => depShow(3));
$('dep-add-nic').addEventListener('click', () => depAddNicRow());
$('dep-image').addEventListener('change', depFillImages);
const submit = wbtn({ id: 'dep-submit', text: '写入部署候选' });
submit.addEventListener('click', depSubmit);
```

⚠️ 实现时与既有机制对齐三处：① `wbtn` 若需要绑进 DOM 的既有按钮（index.html 已有 id=dep-submit），用 `$('dep-submit').setAttribute('data-write','')` 或在 index.html 静态写 `data-write`（**首选静态**，与现网做法一致）；② `api()` 的 body 传参签名以 core/api.js 实现为准（现网 `api(path,{method,headers,body})`）；③ `uiConfirm` 的 opts 形状以 core/dom.js 实现为准（tier/paragraphs/bullets/cli/countdown）。

- [ ] **Step 3: 分级确认桩登记**

`web-console-confirm-selftest.js` 动作表追加（格式随表内既有条目）：

```js
  {
    label: '部署向导：写入部署候选（低危）',
    pre: (ctx) => {
      vm.runInContext('depKind = "vm"', ctx);
      // 桩 fetch 对 POST /virtual-machine-functions 回 201 {committed:false,revision:8}
    },
    tier: 'low',
    req: { method: 'POST', url: /\/virtual-machine-functions$/ },
    // 点击入口：dep-submit（桩 DOM 按需造节点，见 stub 的 unknown-id 分支）
  },
```

（tier:low 的既有断言 = 确认前零写请求、低危无确认词/倒计时闸门、取消后零请求；若桩对未知 id 自动造节点的分支覆盖不到 select/input 取值，给 pre 里补最小桩值。）

- [ ] **Step 4: 校验**

Run: `node --check internal/api/ui/js/views/deploy.js`
Run: `bash contrib/scripts/web-console-confirm-selftest.sh`
Run: `go test ./internal/api/ -count=1`（ui_coverage 自动覆盖新 api 字面量——deploy 路由 endpoints 已在契约）
Expected: 全绿。

- [ ] **Step 5: Commit**

```bash
git add internal/api/ui contrib/scripts
git commit -m "feat(web): 部署向导——四步表单写部署候选（VM/容器）+ 分级确认桩登记"
```

---

### Task 7: 全量验证 + 浏览器黑盒验收

**Files:**
- Create: `internal/api/ui_devserver_manual_test.go`（env 门控的手动验收服务）
- Create: `docs/images/web-redesign-*.png`（验收截图）

**Interfaces:**
- Consumes: 前六批全部产出。
- Produces: 验收证据（截图 + 命令输出记录进本计划的勾选）。

- [ ] **Step 1: 手动验收服务（env 门控，CI 不跑）**

```go
package api

// 手动验收服务：NFVIS_UI_DEV_ADDR=127.0.0.1:18080 go test ./internal/api/ -run TestUIDevServerManual -v
// 起真实 Server（内存引擎 + 桩后端），供浏览器黑盒走查 UI 重构——CI/常规测试不受影响
//（未设 env 时 t.Skip）。
import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestUIDevServerManual(t *testing.T) {
	addr := os.Getenv("NFVIS_UI_DEV_ADDR")
	if addr == "" {
		t.Skip("未设 NFVIS_UI_DEV_ADDR：手动验收服务不随常规测试运行")
	}
	ts := newTestServerOpts(t, Options{})
	t.Cleanup(func() { time.Sleep(50 * time.Millisecond) }) // 让最后的请求落地
	t.Logf("UI 验收服务就绪: %s （ui 路径: %s/ui/）——按 Ctrl-C 结束 go test 进程收尾", ts.URL, ts.URL)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ts.URL+"/ui/", http.StatusFound)
	})
	_ = mux // ts.URL 已可直达；占位防误用
	select {} // 挂住进程直到人工终止
}
```

（`newTestServerOpts` 的能力以现网签名为准；若其监听地址不可外部访问，改 `Options` 里可设 addr 的字段或用 httptest.NewServer 包装同款 handler 链——实现时看 `ui_test.go` 现用构造，保持一致。）

- [ ] **Step 2: make check 全绿**

Run: `make check`（Windows 上逐项跑 Makefile 的 toolcheck 四行 bash + `go vet`/`go test`/cover 门槛，按 make 缺失时逐命令等价执行）
Expected: 七项 toolcheck + go test 全绿。

- [ ] **Step 3: 浏览器黑盒（browser-use 技能）**

用 Step 1 的服务（或真机 dev 构建）逐项走查并截图：
1. 登录页（浅色）→ 登录 → 总览；
2. 左侧树五个分组齐全、折叠记忆、当前页高亮、详情页回列表高亮；
3. 33 页逐页打开无 JS 报错（console 无未捕获异常）；
4. 部署向导四步：选 VM → 填名称/镜像/资源 → 加网卡选交换机（拓扑预览连线正确）→ 摘要 → 写入候选成功文案指向配置页；缺名称提交被预校验拦下；
5. 角色矩阵：read-only 登录（树里无写页、role-notice 出现）、operator（写控件隐藏）；
6. 回归：配置页编辑/确认对话框分档、VM 详情分栏、容器 exec/shell 分栏、noVNC 按钮在位（连接在无后端时如实报错即可）。
截图存 `docs/images/web-redesign-{login,sidebar,deploy-wizard,deploy-topo,roles}.png`。

- [ ] **Step 4: 收尾提交**

```bash
git add internal/api/ui_devserver_manual_test.go docs/images/web-redesign-*.png
git commit -m "test(web): UI 验收手动服务 + 浏览器黑盒证据截图"
```

---

## Self-Review 记录

- **规格覆盖**：spec §3 布局/分组（Task 1/2/3）、§4 模块结构（Task 4/5）、§5 浅色主题（Task 2）、§6 部署向导（Task 6）、§7 守护适配（Task 4/5 内嵌）、§8 测试验收（Task 7）。spec §3 提到的 routes.json `nav` 列按 YAGNI 裁掉（所有非 detail 路由都进树，无需开关）——已回写 spec。
- **占位符扫描**：模块拆分为既有代码搬移，以行号地图+符号清单给出（可执行）；新代码（树渲染/bundle 加载器/向导/验收服务）全部给全文。无 TBD。
- **类型一致性**：core 导出名与 views import 名逐一对过（访问器化清单是唯一易错点，Step 内已显式标注）；wbtn/uiConfirm/api 签名以「与 core 实现对齐」的显式提醒兜底。
