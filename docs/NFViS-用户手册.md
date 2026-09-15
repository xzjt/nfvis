# NFViS 用户手册（安装 → 使用全流程）

| 文档属性 | 内容 |
|---|---|
| 适用版本 | V1（规格书 109 条 FR；决策 80 项） |
| 适用对象 | 一体机部署/运维工程师（需 Linux 与网络基础） |
| 配套文档 | 命令速查：[`NFViS-CLI命令全表.md`](NFViS-CLI命令全表.md)（256 条命令含实测状态）<br>需求真源：`NFViS-系统产品需求与目标架构规格书.md`（附录 A = 决策记录）<br>契约：`NFViS-openapi.yaml`（REST）、`NFViS-CLI命令树完整设计.md`（CLI） |
| 证据口径 | 本手册中的命令与输出均取自 **nfvis-vm 真机实测**（`docs/evidence/v1-closeout-round7/8.txt` 等）；未实测处均显式标注 |

> 本手册在编写时逐条真机核验，撞出 4 处问题，**现已全部修复**：
> ① **已修**（决策 #78）：`nfvis-cli` 的 `-server` 缺省值曾与守护进程缺省不匹配（默认参数连不上），
>    现缺省即 `https://127.0.0.1:443`，**零参数可连**（见 §4.3）；
> ② **已修**（决策 #77，安全）：`request system configuration backup to <path>` 导出件曾为 0644
>    且含 `password_hash`，现为 0600；
> ③ **已修**（决策 #80）：`postinst` 的内核基线块原先不可达（安装期并未应用基线），
>    现已前置——安装时即按机器规格写 GRUB 片段与 fstab，**重启后生效**；
> ④ **已修**（决策 #79）：`set system login user … password …` 曾因命令树与模型键名不一致而经 CLI 不可用，
>    现可正常使用（口令加盐哈希落库、回显脱敏）——见 §4.5。注意形如
>    `set system login user password <口令>` **漏写用户名**仍会被拒并提示正确写法（附录 A #82）。

---

## 目录

1. [系统要求](#1-系统要求)
2. [安装](#2-安装)
3. [底座准备（装完能用的前提）](#3-底座准备装完能用的前提)
4. [首次登录](#4-首次登录)
5. [基础概念](#5-基础概念)
6. [配置任务（按场景）](#6-配置任务按场景)
7. [日常运维](#7-日常运维)
8. [故障排查](#8-故障排查)
9. [附录](#9-附录)

---

## 1. 系统要求

### 1.1 硬件

| 项 | 要求 | 说明 |
|---|---|---|
| CPU | x86_64，≥ 4 核 | VPP 需独占核（`main-core` + `corelist-workers`），VNF 需隔离核 |
| 内存 | ≥ 8 GB | 其中 1G 大页按 `min(RAM/4, 8)` 预留（安装器保守默认） |
| 网卡 | 管理口 1 个 + 业务口 ≥ 1 个 | 管理口留内核驱动（SSH/API）；业务口交 DPDK（vfio-pci）给 VPP。<br>**vmxnet3/ixgbe/i40e 等 DPDK 支持的网卡** |
| 磁盘 | ≥ 40 GB | 镜像仓库、VM 磁盘、转储与诊断归档共用 |
| IOMMU | 建议开启 | vfio 直通需 `intel_iommu=on` / `amd_iommu=on`；无 IOMMU 时可用 `enable_unsafe_noiommu_mode` |

### 1.2 软件底座

| 组件 | 版本 | 用途 |
|---|---|---|
| Ubuntu Server | 26.04 | 宿主（本版本只在该平台验证） |
| VPP | 26.06 | 数据面：BD/VRF/BVI/ACL/NAT/SPAN/policer/bond/LLDP 插件 |
| libvirt / QEMU | 12.0 / 10.x | VM 编排（vhost-user、快照、串口 console） |
| Docker | 29.x | 容器 VNF（memif） |
| Go | ≥ 1.26 | 仅**从源码构建**时需要；用 deb 包则不需要 |

> 底座缺失时 nfvisd **降级运行**（对应编排能力不可用并产生告警），不会拒绝启动（FR-OPS-010）。
> 但「降级运行」意味着**装了也用不了对应的功能**，请按 §3 完成底座准备。

---

## 2. 安装

### 2.1 方式 A：deb 包（生产推荐）

deb 包**必须在 Linux 上构建**（依赖 `dpkg-deb`），且交叉编译目标为 linux/amd64：

```bash
# 在构建机（如 nfvis-vm 或任意 Linux + Go ≥1.26）
git clone <repo> && cd nfvis
make deb VERSION=1.0.0            # 产物：build/nfvis_1.0.0_amd64.deb
```

包内布局（Makefile `deb` 目标）：

| 路径 | 内容 |
|---|---|
| `/usr/bin/nfvisd`、`/usr/bin/nfvis-cli` | 守护进程与 CLI |
| `/lib/systemd/system/nfvis.service` | systemd 单元（`Type=notify`、`Restart=always`、`WatchdogSec=30`） |
| `/usr/share/nfvis/installer/nfvis-baseline.sh` | 内核基线落地脚本 |
| `/usr/share/doc/nfvis/` | OpenAPI 契约、CLI 命令树、规格书、验收记录 |

安装：

```bash
sudo dpkg -i build/nfvis_1.0.0_amd64.deb
```

`postinst` 会做（**校验与提示为主，不阻断安装**）：

1. 建数据目录：`/var/lib/nfvis/{images,backup,captures,coredumps,tech-support,vms}`、
   `/data/incoming`、`/run/nfvis`，以及 **0777** 的 `/run/nfvis/{vhost,memif}`（QEMU/容器需访问 VPP socket）；
2. 检查 `core_pattern` 是否指向 `/var/lib/nfvis/coredumps`（不匹配则**仅提示**，见 §7.6）；
3. 检查大页与 `isolcpus`，缺失时**仅提示**；
4. 探测 `vpp` / `libvirtd` / `dockerd`，缺失时**警告**（对应编排能力降级）；
5. `systemctl enable nfvis.service`——**首次安装不自动 start**（避免安装期抢占网卡）。

> **安装期会应用内核基线（决策 #80 起）**：`postinst` 会在无基线时按机器规格写
> `/etc/default/grub.d/99-nfvis.cfg` 与 fstab 大页行并执行 `update-grub`，
> 输出形如 `按机器规格取默认：RAM 5G → 1G 大页 1 页` + `需重启生效`。
> **重启后才生效**；若已有基线则只做一致性检查。
> 需要自定义（更多大页/隔离核）仍按 §3.1 手工执行 `--apply`。

### 2.2 方式 B：从源码构建（开发/验证）

```bash
git clone <repo> && cd nfvis
go build ./... && go vet ./...
make check                        # vet + 测试 + 覆盖率门槛 + 契约守护（提交前必跑）

# 直接运行（开发用；不装 systemd 单元）
go run ./cmd/nfvisd -db /tmp/nfvis.db -listen 127.0.0.1:18443 -allow-plaintext
```

一次性环境初始化（**仅开发虚机**，幂等可重跑）：

```bash
scp contrib/dev-vm/provision.sh root@<vm>:
ssh root@<vm> 'PROXY=http://<proxy>:2333 ./provision.sh'   # PROXY 按需，直连则省略
```

`provision.sh` 安装：基础工具链、Go 1.26、VPP 26.06、libvirt/QEMU、Docker、
1G×N 大页（经 `-print-kernel-baseline` 生成，与 CLI 同源）、`/opt/nfvis/{src,images,incoming,backup}`。
日志：`/var/log/nfvis-provision.log`。**大页需 reboot 生效**；VPP 装后设为不自启。

### 2.3 安装后检查清单

```bash
dpkg -l nfvis                                  # 已安装
systemctl is-enabled nfvis.service             # enabled
ls /usr/bin/nfvisd /usr/bin/nfvis-cli          # 二进制就位
ls -d /var/lib/nfvis/{images,backup,captures,coredumps,tech-support,vms} /data/incoming
```

### 2.4 编译期与运行期自检（可选，构建机）

```bash
make check        # 全量测试 + vet + 覆盖率 + 依赖方向 + 决策条数 + openapi 同步 + 原型
```

---

## 3. 底座准备（装完能用的前提）

> 这一步做完之前，nfvisd 能启动、能登录、能配（配置文件会落库），
> 但**数据面（VPP）与计算面（VM/容器）不可用**。

### 3.1 内核基线：大页 + 隔离核（**需重启生效**）

两种方式，二选一：

**方式 A：安装器脚本**（deb 包自带）

```bash
/usr/share/nfvis/installer/nfvis-baseline.sh --check       # 只看现状
sudo /usr/share/nfvis/installer/nfvis-baseline.sh --defaults   # 保守默认：1G 页 = min(RAM_GB/4, 8)，不设 isolcpus
# 或按机器规格显式指定：
sudo /usr/share/nfvis/installer/nfvis-baseline.sh --apply \
     --hugepages-1g 8 --isolated-cores 4-15 --thp never --iommu pt
sudo reboot
```

**方式 B：经 CLI**（配置态 → 写 GRUB → 重启）

```bash
nfvis-cli -server https://127.0.0.1:443 -u admin -c "configure
set resource-pools hugepages page-size 1G count 8
set resource-pools cpu isolated-cores 4-15
top
commit"
# commit 会提示内核基线与配置不一致，然后：
nfvis-cli -server https://127.0.0.1:443 -u admin -c "request system kernel apply"
nfvis-cli -server https://127.0.0.1:443 -u admin -c "request system reboot"
```

要点：

- 脚本幂等：内容不变则不重写、不跑 `update-grub`；写入前备份到 `/var/lib/nfvis/kernel-baseline.bak`，
  `update-grub` 失败会自动回退片段（不留未生效配置）。
- GRUB 片段：`/etc/default/grub.d/99-nfvis.cfg`；fstab 大页挂载行带 `# nfvis-hugepages` 标记。
- 生效后核对：`grep -E 'hugepages|isolcpus' /proc/cmdline`；`grep HugePages_Total /proc/meminfo`。
- CLI 自检：`show system kernel` 给出**三方对照**（cmdline / 运行实际 / 配置期望）。

> **大页数量规划**：VNF 内存从大页池分配，1 台 1G 内存的 VM 就占 1 个 1G 大页。
> 池子不足时 commit 会明确报错（`无 1G 大页资源池，无法分配 …MB`）。

### 3.2 业务网卡交 DPDK（vfio-pci）

管理口**保持内核驱动**（SSH/API 走它）；业务口需从内核驱动解绑、绑到 `vfio-pci` 供 VPP 使用。

**方式 A：经 CLI（推荐，产品内建）**

```bash
nfvis-cli -server https://127.0.0.1:443 -u admin -c "request interfaces ens224 bind-dpdk --yes"
# 解绑（接管后内核里已无该网卡，须用 PCI 地址；实测须显式给 to-driver）
nfvis-cli -server https://127.0.0.1:443 -u admin -c \
  "request interfaces 0000:13:00.0 unbind-dpdk to-driver vmxnet3 --yes"
```

> **先弄清有哪些口**（决策 #83）：`set interfaces <ifname>` 的 Tab 候选 = **VPP 中的接口**，
> 也就是**已被 DPDK 接管**的那批，与 `show interfaces physical` 的空态同源。
> 已被接管的口在**内核里已无网卡**（`ls /sys/class/net` / `ip link` 都看不到），
> 所以只能这样发现；反过来，**尚未接管的内核网卡**出现在
> `request interfaces <n> bind-dpdk` 的候选里（两侧候选来源不同，不是同一个清单）。

**方式 B：带外手工（虚机重启后网卡会回到原生驱动，CLI 不可用时用这个）**
```bash
modprobe vfio-pci
echo Y > /sys/module/vfio/parameters/enable_unsafe_noiommu_mode   # 无 IOMMU 时才需要
for d in 0000:0b:00.0 0000:13:00.0; do
  echo $d > /sys/bus/pci/drivers/vmxnet3/unbind 2>/dev/null || true
  echo vfio-pci > /sys/bus/pci/devices/$d/driver_override
  echo $d > /sys/bus/pci/drivers/vfio-pci/bind
done
```

实测平台行为（决策 #72）：

- 绑定后**内核网卡消失**，只能按 PCI 地址定位，结果按 PCI 回读驱动；
- 清空 `driver_override` + `rescan` **不足以**让内核重新探测原生驱动 → 解绑**必须**给 `to-driver`。

### 3.3 启动 VPP 并确认

```bash
systemctl start vpp && sleep 5
vppctl show version          # v26.06-release
vppctl show interface        # 应能看到业务口（状态 down 属正常，尚未配 L2/L3）
```

VPP 的 `startup.conf` 由 nfvisd 按 **committed 配置**生成（`/etc/vpp/startup.conf`）：
`cpu main-core/corelist-workers`、`memory`、`buffers`、`dpdk dev <pci> { name <ifname> }`。
因此：**改 `set vpp …` 后需 `request vpp restart`（或整机重启）才生效**。

> ⚠️ 陷阱（实测踩过）：`request vpp restart` 会用当前 committed 配置**重生成** `startup.conf`。
> 若配置里没有 `vpp dpdk dev <ifname>` 条目，重启后 VPP 里**就没有那些网卡**了
> （表现为 `接口在 VPP 中不存在: ens192（是否未由 DPDK 接管？）`）。
> 建议先备份 `/etc/vpp/startup.conf`。

---

## 4. 首次登录

### 4.1 启动守护进程

```bash
sudo systemctl start nfvis
systemctl status nfvis          # active (running)
journalctl -u nfvis -f          # 观察启动日志
```

启动参数（systemd 单元已设，可经 `/etc/systemd/system/nfvis.service.d/` 覆盖）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-db` | `/var/lib/nfvis/nfvis.db`（单元内） | SQLite 配置库 |
| `-listen` | `:443` | API 监听；通配时会**按管理口地址收敛**（FR-SEC-001） |
| `-tls-cert` / `-tls-key` | 空 | 缺省**自动生成自签证书**并启用 HTTPS（决策 #72） |
| `-allow-plaintext` | 关 | **强制明文**，仅开发/测试；给出即忽略已装/自签证书 |
| `-init-admin-password` | 空 | 首次启动的 admin 口令；缺省随机生成 |
| `-vpp-sock` | `/run/vpp/api.sock` | VPP binary API |

### 4.2 取一次性 admin 口令

首次启动（库中无本地用户）会创建 `admin`（super-user），**随机口令只打印一次**：

```bash
journalctl -u nfvis --since "10 min ago" | grep 一次性口令
# % 首次启动已创建用户 admin (super-user)。一次性口令（仅显示一次，请立即修改）: XiJrwGuFApmADuaK@Aa1
```

> 口令字符集**不含** `!` `$` 反引号 等 shell 敏感字符（决策 #80），可直接复制粘贴；
> 直接 `nfvis-cli -p <口令>` 或用**单引号**包裹均可用。

若口令已丢失：停服务、删库 `rm /var/lib/nfvis/nfvis.db` 会**清空全部配置**——生产环境不要这样做；
正确做法是用 `-init-admin-password` 重新引导或经其他 super-user 重置（见 §6.1.6）。

### 4.3 用 CLI 连接

```bash
nfvis-cli                       # 未给 -p 时在终端下**无回显**索取口令
# Password: ← 粘贴上面的一次性口令
nfvis>
```

口令来源优先级：`-p` → 环境变量 `NFVIS_PASSWORD` → **终端交互提示**。
非终端（脚本/管道）下未提供口令会**立即报错并给出指引**（不会挂起）。

CLI 参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-server` | `https://127.0.0.1:443` | **与守护进程缺省一致**（`NFVIS_LISTEN=:443` + 自动自签 HTTPS）；<br>可用 `NFVIS_SERVER` 环境变量覆盖 |
| `-u` / `-p` | `admin` / `$NFVIS_PASSWORD` | 用户名/口令 |
| `-ca` | 空 | 服务端证书 PEM；**缺省固定本机 `/var/lib/nfvis/tls/server.crt`**（自签场景零配置） |
| `-insecure` | 关 | 跳过证书校验（仅调试） |
| `-source` | `ssh` | 接入源（`ssh`/`console`），影响 FR-CFG-012 自锁保护与 `start shell` 权限 |
| `-c "…"` | 空 | **脚本模式**：执行多行命令后退出（换行分隔；任一行出错即停） |

> **零参数即可连**（决策 #78）：缺省 `https://127.0.0.1:443` 与守护进程缺省一致，
> 且因地址为 `https://`，客户端会**自动固定本机自签证书** `/var/lib/nfvis/tls/server.crt`
> ——即「装完即用」无需任何参数。若 nfvisd 监听在别处（如开发用的
> `-listen 127.0.0.1:18443 -allow-plaintext`），用 `-server http://127.0.0.1:18443`
> 或 `export NFVIS_SERVER=http://127.0.0.1:18443` 覆盖。

**脚本模式**（适合自动化；注意它在配置模式下会自动收尾，**不要手写 `discard`**，否则重复释放会报
`%% 当前会话未持有 candidate`）：

```bash
nfvis-cli -server https://127.0.0.1:443 -u admin -c "configure
set system hostname fw-01
commit"
```

**交互模式**：`?` 列候选（**按键即时，不用回车**）、Tab 补全（唯一匹配自动补全、多匹配响铃并列出候选）、
无歧义缩写、`Ctrl-]` 退出串口、`Ctrl-C` 退出 monitor。

> `?` 是帮助键而非字面量：它不会进入命令行文本。因此取值里**不能直接输入 `?`**
> （与 Cisco IOS 同构，附录 A #81）。

### 4.4 用 REST API 访问

```bash
# 取 token
TOKEN=$(curl -sk https://127.0.0.1/api/v1/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<口令>"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')

curl -sk https://127.0.0.1/api/v1/interfaces -H "Authorization: Bearer $TOKEN"
curl -sk --cacert /var/lib/nfvis/tls/server.crt https://127.0.0.1/api/v1/system/version   # 校验证书
```

- 契约：`/usr/share/doc/nfvis/NFViS-openapi.yaml`；运行时端点 `GET /api/v1/openapi.json`。
- `curl` 用自签证书需 `-k` 或 `--cacert /var/lib/nfvis/tls/server.crt`。

### 4.5 建用户与权限 class

> **CLI 亦可**（决策 #79 已修复）：`set system login user <n> password <s> class <c>` 会**加盐哈希**后落库
>（先过口令策略），且语句回显脱敏为 `«已隐藏»`。下表 REST 端点等价，二者同源。

```bash
TOKEN=$(curl -sk https://127.0.0.1/api/v1/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<admin口令>"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')

# 建用户（201；class 取 super-user|operator|read-only）
curl -sk -X POST https://127.0.0.1/api/v1/system/login-users \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"ops","password":"<强口令>","class":"operator"}'

# 改口令 / 改 class / 删用户
curl -sk -X PUT    https://127.0.0.1/api/v1/system/login-users/ops -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"ops","password":"<新口令>","class":"operator"}'
curl -sk -X DELETE https://127.0.0.1/api/v1/system/login-users/ops -H "Authorization: Bearer $TOKEN"
```

口令策略与自定义 class **可以**经 CLI 配置：

```bash
nfvis-cli -server https://127.0.0.1:443 -u admin -c "configure
set system login password-policy min-length 12
set system login password-policy complexity true
set system login password-policy expire-days 90
top
commit"
```

核对：`nfvis-cli … -c "show users"`。

预置 class 权限矩阵（契约 §4）：

| 命令域 | super-user | operator | read-only |
|---|---|---|---|
| `show *` | ✔ | ✔ | ✔ |
| `configure`（配置模式全部） | ✔ | ✘ | ✘ |
| `request`（生命周期/镜像/接口） | ✔ | ✔ | ✘ |
| `request`（software/reboot/configuration/zeroize） | ✔ | ✘ | ✘ |
| `clear` / `start shell` | ✔ | ✘ | ✘ |

---

## 5. 基础概念

### 5.1 事务模型（最重要的一条）

所有配置都经**事务**，不存在「敲下即生效」：

```
set/delete …   →  candidate（候选配置，仅本会话可见）
commit         →  校验 → 下发底座（失败自动补偿回滚）→ committed
rollback [n]   →  取历史快照为 candidate（还需再 commit）
commit confirmed [分钟] → 超时未确认则自动回滚（改管理口等高风险操作的自锁保护）
```

常用配套命令（配置模式内）：

| 命令 | 用途 |
|---|---|
| `show` | 看当前层级的 candidate |
| `show \| display set` | **未支持**（附录 A #84；请用 `save <file>` 导出 JSON 或 `show configuration` 看块状） |
| `commit check` | 只校验不下发 |
| `compare rollback <n>`（操作模式：`show configuration compare rollback <n>`） | 与历史比对 |
| `annotate <path> "注释"` | 给节点加注释（**路径相对当前层级**） |
| `save <file>` / `load override\|merge <file>` | candidate 导出/导入 JSON（`save` 落盘 0600） |
| `discard` | 丢弃 candidate 并释放会话锁 |
| `run <操作命令>` | 配置模式内执行 show/request |

**commit 失败不会留下半成品**：底座下发失败会补偿回滚，并在输出里说明原因（如
`下发失败 vrf[vs-l3]: 接口在 VPP 中不存在: ens192`）。

### 5.2 配置层次总览

```
system          主机名/时区/NTP/DNS/API/管理口/内核基线/健康阈值/syslog/登录与口令策略
protocols lldp  LLDP
interfaces      物理口（描述/MTU/启停/SR-IOV/限速绑定）
bonds           链路聚合
virtual-switches 虚拟交换机（l2 = bridge-domain，l3 = VRF）
acls / nat / port-mirroring / qos   高级网络功能
resource-pools  大页池与隔离核（唯一真源，VPP 保留核从中扣减）
vpp             数据面运行时配置（生成 startup.conf）
virtual-machine-functions   VM VNF
container-functions         容器 VNF
```

### 5.3 配置态 vs 运行态

- **配置态**：模型里的声明（`show configuration`）。
- **运行态**：底座实际状态（`show interfaces`、`show vrfs <n> routes`、`show vpp capture` 等）。
- 两者可能短暂不一致（底座未收敛）；nfvisd 会在启动与 VPP 重连时**恢复收敛**，
  未收敛项进告警（`show alarms`）。

---

## 6. 配置任务（按场景）

> 以下示例均经真机验证。统一用 `nfvis-cli -server https://127.0.0.1:443 -u admin`，为简洁记为 `nfvis$`。

### 6.1 系统基线

```bash
nfvis$ configure
nfvis# set system hostname fw-01
nfvis# set system timezone Asia/Shanghai
nfvis# set system ntp server 192.168.1.1 prefer
nfvis# set system dns server 8.8.8.8 secondary 1.1.1.1
nfvis# set system management interface ens160          # 管理口（不得用于数据面）
nfvis# set system syslog host 192.168.1.10 port 514 facility local0 severity info
nfvis# set system syslog local level info
nfvis# set system syslog local retention-days 14
nfvis# set system health thresholds cpu-temp-celsius 90
nfvis# set system health thresholds disk-used-percent 85
nfvis# set system idle-timeout-minutes 10
nfvis# top
nfvis# commit
```

> ⚠️ **改管理口地址/网关**：若当前会话来自 SSH，commit 会要求使用 `commit confirmed` 并输出自锁警告
> （FR-CFG-012）——这是防止把自己关在门外的保护。

### 6.2 资源池与 VPP

```bash
nfvis# edit resource-pools
nfvis# set hugepages page-size 1G count 8
nfvis# set cpu isolated-cores 4-15
nfvis# top
nfvis# edit vpp
nfvis# set cpu main-core 4
nfvis# set cpu corelist-workers 5-7
nfvis# set memory main-heap-size 2G
nfvis# set memory hugepage-preference 1G        # 须与资源池页大小一致（commit 校验）
nfvis# set dpdk dev rx-queues 2                 # 全局默认（作用于未单独配置的物理口）
nfvis# set dpdk dev ens224 rx-queues 4          # 单网卡覆盖
nfvis# top
nfvis# commit
# 输出警告：vpp 变更需 request vpp restart（或整机 reboot）后生效
nfvis# exit
nfvis$ request vpp restart
```

核对：

```bash
nfvis$ show resource-pools      # 大页/隔离核 的 总量/已分配/空闲，含 vpp-reserved
nfvis$ show vpp                 # 版本/线程/buffer/内存
nfvis$ show system kernel       # 三方对照（cmdline / 运行实际 / 配置期望）
```

### 6.3 L2 虚拟交换机 + BVI 网关

```bash
nfvis$ configure
nfvis# edit virtual-switches vs-dmz
nfvis# set type l2
nfvis# set vlan access 100
nfvis# set ports 1 interface ens224
nfvis# set gateway ip 192.168.100.1/24
nfvis# top
nfvis# commit
```

L2 交换机在 VPP 侧落地为 **bridge-domain**；`gateway ip` 创建 BVI（三层网关）。
核对：

```bash
nfvis$ show virtual-switches vs-dmz detail
nfvis$ show virtual-switches vs-dmz mac-table
nfvis$ show virtual-switches vs-dmz statistics
```

### 6.4 L3 虚拟交换机 + 静态路由

```bash
nfvis# edit virtual-switches vs-wan
nfvis# set type l3
nfvis# set l3-interface ens192 ip address 192.168.1.2/24
nfvis# set static-routes 10.0.0.0/8 next-hop 192.168.1.1
nfvis# set static-routes default next-hop 192.168.1.1
nfvis# top
nfvis# commit
```

核对：`show vrfs`、`show vrfs vs-wan`、`show vrfs vs-wan routes`（FIB 运行态）。

### 6.5 ACL / QoS / SPAN / NAT

```bash
# ACL（五元组 + 动作）
nfvis# edit acls acl-web
nfvis# set rule 10 source any destination any protocol tcp destination-port 443 action permit
nfvis# set rule 20 source any destination any protocol any action deny
nfvis# top
nfvis# edit virtual-switches vs-wan
nfvis# set l3-interface ens192 acl-in acl-web        # 绑定到 L3 接口入向
nfvis# top

# QoS 限速（VPP policer）
nfvis# edit qos
nfvis# set policies pol-1g cir 1000000000 cbs 1000000
nfvis# top
nfvis# edit interfaces
nfvis# set ens192 ingress-policy pol-1g
nfvis# top

# SPAN 端口镜像
nfvis# edit port-mirroring span-1
nfvis# set source interface ens224 direction both
nfvis# set analyzer interface ens192
nfvis# top

# NAT44（inside 由 L3 交换机派生，outside 由出接口所属 VRF 派生；出接口必填）
nfvis# edit nat
nfvis# set source-pool pool-a address-range 203.0.113.1 to 203.0.113.10
nfvis# set rules 10 match source 192.168.100.0/24 virtual-switch vs-wan action interface ens192
nfvis# top
nfvis# commit
```

> NAT 约束（决策 #38/#52）：VPP NAT44 单实例仅一对 (inside, outside)，
> 故多规则的 virtual-switch 与出接口 VRF 必须各自一致。

核对：`show acls acl-web detail`、`show qos policies`、`show port-mirroring`、`show nat`。

### 6.6 链路聚合与 LLDP

```bash
nfvis# edit bonds bond0
nfvis# set members 0 ens224
nfvis# set lacp mode active interval fast
nfvis# set mtu 9000
nfvis# top
nfvis# edit protocols lldp
nfvis# set enable true
nfvis# set advertisement-interval 30
nfvis# top
nfvis# commit
```

bond 名可在一切接受接口名处引用（与物理口等价，FR-NET-017）。
核对：`show bonds`、`show bonds bond0 detail`、`show lldp neighbors`。

### 6.7 VM VNF

**① 导入镜像**（文件须先放入 `/data/incoming/`，成功后自动清理源文件）

```bash
scp ubuntu-cloud.qcow2 root@<nfvis>:/data/incoming/
nfvis$ request images upload name ubuntu-cloud.qcow2 type vm-image file /data/incoming/ubuntu-cloud.qcow2
nfvis$ show images                          # 状态 ready 才可引用
```

也可从 URL 拉取（**sha256 必填**，FR-SEC-004）：

```bash
nfvis$ request images download name img.qcow2 type vm-image \
        url https://example.com/img.qcow2 sha256 <64位十六进制>
nfvis$ show images img.qcow2 detail          # import_state 看进度
```

**② 定义 VM 并提交**（vCPU 取自隔离核池、内存取自大页池——池子不够 commit 会明确报错）

```bash
nfvis$ configure
nfvis# edit virtual-machine-functions fw-vm
nfvis# set image ubuntu-cloud.qcow2
nfvis# set vcpu count 2
nfvis# set memory size-mb 2048
nfvis# set memory hugepage-size 1G
nfvis# set interfaces eth0 type vhost-user
nfvis# set interfaces eth0 virtual-switch vs-dmz
nfvis# set cloud-init hostname fw-vm
nfvis# set cloud-init user-data "#cloud-config
users:
  - name: admin
    ssh_authorized_keys:
      - ssh-ed25519 AAAA... you@host"
nfvis# set disks data0 size-gb 20                 # 附加 virtio 数据盘（可选）
nfvis# set autostart true
nfvis# top
nfvis# commit
```

**③ 生命周期与串口**

```bash
nfvis$ request virtual-machine-functions fw-vm start
nfvis$ show virtual-machine-functions fw-vm            # state running
nfvis$ show virtual-machine-functions fw-vm interfaces
nfvis$ request virtual-machine-functions fw-vm console  # 串口；Ctrl-] 退出（需真实 TTY）
nfvis$ request virtual-machine-functions fw-vm stop
nfvis$ request virtual-machine-functions fw-vm restart
```

**④ 快照**（FR-CMP-015；**create/rollback 需关机态**——决策 #75）

```bash
nfvis$ request virtual-machine-functions fw-vm stop
nfvis$ request virtual-machine-functions fw-vm snapshot create name snap-before-upgrade
nfvis$ show virtual-machine-functions fw-vm snapshots
nfvis$ request virtual-machine-functions fw-vm snapshot rollback name snap-before-upgrade
nfvis$ request virtual-machine-functions fw-vm snapshot delete name snap-before-upgrade
```

> 为什么必须关机：实测对**运行中**域回滚，libvirt 会**替换 QEMU 进程**（相当于静默重启该 VM）。
> 产品因此在 API/CLI 层显式拒绝并提示先关机（运行中返回 `409`）。

### 6.8 容器 VNF

```bash
# ① 上传容器镜像（docker save 出的 tar）
docker save alpine:3.20 -o /tmp/alpine320.tar
scp /tmp/alpine320.tar root@<nfvis>:/data/incoming/
nfvis$ request images upload name alpine:3.20 type container-image file /data/incoming/alpine320.tar
```

> ⚠️ **重要限制**：`name` **必须与 tar 内的 Docker tag 一致**（如 `alpine:3.20`）。
> 若写成文件名（如 `alpine320.tar`），配置校验会通过，但下发时 Docker 报
> `docker: not found`（实际是镜像找不到）；反之用 Docker tag 而目录里没有同名条目也会被校验拒。
> 详见 `NFViS-CLI命令全表.md` §4①（待产品决策）。

```bash
nfvis$ configure
nfvis# edit container-functions ct-1
nfvis# set image alpine:3.20
nfvis# set vcpu count 1
nfvis# set memory size-mb 128
nfvis# set command /bin/sh
nfvis# set args -c "sleep 3600"
nfvis# set restart-policy on-failure
nfvis# top
nfvis# commit
nfvis$ request container-functions ct-1 start
nfvis$ request container-functions ct-1 log last 50
```

---

## 7. 日常运维

### 7.1 查看状态

```bash
nfvis$ show version                    # nfvis/vpp/dpdk/libvirt/qemu/docker 版本汇总
nfvis$ show system uptime | cpu | memory | storage | hugepages | hardware
nfvis$ show interfaces physical        # 物理口（驱动/链路/速率/VF）
nfvis$ show interfaces ens224 statistics
nfvis$ show virtual-switches
nfvis$ show vrfs vs-wan routes
nfvis$ show virtual-machine-functions
nfvis$ show container-functions
nfvis$ show alarms active              # 或 all
nfvis$ show log system last 50
nfvis$ show log audit last 20
nfvis$ show configuration
```

**通用管道**（所有 `show` 输出可用）：

```bash
nfvis$ show interfaces | match ens
nfvis$ show interfaces | except down
nfvis$ show interfaces | count
nfvis$ show log system | last 20
nfvis$ show configuration | display json
```

### 7.2 连通性测试与监控

```bash
nfvis$ ping 192.168.1.1 count 4              # 经 VPP L3
nfvis$ ping 192.168.1.1 source 192.168.1.2   # source 须为 **VPP 接口**地址
nfvis$ ping 10.0.0.1 vrf vs-wan
nfvis$ traceroute 192.168.1.1                # 宿主侧 ICMP（不支持 vrf，会明确报错）
nfvis$ monitor interfaces ens224 interval 2  # Ctrl-C 退出
nfvis$ monitor vnf fw-vm
nfvis$ clear interfaces statistics ens224
```

### 7.3 抓包（VPP pcap trace）

```bash
nfvis$ request vpp trace start interface ens224 count 100     # 达到报文数自动停止
nfvis$ show vpp capture                                        # 会话状态 + 已导出 pcap 清单
nfvis$ request vpp trace export name my-capture                # 导出（隐含停止）
# 下载：GET /api/v1/vpp/capture/<file>
nfvis$ request vpp trace stop                                  # 停止且不导出
```

### 7.4 备份 / 恢复 / 恢复出厂

```bash
# 生成归档（自动命名，落 /var/lib/nfvis/backup/，0600）
nfvis$ request system configuration backup
# 生成并额外导出到指定路径（同样 0600；导出件含口令哈希，决策 #77 已修正权限）
nfvis$ request system configuration backup to /var/lib/nfvis/backup/pre-change.json

nfvis$ request system configuration restore /var/lib/nfvis/backup/pre-change.json
# 恢复出厂（**双重确认**，不可逆）：
nfvis$ request system zeroize
```

> **归档含账号信息（`password_hash`）**，自动命名与 `to <path>` 导出件**均为 0600**，仅 super-user 可读
> （决策 #56 的既有例外口径；决策 #77 修正了 `to <path>` 原先落 0644 的缺陷）。
> 请勿把归档放到 /tmp 等共享目录，或复制给他人。

### 7.5 诊断与转储

```bash
nfvis$ request system tech-support generate   # 生成 tar.gz（日志+版本+配置+状态）
nfvis$ show system tech-support               # 清单；下载 GET /api/v1/system/tech-support/<file>
nfvis$ show system core-dumps                 # 崩溃转储清单（VPP/QEMU/nfvisd）
nfvis$ request system core-dumps export https://<server>/upload
nfvis$ request system core-dumps delete file <name>
```

core dump 目录：`/var/lib/nfvis/coredumps`（容量上限 + 滚动清理）。
若清单为空，按 §2.1 的 postinst 提示设置 `core_pattern`：

```bash
echo '/var/lib/nfvis/coredumps/core.%e.%p.%t' > /proc/sys/kernel/core_pattern
```

### 7.6 监控接入

| 端点 | 用途 |
|---|---|
| `GET /api/v1/metrics` | Prometheus 指标 |
| `GET /api/v1/events` | SSE 事件流（配置提交、VM 状态变化、告警等） |
| `GET /api/v1/alarms` | 告警列表 |
| `GET /api/v1/audit-logs` | 审计日志（含 `limit`/`offset` 分页） |

### 7.7 软件升级与重启

```bash
nfvis$ request system software add /data/incoming/nfvis_1.0.1_amd64.deb sha256 <hex>
nfvis$ request system software rollback [to 1.0.0]
nfvis$ request system reboot
nfvis$ request system shutdown
nfvis$ request system ntp sync
nfvis$ request system api tls regenerate         # 重签自签证书
nfvis$ request system ssh host-key regenerate
```

安装 deb 升级时，`postinst` 会自动 `try-restart` 已在运行的 nfvisd 以加载新版本（FR-OPS-001）。

---

## 8. 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| CLI 报 `连接 nfvisd 失败` / 超时 | ① `-server` 默认值与守护进程不符（§4.3）；② nfvisd 未启动 | 显式 `-server https://127.0.0.1:443`；`systemctl status nfvis` |
| VM `stop` 报「请求超时」但 VM 实际已停 | 老版本客户端超时 ≤ 服务端 ACPI 等待（已修，决策 #76③） | 升级到含修复的版本；用 `show … <name>` 确认实际状态 |
| commit 报 `接口在 VPP 中不存在: ensX（是否未由 DPDK 接管？）` | 网卡未绑 vfio-pci，或 `request vpp restart` 后 startup.conf 丢了 dpdk 条目 | 按 §3.2 绑定；检查 `/etc/vpp/startup.conf` 的 `dpdk {}` 段 |
| commit 报 `无 1G 大页资源池，无法分配 …MB` | 资源池未配或内核大页未生效 | §3.1 配 `resource-pools hugepages` 并重启生效 |
| commit 报 `隔离核不足：需要 N，可用 0` | VPP 保留核已占满隔离核池 | 扩大 `isolated-cores`（或用 `show resource-pools` 看 `vpp-reserved`） |
| 容器下发报 `docker: not found` | 镜像名与 Docker tag 不一致（§6.8） | 上传时的 `name` 用 Docker tag |
| `show vpp runtime` 报「未接入」 | 该功能 V1 未实现（govpp runtime 解码受限） | 已知限制，非故障 |
| `show lldp neighbors` 为空 | 无 LLDP 对端 | 正常；对端启用 LLDP 后可见 |
| `request sriov …` 报「不支持 SR-IOV」 | 网卡无 SR-IOV 能力或未暴露 VF | 换支持 SR-IOV 的网卡；见 FR-NET-004 |
| VPP 重启后网络不通 | startup.conf 由 committed 配置生成，配置不含端口/网卡 | 检查 `show configuration`，必要时 `request vpp restart` 重放 |
| 配了 cross-connect 后网络风暴/整机失联 | **直通两端同处一个广播域** → 物理二层环路（cross-connect 无 MAC 学习、无环路保护） | 直通两端必须属于**不同**广播域；先断开其一再改配置 |
| 升级后配置丢失 | 不应发生（配置在 `/var/lib/nfvis/nfvis.db`） | 检查 DB 是否存在；`purge` 卸载才清运行态，数据保留 |

**取日志**：

```bash
journalctl -u nfvis -n 200 --no-pager
tail -n 100 /var/log/nfvis-provision.log     # 环境初始化（若用过 provision.sh）
nfvis-cli … -c "show log system last 100"
```

---

## 9. 附录

### 9.1 文件与目录

| 路径 | 内容 |
|---|---|
| `/usr/bin/nfvisd`、`/usr/bin/nfvis-cli` | 可执行文件 |
| `/etc/systemd/system/nfvis.service` | systemd 单元（含环境变量 `NFVIS_DB`/`NFVIS_LISTEN`/`NFVIS_VPP_SOCK`） |
| `/var/lib/nfvis/nfvis.db` | 配置库（SQLite，含 committed 与历史快照） |
| `/var/lib/nfvis/images/` | 镜像仓库（qcow2 / 容器 tar），`index.json` 为目录元数据 |
| `/var/lib/nfvis/backup/` | 配置备份归档（0600） |
| `/var/lib/nfvis/captures/` | 导出的 pcap |
| `/var/lib/nfvis/coredumps/` | 崩溃转储 |
| `/var/lib/nfvis/tech-support/` | 诊断归档 tar.gz |
| `/var/lib/nfvis/vms/` | VM 磁盘与定义 |
| `/var/lib/nfvis/tls/` | `server.crt`/`server.key`（自签；CLI 默认固定此证书） |
| `/var/lib/nfvis/kernel-baseline.bak` | 内核基线备份 |
| `/etc/default/grub.d/99-nfvis.cfg` | 内核基线 GRUB 片段 |
| `/etc/vpp/startup.conf` | VPP 启动配置（**由 nfvisd 生成，勿手改**） |
| `/data/incoming/` | 镜像导入暂存（须放此处才能 `request images upload`） |
| `/run/nfvis/vhost`、`/run/nfvis/memif` | VPP 与 QEMU/容器共享的 socket（0777） |

> 卸载（`apt purge nfvis`）只清 `/run/nfvis`；`/var/lib/nfvis` **数据保留**（避免误删）。
> 需要清空请显式执行 `request system zeroize`。

### 9.2 端口与服务

| 端口/套接字 | 归属 | 说明 |
|---|---|---|
| `TCP 443` | nfvisd | REST API + WebSocket console（默认 HTTPS，可按管理口地址收敛监听） |
| `/run/vpp/api.sock` | VPP | binary API |
| `/run/vpp/cli.sock` | VPP | `vppctl`（ping/trace 经此执行） |
| `/var/run/docker.sock` | Docker | 容器编排 |
| `libvirt` (qemu:///system) | libvirtd | VM 编排 |

### 9.3 命令速查

完整命令表（**256 条**，含逐条实测状态与 API 落点）见 **[`NFViS-CLI命令全表.md`](NFViS-CLI命令全表.md)**。

### 9.4 已知限制（使用前请阅读）

完整清单见 `NFViS-CLI命令全表.md` §4 与 `V1-验收检查表.md`。高频几条：

| 限制 | 说明 |
|---|---|
| 容器镜像目录名须等于 Docker tag | §6.8；唯一可用组合是上传时 `name` 写成 tag |
| 快照 create/rollback 需关机态 | §6.7④（运行中回滚会静默重启 VM） |
| `show vpp runtime` 未接入 | govpp runtime 解码受限，CLI 明确提示 |
| 硬件健康在无 BMC/传感器环境为降级路径 | `show system hardware` 仍可用，值可能为空 |
| SR-IOV / LLDP 邻居 需对应硬件与对端 | 无 PF/VF、无 LLDP 对端时无法演示 |

### 9.5 参考文档

| 文档 | 用途 |
|---|---|
| `NFViS-系统产品需求与目标架构规格书.md` | 需求真源；**附录 A = 决策记录（1~80），实现有疑问先查它** |
| `NFViS-CLI命令全表.md` | 命令速查 + 实测状态 |
| `NFViS-CLI命令树完整设计.md` | CLI 契约 |
| `NFViS-openapi.yaml` | REST 契约 |
| `NFViS-Go工程目录骨架设计.md` | 代码结构、依赖方向、里程碑 |
| `V1-验收检查表.md`、`V1-收尾待办.md` | 验收口径与剩余待办 |
| `docs/evidence/` | 各轮真机证据原始输出 |
