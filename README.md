# NFViS — 网络功能虚拟化基础设施一体机软件

基于 Ubuntu 26.04 + VPP 26.06 + KVM/Libvirt 的 NFVi 一体机软件，Go 实现。
JunOS 风格 CLI（`nfvis-cli`）+ REST API（OpenAPI 契约），当前处于 **V1 设计阶段**。

## 仓库结构

```
├── docs/
│   ├── NFViS-系统产品需求与目标架构规格书.md   # 需求基线（含决策记录附录 A）
│   ├── NFViS-Go工程目录骨架设计.md             # 代码结构与里程碑（M1~M5）
│   ├── NFViS-CLI命令树完整设计.md              # CLI 契约（命令树 + 补全细则）
│   └── NFViS-openapi.yaml                      # REST API 契约（OpenAPI 3.0）
└── prototype/                                  # CLI 补全引擎交互原型（仅演示语义，非产品代码）
```

## 新成员阅读顺序

1. **规格书** — 重点 §1.2 范围摘要与附录 A 决策记录（17+4 项已定决策，不要重新发明）
2. **工程骨架** — 依赖方向规则、底座 Provider 接口、M1~M5 里程碑
3. **命令树 + OpenAPI** — CLI 与 API 一一对应，任何一侧改动必须同步另一侧
4. **原型** — `go run .` 跑一遍，感受事务模型（candidate/commit confirmed/rollback）与补全语义

## 快速开始（原型）

```bash
cd prototype
go run .                       # 交互模式（? 补全 / Tab 补全 / 事务演示）
go run . -c "show version"     # 单命令模式
go test ./...                  # 事务与补全逻辑测试
```

要求 Go ≥ 1.26。M1/M2 开发在任意平台进行（底座用 mock）；M3/M4 需要 Linux + VPP/libvirt 环境（统一开发虚机，待建）。

## 协作规则

1. **契约先行**：`NFViS-openapi.yaml` 与命令树文档是契约。改接口先改文档（MR 评审通过），再改代码；CLI 命令与 API 端点必须保持附录 B 的映射关系。
2. **需求可追溯**：PR 描述必须引用 FR-xxx 需求编号（见规格书各章需求表）。
3. **分支模型**：`main` 保护；开发走 feature 分支 + MR 评审；MR 必须包含单元测试，`internal/schema` 与事务引擎覆盖率门槛 ≥ 70%。
4. **决策记录**：任何偏离规格书的实现决策，先在附录 A 追加决策行并评审，再动代码。

## 里程碑与分工（详见工程骨架 §5）

| 里程碑 | 内容 | 前置 |
|---|---|---|
| M1 | 配置模型 + 命令树 schema + 事务引擎（SQLite，mock Provider） | 无，可立即开工 |
| M2 | REST API + AAA + nfvis-cli 前端（接 nfvisd） | M1 |
| M3 | 网络编排器（govpp：BD/VRF/BVI/ACL/NAT/SPAN/QoS/bond/LLDP）+ 恢复收敛 | M1 |
| M4 | 计算编排器（libvirt：vhost-user/SR-IOV/快照/console）+ Docker + 镜像仓库 | M1 |
| M5 | 事件/监控/审计收尾、deb 打包、e2e 与验收基准 | M2~M4 |
