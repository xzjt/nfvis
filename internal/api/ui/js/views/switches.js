// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 虚拟交换机域：列表 / 详情 / MAC 表 / DHCP 租约。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { notFoundText } from './network.js';
// 就地增删改（配置页去中心化第一刀）：面板复用配置页的「虚拟交换机」节定义（单一真源），
// 写路径走 core/forms.js 的同一套 candidate 事务（取锁 → 写候选 → 校验 → 提交 → 释放）。
import { CFG_SECTIONS } from './config.js';
import { objEditor } from '../core/forms.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsswitchesRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsswitchesRouterModule()).render();
}

// ---------- 就地新建 / 编辑 / 删除（虚拟交换机）----------
//
// 种类描述：节定义取自配置页的 CFG_SECTIONS（**同一份**节，不在这里复制字段清单——字段
// 新增/变更自动生效；节缺失时如实报错，不静默）。cli* 只在确认框里做只读回显（工单对照用）。
function vsKind() {
  const all = Array.isArray(CFG_SECTIONS) ? CFG_SECTIONS : [];
  return {
    label: '虚拟交换机',
    section: all.find((s) => s.list === 'virtual_switches') || null,
    cliNew: (n) => 'set virtual-switches ' + n + ' type l2',
    cliSave: (n) => 'set virtual-switches ' + n + ' …（类型 / 网关 / 成员端口 / DHCP …）',
    cliDelete: (n) => 'delete virtual-switches ' + n,
  };
}

// 列表页一块面板（「+ 新建」）；详情页一块面板（「编辑 / 删除」）。
const vsListEditor = objEditor({ slot: 'vs-list-edit', reload });
const vsdEditor = objEditor({ slot: 'vsd-edit', reload });
const vsdCurrentName = () => { const n = $('vsd-name'); return (n && n.textContent) || ''; };
if (vsListEditor) {
  const b = $('vs-new-btn');
  if (b) b.addEventListener('click', () => vsListEditor.openNew(vsKind()));
}
if (vsdEditor) {
  const eb = $('vsd-edit-btn');
  if (eb) eb.addEventListener('click', () => vsdEditor.openEdit(vsKind(), vsdCurrentName()));
  const db = $('vsd-del-btn');
  if (db) db.addEventListener('click', () => vsdEditor.remove(vsKind(), vsdCurrentName()));
}

// ---------- 虚拟交换机（列表 + 详情）----------

// 列表：点行（或「详情」按钮）进该交换机的详情页——成员口运行态与 MAC 学习表都在详情页，
// 列表本身不再贴一屏文本。
function renderVSwitches(vss, rows) {
  const tbody = $('vs-table').querySelector('tbody');
  tbody.textContent = '';
  const list = rows || [];
  if (!list.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '5', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
  } else {
    list.forEach((r) => {
      const path = '#/network/switches/' + encodeURIComponent(r.cfg.name);
      const tr = rowClickable(el('tr'), path);
      [r.cfg.name, r.cfg.type,
        (r.cfg.ports || []).map((p) => p.interface || p.vnf || p.container || '?').join(', '),
        // 内核数据面下没有 BD-ID 概念：服务端省略该字段（round40 R40-O1），此处如实显示「—」，
        // 不把缺字段误画成 0。
        r.stat ? r.stat.bd_id : undefined].forEach((c) => {
        tr.appendChild(el('td', { text: String(dash(c)) }));
      });
      const cell = el('td', { class: 'actions' });
      const btn = rowButton(el('button', { type: 'button', class: 'ghost small', text: '详情' }));
      btn.addEventListener('click', () => goDetail(path));
      cell.appendChild(btn);
      tr.appendChild(cell);
      tbody.appendChild(tr);
    });
  }
  $('vs-note').textContent = (vss && vss.length) ? '（' + vss.length + ' 个；点行进详情）' : '（未配置虚拟交换机）';

  const pre = $('vs-stat');
  const lines = [];
  list.forEach((r) => {
    (r.stat && r.stat.ports ? r.stat.ports : []).forEach((p) => {
      lines.push(r.cfg.name + ' / ' + p.port + ': ' +
        (p.admin === false ? 'down' : 'up') + '/' + (p.link === false ? 'down' : 'up') +
        '  rx ' + dash(p.rx_packets) + ' / tx ' + dash(p.tx_packets) + ' 包');
    });
  });
  if (!lines.length) {
    pre.hidden = true;
    return;
  }
  pre.hidden = false;
  pre.textContent = '成员口计数（运行态）：\n' + lines.join('\n');
}

// 详情页（#/network/switches/:name）：对象头 + 成员口运行态 + MAC 学习表（按需拉取）。
// 数据都来自路由表声明的 /virtual-switches/{name}（配置对象 + statistics）；**MAC 表不在声明里**——
// 它可能很大，只在点「拉取 MAC 表」时才请求，不随页面刷新反复下载。
let vsdMacFor = '';
export function getVsdMacFor() { return vsdMacFor; }
export function clearVsdMacFor() { vsdMacFor = ''; }   // 当前 MAC 表属于哪台交换机（换对象必须清掉，免得把 A 的表挂在 B 名下）
const VSD_MAC_LIMIT = 200;

function vsdMsg(text, isErr) {
  const p = $('vsd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

export function vsdMacClear(text, isErr) {
  table($('vsd-mac-table').querySelector('tbody'), 3, []);
  $('vsd-mac-note').textContent = '';
  vsdMsg(text || '', isErr === true);
}

// 端口来源的中文标注（决策 #326）：与服务端 source 字段一一对应。
const VSD_SOURCE_TEXT = { config: '配置', vnf: 'VNF 声明', container: '容器声明', runtime: '运行态' };

function renderSwitchDetail(vs, ports, params) {
  const name = (params && params.name) || '';
  const ok = vs && !vs.__err;
  const st = (vs && vs.statistics) || null;
  // 成员端口取**该路径自己的 GET**（决策 #326：配置静态 ports ∪ VNF/容器声明派生，逐条带 source）；
  // 取不到时退回对象里带的 ports（对象详情同样是配置视图，两处同源，前者是独立读法）。
  const cfgPorts = (ports && !ports.__err && Array.isArray(ports)) ? ports
    : ((vs && Array.isArray(vs.ports)) ? vs.ports : []);
  const rt = (st && Array.isArray(st.ports)) ? st.ports : [];
  const rtByName = {};
  rt.forEach((p) => { if (p && p.port != null) rtByName[p.port] = p; });
  // 端口读视图：配置派生在前（端口名已由服务端算好，port 字段），运行态里**不与之同名**的补一条
  // source=runtime（保留「VPP 里存在但配置没写」的可见性，与 CLI `show virtual-switches <n> ports` 同口径）。
  const rows = [], seen = {};
  cfgPorts.forEach((p) => {
    const label = p.port || p.interface || (p.vnf ? p.vnf + '/' + (p.vnf_interface || '') : '') ||
      (p.container ? p.container + '/' + (p.container_interface || '') : '') || '—';
    seen[label] = true;
    // 决策 #444：/ports 行内运行态列优先（与 CLI 同源）；statistics 条目仅作补条目/回退。
    const hasRowRT = p.admin !== undefined || p.link !== undefined ||
      p.rx_packets !== undefined || p.tx_packets !== undefined;
    rows.push({ label, source: p.source, rt: hasRowRT ? p : rtByName[label] });
  });
  rt.forEach((p) => { if (p && p.port != null && !seen[p.port]) rows.push({ label: p.port, source: 'runtime', rt: p }); });
  $('vsd-name').textContent = name;
  const vsdHead = [
    ['类型', vs.type],
    ['成员端口', rows.length],
    ['数据面 BD', st ? st.bd_id : undefined],
    // 决策 #335：DHCP 中继（读视图 dhcp_relay.server；未配置如实「—」，不编造）
    ['DHCP 中继', vs.dhcp_relay && vs.dhcp_relay.server ? vs.dhcp_relay.server : undefined],
    // 决策 #337：MAC 学习上限（读视图 learn_limit；未配置如实「—」，不编造）
    ['学习上限', vs.learn_limit !== undefined && vs.learn_limit !== 0 ? vs.learn_limit : undefined],
  ];
  // 决策 #435：内核数据面下 learn-limit 为阈值告警、非强制上限（读视图 learn_limit_note；
  // VPP 下无该字段——VPP 是硬性上限）。仅在服务端给出该字段时渲染一行，不编造。
  if (vs.learn_limit_note) vsdHead.push(['学习上限说明', vs.learn_limit_note]);
  // 决策 #437：内核数据面下 DHCP 中继的运行态说明（读视图 dhcp_relay_note——机制 + 客户端
  // 寻址依据 + 运行态/计数；VPP 下无该字段）。仅在服务端给出该字段时渲染一行，不编造。
  if (vs.dhcp_relay_note) vsdHead.push(['DHCP 中继说明', vs.dhcp_relay_note]);
  fill($('vsd-head'), ok ? vsdHead : [['读取失败', vs ? vs.__err : notFoundText(name, '虚拟交换机')]]);
  table($('vsd-port-table').querySelector('tbody'), 6, rows.map((r) => {
    const q = r.rt || {};
    return [
      r.label, VSD_SOURCE_TEXT[r.source] || r.source,
      q.admin === false ? 'down' : (q.admin === true ? 'up' : undefined),
      q.link === false ? 'down' : (q.link === true ? 'up' : undefined),
      q.rx_packets, q.tx_packets,
    ];
  }));
  $('vsd-stat-note').textContent = !ok ? ''
    : (st ? '' : '（无运行态：该交换机当前不在数据面，或数据面未连接）');
  // DHCP 服务器块：摘要随对象渲染；租约表换对象即清（同 MAC 表的防串台），配置了才自动拉一次。
  const hasDhcp = ok && vsdDhcpRender(vs);
  if (vsdDhcpFor !== name || !hasDhcp) {
    vsdDhcpFor = '';
    vsdDhcpClear('');
  }
  if (hasDhcp) { vsdDhcpLoad(); }
  // 换了对象：上一台的 MAC 表必须清掉（否则显示的是一台交换机的表、标题却是另一台）。
  if (vsdMacFor !== name) {
    vsdMacFor = '';
    vsdMacClear('');
  }
}

// 按需拉取 MAC 表：limit 由操作者给（默认 200 条），服务端按上限截断。
export async function vsdMacLoad() {
  const name = $('vsd-name').textContent;
  if (!name) { vsdMsg('没有选中虚拟交换机。', true); return; }
  const limit = Math.max(1, Math.floor(Number($('vsd-mac-limit').value) || VSD_MAC_LIMIT));
  vsdMsg('读取 MAC 表…', false);
  try {
    const rows = rowsOf(await api('/virtual-switches/' + encodeURIComponent(name) +
      '/mac-table?limit=' + limit));
    vsdMacFor = name;
    table($('vsd-mac-table').querySelector('tbody'), 3, rows.map((r) => [r.mac, r.port, r.vlan]));
    $('vsd-mac-note').textContent = '（' + rows.length + ' 条' +
      (rows.length >= limit ? '，已达上限——可能还有更多，把条数调大再拉' : '') + '）';
    vsdMsg('', false);
  } catch (e) {
    vsdMacFor = '';
    vsdMacClear('读取失败：' + e.message, true);
  }
}

// DHCP 服务器块与租约表：摘要来自 /virtual-switches/{name} 的 dhcp_server 读对象
// （未配置时服务端整个缺席，页面如实「—」）；租约表按需拉取（运行态，未配置时
// 服务端回 409 + 文案，页面如实显示未配置而非空表）。
let vsdDhcpFor = '';
export function getVsdDhcpFor() { return vsdDhcpFor; }
export function clearVsdDhcpFor() { vsdDhcpFor = ''; }

function vsdDhcpMsg(text, isErr) {
  const p = $('vsd-dhcp-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

export function vsdDhcpClear(text, isErr) {
  table($('vsd-dhcp-lease-table').querySelector('tbody'), 4, []);
  $('vsd-dhcp-note').textContent = '';
  vsdDhcpMsg(text || '', isErr === true);
}

// vsdDhcpRender 渲染「DHCP 服务器」摘要块；返回是否配置了 dhcp-server。
function vsdDhcpRender(vs) {
  const d = vs && vs.dhcp_server;
  fill($('vsd-dhcp'), d ? [
    ['租约池', (d.pool_start || '—') + ' ~ ' + (d.pool_end || '—')],
    ['租约时长', d.lease_time_seconds != null ? d.lease_time_seconds + ' 秒' : undefined],
    ['DNS', d.dns || undefined],
    ['域名', d.domain_name || undefined],
    ['在租数', d.active_leases != null ? d.active_leases : undefined],
  ] : []);
  return !!d;
}

// 按需拉取租约表：未配置（409）与尚未收敛（503）都如实转述服务端文案，不显示成空表。
export async function vsdDhcpLoad() {
  const name = $('vsd-name').textContent;
  if (!name) { vsdDhcpMsg('没有选中虚拟交换机。', true); return; }
  vsdDhcpMsg('读取租约表…', false);
  try {
    const rows = rowsOf(await api('/virtual-switches/' + encodeURIComponent(name) + '/dhcp-leases'));
    vsdDhcpFor = name;
    table($('vsd-dhcp-lease-table').querySelector('tbody'), 4, rows.map((r) => [
      r.ip, r.mac, r.state, r.expires_in_seconds,
    ]));
    $('vsd-dhcp-note').textContent = '（' + rows.length + ' 条）';
    vsdDhcpMsg('', false);
  } catch (e) {
    vsdDhcpFor = '';
    vsdDhcpClear(e.message, true);
  }
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsSwitches = {
  'switches': {
    async render(d) {
      pageWarn(d);
      const vss = rowsOf(d['/virtual-switches']);
      renderVSwitches(vss, await loadVSwitchStats(vss));
    },
    // 离开本页（router 的 leave 钩子）：收掉就地编辑面板——草稿不跨页残留。
    leave() { if (vsListEditor) vsListEditor.leave(); }
  },
  'switchDetail': {
    // 成员端口取 `/virtual-switches/{name}/ports`（该路径的 GET 就是**配置里的成员端口列表**），
    // 运行态计数仍取对象详情里的 statistics——两个端点各管一段事实，不混着猜。
    render(d, params) {
      pageWarn(d);
      renderSwitchDetail(d['/virtual-switches/{name}'], d['/virtual-switches/{name}/ports'], params);
    },
    leave() { if (vsdEditor) vsdEditor.leave(); }
  },
};
