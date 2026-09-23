// NFViS 控制台前端。
//
// 无外部依赖、无构建步骤：改完直接刷新页面即可，产物随二进制内嵌。
// 取数一律走同源 REST 接口并带 Bearer token。
//
// 页面按**路由**组织：路由表在 routes.json（页面地址 `#/…`，与 Go 守护共用同一份真源），
// 路由与切页由 router.js 负责；本文件只提供「每个 view 怎么渲染」——VIEWS 注册表按 routes.json
// 的 view 名索引，每页只渲染自己那块 DOM、只取自己声明的端点（由 router 调 softLoad 取齐）。
//
// 数据来源（只用**对外**的 REST 端点；`/cli/execute` 在契约里标着"仅 nfvis-cli 使用、
// 不承诺第三方兼容"，前端不去碰它，也不去解析终端文本）：
//   /system/status           —— 主机名、运行时长、配置就绪、CPU/内存/磁盘
//   /system/version          —— 各组件版本
//   /resource-pools          —— 大页池（按页大小）与隔离核分配
//   /vpp/status              —— 数据面版本/连接/待重启/线程/buffer/内存
//   /interfaces(/<name>)     —— 接口配置 + 逐口收发计数
//   /virtual-machine-functions、/container-functions、/alarms
//   /events                  —— 事件推送（SSE）
//
// 关于实时刷新：浏览器的 EventSource **无法自定义请求头**，而 /events 需要
// Authorization，故这里用 fetch + 流式读取手工解析 SSE 帧（同样走头部传 token，
// 不把 token 放进 URL——URL 会进日志与浏览器历史）。流断了就按当前页声明的节奏轮询。

const API = location.pathname.replace(/\/ui\/.*$/, '') || '/api/v1';
const TOKEN_KEY = 'nfvis.token';
const USER_KEY = 'nfvis.user';
const MAX_EVENTS = 20;
const MAX_IFACE_DETAIL = 12;

let token = sessionStorage.getItem(TOKEN_KEY) || '';
let pollTimer = null;
let pollRoute = null;      // 当前路由（轮询节奏取它声明的 poll）
let pollPath = '';         // 已武装的路由地址（同一页的重复渲染不重置计时器）
let pollFallback = false;  // 只在实时通道断了之后才轮询
let streamAbort = null;
let reloadTimer = null;
let events = [];

const $ = (id) => document.getElementById(id);

// ---------- 小工具 ----------

// 全部用 textContent 落值（不用 innerHTML 拼数据），服务端返回的字符串因此不会变成标记。
function el(tag, attrs, children) {
  const node = document.createElement(tag);
  for (const k in (attrs || {})) {
    if (k === 'text') node.textContent = attrs[k];
    else if (k === 'class') node.className = attrs[k];
    else node.setAttribute(k, attrs[k]);
  }
  (children || []).forEach((c) => node.appendChild(c));
  return node;
}

const dash = (v) => (v === undefined || v === null || v === '' ? '—' : v);

function fill(dl, pairs) {
  dl.textContent = '';
  pairs.forEach(([k, v]) => {
    dl.appendChild(el('dt', { text: k }));
    dl.appendChild(el('dd', { text: String(dash(v)) }));
  });
}

function table(tbody, cols, rows) {
  tbody.textContent = '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: String(cols), class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((cells) => {
    const tr = el('tr');
    cells.forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    tbody.appendChild(tr);
  });
}

function uptime(sec) {
  if (sec === undefined || sec === null) return undefined;
  const s = Math.floor(Number(sec));
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return (d ? d + ' 天 ' : '') + h + ' 小时 ' + m + ' 分';
}

function bytes(n) {
  if (n === undefined || n === null) return undefined;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}

function pct(ratio) {
  if (ratio === undefined || ratio === null) return undefined;
  return (Number(ratio) * 100).toFixed(1) + '%';
}

function mb(n) {
  if (n === undefined || n === null) return undefined;
  return Number(n) >= 1024 ? (Number(n) / 1024).toFixed(1) + ' GB' : n + ' MB';
}

function fmtTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  return isNaN(d) ? String(ts) : d.toLocaleString();
}

function list(v) {
  if (v === undefined || v === null) return undefined;
  return Array.isArray(v) ? (v.length ? v.join(',') : '—') : String(v);
}

// ---------- 取数 ----------

async function api(path, opts) {
  const o = opts || {};
  o.headers = Object.assign({ Authorization: 'Bearer ' + token }, o.headers || {});
  const res = await fetch(API + path, o);
  if (res.status === 401) {
    // token 失效（过期/被吊销/服务端重启）：回到登录视图，别让页面停在半截数据上。
    signOut('登录状态已失效，请重新登录。');
    throw new Error('未认证');
  }
  if (!res.ok) {
    let msg = 'HTTP ' + res.status;
    try {
      const body = await res.json();
      if (body && body.message) msg = body.message;
    } catch (e) { /* 非 JSON 错误体：保留状态码 */ }
    throw new Error(msg);
  }
  return res.status === 204 ? null : res.json();
}

// 单个端点失败不该拖垮整页：各自降级为"读取失败"。
const soft = (p) => p.catch((e) => ({ __err: e.message }));

// soft() 失败时给的是 {__err} 而不是数组——数组类渲染一律先过 rowsOf()，否则 .filter/.map 抛错
// （浏览器验收抓到的旧缺陷：token 失效时整页报 JS 错，而不是干净地提示）。
const rowsOf = (v) => (Array.isArray(v) ? v : []);

// 页面级取数失败提示：把失败的端点列出来（不静默），全部成功时清掉提示。
// 只清自己写的那条——路由的「页面不存在」提示要留着（否则一重渲染就被抹掉）。
let pageWarnActive = false;
function pageWarn(d) {
  const bad = Object.entries(d || {})
    .filter(([, v]) => v && v.__err)
    .map(([k, v]) => k + '（' + v.__err + '）');
  const box = $('global-error');
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
async function loadVSwitchStats(vss) {
  const head = vss.slice(0, MAX_IFACE_DETAIL);
  const details = await Promise.all(head.map((v) => api('/virtual-switches/' + encodeURIComponent(v.name)).catch(() => null)));
  return head.map((v, n) => ({ cfg: v, stat: (details[n] || {}).statistics || null }));
}

// 网络对象：一次拉齐只读视图（每个端点各自降级，缺一个不影响其余）。
// `pre` 是路由表声明端点的预取结果（softLoad 已取齐）：命中就直接归位，不重复请求；
// 未预取时（别处直接调用）逐条自取。
async function loadNetworkObjects(pre) {
  const pick = (path, fetchIt) => (pre && pre[path] !== undefined ? pre[path] : fetchIt());
  const [vrfs, acls, nat, bonds, lldp, qos, span] = await Promise.all([
    pick('/vrfs', () => soft(api('/vrfs'))),
    pick('/acls', () => soft(api('/acls'))),
    pick('/nat', () => soft(api('/nat'))),
    pick('/bonds', () => soft(api('/bonds'))),
    pick('/protocols/lldp/neighbors', () => soft(api('/protocols/lldp/neighbors'))),
    pick('/qos/policies', () => soft(api('/qos/policies'))),
    pick('/port-mirroring', () => soft(api('/port-mirroring'))),
  ]);
  return { vrfs, acls, nat, bonds, lldp, qos, span };
}

// 逐口取统计（列表端点只有配置字段；收发包数在 /interfaces/<name> 上）。
async function loadInterfaceStats(ifaces) {
  const head = ifaces.slice(0, MAX_IFACE_DETAIL);
  const details = await Promise.all(head.map((i) => api('/interfaces/' + encodeURIComponent(i.name)).catch(() => null)));
  return head.map((i, n) => ({ cfg: i, stat: (details[n] || {}).statistics || null }));
}

// ---------- 各卡渲染（每页只动自己那块 DOM）----------

// 系统卡（含组件版本）。数字全部取自 /system/status（与 CLI show system 同源）；
// 字段缺席（非 Linux 无宿主指标等）时对应行显示「—」，不编造。
function renderSystem(st, ver) {
  $('host-line').textContent = st && st.hostname ? st.hostname : '';

  const cpu = (st && st.cpu) || {};
  const mem = (st && st.memory) || {};
  const disk = (st && st.storage) || {};
  fill($('sys-list'), st && st.__err ? [['读取失败', st.__err]] : [
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
  ]);
  fill($('ver-list'), ver && ver.__err ? [['读取失败', ver.__err]] : [
    ['NFViS', ver && ver.nfvis], ['VPP', ver && ver.vpp], ['DPDK', ver && ver.dpdk],
    ['libvirt', ver && ver.libvirt], ['QEMU', ver && ver.qemu], ['Docker', ver && ver.docker],
    ['Ubuntu', ver && ver.ubuntu],
  ]);
}

// 数据面卡。
function renderVPP(vpp) {
  fill($('vpp-list'), vpp && vpp.__err ? [['读取失败', vpp.__err]] : [
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
function renderPools(pools) {
  const p = $('pools');
  p.textContent = '';
  if (pools && pools.__err) {
    p.appendChild(el('p', { class: 'error', text: pools.__err }));
  } else {
    const hp = (pools && pools.hugepages) || [];
    p.appendChild(el('table', {}, [
      el('thead', {}, [el('tr', {}, ['页大小', '总数', '已分配', '空闲'].map((h) => el('th', { text: h })))]),
      el('tbody', {}, hp.length ? hp.map((h) => el('tr', {}, [
        el('td', { text: String(dash(h.page_size)) }),
        el('td', { text: String(dash(h.total)) }),
        el('td', { text: String(dash(h.allocated)) }),
        el('td', { text: String(dash(h.free)) }),
      ])) : [el('tr', {}, [el('td', { colspan: '4', class: 'muted', text: '（无）' })])]),
    ]));
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

// 接口卡：列表端点只有配置字段（名称/说明/MTU 等），逐口统计在详情端点上（见 loadInterfaceStats）。
function renderInterfaces(ifaces, ifaceRows) {
  const cfgOnly = (ifaces || []).length > ifaceRows.length;
  table($('iface-table').querySelector('tbody'), 7, ifaceRows.map((r) => [
    r.cfg.name, r.cfg.description, r.cfg.mtu,
    r.stat ? r.stat.rx_packets : undefined, r.stat ? r.stat.tx_packets : undefined,
    r.stat ? r.stat.rx_errors : undefined, r.stat ? r.stat.tx_drops : undefined,
  ]));
  $('iface-note').textContent = cfgOnly
    ? '（共 ' + ifaces.length + ' 个接口，此处只列前 ' + MAX_IFACE_DETAIL + ' 个）'
    : '';
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
      el('div', { class: 'muted small', text: (a.source ? a.source + ' · ' : '') + fmtTime(a.raised_at) }),
    ])));
  }
}

// ---------- 页面视图（routes.json 的 view 名 → 渲染函数）----------
//
// 键名必须与 routes.json 的 view 一一对应（Go 守护按名字核对）。每页的取数范围也由路由表声明：
// router.js 按 route.endpoints 调 softLoad 取齐后传进来（`d` 是 { 端点: 数据 }，键就是
// routes.json 里的原文）；参数化路由（详情页）还会把 `params`（对象名）作为第二个入参传进来。
// 页面里另外要拉的**详情/动作类**端点（不在路由表的 endpoints 里，如容器日志、抓包导出）
// 由该页自己用 api() 拉——那些是"点了才看"的东西，不进页面取数范围。
//
// `leave` 可选：离开本页（换页或换对象）时由 router.js 调一次，用于收尾（详情页断开串口）。
export const VIEWS = {
  'overview': {
    render(d) {
      pageWarn(d);
      renderSystem(d['/system/status'], d['/system/version']);
      renderVPP(d['/vpp/status']);
      renderAlarms(rowsOf(d['/alarms']));
      renderEvents();
    },
  },
  'vms': {
    render(d) {
      pageWarn(d);
      renderVMRows(rowsOf(d['/virtual-machine-functions']));
    },
  },
  'vmDetail': {
    render(d, params) {
      pageWarn(d);
      renderVMDetail(d, params);
    },
    // 串口是有状态的（WebSocket + 一次性 ticket）：离开这一页就断开，别把连接留在后台。
    leave() { vmConsoleClose(); },
  },
  'containers': {
    render(d) { pageWarn(d); renderContainerRows(rowsOf(d['/container-functions'])); },
  },
  'containerDetail': {
    render(d, params) { pageWarn(d); renderContainerDetail(d, params); },
  },
  'images': {
    render(d) { pageWarn(d); renderImages(rowsOf(d['/images'])); },
  },
  'imageDetail': {
    render(d, params) { pageWarn(d); renderImageDetail(d['/images/{name}'], params); },
  },
  'network': {
    async render(d) { pageWarn(d); renderNetworkObjects(await loadNetworkObjects(d)); },
  },
  'vrfDetail': {
    render(d, params) { pageWarn(d); renderVrfDetail(d['/vrfs/{name}'], d['/vrfs/{name}/routes'], params); },
  },
  'aclDetail': {
    render(d, params) { pageWarn(d); renderAclDetail(d['/acls/{name}'], params); },
  },
  'bondDetail': {
    render(d, params) { pageWarn(d); renderBondDetail(d['/bonds/{name}'], params); },
  },
  'qosDetail': {
    render(d, params) { pageWarn(d); renderQosDetail(d['/qos/policies'], params); },
  },
  'spanDetail': {
    render(d, params) { pageWarn(d); renderSpanDetail(d['/port-mirroring'], params); },
  },
  'switches': {
    async render(d) {
      pageWarn(d);
      const vss = rowsOf(d['/virtual-switches']);
      renderVSwitches(vss, await loadVSwitchStats(vss));
    },
  },
  'pools': {
    render(d) { pageWarn(d); renderPools(d['/resource-pools']); },
  },
  'interfaces': {
    async render(d) {
      pageWarn(d);
      const ifaces = rowsOf(d['/interfaces']);
      renderInterfaces(ifaces, await loadInterfaceStats(ifaces));
    },
  },
  'config': {
    render(d) { return loadConfig(d['/configuration']); },
  },
  'ops': {
    render(d) { pageWarn(d); renderArchives(rowsOf(d['/system/backup']), rowsOf(d['/system/tech-support'])); },
  },
  'audit': {
    render(d) { pageWarn(d); renderAudit(d['/audit-logs?limit=50']); },
  },
  'diagnostics': {
    // 诊断页是动作面板：ping / traceroute / 清零 / 看服务端日志都按需执行（点按钮才拉），
    // 进页面不自动拉日志——大段文本不该跟着页面刷新反复下载。
    render() {},
  },
  'capture': {
    render(d) { pageWarn(d); renderCapture(d['/vpp/capture']); },
  },
};

function renderEvents() {
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

// ---------- 配置（candidate → 提交） ----------
//
// 写路径与 CLI 同一套语义：PUT /configuration/candidate 取锁并建立 candidate →
// 可选查看差异（GET /configuration/diff）→ 提交（POST /configuration/commit；
// 管理口地址/网关变更会返回 CONFIRM_REQUIRED，须走 commit confirmed）→
// DELETE candidate 结束会话（**提交后也释放**，避免把会话锁留给下一次 CLI 登录）。
//
// 编辑态以原始 JSON 文本为准（表单改动会先写回文本），提交前都会先保存。

let cfg = {
  committed: null, // {configuration, revision}
  editing: false,
};

const CFG_TEXT_ID = 'cfg-text';

// cfgForm 表单定义：把常用字段读写到 candidate 文档上（其余字段走原始 JSON）。
const cfgForm = {
  hostname: {
    label: '主机名',
    get: (c) => (c.system || {}).hostname || '',
    set: (c, v) => { if (!c.system) c.system = {}; if (v) c.system.hostname = v; else delete c.system.hostname; },
  },
  ntp: {
    label: 'NTP 服务器',
    get: (c) => ((c.system || {}).ntp || []).map((n) => n.server || ''),
    set: (c, vals) => {
      const list = vals.filter((v) => v !== '').map((v) => ({ server: v }));
      if (!c.system) c.system = {};
      if (list.length) c.system.ntp = list; else delete c.system.ntp;
    },
  },
  dns: {
    label: 'DNS 服务器',
    get: (c) => ((c.system || {}).dns_servers || []).slice(),
    set: (c, vals) => {
      const list = vals.filter((v) => v !== '');
      if (!c.system) c.system = {};
      if (list.length) c.system.dns_servers = list; else delete c.system.dns_servers;
    },
  },
};

function cfgMsg(text, isErr) {
  const n = $('cfg-msg');
  n.textContent = text || '';
  n.hidden = !text;
  n.className = isErr ? 'error small' : 'muted small';
}

function cfgSetEditing(on) {
  cfg.editing = on;
  $('cfg-read').hidden = on;
  $('cfg-edit').hidden = !on;
  $('cfg-diff').hidden = true;
  $('cfg-commit-confirmed-btn').hidden = true;
  $('cfg-confirm-btn').hidden = true;
  cfgMsg('', false);
}

function cfgCandidate() {
  return cfg.committed ? JSON.parse(JSON.stringify(cfg.committed.configuration || {})) : {};
}

function cfgText() {
  const t = $(CFG_TEXT_ID).value;
  if (!t.trim()) return {};
  return JSON.parse(t); // 语法错误由调用方提示
}

function cfgWriteText(c) {
  $(CFG_TEXT_ID).value = JSON.stringify(c, null, 2);
}

// 表单改动：先解析文本区（保留手工编辑），应用该字段，再写回文本区。
function cfgApplyForm(key, values) {
  let c;
  try {
    c = cfgText();
  } catch (e) {
    cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true);
    return;
  }
  cfgForm[key].set(c, values);
  cfgWriteText(c);
  cfgMsg('已写入 candidate（未保存）——点「保存到 candidate」提交到服务端。', false);
}

// 核列表解析/格式化（隔离核在 JSON 里是数组，表单里写成 "2-5,7" 更好用）。
function parseCores(text) {
  const out = [];
  (text || '').split(',').forEach((part) => {
    const t = part.trim();
    if (!t) return;
    const m = t.match(/^(\d+)\s*-\s*(\d+)$/);
    if (m) {
      for (let i = Number(m[1]); i <= Number(m[2]); i++) out.push(i);
    } else if (/^\d+$/.test(t)) {
      out.push(Number(t));
    }
  });
  return out;
}

function formatCores(list) {
  const nums = (list || []).slice().sort((a, b) => a - b);
  const parts = [];
  for (let i = 0; i < nums.length; ) {
    let j = i;
    while (j + 1 < nums.length && nums[j + 1] === nums[j] + 1) j++;
    parts.push(j > i ? nums[i] + '-' + nums[j] : String(nums[i]));
    i = j + 1;
  }
  return parts.join(',');
}

// 表单渲染：系统节 + 接口节（描述/MTU/启用）+ 资源池节（隔离核/两个大页池）。
// 全部走同一套"改字段 → 写回 JSON 文本 → 保存"的路径。
function cfgRenderForms(c) {
  const box = $('cfg-forms');
  box.textContent = '';
  let seq = 0;

  const addField = (parent, label, value, onchange, kind) => {
    const id = 'cfg-f-' + (seq++);
    parent.appendChild(el('label', { text: label, for: id })); // 关联 label ↔ input（无障碍/可测）
    const inp = el(kind === 'select' ? 'select' : 'input', kind === 'select' ? { id } : { type: kind || 'text', id });
    if (kind === 'select') {
      [['', '（未设置）'], ['true', '启用'], ['false', '禁用']].forEach(([v, t]) => {
        const opt = el('option', { value: v, text: t });
        if (String(value === undefined || value === null ? '' : value) === v) opt.setAttribute('selected', 'selected');
        inp.appendChild(opt);
      });
    } else {
      inp.value = value === undefined || value === null ? '' : value;
    }
    // 监听 input 而非 change：change 只在**用户输入导致的**失焦时触发，程序化赋值
    // （浏览器自动化、脚本回填）不置"值已改"标志、失焦也不发 change；input 两者都覆盖。
    inp.addEventListener('input', onchange);
    inp.addEventListener('change', onchange); // select 用 change
    parent.appendChild(inp);
    return inp;
  };

  // ---- 系统 ----
  const sys = el('fieldset', {});
  sys.appendChild(el('legend', { text: '系统' }));
  addField(sys, cfgForm.hostname.label, cfgForm.hostname.get(c), (e) => cfgApplyForm('hostname', e.target.value));
  const ntpVals = cfgForm.ntp.get(c);
  const ntpInputs = [0, 1].map((i) => addField(sys, cfgForm.ntp.label + ' ' + (i + 1), ntpVals[i],
    () => cfgApplyForm('ntp', ntpInputs.map((x) => x.value.trim()))));
  const dnsVals = cfgForm.dns.get(c);
  const dnsInputs = [0, 1].map((i) => addField(sys, cfgForm.dns.label + ' ' + (i + 1), dnsVals[i],
    () => cfgApplyForm('dns', dnsInputs.map((x) => x.value.trim()))));
  box.appendChild(sys);

  // ---- 接口（对每个已声明接口：描述 / MTU / 启用）----
  const ifs = Array.isArray(c.interfaces) ? c.interfaces : [];
  if (ifs.length) {
    const f = el('fieldset', {});
    f.appendChild(el('legend', { text: '接口' }));
    ifs.forEach((it) => {
      const sub = el('div', { class: 'cfg-sub' });
      sub.appendChild(el('div', { class: 'cfg-subname', text: it.name || '（未命名）' }));
      addField(sub, '描述', it.description, (e) => cfgApplyIface(it.name, 'description', e.target.value));
      addField(sub, 'MTU', it.mtu, (e) => cfgApplyIface(it.name, 'mtu', e.target.value === '' ? '' : Number(e.target.value)));
      addField(sub, '启用', it.enabled === undefined || it.enabled === null ? '' : String(it.enabled),
        (e) => cfgApplyIface(it.name, 'enabled', e.target.value === '' ? '' : e.target.value === 'true'), 'select');
      f.appendChild(sub);
    });
    box.appendChild(f);
  }

  // ---- 资源池（隔离核 + 1G/2M 大页数量）----
  const pools = c.resource_pools || {};
  const pf = el('fieldset', {});
  pf.appendChild(el('legend', { text: '资源池' }));
  addField(pf, '隔离核（如 2-5,7）', formatCores((pools.cpu || {}).isolated_cores),
    (e) => cfgApplyPools('isolated_cores', parseCores(e.target.value)));
  ['1G', '2M'].forEach((size) => {
    const hp = (pools.hugepages || []).find((h) => h.page_size === size) || {};
    addField(pf, '大页 ' + size + ' 数量', hp.count,
      (e) => cfgApplyPools('hugepages', { size, count: e.target.value === '' ? '' : Number(e.target.value) }));
  });
  box.appendChild(pf);
}

// 接口字段改写（按名字定位；名字来自当前 candidate 文本）。
function cfgApplyIface(name, key, value) {
  let c;
  try { c = cfgText(); } catch (e) { cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true); return; }
  const it = (c.interfaces || []).find((x) => x.name === name);
  if (!it) return;
  if (value === '') delete it[key]; else it[key] = value;
  cfgWriteText(c);
  cfgMsg('已写入 candidate（未保存）——点「保存到 candidate」提交到服务端。', false);
}

// 资源池字段改写：isolated_cores 直接替换；hugepages 按页大小定位（不存在则补一条）。
function cfgApplyPools(key, value) {
  let c;
  try { c = cfgText(); } catch (e) { cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true); return; }
  if (!c.resource_pools) c.resource_pools = {};
  if (key === 'isolated_cores') {
    if (!c.resource_pools.cpu) c.resource_pools.cpu = {};
    if (value.length) c.resource_pools.cpu.isolated_cores = value;
    else delete c.resource_pools.cpu.isolated_cores;
  } else {
    const list = Array.isArray(c.resource_pools.hugepages) ? c.resource_pools.hugepages : (c.resource_pools.hugepages = []);
    const found = list.find((h) => h.page_size === value.size);
    if (value.count === '') {
      c.resource_pools.hugepages = list.filter((h) => h.page_size !== value.size);
      if (!c.resource_pools.hugepages.length) delete c.resource_pools.hugepages;
    } else if (found) {
      found.count = value.count;
    } else {
      list.push({ page_size: value.size, count: value.count });
    }
  }
  cfgWriteText(c);
  cfgMsg('已写入 candidate（未保存）——点「保存到 candidate」提交到服务端。', false);
}

// 预校验（POST /configuration/check）：先保存，再让服务端跑与提交相同的全部校验。
async function cfgCheck() {
  if (!(await cfgSave())) return;
  try {
    const res = await api('/configuration/check', { method: 'POST' });
    if (res && res.ok) {
      cfgMsg('预校验通过（服务端已跑与提交相同的全部校验）。', false);
      return;
    }
    const errs = (res && res.errors) || [];
    cfgMsg('预校验发现 ' + errs.length + ' 个问题：' + errs.map((e) => e.path + '：' + e.message).join('；'), true);
  } catch (e) {
    cfgMsg('预校验失败：' + e.message, true);
  }
}

function cfgRenderRead() {
  const rev = cfg.committed ? cfg.committed.revision : undefined;
  fill($('cfg-summary'), [
    ['配置版本', rev != null ? 'revision ' + rev : undefined],
    ['状态', cfg.committed ? '已加载 committed 配置' : '未加载'],
  ]);
  $('cfg-json').textContent = cfg.committed
    ? JSON.stringify(cfg.committed.configuration || {}, null, 2) : '';
  $('cfg-note').textContent = rev != null ? '（committed revision ' + rev + '）' : '';
}

// 读取 committed 配置。`pre` 是路由表声明端点的预取结果（配置页由 softLoad 取齐后传进来，
// 不重复请求）；不传则自己拉（提交事件、结束编辑会话等处调用）。
async function loadConfig(pre) {
  const res = pre !== undefined ? pre : await api('/configuration');
  if (res && res.__err) {
    cfg.committed = null;
    $('cfg-note').textContent = '（读取失败：' + res.__err + '）';
    return false;
  }
  cfg.committed = res;
  cfgRenderRead();
  return true;
}

// 开始编辑：以 committed 为起点建立 candidate（PUT 取锁）。
// 读不到 committed 时**拒绝进入**——否则会把空配置当 candidate 提交（危险）。
async function cfgStartEdit() {
  try {
    if (!(await loadConfig()) || !cfg.committed) {
      cfgSetEditing(false);
      $('cfg-note').textContent = '（无法进入编辑：读不到当前配置，请先刷新）';
      return;
    }
    const base = cfgCandidate();
    await api('/configuration/candidate', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(base),
    });
    cfgWriteText(base);
    cfgRenderForms(base);
    cfgSetEditing(true);
    cfgMsg('已进入编辑会话（candidate = 当前 committed）。改完点「保存到 candidate」。', false);
  } catch (e) {
    cfgSetEditing(false);
    $('cfg-note').textContent = '（无法进入编辑：' + e.message + '）';
  }
}

async function cfgSave() {
  let c;
  try {
    c = cfgText();
  } catch (e) {
    cfgMsg('原始 JSON 语法错误：' + e.message, true);
    return false;
  }
  try {
    const res = await api('/configuration/candidate', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(c),
    });
    cfgMsg(res && res.dirty ? '已保存到 candidate（有未提交变更）。' : '已保存到 candidate（与 committed 一致）。', false);
    return true;
  } catch (e) {
    cfgMsg('保存失败：' + e.message, true);
    return false;
  }
}

async function cfgShowDiff() {
  try {
    const res = await fetch(API + '/configuration/diff', { headers: { Authorization: 'Bearer ' + token } });
    const text = await res.text();
    const pre = $('cfg-diff');
    pre.textContent = res.ok ? (text.trim() || '（candidate 与 committed 无差异）') : '读取差异失败：HTTP ' + res.status;
    pre.hidden = false;
  } catch (e) {
    cfgMsg('读取差异失败：' + e.message, true);
  }
}

// 结束编辑会话：DELETE candidate（discard）并释放锁。
async function cfgEndSession() {
  try {
    await api('/configuration/candidate', { method: 'DELETE' });
  } catch (e) { /* 会话可能已因超时释放：不阻塞退出 */ }
  cfgSetEditing(false);
  await loadConfig();
}

async function cfgCommit(confirmedMinutes) {
  if (!(await cfgSave())) return;
  const body = confirmedMinutes ? { confirmed_minutes: confirmedMinutes } : {};
  try {
    const res = await api('/configuration/commit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const warns = (res && res.warnings) || [];
    const parts = ['提交成功（revision ' + (res && res.revision) + '）'];
    if (res && res.confirmed_until) {
      parts.push('待确认：' + fmtTime(res.confirmed_until) + ' 前未点「确认在途提交」将自动回滚');
      $('cfg-confirm-btn').hidden = false;
      $('cfg-commit-confirmed-btn').hidden = true;
    }
    if (warns.length) parts.push('提示：' + warns.join('；'));
    cfgMsg(parts.join('；'), false);
    if (!(res && res.confirmed_until)) {
      await cfgEndSession(); // 已生效：释放会话锁，回到只读视图
      cfgMsg('提交成功（revision ' + (res && res.revision) + '）。' + (warns.length ? '提示：' + warns.join('；') : ''), false);
    }
  } catch (e) {
    // 服务端要求 commit confirmed 时（管理口地址/网关变更）揭示确认入口。
    // 注意：按 FR-CFG-012 原文，该强制**只约束 SSH 会话**——控制台（api 来源）改管理口
    // 当前不会被要求 confirmed（提交成功但带"注意连通性"警告，见下面的 warnings 展示）。
    // 这里保留处理是**纵深防御**：服务端一旦对 api 来源也要求确认，页面即已就绪。
    if (/必须.*commit confirmed|confirm/i.test(e.message)) {
      cfgMsg('该变更（管理口地址/网关）必须以 commit confirmed 提交：' + e.message, true);
      $('cfg-commit-confirmed-btn').hidden = false;
      return;
    }
    cfgMsg('提交失败：' + e.message, true);
  }
}

async function cfgConfirmPending() {
  try {
    await api('/configuration/commit:confirm', { method: 'POST' });
    await cfgEndSession();
    cfgMsg('已确认，提交生效。', false);
  } catch (e) {
    cfgMsg('确认失败：' + e.message, true);
  }
}

// ---------- 诊断（日志 + 连通性测试） ----------
//
// 与 CLI 同源同口径：ping 只覆盖数据面（VPP），**未通即失败**（0 发包/0 应答都算失败，
// 原始回显照原样展示以便排查）；日志取服务端尾部（与 show log system 同源）。
// 日志不随页面轮询刷新（按需点按钮），避免无谓的重复拉取。

// pingBody 组请求体：源地址留空则不传（服务端按目标自动选路）。
function pingBody(host, count) {
  const body = { host };
  if (count) body.count = count;
  const src = $('diag-source').value.trim();
  if (src) body.source = src;
  return body;
}

async function diagPing() {
  const host = $('diag-host').value.trim();
  const out = $('diag-out');
  out.hidden = false;
  if (!host) {
    out.textContent = '请填写目标地址。';
    return;
  }
  const count = Number($('diag-count').value) || 0;
  out.textContent = '执行中…';
  try {
    const res = await fetch(API + '/diagnostics/ping', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
      body: JSON.stringify(pingBody(host, count)),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      const detail = (body.detail || []).map((d) => d.message).join('\n');
      out.textContent = '失败：' + (body.message || ('HTTP ' + res.status)) + (detail ? '\n' + detail : '');
      return;
    }
    out.textContent = body.output || '（无输出）';
  } catch (e) {
    out.textContent = '失败：' + e.message;
  }
}

async function diagLogs() {
  const pre = $('diag-log');
  pre.hidden = false;
  pre.textContent = '读取中…';
  try {
    const res = await fetch(API + '/system/logs?last=100', { headers: { Authorization: 'Bearer ' + token } });
    pre.textContent = res.ok ? (await res.text() || '（日志为空）') : '读取失败：HTTP ' + res.status;
  } catch (e) {
    pre.textContent = '读取失败：' + e.message;
  }
}

// ---------- 实时通道与刷新 ----------

// router.js 要用本文件的 VIEWS 与取数助手，本文件要用它的 render/navigate。
// 按需动态 import：静态互相 import 会形成环，对两边的求值顺序有隐含要求。
const routerModule = () => import('./router.js');

// 重新渲染**当前路由**：SSE 事件到达、轮询、动作完成、点顶栏「刷新」都走这里
// （不再像以前那样一次刷新所有页面）。
export async function reload() {
  return (await routerModule()).render();
}

function setStream(text, cls) {
  const n = $('stream-state');
  n.textContent = text;
  n.className = 'pill ' + cls;
}

function scheduleReload() {
  // 事件可能成串到达（一次提交触发多条）：合并成一次刷新。
  if (reloadTimer) return;
  reloadTimer = setTimeout(() => {
    reloadTimer = null;
    reload().catch((e) => showGlobalError(e.message));
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

async function startStream() {
  stopStream();
  const ctl = new AbortController();
  streamAbort = ctl;
  try {
    const res = await fetch(API + '/events', {
      headers: { Authorization: 'Bearer ' + token, Accept: 'text/event-stream' },
      signal: ctl.signal,
    });
    if (res.status === 401) { signOut('登录状态已失效，请重新登录。'); return; }
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
          events.unshift({ type: ev.type, timestamp: ev.timestamp, summary: summaryOf(ev) });
          events = events.slice(0, MAX_EVENTS);
          renderEvents();
          // 配置提交事件：顺带刷新配置卡（编辑中不打扰——避免覆盖正在改的文本）。
          if (ev.type === 'config-committed' && !cfg.editing) {
            loadConfig().catch(() => {});
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

function stopStream() {
  if (streamAbort) { streamAbort.abort(); streamAbort = null; }
}

// 轮询是实时通道不可用时的兜底：节奏取**当前路由声明的 poll**（毫秒，0 = 该页不轮询）。
function armPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  const ms = (pollRoute && pollRoute.poll) || 0;
  if (!pollFallback || !ms) return;
  pollTimer = setInterval(() => {
    reload().catch((e) => showGlobalError(e.message));
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

function startPolling() {
  pollFallback = true;
  armPolling();
}

function stopPolling() {
  pollFallback = false;
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
}

export function showGlobalError(msg) {
  const n = $('global-error');
  n.textContent = msg;
  n.hidden = false;
}

// ---------- 登录 / 退出 ----------

function showLogin(msg) {
  $('main-view').hidden = true;
  $('login-view').hidden = false;
  const e = $('login-error');
  e.textContent = msg || '';
  e.hidden = !msg;
  $('password').value = '';
  $('username').focus();
}

async function enterApp(user) {
  $('login-view').hidden = true;
  $('main-view').hidden = false;
  $('global-error').hidden = true;
  $('user-line').textContent = user ? user.name + '（' + user.class + '）' : '';
  cfgSetEditing(false);
  try {
    // 路由表 → 渲染当前 hash 那一页（配置页的数据由 config 视图自己拉）。
    const router = await routerModule();
    await router.loadRoutes();
    await router.start();
  } catch (e) {
    // 拿不到路由表时如实说明，别谎报"登录失效"把人踢回登录页。
    showGlobalError('页面加载失败：' + e.message);
  }
  startStream();
}

function signOut(msg) {
  stopStream();
  stopPolling();
  // 串口是有状态的（WebSocket）：退出登录必须断开，别把连接留在后台。
  vmConsoleClose();
  token = '';
  events = [];
  cfg = { committed: null, editing: false };
  cfgSetEditing(false);
  sessionStorage.removeItem(TOKEN_KEY);
  sessionStorage.removeItem(USER_KEY);
  showLogin(msg);
}

async function doLogin(ev) {
  ev.preventDefault();
  const btn = $('login-btn');
  btn.disabled = true;
  try {
    const res = await fetch(API + '/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: $('username').value, password: $('password').value }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      showLogin(body.message || ('登录失败（HTTP ' + res.status + '）'));
      return;
    }
    token = body.token;
    sessionStorage.setItem(TOKEN_KEY, token);
    sessionStorage.setItem(USER_KEY, JSON.stringify(body.user || {}));
    await enterApp(body.user);
  } catch (e) {
    showLogin('无法连接服务：' + e.message);
  } finally {
    btn.disabled = false;
  }
}

async function doLogout() {
  // 先尽力吊销服务端 token（失败也不影响本地退出——本地清掉后即无凭据可用）。
  try { await api('/logout', { method: 'POST' }); } catch (e) { /* 忽略 */ }
  signOut('');
}

// ---------- 虚拟机：列表行（点行进详情）+ 生命周期动作 ----------

// 状态 → 允许的动作（与 CLI 同口径：运行中可 stop/restart，关机态可 start）。
// 列表页与详情页共用这份判定，两边不会分叉。
const VM_ACTIONS = [
  { key: 'start', label: '启动', states: ['shutoff', 'crashed', '-'] },
  { key: 'stop', label: '停止', states: ['running', 'paused'] },
  { key: 'restart', label: '重启', states: ['running', 'paused'] },
];

// 行内按钮：拦掉冒泡——否则点动作会连带触发行点击（跳进详情页）。动作与拦截是两个监听器，
// 都在按钮自身上，故互不影响。
function rowButton(btn) {
  btn.addEventListener('click', (ev) => ev.stopPropagation());
  return btn;
}

// 列表行的行点击：进该对象的详情页（地址即 `#/…/<name>`，可分享、可刷新保持）。
function rowClickable(tr, path) {
  tr.className = 'row-link';
  tr.addEventListener('click', () => goDetail(path));
  return tr;
}

// 详情页跳转：路由模块按需动态 import（与 reload 同一路，避免静态互相 import 成环）。
async function goDetail(path) {
  (await routerModule()).navigate(path);
}

function renderVMRows(vms) {
  const tbody = $('vm-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = vms || [];
  $('vm-note').textContent = rows.length ? '（' + rows.length + ' 台；点行进详情，动作按钮按当前状态启用）' : '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((v) => {
    const tr = rowClickable(el('tr'), '#/compute/vms/' + encodeURIComponent(v.name));
    [v.name, v.state, v.vcpu ? v.vcpu.count : undefined,
      v.memory ? mb(v.memory.size_mb) : undefined, v.image].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    VM_ACTIONS.forEach((a) => {
      const btn = rowButton(el('button', { type: 'button', class: 'ghost small', text: a.label }));
      btn.disabled = a.states.indexOf(String(v.state)) < 0;
      btn.addEventListener('click', () => vmAction(v.name, a.key, a.label, opsMsg));
      cell.appendChild(btn);
    });
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 生命周期动作。`msg` 是回显去处：列表页用运维页的提示区（历史行为），
// 详情页用自己的提示区（vmDetailMsg）——同一套动作、同一套状态判定，只是回显位置不同。
async function vmAction(name, action, label, msg) {
  const say = msg || opsMsg;
  if (!window.confirm(label + '虚拟机 ' + name + '？运行中的业务会中断。')) return;
  say(label + ' ' + name + '：执行中…', false);
  try {
    await api('/virtual-machine-functions/' + encodeURIComponent(name) + ':' + action, { method: 'POST' });
    say(label + ' ' + name + '：已受理。', false);
  } catch (e) {
    say(label + ' ' + name + ' 失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// vhost-user 口计数（详情端点附带的 statistics；运行态未接入时如实说"未取到计数"）。
// 渲染到 `id` 指定的 pre 上（详情页的概览 Tab）。
function renderVMStats(vms, stats, id) {
  const pre = $(id);
  const rows = [];
  (vms || []).forEach((v) => {
    (stats[v.name] || []).forEach((s) => {
      rows.push(v.name + ' ' + s.vnic + '（' + s.interface + '）: ' +
        (s.available ? 'rx ' + dash(s.rx_packets) + ' / tx ' + dash(s.tx_packets) +
          ' 包，' + dash(bytes(s.rx_bytes)) + ' / ' + dash(bytes(s.tx_bytes)) : '未取到计数（VM 未运行或口未建立）'));
    });
  });
  if (!rows.length) {
    pre.hidden = true;
    return;
  }
  pre.hidden = false;
  pre.textContent = 'vhost-user 口计数：\n' + rows.join('\n');
}

// ---------- VM 详情页（#/compute/vms/:name）----------
//
// 一页一对象：对象头 + 四个 Tab（概览 / 接口 / 快照 / 串口）。Tab 是**页内状态**，不进 hash
// （对象级深链已够用；Tab 进 URL 会把地址拖长、也让"这个链接分享出去看到什么"变模糊）。
// 快照与串口**复用既有实现**（vmSnapLoad/vmSnapCreate/vmSnapAct、vmConsoleOpen/vmConsoleSend/
// vmConsoleClose），只是容器从列表页的行内面板搬到这里的 Tab 面板——一套代码，行为不会分叉。

const VM_TABS = ['overview', 'ifaces', 'snapshots', 'console'];
let vmTab = 'overview';
let vmDetailName = '';

// 切 Tab：只动面板的 hidden 与 Tab 条的选中态（不碰 hash，也不重新取数）。
// `focusBtn` 只在**用户点 Tab** 时为真——重渲染（轮询/事件）里抢焦点会把光标从正在输入的地方挪走。
function vmTabShow(tab, focusBtn) {
  if (VM_TABS.indexOf(tab) < 0) tab = 'overview';
  vmTab = tab;
  VM_TABS.forEach((t) => { $('vm-tab-' + t).hidden = t !== tab; });
  const bar = $('vmd-tabs');
  for (const b of bar.querySelectorAll('button')) {
    if (b.dataset.tab === tab) b.setAttribute('aria-selected', 'true');
    else b.removeAttribute('aria-selected');
  }
  if (focusBtn && tab === 'console' && !termWS) $('vm-console-connect').focus();
}

function vmDetailMsg(text, isErr) {
  const p = $('vmd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function vmDetailHead(vm) {
  return [
    ['状态', vm.state],
    ['镜像', vm.image],
    ['vCPU', vm.vcpu ? vm.vcpu.count : undefined],
    ['内存', vm.memory ? mb(vm.memory.size_mb) : undefined],
  ];
}

function vmDetailInfo(vm) {
  const vcpu = vm.vcpu || {};
  const mem = vm.memory || {};
  const disks = vm.disks || [];
  return [
    ['名称', vm.name],
    ['描述', vm.description],
    ['vCPU 绑定', vcpu.pin === undefined || vcpu.pin === null ? undefined : (vcpu.pin ? '是' : '否')],
    ['绑定核', list(vcpu.cores_assigned)],
    ['内存后端', mem.backing === 'normal' ? '普通内存' : (mem.backing ? '大页' : undefined)],
    ['大页大小', mem.hugepage_size],
    ['NUMA 节点', mem.numa_node],
    ['附加磁盘', disks.length ? disks.map((d) => (d.name || '?') +
      (d.size_gb ? '（' + d.size_gb + ' GB）' : '') +
      (d.image ? '（来自 ' + d.image + '）' : '')).join('；') : undefined],
    ['vNIC', (vm.interfaces || []).length],
    ['串口', vm.serial_console === false ? '未启用' : '已启用'],
    ['开机自启', vm.autostart === undefined || vm.autostart === null ? undefined : (vm.autostart ? '是' : '否')],
  ];
}

// 概览 Tab 的生命周期按钮：与列表页同一份 VM_ACTIONS 判定，回显走本页的提示区。
function vmDetailActions(vm) {
  const box = $('vmd-actions');
  box.textContent = '';
  if (!vm) return;
  VM_ACTIONS.forEach((a) => {
    const btn = el('button', { type: 'button', text: a.label });
    btn.disabled = a.states.indexOf(String(vm.state)) < 0;
    btn.addEventListener('click', () => vmAction(vm.name, a.key, a.label, vmDetailMsg));
    box.appendChild(btn);
  });
}

// 接口 Tab：该 VM 的 vNIC（类型/虚拟交换机/MAC/VLAN/状态 + 逐口包计数）。
function vmDetailIfaces(vm) {
  const tbody = $('vmd-iface-table').querySelector('tbody');
  tbody.textContent = '';
  const list0 = (vm && vm.interfaces) || [];
  const stats = (vm && vm.statistics) || [];
  if (!list0.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '8', class: 'muted', text: '（无 vNIC）' }));
    tbody.appendChild(tr);
    return;
  }
  list0.forEach((i) => {
    const s = stats.find((x) => x.vnic === i.name);
    const live = s && s.available;
    const tr = el('tr');
    [i.name, i.type, i.virtual_switch, i.mac, i.vlan, i.state,
      live ? s.rx_packets : undefined, live ? s.tx_packets : undefined].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    tbody.appendChild(tr);
  });
}

function renderVMDetail(d, params) {
  const name = (params && params.name) || '';
  const vm = d['/virtual-machine-functions/{name}'];
  const snaps = d['/virtual-machine-functions/{name}/snapshots'];
  const ok = vm && !vm.__err;
  // 换了对象才清本页提示——同一对象的轮询重渲染要留住动作回显（"已受理"刚写完就被抹掉
  // 就等于没回显）。
  const changed = vmDetailName !== name;
  vmDetailName = name;
  if (changed) vmDetailMsg('', false);

  $('vmd-name').textContent = name;
  fill($('vmd-head'), ok ? vmDetailHead(vm) : [['读取失败', vm ? vm.__err : '未取到数据']]);
  fill($('vmd-info'), ok ? vmDetailInfo(vm) : []);
  vmDetailActions(ok ? vm : null);
  vmDetailIfaces(ok ? vm : null);
  // 快照：名字变了才清输入框与提示（轮询重渲染不该把正在输入的快照名抹掉）；
  // 列表数据用路由表预取的这一份，不重复请求（点「刷新」才重新拉）。
  vmSnapOpen(name, snaps);
  renderVMStats([{ name }], { [name]: ok ? vm.statistics : null }, 'vmd-stat');
  // 串口 Tab 的连接按钮：未启用串口的 VM 不给连（服务端也会拒绝，这里只是别让人白点）。
  termNoSerial = !!(vm && vm.serial_console === false);
  vmConsoleSyncBtn();
  vmTabShow(vmTab); // 重渲染后保持当前 Tab（默认概览）
}

// ---------- VM 快照（列表 / 创建 / 删除 / 回滚；create+rollback 需关机态）----------
//
// 容器在 VM 详情页的「快照」Tab 里（原来挂在列表页的行内面板上）；取数与动作逻辑没变。

let snapVM = '';

function snapMsg(text, isErr) {
  const p = $('vm-snap-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// 定位到某台 VM 的快照：进详情页时调用。`pre` 是路由表预取的快照列表（有就直接渲染，
// 不再打一次请求）；不传则自己拉（「刷新」按钮、创建/删除/回滚之后）。
// 只有**换了一台 VM** 才清输入框与提示——轮询重渲染不该抹掉正在输入的快照名。
function vmSnapOpen(name, pre) {
  const changed = snapVM !== name;
  snapVM = name;
  if (changed) { $('vm-snap-new').value = ''; snapMsg('', false); }
  return vmSnapLoad(pre);
}

async function vmSnapLoad(pre) {
  const tbody = $('vm-snap-table').querySelector('tbody');
  tbody.textContent = '';
  if (!snapVM) return;
  let rows = pre;
  if (rows === undefined) {
    try {
      rows = await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) + '/snapshots');
    } catch (e) {
      snapMsg('读取快照失败：' + e.message, true);
      return;
    }
  }
  if (rows && rows.__err) {
    snapMsg('读取快照失败：' + rows.__err, true);
    return;
  }
  const list = Array.isArray(rows) ? rows : (rows && rows.snapshots) || [];
  if (!list.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '3', class: 'muted', text: '（无快照）' }));
    tbody.appendChild(tr);
    return;
  }
  list.forEach((s) => {
    const nm = s.name || s.snapshot;
    const tr = el('tr');
    [nm, fmtTime(s.created_at || s.created || s.creation_time)].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const rb = el('button', { type: 'button', class: 'ghost small', text: '回滚' });
    rb.addEventListener('click', () => vmSnapAct(nm, 'rollback'));
    const db = el('button', { type: 'button', class: 'danger small', text: '删除' });
    db.addEventListener('click', () => vmSnapAct(nm, 'delete'));
    cell.appendChild(rb);
    cell.appendChild(db);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

async function vmSnapAct(snap, kind) {
  const isRollback = kind === 'rollback';
  const warn = isRollback
    ? '回滚 ' + snapVM + ' 到快照 ' + snap + '？VM 必须处于关机态，磁盘内容会被替换为该快照的内容。'
    : '删除快照 ' + snap + '？该快照将不可恢复。';
  if (!window.confirm(warn)) return;
  snapMsg((isRollback ? '回滚' : '删除') + '中…', false);
  try {
    if (isRollback) {
      await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) +
        '/snapshots/' + encodeURIComponent(snap) + ':rollback', { method: 'POST' });
    } else {
      await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) +
        '/snapshots/' + encodeURIComponent(snap), { method: 'DELETE' });
    }
    snapMsg((isRollback ? '回滚' : '删除') + '已完成。', false);
  } catch (e) {
    // 关机态约束等拒绝理由原样展示（不吞错误）
    snapMsg((isRollback ? '回滚' : '删除') + '失败：' + e.message, true);
  }
  await vmSnapLoad();
}

async function vmSnapCreate() {
  const nm = $('vm-snap-new').value.trim();
  if (!nm) { snapMsg('请填写快照名。', true); return; }
  if (!window.confirm('为 ' + snapVM + ' 创建快照 ' + nm + '？VM 必须处于关机态。')) return;
  snapMsg('创建中…', false);
  try {
    await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) + '/snapshots', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: nm }),
    });
    snapMsg('快照已创建。', false);
    $('vm-snap-new').value = '';
  } catch (e) {
    snapMsg('创建失败：' + e.message, true);
  }
  await vmSnapLoad();
}

// ---------- 串口 console（一次性 ticket → WebSocket）----------
//
// 容器在 VM 详情页的「串口」Tab 里（原来挂在列表页的行内面板上）。同一时刻只连一台 VM
// （避免误操作）；离开详情页时由路由调 vmConsoleClose 断开，WebSocket 不会留在后台。

// 终端状态：WebSocket 与当前 VM 名（同一时刻只连一台，避免误操作）。
let termWS = null;
let termVM = '';
// 当前 VM 是否未启用串口（模型里 serial_console=false）：连接按钮随之禁用——
// 与列表页原来"未启用就不显示串口按钮"同一口径，只是这里说得出原因。
let termNoSerial = false;

// 连接按钮跟着真实状态走（连着、或该 VM 没启用串口，都禁用——点了只会得到一句解释）。
function vmConsoleSyncBtn() {
  const b = $('vm-console-connect');
  if (b) b.disabled = !!termWS || termNoSerial;
}

// 串口输出含 ANSI 转义（颜色/光标），去掉后按纯文本渲染（不引入终端模拟器）。
function stripANSI(s) {
  return s.replace(/\[[0-9;?]*[ -/]*[@-~]/g, '').replace(/[\]\][^]*/g, '');
}

function termAppend(text) {
  const out = $('vm-console-out');
  const atBottom = out.scrollTop + out.clientHeight >= out.scrollHeight - 8;
  out.textContent += stripANSI(text);
  if (out.textContent.length > 200000) out.textContent = out.textContent.slice(-150000);
  if (atBottom) out.scrollTop = out.scrollHeight;
}

function termMsg(text, isErr) {
  const out = $('vm-console-out');
  out.textContent += (isErr ? '\n[错误] ' : '\n[信息] ') + text + '\n';
  out.scrollTop = out.scrollHeight;
}

async function vmConsoleOpen(name) {
  if (termWS) { termMsg('先断开当前 console（' + termVM + '）。', true); return; }
  $('vm-console').hidden = false;
  $('vm-console-name').textContent = name;
  $('vm-console-out').textContent = '';
  termMsg('正在申请 console 凭证…', false);
  let res;
  try {
    res = await api('/virtual-machine-functions/' + encodeURIComponent(name) + '/console', { method: 'POST' });
  } catch (e) {
    termMsg('申请凭证失败：' + e.message, true);
    return;
  }
  const wsURL = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + res.ws_url;
  try {
    termWS = new WebSocket(wsURL);
  } catch (e) {
    termWS = null;
    termMsg('打开 WebSocket 失败：' + e.message, true);
    vmConsoleSyncBtn();
    return;
  }
  termVM = name;
  vmConsoleSyncBtn();
  termWS.onopen = () => termMsg('已连接 ' + name + ' 的串口（回车可让 guest 重绘提示符）。', false);
  termWS.onmessage = (ev) => termAppend(ev.data);
  termWS.onclose = () => { termMsg('连接已关闭。', false); termWS = null; termVM = ''; vmConsoleSyncBtn(); };
  termWS.onerror = () => termMsg('WebSocket 出错（凭证过期或串口不可用）。', true);
  $('vm-console-in').focus();
}

function vmConsoleClose() {
  if (termWS) { termWS.close(); termWS = null; termVM = ''; }
  vmConsoleSyncBtn();
  const box = $('vm-console');
  if (box) box.hidden = true;
  const out = $('vm-console-out');
  if (out) out.textContent = '';
}

function vmConsoleSend(text, enter) {
  if (!termWS || termWS.readyState !== 1) { termMsg('尚未连接。', true); return; }
  termWS.send(text + (enter ? '\r' : ''));
}

// ---------- 虚拟交换机（运行态 + 成员口计数）----------

function renderVSwitches(vss, rows) {
  table($('vs-table').querySelector('tbody'), 4, (rows || []).map((r) => [
    r.cfg.name, r.cfg.type,
    (r.cfg.ports || []).map((p) => p.interface || p.vnf || p.container || '?').join(', '),
    r.stat ? r.stat.bd_id : undefined,
  ]));
  $('vs-note').textContent = (vss && vss.length) ? '' : '（未配置虚拟交换机）';
  const pre = $('vs-stat');
  const lines = [];
  (rows || []).forEach((r) => {
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

// ---------- 容器：列表行（点行进详情）+ 生命周期 ----------

// 与 VM 同一套状态口径（容器状态来自 Docker）。
function renderContainerRows(cts) {
  const tbody = $('ct-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = cts || [];
  $('ct-note').textContent = rows.length ? '（' + rows.length + ' 个；点行进详情，动作按钮按当前状态启用）' : '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((c) => {
    const tr = rowClickable(el('tr'), '#/compute/containers/' + encodeURIComponent(c.name));
    [c.name, c.state, c.vcpu, c.memory_mb ? mb(c.memory_mb) : undefined, c.image].forEach((v) => {
      tr.appendChild(el('td', { text: String(dash(v)) }));
    });
    const cell = el('td', { class: 'actions' });
    const detBtn = rowButton(el('button', { type: 'button', class: 'ghost small', text: '详情' }));
    detBtn.addEventListener('click', () => goDetail('#/compute/containers/' + encodeURIComponent(c.name)));
    cell.appendChild(detBtn);
    VM_ACTIONS.forEach((a) => {
      const btn = rowButton(el('button', { type: 'button', class: 'ghost small', text: a.label }));
      btn.disabled = a.states.indexOf(String(c.state)) < 0;
      btn.addEventListener('click', () => ctAction(c.name, a.key, a.label, opsMsg));
      cell.appendChild(btn);
    });
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// `msg` 是回显去处：列表页用运维页的提示区（历史行为），详情页用本页的（ctdMsg）。
async function ctAction(name, action, label, msg) {
  const say = msg || opsMsg;
  if (!window.confirm(label + '容器 ' + name + '？容器内的进程会被' +
    (action === 'stop' ? '停止' : '重启') + '。')) return;
  say(label + ' ' + name + '：执行中…', false);
  try {
    await api('/container-functions/' + encodeURIComponent(name) + ':' + action, { method: 'POST' });
    say(label + ' ' + name + '：已受理。', false);
  } catch (e) {
    say(label + ' ' + name + ' 失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 容器详情页（#/compute/containers/:name）----------
//
// 对象头 + 两个 Tab（概览 / 日志）。日志复用 ctLogsLoad（tail=200，与 CLI 同源），
// 且**进 Tab 或点「刷新」才拉**——大段文本不该跟着页面轮询反复下载。

const CT_TABS = ['overview', 'logs'];
let ctTab = 'overview';
let ctDetailName = '';
let ctLogsName = '';

function ctTabShow(tab) {
  if (CT_TABS.indexOf(tab) < 0) tab = 'overview';
  ctTab = tab;
  CT_TABS.forEach((t) => { $('ct-tab-' + t).hidden = t !== tab; });
  const bar = $('ctd-tabs');
  for (const b of bar.querySelectorAll('button')) {
    if (b.dataset.tab === tab) b.setAttribute('aria-selected', 'true');
    else b.removeAttribute('aria-selected');
  }
}

// 点 Tab：切面板；进日志 Tab 顺手拉一次（与原来点行内「日志」按钮同一行为）。
function ctTabClick(tab) {
  ctTabShow(tab);
  if (tab === 'logs') ctLogsOpen(ctDetailName);
}

function ctdMsg(text, isErr) {
  const p = $('ctd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function ctDetailHead(ct) {
  return [
    ['状态', ct.state],
    ['镜像', ct.image],
    ['vCPU', ct.vcpu],
    ['内存', ct.memory_mb ? mb(ct.memory_mb) : undefined],
  ];
}

// 环境变量：契约里是字符串字典，按 `K=V` 串起来（空字典显示「—」）。
function envText(env) {
  const keys = Object.keys(env || {});
  return keys.length ? keys.map((k) => k + '=' + env[k]).join('；') : undefined;
}

function ctDetailInfo(ct) {
  const restart = ct.restart_policy === 'on-failure' ? '失败时重启'
    : (ct.restart_policy === 'no' ? '不自动重启' : ct.restart_policy);
  return [
    ['名称', ct.name],
    ['描述', ct.description],
    ['命令', ct.command],
    ['参数', list(ct.args)],
    ['环境变量', envText(ct.env)],
    ['重启策略', restart],
    ['开机自启', ct.autostart === undefined || ct.autostart === null ? undefined : (ct.autostart ? '是' : '否')],
    ['vNIC', (ct.interfaces || []).length],
  ];
}

function ctDetailActions(ct) {
  const box = $('ctd-actions');
  box.textContent = '';
  if (!ct) return;
  VM_ACTIONS.forEach((a) => {
    const btn = el('button', { type: 'button', text: a.label });
    btn.disabled = a.states.indexOf(String(ct.state)) < 0;
    btn.addEventListener('click', () => ctAction(ct.name, a.key, a.label, ctdMsg));
    box.appendChild(btn);
  });
}

function ctDetailIfaces(ct) {
  const tbody = $('ctd-iface-table').querySelector('tbody');
  tbody.textContent = '';
  const list0 = (ct && ct.interfaces) || [];
  if (!list0.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无 vNIC）' }));
    tbody.appendChild(tr);
    return;
  }
  list0.forEach((i) => {
    const tr = el('tr');
    [i.name, i.type, i.virtual_switch, i.mac, i.vlan, i.state].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    tbody.appendChild(tr);
  });
}

function renderContainerDetail(d, params) {
  const name = (params && params.name) || '';
  const ct = d['/container-functions/{name}'];
  const ok = ct && !ct.__err;
  const changed = ctDetailName !== name;
  ctDetailName = name;
  if (changed) { ctdMsg('', false); ctLogsPoint(name); }

  $('ctd-name').textContent = name;
  fill($('ctd-head'), ok ? ctDetailHead(ct) : [['读取失败', ct ? ct.__err : '未取到数据']]);
  fill($('ctd-info'), ok ? ctDetailInfo(ct) : []);
  ctDetailActions(ok ? ct : null);
  ctDetailIfaces(ok ? ct : null);
  ctTabShow(ctTab);
  // 换了对象且正停在日志 Tab：把新对象的日志拉出来（同一对象的轮询重渲染不重复拉）。
  if (changed && ctTab === 'logs') ctLogsOpen(name);
}

// 定位到某容器的日志（进详情页时调用；**不取数**——取数在进 Tab 或点「刷新」时）。
function ctLogsPoint(name) {
  if (ctLogsName === name) return;
  ctLogsName = name;
  $('ct-logs-name').textContent = name;
  $('ct-logs').textContent = '';
}

// 进日志 Tab：定位 + 拉一次。
async function ctLogsOpen(name) {
  ctLogsPoint(name);
  await ctLogsLoad();
}

async function ctLogsLoad() {
  const pre = $('ct-logs');
  if (!ctLogsName) return;
  pre.textContent = '读取中…';
  try {
    const res = await fetch(API + '/container-functions/' + encodeURIComponent(ctLogsName) + '/logs?tail=200', {
      headers: { Authorization: 'Bearer ' + token },
    });
    if (!res.ok) {
      let msg = 'HTTP ' + res.status;
      try { const b = await res.json(); if (b && b.message) msg = b.message; } catch (e) { /* 纯文本错误体 */ }
      pre.textContent = '读取失败：' + msg;
      return;
    }
    pre.textContent = (await res.text()) || '（无输出）';
  } catch (e) {
    pre.textContent = '读取失败：' + e.message;
  }
}

// ---------- 镜像仓库 ----------

function renderImages(imgs) {
  const tbody = $('img-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = imgs || [];
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((i) => {
    const tr = rowClickable(el('tr'), '#/compute/images/' + encodeURIComponent(i.name));
    [i.name, i.type, i.size_bytes ? bytes(i.size_bytes) : undefined, i.ref_count,
      i.import_state || 'ready'].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const detBtn = rowButton(el('button', { type: 'button', class: 'ghost small', text: '详情' }));
    detBtn.addEventListener('click', () => goDetail('#/compute/images/' + encodeURIComponent(i.name)));
    cell.appendChild(detBtn);
    const btn = rowButton(el('button', { type: 'button', class: 'danger small', text: '删除' }));
    btn.addEventListener('click', () => imgDelete(i.name, i.ref_count));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 镜像详情页（#/compute/images/:name）：完整元数据（元数据是静态的，本页不轮询）。
function renderImageDetail(img, params) {
  const name = (params && params.name) || '';
  $('imd-name').textContent = name;
  const ok = img && !img.__err;
  fill($('imd-head'), ok ? [
    ['类型', img.type],
    ['大小', img.size_bytes ? bytes(img.size_bytes) : undefined],
    ['引用数', img.ref_count],
  ] : [['读取失败', img ? img.__err : '未取到数据']]);
  fill($('imd-info'), ok ? [
    ['名称', img.name],
    ['描述', img.description],
    ['类型', img.type],
    ['格式', img.format],
    ['大小', img.size_bytes ? bytes(img.size_bytes) + '（' + img.size_bytes + ' 字节）' : undefined],
    ['sha256', img.sha256],
    ['引用计数', img.ref_count],
    ['导入状态', img.import_state || 'ready'],
    ['导入时间', img.imported_at ? fmtTime(img.imported_at) : undefined],
  ] : []);
}

// imgIsFailed / imgOutcome：**不把 2xx 当成功**——服务端可能已受理但导入失败
// （记录会以 import_state=failed 落库，界面必须如实呈现，否则就是"假绿"）。
function imgIsFailed(res) {
  const st = res && res.import_state;
  return st === 'failed';
}

function imgOutcome(res, name) {
  const st = (res && res.import_state) || '';
  if (st === 'failed') {
    const why = (res && (res.error || res.message)) || '服务端未给出原因';
    return '导入失败（' + name + '）：' + why;
  }
  if (st === 'ready' || st === 'imported') return '导入完成：' + name;
  if (st) return '已受理（' + name + '）：状态 ' + st + '，可在列表中查看';
  return '已受理：' + name + '（可在列表中查看导入状态）';
}

function imgMsg(text, isErr) {
  const p = $('img-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

async function imgDelete(name, refCount) {
  if (!window.confirm('删除镜像 ' + name + '？' +
    (refCount ? '（当前被引用 ' + refCount + ' 次，服务端会拒绝）' : '（不可恢复）'))) return;
  imgMsg('删除 ' + name + '：执行中…', false);
  try {
    await api('/images/' + encodeURIComponent(name), { method: 'DELETE' });
    imgMsg('已删除 ' + name + '。', false);
  } catch (e) {
    imgMsg('删除失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

function imgCommon() {
  const name = $('img-name').value.trim();
  const type = $('img-type').value.trim() || 'vm-image';
  if (!name) { imgMsg('请填写名称。', true); return null; }
  return { name, type };
}

async function imgImportURL() {
  const c = imgCommon();
  if (!c) return;
  const url = $('img-url').value.trim();
  const sha = $('img-sha').value.trim();
  if (!url) { imgMsg('请填写 URL。', true); return; }
  if (!sha) { imgMsg('URL 拉取必须提供 sha256（服务端默认强制校验）。', true); return; }
  if (!window.confirm('从 ' + url + ' 拉取并导入为 ' + c.name + '？大镜像可能耗时较久。')) return;
  imgMsg('拉取中…（大镜像可能耗时较久，请勿关闭页面）', false);
  try {
    const res = await api('/images', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: c.name, type: c.type, url, sha256: sha }),
    });
    imgMsg(imgOutcome(res, c.name), imgIsFailed(res));
  } catch (e) {
    imgMsg('导入失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

async function imgImportIncoming() {
  const c = imgCommon();
  if (!c) return;
  const file = $('img-incoming').value.trim();
  if (!file) { imgMsg('请填写 incoming 文件名。', true); return; }
  if (!window.confirm('把 /data/incoming/' + file + ' 导入为 ' + c.name + '？导入成功后该文件会被清理。')) return;
  imgMsg('导入中…', false);
  try {
    const res = await api('/images', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: c.name, type: c.type, incoming_file: file }),
    });
    imgMsg(imgOutcome(res, c.name), imgIsFailed(res));
  } catch (e) {
    imgMsg('导入失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

async function imgImportFile() {
  const c = imgCommon();
  if (!c) return;
  const f = $('img-file').files[0];
  if (!f) { imgMsg('请选择文件。', true); return; }
  if (!window.confirm('上传 ' + f.name + '（' + bytes(f.size) + '）并导入为 ' + c.name + '？')) return;
  const fd = new FormData();
  fd.append('name', c.name);
  fd.append('type', c.type);
  fd.append('file', f);
  imgMsg('上传中…（大镜像可能耗时较久，请勿关闭页面）', false);
  try {
    const res = await fetch(API + '/images', { method: 'POST', headers: { Authorization: 'Bearer ' + token }, body: fd });
    if (!res.ok) {
      let msg = 'HTTP ' + res.status;
      try { const b = await res.json(); if (b && b.message) msg = b.message; } catch (e) { /* 非 JSON */ }
      imgMsg('导入失败：' + msg, true);
    } else {
      const body = await res.json().catch(() => ({}));
      imgMsg(imgOutcome(body, c.name), imgIsFailed(body));
    }
  } catch (e) {
    imgMsg('导入失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 网络对象（只读总览）----------

// 每块：[标题, 数据, 列名, 取值函数, 详情页路由前缀（可选）]；有前缀时表格多一列"详情"，
// 且整行可点进详情页。NAT 与 LLDP 没有独立详情页（没有"单对象"语义：NAT 是配置对象，
// LLDP 是邻居表），故只列在总览里。
const NET_OBJECT_VIEWS = [
  ['VRF（L3 虚拟交换机）', 'vrfs', ['名称', 'L3 接口', '路由数'], (v) => [
    v.name,
    (v.l3_interfaces || []).map((i) => i.interface).join(', '),
    v.routes != null ? v.routes : undefined,
  ], '#/network/vrfs/'],
  ['ACL', 'acls', ['名称', '规则数'], (a) => [a.name, (a.rules || []).length], '#/network/acls/'],
  // NAT 是对象（source_pools/rules/static），按池与规则各出一行
  ['NAT', 'nat', ['类型', '内容'], (n) => [n.kind, n.summary]],
  ['链路聚合（bond）', 'bonds', ['名称', '模式', '成员'], (b) => [
    b.name, b.mode, (b.members || []).join(', '),
  ], '#/network/bonds/'],
  ['LLDP 邻居', 'lldp', ['本地口', '邻居', '管理地址'], (n) => [
    n.local_interface || n.interface, n.system_name || n.chassis_id, n.management_address,
  ]],
  ['QoS 策略', 'qos', ['名称', '类型', '目标'], (q) => [q.name, q.type, q.target || q.interface], '#/network/qos/'],
  ['端口镜像（SPAN）', 'span', ['名称', '源', '目的'], (s) => [
    s.name, list(s.sources || s.source), s.destination,
  ], '#/network/span/'],
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

function renderNetworkObjects(nets) {
  const box = $('net-objects');
  box.textContent = '';
  if (!nets) {
    box.appendChild(el('p', { class: 'muted', text: '（读取失败）' }));
    return;
  }
  NET_OBJECT_VIEWS.forEach(([title, key, cols, pick, routePrefix]) => {
    const data = nets[key];
    const wrap = el('div', { class: 'net-object' });
    wrap.appendChild(el('h3', { text: title }));
    if (data && data.__err) {
      wrap.appendChild(el('p', { class: 'muted small', text: '读取失败：' + data.__err }));
      box.appendChild(wrap);
      return;
    }
    const rows = key === 'nat' ? natRows(data) : (Array.isArray(data) ? data : (data ? [data] : []));
    const t = el('table');
    const thead = el('thead');
    const htr = el('tr');
    cols.forEach((c) => htr.appendChild(el('th', { text: c })));
    if (routePrefix) htr.appendChild(el('th', { text: '详情' }));
    thead.appendChild(htr);
    t.appendChild(thead);
    const tbody = el('tbody');
    if (!rows.length) {
      const tr = el('tr');
      tr.appendChild(el('td', { colspan: String(cols.length + (routePrefix ? 1 : 0)), class: 'muted', text: '（无）' }));
      tbody.appendChild(tr);
    } else {
      rows.forEach((r) => {
        const path = routePrefix ? routePrefix + encodeURIComponent(r.name) : '';
        const tr = path ? rowClickable(el('tr'), path) : el('tr');
        pick(r).forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
        if (path) {
          const cell = el('td', { class: 'actions' });
          const b = rowButton(el('button', { type: 'button', class: 'ghost small', text: '详情' }));
          b.addEventListener('click', () => goDetail(path));
          cell.appendChild(b);
          tr.appendChild(cell);
        }
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
function pickByName(rows, name) {
  return rowsOf(rows).find((r) => r.name === name) || null;
}

// 列表里没找到时给一句能读懂的话（而不是把空对象渲染成一片「—」）。
function notFoundText(name, label) {
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
async function vrfRoutesLoad(name) {
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
  ] : []);
}

function renderSpanDetail(rows, params) {
  const name = (params && params.name) || '';
  const s = pickByName(rows, name);
  const err = rows && rows.__err;
  const src = (s && (s.source || {})) || {};
  $('spd-name').textContent = name;
  fill($('spd-head'), s ? [
    ['源', src.interface || src.vnf_interface || src.vnf],
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

// ---------- 大表（NAT 会话）：按需拉取 ----------

function bigMsg(text, isErr) {
  const p = $('big-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function bigOut(text) {
  const pre = $('big-out');
  pre.hidden = !text;
  pre.textContent = text || '';
}

async function bigNat() {
  bigMsg('读取 NAT 会话…', false);
  bigOut('');
  try {
    const rows = await api('/nat/sessions');
    const list = Array.isArray(rows) ? rows : [];
    if (!list.length) { bigMsg('无 NAT 会话。', false); return; }
    const lines = list.map((r) => dash(r.inside_ip) + ':' + dash(r.inside_port) +
      '  ->  ' + dash(r.outside_ip) + ':' + dash(r.outside_port));
    bigOut('inside -> outside' + '\n' + lines.join('\n'));
    bigMsg('NAT 会话 ' + list.length + ' 条。', false);
  } catch (e) {
    bigMsg('读取失败：' + e.message, true);
  }
}

// ---------- 审计日志 ----------

function renderAudit(audit) {
  const rows = (audit && !audit.__err && Array.isArray(audit.items)) ? audit.items : (Array.isArray(audit) ? audit : []);
  table($('audit-table').querySelector('tbody'), 5, rows.slice(0, 50).map((a) => [
    fmtTime(a.ts || a.time || a.timestamp), a.user, a.action || a.event,
    a.detail || a.message, a.result || a.outcome,
  ]));
  $('audit-note').textContent = audit && audit.__err ? '（读取失败：' + audit.__err + '）'
    : (rows.length ? '（最近 ' + Math.min(rows.length, 50) + ' 条）' : '');
}

// ---------- 抓包（数据面 pcap trace）----------

function capMsg(text, isErr) {
  const p = $('cap-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function renderCapture(cap) {
  const active = cap && !cap.__err ? cap.active : null;
  const files = (cap && !cap.__err && cap.files) || [];
  $('cap-note').textContent = cap && cap.__err ? '（读取失败：' + cap.__err + '）' : '';
  $('cap-active').textContent = active
    ? '进行中：接口 ' + active.interface + '，已抓 ' + dash(active.captured) + ' 包，深度 ' +
      dash(active.max_depth) + '，开始于 ' + fmtTime(active.started_at)
    : '（无进行中的抓包）';
  $('cap-active').className = active ? 'muted small' : 'muted small';

  const tbody = $('cap-table').querySelector('tbody');
  tbody.textContent = '';
  if (!files.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '4', class: 'muted', text: '（无已导出 pcap）' }));
    tbody.appendChild(tr);
    return;
  }
  files.forEach((f) => {
    const tr = el('tr');
    [f.name, bytes(f.size_bytes), fmtTime(f.created_at)].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const btn = el('button', { type: 'button', class: 'ghost small', text: '下载' });
    btn.addEventListener('click', () => downloadFile('/vpp/capture/' + encodeURIComponent(f.name), f.name, capMsg));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// downloadFile 带 Authorization 取文件再触发浏览器下载（<a href> 带不了请求头）。
async function downloadFile(path, name, note) {
  if (note) note('下载 ' + name + '：准备中…', false);
  try {
    const res = await fetch(API + path, { headers: { Authorization: 'Bearer ' + token } });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = el('a', { href: url, download: name });
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    if (note) note('已触发下载：' + name + '（' + bytes(blob.size) + '）', false);
  } catch (e) {
    if (note) note('下载失败：' + e.message, true);
  }
}

// 归档（配置备份 / 诊断归档）：列表 + 下载
function renderArchives(backups, techs) {
  archiveTable($('ops-backup-table').querySelector('tbody'), backups, '/system/backup/');
  archiveTable($('ops-tech-table').querySelector('tbody'), techs, '/system/tech-support/');
}

function archiveTable(tbody, rows, prefix) {
  tbody.textContent = '';
  const list = (rows && !rows.__err && Array.isArray(rows)) ? rows : [];
  if (!list.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '4', class: 'muted', text: rows && rows.__err ? '读取失败：' + rows.__err : '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  list.forEach((f) => {
    const name = f.file || f.name;
    const tr = el('tr');
    [name, bytes(f.size_bytes), fmtTime(f.created_at || f.created)].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const btn = el('button', { type: 'button', class: 'ghost small', text: '下载' });
    btn.addEventListener('click', () => downloadFile(prefix + encodeURIComponent(name), name, opsMsg));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// ---------- 运维动作（写操作，均二次确认）----------

function opsMsg(text, isErr) {
  const p = $('ops-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function opsOut(text) {
  const pre = $('ops-out');
  pre.hidden = !text;
  pre.textContent = text || '';
}

// opsRun 统一的"确认 → 调用 → 回显"流程（写操作一律先确认，与 CLI 的 --yes 同口径）。
async function opsRun(label, confirmText, fn) {
  if (!window.confirm(confirmText)) return;
  opsMsg(label + '：执行中…', false);
  opsOut('');
  try {
    const out = await fn();
    opsMsg(label + '：完成。', false);
    if (out) opsOut(typeof out === 'string' ? out : JSON.stringify(out, null, 2));
  } catch (e) {
    opsMsg(label + ' 失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 启动 ----------

$('login-form').addEventListener('submit', doLogin);
$('logout-btn').addEventListener('click', doLogout);
$('refresh-btn').addEventListener('click', () => reload().catch((e) => showGlobalError(e.message)));
$('cfg-edit-btn').addEventListener('click', cfgStartEdit);
$('cfg-save-btn').addEventListener('click', cfgSave);
$('cfg-check-btn').addEventListener('click', cfgCheck);
$('diag-ping-btn').addEventListener('click', diagPing);
$('diag-log-btn').addEventListener('click', diagLogs);
$('cfg-diff-btn').addEventListener('click', cfgShowDiff);
$('cfg-commit-btn').addEventListener('click', () => cfgCommit(0));
$('cfg-commit-confirmed-btn').addEventListener('click', () => cfgCommit(10));
$('cfg-confirm-btn').addEventListener('click', cfgConfirmPending);
$('cfg-discard-btn').addEventListener('click', cfgEndSession);

// 审计卡：写入不发事件，故给一个显式刷新（否则 SSE 连着时卡片会停在旧内容上）
$('audit-refresh-btn').addEventListener('click', async () => {
  const btn = $('audit-refresh-btn');
  btn.disabled = true;
  try {
    renderAudit(await api('/audit-logs?limit=50'));
    $('audit-note').textContent = '（已刷新 ' + new Date().toLocaleTimeString() + '）';
  } catch (e) {
    $('audit-note').textContent = '（刷新失败：' + e.message + '）';
  } finally {
    btn.disabled = false;
  }
});

// 大表（按需拉取；VRF 路由表已移进 VRF 详情页）
$('big-nat-btn').addEventListener('click', bigNat);
$('big-clear-btn').addEventListener('click', () => { bigMsg('', false); bigOut(''); });

// 详情页 Tab 条（页内状态，不进 hash）
for (const b of $('vmd-tabs').querySelectorAll('button')) {
  b.addEventListener('click', () => vmTabShow(b.dataset.tab, true));
}
for (const b of $('ctd-tabs').querySelectorAll('button')) {
  b.addEventListener('click', () => ctTabClick(b.dataset.tab));
}

// VRF 详情：路由表按需重拉
$('vrd-routes-btn').addEventListener('click', () => vrfRoutesLoad($('vrd-name').textContent));

// VM 快照
$('vm-snap-refresh').addEventListener('click', () => vmSnapLoad());
$('vm-snap-create').addEventListener('click', vmSnapCreate);

// 镜像导入
$('img-url-btn').addEventListener('click', imgImportURL);
$('img-incoming-btn').addEventListener('click', imgImportIncoming);
$('img-file-btn').addEventListener('click', imgImportFile);

// 容器日志
$('ct-logs-refresh').addEventListener('click', ctLogsLoad);

// 串口 console
$('vm-console-connect').addEventListener('click', () => vmConsoleOpen(vmDetailName));
$('vm-console-close').addEventListener('click', vmConsoleClose);
$('vm-console-send').addEventListener('click', () => {
  const inp = $('vm-console-in');
  vmConsoleSend(inp.value, true);
  inp.value = '';
});
$('vm-console-enter').addEventListener('click', () => vmConsoleSend('', true));
$('vm-console-in').addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter') {
    ev.preventDefault();
    vmConsoleSend($('vm-console-in').value, true);
    $('vm-console-in').value = '';
  }
});

// 抓包动作
$('cap-start-btn').addEventListener('click', () => {
  const ifname = $('cap-iface').value.trim();
  if (!ifname) { capMsg('请填写接口名（如 ens192）。', true); return; }
  const count = Number($('cap-count').value) || 0;
  return opsRun('开始抓包', '在接口 ' + ifname + ' 上开始抓包？（占用少量数据面开销，用完请停止）',
    () => api('/vpp/capture', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(count ? { interface: ifname, count } : { interface: ifname }),
    })).then(() => capMsg('抓包已开始：' + ifname, false));
});
$('cap-stop-btn').addEventListener('click', () => opsRun('停止抓包（丢弃）',
  '停止抓包并丢弃缓冲？（不导出文件）',
  async () => {
    await api('/vpp/capture', { method: 'DELETE' });
    capMsg('已停止（未导出）。', false);
    return '';
  }));
$('cap-export-btn').addEventListener('click', () => opsRun('停止并导出 pcap',
  '停止抓包并导出 pcap 文件？',
  async () => {
    const r = await api('/vpp/capture?export=true', { method: 'DELETE' });
    capMsg(r && r.exported ? '已导出：' + r.name + '（' + bytes(r.size_bytes) + '）'
      : (r && r.message) || '未捕获到报文，无文件导出', false);
    return '';
  }));

// 运维动作（写操作；confirm 文案写清影响面）
$('ops-backup-btn').addEventListener('click', () => opsRun('生成配置备份',
  '生成一份配置备份归档？（只读操作，不改运行配置）',
  async () => {
    const r = await api('/system/backup', { method: 'POST' });
    return r && r.file ? '备份文件：' + r.file : '已生成。';
  }));
$('ops-tls-btn').addEventListener('click', () => opsRun('重签自签证书',
  '重签本机自签证书？浏览器会提示证书变化（客户端需重新固定），当前页面可继续使用。',
  () => api('/system/tls:regenerate', { method: 'POST' })));
$('ops-sshkey-btn').addEventListener('click', () => opsRun('重新生成 SSH host key',
  '重新生成 SSH host key？新连接的 host key 会变化，客户端需更新 known_hosts。',
  () => api('/system/ssh-host-key:regenerate', { method: 'POST' })));
$('ops-techsupport-btn').addEventListener('click', () => opsRun('生成 tech-support 归档',
  '生成诊断归档？（收集配置/日志/状态，只读）',
  () => api('/system/tech-support', { method: 'POST' })));
$('ops-coredumps-btn').addEventListener('click', () => opsRun('列出 core dump',
  '列出当前 core dump 文件？',
  async () => {
    const rows = await api('/system/core-dumps');
    if (!rows || !rows.length) return '（无 core dump）';
    return rows.map((r) => r.file + '  ' + r.process + '  ' + bytes(r.size_bytes) + '  ' + fmtTime(r.occurred_at)).join('\n');
  }));
$('ops-alarms-btn').addEventListener('click', () => opsRun('清除已恢复告警',
  '清除已恢复（resolved）的告警？活动告警不受影响。',
  () => api('/alarms:clear', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' })));
$('ops-export-btn').addEventListener('click', () => {
  const url = $('ops-export-url').value.trim();
  if (!url) { opsMsg('请填写导出目标地址（http/https）。', true); return; }
  return opsRun('导出 core dump 清单', '把 core dump 清单（JSON）POST 到 ' + url + '？',
    () => api('/system/core-dumps:export', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ url }),
    }));
});
$('ops-vpprestart-btn').addEventListener('click', () => opsRun('重启数据面（VPP）',
  '重启数据面？所有经 VPP 的业务流量会中断数秒，VNF 的 vhost-user 口会重建。',
  () => api('/vpp/restart', { method: 'POST' })));
$('ops-reboot-btn').addEventListener('click', () => opsRun('重启主机',
  '重启整台主机？所有 VNF 与容器会停止，管理面会断开数分钟。',
  () => api('/system:reboot', { method: 'POST' })));
$('ops-shutdown-btn').addEventListener('click', () => opsRun('关机',
  '关闭整台主机？所有 VNF 与容器会停止，管理面断开后需带外开机。',
  () => api('/system:shutdown', { method: 'POST' })));

// 诊断卡新增：traceroute 与接口计数清零
$('diag-trace-btn').addEventListener('click', async () => {
  const host = $('diag-host').value.trim();
  const out = $('diag-out');
  out.hidden = false;
  if (!host) { out.textContent = '请填写目标地址。'; return; }
  out.textContent = '执行中…';
  try {
    const res = await fetch(API + '/diagnostics/traceroute', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
      body: JSON.stringify(pingBody(host, 0)),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      const detail = (body.detail || []).map((d) => d.message).join('\n');
      out.textContent = '失败：' + (body.message || ('HTTP ' + res.status)) + (detail ? '\n' + detail : '');
      return;
    }
    out.textContent = body.output || '（无输出）';
  } catch (e) {
    out.textContent = '失败：' + e.message;
  }
});
$('diag-clear-btn').addEventListener('click', () => {
  const name = $('diag-clear-if').value.trim();
  return opsRun('清零接口统计',
    name ? '清零接口 ' + name + ' 的收发计数？' : '清零所有接口的收发计数？',
    () => api('/interfaces:clear-statistics', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(name ? { name } : {}),
    }));
});

// 关页面/刷新时收尾：实时通道、轮询、以及串口 WebSocket（详情页的串口连着就要断开）。
window.addEventListener('beforeunload', () => { stopStream(); stopPolling(); vmConsoleClose(); });

// 动作处理器与浏览器控制台用得上：刷新当前页 / 跳到某条路由（如 #/ops/audit）。
window.nfvis = {
  reload,
  navigate: async (path) => (await routerModule()).navigate(path),
  // 串口状态（浏览器验收要断言"离开详情页后 WebSocket 已关"，这里给一个可读的判据）。
  consoleOpen: () => !!termWS,
};

(async function boot() {
  if (!token) { showLogin(''); return; }
  let user = null;
  try { user = JSON.parse(sessionStorage.getItem(USER_KEY) || 'null'); } catch (e) { user = null; }
  try {
    await enterApp(user);
  } catch (e) {
    signOut('登录状态已失效，请重新登录。');
  }
})();
