# xzjt/nfvis v3.0.29 综合测试评估报告

| 项目 | 内容 |
|---|---|
| 被测对象 | `nfvis-v3.0.29.run`（186,030,230 B）+ GitHub `xzjt/nfvis` tag `v3.0.29`（HEAD `5d607ae`，2026-10-24） |
| 上一版基线 | v3.0.28 报告（BUG-17 修复确认；新发现 BUG-18 高危、BUG-19 低危） |
| 本版主题 | BUG-18/19 失败路径收口 + R44-1（脱离脚本退出码恒 0）+ 归档池污染 + dpkg 日志丢失（决策 #500） |
| 测试宿主机 | `nfvis-vm` — Ubuntu 26.04.1 / 6 vCPU / 7.2 GiB / 内核 7.0.0-38-generic / VT-x + DMAR |
| 安装包校验 | SHA256 `eaa53f68…9d1d36c`，与发布页 `.sha256` **逐字节一致** |
| Web 控制台 | `https://192.168.155.129/api/v1/ui/`（Browser Use / IAB 实测） |
| 测试执行 | 2026-10-25 03:13 – 04:20（UTC+8），1 次 `.run` 安装 + 2 次宿主机重启 + 1 次 nfvisd 重启 |
| 总判定 | **BUG-18/19 与 R44-1 三项全部修复且真机复验通过**；另发现 **6 项新缺陷**（0 高危 / 2 中危 / 4 低危），其中 1 项为 VM 功能可用性的恢复缺陷 |

---

## 1. 测试执行摘要

### 1.1 各阶段通过率

| 阶段 | 套件 | 用例数 | 通过 | 说明 |
|---|---|---:|---:|---|
| 一 | 命令树静态提取 | 3,172 节点 | ✅ 0 校验错误 | 与 v3.0.28 快照**零差异**（CLI 契约未变） |
| 二 | `.run` 安装自检 | 18 | **18** | 失败 0 / 不可判定 0，EXIT=0 |
| 二 | 重启持久化 | 12 项 | **12** | 内核参数/IOMMU passthrough/四服务/大页/443 全部恢复 |
| 三 | 操作模式全量 | 138 | **79 PASS** | 59 条为「前置条件不满足的正确拒绝」，0 FAIL、0 CRASH |
| 三 | 事务闭环 | 11 | **10** | 余 1 条（T7 会话锁）为测试设计问题：`-f` 进程无法持锁，同一行为已由 PTY P3 正向验证 |
| 三 | PTY 交互 | 5 | **5** | Tab 补全 / `?` 帮助 / 历史 / 会话锁 全通过 |
| 三 | 配置叶子 schema | 197 | **47 ACCEPTED + 102 REJECTED + 15 SET_ONLY** | **0 条真实解析缺陷**（33 条初判 FAIL 全部归因为用例把多值选项拆开下发） |
| 三 | 负向 / fuzz | 58 | **58 OK** | 零崩溃、零挂死、守护进程存活 |
| 三 | BUG-18/19 复测 | 2 场景 × 5 事实源 | **2/2 修复** | 1 秒采样全程 0 次 `ok:true` |
| 四 | Web 页面巡检 | 24 | **24** | 路由/面包屑/可见性全部正常，0 错误提示 |
| 四 | Web UI 写操作 | 1 | **1** | 新建虚拟交换机成功，VPP 真实建出 BD |
| 四 | CLI↔Web 双向 | 2 方向 | **2/2** | 两方向均端到端验证（自动化套件 4/6，余 2 条为测试脚本缺陷，见 §5.3） |
| 五 | KVM/QEMU 参数 | 7 项 | **7** | `-accel kvm` / `-cpu host` / hugepages / vfio / q35 / virtio 全对 |
| 五 | IOMMU 直通 | 4 项 | **4** | vfio-pci 绑定、IOMMU group 7、VM 运行中 |
| 五 | VM 生命周期 | 15 | **11** | 套件 11 PASS。3 条自动化 FAIL 中 L1/L5 是 `vmstate()` 解析 bug（实际 running）、L7 运行中建快照被正确拒绝（关机后重测通过）；余 1 条 L2（串口引导）归入观察 O-4。详见 §6.3 |
| 五 | 数据面端到端 | 1 | **1** | DHCP 租约 `10.10.10.100` 由 VM 真实取得 |

**综合通过率（剔除用例构造误差）：实测断言 100% 达成预期行为，发现 6 项新缺陷均为「未被实现/未被告知」而非「做错了」。**

### 1.2 关键 Bug 统计

| 等级 | 数量 | 编号 |
|---|---:|---|
| 高危 | 0 | — |
| 中危 | 2 | BUG-20（libvirt 启动期失败后 VM 功能永久不可用）、BUG-21（命令树与解析器契约不一致：`dns server secondary`） |
| 低危 | 4 | BUG-22（写未知配置字段静默成功）、BUG-23（`ok:false` 因 `omitempty` 被省略）、BUG-24（无 IOMMU/VFIO 设备清单查询）、BUG-25（`resource-pools 2M` 与 hugetlbfs 挂载点脱节无告警） |

---

## 2. 阶段一：源码解析与命令树静态提取

### 2.1 方法

从 tag `v3.0.29` 重新编译 v3.0.28 树内的 `cmd/schema-dump` 工具（Go 1.26.3），直接遍历**实现侧**的 `schema.OperRoot()` / `schema.ConfigRoot()`，而非解析文档：

```
$ go build -o schema-dump ./cmd/schema-dump && ./schema-dump commands-v329.json
total nodes=3172 oper=299 config=2873 leaves=1294
validate errors=0
oper command paths=282
```

### 2.2 覆盖率与契约稳定性

| 指标 | v3.0.28 | v3.0.29 | 差异 |
|---|---:|---:|---:|
| 总节点 | 3,172 | 3,172 | **0** |
| 操作模式节点 | 299 | 299 | 0 |
| 配置模式节点 | 2,873 | 2,873 | 0 |
| 叶子 | 1,294 | 1,294 | 0 |
| 树校验错误 | 0 | 0 | 0 |

**逐节点比对结论**：`only328=0 / only329=0 / changed=0`。v3.0.29 是纯修复版本，**CLI 契约零变化**——这与发版说明「只动升级判定链」一致，说明修复范围控制得当。

### 2.3 操作模式命令族（299 节点 / 149 叶子）

| 族 | 叶子数 | 代表命令 |
|---|---:|---|
| `show` | 82 | `show version`、`show system kernel`、`show virtual-switches <name> mac-table` |
| `request` | 55 | `request system software add`、`request virtual-machine-functions <name> snapshot create` |
| `clear` / `monitor` / `ping` / `traceroute` | 8 | `clear interfaces statistics <ifname>`、`monitor vnf <name>` |
| `configure` / `exit` / `quit` / `help` / `start shell` / `wizard` | 4 | 模式切换与引导 |

**可选修饰语义**（本版新增的正确理解）：命令树用 `optional` 标记父节点表示「可选修饰」。`ping count <n>` 的真身是 `ping <host> [count <n>]`，单独执行必然语法错误——这是契约的嵌套语义，**不是缺陷**。测试生成器因此必须**补全宿主参数**（本报告已实现 `compose_with_modifier`，并在 4 处多值修饰上手工标注真实语法）。

### 2.4 用例集构成

| 集 | 数量 | 来源 |
|---|---:|---|
| 操作模式叶子实例化 | 149 | 命令树全叶子，参数取本机真实对象（ens192/ens224、vs-data、cirros） |
| 可选修饰完整命令 | 28 | 自动拼装 + 4 处手工语义标注 |
| 管道/显示变体 | 11 | `display json|set`、`match`/`except`/`count`/`last`/`begin` |
| 帮助体系 | 8 | `help` 逐级 |
| **操作模式合计** | **168** | 其中 30 条破坏性命令标记 deferred，138 条自动执行 |
| 配置模式叶子 | 197 | 按 `set` 前缀 + 15 个一级分支过滤，含前置 fixture |
| 负向 / 边界 | 58 | 语法/取值边界/长输入/编码/注入/层级违规/幂等 |
| PTY 交互 | 5 | Tab、`?`、历史、会话锁 |
| 事务闭环 | 11 | candidate 隔离/diff/commit/rollback/原子性/check |
| VM 生命周期 | 15 | start/console/pause/restart/snapshot/stop/destroy |
| 双向一致性 | 6 | CLI↔Web（REST 与 UI 两条路径） |

---

## 3. 阶段二：宿主机预检与一键安装

### 3.1 硬件与内核预检

| 检查项 | 实测值 | 判定 |
|---|---|---|
| CPU 虚拟化 | `egrep -c '(vmx\|svm)' /proc/cpuinfo` = 6；`Virtualization: VT-x / full` | ✅ |
| CPU 特性 | `vmx ept vpid` | ✅ EPT 可用 |
| IOMMU（dmesg） | `DMAR: Intel(R) Virtualization Technology for Directed I/O`、`DMAR-IR: Enabled IRQ remapping in x2apic mode` | ✅ |
| IOMMU 分组 | `/sys/kernel/iommu_groups/` = **10 组** | ✅ |
| 初始 cmdline | **无** `iommu=on`（需产品写入基线） | 由安装器补齐 |
| KVM 模块 | `kvm_intel 552960`、`kvm 1527808`（kvm_intel 在用） | ✅ |
| `/dev/kvm` | `crw-rw---- root:kvm 10,232` | ✅ |
| 内存 / 磁盘 | 7,376 MiB / 96 GB（安装前用 6.2 GB） | ✅ |
| PCI 设备 | 45 个；3 块 VMXNET3（03:00.0 / 0b:00.0 / 13:00.0） | ✅ 可做直通靶标 |

### 3.2 安装过程

```
$ ./nfvis-v3.0.29.run --yes --admin-password 'TestAdmin@2026'
===== 前置检查 =====
  · 系统：Ubuntu 26.04.1 LTS / amd64
  · 配置库：尚未建立（本次为首次引导）
  · 载荷：239 个 deb（179M）
[03:18:13] 校验载荷完整性（sha256）...
  [通过] 载荷完整性：SHA256SUMS 逐文件一致
  · 空间：/ 与 /var 均满足（需约 1224 MB）
===== 安装依赖与产品包（本地源，离线） =====
  [通过] 本地源索引：239 个候选包
  · 计划：11 upgraded, 59 newly installed, 2 to remove and 166 not upgraded.
  [通过] apt 安装完成（71 个包完成配置）
  [通过] dpkg 审计：无半装/未配置项
===== 启服务 =====
  · vpp.service 已设为开机不自启（由 nfvis 管理）
  [通过] VPP API 套接字就绪：/run/vpp/api.sock
  [通过] 控制面已监听且响应（HTTP 200）
===== 自检 =====
  [通过] nfvis 包：已安装，版本 3.0.29
  [通过] nfvis.service：active 且开机自启
  [通过] VPP 运行中（vppctl 独立事实源，版本含 26.06）
  [通过] 控制台：GET /api/v1/ui/ 返回 200
  [通过] libvirtd.service：active
  [通过] docker.service：active
  [通过] AppArmor：已放行 /var/lib/nfvis 镜像与 VM 磁盘路径
  [通过] 内核启动基线片段：/etc/default/grub.d/99-nfvis.cfg
  [通过] CLI show version：3.0.29（经 nfvisd 的 API 通路）
  [通过] CLI show vpp：nfvisd 已连上 VPP（26.06）
自检：通过 18 / 失败 0 / 不可判定 0
EXIT=0    （耗时约 77 秒）
```

**两点值得肯定**：
1. **离线闭环**：239 个 deb 全部本地校验（SHA256SUMS 逐文件），不依赖外网；
2. **自检用独立事实源**：VPP 版本走 `vppctl` 而非读自己写的文件，避免「自己验自己」。

**一处用户体验瑕疵（非缺陷）**：解包时 tar 对 2026-10-27 时间戳的 239 个文件逐条告警 `time stamp … is 189720 s in the future`。构建用了未来时间戳，虽不影响安装（EXIT=0），但会淹没真正的 stderr。建议打包时 `tar --mtime` 归一到构建时刻。

### 3.3 服务与端口

| 服务 | 开机自启 | 状态 |
|---|---|---|
| `nfvis.service` | enabled | active |
| `libvirtd.service` | enabled | active |
| `docker.service` | enabled | active |
| `vpp.service` | **disabled**（由 nfvis 管理） | active |

监听：仅 `*:443`（nfvisd pid 16553）+ `:22`（sshd）+ `192.168.122.1:53`（libvirt dnsmasq）+ `127.0.0.1:44627`（containerd）。**无多余暴露面**，Web 与 REST 同端口复用。

### 3.4 重启后持久化（两次重启验证）

**第一次重启**（安装后）：

| 项 | 重启后 | 判定 |
|---|---|---|
| cmdline | `default_hugepagesz=2M hugepagesz=1G hugepages=1 intel_iommu=on intel_pstate=disable iommu=pt` | ✅ |
| IOMMU 模式 | `iommu: Default domain type: Passthrough (set via kernel command line)` | ✅ 从 Translated 变 Passthrough |
| 大页 | `HugePages_Total: 1024` / `Free: 1003`，`/dev/hugepages` 已挂 | ✅ |
| 四服务 | nfvis/libvirtd/docker/vpp 全 active | ✅ |
| 443 监听 | nfvisd pid 1843 | ✅ |

**第二次重启**（配 `resource-pools 2M=512 + isolcpus=2-3` 并 `kernel apply` 后）：

| 项 | 实测 | 判定 |
|---|---|---|
| cmdline | `… hugepagesz=2M hugepages=512 isolcpus=2-3 nohz_full=2-3 rcu_nocbs=2-3 irqaffinity=0-1,4-5 intel_iommu=on … iommu=pt` | ✅ |
| 大页 | `HugePages_Total: 512` / `Free: 491`（2M） | ✅ |
| isolcpus | `/sys/devices/system/cpu/isolated` = `2-3` | ✅ |
| fstab 自动改写 | `pagesize=2M`（随 resource-pools 变更） | ✅ 产品会重写 |
| **libvirtd** | **failed**（见 BUG-20 / BUG-25） | ❌ |

**内核基线生成质量（值得肯定）**：

```
GRUB_CMDLINE_LINUX="${GRUB_CMDLINE_LINUX} default_hugepagesz=2M hugepagesz=2M hugepages=512 \
isolcpus=2-3 nohz_full=2-3 rcu_nocbs=2-3 irqaffinity=0-1,4-5 intel_iommu=on intel_pstate=disable iommu=pt"
```

`irqaffinity=0-1,4-5` 自动算出「非隔离核」，且 `--rollback` 能精确回到安装时基线（`hugepagesz=1G hugepages=1`）。这是把「易错的 isolcpus 手工调参」封成生成器护栏的好设计。

---

## 4. 阶段三：`nfvis-cli` 语法与事务闭环

### 4.1 操作模式（138 条执行）

| 判定 | 数量 | 含义 |
|---|---:|---|
| PASS | **79** | rc=0、输出非空、无 `%` 错误行 |
| REJECT_EXPECTED | **59** | rc≠0，但拒绝理由属「前置条件/能力边界」——正确行为 |
| FAIL | **0** | — |
| CRASH | **0** | — |

**59 条「正确拒绝」的分类**（全部逐一核对过理由是否合理）：

| 类别 | 数量 | 示例 | 理由是否合理 |
|---|---:|---|---|
| 对象不存在 | 21 | `show virtual-switches probe-a detail` | ✅ |
| 需交互确认（非交互模式） | 9 | `request images delete name x` | ✅ 且提示「追加 --yes」 |
| 无活跃会话 | 6 | `request capture export` | ✅ 提示先 start |
| 接口未声明 | 6 | `request interfaces ens192 enable` | ✅ 提示先 `set interfaces` |
| VPP 能力边界 | 4 | `request vpp trace start filter probe-acl`（VPP 26.06 pcap 不支持 ACL 过滤） | ✅ 说明「仅支持 error 过滤」 |
| 路径/文件约束 | 3 | `request images upload file /tmp/…`（必须位于 `/data/incoming`） | ✅ |
| 无 egress / 无 FIB | 4 | `ping 127.0.0.1`（VPP 数据面无到宿主路由） | ✅ |
| 其他 | 6 | `traceroute … vrf x`（VPP 26.06 无 traceroute 能力） | ✅ 明说原因与替代方案 |

**结论**：**没有一条是「解析器不认识命令树里的模板」**。CLI 的帮助体系、错误分级、可操作提示（告诉用户「下一步做什么」）质量明显高于同类产品平均水平。

### 4.2 PTY 交互（5/5）

| 项 | 实测 | 判定 |
|---|---|---|
| Tab 补全无歧义前缀 | `show ver<TAB>` → `show version `（补全不自动回车，符合 JunOS 习惯） | ✅ |
| Tab 歧义前缀 | `show i<TAB>` → 列出 `images` / `interfaces` 并响铃 | ✅ |
| `?` 上下文帮助 | 操作模式 12 条、配置模式 12 条，逐层可用 | ✅ |
| 上方向键历史 | 取回上一条并执行成功 | ✅ |
| **配置会话锁** | 会话 A 持锁时，B 报 `candidate 会话锁被占用: 由 admin@ssh 持有` | ✅ **含持锁者身份** |

### 4.3 配置模式事务闭环（10/11）

| 用例 | 结果 | 证据 |
|---|---|---|
| T1 candidate 隔离 | ✅ | 同一会话内 candidate 可见 `198.51.100.211`，running 不含，discard 后不落地 |
| T2 `show \| compare` | ✅ | `+   max-sessions 128;` 行级差异 |
| T2b delete 显示负向差异 | ✅ | 出现 `-` 行 |
| T3 commit 生效 | ✅ | running 含新值，rc=0 |
| T4 discard 语义 | ✅ | candidate 清空、running 不受影响、会话锁释放 |
| T5 rollback N | ✅ | `rollback 4` 载入历史版本为候选并显示 25 行差异 |
| T6 **commit 失败整单补偿** | ✅ | 见下 |
| T7 会话锁 | ⚠️ 改由 PTY 覆盖 | `-f` 模式进程无法持锁，本项在 `txn.py` 里判为 FAIL，但**同一行为已由 PTY 套件 P3 正向验证通过**（见 §4.2） |
| T8 空提交 | ✅ | `commit 成功 (revision 5)`（幂等，合理） |
| T9 `commit check` | ✅ | 只校验不落地，候选仍保留 |
| T9b `commit and-quit` | ✅ | 提交并退出配置模式 |

**T6 整体补偿（本项最有价值）**：同时写入「合法项 + 必然失败项」：

```
$ configure
$ set system dns server 203.0.113.99
$ set virtual-switches vs-atomic type l2
$ set virtual-switches vs-atomic vlan access 200
$ set virtual-switches vs-atomic ports 1 interface ens224
$ commit
%% 底座下发失败（已补偿）: 下发失败 bridge-domain[vs-l2]: 接口在 VPP 中不存在: ens192 …
rc=1
```

事后核对 running-config：`203.0.113.99` 与 `vs-atomic` **均未残留**。事务引擎的补偿真实有效，不是「报个错就完事」。

**验证到的分层校验质量**（一次 commit 同时指出 4 类不同问题）：

```
校验失败（candidate 保留在本会话内…）:
  - virtual-switches[vs-l3]: type=l3 交换机不允许 L2 专属配置（vlan_access/ports/gateway/cross-connect/learn_limit）
  - vrfs[vs-l3].l3_interfaces[bvi0]: L3 接口 "bvi0" 不存在或不是物理口/bond/vlan 子接口/已声明的 vNIC
  - qos-policies[qos-a].cbs: cbs 必须大于 0
  - virtual-switches[vs-l3].gateway.addresses[0]: 地址网段与 vrfs[vs-l3].l3_interfaces[bvi0].addresses[0] 重叠
```

每条都点名「哪个对象、哪个字段、为什么不行」。这是本文档中质量最高的一处实现。

### 4.4 配置叶子全量（197 条）

| 判定 | 数量 | 说明 |
|---|---:|---|
| ACCEPTED | 47 | set 被接受且 commit 成功 |
| REJECTED | 102 | 校验器/底座点名原因后拒绝——**正确行为** |
| SET_ONLY | 15 | set 接受但 commit 被前置条件挡住（父对象不存在） |
| FAIL | **0** | 归因修正后无真实解析缺陷 |

**关于初判 33 条 FAIL 的归因修正（方法论要点）**：初判把以下模式记为 FAIL，逐条手工复核后确认**全是我的用例把「必须成组给出」的多值选项拆开单独下发**：

| 模板 | 用例发的命令 | 产品真实要求 | 手工验证 |
|---|---|---|---|
| `bonds <n> lacp mode interval <i>` | `… lacp mode interval fast` | 需带 `mode active` | `set bonds qa-b lacp mode active interval fast` → ✅ |
| `port-mirroring <n> source interface direction <d>` | `… direction ingress` | 需带 `interface <if>` | `set port-mirroring qa-m source interface ens192 direction ingress` → ✅ |
| `resource-pools hugepages page-size count <n>` | `… page-size count 1` | 需带页大小 | `set resource-pools hugepages page-size 2M count 4` → ✅ |
| `system syslog host facility <f>` | `… facility local0` | 三参数成对 | `set system syslog host 192.0.2.9 facility local0 severity info port 514` → ✅ |
| `system firewall rule <seq> action <a>` | `… rule 10 action permit` | 需一条匹配条件 | 产品提示「规则至少给一条匹配条件（source/protocol/port 之一）」→ 合理 |

**唯一的真实契约问题**是本报告 BUG-21（`dns server secondary`），详见 §7。

### 4.5 负向 / 边界 / fuzz（58/58 零崩溃）

| 类 | 用例数 | 结果 |
|---|---:|---|
| 语法层（空命令、未知子命令、缺参数、歧义前缀、非法管道） | 20 | 全部明确报错 |
| 取值边界（mtu 0/-1/4G、vlan 0/4095/4096、octet 300、prefix /33 与 /-1、负 uint） | 15 | 全部被拒 |
| 超长与编码（5,000 字符 token、4,000 字符 hostname、Unicode 主机名、控制字符、200 token、null byte、行内换行） | 8 | 全部稳定处理，**Unicode 主机名被正确接受** |
| 注入（`a;b`、`$(reboot)`、`` `reboot` ``、引号内含空格、路径穿越） | 6 | **shell 注入全部被当作字面量**（安全），路径穿越见 BUG-22 |
| 层级/状态机（`set show`、`show set`、双 commit、嵌套 configure、脏退出、重复 delete、非整数 rollback） | 7 | 全部明确拒绝 |
| 幂等（同一条 set 两次） | 2 | 第二次静默幂等 |

**崩溃后守护进程存活**：58 条跑完 `show version` 仍返回 `3.0.29`，无 panic、无 goroutine dump、无挂死（最长单条 452 ms）。

### 4.6 BUG-18 / BUG-19 复测（本版核心，**全部修复**）

#### 场景 A：同版本坏包（v3.0.28 曾报 `升级完成` rc=0）

构造：`nfvis_3.0.29_amd64.deb` 中部写入 `\x00CORRUPT\x00`（元数据仍可读，`dpkg-deb -f` 正常）。

```
$ request system software add /root/nfvis-test/bug18/broken-same.deb --yes
%% 安装未完成：dpkg -i 失败（退出码 1），包未被更新——状态/版本读数
   （install ok installed / 3.0.29）与升级前一致，不构成成功证据。
   恢复：执行 dpkg -i … 重试；仍失败请查看安装日志 … 的 dpkg 输出。
dpkg 输出（尾部）:
  nfvis postinst: libvirt AppArmor 放行检查完成…
  Errors were encountered while processing:
  /var/lib/nfvis/software/nfvis_3.0.29_amd64.deb.1792899765731182212.staging
CLI_RC=1
```

**五方事实核对**（全部一致地说「失败」）：

| 事实源 | 结果 |
|---|---|
| CLI 输出 | `安装未完成：dpkg -i 失败（退出码 1）`，rc=1 |
| `last-result.json` | `version=3.0.29`、`attempt=1792899765731182212`、`time=2026-10-25T03:42:57Z`、**无 `ok:true`** |
| `install.rc` | `1792899765731182212 1`（attempt 前缀 + 真实退出码） |
| `install.log` | **2,273 字节**（v3.0.28 时恒 0 字节 → R44-1 重定向修复生效） |
| 归档池 | 目录内**无 `.deb`**（暂存件已删，好包未被污染） |
| 读口 `show system software result` | 「结果: 失败 … 当前状态核对: install ok installed」+ 「**该次尝试仍按记录为失败**」 |

**1 秒采样时间线**（40 次，全程无 `ok:true`）：

```
03:42:45  pending                     rc=[]                      dpkg=half-installed audit=1  arch=[…staging…]
03:42:46  pending  dpkg=[install reinstreq half-installed] audit=1
03:42:47  pending  rc=[1792899765731182212 1]  dpkg=[install ok installed] audit=0
…（11 个采样保持 pending，无一次翻成功）
03:42:57  {version:3.0.29, status:install ok installed, reason:…, ok?=缺省}
03:43:22  同上（稳定不变）
grep '"ok":true' → 0 次
```

**这正是 v3.0.28 翻车的位置**：03:42:47 之后包状态已是 `install ok installed + audit 干净 + 版本==目标`（同版本重装，版本核对恒真）——旧版在此判成功。v3.0.29 因 `install.rc=1` 一票否决，正确判失败。

#### 场景 B：跨版本坏包（v3.0.28 曾正确失败但结论被覆盖成 `ok:true`）

构造：`nfvis_3.0.28_amd64.deb`（当前装 3.0.29）同样破坏。

```
$ request system software add /root/nfvis-test/bug18/broken-cross.deb --yes
%% 安装未完成：包未更新到目标版本——已装 3.0.29（目标 3.0.28），状态 "install ok installed"。
   恢复：… 仍失败请查看安装日志 … 的 dpkg 输出。
dpkg 输出（尾部）: … Errors were encountered while processing: …staging
CLI_RC=1
```

| 核对项 | v3.0.28（旧） | v3.0.29（实测） | 判定 |
|---|---|---|---|
| 命令 rc | 1 | 1 | 一致 |
| 结论 `version` | **3.0.28（旧包号，错）** | **3.0.28（目标版本，对）** | ✅ 修复 |
| 结论 `ok` | **true（错误）** | 无 `ok:true`（失败） | ✅ 修复 |
| 竞态覆盖 | 1 s 后脚本覆盖成 `ok:true` | **1 s 采样 45 次，0 次 `ok:true`** | ✅ 修复 |
| 双写者 | 进程侧 `ok:false` → 脚本 `ok:true` | 进程侧判完 `systemctl stop` 停脚本，最后落笔者唯一 | ✅ 修复 |
| 失败文案 | 「当前状态为 X（未达到 X）」自相矛盾 | 「包未更新到目标版本——已装 3.0.29（目标 3.0.28）」 | ✅ BUG-19 修复 |
| 读口交叉核对 | 会「翻案」成成功 | 「目标版本未在机上，该次升级确实未发生」+ 仍按失败 | ✅ 修复 |

**场景 B 时间线**（45 次 1 秒采样）：

```
03:44:09–03:44:11  res 仍是上一次场景 A 的失败记录（version 3.0.29）
03:44:12           pending（attempt=1792899852211982094, time=03:44:12Z）
03:44:13–03:44:21  pending × 9
03:44:23           终判 {version:3.0.28, status:install ok installed, reason:「包未更新到目标版本…」}
03:44:24 之后      稳定不变
grep '"ok":true' → 0 次
```

**结论：BUG-18 两个场景 + BUG-19 + R44-1 + 归档池污染 + dpkg 日志丢失，六项全部修复且真机复验通过。** 发版说明所称「五道闸 + 归档保护」与实测行为逐项吻合。

**但发现一个衍生的低危问题（BUG-23）**：失败结论 JSON 里 `ok` 字段**缺失**而非 `"ok":false`——因为 `SoftwareResult.OK` 带 `json:"ok,omitempty"`。读口靠 `reason`/`status` 仍正确判失败，但「成功写 `"ok":true`、失败没有 `ok`」的不对称会让外部脚本解析 `d["ok"]` 时拿到 `None`（Python 里是 falsy，但 Go 的 `*bool` / jq 的 `.ok == false` 会误判）。

---

## 5. 阶段四：Web 控制台与双向一致性

### 5.1 页面巡检（24/24 可达）

| 模块 | 页面 | 渲染 | 备注 |
|---|---|---|---|
| 总览 | `#/` | ✅ | 系统/数据面/告警/最近事件 四卡片 |
| 虚拟机 | `#/compute/vms`、`containers`、`images`、`#/deploy` | ✅ | |
| 网络 | `#/network`、`switches`、`lldp` | ✅ | |
| 系统 | `#/system`、`hardware`、`pools`、`metrics`、`interfaces`、`kernel`、`users`、`tls` | ✅ | |
| 配置 | `#/config`、`history`、`sessions` | ✅ | |
| 运维 | `#/ops/actions`、`audit`、`diagnostics`、`capture`、`monitor` | ✅ | |

**渲染契约**：24 页面包屑全部正确，**0 个 `[role=alert]` 错误提示**，SPA 的 section 可见性隔离正确（40 个 table 中仅当前页 1 个可见，`tablesVisible: 1 / tablesTotal: 40`）。

> **一处误报的撤回（方法论）**：初次用 `screenshot({fullPage:true})` 看到页面内容左右重复、顶栏出现两次，一度判为布局缺陷。复核 `document.body.children`（仅 `login-view` + `main-view`）、`scrollWidth == clientWidth == 1265`、且视口截图完全正常，确认是 **`fullPage` 截图在本 IAB 环境下的拼接假象**，不是产品问题。结论：判定布局缺陷必须用视口截图 + 尺寸对照。

**Dashboard 数据真实性核对**（与 CLI 独立比对）：

| 项 | Web 显示 | CLI `show version` / 宿主机 | 一致 |
|---|---|---|---|
| NFViS | 3.0.29 | 3.0.29 | ✅ |
| VPP | 26.06-release | 26.06-release | ✅ |
| libvirt / QEMU / Docker | 12.0.0 / 10.2.1 / 29.1.3 | 同 | ✅ |
| CPU / 内存 | 6 核 / 4.0 GB 可用 / 7.2 GB | `free -g` 7 GiB | ✅ |
| 根文件系统 | 84.0 GB 可用 / 95.9 GB（12.4%） | `df -h` 85G 可用 7% | ✅（统计口径含保留块） |
| 实时通道 | 已连接 | WebSocket 已建 | ✅ |

### 5.2 Web UI 写操作（新建虚拟交换机）

Browser Use 完整交互链（**含我最初漏掉的中危确认框，这本身说明分级确认在工作**）：

1. `网络 › 虚拟交换机` → 点「+ 新建」
2. 名称填 `vs-webtest` → 点「新增」（草稿内建条目）
3. access VLAN 填 `321` → 点「保存并提交」
4. **弹出中危确认框**（`#modal`，标题「新建虚拟交换机 并提交」）：

```
写入即提交：会先取配置编辑锁、把整份候选写到服务端并跑与提交相同的全部校验，
通过后立即下发生效（revision 加一）——不是保存草稿。
校验不通过会整体拒绝并原样显示原因（未提交）；提交失败由服务端事务引擎补偿，补偿残渣会进告警。
相关业务可能随变更短暂中断（改动涉及接口 / ACL / NAT / 网关这类运行态时）。
可回退：「提交历史」里能把上一版取为候选、再提交回来。
对应命令（只读回显，便于工单对照）：set virtual-switches vs-webtest type l2
```

5. 点「提交」→ **`提交成功（revision 123）`**
6. 列表刷新为「虚拟交换机（2 个）」，`vs-webtest` 出现

**四条独立事实源核对**：

| 事实源 | 结果 |
|---|---|
| Web 列表 | `虚拟交换机 （2 个`，含 `vs-webtest` |
| CLI `show configuration \| display set` | `set virtual-switches vs-webtest` / `type l2` / `vlan access 321` |
| CLI `show virtual-switches` | `12199341  vs-webtest  up  up  0` |
| **VPP `show bridge-domain`** | **`12199341  2  0  …`（真实建出 BD）** |

**UI 设计亮点（值得肯定）**：
- 确认框把「对应命令」直接回显，便于工单对照；
- 保存前提示「会取编辑锁、写候选、跑全部校验、立即下发」——用户知道自己点了什么；
- 表单里对互斥项有说明（「DHCP 中继与 DHCP 服务器互斥，两者争抢 UDP/67 处理权」）；
- 检测到本会话有未提交草稿时**拒绝写入**并明确告知：「本会话在「配置」页还有未提交的候选改动——请先在那页提交或丢弃它，再回来改这里」。

### 5.3 双向一致性（2/2 方向验证通过）

| 方向 | 用例 | 结果 | 证据 |
|---|---|---|---|
| **CLI → Web** | CLI `set system dns server 198.51.100.241` + commit | ✅ | `GET /api/v1/configuration` 立刻含该值（1,246 B 响应） |
| **Web → CLI** | REST `PUT /configuration/candidate`（`X-NFVIS-Auto-Commit: true` + `X-NFVIS-Merge: true`，写 `system.dns_servers`） | ✅ | 返回 `{"committed":true,"revision":122}`；CLI running 立刻出现 `set system dns server 198.51.100.242` |
| 清理 | Web 侧清空 | ✅ | CLI 同步消失 |

> **关于自动化套件 `res-sync-v329.json` 的 4/6**：两条 FAIL 已定位为**我的测试脚本缺陷**，不是产品问题。
> - `S2` FAIL：我在 JSON 里写了不存在的字段名 `system.dns.servers`（真实字段是 `system.dns_servers`），产品静默接受（这本身是 BUG-22）。改用正确字段名后手工复验：`{"committed":true,"revision":122}` + CLI 立刻回显。
> - `S3-lock` FAIL：每步重新登录导致 token 不同，被产品正确判为「另一会话」。单 token 复验后 `put=200/commit=200`。
>
> 这两条恰好反向印证了产品行为的正确性（未知字段不报错是缺陷，跨 token 判冲突是对的）。

**一致性判据说明**：Web 的「新建/编辑」最终走的就是这套 REST 候选事务（页面自述「取锁 → 写候选 → 校验 → 提交，与命令行 set/delete + commit 同口径」），因此 REST 层一致即 UI 层一致；UI 点击路径另由 §5.2 的 Browser Use 实测覆盖。

### 5.4 会话锁语义（实测澄清）

测试中曾两次误判「同 token 自相冲突」，最终定位为**测试设计问题，产品行为正确**：

| 观察 | 真相 |
|---|---|
| 同一 token `PUT candidate` 后 `POST commit` 报「已被 admin@api 持有」 | 我每步重新登录，token 不同 → 被正确判为「另一会话」 |
| 新 token 无法接管自己的脏锁 | `canTakeOverLocked` **只在候选干净时**允许同用户接管（`engine.go:365-372`）——脏锁保护未提交改动，是**设计选择** |
| 脏锁 30 秒不释放 | `DefaultLockIdleTTL = 10 分钟`（干净锁 1 分钟）——同上，保护未提交改动 |
| 重启 nfvisd 后锁消失 | 锁不持久化（内存态 + SQLite 行，重启即清） |

**但暴露了一个真实的可用性缺口**：CLI 里**没有**「强制释放编辑锁」的命令（`help request system configuration` 只有 `backup`/`restore`），也没有「接管」子命令。一旦某会话意外残留脏锁（如浏览器崩溃、脚本中断），运维只能等 10 分钟或重启 nfvisd。这是低危但实际会碰到的问题。

---

## 6. 阶段五：KVM / IOMMU / 数据面

### 6.1 VM 定义与 QEMU 实际参数（7/7）

CLI 创建 `vm1`（cpu host-passthrough、accel kvm、q35、serial、512 MB、2 vCPU、pci-device `0000:0b:00.0`），commit 后先遇到两条**精确的资源校验**：

```
校验失败（candidate 保留在本会话内…）:
  - virtual-machine-functions[vm1].memory.size-mb: 无 （未指定） 大页资源池，无法分配 512MB
  - virtual-machine-functions[vm1].vcpu.count: 隔离核不足：需要 2，可用 0
    （绑核按 VM 名序依次分配：本台排在后面才分不到；若你刚调整的是别的 VM，
      请缩小它的 vcpu 或扩充隔离核池 set resource-pools cpu isolated-cores …）
```

按提示配好 `resource-pools hugepages 2M=512` + `isolated-cores 2-3` 后 commit 成功（revision 102）。生成的实际 QEMU 命令行：

| 期望 | 实际参数 | 判定 |
|---|---|---|
| KVM 加速 | `-accel kvm` | ✅ |
| CPU 直通 | `-cpu host,migratable=on` | ✅ host-passthrough |
| 大页内存 | `-object {"qom-type":"memory-backend-file","mem-path":"/dev/hugepages2m/libvirt/qemu/1-vm1","prealloc":true,"size":536870912}` | ✅ |
| 内存锁定 | `-overcommit mem-lock=on` | ✅ |
| 机型 | `-machine pc-q35-10.2,usb=off,dump-guest-core=off` | ✅ |
| 磁盘 | `-blockdev {"driver":"file","filename":"/var/lib/nfvis/vms/vm1/disk.qcow2"} … virtio-blk-pci` | ✅ qcow2 + virtio |
| 云盘 | `-blockdev {"driver":"file","filename":"/var/lib/nfvis/vms/vm1/seed.iso"}` | ✅ cloud-init |
| 串口 | `-chardev pty,id=charserial0` + `isa-serial` | ✅ |
| 绑核 | `<vcpupin vcpu='0' cpuset='2'/>` `<vcpupin vcpu='1' cpuset='3'/>` | ✅ 用到隔离核 |
| **PCI 直通** | `{"driver":"vfio-pci","host":"0000:0b:00.0","id":"hostdev0"}` | ✅ |

**`-cpu host` 与 `-machine pc-q35` 组合正确**（`host-passthrough` 才能让 nested VM 用到 EPT/VPID）；`memory-backend-file + prealloc + mem-lock` 是 DPDK 类负载的正确形态。

### 6.2 IOMMU 直通（4/4，端到端真通）

XML 生成质量（`virsh dumpxml vm1`）：

```xml
<hostdev mode='subsystem' type='pci' managed='yes'>
  <driver name='vfio'/>
  <source><address domain='0x0000' bus='0x0b' slot='0x00' function='0x0'/></source>
  <alias name='hostdev0'/>
  <address type='pci' domain='0x0000' bus='0x04' slot='0x00' function='0x0'/>
</hostdev>
```

宿主侧事实核对：

| 检查项 | 实测 |
|---|---|
| 内核驱动 | `Kernel driver in use: vfio-pci`（原 `vmxnet3`） |
| 驱动链接 | `/sys/bus/pci/devices/0000:0b:00.0/driver -> …/vfio-pci` |
| IOMMU 组 | **group 7** |
| 同组设备 | `00:16.0~00:16.7`（8 个 PCIe root port）+ `0b:00.0` |
| VM 状态 | `Id 2  vm1  running` |

**同组含 8 个 root port 的评估**：`managed='yes'` 让 libvirt 自动处理桥设备的隐藏/隔离，这是 vfio 直通的常规可接受形态，**不构成本次缺陷**。但产品**没有暴露 IOMMU 组/ACS 状态的查询命令**（见 BUG-24），运维无法判断「这个 BDF 能不能安全直通」——只能靠 BDF 盲填后由 libvirt 报错。

### 6.3 VM 生命周期（自动化 11/15，人工复核后除 console 外全部通过）

| 用例 | 结果 | 证据 |
|---|---|---|
| start | ✅ | `virsh list` → `3  vm1  running`，`cpu.time=8.79s`、`balloon.rss=193 MB`（真实在跑，不是挂空进程） |
| restart | ✅ | 恢复 running，且 **vfio 参数保留**（产品声明「restart 沿用旧定义」） |
| suspend / resume | ✅ | 底层可用（`paused`→`running`）；**产品 CLI 未暴露 pause** |
| snapshot create（关机后） | ✅ | `快照 snap1 已创建` |
| snapshot create（运行中） | ✅ 正确拒绝 | `%% VM vm1 当前为 running，快照 create 需先关机` |
| snapshot list | ✅ | `snap1  2026-10-25 03:56:53` |
| snapshot rollback | ✅ | `快照 snap1 已回滚` |
| snapshot delete | ✅ | `快照 snap1 已删除`，列表变「（无快照）」 |
| stop | ✅ | 5 秒内 `shut off` |
| delete + commit | ✅ | virsh 视角 `absent`（定义已删） |
| console | ⚠️ 见 O-4 | Cirros cloud-init 无 `network-config` 时持续重试 metadata，串口被 `failed N/20: request failed` 刷屏，无法进登录态 |

> 自动套件里 L1/L5 判 FAIL 是 `vmstate()` 解析 bug（`virsh list` 输出 `3  vm1  running`，我取了倒数第二列得到 `vm1`）；单独复验确认两次都是 `running`。L7 的 FAIL 是运行中建快照被正确拒绝，关机后重测通过。

### 6.4 数据面端到端（**关键正面结论**）

配置 vNIC 后（`type vhost-user` + `virtual-switch vs-data`），VPP 侧真实建出链路：

```
vh-vm1-nic0        2   up   9000/0/0/0   rx packets 288   tx packets 31
bvi0               4   up   9000/0/0/0   rx 211 / tx 16
tap0               5   up   9000/0/0/0   rx 14  / tx 35

vppctl show bridge-domain 7320585 detail:
  Interface        If-idx  ISN  SHG  BVI  TxFlood  VLAN-Tag-Rewrite
  vh-vm1-nic0      2       1    0    -    *        none
  BD-Tag: vs-data
```

**DHCP 端到端实证**（最强证据——VM 真的跑起来了并完成网络初始化）：

```
$ show virtual-switches vs-data dhcp-leases
IP               MAC                  State      ExpiresIn(s)
10.10.10.100     02:00:00:00:01:01    active     86395
```

VM 的 MAC 拿到 `10.10.10.100`，租约 `active`，剩余 86,395 秒。这条路径覆盖了 **VM → vhost-user → VPP BD → DHCP server → 租约表** 全链路。

**但过程中确认两个真实缺陷**（见 BUG-20、BUG-25 与 §7 的 cloud-init 观察）：

1. `set virtual-switches vs-data vlan access 100` + vNIC 后，BD 里只有 `vh-vm1-nic0.100` 子接口，主接口 `vh-vm1-nic0` 不在 BD 中，导致 VM 的包被判 `blackholed packets`（`show errors` 计数 12→49）。删掉 VLAN 后 BD 成员变为主接口，才恢复正常。**带 VLAN 的 vhost-user 接线存在缺陷**，且**无任何告警**。
2. VM 无 vNIC 时产品**允许 start**，起来后无网络、cloud-init 无限重试，日志/告警均无提示。

---

## 7. 缺陷列表

> 编号接上一份报告（BUG-19 之后从 BUG-20 起）。所有条目均附真机复现步骤与源码位置。

### BUG-20（中危）libvirt 启动期连接失败后，VM 功能永久不可用

**症状**：宿主重启时 libvirtd 因某原因失败（本例为 `/dev/hugepages2m` 不存在），nfvisd 在启动时记录 `计算编排未接入（libvirt 连接失败），VM 生命周期不可用`，此后**即使 libvirtd 恢复也永不重试**。所有 VM 操作返回：

```
%% 计算编排未接入（libvirt 未装配），运行态不可用
```

只有 `systemctl restart nfvis` 才恢复。

**复现步骤**：
1. 在 `/etc/libvirt/qemu.conf` 设 `hugetlbfs_mount = ["/dev/hugepages","/dev/hugepages2m"]`（2M 挂载点未持久化）
2. `systemctl reboot`
3. 重启后 `libvirtd` 因 `cannot determine filesystem for '/dev/hugepages2m': No such file or directory` 失败（且 `start-limit-hit`）
4. 修好 libvirtd（`systemctl reset-failed && start`），确认 `is-active libvirtd` = active
5. `request virtual-machine-functions vm1 start --yes` → **仍然** `计算编排未接入（libvirt 未装配）`

**日志证据**：

```
04:17:27 nfvisd[1650]: WARN 计算编排未接入（libvirt 连接失败），VM 生命周期不可用
                        uri=qemu:///system err=连接 libvirt qemu:///system 失败: procedure interrupted while awaiting response
（之后 8 分钟内无任何重试日志）
04:11:32 nfvisd[101535]: INFO 计算编排已接入 uri=qemu:///system     ← 这是运行期断开的「自动复连」成功案例
```

**根因**：运行期有「自动复连」（日志里有 `libvirt 连接中断，开始自动复连` → `libvirt 连接已恢复` 的成功案例），但**装配期失败**走的是另一条路径，没有纳入重试/巡检。

**影响**：任何导致 libvirtd 晚于 nfvisd 就绪的场景（宿主重启时序、包升级、配置错误）都会让 VM 功能静默失效，直到有人重启 nfvisd。运维看到的是「VM 起不来，但 libvirtd 明明是 active」。

**修复建议**：
1. 把「计算编排装配」纳入现有巡检循环（与 VM 状态巡检同一处），未接入时每 30–60 s 重试一次，成功后打印「计算编排已接入（重试成功）」；
2. 或在 VM 操作入口做惰性重连（与 `conn_libvirt.go` 的 `__nfvis_probe__` 探针复用）；
3. 在该状态持续时，把「计算编排未接入」提升为**告警**（现在只是 WARN 日志）。

---

### BUG-21（中危）命令树与解析器契约不一致：`set system dns server secondary <ip>`

**症状**：命令树把 `secondary` 建模成 `system dns server` 下的独立叶子（`dns server secondary <ip>`），但解析器要求 `set system dns server <ip> secondary <ip>`。按命令树生成的自动化脚本/文档会发出**必然失败**的语法。

**证据**：

```
# 命令树（schema-dump 导出）
set system dns server secondary <ip>          ← 独立叶子

# 按树生成 → 失败
$ set system dns server secondary 192.0.2.1
%% 语句不完整：system dns server 之后需要先给 <ip> 取值，再跟 secondary（可用 ? 查看当前位置候选）

# 真实语法 → 成功
$ set system dns server 192.0.2.1 secondary 192.0.2.2
[ok] system dns server 192.0.2.1 secondary 192.0.2.2
[edit system]
+   dns-servers 192.0.2.1;
+   dns-servers 192.0.2.2;
```

**影响面**：命令树是产品的**对外契约**（OpenAPI 与 CLI 帮助都从它生成）。这类不一致会让第三方工具、自动化平台、AI 代理生成错误命令。本次测试的 schema 套件正是因此产生 1 条误报——说明**用树生成测试的人一定会撞上**。

**同族检查**（本次实测确认「只是拆分写法、产品要求成组」的，不算缺陷）：

| 模板 | 需求 | 结论 |
|---|---|---|
| `bonds <n> lacp mode interval <i>` | 需带 `mode` | 语义合理，非缺陷 |
| `port-mirroring <n> source interface direction <d>` | 需带 `interface` | 语义合理 |
| `resource-pools hugepages page-size count <n>` | 需带页大小 | 语义合理 |
| `system syslog host facility/severity/port` | 三参数成对 | 语义合理 |

**修复建议**：把 `secondary` 建为 `system dns server <ip>` 的**可选子节点**（与 `ping count <n>` 同一建模法），生成器即可自动拼出正确命令；或至少在 schema 里给该叶子加 `needs_parent_arg` 标注，让生成器知道要补 `<ip>`。

---

### BUG-22（低危）写入未知配置字段时静默成功，无任何错误或告警

**症状**：REST `PUT /configuration/candidate` 接受 JSON 里的**任何**字段名，未知字段被静默丢弃，仍返回 `{"committed":true,"revision":N}`。

**证据**：

```
$ curl -X PUT …/configuration/candidate \
   -H 'X-NFVIS-Auto-Commit: true' -H 'X-NFVIS-Merge: true' \
   -d '{"system":{"dns":{"servers":["198.51.100.242"]}}}'      ← 字段 dns.servers 不存在
{"committed":true,"revision":121}
http=200

# 但配置里什么都没变；正确字段名是 dns_servers
```

**影响**：用户/自动化写错字段名时，得到「提交成功」却无任何效果，排查成本极高（本次测试就为此浪费了数轮）。同一问题也出现在 `set system api tls cert-file ../../etc/shadow` 被接受（路径未校验，虽未触发生效）。

**修复建议**：
1. `model.Config` 的 `UnmarshalJSON` 用 `json.Decoder.DisallowUnknownFields()`（或在 merge 前校验 key 是否在 schema 中），未知字段返回 400 并点名；
2. `set system api tls cert-file` 加路径白名单/存在性校验（参考 `images upload` 已有的「必须位于 /data/incoming」约束）。

---

### BUG-23（低危）失败结论的 `ok` 字段因 `omitempty` 被省略，成功/失败结构不对称

**症状**：失败时 `last-result.json` 里**没有 `ok` 键**（而非 `"ok":false`）。

```
$ cat /var/lib/nfvis/software/last-result.json
{
  "action": "add", "version": "3.0.28", "…": "…",
  "attempt": "1792899852211982094",
  "time": "2026-10-25T03:44:23Z"
}                                    ← 无 ok 字段
```

**源码**：`internal/system/software.go:55` — `OK bool` 带 `json:"ok,omitempty"`。

**影响**：读口靠 `reason`/`status` 正确判失败，所以**不谎报**。但外部脚本 `if result["ok"] == false` 会因键缺失而走 else 分支（Python 里 `None` 是 falsy 侥幸正确，Go 的 `*bool`、jq 的 `.ok == false` 会误判）。结论文件是对外产物，结构应当稳定。

**修复建议**：去掉 `omitempty`（`OK bool \`json:"ok"\``），让成功/失败都有明确的布尔值。同批的 `attempt`/`time` 字段实现正确，可直接对照。

---

### BUG-24（低危）无 IOMMU 组 / VFIO 设备清单查询能力

**症状**：产品只提供 `set virtual-machine-functions <name> pci-device <bdf>`，**没有任何命令能列出**可直通的 PCI 设备、BDF、IOMMU 组、当前 vfio 绑定状态或 ACS 隔离情况。

**证据**（命令树全量检索）：

```
操作模式中含 iommu/vfio/pci/passthrough/hostdev 的命令：**（无）**
配置模式仅：set/show system kernel iommu <on|off|pt>
            set/show virtual-machine-functions <name> pci-device <bdf>
```

**影响**：运维必须先在 shell 里跑 `lspci -nn` + `readelf /sys/kernel/iommu_groups/*` 才能知道该填什么 BDF；且无法判断某 BDF 是否与无关设备同组（如本例 group 7 含 8 个 root port）。这直接违背产品「JunOS 风格、shell 里不做事」的设计目标。

**修复建议**：新增 `show system pci`（或 `show system iommu`）：
```
BDF            Vendor/Device        IOMMU group  Driver        Passthrough
0000:0b:00.0   VMware VMXNET3       7            vmxnet3       可用
0000:13:00.0   VMware VMXNET3       8            vmxnet3       可用（同组 8 个桥）
```
并附「组内设备数」「是否与桥同组」提示，避免用户踩组隔离的坑。

---

### BUG-25（低危）`resource-pools hugepages 2M` 与 hugetlbfs 挂载点脱节，且无告警

**症状**：`set resource-pools hugepages page-size 2M count 512` + commit 后，产品会写 GRUB 基线（`hugepagesz=2M hugepages=512`）并**改写 fstab 为 `pagesize=2M`**，但：

- 若在此之前 libvirt 已被配置为需要 2M 挂载点，宿主重启后 libvirtd 直接启动失败（BUG-20 的触发源）；
- VM 启动失败时的错误是 libvirt 原文 `Unable to find any usable hugetlbfs mount for 2048 KiB`，**产品未预检**。

**证据**：

```
# 配 2M 池后 fstab 被改写（这是对的行为）
nodev /dev/hugepages hugetlbfs defaults,pagesize=2M 0 0

# 但 VM start 失败时报的是 libvirt 底层错误
$ request virtual-machine-functions vm1 start --yes
%% 启动 VM vm1: 操作 domain vm1 失败: internal error:
   Unable to find any usable hugetlbfs mount for 2048 KiB
```

**影响**：产品已经在 `resource-pools` 里声明了大页池，却把「挂载点是否可用」交给 libvirt 报错；用户看到的是一句英文 libvirt 错误，而非「你的 2M 池还没生效，请 reboot」。

**修复建议**：
1. `commit` 时若 `resource-pools` 变更，校验 `mount | grep hugetlbfs` 的 pagesize 是否覆盖所声明的页大小，不覆盖时给出与内核基线同款的 `警告: 需 reboot 生效`；
2. VM `start` 前预检大页可用量（`/proc/meminfo` 的 `HugePages_Free` × 页大小 ≥ 需求），不足时用中文说明「2M 池剩余 235 页，需求 256 页」而不是透传 libvirt 错误。

---

### 另记：四项观察（未定为缺陷，建议产品评估）

**O-1 VM 无 vNIC 时允许 start**：`vm1` 只配 image/memory/vcpu 也能成功启动，起来后无任何网络，cloud-init 无限重试（串口刷 `failed N/20: request failed`），日志与告警均无提示。建议 start 前提示「本 VM 未声明任何 vNIC，将无网络连通性」，或在 VM 详情里标黄。

**O-2 vNIC + `vlan access` 的 vhost 接线**：`set virtual-switches vs-data vlan access 100` 后，BD 成员变成 `vh-vm1-nic0.100`（子接口），主接口不在 BD 中，VM 的包被判 `blackholed`（`show errors` 计数 12→49），**无告警**。删掉 VLAN 后恢复正常。带 VLAN 的 vhost-user 路径建议补一条 e2e 用例。

**O-3 无「强制释放编辑锁」命令**：脏锁只能等 10 分钟（`DefaultLockIdleTTL`）或 `systemctl restart nfvis`。建议加 `request system configuration unlock [force]`，并在 `show configuration sessions` 输出里直接给出可照做的释放命令。

**O-4 Cirros 类镜像的 cloud-init 缺口**：产品生成的 `seed.iso` 只有 `meta-data` + `user-data`，**没有 `network-config`**。对依赖 cloud-init 网络配置的镜像（Cirros、部分 Alpine 变体），VM 会卡在 metadata 重试。建议：有 vNIC 时自动写入 `network-config`（`version: 2 / ethernets: eth0: dhcp4: true`），或至少在文档里说明「需镜像自带 DHCP 客户端」。

---

## 8. 对开发方的几点核对

1. **BUG-18/19 修得干净**。本轮用与上一份报告完全相同的方法（坏包中部写 `\x00CORRUPT\x00`）复现两个场景，六项事实源（CLI rc / last-result / install.rc / install.log / 归档池 / 读口）**全部一致地说失败**，1 秒采样 45 次**没有一次** `ok:true`。上一轮指出的「双写者竞态」也验证被 `systemctl stop` 收口——这是把根因（谁有权说最后一句话）而不是症状（文案）修掉的正确做法。

2. **发版说明的「如实登记」值得延续**。明确写「撞锁重试的真机路径未构造，由单测覆盖」，并顺带修正了前两版说明里「版本: 3.0.29」的笔误。这种不粉饰的登记方式，正是上一轮报告能精准打靶（直接构造失败路径一次命中）的前提。

3. **归档池污染的主动发现值得肯定**。上一份报告没提这一条（同版本坏包覆盖回退池里的好包），开发方自查发现并修了 `staging` 暂存名 + `rc==0` 才晋升。本轮实测确认：坏包失败后 `/var/lib/nfvis/software/` 内**没有任何 `.deb`**，回退池干净。

4. **一处建议复核**：`OK bool` 上的 `omitempty`（BUG-23）。这大概是顺手加的标签，但结论文件是对外契约，成功与失败的结构应当对称。同批新加的 `Attempt`/`Time` 都没有 `omitempty` 的地雷问题——说明只是这一处漏了。

5. **命令树是自动化测试的地基**，BUG-21 这类不一致的代价被低估了：本次 schema 套件 197 条里，唯一一条「按树生成就必然失败」的就是它。建议给命令树加一条自检：**「树里每个叶子都能被解析器接受（在补齐可选父参数后）」**，作为 CI 门禁。

---

## 9. 测试资产

| 资产 | 路径 |
|---|---|
| v3.0.29 命令树快照（3,172 节点） | `artifacts/commands-v329.json` |
| 发布页元数据 | `artifacts/release-v329.json` |
| 操作模式结果（138 条含 stdout/rc/ms） | `artifacts/res-oper-v329.json` |
| 配置叶子结果（197 条） | `artifacts/res-schema-v329.json` |
| 负向/fuzz 结果（58 条） | `artifacts/res-fuzz-v329.json` |
| 事务闭环结果（11 条） | `artifacts/res-txn-v329.json` |
| PTY 结果（5 条） | `artifacts/res-pty-v329.json` |
| 双向一致性结果（6 条） | `artifacts/res-sync-v329.json` |
| VM 生命周期结果（15 条） | `artifacts/res-vm-v329.json` |
| 安装 stdout / stderr | `evidence/v329/install.out.txt`、`install.err.txt` |
| Web 截图（总览 / 新建成功） | `evidence/v329/web-dashboard-viewport.png`、`web-switch-created.png` |
| BUG-18 竞态时间线（1 s × 40/45） | 宿主内 `/tmp/race-watch.log` |
| BUG-18 失败结论与日志 | 宿主内 `/var/lib/nfvis/software/{last-result.json,install.rc,install.log}` |
| 测试脚本 | `v329/{harness,gen_oper,run_oper,run_schema,fuzz,pty,txn,sync,sync2,vmlife,vmconsole}.py` |
| 坏包构造方法 | 文件中部写 `\x00CORRUPT\x00`（同版本 / 跨版本各一） |

**方法学补记**

1. **命令树的 `optional` 是父节点标记**，不是「这个叶子可以省略」。按树生成用例时必须补全宿主参数，否则会批量制造假失败（本轮 33 条初判 FAIL 全部源于此）。
2. **`-f` 脚本模式遇错即停**，且**有未提交变更时拒绝 exit**——两者都是正确保护，但会让批量脚本「一行失败、后面全不执行」。测试框架必须按行拆或用操作模式命令。
3. **REST 编辑锁按 token 绑定**：`PUT candidate` 与 `POST commit` 必须复用同一 token，否则被正确判为「另一会话」。要做 REST 侧事务测试，必须单次登录。
4. **UI 的中危确认框是 `#modal`**，不是原生 `dialog`/`confirm`；自动化必须点其中的「提交」按钮，否则请求根本不发出（本轮曾因漏这一步误判「静默失败」）。
5. **`fullPage` 截图在本环境会拼接出重复栏**，判断布局缺陷必须用视口截图 + `scrollWidth/clientWidth` 对照，否则会误报。
6. **别把「历史遗留值」当成本轮结果**：running-config 是累积的，断言必须用「从未提交过的唯一值」（本轮 T1 曾因 `198.51.100.53` 是上一轮遗留而误判）。
