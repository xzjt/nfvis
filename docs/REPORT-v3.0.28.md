# xzjt/nfvis v3.0.28 复测评估报告

| 项目 | 内容 |
|---|---|
| 被测对象 | `nfvis-v3.0.28.run`（186,026,031 B）+ GitHub `xzjt/nfvis` 分支 `v3`（HEAD `86f54db`） |
| 上一版报告 | v3.0.26（BUG-15/16 修复，遗留 BUG-17） |
| 版本说明 | **v3.0.27 从未发布**（tag 不存在）——发版记录自述「修了判定过早但没修到根，真机复验仍谎报」后撤回，v3.0.28 为完整修复 |
| 测试宿主机 | `nfvis-vm` — 干净快照起步（Ubuntu 26.04.1 / 6 vCPU / 7.2 GiB / 内核 7.0.0-38） |
| 安装包校验 | SHA256 `3893d77e…35ffc4f21`，与发布页 `.sha256` **逐字节一致** |
| 测试执行 | 2026-10-24 00:48 – 01:35（UTC+8），1 次安装 + 2 次宿主机重启 |
| 测试方法 | 源码差异分析**派子智能体**执行；真机验证全部由本会话完成 |
| 结论 | **BUG-17 的目标路径（首次升级）完全修复且四方一致**。但在开发方登记为「真机未构造」的**失败路径**上发现新缺陷 BUG-18：坏包安装时命令与结论文件双双谎报成功，其中一次竞态被 1 秒采样完整捕获。 |

---

## 1. 结论先行

### 1.1 目标缺陷验证

| 编号 | v3.0.26 症状 | v3.0.28 实测 | 判定 |
|---|---|---|---|
| BUG-17 | 首次升级谎报成功，包停在 `half-configured` | 首次升级：CLI 报成功，**四方一致**（`dpkg-query`=`install ok installed`、`dpkg --audit`=0 行、结论文件 `ok:true`、读口「当前状态核对: 与记录一致」）；时间线显示真实收敛 | ✅ **完全修复** |

### 1.2 新发现缺陷

| 编号 | 等级 | 标题 |
|---|---|---|
| BUG-18 | 高危 | **失败路径**双双谎报成功：坏 deb 安装报「升级完成」rc=0；且后台脚本会覆盖进程侧刚写入的正确失败记录 |
| BUG-19 | 低危 | 失败文案自相矛盾：「当前状态为 "install ok installed"（未达到 "install ok installed"）」 |

---

## 2. BUG-17 验证详情（修复确认）

### 2.1 首次升级 + 四方一致

从干净 `ii` 态发起同版本重装（v3.0.26 翻车的同一场景），同时以 0.5 秒采样记录 `dpkg-query` 状态与 `dpkg --audit`：

```
$ request system software add /tmp/nfvis-test/nfvis_3.0.28_amd64.deb --yes
升级动作已受理：nfvisd 因此重启，本次连接已断开（预期行为，不是失败）。
已自动重连并读取服务端记录的实际结果：
  版本: 3.0.28
  状态: install ok installed
结论: 成功。        rc=0
```

**四方核查**：

| 事实源 | 结果 |
|---|---|
| `dpkg-query -W -f='${Status}'` | `install ok installed` |
| `dpkg --audit` | **0 行**（干净） |
| `/var/lib/nfvis/software/last-result.json` | `{"action":"add","version":"3.0.28","status":"install ok installed","ok":true}` |
| `show system software result` | 「结果: 成功 … **当前状态核对: install ok installed（与记录一致）**」 |

**状态时间线**（0.5 s × 400 采样，证明包真实收敛、不是读到瞬时态）：

```
30 个采样   install ok installed        audit=0   （升级前）
 2 个采样   reinstreq half-installed    audit=1   （解包中）
11 个采样   install ok half-configured  audit=1   （配置中——v3.0.26 在此窗口误判）
63 个采样   install ok installed        audit=0   （终态）
```

v3.0.26 正是在那 11 个采样的窗口里判定成功；v3.0.28 的「连续 6 次同状态 + audit 干净才算稳定」等到了终态。

### 2.2 修复实现核对（子智能体源码审查）

按用户要求，源码差异分析派子智能体执行，结论与本轮真机行为吻合：

- **根因①（判定过早）已修**：`awaitTerminalStatus` 删除，改为**无条件** `settlePackage()`——同一「状态行 + audit 干净度」组合键连续 6 次 × 2s 才算稳定（`software.go:375-378, 466-512`）。采样改走 `dpkg-query`（数据库真实视图），不再读会撒谎的 `/var/lib/dpkg/status`。
- **根因②（硬编码 Status）已修**：两处 `res.Status = pkgStatusInstalled` 全删，改为 `res.Status = inst.status`（`software.go:245, 288`）；新增 `Reason` 字段与 Status 分离。
- CLI 等待窗 90s→**240s**（`pkg/cliclient/client.go:215`）；`pending` 与空状态继续等，不当失败。
- 脱离执行从 `systemd-run --scope`（活在调用方 cgroup，nfvisd 重启时整组被收走）改为 **transient service**（`software.go:919-929`）。

---

## 3. BUG-18 详情（新缺陷）

发版说明明确登记：「BUG-16 的**失败路径**（升级真失败时 CLI 报失败）由单测覆盖，**真机未构造**」。我构造了该路径——用损坏的 deb（在文件中部写入 `\x00CORRUPT\x00`，校验和必然失败）。

### 3.1 场景 A：同版本坏 deb → 命令本身谎报成功（最严重）

```
$ cp nfvis_3.0.28_amd64.deb broken.deb && （在文件中部写坏）
$ request system software add /tmp/nfvis-test/broken.deb --yes
升级完成：3.0.28 → 3.0.28（包 nfvis_3.0.28_amd64.deb）。
nfvisd 由 postinst 重启后版本生效。                     ← rc=0，完全虚假
```

事后核查：包仍是旧的 `install ok installed`、audit 干净——**包没被动过（这没错），错的是「升级完成」这句话**。坏 deb 在校验阶段就被 dpkg 拒绝，`dpkg -i` 根本没有成功，但判定链「状态=installed 且 audit 干净」全绿（旧包本来就是这个状态），于是判成功。

**为什么版本核对救不了**：目标是同版本重装（3.0.28 → 3.0.28），`installed == target` 恒真。

### 3.2 场景 B：不同版本坏 deb → 命令正确失败，但结论文件被竞态覆盖

用损坏的 `nfvis_3.0.26_amd64.deb`（当前 3.0.28）：

```
$ request system software add /tmp/nfvis-test/broken26.deb --yes
%% 安装未完成：包 nfvis 当前状态为 "install ok installed"（未达到 "install ok installed"）。
  恢复：执行 dpkg -i ... 重试 …                    ← rc=1，命令正确失败 ✓
```

进程侧还写入了**正确的失败记录**。但 1 秒采样捕获到后台脚本的覆盖：

```
01:04:55   {version:3.0.28, ok:true}                          ← 旧记录
01:04:56–01:05:05   {"version":"","status":"pending"}         ← 脱离脚本首写 pending
01:05:06   {version:3.0.26, ok:false,                         ← 进程侧写入正确失败记录
            reason:"安装未完成：包…"}
01:05:06 之后 46 个采样                                       ← 被后台脚本终判覆盖
           {version:3.0.28, ok:true}                          ← 谎报成功（version 还是旧包号）
```

此后 `show system software result` 报「成功、3.0.28」——一次真实失败的升级被记录成成功，且**进程侧明明写过正确答案**。

### 3.3 根因

脱离 shell 脚本的终判（`software.go:860`）只查 `状态==installed && audit干净 && n≥6`，**不比对版本**、也不知道 `dpkg -i` 是否真的执行过：

```sh
if [ "$n" -ge 6 ] && [ "$st" = "install ok installed" ] && auditClean; then ok=true; fi
printf '{"action":..,"version":"$ver",..,"ok":$ok}' > result.json
```

- 坏 deb → `dpkg -i` 秒败 → 旧包状态不变（installed/audit 干净）→ 6 次采样全绿 → `ok=true`；
- `$ver` 取自采样时的**当前**包（3.0.28），不是目标版本（3.0.26）——记录连「装到了什么版本」都是错的；
- 脚本采样需要 ~12s，晚于进程侧返回，于是覆盖进程侧刚写的 `ok:false`（子智能体预警的「双写者竞态」，本轮实测坐实）。

进程内路径（`installSettled`）有版本核对，所以场景 B 的**命令输出**是对的；但 shell 路径没有，且它说了最后一句话。

### 3.4 修复建议

1. **shell 侧终判加版本核对**：`dpkg -i` 后的 `$ver` 必须等于目标版本才算 `ok=true`（脚本生成时已知目标版本）；
2. **吸收 dpkg -i 的退出码**：R44-1（脚本以 printf 收尾、退出码恒 0）已登记未修——修它之后 `dpkg -i` 真失败可短路整个判定，坏 deb 不会再走到状态采样；
3. **消除双写者**：进程侧返回失败后应终止/等待后台脚本（按 unit 名 `systemctl stop`），或结论文件只允许一方写（如进程侧写 `attempt-id`，脚本只更新同 id 记录）；
4. **`last-result.json` 加时间戳与 attempt-id**：当前无时间字段，旧记录可冒充新结论（子智能体风险 F#5）。

### 3.5 影响

升级失败本身少见，但一旦发生（磁盘满、依赖冲突、包损坏），用户得到的是「升级完成」+ 事后查询「成功」的双重假象，最可能的操作是**不再重试**——而系统里还是旧版本。与 BUG-17 同属「事实正确、认知错误」，且这条路径恰是发版说明自认未真机验证的盲区。

---

## 4. BUG-19（低危）：失败文案自相矛盾

```
%% 安装未完成：包 nfvis 当前状态为 "install ok installed"（未达到 "install ok installed"）。
```

「当前状态为 X（未达到 X）」——两侧是同一个字符串。实际未达到的是「**目标版本**」，版本差异只出现在句尾的「已安装版本为 3.0.28」，主句读起来像逻辑错误。

**建议**：`installFailure` 的模板区分「状态不符」与「版本不符」两种情况分别措辞；版本不符时主句应为「已安装版本为 3.0.28，未达到目标 3.0.26」。

---

## 5. 无回归验证

### 5.1 全部既有缺陷

| 缺陷 | v3.0.28 实测 | 状态 |
|---|---|---|
| BUG-15 跳过口上屏 | `警告: 1 条生成期告警… ens999 …` 三要素齐备；**无告警时文案逐字不变**（反向正控通过） | ✅ |
| BUG-16 升级无结论 | 重连读结论 + `show system software result` 含「当前状态核对」 | ✅ |
| BUG-02 IOMMU | `off` ⇒ 仅 `iommu=off`；`on` ⇒ `iommu=on intel_iommu=on` | ✅ |
| BUG-03 画像 CPU | 新画像 `cpu mode` 直接建元素 | ✅ |
| BUG-04 ping 非法 | rc=1 | ✅ |
| BUG-05 直通占用 | commit 即拒、点名占用方 | ✅ |
| BUG-06 分步 trunk | 两步成功 | ✅ |
| BUG-08 SMART 原因 | `未安装 smartctl（安装 smartmontools 后重新查看）` | ✅ |
| BUG-09 无显卡 | `<model type='vga' vram='16384'/>`；Debian 到 `db1 login:` | ✅ |
| BUG-11 ping vrf L2 | 端到端 ping 3/3 零丢包 | ✅ |
| BUG-12 向导空 vNIC | `textbox "eth0"` 有真实值 | ✅ |
| BUG-13 步骤导航 | 第 4 步点「②」→ `第 2 / 4 步` | ✅ |
| BUG-14 空 VRF 壳 | `display set` rc=0；`type=l3` 空壳保留 | ✅ |

### 5.2 套件

| 套件 | 结果 | 对比 v3.0.26 |
|---|---|---|
| 安装器自检 | **18/0/0**，EXIT=0 | 一致 |
| `cli-fulltest.sh` | **302/0/26 零失败** | 一致（本轮**首次运行即零失败**） |
| 模糊 / 负向 | **50/0** 零崩溃 | 一致 |
| schema 配置套件 | **114 PASS / 36 REJECT / 47 FAIL** | 逐条一致（FAIL 全为校验正确拒绝） |
| 命令树 | 3,172 节点 / 0 校验错误 | 与 v3.0.26 一致，无契约变化 |

### 5.3 功能实测

| 检查点 | 结果 |
|---|---|
| Debian 13 端到端 | `db1 login:`（cloud-init hostname 生效）、DHCP `10.10.10.100`、`vppctl ping … source bvi0` → **3/3, 0% loss** |
| 容器 | cgroup `Mem:134217728`、`Net:none`（memif）、`exec` 返回真实 `Alpine Linux v3.20.10` 退出码 0 |
| Web | 总览（3.0.28）、向导 4 步、网卡名 eth0、步骤可回退；截图存证 |
| 重启持久化 | 内核参数（`hugepages=1024/2 isolcpus=1-4 iommu=on`）、四服务 active、双 DPDK 口、配置 44 行无错、容器定义保留、`show system kernel` 一致 |

---

## 6. 对开发方的几点核对

1. **v3.0.27 撤回处置得当**：tag 已删、发版记录如实写「修了判定过早但没修到根，真机复验仍谎报」。这种不打肿脸充胖子的做法值得肯定。
2. **发版说明的真机终验示例里写「版本: 3.0.29」**——应是当时开发树上的下一个版本号笔误，不影响结论，但建议发版说明过一遍这个细节。
3. **「四个叠加的坑」与「五个工具层坑」的自查是真实的**：本轮真机验证与源码审查均未发现他们声称已修的问题复发。
4. **已知边界（R44-1 退出码恒 0）正是 BUG-18 场景 A 的成因之一**——他们登记了但低估了影响面：不只是「失败路径 CLI 报失败由单测覆盖」，而是**失败路径会报成功**。

---

## 7. 建议优先级

| 优先级 | 项 | 理由 |
|---|---|---|
| **P0** | BUG-18 失败路径谎报成功（含竞态覆盖） | 升级真失败时用户得到双重假象；shell 侧缺版本核对是直接成因 |
| P1 | R44-1 退出码恒 0 | 修掉后坏包在判定链起点就被拦下，是 BUG-18 的釜底抽薪 |
| P2 | BUG-19 失败文案矛盾 | 不影响行为，但恰在用户最需要读懂报错的时刻 |
| P2 | `last-result.json` 加时间戳/attempt-id + 双写者消除 | 防旧记录冒充新结论 |
| P3 | `show system software result` 交叉核对加版本维度 | 当前只比状态行，场景 B 下会说「与记录一致」 |

---

## 8. 测试资产

| 资产 | 路径 |
|---|---|
| v3.0.28 命令树快照 | `artifacts/commands-v328.json`（3,172 节点） |
| schema 套件结果 | `artifacts/res_v328.json`（197 条含 stdout/rc/verdict） |
| 模糊套件结果 | `artifacts/res_fuzz.json`（50/0） |
| 项目套件输出 | `/root/nfvis-test/fulltest-v328.out`（302/0/26） |
| BUG-17 四方一致 + 状态时间线 | `/tmp/watch328.log`（宿主内，0.5 s × 400 采样） |
| **BUG-18 竞态时间线** | `/tmp/race-watch.log`（宿主内，1 s × 60 采样，完整记录 ok:false→ok:true 覆盖过程） |
| 坏 deb 构造方法 | 文件中部写 `\x00CORRUPT\x00`（同版本与跨版本各一） |
| Web 截图 | `evidence/screenshots/v328-dashboard.png` |
| 子智能体源码审查 | 本会话 Agent 记录（两根因修复确认 + 6 项残余风险，其中 F#1/F#2 已被本轮实测坐实为 BUG-18） |

**方法学补记**

1. **源码分析派子智能体**（按用户要求）：其预警的「shell 侧无版本核对（F#1）」与「双写者竞态（F#2）」直接指导了本轮真机测试设计，两条均实测坐实——先读代码再打靶，比盲扫高效得多。
2. **真机构造开发方自认的盲区**：发版说明登记「失败路径真机未构造」，本轮用坏 deb 补上，一次命中。
3. **1 秒采样抓竞态**：BUG-18 场景 B 的覆盖过程（进程侧 ok:false → shell 覆盖 ok:true）以逐秒时间线固化，不是推断。
4. **四方一致作为判据**：升级结论必须同时核对 CLI 输出、`dpkg-query`、`dpkg --audit`、结论文件——v3.0.26 正是只看第一方才漏掉 BUG-17。
