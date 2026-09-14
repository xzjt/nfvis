# NFViS — 网络功能虚拟化基础设施一体机软件

基于 Ubuntu 26.04 + VPP 26.06 + KVM/Libvirt 的 NFVi 一体机软件，Go 实现。
JunOS 风格 CLI（`nfvis-cli`）+ REST API（OpenAPI 契约），当前处于 **M5（V1 收尾）已完成阶段**（M1~M4 已合并；M5 主体 M5-1~M5-11 与 T0-1/2/6/7 已完成并有真机证据，详见 `docs/M5-验收记录.md`、`docs/M5-11-端到端与基准报告.md`）。

## 仓库结构

```
├── docs/
│   ├── NFViS-系统产品需求与目标架构规格书.md   # 需求基线（含决策记录附录 A）
│   ├── NFViS-Go工程目录骨架设计.md             # 代码结构与里程碑（M1~M5）
│   ├── NFViS-CLI命令树完整设计.md              # CLI 契约（命令树 + 补全细则）
│   └── NFViS-openapi.yaml                      # REST API 契约（OpenAPI 3.0）
├── cmd/
│   ├── nfvisd/                                 #   守护进程入口（装配/信号/优雅退出）
│   └── nfvis-cli/                              #   CLI 入口（M2 后续任务）
├── internal/                                   # 产品代码（M1 起按工程骨架布局）
│   ├── model/                                  #   配置模型（单一数据源）+ 校验 + diff/merge + 资源账本
│   ├── schema/                                 #   命令树 schema（补全/缩写/权限，CLI 与 nfvisd 编译期共享）
│   ├── config/                                 #   事务引擎：candidate/commit confirmed/rollback + SQLite
│   ├── orchestrator/                           #   底座适配接口（govpp/libvirt/docker 实现于 M3/M4）
│   ├── aaa/                                    #   本地用户/class/口令策略/Token（M2）
│   └── api/                                    #   REST server（Bearer 中间件/统一错误，M2 已合并）
├── Makefile                                    # make check = vet + 覆盖率门槛 + 原型全绿
└── prototype/                                  # CLI 补全薄演示（引用 internal/schema，非产品代码）
```

## 新成员阅读顺序

1. **规格书** — 重点 §1.2 范围摘要与附录 A 决策记录（29 项已定决策，不要重新发明）
2. **工程骨架** — 依赖方向规则、底座 Provider 接口、M1~M5 里程碑
3. **命令树 + OpenAPI** — CLI 与 API 一一对应，任何一侧改动必须同步另一侧
4. **原型** — `cd prototype && go run .` 感受补全语义（命令树引用 `internal/schema`）；事务全流程走 `nfvis-cli`

## 快速开始（原型）

```bash
cd prototype
go run .                       # 交互：? 列候选 / Tab 补全 / 缩写消歧
```

产品代码（M1 事务引擎已合并；M2 API/AAA 开发中，纯 Go + SQLite，任意平台可开发验证）：

```bash
go build ./... && go vet ./...
make check                     # vet + 覆盖率门槛（config/model/schema ≥ 70%）+ 原型全绿

# 启动守护进程（首次启动自动引导 admin，随机口令打印一次）
go run ./cmd/nfvisd -listen :8443 -db /tmp/nfvis.db
```

要求 Go ≥ 1.26。M1/M2 开发在任意平台进行（底座用 mock）；M3/M4 需要 Linux + VPP/libvirt 环境（统一开发虚机，待建）。

## 开发与验证环境

| 环境 | 配置 | 用途 |
|---|---|---|
| 本地 | Win10 + Git Bash + WSL | M1/M2 日常开发、单元测试（纯 Go，mock Provider） |
| nfvis-vm | 6C / 6G / 100G / 3×vmxnet3，Ubuntu 26.04 Server，`ssh root@nfvis-vm`（密钥登录） | M3/M4 集成验证（VPP / libvirt / Docker） |

**虚机初始化**（脚本幂等，失败项修复后可重跑）：

```bash
scp contrib/dev-vm/provision.sh root@nfvis-vm:
ssh root@nfvis-vm 'PROXY=http://192.168.155.1:2333 ./provision.sh'
```

- 安装内容：基础工具链、Go 1.26（固定版本 tarball）、VPP 26.06（fd.io 仓库，codename 缺失自动回退 noble）、libvirt/QEMU、Docker、1G×4 大页、`/opt/nfvis/{src,images,incoming,backup}` 工作目录。
- **代理 `192.168.155.1:2333`（http/socks5）按需启用**：设置 `PROXY=http://192.168.155.1:2333` 环境变量即生效（apt/go/docker 统一走它），不设置则直连。
- 大页需 reboot 生效；VPP 安装后设为不自启（避免抢占网卡），验证时手动 `systemctl start vpp`。
- 日志在虚机 `/var/log/nfvis-provision.log`。

**换行符**：`.gitattributes` 已统一仓库内 LF，Windows 端无需额外配置；发现 diff 全脏时执行 `git add --renormalize .`。

## 协作规则

1. **契约先行**：`NFViS-openapi.yaml` 与命令树文档是契约。改接口先改文档（MR 评审通过），再改代码；CLI 命令与 API 端点必须保持附录 B 的映射关系。
2. **需求可追溯**：PR 描述必须引用 FR-xxx 需求编号（见规格书各章需求表）。
3. **分支模型**：`main` 保护；开发走 feature 分支 + MR 评审；MR 必须包含单元测试，`internal/schema` 与事务引擎覆盖率门槛 ≥ 70%。
4. **决策记录**：任何偏离规格书的实现决策，先在附录 A 追加决策行并评审，再动代码。

## 监控与质量门禁（四层）

> 用 ZCode 开发的同事：无需安装任何插件。仓库已随附 workspace 配置——`AGENTS.md`（项目规则，打开仓库自动加载）、`.zcode/commands/contract-check`（契约一致性自检）与 `.zcode/commands/fr <编号>`（查需求定义与实现要点），克隆后在输入框输入 `/` 即可使用。本地一次性配置只有钩子启用：`git config core.hooksPath contrib/hooks`。
>
> **推荐个人启用插件**：`superpowers`（Settings → Plugin Management → Discover）。它提供 TDD、系统化调试、计划编写/执行、完成前验证等方法论技能，与 M1 事务引擎的开发方式直接匹配。注意：插件属于个人配置（user scope），仓库无法强制分发；仓库的契约/门禁规则不依赖它，未安装也不影响合规——但团队建议统一启用，保持会话行为一致。
>
> **推荐个人配置 MCP**：`context7`（文档实时查询）。M3/M4 对接 govpp/libvirt/VPP 插件等版本敏感 API 时，会话应先查 context7 核对官方文档再写调用代码；使用约定见 `AGENTS.md` 的「外部文档查询」一节（含垂直库查不到时的降级规则）。

| 层 | 机制 | 拦截时机 | 说明 |
|---|---|---|---|
| 1 | `AGENTS.md` | AI/开发会话启动时 | 项目规则持久化：契约先行、FR 追溯、历史踩坑清单，任何会话自动遵守 |
| 2 | pre-commit 钩子 | 每次提交 | `git config core.hooksPath contrib/hooks`（克隆后执行一次）；拦截未格式化/vet/测试失败/文档未决标记 |
| 3 | CI（`.github/workflows/ci.yml`） | 每次 push/MR | 构建 + vet + 测试覆盖率 + OpenAPI 可解析 + 无未决标记；用 GitLab/Gitee 时按此语义迁移 |
| 4 | 每日巡检（ZCode 定时任务） | 每天 09:00 | 自动 review 新提交、契约漂移检查、跑测试；机械问题直接修复提交（`chore(巡检):`），实质问题写入 `docs/reviews/` 并报告 |

## 里程碑与分工（详见工程骨架 §5）

| 里程碑 | 内容 | 前置 |
|---|---|---|
| M1 | 配置模型 + 命令树 schema + 事务引擎（SQLite，mock Provider） | 无，可立即开工 |
| M2 | REST API + AAA + nfvis-cli 前端（接 nfvisd） | M1 |
| M3 | 网络编排器（govpp：BD/VRF/BVI/ACL/NAT/SPAN/QoS/bond/LLDP）+ 恢复收敛 | M1 |
| M4 | 计算编排器（libvirt：vhost-user/SR-IOV/快照/console）+ Docker + 镜像仓库 | M1 |
| M5 | 事件/监控/审计收尾、deb 打包、e2e 与验收基准 | M2~M4 |
