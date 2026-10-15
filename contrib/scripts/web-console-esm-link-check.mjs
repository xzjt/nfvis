//go:build ignore

// Web 控制台前端 ES module 图的**链接期守护**（Web 控制台重构 2026-10 配套工具）。
//
// 由来：app.js 按域拆分后（ui/js/core/** + ui/js/views/** + ui/js/boot.js + 聚合入口 app.js），
// 各模块是独立作用域的 ES modules——「跨模块引用了没导出的名字」在浏览器里是加载期错误
// （整条 import 链一起死，页面只剩登录壳），而 vm 桩自校准的「递归拼接成经典脚本」加载器
// 把所有模块混进同一作用域，这类错误在桩里**永远看不到**（browser-use 黑盒第一轮就抓到
// expandEndpoint/setStream/usrBannerSave 等一批幻影 import）。
//
// 本脚本用 node --experimental-vm-modules 的 SourceTextModule 对整条 import 图做
// link()——正是浏览器加载期的同一道检查。用法：
//
//   node --experimental-vm-modules contrib/scripts/web-console-esm-link-check.mjs
//
// 退出码：0 = 全部 import 名在目标模块都有导出；1 = 有缺口（逐条打印）。
// 不进 make toolcheck（无 node 时由 shell 层如实跳过——与两桩自校准同口径）。
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'internal', 'api', 'ui');
const SL = String.fromCharCode(92); // 反斜杠（脚本自身要保持无字面反斜杠正则的兼容写法）

// 入口：app.js（聚合）+ router.js + vncview.js 会沿 import 递归到全部 js/core/**、js/views/**。
const ENTRY = ['app.js', 'router.js', 'vncview.js'].map((f) => path.join(ROOT, f));

const mods = new Map();
function moduleKey(p) {
  return p.split(SL).join('/');
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
})().catch((e) => {
  console.log('LINK FAIL: ' + e.message);
  process.exit(1);
});
