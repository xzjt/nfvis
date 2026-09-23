// 控制台「配置页 → 登录与口令策略 → 口令控件」这条路径的桩式自校准（Node 里的最小 DOM 桩）。
//
// 由来（2026-09-24，round61 证据 §4/§5 的判定）：明文 HTTP 打开控制台时，口令控件填明文 →
// 点「保存到 candidate」→ 界面报「已保存到 candidate（有未提交变更）」，而独立事实源
// GET /configuration/candidate 里该用户的 password_hash 是空的——**口令改动被静默丢掉、
// 界面却报成功**（假绿）。代码里本来就有「没有 Web Crypto 就拒绝派生」的防线，但那个异常
// 只让**这一个字段**没写进去，保存照发、照报成功。
//
// 教训（同一份证据 §5 第 3 条）：上一轮的 DOM 桩跑在 Node 里，Node 有 crypto.subtle，
// 等价于"安全上下文"——**验证环境比目标环境更强**，于是这条路径根本没被走到。
// 故本桩把两种能力环境都跑一遍：**显式屏蔽 crypto.subtle**（等价明文 HTTP）与**提供它**
// （等价 HTTPS / localhost），断言各自该有的结果。
//
// 手法：把真正的 internal/api/ui/app.js 整份跑在 vm 里（不另写一份实现，否则测的不是实现），
// 配一个最小 DOM 桩驱动真实表单路径：cfgRenderForms → 口令控件填明文 → input 事件 → cfgSave。
// 「candidate 有没有被写」的判据是**服务端调用**：桩 fetch 记录每一次请求，没有 PUT
// /configuration/candidate 就是没写；写了的则解析请求体，看里面有没有 pbkdf2$ 哈希。
//
// 用法：node contrib/scripts/web-console-config-selftest.js
// 红-绿验证本自校准自身：WEB_CONSOLE_APP_JS=<拆掉保存前闸门的副本> node … 必须报 ✗
//   —— 由 contrib/scripts/web-console-config-selftest.sh 自动跑这一条。
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto, pbkdf2Sync } = require('node:crypto');

const APP_JS = process.env.WEB_CONSOLE_APP_JS ||
  path.join(__dirname, '..', '..', 'internal', 'api', 'ui', 'app.js');

// app.js 是 ES 模块（index.html 用 `type="module"` 加载），只有 `export` 没有静态 `import`
// （唯一的 import 是 routerModule() 里的动态 `import('./router.js')`，本桩的路径用不到它）。
// vm 里按**经典脚本**跑，故剥掉行首的 `export ` 前缀——纯语法层面，不改任何实现；
// 哪天真加了静态 import，下面这条判据会先报出来（而不是让本桩悄悄测别的东西）。
const APP_SRC_RAW = fs.readFileSync(APP_JS, 'utf8');
if (/^\s*import\s/m.test(APP_SRC_RAW)) {
  throw new Error(APP_JS + ' 出现了静态 import——本桩按经典脚本跑，需改用模块方式加载');
}
const APP_SRC = APP_SRC_RAW.replace(/^export /gm, '');
if ((APP_SRC_RAW.match(/^export /gm) || []).length < 3) {
  throw new Error(APP_JS + ' 的顶层 export 变少了？本桩的剥法要跟着改');
}

// ---- 最小 DOM 桩：只实现 app.js 真的用到的那几个成员 ----
function makeDom() {
  const byId = new Map();
  const created = [];

  function mk(tag) {
    const node = {
      tagName: String(tag).toUpperCase(),
      attrs: {}, listeners: {}, children: [],
      value: '', textContent: '', className: '', hidden: false, disabled: false,
      appendChild(c) { this.children.push(c); return c; },
      insertBefore(c) { return this.appendChild(c); },
      removeChild(c) {
        const i = this.children.indexOf(c);
        if (i >= 0) this.children.splice(i, 1);
        return c;
      },
      setAttribute(k, v) {
        this.attrs[k] = String(v);
        if (k === 'id') byId.set(String(v), this);
      },
      getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
      hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k); },
      removeAttribute(k) { delete this.attrs[k]; },
      addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
      removeEventListener() {},
      focus() {}, blur() {}, click() {},
      querySelector() { return null; },
      querySelectorAll() { return []; },
    };
    created.push(node);
    return node;
  }

  const doc = {
    createElement: mk,
    // 未知 id 按需造一个：app.js 在脚本求值期就会给一堆控件挂监听
    getElementById: (id) => {
      const key = String(id);
      if (!byId.has(key)) mk('div').setAttribute('id', key);
      return byId.get(key);
    },
    addEventListener() {}, removeEventListener() {},
    querySelector() { return null; }, querySelectorAll() { return []; },
  };
  doc.body = mk('body');
  return { doc, created };
}

// 桩 fetch：记录每一次服务端调用（判「candidate 到底有没有被写」的唯一事实源）
function makeFetch(calls) {
  return async (url, opts) => {
    const o = opts || {};
    const method = (o.method || 'GET').toUpperCase();
    calls.push({ url: String(url), method, body: o.body === undefined ? null : String(o.body) });
    return {
      ok: true,
      status: 200,
      json: async () => ({ candidate: o.body ? JSON.parse(o.body) : {}, dirty: true }),
      text: async () => '',
    };
  };
}

// 两种能力环境：① 明文 HTTP —— crypto 在、subtle 不在（真实浏览器的形态）；
//                 ② 安全上下文（HTTPS / localhost）—— Web Crypto 齐备。
const CRYPTO_INSECURE = {
  getRandomValues(a) { for (let i = 0; i < a.length; i++) a[i] = (i * 7 + 1) & 0xff; return a; },
};
const CRYPTO_SECURE = {
  getRandomValues: (a) => webcrypto.getRandomValues(a),
  subtle: webcrypto.subtle,
};

// 被编辑的配置底稿：一个已有本地用户（口令控件就在它的行里）。
const COMMITTED = {
  system: {
    login: {
      password_policy: { min_length: 8, complexity: true },
      users: [{ name: 'admin', class: 'super-user' }],
    },
  },
};

function makeContext(cryptoObj) {
  const { doc, created } = makeDom();
  const calls = [];
  const win = { addEventListener() {}, removeEventListener() {}, confirm: () => true };
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
    crypto: cryptoObj,
    fetch: makeFetch(calls),
    btoa: (s) => Buffer.from(String(s), 'binary').toString('base64'),
    atob: (s) => Buffer.from(String(s), 'base64').toString('binary'),
    console, setTimeout, clearTimeout, setInterval, clearInterval,
    TextEncoder, TextDecoder,
    URL: { createObjectURL: () => 'blob:stub', revokeObjectURL() {} },
    WebSocket: function WebSocket() { this.close = () => {}; },
    EventSource: function EventSource() { this.close = () => {}; },
    __stub: {
      committed: COMMITTED,
      plaintext: '',
      typing: false,
      blur: false,
      calls,
      // 触发表单事件：app.js 的监听器是 async，把返回的 promise 一起等掉
      async fire(node, type) {
        const hs = node.listeners[type] || [];
        await Promise.all(hs.map((h) => h({ type, target: node, preventDefault() {}, stopPropagation() {} })));
      },
      // 表单里的口令控件（渲染时 type=password）
      findByType(t) { return created.find((n) => n.attrs.type === t) || null; },
    },
  };
  sandbox.globalThis = sandbox;
  const ctx = vm.createContext(sandbox);
  vm.runInContext(APP_SRC, ctx, { filename: 'internal/api/ui/app.js' });
  return ctx;
}

// 这段代码在**桩上下文里**执行（vm.runInContext 同一个 context）：cfg / cfgSave / cfgRenderForms
// 都是 app.js 的顶层声明，document / __stub 是桩。步骤与操作者在页面上做的一致：
// 进入编辑（candidate = 当前配置）→ 表单里填明文 → 触发 input → 点「保存到 candidate」。
function driveFlow() {
  return (async () => {
    cfg.committed = { configuration: JSON.parse(JSON.stringify(__stub.committed)), revision: 7 };
    cfgWriteText(cfg.committed.configuration);
    cfgRenderForms(cfg.committed.configuration);
    const pw = __stub.findByType('password');
    if (!pw) return { error: '表单里找不到口令控件（type=password）' };
    if (__stub.typing) {
      // 逐键输入：每次按键 = 在**控件当前内容**后面追加一个字符（键盘就是这么工作的）
      for (const ch of __stub.plaintext) {
        pw.value += ch;
        await __stub.fire(pw, 'input');
      }
    } else {
      pw.value = __stub.plaintext;
      await __stub.fire(pw, 'input');
    }
    // 点「保存到 candidate」会先让控件失焦 → change 事件（真实浏览器里的顺序）
    if (__stub.blur) await __stub.fire(pw, 'change');
    const saved = await cfgSave();
    const puts = __stub.calls.filter((c) => c.method === 'PUT' && /\/configuration\/candidate$/.test(c.url));
    let users = null;
    if (puts.length) users = JSON.parse(puts[puts.length - 1].body).system.login.users;
    return {
      saved,
      msg: document.getElementById('cfg-msg').textContent,
      control: pw.value,
      putCount: puts.length,
      users,
    };
  })();
}

// 用例：能力环境 + 明文 + 交互方式 → 跑一遍真实路径
async function runCase(cryptoObj, plaintext, opts) {
  const o = opts || {};
  const ctx = makeContext(cryptoObj);
  ctx.__stub.plaintext = plaintext;
  ctx.__stub.typing = !!o.typing;
  ctx.__stub.blur = !!o.blur;
  const out = await vm.runInContext('(' + driveFlow.toString() + ')()', ctx, { filename: 'drive.js' });
  out.hash = out.users && out.users[0] ? out.users[0].password_hash : undefined;
  out.hashIsPlaintext = out.hash === plaintext;
  return out;
}

// 独立复算：哈希必须真是「这段明文 + 这个盐 + 这个迭代数」的 PBKDF2-SHA256（与命令行同格式）
function verifyHash(hash, plaintext) {
  const m = /^pbkdf2\$sha256\$(\d+)\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$/.exec(hash || '');
  if (!m) return '不是 pbkdf2$sha256$<迭代数>$<盐>$<哈希> 形态';
  const iter = Number(m[1]);
  const salt = Buffer.from(m[2], 'base64');
  const want = Buffer.from(m[3], 'base64');
  const got = pbkdf2Sync(plaintext, salt, iter, want.length, 'sha256');
  if (!got.equals(want)) return '用同一套参数复算不出这个哈希';
  if (iter !== 600000) return '迭代数是 ' + iter + '（期望 600000）';
  return '';
}

let RC = 0;
function ok(name, cond, detail) {
  if (cond) { console.log('  ✓ ' + name); return; }
  console.log('  ✗ ' + name + (detail ? '：' + detail : ''));
  RC = 1;
}
// 消息区内容照原样打出来：这几行就是**操作者会读到的话**，是本次修复的关键证据
// （以前它写的是「已保存到 candidate（有未提交变更）」，而候选里什么都没改）。
function say(msg) { console.log('  · 消息区：' + msg); }

(async () => {
  const PW = 'Verdict123!x';

  console.log('— ① 明文 HTTP（屏蔽 crypto.subtle）：保存必须被拒，candidate 一个字都不许改 —');
  const a = await runCase(CRYPTO_INSECURE, PW);
  if (a.error) { ok(a.error, false); }
  ok('保存被拒（cfgSave 返回 false，界面不许报成功）', a.saved === false, 'saved=' + JSON.stringify(a.saved));
  ok('消息含 HTTPS 提示（告诉操作者怎么才能成功）', /HTTPS/.test(a.msg), 'msg=' + JSON.stringify(a.msg));
  ok('没有向 candidate 发过任何写请求', a.putCount === 0, 'PUT 次数=' + a.putCount);
  ok('口令控件保留明文（切到 HTTPS 后可重试）', a.control === PW, 'control=' + JSON.stringify(a.control));
  ok('candidate 里没有明文口令', a.hashIsPlaintext !== true && a.hash === undefined,
    'password_hash=' + JSON.stringify(a.hash));
  say(a.msg);

  console.log('— ② 安全上下文（提供 crypto.subtle）：派生成功，写入哈希并清空控件 —');
  const b = await runCase(CRYPTO_SECURE, PW);
  if (b.error) { ok(b.error, false); }
  ok('保存成功（cfgSave 返回 true）', b.saved === true, 'saved=' + JSON.stringify(b.saved));
  ok('candidate 里出现 pbkdf2$ 前缀的哈希', typeof b.hash === 'string' && b.hash.startsWith('pbkdf2$'),
    'password_hash=' + JSON.stringify(b.hash));
  ok('candidate 里不是明文', b.hashIsPlaintext !== true);
  ok('哈希可用同参数 PBKDF2-SHA256/600000 复算出来', verifyHash(b.hash, PW) === '',
    verifyHash(b.hash, PW));
  ok('控件已被清空（明文不留在页面上）', b.control === '', 'control=' + JSON.stringify(b.control));
  say(b.msg);

  console.log('— ③ 安全上下文 + 口令不合策略：同样要显式失败（闸门不只管"没有 Web Crypto"这一条）—');
  const c = await runCase(CRYPTO_SECURE, 'abc');
  if (c.error) { ok(c.error, false); }
  ok('保存被拒', c.saved === false, 'saved=' + JSON.stringify(c.saved));
  ok('没有向 candidate 发过任何写请求', c.putCount === 0, 'PUT 次数=' + c.putCount);
  ok('口令控件保留输入', c.control === 'abc', 'control=' + JSON.stringify(c.control));
  ok('原因说的是策略而不是 HTTPS（别把人往错的方向指）', /策略/.test(c.msg) && !/HTTPS/.test(c.msg),
    'msg=' + JSON.stringify(c.msg));
  say(c.msg);

  console.log('— ④ 安全上下文 + 逐键输入后失焦（真人打字 → 点保存）：只写整段口令的哈希 —');
  const d = await runCase(CRYPTO_SECURE, PW, { typing: true, blur: true });
  if (d.error) { ok(d.error, false); }
  ok('保存成功', d.saved === true, 'saved=' + JSON.stringify(d.saved));
  ok('candidate 里是**整段**明文的哈希（不是被打断的某个前缀）', verifyHash(d.hash, PW) === '',
    verifyHash(d.hash, PW));
  ok('控件已被清空（离开控件即清，明文不留在页面上）', d.control === '',
    'control=' + JSON.stringify(d.control));
  say(d.msg);

  if (RC === 0) console.log('全部符合预期');
  else console.log('有不符合预期的用例');
  process.exit(RC);
})();
