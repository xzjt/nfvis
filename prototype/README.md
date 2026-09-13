# nfvis-cli-proto —— CLI 补全引擎薄演示

M2 之后，CLI 的三层核心机制已全部在产品代码中落地：

| 机制 | 产品位置 |
|---|---|
| 命令树 schema（补全/缩写/权限） | `internal/schema`（nfvisd 与 nfvis-cli 编译期共享） |
| raw 模式行编辑 / `?`·Tab 补全 / 历史 | `internal/cli` |
| JunOS 风格事务（candidate/commit/commit confirmed/rollback/compare） | `internal/config` + `internal/api`（经 `pkg/cliclient` 访问 nfvisd） |

本原型（T0-2 决策）**不再自带命令树/事务/编辑器的副本**（曾与产品实现漂移），改为直接引用
`internal/schema` 的命令树，仅演示补全语义，保留其教学价值。

## 运行

```bash
cd prototype
go run .            # 交互（非终端环境退化为行输入）
```

## 建议体验路径

```
nfvis> show vir?                    → 列 virtual-switches / virtual-machine-functions（歧义候选）
nfvis> sh conf<Tab>                 → 补全为 show configuration
nfvis> conf                         → 缩写消歧为 configure
nfvis> set sys hostn<Tab>           → 补全为 set system hostname
nfvis> set sys hostn demo-node      → 回显规范化后的语句
```

## 事务 / 行编辑 / 历史 / 空闲超时

这些能力请使用真实 CLI（需要 nfvisd 在跑）：

```bash
go run ./cmd/nfvisd -listen :8443 -db /tmp/nfvis.db     # 首次启动打印一次性 admin 口令
go run ./cmd/nfvis-cli -server http://127.0.0.1:8443 -u admin
# nfvis> configure / set ... / commit confirmed / rollback / show configuration | display json
```

原型仅为命令树语义的离线演示，非产品代码，禁止把产品逻辑写回此处（AGENTS.md 规则 3）。
