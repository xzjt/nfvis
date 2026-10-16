// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 容器详情域：详情页 / 日志 / exec / 交互终端。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { ctAction, ctDetailEditorLeave } from './containers.js';
import { VM_ACTIONS } from './vms.js';
import { stripANSI } from './vmDetail.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewscontainerDetailRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewscontainerDetailRouterModule()).render();
}

// ---------- 容器详情页（#/compute/containers/:name）----------
//
// 对象头 + 两个 Tab（概览 / 日志）。日志复用 ctLogsLoad（tail=200，与 CLI 同源），
// 且**进 Tab 或点「刷新」才拉**——大段文本不该跟着页面轮询反复下载。

// 容器详情页的分栏：新增分栏必须同时加到这里（`ctTabShow` 按它切换面板，
// 漏加会让点击静默回落「概览」——静态守护看不见，只有真机点一遍才发现，决策 #141）。
const CT_TABS = ['overview', 'logs', 'exec', 'shell'];
let ctTab = 'overview';
let ctDetailName = '';
export function getCtDetailName() { return ctDetailName; }
let ctLogsName = '';

function ctTabShow(tab) {
  if (CT_TABS.indexOf(tab) < 0) tab = 'overview';
  ctTab = tab;
  CT_TABS.forEach((t) => { $('ct-tab-' + t).hidden = t !== tab; });
  const bar = $('ctd-tabs');
  for (const b of bar.querySelectorAll('button')) {
    if (b.dataset.tab === tab) b.setAttribute('aria-selected', 'true');
    else b.removeAttribute('aria-selected');
  }
}

// 点 Tab：切面板；进日志 Tab 顺手拉一次（与原来点行内「日志」按钮同一行为）。
export function ctTabClick(tab) {
  ctTabShow(tab);
  if (tab === 'logs') ctLogsOpen(ctDetailName);
}

// ---- 容器内执行命令（决策 #357，S 档）----
// 入口是静态写控件（data-write），operator/只读由 body.role-nonsuper 的 CSS 隐藏（#145/#327 同一门禁）。
// 如实口径：命令跑完（哪怕非 0 退出码）⇒ 成功，退出码是**结果**；超时只中止本页等待，
// 此时**不报**退出码（容器内进程可能仍在运行，Docker 不提供中止接口）。

// ---- 容器交互式终端（决策 #358，S 档）----
// 与 VM 串口页**同一形态**（一次性 ticket → WebSocket → 纯文本终端 + 输入行 + 断开），
// 底座是 Docker exec 的 TTY。**断开只关本页桥接**：WS 一关，容器内的 shell 进程**可能仍在运行**
// （Docker 不提供 exec 进程的中止接口——真机对照实验：会话结束后容器内仍有 /bin/sh）。
// 入口是静态写控件（data-write），非 super-user 由 body.role-nonsuper 的 CSS 隐藏。

let ctShellWS = null;
let ctShellName = '';

function ctShellSyncBtn() {
  const b = $('ct-shell-connect');
  if (b) b.disabled = !!ctShellWS;
}

function ctShellAppend(text) {
  const out = $('ct-shell-out');
  const atBottom = out.scrollTop + out.clientHeight >= out.scrollHeight - 8;
  out.textContent += stripANSI(text);
  if (out.textContent.length > 200000) out.textContent = out.textContent.slice(-150000);
  if (atBottom) out.scrollTop = out.scrollHeight;
}

function ctShellMsg(text, isErr) {
  const out = $('ct-shell-out');
  out.textContent += (isErr ? '\n[错误] ' : '\n[信息] ') + text + '\n';
  out.scrollTop = out.scrollHeight;
}

export async function ctShellOpen(name) {
  if (ctShellWS) { ctShellMsg('先断开当前终端（' + ctShellName + '）。', true); return; }
  $('ct-shell').hidden = false;
  $('ct-shell-name').textContent = name;
  $('ct-shell-out').textContent = '';
  ctShellMsg('正在申请终端凭证…', false);
  let res;
  try {
    res = await api('/container-functions/' + encodeURIComponent(name) + '/shell', { method: 'POST' });
  } catch (e) {
    ctShellMsg('申请凭证失败：' + apiErrText(e, '请确认容器处于运行中（先「启动」）'), true);
    return;
  }
  const wsURL = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + res.ws_url;
  try {
    ctShellWS = new WebSocket(wsURL);
  } catch (e) {
    ctShellWS = null;
    ctShellMsg('打开 WebSocket 失败：' + e.message, true);
    ctShellSyncBtn();
    return;
  }
  ctShellName = name;
  ctShellSyncBtn();
  ctShellWS.onopen = () => ctShellMsg('已连接 ' + name + ' 的容器终端（输入命令后回车）。注意：断开只关本页桥接，容器内进程可能仍在运行。', false);
  ctShellWS.onmessage = (ev) => ctShellAppend(ev.data);
  ctShellWS.onclose = () => { ctShellMsg('连接已关闭（本页桥接已断开；容器内进程可能仍在运行——需要时用「执行命令」清理）。', false); ctShellWS = null; ctShellName = ''; ctShellSyncBtn(); };
  ctShellWS.onerror = () => ctShellMsg('WebSocket 出错（凭证过期或容器终端不可用）。', true);
  $('ct-shell-in').focus();
}

export function ctShellClose() {
  if (ctShellWS) { ctShellWS.close(); ctShellWS = null; ctShellName = ''; }
  ctShellSyncBtn();
  const box = $('ct-shell');
  if (box) box.hidden = true;
  const out = $('ct-shell-out');
  if (out) out.textContent = '';
}

export function ctShellSend(text, enter) {
  if (!ctShellWS || ctShellWS.readyState !== 1) { ctShellMsg('尚未连接。', true); return; }
  ctShellWS.send(text + (enter ? '\n' : ''));
}

function ctExecMsg(text, isErr) {
  const p = $('ct-exec-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// ctExecReset 换对象/重新执行前清干净，避免把上一台容器的输出留在页面上。
function ctExecReset() {
  ctExecMsg('', false);
  const out = $('ct-exec-out');
  out.textContent = '';
  out.hidden = true;
}

function ctExecRender(d) {
  const out = $('ct-exec-out');
  const lines = [];
  // 决策 #366：超时不再以 200+timed_out 返回——超时走 504，由 catch 分支渲染服务端错误
  // message（「命令在 N 内未结束…」）；此处只渲染跑完的 200 结果。
  if (d.exit_code !== undefined && d.exit_code !== null) {
    lines.push('退出码: ' + d.exit_code + '（耗时 ' + dash(d.duration_ms) + 'ms）');
  }
  lines.push('--- stdout ---');
  lines.push(d.stdout ? String(d.stdout).replace(/\n+$/, '') : '（无输出）');
  lines.push('--- stderr ---');
  lines.push(d.stderr ? String(d.stderr).replace(/\n+$/, '') : '（无输出）');
  if (d.truncated === true) lines.push('（输出已截断：stdout/stderr 至少一侧超过 256 KiB 上限）');
  out.textContent = lines.join('\n');
  out.hidden = false;
}

export async function ctExecRun() {
  const name = ctDetailName;
  if (!name) return;
  const cmd = ($('ct-exec-cmd').value || '').trim();
  const tmo = Number($('ct-exec-timeout').value || 30);
  if (!cmd) {
    ctExecMsg('请先输入要执行的命令（含空格请整体输入，无需额外引号）。', true);
    return;
  }
  if (!Number.isInteger(tmo) || tmo < 1 || tmo > 300) {
    ctExecMsg('超时须为 1..300 的整数秒。', true);
    return;
  }
  ctExecMsg('执行中…（最长 ' + tmo + ' 秒）', false);
  const out = $('ct-exec-out');
  out.textContent = '';
  out.hidden = true;
  try {
    const d = await api('/container-functions/' + encodeURIComponent(name) + ':exec', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ command: cmd, timeout_seconds: tmo }),
    });
    ctExecMsg('', false);
    ctExecRender(d);
  } catch (e) {
    ctExecMsg('执行失败：' + apiErrText(e, '请确认容器处于运行中（先「启动」）'), true);
  }
}

function ctdMsg(text, isErr) {
  const p = $('ctd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function ctDetailHead(ct) {
  return [
    ['状态', ct.state],
    // 重启次数（决策 #432）：契约字段 `restart_count`（Docker 累计重启数）。**取不到就省略该
    // 字段**——此时如实显示「—」，不把它当 0（0 是「确实没重启过」的读数，两者不是一回事）。
    ['重启次数', ct.restart_count],
    ['镜像', ct.image],
    ['vCPU', ct.vcpu],
    ['内存', ct.memory_mb ? mb(ct.memory_mb) : undefined],
  ];
}

// 环境变量：契约里是字符串字典，按 `K=V` 串起来（空字典显示「—」）。
function envText(env) {
  const keys = Object.keys(env || {});
  return keys.length ? keys.map((k) => k + '=' + env[k]).join('；') : undefined;
}

function ctDetailInfo(ct) {
  const restart = ct.restart_policy === 'on-failure' ? '失败时重启'
    : (ct.restart_policy === 'no' ? '不自动重启' : ct.restart_policy);
  return [
    ['名称', ct.name],
    ['描述', ct.description],
    ['命令', ct.command],
    ['参数', list(ct.args)],
    ['环境变量', envText(ct.env)],
    ['重启策略', restart],
    ['开机自启', ct.autostart === undefined || ct.autostart === null ? undefined : (ct.autostart ? '是' : '否')],
    ['vNIC', (ct.interfaces || []).length],
  ];
}

function ctDetailActions(ct) {
  const box = $('ctd-actions');
  box.textContent = '';
  if (!ct) return;
  VM_ACTIONS.forEach((a) => {
    const btn = wbtn({ type: 'button', text: a.label });
    btn.disabled = a.states.indexOf(String(ct.state)) < 0;
    btn.addEventListener('click', () => ctAction(ct.name, a.key, a.label, ctdMsg));
    box.appendChild(btn);
  });
}

function ctDetailIfaces(ct) {
  const tbody = $('ctd-iface-table').querySelector('tbody');
  tbody.textContent = '';
  const list0 = (ct && ct.interfaces) || [];
  if (!list0.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无 vNIC）' }));
    tbody.appendChild(tr);
    return;
  }
  list0.forEach((i) => {
    const tr = el('tr');
    [i.name, i.type, i.virtual_switch, i.mac, i.vlan, i.state].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    tbody.appendChild(tr);
  });
}

function renderContainerDetail(d, params) {
  const name = (params && params.name) || '';
  const ct = d['/container-functions/{name}'];
  const ok = ct && !ct.__err;
  const changed = ctDetailName !== name;
  ctDetailName = name;
  if (changed) { ctdMsg('', false); ctLogsPoint(name); ctExecReset(); }

  $('ctd-name').textContent = name;
  fill($('ctd-head'), ok ? ctDetailHead(ct) : [['读取失败', ct ? ct.__err : '未取到数据']]);
  fill($('ctd-info'), ok ? ctDetailInfo(ct) : []);
  ctDetailActions(ok ? ct : null);
  ctDetailIfaces(ok ? ct : null);
  ctTabShow(ctTab);
  // 换了对象且正停在日志 Tab：把新对象的日志拉出来（同一对象的轮询重渲染不重复拉）。
  if (changed && ctTab === 'logs') ctLogsOpen(name);
}

// 定位到某容器的日志（进详情页时调用；**不取数**——取数在进 Tab 或点「刷新」时）。
function ctLogsPoint(name) {
  if (ctLogsName === name) return;
  ctLogsName = name;
  $('ct-logs-name').textContent = name;
  $('ct-logs').textContent = '';
}

// 进日志 Tab：定位 + 拉一次。
async function ctLogsOpen(name) {
  ctLogsPoint(name);
  await ctLogsLoad();
}

export async function ctLogsLoad() {
  const pre = $('ct-logs');
  if (!ctLogsName) return;
  pre.textContent = '读取中…';
  try {
    const res = await fetch(API + '/container-functions/' + encodeURIComponent(ctLogsName) + '/logs?tail=200', {
      headers: { Authorization: 'Bearer ' + getToken() },
    });
    if (!res.ok) {
      pre.textContent = '读取失败：' + await httpErrText(res);
      return;
    }
    pre.textContent = (await res.text()) || '（无输出）';
  } catch (e) {
    pre.textContent = '读取失败：' + e.message;
  }
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsContainerDetail = {
  'containerDetail': {
    render(d, params) { pageWarn(d); renderContainerDetail(d, params); },
    // 交互终端是有状态的（WebSocket）：离开这一页就断开，别把连接留在后台（与 VM 串口同纪律）；
    // 同理收掉就地编辑面板（草稿不跨页残留）。
    leave() { ctShellClose(); ctDetailEditorLeave(); }
  },
};
