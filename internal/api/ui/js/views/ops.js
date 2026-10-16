// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 运维域：诊断 / 大表 / 审计 / 抓包 / 运维动作 / 软件版本 / 恢复 / zeroize / format-data。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';
// 就地编辑（配置页去中心化第三刀·系统族）：面板用 renderSection 画配置页那一份 VPP 节定义
// （CFG_VPP_SECTIONS，单一真源），写路径走 core/forms.js 的同一套 candidate 事务。
import { CFG_VPP_SECTIONS } from './config.js';
import { objEditor } from '../core/forms.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsopsRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsopsRouterModule()).render();
}

// ---------- 诊断（日志 + 连通性测试） ----------
//
// 与 CLI 同源同口径：ping 只覆盖数据面（VPP），**未通即失败**（0 发包/0 应答都算失败，
// 原始回显照原样展示以便排查）；日志取服务端尾部（与 show log system 同源）。
// 日志不随页面轮询刷新（按需点按钮），避免无谓的重复拉取。

// pingBody 组请求体：源地址留空则不传（服务端按目标自动选路）；IPv6 勾选则 ipv6=true
// （决策 #330：ping 走 v6 平面、traceroute 走宿主侧 ICMPv6，与 CLI `ping/traceroute ipv6` 同源）。
// withVrf：仅 ping 带 vrf（决策 #402）——traceroute 的 vrf 维持不支持（REST 明文拒绝），故不传。
export function pingBody(host, count, withVrf) {
  const body = { host };
  if (count) body.count = count;
  const src = $('diag-source').value.trim();
  if (src) body.source = src;
  if (withVrf) {
    const vrf = $('diag-vrf').value.trim();
    if (vrf) body.vrf = vrf; // 目标在 VRF 内（VNF/交换机域）时必填，否则服务端 no egress interface
  }
  const v6 = $('diag-ipv6');
  if (v6 && v6.checked) body.ipv6 = true;
  return body;
}

// 诊断页说明按**当前数据面**（装配事实）切换：VPP 数据面下 ping 只覆盖数据面（管理口在
// 内核平面，要到宿主机上 ping）；内核数据面下 ping 由宿主网络栈发出（管理口可直接测）。
// 数据面取不到时保留静态文案（VPP 口径，也是缺省数据面）。
function renderDiagPlaneNote(st) {
  const note = $('diag-plane-note');
  if (!note) return;
  const kernel = !!(st && st.dataplane === 'kernel');
  note.textContent = kernel
    ? '内核数据面下 ping 由宿主网络栈发出（可测管理口与 VRF 内目标）。目标在 VRF 内时（如 VNF/交换机域）须在上方填该 VRF 名，否则会报 no egress interface（与 CLI ping … vrf 同源）。traceroute 走宿主侧 ICMP/ICMPv6，可测管理口；其 vrf 参数不支持（要经 VRF 测请用 ping vrf）。勾选 IPv6 后 ping 走 v6 平面、traceroute 走宿主侧 ICMPv6。'
    : 'ping 只覆盖数据面（VPP）——管理口在内核平面，请在宿主机上 ping。目标在 VRF 内时（如 VNF/交换机域）须在上方填该 VRF 名，否则会报 no egress interface（与 CLI ping … vrf 同源）。traceroute 走宿主侧 ICMP/ICMPv6，可测管理口；其 vrf 参数不支持（要经 VRF 测请用 ping vrf）。勾选 IPv6 后 ping 走 v6 平面、traceroute 走宿主侧 ICMPv6。';
}

export async function diagPing() {
  const host = $('diag-host').value.trim();
  const out = $('diag-out');
  out.hidden = false;
  if (!host) {
    out.textContent = '请填写目标地址。';
    return;
  }
  const count = Number($('diag-count').value) || 0;
  out.textContent = '执行中…';
  try {
    const res = await fetch(API + '/diagnostics/ping', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + getToken(), 'Content-Type': 'application/json' },
      body: JSON.stringify(pingBody(host, count, true)),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      const detail = (body.detail || []).map((d) => d.message).join('\n');
      out.textContent = '失败：' + (body.message || ('HTTP ' + res.status)) + (detail ? '\n' + detail : '');
      return;
    }
    out.textContent = body.output || '（无输出）';
  } catch (e) {
    out.textContent = '失败：' + e.message;
  }
}

export async function diagLogs() {
  const pre = $('diag-log');
  pre.hidden = false;
  pre.textContent = '读取中…';
  try {
    const res = await fetch(API + '/system/logs?last=100', { headers: { Authorization: 'Bearer ' + getToken() } });
    const text = await res.text();
    pre.textContent = res.ok ? (text || '（日志为空）') : '读取失败：' + errTextOf(text, res.status);
  } catch (e) {
    pre.textContent = '读取失败：' + e.message;
  }
}


// ---------- 大表（NAT 会话）：按需拉取 ----------

export function bigMsg(text, isErr) {
  const p = $('big-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

export function bigOut(text) {
  const pre = $('big-out');
  pre.hidden = !text;
  pre.textContent = text || '';
}

export async function bigNat() {
  bigMsg('读取 NAT 会话…', false);
  bigOut('');
  try {
    const rows = await api('/nat/sessions');
    const list = Array.isArray(rows) ? rows : [];
    if (!list.length) { bigMsg('无 NAT 会话。', false); return; }
    const lines = list.map((r) => dash(r.inside_ip) + ':' + dash(r.inside_port) +
      '  ->  ' + dash(r.outside_ip) + ':' + dash(r.outside_port));
    bigOut('inside -> outside' + '\n' + lines.join('\n'));
    bigMsg('NAT 会话 ' + list.length + ' 条。', false);
  } catch (e) {
    bigMsg('读取失败：' + e.message, true);
  }
}

// ---------- 审计日志 ----------

export function renderAudit(audit) {
  const rows = (audit && !audit.__err && Array.isArray(audit.items)) ? audit.items : (Array.isArray(audit) ? audit : []);
  table($('audit-table').querySelector('tbody'), 5, rows.slice(0, 50).map((a) => [
    fmtTime(a.ts || a.time || a.timestamp), a.user, a.action || a.event,
    a.detail || a.message, a.result || a.outcome,
  ]));
  $('audit-note').textContent = audit && audit.__err ? '（读取失败：' + audit.__err + '）'
    : (rows.length ? '（最近 ' + Math.min(rows.length, 50) + ' 条）' : '');
}

// ---------- 抓包（数据面无关：VPP pcap trace / 内核 tcpdump，端点同源 /capture）----------

export function capMsg(text, isErr) {
  const p = $('cap-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function renderCapture(cap) {
  const active = cap && !cap.__err ? cap.active : null;
  const files = (cap && !cap.__err && cap.files) || [];
  $('cap-note').textContent = cap && cap.__err ? '（读取失败：' + cap.__err + '）' : '';
  // 决策 #465：会话有两种状态——「进行中」与「已停止、数据待导出」（stop 之后、export 之前）。
  // 后者别显示成「无进行中的抓包」，否则操作者会以为数据已经没了。
  // 决策 #466：重启收敛重建的会话点明来源（操作者多半没在这个进程里停过它）。
  $('cap-active').textContent = !active
    ? '（无进行中的抓包）'
    : (active.stopped
      ? '已停止，数据待导出' + (active.recovered ? '（重启前会话已恢复）' : '') + '：接口 ' + active.interface +
        '，已抓 ' + dash(active.captured) + ' 包，深度 ' +
        dash(active.max_depth) + '，开始于 ' + fmtTime(active.started_at) + '——取走 pcap：用「停止并导出」'
      : '进行中：接口 ' + active.interface + '，已抓 ' + dash(active.captured) + ' 包，深度 ' +
        dash(active.max_depth) + '，开始于 ' + fmtTime(active.started_at));
  $('cap-active').className = 'muted small';

  const tbody = $('cap-table').querySelector('tbody');
  tbody.textContent = '';
  if (!files.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '4', class: 'muted', text: '（无已导出 pcap）' }));
    tbody.appendChild(tr);
    return;
  }
  files.forEach((f) => {
    const tr = el('tr');
    [f.name, bytes(f.size_bytes), fmtTime(f.created_at)].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const btn = wbtn({ type: 'button', class: 'ghost small', text: '下载' });
    btn.addEventListener('click', () => downloadFile('/capture/' + encodeURIComponent(f.name), f.name, capMsg));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// downloadFile 带 Authorization 取文件再触发浏览器下载（<a href> 带不了请求头）。
export async function downloadFile(path, name, note) {
  if (note) note('下载 ' + name + '：准备中…', false);
  try {
    const res = await fetch(API + path, { headers: { Authorization: 'Bearer ' + getToken() } });
    if (!res.ok) throw new Error(await httpErrText(res));
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = el('a', { href: url, download: name });
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    if (note) note('已触发下载：' + name + '（' + bytes(blob.size) + '）', false);
  } catch (e) {
    if (note) note('下载失败：' + e.message, true);
  }
}

// 归档（配置备份 / 诊断归档）：列表 + 下载
function renderArchives(backups, techs) {
  archiveTable($('ops-backup-table').querySelector('tbody'), backups, '/system/backup/');
  archiveTable($('ops-tech-table').querySelector('tbody'), techs, '/system/tech-support/');
  // 恢复用的备份选择框取自同一份列表（不再第二次请求）：列表刷新时选项跟着刷新，
  // 但**保留当前选中项**（免得操作者选好文件后被一次刷新清掉选择）。
  const sel = $('ops-restore-file');
  const keep = sel.value;
  sel.textContent = '';
  const rows = (backups && !backups.__err && Array.isArray(backups)) ? backups : [];
  if (!rows.length) {
    sel.appendChild(el('option', { value: '', text: '（本机没有配置备份——先点「生成配置备份」）' }));
    return;
  }
  rows.forEach((f) => {
    const name = f.file || f.name;
    sel.appendChild(el('option', { value: name, text: name + '（' + fmtTime(f.created_at || f.created) + '）' }));
  });
  if (keep) sel.value = keep;
}

function archiveTable(tbody, rows, prefix) {
  tbody.textContent = '';
  const list = (rows && !rows.__err && Array.isArray(rows)) ? rows : [];
  if (!list.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '4', class: 'muted', text: rows && rows.__err ? '读取失败：' + rows.__err : '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  list.forEach((f) => {
    const name = f.file || f.name;
    const tr = el('tr');
    [name, bytes(f.size_bytes), fmtTime(f.created_at || f.created)].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const btn = wbtn({ type: 'button', class: 'ghost small', text: '下载' });
    btn.addEventListener('click', () => downloadFile(prefix + encodeURIComponent(name), name, opsMsg));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// ---------- core dump 清单（列表 + 逐条删除）----------
//
// 与 CLI 同源：列表取自 GET /system/core-dumps（`show system core-dumps`），逐条删除走
// DELETE /system/core-dumps?file=<name>（`request system core-dumps delete file <name>`；端点缺省
// file 是"全部"，故逐条删除必须显式带上文件名）。该 DELETE 端点在 server.go 里经 cfgAPI 注册
// （super-user 级），故行内删除入口经 wbtn 打 data-write——非 super 角色看不到（决策 #327）。
export function cdRender(rows) {
  const tbody = $('ops-coredump-table').querySelector('tbody');
  tbody.textContent = '';
  const list = Array.isArray(rows) ? rows : [];
  if (!list.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '5', class: 'muted', text: '（无 core dump）' }));
    tbody.appendChild(tr);
    return;
  }
  list.forEach((r) => {
    const tr = el('tr');
    [r.file, r.process, r.size_bytes ? bytes(r.size_bytes) : undefined, fmtTime(r.occurred_at)].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const btn = wbtn({ type: 'button', class: 'danger small', text: '删除' });
    btn.addEventListener('click', () => cdDelete(r.file));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 删除单个 core dump：中危（删本机文件、不可恢复）。失败理由（如文件已不在）原样展示，不吞。
async function cdDelete(file) {
  const ok = await uiConfirm('删除 core dump', {
    tier: 'mid',
    paragraphs: ['删除本机 core dump 文件 ' + file + '。'],
    bullets: [
      '该转储文件从本机移除，不可恢复（要留档请先用「导出清单」或从本机取走文件）。',
      '只删这一个文件，其他 core dump 与运行态不受影响。',
    ],
    cli: 'request system core-dumps delete file ' + file,
  });
  if (!ok) return;
  opsMsg('删除 ' + file + '：执行中…', false);
  try {
    await api('/system/core-dumps?file=' + encodeURIComponent(file), { method: 'DELETE' });
    opsMsg('已删除 ' + file + '。', false);
  } catch (e) {
    opsMsg('删除失败：' + e.message, true);
  }
  // 重新列一遍（删成功/失败都刷；失败时表格保持上一次的内容，理由已在上面的提示里如实给出）。
  try {
    cdRender(await api('/system/core-dumps'));
  } catch (e) {
    opsMsg('读取 core dump 清单失败：' + e.message, true);
  }
}

// ---------- 运维动作（写操作，均二次确认）----------

export function opsMsg(text, isErr) {
  const p = $('ops-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function opsOut(text) {
  const pre = $('ops-out');
  pre.hidden = !text;
  pre.textContent = text || '';
}

// opsRun 统一的"确认 → 调用 → 回显"流程（写操作一律先确认，与 CLI 的 --yes 同口径）。
// `ask` 是这次动作的档位与影响面（tier / paragraphs / bullets / cli / confirmLabel），
// 交 uiDialog 渲染——低危一句话，中危逐条列影响面并标红，高危再加确认词与倒计时。
export async function opsRun(label, ask, fn) {
  if (!(await uiConfirm(label, ask))) return;
  opsMsg(label + '：执行中…', false);
  opsOut('');
  try {
    const out = await fn();
    opsMsg(label + '：完成。', false);
    if (out) opsOut(typeof out === 'string' ? out : JSON.stringify(out, null, 2));
  } catch (e) {
    opsMsg(label + ' 失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 运维动作：软件版本（高危档）----------
//
// 两条都是**高危**：软件升级会安装 deb 并在维护脚本里重启管理服务（本页面掉线、需要重新登录），
// 回退同样是替换整机软件版本。确认词用固定词（这两个动作没有"对象名"可打）：upgrade / rollback。
// 界面不做任何"代替确认"的捷径——不自动重试、不预判成功，只如实显示服务端的结果或报错。
export async function swAdd(pkg, sha) {
  const p = (pkg || '').trim();
  if (!p) { opsMsg('请先填软件包路径或下载地址。', true); return; }
  const s = (sha || '').trim();
  const ok = await uiConfirm('安装升级包（软件升级）', {
    tier: 'high',
    requireWord: 'upgrade',
    bullets: [
      '安装 ' + p + '：校验包名（必须是 nfvis 包）与版本 → 归档到本机（供回退）→ 安装。',
      '安装过程会重启管理服务：本页面会掉线，需要重新登录；期间命令行与接口都不可用。',
      '虚拟机与数据面进程不随这次重启停止；数据面配置里标着「需重启数据面」的部分仍要另行重启数据面。',
      '安装失败时按服务端提示处理（可点「回退到上一版本」，或命令行执行软件回退）。',
      '可回退：本页的「回退到上一版本」（用的是本机归档里安装过的版本）。',
    ],
    cli: 'request system software add ' + p + (s ? ' sha256 ' + s : ''),
  });
  if (!ok) return;
  opsMsg('安装 ' + p + '：执行中（安装期间管理面会短暂中断）…', false);
  opsOut('');
  try {
    const r = await api('/system/software', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(s ? { package: p, sha256: s } : { package: p }),
    });
    opsMsg('安装已受理：' + dash(r && r.previous) + ' → ' + dash(r && r.version) +
      '（安装过程会重启管理服务，本页面稍后需要重新登录）。', false);
    if (r) opsOut(JSON.stringify(r, null, 2));
  } catch (e) {
    opsMsg('安装失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

export async function swRollback() {
  const ok = await uiConfirm('回退软件版本', {
    tier: 'high',
    requireWord: 'rollback',
    bullets: [
      '把整机软件版本回退到本机归档里安装过的上一个版本（没有归档时服务端明确拒绝）。',
      '回退同样会重启管理服务：本页面会掉线，需要重新登录。',
      '回退不改配置：配置库与数据都在，回退后仍按当前生效配置运行。',
      '可回退：再装一次要用的版本（本页「安装升级包」）。',
    ],
    cli: 'request system software rollback',
  });
  if (!ok) return;
  opsMsg('回退软件版本：执行中（回退期间管理面会短暂中断）…', false);
  opsOut('');
  try {
    const r = await api('/system/software:rollback', { method: 'POST' });
    opsMsg('回退已受理：' + dash(r && r.previous) + ' → ' + dash(r && r.version) + '。', false);
    if (r) opsOut(JSON.stringify(r, null, 2));
  } catch (e) {
    opsMsg('回退失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 运维动作：恢复配置（高危档）----------
//
// 从**本机已有的备份归档**恢复（列表见「归档下载」）：界面不弹文件选择器，而是把选中的归档
// 从服务端取回来、再作为 multipart 上传（契约收的就是 multipart file）——这样真人与自动化
// 都走得通，也不会出现"文件在操作者本机、服务端要的是上传件"的错配。
// 确认词取**归档文件名**（对象名）：恢复覆盖的是当前生效配置，打一遍文件名最有效的拦阻。
export async function restoreRun(name) {
  const file = (name || '').trim();
  if (!file) { opsMsg('请先选择要恢复的备份归档（没有备份就先点「生成配置备份」）。', true); return; }
  const ok = await uiConfirm('从备份归档恢复配置', {
    tier: 'high',
    requireWord: file,
    bullets: [
      '用归档 ' + file + ' 里的配置**覆盖当前生效配置**（导入为候选后立即提交），现有生效配置被替换。',
      '归档里不含镜像文件本体：配置引用了镜像时，镜像需要另行导入（服务端会在结果里说明归档内的镜像条目数）。',
      '本机已有候选编辑（本会话或别的会话在「配置」页编辑中）会让这次恢复被拒绝（编辑锁被占用）——界面如实显示该拒绝。',
      '配置里标着「需重启生效」的部分，要重启数据面/主机后才完全生效。',
      '可回退：恢复前先生成一份当前配置的备份，要退回来就再用它恢复一次。',
    ],
    cli: 'request system configuration restore <备份归档文件>',
  });
  if (!ok) return;
  opsMsg('读取归档 ' + file + '…', false);
  opsOut('');
  try {
    const res = await fetch(API + '/system/backup/' + encodeURIComponent(file), {
      headers: { Authorization: 'Bearer ' + getToken() },
    });
    if (!res.ok) throw new Error('读取归档失败（' + await httpErrText(res) + '）');
    const blob = await res.blob();
    const fd = new FormData();
    fd.append('file', blob, file);
    opsMsg('恢复中…', false);
    const r = await api('/system/restore', { method: 'POST', body: fd });
    opsMsg('已从 ' + file + ' 恢复：生效配置 revision ' + dash(r && r.revision) +
      (r && r.images_in_archive ? '；归档含 ' + r.images_in_archive + ' 个镜像条目（文件本体不在归档内）' : ''), false);
    if (r) opsOut(JSON.stringify(r, null, 2));
  } catch (e) {
    opsMsg('恢复失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 运维动作：恢复出厂（高危档，不可撤销）----------
//
// 契约要求请求体 confirm=true（服务端的双重确认）；界面这一层是确认词 zeroize + 10 秒倒计时。
// 两者都不是"多余的仪式"：这一步会清空数据分区的配置与镜像、级联删除虚拟机与容器、账号复位，
// 且**本机不会自动重启**——重启后进入初始化状态，需要带外/控制台重新初始化。没有撤销入口。
export async function zeroizeRun() {
  const ok = await uiConfirm('恢复出厂（清空配置与镜像）', {
    tier: 'high',
    requireWord: 'zeroize',
    bullets: [
      '清空数据分区的配置与镜像，级联删除虚拟机与容器，账号随空配置复位（本机没有「撤销恢复出厂」）。',
      '**本机不会自动重启**：重启后进入初始化状态，届时需要带外/控制台重新初始化，本页与命令行都帮不上忙。',
      '正在运行的虚拟机与容器会被级联删除（业务中断）；镜像文件从本机仓库移除，需要重新导入。',
      '重启前先确认你还有带外通道，并已把需要的配置备份取到本机之外。',
      '不可撤销：这一步之后本机不再保留任何原有配置与账号。',
    ],
    cli: 'request system zeroize',
  });
  if (!ok) return;
  opsMsg('恢复出厂：执行中…', false);
  opsOut('');
  try {
    const r = await api('/system:zeroize', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true }),
    });
    opsMsg('已恢复出厂：配置/镜像/VNF 已清空、账号复位（revision ' + dash(r && r.revision) +
      '，移除镜像 ' + dash(r && r.removed_images) + ' 个）。**本机不会自动重启**——重启后进入初始化状态。', false);
    if (r) opsOut(JSON.stringify(r, null, 2));
  } catch (e) {
    opsMsg('恢复出厂失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// ---------- 运维动作：重置数据分区（高危档，不可撤销）----------
//
// 与「恢复出厂」是**两件事**：这是「恢复出厂**数据状态**、保留管理面可达」——删全部受管 VNF/容器与
// 网络配置对象、清受管数据目录（镜像/备份/抓包/转储/诊断归档），但**管理口/API/登录用户**与
// **物理口/DPDK 声明**原样保留，执行完仍能登录继续配置、不必重启。契约要求请求体 confirm=true；
// 界面这一层是确认词 format-data + 10 秒倒计时。
export async function formatDataRun() {
  const ok = await uiConfirm('重置数据分区（恢复出厂数据状态）', {
    tier: 'high',
    requireWord: 'format-data',
    bullets: [
      '删除全部受管虚拟机与容器（含其快照；业务中断），以及全部网络配置对象（交换机/VRF/静态路由/ACL/NAT/QoS/端口镜像/LAG/LLDP）。',
      '清空受管数据目录：镜像、备份归档、抓包文件、core dump、诊断归档、虚拟机磁盘数据（不可撤销，需要的先取到本机之外）。',
      '**保留管理面配置**：管理口、API（端口/TLS/token TTL）、登录用户与权限类；物理口声明与 DPDK 声明也保留——执行完仍可登录继续配置，不必重启。',
      '配置库重置为「保留节」最小配置；结果会如实给出删除对象数、清理文件数与释放空间。',
      '不可撤销：本机没有「撤销重置数据分区」；要退回来只能用先前的配置备份恢复（镜像需另行导入）。',
    ],
    cli: 'request system storage format-data',
  });
  if (!ok) return;
  opsMsg('重置数据分区：执行中…', false);
  opsOut('');
  try {
    const r = await api('/system:format-data', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true }),
    });
    if (r && r.already_factory) {
      opsMsg('已是出厂态：无受管对象/数据可清（修订 ' + dash(r.revision) + '）。', false);
    } else {
      opsMsg('已重置数据分区：删除对象 ' + fmtDataCount(r && r.removed_objects) +
        ' 个、清理文件 ' + dash(r && r.purged_files) + ' 个、释放 ' + fmtBytes(r && r.freed_bytes) +
        '（修订 ' + dash(r && r.revision) + '）。管理面配置与物理口/DPDK 声明已保留。', false);
    }
    if (r) opsOut(JSON.stringify(r, null, 2));
  } catch (e) {
    opsMsg('重置数据分区失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// fmtDataCount 累加 removed_objects 各类计数（服务端按类别给，界面只要总数）。
function fmtDataCount(o) {
  if (!o) return '—';
  return Object.keys(o).reduce((a, k) => a + (Number(o[k]) || 0), 0);
}

// ---------- 数据面（VPP）配置：就地编辑（配置页去中心化第三刀·系统族）----------
//
// 面板画配置页那一份「VPP（数据面）」节（CFG_VPP_SECTIONS，含 CPU / 内存 / DPDK 全局参数 /
// 单网卡覆盖 / 插件开关子表——单一真源，不复制字段清单），写路径走 core/forms.js 的同一套
// candidate 事务（取锁 → 写候选 → 校验 → 提交 → 释放）。
//
// 生效口径以**服务端提交期的实际警告**为准（engine.go：vpp 段 StartupKey 子集变更 ⇒
// 「警告: vpp 变更需 request vpp restart（或整机 reboot）后生效」；只改数据面 DNS 代理上游
// 不在其列——决策 #400）。本页下方就有「重启数据面（VPP）」按钮，界面不另立第二套说法。
function opsVppKind() {
  const all = Array.isArray(CFG_VPP_SECTIONS) ? CFG_VPP_SECTIONS : [];
  return {
    label: '数据面（VPP）配置',
    section: all.find((s) => s && s.legend === 'VPP（数据面）') || null,
    cliSave: () => 'set vpp …（cpu / memory / dpdk / plugins）',
    note: () => 'CPU 主核与 worker 核须取自「资源池」的隔离核池，大页偏好须与资源池的页大小一致' +
      '（服务端在提交时校验）；CPU / 内存 / DPDK / 插件类改动提交后服务端会给' +
      '「需 request vpp restart（或整机 reboot）后生效」提示——**提交不等于生效**，重启数据面后新参数才被采用' +
      '（重启会中断全部数据面转发）。单网卡覆盖要求该口已在「配置」里声明。' +
      '只改数据面 DNS 代理上游不需要重启（那项在「系统」页的就地编辑面板里）。',
    bullets: () => [
      '写入即提交（revision 加一），但 CPU / 内存 / DPDK / 插件类改动**要重启数据面才生效**（服务端提交期会给该提示）。',
      '重启数据面会中断全部转发路径（虚拟机 / 容器数据口一并断开）——入口在本页「电源」一组里。',
      '主核 / worker 核必须落在资源池的隔离核池内；大页偏好须与资源池声明的页尺寸一致，否则提交期拒绝。',
    ],
  };
}
const opsVppEditor = objEditor({ slot: 'ops-vpp-edit', reload });
if (opsVppEditor) {
  const b = $('ops-vpp-edit-btn');
  if (b) b.addEventListener('click', () => opsVppEditor.openEdit(opsVppKind(), ''));
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsOps = {
  'ops': {
    render(d) { pageWarn(d); renderArchives(rowsOf(d['/system/backup']), rowsOf(d['/system/tech-support'])); },
    // 离开本页（router 的 leave 钩子）：收掉就地编辑面板——草稿不跨页残留。
    leave() { if (opsVppEditor) opsVppEditor.leave(); }
  },
  'audit': {
    render(d) { pageWarn(d); renderAudit(d['/audit-logs?limit=50']); }
  },
  'diagnostics': {
    // 诊断页是动作面板：ping / traceroute / 清零 / 看服务端日志都按需执行（点按钮才拉），
    // 进页面不自动拉日志——大段文本不该跟着页面刷新反复下载。
    // 页面说明按**当前数据面**（/system/status.dataplane，装配事实）切换：内核数据面下
    // ping 走宿主网络栈，不能沿用「只覆盖数据面（VPP）」的说法。
    render(d) { renderDiagPlaneNote(d['/system/status']); }
  },
  'capture': {
    render(d) { pageWarn(d); renderCapture(d['/capture']); }
  },
};
