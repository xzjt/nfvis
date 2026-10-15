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
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewscontainersRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewscontainersRouterModule()).render();
}

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
    VM_ACTIONS.forEach((a) => {
      const btn = rowButton(wbtn({ type: 'button', class: 'ghost small', text: a.label }));
      btn.disabled = a.states.indexOf(String(c.state)) < 0;
      btn.addEventListener('click', () => ctAction(c.name, a.key, a.label, opsMsg));
      cell.appendChild(btn);
    });
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
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

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsContainers = {
  'containers': {
    render(d) { pageWarn(d); renderContainerRows(rowsOf(d['/container-functions'])); }
  },
};
