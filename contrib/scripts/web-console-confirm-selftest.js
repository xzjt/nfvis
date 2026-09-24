// 控制台「分级确认」（低 / 中 / 高危三档）的桩式自校准（Node 里的最小 DOM 桩 + 真实 app.js）。
//
// 判据（为什么这么写）：
//   ① 确认强度**必须真的拦得住**：高危档在「确认词不匹配」或「倒计时没走完」时，
//      「执行」按钮必须点不动——不是"提示了一句"就算数。故断言直接看按钮的 disabled 与
//      服务端调用记录（桩 fetch）：**确认之前一条请求都不许发**，取消之后也不许发。
//   ② 档位归位要能挡住回归：中危动作必须列出影响面 + 主按钮标红，低危不许变成一屏字。
//      这条由「动作 → 档位」表驱动，改档位即报 ✗。
//   ③ 不许退回浏览器自带的 confirm：它没有影响面、没有只读回显、没有闸门。桩把
//      window.confirm 记下来，整轮下来必须是 0 次。
//
// 手法：把真正的 internal/api/ui/app.js 整份跑在 vm 里（不另写一份实现，否则测的不是实现），
// 配最小 DOM 桩驱动真实处理器：点按钮 → 框建出来 → 断言 → 取消/确认 → 看桩 fetch 记录。
//
// 用法：node contrib/scripts/web-console-confirm-selftest.js
// 红-绿（拆掉闸门应报 ✗）：node … --mutate <变异名> <输出文件> 生成变异体，
//   WEB_CONSOLE_APP_JS=<变异体> node … 必须报 ✗ —— 由同名 .sh 自动跑这一条。
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const APP_JS = process.env.WEB_CONSOLE_APP_JS ||
  path.join(__dirname, '..', '..', 'internal', 'api', 'ui', 'app.js');
// 骨架永远取仓库里那一份（红-绿跑的是 app.js 的变异体，骨架没变——别跟着变异体去临时目录找）。
const INDEX_HTML = path.join(__dirname, '..', '..', 'internal', 'api', 'ui', 'index.html');

// app.js 是 ES 模块（index.html 用 `type="module"` 加载），只有 `export` 没有静态 `import`
// （唯一的 import 是 routerModule() 里的动态 `import('./router.js')`，本桩用不到它——
// 动态 import 在 vm 里会以 ERR_VM_DYNAMIC_IMPORT_CALLBACK_MISSING 拒绝，动作收尾的
// reload() 已被 .catch 吞掉，不影响本桩的断言）。
const APP_SRC_RAW = fs.readFileSync(APP_JS, 'utf8');
if (/^\s*import\s/m.test(APP_SRC_RAW)) {
  throw new Error(APP_JS + ' 出现了静态 import——本桩按经典脚本跑，需改用模块方式加载');
}
const APP_SRC = APP_SRC_RAW.replace(/^export /gm, '');
if ((APP_SRC_RAW.match(/^export /gm) || []).length < 3) {
  throw new Error(APP_JS + ' 的顶层 export 变少了？本桩的剥法要跟着改');
}

// ---- 红-绿用的变异表：每条都对着闸门/档位的一行实现，锚点必须唯一 ----
const MUTATIONS = {
  // 两道闸门一起拆掉
  gates: { find: 'const gatesOK = () => wordOK && timeOK;', replace: 'const gatesOK = () => true;' },
  // 只拆确认词校验
  word: {
    find: 'wordOK = !wordMissing && wordInput.value.trim() === word;',
    replace: 'wordOK = true;',
  },
  // 只拆倒计时（初始就当成"时间已到"）
  timer: { find: 'let timeOK = countdown <= 0;', replace: 'let timeOK = true;' },
  // 把「重启主机」从高危档（中危）降级——档位归位的回归
  reboot: {
    find: "$('ops-reboot-btn').addEventListener('click', () => opsRun('重启主机', {\n  tier: 'mid',",
    replace: "$('ops-reboot-btn').addEventListener('click', () => opsRun('重启主机', {\n  tier: 'low',",
  },
  // 把「恢复出厂」从高危档降级——本段新增高危动作的档位回归（拆掉即报 ✗）
  zeroize: {
    find: "tier: 'high',\n    requireWord: 'zeroize',",
    replace: "tier: 'mid',\n    requireWord: 'zeroize',",
  },
  // 去掉 DPDK 绑定请求里的 confirm=true（服务端要求它）——"界面不得绕过服务端要求"的回归
  dpdk: {
    find: "'/dpdk?confirm=true'",
    replace: "'/dpdk'",
  },
  // 把下载路径的错误文案退回裸状态码——"服务端的原话要到达操作者"的回归（⑧ 应报 ✗）
  rawhttp: {
    find: 'if (!res.ok) throw new Error(await httpErrText(res));',
    replace: "if (!res.ok) throw new Error('HTTP ' + res.status);",
  },
};

if (process.argv[2] === '--mutate') {
  const name = process.argv[3];
  const out = process.argv[4];
  const m = MUTATIONS[name];
  if (!m || !out) {
    console.error('用法：node … --mutate <' + Object.keys(MUTATIONS).join('|') + '> <输出文件>');
    process.exit(2);
  }
  const hits = APP_SRC_RAW.split(m.find).length - 1;
  if (hits !== 1) {
    console.error('✗ 变异 ' + name + ' 的锚点在 app.js 里命中 ' + hits + ' 处（期望 1）——锚点变了，本自校准要跟着改');
    process.exit(2);
  }
  fs.writeFileSync(out, APP_SRC_RAW.split(m.find).join(m.replace));
  process.exit(0);
}

// ---- 最小 DOM 桩：只实现 app.js 真的用到的那几个成员 ----
// 与浏览器对齐的两处关键行为（上一轮的教训：桩比目标环境宽松，缺陷就漏网了）：
//   · 给 textContent 赋空串会**清掉子节点**（组件每次开框都靠它清空闸门区）；
//   · 节点被清掉/移除后，它的 id 不再能通过 getElementById 找到（浏览器就是这么找的）。
function makeDom() {
  const byId = new Map();
  const created = [];

  function register(node) {
    if (node.attrs && node.attrs.id) byId.set(node.attrs.id, node);
    node.children.forEach(register);
  }
  function unregister(node) {
    if (node.attrs && node.attrs.id && byId.get(node.attrs.id) === node) byId.delete(node.attrs.id);
    node.children.forEach(unregister);
  }

  function mk(tag) {
    const node = {
      tagName: String(tag).toUpperCase(),
      attrs: {}, listeners: {}, children: [],
      value: '', className: '', hidden: false, disabled: false, parent: null, _text: '',
      appendChild(c) { this.children.push(c); c.parent = this; register(c); return c; },
      insertBefore(c) { return this.appendChild(c); },
      removeChild(c) {
        const i = this.children.indexOf(c);
        if (i >= 0) { this.children.splice(i, 1); c.parent = null; unregister(c); }
        return c;
      },
      setAttribute(k, v) {
        this.attrs[k] = String(v);
        if (k === 'id') byId.set(String(v), this);
      },
      getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
      hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k); },
      removeAttribute(k) {
        if (k === 'id' && byId.get(this.attrs.id) === this) byId.delete(this.attrs.id);
        delete this.attrs[k];
      },
      addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
      removeEventListener() {},
      focus() { this.focused = true; }, blur() {}, click() {},
      // 惰性子节点：渲染函数里的 `$('x').querySelector('tbody')` 用得上（本桩不驱动列表渲染）
      querySelector(sel) {
        const key = '__q_' + sel;
        if (!this[key]) this[key] = mk('div');
        return this[key];
      },
      querySelectorAll() { return []; },
      get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); },
      set textContent(v) {
        this.children.slice().forEach((c) => { c.parent = null; unregister(c); });
        this.children.length = 0;
        this._text = v === undefined || v === null ? '' : String(v);
      },
    };
    created.push(node);
    return node;
  }

  const doc = {
    createElement: mk,
    // 未知 id 按需造一个：app.js 在脚本求值期就会给一堆控件挂监听（index.html 的骨架节点）
    getElementById(id) {
      const key = String(id);
      if (!byId.has(key)) mk('div').setAttribute('id', key);
      return byId.get(key) || null;
    },
    addEventListener() {}, removeEventListener() {},
    querySelector() { return null; }, querySelectorAll() { return []; },
  };
  doc.body = mk('body');
  // `has`：判断某个 id 当前是否**真的挂在文档上**（不触发按需造节点——这正是浏览器
  // getElementById 的语义，也是本桩要能分辨"闸门区有没有那个输入框"的判据）。
  return { doc, created, has: (id) => byId.has(String(id)) };
}

// 桩 fetch：记录每一次服务端调用（判「确认之前有没有偷偷发请求」的唯一事实源）。
// `blob`：恢复配置那条路径要把归档从服务端取回来、再作为 multipart 上传，故桩要给 blob()。
// `state.dirty`：模拟"本会话在配置页还有未提交的候选"（用户页的写操作应先被拦下）。
function makeFetch(calls, state) {
  return async (url, opts) => {
    const o = opts || {};
    const method = (o.method || 'GET').toUpperCase();
    const u = String(url);
    calls.push({ url: u, method, body: o.body === undefined ? null : String(o.body) });
    let body = {};
    if (/\/configuration\/commit$/.test(u)) body = { revision: 8 };
    else if (/\/configuration\/candidate$/.test(u)) {
      body = { candidate: o.body ? JSON.parse(String(o.body)) : {}, dirty: state.dirty };
    } else if (/\/system\/configuration\/sessions$/.test(u)) {
      // 编辑锁会话列表：脏候选时把锁挂在当前用户（admin@api）名下，供用户页的前置检查读。
      body = state.dirty ? [{ holder: 'admin@api', acquired_at: '2026-09-24T00:00:00Z' }] : [];
    }
    return {
      ok: true, status: 200, json: async () => body, text: async () => '',
      // 真 Blob（不是普通对象）：恢复配置那条路径要把归档塞进 FormData，
      // 而 FormData.append 只接受 Blob/File——用假对象会让这条路径**静默走不通**。
      blob: async () => new Blob(['{"format":"nfvis-config-backup"}'], { type: 'application/json' }),
    };
  };
}

// 假时钟：只接管 setInterval（app.js 的高危倒计时用它）——setTimeout 仍是真的，
// 因为桩的 tick() 靠它让事件循环转起来。这样「等 10 秒倒计时」在测试里是瞬时的，
// 而且能**确定地**断言"差 1 秒也不能点"（真等待会变成碰运气）。
function makeClock() {
  let seq = 0;
  const timers = new Map(); // id → { fn }
  return {
    setInterval(fn) { const id = ++seq; timers.set(id, { fn }); return id; },
    clearInterval(id) { timers.delete(id); },
    live: () => timers.size,
    advance(seconds) {
      for (let s = 0; s < seconds; s++) {
        for (const [id, t] of Array.from(timers)) {
          if (!timers.has(id)) continue; // 回调里可能把自己清掉（倒计时走完就是）
          t.fn();
        }
      }
    },
  };
}

function makeContext() {
  const { doc, created, has } = makeDom();
  const calls = [];
  const confirms = []; // 浏览器自带 confirm 的调用记录：整轮下来必须是 0 次
  const clock = makeClock();
  const state = { dirty: false }; // 「本会话在配置页有未提交候选」——由用例按需打开
  const win = {
    addEventListener() {}, removeEventListener() {},
    confirm(msg) { confirms.push(String(msg)); return true; },
  };
  const sandbox = {
    document: doc,
    window: win,
    location: {
      pathname: '/api/v1/ui/', protocol: 'http:', host: '127.0.0.1:8443', hash: '',
      href: 'http://127.0.0.1:8443/api/v1/ui/',
    },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    navigator: { userAgent: 'stub' },
    // 本桩不驱动配置页的口令路径（那是另一支自校准的事），给一个最小实现即可
    crypto: { getRandomValues(a) { for (let i = 0; i < a.length; i++) a[i] = (i * 7 + 1) & 0xff; return a; } },
    fetch: makeFetch(calls, state),
    btoa: (s) => Buffer.from(String(s), 'binary').toString('base64'),
    atob: (s) => Buffer.from(String(s), 'base64').toString('binary'),
    console, setTimeout, clearTimeout, setInterval: clock.setInterval, clearInterval: clock.clearInterval,
    TextEncoder, TextDecoder, FormData,
    URL: { createObjectURL: () => 'blob:stub', revokeObjectURL() {} },
    WebSocket: function WebSocket() { this.close = () => {}; },
    EventSource: function EventSource() { this.close = () => {}; },
    __stub: { doc, calls, confirms, created, has, advance: clock.advance, clockLive: clock.live, setDirty: (v) => { state.dirty = !!v; } },
  };
  sandbox.globalThis = sandbox;
  const ctx = vm.createContext(sandbox);
  vm.runInContext(APP_SRC, ctx, { filename: 'internal/api/ui/app.js' });
  return ctx;
}

// 写请求（方法不是 GET）：判「确认之前有没有偷偷动手」的唯一事实源。
// 只读预检（如用户页在确认前先看本会话有没有未提交的候选）不算"动手"，故按方法过滤。
const writes = (ctx) => ctx.__stub.calls.filter((c) => c.method !== 'GET');


// ---- 驱动 ----

const tick = (ms) => new Promise((r) => setTimeout(r, ms || 0));

const nodeOf = (ctx, id) => ctx.__stub.doc.getElementById(id);

// 触发某个节点的某类事件（返回所有监听器的 promise）。**点按钮开框时不要 await 它**：
// 那个 promise 要等确认框被点了才会完成。
function fire(ctx, id, type, extra) {
  const node = nodeOf(ctx, id);
  if (!node) throw new Error('桩里找不到节点 ' + id);
  const hs = node.listeners[type] || [];
  const ev = Object.assign({ type, target: node, preventDefault() {}, stopPropagation() {} }, extra || {});
  return Promise.all(hs.map((h) => h(ev)));
}

// 确认框的当前状态（一律从 DOM 读，不看内部变量——界面给人看到的就是这些）
// 注意：闸门区的控件是**每次开框现建**的，故用 `has()` 判断"有没有"，不能用 getElementById
// （桩的 getElementById 会按需造节点，那正是"桩比浏览器宽松"的坑）。
function dialog(ctx) {
  const doc = ctx.__stub.doc;
  const body = nodeOf(ctx, 'modal-body');
  const cli = nodeOf(ctx, 'modal-cli');
  const ok = nodeOf(ctx, 'modal-ok');
  const guard = nodeOf(ctx, 'modal-guard');
  const hasWord = ctx.__stub.has('modal-word');
  const hasCount = ctx.__stub.has('modal-count');
  const word = hasWord ? nodeOf(ctx, 'modal-word') : null;
  const uls = body.children.filter((c) => c.tagName === 'UL');
  return {
    visible: nodeOf(ctx, 'modal').hidden === false,
    title: nodeOf(ctx, 'modal-title').textContent,
    paragraphs: body.children.filter((c) => c.tagName === 'P').map((c) => c.textContent),
    bullets: uls.reduce((n, ul) => n + ul.children.length, 0),
    cli: cli.hidden ? '' : cli.textContent,
    okDisabled: ok.disabled === true,
    okDanger: /(^|\s)danger(\s|$)/.test(ok.className),
    okLabel: ok.textContent,
    guardHidden: guard.hidden === true,
    guardText: guard.textContent,
    hasWord: hasWord,
    wordDisabled: !!word && word.disabled === true,
    count: hasCount ? nodeOf(ctx, 'modal-count').textContent : '',
  };
}

// 输入确认词（逐键输入的等价物：设值 + input 事件，与真人打字走到同一条监听器）
async function typeWord(ctx, text) {
  const inp = nodeOf(ctx, 'modal-word');
  if (!inp) throw new Error('这个框里没有确认词输入框');
  inp.value = text;
  await fire(ctx, 'modal-word', 'input');
}

// 开一个框：点下去（不 await），等一个 tick 让同步那段建好 DOM，再把框读出来。
async function open(ctx, start) {
  ctx.__stub.calls.length = 0;
  ctx.__stub.confirms.length = 0;
  const pending = start();
  await tick();
  return { d: dialog(ctx), pending };
}

// ---- 用例骨架 ----

let RC = 0;
function ok(name, cond, detail) {
  if (cond) { console.log('  ✓ ' + name); return; }
  console.log('  ✗ ' + name + (detail ? '：' + detail : ''));
  RC = 1;
}

// 动作 → 档位表（**这就是界面上写操作的档位清单**，改档位会让本表报 ✗）。
//   tier：low = 一次确认；mid = 逐条列影响面 + 主按钮标红 + 只读回显命令。
//   req ：确认后必须发出的那一条请求（取消时则必须一条都没有）。
const ACTIONS = [
  // —— 低危：单对象、可回退 ——
  { id: 'cap-start-btn', title: '开始抓包', tier: 'low', cli: /request vpp trace start interface ens192 count 1000/,
    req: { method: 'POST', url: /\/vpp\/capture$/ },
    pre: (ctx) => { nodeOf(ctx, 'cap-iface').value = 'ens192'; nodeOf(ctx, 'cap-count').value = '1000'; } },
  { id: 'cap-stop-btn', title: '停止抓包（丢弃）', tier: 'low', cli: /request vpp trace stop$/,
    req: { method: 'DELETE', url: /\/vpp\/capture$/ } },
  { id: 'cap-export-btn', title: '停止并导出 pcap', tier: 'low', cli: /request vpp trace export$/,
    req: { method: 'DELETE', url: /\/vpp\/capture\?export=true$/ } },
  { id: 'ops-backup-btn', title: '生成配置备份', tier: 'low', cli: /request system configuration backup$/,
    req: { method: 'POST', url: /\/system\/backup$/ } },
  { id: 'ops-techsupport-btn', title: '生成 tech-support 归档', tier: 'low', cli: /request system tech-support generate$/,
    req: { method: 'POST', url: /\/system\/tech-support$/ } },
  { id: 'ops-coredumps-btn', title: '列出 core dump', tier: 'low', cli: /show system core-dumps$/,
    req: { method: 'GET', url: /\/system\/core-dumps$/ } },
  { id: 'ops-alarms-btn', title: '清除已恢复告警', tier: 'low', cli: /request alarms clear all$/,
    req: { method: 'POST', url: /\/alarms:clear$/ } },
  { id: 'ops-ntp-btn', title: '立即同步时间', tier: 'low', cli: /request system ntp sync$/,
    req: { method: 'POST', url: /\/system\/ntp:sync$/ } },
  { id: 'ops-export-btn', title: '导出 core dump 清单', tier: 'low', cli: /request system core-dumps export /,
    req: { method: 'POST', url: /\/system\/core-dumps:export$/ },
    pre: (ctx) => { nodeOf(ctx, 'ops-export-url').value = 'http://192.0.2.10:8080/collect'; } },
  { id: 'diag-clear-btn', title: '清零接口统计', tier: 'low', cli: /clear interfaces statistics$/,
    req: { method: 'POST', url: /\/interfaces:clear-statistics$/ } },
  { id: 'cfg-discard-btn', title: '丢弃并结束编辑', tier: 'low', cli: /^对应命令（只读回显，便于工单对照）：discard$/,
    req: { method: 'DELETE', url: /\/configuration\/candidate$/ },
    pre: (ctx) => { vm.runInContext('cfg.editing = true', ctx); } },
  // —— 中危：跨对象 / 跨会话，逐条列影响面 ——
  { id: 'ops-sshkey-btn', title: '重新生成 SSH host key', tier: 'mid', cli: /request system ssh host-key regenerate$/,
    req: { method: 'POST', url: /\/system\/ssh-host-key:regenerate$/ } },
  { id: 'ops-vpprestart-btn', title: '重启数据面（VPP）', tier: 'mid', cli: /request vpp restart$/,
    req: { method: 'POST', url: /\/vpp\/restart$/ } },
  { id: 'ops-reboot-btn', title: '重启主机', tier: 'mid', cli: /request system reboot$/,
    req: { method: 'POST', url: /\/system:reboot$/ } },
  { id: 'ops-shutdown-btn', title: '关机', tier: 'mid', cli: /request system shutdown$/,
    req: { method: 'POST', url: /\/system:shutdown$/ } },
  { id: 'cfg-commit-btn', title: '提交配置', tier: 'mid', cli: /^对应命令（只读回显，便于工单对照）：commit$/,
    req: { method: 'POST', url: /\/configuration\/commit$/ } },
  { id: 'cfg-commit-confirmed-btn', title: '以 commit confirmed 提交', tier: 'mid', cli: /commit confirmed 10$/,
    req: { method: 'POST', url: /\/configuration\/commit$/ } },
  // —— 中危：本段新增的危险动作（内核基线 / 驱动接管 / VF 数量 / 删策略与会话）——
  { id: 'knl-apply-btn', title: '写入内核基线', tier: 'mid', cli: /request system kernel apply$/,
    req: { method: 'POST', url: /\/system\/kernel:apply$/ } },
  { id: 'knl-rollback-btn', title: '回退内核基线', tier: 'mid', cli: /request system kernel rollback$/,
    req: { method: 'POST', url: /\/system\/kernel:rollback$/ } },
  { id: 'ifd-dpdk-bind', title: '绑定到 DPDK 驱动', tier: 'mid',
    cli: /request interfaces ens192 bind-dpdk uio-driver vfio-pci$/,
    req: { method: 'PUT', url: /\/interfaces\/ens192\/dpdk\?confirm=true$/ },
    pre: (ctx) => { nodeOf(ctx, 'ifd-name').textContent = 'ens192'; nodeOf(ctx, 'ifd-uio').value = 'vfio-pci'; } },
  { id: 'ifd-dpdk-unbind', title: '解绑交还内核驱动', tier: 'mid',
    cli: /request interfaces ens192 unbind-dpdk to-driver vmxnet3$/,
    req: { method: 'PUT', url: /\/interfaces\/ens192\/dpdk\?confirm=true$/ },
    pre: (ctx) => { nodeOf(ctx, 'ifd-name').textContent = 'ens192'; nodeOf(ctx, 'ifd-todrv').value = 'vmxnet3'; } },
  { id: 'ifd-sriov-btn', title: '设置 SR-IOV VF 数量', tier: 'mid',
    cli: /request sriov create-vfs ens192 count 4$/,
    req: { method: 'PUT', url: /\/interfaces\/ens192\/sriov$/ },
    pre: (ctx) => { nodeOf(ctx, 'ifd-name').textContent = 'ens192'; nodeOf(ctx, 'ifd-vfs').value = '4'; } },
  { id: 'tls-regen-btn', title: '重签自签证书', tier: 'mid', cli: /request system api tls regenerate$/,
    req: { method: 'POST', url: /\/system\/tls:regenerate$/ } },
  { id: 'qsd-del-btn', title: '删除 QoS 策略', tier: 'mid', cli: /delete qos policy name q1$/,
    req: { method: 'DELETE', url: /\/qos\/policies\/q1$/ },
    pre: (ctx) => { nodeOf(ctx, 'qsd-name').textContent = 'q1'; } },
  { id: 'spd-del-btn', title: '删除端口镜像会话', tier: 'mid', cli: /delete port-mirroring name sp1$/,
    req: { method: 'DELETE', url: /\/port-mirroring\/sp1$/ },
    pre: (ctx) => { nodeOf(ctx, 'spd-name').textContent = 'sp1'; } },
];

// 函数驱动的动作（列表行/详情页/镜像页的按钮是渲染出来的，直接调处理器等价于点它）
const FN_ACTIONS = [
  { expr: 'vmAction("vnf-a", "stop", "停止")', title: '停止虚拟机', tier: 'low',
    cli: /request virtual-machine-functions vnf-a stop$/,
    req: { method: 'POST', url: /\/virtual-machine-functions\/vnf-a:stop$/ } },
  { expr: 'ctAction("c1", "restart", "重启")', title: '重启容器', tier: 'low',
    cli: /request container-functions c1 restart$/,
    req: { method: 'POST', url: /\/container-functions\/c1:restart$/ } },
  { expr: 'vmSnapCreate()', title: '创建快照', tier: 'low',
    cli: /request virtual-machine-functions vnf-a snapshot create name s1$/,
    req: { method: 'POST', url: /\/virtual-machine-functions\/vnf-a\/snapshots$/ },
    pre: (ctx) => { vm.runInContext('snapVM = "vnf-a"', ctx); nodeOf(ctx, 'vm-snap-new').value = 's1'; } },
  { expr: 'vmSnapAct("s1", "delete")', title: '删除快照', tier: 'low',
    cli: /request virtual-machine-functions vnf-a snapshot delete name s1$/,
    req: { method: 'DELETE', url: /\/virtual-machine-functions\/vnf-a\/snapshots\/s1$/ },
    pre: (ctx) => { vm.runInContext('snapVM = "vnf-a"', ctx); } },
  { expr: 'vmSnapAct("s1", "rollback")', title: '回滚虚拟机到快照', tier: 'mid',
    cli: /request virtual-machine-functions vnf-a snapshot rollback name s1$/,
    req: { method: 'POST', url: /\/virtual-machine-functions\/vnf-a\/snapshots\/s1:rollback$/ },
    pre: (ctx) => { vm.runInContext('snapVM = "vnf-a"', ctx); } },
  { expr: 'imgDelete("img1", 2)', title: '删除镜像', tier: 'mid',
    cli: /request images delete name img1$/,
    req: { method: 'DELETE', url: /\/images\/img1$/ } },
];

// 高危动作 → 档位表（**分级表的"高危"档**：不可撤销 / 会覆盖全局状态 / 动的是登录凭据）。
// 判据（与低/中危的差别就在这两道闸门）：
//   · 必须出现**确认词输入框**，且确认词**不匹配就不放行**（空、多打一个字符都不行）；
//   · 倒计时**没走完不放行**（差 1 秒也不行），走完且确认词正确才可点「执行」；
//   · 确认之前**一条写请求都不许发**（只读预检不算），取消之后同样不许发。
const HIGH_ACTIONS = [
  { id: 'tls-install-btn', title: '安装外部证书（管理面证书）', word: 'install-cert',
    cli: /set system api tls cert-file/, req: { method: 'PUT', url: /\/system\/tls$/ },
    pre: (ctx) => {
      nodeOf(ctx, 'tls-cert').value = '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----';
      nodeOf(ctx, 'tls-key').value = '-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----';
    } },
  { id: 'ops-sw-add-btn', title: '安装升级包（软件升级）', word: 'upgrade',
    cli: /request system software add \/root\/nfvis_1\.1\.29_amd64\.deb$/,
    req: { method: 'POST', url: /\/system\/software$/ },
    pre: (ctx) => { nodeOf(ctx, 'ops-sw-pkg').value = '/root/nfvis_1.1.29_amd64.deb'; } },
  { id: 'ops-sw-rollback-btn', title: '回退软件版本', word: 'rollback',
    cli: /request system software rollback$/, req: { method: 'POST', url: /\/system\/software:rollback$/ } },
  { id: 'ops-restore-btn', title: '从备份归档恢复配置', word: 'cfg-20260924.json',
    cli: /request system configuration restore/, req: { method: 'POST', url: /\/system\/restore$/ },
    pre: (ctx) => { nodeOf(ctx, 'ops-restore-file').value = 'cfg-20260924.json'; } },
  { id: 'ops-zeroize-btn', title: '恢复出厂（清空配置与镜像）', word: 'zeroize',
    cli: /request system zeroize$/, req: { method: 'POST', url: /\/system:zeroize$/ } },
  { id: 'usr-create-btn', title: '创建本地用户', word: 'netop2',
    cli: /set system login user netop2 password/, req: { method: 'POST', url: /\/system\/login-users$/ },
    pre: (ctx) => {
      vm.runInContext('currentUser = "admin"', ctx);
      nodeOf(ctx, 'usr-new-name').value = 'netop2';
      nodeOf(ctx, 'usr-new-class').value = 'operator';
      nodeOf(ctx, 'usr-new-pw').value = 'S3cret-Passw0rd!';
    } },
  { id: 'usr-mypw-btn', title: '修改我的口令', word: 'admin',
    cli: /request system password change$/,
    req: { method: 'POST', url: /\/system\/login-users\/admin:change-password$/ },
    pre: (ctx) => {
      vm.runInContext('currentUser = "admin"', ctx);
      nodeOf(ctx, 'usr-my-old').value = 's3cret-Passw0rd!';
      nodeOf(ctx, 'usr-my-new').value = 'S3cret-Passw0rd!2';
    } },
];

// 高危的动作类函数（用户页的行内按钮）：直接调处理器，等价于点那一行上的按钮。
const HIGH_FN_ACTIONS = [
  { expr: 'usrApply("netop", "operator", "")', title: '修改用户权限类', word: 'netop',
    cli: /set system login user netop class operator$/,
    req: { method: 'PUT', url: /\/system\/login-users\/netop$/ },
    pre: (ctx) => { vm.runInContext('currentUser = "admin"', ctx); } },
  { expr: 'usrApply("netop", "read-only", "S3cret-Passw0rd!9")', title: '重置用户口令', word: 'netop',
    cli: /set system login user netop password/,
    req: { method: 'PUT', url: /\/system\/login-users\/netop$/ },
    pre: (ctx) => { vm.runInContext('currentUser = "admin"', ctx); } },
  { expr: 'usrDelete("netop")', title: '删除本地用户', word: 'netop',
    cli: /delete system login user netop$/,
    req: { method: 'DELETE', url: /\/system\/login-users\/netop$/ },
    pre: (ctx) => { vm.runInContext('currentUser = "admin"', ctx); } },
];


function reqText(r) { return r.method + ' ' + String(r.url); }

async function runAction(ctx, act, start) {
  const label = act.title + '（' + (act.tier === 'mid' ? '中危' : '低危') + '）';

  // —— 第一遍：只看框，然后取消 ——
  if (act.pre) act.pre(ctx);
  const first = await open(ctx, start);
  const d = first.d;
  ok(label + '：确认框弹出且标题是动作名', d.visible && d.title === act.title,
    'visible=' + d.visible + ' title=' + JSON.stringify(d.title));
  if (act.tier === 'mid') {
    ok(label + '：逐条列出影响面（≥ 2 条）', d.bullets >= 2, '条目数=' + d.bullets);
    ok(label + '：主按钮标红', d.okDanger, 'class=' + JSON.stringify(nodeOf(ctx, 'modal-ok').className));
  } else {
    ok(label + '：不列影响面（单击确认，不该被一屏字挡住）', d.bullets === 0, '条目数=' + d.bullets);
    ok(label + '：有说明文案', d.paragraphs.length >= 1, '段数=' + d.paragraphs.length);
  }
  ok(label + '：没有确认词闸门（只有高危才有）', d.guardHidden && !d.hasWord,
    'guardHidden=' + d.guardHidden + ' hasWord=' + d.hasWord);
  ok(label + '：按钮可直接点（无倒计时）', !d.okDisabled, 'label=' + JSON.stringify(d.okLabel));
  ok(label + '：只读回显对应命令', act.cli.test(d.cli), JSON.stringify(d.cli));
  ok(label + '：确认之前一条请求都没发', ctx.__stub.calls.length === 0,
    '已发：' + JSON.stringify(ctx.__stub.calls.map((c) => c.method + ' ' + c.url)));
  await fire(ctx, 'modal-cancel', 'click');
  await first.pending;
  ok(label + '：取消之后仍然没发请求（取消 = 什么都没做）', ctx.__stub.calls.length === 0,
    '已发：' + JSON.stringify(ctx.__stub.calls.map((c) => c.method + ' ' + c.url)));
  ok(label + '：取消之后框收起、倒计时没有留在后台', dialog(ctx).visible === false &&
    vm.runInContext('dialogTimer', ctx) === null);

  // —— 第二遍：确认，必须真的执行 ——
  if (act.pre) act.pre(ctx);
  const second = await open(ctx, start);
  await fire(ctx, 'modal-ok', 'click');
  await second.pending;
  const hit = ctx.__stub.calls.some((c) => c.method === act.req.method && act.req.url.test(c.url));
  ok(label + '：确认后发出 ' + reqText(act.req), hit,
    '已发：' + JSON.stringify(ctx.__stub.calls.map((c) => c.method + ' ' + c.url)));
  ok(label + '：执行完框已收起', dialog(ctx).visible === false);
}

// runHighAction：高危档的完整一遍——两道闸门**逐条**验（不是"提示了一句"就算数），
// 再验"闸门满足后确实发了那一条请求"。
async function runHighAction(ctx, act, start) {
  const label = act.title + '（高危）';

  // —— 第一遍：闸门全程关着，最后取消 ——
  if (act.pre) act.pre(ctx);
  const first = await open(ctx, start);
  const d = first.d;
  ok(label + '：确认框弹出且标题是动作名', d.visible && d.title === act.title,
    'visible=' + d.visible + ' title=' + JSON.stringify(d.title));
  ok(label + '：逐条列出影响面（≥ 2 条）', d.bullets >= 2, '条目数=' + d.bullets);
  ok(label + '：主按钮标红', d.okDanger, 'class=' + JSON.stringify(nodeOf(ctx, 'modal-ok').className));
  ok(label + '：出现确认词闸门（输入框真的在）', !d.guardHidden && d.hasWord,
    'guardHidden=' + d.guardHidden + ' hasWord=' + d.hasWord);
  ok(label + '：闸门里写明确认词是 ' + JSON.stringify(act.word), d.guardText.indexOf(act.word) >= 0,
    JSON.stringify(d.guardText));
  ok(label + '：初始「执行」不可点', d.okDisabled);
  ok(label + '：默认倒计时 10 秒（提示里写明）', /10 秒/.test(d.count), JSON.stringify(d.count));
  ok(label + '：按钮上标注还要等几秒', /10 秒后可点/.test(d.okLabel), JSON.stringify(d.okLabel));
  ok(label + '：只读回显对应命令', act.cli.test(d.cli), JSON.stringify(d.cli));
  ok(label + '：确认之前一条写请求都没发', writes(ctx).length === 0,
    '已发写请求：' + JSON.stringify(writes(ctx).map((c) => c.method + ' ' + c.url)));
  await typeWord(ctx, '');
  ok(label + '：确认词为空：仍不可点', dialog(ctx).okDisabled);
  await typeWord(ctx, act.word + 'x');
  ok(label + '：确认词不匹配（多打一个字符）：仍不可点', dialog(ctx).okDisabled);
  await typeWord(ctx, act.word);
  ok(label + '：确认词正确但倒计时没走完：仍不可点', dialog(ctx).okDisabled,
    'label=' + JSON.stringify(dialog(ctx).okLabel));
  ctx.__stub.advance(9);
  ok(label + '：倒计时差 1 秒（9/10）：仍不可点', dialog(ctx).okDisabled,
    'label=' + JSON.stringify(dialog(ctx).okLabel));
  ctx.__stub.advance(1);
  ok(label + '：倒计时走完（10 秒）：按钮才可点', !dialog(ctx).okDisabled,
    'label=' + JSON.stringify(dialog(ctx).okLabel));
  await fire(ctx, 'modal-cancel', 'click');
  await first.pending;
  ok(label + '：取消之后仍然没有写请求（取消 = 什么都没做）', writes(ctx).length === 0,
    '已发写请求：' + JSON.stringify(writes(ctx).map((c) => c.method + ' ' + c.url)));
  ok(label + '：取消之后框收起、倒计时没有留在后台', dialog(ctx).visible === false &&
    vm.runInContext('dialogTimer', ctx) === null && ctx.__stub.clockLive() === 0);

  // —— 第二遍：两道闸门都满足，必须真的发出那一条请求 ——
  if (act.pre) act.pre(ctx);
  const second = await open(ctx, start);
  await typeWord(ctx, act.word);
  ctx.__stub.advance(10);
  await fire(ctx, 'modal-ok', 'click');
  await second.pending;
  const hit = ctx.__stub.calls.some((c) => c.method === act.req.method && act.req.url.test(c.url));
  ok(label + '：确认后发出 ' + reqText(act.req), hit,
    '已发：' + JSON.stringify(ctx.__stub.calls.map((c) => c.method + ' ' + c.url)));
  ok(label + '：执行完框已收起', dialog(ctx).visible === false);
}

(async () => {
  const ctx = makeContext();

  console.log('— ⓪ 骨架与脚本对得上吗（桩的 getElementById 会按需造节点，比浏览器宽松） —');
  // 组件直接摸的那几个 id 必须真的在 index.html 的骨架里：浏览器里取不到就是 null，
  // 第一行 `null.hidden = …` 就崩——桩会替它造一个，正好把这处漏检。
  const html = fs.readFileSync(INDEX_HTML, 'utf8');
  for (const id of ['modal', 'modal-title', 'modal-body', 'modal-cli', 'modal-guard', 'modal-actions']) {
    ok('骨架 index.html 里有 #' + id, new RegExp('id="' + id + '"').test(html));
  }
  // 闸门区与按钮由脚本现建（每次开框重建，不带上次的输入）：骨架里**不该**再有同名 id。
  for (const id of ['modal-word', 'modal-count', 'modal-ok', 'modal-cancel']) {
    ok('#' + id + ' 由脚本现建（骨架里没有，避免重复 id）', !new RegExp('id="' + id + '"').test(html));
  }

  console.log('— ① 逐个动作走一遍：档位特征 + 确认前/取消后都不许发请求 —');
  for (const act of ACTIONS) {
    await runAction(ctx, act, () => fire(ctx, act.id, 'click'));
  }
  for (const act of FN_ACTIONS) {
    await runAction(ctx, act, () => vm.runInContext('(' + act.expr + ')', ctx));
  }

  console.log('— ①b 高危动作逐个走一遍：确认词 + 倒计时两道闸门 + 确认后才发请求 —');
  for (const act of HIGH_ACTIONS) {
    await runHighAction(ctx, act, () => fire(ctx, act.id, 'click'));
  }
  for (const act of HIGH_FN_ACTIONS) {
    await runHighAction(ctx, act, () => vm.runInContext('(' + act.expr + ')', ctx));
  }

  console.log('— ② 高危档：确认词 + 倒计时真的拦得住（默认 10 秒；用假时钟推进，不真等） —');
  const hi = await open(ctx, () => vm.runInContext(
    'uiConfirm("恢复出厂", { tier: "high", requireWord: "zeroize", cli: "request system zeroize",' +
    ' bullets: ["配置库与数据会被清空，不可逆", "重启后按初始状态引导，需要带外或控制台"] })', ctx));
  ok('高危：闸门区出现（确认词输入框）', !hi.d.guardHidden && hi.d.hasWord);
  ok('高危：影响面逐条列 + 主按钮标红', hi.d.bullets === 2 && hi.d.okDanger,
    '条目数=' + hi.d.bullets + ' danger=' + hi.d.okDanger);
  ok('高危：初始「执行」不可点', hi.d.okDisabled);
  ok('高危：默认倒计时是 10 秒（提示里写明）', /10 秒/.test(hi.d.count), JSON.stringify(hi.d.count));
  ok('高危：按钮上标注还要等几秒', /10 秒后可点/.test(hi.d.okLabel), JSON.stringify(hi.d.okLabel));
  ok('高危：只读回显对应命令', /request system zeroize$/.test(hi.d.cli), JSON.stringify(hi.d.cli));
  await typeWord(ctx, 'zeroiz');
  ok('确认词不匹配（少一个字母）：「执行」仍不可点', dialog(ctx).okDisabled);
  await typeWord(ctx, 'ZEROIZE');
  ok('确认词大小写不匹配：「执行」仍不可点', dialog(ctx).okDisabled);
  await typeWord(ctx, 'zeroize');
  ok('确认词正确但倒计时没走完：「执行」仍不可点', dialog(ctx).okDisabled,
    'label=' + JSON.stringify(dialog(ctx).okLabel));
  await fire(ctx, 'modal-cancel', 'click');
  await hi.pending;
  ok('倒计时期间取消：框收起、定时器被清掉（不会在后台继续跑）',
    dialog(ctx).visible === false && vm.runInContext('dialogTimer', ctx) === null && ctx.__stub.clockLive() === 0);

  console.log('— ③ 高危档：倒计时走完 + 确认词正确，才放行（假时钟推进，不真等） —');
  const short = await open(ctx, () => vm.runInContext(
    'uiConfirm("恢复出厂", { tier: "high", requireWord: "zeroize",' +
    ' cli: "request system zeroize" })', ctx));
  await typeWord(ctx, 'zeroize');
  ok('倒计时进行中：确认词已正确但按钮仍不可点', dialog(ctx).okDisabled,
    'label=' + JSON.stringify(dialog(ctx).okLabel));
  ctx.__stub.advance(5);
  ok('倒计时走到一半（5 秒）：仍不可点', dialog(ctx).okDisabled, 'label=' + JSON.stringify(dialog(ctx).okLabel));
  ctx.__stub.advance(4);
  ok('差 1 秒（9 秒）：仍不可点', dialog(ctx).okDisabled, 'label=' + JSON.stringify(dialog(ctx).okLabel));
  ctx.__stub.advance(1);
  const after = dialog(ctx);
  ok('倒计时走完（10 秒）：按钮可点', !after.okDisabled, 'label=' + JSON.stringify(after.okLabel));
  ok('倒计时走完：提示如实说明（确认词仍需匹配）', /倒计时已结束/.test(after.count), JSON.stringify(after.count));
  // 回车等价于点「执行」（键盘路径也要能走通）
  await fire(ctx, 'modal-word', 'keydown', { key: 'Enter' });
  await short.pending;
  ok('倒计时走完 + 回车：框按「确认」收起', dialog(ctx).visible === false);

  console.log('— ④ 高危档：倒计时走完后，确认词仍是最后一道闸门（单独验它，不与倒计时混在一起） —');
  const wordGate = await open(ctx, () => vm.runInContext(
    'uiConfirm("恢复出厂", { tier: "high", countdown: 0, requireWord: "zeroize",' +
    ' cli: "request system zeroize" })', ctx));
  ok('确认词没输：按钮不可点', wordGate.d.okDisabled);
  await typeWord(ctx, 'zer');
  ok('确认词不对（只打了前三个字母）：仍不可点', dialog(ctx).okDisabled);
  await typeWord(ctx, '');
  ok('确认词清空：仍不可点', dialog(ctx).okDisabled);
  await typeWord(ctx, 'zeroize');
  ok('确认词正确：按钮才可点', !dialog(ctx).okDisabled, 'label=' + JSON.stringify(dialog(ctx).okLabel));
  await fire(ctx, 'modal-ok', 'click');
  await wordGate.pending;
  ok('点「执行」后框按确认收起', dialog(ctx).visible === false);

  console.log('— ⑤ 高危档：调用方没给确认词时**失败关闭**（宁可点不动，也不放过闸门） —');
  const noWord = await open(ctx, () => vm.runInContext(
    'uiConfirm("恢复出厂", { tier: "high", cli: "request system zeroize" })', ctx));
  ok('没给确认词：输入框直接禁用', noWord.d.hasWord && noWord.d.wordDisabled);
  ok('没给确认词：框里写明无法执行（不许静默放行）', /无法执行/.test(noWord.d.guardText),
    JSON.stringify(noWord.d.guardText));
  ctx.__stub.advance(10);
  ok('没给确认词：倒计时走完也不放行', dialog(ctx).okDisabled);
  await fire(ctx, 'modal-cancel', 'click');
  await noWord.pending;

  console.log('— ⑥ 用户与权限：本会话有未提交候选时，写操作**先被拒绝**（不发写请求、不弹确认框） —');
  // 为什么必须拦：服务端的用户写路径是"取配置编辑锁 → 写候选 → 立即提交"，而同一持有者的候选
  // 就是同一份——不拦就会把操作者在「配置」页改了半天的候选一并提交上去。
  vm.runInContext('currentUser = "admin"', ctx);
  ctx.__stub.setDirty(true);
  ctx.__stub.calls.length = 0;
  await vm.runInContext('(usrDelete("netop"))', ctx);
  ok('有未提交候选时：不弹确认框（先说明原因，而不是让人白确认一次）', dialog(ctx).visible === false);
  ok('有未提交候选时：一条写请求都没发', writes(ctx).length === 0,
    '已发写请求：' + JSON.stringify(writes(ctx).map((c) => c.method + ' ' + c.url)));
  ok('有未提交候选时：提示写明要先提交或丢弃',
    /未提交的候选配置/.test(nodeOf(ctx, 'usr-msg').textContent), JSON.stringify(nodeOf(ctx, 'usr-msg').textContent));
  // 关掉"脏候选"：同一个动作应当照常走确认框（证明拦截来自那个前置检查，而不是动作本身坏了）
  ctx.__stub.setDirty(false);
  const okCase = await open(ctx, () => vm.runInContext('(usrDelete("netop"))', ctx));
  ok('候选干净时：同一个动作照常弹确认框（拦截来自前置检查，不是动作坏了）',
    okCase.d.visible && okCase.d.title === '删除本地用户', 'visible=' + okCase.d.visible);
  await fire(ctx, 'modal-cancel', 'click');
  await okCase.pending;

  console.log('— ⑦ 不许退回浏览器自带的 confirm（没有影响面、没有只读回显、没有闸门） —');  ok('整轮下来 window.confirm 一次都没被调用', ctx.__stub.confirms.length === 0,
    '被调用 ' + ctx.__stub.confirms.length + ' 次：' + JSON.stringify(ctx.__stub.confirms.slice(0, 3)));
  ok('app.js 里已经没有 window.confirm(', !/window\.confirm\s*\(/.test(APP_SRC_RAW));
  ok('高危档的倒计时不许被调用点缩短（app.js 里不该出现 countdown 覆盖）',
    !/\bcountdown:\s*[0-9]/.test(APP_SRC_RAW));

  // 直连 fetch（下载/上传/日志这类不走 api() 的路径）失败时，**服务端的原话要到达操作者**：
  // 服务端会说明原因（如「当前 class 无权执行该操作」），界面只剩一个 HTTP 403 就是把原因丢了。
  // 判据是结构性的：裸状态码只允许出现在两个取文案的助手函数里，其余调用点都必须经它们。
  console.log('— ⑧ 直连 fetch 的失败文案：服务端原话优先（不许只剩 HTTP 状态码） —');
  const viaHelper = (APP_SRC_RAW.match(/await httpErrText\(res\)/g) || []).length;
  ok('下载/归档读取/容器日志/镜像上传都经 httpErrText（4 处调用）', viaHelper === 4, '出现 ' + viaHelper + ' 处');
  const viaTextHelper = (APP_SRC_RAW.match(/errTextOf\(text, res\.status\)/g) || []).length;
  ok('文本类端点（差异/日志）经 errTextOf（2 处调用）', viaTextHelper === 2, '出现 ' + viaTextHelper + ' 处');
  // 回归判据：被修掉的那几处**裸状态码文案**不许再出现（各自写明端点的读失败）。
  for (const bad of ['读取失败：HTTP ', '读取差异失败：HTTP ', '读取归档失败（HTTP ']) {
    ok('不再出现「' + bad + '」这种丢了服务端原因的文案', APP_SRC_RAW.indexOf(bad) < 0);
  }

  if (RC === 0) console.log('全部符合预期');
  else console.log('有不符合预期的用例');
  process.exit(RC);
})();
