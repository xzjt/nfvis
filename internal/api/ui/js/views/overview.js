// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 总览域：系统卡 / 数据面卡 / 告警卡 / 最近事件。

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

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsoverviewRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsoverviewRouterModule()).render();
}



// 系统卡（含组件版本）。数字全部取自 /system/status（与 CLI show system 同源）；
// 字段缺席（非 Linux 无宿主指标等）时对应行显示「—」，不编造。
//
// 两处用到同一份字段表（总览页的系统卡、系统域页 #/system）：抽成 systemPairs/versionPairs，
// 免得两页各写一份、加字段时漏改一处（同一份事实两处实现）。
function systemPairs(st) {
  const cpu = (st && st.cpu) || {};
  const mem = (st && st.memory) || {};
  const disk = (st && st.storage) || {};
  return [
    ['主机名', st && st.hostname],
    ['运行时长', uptime(st && st.uptime_seconds)],
    ['配置就绪', st && st.config_ready === false ? '否' : '是'],
    ['CPU', cpu.total != null ? cpu.total + ' 核' : undefined],
    ['CPU 使用率', cpu.usage_percent != null ? cpu.usage_percent.toFixed(1) + '%' : undefined],
    ['内存', mem.total_mb != null
      ? (mem.used_mb != null ? mb(mem.total_mb - mem.used_mb) + ' 可用 / ' : '') + mb(mem.total_mb)
      : undefined],
    ['根文件系统', disk.total_bytes
      ? bytes(disk.free_bytes) + ' 可用 / ' + bytes(disk.total_bytes) +
        (disk.used_ratio != null ? '（已用 ' + pct(disk.used_ratio) + '）' : '')
      : undefined],
  ];
}

function versionPairs(ver) {
  return [
    ['NFViS', ver && ver.nfvis], ['VPP', ver && ver.vpp], ['DPDK', ver && ver.dpdk],
    ['libvirt', ver && ver.libvirt], ['QEMU', ver && ver.qemu], ['Docker', ver && ver.docker],
    ['Ubuntu', ver && ver.ubuntu],
  ];
}

function renderSystem(st, ver) {
  $('host-line').textContent = st && st.hostname ? st.hostname : '';
  fill($('sys-list'), st && st.__err ? [['读取失败', st.__err]] : systemPairs(st));
  fill($('ver-list'), ver && ver.__err ? [['读取失败', ver.__err]] : versionPairs(ver));
}

// 系统域一级页（#/system）：状态与版本（与总览同源）+ **已生效配置**的系统段只读回显。
// 系统段里口令哈希这类敏感叶子由服务端脱敏后才发出来（界面拿不到原文，也不回显）。
// 改字段仍在「配置」页的系统段表单里做（只写候选、提交后生效）——本页不提供第二个写入口。
export function renderSystemPage(st, ver, sysCfg, dnsProxy, fw) {
  $('sysp-note').textContent = st && st.hostname ? '（' + st.hostname + '）' : '';
  const pairs = st && st.__err ? [['读取失败', st.__err]] : systemPairs(st);
  // 决策 #345：数据面 DNS 代理（只读两行，与 `show dns proxy` / GET /dns/proxy 同源）。
  // 与「宿主解析器」无关——这里读的是 vpp/交换机的 dns_proxy_servers；未配置如实「未配置」不编造。
  if (dnsProxy && !dnsProxy.__err) {
    const sw = Array.isArray(dnsProxy.switches) ? dnsProxy.switches : [];
    const perDomain = sw.length ? sw.map(x => x.name + '→' + ((x.servers || []).join('、') || '—')).join('；') : '—';
    pairs.push(['数据面 DNS 代理', dnsProxy.enabled ? '启用' : '未配置']);
    pairs.push(['全局上游', dnsProxy.enabled ? (list(dnsProxy.servers) || '（无）') : undefined]);
    pairs.push(['按域上游', dnsProxy.enabled ? perDomain : undefined]);
    // 决策 #439：内核数据面下服务端另发运行态块（服务落点数 + 计数）——只读一行；
    // VPP 数据面/未启用时该字段不发（omitempty），这里据其有无如实决定是否成行。
    if (dnsProxy.runtime) {
      const dm = Array.isArray(dnsProxy.runtime.domains) ? dnsProxy.runtime.domains.length : 0;
      pairs.push(['运行态', '服务中 ' + dm + ' 个落点 / 已应答 ' + dash(dnsProxy.runtime.answered) +
        ' / SERVFAIL ' + dash(dnsProxy.runtime.servfail)]);
    }
  }
  fill($('sysp-list'), pairs);
  fill($('sysp-ver'), ver && ver.__err ? [['读取失败', ver.__err]] : versionPairs(ver));
  $('sysp-cfg').textContent = sysCfg && sysCfg.__err
    ? '读取失败：' + sysCfg.__err
    : JSON.stringify(sysCfg || {}, null, 2);
  renderFirewallCard(fw);
}

// 主机防火墙只读卡（决策 #388）：默认策略 / 规则表（含逐规则命中计数）/ 保留项 / 计数口径，
// 与 `show system firewall` 和 `GET /system/firewall` 同源。本页不加写控件——写路径走
// 「配置」页与命令行（非本地串口会话的防火墙变更须用 commit confirmed 提交）。
// 读数取不到时如实显示「未收敛（原因）」或「读取失败」，不编造（计数缺席显示 -）。
function renderFirewallCard(fw) {
  fw = fw || {};
  const failed = !!fw.__err;
  $('fw-note').textContent = failed ? '（读取失败：' + fw.__err + '）' : '';
  fill($('fw-list'), failed ? [] : [
    ['状态', fw.enabled ? '已配置' : '未配置（管理口入向不做过滤）'],
    ['默认策略', fw.default_policy === 'drop' ? 'drop（白名单模式）' : 'accept（缺省放行）'],
    ['管理口', fw.mgmt_interface],
    ['下发状态', fw.applied ? '已收敛' : ('未收敛（' + (fw.error || '原因未给出') + '）')],
  ]);
  const tbody = $('fw-table').querySelector('tbody');
  tbody.textContent = '';
  const rules = failed ? [] : (Array.isArray(fw.rules) ? fw.rules : []);
  if (!rules.length) {
    tbody.appendChild(el('tr', {}, [el('td', { colspan: '6', class: 'muted',
      text: failed ? '读取失败' : (fw.enabled ? '（无规则：仅默认策略与保留项生效）' : '（未配置）') })]));
  } else {
    rules.forEach((r) => tbody.appendChild(el('tr', {}, [
      el('td', { text: String(dash(r.seq)) }),
      el('td', { text: String(dash(r.action)) }),
      el('td', { text: String(dash(r.source)) }),
      el('td', { text: String(dash(r.protocol)) }),
      el('td', { text: String(dash(r.port)) }),
      el('td', { text: r.packets !== undefined ? (r.packets + ' / ' + r.bytes) : '-' }),
    ])));
  }
  const reserved = Array.isArray(fw.reserved) ? fw.reserved : [];
  $('fw-reserved').textContent = failed ? ''
    : ('保留项（用户规则不可覆盖）：' + (reserved.length ? reserved.join('；') : '（未提供）'));
  $('fw-counters').textContent = failed ? '' : (fw.counters_note || '');
}


// 数据面卡。
// `dataplane` 是**装配事实**（本进程实际装了哪套数据面实现）：kernel 下 VPP 整组读数
// 都不适用——先如实点明当前数据面，其余行按内核口径呈现，不显示「未连接」（那会被读成
// 「VPP 该起来却没起来」，把人引向一条无效路径）；读数取不到一律显示「—」。
function renderVPP(vpp) {
  const l = $('vpp-list');
  if (vpp && vpp.__err) {
    fill(l, [['读取失败', vpp.__err]]);
    return;
  }
  const dp = vpp && vpp.dataplane;
  if (dp === 'kernel') {
    fill(l, [
      ['当前数据面', 'Linux 内核网络（未使用 VPP）'],
      ['版本', vpp && vpp.version],
      ['连接状态', '不适用（未使用 VPP）'],
      ['说明', vpp && vpp.last_error],
    ]);
    return;
  }
  fill(l, [
    ['当前数据面', dp === 'vpp' ? 'VPP' : undefined],
    ['版本', vpp && vpp.version],
    ['连接状态', vpp && vpp.connected === true ? '已连接' : (vpp && vpp.connected === false ? '未连接' : undefined)],
    ['待重启生效', vpp && (vpp.pending_restart ? '是' : '否')],
    ['线程数', vpp && Array.isArray(vpp.threads) ? vpp.threads.length : undefined],
    ['内存', vpp && vpp.memory ? bytes(vpp.memory.used) + ' / ' + bytes(vpp.memory.total) : undefined],
    ['buffer 池', vpp && Array.isArray(vpp.buffers) && vpp.buffers.length
      ? vpp.buffers.map((b) => b.name + '：用 ' + b.used + ' / 可用 ' + b.available).join('；')
      : (vpp && vpp.buffers_source ? '不可用（' + vpp.buffers_source + '）' : undefined)],
  ]);
}

// 资源池卡（大页池 + 隔离核分配）。
// 决策 #480：首表大页数字取**内核实况**，与下方「大页池」段（show system hugepages）同源——
// 「已分配（在用）」= 内核在用页数（含数据面固定占用），「可分配（VNF）」= 内核空闲页数（VNF 实际可起页数）；
// 故 #403 的「两处口径不同、数字不必相等」说明不再成立（两处现同源）。
export function renderPools(pools) {
  const p = $('pools');
  p.textContent = '';
  if (pools && pools.__err) {
    p.appendChild(el('p', { class: 'error', text: pools.__err }));
  } else {
    const hp = (pools && pools.hugepages) || [];
    p.appendChild(el('table', {}, [
      el('thead', {}, [el('tr', {}, ['页大小', '声明', '总数（内核实际）', '已分配（在用）', '可分配（VNF）'].map((h) => el('th', { text: h })))]),
      el('tbody', {}, hp.length ? hp.map((h) => el('tr', {}, [
        el('td', { text: String(dash(h.page_size)) }),
        el('td', { text: String(dash(h.count)) }),
        el('td', { text: hpNum(h.total) }),
        el('td', { text: hpNum(h.allocated) }),
        el('td', { text: hpNum(h.free) }),
      ])) : [el('tr', {}, [el('td', { colspan: '5', class: 'muted', text: '（无）' })])]),
    ]));
    p.appendChild(el('p', { class: 'muted small', text: '大页数字取自内核实况，与下方「大页池」段同源：' +
      '已分配（在用）= 内核在用页数（含数据面固定占用）；可分配（VNF）= 内核空闲页数（VNF 实际可起页数）；取不到内核值显示「取不到」。' }));
    const cpu = (pools && pools.cpu) || {};
    fill(p.appendChild(el('dl', { class: 'kv' })), [
      ['隔离核', list(cpu.isolated_cores)],
      ['VPP 保留核', list(cpu.vpp_reserved)],
      ['空闲核', list(cpu.free)],
      ['已分配', Array.isArray(cpu.allocated) && cpu.allocated.length
        ? cpu.allocated.map((a) => (a.vnf || '?') + '→' + list(a.cores)).join('；')
        : undefined],
    ]);
  }
}

// 大页池卡（决策 #329）：三方数字（声明/内核实际/在用）+ 可回收，与 `show system hugepages`
// 和 `GET /system/hugepages` 同源。回收只收「实际高于声明且空闲」的多余页（在用页一律不动），
// 且**不改声明值**——回收按钮按既有门禁 wbtn/data-write 收敛（super-user 级，非 super 隐藏）。
function hpMsg(text, isErr) {
  const p = $('hp-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

const hpNum = (v) => (v === undefined || v === null || v < 0 ? '取不到' : String(v));
const hpDeclared = (v) => (v === undefined || v === null || v <= 0 ? '未声明' : String(v));
const hpState = (s) => ({
  ok: '一致',
  under: '声明未全部生效（需重启分配）',
  surplus: '有可回收的空闲多余页',
  in_use: '多余页在用（回收不了/只能回收一部分）',
  orphan: '有无主占用页（产品侧不可回收）',
  unmanaged: '未声明（不托管）',
  unreadable: '取不到内核实际值',
}[s] || dash(s));

export function renderHugepagePools(hp) {
  const tbody = $('hp-table').querySelector('tbody');
  tbody.textContent = '';
  const ok = hp && !hp.__err;
  $('hp-note').textContent = ok ? '' : '（读取失败：' + (hp ? hp.__err : '未取到数据') + '）';
  const pools = ok ? rowsOf(hp.pools) : [];
  if (!pools.length) {
    tbody.appendChild(el('tr', {}, [el('td', { colspan: '10', class: 'muted', text: ok ? '（无）' : '读取失败' })]));
    hpDataplaneNote(pools);
    return;
  }
  pools.forEach((p) => {
    const tr = el('tr');
    [p.page_size, hpDeclared(p.declared), hpNum(p.actual), hpNum(p.in_use), hpNum(p.held),
      hpNum(p.held_by_dataplane), hpNum(p.orphan), hpNum(p.free), p.reclaimable, hpState(p.state),
    ].forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    tbody.appendChild(tr);
  });
  hpDataplaneNote(pools);
}

// 1G 池的数据面固定占用说明（决策 #353；归属措辞由 #460 更正）：仅当**实测**
// held_by_dataplane >= 1 时显示，与 `show system hugepages` 的说明行同口径。
// 归属口径（#460 实测）：该 1G 页是 **DPDK EAL 段**（memfd:seg_0-0），不是 VPP 主堆
// （主堆走 2M 页）；页数口径不变（数据面固定占 1 页、无配置键可释放 ⇒ VNF 可用 = 空闲页数）。
// 取不到（-1）或非 1G 池时不显示，不编造。
function hpDataplaneNote(pools) {
  const box = $('hp-dp-note');
  const p1 = pools.find((p) => p && p.page_size === '1G') || {};
  const dp = p1.held_by_dataplane;
  if (!(typeof dp === 'number' && dp >= 1)) {
    box.hidden = true;
    box.textContent = '';
    return;
  }
  box.hidden = false;
  box.textContent = '数据面（VPP）固定占用 ' + dp + ' 页（实测；不可释放）——该页实测归属为 DPDK EAL 段' +
    '（memfd:seg_0-0，按最大页优先取页），不是 VPP 主堆（主堆按 main-heap-page-size 落在 2M 页）；' +
    'VNF 可起页数 = 空闲页数（' + hpNum(p1.free) + '）。';
}

export async function hpReclaim() {
  const ok = await uiConfirm('回收空闲的多余大页', {
    tier: 'mid',
    bullets: [
      '只回收「内核实际高于声明、且**空闲**」的多余大页；**在用页一律不动**（不会抽走 VPP/VNF 正在用的大页）。',
      '⚠️ **无主占用页不在回收范围**：这类页多为被进程预留（reserve）但未使用的大页（如数据面 DPDK 预留），不在空闲链表上，写 nr_hugepages 释放不了——需从预留者一侧释放。',
      '收敛目标 = 已声明值；本动作**不改变**声明值（改声明在「配置」页的 resource-pools，需重启生效）。',
      '写后会回读内核实际值确认；无变化即如实报「无可回收的空闲多余页」，被在用页挡住会如实报出谁在占用。',
      '可回退：把声明值调回去并重启，或重启主机按引导基线重新分配。',
    ],
    cli: 'request system hugepages reclaim',
  });
  if (!ok) return;
  hpMsg('回收大页池：执行中…', false);
  $('hp-out').hidden = true;
  try {
    const r = await api('/system/hugepages:reclaim', { method: 'POST' });
    hpMsg('已执行回收（结果见下表刷新）。', false);
    if (r) {
      $('hp-out').hidden = false;
      $('hp-out').textContent = typeof r === 'string' ? r : JSON.stringify(r, null, 2);
    }
  } catch (e) {
    hpMsg('回收大页池失败：' + apiErrText(e, '请在命令行执行 request system hugepages reclaim'), true);
  }
  await reload().catch(() => {});
}

// 接口卡：列表端点只有配置字段（名称/说明/MTU 等），逐口统计在详情端点上（见 loadInterfaceStats）。
// 行可点进详情页（#/system/interfaces/:name）——驱动接管（DPDK）与 VF 数量都在那一页。
//
// 已知缺口（决策 #431）：CLI `show interfaces physical` 在内核数据面下会把「已声明却没进
// 数据面」的口在备注列**点名原因**（仍绑 vfio-pci / 被 networkd 持有为 down），该文本走 CLI
// 结构化输出的 `source` 字段；而 Web 取数的 REST `GET /interfaces`（`Interface` schema）**没有**
// 这个字段（后端按口径未改契约），故本页**拿不到该点名**——不自己编一个近似值，如实留白
// （`taken_over` 只说明「在不在数据面」，说不出「为什么不在」）。将来契约补字段再同源渲染。
export function renderInterfaces(ifaces, ifaceRows) {
  const tbody = $('iface-table').querySelector('tbody');
  tbody.textContent = '';
  if (!ifaceRows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '7', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
  }
  ifaceRows.forEach((r) => {
    const tr = rowClickable(el('tr'), '#/system/interfaces/' + encodeURIComponent(r.cfg.name));
    [r.cfg.name, r.cfg.description, r.cfg.mtu,
      r.stat ? r.stat.rx_packets : undefined, r.stat ? r.stat.tx_packets : undefined,
      r.stat ? r.stat.rx_errors : undefined, r.stat ? r.stat.tx_drops : undefined,
    ].forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    tbody.appendChild(tr);
  });
  const cfgOnly = (ifaces || []).length > ifaceRows.length;
  $('iface-note').textContent = cfgOnly
    ? '（共 ' + ifaces.length + ' 个接口，此处只列前 ' + MAX_IFACE_DETAIL + ' 个）'
    : '';
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsOverview = {
  'overview': {
    render(d) {
      pageWarn(d);
      renderSystem(d['/system/status'], d['/system/version']);
      renderVPP(d['/vpp/status']);
      renderAlarms(rowsOf(d['/alarms']));
      renderEvents();
    }
  },
};
