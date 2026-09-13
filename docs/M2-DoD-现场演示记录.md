# M2 DoD 现场演示记录（T0-3）

| 文档属性 | 内容 |
|---|---|
| 目的 | 完成 `docs/M2-收尾任务清单.md` DoD 第 3 条的人工现场演示，并留存可复现记录 |
| 日期 | 2026-09-13 |
| 基线 | `main` @ 92268d3（M1、M2 W1~W9 + 缩写/补全修复均已在 main） |
| 环境 | Windows 10 + Git Bash；`go build` 产物本地运行（nfvisd 明文 HTTP，仅开发用） |
| 结论 | 全部通过 |

## 演示拓扑

单机、无底座依赖（M2 使用 `NewNoopApplier`，配置事务落 SQLite，不触 VPP）：
`nfvis-cli` ──HTTP──▶ `nfvisd`（`-db .demo/nfvis.db -listen 127.0.0.1:18444`，`-init-admin-password` 引导 admin）。

## 步骤与结果

### 1. `?` / Tab 补全、行为缩写（FR-CLI-003/004）

```
$ printf 'sh vir?\nsh conf\t\nsh ver\n' | nfvis-cli -u admin
nfvis>   virtual-machine-functionsVM VNF
  virtual-switches        虚拟交换机
nfvis> system {
    hostname dod-node;
    ...
}
nfvis> NFViS 1.0.0-dev（M2：配置事务可用，网络底座 M3+ 接入）
```

- `sh vir?` 在歧义处列出 `virtual-switches` / `virtual-machine-functions` 两个候选；
- `sh conf<Tab>` 补全为 `show configuration` 并执行；
- `sh ver` 无 Tab 直接回车，经服务端 token 规整（`Canonicalize`）执行为 `show version`。

### 2. 管道过滤 `| display json`（FR-CLI-005）

```
$ nfvis-cli -c "show configuration | display json"
$ ... | tail -n +2 | python -c "import json,sys; d=json.load(sys.stdin); print('JSON OK, keys=', list(d.keys()))"
JSON OK, keys= ['system']
```

`show configuration | count` 亦可用（见步骤 3 末尾「计数: 11」）。

### 3. candidate 配置事务全流程（FR-CFG-001/002/007/008）

```
$ nfvis-cli -c "conf
set sys hostn dod-node
set sys idle-timeout-min 15
commit check
show
commit
exit
sh conf | count"

[ok] system hostname dod-node
[ok] system idle-timeout-minutes 15
校验通过
system {
    hostname dod-node;
    idle-timeout-minutes 15;
    login {
        users admin {
            class super-user;
            name admin;
            password-hash «已隐藏»;
        }
    }
}
commit 成功 (revision 3)

计数: 11
```

- `configure` 进候选态、`set` 只改 candidate、`commit check` 只校验、`commit` 落库（revision 3）；
- `show` 输出中口令哈希脱敏为「已隐藏」（FR-SEC-007）。

### 4. `commit confirmed` 超时自动回滚（FR-CFG-003）

```
$ nfvis-cli -c "conf
set sys hostn confirmed-node2
commit confirmed 1"
[ok] system hostname confirmed-node2
commit 成功 (revision 4)，confirmed 模式：09:55:54 前再次 commit 确认，否则自动回滚

（等待 70 秒）

$ nfvis-cli -c "show configuration"
system {
    hostname dod-node;        ← 已回落到 rev 3 的值
    idle-timeout-minutes 15;
    ...
}
```

审计记录（`GET /api/v1/audit-logs`）：

```
2026-09-13T01:55:54Z  system  config.rollback-auto  commit confirmed 超时未确认，已自动回滚（基线 rev 3）
2026-09-13T01:54:54Z  admin   config.commit        [edit system]
2026-09-13T01:54:48Z  admin   config.commit        [edit system]
```

### 5. 命令历史与 Ctrl-R（FR-CLI-006）

交互式 ↑/↓ 与 Ctrl-R 需 TTY，无法在管道脚本中复现；由 `internal/cli` 单测覆盖：

- `TestHistoryAddDedupAdjacent`：相邻重复命令去重；
- `TestHistoryUpDownNavigation`：↑ 从最新上溯、↓ 越过最新回草稿；
- `TestCtrlRSearch`：Ctrl-R 首击命中最新、再次上溯、无更多命中停留；
- `TestREPLIdleTimeoutLogsOut`：空闲超时吊销会话并提示。

## 复现方式

```bash
go build -o .demo/nfvisd ./cmd/nfvisd && go build -o .demo/nfvis-cli ./cmd/nfvis-cli
./.demo/nfvisd -db .demo/nfvis.db -listen 127.0.0.1:18444 -init-admin-password 'Admin@12345' &
NFVIS_PASSWORD='Admin@12345' ./.demo/nfvis-cli -server http://127.0.0.1:18444 -u admin
```

注意：API 会话以 `user@source`（默认 `admin@ssh`）为键，配置模式跨多次 CLI 进程延续；做操作模式演示前先 `exit` 或使用全新库。
