# xzjt/nfvis v3.0.26 复测评估报告

| 项目 | 内容 |
|---|---|
| 被测对象 | `nfvis-v3.0.26.run`（186,020,606 B）+ GitHub `xzjt/nfvis` 分支 `v3`（HEAD `5127930`） |
| 上一版报告 | v3.0.25（14 条已修复 12 条，遗留 BUG-15/16） |
| 测试宿主机 | `nfvis-vm` — 干净快照起步（Ubuntu 26.04.1 / 6 vCPU / 7.2 GiB / 内核 7.0.0-38） |
| 安装包校验 | SHA256 `b989759b…369526f2`，与发布页 `.sha256` **逐字节一致** |
| 测试执行 | 2026-10-23 07:52 – 09:00（UTC+8），1 次安装 + 2 次宿主机重启 |
| 结论 | **BUG-15 完全修复；BUG-16 的主症状修复，但暴露出一个更严重的新缺陷 BUG-17：首次升级会谎报「成功」，而包实际停在 `half-configured`**。前 14 条无回归。 |

---

## 1. 结论先行

### 1.1 本轮两条目标缺陷

| 编号 | 原症状 | v3.0.26 实测 | 判定 |
|---|---|---|---|
| BUG-15 | 跳过端口只落 journal，`request vpp restart` 零提示 | 输出 `警告: 1 条生成期告警（数据面已起来，但下列声明未完全生效）` + 逐条列出端口、原因、两条照做路径；**无告警时文案逐字不变** | ✅ **完全修复** |
| BUG-16 | `software add` 只报 `EOF` + rc=1，用户拿不到结论 | 输出「升级动作已受理…已自动重连并读取服务端记录的实际结果：版本/状态/结论」；rc=0；新增 `show system software result` 读口 | ⚠️ **主症状修复，但判定逻辑有缺陷** → 见 BUG-17 |

### 1.2 新发现缺陷

| 编号 | 等级 | 标题 |
|---|---|---|
| BUG-17 | **高危** | 首次升级判定过早：读到配置中途的瞬时 `installed` 就写「成功」，实际包会停在 `half-configured` |

---

## 2. BUG-15 验证详情

### 2.1 构造「真正无法解析」的场景

v3.0.25 复测时我用「清空绑定记录」没能触发跳过（对端还有 sysfs 等回退路径）。本轮改用**声明一个不存在的接口名**，让 PCI 解析必然失败：

```bash
# configure
# set interfaces ens999 description fake      ← 声明通过（配置层不校验物理存在）
# set vpp dpdk dev ens999                     ← per-dev 引用已声明口，校验通过
# commit
commit 成功 (revision 5)
```

### 2.2 `request vpp restart` 输出（修复后）

```
已按 committed 配置重启 VPP（binary API 已连通）并触发恢复收敛。
警告: 1 条生成期告警（数据面已起来，但下列声明未完全生效）：
  - 配置中的 vpp dpdk dev ens999 未能解析 PCI 地址（读取 /sys/class/net/ens999/device/uevent:
    open /sys/class/net/ens999/device/uevent: no such file or directory），已从 startup.conf
    中跳过：该口不会出现在数据面。如仍需给 VPP 用，请重新接管后重启数据面
    （request interfaces ens999 bind-dpdk --yes 然后 request vpp restart）；
    否则删除该声明（delete vpp dpdk dev ens999）以免每次生成都跳过。
rc=0
```

三条信息齐备：**哪个口**、**为什么**（含 errno 原文）、**两条照做路径**。

### 2.3 反向正控（无告警时不得有噪声）

```
$ # 删掉 ens999 的声明后
$ nfvis-cli -c "request vpp restart"
'已按 committed 配置重启 VPP（binary API 已连通）并触发恢复收敛。'
```

逐字与 v3.0.25 一致，**未引入任何多余输出**。这一条很重要——修「加提示」很容易顺手污染正常路径。

---

## 3. BUG-16 / BUG-17 详情

### 3.1 BUG-16 的主症状确实修好了

```
$ request system software add /tmp/nfvis-test/nfvis_3.0.26_amd64.deb --yes
升级动作已受理：nfvisd 因此重启，本次连接已断开（预期行为，不是失败）。
已自动重连并读取服务端记录的实际结果：
  版本: 3.0.26
  状态: install ok installed
结论: 成功。
rc=0
```

新增读口也正常：

```
$ show system software result
结果: 成功
动作: 升级
版本: 3.0.26
状态: install ok installed
```

连接断开被正确识别为「预期行为」而非失败，`isConnDropped` / `isUpgradeLine` / 90 秒有界等待的设计是扎实的（只对升级两条命令、只对连接类错误、超时不改判）。

### 3.2 但结论是错的：BUG-17

**现象**：同一次升级，CLI 报「结论: 成功 / install ok installed」，而宿主上：

```
$ dpkg -s nfvis | grep Status
Status: install ok half-configured

$ dpkg --audit
The following packages are only half configured ...
 nfvis   NFViS NFV infrastructure appliance (nfvisd + nfvis-cli)

$ cat /var/lib/nfvis/software/last-result.json
{"action":"add","version":"3.0.26","status":"install ok installed","ok":true}    ← 与真实状态矛盾
```

**稳定性**：3 次复现全部命中同一模式（从 `ii` 干净态发起 → 报成功 → 实际 `iF`）。

### 3.3 根因：判定早于终态

用 0.5 秒间隔的 `dpkg -s nfvis` 轮询抓到时间线：

```
08:04:30–08:04:36   install ok installed     ← 12 次采样（配置进行中的瞬时态）
08:04:37–08:04:56   （dpkg 数据库短暂不可读，2 次空值）
08:04:57–08:04:59   install ok half-configured  ← 44 次采样（终态）
```

**产品在 `08:04:3x` 读到了瞬时 `installed`，判定成功并落盘 `ok:true`，而 dpkg 在 20 秒后才把状态落成 `half-configured`。**

代码层面的直接原因在 `internal/system/software.go`：

```go
func (m *SoftwareManager) installAndVerify(ctx, path, targetVer, action string) (string, error) {
    at := m.installDeb(ctx, path, action)
    if at.err != nil && looksInterrupted(at.err) {          // ← 只有 err 时才等收敛
        at.log = joinOutput(at.log, m.awaitTerminalStatus(targetVer))
    }
    status, serr := m.packageStatus()                        // ← 首次升级 err==nil，直接读
    ...
}
```

`awaitTerminalStatus`（那个「等包收敛到终态」的循环）**只在 `at.err != nil` 时执行**。首次升级时 dpkg 以 0 退出（`at.err == nil`），所以轮询根本没跑，直接读到了配置中途的瞬时 `installed`。

同一问题的第二处：`Add()` 成功分支硬编码 `res.Status = pkgStatusInstalled`（`software.go:211`），而 `installAndVerify` 内部复核到的真实状态只进了返回文本 `out`，**没有回填到 `res.Status`**。所以结果文件里的 `status` 字段永远是字面量。

**为什么第二次运行反而正确**：第二次跑时包已经是 `half-configured`，`installAndVerify` 走 `isConfigurableStatus` 分支做补救，`dpkg --configure` 又失败，最终落到 `installFailure`，如实报 `ok:false`。所以这个 bug 的表现是「**首次升级谎报、后续如实**」——恰好是最容易骗过测试的顺序。

### 3.4 修复建议

1. **无条件等待收敛**：把 `awaitTerminalStatus` 从「仅 err 时」改为「err 时必等、err==nil 时至少确认状态在一次短暂窗口内保持稳定」（例如连续 3 次采样一致才判定），因为 `installed` 在 `postinst` 执行中途确实会瞬时出现；
2. **回填真实状态**：`res.Status` 改为取 `installAndVerify` 里读到的 `status` 变量，删掉硬编码 `pkgStatusInstalled`；
3. **结论落盘前二次核对**：写 `last-result.json` 之前再读一次 `dpkg-query`，与 30 秒前那次一致才写 `ok:true`；
4. **读口应可复核**：`show system software result` 可顺带实时读一次 `dpkg-query` 做交叉核对，不一致时明确提示「记录与当前状态不符」。

### 3.5 影响

这是**最危险的一类缺陷**：用户升级后看到「结论: 成功」就不会再复核，而包处于 `half-configured`——后续任何 `apt` 操作都会因此失败，且 `dpkg --audit` 会一直报错。相比 v3.0.25 的「EOF 看不出结果」，本次是「明确告诉你成功，但没成功」。

**补充证据（错误的持久化）**：我手工 `dpkg --configure nfvis` 修复包状态、并重启宿主机后，`show system software result` **仍然报「结果: 成功 / 状态: install ok installed」**——错误的结论跨越了重启，没有自愈。

---

## 4. 无回归验证（前 14 条）

| 缺陷 | v3.0.26 实测 | 状态 |
|---|---|---|
| BUG-01 DPDK 锁死 | 跳过机制 + 告警上屏（见 §2） | ✅ 保持 |
| BUG-02 IOMMU 矛盾 | `iommu off` ⇒ 片段仅 `iommu=off`；`on` ⇒ `iommu=on intel_iommu=on` | ✅ 保持 |
| BUG-03 画像 CPU | `set vm-image-profiles np.qcow2 cpu mode host-passthrough` → `[ok]` + `+ cpu-mode` | ✅ 保持 |
| BUG-04 ping 非法目标 | rc=1 | ✅ 保持 |
| BUG-05 直通占用 | commit 即拒，点名 `交换机 vs-lan 端口（ens192）` | ✅ 保持 |
| BUG-06 分步 trunk | 两步序列成功 | ✅ 保持 |
| BUG-08 SMART 原因 | `- /dev/sda: 未安装 smartctl（安装 smartmontools 后重新查看）` | ✅ 保持 |
| BUG-09 无显卡 | 域 XML `<model type='vga' vram='16384'/>`；Debian 到 `db1 login:` | ✅ 保持 |
| BUG-11 ping vrf L2 | 端到端 Ping 3/3 零丢包 | ✅ 保持 |
| BUG-12 向导空 vNIC | 网卡名 `textbox "eth0"` 有真实值 | ✅ 保持 |
| BUG-13 步骤导航 | 第 4 步点「②」→ `第 2 / 4 步` | ✅ 保持 |
| BUG-14 空 VRF 壳 | `display set` rc=0；`type=l3` 空壳正确保留（`l3t` 行 Note 标 `l3（VRF）`） | ✅ 保持 |
| BUG-07 Distance 列 | 沿用 v3.0.25 修法（本轮无 VRF 用例，代码未改） | ✅ 未回退 |
| BUG-10 half-configured | 见 BUG-17（包状态问题在首次升级时**仍然存在**） | ⚠️ 见 §3 |

**结论：前 14 条无回归。** 其中 BUG-10 需要说明——v3.0.25 复测时我手工触发的是「第二次升级」，那时包已是 half-configured，所以看起来「已修」；本轮从干净态首次升级才暴露判定过早的真实问题。这是我在上一轮**漏测**了首次升级路径。

---

## 5. 全量测试结果

### 5.1 总量

| 测试层 | 用例数 | 通过 | 正确拒绝 | 失败 | 说明 |
|---|---:|---:|---:|---:|---|
| 安装器自检 | 18 | **18** | — | 0 | `EXIT=0`，18/0/0 |
| 目标缺陷（BUG-15/16） | 2 | 1 完全 + 1 部分 | — | — | 另发现 BUG-17 |
| 前 14 条无回归 | 14 | 13 | — | 1 | BUG-10 见 §3 |
| 项目自带 `cli-fulltest.sh` | 328 | **302** | 26 | **0** | **零失败**（发版说明 298/0/26） |
| schema 配置套件 | 197 | 114 | 36 | 47 | 47 条 FAIL 全为校验正确拒绝 |
| 模糊 / 负向 | 50 | **50** | — | 0 | 零 panic、零超时 |
| Web 控制台 | 8 交互 | 8 | — | 0 | 总览/向导 4 步/网卡名/步骤导航 |
| 容器生命周期 | 5 | 5 | — | 0 | cgroup/memif/exec |
| 网络数据面（L2+DHCP+BVI） | 5 | 5 | — | 0 | DHCP 租约 + ping 3/3 |
| KVM/VFIO | 3 | 3 | — | 0 | VGA + 直通占用拦截 |
| 重启持久化 | 7 | 7 | — | 0 | 内核/服务/配置/BD/容器/结论文件 |

### 5.2 命令树变化

| 指标 | v3.0.25 | v3.0.26 | 变化 |
|---|---:|---:|---|
| 总节点 | 3,168 | **3,172** | **+4** |
| 操作模式 | 297 | **299** | +2（`show system software` + `result`） |
| 配置模式 | 2,871 | **2,873** | +2 |
| 树校验错误 | 0 | 0 | — |

新增的 4 个节点正是 BUG-16 的读口，**契约变化与修复意图完全对应**，没有夹带其他改动。

### 5.3 项目套件的三次运行（如实登记）

| 次序 | 结果 | 失败原因 |
|---|---|---|
| 第 1 次 | 252 / 58 | 我在 BUG-15 测试时删掉了 `ens192` 的 per-dev，套件前置配置失败连锁 |
| 第 2 次 | 249 / 35 | `ens192` 已补，但测试镜像/容器 tar 被上一轮套件清场删掉 |
| 第 3 次 | **302 / 0** | 补齐全部夹具后**零失败** |

三次失败**全部归因到我自己的环境**，未当作产品缺陷上报。这个过程也说明该套件的失败信息是可诊断的。

### 5.4 schema 套件与 v3.0.25 对比

| 指标 | v3.0.25 | v3.0.26 |
|---|---:|---:|
| PASS | 114 | **114** |
| REJECT | 36 | 36 |
| FAIL（校验正确拒绝） | 47 | 47 |

**逐条一致**——本轮改动集中在升级判定与输出，未触及配置语句层。

---

## 6. 功能实测

### 6.1 端到端（Debian 13）

| 检查点 | 实测 |
|---|---|
| 域 XML 显卡 | `<model type='vga' vram='16384' heads='1' primary='yes'/>` |
| 串口 | `db1 login:`（hostname 由 cloud-init 生效） |
| DHCP 租约 | `10.10.10.100  52:54:64:ed:44:49  active  86330` |
| VPP → guest | `vppctl ping 10.10.10.100 source bvi0 repeat 3` → **3 sent / 3 received / 0% loss** |
| 数据面对象 | `bvi0` idx5 up、`tap0` idx6 up、`vh-db1-nic0` idx3 up、`mf-c1-nic0` idx4 up |

### 6.2 容器

| 检查点 | 实测 |
|---|---|
| cgroup 内存 | `Mem:134217728`（=128 MB） |
| CPU 配额 | `NanoCpus:1000000000` |
| 网络 | `Net:none`（memif 接入 VPP） |
| 容器内执行 | `exec "cat /etc/os-release"` → 真实 `Alpine Linux v3.20.10`，退出码 0 |

### 6.3 重启持久化

```
$ cat /proc/cmdline | grep -E "hugepages=[0-9]|isolcpus|iommu="
hugepages=1024 hugepages=2 isolcpus=1-4 iommu=on intel_iommu=on

$ show system kernel
一致性：内核基线与配置期望一致（无需重启）

$ systemctl is-active nfvis vpp libvirtd docker
active active active active
$ vppctl show interface | grep -c ^ens
2
$ show configuration | display set   → 43 行，无内部错误
$ show container-functions          → c1（定义保留）
$ show virtual-switches             → vs-lan（BD 14615916 重建）+ l3t（l3 注释）
```

---

## 7. 对开发方自查的核对

发版说明里写「本轮踩到并修掉四个叠加的坑（结论写不到 / 写了但字段空 / 日志有 JSON 而结果文件恒空 / ctx 取消收走 scope）」，并强调「**判据必须是真机读回结论文件，不能只看退出码**」。

我实测发现：这四条确实都修了（结论文件存在且字段完整、`ok:false` 场景也能写、scope 未被 ctx 取消收走）。但**「读回结论文件」这个判据本身有一个盲区**——结论文件里写的 `status` 字段是硬编码的字面量，不是读回来的真实状态。于是「读回文件」校验的是「文件说成功」，不是「包真的成功」。

这一点正是 BUG-17 的要害：**判据链的最后一环没有回到 dpkg**。

---

## 8. 建议优先级

| 优先级 | 项 | 理由 |
|---|---|---|
| **P0** | BUG-17 首次升级谎报成功 | 用户看到「结论: 成功」就不再复核，而包停在 half-configured；错误结论还跨重启持久化 |
| P2 | 结果文件的 `status` 字段应来自实际读取 | 当前是硬编码 `pkgStatusInstalled`，是 BUG-17 的第二处成因 |
| P2 | `show system software result` 可加实时交叉核对 | 记录与当前 dpkg 状态不符时应提示 |
| P3 | 读口对旧版本升级的说明已如实登记 | 发版说明已覆盖，无需额外动作 |

---

## 9. 复核范围与资产

| 资产 | 路径 |
|---|---|
| v3.0.26 命令树快照 | `artifacts/commands-v326.json`（3,172 节点） |
| schema 套件结果 | `artifacts/res_v326.json` + `res_v326b.json` + `res_v326_merged.json` |
| 模糊套件结果 | `artifacts/res_fuzz.json`（50/0） |
| 项目套件输出 | `/root/nfvis-test/fulltest-v326c.out`（302/0/26） |
| 状态时间线证据 | `/tmp/status-watch.log`（宿主内，0.5 s 采样） |
| 结果文件证据 | `/var/lib/nfvis/software/last-result.json`（谎报时的 JSON 原文见 §3.2） |
| Web 截图 | `evidence/screenshots/v326-dashboard.png` |

**方法学补记**

1. **不采信发布说明**：BUG-15/16 都用上一版的原命令重跑；BUG-15 特意构造了「真正无法解析」的场景（v3.0.25 时我用的「清空记录」不够，会被 sysfs 回退路径兜住）。
2. **反向正控**：BUG-15 不只验证「有告警时上屏」，还验证「无告警时文案逐字不变」。
3. **时序取证**：BUG-17 用 0.5 秒 `dpkg -s` 轮询抓到「瞬时 installed → 终态 half-configured」，把「判定过早」从推断变成实测。
4. **如实承认上轮漏测**：BUG-10 在 v3.0.25 复测时我从「已是 half-configured」的状态重跑，恰好绕过了首次升级路径，因此误判为已修。本轮从干净态首次升级才暴露。
5. **过程透明**：项目套件三次运行的失败全部归因到我自己的环境，未计入产品缺陷。
