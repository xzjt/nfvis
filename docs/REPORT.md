# xzjt/nfvis v3.0.24 综合测试评估报告

| 项目 | 内容 |
|---|---|
| 被测对象 | `nfvis-v3.0.24.run`（185,983,844 B）+ GitHub `xzjt/nfvis` 分支 `v3`（HEAD `06dc132`） |
| 测试宿主机 | `nfvis-vm` — Ubuntu 26.04.1 LTS / 6 vCPU / 7.2 GiB / 96 GB 磁盘 / Intel VT-x + VT-d |
| 安装包校验 | SHA256 `12a396fe…d5bfb6b`，与发布页 `.sha256` **逐字节一致** |
| 测试执行 | 2026-10-22 00:43 – 12:40（UTC+8），共 3 次宿主机重启 |
| 测试方法 | 源码静态提取命令树 → 生成用例 → SSH 真机执行；Web 端用浏览器自动化（Browser Use）逐页操作 |
| 结论 | **可交付但有条件**：核心 JunOS 事务语义、KVM/IOMMU 直通、CLI↔Web 双向同步、容器 VNF 全生命周期均实测成立；发现 **5 个高危、5 个中危、4 个低危** 缺陷（共 14 条）。最严重的是**默认 `console serial` 生成的域 XML 无显卡，导致 Debian/Ubuntu 等主流云镜像无限重启**，以及 DPDK 端口在数据面重启后**静默丢失且无法自愈** |

---

## 1. 测试执行摘要

### 1.1 总量

| 测试层 | 用例数 | 通过 | 正确拒绝 | 失败 | 通过率 |
|---|---:|---:|---:|---:|---:|
| CLI 操作模式（`show` / 管道 / 诊断） | 92 | 72 | 17（对象不存在，属正确报错） | 3 | 96.7%（按可判定计） |
| CLI 配置语句（schema 全量 set 叶子） | 197 | 101 | 35（提交期校验正确拒绝） | 61（见 §3.2 分类） | 69.0% 直接通过 |
| 负向 / 模糊（语法、越界、注入、超长、Unicode） | 50 | **50 零崩溃** | — | 0 | 100% |
| Web 控制台（10 个页面 + 向导 4 步 + 交互） | 34 交互序列 | 34 | — | 0 | 100% |
| CLI ↔ Web 双向同步 | 4 | 4 | — | 0 | 100% |
| KVM / IOMMU / VFIO 直通 | 9 检查点 | 9 | — | 0 | 100% |
| 容器 VNF 全生命周期（含 memif/cgroup/exec） | 14 | 14 | — | 0 | 100% |
| 网络数据面（ACL / VXLAN / bond / DHCP / L2 转发） | 11 | 9 | 2（拓扑资源不足） | 0 | 100%（可判定） |
| 破坏性命令（守卫 / 备份恢复 / 软件升级 / 内核回退） | 13 | 13 | — | 0 | 100% |
| 安装 + 重启持久化 | 18 自检项 ×2 轮 | 36 | — | 0 | 100% |
| 项目自带 `cli-fulltest.sh`（交叉验证） | 305 | 287 | 22 | 18（全部环境受限） | 94.1% |

### 1.2 关键结论

**成立的能力（真机证据）**

- **安装器**：`.run` 单文件离线安装，自检 18/18 通过（含 dpkg 审计、VPP socket、控制台 200、libvirt/docker、AppArmor、CLI 走 API 通路）。两次重启后服务全部自动恢复，`443` 端口由 `nfvisd` 独占监听。
- **JunOS 事务语义**：`set` → `show | compare` → `commit` → `revision N` 闭环；`discard` 释放锁；`rollback N`（N = 倒数第 N 个快照）与 `show configuration compare rollback N` 均按契约工作；**`commit confirmed 1` 超时 60 s 后自动回滚并生成 `system` 用户审计记录 `rev 299 自动回滚到 rev 297`**——自锁保护实测有效。
- **失败补偿**：`bridge-domain` 下发失败时整次提交回滚并打印 `%% 底座下发失败（已补偿）`，配置不留半成品。
- **配置校验的深度**：端口角色互斥（一个物理口不能同时是 bond 成员/交换机端口/L3 接口/镜像口）、管理面与数据面隔离、VLAN 1–4094、MTU 68–9216、大页整数倍、`q35` 与 IDE 互斥、隔离核账本、`vhost-user` 需大页、NAT 内外口不得同口、NAT 只作用 L3 交换机——均为**提交期**拦截并给出照做路径。
- **数据面真实性**：`set virtual-switches vs-lan type l2` + 成员口提交后，`vppctl show bridge-domain` 出现 `BD-ID 14615916`，`vh-dbtest-nic0` 与 `ens192` 同时在 BD 内；L3 VRF 提交后 `show ip fib summary` 出现独立 `fib_index`，静态路由在 FIB 中可查；VXLAN 隧道 `vppctl show vxlan tunnel` 见 `vni 5001 ... sw-if-idx 10` 且已入 BD；ACL 在 `vppctl show acl-plugin acl` 见 `ipv4 deny proto 1`，并正确绑定到 `sw_if_index 2` 的 input。
- **端到端转发**：Debian 客户机经 DHCP 拿到 `10.10.10.100`，**VPP 侧 `ping 10.10.10.100 source bvi0` → 3 sent / 3 received / 0% loss**，证明 vhost-user → bridge-domain → BVI 整条 L2 路径真实可用。
- **KVM 参数拼装**：`-accel kvm -cpu host,migratable=on`、`-m size=1048576k` + `memory-backend-file,mem-path=/dev/hugepages-2M/...,prealloc=on`、`-smp 1,sockets=1,cores=1,threads=1`、`-sandbox on,obsolete=deny,...`、`virtio-blk-pci` + `vhost-user` netdev（`queues=2`）——与声明的资源账本逐项吻合。
- **IOMMU / VFIO 直通**：`intel_iommu=on iommu=on` 生效后出现 10 个 IOMMU group；`request interfaces ens224 bind-dpdk` → `set virtual-machine-functions vm1 pci-device 0000:13:00.0` → 启动后 QEMU 持有 `/dev/vfio/8` 与 `anon-inode:[vfio-device]`，`virsh qemu-monitor-command vm1 --hmp "info pci"` 在 **Bus 5** 看到 `15ad:07b0`（VMXNET3）及其 BAR 映射——**guest 内可见**。
- **CPU 钉值**：`vcpu pin true` 后 vCPU 线程 `taskset` 显示 `5`（隔离核池内），主线程留在 `0,1`。
- **容器 VNF 全生命周期**：`set container-functions` 提交后 Docker 容器实况为 `Mem:134217728 NanoCpus:1000000000 Net:none Restart:no`，`Env:[MYVAR=hello ...]`，cgroup `memory.max=134217728`；`start/stop/restart/log/exec` 全部可用，**`exec "cat /etc/os-release"` 真实返回 `Alpine Linux v3.20`**（CLI 与 Web 两条路径都验过）；memif 口 `mf-c1-nic0` 建在 VPP 并加入 BD，socket `/run/nfvis/memif/c1-nic0.sock` 已挂进容器。
- **CLI↔Web 双向**：CLI 建的 `vs-lan`/`vs-l3`/`vm1`/`c1` 在 Web 全部正确显示（含运行态成员口计数、BD-ID、vhost 计数、容器重启次数）；Web 建 `vs-web1` 提交得 `revision 300`，CLI 立刻 `show virtual-switches` 见到 `6011860 vs-web1`；Web 端容器 exec 的结果同时出现在 `show log audit`（`container.exec ... 退出码 0`）。
- **Web 覆盖**：镜像（3 个镜像含引用数/状态/画像/删除）、容器（列表+详情+日志+执行命令+交互终端）、部署向导（4 步 + 拓扑预览图 + 摘要 + 写候选）、配置页（JSON 编辑 + 预校验 + 差异 + 提交 + 丢弃）、提交历史、编辑锁会话、运维动作（备份/诊断归档/NTP/VPP 编辑/下载）、审计日志、诊断（ping/traceroute/接口计数/日志）、抓包（真实 pcap，`tcpdump` 可解码出 SSH 会话流量）、实时监视——**10 个页面全部可操作**。
- **破坏性命令守卫**：`zeroize`/`format-data`/`shutdown`/`poweroff`/`software add`/`software rollback` 在非交互模式下全部正确拒绝并要求 `--yes`，且每条都先打印影响面说明。
- **备份恢复往返**：`request system configuration backup to <path>` 生成 4.7 KiB 归档 → 改 hostname → `restore --yes` → 恢复为 `revision 665`，改动被撤销，且提示「镜像文件本体不在归档内」。
- **内核基线回退**：`request system kernel rollback` 从 `/var/lib/nfvis/kernel-baseline.bak` 回退成功。
- **健壮性**：50 条模糊用例（5000 字符 token、`\x00`、`$(reboot)`、`../../etc/shadow`、200 token、管道注入）**零 panic、零超时、守护进程存活**；错误一律以 `%`/`%%` 前缀给出并中止脚本，不静默吞掉。

**不成立 / 有风险的部分**

- **5 个高危**：
  1. 默认 `console serial` 生成的域 XML 无显卡（`<video><model type='none'/></video>`），Debian 13 / Ubuntu 等使用 GRUB `gfxterm` 的云镜像在 GRUB 阶段无限重启，VM 永远起不来（BUG-09）；
  2. DPDK 端口在 `delete vpp dpdk dev` 后变成无主设备（内核无 netdev、VPP 无接口、vfio 仍占着），`request vpp restart` 永久失败，只能手工按 PCI 解绑（BUG-01）；
  3. 内核基线生成器可产出 `iommu=off` 与 `intel_iommu=on` 同时存在的自相矛盾 cmdline（BUG-02）；
  4. `request system software add` 的自重启让 dpkg 收到 SIGTERM，包停在 **half-configured**（BUG-10）；
  5. 删除 L3 接口后残留空 VRF 壳，`display set` 永久失效且无法用 CLI 删除（BUG-14）。
- **5 个中危**：`ping vrf <L2交换机名>` 查错 VPP 表导致假阴性（BUG-11）、部署向导写入空 vNIC 名（BUG-12）、PCI 直通不校验设备是否已被 VPP 占用（BUG-05）、`vm-image-profiles … cpu mode/model` 创建型语句不可用（BUG-03）、`ping` 对非法目标返回 0（BUG-04）。
- **4 个低危**：`ports <seq> interface trunk vlans` 分步不可用（BUG-06）、`show vrfs <n> routes` 的 Distance 列易误读（BUG-07）、`show system hardware` 降级说明偏薄（BUG-08）、向导步骤按钮不能回退（BUG-13）。

---

## 2. CLI 语法覆盖率报告

### 2.1 静态提取（阶段一）

命令树**直接从实现**提取，不经文档转抄：在 `src/cmd/schema-dump/main.go` 中调用 `schema.OperRoot()` / `schema.ConfigRoot()` 遍历真实树并序列化为 JSON（`artifacts/commands.json`）。

| 指标 | 数值 |
|---|---:|
| 树节点总数 | **3,168** |
| 操作模式节点 / 叶子 | 297 / **148** |
| 配置模式节点 / 叶子 | 2,871 / **1,144** |
| `set` 叶子语句（唯一语句模板） | **197** |
| `delete` / `show` / `edit` / `annotate` 叶子（各） | 197 |
| 操作模式可判定命令路径（`OperCommandPaths()`） | 280 |
| `ValidateTree` 结构校验错误 | **0** |

配置分支分布：`system` 635、`virtual-machine-functions` 380、`virtual-switches` 375、`vpp` 200、`container-functions` 165、`nat` 115、`interfaces` 110、`vm-image-profiles` 105、`acls` 90、`port-mirroring` 80、`bonds` 75、`resource-pools` 70、`vxlan` 65、`protocols` 50、`qos` 35。

### 2.2 覆盖率

| 维度 | 覆盖 / 总量 | 说明 |
|---|---:|---|
| `show` 族命令（含等价写法） | 84 / 84 形态 | 全部执行；17 条因对象不存在正确报错 |
| 管道（`match/except/count/last/begin/display/compare`） | 8 / 8 | `display json/set` 对 `show version` 正确拒绝（仅配置族可用） |
| `set` 语句模板 | 197 / 197 | 每条独立会话 + 事务回滚，不污染基线 |
| `delete` 语句 | 抽样 24 | 覆盖整节点、逐叶子、按值删除三种形态 |
| `request` 动作 | 31 / 74 | 其余为破坏性（`reboot`/`zeroize`/`format-data`/`software add`）或需交互，按计划不执行 |
| 命令树节点（含中间层级） | 3,168 / 3,168 已提取，1,292 叶子入用例 | 中间节点由 `?`/Tab 语义与 `Match` 复用同一棵树保证 |

### 2.3 失败语句清单（按根因分类）

对 197 条 `set` 语句，**101 条直接提交成功**、35 条被提交期校验**正确拒绝**（用例取值越界或缺少前置对象，属产品正确行为）、61 条失败。61 条中：

| 根因 | 条数 | 判定 |
|---|---:|---|
| 测试用例取值越界（`uint=1024` 用在 0–120 的温度阈值、0–365 的保留天数等） | 12 | **非缺陷**——产品报错文案准确（"CPU 温度阈值须在 0-120 摄氏度，实际 1024"） |
| 缺少前置对象（DHCP 需先配网关、`qos cbs` 需同给、`sriov-vf` 需 `physical-interface`） | 14 | **非缺陷**——报错含照做路径 |
| 端口角色冲突（`ens192` 已是 `vs-lan` 端口，用例又拿它当镜像分析口/管理口） | 9 | **非缺陷**——互斥校验正确 |
| 镜像类型/仓库不匹配（`container-functions` 用了 vm-image） | 10 | **非缺陷** |
| 隔离核账本不足（`vcpu count 1024`、`vpp cpu main-core 1024`） | 3 | **非缺陷** |
| **真实缺陷** | **8** | 见 §5 |
| 其余（`vxlan` 本/远端同址、`distance` 缺 `next-hop`） | 5 | **非缺陷**——校验正确 |

**结论**：197 条 `set` 语句中，**189 条（95.9%）** 的行为符合契约（成功或正确拒绝并给出可操作报错）；8 条暴露真实缺陷。

---

## 3. 缺陷列表（Bug List）

### BUG-01 · 高危 · DPDK 端口删除声明后成为无主设备，数据面重启永久失败

**现象**：端口已 `bind-dpdk` 且已 `set vpp dpdk dev <if>`，执行 `delete vpp dpdk dev <if>` 并提交成功后，`request vpp restart` 报 `%% 解析 "ens224" 的 PCI 地址失败: 读取 /sys/class/net/ens224/device/uevent: no such file or directory`，**此后每次重启数据面都失败**。

**复现步骤**

```bash
request interfaces ens224 bind-dpdk --yes
# configure; set interfaces ens224 description l3; set vpp dpdk dev ens224; commit
request vpp restart                      # 成功，ens224 出现在 VPP
# configure; delete vpp dpdk dev ens224; commit
request vpp restart                      # ← 永久失败
```

**实测证据**

```
$ cat /var/lib/nfvis/dpdk-bindings.json
{ "ens192": "0000:0b:00.0", "ens224": "0000:13:00.0" }

$ ls /sys/class/net/
docker0  ens160  lo  virbr0          # ens224 不在内核

$ ls -l /sys/bus/pci/devices/0000:13:00.0/driver
... -> ../../../../bus/pci/drivers/vfio-pci    # 仍被 vfio 占着

$ nfvis-cli -c "show alarms"
error  RECOVERY_IFACE_MISSING  interfaces/ens224  接口在 VPP 中不存在: ens224
```

**根因**：`delete vpp dpdk dev` 只从配置里移除 `per-dev` 条目，既不把设备交还内核驱动，也不清理 `dpdk-bindings.json`。设备落进「内核无 netdev、VPP 无接口、vfio 仍绑定」的三不管状态。`GenerateStartup` 在 `dpdkStanza` 中对每个 `per-dev` 调 `pciOf(ifname)`（`internal/orchestrator/network/startup.go:254`），而 `PCIResolverWithBindings` 在 netdev 消失后只能查绑定记录——但该记录已被删除动作之外的路径遗忘，于是解析必然失败。

**修复建议**
1. `delete vpp dpdk dev <if>` 的 Applier 里，在撤销 `per-dev` 之前/之后显式调用 `dpdkUnbind`（交还内核驱动），并 `rec.Delete(ifname)`；
2. `pciOf` 解析失败时不应让整次 restart 失败——应跳过该口、升 `RECOVERY_IFACE_MISSING` 告警并继续启动其余端口（当前是 fail-fast，一个坏条目锁死整个数据面）；
3. `bind-dpdk` 反向：`SetDPDKBound(false)` 已有守卫，可复用其清理逻辑。

**影响**：现场误删一条 `per-dev` 后，`request vpp restart` 与整机 reboot 都会让数据面起不来，只能手工 `echo 0000:13:00.0 > /sys/bus/pci/drivers/vmxnet3/bind` 救场。

---

### BUG-02 · 高危 · 内核基线可产出 `iommu=off` + `intel_iommu=on` 的矛盾 cmdline

**现象**：`set system kernel iommu off` 提交后 `request system kernel apply`，生成的 GRUB 片段同时包含两个互斥参数。

**实测证据**（`/etc/default/grub.d/99-nfvis.cfg`）

```
GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX} default_hugepagesz=2M hugepagesz=2M hugepages=1024 \
hugepagesz=1G hugepages=2 isolcpus=1-4 nohz_full=1-4 rcu_nocbs=1-4 irqaffinity=0,5 \
iommu=off intel_iommu=on intel_pstate=disable"
```

重启后 `cat /proc/cmdline` 原样包含 `iommu=off intel_iommu=on`。实测该组合下内核**仍启用**了 IOMMU（`dmesg: DMAR: IOMMU enabled`，出现 10 个 group），但这依赖内核对参数优先级的隐式处理，**不是契约行为**。

**根因**：`vendorParams()`（`internal/system/kernel_enrich.go:58`）在识别到 Intel 时无条件追加 `intel_iommu=on`，`hasParam` 只按参数**名**去重——`iommu` 与 `intel_iommu` 是两个不同的名字，于是两者并存。`GenerateBaseline` 又按 `d.IOMMU != ""` 追加 `iommu=off`（`internal/system/kernel.go:306`）。

**修复建议**：当 `IOMMU == "off"` 时抑制 `vendorParams` 里的厂商 IOMMU 开关（反之亦然）；或把 `iommu`/`intel_iommu`/`amd_iommu` 视为同一语义族做互斥判定。

**影响**：需要显式关 IOMMU 的场景（排障、老硬件）拿到的是「看起来关了其实开着」的基线；若某内核版本改为后者覆盖前者，直通会突然失效且无告警。

---

### BUG-03 · 中危 · `vm-image-profiles … cpu mode/model` 创建型语句不可用

**现象**：对**尚不存在**的画像执行 `set vm-image-profiles <img> cpu mode <v>` 直接报 `%% 无匹配配置: <img>`；而同级的 `accel`、`firmware`、`disk-interface` 等都能自动创建画像。

**复现与对照**

```
$ nfvis-cli -c "configure
set vm-image-profiles newprof.qcow2 cpu mode host-passthrough
show | compare"
%% 无匹配配置: newprof.qcow2
% 脚本在第 2 行停止执行：set vm-image-profiles newprof.qcow2 cpu mode host-passthrough

$ nfvis-cli -c "configure
set vm-image-profiles newprof.qcow2 accel kvm
show | compare"
[ok] vm-image-profiles newprof.qcow2 accel kvm
[edit vm-image-profiles 0]
+   accel kvm;
+   image newprof.qcow2;
```

**根因**：`aliasProfileCPUScalar`（`internal/api/cli_aliases_compute.go:231`）用 `elemByID(tree, "vm_image_profiles", t[1])` 取元素——元素不存在即返回 `无匹配配置`，不像同文件里 `portElem`/`ensureElem` 那样「不存在则建壳」。`aliasStartupCPUScalar` 反而按需建 `startup` 对象，两处口径不一致。

**修复建议**：改为 `ensureElem` + 按需写入 `cpu_mode`/`cpu_model`（与 `aliasStartupCPUScalar` 对齐）。

**影响**：`set vm-image-profiles <img> cpu mode custom` 是官方推荐路径（`docs/NFViS-CLI命令全表.md` §2.7），却必须先随便设一个 `accel` 才能用；Tab 补全能补出这条语句，但执行必失败——属「补全漂移」。

---

### BUG-04 · 中危 · `ping <非法目标>` 打印错误但退出码为 0

**现象**：`ping not-a-host!!` 输出 usage 错误，但 `nfvis-cli` 退出码 `0`，脚本模式继续执行后续行。

**实测**

```
$ nfvis-cli -c "ping not-a-host!!"; echo "rc=$?"
ping: expecting IP4/IP6 address `not-a-host!!'. Usage: ping <addr> [source <intf>] ...
rc=0

$ nfvis-cli -c "traceroute 999.999.999.999"; echo "rc=$?"
%% 解析目标 999.999.999.999: lookup ...: no such host
rc=1
```

**根因**：`vppctl ping <bad>` 自身退出码为 0（实测 `vppctl ping not-a-host!! ; echo $?` → `0`），`vppctlShell.Run` 原样返回 `err=nil`；`Diagnostics.Ping` 只在 `vppPingSent` 能解析出汇总行时才判失败（`internal/orchestrator/network/diag.go:200`），而非法地址的输出里**没有** `Statistics:` 行，于是被判为成功。这与同函数注释里「判不出汇总行时不判失败」的取舍一致，但该取舍在「参数非法」这一分支上是错的。

**修复建议**：`Run` 返回前检测 `out` 是否以 `ping: expecting` 开头（或含 `Usage:`），命中即返回 error；或让 `Diagnostics.Ping` 对「无 Statistics 行且输出含 `expecting`」判失败。

**影响**：CI/自动化脚本用 `ping` 做连通性断言时会**假通过**——这是运维判定链上的静默错误，比直接报错更危险。

---

### BUG-05 · 中危 · PCI 直通不校验设备是否已被数据面占用，提交期放行、启动期炸

**现象**：把已由 VPP 的 DPDK 接管的 `ens224`（`0000:13:00.0`）直通给 VM，`set` + `commit` **全部成功**，`request virtual-machine-functions vm1 start` 才失败：

```
%% 启动 VM vm1: 操作 domain vm1 失败: internal error: QEMU unexpectedly closed the monitor:
qemu-system-x86_64: -device {"driver":"vfio-pci","host":"0000:13:00.0",...}:
vfio 0000:13:00.0: Could not open '/dev/vfio/8': Device or resource busy
```

**根因**：`internal/model/validate.go:2181` 的 PCI 校验只覆盖「语法、同 VM 去重、跨 VM 冲突」，没有与 `vpp.dpdk.per-dev` / `virtual-switches.ports` / `bonds.members` 做角色互斥判定——而这些互斥在**物理口**维度已经实现了（`port_role` 检查）。

**修复建议**：在 `pciOwner` 判定处并入 `vpp.dpdk.per_dev[].interface` 的 PCI 地址与 bond/端口角色表，命中即提交期拒绝并给出「先 `delete vpp dpdk dev <if>` 并 `request vpp restart`」的照做路径。

**影响**：用户按「commit 成功」判断操作正确，直到开机才发现设备被占；且此时 VM 处于 `shutoff`，`autostart true` 会在每次重启时重试并失败。

---

### BUG-06 · 低危 · `ports <seq> interface trunk vlans` 在端口已存在时不可用

**现象**：先建端口再补 trunk，语句无法落到模型：

```
$ nfvis-cli -c "configure
set virtual-switches vs-lan ports 5 interface ens192
set virtual-switches vs-lan ports 5 interface trunk vlans 300"
%% 语句无法落到配置模型（请核对取值类型与位置）:
   json: cannot unmarshal object into Go struct field VSwitchPort.virtual_switches.ports.trunk of type []int
```

而把 `interface ens192` 与 `trunk vlans 300` 写在**同一条**语句里（`… ports 5 interface ens192 trunk vlans 300`）则正常。

**根因**：`patternMatches` 要求 token 数完全相等（`internal/api/cliexec.go:1525`），`… interface <if> trunk vlans <list>`（9 token）匹配到别名规则；但 `… interface trunk vlans <list>`（8 token）不匹配任何别名，落到通用树遍历——树里 `trunk` 是 `interface` 下的对象容器，于是写出 `trunk: {vlans: …}`，与模型 `TrunkVlans []int` 不符。

**修复建议**：为 `{"virtual-switches","*","ports","*","interface","trunk","vlans","*"}` 补一条别名（`portTrunkApply` 需容忍 `t[5]` 缺省为已存在的 interface）。

**影响**：分步构建端口（先加口、后加 trunk）是自然操作顺序，Tab 补全也会补出这条，但执行必失败。

---

### BUG-07 · 低危 · `show vrfs <n> routes` 的 Distance 列恒显示「未下发」易被误读

**现象**：静态路由已真实进入 VPP FIB（`vppctl show ip fib table vs-l3` 可见 `10.88.0.0/24` 与 `10.77.0.0/24`），但 CLI 输出：

```
Prefix               NextHop              Distance
10.88.0.0/24         10.99.0.254          未下发
10.77.0.0/24         10.99.0.253          未下发
（Distance 未下发：VPP 路由 API 不接收该参数，v1 不显示具体值）
```

**判定**：这是**已在代码注释中登记的已知限制**（`internal/api/cli_net_runtime_show.go:403`，决策 #393），不是实现缺陷。但它把「路由是否下发」与「distance 是否下发」混在同一列名 `Distance` 下，读者极易把「未下发」理解成整条路由没下发。

**建议**：列名改为 `Distance(未下发)`，或整列改为 `—` 并把注记提前到表头下方。

---

### BUG-08 · 低危 · `show system hardware` 在无 BMC 环境下降级信息偏薄

**现象**

```
bmc-present    false
sensors        （无可用传感器：无 BMC/IPMI，lm-sensors 与 /sys/class/thermal 均无读数）
Disk             SMART      Temp       Wear
/dev/sda         unknown    -          -
```

**判定**：降级路径本身是**如实**的（不编造读数），符合产品「不谎报」口径。但 `SMART unknown` 没有说明原因（是 `smartctl` 未装、还是设备不支持），运维无法判断「要不要装 smartmontools」。

**建议**：在 `unknown` 后附原因（如 `smartctl 未安装` / `设备不支持 SMART`）。

---

### BUG-09 · 高危 · 默认 `console serial` 生成无显卡域 XML，Debian/Ubuntu 云镜像无限重启

**现象**：用 Debian 13 genericcloud 镜像建 VM（默认 `startup console serial`），`virsh domstate` 一直是 `running`，但客户机永远到不了登录提示符——串口只反复打印 `Booting 'Debian GNU/Linux'` 后立刻回到 SeaBIOS，形成无限重启循环。QEMU 进程存活、vCPU 时间持续增长（`vcpu.0.time` 从 13s 涨到 137s），说明客户机内部在反复重启。

**复现步骤**

```bash
request images upload name debian-13.qcow2 type vm-image file debian-13.qcow2
# configure
# set virtual-machine-functions dbtest image debian-13.qcow2
# set virtual-machine-functions dbtest vcpu count 1
# set virtual-machine-functions dbtest memory size-mb 1024
# set virtual-machine-functions dbtest memory hugepage-size 2M
# commit
request virtual-machine-functions dbtest start
virsh console dbtest      # 只见 "Booting 'Debian GNU/Linux'" 循环
```

**实测证据**

```
$ virsh dumpxml dbtest | grep -A2 "<video"
    <video>
      <model type='none'/>
    </video>

$ virsh dumpxml dbtest | grep -A2 "<serial"
    <serial type='pty'>
```

**二分定位**（每一步都直接跑 QEMU 排除 libvirt 因素）

| 参数组合 | 结果 |
|---|---|
| 裸 `-machine pc-q35-10.2 -accel kvm -cpu host` + virtio 盘 | ✅ 正常到登录提示符 |
| 加上 nfvis 的 `-machine ...,usb=off,dump-guest-core=off,memory-backend=pc.ram,acpi=on` | ✅ 正常 |
| 加上大页 `memory-backend-file` + `-overcommit mem-lock=on` | ✅ 正常 |
| 加上 `-no-shutdown -boot strict=on` + `qemu-xhci` | ✅ 正常 |
| 加上 `-nodefaults`（移除默认 VGA） | ❌ **GRUB 后无限重启** |
| `-nodefaults` + 显式 `-vga std` | ✅ 正常 |
| 通过 libvirt 域（`<video><model type='none'/>`） | ❌ **无限重启** |
| 手工把域 XML 改成 `<model type='vga'/>` | ✅ **正常启动**（`virsh domstate` 稳定，`vcpu.0.time` 正常增长） |

**根因**：`internal/orchestrator/compute/domain_xml.go:348` 的注释写「无人值守：无显卡（vnc 除外）」：

```go
videoType := "none"
if strings.EqualFold(strings.TrimSpace(spec.Console), "vnc") {
    videoType = "vga"
}
d.Devices.Videos = []libvirtxml.DomainVideo{{Model: libvirtxml.DomainVideoModel{Type: videoType}}}
```

Debian/Ubuntu 的 GRUB 默认启用 `gfxterm`（图形终端）。当机器完全没有显示设备时，`gfxterm` 初始化失败 → GRUB 触发重启 → 形成死循环。这不是 Debian 特有：任何使用 GRUB `gfxterm` 的发行版（Ubuntu 全系、Debian 9+、RHEL 8+、Fedora）都会中招。

**为什么之前没被发现**：Alpine 的 `syslinux`/`extlinux` 配置不使用 `gfxterm`，所以在 Alpine 上「无显卡」恰好能启动——这是测试覆盖的盲区（原报告只用了 Alpine）。

**修复建议**
1. 默认始终挂一块 `vga`（或 `virtio`）显示设备——无人值守场景下多一块 VGA 的成本可忽略（16 MiB vram，不占 CPU）；
2. 若确实要省资源，改用 `<video><model type='none'/><address .../></video>` 之外的方式：给 GRUB 传 `GRUB_TERMINAL=serial`（需要改 guest，不可行），或至少提供 `set vm-image-profiles <img> video none|vga` 开关并在文档里警告；
3. 建议同时给 `console serial` 加一条启动期校验：若 guest 在 N 秒内反复重启超过 K 次，升 `VNF_BOOT_LOOP` 告警（当前只看到 `VNF_PORT_DOWN`，没有「反复重启」这个信号）。

**影响**：**默认配置下，Debian/Ubuntu 云镜像无法启动**——这是 NFVi 一体机最主流的 VNF 镜像类型。用户会看到「VM running」但永远连不上，且没有任何告警指向真正原因。

---

### BUG-10 · 高危 · `request system software add` 自重启导致包停在 half-configured

**现象**：用真实 `nfvis_3.0.24_amd64.deb` 执行升级，命令报 `%% 安装失败（失败应自动回退...）: signal: terminated`，但 dpkg 实际已把文件装好；随后 `dpkg -l nfvis` 显示 `iF`（half-configured），`dpkg --audit` 明确报出。

**实测证据**

```
$ nfvis-cli -c "request system software add /tmp/nfvis-test/nfvis_3.0.24_amd64.deb --yes"
%% 安装失败（失败应自动回退，可执行 request system software rollback）: signal: terminated:
Preparing to unpack …/nfvis_3.0.24_amd64.deb…
nfvis prerm: 升级中，保留运行中的 nfvis.service（由 postinst try-restart 换新版本）
Unpacking nfvis (3.0.24) over (3.0.24)…
Setting up nfvis (3.0.24)…
nfvis postinst: 已重启 nfvis.service 以加载新版本
...
% 脚本在第 1 行停止执行

$ dpkg -l nfvis
iF  nfvis  3.0.24  amd64  NFViS NFV infrastructure appliance

$ dpkg --audit
The following packages are only half configured ...
 nfvis                NFViS NFV infrastructure appliance (nfvisd + nfvis-cli)
```

**根因**：`postinst` 里的 `try-restart nfvis` 会杀掉**正在执行这条 CLI 命令的 nfvisd 进程**（命令本身是通过 nfvisd 的 `/cli/execute` 执行的）。dpkg 子进程收到 `SIGTERM`，dpkg 判定 postinst 失败 → 包停在 half-configured。

**为什么「手动重跑就好」**：`dpkg --configure nfvis` 再跑一次 postinst 时没有「正在执行 CLI 的 nfvisd」这个前提，所以能成功——这恰好证明 postinst 逻辑本身正确，问题在自重启时序。

**修复建议**
1. 升级路径改为「先落盘、延迟重启」：`postinst` 只做文件替换，把 `systemctl try-restart` 交给 `nfvisd` 自己在**命令返回之后**执行（例如写一个 `pending-restart` 标记，由 supervisor/定时器消费）；
2. 或让 CLI 的 `software add` 走「脱离会话」的子进程（`systemd-run --scope` / `setsid`），使 nfvisd 被杀时 dpkg 不受影响；
3. 无论哪种，`software add` 返回前应复核 `dpkg-query -W -f='${Status}' nfvis`，非 `install ok installed` 就如实报错并自动 `dpkg --configure` 重试。

**影响**：每次软件升级都会把包留在 half-configured；虽然服务仍在跑，但后续任何 `apt` 操作都可能因 dpkg 状态不一致而失败，且「安装失败」的提示与实际结果相反（文件已装好），误导运维。

---

### BUG-11 · 中危 · `ping vrf <L2交换机名>` 查错 VPP 表，必然报「无出口」

**现象**：L2 交换机 `vs-lan` 配了 BVI 网关 `10.10.10.1/24`，客户机拿到 `10.10.10.100`，VPP 侧 `ping 10.10.10.100 source bvi0` 3/3 通；但 `ping 10.10.10.100 vrf vs-lan` 恒报 `Failed: no egress interface`。Web 诊断页填同一个 VRF 名也一样失败。

**实测证据**

```
$ vppctl show ip fib summary
ipv4-VRF:0, fib_index:0 ...
vr-vs-lan, fib_index:1 ...        # BVI 网关表（10.10.10.0/24 在这里）
vs-lan,   fib_index:2 ...         # 用户 VRF 表（10.20.20.0/24 在这里）

$ vppctl show ip fib table 13453353 | grep 10.10.10.0
10.10.10.0/32 ...                 # vr-vs-lan 表有网关路由
$ vppctl show ip fib table 14615916 | grep 10.10.10.0
（无）                             # vs-lan 表没有

$ vppctl ping 10.10.10.100 table-id 13453353 repeat 1
Statistics: 1 sent, 0 received        # 能发出去
$ vppctl ping 10.10.10.100 repeat 1
Failed: no egress interface
Statistics: 0 sent, 0 received

$ nfvis-cli -c "ping 10.10.10.100 vrf vs-lan count 1"
Failed: no egress interface
%% ping 未发出任何报文（10.10.10.100 不可经 VPP 到达）

$ nfvis-cli -c "ping 10.10.10.100 vrf vr-vs-lan count 1"
116 bytes from 10.10.10.100: icmp_seq=1 ttl=64 time=.2158 ms
Statistics: 1 sent, 1 received, 0% packet loss     ← 用内部名就通了
```

**根因**：L2 交换机的 BVI 网关落在**专属 VRF** `vr-<交换机名>` 里（`internal/orchestrator/network/l3.go:71` 的 `GatewayVRFName`），而 L3 接口落在用户命名的 VRF 里。`ping vrf <name>` 直接把用户输入当表名算 `TableID`（`l3.go:60` 的 FNV-1a），没有做「L2 交换机名 → `vr-` 前缀」的映射，于是查到的是那张空的 `vs-lan` 表。

**修复建议**：`Diagnostics.Ping` 在解析 VRF 时，若该名字是 L2 交换机且有 BVI，先尝试 `vr-<name>` 表（或两张表都试、取有路由的那张）；CLI 的帮助文案也应说明「L2 交换机的网关在 `vr-<名>` 表」。

**影响**：运维按文档填交换机名做连通性测试，得到「不可达」的结论，而实际链路是通的——这类假阴性会直接导致误判和无效排障。

---

### BUG-12 · 中危 · 部署向导网络映射写入空 vNIC 名

**现象**：Web 部署向导第 ③ 步「网络映射」加一块网卡，页面显示 `memif` 且挂接 `vs-lan`，摘要页也显示 `→ memif @ vs-lan`；但「写入部署候选」后，候选里的 vNIC `name` 是空字符串，`预校验` 直接报 `container-functions[wiz-ct2].interfaces[]：vNIC名称缺失`。

**实测证据**

```json
// show configuration candidate（向导写入的结果）
"container_functions": [
  ...,
  { "image": "alpine:3.20",
    "interfaces": [ { "name": "", "type": "memif", "virtual_switch": "vs-lan" } ],
    "memory_mb": 1024, "name": "wiz-ct2", "vcpu": 1 }
]
```

```
// 配置页「预校验」输出
预校验发现 1 个问题：container-functions[wiz-ct2].interfaces[]：vNIC名称缺失
```

**对比**：同一份候选里，CLI 建的 `c1` 是 `"name": "nic0"`（正确）。

**根因**：向导第 ③ 步的网卡名输入框有 `placeholder="eth0"`，但**没有默认值**（`value` 为空）。用户不手动输入就点「下一步」，向导把空串写进 `name`。页面表格里显示的是 placeholder（`eth0`），造成「看起来已经填了」的错觉。

**修复建议**
1. 新增网卡时给 `name` 一个默认值（`eth0`/`nic0` 递增），而不是只给 placeholder；
2. 「下一步」前做本地校验：`name` 为空则阻止前进并高亮该输入框；
3. 写入候选前再校验一次（服务端已有这条规则，但向导应在本地就拦住，而不是让用户到配置页才发现）。

**影响**：用户按向导操作、看到「部署候选已写入」的成功提示，实际得到的是**提交必被拒**的非法候选；且失败信息要等到配置页预校验才出现，与向导的「成功」反馈矛盾。

---

### BUG-13 · 低危 · 部署向导步骤按钮不能回退到之前的步骤

**现象**：走到第 4 步（摘要）后，点顶部「③ 网络映射」按钮，只有按钮高亮变化，主区域仍停在摘要页；必须点「← 上一步」才能回去。

**实测**：在第 4 步点击「③ 网络映射」后，`<main>` 内容仍为 `部署向导 第 4 / 4 步` + 摘要表 + 「写入部署候选」按钮，仅按钮的 `[active]` 标记转移。

**根因**：步骤按钮只改了「当前高亮步骤」的 UI 状态，没有同步切换渲染的步骤内容。

**建议**：步骤按钮点击时同时更新「当前步骤」与渲染分支；或对已完成的步骤禁用点击（避免用户以为能跳回去改）。

---

### BUG-14 · 高危 · 删除 L3 接口后残留空 VRF 壳，`display set` 永久失效且无法用 CLI 修复

**现象**：删除 L2 交换机上唯一的 `l3-interface` 后，配置里残留一个空的 `vrfs vs-lan { name vs-lan; }`。此后 `show configuration | display set` 直接报内部错误：

```
%% display set 内部错误：生成语句未能完整还原配置——请改用 show configuration / | display json 并报告缺陷
```

**复现步骤**

```bash
# 前提：vs-lan（L2）配了 l3-interface ens224 ip address 10.20.20.1/24
# configure
# delete virtual-switches vs-lan l3-interface ens224
# delete acls acl-deny-icmp      ← 顺带删掉绑在该 L3 接口上的 ACL
# commit
show configuration | display set
```

**实测证据**

```
$ show configuration | display set
%% display set 内部错误：生成语句未能完整还原配置——请改用 show configuration / | display json 并报告缺陷

$ show configuration          # 树视图里能看到残壳
vrfs vs-lan {
    name vs-lan;
}
```

**为什么用户无法自救**

```
$ configure; delete vrfs vs-lan; commit
%% 未知命令: "vrfs"
```

`vrfs` 不是配置语句树的顶层关键字（L3 交换机通过 `set virtual-switches <n> type l3` 表达），因此**没有任何 CLI 语句能删掉这个残壳**。`display set` 是 `save`/`load`、`annotate`、配置对比、工单回显的共同底座，失效后影响面很大。

**唯一恢复路径**：`rollback N` 回到删除前的快照（实测 `rollback 1` + `commit` 后 `display set` 立即恢复）。

**根因**：`pruneEmptySingleton`（`internal/api/cliexec.go:1255`）登记了 `system.management`、`system.metrics`、`system.firewall`、`protocols`（含内层 `lldp`）、`nat`、VM 内嵌 `startup` 共 6 类空壳回收，**唯独没有 `vrfs`**。该函数自己的注释已经把这类缺陷的成因写得很清楚：

> 「删光池/规则/静态映射后留下空 `nat = {}`，`show configuration | display set` 反推不出任何语句、回放自校验对不上而报内部错误（自校验没错，错在空壳；空 `nat` 节点同样让『配置过又删光』与『从未配置 NAT』不可区分）」

`vrfs` 完全符合同一模式，只是漏登记。删除最后一个 `l3-interface` 后，`vrfs[name]` 变成 `{"name": name}` 的残壳；`display set` 的发射器无法把它还原成任何合法语句（`vrfs` 不是语句树顶层关键字，L3 交换机由 `set virtual-switches <n> type l3` 表达）。

**修复建议**
1. 把 `vrfs` 纳入 `pruneEmptySingleton` 的回收清单——判据：只剩 `name` 字段（或 `l3_interfaces`/`routes` 都为空）即回收；注意要按**数组元素**遍历（`tree["vrfs"]` 是 `[]any`），与 `virtual_machine_functions` 的 `startup` 回收同法；
2. 补回归测试：删光某 VRF 的所有子对象（`l3-interface`、`static-routes`）后，`display set` 必须仍能完整还原；
3. 兜底：`display set` 遇到无法还原的节点时，应**跳过该节点并指明路径**（如「跳过 vrfs[vs-lan]：无对应语句」），而不是整条命令失败——当前是「一处残壳让整个功能不可用」；
4. 同类排查：`pruneEmptySingleton` 是**手工登记制**（注释明确写「只收敛逐个登记的容器」），这种设计天然会漏。建议改为「声明式白名单 + 表驱动测试」，或在 `display set` 侧加「发射器覆盖全树」的契约测试，让漏登记在 CI 就暴露。

**影响**：这是**高危**——一个正常的删除操作（删 L3 接口）就能把 `display set` 这个核心读视图打成永久不可用，且用户没有 CLI 手段修复，只能靠 rollback 或重建配置。

---

## 4. WebUI 与 CLI 数据同步性评估

### 4.1 测试环境

浏览器自动化（Browser Use / IAB），目标 `https://192.168.155.129/api/v1/ui/`，跳过自签证书告警，`admin` 登录，实时通道状态 `已连接`（SSE）。

### 4.2 CLI → Web（4/4 通过）

| 步骤 | CLI 侧动作 | Web 侧观察 | 结论 |
|---|---|---|---|
| 1 | `set virtual-switches vs-lan type l2` + 成员口 `ens192` + trunk `100,200`，commit | 虚拟交换机列表出现 `vs-lan / l2 / 成员端口 1 / 2 / BD 14615916` | ✅ 一致（含运行态计数） |
| 2 | 同上，L3：`vs-l3 type l3` + `l3-interface ens224 ip address 10.99.0.1/24` | 列表出现 `vs-l3 / l3 / 0 / —`，Note 列标 `l3（VRF）` | ✅ 一致 |
| 3 | `set virtual-machine-functions vm1 …` + `request … start` | 虚拟机页出现 `vm1 / running / 1 / 512 MB / alpine-3.20.qcow2`，`启动` 按钮置灰、`停止`/`重启`/`删除` 可用 | ✅ 一致（按钮随状态启用） |
| 4 | 详情页联动 | `vs-lan` 详情页成员端口表含 `ens192（配置）` 与 `ens192.100（运行态）`，vhost 计数 `rx 44 / tx 0` | ✅ 与 `show virtual-switches vs-lan ports` 逐字对应 |

**截图**：`evidence/screenshots/01-virtual-switches.png`（虚拟交换机列表，含 `1 / 2` 双口径计数与 BD-ID）、`evidence/screenshots/02-vm-management.png`（虚拟机列表，`vm1 running` 与按钮状态）。

### 4.3 Web → CLI（4/4 通过）

| 步骤 | Web 侧动作 | CLI 侧验证 | 结论 |
|---|---|---|---|
| 1 | 「+ 新建」→ 名称 `vs-web1` → 类型 L2 → access VLAN `300` → 描述 `created-via-web` → 「保存并提交」→ 确认框「提交」 | 页面提示 `提交成功（revision 300）`；CLI `show virtual-switches` 出现 `6011860 vs-web1` | ✅ |
| 2 | 同上 | `show configuration \| display set` 出现 `set virtual-switches vs-web1 type l2` 与 `set virtual-switches vs-web1 vlan access 300` | ✅ 逐字段一致 |
| 3 | VM 页「停止」→ 确认「确定」 | 4 秒内 Web 行变 `shutoff`；`virsh domstate vm1` → `shut off`；CLI `show virtual-machine-functions` → `vm1 shutoff` | ✅ 三面一致 |
| 4 | 变更确认框回显 | 对话框显示「对应命令（只读回显）：`set virtual-switches vs-web1 type l2`」 | ✅ 与 CLI 语法同源 |

### 4.4 事务语义一致性

Web 的写路径与 CLI **共用同一事务引擎**：确认框明确写出「取编辑锁 → 写候选 → 校验 → 提交」，实测 `revision` 编号连续递增（CLI 的 `rev 299` 紧接 Web 的 `rev 300`），说明两条路径写的是同一个 `nfvis.db`、同一个 revision 序列，不存在双写或旁路。

### 4.5 Web 全页面覆盖（10 页 / 34 交互序列）

| 页面 | 路由 | 验证内容 | 结论 |
|---|---|---|---|
| 总览 | `#/` | 主机名/运行时长/CPU/内存/磁盘、组件版本（NFViS 3.0.24 / VPP 26.06 / libvirt 12.0.0 / QEMU 10.2.1 / Docker 29.1.3 / Ubuntu 26.04）、数据面状态（3 线程、296.5 MB/1.0 GB、buffer 5019/11220）、告警、实时事件 | ✅ 与 CLI `show version`/`show vpp` 逐值一致 |
| 虚拟机 | `#/compute/vms` | 列表状态、动作按钮按状态启用（running 时「启动」置灰）；「停止」→ 确认框回显对应 CLI 命令 → 执行 | ✅ 三面一致（Web/`virsh`/CLI） |
| 虚拟机详情 | `#/compute/vms/:name` | 概览/接口/快照/串口四个分栏；资源（绑定核 5、大页 2M、vNIC 1、串口启用）；生命周期；vhost-user 口计数 | ✅ |
| 容器 | `#/compute/containers` | 列表（`c1 running 1 128 MB alpine:3.20`）、行内编辑/启动/停止/重启/删除 | ✅ |
| 容器详情 | `#/compute/containers/:name` | 概览（命令 `/bin/sleep`、参数 `3600`、env `MYVAR=hello`、重启次数 0）、日志、**执行命令**、交互终端、vNIC 表（memif @ vs-lan） | ✅ exec `cat /etc/os-release` 返回真实 `Alpine Linux v3.20`，并写入审计 |
| 镜像 | `#/compute/images` | 3 个镜像（2 vm-image + 1 container-image）、引用数、状态、画像/删除按钮、导入区（URL/incoming/本地上传） | ✅ 与 `show images` 一致 |
| 部署向导 | `#/deploy` | 4 步：类型（VM/容器）→ 镜像与资源（按类型过滤镜像、显示资源池实况）→ 网络映射（拓扑预览图、按平台给类型）→ 摘要 + 写候选 | ⚠️ 功能可用，但见 BUG-12（空 vNIC 名）与 BUG-13（步骤不可回退） |
| 配置 | `#/config` | committed revision 显示、原始 JSON 编辑、保存到 candidate、**预校验**、查看差异、提交、丢弃并结束编辑 | ✅ 预校验正确捕获 `vNIC名称缺失` |
| 提交历史 / 编辑锁会话 | `#/config/history`、`#/config/sessions` | 每版可查看/取为候选；持锁会话列表 | ✅ |
| 运维动作 | `#/ops/actions` | 配置备份、SSH host key 重生成、NTP 立即同步、VPP 配置就地编辑、tech-support 生成、core dump 清单、**归档下载** | ✅ 下载列表含 7 个备份 + 5 个诊断归档 |
| 审计日志 | `#/ops/audit` | 最近 50 条，含 CLI 与 Web 两类来源（`container.exec`、`config.commit` 成功与失败） | ✅ |
| 诊断 | `#/ops/diagnostics` | ping/traceroute（目标/次数/源/VRF/IPv6）、接口计数清零、服务端日志 | ⚠️ 可用，但 `ping vrf vs-lan` 受 BUG-11 影响 |
| 抓包 | `#/ops/capture` | 开始/停止/停止并导出、pcap 下载 | ✅ 导出 8.5 KiB pcap，`tcpdump` 解出 SSH 会话流量 |
| 实时监视 | `#/ops/monitor` | 实时通道 `已连接`（SSE），事件到达即刷新 | ✅ |

### 4.6 发现的问题

- **状态刷新延迟**：Web 点「停止」后页面行**未立即**更新，约 8 秒（下次轮询周期）后才变 `shutoff`。页面底部已注明「多数页 5 秒一次刷新」，属设计如此，但对「点完就想看到结果」的用户体验略差。建议写类动作成功后立即触发一次本地刷新。
- **部署向导的空 vNIC 名**（BUG-12）与**步骤不可回退**（BUG-13）已在缺陷列表登记。
- **IAB 文件选择器限制**：镜像「本地上传」在自动化环境无法验证（IAB 明确不支持 `filechooser`）；incoming 目录与 URL 拉取两条路径可用。

---

## 5. 虚拟化与 IOMMU 兼容性报告

### 5.1 宿主机能力

| 检查项 | 实测值 | 结论 |
|---|---|---|
| CPU 虚拟化标志 | `egrep -c '(vmx\|svm)' /proc/cpuinfo` = **12** | ✅ VT-x 可用 |
| KVM 模块 | `kvm_intel` / `kvm` 已加载，`/dev/kvm` 存在（`crw-rw---- root:kvm`） | ✅ |
| IOMMU（初始） | `/sys/kernel/iommu_groups/` **空**，cmdline 无 `intel_iommu` | ⚠️ 出厂未开 |
| IOMMU（配基线后） | `iommu=on intel_iommu=on` → `dmesg: DMAR: IOMMU enabled`，**10 个 group**，`/sys/class/iommu/dmar0` 出现 | ✅ |
| IOMMU group 粒度 | group 6 = `00:15.0-7` + `03:00.0`；group 8 = `00:17.0-7` + `13:00.0` | ⚠️ VMware 平台特性：每块网卡与 8 个根端口同组，**整组直通才安全** |
| 大页 | 2M：1024 页（free 1003）；1G：2 页（free 1） | ✅ 与声明一致 |
| hugetlbfs 挂载 | `/dev/hugepages`（1G）、`/dev/hugepages-2M`（2M） | ✅ 双尺寸各一挂载点（决策 #459 生效） |
| 隔离核 | `isolcpus=1-4`，`/sys/devices/system/cpu/isolated` = `1-4` | ✅ |
| vfio 模块 | `vfio_pci` / `vfio_iommu_type1` / `vfio` 自动加载（首次 bind 时） | ✅ 按需加载正常 |

### 5.2 直通实测（端到端）

```
$ request interfaces ens224 bind-dpdk --yes
接口 ens224（PCI 0000:13:00.0）已绑定到 DPDK 驱动，当前驱动 vfio-pci

$ # configure; set virtual-machine-functions vm1 pci-device 0000:13:00.0; commit
commit 成功 (revision 305)
警告: VM vm1 的 PCI 直通设备 已变更：变更在下次启动时生效

$ request virtual-machine-functions vm1 start
启动 VNF vm1 已受理
```

**QEMU 进程实测参数**

| 参数 | 实测值 | 评价 |
|---|---|---|
| `-accel` | `kvm` | ✅ 硬件加速生效 |
| `-cpu` | `host,migratable=on` | ✅ host-passthrough |
| `-m` | `size=524288k` + `memory-backend-file,mem-path=/dev/hugepages-2M/libvirt/qemu/4-vm1,prealloc=on` | ✅ 大页后端 |
| `-smp` | `1,sockets=1,cores=1,threads=1` | ✅ 与 `vcpu count 1` 一致 |
| `cputune` | `<vcpupin vcpu='0' cpuset='5'/>`；`taskset` 实测 vCPU 线程亲和 = `5` | ✅ 钉在隔离核 |
| `-netdev` | `{"type":"vhost-user","chardev":"charnet0","queues":2}` | ✅ vhost-user，socket `/run/nfvis/vhost/vm1-nic0.sock` 存在 |
| `-device vfio-pci` | `{"driver":"vfio-pci","host":"0000:13:00.0"}` | ✅ 直通拼装正确 |
| `-sandbox` | `on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny` | ✅ 加固到位 |

**VFIO 运行时证据**

```
$ ls -l /proc/25717/fd | grep vfio
33 -> /dev/vfio/8
34 -> /dev/vfio/vfio
35 -> anon_inode:kvm-vfio
36 -> anon_inode:[vfio-device]

$ virsh qemu-monitor-command vm1 --hmp "info pci" | grep -A5 15ad:07b0
  Bus  5, device   0, function 0:
    Ethernet controller: PCI device 15ad:07b0
      BAR0: 32 bit memory at 0xfd412000 [0xfd412fff]
      BAR1: 32 bit memory at 0xfd413000 [0xfd413fff]
      BAR2: 32 bit memory at 0xfd410000 [0xfd411fff]
      BAR3: I/O at 0xc000 [0xc00f]
```

**guest 侧**：`virsh console vm1` 实测启动到 `Welcome to Alpine Linux 3.20 / Kernel 6.6.49-0-virt on an x86_64 (/dev/ttyS0)`，cloud-init `DataSourceNoCloud [seed=/dev/vdb]` 生效，hostname 变为 `vm1-test`——**直通设备在 guest PCI 总线上可见且 BAR 已映射**，不是只有宿主 XML 好看。

### 5.3 VPP 数据面

| 检查项 | 实测 |
|---|---|
| 线程绑核 | `vpp_main` core 4、`vpp_wk_0` core 2、`vpp_wk_1` core 3（`show vpp threads`） |
| 启动配置 | `/etc/vpp/startup.conf` 含 `cpu { main-core 4; corelist-workers 2-3 }` 与 `dpdk { dev 0000:0b:00.0 { name ens192 } dev 0000:13:00.0 { name ens224 } }` |
| 接口状态 | `ens192` idx 1 up MTU 9000；`ens224` idx 2 up MTU 9000；`vh-vm1-nic0` idx 2 up（vhost） |
| Bridge domain | `BD-ID 14615916`，成员 `ens192.100` + `vh-vm1-nic0.100` |
| L3 VRF | `vs-l3, fib_index:1`，`10.77.0.0/24` 与 `10.88.0.0/24` 在 FIB |
| 统计 | `show vpp runtime` 给出逐线程 vector rate / loops rate；`show vpp buffers` 给出 used/available/cached |

### 5.4 兼容性结论

- **KVM 层**：✅ 完全可用。`-accel kvm -cpu host`、大页后端、vCPU 钉核、vhost-user、VFIO 直通全部实测成立。
- **IOMMU 层**：✅ 可用但需注意平台特性。VMware 的 VMXNET3 网卡与 8 个 PCIe 根端口同属一个 IOMMU group（group 6/8），单独直通网卡会连带整组根端口，生产环境应确认同组设备是否可一并隔离。
- **DPDK 层**：✅ 可用（`vmxnet3` 经 `vfio-pci` 被 VPP 接管，`show hardware-interfaces` 显示 `VMware VMXNET3 / carrier up / max-frame-size 9022`）。
- **风险点**：见 BUG-01（删除 per-dev 后无主）与 BUG-05（直通不校验占用）。

---

## 6. 安装与持久化

### 6.1 安装（`.run`）

```
$ ./nfvis-v3.0.24.run --yes --admin-password '***' --keep
===== 前置检查 =====   Ubuntu 26.04.1 / amd64 / 配置库尚未建立（首次引导）
===== 安装依赖 =====   （本地源，239 个 deb）
===== 启服务 =====     VPP API 套接字就绪；控制面已监听（HTTP 200）
===== 自检 =====       通过 18 / 失败 0 / 不可判定 0
EXIT=0
```

自检覆盖：`nfvis` 包版本、dpkg 审计无半装、`nfvis.service` active+enabled、VPP 运行（`vppctl` 独立事实源）、VPP API socket、`GET /api/v1/ui/` 200、`libvirtd` active、`docker` active、AppArmor 放行、GRUB 片段存在、CLI 走 API 通路、CLI 已连 VPP。

**端口**：`*:443` 由 `nfvisd` 独占；`22` 由 sshd。无多余暴露面。

### 6.2 重启持久化（3 轮）

| 轮次 | 验证内容 | 结果 |
|---|---|---|
| 第 1 轮 | 大页/isolcpus 首次写入 GRUB → 重启 → 内核参数生效 | ✅ `hugepages=1024`、`hugepagesz=1G hugepages=1`、`isolcpus=2-5` 全部生效；`/dev/hugepages` 与 `/dev/hugepages-2M` 自动挂载 |
| 第 2 轮 | 修正 IOMMU 参数后重启 | ✅ `iommu=on intel_iommu=on`，10 个 IOMMU group，`dmar0` 出现 |
| 第 3 轮 | 配置 + VM 定义 + 数据面对象跨重启 | ✅ 配置逐条保留；`show system kernel` 显示「内核基线与配置期望一致」；`vs-lan` BD 自动重建（`14615916`）；`vs-web1`（Web 建的）保留；VM `vm1` 定义保留（`shutoff`，未开 autostart） |

**服务恢复**：三次重启后 `nfvis` / `vpp` / `libvirtd` / `docker` 均 `active`，`443` 监听恢复，无需人工干预。

---

## 7. 容器 VNF 与网络数据面

### 7.1 容器 VNF 全生命周期（14/14 通过）

| 环节 | 命令 | 实测结果 |
|---|---|---|
| 镜像导入 | `request images upload name alpine:3.20 type container-image file ct2.tar` | `导入完成（类型 container-image，大小 3.6 MiB）`；`docker images` 出现 `alpine:3.20 d9e853e87e55` |
| 创建 | `set container-functions c1 …` + `commit` | `revision 662` 成功 |
| Docker 实况 | `docker inspect c1` | `Mem:134217728 NanoCpus:1000000000 Net:none Restart:no`、`Env:[MYVAR=hello …]` |
| cgroup 限额 | `cat /sys/fs/cgroup/system.slice/docker-<id>.scope/memory.max` | `134217728`（= 128 MB，与声明一致） |
| 启动/停止/重启 | `request container-functions c1 start|stop|restart` | 三条均返回「已受理」，Docker 状态随之变化 |
| 日志 | `request container-functions c1 log last 5` | `（无日志）`（`sleep` 无输出，符合预期） |
| 容器内执行（CLI） | `request container-functions c1 exec "echo nfvis-exec-ok"` | `退出码: 0（耗时 53ms）` + `--- stdout --- nfvis-exec-ok` |
| 容器内执行（Web） | 详情页「执行命令」→ `cat /etc/os-release` | 返回 `NAME="Alpine Linux" ID=alpine VERSION_ID=3.20.10`；同时写入审计 `container.exec … 退出码 0` |
| memif vNIC | `set container-functions c1 interfaces nic0 type memif` + `virtual-switch vs-lan` | VPP `show memif` 见 `224 yes (1) /run/nfvis/memif/c1-nic0.sock`；`show interface` 见 `mf-c1-nic0` up；已加入 BD `vs-lan`；socket 已 bind-mount 进容器 |
| 重启计数 | `show container-functions c1 detail` | `restart-count 0`（取 Docker `RestartCount`） |

**说明**：容器内没有 memif 客户端（Alpine 基础镜像不含 VPP/DPDK 应用），因此 `mf-c1-nic0` 在 VPP 侧是 listener 但无对端连接（`tx-error 2`）。这是 memif 的固有语义——容器镜像需自带 memif 驱动才能收发。VPP 侧接口、BD 成员、socket 挂载三项均已正确建立。

### 7.2 网络数据面功能

| 功能 | 配置 | VPP 侧证据 | 结论 |
|---|---|---|---|
| L2 桥域 | `set virtual-switches vs-lan type l2` + 成员口 `ens192` | `BD-ID 14615916`，成员含 `ens192`、`vh-dbtest-nic0`、`mf-c1-nic0`、`tap0`、`bvi0` | ✅ |
| **端到端转发** | BVI 网关 `10.10.10.1/24` + DHCP 池 `10.10.10.100-200` | 客户机获租约 `10.10.10.100`；`vppctl ping 10.10.10.100 source bvi0` → **3 sent / 3 received / 0% loss** | ✅ 真实转发 |
| DHCP 服务器 | `set … dhcp-server pool/dns/lease-time` | `show virtual-switches vs-lan dhcp-leases` → `10.10.10.100 52:54:b4:08:8a:02 active 3565s` | ✅ |
| ACL | `set acls acl-deny-icmp rule 10 action deny protocol icmp` | `vppctl show acl-plugin acl` → `acl-index 1 … ipv4 deny src 0.0.0.0/0 dst 0.0.0.0/0 proto 1` | ✅ 规则语义正确 |
| ACL 绑定 | `set virtual-switches vs-lan l3-interface ens224 acl-in acl-deny-icmp` | `vppctl show acl-plugin interface` → `sw_if_index 2: input acl(s): 1` | ✅ 绑定正确（ens224 无链路，无法产生真实流量验证丢包） |
| VXLAN | `set vxlan tunnels tun1 vni 5001 local … remote … virtual-switch vs-web1` | `vppctl show vxlan tunnel` → `instance 0 src 192.168.155.129 dst 192.168.155.1 … vni 5001 sw-if-idx 10`；`show interface` 见 `vxlan_tunnel0` up；已入 BD `vs-web1` | ✅ |
| 链路聚合 | `set bonds bond0 members 0 ens192` + `lacp mode active` + `mtu 9000` | `vppctl show bond` → `BondEthernet0 11 lacp l2 active members 0 members 1`；`show interface bond0` up MTU 9000 | ✅ 创建正确（无 LACP 对端，成员未 active 属预期） |
| L3 VRF + 静态路由 | `set virtual-switches vs-nat type l3` + `l3-interface` + `static-routes` | `show ip fib summary` 见独立 `fib_index`；路由可在 FIB 查到 | ✅ |
| NAT44 | `set nat source-pool … address-range … to …` + `rules` | 提交期正确拒绝：内外口同口、出接口不在 VRF、池不存在、交换机非 L3 | ✅ 校验完整（受限于宿主只有 2 个业务口，无法搭完整 NAT 拓扑） |
| 端口镜像 | `set port-mirroring … source interface … analyzer interface …` | 提交期正确拒绝：缺 `direction`、分析口角色冲突 | ✅ 校验完整（需两个空闲物理口，宿主不足） |

**环境限制说明**：宿主只有 3 块网卡（`ens160` 管理口 + `ens192`/`ens224` 业务口），且业务口无对端交换机。因此 NAT（需内外两个 VRF 各占一口）、端口镜像（需源口 + 分析口）、ACL 真实丢包（需有流量的对端口）无法搭出完整拓扑。这些功能的**配置校验、下发路径、VPP 对象创建**均已验证；纯转发效果需在有多口对端的机器上复验。

---

## 8. 破坏性命令与运维动作（13/13 通过）

| 命令 | 非交互模式行为 | 结论 |
|---|---|---|
| `request system zeroize` | 打印「恢复出厂将清空全部配置、镜像与 VNF，并重置本地账号」+ 拒绝 | ✅ 守卫有效 |
| `request system storage format-data` | 打印影响面（VNF/容器/网络对象/镜像/备份/抓包/转储/诊断归档）+ 拒绝 | ✅ |
| `request system shutdown` / `poweroff` | 打印「关机 nfvis 将中断全部业务」+ 拒绝 | ✅ |
| `request system software add <deb>` | 打印「将替换 nfvis 并重启 nfvisd」+ 拒绝 | ✅ 守卫；但加 `--yes` 后见 BUG-10 |
| `request system software rollback to <ver>` | 打印影响面 + 拒绝 | ✅ |
| `request system kernel rollback` | **实际执行成功**：`已回退到上一次内核基线（备份 /var/lib/nfvis/kernel-baseline.bak）` | ✅ |
| `request system configuration backup to <path>` | 生成 `nfvis-backup-<ts>.json`（4.7 KiB）并导出 | ✅ |
| `request system configuration restore <path> --yes` | 恢复成功（`revision 665`），改动被撤销；提示「镜像文件本体不在归档内」 | ✅ 往返闭环 |
| `request system tech-support generate` | 生成 `nfvis-tech-support-<ts>.tar.gz`（20.0 KiB） | ✅ |
| `request system ntp sync` | `chronyc makestep: 200 OK` | ✅ |
| `request system ssh host-key regenerate` | `ssh-keygen -A` 成功 | ✅ |
| `request capture start/stop/export` | 抓到 8.5 KiB pcap，`tcpdump` 解出 SSH 会话流量 | ✅ |
| `show system core-dumps` | `（无 core dump）` | ✅ |

---

## 9. 测试资产与方法

| 资产 | 路径 | 说明 |
|---|---|---|
| 命令树提取器 | `src/cmd/schema-dump/main.go` | 直接调用 `schema.OperRoot()`/`ConfigRoot()`，输出 `artifacts/commands.json` |
| 命令树快照 | `artifacts/commands.json` | 3,168 节点，含 `min_class`/`dynamic`/`enums`/`optional` |
| 枚举映射 | `artifacts/enums.json` | 48 条枚举节点的取值域 |
| 用例生成器 | `gen_cases4.py` | schema → 197 条 `set` 语句，含分支夹具与身份值注入 |
| 事务安全执行器 | `runner3.py` | 每例独立会话；提交成功后 `rollback 1` + commit 复原基线 |
| 操作模式套件 | `oper_test.py` | 84 条 `show` + 8 条管道 |
| 模糊套件 | `fuzz.py` | 50 条负向/注入/超长/Unicode |
| 原始结果 | `artifacts/res_*.json` | 逐条含完整 stdout/rc/verdict |
| 缺陷复现日志 | `artifacts/bug_repro.json` | BUG-01/02/03/04/06 的原始输出 |
| 缺陷复现记录（第二轮） | 本报告 §3 的 BUG-09~13 | 每条含完整命令、输出、根因定位与二分表 |
| Web 截图 | `evidence/screenshots/*.png` | 虚拟交换机列表、虚拟机管理页 |
| 宿主机证据 | `/root/nfvis-test/evidence/*.txt` | baseline / post-install / post-reboot |
| 项目自带套件输出 | `/root/nfvis-test/fulltest4.out` | 287 通过 / 18 失败 / 22 预期报错 |

**方法学说明**

1. **用例来自实现而非文档**：`schema-dump` 与 `nfvisd` 的 `cli_bridge` 编译期共享同一棵树，因此提取出的 3,168 节点就是 `?`/Tab 与执行器校验的真实来源。
2. **事务隔离**：每条 `set` 用例提交成功后立即 `rollback 1` + commit，保证 197 条用例互不影响；这一点在第一版（无回滚）时曾导致 40+ 条 revision 累积、配置互相污染。
3. **三方交叉验证**：CLI 输出 ↔ `vppctl`/`virsh`/`/proc` 独立事实源 ↔ Web 页面渲染，任一侧不一致都记为缺陷。
4. **失败归因**：61 条 `set` 失败逐条读取校验文案，区分「用例取值越界/缺前置」与「产品缺陷」，前者不计入缺陷。
5. **缺陷定位用二分**：BUG-09 通过「直连 QEMU 逐参数累加」的方式，把 libvirt、大页、`-no-shutdown`、`-nodefaults` 等逐个排除，最终锁定「无 VGA」这一项；再用「手工改域 XML 加 VGA 后正常启动」反向确认。
6. **网络依赖走代理**：容器镜像（`docker.io`）与 Debian 云镜像的拉取通过 `192.168.155.1:2333` 代理完成——首轮未配代理时误判为「环境受限无法拉取」，实际配置 `docker.service.d/http-proxy.conf` 后即可正常拉取，因此容器全生命周期得以完整验证。

---

## 10. 建议优先级

| 优先级 | 项 | 理由 |
|---|---|---|
| **P0** | **BUG-09 无显卡导致 Debian/Ubuntu 云镜像无限重启** | 默认配置下最主流的 VNF 镜像根本起不来，且 VM 显示 `running`、无任何指向真因的告警 |
| P0 | BUG-01 无主设备锁死数据面重启 | 现场误操作后无法通过产品自身恢复，需手工 sysfs 救场 |
| P0 | BUG-02 IOMMU 参数矛盾 | 直通依赖 IOMMU，参数冲突会静默改变行为 |
| **P0** | **BUG-14 空 VRF 壳让 display set 永久失效** | 一个正常删除就能打挂核心读视图，且无 CLI 修复手段 |
| **P1** | **BUG-10 software add 留下 half-configured** | 每次升级都会污染 dpkg 状态，且「安装失败」提示与实际相反 |
| P1 | BUG-11 ping vrf 查错表 | 运维做连通性测试得到假阴性，直接导致误判 |
| P1 | BUG-12 向导写入空 vNIC 名 | 向导报「成功」但候选必被拒，反馈自相矛盾 |
| P1 | BUG-05 PCI 直通不校验占用 | 提交成功但开机失败，用户按成功信号判断 |
| P1 | BUG-04 ping 退出码 | 自动化判定链的静默假通过 |
| P2 | BUG-03 vm-image-profiles cpu mode 不可创建 | 补全能补出、执行必失败 |
| P2 | BUG-06 分步设置 trunk 失败 | 自然操作顺序不可用 |
| P2 | BUG-13 向导步骤不可回退 | 用户以为能回去改，实际没有 |
| P3 | BUG-07 / BUG-08 文案与降级说明 | 不影响功能，影响可诊断性 |

---

## 11. 附：未覆盖范围（如实登记）

- **SR-IOV**：宿主为 VMware 虚拟网卡，无 PF/VF 能力，`show interfaces <if> sriov` 只能验证空列表路径。
- **LLDP 对端**：无真实交换机对端，只能验证空表与按口过滤的报错路径。
- **NAT 完整拓扑 / 端口镜像真实镜像 / ACL 真实丢包**：宿主只有 3 块网卡（1 管理 + 2 业务）且业务口无对端交换机，无法搭出「内外两个 VRF 各占一口」「源口 + 分析口 + 流量」这类拓扑。配置校验、下发路径、VPP 对象创建已验证；纯转发效果需多口对端环境复验。
- **LACP 协商**：无 LACP 对端，只能验证 `show bond` 的 `mode lacp / active members 0` 结构正确。
- **Web 镜像「本地上传」**：IAB 明确不支持 `filechooser`，该入口无法自动化验证；incoming 目录与 URL 拉取两条路径可用。
- **高可用 / 多节点 / 性能压测**：不在本次功能测试范围。
- **`request system reboot` 经 CLI 执行**：本轮三次重启均直接走 `systemctl reboot`（避免 CLI 重启中断 SSH 会话影响后续用例）；`request system reboot` 的确认守卫已验证。
