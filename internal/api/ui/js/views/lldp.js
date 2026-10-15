// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// LLDP 域：开关状态与邻居表。

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

// ---------- LLDP（#/network/lldp）----------

// 只读页：全局开关状态 + **实际在发广告的口** + 邻居表（运行态）。
// **开关本身是配置**（改它要写候选配置并提交），故本页不提供开关按钮，只如实显示当前状态，
// 并指到「配置」页的表单——避免界面上出现"改了运行态、配置里却没有"的双份事实。
//
// 决策 #451：全局 `enable true` 的作用集是全部已声明物理口，故接口表列的是**实际在发广告的口**
// （服务端 advertising；内核数据面下以运行态为准，并附每口的收敛状态/未收敛原因）——不再只列
// 逐口覆盖条目（那会漏掉「全局开、逐口没声明」的口）。
function renderLldp(cfg, neighbors) {
  const ok = cfg && !cfg.__err;
  $('lldp-note').textContent = ok ? '' : '（读取失败：' + (cfg ? cfg.__err : '未取到数据') + '）';
  const adv = ok && Array.isArray(cfg.advertising) ? cfg.advertising : [];
  const rt = ok && cfg.runtime ? cfg.runtime : null;
  // 内核数据面下以运行态为准（含起失败的口，converged=false）；VPP 侧用配置派生值。
  const rtBy = {};
  let names = adv;
  if (rt && Array.isArray(rt.interfaces) && rt.interfaces.length) {
    names = rt.interfaces.map((r) => { rtBy[r.name] = r; return r.name; });
  }
  const advertising = names.filter((n) => !rtBy[n] || rtBy[n].converged !== false);
  fill($('lldp-list'), ok ? [
    ['全局开关', cfg.enabled === true ? '已启用' : (cfg.enabled === false ? '已关闭' : '未声明（按关闭处理）')],
    ['通告间隔', cfg.advertisement_interval != null ? cfg.advertisement_interval + ' 秒' : '未声明（用默认值）'],
    ['在发广告的口', advertising.length ? advertising.join('、') : '（无）'],
  ] : []);
  table($('lldp-iface-table').querySelector('tbody'), 2, names.map((name) => {
    const r = rtBy[name];
    let state = '启用';
    if (r && r.converged === false) state = '未收敛' + (r.error ? '：' + r.error : '');
    else if (r && r.mac) state = '启用（' + r.mac + '）';
    return [name, state];
  }));
  // 邻居字段名以契约声明为准（local_interface）；服务端现在发的就是这个名字，
  // 仍留 interface 作兜底（连到更早的守护进程时这一列不该整列变空）——取不到就是「—」，不猜。
  table($('lldp-nbr-table').querySelector('tbody'), 4, rowsOf(neighbors).map((n) => [
    n.local_interface || n.interface, n.chassis_id, n.port_id, n.ttl,
  ]));
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsLldp = {
  'lldp': {
    render(d) { pageWarn(d); renderLldp(d['/protocols/lldp'], d['/protocols/lldp/neighbors']); }
  },
};
