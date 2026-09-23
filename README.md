# NFViS

[![CI](https://github.com/xzjt/nfvis/actions/workflows/ci.yml/badge.svg)](https://github.com/xzjt/nfvis/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-%E2%89%A5%201.26-00ADD8.svg)](go.mod)

**网络功能虚拟化基础设施（NFVi）一体机软件**：基于 Ubuntu 26.04 + VPP 26.06 + KVM/libvirt + Docker，
向前端提供 **JunOS 风格 CLI**（`nfvis-cli`）与 **REST API**（OpenAPI 契约），
用于在一台服务器上编排 L2/L3 网络、虚拟机 VNF 与容器 VNF。

> **当前状态：V1（首个发布版）**
> 规格书 **109 条 FR**：**通过 101 / 未验 4 / 降级 2 / 移 V2 2**（口径与逐条证据见
> [`docs/V1-验收检查表.md`](docs/V1-验收检查表.md)）；已定**决策 124 项**（规格书附录 A）；
> `make check` 全绿。**已知限制请先读** [`docs/NFViS-CLI命令全表.md`](docs/NFViS-CLI命令全表.md) §4。
>
> **发布的二进制**见 [Releases](https://github.com/xzjt/nfvis/releases)（最新 **v1.1.26**：Web 控制面
> 增量 1~3 第一刀——只读总览 / 配置读写 / 诊断视图，内嵌同源托管、前端免构建）；
> 命令全表把 256 条 CLI 命令**逐条真机执行**并标注状态（现全部 ✅／⊘／🚫，无「不可用」项）。

---

## 它能做什么

| 能力域 | 内容 |
|---|---|
| **网络编排** | L2 虚拟交换机（VPP bridge-domain）+ BVI 网关；L3 虚拟交换机（VRF）+ 静态路由（v4/v6）；ACL；NAT44（跨 VRF）；QoS 限速（policer）；端口镜像（SPAN）；链路聚合（bond + LACP）；LLDP |
| **计算编排** | VM VNF：vhost-user / SR-IOV vNIC、cloud-init（NoCloud seed ISO）、附加 virtio 数据盘、串口 console、快照、异常退出告警 |
| **容器编排** | Docker 容器 VNF：cgroup CPU/内存限制、memif vNIC、env/command/args、重启策略 |
| **镜像仓库** | 本地导入（`/data/incoming`）与 URL 拉取（**强制 sha256**）、引用计数、级联清理 |
| **资源管理** | 大页池（2M/1G）与隔离核的**统一账本**（VPP 保留核优先扣减），VNF 按账本分配；内核启动基线（GRUB）托管 |
| **配置事务** | candidate → `commit`（校验 + 下发 + **失败自动补偿**）→ committed；历史快照 `rollback`；`commit confirmed` 自锁保护；注释（`annotate`）；`save`/`load` JSON 往返 |
| **运维** | 事件总线 `/events`（SSE）、Prometheus `/metrics`、告警、审计日志、日志（本地保留策略 + 远程 syslog）、tech-support 归档、core dump 收集、VPP pcap 抓包导出、备份/恢复/zeroize、软件升级/回退、TLS 热换证 |
| **安全** | 本地用户 / class 权限矩阵 / 口令策略（PBKDF2）；Bearer Token；**默认 HTTPS**（自动自签 + 客户端证书固定）；管理口与数据面隔离强制；口令哈希全链路脱敏 |
| **Web 控制台** | `GET /api/v1/ui/`（内嵌进 nfvisd **同源托管**、前端**免构建**）：只读总览、配置读写（candidate → 差异 → 预校验 → 提交/丢弃）、诊断（日志 / ping / traceroute / 清零统计） |

**交互示例**（配置事务 + commit 校验）：

```
nfvis> configure
nfvis# edit virtual-switches vs-dmz
nfvis# set type l2
nfvis# set ports 1 interface ens224
nfvis# set gateway ip 192.168.100.1/24
nfvis# top
nfvis# commit
校验失败（candidate 保留）:
  - virtual-switches[vs-dmz].gateway.ip: 地址与 l3-interface 冲突（FR-CFG-011）
```

---

## 快速开始

### 安装（deb）

```bash
git clone https://github.com/xzjt/nfvis.git && cd nfvis
make deb VERSION=1.0.0                  # 需在 Linux 上执行（依赖 dpkg-deb）
sudo dpkg -i build/nfvis_1.0.0_amd64.deb
sudo systemctl start nfvis
```

### 首次登录

```bash
# 一次性 admin 口令仅打印一次
journalctl -u nfvis --since "10 min ago" | grep 一次性口令

nfvis-cli                               # 缺省即 https://127.0.0.1:443（零参数；自签证书自动固定）
nfvis> show version
```

### 底座准备（**装完能用的前提**）

```bash
# 1) 内核基线：大页 + 隔离核（写 GRUB，需重启生效）
sudo /usr/share/nfvis/installer/nfvis-baseline.sh --defaults && sudo reboot

# 2) 业务网卡交 DPDK（管理口保持内核驱动）
nfvis-cli -c "request interfaces ens224 bind-dpdk --yes"

# 3) 起 VPP 并确认
systemctl start vpp && vppctl show interface
```

> `postinst` 会按机器规格**自动应用内核基线**（写 GRUB 片段 + fstab 并 `update-grub`）——
> 但**需重启生效**。完整流程见 **[用户手册](docs/NFViS-用户手册.md)**
> （安装 → 底座准备 → 首次登录 → 配置任务 → 日常运维 → 故障排查）。

---

## 文档

| 文档 | 用途 |
|---|---|
| **[用户手册](docs/NFViS-用户手册.md)** | **从安装到使用的全流程**（含故障排查、已知限制） |
| **[CLI 命令全表](docs/NFViS-CLI命令全表.md)** | 256 条命令，含权限、API 落点与**逐条真机实测状态** |
| [系统产品需求与目标架构规格书](docs/NFViS-系统产品需求与目标架构规格书.md) | **需求真源**；附录 A = 决策记录（1~124），实现有疑问先查它 |
| [CLI 命令树完整设计](docs/NFViS-CLI命令树完整设计.md) | CLI **契约**（命令树、补全、权限矩阵） |
| [OpenAPI](docs/NFViS-openapi.yaml) | REST **契约**（`openapi.json` 随二进制嵌入，由 CI 守护同步） |
| [Go 工程目录骨架设计](docs/NFViS-Go工程目录骨架设计.md) | 代码结构、依赖方向规则、里程碑 |
| [V1 验收检查表](docs/V1-验收检查表.md) / [收尾待办](docs/V1-收尾待办.md) | 验收口径（109 条 FR 逐条）与剩余待办 |
| [docs/evidence/](docs/evidence/) | 各轮**真机证据原始输出** |

---

## 仓库结构

```
├── cmd/
│   ├── nfvisd/           守护进程（装配 / 信号 / 恢复收敛 / 优雅退出）
│   └── nfvis-cli/        CLI 入口（薄客户端；行编辑/补全/提示符在 internal/cli）
├── internal/
│   ├── model/            配置模型（单一真源）+ 校验 + diff/merge + 资源账本
│   ├── schema/           命令树 schema（补全/缩写/权限；CLI 与 nfvisd 编译期共享）
│   ├── config/           事务引擎（candidate / commit confirmed / rollback + SQLite）
│   ├── orchestrator/     底座适配（govpp / libvirt / Docker），Provider 接口隔离
│   ├── api/              REST server + CLI 执行器 + 契约守护测试
│   ├── aaa/              本地用户 / class / 口令策略 / Token
│   ├── system/           宿主交互（内核基线、TLS、备份、诊断、日志）
│   ├── cli/ events/ metrics/ images/ state/
│   └── archtest/         依赖方向守护（CI 执行）
├── deploy/               systemd 单元、deb 打包脚本、安装器（内核基线）
├── contrib/
│   ├── dev-vm/           开发/验证虚机初始化脚本
│   ├── hooks/            pre-commit 门禁（git config core.hooksPath contrib/hooks）
│   └── scripts/          CI 守护脚本 + CLI 全功能冒烟（cli-fulltest.sh）
├── test/
│   ├── integration/      真机集成（build tag integration，需 VPP/libvirt）
│   └── e2e/              端到端验收（build tag e2e，经 HTTP API）
├── prototype/            CLI 补全薄演示（引用 internal/schema；非产品代码）
└── docs/                 见上表
```

**依赖方向**（CI 强制，`internal/archtest` 守护）：CLI 前端**不得** import 事务引擎/API/编排/AAA；
底座交互（govpp/libvirt/Docker）必须藏在 Provider 接口后，单测用 mock。

---

## 开发

要求 **Go ≥ 1.26**。M1/M2 相关开发可在任意平台（底座 mock）；M3/M4 需 Linux + VPP/libvirt/Docker。

```bash
make check          # 提交前/CI 统一入口：vet + 全量测试 + 覆盖率门槛 + 依赖方向 + 契约守护 + 原型全绿
make integration    # 真机集成（需 VPP socket；缺省自动跳过，CI 不跑）
make e2e            # 端到端验收（需 NFVIS_API 指向已装环境）
make deb            # 打包 deb（需 dpkg-deb）
```

一次性配置：`git config core.hooksPath contrib/hooks`（启用 pre-commit 门禁）。

**CLI 冒烟（手动，不在 CI）**：`contrib/scripts/cli-fulltest.sh` 按契约命令树**逐条真机执行**，
区分「预期报错」与真失败——`docs/NFViS-CLI命令全表.md` 的实测状态即由此产出：

```bash
bash contrib/scripts/cli-fulltest.sh        # 全部阶段；也可 `... 1 5` 只跑指定阶段
```

**质量门禁（四层）**：`AGENTS.md`（AI/开发会话规则）→ pre-commit 钩子 → CI（每次 push/PR）→
每日巡检（ZCode 定时任务，产出进 `docs/reviews/`）。

> 本仓库把「契约」当作**可执行的约束**：`internal/api` 与 `internal/archtest` 里有多组守护测试
> （OpenAPI↔路由、CLI 语句↔模型、CLI 命令覆盖、依赖方向、决策条数、`openapi.json` 同步），
> 契约漂移会直接让 `make check` 失败。

---

## 已知限制

V1 的降级/未验项统一登记在 [`docs/NFViS-CLI命令全表.md`](docs/NFViS-CLI命令全表.md) §4 与
[`docs/V1-验收检查表.md`](docs/V1-验收检查表.md) §5。常见几条：

- **SR-IOV / LLDP 邻居**需对应硬件与对端（验证环境不具备；代码与单测齐备）；
- **快照 create/rollback 需关机态**（对运行中域回滚会静默重启该 VM，故显式拒绝）；
- **容器镜像的目录名须等于 Docker tag**，否则下发报 `docker: not found`；
- `show vpp runtime` 未接入（govpp runtime 解码受限，CLI 明确提示而非静默空值）；
- **Web 控制台尚未覆盖的 CLI 能力**：console 交互终端、`ssh host-key regenerate`、
  `core-dumps export`、`load merge` 与 VS/VM 详情的 statistics 字段（逐项见
  [`docs/CLI-REST覆盖核查.md`](docs/CLI-REST覆盖核查.md)；配置类语句经 candidate API 已全覆盖）。

> ⚠️ **配 cross-connect 前必读**：它是二层直通、**无 MAC 学习、无环路保护**。把**同一广播域**内的
> 两个端口直通（如同一虚拟交换机上的两块网卡）会造成**物理二层环路 / 广播风暴**；两端须属不同广播域。
> 见[用户手册](docs/NFViS-用户手册.md) §8。

---

## 许可

[MIT](LICENSE)
