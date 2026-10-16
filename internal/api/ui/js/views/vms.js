// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 虚拟机列表域：列表行 + 生命周期动作 + 统计。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { opsMsg } from './ops.js';
// 就地新建 / 编辑（配置页去中心化第二刀·计算族）：面板复用配置页的「虚拟机」节定义
// （CFG_SECTIONS 里的同一条，不复制字段清单），写路径走 core/forms.js 的同一套 candidate
// 事务（取锁 → 写候选 → 校验 → 提交 → 释放）。删除沿用本文件既有的 vmDelete（同口径中危确认）。
import { CFG_SECTIONS } from './config.js';
import { objEditor } from '../core/forms.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsvmsRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsvmsRouterModule()).render();
}

// ---------- 虚拟机：就地新建 / 编辑（列表页「+ 新建」、详情页「编辑」）----------

// 运行态缓存：列表页与详情页渲染时各自刷新，编辑面板打开时据此如实提示「需 stop+start 生效」。
const vmStateCache = {};
export function vmRememberState(name, state) {
  if (name) vmStateCache[name] = state;
}
function vmIsRunning(name) {
  const s = String(vmStateCache[name] || '');
  return s === 'running' || s === 'paused';
}

// 种类描述：节定义取自配置页的 CFG_SECTIONS（**同一份**节，字段新增/变更自动生效）；
// cli* 只在确认框里做只读回显（工单对照用）。
function vmKind() {
  const all = Array.isArray(CFG_SECTIONS) ? CFG_SECTIONS : [];
  return {
    label: '虚拟机',
    section: all.find((s) => s.list === 'virtual_machine_functions') || null,
    cliNew: (n) => 'set virtual-machine-functions ' + n + ' …（镜像 / 资源 / vNIC / 启动画像 …） + commit',
    cliSave: (n) => 'set virtual-machine-functions ' + n + ' …（镜像 / 资源 / vNIC / 启动画像 …） + commit',
    cliDelete: (n) => 'request virtual-machine-functions ' + n + ' delete',
    // 运行中 VM 的形态变更：产品口径 FR-CMP-012 是「需停机重定义才生效」——提交只落配置，
    // 运行中不做热修改。面板里先提示，别让人以为改完立刻作用到运行中的虚拟机上。
    note: (mode, name) => (name && vmIsRunning(name)
      ? '注意：虚拟机 ' + name + ' 当前正在运行——vCPU / 内存 / 启动画像 / vNIC / 磁盘这类形态变更' +
        '提交后只落配置，需 stop 后 start（停机重定义）才会作用到这台虚拟机上，运行中不会热改。'
      : ''),
    bullets: (mode, name) => (name && vmIsRunning(name)
      ? ['该虚拟机正在运行：本次形态变更提交后只落配置，需 stop + start 才生效（停机重定义；产品不做热修改）。']
      : []),
  };
}

// 列表页一块面板（「+ 新建」）；详情页一块面板（「编辑」）。删除沿用 vmDelete（中危确认），
// 不复用 objEditor.remove——它只删配置条目，而这里的删除还要清磁盘（服务端
// `request virtual-machine-functions <n> delete` 一并做掉）。
const vmListEditor = objEditor({ slot: 'vm-list-edit', reload });
const vmdEditor = objEditor({ slot: 'vmd-edit', reload });
// 详情页当前对象名（#vmd-name 由 vmDetail.js 的渲染写入）——名称刻意避开 vmDetail.js 的
// vmDetailName（桩式自校准把各模块拼进同一作用域，重名会让整份前端加载期报 SyntaxError）。
function vmdNameOf() { const n = $('vmd-name'); return (n && n.textContent) || ''; }
if (vmListEditor) {
  const b = $('vm-new-btn');
  if (b) b.addEventListener('click', () => vmListEditor.openNew(vmKind()));
}
if (vmdEditor) {
  const eb = $('vmd-edit-btn');
  if (eb) eb.addEventListener('click', () => vmdEditor.openEdit(vmKind(), vmdNameOf()));
}
// 详情页面板的收场钩子（vmDetail.js 的 leave 调用）：草稿不跨页残留。
export function vmDetailEditorLeave() { if (vmdEditor) vmdEditor.leave(); }

// ---------- 虚拟机：列表行（点行进详情）+ 生命周期动作 ----------

// 状态 → 允许的动作（与 CLI 同口径：运行中可 stop/restart，关机态可 start）。
// 列表页与详情页共用这份判定，两边不会分叉。
export const VM_ACTIONS = [
  { key: 'start', label: '启动', states: ['shutoff', 'crashed', '-'] },
  { key: 'stop', label: '停止', states: ['running', 'paused'] },
  { key: 'restart', label: '重启', states: ['running', 'paused'] },
];

function renderVMRows(vms) {
  const tbody = $('vm-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = vms || [];
  $('vm-note').textContent = rows.length ? '（' + rows.length + ' 台；点行进详情，动作按钮按当前状态启用）' : '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((v) => {
    vmRememberState(v.name, v.state); // 供「编辑」面板判断是否运行中（形态变更提示）
    const tr = rowClickable(el('tr'), '#/compute/vms/' + encodeURIComponent(v.name));
    [v.name, v.state, v.vcpu ? v.vcpu.count : undefined,
      v.memory ? mb(v.memory.size_mb) : undefined, v.image].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    VM_ACTIONS.forEach((a) => {
      const btn = rowButton(wbtn({ type: 'button', class: 'ghost small', text: a.label }));
      btn.disabled = a.states.indexOf(String(v.state)) < 0;
      btn.addEventListener('click', () => vmAction(v.name, a.key, a.label, opsMsg));
      cell.appendChild(btn);
    });
    // 删除 = super-user 级写入口（DELETE /virtual-machine-functions/{name}，见 server.go 的 cfgAPI）
    // ——必须经 wbtn 带 data-write，否则 read-only/operator 也会看到它（决策 #327）。
    const delBtn = rowButton(wbtn({ type: 'button', class: 'danger small', text: '删除' }));
    delBtn.addEventListener('click', () => vmDelete(v.name, vmListMsg));
    cell.appendChild(delBtn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 列表页的提示落点（#vm-msg）。删除走这里而不是 opsMsg：opsMsg 在运维动作页，从本页看不见，
// 而删除失败（如对象仍被引用）必须把服务端的拒绝理由摆在操作者眼前。
function vmListMsg(text, isErr) {
  const p = $('vm-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// 生命周期动作。`msg` 是回显去处：列表页用运维页的提示区（历史行为），
// 详情页用自己的提示区（vmDetailMsg）——同一套动作、同一套状态判定，只是回显位置不同。
// 确认档位：低危（单台 VM 的启停/重启，坏了再点回去即可）。
const VM_ACTION_IMPACT = {
  start: '虚拟机按自身配置启动，启动期间不占用其他对象。',
  stop: '运行中的业务会中断；停止是正常关机（超时会强杀），配置与磁盘保留。',
  restart: '重启期间业务中断数秒到数十秒。',
};
export async function vmAction(name, action, label, msg) {
  const say = msg || opsMsg;
  const ok = await uiConfirm(label + '虚拟机', {
    tier: 'low',
    paragraphs: ['对虚拟机 ' + name + ' 执行「' + label + '」？' +
      (VM_ACTION_IMPACT[action] || '')],
    cli: 'request virtual-machine-functions ' + name + ' ' + action,
  });
  if (!ok) return;
  say(label + ' ' + name + '：执行中…', false);
  try {
    await api('/virtual-machine-functions/' + encodeURIComponent(name) + ':' + action, { method: 'POST' });
    say(label + ' ' + name + '：已受理。', false);
  } catch (e) {
    say(label + ' ' + name + ' 失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 删除虚拟机（与 CLI `request virtual-machine-functions <name> delete` 同源）。
// 确认档位：中危（删对象——配置与磁盘数据一并移除、不可恢复，且可能正被别的对象引用）。
// `say` 是回显去处：列表页用本页提示区（vmListMsg），详情页用详情页提示区。
// `navTo` 只给详情页用：删成功后该对象已不存在，留在详情页只会显示"读取失败"，故回列表页。
export async function vmDelete(name, say, navTo) {
  const emit = say || opsMsg;
  const ok = await uiConfirm('删除虚拟机', {
    tier: 'mid',
    paragraphs: ['删除虚拟机 ' + name + '。'],
    bullets: [
      '虚拟机的配置与磁盘数据一并删除（含其快照），不可恢复。',
      '删除立即提交生效（与 CLI 同口径），不需要再到「配置」页提交。',
      '运行中的虚拟机会先被停止，业务随之中断。',
      '仍被别的对象引用时服务端会拒绝（如交换机/接口上还挂着它的 vNIC）——按服务端提示先解引用再删。',
    ],
    cli: 'request virtual-machine-functions ' + name + ' delete',
  });
  if (!ok) return;
  emit('删除 ' + name + '：执行中…', false);
  try {
    // 服务端两条硬要求，缺一即失败：`confirm=true`（二次确认，vm.go 的 handleDeleteVM）
    // 与 `X-NFVIS-Auto-Commit: true`（写候选 + 提交一个请求内完成的一次性事务）。
    // 用一次性事务是与 CLI 同口径（`request virtual-machine-functions <n> delete` 走
    // commitMutate = edit→mutate→commit），删完即生效；**不是** QoS/SPAN 那种"只写候选、
    // 到配置页提交才生效"的路径——删除类动作若只写候选，对象仍在列表里，等于假删除。
    await api('/virtual-machine-functions/' + encodeURIComponent(name) + '?confirm=true',
      { method: 'DELETE', headers: { 'X-NFVIS-Auto-Commit': 'true' } });
    emit('已删除 ' + name + '。', false);
    if (navTo) { await goDetail(navTo); return; }
  } catch (e) {
    // 服务端拒绝的理由（被引用 / 校验类 detail[]）原样展示，不吞。
    emit('删除失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// vhost-user 口计数（详情端点附带的 statistics；运行态未接入时如实说"未取到计数"）。
// 渲染到 `id` 指定的 pre 上（详情页的概览 Tab）。
export function renderVMStats(vms, stats, id) {
  const pre = $(id);
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

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsVms = {
  'vms': {
    render(d) {
      pageWarn(d);
      renderVMRows(rowsOf(d['/virtual-machine-functions']));
    },
    // 离开本页（router 的 leave 钩子）：收掉就地编辑面板——草稿不跨页残留。
    leave() { if (vmListEditor) vmListEditor.leave(); }
  },
};
