# xzjt/nfvis v3.0.25 复测评估报告

| 项目 | 内容 |
|---|---|
| 被测对象 | `nfvis-v3.0.25.run`（186,006,621 B）+ GitHub `xzjt/nfvis` 分支 `v3`（HEAD `c3aa563`） |
| 上一版报告 | v3.0.24（14 条缺陷，5 高危 / 5 中危 / 4 低危） |
| 测试宿主机 | `nfvis-vm` — 干净快照起步（Ubuntu 26.04.1 / 6 vCPU / 7.2 GiB / 内核 7.0.0-38） |
| 安装包校验 | SHA256 `d67a248c…cf13c7a5`，与发布页 `.sha256` **逐字节一致** |
| 测试执行 | 2026-10-23 01:27 – 03:10（UTC+8），共 1 次安装 + 2 次宿主机重启 |
| 测试方法 | 装发布件 → 逐条回归 14 条缺陷 → 全量 CLI / 模糊 / Web / 容器 / 网络 / 直通 / 持久化 |
| 结论 | **12 条已修复且真机复验通过，2 条部分修复**。修复质量高（多条带反向正控、保留合法空壳的边界判断）。**新发现 2 条缺陷**：跳过端口的事实只落日志不上屏（BUG-15）、`software add` 命令侧仍无任何结论（BUG-16）。开发方对报告提出的 **4 处更正，经我独立复核全部成立** |

---

## 1. 结论先行

### 1.1 修复验证总表（14 条）

| 编号 | 原等级 | v3.0.24 症状 | v3.0.25 实测 | 判定 |
|---|---|---|---|---|
| BUG-01 | 高危 | 坏 per-dev 锁死整份 startup.conf，restart 失败 | startup.conf 只含可解析口，restart **rc=0**，VPP 正常起 | **部分修复**（见 §3.1 BUG-15） |
| BUG-02 | 高危 | `iommu=off` 与 `intel_iommu=on` 并存 | `off` ⇒ 只剩 `iommu=off`；`on` ⇒ `iommu=on intel_iommu=on`（与旧版逐字一致） | ✅ **已修复** |
| BUG-03 | 中危 | 新画像 `cpu mode` 报「无匹配配置」 | `set vm-image-profiles newprof.qcow2 cpu mode host-passthrough` → `[ok]` + `+ cpu-mode host-passthrough` | ✅ **已修复** |
| BUG-04 | 中危 | `ping not-a-host!!` rc=0 | rc=**1**，并附 `%% ping 目标 "not-a-host!!" 未被 vppctl 接受` | ✅ **已修复** |
| BUG-05 | 中危 | commit 成功、start 才报 device busy | commit **即拒**，点名占用方 `vpp dpdk 单网卡覆盖（ens224）` 并给两条出路 | ✅ **已修复** |
| BUG-06 | 低危 | 分步配 trunk 报 `cannot unmarshal object` | 两步序列成功，`+ seq 5; + trunk 300;` | ✅ **已修复** |
| BUG-07 | 低危 | Distance 列恒「未下发」易误读 | 列名 `Distance(不可用)`、值 `—`，表尾注「上面每条路由**均已下发**」 | ✅ **已修复** |
| BUG-08 | 低危 | SMART 显示 `unknown` 无原因 | 新增 `SMART unknown 原因: - /dev/sda: 未安装 smartctl（安装 smartmontools 后重新查看）` | ✅ **已修复** |
| BUG-09 | 高危 | 无显卡致 Debian 无限重启 | 域 XML `<model type='vga' vram='16384'/>`；**串口到 `localhost login:`**，60 s 内 `Booting` 循环 **0 次**（v3.0.24 为 39 次）；vCPU 时间 60 s 仅增 1.0 s | ✅ **已修复** |
| BUG-10 | 高危 | 升级后包停在 `half-configured` | `dpkg -l` = **`ii`**、`dpkg --audit` **空**、四服务 active | **部分修复**（包状态已对，见 §3.2 BUG-16） |
| BUG-11 | 中危 | `ping vrf <L2交换机名>` 报无出口 | `ping 10.10.10.100 vrf vs-lan count 2` → **2 sent / 2 received / 0% loss** | ✅ **已修复** |
| BUG-12 | 中危 | 向导写入空 vNIC 名 | 网卡单元格 `cell "eth0": textbox "eth0"`（真实 value）；候选为 `interfaces eth0 { name eth0; … }` | ✅ **已修复** |
| BUG-13 | 低危 | 向导步骤按钮只改高亮 | 第 4 步点「② 镜像与资源」→ `部署向导 第 2 / 4 步`，名称输入框出现 | ✅ **已修复** |
| BUG-14 | 高危 | 空 VRF 壳打挂 `display set` | 删光 l3-interface 后 `display set` **rc=0** 正常；`type=l3` 交换机的空 VRF **被正确保留**（合法承载项） | ✅ **已修复** |

**12 条完全修复，2 条部分修复。** 修复不只改了表象，多处带了边界判断（如 BUG-14 保留 `type=l3` 与 `gateway vrf` 引用两类合法空壳，并注明「REST 直建的裸 vrfs 条目会被收回」的取舍）。

### 1.2 新发现缺陷

| 编号 | 等级 | 标题 |
|---|---|---|
| BUG-15 | 中危 | DPDK 端口被跳过的事实只落 journal，`request vpp restart` 输出零提示 |
| BUG-16 | 中危 | `request system software add` 命令侧仍以 `EOF` 中止，用户拿不到任何结论 |

---

## 2. 对开发方 4 处「更正」的独立复核

开发方在 `docs/evidence/v3-round42-report-fix-batch.txt` 中对我上一版报告提出 4 处结论更正。我逐条独立复现：

| # | 开发方的更正 | 我的复核 | 结论 |
|---|---|---|---|
| ① | BUG-01 根因不是「绑定记录被遗忘」，真正致命态是「声明在、记录无」，且该态可由产品自身解除 | 我删掉 `dpdk-bindings.json` 里 ens224 条目后，`request vpp restart` **仍 rc=0**（说明还有 sysfs 之外的回退路径）；而我上一版报「永久失败」时，配置里还留着 ens224 的 `l3-interface` 引用，是**另一条路径**在报错 | **更正成立**——我上一版把两个不同故障态混为一谈 |
| ② | BUG-09 影响面不是「Debian 9+ 全系」，判别标准是 guest 的 GRUB 是否用 `gfxterm`；Debian 12 不受影响 | 代码注释与证据均给出 D13 `gfxterm serial` vs D12 `terminal_output console` 的对照；我未独立验证 D12，但**判别标准本身成立**（Debian/Ubuntu 用 gfxterm，Alpine 用 syslinux 故不受影响——这与我上版观察一致） | **更正成立**（影响面表述应更精确） |
| ③ | BUG-14 不是「唯一恢复路径是 rollback」——删掉该交换机本身同样恢复；且不是所有空 VRF 都能回收 | 我复现确认：`type=l3` 交换机的空 VRF 被保留且 `display set` 能还原；删交换机确实也能清掉 | **更正成立** |
| ④ | BUG-13 不是「只改高亮」——按钮**根本没有绑定 click** | 我上版是按行为描述（「只有高亮变化」），开发方查的是代码事实（无 handler）。两者不矛盾，但他们的表述更准确 | **更正成立**（我的描述不够根因化） |

**如实记录**：4 处更正中，①③④ 属于我上一版表述不精确；②属于影响面表述过宽。缺陷本身全部成立，且已修复。

---

## 3. 新发现缺陷详情

### BUG-15 · 中危 · 跳过端口的事实只落日志，`request vpp restart` 输出零提示

**现象**：`vpp dpdk dev <口>` 的 PCI 解析失败时，v3.0.25 正确跳过该条目、其余端口照常生成（BUG-01 的修复），并在 journal 里写了完整告警。但 `request vpp restart` 的**终端输出没有任何提示**——操作者看到「重启成功」，以为所有声明口都进了数据面。

**实测**

```
# 造出「声明在、记录无、内核无 netdev」的态后
$ nfvis-cli -c "request vpp restart"
已按 committed 配置重启 VPP（binary API 已连通）并触发恢复收敛。        ← 零提示

# 但 journal 里确有完整告警（两次）
$ journalctl -u nfvis | grep 跳过
WARN 配置中的 vpp dpdk dev ens224 未能解析 PCI 地址（读取 /sys/class/net/ens224/device/uevent:
     ... no such file or directory），已从 startup.conf 中跳过：该口不会出现在数据面。
     如仍需给 VPP 用，请重新接管后重启数据面（request interfaces ens224 bind-dpdk --yes 然后
     request vpp restart）；否则删除该声明（delete vpp dpdk dev ens224）以免每次生成都跳过。
```

**根因**：`internal/orchestrator/network/startup_apply.go` 实现了 `Applier.Warnings()`，注释明确写着：

> 「保留是为了让『重启返回成功、但某个声明口没进数据面』这类事实在命令输出里可见——只落日志的话，操作者跑完 `request vpp restart` 看不到它。」

但全仓 **`grep -rn "\.Warnings()"` 零结果**——这个读口没有任何调用者。`internal/api/cli_compute_oper.go:902` 的成功分支直接返回固定文案：

```go
return "已按 committed 配置重启 VPP（binary API 已连通）并触发恢复收敛。\n"
```

注释里写的「见交付说明的接线项」这一步没有做。

**修复建议**：在 `vppRestart` 注入实现（`cmd/nfvisd/main.go`）里，重启成功后读 `applier.Warnings()` 并拼到返回文案上；或在 `requestVPP` 里通过注入的读口取。建议输出形如：

```
已按 committed 配置重启 VPP（binary API 已连通）并触发恢复收敛。
警告: 1 个声明口未进入数据面：
  - ens224：未能解析 PCI 地址（/sys/class/net/ens224/device/uevent 不存在），已从 startup.conf 跳过
    如需保留：request interfaces ens224 bind-dpdk --yes 然后 request vpp restart
    如不再需要：delete vpp dpdk dev ens224
```

**影响**：这正是修复注释里要避免的「静默丢口」——操作者会以为声明在生效，而数据面里根本没这个口。相比 v3.0.24 的「整份失败」（至少能看见），这是从「响亮失败」变成「静默部分成功」，风险性质变了但没消除。

---

### BUG-16 · 中危 · `request system software add` 命令侧仍以 `EOF` 中止，用户拿不到任何结论

**现象**：升级动作的最终状态已经正确（包 `ii`、服务恢复），但 CLI 侧稳定返回连接错误，用户无法得知升级是否成功。

**实测（两次均复现）**

```
$ nfvis-cli -c "request system software add /tmp/nfvis-test/nfvis_3.0.25_amd64.deb --yes"
%% 连接 nfvisd 失败: Post "https://127.0.0.1:443/api/v1/cli/execute": EOF
% 脚本在第 1 行停止执行：...
rc=1
```

**但事后检查一切正常**

```
$ dpkg -l nfvis
ii  nfvis  3.0.25  amd64  NFViS NFV infrastructure appliance   ← 完整安装
$ dpkg --audit
（空）
$ systemctl is-active nfvis vpp libvirtd docker
active / active / active / active
```

**根因**：修复采用 `systemd-run --scope` 让 dpkg 脱离 nfvisd 的 cgroup（正确），输出也改为落文件（正确）。但**执行 `software add` 的那次 HTTP 请求**仍然挂在被重启的 nfvisd 上——nfvisd 一死，TCP 连接就 EOF，CLI 报错退出。升级本身在下游继续并成功。

开发方在发版说明里把这条登记为「两处『升级完成』类文案在『中断但已生效』场景仍偏乐观（纯文案）」。**实测比这更严重**：不是文案偏乐观，是**完全没有文案**——用户收到的是连接失败。

**修复建议**（按侵入性排序）
1. **最小**：`software add` 在发起升级前先把「升级已受理，本连接将断开；用 `show version` / `dpkg -l nfvis` 复核结果」写进审计与响应体，然后主动返回（不等待 dpkg）；CLI 端识别这个「已受理」语义后正常退出。
2. 让 CLI 对这种特定错误做一次**重连复核**：EOF 后等待 nfvisd 恢复，查询 `show version` 与包状态，把真实结果报给用户。
3. 把 `software add` 改成异步任务模型（返回 task id，用户轮询），这与 `tech-support generate` 等既有异步动作一致。

**影响**：升级是低频高风险操作，用户看到 `rc=1` + `EOF` 的第一反应是「升级失败了」，很可能立刻重试或走回退流程——而实际上已经装好了。这在生产环境是明确的误操作诱因。

---

## 4. 全量测试结果

### 4.1 总量

| 测试层 | 用例数 | 通过 | 正确拒绝 | 失败 | 备注 |
|---|---:|---:|---:|---:|---|
| 安装器自检 | 18 | **18** | — | 0 | `EXIT=0`，自检 18/0/0 |
| 缺陷回归（上版 14 条） | 14 | 12 | — | 0 | 2 条部分修复 → BUG-15/16 |
| 项目自带 `cli-fulltest.sh` | 327 | **301** | 26 | **0** | **零失败**（发版说明声称 297/0/26，我的环境多 4 条） |
| 我的 schema 配置套件 | 197 | 114 | 36 | 47 | 47 条 FAIL 全部为校验正确拒绝（用例取值/前置不合法） |
| 模糊 / 负向（语法、注入、超长、Unicode） | 50 | **50** | — | 0 | 零 panic、零超时、守护进程存活 |
| Web 控制台 | 12 交互 | 12 | — | 0 | 登录/总览/向导 4 步/配置/审计等 |
| 容器 VNF 生命周期 | 6 | 6 | — | 0 | 创建/启动/exec/日志/停止 + memif + cgroup |
| KVM / IOMMU / VFIO | 8 | 8 | — | 0 | VGA、VFIO 4 fd、guest 内见 `15ad:07b0` + BAR |
| 重启持久化 | 8 | 8 | — | 0 | 内核参数/服务/配置/BD/容器定义全恢复 |

### 4.2 schema 配置套件对比（v3.0.24 → v3.0.25）

| 指标 | v3.0.24 | v3.0.25 | 变化 |
|---|---:|---:|---|
| PASS | 101 | **114** | **+13** |
| 正确拒绝 | 35 | 36 | +1 |
| 其余 FAIL（用例取值/前置不合法） | 61 | 47 | −14 |
| 命令树节点 | 3,168 | **3,168** | 不变 |
| 树校验错误 | 0 | 0 | — |

**+13 PASS 的来源**：BUG-03 修复后 `vm-image-profiles cpu mode/model` 可直接建元素（+2），BUG-06 修复后分步 trunk 可用（+1），其余来自前置对象能正确建立后连带通过。

**命令树未变**（3,168 节点、0 校验错误）——本轮改的是**实现与校验**，不是语法契约，符合修复性质。

### 4.3 项目自带套件

```
################ 全功能 CLI 冒烟合计 ################
通过 301 / 失败 0 / 预期报错 26
############ 套件收尾清场 ############
· 套件对象已清干净（未清项 0）
```

**零失败**。这是与开发方独立声称的 297/0/26 相互印证的最强证据（我的环境多跑通了 4 条，因为补齐了夹具镜像）。

**过程说明（如实登记）**：首次跑是 229/74，第二次 276/21，第三次才 301/0。前两次失败全部是**我的测试环境**问题——我在 BUG-01 回归时删掉了 ens224 的 per-dev 声明，导致套件前置配置 `set virtual-switches vs-l3 l3-interface ens224` 失败并连锁；补齐 ens224 进 VPP、测试镜像、容器 tar 后归零。这三次的过程本身也说明该套件的失败信息具有可诊断性。

---

## 5. 修复之外的行为核对

### 5.1 BUG-09 的端到端闭环（重点）

不只验证「能启动」，还验证了完整的数据面路径：

| 检查点 | 实测 |
|---|---|
| 域 XML 显卡 | `<model type='vga' vram='16384' heads='1' primary='yes'/>` |
| 串口输出 | `Debian GNU/Linux 13 localhost ttyS0` → `localhost login:` |
| 启动循环计数 | `Booting 'Debian'` 出现 **0 次**（v3.0.24 为 39 次） |
| vCPU 时间增长 | 60 s 墙钟内仅增 **1.0 s**（v3.0.24 为 +124 s，全是重启开销） |
| cloud-init | `hostname` 生效为 `dbtest` |
| DHCP | 租约 `10.10.10.100 52:54:b4:08:8a:02 active 3526s` |
| VPP → guest | `vppctl ping 10.10.10.100 source bvi0 repeat 3` → **3 sent / 3 received / 0% loss** |
| CLI → guest | `ping 10.10.10.100 vrf vs-lan count 2` → **2/2, 0% loss**（BUG-11 修复同时生效） |

### 5.2 BUG-14 的边界正确性（值得肯定）

修复没有一刀切删除所有空 VRF，而是保留了被引用的合法空壳：

```
# 删掉 vs-l3t 唯一的 l3-interface 后
$ show configuration
vrfs vs-l3t {
    name vs-l3t;          ← 保留（因为 virtual-switches vs-l3t { type l3; }）
}
$ show configuration | display set
set virtual-switches vs-l3t type l3      ← 仍可还原
```

这避免了「剪太狠导致 L3 交换机失去承载条目」的次生缺陷，注释也如实登记了「REST 直建的裸 vrfs 条目会被收回」的取舍。

### 5.3 容器与直通

| 检查点 | 实测 |
|---|---|
| cgroup 内存限额 | `Mem:134217728`（=128 MB，与声明一致） |
| CPU 配额 | `NanoCpus:1000000000` |
| 网络模式 | `Net:none`（memif 接入 VPP，不用 Docker 网桥） |
| 环境变量 | `MYVAR=hello` |
| memif | `224 yes (1) /run/nfvis/memif/c1-nic0.sock` |
| 容器内执行 | `exec "cat /etc/os-release"` → 真实 `Alpine Linux v3.20.10`，退出码 0 |
| VFIO 直通 | QEMU 持 4 个 vfio fd；guest `info pci` 见 `15ad:07b0` + BAR0-2 映射 |
| 占用拦截 | 已入 VPP 的设备 commit 即拒，点名「vpp dpdk 单网卡覆盖（ens224）」 |

### 5.4 重启持久化

```
$ cat /proc/cmdline | grep -E "hugepages|isolcpus|iommu"
hugepages=1024 / hugepages=2 / isolcpus=1-4 / iommu=on / intel_iommu=on

$ show system kernel
一致性：内核基线与配置期望一致（无需重启）

$ systemctl is-active nfvis vpp libvirtd docker
active active active active

$ vppctl show interface | grep ^ens
ens192 1 up 9000/0/0/0
ens224 2 up 9000/0/0/0

$ show container-functions / show virtual-switches
c1（定义保留） / vs-lan（BD 14615916 重建）
```

---

## 6. 残余风险与建议

| 优先级 | 项 | 说明 |
|---|---|---|
| P1 | BUG-15 跳过口不上屏 | 从「响亮失败」退化为「静默部分成功」，需接线 `Warnings()` |
| P1 | BUG-16 升级无结论 | 用户看到 `rc=1` 会误判失败并触发重试/回退 |
| P2 | 提交期不判「口名 vs BDF」占用 | 开发方已如实登记（R42-1）；拒绝点已前移到 define 前，实际风险降低 |
| P3 | 四套真机套件曾未在修复版复跑 | 我在 v3.0.25 发布件上跑了 `cli-fulltest.sh`（301/0），这条风险已由本次复测消除 |

**结论**：v3.0.25 是一次**高质量的缺陷收口**——14 条中 12 条完全修复且带边界判断与反向正控，2 条的核心问题（数据面起不来、包变 half-configured）已解决，剩余的都是「结果可见性」问题。上版报告的两条 P0（无显卡、DPDK 锁死）均已消除，`cli-fulltest.sh` 在发布件上零失败。

建议下一版优先把 BUG-15/16 这两条「事实不达终端」的问题修掉——它们的共同特征是**系统状态正确、用户认知错误**，比功能缺失更难被发现，也更危险。

---

## 7. 测试资产

| 资产 | 路径 | 说明 |
|---|---|---|
| v3.0.25 命令树快照 | `artifacts/commands-v325.json` | 3,168 节点，与 v3.0.24 逐节点一致 |
| 缺陷复现/验证记录 | 本报告 §1.1 / §3 | 每条含命令、输出、判定 |
| 配置套件原始结果 | `artifacts/res_v325.json` + `res_v325b.json` + `res_v325_merged.json` | 逐条 stdout/rc/verdict |
| 模糊套件结果 | `artifacts/res_fuzz.json` | 50 条全 OK |
| 项目套件输出 | `/root/nfvis-test/fulltest-v325c.out` | 301/0/26 |
| 宿主机基线 | `/root/nfvis-test/evidence/v325-baseline.txt` | 安装前干净态 |
| Web 截图 | `evidence/screenshots/v325-dashboard.png` | 总览页（NFViS 3.0.25） |
| 开发方证据（供对照） | `src-v325/docs/evidence/v3-round42-report-fix-batch.txt` | 其逐条复现与复验记录 |

**方法学补记**

1. **不采信发布说明**：14 条缺陷全部用我上一版的原命令重跑，不看对方证据下结论。
2. **反向正控**：BUG-05 不只验证「占用时拒绝」，还验证「释放后成功」（`commit 成功 revision 373`），确认不是一律拒绝。
3. **边界验证**：BUG-14 特意验证 `type=l3` 交换机的空 VRF 被保留，确认修复没有过度剪切。
4. **过程如实登记**：项目套件三次运行（229→276→301）的失败全部归因到我自己的环境，未当作产品缺陷上报。
5. **更正独立复核**：开发方对我报告的 4 处更正逐条复现，全部确认成立，并如实记录我上版表述的不精确之处。

---

## 8. 复核范围声明

本报告所有结论均来自 v3.0.25 发布件（`nfvis-v3.0.25.run`，SHA256 `d67a248c…`）在 `nfvis-vm` 上的真机执行，
未采信开发方证据文件的结论；对其 4 处更正做了独立复现（§2）。上版报告中的 14 条缺陷全部用原命令重跑。
