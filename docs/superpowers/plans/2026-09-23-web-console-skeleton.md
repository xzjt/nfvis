# Web 控制台「骨架刀」实施计划（hash 路由 + 页面骨架 + 每页按需取数）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把单页控制台改成「资源域一级 + 对象详情二级」的骨架：hash 路由 + 顶栏导航 + 面包屑 + 现有 16 张卡按域搬进各页 + 每页只拉自己声明的端点（总览不再全量刷新，破坏性动作移出总览）。

**Architecture:** 路由表是**数据**（`routes.json`，Go 守护与前端共用一份，真 JSON → 两边都用真解析器）；路由是**代码**（`router.js`：hash 解析 → 匹配路由 → 按 `endpoints[]` 取数 → 调 `view` 渲染 → 导航/面包屑/404）；页面是**容器**（`index.html` 里每个路由一个 `<section class="page">`，卡原样搬入）；渲染函数留在 `app.js`，用 `VIEWS` 注册表按 `view` 名索引。

**Tech Stack:** 原生 HTML/CSS/JS（免构建，浏览器原生 ES modules，产物入库）；Go `embed` 同源托管；守护用 Go 测试（`internal/api/*_test.go`）；验收用 Browser Use（浏览器操作 + DOM 快照 + 截图 + 与独立事实源对照）。

**Spec:** `docs/superpowers/specs/2026-09-23-web-console-ia-design.md`（§5 信息架构、§9 刷新与性能、§11 守护与验收、§12 刀 1）

## Global Constraints

- **免构建**：不引 npm/打包器；新增前端文件直接入库（`internal/api/ui/`），CI 与发版仍不依赖 node。
- **同源托管**：静态资源仍由 `GET /api/v1/ui/` 提供；**不新增服务端路由**（`routes_contract` 守护不受影响）。
- **契约先行**：本刀**不新增端点**，`routes.json` 里只出现已存在的契约路径（由守护断言）。
- **界面覆盖守护不许放松**：`ui_coverage_test.go` 的已接路径 = `routes.json` 的 `endpoints[]` ∪ JS 里的 `api('/x')` 字面量（动作类端点如 `:start` 只出现在后者）。
- **`uiNotWired` 语义不变**：本刀不减条目（28 条照旧），收口发生在刀 4。
- **操作者可见文本**（`.html/.js/.css`）不得含内部引用（`FR-xxx`、`决策 #nn`、`§n`、`M4-8`）——`user_text` 守护判。
- **验收口径（决策 #141）**：每个交付的功能都要在浏览器里真的操作一遍，并留"操作前后 DOM 快照 + 关键步骤截图 + 与独立事实源对照"三类证据。
- **环境**：真机验收用**明文开发态实例**（已装二进制或 main 构建 + 生产库副本，`0.0.0.0:8443`），收尾必须 `systemctl restart vpp` 清残留并起回已装服务、核对配置库指纹。

## 卡 → 页面映射（16 张卡，`index.html` 现 43~367 行）

| 页面路由 | 标题 | 卡（按 index.html 现有顺序） |
|---|---|---|
| `#/` | 总览 | 系统（含组件版本）、数据面、告警、最近事件 |
| `#/compute/vms` | 虚拟机 | 虚拟机 |
| `#/compute/containers` | 容器 | 容器 |
| `#/compute/images` | 镜像 | 镜像 |
| `#/network` | 网络对象 | 网络对象（VRF/ACL/NAT/聚合/LLDP/QoS/SPAN 七张子表，本刀保持一卡；拆域在刀 2） |
| `#/network/switches` | 虚拟交换机 | 虚拟交换机 |
| `#/system/pools` | 资源池 | 资源池 |
| `#/system/interfaces` | 接口 | 接口 |
| `#/config` | 配置 | 配置 |
| `#/ops/actions` | 运维动作 | 运维动作（含归档下载与电源组） |
| `#/ops/audit` | 审计日志 | 审计日志 |
| `#/ops/diagnostics` | 诊断 | 诊断 |
| `#/ops/capture` | 抓包 | 抓包 |

> 破坏性动作（重启主机/关机/重启数据面/重签证书）随「运维动作」卡落到 `#/ops/actions`，**总览页不再出现**——这就是本刀的"总览瘦身"。

---

### Task 1: 路由表 `routes.json` + Go 守护

**Files:**
- Create: `internal/api/ui/routes.json`
- Create: `internal/api/ui_routes_test.go`
- Test: 同上（Go 测试即守护）

**Interfaces:**
- Produces: `routes.json` 的形状 —— `{ "routes": [ { "path": "#/compute/vms", "title": "虚拟机", "view": "vms", "breadcrumb": ["计算","虚拟机"], "endpoints": ["/virtual-machine-functions"], "poll": 5000 } ] }`；后续任务按 `view` 索引 `VIEWS`、按 `endpoints` 取数。

- [ ] **Step 1: 写失败测试**（`internal/api/ui_routes_test.go`）

```go
package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// routes.json 是控制台路由表的**唯一真源**：前端用它渲染导航与取数，守护用它核对契约。
type uiRoute struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	View       string   `json:"view"`
	Breadcrumb []string `json:"breadcrumb"`
	Endpoints  []string `json:"endpoints"`
	Poll       int      `json:"poll"`
}

func loadUIRoutes(t *testing.T) []uiRoute {
	t.Helper()
	b, err := os.ReadFile("ui/routes.json")
	if err != nil {
		t.Fatalf("读取 routes.json: %v", err)
	}
	var doc struct {
		Routes []uiRoute `json:"routes"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("routes.json 不是合法 JSON（守护要求真解析器，不用正则）: %v", err)
	}
	if len(doc.Routes) == 0 {
		t.Fatal("routes.json 里没有任何路由")
	}
	return doc.Routes
}

// ① 每条路由的 path 以 `#/` 开头且唯一；② 每个 endpoint 必须在契约里；
// ③ view 必须在 app.js 的 VIEWS 注册表里出现（否则运行期渲染不出来）。
func TestUIRoutesAreConsistent(t *testing.T) {
	routes := loadUIRoutes(t)
	seen := map[string]bool{}
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, "#/") {
			t.Errorf("路由 %q 必须以 #/ 开头（hash 路由）", r.Path)
		}
		if seen[r.Path] {
			t.Errorf("路由 %q 重复", r.Path)
		}
		seen[r.Path] = true
		if r.Title == "" || r.View == "" {
			t.Errorf("路由 %q 缺少 title 或 view", r.Path)
		}
		if len(r.Endpoints) == 0 {
			t.Errorf("路由 %q 没有声明任何 endpoint（每页必须自报取数范围）", r.Path)
		}
	}
	app, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读取 app.js: %v", err)
	}
	for _, r := range routes {
		if !strings.Contains(string(app), "'"+r.View+"':") && !strings.Contains(string(app), `"`+r.View+`":`) {
			t.Errorf("路由 %q 的 view %q 在 app.js 的 VIEWS 注册表里找不到", r.Path, r.View)
		}
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/api/ -run TestUIRoutesAreConsistent -v`
Expected: FAIL（`读取 routes.json: open ui/routes.json: no such file or directory`）

- [ ] **Step 3: 写 `routes.json`**（13 条路由，端点全部取自现有契约）

```json
{
  "routes": [
    { "path": "#/", "title": "总览", "view": "overview", "breadcrumb": ["总览"],
      "endpoints": ["/system/status", "/system/version", "/vpp/status", "/alarms"], "poll": 5000 },
    { "path": "#/compute/vms", "title": "虚拟机", "view": "vms", "breadcrumb": ["计算", "虚拟机"],
      "endpoints": ["/virtual-machine-functions"], "poll": 5000 },
    { "path": "#/compute/containers", "title": "容器", "view": "containers", "breadcrumb": ["计算", "容器"],
      "endpoints": ["/container-functions"], "poll": 5000 },
    { "path": "#/compute/images", "title": "镜像", "view": "images", "breadcrumb": ["计算", "镜像"],
      "endpoints": ["/images"], "poll": 0 },
    { "path": "#/network", "title": "网络对象", "view": "network", "breadcrumb": ["网络", "网络对象"],
      "endpoints": ["/vrfs", "/acls", "/nat", "/bonds", "/protocols/lldp/neighbors", "/qos/policies", "/port-mirroring"], "poll": 5000 },
    { "path": "#/network/switches", "title": "虚拟交换机", "view": "switches", "breadcrumb": ["网络", "虚拟交换机"],
      "endpoints": ["/virtual-switches"], "poll": 5000 },
    { "path": "#/system/pools", "title": "资源池", "view": "pools", "breadcrumb": ["系统", "资源池"],
      "endpoints": ["/resource-pools"], "poll": 5000 },
    { "path": "#/system/interfaces", "title": "接口", "view": "interfaces", "breadcrumb": ["系统", "接口"],
      "endpoints": ["/interfaces"], "poll": 5000 },
    { "path": "#/config", "title": "配置", "view": "config", "breadcrumb": ["配置"],
      "endpoints": ["/configuration"], "poll": 0 },
    { "path": "#/ops/actions", "title": "运维动作", "view": "ops", "breadcrumb": ["运维", "动作"],
      "endpoints": ["/system/backup", "/system/tech-support"], "poll": 0 },
    { "path": "#/ops/audit", "title": "审计日志", "view": "audit", "breadcrumb": ["运维", "审计"],
      "endpoints": ["/audit-logs?limit=50"], "poll": 5000 },
    { "path": "#/ops/diagnostics", "title": "诊断", "view": "diagnostics", "breadcrumb": ["运维", "诊断"],
      "endpoints": ["/system/logs"], "poll": 0 },
    { "path": "#/ops/capture", "title": "抓包", "view": "capture", "breadcrumb": ["运维", "抓包"],
      "endpoints": ["/vpp/capture"], "poll": 0 }
  ]
}
```

> 注：`/diagnostics/*` 是 POST 动作端点（非页面取数），故 `diagnostics` 路由的 `endpoints` 为空——守护因此只对非空 `endpoints` 断言"在契约里"，空数组合法（见 Step 1 的断言只查 `len==0` 报错的是"没有声明"…**注意**：本步实现里 `len(r.Endpoints)==0` 会报错，故 `diagnostics` 路由改为声明 `["/system/logs"]`（该端点确实存在，且"刷新服务端日志"按钮就是取它））。

- [ ] **Step 4: 跑测试看它通过**

Run: `go test ./internal/api/ -run TestUIRoutesAreConsistent -v`
Expected: PASS

- [ ] **Step 5: 红-绿验证（守护真的会红）**

把 `routes.json` 里某条 `endpoints` 改成不存在的路径（如 `/nope`），跑测试应 FAIL；改回后 PASS。
（本轮先只做"view 存在 + 形状"两项断言；"endpoint 在契约里"的断言在 Task 3 与覆盖守护一起加，避免与现有 `ui_coverage_test.go` 重复。）

- [ ] **Step 6: 提交**

```bash
git add internal/api/ui/routes.json internal/api/ui_routes_test.go
git commit -m "test(web): 路由表 routes.json + 形状守护（控制台 IA 骨架刀之一）"
```

---

### Task 2: 页面骨架（`index.html` 分页 + 导航 + 面包屑）

**Files:**
- Modify: `internal/api/ui/index.html`（16 张卡按上表搬进 `<section class="page">`；顶栏加导航）
- Modify: `internal/api/ui/style.css`（`.page[hidden]` 与导航样式）

**Interfaces:**
- Produces: 每个路由一个 `<section class="page" id="page-<view>" hidden>`；顶栏 `<nav id="nav">` 与 `<div id="crumb">` 供 `router.js` 填充。

- [ ] **Step 1: 顶栏加导航与面包屑**

在 `index.html` 的 `<header class="topbar">` 内、`user-line` 之前插入：

```html
<nav id="nav" class="nav" aria-label="主导航"></nav>
<div id="crumb" class="crumb" aria-label="面包屑"></div>
```

- [ ] **Step 2: 把 16 张卡按映射表包进页面容器**

在 `<div class="grid">`（现有卡片容器）内部，按上表把卡切成 13 个容器（每个容器含该页的卡，**卡的内容一字不改**）：

```html
<section class="page" id="page-overview" hidden>
  <!-- 系统卡（含组件版本）、数据面卡、告警卡、最近事件卡：原样搬入 -->
</section>
<section class="page" id="page-vms" hidden>…虚拟机卡…</section>
…
<section class="page" id="page-diagnostics" hidden>…诊断卡…</section>
<section class="page" id="page-capture" hidden>…抓包卡…</section>
```

**判定标准**：`grep -c '<article class="card' index.html` 仍为 16；每张卡只出现在一个 `.page` 里。

- [ ] **Step 3: 样式**

`style.css` 追加：`.page { display: grid; gap: 14px; }`、`.page[hidden] { display: none; }`（**必须显式写 `[hidden]`**——round39 的教训：作者样式会盖掉 `hidden` 属性）、`.nav a` / `.nav a[aria-current="page"]`、`.crumb`。

- [ ] **Step 4: 语法自检**

Run: `node --check internal/api/ui/app.js`（前端无测试运行器，语法用 node 校验；CI 不依赖 node）
Expected: 无输出（通过）

- [ ] **Step 5: 提交**

```bash
git add internal/api/ui/index.html internal/api/ui/style.css
git commit -m "feat(web): 控制台页面骨架——16 张卡按域分页 + 导航/面包屑容器（骨架刀之二）"
```

---

### Task 3: `router.js`（hash 路由 + 按路由取数 + 404）

**Files:**
- Create: `internal/api/ui/router.js`
- Modify: `internal/api/ui/index.html`（`<script type="module" src="router.js">`，`app.js` 改为导出 `VIEWS` 与取数助手）
- Modify: `internal/api/ui/app.js`（`loadAll()` 拆成 `LOADERS`；`VIEWS` 注册表；`enterApp` 改为启动路由）

**Interfaces:**
- Consumes: `routes.json`（Task 1 的形状）、`VIEWS`（app.js 导出）、`api(path)`、`$`。
- Produces: `window.nfvis = { routes, navigate(path), current() }`（供动作后刷新当前页）；`renderNav()`、`renderCrumb(route)`。

- [ ] **Step 1: 写 `router.js`**

```js
// 控制台路由：hash 路由 + 按路由声明的端点取数 + 导航/面包屑。
// 路由表是数据（routes.json，Go 守护与前端共用），视图是代码（app.js 的 VIEWS 注册表）。
import { VIEWS, softLoad } from './app.js';

let routes = [];
let current = null;

export async function loadRoutes() {
  const res = await fetch('routes.json', { cache: 'no-store' });
  routes = (await res.json()).routes;
  return routes;
}

export function match(hash) {
  const h = hash && hash.startsWith('#/') ? hash : '#/';
  return routes.find((r) => r.path === h) || null;
}

export function navigate(hash) {
  if (location.hash !== hash) { location.hash = hash; return; }
  render();
}

export async function render() {
  const route = match(location.hash);
  if (!route) {
    showNotFound(location.hash);
    return;
  }
  current = route;
  for (const s of document.querySelectorAll('.page')) s.hidden = s.id !== 'page-' + route.view;
  renderNav(route);
  renderCrumb(route);
  const view = VIEWS[route.view];
  if (!view) { showGlobalError('页面未实现：' + route.view); return; }
  const data = await softLoad(route.endpoints);
  view.render(data);
}

function renderNav(route) {
  const nav = document.getElementById('nav');
  nav.textContent = '';
  for (const r of routes) {
    if (r.path === '#/' || r.breadcrumb.length < 2) {
      const a = document.createElement('a');
      a.href = r.path; a.textContent = r.title;
      if (r.path === route.path) a.setAttribute('aria-current', 'page');
      nav.appendChild(a);
    }
  }
  const groups = ['计算', '网络', '系统', '配置', '运维'];
  for (const g of groups) {
    const kids = routes.filter((r) => r.breadcrumb[0] === g);
    if (!kids.length) continue;
    const wrap = document.createElement('div');
    wrap.className = 'nav-group';
    wrap.appendChild(Object.assign(document.createElement('span'), { className: 'nav-title', textContent: g }));
    for (const r of kids) {
      const a = document.createElement('a');
      a.href = r.path; a.textContent = r.title;
      if (r.path === route.path) a.setAttribute('aria-current', 'page');
      wrap.appendChild(a);
    }
    nav.appendChild(wrap);
  }
}

function renderCrumb(route) {
  const c = document.getElementById('crumb');
  c.textContent = '';
  route.breadcrumb.forEach((seg, i) => {
    if (i) c.appendChild(document.createTextNode(' › '));
    const s = document.createElement('span');
    s.textContent = seg;
    c.appendChild(s);
  });
}

function showNotFound(hash) {
  const box = document.getElementById('global-error');
  box.textContent = '页面不存在：' + hash + '（已回到总览）';
  box.hidden = false;
  navigate('#/');
}

export function start() {
  window.addEventListener('hashchange', () => render());
  return render();
}
```

- [ ] **Step 2: `app.js` 拆取数 + 注册视图**

把 `loadAll()`（现 137~166 行）替换为**按端点取数**的通用助手 + 每页 loader：

```js
// 按路由声明的端点取数（各自降级，单个失败不拖垮整页）；返回 { path: data } 映射。
export async function softLoad(paths) {
  const out = {};
  await Promise.all((paths || []).map(async (p) => {
    out[p] = await soft(api(p));
  }));
  return out;
}

// 视图注册表：routes.json 的 view 名 → 渲染函数（键名与 routes.json 一一对应，由 Go 守护核对）。
export const VIEWS = {
  overview: { render: (d) => { /* 系统+数据面+告警+最近事件，复用现有 render 的对应片段 */ } },
  vms: { render: (d) => { /* 虚拟机卡 + vhost-user 口计数 */ } },
  containers: { render: (d) => { /* 容器卡 */ } },
  images: { render: (d) => { /* 镜像卡 */ } },
  network: { render: (d) => { /* 网络对象七张子表 */ } },
  switches: { render: (d) => { /* 虚拟交换机卡 + 成员口计数 */ } },
  pools: { render: (d) => { /* 资源池卡 */ } },
  interfaces: { render: (d) => { /* 接口卡 + 逐口统计 */ } },
  config: { render: () => { /* 配置卡：取数仍走 loadConfig() */ } },
  ops: { render: (d) => { /* 运维动作 + 归档下载 */ } },
  audit: { render: (d) => { /* 审计卡 */ } },
  diagnostics: { render: (d) => { /* 诊断卡 + 日志按钮 */ } },
  capture: { render: (d) => { /* 抓包卡 */ } },
};
```

- 现有 `render(...)`、`renderVSwitches`、`renderImages`、`renderNetworkObjects`、`renderAudit`、`renderCapture`、`renderArchives`、`renderVMStats` **原样复用**，只是改由各 view 调用（把它们从"全局大 render"变成"每页各自的 render"）。
- `enterApp()`：`await Promise.all([loadAll(), loadConfig()])` 改为 `await loadRoutes(); await startRouter();`（配置卡的数据由 `config` 视图自己拉）。
- 轮询：`startPolling()` 改为"每 `route.poll` 毫秒重跑当前路由的 render"；`poll === 0` 的页面不轮询。SSE 事件到达时仍触发当前路由重渲染（不再全量）。
- 动作后刷新：写操作成功后调 `nfvis.reload()`（= 当前路由重渲染），取代原来的 `loadAll()`。

- [ ] **Step 3: 语法自检 + 起开发态实例**

```bash
node --check internal/api/ui/router.js && node --check internal/api/ui/app.js
ssh nfvis-vm 'bash /root/ui-browse-up.sh'   # 已装二进制 + 生产库副本，明文 0.0.0.0:8443
```

- [ ] **Step 4: 浏览器逐页冒烟（Browser Use 操作）**

对 13 条路由逐条操作：直接打开 `http://192.168.155.129:8443/api/v1/ui/#/<path>` → 断言该页可见、其它页隐藏、面包屑正确、该页数据非空（DOM 快照）；再点顶栏导航切换一次（验证 `hashchange` 生效）。
**必须留证据**：`#/`、`#/compute/vms`、`#/config` 三页的截图 + 全部 13 页的 DOM 快照结论。

- [ ] **Step 5: 每页取数范围（与独立事实源对照）**

在浏览器里对若干页执行 `performance.getEntriesByType('resource')`（Playwright `evaluate`），断言：
- `#/compute/vms` 只出现 `/virtual-machine-functions`（+ 详情端点），**不出现** `/audit-logs`、`/vpp/capture`；
- `#/` 不出现 `/virtual-machine-functions`；
- `#/ops/audit` 出现 `/audit-logs?limit=50`。
（这是"每页只拉自己的端点"的机器证据，替代"看服务端日志"。）

- [ ] **Step 6: 提交**

```bash
git add internal/api/ui/router.js internal/api/ui/index.html internal/api/ui/app.js
git commit -m "feat(web): hash 路由 + 每页按需取数（总览不再全量刷新，破坏性动作移出总览）"
```

---

### Task 4: 界面覆盖守护切换到路由表

**Files:**
- Modify: `internal/api/ui_coverage_test.go`（已接路径来源 = `routes.json` 的 `endpoints[]` ∪ JS 的 `api('/x')` 字面量）

**Interfaces:**
- Consumes: `routes.json`（Task 1）、现有 `api('/x')` 提取逻辑。
- Produces: 覆盖断言不变（契约里每条路径要么已接、要么在 `uiNotWired` 里写明理由）。

- [ ] **Step 1: 改提取来源（保留现有正则提取，二者取并集）**

```go
// 已接 = 路由表声明的端点 ∪ JS 里的 api('/x') 字面量。
// 路由表是"页面取数"的真源；字面量覆盖"动作类"端点（如 /virtual-machine-functions/{name}:start）。
func wiredPathsFromRoutes(t *testing.T) []string { /* 读 ui/routes.json，取 endpoints，去掉查询串 */ }
```

- [ ] **Step 2: 加断言：路由表里的每个端点必须在契约里**

`TestUIRoutesEndpointsExistInContract`：`routes.json` 的每个 endpoint（去查询串）必须出现在 OpenAPI 路径集合里；不在 → 红（防止路由表写出幽灵端点）。

- [ ] **Step 3: 红-绿验证**

把 `routes.json` 的某个 endpoint 改成 `/nope` → `go test ./internal/api/ -run TestUIRoutes` FAIL；改回 → PASS。

- [ ] **Step 4: 全量门禁**

Run: `make check`
Expected: 全绿（决策条数、附录 A 结构、`openapi.json` 同步、`user_text`、覆盖守护都过）

- [ ] **Step 5: 提交**

```bash
git add internal/api/ui_coverage_test.go
git commit -m "test(web): 界面覆盖守护改从路由表提取端点（+ 幽灵端点断言）"
```

---

### Task 5: 收尾（手册、证据、真机验收、PR）

**Files:**
- Modify: `docs/NFViS-用户手册.md`（§10.12 控制台章节：改为"导航 + 每页做什么 + 深链"）
- Create: `docs/evidence/v1-closeout-round57-web-console-skeleton.txt`
- Modify: `docs/V1-收尾待办.md`（§0 记录本刀完成与下一步）

- [ ] **Step 1: 手册 §10.12 重写**（按界面上真实有的导航与页面写，含深链示例与"总览页不放破坏性动作"的说明）
- [ ] **Step 2: 浏览器全套操作验收**（按 §11.2 口径）：13 页逐页 + 深链分享（新标签直接打开带 hash 的 URL）+ 刷新保持 + 前进/后退 + 登录后回到目标地址 + 未知路由回总览 + 只读账号看不到写入口（用 read-only 账号登一次）
- [ ] **Step 3: 写证据文件**（含每页 DOM 快照结论、三张截图、`performance` 取数范围证据、与 CLI/配置库对照结果）
- [ ] **Step 4: 真机环境复原**：停开发态实例 → `systemctl restart vpp` → 起回已装服务 → 配置库指纹与备份逐项一致 → BD/端口/BVI ping 复核
- [ ] **Step 5: `make check` + 提交 + PR + CI 绿 + squash 合并**

---

## 自检（写完计划后核对）

- **Spec 覆盖**：§5.1 路由与深链（Task 3）、§5.2 路由表（Task 1）、§5.3 模板（Task 2 的页面容器；详情/表单/向导模板在刀 2/3）、§5.4 页面清单（Task 2 映射表，网络域细分与系统域新页留刀 2/4）、§9 刷新与性能（Task 3 Step 2/5）、§11.1 守护（Task 4）、§11.2 验收（Task 3 Step 4/5、Task 5 Step 2）、§12 刀 1（全部任务）。
- **占位符**：无 `TBD/TODO`；`VIEWS` 里的注释是"复用现有渲染函数"的指路，实现时按现有函数名照搬。
- **类型/命名一致**：`routes.json` 的字段（`path/title/view/breadcrumb/endpoints/poll`）在 Task 1、3、4 中一致；`softLoad(paths) -> {path: data}` 与 `VIEWS[view].render(data)` 一致；`nfvis.reload()` 在 Task 3 Step 2 定义、Task 5 验收使用。
