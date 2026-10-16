// NFViS 控制台 · 节驱动的配置表单引擎（从 views/config.js 抽出，逻辑逐字保留；Web 控制台重构 2026-10）。
//
// 抽出的目的（为「配置页去中心化」铺路）：把「渲染某一节」「candidate 事务（取锁 / 写候选 /
// 校验 / 提交 / 释放）」做成**页面无关**的可复用件，供后续每个对象页渲染自己的节调用。
// 节定义（CFG_SYS_SECTIONS / CFG_VPP_SECTIONS / CFG_SECTIONS）是各页面的**数据**，仍留在页面侧，
// 经 renderSection / renderSections 传进来——引擎不认识任何具体节。
//
// 与页面的接线只有一处：结构变化（新增 / 删除条目）后引擎需要页面重画整份表单，故页面经
// initForms({ rerender }) 注入重画回调。配置页瘦身后没有页面再走默认宿主（对象页/系统页渲染
// 时都传自己的 opts），默认重画因此是空操作；引擎内部不认识页面。
//
// 原始 JSON 文本区（#cfg-text）与消息区（#cfg-msg）是本引擎与页面共享的两个落点：引擎的字段
// 改动一律「写回 JSON 文本 → 由页面保存到 candidate」，与本仓库命令行同一套候选语义。

import { $, el, dash, wbtn, uiConfirm } from './dom.js';
import { api } from './api.js';

// 页面注入的重画回调（默认空操作：未接线的页面调用引擎只会渲染，不会重画）。
let pageRerender = () => {};
export function initForms(opts) {
  if (opts && typeof opts.rerender === 'function') pageRerender = opts.rerender;
}

// —— 渲染宿主：把「重画回调 / 草稿落点（配置文档的读与写）/ 消息落点 / 保存按钮名」按**本次渲染**
// 捕获进控件闭包 ——
//
// 默认（opts 缺席）= 全局 pageRerender + #cfg-text + #cfg-msg（配置页的用法，逐字不变）。
// 对象页（配置页去中心化）渲染自己的节时传自己的落点：面板画在自己页里、草稿存在自己的
// 工作底稿里、消息显示在自己的消息区——与配置页的编辑会话互不干扰（共用 #cfg-text 的话，
// 谁后渲染谁就把对方的文本区冲掉，两边都会保存错底稿）。
function cfgHostOf(opts) {
  const o = opts || {};
  return {
    rerender: typeof o.rerender === 'function' ? o.rerender : pageRerender,
    read: typeof o.read === 'function' ? o.read : cfgText,
    write: typeof o.write === 'function' ? o.write : cfgWriteText,
    msg: typeof o.msg === 'function' ? o.msg : cfgMsg,
    saveLabel: o.saveLabel || '保存到 candidate',
  };
}

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
export function cfgPwResetAll() {
  cfgPwFields.clear();
  cfgPwBlocked.clear();
}

// cfgPwGate 保存前的闸门。先等正在派生的落地，再把「填了口令却没派生出哈希」的控件拦下；
// 控件里还留着明文的（程序化填值、或打完没离开控件）就地派生并清空——保存的语义就是
// 「这份口令要写进去」，不能因为没失焦就把它丢掉。任一步失败即返回 false，
// 调用方据此**整次保存失败**，不写 candidate。
export async function cfgPwGate() {
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

export function cfgMsg(text, isErr) {
  const n = $('cfg-msg');
  n.textContent = text || '';
  n.hidden = !text;
  n.className = isErr ? 'error small' : 'muted small';
}

export function cfgText() {
  const t = $(CFG_TEXT_ID).value;
  if (!t.trim()) return {};
  return JSON.parse(t); // 语法错误由调用方提示
}

export function cfgWriteText(c) {
  $(CFG_TEXT_ID).value = JSON.stringify(c, null, 2);
}

// —— candidate 事务（取锁 / 写候选 / 校验 / 提交 / 释放）——
//
// 与命令行同一套语义：PUT /configuration/candidate 取锁并建立 candidate →（可选）差异 →
// 校验（POST /configuration/check，跑与提交相同的全部校验）→ 提交（POST /configuration/commit；
// 管理口地址/网关变更会返回 CONFIRM_REQUIRED，须走 commit confirmed）→
// DELETE candidate 结束会话（**提交后也释放**，避免把会话锁留给下一次 CLI 登录）。
// 本组封装只负责「请求怎么写」，成功/失败的**文案与确认闸门**仍由页面决定。
export function candidateAcquire(base) {
  return api('/configuration/candidate', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(base),
  });
}

export function candidateWrite(doc) {
  return api('/configuration/candidate', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(doc),
  });
}

export function candidateCheck() {
  return api('/configuration/check', { method: 'POST' });
}

export function candidateCommit(body) {
  return api('/configuration/commit', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
}

export function candidateRelease() {
  return api('/configuration/candidate', { method: 'DELETE' });
}

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

// PCI 地址（BDF）的形态判据与归一（与 model.NormalizeBDF 同源）：`[domain:]bus:slot.function`，
// 十六进制、大小写不敏感、domain 可省略（缺省 0000）；段位宽上限 domain 4 / bus 2 / slot 2（≤0x1f）
// / function 1 位（≤7）。归一为 `dddd:bb:ss.f`（小写定宽）——用于本地判「同一设备重复声明」，
// 跨 VM 冲突与存在性仍由服务端裁决。
function cfgIsBDF(s) {
  const m = /^(?:([0-9a-fA-F]{1,4}):)?([0-9a-fA-F]{1,2}):([0-9a-fA-F]{1,2})\.([0-7])$/.exec(s);
  if (!m) return false;
  return parseInt(m[2], 16) <= 0xff && parseInt(m[3], 16) <= 0x1f;
}

function cfgNormBDF(s) {
  const m = /^(?:([0-9a-fA-F]{1,4}):)?([0-9a-fA-F]{1,2}):([0-9a-fA-F]{1,2})\.([0-7])$/.exec(s);
  if (!m) return '';
  const pad = (x, n) => x.toLowerCase().padStart(n, '0');
  return (m[1] ? pad(m[1], 4) : '0000') + ':' + pad(m[2], 2) + ':' + pad(m[3], 2) + '.' + m[4];
}

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
// h（可选）= 渲染宿主：策略判据要从**该宿主自己的**配置文档读（对象页的草稿不在 #cfg-text）。
async function cfgPreparePassword(text, h) {
  if (text.startsWith('pbkdf2$')) return text;
  let c;
  try {
    c = h ? h.read() : cfgText();
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
async function cfgPwDerive(id, inp, spec, pw, prepare, clear, h) {
  let hash;
  try {
    hash = await prepare(pw, h);
  } catch (e) {
    if (inp.value === pw) {
      cfgPwBlocked.set(id, { text: spec.label + '：' + e.message, secure: !!e.cfgSecureContext });
      // 立刻说明，别等到点保存才知道；保存那次还会再拦一次（见 cfgPwGate）
      (h ? h.msg : cfgMsg)(spec.label + '：' + e.message + '——未写入 candidate，保存会被拒绝。' +
        (e.cfgSecureContext ? CFG_PW_SECURE_HINT : ''), true);
    }
    return null;
  }
  if (inp.value !== pw) return null; // 期间又改了：这次结果作废，等最后一次派生的结果
  cfgPwBlocked.delete(id);
  if (clear) inp.value = '';
  return hash;
}

// 额外 QEMU 参数的拒绝前缀（与服务端 model.extraArgDenyPrefixes 同源，决策 #485）：
// 覆盖资源账本 / 抢走产品的网络·磁盘·console 模型 / 有结构化字段可代替 / 影响进程生命周期。
const CFG_QEMU_ARG_DENY = ['-m', '-mem', '-smp', '-object', '-numa',
  '-kernel', '-initrd', '-append',
  '-drive', '-blockdev', '-netdev', '-nic', '-net',
  '-chardev', '-monitor', '-qmp', '-mon', '-serial',
  '-cpu', '-machine', '-boot', '-device', '-M',
  '-uuid', '-daemonize', '-pidfile', '-S'];

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
  // QEMU 机器类型（启动画像 machine）：契约允许 q35 / pc 两个固定值，另允许版本化机型名前缀
  // pc-i440fx-* / pc-q35-*（如 pc-q35-6.2）——故不能用固定取值下拉，按文本控件 + 前缀判据。
  machineName: {
    fmt: cfgTextFmt,
    parse: (t) => {
      const s = t.trim();
      if (s === 'q35' || s === 'pc' || s.startsWith('pc-i440fx-') || s.startsWith('pc-q35-')) {
        return { value: s };
      }
      return { error: '机器类型必须为 q35|pc|pc-i440fx-*|pc-q35-*（如 pc-q35-6.2）' };
    },
  },
  ip: {
    fmt: cfgTextFmt,
    parse: (t) => (cfgIsIP(t.trim()) ? { value: t.trim() } : { error: '必须是 IP 地址（如 192.168.1.1）' }),
  },
  // 严格 IPv4（VXLAN 下垫地址 / DHCP 服务器与中继的地址都是 IPv4-only，服务端也按 IPv4 判）
  ip4: {
    fmt: cfgTextFmt,
    parse: (t) => (cfgIsIPv4(t.trim()) ? { value: t.trim() }
      : { error: '必须是 IPv4 地址（如 192.168.1.1）' }),
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
  // 主机防火墙的来源：ip-prefix 或单个 IP（服务端把裸 IP 按 /32、/128 归一，故两态都接受）
  ipOrCidr: {
    fmt: cfgTextFmt,
    parse: (t) => {
      const s = t.trim();
      return (cfgIsCIDR(s) || cfgIsIP(s)) ? { value: s }
        : { error: '必须是 IP 地址或 ip-prefix（如 10.0.0.1 或 10.0.0.0/8）' };
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
  // 直通 PCI 设备（BDF）列表：逗号分隔；逐条按 model.NormalizeBDF 的形态判，归一后查重复
  // （同一设备不能在本 VM 里重复声明；跨 VM 冲突与设备存在性由服务端裁决）。
  bdflist: {
    fmt: (v) => (Array.isArray(v) ? v.join(', ') : ''),
    parse: (t) => {
      if (!t.trim()) return { value: '' };
      const out = [];
      const seen = new Set();
      for (const part of t.split(',')) {
        const p = part.trim();
        if (!p) continue;
        if (!cfgIsBDF(p)) {
          return { error: p + ' 不是合法的 PCI 地址（形如 0000:03:00.0，domain 可省略写 03:00.0）' };
        }
        const norm = cfgNormBDF(p);
        if (seen.has(norm)) return { error: 'PCI 设备 ' + norm + ' 重复声明（同一设备每台 VM 只能一条）' };
        seen.add(norm);
        out.push(p);
      }
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
  // 额外 QEMU 参数（窄自由口，每行一条）：形态与 denylist 判据与 model.ValidateExtraArgs 同源。
  // 命中拒绝前缀即**本地拦下**（该类参数要么覆盖资源账本、要么与产品的网络/磁盘/console 模型冲突，
  // 到提交期也必被拒）；其余照写，最终仍以服务端裁决为准。
  qemuArgs: {
    fmt: (v) => (Array.isArray(v) ? v.join('\n') : ''),
    multiline: true,
    parse: (t) => {
      const out = t.split('\n').map((x) => x.trim()).filter((x) => x !== '');
      for (const a of out) {
        for (const tok of a.split(/\s+/)) {
          const bad = CFG_QEMU_ARG_DENY.find((p) => tok.startsWith(p));
          if (bad) {
            return { error: '额外 QEMU 参数 ' + tok + ' 不被允许（' + bad +
              ' 前缀）：该类参数要么覆盖资源账本、要么与产品的网络/磁盘/console 模型冲突——' +
              '请改用对应的结构化字段（machine/cpu mode/firmware/disk-interface/boot-order/accel/console）' };
          }
        }
      }
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
  // 核号列表（值落成**整数数组**，如资源池的 isolated_cores）：写法与 corelist 同族
  // （逗号分隔的核号或核号区间），但 corelist 对应的是模型里的**字符串**字段
  // （vpp.cpu.corelist_workers），两者形态不同不能混用。重复核号去重，展示时排序并压成区间。
  corenums: {
    fmt: (v) => {
      const nums = (Array.isArray(v) ? v : []).slice().sort((a, b) => a - b);
      const parts = [];
      for (let i = 0; i < nums.length; ) {
        let j = i;
        while (j + 1 < nums.length && nums[j + 1] === nums[j] + 1) j++;
        parts.push(j > i ? nums[i] + '-' + nums[j] : String(nums[i]));
        i = j + 1;
      }
      return parts.join(',');
    },
    parse: (t) => {
      const s = t.replace(/\s+/g, '');
      if (!/^\d+(-\d+)?(,\d+(-\d+)?)*$/.test(s)) return { error: '形如 2-5,7（逗号分隔的核号或核号区间）' };
      const out = [];
      for (const part of s.split(',')) {
        const [lo, hi] = part.split('-');
        if (hi !== undefined && Number(lo) > Number(hi)) {
          return { error: '核区间 ' + part + ' 不合法（起始核号应不大于结束核号）' };
        }
        const a = Number(lo), b = hi === undefined ? a : Number(hi);
        for (let n = a; n <= b; n++) if (out.indexOf(n) < 0) out.push(n);
      }
      return { value: out.length ? out : '' };
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
  // DHCP 中继与服务器互斥（同一交换机只能选其一，服务端也这么判——两者争抢 UDP/67 处理权）
  dhcpMode: [
    { path: ['dhcp_relay_server'], label: 'DHCP 中继' },
    { path: ['dhcp_server_pool_start'], label: 'DHCP 服务器租约池' },
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

function cfgApplyMsg(ok, text, h) {
  (h ? h.msg : cfgMsg)(text, !ok);
  return ok;
}

// 表单改动 → candidate：解析文本区（保留手工编辑）→ 本地判 → 写一个字段 → 写回文本区。
// h = 渲染宿主（见 cfgHostOf）：草稿与消息都落宿主自己的落点；h 缺席 = 配置页的全局落点。
function cfgApplyField(loc, spec, text, h) {
  const kind = CFG_KINDS[spec.kind];
  // 少数控件「留空 = 不改动」而非「删除」（口令：删掉哈希会让账号失去口令，服务端也会拒）
  if (text.trim() === '' && kind.emptyNoop) {
    return cfgApplyMsg(true, spec.label + '：留空表示不改动（未写入 candidate）。', h);
  }
  let c;
  try {
    c = h ? h.read() : cfgText();
  } catch (e) {
    return cfgApplyMsg(false, '原始 JSON 语法错误，请先修正：' + e.message, h);
  }
  // 空（含只有空白）一律表示「删除该字段」——清空即删除，与上面三节同口径
  const r = text.trim() === '' ? { value: '' } : kind.parse(text, spec);
  if (r.error) return cfgApplyMsg(false, spec.label + '：' + r.error + '——未写入 candidate，请修正后重试。', h);
  if (spec.group && r.value !== '') {
    const clash = (CFG_GROUPS[spec.group] || []).find((g) =>
      g.path.join('.') !== spec.path.join('.') && !cfgEmptyVal(cfgRead(c, loc, g.path)));
    if (clash) {
      return cfgApplyMsg(false, spec.label + '与「' + clash.label +
        '」互斥（模型里只能指定一个）——请先清空后者，未写入 candidate。', h);
    }
  }
  if (!cfgWrite(c, loc, spec.path, r.value)) {
    return cfgApplyMsg(false, '定位不到要改的对象（配置可能已被改动）——请刷新后重试。', h);
  }
  // 伴生字段：清掉主字段时一并清掉它的从属字段（如清了 VNF 名，它的 vNIC 名也不该留着）
  if (r.value === '' && spec.clears) spec.clears.forEach((p) => cfgWrite(c, loc, p, ''));
  (h ? h.write : cfgWriteText)(c);
  // 保存按钮名按宿主给（对象页的按钮是「保存并提交」，配置页是「保存到 candidate」）
  return cfgApplyMsg(true, '已写入 candidate（未保存）——点「' + (h ? h.saveLabel : '保存到 candidate') +
    '」提交到服务端。', h);
}

// 新增条目（= 对应的 set 语句，只改 candidate）：标识先本地判（非空、不重名）。
function cfgAdd(cont, spec, rawVal, h) {
  const kind = CFG_KINDS[spec.keyKind || 'text'];
  const r = rawVal.trim() === '' ? { value: '' } : kind.parse(rawVal, spec);
  if (r.error) return cfgApplyMsg(false, spec.keyLabel + '：' + r.error + '——未新增。', h);
  if (r.value === '') return cfgApplyMsg(false, '请先填写' + spec.keyLabel + '。', h);
  let c;
  try {
    c = h ? h.read() : cfgText();
  } catch (e) {
    return cfgApplyMsg(false, '原始 JSON 语法错误，请先修正：' + e.message, h);
  }
  const holder = cfgHolder(c, cont, true);
  if (!holder) return cfgApplyMsg(false, '定位不到要新增到的位置——请刷新后重试。', h);
  if (holder[cont.list].some((x) => x && x[cont.key] === r.value)) {
    return cfgApplyMsg(false, spec.keyLabel + ' ' + r.value + ' 已存在——未新增。', h);
  }
  holder[cont.list].push(Object.assign({ [cont.key]: r.value }, spec.extra ? spec.extra() : {}));
  (h ? h.write : cfgWriteText)(c);
  (h ? h.rerender : pageRerender)(c); // 结构变了：表单按新的 candidate 重画（已改的字段都在草稿里，不会丢）
  return cfgApplyMsg(true, '已在 candidate 中新增' + spec.what + ' ' + r.value +
    '（未保存）——提交后才生效。', h);
}

// 删除条目（= 对应的 delete 语句，只改 candidate）。
function cfgRemove(cont, val, what, h) {
  const msg = h ? h.msg : cfgMsg;
  let c;
  try {
    c = h ? h.read() : cfgText();
  } catch (e) {
    msg('原始 JSON 语法错误，请先修正：' + e.message, true);
    return;
  }
  const holder = cfgHolder(c, cont, false);
  const i = holder ? holder[cont.list].findIndex((x) => x && x[cont.key] === val) : -1;
  if (i < 0) {
    msg('要删除的' + what + ' ' + val + '已不在 candidate 里——请刷新后重试。', true);
    return;
  }
  holder[cont.list].splice(i, 1);
  if (!holder[cont.list].length) delete holder[cont.list]; // 空数组是噪声，整段删掉
  (h ? h.write : cfgWriteText)(c);
  (h ? h.rerender : pageRerender)(c);
  msg('已从 candidate 中删除' + what + ' ' + val + '（未保存）——提交后才生效。', false);
}

// 清空整节（单对象节：NAT / LLDP），= `delete nat` 一类语句。
function cfgClearSection(path, what, h) {
  const msg = h ? h.msg : cfgMsg;
  let c;
  try {
    c = h ? h.read() : cfgText();
  } catch (e) {
    msg('原始 JSON 语法错误，请先修正：' + e.message, true);
    return;
  }
  let cur = c;
  for (let i = 0; i < path.length - 1; i++) cur = cur ? cur[path[i]] : undefined;
  if (!cur) {
    msg('candidate 里本来就没有' + what + '。', false);
    return;
  }
  delete cur[path[path.length - 1]];
  (h ? h.write : cfgWriteText)(c);
  (h ? h.rerender : pageRerender)(c);
  msg('已从 candidate 中删除' + what + '（未保存）——提交后才生效。', false);
}

// 控件 id 计数器：前缀与上面三节（`cfg-f-`）**分开**——同一份 DOM 里 id 不能重名，
// 否则 label 的 for 会指到别人的输入框（点标签跳错字段、自动化也取错元素）。
let cfgSeq = 0;

// 一个字段：label + 控件；改动即写回 candidate（本地判不过则不写并标红）。
// h = 渲染宿主（见 cfgHostOf）：闭包把它捕获下来——控件上的事件在渲染之后才发生，
// 那时按宿主自己的草稿/消息落点工作（配置页缺席 = 全局 #cfg-text / #cfg-msg）。
function cfgRenderField(parent, c, loc, spec, h) {
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
    (h ? h.msg : cfgMsg)(spec.label + '：正在本机派生根哈希…', false);
    const job = cfgPwDerive(id, inp, spec, pw, kind.prepare, clear, h);
    cfgPwInflight.add(job);
    let hash;
    try { hash = await job; } finally { cfgPwInflight.delete(job); }
    if (hash === null) {
      // 失败（或这次派生已被后来的输入作废）：明文仍在控件里，便于切到 HTTPS 后重试
      if (inp.value === pw) inp.className = 'invalid';
      return;
    }
    prepared = true;
    inp.className = cfgApplyField(loc, spec, hash, h) ? '' : 'invalid';
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
    inp.className = cfgApplyField(loc, spec, inp.value, h) ? '' : 'invalid';
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
function cfgRenderAdd(cont, spec, h) {
  const box = el('div', { class: 'cfg-add' });
  const id = 'cfg-df-' + (cfgSeq++);
  box.appendChild(el('label', { text: spec.keyLabel, for: id }));
  const inp = el('input', { type: 'text', id });
  if (spec.placeholder) inp.setAttribute('placeholder', spec.placeholder);
  box.appendChild(inp);
  const btn = wbtn({ type: 'button', class: 'small', text: spec.button || '新增' });
  btn.addEventListener('click', () => cfgAdd(cont, spec, inp.value, h));
  inp.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') { ev.preventDefault(); cfgAdd(cont, spec, inp.value, h); }
  });
  box.appendChild(btn);
  return box;
}

// 条目内的子表（成员端口 / 规则 / 路由 / vNIC / 数据盘…）：每条一块（标题 + 字段 + 删除），
// 底部一行新增。子表的数组字段挂在**条目**上（NAT 这类挂在对象上，见 sub.loc）。
function cfgRenderSub(sub, loc, c, h) {
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
      () => cfgRemove(cont, row[sub.key], sub.what || sub.title, h)));
    const grid = el('div', { class: 'cfg-fields' });
    (sub.fields || []).forEach((spec) => cfgRenderField(grid, c, rloc, spec, h));
    box.appendChild(grid);
    wrap.appendChild(box);
  });
  wrap.appendChild(cfgRenderAdd(cont, sub, h));
  return wrap;
}

// 一个条目：标题（标识字段的值，不可改——等同命令行的路径参数）+ 字段 + 子表 + 删除。
function cfgRenderItem(sec, loc, it, c, h) {
  const wrap = el('div', { class: 'cfg-sub' });
  const cont = { loc: [], list: sec.list, key: sec.key };
  wrap.appendChild(cfgRowHead(sec.title(it), () => cfgRemove(cont, it[sec.key], sec.what, h)));
  const grid = el('div', { class: 'cfg-fields' });
  (sec.fields || []).forEach((spec) => cfgRenderField(grid, c, loc, spec, h));
  wrap.appendChild(grid);
  (sec.sublists || []).forEach((sub) => wrap.appendChild(cfgRenderSub(sub, loc, c, h)));
  return wrap;
}

// 渲染一节：fieldset（标题 + 说明）+ 条目 + 新增行（单对象节则直接是字段 + 子表 + 清空）。
// opts（可选，对象页用，见 cfgHostOf）：本次渲染专属的重画回调 / 草稿落点 / 消息落点。
export function renderSection(box, sec, c, opts) {
  const h = cfgHostOf(opts);
  const fs = el('fieldset', { class: sec.wide ? 'cfg-wide' : '' });
  fs.appendChild(el('legend', { text: sec.legend }));
  if (sec.note) fs.appendChild(el('p', { class: 'muted small', text: sec.note }));
  if (sec.list) {
    const items = Array.isArray(c[sec.list]) ? c[sec.list] : [];
    if (!items.length) fs.appendChild(el('p', { class: 'muted small', text: '（未配置）' }));
    items.forEach((it) => fs.appendChild(cfgRenderItem(sec, [{ list: sec.list, key: sec.key, val: it[sec.key] }], it, c, h)));
    fs.appendChild(cfgRenderAdd({ loc: [], list: sec.list, key: sec.key },
      Object.assign({ what: sec.what }, sec.add), h));
  } else {
    // 单对象节（NAT / LLDP）：没有"条目"这层，字段与子表直接挂在节里
    const grid = el('div', { class: 'cfg-fields' });
    (sec.fields || []).forEach((spec) => cfgRenderField(grid, c, sec.loc || [], spec, h));
    if ((sec.fields || []).length) fs.appendChild(grid);
    (sec.sublists || []).forEach((sub) => fs.appendChild(cfgRenderSub(sub, sec.loc || [], c, h)));
    if (sec.clearPath) {
      const btn = wbtn({ type: 'button', class: 'small danger', text: '清空本节（' + sec.legend + '）' });
      btn.addEventListener('click', () => cfgClearSection(sec.clearPath, sec.legend + '配置', h));
      const row = el('div', { class: 'cfg-add' });
      row.appendChild(btn);
      fs.appendChild(row);
    }
  }
  box.appendChild(fs);
}

// 把一组节定义画进表单容器（系统段 / VPP 段 / 网络域与计算域共用）。
export function renderSections(box, list, c, opts) {
  list.forEach((sec) => renderSection(box, sec, c, opts));
}

// ---------- 对象页就地编辑（配置页去中心化的第一刀）----------
//
// 目标形态：每个对象的增删改回到它自己的列表页 / 详情页。面板用**配置页那一份节定义**
// （CFG_SECTIONS 里的一条，单一真源）经 renderSection 画出来，写路径与配置页 / 命令行
// 同一套 candidate 语义（取锁 → 写候选 → 校验 → 提交 → 释放），界面不另造第二套通道。

// objTxn：一次性 candidate 事务（对象页的保存 / 删除共用）。apply = 「写候选」这一步的
// Promise 工厂，之后统一做「校验 → 提交 → 释放」：
//   · 通用路径传 () => candidateAcquire(整份文档)——与配置页同一份文档语义（对象页的
//     新建 / 编辑 / 删除都走它）；
//   · **类型化单对象端点**（如 DELETE /qos/policies/{name}，服务端内部就是「取锁 + 写候选」）
//     也可以直接当 apply 传进来——本函数负责其后的校验、提交与释放。
// 与配置页的唯一区别是节奏：那里「保存草稿、等操作者再点提交」，这里保存即提交——
// 对象页的一次编辑就该只有一次事务。失败同样释放会话锁（不留脏锁给下一次登录），
// 返回的 text 是给操作者看的原文（含服务端 detail），调用方不许吞。
//
// opts.confirmAsk（可选，系统族专用）：服务端对「会改变管理路径」的变更（管理口地址/网关
// FR-CFG-012、主机防火墙 决策 #388）在非 console 会话上强制要求 commit confirmed——两条
// 拒绝文案都以「必须以 commit confirmed」起头。给了 confirmAsk 时：命中该拒绝**不丢弃**
// 候选与编辑锁，先让调用方问一次（返回确认窗口的分钟数），同意就带 confirmed_minutes 重提
// 并返回 pending:true（那一版已生效但带确认窗口，须由调用方给「确认在途提交」入口——与配置文件
// 页的「以 commit confirmed 提交 → 确认在途提交」是同一条路）；不同意则丢弃候选、如实说明。
// 未给 confirmAsk 的调用方（其余所有对象页）行为与从前逐字一致。
const CFG_CONFIRM_REQ_RE = /必须以 commit confirmed/;

// confirmRequiredAsk：服务端要求 commit confirmed 时的**第二次确认**（与配置页
// cfgCommitAsk 的「以 commit confirmed 提交」同一套口径、同一个动作）。同意返回分钟数
// （10，与配置页同值），不同意返回 0（调用方据此丢弃候选并如实说明）。
export async function confirmRequiredAsk(reason) {
  const ok = await uiConfirm('以 commit confirmed 提交（10 分钟）', {
    tier: 'mid',
    paragraphs: ['服务端要求这次变更以 commit confirmed 提交：' + (reason || '（未给原因）')],
    bullets: [
      '提交立即生效；窗口期内不点「确认在途提交」就自动回滚到提交前那一版。',
      '这是防止改坏管理路径把自己锁在门外的自锁保护：确认管理面仍然连得上，再点「确认在途提交」。',
      '变更会立即下发给各子系统，相关业务可能随变更中断（管理口/防火墙这类）。',
    ],
    cli: 'commit confirmed 10',
    confirmLabel: '提交',
  });
  return ok ? 10 : 0;
}

export async function objTxn(apply, opts) {
  // 本会话在「配置」页还有未提交的草稿时拒绝（与用户管理页同一口径）：这里写的是
  // **整份**配置（或把改动叠进同一份候选），提交会连那份草稿一起提交，不替操作者做这个决定。
  let existing = null;
  try { existing = await api('/configuration/candidate'); } catch (e) { existing = null; }
  if (existing && existing.dirty) {
    return { ok: false, text: '本会话在「配置」页还有未提交的候选改动——请先在那页提交或丢弃它，再回来改这里（本操作会把它一并提交 / 覆盖）。' };
  }
  try {
    await apply();
  } catch (e) {
    return { ok: false, text: '写入候选失败（编辑锁可能正被别的会话持有，或改动本身被拒绝）：' + e.message };
  }
  try {
    const chk = await candidateCheck();
    if (!chk || !chk.ok) {
      const errs = (chk && chk.errors) || [];
      await candidateRelease().catch(() => {});
      return { ok: false, text: '预校验未通过（与服务端提交同一套全部校验；候选已丢弃、未提交）：' +
        (errs.length ? errs.map((x) => (x.path ? x.path + '：' : '') + x.message).join('；') : '服务端校验未通过') };
    }
    const res = await candidateCommit({});
    await candidateRelease().catch(() => {});
    const warns = (res && res.warnings) || [];
    return { ok: true, revision: res && res.revision,
      text: '提交成功（revision ' + (res && res.revision) + '）' +
        (warns.length ? '；提示：' + warns.join('；') : '') };
  } catch (e) {
    // 自锁保护：候选与编辑锁都还在，先问一次再决定——不同意就按老口径丢弃并如实说明。
    if (opts && typeof opts.confirmAsk === 'function' && CFG_CONFIRM_REQ_RE.test(e.message || '')) {
      let minutes = 0;
      try { minutes = Number(await opts.confirmAsk(e.message)) || 0; } catch (e2) { minutes = 0; }
      if (minutes > 0) {
        try {
          const res = await candidateCommit({ confirmed_minutes: minutes });
          const warns = (res && res.warnings) || [];
          // 有意**不**释放候选/编辑锁：确认窗口要靠同一会话确认（POST /configuration/commit:confirm
          // 须由持锁会话发起，决策 #364）；释放了就没法确认，只能等它自动回滚。
          return { ok: true, revision: res && res.revision, pending: true,
            confirmedUntil: res && res.confirmed_until,
            text: '提交成功（revision ' + (res && res.revision) + '）：这一版已生效，但设有 ' + minutes +
              ' 分钟确认窗口——请确认管理面仍然连得上，再点「确认在途提交」；窗口内不确认会自动回滚到提交前那一版' +
              (warns.length ? '；提示：' + warns.join('；') : '') };
        } catch (e2) {
          await candidateRelease().catch(() => {});
          return { ok: false, text: '失败（候选已丢弃、编辑锁已释放）：' + e2.message };
        }
      }
      await candidateRelease().catch(() => {});
      return { ok: false, text: '未提交：' + e.message + '（已丢弃候选并释放编辑锁）' };
    }
    await candidateRelease().catch(() => {});
    return { ok: false, text: '失败（候选已丢弃、编辑锁已释放）：' + e.message };
  }
}

// objEditor：把一个「对象种类」的节画进页面自己的面板，并负责保存 / 删除两条一次性事务。
//
// opts:
//   slot   —— 面板落点（元素或元素 id；面板与页面级提示都由编辑器建在这块容器里）
//   reload —— 成功后的收尾（页面刷新自己，通常是 router 的重渲染）
// kind = { section, sections, label, cliNew, cliSave, cliDelete, note, bullets, confirmAsk }：
//   section   —— 节定义（CFG_SECTIONS 里的那一条）；有 list = 多对象节，无 list = 单对象节
//   sections  —— 可选（系统族用）：**一组**节定义，同一面板里按顺序画出来（如系统页把
//                「系统（基础）／日志／健康阈值／管理口／API 与 TLS」画在一个面板里）。
//                与 section 二选一；一组节里目前都是单对象节（没有 list 那层）。任一节缺失
//                时调用方应传空数组（renderSection 找不到节就无从画），面板据此如实拒绝打开。
//   label     —— 操作者见到的对象类名（「虚拟交换机」…），用于标题与确认框
//   cliNew / cliSave / cliDelete —— 确认框里只读回显的 CLI 等价语句（工单对照用）
//   note(mode, name)    —— 可选：追加在面板说明后的一段对象类专属提示（如运行中 VM 的形态变更
//                          须 stop+start 才生效——产品口径 FR-CMP-012，UI 不与之矛盾）
//   bullets(mode, name) —— 可选：追加在保存确认框影响面里的条目（与 note 同一口径）
//   confirmAsk          —— 可选：服务端要求 commit confirmed 时的第二次确认（见 objTxn 与
//                          confirmRequiredAsk）；只有「会改变管理路径」的节（管理口/防火墙）需要。
//
// 底稿 = 当前**已生效配置**的全量克隆：面板里改的只是这份草稿，点「保存并提交」才写服务端。
// 多对象节的显示面 = 草稿里「本次编辑的那些条目」（编辑：原对象 + 面板内新增的；新建：只
// 显示新增的）——其余条目原样留在草稿里，提交时整份写回，不会误删其它对象。
export function objEditor(opts) {
  const slot = typeof opts.slot === 'string' ? $(opts.slot) : opts.slot;
  if (!slot) return null;

  const panel = el('div', { class: 'obj-edit', hidden: '' });
  const title = el('h3');
  const note = el('p', { class: 'muted small' });
  const forms = el('div', { class: 'cfg-forms' });
  const actions = el('div', { class: 'cfg-actions' });
  const saveBtn = wbtn({ type: 'button', class: 'primary', text: '保存并提交' });
  const cancelBtn = el('button', { type: 'button', class: 'ghost', text: '取消' });
  const pMsg = el('p', { class: 'muted small', hidden: '' });
  const pageMsg = el('p', { class: 'muted small', hidden: '' });
  actions.appendChild(saveBtn);
  actions.appendChild(cancelBtn);
  panel.appendChild(title);
  panel.appendChild(note);
  panel.appendChild(forms);
  panel.appendChild(actions);
  panel.appendChild(pMsg);
  slot.appendChild(panel);
  slot.appendChild(pageMsg);

  // —— 在途 commit confirmed 的确认入口（管理口/防火墙这类「会改变管理路径」的变更）——
  //
  // 只有服务端要求 confirmed 的提交才会走到这里（见 objTxn 的 opts.confirmAsk）：那一版配置
  // **已生效但带确认窗口**，窗口内不点「确认在途提交」就自动回滚。这个按钮就是配置页
  // 「确认在途提交」的同一个动作（POST /configuration/commit:confirm，须由持锁会话发起）。
  // 这一行挂在面板**外面**：面板收起（close）、切到别处（leave）时它都还在——待确认的在途
  // 提交不因面板关闭而消失（整页刷新会丢掉它，那时按提示到「配置」页或命令行确认）。
  const pendingBox = el('div', { hidden: '' });
  const pendingNote = el('p', { class: 'muted small' });
  const pendingActions = el('div', { class: 'cfg-actions' });
  const pendingBtn = wbtn({ type: 'button', class: 'danger', text: '确认在途提交' });
  pendingActions.appendChild(pendingBtn);
  pendingBox.appendChild(pendingNote);
  pendingBox.appendChild(pendingActions);
  slot.appendChild(pendingBox);
  const setPending = (text, isErr, done) => {
    pendingBox.hidden = !text;
    pendingNote.textContent = text || '';
    pendingNote.className = isErr ? 'error small' : 'muted small';
    pendingActions.hidden = !!done; // 确认成功后收掉按钮，只留结果回显
  };
  const clearPending = () => setPending('', false, false);
  pendingBtn.addEventListener('click', async () => {
    setPending('确认中…', false);
    try {
      await api('/configuration/commit:confirm', { method: 'POST' });
      await candidateRelease().catch(() => {}); // 与配置页同口径：确认后结束编辑会话、释放编辑锁
      setPending('已确认，提交生效（本会话的编辑锁已释放）。', false, true);
    } catch (e) {
      setPending('确认失败：' + e.message +
        '——窗口到点会自动回滚；也可到「配置」页（无变更提交会隐式确认在途提交）或命令行确认。', true);
    }
  });

  let cur = null; // { kind, mode, name, sec, secs, working, origin, keep, existed }

  const setText = (node, text, isErr) => {
    node.hidden = !text;
    node.textContent = text || '';
    node.className = isErr ? 'error small' : 'muted small';
  };
  const pOut = (text, isErr) => setText(pMsg, text, isErr);
  const gOut = (text, isErr) => setText(pageMsg, text, isErr);

  // 宿主：面板的草稿读写、重画与消息都落在自己这一份上（见 cfgHostOf）。
  function host() {
    return {
      rerender: (work) => { if (cur) { cur.working = work; draw(); } },
      read: () => cur.working,
      write: (work) => { cur.working = work; },
      msg: pOut,
      saveLabel: '保存并提交',
    };
  }

  // 显示面：多对象节只画「本次编辑的条目」（面板内经「新增」建出来的自动并入）；单对象节整份画。
  function projection() {
    if (!cur.sec.list) return cur.working;
    const items = Array.isArray(cur.working[cur.sec.list]) ? cur.working[cur.sec.list] : [];
    items.forEach((it) => { if (it && !cur.origin.has(it[cur.sec.key])) cur.keep.add(it[cur.sec.key]); });
    return { [cur.sec.list]: items.filter((it) => it && cur.keep.has(it[cur.sec.key])) };
  }

  function draw() {
    cfgPwResetAll(); // 面板重画：旧控件上的口令登记一并作废
    forms.textContent = '';
    // 单一节与一组节（kind.sections）走同一条渲染路径：逐个 renderSection 画进同一个表单容器。
    cur.secs.forEach((s) => renderSection(forms, s, projection(), host()));
  }

  function close() {
    cur = null;
    forms.textContent = '';
    panel.hidden = true;
    pOut('', false);
  }

  // 读一次「已生效配置」当底稿；读不到 / 被挡时返回 null 并把原因如实写出来。
  async function takeBase(kind, mode, name) {
    // kind.sections（一组节，系统族用）与 kind.section（单节）二选一：统一成 secs 走后续逻辑；
    // 一组节里目前都是单对象节，故多对象分支（list 那层）按 secs[0] 判——两处不混用。
    const secs = kind && (Array.isArray(kind.sections) ? kind.sections
      : (kind.section ? [kind.section] : [])) || [];
    if (!secs.length) {
      gOut('页面上找不到' + ((kind && kind.label) || '该对象') + '的节定义（配置节被改动？）——无法就地编辑。', true);
      return null;
    }
    let existing = null;
    try { existing = await api('/configuration/candidate'); } catch (e) { existing = null; }
    if (existing && existing.dirty) {
      gOut('本会话在「配置」页还有未提交的候选改动——请先在那页提交或丢弃它，再回来改这里（本操作写整份配置，会覆盖那份草稿）。', true);
      return null;
    }
    let committed;
    try {
      committed = await api('/configuration');
    } catch (e) {
      gOut('读取当前配置失败：' + e.message, true);
      return null;
    }
    const working = JSON.parse(JSON.stringify((committed && committed.configuration) || {}));
    const sec = secs[0];
    const origin = new Set();
    const keep = new Set();
    let existed = false;
    if (sec.list) {
      const items = Array.isArray(working[sec.list]) ? working[sec.list] : [];
      items.forEach((it) => { if (it && it[sec.key] !== undefined) origin.add(it[sec.key]); });
      if (mode === 'edit') {
        if (!items.some((it) => it && it[sec.key] === name)) {
          gOut('已生效配置里没有' + kind.label + ' ' + name + '（可能已被删除）——请刷新后重试。', true);
          return null;
        }
        keep.add(name);
        existed = true;
      }
      // upsert（按标识编辑或新建，如镜像启动画像按镜像名）：条目不在已生效配置里就先在草稿里
      // 建出来——操作者从对象页点「编辑画像」时，尚未配置的镜像也该能一步建好，而不是先让他
      // 在空表单里重复输入一遍对象名。
      if (mode === 'upsert') {
        existed = items.some((it) => it && it[sec.key] === name);
        if (!existed) {
          items.push({ [sec.key]: name });
          working[sec.list] = items;
        }
        keep.add(name);
      }
    }
    return { kind, mode, name: name || '', sec, secs, working, origin, keep, existed };
  }

  async function open(kind, mode, name) {
    close();
    if (mode === 'upsert' && !String(name || '').trim()) {
      gOut('请先选中要编辑' + ((kind && kind.label) || '') + '的对象（名不能为空）。', true);
      return false;
    }
    cur = await takeBase(kind, mode, name);
    if (!cur) return false;
    const sec = cur.sec;
    const isCreate = mode === 'new' || (mode === 'upsert' && !cur.existed);
    title.textContent = (isCreate ? '新建 ' : '编辑 ') + kind.label + (name ? '：' + name : '');
    if (mode === 'new' && sec.list) {
      note.textContent = '先在下面「' + (sec.add ? sec.add.keyLabel : '名称') + '」里填名字并点「新增」，再填其余字段；' +
        '所有改动只进本页草稿，点「保存并提交」才写服务端（取编辑锁 → 写候选 → 校验 → 立即提交生效）。';
    } else if (mode === 'upsert' && !cur.existed) {
      note.textContent = '当前还没有这条' + kind.label + '（键 = ' + name + '）：字段改动只进本页草稿，' +
        '点「保存并提交」才写服务端（取编辑锁 → 写候选 → 校验 → 立即提交生效）。';
    } else {
      note.textContent = '字段与子表改动只进本页草稿（尚未写服务端）；点「保存并提交」才会取编辑锁 → 写候选 → ' +
        '校验 → 立即提交生效（失败原因原样显示，不会吞掉）。' +
        (sec.list ? '要删掉这个对象，用页面上的「删除」按钮。' : '');
    }
    // 对象类专属提示（如运行中 VM 的形态变更须 stop+start 生效）——由 kind.note 给，无则不加。
    const extraNote = typeof kind.note === 'function' ? String(kind.note(mode, name) || '') : '';
    if (extraNote) note.textContent += ' ' + extraNote;
    draw();
    panel.hidden = false;
    if (panel.scrollIntoView) panel.scrollIntoView({ block: 'nearest' });
    return true;
  }

  async function save() {
    if (!cur) return;
    const shown = projection(); // 顺带把面板内新增的条目并入本次编辑面
    const created = cur.sec.list ? shown[cur.sec.list].map((it) => it[cur.sec.key]) : [];
    if (cur.sec.list && !created.length) {
      pOut('还没有任何' + cur.kind.label + '可保存——先在下面新增一个（或点「取消」关掉面板）。', true);
      return;
    }
    const echoName = cur.name || created[0] || '<name>';
    const isCreate = cur.mode !== 'edit' && !(cur.mode === 'upsert' && cur.existed);
    const what = (isCreate ? '新建' + cur.kind.label
      : '保存' + cur.kind.label + (cur.name ? ' ' + cur.name : ''));
    // 对象类专属影响面条目（如运行中 VM 的形态变更须 stop+start 生效）——由 kind.bullets 给。
    const extraBullets = typeof cur.kind.bullets === 'function' ? (cur.kind.bullets(cur.mode, cur.name) || []) : [];
    const ok = await uiConfirm(what + ' 并提交', {
      tier: 'mid',
      bullets: [
        '写入即提交：会先取配置编辑锁、把整份候选写到服务端并跑与提交相同的全部校验，通过后立即下发生效（revision 加一）——不是保存草稿。',
        '校验不通过会整体拒绝并原样显示原因（未提交）；提交失败由服务端事务引擎补偿，补偿残渣会进告警。',
        '相关业务可能随变更短暂中断（改动涉及接口 / ACL / NAT / 网关这类运行态时）。',
        '可回退：「提交历史」里能把上一版取为候选、再提交回来。',
      ].concat(extraBullets),
      cli: isCreate
        ? (cur.kind.cliNew ? cur.kind.cliNew(echoName) : 'set … + commit')
        : (cur.kind.cliSave ? cur.kind.cliSave(echoName) : 'commit'),
      confirmLabel: '提交',
    });
    if (!ok) return;
    pOut('提交中…', false);
    // confirmAsk（可选）由 kind 给：服务端要求 commit confirmed 时（管理口/防火墙）走同一会话的
    // 确认窗口路径，而不是把候选丢掉报失败——见 objTxn 与 confirmRequiredAsk。
    const res = await objTxn(() => candidateAcquire(cur.working), { confirmAsk: cur.kind.confirmAsk });
    if (!res.ok) { pOut(res.text, true); return; } // 面板留着：草稿还在，按报错修正后可重试
    close();
    if (res.pending) {
      // 这一版已生效但带确认窗口：把「确认在途提交」入口摆在面板位置（含服务端给的提示原文）。
      gOut('', false);
      setPending(res.text + '（请在本页确认；刷新页面后本行会消失——那时可在「配置」页点一次「提交」隐式确认，或到命令行确认。）', false);
    } else {
      clearPending(); // 这次提交成功即隐式确认了在途的 confirmed（服务端口径），待确认行随之作废
      gOut(res.text, false);
    }
    if (opts.reload) { try { await opts.reload(); } catch (e) { /* 刷新失败不该盖掉提交成功的回显 */ } }
  }

  // remove：删除一个已生效对象（中危确认 → 同一套一次性事务）。被引用的对象删不掉时服务端
  // 会拒绝并指出谁在引用——界面照实转述，不猜解引用顺序。
  async function remove(kind, name) {
    if (!kind || !kind.section) {
      gOut('页面上找不到' + ((kind && kind.label) || '该对象') + '的节定义——无法删除。', true);
      return false;
    }
    let committed;
    try {
      committed = await api('/configuration');
    } catch (e) {
      gOut('读取当前配置失败：' + e.message, true);
      return false;
    }
    const working = JSON.parse(JSON.stringify((committed && committed.configuration) || {}));
    const sec = kind.section;
    if (sec.list) {
      const items = Array.isArray(working[sec.list]) ? working[sec.list] : [];
      const i = items.findIndex((it) => it && it[sec.key] === name);
      if (i < 0) {
        gOut('已生效配置里没有' + kind.label + ' ' + name + '（可能已被删除）——请刷新后重试。', true);
        return false;
      }
      items.splice(i, 1);
      if (!items.length) delete working[sec.list];
    } else {
      // 单对象节（NAT）：整节清掉 = `delete nat`
      const path = sec.clearPath || [];
      let node = working;
      for (let i = 0; i + 1 < path.length && node; i++) node = node[path[i]];
      if (path.length && node) delete node[path[path.length - 1]];
    }
    const ok = await uiConfirm('删除' + kind.label + (name ? ' ' + name : ''), {
      tier: 'mid',
      bullets: [
        '删除会同时取编辑锁、写整份候选、校验并立即提交生效（revision 加一）——不是只写候选等下次提交。',
        '被别的对象引用的对象删不掉：服务端会拒绝并指出谁在引用，原因原样显示（这里不替你猜解引用顺序）。',
        '生效后该对象在数据面上的配置随之撤销，相关流量与业务可能中断。',
        '可回退：「提交历史」里能把上一版取为候选、再提交回来。',
      ],
      cli: kind.cliDelete ? kind.cliDelete(name) : 'delete ' + name,
      confirmLabel: '删除',
    });
    if (!ok) return false;
    gOut('删除中…', false);
    const res = await objTxn(() => candidateAcquire(working));
    gOut(res.text, !res.ok);
    if (res.ok) clearPending(); // 成功提交即隐式确认了在途的 confirmed（服务端口径）
    if (res.ok && opts.reload) { try { await opts.reload(); } catch (e) { /* 刷新失败不盖掉结果回显 */ } }
    return res.ok;
  }

  saveBtn.addEventListener('click', () => { save(); });
  cancelBtn.addEventListener('click', () => close());

  return {
    openNew: (kind) => open(kind, 'new', ''),
    openEdit: (kind, name) => open(kind, 'edit', name),
    // openUpsert：按标识编辑或新建（条目不存在就先在草稿里建出再编辑）——供「按名索引的
    // 画像/覆盖类」对象用（如镜像启动画像：镜像名即键，未配置的镜像也该能一步建好）。
    openUpsert: (kind, name) => open(kind, 'upsert', name),
    remove: (kind, name) => remove(kind, name),
    close,
    leave: () => close(), // 离开页面（router 的 leave 钩子）时收掉面板，草稿不跨页残留
  };
}
