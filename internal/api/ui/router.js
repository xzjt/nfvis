// NFViS 控制台路由（hash 路由）。
//
// 三件事各归其位：路由表是**数据**（routes.json，与 Go 守护共用同一份真源）、
// 页面是**容器**（index.html 里每个路由一个 .page 段）、视图是**代码**（app.js 的 VIEWS 注册表）。
// 一次只显示当前路由那一页；取数只取该路由声明的端点（endpoints）；刷新节奏也由路由声明（poll）。
// 页面地址就是 `#/…`，可直接分享、收藏、前进后退——服务端不新增任何路径。

import { VIEWS, softLoad, setPollRoute, showGlobalError } from './app.js';

let routes = [];
let started = false;
// 未知路由的提示单独占一个元素（#route-notice）：显示在"回总览"那一次，
// 用户下一次导航时收起——不与页面取数失败的提示（#global-error）互相抹掉。
function showNotice(msg) {
  const n = document.getElementById('route-notice');
  n.textContent = msg;
  n.hidden = false;
}
function clearNotice() {
  const n = document.getElementById('route-notice');
  if (!n) return;
  n.textContent = '';
  n.hidden = true;
}

// 提示的生命周期：notFound 先记下（pendingNotice）→ 回总览那一次渲染里显示并置 clearOnNextRender
// → 用户下一次导航时才收起。这样它既不会被"回总览"这次渲染立刻抹掉，也不会常驻。
let pendingNotice = '';
let clearOnNextRender = false;

// 载入路由表（真 JSON：前端 JSON.parse、守护用 Go 的解析器，两边同一份）。
export async function loadRoutes() {
  const res = await fetch('routes.json', { cache: 'no-store' });
  if (!res.ok) throw new Error('HTTP ' + res.status);
  routes = (await res.json()).routes || [];
  return routes;
}

// hash → { route, params }。不是 `#/…` 或表里没有的，返回 null（调用方提示并回总览）。
//
// 参数化路由（`#/compute/vms/:name`）按**段**匹配：静态路由优先（先查全等，再查参数化，
// 免得 `#/network` 被 `#/network/:x` 抢走）；命中的 `:name` 段经 decodeURIComponent 放进
// params，供取数时展开端点里的 `{name}` 占位与视图判断"看的是哪个对象"。
export function match(hash) {
  const h = stripQuery(hash && hash.indexOf('#/') === 0 ? hash : '#/');
  const exact = routes.find((r) => r.path === h);
  if (exact) return { route: exact, params: {} };
  const segs = h.split('/');
  for (const r of routes) {
    if (r.path.indexOf(':') < 0) continue; // 只试参数化路由
    const pat = r.path.split('/');
    if (pat.length !== segs.length) continue;
    const params = {};
    let ok = true;
    for (let i = 0; i < pat.length; i++) {
      if (pat[i].charAt(0) === ':') {
        if (!segs[i]) { ok = false; break; } // 空段不匹配（`#/compute/vms/` 不是详情页）
        params[pat[i].slice(1)] = decodeSeg(segs[i]);
      } else if (pat[i] !== segs[i]) { ok = false; break; }
    }
    if (ok) return { route: r, params };
  }
  return null;
}

// 手改出的 `#/compute/vms/vnf-a?tab=snapshots` 这类地址：本刀不支持 Tab 深链，
// 但**不该因为多了一段查询串就判成"页面不存在"**——按对象级路由打开，Tab 落在默认那个。
function stripQuery(h) {
  const i = h.indexOf('?');
  return i < 0 ? h : h.slice(0, i);
}

// 非法转义（`%zz`）会让 decodeURIComponent 抛错：按原样用，交给服务端判"对象不存在"。
function decodeSeg(seg) {
  try { return decodeURIComponent(seg); } catch (e) { return seg; }
}

export function navigate(hash) {
  if (location.hash !== hash) { location.hash = hash; return; }
  render();
}

// 渲染当前 hash 对应的页面：切页 → 导航/面包屑 → 按路由取数 → 交给该页的视图。
// 取数失败已在 softLoad 里各自降级（单个端点失败不拖垮整页）。
export async function render() {
  try {
    const m = match(location.hash);
    if (!m) { notFound(location.hash); return; }
    const route = m.route;
    leavePage(route.path + '|' + JSON.stringify(m.params), route.view);
    for (const s of document.querySelectorAll('.page')) s.hidden = s.id !== 'page-' + route.view;
    renderNav(route);
    renderCrumb(route);
    renderRoleNotice(route);
    setPollRoute(route);
    const view = VIEWS[route.view];
    if (!view) { showGlobalError('页面未实现：' + route.view); return; }
    // params 作为第二个入参：视图据此知道"看的是哪个对象"，并从**同一串**端点键取值
    // （softLoad 的键就是路由表里声明的那一串，如 '/virtual-machine-functions/{name}'）。
    await view.render(await softLoad(route.endpoints, m.params), m.params);
    if (pendingNotice) { showNotice(pendingNotice); pendingNotice = ''; clearOnNextRender = true; }
    else if (clearOnNextRender) { clearNotice(); clearOnNextRender = false; }
  } catch (e) {
    showGlobalError('页面加载失败：' + e.message);
  }
}

// 当前页的"身份"（路由 path + 参数）与它对应的视图。
// 同一页的重渲染（轮询/事件/顶栏刷新）不算"离开"；**换页或换对象**才算——离开时给旧视图
// 一次收尾机会（详情页据此断开串口，避免 WebSocket 留在后台）。
let pageKey = '';
let pageView = '';

function leavePage(nextKey, nextView) {
  const leaving = pageKey !== nextKey;
  const prev = leaving ? VIEWS[pageView] : null;
  pageKey = nextKey;
  pageView = nextView;
  if (prev && typeof prev.leave === 'function') {
    try { prev.leave(); } catch (e) { /* 收尾失败不该阻塞切页 */ }
  }
}

// 重新渲染当前页（动作完成后刷新、点顶栏「刷新」）。
export function reload() { return render(); }

export function start() {
  if (!started) {
    window.addEventListener('hashchange', () => {
      // 未登录时不跟着 hash 走：那时没有 token，取数只会换来一串未认证。
      if (document.getElementById('main-view').hidden) return;
      render();
    });
    started = true;
  }
  return render();
}

// ---------- 导航与面包屑 ----------

// 导航只列路由表里已有的**页面**：一级项（面包屑只有一段）直接列，其余按面包屑首段分组。
// 对象详情页（detail: true）不进导航——它是对象级页面，逐个对象列出来会把导航撑爆；
// 它高亮的是自己的列表页（见 navBasePath）。
//
// 角色渲染（决策 #145，设计 §8）：read-only 账号**不列"写页"**（路由表里 write: true 的那几条），
// 导航里只剩只读页；深链落到写页时的说明由 renderRoleNotice 负责。
function isReadOnlyRole() {
  return document.body.classList.contains('role-readonly');
}

// 角色说明条（决策 #145）：read-only 账号落到"写页"上（深链/收藏/被导航过滤后手敲地址）时，
// 页面里的写入口已被 CSS 隐藏——这里讲清**为什么这页是空的、该找谁**，而不是让人以为界面坏了。
// 非写页/非只读角色时清空（元素本身的显示由 style.css 的 body.role-readonly 规则控制）。
function renderRoleNotice(route) {
  const n = document.getElementById('role-notice');
  if (!n) return;
  const on = !!route.write && isReadOnlyRole();
  // 元素本身的显示由 style.css 的 body.role-readonly.role-notice-on 规则决定：
  // 只有"只读角色 + 当前页是写页"两条同时成立才出现（只读页上不该出现这条说明）。
  document.body.classList.toggle('role-notice-on', on);
  n.textContent = on
    ? '当前账号是 read-only：本页（' + route.title +
      '）的动作入口已隐藏，页面内容按你的权限只读呈现。需要执行这些动作请用 operator / super-user 账号。'
    : '';
}

function renderNav(route) {
  const nav = document.getElementById('nav');
  nav.textContent = '';
  const active = navBasePath(route);
  const groups = new Map(); // 组名 → 该组的容器（顺序即路由表里的出现顺序）
  for (const r of routes) {
    if (r.detail) continue;
    if (r.write && isReadOnlyRole()) continue;
    const g = r.breadcrumb.length > 1 ? r.breadcrumb[0] : '';
    let box = nav;
    if (g) {
      if (!groups.has(g)) {
        const wrap = document.createElement('div');
        wrap.className = 'nav-group';
        wrap.appendChild(span('nav-title', g));
        nav.appendChild(wrap);
        groups.set(g, wrap);
      }
      box = groups.get(g);
    }
    box.appendChild(navLink(r, active));
  }
}

function navLink(r, activePath) {
  const a = document.createElement('a');
  a.href = r.path;
  a.textContent = r.title;
  if (r.path === activePath) a.setAttribute('aria-current', 'page');
  return a;
}

// 详情页的"上一级"= 沿路径逐段回退后**第一个在路由表里存在的祖先路由**：
// `#/compute/vms/:name` → `#/compute/vms`；网络对象详情（`#/network/vrfs/:name`）没有
// `#/network/vrfs` 这一层，要再退一段落到 `#/network`（VRF/ACL/聚合/QoS/SPAN 共用网络对象页）。
// 导航据此高亮、面包屑据此把末段做成回列表页的链接。
function navBasePath(route) {
  if (!route.detail) return route.path;
  const segs = route.path.split('/');
  while (segs.length > 1) {
    segs.pop();
    const p = segs.join('/');
    if (routes.some((r) => r.path === p && !r.detail)) return p;
  }
  return route.path;
}

// 面包屑：末段在路由表里有对应的列表页时做成链接（详情页据此回列表页——
// 列表行不再各自实现"返回"按钮）。列表页的末段没有更上一级路由，保持纯文本。
function renderCrumb(route) {
  const crumb = document.getElementById('crumb');
  crumb.textContent = '';
  const back = navBasePath(route);
  const parent = back === route.path ? null : routes.find((r) => r.path === back);
  route.breadcrumb.forEach((seg, i) => {
    if (i) crumb.appendChild(document.createTextNode(' › '));
    if (parent && i === route.breadcrumb.length - 1) {
      const a = document.createElement('a');
      a.href = parent.path;
      a.textContent = seg;
      crumb.appendChild(a);
      return;
    }
    crumb.appendChild(document.createTextNode(seg));
  });
}

function span(cls, text) {
  const n = document.createElement('span');
  n.className = cls;
  n.textContent = text;
  return n;
}

// 未知 hash：如实说一句，然后回总览（总览是登录后的落点）。
function notFound(hash) {
  pendingNotice = '页面不存在：' + (hash || '') + '（已回到总览）';
  navigate('#/');
}
