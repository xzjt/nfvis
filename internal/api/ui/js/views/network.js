// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 网络域：网络对象总览 + 四类详情（vrf/acl/bond/qos/span）。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';
// 就地增删改（配置页去中心化第一刀）：面板复用配置页的节定义（单一真源，不在这里复制字段
// 清单），写路径走 core/forms.js 的同一套 candidate 事务（取锁 → 写候选 → 校验 → 提交 → 释放）。
import { CFG_SECTIONS } from './config.js';
import { objEditor, objTxn } from '../core/forms.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsnetworkRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsnetworkRouterModule()).render();
}

// ---------- 就地增删改：对象种类与面板（网络对象族）----------
//
// 每个种类的面板都画配置页那一份节（CFG_SECTIONS 里的一条）；节缺失时如实报错，不静默。
// cli* 只在确认框里做只读回显（工单对照用），关键字取自命令行同一族语句。
const NET_EDIT_KINDS = {
  vrfs: {
    list: 'vrfs', label: 'VRF',
    cliNew: (n) => 'set virtual-switches ' + n + ' type l3',
    cliSave: (n) => 'set virtual-switches ' + n + ' l3-interface … / static-routes …',
    cliDelete: (n) => 'delete virtual-switches ' + n + ' type',
  },
  acls: {
    list: 'acls', label: 'ACL',
    cliNew: (n) => 'set acls ' + n + ' rule <seq> …',
    cliSave: (n) => 'set acls ' + n + ' rule <seq> …',
    cliDelete: (n) => 'delete acls ' + n,
  },
  nat: {
    legend: 'NAT', label: 'NAT 配置',
    cliSave: () => 'set nat source-pool … / nat rules … / nat static …',
    cliDelete: () => 'delete nat',
  },
  bonds: {
    list: 'bonds', label: '链路聚合（bond）',
    cliNew: (n) => 'set bonds ' + n + ' members <成员口…>',
    cliSave: (n) => 'set bonds ' + n + ' members <成员口…> / lacp …',
    cliDelete: (n) => 'delete bonds ' + n,
  },
  qos: {
    list: 'qos_policies', label: 'QoS 策略',
    cliNew: (n) => 'set qos policies ' + n + ' cir <bps> cbs <bytes>',
    cliSave: (n) => 'set qos policies ' + n + ' cir <bps> cbs <bytes>',
    cliDelete: (n) => 'delete qos policies ' + n,
  },
  span: {
    list: 'port_mirroring', label: '端口镜像会话',
    cliNew: (n) => 'set port-mirroring ' + n + ' source … analyzer interface …',
    cliSave: (n) => 'set port-mirroring ' + n + ' source … analyzer interface …',
    cliDelete: (n) => 'delete port-mirroring ' + n,
  },
  vxlan: {
    list: 'vxlan_tunnels', label: 'VXLAN 隧道',
    cliNew: (n) => 'set vxlan tunnels ' + n + ' vni <n> local <ip> remote <ip>',
    cliSave: (n) => 'set vxlan tunnels ' + n + ' vni <n> local <ip> remote <ip>',
    cliDelete: (n) => 'delete vxlan tunnels ' + n,
  },
};

function netKind(key) {
  const k = NET_EDIT_KINDS[key];
  if (!k) return null;
  const all = Array.isArray(CFG_SECTIONS) ? CFG_SECTIONS : [];
  const section = all.find((s) => (k.list ? s.list === k.list : s.legend === k.legend)) || null;
  return Object.assign({}, k, { section });
}

// 网络对象总览页一块面板（所有种类共用：节定义不同，事务与确认口径完全相同）。
const netPageEditor = objEditor({ slot: 'net-obj-edit', reload });
// 各对象详情页一块面板（VRF / ACL / bond / QoS / SPAN）。
const vrdEditor = objEditor({ slot: 'vrd-edit', reload });
const acdEditor = objEditor({ slot: 'acd-edit', reload });
const bndEditor = objEditor({ slot: 'bnd-edit', reload });
const qsdEditor = objEditor({ slot: 'qsd-edit', reload });
const spdEditor = objEditor({ slot: 'spd-edit', reload });

function netDetailName(id) { const n = $(id); return (n && n.textContent) || ''; }
function netBind(btnId, fn) { const b = $(btnId); if (b) b.addEventListener('click', fn); }
if (vrdEditor) {
  netBind('vrd-edit-btn', () => vrdEditor.openEdit(netKind('vrfs'), netDetailName('vrd-name')));
  netBind('vrd-del-btn', () => vrdEditor.remove(netKind('vrfs'), netDetailName('vrd-name')));
}
if (acdEditor) {
  netBind('acd-edit-btn', () => acdEditor.openEdit(netKind('acls'), netDetailName('acd-name')));
  netBind('acd-del-btn', () => acdEditor.remove(netKind('acls'), netDetailName('acd-name')));
}
if (bndEditor) {
  netBind('bnd-edit-btn', () => bndEditor.openEdit(netKind('bonds'), netDetailName('bnd-name')));
  netBind('bnd-del-btn', () => bndEditor.remove(netKind('bonds'), netDetailName('bnd-name')));
}
if (qsdEditor) netBind('qsd-edit-btn', () => qsdEditor.openEdit(netKind('qos'), netDetailName('qsd-name')));
if (spdEditor) netBind('spd-edit-btn', () => spdEditor.openEdit(netKind('span'), netDetailName('spd-name')));

// ---------- 网络对象（只读总览）----------

// QoS 绑定读视图（决策 #331）：`接口:in|out` 逐条列出，与 CLI `show qos policies` 的
// 「绑定方向（接口:in|out）」列同源；无绑定时返回 undefined（表格里如实显示「—」）。
// 列表卡与详情页共用——两处各写一遍就会像 R110-1 那样漂移（曾取响应里不存在的字段）。
function qosBindingsText(q) {
  return Array.isArray(q.bindings) && q.bindings.length
    ? q.bindings.map((b) => b.interface + ':' + (b.direction === 'egress' ? 'out' : 'in')).join('；')
    : undefined;
}

// 每块：[标题, 数据, 列名, 取值函数, 详情页路由前缀（可选）, 就地编辑的种类键（可选）]。
// 有详情前缀时表格多一列"详情"，且整行可点进详情页；种类键让块右上角出现「+ 新建」（单对象节
// 是「编辑」），行内动作只给没有独立详情路由的 VXLAN（它的「编辑 / 删除」就在行里）。
// NAT 没有独立详情页（没有"单对象"语义：NAT 是配置对象），故只列在总览里，且是整节编辑；
// LLDP 也有自己的页面（#/network/lldp），不在这里重复列一遍。
const NET_OBJECT_VIEWS = [
  ['VRF（L3 虚拟交换机）', 'vrfs', ['名称', 'L3 接口', '路由数'], (v) => [
    v.name,
    (v.l3_interfaces || []).map((i) => i.interface).join(', '),
    // 列名是「路由数」：routes 是对象数组，直接塞进单元格会渲染成
    // "[object Object],[object Object]"（真机 round88 现场）。取长度，与详情页同一口径。
    v.routes != null ? v.routes.length : undefined,
  ], '#/network/vrfs/', 'vrfs'],
  ['ACL', 'acls', ['名称', '规则数'], (a) => [a.name, (a.rules || []).length], '#/network/acls/', 'acls'],
  // NAT 是对象（source_pools/rules/static），按池与规则各出一行
  ['NAT', 'nat', ['类型', '内容'], (n) => [n.kind, n.summary], undefined, 'nat'],
  ['链路聚合（bond）', 'bonds', ['名称', '模式', '成员'], (b) => [
    b.name, b.mode, (b.members || []).join(', '),
  ], '#/network/bonds/', 'bonds'],
  // 列与 CLI `show qos policies`（Policy / CIR(bps) / 绑定方向）同源；CBS 只在详情页展示。
  // 曾取 `q.type`/`q.target`/`q.interface` —— 响应里从来没有这些字段，「类型/目标」恒「—」
  // （R110-1，决策 #332），字段以契约 QosPolicy schema 为准。
  ['QoS 策略', 'qos', ['名称', 'CIR(bps)', '绑定（接口:in|out）'], (q) => [q.name, q.cir, qosBindingsText(q)], '#/network/qos/', 'qos'],
  ['端口镜像（SPAN）', 'span', ['名称', '源', '目的'], (s) => [
    s.name, spanSourceText(s.source), s.analyzer,
  ], '#/network/span/', 'span'],
  // VXLAN 隧道（决策 #383）：读 GET /vxlan-tunnels（配置声明 × 数据面实况，与 CLI
  // `show vxlan tunnels` 同源）。「状态」列取 `in_vpp`（数据面上有没有**该名字**的隧道口——
  // 按平台打在接口上的标记识别；底座没有可回读隧道参数的清单，VNI/地址/端口以配置为准）。
  // 运行态不可用时逐条如实标「运行态不可用」——此时 in_vpp 不可信，不把「说不清」当「未收敛」。
  ['VXLAN 隧道', 'vxlan', ['名称', 'VNI', '本地 → 远端', '端口', '交换机', '状态'], (t) => [
    t.name, t.vni, (t.local || '—') + ' → ' + (t.remote || '—'), t.dst_port, t.virtual_switch,
    t.runtime_available === false ? '运行态不可用'
      : (t.in_vpp ? '已在 VPP（接口 ' + dash(t.interface_name) + '）' : '未收敛'),
  ], undefined, 'vxlan'],
];

// natRows 把 NAT 配置对象摊平成行（池 / 规则 / 静态映射各一行）。
function natRows(nat) {
  if (!nat || nat.__err) return [];
  const rows = [];
  (nat.source_pools || []).forEach((p) => rows.push({ kind: '地址池', summary: (p.name || '') + ' ' + (p.address_range || '') }));
  (nat.rules || []).forEach((r) => rows.push({ kind: '规则 ' + (r.seq != null ? r.seq : ''), summary: (r.match_source || '') + ' → ' + (r.virtual_switch || '') }));
  (nat.static || nat.static_mappings || []).forEach((m) => rows.push({ kind: '静态映射', summary: (m.external || '') + ' → ' + (m.internal || '') }));
  return rows;
}

// vxlanRows：GET /vxlan-tunnels 的响应是 {tunnels, runtime_available, runtime_reason}
// 的对象（不是裸数组），卡片取其中的 tunnels；读取失败/形状不符时返回空表（如实显示「无」）。
// 运行态不可用时把 runtime_available=false 透传到每一行（渲染的「状态」列据此如实标注）。
function vxlanRows(vx) {
  if (!vx || vx.__err || !Array.isArray(vx.tunnels)) return [];
  const ok = vx.runtime_available !== false;
  return vx.tunnels.map((t) => Object.assign({}, t, { runtime_available: ok }));
}

function renderNetworkObjects(nets) {
  const box = $('net-objects');
  box.textContent = '';
  if (!nets) {
    box.appendChild(el('p', { class: 'muted', text: '（读取失败）' }));
    return;
  }
  NET_OBJECT_VIEWS.forEach(([title, key, cols, pick, routePrefix, editKey]) => {
    const data = nets[key];
    const kind = editKey ? netKind(editKey) : null;
    // 没有独立详情路由的对象（VXLAN）在行内就地编辑 / 删除；有详情页的块只给「+ 新建」。
    const rowEdit = !!(kind && !routePrefix);
    const actionCol = routePrefix ? '详情' : (rowEdit ? '操作' : '');
    const wrap = el('div', { class: 'net-object' });
    const head = el('h3', { text: title });
    if (kind) {
      // 多对象节给「+ 新建」；单对象节（NAT）整节只有一份配置，给「编辑」。
      const isSingle = !kind.list;
      const b = rowButton(wbtn({ type: 'button', class: 'small', text: isSingle ? '编辑' : '+ 新建' }));
      b.addEventListener('click', () => {
        if (!netPageEditor) return;
        if (isSingle) netPageEditor.openEdit(kind, '');
        else netPageEditor.openNew(kind);
      });
      head.appendChild(b);
    }
    wrap.appendChild(head);
    if (data && data.__err) {
      wrap.appendChild(el('p', { class: 'muted small', text: '读取失败：' + data.__err }));
      box.appendChild(wrap);
      return;
    }
    const rows = key === 'nat' ? natRows(data)
      : (key === 'vxlan' ? vxlanRows(data) : (Array.isArray(data) ? data : (data ? [data] : [])));
    const t = el('table');
    const thead = el('thead');
    const htr = el('tr');
    cols.forEach((c) => htr.appendChild(el('th', { text: c })));
    if (actionCol) htr.appendChild(el('th', { text: actionCol }));
    thead.appendChild(htr);
    t.appendChild(thead);
    const tbody = el('tbody');
    if (!rows.length) {
      const tr = el('tr');
      tr.appendChild(el('td', { colspan: String(cols.length + (actionCol ? 1 : 0)), class: 'muted', text: '（无）' }));
      tbody.appendChild(tr);
    } else {
      rows.forEach((r) => {
        const path = routePrefix ? routePrefix + encodeURIComponent(r.name) : '';
        const tr = path ? rowClickable(el('tr'), path) : el('tr');
        pick(r).forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
        const cell = el('td', { class: 'actions' });
        if (path) {
          const b = rowButton(el('button', { type: 'button', class: 'ghost small', text: '详情' }));
          b.addEventListener('click', () => goDetail(path));
          cell.appendChild(b);
        }
        if (rowEdit) {
          // 行内写入口必须经 wbtn（data-write）：read-only 角色由 CSS 隐藏（决策 #145/#327）。
          const eb = rowButton(wbtn({ type: 'button', class: 'small', text: '编辑' }));
          eb.addEventListener('click', () => { if (netPageEditor) netPageEditor.openEdit(kind, r.name); });
          const db = rowButton(wbtn({ type: 'button', class: 'small danger', text: '删除' }));
          db.addEventListener('click', () => { if (netPageEditor) netPageEditor.remove(kind, r.name); });
          cell.appendChild(eb);
          cell.appendChild(db);
        }
        if (actionCol) tr.appendChild(cell);
        tbody.appendChild(tr);
      });
    }
    t.appendChild(tbody);
    wrap.appendChild(t);
    box.appendChild(wrap);
  });
}

// ---------- 网络对象详情页（vrf / acl / bond / qos / span）----------
//
// 一页一对象，取代原来的共享浮层（浮层把 JSON 原样贴出来，既不好读、也不能分享地址）。
// QoS 与 SPAN 的详情**从列表端点取数**：契约里 `/qos/policies/{name}` 与 `/port-mirroring/{name}`
// 只有 DELETE（没有 GET），按单取路径请求只会得到 405——列表端点里本来就有完整的对象。

// 列表里按名字取对象（QoS / SPAN 的详情页用；列表端点缺省不截断，故能取全）。
export function pickByName(rows, name) {
  return rowsOf(rows).find((r) => r.name === name) || null;
}

// 列表里没找到时给一句能读懂的话（而不是把空对象渲染成一片「—」）。
export function notFoundText(name, label) {
  return '未找到' + label + ' ' + name + '（可能已被删除，或当前配置里没有它）';
}

// 路由表文本（prefix / next_hop / distance 三列对齐）：与 CLI 的 show vrfs <name> routes 同源。
function routesText(list) {
  const head = 'prefix'.padEnd(30) + ' next_hop'.padEnd(20) + ' distance';
  const lines = list.map((r) => String(dash(r.prefix)).padEnd(30) + ' ' +
    String(dash(r.next_hop)).padEnd(19) + ' ' + dash(r.distance));
  return head + '\n' + lines.join('\n');
}

// 把一份（预取或现拉的）路由表结果画到 pre 上：读取失败与"无路由"要分得开。
function vrfRoutesShow(res) {
  const pre = $('vrd-routes');
  pre.hidden = false;
  if (res && res.__err) { pre.textContent = '读取失败：' + res.__err; return; }
  const list = Array.isArray(res) ? res : [];
  pre.textContent = list.length ? routesText(list) : '（无路由）';
}

// 按需重拉路由表（大表不进轮询；进页面时用路由表预取的那一份先画出来）。
export async function vrfRoutesLoad(name) {
  const pre = $('vrd-routes');
  pre.hidden = false;
  pre.textContent = '读取中…';
  try {
    vrfRoutesShow(await api('/vrfs/' + encodeURIComponent(name) + '/routes'));
  } catch (e) {
    pre.textContent = '读取失败：' + e.message;
  }
}

function renderVrfDetail(vrf, routes, params) {
  const name = (params && params.name) || '';
  const ok = vrf && !vrf.__err;
  $('vrd-name').textContent = name;
  fill($('vrd-head'), ok ? [
    ['描述', vrf.description],
    ['L3 接口', (vrf.l3_interfaces || []).length],
    ['静态路由', (vrf.routes || []).length],
  ] : [['读取失败', vrf ? vrf.__err : notFoundText(name, 'VRF')]]);
  const l3 = ok ? (vrf.l3_interfaces || []) : [];
  table($('vrd-l3-table').querySelector('tbody'), 4, l3.map((i) => [
    i.interface, i.vlan, list(i.addresses), i.acl_in,
  ]));
  vrfRoutesShow(routes);
}

function renderAclDetail(acl, params) {
  const name = (params && params.name) || '';
  const ok = acl && !acl.__err;
  $('acd-name').textContent = name;
  fill($('acd-head'), ok ? [
    ['规则数', (acl.rules || []).length],
  ] : [['读取失败', acl ? acl.__err : notFoundText(name, 'ACL')]]);
  const rules = ok ? (acl.rules || []) : [];
  table($('acd-rule-table').querySelector('tbody'), 6, rules.map((r) => [
    r.seq, r.direction, r.source, r.destination, r.protocol, r.source_port,
  ]));
}

function renderBondDetail(bond, params) {
  const name = (params && params.name) || '';
  const ok = bond && !bond.__err;
  const st = (bond && bond.state) || {};
  $('bnd-name').textContent = name;
  fill($('bnd-head'), ok ? [
    ['成员', list(bond.members)],
    ['模式', bond.lacp ? 'LACP（' + (bond.lacp.mode || '') + '，' + (bond.lacp.interval || '') + '）' : '静态聚合'],
  ] : [['读取失败', bond ? bond.__err : notFoundText(name, '聚合口')]]);
  fill($('bnd-info'), ok ? [
    ['名称', bond.name],
    ['成员口', list(bond.members)],
    ['LACP 模式', bond.lacp ? bond.lacp.mode : undefined],
    ['LACP 速率', bond.lacp ? bond.lacp.interval : undefined],
    ['MTU', bond.mtu],
    ['描述', bond.description],
    ['链路状态', st.link],
    ['活动成员数', st.active_members],
  ] : []);
}

function renderQosDetail(rows, params) {
  const name = (params && params.name) || '';
  const q = pickByName(rows, name);
  const err = rows && rows.__err;
  $('qsd-name').textContent = name;
  fill($('qsd-head'), q ? [
    ['CIR', q.cir != null ? q.cir + ' bps' : undefined],
    ['CBS', q.cbs != null ? q.cbs + ' 字节' : undefined],
  ] : [['读取失败', err ? err : notFoundText(name, 'QoS 策略')]]);
  fill($('qsd-info'), q ? [
    ['名称', q.name],
    ['承诺速率（CIR）', q.cir != null ? q.cir + ' bps' : undefined],
    ['突发（CBS）', q.cbs != null ? q.cbs + ' 字节' : undefined],
    ['绑定接口', list(q.bound_interfaces)],
    // 决策 #331：逐条方向绑定（同一策略可被不同接口以不同方向引用）——与列表卡同一 helper。
    ['绑定（方向）', qosBindingsText(q)],
  ] : []);
}

// 删 QoS 策略（DELETE /qos/policies/{name}，该路径**只有 DELETE**——所以详情取自列表端点）。
//
// 写路径与其它对象同一套一次性事务（core/forms.js 的 objTxn）：**改候选**这一步走该对象自己的
// 端点（服务端内部就是「取配置编辑锁 + 把删除写进候选」，且带对象级检查——被接口绑定的策略由它
// 精确拒绝），随后统一 校验（POST /configuration/check）→ 提交（POST /configuration/commit）→
// 释放（DELETE /configuration/candidate）——与 CLI `delete qos policies <n>` + commit 同口径。
// 档位：中危（删对象一类）——逐条列影响面 + 主按钮标红。标题与只读回显语句是既有验收锚点
// （contrib/scripts/web-console-confirm-selftest.js 的动作表逐字钉住），保持原样。
function qsdMsg(text, isErr) {
  const p = $('qsd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

export async function qsdDelete() {
  const name = $('qsd-name').textContent;
  if (!name) { qsdMsg('没有选中策略。', true); return; }
  const ok = await uiConfirm('删除 QoS 策略', {
    tier: 'mid',
    bullets: [
      '从配置里删除限速策略 ' + name + '：写入即提交（取编辑锁 → 写候选 → 校验 → 立即提交生效），等价命令行的 delete + commit。',
      '被接口绑定的策略由服务端拒绝删除（要先把接口上的引用解掉），界面会把它的报错原文显示出来。',
      '生效后绑过它的接口不再限速，相关流量行为可能随之变化。',
      '可回退：「提交历史」里能把上一版取为候选、再提交回来。',
    ],
    cli: 'delete qos policy name ' + name,
  });
  if (!ok) return;
  qsdMsg('提交中…', false);
  const res = await objTxn(() => api('/qos/policies/' + encodeURIComponent(name), { method: 'DELETE' }));
  qsdMsg(res.text, !res.ok);
  if (res.ok) await reload().catch(() => {});
}

// SPAN 源的统一文本（决策 #430②）：vNIC 源在契约里是 `source.vnf` + `source.vnf_interface`
// 两个字段（此时 `source.interface` 为空）——标题行只显示 vNIC 名会让操作者看不出是哪台 VM。
// 合并成 `vnf <vm>/<vnic>` 一行后 VM 与 vNIC 都一眼可见（与交换机成员端口的 `vnf/<vnic>`
// 写法同源）；物理口源仍显示口名。取不到源回 undefined ⇒ 该行如实显示「—」。
function spanSourceText(src) {
  if (!src) return undefined;
  if (src.vnf) return 'vnf ' + src.vnf + '/' + (src.vnf_interface || '—');
  return src.interface;
}

function renderSpanDetail(rows, params) {
  const name = (params && params.name) || '';
  const s = pickByName(rows, name);
  const err = rows && rows.__err;
  const src = (s && (s.source || {})) || {};
  $('spd-name').textContent = name;
  fill($('spd-head'), s ? [
    ['源', spanSourceText(src)],
    ['方向', src.direction],
    ['分析口', s.analyzer],
  ] : [['读取失败', err ? err : notFoundText(name, 'SPAN 会话')]]);
  fill($('spd-info'), s ? [
    ['名称', s.name],
    ['源接口', src.interface],
    ['源 VNF', src.vnf],
    ['源 vNIC', src.vnf_interface],
    ['方向', src.direction],
    ['分析端口', s.analyzer],
  ] : []);
}

// 删端口镜像会话（DELETE /port-mirroring/{name}，同样只有 DELETE）。语义与 QoS 删除一致：
// 改候选走该对象自己的端点（服务端内部「取锁 + 写候选」），随后统一 校验 → 提交 → 释放，
// 等价 CLI `delete port-mirroring <n>` + commit。标题与只读回显语句是既有验收锚点，保持原样。
function spdMsg(text, isErr) {
  const p = $('spd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

export async function spdDelete() {
  const name = $('spd-name').textContent;
  if (!name) { spdMsg('没有选中镜像会话。', true); return; }
  const ok = await uiConfirm('删除端口镜像会话', {
    tier: 'mid',
    bullets: [
      '从配置里删除镜像会话 ' + name + '：写入即提交（取编辑锁 → 写候选 → 校验 → 立即提交生效），等价命令行的 delete + commit。',
      '生效后该会话的镜像流量停止（分析口不再收到复制流量）。',
      '被别处引用的会话删不掉时服务端会拒绝并指出原因，报错原文照实显示。',
      '可回退：「提交历史」里能把上一版取为候选、再提交回来。',
    ],
    cli: 'delete port-mirroring name ' + name,
  });
  if (!ok) return;
  spdMsg('提交中…', false);
  const res = await objTxn(() => api('/port-mirroring/' + encodeURIComponent(name), { method: 'DELETE' }));
  spdMsg(res.text, !res.ok);
  if (res.ok) await reload().catch(() => {});
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsNetwork = {
  'network': {
    async render(d) { pageWarn(d); renderNetworkObjects(await loadNetworkObjects(d)); },
    // 离开本页（router 的 leave 钩子）：收掉就地编辑面板——草稿不跨页残留。
    leave() { if (netPageEditor) netPageEditor.leave(); }
  },
  'vrfDetail': {
    render(d, params) { pageWarn(d); renderVrfDetail(d['/vrfs/{name}'], d['/vrfs/{name}/routes'], params); },
    leave() { if (vrdEditor) vrdEditor.leave(); }
  },
  'aclDetail': {
    render(d, params) { pageWarn(d); renderAclDetail(d['/acls/{name}'], params); },
    leave() { if (acdEditor) acdEditor.leave(); }
  },
  'bondDetail': {
    render(d, params) { pageWarn(d); renderBondDetail(d['/bonds/{name}'], params); },
    leave() { if (bndEditor) bndEditor.leave(); }
  },
  'qosDetail': {
    render(d, params) { pageWarn(d); renderQosDetail(d['/qos/policies'], params); },
    leave() { if (qsdEditor) qsdEditor.leave(); }
  },
  'spanDetail': {
    render(d, params) { pageWarn(d); renderSpanDetail(d['/port-mirroring'], params); },
    leave() { if (spdEditor) spdEditor.leave(); }
  },
};
