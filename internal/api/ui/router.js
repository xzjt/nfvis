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

// hash → 路由。不是 `#/…` 或表里没有的，返回 null（调用方提示并回总览）。
export function match(hash) {
  const h = hash && hash.indexOf('#/') === 0 ? hash : '#/';
  return routes.find((r) => r.path === h) || null;
}

export function navigate(hash) {
  if (location.hash !== hash) { location.hash = hash; return; }
  render();
}

// 渲染当前 hash 对应的页面：切页 → 导航/面包屑 → 按路由取数 → 交给该页的视图。
// 取数失败已在 softLoad 里各自降级（单个端点失败不拖垮整页）。
export async function render() {
  try {
    const route = match(location.hash);
    if (!route) { notFound(location.hash); return; }
    for (const s of document.querySelectorAll('.page')) s.hidden = s.id !== 'page-' + route.view;
    renderNav(route);
    renderCrumb(route);
    setPollRoute(route);
    const view = VIEWS[route.view];
    if (!view) { showGlobalError('页面未实现：' + route.view); return; }
    await view.render(await softLoad(route.endpoints));
    if (pendingNotice) { showNotice(pendingNotice); pendingNotice = ''; clearOnNextRender = true; }
    else if (clearOnNextRender) { clearNotice(); clearOnNextRender = false; }
  } catch (e) {
    showGlobalError('页面加载失败：' + e.message);
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

// 导航只列路由表里已有的页面：一级项（面包屑只有一段）直接列，其余按面包屑首段分组。
function renderNav(route) {
  const nav = document.getElementById('nav');
  nav.textContent = '';
  const groups = new Map(); // 组名 → 该组的容器（顺序即路由表里的出现顺序）
  for (const r of routes) {
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
    box.appendChild(navLink(r, route));
  }
}

function navLink(r, route) {
  const a = document.createElement('a');
  a.href = r.path;
  a.textContent = r.title;
  if (r.path === route.path) a.setAttribute('aria-current', 'page');
  return a;
}

function renderCrumb(route) {
  document.getElementById('crumb').textContent = route.breadcrumb.join(' › ');
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
