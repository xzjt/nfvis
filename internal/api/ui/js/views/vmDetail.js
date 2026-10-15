// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 虚拟机详情域：详情页 / 快照 / 串口 console（含 VNC）。

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
const jsviewsvmDetailRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsvmDetailRouterModule()).render();
}

// ---------- VM 详情页（#/compute/vms/:name）----------
//
// 一页一对象：对象头 + 四个 Tab（概览 / 接口 / 快照 / 串口）。Tab 是**页内状态**，不进 hash
// （对象级深链已够用；Tab 进 URL 会把地址拖长、也让"这个链接分享出去看到什么"变模糊）。
// 快照与串口**复用既有实现**（vmSnapLoad/vmSnapCreate/vmSnapAct、vmConsoleOpen/vmConsoleSend/
// vmConsoleClose），只是容器从列表页的行内面板搬到这里的 Tab 面板——一套代码，行为不会分叉。

const VM_TABS = ['overview', 'ifaces', 'snapshots', 'console'];
let vmTab = 'overview';
let vmDetailName = '';

// 切 Tab：只动面板的 hidden 与 Tab 条的选中态（不碰 hash，也不重新取数）。
// `focusBtn` 只在**用户点 Tab** 时为真——重渲染（轮询/事件）里抢焦点会把光标从正在输入的地方挪走。
function vmTabShow(tab, focusBtn) {
  if (VM_TABS.indexOf(tab) < 0) tab = 'overview';
  vmTab = tab;
  VM_TABS.forEach((t) => { $('vm-tab-' + t).hidden = t !== tab; });
  const bar = $('vmd-tabs');
  for (const b of bar.querySelectorAll('button')) {
    if (b.dataset.tab === tab) b.setAttribute('aria-selected', 'true');
    else b.removeAttribute('aria-selected');
  }
  if (focusBtn && tab === 'console' && !termWS) $('vm-console-connect').focus();
}

function vmDetailMsg(text, isErr) {
  const p = $('vmd-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

function vmDetailHead(vm) {
  return [
    ['状态', vm.state],
    ['镜像', vm.image],
    ['vCPU', vm.vcpu ? vm.vcpu.count : undefined],
    ['内存', vm.memory ? mb(vm.memory.size_mb) : undefined],
  ];
}

function vmDetailInfo(vm) {
  const vcpu = vm.vcpu || {};
  const mem = vm.memory || {};
  const disks = vm.disks || [];
  return [
    ['名称', vm.name],
    ['描述', vm.description],
    ['vCPU 绑定', vcpu.pin === undefined || vcpu.pin === null ? undefined : (vcpu.pin ? '是' : '否')],
    ['绑定核', list(vcpu.cores_assigned)],
    ['内存后端', mem.backing === 'normal' ? '普通内存' : (mem.backing ? '大页' : undefined)],
    ['大页大小', mem.hugepage_size],
    ['NUMA 节点', mem.numa_node],
    ['附加磁盘', disks.length ? disks.map((d) => (d.name || '?') +
      (d.size_gb ? '（' + d.size_gb + ' GB）' : '') +
      (d.image ? '（来自 ' + d.image + '）' : '')).join('；') : undefined],
    ['vNIC', (vm.interfaces || []).length],
    ['直通 PCI 设备', (vm.pci_devices || []).length ? vm.pci_devices.join('；') : undefined],
    ['串口', vm.serial_console === false ? '未启用' : '已启用'],
    ['开机自启', vm.autostart === undefined || vm.autostart === null ? undefined : (vm.autostart ? '是' : '否')],
  ];
}

// 概览 Tab 的生命周期按钮：与列表页同一份 VM_ACTIONS 判定，回显走本页的提示区。
function vmDetailActions(vm) {
  const box = $('vmd-actions');
  box.textContent = '';
  if (!vm) return;
  VM_ACTIONS.forEach((a) => {
    const btn = wbtn({ type: 'button', text: a.label });
    btn.disabled = a.states.indexOf(String(vm.state)) < 0;
    btn.addEventListener('click', () => vmAction(vm.name, a.key, a.label, vmDetailMsg));
    box.appendChild(btn);
  });
}

// 接口 Tab：该 VM 的 vNIC（类型/虚拟交换机/MAC/VLAN/状态 + 逐口包计数）。
function vmDetailIfaces(vm) {
  const tbody = $('vmd-iface-table').querySelector('tbody');
  tbody.textContent = '';
  const list0 = (vm && vm.interfaces) || [];
  const stats = (vm && vm.statistics) || [];
  if (!list0.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '8', class: 'muted', text: '（无 vNIC）' }));
    tbody.appendChild(tr);
    return;
  }
  list0.forEach((i) => {
    const s = stats.find((x) => x.vnic === i.name);
    const live = s && s.available;
    const tr = el('tr');
    [i.name, i.type, i.virtual_switch, i.mac, i.vlan, i.state,
      live ? s.rx_packets : undefined, live ? s.tx_packets : undefined].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    tbody.appendChild(tr);
  });
}

function renderVMDetail(d, params) {
  const name = (params && params.name) || '';
  const vm = d['/virtual-machine-functions/{name}'];
  const snaps = d['/virtual-machine-functions/{name}/snapshots'];
  const ok = vm && !vm.__err;
  // 换了对象才清本页提示——同一对象的轮询重渲染要留住动作回显（"已受理"刚写完就被抹掉
  // 就等于没回显）。
  const changed = vmDetailName !== name;
  vmDetailName = name;
  if (changed) vmDetailMsg('', false);

  $('vmd-name').textContent = name;
  fill($('vmd-head'), ok ? vmDetailHead(vm) : [['读取失败', vm ? vm.__err : '未取到数据']]);
  fill($('vmd-info'), ok ? vmDetailInfo(vm) : []);
  vmDetailActions(ok ? vm : null);
  vmDetailIfaces(ok ? vm : null);
  // 快照：名字变了才清输入框与提示（轮询重渲染不该把正在输入的快照名抹掉）；
  // 列表数据用路由表预取的这一份，不重复请求（点「刷新」才重新拉）。
  vmSnapOpen(name, snaps);
  renderVMStats([{ name }], { [name]: ok ? vm.statistics : null }, 'vmd-stat');
  // 串口 Tab 的连接按钮：未启用串口且非显式 VNC 控制台的 VM 不给连
  // （服务端也会拒绝，这里只是别让人白点）；显式设 console=vnc 时该 VM 走内置 VNC 查看器，照样可连。
  const vncConsole = !!(vm && vm.startup && vm.startup.console === 'vnc');
  termNoSerial = !!(vm && vm.serial_console === false && !vncConsole);
  vmConsoleSyncBtn();
  vmTabShow(vmTab); // 重渲染后保持当前 Tab（默认概览）
}

// ---------- VM 快照（列表 / 创建 / 删除 / 回滚；create+rollback 需关机态）----------
//
// 容器在 VM 详情页的「快照」Tab 里（原来挂在列表页的行内面板上）；取数与动作逻辑没变。

let snapVM = '';

function snapMsg(text, isErr) {
  const p = $('vm-snap-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// 定位到某台 VM 的快照：进详情页时调用。`pre` 是路由表预取的快照列表（有就直接渲染，
// 不再打一次请求）；不传则自己拉（「刷新」按钮、创建/删除/回滚之后）。
// 只有**换了一台 VM** 才清输入框与提示——轮询重渲染不该抹掉正在输入的快照名。
function vmSnapOpen(name, pre) {
  const changed = snapVM !== name;
  snapVM = name;
  if (changed) { $('vm-snap-new').value = ''; snapMsg('', false); }
  return vmSnapLoad(pre);
}

async function vmSnapLoad(pre) {
  const tbody = $('vm-snap-table').querySelector('tbody');
  tbody.textContent = '';
  if (!snapVM) return;
  let rows = pre;
  if (rows === undefined) {
    try {
      rows = await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) + '/snapshots');
    } catch (e) {
      snapMsg('读取快照失败：' + e.message, true);
      return;
    }
  }
  if (rows && rows.__err) {
    snapMsg('读取快照失败：' + rows.__err, true);
    return;
  }
  const list = Array.isArray(rows) ? rows : (rows && rows.snapshots) || [];
  if (!list.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '3', class: 'muted', text: '（无快照）' }));
    tbody.appendChild(tr);
    return;
  }
  list.forEach((s) => {
    const nm = s.name || s.snapshot;
    const tr = el('tr');
    [nm, fmtTime(s.created_at || s.created || s.creation_time)].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const rb = wbtn({ type: 'button', class: 'ghost small', text: '回滚' });
    rb.addEventListener('click', () => vmSnapAct(nm, 'rollback'));
    const db = wbtn({ type: 'button', class: 'danger small', text: '删除' });
    db.addEventListener('click', () => vmSnapAct(nm, 'delete'));
    cell.appendChild(rb);
    cell.appendChild(db);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

async function vmSnapAct(snap, kind) {
  const isRollback = kind === 'rollback';
  // 档位：回滚磁盘内容 = 中危（列影响面 + 标红）；删快照 = 低危（只丢这一份快照）。
  const ok = isRollback
    ? await uiConfirm('回滚虚拟机到快照', {
      tier: 'mid',
      paragraphs: ['把虚拟机 ' + snapVM + ' 的磁盘内容回滚到快照 ' + snap + '。'],
      bullets: [
        '虚拟机必须处于关机态；运行中回滚会被服务端拒绝（运行中回滚会替换 QEMU 进程）。',
        '磁盘内容会被替换成拍这份快照那一刻的内容：之后写入的数据不在磁盘上。',
        '回滚不改配置，只换磁盘内容；要留后路就先对当前状态建一份快照（同样需关机态）。',
        '回滚不可撤销：回滚后要回到当前状态，只能靠另一份快照。',
      ],
      cli: 'request virtual-machine-functions ' + snapVM + ' snapshot rollback name ' + snap,
    })
    : await uiConfirm('删除快照', {
      tier: 'low',
      paragraphs: ['删除虚拟机 ' + snapVM + ' 的快照 ' + snap + '？这份快照将不可恢复' +
        '（虚拟机的当前状态不受影响）。'],
      cli: 'request virtual-machine-functions ' + snapVM + ' snapshot delete name ' + snap,
    });
  if (!ok) return;
  snapMsg((isRollback ? '回滚' : '删除') + '中…', false);
  try {
    if (isRollback) {
      await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) +
        '/snapshots/' + encodeURIComponent(snap) + ':rollback', { method: 'POST' });
    } else {
      await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) +
        '/snapshots/' + encodeURIComponent(snap), { method: 'DELETE' });
    }
    snapMsg((isRollback ? '回滚' : '删除') + '已完成。', false);
  } catch (e) {
    // 关机态约束等拒绝理由原样展示（不吞错误）
    snapMsg((isRollback ? '回滚' : '删除') + '失败：' + e.message, true);
  }
  await vmSnapLoad();
}

async function vmSnapCreate() {
  const nm = $('vm-snap-new').value.trim();
  if (!nm) { snapMsg('请填写快照名。', true); return; }
  const ok = await uiConfirm('创建快照', {
    tier: 'low',
    paragraphs: ['为虚拟机 ' + snapVM + ' 创建快照 ' + nm + '？虚拟机必须处于关机态' +
      '（运行中创建会被服务端拒绝），快照会占用磁盘空间。'],
    cli: 'request virtual-machine-functions ' + snapVM + ' snapshot create name ' + nm,
  });
  if (!ok) return;
  snapMsg('创建中…', false);
  try {
    await api('/virtual-machine-functions/' + encodeURIComponent(snapVM) + '/snapshots', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: nm }),
    });
    snapMsg('快照已创建。', false);
    $('vm-snap-new').value = '';
  } catch (e) {
    snapMsg('创建失败：' + e.message, true);
  }
  await vmSnapLoad();
}

// ---------- 串口 console（一次性 ticket → WebSocket）----------
//
// 容器在 VM 详情页的「串口」Tab 里（原来挂在列表页的行内面板上）。同一时刻只连一台 VM
// （避免误操作）；离开详情页时由路由调 vmConsoleClose 断开，WebSocket 不会留在后台。

// 终端状态：WebSocket 与当前 VM 名（同一时刻只连一台，避免误操作）。
let termWS = null;
let termVM = '';
// 当前会话是否为 VNC 图形查看器（决策 #485）：断开路径与按钮状态随之分派。
let termVNC = false;
// 关闭 VNC 时抑制 disconnect 回调里的提示（是我们主动关的，不是异常断开）。
let vncClosing = false;
// 当前 VM 是否未启用串口（模型里 serial_console=false）：连接按钮随之禁用——
// 与列表页原来"未启用就不显示串口按钮"同一口径，只是这里说得出原因。
let termNoSerial = false;

// 连接按钮跟着真实状态走（连着 VNC/串口、或该 VM 没启用串口且非显式 VNC，都禁用）。
function vmConsoleSyncBtn() {
  const b = $('vm-console-connect');
  const vncOpen = !!(window.nfvisVnc && window.nfvisVnc.isOpen());
  if (b) b.disabled = !!termWS || vncOpen || termNoSerial;
}

// 串口输出含 ANSI 转义（颜色/光标），去掉后按纯文本渲染（不引入终端模拟器）。
function stripANSI(s) {
  return s.replace(/\[[0-9;?]*[ -/]*[@-~]/g, '').replace(/[\]\][^]*/g, '');
}

function termAppend(text) {
  const out = $('vm-console-out');
  const atBottom = out.scrollTop + out.clientHeight >= out.scrollHeight - 8;
  out.textContent += stripANSI(text);
  if (out.textContent.length > 200000) out.textContent = out.textContent.slice(-150000);
  if (atBottom) out.scrollTop = out.scrollHeight;
}

function termMsg(text, isErr) {
  const out = $('vm-console-out');
  out.textContent += (isErr ? '\n[错误] ' : '\n[信息] ') + text + '\n';
  out.scrollTop = out.scrollHeight;
}

// VNC 视图内的可见提示：VNC 视图会隐藏 #vm-console-out（见 vmConsoleShowVNC），故 VNC 的
// 连接/错误/断开提示必须写进这块**可见**元素——否则真实连接失败或断开只会让用户看到黑屏、
// 毫无解释。追加式保留本次会话内的提示历史。
function vncNotice(text, isErr) {
  const n = $('vm-vnc-notice');
  if (!n) return;
  n.hidden = false;
  n.classList.toggle('err', !!isErr);
  n.textContent += (isErr ? '[错误] ' : '[信息] ') + text + '\n';
}

// 清空并隐藏 VNC 提示条（切换视图/关闭 console 时调用）。
function vmConsoleHideVNCNotice() {
  const n = $('vm-vnc-notice');
  if (n) { n.textContent = ''; n.hidden = true; n.classList.remove('err'); }
}

async function vmConsoleOpen(name) {
  if (termWS || (window.nfvisVnc && window.nfvisVnc.isOpen())) {
    termMsg('先断开当前 console（' + termVM + '）。', true); return;
  }
  $('vm-console').hidden = false;
  $('vm-console-name').textContent = name;
  $('vm-console-out').textContent = '';
  termMsg('正在申请 console 凭证…', false);
  let res;
  try {
    res = await api('/virtual-machine-functions/' + encodeURIComponent(name) + '/console', { method: 'POST' });
  } catch (e) {
    termMsg('申请凭证失败：' + e.message, true);
    return;
  }
  // 控制台形态分派（决策 #485）：vnc 是图形控制台，服务端在 /console 端点不签发串口 ticket——
  // 文本串口管线不适用，改走 Web 内置 noVNC 查看器（另换一次性 ticket，见下面 vmConsoleOpenVNC）。
  if (res && res.kind === 'vnc') {
    await vmConsoleOpenVNC(name);
    return;
  }
  // 文本串口（serial/telnet）：切回文本显示（清掉可能留着的 VNC 视图）。
  termVNC = false;
  vmConsoleShowText();
  const wsURL = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + res.ws_url;
  try {
    termWS = new WebSocket(wsURL);
  } catch (e) {
    termWS = null;
    termMsg('打开 WebSocket 失败：' + e.message, true);
    vmConsoleSyncBtn();
    return;
  }
  termVM = name;
  vmConsoleSyncBtn();
  termWS.onopen = () => termMsg('已连接 ' + name + ' 的串口（回车可让 guest 重绘提示符）。', false);
  termWS.onmessage = (ev) => termAppend(ev.data);
  termWS.onclose = () => { termMsg('连接已关闭。', false); termWS = null; termVM = ''; vmConsoleSyncBtn(); };
  termWS.onerror = () => termMsg('WebSocket 出错（凭证过期或串口不可用）。', true);
  $('vm-console-in').focus();
}

// vmConsoleOpenVNC 打开内置 noVNC 查看器：向 /console/vnc 换一次性 ticket，再让 vncview.js
// （window.nfvisVnc）把 WS 桥接到该 VM 的 VNC 服务（127.0.0.1:<port>，域 XML 里的实际端口）。
async function vmConsoleOpenVNC(name) {
  if (!window.nfvisVnc) {
    termMsg('内置 VNC 查看器脚本未加载（novnc/vncview.js 未就绪）。', true);
    return;
  }
  let ticket;
  try {
    ticket = await api('/virtual-machine-functions/' + encodeURIComponent(name) + '/console/vnc', { method: 'POST' });
  } catch (e) {
    termMsg('申请 VNC 凭证失败：' + e.message, true);
    return;
  }
  if (!ticket || !ticket.ws_url) {
    termMsg('服务端未返回 VNC 连接地址（该 VM 的控制台形态可能不是 VNC）。', true);
    return;
  }
  termVNC = true;
  termVM = name;
  vmConsoleShowVNC();
  vmConsoleSyncBtn();
  const wsURL = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + ticket.ws_url;
  window.nfvisVnc.open(wsURL, $('vm-vnc-view'), {
    onConnect: () => vncNotice('已连接 ' + name + ' 的 VNC 图形控制台（鼠标/键盘直接作用于 guest）。', false),
    onError: (e) => vncNotice('VNC 连接失败：' + (e && e.message ? e.message : e), true),
    onDisconnect: (d) => {
      const clean = !!(d && d.clean);
      termVNC = false; termVM = '';
      vmConsoleSyncBtn();
      if (vncClosing) { vncClosing = false; return; }
      vncNotice(clean ? 'VNC 连接已关闭。'
        : 'VNC 连接已断开（凭证过期、VM 未运行或 VNC 服务不可达）。', !clean);
    },
  });
}

// 文本串口显示：显示输出与输入行，隐藏 VNC 视图；同步标题文案。
function vmConsoleShowText() {
  const out = $('vm-console-out'); if (out) out.hidden = false;
  const io = $('vm-console-io'); if (io) io.hidden = false;
  const v = $('vm-vnc-view'); if (v) v.hidden = true;
  vmConsoleHideVNCNotice();
  vmConsoleTitle('串口 console ');
}

// VNC 图形显示：隐藏文本输出与输入行，显示 VNC 视图；同步标题文案。
function vmConsoleShowVNC() {
  const out = $('vm-console-out'); if (out) out.hidden = true;
  const io = $('vm-console-io'); if (io) io.hidden = true;
  const v = $('vm-vnc-view'); if (v) v.hidden = false;
  vmConsoleHideVNCNotice();
  vmConsoleTitle('VNC 图形控制台 ');
}

// 改 `#vm-console` 内 <h3> 的首个文本节点（保留其后的名字 span 与断开按钮）。
function vmConsoleTitle(text) {
  const box = $('vm-console');
  if (!box) return;
  const h3 = box.querySelector('h3');
  if (h3 && h3.firstChild && h3.firstChild.nodeType === 3) {
    h3.firstChild.nodeValue = text;
  }
}

function vmConsoleClose() {
  if (termWS) { termWS.close(); termWS = null; termVM = ''; }
  if (window.nfvisVnc && window.nfvisVnc.isOpen()) {
    vncClosing = true;
    window.nfvisVnc.close();
  }
  termVNC = false;
  vmConsoleSyncBtn();
  const box = $('vm-console');
  if (box) box.hidden = true;
  const out = $('vm-console-out');
  if (out) out.textContent = '';
  vmConsoleHideVNCNotice();
}

function vmConsoleSend(text, enter) {
  if (!termWS || termWS.readyState !== 1) { termMsg('尚未连接。', true); return; }
  termWS.send(text + (enter ? '\r' : ''));
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsVmDetail = {
  'vmDetail': {
    render(d, params) {
      pageWarn(d);
      renderVMDetail(d, params);
    },
    // 串口是有状态的（WebSocket + 一次性 ticket）：离开这一页就断开，别把连接留在后台。
    leave() { vmConsoleClose(); }
  },
};
