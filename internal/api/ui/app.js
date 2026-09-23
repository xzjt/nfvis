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
  const [st, ver, vpp, pools, ifaces, vms, cts, alarms, vss, imgs, nets, audit, cap] = await Promise.all([
    soft(api('/system/status')),
    soft(api('/system/version')),
    soft(api('/vpp/status')),
    soft(api('/resource-pools')),
    soft(api('/interfaces')),
    soft(api('/virtual-machine-functions')),
    soft(api('/container-functions')),
    soft(api('/alarms')),
    soft(api('/virtual-switches')),
    soft(api('/images')),
    soft(loadNetworkObjects()),
    soft(api('/audit-logs?limit=50')),
    soft(api('/vpp/capture')),
  ]);
  const ifaceRows = Array.isArray(ifaces) ? await loadInterfaceStats(ifaces) : [];
  const vsRows = Array.isArray(vss) ? await loadVSwitchStats(vss) : [];
  const vmStats = Array.isArray(vms) ? await loadVMStats(vms) : {};
  render(st, ver, vpp, pools, ifaces, ifaceRows, vms, cts, alarms);
  renderVSwitches(vss, vsRows);
  renderImages(imgs);
  renderNetworkObjects(nets);
  renderAudit(audit);
  renderCapture(cap);
  renderVMStats(vms, vmStats);
}

// 虚拟交换机：列表取自运行态（/virtual-switches 是配置视图，统计在详情上）。
async function loadVSwitchStats(vss) {
  const head = vss.slice(0, MAX_IFACE_DETAIL);
  const details = await Promise.all(head.map((v) => api('/virtual-switches/' + encodeURIComponent(v.name)).catch(() => null)));
  return head.map((v, n) => ({ cfg: v, stat: (details[n] || {}).statistics || null }));
}

// 每台 VM 的 vhost-user 口计数（在详情端点上）。
async function loadVMStats(vms) {
  const out = {};
  await Promise.all(vms.slice(0, MAX_IFACE_DETAIL).map(async (vm) => {
    const d = await api('/virtual-machine-functions/' + encodeURIComponent(vm.name)).catch(() => null);
    if (d && d.statistics) out[vm.name] = d.statistics;
  }));
  return out;
}

// 网络对象：一次拉齐只读视图（每个端点各自降级，缺一个不影响其余）。
async function loadNetworkObjects() {
  const [vrfs, acls, nat, bonds, lldp, qos, span] = await Promise.all([
    soft(api('/vrfs')),
    soft(api('/acls')),
    soft(api('/nat')),
    soft(api('/bonds')),
    soft(api('/protocols/lldp/neighbors')),
    soft(api('/qos/policies')),
    soft(api('/port-mirroring')),
  ]);
  return { vrfs, acls, nat, bonds, lldp, qos, span };
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

  renderVMRows(vms);
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

// ---------- 诊断（日志 + 连通性测试） ----------
//
// 与 CLI 同源同口径：ping 只覆盖数据面（VPP），**未通即失败**（0 发包/0 应答都算失败，
// 原始回显照原样展示以便排查）；日志取服务端尾部（与 show log system 同源）。
// 日志不随 5 秒轮询刷新（按需点按钮），避免无谓的重复拉取。

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

// ---------- 虚拟机：行内生命周期动作 + vhost-user 口计数 ----------

// 状态 → 允许的动作（与 CLI 同口径：运行中可 stop/restart，关机态可 start）。
const VM_ACTIONS = [
  { key: 'start', label: '启动', states: ['shutoff', 'crashed', '-'] },
  { key: 'stop', label: '停止', states: ['running', 'paused'] },
  { key: 'restart', label: '重启', states: ['running', 'paused'] },
];

function renderVMRows(vms) {
  const tbody = $('vm-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = vms || [];
  $('vm-note').textContent = rows.length ? '（' + rows.length + ' 台；动作按钮按当前状态启用）' : '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((v) => {
    const tr = el('tr');
    [v.name, v.state, v.vcpu ? v.vcpu.count : undefined,
      v.memory ? mb(v.memory.size_mb) : undefined, v.image].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    VM_ACTIONS.forEach((a) => {
      const btn = el('button', { type: 'button', class: 'ghost small', text: a.label });
      btn.disabled = a.states.indexOf(String(v.state)) < 0;
      btn.addEventListener('click', () => vmAction(v.name, a.key, a.label));
      cell.appendChild(btn);
    });
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

async function vmAction(name, action, label) {
  if (!window.confirm(label + '虚拟机 ' + name + '？运行中的业务会中断。')) return;
  opsMsg(label + ' ' + name + '：执行中…', false);
  try {
    await api('/virtual-machine-functions/' + encodeURIComponent(name) + ':' + action, { method: 'POST' });
    opsMsg(label + ' ' + name + '：已受理。', false);
  } catch (e) {
    opsMsg(label + ' ' + name + ' 失败：' + e.message, true);
  }
  await loadAll().catch(() => {});
}

function renderVMStats(vms, stats) {
  const pre = $('vm-stat');
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

// ---------- 镜像仓库 ----------

function renderImages(imgs) {
  table($('img-table').querySelector('tbody'), 4, (imgs || []).map((i) => [
    i.name, i.type, i.size_bytes ? bytes(i.size_bytes) : undefined, i.ref_count,
  ]));
}

// ---------- 网络对象（只读总览）----------

// 每块：[标题, 数据, 列名, 取值函数]；数据缺席（读取失败）时该块显示原因。
const NET_OBJECT_VIEWS = [
  ['VRF（L3 虚拟交换机）', 'vrfs', ['名称', 'L3 接口', '路由数'], (v) => [
    v.name,
    (v.l3_interfaces || []).map((i) => i.interface).join(', '),
    v.routes != null ? v.routes : undefined,
  ]],
  ['ACL', 'acls', ['名称', '规则数'], (a) => [a.name, (a.rules || []).length]],
  // NAT 是对象（source_pools/rules/static），按池与规则各出一行
  ['NAT', 'nat', ['类型', '内容'], (n) => [n.kind, n.summary]],
  ['链路聚合（bond）', 'bonds', ['名称', '模式', '成员'], (b) => [
    b.name, b.mode, (b.members || []).join(', '),
  ]],
  ['LLDP 邻居', 'lldp', ['本地口', '邻居', '管理地址'], (n) => [
    n.local_interface || n.interface, n.system_name || n.chassis_id, n.management_address,
  ]],
  ['QoS 策略', 'qos', ['名称', '类型', '目标'], (q) => [q.name, q.type, q.target || q.interface]],
  ['端口镜像（SPAN）', 'span', ['名称', '源', '目的'], (s) => [
    s.name, list(s.sources || s.source), s.destination,
  ]],
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
  NET_OBJECT_VIEWS.forEach(([title, key, cols, pick]) => {
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
    thead.appendChild(htr);
    t.appendChild(thead);
    const tbody = el('tbody');
    if (!rows.length) {
      const tr = el('tr');
      tr.appendChild(el('td', { colspan: String(cols.length), class: 'muted', text: '（无）' }));
      tbody.appendChild(tr);
    } else {
      rows.forEach((r) => {
        const tr = el('tr');
        pick(r).forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
        tbody.appendChild(tr);
      });
    }
    t.appendChild(tbody);
    wrap.appendChild(t);
    box.appendChild(wrap);
  });
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
    btn.addEventListener('click', () => downloadCapture(f.name));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// downloadCapture 带 Authorization 取文件再触发浏览器下载（<a href> 带不了请求头）。
async function downloadCapture(name) {
  capMsg('下载 ' + name + '：准备中…', false);
  try {
    const res = await fetch(API + '/vpp/capture/' + encodeURIComponent(name), {
      headers: { Authorization: 'Bearer ' + token },
    });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = el('a', { href: url, download: name });
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    capMsg('已触发下载：' + name + '（' + bytes(blob.size) + '）', false);
  } catch (e) {
    capMsg('下载失败：' + e.message, true);
  }
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
  await loadAll().catch(() => {});
}

// ---------- 启动 ----------

$('login-form').addEventListener('submit', doLogin);
$('logout-btn').addEventListener('click', doLogout);
$('refresh-btn').addEventListener('click', () => loadAll().catch((e) => showGlobalError(e.message)));
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
