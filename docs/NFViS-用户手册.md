# NFViS 用户手册（安装 → 使用全流程）

| 文档属性 | 内容 |
|---|---|
| 适用版本 | V1.1.19（随包安装于 `/usr/share/doc/nfvis/`） |
| 适用对象 | 一体机部署/运维工程师（需 Linux 与网络基础） |
| 配套文档 | 命令速查：[`NFViS-CLI命令全表.md`](NFViS-CLI命令全表.md)（逐条命令含实测状态）<br>契约：[`NFViS-openapi.yaml`](NFViS-openapi.yaml)（REST）、[`NFViS-CLI命令树完整设计.md`](NFViS-CLI命令树完整设计.md)（CLI）<br>需求真源：`NFViS-系统产品需求与目标架构规格书.md` |
| 证据口径 | 本手册中的命令与输出均取自 **nfvis-vm 真机实测**（`docs/evidence/` 各轮原始输出）；未实测处均显式标注 |

> **快速上手（5 分钟版）**：装 deb → 重启 → `journalctl -u nfvis | grep 一次性口令` 取 admin 口令
> → `nfvis-cli` 登录 → `wizard` 回答几问完成资源池与内核基线 → 重启 → 按 §8 声明业务口并
> `request vpp restart` → 按 §9 建 VM/容器。每一步的细节与排错都在后文对应章节。

---

## 目录

1. [系统要求与部署形态](#1-系统要求与部署形态)
2. [安装与卸载](#2-安装与卸载)
3. [首次启动与登录](#3-首次启动与登录)
4. [CLI 基础](#4-cli-基础)
5. [初始化：wizard 向导](#5-初始化wizard-向导)
6. [内核基线：大页与隔离核](#6-内核基线大页与隔离核)
7. [数据面：网卡、DPDK 与 VPP](#7-数据面网卡dpdk-与-vpp)
8. [网络配置任务](#8-网络配置任务)
9. [计算负载：VM 与容器](#9-计算负载vm-与容器)
10. [日常运维](#10-日常运维)
11. [故障排查](#11-故障排查)
12. [附录](#12-附录)

---

## 1. 系统要求与部署形态

### 1.1 硬件

| 项 | 要求 | 说明 |
|---|---|---|
| CPU | x86_64，≥ 4 核 | VPP 需独占核（`main-core` + `corelist-workers`），VNF 的 vCPU 从隔离核池分配。核数规划见 §6.5 |
| 内存 | ≥ 8 GB | 1G 大页按 `min(RAM_GB/4, 8)` 预留（安装器保守默认），VNF 内存从大页池分配 |
| 网卡 | 管理口 1 个 + 业务口 ≥ 1 个 | 管理口**保持内核驱动**（SSH/API 走它）；业务口交 DPDK（vfio-pci）给 VPP。需为 DPDK 支持的网卡（vmxnet3/ixgbe/i40e/virtio 等） |
| 磁盘 | ≥ 40 GB | 镜像仓库、VM 磁盘、崩溃转储与诊断归档共用 |
| IOMMU | 建议开启 | vfio 直通需 `intel_iommu=on` / `amd_iommu=on`（内核基线会按 CPU 厂商**自动补**，见 §6.2）；无 IOMMU 时需打开 `enable_unsafe_noiommu_mode` |

### 1.2 软件底座

| 组件 | 版本 | 用途 |
|---|---|---|
| Ubuntu Server | 26.04 | 宿主（本版本只在该平台验证） |
| VPP | 26.06 | 数据面：BD/VRF/BVI/ACL/NAT/SPAN/policer/bond/LLDP 插件 |
| libvirt / QEMU | 12.0 / 10.x | VM 编排（vhost-user、快照、串口 console） |
| Docker | 29.x | 容器 VNF（memif） |
| Go | ≥ 1.26 | 仅**从源码构建**时需要；用 deb 包则不需要 |
| cloud-image-utils | 任意 | VM 用 cloud-init seed（`cloud-localds`）；缺了 VM 无法引导 |
| qemu-utils | 任意 | VM 磁盘创建/克隆（`qemu-img`） |

> 底座缺失时 nfvisd **降级运行**（对应编排能力不可用并产生告警），不会拒绝启动——
> 但「降级运行」意味着**装了也用不了对应的功能**，请按 §6/§7 完成底座准备。
> `dpkg -i` 安装时 postinst 会逐项探测并打印缺失项，照着装即可。

### 1.3 平面划分（先建立这个概念，后面所有章节都用它）

NFViS 把整机划成三个平面，**网卡的归属由平面决定**：

| 平面 | 跑什么 | 网卡 | 内核/驱动 |
|---|---|---|---|
| 管理面 | SSH、REST API、CLI、libvirt、Docker | 管理口（如 `ens160`） | **内核驱动**，绝不交 DPDK（产品会直接拒绝，见 §7.1） |
| 数据面 | VPP 转发（L2/L3/ACL/NAT…） | 业务口（如 `ens192`/`ens224`） | **DPDK 接管**（内核里网卡消失），端口是否进 VPP 由配置声明决定（§7.3） |
| 计算面 | VM（QEMU）、容器 | 走 vhost-user/memif socket 与数据面互通 | vCPU 从隔离核池绑核，内存从大页池分配 |

由此推出三条铁律（后文反复出现）：

1. **管理口永远留在内核**——绑定管理口会当场失去 SSH 与管理 API，只能带外重启恢复；
2. **业务口先 DPDK 接管、再配置声明、再 `request vpp restart`**，顺序不能反（§7.3）；
3. **VPP 只能看到配置声明过的口**——没声明的口重启数据面后就不在了（§7.4 掉口防呆）。

---

## 2. 安装与卸载

### 2.1 方式 A：deb 包（生产推荐）

deb 包**必须在 Linux 上构建**（依赖 `dpkg-deb`），目标平台 linux/amd64：

```bash
# 在构建机上（任意 Linux + Go ≥1.26；**还需要 make**，dpkg-deb 随 dpkg 已有；构建不需要 gcc）
sudo apt-get install -y make          # ⚠️ 全新 Ubuntu Server 不带 make：缺了直接 make: command not found
git clone <repo> && cd nfvis
make deb VERSION=1.1.19               # 产物：build/nfvis_1.1.19_amd64.deb
```

包内布局：

| 路径 | 内容 |
|---|---|
| `/usr/bin/nfvisd`、`/usr/bin/nfvis-cli` | 守护进程与 CLI |
| `/lib/systemd/system/nfvis.service` | systemd 单元（`Type=notify`、`Restart=always`、`WatchdogSec=30`） |
| `/usr/share/nfvis/installer/nfvis-baseline.sh` | 内核基线落地脚本（§6） |
| `/usr/share/nfvis/installer/nfvis-ssh-harden.sh` | SSH 强化脚本（§2.2⑥） |
| `/usr/share/doc/nfvis/` | OpenAPI 契约、CLI 命令树、规格书、用户手册、命令全表 |

安装：

```bash
sudo dpkg -i build/nfvis_1.1.19_amd64.deb
```

### 2.2 安装脚本做了什么（`postinst` 逐条）

postinst 的设计原则是「**校验与提示为主，不阻断安装**」——底座差异只会产生提示，不会让 `dpkg` 失败。逐条：

1. **建数据目录**：`/var/lib/nfvis/{images,backup,captures,coredumps,tech-support,vms}`、
   `/data/incoming`、`/run/nfvis`，以及 **0777** 的 `/run/nfvis/{vhost,memif}`（QEMU/容器要访问 VPP 的 socket）；
2. **core dump 检查**：`/proc/sys/kernel/core_pattern` 未指向 `/var/lib/nfvis/coredumps` 时**仅提示**（§10.8）；
3. **大页检查**：只看 **1G 池**（`/proc/meminfo` 的 `HugePages_Total` 是各尺寸合计，Ubuntu 默认就有
   1024 个 2M 页，用它判断会误报）；缺失时给出可照做的指引（§6）；
4. **isolcpus 检查**：内核命令行没有隔离核时提示（§6）；
5. **底座探测**：`vpp` / `libvirtd` / `dockerd`，缺失时**警告**（对应能力降级）；
6. **SSH 强化**：写 drop-in `/etc/ssh/sshd_config.d/99-nfvis.conf` 禁用 root **口令**登录（root 密钥登录保留）。
   自带防锁死闸门：找不到密钥登录路径时主动跳过；**不重启 sshd**（下一次 sshd 启动才生效，不会切断当前会话）；
7. **内核基线**：无基线时按机器规格应用保守默认——1G 大页 = `min(RAM_GB/4, 8)`、**不设隔离核**，
   写 GRUB 片段 + fstab 并 `update-grub`（输出形如 `按机器规格取默认：RAM 7G → 1G 大页 1 页`），
   **需重启生效**；已有基线则只做一致性检查；
8. **systemd 装载**：`daemon-reload` + `enable`；**首次安装不自动 start**（避免安装期抢占网卡）；
   **升级时不停止**已在运行的 nfvisd（由 postinst 换新版本二进制并 try-restart）。

> **基线会自动补全本机参数**：安装期写出的 GRUB 片段除大页外还含按 CPU 厂商自动补的参数
> （Intel：`intel_iommu=on intel_pstate=disable`；AMD：`amd_iommu=on amd_pstate=disable`；都补 `iommu=pt`）。
> 实测这能把 VMware 等 vIOMMU 真正打开，vfio 绑定不再需要不安全模式。

### 2.3 安装后检查清单

```bash
dpkg -l nfvis                                       # 已安装
systemctl is-enabled nfvis.service                  # enabled
ls /usr/bin/nfvisd /usr/bin/nfvis-cli               # 二进制就位
ls -d /var/lib/nfvis/{images,backup,captures,coredumps,tech-support,vms} /data/incoming
cat /etc/default/grub.d/99-nfvis.cfg                # 基线片段（需重启生效）
ls /etc/ssh/sshd_config.d/                          # 应有 99-nfvis.conf
```

### 2.4 方式 B：从源码运行（开发/验证）

```bash
git clone <repo> && cd nfvis
go build ./... && go vet ./...
make check                        # vet + 全量测试 + 覆盖率门槛 + 各类契约守护（提交前必跑）

# 直接运行（开发用；不装 systemd 单元；明文 HTTP 必须显式开关）
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

### 2.5 升级与降级

```bash
# 升级：直接装新 deb。postinst 会 try-restart 已在运行的 nfvisd 加载新版本（服务保持运行换二进制）
sudo dpkg -i nfvis_1.1.19_amd64.deb
systemctl is-active nfvis && /usr/bin/nfvisd -version   # 验证版本

# 降级：dpkg 允许；同上验证版本即可
sudo dpkg -i nfvis_1.1.18_amd64.deb
```

> 配置库在 `/var/lib/nfvis/nfvis.db`，**升级/降级/purge 都不会动它**（见 §2.6）。
> 保险起见，动包管理前先备份（§10.7 的 `request system configuration backup`，或直接
> `sqlite3` 在线备份 `/var/lib/nfvis/nfvis.db`）。

`request system software add` 是**经 CLI/API 的在线升级**路径（校验 sha256 → 升级 → 重启
nfvisd → 报告），见 §10.11。

### 2.6 卸载（purge）与残留清理

```bash
sudo dpkg --purge nfvis
```

purge 的行为（postrm）：

| 项 | purge 时怎么处理 |
|---|---|
| `/run/nfvis` | **自动清理** |
| SSH 强化 drop-in `/etc/ssh/sshd_config.d/99-nfvis.conf` | **自动撤销**（删除 → `sshd -t` 校验 → reload；恢复 sshd 默认） |
| `/var/lib/nfvis/`（配置库/镜像/备份） | **保留**（避免误删；恢复出厂请显式 `request system zeroize`） |
| `/etc/default/grub.d/99-nfvis.cfg`、fstab 大页行 | **不自动改**，打印可照做的手工步骤（`rm 片段 && update-grub`；删 fstab 标记行）——这两个改动要重启才彻底生效 |

> 配置库保留意味着 purge 后重装会**回到原配置**（含用户与口令）。要彻底清零，
> 重装后执行 `request system zeroize`（双重确认，见 §10.7）。

---

## 3. 首次启动与登录

### 3.1 启动守护进程

```bash
sudo systemctl start nfvis
systemctl status nfvis          # active (running)
journalctl -u nfvis -f          # 观察启动日志
```

启动参数（systemd 单元已设，可经 `/etc/systemd/system/nfvis.service.d/` 覆盖）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-db` | `/var/lib/nfvis/nfvis.db`（单元内） | SQLite 配置库 |
| `-listen` | `:443` | API 监听；通配时会**按管理口地址收敛** |
| `-tls-cert` / `-tls-key` | 空 | 缺省**自动生成自签证书**并启用 HTTPS |
| `-allow-plaintext` | 关 | **强制明文**，仅开发/测试；给出即忽略已装/自签证书 |
| `-init-admin-password` | 空 | 首次启动的 admin 口令；缺省随机生成 |
| `-vpp-sock` | `/run/vpp/api.sock` | VPP binary API |

### 3.2 取一次性 admin 口令

首次启动（库中无本地用户）会创建 `admin`（super-user），**随机口令只打印一次**：

```bash
journalctl -u nfvis --since "10 min ago" | grep 一次性口令
# % 首次启动已创建用户 admin (super-user)。一次性口令（仅显示一次，请立即修改）: XiJrwGuFApmADuaK@Aa1
```

> 口令字符集**不含** `!` `$` 反引号等 shell 敏感字符，可直接复制粘贴；
> 直接 `nfvis-cli -p <口令>` 或用**单引号**包裹均可用。

若口令已丢失：停服务、删库 `rm /var/lib/nfvis/nfvis.db` 会**清空全部配置**——生产环境不要这样做；
正确做法是用 `-init-admin-password` 重新引导或经其他 super-user 重置（见 §3.5）。

### 3.3 用 CLI 连接

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
| `-server` | `https://127.0.0.1:443` | **与守护进程缺省一致**；可用 `NFVIS_SERVER` 环境变量覆盖 |
| `-u` / `-p` | `admin` / `$NFVIS_PASSWORD` | 用户名/口令 |
| `-ca` | 空 | 服务端证书 PEM；**缺省固定本机 `/var/lib/nfvis/tls/server.crt`**（自签场景零配置） |
| `-insecure` | 关 | 跳过证书校验（仅调试） |
| `-source` | `ssh` | 接入源（`ssh`/`console`），影响管理口自锁保护与 `start shell` 权限 |
| `-c "…"` | 空 | **脚本模式**：执行多行命令后退出（换行分隔；任一行出错即停） |
| `-version` | - | CLI 版本（与守护进程同源注入） |

> **零参数即可连**：缺省 `https://127.0.0.1:443` 与守护进程缺省一致，
> 且因地址为 `https://`，客户端会**自动固定本机自签证书** `/var/lib/nfvis/tls/server.crt`
> ——「装完即用」。若 nfvisd 监听在别处（如开发用的 `-listen 127.0.0.1:18443
> -allow-plaintext`），用 `-server http://127.0.0.1:18443` 覆盖。

**脚本模式**（适合自动化；它在退出时会自动收尾——丢弃 candidate、退出配置模式、吊销 token，
**不要手写 `discard`**，否则重复释放会报 `%% 当前会话未持有 candidate`）：

```bash
nfvis-cli -c "configure
set system hostname fw-01
commit"
```

**交互模式**：`?` 列候选（**按键即时，不用回车**）、Tab 补全（唯一匹配自动补全、多匹配响铃并列出）、
无歧义缩写、`Ctrl-]` 退出串口、`Ctrl-C` 退出 monitor。详见 §4。

> `?` 是帮助键而非字面量：它不会进入命令行文本。因此取值里**不能直接输入 `?`**
> （与 Cisco IOS 同构）。

### 3.4 用 REST API 访问

```bash
# 取 token
TOKEN=$(curl -sk https://127.0.0.1/api/v1/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<口令>"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')

curl -sk https://127.0.0.1/api/v1/interfaces -H "Authorization: Bearer $TOKEN"
curl -sk --cacert /var/lib/nfvis/tls/server.crt https://127.0.0.1/api/v1/system/version
```

- 契约：`/usr/share/doc/nfvis/NFViS-openapi.yaml`；运行时端点 `GET /api/v1/openapi.json`（无鉴权）。
- `curl` 用自签证书需 `-k` 或 `--cacert /var/lib/nfvis/tls/server.crt`。
- 常用端点速查见 §10.10；`GET /api/v1/metrics` 与 `GET /api/v1/openapi.json` 无需鉴权。

### 3.5 用户、口令与权限 class

> **CLI 与 REST 同源**：`set system login user <n> password <s> class <c>` 会**加盐哈希**后落库
>（先过口令策略），且语句回显脱敏为 `«已隐藏»`。注意**用户名不能省**：
> `set system login user password <口令>`（漏写用户名）会被拒绝并提示正确写法；
> 只写 `set system login user <名字>`（无 password/class）也会被拒——避免静默建出空账号。

```bash
nfvis$ configure
nfvis# set system login user ops password '<强口令>' class operator
nfvis# set system login password-policy min-length 12
nfvis# set system login password-policy complexity true
nfvis# set system login password-policy expire-days 90
nfvis# set system login password-policy lockout-threshold 5
nfvis# set system login password-policy lockout-minutes 10
nfvis# top
nfvis# commit
nfvis$ show users
```

REST 等价（`POST /api/v1/system/login-users`，201）见配套 OpenAPI；改口令/改 class 用 `PUT`，
删除用 `DELETE`。

自助改密（登录者本人，验证旧口令）：

```bash
nfvis$ request system password change
```

预置 class 权限矩阵：

| 命令域 | super-user | operator | read-only |
|---|---|---|---|
| `show *` | ✔ | ✔ | ✔ |
| `wizard` / `configure`（配置模式全部） | ✔ | ✘ | ✘ |
| `request`（生命周期/镜像/接口） | ✔ | ✔ | ✘ |
| `request`（software/reboot/configuration/zeroize） | ✔ | ✘ | ✘ |
| `clear` / `start shell` | ✔ | ✘ | ✘ |

自定义 class 可按命令树节点做 allow/deny（`set system login class <名> allow <路径>`）。

---

## 4. CLI 基础

> 这一节是后面所有章节的地基：模式与导航、事务模型、补全、管道。建议通读一遍。

### 4.1 两种模式与导航

```
nfvis>                              ← 操作模式：show / request / monitor / ping / wizard / clear
nfvis# configure                    ← 进入配置模式（super-user/operator）
[edit] nfvis#  …                    ← 配置模式：set / delete / commit / …
[edit system] nfvis#  …             ← edit <path> 进入层级，提示符显示当前位置
nfvis# top | up | exit              ← 回顶层 / 上一级 / 退出配置模式
```

命令缩写：**无歧义前缀即可执行**（`sh vi` ≈ `show virtual-machine-functions`）；
`?` 在任意位置列出候选，Tab 补全。配置模式内可用 `run <操作命令>` 执行操作命令（不必退出）。

### 4.2 事务模型（最重要的一条）

所有配置都经**事务**，不存在「敲下即生效」：

```
set/delete …   →  candidate（候选配置，仅本会话可见；会话按 用户@接入源 隔离并持锁）
commit         →  schema/语义校验 → 下发底座（失败自动补偿回滚）→ committed
rollback [n]   →  取历史快照为 candidate（还需再 commit）——注意 n 从 1 起；
                  丢弃未提交改动用 discard，rollback 0 不存在
commit confirmed [分钟] → 超时未确认则自动回滚（改管理口等高风险操作的自锁保护）
```

`commit` 的变体：

| 命令 | 用途 |
|---|---|
| `commit` | 校验 → 下发 → 提交 |
| `commit check` | 只校验不下发（看会报什么错） |
| `commit confirmed [分钟]` | 提交但设确认窗口；**超时未确认自动回滚**。用于「改完可能断自己」的操作 |
| `commit and-quit` | 提交并退出配置模式 |

**哪些操作要求 `commit confirmed`**：管理口的地址/网关/网卡名**任何变更（含首次声明）**——
若会话来自 SSH，commit 会拒绝普通 commit 并输出自锁警告；普通 commit 被拒时改用
`commit confirmed` + 再次确认即可。确认方式：在窗口期内再提交一次（或按提示确认）。

**commit 失败不会留下半成品**：底座下发失败会补偿回滚，并在输出里说明原因（如
`下发失败 vrf[vs-l3]: 接口在 VPP 中不存在: ens192`）。校验类失败会逐条列出（如资源不足、
VPP 线程核不在隔离核内）。

**提交的文档必须至少保留一个 super-user 账号**：本地账号是唯一的登录途径，提交后一个
`class super-user` 都不剩，就等于把自己锁在门外（只能带外恢复）。这条对**整文档替换**的几条路
一视同仁：`commit`、`load override`（等价 REST `PUT /configuration/candidate`）与
`request system configuration restore`。注意**没写 class 的账号按只读算**，不算 super-user。
被拒时错误会说明还缺什么，候选与编辑锁都留着，补一个 super-user 再提交即可；要复位全部账号
请用 `request system zeroize`（恢复出厂）——那是唯一被允许把账号表清空的路径。

会话锁：一个会话持 candidate 时，其他会话不能取得写锁（报 `candidate 会话锁被占用: 由 … 持有`，
直到对方 commit/discard 或空闲超时）。查看谁持锁：`show system configuration sessions`。

**「提交即生效」的写操作不会占着锁**：`system login-users` 一族（建/删用户、改权限类、重置口令、
自助改密）与带 `X-NFVIS-Auto-Commit: true` 的直提写都是**一次性事务**——提交生效后立刻交还会话锁，
不必（也不该）为了放锁而登出；提交**因校验失败**时锁与候选都留着，让你接着改。
REST 客户端注意：直提之后再读 `GET /configuration/candidate` 会得到 409（本会话已不在编辑态），
要基于生效配置继续写请读 `GET /configuration`。

### 4.3 compare / rollback / load / save / annotate

| 命令（配置模式内） | 用途 |
|---|---|
| `show` | 当前层级的 candidate |
| `show \| compare` | candidate ⇄ committed 的差异（提交前必看） |
| `show configuration`（操作模式） | committed 配置（块状） |
| `show configuration \| compare rollback <n>` | committed ⇄ 第 n 个历史快照 |
| `rollback [n]` | 取第 n 个历史快照为 candidate（还需再 commit） |
| `annotate <path> "注释"` | 给节点加注释（路径相对当前层级） |
| `save <file>` | candidate 导出 JSON（落盘 0600） |
| `load override \| merge <file>` | JSON 导入（override 整体替换 / merge 合并） |
| `discard` | 丢弃 candidate 并释放会话锁 |
| `show \| display set` | **未支持**（需 model→语句的反向映射）；替代：`save`/`show configuration`/`\| display json` |

> `show configuration \| compare rollback <n>` 与配置模式 `\| compare` 均已实现；
> `rollback 0` 不存在（快照编号从 1 起）。

### 4.4 通用管道（所有 show 输出可用）

```bash
nfvis$ show interfaces | match ens          # 只留匹配行
nfvis$ show interfaces | except down        # 排除匹配行
nfvis$ show interfaces | count              # 行数
nfvis$ show log system | last 20            # 末 N 行
nfvis$ show log system | begin alarm        # 从首个匹配行开始显示
nfvis$ show configuration | display json    # JSON 输出（配置 show 族）
```

### 4.5 配置层次总览

```
system           主机名/时区/NTP/DNS/API/管理口/内核参数/健康阈值/syslog/登录与口令策略
protocols lldp   LLDP（独立顶级层级）
interfaces       物理口（描述/MTU/启停/SR-IOV/限速绑定）
bonds            链路聚合
virtual-switches 虚拟交换机（l2 = bridge-domain，l3 = VRF）
acls / nat / port-mirroring / qos   高级网络功能
resource-pools   大页池与隔离核（唯一真源，VPP 保留核从中扣减）
vpp              数据面运行时配置（生成 startup.conf）
virtual-machine-functions   VM VNF
container-functions         容器 VNF
```

### 4.6 配置态 vs 运行态

- **配置态**：模型里的声明（`show configuration`）；
- **运行态**：底座实际状态（`show interfaces physical`、`show vrfs <n> routes` 等）。

两者可能短暂不一致（底座未收敛）；nfvisd 会在启动与 VPP 重连时**恢复收敛**，
未收敛项进告警（`show alarms`）。判断「命令答的是哪一边」：
`show` 族凡带 `statistics/mac-table/routes/physical` 的都是运行态，其余多为配置态。

---

## 5. 初始化：wizard 向导

### 5.1 是什么、什么时候用

`wizard` 是**交互式初始化向导**：按问答规划「隔离核 / VPP 线程 / 大页池 / 低延迟」四件事，
展示将要提交的语句清单，确认后自动执行 `configure → set… → commit → request system kernel apply`。
适合**首次配置**；在已配置机器上重跑会沿用既有值（相同值显示为「未产生配置变更」，不算错误）。

```bash
nfvis$ wizard
```

- 只在**交互终端**可用；管道/脚本下打印指引即返回（不挂起）。
- **不绕过任何校验**：向导只是语句的生成器，commit 校验与内核基线护栏（§6.3）原样生效；
  向导阶段就会提前给出同类可读错误（宿主核不足、线程不在隔离核内等）。
- 数据口的 DPDK 绑定/声明**不在向导范围内**（运行期动作），向导结束会打印后续动作清单（§7.5）。

### 5.2 问题与默认值

| 问题 | 默认值（Enter 即取） | 说明 |
|---|---|---|
| 1/4 隔离核 | `2-<末核>`（6 核机 = `2-5`） | 保留 0-1 给宿主/管理面；**宿主核至少留 2 个**；3 核机默认隔离 `2`；核数不足以保留 2 个时不给默认，要求显式输入 |
| 2/4 VPP 主线程核 | 隔离范围**最大**核 | 输入 `-` 表示不配置 VPP CPU；核必须在隔离范围内 |
| 2b/4 VPP 工作线程核 | 隔离范围**次大**核（无则不设） | 与主线程同核会被拒 |
| 3/4 1G 大页数量 | 沿用既有池；否则 `min(RAM_GB/4, 8)` 钳 1~8 | VNF 内存从 1G 池分配 |
| 3b/4 2M 大页数量 | 沿用既有池；否则 `768`（输入 0 = 不配） | VPP 缓冲用（`hugepage-preference 2M` 时） |
| 4/4 低延迟参数组 | `false` | 代价一句话明说（§6.4）；虚拟机上自动省略机型相关项 |

任意一步输入 `q` 中止（未做任何变更）。全部答完会显示计划与语句清单，最后一次确认（默认 yes）。

### 5.3 输出样例（6 核、7 GB 内存、未配置过的机器）

```
NFViS 初始化向导（wizard）——规划资源池与内核基线（Enter 取 [默认]；输入 q 中止）。
本机事实：在线核 6 个；内存 7 GB；committed 现有大页池 （无）。

1/4 隔离核（供 VPP 与 VM 使用，宿主/管理面保留其余至少 2 个） [默认 2-5]：
2/4 VPP 主线程核 [默认 5]（输入 - 表示不配置 VPP CPU）：
    VPP 工作线程核 [默认 4]（输入 - 表示不设工作线程）：
3/4 1G 大页数量（VM 内存从 1G 池分配） [默认 1]：
    2M 大页数量（VPP 缓冲用；0 = 不配 2M 池） [默认 768]：
4/4 低延迟参数组（mitigations=off 等：降低安全缓解与可诊断性；虚拟机上自动省略 idle=poll/tsc=reliable） [默认 false]：
    启用？true/false [默认 false]：

—— 将提交以下语句 ——
  configure
  set resource-pools hugepages page-size 1G count 1
  set resource-pools hugepages page-size 2M count 768
  set resource-pools cpu isolated-cores 2-5
  set vpp cpu main-core 5
  set vpp cpu corelist-workers 4
  set vpp memory hugepage-preference 2M
  top
  commit
  request system kernel apply
确认提交？[yes/no]（默认 yes）：

向导完成。内核基线需重启生效：request system reboot。
重启后的固定动作（数据口绑定不跨重启）：request interfaces <数据口> bind-dpdk --yes → request vpp restart。
（vfio 模块由绑定命令自动加载并持久化开机加载，无需手工 modprobe）
数据口的声明（set interfaces / set vpp dpdk dev）不在向导范围内，见用户手册「3.2 业务网卡交 DPDK」。
```

### 5.4 向导完成后还差什么

1. **重启**（`request system reboot`）让内核基线生效，核对 `show system kernel` 三方一致；
2. **数据面**：`request interfaces <业务口> bind-dpdk --yes` →
   配置声明（§7.3）→ `request vpp restart`（vfio 模块由绑定命令自动加载并持久化开机加载）；
3. 镜像导入与业务创建（§9）。

---

## 6. 内核基线：大页与隔离核

> 这一步做完之前，nfvisd 能启动、能登录、能配（配置落库），但**数据面与计算面不可用**。
> 基线写入 GRUB，**需重启生效**；运行期只有 2M 页可以追加预留，1G 页必须开机时给足。

### 6.1 两条操作者路径（wizard / CLI）

**方式 0：`wizard`**（推荐）——见 §5。

**方式 A：经 CLI**（配置态 → 写 GRUB → 重启）

```bash
nfvis$ configure
nfvis# set resource-pools hugepages page-size 1G count 8
nfvis# set resource-pools hugepages page-size 2M count 768
nfvis# set resource-pools cpu isolated-cores 4-15
nfvis# set vpp cpu main-core 15
nfvis# set vpp cpu corelist-workers 4-14
nfvis# top
nfvis# commit                      # 会警告：内核基线与配置不一致，需写入 GRUB 并重启生效
nfvis$ request system kernel apply # 写 GRUB 片段（两条路径同一生成器）
nfvis$ request system reboot
```

`request system kernel rollback` 回退到上一次片段（写入前自动备份到
`/var/lib/nfvis/kernel-baseline.bak`；`update-grub` 失败也会自动回退，不留未生效配置）。

> **内部恢复工具**：`/usr/share/nfvis/installer/nfvis-baseline.sh` 保留在包内，但**日常调整不走它**
> ——它的操作者用法已由 wizard 与 `request system kernel apply` 取代（同一生成器，避免两套用法漂移）。
> 它只在两个场景直接使用：安装期基线预置（postinst 自动调用）；**守护进程不可用时的救急**
> （不碰配置库、不需要登录）：`--check` 看现状、`--apply …` 直接写 GRUB 片段、`--rollback` 回退。
> 卸载后的基线清理步骤见 purge 输出（手工删除片段并 `update-grub`）。

### 6.2 自动补全规则（生成器按本机事实）

写入的 GRUB 片段除显式参数外**自动补全**（补全值与显式参数同名冲突时，以显式为准）：

| 补全项 | 内容 | 依据 |
|---|---|---|
| CPU 厂商参数 | Intel 补 `intel_iommu=on intel_pstate=disable`；AMD 补 `amd_iommu=on amd_pstate=disable`；两者都补 `iommu=pt` | `/proc/cpuinfo` 的 vendor_id |
| `irqaffinity=` | 隔离核的**补集**（中断亲和到非隔离核上）；用户已给则不覆盖 | 隔离核 + 在线核 |
| `nohz_full=` / `rcu_nocbs=` | 内核未编入 nohz_full 支持时**省略**（写了也只会被忽略） | sysfs / 内核配置探测 |
| 双大页池 | 1G 与 2M 都配置时连写 `hugepagesz=1G hugepages=N hugepagesz=2M hugepages=M` | resource-pools（1G 给 VM、2M 给 VPP） |

生效后核对：

```bash
grep -E 'hugepages|isolcpus|irqaffinity' /proc/cmdline
grep HugePages_Total /proc/meminfo
cat /sys/devices/system/cpu/isolated           # 隔离核生效清单
nfvis$ show system kernel                      # 三方对照（cmdline / 运行实际 / 配置期望）
```

### 6.3 护栏（写错 isolcpus 是重启后才暴露的「进不了系统」级错误）

以下情况**生成被拒绝、什么都不会写**（CLI 与脚本两条路径同样生效）：

- 隔离核包含本机不存在的核号；
- **非隔离核不足 2 个**（内核自身、中断处理、管理面需要；向导与脚本同样把守）；
- 附加参数（`--params`）里夹带 `isolcpus=` 同样被拦。

被拒时的报错可直接照做，例如：
`隔离核 0-5 会把本机 6 个核几乎全部隔离（只剩 0 个非隔离核），至少需保留 2 个给内核/中断/管理面；请缩小隔离核范围`。

### 6.4 低延迟 profile（可选，显式开启）

```bash
nfvis$ configure
nfvis# set system kernel low-latency true
nfvis# top
nfvis$ request system kernel apply
```

写入 `mitigations=off audit=0 mce=off nosoftlockup numa_balancing=disable`（NMI watchdog
未托管时一并补 `nmi_watchdog=0`），**裸机再补 `idle=poll tsc=reliable`**。

**代价要知道**：`mitigations=off` 关闭 CPU 安全缓解、`mce/nosoftlockup` 关闭底层故障排查手段、
`idle=poll` 让核常驻满载（功耗/发热变大）；`tsc=reliable` 在 guest 里未必成立。**虚拟机里产品会
自动省略 `idle=poll/tsc=reliable`**。确认接受再开启；关闭用 `set system kernel low-latency false` 再 apply。

### 6.5 规划速查

| 资源 | 规划规则 |
|---|---|
| 隔离核 | = VPP 线程核 + 全部 VM 的 vCPU 数。宿主/管理面至少留 2 个核 |
| VPP 线程 | `main-core` + `corelist-workers` 都必须在隔离核内；worker 数建议与业务口 `rx-queues` 一致 |
| 1G 大页 | 每台 1G 内存的 VM 占 1 页；**VPP 用 2M 页（`hugepage-preference 2M`）把 1G 页让给 VM** |
| 2M 大页 | VPP 缓冲用，768 页（1.5G）够起步；可运行期追加 |
| 池子不足 | commit 明确报错（`无 1G 大页资源池，无法分配 …MB` / `隔离核不足：需要 N，可用 0`） |

---

## 7. 数据面：网卡、DPDK 与 VPP

### 7.1 管理口守卫（产品会直接拒绝）

`request interfaces <管理口> bind-dpdk` / `unbind-dpdk` **一律拒绝**（无论是否在
`set system management interface` 里声明过）。判据是三条内核/配置事实：**配置声明的管理口**、
**承载默认路由的口**、**守护进程正在监听的网卡**（启动日志有「管理口守卫事实」一行可核对）。

拒绝是有意的——绑定管理口会**当场**失去 SSH 与管理 API，只能带外重启才能恢复（真机实测过）。
若确需变更该网卡的驱动，用 §7.2 的**方式 B** 带外操作。

### 7.2 业务口绑定与解绑（vfio-pci）

**前置条件**：`vfio-pci` 模块**由绑定命令自动加载**——首次绑定若模块未加载会自动 `modprobe`，
成功后写入 `/etc/modules-load.d/nfvis-vfio-pci.conf` 持久化开机加载，操作者无需手工干预
（重启后网卡回内核驱动、绑定要重做，但模块始终在位）。唯一仍需手工的是**无 IOMMU 的机器**：
不开不安全模式时绑定会报 `No such device`（判据：`ls /sys/kernel/iommu_groups/` 为空 = IOMMU 未生效，
首选重启让内核基线生效）：

```bash
echo Y > /sys/module/vfio/parameters/enable_unsafe_noiommu_mode   # 仅无 IOMMU 时才需要
```

```bash
# 绑定（会中断该网卡现有流量，须 --yes）
nfvis$ request interfaces ens224 bind-dpdk --yes
# 解绑：接管后内核里已无该网卡，用 PCI 地址或**口名**均可
# （绑定过的口产品记着「口名 → PCI」）；实测必须显式给 to-driver
nfvis$ request interfaces ens224 unbind-dpdk to-driver vmxnet3 --yes
```

> ⚠️ **解绑前先把该口移出数据面**（在配置里删掉它的 DPDK 声明与接口声明 → `request vpp restart`）。
> 对**正在被数据面使用**的口直接 `unbind-dpdk`：该口悬空、命令迟迟不返回，
> 且 CLI 通道会被占住直到重启守护进程（真机实测；处置见 §11）。

> **先弄清有哪些口（两侧候选来源不同）**：`set vpp dpdk dev <ifname>` 的 Tab 候选 =
> **VPP 中的接口**（已被 DPDK 接管的口），与 `show interfaces physical` 同源；
> **尚未接管的内核网卡**出现在 `request interfaces <n> bind-dpdk` 的候选里。
> 已被接管的口在内核里已无网卡（`ip link` 看不到），所以只能这样发现。

**方式 B：带外手工**（虚机重启后网卡回到原生驱动、CLI 不可用时用这个）

```bash
modprobe vfio-pci
echo Y > /sys/module/vfio/parameters/enable_unsafe_noiommu_mode   # 无 IOMMU 时才需要
for d in 0000:0b:00.0 0000:13:00.0; do
  echo $d > /sys/bus/pci/drivers/vmxnet3/unbind 2>/dev/null || true
  echo vfio-pci > /sys/bus/pci/devices/$d/driver_override
  echo $d > /sys/bus/pci/drivers/vfio-pci/bind
done
```

> 方式 B 只解决**驱动接管**；要让口出现在数据面里，仍须按 §7.3 在配置中声明它们。

实测平台行为：绑定后**内核网卡消失**，只能按 PCI 地址定位；清空 `driver_override` + `rescan`
**不足以**让内核重新探测原生驱动 → 解绑**必须**给 `to-driver`。

### 7.3 把业务口交给 VPP（声明 → 重启数据面）

网卡交 vfio-pci 只是第一步：**VPP 里有哪些口，由配置声明决定**。三步走，顺序不能反：

```bash
# ① 声明每一个要用到的业务口 + 声明它们由 DPDK 接管（缺一不可）
nfvis$ configure
nfvis# set interfaces ens192
nfvis# set interfaces ens224
nfvis# set vpp dpdk dev ens192
nfvis# set vpp dpdk dev ens224
nfvis# top
nfvis# commit
# ② 按配置重生成 startup.conf 并重启数据面
nfvis$ request vpp restart
# ③ 确认：接口出现在数据面（状态 down 属正常，尚未配 L2/L3）
nfvis$ show interfaces physical
vppctl show interface
```

**要点**：

- ① 的 commit **会成功**，即便这些口此刻还没进数据面：产品的做法是**延后收敛**
  （日志会写明「尚未进入数据面…执行 request vpp restart 后自动收敛」）；
- **「vpp 变更待重启」由 commit 自己警告**（`警告: vpp 变更需 request vpp restart … 后生效`），
  照做即可；
- 产品生成 `/etc/vpp/startup.conf` 时，把口名解析成 PCI 靠的是**绑定时记下的映射**
  （`/var/lib/nfvis/dpdk-bindings.json`）——所以务必先 `bind-dpdk` 再声明；顺序反了会在
  ② 报「解析 … 的 PCI 地址失败」；
- 覆盖安装时若原来手写过 `startup.conf`，产品启动时会把其中的端口映射读进记录（不必重新绑定），
  但**文件本身会被产品重生成覆盖**；
- 同一提交里把尚未进数据面的口挂进虚拟交换机/VRF/bond **仍会失败**（那些对象按数据面里的
  接口定位）。顺序是「先让口进数据面（本节②），再挂交换机」。

### 7.4 startup.conf 与掉口防呆

`request vpp restart` 用当前 committed 配置**重生成**整个 `/etc/vpp/startup.conf`：

```conf
cpu { main-core 4  corelist-workers 5 }
memory { default-hugepage-size 2M }
dpdk { dev 0000:0b:00.0 { name ens192 }  dev 0000:13:00.0 { name ens224 } }
```

**没在配置里声明的口，重启后就不在数据面里了**——即使它此刻还在（比如只写进了手写的
startup.conf）。为此产品在重生成前会告警：
`以下已由 DPDK 接管的物理口未在配置中声明，重启数据面后将不再出现在数据面：…`。
看到它请先补齐 `set vpp dpdk dev <口>` 再重启；如需回退，备份 `/etc/vpp/startup.conf`
是没有用的（会被覆盖），要改的是**配置**。

### 7.5 重启后的固定动作清单（备忘）

宿主重启后，DPDK 绑定与 VPP 都回到未初始化状态（vfio 绑定不跨重启）：

```bash
nfvis$ request interfaces ens192 bind-dpdk --yes               # ① 逐口重绑（管理口守卫照常生效；vfio 模块自动加载）
nfvis$ request interfaces ens224 bind-dpdk --yes
nfvis$ request vpp restart                                     # ② 按 committed 重生成并重启数据面
nfvis$ show interfaces physical                                # ③ 核对两口在列
```

> committed 配置本身是持久的（config 声明与「口名→PCI」记录都在磁盘上），所以重启后
> **不需要**重新声明，只需要②③两步。

---

## 8. 网络配置任务

> 以下示例均经真机验证。统一用 `nfvis-cli`，为简洁记 `nfvis$`（操作模式）与 `nfvis#`（配置模式）。

### 8.1 系统基线

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
nfvis# set system syslog local max-size-mb 100
nfvis# set system health thresholds cpu-temp-celsius 90
nfvis# set system health thresholds disk-used-percent 85
nfvis# set system idle-timeout-minutes 10
nfvis# top
nfvis# commit
```

> ⚠️ **管理口的任何变更（含首次声明）必须用 `commit confirmed`**：改管理口可能切断当前 SSH
> 会话，普通 commit 会被拒并输出自锁警告；confirmed 让它在超时未确认时自动回滚。
> 管理口 IP 只在下次启动时收敛监听（运行期不热改地址）。

其他系统级语句：`set system api port <uint>`、`token-ttl-minutes`、`max-sessions`、
`api tls cert-file <p> key-file <p>`（装外部证书，立即生效）、`api tls self-signed regenerate`
（重签自签）、`set protocols lldp …`（见 §8.10）。

### 8.2 资源池与 VPP 运行时

```bash
nfvis# edit resource-pools
nfvis# set hugepages page-size 1G count 8
nfvis# set hugepages page-size 2M count 768
nfvis# set cpu isolated-cores 4-15
nfvis# top
nfvis# edit vpp
nfvis# set cpu main-core 15
nfvis# set cpu corelist-workers 4-14
nfvis# set memory main-heap-size 2G
nfvis# set memory buffers-per-numa 16385
nfvis# set memory hugepage-preference 2M       # 须与 resource-pools 的页大小一致（commit 校验）
nfvis# set dpdk dev rx-queues 2                # 全局默认（作用于未单独配置的物理口）
nfvis# set dpdk dev ens224 rx-queues 4         # 单网卡覆盖（该口须为 DPDK 接管的物理口）
nfvis# set dpdk dev ens224 tx-descriptors 2048
nfvis# delete dpdk dev ens224 rx-queues        # 仅删单项，回落全局默认
nfvis# set plugins nat state disable           # 插件开关（acl/nat/span/dpdk/linux-cp…）
nfvis# top
nfvis# commit
# 输出警告：vpp 变更需 request vpp restart（或整机 reboot）后生效
nfvis# exit
nfvis$ request vpp restart
```

联动约束（commit 语义校验，违者明确报错）：

1. `vpp cpu` 的核必须在 `resource-pools cpu isolated-cores` 内；
2. VPP 保留核先从隔离核池扣减，剩余才给 VM（`show resource-pools` 单独展示 `vpp-reserved`）；
3. `hugepage-preference` 必须与所配大页池的页大小一致；
4. `dpdk dev <ifname>` 必须是 DPDK 接管的物理口。

核对：

```bash
nfvis$ show resource-pools      # 大页/隔离核的 总量/已分配/空闲，含 vpp-reserved
nfvis$ show vpp                 # 版本/线程/buffer/内存（含 pending_restart 提示）
nfvis$ show vpp threads
nfvis$ show vpp memory
nfvis$ show system kernel       # 三方对照（cmdline / 运行实际 / 配置期望）
```

### 8.3 L2 虚拟交换机 + BVI 网关

```bash
nfvis$ configure
nfvis# edit virtual-switches vs-dmz
nfvis# set type l2
nfvis# set vlan access 100
nfvis# set ports 1 interface ens224
nfvis# set ports 2 vnf fw-vm interface eth0
nfvis# set gateway ip 192.168.100.1/24
nfvis# set gateway acl-in acl-web
nfvis# top
nfvis# commit
```

L2 交换机在 VPP 侧落地为 **bridge-domain**；`gateway ip` 创建 BVI（三层网关），
缺省归入专属 VRF `vr-<交换机名>`（也可 `set gateway vrf <name>` 挂到别的 VRF）。
端口支持 trunk（`trunk vlans 100,200`）与 native VLAN。

核对：

```bash
nfvis$ show virtual-switches vs-dmz detail
nfvis$ show virtual-switches vs-dmz ports
nfvis$ show virtual-switches vs-dmz mac-table
nfvis$ show virtual-switches vs-dmz statistics
```

### 8.4 L3 虚拟交换机 + 静态路由

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

### 8.5 ACL

```bash
nfvis# edit acls acl-web
nfvis# set rule 10 source any destination any protocol tcp destination-port 443 action permit
nfvis# set rule 20 source any destination 10.0.0.0/8 protocol any action deny
nfvis# set rule 20 direction ingress
nfvis# top
nfvis# edit virtual-switches vs-wan
nfvis# set l3-interface ens192 acl-in acl-web        # 绑定到 L3 接口入向
nfvis# top
nfvis# commit
```

核对：`show acls`、`show acls acl-web detail`。

### 8.6 QoS（VPP policer）

```bash
nfvis# edit qos
nfvis# set policies pol-1g cir 1000000000 cbs 1000000    # bps / bytes
nfvis# top
nfvis# edit interfaces
nfvis# set ens192 ingress-policy pol-1g
nfvis# top
nfvis# commit
```

核对：`show qos policies`。

### 8.7 SPAN（端口镜像）

```bash
nfvis# edit port-mirroring span-1
nfvis# set source interface ens224 direction both     # 源也可为 vnf <vm> interface <vnic>
nfvis# set analyzer interface ens192
nfvis# top
nfvis# commit
```

### 8.8 NAT44

```bash
nfvis# edit nat
nfvis# set source-pool pool-a address-range 203.0.113.1 to 203.0.113.10
nfvis# set rules 10 match source 192.168.100.0/24 virtual-switch vs-wan action interface ens192
nfvis# set static 192.168.100.5 to 203.0.113.5        # 1:1 发布（可选）
nfvis# top
nfvis# commit
```

> NAT 约束：inside 转发域由 virtual-switch（须 l3）派生，outside 由**出接口所属 VRF** 派生
> （出接口须为某 l3 交换机的 l3-interface 且已配地址）；VPP NAT44 单实例仅一对 (inside, outside)，
> 故多规则的 virtual-switch 与出接口 VRF 必须各自一致。

核对：`show nat`。

### 8.9 链路聚合（bond）

```bash
nfvis# edit bonds bond0
nfvis# set members 0 ens224
nfvis# set members 1 ens223
nfvis# set lacp mode active interval fast    # 缺省静态聚合；lacp disable 关闭
nfvis# set mtu 9000
nfvis# top
nfvis# commit
```

bond 名可在一切接受接口名处引用（虚拟交换机端口、l3-interface、`vpp dpdk dev` 等），
与物理口等价。成员口须未被虚拟交换机引用。核对：`show bonds`、`show bonds bond0 detail`。

### 8.10 LLDP

```bash
nfvis# edit protocols lldp
nfvis# set enable true
nfvis# set advertisement-interval 30
nfvis# set interface ens192 enable true
nfvis# top
nfvis# commit
```

核对：`show lldp neighbors`（需对端也启用 LLDP；无对端时为空属正常）。

### 8.11 cross-connect（直通，慎用）

```bash
nfvis# edit virtual-switches vs-xc
nfvis# set type l2
nfvis# set cross-connect 1 2          # 端口 1 与 2 直通（与 ports/gateway 互斥）
nfvis# top
nfvis# commit
```

> ⚠️ **直通是二层裸对连：无 MAC 学习、无环路保护**。两端若处于同一广播域会形成物理环路/
> 广播风暴——实测导致整机 load 飙升、soft lockup、网络完全不可达。**两端必须属于不同广播域**；
> 出现风暴时先断开其一再改配置。

---

## 9. 计算负载：VM 与容器

### 9.1 镜像管理

**导入本地文件**（须先放入 `/data/incoming/`，成功后自动清理源文件）：

```bash
scp ubuntu-cloud.qcow2 root@<nfvis>:/data/incoming/
nfvis$ request images upload name ubuntu-cloud.qcow2 type vm-image file /data/incoming/ubuntu-cloud.qcow2
nfvis$ show images                          # import_state=ready 才可引用
```

**从 URL 拉取**（sha256 必填；异步受理，进度看 `import_state`）：

```bash
nfvis$ request images download name img.qcow2 type vm-image \
        url https://example.com/img.qcow2 sha256 <64位十六进制>
nfvis$ show images img.qcow2 detail
```

**容器镜像**（docker save 出的 tar）：

```bash
docker save alpine:3.20 -o /tmp/alpine320.tar
scp /tmp/alpine320.tar root@<nfvis>:/data/incoming/
nfvis$ request images upload name alpine:3.20 type container-image file /data/incoming/alpine320.tar
```

> ⚠️ 容器镜像的 `name` **必须与 tar 内的 Docker tag 一致**（如 `alpine:3.20`）。
> 写成文件名会导致下发时报 `docker: not found`（实际是镜像找不到）。

删除镜像有引用检查（被 VM/容器引用时拒绝）。

### 9.2 VM VNF 全流程

**① 定义并提交**（vCPU 从隔离核池分配并绑核、内存从大页池分配——池子不够 commit 会明确报错）：

```bash
nfvis$ configure
nfvis# edit virtual-machine-functions fw-vm
nfvis# set image ubuntu-cloud.qcow2
nfvis# set vcpu count 2                          # pin 默认 true（绑到隔离核）
nfvis# set memory size-mb 2048
nfvis# set memory hugepage-size 1G               # 指定页池（缺省取主池）
nfvis# set memory backing hugepage               # normal 则不用大页（此时禁止 vhost-user vNIC）
nfvis# set interfaces eth0 type vhost-user
nfvis# set interfaces eth0 virtual-switch vs-dmz
nfvis# set cloud-init hostname fw-vm
nfvis# set cloud-init user-data "#cloud-config
users:
  - name: admin
    ssh_authorized_keys:
      - ssh-ed25519 AAAA... you@host"
nfvis# set cloud-init ssh-key "ssh-ed25519 AAAA... other@host"
nfvis# set disks data0 size-gb 20                # 附加 virtio 数据盘（可选；或 image <名> 从镜像克隆）
nfvis# set serial console enable                 # 默认已启用
nfvis# set autostart true
nfvis# set description 边界防火墙
nfvis# top
nfvis# commit
```

**② 生命周期与串口**：

```bash
nfvis$ request virtual-machine-functions fw-vm start
nfvis$ show virtual-machine-functions fw-vm            # state running
nfvis$ show virtual-machine-functions fw-vm detail     # 域 XML 摘要/资源分配
nfvis$ show virtual-machine-functions fw-vm interfaces # vNIC：类型/MAC/socket/所属交换机
nfvis$ show virtual-machine-functions fw-vm statistics # vhost-user 口计数
nfvis$ request virtual-machine-functions fw-vm console  # 串口接管；Ctrl-] 退出（需真实 TTY）
nfvis$ request virtual-machine-functions fw-vm stop
nfvis$ request virtual-machine-functions fw-vm restart
nfvis$ request virtual-machine-functions fw-vm delete   # super-user；交互确认；级联清理 vNIC/快照
```

> guest 内要有 getty 监听串口（云镜像一般自带 `console=ttyS0`）才能在 console 里看到登录提示。

**③ cloud-init 注意事项**：

- seed 以 **virtio 磁盘**交付（`cloud-localds` 生成、卷标 `cidata` 的 ISO 镜像文件，挂成 `vdb`）。
  能否被消费取决于 **guest 的 cloud-init 怎么挂它**：cloud-init 只接受 `TYPE` 为 `vfat`/`iso9660`
  且带 `cidata` 卷标的设备，并用 `mount -o ro -t auto` 挂载。用 **util-linux 的 `mount`**（Debian/Ubuntu
  一类）时 `auto` 由 libblkid 判类型，**任何云镜像都能用**；而用 **busybox 的 `mount`**（Alpine 一类）
  时 `auto` 只认**内核已加载**的文件系统类型——cloud-init 跑的时刻只有根盘驱动（通常 ext4）在，
  `isofs`/`vfat` 都还是未加载模块 → 挂载失败 → cloud-init 回落 `DataSourceNone`：
  **user-data 被丢弃、hostname 与 SSH 公钥都不生效，且全程无报错**（属"静默失效"）。
  实测：`debian-12-generic-amd64`（完整内核）与 `debian-12-genericcloud`（缺 `ahci`，virtio 交付后可用）
  **可用**；**`alpine.qcow2` 不可用**——不是缺 `ahci`（它的 6.6.31-0-virt 内核 `ahci` 是 builtin），
  而是上面这条 busybox `mount` 的限制，**产品侧无法绕过**（改镜像：装 util-linux 或把 `isofs`
  加进 `/etc/modules`）。排查方法见 §11「cloud-init 未生效」一行；
- guest 内网卡名由 guest 的命名策略决定（Debian 用可预测名如 `enp1s0`），**不是**产品模型里的
  `eth0`（那只是 VPP 侧的逻辑名）——在 user-data 里配网卡前先在 guest 里 `ip -o link` 确认；
- **user-data 的两种形式**（`set cloud-init user-data`）：`#cloud-config` 开头的 YAML，或以
  `#!` 开头的脚本（如 `#!/bin/sh`）；**给脚本必须带 shebang**——cloud-init 对脚本段是直接
  exec，缺 shebang 会在 guest 里失败（`Exec format error`）。两种都不是的内容会被拒绝，
  不会静默丢弃；
- **多行内容建议写成文件再给路径**（`set cloud-init user-data /data/incoming/ud.yaml`）：
  CLI 的引号内不解析多行、`|` 会被当作 CLI 管道，交互里粘多行容易出错；
- **改 user-data 后需重建 VM 或改 `cloud-init hostname`**：cloud-init 按 instance-id
  判定「是否首次」，产品的 instance-id 含输入摘要（改了就重放），但同一输入重启不重放；
- 验证注入是否生效：串口里看 `Cloud-init ... finished ... Datasource DataSourceNoCloud`，
  以及自己脚本往 `/dev/ttyS0` 打的标记行。

### 9.3 VM 快照（create/rollback 需关机态）

```bash
nfvis$ request virtual-machine-functions fw-vm stop
nfvis$ request virtual-machine-functions fw-vm snapshot create name snap-before-upgrade
nfvis$ show virtual-machine-functions fw-vm snapshots
nfvis$ request virtual-machine-functions fw-vm snapshot rollback name snap-before-upgrade
nfvis$ request virtual-machine-functions fw-vm snapshot delete name snap-before-upgrade
```

> 为什么必须关机：实测对**运行中**域回滚，libvirt 会**替换 QEMU 进程**（相当于静默重启该 VM）。
> 产品因此在 API/CLI 层显式拒绝并提示先关机（运行中返回 `409`）。回滚是**磁盘内容级**的：
> 重启后 guest 读到的只能是快照点内容。

### 9.4 容器 VNF

```bash
nfvis$ configure
nfvis# edit container-functions ct-1
nfvis# set image alpine:3.20
nfvis# set vcpu count 1
nfvis# set memory size-mb 128
nfvis# set interfaces eth0 type memif virtual-switch vs-dmz
nfvis# set env TZ Asia/Shanghai
nfvis# set command /bin/sh
nfvis# set args -c "sleep 3600"
nfvis# set restart-policy on-failure
nfvis# set autostart true
nfvis# top
nfvis# commit
nfvis$ request container-functions ct-1 start
nfvis$ request container-functions ct-1 log last 50
nfvis$ request container-functions ct-1 stop
nfvis$ request container-functions ct-1 delete
```

---

## 10. 日常运维

### 10.1 状态查看

```bash
nfvis$ show version                    # nfvis/ubuntu/vpp/dpdk/libvirt/qemu/docker 版本汇总
nfvis$ show system uptime              # 运行时长
nfvis$ show system cpu                 # 核数与占用
nfvis$ show system memory              # 内存与大页使用
nfvis$ show system storage             # 磁盘与镜像仓库占用
nfvis$ show system hugepages           # 大页内核参数与池状态
nfvis$ show system kernel              # 内核基线三方对照
nfvis$ show system hardware            # 硬件健康（温度/SMART；无 BMC 时为降级路径）
nfvis$ show interfaces physical        # 物理口（驱动/链路/速率/VF）
nfvis$ show interfaces ens224 statistics
nfvis$ show interfaces management      # 管理口（内核侧 IP/链路）
nfvis$ show virtual-switches
nfvis$ show vrfs vs-wan routes
nfvis$ show bonds bond0 detail
nfvis$ show virtual-machine-functions
nfvis$ show container-functions
nfvis$ show resource-pools
nfvis$ show images
nfvis$ show configuration
nfvis$ show users
```

### 10.2 连通性测试

```bash
nfvis$ ping 192.168.1.1 count 4              # **仅 VPP 数据面**（经 VPP 路由/接口）
nfvis$ ping 192.168.1.1 source 192.168.1.2   # source 须为 **VPP 接口**地址
nfvis$ ping 10.0.0.1 vrf vs-wan
nfvis$ traceroute 192.168.1.1                # 宿主侧 ICMP（不支持 vrf，会明确报错）
```

> **ping 未通即失败**：一个包都没发出去（VPP 无到达目标的接口/路由）或发出了但无应答
> （`100% packet loss`）都返回**错误**，并点明平面归属——管理口属内核平面，VPP 看不到它，
> `ping <管理口网关>` 必然失败，请改用宿主 `ping` 或 `traceroute`。

### 10.3 实时监控与统计清零

```bash
nfvis$ monitor interfaces ens224 interval 2   # 实时刷新计数，Ctrl-C 退出
nfvis$ monitor vnf fw-vm                      # 跟踪 VM 状态/事件，Ctrl-C 退出
nfvis$ clear interfaces statistics ens224     # 清零（super-user）
```

### 10.4 抓包（VPP pcap trace）

```bash
nfvis$ request vpp trace start interface ens224 count 100     # 达到报文数自动停止
nfvis$ show vpp capture                                        # 会话状态 + 已导出 pcap 清单
nfvis$ request vpp trace export name my-capture                # 导出（隐含停止）
# 下载：GET /api/v1/vpp/capture/<file>
nfvis$ request vpp trace stop                                  # 停止且不导出
```

### 10.5 日志与审计

```bash
nfvis$ show log system last 100               # 系统日志（level 可过滤 debug|info|warn|error）
nfvis$ show log audit last 20                 # 审计：登录/配置/运维动作（谁、何时、做了什么）
nfvis$ show log vnf fw-vm last 50             # VNF 控制台/事件日志
```

- 审计含「时钟是否已同步」标记：NTP 未同步的记录带 `[时钟未同步]`；
- **高危动作记两条**：恢复出厂、软件升级/回退、恢复配置、证书上传，以及本地用户与口令策略
  的写操作，会先记一条**意图**（Result 列显示 `intent`，说明「要做什么」、含关键参数），
  动作结束后再记一条**结果**（`success`/`failure`，失败带原因）。两条的 Action 相同，
  据此配对——**只看 `intent` 那条而无结果，说明该动作没走完**。其余动作仍只有一条记录；
  审计里**不出现**口令、口令哈希与证书/私钥正文；
- 日志保留策略：`set system syslog local retention-days`（天数）与 `max-size-mb`（滚动覆盖）；
- 远程 syslog：`set system syslog host <ip> [port] [facility] [severity]`。

### 10.6 告警

```bash
nfvis$ show alarms active                     # 活跃告警（底座缺失、配置未收敛、vpp 待重启、健康越限…）
nfvis$ show alarms all
nfvis$ request alarms clear id <id>           # 清除已 resolved 的告警
nfvis$ request alarms clear all
```

### 10.7 备份 / 恢复 / 恢复出厂

```bash
# 生成归档（自动命名，落 /var/lib/nfvis/backup/，0600）
nfvis$ request system configuration backup
# 生成并额外导出到指定路径（同样 0600；导出件含口令哈希）
nfvis$ request system configuration backup to /var/lib/nfvis/backup/pre-change.json

nfvis$ request system configuration restore /var/lib/nfvis/backup/pre-change.json
# 恢复出厂（**双重确认**，不可逆；清配置库与数据，重启后重新引导 admin）：
nfvis$ request system zeroize
```

> **`to <path>` 是「另存一份」**——导出不会改写既有文件。因此该命令要求：
> 路径必须是**绝对路径**（相对路径会随工作目录漂移，故一律拒绝）、父目录**必须已存在**、
> 目标**必须不存在**——目标已存在时如实报错（不覆盖），请换一个文件名（例如带时间戳）
> 或先自行删除它。这样一次手误（把路径指到系统文件上）不会毁掉系统文件。
>
> **归档含账号信息（`password_hash`）**，所有导出件均 0600、仅 super-user 可读；
> 经 REST 下载同一归档（`GET /api/v1/system/backup/<文件>`）同样只允许 super-user
> （归档要能恢复就不能脱敏，故收紧的是权限）。**归档列表**（`GET /api/v1/system/backup`）只回
> 文件名/大小/时间，read-only 账号也能看，但看得到不等于拿得走。
> 请勿放到 /tmp 等共享目录。**动包管理（升级/purge）前也建议先做一份备份。**

### 10.8 诊断与转储

```bash
nfvis$ request system tech-support generate   # 生成 tar.gz（日志+版本+配置+状态）
nfvis$ show system tech-support               # 清单；下载 GET /api/v1/system/tech-support/<file>
nfvis$ show system core-dumps                 # 崩溃转储清单（VPP/QEMU/nfvisd）
nfvis$ request system core-dumps export https://<server>/upload
nfvis$ request system core-dumps delete file <name>
```

> **诊断包里的配置是脱敏视图**：`config.json` 里的 `password_hash` 等敏感字段已被隐藏
> （与页面上看配置同一口径），`logs.txt` 里首次启动打印的一次性口令也已打码——诊断包是
> **要交给支持人员**的件，故任何账号都能下载（含 read-only）。因此它**不能**用来恢复配置：
> 需要可恢复的完整配置请用配置备份导出（见 §10.7，仅 super-user）。
> 归档落盘在 `/var/lib/nfvis/tech-support/`（文件 0600、目录 0700），同机其他用户读不到。

core dump 目录：`/var/lib/nfvis/coredumps`（容量上限 + 滚动清理）。
清单为空时按安装期提示设置 `core_pattern`：

```bash
echo '/var/lib/nfvis/coredumps/core.%e.%p.%t' > /proc/sys/kernel/core_pattern
```

### 10.9 证书、密钥与时间

```bash
nfvis$ request system api tls regenerate       # 重签自签证书（SAN 按本机地址自动生成，CLI 零参数继续可用）
nfvis$ request system ssh host-key regenerate  # 重新生成 SSH host key
nfvis$ request system ntp sync                 # 手动触发一次 NTP 同步
```

外部证书：`set system api tls cert-file <p> key-file <p>` 后 commit（立即生效）。

### 10.10 监控与自动化接入

| 端点 | 鉴权 | 用途 |
|---|---|---|
| `GET /api/v1/metrics` | 无 | Prometheus 指标（系统/资源池/VPP 等；向导与监控都从这里取事实） |
| `GET /api/v1/events` | Bearer | SSE 事件流（配置提交、VM 状态变化、告警等） |
| `GET /api/v1/alarms` | Bearer | 告警列表 |
| `GET /api/v1/audit-logs` | Bearer | 审计日志（`limit`/`offset` 分页） |
| `GET /api/v1/openapi.json` | 无 | 运行时 OpenAPI 规范 |

列表类端点支持分页（`limit`/`offset`）。CLI 脚本模式（§3.3）+ `| display json` 适合轻量自动化；
大规模对接建议直接走 REST + events。

### 10.11 软件升级、重启与关机

```bash
nfvis$ request system software add /data/incoming/nfvis_1.1.19_amd64.deb sha256 <hex>
nfvis$ request system software rollback [to <version>]
nfvis$ request system reboot
nfvis$ request system shutdown
```

- `software add` 先校验 sha256（可选但强烈建议）→ 安装 → 重启 nfvisd → 报告；
- 升级期间配置库不受影响；`purge` 才会清运行态（配置数据仍保留，见 §2.6）。

### 10.12 Web 控制台（导航 / 配置 / 运维）

浏览器打开 **`https://<管理地址>/api/v1/ui/`**（少打末尾斜杠会自动补上）。界面随 nfvisd 一起发布，
**不需要额外部署任何东西**，也不连外网。

**怎么找东西（导航）**：顶栏按资源域分组，点一下就切页——总览 / 计算（虚拟机、容器、镜像）/
网络（网络对象、虚拟交换机、LLDP）/ 系统（系统、硬件健康、资源池、接口、内核基线、用户与权限、证书）/
配置（配置、提交历史、编辑锁会话）/ 运维（动作、审计、诊断、抓包）。页面右上角有面包屑（如「计算 › 虚拟机」）；
再往右是实时通道状态、当前登录账号与「我的账号」（改自己的口令，见下）。

**点对象看详情**：列表里的行可以直接点进**详情页**（虚拟机、容器、镜像、网络对象、虚拟交换机、接口
各有独立页面），地址形如 `#/compute/vms/vnf-a`——可分享、可刷新保持。虚拟机详情页分「概览 / 接口 /
快照 / 串口」四个页签，容器详情页分「概览 / 日志」；**离开详情页会自动断开串口连接**。
面包屑末段可点，回到对应列表页。接口详情页上放着**驱动接管（DPDK 绑定/解绑）**与 **SR-IOV 的 VF 数量**。

**地址就是页面**：地址栏里的 `#/…` 段就是当前页（`#/compute/vms`、`#/ops/audit`……），
可以直接**分享、收藏、刷新保持**，浏览器前进/后退也能用；打进一个不存在的地址会提示并回到总览。

**每页刷新各按自己的节奏**：总览/计算/网络/系统等页有实时数据，约每 5 秒兜底刷新一次、
有事件推送时立即刷新；**配置页、运维动作页、内核基线页、用户与权限页、证书页不自动刷新**
（免得你正在填表时被刷掉）。每页只请求它自己需要的数据，不再整页全量刷新。

**破坏性动作都在专门页面上，并按影响面分级确认**——总览页不放破坏性动作：

| 动作 | 在哪一页 | 确认档位 |
|---|---|---|
| 单台虚拟机/容器启停重启、清接口计数、清告警、删快照、取消抓包、生成备份/诊断归档、立即同步时间 | 各详情页 / 运维动作页 | 低危：点一次确认 |
| 重启数据面、重启主机、关机、重签自签证书、重生成 SSH host key、**内核基线写入/回退**、**DPDK 绑定/解绑**、**SR-IOV VF 数量**、删除 QoS 策略/端口镜像会话 | 运维动作页 / 系统域各页 | 中危：逐条列出影响面 + 主按钮标红 |
| **软件升级/回退、从备份恢复配置、恢复出厂、安装外部证书、创建/删除用户、改口令与权限类** | 运维动作页 / 证书页 / 用户与权限页 | 高危：还要手工输入确认词 + 等 10 秒倒计时走完 |

每个确认框里都只读回显这条动作对应的命令行语句（便于工单对照，不需要你输入语句）。
**改自己的口令**：顶栏「我的账号」对**所有账号可见**（只读账号也能用——改口令是"本人 + 验旧口令"的自助动作）：
填当前口令与新口令、点「修改口令」，再按上面的高危档确认（确认词＝你的用户名，10 秒倒计时），
改完用新口令重新登录（**不会自动登出**，框随时可取消）。「用户与权限」页上那条改口令表单仍然保留。
**界面不放松任何服务端守卫**：管理口不得交给数据面、候选配置的编辑锁、口令策略、
「管理口变更必须 commit confirmed」这些都在服务端实现里判定——界面被拒绝时会把服务端原话显示出来，
不会显示成"成功"。

**能看什么**：总览页给系统与组件版本、数据面状态、未解决告警与最近事件；计算/网络/系统各页给
虚拟机与容器（含 vhost-user 口计数）、镜像仓库、虚拟交换机（含成员口计数与 MAC 学习表）、网络对象
（VRF/ACL/NAT/聚合/QoS/端口镜像）、LLDP 开关与邻居、资源池、接口（链路/速率/驱动）、硬件健康与阈值、
内核基线（运行实际 / 配置期望 / 差异三方对照）、用户与权限、证书信息。
组件版本取自本机探测：Ubuntu/VPP/libvirt/QEMU/Docker 探测不到时显示「—」，
DPDK 没有独立版本来源（随 VPP 一起编译），同样显示「—」。

**能改什么（「配置」页）**：页面上先显示当前生效配置的版本号与内容预览，点「开始编辑」会以当前生效配置为
底稿建立一个候选配置——**它与 CLI 的 candidate 是同一把锁**，因此同一时刻只能有一个会话在编辑
（谁持有可在「编辑锁会话」页看，或用 CLI `show system configuration sessions`，见 §11.1）。之后的顺序是：

1. 在表单里改（系统 / 接口 / 资源池 / 数据面 / 网络域 / 计算域各节），或直接改原始 JSON；
2. 「保存到 candidate」把改动写到服务端候选（只写候选，尚未生效）；
3. 「查看差异」逐项列出候选与生效配置的差别；
4. 「预校验」用与 `commit check` 相同的校验器查一遍（**不落库、不下发**），有问题会逐条列出；
5. 「提交」使其生效；不想要就「丢弃并结束编辑」放弃全部改动。

两条与 CLI 完全一致的约束：① **管理口（地址/网关）的变更必须用「以 commit confirmed 提交」**——
提交后页面会出现确认按钮（按钮上写着确认时限），在时限内点「确认在途提交」才最终生效，
**不确认会自动回滚**（这是防止把自己锁在门外）；② **提交或丢弃都会释放编辑锁**，
之后 CLI 才能正常进入配置模式。

**历史与回滚（「提交历史」页）**：列出保留的每一版（时间/用户/注释），**回滚是两段式**——
先把某一版取为候选，候选不会自己生效，要到「配置」页点「提交」才改变运行配置。

**用户与权限（「用户与权限」页）**：改权限类、重置口令、新建与删除用户、修改自己的口令。
这些写操作走的是与命令行同一套实现：**取用配置编辑锁、写入候选、立即提交生效**（不是「候选 → 稍后提交」
两段式），提交生效后**立刻交还编辑锁**（不会占着锁等你登出，别的会话可立即写配置），并都记入审计。
两条注意事项：① 若你正在「配置」页编辑候选（有未提交改动），本页的写操作会
**先被拒绝**——避免把你的候选改动一并提交，请先在配置页提交或丢弃；② 不能删当前登录用户，
也不能删最后一个 super-user（服务端拒绝，界面照实显示）。
③ 「配置」页的**整文档提交同样不能把账号表清空**：文档里一个 super-user 都不剩时提交被拒
（与 CLI 同一条约束，见 §4.2），补一个 super-user 再提交即可。

**证书（「证书」页）**：查看当前管理面证书（主体/签发者/有效期/指纹），安装外部证书（粘贴 PEM 文本，
热生效），重签自签证书。安装证书是高危动作：换成你手里的客户端**不信任**的证书会让管理面连不上，
请先确认证书链与地址条目能被你要用的浏览器/命令行信任，并保留带外或控制台通道。

**内核基线（「内核基线」页）**：三方对照（运行实际 / 配置期望 / 差异），以及「写入基线」「回退上一次基线」。
两条都要**重启主机才生效**，本页不重启主机——重启入口在「运维动作」页。大页与隔离核的唯一真源是
「资源池」配置，写入前请先核对本页的「配置期望」。

**恢复配置与恢复出厂（「运维动作」页）**：恢复配置从**本机已有的备份归档**里选一个恢复（归档列表在
同页「归档下载」），恢复会把归档里的配置导入为候选并立即提交生效；归档不含镜像文件本体，配置引用了
镜像时镜像要另行导入。恢复出厂会清空数据分区的配置与镜像、级联删除虚拟机与容器、账号复位，且
**本机不会自动重启**——重启后进入初始化状态，需要带外/控制台重新初始化，本机没有「撤销恢复出厂」。
两个动作都是高危档，审计里各留「意图 + 结果」两条（§10.5）。

**诊断与抓包**：经数据面执行 `ping` / `traceroute`（**未通即判失败**，失败时把底层原始回显一并显示）、
按需清零接口计数、按需拉取服务端日志（末 100 行）、数据面抓包（开始/停止/停止并导出 pcap，导出件可下载）。

**界面上还没有的**：命令行的交互式编排（`wizard` 向导、`start shell`、`monitor` 的实时跟踪）、
`load merge` 式的配置合并语义（界面在「配置」页用原始 JSON 整体替换候选）、以及数据分区格式化
（明确延期）。这些仍在 CLI（§4）里做。

- **登录**：用与 CLI 相同的用户名/口令。凭据只用于换取访问令牌，令牌存在浏览器会话里，
  **关闭标签页即失效**；口令不在浏览器里保存。
- **自签证书**：缺省是自签证书，浏览器会提示不受信任——按 §3.4 的说明把
  `/var/lib/nfvis/tls/server.crt` 导入为受信任证书，或换成正式证书（§10.9）。
- **取不到数据时**：先按 §11.1 确认服务在跑、地址与端口正确；页面右上角的
  「实时通道」会显示是否连着事件推送（连不上会自动退化为轮询，功能不受影响）。

---

## 11. 故障排查

### 11.1 安装与登录

| 现象 | 原因 | 处理 |
|---|---|---|
| `make deb` 报 `make: command not found` | 全新 Ubuntu 不带 make | `apt-get install -y make` |
| CLI 报 `连接 nfvisd 失败` / 超时 | ① nfvisd 未启动；② 监听地址非缺省 | `systemctl status nfvis`；显式 `-server` 或 `NFVIS_SERVER` |
| CLI 报 `x509: …` 证书错误 | 自签证书轮换后本地固定失效 | 重签后重启 CLI 即重新固定；或 `-ca /var/lib/nfvis/tls/server.crt` |
| 一次性口令丢了 | 只打印一次 | 见 §3.2（勿直接删库；用 `-init-admin-password` 或其他 super-user 重置） |
| `candidate 会话锁被占用: 由 … 持有` | 另一会话正在编辑候选（或持锁未释放） | 让其 commit/discard；或 `show system configuration sessions` 看谁持锁，等空闲超时。「提交即生效」的写（用户与权限、直提写）提交后会自动放锁，不占锁 |

### 11.2 内核基线与数据面

| 现象 | 原因 | 处理 |
|---|---|---|
| VM 起不来报 `Cannot allocate memory` | 1G 大页不足（被 VPP 或其他 VM 占用） | `show system hugepages` 看空闲；按 §6.5 规划（VPP 用 2M 把 1G 让给 VM） |
| `request system kernel apply` 报「隔离核 … 至少需保留 2 个」 | 护栏：隔离核把宿主挤满 | 缩小隔离核范围（报错可直接照做） |
| `show system kernel` 报「期望 N 实际 M」 | 配置改了但没重启 | `request system reboot` 后复核 |
| `request interfaces <口> bind-dpdk` 报 vfio-pci 不可用 | 自动加载模块失败（内核/模块缺失）或 IOMMU 未生效 | 看报错里的加载失败原因；无 IOMMU 时 `enable_unsafe_noiommu_mode`（§7.2） |
| 同上但报「拒绝操作管理口」 | 该口被判为管理路径（配置声明/默认路由/监听口）——有意拒绝 | 换业务口；确需变更用带外方式 B |
| commit 报 `接口在 VPP 中不存在: ensX（若该口由 DPDK 接管…）` | 该口尚未进数据面（刚声明/刚接管的过渡态），或未绑 vfio、或口名不对 | `request vpp restart`；仍失败按 §7.2 核对（`ip link` 里没有 = 已被接管） |
| 日志报 `以下已由 DPDK 接管的物理口未在配置中声明…` | 该口没在 committed 里声明为 DPDK 口 | 补 `set vpp dpdk dev <口>`（并确认 `set interfaces <口>`）再重启 |
| `request vpp restart` 报「解析 … 的 PCI 地址失败」 | 先声明后绑定，映射记录缺失 | 先 `bind-dpdk` 再 commit 声明 |
| `request interfaces <口> unbind-dpdk` 长时间不返回 / 报「请求超时（1m30s）」 | 该口正被数据面使用（VPP 持有 vfio group）→ 内核解绑写阻塞；CLI 通道可能被占住 | 先移出数据面（删声明 → `request vpp restart`）再解绑。已卡住：`systemctl stop vpp` → `systemctl restart nfvis` → `request vpp restart` |
| VPP 重启后网络不通 | startup.conf 由 committed 生成，配置不含该口 | 检查 `show configuration`，补声明后 `request vpp restart` 重放 |
| 配了 cross-connect 后网络风暴/整机失联 | 直通两端同处一个广播域 → 物理二层环路 | 两端必须不同广播域；先断开其一再改配置 |

### 11.3 资源与负载

| 现象 | 原因 | 处理 |
|---|---|---|
| commit 报 `无 1G 大页资源池，无法分配 …MB` | 资源池未配或内核大页未生效 | §6 配池并重启生效 |
| commit 报 `隔离核不足：需要 N，可用 0` | VPP 保留核占满隔离核池 | 扩大 `isolated-cores`（`show resource-pools` 看 `vpp-reserved`） |
| VM 有 console 无输出 | guest 未开串口 getty | 云镜像确认 `console=ttyS0`；见 §9.2 |
| VM 引导慢 / cloud-init 未生效 | guest 的 cloud-init 挂不上 seed（§9.2③：只认 `vfat`/`iso9660` + 用 `-t auto` 挂；busybox 的 `mount` 只认已加载的文件系统类型）→ 回落 `DataSourceNone`、**无报错** | 换用 util-linux `mount` 的云镜像（§9.2③ 实测清单）；在 guest 内 `grep -i "Failed to mount\|Datasource" /var/log/cloud-init.log`、`ls /var/lib/cloud/instances/` 定位 |
| user-data 里 `ip addr add … dev eth0` 报 `Cannot find device` | guest 网卡名不是 eth0（可预测网名） | guest 内 `ip -o link` 确认实际名；脚本按非 lo 网卡遍历（§9.2③） |
| 容器下发报 `docker: not found` | 镜像名与 Docker tag 不一致 | 上传时 `name` 用 Docker tag（§9.1） |
| `show lldp neighbors` 为空 | 无 LLDP 对端 | 正常；对端启用后可见 |
| `request sriov …` 报「不支持 SR-IOV」 | 网卡无 SR-IOV 能力 | 换支持的网卡 |
| `show vpp runtime` 报「未接入」 | V1 未实现（解码受限） | 已知限制，非故障 |
| `show system hardware` 值为空 | 无 BMC/传感器 | 降级路径，非故障 |

### 11.4 取日志

```bash
journalctl -u nfvis -n 200 --no-pager
journalctl -u nfvis | grep 管理口守卫        # 看产品认哪个口是管理口
nfvis-cli -c "show log system last 100"
nfvis-cli -c "show log audit last 50"
tail -n 100 /var/log/nfvis-provision.log     # 环境初始化（若用过 provision.sh）
```

---

## 12. 附录

### 12.1 文件与目录

| 路径 | 内容 |
|---|---|
| `/usr/bin/nfvisd`、`/usr/bin/nfvis-cli` | 可执行文件 |
| `/etc/systemd/system/nfvis.service` | systemd 单元（环境变量 `NFVIS_DB`/`NFVIS_LISTEN`/`NFVIS_VPP_SOCK`） |
| `/var/lib/nfvis/nfvis.db` | 配置库（SQLite，committed 与历史快照；升级/purge 不动它） |
| `/var/lib/nfvis/images/` | 镜像仓库（qcow2 / 容器 tar），`index.json` 为元数据 |
| `/var/lib/nfvis/backup/` | 配置备份归档（0600） |
| `/var/lib/nfvis/captures/` | 导出的 pcap |
| `/var/lib/nfvis/coredumps/` | 崩溃转储 |
| `/var/lib/nfvis/tech-support/` | 诊断归档 tar.gz（0600，目录 0700） |
| `/var/lib/nfvis/vms/` | VM 磁盘与定义（seed.iso/user-data/meta-data 在各 VM 子目录） |
| `/var/lib/nfvis/tls/` | `server.crt`/`server.key`（自签；CLI 默认固定此证书） |
| `/var/lib/nfvis/kernel-baseline.bak` | 内核基线上一次片段备份 |
| `/var/lib/nfvis/dpdk-bindings.json` | 「口名 → PCI」绑定记录（生成 startup.conf 用） |
| `/etc/default/grub.d/99-nfvis.cfg` | 内核基线 GRUB 片段 |
| `/etc/ssh/sshd_config.d/99-nfvis.conf` | SSH 强化 drop-in（purge 时自动撤销） |
| `/etc/vpp/startup.conf` | VPP 启动配置（**由 nfvisd 生成，勿手改**） |
| `/data/incoming/` | 镜像导入暂存（`request images upload` 只认这里） |
| `/run/nfvis/vhost`、`/run/nfvis/memif` | VPP 与 QEMU/容器共享的 socket（0777） |

### 12.2 端口与服务

| 端口/套接字 | 归属 | 说明 |
|---|---|---|
| `TCP 443` | nfvisd | REST API + WebSocket console（默认 HTTPS；监听可按管理口地址收敛） |
| `GET /api/v1/metrics`、`/api/v1/openapi.json` | nfvisd | 无鉴权端点 |
| `/run/vpp/api.sock` | VPP | binary API |
| `/run/vpp/cli.sock` | VPP | `vppctl`（ping/trace 经此执行） |
| `/var/run/docker.sock` | Docker | 容器编排 |
| `libvirt`（qemu:///system） | libvirtd | VM 编排 |

### 12.3 命令速查

完整命令表（逐条含实测状态与 API 落点）见 **[`NFViS-CLI命令全表.md`](NFViS-CLI命令全表.md)**。
高频十条：

```bash
wizard                                   # 初始化（§5）
show system kernel                       # 基线三方对照（§6）
request system kernel apply              # 写基线（§6）
request interfaces <口> bind-dpdk --yes  # 业务口接管（§7）
request vpp restart                      # 数据面按配置重启（§7）
show interfaces physical                 # 数据面端口（§7）
request virtual-machine-functions <n> start | console | stop   # VM 生命周期（§9）
request system configuration backup      # 备份（§10）
show alarms active                       # 告警（§10）
show log audit last 20                   # 审计（§10）
```

### 12.4 已知限制（使用前请阅读）

完整清单见 [`NFViS-CLI命令全表.md`](NFViS-CLI命令全表.md)。高频几条：

| 限制 | 说明 |
|---|---|
| 容器镜像目录名须等于 Docker tag | §9.1 |
| 快照 create/rollback 需关机态 | §9.3（运行中回滚会静默重启 VM） |
| `show \| display set` 未实现 | 用 `save`（JSON）/`show configuration`/`\| display json` 替代 |
| `show vpp runtime` 未接入 | govpp runtime 解码受限，CLI 明确提示 |
| `?` 不能作为取值字面量 | 它是即时帮助键（§3.3） |
| 硬件健康在无 BMC/传感器环境为降级路径 | 值可能为空，非故障 |
| SR-IOV / LLDP 邻居需对应硬件与对端 | 无 PF/VF、无对端时无法演示 |
| 管理口 IP 运行期不热改 | 声明后下次启动收敛监听；变更须 commit confirmed |
| guest 读 seed 取决于其 cloud-init 的挂载方式 | busybox `mount` 的镜像（Alpine 类）挂不上 seed → user-data 静默失效（§9.2③ 实测清单） |

### 12.5 参考文档

| 文档 | 用途 |
|---|---|
| `NFViS-CLI命令全表.md` | 命令速查 + 实测状态 |
| `NFViS-CLI命令树完整设计.md` | CLI 契约（语义细节以它为准） |
| `NFViS-openapi.yaml` | REST 契约 |
| `NFViS-系统产品需求与目标架构规格书.md` | 需求真源（附录 A 为决策记录） |
| `NFViS-Go工程目录骨架设计.md` | 代码结构与依赖方向 |
| `docs/evidence/` | 各轮真机证据原始输出 |
