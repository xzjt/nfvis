// NFViS 控制台前端（只读总览）。
//
// 无外部依赖、无构建步骤：改完直接刷新页面即可，产物随二进制内嵌。
// 取数一律走同源 REST 接口并带 Bearer token；本文件不写任何配置。
//
// 关于实时刷新：浏览器的 EventSource **无法自定义请求头**，而 /events 需要
// Authorization，故这里用 fetch + 流式读取手工解析 SSE 帧（同样走头部传 token，
// 不把 token 放进 URL——URL 会进日志与浏览器历史）。流断了就退化为定时轮询。

const API = location.pathname.replace(/\/ui\/.*$/, '') || '/api/v1';
const TOKEN_KEY = 'nfvis.token';
const USER_KEY = 'nfvis.user';
const POLL_MS = 5000;
const MAX_EVENTS = 20;

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

function fill(dl, pairs) {
  dl.textContent = '';
  pairs.forEach(([k, v]) => {
    dl.appendChild(el('dt', { text: k }));
    dl.appendChild(el('dd', { text: v === undefined || v === null || v === '' ? '—' : String(v) }));
  });
}

function table(tbody, rows) {
  tbody.textContent = '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '8', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((cells) => {
    const tr = el('tr');
    cells.forEach((c) => tr.appendChild(el('td', { text: c === undefined || c === null || c === '' ? '—' : String(c) })));
    tbody.appendChild(tr);
  });
}

function uptime(sec) {
  if (!sec && sec !== 0) return '—';
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  return (d ? d + ' 天 ' : '') + h + ' 小时 ' + m + ' 分';
}

function bytes(n) {
  if (n === undefined || n === null) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}

function mb(n) {
  if (n === undefined || n === null) return '—';
  return Number(n) >= 1024 ? (Number(n) / 1024).toFixed(1) + ' GB' : n + ' MB';
}

function fmtTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  return isNaN(d) ? ts : d.toLocaleString();
}

function list(v) {
  if (!v) return '—';
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

async function loadAll() {
  // 只读端点并行取；单个失败不拖垮整页（各自的卡片显示错误行）。
  const [sys, ver, vpp, pools, ifaces, vms, cts, alarms] = await Promise.all([
    api('/system/status').catch((e) => ({ __err: e.message })),
    api('/system/version').catch((e) => ({ __err: e.message })),
    api('/vpp/status').catch((e) => ({ __err: e.message })),
    api('/resource-pools').catch((e) => ({ __err: e.message })),
    api('/interfaces').catch(() => []),
    api('/virtual-machine-functions').catch(() => []),
    api('/container-functions').catch(() => []),
    api('/alarms').catch(() => []),
  ]);
  render(sys, ver, vpp, pools, ifaces, vms, cts, alarms);
}

function render(sys, ver, vpp, pools, ifaces, vms, cts, alarms) {
  $('host-line').textContent = sys && sys.hostname ? sys.hostname : '';
  fill($('sys-list'), [
    ['主机名', sys && sys.hostname],
    ['运行时长', sys && uptime(sys.uptime_seconds)],
    ['CPU', sys && sys.cpu ? sys.cpu.total + ' 核（隔离 ' + list(sys.cpu.isolated) + '）' : undefined],
    ['CPU 使用率', sys && sys.cpu && sys.cpu.usage_percent !== undefined ? sys.cpu.usage_percent + '%' : undefined],
    ['内存', sys && sys.memory ? mb(sys.memory.used_mb) + ' / ' + mb(sys.memory.total_mb) : undefined],
    ['大页', sys && sys.hugepages ? sys.hugepages.total + ' × ' + (sys.hugepages.page_size_kb / 1024) + 'G（空闲 ' + sys.hugepages.free + '）' : undefined],
    ['数据盘', sys && sys.storage ? sys.storage.data_used_gb + ' / ' + sys.storage.data_total_gb + ' GB' : undefined],
    ['镜像占用', sys && sys.storage ? sys.storage.images_gb + ' GB' : undefined],
  ]);
  fill($('ver-list'), ver && ver.__err ? [['读取失败', ver.__err]] : [
    ['NFViS', ver && ver.nfvis], ['VPP', ver && ver.vpp], ['DPDK', ver && ver.dpdk],
    ['libvirt', ver && ver.libvirt], ['QEMU', ver && ver.qemu], ['Docker', ver && ver.docker],
    ['Ubuntu', ver && ver.ubuntu],
  ]);

  fill($('vpp-list'), vpp && vpp.__err ? [['读取失败', vpp.__err]] : [
    ['状态', vpp && vpp.state],
    ['版本', vpp && vpp.version],
    ['配置版本', vpp && vpp.config_revision],
    ['待重启生效', vpp && (vpp.pending_restart ? '是' : '否')],
    ['主堆', vpp && vpp.main_heap_size],
    ['线程数', vpp && vpp.threads ? vpp.threads.length : undefined],
  ]);

  const p = $('pools');
  p.textContent = '';
  if (pools && pools.__err) {
    p.appendChild(el('p', { class: 'error', text: pools.__err }));
  } else {
    const rows = (pools && pools.hugepages || []).map((h) => [h.page_size, h.total, h.allocated, h.free]);
    const t = el('table', {}, [
      el('thead', {}, [el('tr', {}, ['页大小', '总数', '已分配', '空闲'].map((h) => el('th', { text: h })))]),
      el('tbody', {}, rows.length ? rows.map((r) => el('tr', {}, r.map((c) => el('td', { text: String(c) })))) :
        [el('tr', {}, [el('td', { colspan: '4', class: 'muted', text: '（无）' })])]),
    ]);
    p.appendChild(t);
    if (pools && pools.cpu) {
      fill(p.appendChild(el('dl', { class: 'kv' })), [
        ['隔离核', list(pools.cpu.isolated_cores)],
        ['VPP 保留核', list(pools.cpu.vpp_reserved)],
        ['空闲核', list(pools.cpu.free)],
      ]);
    }
  }

  table($('iface-table').querySelector('tbody'), (ifaces || []).map((i) => [
    i.name, i.kind, i.driver, i.enabled === false ? 'down' : 'up', i.link, i.speed_mbps ? i.speed_mbps + ' Mb/s' : '—', i.mtu, i.description,
  ]));
  table($('vm-table').querySelector('tbody'), (vms || []).map((v) => [
    v.name, v.state, v.vcpu ? v.vcpu.count : '—', v.memory ? mb(v.memory.size_mb) : '—', v.image,
  ]));
  table($('ct-table').querySelector('tbody'), (cts || []).map((c) => [
    c.name, c.state, c.vcpu, c.memory_mb ? mb(c.memory_mb) : '—', c.image,
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
        for (const line of frame.split('\n')) {
          if (line.startsWith('data:')) data += line.slice(5).trim();
        }
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
    // 流正常结束（服务端重启等）：退化为轮询并继续尝试重连。
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
