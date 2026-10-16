// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 容器列表域：列表行 + 生命周期动作。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { opsMsg } from './ops.js';
import { VM_ACTIONS } from './vms.js';
// 就地编辑（配置页去中心化第二刀·计算族）：面板复用配置页的「容器」节定义（CFG_SECTIONS 里的
// 同一条，不复制字段清单），写路径走 core/forms.js 的同一套 candidate 事务（取锁 → 写候选 →
// 校验 → 提交 → 释放）。删除沿用本文件既有的 ctDelete（同口径中危确认）。
// **不在这里造创建入口**：新建容器走 #/deploy 部署向导（或命令行），避免两套创建路径漂移。
import { CFG_SECTIONS } from './config.js';
import { objEditor } from '../core/forms.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewscontainersRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewscontainersRouterModule()).render();
}

// ---------- 容器：就地编辑（列表行「编辑」、详情页「编辑」）----------

// 种类描述：节定义取自配置页的 CFG_SECTIONS（**同一份**节）；cli* 只在确认框里做只读回显。
function ctKind() {
  const all = Array.isArray(CFG_SECTIONS) ? CFG_SECTIONS : [];
  return {
    label: '容器',
    section: all.find((s) => s.list === 'container_functions') || null,
    cliNew: (n) => 'set container-functions ' + n + ' …（镜像 / 资源 / vNIC …） + commit',
    cliSave: (n) => 'set container-functions ' + n + ' …（镜像 / 资源 / vNIC …） + commit',
    cliDelete: (n) => 'request container-functions ' + n + ' delete',
  };
}

// 列表页一块面板（行内「编辑」用）；详情页一块面板（「编辑」用）。删除沿用 ctDelete
// （中危确认 + confirm=true + 一次性事务），不复用 objEditor.remove——理由同 vms.js。
const ctListEditor = objEditor({ slot: 'ct-list-edit', reload });
const ctdEditor = objEditor({ slot: 'ctd-edit', reload });
function ctdName() { const n = $('ctd-name'); return (n && n.textContent) || ''; }
if (ctdEditor) {
  const eb = $('ctd-edit-btn');
  if (eb) eb.addEventListener('click', () => ctdEditor.openEdit(ctKind(), ctdName()));
}
// 详情页面板的收场钩子（containerDetail.js 的 leave 调用）。
export function ctDetailEditorLeave() { if (ctdEditor) ctdEditor.leave(); }

// ---------- 容器：列表行（点行进详情）+ 生命周期 ----------

// 与 VM 同一套状态口径（容器状态来自 Docker）。
function renderContainerRows(cts) {
  const tbody = $('ct-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = cts || [];
  $('ct-note').textContent = rows.length ? '（' + rows.length + ' 个；点行进详情，动作按钮按当前状态启用）' : '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((c) => {
    const tr = rowClickable(el('tr'), '#/compute/containers/' + encodeURIComponent(c.name));
    [c.name, c.state, c.vcpu, c.memory_mb ? mb(c.memory_mb) : undefined, c.image].forEach((v) => {
      tr.appendChild(el('td', { text: String(dash(v)) }));
    });
    const cell = el('td', { class: 'actions' });
    const detBtn = rowButton(el('button', { type: 'button', class: 'ghost small', text: '详情' }));
    detBtn.addEventListener('click', () => goDetail('#/compute/containers/' + encodeURIComponent(c.name)));
    cell.appendChild(detBtn);
    // 行内「编辑」= super-user 级写入口（写入路径是 candidate 事务）——必须经 wbtn 带 data-write，
    // 否则 read-only/operator 也会看到它（决策 #327）。
    const editBtn = rowButton(wbtn({ type: 'button', class: 'small', text: '编辑' }));
    editBtn.addEventListener('click', () => { if (ctListEditor) ctListEditor.openEdit(ctKind(), c.name); });
    cell.appendChild(editBtn);
    VM_ACTIONS.forEach((a) => {
      const btn = rowButton(wbtn({ type: 'button', class: 'ghost small', text: a.label }));
      btn.disabled = a.states.indexOf(String(c.state)) < 0;
      btn.addEventListener('click', () => ctAction(c.name, a.key, a.label, opsMsg));
      cell.appendChild(btn);
    });
    // 删除 = super-user 级写入口（DELETE /container-functions/{name}，见 server.go 的 cfgAPI）
    // ——必须经 wbtn 带 data-write，否则 read-only/operator 也会看到它（决策 #327）。
    const delBtn = rowButton(wbtn({ type: 'button', class: 'danger small', text: '删除' }));
    delBtn.addEventListener('click', () => ctDelete(c.name, ctListMsg));
    cell.appendChild(delBtn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 列表页的提示落点（#ct-msg）。理由同 vms.js：删除失败的理由要落在本页可见处，不能落到
// 运维动作页的提示区（从本页看不见）。
function ctListMsg(text, isErr) {
  const p = $('ct-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// `msg` 是回显去处：列表页用运维页的提示区（历史行为），详情页用本页的（ctdMsg）。
// 确认档位：低危（单个容器的启停/重启）。
const CT_ACTION_IMPACT = {
  start: '容器按其配置启动。',
  stop: '容器内的进程会被停止；容器与镜像保留，可以再启动。',
  restart: '容器内的进程会被重启，服务短暂中断。',
};
export async function ctAction(name, action, label, msg) {
  const say = msg || opsMsg;
  const ok = await uiConfirm(label + '容器', {
    tier: 'low',
    paragraphs: ['对容器 ' + name + ' 执行「' + label + '」？' + (CT_ACTION_IMPACT[action] || '')],
    cli: 'request container-functions ' + name + ' ' + action,
  });
  if (!ok) return;
  say(label + ' ' + name + '：执行中…', false);
  try {
    await api('/container-functions/' + encodeURIComponent(name) + ':' + action, { method: 'POST' });
    say(label + ' ' + name + '：已受理。', false);
  } catch (e) {
    say(label + ' ' + name + ' 失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 删除容器（与 CLI `request container-functions <name> delete` 同源）。
// 确认档位：中危（删对象——容器与其可写层一并移除、不可恢复）。
// `say` 是回显去处：列表页用本页提示区（ctListMsg），详情页用详情页提示区。
// `navTo` 只给详情页用：删成功后容器已不存在，留在详情页只会显示"读取失败"，故回列表页。
export async function ctDelete(name, say, navTo) {
  const emit = say || opsMsg;
  const ok = await uiConfirm('删除容器', {
    tier: 'mid',
    paragraphs: ['删除容器 ' + name + '。'],
    bullets: [
      '容器与其可写层一并删除，不可恢复（镜像本身不受影响，可以再建）。',
      '删除立即提交生效（与 CLI 同口径），不需要再到「配置」页提交。',
      '运行中的容器会先被停止，容器内进程随之中断。',
      '仍被别的对象引用时服务端会拒绝（如交换机上还挂着它的 vNIC）——按服务端提示先解引用再删。',
    ],
    cli: 'request container-functions ' + name + ' delete',
  });
  if (!ok) return;
  emit('删除 ' + name + '：执行中…', false);
  try {
    // 与 VM 删除同口径（见 vms.js）：`confirm=true`（二次确认，container.go 的
    // handleDeleteContainer）+ `X-NFVIS-Auto-Commit: true`（写候选 + 提交一个请求内完成）——
    // 删完即生效，与 CLI `request container-functions <n> delete`（commitMutate）一致。
    await api('/container-functions/' + encodeURIComponent(name) + '?confirm=true',
      { method: 'DELETE', headers: { 'X-NFVIS-Auto-Commit': 'true' } });
    emit('已删除 ' + name + '。', false);
    if (navTo) { await goDetail(navTo); return; }
  } catch (e) {
    // 服务端拒绝的理由（被引用 / 校验类 detail[]）原样展示，不吞。
    emit('删除失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsContainers = {
  'containers': {
    render(d) { pageWarn(d); renderContainerRows(rowsOf(d['/container-functions'])); },
    // 离开本页（router 的 leave 钩子）：收掉就地编辑面板——草稿不跨页残留。
    leave() { if (ctListEditor) ctListEditor.leave(); }
  },
};
