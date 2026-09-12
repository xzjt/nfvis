# nfvis-cli-proto — CLI 补全引擎交互原型

模拟后端的 `nfvis-cli` 交互原型，用于验证《CLI 命令树完整设计》中的三层核心机制：

1. **raw 模式行编辑**：光标移动、历史（↑/↓）、Backspace/Delete/Home/End；
2. **`?` / Tab 补全**：任意位置列出候选、唯一匹配自动补全、多匹配先补公共前缀再列候选、动态候选（接口名/VNF 名/镜像名/快照号实时生成）；
3. **JunOS 风格事务**：candidate / commit / commit confirmed（15 秒超时自动回滚）/ rollback / compare（JunOS 风格 diff）/ discard。

## 运行

```bash
cd nfvis-cli-proto
go run .            # 交互模式（Windows 控制台 / Git Bash / 类 Unix 终端）
go run . -c "show version"    # 单命令模式（脚本化验证）
go test ./...       # 事务与补全逻辑单元测试
```

注意：非终端环境（管道）自动退化为普通行输入，补全不可用但命令可执行。

## 建议体验路径

```
nfvis> show vir<Tab>                    → 补全为 virtual-machine-functions
nfvis> show virtual-machine-functions <Tab>   → 列出 fw-vm / probe-vm（动态候选）
nfvis> show configuration | compare ?   
nfvis> configure
[edit] nfvis# set system hostname ?     → 列出 <string>
[edit] nfvis# set virtual-machine-functions <Tab>   → 列出现有 VM
[edit] nfvis# set virtual-machine-functions fw-vm interfaces eth1 type vhost-user
[edit] nfvis# set virtual-machine-functions fw-vm interfaces eth1 virtual-switch <Tab>  → vs-app / vs-mgmt
[edit] nfvis# set virtual-machine-functions fw-vm image no-such-image
[edit] nfvis# commit                    → 模拟校验失败（镜像不存在）
[edit] nfvis# delete virtual-machine-functions fw-vm image
[edit] nfvis# compare                   → JunOS 风格 + / - diff
[edit] nfvis# set system hostname nfvis-test2
[edit] nfvis# commit confirmed          → 15 秒内未确认将自动回滚（验证回滚告警输出）
[edit] nfvis# commit                    → 确认
[edit] nfvis# rollback 1
[edit] nfvis# commit
[edit] nfvis# exit
nfvis> request virtual-machine-functions fw-vm console
nfvis> exit
```

## 文件结构（对应工程骨架的映射）

| 原型文件 | 真实工程位置 |
|---|---|
| `tree.go`（命令树 schema） | `internal/schema`（nfvisd 与 nfvis-cli 编译期共享） |
| `editor.go`（行编辑/补全） | `internal/cli`（薄客户端本地交互） |
| `engine.go`（事务/状态） | `internal/config`（事务引擎）+ `internal/state`（运行态），真实实现经 `pkg/cliclient` 调 nfvisd |
