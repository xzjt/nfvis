// NFViS 控制台启动域（从 app.js 拆出；登录 / 退出 / 角色 / 全局事件接线 / 引导）。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from './core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from './core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, expandEndpoint, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from './core/api.js';
import { fmtTime, fmtClockHint } from './core/format.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError, setCfgEditingProbe } from './core/state.js';

// 重新渲染**当前路由**：SSE 事件到达、轮询、动作完成、点顶栏「刷新」都走这里。
const routerModule = () => import('./router.js');
export async function reload() {
  return (await routerModule()).render();
}
// ---------- 登录 / 退出 ----------

// 登录横幅（决策 #303）：GET /login-banner 未认证可达，登录页每次回到登录视图都取一次
//（设置/清除横幅后无需刷新浏览器）。任何失败都静默：横幅是展示性功能，不能挡住登录。
async function loadLoginBanner() {
  const box = $('login-banner');
  try {
    const res = await fetch(API + '/login-banner');
    if (!res.ok) { box.hidden = true; return; }
    const body = await res.json().catch(() => ({}));
    if (body && typeof body.banner === 'string' && body.banner !== '') {
      box.textContent = body.banner; // textContent 落值：横幅文本不会变成标记
      box.hidden = false;
    } else {
      box.hidden = true; // 未设置时字段省略：不渲染横幅块
    }
  } catch (e) {
    box.hidden = true;
  }
}

function showLogin(msg) {
  $('main-view').hidden = true;
  $('login-view').hidden = false;
  const e = $('login-error');
  e.textContent = msg || '';
  e.hidden = !msg;
  $('password').value = '';
  $('username').focus();
  loadLoginBanner();
}

// applyRole：按账号 class 切角色渲染（决策 #145，设计 §8：`read-only` 只见只读页、写按钮**隐藏**；
// 决策 #324 补齐 operator 粒度：非 super-user 看不到声明为 super-user 的写入口）。
// 机制：给 <body> 挂 role-readonly（隐藏所有 [data-write]）与 role-nonsuper
// （隐藏未带 data-op 的 [data-write]——即服务端按 REST 端点 class 会拒绝的那些）；
// 导航里去掉"写页"（routes.json 里 write: true 的那几条，由 router.js 过滤）。
//
// **隐藏不是安全边界**：服务端按 class 判定（决策 #141 前提⑥），界面只是不把"点下去必被拒"的入口摆出来。
// class 取不到时按"全量"渲染（fail-open）——界面是呈现层，服务端才是边界；这样也不会因为登录响应缺字段
// 把 super-user 自己的入口藏掉。
function applyRole(user) {
  const cls = (user && user.class) || '';
  document.body.classList.toggle('role-readonly', cls === 'read-only');
  // 非 super-user（operator / 自定义 class）藏掉 super-user 级写入口；class 缺失时 fail-open。
  document.body.classList.toggle('role-nonsuper', cls !== '' && cls !== 'super-user');
}

async function enterApp(user) {
  $('login-view').hidden = true;
  $('main-view').hidden = false;
  $('global-error').hidden = true;
  setUser((user && user.name) || '');
  setCurrentClass((user && user.class) || '');
  $('user-line').textContent = user ? user.name + '（' + user.class + '）' : '';
  applyRole(user);
  setCfgEditingProbe(() => cfg.editing);
  cfgSetEditing(false);
  try {
    // 路由表 → 渲染当前 hash 那一页（配置页的数据由 config 视图自己拉）。
    const router = await routerModule();
    await router.loadRoutes();
    await router.start();
  } catch (e) {
    // 拿不到路由表时如实说明，别谎报"登录失效"把人踢回登录页。
    showGlobalError('页面加载失败：' + e.message);
  }
  startStream();
}

export function signOut(msg) {
  stopStream();
  stopPolling();
  // 串口是有状态的（WebSocket）：退出登录必须断开，别把连接留在后台。
  vmConsoleClose();
  // 「我的账号」框里可能有刚填的口令：退出登录时连值一起清掉，别留在 DOM 里。
  closeAcct();
  setToken('');
  setEvents([]);
  setUser('');
  setCurrentClass('');
  applyRole(null);
  cfg = { committed: null, editing: false };
  cfgSetEditing(false);
  sessionStorage.removeItem(TOKEN_KEY);
  sessionStorage.removeItem(USER_KEY);
  showLogin(msg);
}

async function doLogin(ev) {
  ev.preventDefault();
  const btn = $('login-btn');
  btn.disabled = true;
  try {
    const res = await fetch(API + '/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: $('username').value, password: $('password').value }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      showLogin(body.message || ('登录失败（HTTP ' + res.status + '）'));
      return;
    }
    setToken(body.token);
    sessionStorage.setItem(TOKEN_KEY, body.token);
    sessionStorage.setItem(USER_KEY, JSON.stringify(body.user || {}));
    await enterApp(body.user);
  } catch (e) {
    showLogin('无法连接服务：' + e.message);
  } finally {
    btn.disabled = false;
  }
}

async function doLogout() {
  // 先尽力吊销服务端 token（失败也不影响本地退出——本地清掉后即无凭据可用）。
  try { await api('/logout', { method: 'POST' }); } catch (e) { /* 忽略 */ }
  signOut('');
}

// ---------- 全局事件接线 ----------


$('login-form').addEventListener('submit', doLogin);
$('logout-btn').addEventListener('click', doLogout);
$('refresh-btn').addEventListener('click', () => reload().catch((e) => showGlobalError(e.message)));
$('cfg-edit-btn').addEventListener('click', cfgStartEdit);
$('cfg-save-btn').addEventListener('click', cfgSave);
$('cfg-check-btn').addEventListener('click', cfgCheck);
$('diag-ping-btn').addEventListener('click', diagPing);
$('diag-log-btn').addEventListener('click', diagLogs);
$('cfg-diff-btn').addEventListener('click', cfgShowDiff);
$('cfg-commit-btn').addEventListener('click', () => cfgCommitAsk(0));
$('cfg-commit-confirmed-btn').addEventListener('click', () => cfgCommitAsk(10));
$('cfg-confirm-btn').addEventListener('click', cfgConfirmPending);
$('cfg-discard-btn').addEventListener('click', cfgDiscardAsk);

// 确认对话框：点遮罩或按 Esc 都算取消（不执行动作）。
$('modal').addEventListener('click', (ev) => { if (ev.target === $('modal')) closeDialog(false); });
// 「我的账号」框（决策 #147）：同一口径——点遮罩或按 Esc 都算取消。
// Esc 先给确认框（它叠在账号框上面）：确认框开着时按 Esc 只收起它，账号框还在。
$('acct').addEventListener('click', (ev) => { if (ev.target === $('acct')) closeAcct(); });
window.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape') return;
  if (!$('modal').hidden) { closeDialog(false); return; }
  if (!$('acct').hidden) closeAcct();
});

// 审计卡：写入不发事件，故给一个显式刷新（否则 SSE 连着时卡片会停在旧内容上）
$('audit-refresh-btn').addEventListener('click', async () => {
  const btn = $('audit-refresh-btn');
  btn.disabled = true;
  try {
    renderAudit(await api('/audit-logs?limit=50'));
    $('audit-note').textContent = '（已刷新 ' + fmtClockHint(new Date()) + '）';
  } catch (e) {
    $('audit-note').textContent = '（刷新失败：' + e.message + '）';
  } finally {
    btn.disabled = false;
  }
});

// 大表（按需拉取；VRF 路由表已移进 VRF 详情页）
$('big-nat-btn').addEventListener('click', bigNat);
$('big-clear-btn').addEventListener('click', () => { bigMsg('', false); bigOut(''); });

// 详情页 Tab 条（页内状态，不进 hash）
for (const b of $('vmd-tabs').querySelectorAll('button')) {
  b.addEventListener('click', () => vmTabShow(b.dataset.tab, true));
}
for (const b of $('ctd-tabs').querySelectorAll('button')) {
  b.addEventListener('click', () => ctTabClick(b.dataset.tab));
}
// 容器内执行命令（决策 #357）：入口按 data-write 门禁，非 super-user 时按钮/输入被 CSS 隐藏。
$('ct-exec-run').addEventListener('click', ctExecRun);
// 容器交互终端（决策 #358）：连接/断开/发送；输入框回车即发送。
$('ct-shell-connect').addEventListener('click', () => ctShellOpen(ctDetailName));
$('ct-shell-close').addEventListener('click', ctShellClose);
$('ct-shell-send').addEventListener('click', () => { const i = $('ct-shell-in'); ctShellSend(i.value, true); i.value = ''; });
$('ct-shell-enter').addEventListener('click', () => ctShellSend('', true));
$('ct-shell-in').addEventListener('keydown', (e) => { if (e.key === 'Enter') { ctShellSend($('ct-shell-in').value, true); $('ct-shell-in').value = ''; } });
$('ct-exec-cmd').addEventListener('keydown', (e) => { if (e.key === 'Enter') ctExecRun(); });

// VRF 详情：路由表按需重拉
$('vrd-routes-btn').addEventListener('click', () => vrfRoutesLoad($('vrd-name').textContent));

// 虚拟交换机详情：MAC 学习表按需拉取（大表不进页面刷新）
$('vsd-mac-btn').addEventListener('click', vsdMacLoad);
$('vsd-mac-clear-btn').addEventListener('click', () => { vsdMacFor = ''; vsdMacClear(''); });

// 虚拟交换机详情：DHCP 租约表按需拉取（运行态；未配置时服务端 409，页面如实转述）
$('vsd-dhcp-btn').addEventListener('click', vsdDhcpLoad);
$('vsd-dhcp-clear-btn').addEventListener('click', () => { vsdDhcpFor = ''; vsdDhcpClear(''); });

// VM 快照
$('vm-snap-refresh').addEventListener('click', () => vmSnapLoad());
$('vm-snap-create').addEventListener('click', vmSnapCreate);

// 镜像导入
$('img-url-btn').addEventListener('click', imgImportURL);
$('img-incoming-btn').addEventListener('click', imgImportIncoming);
$('img-file-btn').addEventListener('click', imgImportFile);

// 容器日志
$('ct-logs-refresh').addEventListener('click', ctLogsLoad);

// 串口 console
$('vm-console-connect').addEventListener('click', () => vmConsoleOpen(vmDetailName));
$('vm-console-close').addEventListener('click', vmConsoleClose);
$('vm-console-send').addEventListener('click', () => {
  const inp = $('vm-console-in');
  vmConsoleSend(inp.value, true);
  inp.value = '';
});
$('vm-console-enter').addEventListener('click', () => vmConsoleSend('', true));
$('vm-console-in').addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter') {
    ev.preventDefault();
    vmConsoleSend($('vm-console-in').value, true);
    $('vm-console-in').value = '';
  }
});

// 抓包动作（低危：只影响抓包缓冲，随时可停）
$('cap-start-btn').addEventListener('click', () => {
  const ifname = $('cap-iface').value.trim();
  if (!ifname) { capMsg('请填写接口名（如 ens192）。', true); return; }
  const count = Number($('cap-count').value) || 0;
  return opsRun('开始抓包', {
    tier: 'low',
    paragraphs: ['在接口 ' + ifname + ' 上开始数据面抓包' + (count ? '（报文数 ' + count + '）' : '') +
      '？抓包会占用少量数据面开销，用完请停止或导出。'],
    cli: 'request capture start interface ' + ifname + (count ? ' count ' + count : ''),
  }, () => api('/capture', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(count ? { interface: ifname, count } : { interface: ifname }),
  })).then(() => capMsg('抓包已开始：' + ifname, false));
});
$('cap-stop-btn').addEventListener('click', () => opsRun('停止抓包（丢弃）', {
  tier: 'low',
  paragraphs: ['停止抓包并丢弃已捕获的报文？不导出文件，缓冲区内容不会留下。'],
  cli: 'request capture stop',
}, async () => {
  await api('/capture', { method: 'DELETE' });
  capMsg('已停止（未导出）。', false);
  return '';
}));
$('cap-export-btn').addEventListener('click', () => opsRun('停止并导出 pcap', {
  tier: 'low',
  paragraphs: ['停止抓包并把缓冲区导出为 pcap 文件？导出后可在本页下载。'],
  cli: 'request capture export',
}, async () => {
  const r = await api('/capture?export=true', { method: 'DELETE' });
  capMsg(r && r.exported ? '已导出：' + r.name + '（' + bytes(r.size_bytes) + '）'
    : (r && r.message) || '未捕获到报文，无文件导出', false);
  return '';
}));

// 运维动作（写操作；按影响面分档：中危逐条列影响面并标红主按钮）
// 立即同步时间：只调整本机时钟，不改配置（同步源取自已提交配置里声明的 NTP 服务器）。
$('ops-ntp-btn').addEventListener('click', () => opsRun('立即同步时间', {
  tier: 'low',
  paragraphs: ['立刻触发一次 NTP 时间同步？只调整本机时钟（可能有一次时间跳变，通常很小），' +
    '不改任何配置；同步源取自已提交配置里声明的 NTP 服务器，没声明时用系统默认时间源。'],
  cli: 'request system ntp sync',
}, async () => {
  const r = await api('/system/ntp:sync', { method: 'POST' });
  // 如实回显服务端给的同步结果（不把"请求成功"当成"时间已同步"）。
  return '结果：' + ((r && r.status) || '已触发') + (r && r.detail ? '\n' + r.detail : '');
}));
$('ops-backup-btn').addEventListener('click', () => opsRun('生成配置备份', {
  tier: 'low',
  paragraphs: ['生成一份配置备份归档？只读操作：只读当前生效配置，不改运行配置；生成后可在本页下载。'],
  cli: 'request system configuration backup',
}, async () => {
  const r = await api('/system/backup', { method: 'POST' });
  return r && r.file ? '备份文件：' + r.file : '已生成。';
}));
$('ops-sshkey-btn').addEventListener('click', () => opsRun('重新生成 SSH host key', {
  tier: 'mid',
  bullets: [
    '本机 SSH host key 会被换掉：新连接会看到新的主机指纹。',
    '客户端需要更新 known_hosts，否则会报主机密钥不一致并拒绝连接。',
    '已经建立的 SSH 会话不受影响；不重启 sshd 之外的服务。',
  ],
  cli: 'request system ssh host-key regenerate',
}, () => api('/system/ssh-host-key:regenerate', { method: 'POST' })));
$('ops-techsupport-btn').addEventListener('click', () => opsRun('生成 tech-support 归档', {
  tier: 'low',
  paragraphs: ['生成诊断归档？只读操作：收集配置、日志与状态，不改运行配置；生成后可在本页下载。'],
  cli: 'request system tech-support generate',
}, () => api('/system/tech-support', { method: 'POST' })));
$('ops-coredumps-btn').addEventListener('click', () => opsRun('列出 core dump', {
  tier: 'low',
  paragraphs: ['列出本机 core dump 文件清单？只读操作，不改任何状态。'],
  cli: 'show system core-dumps',
}, async () => {
  const rows = await api('/system/core-dumps');
  if (!rows || !rows.length) return '（无 core dump）';
  return rows.map((r) => r.file + '  ' + r.process + '  ' + bytes(r.size_bytes) + '  ' + fmtTime(r.occurred_at)).join('\n');
}));
$('ops-alarms-btn').addEventListener('click', () => opsRun('清除已恢复告警', {
  tier: 'low',
  paragraphs: ['清除已恢复（resolved）的告警记录？活动中的告警不受影响，清除后不可恢复（只是丢掉历史记录）。'],
  cli: 'request alarms clear all',
}, () => api('/alarms:clear', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' })));
$('ops-export-btn').addEventListener('click', () => {
  const url = $('ops-export-url').value.trim();
  if (!url) { opsMsg('请填写导出目标地址（http/https）。', true); return; }
  return opsRun('导出 core dump 清单', {
    tier: 'low',
    paragraphs: ['把 core dump 清单（JSON）POST 到下面这个地址？' + url +
      '——本机的配置与状态不变，但这意味着这份清单会离开本机，请确认地址可信。'],
    cli: 'request system core-dumps export ' + url,
  }, () => api('/system/core-dumps:export', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ url }),
  }));
});
$('ops-vpprestart-btn').addEventListener('click', () => opsRun('重启数据面（VPP）', {
  tier: 'mid',
  bullets: [
    '所有经数据面的流量会中断数秒（虚拟机之间、虚拟机对外）。',
    '虚拟机与容器的数据口（vhost-user 口、成员口）按当前生效的配置重建。',
    '配置里没声明过的物理口不会回到数据面——把业务口交给数据面请先在配置里声明。',
    '配置改动里标着「需重启数据面」的部分，会随这次重启生效。',
    '可回退：配置改坏了就回滚配置并再重启一次数据面。',
  ],
  cli: 'request vpp restart',
}, () => api('/vpp/restart', { method: 'POST' })));
$('ops-reboot-btn').addEventListener('click', () => opsRun('重启主机', {
  tier: 'mid',
  bullets: [
    '整台主机重启：所有虚拟机与容器随之停止，业务全部中断。',
    '管理面（本页面与命令行连接）会断开数分钟，重启期间无法操作，请等主机起来后重新登录。',
    '除配了开机自启的虚拟机外，虚拟机不会自己回来，需要在重启后逐台启动。',
    '可回退：没有「取消重启」——本机重启完成后就是新的运行态（配置仍在）。',
  ],
  cli: 'request system reboot',
}, () => api('/system:reboot', { method: 'POST' })));
$('ops-shutdown-btn').addEventListener('click', () => opsRun('关机', {
  tier: 'mid',
  bullets: [
    '整台主机关机：所有虚拟机与容器随之停止，业务全部中断。',
    '管理面断开后本机不会自己起来，需要带外（控制台/IPMI 之类）或按电源键开机。',
    '关机前请确认还有别的路径能把这台机器开起来。',
    '可回退：开机后配置与数据都在，不丢配置。',
  ],
  cli: 'request system shutdown',
}, () => api('/system:shutdown', { method: 'POST' })));


// 运维动作：软件版本 / 恢复配置 / 恢复出厂（三个高危入口，处理器见上面的 swAdd/swRollback/
// restoreRun/zeroizeRun；按钮本身不做事，值都从控件现取）。
$('ops-sw-add-btn').addEventListener('click', () => swAdd($('ops-sw-pkg').value, $('ops-sw-sha').value));
$('ops-sw-rollback-btn').addEventListener('click', () => swRollback());
$('ops-restore-refresh').addEventListener('click', () => reload().catch((e) => opsMsg('刷新失败：' + e.message, true)));
$('ops-restore-btn').addEventListener('click', () => restoreRun($('ops-restore-file').value));
$('ops-zeroize-btn').addEventListener('click', () => zeroizeRun());
$('ops-formatdata-btn').addEventListener('click', () => formatDataRun());

// 接口详情页：驱动接管（DPDK 绑定/解绑）与 SR-IOV VF 数量。
$('ifd-dpdk-bind').addEventListener('click', () => dpdkAct(true));
$('ifd-dpdk-unbind').addEventListener('click', () => dpdkAct(false));
$('ifd-sriov-btn').addEventListener('click', sriovSet);

// 内核基线：写入 / 回退（都需重启生效，故按中危档确认）。
$('knl-apply-btn').addEventListener('click', () => knlAct('apply'));
$('knl-rollback-btn').addEventListener('click', () => knlAct('rollback'));
$('hp-reclaim-btn').addEventListener('click', () => hpReclaim());

// 用户与权限（写操作全是高危档；口令控件只在提交那一刻读值，不留在别处）。
$('usr-create-btn').addEventListener('click', () => {
  const n = $('usr-new-name').value;
  const cls = $('usr-new-class').value;
  const pw = $('usr-new-pw').value;
  return usrCreate(n, cls, pw).then(() => { $('usr-new-pw').value = ''; });
});
$('usr-mypw-btn').addEventListener('click', () => {
  const oldPw = $('usr-my-old').value;
  const newPw = $('usr-my-new').value;
  return usrMyPassword(oldPw, newPw).then(() => { $('usr-my-old').value = ''; $('usr-my-new').value = ''; });
});

// 登录横幅卡（决策 #303）：低危档确认，写模式与用户管理同一套（取锁直提入审计）。
$('usr-banner-save-btn').addEventListener('click', usrBannerSave);
$('usr-banner-clear-btn').addEventListener('click', usrBannerClear);

// 顶栏「我的账号」（决策 #147）：**所有角色**都看得见（不带 data-write），
// 走的是同一条处理器 usrMyPassword，提示落在这个框里（用户页那条仍落在 #usr-msg）。
$('account-btn').addEventListener('click', openAcct);
$('acct-close').addEventListener('click', closeAcct);
$('acct-submit').addEventListener('click', () => {
  const oldPw = $('acct-old').value;
  const newPw = $('acct-new').value;
  return usrMyPassword(oldPw, newPw, acctMsg).then(() => {
    $('acct-old').value = '';
    $('acct-new').value = '';
  });
});

// 证书：安装外部证书（高危）与重签自签证书（中危）。
$('tls-install-btn').addEventListener('click', () => tlsInstall($('tls-cert').value, $('tls-key').value));
$('tls-regen-btn').addEventListener('click', tlsRegenerate);

// 网络对象详情页：删除策略 / 删除镜像会话（都只写候选，提交后才生效）。
$('qsd-del-btn').addEventListener('click', qsdDelete);
$('spd-del-btn').addEventListener('click', spdDelete);

// 诊断卡新增：traceroute 与接口计数清零
$('diag-trace-btn').addEventListener('click', async () => {
  const host = $('diag-host').value.trim();
  const out = $('diag-out');
  out.hidden = false;
  if (!host) { out.textContent = '请填写目标地址。'; return; }
  out.textContent = '执行中…';
  try {
    const res = await fetch(API + '/diagnostics/traceroute', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + getToken(), 'Content-Type': 'application/json' },
      body: JSON.stringify(pingBody(host, 0, false)),
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
});
$('diag-clear-btn').addEventListener('click', () => {
  const name = $('diag-clear-if').value.trim();
  return opsRun('清零接口统计', {
    tier: 'low',
    paragraphs: [(name ? '清零接口 ' + name + ' 的收发计数？' : '清零「所有接口」的收发计数？') +
      '只影响统计数字（业务流量不受影响），清零后计数从 0 重新开始。'],
    cli: 'clear interfaces statistics' + (name ? ' ' + name : ''),
  }, () => api('/interfaces:clear-statistics', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(name ? { name } : {}),
  }));
});

// 关页面/刷新时收尾：实时通道、轮询、以及串口 WebSocket（详情页的串口连着就要断开）。
window.addEventListener('beforeunload', () => { stopStream(); stopPolling(); vmConsoleClose(); });

// 动作处理器与浏览器控制台用得上：刷新当前页 / 跳到某条路由（如 #/ops/audit）。
window.nfvis = {
  reload,
  navigate: async (path) => (await routerModule()).navigate(path),
  // 串口状态（浏览器验收要断言"离开详情页后 WebSocket 已关"，这里给一个可读的判据）。
  consoleOpen: () => !!termWS,
};

export async function boot() {
  if (!token) { showLogin(''); return; }
  let user = null;
  try { user = JSON.parse(sessionStorage.getItem(USER_KEY) || 'null'); } catch (e) { user = null; }
  try {
    await enterApp(user);
  } catch (e) {
    signOut('登录状态已失效，请重新登录。');
  }
}

