// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 虚拟机列表域：列表行 + 生命周期动作 + 统计。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, expandEndpoint, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsvmsRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsvmsRouterModule()).render();
}

// ---------- 虚拟机：列表行（点行进详情）+ 生命周期动作 ----------

// 状态 → 允许的动作（与 CLI 同口径：运行中可 stop/restart，关机态可 start）。
// 列表页与详情页共用这份判定，两边不会分叉。
const VM_ACTIONS = [
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
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 生命周期动作。`msg` 是回显去处：列表页用运维页的提示区（历史行为），
// 详情页用自己的提示区（vmDetailMsg）——同一套动作、同一套状态判定，只是回显位置不同。
// 确认档位：低危（单台 VM 的启停/重启，坏了再点回去即可）。
const VM_ACTION_IMPACT = {
  start: '虚拟机按自身配置启动，启动期间不占用其他对象。',
  stop: '运行中的业务会中断；停止是正常关机（超时会强杀），配置与磁盘保留。',
  restart: '重启期间业务中断数秒到数十秒。',
};
async function vmAction(name, action, label, msg) {
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

// vhost-user 口计数（详情端点附带的 statistics；运行态未接入时如实说"未取到计数"）。
// 渲染到 `id` 指定的 pre 上（详情页的概览 Tab）。
function renderVMStats(vms, stats, id) {
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
    }
  },
};
