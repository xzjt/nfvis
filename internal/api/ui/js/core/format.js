// NFViS 控制台共享件 · 格式化（从 app.js 拆出，逻辑逐字保留；Web 控制台重构 2026-10）。

// dash：缺值显示「—」的唯一实现（dom.js 是零业务知识的件，它导出的 dash 就是这一份；
// 本模块需要它只为 ifaceDropsText/ifaceDropsCell 的缺值分支——单向依赖，不成环）。
import { dash } from './dom.js';

export function uptime(sec) {
  if (sec === undefined || sec === null) return undefined;
  const s = Math.floor(Number(sec));
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return (d ? d + ' 天 ' : '') + h + ' 小时 ' + m + ' 分';
}

export function bytes(n) {
  if (n === undefined || n === null) return undefined;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}

export function pct(ratio) {
  if (ratio === undefined || ratio === null) return undefined;
  return (Number(ratio) * 100).toFixed(1) + '%';
}

// portSecText：接口端口安全白名单的只读摘要（决策 #389）。白名单非空即启用；
// 未配置（空/缺省）回 undefined ⇒ 该行如实显示「—」（与同页其它未配置项一致）。
export function portSecText(ps) {
  if (!Array.isArray(ps) || ps.length === 0) return undefined;
  return ps.length + ' 条：' + ps.join(' ') + '（白名单外源 MAC 丢弃）';
}

// stormControlText：接口入向风暴抑制的只读摘要（决策 #385）。只列已配置的类
// （字段缺省＝该类未配置）；两类都没配时回 undefined ⇒ 该行如实显示「—」
// （与同页「入向/出向限速策略」等未配置项一致），不编造「0 kbps」。
export function stormControlText(sc) {
  if (!sc || typeof sc !== 'object') return undefined;
  const parts = [];
  if (sc.broadcast_kbps) parts.push('广播 ' + sc.broadcast_kbps + ' kbps');
  if (sc.multicast_kbps) parts.push('组播 ' + sc.multicast_kbps + ' kbps');
  return parts.length ? parts.join(' / ') + '（入向，超速丢弃）' : undefined;
}

export function mb(n) {
  if (n === undefined || n === null) return undefined;
  return Number(n) >= 1024 ? (Number(n) / 1024).toFixed(1) + ' GB' : n + ' MB';
}

// ifaceDropsText：接口丢弃计数的**统一口径渲染**（决策 #504）。
//
// 两条数据面给的东西不一样：VPP 的 stats segment 只有 `/interfaces/<if>/drops` 一个
// **方向无关**的总数，内核网络数据面的 sysfs 才有 `rx_dropped`/`tx_dropped` 两个真值。
// 故同一个单元格在两处各按自己的口径显示，**不适用的键在响应里缺席**（不是 0）：
//   - 有 `drops`（VPP）⇒ 「总数 N」；
//   - 有 `rx_drops`/`tx_drops`（内核）⇒ 「收 X / 发 Y」；
//   - 都没有（取不到计数）⇒ undefined ⇒ 显示「—」。
export function ifaceDropsText(st) {
  if (!st || typeof st !== 'object') return undefined;
  if (st.drops !== undefined && st.drops !== null) return '总数 ' + st.drops;
  if (st.rx_drops !== undefined || st.tx_drops !== undefined) {
    return '收 ' + dash(st.rx_drops) + ' / 发 ' + dash(st.tx_drops);
  }
  return undefined;
}

// ifaceDropsCell：概览接口卡的单列「丢弃」值——口径同上，只取当前数据面发的那一个数。
// VPP 下是方向无关总数；内核下是收+发两个方向（分开写，不相加冒充总数）。
export function ifaceDropsCell(st) {
  if (!st || typeof st !== 'object') return undefined;
  if (st.drops !== undefined && st.drops !== null) return st.drops;
  if (st.rx_drops !== undefined || st.tx_drops !== undefined) {
    return dash(st.rx_drops) + ' / ' + dash(st.tx_drops);
  }
  return undefined;
}

// **记录类**绝对时间的统一渲染（形状与 CLI 对齐）：`2026-09-25 01:24:22 UTC`。
//
// 为什么一律按 UTC 并带显式标记：控制台的设计目标之一是「与 CLI 工单对照」（确认框里就回显
// 等价的命令行语句），而审计/告警/归档这类时间还要与 `journalctl`（UTC）核对——三者必须同一
// 口径。此前用浏览器本地时区渲染（`toLocaleString()`），同一台机上控制台会比 `show log audit`
// 差一个时区偏移、且不带任何标记，对不上账（round80 缺陷）。
// 故：**记录类**时间一律 UTC（审计、告警、事件、提交历史、会话、镜像/虚拟机创建、归档、
// 抓包开始、证书有效期……凡是这里渲染的都是记录时间）；只有**纯 UI 提示**（说明"这张卡刚
// 被点过"、与任何记录无关）才允许用本地时间，且必须经下面的 fmtClockHint()，写明理由。
//
// 输入可能是 RFC3339（带 `Z` 或 `±hh:mm` 偏移）、服务端已格式化的字符串，或本来就不是时间：
//   · 不带时区但形状是 `YYYY-MM-DD[ T]HH:MM:SS` 的字符串：服务端的时间戳都是 UTC，按 UTC
//     字面量渲染——交给 Date 解析会按**浏览器本地时区**理解，等于平白多出一个偏移；
//   · 其余（带时区的 RFC3339、数字、Date 对象）交给 Date 的原有语义；
//   · 解析失败（非时间）：原样返回（保持既有容错，不抛异常）。
export const FMT_TIME_NAIVE_RE = /^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})(?:\.\d+)?$/;

export function fmtTime(ts) {
  if (!ts) return '—';
  const naive = typeof ts === 'string'
    ? FMT_TIME_NAIVE_RE.exec(ts.trim().replace(/\s+UTC$/, '')) : null;
  const d = naive ? new Date(naive[1] + 'T' + naive[2] + 'Z') : new Date(ts);
  return isNaN(d) ? String(ts) : d.toISOString().slice(0, 19).replace('T', ' ') + ' UTC';
}

// 纯 UI 提示用的**墙上时钟**（本地时区，故意不加 UTC 标记）。
// 只用于说明"这张卡片刚刚被刷新过"这类与记录无关的提示：它是操作者自己刚点的动作，
// 与操作者本机时钟对照最直观，与 CLI/journalctl 的记账时间无关（那些一律走 fmtTime()）。
// 本仓库对"本地时间"的纪律：**只有经本函数**才允许——结构守护会拒绝别处出现本地时区格式化。
export function fmtClockHint(date) {
  return date.toLocaleTimeString();
}

// fmtTimeOpt：fmtTime 的「可缺席」变体（会话条目用）——零值时间（例如只有在途的待确认提交、
// 没有持锁会话）不是真实时间，显示「—」而不是 0001 年。真实时间一律交给 fmtTime()（UTC + 标记）。
export function fmtTimeOpt(ts) {
  if (!ts) return undefined;
  const d = new Date(ts);
  if (isNaN(d) || d.getFullYear() < 1970) return undefined;
  return fmtTime(ts);
}

// fmtBytes 人类可读字节（format-data 结果统计里的释放空间；与 bytes 的差别只是历史沿用的
// 两个实现，合并属清理项，不在本次重构里改行为）。
export function fmtBytes(n) {
  const v = Number(n);
  if (!isFinite(v) || v <= 0) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, x = v;
  while (x >= 1024 && i < u.length - 1) { x /= 1024; i++; }
  return (i === 0 ? x : x.toFixed(1)) + ' ' + u[i];
}
