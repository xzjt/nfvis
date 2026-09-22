// NFViS 控制台前端（只读总览）。
//
// 无外部依赖、无构建步骤：改完直接刷新页面即可，产物随二进制内嵌。
// 取数一律走同源 REST 接口并带 Bearer token；本文件不写任何配置。
//
// 数据来源（只用**对外**的 REST 端点；`/cli/execute` 在契约里标着"仅 nfvis-cli 使用、
// 不承诺第三方兼容"，前端不去碰它，也不去解析终端文本）：
//   /metrics                 —— CPU 使用率、内存、根文件系统（Prometheus 文本，格式稳定）
//   /system/status           —— 主机名、运行时长、配置是否就绪
//   /system/version          —— 各组件版本
//   /resource-pools          —— 大页池（按页大小）与隔离核分配
//   /vpp/status              —— 数据面版本/连接/待重启/线程/buffer/内存
//   /interfaces(/<name>)     —— 接口配置 + 逐口收发计数
//   /virtual-machine-functions、/container-functions、/alarms
//   /events                  —— 事件推送（SSE）
//
// 关于实时刷新：浏览器的 EventSource **无法自定义请求头**，而 /events 需要
// Authorization，故这里用 fetch + 流式读取手工解析 SSE 帧（同样走头部传 token，
// 不把 token 放进 URL——URL 会进日志与浏览器历史）。流断了就退化为定时轮询。

const API = location.pathname.replace(/\/ui\/.*$/, '') || '/api/v1';
const TOKEN_KEY = 'nfvis.token';
const USER_KEY = 'nfvis.user';
const POLL_MS = 5000;
const MAX_EVENTS = 20;
const MAX_IFACE_DETAIL = 12;

let token = sessionStorage.getItem(TOKEN_KEY) || '';
let pollTimer = null;
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

// Prometheus 文本解析：只要"指标名 → 数值"，带 label 的同一指标名相加
// （本页用到的系统类指标都是单序列，相加只是为了让实现对多序列也成立）。
function parseMetrics(text) {
  const acc = {};
  text.split('\n').forEach((raw) => {
    const line = raw.trim();
    if (!line || line.charAt(0) === '#') return;
    const sp = line.lastIndexOf(' ');
    if (sp < 0) return;
    const name = line.slice(0, sp).split('{')[0];
    const v = parseFloat(line.slice(sp + 1));
    if (isNaN(v)) return;
    acc[name] = (acc[name] || 0) + v;
  });
  return acc;
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

async function apiText(path) {
  const res = await fetch(API + path, { headers: { Authorization: 'Bearer ' + token } });
  if (!res.ok) throw new Error('HTTP ' + res.status);
  return res.text();
}

// 单个端点失败不该拖垮整页：各自降级为"读取失败"。
const soft = (p) => p.catch((e) => ({ __err: e.message }));

async function loadAll() {
  const [st, ver, vpp, pools, ifaces, vms, cts, alarms, metrics] = await Promise.all([
    soft(api('/system/status')),
    soft(api('/system/version')),
    soft(api('/vpp/status')),
    soft(api('/resource-pools')),
    soft(api('/interfaces')),
    soft(api('/virtual-machine-functions')),
    soft(api('/container-functions')),
    soft(api('/alarms')),
    soft(apiText('/metrics')),
  ]);
  const ifaceRows = Array.isArray(ifaces) ? await loadInterfaceStats(ifaces) : [];
  render(st, ver, vpp, pools, ifaces, ifaceRows, vms, cts, alarms, metrics);
}

// 逐口取统计（列表端点只有配置字段；收发包数在 /interfaces/<name> 上）。
async function loadInterfaceStats(ifaces) {
  const head = ifaces.slice(0, MAX_IFACE_DETAIL);
  const details = await Promise.all(head.map((i) => api('/interfaces/' + encodeURIComponent(i.name)).catch(() => null)));
  return head.map((i, n) => ({ cfg: i, stat: (details[n] || {}).statistics || null }));
}

function render(st, ver, vpp, pools, ifaces, ifaceRows, vms, cts, alarms, metrics) {
  const m = (metrics && typeof metrics === 'string') ? parseMetrics(metrics) : {};
  $('host-line').textContent = st && st.hostname ? st.hostname : '';

  fill($('sys-list'), st && st.__err ? [['读取失败', st.__err]] : [
    ['主机名', st && st.hostname],
    ['运行时长', uptime(st && st.uptime_seconds)],
    ['配置就绪', st && st.config_ready === false ? '否' : '是'],
    ['CPU', m.nfvis_system_cpu_online_count ? m.nfvis_system_cpu_online_count + ' 核' : undefined],
    ['CPU 使用率', pct(m.nfvis_system_cpu_utilization_ratio)],
    ['内存', m.nfvis_system_memory_total_bytes
      ? bytes(m.nfvis_system_memory_available_bytes) + ' 可用 / ' + bytes(m.nfvis_system_memory_total_bytes)
      : undefined],
    ['根文件系统', m.nfvis_system_disk_total_bytes
      ? bytes(m.nfvis_system_disk_free_bytes) + ' 可用 / ' + bytes(m.nfvis_system_disk_total_bytes) + '（已用 ' + pct(m.nfvis_system_disk_used_ratio) + '）'
      : undefined],
  ]);
  fill($('ver-list'), ver && ver.__err ? [['读取失败', ver.__err]] : [
    ['NFViS', ver && ver.nfvis], ['VPP', ver && ver.vpp], ['DPDK', ver && ver.dpdk],
    ['libvirt', ver && ver.libvirt], ['QEMU', ver && ver.qemu], ['Docker', ver && ver.docker],
    ['Ubuntu', ver && ver.ubuntu],
  ]);

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

  // 接口：列表端点只有配置字段（名称/说明/MTU 等），逐口统计在详情端点上。
  const cfgOnly = (ifaces || []).length > ifaceRows.length;
  table($('iface-table').querySelector('tbody'), 7, ifaceRows.map((r) => [
    r.cfg.name, r.cfg.description, r.cfg.mtu,
    r.stat ? r.stat.rx_packets : undefined, r.stat ? r.stat.tx_packets : undefined,
    r.stat ? r.stat.rx_errors : undefined, r.stat ? r.stat.tx_drops : undefined,
  ]));
  $('iface-note').textContent = cfgOnly
    ? '（共 ' + ifaces.length + ' 个接口，此处只列前 ' + MAX_IFACE_DETAIL + ' 个）'
    : '';

  table($('vm-table').querySelector('tbody'), 5, (vms || []).map((v) => [
    v.name, v.state, v.vcpu ? v.vcpu.count : undefined, v.memory ? mb(v.memory.size_mb) : undefined, v.image,
  ]));
  table($('ct-table').querySelector('tbody'), 5, (cts || []).map((c) => [
    c.name, c.state, c.vcpu, c.memory_mb ? mb(c.memory_mb) : undefined, c.image,
  ]));

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
  renderEvents();
}

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

// ---------- 实时通道 ----------

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
    loadAll().catch((e) => showGlobalError(e.message));
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

function startPolling() {
  if (pollTimer) return;
  pollTimer = setInterval(() => {
    loadAll().catch((e) => showGlobalError(e.message));
  }, POLL_MS);
}

function stopPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
}

function showGlobalError(msg) {
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
  await loadAll();
  startStream();
}

function signOut(msg) {
  stopStream();
  stopPolling();
  token = '';
  events = [];
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

// ---------- 启动 ----------

$('login-form').addEventListener('submit', doLogin);
$('logout-btn').addEventListener('click', doLogout);
$('refresh-btn').addEventListener('click', () => loadAll().catch((e) => showGlobalError(e.message)));
window.addEventListener('beforeunload', () => { stopStream(); stopPolling(); });

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
