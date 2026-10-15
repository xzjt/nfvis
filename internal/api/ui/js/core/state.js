// NFViS 控制台共享件 · 会话与实时通道状态（从 app.js 拆出，逻辑逐字保留；Web 控制台重构 2026-10）。
//
// 跨模块可变状态（token/用户/事件缓存/轮询状态）**不直接 export let**——ES 模块的顶层
// let 对 import 方不可见重赋值，故一律访问器化；单向常量才直接导出。
//
// 关于实时刷新：浏览器的 EventSource **无法自定义请求头**，而 /events 需要
// Authorization，故这里用 fetch + 流式读取手工解析 SSE 帧（同样走头部传 token，
// 不把 token 放进 URL——URL 会进日志与浏览器历史）。流断了就按当前页声明的节奏轮询。

import { API, TOKEN_KEY } from './const.js';
import { $, el } from './dom.js';
import { fmtTime } from './format.js';

export const MAX_EVENTS = 20;

let token = sessionStorage.getItem(TOKEN_KEY) || '';
// 当前登录用户名（接口来源的会话持有者记作 `用户@api`，配置页据此判断候选是不是本会话的）。
let currentUser = '';
// 当前登录账号的权限类（决策 #304：非 super-user 只看得到自己所属 class 的「生效权限」入口）。
let currentClass = '';
let pollTimer = null;
let pollRoute = null;      // 当前路由（轮询节奏取它声明的 poll）
let pollPath = '';         // 已武装的路由地址（同一页的重复渲染不重置计时器）
let pollFallback = false;  // 只在实时通道断了之后才轮询
let streamAbort = null;
let reloadTimer = null;
let events = [];

// ---------- 访问器（跨模块可变状态的唯一通道） ----------

export function getToken() { return token; }
export function setToken(v) { token = v; }
export function getUser() { return currentUser; }
export function setUser(v) { currentUser = v; }
export function getCurrentClass() { return currentClass; }
export function setCurrentClass(v) { currentClass = v; }

export function getEvents() { return events; }
export function setEvents(v) { events = v; }
export function pushEvent(ev) {
  events.unshift(ev);
  events = events.slice(0, MAX_EVENTS);
}

// ---------- 最近事件（总览页事件卡） ----------

export function renderEvents() {
  const ul = $('events');
  ul.textContent = '';
  if (!events.length) {
    ul.appendChild(el('li', { class: 'muted', text: '（暂无事件）' }));
    return;
  }
  events.forEach((e) => ul.appendChild(el('li', {}, [
    el('span', { class: 'muted small', text: fmtTime(e.timestamp) + ' ' }),
    el('span', { text: e.type + (e.summary ? '：' + e.summary : '') }),
  ])));
}

// ---------- 实时通道与刷新 ----------

// reload（重渲染当前路由）在 boot.js：按需动态 import（与 goDetail 同一防环手法）。
async function reloadViaBoot() {
  const boot = await import('../boot.js');
  return boot.reload();
}

function setStream(text, cls) {
  const n = $('stream-state');
  n.textContent = text;
  n.className = 'pill ' + cls;
}

export function scheduleReload() {
  // 事件可能成串到达（一次提交触发多条）：合并成一次刷新。
  if (reloadTimer) return;
  reloadTimer = setTimeout(() => {
    reloadTimer = null;
    reloadViaBoot().catch((e) => showGlobalError(e.message));
  }, 400);
}

function summaryOf(ev) {
  const p = (ev && ev.payload) || {};
  const parts = [];
  ['resource', 'name', 'state', 'severity', 'message', 'revision'].forEach((k) => {
    if (p[k] !== undefined && p[k] !== null && p[k] !== '') parts.push(k + '=' + p[k]);
  });
  return parts.join(' ');
}

export async function startStream() {
  stopStream();
  const ctl = new AbortController();
  streamAbort = ctl;
  try {
    const res = await fetch(API + '/events', {
      headers: { Authorization: 'Bearer ' + token, Accept: 'text/event-stream' },
      signal: ctl.signal,
    });
    if (res.status === 401) {
      const boot = await import('../boot.js');
      boot.signOut('登录状态已失效，请重新登录。');
      return;
    }
    if (!res.ok || !res.body) throw new Error('HTTP ' + res.status);
    setStream('实时通道：已连接', 'pill-ok');
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, i);
        buf = buf.slice(i + 2);
        let data = '';
        frame.split('\n').forEach((line) => {
          if (line.indexOf('data:') === 0) data += line.slice(5).trim();
        });
        if (!data) continue;   // 心跳等注释帧
        try {
          const ev = JSON.parse(data);
          pushEvent({ type: ev.type, timestamp: ev.timestamp, summary: summaryOf(ev) });
          renderEvents();
          // 配置提交事件：顺带刷新配置卡（编辑中不打扰——避免覆盖正在改的文本）。
          if (ev.type === 'config-committed' && !isCfgEditing()) {
            const views = await import('../views/config.js');
            views.loadConfig().catch(() => {});
          }
          scheduleReload();
        } catch (e) { /* 非 JSON 帧：忽略，不影响后续 */ }
      }
    }
    // 流正常结束（服务端重启等）：退化为轮询。
    setStream('实时通道：已断开（改为轮询）', 'pill-warn');
    startPolling();
  } catch (e) {
    if (ctl.signal.aborted) return;
    setStream('实时通道：不可用（改为轮询）', 'pill-warn');
    startPolling();
  }
}

// config 编辑态由 views/config.js 持有：state 只经钩子读它（避免 state → config 的静态依赖）。
let cfgEditingProbe = () => false;
export function setCfgEditingProbe(fn) { cfgEditingProbe = fn; }
function isCfgEditing() { return cfgEditingProbe(); }

export function stopStream() {
  if (streamAbort) { streamAbort.abort(); streamAbort = null; }
}

// 轮询是实时通道不可用时的兜底：节奏取**当前路由声明的 poll**（毫秒，0 = 该页不轮询）。
export function armPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  const ms = (pollRoute && pollRoute.poll) || 0;
  if (!pollFallback || !ms) return;
  pollTimer = setInterval(() => {
    reloadViaBoot().catch((e) => showGlobalError(e.message));
  }, ms);
}

// 路由切换时同步轮询节奏（由 router.js 在每次渲染时调用）。
export function setPollRoute(route) {
  const path = route ? route.path : '';
  if (path === pollPath) return;
  pollPath = path;
  pollRoute = route;
  armPolling();
}

export function startPolling() {
  pollFallback = true;
  armPolling();
}

export function stopPolling() {
  pollFallback = false;
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
}

export function showGlobalError(msg) {
  const n = $('global-error');
  n.textContent = msg;
  n.hidden = false;
}
