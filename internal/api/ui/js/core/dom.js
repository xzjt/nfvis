// NFViS 控制台共享件 · DOM 与对话框（从 app.js 拆出，逻辑逐字保留；Web 控制台重构 2026-10）。
//
// 本模块零业务知识：只提供元素构造/表格填充/分级确认对话框这些页面无关的件。
// 路由模块（router.js）与各视图模块都从这里取用。

// ---------- 小工具 ----------

export const $ = (id) => document.getElementById(id);

// 全部用 textContent 落值（不用 innerHTML 拼数据），服务端返回的字符串因此不会变成标记。
export function el(tag, attrs, children) {
  const node = document.createElement(tag);
  for (const k in (attrs || {})) {
    if (k === 'text') node.textContent = attrs[k];
    else if (k === 'class') node.className = attrs[k];
    else node.setAttribute(k, attrs[k]);
  }
  (children || []).forEach((c) => node.appendChild(c));
  return node;
}

export const dash = (v) => (v === undefined || v === null || v === '' ? '—' : v);

// wbtn：**动态生成的写/运维入口**（决策 #145，设计 §8）——带 `data-write` 标记，
// read-only 账号下由 CSS 隐藏（见 style.css 的 body.role-readonly 规则）。
// 只用于"会改状态或需要 O 级以上"的动作；读类（详情/刷新/下载/看差异）仍用 el()。
export const wbtn = (opts) => el('button', Object.assign({ 'data-write': '' }, opts));

export function fill(dl, pairs) {
  dl.textContent = '';
  pairs.forEach(([k, v]) => {
    dl.appendChild(el('dt', { text: k }));
    dl.appendChild(el('dd', { text: String(dash(v)) }));
  });
}

export function table(tbody, cols, rows) {
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

export function list(v) {
  if (v === undefined || v === null) return undefined;
  return Array.isArray(v) ? (v.length ? v.join(',') : '—') : String(v);
}

// ---------- 确认对话框（分级确认：低 / 中 / 高，三档共用一个组件） ----------
//
// 三档的差别只看「要不要把人拦一下」，不是三套实现：
//   低危（tier 'low'）：一次确认。文案用 paragraphs 说清要做什么，不列影响面、不要求输入。
//     用于可回退、影响单对象、坏了也能马上再点回去的动作（单台 VM/容器启停、清计数、
//     清告警、删快照、取消抓包…）。
//   中危（tier 'mid'）：逐条列出**影响面**（bullets：谁会断、是否需重启生效、能不能回退）
//     + 主按钮标红。用于重启数据面/主机、删对象、重签证书、重生成 SSH host key、配置提交/
//     回滚这类「影响面跨对象或跨会话」的动作。
//   高危（tier 'high'）：中危的全部，再加两道闸门——
//     ① 输入确认词（调用方给 requireWord：对象名或固定词；输入不匹配则「执行」保持禁用）；
//     ② 倒计时（默认 10 秒，走完之前「执行」保持禁用，期间随时可取消）。
//     用于恢复出厂、软件升级/回退、恢复配置、删用户/改口令策略、证书上传这类不可逆动作。
//
// 三档共同点：内容一律经 textContent 落值（服务端字符串不会变成标记）；点遮罩、按 Esc 都算取消；
// 每个框都**只读回显**这条动作对应的命令行语句（工单与审计对照用——操作者不需要输入语句）。
// 审计由服务端记；界面只负责把「意图」说清楚。
//
// 返回 Promise<boolean>：确认 true / 取消（点取消、点遮罩、按 Esc）false。
let dialogResolve = null;
let dialogTimer = null;    // 高危档的倒计时（取消/关闭时必须清掉，否则它会继续跑并动到下一个框）
export const CONFIRM_COUNTDOWN = 10;  // 高危档倒计时秒数（调用方可用 countdown 覆盖，自校准用短值）

export function uiDialog(opts) {
  const o = opts || {};
  const tier = o.tier === 'high' ? 'high' : (o.tier === 'mid' ? 'mid' : 'low');
  const high = tier === 'high';
  const mid = tier !== 'low';
  const countdown = high
    ? Math.max(0, Math.floor(Number(o.countdown === undefined ? CONFIRM_COUNTDOWN : o.countdown) || 0))
    : 0;
  // 高危必须有确认词：调用方没给就**失败关闭**（宁可这个动作点不动，也不能放过一道闸门）。
  const word = high ? String(o.requireWord === undefined ? '' : o.requireWord) : '';
  const wordMissing = high && !word;
  const label = o.confirmLabel || (high ? '执行' : '确定');

  $('modal-title').textContent = o.title || '';
  const body = $('modal-body');
  body.textContent = '';
  (o.paragraphs || []).forEach((t) => body.appendChild(el('p', { text: t })));
  // 影响面逐条列（低危不列：单击确认不该被一屏字挡住）。
  if (mid && (o.bullets || []).length) {
    const ul = el('ul');
    o.bullets.forEach((t) => ul.appendChild(el('li', { text: t })));
    body.appendChild(ul);
  }
  const cli = $('modal-cli');
  cli.hidden = !o.cli;
  cli.textContent = o.cli ? '对应命令（只读回显，便于工单对照）：' + o.cli : '';

  // 高危档的两道闸门。两个状态变量是唯一的判据，`syncOk()` 只读它们——
  // 「执行」按钮的可用性完全由它决定（自校准就是拆这两个变量）。
  let wordOK = !high;
  let timeOK = countdown <= 0;
  const gatesOK = () => wordOK && timeOK;

  // 闸门控件整块由这里现建（见 index.html 的骨架只有容器）：每次打开框都重建，
  // 不会把上一次的输入或状态带进来。
  const guard = $('modal-guard');
  guard.hidden = !high;
  guard.textContent = '';
  let wordInput = null;
  if (high) {
    const hint = el('p', {
      class: wordMissing ? 'small error' : 'small',
      text: wordMissing
        ? '这一步没有拿到确认词，无法执行（请报告这一处缺陷）。'
        : '这一步要求手工输入确认词：' + word + '（区分大小写）。输入不匹配时「执行」保持不可点。',
    });
    wordInput = el('input', {
      type: 'text', id: 'modal-word', autocomplete: 'off', spellcheck: 'false',
      placeholder: wordMissing ? '' : word,
    });
    wordInput.disabled = wordMissing;
    wordInput.addEventListener('input', () => {
      wordOK = !wordMissing && wordInput.value.trim() === word;
      syncOk();
    });
    // 回车 = 点「执行」，只在闸门都放行时生效（禁用状态下回车什么也不做）。
    wordInput.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Enter') return;
      if (ev.preventDefault) ev.preventDefault();
      if (gatesOK()) closeDialog(true);
    });
    guard.appendChild(hint);
    guard.appendChild(wordInput);
    guard.appendChild(el('p', {
      class: 'muted small', id: 'modal-count',
      text: wordMissing ? ''
        : (countdown > 0
          ? '还有 ' + countdown + ' 秒倒计时结束；倒计时期间可以随时取消。'
          : '本次没有倒计时（可立即执行）。'),
    }));
  }

  const acts = $('modal-actions');
  acts.textContent = '';
  const ok = el('button', { type: 'button', id: 'modal-ok', class: mid || o.danger ? 'danger' : '' });
  ok.addEventListener('click', () => { if (!ok.disabled) closeDialog(true); });
  // 倒计时与闸门状态都要如实显示在按钮上：禁用时按钮上写着还要等几秒。
  let left = countdown;
  function syncOk() {
    if (!high) {
      ok.disabled = false;
      ok.textContent = label;
      return;
    }
    ok.disabled = !gatesOK();
    ok.textContent = timeOK ? label : (label + '（' + left + ' 秒后可点）');
  }
  // 焦点落在「取消」上（没有取消按钮时落在主按钮）：破坏性动作不该被一个回车键直接执行。
  // 高危档焦点在确认词输入框上（本来就要打字，且它在闸门放行前不触发任何动作）。
  let focusTarget = wordInput || ok;
  if (o.cancelLabel !== '') {
    const cancel = el('button', { type: 'button', id: 'modal-cancel', class: 'ghost', text: o.cancelLabel || '取消' });
    cancel.addEventListener('click', () => closeDialog(false));
    acts.appendChild(cancel);
    focusTarget = wordInput || cancel;
  }
  acts.appendChild(ok);

  // 倒计时：每秒一格，走完即放行（此刻确认词还得对）。
  if (high && left > 0) {
    dialogTimer = setInterval(() => {
      left -= 1;
      const count = $('modal-count');
      if (left <= 0) {
        left = 0;
        timeOK = true;
        if (dialogTimer) { clearInterval(dialogTimer); dialogTimer = null; }
        if (count) count.textContent = '倒计时已结束，现在可以执行（确认词仍需匹配）。';
      } else if (count) {
        count.textContent = '还有 ' + left + ' 秒倒计时结束；倒计时期间可以随时取消。';
      }
      syncOk();
    }, 1000);
  }
  syncOk();

  $('modal').hidden = false;
  if (focusTarget.focus) focusTarget.focus();
  return new Promise((resolve) => { dialogResolve = resolve; });
}

export function closeDialog(value) {
  if (dialogTimer) { clearInterval(dialogTimer); dialogTimer = null; }
  $('modal').hidden = true;
  const resolve = dialogResolve;
  dialogResolve = null;
  if (resolve) resolve(value);
}

// uiConfirm：给动作调用点用的薄封装——标题统一是动作名，其余按档位传
// （tier / paragraphs / bullets / cli / confirmLabel / requireWord）。
export function uiConfirm(label, opts) {
  const o = Object.assign({}, opts || {});
  o.title = o.title || label;
  return uiDialog(o);
}

// ---------- 列表行交互（VM/容器/镜像等列表页共用） ----------

// 行内按钮：拦掉冒泡——否则点动作会连带触发行点击（跳进详情页）。动作与拦截是两个监听器，
// 都在按钮自身上，故互不影响。
export function rowButton(btn) {
  btn.addEventListener('click', (ev) => ev.stopPropagation());
  return btn;
}

// 列表行的行点击：进该对象的详情页（地址即 `#/…/<name>`，可分享、可刷新保持）。
export function rowClickable(tr, path) {
  tr.className = 'row-link';
  tr.addEventListener('click', () => goDetail(path));
  return tr;
}

// 详情页跳转：路由模块按需动态 import（与 reload 同一路，避免静态互相 import 成环）。
// 名字带 Go 前缀：app.js 顶层已有 routerModule（'./router.js'），拼接域里不能撞名。
async function goRouterModule() {
  return import('../router.js');
}
export async function goDetail(path) {
  (await goRouterModule()).navigate(path);
}
