// 从 app.js 拆出（Web 控制台重构 2026-10）；运维域：实时监视（#/ops/monitor）。
//
// 命令行 `monitor vnf` 是**真跟踪**（持续刷新，不是一次快照，决策 #92）。Web 的等价形态＝
// 实时通道（/events SSE）推送触发本页重渲染（js/core/state.js 的 scheduleReload：事件到达即
// 重渲染**当前路由**）+ 路由声明的 poll 兜底轮询（实时通道断开时才启用，见 core/state.js 的
// armPolling）。两条都是既有机制——本页只做只读呈现，**不新造**第二套刷新通道。
//
// 只读页：无任何写控件（故不需 wbtn / data-write 门禁），任何角色都能看。

import { $, el, dash, rowClickable } from '../core/dom.js';
import { rowsOf, pageWarn } from '../core/api.js';
import { mb, fmtClockHint } from '../core/format.js';

// 一张状态表：行可点进对应详情页（与其它列表页同一交互），状态缺失如实显示「—」。
function renderMonitorTable(tbodyID, noteID, rows, pathPrefix, cells) {
  const tbody = $(tbodyID).querySelector('tbody');
  tbody.textContent = '';
  $(noteID).textContent = rows.length ? '（' + rows.length + '）' : '';
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '5', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((o) => {
    const tr = rowClickable(el('tr'), pathPrefix + encodeURIComponent(o.name));
    cells(o).forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    tbody.appendChild(tr);
  });
}

function renderMonitor(d) {
  pageWarn(d);
  renderMonitorTable('mon-vm-table', 'mon-vm-note', rowsOf(d['/virtual-machine-functions']),
    '#/compute/vms/', (v) => [v.name, v.state, v.vcpu ? v.vcpu.count : undefined,
      v.memory ? mb(v.memory.size_mb) : undefined, v.image]);
  renderMonitorTable('mon-ct-table', 'mon-ct-note', rowsOf(d['/container-functions']),
    '#/compute/containers/', (c) => [c.name, c.state, c.vcpu,
      c.memory_mb ? mb(c.memory_mb) : undefined, c.image]);
  // 更新时刻是**纯 UI 提示**（说明这张卡刚被刷过），与任何记录无关，故用本地时间的唯一出口。
  $('mon-note').textContent = '（更新于 ' + fmtClockHint(new Date()) + '）';
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsMonitor = {
  'monitor': {
    render(d) { renderMonitor(d); }
  },
};
