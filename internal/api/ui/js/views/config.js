// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 配置域：candidate → 提交全流程 / 表单引擎 / 提交历史 / 编辑锁会话。

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
const jsviewsconfigRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsconfigRouterModule()).render();
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
// （跨模块访问经下方访问器：boot.js 的编辑态探针与登录复位走这里，不直接 import let。）
  committed: null, // {configuration, revision}
  editing: false,
  // 由「提交历史」页取候选后落回本页时记下偏移（> 0 时本页载入候选并直接展示差异）。
  landedFromRollback: 0,
};

const CFG_TEXT_ID = 'cfg-text';

// —— 口令派生失败的登记 + 保存前的闸门 ——
//
// 口令控件填明文时，明文在浏览器内就地派生成加盐哈希才写进 candidate。派生可能失败
// （控制台不是安全上下文、没有 Web Crypto；或口令不合策略、原始 JSON 有语法错），此时
// **这次保存必须整体失败**：若只是不写这个字段而照常保存，服务端收到的就是一个没有
// password_hash 的用户条目，界面还报「已保存到 candidate」——操作者以为改了口令，实际
// 什么也没改（假绿）。故派生失败的控件登记在这里，保存前统一拦下，原因照实说。
const cfgPwBlocked = new Map();  // 控件 id → { text: 原因（给操作者看）, secure: 是否因非安全上下文 }
const cfgPwInflight = new Set(); // 正在派生的任务：保存前先等它们落地（免得刚点保存、派生才失败）
const cfgPwFields = new Map();   // 控件 id → { inp, derive }：表单里的口令控件（保存前统一收口）
const CFG_PW_SECURE_HINT = '请用 HTTPS（或 localhost）打开控制台后重试。';

// cfgPwResetAll 表单重画 / 结束编辑时清账：旧控件都没了，失败登记与控件表一起作废——
// 否则一个已不存在的控件会永远拦住保存。
function cfgPwResetAll() {
  cfgPwFields.clear();
  cfgPwBlocked.clear();
}

// cfgPwGate 保存前的闸门。先等正在派生的落地，再把「填了口令却没派生出哈希」的控件拦下；
// 控件里还留着明文的（程序化填值、或打完没离开控件）就地派生并清空——保存的语义就是
// 「这份口令要写进去」，不能因为没失焦就把它丢掉。任一步失败即返回 false，
// 调用方据此**整次保存失败**，不写 candidate。
async function cfgPwGate() {
  while (cfgPwInflight.size) await Promise.allSettled(Array.from(cfgPwInflight));
  for (const f of Array.from(cfgPwFields.values())) {
    if (f.inp.value.trim() === '') continue;
    await f.derive(true);
  }
  if (!cfgPwBlocked.size) return true;
  const items = Array.from(cfgPwBlocked.values());
  const why = items.map((x) => x.text).join('；');
  const hint = items.some((x) => x.secure) ? CFG_PW_SECURE_HINT : '请修正后重试。';
  cfgMsg('保存失败（未写入 candidate）：' + why + '——' + hint +
    '（这次不需要改口令的话，把口令控件清空即可保存。）', true);
  return false;
}

// 说明：系统段的字段全部由下面的 CFG_SYS_SECTIONS 声明（与网络域/计算域同一套表单引擎，
// 见「各节定义」）。此处只留事务级的公共函数。

function cfgMsg(text, isErr) {
  const n = $('cfg-msg');
  n.textContent = text || '';
  n.hidden = !text;
  n.className = isErr ? 'error small' : 'muted small';
}

export function cfgSetEditing(on) {
  cfg.editing = on;
  $('cfg-read').hidden = on;
  $('cfg-edit').hidden = !on;
  $('cfg-diff').hidden = true;
  $('cfg-commit-confirmed-btn').hidden = true;
  $('cfg-confirm-btn').hidden = true;
  if (!on) cfgPwResetAll(); // 离开编辑态：口令控件的登记一并作废
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

// 表单渲染（顺序即界面顺序）：系统段（基础 / API 与 TLS / 管理口 / 内核启动参数 / 日志 /
// 健康阈值 / 登录与口令策略）→ 接口 → 资源池 → VPP 段 → 网络域与计算域各节。
// 系统段、VPP 段与网络域/计算域**共用同一套节定义与写路径**（见 CFG_SYS_SECTIONS /
// CFG_VPP_SECTIONS / CFG_SECTIONS）；接口与资源池是更早的两节，走下面的 addField。
// 全部都是"改字段 → 写回 JSON 文本 → 保存到 candidate"，提交才生效。
function cfgRenderForms(c) {
  const box = $('cfg-forms');
  box.textContent = '';
  cfgPwResetAll(); // 表单重画：口令控件的登记随之作废
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

  // ---- 系统段 ----
  cfgRenderList(box, CFG_SYS_SECTIONS, c);

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
      addField(sub, '入向限速策略', it.ingress_policy,
        (e) => cfgApplyIface(it.name, 'ingress_policy', e.target.value));
      addField(sub, '出向限速策略', it.egress_policy,
        (e) => cfgApplyIface(it.name, 'egress_policy', e.target.value));
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

  // ---- VPP 段 ----
  cfgRenderList(box, CFG_VPP_SECTIONS, c);

  // ---- 网络域与计算域（虚拟交换机 / VRF 与路由 / ACL / NAT / QoS / 端口镜像 / 聚合 / LLDP /
  //      虚拟机 / 容器）：节定义见下面的 CFG_SECTIONS，写路径与上面各节完全相同 ----
  cfgRenderDomainForms(box, c);
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

// ---------- 配置表单：网络域与计算域 ----------
//
// 上面三节（系统 / 接口 / 资源池）之外的配置都落在这里：网络域的虚拟交换机、VRF 与路由、ACL、
// NAT、QoS、端口镜像、链路聚合、LLDP，计算域的虚拟机与容器。写路径与那三节**完全同一套**：
// 改字段 → 写回「原始 JSON」文本区 → 「保存到 candidate」→ 预校验 / 差异 / 提交，
// 不另开通道，也不直接生效（提交才生效）。
//
// 三条口径：
//   ① 定位数组元素用**标识字段**（名字 / seq / 网段），不用数组下标——文本区可被手工编辑，下标会漂；
//   ② 条目标识等同命令行的路径参数，表单里**不可改**（改标识 = 删除后重建）：否则这一行在改完
//      标识后就找不到自己，而"定位失败"是静默的，那才是最坏的形态；
//   ③ 本地判得了的先判——IP/CIDR、端口与端口范围、VLAN 与整数上下限、三选一互斥、
//      新增时标识非空且不重名；判不过**不写入** candidate 并说明。引用关系（交换机/ACL/镜像/
//      策略是否存在）、必填项与容量余量交给服务端预校验（与提交用的是同一套校验）。

// 控件 id 计数器：前缀与上面三节（`cfg-f-`）**分开**——同一份 DOM 里 id 不能重名，
// 否则 label 的 for 会指到别人的输入框（点标签跳错字段、自动化也取错元素）。
let cfgSeq = 0;

// —— 本地格式判据（与服务端同口径的形态检查；语义合法性最终仍由服务端裁决）——

function cfgIsIPv4(s) {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
  return !!m && m.slice(1, 5).every((x) => Number(x) <= 255);
}

function cfgIsIPv6(s) {
  return s.indexOf(':') > 0 && /^[0-9a-fA-F:.]+$/.test(s);
}

function cfgIsIP(s) { return cfgIsIPv4(s) || cfgIsIPv6(s); }

function cfgIsCIDR(s) {
  const i = s.lastIndexOf('/');
  if (i <= 0) return false;
  const ip = s.slice(0, i), bits = s.slice(i + 1);
  if (!/^\d+$/.test(bits) || !cfgIsIP(ip)) return false;
  return Number(bits) <= (cfgIsIPv6(ip) ? 128 : 32);
}

function cfgIsMAC(s) { return /^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$/.test(s); }

function cfgIsPortSpec(s) {
  const m = /^(\d+)(?:-(\d+))?$/.exec(s);
  if (!m) return false;
  const lo = Number(m[1]), hi = m[2] === undefined ? lo : Number(m[2]);
  return lo >= 1 && lo <= 65535 && hi >= lo && hi <= 65535;
}

const cfgTextFmt = (v) => (v === undefined || v === null ? '' : String(v));

// —— 口令派生（用户口令字段）——
//
// 模型只存 `password_hash`（命令行语句也是先哈希再落库），所以表单**绝不能把明文写进
// password_hash**——那会变成明文口令落进配置库与审计。故：明文在浏览器里就地派生成
// 与命令行同一格式的加盐哈希，再写进 candidate；明文既不进 candidate，也不留在页面上。
//
// 格式与服务端一致：pbkdf2$sha256$<迭代数>$<b64盐>$<b64哈希>（盐 16 字节、密钥 32 字节、
// 无填充 base64）。迭代数与密钥长度取服务端同一组值，哈希里自带迭代数，故服务端能校验。
const CFG_PBKDF2_ITERATIONS = 600000;
const CFG_PW_HASH_RE = /^pbkdf2\$sha256\$\d+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$/;

function cfgB64Raw(bytes) {
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/=+$/, '');
}

async function cfgHashPassword(pw) {
  const c = globalThis.crypto || {};
  // Web Crypto 只在安全上下文可用（HTTPS 或 localhost）：明文 HTTP 打开的页面里没有它，
  // 此时**拒绝**而不是退化成明文写入。这个错要能被保存前的闸门认出来（区别于口令不合策略）。
  if (!c.subtle || !c.getRandomValues) {
    const err = new Error('本页面不能派生口令哈希（浏览器只在 HTTPS 或 localhost 下提供该能力）——' +
      '口令请改用命令行或用户管理入口设置');
    err.cfgSecureContext = true;
    throw err;
  }
  const salt = new Uint8Array(16);
  c.getRandomValues(salt);
  const key = await c.subtle.importKey('raw', new TextEncoder().encode(pw), 'PBKDF2', false, ['deriveBits']);
  const bits = await c.subtle.deriveBits(
    { name: 'PBKDF2', hash: 'SHA-256', salt, iterations: CFG_PBKDF2_ITERATIONS }, key, 256);
  return 'pbkdf2$sha256$' + CFG_PBKDF2_ITERATIONS + '$' +
    cfgB64Raw(salt) + '$' + cfgB64Raw(new Uint8Array(bits));
}

// cfgPasswordPolicyIssues 本地复刻服务端口令策略判据（长度按**字节**、复杂度 ≥3/4 类字符）：
// 明文只在这里出现，等服务端看到时已是哈希，服务端无从再判——所以策略必须在派生**之前**判。
function cfgPasswordPolicyIssues(c, pw) {
  const pol = (((c || {}).system || {}).login || {}).password_policy || {};
  const minLen = pol.min_length > 0 ? pol.min_length : 8;
  const bad = [];
  if (new TextEncoder().encode(pw).length < minLen) bad.push('长度不足 ' + minLen + ' 字符');
  if (pol.complexity) {
    const kinds = [/\p{Lu}/u, /\p{Ll}/u, /\p{Nd}/u, /[\p{P}\p{S}]/u].filter((re) => re.test(pw)).length;
    if (kinds < 3) bad.push('复杂度不足（需至少 3/4 类：大写/小写/数字/特殊字符）');
  }
  return bad;
}

// cfgPreparePassword 控件值 → 可写入的值：已是哈希则原样接受（load/克隆语义，与命令行一致）。
async function cfgPreparePassword(text) {
  if (text.startsWith('pbkdf2$')) return text;
  let c;
  try {
    c = cfgText();
  } catch (e) {
    throw new Error('原始 JSON 语法错误，请先修正：' + e.message);
  }
  const bad = cfgPasswordPolicyIssues(c, text);
  if (bad.length) throw new Error('口令不满足策略：' + bad.join('；'));
  return await cfgHashPassword(text);
}

// cfgPwDerive 派生一次口令（控件的 prepare）：成功返回哈希（clear 为真时顺带清空控件，
// 明文不留在页面上）并解除登记；失败登记原因并返回 null（登记后这次保存会被 cfgPwGate 拦下）。
// 只认「控件里的值仍是这次派生用的那份」的结果——操作者边打边改时，先完成的那次派生
// 既不会清掉后来输入的内容，也不会覆盖它的结果。
async function cfgPwDerive(id, inp, spec, pw, prepare, clear) {
  let hash;
  try {
    hash = await prepare(pw);
  } catch (e) {
    if (inp.value === pw) {
      cfgPwBlocked.set(id, { text: spec.label + '：' + e.message, secure: !!e.cfgSecureContext });
      // 立刻说明，别等到点保存才知道；保存那次还会再拦一次（见 cfgPwGate）
      cfgMsg(spec.label + '：' + e.message + '——未写入 candidate，保存会被拒绝。' +
        (e.cfgSecureContext ? CFG_PW_SECURE_HINT : ''), true);
    }
    return null;
  }
  if (inp.value !== pw) return null; // 期间又改了：这次结果作废，等最后一次派生的结果
  cfgPwBlocked.delete(id);
  if (clear) inp.value = '';
  return hash;
}

// 字段类型：控件形态 + 文本 ↔ 配置值的转换。空串一律表示「删除该字段」（与上面三节同口径）。
// parse 返回 { value } 或 { error }（error 时调用方**不写入** candidate）。
const CFG_KINDS = {
  text: { fmt: cfgTextFmt, parse: (t) => ({ value: t.trim() }) },
  // 多行文本（user-data / 环境变量）：不 trim——缩进与换行是内容的一部分
  raw: { fmt: cfgTextFmt, multiline: true, parse: (t) => ({ value: t.replace(/\s+$/, '') }) },
  int: {
    fmt: cfgTextFmt,
    parse: (t, spec) => {
      const s = t.trim();
      if (!/^\d+$/.test(s)) return { error: '必须是整数' };
      const n = Number(s);
      if (spec.min !== undefined && n < spec.min) return { error: '不得小于 ' + spec.min };
      if (spec.max !== undefined && n > spec.max) return { error: '不得大于 ' + spec.max };
      return { value: n };
    },
  },
  bool: {
    fmt: (v) => (v === true ? 'true' : (v === false ? 'false' : '')),
    parse: (t) => ({ value: t === '' ? '' : t === 'true' }),
    options: [['true', '启用'], ['false', '禁用']],
  },
  select: { fmt: cfgTextFmt, parse: (t) => ({ value: t }) },
  ip: {
    fmt: cfgTextFmt,
    parse: (t) => (cfgIsIP(t.trim()) ? { value: t.trim() } : { error: '必须是 IP 地址（如 192.168.1.1）' }),
  },
  cidr: {
    fmt: cfgTextFmt,
    parse: (t) => (cfgIsCIDR(t.trim()) ? { value: t.trim() } : { error: '必须是 ip-prefix（如 10.0.0.0/8）' }),
  },
  // ACL 的源/目的：ip-prefix 或 any
  ipAny: {
    fmt: cfgTextFmt,
    parse: (t) => {
      const s = t.trim();
      if (s === 'any') return { value: s };
      return cfgIsCIDR(s) ? { value: s } : { error: '必须是 ip-prefix（如 10.0.0.0/8）或 any' };
    },
  },
  mac: {
    fmt: cfgTextFmt,
    parse: (t) => (cfgIsMAC(t.trim()) ? { value: t.trim() } : { error: 'MAC 形如 52:54:00:aa:bb:cc' }),
  },
  port: {
    fmt: cfgTextFmt,
    parse: (t) => (cfgIsPortSpec(t.trim()) ? { value: t.trim() }
      : { error: '必须是端口或端口范围（如 80 或 1024-65535）' }),
  },
  intlist: {
    fmt: (v) => (Array.isArray(v) ? v.join(',') : ''),
    parse: (t, spec) => {
      const s = t.trim();
      if (!s) return { value: '' };
      const out = [];
      for (const part of s.split(',')) {
        const p = part.trim();
        if (!/^\d+$/.test(p)) return { error: '只能是整数，用逗号分隔（如 100,200）' };
        const n = Number(p);
        if (spec.min !== undefined && n < spec.min) return { error: p + ' 小于 ' + spec.min };
        if (spec.max !== undefined && n > spec.max) return { error: p + ' 大于 ' + spec.max };
        out.push(n);
      }
      return { value: out };
    },
  },
  strlist: {
    fmt: (v) => (Array.isArray(v) ? v.join(',') : ''),
    parse: (t) => {
      const out = t.split(',').map((x) => x.trim()).filter((x) => x !== '');
      return { value: out.length ? out : '' };
    },
  },
  cidrlist: {
    fmt: (v) => (Array.isArray(v) ? v.join(', ') : ''),
    parse: (t) => {
      if (!t.trim()) return { value: '' };
      const out = [];
      for (const part of t.split(',')) {
        const p = part.trim();
        if (!p) continue;
        if (!cfgIsCIDR(p)) return { error: p + ' 不是合法的 ip-prefix（如 192.168.100.1/24）' };
        out.push(p);
      }
      return { value: out.length ? out : '' };
    },
  },
  // NAT 源地址池的范围写法（模型是 "<ip> to <ip>" 一个字符串）
  addrRange: {
    fmt: cfgTextFmt,
    parse: (t) => {
      const s = t.trim();
      const parts = s.split(/\s+to\s+/i);
      if (parts.length !== 2 || !cfgIsIP(parts[0].trim()) || !cfgIsIP(parts[1].trim())) {
        return { error: '写成 "<起始 IP> to <结束 IP>"（如 192.168.155.220 to 192.168.155.225）' };
      }
      return { value: parts[0].trim() + ' to ' + parts[1].trim() };
    },
  },
  // 逗号分隔的 IP 列表（DNS 服务器这类）
  iplist: {
    fmt: (v) => (Array.isArray(v) ? v.join(', ') : ''),
    parse: (t) => {
      const out = [];
      for (const part of t.split(',')) {
        const p = part.trim();
        if (!p) continue;
        if (!cfgIsIP(p)) return { error: p + ' 不是 IP 地址（多条用逗号分隔，如 8.8.8.8, 1.1.1.1）' };
        out.push(p);
      }
      return { value: out.length ? out : '' };
    },
  },
  // 每行一条的字符串列表（内核参数：值里可能自带逗号，故不按逗号拆）
  lines: {
    fmt: (v) => (Array.isArray(v) ? v.join('\n') : ''),
    multiline: true,
    parse: (t) => {
      const out = t.split('\n').map((x) => x.trim()).filter((x) => x !== '');
      return { value: out.length ? out : '' };
    },
  },
  // VPP 的 worker 核列表（如 5,7,9-11）：形态与区间方向本地判，是否落在隔离核池由服务端判
  corelist: {
    fmt: cfgTextFmt,
    parse: (t) => {
      const s = t.replace(/\s+/g, '');
      if (!/^\d+(-\d+)?(,\d+(-\d+)?)*$/.test(s)) return { error: '形如 5,7,9-11（逗号分隔的核号或核号区间）' };
      for (const part of s.split(',')) {
        const [lo, hi] = part.split('-');
        if (hi !== undefined && Number(lo) > Number(hi)) {
          return { error: '核区间 ' + part + ' 不合法（起始核号应不大于结束核号）' };
        }
      }
      return { value: s };
    },
  },
  // VPP 主堆大小（如 1G / 512M）
  heapSize: {
    fmt: cfgTextFmt,
    parse: (t) => (/^\d+\s*[KMG]$/i.test(t.trim())
      ? { value: t.trim().toUpperCase() }
      : { error: '形如 1G 或 512M' }),
  },
  // 口令（写 password_hash）：留空 = 不改动；填明文 → 浏览器就地派生成加盐哈希再写入；
  // 填 pbkdf2$… 哈希 → 直接写入（与命令行 load/克隆的口径一致）。明文永不进 candidate。
  password: {
    fmt: () => '', // 口令哈希是敏感叶子，配置视图本就不返回——表单也不回显
    emptyNoop: true,
    prepare: cfgPreparePassword,
    parse: (t) => (CFG_PW_HASH_RE.test(t) ? { value: t }
      : { error: '口令哈希格式不合法（应为 pbkdf2$sha256$…）' }),
  },
  // 键值对（容器的环境变量）：每行 KEY=VALUE
  kvmap: {
    fmt: (v) => {
      const m = v || {};
      return Object.keys(m).map((k) => k + '=' + m[k]).join('\n');
    },
    multiline: true,
    parse: (t) => {
      const out = {};
      for (const line of t.split('\n')) {
        const s = line.trim();
        if (!s) continue;
        const i = s.indexOf('=');
        if (i <= 0) return { error: '每行写成 KEY=VALUE（如 TEST_KEY=1）' };
        const k = s.slice(0, i).trim();
        if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(k)) return { error: '变量名 ' + k + ' 不合法（字母或下划线开头）' };
        out[k] = s.slice(i + 1).trim();
      }
      return { value: Object.keys(out).length ? out : '' };
    },
  },
};

// 互斥字段组：模型里"三选一 / 二选一"的字段。写其中一个之前先确认同组其余为空——
// 本地判得了的规则不留给服务端（提交时才报错就白改一轮）。
const CFG_GROUPS = {
  portKind: [
    { path: ['interface'], label: '物理口 / bond' },
    { path: ['vnf'], label: 'VNF' },
    { path: ['container'], label: '容器' },
  ],
  pmSource: [
    { path: ['source', 'interface'], label: '源：物理口 / bond' },
    { path: ['source', 'vnf'], label: '源：VNF' },
  ],
  // VPP 的 worker 分配方式二选一：显式核列表，或按 NUMA 自动分配（服务端也这么判）
  vppWorkers: [
    { path: ['cpu', 'corelist_workers'], label: 'worker 核列表' },
    { path: ['cpu', 'workers_per_numa'], label: '每 NUMA worker 数' },
  ],
};

// —— 定位器：从文档根逐级下潜到要改的对象 ——
// 每级两种形态之一：{ list, key, val } 取数组里标识字段相符的元素；{ obj } 取对象字段。
// 空数组 = 文档根本身（单对象节，如 NAT）。
function cfgLocate(c, loc, create) {
  let cur = c;
  for (const step of loc || []) {
    if (!cur) return null;
    if (step.obj) {
      if (!cur[step.obj] || typeof cur[step.obj] !== 'object') {
        if (!create) return null;
        cur[step.obj] = {};
      }
      cur = cur[step.obj];
      continue;
    }
    const arr = cur[step.list];
    if (!Array.isArray(arr)) return null;
    const found = arr.find((x) => x && x[step.key] === step.val);
    if (!found) return null;
    cur = found;
  }
  return cur || null;
}

function cfgRead(c, loc, path) {
  let cur = cfgLocate(c, loc, false);
  for (const k of path) {
    if (cur === null || cur === undefined) return undefined;
    cur = cur[k];
  }
  return cur;
}

function cfgEmptyVal(v) {
  if (v === null || v === undefined || v === '') return true;
  if (Array.isArray(v)) return v.length === 0;
  return typeof v === 'object' && Object.keys(v).length === 0;
}

// 写一个字段（'' = 删除），并把写空后残留的空容器一并清掉——否则差异里会多出
// `"gateway": {}`、`"lacp": {}` 这类噪声，看着像改了其实什么都没配。
function cfgWrite(c, loc, path, value) {
  const obj = cfgLocate(c, loc, true);
  if (!obj) return false;
  const chain = [obj];
  let cur = obj;
  for (let i = 0; i < path.length - 1; i++) {
    const k = path[i];
    if (cur[k] === null || typeof cur[k] !== 'object') cur[k] = {};
    cur = cur[k];
    chain.push(cur);
  }
  const last = path[path.length - 1];
  if (value === '' || value === undefined || value === null) delete cur[last];
  else cur[last] = value;
  for (let i = chain.length - 1; i > 0; i--) {
    const parent = chain[i - 1], key = path[i - 1];
    if (cfgEmptyVal(parent[key])) delete parent[key];
  }
  return true;
}

// 容器 = 「某个对象上的某个数组字段」：{ loc, list }（loc 为空 = 文档根）。
// 返回**持有该数组的对象**（create=true 时按需补出中间对象与空数组）。
function cfgHolder(c, cont, create) {
  let base = c;
  for (const step of cont.loc || []) {
    if (step.obj) {
      if (!base[step.obj] || typeof base[step.obj] !== 'object') {
        if (!create) return null;
        base[step.obj] = {};
      }
      base = base[step.obj];
      continue;
    }
    const arr = base[step.list];
    if (!Array.isArray(arr)) return null;
    base = arr.find((x) => x && x[step.key] === step.val);
    if (!base) return null;
  }
  if (!Array.isArray(base[cont.list])) {
    if (!create) return null;
    base[cont.list] = [];
  }
  return base;
}

function cfgApplyMsg(ok, text) {
  cfgMsg(text, !ok);
  return ok;
}

// 表单改动 → candidate：解析文本区（保留手工编辑）→ 本地判 → 写一个字段 → 写回文本区。
function cfgApplyField(loc, spec, text) {
  const kind = CFG_KINDS[spec.kind];
  // 少数控件「留空 = 不改动」而非「删除」（口令：删掉哈希会让账号失去口令，服务端也会拒）
  if (text.trim() === '' && kind.emptyNoop) {
    return cfgApplyMsg(true, spec.label + '：留空表示不改动（未写入 candidate）。');
  }
  let c;
  try {
    c = cfgText();
  } catch (e) {
    return cfgApplyMsg(false, '原始 JSON 语法错误，请先修正：' + e.message);
  }
  // 空（含只有空白）一律表示「删除该字段」——清空即删除，与上面三节同口径
  const r = text.trim() === '' ? { value: '' } : kind.parse(text, spec);
  if (r.error) return cfgApplyMsg(false, spec.label + '：' + r.error + '——未写入 candidate，请修正后重试。');
  if (spec.group && r.value !== '') {
    const clash = (CFG_GROUPS[spec.group] || []).find((g) =>
      g.path.join('.') !== spec.path.join('.') && !cfgEmptyVal(cfgRead(c, loc, g.path)));
    if (clash) {
      return cfgApplyMsg(false, spec.label + '与「' + clash.label +
        '」互斥（模型里只能指定一个）——请先清空后者，未写入 candidate。');
    }
  }
  if (!cfgWrite(c, loc, spec.path, r.value)) {
    return cfgApplyMsg(false, '定位不到要改的对象（配置可能已被改动）——请刷新后重试。');
  }
  // 伴生字段：清掉主字段时一并清掉它的从属字段（如清了 VNF 名，它的 vNIC 名也不该留着）
  if (r.value === '' && spec.clears) spec.clears.forEach((p) => cfgWrite(c, loc, p, ''));
  cfgWriteText(c);
  return cfgApplyMsg(true, '已写入 candidate（未保存）——点「保存到 candidate」提交到服务端。');
}

// 新增条目（= 对应的 set 语句，只改 candidate）：标识先本地判（非空、不重名）。
function cfgAdd(cont, spec, rawVal) {
  const kind = CFG_KINDS[spec.keyKind || 'text'];
  const r = rawVal.trim() === '' ? { value: '' } : kind.parse(rawVal, spec);
  if (r.error) return cfgApplyMsg(false, spec.keyLabel + '：' + r.error + '——未新增。');
  if (r.value === '') return cfgApplyMsg(false, '请先填写' + spec.keyLabel + '。');
  let c;
  try {
    c = cfgText();
  } catch (e) {
    return cfgApplyMsg(false, '原始 JSON 语法错误，请先修正：' + e.message);
  }
  const holder = cfgHolder(c, cont, true);
  if (!holder) return cfgApplyMsg(false, '定位不到要新增到的位置——请刷新后重试。');
  if (holder[cont.list].some((x) => x && x[cont.key] === r.value)) {
    return cfgApplyMsg(false, spec.keyLabel + ' ' + r.value + ' 已存在——未新增。');
  }
  holder[cont.list].push(Object.assign({ [cont.key]: r.value }, spec.extra ? spec.extra() : {}));
  cfgWriteText(c);
  cfgRenderForms(c); // 结构变了：表单按新的 candidate 重画（已改的字段都在文本区里，不会丢）
  return cfgApplyMsg(true, '已在 candidate 中新增' + spec.what + ' ' + r.value +
    '（未保存）——提交后才生效。');
}

// 删除条目（= 对应的 delete 语句，只改 candidate）。
function cfgRemove(cont, val, what) {
  let c;
  try {
    c = cfgText();
  } catch (e) {
    cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true);
    return;
  }
  const holder = cfgHolder(c, cont, false);
  const i = holder ? holder[cont.list].findIndex((x) => x && x[cont.key] === val) : -1;
  if (i < 0) {
    cfgMsg('要删除的' + what + ' ' + val + '已不在 candidate 里——请刷新后重试。', true);
    return;
  }
  holder[cont.list].splice(i, 1);
  if (!holder[cont.list].length) delete holder[cont.list]; // 空数组是噪声，整段删掉
  cfgWriteText(c);
  cfgRenderForms(c);
  cfgMsg('已从 candidate 中删除' + what + ' ' + val + '（未保存）——提交后才生效。', false);
}

// 清空整节（单对象节：NAT / LLDP），= `delete nat` 一类语句。
function cfgClearSection(path, what) {
  let c;
  try {
    c = cfgText();
  } catch (e) {
    cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true);
    return;
  }
  let cur = c;
  for (let i = 0; i < path.length - 1; i++) cur = cur ? cur[path[i]] : undefined;
  if (!cur) {
    cfgMsg('candidate 里本来就没有' + what + '。', false);
    return;
  }
  delete cur[path[path.length - 1]];
  cfgWriteText(c);
  cfgRenderForms(c);
  cfgMsg('已从 candidate 中删除' + what + '（未保存）——提交后才生效。', false);
}

// 一个字段：label + 控件；改动即写回 candidate（本地判不过则不写并标红）。
function cfgRenderField(parent, c, loc, spec) {
  const cell = el('div', { class: 'cfg-field' });
  const id = 'cfg-df-' + (cfgSeq++);
  cell.appendChild(el('label', { text: spec.label, for: id }));
  const kind = CFG_KINDS[spec.kind];
  const options = spec.options || kind.options;
  const inp = kind.multiline
    ? el('textarea', { id, rows: '3', spellcheck: 'false' })
    : (options ? el('select', { id }) : el('input', { type: spec.kind === 'password' ? 'password' : 'text', id }));
  if (spec.kind === 'password') inp.setAttribute('autocomplete', 'new-password');
  if (options) {
    [['', '（未设置）']].concat(options).forEach(([v, t]) => {
      const opt = el('option', { value: v, text: t });
      if (kind.fmt(cfgRead(c, loc, spec.path)) === v) opt.setAttribute('selected', 'selected');
      inp.appendChild(opt);
    });
  } else {
    inp.value = kind.fmt(cfgRead(c, loc, spec.path));
  }
  if (spec.hint) inp.setAttribute('placeholder', spec.hint);
  // 与上面三节同口径：监听 input（程序化赋值也会触发）与 change（失焦/回车，下拉框也用它）
  //
  // kind.prepare（口令）：控件里的**明文**要就地派生成加盐哈希才写进 candidate，而派生是
  // **异步**的（几十~几百毫秒），input 却是每个键一次。三条口径：
  //   ① 输入过程中派生成功后**不清空**控件——清了会把还没打完的口令吃掉（后面敲的键落进空
  //      控件，最后写进 candidate 的会是半截口令的哈希，界面还报成功，操作者无从发现）；
  //   ② 离开控件（change）或点保存（见 cfgPwGate）时才清空——明文不留在页面上；
  //   ③ 派生失败（非安全上下文、口令不合策略）**保留明文**并登记，让这次保存整体失败。
  let prepared = false;
  const runPrepare = async (clear) => {
    const pw = inp.value;
    if (pw.trim() === '') return;
    cfgMsg(spec.label + '：正在本机派生根哈希…', false);
    const job = cfgPwDerive(id, inp, spec, pw, kind.prepare, clear);
    cfgPwInflight.add(job);
    let hash;
    try { hash = await job; } finally { cfgPwInflight.delete(job); }
    if (hash === null) {
      // 失败（或这次派生已被后来的输入作废）：明文仍在控件里，便于切到 HTTPS 后重试
      if (inp.value === pw) inp.className = 'invalid';
      return;
    }
    prepared = true;
    inp.className = cfgApplyField(loc, spec, hash) ? '' : 'invalid';
  };
  if (kind.prepare) cfgPwFields.set(id, { inp, derive: runPrepare });
  const onEdit = (ev) => {
    if (kind.prepare && inp.value.trim() !== '') {
      runPrepare(!!ev && ev.type === 'change'); // 失焦/回车 = 这个字段改完了，派生成功后清空
      return;
    }
    if (prepared) {
      prepared = false; // 清空引起的重复事件：保留上一次的结果提示
      return;
    }
    if (kind.prepare) cfgPwBlocked.delete(id); // 控件已空 = 这次保存不带口令，不必再拦
    inp.className = cfgApplyField(loc, spec, inp.value) ? '' : 'invalid';
  };
  inp.addEventListener('input', onEdit);
  inp.addEventListener('change', onEdit);
  cell.appendChild(inp);
  if (spec.note) cell.appendChild(el('p', { class: 'muted small', text: spec.note }));
  parent.appendChild(cell);
  return inp;
}

function cfgRowHead(text, onDelete, deleteLabel) {
  const head = el('div', { class: 'cfg-rowhead' });
  head.appendChild(el('span', { class: 'cfg-subname', text }));
  if (onDelete) {
    const btn = wbtn({ type: 'button', class: 'small danger', text: deleteLabel || '删除' });
    btn.addEventListener('click', onDelete);
    head.appendChild(btn);
  }
  return head;
}

// 新增行：标识输入 + 按钮（回车等同点按钮）。
function cfgRenderAdd(cont, spec) {
  const box = el('div', { class: 'cfg-add' });
  const id = 'cfg-df-' + (cfgSeq++);
  box.appendChild(el('label', { text: spec.keyLabel, for: id }));
  const inp = el('input', { type: 'text', id });
  if (spec.placeholder) inp.setAttribute('placeholder', spec.placeholder);
  box.appendChild(inp);
  const btn = wbtn({ type: 'button', class: 'small', text: spec.button || '新增' });
  btn.addEventListener('click', () => cfgAdd(cont, spec, inp.value));
  inp.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') { ev.preventDefault(); cfgAdd(cont, spec, inp.value); }
  });
  box.appendChild(btn);
  return box;
}

// 条目内的子表（成员端口 / 规则 / 路由 / vNIC / 数据盘…）：每条一块（标题 + 字段 + 删除），
// 底部一行新增。子表的数组字段挂在**条目**上（NAT 这类挂在对象上，见 sub.loc）。
function cfgRenderSub(sub, loc, c) {
  const wrap = el('div', { class: 'cfg-sub2' });
  const cont = { loc: loc.concat(sub.loc || []), list: sub.list, key: sub.key };
  wrap.appendChild(el('div', { class: 'cfg-subname', text: sub.title }));
  if (sub.note) wrap.appendChild(el('p', { class: 'muted small', text: sub.note }));
  // 子表挂在**持有它的对象**上：列表节里是条目本身，单对象节（NAT）里是那个对象
  const parent = cfgLocate(c, cont.loc, false);
  const rows = parent && Array.isArray(parent[sub.list]) ? parent[sub.list] : [];
  if (!rows.length) wrap.appendChild(el('p', { class: 'muted small', text: sub.empty || '（无）' }));
  rows.forEach((row) => {
    const rloc = cont.loc.concat([{ list: sub.list, key: sub.key, val: row[sub.key] }]);
    const box = el('div', { class: 'cfg-sub3' });
    box.appendChild(cfgRowHead(String(dash(row[sub.key])),
      () => cfgRemove(cont, row[sub.key], sub.what || sub.title)));
    const grid = el('div', { class: 'cfg-fields' });
    (sub.fields || []).forEach((spec) => cfgRenderField(grid, c, rloc, spec));
    box.appendChild(grid);
    wrap.appendChild(box);
  });
  wrap.appendChild(cfgRenderAdd(cont, sub));
  return wrap;
}

// 一个条目：标题（标识字段的值，不可改——等同命令行的路径参数）+ 字段 + 子表 + 删除。
function cfgRenderItem(sec, loc, it, c) {
  const wrap = el('div', { class: 'cfg-sub' });
  const cont = { loc: [], list: sec.list, key: sec.key };
  wrap.appendChild(cfgRowHead(sec.title(it), () => cfgRemove(cont, it[sec.key], sec.what)));
  const grid = el('div', { class: 'cfg-fields' });
  (sec.fields || []).forEach((spec) => cfgRenderField(grid, c, loc, spec));
  wrap.appendChild(grid);
  (sec.sublists || []).forEach((sub) => wrap.appendChild(cfgRenderSub(sub, loc, c)));
  return wrap;
}

// 渲染一节：fieldset（标题 + 说明）+ 条目 + 新增行（单对象节则直接是字段 + 子表 + 清空）。
function cfgRenderSection(box, sec, c) {
  const fs = el('fieldset', { class: sec.wide ? 'cfg-wide' : '' });
  fs.appendChild(el('legend', { text: sec.legend }));
  if (sec.note) fs.appendChild(el('p', { class: 'muted small', text: sec.note }));
  if (sec.list) {
    const items = Array.isArray(c[sec.list]) ? c[sec.list] : [];
    if (!items.length) fs.appendChild(el('p', { class: 'muted small', text: '（未配置）' }));
    items.forEach((it) => fs.appendChild(cfgRenderItem(sec, [{ list: sec.list, key: sec.key, val: it[sec.key] }], it, c)));
    fs.appendChild(cfgRenderAdd({ loc: [], list: sec.list, key: sec.key },
      Object.assign({ what: sec.what }, sec.add)));
  } else {
    // 单对象节（NAT / LLDP）：没有"条目"这层，字段与子表直接挂在节里
    const grid = el('div', { class: 'cfg-fields' });
    (sec.fields || []).forEach((spec) => cfgRenderField(grid, c, sec.loc || [], spec));
    if ((sec.fields || []).length) fs.appendChild(grid);
    (sec.sublists || []).forEach((sub) => fs.appendChild(cfgRenderSub(sub, sec.loc || [], c)));
    if (sec.clearPath) {
      const btn = wbtn({ type: 'button', class: 'small danger', text: '清空本节（' + sec.legend + '）' });
      btn.addEventListener('click', () => cfgClearSection(sec.clearPath, sec.legend + '配置'));
      const row = el('div', { class: 'cfg-add' });
      row.appendChild(btn);
      fs.appendChild(row);
    }
  }
  box.appendChild(fs);
}

// —— 各节定义（顺序即界面顺序）——
//
// 每条语句 ↔ 一个控件：`path` 就是模型里的字段路径（与 JSON 里看到的一致）。
// 只有标识字段（名字 / seq / 网段 / 接口名）不进 `fields`——它在标题上，靠「新增 / 删除」维护。
//
// 系统段与 VPP 段（CFG_SYS_SECTIONS / CFG_VPP_SECTIONS）用**同一套**声明：单对象节
// （loc 指向文档里的那个对象）+ 字段 + 子表 + 可选「清空本节」，与下面的网络域/计算域完全一致。

// RFC 5424 facility 取值（与服务端同一份名单）
const CFG_FACILITIES = ['kern', 'user', 'mail', 'daemon', 'auth', 'syslog', 'lpr', 'news', 'uucp',
  'cron', 'authpriv', 'ftp', 'local0', 'local1', 'local2', 'local3', 'local4', 'local5', 'local6', 'local7']
  .map((f) => [f, f]);

// 系统段（命令行 `set system …`）。危险/高影响项（证书路径、管理口地址、用户与口令策略）
// 与普通项一样只写 candidate——提交才生效，界面不提供任何绕过服务端校验与守卫的捷径。
const CFG_SYS_SECTIONS = [
  {
    legend: '系统（基础）',
    loc: [{ obj: 'system' }],
    note: '主机名 / 时区 / DNS / NTP / 命令行会话空闲超时。',
    fields: [
      { label: '主机名', path: ['hostname'], kind: 'text' },
      { label: '时区', path: ['timezone'], kind: 'text', hint: '如 Asia/Shanghai' },
      { label: 'DNS 服务器（逗号分隔）', path: ['dns_servers'], kind: 'iplist', hint: '如 8.8.8.8, 1.1.1.1' },
      { label: '会话空闲超时（分钟）', path: ['idle_timeout_minutes'], kind: 'int', min: 1 },
    ],
    sublists: [{
      list: 'ntp', key: 'server', keyKind: 'text', title: 'NTP 服务器', what: 'NTP 服务器',
      keyLabel: '服务器', placeholder: '如 10.0.0.1 或 ntp.example.com',
      note: '服务器地址可写 IP 或主机名；「首选」标记该服务器为首选源。',
      fields: [{ label: '首选', path: ['prefer'], kind: 'bool' }],
    }],
  },
  {
    legend: '系统 · API 与 TLS',
    loc: [{ obj: 'system' }, { obj: 'api' }],
    clearPath: ['system', 'api'],
    note: '控制面 API 的端口、令牌有效期、并发上限与证书/私钥的**文件路径**（证书内容由运维侧放到该路径，' +
      '本页只声明路径，不上传文件）。改完提交才生效。',
    fields: [
      { label: 'HTTPS 端口', path: ['port'], kind: 'int', min: 1, max: 65535 },
      { label: '令牌有效期（分钟）', path: ['token_ttl_minutes'], kind: 'int', min: 1 },
      { label: '并发会话上限', path: ['max_sessions'], kind: 'int', min: 1 },
      { label: '证书文件（PEM 路径）', path: ['cert_file'], kind: 'text', hint: '如 /etc/nfvis/tls/server.crt' },
      { label: '私钥文件（PEM 路径）', path: ['key_file'], kind: 'text', hint: '如 /etc/nfvis/tls/server.key' },
      { label: '使用自签证书', path: ['tls_self_signed'], kind: 'bool',
        note: '声明用自签证书（缺证书时由守护进程生成）。重签自签证书是运维动作，不在这里。' },
    ],
  },
  {
    legend: '系统 · 管理口',
    loc: [{ obj: 'system' }, { obj: 'management' }],
    clearPath: ['system', 'management'],
    note: '管理网卡与它的静态地址/网关。管理口不得用于数据面（服务端会拒绝）；' +
      '改地址或网关后提交时，服务端会要求以「commit confirmed」方式提交（避免把自己锁在门外），界面不做替代。',
    fields: [
      { label: '管理网卡', path: ['interface'], kind: 'text', hint: '如 ens160' },
      { label: '管理口地址（CIDR）', path: ['address'], kind: 'cidr', hint: '如 192.168.1.10/24' },
      { label: '管理口网关', path: ['gateway'], kind: 'ip', hint: '如 192.168.1.1' },
    ],
  },
  {
    legend: '系统 · 内核启动参数',
    loc: [{ obj: 'system' }, { obj: 'kernel' }],
    clearPath: ['system', 'kernel'],
    note: '内核启动基线（大页与隔离核的唯一真源是「资源池」节）。改动写入配置后需重启主机才生效。',
    fields: [
      { label: 'NMI watchdog', path: ['nmi_watchdog'], kind: 'bool' },
      { label: '透明大页', path: ['transparent_hugepages'], kind: 'select',
        options: [['always', 'always'], ['madvise', 'madvise'], ['never', 'never']] },
      { label: 'IOMMU', path: ['iommu'], kind: 'select',
        options: [['on', 'on'], ['off', 'off'], ['pt', 'pt（直通）']] },
      { label: '低延迟参数组', path: ['low_latency'], kind: 'bool' },
      { label: 'tuned 性能档', path: ['tuned_profile'], kind: 'text', hint: '如 nfvis-throughput' },
      { label: '附加内核参数（每行一条）', path: ['params'], kind: 'lines', hint: '如 mitigations=off' },
    ],
  },
  {
    legend: '系统 · 日志（syslog）',
    loc: [{ obj: 'system' }, { obj: 'syslog' }],
    clearPath: ['system', 'syslog'],
    note: '远程 syslog 目标与本地日志的级别、保留天数、容量上限（超上限滚动覆盖）。',
    fields: [
      { label: '远程主机', path: ['remote_host'], kind: 'ip', hint: '如 10.0.0.9' },
      { label: '远程端口', path: ['remote_port'], kind: 'int', min: 1, max: 65535 },
      { label: 'facility', path: ['facility'], kind: 'select', options: CFG_FACILITIES },
      { label: '远程转发级别', path: ['severity'], kind: 'select',
        options: [['debug', 'debug'], ['info', 'info'], ['warn', 'warn'], ['error', 'error']] },
      { label: '本地日志级别', path: ['level'], kind: 'select',
        options: [['debug', 'debug'], ['info', 'info'], ['warn', 'warn'], ['error', 'error']] },
      { label: '本地保留天数', path: ['retention_days'], kind: 'int', min: 0 },
      { label: '本地容量上限（MB）', path: ['max_size_mb'], kind: 'int', min: 1 },
    ],
  },
  {
    legend: '系统 · 健康阈值',
    loc: [{ obj: 'system' }, { obj: 'health' }, { obj: 'thresholds' }],
    clearPath: ['system', 'health'],
    note: '超过阈值即产生告警。留空 = 不设该阈值（不产生对应告警）。',
    fields: [
      { label: 'CPU 温度（℃）', path: ['cpu_temp_celsius'], kind: 'int', min: 1, max: 200 },
      { label: '磁盘温度（℃）', path: ['disk_temp_celsius'], kind: 'int', min: 1, max: 200 },
      { label: '磁盘使用率（%）', path: ['disk_used_percent'], kind: 'int', min: 1, max: 100 },
    ],
  },
  {
    legend: '系统 · 登录与口令策略',
    wide: true,
    loc: [{ obj: 'system' }, { obj: 'login' }],
    clearPath: ['system', 'login'],
    note: '本地用户、自定义 class 与口令策略。口令在本页就地派生成加盐哈希后才写入 candidate——' +
      '明文不进配置；离开口令控件（或点保存）时完成派生并清空控件，派生失败（例如控制台不是用 ' +
      'HTTPS 打开的）则保留输入，并让这次保存整体失败（不会写入没有口令的用户条目）。' +
      '已有用户不填口令即保持原口令不变。',
    fields: [
      { label: '口令最小长度', path: ['password_policy', 'min_length'], kind: 'int', min: 4, max: 128 },
      { label: '口令复杂度（≥3/4 类字符）', path: ['password_policy', 'complexity'], kind: 'bool' },
      { label: '口令有效期（天，0 = 不过期）', path: ['password_policy', 'expire_days'], kind: 'int', min: 0 },
      { label: '连续失败锁定阈值', path: ['password_policy', 'lockout_threshold'], kind: 'int', min: 1, max: 100 },
      { label: '锁定时长（分钟）', path: ['password_policy', 'lockout_minutes'], kind: 'int', min: 1, max: 10080 },
    ],
    sublists: [
      {
        list: 'users', key: 'name', keyKind: 'text', title: '本地用户', what: '用户',
        keyLabel: '用户名', placeholder: '如 ops',
        note: '新增用户后**必须立刻设置口令**——服务端不接受无口令账号（预校验/提交会拒）。',
        fields: [
          { label: '归属 class', path: ['class'], kind: 'text',
            hint: 'super-user / operator / read-only 或自定义 class 名' },
          { label: '设置口令（留空 = 不改）', path: ['password_hash'], kind: 'password',
            note: '填明文口令即在本机派生成加盐哈希写入；也可粘贴 pbkdf2$ 哈希（与配置导入同一口径）。' +
              '派生要用浏览器只在 HTTPS 或 localhost 下提供的能力——不满足时保存会被拒绝，输入会保留。' },
        ],
      },
      {
        list: 'classes', key: 'name', keyKind: 'text', title: '自定义 class', what: 'class',
        keyLabel: 'class 名', placeholder: '如 netops',
        note: 'allow / deny 是命令树节点前缀（逗号分隔，deny 优先）；预置 class 不必在此定义。',
        fields: [
          { label: '允许的节点', path: ['allow'], kind: 'strlist', hint: '如 show,configure' },
          { label: '拒绝的节点', path: ['deny'], kind: 'strlist' },
        ],
      },
    ],
  },
];

// VPP 段（命令行 `set vpp …`）：生成数据面启动配置的运行时参数。
const CFG_VPP_SECTIONS = [
  {
    legend: 'VPP（数据面）',
    wide: true,
    loc: [{ obj: 'vpp' }],
    clearPath: ['vpp'],
    note: '数据面运行时配置。主核与 worker 核必须取自「资源池」的隔离核池，大页偏好须与资源池的页大小一致' +
      '（服务端在提交时校验）；CPU / 内存 / DPDK 的改动提交后需重启数据面才生效。逐字段清空即回落默认值。',
    fields: [
      { label: 'CPU 主核', path: ['cpu', 'main_core'], kind: 'int', min: 0 },
      { label: 'worker 核列表', path: ['cpu', 'corelist_workers'], kind: 'corelist', hint: '如 5,7,9-11',
        group: 'vppWorkers' },
      { label: '每 NUMA worker 数', path: ['cpu', 'workers_per_numa'], kind: 'int', min: 1,
        group: 'vppWorkers' },
      { label: '内存主堆大小', path: ['memory', 'main_heap_size'], kind: 'heapSize', hint: '如 1G、512M' },
      { label: '每 NUMA buffer 数', path: ['memory', 'buffers_per_numa'], kind: 'int', min: 1 },
      { label: '大页偏好', path: ['memory', 'hugepage_preference'], kind: 'select',
        options: [['2M', '2M'], ['1G', '1G']] },
      { label: 'DPDK 全局收队列', path: ['dpdk', 'dev', 'rx_queues'], kind: 'int', min: 1 },
      { label: 'DPDK 全局发队列', path: ['dpdk', 'dev', 'tx_queues'], kind: 'int', min: 1 },
      { label: 'DPDK 全局收描述符', path: ['dpdk', 'dev', 'rx_descriptors'], kind: 'int', min: 1 },
      { label: 'DPDK 全局发描述符', path: ['dpdk', 'dev', 'tx_descriptors'], kind: 'int', min: 1 },
      { label: 'UIO 驱动', path: ['dpdk', 'uio_driver'], kind: 'select',
        options: [['vfio-pci', 'vfio-pci'], ['igb-uio', 'igb-uio']] },
    ],
    sublists: [
      {
        list: 'per_dev', key: 'interface', keyKind: 'text', title: '单网卡覆盖', what: '单网卡覆盖',
        loc: [{ obj: 'dpdk' }], // per_dev 挂在 vpp.dpdk 下（不是 vpp 下）
        keyLabel: '接口名', placeholder: '如 ens192',
        note: '只对已声明的物理口生效（服务端会校验）；未覆盖的参数继承上面的全局默认。' +
          '删除整行 = 该网卡全部参数回落全局默认。',
        fields: [
          { label: '收队列', path: ['rx_queues'], kind: 'int', min: 1 },
          { label: '发队列', path: ['tx_queues'], kind: 'int', min: 1 },
          { label: '收描述符', path: ['rx_descriptors'], kind: 'int', min: 1 },
          { label: '发描述符', path: ['tx_descriptors'], kind: 'int', min: 1 },
        ],
      },
      {
        list: 'plugins', key: 'name', keyKind: 'text', title: '插件开关', what: '插件',
        keyLabel: '插件名', placeholder: '如 acl / nat / span',
        note: '删除该行 = 恢复默认（启用）。',
        fields: [{
          label: '状态', path: ['state'], kind: 'select',
          options: [['enable', '启用'], ['disable', '禁用']],
        }],
      },
    ],
  },
];

const CFG_SECTIONS = [
  {
    legend: '虚拟交换机',
    wide: true,
    list: 'virtual_switches',
    key: 'name',
    what: '虚拟交换机',
    title: (it) => (it.name || '（未命名）') + (it.type ? '（' + it.type + '）' : ''),
    note: 'L2 = 桥域（access VLAN / 网关 / 成员端口），L3 = VRF（三层接口与静态路由在「VRF / 路由」节的同名条目里）。' +
      '类型创建后不可改；新增按 L2 建立，要 L3 请在新增后把类型改为 L3。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 vs-a', extra: () => ({ type: 'l2' }) },
    fields: [
      { label: '类型', path: ['type'], kind: 'select', options: [['l2', 'L2（桥域）'], ['l3', 'L3（VRF）']] },
      { label: '描述', path: ['description'], kind: 'text' },
      { label: 'access VLAN', path: ['vlan_access'], kind: 'int', min: 1, max: 4094 },
      { label: '端口直通（cross-connect）', path: ['cross_connect'], kind: 'bool',
        note: '配对＝该交换机的**两个成员端口**（恰好两个，按成员端口顺序直通）——打开前先把这两条成员端口配好。' +
          '直通是二层透传、无环路保护：两端必须属于不同广播域，同一广播域内直通会成环。' },
      { label: '网关地址（CIDR，逗号分隔）', path: ['gateway', 'addresses'], kind: 'cidrlist' },
      { label: '网关所属 VRF', path: ['gateway', 'vrf'], kind: 'text' },
      { label: '网关入向 ACL', path: ['gateway', 'acl_in'], kind: 'text' },
      { label: '网关出向 ACL', path: ['gateway', 'acl_out'], kind: 'text' },
    ],
    sublists: [{
      list: 'ports', key: 'seq', keyKind: 'int', title: '成员端口', what: '端口',
      keyLabel: '序号', placeholder: '如 1',
      note: '物理口 / VNF / 容器三选一（本地就会拦下同时填两个的情况）；trunk VLAN 用逗号分隔。',
      fields: [
        { label: '物理口 / bond', path: ['interface'], kind: 'text', group: 'portKind' },
        { label: 'VNF（VM 名）', path: ['vnf'], kind: 'text', group: 'portKind',
          clears: [['vnf_interface']] },
        { label: 'VNF 的 vNIC', path: ['vnf_interface'], kind: 'text' },
        { label: '容器名', path: ['container'], kind: 'text', group: 'portKind',
          clears: [['container_interface']] },
        { label: '容器的 vNIC', path: ['container_interface'], kind: 'text' },
        { label: 'trunk VLAN 列表', path: ['trunk'], kind: 'intlist', min: 1, max: 4094 },
        { label: 'native VLAN', path: ['native'], kind: 'int', min: 1, max: 4094 },
        { label: '入向 ACL', path: ['acl_in'], kind: 'text' },
        { label: '出向 ACL', path: ['acl_out'], kind: 'text' },
      ],
    }],
  },
  {
    legend: 'VRF / 路由',
    wide: true,
    list: 'vrfs',
    key: 'name',
    what: 'VRF',
    title: (it) => it.name || '（未命名）',
    note: 'L3 虚拟交换机的三层接口与静态路由就存在与交换机同名的条目里——在「虚拟交换机」节把类型改为 L3 后，' +
      '这里会出现同名条目（反过来：这里新建的条目若没有同名 L3 交换机，只是独立的 VRF 配置）。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 vs-l3' },
    fields: [{ label: '描述', path: ['description'], kind: 'text' }],
    sublists: [
      {
        list: 'l3_interfaces', key: 'interface', keyKind: 'text', title: '三层接口', what: '三层接口',
        keyLabel: '接口名', placeholder: '如 ens192 或 ens192.100',
        note: '接口名与「接口」节里的物理口一致；VLAN 子接口写成 ens192.100。',
        fields: [
          { label: 'VLAN', path: ['vlan'], kind: 'int', min: 1, max: 4094 },
          { label: '地址（CIDR，逗号分隔）', path: ['addresses'], kind: 'cidrlist' },
          { label: '入向 ACL', path: ['acl_in'], kind: 'text' },
        ],
      },
      {
        list: 'routes', key: 'prefix', keyKind: 'cidr', title: '静态路由', what: '静态路由',
        keyLabel: '目的网段', placeholder: '如 10.0.0.0/8 或 0.0.0.0/0',
        fields: [
          { label: '下一跳', path: ['next_hop'], kind: 'ip' },
          { label: '管理距离', path: ['distance'], kind: 'int', min: 0, max: 255 },
        ],
      },
    ],
  },
  {
    legend: 'ACL',
    wide: true,
    list: 'acls',
    key: 'name',
    what: 'ACL',
    title: (it) => (it.name || '（未命名）') + '（' + ((it.rules || []).length) + ' 条规则）',
    note: '规则按 seq 从小到大生效；源/目的可写 ip-prefix 或 any。绑定在交换机端口 / L3 接口 / 网关的 ACL 字段上。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 acl-a' },
    fields: [{ label: '描述', path: ['description'], kind: 'text' }],
    sublists: [{
      list: 'rules', key: 'seq', keyKind: 'int', title: '规则', what: '规则',
      keyLabel: '序号', placeholder: '如 10',
      fields: [
        { label: '方向', path: ['direction'], kind: 'select', options: [['ingress', 'ingress'], ['egress', 'egress']] },
        { label: '源', path: ['source'], kind: 'ipAny', hint: 'ip-prefix 或 any' },
        { label: '目的', path: ['destination'], kind: 'ipAny', hint: 'ip-prefix 或 any' },
        { label: '协议', path: ['protocol'], kind: 'select', options: [['tcp', 'tcp'], ['udp', 'udp'], ['icmp', 'icmp'], ['any', 'any']] },
        { label: '源端口', path: ['source_port'], kind: 'port', hint: '如 80 或 1024-65535' },
        { label: '目的端口', path: ['destination_port'], kind: 'port' },
        { label: '动作', path: ['action'], kind: 'select', options: [['permit', 'permit'], ['deny', 'deny']] },
      ],
    }],
  },
  {
    legend: 'NAT',
    wide: true,
    loc: [{ obj: 'nat' }],
    clearPath: ['nat'],
    note: 'NAT44 仅作用于 L3 交换机；规则的出接口必填（不填提交会被拒），且出接口须是某 L3 交换机的三层接口并配了地址。',
    sublists: [
      {
        list: 'source_pools', key: 'name', keyKind: 'text', title: '源地址池', what: '地址池',
        keyLabel: '池名', placeholder: '如 pool-a',
        fields: [{ label: '地址范围', path: ['address_range'], kind: 'addrRange', hint: '192.168.155.220 to 192.168.155.225' }],
      },
      {
        list: 'rules', key: 'seq', keyKind: 'int', title: '转换规则', what: 'NAT 规则',
        keyLabel: '序号', placeholder: '如 10',
        fields: [
          { label: '匹配源（CIDR）', path: ['match_source'], kind: 'cidr' },
          { label: 'L3 交换机', path: ['virtual_switch'], kind: 'text' },
          { label: '出接口', path: ['action', 'interface'], kind: 'text' },
          { label: '源地址池', path: ['action', 'source_pool'], kind: 'text' },
        ],
      },
      {
        list: 'static', key: 'inside_ip', keyKind: 'ip', title: '静态 1:1 映射', what: '静态映射',
        keyLabel: '内部 IP', placeholder: '如 10.10.0.10',
        fields: [{ label: '外部 IP', path: ['outside_ip'], kind: 'ip' }],
      },
    ],
  },
  {
    legend: 'QoS（限速）',
    list: 'qos_policies',
    key: 'name',
    what: 'QoS 策略',
    title: (it) => (it.name || '（未命名）') +
      (it.cir ? '（' + it.cir + ' bps）' : ''),
    note: '绑定到接口用「接口」节的「入向限速策略」。',
    add: { keyLabel: '策略名', keyKind: 'text', placeholder: '如 lim-a' },
    fields: [
      { label: 'CIR（bps）', path: ['cir'], kind: 'int', min: 1 },
      { label: 'CBS（字节）', path: ['cbs'], kind: 'int', min: 1 },
    ],
  },
  {
    legend: '端口镜像（SPAN）',
    wide: true,
    list: 'port_mirroring',
    key: 'name',
    what: '镜像会话',
    title: (it) => it.name || '（未命名）',
    note: '源二选一：物理口，或某台 VM 的 vNIC（本地会拦下同时填两个的情况）。分析口必须是物理口。',
    add: { keyLabel: '会话名', keyKind: 'text', placeholder: '如 span-a' },
    fields: [
      { label: '源：物理口 / bond', path: ['source', 'interface'], kind: 'text', group: 'pmSource',
        clears: [['source', 'vnf'], ['source', 'vnf_interface']] },
      { label: '源：VNF（VM 名）', path: ['source', 'vnf'], kind: 'text', group: 'pmSource',
        clears: [['source', 'interface']] },
      { label: '源：VNF 的 vNIC', path: ['source', 'vnf_interface'], kind: 'text' },
      { label: '方向', path: ['source', 'direction'], kind: 'select', options: [['ingress', 'ingress'], ['egress', 'egress'], ['both', 'both']] },
      { label: '分析端口', path: ['analyzer'], kind: 'text' },
    ],
  },
  {
    legend: '链路聚合（bond）',
    list: 'bonds',
    key: 'name',
    what: 'bond',
    title: (it) => (it.name || '（未命名）') + (it.lacp ? '（LACP）' : '（静态聚合）'),
    note: '成员口须是未被虚拟交换机引用的物理口；清空 LACP 模式与速率即回到静态聚合。',
    add: { keyLabel: 'bond 名', keyKind: 'text', placeholder: '如 bond0' },
    fields: [
      { label: '成员口（逗号分隔）', path: ['members'], kind: 'strlist', hint: '如 ens224,ens256' },
      { label: 'LACP 模式', path: ['lacp', 'mode'], kind: 'select', options: [['active', 'active'], ['passive', 'passive']] },
      { label: 'LACP 速率', path: ['lacp', 'interval'], kind: 'select', options: [['fast', 'fast'], ['slow', 'slow']] },
      { label: 'MTU', path: ['mtu'], kind: 'int', min: 68, max: 9216 },
      { label: '描述', path: ['description'], kind: 'text' },
    ],
  },
  {
    legend: 'LLDP',
    loc: [{ obj: 'protocols' }, { obj: 'lldp' }],
    clearPath: ['protocols', 'lldp'],
    note: '全局开关 + 按接口覆盖（基于 VPP lldp 插件）。邻居表在「网络对象」页。',
    fields: [
      { label: '全局启用', path: ['enabled'], kind: 'bool' },
      { label: '通告间隔（秒）', path: ['advertisement_interval'], kind: 'int', min: 1, max: 3600 },
    ],
    sublists: [{
      list: 'interfaces', key: 'interface', keyKind: 'text', title: '按接口覆盖', what: '接口覆盖',
      keyLabel: '接口名', placeholder: '如 ens224',
      fields: [{ label: '启用', path: ['enabled'], kind: 'bool' }],
    }],
  },
  {
    legend: '虚拟机（VM VNF）',
    wide: true,
    list: 'virtual_machine_functions',
    key: 'name',
    what: '虚拟机',
    title: (it) => (it.name || '（未命名）') + (it.image ? '（' + it.image + '）' : ''),
    note: '镜像须是仓库里的 vm-image；vCPU 从隔离核池分配，内存从大页池分配。' +
      '内存后端选「普通内存」时不允许 vhost-user 口（提交会被拒）。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 vnf-a' },
    fields: [
      { label: '镜像', path: ['image'], kind: 'text', hint: '如 base.qcow2' },
      { label: '描述', path: ['description'], kind: 'text' },
      { label: 'vCPU 数量', path: ['vcpu', 'count'], kind: 'int', min: 1, max: 256 },
      { label: 'vCPU 绑核', path: ['vcpu', 'pin'], kind: 'bool' },
      { label: '内存（MB）', path: ['memory', 'size_mb'], kind: 'int', min: 1 },
      { label: '大页大小', path: ['memory', 'hugepage_size'], kind: 'select', options: [['2M', '2M'], ['1G', '1G']] },
      { label: 'NUMA 节点', path: ['memory', 'numa_node'], kind: 'int', min: 0, max: 63 },
      { label: '内存后端', path: ['memory', 'backing'], kind: 'select', options: [['hugepage', '大页'], ['normal', '普通内存（禁止 vhost-user 口）']] },
      { label: '串口控制台', path: ['serial_console'], kind: 'bool' },
      { label: '开机自启', path: ['autostart'], kind: 'bool' },
      { label: 'guest 主机名', path: ['cloud_init', 'hostname'], kind: 'text' },
      { label: 'SSH 公钥（逗号分隔）', path: ['cloud_init', 'ssh_keys'], kind: 'strlist' },
      { label: 'user-data（YAML 文本）', path: ['cloud_init', 'user_data'], kind: 'raw' },
    ],
    sublists: [
      {
        list: 'disks', key: 'name', keyKind: 'text', title: '附加数据盘', what: '数据盘',
        keyLabel: '盘名', placeholder: '如 data0',
        note: '容量与来源镜像二选一（都填或都不填提交会被拒）。',
        fields: [
          { label: '容量（GB，空盘）', path: ['size_gb'], kind: 'int', min: 1 },
          { label: '来源镜像', path: ['image'], kind: 'text' },
        ],
      },
      {
        list: 'interfaces', key: 'name', keyKind: 'text', title: 'vNIC', what: 'vNIC',
        keyLabel: 'vNIC 名', placeholder: '如 eth0',
        note: 'vhost-user 接虚拟交换机；sriov-vf 还需物理口与 VF 号。',
        fields: [
          { label: '类型', path: ['type'], kind: 'select', options: [['vhost-user', 'vhost-user'], ['sriov-vf', 'sriov-vf（SR-IOV 直通）']] },
          { label: '虚拟交换机', path: ['virtual_switch'], kind: 'text' },
          { label: 'MAC', path: ['mac'], kind: 'mac', hint: '留空自动生成' },
          { label: 'VLAN', path: ['vlan'], kind: 'int', min: 1, max: 4094 },
          { label: 'SR-IOV 物理口', path: ['sriov', 'physical_interface'], kind: 'text' },
          { label: 'SR-IOV VF 号', path: ['sriov', 'vf_id'], kind: 'int', min: 0, max: 255 },
        ],
      },
    ],
  },
  {
    legend: '容器（container VNF）',
    wide: true,
    list: 'container_functions',
    key: 'name',
    what: '容器',
    title: (it) => (it.name || '（未命名）') + (it.image ? '（' + it.image + '）' : ''),
    note: '镜像须是仓库里的 container-image；vNIC 固定为 memif。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 sbc-ct1' },
    fields: [
      { label: '镜像', path: ['image'], kind: 'text', hint: '如 alpine:3.20' },
      { label: '描述', path: ['description'], kind: 'text' },
      { label: 'vCPU 限制', path: ['vcpu'], kind: 'int', min: 1 },
      { label: '内存限制（MB）', path: ['memory_mb'], kind: 'int', min: 1 },
      { label: '入口命令', path: ['command'], kind: 'text' },
      { label: '命令参数（逗号分隔）', path: ['args'], kind: 'strlist' },
      { label: '重启策略', path: ['restart_policy'], kind: 'select', options: [['no', '不自动重启'], ['on-failure', '失败时重启']] },
      { label: '开机自启', path: ['autostart'], kind: 'bool' },
      { label: '环境变量（每行 KEY=VALUE）', path: ['env'], kind: 'kvmap' },
    ],
    sublists: [{
      list: 'interfaces', key: 'name', keyKind: 'text', title: 'memif vNIC', what: 'vNIC',
      keyLabel: 'vNIC 名', placeholder: '如 eth0',
      fields: [
        { label: '类型', path: ['type'], kind: 'select', options: [['memif', 'memif']] },
        { label: '虚拟交换机', path: ['virtual_switch'], kind: 'text' },
        { label: 'MAC', path: ['mac'], kind: 'mac', hint: '留空自动生成' },
        { label: 'VLAN', path: ['vlan'], kind: 'int', min: 1, max: 4094 },
      ],
    }],
  },
];

// 把一组节定义画进表单容器（系统段 / VPP 段 / 网络域与计算域共用）。
function cfgRenderList(box, list, c) {
  list.forEach((sec) => cfgRenderSection(box, sec, c));
}

// 把网络域与计算域各节画进表单容器（与上面各节同一个容器、同一套写路径）。
function cfgRenderDomainForms(box, c) {
  cfgRenderList(box, CFG_SECTIONS, c);
}

// 预校验（POST /configuration/check）：先保存，再让服务端跑与提交相同的全部校验。
export async function cfgCheck() {
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

// 数据面配置段（GET /vpp/config，committed 的 vpp 段）：它是生成启动配置的源，
// 与「完整配置」里的 vpp 段是同一份事实——单列出来是因为排障时常要单独看它（与命令行
// 的 `show vpp` 同源），省得在大 JSON 里翻。只读回显，改它仍在下面的表单里。
function renderCfgVpp(vpp) {
  $('cfg-vpp').textContent = !vpp ? ''
    : (vpp.__err ? '读取失败：' + vpp.__err : JSON.stringify(vpp, null, 2));
}

// 读取 committed 配置。`pre` 是路由表声明端点的预取结果（配置页由 softLoad 取齐后传进来，
// 不重复请求）；不传则自己拉（提交事件、结束编辑会话等处调用）。
export function getCfg() { return cfg; }
export function cfgSetEditingForBoot(on) { cfgSetEditing(on); }
export async function loadConfig(pre) {
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

function cfgLockNote(text) {
  const n = $('cfg-lock-note');
  n.textContent = text || '';
  n.hidden = !text;
}

// 进本页时对齐服务端的编辑会话：本会话已经有候选（例如刚从「提交历史」页取的历史快照，
// 或刷新页面后仍在编辑）就直接进编辑态并展示差异——否则操作者再点一次「开始编辑」会把
// 刚取到的候选覆盖成当前配置，白做一次回滚。锁在别的会话手里时如实说明是谁持有
// （本页保持只读；写操作由服务端按会话锁规则拒绝，界面不做第二个判定）。
async function cfgSyncSession() {
  const landed = cfg.landedFromRollback; // 刚从「提交历史」页取过候选（值是偏移）
  cfg.landedFromRollback = 0;
  // 已在编辑态且不是刚取过候选：不动（别覆盖操作者正在改的文本）。
  // 刚取过候选时**要**刷新编辑区——服务端的候选已经被那一版替换，留旧文本会让人改错底稿。
  if (cfg.editing && !landed) return;
  if (!cfg.committed) return; // 读不到当前配置：不进编辑态（与「开始编辑」同一条纪律）
  let cand = null;
  try {
    cand = await api('/configuration/candidate');
  } catch (e) {
    cfgLockNote(''); // 服务端没有候选：只读视图（点「开始编辑」才建立会话）
    return;
  }
  const holder = await cfgLockHolder();
  if (holder !== getUser() + '@api') {
    cfgLockNote('当前候选配置由 ' + (holder || '未知会话') + ' 持有编辑锁——本页只读，' +
      '可在「编辑锁会话」页查看；要自己改配置请等对方结束编辑。');
    return;
  }
  const doc = (cand && cand.candidate) || {};
  cfgWriteText(doc);
  cfgRenderForms(doc);
  cfgSetEditing(true);
  cfgLockNote('');
  if (landed) {
    cfgMsg('已把第 ' + landed + ' 版取为候选（未提交）——先看差异、预校验，确认后点「提交」；' +
      '不提交就点「丢弃并结束编辑」。', false);
    await cfgShowDiff();
  } else {
    cfgMsg('本会话已有未提交的候选配置（刷新页面后仍在编辑会话里）。', false);
  }
}

// 当前编辑锁的持有者：取自持锁会话列表里**有取锁时间**的那一条（只有一条会话锁，
// 另有在途 commit confirmed 时它不带取锁时间，不能当成持锁者）；查不到时返回空串。
export async function cfgLockHolder() {
  try {
    const list = await api('/system/configuration/sessions');
    const lock = (Array.isArray(list) ? list : []).find((s) => s && s.acquired_at);
    return (lock && lock.holder) || '';
  } catch (e) {
    return '';
  }
}

// 开始编辑：以 committed 为起点建立 candidate（PUT 取锁）。
// 读不到 committed 时**拒绝进入**——否则会把空配置当 candidate 提交（危险）。
export async function cfgStartEdit() {
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

export async function cfgSave() {
  // 口令没派生出哈希（明文还留在控件里）时**整次保存失败**：不写 candidate，也不报成功。
  if (!(await cfgPwGate())) return false;
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

export async function cfgShowDiff() {
  try {
    const res = await fetch(API + '/configuration/diff', { headers: { Authorization: 'Bearer ' + getToken() } });
    const text = await res.text();
    const pre = $('cfg-diff');
    pre.textContent = res.ok ? (text.trim() || '（candidate 与 committed 无差异）') : '读取差异失败：' + errTextOf(text, res.status);
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

// 丢弃并结束编辑：低危确认（只丢自己这份候选草稿，运行配置不变）。没有进入编辑态时
// 没有草稿可丢，就不打扰——直接收尾（可能只是把超时残留的锁收掉）。
export async function cfgDiscardAsk() {
  if (cfg.editing) {
    const ok = await uiConfirm('丢弃并结束编辑', {
      tier: 'low',
      paragraphs: ['丢弃候选配置并释放编辑锁？运行中的配置不受影响；' +
        '未提交的改动会丢掉，拿不准就先点「查看差异」看一眼。'],
      cli: 'discard',
    });
    if (!ok) return;
  }
  await cfgEndSession();
}

// 提交（含以 commit confirmed 提交）：中危确认——提交是把差异**立即下发到运行配置**，
// 不是保存草稿；改到管理路径（管理口地址/网关）时还可能把自己关在门外，故逐条列影响面。
// 注意：确认框只加在按钮这一层，cfgCommit 本身保持原样（结束会话/失败补偿等内部路径
// 不该被二次确认挡住）。
export async function cfgCommitAsk(confirmedMinutes) {
  const ok = await uiConfirm(confirmedMinutes ? '以 commit confirmed 提交' : '提交配置', {
    tier: 'mid',
    paragraphs: [confirmedMinutes
      ? '把候选配置提交为运行配置，并设 ' + confirmedMinutes + ' 分钟的确认窗口。'
      : '把候选配置提交为运行配置（revision 加一，立即生效）。'],
    bullets: confirmedMinutes ? [
      '提交立即生效；窗口期内不点「确认在途提交」就自动回滚到提交前那一版。',
      '这是防止改坏管理路径把自己锁在门外的自锁保护：确认管理面仍然连得上，再点「确认在途提交」。',
      '变更会立即下发给各子系统，相关业务可能随变更中断（接口、交换机、ACL、NAT 这类）。',
      '提交失败会自动补偿（事务引擎），失败原因按页面提示看。',
    ] : [
      '候选与运行配置的差异会立即下发给各子系统，相关业务可能随变更中断（接口、交换机、ACL、NAT 这类）。',
      '这不是保存草稿：要撤销得回滚到上一版再提交一次。',
      '管理口地址、网关这类会改变管理路径的字段，请改用「以 commit confirmed 提交」——超时未确认会自动回滚。',
      '拿不准就先点「预校验」「查看差异」，提交前确认变更内容。',
    ],
    cli: confirmedMinutes ? 'commit confirmed ' + confirmedMinutes : 'commit',
    confirmLabel: '提交',
  });
  if (!ok) return;
  await cfgCommit(confirmedMinutes);
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

// 确认在途提交：**有意不做确认框**——它的语义就是「保住刚才那次提交」，
// 而窗口正在倒计时（超时自动回滚）；在这里再拦一次反而是害人。审计仍由服务端记。
export async function cfgConfirmPending() {
  try {
    await api('/configuration/commit:confirm', { method: 'POST' });
    await cfgEndSession();
    cfgMsg('已确认，提交生效。', false);
  } catch (e) {
    cfgMsg('确认失败：' + e.message, true);
  }
}

// ---------- 配置提交历史（#/config/history）与编辑锁会话（#/config/sessions） ----------
//
// 历史页 = 只读列表 + **两段式回滚**的入口：回滚沿用命令行的语义——把某一版取为 candidate
// （POST /configuration/rollback/{n}），**不直接生效**；要改变运行配置仍得到「配置」页提交。
//
// 两个容易搞错的地方，这里按契约与引擎的实际语义钉住：
//   ① 端点里的 n 是**相对最新 committed 的偏移**（n=1 = 最新版的前一版），不是 Rev 本身；
//      故由列表里的 Rev 与最新 Rev 相减算出，Rev 相同（当前生效那一版）没有可回滚的偏移。
//   ② 服务端只有「候选 ⇄ 当前生效配置」的差异读取（GET /configuration/diff），**没有**
//      「直接与某个历史版本比对」的读取接口；故「看差异」给的是引导（先把该版取为候选，
//      再到「配置」页看差异），而不是就地显示差异——不臆造端点，也不假装拿到了差异。

function cfghMsg(text, isErr) {
  const n = $('cfgh-msg');
  n.textContent = text || '';
  n.hidden = !text;
  n.className = isErr ? 'error small' : 'muted small';
}

function renderHistory(revs) {
  const tbody = $('cfgh-table').querySelector('tbody');
  tbody.textContent = '';
  cfghMsg('', false); // 上一次操作的回显不跨轮次残留
  const list = revs || [];
  const latest = list.reduce((m, r) => Math.max(m, Number(r.rev) || 0), 0);
  $('cfgh-note').textContent = list.length ? '（保留 ' + list.length + ' 版，最新在前）' : '';
  if (!list.length) {
    tbody.appendChild(el('tr', {}, [
      el('td', { colspan: '6', class: 'muted', text: '（暂无历史版本）' }),
    ]));
    return;
  }
  list.forEach((r) => {
    const rev = Number(r.rev) || 0;
    const n = latest - rev; // 相对最新的偏移：0 = 当前生效那一版
    const tr = el('tr');
    [rev, fmtTime(r.committed_at), r.user ? r.user : '（未记录）', r.comment, r.current ? '当前生效' : '']
      .forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    const cell = el('td', { class: 'actions' });
    if (n < 1) {
      cell.appendChild(el('span', { class: 'muted small', text: '（当前生效，无需回滚）' }));
    } else {
      const diff = el('button', { type: 'button', class: 'ghost small', text: '看差异' });
      diff.addEventListener('click', () => cfghDiffGuide(rev, n));
      const rb = wbtn({ type: 'button', class: 'small danger', text: '回滚到这一版' });
      rb.addEventListener('click', () => cfghRollback(rev, n, r));
      cell.appendChild(diff);
      cell.appendChild(rb);
    }
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 「看差异」：服务端没有按历史版本比对的读取接口，故这里只给**引导**（不取数、不改任何东西）。
async function cfghDiffGuide(rev, n) {
  await uiDialog({
    title: '看差异：第 ' + n + ' 版（revision ' + rev + '）与当前配置',
    tier: 'low',
    paragraphs: [
      '服务端只提供「候选配置与当前生效配置」的差异读取，没有「直接与某个历史版本比对」的读取接口，' +
        '因此本页无法就地显示这一版与当前配置的差异。',
      '要看差异：先在本页点「回滚到这一版」（第一步只把它取为候选，不会生效），' +
        '随后在「配置」页点「查看差异」——那里显示的是「当前生效配置与这一版」的差异' +
        '（命令行里 compare rollback 的方向相反：那一侧是「这一版 → 当前生效配置」，改动内容是同一份）；' +
        '确认不需要这一版，就点「丢弃并结束编辑」，运行配置不受影响。',
    ],
    cli: 'show configuration | compare rollback ' + n,
    confirmLabel: '知道了',
    cancelLabel: '',
  });
}

// 回滚第一步：把第 n 版取为候选（不提交）。中危确认——影响面逐条列出、主按钮标红。
async function cfghRollback(rev, n, row) {
  const dirty = await cfghCandidateDirty();
  const bullets = [
    '会以第 ' + n + ' 版（revision ' + rev + '，提交于 ' + fmtTime(row && row.committed_at) +
      '）为底稿建立候选配置；正在运行的配置此刻不受影响。',
    '候选不会自己生效：要到「配置」页点「提交」才改变运行配置；提交之前可以随时「丢弃并结束编辑」。',
    '提交前请在「配置」页用「查看差异」「预校验」确认这次变更。',
  ];
  if (dirty) bullets.push('当前已有候选配置（含未提交的变更），会被这一版覆盖。');
  bullets.push('取为候选会占用编辑锁（本会话还没有编辑会话时，本页先按当前配置取锁）；' +
    '锁被别的会话持有时会失败并说明是谁持有。');
  const ok = await uiDialog({
    title: '回滚到第 ' + n + ' 版（取为候选，不立即生效）',
    tier: 'mid',
    bullets,
    cli: 'rollback ' + n,
    confirmLabel: '取为候选（不提交）',
  });
  if (!ok) return;
  await cfghTakeCandidate(n);
}

// 当前是否已有未提交的候选（影响面里要如实说「会被覆盖」）。没有候选（未进入编辑会话）按「无」。
async function cfghCandidateDirty() {
  try {
    const c = await api('/configuration/candidate');
    return !!(c && c.dirty);
  } catch (e) {
    return false;
  }
}

// 取为候选：先直接试一次；报「未持有 candidate」说明本会话还没有编辑会话（或锁在别人手里），
// 此时按当前配置取一次编辑锁再重试——锁真被别人持有时这一步会失败并报出持有者，
// 不会覆盖别人的候选（服务端在写候选之前就先判锁）。
async function cfghTakeCandidate(n) {
  cfghMsg('正在把第 ' + n + ' 版取为候选…', false);
  try {
    await api('/configuration/rollback/' + n, { method: 'POST' });
  } catch (e) {
    if (!/未持有/.test(e.message)) {
      cfghMsg('取为候选失败：' + e.message, true);
      return;
    }
    if (!(await cfghAcquireLock())) return;
    try {
      await api('/configuration/rollback/' + n, { method: 'POST' });
    } catch (e2) {
      cfghMsg('取为候选失败：' + e2.message, true);
      return;
    }
  }
  // 落到「配置」页：那一页会载入这份候选并直接展示差异（提交或丢弃都在那一页）。
  cfg.landedFromRollback = n;
  await goDetail('#/config');
}

// 取编辑锁：以当前生效配置为底稿写一次候选（与「配置」页的「开始编辑」是同一个动作）。
async function cfghAcquireLock() {
  try {
    const cur = await api('/configuration');
    const base = (cur && cur.configuration) || {};
    await api('/configuration/candidate', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(base),
    });
    return true;
  } catch (e) {
    cfghMsg('无法取得编辑锁：' + e.message + '——锁可能正被别的会话持有，可在「编辑锁会话」页查看。', true);
    return false;
  }
}

// 编辑锁会话（只读）：按端点返回的字段渲染（谁 / 来源 / 时间 / 未提交变更 / 待确认截止），
// 字段缺席时显示「—」，不编造。
function renderSessions(list) {
  const tbody = $('cfgs-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = list || [];
  $('cfgs-note').textContent = rows.length ? '（' + rows.length + ' 个持锁条目）' : '';
  if (!rows.length) {
    tbody.appendChild(el('tr', {}, [
      el('td', { colspan: '6', class: 'muted', text: '（当前没有会话持有编辑锁）' }),
    ]));
    return;
  }
  rows.forEach((s) => {
    const holder = s.holder || '';
    const tr = el('tr');
    [
      holder,
      sessionSource(holder),
      fmtTimeOpt(s.acquired_at),
      fmtTimeOpt(s.last_activity),
      s.dirty === undefined || s.dirty === null ? undefined : (s.dirty ? '有' : '无'),
      fmtTimeOpt(s.confirmed_until),
    ].forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    tbody.appendChild(tr);
  });
}

// 来源取自持有者标识的后缀：引擎把持有者记作 `用户@来源`（命令行/串口/接口分别记作
// ssh / console / api）；没有 @ 时如实显示「—」，不猜。
function sessionSource(holder) {
  const i = (holder || '').indexOf('@');
  return i > 0 && i < holder.length - 1 ? holder.slice(i + 1) : undefined;
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsConfig = {
  'config': {
    // 取数后对齐一次服务端的编辑会话（本会话已有候选就直接进编辑态，见 cfgSyncSession）。
    async render(d) {
      pageWarn(d);
      await loadConfig(d['/configuration']);
      renderCfgVpp(d['/vpp/config']);
      await cfgSyncSession();
    }
  },
  'configHistory': {
    render(d) { pageWarn(d); renderHistory(rowsOf(d['/configuration/history'])); }
  },
  'configSessions': {
    render(d) { pageWarn(d); renderSessions(rowsOf(d['/system/configuration/sessions'])); }
  },
};
