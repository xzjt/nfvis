# 历史时序存储 v1 · 设计（决策 #356，收口 `docs/v2待做.md` 三.6）

> 立项来源：`docs/NFViS-功能差距分析.md` §3-7「历史时序数据——`/metrics` 是即时快照、无时序存储；
> Web 总览 5s 轮询同样没有趋势图数据底座，容量规划与事后回溯无从谈起」。
> 本文件是**规模评估 + 设计口径**；契约（openapi / 命令树 / 命令全表）与决策行见附录 A #356。

## 1. 规模评估（先评规模，再定选型）

**序列数**（本机实测口径，2026-10-03 对照 `/api/v1/metrics` 输出）：

| 来源 | 序列数（本机） | 随什么增长 |
|---|---|---|
| 主机（cpu/mem/hugepages/disk/uptime） | 10 | 固定 |
| VPP 聚合（线程/主堆三值/buffer 池两值 + 可用性） | 6 | buffer 池按 NUMA |
| VPP 逐接口（rx/tx 包·字节·错·丢） | 8 × N 口 | 接口数 N |
| 配置计数 | 5 | 固定 |
| VNF（逐对象 up + 可用性 + 聚合 running/vcpu） | 2 + 2×M | VNF 数 M |
| 告警计数 | 4 | 固定 |

本机（N=6 口、M=2）≈ **80 序列/次采样**。中等规模（N=24、M=20）≈ **220 序列**。

**数据量与磁盘**：

| 采样间隔 | 行/天（80 序列） | 字节/天（≈40B/行含索引） | 7 天 |
|---|---|---|---|
| 30s | 230k | ≈9 MB | ≈64 MB |
| **60s（默认）** | **115k** | **≈4.6 MB** | **≈32 MB** |
| 300s | 23k | ≈1 MB | ≈7 MB |

真机数据分区 **79 GB 可用**（`/` 与 `/var/lib/nfvis` 同卷，96G 用 13G）。即使 220 序列 × 30s × 365 天
也 ≈ 3.3 GB，占比 <5%。

**结论**：**SQLite 足够**，不需要外部 TSDB / 专用时序库（引入外部依赖与运维面，与本产品「单体 appliance」定位相悖）。
行宽由「维度表 + 窄事实表」压到 ≈40B/行（含索引），足以覆盖默认 7 天窗口。

## 2. 存储选型

- **引擎**：SQLite，驱动 `modernc.org/sqlite`（**既有依赖**，纯 Go，无 CGO；与配置库同款）。
- **库文件**：`/var/lib/nfvis/metrics.db`（**独立库**，与配置库 `nfvis.db` 分开）。理由：
  1. 不与事务提交/候选锁/`_txlock=immediate` 竞争；
  2. **不进配置备份/恢复语义**（备份的是配置，不是运行数据；恢复不该把历史数据一起倒回去）；
  3. 可独立裁剪/删除（`rm metrics.db` 即回到「无历史」，不影响配置）。
- **表结构**（维度表 + 窄事实表，避免每行重复 `name+labels`）：

```sql
PRAGMA user_version = 1;
CREATE TABLE IF NOT EXISTS series (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  name   TEXT NOT NULL,
  labels TEXT NOT NULL DEFAULT '{}',     -- 标签的**规范 JSON**（键排序；唯一约束用，避免 k=v 拼接歧义）
  UNIQUE(name, labels)
);
CREATE TABLE IF NOT EXISTS samples (
  series_id INTEGER NOT NULL,
  ts        INTEGER NOT NULL,          -- unix 秒（采样时刻）
  value     REAL    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_ts        ON samples(ts);
CREATE INDEX IF NOT EXISTS idx_samples_series_ts ON samples(series_id, ts);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

- `meta` 存采样器心跳（`last_tick_ts` / `last_error`），供读视图**如实**报告「采样是否在跑、上次何时」；
  不把心跳做成告警码（v1 只在读视图如实呈现，见 §6 边界）。
- 打开参数与配置库同口径：`?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate`，`MaxOpenConns(4)`。

## 3. 采集

- **同一采集路径**：把 `handleMetrics` 的取数体抽成 `Server.GatherSamples(ctx)`，**`/metrics` 与历史采样器共用**
  ⇒ 「能画出来的趋势 = 能 scrape 到的指标」天然一致（不做第二套清单）。
- **后台采样器**（`cmd/nfvisd`，进程生命周期 goroutine，启动**不阻塞 READY**——沿用 #348/#351/#354 纪律）：
  周期性 `GatherSamples` + 单事务批量写入；间隔取自 **committed 配置**（改配置**无需重启**即生效）。
- **失败如实**：单次采集部分来源不可用（如 VPP 未连）时，可得的序列照写、不可得的序列**当轮不写**
  （与 `/metrics` 同口径：宁缺不谎报）；整轮异常（**写库/事务失败**等）记入 `meta.last_error` 并按下一周期重试
  （决策 #365 口径更正：`GatherSamples` 本身不返回整轮错误，来源不可用由各序列自身缺项体现）。

## 4. 保留策略

- **时间窗**：默认 **7 天**，可配 1..365 天（`set system metrics history retention-days`）。
- **硬行顶**：2,000,000 行（≈80 MB），越顶按最旧裁剪——**双界**保证磁盘有界，与时间窗无关地兜底。
- **裁剪节奏**：每 5 分钟一次（`DELETE FROM samples WHERE ts < cutoff`，再按行顶裁剪，最后回收孤立 `series`）。
  写成功 ≠ 收敛：裁剪后**回读**（count/最旧 ts）并反映到读视图。
- **采集间隔**：默认 60s，可配 10..3600s（`set system metrics history interval`）。间隔越短分辨率越高、占用越大，
  读视图给出**实测**库大小、序列数/样本数与时间范围（决策 #365 口径更正：v1 **不做**「当前间隔 → 估算占用」
  的文字估算——按实测数值如实呈现，不编造推算值）。

## 5. 三面读视图（同源）

| 面 | 入口 | 内容 |
|---|---|---|
| CLI | `show system metrics history` | **概览**：存储状态（可用/不可用+原因）、采样间隔（生效值）、保留天数、库大小、序列数、样本数、时间范围（最旧/最新）、上次采样时刻与是否落后 |
| CLI | `show system metrics history name <metric> [last <duration>] [step <duration>]` | 某指标**各序列**（按标签分组）的时间点；`<metric>` 支持 `?`/Tab 动态补全（来源=存储内已知指标名） |
| REST | `GET /metrics/history?name=&last=&step=&since=&until=&limit=` | 同上数据（JSON）；**需鉴权**（Bearer） |
| Web | 「系统 · 历史趋势」页（`#/system/metrics`） | 指标选择器 + 窗口选择器 + 内联 SVG 折线（免构建），与 REST 同源 |

- **鉴权口径**：`/metrics` 保持**无鉴权**（Prometheus 抓取，契约 `security: []` 不变）；`/metrics/history` 是
  运维读视图（给控制台/CLI 用），**要求 Bearer**——抓取方不需要历史，操作者需要。
- **降采样**：`step` 由服务端分桶（同桶取**桶内最后一个**样本，与「瞬时值」语义一致），默认自动
  （目标 ≤120 点，**分桶不算裁剪**）；`truncated` 标记**因 `limit` 被裁剪**（保留最近的点；决策 #365 口径更正）。

## 6. 边界与不做（如实登记）

- **不做**：外部 TSDB、SNMP/流采样、告警阈值引擎、导出为文件、按指标开关采集、
  clickhouse/opentsdb 之类外部存储（均属 v3）。
- **不新增告警码**（v1）：采样停滞只在读视图以 `stale` 字段 + `meta.last_tick_ts` 如实呈现；
  「采样停滞告警」登记为后续（需要与告警对账口径一起设计）。
- **不含在备份/零化语义内**：`request system storage format-data` 会清数据分区（历史随配置库一并被清，
  这是既有语义）；`zeroize` 的处置沿用既有范围，不额外扩展。
- **精度如实**：历史点是**采样瞬间的瞬时值/累计值**，不是区间聚合；计数器（counter 类型）单调递增，
  速率需读侧自行差分——读视图不伪造 rate。
- **1G/2M 大页、VPP 不可用等既有边界**不因本特性改变。
