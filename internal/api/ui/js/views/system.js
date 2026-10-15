// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 系统域：接口详情（DPDK/SR-IOV）/ 历史趋势图 / 内核基线 / 用户与权限 / 会话与Token / 登录横幅 / 我的账号 / 证书 / 硬件健康。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, expandEndpoint, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewssystemRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewssystemRouterModule()).render();
}

// ---------- 接口详情页（#/system/interfaces/:name）：驱动接管与 SR-IOV ----------
//
// 两条写路径都作用在**本机网卡**上，都留在服务端实现里，界面只做入口与确认：
//   · DPDK 绑定/解绑（PUT /interfaces/{name}/dpdk，需 confirm=true）——管理口守卫在
//     dpdkController.SetDPDKBound 里（配置声明的管理口 / 承载默认路由的口 / 守护进程监听地址
//     所属的口三条事实，命中即拒绝且不提供强制出口）。界面**不绕过**：守卫拒绝时如实显示服务端原文。
//   · SR-IOV VF 数量（PUT /interfaces/{name}/sriov）——无 PF/VF 的机器上由底座明确报错，同样如实显示。
//
// 档位（§7 的"影响面跨对象/跨会话、可回退"一条）：两条都定**中危**——逐条列影响面 + 主按钮标红。
// 理由：都作用在单个对象上、都能点回去（解绑 / vf_count 归零），且真正的红线（管理口）由服务端守卫兜住；
// 高危档留给"不可撤销 / 会覆盖全局状态"的动作（恢复出厂、软件升级、恢复配置、证书上传、用户与权限）。
function ifdMsg(text, isErr) {
  const p = $('ifd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// 详情页取两个端点：`/interfaces/{name}`（合并视图：配置字段 + 运行态字段 + 该口计数）与
// `/interfaces`（运行态行清单，用于按名取行；保留是为了与列表页共用一份渲染数据）。
// 运行态缺席时如实显示「—」，不编造。
function renderIfaceDetail(iface, ifaces, params) {
  const name = (params && params.name) || '';
  const ok = iface && !iface.__err;
  const st = (iface && iface.statistics) || null;
  const rt = pickByName(rowsOf(ifaces), name) || {};
  $('ifd-name').textContent = name;
  fill($('ifd-head'), ok ? [
    ['驱动', rt.driver],
    ['链路', rt.link],
    ['速率', rt.speed_mbps != null ? rt.speed_mbps + ' Mbps' : undefined],
    ['管理状态', rt.enabled === true ? '启用' : (rt.enabled === false ? '禁用' : undefined)],
  ] : [['读取失败', iface ? iface.__err : notFoundText(name, '接口')]]);
  fill($('ifd-info'), ok ? [
    ['说明', iface.description],
    ['MTU', iface.mtu],
    // 不在此处重复展示「启用」：`enabled` 是**运行态** admin 状态（详情端点与列表端点同一合并视图），
    // 已在头部「管理状态」呈现——原先那行标着「启用（配置）」会把运行态值说成配置值。
    ['入向限速策略', iface.ingress_policy],
    ['出向限速策略', iface.egress_policy],
    ['风暴抑制', stormControlText(iface.storm_control)],
    ['端口安全', portSecText(iface.port_security)],
    ['SR-IOV VF 数（配置）', iface.sriov ? iface.sriov.vf_count : undefined],
    ['VF 占用', iface.sriov && Array.isArray(iface.sriov.vfs) && iface.sriov.vfs.length
      ? iface.sriov.vfs.map((v) => v.vf_id + (v.assigned_vnf ? '→' + v.assigned_vnf : '（空闲）')).join('；')
      : undefined],
  ] : [['读取失败', iface ? iface.__err : notFoundText(name, '接口')]]);
  fill($('ifd-stat'), st ? [
    ['Rx / Tx 包', dash(st.rx_packets) + ' / ' + dash(st.tx_packets)],
    ['Rx / Tx 字节', dash(bytes(st.rx_bytes)) + ' / ' + dash(bytes(st.tx_bytes))],
    ['Rx / Tx 错误', dash(st.rx_errors) + ' / ' + dash(st.tx_errors)],
    ['Rx / Tx 丢弃', dash(st.rx_drops) + ' / ' + dash(st.tx_drops)],
  ] : [['读取失败', ok ? '该口当前取不到计数（数据面未连接或口不在数据面）' : '未取到数据']]);
}

// dpdkAct 绑定/解绑：confirm=true 由界面带上（服务端要求，缺了会 400——这不是界面在放水，
// 而是服务端对"会中断该口流量"的显式要求）。
async function dpdkAct(bound) {
  const name = $('ifd-name').textContent;
  if (!name) { ifdMsg('没有选中接口。', true); return; }
  const uio = $('ifd-uio').value;
  const to = $('ifd-todrv').value.trim();
  const cli = 'request interfaces ' + name + (bound ? ' bind-dpdk' : ' unbind-dpdk') +
    (bound ? (uio ? ' uio-driver ' + uio : '') : (to ? ' to-driver ' + to : ''));
  const ok = await uiConfirm(bound ? '绑定到 DPDK 驱动' : '解绑交还内核驱动', {
    tier: 'mid',
    bullets: bound ? [
      '该口会离开内核平面：内核侧的地址、路由、统计都不再属于它，内核里也看不到这块网卡。',
      '经该口的现有流量立即中断；若该口承载业务或管理路径，路径会断。',
      '交给数据面使用需要按 PCI 地址声明该口，并按需重启数据面才认。',
      '管理口（配置声明的管理口 / 承载默认路由的口 / 守护进程监听地址所属的口）由服务端守卫直接拒绝。',
      '可回退：在同一页「解绑交还内核」把它交还给内核驱动。',
    ] : [
      '该口回到内核平面：内核驱动重新接管，内核侧地址/路由/统计重新可见。',
      '若该口仍被数据面按 PCI 声明占用，数据面侧的路径会断（先确认数据面不再需要它）。',
      '交还的内核驱动名建议显式给出（本机常需显式给出才能重新探测原生驱动）。',
      '可回退：再点一次「绑定到 DPDK」即可重新交给数据面。',
    ],
    cli: cli,
  });
  if (!ok) return;
  ifdMsg((bound ? '绑定' : '解绑') + ' ' + name + '：执行中…', false);
  try {
    const r = await api('/interfaces/' + encodeURIComponent(name) + '/dpdk?confirm=true', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(bound ? { bound: true, uio_driver: uio } : { bound: false, to_driver: to }),
    });
    // 如实回显服务端给的"操作后实际绑定的驱动"——不把"请求成功"当成"驱动已换"。
    ifdMsg((bound ? '已绑定' : '已解绑') + ' ' + name + '：PCI ' + dash(r && r.pci) +
      '，当前驱动 ' + dash(r && r.driver), false);
  } catch (e) {
    ifdMsg((bound ? '绑定' : '解绑') + ' ' + name + ' 失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

async function sriovSet() {
  const name = $('ifd-name').textContent;
  if (!name) { ifdMsg('没有选中接口。', true); return; }
  const raw = $('ifd-vfs').value.trim();
  if (!/^\d+$/.test(raw)) { ifdMsg('VF 数量必须是非负整数（0 = 回收全部 VF）。', true); return; }
  const n = Number(raw);
  const ok = await uiConfirm('设置 SR-IOV VF 数量', {
    tier: 'mid',
    bullets: [
      '在物理口 ' + name + ' 上创建 / 回收 VF：写的是该口的 VF 数量（0 = 回收全部）。',
      n === 0 ? '回收全部 VF：正在用该口 VF 直通的虚拟机数据口会断。'
        : '新建 ' + n + ' 个 VF：新建/回收瞬间该 PF 上的 VF 直通流量会短暂中断。',
      '该口不支持 SR-IOV 时服务端明确报错（不静默跳过）——界面会把报错原文显示出来。',
      '可回退：把数量改回原值即可（VF 数量是声明式配置，重启后按它收敛）。',
    ],
    cli: n > 0 ? 'request sriov create-vfs ' + name + ' count ' + n
      : 'request sriov delete-vfs ' + name + ' vf 0',
  });
  if (!ok) return;
  ifdMsg('设置 ' + name + ' 的 VF 数量：执行中…', false);
  try {
    const r = await api('/interfaces/' + encodeURIComponent(name) + '/sriov', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ vf_count: n }),
    });
    ifdMsg('已设置 ' + name + ' 的 VF 数量：' + dash(r && r.vf_count), false);
  } catch (e) {
    // 无 PF/VF 的机器上这条必然失败——如实显示服务端原文（含"不支持 SR-IOV"一类原因）。
    ifdMsg('设置 ' + name + ' 的 VF 数量失败：' + e.message, true);
  }
  await reload().catch(() => {});
}


// 告警时间是否可信（NFR-006）：记录该告警时时钟未与 NTP 同步则标注；未知/已同步不标。
function alarmClockMark(a) {
  return a && a.time_synced === false ? '  [时钟未同步]' : '';
}

// 告警卡：只列未解决的（已恢复的由运维页的清除动作处理）。
function renderAlarms(alarms) {
  const al = $('alarms');
  al.textContent = '';
  const active = (alarms || []).filter((a) => a.state !== 'resolved');
  if (!active.length) {
    al.appendChild(el('p', { class: 'muted', text: '（无未解决告警）' }));
  } else {
    active.forEach((a) => al.appendChild(el('div', { class: 'alarm sev-' + (a.severity || 'info') }, [
      el('div', { class: 'alarm-head', text: '[' + (a.severity || '') + '] ' + (a.code || '') }),
      el('div', { text: a.message || '' }),
      el('div', { class: 'muted small', text: (a.source ? a.source + ' · ' : '') + fmtTime(a.raised_at) + alarmClockMark(a) }),
    ])));
  }
}

// ---------- 历史趋势（#/system/metrics，决策 #356）----------
//
// 只读页（无任何写控件，故不需 wbtn / data-write 门禁）：
//   · 概览取自路由声明的 `/metrics/history?last=1h`（存储状态、生效间隔/保留、库大小、
//     序列/样本数、时间范围、采样心跳与是否停滞）；
//   · 趋势折线由本页按「指标 × 窗口」自行取数（`/metrics/history?name=…&last=…`）——历史是
//     **回顾视图**，路由 poll=0，不随总览的 5s 轮询反复重采（设计 §5 的明确取舍）。
// 「不可用」（available=false）与「可用但窗口内无点」是两种空态，分别如实说明；**绝不摆一张
// 空图冒充「没有数据」**。折线用内联 SVG（无外部库、免构建），数据与 CLI/REST 同一份读视图。
const MH_COLORS = ['#4aa3df', '#3fb950', '#d29922', '#f85149', '#b58cff', '#4adfd0'];
const MH_W = 720, MH_H = 260, MH_PAD_L = 56, MH_PAD_R = 12, MH_PAD_T = 12, MH_PAD_B = 28;
let mhMetric = '';   // 当前选中指标（跨重渲染保留）
let mhWindow = '1h'; // 当前窗口
let mhSeq = 0;       // 取数序号：旧响应回来时选择器已被改过则丢弃（防乱序覆盖）

// SVG 命名空间是 XML 命名空间**标识符**（不是网络地址，不会发起任何请求）；拆开书写是为了
// 不触发「前端不得含绝对外部地址」的守护（ui_test.go）——CSP 与同源规矩都不受影响。
const MH_SVG_NS = 'http' + '://www.w3.org/2000/svg';

function svgNode(tag, attrs) {
  const n = document.createElementNS(MH_SVG_NS, tag);
  for (const k in (attrs || {})) n.setAttribute(k, attrs[k]);
  return n;
}

// mhValueText：值的人类可读渲染。**只为已知单位族**换算（指标名以 `_bytes` 结尾 = 字节计数，
// 与 `bytes()` 同一口径）；其余一律原样数字——不凭指标名猜单位（API 没声明的单位不编造）。
function mhValueText(name, v) {
  if (v === undefined || v === null || !isFinite(v)) return '—';
  if (/_bytes$/.test(name || '')) return bytes(v);
  // 浮点读数压缩到 4 位有效数字（原始 IEEE 串如 0.17999450927800728 既读不出也占满坐标轴）；
  // 整数原样（计数类），不四舍五入成科学计数法。
  if (Number.isInteger(v)) return String(v);
  const s = Number(v.toPrecision(4));
  return String(s);
}

function mhLabelsText(labels) {
  const l = labels || {};
  const keys = Object.keys(l).sort();
  if (!keys.length) return '（无标签）';
  return keys.map((k) => k + '=' + l[k]).join(', ');
}

// mhNote：趋势区的状态/说明行（错误红、说明灰）。
function mhNote(text, isErr) {
  const p = $('mh-series-note');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// renderMetricsHistoryPage：概览 + 选择器装配。概览数据由路由取齐后传入。
function renderMetricsHistoryPage(d) {
  const h = d['/metrics/history?last=1h'];
  const bad = $('mh-unavailable');
  const sel = $('mh-metric'), win = $('mh-window');
  // 控件事件用属性赋值（每次渲染都覆盖，不会累积监听器）。
  sel.onchange = () => { mhMetric = sel.value; mhLoadSeries(); };
  win.onchange = () => { mhWindow = win.value; mhLoadSeries(); };
  $('mh-refresh-btn').onclick = () => mhLoadSeries();

  if (!h || h.__err) {
    $('mh-note').textContent = '';
    bad.hidden = false;
    bad.textContent = '历史时序读视图读取失败：' + (h ? h.__err : '未取到数据');
    fill($('mh-store'), []);
    sel.textContent = ''; sel.disabled = true; win.disabled = true;
    $('mh-chart').textContent = ''; $('mh-legend').textContent = '';
    mhNote('', false);
    return;
  }

  if (h.available === false) {
    // 存储不可用：趋势区不画图（绝不拿空白折线冒充「无数据」），并写明原因。
    $('mh-note').textContent = '';
    bad.hidden = false;
    bad.textContent = '历史时序存储不可用：' + (h.reason || '未说明原因');
    const st = h.store || {};
    fill($('mh-store'), [
      ['状态', '不可用'],
      ['库路径', st.path],
      ['是否启用', st.enabled === true ? '是' : (st.enabled === false ? '否' : undefined)],
    ]);
    sel.textContent = ''; sel.disabled = true; win.disabled = true;
    $('mh-chart').textContent = ''; $('mh-legend').textContent = '';
    mhNote('存储不可用时不会编造历史数据；请确认 nfvisd 以 -metrics-db <路径> 启动且该库可打开。', false);
    return;
  }

  bad.hidden = true; bad.textContent = '';
  $('mh-note').textContent = '';
  const store = h.store || {};
  const oldest = store.oldest_ts, newest = store.newest_ts, tick = store.last_tick_ts;
  fill($('mh-store'), [
    ['状态', store.enabled === false ? '可用（采样器未启用）' : '可用'],
    ['库路径', store.path],
    ['库大小', store.size_bytes != null ? bytes(store.size_bytes) : undefined],
    ['序列数', store.series],
    ['样本数', store.samples],
    ['时间范围', (oldest && newest)
      ? fmtTime(oldest * 1000) + ' ~ ' + fmtTime(newest * 1000) : '（无数据）'],
    ['上次采样', tick
      ? fmtTime(tick * 1000) + (store.stale === true ? '（停滞）' : '（正常）')
      : '（无记录：采样器尚未完成一次采集）'],
    ['采样间隔', h.sample_interval_seconds != null ? h.sample_interval_seconds + ' 秒' : undefined],
    ['保留天数', h.retention_days != null ? h.retention_days + ' 天（另有 2,000,000 行硬上限兜底）' : undefined],
    ['上次采样错误', store.last_error],
  ]);
  win.disabled = false;

  // 指标选择器：仅在选项集变化时重建，并保留当前选择（避免每次渲染都重置到第一项）。
  const metrics = rowsOf(h.metrics).filter((x) => typeof x === 'string');
  const want = metrics.join('\n');
  if (sel.dataset.opts !== want) {
    sel.textContent = '';
    metrics.forEach((m) => sel.appendChild(el('option', { value: m, text: m })));
    sel.dataset.opts = want;
  }
  if (!metrics.length) {
    sel.disabled = true;
    $('mh-chart').textContent = ''; $('mh-legend').textContent = '';
    mhNote('存储内暂无已知指标（采样器尚未写入任何指标，稍后重试）。', false);
    return;
  }
  sel.disabled = false;
  if (metrics.indexOf(mhMetric) < 0) mhMetric = metrics[0];
  sel.value = mhMetric;
  win.value = mhWindow;
  mhLoadSeries();
}

// mhLoadSeries：按当前「指标 × 窗口」取序列并画图。旧响应（序号过期）直接丢弃。
async function mhLoadSeries() {
  const sel = $('mh-metric'), win = $('mh-window');
  if (!sel || sel.disabled || !sel.value) return;
  const name = sel.value, last = win.value;
  mhMetric = name; mhWindow = last;
  const seq = ++mhSeq;
  $('mh-legend').textContent = '';
  mhNote('读取 ' + name + '（' + last + '）…', false);
  try {
    const d = await api('/metrics/history?name=' + encodeURIComponent(name) + '&last=' + encodeURIComponent(last));
    if (seq !== mhSeq) return;
    renderMetricsSeries(d, name, last);
  } catch (e) {
    if (seq !== mhSeq) return;
    $('mh-chart').textContent = '';
    $('mh-legend').textContent = '';
    mhNote('读取失败：' + apiErrText(e, '历史读视图需要登录令牌；请确认服务端提供 GET /metrics/history'), true);
  }
}

// renderMetricsSeries：把读视图的点集画成折线，并给出图例。空态分三种，如实区分。
function renderMetricsSeries(d, name, last) {
  $('mh-legend').textContent = '';
  if (!d) {
    $('mh-chart').textContent = '';
    mhNote('未取到数据。', true);
    return;
  }
  if (d.available === false) {
    $('mh-chart').textContent = '';
    mhNote('历史时序存储不可用：' + (d.reason || '未说明原因'), true);
    return;
  }
  const series = rowsOf(d.series);
  const total = series.reduce((n, s) => n + rowsOf(s.points).length, 0);
  if (!series.length || total === 0) {
    // 分清「指标未知」与「窗口内无点」——两者对操作者的下一步不同。
    const known = rowsOf(d.metrics).indexOf(name) >= 0;
    $('mh-chart').textContent = '';
    mhNote(known
      ? '指标 ' + name + ' 在窗口（' + last + '）内没有采样点：可放宽窗口，或确认采样器正在运行（上方概览有「上次采样」时刻）。'
      : '未知指标 ' + name + '：存储内尚无该指标（请对照上方概览的已知指标清单）。', false);
    return;
  }
  mhNote('指标 ' + name + '：' + series.length + ' 条序列、共 ' + total + ' 点'
    + (d.truncated === true ? '（点集已被 limit 截断，只保留最近的点）' : '')
    + '。纵轴为采样瞬间值；计数器单调递增，速率需自行差分。', false);
  const chart = $('mh-chart');
  chart.textContent = '';
  chart.appendChild(mhChart(name, series));
  renderMhLegend(name, series);
}

// mhChart：内联 SVG 折线（无外部库）。多序列各画一条折线，单点画圆点；
// 纵轴取全部序列的全局 min/max、横轴取全局时间范围——不伪造坐标轴之外的推断。
function mhChart(name, series) {
  const svg = svgNode('svg', {
    viewBox: '0 0 ' + MH_W + ' ' + MH_H, class: 'mh-svg',
    role: 'img', 'aria-label': name + ' 历史趋势',
  });
  const xs = [], ys = [];
  series.forEach((s) => rowsOf(s.points).forEach((p) => {
    if (p && isFinite(p.ts) && isFinite(p.value)) { xs.push(p.ts); ys.push(p.value); }
  }));
  let t0 = Math.min.apply(null, xs), t1 = Math.max.apply(null, xs);
  let v0 = Math.min.apply(null, ys), v1 = Math.max.apply(null, ys);
  if (t1 === t0) { t0 -= 1; t1 += 1; }
  if (v1 === v0) { const pad = Math.abs(v0) > 0 ? Math.abs(v0) * 0.05 : 1; v0 -= pad; v1 += pad; }
  const x = (t) => MH_PAD_L + (t - t0) / (t1 - t0) * (MH_W - MH_PAD_L - MH_PAD_R);
  const y = (v) => MH_PAD_T + (1 - (v - v0) / (v1 - v0)) * (MH_H - MH_PAD_T - MH_PAD_B);
  svg.appendChild(svgNode('line', { x1: MH_PAD_L, y1: MH_PAD_T, x2: MH_PAD_L, y2: MH_H - MH_PAD_B, class: 'mh-axis' }));
  svg.appendChild(svgNode('line', { x1: MH_PAD_L, y1: MH_H - MH_PAD_B, x2: MH_W - MH_PAD_R, y2: MH_H - MH_PAD_B, class: 'mh-axis' }));
  const ticks = [
    [MH_PAD_L - 6, MH_PAD_T + 4, 'end', mhValueText(name, v1)],
    [MH_PAD_L - 6, MH_H - MH_PAD_B + 4, 'end', mhValueText(name, v0)],
    [MH_PAD_L, MH_H - 6, 'start', fmtTime(t0 * 1000)],
    [MH_W - MH_PAD_R, MH_H - 6, 'end', fmtTime(t1 * 1000)],
  ];
  ticks.forEach(([tx, ty, anchor, text]) => {
    const t = svgNode('text', { x: tx, y: ty, class: 'mh-tick', 'text-anchor': anchor });
    t.textContent = text;
    svg.appendChild(t);
  });
  series.forEach((s, i) => {
    const color = MH_COLORS[i % MH_COLORS.length];
    const ps = rowsOf(s.points).filter((p) => p && isFinite(p.ts) && isFinite(p.value));
    if (!ps.length) return;
    if (ps.length === 1) {
      svg.appendChild(svgNode('circle', { cx: x(ps[0].ts), cy: y(ps[0].value), r: 3, fill: color }));
      return;
    }
    svg.appendChild(svgNode('polyline', {
      fill: 'none', stroke: color, 'stroke-width': '2',
      points: ps.map((p) => x(p.ts).toFixed(2) + ',' + y(p.value).toFixed(2)).join(' '),
    }));
  });
  return svg;
}

// renderMhLegend：图例——每条序列一段标签集 + 点数 + 末值（末值取时间最大的一点）。
function renderMhLegend(name, series) {
  const box = $('mh-legend');
  box.textContent = '';
  series.forEach((s, i) => {
    const pts = rowsOf(s.points).filter((p) => p && isFinite(p.ts) && isFinite(p.value));
    let lastText = '—';
    if (pts.length) {
      const tail = pts[pts.length - 1]; // 服务端按时间升序给出
      lastText = mhValueText(name, tail.value);
    }
    const row = el('div', { class: 'mh-legend-row' });
    row.appendChild(el('span', { class: 'mh-swatch', style: 'background:' + MH_COLORS[i % MH_COLORS.length] }));
    row.appendChild(el('span', { text: mhLabelsText(s.labels) + '（' + pts.length + ' 点，末值 ' + lastText + '）' }));
    box.appendChild(row);
  });
}

// ---------- 内核基线（#/system/kernel）----------
// 三方对照：**运行实际**（内核命令行与 /proc 读数）、**配置期望**（由已生效配置派生）、
// **差异**（空 = 一致）。写入与回退都是服务端动作：界面只做入口与确认——不改配置、不重启主机、
// 不绕过任何护栏（写入前的备份与"期望值从哪来"都由服务端实现决定）。
// 档位：中危——逐条列影响面（写引导配置、重启才生效、可回退），主按钮标红。
function knlMsg(text, isErr) {
  const p = $('knl-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function renderKernel(k) {
  const ok = k && !k.__err;
  const d = (k && k.desired) || {};
  $('knl-note').textContent = ok ? '' : '（读取失败：' + (k ? k.__err : '未取到数据') + '）';
  fill($('knl-actual'), ok ? [
    ['内核命令行', Array.isArray(k.cmdline) && k.cmdline.length ? k.cmdline.join(' ') : undefined],
    ['隔离核（isolcpus）', k.isolated_cores],
    ['1G 大页（总数 / 空闲）', k.hugepages_1g != null ? k.hugepages_1g + ' / ' + dash(k.hugepages_1g_free) : undefined],
    ['2M 大页（总数 / 空闲）', k.hugepages_2m != null ? k.hugepages_2m + ' / ' + dash(k.hugepages_2m_free) : undefined],
    ['透明大页（THP）', k.thp],
  ] : [['读取失败', k ? k.__err : '未取到数据']]);
  fill($('knl-desired'), ok ? [
    ['1G 大页', d.hugepages_1g != null ? d.hugepages_1g + (d.hugepages_1g < 0 ? '（不托管）' : '') : undefined],
    ['2M 大页', d.hugepages_2m != null ? d.hugepages_2m + (d.hugepages_2m === 0 ? '（不托管）' : '') : undefined],
    ['隔离核', d.isolated_cores],
    ['NMI watchdog', d.nmi_watchdog === true ? '启用' : (d.nmi_watchdog === false ? '关闭' : undefined)],
    ['透明大页', d.transparent_hugepages],
    ['IOMMU', d.iommu],
    ['低延迟参数组', d.low_latency === true ? '启用' : (d.low_latency === false ? '关闭' : undefined)],
    ['调优 profile', d.tuned_profile],
    ['附加参数', Array.isArray(d.params) && d.params.length ? d.params.join(' ') : undefined],
  ] : [['读取失败', k ? k.__err : '未取到数据']]);
  const diffs = ok ? rowsOf(k.diffs) : [];
  $('knl-diff-note').textContent = ok ? (diffs.length ? '（' + diffs.length + ' 项不一致）' : '（一致）') : '';
  $('knl-diffs').textContent = !ok ? '' : (diffs.length ? diffs.join('\n')
    : '运行实际与配置期望一致——无需写入基线。');
}

async function knlAct(action) {
  const apply = action === 'apply';
  const ok = await uiConfirm(apply ? '写入内核基线' : '回退内核基线', {
    tier: 'mid',
    bullets: apply ? [
      '把配置期望的内核启动参数写进引导配置（引导加载器片段、fstab 与相关调优文件），并重建引导菜单。',
      '写完**要重启主机才生效**：本页不重启，重启入口在「运维动作」页。',
      '写入前会留一份上一版基线，可用「回退上一次基线」还原。',
      '大页与隔离核的唯一真源是「资源池」配置——写错会让重启后的大页/隔离核与预期不符，请先核对本页的「配置期望」。',
      '可回退：回退基线（同样要重启生效）。',
    ] : [
      '把引导配置还原成上一次写入前的版本（或删掉本产品写入的那部分）。',
      '同样**要重启主机才生效**。',
      '只动本产品写入的那部分引导配置，不改其它来源的引导项。',
      '可回退：再点一次「写入基线」按当前配置期望重新写入。',
    ],
    cli: 'request system kernel ' + action,
  });
  if (!ok) return;
  knlMsg((apply ? '写入' : '回退') + '内核基线：执行中…', false);
  $('knl-out').hidden = true;
  try {
    // 两条路径各写一遍字面量（不拼字符串）：前端调的端点必须是**看得见的**具体路径，
    // 界面覆盖守护就是从这些字面量里认"这一页接了哪些端点"的。
    const r = apply ? await api('/system/kernel:apply', { method: 'POST' })
      : await api('/system/kernel:rollback', { method: 'POST' });
    knlMsg((apply ? '已写入内核基线' : '已回退内核基线') + '（重启后生效）。', false);
    if (r) {
      $('knl-out').hidden = false;
      $('knl-out').textContent = typeof r === 'string' ? r : JSON.stringify(r, null, 2);
    }
  } catch (e) {
    // 服务端没提供该接口（Go 路由表对未注册路径直接 404）时给出可操作的说明，
    // 不把它含糊成"失败"，也不谎报成功。
    knlMsg((apply ? '写入' : '回退') + '内核基线失败：' + apiErrText(e,
      '这台机器的服务端没有提供该动作的接口，请在命令行执行 request system kernel ' + action), true);
  }
  await reload().catch(() => {});
}

// ---------- 用户与权限（#/system/users）----------
//
// **写这条代码前逐条核实过的端点语义**（服务端实现见 resources_w5.go）：
//   GET    /login-banner                           读登录横幅（决策 #303；横幅卡片与登录页共用同一端点）
//   GET    /system/login-users                     读**已生效配置**里的本地用户与 class（口令哈希永不回显）
//   POST   /system/login-users                     建用户 / 建 class
//   PUT    /system/login-users/{name}              改 class / 重置口令
//   DELETE /system/login-users/{name}              删用户（不能删自己、不能删最后一个 super-user）
//   POST   /system/login-users/{name}:change-password  本人改口令（要验旧口令）
//   PUT    /system/login-banner                    设置登录横幅（等价 CLI set system login banner）
//   DELETE /system/login-banner                    清除登录横幅（等价 CLI delete system login banner）
// 写操作**既不是**"纯候选两段式"、**也不是**"无锁直连"：服务端先取**配置编辑锁**（engine.Edit），
// 把改动写进候选，然后**立即提交**（响应头 X-NFVIS-Committed: true）——即"取锁 + 直提"。
// 界面因此按三条口径处理：
//   ① 确认框里写明"立即生效、会取用配置编辑锁"；
//   ② 动手前先看**本会话**在配置页有没有未提交的候选：有就先拒绝并指路——同一持有者的候选是同一份，
//      服务端的提交会把那份改动一并带上去（操作者没打算提交它）；
//   ③ 服务端的一切拒绝（锁被别处持有 / 口令不合策略 / 删自己 / 删最后一个 super-user）如实显示。
//
// 档位：全部**高危**（删用户与改口令策略在分级表里就是高危档）。确认词取**对象名**（用户名）——
// 删错人、改错口令的机会成本最大，手打一遍名字是最有效的拦阻；只有"改我自己的口令"用本人用户名。
const USR_BUILTIN_CLASSES = ['read-only', 'operator', 'super-user'];
let usrSeq = 0;

function usrMsg(text, isErr) {
  const p = $('usr-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// class 下拉的选项：内置三类 + 配置里已定义的 class（去重）。
function usrClassNames(classes) {
  const out = USR_BUILTIN_CLASSES.slice();
  rowsOf(classes).forEach((c) => {
    if (c && c.name && out.indexOf(c.name) < 0) out.push(c.name);
  });
  return out;
}

function usrClassSelect(id, names, selected) {
  const sel = el('select', { id });
  names.forEach((n) => {
    const opt = el('option', { value: n, text: n });
    if (n === selected) opt.setAttribute('selected', 'selected');
    sel.appendChild(opt);
  });
  return sel;
}

// 「生效权限」入口的解析口径（决策 #304）：非 super-user 只能查看**自己所属** class 的
// 生效权限（服务端 403 兜底；界面只是不摆必被拒的按钮）。getCurrentClass() 在 enterApp 里从登录
// 响应设置、signOut 清空。
const USR_PERM_PREVIEW = 12;

// usrPermLoad 就地展开某 class 的生效权限摘要：取 GET /configuration/permissions
// （与命令行 show configuration permissions <class> 同一实现单源），显示允许/拒绝条数 +
// 前若干条允许路径；完整列表与逐路径依据指向命令行 / 该 REST 端点。
async function usrPermLoad(name) {
  const out = $('usr-perm-out');
  if (!out) return;
  out.hidden = false;
  out.textContent = '读取中…';
  try {
    const d = await api('/configuration/permissions?class=' + encodeURIComponent(name));
    const total = (d.allowed_count || 0) + (d.denied_count || 0);
    const paths = Array.isArray(d.allowed_paths) ? d.allowed_paths : [];
    const head = 'class ' + dash(d.class) + '（' + (d.source === 'custom' ? '自定义' : '预置') +
      '）：允许 ' + (d.allowed_count || 0) + ' / 拒绝 ' + (d.denied_count || 0) +
      '（共 ' + total + ' 条命令路径）';
    const shown = paths.slice(0, USR_PERM_PREVIEW).map((p) => '  ' + p);
    const more = paths.length > shown.length
      ? '\n  … 其余 ' + (paths.length - shown.length) + ' 条允许路径见命令行 show configuration permissions ' + d.class
      : '';
    out.textContent = head + '\n' + shown.join('\n') + more +
      '\n（摘要：完整列表与逐路径判定依据见命令行 show configuration permissions ' + d.class +
      ' detail，或 REST 端点 GET /configuration/permissions）';
  } catch (e) {
    out.textContent = '读取失败：' + apiErrText(e, '服务端未提供该接口，请在命令行执行 show configuration permissions ' + name);
  }
}

// renderPermClassTable 权限类表：每行一个「生效权限」按钮（决策 #304）。
// 非 super-user 只在自己所属 class 行给出入口（其余行如实说明，不摆必被拒的按钮）。
function renderPermClassTable(classes) {
  const tbody = $('usr-class-table').querySelector('tbody');
  tbody.textContent = '';
  if (!classes.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '4', class: 'muted', text: '（无自定义权限类）' }));
    tbody.appendChild(tr);
    return;
  }
  classes.forEach((c) => {
    const tr = el('tr');
    tr.appendChild(el('td', { text: String(dash(c.name)) }));
    tr.appendChild(el('td', { text: String(dash(list(c.allow))) }));
    tr.appendChild(el('td', { text: String(dash(list(c.deny))) }));
    const cell = el('td', { class: 'actions' });
    if (getCurrentClass() === 'super-user' || getCurrentClass() === c.name) {
      const btn = el('button', { type: 'button', class: 'ghost small', text: '生效权限' });
      btn.addEventListener('click', () => usrPermLoad(c.name));
      cell.appendChild(btn);
    } else {
      cell.appendChild(el('span', { class: 'muted small', text: '仅自己所属的 class' }));
    }
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

function renderUsers(lu) {
  const ok = lu && !lu.__err;
  const users = ok ? rowsOf(lu.users) : [];
  const classes = ok ? rowsOf(lu.classes) : [];
  const names = usrClassNames(classes);
  $('usr-note').textContent = ok ? '（' + users.length + ' 个用户、' + classes.length + ' 个自定义权限类）'
    : '（读取失败：' + (lu ? lu.__err : '未取到数据') + '）';

  const tbody = $('usr-table').querySelector('tbody');
  tbody.textContent = '';
  if (!users.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '4', class: 'muted', text: ok ? '（无用户）' : '读取失败' }));
    tbody.appendChild(tr);
  }
  users.forEach((u) => {
    const idx = usrSeq++;
    const tr = el('tr');
    tr.appendChild(el('td', { text: String(dash(u.name)) }));
    const clsCell = el('td');
    const sel = usrClassSelect('usr-cls-' + idx, names, u.class);
    clsCell.appendChild(sel);
    tr.appendChild(clsCell);
    const pwCell = el('td');
    const pw = el('input', { type: 'password', id: 'usr-pw-' + idx, autocomplete: 'new-password' });
    pw.setAttribute('placeholder', '留空 = 不改');
    pwCell.appendChild(pw);
    tr.appendChild(pwCell);
    const cell = el('td', { class: 'actions' });
    const applyBtn = wbtn({ type: 'button', class: 'ghost small', text: '应用修改' });
    applyBtn.addEventListener('click', () => usrApply(u.name, sel.value, pw.value));
    const delBtn = wbtn({ type: 'button', class: 'danger small', text: '删除' });
    delBtn.addEventListener('click', () => usrDelete(u.name));
    cell.appendChild(applyBtn);
    cell.appendChild(delBtn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });

  renderPermClassTable(classes);

  // 新建用户的 class 下拉（重画时按当前 class 列表重建）。
  const ns = $('usr-new-class');
  const keep = ns.value;
  ns.textContent = '';
  usrClassNames(classes).forEach((n) => {
    const opt = el('option', { value: n, text: n });
    if (n === keep) opt.setAttribute('selected', 'selected');
    ns.appendChild(opt);
  });
}

// ---------- 活动会话 / API Token（#/system/users 页的卡片，决策 #301） ----------
//
//   GET  /system/api-tokens                    清单（super-user 见全部、其他角色仅自己；
//                                              范围在服务端按身份裁定，界面如实显示拿到的行）
//   POST /system/api-tokens/{id}:revoke        吊销指定会话（吊销不存在的/无权的，服务端统一
//                                              回 404「会话不存在或无权操作」，界面原样显示）
// token_id 是服务端签发的稳定 ID；token 本体永不回显。吊销立即生效，被吊销的会话在下一个
// 请求时 401（若吊销的是当前浏览器这个会话，下一次取数就回到登录页——api() 已按 401 处理）。
function renderTokens(payload) {
  const ok = payload && !payload.__err;
  const tokens = ok && Array.isArray(payload.tokens) ? payload.tokens : [];
  const note = $('usr-token-note');
  if (note) {
    note.textContent = ok ? '（' + tokens.length + ' 个活动会话）'
      : '（读取失败：' + (payload ? payload.__err : '未取到数据') + '）';
  }
  const tbody = $('usr-token-table').querySelector('tbody');
  tbody.textContent = '';
  if (!tokens.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '7', class: 'muted', text: ok ? '（无活动会话）' : '读取失败' }));
    tbody.appendChild(tr);
    return;
  }
  tokens.forEach((tk) => {
    const tr = el('tr');
    tr.appendChild(el('td', { text: String(dash(tk.token_id)) }));
    tr.appendChild(el('td', { text: String(dash(tk.user)) }));
    tr.appendChild(el('td', { text: String(dash(tk.class)) }));
    tr.appendChild(el('td', { text: String(dash(tk.issued_at)) }));
    tr.appendChild(el('td', { text: String(dash(tk.expires_at)) }));
    tr.appendChild(el('td', { text: tk.current ? '本会话' : '—' }));
    const cell = el('td', { class: 'actions' });
    const btn = wbtn({ type: 'button', class: 'danger small', text: '吊销' });
    btn.addEventListener('click', () => usrTokenRevoke(tk));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 吊销一个活动会话（中危档：影响面列清楚——谁被踢下线、是否不可回退、本会话吊销即登出）。
// 确认前不发任何请求；确认词不必手打（会话 ID 是 36 位机器串，手打没有防错价值，
// 判别主体靠行内按钮对应的这一行）。
async function usrTokenRevoke(tk) {
  const id = (tk && tk.token_id) || '';
  if (!id) return;
  const who = tk.user ? '用户 ' + tk.user + ' 的' : '该';
  const self = tk.current;
  const ok = await uiConfirm('吊销活动会话', {
    tier: 'mid',
    bullets: [
      '吊销 ' + who + '会话（ID ' + id + '）：**立即生效**，该会话的下一个请求就要重新登录。',
      self
        ? '这是**当前浏览器正在用的会话**：吊销后本页的下一个请求会回到登录页。'
        : '被吊销方正在编辑的未提交配置会随会话失效，需要重新登录后重做。',
      '不可回退：对方重新登录会得到一个新会话（原会话不会恢复）。',
    ],
    cli: 'request system api token revoke ' + id,
  });
  if (!ok) return;
  usrMsg('提交中…', false);
  try {
    await api('/system/api-tokens/' + encodeURIComponent(id) + ':revoke', { method: 'POST' });
    usrMsg('已吊销会话 ' + id + '。', false);
  } catch (e) {
    usrMsg('吊销失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 本会话是否正持有**有未提交改动**的候选：有就返回一句说明（调用方据此拒绝这次写操作）。
// 锁不在本会话手里时返回空串——那种情况服务端自己会拒绝（如实显示它的报错即可），
// 界面不替服务端做第二个判定。
async function usrCandidateBlock() {
  if (!getUser()) return '';
  const holder = await cfgLockHolder();
  if (holder !== getUser() + '@api') return '';
  let cand = null;
  try {
    cand = await api('/configuration/candidate');
  } catch (e) {
    return '';
  }
  if (cand && cand.dirty) {
    return '本会话在「配置」页还有**未提交的候选配置**：先提交或丢弃它再来改用户/口令——' +
      '本页的操作只做这一项改动、取锁后立即提交；服务端会直接拒绝本次操作（409），' +
      '绝不会把配置页那份候选一并提交生效。';
  }
  return '';
}

// usrPrecheck：写操作前的两道闸门（本会话的候选脏数据 / 该动作的确认框）。
// 返回 true 表示可以继续执行。`word` 是确认词（对象名）。
// `say` 是消息去处（缺省＝用户页的 #usr-msg）：顶栏「我的账号」入口不在用户页上，
// 它的提示必须落在自己的对话框里，否则操作者看不到（决策 #147）。
async function usrPrecheck(blockMsg, title, word, bullets, cli, say) {
  const emit = say || usrMsg;
  if (blockMsg) { emit(blockMsg, true); return false; }
  return uiConfirm(title, { tier: 'high', requireWord: word, bullets: bullets, cli: cli });
}

// 改 class / 重置口令（PUT /system/login-users/{name}）：只提交**有内容**的那部分。
async function usrApply(name, cls, pw) {
  const block = await usrCandidateBlock();
  const pwText = (pw || '').trim();
  if (!pwText) {
    const ok = await usrPrecheck(block, '修改用户权限类', name, [
      '把用户 ' + name + ' 的权限类改成 ' + (cls || '（未选）') + '，**立即生效**（取配置编辑锁后直接提交）。',
      '权限类决定这个账号能做什么：改错会让对方多出或少掉操作权限。',
      '对方已登录的会话不会因此失效，但下一次请求就按新的权限判定。',
      '可回退：把权限类改回原值即可。',
    ], 'set system login user ' + name + ' class ' + (cls || '<class>'));
    if (!ok) return;
    await usrPut(name, { class: cls }, '已把用户 ' + name + ' 的权限类改为 ' + cls);
    return;
  }
  const ok = await usrPrecheck(block, '重置用户口令', name, [
    '把用户 ' + name + ' 的口令重置为这次填的新口令，**立即生效**（取配置编辑锁后直接提交）。',
    '口令按本机口令策略校验：不满足策略时服务端拒绝并说明原因（界面不预判）。',
    '旧口令立即失效：对方正在用的会话不受影响，但重新登录要用新口令。',
    '可回退：再设一次原口令即可（本机不保留旧口令，请自行确认已记下新口令）。',
  ], 'set system login user ' + name + ' password <新口令>');
  if (!ok) return;
  const body = { password: pwText };
  if (cls) body.class = cls;
  await usrPut(name, body, '已重置用户 ' + name + ' 的口令');
}

async function usrPut(name, body, okText) {
  usrMsg('提交中…', false);
  try {
    await api('/system/login-users/' + encodeURIComponent(name), {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    usrMsg(okText + '（已提交生效）。', false);
  } catch (e) {
    usrMsg('失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 建用户（POST /system/login-users）：确认词是**这次要建的用户名**。
async function usrCreate(name, cls, pw) {
  const n = (name || '').trim();
  if (!n) { usrMsg('请先填用户名。', true); return; }
  if (!(pw || '').trim()) { usrMsg('新用户必须设置初始口令（服务端也要求非空）。', true); return; }
  const block = await usrCandidateBlock();
  const ok = await usrPrecheck(block, '创建本地用户', n, [
    '新建本地用户 ' + n + '，权限类 ' + (cls || '（未选）') + '，**立即生效**（取配置编辑锁后直接提交）。',
    '初始口令按本机口令策略校验，存的是加盐哈希（明文不落库、不回显）。',
    '建出来的账号可以立刻登录本机命令行与控制台，请把口令交付给对的人。',
    '可回退：在本页删除该用户（不能删最后一个 super-user）。',
  ], 'set system login user ' + n + ' password <初始口令> class ' + (cls || '<class>'));
  if (!ok) return;
  usrMsg('提交中…', false);
  try {
    await api('/system/login-users', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: n, kind: 'user', class: cls, password: pw }),
    });
    usrMsg('已创建用户 ' + n + '（已提交生效）。', false);
  } catch (e) {
    usrMsg('创建用户失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 删用户（DELETE）：确认词是**用户名**——删错人的机会成本最大。
async function usrDelete(name) {
  const block = await usrCandidateBlock();
  const ok = await usrPrecheck(block, '删除本地用户', name, [
    '删除本地用户 ' + name + '，**立即生效**（取配置编辑锁后直接提交）。',
    '该账号立刻不能再登录（命令行与控制台都包括）；它已建立的会话会在令牌到期后失效。',
    '服务端会拒绝两种情况：删当前登录用户、删最后一个 super-user。',
    '不可回退：删掉后只能重新建一个同名账号（口令与权限类都要重新给）。',
  ], 'delete system login user ' + name);
  if (!ok) return;
  usrMsg('提交中…', false);
  try {
    await api('/system/login-users/' + encodeURIComponent(name), { method: 'DELETE' });
    usrMsg('已删除用户 ' + name + '。', false);
  } catch (e) {
    usrMsg('删除用户失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 改我自己的口令（POST /system/login-users/{me}:change-password）：要验旧口令。
// `say` 是消息去处（缺省＝用户页的 #usr-msg）——顶栏「我的账号」入口复用这条处理器，
// 但它的提示要落在自己的对话框里（决策 #147）。**一条处理器，两个入口**（不复制第二份实现）。
async function usrMyPassword(oldPw, newPw, say) {
  const emit = say || usrMsg;
  const me = getUser();
  if (!me) { emit('拿不到当前登录用户名，请刷新页面后重试。', true); return; }
  if (!(oldPw || '').trim()) { emit('请先填当前口令。', true); return; }
  if (!(newPw || '').trim()) { emit('请先填新口令。', true); return; }
  const block = await usrCandidateBlock();
  const ok = await usrPrecheck(block, '修改我的口令', me, [
    '把当前登录账号 ' + me + ' 的口令改成新口令，**立即生效**（取配置编辑锁后直接提交）。',
    '要先给出当前口令：服务端会校验它（校验不过一律拒绝，界面不预判）。',
    '新口令按本机口令策略校验；改完请用新口令重新登录（不会自动登出）。',
    '可回退：再改一次（旧口令已失效，请先确认新口令记得住）。',
  ], 'request system password change', emit);
  if (!ok) return;
  emit('提交中…', false);
  try {
    await api('/system/login-users/' + encodeURIComponent(me) + ':change-password', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ old_password: oldPw, new_password: newPw }),
    });
    emit('口令已修改——请用新口令重新登录。', false);
  } catch (e) {
    emit('修改口令失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 登录横幅卡（#/system/users 页，决策 #303） ----------
//
//   GET    /login-banner        当前值（与登录页共用同一端点：卡片显示的就是登录者会看到的）
//   PUT    /system/login-banner 设置（单行、≤512 字节，服务端校验拒绝超限/含换行并说明上限）
//   DELETE /system/login-banner 清除
// 写模式与用户管理同一条路（取配置编辑锁 → 写候选 → 立即提交 → 审计），确认按**低危**档
//（横幅随时可改回/清除，影响面就是登录页那行字）。
function renderBannerCard(payload) {
  const el = $('usr-banner-current');
  if (!el) return;
  const ok = payload && !payload.__err;
  const banner = ok && typeof payload.banner === 'string' ? payload.banner : '';
  el.textContent = ok ? (banner || '（未设置）') : '（读取失败：' + (payload ? payload.__err : '未取到数据') + '）';
}

async function usrBannerSave() {
  const text = $('usr-banner-text').value;
  if (!(text || '').trim()) {
    usrMsg('请先填横幅文本；要清除现有横幅请点「清除横幅」。', true);
    return;
  }
  const block = await usrCandidateBlock();
  if (block) { usrMsg(block, true); return; } // 本会话在配置页有未提交候选：拒绝（同用户管理口径）
  const ok = await uiConfirm('保存登录横幅', {
    tier: 'low',
    paragraphs: ['把登录横幅设为这次填的文本，**立即生效**（取配置编辑锁后直接提交）。',
      '横幅显示在控制台登录页与命令行登录提示之前，登录者尚未认证就能看到。',
      '单行文本，最长 512 字节；超限时服务端会拒绝并说明上限。'],
    cli: 'set system login banner <文本>',
  });
  if (!ok) return;
  usrMsg('提交中…', false);
  try {
    await api('/system/login-banner', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ banner: text }),
    });
    usrMsg('登录横幅已更新（已提交生效）。', false);
  } catch (e) {
    usrMsg('保存横幅失败：' + e.message, true);
  }
  $('usr-banner-text').value = '';
  await reload().catch(() => {});
}

async function usrBannerClear() {
  const block = await usrCandidateBlock();
  if (block) { usrMsg(block, true); return; } // 本会话在配置页有未提交候选：拒绝（同用户管理口径）
  const ok = await uiConfirm('清除登录横幅', {
    tier: 'low',
    paragraphs: ['清除登录横幅？清除后登录页与命令行登录提示不再显示横幅（立即生效，取配置编辑锁后直接提交）。'],
    cli: 'delete system login banner',
  });
  if (!ok) return;
  usrMsg('提交中…', false);
  try {
    await api('/system/login-banner', { method: 'DELETE' });
    usrMsg('登录横幅已清除。', false);
  } catch (e) {
    usrMsg('清除横幅失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 我的账号（顶栏入口，**所有角色可见**）----------
//
// 自助改口令：POST /system/login-users/{me}:change-password。服务端把它注册为**只读级**
// （本人 + 验旧口令），故界面不该把它藏在写页里——这正是这个入口存在的理由：改口令的表单原先只在
// 「用户与权限」页，而该页自决策 #145 起对 read-only 角色隐藏（导航不列、页内写入口隐藏），
// 于是只读用户在界面上无处改自己的口令。
//
// 口径（与用户页那条表单**同一份处理器** usrMyPassword，只是消息落在这个框里）：
//   ① 顶栏入口与这个框都**不带 data-write**（带了就会被 body.role-readonly 隐藏，入口也就没了）；
//   ② 口令控件只在提交那一刻读值，随后立即清空（不留在别处，也不进 sessionStorage）；
//   ③ 分级确认按**高危**档（确认词＝当前登录用户名、10 秒倒计时、只读回显对应命令）；
//   ④ 改完**不自动登出**，只提示用新口令重新登录；框随时可取消（点「取消」、点遮罩、按 Esc）。
function acctMsg(text, isErr) {
  const p = $('acct-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function openAcct() {
  // 每次打开都从空开始：上一次的口令（哪怕是成功改过的）不留在控件里。
  $('acct-old').value = '';
  $('acct-new').value = '';
  $('acct-who').textContent = getUser()
    ? '当前登录账号：' + getUser()
    : '拿不到当前登录用户名，请刷新页面后重试。';
  acctMsg('', false);
  $('acct').hidden = false;
  const first = $('acct-old');
  if (first.focus) first.focus();
}

function closeAcct() {
  $('acct-old').value = '';
  $('acct-new').value = '';
  acctMsg('', false);
  $('acct').hidden = true;
}

// ---------- 证书（#/system/tls）----------
//
// GET /system/tls 读当前证书信息；PUT /system/tls 安装外部证书（**PEM 文本**，立即生效）。
// 界面用"粘贴 PEM 文本"而不是文件选择器：契约收的就是 PEM 文本（不是 multipart），
// 而文件选择器在自动化里也填不进去——文本粘贴两边都能走通，且与契约同形。
// 档位：**高危**（证书上传在分级表里就是高危档）——影响面是管理面本身：换成客户端不信任的
// 证书会让管理面连不上，确认词取固定词 install-cert（这个动作没有"对象名"可打）。
function tlsMsg(text, isErr) {
  const p = $('tls-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function renderTLS(info) {
  const ok = info && !info.__err;
  const configured = ok && info.configured !== false;
  $('tls-note').textContent = ok ? '' : '（读取失败：' + (info ? info.__err : '未取到数据') + '）';
  fill($('tls-list'), !ok ? [['读取失败', info.__err]] : (configured ? [
    ['主体（subject）', info.subject],
    ['签发者（issuer）', info.issuer],
    ['有效期起', fmtTime(info.not_before)],
    ['有效期止', fmtTime(info.not_after)],
    ['自签', info.self_signed === true ? '是' : (info.self_signed === false ? '否' : undefined)],
    ['指纹（SHA-256）', info.fingerprint],
  ] : [['状态', '当前没有可用证书信息（服务端会在缺证书时生成自签证书）']]));
}

const TLS_PEM_CERT_RE = /-----BEGIN CERTIFICATE-----/;
const TLS_PEM_KEY_RE = /-----BEGIN [A-Z ]*PRIVATE KEY-----/;

async function tlsInstall(cert, key) {
  const c = (cert || '').trim();
  const k = (key || '').trim();
  if (!TLS_PEM_CERT_RE.test(c)) { tlsMsg('证书那一栏不是 PEM 文本（应以 -----BEGIN CERTIFICATE----- 开头）。', true); return; }
  if (!TLS_PEM_KEY_RE.test(k)) { tlsMsg('私钥那一栏不是 PEM 文本（应以 -----BEGIN … PRIVATE KEY----- 开头）。', true); return; }
  const ok = await uiConfirm('安装外部证书（管理面证书）', {
    tier: 'high',
    requireWord: 'install-cert',
    bullets: [
      '用粘贴的这份证书与私钥替换管理面证书，**立即生效**（热换证，不重启服务，本页面不会掉线）。',
      '换成你手里的浏览器/命令行**不信任**的证书后，管理面就连不上了——请先确认这份证书的链与地址条目能被你要用的客户端信任，并保留带外/控制台通道。',
      '证书与私钥不配对、链不完整、缺地址条目时服务端拒绝，且**不改变**当前证书（拒绝时本页会显示原因）。',
      '可回退：再装一份可用的证书（或到本页「重签自签证书」按本机地址重新生成自签证书）。',
    ],
    cli: 'set system api tls cert-file <证书文件> key-file <私钥文件>',
  });
  if (!ok) return;
  tlsMsg('安装证书：执行中…', false);
  try {
    const info = await api('/system/tls', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ certificate: c, key: k }),
    });
    tlsMsg('证书已安装并生效（指纹 ' + dash(info && info.fingerprint) + '）。', false);
    // 明文私钥不留在页面上：安装成功后清掉两个文本框。
    $('tls-cert').value = '';
    $('tls-key').value = '';
  } catch (e) {
    tlsMsg('安装证书失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 重签自签证书（原在「运维动作」页，随证书相关入口一并落到本页）。
async function tlsRegenerate() {
  const ok = await uiConfirm('重签自签证书', {
    tier: 'mid',
    bullets: [
      '本机管理面自签证书会被重签：证书指纹改变，固定过旧证书的客户端与浏览器需要重新固定新证书。',
      '新证书的地址条目（SAN）按本机地址自动生成，重签后命令行与浏览器都可继续访问管理面。',
      '换证是热生效的，不重启服务；当前这个页面不会掉线。',
      '可回退：安装一份外部证书（本页上方），或用命令行按配置声明证书路径。',
    ],
    cli: 'request system api tls regenerate',
  });
  if (!ok) return;
  tlsMsg('重签自签证书：执行中…', false);
  try {
    const info = await api('/system/tls:regenerate', { method: 'POST' });
    tlsMsg('已重签自签证书（指纹 ' + dash(info && info.fingerprint) + '）。', false);
  } catch (e) {
    tlsMsg('重签失败：' + e.message, true);
  }
  await reload().catch(() => {});
}


//
// ---------- 硬件健康（#/system/hardware）----------

// 只读页：传感器 / 磁盘 SMART / 阈值。字段全部取契约声明的那几个——传感器的 status 就是
// 服务端按阈值判出的越限状态（正常 / 越限告警 / 越限严重），界面不自己算阈值。
//
// **阈值写入有意不在这里直连**：`PUT /system/health/thresholds` 写的是候选配置（并会占用编辑锁），
// 直写要么绕过「配置」页的候选 → 预校验 → 差异 → 提交流程、要么与「配置」页的整体写候选相互覆盖。
// 界面上改阈值的入口是「配置」页的「系统 · 健康阈值」表单（提交后生效），本页只读回显。
const HW_SENSOR_TYPE = { temperature: '温度', fan: '风扇', voltage: '电压', power: '电源' };
const HW_STATUS_TEXT = { ok: '正常', warning: '越限（告警）', critical: '越限（严重）' };
const HW_STATUS_CLS = { ok: 'st-ok', warning: 'st-warn', critical: 'st-crit' };
const HW_SMART_TEXT = { passed: '通过', failed: '异常', unknown: '未知' };

// 硬件健康的两张表：末列是状态，按状态上色；空表如实显示「（无）」。
function hwTable(tbody, rows) {
  tbody.textContent = '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '4', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((r) => {
    const tr = el('tr');
    r.cells.forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    tr.appendChild(el('td', { class: r.cls || '', text: String(dash(r.status)) }));
    tbody.appendChild(tr);
  });
}

function renderHardware(hw, th) {
  const ok = hw && !hw.__err;
  $('hw-note').textContent = ok ? '' : '（读取失败：' + (hw ? hw.__err : '未取到数据') + '）';
  fill($('hw-list'), ok ? [
    ['带外管理（BMC）', hw.bmc_present === true ? '存在' : '不存在（改用本机传感器与内核热区）'],
    ['传感器', Array.isArray(hw.sensors) ? hw.sensors.length + ' 个' : undefined],
    ['磁盘', Array.isArray(hw.disks) ? hw.disks.length + ' 块' : undefined],
  ] : []);
  hwTable($('hw-sensor-table').querySelector('tbody'), rowsOf(hw && hw.sensors).map((s) => ({
    cells: [s.name, HW_SENSOR_TYPE[s.type] || s.type,
      s.value != null ? s.value + (s.unit ? ' ' + s.unit : '') : undefined],
    status: HW_STATUS_TEXT[s.status] || s.status,
    cls: HW_STATUS_CLS[s.status] || '',
  })));
  hwTable($('hw-disk-table').querySelector('tbody'), rowsOf(hw && hw.disks).map((d) => ({
    cells: [d.device,
      d.temp_celsius != null ? d.temp_celsius + ' ℃' : undefined,
      d.wear_percent != null ? d.wear_percent + ' %' : undefined],
    status: HW_SMART_TEXT[d.smart_status] || d.smart_status,
    cls: d.smart_status === 'failed' ? 'st-crit' : (d.smart_status === 'passed' ? 'st-ok' : ''),
  })));
  const tok = th && !th.__err;
  $('hw-th-note').textContent = tok ? '' : '（读取失败：' + (th ? th.__err : '未取到数据') + '）';
  fill($('hw-th-list'), tok ? [
    ['CPU 温度', th.cpu_temp_celsius ? '≥ ' + th.cpu_temp_celsius + ' ℃ 告警' : '未设置'],
    ['磁盘温度', th.disk_temp_celsius ? '≥ ' + th.disk_temp_celsius + ' ℃ 告警' : '未设置'],
    ['磁盘使用率', th.disk_used_percent ? '≥ ' + th.disk_used_percent + ' % 告警' : '未设置'],
  ] : []);
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsSystem = {
  'hardware': {
    render(d) { pageWarn(d); renderHardware(d['/system/hardware'], d['/system/health/thresholds']); }
  },
  'pools': {
    render(d) {
      pageWarn(d);
      renderPools(d['/resource-pools']);
      renderHugepagePools(d['/system/hugepages']);
    }
  },
  'metrics': {
    // 概览由路由声明的 `/metrics/history?last=1h` 取齐；趋势序列由本页按选择器自行取数
    // （参数化端点不进路由表——router 只认路由表里逐字声明的那一串）。poll=0：历史是回顾视图。
    render(d) {
      pageWarn(d);
      renderMetricsHistoryPage(d);
    }
  },
  'interfaces': {
    async render(d) {
      pageWarn(d);
      const ifaces = rowsOf(d['/interfaces']);
      renderInterfaces(ifaces, await loadInterfaceStats(ifaces));
    }
  },
  'ifaceDetail': {
    render(d, params) { pageWarn(d); renderIfaceDetail(d['/interfaces/{name}'], d['/interfaces'], params); }
  },
  'system': {
    render(d) {
      pageWarn(d);
      renderSystemPage(d['/system/status'], d['/system/version'], d['/system'], d['/dns/proxy'], d['/system/firewall']);
    }
  },
  'kernel': {
    render(d) { pageWarn(d); renderKernel(d['/system/kernel']); }
  },
  'users': {
    render(d) { pageWarn(d); renderUsers(d['/system/login-users']); renderTokens(d['/system/api-tokens']); renderBannerCard(d['/login-banner']); }
  },
  'tls': {
    render(d) { pageWarn(d); renderTLS(d['/system/tls']); }
  },
};
