// NFViS 控制台共享件 · 取数（从 app.js 拆出，逻辑逐字保留；Web 控制台重构 2026-10）。
//
// 本模块只管「怎么拿数据、失败怎么如实降级」；路由声明（拉哪些端点）在 routes.json，
// 页面渲染在各 views 模块。token 经 getToken() 访问器取（模块内可变状态不直导）。

import { getToken } from './state.js';
import { API, MAX_IFACE_DETAIL } from './const.js';

export { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from './const.js';

async function signOutViaRouter(msg) {
  // signOut 在 boot.js（登录域）：按需动态 import，避免静态互引成环。
  const boot = await import('../boot.js');
  return boot.signOut(msg);
}

export async function api(path, opts) {
  const o = opts || {};
  o.headers = Object.assign({ Authorization: 'Bearer ' + getToken() }, o.headers || {});
  const res = await fetch(API + path, o);
  if (res.status === 401) {
    // token 失效（过期/被吊销/服务端重启）：回到登录视图，别让页面停在半截数据上。
    await signOutViaRouter('登录状态已失效，请重新登录。');
    throw new Error('未认证');
  }
  if (!res.ok) {
    let msg = 'HTTP ' + res.status;
    try {
      const body = await res.json();
      if (body && body.message) msg = body.message;
      // 校验类失败会带 `detail[]`（逐条说明"哪里不对、该怎么办"）——那正是操作者最需要的一行，
      // 不能只把 summary（如「commit 校验失败」）摆出来：含糊的失败和谎报成功一样不可接受。
      if (body && Array.isArray(body.detail) && body.detail.length) {
        const lines = body.detail
          .map((d) => (d && d.message ? ((d.path ? d.path + '：' : '') + d.message) : ''))
          .filter(Boolean);
        if (lines.length) msg += '——' + lines.join('；');
      }
    } catch (e) { /* 非 JSON 错误体：保留状态码 */ }
    throw new Error(msg);
  }
  return res.status === 204 ? null : res.json();
}

// 单个端点失败不该拖垮整页：各自降级为"读取失败"。
export const soft = (p) => p.catch((e) => ({ __err: e.message }));

// apiErrText：把"服务端没有这个接口"（Go 的路由表对未注册路径直接 404，错误体是纯文本，
// 界面只能看到 HTTP 404）说成可操作的说明。**不改变判定**——404 仍然是失败，只是把
// "为什么失败、接下来怎么办"讲清楚（含糊的"失败"和谎报成功一样不可接受）。
export function apiErrText(e, hint) {
  const m = (e && e.message) || '未知错误';
  return /^HTTP 404$/.test(m) ? m + '（本机服务端没有提供这个接口；' + hint + '）' : m;
}

// soft() 失败时给的是 {__err} 而不是数组——数组类渲染一律先过 rowsOf()，否则 .filter/.map 抛错
// （浏览器验收抓到的旧缺陷：token 失效时整页报 JS 错，而不是干净地提示）。
export const rowsOf = (v) => (Array.isArray(v) ? v : []);

// httpErrText：**直连 fetch**（下载/上传/日志这类不走 api() 的路径）失败时的文案。
// 与 api() 同口径——错误体里的原话优先，取不到才回落到状态码：服务端讲了原因
// （如「当前 class 无权执行该操作」），就要让操作者看到，而不是只剩一个 HTTP 403。
export async function httpErrText(res) {
  let msg = 'HTTP ' + res.status;
  try { const b = await res.json(); if (b && b.message) msg = b.message; } catch (e) { /* 非 JSON 错误体：保留状态码 */ }
  return msg;
}

// 同上，但给**已经把响应体读成文本**的调用方用（文本类端点：差异 / 日志）。
export function errTextOf(text, status) {
  try { const b = JSON.parse(text); if (b && b.message) return b.message; } catch (e) { /* 非 JSON 错误体：保留状态码 */ }
  return 'HTTP ' + status;
}

// 页面级取数失败提示：把失败的端点列出来（不静默），全部成功时清掉提示。
// 只清自己写的那条——路由的「页面不存在」提示要留着（否则一重渲染就被抹掉）。
let pageWarnActive = false;
export function pageWarn(d) {
  const bad = Object.entries(d || {})
    .filter(([, v]) => v && v.__err)
    .map(([k, v]) => k + '（' + v.__err + '）');
  const box = document.getElementById('global-error');
  if (bad.length) {
    box.textContent = '以下数据读取失败：' + bad.join('、');
    box.hidden = false;
    pageWarnActive = true;
    return;
  }
  if (pageWarnActive) { box.hidden = true; box.textContent = ''; pageWarnActive = false; }
}

// 按路由声明的端点取数：返回 { 端点: 数据 }（各自降级，单个失败不拖垮整页）。
// `params` 是参数化路由取出的对象名（如 `:name`）：端点里的 `{name}` 占位在**取数时**展开，
// 返回值的键仍是路由表里声明的那一串（`'/virtual-machine-functions/{name}'`）——
// 视图照路由表原文取值，不必自己拼路径，也不会因为对象名不同而取错键。
export async function softLoad(paths, params) {
  const out = {};
  await Promise.all((paths || []).map(async (p) => {
    out[p] = await soft(api(expandEndpoint(p, params)));
  }));
  return out;
}

// 端点占位展开：`{name}` → 参数值（URL 编码）。参数缺席时**保留占位原样**（请求会得到
// 404 而不是打到某个碰巧同名的对象上），页面上会如实显示这条读取失败。
function expandEndpoint(path, params) {
  const p = params || {};
  return path.replace(/\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (m, k) =>
    (p[k] === undefined || p[k] === null ? m : encodeURIComponent(p[k])));
}

// 虚拟交换机：列表取自运行态（/virtual-switches 是配置视图，统计在详情上）。
export async function loadVSwitchStats(vss) {
  const head = vss.slice(0, MAX_IFACE_DETAIL);
  const details = await Promise.all(head.map((v) => api('/virtual-switches/' + encodeURIComponent(v.name)).catch(() => null)));
  return head.map((v, n) => ({ cfg: v, stat: (details[n] || {}).statistics || null }));
}

// 网络对象：一次拉齐只读视图（每个端点各自降级，缺一个不影响其余）。
// `pre` 是路由表声明端点的预取结果（softLoad 已取齐）：命中就直接归位，不重复请求；
// 未预取时（别处直接调用）逐条自取。
// LLDP 不在这一页：它的开关状态与邻居表有自己的页面（#/network/lldp）——本页只取路由表声明的端点。
export async function loadNetworkObjects(pre) {
  const pick = (path, fetchIt) => (pre && pre[path] !== undefined ? pre[path] : fetchIt());
  const [vrfs, acls, nat, bonds, qos, span, vxlan] = await Promise.all([
    pick('/vrfs', () => soft(api('/vrfs'))),
    pick('/acls', () => soft(api('/acls'))),
    pick('/nat', () => soft(api('/nat'))),
    pick('/bonds', () => soft(api('/bonds'))),
    pick('/qos/policies', () => soft(api('/qos/policies'))),
    pick('/port-mirroring', () => soft(api('/port-mirroring'))),
    pick('/vxlan-tunnels', () => soft(api('/vxlan-tunnels'))),
  ]);
  return { vrfs, acls, nat, bonds, qos, span, vxlan };
}

// 逐口取统计（列表端点只有配置字段；收发包数在 /interfaces/<name> 上）。
export async function loadInterfaceStats(ifaces) {
  const head = ifaces.slice(0, MAX_IFACE_DETAIL);
  const details = await Promise.all(head.map((i) => api('/interfaces/' + encodeURIComponent(i.name)).catch(() => null)));
  return head.map((i, n) => ({ cfg: i, stat: (details[n] || {}).statistics || null }));
}
