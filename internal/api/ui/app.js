// NFViS 控制台前端（只读总览）。
//
// 无外部依赖、无构建步骤：改完直接刷新页面即可，产物随二进制内嵌。
// 取数一律走同源 REST 接口并带 Bearer token；本文件不写任何配置。
//
// 数据来源（只用**对外**的 REST 端点；`/cli/execute` 在契约里标着"仅 nfvis-cli 使用、
// 不承诺第三方兼容"，前端不去碰它，也不去解析终端文本）：
//   /system/status           —— 主机名、运行时长、配置就绪、CPU/内存/磁盘
//                               （R37-1 收口后这些数字与 CLI show system 同源、直接可取，
//                                不再自己解析 /metrics 的 Prometheus 文本）
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

async function loadAll() {
  const [st, ver, vpp, pools, ifaces, vms, cts, alarms] = await Promise.all([
    soft(api('/system/status')),
    soft(api('/system/version')),
    soft(api('/vpp/status')),
    soft(api('/resource-pools')),
    soft(api('/interfaces')),
    soft(api('/virtual-machine-functions')),
    soft(api('/container-functions')),
    soft(api('/alarms')),
  ]);
  const ifaceRows = Array.isArray(ifaces) ? await loadInterfaceStats(ifaces) : [];
  render(st, ver, vpp, pools, ifaces, ifaceRows, vms, cts, alarms);
}

// 逐口取统计（列表端点只有配置字段；收发包数在 /interfaces/<name> 上）。
async function loadInterfaceStats(ifaces) {
  const head = ifaces.slice(0, MAX_IFACE_DETAIL);
  const details = await Promise.all(head.map((i) => api('/interfaces/' + encodeURIComponent(i.name)).catch(() => null)));
  return head.map((i, n) => ({ cfg: i, stat: (details[n] || {}).statistics || null }));
}

function render(st, ver, vpp, pools, ifaces, ifaceRows, vms, cts, alarms) {
  $('host-line').textContent = st && st.hostname ? st.hostname : '';

  // 系统卡片的数字全部取自 /system/status（R37-1 收口后与 CLI show system 同源）；
  // 字段缺席（非 Linux 无宿主指标等）时对应行显示「—」，不编造。
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

function cfgRenderForms(c) {
  const box = $('cfg-forms');
  box.textContent = '';
  const sys = el('fieldset', {});
  sys.appendChild(el('legend', { text: '系统' }));

  let seq = 0;
  const addInput = (label, value, onchange) => {
    const id = 'cfg-f-' + (seq++);
    sys.appendChild(el('label', { text: label, for: id })); // 关联 label ↔ input（无障碍/可测）
    const inp = el('input', { type: 'text', id });
    inp.value = value || '';
    // 监听 input 而非 change：change 只在**用户输入导致的**失焦时触发，程序化赋值
    // （浏览器自动化、脚本回填）不置"值已改"标志、失焦也不发 change；input 两者都覆盖。
    inp.addEventListener('input', onchange);
    sys.appendChild(inp);
    return inp;
  };

  const hostnameVal = cfgForm.hostname.get(c);
  addInput(cfgForm.hostname.label, hostnameVal, (e) => cfgApplyForm('hostname', e.target.value));

  const ntpVals = cfgForm.ntp.get(c);
  const ntpInputs = [0, 1].map((i) => addInput(cfgForm.ntp.label + ' ' + (i + 1), ntpVals[i],
    () => cfgApplyForm('ntp', ntpInputs.map((x) => x.value.trim()))));

  const dnsVals = cfgForm.dns.get(c);
  const dnsInputs = [0, 1].map((i) => addInput(cfgForm.dns.label + ' ' + (i + 1), dnsVals[i],
    () => cfgApplyForm('dns', dnsInputs.map((x) => x.value.trim()))));

  box.appendChild(sys);
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

async function loadConfig() {
  const res = await api('/configuration');
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
  cfgSetEditing(false);
  await Promise.all([loadAll(), loadConfig().catch(() => {})]);
  startStream();
}

function signOut(msg) {
  stopStream();
  stopPolling();
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

// ---------- 启动 ----------

$('login-form').addEventListener('submit', doLogin);
$('logout-btn').addEventListener('click', doLogout);
$('refresh-btn').addEventListener('click', () => loadAll().catch((e) => showGlobalError(e.message)));
$('cfg-edit-btn').addEventListener('click', cfgStartEdit);
$('cfg-save-btn').addEventListener('click', cfgSave);
$('cfg-diff-btn').addEventListener('click', cfgShowDiff);
$('cfg-commit-btn').addEventListener('click', () => cfgCommit(0));
$('cfg-commit-confirmed-btn').addEventListener('click', () => cfgCommit(10));
$('cfg-confirm-btn').addEventListener('click', cfgConfirmPending);
$('cfg-discard-btn').addEventListener('click', cfgEndSession);
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
