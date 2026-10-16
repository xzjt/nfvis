// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 配置域：candidate → 提交全流程 / 提交历史 / 编辑锁会话。
// 配置页去中心化第四刀（2026-10）：配置页不再渲染节表单，只留原始 JSON 编辑 + 提交链；
// 节定义（CFG_*_SECTIONS）仍在本文件导出，供各对象页 / 系统页的就地编辑面板 import。

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
// 表单引擎（节驱动的渲染 + candidate 事务 + 口令闸门）在 core/forms.js：
// 本节只留「配置页」自己的编排（取数、编辑会话、提交/回滚/历史）与节定义（供对象页复用）。
import { cfgPwGate, cfgPwResetAll, cfgText, cfgWriteText, cfgMsg,
  candidateAcquire, candidateWrite, candidateCheck, candidateCommit, candidateRelease,
  renderSections } from '../core/forms.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsconfigRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsconfigRouterModule()).render();
}

// ---------- 配置（candidate → 提交） ----------
//
// 写路径与 CLI 同一套语义：PUT /configuration/candidate 取锁并建立 candidate →
// 可选查看差异（GET /configuration/diff）→ 提交（POST /configuration/commit；
// 管理口地址/网关变更会返回 CONFIRM_REQUIRED，须走 commit confirmed）→
// DELETE candidate 结束会话（**提交后也释放**，避免把会话锁留给下一次 CLI 登录）。
//
// 编辑态以原始 JSON 文本为准，提交前都会先保存。

let cfg = {
// （跨模块访问经下方访问器：boot.js 的编辑态探针与登录复位走这里，不直接 import let。）
  committed: null, // {configuration, revision}
  editing: false,
  // 由「提交历史」页取候选后落回本页时记下偏移（> 0 时本页载入候选并直接展示差异）。
  landedFromRollback: 0,
};

// 口令派生闸门（cfgPwGate / cfgPwResetAll）、消息区（cfgMsg）与原始 JSON 文本读写
// （cfgText / cfgWriteText）已抽到 core/forms.js——本节不再定义它们，只从那里取用。
// 系统段的字段全部由下面的 CFG_SYS_SECTIONS 声明（与网络域/计算域同一套表单引擎，见
// core/forms.js 与「各节定义」）。此处只留事务级的公共函数。

export function cfgSetEditing(on) {
  cfg.editing = on;
  $('cfg-read').hidden = on;
  $('cfg-edit').hidden = !on;
  $('cfg-diff').hidden = true;
  $('cfg-commit-confirmed-btn').hidden = true;
  $('cfg-confirm-btn').hidden = true;
  if (!on) cfgPwResetAll(); // 离开编辑态：口令控件的登记一并作废
  cfgMsg('', false);
}

function cfgCandidate() {
  return cfg.committed ? JSON.parse(JSON.stringify(cfg.committed.configuration || {})) : {};
}

// 核列表解析/格式化（隔离核在 JSON 里是数组，表单里写成 "2-5,7" 更好用）。
function parseCores(text) {
  const out = [];
  (text || '').split(',').forEach((part) => {
    const t = part.trim();
    if (!t) return;
    const m = t.match(/^(\d+)\s*-\s*(\d+)$/);
    if (m) {
      for (let i = Number(m[1]); i <= Number(m[2]); i++) out.push(i);
    } else if (/^\d+$/.test(t)) {
      out.push(Number(t));
    }
  });
  return out;
}

function formatCores(list) {
  const nums = (list || []).slice().sort((a, b) => a - b);
  const parts = [];
  for (let i = 0; i < nums.length; ) {
    let j = i;
    while (j + 1 < nums.length && nums[j + 1] === nums[j] + 1) j++;
    parts.push(j > i ? nums[i] + '-' + nums[j] : String(nums[i]));
    i = j + 1;
  }
  return parts.join(',');
}

// 节表单渲染（顺序即界面顺序）：系统段（基础 / API 与 TLS / 管理口 / 内核启动参数 / 日志 /
// 健康阈值 / 登录与口令策略）→ 接口 → 资源池 → VPP 段 → 网络域与计算域各节。
// 系统段、VPP 段与网络域/计算域**共用同一套节定义与写路径**（见 CFG_SYS_SECTIONS /
// CFG_VPP_SECTIONS / CFG_SECTIONS）；接口与资源池是更早的两节，走下面的 addField。
// 全部都是"改字段 → 写回 JSON 文本 → 保存到 candidate"，提交才生效。
//
// 配置页去中心化第四刀后**配置页不再调用本函数**（节表单已从页面撤下，字段都在各自对象页 /
// 系统页的就地编辑面板里改）；函数体自拆分以来逐字未动，保留的唯一原因是
// contrib/scripts/web-console-config-selftest.js 的桩沿 bundle 顶层符号**直接调用它**来驱动
// 「口令控件 → 保存前闸门」这条被钉住的路径——删掉它该自校准整份报 ReferenceError（桩不在
// 本刀可改范围内）。纯内部助手（addField / cfgApplyIface / cfgApplyIfaceStorm / cfgApplyPools /
// parseCores / formatCores）同样只服务于这条路。
function cfgRenderForms(c) {
  const box = $('cfg-forms');
  box.textContent = '';
  cfgPwResetAll(); // 表单重画：口令控件的登记随之作废
  let seq = 0;

  const addField = (parent, label, value, onchange, kind) => {
    const id = 'cfg-f-' + (seq++);
    parent.appendChild(el('label', { text: label, for: id })); // 关联 label ↔ input（无障碍/可测）
    const inp = el(kind === 'select' ? 'select' : 'input', kind === 'select' ? { id } : { type: kind || 'text', id });
    if (kind === 'select') {
      [['', '（未设置）'], ['true', '启用'], ['false', '禁用']].forEach(([v, t]) => {
        const opt = el('option', { value: v, text: t });
        if (String(value === undefined || value === null ? '' : value) === v) opt.setAttribute('selected', 'selected');
        inp.appendChild(opt);
      });
    } else {
      inp.value = value === undefined || value === null ? '' : value;
    }
    // 监听 input 而非 change：change 只在**用户输入导致的**失焦时触发，程序化赋值
    // （浏览器自动化、脚本回填）不置"值已改"标志、失焦也不发 change；input 两者都覆盖。
    inp.addEventListener('input', onchange);
    inp.addEventListener('change', onchange); // select 用 change
    parent.appendChild(inp);
    return inp;
  };

  // ---- 系统段 ----
  renderSections(box, CFG_SYS_SECTIONS, c);

  // ---- 接口（对每个已声明接口：描述 / MTU / 启用）----
  const ifs = Array.isArray(c.interfaces) ? c.interfaces : [];
  if (ifs.length) {
    const f = el('fieldset', {});
    f.appendChild(el('legend', { text: '接口' }));
    f.appendChild(el('p', { class: 'muted small', text: '入向风暴抑制按目的 MAC 分类限速（单位 **kbps**，超速丢弃，不做 pps 换算；' +
      '两类各自独立，只配一类时另一类不限）。它与「端口安全白名单」**同接口互斥**——两者都绑该口唯一的 ' +
      'L2 入向分类槽，并存会被提交期双向拒绝（端口安全在「接口详情」页维护）。' }));
    ifs.forEach((it) => {
      const sub = el('div', { class: 'cfg-sub' });
      sub.appendChild(el('div', { class: 'cfg-subname', text: it.name || '（未命名）' }));
      addField(sub, '描述', it.description, (e) => cfgApplyIface(it.name, 'description', e.target.value));
      addField(sub, 'MTU', it.mtu, (e) => cfgApplyIface(it.name, 'mtu', e.target.value === '' ? '' : Number(e.target.value)));
      addField(sub, '启用', it.enabled === undefined || it.enabled === null ? '' : String(it.enabled),
        (e) => cfgApplyIface(it.name, 'enabled', e.target.value === '' ? '' : e.target.value === 'true'), 'select');
      addField(sub, '入向限速策略', it.ingress_policy,
        (e) => cfgApplyIface(it.name, 'ingress_policy', e.target.value));
      addField(sub, '出向限速策略', it.egress_policy,
        (e) => cfgApplyIface(it.name, 'egress_policy', e.target.value));
      addField(sub, '风暴抑制 · 广播（kbps）', (it.storm_control || {}).broadcast_kbps,
        (e) => cfgApplyIfaceStorm(it.name, 'broadcast', e.target.value));
      addField(sub, '风暴抑制 · 组播（kbps）', (it.storm_control || {}).multicast_kbps,
        (e) => cfgApplyIfaceStorm(it.name, 'multicast', e.target.value));
      f.appendChild(sub);
    });
    box.appendChild(f);
  }

  // ---- 资源池（隔离核 + 1G/2M 大页数量）----
  const pools = c.resource_pools || {};
  const pf = el('fieldset', {});
  pf.appendChild(el('legend', { text: '资源池' }));
  addField(pf, '隔离核（如 2-5,7）', formatCores((pools.cpu || {}).isolated_cores),
    (e) => cfgApplyPools('isolated_cores', parseCores(e.target.value)));
  ['1G', '2M'].forEach((size) => {
    const hp = (pools.hugepages || []).find((h) => h.page_size === size) || {};
    addField(pf, '大页 ' + size + ' 数量', hp.count,
      (e) => cfgApplyPools('hugepages', { size, count: e.target.value === '' ? '' : Number(e.target.value) }));
  });
  box.appendChild(pf);

  // ---- VPP 段 ----
  renderSections(box, CFG_VPP_SECTIONS, c);

  // ---- 网络域与计算域（虚拟交换机 / VRF 与路由 / ACL / NAT / QoS / 端口镜像 / 聚合 / LLDP /
  //      VXLAN / 虚拟机 / 容器）：节定义见下面的 CFG_SECTIONS，写路径与上面各节完全相同 ----
  renderSections(box, CFG_SECTIONS, c);
}

// 接口字段改写（按名字定位；名字来自当前 candidate 文本）。
function cfgApplyIface(name, key, value) {
  let c;
  try { c = cfgText(); } catch (e) { cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true); return; }
  const it = (c.interfaces || []).find((x) => x.name === name);
  if (!it) return;
  if (value === '') delete it[key]; else it[key] = value;
  cfgWriteText(c);
  cfgMsg('已写入 candidate（未保存）——点「保存到 candidate」提交到服务端。', false);
}

// 入向风暴抑制字段改写（storm_control.broadcast_kbps / multicast_kbps，单位 kbps）。
// 与服务端同口径的本地判据：整数、1-100000000；清空 = 删除该类（两类都空时整段 storm_control 消失）。
// 另拦「与端口安全白名单同接口并存」——两者绑该口唯一的 L2 入向分类槽（服务端双向拒绝），
// 本地先拦下省一轮提交（最终仍以服务端校验为准）。
function cfgApplyIfaceStorm(name, cls, text) {
  let c;
  try { c = cfgText(); } catch (e) { cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true); return; }
  const it = (c.interfaces || []).find((x) => x.name === name);
  if (!it) return;
  const key = cls === 'broadcast' ? 'broadcast_kbps' : 'multicast_kbps';
  const label = cls === 'broadcast' ? '广播抑制' : '组播抑制';
  const t = (text || '').trim();
  if (t !== '') {
    if (!/^\d+$/.test(t)) {
      cfgMsg(label + '：必须是整数（kbps）——未写入 candidate，请修正后重试。', true);
      return;
    }
    const n = Number(t);
    if (n < 1 || n > 100000000) {
      cfgMsg(label + '：' + n + ' 超出范围（须为 1-100000000 kbps）——未写入 candidate，请修正后重试。', true);
      return;
    }
    if (Array.isArray(it.port_security) && it.port_security.length) {
      cfgMsg('接口 ' + name + ' 已配端口安全白名单：它与风暴抑制同接口互斥（两者绑该口唯一的 L2 入向分类槽）——' +
        '请先在「接口详情」页删除白名单，或两者留其一。未写入 candidate。', true);
      return;
    }
  }
  if (t === '') {
    if (it.storm_control && typeof it.storm_control === 'object') {
      delete it.storm_control[key];
      if (!Object.keys(it.storm_control).length) delete it.storm_control;
    }
  } else {
    if (!it.storm_control || typeof it.storm_control !== 'object') it.storm_control = {};
    it.storm_control[key] = Number(t);
  }
  cfgWriteText(c);
  cfgMsg('已写入 candidate（未保存）——点「保存到 candidate」提交到服务端。', false);
}

// 资源池字段改写：isolated_cores 直接替换；hugepages 按页大小定位（不存在则补一条）。
function cfgApplyPools(key, value) {
  let c;
  try { c = cfgText(); } catch (e) { cfgMsg('原始 JSON 语法错误，请先修正：' + e.message, true); return; }
  if (!c.resource_pools) c.resource_pools = {};
  if (key === 'isolated_cores') {
    if (!c.resource_pools.cpu) c.resource_pools.cpu = {};
    if (value.length) c.resource_pools.cpu.isolated_cores = value;
    else delete c.resource_pools.cpu.isolated_cores;
  } else {
    const list = Array.isArray(c.resource_pools.hugepages) ? c.resource_pools.hugepages : (c.resource_pools.hugepages = []);
    const found = list.find((h) => h.page_size === value.size);
    if (value.count === '') {
      c.resource_pools.hugepages = list.filter((h) => h.page_size !== value.size);
      if (!c.resource_pools.hugepages.length) delete c.resource_pools.hugepages;
    } else if (found) {
      found.count = value.count;
    } else {
      list.push({ page_size: value.size, count: value.count });
    }
  }
  cfgWriteText(c);
  cfgMsg('已写入 candidate（未保存）——点「保存到 candidate」提交到服务端。', false);
}

// 表单引擎（节驱动的渲染、字段类型、candidate 写路径）已抽到 core/forms.js：
// cfgRenderForms 只负责把「系统段 / 接口 / 资源池 / VPP 段 / 网络域与计算域」按顺序画进
// 表单容器（renderSections 见 core/forms.js），但配置页瘦身后已不再调用它（见上文说明）。
// 节定义（CFG_*_SECTIONS）仍在本文件导出（页面数据）：各对象页 / 系统页的就地编辑面板
// （network.js / switches.js / vms.js / containers.js / images.js / system.js / ops.js）import 它们渲染。

// —— 各节定义（顺序即界面顺序）——
//
// 每条语句 ↔ 一个控件：`path` 就是模型里的字段路径（与 JSON 里看到的一致）。
// 只有标识字段（名字 / seq / 网段 / 接口名）不进 `fields`——它在标题上，靠「新增 / 删除」维护。
//
// 系统段与 VPP 段（CFG_SYS_SECTIONS / CFG_VPP_SECTIONS）用**同一套**声明：单对象节
// （loc 指向文档里的那个对象）+ 字段 + 子表 + 可选「清空本节」，与下面的网络域/计算域完全一致。

// RFC 5424 facility 取值（与服务端同一份名单）
const CFG_FACILITIES = ['kern', 'user', 'mail', 'daemon', 'auth', 'syslog', 'lpr', 'news', 'uucp',
  'cron', 'authpriv', 'ftp', 'local0', 'local1', 'local2', 'local3', 'local4', 'local5', 'local6', 'local7']
  .map((f) => [f, f]);

// 系统段（命令行 `set system …`）。危险/高影响项（证书路径、管理口地址、用户与口令策略）
// 与普通项一样只写 candidate——提交才生效，界面不提供任何绕过服务端校验与守卫的捷径。
// 导出给系统各页复用（配置页去中心化第三刀）：system.js 把自己的面板用**同一份**节定义
// 渲染（renderSection），不复制字段清单——加字段/改 kind 自动跟随两处。
export const CFG_SYS_SECTIONS = [
  {
    legend: '系统（基础）',
    loc: [{ obj: 'system' }],
    note: '主机名 / 时区 / DNS / NTP / 命令行会话空闲超时。',
    fields: [
      { label: '主机名', path: ['hostname'], kind: 'text' },
      { label: '时区', path: ['timezone'], kind: 'text', hint: '如 Asia/Shanghai' },
      { label: 'DNS 服务器（逗号分隔）', path: ['dns_servers'], kind: 'iplist', hint: '如 8.8.8.8, 1.1.1.1' },
      { label: '会话空闲超时（分钟）', path: ['idle_timeout_minutes'], kind: 'int', min: 1 },
    ],
    sublists: [{
      list: 'ntp', key: 'server', keyKind: 'text', title: 'NTP 服务器', what: 'NTP 服务器',
      keyLabel: '服务器', placeholder: '如 10.0.0.1 或 ntp.example.com',
      note: '服务器地址可写 IP 或主机名；「首选」标记该服务器为首选源。',
      fields: [{ label: '首选', path: ['prefer'], kind: 'bool' }],
    }],
  },
  {
    legend: '系统 · API 与 TLS',
    loc: [{ obj: 'system' }, { obj: 'api' }],
    clearPath: ['system', 'api'],
    note: '控制面 API 的端口、令牌有效期、并发上限与证书/私钥的**文件路径**（证书内容由运维侧放到该路径，' +
      '本页只声明路径，不上传文件）。改完提交才生效。',
    fields: [
      { label: 'HTTPS 端口', path: ['port'], kind: 'int', min: 1, max: 65535 },
      { label: '令牌有效期（分钟）', path: ['token_ttl_minutes'], kind: 'int', min: 1 },
      { label: '并发会话上限', path: ['max_sessions'], kind: 'int', min: 1 },
      { label: '证书文件（PEM 路径）', path: ['cert_file'], kind: 'text', hint: '如 /etc/nfvis/tls/server.crt' },
      { label: '私钥文件（PEM 路径）', path: ['key_file'], kind: 'text', hint: '如 /etc/nfvis/tls/server.key' },
      { label: '使用自签证书', path: ['tls_self_signed'], kind: 'bool',
        note: '声明用自签证书（缺证书时由守护进程生成）。重签自签证书是运维动作，不在这里。' },
    ],
  },
  {
    legend: '系统 · 管理口',
    loc: [{ obj: 'system' }, { obj: 'management' }],
    clearPath: ['system', 'management'],
    note: '管理网卡与它的静态地址/网关。管理口不得用于数据面（服务端会拒绝）；' +
      '改地址或网关后提交时，服务端会要求以「commit confirmed」方式提交（避免把自己锁在门外），界面不做替代。',
    fields: [
      { label: '管理网卡', path: ['interface'], kind: 'text', hint: '如 ens160' },
      { label: '管理口地址（CIDR）', path: ['address'], kind: 'cidr', hint: '如 192.168.1.10/24' },
      { label: '管理口网关', path: ['gateway'], kind: 'ip', hint: '如 192.168.1.1' },
    ],
  },
  {
    legend: '系统 · 内核启动参数',
    loc: [{ obj: 'system' }, { obj: 'kernel' }],
    clearPath: ['system', 'kernel'],
    note: '内核启动基线（大页与隔离核的唯一真源是「资源池」节）。改动写入配置后需重启主机才生效。',
    fields: [
      { label: 'NMI watchdog', path: ['nmi_watchdog'], kind: 'bool' },
      { label: '透明大页', path: ['transparent_hugepages'], kind: 'select',
        options: [['always', 'always'], ['madvise', 'madvise'], ['never', 'never']] },
      { label: 'IOMMU', path: ['iommu'], kind: 'select',
        options: [['on', 'on'], ['off', 'off'], ['pt', 'pt（直通）']] },
      { label: '低延迟参数组', path: ['low_latency'], kind: 'bool' },
      { label: 'tuned 性能档', path: ['tuned_profile'], kind: 'text', hint: '如 nfvis-throughput' },
      { label: '附加内核参数（每行一条）', path: ['params'], kind: 'lines', hint: '如 mitigations=off' },
    ],
  },
  {
    legend: '系统 · 日志（syslog）',
    loc: [{ obj: 'system' }, { obj: 'syslog' }],
    clearPath: ['system', 'syslog'],
    note: '远程 syslog 目标与本地日志的级别、保留天数、容量上限（超上限滚动覆盖）。',
    fields: [
      { label: '远程主机', path: ['remote_host'], kind: 'ip', hint: '如 10.0.0.9' },
      { label: '远程端口', path: ['remote_port'], kind: 'int', min: 1, max: 65535 },
      { label: 'facility', path: ['facility'], kind: 'select', options: CFG_FACILITIES },
      { label: '远程转发级别', path: ['severity'], kind: 'select',
        options: [['debug', 'debug'], ['info', 'info'], ['warn', 'warn'], ['error', 'error']] },
      { label: '本地日志级别', path: ['level'], kind: 'select',
        options: [['debug', 'debug'], ['info', 'info'], ['warn', 'warn'], ['error', 'error']] },
      { label: '本地保留天数', path: ['retention_days'], kind: 'int', min: 0 },
      { label: '本地容量上限（MB）', path: ['max_size_mb'], kind: 'int', min: 1 },
    ],
  },
  {
    legend: '系统 · 健康阈值',
    loc: [{ obj: 'system' }, { obj: 'health' }, { obj: 'thresholds' }],
    clearPath: ['system', 'health'],
    note: '超过阈值即产生告警。留空 = 不设该阈值（不产生对应告警）。',
    fields: [
      { label: 'CPU 温度（℃）', path: ['cpu_temp_celsius'], kind: 'int', min: 1, max: 200 },
      { label: '磁盘温度（℃）', path: ['disk_temp_celsius'], kind: 'int', min: 1, max: 200 },
      { label: '磁盘使用率（%）', path: ['disk_used_percent'], kind: 'int', min: 1, max: 100 },
    ],
  },
  {
    legend: '系统 · 登录与口令策略',
    wide: true,
    loc: [{ obj: 'system' }, { obj: 'login' }],
    clearPath: ['system', 'login'],
    note: '本地用户、自定义 class 与口令策略。口令在本页就地派生成加盐哈希后才写入 candidate——' +
      '明文不进配置；离开口令控件（或点保存）时完成派生并清空控件，派生失败（例如控制台不是用 ' +
      'HTTPS 打开的）则保留输入，并让这次保存整体失败（不会写入没有口令的用户条目）。' +
      '已有用户不填口令即保持原口令不变。',
    fields: [
      { label: '口令最小长度', path: ['password_policy', 'min_length'], kind: 'int', min: 4, max: 128 },
      { label: '口令复杂度（≥3/4 类字符）', path: ['password_policy', 'complexity'], kind: 'bool' },
      { label: '口令有效期（天，0 = 不过期）', path: ['password_policy', 'expire_days'], kind: 'int', min: 0 },
      { label: '连续失败锁定阈值', path: ['password_policy', 'lockout_threshold'], kind: 'int', min: 1, max: 100 },
      { label: '锁定时长（分钟）', path: ['password_policy', 'lockout_minutes'], kind: 'int', min: 1, max: 10080 },
    ],
    sublists: [
      {
        list: 'users', key: 'name', keyKind: 'text', title: '本地用户', what: '用户',
        keyLabel: '用户名', placeholder: '如 ops',
        note: '新增用户后**必须立刻设置口令**——服务端不接受无口令账号（预校验/提交会拒）。',
        fields: [
          { label: '归属 class', path: ['class'], kind: 'text',
            hint: 'super-user / operator / read-only 或自定义 class 名' },
          { label: '设置口令（留空 = 不改）', path: ['password_hash'], kind: 'password',
            note: '填明文口令即在本机派生成加盐哈希写入；也可粘贴 pbkdf2$ 哈希（与配置导入同一口径）。' +
              '派生要用浏览器只在 HTTPS 或 localhost 下提供的能力——不满足时保存会被拒绝，输入会保留。' },
        ],
      },
      {
        list: 'classes', key: 'name', keyKind: 'text', title: '自定义 class', what: 'class',
        keyLabel: 'class 名', placeholder: '如 netops',
        note: 'allow / deny 是命令树节点前缀（逗号分隔，deny 优先）；预置 class 不必在此定义。',
        fields: [
          { label: '允许的节点', path: ['allow'], kind: 'strlist', hint: '如 show,configure' },
          { label: '拒绝的节点', path: ['deny'], kind: 'strlist' },
        ],
      },
    ],
  },
  {
    legend: '系统 · 主机防火墙',
    wide: true,
    loc: [{ obj: 'system' }, { obj: 'firewall' }],
    clearPath: ['system', 'firewall'],
    note: '管理面主机防火墙（`set system firewall …`）：由 nfvisd 渲染成宿主 nftables 独立表，' +
      '只作用于**管理口入向**，数据面不受影响。规则按序号升序首命中生效；' +
      '配置前须先声明管理口（`set system management interface <ifname>`），否则提交会被拒。',
    fields: [
      { label: '默认策略', path: ['default_policy'], kind: 'select',
        options: [['accept', 'accept'], ['drop', 'drop（白名单模式）']],
        note: '缺省 accept；drop 即白名单模式——未命中任何规则的管理口入向流量被丢弃。' },
    ],
    sublists: [{
      list: 'rules', key: 'seq', keyKind: 'int', title: '规则', what: '规则',
      keyLabel: '序号', placeholder: '如 10',
      extra: () => ({ action: 'accept' }),
      note: '每条规则至少给一条匹配条件（来源 / 协议 / 端口之一）——只写动作的规则会匹配全部管理入向流量，提交会被拒。' +
        '端口仅 tcp/udp 可配；来源可写前缀或单个 IP（服务端把裸 IP 按 /32、/128 归一）。',
      fields: [
        { label: '动作', path: ['action'], kind: 'select', options: [['accept', 'accept'], ['drop', 'drop']] },
        { label: '来源前缀', path: ['source'], kind: 'ipOrCidr',
          hint: '如 192.168.1.0/24 或 10.0.0.1（留空 = 任意来源）' },
        { label: '协议', path: ['protocol'], kind: 'select',
          options: [['tcp', 'tcp'], ['udp', 'udp'], ['icmp', 'icmp'], ['any', 'any（任意）']] },
        { label: '目的端口', path: ['port'], kind: 'int', min: 1, max: 65535, note: '仅 tcp/udp 可配。' },
      ],
    }],
  },
  {
    legend: '系统 · 数据面 DNS 代理',
    loc: [{ obj: 'vpp' }],
    clearPath: ['vpp', 'dns_proxy_servers'],
    note: '数据面 DNS 代理的**全局上游**（`set system dns proxy server <ip> [secondary <ip>]`）：' +
      '域内 VNF/容器把 resolver 指向产品自己的地址即可解析，上游由 nfvisd 用**宿主网络栈**发起（不是 VPP FIB）。' +
      '全局或任一交换机的按域上游非空即启用；全空即注销。上游可写 IPv4 或 IPv6，但**实际只覆盖 IPv4/UDP/53**' +
      '（TCP 查询与 IPv6 解析查询尚未覆盖）；纯转发不缓存。按交换机覆盖见「虚拟交换机」节的「数据面 DNS 代理上游」。',
    fields: [{
      label: '全局上游 DNS（逗号分隔）', path: ['dns_proxy_servers'], kind: 'iplist',
      hint: '如 192.168.155.1, 1.1.1.1',
      note: '留空并保存 = 删除该项（全局与各域都空时整个代理停用）。清空本节只清全局这一项，不动交换机的按域上游。',
    }],
  },
];

// VPP 段（命令行 `set vpp …`）：生成数据面启动配置的运行时参数。
// 导出给系统各页复用（同上）：运维动作页的画整节，接口详情页只取它的 per_dev 子表。
export const CFG_VPP_SECTIONS = [
  {
    legend: 'VPP（数据面）',
    wide: true,
    loc: [{ obj: 'vpp' }],
    clearPath: ['vpp'],
    note: '数据面运行时配置。主核与 worker 核必须取自「资源池」的隔离核池，大页偏好须与资源池的页大小一致' +
      '（服务端在提交时校验）；CPU / 内存 / DPDK 的改动提交后需重启数据面才生效。逐字段清空即回落默认值。',
    fields: [
      { label: 'CPU 主核', path: ['cpu', 'main_core'], kind: 'int', min: 0 },
      { label: 'worker 核列表', path: ['cpu', 'corelist_workers'], kind: 'corelist', hint: '如 5,7,9-11',
        group: 'vppWorkers' },
      { label: '每 NUMA worker 数', path: ['cpu', 'workers_per_numa'], kind: 'int', min: 1,
        group: 'vppWorkers' },
      { label: '内存主堆大小', path: ['memory', 'main_heap_size'], kind: 'heapSize', hint: '如 1G、512M' },
      { label: '每 NUMA buffer 数', path: ['memory', 'buffers_per_numa'], kind: 'int', min: 1 },
      { label: '大页偏好', path: ['memory', 'hugepage_preference'], kind: 'select',
        options: [['2M', '2M'], ['1G', '1G']] },
      { label: 'DPDK 全局收队列', path: ['dpdk', 'dev', 'rx_queues'], kind: 'int', min: 1 },
      { label: 'DPDK 全局发队列', path: ['dpdk', 'dev', 'tx_queues'], kind: 'int', min: 1 },
      { label: 'DPDK 全局收描述符', path: ['dpdk', 'dev', 'rx_descriptors'], kind: 'int', min: 1 },
      { label: 'DPDK 全局发描述符', path: ['dpdk', 'dev', 'tx_descriptors'], kind: 'int', min: 1 },
      { label: 'UIO 驱动', path: ['dpdk', 'uio_driver'], kind: 'select',
        options: [['vfio-pci', 'vfio-pci'], ['igb-uio', 'igb-uio']] },
    ],
    sublists: [
      {
        list: 'per_dev', key: 'interface', keyKind: 'text', title: '单网卡覆盖', what: '单网卡覆盖',
        loc: [{ obj: 'dpdk' }], // per_dev 挂在 vpp.dpdk 下（不是 vpp 下）
        keyLabel: '接口名', placeholder: '如 ens192',
        note: '只对已声明的物理口生效（服务端会校验）；未覆盖的参数继承上面的全局默认。' +
          '删除整行 = 该网卡全部参数回落全局默认。',
        fields: [
          { label: '收队列', path: ['rx_queues'], kind: 'int', min: 1 },
          { label: '发队列', path: ['tx_queues'], kind: 'int', min: 1 },
          { label: '收描述符', path: ['rx_descriptors'], kind: 'int', min: 1 },
          { label: '发描述符', path: ['tx_descriptors'], kind: 'int', min: 1 },
        ],
      },
      {
        list: 'plugins', key: 'name', keyKind: 'text', title: '插件开关', what: '插件',
        keyLabel: '插件名', placeholder: '如 acl / nat / span',
        note: '删除该行 = 恢复默认（启用）。',
        fields: [{
          label: '状态', path: ['state'], kind: 'select',
          options: [['enable', '启用'], ['disable', '禁用']],
        }],
      },
    ],
  },
];

// 网络域 / 计算域各节（导出给对象页：配置页去中心化后，各对象的列表页/详情页经
// renderSection(box, sec, c, opts) 用**同一份**节定义就地渲染表单——节定义只此一份，不复制）。
export const CFG_SECTIONS = [
  {
    legend: '虚拟交换机',
    wide: true,
    list: 'virtual_switches',
    key: 'name',
    what: '虚拟交换机',
    title: (it) => (it.name || '（未命名）') + (it.type ? '（' + it.type + '）' : ''),
    note: 'L2 = 桥域（access VLAN / 网关 / 成员端口），L3 = VRF（三层接口与静态路由在「VRF / 路由」节的同名条目里）。' +
      '类型创建后不可改；新增按 L2 建立，要 L3 请在新增后把类型改为 L3。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 vs-a', extra: () => ({ type: 'l2' }) },
    fields: [
      { label: '类型', path: ['type'], kind: 'select', options: [['l2', 'L2（桥域）'], ['l3', 'L3（VRF）']] },
      { label: '描述', path: ['description'], kind: 'text' },
      { label: 'access VLAN', path: ['vlan_access'], kind: 'int', min: 1, max: 4094 },
      { label: '端口直通（cross-connect）', path: ['cross_connect'], kind: 'bool',
        note: '配对＝该交换机的**两个成员端口**（恰好两个，按成员端口顺序直通）——打开前先把这两条成员端口配好。' +
          '直通是二层透传、无环路保护：两端必须属于不同广播域，同一广播域内直通会成环。' },
      { label: '网关地址（CIDR，逗号分隔）', path: ['gateway', 'addresses'], kind: 'cidrlist' },
      { label: '网关所属 VRF', path: ['gateway', 'vrf'], kind: 'text' },
      { label: '网关入向 ACL', path: ['gateway', 'acl_in'], kind: 'text' },
      { label: '网关出向 ACL', path: ['gateway', 'acl_out'], kind: 'text' },
      { label: 'DHCP 中继服务器', path: ['dhcp_relay_server'], kind: 'ip4', group: 'dhcpMode',
        hint: '如 192.168.99.10',
        note: 'DHCP 中继：仅已配网关（BVI）的 L2 交换机可配，中继源地址自动取网关的 IPv4 地址。' +
          '与 DHCP 服务器**互斥**（同一交换机只能选其一；跨交换机也不能同时配——两者争抢 UDP/67 处理权）。' },
      { label: 'DHCP 服务器池起始', path: ['dhcp_server_pool_start'], kind: 'ip4', group: 'dhcpMode',
        hint: '如 192.168.99.100',
        note: 'DHCP 服务器（用户态）：启用要件，须与结束地址同时给；仅已配网关的 L2 交换机可配，且与 DHCP 中继互斥。' +
          '池须与网关同子网、不含网关/网络/广播地址、最多 4096 个地址。' },
      { label: 'DHCP 服务器池结束', path: ['dhcp_server_pool_end'], kind: 'ip4', hint: '如 192.168.99.200' },
      { label: 'DHCP 租约时长（秒）', path: ['dhcp_server_lease_time_seconds'], kind: 'int',
        min: 60, max: 2592000, note: '缺省 86400。' },
      { label: 'DHCP 下发 DNS', path: ['dhcp_server_dns'], kind: 'ip4', hint: '缺省＝网关地址' },
      { label: 'DHCP 下发域名', path: ['dhcp_server_domain_name'], kind: 'text', hint: '如 lab.local' },
      { label: '数据面 DNS 代理上游（本域，逗号分隔）', path: ['dns_proxy_servers'], kind: 'iplist',
        hint: '如 192.168.99.10（留空 = 回落全局）',
        note: '只对**本交换机转发域**的入向查询生效（L2＝网关 BVI 地址；L3＝其三层接口地址）：' +
          '本域非空优先，否则回落「系统 · 数据面 DNS 代理」节的全局上游；两者皆空的域查询回 SERVFAIL。' +
          '内核数据面下该交换机还须至少一个 IPv4 落点（L2＝IPv4 网关地址；L3＝同名 VRF 的 IPv4 三层接口地址），' +
          '否则提交期拒绝。' },
      { label: 'MAC 学习上限（环路缓解）', path: ['learn_limit'], kind: 'int', min: 1, max: 16777216,
        hint: '如 8192（留空 = 不设限）',
        note: '仅 L2：桥域 MAC 学习条数上限（1-16777216，超出提交期拒绝），是环路/广播风暴的**缓解**手段' +
          '（只告警不阻断，非 STP）；清空即回默认不设限。L3 交换机配它会被提交期拒绝。' },
    ],
    sublists: [{
      list: 'ports', key: 'seq', keyKind: 'int', title: '成员端口', what: '端口',
      keyLabel: '序号', placeholder: '如 1',
      note: '物理口 / VNF / 容器三选一（本地就会拦下同时填两个的情况）；trunk VLAN 用逗号分隔。',
      fields: [
        { label: '物理口 / bond', path: ['interface'], kind: 'text', group: 'portKind' },
        { label: 'VNF（VM 名）', path: ['vnf'], kind: 'text', group: 'portKind',
          clears: [['vnf_interface']] },
        { label: 'VNF 的 vNIC', path: ['vnf_interface'], kind: 'text' },
        { label: '容器名', path: ['container'], kind: 'text', group: 'portKind',
          clears: [['container_interface']] },
        { label: '容器的 vNIC', path: ['container_interface'], kind: 'text' },
        { label: 'trunk VLAN 列表', path: ['trunk'], kind: 'intlist', min: 1, max: 4094 },
        { label: 'native VLAN', path: ['native'], kind: 'int', min: 1, max: 4094 },
        { label: '入向 ACL', path: ['acl_in'], kind: 'text' },
        { label: '出向 ACL', path: ['acl_out'], kind: 'text' },
      ],
    }],
  },
  {
    legend: 'VRF / 路由',
    wide: true,
    list: 'vrfs',
    key: 'name',
    what: 'VRF',
    title: (it) => it.name || '（未命名）',
    note: 'L3 虚拟交换机的三层接口与静态路由就存在与交换机同名的条目里——在「虚拟交换机」节把类型改为 L3 后，' +
      '这里会出现同名条目（反过来：这里新建的条目若没有同名 L3 交换机，只是独立的 VRF 配置）。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 vs-l3' },
    fields: [{ label: '描述', path: ['description'], kind: 'text' }],
    sublists: [
      {
        list: 'l3_interfaces', key: 'interface', keyKind: 'text', title: '三层接口', what: '三层接口',
        keyLabel: '接口名', placeholder: '如 ens192 或 ens192.100',
        note: '接口名与「接口」节里的物理口一致；VLAN 子接口写成 ens192.100。',
        fields: [
          { label: 'VLAN', path: ['vlan'], kind: 'int', min: 1, max: 4094 },
          { label: '地址（CIDR，逗号分隔）', path: ['addresses'], kind: 'cidrlist' },
          { label: '入向 ACL', path: ['acl_in'], kind: 'text' },
        ],
      },
      {
        list: 'routes', key: 'prefix', keyKind: 'cidr', title: '静态路由', what: '静态路由',
        keyLabel: '目的网段', placeholder: '如 10.0.0.0/8 或 0.0.0.0/0',
        fields: [
          { label: '下一跳', path: ['next_hop'], kind: 'ip' },
          { label: '管理距离', path: ['distance'], kind: 'int', min: 0, max: 255 },
        ],
      },
    ],
  },
  {
    legend: 'ACL',
    wide: true,
    list: 'acls',
    key: 'name',
    what: 'ACL',
    title: (it) => (it.name || '（未命名）') + '（' + ((it.rules || []).length) + ' 条规则）',
    note: '规则按 seq 从小到大生效；源/目的可写 ip-prefix 或 any。绑定在交换机端口 / L3 接口 / 网关的 ACL 字段上。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 acl-a' },
    fields: [{ label: '描述', path: ['description'], kind: 'text' }],
    sublists: [{
      list: 'rules', key: 'seq', keyKind: 'int', title: '规则', what: '规则',
      keyLabel: '序号', placeholder: '如 10',
      fields: [
        { label: '方向', path: ['direction'], kind: 'select', options: [['ingress', 'ingress'], ['egress', 'egress']] },
        { label: '源', path: ['source'], kind: 'ipAny', hint: 'ip-prefix 或 any' },
        { label: '目的', path: ['destination'], kind: 'ipAny', hint: 'ip-prefix 或 any' },
        { label: '协议', path: ['protocol'], kind: 'select', options: [['tcp', 'tcp'], ['udp', 'udp'], ['icmp', 'icmp'], ['any', 'any']] },
        { label: '源端口', path: ['source_port'], kind: 'port', hint: '如 80 或 1024-65535' },
        { label: '目的端口', path: ['destination_port'], kind: 'port' },
        { label: '动作', path: ['action'], kind: 'select', options: [['permit', 'permit'], ['deny', 'deny']] },
      ],
    }],
  },
  {
    legend: 'NAT',
    wide: true,
    loc: [{ obj: 'nat' }],
    clearPath: ['nat'],
    note: 'NAT44 仅作用于 L3 交换机；规则的出接口必填（不填提交会被拒），且出接口须是某 L3 交换机的三层接口并配了地址。',
    sublists: [
      {
        list: 'source_pools', key: 'name', keyKind: 'text', title: '源地址池', what: '地址池',
        keyLabel: '池名', placeholder: '如 pool-a',
        fields: [{ label: '地址范围', path: ['address_range'], kind: 'addrRange', hint: '192.168.155.220 to 192.168.155.225' }],
      },
      {
        list: 'rules', key: 'seq', keyKind: 'int', title: '转换规则', what: 'NAT 规则',
        keyLabel: '序号', placeholder: '如 10',
        fields: [
          { label: '匹配源（CIDR）', path: ['match_source'], kind: 'cidr' },
          { label: 'L3 交换机', path: ['virtual_switch'], kind: 'text' },
          { label: '出接口', path: ['action', 'interface'], kind: 'text' },
          { label: '源地址池', path: ['action', 'source_pool'], kind: 'text' },
        ],
      },
      {
        list: 'static', key: 'inside_ip', keyKind: 'ip', title: '静态 1:1 映射', what: '静态映射',
        keyLabel: '内部 IP', placeholder: '如 10.10.0.10',
        fields: [{ label: '外部 IP', path: ['outside_ip'], kind: 'ip' }],
      },
    ],
  },
  {
    legend: 'QoS（限速）',
    list: 'qos_policies',
    key: 'name',
    what: 'QoS 策略',
    title: (it) => (it.name || '（未命名）') +
      (it.cir ? '（' + it.cir + ' bps）' : ''),
    note: '绑定到接口用「接口」节的「入向限速策略」。',
    add: { keyLabel: '策略名', keyKind: 'text', placeholder: '如 lim-a' },
    fields: [
      { label: 'CIR（bps）', path: ['cir'], kind: 'int', min: 1 },
      { label: 'CBS（字节）', path: ['cbs'], kind: 'int', min: 1 },
    ],
  },
  {
    legend: '端口镜像（SPAN）',
    wide: true,
    list: 'port_mirroring',
    key: 'name',
    what: '镜像会话',
    title: (it) => it.name || '（未命名）',
    note: '源二选一：物理口，或某台 VM 的 vNIC（本地会拦下同时填两个的情况）。分析口必须是物理口。',
    add: { keyLabel: '会话名', keyKind: 'text', placeholder: '如 span-a' },
    fields: [
      { label: '源：物理口 / bond', path: ['source', 'interface'], kind: 'text', group: 'pmSource',
        clears: [['source', 'vnf'], ['source', 'vnf_interface']] },
      { label: '源：VNF（VM 名）', path: ['source', 'vnf'], kind: 'text', group: 'pmSource',
        clears: [['source', 'interface']] },
      { label: '源：VNF 的 vNIC', path: ['source', 'vnf_interface'], kind: 'text' },
      { label: '方向', path: ['source', 'direction'], kind: 'select', options: [['ingress', 'ingress'], ['egress', 'egress'], ['both', 'both']] },
      { label: '分析端口', path: ['analyzer'], kind: 'text' },
    ],
  },
  {
    legend: '链路聚合（bond）',
    list: 'bonds',
    key: 'name',
    what: 'bond',
    title: (it) => (it.name || '（未命名）') + (it.lacp ? '（LACP）' : '（静态聚合）'),
    note: '成员口须是未被虚拟交换机引用的物理口；清空 LACP 模式与速率即回到静态聚合。',
    add: { keyLabel: 'bond 名', keyKind: 'text', placeholder: '如 bond0' },
    fields: [
      { label: '成员口（逗号分隔）', path: ['members'], kind: 'strlist', hint: '如 ens224,ens256' },
      { label: 'LACP 模式', path: ['lacp', 'mode'], kind: 'select', options: [['active', 'active'], ['passive', 'passive']] },
      { label: 'LACP 速率', path: ['lacp', 'interval'], kind: 'select', options: [['fast', 'fast'], ['slow', 'slow']] },
      { label: 'MTU', path: ['mtu'], kind: 'int', min: 68, max: 9216 },
      { label: '描述', path: ['description'], kind: 'text' },
    ],
  },
  {
    legend: 'LLDP',
    loc: [{ obj: 'protocols' }, { obj: 'lldp' }],
    clearPath: ['protocols', 'lldp'],
    note: '全局开关 + 按接口覆盖（基于 VPP lldp 插件）。邻居表在「网络对象」页。',
    fields: [
      { label: '全局启用', path: ['enabled'], kind: 'bool' },
      { label: '通告间隔（秒）', path: ['advertisement_interval'], kind: 'int', min: 1, max: 3600 },
    ],
    sublists: [{
      list: 'interfaces', key: 'interface', keyKind: 'text', title: '按接口覆盖', what: '接口覆盖',
      keyLabel: '接口名', placeholder: '如 ens224',
      fields: [{ label: '启用', path: ['enabled'], kind: 'bool' }],
    }],
  },
  {
    legend: '虚拟机（VM VNF）',
    wide: true,
    list: 'virtual_machine_functions',
    key: 'name',
    what: '虚拟机',
    title: (it) => (it.name || '（未命名）') + (it.image ? '（' + it.image + '）' : ''),
    note: '镜像须是仓库里的 vm-image；vCPU 从隔离核池分配，内存从大页池分配。' +
      '内存后端选「普通内存」时不允许 vhost-user 口（提交会被拒）。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 vnf-a' },
    fields: [
      { label: '镜像', path: ['image'], kind: 'text', hint: '如 base.qcow2' },
      { label: '描述', path: ['description'], kind: 'text' },
      { label: 'vCPU 数量', path: ['vcpu', 'count'], kind: 'int', min: 1, max: 256 },
      { label: 'vCPU 绑核', path: ['vcpu', 'pin'], kind: 'bool' },
      { label: '内存（MB）', path: ['memory', 'size_mb'], kind: 'int', min: 1 },
      { label: '大页大小', path: ['memory', 'hugepage_size'], kind: 'select', options: [['2M', '2M'], ['1G', '1G']] },
      { label: 'NUMA 节点', path: ['memory', 'numa_node'], kind: 'int', min: 0, max: 63 },
      { label: '内存后端', path: ['memory', 'backing'], kind: 'select', options: [['hugepage', '大页'], ['normal', '普通内存（禁止 vhost-user 口）']] },
      { label: '串口控制台', path: ['serial_console'], kind: 'bool' },
      { label: '开机自启', path: ['autostart'], kind: 'bool' },
      { label: 'guest 主机名', path: ['cloud_init', 'hostname'], kind: 'text' },
      { label: 'SSH 公钥（逗号分隔）', path: ['cloud_init', 'ssh_keys'], kind: 'strlist' },
      { label: 'user-data（YAML 文本）', path: ['cloud_init', 'user_data'], kind: 'raw' },
      { label: 'PCI 直通设备（BDF，逗号分隔）', path: ['pci_devices'], kind: 'bdflist',
        hint: '如 0000:03:00.0, 05:10.1（domain 可省略）',
        note: '通用 PCI 直通（`set virtual-machine-functions <n> pci-device <bdf>`）：同一设备不能同时直通给两台 VM' +
          '（提交期拒绝）；设备必须真实存在（组装前核对 /sys/bus/pci/devices），驱动预绑定与 IOMMU 分组由宿主侧负责。' },
      { label: '启动画像 · 机器类型', path: ['startup', 'machine'], kind: 'machineName',
        hint: 'q35 | pc | pc-i440fx-* | pc-q35-*（留空 = 用镜像画像/默认）',
        note: '以下「启动画像」字段**覆盖镜像画像**的同名项（实例覆盖 > 镜像画像 > 产品默认）；留空即不覆盖该字段。' },
      { label: '启动画像 · CPU 模式', path: ['startup', 'cpu_mode'], kind: 'select',
        options: [['host-passthrough', 'host-passthrough'], ['host-model', 'host-model'], ['custom', 'custom']],
        note: 'custom 必须给出 CPU 型号（服务端拒绝）；accel=tcg（纯软件模拟）不能与 host-passthrough 同用。' },
      { label: '启动画像 · CPU 型号', path: ['startup', 'cpu_model'], kind: 'text',
        hint: '如 Skylake-Client-IBRS（仅 cpu 模式 = custom 必填）' },
      { label: '启动画像 · 固件', path: ['startup', 'firmware'], kind: 'select',
        options: [['bios', 'bios'], ['uefi', 'uefi']] },
      { label: '启动画像 · 磁盘接口', path: ['startup', 'disk_interface'], kind: 'select',
        options: [['virtio', 'virtio'], ['sata', 'sata'], ['ide', 'ide'], ['scsi', 'scsi']],
        note: 'q35 系列机器类型没有 IDE 总线——ide 与 q35/pc-q35-* 同用会被提交期拒绝。' },
      { label: '启动画像 · 引导顺序', path: ['startup', 'boot_order'], kind: 'select',
        options: [['hd', 'hd'], ['cd', 'cd'], ['dc', 'dc']] },
      { label: '启动画像 · 加速器', path: ['startup', 'accel'], kind: 'select',
        options: [['kvm', 'kvm'], ['tcg', 'tcg（纯软件模拟）']] },
      { label: '启动画像 · 控制台形态', path: ['startup', 'console'], kind: 'select',
        options: [['serial', 'serial'], ['telnet', 'telnet'], ['vnc', 'vnc（Web 内置查看器）']],
        note: '显式设定即**权威**（独立于上面的「串口控制台」开关）；telnet 端口由 VM 名确定性派生，' +
          '两台 telnet 控制台的 VM 可能撞端口——提交期会点名冲突的两台。' },
      { label: '启动画像 · 额外 QEMU 参数（每行一条）', path: ['startup', 'extra_args'], kind: 'qemuArgs',
        hint: '如 -no-hpet',
        note: '窄自由口（S 档）：命中拒绝前缀（-m/-drive/-netdev/-cpu/-machine 等，与资源账本、网络/磁盘/console 模型冲突的）' +
          '本地即拦下，提交期同判。这些参数**已绕过资源账本**——产品不再校验其资源含义。' },
    ],
    sublists: [
      {
        list: 'disks', key: 'name', keyKind: 'text', title: '附加数据盘', what: '数据盘',
        keyLabel: '盘名', placeholder: '如 data0',
        note: '容量与来源镜像二选一（都填或都不填提交会被拒）。',
        fields: [
          { label: '容量（GB，空盘）', path: ['size_gb'], kind: 'int', min: 1 },
          { label: '来源镜像', path: ['image'], kind: 'text' },
        ],
      },
      {
        list: 'interfaces', key: 'name', keyKind: 'text', title: 'vNIC', what: 'vNIC',
        keyLabel: 'vNIC 名', placeholder: '如 eth0',
        note: 'vhost-user 接虚拟交换机；sriov-vf 还需物理口与 VF 号。',
        fields: [
          { label: '类型', path: ['type'], kind: 'select', options: [['vhost-user', 'vhost-user'], ['sriov-vf', 'sriov-vf（SR-IOV 直通）']] },
          { label: '虚拟交换机', path: ['virtual_switch'], kind: 'text' },
          { label: 'MAC', path: ['mac'], kind: 'mac', hint: '留空自动生成' },
          { label: 'VLAN', path: ['vlan'], kind: 'int', min: 1, max: 4094 },
          { label: 'SR-IOV 物理口', path: ['sriov', 'physical_interface'], kind: 'text' },
          { label: 'SR-IOV VF 号', path: ['sriov', 'vf_id'], kind: 'int', min: 0, max: 255 },
        ],
      },
    ],
  },
  {
    legend: '容器（container VNF）',
    wide: true,
    list: 'container_functions',
    key: 'name',
    what: '容器',
    title: (it) => (it.name || '（未命名）') + (it.image ? '（' + it.image + '）' : ''),
    note: '镜像须是仓库里的 container-image；vNIC 固定为 memif。',
    add: { keyLabel: '名称', keyKind: 'text', placeholder: '如 sbc-ct1' },
    fields: [
      { label: '镜像', path: ['image'], kind: 'text', hint: '如 alpine:3.20' },
      { label: '描述', path: ['description'], kind: 'text' },
      { label: 'vCPU 限制', path: ['vcpu'], kind: 'int', min: 1 },
      { label: '内存限制（MB）', path: ['memory_mb'], kind: 'int', min: 1 },
      { label: '入口命令', path: ['command'], kind: 'text' },
      { label: '命令参数（逗号分隔）', path: ['args'], kind: 'strlist' },
      { label: '重启策略', path: ['restart_policy'], kind: 'select', options: [['no', '不自动重启'], ['on-failure', '失败时重启']] },
      { label: '开机自启', path: ['autostart'], kind: 'bool' },
      { label: '环境变量（每行 KEY=VALUE）', path: ['env'], kind: 'kvmap' },
    ],
    sublists: [{
      list: 'interfaces', key: 'name', keyKind: 'text', title: 'memif vNIC', what: 'vNIC',
      keyLabel: 'vNIC 名', placeholder: '如 eth0',
      fields: [
        { label: '类型', path: ['type'], kind: 'select', options: [['memif', 'memif']] },
        { label: '虚拟交换机', path: ['virtual_switch'], kind: 'text' },
        { label: 'MAC', path: ['mac'], kind: 'mac', hint: '留空自动生成' },
        { label: 'VLAN', path: ['vlan'], kind: 'int', min: 1, max: 4094 },
      ],
    }],
  },
  {
    legend: 'VXLAN overlay',
    wide: true,
    list: 'vxlan_tunnels',
    key: 'name',
    what: 'VXLAN 隧道',
    title: (it) => (it.name || '（未命名）') + (it.vni ? '（VNI ' + it.vni + '）' : ''),
    note: 'VXLAN overlay（`set vxlan tunnels …`，VPP vxlan plugin）：单播 remote、IPv4 下垫层。' +
      'virtual-switch 把隧道口加入该 **L2** 交换机的 bridge-domain（L3 交换机没有 BD，提交会被拒）；' +
      '被隧道引用的交换机在解引用前删不掉。',
    add: { keyLabel: '隧道名', keyKind: 'text', placeholder: '如 tun-a' },
    fields: [
      { label: 'VNI', path: ['vni'], kind: 'int', min: 1, max: 16777215 },
      { label: '本地下垫地址', path: ['local'], kind: 'ip4', hint: '如 10.99.0.1' },
      { label: '远端下垫地址', path: ['remote'], kind: 'ip4', hint: '如 10.99.0.2' },
      { label: '目的端口', path: ['dst_port'], kind: 'int', min: 1, max: 65535, note: '缺省 4789。' },
      { label: '虚拟交换机（L2）', path: ['virtual_switch'], kind: 'text', hint: '如 vs-lan' },
    ],
  },
  {
    legend: '镜像启动画像',
    wide: true,
    list: 'vm_image_profiles',
    key: 'image',
    what: '镜像画像',
    title: (it) => (it.image || '（未命名）'),
    note: '镜像级启动画像（`set vm-image-profiles <镜像名> …`）：按镜像名给出 QEMU 启动参数，' +
      'VM 用该镜像时自动套用；VM 自己的「启动画像」字段可逐项覆盖（实例覆盖 > 镜像画像 > 产品默认）。' +
      '逐项留空即不设该项（合并期落产品默认）。改动在**下次定义/启动**该 VM 时生效。',
    add: { keyLabel: '镜像名', keyKind: 'text', placeholder: '如 alpine.qcow2' },
    fields: [
      { label: '机器类型', path: ['machine'], kind: 'machineName',
        hint: 'q35 | pc | pc-i440fx-* | pc-q35-*（留空 = 产品默认 q35）' },
      { label: 'CPU 模式', path: ['cpu_mode'], kind: 'select',
        options: [['host-passthrough', 'host-passthrough'], ['host-model', 'host-model'], ['custom', 'custom']],
        note: 'custom 必须给出 CPU 型号（服务端拒绝）；accel=tcg（纯软件模拟）不能与 host-passthrough 同用。' },
      { label: 'CPU 型号', path: ['cpu_model'], kind: 'text',
        hint: '如 Skylake-Client-IBRS（仅 cpu 模式 = custom 必填）' },
      { label: '固件', path: ['firmware'], kind: 'select',
        options: [['bios', 'bios'], ['uefi', 'uefi']] },
      { label: '磁盘接口', path: ['disk_interface'], kind: 'select',
        options: [['virtio', 'virtio'], ['sata', 'sata'], ['ide', 'ide'], ['scsi', 'scsi']],
        note: 'q35 系列机器类型没有 IDE 总线——ide 与 q35/pc-q35-* 同用会被提交期拒绝。' },
      { label: '引导顺序', path: ['boot_order'], kind: 'select',
        options: [['hd', 'hd'], ['cd', 'cd'], ['dc', 'dc']] },
      { label: '加速器', path: ['accel'], kind: 'select',
        options: [['kvm', 'kvm'], ['tcg', 'tcg（纯软件模拟）']] },
      { label: '控制台形态', path: ['console'], kind: 'select',
        options: [['serial', 'serial'], ['telnet', 'telnet'], ['vnc', 'vnc（Web 内置查看器）']],
        note: '显式设定即**权威**（独立于 VM 的「串口控制台」开关）；telnet 端口由 VM 名确定性派生，' +
          '两台 telnet 控制台的 VM 可能撞端口——提交期会点名冲突的两台。' },
      { label: '额外 QEMU 参数（每行一条）', path: ['extra_args'], kind: 'qemuArgs',
        hint: '如 -no-hpet',
        note: '窄自由口（S 档）：命中拒绝前缀（-m/-drive/-netdev/-cpu/-machine 等，与资源账本、网络/磁盘/console 模型冲突的）' +
          '本地即拦下，提交期同判。这些参数**已绕过资源账本**——产品不再校验其资源含义。' },
    ],
  },
];

// 预校验（POST /configuration/check）：先保存，再让服务端跑与提交相同的全部校验。
export async function cfgCheck() {
  if (!(await cfgSave())) return;
  try {
    const res = await candidateCheck();
    if (res && res.ok) {
      cfgMsg('预校验通过（服务端已跑与提交相同的全部校验）。', false);
      return;
    }
    const errs = (res && res.errors) || [];
    cfgMsg('预校验发现 ' + errs.length + ' 个问题：' + errs.map((e) => e.path + '：' + e.message).join('；'), true);
  } catch (e) {
    cfgMsg('预校验失败：' + e.message, true);
  }
}

function cfgRenderRead() {
  const rev = cfg.committed ? cfg.committed.revision : undefined;
  fill($('cfg-summary'), [
    ['配置版本', rev != null ? 'revision ' + rev : undefined],
    ['状态', cfg.committed ? '已加载 committed 配置' : '未加载'],
  ]);
  $('cfg-json').textContent = cfg.committed
    ? JSON.stringify(cfg.committed.configuration || {}, null, 2) : '';
  $('cfg-note').textContent = rev != null ? '（committed revision ' + rev + '）' : '';
}

// 数据面配置段（GET /vpp/config，committed 的 vpp 段）：它是生成启动配置的源，
// 与「完整配置」里的 vpp 段是同一份事实——单列出来是因为排障时常要单独看它（与命令行
// 的 `show vpp` 同源），省得在大 JSON 里翻。只读回显，改它仍在下面的表单里。
function renderCfgVpp(vpp) {
  $('cfg-vpp').textContent = !vpp ? ''
    : (vpp.__err ? '读取失败：' + vpp.__err : JSON.stringify(vpp, null, 2));
}

// 读取 committed 配置。`pre` 是路由表声明端点的预取结果（配置页由 softLoad 取齐后传进来，
// 不重复请求）；不传则自己拉（提交事件、结束编辑会话等处调用）。
export function getCfg() { return cfg; }
export function cfgSetEditingForBoot(on) { cfgSetEditing(on); }
export async function loadConfig(pre) {
  const res = pre !== undefined ? pre : await api('/configuration');
  if (res && res.__err) {
    cfg.committed = null;
    $('cfg-note').textContent = '（读取失败：' + res.__err + '）';
    return false;
  }
  cfg.committed = res;
  cfgRenderRead();
  return true;
}

function cfgLockNote(text) {
  const n = $('cfg-lock-note');
  n.textContent = text || '';
  n.hidden = !text;
}

// 进本页时对齐服务端的编辑会话：本会话已经有候选（例如刚从「提交历史」页取的历史快照，
// 或刷新页面后仍在编辑）就直接进编辑态并展示差异——否则操作者再点一次「开始编辑」会把
// 刚取到的候选覆盖成当前配置，白做一次回滚。锁在别的会话手里时如实说明是谁持有
// （本页保持只读；写操作由服务端按会话锁规则拒绝，界面不做第二个判定）。
async function cfgSyncSession() {
  const landed = cfg.landedFromRollback; // 刚从「提交历史」页取过候选（值是偏移）
  cfg.landedFromRollback = 0;
  // 已在编辑态且不是刚取过候选：不动（别覆盖操作者正在改的文本）。
  // 刚取过候选时**要**刷新编辑区——服务端的候选已经被那一版替换，留旧文本会让人改错底稿。
  if (cfg.editing && !landed) return;
  if (!cfg.committed) return; // 读不到当前配置：不进编辑态（与「开始编辑」同一条纪律）
  let cand = null;
  try {
    cand = await api('/configuration/candidate');
  } catch (e) {
    cfgLockNote(''); // 服务端没有候选：只读视图（点「开始编辑」才建立会话）
    return;
  }
  const holder = await cfgLockHolder();
  if (holder !== getUser() + '@api') {
    cfgLockNote('当前候选配置由 ' + (holder || '未知会话') + ' 持有编辑锁——本页只读，' +
      '可在「编辑锁会话」页查看；要自己改配置请等对方结束编辑。');
    return;
  }
  const doc = (cand && cand.candidate) || {};
  cfgWriteText(doc);
  cfgSetEditing(true);
  cfgLockNote('');
  if (landed) {
    cfgMsg('已把第 ' + landed + ' 版取为候选（未提交）——先看差异、预校验，确认后点「提交」；' +
      '不提交就点「丢弃并结束编辑」。', false);
    await cfgShowDiff();
  } else {
    cfgMsg('本会话已有未提交的候选配置（刷新页面后仍在编辑会话里）。', false);
  }
}

// 当前编辑锁的持有者：取自持锁会话列表里**有取锁时间**的那一条（只有一条会话锁，
// 另有在途 commit confirmed 时它不带取锁时间，不能当成持锁者）；查不到时返回空串。
export async function cfgLockHolder() {
  try {
    const list = await api('/system/configuration/sessions');
    const lock = (Array.isArray(list) ? list : []).find((s) => s && s.acquired_at);
    return (lock && lock.holder) || '';
  } catch (e) {
    return '';
  }
}

// 开始编辑：以 committed 为起点建立 candidate（PUT 取锁）。
// 读不到 committed 时**拒绝进入**——否则会把空配置当 candidate 提交（危险）。
export async function cfgStartEdit() {
  try {
    if (!(await loadConfig()) || !cfg.committed) {
      cfgSetEditing(false);
      $('cfg-note').textContent = '（无法进入编辑：读不到当前配置，请先刷新）';
      return;
    }
    const base = cfgCandidate();
    await candidateAcquire(base);
    cfgWriteText(base);
    cfgSetEditing(true);
    cfgMsg('已进入编辑会话（candidate = 当前 committed）。改完点「保存到 candidate」。', false);
  } catch (e) {
    cfgSetEditing(false);
    $('cfg-note').textContent = '（无法进入编辑：' + e.message + '）';
  }
}

export async function cfgSave() {
  // 口令没派生出哈希（明文还留在控件里）时**整次保存失败**：不写 candidate，也不报成功。
  if (!(await cfgPwGate())) return false;
  let c;
  try {
    c = cfgText();
  } catch (e) {
    cfgMsg('原始 JSON 语法错误：' + e.message, true);
    return false;
  }
  try {
    const res = await candidateWrite(c);
    cfgMsg(res && res.dirty ? '已保存到 candidate（有未提交变更）。' : '已保存到 candidate（与 committed 一致）。', false);
    return true;
  } catch (e) {
    cfgMsg('保存失败：' + e.message, true);
    return false;
  }
}

export async function cfgShowDiff() {
  try {
    const res = await fetch(API + '/configuration/diff', { headers: { Authorization: 'Bearer ' + getToken() } });
    const text = await res.text();
    const pre = $('cfg-diff');
    pre.textContent = res.ok ? (text.trim() || '（candidate 与 committed 无差异）') : '读取差异失败：' + errTextOf(text, res.status);
    pre.hidden = false;
  } catch (e) {
    cfgMsg('读取差异失败：' + e.message, true);
  }
}

// 结束编辑会话：DELETE candidate（discard）并释放锁。
async function cfgEndSession() {
  try {
    await candidateRelease();
  } catch (e) { /* 会话可能已因超时释放：不阻塞退出 */ }
  cfgSetEditing(false);
  await loadConfig();
}

// 丢弃并结束编辑：低危确认（只丢自己这份候选草稿，运行配置不变）。没有进入编辑态时
// 没有草稿可丢，就不打扰——直接收尾（可能只是把超时残留的锁收掉）。
export async function cfgDiscardAsk() {
  if (cfg.editing) {
    const ok = await uiConfirm('丢弃并结束编辑', {
      tier: 'low',
      paragraphs: ['丢弃候选配置并释放编辑锁？运行中的配置不受影响；' +
        '未提交的改动会丢掉，拿不准就先点「查看差异」看一眼。'],
      cli: 'discard',
    });
    if (!ok) return;
  }
  await cfgEndSession();
}

// 提交（含以 commit confirmed 提交）：中危确认——提交是把差异**立即下发到运行配置**，
// 不是保存草稿；改到管理路径（管理口地址/网关）时还可能把自己关在门外，故逐条列影响面。
// 注意：确认框只加在按钮这一层，cfgCommit 本身保持原样（结束会话/失败补偿等内部路径
// 不该被二次确认挡住）。
export async function cfgCommitAsk(confirmedMinutes) {
  const ok = await uiConfirm(confirmedMinutes ? '以 commit confirmed 提交' : '提交配置', {
    tier: 'mid',
    paragraphs: [confirmedMinutes
      ? '把候选配置提交为运行配置，并设 ' + confirmedMinutes + ' 分钟的确认窗口。'
      : '把候选配置提交为运行配置（revision 加一，立即生效）。'],
    bullets: confirmedMinutes ? [
      '提交立即生效；窗口期内不点「确认在途提交」就自动回滚到提交前那一版。',
      '这是防止改坏管理路径把自己锁在门外的自锁保护：确认管理面仍然连得上，再点「确认在途提交」。',
      '变更会立即下发给各子系统，相关业务可能随变更中断（接口、交换机、ACL、NAT 这类）。',
      '提交失败会自动补偿（事务引擎），失败原因按页面提示看。',
    ] : [
      '候选与运行配置的差异会立即下发给各子系统，相关业务可能随变更中断（接口、交换机、ACL、NAT 这类）。',
      '这不是保存草稿：要撤销得回滚到上一版再提交一次。',
      '管理口地址、网关这类会改变管理路径的字段，请改用「以 commit confirmed 提交」——超时未确认会自动回滚。',
      '拿不准就先点「预校验」「查看差异」，提交前确认变更内容。',
    ],
    cli: confirmedMinutes ? 'commit confirmed ' + confirmedMinutes : 'commit',
    confirmLabel: '提交',
  });
  if (!ok) return;
  await cfgCommit(confirmedMinutes);
}

async function cfgCommit(confirmedMinutes) {
  if (!(await cfgSave())) return;
  const body = confirmedMinutes ? { confirmed_minutes: confirmedMinutes } : {};
  try {
    const res = await candidateCommit(body);
    const warns = (res && res.warnings) || [];
    const parts = ['提交成功（revision ' + (res && res.revision) + '）'];
    if (res && res.confirmed_until) {
      parts.push('待确认：' + fmtTime(res.confirmed_until) + ' 前未点「确认在途提交」将自动回滚');
      $('cfg-confirm-btn').hidden = false;
      $('cfg-commit-confirmed-btn').hidden = true;
    }
    if (warns.length) parts.push('提示：' + warns.join('；'));
    cfgMsg(parts.join('；'), false);
    if (!(res && res.confirmed_until)) {
      await cfgEndSession(); // 已生效：释放会话锁，回到只读视图
      cfgMsg('提交成功（revision ' + (res && res.revision) + '）。' + (warns.length ? '提示：' + warns.join('；') : ''), false);
    }
  } catch (e) {
    // 服务端要求 commit confirmed 时（管理口地址/网关变更）揭示确认入口。
    // 注意：按 FR-CFG-012 原文，该强制**只约束 SSH 会话**——控制台（api 来源）改管理口
    // 当前不会被要求 confirmed（提交成功但带"注意连通性"警告，见下面的 warnings 展示）。
    // 这里保留处理是**纵深防御**：服务端一旦对 api 来源也要求确认，页面即已就绪。
    if (/必须.*commit confirmed|confirm/i.test(e.message)) {
      cfgMsg('该变更（管理口地址/网关）必须以 commit confirmed 提交：' + e.message, true);
      $('cfg-commit-confirmed-btn').hidden = false;
      return;
    }
    cfgMsg('提交失败：' + e.message, true);
  }
}

// 确认在途提交：**有意不做确认框**——它的语义就是「保住刚才那次提交」，
// 而窗口正在倒计时（超时自动回滚）；在这里再拦一次反而是害人。审计仍由服务端记。
export async function cfgConfirmPending() {
  try {
    await api('/configuration/commit:confirm', { method: 'POST' });
    await cfgEndSession();
    cfgMsg('已确认，提交生效。', false);
  } catch (e) {
    cfgMsg('确认失败：' + e.message, true);
  }
}

// ---------- 配置提交历史（#/config/history）与编辑锁会话（#/config/sessions） ----------
//
// 历史页 = 只读列表 + **两段式回滚**的入口：回滚沿用命令行的语义——把某一版取为 candidate
// （POST /configuration/rollback/{n}），**不直接生效**；要改变运行配置仍得到「配置」页提交。
//
// 两个容易搞错的地方，这里按契约与引擎的实际语义钉住：
//   ① 端点里的 n 是**相对最新 committed 的偏移**（n=1 = 最新版的前一版），不是 Rev 本身；
//      故由列表里的 Rev 与最新 Rev 相减算出，Rev 相同（当前生效那一版）没有可回滚的偏移。
//   ② 服务端只有「候选 ⇄ 当前生效配置」的差异读取（GET /configuration/diff），**没有**
//      「直接与某个历史版本比对」的读取接口；故「看差异」给的是引导（先把该版取为候选，
//      再到「配置」页看差异），而不是就地显示差异——不臆造端点，也不假装拿到了差异。

function cfghMsg(text, isErr) {
  const n = $('cfgh-msg');
  n.textContent = text || '';
  n.hidden = !text;
  n.className = isErr ? 'error small' : 'muted small';
}

function renderHistory(revs) {
  const tbody = $('cfgh-table').querySelector('tbody');
  tbody.textContent = '';
  cfghMsg('', false); // 上一次操作的回显不跨轮次残留
  const list = revs || [];
  const latest = list.reduce((m, r) => Math.max(m, Number(r.rev) || 0), 0);
  $('cfgh-note').textContent = list.length ? '（保留 ' + list.length + ' 版，最新在前）' : '';
  if (!list.length) {
    tbody.appendChild(el('tr', {}, [
      el('td', { colspan: '6', class: 'muted', text: '（暂无历史版本）' }),
    ]));
    return;
  }
  list.forEach((r) => {
    const rev = Number(r.rev) || 0;
    const n = latest - rev; // 相对最新的偏移：0 = 当前生效那一版
    const tr = el('tr');
    [rev, fmtTime(r.committed_at), r.user ? r.user : '（未记录）', r.comment, r.current ? '当前生效' : '']
      .forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    const cell = el('td', { class: 'actions' });
    if (n < 1) {
      cell.appendChild(el('span', { class: 'muted small', text: '（当前生效，无需回滚）' }));
    } else {
      const diff = el('button', { type: 'button', class: 'ghost small', text: '看差异' });
      diff.addEventListener('click', () => cfghDiffGuide(rev, n));
      const rb = wbtn({ type: 'button', class: 'small danger', text: '回滚到这一版' });
      rb.addEventListener('click', () => cfghRollback(rev, n, r));
      cell.appendChild(diff);
      cell.appendChild(rb);
    }
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 「看差异」：服务端没有按历史版本比对的读取接口，故这里只给**引导**（不取数、不改任何东西）。
async function cfghDiffGuide(rev, n) {
  await uiDialog({
    title: '看差异：第 ' + n + ' 版（revision ' + rev + '）与当前配置',
    tier: 'low',
    paragraphs: [
      '服务端只提供「候选配置与当前生效配置」的差异读取，没有「直接与某个历史版本比对」的读取接口，' +
        '因此本页无法就地显示这一版与当前配置的差异。',
      '要看差异：先在本页点「回滚到这一版」（第一步只把它取为候选，不会生效），' +
        '随后在「配置」页点「查看差异」——那里显示的是「当前生效配置与这一版」的差异' +
        '（命令行里 compare rollback 的方向相反：那一侧是「这一版 → 当前生效配置」，改动内容是同一份）；' +
        '确认不需要这一版，就点「丢弃并结束编辑」，运行配置不受影响。',
    ],
    cli: 'show configuration | compare rollback ' + n,
    confirmLabel: '知道了',
    cancelLabel: '',
  });
}

// 回滚第一步：把第 n 版取为候选（不提交）。中危确认——影响面逐条列出、主按钮标红。
async function cfghRollback(rev, n, row) {
  const dirty = await cfghCandidateDirty();
  const bullets = [
    '会以第 ' + n + ' 版（revision ' + rev + '，提交于 ' + fmtTime(row && row.committed_at) +
      '）为底稿建立候选配置；正在运行的配置此刻不受影响。',
    '候选不会自己生效：要到「配置」页点「提交」才改变运行配置；提交之前可以随时「丢弃并结束编辑」。',
    '提交前请在「配置」页用「查看差异」「预校验」确认这次变更。',
  ];
  if (dirty) bullets.push('当前已有候选配置（含未提交的变更），会被这一版覆盖。');
  bullets.push('取为候选会占用编辑锁（本会话还没有编辑会话时，本页先按当前配置取锁）；' +
    '锁被别的会话持有时会失败并说明是谁持有。');
  const ok = await uiDialog({
    title: '回滚到第 ' + n + ' 版（取为候选，不立即生效）',
    tier: 'mid',
    bullets,
    cli: 'rollback ' + n,
    confirmLabel: '取为候选（不提交）',
  });
  if (!ok) return;
  await cfghTakeCandidate(n);
}

// 当前是否已有未提交的候选（影响面里要如实说「会被覆盖」）。没有候选（未进入编辑会话）按「无」。
async function cfghCandidateDirty() {
  try {
    const c = await api('/configuration/candidate');
    return !!(c && c.dirty);
  } catch (e) {
    return false;
  }
}

// 取为候选：先直接试一次；报「未持有 candidate」说明本会话还没有编辑会话（或锁在别人手里），
// 此时按当前配置取一次编辑锁再重试——锁真被别人持有时这一步会失败并报出持有者，
// 不会覆盖别人的候选（服务端在写候选之前就先判锁）。
async function cfghTakeCandidate(n) {
  cfghMsg('正在把第 ' + n + ' 版取为候选…', false);
  try {
    await api('/configuration/rollback/' + n, { method: 'POST' });
  } catch (e) {
    if (!/未持有/.test(e.message)) {
      cfghMsg('取为候选失败：' + e.message, true);
      return;
    }
    if (!(await cfghAcquireLock())) return;
    try {
      await api('/configuration/rollback/' + n, { method: 'POST' });
    } catch (e2) {
      cfghMsg('取为候选失败：' + e2.message, true);
      return;
    }
  }
  // 落到「配置」页：那一页会载入这份候选并直接展示差异（提交或丢弃都在那一页）。
  cfg.landedFromRollback = n;
  await goDetail('#/config');
}

// 取编辑锁：以当前生效配置为底稿写一次候选（与「配置」页的「开始编辑」是同一个动作）。
async function cfghAcquireLock() {
  try {
    const cur = await api('/configuration');
    const base = (cur && cur.configuration) || {};
    await candidateAcquire(base);
    return true;
  } catch (e) {
    cfghMsg('无法取得编辑锁：' + e.message + '——锁可能正被别的会话持有，可在「编辑锁会话」页查看。', true);
    return false;
  }
}

// 编辑锁会话（只读）：按端点返回的字段渲染（谁 / 来源 / 时间 / 未提交变更 / 待确认截止），
// 字段缺席时显示「—」，不编造。
function renderSessions(list) {
  const tbody = $('cfgs-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = list || [];
  $('cfgs-note').textContent = rows.length ? '（' + rows.length + ' 个持锁条目）' : '';
  if (!rows.length) {
    tbody.appendChild(el('tr', {}, [
      el('td', { colspan: '6', class: 'muted', text: '（当前没有会话持有编辑锁）' }),
    ]));
    return;
  }
  rows.forEach((s) => {
    const holder = s.holder || '';
    const tr = el('tr');
    [
      holder,
      sessionSource(holder),
      fmtTimeOpt(s.acquired_at),
      fmtTimeOpt(s.last_activity),
      s.dirty === undefined || s.dirty === null ? undefined : (s.dirty ? '有' : '无'),
      fmtTimeOpt(s.confirmed_until),
    ].forEach((c) => tr.appendChild(el('td', { text: String(dash(c)) })));
    tbody.appendChild(tr);
  });
}

// 来源取自持有者标识的后缀：引擎把持有者记作 `用户@来源`（命令行/串口/接口分别记作
// ssh / console / api）；没有 @ 时如实显示「—」，不猜。
function sessionSource(holder) {
  const i = (holder || '').indexOf('@');
  return i > 0 && i < holder.length - 1 ? holder.slice(i + 1) : undefined;
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsConfig = {
  'config': {
    // 取数后对齐一次服务端的编辑会话（本会话已有候选就直接进编辑态，见 cfgSyncSession）。
    async render(d) {
      pageWarn(d);
      await loadConfig(d['/configuration']);
      renderCfgVpp(d['/vpp/config']);
      await cfgSyncSession();
    }
  },
  'configHistory': {
    render(d) { pageWarn(d); renderHistory(rowsOf(d['/configuration/history'])); }
  },
  'configSessions': {
    render(d) { pageWarn(d); renderSessions(rowsOf(d['/system/configuration/sessions'])); }
  },
};
