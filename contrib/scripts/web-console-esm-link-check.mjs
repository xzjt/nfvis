//go:build ignore

// Web 控制台前端 ES module 图的**结构守护**（Web 控制台重构 2026-10 配套工具，两层检查）。
//
// 由来一（LINK 层）：app.js 按域拆分后（ui/js/core/** + ui/js/views/** + ui/js/boot.js + 聚合入口
// app.js），各模块是独立作用域的 ES modules——「import 了目标模块没有导出的名字」在浏览器里是
// 加载期错误（整条 import 链一起死），而 vm 桩自校准的「递归拼接成经典脚本」加载器把所有模块混进
// 同一作用域，这类错误在桩里**永远看不到**（黑盒首轮抓到 expandEndpoint/setStream 等一批幻影 import）。
// 本脚本用 SourceTextModule.link() 对整条 import 图做链接期检查。
//
// 由来二（XREF 层）：link() **看不见**「A 模块顶层声明了 VM_ACTIONS、B 模块用了却没 import」
// ——那是运行期 ReferenceError（v3.0.19 实测教训：容器/VM 存在时 containers/containerDetail/
// vmDetail 三页整页失败，零对象现场走查抓不到；同批还有 stripANSI/bytes/opsMsg 等）。故加一层
// 保守的静态核对（行首顶层声明 × 非 import 使用；对象键/方法简写/形参与局部声明做规避；
// 兜不住的逐条列 XREF_ALLOW 白名单并写明理由）。
//
// 用法：node --experimental-vm-modules contrib/scripts/web-console-esm-link-check.mjs
// 退出码：0 = LINL 与 XREF 双通过；1 = 有缺口（逐条打印）。
// 不进 make toolcheck（无 node 时由 shell 层如实跳过——与两桩自校准同口径）。
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'internal', 'api', 'ui');

// 入口：app.js（聚合）+ router.js + vncview.js 会沿 import 递归到全部 js/core/**、js/views/**。
const ENTRY = ['app.js', 'router.js', 'vncview.js'].map((f) => path.join(ROOT, f));

const mods = new Map();
function moduleKey(p) {
  return p.split(path.sep).join('/');
}
function getModule(p) {
  const key = moduleKey(p);
  if (mods.has(key)) return mods.get(key);
  const m = new vm.SourceTextModule(fs.readFileSync(p, 'utf8'), { identifier: key });
  mods.set(key, m);
  return m;
}

async function resolve(specifier, ref) {
  const refDir = ref.identifier.slice(0, ref.identifier.lastIndexOf('/'));
  const target = path.resolve(refDir, specifier);
  try {
    return getModule(target);
  } catch (e) {
    throw new Error('找不到 ' + specifier + '（被 ' + ref.identifier + ' 引用）: ' + e.message);
  }
}

// ---------- XREF：跨模块自由标识符核对 ----------

// 已知良性例外（位置启发式兜不住的名字）→ 理由。清单只增不改，新增条目必须写明理由。
const XREF_ALLOW = new Map([
  ['navigate', 'window.nfvis 对象字面量的键（boot.js）；router.js 顶层另有同名 export'],
  ['render', 'VIEWS 注册表的方法简写名（js/views/*）；router.js 顶层另有同名 export'],
  ['start', '动作映射表的对象键（vms/containers 的 impact 表）；router.js 顶层另有同名 export'],
]);

// stripCodeJS：注释按原长度抹成空格（换行保留），引号/模板串内容同样抹掉。
function stripCodeJS(src) {
  const out = [];
  let i = 0, mode = 0; // 0=code 1=line 2=block 3=' 4=" 5=`
  const NL = String.fromCharCode(10), BS = String.fromCharCode(92);
  while (i < src.length) {
    const c = src[i], n2 = src[i + 1] || '';
    if (mode === 0) {
      if (c === '/' && n2 === '/') { mode = 1; i += 2; continue; }
      if (c === '/' && n2 === '*') { mode = 2; i += 2; continue; }
      if (c === "'") { mode = 3; i += 1; continue; }
      if (c === '"') { mode = 4; i += 1; continue; }
      if (c === '`') { mode = 5; i += 1; continue; }
      out.push(c); i += 1; continue;
    }
    if (mode === 1) { out.push(c === NL ? NL : ' '); mode = c === NL ? 0 : 1; i += 1; continue; }
    if (mode === 2) {
      if (c === '*' && n2 === '/') { mode = 0; i += 2; }
      else { out.push(c === NL ? NL : ' '); i += 1; }
      continue;
    }
    if (c === BS) { out.push(' ', ' '); i += 2; continue; }
    const q = mode === 3 ? "'" : mode === 4 ? '"' : '`';
    if (c === q) mode = 0;
    out.push(c === NL ? NL : ' ');
    i += 1;
  }
  return out.join('');
}

const JS_GLOBALS = new Set(('document,window,sessionStorage,localStorage,location,fetch,console,setTimeout,' +
  'clearTimeout,setInterval,clearInterval,requestAnimationFrame,Promise,JSON,Object,Array,String,Number,Boolean,' +
  'Date,Math,RegExp,Error,TypeError,RangeError,FormData,TextEncoder,TextDecoder,AbortController,URL,URLSearchParams,' +
  'WebSocket,EventSource,btoa,atob,isNaN,isFinite,parseInt,parseFloat,Set,Map,WeakMap,Symbol,undefined,NaN,' +
  'Infinity,globalThis,Reflect,Proxy,Blob,File,FileReader,CustomEvent,Event,MessageChannel,structuredClone,' +
  'arguments,this,null,true,false').split(','));

function xrefCheck() {
  // 只核对**自有**模块：novnc/ 是第三方库（短名遍布、不为我们的拆分管），从图里剔除。
  const files = [...mods.keys()].filter((f) => !f.includes('/novnc/'));
  const info = new Map();
  const topAll = new Map();
  for (const f of files) {
    const raw = fs.readFileSync(f, 'utf8');
    const code = stripCodeJS(raw);
    const imports = new Set();
    for (const m of raw.matchAll(/import\s*\{([^}]+)\}\s*from/g)) {
      for (const x of m[1].split(',')) { const t = x.trim().split(/\s+as\s+/)[0]; if (t) imports.add(t); }
    }
    for (const m of raw.matchAll(/import\s+(\w+)\s+from/g)) imports.add(m[1]);
    // re-export 行（`export { A, B } from '…'`）里的名字：属导出而非使用，并入 imports 面。
    for (const m of raw.matchAll(/^export\s*\{([^}]+)\}\s*from/gm)) {
      for (const x of m[1].split(',')) { const t = x.trim().split(/\s+as\s+/)[0]; if (t) imports.add(t); }
    }
    const top = new Set();
    for (const m of code.matchAll(/^(?:export\s+)?(?:async\s+)?function\s+(\w+)/gm)) top.add(m[1]);
    for (const m of code.matchAll(/^(?:export\s+)?(?:const|let|var)\s+(\w+)/gm)) top.add(m[1]);
    for (const m of code.matchAll(/^(?:export\s+)?class\s+(\w+)/gm)) top.add(m[1]);
    // 任意位置的本地声明（含函数内部）与形参（函数/方法简写/箭头/for-of/数组与对象解构）
    const local = new Set();
    for (const m of code.matchAll(/\b(?:function|class|const|let|var)\s+(\w+)/g)) local.add(m[1]);
    for (const m of code.matchAll(/function\s*\w*\s*\(([^)]*)\)/g)) {
      for (const p of m[1].split(',')) {
        const t = p.trim().split('=')[0].split(':')[0].replace(/^\.+/, '').trim();
        if (/^\w+$/.test(t)) local.add(t);
      }
    }
    // 方法简写形参（`open(a, b, c) {` / `render(d) {`）：行首或 `{`/`,` 后的 `name(` 且后随 `{`
    for (const m of code.matchAll(/(?:^|[{,])\s*\w+\s*\(([^)]*)\)\s*\{/gm)) {
      for (const p of m[1].split(',')) {
        const t = p.trim().split('=')[0].split(':')[0].replace(/^\.+/, '').trim();
        if (/^\w+$/.test(t)) local.add(t);
      }
    }
    for (const m of code.matchAll(/\(([^)]*)\)\s*=>/g)) {
      for (const p of m[1].split(',')) {
        const t = p.trim().split('=')[0].split(':')[0].replace(/^\.+/, '').trim();
        if (/^\w+$/.test(t)) local.add(t);
      }
    }
    for (const m of code.matchAll(/\b(\w+)\s*=>/g)) local.add(m[1]);
    for (const m of code.matchAll(/for\s*\(\s*(?:const|let|var)\s+(\w+)/g)) local.add(m[1]);
    // 数组解构：const [a, b, c] =
    for (const m of code.matchAll(/(?:const|let|var)\s*\[([^\]]+)\]/g)) {
      for (const p of m[1].split(',')) {
        const t = p.trim().split('=')[0].replace(/^\.+/, '').trim();
        if (/^\w+$/.test(t)) local.add(t);
      }
    }
    // 对象解构：const { a, b: c } =
    for (const m of code.matchAll(/(?:const|let|var)\s*\{([^}]+)\}/g)) {
      for (const p of m[1].split(',')) {
        const t = p.split(':').pop().trim().split('=')[0].replace(/^\.+/, '').trim();
        if (/^\w+$/.test(t)) local.add(t);
      }
    }
    info.set(f, { imports, top, local, code });
    for (const name of top) {
      if (!topAll.has(name)) topAll.set(name, []);
      topAll.get(name).push(f);
    }
  }
  const bad = [];
  for (const [f, d] of info) {
    for (const [name, owners] of topAll) {
      if (owners.includes(f) || d.top.has(name) || d.local.has(name) || d.imports.has(name) || JS_GLOBALS.has(name)) continue;
      const pat = new RegExp('(?<![\\w$.])' + name + '(?![\\w$])', 'g');
      let used = false;
      for (const m of d.code.matchAll(pat)) {
        const lineStart = d.code.lastIndexOf('\n', m.index) + 1;
        const linePre = d.code.slice(lineStart, m.index);
        const after = d.code.slice(m.index + name.length);
        const lineText = linePre + name + after.split('\n')[0];
        if (new RegExp('[{,]\\s*' + name + '\\s*:').test(linePre + name + after.slice(0, 4))) continue; // { name: ... }
        if (new RegExp('(?:^\\s*|[{,]\\s*)' + name + '\\s*[:(]').test(lineText)) continue; // 方法简写/标签位
        used = true;
        break;
      }
      if (used && !XREF_ALLOW.has(name)) {
        bad.push(moduleKey(f).split('/').slice(-2).join('/') + ' -> ' + name +
          '（顶层声明于：' + owners.map((x) => x.split('/').pop()).join(',') + '）');
      }
    }
  }
  if (bad.length) {
    console.log('XREF FAIL —— 以下模块引用了别处顶层声明的名字、却没 import（运行时 ReferenceError）：');
    for (const b of bad) console.log('  ' + b);
    return false;
  }
  console.log('XREF OK —— 跨模块自由标识符核对通过（' + files.length + ' 个模块）');
  return true;
}

// ---------- DYN：动态 import() 说明符的**可解析性**核对 ----------
//
// 由来三（v3.0.20 真机教训）：LINK 层用 SourceTextModule.link() 只解析**静态** import/export，
// **动态** `import('…')` 是运行期才取模块，link() 一概看不见。Web 重构后
// `js/core/dom.js` 的 `goDetail()` 写了 `import('../router.js')`——本文件在 js/core/ 下，
// 该说明符解析到 **js/router.js（不存在）**，而 router.js 实际在 ui/ 根。后果是**详情按钮与
// 列表行点击全部静默失效**（只抛一个未处理的 rejection，界面无任何提示），零对象/纯 hash
// 走查都抓不到（hash 直连走的是 app.js 的静态 import，那条路是好的）。
// 故本层把每个相对说明符**按文件所在目录解析**并断言目标存在。
function dynCheck() {
  const files = [];
  (function walk(dir) {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (e.name === 'novnc') continue; // 第三方库，不为我们的拆分管
      const p = path.join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name.endsWith('.js')) files.push(p);
    }
  })(ROOT);
  const bad = [];
  for (const f of files) {
    const raw = fs.readFileSync(f, 'utf8');
    for (const m of raw.matchAll(/import\(\s*['"]([^'"]+)['"]\s*\)/g)) {
      const spec = m[1];
      if (!spec.startsWith('.')) continue; // 裸说明符/URL 不在本层范围
      const target = path.resolve(path.dirname(f), spec);
      if (!fs.existsSync(target)) {
        bad.push(moduleKey(path.relative(ROOT, f)).replace(/\\/g, '/') + ' -> ' + spec +
          '（解析到 ' + moduleKey(path.relative(ROOT, target)).replace(/\\/g, '/') + '，该文件不存在）');
      }
    }
  }
  if (bad.length) {
    console.log('DYN FAIL —— 以下动态 import() 的相对路径解析不到文件（运行期 404 ⇒ 该动作静默失效）：');
    for (const b of bad) console.log('  ' + b);
    return false;
  }
  console.log('DYN OK —— 动态 import() 说明符全部可解析（' + files.length + ' 个模块）');
  return true;
}

// ---------- 主流程：LINK（链接期）→ XREF（自由标识符） ----------

(async () => {
  for (const f of ENTRY) getModule(f);
  // 循环 link 直到闭包完成（link 是传递闭包，逐个顺序推进并对幂等报错容错）。
  for (;;) {
    const pending = [...mods.values()].filter((m) => m.status === 'unlinked');
    if (!pending.length) break;
    let progressed = false;
    for (const m of pending) {
      try {
        await m.link(resolve);
        progressed = true;
      } catch (e) {
        if (/already been linked/.test(e.message)) continue;
        console.log('LINK FAIL on ' + m.identifier + ' -> ' + e.message);
        process.exit(1);
      }
    }
    if (!progressed) {
      console.log('LINK FAIL: 仍有未链接模块（依赖图异常）');
      process.exit(1);
    }
  }
  console.log('LINK OK —— ' + mods.size + ' 个模块的 import 名全部在目标模块有导出');
  if (!xrefCheck()) process.exit(1);
  if (!dynCheck()) process.exit(1);
})().catch((e) => {
  console.log('LINK FAIL: ' + e.message);
  process.exit(1);
});
